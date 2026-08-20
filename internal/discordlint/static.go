package discordlint

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Phase 2: static-image rules (DESIGN.md §5.1, §4.2 "Static (n == 1)").

// Static rule ids.
const (
	RuleStaticSizeLimit = "static.size-limit" // error: Bytes <= Limit(target)
	RuleStaticEmoteDims = "static.emote-dims" // warn for emote: <= 128x128
	RuleStaticSticker   = "static.sticker"    // sticker: dims > 320 warn; PNG is the only static sticker format → error for other formats
	RuleStaticFormat    = "static.format"     // info: format/dimensions/alpha summary (warn when the header does not parse)
)

// emoteMaxSide is the size Discord stores emoji at; larger uploads are
// shrunk, so exceeding it only warns.
const emoteMaxSide = 128

// LintStatic evaluates a single image for target. format is "png", "jpeg",
// "webp" or "avif"; dimensions and alpha come from the file header (PNG
// IHDR/tRNS; JPEG SOF; WebP VP8/VP8L/VP8X; AVIF ispe box — best effort, 0
// when unknown). Report.Format = format, Frames 1, LoopForever true.
//
// An error is returned only for an unsupported format name ("jpg" is
// accepted as an alias of "jpeg"); a header that does not parse leaves the
// dimensions at 0 and fails static.format with a warning.
func LintStatic(format string, data []byte, target Target) (Report, error) {
	format = normaliseStaticFormat(format)
	info, err := probeStatic(format, data)
	if err != nil {
		return Report{}, fmt.Errorf("discordlint: %w", err)
	}
	l := &staticLinter{format: format, info: info, size: len(data), target: target}
	l.run()
	return Report{
		RulesVersion: RulesVersion,
		Format:       format,
		Target:       target,
		Bytes:        int64(len(data)),
		Limit:        Limit(target),
		Width:        info.width,
		Height:       info.height,
		Frames:       1,
		LoopForever:  true,
		HasAlpha:     info.alpha,
		Checks:       []Check(l.checks),
		OK:           l.checks.allOK(),
	}, nil
}

// normaliseStaticFormat lower-cases the format name and folds aliases.
func normaliseStaticFormat(format string) string {
	f := strings.ToLower(strings.TrimSpace(format))
	if f == "jpg" {
		return "jpeg"
	}
	return f
}

// staticInfo is what the header probes report.
type staticInfo struct {
	width, height int
	alpha         bool
	desc          string // human-readable summary for static.format
	problem       string // non-empty when the header did not parse
}

// probeStatic reads the header for the named format. It fails only for an
// unknown format name.
func probeStatic(format string, data []byte) (staticInfo, error) {
	switch format {
	case "png":
		return probePNGStatic(data), nil
	case "jpeg":
		return probeJPEG(data), nil
	case "webp":
		return probeWebPStatic(data), nil
	case "avif":
		return probeAVIF(data), nil
	}
	return staticInfo{}, fmt.Errorf("static: unsupported format %q (want png, jpeg, webp or avif)", format)
}

// staticLinter evaluates the static rules.
type staticLinter struct {
	format string
	info   staticInfo
	size   int
	target Target
	checks checkList
}

func (l *staticLinter) run() {
	l.ruleFormat()
	l.ruleSizeLimit()
	switch l.target {
	case TargetEmote:
		l.ruleEmoteDims()
	case TargetSticker:
		l.ruleSticker()
	}
}

// ruleFormat summarises the header; it fails with a warning when the
// header could not be read.
func (l *staticLinter) ruleFormat() {
	const rule = RuleStaticFormat
	if l.info.problem != "" {
		l.checks.fail(rule, LevelWarn, fmt.Sprintf("%s header did not parse: %s", strings.ToUpper(l.format), l.info.problem))
		return
	}
	l.checks.pass(rule, LevelInfo, l.info.desc)
}

