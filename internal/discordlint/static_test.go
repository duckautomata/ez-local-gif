package discordlint

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestLintStaticFixtures(t *testing.T) {
	cases := []struct {
		file, format string
		w, h         int
		alpha        bool
		desc         string // substring of static.format
	}{
		{"ff_still.png", "png", 64, 64, true, "PNG 64x64, RGBA 8-bit (colour type 6), alpha"},
		{"ff_still_opaque.png", "png", 64, 64, false, "PNG 64x64, RGB 8-bit (colour type 2), no alpha"},
		{"ff_still.jpg", "jpeg", 64, 64, false, "JPEG 64x64, baseline, 8-bit YCbCr (3 components), no alpha"},
		{"ff_still.webp", "webp", 64, 64, false, "WebP 64x64, VP8, no alpha"},
		{"ff_still_alpha.webp", "webp", 64, 64, true, "WebP 64x64, VP8L, alpha"},
		{"ff_still_alpha.avif", "avif", 64, 64, true, "AVIF 64x64, alpha"},
		{"ff_still_opaque.avif", "avif", 64, 64, false, "AVIF 64x64, no alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data := readFixture(t, tc.file)
			r := lintStatic(t, tc.format, data, TargetEmote)
			if r.Format != tc.format || r.Width != tc.w || r.Height != tc.h || r.HasAlpha != tc.alpha || r.Frames != 1 || !r.LoopForever || r.DurationMS != 0 || r.MinDelayMS != 0 {
				t.Errorf("report: %+v", r)
			}
			if r.RulesVersion != RulesVersion || r.Target != TargetEmote || r.Bytes != int64(len(data)) || r.Limit != 262144 || !r.OK {
				t.Errorf("report header: %+v", r)
			}
			c := expectCheck(t, r, RuleStaticFormat, true, false)
			if c.Level != LevelInfo || !strings.Contains(c.Detail, tc.desc) {
				t.Errorf("format detail %q does not contain %q", c.Detail, tc.desc)
			}
			want := []string{RuleStaticFormat, RuleStaticSizeLimit, RuleStaticEmoteDims}
			if got := ruleIDs(r); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("rules = %v, want %v", got, want)
			}
		})
	}
}

func TestLintStaticTargets(t *testing.T) {
	pngData := readFixture(t, "ff_still.png")
	jpg := readFixture(t, "ff_still.jpg")

	// Emote: 64x64 fits; 200x100 warns; the report stays OK.
	if c := findCheck(t, lintStatic(t, "png", pngData, TargetEmote), RuleStaticEmoteDims); !c.OK || c.Level != LevelWarn || c.Detail != "64x64 fits 128x128" {
		t.Errorf("emote 64: %+v", c)
	}
	r := lintStatic(t, "png", patchIHDRDims(t, pngData, 200, 100), TargetEmote)
	if c := expectCheck(t, r, RuleStaticEmoteDims, false, false); c.Level != LevelWarn || c.Detail != "200x100 is larger than 128x128; Discord shrinks emoji to 128x128" || !r.OK {
		t.Errorf("emote 200x100: %+v ok=%v", c, r.OK)
	}

	// Sticker: PNG passes / warns on size; any other format is an error.
	if c := findCheck(t, lintStatic(t, "png", pngData, TargetSticker), RuleStaticSticker); !c.OK || c.Level != LevelError || c.Detail != "PNG 64x64 fits 320x320" {
		t.Errorf("sticker png: %+v", c)
	}
	for _, tc := range []struct {
		w, h   uint32
		ok     bool
		detail string
	}{
		{320, 320, true, "PNG 320x320 fits 320x320"},
		{320, 200, true, "PNG 320x200 fits 320x320"},
		{400, 400, false, "400x400 is larger than 320x320; Discord shrinks stickers to 320x320"},
		{321, 320, false, "321x320 is larger than 320x320; Discord shrinks stickers to 320x320"},
	} {
		r := lintStatic(t, "png", patchIHDRDims(t, pngData, tc.w, tc.h), TargetSticker)
		c := findCheck(t, r, RuleStaticSticker)
		if c.OK != tc.ok || c.Detail != tc.detail || !r.OK {
			t.Errorf("sticker %dx%d: %+v ok=%v", tc.w, tc.h, c, r.OK)
		}
		if !tc.ok && c.Level != LevelWarn {
			t.Errorf("sticker %dx%d: level %s", tc.w, tc.h, c.Level)
		}
	}
	for _, tc := range []struct {
		format string
		data   []byte
		word   string
	}{
		{"jpeg", jpg, "JPEG is not a Discord sticker format"},
		{"webp", readFixture(t, "ff_still_alpha.webp"), "WEBP is not a Discord sticker format"},
		{"avif", readFixture(t, "ff_still_alpha.avif"), "AVIF is not a Discord sticker format"},
	} {
		r := lintStatic(t, tc.format, tc.data, TargetSticker)
		c := expectCheck(t, r, RuleStaticSticker, false, false)
		if c.Level != LevelError || !strings.Contains(c.Detail, tc.word) || r.OK {
			t.Errorf("sticker %s: %+v ok=%v", tc.format, c, r.OK)
		}
	}

	// Attachment: format + size only; TargetNone: format only, no limit.
	if r := lintStatic(t, "jpeg", jpg, TargetAttachment); strings.Join(ruleIDs(r), ",") != RuleStaticFormat+","+RuleStaticSizeLimit || r.Limit != Limit(TargetAttachment) {
		t.Errorf("attachment rules: %v limit=%d", ruleIDs(r), r.Limit)
	}
	if r := lintStatic(t, "jpeg", jpg, TargetNone); strings.Join(ruleIDs(r), ",") != RuleStaticFormat || r.Limit != 0 {
		t.Errorf("none rules: %v limit=%d", ruleIDs(r), r.Limit)
	}
}

