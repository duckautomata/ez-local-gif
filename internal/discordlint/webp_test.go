package discordlint

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// --- synthetic WebP builders -------------------------------------------------

func chunk(fourcc string, payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload)+1)
	out = append(out, fourcc...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	if len(payload)&1 == 1 {
		out = append(out, 0)
	}
	return out
}

func riff(chunks ...[]byte) []byte {
	body := []byte("WEBP")
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...)
}

func le24bytes(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16)} }

func vp8xChunk(flags byte, w, h int) []byte {
	p := []byte{flags, 0, 0, 0}
	p = append(p, le24bytes(w-1)...)
	p = append(p, le24bytes(h-1)...)
	return chunk("VP8X", p)
}

func animChunk(loop uint16) []byte {
	p := []byte{0xFF, 0xFF, 0xFF, 0xFF} // background colour BGRA
	p = binary.LittleEndian.AppendUint16(p, loop)
	return chunk("ANIM", p)
}

func anmfChunk(x, y, w, h, durMS int, flags byte, sub ...[]byte) []byte {
	p := le24bytes(x / 2)
	p = append(p, le24bytes(y/2)...)
	p = append(p, le24bytes(w-1)...)
	p = append(p, le24bytes(h-1)...)
	p = append(p, le24bytes(durMS)...)
	p = append(p, flags)
	for _, s := range sub {
		p = append(p, s...)
	}
	return chunk("ANMF", p)
}

// vp8lChunk builds a VP8L chunk whose 5-byte header declares w x h and the
// alpha_is_used bit; the rest is filler (never decoded).
func vp8lChunk(w, h int, alpha bool) []byte {
	bits := uint32(w-1) | uint32(h-1)<<14
	if alpha {
		bits |= 1 << 28
	}
	p := []byte{0x2F}
	p = binary.LittleEndian.AppendUint32(p, bits)
	return chunk("VP8L", append(p, 0xAA, 0xBB, 0xCC))
}

// vp8Chunk builds a VP8 chunk with a key-frame header for w x h.
func vp8Chunk(w, h int) []byte {
	p := []byte{0x10, 0x02, 0x00, 0x9D, 0x01, 0x2A} // frame tag (key frame), start code
	p = binary.LittleEndian.AppendUint16(p, uint16(w))
	p = binary.LittleEndian.AppendUint16(p, uint16(h))
	return chunk("VP8 ", append(p, 0x00, 0x00, 0x00, 0x00))
}

func alphChunk() []byte { return chunk("ALPH", []byte{0x00, 1, 2, 3}) }

// goodAnim is a compliant 2-frame animation with alpha (ALPH + VP8, and a
// VP8L frame with alpha_is_used).
func goodAnim() []byte {
	return riff(
		vp8xChunk(vp8xFlagAnim|vp8xFlagAlpha, 64, 48),
		animChunk(0),
		anmfChunk(0, 0, 64, 48, 100, 0, alphChunk(), vp8Chunk(64, 48)),
		anmfChunk(2, 4, 30, 20, 40, anmfFlagNoBlend|anmfFlagDispose, vp8lChunk(30, 20, true)),
	)
}

func lintWebP(t *testing.T, data []byte, target Target) Report {
	t.Helper()
	r, err := LintWebP(data, target)
	if err != nil {
		t.Fatalf("LintWebP: %v", err)
	}
	return r
}

// --- tests -------------------------------------------------------------------

