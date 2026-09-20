package discordlint

import (
	"bytes"
	"errors"
	"fmt"
	"image"
)

// ErrAnalysisCap marks a file that is too large to play: a logical screen over
// maxCanvasPixels, or more than maxWalkPixels to decode. Such a file is not
// wrong, it just cannot be judged (errors.Is).
var ErrAnalysisCap = errors.New("over the analysis cap")

// Playing a GIF: the same per-spec compositing as walkGIFCanvas (gifcanvas.go:
// 0 = background / cleared, any drawn pixel an opaque colour value, disposal 2
// clears the frame's rectangle, 3 restores it), but on a canvas that spans the
// whole logical screen and with the picture handed out after every frame — so
// two files can be compared, which the walker's bounding-box canvas and
// byte-identical fast path do not allow.

// gifShown is one stretch of an animation: a picture and how long it stays.
type gifShown struct {
	hash    uint64 // of the composited canvas (colour values, not palette indices)
	clear   int    // pixels showing the background
	delayCS int
}

// playGIF composites the frames and calls show with the canvas after each
// frame was drawn (valid during the call only) — or with nil for a frame that
// provably repeats its predecessor's picture: the same image bytes drawn with
// the same GCE semantics (walkGIFCanvas's fast path, with the same condition
// for disposal 2). Held frames of a coalesced file are of that kind, so they
// cost neither a decode nor a share of the budget. It stops with an error at
// the first frame decoders disagree on or that cannot be decoded — a frame
// outside the logical screen, a reserved disposal, bad LZW data — and with
// ErrAnalysisCap for a screen over maxCanvasPixels or once the frames still to
// decode exceed maxWalkPixels (known up front from the image descriptors, so
// no time is spent on a file that cannot be finished).
func playGIF(g *gifFile, frames []gifFrame, show func(k int, canvas []uint32)) error {
	w, h := int(g.width), int(g.height)
	if int64(w)*int64(h) > maxCanvasPixels {
		return fmt.Errorf("logical screen of %dx%d pixels: %w", w, h, ErrAnalysisCap)
	}
	screen := image.Rect(0, 0, w, h)
	ctls := make([]frameCtl, len(frames))
	repeats := make([]bool, len(frames)) // byte-identical to the predecessor, same GCE semantics
	var toDecode int64
	for k, f := range frames {
		img := f.image
		ctl := frameCtl{rect: image.Rect(int(img.left), int(img.top), int(img.left)+int(img.width), int(img.top)+int(img.height))}
		if !ctl.rect.In(screen) {
			return fmt.Errorf("frame %d (%dx%d at %d,%d) exceeds the %dx%d logical screen", k, img.width, img.height, img.left, img.top, g.width, g.height)
		}
		if ctl.rect.Empty() {
			ctl.rect = image.Rectangle{}
		}
		if f.gce != nil {
			ctl.disposal, ctl.transparent, ctl.transIndex = f.gce.disposal, f.gce.transparent, f.gce.transIndex
		}
		if ctl.disposal > 3 {
			return fmt.Errorf("frame %d uses the reserved disposal %d, which decoders read differently", k, ctl.disposal)
		}
		ctls[k] = ctl
		if k > 0 && ctl.sameDrawing(ctls[k-1]) && bytes.Equal(img.raw, frames[k-1].image.raw) {
			repeats[k] = true
		} else {
			toDecode += int64(img.width) * int64(img.height)
		}
	}
	// A repeat with disposal 2 may still have to be decoded (predClean below),
	// so this is a lower bound: a file over it can never be finished.
	if toDecode > maxWalkPixels {
		return fmt.Errorf("%d pixels to decode: %w", toDecode, ErrAnalysisCap)
	}

	c := &gifCanvas{g: g, w: w, cur: make([]uint32, w*h), budget: maxWalkPixels}
	// pred is the last frame that was drawn; predClean says its rectangle was
	// entirely clear before it was drawn (frame 0, or its predecessor cleared
	// a superset of it) — only then does redrawing the same bytes after a
	// disposal-2 clear reproduce the same picture.
	var pred frameCtl
	predClean := false
	for k, f := range frames {
		ctl := ctls[k]
		if repeats[k] && (ctl.disposal != 2 || predClean) {
			show(k, nil)
			continue // pred and predClean describe this frame as well
		}
		if k > 0 {
			switch pred.disposal {
			case 2:
				c.clear(pred.rect)
			case 3:
				c.restore(pred.rect)
			}
		}
		if ctl.disposal == 3 {
			c.save(ctl.rect)
		}
		if err := c.draw(f.image, ctl); err != nil {
			if c.budget < 0 {
				return fmt.Errorf("frame %d: %w", k, ErrAnalysisCap)
			}
			return fmt.Errorf("frame %d: %w", k, err)
		}
		show(k, c.cur)
		predClean = k == 0 || (pred.disposal == 2 && ctl.rect.In(pred.rect))
		pred = ctl
	}
	return nil
}

