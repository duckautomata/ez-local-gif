package discordlint

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math/rand/v2"
	"strings"
	"testing"
)

// lintFix runs LintGIF with fix=true and returns report + bytes.
func lintFix(t *testing.T, data []byte, target Target) (Report, []byte) {
	t.Helper()
	r, out, err := LintGIF(data, target, true)
	if err != nil {
		t.Fatalf("LintGIF: %v", err)
	}
	return r, out
}

// lintOnly runs LintGIF with fix=false and asserts the bytes are untouched.
func lintOnly(t *testing.T, data []byte, target Target) Report {
	t.Helper()
	r, out, err := LintGIF(data, target, false)
	if err != nil {
		t.Fatalf("LintGIF: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("fix=false must return the input bytes")
	}
	return r
}

// assertFixed is the common shape of a fixable-rule test: the rule fails
// with fix=false, is reported fixed with fix=true, the fixed bytes re-lint
// clean, decode with image/gif and keep every pixel.
func assertFixed(t *testing.T, data []byte, target Target, rule string) []byte {
	t.Helper()
	before := lintOnly(t, data, target)
	expectCheck(t, before, rule, false, false)

	after, out := lintFix(t, data, target)
	expectCheck(t, after, rule, true, true)
	if bytes.Equal(out, data) {
		t.Fatal("fix=true returned unchanged bytes although a fix was reported")
	}
	if int64(len(out)) != after.Bytes {
		t.Errorf("Report.Bytes %d != len(out) %d", after.Bytes, len(out))
	}
	relint, again := lintFix(t, out, target)
	expectClean(t, relint)
	if !bytes.Equal(again, out) {
		t.Error("re-linting the fixed bytes changed them again")
	}
	samePixels(t, data, out)
	return out
}

func TestLintGIFCompliantFixtures(t *testing.T) {
	cases := map[string]struct {
		data  []byte
		alpha bool
	}{
		"ff_alpha.gif": {readFixture(t, "ff_alpha.gif"), true},
		"syn_opaque":   {encodeFx(t, opaqueAnim()), false},
		"syn_alpha":    {encodeFx(t, alphaAnim()), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := tc.data
			r, out := lintFix(t, data, TargetEmote)
			expectClean(t, r)
			if !bytes.Equal(out, data) {
				t.Error("compliant file was rewritten")
			}
			if r.Format != "gif" || r.Target != TargetEmote || r.RulesVersion != RulesVersion || r.Limit != 262144 {
				t.Errorf("report header: %+v", r)
			}
			if !r.LoopForever {
				t.Error("LoopForever false")
			}
			if r.HasAlpha != tc.alpha {
				t.Errorf("HasAlpha = %v, want %v", r.HasAlpha, tc.alpha)
			}
			// Every rule is reported for a generic file (plus emote dims).
			want := []string{
				RuleGIFGCEEveryFrame, RuleGIFFrame0Transparent, RuleGIFLSDBackground, RuleGIFDisposal,
				RuleGIFNetscapeLoop, RuleGIFMinDelay, RuleGIFGlobalPalette, RuleGIFNoInterlace,
				RuleGIFNoExtraExtensions, RuleGIFFirstFrameVisible, RuleGIFTrailer, RuleGIFSizeLimit, RuleGIFEmoteDims,
			}
			if got := ruleIDs(r); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("rules = %v, want %v", got, want)
			}
		})
	}
}

func TestLintGIFReportFields(t *testing.T) {
	r, _ := lintFix(t, readFixture(t, "ff_alpha.gif"), TargetNone)
	if r.Width != 64 || r.Height != 64 || r.Frames != 10 || r.DurationMS != 1000 || r.MinDelayMS != 100 {
		t.Errorf("ff_alpha.gif: %+v", r)
	}
	if r.Limit != 0 || hasCheck(r, RuleGIFSizeLimit) || hasCheck(r, RuleGIFEmoteDims) || hasCheck(r, RuleGIFStickerDims) {
		t.Errorf("TargetNone must not carry a byte limit or target rules: %v", ruleIDs(r))
	}
	if r.Bytes != 7505 {
		t.Errorf("Bytes = %d", r.Bytes)
	}

	a := opaqueAnim()
	a.frames[1].delay = 4
	a.frames[2].delay = 25
	r, _ = lintFix(t, encodeFx(t, a), TargetNone)
	if r.Frames != 3 || r.DurationMS != 390 || r.MinDelayMS != 40 || r.HasAlpha || !r.LoopForever {
		t.Errorf("synthetic timing: %+v", r)
	}
	if r.Width != fxW || r.Height != fxH {
		t.Errorf("dims %dx%d", r.Width, r.Height)
	}
}

// ffmpeg's palette pipeline leaves frame 0 opaque and uses transparency for
// inter-frame diffs; the linter must set frame 0's flag to the index the
// other frames use (255, unused by frame 0) and align the LSD background.
func TestLintGIFFFmpegTransdiffGetsFrame0Flag(t *testing.T) {
	data := readFixture(t, "ff_transdiff.gif")
	before := lintOnly(t, data, TargetEmote)
	c := expectCheck(t, before, RuleGIFFrame0Transparent, false, false)
	if !strings.Contains(c.Detail, "index 255") {
		t.Errorf("detail should name index 255: %s", c.Detail)
	}
	// With frame 0 opaque, background 255 collides with the other frames'
	// transparent index (lilliput would drop their transparency).
	expectCheck(t, before, RuleGIFLSDBackground, false, false)

	after, out := lintFix(t, data, TargetEmote)
	expectCheck(t, after, RuleGIFFrame0Transparent, true, true)
	// Once frame 0 is transparent with index 255 the background index is
	// already right, so nothing needs fixing there.
	expectCheck(t, after, RuleGIFLSDBackground, true, false)
	if !after.OK || !after.HasAlpha {
		t.Errorf("after: ok=%v alpha=%v", after.OK, after.HasAlpha)
	}
	gces := frameGCEs(t, out)
	if !gces[0].transparent || gces[0].transIndex != 255 {
		t.Errorf("frame 0 GCE after fix: %+v", *gces[0])
	}
	// Only the GCE packed byte and the transparent index byte differ.
	if n := countDiff(t, data, out); n != 2 {
		t.Errorf("%d bytes differ, want 2 (GCE packed byte + transparent index)", n)
	}
	samePixels(t, data, out)
	relint, _ := lintFix(t, out, TargetEmote)
	expectClean(t, relint)
}

