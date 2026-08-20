package fit

import "testing"

func TestKnobFor(t *testing.T) {
	cases := []struct {
		format string
		want   Knob
	}{
		{"gif", Knob{Min: 0, Max: 200, Mild: 30, Harsh: 150, Name: KnobLossy}},
		{"webp", Knob{Min: 5, Max: 90, Mild: 20, Harsh: 70, Name: KnobQuality}},
		{"avif", Knob{Min: 5, Max: 90, Mild: 20, Harsh: 70, Name: KnobQuality}},
		{"apng", Knob{Min: 0, Max: 2, Mild: 0, Harsh: 2, Name: KnobColourStep}},
		{"png", Knob{Min: 0, Max: 2, Mild: 0, Harsh: 2, Name: KnobColourStep}},
		{"jpeg", Knob{Min: 10, Max: 80, Mild: 20, Harsh: 60, Name: KnobQuality}},
		{"frames", Knob{Min: 0, Max: 100, Mild: 20, Harsh: 70, Name: KnobLevel}},
		{"", Knob{Min: 0, Max: 100, Mild: 20, Harsh: 70, Name: KnobLevel}},
	}
	for _, c := range cases {
		if got := KnobFor(c.format); got != c.want {
			t.Errorf("KnobFor(%q) = %+v, want %+v", c.format, got, c.want)
		}
		// Every built-in knob must survive normalisation unchanged (apart
		// from the documented Mild 0 → Min, which is Min anyway).
		n, err := normalizeKnob(c.want)
		if err != nil {
			t.Errorf("KnobFor(%q) does not normalise: %v", c.format, err)
		}
		if n.Mild != c.want.Mild || n.Harsh != c.want.Harsh || n.Mild >= n.Harsh {
			t.Errorf("KnobFor(%q) normalised to %+v", c.format, n)
		}
	}
}

// TestColourStepKnob pins the per-rung colour-step bound: halvings below
// the rung's palette, floored at 64 (DESIGN.md §5.4 "APNG: colours 256→64"),
// with the harsh probe at most two halvings.
func TestColourStepKnob(t *testing.T) {
	cases := []struct {
		colors int
		want   Knob
	}{
		{256, Knob{Min: 0, Max: 2, Mild: 0, Harsh: 2, Name: KnobColourStep}},
		{128, Knob{Min: 0, Max: 1, Mild: 0, Harsh: 1, Name: KnobColourStep}},
		{64, Knob{Min: 0, Max: 0, Mild: 0, Harsh: 0, Name: KnobColourStep}},
		// Below the floor there is nothing to halve: a single-point probe.
		{32, Knob{Min: 0, Max: 0, Mild: 0, Harsh: 0, Name: KnobColourStep}},
		{1024, Knob{Min: 0, Max: 4, Mild: 0, Harsh: 2, Name: KnobColourStep}},
	}
	for _, c := range cases {
		k := colourStepKnob(c.colors)
		if *k != c.want {
			t.Errorf("colourStepKnob(%d) = %+v, want %+v", c.colors, *k, c.want)
		}
		// Every override must survive normalisation (Search validates them).
		n, err := normalizeKnob(*k)
		if err != nil {
			t.Errorf("colourStepKnob(%d) does not normalise: %v", c.colors, err)
		}
		// No knob in [Min, Max] may take the palette below 64 (unless the
		// rung itself is already below the floor).
		for v := n.Min; v <= n.Max; v++ {
			if got := c.colors >> v; got < 64 && got < c.colors {
				t.Errorf("colourStepKnob(%d): step %d reaches %d colours (< 64)", c.colors, v, got)
			}
		}
	}
	// The truecolour probe knob is a single point too.
	if n, err := normalizeKnob(*truecolorKnob); err != nil || n.Mild != n.Harsh {
		t.Errorf("truecolorKnob normalised to %+v, %v", n, err)
	}
}

func TestNormalizeKnob(t *testing.T) {
	ok := []struct {
		in, want Knob
	}{
		{Knob{Min: 10, Max: 90}, Knob{Min: 10, Max: 90, Mild: 10, Harsh: 90, Name: "knob"}},
		{Knob{Min: 0, Max: 200, Mild: 30, Harsh: 150, Name: "lossy"}, Knob{Min: 0, Max: 200, Mild: 30, Harsh: 150, Name: "lossy"}},
		{Knob{Min: 10, Max: 90, Mild: 1, Harsh: 500}, Knob{Min: 10, Max: 90, Mild: 10, Harsh: 90, Name: "knob"}},
		{Knob{Min: 7, Max: 7}, Knob{Min: 7, Max: 7, Mild: 7, Harsh: 7, Name: "knob"}},
		{Knob{Min: 0, Max: 3, Harsh: 2}, Knob{Min: 0, Max: 3, Mild: 0, Harsh: 2, Name: "knob"}},
	}
	for _, c := range ok {
		got, err := normalizeKnob(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalizeKnob(%+v) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	bad := []Knob{
		{Min: 5, Max: 1},
		{Min: 0, Max: 100, Mild: 60, Harsh: 40},
	}
	for _, k := range bad {
		if _, err := normalizeKnob(k); err == nil {
			t.Errorf("normalizeKnob(%+v) accepted", k)
		}
	}
}

func TestDescribeKnob(t *testing.T) {
	cases := []struct {
		k    Knob
		v    int
		want string
	}{
		{KnobFor("gif"), 60, "lossy 60"},
		{KnobFor("webp"), 20, "quality 80"},
		{KnobFor("jpeg"), 60, "quality 40"},
		{KnobFor("apng"), 2, "colour step 2"},
		{KnobFor("mp4"), 50, "level 50"},
		{Knob{}, 3, "knob 3"},
		{Knob{Name: "crf"}, 28, "crf 28"},
	}
	for _, c := range cases {
		if got := describeKnob(c.k, c.v); got != c.want {
			t.Errorf("describeKnob(%q, %d) = %q, want %q", c.k.Name, c.v, got, c.want)
		}
	}
}
