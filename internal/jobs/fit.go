package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/fit"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Fit-to-size (DESIGN.md §5.4): the fit engine (internal/fit) runs a ladder
// of rungs, each searched over one quality knob; jobs supplies the encoder.
// Every candidate is encoded from the master through an enc.Variant (fps
// drop / downscale), linted for the recipe's target, and reported to the
// engine at its real byte size — unless a LevelError rule other than the
// plain byte cap (*.size-limit) failed, in which case it is reported as far
// over the target so it is never chosen (reportedSize). The byte cap is
// exactly what the engine searches on, so over-cap candidates must keep
// their real size or the secant search cannot see the size curve and
// converges far below the budget at a needlessly harsh knob. The engine's
// target is Output.FitBytes clamped to the Discord cap of the recipe's
// target (fitTarget), so a candidate over the cap can never win even when
// the user's budget exceeds the cap. The winner becomes out.<ext>, the
// runner-ups alt1/alt2.<ext>; when nothing fits the smallest attempt is
// still delivered with a failing report so the user sees what was achieved.

// fitFormats are the output formats with a fit knob.
var fitFormats = map[string]bool{
	recipe.FormatGIF: true, recipe.FormatWebP: true, recipe.FormatAPNG: true, recipe.FormatAVIF: true,
	recipe.FormatPNG: true, recipe.FormatJPEG: true,
}

const (
	// fitMargin is the fraction kept free under Output.FitBytes.
	fitMargin = 0.02
	// fitAlternatives is how many runner-up rungs are delivered.
	fitAlternatives = 2
	// fitOverTarget is the size reported to the engine for a candidate that
	// fails a LevelError lint rule other than the plain byte cap (structural
	// problems, dimension/duration limits): finite (the secant search takes a
	// log), but beyond any target. A candidate whose only failing LevelError
	// rules are *.size-limit reports its real size instead (reportedSize) so
	// the secant search sees the true size curve.
	fitOverTarget = int64(math.MaxInt32)
	// fitDirName is the scratch subdirectory of the candidates.
	fitDirName = "fit"
	// fitProgressScale shapes the encode-stage percentage: after this many
	// encodes the bar is halfway through the stage.
	fitProgressScale = 8.0
)

// RuleFitTarget is the check appended when no candidate fit under
// Output.FitBytes and the smallest attempt is delivered instead.
const RuleFitTarget = "fit.target"

// fitParallel is the engine's concurrency (max(1, NumCPU/2)).
func fitParallel() int { return max(1, runtime.NumCPU()/2) }

// fitCandidate is one encoded attempt.
type fitCandidate struct {
	path   string
	format string
	bytes  int64
	report discordlint.Report
	rung   fit.Rung
	knob   int
	ok     bool // no LevelError check failed
}

// fitCandidates is the thread-safe record of every attempt of a search.
type fitCandidates struct {
	mu     sync.Mutex
	byPath map[string]*fitCandidate
}

func newFitCandidates() *fitCandidates { return &fitCandidates{byPath: map[string]*fitCandidate{}} }

func (c *fitCandidates) add(cand *fitCandidate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byPath[cand.path] = cand
}

func (c *fitCandidates) get(path string) *fitCandidate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byPath[path]
}

// smallest returns the best attempt when nothing fit: structurally sound
// files first (only limit rules failed), then by size.
func (c *fitCandidates) smallest() *fitCandidate {
	c.mu.Lock()
	defer c.mu.Unlock()
	var all []*fitCandidate
	for _, cand := range c.byPath {
		all = append(all, cand)
	}
	sort.Slice(all, func(i, j int) bool {
		si, sj := hasStructuralError(all[i].report), hasStructuralError(all[j].report)
		if si != sj {
			return !si
		}
		if all[i].bytes != all[j].bytes {
			return all[i].bytes < all[j].bytes
		}
		return all[i].path < all[j].path
	})
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

func (c *fitCandidates) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byPath)
}

// paths returns every candidate path.
func (c *fitCandidates) paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byPath))
	for p := range c.byPath {
		out = append(out, p)
	}
	return out
}

