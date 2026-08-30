package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Phase 4 render paths (DESIGN.md §10 item 4 / phase4 design):
//
//   - MP4 / WebM: the master goes through the opaque video tails
//     (enc.MP4Args / enc.WebMArgs — flattened onto Output.Matte, padded to
//     even dimensions, libx264 CRF / libvpx-vp9 CRF) and is linted with
//     discordlint.LintVideo. Emote/sticker targets are refused at Submit
//     (validatePhase4Output). Output.Quality IS the encoder CRF for these
//     formats (0 = enc.DefaultX264CRF / DefaultVP9CRF — the docs/web
//     contract), not a 1..100 slider. The fit search runs the generic
//     ladder with the CRF knob (fit.KnobFor); the knob value goes verbatim
//     into enc.MP4Options.CRF / WebMOptions.CRF.
//   - gifski: Output.Encoder "gifski" (gif + target none/attachment only)
//     encodes the master's PNG frames with gifski instead of the ffmpeg
//     palette pipeline; Output.Quality is gifski's --quality (0 = 90) and
//     the fit knob when FitBytes is set. gifski always writes an infinite
//     loop, so a non-default Output.Loop is restated by a gifsicle pass (and
//     refused with a clear error when gifsicle is missing — never silently
//     delivered looping forever).
//   - lossless gifsicle fast path: a GIF source whose recipe only trims on
//     the source frame grid, crops, drops to a rate reachable by dropping
//     every 2nd..4th frame — (n-1)/n of the source rate (1/2, 2/3 or 3/4),
//     not source/N — and/or sets the loop count (format gif, default
//     encoder, no fit budget, target
//     none/attachment) never decodes at all — gifsicle applies the edits on
//     the compressed frames (enc.GifsicleFastPathArgs), pixels untouched.
//     Eligibility is decided up front (fastPathFor); anything it cannot
//     prove eligible silently takes the normal decode pipeline.
//
// Bounce admission lives in scratch.go/proxy.go: graph doubles Plan.Frames
// and Plan.Duration per bounce, so the master estimate needs no extra
// factor, and the still/proxy admission treats a bounced plan like a
// reversed one (the bounce's reverse branch buffers output-sized frames).

// EncoderGifski is the recipe.Output.Encoder value of the gifski HQ path.
const EncoderGifski = "gifski"

// FastPathDesc is the File.Desc of the lossless gifsicle fast path.
const FastPathDesc = "lossless gifsicle (no re-encode)"

// fastPathFPSTolerance is how far (relative) a wanted fps may be from the
// source rate (keep every frame) or a drop-every-N of it for the lossless
// fast path. It only needs to absorb the web UI's 2-3 decimal rounding of
// prefilled rates (~0.02% at 30 fps) — a genuinely different rate (29 on a
// 30 fps source) must fall through to the decode pipeline's exact resample,
// unlike the opt-in optimize preset's looser optimizeFPSTolerance.
const fastPathFPSTolerance = 0.002

// isGifskiOutput reports whether the recipe asks for the gifski encoder.
func isGifskiOutput(out recipe.Output) bool {
	return strings.EqualFold(strings.TrimSpace(out.Encoder), EncoderGifski)
}

// gifskiQuality is the effective gifski --quality (0 = 90, clamped 1..100).
func gifskiQuality(q int) int {
	if q <= 0 {
		return enc.DefaultGifskiQuality
	}
	return min(max(q, 1), 100)
}

// flattenedFormat reports whether format is delivered flattened onto the
// matte with no alpha channel at all (jpeg and the Phase 4 video formats):
// the master alpha scan must not override such a report's HasAlpha — the
// file truly has none.
func flattenedFormat(format string) bool {
	return format == recipe.FormatJPEG || recipe.IsVideoFormat(format)
}

