package discordlint

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// WebP rule ids (stable; referenced by the UI and by report.json).
const (
	RuleWebPRIFF        = "webp.riff"
	RuleWebPAnimFlag    = "webp.anim-flag"
	RuleWebPAlphaFlag   = "webp.alpha-flag"
	RuleWebPLoopForever = "webp.loop-forever"
	RuleWebPCanvas      = "webp.canvas"
	RuleWebPNoMetadata  = "webp.no-metadata"
	RuleWebPMinDelay    = "webp.min-delay"
	RuleWebPSizeLimit   = "webp.size-limit"
	RuleWebPSticker     = "webp.sticker"
	RuleWebPEmoteDims   = "webp.emote-dims"
)

// VP8X feature flags.
const (
	vp8xFlagICC   = 0x20
	vp8xFlagAlpha = 0x10
	vp8xFlagEXIF  = 0x08
	vp8xFlagXMP   = 0x04
	vp8xFlagAnim  = 0x02
)

// ANMF flag bits.
const (
	anmfFlagNoBlend = 0x02 // 1 = overwrite, 0 = alpha-blend onto the canvas
	anmfFlagDispose = 0x01 // 1 = dispose to background colour after display
)

// Frame-duration thresholds for webp.min-delay. Browsers (Blink and Gecko
// alike) replace any duration <= 10 ms with 100 ms, so such frames play ten
// times slower than authored — a warning. 11–19 ms plays as authored but is
// faster than the 20 ms floor GIF (2 cs) and APNG output use, so it only
// earns an informational note recommending >= 20 ms.
const (
	webpClampDelayMS = 10 // <= this is shown as 100 ms
	webpMinDelayMS   = 20 // recommended minimum
)

// LintWebP evaluates the WebP rules for target: RIFF/VP8X present for
// animations; ANIM chunk present for animations, with loop count 0 for
// Discord targets (any count is reported for TargetNone); ALPHA flag set
// iff any frame has alpha; ANIM flag iff more than one frame; canvas dims
// == frame extents and every ANMF inside the canvas; no EXIF/XMP/ICCP;
// frame durations > 10 ms (warn) and ideally >= 20 ms (info); byte limit;
// sticker → always fails (WebP is not a sticker format).
func LintWebP(data []byte, target Target) (Report, error) {
	f, err := parseWebP(data)
	if err != nil {
		return Report{}, fmt.Errorf("discordlint: %w", err)
	}
	l := &webpLinter{f: f, target: target}
	l.run()
	duration, minDelay := f.timing()
	w, h := f.canvas()
	return Report{
		RulesVersion: RulesVersion,
		Format:       "webp",
		Target:       target,
		Bytes:        int64(len(data)),
		Limit:        Limit(target),
		Width:        w,
		Height:       h,
		Frames:       f.frameCount(),
		DurationMS:   duration,
		MinDelayMS:   minDelay,
		// Looping does not apply to a still or a single frame (see
		// Report.LoopForever).
		LoopForever: f.frameCount() <= 1 || (f.anim != nil && f.anim.loopCount == 0),
		HasAlpha:    f.hasAlpha(),
		Checks:      []Check(l.checks),
		OK:          l.checks.allOK(),
	}, nil
}

// webpLinter evaluates the rules against a parsed file.
type webpLinter struct {
	f      *webpFile
	target Target
	checks checkList
}

