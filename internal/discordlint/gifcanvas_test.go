package discordlint

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Fixtures for the canvas simulation. cvFrame is more flexible than fxFrame
// (per-pixel content, any transparent index, local tables, interlace, no
// GCE); files are still produced by image/gif.EncodeAll plus byte surgery on
// the 16x16 fxPalette canvas.

// cvFrame describes one frame. pix (canvas coordinates) overrides fill.
type cvFrame struct {
	rect      image.Rectangle
	fill      byte
	pix       func(x, y int) byte
	trans     int // transparent index + 1; 0 = opaque (see ti)
	delay     int
	disposal  byte
	local     color.Palette // nil = the global fxPalette
	interlace bool          // store the rows in interlace order and set the flag
	noGCE     bool          // remove the frame's Graphic Control Extension
}

func either(cond bool, a, b byte) byte {
	if cond {
		return a
	}
	return b
}

// ti marks idx as a frame's transparent index.
func ti(idx byte) int { return int(idx) + 1 }

var (
	cvFull  = image.Rect(0, 0, fxW, fxH)
	cvRectA = image.Rect(2, 2, 10, 10)
	cvRectB = image.Rect(6, 6, 14, 14)
)

// interlaceOrder lists the frame rows in the order an interlaced stream
// stores them.
func interlaceOrder(h int) []int {
	var rows []int
	for pass := range 4 {
		for y := interlaceStart[pass]; y < h; y += interlaceStep[pass] {
			rows = append(rows, y)
		}
	}
	return rows
}

// encodeCv renders frames with image/gif, then applies the interlace / noGCE
// surgery.
func encodeCv(t testing.TB, frames []cvFrame) []byte {
	t.Helper()
	g := &gif.GIF{Config: image.Config{ColorModel: fxPalette, Width: fxW, Height: fxH}}
	for _, f := range frames {
		pal := f.local
		if pal == nil {
			pal = fxPalette
		}
		if f.trans > 0 {
			// Alpha 0 with the RGB kept: image/gif flags the index and still
			// writes the same colour table bytes.
			pal = append(color.Palette(nil), pal...)
			c := pal[f.trans-1].(color.RGBA)
			pal[f.trans-1] = color.RGBA{c.R, c.G, c.B, 0}
		}
		pm := image.NewPaletted(f.rect, pal)
		rows := make([]int, f.rect.Dy())
		for i := range rows {
			rows[i] = i
		}
		if f.interlace {
			rows = interlaceOrder(f.rect.Dy())
		}
		for i, row := range rows { // stream row i holds picture row `row`
			for x := f.rect.Min.X; x < f.rect.Max.X; x++ {
				v := f.fill
				if f.pix != nil {
					v = f.pix(x, f.rect.Min.Y+row)
				}
				pm.SetColorIndex(x, f.rect.Min.Y+i, v)
			}
		}
		g.Image = append(g.Image, pm)
		g.Delay = append(g.Delay, f.delay)
		g.Disposal = append(g.Disposal, f.disposal)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("EncodeAll: %v", err)
	}
	parsed, err := parseGIF(buf.Bytes())
	if err != nil {
		t.Fatalf("parseGIF: %v", err)
	}
	var drop []int
	surgery := false
	pf, _ := parsed.frames()
	for i, f := range frames {
		if f.interlace {
			img := pf[i].image
			img.raw = append([]byte(nil), img.raw...)
			img.raw[9] |= 0x40
			img.packed |= 0x40
			surgery = true
		}
		if f.noGCE && pf[i].gce != nil {
			drop = append(drop, pf[i].gceBlock)
			surgery = true
		}
	}
	if !surgery {
		return buf.Bytes()
	}
	parsed.removeBlocks(drop)
	return parsed.encode()
}

// verdictsOf runs the walker and requires a complete analysis.
func verdictsOf(t testing.TB, data []byte) []frameVerdict {
	t.Helper()
	g, err := parseGIF(data)
	if err != nil {
		t.Fatalf("parseGIF: %v", err)
	}
	frames, _ := g.frames()
	v, err := walkGIFCanvas(g, frames)
	if err != nil {
		t.Fatalf("walkGIFCanvas: %v", err)
	}
	if len(v) != len(frames) {
		t.Fatalf("%d verdicts for %d frames", len(v), len(frames))
	}
	return v
}

// walkPrefix runs the walker on a file whose analysis may end early and
// returns the verdicts of the analysed prefix, the frame count and the error
// that ended the walk.
func walkPrefix(t testing.TB, data []byte) ([]frameVerdict, int, error) {
	t.Helper()
	g, err := parseGIF(data)
	if err != nil {
		t.Fatalf("parseGIF: %v", err)
	}
	frames, _ := g.frames()
	v, err := walkGIFCanvas(g, frames)
	if (err == nil) != (len(v) == len(frames)) {
		t.Fatalf("%d verdicts for %d frames with err = %v", len(v), len(frames), err)
	}
	return v, len(frames), err
}

const (
	vC = frameChanges
	vH = frameNoopHarmless
	vU = frameNoopUnsafe
)

// Reference compositor, independent of the walker: image/gif decodes (and
// de-interlaces) the frames, full canvases are kept for every frame.

type refFrame struct {
	p, d     []uint32 // canvas after drawing / after disposal
	delay    int
	disposal byte
	rect     image.Rectangle
}

func refComposite(t testing.TB, data []byte) []refFrame {
	t.Helper()
	return refCompositeGIF(decodeGIF(t, data), parsedFrames(t, data), false)
}

// refCompositeGIF composites a decoded file; parsed only says which frames
// have no GCE: image/gif resets delay and transparency after every image but
// lets the disposal of the last GCE leak onto later frames that have none.
// The reserved disposals 5-7 leave the canvas; 4 does too (ffmpeg, gifsicle)
// unless browserModel is set, which restores as Chromium and Firefox do.
func refCompositeGIF(g *gif.GIF, parsed []gifFrame, browserModel bool) []refFrame {
	for k, f := range parsed {
		if f.gce == nil {
			g.Disposal[k] = 0
		}
	}
	w, h := g.Config.Width, g.Config.Height
	cur := make([]uint32, w*h)
	out := make([]refFrame, 0, len(g.Image))
	for k, img := range g.Image {
		before := slices.Clone(cur)
		r := img.Bounds()
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				cr, cg, cb, ca := img.Palette[img.ColorIndexAt(x, y)].RGBA()
				if ca != 0 {
					cur[y*w+x] = 0xFF000000 | (cr>>8)<<16 | (cg>>8)<<8 | cb>>8
				}
			}
		}
		f := refFrame{p: slices.Clone(cur), delay: g.Delay[k], disposal: g.Disposal[k], rect: r}
		effective := f.disposal
		if browserModel && effective == 4 {
			effective = gif.DisposalPrevious
		}
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				switch effective {
				case gif.DisposalBackground:
					cur[y*w+x] = 0
				case gif.DisposalPrevious:
					cur[y*w+x] = before[y*w+x]
				}
			}
		}
		f.d = slices.Clone(cur)
		out = append(out, f)
	}
	return out
}

