package fit

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// rungLine renders a rung compactly for diffs: "fps/WxH/colours/dither/format|label"
// (truecolour rungs are prefixed "RGBA ").
func rungLine(r Rung) string {
	s := fmt.Sprintf("%g/%dx%d/%d/%s/%s|%s", r.FPS, r.Width, r.Height, r.Colors, r.Dither, r.Format, r.Label)
	if r.Truecolor {
		return "RGBA " + s
	}
	return s
}

func rungLines(rs []Rung) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = rungLine(r)
	}
	return out
}

func assertLadder(t *testing.T, name string, got []Rung, want []string) {
	t.Helper()
	g := rungLines(got)
	if !reflect.DeepEqual(g, want) {
		t.Errorf("%s:\n got: %s\nwant: %s", name, strings.Join(g, "\n      "), strings.Join(want, "\n      "))
	}
}

func TestEmoteGIF(t *testing.T) {
	assertLadder(t, "30 fps 128²", EmoteGIF(30, 128, 128), []string{
		"25/0x0/256/bayer/gif|25 fps · 256 colours · 128 px",
		"20/0x0/128/bayer/gif|20 fps · 128 colours · 128 px",
		"16.7/0x0/128/bayer/gif|16.7 fps · 128 colours · 128 px",
		"12.5/112x112/64/none/gif|12.5 fps · 64 colours · 112 px",
		"10/96x96/32/none/gif|10 fps · 32 colours · 96 px",
	})
	// 15 fps master: the 25/20/16.7 rungs clamp to the master fps and the
	// duplicate collapses; colour/scale steps survive.
	assertLadder(t, "15 fps", EmoteGIF(15, 128, 128), []string{
		"0/0x0/256/bayer/gif|15 fps · 256 colours · 128 px",
		"0/0x0/128/bayer/gif|15 fps · 128 colours · 128 px",
		"12.5/112x112/64/none/gif|12.5 fps · 64 colours · 112 px",
		"10/96x96/32/none/gif|10 fps · 32 colours · 96 px",
	})
	// Exactly 25 fps is "master fps", 24 fps is a real drop.
	if r := EmoteGIF(25, 128, 128)[0]; r.FPS != 0 || r.Label != "25 fps · 256 colours · 128 px" {
		t.Errorf("25 fps master: %s", rungLine(r))
	}
	if r := EmoteGIF(24, 128, 128)[0]; r.FPS != 0 || r.Label != "24 fps · 256 colours · 128 px" {
		t.Errorf("24 fps master: %s", rungLine(r))
	}
	if r := EmoteGIF(29.97, 128, 128)[0]; r.FPS != 25 {
		t.Errorf("29.97 fps master: %s", rungLine(r))
	}
	// Small master: never upscale; 128 and 112 both mean "master size".
	assertLadder(t, "100 px master", EmoteGIF(30, 100, 100), []string{
		"25/0x0/256/bayer/gif|25 fps · 256 colours · 100 px",
		"20/0x0/128/bayer/gif|20 fps · 128 colours · 100 px",
		"16.7/0x0/128/bayer/gif|16.7 fps · 128 colours · 100 px",
		"12.5/0x0/64/none/gif|12.5 fps · 64 colours · 100 px",
		"10/96x96/32/none/gif|10 fps · 32 colours · 96 px",
	})
	// Non-square masters scale the longer side and keep aspect.
	wide := EmoteGIF(30, 128, 64)
	if got := rungLine(wide[3]); got != "12.5/112x56/64/none/gif|12.5 fps · 64 colours · 112×56" {
		t.Errorf("wide: %s", got)
	}
	tall := EmoteGIF(30, 64, 128)
	if got := rungLine(tall[4]); got != "10/48x96/32/none/gif|10 fps · 32 colours · 48×96" {
		t.Errorf("tall: %s", got)
	}
	if wide[0].Label != "25 fps · 256 colours · 128×64" {
		t.Errorf("wide master label: %q", wide[0].Label)
	}
	// Unknown master: everything is "as the master", duplicates collapse.
	assertLadder(t, "unknown master", EmoteGIF(0, 0, 0), []string{
		"0/0x0/256/bayer/gif|256 colours",
		"0/0x0/128/bayer/gif|128 colours",
		"0/0x0/64/none/gif|64 colours",
		"0/0x0/32/none/gif|32 colours",
	})
}

