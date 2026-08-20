package discordlint

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/png"
	"strings"
	"testing"
	"time"
)

// --- fixtures -----------------------------------------------------------------

func TestLintAPNGFFmpegFixtures(t *testing.T) {
	type want struct {
		format                           string
		frames, w, h, duration, minDelay int
		loop, alpha, okSticker, okNone   bool
		failedSticker                    []string
	}
	cases := map[string]want{
		"ff_rgba.apng":        {"apng", 10, 64, 64, 1000, 100, true, true, true, true, nil},
		"ff_plays1.apng":      {"apng", 10, 64, 64, 1000, 100, false, true, false, true, []string{RuleAPNGPlays}},
		"ff_indexed.apng":     {"apng", 10, 64, 64, 1000, 100, true, true, true, true, nil},
		"ff_still.png":        {"png", 1, 64, 64, 0, 0, true, true, true, true, nil},
		"ff_still_opaque.png": {"png", 1, 64, 64, 0, 0, true, false, true, true, nil},
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			data := readFixture(t, name)
			r := lintAPNG(t, data, TargetSticker)
			if r.Format != w.format || r.Frames != w.frames || r.Width != w.w || r.Height != w.h ||
				r.DurationMS != w.duration || r.MinDelayMS != w.minDelay || r.LoopForever != w.loop || r.HasAlpha != w.alpha {
				t.Errorf("report: %+v", r)
			}
			if r.RulesVersion != RulesVersion || r.Target != TargetSticker || r.Bytes != int64(len(data)) || r.Limit != 524288 {
				t.Errorf("report header: %+v", r)
			}
			if r.OK != w.okSticker {
				t.Errorf("OK = %v, want %v: %v", r.OK, w.okSticker, r.Checks)
			}
			var failed []string
			for _, c := range r.Checks {
				if !c.OK && c.Level == LevelError {
					failed = append(failed, c.Rule)
				}
			}
			if strings.Join(failed, ",") != strings.Join(w.failedSticker, ",") {
				t.Errorf("failed errors = %v, want %v", failed, w.failedSticker)
			}
			for _, c := range r.Checks {
				if c.Fixed {
					t.Errorf("no fixer, yet %s reports Fixed", c.Rule)
				}
			}
			if rn := lintAPNG(t, data, TargetNone); rn.OK != w.okNone || rn.Limit != 0 || hasCheck(rn, RuleAPNGSizeLimit) || hasCheck(rn, RuleAPNGSticker) {
				t.Errorf("TargetNone: ok=%v limit=%d rules=%v", rn.OK, rn.Limit, ruleIDs(rn))
			}
		})
	}
}

// The rule list is stable per target so the UI can render a checklist.
func TestLintAPNGRuleList(t *testing.T) {
	base := []string{RuleAPNGContainer, RuleAPNGPlays, RuleAPNGFirstFrame, RuleAPNGCanvas, RuleAPNGMinDelay, RuleAPNGIndexed}
	cases := []struct {
		target Target
		extra  []string
	}{
		{TargetNone, nil},
		{TargetSticker, []string{RuleAPNGSizeLimit, RuleAPNGSticker}},
		{TargetEmote, []string{RuleAPNGSizeLimit, RuleAPNGNotEmote}},
		{TargetAttachment, []string{RuleAPNGSizeLimit, RuleAPNGAttachment}},
	}
	for _, name := range []string{"ff_rgba.apng", "ff_still.png"} {
		data := readFixture(t, name)
		for _, tc := range cases {
			want := append(append([]string(nil), base...), tc.extra...)
			got := ruleIDs(lintAPNG(t, data, tc.target))
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s %q: rules = %v, want %v", name, tc.target, got, want)
			}
		}
	}
}

// Chunk layout round trip: the linter's parser sees exactly the chunks an
// independent walker sees, in order, and re-serialising them reproduces the
// file byte for byte (ffmpeg's CRCs are correct, so the recomputed ones
// match).
func TestLintAPNGChunkRoundTrip(t *testing.T) {
	for name, wantTypes := range map[string]string{
		"ff_rgba.apng":    "IHDR pHYs acTL fcTL IDAT " + strings.Repeat("fcTL fdAT ", 9) + "IEND",
		"ff_indexed.apng": "IHDR pHYs acTL PLTE tRNS fcTL IDAT " + strings.Repeat("fcTL fdAT ", 9) + "IEND",
		"ff_still.png":    "IHDR pHYs IDAT IEND",
	} {
		data := readFixture(t, name)
		raw := splitChunks(t, data)
		if got := strings.Join(chunkTypes(raw), " "); got != wantTypes {
			t.Errorf("%s: chunk types %q, want %q", name, got, wantTypes)
		}
		if !bytes.Equal(encodePNG(raw), data) {
			t.Errorf("%s: re-serialised chunks differ from the file", name)
		}
		f, err := parsePNG(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.chunks) != len(raw) || len(f.problems) != 0 {
			t.Fatalf("%s: parsePNG saw %d chunks (want %d), problems %v", name, len(f.chunks), len(raw), f.problems)
		}
		for i := range raw {
			if f.chunks[i].typ != raw[i].typ || !bytes.Equal(f.chunks[i].data, raw[i].data) {
				t.Errorf("%s: chunk %d differs (%s vs %s)", name, i, f.chunks[i].typ, raw[i].typ)
			}
		}
		// Offsets point at the length field of each chunk.
		for _, c := range f.chunks {
			if string(data[c.offset+4:c.offset+8]) != c.typ {
				t.Errorf("%s: chunk %s offset %d does not point at its header", name, c.typ, c.offset)
			}
		}
		if f.trailing != 0 || !f.hasIEND {
			t.Errorf("%s: trailing=%d hasIEND=%v", name, f.trailing, f.hasIEND)
		}
	}
	// The fcTL fields of the real fixture are decoded correctly (frame 1 is
	// ffmpeg's 64x56 diff rect at 0,0 with blend-over).
	f, _ := parsePNG(readFixture(t, "ff_rgba.apng"))
	if len(f.frames) != 10 || !f.frames[0].isDefault || f.frames[0].dataChunks != 1 || f.frames[1].dataChunks != 1 {
		t.Fatalf("frames: %+v", f.frames)
	}
	if fr := f.frames[1]; fr.width != 64 || fr.height != 56 || fr.x != 0 || fr.y != 0 || fr.delayNum != 1 || fr.delayDen != 10 || fr.blend != 1 || fr.seq != 1 || fr.isDefault {
		t.Errorf("frame 1: %+v", fr)
	}
	if fr := f.frames[3]; fr.dispose != 2 || fr.y != 4 {
		t.Errorf("frame 3: %+v", fr)
	}
	if f.ihdr.colorType != pngRGBA || f.ihdr.bitDepth != 8 || f.plte != -1 || f.hasTRNS {
		t.Errorf("ihdr/palette: %+v plte=%d trns=%v", f.ihdr, f.plte, f.hasTRNS)
	}
	if got := f.ancillary; len(got) != 1 || got[0] != "pHYs" {
		t.Errorf("ancillary = %v", got)
	}
}