// countDiff returns the number of differing bytes between two equally long
// byte slices.
func countDiff(t *testing.T, a, b []byte) int {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("length changed %d -> %d", len(a), len(b))
	}
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// ff_opaque.gif uses a different diff index per frame; the fixer picks the
// most common one (13) for frame 0 and moves the background index to it.
func TestLintGIFFFmpegOpaquePicksCommonIndex(t *testing.T) {
	data := readFixture(t, "ff_opaque.gif")
	before := lintOnly(t, data, TargetAttachment)
	expectCheck(t, before, RuleGIFFrame0Transparent, false, false)
	expectCheck(t, before, RuleGIFLSDBackground, true, false) // bg 31 is nobody's transparent index

	after, out := lintFix(t, data, TargetAttachment)
	expectCheck(t, after, RuleGIFFrame0Transparent, true, true)
	expectCheck(t, after, RuleGIFLSDBackground, true, true)
	gces := frameGCEs(t, out)
	if !gces[0].transparent || gces[0].transIndex != 13 {
		t.Errorf("frame 0 GCE: %+v", *gces[0])
	}
	if out[11] != 13 {
		t.Errorf("LSD background index = %d, want 13", out[11])
	}
	if n := countDiff(t, data, out); n != 3 {
		t.Errorf("%d bytes differ, want 3 (background index, GCE packed byte, transparent index)", n)
	}
	samePixels(t, data, out)
	relint, _ := lintFix(t, out, TargetAttachment)
	expectClean(t, relint)
}

func TestLintGIFGCEEveryFrame(t *testing.T) {
	// image/gif omits the GCE when delay, disposal and transparency are all
	// zero — a file with no GCE at all.
	a := opaqueAnim()
	for i := range a.frames {
		a.frames[i].delay = 0
		a.frames[i].disposal = 0
	}
	data := encodeFx(t, a)
	for _, g := range frameGCEs(t, data) {
		if g != nil {
			t.Fatal("fixture unexpectedly has a GCE")
		}
	}
	out := assertFixed(t, data, TargetEmote, RuleGIFGCEEveryFrame)
	// The fix is byte-identical to what image/gif writes for delay 10 /
	// disposal 1 (the defaults used for synthesised GCEs).
	want := opaqueAnim()
	want.frames[2].disposal = 1
	if !bytes.Equal(out, encodeFx(t, want)) {
		t.Error("fixed bytes differ from the image/gif encoding with GCEs")
	}
	dec := decodeGIF(t, out)
	for i, d := range dec.Delay {
		if d != 10 || dec.Disposal[i] != gif.DisposalNone {
			t.Errorf("frame %d delay %d disposal %d", i, d, dec.Disposal[i])
		}
	}

	// A single frame without a GCE among frames with one copies the
	// neighbour's delay.
	b := opaqueAnim()
	b.frames[0].delay, b.frames[2].delay = 7, 7
	b.frames[1].delay, b.frames[1].disposal = 0, 0
	out = assertFixed(t, encodeFx(t, b), TargetNone, RuleGIFGCEEveryFrame)
	if g := frameGCEs(t, out)[1]; g == nil || g.delayCS != 7 || g.disposal != 1 || g.transparent {
		t.Errorf("synthesised GCE: %+v", g)
	}

	// Duplicate GCEs before a frame are collapsed to the last one.
	dup := mutateGIF(t, encodeFx(t, opaqueAnim()), func(g *gifFile) {
		frames, _ := g.frames()
		g.insertBlock(frames[1].gceBlock, &gifGCE{disposal: 3, delayCS: 99})
	})
	before := lintOnly(t, dup, TargetNone)
	if c := expectCheck(t, before, RuleGIFGCEEveryFrame, false, false); !strings.Contains(c.Detail, "more than one") {
		t.Errorf("detail: %s", c.Detail)
	}
	out = assertFixed(t, dup, TargetNone, RuleGIFGCEEveryFrame)
	if !bytes.Equal(out, encodeFx(t, opaqueAnim())) {
		t.Error("removing the duplicate should restore the original bytes")
	}
}

func TestLintGIFNoFramesFails(t *testing.T) {
	data := encodeFx(t, opaqueAnim())
	empty := mutateGIF(t, data, func(g *gifFile) { g.blocks = nil })
	r := lintOnly(t, empty, TargetNone)
	if c := expectCheck(t, r, RuleGIFGCEEveryFrame, false, false); !strings.Contains(c.Detail, "no image frames") {
		t.Errorf("detail: %s", c.Detail)
	}
	if r.OK || r.Frames != 0 {
		t.Errorf("report: ok=%v frames=%d", r.OK, r.Frames)
	}
}

func TestLintGIFFrame0Transparency(t *testing.T) {
	data := encodeFx(t, opaqueFrame0Anim())
	before := lintOnly(t, data, TargetEmote)
	if c := expectCheck(t, before, RuleGIFFrame0Transparent, false, false); !strings.Contains(c.Detail, "transparent index really used on frame 1") {
		t.Errorf("detail: %s", c.Detail)
	}
	if !before.HasAlpha {
		t.Error("HasAlpha should be true: later frames are transparent")
	}
	out := assertFixed(t, data, TargetEmote, RuleGIFFrame0Transparent)
	// Golden: identical to image/gif encoding the same animation with the
	// transparent palette on frame 0 (flag + index 5) and background 5.
	want := opaqueFrame0Anim()
	want.frames[0].transparent = true
	want.bg = fxTransIx
	if !bytes.Equal(out, encodeFx(t, want)) {
		t.Error("fixed bytes differ from the golden image/gif encoding")
	}
	after, _ := lintFix(t, data, TargetEmote)
	expectCheck(t, after, RuleGIFLSDBackground, true, true)
	dec := decodeGIF(t, out)
	if _, _, _, a := dec.Image[0].Palette[fxTransIx].RGBA(); a != 0 {
		t.Error("frame 0 palette entry 5 is not transparent after the fix")
	}
	if dec.BackgroundIndex != fxTransIx {
		t.Errorf("background index %d, want %d", dec.BackgroundIndex, fxTransIx)
	}
}

