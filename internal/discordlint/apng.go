package discordlint

import (
	"fmt"
	"strings"
)

// Phase 2: APNG rules (DESIGN.md §5.3 APNG, §5.1, §9a). Static-image rules
// live in static.go.

// APNG rule ids.
const (
	RuleAPNGContainer  = "apng.container"     // error: PNG signature, IHDR first, acTL before the first IDAT, IEND present
	RuleAPNGPlays      = "apng.plays-forever" // error for Discord targets: acTL num_plays == 0; TargetNone: info with the count
	RuleAPNGFirstFrame = "apng.first-frame"   // warn: the first frame (IDAT) has an fcTL, i.e. is part of the animation (Discord shows a still of it otherwise)
	RuleAPNGCanvas     = "apng.canvas"        // error: every fcTL rect inside IHDR width x height
	RuleAPNGMinDelay   = "apng.min-delay"     // error for sticker (0 delay → "frame rate too small or too large"), warn otherwise: never 0; <= 10 ms is a warning for every target (browsers show 100 ms), 11-19 ms an info note recommending >= 20 ms (Discord accepts up to 60 fps)
	RuleAPNGSizeLimit  = "apng.size-limit"    // error: Bytes <= Limit(target)
	RuleAPNGSticker    = "apng.sticker"       // sticker: dims > 320 on a side → warn (Discord shrinks); duration <= 5000 ms, frames <= 1000, fps <= 60 → error
	RuleAPNGNotEmote   = "apng.not-emote"     // error for emote: APNG is not an animated-emoji format
	RuleAPNGAttachment = "apng.attachment"    // info for attachment: Discord shows only frame 0 of APNG attachments
	RuleAPNGIndexed    = "apng.indexed"       // info (non-blocking): OK only for colour type 3 + PLTE + tRNS — the indexed 8-bit-alpha APNG the sticker default rung produces; detail lists palette size
)

// Frame-delay thresholds for apng.min-delay (DESIGN.md §5.3: delays >= 20 ms
// and never 0). Like GIF and WebP, browsers replace any delay <= 10 ms with
// 100 ms, so such frames play ten times slower than authored — a warning.
// 11-19 ms plays as authored (Discord accepts stickers up to 60 fps) but is
// below the 20 ms floor the GIF output uses, so it only earns an
// informational note recommending >= 20 ms.
const (
	apngClampDelayMS = 10 // <= this is shown as 100 ms by browsers
	apngMinDelayMS   = 20 // recommended minimum
)

// Sticker geometry: Discord shrinks larger uploads (user-verified
// 2026-08-19), so exceeding it is a warning, not an error.
const stickerMaxSide = 320

// LintAPNG parses data as PNG/APNG (IHDR, acTL, fcTL/fdAT/IDAT, PLTE, tRNS,
// IEND) and evaluates the APNG rules for target. Report fields: Format
// "apng" (or "png" when there is no acTL), Width/Height from IHDR, Frames
// from acTL num_frames (1 for a plain PNG), DurationMS and MinDelayMS from
// the fcTL delays (delay_num/delay_den, den 0 → 100), LoopForever
// (num_plays == 0; also true for a plain PNG), HasAlpha (colour type 4/6, or
// 3 with tRNS, or tRNS chunk present). No fixer in Phase 2 (the encoders
// are ours); OK is false when any LevelError check fails.
//
// An error is returned only when data is not a PNG file at all (signature
// missing); every structural problem inside the file is reported through
// apng.container instead.
func LintAPNG(data []byte, target Target) (Report, error) {
	f, err := parsePNG(data)
	if err != nil {
		return Report{}, fmt.Errorf("discordlint: %w", err)
	}
	l := &apngLinter{f: f, target: target}
	l.run()
	totalUS, minUS := f.timing()
	w, h := f.dims()
	format := "png"
	if f.animated() {
		format = "apng"
	}
	return Report{
		RulesVersion: RulesVersion,
		Format:       format,
		Target:       target,
		Bytes:        int64(len(data)),
		Limit:        Limit(target),
		Width:        w,
		Height:       h,
		Frames:       f.frameCount(),
		DurationMS:   roundUSToMS(totalUS),
		MinDelayMS:   roundUSToMS(minUS),
		LoopForever:  f.loopForever(),
		HasAlpha:     f.hasAlpha(),
		Checks:       []Check(l.checks),
		OK:           l.checks.allOK(),
	}, nil
}

