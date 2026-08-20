package enc

// Phase 2 builders: fit-search variants, APNG (RGBA and indexed 8-bit
// alpha), AVIF via avifenc, static images, frame extraction and the
// gifsicle-only GIF optimiser. All pure argv (no processes). See
// docs/DESIGN.md §4.2 rows APNG / Animated AVIF / PNG frames / Static /
// GIF → GIF, and §5.4 for the fit ladders.
//
// Every tool-facing shape here was verified against the runtime image
// (FFmpeg 9.0.1, gifsicle 1.96, pngquant 2.18, oxipng 10.2, libavif 1.2.1);
// phase2_tools_test.go re-runs those checks whenever the tools are on PATH.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Defaults applied for zero-valued Phase 2 options (DESIGN.md §4.2).
const (
	DefaultAPNGPred         = "mixed" // apng -pred
	DefaultPngquantSpeed    = 3       // pngquant --speed
	DefaultPngquantMinQ     = 70      // PngquantFileArgs --quality MIN when both bounds are 0
	DefaultPngquantMaxQ     = 100     // PngquantFileArgs --quality MAX when both bounds are 0
	DefaultOxipngLevel      = 2       // oxipng -o
	DefaultAVIFQuality      = 60      // avifenc -q
	DefaultAVIFAlphaQuality = 90      // avifenc --qalpha
	DefaultAVIFSpeed        = 8       // avifenc -s
	DefaultAVIFYUV          = "420"   // avifenc -y
	DefaultJPEGQuality      = 90      // JPEG quality 1..100 before the mjpeg -q:v mapping
	DefaultSequenceFPS      = 10      // SequenceInputArgs when fps <= 0 (1000 ms / the 100 ms default delay)
	// MaxTileWidth caps the sprite sheet TileGrid produces (cols*frameW):
	// well inside what PNG/pngquant handle and the 16384 px many decoders cap
	// textures at.
	MaxTileWidth = 16384
	// AVIFTimescale is the --timescale used for fractional frame rates: one
	// frame then lasts --duration 1000 ticks and the rate is exact to 1/1000
	// fps (avifenc 1.2.1 parses --fps with atoi, so 12.5 would become 12).
	AVIFTimescale = 1000
	// MinSVTDim is the smallest frame side SVT-AV1 encodes: libavif's "-c svt"
	// fails with "Source Width/Height must be at least 64" below it (verified
	// with SVT-AV1 2.3.0), so callers use Codec "svt" only when both sides are
	// >= MinSVTDim and leave Codec "" (libaom) otherwise.
	MinSVTDim = 64
	// AVIFDecPattern is the file name pattern avifdec writes for
	// AVIFDecArgs(in, dir): "<dir>/frame-0000000000.png", "…0001.png", …
	// (avifdec 1.2.1 appends "-%010d" to the output stem, counting from 0).
	AVIFDecPattern = "frame-%010d.png"
	// avifDecStem is the output name AVIFDecArgs passes to avifdec.
	avifDecStem = "frame.png"
	// mjpegMinQ/mjpegMaxQ bound the mjpeg -q:v scale (2 = best, 31 = worst).
	mjpegMinQ = 2
	mjpegMaxQ = 31
)

// Variant pre-filters the RGBA master before an encoder: an fps drop and/or
// a downscale. It is how fit-search rungs are realised without re-decoding
// the source. Zero fields = unchanged.
type Variant struct {
	FPS    float64 // output fps (0 = master fps); must be <= master fps
	Width  int     // target width (0 = master); Height 0 keeps aspect
	Height int
}

