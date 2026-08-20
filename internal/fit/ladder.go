package fit

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// master is what a ladder knows about the source it pre-filters: its fps
// and size after the op stack. Zero values mean "unknown" and make the
// corresponding rung dimension "as the master".
type master struct {
	fps  float64
	w, h int
}

// step is one row of a preset ladder table (DESIGN.md §5.4). Zero fps / px
// mean "as the master"; zero colours mean "format default".
type step struct {
	fps       float64
	colors    int
	px        int // longer-side target
	dither    string
	format    string
	truecolor bool // RGBA probe rung (sticker apng): single encode, no palette
}

// fpsEpsilon absorbs float noise when comparing a rung fps with the
// master's (16.7 vs 16.70000001).
const fpsEpsilon = 1e-6

var (
	// emoteGIFSteps: (25,256,128,bayer) → (20,128) → (16.7,128) → (12.5,64,112,none) → (10,32,96).
	emoteGIFSteps = []step{
		{fps: 25, colors: 256, px: 128, dither: "bayer", format: recipe.FormatGIF},
		{fps: 20, colors: 128, px: 128, dither: "bayer", format: recipe.FormatGIF},
		{fps: 16.7, colors: 128, px: 128, dither: "bayer", format: recipe.FormatGIF},
		{fps: 12.5, colors: 64, px: 112, dither: "none", format: recipe.FormatGIF},
		{fps: 10, colors: 32, px: 96, dither: "none", format: recipe.FormatGIF},
	}
	// emoteWebPSteps: the same fps rungs at 128/112/96 px, quality knob.
	emoteWebPSteps = []step{
		{fps: 25, px: 128, format: recipe.FormatWebP},
		{fps: 20, px: 128, format: recipe.FormatWebP},
		{fps: 16.7, px: 128, format: recipe.FormatWebP},
		{fps: 12.5, px: 112, format: recipe.FormatWebP},
		{fps: 10, px: 96, format: recipe.FormatWebP},
	}
	// stickerRGBASteps: the RGBA truecolour APNG probes of §5.4/§9a ("RGBA
	// APNG only when it fits at ≥ 12 fps"): 25 → 20 → 16.7 → 12.5 fps, each a
	// single encode (truecolorKnob) that is skipped when over the target.
	stickerRGBASteps = []step{
		{fps: 25, truecolor: true, format: recipe.FormatAPNG},
		{fps: 20, truecolor: true, format: recipe.FormatAPNG},
		{fps: 16.7, truecolor: true, format: recipe.FormatAPNG},
		{fps: 12.5, truecolor: true, format: recipe.FormatAPNG},
	}
	// stickerAPNGSteps: indexed 8-bit-alpha APNG (25,256) → (20,256) → (16.7,256) → (12.5,128) → (10,64).
	stickerAPNGSteps = []step{
		{fps: 25, colors: 256, format: recipe.FormatAPNG},
		{fps: 20, colors: 256, format: recipe.FormatAPNG},
		{fps: 16.7, colors: 256, format: recipe.FormatAPNG},
		{fps: 12.5, colors: 128, format: recipe.FormatAPNG},
		{fps: 10, colors: 64, format: recipe.FormatAPNG},
	}
	// stickerGIFSteps: the GIF fallback with the lossy knob, never downscaled.
	stickerGIFSteps = []step{
		{fps: 25, colors: 256, dither: "bayer", format: recipe.FormatGIF},
		{fps: 20, colors: 128, dither: "bayer", format: recipe.FormatGIF},
		{fps: 16.7, colors: 128, dither: "bayer", format: recipe.FormatGIF},
		{fps: 12.5, colors: 64, dither: "none", format: recipe.FormatGIF},
		{fps: 10, colors: 32, dither: "none", format: recipe.FormatGIF},
	}

	// Generic ladder tables (perceptual cost order of §5.4).
	genericFPS    = []float64{30, 24, 20, 15}
	genericColors = []int{128, 64}
	genericScales = []float64{0.75, 0.5}
)

// buildLadder turns a step table into rungs for m and drops duplicates.
func buildLadder(m master, steps []step, showFormat bool) []Rung {
	out := make([]Rung, 0, len(steps))
	for _, st := range steps {
		out = append(out, buildRung(m, st, showFormat))
	}
	return dedupeRungs(out)
}

// buildRung realises one step against the master: fps at or above the
// master's becomes "master fps", a size at or above the master's longer
// side becomes "master size", downscales are computed from the longer side.
// Truecolour steps get the single-probe knob; indexed APNG/PNG steps get a
// colour-step knob bounded by their own palette (floor 64, §5.4).
func buildRung(m master, st step, showFormat bool) Rung {
	r := Rung{Colors: st.colors, Dither: st.dither, Format: st.format, Truecolor: st.truecolor}
	switch {
	case st.truecolor:
		r.Colors = 0
		r.Knob = truecolorKnob
	case st.colors > 0 && (st.format == recipe.FormatAPNG || st.format == recipe.FormatPNG):
		r.Knob = colourStepKnob(st.colors)
	}
	if st.fps > 0 && m.fps > 0 && st.fps < m.fps-fpsEpsilon {
		r.FPS = st.fps
	}
	if st.px > 0 && m.w > 0 && m.h > 0 && st.px < max(m.w, m.h) {
		r.Width, r.Height = scaleToLongSide(m.w, m.h, st.px)
	}
	r.Label = labelFor(r, m, showFormat)
	return r
}

// scaleToLongSide returns the size whose longer side is px and whose aspect
// matches w:h (the other side rounded, at least 1).
func scaleToLongSide(w, h, px int) (int, int) {
	if w >= h {
		return px, max(1, int(math.Round(float64(px)*float64(h)/float64(w))))
	}
	return max(1, int(math.Round(float64(px)*float64(w)/float64(h)))), px
}

