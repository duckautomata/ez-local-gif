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

// Fit modes shared by the resize op and Output.Fit.
const (
	fitContain = "contain"
	fitCover   = "cover"
	fitExact   = "exact"
)

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

	w, h     int  // current frame size
	hasAlpha bool // current frame carries alpha (source alpha or transparent padding)

	stages []string // filter stages, joined with ","
	plan   Plan
}

func newCompiler(src recipe.ProbeInfo, out recipe.Output) *compiler {
	return &compiler{
		src:      src,
		out:      out,
		w:        src.Width,
		h:        src.Height,
		hasAlpha: src.HasAlpha,
		plan:     Plan{OutLabel: outLabel, Speed: 1},
	}
}

func (c *compiler) emit(stage string) {
	c.stages = append(c.stages, stage)
}

// ---------------------------------------------------------------------------
// Temporal stages: unpremultiply (hoisted), trim (input args), speed, fps.
// ---------------------------------------------------------------------------

func (c *compiler) temporal(ops []decodedOp) error {
	c.unpremultiply(ops)
	c.plan.InputArgs = append(c.plan.InputArgs, decoderArgs(c.src)...)
	if err := c.trim(ops); err != nil {
		return err
	}
	if err := c.speed(ops); err != nil {
		return err
	}
	return c.fps(ops)
}

// unpremultiply hoists the unpremultiply op to run right after decode, at
// the source's native bit depth so 10/12-bit ProRes alpha edges are not
// truncated to 8 bits before the division. It is a no-op for sources
// without alpha.
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
	if !c.src.HasAlpha {
		return
	}
	for _, d := range ops {
		if d.kind != recipe.OpUnpremultiply {
			continue
		}
		format := "gbrap"
		switch c.src.Bits {
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
// filter so the master is constant-frame-rate.
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
	case c.src.FPS > 0 && !math.IsInf(c.src.FPS, 0):
		requested, origin = c.src.FPS, "source fps"
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
	// (verified with FFmpeg 7.1 and 9.0). Stills therefore keep the nominal
	// rate for the rawvideo master (-r) but skip the filter itself.
	if c.src.IsStill {
		return nil
	}
	c.emit("fps=" + fnum(snapped))
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
	p.Filter = "[0:v]" + strings.Join(c.stages, ",") + outLabel
	p.Width, p.Height = c.w, c.h
	p.HasAlpha = c.hasAlpha
	if c.src.FPS > 0 && !math.IsInf(c.src.FPS, 0) {
		p.SourceFPS = c.src.FPS
	}
	switch {
	case c.src.IsStill:
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
		p.Frames = max(1, int(math.Round(p.Duration*p.FPS)))
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
// frames/fps when only those are known; 0 = unknown.
func (c *compiler) sourceDuration() float64 {
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