// The byte-surgery helpers produce files image/png (an independent decoder)
// still accepts, so the variants are real PNGs, not just linter bait.
func TestLintAPNGSurgeryKeepsValidPNG(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")
	variants := map[string][]byte{
		"zero-delay": patchFCTL(t, data, 3, func(f []byte) { setDelay(f, 0, 100) }),
		"plays3":     patchACTL(t, data, 10, 3),
		"synthetic":  goodSynthAPNG().bytes(), // dummy IDAT: image/png must at least read the header
	}
	for name, v := range variants {
		cfg, err := png.DecodeConfig(bytes.NewReader(v))
		if err != nil {
			t.Errorf("%s: image/png rejects the header: %v", name, err)
			continue
		}
		if cfg.Width != 64 || cfg.Height != 64 {
			t.Errorf("%s: %dx%d", name, cfg.Width, cfg.Height)
		}
	}
	if _, err := png.Decode(bytes.NewReader(variants["zero-delay"])); err != nil {
		t.Errorf("image/png cannot decode the patched fixture: %v", err)
	}
}

// --- rules --------------------------------------------------------------------

func TestLintAPNGContainerRule(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")
	good := lintAPNG(t, data, TargetSticker)
	if c := expectCheck(t, good, RuleAPNGContainer, true, false); c.Level != LevelError || !strings.Contains(c.Detail, "APNG container OK (24 chunks, 10 frames)") || !strings.Contains(c.Detail, "other chunks: pHYs") {
		t.Errorf("good detail: %+v", c)
	}
	if c := findCheck(t, lintAPNG(t, readFixture(t, "ff_still.png"), TargetNone), RuleAPNGContainer); !strings.Contains(c.Detail, "PNG container OK (4 chunks, no acTL)") {
		t.Errorf("still detail: %+v", c)
	}

	// Not a PNG at all → error, like LintGIF/LintWebP.
	for _, bad := range [][]byte{nil, []byte("GIF89a"), []byte("\x89PNG\r\n\x1a"), []byte("RIFF\x04\x00\x00\x00WEBP")} {
		if _, err := LintAPNG(bad, TargetNone); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}

	cases := map[string]struct {
		mutate func(c []rawChunk) []rawChunk
		want   string
	}{
		"no IEND": {func(c []rawChunk) []rawChunk { return c[:len(c)-1] }, "no IEND chunk"},
		"IHDR not first": {func(c []rawChunk) []rawChunk {
			return append([]rawChunk{c[1]}, append([]rawChunk{c[0]}, c[2:]...)...)
		}, "IHDR is chunk 1, not the first"},
		"acTL after IDAT": {func(c []rawChunk) []rawChunk {
			actl := c[2]
			out := append([]rawChunk(nil), c[:2]...) // IHDR pHYs
			out = append(out, c[3:5]...)             // fcTL IDAT
			out = append(out, actl)
			return append(out, c[5:]...)
		}, "acTL chunk after the first IDAT"},
		"acTL frame count": {func(c []rawChunk) []rawChunk {
			binary.BigEndian.PutUint32(c[2].data[0:4], 12)
			return c
		}, "acTL declares 12 frames but the file has 10 fcTL chunks"},
		"duplicate acTL": {func(c []rawChunk) []rawChunk {
			return append(append([]rawChunk(nil), c[:3]...), c[2:]...)
		}, "duplicate acTL chunk"},
		"bad sequence": {func(c []rawChunk) []rawChunk {
			binary.BigEndian.PutUint32(c[5].data[0:4], 7) // second fcTL: seq 1 → 7
			return c
		}, "fcTL sequence number 7, expected 1"},
		"fdAT without fcTL": {func(c []rawChunk) []rawChunk {
			return append(append([]rawChunk(nil), c[:5]...), c[6:]...) // drop the second fcTL
		}, "fdAT sequence number 2, expected 1; fdAT at chunk 5 follows the default image's fcTL"},
		"frame without fdAT": {func(c []rawChunk) []rawChunk {
			return append(append([]rawChunk(nil), c[:6]...), c[7:]...) // drop the first fdAT
		}, "frame 1 has no fdAT data"},
		"two fcTL before IDAT": {func(c []rawChunk) []rawChunk {
			return append(append([]rawChunk(nil), c[:4]...), c[3:]...)
		}, "more than one fcTL before the first IDAT"},
		"short fcTL": {func(c []rawChunk) []rawChunk {
			c[3].data = c[3].data[:20]
			return c
		}, "fcTL at chunk 3 is 20 bytes, need 26"},
		"short acTL": {func(c []rawChunk) []rawChunk {
			c[2].data = c[2].data[:4]
			return c
		}, "acTL payload is 4 bytes, need 8"},
		"IHDR size": {func(c []rawChunk) []rawChunk {
			c[0].data = c[0].data[:12]
			return c
		}, "IHDR payload is 12 bytes, need 13"},
		"zero canvas": {func(c []rawChunk) []rawChunk {
			binary.BigEndian.PutUint32(c[0].data[0:4], 0)
			return c
		}, "IHDR dimensions 0x64 are outside 1..2147483647"},
		"bad depth": {func(c []rawChunk) []rawChunk {
			c[0].data[8] = 7
			return c
		}, "colour type 6 with bit depth 7 is not a valid PNG combination"},
		"tRNS with RGBA": {func(c []rawChunk) []rawChunk {
			return append([]rawChunk{c[0], {"tRNS", []byte{0, 0, 0, 0, 0, 0}}}, c[1:]...)
		}, "tRNS chunk is not allowed with colour type 6"},
		"no IDAT": {func(c []rawChunk) []rawChunk {
			return append(append([]rawChunk(nil), c[:4]...), c[5:]...)
		}, "no IDAT chunk"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := lintAPNG(t, mutatePNG(t, data, tc.mutate), TargetSticker)
			c := expectCheck(t, r, RuleAPNGContainer, false, false)
			if c.Level != LevelError || !strings.Contains(c.Detail, tc.want) {
				t.Errorf("detail %q does not contain %q", c.Detail, tc.want)
			}
			if r.OK {
				t.Error("report OK despite a container error")
			}
		})
	}

	// Truncation mid-chunk: reported, no panic, and what was parsed is used.
	cut := data[:10000]
	r := lintAPNG(t, cut, TargetSticker)
	c := expectCheck(t, r, RuleAPNGContainer, false, false)
	if !strings.Contains(c.Detail, "truncated file") || !strings.Contains(c.Detail, "no IEND chunk") {
		t.Errorf("truncated detail: %s", c.Detail)
	}
	if r.Frames != 10 || r.Width != 64 || r.Bytes != 10000 {
		t.Errorf("truncated report: %+v", r)
	}

	// Bytes after IEND are ignored, not an error; CRCs are not checked.
	trailing := append(append([]byte(nil), data...), "junk"...)
	r = lintAPNG(t, trailing, TargetSticker)
	if c := expectCheck(t, r, RuleAPNGContainer, true, false); !strings.Contains(c.Detail, "4 bytes after IEND are ignored") {
		t.Errorf("trailing detail: %s", c.Detail)
	}
	badCRC := append([]byte(nil), data...)
	badCRC[len(badCRC)-1] ^= 0xFF // IEND CRC
	badCRC[8+8+13+1] ^= 0xFF      // IHDR CRC (chunk at offset 8: 8 len/type + 13 payload, then 4 CRC bytes)
	if r := lintAPNG(t, badCRC, TargetSticker); !r.OK || !findCheck(t, r, RuleAPNGContainer).OK {
		t.Errorf("bad CRCs must not fail the lint: %+v", r.Checks)
	}

	// Unknown chunks are tolerated and listed.
	withText := mutatePNG(t, data, func(c []rawChunk) []rawChunk {
		return append([]rawChunk{c[0], {"tEXt", []byte("Comment\x00hi")}, {"zzZz", []byte{1, 2, 3}}}, c[1:]...)
	})
	if c := findCheck(t, lintAPNG(t, withText, TargetSticker), RuleAPNGContainer); !c.OK || !strings.Contains(c.Detail, "other chunks: tEXt, zzZz, pHYs") {
		t.Errorf("unknown chunks: %+v", c)
	}
}

