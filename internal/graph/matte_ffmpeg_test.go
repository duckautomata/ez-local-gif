package graph_test

// Real-ffmpeg pixel checks of the Phase 5b matte merge (compiler.matte /
// mergeMatte): the matte sequence — gray PNGs read by image2 at the plan's
// rate — becomes the alpha of an opaque main (scaled to the frame), is
// multiplied into the alpha of an rgba main, travels with its frame through
// a reverse and a bounce behind the merge, and keeps its input index in
// front of an overlay registered later. External test package so
// internal/graph stays process-free; skips when ffmpeg is not on PATH. The
// argv is built here from the plan (p3Render), independent of enc.

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

const (
	mtFrames = 4  // frames of the main clip (10 fps)
	mtSize   = 16 // the main frame is mtSize x mtSize
	mtMatte  = 8  // the matte PNGs are mtMatte x mtMatte: the merge scales them up
)

// mtColor is the opaque colour of main frame i (distinct per frame).
func mtColor(i int) color.NRGBA {
	return color.NRGBA{R: uint8(40 * (i + 1)), G: 200, B: 30, A: 255}
}

// mtLeft / mtRight are the matte values of frame i: a flat left half and a
// flat right half (a step the bicubic upscale leaves exact away from the
// seam — the four outer columns on either side, see mtAlphaCols).
func mtLeft(i int) uint8  { return uint8(15 + 60*i) }
func mtRight(i int) uint8 { return 255 - mtLeft(i) }

// mtAlphaCols are the output columns checked against the left and right
// matte values: at least two source pixels from the step on either side,
// outside the bicubic support.
var mtAlphaCols = struct{ left, right []int }{[]int{0, 1, 2, 3}, []int{12, 13, 14, 15}}

// matteSequence writes the mtFrames gray matte PNGs (%06d.png, 1-based) into
// dir and returns the image2 pattern.
func matteSequence(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < mtFrames; i++ {
		img := image.NewGray(image.Rect(0, 0, mtMatte, mtMatte))
		for y := 0; y < mtMatte; y++ {
			for x := 0; x < mtMatte; x++ {
				v := mtLeft(i)
				if x >= mtMatte/2 {
					v = mtRight(i)
				}
				img.SetGray(x, y, color.Gray{Y: v})
			}
		}
		writePNG(t, filepath.Join(dir, fmt.Sprintf("%06d.png", i+1)), img)
	}
	return filepath.Join(dir, "%06d.png")
}

// mainFrames are the opaque main frames (frame i in mtColor(i)); with alpha
// set the left half is opaque and the right half half-transparent.
func mainFrames(alpha bool) []image.Image {
	frames := make([]image.Image, mtFrames)
	for i := range frames {
		img := solid(mtSize, mtSize, mtColor(i))
		if alpha {
			for y := 0; y < mtSize; y++ {
				for x := mtSize / 2; x < mtSize; x++ {
					c := mtColor(i)
					c.A = 128
					img.SetNRGBA(x, y, c)
				}
			}
		}
		frames[i] = img
	}
	return frames
}

// checkMatteInput checks the compiler registered exactly one matte input
// at position 0 with the contract's args for a 10 fps plan.
func checkMatteInput(t *testing.T, p *graph.Plan, extras int) {
	t.Helper()
	if len(p.ExtraInputs) != extras || p.ExtraInputs[0].Matte == nil || p.ExtraInputs[0].Source != 0 {
		t.Fatalf("ExtraInputs %+v, want %d with a matte input first", p.ExtraInputs, extras)
	}
	if want := []string{"-f", "image2", "-framerate", "10", "-start_number", "1"}; !reflect.DeepEqual(p.ExtraInputs[0].Args, want) || p.ExtraInputs[0].Matte.FPS != "10" {
		t.Fatalf("matte input args %q / fps %q, want %q / \"10\"", p.ExtraInputs[0].Args, p.ExtraInputs[0].Matte.FPS, want)
	}
}

// checkMatted checks rendered frame i of an opaque main: the colour is the
// source's and the alpha is the matte's left / right value (scaled).
func checkMatted(t *testing.T, f []byte, i int, name string) {
	t.Helper()
	c := mtColor(i)
	for y := 0; y < mtSize; y += 5 {
		for _, x := range append(append([]int{}, mtAlphaCols.left...), mtAlphaCols.right...) {
			px := pixel(f, mtSize, x, y)
			want := mtLeft(i)
			if x >= mtSize/2 {
				want = mtRight(i)
			}
			if !near(px[0], c.R, 0) || !near(px[1], c.G, 0) || !near(px[2], c.B, 0) {
				t.Errorf("%s: frame %d (%d,%d) colour %v, want %v (the colour must be untouched)", name, i, x, y, px, c)
			}
			if !near(px[3], want, 1) {
				t.Errorf("%s: frame %d (%d,%d) alpha %d, want the scaled matte %d", name, i, x, y, px[3], want)
			}
		}
	}
}

func TestMattePixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	pattern := matteSequence(t, filepath.Join(dir, "mattes"))
	out := recipe.Output{Format: "webp"}
	m := recipe.Op{Kind: recipe.OpMatte}

	t.Run("opaque main: the matte becomes the alpha, scaled to the frame", func(t *testing.T) {
		clip, info := pngClip(t, ff, dir, "opaque", mainFrames(false), 10, false)
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{m}, out)
		checkMatteInput(t, p, 1)
		if !p.HasAlpha || p.Frames != mtFrames {
			t.Fatalf("plan: HasAlpha %v Frames %d", p.HasAlpha, p.Frames)
		}
		frames := p3Render(t, ff, clip, p, []string{pattern}, nil)
		if len(frames) != mtFrames {
			t.Fatalf("rendered %d frames, want %d (no framesync repeat or drop)", len(frames), mtFrames)
		}
		for i, f := range frames {
			checkMatted(t, f, i, "opaque")
		}
	})

	t.Run("rgba main: the matte is multiplied into the source alpha", func(t *testing.T) {
		clip, info := pngClip(t, ff, dir, "alpha", mainFrames(true), 10, true)
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{m}, out)
		checkMatteInput(t, p, 1)
		frames := p3Render(t, ff, clip, p, []string{pattern}, nil)
		if len(frames) != mtFrames {
			t.Fatalf("rendered %d frames, want %d", len(frames), mtFrames)
		}
		for i, f := range frames {
			c := mtColor(i)
			for y := 0; y < mtSize; y += 5 {
				for _, x := range mtAlphaCols.left {
					// Opaque source pixels: the alpha is the matte itself.
					if px := pixel(f, mtSize, x, y); !near(px[3], mtLeft(i), 1) || !near(px[0], c.R, 1) {
						t.Errorf("frame %d (%d,%d) %v, want alpha %d (255 x matte) and colour %v", i, x, y, px, mtLeft(i), c)
					}
				}
				for _, x := range mtAlphaCols.right {
					// Half-transparent source pixels: 128 x matte / 255.
					want := uint8((128*int(mtRight(i)) + 127) / 255)
					if px := pixel(f, mtSize, x, y); !near(px[3], want, 1) || !near(px[0], c.R, 1) {
						t.Errorf("frame %d (%d,%d) %v, want alpha %d (128 x matte %d / 255) and colour %v", i, x, y, px, want, mtRight(i), c)
					}
				}
			}
		}
	})

	t.Run("reverse and bounce behind the merge carry the mattes with their frames", func(t *testing.T) {
		clip, info := pngClip(t, ff, dir, "order", mainFrames(false), 10, false)
		forward := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{m}, out)
		fw := p3Render(t, ff, clip, forward, []string{pattern}, nil)
		if len(fw) != mtFrames {
			t.Fatalf("forward rendered %d frames, want %d", len(fw), mtFrames)
		}
		reversed := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{m, {Kind: recipe.OpReverse}}, out)
		checkMatteInput(t, reversed, 1)
		if !reversed.Reversed {
			t.Fatalf("plan is not Reversed: %s", reversed.Filter)
		}
		rv := p3Render(t, ff, clip, reversed, []string{pattern}, nil)
		if len(rv) != mtFrames {
			t.Fatalf("reversed rendered %d frames, want %d", len(rv), mtFrames)
		}
		for j, f := range rv {
			// The j-th reversed frame is forward frame n-1-j, colour AND
			// alpha: the matte of frame n-1-j, not matte j.
			if i := mtFrames - 1 - j; !reflect.DeepEqual(f, fw[i]) {
				t.Errorf("reversed frame %d differs from forward frame %d: %v vs %v", j, i, pixel(f, mtSize, 1, 1), pixel(fw[i], mtSize, 1, 1))
			}
			checkMatted(t, f, mtFrames-1-j, "reversed")
		}
		bounced := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{m, {Kind: recipe.OpBounce}}, out)
		checkMatteInput(t, bounced, 1)
		if bounced.Bounces != 1 || !bounced.Bounced || bounced.Frames != 2*mtFrames {
			t.Fatalf("bounced plan: Bounces %d Frames %d", bounced.Bounces, bounced.Frames)
		}
		bn := p3Render(t, ff, clip, bounced, []string{pattern}, nil)
		if len(bn) != 2*mtFrames {
			t.Fatalf("bounced rendered %d frames, want %d (manifest frames x 2^Bounces)", len(bn), 2*mtFrames)
		}
		for j, f := range bn {
			i := j
			if j >= mtFrames {
				i = 2*mtFrames - 1 - j
			}
			if !reflect.DeepEqual(f, fw[i]) {
				t.Errorf("bounced frame %d differs from forward frame %d", j, i)
			}
		}
	})

	t.Run("matte before an overlay: input 1 is the matte, input 2 the overlay", func(t *testing.T) {
		clip, info := pngClip(t, ff, dir, "ov", mainFrames(false), 10, false)
		ovPath := filepath.Join(dir, "ov.png")
		writePNG(t, ovPath, solid(4, 4, color.NRGBA{R: 255, A: 255}))
		ov := stillInfo(4, 4, true)
		p := compileSrcs(t, []recipe.ProbeInfo{info, ov}, []recipe.Op{m, {Kind: recipe.OpOverlay, Params: []byte(`{"source":1}`)}}, out)
		checkMatteInput(t, p, 2)
		if p.ExtraInputs[1].Source != 1 || p.ExtraInputs[1].Matte != nil {
			t.Fatalf("second input must be the overlay: %+v", p.ExtraInputs)
		}
		frames := p3Render(t, ff, clip, p, []string{pattern, ovPath}, nil)
		if len(frames) != mtFrames {
			t.Fatalf("rendered %d frames, want %d", len(frames), mtFrames)
		}
		for i, f := range frames {
			// Under the overlay: opaque red whatever the matte (the overlay
			// is composited after the merge, over the matted frame).
			if px := pixel(f, mtSize, 1, 1); !isRGBA(px, 255, 0, 0, 255, 1) {
				t.Errorf("frame %d: overlay pixel %v, want opaque red", i, px)
			}
			// Beside it: the matted main (right half, away from the seam).
			c := mtColor(i)
			if px := pixel(f, mtSize, 14, 8); !near(px[3], mtRight(i), 1) || !near(px[0], c.R, 0) {
				t.Errorf("frame %d: main pixel %v, want colour %v with alpha %d", i, px, c, mtRight(i))
			}
		}
	})
}