func (l *webpLinter) run() {
	f := l.f
	frames := f.frameCount()

	// webp.riff — container structure.
	if len(f.problems) == 0 {
		detail := fmt.Sprintf("RIFF/WEBP container OK (%s)", plural(len(f.chunks), "chunk"))
		if f.trailing > 0 {
			detail += fmt.Sprintf("; %d bytes after the RIFF payload are ignored", f.trailing)
		}
		l.checks.pass(RuleWebPRIFF, LevelError, detail)
	} else {
		l.checks.fail(RuleWebPRIFF, LevelError, strings.Join(f.problems, "; "))
	}

	// webp.anim-flag — VP8X ANIM flag iff more than one frame.
	switch {
	case f.vp8x == nil && frames > 1:
		l.checks.fail(RuleWebPAnimFlag, LevelError, "animation frames without a VP8X chunk")
	case f.vp8x == nil:
		l.checks.pass(RuleWebPAnimFlag, LevelError, "simple (non-VP8X) still image")
	default:
		flag := f.vp8x.flags&vp8xFlagAnim != 0
		switch {
		case flag && frames <= 1:
			l.checks.fail(RuleWebPAnimFlag, LevelError, fmt.Sprintf("VP8X ANIM flag set but the file has %s; encode single frames as a still", plural(frames, "frame")))
		case !flag && frames > 1:
			l.checks.fail(RuleWebPAnimFlag, LevelError, fmt.Sprintf("VP8X ANIM flag unset but the file has %d frames", frames))
		case flag:
			l.checks.pass(RuleWebPAnimFlag, LevelError, fmt.Sprintf("ANIM flag set, %d frames", frames))
		default:
			l.checks.pass(RuleWebPAnimFlag, LevelError, "ANIM flag unset, single frame")
		}
	}

	// webp.alpha-flag — VP8X ALPHA flag iff some frame carries alpha
	// (Discord's lilliput drops alpha when the flag is unset).
	alpha := f.hasAlpha()
	switch {
	case f.vp8x == nil && alpha:
		l.checks.pass(RuleWebPAlphaFlag, LevelError, "no VP8X chunk; alpha is signalled by the VP8L bitstream")
	case f.vp8x == nil:
		l.checks.pass(RuleWebPAlphaFlag, LevelError, "no VP8X chunk and no alpha")
	default:
		flag := f.vp8x.flags&vp8xFlagAlpha != 0
		switch {
		case flag && !alpha:
			l.checks.fail(RuleWebPAlphaFlag, LevelError, "VP8X ALPHA flag set but no frame carries alpha (no ALPH chunk, VP8L alpha_is_used unset)")
		case !flag && alpha:
			l.checks.fail(RuleWebPAlphaFlag, LevelError, "frames carry alpha but the VP8X ALPHA flag is unset; Discord drops the alpha channel")
		case flag:
			l.checks.pass(RuleWebPAlphaFlag, LevelError, "ALPHA flag set and frames carry alpha")
		default:
			l.checks.pass(RuleWebPAlphaFlag, LevelError, "ALPHA flag unset and no frame carries alpha")
		}
	}

	// webp.loop-forever — animations carry an ANIM chunk; its loop count is
	// the number of plays (0 = forever). Discord targets require 0 (Discord
	// honours a finite count and the animation stops); for TargetNone any
	// count is the user's choice and is only reported.
	animated := frames > 1 || f.anim != nil || (f.vp8x != nil && f.vp8x.flags&vp8xFlagAnim != 0)
	switch {
	case !animated:
		l.checks.pass(RuleWebPLoopForever, LevelError, "still image; no loop count")
	case f.anim == nil:
		l.checks.fail(RuleWebPLoopForever, LevelError, "animation without an ANIM chunk (no loop count)")
	case f.anim.loopCount == 0:
		l.checks.pass(RuleWebPLoopForever, LevelError, "ANIM loop count 0 (loops forever)")
	case l.target == TargetNone:
		l.checks.pass(RuleWebPLoopForever, LevelInfo, fmt.Sprintf("ANIM loop count %d (%s); Discord targets require 0 (loop forever)", f.anim.loopCount, plays(int(f.anim.loopCount))))
	default:
		l.checks.fail(RuleWebPLoopForever, LevelError, fmt.Sprintf("ANIM loop count is %d (%s); Discord needs 0 = loop forever — encode with -loop 0", f.anim.loopCount, plays(int(f.anim.loopCount))))
	}

	// webp.canvas — canvas dims sane, every frame inside it, bitstream dims
	// match the frame rectangle.
	l.checkCanvas()

	// webp.no-metadata.
	var meta []string
	if f.hasEXIF {
		meta = append(meta, "EXIF chunk")
	}
	if f.hasXMP {
		meta = append(meta, "XMP chunk")
	}
	if f.hasICCP {
		meta = append(meta, "ICCP chunk")
	}
	if f.vp8x != nil {
		for _, fl := range []struct {
			bit  byte
			name string
		}{{vp8xFlagICC, "ICC"}, {vp8xFlagEXIF, "EXIF"}, {vp8xFlagXMP, "XMP"}} {
			if f.vp8x.flags&fl.bit != 0 {
				meta = append(meta, "VP8X "+fl.name+" flag")
			}
		}
	}
	if len(meta) > 0 {
		l.checks.fail(RuleWebPNoMetadata, LevelWarn, "metadata present ("+strings.Join(meta, ", ")+"); Discord's metadata stripper has corrupted animated WebPs — encode with -map_metadata -1")
	} else {
		l.checks.pass(RuleWebPNoMetadata, LevelWarn, "no EXIF/XMP/ICC metadata")
	}

	// webp.min-delay — see webpClampDelayMS / webpMinDelayMS.
	l.checkMinDelay()

	// webp.size-limit.
	if limit := Limit(l.target); limit > 0 {
		if int64(f.size) > limit {
			l.checks.fail(RuleWebPSizeLimit, LevelError, fmt.Sprintf("%d bytes exceeds the %d byte limit for %s", f.size, limit, l.target))
		} else {
			l.checks.pass(RuleWebPSizeLimit, LevelError, fmt.Sprintf("%d of %d bytes", f.size, limit))
		}
	}

	// Target-specific.
	w, h := f.canvas()
	switch l.target {
	case TargetSticker:
		l.checks.fail(RuleWebPSticker, LevelError, "WebP is not a Discord sticker format; use APNG or GIF")
	case TargetEmote:
		if w <= 128 && h <= 128 {
			l.checks.pass(RuleWebPEmoteDims, LevelWarn, fmt.Sprintf("%dx%d fits 128x128", w, h))
		} else {
			l.checks.fail(RuleWebPEmoteDims, LevelWarn, fmt.Sprintf("%dx%d is larger than 128x128; Discord shrinks emoji to 128x128", w, h))
		}
	}
}

