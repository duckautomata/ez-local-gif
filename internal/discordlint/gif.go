package discordlint

import (
	"fmt"
	"slices"
	"strings"
)

// GIF rule ids (stable; referenced by the UI and by report.json).
const (
	RuleGIFGCEEveryFrame     = "gif.gce-every-frame"
	RuleGIFFrame0Transparent = "gif.frame0-transparency"
	RuleGIFLSDBackground     = "gif.lsd-background-index"
	RuleGIFDisposal          = "gif.disposal"
	RuleGIFNetscapeLoop      = "gif.netscape-loop"
	RuleGIFMinDelay          = "gif.min-delay"
	RuleGIFGlobalPalette     = "gif.global-palette"
	RuleGIFNoInterlace       = "gif.no-interlace"
	RuleGIFNoExtraExtensions = "gif.no-extra-extensions"
	RuleGIFFirstFrameVisible = "gif.first-frame-visible"
	RuleGIFTrailer           = "gif.trailer"
	RuleGIFSizeLimit         = "gif.size-limit"
	RuleGIFStickerDims       = "gif.sticker-dims"
	RuleGIFStickerDuration   = "gif.sticker-duration"
	RuleGIFEmoteDims         = "gif.emote-dims"
)

// Delay applied when a frame's delay is 0 or 1 cs (what browsers render) and
// the fallback for synthesised GCEs without a neighbour to copy from.
const gifDefaultDelayCS = 10

// gifLinter holds the state of one LintGIF run.
type gifLinter struct {
	g      *gifFile
	target Target
	fix    bool
	pix    pixelCache
	checks checkList
	dirty  bool // a fix modified the block model

	// Content facts computed before any fix is applied.
	anyTransparent bool   // some frame really shows the canvas through
	transparentWhy string // how anyTransparent was decided (for details)
	loopForever    bool   // set by ruleNetscapeLoop (post-fix state)
}

// LintGIF parses data, evaluates the GIF rules for target and, when fix is
// true, returns rewritten bytes with every fixable violation corrected
// (GCE inserted on frames lacking one; frame-0 transparency flag + index
// and LSD background index set when any frame is transparent; disposal
// 0→1; delays 0/1 cs → 10 cs; NETSCAPE2.0 loop block inserted when
// missing and, for Discord targets, its count forced to 0 = loop forever
// — TargetNone keeps the file's own count; comment / plain-text /
// non-NETSCAPE application extensions stripped). Unfixable violations
// (disposal 3, interlaced frames, local colour tables, no free palette
// slot, over byte limit, sticker duration/frame/fps limits, and — as a
// warning — a sticker side over 320 px) are reported as failed checks so
// the caller can fall back to a re-encode. The returned bytes equal data
// when fix is false or nothing changed.
func LintGIF(data []byte, target Target, fix bool) (Report, []byte, error) {
	g, err := parseGIF(data)
	if err != nil {
		return Report{}, nil, fmt.Errorf("discordlint: %w", err)
	}
	l := &gifLinter{g: g, target: target, fix: fix}
	l.analyseTransparency()

	l.ruleGCEEveryFrame()
	l.ruleFrame0Transparency()
	l.ruleLSDBackground()
	l.ruleDisposal()
	l.ruleNetscapeLoop()
	l.ruleMinDelay()
	l.ruleGlobalPalette()
	l.ruleNoInterlace()
	l.ruleNoExtraExtensions()
	l.ruleFirstFrameVisible()
	l.ruleTrailer()

	out := data
	if fix && l.dirty {
		// Every fix relies on GIF89a features (Graphic Control and
		// Application extensions), so a rewritten GIF87a file is relabelled.
		if string(g.header[:]) == "GIF87a" {
			copy(g.header[:], "GIF89a")
		}
		out = g.encode()
	}
	l.ruleSizeLimit(RuleGIFSizeLimit, len(out))
	l.ruleTargetShape()

	return l.report(len(out)), out, nil
}

