// Package graph compiles a recipe's op stack into a single ffmpeg
// filter_complex graph (the "P0 universal decode" graph in docs/DESIGN.md
// §4.1). It is pure: no ffmpeg is executed here, only argv/filter text is
// produced, which makes it golden-file testable.
//
// Contract:
//   - The main input is ffmpeg input 0 ("[0:v]"). Plan.InputArgs are placed
//     immediately before "-i <main>" (e.g. -ss/-to for trim, -c:v libvpx-vp9
//     for VP9-alpha WebM, -ignore_loop 0 for gif inputs is NOT needed for the
//     main input).
//   - Plan.Filter is a complete filter_complex string ending in Plan.OutLabel
//     ("[out]") whose frames are format=rgba, at Plan.Width x Plan.Height,
//     Plan.FPS frames per second (constant frame rate).
//   - Stage order in the filter text: unpremultiply (hoisted, at native
//     depth, preceded by setparams=alpha_mode=premultiplied so FFmpeg >= 8's
//     alpha_mode negotiation does not auto-insert a cancelling premultiply)
//     → speed (setpts) → fps (always present, so the output is CFR) →
//     the geometry ops in the order given (crop, premultiplied lanczos
//     scale, canvas pad, flip/rotate; each sees the frame size produced by
//     the previous one) → output fit (Output.Width/Height/Fit) →
//     format=rgba. Trim never becomes a filter: it is expressed as -ss/-to
//     input seek args (source time). Temporal and spatial filters commute,
//     so hoisting setpts/fps in front of the geometry ops does not change
//     the result but keeps the frame count low before scaling.
//   - Output.FPS (or the fps op) is capped with SnapFPS(out.Format, fps): 50
//     for GIF, 60 otherwise; no other snapping (see SnapFPS for why 30 fps
//     GIFs need none).
//   - Sizes are bounded: resize/canvas/Output dimensions and every resulting
//     frame must be <= 8192 px per side and <= 32 megapixels, the speed factor
//     must lie in [0.05, 100] and the expected RGBA master (W*H*4*Frames, when
//     the frame count is known) must fit in 8 GiB; Compile rejects anything
//     larger with a descriptive error.
package graph

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Plan is the compiled graph plus the facts the encoders need.
type Plan struct {
	InputArgs []string // args placed before "-i <main input>"
	Filter    string   // filter_complex text producing OutLabel
	OutLabel  string   // always "[out]"

	Width, Height int     // output frame size
	FPS           float64 // output frame rate (constant); > 0
	HasAlpha      bool    // whether the output carries (non-trivial) alpha
	Duration      float64 // expected output duration in seconds (0 = unknown)
	Frames        int     // expected frame count (0 = unknown)

	// Facts the still/proxy renderers need to map a preview time to source
	// time: TrimStart/TrimEnd are the source-time bounds selected by the trim
	// op (0,0 = whole source; TrimEnd 0 = to the end); Speed is the speed
	// factor (1 = unchanged). A preview at output time t corresponds to
	// source time TrimStart + t*Speed.
	TrimStart float64
	TrimEnd   float64
	Speed     float64

	// SourceFPS is the probed frame rate of the source (recipe.ProbeInfo.FPS
	// as given, not snapped; 0 = unknown, e.g. stills). The still renderer
	// uses it to decide how far before the wanted frame it must seek so that
	// at least one decodable source frame precedes the target.
	SourceFPS float64
}

// ErrNotImplemented is kept for API compatibility with the Phase-1 stubs.
// Nothing in this package returns it any more.
var ErrNotImplemented = errors.New("graph: not implemented")

// outLabel is the output pad label of every compiled graph.
const outLabel = "[out]"

// Compile builds the Plan for src + ops + out. It validates op params
// (unknown kinds, out-of-range crops, non-positive sizes) and the upper
// bounds (MaxDim, MaxPixels, MinSpeed/MaxSpeed, MaxMasterBytes) and returns
// descriptive errors suitable for showing in the UI.
func Compile(src recipe.ProbeInfo, ops []recipe.Op, out recipe.Output) (*Plan, error) {
	if src.Width <= 0 || src.Height <= 0 {
		return nil, errorf("source has no usable frame size (%dx%d)", src.Width, src.Height)
	}
	decoded, err := decodeOps(ops)
	if err != nil {
		return nil, err
	}
	c := newCompiler(src, out)
	if err := c.temporal(decoded); err != nil {
		return nil, err
	}
	if err := c.geometry(decoded); err != nil {
		return nil, err
	}
	if err := c.outputFit(); err != nil {
		return nil, err
	}
	return c.finish()
}

// Frame-rate caps applied by SnapFPS.
const (
	// MaxGIFFPS is the GIF cap. GIF delays are whole centiseconds and
	// browsers clamp delays <= 1 cs to 10 cs, so every delay must be >= 2 cs
	// (DESIGN.md §5.3). ffmpeg's gif muxer runs at a 1/100 s timebase and
	// rounds each frame's pts to the nearest centisecond, so consecutive
	// delays are floor(100/fps) or ceil(100/fps): at <= 50 fps every delay is
	// >= 2 cs (this cap is what protects the >= 2 cs rule), while 60 fps would
	// yield 2,1,2 cs delays. Nothing else needs snapping: a 30 fps master
	// gets 3,4,3 cs delays with an exact total (Bresenham for free) and no
	// frame is dropped or duplicated — an earlier draft snapped to 100/n rates
	// (30 → 33.333), which duplicated 1 in 9 frames. Verified against ffmpeg
	// in gif_timing_ffmpeg_test.go.
	MaxGIFFPS = 50
	// MaxFPS is the cap for every other animated format (Discord stickers
	// allow at most 60 fps; nothing above it survives a browser anyway).
	MaxFPS = 60
)

// SnapFPS returns the frame rate actually used for format: a plain cap at
// MaxGIFFPS (50) for GIF and MaxFPS (60) for everything else, rounded to 3
// decimals so filter text built from it is stable ("fps=29.97"). fps <= 0
// (or NaN) returns 0 (the caller substitutes the source fps first). See
// MaxGIFFPS for why 30 fps GIFs need no further snapping.
func SnapFPS(format string, fps float64) float64 {
	if !(fps > 0) { // also catches NaN
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(format), "gif") {
		return round3(math.Min(fps, MaxGIFFPS))
	}
	return round3(math.Min(fps, MaxFPS))
}

// round3 rounds v to 3 decimals (the precision used everywhere in filter
// text and seek args).
func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// fnum renders v for filter/argv text: rounded to 3 decimals, minimal
// digits, no exponent, no trailing zeros, no "-0" (1.5 → "1.5", 2 → "2",
// 33.3333 → "33.333").
func fnum(v float64) string {
	v = round3(v)
	if v == 0 {
		v = 0 // normalise -0
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// fexact renders v with full precision (used for values that must not be
// rounded, like the speed factor, so Plan.Speed and the setpts expression
// agree exactly). Never uses scientific notation.
func fexact(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