// fcTL dispose_op/blend_op out of range and tRNS-before-PLTE ordering are
// container errors: libpng rejects the former outright, and silently
// discards the palette alpha of the latter.
func TestLintAPNGContainerDisposeBlendTRNSOrder(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")
	badDispose := patchFCTL(t, data, 2, func(f []byte) { f[24] = 3 })
	r := lintAPNG(t, badDispose, TargetSticker)
	if c := expectCheck(t, r, RuleAPNGContainer, false, false); c.Level != LevelError || !strings.Contains(c.Detail, "frame 2 fcTL dispose_op 3 is invalid") || r.OK {
		t.Errorf("dispose 3: %+v ok=%v", c, r.OK)
	}
	badBlend := patchFCTL(t, data, 4, func(f []byte) { f[25] = 2 })
	r = lintAPNG(t, badBlend, TargetSticker)
	if c := expectCheck(t, r, RuleAPNGContainer, false, false); c.Level != LevelError || !strings.Contains(c.Detail, "frame 4 fcTL blend_op 2 is invalid") || r.OK {
		t.Errorf("blend 2: %+v ok=%v", c, r.OK)
	}
	// The highest valid values (dispose 2, blend 1) still pass.
	edge := patchFCTL(t, data, 2, func(f []byte) { f[24], f[25] = 2, 1 })
	expectCheck(t, lintAPNG(t, edge, TargetSticker), RuleAPNGContainer, true, false)

	// tRNS before PLTE with indexed colour: decoders silently drop the alpha.
	idx := readFixture(t, "ff_indexed.apng")
	swapPLTETRNS := func(c []rawChunk) []rawChunk {
		p, tr := nthChunk(t, c, "PLTE", 0), nthChunk(t, c, "tRNS", 0)
		c[p], c[tr] = c[tr], c[p]
		return c
	}
	r = lintAPNG(t, mutatePNG(t, idx, swapPLTETRNS), TargetSticker)
	if c := expectCheck(t, r, RuleAPNGContainer, false, false); c.Level != LevelError || !strings.Contains(c.Detail, "tRNS chunk appears before PLTE") || r.OK {
		t.Errorf("tRNS order: %+v ok=%v", c, r.OK)
	}
	// The fixture's own order (PLTE then tRNS) stays clean, and the ordering
	// check is about indexed colour only: RGB with a suggested palette keeps
	// its tRNS whatever the order.
	expectCheck(t, lintAPNG(t, idx, TargetSticker), RuleAPNGContainer, true, false)
	rgb := mutatePNG(t, synthAPNG{w: 4, h: 4, colorType: pngRGB, plte: 4, trns: 6}.bytes(), swapPLTETRNS)
	expectCheck(t, lintAPNG(t, rgb, TargetNone), RuleAPNGContainer, true, false)
}