// validatePhase4Output enforces the Phase 4 format/encoder/target rules at
// Submit time so invalid combinations answer 4xx before any work starts:
// mp4/webm never for emote/sticker (Discord cannot use video there), and
// Output.Encoder must be "" or "gifski" — gifski only with format gif and a
// none/attachment target (its per-frame local palettes break on Discord
// emotes/stickers), and never with the optimize preset (which bypasses the
// encoders entirely).
func validatePhase4Output(out recipe.Output) error {
	format := strings.ToLower(strings.TrimSpace(out.Format))
	target := discordlint.Target(strings.ToLower(strings.TrimSpace(out.Target)))
	restricted := target == discordlint.TargetEmote || target == discordlint.TargetSticker
	if recipe.IsVideoFormat(format) && restricted {
		return fmt.Errorf("%w: %s output cannot be a Discord %s — video formats cannot be emotes/stickers; pick gif/webp/apng, or an attachment target", ErrInvalidRecipe, format, target)
	}
	switch e := strings.ToLower(strings.TrimSpace(out.Encoder)); e {
	case "":
	case EncoderGifski:
		if format != recipe.FormatGIF {
			return fmt.Errorf("%w: encoder %q applies to format gif only (output format is %q)", ErrInvalidRecipe, EncoderGifski, out.Format)
		}
		if restricted {
			return fmt.Errorf("%w: the gifski encoder cannot be used for the Discord %s target (its local palettes break there); use the default encoder or an attachment target", ErrInvalidRecipe, target)
		}
		if isOptimizePreset(out) {
			return fmt.Errorf("%w: the optimize preset never re-encodes and cannot use the gifski encoder", ErrInvalidRecipe)
		}
	default:
		return fmt.Errorf("%w: unknown encoder %q (\"\" for the default ffmpeg palette pipeline, or %q)", ErrInvalidRecipe, out.Encoder, EncoderGifski)
	}
	return nil
}

// ---- MP4 / WebM --------------------------------------------------------------

// produceVideo is the single-output mp4/webm path: master → video tail →
// LintVideo → produced. The tails flatten onto Output.Matte and pad to even
// dimensions themselves; odd output sizes are fixed, never refused.
func (m *Manager) produceVideo(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) (produced, error) {
	format := strings.ToLower(out.Format)
	path := filepath.Join(scratch, "enc."+extFor(format))
	if err := m.encodeVideoAt(ctx, j, "", master, format, out, nil, 0, path); err != nil {
		return produced{}, err
	}
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(path)
	if err != nil {
		return produced{}, fmt.Errorf("read encoded output: %w", err)
	}
	report, err := discordlint.LintVideo(format, data, target)
	if err != nil {
		return produced{}, fmt.Errorf("lint: %w", err)
	}
	// No applyMasterAlpha: the output is flattened (flattenedFormat).
	item, err := m.finalFile(scratch, format, data, &report)
	if err != nil {
		return produced{}, err
	}
	fillVideoFacts(&item, master)
	return item, nil
}

// fillVideoFacts sets the manifest fallback facts of a video deliverable
// from the master (the report's own parsed values win where present): the
// even-padded frame size, frame count, rate and duration.
func fillVideoFacts(item *produced, master enc.Master) {
	item.width, item.height = evenDim(master.Width), evenDim(master.Height)
	item.frames = master.Frames
	item.fps = master.FPS
	if master.FPS > 0 {
		item.duration = float64(master.Frames) / master.FPS
	}
}

// evenDim is n padded up to the next even number (the video tails' pad).
func evenDim(n int) int { return n + n&1 }

// encodeVideoAt runs one mp4/webm encode of the master (through v; fit
// candidates pass their CRF knob as crf > 0, the single-output path passes 0
// so Output.Quality — which IS the CRF for video, 0 = encoder default —
// decides). tag "" is the single-output path and reports progress.
func (m *Manager) encodeVideoAt(ctx context.Context, j *job, tag string, master enc.Master, format string, out recipe.Output, v *enc.Variant, crf int, outPath string) error {
	if crf <= 0 && out.Quality > 0 {
		crf = out.Quality // Output.Quality is the raw CRF (enc caps it per codec)
	}
	var args []string
	switch format {
	case recipe.FormatMP4:
		args = enc.MP4Args(master, enc.MP4Options{CRF: crf, Matte: out.Matte, Variant: v}, outPath)
	case recipe.FormatWebM:
		args = enc.WebMArgs(master, enc.WebMOptions{CRF: crf, Matte: out.Matte, Variant: v}, outPath)
	default:
		return fmt.Errorf("%q is not a video format", format)
	}
	var onProgress func(ffrun.Progress)
	if tag == "" {
		onProgress = func(p ffrun.Progress) {
			frac := progressFraction(p, master.Frames, 0)
			m.progress(j, pctEncodeStart+frac*(pctEncodeEnd-pctEncodeStart), fmt.Sprintf("%s encode: frame %d/%d", format, p.Frame, master.Frames))
		}
	}
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, onProgress); err != nil {
		return fmt.Errorf("%s encode: %w", format, err)
	}
	return nil
}

