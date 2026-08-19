package discordlint

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"testing"
)

// Synthetic GIF fixtures built with image/gif.EncodeAll (an independent
// encoder) plus targeted byte surgery. The base palette has 8 entries and
// index 5 is opaque black; the "transparent" palette variant is identical
// except entry 5 has alpha 0. Both serialise to the same RGB bytes, so every
// frame shares the single global colour table regardless of which variant
// it uses.

const (
	fxW, fxH  = 16, 16
	fxTransIx = 5 // index that is transparent in the transparent palette
)

var fxPalette = color.Palette{
	color.RGBA{0, 0, 0, 255},       // 0 black
	color.RGBA{255, 255, 255, 255}, // 1 white
	color.RGBA{255, 0, 0, 255},     // 2 red
	color.RGBA{0, 255, 0, 255},     // 3 green
	color.RGBA{0, 0, 255, 255},     // 4 blue
	color.RGBA{0, 0, 0, 255},       // 5 black (transparent in fxPaletteT)
	color.RGBA{255, 255, 0, 255},   // 6 yellow
	color.RGBA{0, 255, 255, 255},   // 7 cyan
}

var fxPaletteT = func() color.Palette {
	p := append(color.Palette(nil), fxPalette...)
	p[fxTransIx] = color.RGBA{}
	return p
}()

// fxFrame describes one synthetic frame.
type fxFrame struct {
	rect        image.Rectangle
	fill        byte // index for every pixel; transRect (if non-empty) is filled with fxTransIx
	transRect   image.Rectangle
	transparent bool // use fxPaletteT (GCE transparency flag, index 5)
	delay       int  // centiseconds
	disposal    byte
}

// fxAnim describes a synthetic animation.
type fxAnim struct {
	frames []fxFrame
	loop   int // image/gif LoopCount: 0 = forever, -1 = no NETSCAPE block
	bg     byte
}

// opaqueAnim is a well-formed 3-frame opaque animation (all rules pass).
func opaqueAnim() fxAnim {
	return fxAnim{
		loop: 0,
		frames: []fxFrame{
			{rect: image.Rect(0, 0, fxW, fxH), fill: 2, delay: 10, disposal: 1},
			{rect: image.Rect(4, 4, 12, 12), fill: 3, delay: 10, disposal: 1},
			{rect: image.Rect(0, 0, fxW, fxH), fill: 4, delay: 10, disposal: 2},
		},
	}
}

// alphaAnim is a well-formed transparent animation: every frame carries the
// transparency flag with index 5 and uses it, frame 0 included; bg = 5.
func alphaAnim() fxAnim {
	a := opaqueAnim()
	a.bg = fxTransIx
	for i := range a.frames {
		a.frames[i].transparent = true
		a.frames[i].transRect = image.Rect(1, 1, 3, 3).Add(a.frames[i].rect.Min)
	}
	return a
}

// opaqueFrame0Anim has an opaque frame 0 followed by transparent frames —
// lilliput's classic black-background case.
func opaqueFrame0Anim() fxAnim {
	a := alphaAnim()
	a.bg = 0
	a.frames[0].transparent = false
	a.frames[0].transRect = image.Rectangle{}
	return a
}

