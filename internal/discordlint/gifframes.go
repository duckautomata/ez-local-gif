package discordlint

import "fmt"

// DisposeCompleteFrames makes a GIF whose every frame is a COMPLETE picture
// render as such: every Graphic Control Extension gets disposal 2 (restore to
// background) and frames without a transparency flag get the file's
// transparent index, so that each frame replaces the previous one instead of
// being drawn over it.
//
// It exists for ffmpeg's gif encoder on a clip that mixes fully opaque frames
// and frames with transparency (enc.GIFArgs with CompleteFrames: "-gifflags
// -offsetting-transdiff", every frame written full-canvas without inter-frame
// diffing — jobs encodes such a clip that way, see GIFNeedsCompleteFrames). The encoder
// still chooses the
// disposal per frame: a frame with a transparent pixel gets disposal 2, a
// fully opaque one disposal 1 and no transparency flag. In a clip that mixes
// the two, the opaque picture then stays on the canvas under the transparent
// frame that follows (FFmpeg 9.0.1 and the 2026-08 git build). With every
// frame disposed the clip is exact. The transparency flag on the opaque
// frames changes no pixel (it is only set when the frame's pixel data does
// not use the index) — it is there for decoders that clear a disposed frame to
// the opaque background colour unless the frame declares transparency
// (ffmpeg's own gif decoder, which the render's verify step runs).
//
// The caller asserts that every frame is a complete picture: a frame that
// relies on what its predecessor left behind (a delta, e.g. ffmpeg's default
// transdiff output) cannot be told apart from one that does not, and would
// lose its background — far worse than what this repairs. What can be checked
// is: the file is left untouched (the input slice comes back, patched == 0)
// unless every frame has exactly one canonical GCE, covers the whole
// logical screen, uses the global colour table and keeps to disposal 0, 1 or
// 2, and at least one frame declares transparency — an opaque animation needs
// nothing. Frames whose pixel data cannot be decoded, or uses the transparent
// index, keep their flag as it is. Every other byte round-trips exactly; err
// is only returned for unparseable input.
func DisposeCompleteFrames(data []byte) (out []byte, patched int, err error) {
	g, err := parseGIF(data)
	if err != nil {
		return nil, 0, fmt.Errorf("discordlint: %w", err)
	}
	frames, _ := g.frames()
	var count [256]int
	transIndex := -1
	for i := range frames {
		f := &frames[i]
		img := f.image
		if !canonicalGCE(f.gce) || len(f.dupGCE) > 0 || f.gce.disposal > 2 ||
			img.hasLCT() || img.left != 0 || img.top != 0 || img.width != g.width || img.height != g.height {
			return data, 0, nil
		}
		if f.gce.transparent {
			count[f.gce.transIndex]++
			if transIndex < 0 || count[f.gce.transIndex] > count[transIndex] {
				transIndex = int(f.gce.transIndex)
			}
		}
	}
	if transIndex < 0 {
		return data, 0, nil
	}
	var pix pixelCache
	for i := range frames {
		f := &frames[i]
		changed := false
		if f.gce.disposal != 2 {
			f.gce.setDisposal(2)
			changed = true
		}
		if !f.gce.transparent {
			if used, known := pix.usesIndex(f, byte(transIndex)); known && !used {
				f.gce.setTransparent(true, byte(transIndex))
				changed = true
			}
		}
		if changed {
			patched++
		}
	}
	if patched == 0 {
		return data, 0, nil
	}
	out = g.encode()
	if !g.hasTrailer {
		out = out[:len(out)-1] // encode always writes one; keep the input's shape
	}
	return out, patched, nil
}

// canonicalGCE reports whether gce exists and has the canonical 8-byte form
// (one 4-byte sub-block), so that re-encoding it from its fields — which is
// what setDisposal / setTransparent do — changes nothing but those fields.
func canonicalGCE(gce *gifGCE) bool {
	return gce != nil && (gce.raw == nil || (len(gce.raw) == 8 && gce.raw[2] == 4))
}

// GIFDisposals counts the frames of a GIF by disposal method (the index is
// the GCE's 3-bit value; a frame without a GCE counts as 0). It reads the
// block structure only (tests use it to check what an encode path wrote).
func GIFDisposals(data []byte) (counts [8]int, err error) {
	g, err := parseGIF(data)
	if err != nil {
		return counts, fmt.Errorf("discordlint: %w", err)
	}
	frames, _ := g.frames()
	for _, f := range frames {
		if f.gce == nil {
			counts[0]++
		} else {
			counts[f.gce.disposal&7]++
		}
	}
	return counts, nil
}

// GIFNeedsCompleteFrames reports whether ffmpeg's GIF of a master with
// transparency (enc.GIFArgs with HasAlpha) is one its encoder renders wrong,
// so that jobs must encode it again as complete frames (see
// DisposeCompleteFrames). The encoder marks the two kinds of frame itself, at
// the very size and alpha threshold it encodes: a frame with a transparent
// pixel gets disposal 2 and is written whole, a fully opaque one disposal 0/1
// and is diffed against the previous frame. The clip is wrong when an opaque
// frame is diffed against a disposed frame that drew something (its
// "unchanged" pixels become holes) or when a frame with transparency follows
// an opaque one (which stays on the canvas under it). That is every clip in
// which both kinds occur — except one: every disposal-2 frame precedes every
// opaque frame and the last of them draws nothing (an entirely transparent
// lead-in: nothing to punch holes with, nothing transparent afterwards).
// A frame whose pixels cannot be decoded counts as drawing something.
func GIFNeedsCompleteFrames(data []byte) (bool, error) {
	g, err := parseGIF(data)
	if err != nil {
		return false, fmt.Errorf("discordlint: %w", err)
	}
	frames, _ := g.frames()
	lastDisposed, firstKept := -1, -1
	for i, f := range frames {
		if f.gce != nil && f.gce.disposal == 2 {
			lastDisposed = i
		} else if firstKept < 0 {
			firstKept = i
		}
	}
	if lastDisposed < 0 || firstKept < 0 {
		return false, nil // one kind only
	}
	if lastDisposed > firstKept {
		return true, nil // a frame with transparency follows an opaque one
	}
	f := &frames[lastDisposed]
	if !f.gce.transparent {
		return true, nil
	}
	var pix pixelCache
	p, err := pix.get(f)
	return err != nil || !p.usesOnly(f.gce.transIndex), nil
}