// report assembles the Report from the (possibly fixed) block model.
func (l *gifLinter) report(size int) Report {
	frames, _ := l.g.frames()
	duration, minDelay := gifTiming(frames)
	return Report{
		RulesVersion: RulesVersion,
		Format:       "gif",
		Target:       l.target,
		Bytes:        int64(size),
		Limit:        Limit(l.target),
		Width:        int(l.g.width),
		Height:       int(l.g.height),
		Frames:       len(frames),
		DurationMS:   duration,
		MinDelayMS:   minDelay,
		// Looping does not apply to a single frame (see Report.LoopForever).
		LoopForever: l.loopForever || len(frames) <= 1,
		HasAlpha:    l.anyTransparent,
		Checks:      []Check(l.checks),
		OK:          l.checks.allOK(),
	}
}

// gifTiming returns the total duration and the minimum frame delay in
// milliseconds. A frame without a GCE has delay 0 (as browsers see it).
func gifTiming(frames []gifFrame) (durationMS, minDelayMS int) {
	minDelay := -1
	for _, f := range frames {
		d := 0
		if f.gce != nil {
			d = int(f.gce.delayCS)
		}
		durationMS += d * 10
		if minDelay < 0 || d < minDelay {
			minDelay = d
		}
	}
	return durationMS, max(minDelay, 0) * 10
}

// analyseTransparency decides whether the animation really shows the canvas
// through: a frame whose GCE transparency flag is set and whose pixel data
// contains the transparent index (assumed when the pixels cannot be
// decoded), or a frame 0 that does not cover the logical screen (browsers
// leave the border transparent; lilliput would paint the background colour
// unless frame 0 declares transparency).
func (l *gifLinter) analyseTransparency() {
	frames, _ := l.g.frames()
	if len(frames) == 0 {
		return
	}
	var certain, assumed []int
	for i := range frames {
		f := &frames[i]
		if f.gce == nil || !f.gce.transparent {
			continue
		}
		used, known := l.pix.usesIndex(f, f.gce.transIndex)
		switch {
		case used && known:
			certain = append(certain, f.index)
		case !known:
			assumed = append(assumed, f.index)
		}
		if len(certain) > 0 {
			break // one frame that really uses its index settles it; skip decoding the rest
		}
	}
	f0 := frames[0].image
	uncovered := f0.left != 0 || f0.top != 0 || f0.width != l.g.width || f0.height != l.g.height
	var why []string
	if len(certain) > 0 {
		why = append(why, fmt.Sprintf("transparent index really used on %s", frameList(certain)))
	}
	if len(assumed) > 0 {
		why = append(why, fmt.Sprintf("transparency flag set on %s whose pixel data could not be decoded (transparency assumed)", frameList(assumed)))
	}
	if uncovered {
		why = append(why, fmt.Sprintf("frame 0 (%dx%d at %d,%d) does not cover the %dx%d canvas", f0.width, f0.height, f0.left, f0.top, l.g.width, l.g.height))
	}
	l.anyTransparent = len(why) > 0
	l.transparentWhy = strings.Join(why, "; ")
}