// VariantFilter returns the ffmpeg filter chain that turns the raw master
// into the variant, or "" when v is nil/zero. Shape:
//
//	fps=F:round=down,format=gbrap,premultiply=inplace=1,scale=W:H:flags=lanczos,unpremultiply=inplace=1,format=rgba
//
// (premultiplied lanczos when m.HasAlpha, plain lanczos otherwise; fps only
// when v.FPS > 0 and < m.FPS; scale only when the size changes). Encoders
// that take a Variant prepend "[0:v]<VariantFilter>[v]" to their graph and
// read from [v] instead of [0:v].
//
// round=down makes the fps filter round the input's end time DOWN onto the
// output grid, so a drop never lengthens the clip past the master: the
// default rounding rounds half up and a 5.0 s master dropped to 16.7 fps
// came out as 84 x 59.9 ms = 5.03 s, breaking the Discord sticker 5 s cap.
//
// Sizes: with one of Width/Height the other follows the master's aspect
// (rounded, >= 1, like graph's resize); with both the frame is scaled
// exactly. A variant never upscales — a target larger than the master is
// clamped to the master size. The fps stage is skipped for a single-frame
// master (a still has no rate to drop) and whenever the drop would leave no
// frame at all (Frames known and floor(Frames*FPS/masterFPS) < 1, e.g. two
// frames at 25 fps dropped to 10 fps) — ffmpeg would otherwise write an
// empty output.
func VariantFilter(m Master, v *Variant) string {
	if v == nil {
		return ""
	}
	var stages []string
	if fps := variantFPS(m, v); fps > 0 {
		stages = append(stages, "fps="+formatFloat(fps)+":round=down")
	}
	if w, h := variantSize(m, v); w != m.Width || h != m.Height {
		scale := "scale=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":flags=lanczos"
		if m.HasAlpha {
			stages = append(stages, "format=gbrap", "premultiply=inplace=1", scale, "unpremultiply=inplace=1", "format=rgba")
		} else {
			stages = append(stages, scale)
		}
	}
	return strings.Join(stages, ",")
}

// VariantMaster returns the master as the encoders see it after v: the
// variant's size and frame rate, and the frame count ffmpeg's fps filter
// yields for the drop — floor(Frames*newFPS/masterFPS): with round=down
// (VariantFilter's fps stage) the filter rounds the input's end time down
// onto the output grid and emits one frame per output slot before it, so a
// drop never lengthens the clip; verified against ffmpeg in
// phase2_tools_test.go (TestVariantFramesMatchFFmpeg). Frames 0 (unknown)
// stays 0; Path and HasAlpha are unchanged. Callers use it for TileGrid,
// UntileAPNGArgs (frames, fps), AVIFEncArgs (fps) and the result metadata.
func VariantMaster(m Master, v *Variant) Master {
	if v == nil {
		return m
	}
	out := m
	if fps := variantFPS(m, v); fps > 0 {
		if m.Frames > 0 {
			out.Frames = fpsFrames(m.Frames, masterFPS(m), fps)
		}
		out.FPS = fps
	}
	out.Width, out.Height = variantSize(m, v)
	return out
}

// fpsFrames is the number of frames ffmpeg's fps filter with round=down
// emits for frames input frames at rate in re-timed to rate out (the input's
// end time floored onto the output grid; the 1e-9 absorbs float error when
// the product is exact, e.g. 62 * 12.5 / 25 = 31).
func fpsFrames(frames int, in, out float64) int {
	return int(math.Floor(float64(frames)*out/in + 1e-9))
}

// variantFPS returns the fps stage's rate, or 0 when the variant keeps the
// master rate (no drop requested, a rate at/above the master's, a
// single-frame master, or a drop that would leave no frame).
func variantFPS(m Master, v *Variant) float64 {
	if v == nil || !(v.FPS > 0) || math.IsInf(v.FPS, 0) || m.Frames == 1 {
		return 0
	}
	if v.FPS >= masterFPS(m) {
		return 0
	}
	if m.Frames > 1 && fpsFrames(m.Frames, masterFPS(m), v.FPS) < 1 {
		return 0
	}
	return v.FPS
}

// variantSize returns the frame size after the variant's scale (the master
// size when nothing changes). Targets above the master are clamped to it.
func variantSize(m Master, v *Variant) (int, int) {
	if v == nil || m.Width <= 0 || m.Height <= 0 {
		return m.Width, m.Height
	}
	w, h := v.Width, v.Height
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	w, h = min(w, m.Width), min(h, m.Height)
	switch {
	case w > 0 && h > 0:
		return w, h
	case w > 0:
		return w, max(1, roundDiv(m.Height*w, m.Width))
	case h > 0:
		return max(1, roundDiv(m.Width*h, m.Height)), h
	default:
		return m.Width, m.Height
	}
}

// roundDiv returns a/b rounded to the nearest integer (half up) for a >= 0,
// b > 0.
func roundDiv(a, b int) int {
	return (a + b/2) / b
}

