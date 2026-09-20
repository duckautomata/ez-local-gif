package enc_test

// Real-ffmpeg regression tests for the two frame optimisations of ffmpeg's GIF
// encoder (libavcodec/gif.c, -gifflags +offsetting+transdiff, both on by
// default). GIFArgs switches offsetting off for every master with
// transparency, and transdiff too only with GIFOptions.CompleteFrames — the
// second encode jobs runs for a clip that mixes fully opaque and transparent
// frames. Skips when ffmpeg is not on PATH; the masters are synthesised in Go.
//
// offsetting: gif_crop_translucent crops a frame that has transparent pixels
// to its opaque bounding box, but scans the columns over the rows
// [top, bottom) — "for (i = *y_start; i < y_end; i++)", where gif_crop_opaque
// has "y <= y_end". The part of the box's bottom row that sticks out past the
// columns the rows above use falls outside the emitted rectangle and becomes
// transparent (FFmpeg 8.0, 9.0.1 and master as of 2026-09).
//
// transdiff: a fully opaque frame is diffed against the previous frame and
// its unchanged pixels are written as transparent, with disposal 1 — also
// inside a clip whose other frames are disposed to background, where those
// pixels then show through as holes. For such a mixed clip jobs encodes a
// second time with GIFOptions.CompleteFrames (both flags off, every frame a
// complete picture) and discordlint.DisposeCompleteFrames gives the opaque
// frames disposal 2 too, so they do not stay under the next transparent one.
//
// gif_crop_opaque, the inter-frame diff crop of clips without transparency,
// scans [top, bottom] and is fine: the opaque path keeps the default flags
// (and its much smaller frames).

import (
	"bytes"
	"image"
	"image/gif"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
)

const cropW, cropH, cropFrames = 24, 16, 4

// cropShapeAt reports whether (x, y) of frame f is part of the test shape: a
// 4 px bar over rows 3..8 standing on a 12 px foot on row 9, moving 2 px per
// frame. The foot row is the bottom row of the shape's bounding box and holds
// its left-most and its right-most pixels — in no other row.
func cropShapeAt(f, x, y int) bool {
	x -= 2 * f
	return (y >= 3 && y < 9 && x >= 8 && x < 12) || (y == 9 && x >= 4 && x < 16)
}

// writeCropMaster writes the RGBA master: the shape in red over a transparent
// (alpha) or blue (opaque) background.
func writeCropMaster(t *testing.T, dir string, alpha bool) enc.Master {
	t.Helper()
	buf := make([]byte, 0, cropW*cropH*4*cropFrames)
	for f := 0; f < cropFrames; f++ {
		for y := 0; y < cropH; y++ {
			for x := 0; x < cropW; x++ {
				switch {
				case cropShapeAt(f, x, y):
					buf = append(buf, 230, 40, 40, 255)
				case alpha:
					buf = append(buf, 0, 0, 0, 0)
				default:
					buf = append(buf, 40, 40, 230, 255)
				}
			}
		}
	}
	path := filepath.Join(dir, "crop.rgba")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return enc.Master{Path: path, Width: cropW, Height: cropH, FPS: 10, Frames: cropFrames, HasAlpha: alpha}
}

// cropMismatches encodes nothing: it decodes the GIF at path, composites it
// per spec and returns the pixels whose "is the shape drawn here" differs from
// the master (the shape is opaque red; everything else transparent or blue),
// plus the frame rectangles.
func cropMismatches(t *testing.T, path string) (wrong []image.Point, rects []image.Rectangle) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Image) != cropFrames {
		t.Fatalf("%d frames, want %d (one per master frame)", len(g.Image), cropFrames)
	}
	for f, canvas := range compositeGIF(g) {
		rects = append(rects, g.Image[f].Rect)
		for y := 0; y < cropH; y++ {
			for x := 0; x < cropW; x++ {
				c := canvas[y*cropW+x]
				if shape := c.A != 0 && c.R > c.B; shape != cropShapeAt(f, x, y) {
					wrong = append(wrong, image.Pt(x, y))
				}
			}
		}
	}
	return wrong, rects
}

