package enc_test

// Phase 5c: the stabilisation, tracker-stream and gate argv against a real
// ffmpeg (skips without one on PATH), on tiny synthetic gray sequences:
//
//   - MatteStabiliseArgs: N frames in → exactly N files out for both chains,
//     frame k out is frame k in (no lag), a 1-frame pop in either direction
//     disappears under light, a real transition is kept, the ends pass
//     through, and strong holds a vanished pixel >= 128 for one extra frame;
//   - MatteGateArgs: N files out, core → 255, outside → 0, band → the edge
//     matte of the SAME frame (scaled from the segmenter's square to the
//     tracking size), an empty mask → all 0, a full mask → all 255, radius
//     0 → the mask itself;
//   - MatteTrackSourceArgs streams frames x W x H x 3 bytes and
//     MatteTrackFrameArgs' slot k equals the stream's frame k (a slot past
//     the end: the last frame).

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// grayFrame is a w x h gray picture; fill paints a rectangle.
func grayFrame(w, h int, v uint8) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = v
	}
	return img
}

func fill(img *image.Gray, x0, y0, x1, y1 int, v uint8) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetGray(x, y, color.Gray{Y: v})
		}
	}
}

// writeGraySeq writes frames as 000001.png … to dir.
func writeGraySeq(t *testing.T, dir string, frames []*image.Gray) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, fr := range frames {
		writePNG(t, filepath.Join(dir, matteFile(i+1)), fr)
	}
}

// readGraySeq reads every 000001.png … in dir (in order, until a number is
// missing) as gray pictures.
func readGraySeq(t *testing.T, dir string) []*image.Gray {
	t.Helper()
	var out []*image.Gray
	for n := 1; ; n++ {
		data, err := os.ReadFile(filepath.Join(dir, matteFile(n)))
		if err != nil {
			return out
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", matteFile(n), err)
		}
		g, ok := img.(*image.Gray)
		if !ok {
			t.Fatalf("%s decodes to %T, want 8-bit gray", matteFile(n), img)
		}
		out = append(out, g)
	}
}