func TestEmoteWebP(t *testing.T) {
	assertLadder(t, "30 fps 128²", EmoteWebP(30, 128, 128), []string{
		"25/0x0/0//webp|25 fps · 128 px",
		"20/0x0/0//webp|20 fps · 128 px",
		"16.7/0x0/0//webp|16.7 fps · 128 px",
		"12.5/112x112/0//webp|12.5 fps · 112 px",
		"10/96x96/0//webp|10 fps · 96 px",
	})
	assertLadder(t, "12 fps", EmoteWebP(12, 128, 128), []string{
		"0/0x0/0//webp|12 fps · 128 px",
		"0/112x112/0//webp|12 fps · 112 px",
		"10/96x96/0//webp|10 fps · 96 px",
	})
	for _, r := range EmoteWebP(60, 128, 128) {
		if r.Colors != 0 || r.Dither != "" {
			t.Errorf("webp rung carries palette settings: %s", rungLine(r))
		}
	}
}

func TestStickerAPNGThenGIF(t *testing.T) {
	full := StickerAPNGThenGIF(30, 320, 320)
	assertLadder(t, "30 fps 320²", full, []string{
		"RGBA 25/0x0/0//apng|APNG · RGBA · 25 fps · 320 px",
		"RGBA 20/0x0/0//apng|APNG · RGBA · 20 fps · 320 px",
		"RGBA 16.7/0x0/0//apng|APNG · RGBA · 16.7 fps · 320 px",
		"RGBA 12.5/0x0/0//apng|APNG · RGBA · 12.5 fps · 320 px",
		"25/0x0/256//apng|APNG · 25 fps · 256 colours · 320 px",
		"20/0x0/256//apng|APNG · 20 fps · 256 colours · 320 px",
		"16.7/0x0/256//apng|APNG · 16.7 fps · 256 colours · 320 px",
		"12.5/0x0/128//apng|APNG · 12.5 fps · 128 colours · 320 px",
		"10/0x0/64//apng|APNG · 10 fps · 64 colours · 320 px",
		"25/0x0/256/bayer/gif|GIF · 25 fps · 256 colours · 320 px",
		"20/0x0/128/bayer/gif|GIF · 20 fps · 128 colours · 320 px",
		"16.7/0x0/128/bayer/gif|GIF · 16.7 fps · 128 colours · 320 px",
		"12.5/0x0/64/none/gif|GIF · 12.5 fps · 64 colours · 320 px",
		"10/0x0/32/none/gif|GIF · 10 fps · 32 colours · 320 px",
	})
	// Per-rung knobs: RGBA rungs probe once; indexed APNG rungs are bounded
	// by their own palette (floor 64, §5.4); GIF rungs use the request knobs.
	for _, r := range full {
		switch {
		case r.Truecolor:
			if r.Knob == nil || r.Knob.Min != r.Knob.Max || r.Colors != 0 {
				t.Errorf("RGBA rung without a single-point knob: %s (%+v)", rungLine(r), r.Knob)
			}
		case r.Format == "apng":
			want := colourStepKnob(r.Colors)
			if r.Knob == nil || *r.Knob != *want {
				t.Errorf("indexed rung %s knob = %+v, want %+v", rungLine(r), r.Knob, want)
			}
		default:
			if r.Knob != nil {
				t.Errorf("gif rung %s carries a knob override: %+v", rungLine(r), *r.Knob)
			}
		}
	}
	// Stickers are never downscaled, whatever the master size.
	for _, r := range StickerAPNGThenGIF(30, 640, 480) {
		if r.Width != 0 || r.Height != 0 {
			t.Errorf("sticker rung downscales: %s", rungLine(r))
		}
	}
	// Low-fps master collapses the fps rungs in each half independently (the
	// RGBA probes collapse to a single one at the master fps).
	got := StickerAPNGThenGIF(12, 320, 320)
	want := []string{
		"RGBA 0/0x0/0//apng|APNG · RGBA · 12 fps · 320 px",
		"0/0x0/256//apng|APNG · 12 fps · 256 colours · 320 px",
		"0/0x0/128//apng|APNG · 12 fps · 128 colours · 320 px",
		"10/0x0/64//apng|APNG · 10 fps · 64 colours · 320 px",
		"0/0x0/256/bayer/gif|GIF · 12 fps · 256 colours · 320 px",
		"0/0x0/128/bayer/gif|GIF · 12 fps · 128 colours · 320 px",
		"0/0x0/64/none/gif|GIF · 12 fps · 64 colours · 320 px",
		"10/0x0/32/none/gif|GIF · 10 fps · 32 colours · 320 px",
	}
	assertLadder(t, "12 fps", got, want)
}

