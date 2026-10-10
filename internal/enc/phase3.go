package enc

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// Phase 3 builders (DESIGN.md §4.3): content-box detection for the autocrop
// op, font enumeration for drawtext, and the conventions MasterArgs /
// StillArgs / StillArgsFromStart / ProxyArgs honour once a plan carries
// graph.Plan.ExtraInputs, Reversed or TextFiles. Every shape below was
// verified against FFmpeg 9.0.1 (phase3_ffmpeg_test.go re-runs the checks
// whenever ffmpeg is on PATH):
//
//   - every ExtraInput is emitted right after the main input as
//     [ExtraInput.Args...] -i ExtraInput.Path (ffmpeg input index = position
//     + 1), so the compiler's "[1:v]", "[2:v]" labels line up; MasterArgs and
//     ProxyArgs pass them through unchanged;
//   - a still keeps the render's own clock instead of re-basing time at its
//     seek point: StillArgs seeks the main input with "-ss S -itsoffset
//     (S - TrimStart)", which makes every decoded timestamp the absolute
//     output time the render assigns to that frame (the plan's fps grid,
//     its enable='gte(t+0.0001,S)*lt(t+0.0001,E)' windows — half-open
//     [S, E) with a 1e-4 s tolerance — and its overlay clocks all line
//     up), and the frame shown at t is selected by that absolute time. Extra
//     inputs therefore need no seek at all — they start at 0 exactly as in
//     the render, whatever their container: webp_anim cannot seek (nothing
//     is decoded after any -ss), gif and image2 lose the frame covering a
//     seek point, and a seeked animation would be off by the seek-back
//     otherwise. The cost is decoding an overlay from its start up to t,
//     which is negligible for the images this app composites;
//   - a reversed plan (graph.Plan.Reversed) on a CFR source (video, image
//     sequence) is seeked to source time TrimStart + (Duration - t)*Speed
//     with the same seek-back and grid snapping — additionally a whole
//     number of source frames after TrimStart, because ffmpeg cuts a seeked
//     input at "-to" by a duration counted from the first decoded frame and
//     the decode must end on the render's frame (see reversedSeekFor;
//     without such an alignment the decode starts at TrimStart) — keeps "-to
//     TrimEnd", and runs the plan unchanged: the reverse stage stamps the
//     forward timestamps onto the reversed frames (verified), so after a
//     seek the j-th reversed frame is the render's j-th frame and it is
//     selected by index (select='gte(n,j)'); its timestamps count from the
//     seek point, which is what the render's overlay clock reads at that
//     frame. A reversed plan on an animation source (graph.Plan.SourceVFR:
//     frames with individual delays, a seek that can land inside a hold) is
//     decoded from TrimStart instead, which is exact (verified on a GIF with
//     a 600 ms hold; the seek picked the wrong frame);
//   - a plan whose main source cannot be seeked (graph.Plan.SeekUnsafe —
//     animated WebP, whose demuxer decodes nothing after any input seek;
//     its trim, if any, is a filter stage, graph.Plan.FilterTrim) is never
//     seeked, forward or reversed, trimmed or not, by either still variant
//     (no -ss, -itsoffset or -to at all: one decode from the file's start
//     yields the frame, so the still never needs the from-start retry) nor
//     by ProxyArgs for a reversed plan's tail (the whole clip is decoded
//     and buffered). MasterArgs and CropDetectPlanArgs need nothing special
//     (the plan's InputArgs simply carry no seek);
//   - every builder expects a plan whose TextFiles are already bound
//     (graph.BindTextFiles) and whose ExtraInputs carry a Path — a
//     "textfile=__EZLG_TEXT_…" placeholder (or a TextFile.Placeholder) left
//     in the filter, or an ExtraInput without Path, is a programming error
//     the builders report by returning nil.

// Phase 3 constants.
const (
	// DefaultCropDetectLimit is cropdetect's black threshold for opaque
	// sources (8-bit units; ffmpeg's own default).
	DefaultCropDetectLimit = 24
	// CropDetectSampleMS is the minimum spacing, in milliseconds, between the
	// frames the detection pass analyses (5 frames per second of the trimmed
	// source).
	CropDetectSampleMS = "200"
	// textPlaceholderPrefix starts every drawtext placeholder the graph
	// compiler embeds as textfile=<placeholder>; see graph.TextFile.
	textPlaceholderPrefix = "__EZLG_TEXT_"
)