// variantLabel is the pad label the variant chain writes to.
const variantLabel = "[v]"

// variantPrefix returns the "[0:v]<variant>[v];" prefix and the label the
// rest of a graph reads from ("[v]", or "[0:v]" when there is no variant).
func variantPrefix(m Master, v *Variant) (prefix, in string) {
	f := VariantFilter(m, v)
	if f == "" {
		return "", "[0:v]"
	}
	return "[0:v]" + f + variantLabel + ";", variantLabel
}

// variantArgs returns the "-filter_complex [0:v]<variant>[v] -map [v]"
// pair for encoders whose graph is otherwise just the input, or nil when
// the variant is a no-op (so the argv stays byte-identical to Phase 1).
func variantArgs(m Master, v *Variant) []string {
	f := VariantFilter(m, v)
	if f == "" {
		return nil
	}
	return []string{"-filter_complex", "[0:v]" + f + variantLabel, "-map", variantLabel}
}

// plays maps recipe loop semantics (0 = forever, N = play N+1 times, < 0
// counts as 0) to a play count for muxers that store plays (apng -plays,
// webp -loop): 0 stays 0 (infinite), N becomes N+1 (capped at uint16).
func plays(loop int) int {
	if loop <= 0 {
		return 0
	}
	return min(loop, maxLoopCount-1) + 1
}

// --- APNG -------------------------------------------------------------------

// APNGOptions controls the APNG encoders. Zero values: Pred "mixed",
// Loop 0 (forever), Colors 0 = RGBA (truecolour); Colors 2..256 = indexed
// path (tile → pngquant → untile: TileGrid/TileArgs → PngquantArgs(colors)
// → UntileAPNGArgs → OxipngArgs; Colors itself is consumed by the caller
// when it builds that chain, APNGArgs ignores it).
type APNGOptions struct {
	Colors  int
	Loop    int    // recipe semantics: 0 = forever, N = play N+1 times → apng -plays N+1
	Pred    string // "mixed" (default) | "paeth" | "none"
	Variant *Variant
}

// apngPreds lists the png/apng encoder's -pred values.
var apngPreds = map[string]bool{"none": true, "sub": true, "up": true, "avg": true, "paeth": true, "mixed": true}

// pred returns the -pred value with the default applied.
func (o APNGOptions) pred() string {
	if apngPreds[o.Pred] {
		return o.Pred
	}
	return DefaultAPNGPred
}

// apngTail is the common "-c:v apng -pred P -plays N -f apng out" tail;
// pal8 inserts -pix_fmt pal8 for the indexed path.
func apngTail(o APNGOptions, pal8 bool, outPath string) []string {
	args := []string{"-c:v", "apng"}
	if pal8 {
		args = append(args, "-pix_fmt", "pal8")
	}
	return append(args,
		"-pred", o.pred(),
		"-plays", strconv.Itoa(plays(o.Loop)),
		"-f", "apng",
		outPath,
	)
}

// APNGArgs encodes the master as a truecolour RGBA APNG:
// [RawInputArgs] [-filter_complex variant -map [v]] -c:v apng -pred mixed
// -plays P -f apng outPath   (P = 0 for Loop 0, else Loop+1).
func APNGArgs(m Master, o APNGOptions, outPath string) []string {
	args := RawInputArgs(m)
	args = append(args, variantArgs(m, o.Variant)...)
	return append(args, apngTail(o, false, outPath)...)
}

// TileArgs renders every master frame (after the optional variant) into one
// CxR sprite sheet PNG (RGBA, -compression_level 1) so pngquant can build a
// single shared palette with alpha:
// [RawInputArgs] -filter_complex "[0:v]<variant,>tile=CxR[t]" -map [t] -frames:v 1 -c:v png -compression_level 1 outPNG
// Frames beyond C*R are dropped; the caller picks C*R >= frames (TileGrid).
// Unused cells of an alpha master are padded fully transparent
// (tile=…:color=0x00000000) so they share the palette's transparent entry
// instead of adding opaque black; an opaque master keeps tile's default.
func TileArgs(m Master, v *Variant, cols, rows int, outPNG string) []string {
	cols, rows = max(cols, 1), max(rows, 1)
	tile := "tile=" + strconv.Itoa(cols) + "x" + strconv.Itoa(rows)
	if m.HasAlpha {
		tile += ":color=0x00000000"
	}
	f := VariantFilter(m, v)
	if f != "" {
		f += ","
	}
	args := RawInputArgs(m)
	return append(args,
		"-filter_complex", "[0:v]"+f+tile+"[t]",
		"-map", "[t]",
		"-frames:v", "1",
		"-c:v", "png", "-compression_level", "1",
		outPNG,
	)
}