// refVerdicts applies the rule's definition to the reference canvases. The
// analysis ends at the first frame with a reserved disposal (decoders
// disagree on it): only the frames before it get a verdict, and none of them
// is the last frame of the file.
func refVerdicts(frames []refFrame) []frameVerdict {
	out := make([]frameVerdict, len(frames))
	for k := range frames {
		if frames[k].disposal > 3 {
			return out[:k]
		}
		if k == 0 {
			continue
		}
		f, p := frames[k], frames[k-1]
		if !slices.Equal(f.p, p.p) {
			continue
		}
		harmless := slices.Equal(f.d, p.d)
		if f.disposal == 3 || p.disposal == 3 {
			harmless = f.disposal == p.disposal && f.rect == p.rect
		}
		out[k] = vU
		if harmless || k == len(frames)-1 {
			out[k] = vH
		}
	}
	return out
}

// refSpan is a stretch of the timeline showing one picture.
type refSpan struct {
	canvas []uint32
	cs     int
}

// refTimeline is the composited animation sampled per centisecond, run-length
// encoded: frames with delay 0 vanish, equal neighbours fuse.
func refTimeline(frames []refFrame) []refSpan {
	var out []refSpan
	for _, f := range frames {
		switch {
		case f.delay == 0:
		case len(out) > 0 && slices.Equal(out[len(out)-1].canvas, f.p):
			out[len(out)-1].cs += f.delay
		default:
			out = append(out, refSpan{f.p, f.delay})
		}
	}
	return out
}

func sameTimeline(a, b []refSpan) bool {
	return slices.EqualFunc(a, b, func(x, y refSpan) bool { return x.cs == y.cs && slices.Equal(x.canvas, y.canvas) })
}

// Fixtures named by the tests below.

// holdThenClear is what gifsicle -O2 writes for "hold pose A, then pose B":
// A with disposal 1 for the hold, an all-transparent disposal-2 frame over
// A's rectangle whose only job is the clear, then B.
func holdThenClear() []cvFrame {
	return []cvFrame{
		{rect: cvRectA, fill: 2, trans: ti(5), delay: 100, disposal: 1},
		{rect: cvRectA, fill: 5, trans: ti(5), delay: 4, disposal: 2},
		{rect: cvRectB, fill: 3, trans: ti(5), delay: 10, disposal: 1},
	}
}

// harmlessHolds holds pose A with 1x1 transparent disposal-1 frames.
func harmlessHolds() []cvFrame {
	dot := image.Rect(0, 0, 1, 1)
	return []cvFrame{
		{rect: cvRectA, fill: 2, trans: ti(5), delay: 10, disposal: 1},
		{rect: dot, fill: 5, trans: ti(5), delay: 20, disposal: 1},
		{rect: dot, fill: 5, trans: ti(5), delay: 30, disposal: 1},
		{rect: cvRectB, fill: 3, trans: ti(5), delay: 40, disposal: 1},
	}
}