// checkMinDelay evaluates webp.min-delay: durations <= 10 ms are shown as
// 100 ms by browsers (warning); 11–19 ms plays as authored but >= 20 ms is
// recommended (info); >= 20 ms passes; a still has nothing to check.
func (l *webpLinter) checkMinDelay() {
	const rule = RuleWebPMinDelay
	f := l.f
	if len(f.frames) == 0 {
		l.checks.pass(rule, LevelWarn, "still image")
		return
	}
	var clamped, short []int
	for _, fr := range f.frames {
		switch {
		case fr.durationMS <= webpClampDelayMS:
			clamped = append(clamped, fr.index)
		case fr.durationMS < webpMinDelayMS:
			short = append(short, fr.index)
		}
	}
	_, minDelay := f.timing()
	shortNote := fmt.Sprintf("durations of %d-%d ms on %s play as authored in browsers, but >= %d ms is recommended (the 2 cs floor GIF and APNG output use; anything shorter risks the 100 ms clamp after a GIF transcode)", webpClampDelayMS+1, webpMinDelayMS-1, frameList(short), webpMinDelayMS)
	switch {
	case len(clamped) > 0:
		detail := fmt.Sprintf("durations <= %d ms on %s (minimum %d ms); browsers show them as 100 ms", webpClampDelayMS, frameList(clamped), minDelay)
		if len(short) > 0 {
			detail += "; " + shortNote
		}
		l.checks.fail(rule, LevelWarn, detail)
	case len(short) > 0:
		l.checks.fail(rule, LevelInfo, fmt.Sprintf("%s (minimum %d ms)", shortNote, minDelay))
	default:
		l.checks.pass(rule, LevelWarn, fmt.Sprintf("all frame durations >= %d ms (minimum %d ms)", webpMinDelayMS, minDelay))
	}
}

