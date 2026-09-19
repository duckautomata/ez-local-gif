package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// masterAlphaChunk is the read size of the post-render alpha scan (a
// multiple of 4 so pixel alignment survives across chunks).
const masterAlphaChunk = 4 << 20

// Result-dir file names.
const (
	// primaryBase is the base name of the primary output ("out.<ext>").
	primaryBase = "out"
	// altBase prefixes fit-search alternatives ("alt1.<ext>").
	altBase = "alt"
	// reportName is the primary file's lint report inside the result dir.
	reportName = "report.json"
)

// produced is one deliverable written by an encoder before it is staged:
// the file (still under scratch), its name in the result dir and the facts
// the manifest needs.
type produced struct {
	path   string // absolute path under scratch
	name   string // file name inside the result dir
	format string // effective format (may differ from Output.Format, e.g. a sticker that fit as gif)
	kind   string // FileKind*
	index  int    // File.Index
	desc   string // File.Desc
	report *discordlint.Report
	verify bool // decode-verify with ffmpeg before delivery

	// Facts for files without a report (frames, archives); a report's
	// values win when present.
	width, height, frames int
	fps, duration         float64
}

// extFor maps an output format to the file extension used in result dirs.
// APNG is delivered as .png (it is a PNG; Discord's sticker uploader and
// every file picker accept it as one) and JPEG as .jpg.
func extFor(format string) string {
	switch format {
	case recipe.FormatAPNG:
		return "png"
	case recipe.FormatJPEG:
		return "jpg"
	}
	return format
}

// run executes the pipeline for j and records the outcome. It never panics
// the process: a panic inside the pipeline becomes a job error.
func (m *Manager) run(ctx context.Context, j *job) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("jobs: job %s panicked: %v", j.snap.ID, r)
			m.fail(j, fmt.Errorf("internal error: %v", r))
		}
	}()
	res, err := m.render(ctx, j)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("cancelled")
		} else {
			err = m.describeNoSpace(err)
		}
		m.fail(j, err)
		return
	}
	m.succeed(j, res)
}

// render is the pipeline proper. It returns the manifest on success.
func (m *Manager) render(ctx context.Context, j *job) (*Result, error) {
	// Wait for a render slot (cancellable while queued).
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-m.sem }()

	started := time.Now()
	m.mu.Lock()
	j.snap.Started = started
	r := j.snap.Recipe
	hash := j.snap.RecipeHash
	id := j.snap.ID
	m.mu.Unlock()
	format := strings.ToLower(r.Output.Format)
	target := discordlint.Target(r.Output.Target)

	// 1. Sources.
	m.setStage(j, StageProbe, 0, "looking up sources")
	if m.tools.FFmpeg == "" {
		return nil, errors.New("ffmpeg is not available on this server")
	}
	srcs, err := m.resolveSources(r.Sources)
	if err != nil {
		return nil, err
	}
	src := srcs.main()

	// Someone may have finished the same recipe while we were queued
	// (Submit already served an existing result): checked before the
	// compile so a cached recipe never pays for an autocrop detection.
	if m.st.HasResult(hash) {
		if res, err := m.LoadResult(hash); err == nil {
			res.Cached = true
			res.RenderMS = 0
			return res, nil
		}
	}

	// The gifsicle-only optimiser never decodes: no plan, no master.
	if isOptimizePreset(r.Output) {
		releaseScratch, err := m.admitOptimizeScratch(ctx, j, src.Path, r.Output)
		if err != nil {
			return nil, err
		}
		defer releaseScratch()
		scratch, cleanup, err := m.st.ScratchDir(id)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		items, err := m.renderOptimize(ctx, j, scratch, src, r, target)
		if err != nil {
			return nil, err
		}
		return m.deliver(ctx, j, started, r, hash, scratch, items)
	}

	// Phase 4: the lossless gifsicle fast path. An eligible GIF → GIF edit
	// (a trim on the source frame grid, a crop, an fps of (n-1)/n of the
	// source rate — every 2nd..4th frame dropped — and/or a
	// loop count; format gif, default encoder, no fit budget, target
	// none/attachment) is applied by gifsicle on the compressed frames —
	// no decode, no master, pixels untouched (phase4.go). Recipes the check
	// cannot prove eligible fall through to the decode pipeline below.
	if fp, ok := m.fastPathFor(src, r); ok {
		scratch, cleanup, err := m.st.ScratchDir(id)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		items, err := m.renderFastPath(ctx, j, scratch, src.Path, fp, r.Output, target)
		if err != nil {
			return nil, err
		}
		return m.deliver(ctx, j, started, r, hash, scratch, items)
	}

	// 2. Compile (autocrop resolved, overlay inputs bound to their blobs).
	plan, err := m.compile(ctx, srcs, r.Ops, r.Output)
	if err != nil {
		return nil, err
	}
	static := recipe.IsStaticFormat(format)
	if static {
		// A reversed static render is admitted by its reverse buffer
		// BEFORE the plan is cut to one frame: "-frames:v 1" shortens the
		// encode, not the decode — a reverse filter ahead of the output
		// holds the whole trimmed clip in RAM at the output size before it
		// can emit its first (= the last source) frame, exactly as a
		// reversed still does (admitReversed). The one-frame plan below
		// would pass admitScratch with one frame's worth, and with no
		// frame-count cap in the graph a reversed PNG export of an
		// untrimmed 4K clip would otherwise buffer 9.7 GiB bounded by
		// nothing but host RAM. A bounce ALONE is not checked here: its
		// graph is split[f][r];[r]reverse[rr];[f][rr]concat, so the first
		// output frame is the forward branch's first frame and "-frames:v
		// 1" ends the run before the reverse branch has buffered anything
		// (measured on FFmpeg 9: a 600-frame 1080p bounce with -frames:v 1
		// peaks at 4 MiB like a forward graph; a reverse graph at 4.8 GiB)
		// — a bounced PNG of an untrimmed clip costs one decoded frame. A
		// [reverse, bounce] stack sets Reversed and is covered: that
		// reverse must consume the whole clip before the split sees a
		// frame. Plan.Frames — doubled per bounce — is the count the
		// [bounce, reverse] case really buffers (the reverse consumes the
		// concat's 2N frames) and a conservative bound for [reverse,
		// bounce] (N buffered).
		if plan.Reversed {
			if err := m.admitReversed(plan, plan.Frames, "this render"); err != nil {
				return nil, err
			}
		}
		// Static formats encode the first frame only: cut the master there
		// (admission, progress and the encoders all see a one-frame plan).
		plan = oneFramePlan(plan)
	}
	if format == recipe.FormatFrames && plan.Frames > MaxExtractFrames {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRecipe, tooManyFramesMsg(plan.Frames))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 3./4. Scratch admission, then the master. The reservation is released
	// after the scratch dir is removed (defers run last-in-first-out). Text
	// overlays are written into the scratch dir and bound into the plan
	// before any encoder sees it.
	releaseScratch, err := m.admitScratch(ctx, j, plan, scratchFactor(r.Output))
	if err != nil {
		return nil, err
	}
	defer releaseScratch()
	scratch, cleanup, err := m.st.ScratchDir(id)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if plan, err = bindTextFiles(plan, scratch); err != nil {
		return nil, err
	}
	master, err := m.renderMaster(ctx, j, src.Path, plan, scratch, static)
	if err != nil {
		return nil, err
	}

	// 5./6. Encode + lint.
	m.setStage(j, StageEncode, pctEncodeStart, "encoding "+format)
	items, err := m.encodeOutputs(ctx, j, scratch, master, r.Output, target)
	if err != nil {
		return nil, err
	}
	return m.deliver(ctx, j, started, r, hash, scratch, items)
}

