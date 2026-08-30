package discordlint

import (
	"fmt"
	"strings"
)

// Phase 4: structural rules for the opaque video exports (DESIGN.md §5.3).
// Video files are never rewritten — there is no fixer — and Discord takes
// them only as chat attachments, never as emotes or stickers.

// Video rule ids (stable; referenced by the UI and by report.json).
const (
	RuleVideoContainer      = "video.container"       // error: MP4 (ftyp brand + moov + mdat, clean box stream) / WebM (EBML DocType webm + Segment)
	RuleVideoFaststart      = "video.faststart"       // warn, mp4 only: moov before mdat (+faststart) so Discord can stream the preview; omitted when moov or mdat is missing
	RuleVideoCodec          = "video.codec"           // error: H.264 (avc1/avc3) for mp4, V_VP9 for webm; an info pass when the codec cannot be determined
	RuleVideoDims           = "video.dims"            // warn: even dimensions, no side over videoMaxSide; an info pass when the dimensions cannot be parsed
	RuleVideoDuration       = "video.duration"        // info: best-effort duration / frame count / fps report (never fails)
	RuleVideoSizeLimit      = "video.size-limit"      // error: Bytes <= Limit(target)
	RuleVideoAttachmentOnly = "video.attachment-only" // error for emote/sticker (video formats cannot be emotes/stickers); a pass for the attachment tiers; omitted for TargetNone
)

// videoMaxSide is the sane-bounds cap for video.dims: Discord plays larger
// files but the preview pipeline has no business receiving them from us.
const videoMaxSide = 4096

// IsVideoFormat reports whether format names one of the opaque video
// exports LintVideo accepts: "mp4" or "webm" (case-insensitive, trimmed —
// the same folding LintVideo applies).
func IsVideoFormat(format string) bool {
	switch normaliseVideoFormat(format) {
	case "mp4", "webm":
		return true
	}
	return false
}

// VideoTargetOK reports whether target may receive a video export: emote
// and sticker cannot (Discord only accepts image formats there); every
// other target — the attachment tiers, TargetNone, and unknown strings
// (treated like TargetNone, as elsewhere in the package) — can. LintVideo
// enforces the same restriction via video.attachment-only; this helper
// lets jobs/server refuse the combination before rendering.
func VideoTargetOK(t Target) bool {
	return t != TargetEmote && t != TargetSticker
}

// normaliseVideoFormat lower-cases and trims the format name.
func normaliseVideoFormat(format string) string {
	return strings.ToLower(strings.TrimSpace(format))
}

// LintVideo evaluates the structural video rules for target. format is
// "mp4" or "webm"; everything about the file itself is best-effort byte
// parsing (MP4: top-level ISO-BMFF boxes, moov/trak/stsd/stts; WebM: EBML
// elements, Segment Info/Tracks and SimpleBlock counting) — a value that
// cannot be parsed leaves its Report field at 0 and turns the affected
// check into an informational pass instead of a failure, except
// video.container, which is the structural gate and fails hard.
//
// An error is returned only for an unsupported format name. There is no
// fixer: no check ever reports Fixed and the input bytes are never
// rewritten. Report.HasAlpha is always false (both exports are opaque) and
// Report.LoopForever is true (looping does not apply to video files —
// Discord's player controls playback).
func LintVideo(format string, data []byte, target Target) (Report, error) {
	format = normaliseVideoFormat(format)
	var info videoInfo
	switch format {
	case "mp4":
		info = probeMP4(data)
	case "webm":
		info = probeWebM(data)
	default:
		return Report{}, fmt.Errorf("discordlint: video: unsupported format %q (want mp4 or webm)", format)
	}
	l := &videoLinter{format: format, info: info, size: len(data), target: target}
	l.run()
	return Report{
		RulesVersion: RulesVersion,
		Format:       format,
		Target:       target,
		Bytes:        int64(len(data)),
		Limit:        Limit(target),
		Width:        info.width,
		Height:       info.height,
		Frames:       info.frames,
		DurationMS:   info.durationMS,
		LoopForever:  true, // looping does not apply (see the doc comment)
		HasAlpha:     false,
		Checks:       []Check(l.checks),
		OK:           l.checks.allOK(),
	}, nil
}

// videoInfo is what the container probes report. Zero values mean
// "unknown"; problems non-empty means the container check fails.
type videoInfo struct {
	container  string   // pass detail for video.container
	problems   []string // container problems; non-empty → video.container fails
	faststart  int      // mp4 only: 1 moov before mdat, -1 after, 0 rule omitted (webm, or a box is missing)
	codec      string   // human-readable codec found ("H.264 (avc1)", "V_VP9", …)
	codecOK    bool     // codec is the one the format requires
	codecNote  string   // non-empty → codec could not be determined (info pass with this reason)
	width      int      // 0 = unknown
	height     int      // 0 = unknown
	durationMS int      // 0 = unknown
	frames     int      // 0 = unknown
}

// videoLinter evaluates the video rules.
type videoLinter struct {
	format string
	info   videoInfo
	size   int
	target Target
	checks checkList
}