// fitRun is the state of one master-based fit search.
type fitRun struct {
	m       *Manager
	j       *job
	dir     string // <scratch>/fit
	master  enc.Master
	out     recipe.Output
	target  discordlint.Target
	format  string // request format (lower case)
	cands   *fitCandidates
	seq     atomic.Int64
	encodes atomic.Int64

	// Per-variant intermediates shared by every knob probe of a rung.
	sheets sync.Map // variantKey → *sheetEntry
	pngs   sync.Map // variantKey → *pngEntry

	gifsicleWarned atomic.Bool
}

type sheetEntry struct {
	once  sync.Once
	sheet *tileSheet
	err   error
}

type pngEntry struct {
	once   sync.Once
	frames []string
	err    error
}

// renderFit runs the ladder for the recipe and returns the deliverables
// (primary first, then alternatives).
func (m *Manager) renderFit(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) ([]produced, error) {
	format := strings.ToLower(out.Format)
	rungs := fitLadder(format, out, master)
	if len(rungs) == 0 {
		return nil, fmt.Errorf("%w: no fit ladder for format %q", ErrInvalidRecipe, out.Format)
	}
	if m.tools.Pngquant == "" {
		// Indexed APNG rungs need pngquant; dropping them up front saves the
		// sprite-sheet render each would otherwise burn before failing.
		kept, dropped := dropIndexedAPNGRungs(rungs, format)
		if dropped > 0 {
			log.Printf("jobs: job %s fit: skipping %d indexed APNG rungs (pngquant is not available)", j.snap.ID, dropped)
			rungs = kept
		}
		if len(rungs) == 0 {
			return nil, errors.New("the APNG fit search needs pngquant, which is not available on this server (remove the fit budget for truecolour APNG, or pick another format)")
		}
	}
	run := &fitRun{
		m: m, j: j, dir: filepath.Join(scratch, fitDirName), master: master, out: out, target: target,
		format: format, cands: newFitCandidates(),
	}
	if err := os.MkdirAll(run.dir, 0o755); err != nil {
		return nil, fmt.Errorf("fit dir: %w", err)
	}
	budget := fitTarget(out.FitBytes, target)
	m.progress(j, pctEncodeStart, fmt.Sprintf("fit: searching for <= %s", humanBytes(budget)))

	req := fit.Request{
		Target:       budget,
		Margin:       fitMargin,
		Ladder:       rungs,
		Knob:         fitKnob(format, out),
		Knobs:        knobsFor(rungs, format, out),
		Format:       format,
		Parallel:     fitParallel(),
		Alternatives: fitAlternatives,
	}
	res, err := fit.Search(ctx, req, run.encode)
	if err != nil && !errors.Is(err, fit.ErrNoFit) {
		return nil, fmt.Errorf("fit search: %w", err)
	}
	id := j.snap.ID
	log.Printf("jobs: job %s fit: %d encodes, %d rungs skipped, %d errors, fit=%v", id, res.Tried, len(res.Skipped), len(res.Errors), res.Best != nil)
	for _, e := range res.Errors {
		log.Printf("jobs: job %s fit: %s", id, e)
	}
	items, err := m.fitDeliverables(ctx, run.cands, res.Best, res.Alternatives, budget, res.Skipped, res.Errors, scratch, master, run.describe)
	if err != nil {
		return nil, err
	}
	os.RemoveAll(run.dir) // every candidate not moved out above
	return items, nil
}

// fitTarget is the engine's byte target: Output.FitBytes clamped to the
// Discord cap of the recipe's target (when it has one). With reportedSize
// giving over-cap candidates their real size, this clamp is what keeps "over
// the Discord cap never wins" when the user's budget exceeds the cap.
func fitTarget(fitBytes int64, target discordlint.Target) int64 {
	if lim := discordlint.Limit(target); lim > 0 && lim < fitBytes {
		return lim
	}
	return fitBytes
}

// knobsFor returns the per-format knobs of a ladder (a sticker ladder mixes
// apng and gif rungs), each with the mild probe at the user's own settings.
func knobsFor(rungs []fit.Rung, request string, out recipe.Output) map[string]fit.Knob {
	knobs := map[string]fit.Knob{}
	for _, r := range rungs {
		f := effectiveFormat(r, request)
		if _, ok := knobs[f]; !ok {
			knobs[f] = fitKnob(f, out)
		}
		if r.Format != "" {
			knobs[r.Format] = knobs[f]
		}
	}
	return knobs
}