func (l *staticLinter) ruleSizeLimit() {
	const rule = RuleStaticSizeLimit
	limit := Limit(l.target)
	if limit <= 0 {
		return
	}
	if int64(l.size) > limit {
		l.checks.fail(rule, LevelError, fmt.Sprintf("%d bytes exceeds the %d byte limit for %s", l.size, limit, l.target))
		return
	}
	l.checks.pass(rule, LevelError, fmt.Sprintf("%d of %d bytes", l.size, limit))
}

// ruleEmoteDims warns when the image is larger than 128x128.
func (l *staticLinter) ruleEmoteDims() {
	const rule = RuleStaticEmoteDims
	w, h := l.info.width, l.info.height
	switch {
	case w == 0 || h == 0:
		l.checks.pass(rule, LevelWarn, "dimensions unknown; could not check the 128x128 emoji size")
	case w <= emoteMaxSide && h <= emoteMaxSide:
		l.checks.pass(rule, LevelWarn, fmt.Sprintf("%dx%d fits %dx%d", w, h, emoteMaxSide, emoteMaxSide))
	default:
		l.checks.fail(rule, LevelWarn, fmt.Sprintf("%dx%d is larger than %dx%d; Discord shrinks emoji to %dx%d", w, h, emoteMaxSide, emoteMaxSide, emoteMaxSide, emoteMaxSide))
	}
}

// ruleSticker: static stickers must be PNG (error otherwise); a side over
// 320 px only warns (Discord shrinks larger stickers).
func (l *staticLinter) ruleSticker() {
	const rule = RuleStaticSticker
	if l.format != "png" {
		l.checks.fail(rule, LevelError, fmt.Sprintf("%s is not a Discord sticker format; static stickers must be PNG (animated: APNG or GIF)", strings.ToUpper(l.format)))
		return
	}
	w, h := l.info.width, l.info.height
	dims := stickerDimsDetail(w, h)
	switch {
	case w == 0 || h == 0:
		l.checks.pass(rule, LevelError, "PNG; dimensions unknown, could not check the 320x320 sticker size")
	case dims != "":
		l.checks.fail(rule, LevelWarn, dims)
	default:
		l.checks.pass(rule, LevelError, fmt.Sprintf("PNG %dx%d fits %dx%d", w, h, stickerMaxSide, stickerMaxSide))
	}
}

// ---------------------------------------------------------------------------
// Header probes

// probePNGStatic reads IHDR/tRNS (and notes an acTL) through the PNG parser.
func probePNGStatic(data []byte) staticInfo {
	f, err := parsePNG(data)
	if err != nil {
		return staticInfo{problem: "not a PNG file (bad signature)"}
	}
	if f.ihdr == nil {
		return staticInfo{problem: "no IHDR chunk"}
	}
	w, h := f.dims()
	info := staticInfo{width: w, height: h, alpha: f.hasAlpha()}
	info.desc = fmt.Sprintf("PNG %dx%d, %s, %s", w, h, f.colourDescription(), alphaWord(info.alpha))
	if f.animated() {
		info.desc += fmt.Sprintf("; animated (APNG, %s) — only the default image counts as a still", plural(f.frameCount(), "frame"))
	}
	if len(f.problems) > 0 {
		info.problem = strings.Join(f.problems, "; ")
	}
	return info
}

// probeWebPStatic reads the canvas and alpha flags through the RIFF parser.
func probeWebPStatic(data []byte) staticInfo {
	f, err := parseWebP(data)
	if err != nil {
		return staticInfo{problem: "not a RIFF/WEBP file"}
	}
	w, h := f.canvas()
	info := staticInfo{width: w, height: h, alpha: f.hasAlpha()}
	kind := "VP8X"
	switch {
	case f.still != nil:
		kind = f.still.kind
	case len(f.frames) > 0:
		kind = "animated"
	}
	info.desc = fmt.Sprintf("WebP %dx%d, %s, %s", w, h, kind, alphaWord(info.alpha))
	if n := f.frameCount(); n > 1 {
		info.desc += fmt.Sprintf(" (%d frames — not a still)", n)
	}
	if len(f.problems) > 0 {
		info.problem = strings.Join(f.problems, "; ")
	}
	return info
}