func TestLintStaticSizeLimit(t *testing.T) {
	data := readFixture(t, "ff_still.png")
	big := append(append([]byte(nil), data...), make([]byte, 600000)...)
	for _, tc := range []struct {
		target Target
		ok     bool
	}{{TargetEmote, false}, {TargetSticker, false}, {TargetAttachment, true}} {
		r := lintStatic(t, "png", big, tc.target)
		c := expectCheck(t, r, RuleStaticSizeLimit, tc.ok, false)
		if !tc.ok && (c.Level != LevelError || !strings.Contains(c.Detail, "exceeds") || r.OK) {
			t.Errorf("%s: %+v ok=%v", tc.target, c, r.OK)
		}
		if r.Bytes != int64(len(big)) {
			t.Errorf("%s: Bytes %d", tc.target, r.Bytes)
		}
	}
	if c := findCheck(t, lintStatic(t, "png", data, TargetEmote), RuleStaticSizeLimit); c.Detail != "2182 of 262144 bytes" {
		t.Errorf("size detail: %s", c.Detail)
	}
}

// A header that does not parse is reported (static.format warns, dims 0)
// rather than returned as an error; an unknown format name is an error.
func TestLintStaticBadHeader(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "webp", "avif"} {
		for _, data := range [][]byte{nil, []byte("hello"), readFixture(t, "ff_alpha.gif")} {
			r := lintStatic(t, format, data, TargetSticker)
			c := expectCheck(t, r, RuleStaticFormat, false, false)
			if c.Level != LevelWarn || !strings.Contains(c.Detail, strings.ToUpper(format)+" header did not parse") {
				t.Errorf("%s %q: %+v", format, data, c)
			}
			if r.Width != 0 || r.Height != 0 || r.HasAlpha || r.Format != format || r.Frames != 1 {
				t.Errorf("%s: report %+v", format, r)
			}
			// The target rules still report, without asserting a size.
			if format == "png" {
				if c := findCheck(t, r, RuleStaticSticker); !c.OK || !strings.Contains(c.Detail, "dimensions unknown") {
					t.Errorf("png sticker with unknown dims: %+v", c)
				}
			}
		}
	}
	// Wrong format for the bytes: PNG bytes as JPEG.
	if c := findCheck(t, lintStatic(t, "jpeg", readFixture(t, "ff_still.png"), TargetNone), RuleStaticFormat); c.OK || !strings.Contains(c.Detail, "no SOI marker") {
		t.Errorf("png as jpeg: %+v", c)
	}
	if c := findCheck(t, lintStatic(t, "png", readFixture(t, "ff_still.jpg"), TargetEmote), RuleStaticEmoteDims); !c.OK || !strings.Contains(c.Detail, "dimensions unknown") {
		t.Errorf("jpeg as png emote dims: %+v", c)
	}
	if _, err := LintStatic("gif", readFixture(t, "ff_alpha.gif"), TargetNone); err == nil || !strings.Contains(err.Error(), `unsupported format "gif"`) {
		t.Errorf("gif: err=%v", err)
	}
	if _, err := LintStatic("", nil, TargetNone); err == nil {
		t.Error("empty format: want error")
	}
	// Aliases and case are normalised.
	if r := lintStatic(t, "JPG", readFixture(t, "ff_still.jpg"), TargetNone); r.Format != "jpeg" || r.Width != 64 {
		t.Errorf("JPG alias: %+v", r)
	}
	// A PNG with structural problems still yields dims but warns.
	broken := mutatePNG(t, readFixture(t, "ff_still.png"), func(c []rawChunk) []rawChunk { return c[:len(c)-1] })
	r := lintStatic(t, "png", broken, TargetNone)
	if c := findCheck(t, r, RuleStaticFormat); c.OK || !strings.Contains(c.Detail, "no IEND chunk") || r.Width != 64 {
		t.Errorf("broken png: %+v w=%d", c, r.Width)
	}
	// An APNG through the static path is described as animated.
	if c := findCheck(t, lintStatic(t, "png", readFixture(t, "ff_rgba.apng"), TargetNone), RuleStaticFormat); !c.OK || !strings.Contains(c.Detail, "animated (APNG, 10 frames)") {
		t.Errorf("apng as static: %+v", c)
	}
	if c := findCheck(t, lintStatic(t, "webp", readFixture(t, "ff_lossy_alpha.webp"), TargetNone), RuleStaticFormat); !c.OK || !strings.Contains(c.Detail, "(10 frames — not a still)") {
		t.Errorf("animated webp as static: %+v", c)
	}
}