func TestLintWebPFFmpegFixtures(t *testing.T) {
	type want struct {
		frames, w, h, duration, minDelay int
		loop, alpha, ok                  bool
		failed                           []string
	}
	cases := map[string]want{
		"ff_lossy_alpha.webp":    {10, 64, 64, 1000, 100, true, true, true, nil},
		"ff_lossless_alpha.webp": {10, 64, 64, 1000, 100, true, true, true, nil},
		"ff_opaque_anim.webp":    {10, 64, 64, 1000, 100, true, true, true, nil}, // libwebp_anim uses ALPH for frame diffs
		"ff_loop1.webp":          {10, 64, 64, 1000, 100, false, true, false, []string{RuleWebPLoopForever}},
		"ff_still.webp":          {1, 64, 64, 0, 0, true, false, true, nil}, // stills: looping does not apply → LoopForever
		"ff_still_alpha.webp":    {1, 64, 64, 0, 0, true, true, true, nil},
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			r := lintWebP(t, readFixture(t, name), TargetEmote)
			if r.Frames != w.frames || r.Width != w.w || r.Height != w.h || r.DurationMS != w.duration || r.MinDelayMS != w.minDelay {
				t.Errorf("frames=%d %dx%d duration=%d min=%d; want %+v", r.Frames, r.Width, r.Height, r.DurationMS, r.MinDelayMS, w)
			}
			if r.LoopForever != w.loop || r.HasAlpha != w.alpha || r.OK != w.ok {
				t.Errorf("loop=%v alpha=%v ok=%v; want %+v", r.LoopForever, r.HasAlpha, r.OK, w)
			}
			if r.Format != "webp" || r.Target != TargetEmote || r.RulesVersion != RulesVersion || r.Limit != 262144 {
				t.Errorf("header: %+v", r)
			}
			var failed []string
			for _, c := range r.Checks {
				if !c.OK {
					failed = append(failed, c.Rule)
				}
			}
			if strings.Join(failed, ",") != strings.Join(w.failed, ",") {
				t.Errorf("failed rules %v, want %v", failed, w.failed)
			}
			wantRules := []string{RuleWebPRIFF, RuleWebPAnimFlag, RuleWebPAlphaFlag, RuleWebPLoopForever, RuleWebPCanvas, RuleWebPNoMetadata, RuleWebPMinDelay, RuleWebPSizeLimit, RuleWebPEmoteDims}
			if got := ruleIDs(r); strings.Join(got, ",") != strings.Join(wantRules, ",") {
				t.Errorf("rules %v, want %v", got, wantRules)
			}
		})
	}
}

func TestParseWebPFFmpegStructure(t *testing.T) {
	f, err := parseWebP(readFixture(t, "ff_lossy_alpha.webp"))
	if err != nil {
		t.Fatal(err)
	}
	if f.vp8x == nil || f.vp8x.flags != vp8xFlagAnim|vp8xFlagAlpha || f.vp8x.width != 64 || f.vp8x.height != 64 {
		t.Errorf("VP8X: %+v", f.vp8x)
	}
	if f.anim == nil || f.anim.loopCount != 0 || f.anim.bgColor != 0xFFFFFFFF {
		t.Errorf("ANIM: %+v", f.anim)
	}
	if len(f.frames) != 10 || len(f.chunks) != 12 || len(f.problems) != 0 {
		t.Fatalf("frames=%d chunks=%d problems=%v", len(f.frames), len(f.chunks), f.problems)
	}
	f0 := f.frames[0]
	if f0.x != 12 || f0.y != 12 || f0.width != 40 || f0.height != 40 || f0.durationMS != 100 || f0.blend || f0.dispose || !f0.hasALPH || f0.bs == nil || f0.bs.kind != "VP8" || f0.bs.width != 40 || f0.bs.height != 40 {
		t.Errorf("frame 0: %+v bs=%+v", f0, f0.bs)
	}
	if f1 := f.frames[1]; !f1.blend || f1.dispose {
		t.Errorf("frame 1 flags: blend=%v dispose=%v", f1.blend, f1.dispose)
	}
	if f4 := f.frames[4]; f4.blend || !f4.dispose {
		t.Errorf("frame 4 flags: blend=%v dispose=%v", f4.blend, f4.dispose)
	}

	f, err = parseWebP(readFixture(t, "ff_lossless_alpha.webp"))
	if err != nil {
		t.Fatal(err)
	}
	if fr := f.frames[0]; fr.hasALPH || fr.bs.kind != "VP8L" || !fr.bs.alpha || fr.width != 64 || fr.height != 64 {
		t.Errorf("lossless frame 0: %+v bs=%+v", fr, fr.bs)
	}

	f, err = parseWebP(readFixture(t, "ff_still_alpha.webp"))
	if err != nil {
		t.Fatal(err)
	}
	if f.vp8x != nil || f.still == nil || f.still.kind != "VP8L" || !f.still.alpha || f.still.width != 64 || f.still.height != 64 {
		t.Errorf("still alpha: vp8x=%+v still=%+v", f.vp8x, f.still)
	}
	f, err = parseWebP(readFixture(t, "ff_still.webp"))
	if err != nil {
		t.Fatal(err)
	}
	if f.vp8x != nil || f.still == nil || f.still.kind != "VP8" || f.still.alpha || f.still.width != 64 {
		t.Errorf("still: vp8x=%+v still=%+v", f.vp8x, f.still)
	}
}