// TileGrid picks a near-square grid with cols*rows >= frames and
// cols*frameW <= 16384 (PNG/pngquant sanity): returns cols, rows.
// frames < 1 counts as 1; frameW <= 0 disables the width cap. Empty trailing
// columns are trimmed (cols = ceil(frames/rows)).
func TileGrid(frames, frameW, frameH int) (cols, rows int) {
	frames = max(frames, 1)
	cols = int(math.Ceil(math.Sqrt(float64(frames))))
	if frameW > 0 {
		cols = min(cols, max(1, MaxTileWidth/frameW))
	}
	rows = ceilDiv(frames, cols)
	cols = ceilDiv(frames, rows) // drop empty trailing columns
	return cols, rows
}

// ceilDiv returns ceil(a/b) for a >= 0, b > 0.
func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}

// PngquantArgs quantises a PNG to at most colors entries (2..256) with an
// alpha-aware shared palette: --nofs (ordered, no error diffusion, so frames
// do not shimmer) unless dither, --speed S (0 = 3), --force -o out in.
// Shape: [--nofs] COLORS --speed S --force -o out in.
func PngquantArgs(in, out string, colors int, dither bool, speed int) []string {
	args := make([]string, 0, 9)
	if !dither {
		args = append(args, "--nofs")
	}
	return append(args,
		strconv.Itoa(clampInt(colors, 2, 256, 256)),
		"--speed", strconv.Itoa(clampInt(speed, 1, 11, DefaultPngquantSpeed)),
		"--force", "-o", out, in,
	)
}

// UntileAPNGArgs slices the quantised sprite sheet back into frames and
// writes an indexed APNG (PLTE+tRNS preserved, verified on FFmpeg 9.0.1):
// -framerate FPS/(C*R) -i sheetPNG -filter_complex "[0:v]untile=CxR[f]"
// -map [f] -frames:v N -c:v apng -pix_fmt pal8 -pred mixed -plays P -f apng outPath
//
// The image2 input rate is FPS divided by the cell count: untile multiplies
// the input rate by C*R, so the frames come out at exactly FPS with exact
// delays (1/25, 2/25, 1000/33333 s — verified), and the untiled pal8 frames
// reach the encoder without any format conversion (pixels identical to the
// sheet). An output-side "-r FPS" must NOT be used: untile's native rate
// would be 25*C*R fps and -r would drop all but every (C*R)th frame (and
// "-loop 1" would then cycle the sheet); -frames:v N cuts the padding cells.
func UntileAPNGArgs(sheetPNG string, cols, rows, frames int, fps float64, o APNGOptions, outPath string) []string {
	cols, rows = max(cols, 1), max(rows, 1)
	if !(fps > 0) || math.IsInf(fps, 0) {
		fps = fallbackFPS
	}
	args := []string{
		"-framerate", formatFloat(fps) + "/" + strconv.Itoa(cols*rows),
		"-i", sheetPNG,
		"-filter_complex", "[0:v]untile=" + strconv.Itoa(cols) + "x" + strconv.Itoa(rows) + "[f]",
		"-map", "[f]",
		"-frames:v", strconv.Itoa(max(frames, 1)),
	}
	return append(args, apngTail(o, true, outPath)...)
}

// OxipngArgs optimises a PNG/APNG in place: -o L (0 = 2) --strip safe --quiet path.
// (oxipng 10 keeps acTL/fcTL/fdAT, so the APNG survives; verified.)
func OxipngArgs(path string, level int) []string {
	return []string{"-o", strconv.Itoa(clampInt(level, 0, 6, DefaultOxipngLevel)), "--strip", "safe", "--quiet", path}
}

// --- frame extraction ---------------------------------------------------------

