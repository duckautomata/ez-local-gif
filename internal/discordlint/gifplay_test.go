package discordlint

import (
	"bytes"
	"errors"
	"image/color"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// playSprite / playOpaque are the two pictures of the play tests: a sprite on
// the transparent background (index 5) and a fully opaque frame.
func playSprite(x, y int) byte { return either(x >= 4 && x < 10 && y >= 4 && y < 10, 3, 5) }

func TestSameGIFAnimation(t *testing.T) {
	// One animation, three structures: complete frames with disposal 2, the
	// same with a hold merged, and a delta version (the sprite is erased by a
	// disposal-2 frame, then redrawn elsewhere as a sub-rectangle).
	moved := func(x, y int) byte { return either(x >= 8 && x < 14 && y >= 4 && y < 10, 3, 5) }
	full := encodeCv(t, []cvFrame{
		{rect: cvFull, pix: playSprite, trans: ti(5), delay: 10, disposal: 2},
		{rect: cvFull, pix: playSprite, trans: ti(5), delay: 10, disposal: 2},
		{rect: cvFull, pix: moved, trans: ti(5), delay: 30, disposal: 2},
	})
	merged, n, err := MergeGIFHolds(full)
	if err != nil || n != 1 {
		t.Fatalf("fixture: merged %d, err %v", n, err)
	}
	spriteRect, movedRect := cvFull, cvFull
	spriteRect.Min.X, spriteRect.Min.Y, spriteRect.Max.X, spriteRect.Max.Y = 4, 4, 10, 10
	movedRect.Min.X, movedRect.Min.Y, movedRect.Max.X, movedRect.Max.Y = 8, 4, 14, 10
	delta := encodeCv(t, []cvFrame{
		{rect: spriteRect, fill: 3, trans: ti(5), delay: 20, disposal: 2},
		{rect: movedRect, fill: 3, delay: 30, disposal: 1},
	})
	// …and one with the colours in a local table in another order.
	local := encodeCv(t, []cvFrame{
		{rect: cvFull, pix: func(x, y int) byte { return either(playSprite(x, y) == 3, 1, 0) }, trans: ti(0), delay: 20, disposal: 2,
			local: color.Palette{color.RGBA{}, fxPalette[3]}},
		{rect: cvFull, pix: moved, trans: ti(5), delay: 30, disposal: 2},
	})
	for name, other := range map[string][]byte{"itself": full, "holds merged": merged, "delta frames": delta, "local table, other index order": local} {
		if same, detail, err := SameGIFAnimation(full, other); err != nil || !same {
			t.Errorf("%s: same=%v detail=%q err=%v, want the same animation", name, same, detail, err)
		}
	}

	// What gifsicle -U does to a clip whose first frame is opaque: the later
	// frames lose their transparency (here: the background becomes colour 4).
	mixed := []cvFrame{
		{rect: cvFull, fill: 4, delay: 10, disposal: 2},
		{rect: cvFull, pix: playSprite, trans: ti(5), delay: 10, disposal: 2},
	}
	flattened := []cvFrame{
		{rect: cvFull, fill: 4, delay: 10, disposal: 1},
		{rect: cvFull, pix: func(x, y int) byte { return either(playSprite(x, y) == 3, 3, 4) }, delay: 10, disposal: 1},
	}
	recoloured := []cvFrame{mixed[0], mixed[1]}
	recoloured[0].fill = 3
	resprited := []cvFrame{mixed[0], mixed[1]}
	resprited[1].pix = func(x, y int) byte { return either(playSprite(x, y) == 3, 6, 5) }
	slower := []cvFrame{mixed[0], mixed[1]}
	slower[1].delay = 11
	longer := append([]cvFrame{}, mixed...)
	longer = append(longer, cvFrame{rect: cvFull, fill: 3, delay: 10, disposal: 2})
	for name, c := range map[string]struct {
		other []cvFrame
		want  string
	}{
		"transparency lost": {flattened, "background pixels"},
		"another colour":    {recoloured, "differs in colour"},
		"a recoloured sprite on the transparent picture": {resprited, "differs in colour"},
		"another delay":    {slower, "shown for 10 cs vs 11 cs"},
		"one picture more": {longer, "2 vs 3 distinct pictures"},
	} {
		same, detail, err := SameGIFAnimation(encodeCv(t, mixed), encodeCv(t, c.other))
		if err != nil || same || !strings.Contains(detail, c.want) {
			t.Errorf("%s: same=%v detail=%q err=%v, want a difference mentioning %q", name, same, detail, err, c.want)
		}
	}

	// A file that cannot be played cannot be judged.
	reserved := append([]cvFrame{}, mixed...)
	reserved[1].disposal = 4
	if _, _, err := SameGIFAnimation(encodeCv(t, mixed), encodeCv(t, reserved)); err == nil || !strings.Contains(err.Error(), "second file") {
		t.Errorf("reserved disposal: err = %v, want an error naming the second file", err)
	}
	if _, _, err := SameGIFAnimation([]byte("GIF89a nope"), full); err == nil {
		t.Error("unparseable first file: want an error")
	}
}

func TestGIFPlaybackShowsBackground(t *testing.T) {
	T := cvFrame{rect: cvFull, pix: playSprite, trans: ti(5), delay: 4, disposal: 2}
	O := cvFrame{rect: cvFull, fill: 4, delay: 4, disposal: 2}
	keep := cvFrame{rect: cvFull, fill: 4, delay: 4, disposal: 1}
	small := cvFrame{rect: cvRectB, fill: 4, delay: 4, disposal: 1} // opaque, but does not cover the screen
	solid := cvFrame{rect: cvRectB, fill: 3, delay: 4, disposal: 1} // a solid sprite: no transparent pixel in its data
	for _, c := range []struct {
		name   string
		frames []cvFrame
		want   bool
	}{
		{"opaque first, transparent later", []cvFrame{O, T}, true},
		{"transparent first", []cvFrame{T, O}, true},
		{"opaque throughout", []cvFrame{keep, keep}, false},
		{"opaque throughout, cleared and fully repainted", []cvFrame{O, O}, false},
		{"first frame leaves the border uncovered", []cvFrame{small, keep}, true},
		// No frame uses a transparent index: the background only shows
		// because the full frame was disposed before the sprite is drawn.
		{"transparency from a disposal alone", []cvFrame{O, solid}, true},
		{"single opaque frame", []cvFrame{keep}, false},
	} {
		p, err := PlayGIF(encodeCv(t, c.frames))
		if err != nil || p.ShowsBackground() != c.want {
			t.Errorf("%s: ShowsBackground = %v (err %v), want %v", c.name, err == nil && p.ShowsBackground(), err, c.want)
		}
	}
	if _, err := PlayGIF([]byte("GIF89a nope")); err == nil {
		t.Error("unparseable input: want an error")
	}
}

func TestPrependTransparentFrame(t *testing.T) {
	in := encodeCv(t, []cvFrame{
		{rect: cvFull, fill: 4, delay: 10, disposal: 2},
		{rect: cvFull, pix: playSprite, trans: ti(5), delay: 20, disposal: 2},
	})
	out, err := PrependTransparentFrame(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in)+8+len(gifLeadInImage) {
		t.Errorf("%d bytes -> %d, want exactly one GCE and the 1x1 image more", len(in), len(out))
	}
	g, err := parseGIF(out)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := g.frames()
	if len(frames) != 3 {
		t.Fatalf("%d frames, want 3", len(frames))
	}
	f := frames[0]
	if f.image.width != 1 || f.image.height != 1 || f.image.left != 0 || f.image.top != 0 || f.image.hasLCT() ||
		f.gce == nil || !f.gce.transparent || f.gce.transIndex != 0 || f.gce.disposal != 2 || f.gce.delayCS != 0 {
		t.Errorf("lead-in frame = %+v gce %+v", f.image, f.gce)
	}
	if pix, err := decodeIndices(f.image); err != nil || pix.pixels != 1 || !pix.usesOnly(0) {
		t.Errorf("lead-in pixel data: %+v, err %v — want one pixel of index 0", pix, err)
	}
	// The extensions that preceded frame 0 (the NETSCAPE loop block) still do.
	if first := g.firstImageBlock(); first < 2 {
		t.Errorf("first image at block %d: the lead-in went in front of the loop block", first)
	} else if _, ok := g.blocks[0].(*gifAppExt); !ok {
		t.Errorf("block 0 is %T, want the NETSCAPE application extension", g.blocks[0])
	}
	// It draws nothing: an empty picture for 0 cs, then the animation as it was.
	was, err := PlayGIF(in)
	if err != nil {
		t.Fatal(err)
	}
	now, err := PlayGIF(out)
	if err != nil {
		t.Fatal(err)
	}
	if lead := now.shown[0]; lead.delayCS != 0 || lead.clear != fxW*fxH {
		t.Errorf("lead-in picture = %+v, want an entirely clear screen for 0 cs", lead)
	}
	now.shown = now.shown[1:]
	if same, detail := was.Same(now); !same {
		t.Errorf("animation after the lead-in changed: %s", detail)
	}
	// Go's decoder agrees it is a valid file.
	if dg := decodeGIF(t, out); len(dg.Image) != 3 || dg.Delay[0] != 0 {
		t.Errorf("image/gif: %d frames, first delay %d", len(dg.Image), dg.Delay[0])
	}

	// No trailer in, no trailer out; GIF87a is relabelled.
	cut, err := PrependTransparentFrame(in[:len(in)-1])
	if err != nil || !bytes.Equal(cut, out[:len(out)-1]) {
		t.Errorf("trailer-less input: err %v, equals the full result minus its trailer: %v", err, bytes.Equal(cut, out[:len(out)-1]))
	}
	old := append([]byte(nil), in...)
	copy(old, "GIF87a")
	if o, err := PrependTransparentFrame(old); err != nil || string(o[:6]) != "GIF89a" {
		t.Errorf("GIF87a input: err %v, header %q", err, o[:6])
	}

	// Refused: no global colour table, garbage.
	g2, _ := parseGIF(in)
	g2.lsdPacked &^= 0x80
	g2.gct = nil
	if _, err := PrependTransparentFrame(g2.encode()); err == nil {
		t.Error("no global colour table: want an error")
	}
	if _, err := PrependTransparentFrame([]byte("GIF89a nope")); err == nil {
		t.Error("unparseable input: want an error")
	}
}

// TestPlayGIFHeldFrames: a frame that repeats its predecessor's bytes with the
// same GCE semantics is a hold — no decode, no budget — except after a
// disposal-2 clear of a smaller area than the frame redraws over older
// content, where the same bytes give another picture. Both must play exactly
// like the slow path (walkGIFCanvas's reference compositor agrees).
func TestPlayGIFHeldFrames(t *testing.T) {
	hold := encodeCv(t, []cvFrame{
		{rect: cvFull, pix: playSprite, trans: ti(5), delay: 10, disposal: 2},
		{rect: cvFull, pix: playSprite, trans: ti(5), delay: 10, disposal: 2},
		{rect: cvFull, pix: playSprite, trans: ti(5), delay: 10, disposal: 2},
		{rect: cvFull, fill: 4, delay: 5, disposal: 1},
		{rect: cvFull, fill: 4, delay: 5, disposal: 1},
	})
	p, err := PlayGIF(hold)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.shown) != 2 || p.shown[0].delayCS != 30 || p.shown[1].delayCS != 10 || p.shown[1].clear != 0 {
		t.Errorf("held frames: %+v, want the sprite for 30 cs and the opaque picture for 10 cs", p.shown)
	}
	// The held frames cost nothing: a broken copy of a repeated frame's pixel
	// data would stop a player that decodes it. (Frame 1 repeats frame 0.)
	merged, n, err := MergeGIFHolds(hold)
	if err != nil || n != 3 {
		t.Fatalf("fixture: merged %d, err %v", n, err)
	}
	if same, detail, err := SameGIFAnimation(hold, merged); err != nil || !same {
		t.Errorf("hold vs merged: %q, err %v", detail, err)
	}

	// smallerClearThenRepeat (gifcanvas_test.go): frames 2 and 3 are
	// byte-identical disposal-2 frames, but frame 2 was drawn over content
	// that frame 1's smaller clear left behind — frame 3 shows less.
	tricky := encodeCv(t, smallerClearThenRepeat())
	p, err = PlayGIF(tricky)
	if err != nil {
		t.Fatal(err)
	}
	ref := refComposite(t, tricky)
	pictures := 1
	for k := 1; k < len(ref); k++ {
		if !slices.Equal(ref[k].p, ref[k-1].p) {
			pictures++
		}
	}
	if len(p.shown) != pictures {
		t.Errorf("%d pictures, the reference compositor shows %d: a repeated frame was taken for a hold", len(p.shown), pictures)
	}
}