// encodeOutputs dispatches on the output format (and the fit engine) and
// returns the deliverables, primary first.
func (m *Manager) encodeOutputs(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) ([]produced, error) {
	format := strings.ToLower(out.Format)
	if out.FitBytes > 0 && fitFormats[format] {
		return m.renderFit(ctx, j, scratch, master, out, target)
	}
	if out.FitBytes > 0 {
		log.Printf("jobs: fitBytes ignored for format %q", format)
	}
	var (
		item produced
		err  error
	)
	switch format {
	case recipe.FormatGIF:
		if isGifskiOutput(out) {
			item, err = m.produceGifski(ctx, j, scratch, master, out, target)
		} else {
			item, err = m.produceGIF(ctx, j, scratch, master, out, target)
		}
	case recipe.FormatWebP:
		item, err = m.produceWebP(ctx, j, scratch, master, out, target)
	case recipe.FormatAPNG:
		item, err = m.produceAPNG(ctx, j, scratch, master, out, target)
	case recipe.FormatAVIF:
		item, err = m.produceAVIF(ctx, j, scratch, master, out, target)
	case recipe.FormatPNG, recipe.FormatJPEG:
		item, err = m.produceStatic(ctx, j, scratch, master, out, target)
	case recipe.FormatMP4, recipe.FormatWebM:
		item, err = m.produceVideo(ctx, j, scratch, master, out, target)
	case recipe.FormatFrames:
		return m.produceFrames(ctx, j, scratch, master, out)
	default:
		return nil, fmt.Errorf("%w: unsupported output format %q", ErrInvalidRecipe, out.Format)
	}
	if err != nil {
		return nil, err
	}
	return []produced{item}, nil
}

