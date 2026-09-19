package discordlint

import (
	"bytes"
	"compress/lzw"
	"fmt"
	"image"
	"io"
	"slices"
)

// Canvas simulation. gif.noop-frame-disposal and MergeGIFHolds need to know
// which frames leave the composited picture unchanged, so the animation is
// played per spec on a canvas where 0 means "background / cleared" and any
// drawn pixel is an opaque colour value:
//
//   - pixels equal to the frame's transparent index (GCE transparency flag
//     set) are skipped; colours come from the frame's local table, else the
//     global one, so two frames with different tables compare by RGB;
//   - disposal 2 clears the frame's rectangle to 0, disposal 3 restores it
//     to what it held before the frame was drawn, 0/1 leave it;
//   - a frame without a GCE is opaque with disposal 0.
//
// Two kinds of frame END THE ANALYSIS, because decoders disagree on what they
// show: a frame that sticks out of the logical screen (clipped, the screen
// grown, or the file rejected) and a frame with a reserved disposal 4-7
// (Chromium and Firefox read 4 as "restore previous", ffmpeg and gifsicle
// read 4-7 as "leave"). The frames before the first such frame are analysed
// as usual — a verdict for frame k only depends on frames 0..k — and nothing
// is concluded about that frame or any later one.
//
// With P[k] the canvas after drawing frame k and D[k] the canvas after then
// applying its disposal, frame k >= 1 is a NO-OP when P[k] == P[k-1]. A
// decoder that drops such a frame (folding its delay into k-1) shows the same
// animation only when D[k] == D[k-1], or when k is the last frame (the canvas
// is reset on loop restart, so its disposal never matters): those no-ops are
// HARMLESS, the others UNSAFE.
//
// Two canvases are kept (current and P[k-1]) and compared / re-synced only
// over the rectangles a frame touched, and frames are decoded straight onto
// the canvas a few rows at a time, so the cost is the LZW decode plus a few
// passes over each frame's rectangle. Interlaced frames are de-interlaced.

const (
	// maxCanvasPixels caps the canvas area the simulation allocates for (the
	// part of the logical screen the frames cover; two uint32 canvases, 128
	// MiB at the cap). Far beyond any Discord upload; larger files are
	// reported as not analysed.
	maxCanvasPixels = 16 << 20
	// maxWalkPixels caps the pixels decoded over all frames of one walk
	// (LZW expands ~1000:1 on flat frames, so file size is no bound).
	maxWalkPixels = int64(1) << 31
)

// frameVerdict classifies one frame of a walk.
type frameVerdict uint8

const (
	frameChanges      frameVerdict = iota // frame 0, or the picture changed
	frameNoopHarmless                     // picture unchanged, dropping it is invisible
	frameNoopUnsafe                       // picture unchanged, but its disposal differs from its predecessor's
)

// frameCtl is what the simulation needs from a frame besides its pixels.
type frameCtl struct {
	disposal    byte
	transparent bool
	transIndex  byte
	rect        image.Rectangle // relative to the simulated area
}

func (a frameCtl) sameDrawing(b frameCtl) bool {
	return a.disposal == b.disposal && a.rect == b.rect && a.transparent == b.transparent &&
		(!a.transparent || a.transIndex == b.transIndex)
}