func TestPlayGIFAnalysisCap(t *testing.T) {
	g, err := parseGIF(encodeCv(t, []cvFrame{{rect: cvFull, fill: 4, delay: 10, disposal: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	g.width, g.height = 5000, 5000 // 25 M pixels: over maxCanvasPixels
	if _, err := PlayGIF(g.encode()); !errors.Is(err, ErrAnalysisCap) {
		t.Errorf("huge logical screen: err = %v, want ErrAnalysisCap", err)
	}
	if _, err := PlayGIF([]byte("GIF89a nope")); err == nil || errors.Is(err, ErrAnalysisCap) {
		t.Errorf("unparseable input: err = %v, want an error that is not the analysis cap", err)
	}
}

// TestPlayGIFMatchesReference: on seeded random animations — with frames
// repeated byte for byte, so the held-frame fast path is taken and refused in
// every combination of disposals and rectangles — PlayGIF shows exactly what
// the independent image/gif compositor of gifcanvas_test.go shows: the same
// pictures for the same time.
func TestPlayGIFMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260920, 7))
	played, held := 0, 0
	for range 600 {
		anim := randomCvAnim(rng)
		// Repeat about a third of the frames right after themselves.
		var frames []cvFrame
		for _, f := range anim {
			frames = append(frames, f)
			for rng.IntN(3) == 0 {
				frames = append(frames, f)
				held++
			}
		}
		data := encodeCv(t, frames)
		p, err := PlayGIF(data)
		if err != nil {
			continue // reserved disposals, frames without a playable shape: nothing to compare
		}
		played++
		var want []gifShown
		for _, fr := range refComposite(t, data) {
			hash, clear := uint64(14695981039346656037), 0
			for _, v := range fr.p {
				if v == 0 {
					clear++
				}
				hash = (hash ^ uint64(v)) * 1099511628211
			}
			if n := len(want); n > 0 && want[n-1].hash == hash && want[n-1].clear == clear {
				want[n-1].delayCS += fr.delay
			} else {
				want = append(want, gifShown{hash, clear, fr.delay})
			}
		}
		if !slices.Equal(p.shown, want) {
			t.Fatalf("animation %d (%d frames): PlayGIF shows %+v, the reference %+v", played, len(frames), p.shown, want)
		}
	}
	if played < 300 || held < 300 {
		t.Errorf("only %d animations played, %d repeated frames: the generator no longer exercises the player", played, held)
	}
}