// checkCanvas evaluates webp.canvas.
func (l *webpLinter) checkCanvas() {
	f := l.f
	w, h := f.canvas()
	var problems []string
	if f.vp8x != nil && uint64(f.vp8x.width)*uint64(f.vp8x.height) >= 1<<32 {
		problems = append(problems, fmt.Sprintf("VP8X canvas %dx%d exceeds libwebp's 2^32 pixel limit", f.vp8x.width, f.vp8x.height))
	}
	if f.still != nil {
		if f.still.err != "" {
			problems = append(problems, "still bitstream: "+f.still.err)
		} else if f.vp8x != nil && (f.still.width != w || f.still.height != h) {
			problems = append(problems, fmt.Sprintf("VP8X canvas %dx%d differs from the %s bitstream %dx%d", w, h, f.still.kind, f.still.width, f.still.height))
		}
	}
	for _, fr := range f.frames {
		if len(fr.problems) > 0 || fr.bs == nil {
			problems = append(problems, fmt.Sprintf("frame %d: %s", fr.index, strings.Join(fr.problems, "; ")))
			continue
		}
		if fr.x+fr.width > w || fr.y+fr.height > h {
			problems = append(problems, fmt.Sprintf("frame %d rect %dx%d at %d,%d lies outside the %dx%d canvas", fr.index, fr.width, fr.height, fr.x, fr.y, w, h))
		}
		if fr.bs.err != "" {
			problems = append(problems, fmt.Sprintf("frame %d %s bitstream: %s", fr.index, fr.bs.kind, fr.bs.err))
		} else if fr.bs.width != fr.width || fr.bs.height != fr.height {
			problems = append(problems, fmt.Sprintf("frame %d ANMF rect %dx%d differs from its %s bitstream %dx%d", fr.index, fr.width, fr.height, fr.bs.kind, fr.bs.width, fr.bs.height))
		}
	}
	if len(problems) > 0 {
		l.checks.fail(RuleWebPCanvas, LevelError, strings.Join(problems, "; "))
		return
	}
	switch {
	case len(f.frames) > 0:
		l.checks.pass(RuleWebPCanvas, LevelError, fmt.Sprintf("canvas %dx%d; all %d frames inside it and matching their bitstreams", w, h, len(f.frames)))
	case f.still != nil:
		l.checks.pass(RuleWebPCanvas, LevelError, fmt.Sprintf("%dx%d %s still", w, h, f.still.kind))
	default:
		l.checks.pass(RuleWebPCanvas, LevelError, "no image data to check")
	}
}

// ---------------------------------------------------------------------------
// RIFF parsing

type webpChunk struct {
	fourcc  string
	payload []byte
	offset  int // offset of the chunk header within its container
}

// webpBitstream describes a VP8 or VP8L bitstream header.
type webpBitstream struct {
	kind          string // "VP8" or "VP8L"
	width, height int
	alpha         bool   // VP8L alpha_is_used bit
	err           string // header problem, "" when parsed
}

// webpFrame is one ANMF chunk.
type webpFrame struct {
	index         int
	x, y          int
	width, height int
	durationMS    int
	blend         bool // alpha-blend onto the canvas (flag bit 1 clear)
	dispose       bool // dispose to background after display (flag bit 0)
	hasALPH       bool
	bs            *webpBitstream // nil when the frame has no VP8/VP8L chunk
	problems      []string
}

func (fr *webpFrame) hasAlpha() bool {
	return fr.hasALPH || (fr.bs != nil && fr.bs.alpha)
}

type vp8xInfo struct {
	flags         byte
	width, height int
}

type animInfo struct {
	bgColor   uint32 // B, G, R, A byte order
	loopCount uint16
}

// webpFile is a parsed WebP container.
type webpFile struct {
	size     int
	riffSize uint32
	trailing int // bytes after the RIFF payload
	chunks   []webpChunk
	vp8x     *vp8xInfo
	anim     *animInfo
	frames   []webpFrame
	still    *webpBitstream // top-level VP8/VP8L
	stillALP bool           // top-level ALPH chunk
	hasEXIF  bool
	hasXMP   bool
	hasICCP  bool
	problems []string // container-level problems (webp.riff)
}

func (f *webpFile) frameCount() int {
	if len(f.frames) > 0 {
		return len(f.frames)
	}
	if f.still != nil {
		return 1
	}
	return 0
}

// canvas returns the canvas size: VP8X if present, else the still bitstream.
func (f *webpFile) canvas() (int, int) {
	if f.vp8x != nil {
		return f.vp8x.width, f.vp8x.height
	}
	if f.still != nil {
		return f.still.width, f.still.height
	}
	return 0, 0
}

func (f *webpFile) hasAlpha() bool {
	for i := range f.frames {
		if f.frames[i].hasAlpha() {
			return true
		}
	}
	return f.stillALP || (f.still != nil && f.still.alpha)
}

// timing returns total and minimum frame duration in ms (0, 0 for stills).
func (f *webpFile) timing() (durationMS, minDelayMS int) {
	minDelay := -1
	for _, fr := range f.frames {
		durationMS += fr.durationMS
		if minDelay < 0 || fr.durationMS < minDelay {
			minDelay = fr.durationMS
		}
	}
	return durationMS, max(minDelay, 0)
}

