package discordlint

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// attachmentTiers is every chat-attachment tier, smallest cap first.
var attachmentTiers = []Target{TargetAttachment, TargetAttachment50, TargetAttachment100, TargetAttachment500}

func targetStrings(ts []Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

// The target table: caps per tier, the class helpers, the display order
// and the wording details quote.
func TestTargetTable(t *testing.T) {
	limits := map[Target]int64{
		TargetNone:          0,
		TargetEmote:         262144,
		TargetSticker:       524288,
		TargetAttachment:    20_000_000,
		TargetAttachment50:  50_000_000,
		TargetAttachment100: 100_000_000,
		TargetAttachment500: 500_000_000,
	}
	for tg, want := range limits {
		if got := Limit(tg); got != want {
			t.Errorf("Limit(%q) = %d, want %d", tg, got, want)
		}
	}
	if got := strings.Join(targetStrings(Targets()), ","); got != "emote,sticker,attachment,attachment-50,attachment-100,attachment-500" {
		t.Errorf("Targets() = %s", got)
	}
	if ts := Targets(); len(ts) != len(limits)-1 {
		t.Errorf("Targets() lists %d targets, the limit table has %d Discord targets", len(ts), len(limits)-1)
	}
	// Targets hands out a copy; callers cannot reorder the package's list.
	Targets()[0] = "x"
	if Targets()[0] != TargetEmote {
		t.Error("Targets() returned the package's own slice")
	}
	for i := 1; i < len(attachmentTiers); i++ {
		if Limit(attachmentTiers[i]) <= Limit(attachmentTiers[i-1]) {
			t.Errorf("tier caps must increase: %s %d <= %s %d", attachmentTiers[i], Limit(attachmentTiers[i]), attachmentTiers[i-1], Limit(attachmentTiers[i-1]))
		}
	}
	for _, tg := range Targets() {
		if !IsDiscord(tg) || !Valid(tg) || Limit(tg) <= 0 {
			t.Errorf("%q: discord %v valid %v limit %d", tg, IsDiscord(tg), Valid(tg), Limit(tg))
		}
		if want := strings.HasPrefix(string(tg), "attachment"); IsAttachment(tg) != want {
			t.Errorf("IsAttachment(%q) = %v, want %v", tg, IsAttachment(tg), want)
		}
	}
	if IsDiscord(TargetNone) || IsAttachment(TargetNone) || !Valid(TargetNone) || Describe(TargetNone) != "no Discord target" {
		t.Errorf("TargetNone: discord %v attachment %v valid %v %q", IsDiscord(TargetNone), IsAttachment(TargetNone), Valid(TargetNone), Describe(TargetNone))
	}
	// Comparison is exact: case variants, padding and made-up tiers are
	// unknown — no cap, no class, not valid.
	for _, tg := range []Target{"attachment-1000", "Attachment", "ATTACHMENT-50", " emote", "emote ", "none", "attachment-20"} {
		if Limit(tg) != 0 || IsAttachment(tg) || IsDiscord(tg) || Valid(tg) {
			t.Errorf("%q must be unknown: limit %d attachment %v discord %v valid %v", tg, Limit(tg), IsAttachment(tg), IsDiscord(tg), Valid(tg))
		}
		if d := Describe(tg); d != fmt.Sprintf("unknown target %q", string(tg)) {
			t.Errorf("Describe(%q) = %q", tg, d)
		}
	}
	for tg, want := range map[Target]string{
		TargetEmote:         "emote (256 KiB)",
		TargetSticker:       "sticker (512 KiB)",
		TargetAttachment:    "attachment (20 MB, free tier)",
		TargetAttachment50:  "attachment-50 (50 MB, Nitro Basic or a Level-2 boosted server)",
		TargetAttachment100: "attachment-100 (100 MB, Level-3 boosted server)",
		TargetAttachment500: "attachment-500 (500 MB, Nitro)",
	} {
		if got := Describe(tg); got != want {
			t.Errorf("Describe(%q) = %q, want %q", tg, got, want)
		}
	}
}

// The byte-limit rule at the boundary of every target: the cap itself
// passes, one byte more fails naming the tier and its cap; TargetNone and
// unknown targets have no rule at all.
func TestSizeLimitBoundary(t *testing.T) {
	for _, tg := range Targets() {
		cap := Limit(tg)
		var at, over checkList
		at.sizeLimit("x.size-limit", cap, tg)
		over.sizeLimit("x.size-limit", cap+1, tg)
		if len(at) != 1 || !at[0].OK || at[0].Fixed || at[0].Level != LevelError || at[0].Rule != "x.size-limit" || at[0].Detail != fmt.Sprintf("%d of %d bytes", cap, cap) {
			t.Errorf("%s at the cap: %+v", tg, at)
		}
		if len(over) != 1 || over[0].OK || over[0].Fixed || over[0].Level != LevelError || over[0].Rule != "x.size-limit" {
			t.Errorf("%s over the cap: %+v", tg, over)
			continue
		}
		want := fmt.Sprintf("%d bytes exceeds the %d byte limit for %s", cap+1, cap, Describe(tg))
		if over[0].Detail != want {
			t.Errorf("%s over the cap: detail %q, want %q", tg, over[0].Detail, want)
		}
	}
	for _, tg := range []Target{TargetNone, "attachment-1000"} {
		var none checkList
		none.sizeLimit("x.size-limit", 1<<40, tg)
		if len(none) != 0 {
			t.Errorf("%q has no cap, yet the rule reported: %+v", tg, none)
		}
	}
}

// Through the public linters: a file one byte over the free attachment cap
// fails "attachment" and passes the larger tiers, each report carrying its
// own tier's cap, and no tier carries an emote or sticker rule. The padding
// sits after the GIF trailer / RIFF payload / IEND, which every parser
// ignores, so the files stay compliant.
func TestLintAttachmentTiers(t *testing.T) {
	const size = 20_000_001
	pad := func(data []byte) []byte {
		out := make([]byte, size)
		copy(out, data)
		return out
	}
	gifData := pad(encodeFx(t, opaqueAnim()))
	webpData := pad(goodAnim())
	apngData := pad(readFixture(t, "ff_rgba.apng"))
	pngData := pad(readFixture(t, "ff_still.png"))
	shapeRules := []string{
		RuleGIFEmoteDims, RuleGIFStickerDims, RuleGIFStickerDuration,
		RuleWebPEmoteDims, RuleWebPSticker,
		RuleAPNGNotEmote, RuleAPNGSticker,
		RuleStaticEmoteDims, RuleStaticSticker,
	}
	formats := []struct {
		name, rule string
		lint       func(Target) Report
	}{
		{"gif", RuleGIFSizeLimit, func(tg Target) Report { return lintOnly(t, gifData, tg) }},
		{"webp", RuleWebPSizeLimit, func(tg Target) Report { return lintWebP(t, webpData, tg) }},
		{"apng", RuleAPNGSizeLimit, func(tg Target) Report { return lintAPNG(t, apngData, tg) }},
		{"static", RuleStaticSizeLimit, func(tg Target) Report { return lintStatic(t, "png", pngData, tg) }},
	}
	if Limit(TargetAttachment) >= size {
		t.Fatal("fixture must exceed the free attachment cap")
	}
	for _, f := range formats {
		for _, tg := range attachmentTiers {
			ok := Limit(tg) >= size
			r := f.lint(tg)
			c := expectCheck(t, r, f.rule, ok, false)
			if r.Target != tg || r.Bytes != size || r.Limit != Limit(tg) {
				t.Errorf("%s %s: target %q bytes %d limit %d", f.name, tg, r.Target, r.Bytes, r.Limit)
			}
			if ok {
				if c.Detail != fmt.Sprintf("%d of %d bytes", size, Limit(tg)) || !r.OK {
					t.Errorf("%s %s: %+v ok=%v", f.name, tg, c, r.OK)
				}
			} else if c.Level != LevelError || r.OK || !strings.Contains(c.Detail, "20000001 bytes exceeds the 20000000 byte limit for attachment (20 MB, free tier)") {
				t.Errorf("%s %s: %+v ok=%v", f.name, tg, c, r.OK)
			}
			for _, rule := range shapeRules {
				if hasCheck(r, rule) {
					t.Errorf("%s %s carries %s: %v", f.name, tg, rule, ruleIDs(r))
				}
			}
			// The tier changes nothing but the cap: same rule list as the
			// free tier.
			if got, want := ruleIDs(r), ruleIDs(f.lint(TargetAttachment)); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s %s: rules %v differ from the free tier's %v", f.name, tg, got, want)
			}
		}
	}
}