// PNGFramesArgs writes every master frame (after the optional variant) as
// RGBA PNG files dir/<prefix>%05d.png with -compression_level L (1 for
// temporary frames, 6 for deliverables): [RawInputArgs] [-filter_complex
// variant -map [v]] -c:v png -compression_level L -start_number 1 pattern.
// L is clamped to 0..9 (0 as given = 0, ffmpeg's fastest).
func PNGFramesArgs(m Master, v *Variant, pattern string, compression int) []string {
	args := RawInputArgs(m)
	args = append(args, variantArgs(m, v)...)
	return append(args,
		"-c:v", "png", "-compression_level", strconv.Itoa(min(max(compression, 0), 9)),
		"-start_number", "1",
		pattern,
	)
}

// JPEGFramesArgs writes every frame flattened onto matte (RRGGBB, 0 =
// "313338") as JPEG (mjpeg -q:v Q, Q from quality 1..100 mapped to 2..31):
// filter "[0:v]<variant>[c];color=c=0xRRGGBB:s=WxH:r=FPS,format=rgba[bg];[bg][c]overlay=format=auto:shortest=1,format=yuvj420p[f]"
// (without a variant the overlay reads [0:v] directly — a split with an
// unconnected pad is an ffmpeg error), then -map [f] -c:v mjpeg -q:v Q
// -start_number 1 pattern. WxH/FPS are the variant's.
func JPEGFramesArgs(m Master, v *Variant, matte string, quality int, pattern string) []string {
	args := RawInputArgs(m)
	return append(args,
		"-filter_complex", flattenFilter(m, v, matte),
		"-map", "[f]",
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(mjpegQ(quality)),
		"-start_number", "1",
		pattern,
	)
}

// flattenFilter composites the (variant) master onto an opaque matte and
// hands mjpeg full-range 4:2:0 (yuvj420p; FFmpeg 9's mjpeg still lists it).
func flattenFilter(m Master, v *Variant, matte string) string {
	prefix, in := "", "[0:v]"
	if f := VariantFilter(m, v); f != "" {
		prefix, in = "[0:v]"+f+"[c];", "[c]"
	}
	vm := VariantMaster(m, v)
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("color=c=0x" + normalizeMatte(matte) + ":s=" + strconv.Itoa(vm.Width) + "x" + strconv.Itoa(vm.Height) + ":r=" + formatFloat(masterFPS(vm)) + ",format=rgba[bg];")
	b.WriteString("[bg]" + in + "overlay=format=auto:shortest=1,format=yuvj420p[f]")
	return b.String()
}

// mjpegQ maps a 1..100 quality (0 = 90) linearly onto mjpeg's -q:v scale
// 2 (best) .. 31 (worst): 100 → 2, 90 → 5, 50 → 17, 1 → 31.
func mjpegQ(quality int) int {
	q := clampInt(quality, 1, 100, DefaultJPEGQuality)
	return mjpegMaxQ - int(math.Round(float64(q-1)*float64(mjpegMaxQ-mjpegMinQ)/99))
}

// WebPFramesArgs writes every frame as a lossless still WebP (-c:v libwebp
// -lossless 1 -compression_level 4), pattern like dir/f%05d.webp:
// [RawInputArgs] [-filter_complex variant -map [v]] -c:v libwebp -lossless 1
// -compression_level 4 -start_number 1 pattern.
func WebPFramesArgs(m Master, v *Variant, pattern string) []string {
	args := RawInputArgs(m)
	args = append(args, variantArgs(m, v)...)
	return append(args,
		"-c:v", "libwebp", "-lossless", "1", "-compression_level", strconv.Itoa(DefaultWebPCompressionLevel),
		"-start_number", "1",
		pattern,
	)
}

// --- AVIF -------------------------------------------------------------------

// AVIFOptions controls avifenc. Zero values: Quality 60, AlphaQuality 90,
// Speed 8, YUV "420", Codec "" (avifenc default; "svt" when available for
// speed on opaque content), Loop 0 (forever → --repetition-count infinite;
// N → N+1 repetitions).
//
// Codec "svt" needs frames of at least MinSVTDim (64) px per side (SVT-AV1
// rejects smaller sources); it does encode alpha planes. Unknown codec names
// fall back to avifenc's default.
type AVIFOptions struct {
	Quality      int
	AlphaQuality int
	Speed        int
	YUV          string
	Codec        string
	Loop         int
}