// A hostile PNG built of very many tiny distinct-typed chunks must lint in
// linear time (the unknown-chunk dedupe was once quadratic) and keep the
// container detail bounded.
func TestLintAPNGManyDistinctChunkTypes(t *testing.T) {
	const n = 120000
	var buf bytes.Buffer
	buf.Write(pngSignature)
	writeChunk := func(typ string, payload []byte) {
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)))
		copy(hdr[4:], typ)
		buf.Write(hdr[:])
		buf.Write(payload)
		buf.Write([]byte{0, 0, 0, 0}) // CRCs are not verified
	}
	ihdr := binary.BigEndian.AppendUint32(nil, 64)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 64)
	ihdr = append(ihdr, 8, pngRGBA, 0, 0, 0)
	writeChunk("IHDR", ihdr)
	for i := 0; i < n; i++ { // all-lowercase types never collide with the known chunks
		writeChunk(string([]byte{'a' + byte(i%26), 'a' + byte(i/26%26), 'a' + byte(i/676%26), 'a' + byte(i/17576%26)}), nil)
	}
	writeChunk("IDAT", []byte{0x78, 0x9C, 0x63, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01})
	writeChunk("IEND", nil)

	start := time.Now()
	r := lintAPNG(t, buf.Bytes(), TargetNone)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("lint took %v; the unknown-chunk dedupe must stay linear", elapsed)
	}
	c := expectCheck(t, r, RuleAPNGContainer, true, false)
	if !strings.Contains(c.Detail, "other chunks: aaaa, baaa") || !strings.Contains(c.Detail, fmt.Sprintf("and %d more types", n-pngMaxAncillaryTypes)) {
		t.Errorf("detail: %.200s", c.Detail)
	}
	if len(c.Detail) > 500 {
		t.Errorf("container detail is %d bytes; the ancillary list must be capped", len(c.Detail))
	}

	// Repeated unknown types are still listed once, in first-appearance order.
	f, err := parsePNG(mutatePNG(t, readFixture(t, "ff_still.png"), func(c []rawChunk) []rawChunk {
		return append([]rawChunk{c[0], {"zzZz", nil}, {"zzZz", nil}}, c[1:]...)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.ancillary, ",") != "zzZz,pHYs" || f.ancillaryMore != 0 {
		t.Errorf("ancillary = %v, more = %d", f.ancillary, f.ancillaryMore)
	}
}

func TestLintAPNGPlaysRule(t *testing.T) {
	good := readFixture(t, "ff_rgba.apng")
	if c := expectCheck(t, lintAPNG(t, good, TargetSticker), RuleAPNGPlays, true, false); c.Detail != "acTL num_plays 0 (loops forever)" {
		t.Errorf("good: %+v", c)
	}
	once := readFixture(t, "ff_plays1.apng")
	for _, target := range []Target{TargetSticker, TargetEmote, TargetAttachment} {
		r := lintAPNG(t, once, target)
		c := expectCheck(t, r, RuleAPNGPlays, false, false)
		if c.Level != LevelError || !strings.Contains(c.Detail, "num_plays is 1 (plays once)") || !strings.Contains(c.Detail, "-plays 0") || r.LoopForever || r.OK {
			t.Errorf("%s: %+v loop=%v ok=%v", target, c, r.LoopForever, r.OK)
		}
	}
	r := lintAPNG(t, once, TargetNone)
	if c := expectCheck(t, r, RuleAPNGPlays, true, false); c.Level != LevelInfo || !strings.Contains(c.Detail, "num_plays 1 (plays once)") || r.LoopForever || !r.OK {
		t.Errorf("TargetNone: %+v loop=%v ok=%v", c, r.LoopForever, r.OK)
	}
	three := patchACTL(t, good, 10, 3)
	if c := findCheck(t, lintAPNG(t, three, TargetSticker), RuleAPNGPlays); c.OK || !strings.Contains(c.Detail, "num_plays is 3 (plays 3 times)") {
		t.Errorf("plays 3: %+v", c)
	}
	// A plain PNG has nothing to loop.
	r = lintAPNG(t, readFixture(t, "ff_still.png"), TargetSticker)
	if c := expectCheck(t, r, RuleAPNGPlays, true, false); !strings.Contains(c.Detail, "plain PNG") || !r.LoopForever {
		t.Errorf("still: %+v loop=%v", c, r.LoopForever)
	}
}

func TestLintAPNGFirstFrameRule(t *testing.T) {
	good := goodSynthAPNG()
	if c := expectCheck(t, lintAPNG(t, good.bytes(), TargetSticker), RuleAPNGFirstFrame, true, false); c.Level != LevelWarn || !strings.Contains(c.Detail, "fcTL before IDAT") {
		t.Errorf("good: %+v", c)
	}
	hidden := good
	hidden.hideDefault = true
	r := lintAPNG(t, hidden.bytes(), TargetSticker)
	expectCheck(t, r, RuleAPNGContainer, true, false) // a hidden default image is valid APNG
	if c := expectCheck(t, r, RuleAPNGFirstFrame, false, false); c.Level != LevelWarn || !strings.Contains(c.Detail, "default image (IDAT) has no fcTL") {
		t.Errorf("hidden: %+v", c)
	}
	if !r.OK || r.Frames != 4 || r.DurationMS != 400 {
		t.Errorf("hidden: warning must not fail the report: ok=%v %+v", r.OK, r)
	}
	// acTL but no fcTL at all.
	none := hidden
	none.frames = nil
	r = lintAPNG(t, none.bytes(), TargetNone)
	expectCheck(t, r, RuleAPNGContainer, false, false) // acTL declares 0 frames
	if c := expectCheck(t, r, RuleAPNGFirstFrame, false, false); !strings.Contains(c.Detail, "no fcTL chunks") {
		t.Errorf("no frames: %+v", c)
	}
	if c := expectCheck(t, lintAPNG(t, readFixture(t, "ff_still.png"), TargetNone), RuleAPNGFirstFrame, true, false); !strings.Contains(c.Detail, "plain PNG") {
		t.Errorf("still: %+v", c)
	}
}

func TestLintAPNGCanvasRule(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")
	if c := expectCheck(t, lintAPNG(t, data, TargetSticker), RuleAPNGCanvas, true, false); c.Detail != "canvas 64x64; all 10 frame rectangles inside it" {
		t.Errorf("good: %+v", c)
	}
	if c := expectCheck(t, lintAPNG(t, readFixture(t, "ff_still.png"), TargetSticker), RuleAPNGCanvas, true, false); c.Detail != "64x64 still" {
		t.Errorf("still: %+v", c)
	}
	cases := map[string]struct {
		data []byte
		want string
	}{
		"outside right":  {patchFCTL(t, data, 2, func(f []byte) { setFrameRect(f, 40, 40, 30, 0) }), "frame 2 rect 40x40 at 30,0 lies outside the 64x64 canvas"},
		"outside bottom": {patchFCTL(t, data, 5, func(f []byte) { setFrameRect(f, 64, 8, 0, 57) }), "frame 5 rect 64x8 at 0,57 lies outside the 64x64 canvas"},
		"huge offset":    {patchFCTL(t, data, 1, func(f []byte) { setFrameRect(f, 1, 1, 0xFFFFFFFF, 0) }), "frame 1 rect 1x1 at 4294967295,0 lies outside"},
		"zero rect":      {patchFCTL(t, data, 4, func(f []byte) { setFrameRect(f, 0, 10, 0, 0) }), "frame 4 rect is 0x10"},
		"default frame":  {patchFCTL(t, data, 0, func(f []byte) { setFrameRect(f, 60, 64, 0, 0) }), "frame 0 (the default image) rect 60x64 at 0,0 must cover the whole 64x64 canvas at 0,0"},
		"default offset": {patchFCTL(t, data, 0, func(f []byte) { setFrameRect(f, 63, 64, 1, 0) }), "frame 0 (the default image) rect 63x64 at 1,0 must cover"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := lintAPNG(t, tc.data, TargetSticker)
			c := expectCheck(t, r, RuleAPNGCanvas, false, false)
			if c.Level != LevelError || !strings.Contains(c.Detail, tc.want) {
				t.Errorf("detail %q does not contain %q", c.Detail, tc.want)
			}
			if r.OK {
				t.Error("report OK")
			}
			// The canvas rule is independent of the container rule.
			expectCheck(t, r, RuleAPNGContainer, true, false)
		})
	}
	// Without an IHDR the canvas is unknown.
	noIHDR := mutatePNG(t, data, func(c []rawChunk) []rawChunk { return c[1:] })
	r := lintAPNG(t, noIHDR, TargetSticker)
	if c := expectCheck(t, r, RuleAPNGCanvas, false, false); !strings.Contains(c.Detail, "no IHDR") || r.Width != 0 || r.Height != 0 {
		t.Errorf("no IHDR: %+v %dx%d", c, r.Width, r.Height)
	}
}