// TestGIFAlphaKeepsBottomRowEdgePixels: the GIF of an alpha master shows
// exactly the master's opaque pixels, including the ends of the foot row that
// ffmpeg's translucent crop drops.
func TestGIFAlphaKeepsBottomRowEdgePixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m := writeCropMaster(t, dir, true)
	out := filepath.Join(dir, "alpha.gif")
	args := enc.GIFArgs(m, enc.GIFOptions{HasAlpha: true}, out)
	if o, err := tryFF(ff, args); err != nil {
		t.Fatalf("GIFArgs: %v\n%s", err, o)
	}
	wrong, rects := cropMismatches(t, out)
	if len(wrong) > 0 {
		t.Errorf("%d pixels differ from the master's alpha mask, first %v (frame rects %v)", len(wrong), wrong[0], rects)
	}

	// The workaround is load-bearing while the encoder crops wrongly: the
	// same command with the default flags loses the foot's ends. A build that
	// no longer does is only logged — full-canvas frames cost nothing once
	// gifsicle has re-cropped them.
	i := argIndex(args, "-gifflags")
	if i < 0 {
		t.Fatalf("GIFArgs for an alpha master has no -gifflags: %q", args)
	}
	stock := filepath.Join(dir, "stock.gif")
	stockArgs := slices.Concat(args[:i], args[i+2:len(args)-1], []string{stock})
	if o, err := tryFF(ff, stockArgs); err != nil {
		t.Fatalf("default -gifflags: %v\n%s", err, o)
	}
	if lost, stockRects := cropMismatches(t, stock); len(lost) == 0 {
		t.Logf("this ffmpeg's translucent crop keeps the bottom row's edge pixels (rects %v): offsetting need not be switched off for this any more", stockRects)
	} else {
		t.Logf("default -gifflags loses %d pixels, first %v (rects %v): upstream bug present", len(lost), lost[0], stockRects)
	}
}

// TestGIFOpaqueDiffCropKeepsBottomRowEdgePixels: the opaque path keeps
// ffmpeg's default inter-frame diff crop, which has no such defect — the ends
// of the moving foot are the left-most / right-most changed pixels and sit on
// the bottom row of the changed area, and they are repainted.
func TestGIFOpaqueDiffCropKeepsBottomRowEdgePixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m := writeCropMaster(t, dir, false)
	out := filepath.Join(dir, "opaque.gif")
	args := enc.GIFArgs(m, enc.GIFOptions{}, out)
	if argIndex(args, "-gifflags") >= 0 {
		t.Errorf("the opaque path must keep ffmpeg's default -gifflags (diff-cropped frames): %q", args)
	}
	if o, err := tryFF(ff, args); err != nil {
		t.Fatalf("GIFArgs: %v\n%s", err, o)
	}
	wrong, rects := cropMismatches(t, out)
	if len(wrong) > 0 {
		t.Errorf("%d pixels differ from the master, first %v (frame rects %v)", len(wrong), wrong[0], rects)
	}
	cropped := false
	for _, r := range rects[1:] {
		cropped = cropped || r != image.Rect(0, 0, cropW, cropH)
	}
	if !cropped {
		t.Errorf("no frame after the first is diff-cropped (rects %v): the experiment proves nothing about gif_crop_opaque", rects)
	}
}

const mixedFrames = 6

// mixedClassAt is the mixed master: the moving shape in red over a
// transparent background on frames 0, 1, 4 and 5 and over an opaque blue one
// on frames 2 and 3 (0 = transparent, 1 = red, 2 = blue).
func mixedClassAt(f, x, y int) int {
	switch {
	case cropShapeAt(f, x, y):
		return 1
	case f == 2 || f == 3:
		return 2
	}
	return 0
}

// mixedMismatches composites the GIF per spec and counts, per frame, the
// pixels that differ from the mixed master.
func mixedMismatches(t *testing.T, data []byte) (perFrame []int, total int, g *gif.GIF) {
	t.Helper()
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Image) != mixedFrames {
		t.Fatalf("%d frames, want %d (one per master frame)", len(g.Image), mixedFrames)
	}
	for f, canvas := range compositeGIF(g) {
		n := 0
		for y := 0; y < cropH; y++ {
			for x := 0; x < cropW; x++ {
				c, class := canvas[y*cropW+x], 0
				switch {
				case c.A != 0 && c.R > c.B:
					class = 1
				case c.A != 0:
					class = 2
				}
				if class != mixedClassAt(f, x, y) {
					n++
				}
			}
		}
		perFrame = append(perFrame, n)
		total += n
	}
	return perFrame, total, g
}