// ruleGCEEveryFrame: every image is preceded by exactly one Graphic Control
// Extension (lilliput leaves the transparent index uninitialised for frames
// without one; duplicates make giflib and browsers disagree).
func (l *gifLinter) ruleGCEEveryFrame() {
	const rule = RuleGIFGCEEveryFrame
	frames, _ := l.g.frames()
	if len(frames) == 0 {
		l.checks.fail(rule, LevelError, "file contains no image frames")
		return
	}
	var missing, dups []int
	var dupBlocks []int
	for _, f := range frames {
		if f.gce == nil {
			missing = append(missing, f.index)
		}
		if len(f.dupGCE) > 0 {
			dups = append(dups, f.index)
			dupBlocks = append(dupBlocks, f.dupGCE...)
		}
	}
	if len(missing) == 0 && len(dups) == 0 {
		l.checks.pass(rule, LevelError, fmt.Sprintf("all %s have a Graphic Control Extension", plural(len(frames), "frame")))
		return
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("no Graphic Control Extension before %s", frameList(missing)))
	}
	if len(dups) > 0 {
		parts = append(parts, fmt.Sprintf("more than one Graphic Control Extension before %s", frameList(dups)))
	}
	detail := strings.Join(parts, "; ")
	if !l.fix {
		l.checks.fail(rule, LevelError, detail)
		return
	}
	// Drop duplicates (keeping the last GCE of each run, which is what
	// browsers apply), then synthesise the missing ones from the last frame
	// backwards so block indices stay valid.
	l.g.removeBlocks(dupBlocks)
	frames, _ = l.g.frames()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].gce != nil {
			continue
		}
		l.g.insertBlock(frames[i].imageBlock, newGCE(neighbourDelay(frames, i)))
	}
	l.dirty = true
	var fixes []string
	if len(missing) > 0 {
		fixes = append(fixes, "inserted GCEs (disposal 1, delay copied from the nearest frame, opaque)")
	}
	if len(dups) > 0 {
		fixes = append(fixes, "removed the extra GCEs")
	}
	l.checks.fixed(rule, LevelError, detail+"; "+strings.Join(fixes, "; "))
}

// neighbourDelay returns the delay of the nearest frame with a GCE (looking
// backwards first, then forwards) or the default.
func neighbourDelay(frames []gifFrame, i int) uint16 {
	for j := i - 1; j >= 0; j-- {
		if frames[j].gce != nil {
			return frames[j].gce.delayCS
		}
	}
	for j := i + 1; j < len(frames); j++ {
		if frames[j].gce != nil {
			return frames[j].gce.delayCS
		}
	}
	return gifDefaultDelayCS
}

// ruleFrame0Transparency: when the animation is transparent, frame 0's GCE
// must carry the transparency flag — lilliput decides whether the canvas is
// transparent or filled with the background colour from frame 0 alone. When
// frame 0 has no transparent pixels the flag is set with an index its pixels
// do not use, so its appearance does not change.
func (l *gifLinter) ruleFrame0Transparency() {
	const rule = RuleGIFFrame0Transparent
	frames, _ := l.g.frames()
	if len(frames) == 0 {
		return
	}
	if !l.anyTransparent {
		l.checks.pass(rule, LevelError, "no frame is transparent; frame 0 needs no transparency flag")
		return
	}
	f0 := &frames[0]
	if f0.gce != nil && f0.gce.transparent {
		l.checks.pass(rule, LevelError, fmt.Sprintf("frame 0 has the transparency flag (index %d); %s", f0.gce.transIndex, l.transparentWhy))
		return
	}
	problem := "frame 0 has no transparency flag, so Discord's decoder would composite the animation on an opaque background (" + l.transparentWhy + ")"
	idx, why := l.chooseFrame0TransIndex(frames)
	if idx < 0 {
		l.checks.fail(rule, LevelError, problem+"; cannot allocate a transparent index: "+why)
		return
	}
	if !l.fix {
		l.checks.fail(rule, LevelError, fmt.Sprintf("%s; fixable by setting unused index %d", problem, idx))
		return
	}
	if f0.gce == nil { // ruleGCEEveryFrame has run with fix=true, so this is defensive
		gce := newGCE(neighbourDelay(frames, 0))
		l.g.insertBlock(f0.imageBlock, gce)
		f0.gce = gce
	}
	f0.gce.setTransparent(true, byte(idx))
	l.dirty = true
	l.checks.fixed(rule, LevelError, fmt.Sprintf("%s; set frame 0's transparency flag to index %d, which its pixels do not use", problem, idx))
}