// avifYUVs lists avifenc's -y values.
var avifYUVs = map[string]bool{"auto": true, "444": true, "422": true, "420": true, "400": true}

// avifCodecs lists the -c names libavif 1.2 knows; anything else falls back
// to avifenc's default (omitted).
var avifCodecs = map[string]bool{"aom": true, "svt": true, "rav1e": true}

// avifCommon returns "-j all -s S -q Q --qalpha A -y YUV [-c CODEC]".
func avifCommon(o AVIFOptions) []string {
	yuv := o.YUV
	if !avifYUVs[yuv] {
		yuv = DefaultAVIFYUV
	}
	args := []string{
		"-j", "all",
		"-s", strconv.Itoa(clampInt(o.Speed, 0, 10, DefaultAVIFSpeed)),
		"-q", strconv.Itoa(clampInt(o.Quality, 0, 100, DefaultAVIFQuality)),
		"--qalpha", strconv.Itoa(clampInt(o.AlphaQuality, 0, 100, DefaultAVIFAlphaQuality)),
		"-y", yuv,
	}
	if avifCodecs[o.Codec] {
		args = append(args, "-c", o.Codec)
	}
	return args
}

// AVIFEncArgs builds the avifenc argv for an animated AVIF from an ordered
// list of PNG frame files: -j all -s S -q Q --qalpha A -y YUV [-c CODEC]
// <timing> --repetition-count infinite|N frames... outPath.
//
// Timing: an integral fps is "--fps N"; a fractional one is "--timescale
// round(fps*1000) --duration 1000" (one frame = 1000 ticks), because
// avifenc 1.2.1 parses --fps with atoi (12.5 → 12; verified) — both forms
// give the same per-frame duration and the fractional one is exact to
// 1/1000 fps (29.97 → 29970/1000). fps <= 0 falls back to 25.
// --repetition-count N means N+1 plays (libavif writes an mvhd duration of
// (N+1) × the sequence; verified), i.e. recipe Loop N maps to N verbatim and
// Loop 0 (or negative) to "infinite". No frames → nil.
func AVIFEncArgs(frames []string, fps float64, o AVIFOptions, outPath string) []string {
	if len(frames) == 0 {
		return nil
	}
	args := avifCommon(o)
	args = append(args, avifTiming(fps)...)
	rep := "infinite"
	if o.Loop > 0 {
		rep = strconv.Itoa(o.Loop)
	}
	args = append(args, "--repetition-count", rep)
	args = append(args, frames...)
	return append(args, outPath)
}

// avifTiming renders the frame-rate flags described in AVIFEncArgs.
func avifTiming(fps float64) []string {
	if !(fps > 0) || math.IsInf(fps, 0) {
		fps = fallbackFPS
	}
	if fps == math.Trunc(fps) {
		return []string{"--fps", strconv.Itoa(int(fps))}
	}
	return []string{
		"--timescale", strconv.Itoa(int(math.Round(fps * AVIFTimescale))),
		"--duration", strconv.Itoa(AVIFTimescale),
	}
}

// AVIFStillArgs builds avifenc argv for one PNG: -j all -s S -q Q --qalpha A -y YUV [-c CODEC] in out.
func AVIFStillArgs(in string, o AVIFOptions, outPath string) []string {
	return append(avifCommon(o), in, outPath)
}

// AVIFDecArgs dumps every frame of an AVIF (alpha-safe) as PNG:
// -j all --index all -d 8 in <outDir>/frame.png. avifdec 1.2.1 then writes
// <outDir>/frame-0000000000.png, frame-0000000001.png, … (AVIFDecPattern,
// zero-based; RGBA when the file has alpha — verified). A still AVIF yields
// exactly frame-0000000000.png. outDir is joined with "/" (no OS separator)
// so the argv is stable; avifdec accepts either on Windows.
func AVIFDecArgs(in, outDir string) []string {
	return []string{"-j", "all", "--index", "all", "-d", "8", in, joinSlash(outDir, avifDecStem)}
}

// AVIFDecFrame returns the file name avifdec writes for frame i (0-based)
// of AVIFDecArgs(in, outDir).
func AVIFDecFrame(outDir string, i int) string {
	return joinSlash(outDir, fmt.Sprintf(AVIFDecPattern, max(i, 0)))
}