// walkGIFCanvas plays frames on the canvas and returns one verdict per
// analysed frame. When err is non-nil the analysis stopped at frame
// len(verdicts) — a frame outside the logical screen, a reserved disposal,
// undecodable pixel data, a frame over maxAnalysedPixels or the
// maxWalkPixels budget — and nothing is known about that frame or the later
// ones; the verdicts returned (frames 0..len(verdicts)-1) are still valid,
// and none of them is the "last frame" exemption, which needs every frame.
// The one error that voids the whole walk (no verdicts at all) is the canvas
// cap: the frames that could be analysed span more than maxCanvasPixels.
func walkGIFCanvas(g *gifFile, frames []gifFrame) ([]frameVerdict, error) {
	if len(frames) < 2 {
		return make([]frameVerdict, len(frames)), nil // no frame has a predecessor
	}
	// Pixels no frame covers stay 0 for ever, so the canvas only spans the
	// bounding box of the frame rectangles; ctls are relative to it. A small
	// file declaring a huge logical screen then costs nothing. The pre-pass
	// ends at the first frame decoders disagree on (stopErr); only the frames
	// before it are analysed and count towards the bounding box.
	screen := image.Rect(0, 0, int(g.width), int(g.height))
	ctls := make([]frameCtl, 0, len(frames))
	var bounds image.Rectangle
	var stopErr error
	for k, f := range frames {
		img := f.image
		ctl := frameCtl{rect: image.Rect(int(img.left), int(img.top), int(img.left)+int(img.width), int(img.top)+int(img.height))}
		if !ctl.rect.In(screen) {
			// Clipped, the screen grown, or the file rejected: what such a
			// frame shows is not known.
			stopErr = fmt.Errorf("frame %d (%dx%d at %d,%d) exceeds the %dx%d logical screen", k, img.width, img.height, img.left, img.top, g.width, g.height)
			break
		}
		if ctl.rect.Empty() {
			ctl.rect = image.Rectangle{} // zero-size frame: draws and disposes nothing
		}
		if f.gce != nil {
			ctl.disposal, ctl.transparent, ctl.transIndex = f.gce.disposal, f.gce.transparent, f.gce.transIndex
		}
		if ctl.disposal > 3 {
			stopErr = fmt.Errorf("frame %d uses the reserved disposal %d, which decoders read differently (browsers treat 4 as restore previous, ffmpeg and gifsicle as leave)", k, ctl.disposal)
			break
		}
		ctls = append(ctls, ctl)
		bounds = bounds.Union(ctl.rect)
	}
	if len(ctls) == 0 {
		return nil, stopErr // frame 0 already; len(frames) >= 2, so stopErr is set
	}
	w, h := bounds.Dx(), bounds.Dy()
	if int64(w)*int64(h) > maxCanvasPixels {
		return nil, fmt.Errorf("frames span %dx%d pixels, over the analysis cap", w, h)
	}
	for k := range ctls {
		if !ctls[k].rect.Empty() { // Sub would turn an empty rectangle into a non-zero one
			ctls[k].rect = ctls[k].rect.Sub(bounds.Min)
		}
	}
	c := &gifCanvas{g: g, w: w, cur: make([]uint32, w*h), prev: make([]uint32, w*h), budget: maxWalkPixels}
	verdicts := make([]frameVerdict, 0, len(ctls))

	// pred is frame k-1. predClean says pred's rectangle was entirely 0
	// before pred was drawn (frame 0, or frame k-2 cleared a superset of it).
	var pred frameCtl
	predClean := false
	for k, ctl := range ctls {
		img := frames[k].image
		// Fast path: the same image bytes (descriptor, local table and LZW
		// data) drawn with the same GCE semantics as the predecessor repaint
		// the same pixels. With disposal 2 that only reproduces P[k-1] when
		// the predecessor drew onto a cleared rectangle (its transparent
		// pixels may otherwise have shown older content the clear removed).
		// The canvas stays at P[k-1]: no decode, no disposal, no sync, and
		// D[k] == D[k-1] because disposal and rectangle are equal.
		if k > 0 && ctl.sameDrawing(pred) && (ctl.disposal != 2 || predClean) && bytes.Equal(img.raw, frames[k-1].image.raw) {
			verdicts = append(verdicts, frameNoopHarmless)
			continue // pred and predClean describe this frame as well
		}

		// cur is P[k-1]: apply the predecessor's disposal, then draw.
		var disposed image.Rectangle
		if k > 0 {
			switch pred.disposal {
			case 2:
				disposed = pred.rect
				c.clear(disposed)
			case 3:
				disposed = pred.rect
				c.restore(disposed)
			}
		}
		if ctl.disposal == 3 {
			c.save(ctl.rect)
		}
		if err := c.draw(img, ctl); err != nil {
			return verdicts, fmt.Errorf("frame %d: %w", k, err)
		}
		// Only the disposed and the drawn rectangle can differ from P[k-1].
		// Short-circuit order matters: both rectangles must be synced.
		same := c.syncPrev(disposed)
		same = c.syncPrev(ctl.rect) && same

		v := frameChanges
		if k > 0 && same {
			v = frameNoopUnsafe
			// The last frame of the FILE: never true when the pre-pass stopped.
			if k == len(frames)-1 || c.sameDisposalResult(ctl, pred) {
				v = frameNoopHarmless
			}
		}
		verdicts = append(verdicts, v)
		predClean = k == 0 || (pred.disposal == 2 && ctl.rect.In(pred.rect))
		pred = ctl
	}
	return verdicts, stopErr
}

