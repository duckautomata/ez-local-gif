package discordlint

import (
	"bytes"
	"image/color"
	"testing"
)

// disposeTestFrames is what ffmpeg's gif encoder writes for an alpha master
// with "-gifflags -offsetting-transdiff" when the clip mixes transparent and
// fully opaque frames: every frame full-canvas, disposal 2 + transparent index
// 5 on the frames with a transparent pixel, disposal 1 and no transparency
// flag on the fully opaque ones.
func disposeTestFrames() []cvFrame {
	sprite := func(x, y int) byte { return either(x >= 4 && x < 10 && y >= 4 && y < 10, 3, 5) }
	return []cvFrame{
		{rect: cvFull, pix: sprite, trans: ti(5), delay: 4, disposal: 2},
		{rect: cvFull, fill: 4, delay: 4, disposal: 1}, // fully opaque
		{rect: cvFull, fill: 4, delay: 4, disposal: 1},
		{rect: cvFull, pix: sprite, trans: ti(5), delay: 4, disposal: 2},
	}
}

func TestDisposeCompleteFrames(t *testing.T) {
	in := encodeCv(t, disposeTestFrames())
	out, patched, err := DisposeCompleteFrames(in)
	if err != nil {
		t.Fatal(err)
	}
	if patched != 2 {
		t.Errorf("patched = %d, want the 2 opaque frames", patched)
	}
	g, err := parseGIF(out)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := g.frames()
	for _, f := range frames {
		if f.gce.disposal != 2 || !f.gce.transparent || f.gce.transIndex != 5 || f.gce.delayCS != 4 {
			t.Errorf("frame %d: disposal %d transparent %v index %d delay %d, want 2 / true / 5 / 4", f.index, f.gce.disposal, f.gce.transparent, f.gce.transIndex, f.gce.delayCS)
		}
	}
	// Only GCE bytes changed: same length, and the image blocks are untouched.
	if len(out) != len(in) {
		t.Errorf("length %d -> %d: only GCE fields may change", len(in), len(out))
	}
	before, _ := parseGIF(in)
	bf, _ := before.frames()
	for i := range frames {
		if !bytes.Equal(frames[i].image.raw, bf[i].image.raw) {
			t.Errorf("frame %d: image bytes changed", i)
		}
	}
	// What it is for: the opaque picture no longer survives under the
	// transparent frame that follows it.
	shown, was := refComposite(t, out), refComposite(t, in)
	const corner = 0 // (0,0) is outside the sprite: transparent on frames 0 and 3
	if was[3].p[corner] == 0 {
		t.Fatalf("fixture: the unpatched clip already clears the opaque frame")
	}
	if shown[3].p[corner] != 0 {
		t.Errorf("frame 3 still shows the opaque frame's pixel %#x under its transparent area", shown[3].p[corner])
	}
	if shown[1].p[corner] == 0 || shown[0].p[corner] != 0 {
		t.Errorf("frames 0/1 changed their picture: corner %#x / %#x", shown[0].p[corner], shown[1].p[corner])
	}
	// Idempotent, and the input slice comes back when there is nothing to do.
	again, n, err := DisposeCompleteFrames(out)
	if err != nil || n != 0 || &again[0] != &out[0] {
		t.Errorf("second pass: patched %d, err %v, same slice %v", n, err, &again[0] == &out[0])
	}
}

