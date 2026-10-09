package graph_test

// Real-ffmpeg pixel checks of the Phase 5c keep-colour union
// (compiler.keepColour): a keep colour's pixels come back opaque inside a
// region the matte removed, a colour within the similarity does too, a
// colour outside it and every other pixel stay byte-identical to the
// render without the keep, the colour planes never change — through both
// merge shapes (opaque main, rgba main) and with two colours. Built on the
// matte_ffmpeg_test.go fixtures (the same matte sequence and main frames,
// plus four colour patches). External test package so internal/graph stays
// process-free; skips when ffmpeg is not on PATH.

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"path/filepath"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// The keep probe: the mtSize x mtSize main frames of matte_ffmpeg_test.go
// (frame i in mtColor(i); the matte is mtLeft(i) on the left half and
// mtRight(i) on the right — never 255) with four kpPatch x kpPatch patches
// in the columns the bicubic upscale leaves exact (mtAlphaCols):
//
//	keep  at (0,0):  kpKeep, the first keep colour
//	near  at (0,6):  20 off kpKeep in R — RGB distance 0.045 on colorkey's scale: inside the 0.08 default, outside 0.02
//	far   at (0,12): 70 off in R — 0.158, outside the default
//	keep2 at (12,0): kpKeep2, the second keep colour, in the right half (half-transparent on the rgba main)
var (
	kpKeep  = color.NRGBA{R: 10, G: 20, B: 200, A: 255}
	kpNear  = color.NRGBA{R: 30, G: 20, B: 200, A: 255}
	kpFar   = color.NRGBA{R: 80, G: 20, B: 200, A: 255}
	kpKeep2 = color.NRGBA{R: 200, G: 10, B: 10, A: 255}
)

const kpPatch = 4

var kpPatches = []struct {
	name string
	x, y int
	c    color.NRGBA
}{
	{"keep", 0, 0, kpKeep},
	{"near", 0, 6, kpNear},
	{"far", 0, 12, kpFar},
	{"keep2", 12, 0, kpKeep2},
}

// kpPatchAt names the patch covering (x, y), "" outside every patch.
func kpPatchAt(x, y int) string {
	for _, p := range kpPatches {
		if x >= p.x && x < p.x+kpPatch && y >= p.y && y < p.y+kpPatch {
			return p.name
		}
	}
	return ""
}

// keepFrames are mainFrames(alpha) with the four patches painted on every
// frame; on the rgba main a patch in the right half is half-transparent
// like its surroundings, so the union is seen forcing a half-alpha source
// pixel opaque.
func keepFrames(alpha bool) []image.Image {
	frames := mainFrames(alpha)
	for _, f := range frames {
		img := f.(*image.NRGBA)
		for _, p := range kpPatches {
			c := p.c
			if alpha && p.x >= mtSize/2 {
				c.A = 128
			}
			for y := p.y; y < p.y+kpPatch; y++ {
				for x := p.x; x < p.x+kpPatch; x++ {
					img.SetNRGBA(x, y, c)
				}
			}
		}
	}
	return frames
}

func hexOf(c color.NRGBA) string { return fmt.Sprintf("%02x%02x%02x", c.R, c.G, c.B) }

func TestMatteKeepPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	pattern := matteSequence(t, filepath.Join(dir, "mattes"))
	out := recipe.Output{Format: "webp"}

	render := func(t *testing.T, clip string, info recipe.ProbeInfo, params recipe.MatteParams) [][]byte {
		t.Helper()
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{{Kind: recipe.OpMatte, Params: raw}}, out)
		checkMatteInput(t, p, 1)
		frames := p3Render(t, ff, clip, p, []string{pattern}, nil)
		if len(frames) != mtFrames {
			t.Fatalf("rendered %d frames, want %d", len(frames), mtFrames)
		}
		return frames
	}
	// check compares a keep render with the plain matte render of the same
	// clip: the colour planes are untouched everywhere, the patches in kept
	// are opaque, every other pixel's alpha is byte-identical.
	check := func(t *testing.T, got, base [][]byte, kept ...string) {
		t.Helper()
		isKept := map[string]bool{}
		for _, k := range kept {
			isKept[k] = true
		}
		for i := range got {
			for y := 0; y < mtSize; y++ {
				for x := 0; x < mtSize; x++ {
					g, b := pixel(got[i], mtSize, x, y), pixel(base[i], mtSize, x, y)
					if g[0] != b[0] || g[1] != b[1] || g[2] != b[2] {
						t.Errorf("frame %d (%d,%d): colour %v, want %v (the keep must not touch the colour planes)", i, x, y, g[:3], b[:3])
					}
					switch patch := kpPatchAt(x, y); {
					case isKept[patch]:
						if b[3] == 255 {
							t.Fatalf("frame %d (%d,%d): the plain render is already opaque in patch %s — the probe proves nothing", i, x, y, patch)
						}
						if g[3] != 255 {
							t.Errorf("frame %d (%d,%d): alpha %d in patch %s, want 255 (kept colour forced opaque over matte %d)", i, x, y, g[3], patch, b[3])
						}
					case g[3] != b[3]:
						t.Errorf("frame %d (%d,%d): alpha %d, want %d (unchanged outside the kept colours; patch %q)", i, x, y, g[3], b[3], patch)
					}
				}
			}
		}
	}

	t.Run("opaque main", func(t *testing.T) {
		clip, info := pngClip(t, ff, dir, "keep-opaque", keepFrames(false), 10, false)
		base := render(t, clip, info, recipe.MatteParams{})
		// The plain merge: the patches carry the matte like their neighbours.
		for i, f := range base {
			if px := pixel(f, mtSize, 1, 1); !near(px[3], mtLeft(i), 1) || !isRGBA(px, kpKeep.R, kpKeep.G, kpKeep.B, px[3], 0) {
				t.Fatalf("plain frame %d: keep patch %v, want colour %v with the matte's alpha %d", i, px, kpKeep, mtLeft(i))
			}
		}
		t.Run("one colour at the default similarity keeps it and a near colour", func(t *testing.T) {
			got := render(t, clip, info, recipe.MatteParams{Keep: []string{hexOf(kpKeep)}})
			check(t, got, base, "keep", "near")
		})
		t.Run("two colours: each gets its own wrapper", func(t *testing.T) {
			got := render(t, clip, info, recipe.MatteParams{Keep: []string{hexOf(kpKeep), "#" + hexOf(kpKeep2)}})
			check(t, got, base, "keep", "near", "keep2")
		})
		t.Run("a tight similarity keeps the exact colour only", func(t *testing.T) {
			got := render(t, clip, info, recipe.MatteParams{Keep: []string{hexOf(kpKeep)}, KeepSimilarity: 0.02})
			check(t, got, base, "keep")
		})
		t.Run("a wide similarity keeps the far colour too", func(t *testing.T) {
			got := render(t, clip, info, recipe.MatteParams{Keep: []string{hexOf(kpKeep)}, KeepSimilarity: 0.2})
			check(t, got, base, "keep", "near", "far")
		})
	})

	t.Run("rgba main: the union sits on the multiplied alpha", func(t *testing.T) {
		clip, info := pngClip(t, ff, dir, "keep-alpha", keepFrames(true), 10, true)
		base := render(t, clip, info, recipe.MatteParams{})
		// The wrapped merge: the right-half patch is a half-alpha source
		// pixel times the matte.
		for i, f := range base {
			want := uint8((128*int(mtRight(i)) + 127) / 255)
			if px := pixel(f, mtSize, 13, 1); !near(px[3], want, 1) || !near(px[0], kpKeep2.R, 1) {
				t.Fatalf("plain frame %d: keep2 patch %v, want colour %v with alpha %d (128 x matte / 255)", i, px, kpKeep2, want)
			}
		}
		got := render(t, clip, info, recipe.MatteParams{Keep: []string{hexOf(kpKeep), hexOf(kpKeep2)}})
		check(t, got, base, "keep", "near", "keep2")
	})
}