func TestLintWebPSyntheticCompliant(t *testing.T) {
	r := lintWebP(t, goodAnim(), TargetNone)
	expectClean(t, r)
	if r.Frames != 2 || r.Width != 64 || r.Height != 48 || r.DurationMS != 140 || r.MinDelayMS != 40 || !r.LoopForever || !r.HasAlpha {
		t.Errorf("report: %+v", r)
	}
	if hasCheck(r, RuleWebPSizeLimit) || hasCheck(r, RuleWebPEmoteDims) || hasCheck(r, RuleWebPSticker) {
		t.Errorf("TargetNone carries target rules: %v", ruleIDs(r))
	}
}

func TestLintWebPNotWebP(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("RIFF"), []byte("RIFF\x00\x00\x00\x00WAVE"), []byte("GIF89a....................")} {
		if _, err := LintWebP(data, TargetNone); err == nil {
			t.Errorf("%q accepted", data)
		}
	}
}

func TestLintWebPRIFFProblems(t *testing.T) {
	good := goodAnim()

	t.Run("truncated", func(t *testing.T) {
		r := lintWebP(t, good[:len(good)-5], TargetNone)
		c := expectCheck(t, r, RuleWebPRIFF, false, false)
		if !strings.Contains(c.Detail, "truncated") {
			t.Errorf("detail: %s", c.Detail)
		}
		if r.OK {
			t.Error("report OK for a truncated file")
		}
	})

	t.Run("trailing bytes tolerated", func(t *testing.T) {
		r := lintWebP(t, append(append([]byte(nil), good...), 1, 2, 3, 4), TargetNone)
		c := expectCheck(t, r, RuleWebPRIFF, true, false)
		if !strings.Contains(c.Detail, "4 bytes after the RIFF payload") {
			t.Errorf("detail: %s", c.Detail)
		}
		if !r.OK || r.Frames != 2 {
			t.Errorf("report: ok=%v frames=%d", r.OK, r.Frames)
		}
	})

	t.Run("no image data", func(t *testing.T) {
		r := lintWebP(t, riff(vp8xChunk(0, 8, 8)), TargetNone)
		c := expectCheck(t, r, RuleWebPRIFF, false, false)
		if !strings.Contains(c.Detail, "no image data") {
			t.Errorf("detail: %s", c.Detail)
		}
		if r.Frames != 0 || r.Width != 8 {
			t.Errorf("report: %+v", r)
		}
	})

	t.Run("frames without VP8X and ANIM", func(t *testing.T) {
		r := lintWebP(t, riff(anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)), anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false))), TargetNone)
		c := expectCheck(t, r, RuleWebPRIFF, false, false)
		for _, want := range []string{"without a VP8X chunk", "without an ANIM chunk"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail lacks %q: %s", want, c.Detail)
			}
		}
		expectCheck(t, r, RuleWebPAnimFlag, false, false)
		expectCheck(t, r, RuleWebPLoopForever, false, false)
	})

	t.Run("VP8X not first, short ANIM, stray bytes", func(t *testing.T) {
		data := riff(animChunk(0), vp8xChunk(vp8xFlagAnim, 8, 8), chunk("ANIM", []byte{1, 2}), anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)), anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
		data = append(data, 'x', 'y') // inside the declared RIFF size? no — extend size to include them
		binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
		r := lintWebP(t, data, TargetNone)
		c := expectCheck(t, r, RuleWebPRIFF, false, false)
		for _, want := range []string{"VP8X is chunk 1", "duplicate ANIM chunk", "2 stray bytes"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail lacks %q: %s", want, c.Detail)
			}
		}
	})
}