// TestAPNGLadderKnobFloor walks every APNG/PNG rung of the preset ladders
// through its effective knob range and asserts the §5.4 floor: no probe may
// imply a palette below 64 colours (jobs.apngColors halves the rung's
// colours once per knob step, replicated here).
func TestAPNGLadderKnobFloor(t *testing.T) {
	ladders := map[string][]Rung{
		"StickerAPNGThenGIF": StickerAPNGThenGIF(30, 320, 320),
		"Generic apng":       Generic("apng", 25, 320, 320, false, false),
		"Generic png":        Generic("png", 0, 500, 500, false, false),
	}
	for name, rungs := range ladders {
		for _, r := range rungs {
			if r.Truecolor || (r.Format != "apng" && r.Format != "png") || r.Colors == 0 {
				continue
			}
			k := KnobFor(r.Format)
			if r.Knob != nil {
				k = *r.Knob
			}
			n, err := normalizeKnob(k)
			if err != nil {
				t.Fatalf("%s %s: bad knob: %v", name, rungLine(r), err)
			}
			for v := n.Min; v <= n.Max; v++ {
				if got := r.Colors >> v; got < 64 {
					t.Errorf("%s %s: knob %d implies %d colours (< 64)", name, rungLine(r), v, got)
				}
			}
		}
	}
	// The explicit-palette rungs must all carry the bounded per-rung knob.
	for _, r := range Generic("apng", 25, 320, 320, false, false) {
		if r.Colors > 0 && r.Knob == nil {
			t.Errorf("generic apng rung %s has no per-rung knob", rungLine(r))
		}
	}
}

func TestGeneric(t *testing.T) {
	assertLadder(t, "gif 30 fps 640×360", Generic("gif", 30, 640, 360, false, false), []string{
		"0/0x0/0//gif|30 fps · 640×360",
		"24/0x0/0//gif|24 fps · 640×360",
		"20/0x0/0//gif|20 fps · 640×360",
		"15/0x0/0//gif|15 fps · 640×360",
		"15/0x0/128//gif|15 fps · 128 colours · 640×360",
		"15/0x0/64//gif|15 fps · 64 colours · 640×360",
		"15/480x270/64//gif|15 fps · 64 colours · 480×270",
		"15/320x180/64//gif|15 fps · 64 colours · 320×180",
	})
	assertLadder(t, "webp keep both", Generic("webp", 30, 640, 360, true, true), []string{
		"0/0x0/0//webp|30 fps · 640×360",
	})
	assertLadder(t, "webp keep fps", Generic("webp", 30, 640, 360, false, true), []string{
		"0/0x0/0//webp|30 fps · 640×360",
		"0/480x270/0//webp|30 fps · 480×270",
		"0/320x180/0//webp|30 fps · 320×180",
	})
	assertLadder(t, "webp keep size", Generic("webp", 30, 640, 360, true, false), []string{
		"0/0x0/0//webp|30 fps · 640×360",
		"24/0x0/0//webp|24 fps · 640×360",
		"20/0x0/0//webp|20 fps · 640×360",
		"15/0x0/0//webp|15 fps · 640×360",
	})
	// 60 fps master gets the 30 fps rung too; 20 fps master only 15.
	if got := rungLines(Generic("avif", 60, 100, 100, true, false)); len(got) != 5 || got[1] != "30/0x0/0//avif|30 fps · 100 px" {
		t.Errorf("60 fps: %v", got)
	}
	assertLadder(t, "gif 20 fps unknown size", Generic("gif", 20, 0, 0, false, false), []string{
		"0/0x0/0//gif|20 fps",
		"15/0x0/0//gif|15 fps",
		"15/0x0/128//gif|15 fps · 128 colours",
		"15/0x0/64//gif|15 fps · 64 colours",
	})
	// Static formats: no fps rungs; png keeps its palette rungs.
	assertLadder(t, "png", Generic("png", 30, 500, 500, false, false), []string{
		"0/0x0/0//png|500 px",
		"0/0x0/128//png|128 colours · 500 px",
		"0/0x0/64//png|64 colours · 500 px",
		"0/375x375/64//png|64 colours · 375 px",
		"0/250x250/64//png|64 colours · 250 px",
	})
	assertLadder(t, "jpeg", Generic("jpeg", 30, 100, 100, false, false), []string{
		"0/0x0/0//jpeg|100 px",
		"0/75x75/0//jpeg|75 px",
		"0/50x50/0//jpeg|50 px",
	})
	// Nothing known at all → a single knob-only rung.
	assertLadder(t, "nothing known", Generic("webp", 0, 0, 0, false, false), []string{
		"0/0x0/0//webp|master settings",
	})
	// apng is a palette format too.
	if got := Generic("apng", 25, 320, 320, true, true); len(got) != 3 || got[1].Colors != 128 || got[2].Colors != 64 {
		t.Errorf("apng colours: %v", rungLines(got))
	}
}