// smallerClearThenRepeat has a byte-identical disposal-2 repeat (frame 3)
// that is NOT a no-op: frame 1 cleared less than frame 2 covers, so frame 2's
// transparent stripes still showed frame 0's pixels, which frame 2's own
// disposal then removed. It pins the walker's fast-path guard "the
// predecessor drew onto a cleared rectangle" (ctl.rect.In(pred.rect)).
func smallerClearThenRepeat() []cvFrame {
	stripe := func(x, y int) byte { return either(x%3 == 0, 2, 5) }
	return []cvFrame{
		{rect: cvFull, fill: 4, delay: 10, disposal: 1},
		{rect: image.Rect(0, 0, 2, 2), fill: 3, delay: 10, disposal: 2}, // clears less than frame 2 draws over
		{rect: cvFull, pix: stripe, trans: ti(5), delay: 10, disposal: 2},
		{rect: cvFull, pix: stripe, trans: ti(5), delay: 10, disposal: 2}, // same bytes, but frame 0's pixels are gone now
		{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
	}
}

// reservedDisposal4 is the file Chromium and ffmpeg play differently: frame
// 1 (green, disposal 4) is restored away by browsers and left by ffmpeg, so
// the all-transparent disposal-4 frame 2 shows red for 60 s in one and green
// in the other.
func reservedDisposal4() []cvFrame {
	return []cvFrame{
		{rect: cvFull, fill: 2, delay: 10, disposal: 1},
		{rect: cvFull, fill: 3, delay: 10, disposal: 4},
		{rect: cvFull, fill: 5, trans: ti(5), delay: 6000, disposal: 4},
		{rect: image.Rect(0, 0, 1, 1), fill: 4, delay: 10, disposal: 1},
	}
}

// moveOutOfScreen shifts frame k to the right until it sticks out of the
// logical screen by 3 pixels.
func moveOutOfScreen(t *testing.T, data []byte, k int) []byte {
	t.Helper()
	return mutateGIF(t, data, func(g *gifFile) {
		frames, _ := g.frames()
		img := frames[k].image
		left := fxW + 3 - int(img.width)
		img.raw = append([]byte(nil), img.raw...)
		img.left = uint16(left)
		img.raw[1], img.raw[2] = byte(left), 0
	})
}

func TestWalkGIFCanvasVerdicts(t *testing.T) {
	picture := func(x, y int) byte { return byte(1 + (x+3*y)%4) } // rows differ, so row order matters
	stripe := func(x, y int) byte { return either(x%3 == 0, 2, 5) }
	swapped := append(color.Palette(nil), fxPalette...)
	swapped[2], swapped[6] = swapped[6], swapped[2] // red is index 6 here
	cases := []struct {
		name   string
		frames []cvFrame
		want   []frameVerdict
	}{
		{"gifsicle -O2: all-transparent clear frame", holdThenClear(), []frameVerdict{vC, vU, vC}},
		{"gifsicle -O1: pose redrawn with disposal 2", []cvFrame{
			{rect: cvRectA, fill: 2, delay: 100, disposal: 1},
			{rect: cvRectA, fill: 2, delay: 4, disposal: 2},
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vU, vC}},
		{"disposal-1 transparent 1x1 holds", harmlessHolds(), []frameVerdict{vC, vH, vH, vC}},
		{"identical disposal-2 frames (byte-identical fast path)", []cvFrame{
			{rect: cvRectA, fill: 2, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvRectA, fill: 2, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvRectA, fill: 2, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvRectB, fill: 3, trans: ti(5), delay: 10, disposal: 2},
		}, []frameVerdict{vC, vH, vH, vC}},
		{"identical disposal-2 pictures, varying transparent index (decoded)", []cvFrame{
			{rect: cvRectA, fill: 2, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvRectA, fill: 2, trans: ti(7), delay: 10, disposal: 2},
			{rect: cvRectA, fill: 2, delay: 10, disposal: 2},
			{rect: cvRectB, fill: 3, trans: ti(5), delay: 10, disposal: 2},
		}, []frameVerdict{vC, vH, vH, vC}},
		{"byte-identical disposal-2 frame over older content is not a no-op", []cvFrame{
			// Frame 1 is transparent over frame 0's pixels, so they show
			// through in P[1]; frame 1's disposal clears them, and the
			// identical frame 2 then shows a different picture. Frame 3
			// repeats frame 2 on a cleared canvas.
			{rect: cvFull, fill: 4, delay: 10, disposal: 1},
			{rect: cvFull, pix: stripe, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvFull, pix: stripe, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvFull, pix: stripe, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vC, vC, vH, vC}},
		{"byte-identical disposal-2 frame after a smaller clear is not a no-op", smallerClearThenRepeat(), []frameVerdict{vC, vC, vC, vC, vC}},
		{"unsafe-looking no-op as the last frame", holdThenClear()[:2], []frameVerdict{vC, vH}},
		{"disposal-2 no-op with a larger rectangle over empty canvas", []cvFrame{
			{rect: cvRectA, fill: 2, trans: ti(5), delay: 10, disposal: 2},
			{rect: image.Rect(0, 0, 12, 12), pix: func(x, y int) byte { return either(image.Pt(x, y).In(cvRectA), 2, 5) }, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvRectB, fill: 3, trans: ti(5), delay: 10, disposal: 2},
		}, []frameVerdict{vC, vH, vC}},
		{"disposal-2 no-op whose larger rectangle holds pixels", []cvFrame{
			{rect: image.Rect(0, 0, 4, 4), fill: 4, delay: 10, disposal: 1},
			{rect: image.Rect(6, 6, 12, 12), fill: 2, trans: ti(5), delay: 10, disposal: 2},
			{rect: image.Rect(0, 0, 12, 12), pix: func(x, y int) byte { return either(x >= 6 && y >= 6, 2, 5) }, trans: ti(5), delay: 10, disposal: 2},
			{rect: cvRectB, fill: 3, trans: ti(5), delay: 10, disposal: 1},
		}, []frameVerdict{vC, vC, vU, vC}},
		{"redraw that leaves what its predecessor clears", []cvFrame{
			{rect: cvRectA, fill: 2, delay: 100, disposal: 2},
			{rect: cvRectA, fill: 2, delay: 4, disposal: 1},
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vU, vC}},
		{"interlaced redraw with disposal 2", []cvFrame{
			{rect: cvRectA, pix: picture, delay: 100, disposal: 1},
			{rect: cvRectA, pix: picture, delay: 4, disposal: 2, interlace: true},
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vU, vC}},
		{"interlaced frame whose rows are not the picture's", []cvFrame{
			{rect: cvRectA, pix: picture, delay: 100, disposal: 1},
			{rect: cvRectA, pix: func(x, y int) byte { return picture(x, y+1) }, delay: 4, disposal: 2, interlace: true},
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vC, vC}},
		{"local colour table with the same RGB at another index", []cvFrame{
			{rect: cvRectA, fill: 2, delay: 100, disposal: 1},
			{rect: cvRectA, fill: 6, delay: 4, disposal: 2, local: swapped},
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vU, vC}},
		{"local colour table with another RGB at the same index", []cvFrame{
			{rect: cvRectA, fill: 2, delay: 100, disposal: 1},
			{rect: cvRectA, fill: 2, delay: 4, disposal: 2, local: swapped},
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vC, vC}},
		{"frames without a GCE are opaque with disposal 0", []cvFrame{
			{rect: cvRectA, fill: 2, noGCE: true},
			{rect: cvRectA, fill: 2, noGCE: true},            // leaves, as frame 0 does
			{rect: cvRectA, fill: 5, noGCE: true},            // index 5 is opaque black without a GCE
			{rect: cvRectA, fill: 0, delay: 10, disposal: 1}, // the same black from index 0; 1 leaves as 0 does
			{rect: cvRectA, fill: 0, delay: 4, disposal: 2},  // clears what frame 3 leaves
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vH, vC, vH, vU, vC}},
		{"disposal 3 restores the rectangle", []cvFrame{
			{rect: cvFull, fill: 4, delay: 10, disposal: 1},
			{rect: cvRectA, fill: 2, delay: 10, disposal: 3},
			{rect: image.Rect(0, 0, 1, 1), fill: 5, trans: ti(5), delay: 10, disposal: 1}, // shows frame 0 again
			{rect: cvRectA, fill: 2, delay: 10, disposal: 3},
			{rect: cvRectA, fill: 2, trans: ti(5), delay: 10, disposal: 3}, // same disposal and rectangle
			{rect: cvRectA, fill: 2, delay: 10, disposal: 1},               // disposal differs: not provably equal
			{rect: cvRectB, fill: 3, delay: 10, disposal: 1},
		}, []frameVerdict{vC, vC, vC, vC, vH, vU, vC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := encodeCv(t, tc.frames)
			if got := verdictsOf(t, data); !slices.Equal(got, tc.want) {
				t.Errorf("verdicts = %v, want %v", got, tc.want)
			}
			// The reference compositor must agree (it keeps the test table honest).
			if ref := refVerdicts(refComposite(t, data)); !slices.Equal(ref, tc.want) {
				t.Errorf("reference verdicts = %v, want %v", ref, tc.want)
			}
		})
	}
}

// A frame that sticks out of the logical screen ends the analysis AT THAT
// FRAME: decoders clip it, grow the screen or reject the file (image/gif), so
// neither the rule nor MergeGIFHolds may conclude anything from it or from
// what follows. The frames before it are analysed as usual.
func TestWalkGIFCanvasFrameOutsideScreen(t *testing.T) {
	a := harmlessHolds()
	a = append(a, a[3]) // a hold of B behind the offending frame
	data := moveOutOfScreen(t, encodeCv(t, a), 3)
	if got, n, err := walkPrefix(t, data); err == nil || n != 5 || !slices.Equal(got, []frameVerdict{vC, vH, vH}) {
		t.Errorf("verdicts %v of %d frames, err %v", got, n, err)
	}
	c := expectCheck(t, lintOnly(t, data, TargetEmote), RuleGIFNoopFrameDisposal, true, false)
	for _, want := range []string{"not analysed from frame 3 on", "frame 3 (8x8 at 11,6) exceeds the 16x16 logical screen", "nothing found in frames 0-2, the rest is unchecked"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
	// The holds before the frame are merged, the one behind it is not.
	out, merged := mergeHolds(t, data)
	if merged != 2 || !slices.Equal(delaysOf(t, out), []int{60, 40, 40}) {
		t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
	}
	// Frame 0 already: nothing is analysed.
	data = moveOutOfScreen(t, encodeCv(t, a), 0)
	c = expectCheck(t, lintOnly(t, data, TargetEmote), RuleGIFNoopFrameDisposal, true, false)
	if !strings.Contains(c.Detail, "not analysed: frame 0 (8x8 at 11,2) exceeds") || strings.Contains(c.Detail, "nothing found") {
		t.Errorf("detail: %s", c.Detail)
	}
	if _, merged := mergeHolds(t, data); merged != 0 {
		t.Errorf("merged %d frames of a file that was not analysed", merged)
	}

	// An unsafe frame before an out-of-screen LAST frame is still reported.
	b := append(harmlessHolds()[:3], holdThenClear()[1:]...) // A, hold, hold, clear of A (unsafe), B
	b = append(b, b[4], b[4])                                // a hold of B, and the frame that will stick out
	data = moveOutOfScreen(t, encodeCv(t, b), 6)
	if got, _, err := walkPrefix(t, data); err == nil || !slices.Equal(got, []frameVerdict{vC, vH, vH, vU, vC, vH}) {
		t.Errorf("verdicts %v, err %v", got, err)
	}
	r := lintOnly(t, data, TargetEmote)
	c = expectCheck(t, r, RuleGIFNoopFrameDisposal, false, false)
	if r.OK || c.Level != LevelError || !strings.Contains(c.Detail, "frame 3:") || !strings.Contains(c.Detail, "not analysed from frame 6 on") {
		t.Errorf("report OK %v, check %+v", r.OK, c)
	}
	out, merged = mergeHolds(t, data)
	if merged != 3 || !slices.Equal(delaysOf(t, out), []int{60, 4, 20, 10}) {
		t.Errorf("unsafe frame before the end: merged %d, delays %v", merged, delaysOf(t, out))
	}

	// No tail step after a partial analysis: without the last frame the
	// unsafe redraw would become the last frame and go (see the guards test).
	redraw := cvFrame{rect: cvRectA, fill: 2, delay: 4, disposal: 2}
	d := []cvFrame{{rect: cvRectA, fill: 2, delay: 100, disposal: 1}, redraw, redraw, holdThenClear()[2]}
	out, merged = mergeHolds(t, moveOutOfScreen(t, encodeCv(t, d), 3))
	if merged != 1 || !slices.Equal(delaysOf(t, out), []int{100, 8, 10}) {
		t.Errorf("tail step: merged %d, delays %v", merged, delaysOf(t, out))
	}

	// The canvas cap is the one stop that voids the whole walk, unsafe frame
	// 1 included: the canvas cannot be allocated. It only counts the frames
	// that would be analysed.
	huge := func(g *gifFile) {
		g.width, g.height = 65535, 65535
		frames, _ := g.frames()
		img := frames[2].image // 5000x5000 > maxCanvasPixels; its LZW data is never reached
		img.raw = append([]byte(nil), img.raw...)
		img.width, img.height = 5000, 5000
		img.raw[5], img.raw[6], img.raw[7], img.raw[8] = 5000&0xFF, 5000>>8, 5000&0xFF, 5000>>8
	}
	data = mutateGIF(t, encodeCv(t, holdThenClear()), huge)
	if got, _, err := walkPrefix(t, data); len(got) != 0 || err == nil {
		t.Errorf("canvas cap: verdicts %v, err %v", got, err)
	}
	c = expectCheck(t, lintOnly(t, data, TargetEmote), RuleGIFNoopFrameDisposal, true, false)
	if !strings.Contains(c.Detail, "not analysed: frames span 5004x5004 pixels, over the analysis cap") {
		t.Errorf("canvas cap: detail: %s", c.Detail)
	}
	data = mutateGIF(t, data, func(g *gifFile) {
		frames, _ := g.frames()
		frames[2].gce.disposal, frames[2].gce.raw = 4, nil // the huge frame is not analysed any more
	})
	c = expectCheck(t, lintOnly(t, data, TargetEmote), RuleGIFNoopFrameDisposal, false, false)
	if !strings.Contains(c.Detail, "frame 1:") || !strings.Contains(c.Detail, "not analysed from frame 2 on") {
		t.Errorf("huge frame behind the end of the analysis: detail: %s", c.Detail)
	}

	// A zero-size frame far from the others draws and disposes nothing.
	data = mutateGIF(t, encodeCv(t, harmlessHolds()), func(g *gifFile) {
		frames, _ := g.frames()
		img := frames[1].image
		img.raw = append([]byte(nil), img.raw...)
		img.left, img.top, img.width = 15, 15, 0
		img.raw[1], img.raw[3], img.raw[5] = 15, 15, 0
	})
	if got, want := verdictsOf(t, data), []frameVerdict{vC, vH, vH, vC}; !slices.Equal(got, want) {
		t.Errorf("zero-size frame: verdicts = %v, want %v", got, want)
	}
	// A huge logical screen costs nothing when the frames are small.
	data = mutateGIF(t, encodeCv(t, holdThenClear()), func(g *gifFile) { g.width, g.height = 65535, 65535 })
	if got, want := verdictsOf(t, data), []frameVerdict{vC, vU, vC}; !slices.Equal(got, want) {
		t.Errorf("verdicts = %v, want %v", got, want)
	}
}

// A reserved disposal (4-7) ends the analysis like a frame outside the
// screen: Chromium and Firefox read 4 as restore previous, ffmpeg and
// gifsicle as leave, so a frame that is a no-op for one is not for the other.
func TestWalkGIFCanvasReservedDisposal(t *testing.T) {
	data := encodeCv(t, reservedDisposal4())
	// The two decoder models really disagree on this file ...
	g, parsed := decodeGIF(t, data), parsedFrames(t, data)
	leave, browser := refCompositeGIF(g, parsed, false), refCompositeGIF(g, parsed, true)
	if !slices.Equal(leave[2].p, leave[1].p) || slices.Equal(browser[2].p, browser[1].p) {
		t.Fatal("fixture: frame 2 must be a no-op for ffmpeg's reading and a change for the browsers'")
	}
	// ... so nothing is concluded from frame 1 on.
	if got, _, err := walkPrefix(t, data); err == nil || !slices.Equal(got, []frameVerdict{vC}) {
		t.Errorf("verdicts %v, err %v", got, err)
	}
	if _, merged := mergeHolds(t, data); merged != 0 {
		t.Errorf("merged %d frames behind a reserved disposal", merged)
	}
	r := lintOnly(t, data, TargetEmote)
	c := expectCheck(t, r, RuleGIFNoopFrameDisposal, true, false)
	for _, want := range []string{"not analysed from frame 1 on", "frame 1 uses the reserved disposal 4", "nothing found in frame 0, the rest is unchecked"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
	if expectCheck(t, r, RuleGIFDisposal, false, false); r.OK {
		t.Error("gif.disposal must still fail the file")
	}

	// Holds before the reserved frame are merged, an unsafe one is reported,
	// nothing behind it is touched — whatever the reserved value.
	for _, disposal := range []byte{4, 5, 7} {
		a := append(harmlessHolds()[:3], holdThenClear()[1:]...) // A, hold, hold, clear of A (unsafe), B
		a = append(a, a[4], a[4], a[4])
		a[5].disposal, a[6].disposal = disposal, disposal // byte-identical disposal-N frames, then a hold of those
		data := encodeCv(t, a)
		if got, _, err := walkPrefix(t, data); err == nil || !slices.Equal(got, []frameVerdict{vC, vH, vH, vU, vC}) {
			t.Errorf("disposal %d: verdicts %v, err %v", disposal, got, err)
		}
		c := expectCheck(t, lintOnly(t, data, TargetEmote), RuleGIFNoopFrameDisposal, false, false)
		if !strings.Contains(c.Detail, "frame 3:") || !strings.Contains(c.Detail, fmt.Sprintf("not analysed from frame 5 on: frame 5 uses the reserved disposal %d", disposal)) {
			t.Errorf("disposal %d: detail: %s", disposal, c.Detail)
		}
		out, merged := mergeHolds(t, data)
		if merged != 2 || !slices.Equal(delaysOf(t, out), []int{60, 4, 10, 10, 10, 10}) {
			t.Errorf("disposal %d: merged %d, delays %v", disposal, merged, delaysOf(t, out))
		}
	}
}

func TestLintGIFNoopFrameDisposal(t *testing.T) {
	bad := encodeCv(t, holdThenClear())
	for _, target := range []Target{TargetEmote, TargetSticker, TargetAttachment, TargetAttachment500} {
		r, out := lintFix(t, bad, target)
		c := expectCheck(t, r, RuleGIFNoopFrameDisposal, false, false)
		if c.Level != LevelError || r.OK {
			t.Errorf("%s: level %s, report OK %v; want an error", target, c.Level, r.OK)
		}
		for _, want := range []string{"frame 1:", "Discord drops frames that do not change the picture", "re-encode"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail lacks %q: %s", want, c.Detail)
			}
		}
		samePixels(t, bad, out) // nothing the byte fixer could do
	}
	r := lintOnly(t, bad, TargetNone)
	if c := expectCheck(t, r, RuleGIFNoopFrameDisposal, false, false); c.Level != LevelWarn {
		t.Errorf("TargetNone: level %s, want warn", c.Level)
	}

	r = lintOnly(t, encodeCv(t, harmlessHolds()), TargetEmote)
	if c := expectCheck(t, r, RuleGIFNoopFrameDisposal, true, false); c.Level != LevelError || !strings.Contains(c.Detail, "2 harmless hold frames") {
		t.Errorf("check: %+v", c)
	}
	if c := expectCheck(t, lintOnly(t, encodeCv(t, harmlessHolds()), TargetNone), RuleGIFNoopFrameDisposal, true, false); c.Level != LevelWarn {
		t.Errorf("TargetNone pass: level %s, want warn", c.Level)
	}

	// The rule comes right after gif.disposal.
	ids := ruleIDs(r)
	if i := slices.Index(ids, RuleGIFDisposal); i < 0 || i+1 >= len(ids) || ids[i+1] != RuleGIFNoopFrameDisposal {
		t.Errorf("rule order: %v", ids)
	}

	single := encodeCv(t, holdThenClear()[:1])
	if c := expectCheck(t, lintOnly(t, single, TargetEmote), RuleGIFNoopFrameDisposal, true, false); !strings.Contains(c.Detail, "fewer than two frames") {
		t.Errorf("single frame: %s", c.Detail)
	}
}

func TestLintGIFNoopFrameDisposalNotAnalysed(t *testing.T) {
	// The clear frame itself is garbage: nothing is known from there on.
	broken := corruptFrame(t, encodeCv(t, holdThenClear()), 1)
	c := expectCheck(t, lintOnly(t, broken, TargetEmote), RuleGIFNoopFrameDisposal, true, false)
	if !strings.Contains(c.Detail, "not analysed from frame 1 on") || !strings.Contains(c.Detail, "LZW") || !strings.Contains(c.Detail, "nothing found in frame 0, the rest is unchecked") {
		t.Errorf("detail: %s", c.Detail)
	}
	// A finding before the damaged frame stands.
	broken = corruptFrame(t, encodeCv(t, holdThenClear()), 2)
	c = expectCheck(t, lintOnly(t, broken, TargetEmote), RuleGIFNoopFrameDisposal, false, false)
	if !strings.Contains(c.Detail, "frame 1:") || !strings.Contains(c.Detail, "not analysed from frame 2 on") {
		t.Errorf("detail: %s", c.Detail)
	}
}

// parsedFrames returns the frames of data.
func parsedFrames(t testing.TB, data []byte) []gifFrame {
	t.Helper()
	g, err := parseGIF(data)
	if err != nil {
		t.Fatalf("parseGIF: %v", err)
	}
	frames, _ := g.frames()
	return frames
}

func mergeHolds(t testing.TB, data []byte) ([]byte, int) {
	t.Helper()
	out, merged, err := MergeGIFHolds(data)
	if err != nil {
		t.Fatalf("MergeGIFHolds: %v", err)
	}
	if merged == 0 && (len(out) != len(data) || (len(data) > 0 && &out[0] != &data[0])) {
		t.Fatal("merged == 0 must return the input slice")
	}
	if got, want := len(parsedFrames(t, out)), len(parsedFrames(t, data))-merged; got != want {
		t.Fatalf("output has %d frames, want %d", got, want)
	}
	return out, merged
}

func delaysOf(t testing.TB, data []byte) []int {
	var out []int
	for _, f := range parsedFrames(t, data) {
		d := -1 // no GCE
		if f.gce != nil {
			d = int(f.gce.delayCS)
		}
		out = append(out, d)
	}
	return out
}

func TestMergeGIFHolds(t *testing.T) {
	data := encodeCv(t, harmlessHolds())
	out, merged := mergeHolds(t, data)
	if merged != 2 {
		t.Fatalf("merged = %d, want 2", merged)
	}
	// image/gif writes the same bytes for the animation without the holds.
	a := harmlessHolds()
	a[0].delay = 60
	if want := encodeCv(t, []cvFrame{a[0], a[3]}); !bytes.Equal(out, want) {
		t.Errorf("merged bytes differ from the direct encoding\n got %x\nwant %x", out, want)
	}
	if again, n := mergeHolds(t, out); n != 0 || !bytes.Equal(again, out) {
		t.Errorf("second pass merged %d", n)
	}
	if c := findCheck(t, lintOnly(t, out, TargetEmote), RuleGIFNoopFrameDisposal); !c.OK || !strings.Contains(c.Detail, "0 harmless hold frames") {
		t.Errorf("merged file: %+v", c)
	}
}

func TestMergeGIFHoldsKeepsOtherBytes(t *testing.T) {
	// A NETSCAPE block, a comment inside the dropped frame's run, a second
	// GCE before the dropped frame, a 5-byte GCE on the survivor, no trailer
	// and bytes image/gif would never write: all but the dropped frame's
	// blocks and the survivor's two delay bytes must survive verbatim.
	comment := &gifRawExt{label: gifLabelComment, raw: []byte{0x21, 0xFE, 0x02, 'h', 'i', 0x00}}
	data := mutateGIF(t, encodeCv(t, harmlessHolds()), func(g *gifFile) {
		frames, _ := g.frames()
		g.insertBlock(frames[1].gceBlock, newGCE(7)) // duplicate GCE of frame 1's run
		g.insertBlock(frames[1].gceBlock, comment)
		g.insertBlock(0, newNetscapeLoop(3))
		f0 := frames[0].gce
		f0.raw = []byte{0x21, 0xF9, 0x05, f0.raw[3], f0.raw[4], f0.raw[5], f0.raw[6], 0xAB, 0x00}
	})
	data = data[:len(data)-1] // no trailer
	out, merged := mergeHolds(t, data)
	if merged != 2 {
		t.Fatalf("merged = %d, want 2", merged)
	}
	want := mutateGIF(t, data, func(g *gifFile) {
		frames, _ := g.frames()
		var drop []int
		for _, f := range frames[1:3] {
			drop = append(append(append(drop, f.imageBlock), f.gceBlock), f.dupGCE...)
		}
		g.removeBlocks(drop)
		f0 := frames[0].gce
		f0.raw = append([]byte(nil), f0.raw...)
		f0.raw[4], f0.raw[5] = 60, 0
	})
	want = want[:len(want)-1]
	if !bytes.Equal(out, want) {
		t.Errorf("bytes differ\n got %x\nwant %x", out, want)
	}
	if !bytes.Contains(out, comment.raw) || !bytes.Contains(out, []byte{0x21, 0xF9, 0x05}) || out[len(out)-1] == gifTrailer {
		t.Error("comment, 5-byte GCE or the missing trailer did not survive")
	}
}

func TestMergeGIFHoldsGuards(t *testing.T) {
	same := func(t *testing.T, data []byte) {
		t.Helper()
		if _, merged := mergeHolds(t, data); merged != 0 {
			t.Errorf("merged = %d, want 0", merged)
		}
	}
	t.Run("unsafe no-ops are left alone", func(t *testing.T) {
		same(t, encodeCv(t, holdThenClear()))
	})
	t.Run("nothing to merge", func(t *testing.T) {
		same(t, encodeFx(t, alphaAnim()))
		same(t, encodeCv(t, holdThenClear()[:1]))
	})
	t.Run("frame 0 survives a file of identical frames", func(t *testing.T) {
		f := cvFrame{rect: cvFull, fill: 2, delay: 10, disposal: 2}
		out, merged := mergeHolds(t, encodeCv(t, []cvFrame{f, f, f, f}))
		if merged != 3 || !slices.Equal(delaysOf(t, out), []int{40}) {
			t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
		}
	})
	t.Run("a no-op last frame is merged whatever its disposal", func(t *testing.T) {
		out, merged := mergeHolds(t, encodeCv(t, holdThenClear()[:2]))
		if merged != 1 || !slices.Equal(delaysOf(t, out), []int{104}) {
			t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
		}
	})
	t.Run("an unsafe no-op that becomes the last frame goes too", func(t *testing.T) {
		redraw := cvFrame{rect: cvRectA, fill: 2, delay: 4, disposal: 2} // the gifsicle -O1 shape
		a := []cvFrame{{rect: cvRectA, fill: 2, delay: 100, disposal: 1}, redraw, redraw, redraw}
		if got := verdictsOf(t, encodeCv(t, a)); !slices.Equal(got, []frameVerdict{vC, vU, vH, vH}) {
			t.Fatalf("verdicts %v", got)
		}
		out, merged := mergeHolds(t, encodeCv(t, a))
		if merged != 3 || !slices.Equal(delaysOf(t, out), []int{112}) {
			t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
		}
		// Not when the frames behind it were not analysed.
		a = append(a, holdThenClear()[2])
		out, merged = mergeHolds(t, corruptFrame(t, encodeCv(t, a), 4))
		if merged != 2 || !slices.Equal(delaysOf(t, out), []int{100, 12, 10}) {
			t.Errorf("damaged tail: merged %d, delays %v", merged, delaysOf(t, out))
		}
	})
	t.Run("65535 cs overflow", func(t *testing.T) {
		a := harmlessHolds()
		a[0].delay, a[1].delay, a[2].delay = 60000, 5536, 100
		out, merged := mergeHolds(t, encodeCv(t, a))
		// 60000+5536 does not fit: frame 1 stays and takes frame 2 instead.
		if merged != 1 || !slices.Equal(delaysOf(t, out), []int{60000, 5636, 40}) {
			t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
		}
		a[1].delay = 5535
		out, merged = mergeHolds(t, encodeCv(t, a))
		if merged != 1 || !slices.Equal(delaysOf(t, out), []int{65535, 100, 40}) {
			t.Errorf("at the limit: merged %d, delays %v", merged, delaysOf(t, out))
		}
	})
	t.Run("predecessor without a GCE", func(t *testing.T) {
		a := []cvFrame{
			{rect: cvRectA, fill: 2, noGCE: true},
			{rect: cvRectA, fill: 2, delay: 20, disposal: 1},
			{rect: cvRectA, fill: 2, delay: 30, disposal: 1},
			{rect: cvRectB, fill: 3, delay: 40, disposal: 1},
		}
		if got := verdictsOf(t, encodeCv(t, a)); !slices.Equal(got, []frameVerdict{vC, vH, vH, vC}) {
			t.Fatalf("verdicts %v", got)
		}
		out, merged := mergeHolds(t, encodeCv(t, a))
		if merged != 1 || !slices.Equal(delaysOf(t, out), []int{-1, 50, 40}) {
			t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
		}
	})
	t.Run("dropped frame without a GCE adds no delay", func(t *testing.T) {
		a := []cvFrame{
			{rect: cvRectA, fill: 2, delay: 20, disposal: 1},
			{rect: cvRectA, fill: 2, noGCE: true},
			{rect: cvRectB, fill: 3, delay: 40, disposal: 1},
		}
		out, merged := mergeHolds(t, encodeCv(t, a))
		if merged != 1 || !slices.Equal(delaysOf(t, out), []int{20, 40}) {
			t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
		}
	})
	t.Run("malformed predecessor GCE", func(t *testing.T) {
		same(t, mutateGIF(t, encodeCv(t, harmlessHolds()[:2]), func(g *gifFile) {
			frames, _ := g.frames()
			*frames[0].gce = gifGCE{raw: []byte{0x21, 0xF9, 0x02, 0x04, 0x0A, 0x00}} // 2-byte block: parsed as zero fields
		}))
	})
	for _, idx := range []int{0, 1} {
		t.Run(fmt.Sprintf("user-input flag on frame %d", idx), func(t *testing.T) {
			same(t, mutateGIF(t, encodeCv(t, harmlessHolds()[:2]), func(g *gifFile) {
				frames, _ := g.frames()
				frames[idx].gce.userInput = true
				frames[idx].gce.raw = nil
			}))
		})
	}
	t.Run("frames after an undecodable one are left alone", func(t *testing.T) {
		a := harmlessHolds()
		a = append(a, a[3], a[3]) // a second hold behind the damage
		data := corruptFrame(t, encodeCv(t, a), 3)
		out, merged := mergeHolds(t, data)
		if merged != 2 || !slices.Equal(delaysOf(t, out), []int{60, 40, 40, 40}) {
			t.Errorf("merged %d, delays %v", merged, delaysOf(t, out))
		}
	})
	t.Run("unparseable input", func(t *testing.T) {
		if out, merged, err := MergeGIFHolds([]byte("GIF89a")); err == nil || out != nil || merged != 0 {
			t.Errorf("got %v, %d, %v", out, merged, err)
		}
	})
}

// randomCvAnim builds an animation rich in holds, clears and every disposal;
// about one in six has a reserved disposal (4-7) somewhere, which ends the
// analysis there. Frames outside the logical screen are left to the table
// tests: image/gif rejects them, so the reference compositor has no say.
func randomCvAnim(rng *rand.Rand) []cvFrame {
	shuffled := append(color.Palette(nil), fxPalette...)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	n := 2 + rng.IntN(10)
	frames := make([]cvFrame, 0, n)
	for len(frames) < n {
		var f cvFrame
		if len(frames) > 0 && rng.IntN(100) < 45 {
			f = frames[len(frames)-1] // a hold; the knobs below may still differ
		} else {
			x0, y0 := rng.IntN(fxW-1), rng.IntN(fxH-1)
			f.rect = image.Rect(x0, y0, x0+1+rng.IntN(fxW-x0), y0+1+rng.IntN(fxH-y0))
			if rng.IntN(4) == 0 {
				f.rect = cvFull
			}
			a, b, m := byte(rng.IntN(8)), byte(rng.IntN(8)), 1+rng.IntN(5)
			switch rng.IntN(3) {
			case 0:
				f.fill = a
			case 1:
				f.pix = func(x, y int) byte { return either((x+2*y)%m == 0, a, b) }
			case 2:
				f.pix = func(x, y int) byte { return either(y%2 == 0, a, b) }
			}
			f.trans = 0
			if rng.IntN(3) > 0 {
				f.trans = ti([]byte{a, b, byte(rng.IntN(8))}[rng.IntN(3)])
			}
			f.local = nil
			if rng.IntN(5) == 0 {
				f.local = shuffled
			}
		}
		if rng.IntN(3) == 0 {
			f.trans = rng.IntN(9)
		}
		f.interlace = rng.IntN(6) == 0
		f.disposal = []byte{0, 1, 1, 1, 2, 2, 2, 3}[rng.IntN(8)]
		f.delay = []int{0, 2, 5, 10, 100}[rng.IntN(5)]
		f.noGCE = false
		if rng.IntN(12) == 0 {
			f.noGCE, f.trans, f.disposal, f.delay = true, 0, 0, 0
		} else if rng.IntN(36) == 0 {
			f.disposal = byte(4 + rng.IntN(4))
		}
		frames = append(frames, f)
	}
	return frames
}

// TestCanvasProperties checks, on random animations and against the
// image/gif-based reference compositor, that the walker's verdicts follow
// the rule's definition (including where the analysis ends) and that
// MergeGIFHolds leaves the composited timeline (sampled per centisecond)
// untouched — under both readings of disposal 4.
func TestCanvasProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260919, 1))
	table := [][]cvFrame{holdThenClear(), harmlessHolds(), smallerClearThenRepeat(), reservedDisposal4()}
	for range 400 {
		table = append(table, randomCvAnim(rng))
	}
	var noops, unsafeNoops, mergedTotal, partial, mergedPartial int
	for i, anim := range table {
		data := encodeCv(t, anim)
		g, parsed := decodeGIF(t, data), parsedFrames(t, data)
		ref := refCompositeGIF(g, parsed, false)
		want := refVerdicts(ref)
		got, n, err := walkPrefix(t, data)
		if !slices.Equal(got, want) {
			t.Fatalf("animation %d: verdicts %v, reference %v\n%+v", i, got, want, anim)
		}
		complete := err == nil
		if !complete {
			partial++
			if !strings.Contains(err.Error(), fmt.Sprintf("frame %d uses the reserved disposal %d", len(got), anim[len(got)].disposal)) {
				t.Fatalf("animation %d: analysis ended at frame %d of %d with %v\n%+v", i, len(got), n, err, anim)
			}
		}
		for _, v := range want {
			if v != vC {
				noops++
			}
			if v == vU {
				unsafeNoops++
			}
		}
		out, merged := mergeHolds(t, data)
		mergedTotal += merged
		if !complete {
			mergedPartial += merged
		}
		gOut, parsedOut := decodeGIF(t, out), parsedFrames(t, out)
		for _, browserModel := range []bool{false, true} {
			if !sameTimeline(refTimeline(refCompositeGIF(g, parsed, browserModel)), refTimeline(refCompositeGIF(gOut, parsedOut, browserModel))) {
				t.Fatalf("animation %d: merging %d frames changed the timeline (browser model %v)\n%+v", i, merged, browserModel, anim)
			}
		}
		if _, again := mergeHolds(t, out); again != 0 {
			t.Fatalf("animation %d: second pass merged %d more", i, again)
		}
		// Unsafe frames are never dropped while a later frame changes the
		// picture (behind the last change they are the tail, which may go) —
		// and never at all when the analysis ended early: no tail step, and
		// every frame from the first unanalysed one on survives.
		lastChange := 0
		for k, v := range want {
			if v == vC {
				lastChange = k
			}
		}
		after, nOut, _ := walkPrefix(t, out)
		a, b := countVerdict(want[:lastChange], vU), countVerdict(after, vU)
		if !complete {
			a = countVerdict(want, vU)
			if nOut-len(after) != n-len(got) {
				t.Fatalf("animation %d: %d unanalysed frames before merging, %d after: %+v", i, n-len(got), nOut-len(after), anim)
			}
		}
		if b < a || b > countVerdict(want, vU) {
			t.Fatalf("animation %d: %d unsafe frames that must stay, %d after merging: %+v", i, a, b, anim)
		}
	}
	if noops < 200 || unsafeNoops < 50 || mergedTotal < 100 || partial < 30 || mergedPartial < 10 {
		t.Errorf("generator too tame: %d no-ops, %d unsafe, %d merged; %d partial analyses with %d merged", noops, unsafeNoops, mergedTotal, partial, mergedPartial)
	}
}