func TestLintWebPAnimFlag(t *testing.T) {
	// ANIM flag set but a single frame.
	single := riff(vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(0), anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	r := lintWebP(t, single, TargetNone)
	c := expectCheck(t, r, RuleWebPAnimFlag, false, false)
	if !strings.Contains(c.Detail, "1 frame") || r.OK {
		t.Errorf("check %+v ok=%v", c, r.OK)
	}
	// Two frames but flag unset.
	unset := riff(vp8xChunk(0, 8, 8), animChunk(0), anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)), anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	r = lintWebP(t, unset, TargetNone)
	c = expectCheck(t, r, RuleWebPAnimFlag, false, false)
	if !strings.Contains(c.Detail, "unset but the file has 2 frames") {
		t.Errorf("detail: %s", c.Detail)
	}
	// Extended still (VP8X without ANIM) passes.
	still := riff(vp8xChunk(0, 8, 8), vp8lChunk(8, 8, false))
	r = lintWebP(t, still, TargetNone)
	expectCheck(t, r, RuleWebPAnimFlag, true, false)
	expectCheck(t, r, RuleWebPLoopForever, true, false)
	if r.Frames != 1 || !r.OK {
		t.Errorf("extended still: %+v", r)
	}
}

func TestLintWebPAlphaFlag(t *testing.T) {
	// Frames carry alpha (ALPH chunk) but the flag is unset.
	data := riff(vp8xChunk(vp8xFlagAnim, 64, 48), animChunk(0),
		anmfChunk(0, 0, 64, 48, 100, 0, alphChunk(), vp8Chunk(64, 48)),
		anmfChunk(0, 0, 64, 48, 100, 0, vp8Chunk(64, 48)))
	r := lintWebP(t, data, TargetNone)
	c := expectCheck(t, r, RuleWebPAlphaFlag, false, false)
	if !strings.Contains(c.Detail, "ALPHA flag is unset") || !r.HasAlpha || r.OK {
		t.Errorf("check %+v alpha=%v ok=%v", c, r.HasAlpha, r.OK)
	}
	// VP8L alpha_is_used alone counts as alpha.
	data = riff(vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(0),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, true)))
	r = lintWebP(t, data, TargetNone)
	expectCheck(t, r, RuleWebPAlphaFlag, false, false)
	// Flag set but nothing carries alpha.
	data = riff(vp8xChunk(vp8xFlagAnim|vp8xFlagAlpha, 8, 8), animChunk(0),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8Chunk(8, 8)))
	r = lintWebP(t, data, TargetNone)
	c = expectCheck(t, r, RuleWebPAlphaFlag, false, false)
	if !strings.Contains(c.Detail, "no frame carries alpha") || r.HasAlpha {
		t.Errorf("check %+v alpha=%v", c, r.HasAlpha)
	}
	// Extended still with ALPH + VP8 and the flag set.
	data = riff(vp8xChunk(vp8xFlagAlpha, 8, 8), alphChunk(), vp8Chunk(8, 8))
	r = lintWebP(t, data, TargetNone)
	expectClean(t, r)
	if !r.HasAlpha {
		t.Error("HasAlpha false for ALPH+VP8 still")
	}
	// ALPH without VP8X is a container error.
	r = lintWebP(t, riff(alphChunk(), vp8Chunk(8, 8)), TargetNone)
	if c := expectCheck(t, r, RuleWebPRIFF, false, false); !strings.Contains(c.Detail, "ALPH chunk without a VP8X") {
		t.Errorf("detail: %s", c.Detail)
	}
}