// GIFPlayback is what a GIF shows in a spec decoder: one entry per stretch of
// identical pictures (consecutive frames that leave the picture unchanged are
// one stretch, their delays summed as written). It holds hashes, not pixels.
type GIFPlayback struct {
	shown         []gifShown
	width, height int
}

// PlayGIF plays data per the GIF spec. err means the file cannot be judged:
// unparseable, or one of playGIF's limits (a frame outside the logical
// screen, a reserved disposal, undecodable pixel data — or ErrAnalysisCap for
// a file that is merely too large).
func PlayGIF(data []byte) (*GIFPlayback, error) {
	g, err := parseGIF(data)
	if err != nil {
		return nil, fmt.Errorf("discordlint: %w", err)
	}
	frames, _ := g.frames()
	p := &GIFPlayback{width: int(g.width), height: int(g.height)}
	err = playGIF(g, frames, func(k int, canvas []uint32) {
		delay := 0
		if gce := frames[k].gce; gce != nil {
			delay = int(gce.delayCS)
		}
		if canvas == nil { // the predecessor's picture, held
			p.shown[len(p.shown)-1].delayCS += delay
			return
		}
		// FNV-1a over the colour values.
		hash, clear := uint64(14695981039346656037), 0
		for _, v := range canvas {
			if v == 0 {
				clear++
			}
			hash = (hash ^ uint64(v)) * 1099511628211
		}
		if n := len(p.shown); n > 0 && p.shown[n-1].hash == hash && p.shown[n-1].clear == clear {
			p.shown[n-1].delayCS += delay
			return
		}
		p.shown = append(p.shown, gifShown{hash, clear, delay})
	})
	if err != nil {
		return nil, fmt.Errorf("discordlint: %w", err)
	}
	return p, nil
}

// ShowsBackground reports whether any picture of the animation leaves a pixel
// of the logical screen showing the background (transparent in a browser):
// an uncovered border, a pixel drawn with the transparent index on a clear
// canvas, or an area a disposal cleared and nothing repainted.
func (p *GIFPlayback) ShowsBackground() bool {
	for _, s := range p.shown {
		if s.clear != 0 {
			return true
		}
	}
	return false
}

// Same reports whether q shows the same animation as p: the same pictures
// (colours and transparency — whatever the frame rectangles, disposals,
// palette order or number of frames that produce them) for the same time
// each. The last picture's duration is compared too; loop counts and whatever
// happens after the last frame are not. When they differ, detail says where.
func (p *GIFPlayback) Same(q *GIFPlayback) (same bool, detail string) {
	if p.width != q.width || p.height != q.height {
		return false, fmt.Sprintf("logical screen %dx%d vs %dx%d", p.width, p.height, q.width, q.height)
	}
	at := 0
	for i := 0; i < len(p.shown) && i < len(q.shown); i++ {
		switch x, y := p.shown[i], q.shown[i]; {
		case x.clear != y.clear:
			return false, fmt.Sprintf("picture %d (from %d cs on) differs: %d vs %d background pixels", i, at, x.clear, y.clear)
		case x.hash != y.hash:
			return false, fmt.Sprintf("picture %d (from %d cs on) differs in colour (the same %d background pixels)", i, at, x.clear)
		case x.delayCS != y.delayCS:
			return false, fmt.Sprintf("picture %d (from %d cs on) is shown for %d cs vs %d cs", i, at, x.delayCS, y.delayCS)
		}
		at += p.shown[i].delayCS
	}
	if len(p.shown) != len(q.shown) {
		return false, fmt.Sprintf("%d vs %d distinct pictures", len(p.shown), len(q.shown))
	}
	return true, ""
}