// chooseFrame0TransIndex picks a palette index for frame 0's transparency
// flag that frame 0's pixel data does not use: the index the other
// transparent frames use if free, else the lowest unused global palette
// entry. It returns -1 and a reason when none can be chosen.
func (l *gifLinter) chooseFrame0TransIndex(frames []gifFrame) (int, string) {
	f0 := &frames[0]
	if f0.image.hasLCT() {
		return -1, "frame 0 uses a local colour table"
	}
	palette := l.g.gctColors()
	if palette == 0 {
		return -1, "the file has no global colour table"
	}
	pix, err := l.pix.get(f0)
	if err != nil {
		return -1, "frame 0 pixel data could not be decoded (" + err.Error() + ")"
	}
	var count [256]int
	best := -1
	for _, f := range frames[1:] {
		if f.gce == nil || !f.gce.transparent {
			continue
		}
		count[f.gce.transIndex]++
		if best < 0 || count[f.gce.transIndex] > count[best] {
			best = int(f.gce.transIndex)
		}
	}
	if best >= 0 && !pix.used[best] {
		return best, ""
	}
	for i := 0; i < palette; i++ {
		if !pix.used[i] {
			return i, ""
		}
	}
	return -1, fmt.Sprintf("frame 0 uses all %d global palette entries", palette)
}

// ruleLSDBackground: with a transparent frame 0 the Logical Screen
// Descriptor's background index must equal frame 0's transparent index;
// with an opaque frame 0 it must not equal any frame's transparent index
// (lilliput's encoder deletes transparency for a frame whose transparent
// index equals the background index when the background is opaque).
func (l *gifLinter) ruleLSDBackground() {
	const rule = RuleGIFLSDBackground
	frames, _ := l.g.frames()
	if len(frames) == 0 {
		return
	}
	bg := l.g.bgIndex
	if f0 := frames[0].gce; f0 != nil && f0.transparent {
		want := f0.transIndex
		if bg == want {
			l.checks.pass(rule, LevelError, fmt.Sprintf("background index %d equals frame 0's transparent index", bg))
			return
		}
		detail := fmt.Sprintf("background index is %d but frame 0's transparent index is %d", bg, want)
		if l.fix {
			l.g.bgIndex = want
			l.dirty = true
			detail += fmt.Sprintf("; set to %d", want)
		}
		l.checks.outcome(rule, LevelError, l.fix, detail)
		return
	}
	var flagged [256]bool
	var conflict []int
	for _, f := range frames {
		if f.gce != nil && f.gce.transparent {
			flagged[f.gce.transIndex] = true
			if f.gce.transIndex == bg {
				conflict = append(conflict, f.index)
			}
		}
	}
	if len(conflict) == 0 {
		l.checks.pass(rule, LevelError, fmt.Sprintf("background index %d is not the transparent index of any frame", bg))
		return
	}
	detail := fmt.Sprintf("background index %d equals the transparent index of %s while frame 0 is opaque (lilliput would drop that transparency)", bg, frameList(conflict))
	free := -1
	limit := l.g.gctColors()
	if limit == 0 {
		limit = 256
	}
	for i := 0; i < limit; i++ {
		if !flagged[i] {
			free = i
			break
		}
	}
	if free < 0 {
		l.checks.fail(rule, LevelError, detail+"; every palette index is some frame's transparent index")
		return
	}
	if l.fix {
		l.g.bgIndex = byte(free)
		l.dirty = true
		detail += fmt.Sprintf("; set to %d", free)
	}
	l.checks.outcome(rule, LevelError, l.fix, detail)
}

// ruleDisposal: every GCE uses disposal 1 (do not dispose) or 2 (restore to
// background). 0 is rewritten to 1; 3 (restore previous) and the reserved
// values cannot be fixed without re-encoding.
func (l *gifLinter) ruleDisposal() {
	const rule = RuleGIFDisposal
	frames, _ := l.g.frames()
	var zero, bad []int
	for _, f := range frames {
		if f.gce == nil {
			continue
		}
		switch f.gce.disposal {
		case 0:
			zero = append(zero, f.index)
		case 1, 2:
		default:
			bad = append(bad, f.index)
		}
	}
	if len(zero) == 0 && len(bad) == 0 {
		l.checks.pass(rule, LevelError, "every frame uses disposal 1 or 2")
		return
	}
	var parts []string
	if len(zero) > 0 {
		s := fmt.Sprintf("disposal 0 (unspecified) on %s", frameList(zero))
		if l.fix {
			for _, f := range frames {
				if f.gce != nil && f.gce.disposal == 0 {
					f.gce.setDisposal(1)
				}
			}
			l.dirty = true
			s += "; set to 1"
		}
		parts = append(parts, s)
	}
	if len(bad) > 0 {
		parts = append(parts, fmt.Sprintf("disposal 3 (restore previous) or a reserved value on %s, which Discord's decoder mishandles; not fixable without re-encoding", frameList(bad)))
	}
	detail := strings.Join(parts, "; ")
	if len(bad) > 0 {
		l.checks.fail(rule, LevelError, detail)
		return
	}
	l.checks.outcome(rule, LevelError, l.fix, detail)
}