func TestLintAPNGMinDelayRule(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")
	for _, target := range []Target{TargetNone, TargetSticker, TargetAttachment} {
		c := expectCheck(t, lintAPNG(t, data, target), RuleAPNGMinDelay, true, false)
		wantLevel := LevelWarn
		if target == TargetSticker {
			wantLevel = LevelError
		}
		if c.Level != wantLevel || c.Detail != "all frame delays >= 20 ms (minimum 100 ms)" {
			t.Errorf("%q good: %+v", target, c)
		}
	}

	// delay_num 0 on one frame: an error for stickers, a warning otherwise;
	// MinDelayMS reports 0.
	zero := patchFCTL(t, data, 3, func(f []byte) { setDelay(f, 0, 100) })
	for _, tc := range []struct {
		target Target
		level  Level
		ok     bool
	}{{TargetSticker, LevelError, false}, {TargetEmote, LevelWarn, true}, {TargetAttachment, LevelWarn, true}, {TargetNone, LevelWarn, true}} {
		r := lintAPNG(t, zero, tc.target)
		c := expectCheck(t, r, RuleAPNGMinDelay, false, false)
		if c.Level != tc.level || !strings.Contains(c.Detail, "delay 0 on frame 3") || !strings.Contains(c.Detail, "frame rate too small or too large") {
			t.Errorf("%q: %+v", tc.target, c)
		}
		if r.MinDelayMS != 0 || r.DurationMS != 900 {
			t.Errorf("%q: min %d dur %d", tc.target, r.MinDelayMS, r.DurationMS)
		}
		// The emote report still fails for apng.not-emote; judge OK from the
		// sticker/attachment/none reports only.
		if tc.target != TargetEmote && r.OK != tc.ok {
			t.Errorf("%q: OK=%v want %v", tc.target, r.OK, tc.ok)
		}
	}

	// delay_den 0 means 100 per the spec: 1/0 → 10 ms, which browsers clamp
	// to 100 ms — a warning tier, even for stickers.
	den0 := patchFCTL(t, data, 1, func(f []byte) { setDelay(f, 1, 0) })
	r := lintAPNG(t, den0, TargetSticker)
	c := expectCheck(t, r, RuleAPNGMinDelay, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "delays <= 10 ms on frame 1 (minimum 10 ms); browsers show them as 100 ms") || r.MinDelayMS != 10 {
		t.Errorf("den0: %+v min=%d", c, r.MinDelayMS)
	}
	if !r.OK {
		t.Errorf("a clamped delay is only a warning for stickers: %+v", r.Checks)
	}

	// 60 fps delays (1/60 ≈ 16.7 ms) play as authored: only an info note
	// recommending >= 20 ms — a Discord-legal 60 fps sticker must not warn.
	sixty := patchAllFCTL(t, data, func(f []byte) { setDelay(f, 1, 60) })
	for _, target := range []Target{TargetNone, TargetSticker} {
		r := lintAPNG(t, sixty, target)
		c := findCheck(t, r, RuleAPNGMinDelay)
		if c.OK || c.Level != LevelInfo || !strings.Contains(c.Detail, "delays of 11-19 ms on frames 0, 1, 2, 3, 4 and 5 more") || !strings.Contains(c.Detail, ">= 20 ms is recommended") {
			t.Errorf("60 fps %q: %+v", target, c)
		}
		if !r.OK {
			t.Errorf("60 fps %q: the info note must not fail the report: %v", target, r.Checks)
		}
	}
	both := patchFCTL(t, sixty, 2, func(f []byte) { setDelay(f, 0, 60) })
	if c := findCheck(t, lintAPNG(t, both, TargetSticker), RuleAPNGMinDelay); c.OK || c.Level != LevelError || !strings.Contains(c.Detail, "delay 0 on frame 2; ") || !strings.Contains(c.Detail, "delays of 11-19 ms on frames 0, 1, 3, 4, 5 and 4 more") {
		t.Errorf("both: %+v", c)
	}

	// Tier boundaries: 10 ms warns, 11 and 19 ms are info, 20 ms passes
	// (checked below).
	for _, tc := range []struct {
		num   uint16
		level Level
	}{{10, LevelWarn}, {11, LevelInfo}, {19, LevelInfo}} {
		ms := patchAllFCTL(t, data, func(f []byte) { setDelay(f, tc.num, 1000) })
		if c := findCheck(t, lintAPNG(t, ms, TargetSticker), RuleAPNGMinDelay); c.OK || c.Level != tc.level {
			t.Errorf("%d ms: %+v, want level %s", tc.num, c, tc.level)
		}
	}

	// Clamped and short frames together: the warning wins and carries both.
	mixed := patchFCTL(t, patchFCTL(t, data, 1, func(f []byte) { setDelay(f, 5, 1000) }), 2, func(f []byte) { setDelay(f, 15, 1000) })
	if c := findCheck(t, lintAPNG(t, mixed, TargetNone), RuleAPNGMinDelay); c.OK || c.Level != LevelWarn ||
		!strings.Contains(c.Detail, "delays <= 10 ms on frame 1 (minimum 5 ms)") || !strings.Contains(c.Detail, "delays of 11-19 ms on frame 2") {
		t.Errorf("mixed: %+v", c)
	}

	// Exactly 20 ms passes.
	twenty := patchAllFCTL(t, data, func(f []byte) { setDelay(f, 2, 100) })
	if c := findCheck(t, lintAPNG(t, twenty, TargetSticker), RuleAPNGMinDelay); !c.OK {
		t.Errorf("20 ms: %+v", c)
	}
	if c := findCheck(t, lintAPNG(t, readFixture(t, "ff_still.png"), TargetSticker), RuleAPNGMinDelay); !c.OK || c.Detail != "still image; no frame delays" {
		t.Errorf("still: %+v", c)
	}
}

