package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// defaultFPS is used when neither the fps op, Output.FPS nor the probe
// provide a frame rate (stills, or sources with an unknown rate).
const defaultFPS = 10

// minFPS is the lowest frame rate we accept before snapping; anything lower
// would render as "fps=0" after rounding to 3 decimals.
const minFPS = 0.001

// Upper bounds. They keep a typo (or a hostile recipe) from asking ffmpeg
// for a frame it cannot allocate or a master that fills tmpfs.
const (
	// MaxDim is the largest width or height a resize/canvas/Output may
	// request and the largest side any resulting frame may have.
	MaxDim = 8192
	// MaxPixels caps the area of every resulting frame (32 megapixels =
	// 8192x4096; 8K UHD 7680x4320 still fits).
	MaxPixels = 32 << 20
	// MinSpeed and MaxSpeed bound each speed op's factor and the product of
	// all speed ops.
	MinSpeed = 0.05
	MaxSpeed = 100
	// MaxMasterBytes caps the expected RGBA master (Width*Height*4*Frames)
	// when the frame count is known.
	MaxMasterBytes = 8 << 30
)

// Image-sequence frame delays (the "delay" op and recipe.SequenceInfo.DelayMS),
// in milliseconds.
const (
	// MinDelayMS and MaxDelayMS bound a per-frame delay (1 ms .. 60 s).
	MinDelayMS = 1
	MaxDelayMS = 60000
	// DefaultDelayMS applies when neither the delay op nor the probe gives a
	// delay (the GIF-maker convention of 100 ms per frame).
	DefaultDelayMS = 100
)

// Fit modes shared by the resize op and Output.Fit.
const (
	fitContain = "contain"
	fitCover   = "cover"
	fitExact   = "exact"
)

// mainInput is the link label every single-stream graph starts from.
const mainInput = "[0:v]"

// errorf builds a "graph: ..." error. (The inner fmt.Errorf keeps this a
// vet-recognised printf wrapper and supports %w.)
func errorf(format string, args ...any) error {
	return fmt.Errorf("graph: %w", fmt.Errorf(format, args...))
}

// opErrorf builds a "graph: op N (kind): ..." error for a specific op.
func opErrorf(d decodedOp, format string, args ...any) error {
	return fmt.Errorf("graph: op %d (%s): %w", d.index, d.kind, fmt.Errorf(format, args...))
}

// decodedOp is an Op whose params have been decoded into the recipe struct
// for its kind (params is nil for OpUnpremultiply).
type decodedOp struct {
	index  int
	kind   string
	params any
}

// decodeOps decodes every op's params per kind. Unknown kinds and malformed
// params are errors; semantic validation happens when the op is applied.
func decodeOps(ops []recipe.Op) ([]decodedOp, error) {
	decoded := make([]decodedOp, 0, len(ops))
	for i, op := range ops {
		var params any
		switch op.Kind {
		case recipe.OpTrim:
			params = new(recipe.TrimParams)
		case recipe.OpCrop:
			params = new(recipe.CropParams)
		case recipe.OpResize:
			params = new(recipe.ResizeParams)
		case recipe.OpCanvas:
			params = new(recipe.CanvasParams)
		case recipe.OpFPS:
			params = new(recipe.FPSParams)
		case recipe.OpSpeed:
			params = new(recipe.SpeedParams)
		case recipe.OpFlip:
			params = new(recipe.FlipParams)
		case recipe.OpRotate:
			params = new(recipe.RotateParams)
		case recipe.OpDelay:
			params = new(recipe.DelayParams)
		case recipe.OpUnpremultiply:
			// no params
		default:
			return nil, errorf("op %d: unknown op kind %q", i, op.Kind)
		}
		if params != nil && len(bytes.TrimSpace(op.Params)) > 0 {
			if err := json.Unmarshal(op.Params, params); err != nil {
				return nil, errorf("op %d (%s): invalid params: %w", i, op.Kind, err)
			}
		}
		decoded = append(decoded, decodedOp{index: i, kind: op.Kind, params: params})
	}
	return decoded, nil
}

// compiler holds the state threaded through the stages: the current frame
// size, whether the frame carries alpha, and the filter stages emitted so
// far.
type compiler struct {
	src recipe.ProbeInfo
	out recipe.Output
	seq *sequence // set for image-sequence sources

	w, h     int  // current frame size
	hasAlpha bool // current frame carries alpha (source alpha, a merged alpha stream or transparent padding)
	srcAlpha bool // the source pixels carry alpha (in the main stream or a merged alpha stream)
	// depth is the bit depth of the frames reaching the hoisted unpremultiply:
	// the source's (src.Bits), or 8 once a head has converted them to rgba.
	depth int

	// input is the filter text the stage chain is appended to: mainInput
	// ("[0:v]") or the alpha-stream merge head (which already ends in ",").
	input  string
	stages []string // filter stages, joined with ","
	plan   Plan
}