func TestMatteStabiliseChains(t *testing.T) {
	ff := ffmpegOrSkip(t)
	const w, h, n = 16, 12, 8
	// Pixel A: opaque throughout except a 1-frame drop-out on frame 4.
	// Pixel B: transparent throughout except a 1-frame pop-in on frame 3.
	// Pixel C: a real transition — opaque on 0..3, transparent on 4..7.
	// Pixel D: opaque on frame 0 only; pixel E: opaque on frame 7 only.
	// Pixel F: opaque on frames 2..3 only (a 2-frame event — kept).
	type px struct{ x, y int }
	a, b, c, d, e, f := px{2, 2}, px{8, 8}, px{12, 10}, px{14, 1}, px{1, 10}, px{6, 4}
	var in []*image.Gray
	for i := 0; i < n; i++ {
		fr := grayFrame(w, h, 0)
		set := func(p px, on bool) {
			if on {
				fr.SetGray(p.x, p.y, color.Gray{Y: 255})
			}
		}
		set(a, i != 4)
		set(b, i == 3)
		set(c, i < 4)
		set(d, i == 0)
		set(e, i == n-1)
		set(f, i == 2 || i == 3)
		in = append(in, fr)
	}
	at := func(frames []*image.Gray, i int, p px) uint8 { return frames[i].GrayAt(p.x, p.y).Y }

	for _, mode := range []string{enc.MatteStabiliseLight, enc.MatteStabiliseStrong} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			inDir, outDir := filepath.Join(dir, "raw"), filepath.Join(dir, "stab-"+mode)
			writeGraySeq(t, inDir, in)
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				t.Fatal(err)
			}
			run(t, ff, enc.MatteStabiliseArgs(inDir, outDir, n, mode))
			out := readGraySeq(t, outDir)
			if len(out) != n {
				t.Fatalf("%d frames in, %d files out", n, len(out))
			}
			for i, fr := range out {
				if fr.Bounds().Dx() != w || fr.Bounds().Dy() != h {
					t.Fatalf("frame %d is %v, want %dx%d", i, fr.Bounds(), w, h)
				}
			}
			// Both chains: the pops vanish, the transition is on time.
			for i := 0; i < n; i++ {
				if got := at(out, i, a); got != 255 {
					t.Errorf("frame %d: the drop-out pixel is %d, want 255 (restored)", i, got)
				}
				if got := at(out, i, b); got != 0 {
					t.Errorf("frame %d: the pop-in pixel is %d, want 0 (removed)", i, got)
				}
			}
			for i := 0; i < 4; i++ {
				if got := at(out, i, c); got != 255 {
					t.Errorf("frame %d: the transition pixel is %d before the edge, want 255", i, got)
				}
			}
			// The ends pass through: a first-frame / last-frame-only pixel
			// has a cloned neighbour that votes with it.
			if got := at(out, 0, d); got != 255 {
				t.Errorf("frame 0: the first-frame pixel is %d, want 255 (ends pass through)", got)
			}
			if got := at(out, n-1, e); got != 255 {
				t.Errorf("frame %d: the last-frame pixel is %d, want 255 (ends pass through)", n-1, got)
			}
			// A 2-frame event survives the 3-frame median.
			for _, i := range []int{2, 3} {
				if got := at(out, i, f); got != 255 {
					t.Errorf("frame %d: the 2-frame event pixel is %d, want 255", i, got)
				}
			}
			switch mode {
			case enc.MatteStabiliseLight:
				// Bit-exact median: every output pixel is one of its window's
				// values; the transition is sharp and on time (zero lag).
				for i := 4; i < n; i++ {
					if got := at(out, i, c); got != 0 {
						t.Errorf("light frame %d: the transition pixel is %d after the edge, want 0", i, got)
					}
				}
				for i := 1; i < n; i++ {
					if got := at(out, i, d); got != 0 {
						t.Errorf("light frame %d: the first-frame pixel is %d, want 0", i, got)
					}
				}
				for i := 0; i < n; i++ {
					for k := range out[i].Pix {
						if v := out[i].Pix[k]; v != 0 && v != 255 {
							t.Fatalf("light frame %d byte %d is %d: a median of binary frames must stay binary", i, k, v)
						}
					}
				}
			case enc.MatteStabiliseStrong:
				// The decay-0.7 hold: a vanished pixel stays >= 128 for ONE
				// extra frame (255 x 0.7 = 178), below 128 the next (124),
				// and decays away (never back to 255).
				if got := at(out, 4, c); got < 128 || got == 255 {
					t.Errorf("strong frame 4: the transition pixel is %d, want a >= 128 hold below 255", got)
				}
				if got := at(out, 5, c); got >= 128 {
					t.Errorf("strong frame 5: the transition pixel is %d, want < 128 (a one-frame hold)", got)
				}
				for i := 5; i < n; i++ {
					if prev, got := at(out, i-1, c), at(out, i, c); got > prev {
						t.Errorf("strong frame %d: the transition pixel rose %d -> %d", i, prev, got)
					}
				}
				if got := at(out, 1, d); got < 128 {
					t.Errorf("strong frame 1: the first-frame pixel is %d, want the hold", got)
				}
				// A removed pop never leaves a trail: the hold runs AFTER the median.
				for i := 0; i < n; i++ {
					if got := at(out, i, b); got != 0 {
						t.Errorf("strong frame %d: the pop-in pixel is %d, want 0", i, got)
					}
				}
			}
		})
	}

	t.Run("-frames:v caps a longer input", func(t *testing.T) {
		dir := t.TempDir()
		inDir, outDir := filepath.Join(dir, "raw"), filepath.Join(dir, "stab")
		writeGraySeq(t, inDir, in)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, ff, enc.MatteStabiliseArgs(inDir, outDir, 5, enc.MatteStabiliseLight))
		if out := readGraySeq(t, outDir); len(out) != 5 {
			t.Errorf("%d files out, want 5", len(out))
		}
	})
}

