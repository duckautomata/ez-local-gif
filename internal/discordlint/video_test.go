package discordlint

import (
	"encoding/binary"
	"strings"
	"testing"
)

// videoRules is the full mp4 rule list for a Discord target; webm drops
// faststart, TargetNone/unknown drop size-limit and attachment-only.
func videoRules(format string, target Target) []string {
	rules := []string{RuleVideoContainer}
	if format == "mp4" {
		rules = append(rules, RuleVideoFaststart)
	}
	rules = append(rules, RuleVideoCodec, RuleVideoDims, RuleVideoDuration)
	if IsDiscord(target) {
		rules = append(rules, RuleVideoSizeLimit, RuleVideoAttachmentOnly)
	}
	return rules
}

func expectVideoRules(t *testing.T, r Report, format string, target Target) {
	t.Helper()
	want := strings.Join(videoRules(format, target), ",")
	if got := strings.Join(ruleIDs(r), ","); got != want {
		t.Errorf("rules = %v, want %v", got, want)
	}
}

// The committed real-encoder fixtures (see testdata/README.md).
func TestLintVideoFixtures(t *testing.T) {
	for _, tc := range []struct {
		file, format, codecDetail string
	}{
		{"ff_2frame.mp4", "mp4", "H.264 (avc1 sample entry)"},
		{"ff_2frame.webm", "webm", "V_VP9"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data := readFixture(t, tc.file)
			r := lintVideo(t, tc.format, data, TargetAttachment)
			if !r.OK || r.Format != tc.format || r.Target != TargetAttachment || r.Bytes != int64(len(data)) || r.Limit != 20_000_000 {
				t.Errorf("report header: %+v", r)
			}
			if r.Width != 64 || r.Height != 64 || r.Frames != 2 || r.DurationMS < 100 || r.DurationMS > 500 {
				t.Errorf("parsed values: w=%d h=%d frames=%d duration=%d", r.Width, r.Height, r.Frames, r.DurationMS)
			}
			if !r.LoopForever || r.HasAlpha || r.MinDelayMS != 0 || r.RulesVersion != RulesVersion {
				t.Errorf("report flags: %+v", r)
			}
			expectVideoRules(t, r, tc.format, TargetAttachment)
			for _, c := range r.Checks {
				if !c.OK || c.Fixed {
					t.Errorf("%s: ok=%v fixed=%v (%s)", c.Rule, c.OK, c.Fixed, c.Detail)
				}
			}
			if c := findCheck(t, r, RuleVideoCodec); c.Detail != tc.codecDetail {
				t.Errorf("codec detail = %q, want %q", c.Detail, tc.codecDetail)
			}
			if c := findCheck(t, r, RuleVideoDims); !strings.Contains(c.Detail, "64x64, even dimensions") {
				t.Errorf("dims detail = %q", c.Detail)
			}
			if tc.format == "mp4" {
				expectCheck(t, r, RuleVideoFaststart, true, false)
			}
		})
	}
}

// mp4TopBoxes splits the top-level boxes of a well-formed fixture into raw
// byte slices (test-only surgery helper; plain 32-bit sizes only).
func mp4TopBoxes(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var out [][]byte
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 8 || pos+size > len(data) {
			t.Fatalf("fixture box at %d has size %d", pos, size)
		}
		out = append(out, data[pos:pos+size])
		pos += size
	}
	if pos != len(data) {
		t.Fatalf("fixture has %d trailing bytes", len(data)-pos)
	}
	return out
}

// Byte surgery on the real fixture: moving moov after mdat must flip only
// the faststart rule.
func TestLintVideoFaststartSurgery(t *testing.T) {
	data := readFixture(t, "ff_2frame.mp4")
	var moov []byte
	var rest [][]byte
	for _, b := range mp4TopBoxes(t, data) {
		if string(b[4:8]) == "moov" {
			moov = b
			continue
		}
		rest = append(rest, b)
	}
	if moov == nil {
		t.Fatal("fixture has no moov box")
	}
	var slow []byte
	for _, b := range rest {
		slow = append(slow, b...)
	}
	slow = append(slow, moov...)

	r := lintVideo(t, "mp4", slow, TargetAttachment)
	c := expectCheck(t, r, RuleVideoFaststart, false, false)
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "+faststart") || !r.OK {
		t.Errorf("faststart: %+v ok=%v", c, r.OK)
	}
	expectCheck(t, r, RuleVideoContainer, true, false)
	if r.Width != 64 || r.Frames != 2 {
		t.Errorf("moov still parses after the move: %+v", r)
	}
}

