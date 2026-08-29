package graph_test

// Real-ffmpeg pixel checks of the feather op (Phase 3 review item 4): the
// compiled "format=gbrap,gblur=sigma=R:planes=8,format=rgba" stage must turn
// a hard alpha edge into a monotonic gradient while leaving the colour
// planes untouched, both on a source that carries alpha and through the full
// green-screen chain (chromakey, then feather on the key's matte). External
// test package like the other *_ffmpeg_test.go files; skips when ffmpeg is
// not on PATH. Runs on the host build and in the ezlg-dev image.
//
// The keyed check keeps clear of the frame borders: ffmpeg's chromakey
// mis-keys pure-screen pixels ON the frame border itself (column x=0 on odd
// rows and the bottom row came out alpha 189/255 on an all-green border,
// measured on the 2026-08 git build AND the ezlg-dev 9.0.1 — a filter border
// quirk, not a graph bug), and the feather then spreads that ~10 px inward.
// The asserted window sits >= 16 px from every border.

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// featherEdgeFrame is a w x 32 frame split by a hard vertical edge at x =
// edge: left on the left, c on the right, spanning the full height — so the
// vertical blur pass is a no-op and every row shows the same 1-D profile.
func featherEdgeFrame(w, edge int, c, left color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < w; x++ {
			if x >= edge {
				img.SetNRGBA(x, y, c)
			} else {
				img.SetNRGBA(x, y, left)
			}
		}
	}
	return img
}

// checkFeatheredEdge asserts the 1-D alpha profile of row y after a feather
// of sigma 3 across a former hard edge between columns edge-1 and edge,
// inside the window [lo, hi]: monotonically non-decreasing left to right
// (tol absorbs gblur's IIR rounding), ~0 at lo, ~255 at hi, intermediate
// values strictly between 16 and 240 within 3 px on BOTH sides of the edge,
// and the edge itself near the 50% point (midTol widens for the keyed
// chain, whose key softens the hard edge by a pixel before the blur).
func checkFeatheredEdge(t *testing.T, f []byte, w, y, edge, lo, hi, tol, midTol int, name string) {
	t.Helper()
	alpha := func(x int) int { return int(pixel(f, w, x, y)[3]) }
	for x := lo; x < hi; x++ {
		if alpha(x+1) < alpha(x)-tol {
			t.Errorf("%s: alpha not monotonic at x=%d,y=%d: %d then %d", name, x, y, alpha(x), alpha(x+1))
		}
	}
	if a := alpha(lo); a > 8 {
		t.Errorf("%s: alpha %d at x=%d, want ~0 far from the edge", name, a, lo)
	}
	if a := alpha(hi); a < 248 {
		t.Errorf("%s: alpha %d at x=%d, want ~255 far from the edge", name, a, hi)
	}
	for _, side := range []struct {
		lo, hi int
		what   string
	}{
		{edge - 3, edge, "transparent side"},
		{edge, edge + 3, "opaque side"},
	} {
		ok := false
		for x := side.lo; x < side.hi; x++ {
			if a := alpha(x); a > 16 && a < 240 {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s: no intermediate alpha (16..240) within 3 px on the %s of the edge at x=%d,y=%d", name, side.what, edge, y)
		}
	}
	if mid := (alpha(edge-1) + alpha(edge)) / 2; !near(byte(mid), 128, midTol) {
		t.Errorf("%s: alpha %d at the former edge (columns %d/%d), want 128±%d", name, mid, edge-1, edge, midTol)
	}
}

func TestFeatherPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	out := recipe.Output{Format: "webp"}
	red := color.NRGBA{R: 255, A: 255}

	t.Run("hard alpha edge becomes a monotonic gradient and RGB is untouched", func(t *testing.T) {
		const w, edge = 32, 12
		frame := featherEdgeFrame(w, edge, red, color.NRGBA{})
		clip, info := pngClip(t, ff, dir, "alphaedge", []image.Image{frame, frame}, 10, true)
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{{Kind: recipe.OpFeather, Params: []byte(`{"radius":3}`)}}, out)
		if !p.HasAlpha || !strings.Contains(p.Filter, "format=gbrap,gblur=sigma=3:planes=8,format=rgba") {
			t.Fatalf("plan: alpha %v filter %s", p.HasAlpha, p.Filter)
		}
		frames := p3Render(t, ff, clip, p, nil, nil)
		if len(frames) != 2 {
			t.Fatalf("%d frames, want 2", len(frames))
		}
		for i, f := range frames {
			checkFeatheredEdge(t, f, w, 16, edge, 1, w-2, 2, 16, "feather")
			// planes=8 blurs only the alpha plane: the colour bytes of every
			// formerly opaque pixel are exactly the source's red, whatever
			// their blurred alpha is now.
			for x := edge; x < w; x++ {
				if px := pixel(f, w, x, 16); px[0] != 255 || px[1] != 0 || px[2] != 0 {
					t.Errorf("frame %d: RGB (%d,%d,%d) at x=%d, want (255,0,0) untouched", i, px[0], px[1], px[2], x)
					break
				}
			}
		}
	})

	t.Run("a keyed green-screen edge softens the same way through the full chain", func(t *testing.T) {
		const w, edge = 64, 32
		green := color.NRGBA{G: 255, A: 255}
		frame := featherEdgeFrame(w, edge, red, green)
		clip, info := pngClip(t, ff, dir, "greenedge", []image.Image{frame, frame}, 10, false)
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{
			{Kind: recipe.OpChromaKey},
			{Kind: recipe.OpFeather, Params: []byte(`{"radius":3}`)},
		}, out)
		// The key on the opaque source is bare and the feather follows it.
		if !strings.Contains(p.Filter, "chromakey=color=0x00ff00") ||
			!strings.Contains(p.Filter, "despill=type=green:mix=0.6:expand=0.3,format=gbrap,gblur=sigma=3:planes=8,format=rgba") {
			t.Fatalf("filter: %s", p.Filter)
		}
		frames := p3Render(t, ff, clip, p, nil, nil)
		if len(frames) != 2 {
			t.Fatalf("%d frames, want 2", len(frames))
		}
		for i, f := range frames {
			// The chromakey leaves the last screen pixel before the subject
			// partially opaque (its 3x3 neighbourhood sees the subject), so
			// the 50% point shifts a little towards the screen; the shape is
			// the same monotonic gradient. Window [16, 48]: >= 16 px from
			// the frame borders, clear of chromakey's border quirk (above).
			checkFeatheredEdge(t, f, w, 16, edge, edge-16, edge+16, 4, 40, "keyed feather")
			// Deep inside the subject: still red (despill leaves pure red
			// alone; the yuva444p round trip costs a little).
			if px := pixel(f, w, w-2, 16); !near(px[0], 255, 4) || !near(px[1], 0, 4) || !near(px[2], 0, 4) {
				t.Errorf("frame %d: subject %v, want red", i, px)
			}
		}
	})
}