func TestLintGIFFrame0TransparencyPicksUnusedIndex(t *testing.T) {
	// Frame 0 uses index 5 (the others' transparent index) itself, so the
	// lowest index it does not use (0 is used as fill... make it use 0-5)
	// must be chosen.
	a := opaqueFrame0Anim()
	a.frames[0].fill = 5
	a.frames[0].transRect = image.Rectangle{} // no transparency, but pixels ARE index 5
	data := encodeFx(t, a)
	// frame 0 is filled with 5 only → lowest unused is 0
	out := assertFixed(t, data, TargetNone, RuleGIFFrame0Transparent)
	if g := frameGCEs(t, out)[0]; !g.transparent || g.transIndex != 0 {
		t.Errorf("frame 0 GCE: %+v", *g)
	}
	if out[11] != 0 {
		t.Errorf("bg index %d, want 0", out[11])
	}
}

func TestLintGIFFrame0TransparencyUnfixable(t *testing.T) {
	// Frame 0 uses every palette entry.
	a := opaqueFrame0Anim()
	data := encodeFx(t, a)
	full := mutateGIF(t, data, func(g *gifFile) {
		// Replace frame 0's pixel data with an image using all 8 indices,
		// encoded by image/gif and spliced in raw.
		pm := image.NewPaletted(image.Rect(0, 0, fxW, fxH), fxPalette)
		for i := range pm.Pix {
			pm.Pix[i] = byte(i % 8)
		}
		var buf bytes.Buffer
		if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{pm}, Delay: []int{10}, Config: image.Config{ColorModel: fxPalette, Width: fxW, Height: fxH}}); err != nil {
			t.Fatal(err)
		}
		single, err := parseGIF(buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		sf, _ := single.frames()
		frames, _ := g.frames()
		g.blocks[frames[0].imageBlock] = sf[0].image
	})
	r, out := lintFix(t, full, TargetEmote)
	c := expectCheck(t, r, RuleGIFFrame0Transparent, false, false)
	if !strings.Contains(c.Detail, "uses all 8 global palette entries") {
		t.Errorf("detail: %s", c.Detail)
	}
	if r.OK {
		t.Error("report OK despite an unfixable error")
	}
	// Frame 0 stays opaque, so the LSD rule falls back to the opaque-
	// background branch: bg 0 is not any frame's transparent index → OK.
	expectCheck(t, r, RuleGIFLSDBackground, true, false)
	decodeGIF(t, out)

	// Frame 0 with a local colour table is not fixable either.
	lct := opaqueFrame0Anim()
	data = encodeFxWithLocalPalette(t, lct, 0)
	r, _ = lintFix(t, data, TargetNone)
	if c := expectCheck(t, r, RuleGIFFrame0Transparent, false, false); !strings.Contains(c.Detail, "local colour table") {
		t.Errorf("detail: %s", c.Detail)
	}
}

// encodeFxWithLocalPalette encodes a like encodeFx but gives frame idx a
// different palette so image/gif writes a Local Color Table for it.
func encodeFxWithLocalPalette(t *testing.T, a fxAnim, idx int) []byte {
	t.Helper()
	data := encodeFx(t, a)
	dec := decodeGIF(t, data)
	local := append(color.Palette(nil), fxPalette...)
	local[7] = color.RGBA{128, 128, 128, 255}
	dec.Image[idx].Palette = local
	dec.Config.ColorModel = fxPalette
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, dec); err != nil {
		t.Fatal(err)
	}
	g, err := parseGIF(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := g.frames()
	if !frames[idx].image.hasLCT() {
		t.Fatal("fixture frame has no local colour table")
	}
	return buf.Bytes()
}

func TestLintGIFLSDBackgroundIndex(t *testing.T) {
	// Transparent animation whose background index does not match frame
	// 0's transparent index.
	a := alphaAnim()
	a.bg = 2
	data := encodeFx(t, a)
	out := assertFixed(t, data, TargetEmote, RuleGIFLSDBackground)
	if out[11] != fxTransIx {
		t.Errorf("bg index %d, want %d", out[11], fxTransIx)
	}
	if !bytes.Equal(out, encodeFx(t, alphaAnim())) {
		t.Error("only the background index byte should change")
	}

	// Nothing is really transparent (flags set, index unused) but the
	// background index equals a flagged index → moved to a free index.
	b := opaqueAnim()
	b.bg = fxTransIx
	for i := 1; i < 3; i++ {
		b.frames[i].transparent = true // flag with index 5, pixels never use 5
	}
	data = encodeFx(t, b)
	before := lintOnly(t, data, TargetNone)
	if before.HasAlpha {
		t.Error("HasAlpha should be false when the transparent index is unused")
	}
	expectCheck(t, before, RuleGIFFrame0Transparent, true, false)
	out = assertFixed(t, data, TargetNone, RuleGIFLSDBackground)
	if out[11] != 0 {
		t.Errorf("bg index %d, want 0", out[11])
	}
}

func TestLintGIFDisposal(t *testing.T) {
	a := opaqueAnim()
	for i := range a.frames {
		a.frames[i].disposal = 0
	}
	data := encodeFx(t, a)
	out := assertFixed(t, data, TargetEmote, RuleGIFDisposal)
	want := opaqueAnim()
	want.frames[2].disposal = 1
	if !bytes.Equal(out, encodeFx(t, want)) {
		t.Error("fixed bytes differ from the image/gif encoding with disposal 1")
	}

	// Disposal 3 cannot be fixed; disposal 0 on other frames still is,
	// but the rule stays failed.
	d3 := mutateGIF(t, encodeFx(t, opaqueAnim()), func(g *gifFile) {
		frames, _ := g.frames()
		frames[1].gce.setDisposal(3)
		frames[2].gce.setDisposal(0)
	})
	r, out := lintFix(t, d3, TargetEmote)
	c := expectCheck(t, r, RuleGIFDisposal, false, false)
	if !strings.Contains(c.Detail, "disposal 3 (restore previous) or a reserved value on frame 1") || !strings.Contains(c.Detail, "disposal 0 (unspecified) on frame 2; set to 1") {
		t.Errorf("detail: %s", c.Detail)
	}
	if r.OK {
		t.Error("report OK with disposal 3")
	}
	gces := frameGCEs(t, out)
	if gces[1].disposal != 3 || gces[2].disposal != 1 {
		t.Errorf("disposals after fix: %d %d", gces[1].disposal, gces[2].disposal)
	}
	decodeGIF(t, out)
}