// apng.indexed OK means "indexed 8-bit-alpha APNG" (colour type 3 + PLTE +
// tRNS — the sticker default rung); everything else fails at LevelInfo
// without affecting Report.OK.
func TestLintAPNGIndexedRule(t *testing.T) {
	c := expectCheck(t, lintAPNG(t, readFixture(t, "ff_indexed.apng"), TargetSticker), RuleAPNGIndexed, true, false)
	if c.Level != LevelInfo || c.Detail != "indexed 8-bit-alpha APNG: indexed 8-bit (colour type 3), 256-entry palette, tRNS with 1 entry (8-bit alpha)" {
		t.Errorf("indexed: %+v", c)
	}
	// RGBA is not indexed: the check fails (info) but the report stays OK.
	r := lintAPNG(t, readFixture(t, "ff_rgba.apng"), TargetSticker)
	c = expectCheck(t, r, RuleAPNGIndexed, false, false)
	if c.Level != LevelInfo || c.Detail != "RGBA 8-bit (colour type 6); not an indexed 8-bit-alpha APNG (the sticker default rung)" {
		t.Errorf("rgba: %+v", c)
	}
	if !r.OK {
		t.Errorf("an info-level indexed failure must not block the report: %+v", r.Checks)
	}
	// Indexed without tRNS is opaque, not the 8-bit-alpha rung: also a failed
	// info check.
	opaqueIdx := synthAPNG{w: 8, h: 8, colorType: pngIndexed, plte: 16, animated: true, frames: []synthFrame{{w: 8, h: 8, num: 1, den: 10}, {w: 8, h: 8, num: 1, den: 10}}}
	r = lintAPNG(t, opaqueIdx.bytes(), TargetSticker)
	c = expectCheck(t, r, RuleAPNGIndexed, false, false)
	if c.Level != LevelInfo || c.Detail != "indexed APNG without tRNS (opaque): indexed 8-bit (colour type 3), 16-entry palette" || r.HasAlpha || !r.OK {
		t.Errorf("opaque indexed: %+v alpha=%v ok=%v", c, r.HasAlpha, r.OK)
	}
	// Colour type 3 without PLTE is a container error (and not indexed-OK).
	noPLTE := opaqueIdx
	noPLTE.plte = 0
	rn := lintAPNG(t, noPLTE.bytes(), TargetSticker)
	if c := findCheck(t, rn, RuleAPNGContainer); c.OK || !strings.Contains(c.Detail, "indexed colour (type 3) without a PLTE chunk") {
		t.Errorf("no PLTE: %+v", c)
	}
	expectCheck(t, rn, RuleAPNGIndexed, false, false)
	withTRNS := opaqueIdx
	withTRNS.trns = 5
	r = lintAPNG(t, withTRNS.bytes(), TargetSticker)
	if c := expectCheck(t, r, RuleAPNGIndexed, true, false); !strings.HasPrefix(c.Detail, "indexed 8-bit-alpha APNG:") || !strings.Contains(c.Detail, "tRNS with 5 entries") || !r.HasAlpha {
		t.Errorf("tRNS: %+v alpha=%v", c, r.HasAlpha)
	}
}