func TestLintVideoSyntheticMP4(t *testing.T) {
	cases := []struct {
		name string
		opts mp4Opts
		want func(t *testing.T, r Report)
	}{
		{"good", mp4Opts{}, func(t *testing.T, r Report) {
			if !r.OK || r.Width != 64 || r.Height != 64 || r.Frames != 2 || r.DurationMS != 200 {
				t.Errorf("report: %+v", r)
			}
			if c := findCheck(t, r, RuleVideoDuration); c.Detail != "duration 0.20 s, 2 frames (10.0 fps)" {
				t.Errorf("duration detail = %q", c.Detail)
			}
			expectVideoRules(t, r, "mp4", TargetNone)
		}},
		{"moov-after-mdat", mp4Opts{moovLast: true}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoFaststart, false, false)
			if c.Level != LevelWarn || !r.OK {
				t.Errorf("faststart: %+v ok=%v", c, r.OK)
			}
		}},
		{"no-moov", mp4Opts{noMoov: true}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if c.Level != LevelError || !strings.Contains(c.Detail, "no moov box") || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
			if hasCheck(r, RuleVideoFaststart) {
				t.Error("faststart rule must be omitted without moov")
			}
			if c := expectCheck(t, r, RuleVideoCodec, true, false); c.Level != LevelInfo || !strings.Contains(c.Detail, "no moov box") {
				t.Errorf("codec: %+v", c)
			}
			if c := expectCheck(t, r, RuleVideoDims, true, false); c.Level != LevelInfo {
				t.Errorf("dims: %+v", c)
			}
		}},
		{"no-mdat", mp4Opts{noMdat: true}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if !strings.Contains(c.Detail, "no mdat box") || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
			if hasCheck(r, RuleVideoFaststart) {
				t.Error("faststart rule must be omitted without mdat")
			}
			expectCheck(t, r, RuleVideoCodec, true, false) // moov still parses
		}},
		{"bad-brand", mp4Opts{major: "qt  ", compat: []string{}}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if !strings.Contains(c.Detail, `ftyp brands qt  `) || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
		}},
		{"trailing-garbage", mp4Opts{trailing: []byte("garbage")}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if !strings.Contains(c.Detail, "7 bytes at the end") || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
		}},
		{"wrong-codec", mp4Opts{codec: "vp09"}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoCodec, false, false)
			if c.Level != LevelError || !strings.Contains(c.Detail, "VP9 (vp09)") || !strings.Contains(c.Detail, "must be H.264") || r.OK {
				t.Errorf("codec: %+v ok=%v", c, r.OK)
			}
		}},
		{"odd-width", mp4Opts{w: 65, h: 64}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoDims, false, false)
			if c.Level != LevelWarn || c.Detail != "65x64 has an odd width; yuv420p video should use even dimensions" || !r.OK || r.Width != 65 {
				t.Errorf("dims: %+v ok=%v w=%d", c, r.OK, r.Width)
			}
		}},
		{"odd-both", mp4Opts{w: 65, h: 33}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoDims, false, false)
			if !strings.Contains(c.Detail, "odd width and height") {
				t.Errorf("dims: %+v", c)
			}
		}},
		{"oversize-dims", mp4Opts{w: 4098, h: 64}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoDims, false, false)
			if c.Level != LevelWarn || !strings.Contains(c.Detail, "exceeds 4096 px") || !r.OK {
				t.Errorf("dims: %+v ok=%v", c, r.OK)
			}
		}},
		{"tkhd-fallback-v1", mp4Opts{noStsd: true, tkhdV1: true, tkhdW: 32, tkhdH: 32}, func(t *testing.T, r Report) {
			if r.Width != 32 || r.Height != 32 {
				t.Errorf("v1 tkhd dims: %dx%d", r.Width, r.Height)
			}
			expectCheck(t, r, RuleVideoDims, true, false)
		}},
		{"tkhd-fallback", mp4Opts{noStsd: true, tkhdW: 32, tkhdH: 32}, func(t *testing.T, r Report) {
			if r.Width != 32 || r.Height != 32 {
				t.Errorf("tkhd dims: %dx%d", r.Width, r.Height)
			}
			expectCheck(t, r, RuleVideoDims, true, false)
			if c := expectCheck(t, r, RuleVideoCodec, true, false); c.Level != LevelInfo || !strings.Contains(c.Detail, "no stsd sample entry") {
				t.Errorf("codec: %+v", c)
			}
		}},
		{"dims-unknown", mp4Opts{noStsd: true, noTkhd: true}, func(t *testing.T, r Report) {
			if r.Width != 0 || r.Height != 0 {
				t.Errorf("dims: %dx%d", r.Width, r.Height)
			}
			if c := expectCheck(t, r, RuleVideoDims, true, false); c.Level != LevelInfo || !strings.Contains(c.Detail, "could not be parsed") {
				t.Errorf("dims: %+v", c)
			}
			if !r.OK {
				t.Error("unparsed dimensions must not fail the report")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.want(t, lintVideo(t, "mp4", buildMP4(tc.opts), TargetNone))
		})
	}
}

