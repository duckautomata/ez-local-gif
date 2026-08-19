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

	// 2. Compile.
	plan, err := graph.Compile(*src.Info, r.Ops, r.Output)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	if plan.Width <= 0 || plan.Height <= 0 {
		return nil, fmt.Errorf("compiled plan has an empty frame size (%dx%d)", plan.Width, plan.Height)
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
	releaseScratch, err := m.admitScratch(ctx, j, plan)
	if err != nil {
		return nil, err
	}
	defer releaseScratch()
	scratch, cleanup, err := m.st.ScratchDir(id)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	master, err := m.renderMaster(ctx, j, src.Path, plan, scratch)
	if err != nil {
		return nil, err
	}

	// 5. Encode.
	m.setStage(j, StageEncode, pctEncodeStart, "encoding "+format)
	var encoded string
	switch format {
	case "gif":
		encoded, err = m.encodeGIF(ctx, j, scratch, master, r.Output)
	case "webp":
		encoded, err = m.encodeWebP(ctx, j, scratch, master, r.Output)
	default:
		err = fmt.Errorf("%w: unsupported output format %q", ErrInvalidRecipe, r.Output.Format)
	}
	if err != nil {
		return nil, err
	}

	// 6. Lint (+ fix / fallback ladder).
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(encoded)
	if err != nil {
		return nil, fmt.Errorf("read encoded output: %w", err)
	}
	var report discordlint.Report
	switch format {
	case "gif":
		data, report, err = m.lintGIF(ctx, j, scratch, data, target, r.Output)
	case "webp":
		report, err = discordlint.LintWebP(data, target)
	}
	if err != nil {
		return nil, fmt.Errorf("lint: %w", err)
	}
	applyMasterAlpha(&report, master)
	final := filepath.Join(scratch, "final."+format)
	if err := os.WriteFile(final, data, 0o644); err != nil {
		return nil, fmt.Errorf("write linted output: %w", err)
	}

	// 7. Verify.
	m.setStage(j, StageVerify, pctVerify, "verifying decode")
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, enc.VerifyDecodeArgs(final), nil); err != nil {
		return nil, fmt.Errorf("verify: the encoded %s does not decode cleanly: %w", format, err)
	}

	// 8. Stage + commit.
	m.progress(j, pctCommit, "writing result")
	name := "out." + format
	file := File{
		Name:     name,
		URL:      m.opts.PublicBase + "/" + hash + "/" + name,
		Format:   format,
		Bytes:    int64(len(data)),
		Width:    report.Width,
		Height:   report.Height,
		Frames:   report.Frames,
		Duration: float64(report.DurationMS) / 1000,
		Limit:    discordlint.Limit(target),
		Report:   &report,
	}
	if file.Width == 0 || file.Height == 0 {
		file.Width, file.Height = master.Width, master.Height
	}
	if file.Frames == 0 {
		file.Frames = master.Frames
	}
	if report.DurationMS > 0 && report.Frames > 0 {
		file.FPS = float64(report.Frames) / file.Duration
	} else {
		file.FPS = master.FPS
	}
	res := &Result{
		RecipeHash: hash,
		Recipe:     r,
		Files:      []File{file},
		Created:    time.Now().UTC(),
		RenderMS:   time.Since(started).Milliseconds(),
		Tools:      m.ToolVersions(),
	}
	staging := filepath.Join(scratch, "result")
	if err := writeStaging(staging, final, name, &report, res); err != nil {
		return nil, err
	}
	if err := m.st.CommitResult(hash, staging); err != nil {
		return nil, fmt.Errorf("commit result: %w", err)
	}
	return res, nil
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
func (m *Manager) renderMaster(ctx context.Context, j *job, srcPath string, plan *graph.Plan, scratch string) (enc.Master, error) {
	m.setStage(j, StageMaster, pctMasterStart, "decoding source")
	path := filepath.Join(scratch, "frames.rgba")
	args := enc.MasterArgs(srcPath, plan, path)
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

// encodeGIF runs the ffmpeg palette pipeline and, when gifsicle is present,
// the -O2 post-pass. It returns the path of the best file.
func (m *Manager) encodeGIF(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output) (string, error) {
	base := filepath.Join(scratch, "base.gif")
	args := enc.GIFArgs(master, enc.GIFOptions{
		Colors:         out.Colors,
		Dither:         out.Dither,
		AlphaThreshold: out.AlphaThreshold,
		Matte:          out.Matte,
		Loop:           out.Loop,
		HasAlpha:       master.HasAlpha,
	}, base)
	err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, func(p ffrun.Progress) {
		frac := progressFraction(p, master.Frames, 0)
		m.progress(j, pctEncodeStart+frac*(pctEncodeEnd-pctEncodeStart)*0.8, fmt.Sprintf("gif palette pass: frame %d/%d", p.Frame, master.Frames))
	})
	if err != nil {
		return "", fmt.Errorf("gif encode: %w", err)
	}
	if m.tools.Gifsicle == "" {
		return base, nil
	}
	m.progress(j, pctEncodeStart+(pctEncodeEnd-pctEncodeStart)*0.8, "gifsicle optimise")
	opt := filepath.Join(scratch, "opt.gif")
	// gifsicle rewrites the NETSCAPE loop block of everything it touches, so
	// the count ffmpeg wrote (-loop N) is restated (Loop: 0 = forever, N =
	// --loopcount=N) or a "play N+1 times" GIF would come out looping forever.
	gopts := enc.GifsicleOptions{Lossy: out.Lossy, Colors: out.Colors, Loop: out.Loop}
	if out.Colors > 0 {
		gopts.Dither = "o8"
	}
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleArgs(base, opt, gopts)); err != nil {
		return "", fmt.Errorf("gifsicle: %w", err)
	}
	return opt, nil
}

// encodeWebP runs libwebp_anim.
func (m *Manager) encodeWebP(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output) (string, error) {
	path := filepath.Join(scratch, "enc.webp")
	args := enc.WebPArgs(master, enc.WebPOptions{
		Quality:  out.Quality,
		Lossless: out.Lossless,
		Loop:     out.Loop,
	}, path)
	err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, func(p ffrun.Progress) {
		frac := progressFraction(p, master.Frames, 0)
		m.progress(j, pctEncodeStart+frac*(pctEncodeEnd-pctEncodeStart), fmt.Sprintf("webp encode: frame %d/%d", p.Frame, master.Frames))
	})
	if err != nil {
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
// dimension / duration limits, which need the Phase 2 fit engine).
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

// isLimitRule matches rule ids about size/dimension/duration budgets.
func isLimitRule(rule string) bool {
	r := strings.ToLower(rule)
	for _, kw := range []string{"size", "limit", "bytes", "dims", "dimension", "duration", "emote-", "sticker-"} {
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

// writeStaging lays out the result dir: <name>, report.json, manifest.json.
func writeStaging(staging, finalPath, name string, report *discordlint.Report, res *Result) error {
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	if err := os.Rename(finalPath, filepath.Join(staging, name)); err != nil {
		return fmt.Errorf("stage output: %w", err)
	}
	rep, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "report.json"), rep, 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
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