// HasAlpha is structural: alpha channel or tRNS.
func TestLintAPNGHasAlpha(t *testing.T) {
	cases := []struct {
		name string
		s    synthAPNG
		want bool
	}{
		{"rgba", synthAPNG{w: 4, h: 4, colorType: pngRGBA}, true},
		{"grey+alpha", synthAPNG{w: 4, h: 4, colorType: pngGrayAlpha}, true},
		{"rgb", synthAPNG{w: 4, h: 4, colorType: pngRGB}, false},
		{"rgb+tRNS", synthAPNG{w: 4, h: 4, colorType: pngRGB, trns: 6}, true},
		{"grey", synthAPNG{w: 4, h: 4, colorType: pngGray}, false},
		{"grey+tRNS", synthAPNG{w: 4, h: 4, colorType: pngGray, trns: 2}, true},
		{"indexed", synthAPNG{w: 4, h: 4, colorType: pngIndexed, plte: 4}, false},
		{"indexed+tRNS", synthAPNG{w: 4, h: 4, colorType: pngIndexed, plte: 4, trns: 2}, true},
	}
	for _, tc := range cases {
		r := lintAPNG(t, tc.s.bytes(), TargetNone)
		if r.HasAlpha != tc.want {
			t.Errorf("%s: HasAlpha=%v want %v", tc.name, r.HasAlpha, tc.want)
		}
		if !r.OK {
			t.Errorf("%s: %v", tc.name, r.Checks)
		}
	}
}

func TestLintAPNGStickerDimsMatrix(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")
	cases := []struct {
		w, h   uint32
		ok     bool
		level  Level
		detail string
	}{
		{320, 320, true, LevelError, "320x320; 10 frames, 1000 ms, 10.0 fps"},
		{320, 200, true, LevelError, "320x200; 10 frames, 1000 ms, 10.0 fps"},
		{64, 64, true, LevelError, "64x64; 10 frames, 1000 ms, 10.0 fps"},
		{400, 400, false, LevelWarn, "400x400 is larger than 320x320; Discord shrinks stickers to 320x320; 10 frames, 1000 ms, 10.0 fps"},
		{321, 320, false, LevelWarn, "321x320 is larger than 320x320; Discord shrinks stickers to 320x320; 10 frames, 1000 ms, 10.0 fps"},
		{320, 321, false, LevelWarn, "320x321 is larger than 320x320; Discord shrinks stickers to 320x320; 10 frames, 1000 ms, 10.0 fps"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dx%d", tc.w, tc.h), func(t *testing.T) {
			r := lintAPNG(t, resizeCanvas(t, data, tc.w, tc.h), TargetSticker)
			want := Check{Rule: RuleAPNGSticker, Level: tc.level, OK: tc.ok, Detail: tc.detail}
			if got := findCheck(t, r, RuleAPNGSticker); got != want {
				t.Errorf("check = %+v, want %+v", got, want)
			}
			if !r.OK || r.Width != int(tc.w) || r.Height != int(tc.h) {
				t.Errorf("report: ok=%v %dx%d (%v)", r.OK, r.Width, r.Height, r.Checks)
			}
		})
	}
	// A plain PNG sticker (static) only has the size to check.
	r := lintAPNG(t, patchIHDRDims(t, readFixture(t, "ff_still.png"), 500, 100), TargetSticker)
	if c := findCheck(t, r, RuleAPNGSticker); c.OK || c.Level != LevelWarn || !strings.HasPrefix(c.Detail, "500x100 is larger than 320x320") || !r.OK {
		t.Errorf("still 500x100: %+v ok=%v", c, r.OK)
	}
}

func TestLintAPNGStickerTimingRule(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")

	// 10 frames at 1 s each = 10 s.
	long := patchAllFCTL(t, data, func(f []byte) { setDelay(f, 1, 1) })
	r := lintAPNG(t, long, TargetSticker)
	c := expectCheck(t, r, RuleAPNGSticker, false, false)
	if c.Level != LevelError || c.Detail != "duration 10000 ms exceeds 5000 ms" || r.OK || r.DurationMS != 10000 {
		t.Errorf("long: %+v ok=%v dur=%d", c, r.OK, r.DurationMS)
	}

	// 100 fps (10 ms delays): fps error plus the min-delay warning.
	fast := patchAllFCTL(t, data, func(f []byte) { setDelay(f, 1, 100) })
	r = lintAPNG(t, fast, TargetSticker)
	c = expectCheck(t, r, RuleAPNGSticker, false, false)
	if c.Level != LevelError || c.Detail != "100.0 fps exceeds 60 fps" || r.OK {
		t.Errorf("fast: %+v ok=%v", c, r.OK)
	}
	if c := findCheck(t, r, RuleAPNGMinDelay); c.OK || c.Level != LevelWarn {
		t.Errorf("fast min-delay: %+v", c)
	}

	// Exactly 60 fps (1/60 s delays) is within the limit: microsecond
	// timing keeps 10 x 16.667 ms from rounding into 62.5 or 58.8 fps.
	sixty := patchAllFCTL(t, data, func(f []byte) { setDelay(f, 1, 60) })
	r = lintAPNG(t, sixty, TargetSticker)
	c = expectCheck(t, r, RuleAPNGSticker, true, false)
	if c.Detail != "64x64; 10 frames, 167 ms, 60.0 fps" || r.DurationMS != 167 || r.MinDelayMS != 17 {
		t.Errorf("60 fps: %+v dur=%d min=%d", c, r.DurationMS, r.MinDelayMS)
	}
	if !r.OK { // the 17 ms delays are only an info note
		t.Errorf("60 fps report: %v", r.Checks)
	}
	if c := findCheck(t, r, RuleAPNGMinDelay); c.OK || c.Level != LevelInfo {
		t.Errorf("60 fps min-delay must be an info note: %+v", c)
	}
	// 1/61 s delays are over the limit.
	if c := findCheck(t, lintAPNG(t, patchAllFCTL(t, data, func(f []byte) { setDelay(f, 1, 61) }), TargetSticker), RuleAPNGSticker); c.OK || !strings.Contains(c.Detail, "61.0 fps exceeds 60 fps") {
		t.Errorf("61 fps: %+v", c)
	}

	// Too many frames: acTL says 1001 (the container rule also complains,
	// since the file has 10 fcTL); Report.Frames follows acTL.
	many := patchACTL(t, data, 1001, 0)
	r = lintAPNG(t, many, TargetSticker)
	if c := expectCheck(t, r, RuleAPNGSticker, false, false); !strings.Contains(c.Detail, "1001 frames exceeds 1000") || r.Frames != 1001 {
		t.Errorf("many: %+v frames=%d", c, r.Frames)
	}
	expectCheck(t, r, RuleAPNGContainer, false, false)

	// Oversized and too long at once: the error wins and the size is noted.
	big := patchAllFCTL(t, resizeCanvas(t, data, 400, 400), func(f []byte) { setDelay(f, 1, 1) })
	if c := findCheck(t, lintAPNG(t, big, TargetSticker), RuleAPNGSticker); c.OK || c.Level != LevelError || c.Detail != "duration 10000 ms exceeds 5000 ms; 400x400 is larger than 320x320; Discord shrinks stickers to 320x320" {
		t.Errorf("big+long: %+v", c)
	}

	// All-zero delays: duration 0, frame rate undefined.
	zero := patchAllFCTL(t, data, func(f []byte) { setDelay(f, 0, 100) })
	if c := findCheck(t, lintAPNG(t, zero, TargetSticker), RuleAPNGSticker); c.OK || !strings.Contains(c.Detail, "total duration is 0 ms (frame rate undefined)") {
		t.Errorf("zero: %+v", c)
	}
}