// deliver verifies, stages and commits the produced files and builds the
// manifest. items[0] is the primary output.
func (m *Manager) deliver(ctx context.Context, j *job, started time.Time, r recipe.Recipe, hash, scratch string, items []produced) (*Result, error) {
	if len(items) == 0 {
		return nil, errors.New("nothing was produced")
	}
	target := discordlint.Target(r.Output.Target)

	// 7. Verify.
	m.setStage(j, StageVerify, pctVerify, "verifying decode")
	kept := items[:0]
	for i, it := range items {
		if it.verify {
			if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, enc.VerifyDecodeArgs(it.path), nil); err != nil {
				if i == 0 {
					return nil, fmt.Errorf("verify: the encoded %s does not decode cleanly: %w", it.format, err)
				}
				log.Printf("jobs: %s dropped: does not decode cleanly: %v", it.name, err)
				continue
			}
		}
		kept = append(kept, it)
	}
	items = kept

	// 8. Stage + commit.
	m.progress(j, pctCommit, "writing result")
	files := make([]File, 0, len(items))
	for _, it := range items {
		f, err := m.fileFor(hash, it, target)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	res := &Result{
		RecipeHash: hash,
		Recipe:     r,
		Files:      files,
		Created:    time.Now().UTC(),
		RenderMS:   time.Since(started).Milliseconds(),
		Tools:      m.ToolVersions(),
	}
	staging := filepath.Join(scratch, "result")
	if err := writeStaging(staging, items, res); err != nil {
		return nil, err
	}
	if err := m.st.CommitResult(hash, staging); err != nil {
		return nil, fmt.Errorf("commit result: %w", err)
	}
	return res, nil
}

// fileFor builds the manifest entry of a produced file: facts from its
// report when present, else from the encoder's own knowledge.
func (m *Manager) fileFor(hash string, it produced, target discordlint.Target) (File, error) {
	fi, err := os.Stat(it.path)
	if err != nil {
		return File{}, fmt.Errorf("stat %s: %w", it.name, err)
	}
	f := File{
		Name:     it.name,
		URL:      m.opts.PublicBase + "/" + hash + "/" + it.name,
		Format:   it.format,
		Bytes:    fi.Size(),
		Width:    it.width,
		Height:   it.height,
		Frames:   it.frames,
		FPS:      it.fps,
		Duration: it.duration,
		Kind:     it.kind,
		Desc:     it.desc,
		Index:    it.index,
		Report:   it.report,
	}
	if it.kind == FileKindOutput || it.kind == FileKindAlternative {
		f.Limit = discordlint.Limit(target)
	}
	if rep := it.report; rep != nil {
		if rep.Width > 0 && rep.Height > 0 {
			f.Width, f.Height = rep.Width, rep.Height
		}
		if rep.Frames > 0 {
			f.Frames = rep.Frames
		}
		if rep.DurationMS > 0 {
			f.Duration = float64(rep.DurationMS) / 1000
			if rep.Frames > 0 {
				f.FPS = float64(rep.Frames) / f.Duration
			}
		}
	}
	return f, nil
}

// isOptimizePreset reports whether the recipe asks for the gifsicle-only
// GIF → GIF path.
func isOptimizePreset(out recipe.Output) bool {
	return strings.EqualFold(strings.TrimSpace(out.Preset), PresetOptimize)
}

// PresetOptimize is the Output.Preset of the no-decode GIF optimiser.
const PresetOptimize = "optimize"

// oneFramePlan returns a copy of p describing the first frame only (the
// master of a static output).
func oneFramePlan(p *graph.Plan) *graph.Plan {
	c := *p
	c.Frames = 1
	c.Duration = 0
	return &c
}

// oneFrameArgs limits an enc.MasterArgs argv to a single output frame by
// inserting "-frames:v 1" before the output path (always the last arg).
func oneFrameArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[:len(args)-1]...)
	out = append(out, "-frames:v", "1", args[len(args)-1])
	return out
}

// lookupSources resolves every source hash to a blob with probe info. Each
// blob is touched (store.TouchBlob) so the sweeper's TTL counts from this
// use rather than the upload: a source that keeps being rendered from is
// never swept out from under a job.
func (m *Manager) lookupSources(hashes []string) ([]*store.Blob, error) {
	blobs := make([]*store.Blob, 0, len(hashes))
	for i, h := range hashes {
		b, err := m.st.GetBlob(h)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("%w: source %d (%s) not found; upload it again", ErrInvalidRecipe, i, short(h))
			}
			return nil, fmt.Errorf("source %d (%s): %w", i, short(h), err)
		}
		if b.Info == nil {
			return nil, fmt.Errorf("%w: source %d (%s) has no probe info; upload it again", ErrInvalidRecipe, i, short(h))
		}
		if err := m.st.TouchBlob(h); err != nil {
			log.Printf("jobs: touch source %s: %v", short(h), err)
		}
		blobs = append(blobs, b)
	}
	return blobs, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// renderMaster decodes the source through the plan into frames.rgba and