func TestLaddersNeverExceedMaster(t *testing.T) {
	type gen struct {
		name string
		fn   func(fps float64, w, h int) []Rung
	}
	gens := []gen{
		{"EmoteGIF", EmoteGIF},
		{"EmoteWebP", EmoteWebP},
		{"StickerAPNGThenGIF", StickerAPNGThenGIF},
		{"Generic gif", func(f float64, w, h int) []Rung { return Generic("gif", f, w, h, false, false) }},
		{"Generic webp", func(f float64, w, h int) []Rung { return Generic("webp", f, w, h, false, false) }},
	}
	for _, g := range gens {
		for _, fps := range []float64{0, 5, 10, 12.5, 16.7, 20, 24, 25, 29.97, 30, 50, 60} {
			for _, sz := range [][2]int{{0, 0}, {1, 1}, {50, 50}, {96, 96}, {100, 40}, {128, 128}, {128, 64}, {64, 128}, {320, 320}, {1920, 1080}} {
				rungs := g.fn(fps, sz[0], sz[1])
				if len(rungs) == 0 {
					t.Errorf("%s(%g,%v): empty ladder", g.name, fps, sz)
				}
				seen := map[string]bool{}
				for _, r := range rungs {
					if r.FPS < 0 || (fps > 0 && r.FPS > fps) || (fps == 0 && r.FPS != 0) {
						t.Errorf("%s(%g,%v): rung fps %g above master", g.name, fps, sz, r.FPS)
					}
					if r.Width > sz[0] || r.Height > sz[1] || r.Width < 0 || r.Height < 0 {
						t.Errorf("%s(%g,%v): rung size %dx%d above master", g.name, fps, sz, r.Width, r.Height)
					}
					if (r.Width == 0) != (r.Height == 0) {
						t.Errorf("%s(%g,%v): half-set size %dx%d", g.name, fps, sz, r.Width, r.Height)
					}
					if r.Label == "" || r.Format == "" {
						t.Errorf("%s(%g,%v): rung without label/format: %+v", g.name, fps, sz, r)
					}
					key := rungLine(Rung{FPS: r.FPS, Width: r.Width, Height: r.Height, Colors: r.Colors, Dither: r.Dither, Format: r.Format, Truecolor: r.Truecolor})
					if seen[key] {
						t.Errorf("%s(%g,%v): duplicate rung %s", g.name, fps, sz, key)
					}
					seen[key] = true
				}
			}
		}
	}
}

