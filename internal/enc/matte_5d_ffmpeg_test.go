package enc_test

// Phase 5d: MatteMaskScaleArgs against a real ffmpeg (skips without one on
// PATH): one stored matte frame at the model square comes out as exactly
// one 000001.png at the tracking size, 8-bit gray, a flat picture flat
// (the bicubic scale of a constant is exact) and a half-and-half picture
// still half-and-half (the subject side >= 128 where it was, the
// background side 0 where it was).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
)

func TestMatteMaskScale(t *testing.T) {
	ff := ffmpegOrSkip(t)
	const es, tw, th = 48, 32, 24
	dir := t.TempDir()
	t.Run("a flat matte scales to a flat mask of the tracking size", func(t *testing.T) {
		in := writePNG(t, filepath.Join(dir, "flat.png"), grayFrame(es, es, 151))
		out := filepath.Join(dir, "flat-out")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, ff, enc.MatteMaskScaleArgs(in, out, tw, th))
		got := readGraySeq(t, out)
		if len(got) != 1 {
			t.Fatalf("%d files out, want 1", len(got))
		}
		if b := got[0].Bounds(); b.Dx() != tw || b.Dy() != th {
			t.Fatalf("mask is %v, want %dx%d", b, tw, th)
		}
		for i, v := range got[0].Pix {
			if v != 151 {
				t.Fatalf("pixel %d is %d, want 151 (flat in, flat out)", i, v)
			}
		}
	})
	t.Run("a half-and-half matte keeps its halves", func(t *testing.T) {
		img := grayFrame(es, es, 0)
		fill(img, 0, 0, es/2, es, 255) // the left half is the subject
		in := writePNG(t, filepath.Join(dir, "half.png"), img)
		out := filepath.Join(dir, "half-out")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, ff, enc.MatteMaskScaleArgs(in, out, tw, th))
		got := readGraySeq(t, out)
		if len(got) != 1 || got[0].Bounds().Dx() != tw || got[0].Bounds().Dy() != th {
			t.Fatalf("out = %d files, first %v", len(got), got[0].Bounds())
		}
		g := got[0]
		for y := 0; y < th; y++ {
			// Away from the seam the halves are exact; the seam itself is
			// the bicubic ramp (2 px either side).
			if l, r := g.GrayAt(2, y).Y, g.GrayAt(tw-3, y).Y; l < 128 || r >= 128 {
				t.Fatalf("row %d: left %d (want >= 128), right %d (want < 128)", y, l, r)
			}
			if l, r := g.GrayAt(0, y).Y, g.GrayAt(tw-1, y).Y; l != 255 || r != 0 {
				t.Fatalf("row %d: edges %d / %d, want 255 / 0", y, l, r)
			}
		}
	})
}