// TestGIFAlphaOpaqueFramesAreComplete: in an alpha master that mixes fully
// opaque and transparent frames, the opaque frames come out as complete
// full-canvas pictures (no "unchanged = transparent" pixels, which would be
// holes after the disposed frame before them), and with
// discordlint.DisposeCompleteFrames — what jobs runs on this output — the clip
// is exact: the opaque picture does not stay under the transparent frames
// that follow it.
func TestGIFAlphaOpaqueFramesAreComplete(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	buf := make([]byte, 0, cropW*cropH*4*mixedFrames)
	for f := 0; f < mixedFrames; f++ {
		for y := 0; y < cropH; y++ {
			for x := 0; x < cropW; x++ {
				buf = append(buf, [][]byte{{0, 0, 0, 0}, {230, 40, 40, 255}, {40, 40, 230, 255}}[mixedClassAt(f, x, y)]...)
			}
		}
	}
	m := enc.Master{Path: filepath.Join(dir, "mixed.rgba"), Width: cropW, Height: cropH, FPS: 10, Frames: mixedFrames, HasAlpha: true}
	if err := os.WriteFile(m.Path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	// The first encode (what every alpha master gets) shows the mix: ffmpeg
	// gives the frames with transparency disposal 2 and the opaque ones
	// disposal 1. That is what jobs keys the second encode on.
	first := filepath.Join(dir, "first.gif")
	if o, err := tryFF(ff, enc.GIFArgs(m, enc.GIFOptions{HasAlpha: true}, first)); err != nil {
		t.Fatalf("GIFArgs: %v\n%s", err, o)
	}
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	firstPer, firstTotal, fg0 := mixedMismatches(t, firstData)
	if want := []byte{2, 2, 1, 1, 2, 2}; !bytes.Equal(fg0.Disposal, want) {
		t.Errorf("first encode: disposals %v, want %v — jobs detects the mix by them", fg0.Disposal, want)
	}
	if firstTotal == 0 {
		t.Logf("this ffmpeg renders the mixed clip exactly without CompleteFrames: the second encode is no longer needed")
	} else {
		t.Logf("first encode (-gifflags -offsetting): %d wrong pixels %v", firstTotal, firstPer)
	}

	out := filepath.Join(dir, "mixed.gif")
	args := enc.GIFArgs(m, enc.GIFOptions{HasAlpha: true, CompleteFrames: true}, out)
	if o, err := tryFF(ff, args); err != nil {
		t.Fatalf("GIFArgs: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	rawPer, rawTotal, g := mixedMismatches(t, raw)
	for f, fr := range g.Image {
		if fr.Rect != image.Rect(0, 0, cropW, cropH) {
			t.Errorf("frame %d rect %v, want the full canvas", f, fr.Rect)
		}
		if f != 2 && f != 3 {
			continue
		}
		for i, idx := range fr.Pix {
			if _, _, _, a := fr.Palette[idx].RGBA(); a == 0 {
				t.Errorf("opaque frame %d has a transparent pixel at %d,%d: transdiff is still on", f, i%fr.Stride, i/fr.Stride)
				break
			}
		}
	}
	fixed, patched, err := discordlint.DisposeCompleteFrames(raw)
	if err != nil {
		t.Fatal(err)
	}
	per, total, fg := mixedMismatches(t, fixed)
	if total != 0 {
		t.Errorf("after DisposeCompleteFrames (%d GCEs patched) %d pixels differ from the master, per frame %v (disposals %v)", patched, total, per, fg.Disposal)
	}
	t.Logf("ffmpeg's own GCEs: disposals %v, %d wrong pixels %v; every frame disposed: %v, 0 wrong", g.Disposal, rawTotal, rawPer, fg.Disposal)

	// For the record: what the default flags make of the same master.
	i := argIndex(args, "-gifflags")
	stock := filepath.Join(dir, "stock.gif")
	if o, err := tryFF(ff, slices.Concat(args[:i], args[i+2:len(args)-1], []string{stock})); err != nil {
		t.Fatalf("default -gifflags: %v\n%s", err, o)
	}
	stockData, err := os.ReadFile(stock)
	if err != nil {
		t.Fatal(err)
	}
	stockPer, stockTotal, _ := mixedMismatches(t, stockData)
	t.Logf("default -gifflags: %d wrong pixels %v", stockTotal, stockPer)
}