func TestMatteGateChain(t *testing.T) {
	ff := ffmpegOrSkip(t)
	// Tracking size 32 x 24, the segmenter's square 48 x 48 (scaled to the
	// tracking size by the chain). Mask frames: a 16 x 12 box at (8,6); an
	// empty mask; a full mask. Edge frames: flat 150, 90, 200 — flat, so the
	// bicubic scale is exact and a band pixel names its frame.
	const tw, th, es, n, r = 32, 24, 48, 3, 2
	masks := []*image.Gray{grayFrame(tw, th, 0), grayFrame(tw, th, 0), grayFrame(tw, th, 255)}
	fill(masks[0], 8, 6, 24, 18, 255)
	edgeVals := []uint8{150, 90, 200}
	var edges []*image.Gray
	for _, v := range edgeVals {
		edges = append(edges, grayFrame(es, es, v))
	}
	dir := t.TempDir()
	trackDir, edgeDir := filepath.Join(dir, "track"), filepath.Join(dir, "edge")
	writeGraySeq(t, trackDir, masks)
	writeGraySeq(t, edgeDir, edges)

	t.Run("radius 2: core, band, outside", func(t *testing.T) {
		outDir := filepath.Join(dir, "gated-r2")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, ff, enc.MatteGateArgs(trackDir, edgeDir, outDir, n, r))
		out := readGraySeq(t, outDir)
		if len(out) != n {
			t.Fatalf("%d files out, want %d", len(out), n)
		}
		for i, fr := range out {
			if fr.Bounds().Dx() != tw || fr.Bounds().Dy() != th {
				t.Fatalf("frame %d is %v, want the tracking size %dx%d", i, fr.Bounds(), tw, th)
			}
		}
		// Frame 0: the box (8..23, 6..17); core = eroded by 2 (10..21,
		// 8..15) → 255; band = within 2 px outside the box's edge (either
		// side) → the edge value 150; outside dilate (6..25, 4..19) → 0.
		g := out[0]
		for y := 0; y < th; y++ {
			for x := 0; x < tw; x++ {
				var want uint8
				switch {
				case x >= 10 && x < 22 && y >= 8 && y < 16:
					want = 255
				case x >= 6 && x < 26 && y >= 4 && y < 20:
					want = edgeVals[0]
				default:
					want = 0
				}
				if got := g.GrayAt(x, y).Y; got != want {
					t.Errorf("frame 0 (%d,%d) = %d, want %d", x, y, got, want)
				}
			}
		}
		for k, v := range out[1].Pix {
			if v != 0 {
				t.Fatalf("frame 1 (empty mask) byte %d = %d, want 0 whatever the edge matte (%d)", k, v, edgeVals[1])
			}
		}
		for k, v := range out[2].Pix {
			if v != 255 {
				t.Fatalf("frame 2 (full mask) byte %d = %d, want 255 whatever the edge matte (%d)", k, v, edgeVals[2])
			}
		}
	})

	t.Run("the band carries the edge matte of the same frame", func(t *testing.T) {
		// The same box on every frame: the band pixel follows the edge
		// sequence frame for frame (pts pairing), the core stays 255.
		same := []*image.Gray{masks[0], masks[0], masks[0]}
		trackDir2 := filepath.Join(dir, "track-same")
		writeGraySeq(t, trackDir2, same)
		outDir := filepath.Join(dir, "gated-same")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, ff, enc.MatteGateArgs(trackDir2, edgeDir, outDir, n, r))
		out := readGraySeq(t, outDir)
		if len(out) != n {
			t.Fatalf("%d files out, want %d", len(out), n)
		}
		for i := range out {
			if got := out[i].GrayAt(7, 12).Y; got != edgeVals[i] {
				t.Errorf("frame %d band pixel = %d, want edge %d", i, got, edgeVals[i])
			}
			if got := out[i].GrayAt(16, 12).Y; got != 255 {
				t.Errorf("frame %d core pixel = %d, want 255", i, got)
			}
			if got := out[i].GrayAt(0, 0).Y; got != 0 {
				t.Errorf("frame %d outside pixel = %d, want 0", i, got)
			}
		}
	})

	t.Run("radius 0 is the mask itself", func(t *testing.T) {
		outDir := filepath.Join(dir, "gated-r0")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, ff, enc.MatteGateArgs(trackDir, edgeDir, outDir, n, 0))
		out := readGraySeq(t, outDir)
		if len(out) != n {
			t.Fatalf("%d files out, want %d", len(out), n)
		}
		for i := range out {
			if !bytes.Equal(out[i].Pix, masks[i].Pix) {
				t.Errorf("frame %d differs from the mask", i)
			}
		}
	})
}

func TestMatteTrackStreams(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	clip, _ := cfrClip(t, ff, dir)
	p := &graph.Plan{Filter: "[0:v]fps=25,format=rgba[out]", OutLabel: "[out]", Width: clipW, Height: clipH, FPS: matteFPS, Duration: clipSeconds, Frames: matteFrames, Speed: 1, SourceFPS: clipFPS}
	const w, h = 64, 48 // a non-square tracking size that exercises the scale
	frame := w * h * 3

	stream := run(t, ff, enc.MatteTrackSourceArgs(clip, p, w, h))
	if len(stream) != matteFrames*frame {
		t.Fatalf("stream is %d bytes, want %d x %d x %d x 3 = %d", len(stream), matteFrames, w, h, matteFrames*frame)
	}
	// Distinct frames (testsrc moves every frame), so a match names its slot.
	for i := 1; i < matteFrames; i++ {
		if bytes.Equal(stream[(i-1)*frame:i*frame], stream[i*frame:(i+1)*frame]) {
			t.Fatalf("stream frames %d and %d are equal: the clip is not distinct per frame", i-1, i)
		}
	}
	for _, slot := range []int{0, 1, 7, 24, 25, 50, 98, 99, 100, 500} {
		want := min(slot, matteFrames-1)
		got := run(t, ff, enc.MatteTrackFrameArgs(clip, p, w, h, slot))
		if len(got) != frame {
			t.Errorf("slot %d: %d bytes, want one %dx%d rgb24 frame (%d)", slot, len(got), w, h, frame)
			continue
		}
		idx := -1
		for i := 0; i < matteFrames; i++ {
			if bytes.Equal(got, stream[i*frame:(i+1)*frame]) {
				idx = i
				break
			}
		}
		if idx != want {
			t.Errorf("slot %d: the frame still is stream frame %d, want %d (args %q)", slot, idx, want, enc.MatteTrackFrameArgs(clip, p, w, h, slot))
		}
	}
}