// --- JPEG header walker -------------------------------------------------------

// jpegSegment builds a marker segment with a length field.
func jpegSegment(marker byte, payload []byte) []byte {
	out := []byte{0xFF, marker}
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	return append(out, payload...)
}

func sofPayload(precision byte, w, h uint16, comps byte) []byte {
	p := []byte{precision}
	p = binary.BigEndian.AppendUint16(p, h)
	p = binary.BigEndian.AppendUint16(p, w)
	return append(p, comps)
}

func TestProbeJPEG(t *testing.T) {
	soi := []byte{0xFF, 0xD8}
	app0 := jpegSegment(0xE0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))
	dqt := jpegSegment(0xDB, bytes.Repeat([]byte{1}, 65))
	dht := jpegSegment(0xC4, bytes.Repeat([]byte{2}, 20)) // C4 is DHT, not a SOF
	sos := []byte{0xFF, 0xDA, 0x00, 0x02}

	cases := map[string]struct {
		data          []byte
		w, h          int
		desc, problem string
	}{
		"progressive": {
			data: bytes.Join([][]byte{soi, app0, dqt, dht, jpegSegment(0xC2, sofPayload(8, 640, 480, 3)), sos}, nil),
			w:    640, h: 480, desc: "JPEG 640x480, progressive, 8-bit YCbCr (3 components), no alpha",
		},
		"grey with fill bytes": {
			data: bytes.Join([][]byte{soi, {0xFF, 0xFF, 0xFF}, jpegSegment(0xC0, sofPayload(8, 10, 20, 1)), sos}, nil),
			w:    10, h: 20, desc: "JPEG 10x20, baseline, 8-bit greyscale (1 component), no alpha",
		},
		"cmyk 12-bit extended": {
			data: bytes.Join([][]byte{soi, jpegSegment(0xC1, sofPayload(12, 5, 6, 4)), sos}, nil),
			w:    5, h: 6, desc: "JPEG 5x6, extended sequential, 12-bit CMYK/YCCK (4 components), no alpha",
		},
		"restart marker before SOF": {
			data: bytes.Join([][]byte{soi, {0xFF, 0xD0}, jpegSegment(0xC0, sofPayload(8, 3, 3, 3)), sos}, nil),
			w:    3, h: 3, desc: "JPEG 3x3, baseline",
		},
		"DNL height": {
			data: bytes.Join([][]byte{soi, jpegSegment(0xC0, sofPayload(8, 8, 0, 3)), sos}, nil),
			w:    8, h: 0, desc: "height 0 in SOF (defined later by a DNL marker)",
		},
		"no SOF before SOS": {data: bytes.Join([][]byte{soi, app0, sos}, nil), problem: "no SOF (frame header) before the scan data"},
		"EOI only":          {data: []byte{0xFF, 0xD8, 0xFF, 0xD9}, problem: "no SOF (frame header) before the scan data"},
		"truncated":         {data: bytes.Join([][]byte{soi, app0[:3]}, nil), problem: "truncated marker segment"},
		"overlong segment":  {data: bytes.Join([][]byte{soi, app0[:6]}, nil), problem: "invalid length 16"},
		"bad length":        {data: bytes.Join([][]byte{soi, {0xFF, 0xE0, 0x00, 0x01}}, nil), problem: "invalid length 1"},
		"garbage after SOI": {data: []byte{0xFF, 0xD8, 0x12, 0x34}, problem: "expected a marker at offset 2"},
		"short SOF":         {data: bytes.Join([][]byte{soi, jpegSegment(0xC0, []byte{8, 0}), sos}, nil), problem: "SOF0 segment is 2 bytes"},
		"not jpeg":          {data: []byte("\x89PNG"), problem: "no SOI marker"},
		"ends after SOI":    {data: soi, problem: "no SOF (frame header) found"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			info := probeJPEG(tc.data)
			if info.width != tc.w || info.height != tc.h || info.alpha {
				t.Errorf("dims %dx%d alpha=%v, want %dx%d", info.width, info.height, info.alpha, tc.w, tc.h)
			}
			if tc.problem == "" && (info.problem != "" || !strings.Contains(info.desc, tc.desc)) {
				t.Errorf("problem=%q desc=%q (want desc containing %q)", info.problem, info.desc, tc.desc)
			}
			if tc.problem != "" && !strings.Contains(info.problem, tc.problem) {
				t.Errorf("problem=%q, want containing %q", info.problem, tc.problem)
			}
		})
	}
}