// gifCanvas is the state of one walk.
type gifCanvas struct {
	g      *gifFile
	w      int      // row stride: width of the simulated part of the logical screen
	cur    []uint32 // the canvas being composited
	prev   []uint32 // P[k-1]: the canvas right after the previous frame was drawn
	saved  []uint32 // disposal 3: rows of the frame's rectangle before it was drawn
	budget int64    // pixels that may still be decoded

	gctPal   [256]uint32
	gctReady bool
	lctPal   [256]uint32
	src      subBlockReader
	lzw      *lzw.Reader
	rows     []byte
}

func (c *gifCanvas) clear(r image.Rectangle) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		clear(c.cur[y*c.w+r.Min.X : y*c.w+r.Max.X])
	}
}

// save keeps the content of r for a later restore (disposal 3).
func (c *gifCanvas) save(r image.Rectangle) {
	c.saved = c.saved[:0]
	for y := r.Min.Y; y < r.Max.Y; y++ {
		c.saved = append(c.saved, c.cur[y*c.w+r.Min.X:y*c.w+r.Max.X]...)
	}
}

// restore puts back what save stored for the same rectangle.
func (c *gifCanvas) restore(r image.Rectangle) {
	for i, y := 0, r.Min.Y; y < r.Max.Y; i, y = i+r.Dx(), y+1 {
		copy(c.cur[y*c.w+r.Min.X:y*c.w+r.Max.X], c.saved[i:i+r.Dx()])
	}
}

// syncPrev copies r from cur to prev and reports whether it was equal
// already.
func (c *gifCanvas) syncPrev(r image.Rectangle) bool {
	same := true
	for y := r.Min.Y; y < r.Max.Y; y++ {
		a, b := c.cur[y*c.w+r.Min.X:y*c.w+r.Max.X], c.prev[y*c.w+r.Min.X:y*c.w+r.Max.X]
		if !slices.Equal(a, b) {
			same = false
			copy(b, a)
		}
	}
	return same
}

// sameDisposalResult reports whether D[k] == D[k-1] for a no-op frame k
// (cur == P[k] == P[k-1]). For disposals 0/1/2 the two differ exactly where
// one frame clears and the other does not, so they are equal iff that area
// is 0 already. When either frame has disposal 3 the two only count as equal
// when both have it over the same rectangle (the restore then puts back the
// same pixels, because the no-op left the canvas as it was); any other pair
// is not provably equal and counts as different. Reserved disposals never
// get here: they end the walk.
func (c *gifCanvas) sameDisposalResult(ctl, pred frameCtl) bool {
	if ctl.disposal == 3 || pred.disposal == 3 {
		return ctl.disposal == pred.disposal && ctl.rect == pred.rect
	}
	var a, b image.Rectangle // cleared rectangles
	if ctl.disposal == 2 {
		a = ctl.rect
	}
	if pred.disposal == 2 {
		b = pred.rect
	}
	return c.zeroOutside(a, b) && c.zeroOutside(b, a)
}

// zeroOutside reports whether every pixel of r that is not in not is 0.
func (c *gifCanvas) zeroOutside(r, not image.Rectangle) bool {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		row := c.cur[y*c.w : (y+1)*c.w]
		skip := y >= not.Min.Y && y < not.Max.Y
		for x := r.Min.X; x < r.Max.X; x++ {
			if skip && x >= not.Min.X && x < not.Max.X {
				x = not.Max.X - 1
				continue
			}
			if row[x] != 0 {
				return false
			}
		}
	}
	return true
}