// ruleNetscapeLoop: a NETSCAPE2.0 looping block must precede the first
// image (giflib attaches leading extensions to frame 0, so a block found
// only after the first image is ignored by giflib-based decoders such as
// Discord's). Its count N means "play N+1 times"; 0 = loop forever.
//
// For Discord targets the count must be 0 (Discord's GIF→WebP transcode
// honours a finite count and would stop the animation): non-zero counts
// are set to 0, a missing block is inserted with count 0. For TargetNone
// any count is acceptable — the recipe's Loop setting is the user's choice
// — and only reported; the block merely has to exist before the first
// image: a missing one is inserted right after the global colour table
// with count 0 (or with the file's own count when a block sits later in
// the stream), and a malformed loop sub-block is repaired the same way.
// Well-formed blocks are never rewritten for TargetNone.
func (l *gifLinter) ruleNetscapeLoop() {
	const rule = RuleGIFNetscapeLoop
	first := l.g.firstImageBlock()
	var before, after []*gifAppExt
	for i, b := range l.g.blocks {
		if app, ok := b.(*gifAppExt); ok && app.isNetscape() {
			if i < first {
				before = append(before, app)
			} else {
				after = append(after, app)
			}
		}
	}
	all := append(append([]*gifAppExt(nil), before...), after...)
	var counts []uint16 // distinct well-formed counts in stream order
	malformed := false
	for _, app := range all {
		if n, ok := app.loopCount(); !ok {
			malformed = true
		} else if !slices.Contains(counts, n) {
			counts = append(counts, n)
		}
	}
	// The count the fixer writes: 0 for Discord targets; the file's own
	// (first well-formed) count for TargetNone, 0 when it has none.
	mustZero := l.target != TargetNone
	want := uint16(0)
	if !mustZero && len(counts) > 0 {
		want = counts[0]
	}

	var problems []string
	switch {
	case len(all) == 0:
		problems = append(problems, "no NETSCAPE2.0 looping block (plays once)")
	case len(before) == 0:
		problems = append(problems, "NETSCAPE2.0 block appears only after the first frame, where giflib-based decoders (Discord's included) ignore it")
	}
	if malformed {
		problems = append(problems, "NETSCAPE2.0 block has a malformed loop sub-block")
	}
	if mustZero {
		for _, n := range counts {
			if n != 0 {
				problems = append(problems, fmt.Sprintf("NETSCAPE2.0 loop count %d (%s); Discord needs 0 = loop forever", n, gifPlays(n)))
			}
		}
	}
	if len(problems) == 0 {
		l.loopForever = len(counts) == 1 && counts[0] == 0
		detail := "NETSCAPE2.0 " + loopCountWords(counts) + " present before the first frame"
		if !l.loopForever {
			detail += "; Discord targets require 0 (loop forever)"
		}
		l.checks.pass(rule, LevelError, detail)
		return
	}
	detail := strings.Join(problems, "; ")
	if !l.fix {
		l.checks.fail(rule, LevelError, detail)
		return
	}
	var fixes []string
	changed := false
	for _, app := range all {
		if n, ok := app.loopCount(); !ok || (mustZero && n != 0) {
			app.setLoopCount(want)
			changed = true
		}
	}
	if changed {
		fixes = append(fixes, fmt.Sprintf("set to %d", want))
	}
	if len(before) == 0 {
		l.g.insertBlock(0, newNetscapeLoop(want))
		if want == 0 {
			fixes = append(fixes, "inserted loop-forever block after the global colour table")
		} else {
			fixes = append(fixes, fmt.Sprintf("inserted a block with loop count %d after the global colour table", want))
		}
	}
	l.dirty = true
	// After the fix every well-formed count is `want` for Discord targets;
	// for TargetNone the untouched counts stay as they were.
	l.loopForever = want == 0 && (mustZero || len(counts) <= 1)
	l.checks.fixed(rule, LevelError, detail+"; "+strings.Join(fixes, "; "))
}