// encodeFx renders the animation with image/gif.
func encodeFx(t testing.TB, a fxAnim) []byte {
	t.Helper()
	g := &gif.GIF{
		Config:          image.Config{ColorModel: fxPalette, Width: fxW, Height: fxH},
		LoopCount:       a.loop,
		BackgroundIndex: a.bg,
	}
	for _, f := range a.frames {
		pal := fxPalette
		if f.transparent {
			pal = fxPaletteT
		}
		pm := image.NewPaletted(f.rect, pal)
		for i := range pm.Pix {
			pm.Pix[i] = f.fill
		}
		for y := f.transRect.Min.Y; y < f.transRect.Max.Y; y++ {
			for x := f.transRect.Min.X; x < f.transRect.Max.X; x++ {
				pm.SetColorIndex(x, y, fxTransIx)
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
	return buf.Bytes()
}

// fxHeaderLen is the length of header + LSD + the 8-entry GCT.
const fxHeaderLen = 13 + 3*8

// insertAfterGCT splices raw bytes right after the global colour table.
func insertAfterGCT(data, ext []byte) []byte {
	out := append([]byte(nil), data[:fxHeaderLen]...)
	out = append(out, ext...)
	return append(out, data[fxHeaderLen:]...)
}

// patchNetscapeLoop rewrites the loop count of the NETSCAPE2.0 block found
// by byte search.
func patchNetscapeLoop(t *testing.T, data []byte, n uint16) []byte {
	t.Helper()
	i := bytes.Index(data, []byte("NETSCAPE2.0"))
	if i < 0 {
		t.Fatal("fixture has no NETSCAPE2.0 block")
	}
	out := append([]byte(nil), data...)
	// "NETSCAPE2.0" (11) then 0x03 0x01 lo hi
	binary.LittleEndian.PutUint16(out[i+11+2:], n)
	return out
}

// mutateGIF parses data, applies fn to the block model and re-encodes it.
func mutateGIF(t *testing.T, data []byte, fn func(g *gifFile)) []byte {
	t.Helper()
	g, err := parseGIF(data)
	if err != nil {
		t.Fatalf("parseGIF: %v", err)
	}
	fn(g)
	return g.encode()
}

// frameGCEs returns the effective GCE of every frame (nil where absent).
func frameGCEs(t *testing.T, data []byte) []*gifGCE {
	t.Helper()
	g, err := parseGIF(data)
	if err != nil {
		t.Fatalf("parseGIF: %v", err)
	}
	frames, _ := g.frames()
	out := make([]*gifGCE, len(frames))
	for i, f := range frames {
		out[i] = f.gce
	}
	return out
}

// decodeGIF decodes with image/gif (an independent decoder) and fails the
// test when it cannot.
func decodeGIF(t *testing.T, data []byte) *gif.GIF {
	t.Helper()
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image/gif rejected the file: %v", err)
	}
	return g
}

// samePixels asserts that image/gif decodes the same frame rectangles and
// pixel indices from a and b (the LZW data was not touched).
func samePixels(t *testing.T, a, b []byte) {
	t.Helper()
	ga, gb := decodeGIF(t, a), decodeGIF(t, b)
	if len(ga.Image) != len(gb.Image) {
		t.Fatalf("frame count changed: %d -> %d", len(ga.Image), len(gb.Image))
	}
	for i := range ga.Image {
		if ga.Image[i].Rect != gb.Image[i].Rect {
			t.Errorf("frame %d rect changed: %v -> %v", i, ga.Image[i].Rect, gb.Image[i].Rect)
		}
		if !bytes.Equal(ga.Image[i].Pix, gb.Image[i].Pix) {
			t.Errorf("frame %d pixel indices changed", i)
		}
	}
}

// readFixture loads a testdata file.
func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// findCheck returns the check for rule (and fails the test if absent).
func findCheck(t *testing.T, r Report, rule string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Rule == rule {
			return c
		}
	}
	t.Fatalf("report has no check %q; got %v", rule, ruleIDs(r))
	return Check{}
}

func hasCheck(r Report, rule string) bool {
	for _, c := range r.Checks {
		if c.Rule == rule {
			return true
		}
	}
	return false
}

func ruleIDs(r Report) []string {
	ids := make([]string, len(r.Checks))
	for i, c := range r.Checks {
		ids[i] = c.Rule
	}
	return ids
}

// expectCheck asserts the OK/Fixed state of one rule.
func expectCheck(t *testing.T, r Report, rule string, ok, fixed bool) Check {
	t.Helper()
	c := findCheck(t, r, rule)
	if c.OK != ok || c.Fixed != fixed {
		t.Errorf("%s: got ok=%v fixed=%v (%s), want ok=%v fixed=%v", rule, c.OK, c.Fixed, c.Detail, ok, fixed)
	}
	return c
}

// expectClean asserts that no check is failed or fixed (a fully compliant
// file) — used to re-lint fixed output.
func expectClean(t *testing.T, r Report) {
	t.Helper()
	for _, c := range r.Checks {
		if !c.OK || c.Fixed {
			t.Errorf("re-lint of fixed bytes: %s ok=%v fixed=%v (%s)", c.Rule, c.OK, c.Fixed, c.Detail)
		}
	}
	if !r.OK {
		t.Error("re-lint of fixed bytes: report not OK")
	}
}