// --- static outputs ------------------------------------------------------------

// StillOptions controls the static outputs.
type StillOptions struct {
	Quality int    // jpeg 1..100 (0 = 90); webp/avif handled by their own options
	Matte   string // jpeg: RRGGBB flattened background (0 = "313338")
	Variant *Variant
}

// PNGStillArgs writes the master's FIRST frame as RGBA PNG:
// [RawInputArgs] [-filter_complex variant -map [v]] -frames:v 1 -c:v png -compression_level 6 out.
func PNGStillArgs(m Master, o StillOptions, outPath string) []string {
	args := RawInputArgs(m)
	args = append(args, variantArgs(m, o.Variant)...)
	return append(args,
		"-frames:v", "1",
		"-c:v", "png", "-compression_level", "6",
		outPath,
	)
}

// JPEGStillArgs writes the first frame flattened onto Matte as JPEG (mjpeg,
// -q:v from Quality): [RawInputArgs] -filter_complex <flatten> -map [f]
// -frames:v 1 -c:v mjpeg -q:v Q out (the flatten graph is JPEGFramesArgs').
func JPEGStillArgs(m Master, o StillOptions, outPath string) []string {
	args := RawInputArgs(m)
	return append(args,
		"-filter_complex", flattenFilter(m, o.Variant, o.Matte),
		"-map", "[f]",
		"-frames:v", "1",
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(mjpegQ(o.Quality)),
		outPath,
	)
}

// PngquantFileArgs is PngquantArgs specialised for a static output (quality
// range instead of colour count): pngquant --quality MIN-MAX --speed 3 --force -o out in.
// Both bounds 0 = the §4.2 default 70-100; otherwise each is clamped to
// 0..100 (MAX 0 = 100) and MIN is lowered to MAX when it exceeds it.
// pngquant exits 99 and writes nothing when MIN cannot be reached — the
// caller keeps the input PNG in that case.
func PngquantFileArgs(in, out string, minQ, maxQ int) []string {
	if minQ == 0 && maxQ == 0 {
		minQ, maxQ = DefaultPngquantMinQ, DefaultPngquantMaxQ
	}
	minQ = min(max(minQ, 0), 100)
	maxQ = clampInt(maxQ, 1, 100, 100)
	minQ = min(minQ, maxQ)
	return []string{
		"--quality", strconv.Itoa(minQ) + "-" + strconv.Itoa(maxQ),
		"--speed", strconv.Itoa(DefaultPngquantSpeed),
		"--force", "-o", out, in,
	}
}

// --- GIF → GIF ------------------------------------------------------------------

// GifsicleOptimizeOptions drives the no-decode GIF → GIF optimiser
// (ezgif "optimize" parity): lossy, colour reduction, drop every Nth frame
// with delays merged into the kept frames, resize (lossless timing edits
// only — never resizes quantised transparent GIFs, DESIGN §4.2), loop.
type GifsicleOptimizeOptions struct {
	Lossy      int
	Colors     int
	Dither     string // "" | "o8" | "ro64" | "floyd-steinberg" ...
	DropEveryN int    // 0 = keep all; 2 = drop every 2nd frame, 3 = every 3rd ... (delays of dropped frames are added to the previous kept frame)
	Loop       int    // recipe semantics
	Unoptimize bool   // -U first
	Careful    bool   // --careful
}