// loopCountWords renders NETSCAPE loop counts for details: "loop count 0
// (loops forever)", "loop count 3 (plays 4 times)", "loop counts 0, 3".
func loopCountWords(counts []uint16) string {
	if len(counts) == 1 {
		return fmt.Sprintf("loop count %d (%s)", counts[0], gifPlays(counts[0]))
	}
	parts := make([]string, len(counts))
	for i, n := range counts {
		parts[i] = fmt.Sprint(n)
	}
	return "loop counts " + strings.Join(parts, ", ")
}

// gifPlays words a NETSCAPE2.0 loop count: 0 loops forever, N plays N+1
// times.
func gifPlays(n uint16) string {
	if n == 0 {
		return plays(0)
	}
	return plays(int(n) + 1)
}

// ruleMinDelay: every frame delay is >= 2 cs. Browsers render 0 and 1 cs as
// 100 ms and Discord rejects such stickers, so those are set to 10 cs.
func (l *gifLinter) ruleMinDelay() {
	const rule = RuleGIFMinDelay
	frames, _ := l.g.frames()
	var low []int
	minDelay := -1
	for _, f := range frames {
		if f.gce == nil {
			continue
		}
		d := int(f.gce.delayCS)
		if minDelay < 0 || d < minDelay {
			minDelay = d
		}
		if d < 2 {
			low = append(low, f.index)
		}
	}
	if len(low) == 0 {
		l.checks.pass(rule, LevelWarn, fmt.Sprintf("all delays >= 2 cs (minimum %d cs)", max(minDelay, 0)))
		return
	}
	detail := fmt.Sprintf("delays below 2 cs on %s (browsers show them as 100 ms; Discord rejects them for stickers)", frameList(low))
	if l.fix {
		for _, f := range frames {
			if f.gce != nil && f.gce.delayCS < 2 {
				f.gce.setDelay(gifDefaultDelayCS)
			}
		}
		l.dirty = true
		detail += fmt.Sprintf("; set to %d cs", gifDefaultDelayCS)
	}
	l.checks.outcome(rule, LevelWarn, l.fix, detail)
}

// ruleGlobalPalette: a single global colour table, no local ones (per-frame
// palettes have produced random-colour glitches on Discord). An error for
// Discord targets, a warning otherwise; a file with no palette at all is
// always an error.
func (l *gifLinter) ruleGlobalPalette() {
	const rule = RuleGIFGlobalPalette
	level := LevelWarn
	if l.target != TargetNone {
		level = LevelError
	}
	frames, _ := l.g.frames()
	var local, none []int
	for _, f := range frames {
		if f.image.hasLCT() {
			local = append(local, f.index)
		} else if !l.g.hasGCT() {
			none = append(none, f.index)
		}
	}
	if len(none) > 0 {
		l.checks.fail(rule, LevelError, fmt.Sprintf("no global colour table and no local one on %s", frameList(none)))
		return
	}
	if len(local) > 0 {
		l.checks.fail(rule, level, fmt.Sprintf("local colour table on %s (%d of %s); re-encode with one global palette", frameList(local), len(local), plural(len(frames), "frame")))
		return
	}
	l.checks.pass(rule, level, fmt.Sprintf("single global colour table (%d entries), no local tables", l.g.gctColors()))
}

