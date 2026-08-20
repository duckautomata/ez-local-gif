package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/fit"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// The GIF → GIF optimiser (Output.Preset "optimize", DESIGN.md §4.2 last
// row): the source GIF goes through gifsicle only — lossy, colour
// reduction, frame dropping with merged delays — never through a decode, so
// its palette and timing survive untouched. With FitBytes > 0 a small
// ladder (frame drop × colours) is searched over the lossy knob.

const (
	// optimizeMinDrop/MaxDrop bound GifsicleOptimizeOptions.DropEveryN.
	optimizeMinDrop = 2
	optimizeMaxDrop = 4
	// optimizeFPSTolerance is how far (relative) Output.FPS may be from an
	// exact drop ratio of the source rate.
	optimizeFPSTolerance = 0.05
)

// optimizeColorRungs are the palette sizes of the optimiser's fit ladder
// after the source's own (0 = keep).
var optimizeColorRungs = []int{0, 128, 64}

// optimizeDropRungs are the frame-drop rungs of the optimiser's fit ladder
// (DropEveryN values; 0 = keep every frame), ordered mildest first by the
// fraction of frames they keep: every 3rd dropped keeps 2/3, every 2nd 1/2.
var optimizeDropRungs = []int{0, 3, 2}

// renderOptimize runs the gifsicle-only path and returns its deliverables.
// The recipe must have a GIF source, no ops and a GIF output.
func (m *Manager) renderOptimize(ctx context.Context, j *job, scratch string, src *store.Blob, r recipe.Recipe, target discordlint.Target) ([]produced, error) {
	out := r.Output
	if !strings.EqualFold(src.Info.Codec, "gif") || src.IsSequence() {
		return nil, fmt.Errorf("%w: the optimize preset works on GIF sources only (this source is %s); render it as a GIF instead", ErrInvalidRecipe, describeSource(src))
	}
	if len(r.Ops) > 0 {
		return nil, fmt.Errorf("%w: the optimize preset cannot apply edit ops (%d given); remove them or render as a GIF instead", ErrInvalidRecipe, len(r.Ops))
	}
	if f := strings.ToLower(out.Format); f != recipe.FormatGIF {
		return nil, fmt.Errorf("%w: the optimize preset outputs GIF, not %q", ErrInvalidRecipe, out.Format)
	}
	if m.tools.Gifsicle == "" {
		return nil, errors.New("the optimize preset needs gifsicle, which is not available on this server")
	}
	facts, err := readGIFFacts(src.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	drop, err := dropEveryN(src.Info.FPS, out.FPS)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}

	m.setStage(j, StageEncode, pctEncodeStart, "gifsicle optimise")
	if out.FitBytes > 0 {
		return m.optimizeFit(ctx, j, scratch, src.Path, facts, drop, out, target)
	}
	path := filepath.Join(scratch, "opt.gif")
	opts := gifsicleOptimizeOptions(out, out.Lossy, out.Colors, drop)
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleOptimizeArgs(src.Path, path, facts.delays, opts)); err != nil {
		return nil, fmt.Errorf("gifsicle: %w", err)
	}
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read optimised gif: %w", err)
	}
	report, fixed, err := discordlint.LintGIF(data, target, true)
	if err != nil {
		return nil, fmt.Errorf("lint: %w", err)
	}
	if len(fixed) > 0 {
		data = fixed
	}
	item, err := m.finalFile(scratch, recipe.FormatGIF, data, &report)
	if err != nil {
		return nil, err
	}
	item.desc = optimizeDesc(opts, facts.frames)
	return []produced{item}, nil
}

// describeSource names a source for error messages.
func describeSource(b *store.Blob) string {
	if b.IsSequence() {
		return "an image sequence"
	}
	if b.Info != nil && b.Info.Codec != "" {
		return b.Info.Codec + " in " + b.Info.Format
	}
	return "not a GIF"
}