// parseWebP parses a RIFF/WEBP file. It returns an error only when data is
// not a WebP container at all; structural problems inside the container are
// collected in webpFile.problems.
func parseWebP(data []byte) (*webpFile, error) {
	if len(data) < 12 {
		return nil, errors.New("webp: file shorter than the 12-byte RIFF header")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, errors.New("webp: not a RIFF/WEBP file")
	}
	f := &webpFile{size: len(data), riffSize: binary.LittleEndian.Uint32(data[4:8])}
	body := data[12:]
	switch declared := uint64(f.riffSize); {
	case declared < 4:
		f.problems = append(f.problems, fmt.Sprintf("RIFF size %d is too small", f.riffSize))
	case declared-4 > uint64(len(body)):
		f.problems = append(f.problems, fmt.Sprintf("truncated: RIFF size declares %d bytes but the file has %d", declared+8, len(data)))
	default:
		f.trailing = len(body) - int(declared-4)
		body = body[:declared-4]
	}
	if f.riffSize&1 != 0 {
		f.problems = append(f.problems, fmt.Sprintf("RIFF size %d is odd", f.riffSize))
	}
	chunks, err := riffChunks(body)
	if err != nil {
		f.problems = append(f.problems, err.Error())
	}
	f.chunks = chunks
	for i := range chunks {
		f.addChunk(i, &chunks[i])
	}
	switch {
	case len(f.frames) == 0 && f.still == nil:
		f.problems = append(f.problems, "no image data (VP8, VP8L or ANMF chunk)")
	case len(f.frames) > 0 && f.still != nil:
		f.problems = append(f.problems, "both ANMF frames and a top-level image chunk")
	}
	if len(f.frames) > 0 {
		if f.vp8x == nil {
			f.problems = append(f.problems, "ANMF frames without a VP8X chunk")
		}
		if f.anim == nil {
			f.problems = append(f.problems, "ANMF frames without an ANIM chunk")
		}
	}
	if f.stillALP && f.vp8x == nil {
		f.problems = append(f.problems, "ALPH chunk without a VP8X chunk")
	}
	return f, nil
}

// addChunk interprets one top-level chunk.
func (f *webpFile) addChunk(i int, c *webpChunk) {
	switch c.fourcc {
	case "VP8X":
		if i != 0 {
			f.problems = append(f.problems, fmt.Sprintf("VP8X is chunk %d, not the first", i))
		}
		if f.vp8x != nil {
			f.problems = append(f.problems, "duplicate VP8X chunk")
			return
		}
		if len(c.payload) < 10 {
			f.problems = append(f.problems, fmt.Sprintf("VP8X payload is %d bytes, need 10", len(c.payload)))
			return
		}
		f.vp8x = &vp8xInfo{
			flags:  c.payload[0],
			width:  le24(c.payload[4:7]) + 1,
			height: le24(c.payload[7:10]) + 1,
		}
	case "ANIM":
		if f.anim != nil {
			f.problems = append(f.problems, "duplicate ANIM chunk")
			return
		}
		if len(c.payload) < 6 {
			f.problems = append(f.problems, fmt.Sprintf("ANIM payload is %d bytes, need 6", len(c.payload)))
			return
		}
		f.anim = &animInfo{
			bgColor:   binary.LittleEndian.Uint32(c.payload[0:4]),
			loopCount: binary.LittleEndian.Uint16(c.payload[4:6]),
		}
	case "ANMF":
		f.frames = append(f.frames, parseANMF(len(f.frames), c.payload))
	case "VP8 ", "VP8L":
		bs := parseBitstream(c.fourcc, c.payload)
		if f.still != nil {
			f.problems = append(f.problems, "more than one top-level image chunk")
			return
		}
		f.still = &bs
	case "ALPH":
		f.stillALP = true
	case "EXIF":
		f.hasEXIF = true
	case "XMP ":
		f.hasXMP = true
	case "ICCP":
		f.hasICCP = true
	}
}