// --- AVIF box walker ------------------------------------------------------------

func mkBox(typ string, payload []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	out = append(out, typ...)
	return append(out, payload...)
}

func mkFullBox(typ string, payload []byte) []byte {
	return mkBox(typ, append([]byte{0, 0, 0, 0}, payload...))
}

func ispeBox(w, h uint32) []byte {
	p := binary.BigEndian.AppendUint32(nil, w)
	return mkFullBox("ispe", binary.BigEndian.AppendUint32(p, h))
}

func ftypBox(major string, compat ...string) []byte {
	p := []byte(major + "\x00\x00\x00\x00")
	for _, c := range compat {
		p = append(p, c...)
	}
	return mkBox("ftyp", p)
}

func metaBox(ipco ...[]byte) []byte {
	return mkFullBox("meta", mkBox("iprp", mkBox("ipco", bytes.Join(ipco, nil))))
}

func TestProbeAVIF(t *testing.T) {
	alphaURN := mkFullBox("auxC", []byte("urn:mpeg:mpegB:cicp:systems:auxiliary:alpha\x00"))
	cases := map[string]struct {
		data    []byte
		w, h    int
		alpha   bool
		desc    string
		problem string
	}{
		"still": {
			data: bytes.Join([][]byte{ftypBox("avif", "mif1"), metaBox(ispeBox(30, 20))}, nil),
			w:    30, h: 20, desc: "AVIF 30x20, no alpha",
		},
		"still with alpha": {
			data: bytes.Join([][]byte{ftypBox("mif1", "avif"), metaBox(ispeBox(30, 20), mkBox("av1C", []byte{1, 2, 3, 4}), ispeBox(30, 20), alphaURN)}, nil),
			w:    30, h: 20, alpha: true, desc: "AVIF 30x20, alpha",
		},
		"hevc alpha urn": {
			data: bytes.Join([][]byte{ftypBox("avif"), metaBox(ispeBox(4, 4), mkFullBox("auxC", []byte("urn:mpeg:hevc:2015:auxid:1\x00")))}, nil),
			w:    4, h: 4, alpha: true,
		},
		"other aux is not alpha": {
			data: bytes.Join([][]byte{ftypBox("avif"), metaBox(ispeBox(4, 4), mkFullBox("auxC", []byte("urn:mpeg:mpegB:cicp:systems:auxiliary:depth\x00")))}, nil),
			w:    4, h: 4, desc: "no alpha",
		},
		"avis sequence": {
			data: bytes.Join([][]byte{ftypBox("avis", "avif", "msf1"), metaBox(ispeBox(320, 320)), mkBox("moov", nil)}, nil),
			w:    320, h: 320, desc: "AVIF 320x320, no alpha (avis brand: image sequence — not a still)",
		},
		"largesize meta": {
			data: func() []byte {
				body := append([]byte{0, 0, 0, 0}, mkBox("iprp", mkBox("ipco", ispeBox(7, 9)))...)
				hdr := binary.BigEndian.AppendUint32(nil, 1)
				hdr = append(hdr, "meta"...)
				hdr = binary.BigEndian.AppendUint64(hdr, uint64(16+len(body)))
				return bytes.Join([][]byte{ftypBox("avif"), hdr, body}, nil)
			}(),
			w: 7, h: 9,
		},
		"size 0 box runs to EOF": {
			data: func() []byte {
				meta := metaBox(ispeBox(2, 3))
				binary.BigEndian.PutUint32(meta[0:4], 0)
				return bytes.Join([][]byte{ftypBox("avif"), meta}, nil)
			}(),
			w: 2, h: 3,
		},
		"no ispe":        {data: bytes.Join([][]byte{ftypBox("avif"), metaBox(mkBox("av1C", []byte{1}))}, nil), problem: "no ispe box"},
		"no meta":        {data: ftypBox("avif"), problem: "no meta box"},
		"no iprp":        {data: bytes.Join([][]byte{ftypBox("avif"), mkFullBox("meta", mkBox("hdlr", nil))}, nil), problem: "no iprp box"},
		"no ipco":        {data: bytes.Join([][]byte{ftypBox("avif"), mkFullBox("meta", mkBox("iprp", mkBox("ipma", nil)))}, nil), problem: "no ipco box"},
		"wrong brand":    {data: bytes.Join([][]byte{ftypBox("heic", "mif1"), metaBox(ispeBox(1, 1))}, nil), problem: "include neither avif nor avis"},
		"not isobmff":    {data: []byte("RIFF....WEBP"), problem: "no ftyp box"},
		"empty":          {data: nil, problem: "no ftyp box"},
		"truncated ftyp": {data: []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'a', 'v'}, problem: "no ftyp box"},
		"truncated meta": {data: bytes.Join([][]byte{ftypBox("avif"), metaBox(ispeBox(5, 5))[:20]}, nil), problem: "no meta box"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			info := probeAVIF(tc.data)
			if info.width != tc.w || info.height != tc.h || info.alpha != tc.alpha {
				t.Errorf("got %dx%d alpha=%v, want %dx%d alpha=%v", info.width, info.height, info.alpha, tc.w, tc.h, tc.alpha)
			}
			if tc.problem == "" && (info.problem != "" || !strings.Contains(info.desc, tc.desc)) {
				t.Errorf("problem=%q desc=%q (want desc containing %q)", info.problem, info.desc, tc.desc)
			}
			if tc.problem != "" && !strings.Contains(info.problem, tc.problem) {
				t.Errorf("problem=%q, want containing %q", info.problem, tc.problem)
			}
		})
	}
	// The real files decode the same way (ispe 64x64; auxC alpha on the
	// avifenc one).
	if info := probeAVIF(readFixture(t, "ff_still_alpha.avif")); info.width != 64 || info.height != 64 || !info.alpha || info.problem != "" {
		t.Errorf("ff_still_alpha.avif: %+v", info)
	}
	if info := probeAVIF(readFixture(t, "ff_still_opaque.avif")); info.width != 64 || info.height != 64 || info.alpha || info.problem != "" {
		t.Errorf("ff_still_opaque.avif: %+v", info)
	}
}