// ---- gifski ------------------------------------------------------------------

// produceGifski is the single-output HQ GIF path: master → PNG frames →
// gifski (→ gifsicle loop restatement) → LintGIF (with the usual fallback
// ladder) → produced. Output.Quality is gifski's --quality (0 = 90).
func (m *Manager) produceGifski(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) (produced, error) {
	if m.tools.Gifski == "" {
		return produced{}, errors.New("the gifski encoder is not available on this server (use the default encoder)")
	}
	m.progress(j, pctEncodeStart, "gifski: writing frames")
	dir := filepath.Join(scratch, "gifski-frames")
	frames, err := m.renderPNGFrames(ctx, dir, master, nil, 1)
	if err != nil {
		return produced{}, err
	}
	m.progress(j, pctEncodeStart+(pctEncodeEnd-pctEncodeStart)*0.3, fmt.Sprintf("gifski: encoding %d frames", len(frames)))
	q := gifskiQuality(out.Quality)
	path := filepath.Join(scratch, "enc-gifski.gif")
	if err := m.runGifski(ctx, scratch, "", frames, master.FPS, q, out.Loop, path); err != nil {
		return produced{}, err
	}
	os.RemoveAll(dir)
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(path)
	if err != nil {
		return produced{}, fmt.Errorf("read encoded output: %w", err)
	}
	data, report, err := m.lintGIF(ctx, j, scratch, data, target, out)
	if err != nil {
		return produced{}, fmt.Errorf("lint: %w", err)
	}
	applyMasterAlpha(&report, master)
	item, err := m.finalFile(scratch, recipe.FormatGIF, data, &report)
	if err != nil {
		return produced{}, err
	}
	item.desc = fmt.Sprintf("gifski · quality %d", q)
	return item, nil
}

// runGifski encodes the ordered PNG frames with gifski and, when the recipe
// asks for a finite loop count, restates it with a gifsicle pass — gifski
// always writes an infinite NETSCAPE loop (enc.GifskiArgs). A finite loop
// with no gifsicle available is refused up front (before any encode work):
// silently delivering a forever-looping file the user did not ask for would
// even pass the lint, so the honest answer is an error naming the missing
// tool. dir/tag name the loop pass's temp file (fit candidates run
// concurrently).
func (m *Manager) runGifski(ctx context.Context, dir, tag string, frames []string, fps float64, quality, loop int, outPath string) error {
	if loop > 0 && m.tools.Gifsicle == "" {
		return fmt.Errorf("a loop count of %d needs gifsicle to restate gifski's always-infinite loop, and gifsicle is not available on this server (use loop forever or the default encoder)", loop)
	}
	args := enc.GifskiArgs(frames, fps, quality, 0, outPath)
	if args == nil {
		return errors.New("gifski: no frames to encode")
	}
	if err := ffrun.Run(ctx, m.tools.Gifski, args); err != nil {
		return fmt.Errorf("gifski: %w", err)
	}
	if loop <= 0 {
		return nil // forever is what gifski wrote
	}
	fixed := filepath.Join(dir, "gifski-loop"+tag+".gif")
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleArgs(outPath, fixed, enc.GifsicleOptions{Loop: loop})); err != nil {
		return fmt.Errorf("gifsicle loop pass: %w", err)
	}
	return os.Rename(fixed, outPath)
}

// ---- lossless gifsicle fast path --------------------------------------------

// fastPath is a resolved, eligible fast-path render: the source's parsed
// frame table plus the gifsicle options realising the recipe's edits.
type fastPath struct {
	facts gifFacts
	opts  enc.GifsicleFastPathOptions
}

// fastPathSpec is the pure half of the eligibility decision: the recipe's
// edits reduced to what gifsicle can apply losslessly.
type fastPathSpec struct {
	crop    *enc.GifsicleCrop
	trim    recipe.TrimParams
	trimSet bool
	wantFPS float64 // 0 = keep the source rate
}