func TestDisposeCompleteFramesLeavesOthersAlone(t *testing.T) {
	sub := disposeTestFrames()
	sub[2].rect = cvRectB // a delta frame: not a complete picture
	local := disposeTestFrames()
	local[1].local = color.Palette{color.RGBA{1, 2, 3, 255}, color.RGBA{9, 9, 9, 255}}
	local[1].fill = 1
	noGCE := disposeTestFrames()
	noGCE[1].noGCE = true
	restore := disposeTestFrames()
	restore[1].disposal = 3
	opaque := []cvFrame{{rect: cvFull, fill: 4, delay: 4, disposal: 1}, {rect: cvFull, fill: 3, delay: 4, disposal: 1}}
	for name, frames := range map[string][]cvFrame{
		"a frame smaller than the screen": sub, "a local colour table": local, "a frame without GCE": noGCE,
		"disposal 3": restore, "no transparent frame at all": opaque,
	} {
		in := encodeCv(t, frames)
		out, patched, err := DisposeCompleteFrames(in)
		if err != nil || patched != 0 || &out[0] != &in[0] {
			t.Errorf("%s: patched %d, err %v, same slice %v — want the input untouched", name, patched, err, &out[0] == &in[0])
		}
	}

	// An opaque frame whose pixels use the transparent index keeps its flag
	// off (the flag would punch holes), but is still disposed.
	uses := disposeTestFrames()
	uses[1].fill = 5
	out, patched, err := DisposeCompleteFrames(encodeCv(t, uses))
	if err != nil || patched != 2 {
		t.Fatalf("patched %d, err %v", patched, err)
	}
	g, _ := parseGIF(out)
	frames, _ := g.frames()
	if f := frames[1].gce; f.disposal != 2 || f.transparent {
		t.Errorf("frame using the index: disposal %d transparent %v, want 2 / false", f.disposal, f.transparent)
	}
	if f := frames[2].gce; f.disposal != 2 || !f.transparent {
		t.Errorf("frame not using the index: disposal %d transparent %v, want 2 / true", f.disposal, f.transparent)
	}

	if _, _, err := DisposeCompleteFrames([]byte("GIF89a nope")); err == nil {
		t.Error("unparseable input: want an error")
	}
}

func TestGIFDisposals(t *testing.T) {
	frames := disposeTestFrames() // disposals 2, 1, 1, 2
	frames = append(frames, cvFrame{rect: cvFull, fill: 3, delay: 4, disposal: 3}, cvFrame{rect: cvFull, fill: 3, noGCE: true})
	got, err := GIFDisposals(encodeCv(t, frames))
	if err != nil {
		t.Fatal(err)
	}
	if want := [8]int{0: 1, 1: 2, 2: 2, 3: 1}; got != want {
		t.Errorf("GIFDisposals = %v, want %v", got, want)
	}
	if _, err := GIFDisposals([]byte("GIF89a nope")); err == nil {
		t.Error("unparseable input: want an error")
	}
}

func TestGIFNeedsCompleteFrames(t *testing.T) {
	sprite := func(x, y int) byte { return either(x >= 4 && x < 10 && y >= 4 && y < 10, 3, 5) }
	T := cvFrame{rect: cvFull, pix: sprite, trans: ti(5), delay: 4, disposal: 2} // draws a sprite
	E := cvFrame{rect: cvFull, fill: 5, trans: ti(5), delay: 4, disposal: 2}     // entirely transparent
	O := cvFrame{rect: cvFull, fill: 4, delay: 4, disposal: 1}                   // fully opaque
	O0 := cvFrame{rect: cvFull, fill: 4, delay: 4, disposal: 0}
	for _, c := range []struct {
		name   string
		frames []cvFrame
		want   bool
	}{
		{"every frame transparent", []cvFrame{T, E, T}, false},
		{"every frame opaque", []cvFrame{O, O0, O}, false},
		{"transparent lead-in, then opaque to the end", []cvFrame{E, E, O, O}, false},
		{"a drawn frame before the opaque ones (holes)", []cvFrame{E, T, O, O}, true},
		{"drawn, then empty, then opaque", []cvFrame{T, E, O, O0}, false},
		{"opaque then transparent (stays underneath)", []cvFrame{O, O, E}, true},
		{"opaque in the middle", []cvFrame{T, O, T}, true},
		{"lead-in, opaque, transparent again", []cvFrame{E, O, E}, true},
	} {
		got, err := GIFNeedsCompleteFrames(encodeCv(t, c.frames))
		if err != nil || got != c.want {
			t.Errorf("%s: %v (err %v), want %v", c.name, got, err, c.want)
		}
	}
	// An undecodable lead-in frame counts as drawing something.
	g, err := parseGIF(encodeCv(t, []cvFrame{E, O}))
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := g.frames()
	raw := append([]byte(nil), frames[0].image.raw...)
	frames[0].image.minCodeSize = 9
	raw[10] = 9 // LZW minimum code size, out of range
	frames[0].image.raw = raw
	if got, err := GIFNeedsCompleteFrames(g.encode()); err != nil || !got {
		t.Errorf("undecodable lead-in: %v (err %v), want true", got, err)
	}
	if _, err := GIFNeedsCompleteFrames([]byte("GIF89a nope")); err == nil {
		t.Error("unparseable input: want an error")
	}
}