// gifsicleOptimizeOptions maps Output (+ the chosen knob/rung values) onto
// the optimiser options. Ordered dither goes with a colour reduction unless
// the user asked for none / an error-diffusion method.
func gifsicleOptimizeOptions(out recipe.Output, lossy, colors, drop int) enc.GifsicleOptimizeOptions {
	o := enc.GifsicleOptimizeOptions{Lossy: lossy, Colors: colors, DropEveryN: drop, Loop: out.Loop, Careful: true}
	if colors > 0 {
		o.Dither = gifsicleDither(out.Dither)
	}
	return o
}

// gifsicleDither maps the recipe's paletteuse dither names onto gifsicle's
// --dither methods ("" = none).
func gifsicleDither(d string) string {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "none":
		return ""
	case "floyd_steinberg", "floyd-steinberg", "sierra2_4a", "sierra2", "sierra3", "burkes", "atkinson", "heckbert":
		return "floyd-steinberg"
	}
	return "o8"
}

// optimizeDesc describes what the optimiser did.
func optimizeDesc(o enc.GifsicleOptimizeOptions, srcFrames int) string {
	var parts []string
	if o.Lossy > 0 {
		parts = append(parts, fmt.Sprintf("lossy %d", o.Lossy))
	}
	if o.Colors > 0 {
		parts = append(parts, fmt.Sprintf("%d colours", o.Colors))
	}
	if o.DropEveryN > 0 {
		parts = append(parts, fmt.Sprintf("every %s frame dropped (%d of %d kept)", ordinal(o.DropEveryN), keptFrames(srcFrames, o.DropEveryN), srcFrames))
	}
	if len(parts) == 0 {
		return "gifsicle -O2 (lossless)"
	}
	return "gifsicle: " + strings.Join(parts, " · ")
}

// ordinal renders 2 → "2nd", 3 → "3rd", 4 → "4th".
func ordinal(n int) string {
	switch n {
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}

// keptFrames is how many of n frames survive dropping every dropEveryN-th.
func keptFrames(n, dropEveryN int) int {
	if dropEveryN < 2 {
		return n
	}
	return n - n/dropEveryN
}

// gifFacts is what the optimiser needs to know about the source GIF.
type gifFacts struct {
	delays []int // per-frame delays in centiseconds
	frames int
	colors int // largest palette in the file (global or local), 0 if unknown
}

// readGIFFacts reads the source GIF's frame table with the block walk below
// (no pixel decode).
func readGIFFacts(path string) (gifFacts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return gifFacts{}, fmt.Errorf("read source gif: %w", err)
	}
	f, err := parseGIFFacts(data)
	if err != nil {
		return gifFacts{}, fmt.Errorf("the source GIF cannot be decoded (%v); render it as a GIF instead of optimising", err)
	}
	if f.frames == 0 {
		return gifFacts{}, errors.New("the source GIF has no frames")
	}
	return f, nil
}