// sequence holds the resolved facts of an image-sequence source.
type sequence struct {
	count int     // frame count (0 = unknown)
	rate  float64 // 1000/delay ms rounded to 3 decimals; the image2 -framerate
}

func newCompiler(src recipe.ProbeInfo, out recipe.Output) *compiler {
	return &compiler{
		src:      src,
		out:      out,
		w:        src.Width,
		h:        src.Height,
		hasAlpha: src.HasAlpha,
		srcAlpha: src.HasAlpha,
		depth:    src.Bits,
		input:    mainInput,
		plan:     Plan{OutLabel: outLabel, Speed: 1},
	}
}

func (c *compiler) emit(stage string) {
	c.stages = append(c.stages, stage)
}

// singleFrame reports whether the source yields exactly one frame (a still,
// or an image sequence of one frame): no fps filter (it would emit nothing),
// no trim, Frames 1.
func (c *compiler) singleFrame() bool {
	return c.src.IsStill || (c.seq != nil && c.seq.count == 1)
}

// sourceFPS is the frame rate the source is decoded at: the image2
// -framerate for sequences, else the probed rate (0 = unknown).
func (c *compiler) sourceFPS() float64 {
	if c.seq != nil {
		return c.seq.rate
	}
	if c.src.FPS > 0 && !math.IsInf(c.src.FPS, 0) {
		return c.src.FPS
	}
	return 0
}

// ---------------------------------------------------------------------------
// Source heads: image sequences (input args + mixed-size normalisation) and
// separate alpha streams (alphamerge). Both run before every other stage.
// ---------------------------------------------------------------------------

// source resolves how the main input is read. It fills InputArgs /
// InputPattern / SourceFPS for image sequences and the merge head for sources
// whose alpha lives in a separate stream.
func (c *compiler) source(ops []decodedOp) error {
	if c.src.AlphaStream < 0 {
		return errorf("source alpha stream index must be >= 0 (got %d)", c.src.AlphaStream)
	}
	if c.src.ColorStream < 0 {
		return errorf("source colour stream index must be >= 0 (got %d)", c.src.ColorStream)
	}
	if c.src.AlphaStream > 0 && c.src.AlphaStream == c.src.ColorStream {
		return errorf("source alpha stream and colour stream are both v:%d", c.src.AlphaStream)
	}
	if c.src.Kind == recipe.KindSequence {
		return c.sequenceSource(ops)
	}
	switch {
	case c.src.AlphaStream > 0:
		c.alphaStreamHead()
	case c.src.ColorStream > 0:
		// Animated AVIF without alpha: the animation track is not the first
		// video stream (the one-frame primary item is), so address it.
		c.input = fmt.Sprintf("[0:v:%d]", c.src.ColorStream)
	}
	return nil
}

// sequenceSource handles recipe.KindSequence: the image2 demuxer reads the
// frames at -framerate 1000/delay (the hoisted delay op wins over
// SequenceInfo.DelayMS, which wins over DefaultDelayMS) from -start_number 1,
// so the frames are CFR at that rate and every later stage (trim seeks, speed,
// fps, geometry) works as for a video. Every sequence runs with
// -reinit_filter 0 behind the guarding head (sequenceHead): uploaded frames
// are stored as-is and may differ per frame in pixel format or size, either
// of which makes fftools rebuild the filtergraph and lose frames.
func (c *compiler) sequenceSource(ops []decodedOp) error {
	info := c.src.Sequence
	if info == nil {
		return errorf("image sequence source has no sequence info")
	}
	if !strings.Contains(info.Pattern, "%") {
		return errorf("image sequence pattern %q is not an image2 pattern (want e.g. %%06d.png)", info.Pattern)
	}
	if c.src.AlphaStream != 0 {
		return errorf("image sequence sources cannot carry a separate alpha stream")
	}
	delay := info.DelayMS
	switch {
	case delay <= 0:
		delay = DefaultDelayMS
	case delay > MaxDelayMS:
		return errorf("sequence delay %d ms exceeds the %d ms maximum", delay, MaxDelayMS)
	}
	for _, d := range ops { // hoisted, last one wins
		p, ok := d.params.(*recipe.DelayParams)
		if !ok {
			continue
		}
		if p.MS < MinDelayMS || p.MS > MaxDelayMS {
			return opErrorf(d, "ms must be between %d and %d (got %d)", MinDelayMS, MaxDelayMS, p.MS)
		}
		delay = p.MS
	}
	count := info.Count
	if count <= 0 {
		count = c.src.Frames
	}
	c.seq = &sequence{count: count, rate: SequenceFPS(delay)}
	c.plan.InputPattern = info.Pattern
	c.plan.SourceFPS = c.seq.rate
	// Arg order is fixed: "-f image2" first (force the demuxer explicitly, so
	// the render opens the frames exactly like the probe's "-f image2" read —
	// extension-based demuxer guessing fails for patterns ffmpeg does not
	// recognise), then the image2 options, then the fftools per-input option.
	c.plan.InputArgs = append(c.plan.InputArgs, "-f", "image2", "-framerate", fnum(c.seq.rate), "-start_number", "1", "-reinit_filter", "0")
	c.sequenceHead(info.Mixed)
	return nil
}