// TestDisposeCompleteFramesContract pins the preconditions and the
// byte-exactness one by one (each case killed a surviving mutant).
func TestDisposeCompleteFramesContract(t *testing.T) {
	base := encodeCv(t, disposeTestFrames())
	reparse := func() (*gifFile, []gifFrame) {
		g, err := parseGIF(base)
		if err != nil {
			t.Fatal(err)
		}
		fr, _ := g.frames()
		return g, fr
	}
	untouched := func(name string, in []byte) {
		t.Helper()
		out, patched, err := DisposeCompleteFrames(in)
		if err != nil || patched != 0 || &out[0] != &in[0] {
			t.Errorf("%s: patched %d, err %v, same slice %v — want the input untouched", name, patched, err, &out[0] == &in[0])
		}
	}

	g, fr := reparse()
	dup := *fr[1].gce
	g.insertBlock(fr[1].gceBlock, &dup)
	untouched("two GCEs before one frame", g.encode())

	g, fr = reparse()
	c := fr[1].gce
	packed := c.reservedBits | c.disposal<<2
	fr[1].gce.raw = []byte{gifExtensionIntro, gifLabelGCE, 5, packed, byte(c.delayCS), byte(c.delayCS >> 8), c.transIndex, 0xAA, 0}
	untouched("a 5-byte GCE sub-block", g.encode())

	g, fr = reparse()
	raw := append([]byte(nil), fr[1].image.raw...)
	raw[1] = 1 // left = 1 at full size: sticks out of the screen
	fr[1].image.raw = raw
	untouched("a frame offset by one pixel", g.encode())

	narrow := disposeTestFrames()
	narrow[1].rect.Max.X--
	untouched("a frame one pixel narrower than the screen", encodeCv(t, narrow))

	// No trailer in, no trailer out — and nothing else differs from the
	// patched full file.
	full, _, err := DisposeCompleteFrames(base)
	if err != nil {
		t.Fatal(err)
	}
	cut, patched, err := DisposeCompleteFrames(base[:len(base)-1])
	if err != nil || patched != 2 || !bytes.Equal(cut, full[:len(full)-1]) {
		t.Errorf("trailer-less input: patched %d, err %v, equals the patched file minus its trailer: %v", patched, err, bytes.Equal(cut, full[:len(full)-1]))
	}

	// An opaque frame whose pixel data cannot be decoded is disposed but
	// keeps its flag off: nobody knows whether it uses the index.
	g, fr = reparse()
	raw = append([]byte(nil), fr[1].image.raw...)
	raw[10] = 9 // LZW minimum code size out of range
	fr[1].image.raw, fr[1].image.minCodeSize = raw, 9
	out, patched, err := DisposeCompleteFrames(g.encode())
	if err != nil || patched != 2 {
		t.Fatalf("undecodable frame: patched %d, err %v", patched, err)
	}
	og, _ := parseGIF(out)
	ofr, _ := og.frames()
	if f := ofr[1].gce; f.disposal != 2 || f.transparent {
		t.Errorf("undecodable frame: disposal %d transparent %v, want 2 / false", f.disposal, f.transparent)
	}

	// The opaque frames get the index MOST frames use, not the first seen.
	sprite := func(idx byte) func(x, y int) byte {
		return func(x, y int) byte { return either(x >= 4 && x < 10 && y >= 4 && y < 10, 3, idx) }
	}
	out, _, err = DisposeCompleteFrames(encodeCv(t, []cvFrame{
		{rect: cvFull, pix: sprite(6), trans: ti(6), delay: 4, disposal: 2},
		{rect: cvFull, pix: sprite(5), trans: ti(5), delay: 4, disposal: 2},
		{rect: cvFull, pix: sprite(5), trans: ti(5), delay: 4, disposal: 2},
		{rect: cvFull, fill: 4, delay: 4, disposal: 1},
	}))
	if err != nil {
		t.Fatal(err)
	}
	og, _ = parseGIF(out)
	ofr, _ = og.frames()
	if f := ofr[3].gce; !f.transparent || f.transIndex != 5 {
		t.Errorf("opaque frame: transparent %v index %d, want the majority index 5", f.transparent, f.transIndex)
	}
}