// apngLinter evaluates the rules against a parsed file.
type apngLinter struct {
	f      *pngFile
	target Target
	checks checkList
}

func (l *apngLinter) run() {
	l.ruleContainer()
	l.rulePlays()
	l.ruleFirstFrame()
	l.ruleCanvas()
	l.ruleMinDelay()
	l.ruleIndexed()
	l.ruleSizeLimit()
	switch l.target {
	case TargetSticker:
		l.ruleSticker()
	case TargetEmote:
		l.ruleNotEmote()
	case TargetAttachment:
		l.ruleAttachment()
	}
}

// ruleContainer reports the structural problems collected while parsing.
func (l *apngLinter) ruleContainer() {
	const rule = RuleAPNGContainer
	f := l.f
	if len(f.problems) > 0 {
		l.checks.fail(rule, LevelError, strings.Join(f.problems, "; "))
		return
	}
	var detail string
	if f.animated() {
		detail = fmt.Sprintf("APNG container OK (%s, %s)", plural(len(f.chunks), "chunk"), plural(len(f.frames), "frame"))
	} else {
		detail = fmt.Sprintf("PNG container OK (%s, no acTL)", plural(len(f.chunks), "chunk"))
	}
	if len(f.ancillary) > 0 {
		detail += "; other chunks: " + strings.Join(f.ancillary, ", ")
		if f.ancillaryMore > 0 {
			detail += fmt.Sprintf(" and %d more types", f.ancillaryMore)
		}
	}
	if f.trailing > 0 {
		detail += fmt.Sprintf("; %d bytes after IEND are ignored", f.trailing)
	}
	l.checks.pass(rule, LevelError, detail)
}

// rulePlays: acTL num_plays must be 0 for Discord targets (a finite count
// stops the sticker after N plays); TargetNone reports the count.
func (l *apngLinter) rulePlays() {
	const rule = RuleAPNGPlays
	f := l.f
	switch {
	case !f.animated():
		l.checks.pass(rule, LevelError, "plain PNG (no acTL); looping does not apply")
	case f.actl.numPlays == 0:
		l.checks.pass(rule, LevelError, "acTL num_plays 0 (loops forever)")
	case l.target == TargetNone:
		l.checks.pass(rule, LevelInfo, fmt.Sprintf("acTL num_plays %d (%s); Discord targets require 0 (loop forever)", f.actl.numPlays, plays(int(f.actl.numPlays))))
	default:
		l.checks.fail(rule, LevelError, fmt.Sprintf("acTL num_plays is %d (%s); Discord needs 0 = loop forever — encode with -plays 0", f.actl.numPlays, plays(int(f.actl.numPlays))))
	}
}

// ruleFirstFrame: the default image (IDAT) must be frame 0 of the animation
// (an fcTL before IDAT). Otherwise APNG-aware viewers skip it while
// everything else — Discord's still previews included — shows it.
func (l *apngLinter) ruleFirstFrame() {
	const rule = RuleAPNGFirstFrame
	f := l.f
	switch {
	case !f.animated():
		l.checks.pass(rule, LevelWarn, "plain PNG; the image is its only frame")
	case len(f.frames) == 0:
		l.checks.fail(rule, LevelWarn, "no fcTL chunks: the file declares an animation without frames")
	case f.frames[0].isDefault:
		l.checks.pass(rule, LevelWarn, "the default image (IDAT) is frame 0 of the animation (fcTL before IDAT)")
	default:
		l.checks.fail(rule, LevelWarn, "the default image (IDAT) has no fcTL, so it is not part of the animation; Discord's still previews show it while the animation starts elsewhere — encode with frame 0 as the IDAT image")
	}
}