func TestLintWebPLoopForever(t *testing.T) {
	// ANIM loop count 3 = plays 3 times: an error for every Discord target …
	data := riff(vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(3),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	for _, target := range []Target{TargetEmote, TargetSticker, TargetAttachment} {
		r := lintWebP(t, data, target)
		c := expectCheck(t, r, RuleWebPLoopForever, false, false)
		if c.Level != LevelError || !strings.Contains(c.Detail, "loop count is 3 (plays 3 times)") || r.LoopForever || r.OK {
			t.Errorf("%s: check %+v loop=%v ok=%v", target, c, r.LoopForever, r.OK)
		}
	}
	// … but for TargetNone the count is the user's choice: passes as an
	// info-level check quoting the count; LoopForever still means count 0.
	r := lintWebP(t, data, TargetNone)
	c := expectCheck(t, r, RuleWebPLoopForever, true, false)
	if c.Level != LevelInfo || !strings.Contains(c.Detail, "loop count 3 (plays 3 times)") || !strings.Contains(c.Detail, "Discord targets require 0") {
		t.Errorf("check %+v", c)
	}
	if r.LoopForever || !r.OK {
		t.Errorf("loop=%v ok=%v", r.LoopForever, r.OK)
	}
	// Count 1 reads "plays once".
	one := riff(vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(1),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	if c := expectCheck(t, lintWebP(t, one, TargetNone), RuleWebPLoopForever, true, false); !strings.Contains(c.Detail, "loop count 1 (plays once)") {
		t.Errorf("detail: %s", c.Detail)
	}
	// Count 0 passes for every target with LoopForever true.
	for _, target := range []Target{TargetNone, TargetEmote, TargetAttachment} {
		r := lintWebP(t, goodAnim(), target)
		if c := expectCheck(t, r, RuleWebPLoopForever, true, false); !r.LoopForever || !strings.Contains(c.Detail, "loop count 0 (loops forever)") {
			t.Errorf("%s: %+v loop=%v", target, c, r.LoopForever)
		}
	}
	// The real ffmpeg -loop 1 file: error for Discord, fine otherwise.
	loop1 := readFixture(t, "ff_loop1.webp")
	if r := lintWebP(t, loop1, TargetAttachment); r.OK || r.LoopForever {
		t.Errorf("ff_loop1 attachment: ok=%v loop=%v", r.OK, r.LoopForever)
	}
	r = lintWebP(t, loop1, TargetNone)
	expectClean(t, r)
	if r.LoopForever {
		t.Error("ff_loop1 TargetNone: LoopForever true for count 1")
	}
	// ANIM flag with no ANIM chunk: structural, an error for every target.
	data = riff(vp8xChunk(vp8xFlagAnim, 8, 8),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	for _, target := range []Target{TargetNone, TargetEmote} {
		r = lintWebP(t, data, target)
		if c := expectCheck(t, r, RuleWebPLoopForever, false, false); c.Level != LevelError || !strings.Contains(c.Detail, "without an ANIM chunk") || r.LoopForever {
			t.Errorf("%s: %+v loop=%v", target, c, r.LoopForever)
		}
	}
}

// Stills and single-frame files have nothing to loop: LoopForever is true
// so the UI shows no "does not loop" badge, and webp.loop-forever passes.
func TestLintWebPStillLoopForever(t *testing.T) {
	cases := map[string][]byte{
		"simple VP8 still":     readFixture(t, "ff_still.webp"),
		"simple VP8L still":    readFixture(t, "ff_still_alpha.webp"),
		"extended still":       riff(vp8xChunk(0, 8, 8), vp8lChunk(8, 8, false)),
		"extended alpha still": riff(vp8xChunk(vp8xFlagAlpha, 8, 8), alphChunk(), vp8Chunk(8, 8)),
	}
	for name, data := range cases {
		for _, target := range []Target{TargetNone, TargetEmote, TargetAttachment} {
			r := lintWebP(t, data, target)
			if !r.LoopForever || r.Frames != 1 {
				t.Errorf("%s/%s: loop=%v frames=%d", name, target, r.LoopForever, r.Frames)
			}
			if c := expectCheck(t, r, RuleWebPLoopForever, true, false); !strings.Contains(c.Detail, "still image") {
				t.Errorf("%s/%s: %+v", name, target, c)
			}
			expectCheck(t, r, RuleWebPMinDelay, true, false)
		}
	}
	// A one-frame animation container (ANIM chunk, single ANMF) with a
	// finite count: the anim-flag rule complains, but looping still does not
	// apply, so LoopForever is true.
	single := riff(vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(1), anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	r := lintWebP(t, single, TargetEmote)
	if !r.LoopForever || r.Frames != 1 {
		t.Errorf("single-frame animation: loop=%v frames=%d", r.LoopForever, r.Frames)
	}
	expectCheck(t, r, RuleWebPAnimFlag, false, false)
	// Two frames with the same count keep meaning "does not loop forever".
	two := riff(vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(1),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	if r := lintWebP(t, two, TargetEmote); r.LoopForever {
		t.Error("two-frame count-1 animation reports LoopForever")
	}
}

func TestLintWebPCanvas(t *testing.T) {
	// Frame extends past the canvas.
	data := riff(vp8xChunk(vp8xFlagAnim, 32, 32), animChunk(0),
		anmfChunk(0, 0, 32, 32, 100, 0, vp8lChunk(32, 32, false)),
		anmfChunk(10, 20, 30, 16, 100, 0, vp8lChunk(30, 16, false)))
	r := lintWebP(t, data, TargetNone)
	c := expectCheck(t, r, RuleWebPCanvas, false, false)
	if !strings.Contains(c.Detail, "frame 1 rect 30x16 at 10,20 lies outside the 32x32 canvas") || r.OK {
		t.Errorf("check %+v ok=%v", c, r.OK)
	}
	// Bitstream dims differ from the ANMF rectangle.
	data = riff(vp8xChunk(vp8xFlagAnim, 32, 32), animChunk(0),
		anmfChunk(0, 0, 32, 32, 100, 0, vp8lChunk(32, 32, false)),
		anmfChunk(0, 0, 32, 32, 100, 0, vp8Chunk(16, 32)))
	r = lintWebP(t, data, TargetNone)
	c = expectCheck(t, r, RuleWebPCanvas, false, false)
	if !strings.Contains(c.Detail, "frame 1 ANMF rect 32x32 differs from its VP8 bitstream 16x32") {
		t.Errorf("detail: %s", c.Detail)
	}
	// Frame without a bitstream and a frame with a bad VP8L signature.
	data = riff(vp8xChunk(vp8xFlagAnim, 32, 32), animChunk(0),
		anmfChunk(0, 0, 32, 32, 100, 0),
		anmfChunk(0, 0, 32, 32, 100, 0, chunk("VP8L", []byte{0x2E, 0, 0, 0, 0})))
	r = lintWebP(t, data, TargetNone)
	c = expectCheck(t, r, RuleWebPCanvas, false, false)
	for _, want := range []string{"frame 0: no VP8/VP8L bitstream", "frame 1 VP8L bitstream: VP8L signature byte is 0x2E"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
	// Extended still whose VP8X canvas disagrees with the bitstream.
	data = riff(vp8xChunk(0, 10, 10), vp8lChunk(8, 8, false))
	r = lintWebP(t, data, TargetNone)
	if c := expectCheck(t, r, RuleWebPCanvas, false, false); !strings.Contains(c.Detail, "VP8X canvas 10x10 differs from the VP8L bitstream 8x8") {
		t.Errorf("detail: %s", c.Detail)
	}
	// Non-key-frame VP8 payload.
	data = riff(chunk("VP8 ", []byte{0x11, 0x02, 0x00, 0x9D, 0x01, 0x2A, 8, 0, 8, 0, 0, 0}))
	r = lintWebP(t, data, TargetNone)
	if c := expectCheck(t, r, RuleWebPCanvas, false, false); !strings.Contains(c.Detail, "not a key frame") {
		t.Errorf("detail: %s", c.Detail)
	}
}

func TestLintWebPMetadata(t *testing.T) {
	data := riff(vp8xChunk(vp8xFlagAnim|vp8xFlagEXIF, 8, 8), animChunk(0),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		chunk("EXIF", []byte("Exif\x00\x00")), chunk("XMP ", []byte("<x/>")), chunk("ICCP", []byte{1, 2, 3}))
	r := lintWebP(t, data, TargetNone)
	c := expectCheck(t, r, RuleWebPNoMetadata, false, false)
	if c.Level != LevelWarn || !r.OK {
		t.Errorf("metadata should be a warning: %+v ok=%v", c, r.OK)
	}
	for _, want := range []string{"EXIF chunk", "XMP chunk", "ICCP chunk", "VP8X EXIF flag"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
}

func TestLintWebPMinDelay(t *testing.T) {
	// anim builds a loop-forever animation with the given frame durations.
	anim := func(durations ...int) []byte {
		chunks := [][]byte{vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(0)}
		for _, d := range durations {
			chunks = append(chunks, anmfChunk(0, 0, 8, 8, d, 0, vp8lChunk(8, 8, false)))
		}
		return riff(chunks...)
	}

	// <= 10 ms: browsers clamp to 100 ms → warning naming exactly those frames.
	r := lintWebP(t, anim(10, 0, 20), TargetNone)
	c := expectCheck(t, r, RuleWebPMinDelay, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "durations <= 10 ms on frames 0, 1 (minimum 0 ms); browsers show them as 100 ms") || !r.OK {
		t.Errorf("check %+v ok=%v", c, r.OK)
	}
	if strings.Contains(c.Detail, "11-19 ms") {
		t.Errorf("no 11-19 ms frames, yet the detail mentions them: %s", c.Detail)
	}
	if r.DurationMS != 30 || r.MinDelayMS != 0 || r.Frames != 3 {
		t.Errorf("report: %+v", r)
	}

	// 11-19 ms: plays as authored → info-level note recommending >= 20 ms.
	for _, d := range []int{11, 15, 19} {
		r = lintWebP(t, anim(d, 20, 100), TargetEmote)
		c = expectCheck(t, r, RuleWebPMinDelay, false, false)
		if c.Level != LevelInfo || !r.OK {
			t.Errorf("%d ms: level %s ok=%v", d, c.Level, r.OK)
		}
		for _, want := range []string{"durations of 11-19 ms on frame 0", ">= 20 ms is recommended", fmt.Sprintf("(minimum %d ms)", d)} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("%d ms: detail lacks %q: %s", d, want, c.Detail)
			}
		}
		if strings.Contains(c.Detail, "browsers show them as 100 ms") {
			t.Errorf("%d ms must not be described as clamped: %s", d, c.Detail)
		}
	}

	// Both kinds present: the warning wins and the note is appended.
	r = lintWebP(t, anim(5, 15, 20), TargetNone)
	c = expectCheck(t, r, RuleWebPMinDelay, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "durations <= 10 ms on frame 0 (minimum 5 ms)") || !strings.Contains(c.Detail, "durations of 11-19 ms on frame 1") {
		t.Errorf("mixed: %+v", c)
	}

	// >= 20 ms passes; the boundary values are exact.
	for _, ds := range [][]int{{20, 20}, {20, 1000}, {21, 33}} {
		r = lintWebP(t, anim(ds...), TargetNone)
		c = expectCheck(t, r, RuleWebPMinDelay, true, false)
		if !strings.Contains(c.Detail, fmt.Sprintf("all frame durations >= 20 ms (minimum %d ms)", ds[0])) {
			t.Errorf("%v: %s", ds, c.Detail)
		}
	}
	// A still has no durations to check.
	expectCheck(t, lintWebP(t, readFixture(t, "ff_still.webp"), TargetNone), RuleWebPMinDelay, true, false)
}

func TestLintWebPTargets(t *testing.T) {
	good := goodAnim()
	r := lintWebP(t, good, TargetSticker)
	c := expectCheck(t, r, RuleWebPSticker, false, false)
	if c.Level != LevelError || r.OK || r.Limit != 524288 {
		t.Errorf("sticker: %+v ok=%v limit=%d", c, r.OK, r.Limit)
	}
	expectCheck(t, r, RuleWebPSizeLimit, true, false)

	r = lintWebP(t, good, TargetEmote)
	expectCheck(t, r, RuleWebPEmoteDims, true, false)
	big := riff(vp8xChunk(0, 200, 100), vp8lChunk(200, 100, false))
	r = lintWebP(t, big, TargetEmote)
	c = expectCheck(t, r, RuleWebPEmoteDims, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "200x100") || !r.OK {
		t.Errorf("emote dims: %+v ok=%v", c, r.OK)
	}
	r = lintWebP(t, big, TargetAttachment)
	if hasCheck(r, RuleWebPEmoteDims) || hasCheck(r, RuleWebPSticker) {
		t.Errorf("attachment carries emote/sticker rules: %v", ruleIDs(r))
	}

	// Size limit: pad the file with an unknown chunk beyond 256 KiB.
	huge := riff(vp8xChunk(0, 8, 8), vp8lChunk(8, 8, false), chunk("JUNK", make([]byte, 262144)))
	r = lintWebP(t, huge, TargetEmote)
	c = expectCheck(t, r, RuleWebPSizeLimit, false, false)
	if !strings.Contains(c.Detail, "exceeds") || r.OK || r.Bytes != int64(len(huge)) {
		t.Errorf("size: %+v ok=%v bytes=%d", c, r.OK, r.Bytes)
	}
	expectCheck(t, r, RuleWebPRIFF, true, false) // unknown chunks are fine
	r = lintWebP(t, huge, TargetSticker)
	expectCheck(t, r, RuleWebPSizeLimit, true, false)
	r = lintWebP(t, huge, TargetNone)
	if hasCheck(r, RuleWebPSizeLimit) {
		t.Error("TargetNone has no byte limit")
	}
}

func TestParseVP8Headers(t *testing.T) {
	bs := parseVP8L([]byte{0x2F, 0x3F, 0x00, 0x00, 0x00})
	if bs.width != 64 || bs.height != 1 || bs.alpha || bs.err != "" {
		t.Errorf("VP8L 64x1: %+v", bs)
	}
	bs = parseVP8L([]byte{0x2F, 0xFF, 0xFF, 0xFF, 0x1F}) // 16384x16384, alpha, version 0
	if bs.width != 16384 || bs.height != 16384 || !bs.alpha || bs.err != "" {
		t.Errorf("VP8L max: %+v", bs)
	}
	bs = parseVP8L([]byte{0x2F, 0x00, 0x00, 0x00, 0x20}) // version 1
	if bs.err == "" || !strings.Contains(bs.err, "version 1") {
		t.Errorf("VP8L version: %+v", bs)
	}
	if bs = parseVP8L([]byte{0x2F, 0x00}); bs.err == "" {
		t.Error("short VP8L accepted")
	}
	bs = parseVP8(bytes.Join([][]byte{{0x30, 0x01, 0x00, 0x9D, 0x01, 0x2A}, {0x40, 0x81, 0xF0, 0x40}}, nil))
	if bs.width != 0x140 || bs.height != 0xF0 || bs.err != "" { // scale bits masked off
		t.Errorf("VP8: %+v", bs)
	}
	if bs = parseVP8([]byte{0x30, 0x01, 0x00, 0x00, 0x00, 0x00, 1, 0, 1, 0}); !strings.Contains(bs.err, "start code") {
		t.Errorf("VP8 start code: %+v", bs)
	}
	if bs = parseVP8([]byte{0x30, 0x01}); bs.err == "" {
		t.Error("short VP8 accepted")
	}
}

func TestRIFFChunkWalker(t *testing.T) {
	// Odd-sized payload is padded; the walker must skip the pad byte.
	body := append(chunk("AAAA", []byte{1, 2, 3}), chunk("BBBB", []byte{4})...)
	chunks, err := riffChunks(body)
	if err != nil || len(chunks) != 2 || chunks[0].fourcc != "AAAA" || len(chunks[0].payload) != 3 || chunks[1].fourcc != "BBBB" || chunks[1].offset != 12 {
		t.Errorf("chunks=%+v err=%v", chunks, err)
	}
	// Declared size beyond the data.
	bad := append([]byte("CCCC"), 0x10, 0, 0, 0, 1, 2)
	chunks, err = riffChunks(bad)
	if err == nil || len(chunks) != 1 || len(chunks[0].payload) != 2 || !strings.Contains(err.Error(), "declares 16 bytes but only 2 remain") {
		t.Errorf("chunks=%+v err=%v", chunks, err)
	}
	if _, err = riffChunks([]byte{1, 2, 3}); err == nil || !strings.Contains(err.Error(), "3 stray bytes") {
		t.Errorf("stray: %v", err)
	}
}