// cropSample decimates the source to at most one frame per
// CropDetectSampleSeconds without the fps filter: fps=5 emits nothing at all
// for a single image (it needs a second timestamp), whereas select passes
// the first frame and then every frame at least 0.2 s after the previously
// selected one — the same 5 fps on video, one frame on a still. The spacing
// is compared in whole milliseconds so float noise (0.6 - 0.4 < 0.2) cannot
// skip a frame.
const cropSample = "select='isnan(prev_selected_t)+gte(round((t-prev_selected_t)*1000)," + CropDetectSampleMS + ")'"

// CropDetectArgs runs ffmpeg over the (trimmed) raw source and prints the
// accumulated content box to stderr (one "crop=w:h:x:y" per analysed frame;
// the last line is the union, see ParseCropDetect). inputArgs are the plan's
// InputArgs (trim seek, decoder forcing, sequence pattern args); srcPath is
// the main input path (the caller joins a sequence pattern itself, as for
// MasterArgs). Shape:
//
//	[inputArgs...] -i src -vf <sample>,<detector> -an -f null -
//
// For alpha sources (alpha=true) the box is detected on the alpha plane,
// exactly: "format=rgba,alphaextract,lagfun=decay=1,bbox=min_val=T-1" —
// bbox reports the tight box of every pixel whose value is > min_val (so
// alpha >= threshold counts as content; threshold is clamped to 1..255, 0 =
// 1), and lagfun with decay 1 is a running per-pixel maximum, so each frame's
// box is the union of everything seen so far. cropdetect is deliberately not
// used here: it tests the AVERAGE of a row/column against its limit (a thin
// stroke or the sparse top rows of a round emote never pass), and its round
// option rounds x/y up to even and w/h down (round <= 1 silently means 16),
// cutting a column off content at odd offsets. Otherwise (opaque sources)
// the picture's black border is measured the conventional way,
// "cropdetect=limit=24:round=2:reset=0:skip=0" (reset=0 accumulates over
// every frame; skip=0 so a single image is analysed too). Sources whose
// alpha lives in a separate stream (recipe.ProbeInfo.AlphaStream > 0) are
// not handled: the chain reads the first video stream only — use
// CropDetectPlanArgs, whose plan merges the streams, for those.
//
// This is the builder for callers without a compiled plan; the autocrop op
// itself detects on the compiled picture through CropDetectPlanArgs, so a
// keyed source is cropped to its subject rather than to the screen.
//
// cropdetect and bbox log at info level: run this with ffrun.RunFFmpegLog
// (RunFFmpeg's -loglevel error would silence the report).
func CropDetectArgs(srcPath string, inputArgs []string, alpha bool, threshold int) []string {
	args := make([]string, 0, len(inputArgs)+8)
	args = append(args, inputArgs...)
	return append(args,
		"-i", srcPath,
		"-vf", cropDetectChain(alpha, threshold),
		"-an", "-f", "null", "-",
	)
}

// cropDetectOutLabel is the output pad of CropDetectPlanArgs' detector chain.
const cropDetectOutLabel = "[det]"

// CropDetectPlanArgs is CropDetectArgs over a compiled detection plan
// (graph.CompileDetect: the stack's detection-kind ops — trim, speed, fps,
// keying, feather — hoisted in front of the geometry wherever they sit, at
// the source frame size) instead of the raw source, so the box is
// found on the picture the crop is applied to: with a chromakey/colorkey op
// in front of the autocrop, a green-screen clip resolves to the subject's box
// where the raw-source path (opaque picture, cropdetect) reports the full
// frame. Shape:
//
//	[p.InputArgs...] -i src [extra inputs] -filter_complex "<p.Filter>;[out]<sample>,<detector>[det]" -map [det] -an -f null -
//
// The sampler and the detector are exactly CropDetectArgs' (alpha selects the
// alpha-plane bbox chain, otherwise cropdetect; callers pass p.HasAlpha,
// which keying sets — the plan's frames are rgba either way, cropdetect
// measures their luma). A single-frame plan (a still image; the compiler
// emits no fps stage for it) needs no looping: the sampler passes the first
// frame and bbox/cropdetect report it (skip=0). srcPath, the sequence pattern
// and the extra inputs follow MasterArgs; an unusable plan (an unbound text
// placeholder, an extra input without Path) yields nil. Run with the same
// info-level logging as CropDetectArgs and parse with ParseCropDetect.
func CropDetectPlanArgs(srcPath string, p *graph.Plan, alpha bool, threshold int) []string {
	if !planUsable(p) {
		return nil
	}
	args := make([]string, 0, len(p.InputArgs)+12)
	args = append(args, p.InputArgs...)
	args = append(args, "-i", inputPath(srcPath, p))
	args = append(args, extraInputArgs(p)...)
	return append(args,
		"-filter_complex", p.Filter+";"+outLabel(p)+cropDetectChain(alpha, threshold)+cropDetectOutLabel,
		"-map", cropDetectOutLabel,
		"-an", "-f", "null", "-",
	)
}