// returns the filled Master (frames from file size, alpha from a scan).
// srcPath is the blob file, or the blob directory of an image sequence
// (enc joins plan.InputPattern). oneFrame cuts the master to its first
// frame (static outputs).
func (m *Manager) renderMaster(ctx context.Context, j *job, srcPath string, plan *graph.Plan, scratch string, oneFrame bool) (enc.Master, error) {
	m.setStage(j, StageMaster, pctMasterStart, "decoding source")
	path := filepath.Join(scratch, "frames.rgba")
	args := enc.MasterArgs(srcPath, plan, path)
	if oneFrame {
		args = oneFrameArgs(args)
	}
	err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, func(p ffrun.Progress) {
		frac := progressFraction(p, plan.Frames, plan.Duration)
		msg := fmt.Sprintf("decoding frame %d", p.Frame)
		if plan.Frames > 0 {
			msg = fmt.Sprintf("decoding frame %d/%d", p.Frame, plan.Frames)
		}
		if p.Speed != "" {
			msg += " (" + p.Speed + ")"
		}
		m.progress(j, pctMasterStart+frac*(pctMasterEnd-pctMasterStart), msg)
	})
	if err != nil {
		return enc.Master{}, fmt.Errorf("master render: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return enc.Master{}, fmt.Errorf("master render produced no file: %w", err)
	}
	frameBytes := int64(plan.Width) * int64(plan.Height) * 4
	frames := fi.Size() / frameBytes
	if frames <= 0 {
		return enc.Master{}, errors.New("master render produced no frames (is the trim range empty?)")
	}
	if fi.Size()%frameBytes != 0 {
		log.Printf("jobs: master %s has a partial trailing frame (%d bytes, frame %d bytes)", path, fi.Size(), frameBytes)
	}
	m.progress(j, pctMasterEnd, fmt.Sprintf("decoded %d frames; scanning alpha", frames))
	hasAlpha, err := scanMasterAlpha(path)
	if err != nil {
		return enc.Master{}, fmt.Errorf("master alpha scan: %w", err)
	}
	return enc.Master{
		Path:     path,
		Width:    plan.Width,
		Height:   plan.Height,
		FPS:      plan.FPS,
		Frames:   int(frames),
		HasAlpha: hasAlpha,
	}, nil
}