// palette returns the canvas values of img's colour table. Indices beyond
// the table get a value no real colour has, distinct per index.
func (c *gifCanvas) palette(img *gifImage) *[256]uint32 {
	fill := func(pal *[256]uint32, table []byte) {
		n := len(table) / 3
		for i := range pal {
			if i < n {
				pal[i] = 0xFF000000 | uint32(table[3*i])<<16 | uint32(table[3*i+1])<<8 | uint32(table[3*i+2])
			} else {
				pal[i] = 0xFE000000 | uint32(i)
			}
		}
	}
	if img.hasLCT() {
		fill(&c.lctPal, img.lct)
		return &c.lctPal
	}
	if !c.gctReady {
		fill(&c.gctPal, c.g.gct)
		c.gctReady = true
	}
	return &c.gctPal
}

// GIF interlace passes: first row and row step.
var (
	interlaceStart = [4]int{0, 4, 2, 1}
	interlaceStep  = [4]int{8, 8, 4, 2}
)

// draw decodes img's LZW data and composites it onto cur. Every pixel of the
// frame must decode (as in decodeIndices); on error cur is left partly drawn
// and the walk must stop.
func (c *gifCanvas) draw(img *gifImage, ctl frameCtl) error {
	fw, fh := int(img.width), int(img.height)
	n := fw * fh
	if n == 0 {
		return nil
	}
	if n > maxAnalysedPixels {
		return fmt.Errorf("frame of %dx%d exceeds analysis cap", fw, fh)
	}
	if c.budget -= int64(n); c.budget < 0 {
		return fmt.Errorf("more than %d pixels to decode, over the analysis cap", maxWalkPixels)
	}
	if img.minCodeSize < 2 || img.minCodeSize > 8 {
		return fmt.Errorf("LZW minimum code size %d out of range", img.minCodeSize)
	}
	pal := c.palette(img)
	c.src = subBlockReader{blocks: img.data}
	if c.lzw == nil {
		c.lzw = lzw.NewReader(&c.src, lzw.LSB, int(img.minCodeSize)).(*lzw.Reader)
	} else {
		c.lzw.Reset(&c.src, lzw.LSB, int(img.minCodeSize))
	}

	left, top := ctl.rect.Min.X, ctl.rect.Min.Y
	perRead := max(1, (32<<10)/fw)
	if need := perRead * fw; cap(c.rows) < need {
		c.rows = make([]byte, need)
	}
	pass, y := 0, 0 // y is the frame row the next decoded row belongs to
	for done := 0; done < fh; {
		count := min(perRead, fh-done)
		buf := c.rows[:count*fw]
		if got, err := io.ReadFull(c.lzw, buf); err != nil {
			return fmt.Errorf("LZW data: %w (%d of %d pixels)", err, done*fw+got, n)
		}
		for i := 0; i < count; i++ {
			dst := c.cur[(top+y)*c.w+left : (top+y)*c.w+left+fw]
			src := buf[i*fw : (i+1)*fw]
			if ctl.transparent {
				for x, v := range src {
					if v != ctl.transIndex {
						dst[x] = pal[v]
					}
				}
			} else {
				for x, v := range src {
					dst[x] = pal[v]
				}
			}
			if !img.interlaced() {
				y++
				continue
			}
			for y += interlaceStep[pass]; y >= fh && pass < 3; {
				pass++
				y = interlaceStart[pass]
			}
		}
		done += count
	}
	return nil
}

// subBlockReader reads the concatenation of LZW data sub-blocks without
// joining them. compress/lzw only uses ReadByte.
type subBlockReader struct {
	blocks [][]byte
	i, off int
}

func (r *subBlockReader) ReadByte() (byte, error) {
	for r.i < len(r.blocks) {
		if b := r.blocks[r.i]; r.off < len(b) {
			r.off++
			return b[r.off-1], nil
		}
		r.i, r.off = r.i+1, 0
	}
	return 0, io.EOF
}

func (r *subBlockReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		b, err := r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		p[n] = b
		n++
	}
	return n, nil
}