func countVerdict(vs []frameVerdict, v frameVerdict) int {
	n := 0
	for _, x := range vs {
		if x == v {
			n++
		}
	}
	return n
}

// TestNoopFrameDisposalLocalFiles checks the Discord-verified files of the
// 2026-09-19 investigation when they exist in the working tree (tmp/ is not
// part of the repository). The list is explicit — those directories are
// scratch space, and anything else dropped into them is none of this test's
// business; a file that is absent is skipped.
func TestNoopFrameDisposalLocalFiles(t *testing.T) {
	files := []struct {
		path   string // relative to the repository root
		unsafe []int  // frames the rule must report; nil = the check passes
	}{
		{"tmp/discord-stack-test/1-current-pipeline.gif", []int{1, 3, 9}}, // stacks on Discord
		{"tmp/discord-stack-test/2-no-clear-frames.gif", nil},
		{"tmp/discord-stack-test/3-gifsicle-O1.gif", []int{1, 3, 9}}, // stacks on Discord
		{"tmp/discord-stack-test/4-all-disposal2-varying-tidx.gif", nil},
		{"tmp/discord-stack-test/5-all-disposal2-cropped.gif", nil},
		{"tmp/discord-stack-test/6-all-disposal2-fullcanvas.gif", nil},
		{"tmp/Dragoon.gif", nil},
		{"tmp/repro/out/base.gif", nil},
	}
	checked := 0
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(f.path)))
		if os.IsNotExist(err) {
			t.Logf("%s: not present, skipped", f.path)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		checked++
		var unsafeFrames []int
		for k, v := range verdictsOf(t, data) {
			if v == vU {
				unsafeFrames = append(unsafeFrames, k)
			}
		}
		if !slices.Equal(unsafeFrames, f.unsafe) {
			t.Errorf("%s: unsafe frames %v, want %v", f.path, unsafeFrames, f.unsafe)
		}
		start := time.Now()
		r, _, err := LintGIF(data, TargetSticker, false)
		if err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		if c := findCheck(t, r, RuleGIFNoopFrameDisposal); c.OK != (f.unsafe == nil) {
			t.Errorf("%s: %+v", f.path, c)
		}
		out, merged := mergeHolds(t, data)
		if !sameTimeline(refTimeline(refComposite(t, data)), refTimeline(refComposite(t, out))) {
			t.Errorf("%s: merging changed the timeline", f.path)
		}
		t.Logf("%-40s %3d frames, unsafe %v, LintGIF %v, MergeGIFHolds drops %d (%d -> %d bytes)",
			filepath.Base(f.path), r.Frames, unsafeFrames, elapsed.Round(10*time.Microsecond), merged, len(data), len(out))
	}
	if checked == 0 {
		t.Skip("local acceptance files not present")
	}
}