// ruleCanvas: IHDR dimensions are sane and every fcTL rectangle lies inside
// them; the default frame (fcTL before IDAT) must cover the whole canvas at
// 0,0 as the spec requires.
func (l *apngLinter) ruleCanvas() {
	const rule = RuleAPNGCanvas
	f := l.f
	if f.ihdr == nil {
		l.checks.fail(rule, LevelError, "no IHDR; canvas size unknown")
		return
	}
	w, h := uint64(f.ihdr.width), uint64(f.ihdr.height)
	var problems []string
	if w == 0 || h == 0 {
		problems = append(problems, fmt.Sprintf("IHDR canvas is %dx%d", w, h))
	}
	for i := range f.frames {
		fr := &f.frames[i]
		fw, fh := uint64(fr.width), uint64(fr.height)
		switch {
		case fw == 0 || fh == 0:
			problems = append(problems, fmt.Sprintf("frame %d rect is %dx%d", fr.index, fw, fh))
		case uint64(fr.x)+fw > w || uint64(fr.y)+fh > h:
			problems = append(problems, fmt.Sprintf("frame %d rect %dx%d at %d,%d lies outside the %dx%d canvas", fr.index, fw, fh, fr.x, fr.y, w, h))
		case fr.isDefault && (fr.x != 0 || fr.y != 0 || fw != w || fh != h):
			problems = append(problems, fmt.Sprintf("frame 0 (the default image) rect %dx%d at %d,%d must cover the whole %dx%d canvas at 0,0", fw, fh, fr.x, fr.y, w, h))
		}
	}
	if len(problems) > 0 {
		l.checks.fail(rule, LevelError, strings.Join(problems, "; "))
		return
	}
	if !f.animated() {
		l.checks.pass(rule, LevelError, fmt.Sprintf("%dx%d still", w, h))
		return
	}
	l.checks.pass(rule, LevelError, fmt.Sprintf("canvas %dx%d; all %s inside it", w, h, plural(len(f.frames), "frame rectangle")))
}

// ruleMinDelay: every fcTL delay is >= 20 ms and never 0, tiered like
// webp.min-delay. A zero delay is an error for stickers (Discord rejects the
// upload: "frame rate too small or too large") and a warning otherwise.
// Delays <= 10 ms are a warning for every target (browsers show them as
// 100 ms); 11-19 ms plays as authored — Discord accepts stickers up to
// 60 fps — so it is only an informational note recommending >= 20 ms.
func (l *apngLinter) ruleMinDelay() {
	const rule = RuleAPNGMinDelay
	f := l.f
	level := LevelWarn
	if l.target == TargetSticker {
		level = LevelError
	}
	if !f.animated() || len(f.frames) == 0 {
		l.checks.pass(rule, level, "still image; no frame delays")
		return
	}
	var zero, clamped, short []int
	for i := range f.frames {
		fr := &f.frames[i]
		switch ms := fr.delayMS(); {
		case fr.delayNum == 0:
			zero = append(zero, fr.index)
		case ms <= apngClampDelayMS:
			clamped = append(clamped, fr.index)
		case ms < apngMinDelayMS:
			short = append(short, fr.index)
		}
	}
	_, minUS := f.timing()
	minMS := roundUSToMS(minUS)
	clampNote := fmt.Sprintf("delays <= %d ms on %s; browsers show them as 100 ms", apngClampDelayMS, frameList(clamped))
	shortNote := fmt.Sprintf("delays of %d-%d ms on %s play as authored (Discord accepts up to 60 fps), but >= %d ms is recommended (the 2 cs floor the GIF output uses)", apngClampDelayMS+1, apngMinDelayMS-1, frameList(short), apngMinDelayMS)
	switch {
	case len(zero) > 0:
		detail := fmt.Sprintf("delay 0 on %s; Discord rejects such stickers (\"frame rate too small or too large\") and browsers show them as 100 ms", frameList(zero))
		if len(clamped) > 0 {
			detail += "; " + clampNote
		}
		if len(short) > 0 {
			detail += "; " + shortNote
		}
		l.checks.fail(rule, level, detail)
	case len(clamped) > 0:
		detail := fmt.Sprintf("delays <= %d ms on %s (minimum %d ms); browsers show them as 100 ms", apngClampDelayMS, frameList(clamped), minMS)
		if len(short) > 0 {
			detail += "; " + shortNote
		}
		l.checks.fail(rule, LevelWarn, detail)
	case len(short) > 0:
		l.checks.fail(rule, LevelInfo, fmt.Sprintf("%s (minimum %d ms)", shortNote, minMS))
	default:
		l.checks.pass(rule, level, fmt.Sprintf("all frame delays >= %d ms (minimum %d ms)", apngMinDelayMS, minMS))
	}
}