// ruleNoopFrameDisposal: Discord's media pipeline drops a frame that does
// not change the composited picture and folds its delay into the previous
// frame — and the dropped frame's disposal is lost with it (user-verified
// 2026-09-19). gifsicle's optimiser writes exactly such frames after a hold:
// the run of identical frames becomes one disposal-1 frame plus a short
// disposal-2 frame (all-transparent under -O2, a redraw under -O1) whose
// only job is to clear the area; without it the next pose stacks on the old
// one. Harmless no-ops (same disposal result, or the last frame) pass. Not
// fixable at block level: the clear has to move onto a frame that survives,
// which needs a re-encode (MergeGIFHolds before optimising avoids the
// pattern). An error for Discord targets, a warning otherwise (spec
// decoders render the file correctly).
//
// The rule only judges what walkGIFCanvas analysed. When the walk ends early
// (a frame outside the logical screen, a reserved disposal, undecodable LZW
// data, a frame over maxAnalysedPixels, the maxWalkPixels budget) the unsafe
// frames found before that point still fail the rule; with none found the
// check PASSES, and its detail says from which frame on the file was not
// analysed and why — a pass with that note means "nothing found in the
// analysed part", not "safe". The same holds when the canvas cap
// (maxCanvasPixels) voids the whole walk. Known limitation, documented rather
// than turned into a failure: the caps are far beyond any Discord upload,
// gifsicle -O / -U clip out-of-screen frames (so pipeline output has none),
// and a reserved disposal already fails gif.disposal at error level.
func (l *gifLinter) ruleNoopFrameDisposal() {
	const rule = RuleGIFNoopFrameDisposal
	level := LevelWarn
	if IsDiscord(l.target) {
		level = LevelError
	}
	frames, _ := l.g.frames()
	if len(frames) < 2 {
		l.checks.pass(rule, level, "fewer than two frames")
		return
	}
	verdicts, err := walkGIFCanvas(l.g, frames)
	var unsafeFrames []int
	harmless := 0
	for k, v := range verdicts {
		switch v {
		case frameNoopUnsafe:
			unsafeFrames = append(unsafeFrames, k)
		case frameNoopHarmless:
			harmless++
		}
	}
	// note says what was not analysed; analysed names the part that was.
	note, analysed := "", ""
	switch {
	case err != nil && len(verdicts) == 0:
		note = fmt.Sprintf("not analysed: %v", err)
	case err != nil:
		note = fmt.Sprintf("not analysed from frame %d on: %v", len(verdicts), err)
		analysed = "frame 0"
		if len(verdicts) > 1 {
			analysed = fmt.Sprintf("frames 0-%d", len(verdicts)-1)
		}
	}
	if len(unsafeFrames) == 0 {
		detail := fmt.Sprintf("no frame that leaves the picture unchanged alters what is cleared (%s)", plural(harmless, "harmless hold frame"))
		if note != "" {
			detail = note
			if analysed != "" {
				detail += fmt.Sprintf("; nothing found in %s, the rest is unchecked", analysed)
			}
		}
		l.checks.pass(rule, level, detail)
		return
	}
	detail := fmt.Sprintf("%s: the picture stays as the previous frame left it, but the disposal clears a different area — "+
		"Discord drops frames that do not change the picture, and their disposal with them, so the old pixels are never cleared and the following frames stack on top of them; "+
		"needs a re-encode (merge held frames before optimising)", frameList(unsafeFrames))
	if note != "" {
		detail += "; " + note
	}
	l.checks.fail(rule, level, detail)
}