func TestLintGIFNetscapeLoop(t *testing.T) {
	// Missing block: inserted right after the GCT, byte-identical to what
	// image/gif writes for LoopCount 0.
	a := opaqueAnim()
	a.loop = -1
	data := encodeFx(t, a)
	if bytes.Contains(data, []byte("NETSCAPE2.0")) {
		t.Fatal("fixture unexpectedly has a NETSCAPE block")
	}
	before := lintOnly(t, data, TargetEmote)
	if before.LoopForever {
		t.Error("LoopForever true without a NETSCAPE block")
	}
	out := assertFixed(t, data, TargetEmote, RuleGIFNetscapeLoop)
	if !bytes.Equal(out, encodeFx(t, opaqueAnim())) {
		t.Error("fixed bytes differ from the image/gif encoding with LoopCount 0")
	}
	if dec := decodeGIF(t, out); dec.LoopCount != 0 {
		t.Errorf("image/gif LoopCount %d, want 0", dec.LoopCount)
	}

	// Loop count 3 → 0 for every Discord target (byte-patched fixture,
	// byte-level check).
	data = patchNetscapeLoop(t, encodeFx(t, opaqueAnim()), 3)
	if dec := decodeGIF(t, data); dec.LoopCount != 3 {
		t.Fatalf("fixture LoopCount %d", dec.LoopCount)
	}
	for _, target := range []Target{TargetEmote, TargetSticker, TargetAttachment} {
		before = lintOnly(t, data, target)
		c := expectCheck(t, before, RuleGIFNetscapeLoop, false, false)
		if c.Level != LevelError || !strings.Contains(c.Detail, "loop count 3 (plays 4 times)") || before.LoopForever || before.OK {
			t.Errorf("%s before: %+v loop=%v ok=%v", target, c, before.LoopForever, before.OK)
		}
		if target == TargetSticker { // 16x16 fails the sticker dims, so no clean re-lint
			after, out := lintFix(t, data, target)
			expectCheck(t, after, RuleGIFNetscapeLoop, true, true)
			if !bytes.Equal(out, encodeFx(t, opaqueAnim())) || !after.LoopForever {
				t.Errorf("%s: fixed bytes/LoopForever=%v wrong", target, after.LoopForever)
			}
			continue
		}
		out = assertFixed(t, data, target, RuleGIFNetscapeLoop)
		if !bytes.Equal(out, encodeFx(t, opaqueAnim())) {
			t.Errorf("%s: loop count fix should restore the LoopCount 0 encoding", target)
		}
		if after, _ := lintFix(t, data, target); !after.LoopForever {
			t.Errorf("%s: LoopForever false after forcing count 0", target)
		}
	}

	// A NETSCAPE block only after the first image is not where giflib
	// looks: a leading one is inserted.
	late := mutateGIF(t, encodeFx(t, opaqueAnim()), func(g *gifFile) {
		ns := g.blocks[0]
		g.removeBlocks([]int{0})
		frames, _ := g.frames()
		g.insertBlock(frames[1].gceBlock, ns)
	})
	before = lintOnly(t, late, TargetNone)
	if c := expectCheck(t, before, RuleGIFNetscapeLoop, false, false); !strings.Contains(c.Detail, "only after the first frame") {
		t.Errorf("detail: %s", c.Detail)
	}
	out = assertFixed(t, late, TargetNone, RuleGIFNetscapeLoop)
	g, err := parseGIF(out)
	if err != nil {
		t.Fatal(err)
	}
	if app, ok := g.blocks[0].(*gifAppExt); !ok || !app.isNetscape() {
		t.Errorf("block 0 after fix: %T", g.blocks[0])
	}
}