// cropDetectChain is the sampler plus the detector of a detection run (see
// CropDetectArgs): the alpha-plane bbox chain for alpha, else cropdetect.
func cropDetectChain(alpha bool, threshold int) string {
	chain := cropSample + ","
	if alpha {
		return chain + "format=rgba,alphaextract,lagfun=decay=1,bbox=min_val=" + strconv.Itoa(clampInt(threshold, 1, 255, 1)-1)
	}
	return chain + "cropdetect=limit=" + strconv.Itoa(DefaultCropDetectLimit) + ":round=2:reset=0:skip=0"
}

// ParseCropDetect extracts the content box from the stderr of a
// CropDetectArgs run: the union of every "crop=w:h:x:y" field found on the
// detectors' own log lines — those starting with "[Parsed_bbox_" or
// "[Parsed_cropdetect_" at column 0, like "[Parsed_bbox_3 @ …] n:4 pts:…
// x1:21 x2:81 y1:11 y2:51 w:61 h:41 crop=61:41:21:11 drawbox=…" or
// "[Parsed_cropdetect_2 @ …] x1:0 x2:107 … limit:24.000000
// crop=108:118:0:10". Every other line is ignored: at info level ffmpeg
// also echoes the input's (and the null output's) metadata tags as indented
// "    comment         : …" lines, and a tag that records a command line
// ("made with ffmpeg -vf crop=1:1:0:0") must not reach the union. Both
// detectors accumulate, so the union equals the last box; taking the union
// also tolerates a truncated log. Fields with a non-positive size
// (cropdetect prints a negative box when nothing exceeds its limit) or a
// negative offset are ignored. ok is false when no box was found (a fully
// transparent or flat clip).
func ParseCropDetect(stderr string) (w, h, x, y int, ok bool) {
	x1, y1, x2, y2 := 0, 0, -1, -1
	for line := range strings.Lines(stderr) {
		cw, ch, cx, cy, found := parseCropField(line)
		if !found {
			continue
		}
		if !ok {
			x1, y1, x2, y2, ok = cx, cy, cx+cw-1, cy+ch-1, true
			continue
		}
		x1, y1 = min(x1, cx), min(y1, cy)
		x2, y2 = max(x2, cx+cw-1), max(y2, cy+ch-1)
	}
	if !ok {
		return 0, 0, 0, 0, false
	}
	return x2 - x1 + 1, y2 - y1 + 1, x1, y1, true
}

// detectorLogPrefixes start the log lines of the two detectors ParseCropDetect
// reads (ffmpeg's filter logger prints "[Parsed_<filter>_<n> @ <addr>] …" at
// column 0; nothing else in the log — the metadata echoes are indented — can
// start like this).
var detectorLogPrefixes = []string{"[Parsed_bbox_", "[Parsed_cropdetect_"}

