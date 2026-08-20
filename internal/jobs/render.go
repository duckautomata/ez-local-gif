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
	blobs, err := m.lookupSources(r.Sources)
	if err != nil {
		return nil, err
	}
	src := blobs[0]

	// The gifsicle-only optimiser never decodes: no plan, no master.
	if isOptimizePreset(r.Output) {
		if m.st.HasResult(hash) {
			if res, err := m.LoadResult(hash); err == nil {
				res.Cached = true
				res.RenderMS = 0
				return res, nil
			}
		}
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

	// 2. Compile.
	plan, err := graph.Compile(*src.Info, r.Ops, r.Output)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	if plan.Width <= 0 || plan.Height <= 0 {
		return nil, fmt.Errorf("compiled plan has an empty frame size (%dx%d)", plan.Width, plan.Height)
	}
	static := recipe.IsStaticFormat(format)
	if static {
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

	// 3. Someone may have finished the same recipe while we were queued.
	if m.st.HasResult(hash) {
		if res, err := m.LoadResult(hash); err == nil {
			res.Cached = true
			res.RenderMS = 0
			return res, nil
		}
	}

	// 4. Scratch admission, then the master. The reservation is released
	// after the scratch dir is removed (defers run last-in-first-out).
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
		item, err = m.produceGIF(ctx, j, scratch, master, out, target)
	case recipe.FormatWebP:
		item, err = m.produceWebP(ctx, j, scratch, master, out, target)
	case recipe.FormatAPNG:
		item, err = m.produceAPNG(ctx, j, scratch, master, out, target)
	case recipe.FormatAVIF:
		item, err = m.produceAVIF(ctx, j, scratch, master, out, target)
	case recipe.FormatPNG, recipe.FormatJPEG:
		item, err = m.produceStatic(ctx, j, scratch, master, out, target)
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

// lintGIF lints+fixes data and, when structural errors remain and gifsicle
// is available, walks the re-encode ladder (--colors N → -U -O2 --careful →
// -U), re-linting after each rung. It returns the best bytes and report.
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
	rungs := []struct {
		name  string
		opts  enc.GifsicleOptions
		strip bool // drop the -O flag → plain -U (coalesced full frames)
	}{
		{"gifsicle --colors", enc.GifsicleOptions{Colors: colors, Lossy: out.Lossy, Loop: out.Loop}, false},
		{"gifsicle -U -O2 --careful", enc.GifsicleOptions{Unoptimize: true, OptimizeLevel: 2, Lossy: out.Lossy, Loop: out.Loop}, false},
		{"gifsicle -U", enc.GifsicleOptions{Unoptimize: true, Loop: out.Loop}, true},
	}
	in := filepath.Join(scratch, "ladder-in.gif")
	for i, rung := range rungs {
		if err := ctx.Err(); err != nil {
			return nil, report, err
		}
		m.progress(j, pctLint, fmt.Sprintf("re-encoding for Discord (%s)", rung.name))
		if err := os.WriteFile(in, data, 0o644); err != nil {
			return nil, report, err
		}
		outPath := filepath.Join(scratch, fmt.Sprintf("ladder-%d.gif", i))
		args := enc.GifsicleArgs(in, outPath, rung.opts)
		if rung.strip {
			args = stripOptimizeFlag(args)
		}
		if err := ffrun.Run(ctx, m.tools.Gifsicle, args); err != nil {
			log.Printf("jobs: %s failed: %v", rung.name, err)
			continue
		}
		cand, err := os.ReadFile(outPath)
		if err != nil {
			log.Printf("jobs: read %s output: %v", rung.name, err)
			continue
		}
		rep, fixed, err := discordlint.LintGIF(cand, target, true)
		if err != nil {
			log.Printf("jobs: lint after %s: %v", rung.name, err)
			continue
		}
		if len(fixed) > 0 {
			cand = fixed
		}
		data, report = cand, rep
		if !hasStructuralError(rep) {
			break
		}
	}
	return data, report, nil
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

// hasStructuralError reports whether a LevelError check failed that a
// re-encode through gifsicle could plausibly fix (i.e. anything but byte /
// dimension / duration limits, which need the fit engine).
func hasStructuralError(rep discordlint.Report) bool {
	for _, c := range rep.Checks {
		if c.OK || c.Level != discordlint.LevelError {
			continue
		}
		if isLimitRule(c.Rule) {
			continue
		}
		return true
	}
	return false
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

// stripOptimizeFlag removes gifsicle's -O<n>/--optimize flags so that a
// GifsicleOptions{Unoptimize: true} argv becomes the plain "-U" rung of the
// fallback ladder (enc always emits an -O level).
func stripOptimizeFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if (strings.HasPrefix(a, "-O") && len(a) <= 3) || strings.HasPrefix(a, "--optimize") {
			continue
		}
		out = append(out, a)
	}
	return out
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