// TargetNone accepts any loop count (N = play N+1 times, the user's own
// setting) as long as a NETSCAPE2.0 block precedes the first image; only a
// missing or malformed block is fixed, and the file's own count survives.
func TestLintGIFNetscapeLoopTargetNoneKeepsCount(t *testing.T) {
	data := patchNetscapeLoop(t, encodeFx(t, opaqueAnim()), 3)

	// Count 3 passes and the bytes are left alone even with fix=true.
	before := lintOnly(t, data, TargetNone)
	c := expectCheck(t, before, RuleGIFNetscapeLoop, true, false)
	if !strings.Contains(c.Detail, "loop count 3 (plays 4 times)") || !strings.Contains(c.Detail, "Discord targets require 0") {
		t.Errorf("detail: %s", c.Detail)
	}
	if before.LoopForever || !before.OK {
		t.Errorf("loop=%v ok=%v", before.LoopForever, before.OK)
	}
	after, out := lintFix(t, data, TargetNone)
	expectClean(t, after)
	if !bytes.Equal(out, data) {
		t.Error("count-3 file rewritten for TargetNone")
	}
	if after.LoopForever {
		t.Error("LoopForever must mean count 0")
	}
	if dec := decodeGIF(t, out); dec.LoopCount != 3 {
		t.Errorf("image/gif LoopCount %d, want 3", dec.LoopCount)
	}

	// Count 0 passes with LoopForever true.
	r := lintOnly(t, encodeFx(t, opaqueAnim()), TargetNone)
	if c := expectCheck(t, r, RuleGIFNetscapeLoop, true, false); !r.LoopForever || !strings.Contains(c.Detail, "loop count 0 (loops forever)") {
		t.Errorf("count 0: loop=%v detail=%s", r.LoopForever, c.Detail)
	}

	// Missing block: inserted with count 0 (same as the Discord case).
	a := opaqueAnim()
	a.loop = -1
	missing := encodeFx(t, a)
	before = lintOnly(t, missing, TargetNone)
	if c := expectCheck(t, before, RuleGIFNetscapeLoop, false, false); c.Level != LevelError || !strings.Contains(c.Detail, "no NETSCAPE2.0 looping block") {
		t.Errorf("missing: %+v", c)
	}
	out = assertFixed(t, missing, TargetNone, RuleGIFNetscapeLoop)
	if !bytes.Equal(out, encodeFx(t, opaqueAnim())) {
		t.Error("missing block should be inserted with count 0")
	}
	if after, _ := lintFix(t, missing, TargetNone); !after.LoopForever {
		t.Error("LoopForever false after inserting a loop-forever block")
	}

	// Block only after the first image with count 3: the inserted leading
	// block copies that count instead of forcing 0.
	late := mutateGIF(t, data, func(g *gifFile) {
		ns := g.blocks[0]
		g.removeBlocks([]int{0})
		frames, _ := g.frames()
		g.insertBlock(frames[1].gceBlock, ns)
	})
	out = assertFixed(t, late, TargetNone, RuleGIFNetscapeLoop)
	g, err := parseGIF(out)
	if err != nil {
		t.Fatal(err)
	}
	app, ok := g.blocks[0].(*gifAppExt)
	if !ok || !app.isNetscape() {
		t.Fatalf("block 0 after fix: %T", g.blocks[0])
	}
	if n, ok := app.loopCount(); !ok || n != 3 {
		t.Errorf("leading block count %d ok=%v, want 3", n, ok)
	}
	if after, _ := lintFix(t, late, TargetNone); after.LoopForever {
		t.Error("LoopForever true for count 3")
	}
	// The same file for a Discord target gets every block set to 0.
	r, out = lintFix(t, late, TargetEmote)
	expectCheck(t, r, RuleGIFNetscapeLoop, true, true)
	g, err = parseGIF(out)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range g.blocks {
		if app, ok := b.(*gifAppExt); ok && app.isNetscape() {
			if n, ok := app.loopCount(); !ok || n != 0 {
				t.Errorf("block %d count %d ok=%v after Discord fix, want 0", i, n, ok)
			}
		}
	}
	if !r.LoopForever {
		t.Error("LoopForever false after the Discord fix")
	}

	// A malformed loop sub-block is repaired to 0 even for TargetNone.
	malformed := mutateGIF(t, encodeFx(t, opaqueAnim()), func(g *gifFile) {
		g.blocks[0].(*gifAppExt).sub[1] = []byte{gifNetscapeLoopSub, 0} // 2 bytes, not 3
	})
	before = lintOnly(t, malformed, TargetNone)
	if c := expectCheck(t, before, RuleGIFNetscapeLoop, false, false); !strings.Contains(c.Detail, "malformed loop sub-block") || before.LoopForever {
		t.Errorf("malformed: %+v loop=%v", c, before.LoopForever)
	}
	after, out = lintFix(t, malformed, TargetNone)
	if c := expectCheck(t, after, RuleGIFNetscapeLoop, true, true); !strings.Contains(c.Detail, "set to 0") || !after.LoopForever {
		t.Errorf("malformed after fix: %+v loop=%v", c, after.LoopForever)
	}
	relint, again := lintFix(t, out, TargetNone)
	expectClean(t, relint)
	if !bytes.Equal(again, out) {
		t.Error("second fix pass changed the bytes")
	}
	decodeGIF(t, out)
}

// A single-frame GIF has nothing to loop: LoopForever is true whether or
// not a NETSCAPE2.0 block exists (the rule itself is unchanged, so the
// fixer still inserts the block for Discord targets — see
// TestLintGIFSizeLimit).
func TestLintGIFSingleFrameLoopForever(t *testing.T) {
	pm := image.NewPaletted(image.Rect(0, 0, fxW, fxH), fxPalette)
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{pm}, Delay: []int{10}, Disposal: []byte{1}, Config: image.Config{ColorModel: fxPalette, Width: fxW, Height: fxH}}); err != nil {
		t.Fatal(err)
	}
	single := buf.Bytes()
	if bytes.Contains(single, []byte("NETSCAPE2.0")) {
		t.Fatal("image/gif wrote a NETSCAPE block for a single frame")
	}
	for _, target := range []Target{TargetNone, TargetEmote} {
		r := lintOnly(t, single, target)
		expectCheck(t, r, RuleGIFNetscapeLoop, false, false)
		if !r.LoopForever || r.Frames != 1 {
			t.Errorf("%s fix=false: loop=%v frames=%d", target, r.LoopForever, r.Frames)
		}
		r, _ = lintFix(t, single, target)
		expectCheck(t, r, RuleGIFNetscapeLoop, true, true)
		if !r.LoopForever {
			t.Errorf("%s fix=true: LoopForever false", target)
		}
	}
	// A multi-frame file without the block still reports LoopForever false.
	a := opaqueAnim()
	a.loop = -1
	if r := lintOnly(t, encodeFx(t, a), TargetNone); r.LoopForever {
		t.Error("multi-frame file without a NETSCAPE block reports LoopForever")
	}
}

func TestLintGIFMinDelay(t *testing.T) {
	a := opaqueAnim()
	a.frames[0].delay = 0
	a.frames[1].delay = 1
	data := encodeFx(t, a)
	before := lintOnly(t, data, TargetEmote)
	if before.MinDelayMS != 0 || before.DurationMS != 110 {
		t.Errorf("before: min %d duration %d", before.MinDelayMS, before.DurationMS)
	}
	c := expectCheck(t, before, RuleGIFMinDelay, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "delays below 2 cs on frames 0, 1") {
		t.Errorf("check: %+v", c)
	}
	out := assertFixed(t, data, TargetEmote, RuleGIFMinDelay)
	if !bytes.Equal(out, encodeFx(t, opaqueAnim())) {
		t.Error("fixed bytes differ from the image/gif encoding with 10 cs delays")
	}
	after, _ := lintFix(t, data, TargetEmote)
	if after.MinDelayMS != 100 || after.DurationMS != 300 {
		t.Errorf("after: min %d duration %d", after.MinDelayMS, after.DurationMS)
	}
}