// SameGIFAnimation is PlayGIF + Same for two files. err means one of them
// cannot be judged — the caller decides what that means.
//
// jobs guards the hold repair with these: a gifsicle coalesce that changes the
// picture must never be delivered as a repair (gifsicle drops all transparency
// when the first frame has none, and with local colour tables or more than
// 256 colours per picture it gives up — "too complex to unoptimize", exit 0 —
// yet still rewrites the disposals).
func SameGIFAnimation(a, b []byte) (same bool, detail string, err error) {
	pa, err := PlayGIF(a)
	if err != nil {
		return false, "", fmt.Errorf("first file: %w", err)
	}
	pb, err := PlayGIF(b)
	if err != nil {
		return false, "", fmt.Errorf("second file: %w", err)
	}
	same, detail = pa.Same(pb)
	return same, detail, nil
}

// gifLeadInImage is a 1x1 image at 0,0 on the global colour table whose one
// pixel is index 0: descriptor, LZW minimum code size 2, the codes clear / 0 /
// end-of-information, block terminator.
var gifLeadInImage = []byte{gifImageIntro, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0x02, 0x02, 0x44, 0x01, 0x00}

// PrependTransparentFrame inserts a 1x1, zero-delay, entirely transparent
// frame (transparent index 0 of the global colour table, disposal 2) in front
// of the first frame — after the extensions that precede it, so a NETSCAPE
// loop block stays first. It draws nothing and changes no picture.
//
// It exists for gifsicle's unoptimiser, which decides from the FIRST frame
// whether the canvas is transparent at all: a first frame without the
// transparency flag, or one that does not use its transparent index, makes
// every coalesced frame opaque — the clip's later transparency is painted
// over with the background colour. With this lead-in (dropped again by the
// frame selection "#1-", enc.GifsicleOptions.SkipFirstFrame) the coalesce is
// exact, and byte-identical where it already was. The frame must use the
// global table: with a local one gifsicle refuses to unoptimise at all. A file
// without a global colour table is refused (index 0 would mean nothing); a
// GIF87a header is relabelled, as the frame needs a Graphic Control Extension.
func PrependTransparentFrame(data []byte) ([]byte, error) {
	g, err := parseGIF(data)
	if err != nil {
		return nil, fmt.Errorf("discordlint: %w", err)
	}
	if !g.hasGCT() {
		return nil, fmt.Errorf("discordlint: no global colour table to take the transparent index from")
	}
	frames, _ := g.frames()
	if len(frames) == 0 {
		return nil, fmt.Errorf("discordlint: file contains no image frames")
	}
	at := frames[0].imageBlock
	if frames[0].gce != nil {
		at = frames[0].gceBlock
		for _, d := range frames[0].dupGCE {
			at = min(at, d)
		}
	}
	gce := newGCE(0)
	gce.setDisposal(2)
	gce.setTransparent(true, 0)
	img, err := (&gifReader{data: gifLeadInImage}).image()
	if err != nil {
		return nil, fmt.Errorf("discordlint: lead-in frame: %w", err) // cannot happen
	}
	g.insertBlock(at, img)
	g.insertBlock(at, gce)
	if string(g.header[:]) == "GIF87a" {
		copy(g.header[:], "GIF89a")
	}
	out := g.encode()
	if !g.hasTrailer {
		out = out[:len(out)-1] // encode always writes one; keep the input's shape
	}
	return out, nil
}