func TestLintVideoSyntheticWebM(t *testing.T) {
	cases := []struct {
		name string
		opts webmOpts
		want func(t *testing.T, r Report)
	}{
		{"good", webmOpts{}, func(t *testing.T, r Report) {
			if !r.OK || r.Width != 64 || r.Height != 64 || r.Frames != 2 || r.DurationMS != 200 {
				t.Errorf("report: %+v", r)
			}
			c := findCheck(t, r, RuleVideoContainer)
			if !strings.Contains(c.Detail, "DocType webm") || !strings.Contains(c.Detail, "1 cluster") {
				t.Errorf("container detail = %q", c.Detail)
			}
			if c := findCheck(t, r, RuleVideoCodec); c.Detail != "V_VP9" {
				t.Errorf("codec detail = %q", c.Detail)
			}
			expectVideoRules(t, r, "webm", TargetNone)
		}},
		{"matroska-doctype", webmOpts{docType: "matroska"}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if !strings.Contains(c.Detail, `DocType "matroska"`) || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
		}},
		{"no-header", webmOpts{noHeader: true}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if !strings.Contains(c.Detail, "no EBML header") || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
		}},
		{"no-segment", webmOpts{noSegment: true}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if !strings.Contains(c.Detail, "no Segment element") || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
			if c := expectCheck(t, r, RuleVideoCodec, true, false); c.Level != LevelInfo {
				t.Errorf("codec: %+v", c)
			}
		}},
		{"unknown-segment-size", webmOpts{unknownSegSize: true}, func(t *testing.T, r Report) {
			expectCheck(t, r, RuleVideoContainer, true, false)
			if !r.OK || r.Width != 64 || r.Frames != 2 || r.DurationMS != 200 {
				t.Errorf("streamed segment must still parse: %+v", r)
			}
		}},
		{"wrong-codec", webmOpts{codec: "V_VP8"}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoCodec, false, false)
			if c.Level != LevelError || !strings.Contains(c.Detail, "V_VP8") || !strings.Contains(c.Detail, "must be VP9 (V_VP9)") || r.OK {
				t.Errorf("codec: %+v ok=%v", c, r.OK)
			}
		}},
		{"audio-track-first", webmOpts{audioTrack: true}, func(t *testing.T, r Report) {
			if !r.OK || r.Frames != 2 {
				t.Errorf("video track behind an audio track: %+v", r)
			}
			expectCheck(t, r, RuleVideoCodec, true, false)
		}},
		{"no-duration", webmOpts{noDuration: true}, func(t *testing.T, r Report) {
			if r.DurationMS != 0 {
				t.Errorf("duration = %d", r.DurationMS)
			}
			if c := findCheck(t, r, RuleVideoDuration); c.Detail != "2 frames (duration not parsed)" {
				t.Errorf("duration detail = %q", c.Detail)
			}
		}},
		{"no-video-element", webmOpts{w: -1, h: -1}, func(t *testing.T, r Report) {
			if c := expectCheck(t, r, RuleVideoDims, true, false); c.Level != LevelInfo {
				t.Errorf("dims: %+v", c)
			}
			expectCheck(t, r, RuleVideoCodec, true, false) // V_VP9 still found via TrackType
		}},
		{"odd-height", webmOpts{w: 64, h: 65}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoDims, false, false)
			if !strings.Contains(c.Detail, "odd height") || !r.OK {
				t.Errorf("dims: %+v ok=%v", c, r.OK)
			}
		}},
		{"trailing-garbage", webmOpts{trailing: []byte("xx")}, func(t *testing.T, r Report) {
			c := expectCheck(t, r, RuleVideoContainer, false, false)
			if !strings.Contains(c.Detail, "2 bytes at the end") || r.OK {
				t.Errorf("container: %+v ok=%v", c, r.OK)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.want(t, lintVideo(t, "webm", buildWebM(tc.opts), TargetNone))
		})
	}
}