// progressFraction maps an ffmpeg progress block to 0..1 using the expected
// frame count when known, else the expected duration; 0 when neither is.
func progressFraction(p ffrun.Progress, frames int, duration float64) float64 {
	if p.Done {
		return 1
	}
	var f float64
	switch {
	case frames > 0:
		f = float64(p.Frame) / float64(frames)
	case duration > 0:
		f = float64(p.OutTimeMS) / (duration * 1000)
	default:
		return 0
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// scanMasterAlpha reports whether any pixel of the RGBA master has alpha < 255
// (looks at every 4th byte, chunked, stops at the first hit).
func scanMasterAlpha(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, masterAlphaChunk)
	for {
		n, err := io.ReadFull(f, buf)
		for i := 3; i < n; i += 4 {
			if buf[i] != 0xFF {
				return true, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, nil
			}
			return false, err
		}
	}
}

// ---- GIF / WebP (Phase 1 paths) ----------------------------------------------

// produceGIF is the plain GIF path: palette encode → gifsicle → lint (+
// fallback ladder) → produced.
func (m *Manager) produceGIF(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) (produced, error) {
	encoded, err := m.encodeGIF(ctx, j, scratch, master, out, nil, out.Lossy)
	if err != nil {
		return produced{}, err
	}
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(encoded)
	if err != nil {
		return produced{}, fmt.Errorf("read encoded output: %w", err)
	}
	data, report, err := m.lintGIF(ctx, j, scratch, data, target, out)
	if err != nil {
		return produced{}, fmt.Errorf("lint: %w", err)
	}
	applyMasterAlpha(&report, master)
	return m.finalFile(scratch, recipe.FormatGIF, data, &report)
}

// produceWebP is the plain WebP path.
func (m *Manager) produceWebP(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) (produced, error) {
	encoded, err := m.encodeWebP(ctx, j, scratch, master, out, nil, out.Quality)
	if err != nil {
		return produced{}, err
	}
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(encoded)
	if err != nil {
		return produced{}, fmt.Errorf("read encoded output: %w", err)
	}
	report, err := discordlint.LintWebP(data, target)
	if err != nil {
		return produced{}, fmt.Errorf("lint: %w", err)
	}
	applyMasterAlpha(&report, master)
	return m.finalFile(scratch, recipe.FormatWebP, data, &report)
}

// finalFile writes data as the primary output under scratch and describes
// it.
func (m *Manager) finalFile(scratch, format string, data []byte, report *discordlint.Report) (produced, error) {
	name := primaryBase + "." + extFor(format)
	final := filepath.Join(scratch, "final."+extFor(format))
	if err := os.WriteFile(final, data, 0o644); err != nil {
		return produced{}, fmt.Errorf("write linted output: %w", err)
	}
	return produced{path: final, name: name, format: format, kind: FileKindOutput, report: report, verify: true}, nil
}

// encodeGIF runs the ffmpeg palette pipeline and, when gifsicle is present,
// the -O2 post-pass with lossy. v pre-filters the master (fit rungs; nil =
// as-is). outPath files are named after tag so concurrent candidates do not
// collide. It returns the path of the best file.
func (m *Manager) encodeGIF(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, v *enc.Variant, lossy int) (string, error) {
	return m.encodeGIFAt(ctx, j, scratch, "", master, gifOptionsFor(out, v, master), enc.GifsicleOptions{Lossy: lossy, Colors: out.Colors, Loop: out.Loop})
}

// gifOptionsFor maps Output (+ variant) onto enc.GIFOptions.
func gifOptionsFor(out recipe.Output, v *enc.Variant, master enc.Master) enc.GIFOptions {
	return enc.GIFOptions{
		Colors:         out.Colors,
		Dither:         out.Dither,
		AlphaThreshold: out.AlphaThreshold,
		Matte:          out.Matte,
		Loop:           out.Loop,
		HasAlpha:       master.HasAlpha,
		Variant:        v,
	}
}

// encodeGIFAt is encodeGIF with explicit options and a file tag (""
// for the single-output path, a candidate id for fit candidates). The
// gifsicle pass rewrites the NETSCAPE loop block of everything it touches,
// so gopts.Loop restates Output.Loop (0 = forever, N = --loopcount=N) or a
// "play N+1 times" GIF would come out looping forever. --colors is only
// passed when the user asked for a palette size (with ordered dither).
//
// Between the two passes the held frames of base.gif are merged
// (mergeHoldsInFile): ffmpeg writes one frame per master frame, and every -O
// level of gifsicle turns a run of identical frames into one long frame plus
// a short clear-only frame that Discord drops together with its disposal
// (discordlint gif.noop-frame-disposal) — the optimiser must never see a
// hold run. Without gifsicle the merge still runs: it is pixel-exact and the
// file only gets smaller.
func (m *Manager) encodeGIFAt(ctx context.Context, j *job, scratch, tag string, master enc.Master, gopts enc.GIFOptions, sopts enc.GifsicleOptions) (string, error) {
	base := filepath.Join(scratch, "base"+tag+".gif")
	args := enc.GIFArgs(master, gopts, base)
	var onProgress func(ffrun.Progress)
	if tag == "" {
		onProgress = func(p ffrun.Progress) {
			frac := progressFraction(p, master.Frames, 0)
			m.progress(j, pctEncodeStart+frac*(pctEncodeEnd-pctEncodeStart)*0.8, fmt.Sprintf("gif palette pass: frame %d/%d", p.Frame, master.Frames))
		}
	}
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, onProgress); err != nil {
		return "", fmt.Errorf("gif encode: %w", err)
	}
	if tag == "" {
		m.progress(j, pctEncodeStart+(pctEncodeEnd-pctEncodeStart)*0.8, "merging held frames")
	}
	mergeHoldsInFile(base)
	if m.tools.Gifsicle == "" {
		return base, nil
	}
	if tag == "" {
		m.progress(j, pctEncodeStart+(pctEncodeEnd-pctEncodeStart)*0.8, "gifsicle optimise")
	}
	opt := filepath.Join(scratch, "opt"+tag+".gif")
	if sopts.Colors > 0 && sopts.Dither == "" {
		sopts.Dither = "o8"
	}
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleArgs(base, opt, sopts)); err != nil {
		return "", fmt.Errorf("gifsicle: %w", err)
	}
	os.Remove(base)
	return opt, nil
}

// encodeWebP runs libwebp_anim. v pre-filters the master (nil = as-is);
// quality is the -q:v (0 = default).
func (m *Manager) encodeWebP(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, v *enc.Variant, quality int) (string, error) {
	return m.encodeWebPAt(ctx, j, scratch, "", master, enc.WebPOptions{Quality: quality, Lossless: out.Lossless, Loop: out.Loop, Variant: v})
}

// encodeWebPAt is encodeWebP with explicit options and a file tag.
func (m *Manager) encodeWebPAt(ctx context.Context, j *job, scratch, tag string, master enc.Master, o enc.WebPOptions) (string, error) {
	path := filepath.Join(scratch, "enc"+tag+".webp")
	args := enc.WebPArgs(master, o, path)
	var onProgress func(ffrun.Progress)
	if tag == "" {
		onProgress = func(p ffrun.Progress) {
			frac := progressFraction(p, master.Frames, 0)
			m.progress(j, pctEncodeStart+frac*(pctEncodeEnd-pctEncodeStart), fmt.Sprintf("webp encode: frame %d/%d", p.Frame, master.Frames))
		}
	}
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, onProgress); err != nil {
		return "", fmt.Errorf("webp encode: %w", err)
	}
	return path, nil
}

// mergeHoldsInFile rewrites the GIF at path with its held frames merged
// (discordlint.MergeGIFHolds) and returns how many frames went. It is best
// effort: a read/parse/write failure is logged and the file stays as it was —
// the encode continues with the unmerged frames and the lint (plus the hold
// repair) still has the last word.
func mergeHoldsInFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("jobs: merge held frames: %v", err)
		return 0
	}
	out, merged, err := discordlint.MergeGIFHolds(data)
	if err != nil {
		log.Printf("jobs: merge held frames of %s: %v", filepath.Base(path), err)
		return 0
	}
	if merged == 0 {
		return 0
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		log.Printf("jobs: merge held frames: %v", err)
		return 0
	}
	return merged
}

