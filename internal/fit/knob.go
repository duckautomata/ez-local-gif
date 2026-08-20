package fit

import (
	"fmt"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Knob names returned by KnobFor. Candidate.Desc renders them for humans:
// KnobLossy as "lossy N", KnobQuality as "quality Q" with Q = 100 - knob
// (the knob is the documented 100-quality inversion), KnobColourStep as
// "colour step N", anything else as "<name> N".
const (
	KnobLossy      = "lossy"       // gif: gifsicle --lossy N
	KnobQuality    = "quality"     // webp/avif/jpeg: encoder quality = 100 - knob
	KnobColourStep = "colour step" // apng/png: palette halvings below the rung's colours, floored at 64 (256 → 128 → 64)
	KnobLevel      = "level"       // unknown formats: abstract 0..100 harshness
)

// knobFor implements KnobFor.
func knobFor(format string) Knob {
	switch format {
	case recipe.FormatGIF:
		return Knob{Min: 0, Max: 200, Mild: 30, Harsh: 150, Name: KnobLossy}
	case recipe.FormatWebP, recipe.FormatAVIF:
		return Knob{Min: 5, Max: 90, Mild: 20, Harsh: 70, Name: KnobQuality}
	case recipe.FormatAPNG, recipe.FormatPNG:
		// DESIGN.md §5.4 floors the colour search at 64: at most two
		// halvings below the (default 256-colour) palette.
		return Knob{Min: 0, Max: 2, Mild: 0, Harsh: 2, Name: KnobColourStep}
	case recipe.FormatJPEG:
		return Knob{Min: 10, Max: 80, Mild: 20, Harsh: 60, Name: KnobQuality}
	default:
		return Knob{Min: 0, Max: 100, Mild: 20, Harsh: 70, Name: KnobLevel}
	}
}

// colourStepKnob bounds the colour-step knob of an indexed APNG/PNG rung by
// its own palette: DESIGN.md §5.4 floors the fit search at 64 colours, so
// the harshest step is log2(colors/64) halvings — 256 → steps 0..2, 128 →
// 0..1, 64 → the single point 0 (probed once and otherwise skipped, see
// searchRung). The mild probe is always the rung's own palette; the harsh
// probe at most two halvings below it.
func colourStepKnob(colors int) *Knob {
	steps := 0
	for c := colors; c > 64; c /= 2 {
		steps++
	}
	return &Knob{Min: 0, Max: steps, Mild: 0, Harsh: min(2, steps), Name: KnobColourStep}
}

// truecolorKnob is the single-point knob of an RGBA truecolour probe rung:
// there is nothing to search (mild == harsh == 0), so the rung costs exactly
// one encode and is skipped when it does not fit.
var truecolorKnob = &Knob{Name: "probe"}

// normalizeKnob applies the documented defaults (Mild 0 → Min, Harsh 0 →
// Max), clamps the probes into [Min, Max] and validates the shape.
func normalizeKnob(k Knob) (Knob, error) {
	if k.Min > k.Max {
		return Knob{}, fmt.Errorf("fit: knob %q: min %d > max %d", k.Name, k.Min, k.Max)
	}
	if k.Mild == 0 {
		k.Mild = k.Min
	}
	if k.Harsh == 0 {
		k.Harsh = k.Max
	}
	k.Mild = min(max(k.Mild, k.Min), k.Max)
	k.Harsh = min(max(k.Harsh, k.Min), k.Max)
	if k.Harsh < k.Mild {
		return Knob{}, fmt.Errorf("fit: knob %q: harsh probe %d is milder than mild probe %d", k.Name, k.Harsh, k.Mild)
	}
	if k.Name == "" {
		k.Name = "knob"
	}
	return k, nil
}

// describeKnob renders the binding knob for Candidate.Desc.
func describeKnob(k Knob, v int) string {
	switch k.Name {
	case KnobQuality:
		return fmt.Sprintf("quality %d", 100-v)
	case "":
		return fmt.Sprintf("knob %d", v)
	default:
		return fmt.Sprintf("%s %d", k.Name, v)
	}
}