// ruleNoInterlace: no interlaced frames.
func (l *gifLinter) ruleNoInterlace() {
	const rule = RuleGIFNoInterlace
	frames, _ := l.g.frames()
	var inter []int
	for _, f := range frames {
		if f.image.interlaced() {
			inter = append(inter, f.index)
		}
	}
	if len(inter) > 0 {
		l.checks.fail(rule, LevelWarn, fmt.Sprintf("interlaced: %s; not fixable without re-encoding", frameList(inter)))
		return
	}
	l.checks.pass(rule, LevelWarn, "no interlaced frames")
}

// ruleNoExtraExtensions: comment, plain-text, unknown and non-NETSCAPE
// application extensions (and GCEs that apply to no image) are stripped.
func (l *gifLinter) ruleNoExtraExtensions() {
	const rule = RuleGIFNoExtraExtensions
	var drop []int
	var kinds []string
	for i, b := range l.g.blocks {
		switch b := b.(type) {
		case *gifRawExt:
			drop = append(drop, i)
			kinds = append(kinds, extName(b.label))
		case *gifAppExt:
			if !b.isNetscape() {
				drop = append(drop, i)
				kinds = append(kinds, fmt.Sprintf("application extension %q", printableID(b.id())))
			}
		}
	}
	if _, dangling := l.g.frames(); len(dangling) > 0 {
		drop = append(drop, dangling...)
		kinds = append(kinds, plural(len(dangling), "Graphic Control Extension with no following image"))
	}
	if len(drop) == 0 {
		l.checks.pass(rule, LevelInfo, "no comment, plain-text or non-NETSCAPE application extensions")
		return
	}
	detail := "extra extensions present: " + strings.Join(kinds, ", ")
	if l.fix {
		l.g.removeBlocks(drop)
		l.dirty = true
		detail += "; removed"
	}
	l.checks.outcome(rule, LevelInfo, l.fix, detail)
}

func extName(label byte) string {
	switch label {
	case gifLabelComment:
		return "comment extension"
	case gifLabelPlainText:
		return "plain-text extension"
	}
	return fmt.Sprintf("extension 0x%02X", label)
}

// printableID renders an application identifier for a detail string.
func printableID(id string) string {
	if id == "" {
		return "(malformed)"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7E {
			return '?'
		}
		return r
	}, id)
}

// ruleFirstFrameVisible: frame 0 is not entirely transparent (Discord shows
// it as the still whenever the animation is not playing). Skipped when the
// pixels cannot be decoded.
func (l *gifLinter) ruleFirstFrameVisible() {
	const rule = RuleGIFFirstFrameVisible
	frames, _ := l.g.frames()
	if len(frames) == 0 {
		return
	}
	f0 := &frames[0]
	if f0.image.width == 0 || f0.image.height == 0 {
		l.checks.fail(rule, LevelWarn, "frame 0 has zero size")
		return
	}
	if f0.gce == nil || !f0.gce.transparent {
		l.checks.pass(rule, LevelWarn, "frame 0 is opaque")
		return
	}
	pix, err := l.pix.get(f0)
	if err != nil {
		return
	}
	if pix.usesOnly(f0.gce.transIndex) {
		l.checks.fail(rule, LevelWarn, "frame 0 is entirely transparent; Discord shows it as the still image when the animation is not playing")
		return
	}
	l.checks.pass(rule, LevelWarn, "frame 0 has visible pixels")
}

// ruleTrailer: the stream ends with the 0x3B trailer (giflib fails without
// it; browsers do not care).
func (l *gifLinter) ruleTrailer() {
	const rule = RuleGIFTrailer
	if l.g.hasTrailer {
		l.checks.pass(rule, LevelError, "trailer present")
		return
	}
	detail := "file ends without the 0x3B trailer (giflib-based decoders such as Discord's reject it)"
	if l.fix {
		l.g.hasTrailer = true
		l.dirty = true
		detail += "; appended"
	}
	l.checks.outcome(rule, LevelError, l.fix, detail)
}