// lintGIF lints+fixes data and, when structural failures remain
// (hasStructuralError: error-level checks, or gif.noop-frame-disposal at any
// level — so held frames are repaired for target none too) and gifsicle is
// available, walks the re-encode ladder, re-linting after each rung:
// "gifsicle --colors N" (skipped when gif.noop-frame-disposal is the only
// structural failure — a palette pass keeps the frame structure), then the
// hold repair (repairGIFHolds: coalesce → merge held frames → -O2 --careful,
// falling back to the coalesced all-disposal-2 file). It returns the best
// bytes and report.
func (m *Manager) lintGIF(ctx context.Context, j *job, scratch string, data []byte, target discordlint.Target, out recipe.Output) ([]byte, discordlint.Report, error) {
	report, fixed, err := discordlint.LintGIF(data, target, true)
	if err != nil {
		return nil, report, err
	}
	if len(fixed) > 0 {
		data = fixed
	}
	if !hasStructuralError(report) || m.tools.Gifsicle == "" {
		return data, report, nil
	}

	colors := out.Colors
	if colors <= 0 {
		colors = enc.DefaultColors
	}
	// Every rung restates Output.Loop (gifsicle would otherwise reset the
	// NETSCAPE count to forever).
	if ladderTriesColors(report) {
		const name = "gifsicle --colors"
		if err := ctx.Err(); err != nil {
			return nil, report, err
		}
		m.progress(j, pctLint, fmt.Sprintf("re-encoding for Discord (%s)", name))
		cand, rep, err := m.ladderColors(ctx, scratch, data, target, enc.GifsicleOptions{Colors: colors, Lossy: out.Lossy, Loop: out.Loop})
		if err != nil {
			log.Printf("jobs: %s failed: %v", name, err)
		} else {
			data, report = cand, rep
		}
		if !hasStructuralError(report) {
			return data, report, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, report, err
	}
	note := func(step string) { m.progress(j, pctLint, fmt.Sprintf("re-encoding for Discord (%s)", step)) }
	cand, rep, err := m.repairGIFHolds(ctx, scratch, "", data, target, enc.GifsicleOptions{Lossy: out.Lossy, Loop: out.Loop}, note)
	if err != nil {
		if ctx.Err() != nil {
			return nil, report, ctx.Err()
		}
		log.Printf("jobs: hold repair failed: %v", err)
		return data, report, nil
	}
	return cand, rep, nil
}

// ladderTriesColors reports whether the ladder's "gifsicle --colors" rung is
// worth running for rep: it is when any structural error other than
// gif.noop-frame-disposal failed (a palette pass keeps the frame structure,
// so it can never fix that one).
func ladderTriesColors(rep discordlint.Report) bool {
	return hasStructuralError(rep) && !onlyHoldsFailed(rep)
}

// ladderColors is the ladder's first rung: one "gifsicle -O2 --colors N"
// pass over data, linted and fixed.
func (m *Manager) ladderColors(ctx context.Context, scratch string, data []byte, target discordlint.Target, opts enc.GifsicleOptions) ([]byte, discordlint.Report, error) {
	in := filepath.Join(scratch, "ladder-in.gif")
	outPath := filepath.Join(scratch, "ladder-colors.gif")
	defer os.Remove(in)
	defer os.Remove(outPath)
	if err := os.WriteFile(in, data, 0o644); err != nil {
		return nil, discordlint.Report{}, err
	}
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleArgs(in, outPath, opts)); err != nil {
		return nil, discordlint.Report{}, err
	}
	return lintGIFFile(outPath, target)
}

// lintGIFFile reads a GIF and lints it with the byte fixer applied.
func lintGIFFile(path string, target discordlint.Target) ([]byte, discordlint.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, discordlint.Report{}, err
	}
	rep, fixed, err := discordlint.LintGIF(data, target, true)
	if err != nil {
		return nil, rep, err
	}
	if len(fixed) > 0 {
		data = fixed
	}
	return data, rep, nil
}

// holdRepairNote is appended to a deliverable's description when the hold
// repair replaced its bytes.
const holdRepairNote = "held frames re-encoded for Discord"

// holdRepairOptions are the two gifsicle passes of repairGIFHolds for the
// caller's post-pass options: coalesce is step A (-U --disposal=background,
// no optimiser, nothing lossy), reopt step B (-O2 --careful plus the
// caller's Lossy/Colors/Dither). Both restate the caller's Loop.
func holdRepairOptions(sopts enc.GifsicleOptions) (coalesce, reopt enc.GifsicleOptions) {
	coalesce = enc.GifsicleOptions{Unoptimize: true, DisposeBackground: true, NoOptimize: true, Loop: sopts.Loop}
	reopt = enc.GifsicleOptions{Lossy: sopts.Lossy, Colors: sopts.Colors, Dither: sopts.Dither, Threads: sopts.Threads, OptimizeLevel: 2, Loop: sopts.Loop}
	if reopt.Colors > 0 && reopt.Dither == "" {
		reopt.Dither = "o8"
	}
	return coalesce, reopt
}