// JPEG marker bytes.
const (
	jpegSOI = 0xD8
	jpegEOI = 0xD9
	jpegSOS = 0xDA
	jpegDHT = 0xC4
	jpegJPG = 0xC8
	jpegDAC = 0xCC
)

// probeJPEG walks the marker segments up to SOS and reads the first SOFn
// (C0–CF except C4/C8/CC): precision, height, width, components.
func probeJPEG(data []byte) staticInfo {
	if len(data) < 2 || data[0] != 0xFF || data[1] != jpegSOI {
		return staticInfo{problem: "no SOI marker (not a JPEG file)"}
	}
	pos := 2
	for pos < len(data) {
		if data[pos] != 0xFF {
			return staticInfo{problem: fmt.Sprintf("expected a marker at offset %d", pos)}
		}
		for pos < len(data) && data[pos] == 0xFF { // fill bytes
			pos++
		}
		if pos >= len(data) {
			break
		}
		marker := data[pos]
		pos++
		switch {
		case marker == jpegSOI || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			continue // standalone markers without a length
		case marker == jpegEOI || marker == jpegSOS:
			return staticInfo{problem: "no SOF (frame header) before the scan data"}
		}
		if len(data)-pos < 2 {
			return staticInfo{problem: "truncated marker segment"}
		}
		segLen := int(binary.BigEndian.Uint16(data[pos:]))
		if segLen < 2 || segLen > len(data)-pos {
			return staticInfo{problem: fmt.Sprintf("marker 0xFF%02X at offset %d has an invalid length %d", marker, pos-2, segLen)}
		}
		seg := data[pos+2 : pos+segLen]
		pos += segLen
		if marker >= 0xC0 && marker <= 0xCF && marker != jpegDHT && marker != jpegJPG && marker != jpegDAC {
			return jpegFrameInfo(marker, seg)
		}
	}
	return staticInfo{problem: "no SOF (frame header) found"}
}

// jpegFrameInfo decodes a SOFn payload.
func jpegFrameInfo(marker byte, seg []byte) staticInfo {
	if len(seg) < 6 {
		return staticInfo{problem: fmt.Sprintf("SOF%d segment is %d bytes, need at least 6", marker-0xC0, len(seg))}
	}
	precision := int(seg[0])
	h := int(binary.BigEndian.Uint16(seg[1:3]))
	w := int(binary.BigEndian.Uint16(seg[3:5]))
	comps := int(seg[5])
	mode := "baseline"
	switch marker {
	case 0xC1:
		mode = "extended sequential"
	case 0xC2, 0xC6, 0xCA, 0xCE:
		mode = "progressive"
	case 0xC3, 0xC7, 0xCB, 0xCF:
		mode = "lossless"
	}
	colour := "YCbCr"
	switch comps {
	case 1:
		colour = "greyscale"
	case 4:
		colour = "CMYK/YCCK"
	}
	info := staticInfo{width: w, height: h}
	info.desc = fmt.Sprintf("JPEG %dx%d, %s, %d-bit %s (%s), no alpha", w, h, mode, precision, colour, plural(comps, "component"))
	if h == 0 {
		info.desc += "; height 0 in SOF (defined later by a DNL marker)"
	}
	return info
}

// ISOBMFF box walking for AVIF.

type isoBox struct {
	typ     string
	payload []byte
}