// MergeGIFHolds drops every harmless no-op frame — a frame k >= 1 that
// leaves the composited picture as frame k-1 left it and whose disposal
// leaves the same canvas as k-1's would, or any no-op last frame — and adds
// its delay to the surviving predecessor's Graphic Control Extension. The
// animation is unchanged for a spec decoder; what changes is that the
// optimiser run afterwards (gifsicle) no longer sees a run of identical
// frames and so does not manufacture the clear-only frame that
// gif.noop-frame-disposal reports.
//
// What is never merged:
//
//   - frame 0;
//   - a frame whose surviving predecessor has no GCE to carry the delay, a
//     malformed one (block shorter than the 4 data bytes that hold the
//     delay) or one with the user-input flag;
//   - a frame that has the user-input flag itself;
//   - a frame whose delay would take the predecessor's past 65535 cs (the
//     frame stays, and later holds fold into it instead);
//   - an unsafe no-op (its disposal differs from its predecessor's) while
//     any frame that survives comes after it. The tail step is the
//     exception: once the frames behind it are gone, an unsafe no-op is the
//     last frame, where the disposal stops mattering (the canvas is reset
//     when the loop restarts), so it IS dropped too, and so on backwards
//     until a frame that changes the picture or a guard above ends it;
//   - the first frame that could not be analysed and everything after it:
//     a frame outside the logical screen, a reserved disposal (4-7),
//     undecodable LZW data, a frame over maxAnalysedPixels or the
//     maxWalkPixels budget. Harmless holds BEFORE that frame are still
//     merged, but the tail step only runs when every frame was analysed
//     (otherwise the last analysed frame is not the last frame). When the
//     frames span more than maxCanvasPixels nothing is analysed and nothing
//     merged.
//
// Delays are summed as written: a 0 or 1 cs delay that browsers would
// stretch to 100 ms adds 0 or 1 cs. A dropped frame takes its GCE block(s)
// with it; every other byte round-trips exactly. When nothing was merged the
// result is data itself and merged is 0. err is only returned for
// unparseable input.
func MergeGIFHolds(data []byte) (out []byte, merged int, err error) {
	g, err := parseGIF(data)
	if err != nil {
		return nil, 0, fmt.Errorf("discordlint: %w", err)
	}
	frames, _ := g.frames()
	verdicts, _ := walkGIFCanvas(g, frames) // a partial analysis still merges what it covered
	var drop []int
	// merge folds frame k into the surviving frame `into` unless a guard
	// forbids it.
	merge := func(k, into int) bool {
		f, gce := &frames[k], frames[into].gce
		if !mergeableGCE(gce) || (f.gce != nil && f.gce.userInput) {
			return false
		}
		sum := int(gce.delayCS)
		if f.gce != nil {
			sum += int(f.gce.delayCS)
		}
		if sum > 0xFFFF {
			return false
		}
		gce.patchDelay(uint16(sum))
		drop = append(drop, f.imageBlock)
		if f.gce != nil {
			drop = append(append(drop, f.gceBlock), f.dupGCE...)
		}
		merged++
		return true
	}
	// A dropped frame leaves P and D as its predecessor had them, so the
	// verdicts (each relative to the original predecessor) stay valid for
	// the survivors and chains of holds collapse in one pass.
	kept := make([]int, 0, len(verdicts))
	for k, v := range verdicts {
		if k == 0 || v != frameNoopHarmless || !merge(k, kept[len(kept)-1]) {
			kept = append(kept, k)
		}
	}
	// Tail step: dropping the tail can turn an unsafe no-op into the last
	// frame, whose disposal no longer matters. Only when every frame was
	// analysed — after a partial walk kept's last entry is not the file's
	// last frame.
	for len(verdicts) == len(frames) && len(kept) >= 2 {
		last := kept[len(kept)-1]
		if verdicts[last] == frameChanges || !merge(last, kept[len(kept)-2]) {
			break
		}
		kept = kept[:len(kept)-1]
	}
	if merged == 0 {
		return data, 0, nil
	}
	g.removeBlocks(drop)
	out = g.encode()
	if !g.hasTrailer {
		out = out[:len(out)-1] // encode always writes one; keep the input's shape
	}
	return out, merged, nil
}

// mergeableGCE reports whether gce can take over a dropped frame's delay: it
// exists, is well formed (so its delay bytes sit where patchDelay expects
// them) and does not wait for user input.
func mergeableGCE(gce *gifGCE) bool {
	if gce == nil || gce.userInput {
		return false
	}
	return gce.raw == nil || (len(gce.raw) >= 8 && gce.raw[2] >= 4)
}

// patchDelay sets the delay without giving up the block's original bytes
// (setDelay re-encodes canonically, which would also normalise an oversized
// block). raw aliases the input, so it is copied first.
func (b *gifGCE) patchDelay(cs uint16) {
	b.delayCS = cs
	if b.raw == nil {
		return
	}
	raw := append([]byte(nil), b.raw...)
	raw[4], raw[5] = byte(cs), byte(cs>>8)
	b.raw = raw
}