// Every rule keyed on "a Discord target" or "an attachment" applies to
// each tier alike: GIF loop counts are forced to 0 and local palettes are
// errors, WebP and APNG finite loop counts are errors, and the APNG
// frame-0 note is reported. An unrecognised target string, by contrast, is
// treated like TargetNone.
func TestAttachmentTiersShareRules(t *testing.T) {
	loop3 := patchNetscapeLoop(t, encodeFx(t, opaqueAnim()), 3)
	local := encodeFxWithLocalPalette(t, opaqueAnim(), 1)
	webpLoop3 := riff(vp8xChunk(vp8xFlagAnim, 8, 8), animChunk(3),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)),
		anmfChunk(0, 0, 8, 8, 100, 0, vp8lChunk(8, 8, false)))
	plays1 := readFixture(t, "ff_plays1.apng")
	anim := readFixture(t, "ff_rgba.apng")
	still := readFixture(t, "ff_still.png")

	for _, tg := range attachmentTiers {
		before := lintOnly(t, loop3, tg)
		if c := expectCheck(t, before, RuleGIFNetscapeLoop, false, false); c.Level != LevelError || before.OK || before.LoopForever {
			t.Errorf("%s gif loop before: %+v ok=%v loop=%v", tg, c, before.OK, before.LoopForever)
		}
		after, out := lintFix(t, loop3, tg)
		expectCheck(t, after, RuleGIFNetscapeLoop, true, true)
		if !after.LoopForever || !bytes.Equal(out, encodeFx(t, opaqueAnim())) {
			t.Errorf("%s gif loop after: loop=%v, bytes restored=%v", tg, after.LoopForever, bytes.Equal(out, encodeFx(t, opaqueAnim())))
		}
		r := lintOnly(t, local, tg)
		if c := expectCheck(t, r, RuleGIFGlobalPalette, false, false); c.Level != LevelError || r.OK {
			t.Errorf("%s gif local palette: %+v ok=%v", tg, c, r.OK)
		}

		r = lintWebP(t, webpLoop3, tg)
		if c := expectCheck(t, r, RuleWebPLoopForever, false, false); c.Level != LevelError || r.OK || r.LoopForever {
			t.Errorf("%s webp loop: %+v ok=%v loop=%v", tg, c, r.OK, r.LoopForever)
		}

		r = lintAPNG(t, plays1, tg)
		if c := expectCheck(t, r, RuleAPNGPlays, false, false); c.Level != LevelError || r.OK || r.LoopForever {
			t.Errorf("%s apng plays: %+v ok=%v loop=%v", tg, c, r.OK, r.LoopForever)
		}
		r = lintAPNG(t, anim, tg)
		if c := expectCheck(t, r, RuleAPNGAttachment, false, false); c.Level != LevelInfo || !strings.Contains(c.Detail, "only frame 0") || !r.OK {
			t.Errorf("%s apng attachment anim: %+v ok=%v", tg, c, r.OK)
		}
		if got, want := ruleIDs(r), ruleIDs(lintAPNG(t, anim, TargetAttachment)); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s apng rules %v differ from the free tier's %v", tg, got, want)
		}
		r = lintAPNG(t, still, tg)
		if c := expectCheck(t, r, RuleAPNGAttachment, true, false); c.Level != LevelInfo || !r.OK {
			t.Errorf("%s apng attachment still: %+v ok=%v", tg, c, r.OK)
		}
	}

	// Not a Discord target: the loop rules only report, the palette rule
	// warns, there is no cap and no attachment note.
	const bogus Target = "attachment-1000"
	r := lintOnly(t, loop3, bogus)
	if c := expectCheck(t, r, RuleGIFNetscapeLoop, true, false); !strings.Contains(c.Detail, "loop count 3 (plays 4 times)") || r.LoopForever || !r.OK || r.Limit != 0 || hasCheck(r, RuleGIFSizeLimit) {
		t.Errorf("bogus gif loop: %+v ok=%v loop=%v limit=%d rules=%v", c, r.OK, r.LoopForever, r.Limit, ruleIDs(r))
	}
	if c := expectCheck(t, lintOnly(t, local, bogus), RuleGIFGlobalPalette, false, false); c.Level != LevelWarn {
		t.Errorf("bogus gif local palette: %+v", c)
	}
	if c := expectCheck(t, lintWebP(t, webpLoop3, bogus), RuleWebPLoopForever, true, false); c.Level != LevelInfo {
		t.Errorf("bogus webp loop: %+v", c)
	}
	r = lintAPNG(t, plays1, bogus)
	if c := expectCheck(t, r, RuleAPNGPlays, true, false); c.Level != LevelInfo || hasCheck(r, RuleAPNGAttachment) || hasCheck(r, RuleAPNGSizeLimit) {
		t.Errorf("bogus apng: %+v rules=%v", c, ruleIDs(r))
	}
}