// checkFastPathSpec decides eligibility from the recipe and the main
// source's probe info alone (phase4 design §5): single GIF source, format
// gif with the default encoder, no fit budget, no output canvas/quality
// knobs that would re-encode, target none/attachment, and an op stack that
// is a subset of {one trim, one crop, one fps, loop} with valid params
// (anything unparseable or out of range is simply not eligible — the normal
// pipeline then reports it properly). The frame-grid and drop-every-N fps
// checks need the source's timing and live in fastPathFor.
func checkFastPathSpec(info recipe.ProbeInfo, r recipe.Recipe) (fastPathSpec, bool) {
	var s fastPathSpec
	out := r.Output
	if len(r.Sources) != 1 {
		return s, false
	}
	if !strings.EqualFold(info.Codec, "gif") || info.Kind == recipe.KindSequence {
		return s, false
	}
	if strings.ToLower(strings.TrimSpace(out.Format)) != recipe.FormatGIF || strings.TrimSpace(out.Encoder) != "" {
		return s, false
	}
	if out.FitBytes != 0 || out.Lossy != 0 || out.Colors != 0 || out.Lossless {
		return s, false
	}
	if out.Width != 0 || out.Height != 0 {
		return s, false
	}
	if isOptimizePreset(out) {
		return s, false // the optimize preset is its own gifsicle path
	}
	switch discordlint.Target(strings.ToLower(strings.TrimSpace(out.Target))) {
	case discordlint.TargetEmote, discordlint.TargetSticker:
		return s, false // emote/sticker need the fit engine
	}
	wantFPS := out.FPS
	fpsSet := out.FPS > 0
	for _, op := range r.Ops {
		switch op.Kind {
		case recipe.OpTrim:
			var p recipe.TrimParams
			if s.trimSet || json.Unmarshal(op.Params, &p) != nil || p.Start < 0 {
				return s, false
			}
			s.trim, s.trimSet = p, true
		case recipe.OpCrop:
			var p recipe.CropParams
			if s.crop != nil || json.Unmarshal(op.Params, &p) != nil {
				return s, false
			}
			if p.X < 0 || p.Y < 0 || p.W < 1 || p.H < 1 || p.X+p.W > info.Width || p.Y+p.H > info.Height {
				return s, false
			}
			s.crop = &enc.GifsicleCrop{X: p.X, Y: p.Y, W: p.W, H: p.H}
		case recipe.OpFPS:
			var p recipe.FPSParams
			if fpsSet || json.Unmarshal(op.Params, &p) != nil || p.FPS <= 0 {
				return s, false
			}
			wantFPS, fpsSet = p.FPS, true
		default:
			return s, false
		}
	}
	s.wantFPS = wantFPS
	return s, true
}

// fastPathFor completes the eligibility decision against the actual source:
// gifsicle must be available, the GIF's frame table must parse, any timeline
// edit needs a uniform delay table agreeing with the probed rate
// (uniformDelays), a trim must land on the source frame grid (within
// graph.FrameTolerance) and a lower fps must equal the source rate with
// every 2nd..4th frame dropped, matched at the tight fastPathFPSTolerance
// (dropEveryNTol). It returns the executable fast path, or ok = false to
// send the recipe down the normal decode pipeline.
func (m *Manager) fastPathFor(src *store.Blob, r recipe.Recipe) (*fastPath, bool) {
	if m.tools.Gifsicle == "" || src.Info == nil || src.IsSequence() {
		return nil, false
	}
	spec, ok := checkFastPathSpec(*src.Info, r)
	if !ok {
		return nil, false
	}
	facts, err := readGIFFacts(src.Path)
	if err != nil {
		return nil, false
	}
	// Timeline edits (trim by frame index, drop-every-N) are only lossless
	// when the GIF really plays on the constant grid Info.FPS describes —
	// a variable-delay source (held frames are common) would map the trim's
	// seconds onto the wrong content window. Crop/loop-only recipes keep
	// each frame's own delay and stay eligible regardless.
	if spec.trimSet || spec.wantFPS > 0 {
		if !uniformDelays(facts.delays, src.Info.FPS) {
			return nil, false
		}
	}
	fp := &fastPath{facts: facts, opts: enc.GifsicleFastPathOptions{Crop: spec.crop, Loop: r.Output.Loop}}
	if spec.trimSet {
		a, b, ok := trimFrameRange(spec.trim, src.Info.FPS, facts.frames)
		if !ok {
			return nil, false
		}
		fp.opts.FrameStart, fp.opts.FrameEnd = a, b
	}
	// A wanted fps must be the source rate itself (within the tight
	// fastPathFPSTolerance: keep every frame) or a drop-every-N of it.
	// Anything else — a HIGHER rate in particular, which the decode path
	// realises by duplicating frames — is not losslessly expressible and
	// falls through to the decode pipeline's exact fps resample.
	if f, srcFPS := spec.wantFPS, src.Info.FPS; f > 0 && math.Abs(f-srcFPS) > fastPathFPSTolerance*srcFPS {
		if f > srcFPS {
			return nil, false
		}
		drop, err := dropEveryNTol(srcFPS, f, fastPathFPSTolerance)
		if err != nil || drop == 0 {
			return nil, false
		}
		fp.opts.DropEveryN = drop
	}
	return fp, true
}