// ruleIndexed is informational and non-blocking: OK is true only for an
// indexed 8-bit-alpha APNG (colour type 3 with PLTE and tRNS — what the
// sticker default rung produces), so the UI and tests can tell an indexed
// output from an RGBA or opaque-indexed one. A LevelInfo failure does not
// affect Report.OK (allOK only counts LevelError).
func (l *apngLinter) ruleIndexed() {
	const rule = RuleAPNGIndexed
	f := l.f
	desc := f.colourDescription()
	if f.ihdr != nil && f.ihdr.colorType == pngIndexed && f.plte >= 0 {
		if f.hasTRNS {
			l.checks.pass(rule, LevelInfo, "indexed 8-bit-alpha APNG: "+desc)
			return
		}
		l.checks.fail(rule, LevelInfo, "indexed APNG without tRNS (opaque): "+desc)
		return
	}
	l.checks.fail(rule, LevelInfo, desc+"; not an indexed 8-bit-alpha APNG (the sticker default rung)")
}

// ruleSizeLimit checks the byte count against the target's cap.
func (l *apngLinter) ruleSizeLimit() {
	const rule = RuleAPNGSizeLimit
	limit := Limit(l.target)
	if limit <= 0 {
		return
	}
	size := l.f.size
	if int64(size) > limit {
		l.checks.fail(rule, LevelError, fmt.Sprintf("%d bytes exceeds the %d byte limit for %s", size, limit, l.target))
		return
	}
	l.checks.pass(rule, LevelError, fmt.Sprintf("%d of %d bytes", size, limit))
}

// ruleSticker applies the sticker geometry and timing limits: a side over
// 320 px only warns (Discord shrinks larger stickers and accepts smaller or
// non-square ones); duration > 5 s, > 1000 frames or > 60 fps are errors.
func (l *apngLinter) ruleSticker() {
	const rule = RuleAPNGSticker
	f := l.f
	w, h := f.dims()
	totalUS, _ := f.timing()
	timing := stickerLimitsCheck(rule, f.frameCount(), totalUS)
	dims := stickerDimsDetail(w, h)
	switch {
	case !timing.OK:
		detail := timing.Detail
		if dims != "" {
			detail += "; " + dims
		}
		l.checks.fail(rule, LevelError, detail)
	case dims != "":
		l.checks.fail(rule, LevelWarn, dims+"; "+timing.Detail)
	default:
		l.checks.pass(rule, LevelError, fmt.Sprintf("%dx%d; %s", w, h, timing.Detail))
	}
}

// stickerDimsDetail returns a warning detail when either side exceeds the
// 320 px sticker size, or "" when the dimensions are fine.
func stickerDimsDetail(w, h int) string {
	if w > stickerMaxSide || h > stickerMaxSide {
		return fmt.Sprintf("%dx%d is larger than %dx%d; Discord shrinks stickers to %dx%d", w, h, stickerMaxSide, stickerMaxSide, stickerMaxSide, stickerMaxSide)
	}
	return ""
}

// ruleNotEmote: APNG is not an animated-emoji format; a plain PNG is a fine
// static emoji.
func (l *apngLinter) ruleNotEmote() {
	const rule = RuleAPNGNotEmote
	f := l.f
	if f.animated() {
		l.checks.fail(rule, LevelError, "APNG is not an animated-emoji format (Discord animates GIF, WebP and AVIF emoji); the upload would show a still or be rejected")
		return
	}
	w, h := f.dims()
	detail := fmt.Sprintf("plain PNG %dx%d; static PNG is a valid emoji format", w, h)
	if w > 128 || h > 128 {
		detail += " (Discord shrinks emoji to 128x128)"
	}
	l.checks.pass(rule, LevelError, detail)
}

// ruleAttachment: Discord shows only frame 0 of an APNG attachment
// (verified 2026-08-19), so an animation posted in chat looks like a still.
func (l *apngLinter) ruleAttachment() {
	const rule = RuleAPNGAttachment
	if l.f.animated() {
		l.checks.fail(rule, LevelInfo, "Discord shows only frame 0 of APNG attachments; APNG animates only as a server sticker — use GIF, WebP or AVIF for animated attachments")
		return
	}
	l.checks.pass(rule, LevelInfo, "plain PNG; renders as-is")
}