// repairGIFHolds re-encodes a GIF whose frame structure Discord would play
// wrong (gif.noop-frame-disposal: a frame that leaves the picture unchanged
// but carries the disposal that clears it — Discord drops such frames) and
// doubles as the generic structural re-encode of the fallback ladder:
//
//	A. gifsicle -U --disposal=background, no optimiser: every frame becomes a
//	   full-canvas disposal-2 frame (pixel-exact), in which every held frame
//	   is a harmless no-op — discordlint.MergeGIFHolds then folds the holds
//	   into their first frame.
//	B. gifsicle -O2 --careful (+ the caller's Lossy/Colors/Dither) over A:
//	   with no run of identical frames left the optimiser has nothing to
//	   turn into a clear-only frame.
//
// B's bytes are returned unless its report still has a structural failure
// (hasStructuralError, which counts the hold rule at any level) or the pass
// failed, in which case A's are — larger, but structurally the
// plainest file gifsicle can write. With Lossy 0 and Colors 0 both are
// pixel-exact against data. tag keeps the scratch file names unique (fit
// candidates run concurrently); note, when not nil, is told which step
// runs. An error means step A failed and the caller keeps what it had.
func (m *Manager) repairGIFHolds(ctx context.Context, scratch, tag string, data []byte, target discordlint.Target, sopts enc.GifsicleOptions, note func(step string)) ([]byte, discordlint.Report, error) {
	if m.tools.Gifsicle == "" {
		return nil, discordlint.Report{}, errors.New("gifsicle is not available")
	}
	if note == nil {
		note = func(string) {}
	}
	in := filepath.Join(scratch, "holds"+tag+"-in.gif")
	flat := filepath.Join(scratch, "holds"+tag+"-flat.gif")
	opt := filepath.Join(scratch, "holds"+tag+"-opt.gif")
	defer func() {
		for _, p := range []string{in, flat, opt} {
			os.Remove(p)
		}
	}()
	coalesce, reopt := holdRepairOptions(sopts)

	note("gifsicle -U --disposal=background")
	if err := os.WriteFile(in, data, 0o644); err != nil {
		return nil, discordlint.Report{}, err
	}
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleArgs(in, flat, coalesce)); err != nil {
		return nil, discordlint.Report{}, fmt.Errorf("gifsicle coalesce: %w", err)
	}
	mergeHoldsInFile(flat)

	note("gifsicle -O2 --careful")
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleArgs(flat, opt, reopt)); err != nil {
		if ctx.Err() != nil {
			return nil, discordlint.Report{}, ctx.Err()
		}
		log.Printf("jobs: hold repair: gifsicle -O2: %v", err)
	} else if cand, rep, err := lintGIFFile(opt, target); err != nil {
		log.Printf("jobs: hold repair: lint after gifsicle -O2: %v", err)
	} else if !hasStructuralError(rep) {
		return cand, rep, nil
	}
	return lintGIFFile(flat, target)
}

// RuleRenderAlpha is the info-level check jobs appends to a report when the
// linter's structural transparency flag disagrees with the rendered master's
// pixel scan; its detail keeps the structural verdict.
const RuleRenderAlpha = "render.alpha"

// applyMasterAlpha makes Report.HasAlpha mean "the rendered frames carry
// transparency" for outputs this pipeline encoded itself. discordlint's flag
// is structural — a GIF frame that uses its transparent index, a WebP frame
// with an ALPH chunk — and frame-diff optimised encoders (ffmpeg's gif
// encoder, gifsicle -O2, libwebp_anim) use transparency for unchanged
// pixels, so an opaque animation is routinely flagged. The master's alpha
// scan (every pixel of every frame, before encoding) is the truth here.
// When the two disagree the structural detail is kept in an info check.
func applyMasterAlpha(report *discordlint.Report, master enc.Master) {
	structural := report.HasAlpha
	report.HasAlpha = master.HasAlpha
	if structural == master.HasAlpha {
		return
	}
	detail := "rendered frames are opaque (master alpha scan); the file's structural transparency (frame-diff optimisation) is not source alpha"
	if master.HasAlpha {
		detail = "rendered frames carry alpha (master alpha scan) although the encoded file has no structural transparency"
	}
	report.Checks = append(report.Checks, discordlint.Check{
		Rule:   RuleRenderAlpha,
		Level:  discordlint.LevelInfo,
		OK:     true,
		Detail: detail,
	})
}