// uniformDelays reports whether every frame of the GIF holds for the same
// delay and that delay agrees with the probed rate (Info.FPS is ffmpeg's
// r_frame_rate base-cadence estimate — for a variable-delay GIF it is not
// a playback grid; see graph.Plan.SourceVFR). Timeline edits (trim by
// frame index, drop-every-N) are only lossless on such a constant grid.
func uniformDelays(delays []int, fps float64) bool {
	if len(delays) == 0 || fps <= 0 {
		return false
	}
	d := delays[0]
	for _, v := range delays[1:] {
		if v != d {
			return false
		}
	}
	if d <= 0 {
		return false // 0/1 cs delays are clamped to ffmpeg's 10 fps default
	}
	return math.Abs(100/float64(d)-fps) <= 0.01*fps
}

// trimFrameRange maps a trim's second bounds onto the source frame grid:
// first kept frame a = Start*fps, last kept frame b = End*fps - 1 (the trim
// window is half-open [Start, End)). Both bounds must land on the grid
// within graph.FrameTolerance frames — the UI's from-scrubber trims do —
// else the recipe is not eligible. End <= 0 or past the last frame is open
// (b = 0, enc.GifsicleFastPathOptions.FrameEnd semantics); a trim keeping
// only frame 0 would need FrameEnd 0 too and is therefore not eligible.
func trimFrameRange(trim recipe.TrimParams, srcFPS float64, frames int) (a, b int, ok bool) {
	if srcFPS <= 0 || frames <= 0 {
		return 0, 0, false
	}
	a, ok = gridIndex(trim.Start, srcFPS)
	if !ok || a >= frames {
		return 0, 0, false
	}
	if trim.End <= 0 {
		return a, 0, true
	}
	if trim.End*srcFPS >= float64(frames)-graph.FrameTolerance {
		return a, 0, true // end at/after the last frame: keep the whole tail
	}
	n, ok := gridIndex(trim.End, srcFPS)
	if !ok || n <= a {
		return 0, 0, false
	}
	if n-1 <= 0 {
		return 0, 0, false // [0, one frame): FrameEnd 0 would mean "open"
	}
	return a, n - 1, true
}

// gridIndex maps a time in seconds onto the source frame grid; ok is false
// when it does not land on a frame boundary within graph.FrameTolerance.
func gridIndex(t, fps float64) (int, bool) {
	f := t * fps
	i := math.Round(f)
	if i < 0 || math.Abs(f-i) > graph.FrameTolerance {
		return 0, false
	}
	return int(i), true
}

// renderFastPath executes an eligible fast path: one gifsicle run
// (enc.GifsicleFastPathArgs), then LintGIF as usual. The pixels of the kept
// frames are byte-identical to the source's.
func (m *Manager) renderFastPath(ctx context.Context, j *job, scratch, srcPath string, fp *fastPath, out recipe.Output, target discordlint.Target) ([]produced, error) {
	m.setStage(j, StageEncode, pctEncodeStart, "lossless gifsicle pass")
	path := filepath.Join(scratch, "fast.gif")
	if err := ffrun.Run(ctx, m.tools.Gifsicle, enc.GifsicleFastPathArgs(srcPath, path, fp.facts.delays, fp.opts)); err != nil {
		return nil, fmt.Errorf("gifsicle fast path: %w", err)
	}
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fast-path gif: %w", err)
	}
	data, report, err := m.lintGIF(ctx, j, scratch, data, target, out)
	if err != nil {
		return nil, fmt.Errorf("lint: %w", err)
	}
	item, err := m.finalFile(scratch, recipe.FormatGIF, data, &report)
	if err != nil {
		return nil, err
	}
	item.desc = FastPathDesc
	return []produced{item}, nil
}