// isoBoxes splits b into sequential boxes (size, type, optional largesize).
// It stops silently at the first malformed box.
func isoBoxes(b []byte) []isoBox {
	var out []isoBox
	pos := 0
	for len(b)-pos >= 8 {
		size := uint64(binary.BigEndian.Uint32(b[pos:]))
		typ := string(b[pos+4 : pos+8])
		hdr := 8
		switch size {
		case 0:
			size = uint64(len(b) - pos)
		case 1:
			if len(b)-pos < 16 {
				return out
			}
			size = binary.BigEndian.Uint64(b[pos+8:])
			hdr = 16
		}
		if size < uint64(hdr) || size > uint64(len(b)-pos) {
			return out
		}
		out = append(out, isoBox{typ: typ, payload: b[pos+hdr : pos+int(size)]})
		pos += int(size)
	}
	return out
}

// findBox returns the first box of the given type.
func findBox(boxes []isoBox, typ string) *isoBox {
	for i := range boxes {
		if boxes[i].typ == typ {
			return &boxes[i]
		}
	}
	return nil
}

// fullBoxPayload strips the 4-byte version/flags of a FullBox.
func fullBoxPayload(b *isoBox) []byte {
	if b == nil || len(b.payload) < 4 {
		return nil
	}
	return b.payload[4:]
}

// Auxiliary-type URNs that mark an alpha plane (MIAF / HEIF).
var avifAlphaAuxTypes = []string{
	"urn:mpeg:mpegB:cicp:systems:auxiliary:alpha",
	"urn:mpeg:hevc:2015:auxid:1",
}

// probeAVIF reads ftyp brands and the first ispe (image spatial extents)
// under meta/iprp/ipco; alpha is an auxC property with an alpha URN. Best
// effort: a grid or multi-item file may report the first item's size.
func probeAVIF(data []byte) staticInfo {
	top := isoBoxes(data)
	ftyp := findBox(top, "ftyp")
	if ftyp == nil || len(ftyp.payload) < 8 {
		return staticInfo{problem: "no ftyp box (not an AVIF/ISOBMFF file)"}
	}
	brands := []string{string(ftyp.payload[0:4])}
	for p := 8; p+4 <= len(ftyp.payload); p += 4 {
		brands = append(brands, string(ftyp.payload[p:p+4]))
	}
	animated := false
	isAVIF := false
	for _, b := range brands {
		switch b {
		case "avif":
			isAVIF = true
		case "avis":
			isAVIF = true
			animated = true
		}
	}
	if !isAVIF {
		return staticInfo{problem: fmt.Sprintf("ftyp brands %s include neither avif nor avis", strings.Join(brands, ","))}
	}
	meta := fullBoxPayload(findBox(top, "meta"))
	if meta == nil {
		return staticInfo{problem: "no meta box"}
	}
	iprp := findBox(isoBoxes(meta), "iprp")
	if iprp == nil {
		return staticInfo{problem: "no iprp box under meta"}
	}
	ipco := findBox(isoBoxes(iprp.payload), "ipco")
	if ipco == nil {
		return staticInfo{problem: "no ipco box under iprp"}
	}
	info := staticInfo{}
	for _, p := range isoBoxes(ipco.payload) {
		switch p.typ {
		case "ispe":
			if info.width == 0 && info.height == 0 {
				if body := fullBoxPayload(&p); len(body) >= 8 {
					info.width = int(binary.BigEndian.Uint32(body[0:4]))
					info.height = int(binary.BigEndian.Uint32(body[4:8]))
				}
			}
		case "auxC":
			if body := fullBoxPayload(&p); body != nil {
				urn, _, _ := strings.Cut(string(body), "\x00")
				for _, want := range avifAlphaAuxTypes {
					if urn == want {
						info.alpha = true
					}
				}
			}
		}
	}
	if info.width == 0 || info.height == 0 {
		info.problem = "no ispe box (dimensions unknown)"
	}
	info.desc = fmt.Sprintf("AVIF %dx%d, %s", info.width, info.height, alphaWord(info.alpha))
	if animated {
		info.desc += " (avis brand: image sequence — not a still)"
	}
	return info
}

// alphaWord words the alpha flag for details.
func alphaWord(alpha bool) string {
	if alpha {
		return "alpha"
	}
	return "no alpha"
}