func TestLintGIFGlobalPalette(t *testing.T) {
	data := encodeFxWithLocalPalette(t, opaqueAnim(), 1)
	for _, tc := range []struct {
		target Target
		level  Level
	}{{TargetEmote, LevelError}, {TargetSticker, LevelError}, {TargetAttachment, LevelError}, {TargetNone, LevelWarn}} {
		r, out := lintFix(t, data, tc.target)
		c := expectCheck(t, r, RuleGIFGlobalPalette, false, false)
		if c.Level != tc.level || !strings.Contains(c.Detail, "local colour table on frame 1 (1 of 3 frames)") {
			t.Errorf("%s: %+v", tc.target, c)
		}
		if r.OK != (tc.level == LevelWarn) {
			t.Errorf("%s: report OK=%v", tc.target, r.OK)
		}
		if !bytes.Equal(out, data) {
			t.Errorf("%s: local colour tables are not fixable, bytes must be unchanged", tc.target)
		}
	}
	// No global colour table and an opaque frame without a local one: error
	// even for TargetNone.
	noGCT := mutateGIF(t, encodeFx(t, opaqueAnim()), func(g *gifFile) {
		g.lsdPacked &^= 0x80
		g.gct = nil
	})
	r := lintOnly(t, noGCT, TargetNone)
	if c := expectCheck(t, r, RuleGIFGlobalPalette, false, false); c.Level != LevelError || !strings.Contains(c.Detail, "no global colour table") {
		t.Errorf("check: %+v", c)
	}
}

func TestLintGIFNoInterlace(t *testing.T) {
	data := mutateGIF(t, encodeFx(t, opaqueAnim()), func(g *gifFile) {
		frames, _ := g.frames()
		img := frames[2].image
		raw := append([]byte(nil), img.raw...)
		raw[9] |= 0x40
		img.raw = raw
		img.packed |= 0x40
	})
	r, out := lintFix(t, data, TargetEmote)
	c := expectCheck(t, r, RuleGIFNoInterlace, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "interlaced: frame 2") {
		t.Errorf("check: %+v", c)
	}
	if !r.OK || !bytes.Equal(out, data) {
		t.Error("interlace is a warning and not fixable")
	}
	decodeGIF(t, out)
}