// hasStructuralError reports whether a check failed that a re-encode through
// gifsicle could plausibly fix: any failed LevelError check but the byte /
// dimension / duration limits (those need the fit engine), plus a failed
// gif.noop-frame-disposal at ANY level (isStructuralFailure). The latter is
// what makes lintGIF repair the held frames of a "target none" output too.
//
// It is about repairing, not about delivering: whether a report is OK for its
// target stays hasErrorCheck's business (a warn-level hold failure that could
// not be repaired never makes a fit candidate not-ok; fitCandidates.smallest
// merely prefers files without one when nothing fit).
func hasStructuralError(rep discordlint.Report) bool {
	for _, c := range rep.Checks {
		if isStructuralFailure(c) {
			return true
		}
	}
	return false
}

// isStructuralFailure is hasStructuralError / onlyHoldsFailed for one check.
// The hold rule is structural whatever its level: discordlint reports it as
// an error for Discord targets and as a warning for target none, but the file
// is the same one — this tool's own gifsicle -O pass makes the clear-only
// frames out of a clean source, target none is the SPA's default for the
// Optimize preset, and the output usually ends up on Discord anyway. The
// repair is pixel-exact and size-neutral, so there is no reason to withhold
// it. Every other rule keeps the LevelError requirement.
func isStructuralFailure(c discordlint.Check) bool {
	if c.OK || isLimitRule(c.Rule) {
		return false
	}
	return c.Level == discordlint.LevelError || c.Rule == discordlint.RuleGIFNoopFrameDisposal
}

// repairIfOnlyHolds is the lint step of the paths that do not walk the full
// ladder (the optimize preset and the fit candidates): when the only
// structural failure of report is gif.noop-frame-disposal — at any level, so
// for every target including none (isStructuralFailure) — and gifsicle is
// available, the hold repair replaces data and report (repaired = true).
// Without it every candidate of a source with held frames would fail the
// lint for a Discord target and no fit could ever be found, and a target-none
// optimize would deliver the clear-only frames gifsicle just made. A failed
// repair is logged and the inputs come back unchanged (for target none that
// is still a deliverable file: the rule is a warning there).
//
// Known limitation (DESIGN.md §5.3): data already carries the caller's
// --lossy pass and step B of the repair applies sopts.Lossy again, so a
// repaired candidate is second-generation lossy (about 0.5 dB at equal size)
// and costs about 2.3x the gifsicle work of an unrepaired one.
func (m *Manager) repairIfOnlyHolds(ctx context.Context, scratch, tag string, data []byte, report discordlint.Report, target discordlint.Target, sopts enc.GifsicleOptions) ([]byte, discordlint.Report, bool) {
	if !onlyHoldsFailed(report) || m.tools.Gifsicle == "" {
		return data, report, false
	}
	cand, rep, err := m.repairGIFHolds(ctx, scratch, tag, data, target, sopts, nil)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("jobs: hold repair failed: %v", err)
		}
		return data, report, false
	}
	return cand, rep, true
}

// onlyHoldsFailed reports whether gif.noop-frame-disposal failed — at any
// level: error for a Discord target, warn for target none — and is the only
// structural failure (hasStructuralError's notion: byte/dimension/duration
// limits and the other rules' warnings do not count). That is the one
// structural failure a palette pass cannot touch and the hold repair
// (repairGIFHolds) always can.
func onlyHoldsFailed(rep discordlint.Report) bool {
	holds := false
	for _, c := range rep.Checks {
		if !isStructuralFailure(c) {
			continue
		}
		if c.Rule != discordlint.RuleGIFNoopFrameDisposal {
			return false
		}
		holds = true
	}
	return holds
}

// hasErrorCheck reports whether any LevelError check failed (the report is
// not OK for a Discord target).
func hasErrorCheck(rep discordlint.Report) bool {
	for _, c := range rep.Checks {
		if !c.OK && c.Level == discordlint.LevelError {
			return true
		}
	}
	return false
}

// isLimitRule matches rule ids about size/dimension/duration budgets and
// target shape (the emote/sticker rules of every format), i.e. anything a
// structural re-encode cannot fix.
func isLimitRule(rule string) bool {
	r := strings.ToLower(rule)
	for _, kw := range []string{"size", "limit", "bytes", "dims", "dimension", "duration", "emote", "sticker", "fit."} {
		if strings.Contains(r, kw) {
			return true
		}
	}
	return false
}

// writeStaging lays out the result dir: every produced file under its
// result name, report.json for the primary, manifest.json last.
func writeStaging(staging string, items []produced, res *Result) error {
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	for _, it := range items {
		dst := filepath.Join(staging, it.name)
		if it.path == dst {
			continue
		}
		if err := os.Rename(it.path, dst); err != nil {
			return fmt.Errorf("stage %s: %w", it.name, err)
		}
	}
	if rep := items[0].report; rep != nil {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
		if err := os.WriteFile(filepath.Join(staging, reportName), data, 0o644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	man, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, store.ManifestName), man, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// decodeResult parses a manifest.
func decodeResult(data []byte) (*Result, error) {
	var res Result
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&res); err != nil {
		return nil, err
	}
	if res.RecipeHash == "" || len(res.Files) == 0 {
		return nil, errors.New("manifest is incomplete")
	}
	return &res, nil
}