// ruleSizeLimit checks the final byte count against the target's cap.
func (l *gifLinter) ruleSizeLimit(rule string, size int) {
	limit := Limit(l.target)
	if limit <= 0 {
		return
	}
	if int64(size) > limit {
		l.checks.fail(rule, LevelError, fmt.Sprintf("%d bytes exceeds the %d byte limit for %s", size, limit, l.target))
		return
	}
	l.checks.pass(rule, LevelError, fmt.Sprintf("%d of %d bytes", size, limit))
}

// ruleTargetShape applies the sticker / emote geometry and timing rules.
// Sticker dimensions only warn when a side exceeds 320 px: Discord shrinks
// larger stickers and accepts smaller or non-square ones (user-verified
// 2026-08-19), so they are not an error.
func (l *gifLinter) ruleTargetShape() {
	w, h := int(l.g.width), int(l.g.height)
	switch l.target {
	case TargetSticker:
		if dims := stickerDimsDetail(w, h); dims != "" {
			l.checks.fail(RuleGIFStickerDims, LevelWarn, dims)
		} else {
			l.checks.pass(RuleGIFStickerDims, LevelWarn, fmt.Sprintf("%dx%d fits %dx%d", w, h, stickerMaxSide, stickerMaxSide))
		}
		frames, _ := l.g.frames()
		duration, _ := gifTiming(frames)
		l.checks = append(l.checks, stickerDurationCheck(RuleGIFStickerDuration, len(frames), duration))
	case TargetEmote:
		if w <= 128 && h <= 128 {
			l.checks.pass(RuleGIFEmoteDims, LevelWarn, fmt.Sprintf("%dx%d fits 128x128", w, h))
		} else {
			l.checks.fail(RuleGIFEmoteDims, LevelWarn, fmt.Sprintf("%dx%d is larger than 128x128; Discord shrinks emoji to 128x128", w, h))
		}
	}
}

// Animated-sticker timing limits (DESIGN.md §5.1).
const (
	stickerMaxDurationMS = 5000
	stickerMaxFrames     = 1000
	stickerMaxFPS        = 60
)

// stickerDurationCheck evaluates the animated-sticker timing limits
// (<= 5 s, <= 1000 frames, <= 60 fps) for a duration given in whole
// milliseconds (GIF centisecond delays).
func stickerDurationCheck(rule string, frames, durationMS int) Check {
	return stickerLimitsCheck(rule, frames, int64(durationMS)*1000)
}

// stickerLimitsCheck evaluates the animated-sticker timing limits for a
// duration in microseconds. Microsecond precision matters for APNG: a 60 fps
// file has 1/60 s delays, which round to 17 ms per frame and would read as
// 58.8 fps (or 62.5 fps truncated) when summed in whole milliseconds.
func stickerLimitsCheck(rule string, frames int, durationUS int64) Check {
	durationMS := roundUSToMS(durationUS)
	var problems []string
	if durationMS > stickerMaxDurationMS {
		problems = append(problems, fmt.Sprintf("duration %d ms exceeds %d ms", durationMS, stickerMaxDurationMS))
	}
	if frames > stickerMaxFrames {
		problems = append(problems, fmt.Sprintf("%d frames exceeds %d", frames, stickerMaxFrames))
	}
	fps := 0.0
	switch {
	case durationUS > 0:
		fps = float64(frames) * 1e6 / float64(durationUS)
		if fps > stickerMaxFPS {
			problems = append(problems, fmt.Sprintf("%.1f fps exceeds %d fps", fps, stickerMaxFPS))
		}
	case frames > 1:
		problems = append(problems, "total duration is 0 ms (frame rate undefined)")
	}
	if len(problems) > 0 {
		return Check{Rule: rule, Level: LevelError, Detail: strings.Join(problems, "; ")}
	}
	return Check{Rule: rule, Level: LevelError, OK: true, Detail: fmt.Sprintf("%d frames, %d ms, %.1f fps", frames, durationMS, fps)}
}