// genericLadder implements Generic.
func genericLadder(format string, m master, keepSize, keepFPS bool) []Rung {
	palette := isPaletteFormat(format)
	if recipe.IsStaticFormat(format) {
		m.fps = 0 // a still has no frame rate to drop or label
	}

	var out []Rung
	out = append(out, buildRung(m, step{format: format}, false))

	var lowFPS float64 // the lowest fps rung reached; 0 = master
	if m.fps > 0 && !keepFPS {
		for _, f := range genericFPS {
			if f < m.fps-fpsEpsilon {
				out = append(out, buildRung(m, step{fps: f, format: format}, false))
				lowFPS = f
			}
		}
	}
	colors := 0
	if palette {
		for _, c := range genericColors {
			out = append(out, buildRung(m, step{fps: lowFPS, colors: c, format: format}, false))
			colors = c
		}
	}
	if !keepSize && m.w > 0 && m.h > 0 {
		long := max(m.w, m.h)
		for _, f := range genericScales {
			px := int(math.Round(float64(long) * f))
			if px >= 1 && px < long {
				out = append(out, buildRung(m, step{fps: lowFPS, colors: colors, px: px, format: format}, false))
			}
		}
	}
	return dedupeRungs(out)
}

// Filter applies the recipe's FitKeepSize / FitKeepFPS to a preset ladder:
// with keepSize every rung keeps the master size (Width/Height 0), with
// keepFPS every rung keeps the master fps (FPS 0); the colour/dither
// progression of the ladder survives and rungs that collapse onto an earlier
// one are dropped. Changed rungs get their label re-rendered with
// masterFPS/w/h (pass the values the ladder was built with; 0 = unknown),
// untouched rungs keep theirs. The input slice is not modified.
func Filter(rungs []Rung, keepSize, keepFPS bool, masterFPS float64, w, h int) []Rung {
	m := master{masterFPS, w, h}
	out := make([]Rung, 0, len(rungs))
	for _, r := range rungs {
		changed := false
		if keepSize && (r.Width != 0 || r.Height != 0) {
			r.Width, r.Height = 0, 0
			changed = true
		}
		if keepFPS && r.FPS != 0 {
			r.FPS = 0
			changed = true
		}
		if changed {
			showFormat := r.Format != "" && strings.HasPrefix(r.Label, strings.ToUpper(r.Format)+" · ")
			r.Label = labelFor(r, m, showFormat)
		}
		out = append(out, r)
	}
	return dedupeRungs(out)
}

// isPaletteFormat reports whether format has a palette to shrink.
func isPaletteFormat(format string) bool {
	switch format {
	case recipe.FormatGIF, recipe.FormatAPNG, recipe.FormatPNG:
		return true
	}
	return false
}

// rungKey identifies a rung by everything except its label.
type rungKey struct {
	fps           float64
	width, height int
	colors        int
	dither        string
	format        string
	truecolor     bool
	knob          Knob // dereferenced Rung.Knob override; zero when hasKnob is false
	hasKnob       bool
}

// dedupeRungs drops rungs identical (label aside) to an earlier one: fps and
// size clamping collapses neighbouring rungs for low-fps or small masters.
func dedupeRungs(rungs []Rung) []Rung {
	seen := make(map[rungKey]bool, len(rungs))
	out := make([]Rung, 0, len(rungs))
	for _, r := range rungs {
		k := rungKey{fps: r.FPS, width: r.Width, height: r.Height, colors: r.Colors, dither: r.Dither, format: r.Format, truecolor: r.Truecolor}
		if r.Knob != nil {
			k.knob, k.hasKnob = *r.Knob, true
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

// labelFor renders "25 fps · 256 colours · 128 px" from the rung's effective
// values (master values fill the zero fields when known). showFormat
// prefixes the upper-cased format for ladders that mix formats; truecolour
// rungs carry "RGBA" ("APNG · RGBA · 25 fps · 320 px"). Dither is
// deliberately left out (the template of §5.4 has none; Candidate.Rung
// carries it).
func labelFor(r Rung, m master, showFormat bool) string {
	var parts []string
	if showFormat && r.Format != "" {
		parts = append(parts, strings.ToUpper(r.Format))
	}
	if r.Truecolor {
		parts = append(parts, "RGBA")
	}
	fps := r.FPS
	if fps == 0 {
		fps = m.fps
	}
	if fps > 0 {
		parts = append(parts, fpsString(fps)+" fps")
	}
	if r.Colors > 0 {
		parts = append(parts, fmt.Sprintf("%d colours", r.Colors))
	}
	w, h := r.Width, r.Height
	if w == 0 && h == 0 {
		w, h = m.w, m.h
	}
	if s := sizeString(w, h); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "master settings"
	}
	return strings.Join(parts, " · ")
}

// labelOf is the label used in Result/Desc: the rung's own, or one derived
// from its fields for caller-built rungs without a Label.
func labelOf(r Rung) string {
	if r.Label != "" {
		return r.Label
	}
	return labelFor(r, master{}, true)
}

// fpsString prints an fps with up to two decimals and no trailing zeros.
func fpsString(f float64) string {
	return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64)
}

// sizeString prints "128 px" for square (or width-only) sizes and "128×64"
// otherwise; "" when unknown.
func sizeString(w, h int) string {
	switch {
	case w <= 0 && h <= 0:
		return ""
	case h <= 0 || w == h:
		return fmt.Sprintf("%d px", w)
	case w <= 0:
		return fmt.Sprintf("%d px", h)
	default:
		return fmt.Sprintf("%d×%d", w, h)
	}
}