// GifsicleOptimizeArgs builds argv for GifsicleOptimizeOptions. delays is
// the source GIF's per-frame delay list in centiseconds (needed to merge
// delays when dropping frames; obtained via discordlint's GIF parser or
// image/gif). Shape: [-U] in.gif [frame selections with --delay per kept
// frame when DropEveryN > 0] -O2 [--careful] [--lossy=N] [--colors N
// [--dither=M]] --loopcount=forever|N -o out.
//
// Frame dropping: with DropEveryN = n every n-th frame (#n-1, #2n-1, …) is
// left out of the selection and its delay is added to the preceding kept
// frame, which is written as "--delay D #i" (the per-frame option precedes
// its frame; verified with gifsicle 1.96). Dropping implies -U: gifsicle
// deletes frames from an optimised GIF without coalescing, so the kept
// frames would lose the pixels the deleted ones carried (verified). With no
// delays (frame count unknown) DropEveryN is ignored; DropEveryN < 2 keeps
// every frame. Merged delays are capped at 65535 cs (the GCE field).
func GifsicleOptimizeArgs(in, out string, delays []int, o GifsicleOptimizeOptions) []string {
	drop := o.DropEveryN >= 2 && len(delays) > 0
	args := make([]string, 0, 12+3*len(delays))
	if o.Unoptimize || drop {
		args = append(args, "-U")
	}
	args = append(args, in)
	if drop {
		args = append(args, dropSelections(delays, o.DropEveryN)...)
	}
	args = append(args, "-O"+strconv.Itoa(DefaultGifsicleOptimize))
	if o.Careful {
		args = append(args, "--careful")
	}
	if o.Lossy > 0 {
		args = append(args, "--lossy="+strconv.Itoa(min(o.Lossy, 200)))
	}
	if o.Colors > 0 {
		args = append(args, "--colors", strconv.Itoa(clampInt(o.Colors, 2, 256, 256)))
		if o.Dither != "" {
			args = append(args, "--dither="+o.Dither)
		}
	}
	loop := "forever"
	if o.Loop > 0 {
		loop = strconv.Itoa(min(o.Loop, maxLoopCount))
	}
	return append(args, "--loopcount="+loop, "-o", out)
}

// dropSelections returns "--delay D #i" for every frame kept when each n-th
// frame is dropped, D being the frame's own delay plus those of the dropped
// frames that follow it (up to the next kept frame). A leading run of
// dropped frames cannot happen (frame 0 is always kept).
func dropSelections(delays []int, n int) []string {
	merged := MergeDroppedDelays(delays, n)
	sel := make([]string, 0, 3*len(merged))
	for _, f := range merged {
		sel = append(sel, "--delay", strconv.Itoa(f.Delay), "#"+strconv.Itoa(f.Index))
	}
	return sel
}

// KeptFrame is one frame GifsicleOptimizeArgs keeps when dropping frames:
// its index in the source GIF and its merged delay in centiseconds.
type KeptFrame struct {
	Index int
	Delay int
}

// MergeDroppedDelays returns the frames kept when every n-th frame of a GIF
// with the given per-frame delays (centiseconds) is dropped, each with the
// delays of the dropped frames that follow it folded in (so the animation
// keeps its total duration). n < 2 keeps every frame unchanged. Negative
// delays count as 0; merged delays are capped at 65535.
func MergeDroppedDelays(delays []int, n int) []KeptFrame {
	kept := make([]KeptFrame, 0, len(delays))
	for i, d := range delays {
		d = max(d, 0)
		if n >= 2 && (i+1)%n == 0 && len(kept) > 0 {
			kept[len(kept)-1].Delay = min(kept[len(kept)-1].Delay+d, maxLoopCount)
			continue
		}
		kept = append(kept, KeptFrame{Index: i, Delay: min(d, maxLoopCount)})
	}
	return kept
}

// --- image sequences ---------------------------------------------------------------

// SequenceInputArgs returns the image2 demuxer input args for an image
// sequence blob dir: -f image2 -framerate FPS -start_number 1 -i dir/<pattern>
// (pattern from recipe.SequenceInfo.Pattern). fps = 1000/delayMS; fps <= 0
// falls back to 10 (the 100 ms default delay).
//
// The demuxer is forced with "-f image2" (first, before the demuxer options)
// so the render opens the frames exactly like the probe does: without it
// ffmpeg guesses the demuxer from the pattern's extension and a pattern it
// does not recognise fails to open at all.
func SequenceInputArgs(dir, pattern string, fps float64) []string {
	if !(fps > 0) || math.IsInf(fps, 0) {
		fps = DefaultSequenceFPS
	}
	return []string{
		"-f", "image2",
		"-framerate", formatFloat(fps),
		"-start_number", "1",
		"-i", joinSlash(dir, pattern),
	}
}

// joinSlash joins a directory and a name with a single forward slash (no
// OS-specific separator, so argv goldens are stable; ffmpeg and the tools
// accept "/" on every platform).
func joinSlash(dir, name string) string {
	if dir == "" {
		return name
	}
	return strings.TrimRight(dir, "/\\") + "/" + name
}