// Targets: emote/sticker fail hard, the attachment tiers pass with their
// own caps, TargetNone and unknown strings carry no target rules.
func TestLintVideoTargets(t *testing.T) {
	mp4Data := buildMP4(mp4Opts{})
	webmData := buildWebM(webmOpts{})
	for _, format := range []string{"mp4", "webm"} {
		data := mp4Data
		if format == "webm" {
			data = webmData
		}
		for _, tg := range []Target{TargetEmote, TargetSticker} {
			r := lintVideo(t, format, data, tg)
			c := expectCheck(t, r, RuleVideoAttachmentOnly, false, false)
			if c.Level != LevelError || r.OK {
				t.Errorf("%s %s: %+v ok=%v", format, tg, c, r.OK)
			}
			if !strings.Contains(c.Detail, "video formats cannot be emotes/stickers") || !strings.Contains(c.Detail, Describe(tg)) {
				t.Errorf("%s %s detail: %q", format, tg, c.Detail)
			}
			expectVideoRules(t, r, format, tg)
		}
		for _, tg := range attachmentTiers {
			r := lintVideo(t, format, data, tg)
			if !r.OK || r.Limit != Limit(tg) {
				t.Errorf("%s %s: ok=%v limit=%d", format, tg, r.OK, r.Limit)
			}
			c := expectCheck(t, r, RuleVideoAttachmentOnly, true, false)
			if !strings.Contains(c.Detail, "uploads as a chat attachment") {
				t.Errorf("%s %s detail: %q", format, tg, c.Detail)
			}
			expectCheck(t, r, RuleVideoSizeLimit, true, false)
			// The tier changes nothing but the cap.
			if got, want := strings.Join(ruleIDs(r), ","), strings.Join(ruleIDs(lintVideo(t, format, data, TargetAttachment)), ","); got != want {
				t.Errorf("%s %s: rules %v differ from the free tier's %v", format, tg, got, want)
			}
		}
		for _, tg := range []Target{TargetNone, "attachment-1000"} {
			r := lintVideo(t, format, data, tg)
			if hasCheck(r, RuleVideoSizeLimit) || hasCheck(r, RuleVideoAttachmentOnly) || r.Limit != 0 || !r.OK {
				t.Errorf("%s %q: %+v", format, tg, ruleIDs(r))
			}
		}
	}

	// The byte cap through a real payload: a >20 MB mdat fails the free
	// tier and passes attachment-500 without touching the container rule.
	big := buildMP4(mp4Opts{mdatLen: 20_000_001})
	r := lintVideo(t, "mp4", big, TargetAttachment)
	if c := expectCheck(t, r, RuleVideoSizeLimit, false, false); c.Level != LevelError || r.OK {
		t.Errorf("oversize: %+v ok=%v", c, r.OK)
	}
	expectCheck(t, r, RuleVideoContainer, true, false)
	r = lintVideo(t, "mp4", big, TargetAttachment500)
	if !r.OK {
		t.Errorf("attachment-500 must take a 20 MB mp4: %+v", r)
	}
}

func TestVideoFormatPlumbing(t *testing.T) {
	for format, want := range map[string]bool{
		"mp4": true, "webm": true, "MP4": true, " webm ": true, "Mp4": true,
		"gif": false, "webp": false, "apng": false, "": false, "m4v": false, "mov": false,
	} {
		if got := IsVideoFormat(format); got != want {
			t.Errorf("IsVideoFormat(%q) = %v, want %v", format, got, want)
		}
	}
	for _, format := range []string{"gif", "webp", "", "mov"} {
		if _, err := LintVideo(format, buildMP4(mp4Opts{}), TargetNone); err == nil {
			t.Errorf("LintVideo(%q) must reject the format", format)
		}
	}
	// Case folding matches LintStatic's convention.
	if r := lintVideo(t, "MP4", buildMP4(mp4Opts{}), TargetNone); r.Format != "mp4" {
		t.Errorf("Format = %q", r.Format)
	}
	for tg, want := range map[Target]bool{
		TargetEmote: false, TargetSticker: false,
		TargetNone: true, TargetAttachment: true, TargetAttachment50: true,
		TargetAttachment100: true, TargetAttachment500: true, "attachment-1000": true,
	} {
		if got := VideoTargetOK(tg); got != want {
			t.Errorf("VideoTargetOK(%q) = %v, want %v", tg, got, want)
		}
	}
}

// Hostile-input smoke tests: tiny corrupt prefixes must not panic and must
// fail the container rule.
func TestLintVideoGarbage(t *testing.T) {
	inputs := [][]byte{
		nil,
		[]byte("x"),
		[]byte("GIF89a"),
		[]byte("\x00\x00\x00\x08ftyp"),
		[]byte("\x1aE\xdf\xa3"),
		buildMP4(mp4Opts{})[:20],
		buildWebM(webmOpts{})[:10],
	}
	for _, format := range []string{"mp4", "webm"} {
		for i, data := range inputs {
			r := lintVideo(t, format, data, TargetAttachment)
			if c := findCheck(t, r, RuleVideoContainer); c.OK {
				t.Errorf("%s input %d: container passed: %q", format, i, c.Detail)
			}
			if r.OK {
				t.Errorf("%s input %d: report OK", format, i)
			}
		}
	}
}