func TestLintAPNGSizeLimitRule(t *testing.T) {
	data := readFixture(t, "ff_rgba.apng")
	r := lintAPNG(t, data, TargetSticker)
	if c := expectCheck(t, r, RuleAPNGSizeLimit, true, false); c.Detail != fmt.Sprintf("%d of 524288 bytes", len(data)) {
		t.Errorf("detail: %s", c.Detail)
	}
	// Pad past the emote limit with bytes after IEND (still a valid PNG).
	big := append(append([]byte(nil), data...), make([]byte, 262144)...)
	for _, tc := range []struct {
		target Target
		ok     bool
	}{{TargetEmote, false}, {TargetSticker, true}, {TargetAttachment, true}} {
		r := lintAPNG(t, big, tc.target)
		c := expectCheck(t, r, RuleAPNGSizeLimit, tc.ok, false)
		if !tc.ok && !strings.Contains(c.Detail, "exceeds the 262144 byte limit for emote") {
			t.Errorf("%s: %s", tc.target, c.Detail)
		}
		if r.Bytes != int64(len(big)) || r.Limit != Limit(tc.target) {
			t.Errorf("%s: bytes %d limit %d", tc.target, r.Bytes, r.Limit)
		}
	}
	if r := lintAPNG(t, big, TargetNone); hasCheck(r, RuleAPNGSizeLimit) || r.Limit != 0 {
		t.Errorf("TargetNone carries a size limit: %v", ruleIDs(r))
	}
}

func TestLintAPNGEmoteAndAttachmentRules(t *testing.T) {
	anim := readFixture(t, "ff_rgba.apng")
	still := readFixture(t, "ff_still.png")

	r := lintAPNG(t, anim, TargetEmote)
	if c := expectCheck(t, r, RuleAPNGNotEmote, false, false); c.Level != LevelError || !strings.Contains(c.Detail, "not an animated-emoji format") || r.OK {
		t.Errorf("emote anim: %+v ok=%v", c, r.OK)
	}
	r = lintAPNG(t, still, TargetEmote)
	if c := expectCheck(t, r, RuleAPNGNotEmote, true, false); c.Detail != "plain PNG 64x64; static PNG is a valid emoji format" || !r.OK {
		t.Errorf("emote still: %+v ok=%v", c, r.OK)
	}
	r = lintAPNG(t, patchIHDRDims(t, still, 200, 100), TargetEmote)
	if c := findCheck(t, r, RuleAPNGNotEmote); !c.OK || !strings.Contains(c.Detail, "Discord shrinks emoji to 128x128") {
		t.Errorf("emote still 200x100: %+v", c)
	}

	r = lintAPNG(t, anim, TargetAttachment)
	if c := expectCheck(t, r, RuleAPNGAttachment, false, false); c.Level != LevelInfo || !strings.Contains(c.Detail, "only frame 0") || !r.OK {
		t.Errorf("attachment anim: %+v ok=%v", c, r.OK)
	}
	r = lintAPNG(t, still, TargetAttachment)
	if c := expectCheck(t, r, RuleAPNGAttachment, true, false); c.Level != LevelInfo || !r.OK {
		t.Errorf("attachment still: %+v ok=%v", c, r.OK)
	}
	if r := lintAPNG(t, anim, TargetSticker); hasCheck(r, RuleAPNGNotEmote) || hasCheck(r, RuleAPNGAttachment) {
		t.Errorf("sticker carries emote/attachment rules: %v", ruleIDs(r))
	}
}

// Everything wrong at once: every rule reports, nothing panics, OK is false.
func TestLintAPNGEverythingWrong(t *testing.T) {
	s := goodSynthAPNG()
	s.w, s.h = 400, 400
	s.plays = 2
	s.hideDefault = true
	s.frames[1].num = 0
	s.frames[2].x = 390 // 64x16 at 390,48 → outside
	s.frames[3].w, s.frames[3].h = 400, 400
	data := mutatePNG(t, s.bytes(), func(c []rawChunk) []rawChunk { return c[:len(c)-1] }) // drop IEND
	r := lintAPNG(t, data, TargetSticker)
	if r.OK {
		t.Error("report OK")
	}
	for _, rule := range []string{RuleAPNGContainer, RuleAPNGPlays, RuleAPNGFirstFrame, RuleAPNGCanvas, RuleAPNGMinDelay, RuleAPNGSticker} {
		expectCheck(t, r, rule, false, false)
	}
	expectCheck(t, r, RuleAPNGSizeLimit, true, false)
	expectCheck(t, r, RuleAPNGIndexed, false, false) // the synth is RGBA, not indexed 8-bit-alpha
	if r.Frames != 4 || r.Width != 400 || r.LoopForever || r.MinDelayMS != 0 || r.DurationMS != 300 {
		t.Errorf("report: %+v", r)
	}
}