// fitKnob returns the format's search knob with the mild probe moved onto
// the user's own quality settings: the fit search must never degrade below
// what the user asked for when that already fits (an emote-preset GIF at
// lossy 0 used to come out at the knob's default mild, lossy 30). gif →
// Output.Lossy; webp/avif/jpeg → 100 - the effective quality (the format's
// default when Output.Quality is 0); apng/png → colour step 0. When the
// user's setting is harsher than the knob's default harsh probe, the harsh
// probe moves to Max so the bracket stays valid.
func fitKnob(format string, out recipe.Output) fit.Knob {
	k := fit.KnobFor(format)
	switch format {
	case recipe.FormatGIF:
		k.Mild = out.Lossy
	case recipe.FormatWebP, recipe.FormatAVIF, recipe.FormatJPEG:
		q := out.Quality
		if q <= 0 {
			switch format {
			case recipe.FormatAVIF:
				q = DefaultAVIFQuality
			case recipe.FormatJPEG:
				q = enc.DefaultJPEGQuality
			default:
				q = enc.DefaultWebPQuality
			}
		}
		k.Mild = 100 - q
	default:
		k.Mild = 0
	}
	k.Mild = min(max(k.Mild, k.Min), k.Max)
	if k.Mild > k.Harsh {
		k.Harsh = k.Max
	}
	return k
}