// detectorLine reports whether line was logged by a bbox or cropdetect
// filter instance.
func detectorLine(line string) bool {
	for _, prefix := range detectorLogPrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// parseCropField parses the "crop=w:h:x:y" field of one detector log line;
// a line that is not a detector's (see detectorLine) has no field.
func parseCropField(line string) (w, h, x, y int, ok bool) {
	if !detectorLine(line) {
		return 0, 0, 0, 0, false
	}
	const key = "crop="
	i := strings.LastIndex(line, key)
	if i < 0 {
		return 0, 0, 0, 0, false
	}
	field := line[i+len(key):]
	if end := strings.IndexAny(field, " \t\r\n"); end >= 0 {
		field = field[:end]
	}
	parts := strings.Split(field, ":")
	if len(parts) != 4 {
		return 0, 0, 0, 0, false
	}
	var v [4]int
	for k, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		v[k] = n
	}
	if v[0] <= 0 || v[1] <= 0 || v[2] < 0 || v[3] < 0 {
		return 0, 0, 0, 0, false
	}
	return v[0], v[1], v[2], v[3], true
}

// FcListArgs lists font faces for drawtext's fontconfig font= option, one
// line per face "family<TAB>style<TAB>file" (the first family name and the
// first style name of each face):
//
//	fc-list --format '%{family[0]}\t%{style[0]}\t%{file}\n'
//
// The backslash escapes are interpreted by fontconfig itself (verified with
// fontconfig 2.15), so the argument is passed literally.
func FcListArgs() []string {
	return []string{"--format", `%{family[0]}\t%{style[0]}\t%{file}\n`}
}

// Font is one drawtext-usable face.
type Font struct {
	Family string `json:"family"` // fontconfig family name, e.g. "DejaVu Sans"
	Style  string `json:"style"`  // e.g. "Bold"
	File   string `json:"file"`   // absolute path
}

// ParseFcList parses FcListArgs output into unique (family, style) faces
// sorted by family then style. A family or style that still lists aliases
// ("Noto Sans,Noto Sans Regular") is reduced to its first entry; a missing
// style counts as "Regular"; families whose name contains characters
// outside ASCII letters, digits, spaces and hyphens are dropped (the graph
// only accepts those, see recipe.TextParams.Font), as are malformed lines
// and faces without a file.
func ParseFcList(out string) []Font {
	seen := make(map[[2]string]bool)
	var fonts []Font
	for line := range strings.Lines(out) {
		f, ok := parseFcLine(line)
		if !ok {
			continue
		}
		key := [2]string{f.Family, f.Style}
		if seen[key] {
			continue
		}
		seen[key] = true
		fonts = append(fonts, f)
	}
	slices.SortFunc(fonts, func(a, b Font) int {
		return cmp.Or(strings.Compare(a.Family, b.Family), strings.Compare(a.Style, b.Style))
	})
	return fonts
}

// parseFcLine parses one "family<TAB>style<TAB>file" line.
func parseFcLine(line string) (Font, bool) {
	fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	if len(fields) != 3 {
		return Font{}, false
	}
	f := Font{
		Family: firstAlias(fields[0]),
		Style:  firstAlias(fields[1]),
		File:   strings.TrimSpace(fields[2]),
	}
	if f.Style == "" {
		f.Style = "Regular"
	}
	if f.File == "" || !validFontFamily(f.Family) {
		return Font{}, false
	}
	return f, true
}

// firstAlias returns the first entry of a comma-separated fontconfig value
// list, trimmed.
func firstAlias(s string) string {
	first, _, _ := strings.Cut(s, ",")
	return strings.TrimSpace(first)
}

// validFontFamily reports whether name is non-empty and made of ASCII
// letters, digits, spaces and hyphens only.
func validFontFamily(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '-':
		default:
			return false
		}
	}
	return true
}

// --- multi-input plans ---------------------------------------------------------

// planUsable reports whether the builders can emit argv for p: a non-nil
// plan with every drawtext placeholder bound and every extra input located.
func planUsable(p *graph.Plan) bool {
	if p == nil || strings.Contains(p.Filter, textPlaceholderPrefix) {
		return false
	}
	for _, tf := range p.TextFiles {
		if tf.Placeholder != "" && strings.Contains(p.Filter, tf.Placeholder) {
			return false
		}
	}
	for _, e := range p.ExtraInputs {
		if e.Path == "" {
			return false
		}
	}
	return true
}

// extraInputArgs returns "[Args...] -i Path" for every extra input, in
// order (ffmpeg input index = position + 1).
func extraInputArgs(p *graph.Plan) []string {
	var args []string
	for _, e := range p.ExtraInputs {
		args = append(args, e.Args...)
		args = append(args, "-i", e.Path)
	}
	return args
}