// parseGIFFacts walks the GIF block structure without decoding any pixel
// data (no LZW): delays come from each frame's graphic control extension,
// the palette size from the global and local colour tables. Odd-but-valid
// files Go's strict image/gif rejects (frame rects outside the logical
// screen, undecodable frame data) still parse — gifsicle handles them fine,
// so the optimiser does not need a full decode to refuse or accept them.
// (discordlint has the same walk internally but does not export it.)
func parseGIFFacts(data []byte) (gifFacts, error) {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return gifFacts{}, errors.New("not a GIF header")
	}
	var f gifFacts
	pos := 13
	if data[10]&0x80 != 0 { // global colour table
		f.colors = 2 << (data[10] & 7)
		pos += 3 * f.colors
	}
	skipSubBlocks := func() error {
		for {
			if pos >= len(data) {
				return errors.New("unexpected end of file in a data sub-block")
			}
			n := int(data[pos])
			pos++
			if n == 0 {
				return nil
			}
			if pos+n > len(data) {
				return errors.New("truncated data sub-block")
			}
			pos += n
		}
	}
	pending := 0 // delay of the GCE preceding the next frame, centiseconds
	for {
		if pos >= len(data) {
			return gifFacts{}, errors.New("missing trailer")
		}
		b := data[pos]
		pos++
		switch b {
		case 0x3B: // trailer
			return f, nil
		case 0x21: // extension
			if pos >= len(data) {
				return gifFacts{}, errors.New("truncated extension")
			}
			label := data[pos]
			pos++
			if label == 0xF9 && pos < len(data) && int(data[pos]) >= 3 && pos+1+int(data[pos]) <= len(data) {
				// Graphic control: [size][packed][delay lo][delay hi][transp].
				pending = int(data[pos+2]) | int(data[pos+3])<<8
			}
			if err := skipSubBlocks(); err != nil {
				return gifFacts{}, err
			}
		case 0x2C: // image descriptor
			if pos+9 > len(data) {
				return gifFacts{}, errors.New("truncated image descriptor")
			}
			packed := data[pos+8]
			pos += 9
			if packed&0x80 != 0 { // local colour table
				n := 2 << (packed & 7)
				f.colors = max(f.colors, n)
				if pos+3*n > len(data) {
					return gifFacts{}, errors.New("truncated local colour table")
				}
				pos += 3 * n
			}
			if pos >= len(data) {
				return gifFacts{}, errors.New("truncated image data")
			}
			pos++ // LZW minimum code size
			if err := skipSubBlocks(); err != nil {
				return gifFacts{}, err
			}
			f.frames++
			f.delays = append(f.delays, pending)
			pending = 0
		default:
			return gifFacts{}, fmt.Errorf("unknown block 0x%02X", b)
		}
	}
}

// dropEveryN maps Output.FPS against the source rate onto
// GifsicleOptimizeOptions.DropEveryN: 0 keeps every frame (FPS 0, or at or
// above the source); otherwise the wanted rate must equal the source rate
// with every N-th frame dropped (N in 2..4, i.e. 1/2, 2/3 or 3/4 of the
// source rate, within 5 %), since the optimiser never resamples time.
func dropEveryN(srcFPS, wantFPS float64) (int, error) {
	if wantFPS <= 0 || srcFPS <= 0 || wantFPS >= srcFPS {
		return 0, nil
	}
	for n := optimizeMinDrop; n <= optimizeMaxDrop; n++ {
		kept := srcFPS * float64(n-1) / float64(n)
		if math.Abs(wantFPS-kept) <= optimizeFPSTolerance*kept {
			return n, nil
		}
	}
	var options []string
	for n := optimizeMinDrop; n <= optimizeMaxDrop; n++ {
		options = append(options, fmt.Sprintf("%.3g", srcFPS*float64(n-1)/float64(n)))
	}
	return 0, fmt.Errorf("the optimize preset can only drop every 2nd, 3rd or 4th frame of the %.3g fps source: %.3g fps is not reachable (try %s fps, or 0 to keep the frame rate)",
		srcFPS, wantFPS, strings.Join(options, ", "))
}