func TestLintGIFNoExtraExtensions(t *testing.T) {
	base := encodeFx(t, opaqueAnim())
	comment := []byte{0x21, 0xFE, 0x05, 'h', 'e', 'l', 'l', 'o', 0x00}
	plain := []byte{0x21, 0x01, 0x0C, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x02, 'h', 'i', 0x00}
	xmp := append([]byte{0x21, 0xFF, 0x0B}, []byte("XMP DataXMP")...)
	xmp = append(xmp, 0x03, 'a', 'b', 'c', 0x00)
	dangling := []byte{0x21, 0xF9, 0x04, 0x04, 0x0A, 0x00, 0x00, 0x00}
	data := insertAfterGCT(base, append(append(append([]byte(nil), comment...), plain...), xmp...))
	data = append(data[:len(data)-1], append(dangling, 0x3B)...) // dangling GCE before the trailer

	before := lintOnly(t, data, TargetEmote)
	c := expectCheck(t, before, RuleGIFNoExtraExtensions, false, false)
	if c.Level != LevelInfo {
		t.Errorf("level %s", c.Level)
	}
	for _, want := range []string{"comment extension", "plain-text extension", `application extension "XMP DataXMP"`, "Graphic Control Extension with no following image"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
	if !before.OK {
		t.Error("info-level failures must not clear Report.OK")
	}
	out := assertFixed(t, data, TargetEmote, RuleGIFNoExtraExtensions)
	if !bytes.Equal(out, base) {
		t.Error("stripping the extensions should restore the original bytes exactly")
	}
}

func TestLintGIFFirstFrameVisible(t *testing.T) {
	a := alphaAnim()
	a.frames[0].transRect = a.frames[0].rect // frame 0 entirely transparent
	data := encodeFx(t, a)
	r, out := lintFix(t, data, TargetEmote)
	c := expectCheck(t, r, RuleGIFFirstFrameVisible, false, false)
	if c.Level != LevelWarn || !r.OK || !bytes.Equal(out, data) {
		t.Errorf("check %+v ok=%v changed=%v", c, r.OK, !bytes.Equal(out, data))
	}

	// Frame 0 with visible pixels passes; an opaque frame 0 passes.
	r, _ = lintFix(t, encodeFx(t, alphaAnim()), TargetNone)
	expectCheck(t, r, RuleGIFFirstFrameVisible, true, false)
	r, _ = lintFix(t, encodeFx(t, opaqueAnim()), TargetNone)
	expectCheck(t, r, RuleGIFFirstFrameVisible, true, false)

	// Undecodable transparent frame 0: the check is skipped.
	broken := corruptFrame(t, encodeFx(t, alphaAnim()), 0)
	r, _ = lintFix(t, broken, TargetNone)
	if hasCheck(r, RuleGIFFirstFrameVisible) {
		t.Error("first-frame-visible must be skipped when frame 0 cannot be decoded")
	}
	expectCheck(t, r, RuleGIFFrame0Transparent, true, false) // flag already set

	// Undecodable opaque frame 0 followed by transparent frames: the fixer
	// cannot pick an unused index and says so.
	broken = corruptFrame(t, encodeFx(t, opaqueFrame0Anim()), 0)
	r, _ = lintFix(t, broken, TargetNone)
	expectCheck(t, r, RuleGIFFirstFrameVisible, true, false) // opaque frame 0 needs no decoding
	if c := expectCheck(t, r, RuleGIFFrame0Transparent, false, false); !strings.Contains(c.Detail, "could not be decoded") {
		t.Errorf("detail: %s", c.Detail)
	}
}

// corruptFrame overwrites the LZW payload of frame idx with 0xFF bytes (an
// invalid code stream for compress/lzw and image/gif alike) while keeping
// the block structure intact. The synthetic frames fit in one sub-block.
func corruptFrame(t *testing.T, data []byte, idx int) []byte {
	t.Helper()
	return mutateGIF(t, data, func(g *gifFile) {
		frames, _ := g.frames()
		img := frames[idx].image
		if len(img.data) != 1 {
			t.Fatalf("frame %d has %d sub-blocks, helper expects 1", idx, len(img.data))
		}
		raw := append([]byte(nil), img.raw...)
		start := 10 + 1 + 1 // descriptor + min code size + sub-block length byte
		payload := raw[start : len(raw)-1]
		for i := range payload {
			payload[i] = 0xFF
		}
		img.raw = raw
		img.data = [][]byte{payload}
	})
}

func TestLintGIFTransparencyAssumedWhenUndecodable(t *testing.T) {
	// Frame 1 carries the transparency flag but its pixels are garbage:
	// transparency is assumed and frame 0 still gets the flag.
	broken := corruptFrame(t, encodeFx(t, opaqueFrame0Anim()), 1)
	r, out := lintFix(t, broken, TargetNone)
	c := expectCheck(t, r, RuleGIFFrame0Transparent, true, true)
	if !strings.Contains(c.Detail, "transparency assumed") {
		t.Errorf("detail: %s", c.Detail)
	}
	if !r.HasAlpha {
		t.Error("HasAlpha should be assumed true")
	}
	if g := frameGCEs(t, out)[0]; !g.transparent || g.transIndex != fxTransIx {
		t.Errorf("frame 0 GCE: %+v", *g)
	}
}

func TestLintGIFTrailer(t *testing.T) {
	data := encodeFx(t, opaqueAnim())
	cut := data[:len(data)-1]
	before := lintOnly(t, cut, TargetEmote)
	if c := expectCheck(t, before, RuleGIFTrailer, false, false); c.Level != LevelError {
		t.Errorf("check: %+v", c)
	}
	after, out := lintFix(t, cut, TargetEmote)
	expectCheck(t, after, RuleGIFTrailer, true, true)
	if !bytes.Equal(out, data) {
		t.Error("appending the trailer should restore the original bytes")
	}
	relint, _ := lintFix(t, out, TargetEmote)
	expectClean(t, relint)
}

func TestLintGIF87aRelabelledWhenFixed(t *testing.T) {
	a := opaqueAnim()
	a.loop = -1
	data := append([]byte("GIF87a"), encodeFx(t, a)[6:]...)
	// Untouched when nothing needs fixing (fix=false) …
	r := lintOnly(t, data, TargetNone)
	expectCheck(t, r, RuleGIFNetscapeLoop, false, false)
	// … relabelled GIF89a once the fixer writes 89a features.
	_, out := lintFix(t, data, TargetNone)
	if string(out[:6]) != "GIF89a" {
		t.Errorf("header %q after fix, want GIF89a", out[:6])
	}
	if !bytes.Equal(out, encodeFx(t, opaqueAnim())) {
		t.Error("fixed 87a file should equal the 89a encoding with a loop block")
	}
	// A compliant 87a file is left alone.
	clean := append([]byte("GIF87a"), encodeFx(t, opaqueAnim())[6:]...)
	if _, out := lintFix(t, clean, TargetNone); !bytes.Equal(out, clean) {
		t.Error("compliant GIF87a rewritten")
	}
}

func TestFrameListWording(t *testing.T) {
	cases := map[string][]int{
		"frame 3":                         {3},
		"frames 0, 3":                     {0, 3},
		"frames 0, 1, 2, 3, 4":            {0, 1, 2, 3, 4},
		"frames 0, 1, 2, 3, 4 and 2 more": {0, 1, 2, 3, 4, 5, 6},
	}
	for want, idx := range cases {
		if got := frameList(idx); got != want {
			t.Errorf("frameList(%v) = %q, want %q", idx, got, want)
		}
	}
	if plural(1, "frame") != "1 frame" || plural(2, "frame") != "2 frames" {
		t.Error("plural")
	}
	// WebP counts are plays; GIF NETSCAPE counts are plays-1 (0 = forever).
	if plays(0) != "loops forever" || plays(1) != "plays once" || plays(4) != "plays 4 times" {
		t.Errorf("plays: %q %q %q", plays(0), plays(1), plays(4))
	}
	if gifPlays(0) != "loops forever" || gifPlays(1) != "plays 2 times" || gifPlays(65535) != "plays 65536 times" {
		t.Errorf("gifPlays: %q %q %q", gifPlays(0), gifPlays(1), gifPlays(65535))
	}
	if loopCountWords([]uint16{0}) != "loop count 0 (loops forever)" || loopCountWords([]uint16{3}) != "loop count 3 (plays 4 times)" || loopCountWords([]uint16{0, 3}) != "loop counts 0, 3" {
		t.Errorf("loopCountWords: %q %q %q", loopCountWords([]uint16{0}), loopCountWords([]uint16{3}), loopCountWords([]uint16{0, 3}))
	}
}

func TestLintGIFSizeLimit(t *testing.T) {
	// 700x700 of random 8-bit indices does not compress below 512 KiB.
	rng := rand.New(rand.NewPCG(1, 2))
	pal := make(color.Palette, 256)
	for i := range pal {
		pal[i] = color.RGBA{byte(i), byte(255 - i), byte(i * 7), 255}
	}
	pm := image.NewPaletted(image.Rect(0, 0, 700, 700), pal)
	for i := range pm.Pix {
		pm.Pix[i] = byte(rng.IntN(256))
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{pm}, Delay: []int{10}, Disposal: []byte{1}, LoopCount: 0, Config: image.Config{ColorModel: pal, Width: 700, Height: 700}}); err != nil {
		t.Fatal(err)
	}
	big := buf.Bytes()
	if len(big) <= 524288 {
		t.Fatalf("fixture only %d bytes", len(big))
	}
	for _, tc := range []struct {
		target Target
		ok     bool
	}{{TargetEmote, false}, {TargetSticker, false}, {TargetAttachment, true}} {
		r := lintOnly(t, big, tc.target)
		c := expectCheck(t, r, RuleGIFSizeLimit, tc.ok, false)
		if !tc.ok && !strings.Contains(c.Detail, "exceeds") {
			t.Errorf("%s detail: %s", tc.target, c.Detail)
		}
		if r.Bytes != int64(len(big)) || r.Limit != Limit(tc.target) {
			t.Errorf("%s: bytes %d limit %d", tc.target, r.Bytes, r.Limit)
		}
	}
	// The limit is evaluated on the fixed bytes: the NETSCAPE block the
	// fixer adds to this single-frame file (image/gif writes none) counts.
	r, out := lintFix(t, big, TargetAttachment)
	expectCheck(t, r, RuleGIFNetscapeLoop, true, true)
	if r.Bytes != int64(len(out)) || len(out) != len(big)+19 {
		t.Errorf("Bytes %d, len(out) %d, len(in) %d", r.Bytes, len(out), len(big))
	}
	if c := findCheck(t, r, RuleGIFSizeLimit); !strings.Contains(c.Detail, fmt.Sprint(len(out))) {
		t.Errorf("size detail should quote the fixed length %d: %s", len(out), c.Detail)
	}
}