// dropIndexedAPNGRungs removes the rungs that would need pngquant (indexed
// APNG; the RGBA truecolour probes and every other format stay), so a fit
// search without the tool never renders sprite sheets it cannot quantise.
func dropIndexedAPNGRungs(rungs []fit.Rung, request string) (kept []fit.Rung, dropped int) {
	for _, r := range rungs {
		if effectiveFormat(r, request) == recipe.FormatAPNG && !r.Truecolor {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	return kept, dropped
}

// describe renders the binding knob of a candidate ("fit at 20 fps · 128
// colours · lossy 60").
func (r *fitRun) describe(c *fitCandidate) string {
	desc := "fit at " + c.rung.Label
	if k := knobDesc(c.format, c.rung, r.out, c.knob); k != "" {
		desc += " · " + k
	}
	return desc
}

// knobDesc renders a knob value in the format's own terms ("" when the
// knob left the rung as labelled: an APNG at its rung's palette size).
func knobDesc(format string, rung fit.Rung, out recipe.Output, knob int) string {
	if rung.Truecolor {
		// Single-probe rungs (RGBA APNG, lossless WebP): the knob changes
		// nothing; the label already says what the rung is.
		return ""
	}
	switch format {
	case recipe.FormatGIF:
		return fmt.Sprintf("lossy %d", knob)
	case recipe.FormatAPNG:
		if knob <= 0 {
			return ""
		}
		return fmt.Sprintf("palette %d → %d", apngColors(rung, out, 0), apngColors(rung, out, knob))
	case recipe.FormatPNG:
		if knob <= 0 {
			return ""
		}
		if from := pngColors(rung, out, 0); from > 0 {
			return fmt.Sprintf("palette %d → %d", from, pngColors(rung, out, knob))
		}
		return fmt.Sprintf("full colour → %d", pngColors(rung, out, knob))
	}
	return fmt.Sprintf("q %d", qualityFromKnob(knob))
}

// fitDeliverables turns the engine's result (or, when nothing fit, the
// smallest attempt) into produced files under scratch (outside the fit dir,
// which the caller removes). master is used to re-apply the alpha scan
// after a re-lint; pass the zero Master for searches without one. describe
// renders a candidate's binding knob for File.Desc.
func (m *Manager) fitDeliverables(ctx context.Context, cands *fitCandidates, best *fit.Candidate, alts []fit.Candidate, fitBytes int64, skipped, errs []string, scratch string, master enc.Master, describe func(*fitCandidate) string) ([]produced, error) {
	withMaster := master.Path != ""
	if best == nil {
		cand := cands.smallest()
		if cand == nil {
			detail := "no candidate could be encoded"
			if len(errs) > 0 {
				detail = strings.Join(errs, "; ")
			}
			return nil, fmt.Errorf("cannot fit under %s: %s", humanBytes(fitBytes), detail)
		}
		attempt := strings.TrimPrefix(describe(cand), "fit at ")
		suffix := ""
		if len(skipped) > 0 {
			suffix = fmt.Sprintf("; %d rungs skipped", len(skipped))
		}
		var desc string
		if blocking := failingRuleDetails(cand.report); cand.bytes <= fitBytes && len(blocking) > 0 {
			// The attempt IS under the byte budget: the size target is not
			// what blocks delivery, so headline the real failing rule (its
			// check already stands in the report) instead of a contradictory
			// "cannot fit under N".
			scope := "the Discord rules"
			if t := cand.report.Target; t != "" {
				scope = fmt.Sprintf("the Discord %s rules", t)
			}
			desc = fmt.Sprintf("no candidate passes %s (%s); smallest attempt is %s (%s)%s", scope, strings.Join(blocking, "; "), humanBytes(cand.bytes), attempt, suffix)
		} else {
			desc = fmt.Sprintf("cannot fit under %s: smallest attempt is %s (%s)%s", humanBytes(fitBytes), humanBytes(cand.bytes), attempt, suffix)
			cand.report.Checks = append(cand.report.Checks, discordlint.Check{Rule: RuleFitTarget, Level: discordlint.LevelError, OK: false, Detail: desc})
		}
		cand.report.OK = false
		item, err := m.fitItem(ctx, cand, primaryBase, FileKindOutput, 0, desc, scratch, master, withMaster)
		if err != nil {
			return nil, err
		}
		return []produced{item}, nil
	}
	win := cands.get(best.Path)
	if win == nil {
		return nil, fmt.Errorf("fit: winner %s is not a recorded candidate", filepath.Base(best.Path))
	}
	primary, err := m.fitItem(ctx, win, primaryBase, FileKindOutput, 0, describe(win), scratch, master, withMaster)
	if err != nil {
		return nil, err
	}
	items := []produced{primary}
	for _, alt := range alts {
		if len(items) > fitAlternatives {
			break
		}
		cand := cands.get(alt.Path)
		if cand == nil || cand == win {
			continue
		}
		index := len(items)
		item, err := m.fitItem(ctx, cand, fmt.Sprintf("%s%d", altBase, index), FileKindAlternative, index, describe(cand), scratch, master, withMaster)
		if err != nil {
			log.Printf("jobs: fit alternative %d dropped: %v", index, err)
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

// fitItem moves a candidate out of the fit dir (scratch/<base>.<ext>),
// applies the delivery-only PNG optimisation (oxipng, with a re-lint that
// keeps the report honest) and describes it.
func (m *Manager) fitItem(ctx context.Context, cand *fitCandidate, base, kind string, index int, desc, scratch string, master enc.Master, withMaster bool) (produced, error) {
	ext := extFor(cand.format)
	dst := filepath.Join(scratch, base+"."+ext)
	if err := os.Rename(cand.path, dst); err != nil {
		return produced{}, fmt.Errorf("move candidate: %w", err)
	}
	cand.path = dst
	if cand.format == recipe.FormatAPNG || cand.format == recipe.FormatPNG {
		m.oxipng(ctx, dst)
		if data, err := os.ReadFile(dst); err == nil {
			if rep, err := relintPNGFamily(cand.format, data, cand.report.Target); err == nil {
				rep.Checks = append(rep.Checks, fitChecks(cand.report)...)
				rep.OK = rep.OK && cand.report.OK
				if withMaster {
					applyMasterAlpha(&rep, master)
				}
				cand.report = rep
				cand.bytes = int64(len(data))
			}
		}
	}
	report := cand.report
	return produced{
		path: dst, name: base + "." + ext, format: cand.format, kind: kind, index: index, desc: desc,
		report: &report, verify: true,
	}, nil
}

// relintPNGFamily re-lints an oxipng-optimised deliverable (RGBA/indexed
// APNG or static PNG) so its report matches the delivered bytes.
func relintPNGFamily(format string, data []byte, target discordlint.Target) (discordlint.Report, error) {
	if format == recipe.FormatAPNG {
		return discordlint.LintAPNG(data, target)
	}
	return discordlint.LintStatic(recipe.FormatPNG, data, target)
}

// failingRuleDetails lists the failing LevelError checks of a report that
// are not the plain byte cap (*.size-limit): the details when present, else
// the rule ids. It headlines the real blocking rule when every attempt of a
// fit search is under the byte budget but still fails a Discord rule.
func failingRuleDetails(rep discordlint.Report) []string {
	var out []string
	for _, c := range rep.Checks {
		if c.OK || c.Level != discordlint.LevelError || strings.HasSuffix(c.Rule, ".size-limit") {
			continue
		}
		if c.Detail != "" {
			out = append(out, c.Detail)
		} else {
			out = append(out, c.Rule)
		}
	}
	return out
}

// fitChecks returns the jobs-level checks of a report (the fit verdict), so
// a re-lint can carry them over.
func fitChecks(rep discordlint.Report) []discordlint.Check {
	var out []discordlint.Check
	for _, c := range rep.Checks {
		if c.Rule == RuleFitTarget {
			out = append(out, c)
		}
	}
	return out
}

// effectiveFormat is the format a rung encodes to.
func effectiveFormat(r fit.Rung, request string) string {
	if f := strings.ToLower(strings.TrimSpace(r.Format)); f != "" {
		return f
	}
	return request
}

// fitLadder picks the §5.4 ladder for the request: the emote ladders for
// Target emote (GIF, or the WebP ladder for WebP/AVIF), indexed APNG then
// GIF for Target sticker (GIF rungs only when the user forced GIF), the
// generic ladder otherwise. FitKeepSize/FitKeepFPS are honoured for every
// ladder; rungs that cannot change anything against this master (fps drops
// on a still) are neutralised and duplicates dropped. Generic colour rungs
// are clamped against the user's own palette (clampGenericColors), and a
// lossless WebP request gets a single lossless probe as the mildest rung.
func fitLadder(format string, out recipe.Output, master enc.Master) []fit.Rung {
	w, h, fps := master.Width, master.Height, master.FPS
	target := discordlint.Target(strings.ToLower(out.Target))
	var rungs []fit.Rung
	switch {
	case target == discordlint.TargetEmote && format == recipe.FormatGIF:
		rungs = fit.Filter(fit.EmoteGIF(fps, w, h), out.FitKeepSize, out.FitKeepFPS, fps, w, h)
	case target == discordlint.TargetEmote && (format == recipe.FormatWebP || format == recipe.FormatAVIF):
		rungs = fit.Filter(fit.EmoteWebP(fps, w, h), out.FitKeepSize, out.FitKeepFPS, fps, w, h)
		for i := range rungs {
			rungs[i].Format = format // the same geometry ladder serves emote AVIF
		}
	case target == discordlint.TargetSticker && (format == recipe.FormatAPNG || format == recipe.FormatGIF):
		rungs = fit.Filter(fit.StickerAPNGThenGIF(fps, w, h), out.FitKeepSize, out.FitKeepFPS, fps, w, h)
		if format == recipe.FormatGIF {
			// The user forced GIF: the GIF rungs only.
			var gifOnly []fit.Rung
			for _, r := range rungs {
				if effectiveFormat(r, format) == recipe.FormatGIF {
					gifOnly = append(gifOnly, r)
				}
			}
			rungs = gifOnly
		}
	default:
		rungs = clampGenericColors(fit.Generic(format, fps, w, h, out.FitKeepSize, out.FitKeepFPS), out.Colors)
	}
	if len(rungs) == 0 {
		// Knob-only search at the master's own settings.
		rungs = []fit.Rung{{Label: "as rendered"}}
	}
	if format == recipe.FormatWebP && out.Lossless {
		// Output.Lossless is honoured as the mildest rung: a single probe of
		// the lossless encode at the master's settings; only when it misses
		// the budget does the lossy quality search below it take over.
		rungs = append([]fit.Rung{{Label: "lossless", Truecolor: true, Knob: &fit.Knob{Name: "probe"}}}, rungs...)
	}
	return filterRungs(rungs, master)
}

// clampGenericColors post-filters the generic ladder against the user's own
// palette size: a rung palette at or above Output.Colors is not a degrade
// step — encoding at it would quantise to MORE colours than the user asked
// for, making the ladder non-monotone — so such rungs fall back to "as
// requested" (Colors 0; pure colour rungs then collapse onto the rung before
// them and are dropped by filterRungs). Rung palettes strictly below the
// user's are kept: they are genuinely harsher.
func clampGenericColors(rungs []fit.Rung, userColors int) []fit.Rung {
	if userColors <= 0 {
		return rungs
	}
	out := make([]fit.Rung, 0, len(rungs))
	for _, r := range rungs {
		if r.Colors >= userColors && r.Colors > 0 {
			r.Label = stripColoursLabel(r.Label, r.Colors)
			r.Colors = 0
			r.Knob = nil // a colour-step knob bounded by the old palette no longer applies
		}
		out = append(out, r)
	}
	return out
}

// stripColoursLabel removes the "<n> colours" part from a rung label after
// its palette was clamped to "as requested".
func stripColoursLabel(label string, colors int) string {
	parts := strings.Split(label, " · ")
	kept := parts[:0]
	needle := fmt.Sprintf("%d colours", colors)
	for _, p := range parts {
		if p == needle {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return "as requested"
	}
	return strings.Join(kept, " · ")
}

// filterRungs neutralises rungs that do not change anything against this
// master (an fps at or above the master's, any fps for a single-frame
// master, a size at or above the master's) and drops duplicates, keeping
// the ladder order and labels.
func filterRungs(rungs []fit.Rung, master enc.Master) []fit.Rung {
	seen := map[string]bool{}
	var kept []fit.Rung
	for _, r := range rungs {
		if r.FPS > 0 && (r.FPS >= master.FPS || master.Frames <= 1) {
			r.FPS = 0
		}
		if r.Width > 0 && r.Width >= master.Width && (r.Height == 0 || r.Height >= master.Height) {
			r.Width, r.Height = 0, 0
		}
		key := fmt.Sprintf("%.3f|%d|%d|%d|%s|%s|%t", r.FPS, r.Width, r.Height, r.Colors, strings.ToLower(r.Dither), strings.ToLower(r.Format), r.Truecolor)
		if seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, r)
	}
	return kept
}

// ---- the encoder the engine drives ------------------------------------------

// encode is the fit.EncodeFunc: encode the master for rung at knob, lint,
// record, and report the size (or fitOverTarget when a LevelError rule other
// than the plain byte cap failed; see reportedSize).
func (r *fitRun) encode(ctx context.Context, rung fit.Rung, knob int, attempt int) (string, int64, error) {
	format := effectiveFormat(rung, r.format)
	id := fmt.Sprintf("%04d", r.seq.Add(1))
	v := variantFor(r.master, rung.FPS, rung.Width, rung.Height)
	path, err := r.encodeCandidate(ctx, format, id, rung, knob, v)
	if err != nil {
		return "", 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, fmt.Errorf("read candidate: %w", err)
	}
	report, data, changed, err := r.lint(format, data, v)
	if err != nil {
		return "", 0, fmt.Errorf("lint %s candidate: %w", format, err)
	}
	if changed {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", 0, fmt.Errorf("write candidate: %w", err)
		}
	}
	cand := &fitCandidate{path: path, format: format, bytes: int64(len(data)), report: report, rung: rung, knob: knob, ok: !hasErrorCheck(report)}
	r.cands.add(cand)
	n := r.encodes.Add(1)
	r.m.progress(r.j, fitProgressPct(n), fmt.Sprintf("fit: %d encodes (%s, %s %d → %s)", n, rung.Label, knobName(format), knob, humanBytes(cand.bytes)))
	return path, reportedSize(cand), nil
}

// reportedSize is the size a candidate reports to the fit engine: the real
// byte count for clean candidates and for ones whose only failing LevelError
// rules are the plain byte cap (the engine searches on exactly that size, so
// the secant needs the true curve of over-budget probes), fitOverTarget when
// any other LevelError rule failed so the candidate can never be chosen.
func reportedSize(cand *fitCandidate) int64 {
	if !cand.ok && !onlySizeLimitFailed(cand.report) {
		return fitOverTarget
	}
	return cand.bytes
}

// onlySizeLimitFailed reports whether at least one LevelError check failed
// and every failing LevelError check is a plain byte-cap rule
// (gif/webp/apng/static .size-limit).
func onlySizeLimitFailed(rep discordlint.Report) bool {
	any := false
	for _, c := range rep.Checks {
		if c.OK || c.Level != discordlint.LevelError {
			continue
		}
		if !strings.HasSuffix(c.Rule, ".size-limit") {
			return false
		}
		any = true
	}
	return any
}

// fitProgressPct maps the encode count onto the encode stage (monotone,
// asymptotic to the stage end).
func fitProgressPct(n int64) float64 {
	frac := 1 - 1/(1+float64(n)/fitProgressScale)
	return pctEncodeStart + frac*(pctEncodeEnd-pctEncodeStart)
}

// knobName is the human name of a format's knob.
func knobName(format string) string {
	switch format {
	case recipe.FormatGIF:
		return "lossy"
	case recipe.FormatAPNG, recipe.FormatPNG:
		return "colour step"
	}
	return "quality knob"
}

// encodeCandidate writes one candidate file and returns its path.
func (r *fitRun) encodeCandidate(ctx context.Context, format, id string, rung fit.Rung, knob int, v *enc.Variant) (string, error) {
	m, out, master := r.m, r.out, r.master
	tag := "-" + id
	switch format {
	case recipe.FormatGIF:
		gopts := gifOptionsFor(out, v, master)
		if rung.Colors > 0 && (out.Colors <= 0 || rung.Colors < out.Colors) {
			gopts.Colors = rung.Colors
		}
		if rung.Dither != "" {
			gopts.Dither = rung.Dither
		}
		if m.tools.Gifsicle == "" && r.gifsicleWarned.CompareAndSwap(false, true) {
			log.Printf("jobs: gifsicle is not available; the gif fit search cannot apply the lossy knob")
		}
		// The palette pass already quantised to min(rung, user) colours;
		// passing --colors to gifsicle as well would median-cut + dither the
		// frames a second time, so it only applies the lossy knob and loop.
		return m.encodeGIFAt(ctx, r.j, r.dir, tag, master, gopts, enc.GifsicleOptions{Lossy: knob, Loop: out.Loop})
	case recipe.FormatWebP:
		return m.encodeWebPAt(ctx, r.j, r.dir, tag, master, enc.WebPOptions{Quality: qualityFromKnob(knob), Lossless: rung.Truecolor, Loop: out.Loop, Variant: v})
	case recipe.FormatAPNG:
		path := filepath.Join(r.dir, "c"+id+".png")
		if rung.Truecolor {
			// The RGBA probe rung of the sticker ladder must deliver real
			// truecolour bytes, not the indexed pipeline under an RGBA label.
			if err := m.encodeRGBAAPNG(ctx, master, v, out.Loop, path); err != nil {
				return "", err
			}
			return path, nil
		}
		colors := apngColors(rung, out, knob)
		sheet, err := r.sheetFor(ctx, v)
		if err != nil {
			return "", err
		}
		if err := m.quantizeUntile(ctx, r.dir, tag, sheet, colors, out.Loop, path); err != nil {
			return "", err
		}
		return path, nil
	case recipe.FormatAVIF:
		if m.tools.Avifenc == "" {
			return "", errors.New("AVIF output needs avifenc, which is not available on this server")
		}
		frames, err := r.pngFramesFor(ctx, v)
		if err != nil {
			return "", err
		}
		path := filepath.Join(r.dir, "c"+id+".avif")
		vm := enc.VariantMaster(master, v)
		if err := m.encodeAVIF(ctx, frames, vm.FPS, m.avifOptions(vm, qualityFromKnob(knob), out.Loop), path); err != nil {
			return "", err
		}
		return path, nil
	case recipe.FormatPNG:
		path := filepath.Join(r.dir, "c"+id+".png")
		if err := m.encodePNGStill(ctx, r.dir, tag, master, v, pngColors(rung, out, knob), path); err != nil {
			return "", err
		}
		return path, nil
	case recipe.FormatJPEG:
		path := filepath.Join(r.dir, "c"+id+".jpg")
		if err := m.encodeJPEGStill(ctx, master, v, qualityFromKnob(knob), out.Matte, path); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("fit: no encoder for format %q", format)
}

// qualityFromKnob maps the engine's "larger = smaller file" knob onto a
// 1..100 quality (webp/avif/jpeg).
func qualityFromKnob(knob int) int { return min(max(100-knob, 1), 100) }

// apngColors maps an APNG rung + knob (colour reduction steps) onto a
// palette size: the rung's colours (else the request's, else 256) halved per
// step. The fit search never quantises below 64 colours (DESIGN.md §5.4);
// an explicit user base below 64 is kept as-is instead of halved further.
func apngColors(rung fit.Rung, out recipe.Output, knob int) int {
	base := rung.Colors
	if base <= 0 {
		base = out.Colors
	}
	if base <= 0 {
		base = enc.DefaultColors
	}
	floor := min(base, 64)
	c := base
	for i := 0; i < knob; i++ {
		c /= 2
	}
	return max(c, floor)
}

// pngColors maps a static-PNG rung + knob (colour reduction steps) onto a
// pngquant palette size. Unlike apngColors, 0 means "keep full colour" (no
// quantisation, encodePNGStill skips pngquant): the first step of a
// full-colour rung drops to the default palette (256), later steps halve it,
// never below 2.
func pngColors(rung fit.Rung, out recipe.Output, knob int) int {
	base := rung.Colors
	if base <= 0 {
		base = out.Colors
	}
	if base <= 0 {
		if knob == 0 {
			return 0
		}
		base = enc.DefaultColors
		knob--
	}
	for i := 0; i < knob; i++ {
		base /= 2
	}
	return max(base, 2)
}

// sheetFor returns the (cached) sprite sheet of a variant.
func (r *fitRun) sheetFor(ctx context.Context, v *enc.Variant) (*tileSheet, error) {
	key := variantKey(v)
	e, _ := r.sheets.LoadOrStore(key, &sheetEntry{})
	entry := e.(*sheetEntry)
	entry.once.Do(func() {
		entry.sheet, entry.err = r.m.renderTileSheet(ctx, r.dir, "-"+key, r.master, v)
	})
	return entry.sheet, entry.err
}

// pngFramesFor returns the (cached) PNG frames of a variant.
func (r *fitRun) pngFramesFor(ctx context.Context, v *enc.Variant) ([]string, error) {
	key := variantKey(v)
	e, _ := r.pngs.LoadOrStore(key, &pngEntry{})
	entry := e.(*pngEntry)
	entry.once.Do(func() {
		entry.frames, entry.err = r.m.renderPNGFrames(ctx, filepath.Join(r.dir, "png-"+key), r.master, v, 1)
	})
	return entry.frames, entry.err
}

// lint runs the format's checker on a candidate; GIF fixes are applied to
// the returned bytes (changed reports that). The master's alpha scan
// overrides the structural flag for formats that keep alpha.
func (r *fitRun) lint(format string, data []byte, v *enc.Variant) (report discordlint.Report, out []byte, changed bool, err error) {
	out = data
	switch format {
	case recipe.FormatGIF:
		var fixed []byte
		report, fixed, err = discordlint.LintGIF(data, r.target, true)
		if err == nil && len(fixed) > 0 && !bytes.Equal(fixed, data) {
			out, changed = fixed, true
		}
	case recipe.FormatWebP:
		report, err = discordlint.LintWebP(data, r.target)
	case recipe.FormatAPNG:
		report, err = discordlint.LintAPNG(data, r.target)
	case recipe.FormatAVIF:
		report, err = lintAVIF(data, r.target, variantFrames(r.master, v), variantFPS(r.master, v), r.out.Loop)
	case recipe.FormatPNG:
		report, err = discordlint.LintStatic(recipe.FormatPNG, data, r.target)
	case recipe.FormatJPEG:
		report, err = discordlint.LintStatic(recipe.FormatJPEG, data, r.target)
	default:
		return report, data, false, fmt.Errorf("no linter for %q", format)
	}
	if err != nil {
		return report, data, false, err
	}
	if format != recipe.FormatJPEG {
		applyMasterAlpha(&report, r.master)
	}
	return report, out, changed, nil
}