// optimizeFit searches a small ladder — frame drop × colours, mildest first
// — over the lossy knob, all through gifsicle. baseDrop (from Output.FPS)
// is the mildest drop tried; Output.Colors the user's palette size.
func (m *Manager) optimizeFit(ctx context.Context, j *job, scratch, src string, facts gifFacts, baseDrop int, out recipe.Output, target discordlint.Target) ([]produced, error) {
	dir := filepath.Join(scratch, fitDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("fit dir: %w", err)
	}
	rungs, drops := optimizeLadder(baseDrop, out.Colors, facts, out.FitKeepFPS)
	cands := newFitCandidates()
	var seq, encodes atomic.Int64
	encode := func(ctx context.Context, rung fit.Rung, knob int, attempt int) (string, int64, error) {
		path := filepath.Join(dir, fmt.Sprintf("c%04d.gif", seq.Add(1)))
		opts := gifsicleOptimizeOptions(out, knob, rung.Colors, drops[rung.Label])
		if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleOptimizeArgs(src, path, facts.delays, opts)); err != nil {
			return "", 0, fmt.Errorf("gifsicle: %w", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", 0, err
		}
		report, fixed, err := discordlint.LintGIF(data, target, true)
		if err != nil {
			return "", 0, fmt.Errorf("lint: %w", err)
		}
		if len(fixed) > 0 {
			data = fixed
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return "", 0, err
			}
		}
		cand := &fitCandidate{path: path, format: recipe.FormatGIF, bytes: int64(len(data)), report: report, rung: rung, knob: knob, ok: !hasErrorCheck(report)}
		cands.add(cand)
		n := encodes.Add(1)
		m.progress(j, fitProgressPct(n), fmt.Sprintf("fit: %d encodes (%s, lossy %d → %s)", n, rung.Label, knob, humanBytes(cand.bytes)))
		return path, reportedSize(cand), nil
	}
	budget := fitTarget(out.FitBytes, target)
	req := fit.Request{
		Target: budget, Margin: fitMargin, Ladder: rungs, Knob: fitKnob(recipe.FormatGIF, out),
		Parallel: fitParallel(), Alternatives: fitAlternatives,
	}
	res, err := fit.Search(ctx, req, encode)
	if err != nil && !errors.Is(err, fit.ErrNoFit) {
		return nil, fmt.Errorf("fit search: %w", err)
	}
	log.Printf("jobs: job %s optimize fit: %d encodes, %d rungs skipped, %d errors, fit=%v", j.snap.ID, res.Tried, len(res.Skipped), len(res.Errors), res.Best != nil)
	describe := func(c *fitCandidate) string { return "fit at " + c.rung.Label + fmt.Sprintf(" · lossy %d", c.knob) }
	items, err := m.fitDeliverables(ctx, cands, res.Best, res.Alternatives, budget, res.Skipped, res.Errors, scratch, enc.Master{}, describe)
	if err != nil {
		return nil, err
	}
	os.RemoveAll(dir)
	return items, nil
}

// optimizeLadder builds the optimiser's rungs — for each drop (the base
// first, then the harsher standard drops; FitKeepFPS keeps the base drop
// only, so the search never drops frames the user did not ask for) every
// colour rung (the user's palette size first, 0 = source palette; rungs at
// or above the source's own palette change nothing and are skipped) — and
// the DropEveryN of each rung by label (fit.Rung has no field for it;
// labels are unique).
func optimizeLadder(baseDrop, baseColors int, facts gifFacts, keepFPS bool) ([]fit.Rung, map[string]int) {
	drops := []int{baseDrop}
	if !keepFPS {
		for _, d := range optimizeDropRungs {
			if d == 0 || d == baseDrop || (baseDrop > 0 && keptFraction(d) >= keptFraction(baseDrop)) {
				continue
			}
			drops = append(drops, d)
		}
	}
	colors := []int{baseColors}
	for _, c := range optimizeColorRungs {
		if c == 0 || (baseColors > 0 && c >= baseColors) || (facts.colors > 0 && c >= facts.colors) {
			continue
		}
		colors = append(colors, c)
	}
	var rungs []fit.Rung
	dropOf := map[string]int{}
	for _, d := range drops {
		for _, c := range colors {
			var parts []string
			if d > 0 {
				parts = append(parts, fmt.Sprintf("every %s frame dropped (%d frames)", ordinal(d), keptFrames(facts.frames, d)))
			} else {
				parts = append(parts, "all frames")
			}
			if c > 0 {
				parts = append(parts, fmt.Sprintf("%d colours", c))
			} else {
				parts = append(parts, "source colours")
			}
			r := fit.Rung{Colors: c, Format: recipe.FormatGIF, Label: strings.Join(parts, " · ")}
			rungs = append(rungs, r)
			dropOf[r.Label] = d
		}
	}
	return rungs, dropOf
}

// keptFraction is the share of frames a DropEveryN value keeps (1 for 0).
func keptFraction(dropEveryN int) float64 {
	if dropEveryN < 2 {
		return 1
	}
	return float64(dropEveryN-1) / float64(dropEveryN)
}