// parseANMF interprets an ANMF payload: 16-byte header + frame data chunks.
func parseANMF(index int, p []byte) webpFrame {
	fr := webpFrame{index: index}
	if len(p) < 16 {
		fr.problems = append(fr.problems, fmt.Sprintf("ANMF payload is %d bytes, need at least 16", len(p)))
		return fr
	}
	fr.x = 2 * le24(p[0:3])
	fr.y = 2 * le24(p[3:6])
	fr.width = le24(p[6:9]) + 1
	fr.height = le24(p[9:12]) + 1
	fr.durationMS = le24(p[12:15])
	flags := p[15]
	fr.blend = flags&anmfFlagNoBlend == 0
	fr.dispose = flags&anmfFlagDispose != 0
	subs, err := riffChunks(p[16:])
	if err != nil {
		fr.problems = append(fr.problems, err.Error())
	}
	for i := range subs {
		switch subs[i].fourcc {
		case "ALPH":
			fr.hasALPH = true
		case "VP8 ", "VP8L":
			if fr.bs == nil {
				bs := parseBitstream(subs[i].fourcc, subs[i].payload)
				fr.bs = &bs
			}
		}
	}
	if fr.bs == nil {
		fr.problems = append(fr.problems, "no VP8/VP8L bitstream in frame data")
	}
	return fr
}

// riffChunks walks a sequence of RIFF chunks (fourcc, LE32 size, payload,
// pad byte when the size is odd). It returns the chunks it could parse and
// an error describing the first structural problem.
func riffChunks(b []byte) ([]webpChunk, error) {
	var out []webpChunk
	pos := 0
	for pos < len(b) {
		if len(b)-pos < 8 {
			return out, fmt.Errorf("%d stray bytes after the last chunk at offset %d", len(b)-pos, pos)
		}
		fourcc := string(b[pos : pos+4])
		size := binary.LittleEndian.Uint32(b[pos+4 : pos+8])
		start := pos + 8
		if uint64(size) > uint64(len(b)-start) {
			out = append(out, webpChunk{fourcc: fourcc, payload: b[start:], offset: pos})
			return out, fmt.Errorf("chunk %q at offset %d declares %d bytes but only %d remain", fourcc, pos, size, len(b)-start)
		}
		end := start + int(size)
		out = append(out, webpChunk{fourcc: fourcc, payload: b[start:end], offset: pos})
		pos = end + int(size&1)
	}
	return out, nil
}

// parseBitstream reads the dimensions (and VP8L alpha bit) from a VP8 or
// VP8L bitstream header.
func parseBitstream(fourcc string, p []byte) webpBitstream {
	if fourcc == "VP8L" {
		return parseVP8L(p)
	}
	return parseVP8(p)
}

// parseVP8 reads a VP8 key frame header: 3-byte frame tag (bit 0 clear =
// key frame), start code 9D 01 2A, then 14-bit width and height (the top 2
// bits of each are scaling hints).
func parseVP8(p []byte) webpBitstream {
	bs := webpBitstream{kind: "VP8"}
	if len(p) < 10 {
		bs.err = fmt.Sprintf("VP8 payload is %d bytes, need at least 10", len(p))
		return bs
	}
	if p[0]&1 != 0 {
		bs.err = "VP8 frame is not a key frame"
		return bs
	}
	if p[3] != 0x9D || p[4] != 0x01 || p[5] != 0x2A {
		bs.err = "VP8 start code missing"
		return bs
	}
	bs.width = int(binary.LittleEndian.Uint16(p[6:8]) & 0x3FFF)
	bs.height = int(binary.LittleEndian.Uint16(p[8:10]) & 0x3FFF)
	if bs.width == 0 || bs.height == 0 {
		bs.err = fmt.Sprintf("VP8 dimensions %dx%d", bs.width, bs.height)
	}
	return bs
}

// parseVP8L reads a VP8L header: signature 0x2F, then 14 bits width-1,
// 14 bits height-1, 1 bit alpha_is_used, 3 bits version (must be 0).
func parseVP8L(p []byte) webpBitstream {
	bs := webpBitstream{kind: "VP8L"}
	if len(p) < 5 {
		bs.err = fmt.Sprintf("VP8L payload is %d bytes, need at least 5", len(p))
		return bs
	}
	if p[0] != 0x2F {
		bs.err = fmt.Sprintf("VP8L signature byte is 0x%02X, want 0x2F", p[0])
		return bs
	}
	bits := binary.LittleEndian.Uint32(p[1:5])
	bs.width = int(bits&0x3FFF) + 1
	bs.height = int((bits>>14)&0x3FFF) + 1
	bs.alpha = (bits>>28)&1 == 1
	if v := bits >> 29; v != 0 {
		bs.err = fmt.Sprintf("VP8L version %d, want 0", v)
	}
	return bs
}

func le24(b []byte) int {
	return int(b[0]) | int(b[1])<<8 | int(b[2])<<16
}