func (l *videoLinter) run() {
	l.ruleContainer()
	l.ruleFaststart()
	l.ruleCodec()
	l.ruleDims()
	l.ruleDuration()
	l.checks.sizeLimit(RuleVideoSizeLimit, int64(l.size), l.target)
	l.ruleAttachmentOnly()
}

// ruleContainer is the structural gate: box/element stream, brand or
// DocType, and the presence of the movie metadata and media data.
func (l *videoLinter) ruleContainer() {
	if len(l.info.problems) > 0 {
		l.checks.fail(RuleVideoContainer, LevelError, strings.Join(l.info.problems, "; "))
		return
	}
	l.checks.pass(RuleVideoContainer, LevelError, l.info.container)
}

// ruleFaststart (mp4 only): moov before mdat lets Discord stream the
// preview without downloading the whole file. Omitted when either box is
// missing (the container rule already failed).
func (l *videoLinter) ruleFaststart() {
	switch l.info.faststart {
	case 1:
		l.checks.pass(RuleVideoFaststart, LevelWarn, "moov (movie metadata) precedes mdat — the file streams without a full download")
	case -1:
		l.checks.fail(RuleVideoFaststart, LevelWarn, "moov comes after mdat; encode with -movflags +faststart so Discord can stream the preview")
	}
}

// ruleCodec: the format's required codec, an error for anything else and
// an informational pass when it could not be determined.
func (l *videoLinter) ruleCodec() {
	if l.info.codecNote != "" {
		l.checks.pass(RuleVideoCodec, LevelInfo, "codec could not be determined ("+l.info.codecNote+")")
		return
	}
	want := "H.264 (avc1)"
	if l.format == "webm" {
		want = "VP9 (V_VP9)"
	}
	if l.info.codecOK {
		l.checks.pass(RuleVideoCodec, LevelError, l.info.codec)
		return
	}
	l.checks.fail(RuleVideoCodec, LevelError, fmt.Sprintf("codec %s — .%s exports must be %s for Discord playback", l.info.codec, l.format, want))
}

// ruleDims: both dimensions even (yuv420p subsampling) and no side over
// videoMaxSide; an informational pass when they could not be parsed.
func (l *videoLinter) ruleDims() {
	const rule = RuleVideoDims
	w, h := l.info.width, l.info.height
	switch {
	case w <= 0 || h <= 0:
		l.checks.pass(rule, LevelInfo, "dimensions could not be parsed; even-dimension and size checks skipped")
	case w%2 != 0 || h%2 != 0:
		l.checks.fail(rule, LevelWarn, fmt.Sprintf("%dx%d has an odd %s; yuv420p video should use even dimensions", w, h, oddSides(w, h)))
	case w > videoMaxSide || h > videoMaxSide:
		l.checks.fail(rule, LevelWarn, fmt.Sprintf("%dx%d exceeds %d px on a side; downscale before uploading", w, h, videoMaxSide))
	default:
		l.checks.pass(rule, LevelWarn, fmt.Sprintf("%dx%d, even dimensions", w, h))
	}
}

// oddSides words which dimensions are odd.
func oddSides(w, h int) string {
	switch {
	case w%2 != 0 && h%2 != 0:
		return "width and height"
	case w%2 != 0:
		return "width"
	}
	return "height"
}

// ruleDuration is purely informational: duration, frame count and fps as
// far as they parsed.
func (l *videoLinter) ruleDuration() {
	const rule = RuleVideoDuration
	ms, frames := l.info.durationMS, l.info.frames
	switch {
	case ms > 0 && frames > 0:
		l.checks.pass(rule, LevelInfo, fmt.Sprintf("duration %.2f s, %s (%.1f fps)", float64(ms)/1000, plural(frames, "frame"), float64(frames)*1000/float64(ms)))
	case ms > 0:
		l.checks.pass(rule, LevelInfo, fmt.Sprintf("duration %.2f s (frame count not parsed)", float64(ms)/1000))
	case frames > 0:
		l.checks.pass(rule, LevelInfo, fmt.Sprintf("%s (duration not parsed)", plural(frames, "frame")))
	default:
		l.checks.pass(rule, LevelInfo, "duration not parsed")
	}
}

// ruleAttachmentOnly: video uploads exist only as chat attachments. Emote
// and sticker targets fail hard; the attachment tiers record a pass; the
// rule is omitted for TargetNone and unknown targets (like the other
// target-specific rules).
func (l *videoLinter) ruleAttachmentOnly() {
	const rule = RuleVideoAttachmentOnly
	if !IsDiscord(l.target) {
		return
	}
	if !VideoTargetOK(l.target) {
		l.checks.fail(rule, LevelError, fmt.Sprintf("video formats cannot be emotes/stickers; %s only uploads as a chat attachment (target %s)", strings.ToUpper(l.format), Describe(l.target)))
		return
	}
	l.checks.pass(rule, LevelError, fmt.Sprintf("%s uploads as a chat attachment; target %s", strings.ToUpper(l.format), Describe(l.target)))
}