func TestFilter(t *testing.T) {
	base := EmoteGIF(30, 128, 128)
	// keepSize: scale rungs collapse to master size but the colour ladder survives.
	assertLadder(t, "keepSize", Filter(base, true, false, 30, 128, 128), []string{
		"25/0x0/256/bayer/gif|25 fps · 256 colours · 128 px",
		"20/0x0/128/bayer/gif|20 fps · 128 colours · 128 px",
		"16.7/0x0/128/bayer/gif|16.7 fps · 128 colours · 128 px",
		"12.5/0x0/64/none/gif|12.5 fps · 64 colours · 128 px",
		"10/0x0/32/none/gif|10 fps · 32 colours · 128 px",
	})
	// keepFPS: fps rungs collapse to master fps; duplicates drop.
	assertLadder(t, "keepFPS", Filter(base, false, true, 30, 128, 128), []string{
		"0/0x0/256/bayer/gif|30 fps · 256 colours · 128 px",
		"0/0x0/128/bayer/gif|30 fps · 128 colours · 128 px",
		"0/112x112/64/none/gif|30 fps · 64 colours · 112 px",
		"0/96x96/32/none/gif|30 fps · 32 colours · 96 px",
	})
	assertLadder(t, "keep both", Filter(base, true, true, 30, 128, 128), []string{
		"0/0x0/256/bayer/gif|30 fps · 256 colours · 128 px",
		"0/0x0/128/bayer/gif|30 fps · 128 colours · 128 px",
		"0/0x0/64/none/gif|30 fps · 64 colours · 128 px",
		"0/0x0/32/none/gif|30 fps · 32 colours · 128 px",
	})
	// No filter → a copy; the input is never modified; custom labels survive
	// when nothing changes; format prefixes are preserved on re-render.
	same := Filter(base, false, false, 30, 128, 128)
	if !reflect.DeepEqual(same, base) {
		t.Errorf("no-op filter changed the ladder")
	}
	if len(base) > 0 {
		same[0].Label = "mutated"
		if base[0].Label == "mutated" {
			t.Errorf("Filter returned a slice aliasing its input")
		}
	}
	custom := []Rung{{FPS: 10, Width: 50, Height: 50, Label: "my rung"}, {Colors: 16, Label: "my other rung"}}
	got := Filter(custom, false, true, 20, 100, 100)
	if got[0].Label != "20 fps · 50 px" || got[1].Label != "my other rung" {
		t.Errorf("custom labels: %q %q", got[0].Label, got[1].Label)
	}
	st := Filter(StickerAPNGThenGIF(30, 320, 320), false, true, 30, 320, 320)
	if st[0].Label != "APNG · RGBA · 30 fps · 320 px" || st[0].FPS != 0 || !st[0].Truecolor {
		t.Errorf("sticker keepFPS: %s", rungLine(st[0]))
	}
	if st[1].Label != "APNG · 30 fps · 256 colours · 320 px" || st[1].FPS != 0 {
		t.Errorf("sticker keepFPS: %s", rungLine(st[1]))
	}
	if Filter(EmoteGIF(30, 128, 128), true, true, 30, 128, 128)[0].FPS != 0 {
		t.Errorf("keepFPS left an fps rung")
	}
}

func TestLabelHelpers(t *testing.T) {
	cases := []struct {
		r    Rung
		m    master
		show bool
		want string
	}{
		{Rung{}, master{}, false, "master settings"},
		{Rung{}, master{30, 128, 128}, false, "30 fps · 128 px"},
		{Rung{FPS: 16.7, Colors: 128}, master{30, 128, 128}, false, "16.7 fps · 128 colours · 128 px"},
		{Rung{FPS: 23.976023976}, master{}, false, "23.98 fps"},
		{Rung{Width: 100}, master{}, false, "100 px"},
		{Rung{Height: 100}, master{}, false, "100 px"},
		{Rung{Width: 120, Height: 80}, master{}, false, "120×80"},
		{Rung{Format: "apng", Colors: 256}, master{25, 320, 320}, true, "APNG · 25 fps · 256 colours · 320 px"},
		{Rung{Format: "apng", Colors: 256}, master{25, 320, 320}, false, "25 fps · 256 colours · 320 px"},
		{Rung{Format: "apng", Truecolor: true}, master{25, 320, 320}, true, "APNG · RGBA · 25 fps · 320 px"},
		{Rung{Format: "apng", Truecolor: true, FPS: 12.5}, master{25, 320, 320}, false, "RGBA · 12.5 fps · 320 px"},
	}
	for _, c := range cases {
		if got := labelFor(c.r, c.m, c.show); got != c.want {
			t.Errorf("labelFor(%+v, %+v, %v) = %q, want %q", c.r, c.m, c.show, got, c.want)
		}
	}
	if got := labelOf(Rung{Label: "custom"}); got != "custom" {
		t.Errorf("labelOf keeps the label: %q", got)
	}
	if got := labelOf(Rung{FPS: 10, Format: "gif"}); got != "GIF · 10 fps" {
		t.Errorf("labelOf fallback: %q", got)
	}
}

func TestScaleToLongSide(t *testing.T) {
	cases := []struct{ w, h, px, ww, wh int }{
		{128, 128, 112, 112, 112},
		{128, 64, 112, 112, 56},
		{64, 128, 112, 56, 112},
		{1920, 1080, 480, 480, 270},
		{3, 1000, 10, 1, 10},
	}
	for _, c := range cases {
		w, h := scaleToLongSide(c.w, c.h, c.px)
		if w != c.ww || h != c.wh {
			t.Errorf("scaleToLongSide(%d,%d,%d) = %dx%d, want %dx%d", c.w, c.h, c.px, w, h, c.ww, c.wh)
		}
	}
}