func TestLintGIFStickerRules(t *testing.T) {
	small := encodeFx(t, opaqueAnim()) // 16x16, 3 frames, 300 ms
	r, _ := lintFix(t, small, TargetSticker)
	if c := expectCheck(t, r, RuleGIFStickerDims, false, false); !strings.Contains(c.Detail, "16x16") {
		t.Errorf("detail: %s", c.Detail)
	}
	expectCheck(t, r, RuleGIFStickerDuration, true, false)
	if r.OK || hasCheck(r, RuleGIFEmoteDims) {
		t.Errorf("sticker report: ok=%v rules=%v", r.OK, ruleIDs(r))
	}

	// 320x320, but 6 s long and 100 fps.
	pm := image.NewPaletted(image.Rect(0, 0, 320, 320), fxPalette)
	g := &gif.GIF{Config: image.Config{ColorModel: fxPalette, Width: 320, Height: 320}, LoopCount: 0}
	for i := 0; i < 600; i++ {
		g.Image = append(g.Image, pm)
		g.Delay = append(g.Delay, 1)
		g.Disposal = append(g.Disposal, 1)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	// Before fixing: 600 frames x 1 cs = 6000 ms and 100 fps → both fail;
	// after fixing delays to 10 cs the duration is 60 s.
	before := lintOnly(t, buf.Bytes(), TargetSticker)
	expectCheck(t, before, RuleGIFStickerDims, true, false)
	c := expectCheck(t, before, RuleGIFStickerDuration, false, false)
	if !strings.Contains(c.Detail, "6000 ms") || !strings.Contains(c.Detail, "100.0 fps") {
		t.Errorf("detail: %s", c.Detail)
	}
	after, _ := lintFix(t, buf.Bytes(), TargetSticker)
	c = expectCheck(t, after, RuleGIFStickerDuration, false, false)
	if !strings.Contains(c.Detail, "60000 ms") || strings.Contains(c.Detail, "fps exceeds") {
		t.Errorf("detail after fix: %s", c.Detail)
	}
	if after.DurationMS != 60000 || after.Frames != 600 {
		t.Errorf("after: %+v", after)
	}

	// Frame count check.
	c = stickerDurationCheck(RuleGIFStickerDuration, 1001, 5000)
	if c.OK || !strings.Contains(c.Detail, "1001 frames") {
		t.Errorf("1001 frames: %+v", c)
	}
	c = stickerDurationCheck(RuleGIFStickerDuration, 2, 0)
	if c.OK || !strings.Contains(c.Detail, "0 ms") {
		t.Errorf("zero duration: %+v", c)
	}
	if c = stickerDurationCheck(RuleGIFStickerDuration, 1, 0); !c.OK {
		t.Errorf("single frame: %+v", c)
	}
}

func TestLintGIFEmoteDims(t *testing.T) {
	pm := image.NewPaletted(image.Rect(0, 0, 200, 100), fxPalette)
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{pm}, Delay: []int{10}, Disposal: []byte{1}, Config: image.Config{ColorModel: fxPalette, Width: 200, Height: 100}}); err != nil {
		t.Fatal(err)
	}
	r, _ := lintFix(t, buf.Bytes(), TargetEmote)
	c := expectCheck(t, r, RuleGIFEmoteDims, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "200x100") || !r.OK {
		t.Errorf("check %+v ok=%v", c, r.OK)
	}
	r, _ = lintFix(t, buf.Bytes(), TargetAttachment)
	if hasCheck(r, RuleGIFEmoteDims) || hasCheck(r, RuleGIFStickerDims) {
		t.Errorf("attachment target carries emote/sticker rules: %v", ruleIDs(r))
	}
}

// TestLintGIFEverythingWrong combines many violations in one file and
// checks that a single fix pass repairs them all and only them.
func TestLintGIFEverythingWrong(t *testing.T) {
	a := opaqueFrame0Anim()
	a.loop = -1
	a.frames[1].delay = 0
	a.frames[2].disposal = 0
	data := encodeFx(t, a)
	data = insertAfterGCT(data, []byte{0x21, 0xFE, 0x03, 'a', 'b', 'c', 0x00})
	data = data[:len(data)-1] // no trailer

	before := lintOnly(t, data, TargetEmote)
	for _, rule := range []string{RuleGIFFrame0Transparent, RuleGIFNetscapeLoop, RuleGIFMinDelay, RuleGIFDisposal, RuleGIFNoExtraExtensions, RuleGIFTrailer} {
		expectCheck(t, before, rule, false, false)
	}
	if before.OK {
		t.Error("report OK before fixing")
	}
	after, out := lintFix(t, data, TargetEmote)
	for _, rule := range []string{RuleGIFFrame0Transparent, RuleGIFLSDBackground, RuleGIFNetscapeLoop, RuleGIFMinDelay, RuleGIFDisposal, RuleGIFNoExtraExtensions, RuleGIFTrailer} {
		expectCheck(t, after, rule, true, true)
	}
	if !after.OK {
		t.Error("report not OK after fixing")
	}
	want := opaqueFrame0Anim()
	want.frames[0].transparent = true
	want.frames[2].disposal = 1
	want.bg = fxTransIx
	if !bytes.Equal(out, encodeFx(t, want)) {
		t.Error("fixed bytes differ from the golden image/gif encoding of the corrected animation")
	}
	relint, _ := lintFix(t, out, TargetEmote)
	expectClean(t, relint)
	samePixels(t, encodeFx(t, a), out)
}