// sequenceHead guards every image-sequence source against per-frame decode
// parameter changes; for mixed-size sequences it also normalises the size:
// each frame is scaled to fit W x H (the largest frame, keeping aspect) and
// centred on a transparent W x H canvas, so every later stage sees one
// constant size.
//
// Sequences are special for fftools: by default it rebuilds the whole
// filtergraph whenever ANY decoded frame parameter changes — not just the
// size but also the pixel format or colour range (rgba vs rgb24 PNGs, one
// indexed PNG-8 among RGBA frames, yuvj420p vs yuvj444p JPEGs) — and the
// rebuild loses the frame the fps filter holds at every change (verified on
// FFmpeg 9.0.1: six mixed frames came out as six copies of the last one).
// probe.SequenceInfo.Mixed only reflects sampled *size* differences, so
// format mixes (and unsampled size changes) arrive with Mixed=false;
// sequenceSource therefore always sets -reinit_filter 0 to keep one graph,
// and this head must cope with the changing parameters itself:
//
//   - scale reconfigures itself per frame and converts each frame to the
//     format negotiated with the next stage; for frames already W x H in that
//     format it is a pixel-exact pass-through (verified byte-identical on a
//     uniform rgba sequence), so well-behaved sequences are unchanged. A bare
//     format=rgba would NOT do: under -reinit_filter 0 the format filter is
//     only a negotiation constraint, not a converter, so a mid-sequence
//     rgb24 frame reaches the next stage reinterpreted as rgba bytes
//     (garbage; verified on FFmpeg 9.0.1).
//   - pad (mixed sizes only) needs eval=frame: its centring offsets are
//     otherwise fixed at the first frame's size and the uncovered canvas is
//     left uninitialised.
//   - premultiply/unpremultiply do not cope at all (they read the configured
//     size and silently corrupt), so the scale here runs on straight alpha
//     and the hoisted unpremultiply, which comes right after, sees the
//     constant parameters. For premultiplied sources this is the
//     premultiplied-scale order anyway (scale, then divide the alpha out).
func (c *compiler) sequenceHead(mixed bool) {
	w, h := c.src.Width, c.src.Height
	c.emit(fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos", w, h))
	if !mixed {
		return
	}
	c.emit("format=rgba")
	c.emit(fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=0x00000000:eval=frame", w, h))
	c.hasAlpha = true // transparent padding
	c.depth = 8
}

// alphaStreamHead merges a separate single-plane alpha stream (ffmpeg's mov
// demuxer exposes AVIF alpha as stream AlphaStream next to the colour stream
// ColorStream — v:0 for a still AVIF, the animation track (typically v:2,
// alpha v:3) for an animated one, because the one-frame primary item comes
// first) into the main stream before every other stage:
//
//	[0:v:C]format=rgba[c];[0:v:N]format=gray[a];[c][a]alphamerge,
//
// alphamerge takes 8-bit inputs only, so the merged frames are 8-bit rgba
// with straight alpha; a hoisted unpremultiply follows at gbrap.
func (c *compiler) alphaStreamHead() {
	c.input = fmt.Sprintf("[0:v:%d]format=rgba[c];[0:v:%d]format=gray[a];[c][a]alphamerge,", c.src.ColorStream, c.src.AlphaStream)
	c.hasAlpha, c.srcAlpha = true, true
	c.depth = 8
}

// SequenceFPS returns the image2 frame rate for a per-frame delay in
// milliseconds: 1000/delayMS rounded to 3 decimals (what "-framerate" is
// given, so callers computing durations agree with ffmpeg). delayMS <= 0
// means DefaultDelayMS.
func SequenceFPS(delayMS int) float64 {
	if delayMS <= 0 {
		delayMS = DefaultDelayMS
	}
	return round3(1000 / float64(delayMS))
}

// ---------------------------------------------------------------------------
// Temporal stages: unpremultiply (hoisted), trim (input args), speed, fps.
// ---------------------------------------------------------------------------

func (c *compiler) temporal(ops []decodedOp) error {
	c.unpremultiply(ops)
	if c.seq == nil {
		c.plan.InputArgs = append(c.plan.InputArgs, decoderArgs(c.src)...)
	}
	if err := c.trim(ops); err != nil {
		return err
	}
	if err := c.speed(ops); err != nil {
		return err
	}
	return c.fps(ops)
}

// unpremultiply hoists the unpremultiply op to run right after decode (or
// right after the alpha-stream merge / sequence head), at the frames'
// native bit depth so 10/12-bit ProRes alpha edges are not truncated to 8
// bits before the division. It is a no-op for sources without alpha.
//
// FFmpeg >= 8 negotiates AVFrame.alpha_mode across filter links:
// unpremultiply declares its input premultiplied, while decoders (ProRes,
// rawvideo, ...) tag frames alpha_mode=unspecified, so on its own the
// negotiator auto-inserts premultiply_dynamic in front of it and the pair
// cancels out (the toggle became a no-op, minus rounding). setparams tags
// the frames premultiplied first so no conversion is inserted; verified
// pixel-exact on FFmpeg 9.0.1 (see alpha_ffmpeg_test.go). Its position
// relative to the format stage does not matter (the auto-inserted format
// conversion passes alpha_mode through); it sits next to its consumer.
func (c *compiler) unpremultiply(ops []decodedOp) {
	if !c.srcAlpha {
		return
	}
	for _, d := range ops {
		if d.kind != recipe.OpUnpremultiply {
			continue
		}
		format := "gbrap"
		switch c.depth {
		case 10:
			format = "gbrap10le"
		case 12:
			format = "gbrap12le"
		}
		c.emit("format=" + format)
		c.emit("setparams=alpha_mode=premultiplied")
		c.emit("unpremultiply=inplace=1")
		return
	}
}

// decoderArgs forces the libvpx decoders for VP8/VP9 sources with alpha:
// ffmpeg's native vp8/vp9 decoders drop the alpha plane.
func decoderArgs(src recipe.ProbeInfo) []string {
	if !src.HasAlpha {
		return nil
	}
	switch strings.ToLower(src.Codec) {
	case "vp9":
		return []string{"-c:v", "libvpx-vp9"}
	case "vp8":
		return []string{"-c:v", "libvpx"}
	}
	return nil
}

// trim intersects all trim ops (each selects a source-time range) and
// expresses the result as -ss/-to input seek args. No trim filter is
// emitted; input seeking rebases timestamps to 0 so the rest of the graph
// sees a clip starting at t=0.
func (c *compiler) trim(ops []decodedOp) error {
	start, end := 0.0, 0.0 // end 0 = to the end of the source
	trimmed := false
	for _, d := range ops {
		p, ok := d.params.(*recipe.TrimParams)
		if !ok {
			continue
		}
		s, e := round3(p.Start), round3(p.End)
		if e < 0 { // TrimParams: End <= 0 means "to the end"
			e = 0
		}
		if s < 0 {
			return opErrorf(d, "start must be >= 0 (got %s)", fnum(s))
		}
		if e > 0 && e <= s {
			return opErrorf(d, "end (%s s) must be after start (%s s)", fnum(e), fnum(s))
		}
		if s == 0 && e == 0 {
			continue // whole source
		}
		if c.src.IsStill {
			return opErrorf(d, "the source is a still image and cannot be trimmed")
		}
		if c.singleFrame() {
			return opErrorf(d, "the source has a single frame and cannot be trimmed")
		}
		trimmed = true
		start = math.Max(start, s)
		if e > 0 && (end == 0 || e < end) {
			end = e
		}
	}
	if !trimmed {
		return nil
	}
	if end > 0 && end <= start {
		return errorf("trim ranges do not overlap (start %s s, end %s s)", fnum(start), fnum(end))
	}
	if dur := c.sourceDuration(); dur > 0 {
		if start >= dur {
			return errorf("trim start (%s s) is at or beyond the end of the source (%s s)", fnum(start), fnum(dur))
		}
		if end >= dur {
			end = 0 // reading to the end is the same clip and avoids a -to past EOF
		}
	}
	if start > 0 {
		c.plan.InputArgs = append(c.plan.InputArgs, "-ss", fnum(start))
	}
	if end > 0 {
		c.plan.InputArgs = append(c.plan.InputArgs, "-to", fnum(end))
	}
	c.plan.TrimStart, c.plan.TrimEnd = start, end
	return nil
}

// speed multiplies all speed factors and emits setpts=PTS/<factor>. It must
// precede the fps filter so the retimed stream is resampled to CFR. Each
// factor and their product must lie in [MinSpeed, MaxSpeed].
func (c *compiler) speed(ops []decodedOp) error {
	factor := 1.0
	for _, d := range ops {
		p, ok := d.params.(*recipe.SpeedParams)
		if !ok {
			continue
		}
		if !(p.Factor > 0) || math.IsInf(p.Factor, 0) {
			return opErrorf(d, "factor must be > 0 (got %s)", fexact(p.Factor))
		}
		if p.Factor < MinSpeed || p.Factor > MaxSpeed {
			return opErrorf(d, "factor must be between %s and %s (got %s)", fexact(MinSpeed), fexact(MaxSpeed), fexact(p.Factor))
		}
		factor *= p.Factor
	}
	if factor < MinSpeed || factor > MaxSpeed {
		return errorf("combined speed factor %s must be between %s and %s", fexact(factor), fexact(MinSpeed), fexact(MaxSpeed))
	}
	c.plan.Speed = factor
	if factor != 1 {
		c.emit("setpts=PTS/" + fexact(factor))
	}
	return nil
}

// fps resolves the requested frame rate (fps op → Output.FPS → source fps →
// defaultFPS), snaps it for the output format and always emits an fps
// filter so the master is constant-frame-rate. For image sequences the
// source fps is the image2 -framerate (from the delay), so by default the
// master keeps one frame per image.
func (c *compiler) fps(ops []decodedOp) error {
	requested, origin := 0.0, ""
	for _, d := range ops { // last fps op wins
		p, ok := d.params.(*recipe.FPSParams)
		if !ok {
			continue
		}
		if !(p.FPS > 0) || math.IsInf(p.FPS, 0) {
			return opErrorf(d, "fps must be > 0 (got %s)", fexact(p.FPS))
		}
		requested, origin = p.FPS, fmt.Sprintf("op %d (fps)", d.index)
	}
	switch {
	case requested > 0:
	case c.out.FPS < 0 || math.IsNaN(c.out.FPS) || math.IsInf(c.out.FPS, 0):
		return errorf("output fps must be >= 0 (got %s)", fexact(c.out.FPS))
	case c.out.FPS > 0:
		requested, origin = c.out.FPS, "output fps"
	case c.sourceFPS() > 0:
		requested, origin = c.sourceFPS(), "source fps"
	default:
		requested, origin = defaultFPS, "default fps"
	}
	if requested < minFPS {
		return errorf("%s %s is too low (minimum %s)", origin, fexact(requested), fexact(minFPS))
	}
	snapped := SnapFPS(c.out.Format, requested)
	if snapped <= 0 {
		return errorf("%s %s cannot be represented", origin, fexact(requested))
	}
	c.plan.FPS = snapped
	// A single still frame has no second timestamp for the fps filter to
	// work with: on a one-image input ffmpeg's fps filter emits zero frames
	// (verified with FFmpeg 7.1 and 9.0; a one-frame image2 sequence does
	// the same once setpts is in front). Single-frame sources therefore keep
	// the nominal rate for the rawvideo master (-r) but skip the filter.
	if c.singleFrame() {
		return nil
	}
	// round=down: the filter floors the input's end time onto the output
	// grid, so an fps drop never lengthens the clip past the (trimmed)
	// source. The default rounding (near, half away from zero) rounds the
	// end time up and appends a frame: an exactly-5.0 s clip at 16.7 fps
	// became 84 x 59.9 ms = 5.03 s, breaking the Discord sticker 5 s cap.
	// Verified against ffmpeg in TestFPSDropNeverLengthensClip
	// (gif_timing_ffmpeg_test.go) and enc's TestVariantFramesMatchFFmpeg
	// (the same floor model).
	c.emit("fps=" + fnum(snapped) + ":round=down")
	return nil
}

// ---------------------------------------------------------------------------
// Geometry stages, applied in the order given.
// ---------------------------------------------------------------------------

func (c *compiler) geometry(ops []decodedOp) error {
	for _, d := range ops {
		var err error
		switch p := d.params.(type) {
		case *recipe.CropParams:
			err = c.crop(d, p)
		case *recipe.ResizeParams:
			err = c.resize(d, p)
		case *recipe.CanvasParams:
			err = c.canvas(d, p)
		case *recipe.FlipParams:
			c.flip(p)
		case *recipe.RotateParams:
			err = c.rotate(d, p)
		default:
			// temporal ops and unpremultiply were handled already
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *compiler) crop(d decodedOp, p *recipe.CropParams) error {
	if p.W < 1 || p.H < 1 {
		return opErrorf(d, "size must be at least 1x1 (got %dx%d)", p.W, p.H)
	}
	if p.X < 0 || p.Y < 0 {
		return opErrorf(d, "offset must be >= 0 (got %d,%d)", p.X, p.Y)
	}
	if p.X+p.W > c.w || p.Y+p.H > c.h {
		return opErrorf(d, "rectangle %dx%d at (%d,%d) exceeds the %dx%d frame", p.W, p.H, p.X, p.Y, c.w, c.h)
	}
	c.emitCrop(p.W, p.H, p.X, p.Y)
	return nil
}

func (c *compiler) resize(d decodedOp, p *recipe.ResizeParams) error {
	fit, err := normalizeFit(p.Fit)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	if p.Width < 0 || p.Height < 0 {
		return opErrorf(d, "width and height must be >= 0 (got %dx%d)", p.Width, p.Height)
	}
	if p.Width == 0 && p.Height == 0 {
		return opErrorf(d, "width or height is required")
	}
	if p.Width > MaxDim || p.Height > MaxDim {
		return opErrorf(d, "width and height must be <= %d (got %dx%d)", MaxDim, p.Width, p.Height)
	}
	sw, sh := scaledSize(c.w, c.h, p.Width, p.Height, fit)
	if err := checkFrame(sw, sh); err != nil {
		return opErrorf(d, "%v", err)
	}
	c.emitScale(sw, sh)
	if fit == fitCover && p.Width > 0 && p.Height > 0 {
		c.emitCenterCrop(p.Width, p.Height)
	}
	return nil
}

func (c *compiler) canvas(d decodedOp, p *recipe.CanvasParams) error {
	if p.Width < 1 || p.Height < 1 {
		return opErrorf(d, "size must be at least 1x1 (got %dx%d)", p.Width, p.Height)
	}
	if p.Width > MaxDim || p.Height > MaxDim {
		return opErrorf(d, "width and height must be <= %d (got %dx%d)", MaxDim, p.Width, p.Height)
	}
	if err := checkFrame(p.Width, p.Height); err != nil {
		return opErrorf(d, "%v", err)
	}
	color, transparent, err := padColor(p.Color)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	// pad cannot shrink: crop any dimension that is larger than the canvas.
	c.emitCenterCrop(p.Width, p.Height)
	c.emitPad(p.Width, p.Height, color, transparent)
	return nil
}

func (c *compiler) flip(p *recipe.FlipParams) {
	if p.Horizontal {
		c.emit("hflip")
	}
	if p.Vertical {
		c.emit("vflip")
	}
}

func (c *compiler) rotate(d decodedOp, p *recipe.RotateParams) error {
	switch p.Degrees {
	case 90:
		c.emit("transpose=1") // clockwise
		c.w, c.h = c.h, c.w
	case 180:
		c.emit("hflip")
		c.emit("vflip")
	case 270:
		c.emit("transpose=2") // counter-clockwise
		c.w, c.h = c.h, c.w
	default:
		return opErrorf(d, "degrees must be 90, 180 or 270 (got %d)", p.Degrees)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Output fit and finish.
// ---------------------------------------------------------------------------

// validateOutput checks the Output knobs the graph does not otherwise
// consume but that every plan must agree on: FrameFormat (frames export) and
// FitBytes (fit-to-size budget). Format itself is not restricted here: every
// Format* constant compiles to the same RGBA master (static formats take its
// first frame), and the encoders decide what they can write.
func validateOutput(out recipe.Output) error {
	switch out.FrameFormat {
	case "", recipe.FormatPNG, recipe.FormatJPEG, recipe.FormatWebP:
	default:
		return errorf("output: frame format %q must be one of png, jpeg, webp", out.FrameFormat)
	}
	if out.FitBytes < 0 {
		return errorf("output: fitBytes must be >= 0 (got %d)", out.FitBytes)
	}
	return nil
}

// outputFit applies Output.Width/Height/Fit exactly like resize+canvas:
// contain scales to fit and pads transparent to WxH (both given), cover
// scales to cover and centre-crops, exact stretches. With only one of
// Width/Height the frame is scaled to it keeping aspect, without padding.
func (c *compiler) outputFit() error {
	fit, err := normalizeFit(c.out.Fit)
	if err != nil {
		return errorf("output: %v", err)
	}
	w, h := c.out.Width, c.out.Height
	if w < 0 || h < 0 {
		return errorf("output: width and height must be >= 0 (got %dx%d)", w, h)
	}
	if w > MaxDim || h > MaxDim {
		return errorf("output: width and height must be <= %d (got %dx%d)", MaxDim, w, h)
	}
	if w == 0 && h == 0 {
		return nil
	}
	sw, sh := scaledSize(c.w, c.h, w, h, fit)
	if err := checkFrame(sw, sh); err != nil {
		return errorf("output: %v", err)
	}
	c.emitScale(sw, sh)
	if w > 0 && h > 0 {
		switch fit {
		case fitCover:
			c.emitCenterCrop(w, h)
		case fitContain:
			c.emitPad(w, h, "0x00000000", true)
		}
	}
	return nil
}

// finish appends the terminal format=rgba, assembles the filter text,
// derives duration/frame count and applies the size limits to the final
// frame (which catches an oversized source that no op shrinks) and to the
// expected master.
func (c *compiler) finish() (*Plan, error) {
	if err := checkFrame(c.w, c.h); err != nil {
		return nil, errorf("output %v; add a resize", err)
	}
	c.emit("format=rgba")
	p := &c.plan
	p.Filter = c.input + strings.Join(c.stages, ",") + outLabel
	p.Width, p.Height = c.w, c.h
	p.HasAlpha = c.hasAlpha
	p.SourceFPS = c.sourceFPS()
	switch {
	case c.singleFrame():
		p.Duration, p.Frames = 0, 1
	default:
		dur := c.sourceDuration()
		if dur <= 0 {
			return p, nil // unknown
		}
		end := p.TrimEnd
		if end <= 0 {
			end = dur
		}
		p.Duration = math.Max(end-p.TrimStart, 0) / p.Speed
		// floor, matching the fps stage's round=down (the 1e-9 absorbs float
		// error when Duration*FPS is exact); >= 1 so a sub-frame clip still
		// plans a frame.
		p.Frames = max(1, int(math.Floor(p.Duration*p.FPS+1e-9)))
	}
	if bytes := float64(p.Width) * float64(p.Height) * 4 * float64(p.Frames); bytes > MaxMasterBytes {
		return nil, errorf("expected master (%dx%d x %d frames = %.1f GiB) exceeds the %d GiB limit; trim, lower the fps or resize",
			p.Width, p.Height, p.Frames, bytes/(1<<30), MaxMasterBytes>>30)
	}
	return p, nil
}

// checkFrame reports whether a w x h frame is within MaxDim per side and
// MaxPixels in area.
func checkFrame(w, h int) error {
	if w > MaxDim || h > MaxDim || w*h > MaxPixels {
		return fmt.Errorf("frame %dx%d exceeds the limits (%d px per side, %d megapixels)", w, h, MaxDim, MaxPixels>>20)
	}
	return nil
}

// sourceDuration returns the probed duration, falling back to
// frames/fps when only those are known; 0 = unknown. An image sequence
// lasts count/rate (the rate follows the effective delay, so the probed
// duration — computed at the default delay — does not apply).
func (c *compiler) sourceDuration() float64 {
	if c.seq != nil {
		if c.seq.count > 0 {
			return float64(c.seq.count) / c.seq.rate
		}
		return 0
	}
	if c.src.Duration > 0 {
		return c.src.Duration
	}
	if c.src.Frames > 0 && c.src.FPS > 0 {
		return float64(c.src.Frames) / c.src.FPS
	}
	return 0
}

// ---------------------------------------------------------------------------
// Stage emitters. Each keeps the tracked frame size in sync.
// ---------------------------------------------------------------------------

// emitCrop emits crop=w:h:x:y (a no-op crop is skipped). exact=1 stops the
// crop filter from rounding odd sizes/offsets down to the chroma
// subsampling of 4:2:0 sources, which would make the tracked size lie.
func (c *compiler) emitCrop(w, h, x, y int) {
	if w == c.w && h == c.h && x == 0 && y == 0 {
		return
	}
	c.emit(fmt.Sprintf("crop=%d:%d:%d:%d:exact=1", w, h, x, y))
	c.w, c.h = w, h
}

// emitCenterCrop crops the current frame to at most w x h, centred.
func (c *compiler) emitCenterCrop(w, h int) {
	w, h = min(w, c.w), min(h, c.h)
	c.emitCrop(w, h, (c.w-w)/2, (c.h-h)/2)
}

// emitScale emits a lanczos scale to w x h. When the frame carries alpha the
// scale is wrapped in premultiply/unpremultiply (on planar gbrap) so
// transparent edges do not bleed their (usually black) colour. A no-op
// scale is skipped.
//
// No alpha_mode tag is needed here: premultiply's required input mode
// (straight) is what an unspecified stream degrades to, and after a hoisted
// unpremultiply the frames are already tagged straight, so FFmpeg >= 8
// inserts no conversion (verified: no auto_*premultiply_dynamic in the
// -loglevel verbose graph dump; alpha_ffmpeg_test.go checks the pixels).
func (c *compiler) emitScale(w, h int) {
	if w == c.w && h == c.h {
		return
	}
	scale := fmt.Sprintf("scale=%d:%d:flags=lanczos", w, h)
	if c.hasAlpha {
		c.emit("format=gbrap")
		c.emit("premultiply=inplace=1")
		c.emit(scale)
		c.emit("unpremultiply=inplace=1")
	} else {
		c.emit(scale)
	}
	c.w, c.h = w, h
}

// emitPad pads the current frame to w x h, centred, with the given ffmpeg
// colour. The frame must not be larger than w x h (callers crop first). The
// stream is forced to rgba first: pad ignores the colour's alpha on
// alpha-less formats (transparent padding would come out black) and rgba has
// no chroma subsampling, so odd sizes/offsets stay exact.
func (c *compiler) emitPad(w, h int, color string, transparent bool) {
	if w == c.w && h == c.h {
		return
	}
	c.emit("format=rgba")
	c.emit(fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=%s", w, h, color))
	c.w, c.h = w, h
	if transparent {
		c.hasAlpha = true
	}
}

// ---------------------------------------------------------------------------
// Size / colour helpers.
// ---------------------------------------------------------------------------

// normalizeFit validates a fit mode; "" means contain.
func normalizeFit(fit string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(fit)) {
	case "", fitContain:
		return fitContain, nil
	case fitCover:
		return fitCover, nil
	case fitExact:
		return fitExact, nil
	}
	return "", fmt.Errorf("fit %q must be one of contain, cover, exact", fit)
}

// scaledSize returns the size to scale a curW x curH frame to for the given
// box and fit mode: with only one of w/h the aspect is kept; contain returns
// the largest aspect-preserving size inside the box, cover the smallest
// aspect-preserving size covering it (the caller centre-crops), exact the
// box itself. Results are >= 1. Integer arithmetic keeps the bound dimension
// exact.
func scaledSize(curW, curH, w, h int, fit string) (int, int) {
	byWidth := func() (int, int) { return w, max(1, roundDiv(curH*w, curW)) }
	byHeight := func() (int, int) { return max(1, roundDiv(curW*h, curH)), h }
	switch {
	case w > 0 && h > 0:
		switch fit {
		case fitExact:
			return w, h
		case fitCover:
			if w*curH >= h*curW { // width is the tighter cover constraint
				return byWidth()
			}
			return byHeight()
		default: // contain
			if w*curH <= h*curW { // width is the binding constraint
				return byWidth()
			}
			return byHeight()
		}
	case w > 0:
		return byWidth()
	default:
		return byHeight()
	}
}

// roundDiv returns a/b rounded to the nearest integer (half up) for a >= 0,
// b > 0.
func roundDiv(a, b int) int {
	return (a + b/2) / b
}

// padColor converts a CanvasParams colour ("" = fully transparent, else
// RRGGBB or RRGGBBAA) to ffmpeg's 0x notation and reports whether the
// padding is (partly) transparent.
func padColor(s string) (color string, transparent bool, err error) {
	if strings.TrimSpace(s) == "" {
		return "0x00000000", true, nil
	}
	hex, err := recipe.NormalizeHex(s)
	if err != nil {
		return "", false, err
	}
	transparent = len(hex) == 8 && hex[6:] != "ff"
	return "0x" + hex, transparent, nil
}
