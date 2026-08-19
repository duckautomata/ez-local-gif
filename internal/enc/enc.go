// Package enc builds argv for every encoder/tool invocation (DESIGN.md §4.2).
// It is pure (no processes are started here) and golden-file tested: each
// function returns the exact argument list that internal/jobs hands to
// internal/ffrun.
//
// Conventions:
//   - Returned slices never include the binary itself.
//   - ffmpeg invocations do NOT include -hide_banner/-nostdin/-y/-progress;
//     ffrun.RunFFmpeg adds those.
//   - All animated encoders read the RGBA master produced by MasterArgs via
//     RawInputArgs(m) ("-f rawvideo -pix_fmt rgba -s WxH -r FPS -i path").
//   - Builders never fail: out-of-range options are clamped to the documented
//     ranges and unknown enum values fall back to the documented default, so
//     the argv is always something ffmpeg accepts. A nil *graph.Plan yields
//     nil.
package enc

import (
	"math"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Defaults applied for zero-valued options (DESIGN.md §4.2).
const (
	DefaultColors               = 256      // GIF palette size
	MinColors                   = 2        // palettegen max_colors minimum without a reserved transparent slot
	MinColorsAlpha              = 3        // palettegen max_colors minimum with reserve_transparent=1 (2 real colours + the slot)
	DefaultBayerScale           = 3        // paletteuse bayer_scale
	DefaultAlphaThreshold       = 128      // GIF 1-bit alpha cut-off
	DefaultMatte                = "313338" // Discord dark background
	DefaultStatsMode            = "diff"   // palettegen stats_mode
	DefaultGifsicleOptimize     = 2        // gifsicle -O level (never 3 for Discord)
	DefaultWebPQuality          = 80       // libwebp_anim -q:v
	DefaultWebPCompressionLevel = 4        // libwebp_anim -compression_level
	DefaultProxyMaxWidth        = 360      // ProxyArgs maxW
	DefaultProxyMaxSeconds      = 10.0     // ProxyArgs maxSeconds
	DefaultAlphaScanFrames      = 60       // AlphaScanArgs maxFrames
	DefaultOutLabel             = "[out]"  // graph.Plan.OutLabel when unset

	// proxyMaxFPS is the frame-rate cap of the animated preview.
	proxyMaxFPS = 15
	// stillEndMargin keeps the still target strictly inside the source range
	// (seconds): the last decodable frame ends at the source end, so a target
	// exactly at TrimEnd/Duration would map to no frame at all.
	stillEndMargin = 0.001
	// stillMinSeekBack is the least distance (source seconds) the still seeks
	// before its target so at least one decodable frame precedes it even when
	// source timestamps jitter; stillUnknownFPSSeekBack applies when the
	// source rate is unknown.
	stillMinSeekBack        = 0.1
	stillUnknownFPSSeekBack = 0.5
	// stillPadSlack (output seconds) is how much longer than the target
	// offset the still's tpad clones the last frame, covering the fps
	// filter's rounding tail and t == Duration.
	stillPadSlack = 1
	// stillSlotEpsilon absorbs float noise when t*FPS lands on a slot
	// boundary (in slots).
	stillSlotEpsilon = 1e-6
	// fallbackFPS is used when a Master reports no frame rate; it equals the
	// rawvideo demuxer's own default so behaviour matches omitting -r.
	fallbackFPS = 25
)

// Master describes the decoded RGBA rawvideo master on tmpfs.
type Master struct {
	Path     string // frames.rgba
	Width    int
	Height   int
	FPS      float64
	Frames   int  // 0 if unknown before rendering; filled from file size / (W*H*4) afterwards
	HasAlpha bool // any pixel with alpha < 255 (scanned after render)
}

// MasterArgs renders the plan to an RGBA rawvideo file:
// [plan.InputArgs...] -i src -filter_complex <plan.Filter> -map [out]
// -f rawvideo -pix_fmt rgba outPath
func MasterArgs(srcPath string, p *graph.Plan, outPath string) []string {
	if p == nil {
		return nil
	}
	args := make([]string, 0, len(p.InputArgs)+14)
	args = append(args, p.InputArgs...)
	args = append(args,
		"-i", srcPath,
		"-filter_complex", p.Filter,
		"-map", outLabel(p),
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgba",
		outPath,
	)
	return args
}

// StillArgs renders the frame the output shows at time t (seconds, output
// time) at most maxW pixels wide (0 = plan width) as PNG on stdout:
// [-ss S] … -i src -frames:v 1 -filter_complex "<plan.Filter>;[out]tpad=…,
// select=…[,scale][outs]" -map [outs] -c:v png -compression_level 1
// -f image2pipe pipe:1.
//
// t is an output-time offset: the wanted source time is TrimStart + t*Speed
// (Speed <= 0 counts as 1), clamped to >= TrimStart and to just inside the
// source range (TrimEnd, else TrimStart + Duration*Speed when the duration is
// known). The seek S lies a little BEFORE that target — at least
// 2/SourceFPS (0.5 s when unknown), never less than 0.1 s, plus one output
// frame period — and is snapped onto the plan's output frame grid
// (TrimStart + k*Speed/FPS), so the plan's fps stage produces the same frame
// slots as the real render; a `-ss` at or past the last decodable frame
// would otherwise yield no image at all near the clip end. The plan filter
// runs unchanged (its fps stage also gives tpad a frame rate) and is
// followed by tpad=stop_mode=clone (the last frame is held for t up to
// Duration and through the fps filter's rounding tail) and a select for the
// slot displayed at t. When maxW > 0 the selected frame is scaled to at most
// maxW wide, wrapped in premultiply/unpremultiply when the plan has alpha
// (the same chain graph uses, so the preview shows no dark fringes the
// output does not have). -ss is omitted when S == 0 (some animation
// demuxers, e.g. FFmpeg 9's webp_anim, return nothing after any seek).
//
// Any -ss/-to/-t/-sseof pairs in plan.InputArgs are dropped because the seek
// replaces them; every other input option (e.g. -c:v libvpx-vp9) is kept.
// The output label is always [outs].
//
// The seek relies on frames existing at or after S; variable-frame-rate
// sources whose last frame is held for longer than the seek-back (a GIF
// ending in a 2 s hold) yield no image for t inside that hold. Callers
// should retry an empty result with StillArgsFromStart.
func StillArgs(srcPath string, p *graph.Plan, t float64, maxW int) []string {
	if p == nil {
		return nil
	}
	return stillArgs(srcPath, p, stillSeekFor(p, t, false), maxW)
}

// StillArgsFromStart is StillArgs without the seek-back: it decodes from
// TrimStart (no -ss when that is 0) and lets the plan's fps stage and tpad
// carry every source frame — including a held last frame — up to t. It is
// exact (the frame slots are the render's own) but costs a decode of
// TrimStart..t, so it is meant as the fallback when StillArgs produced no
// image.
func StillArgsFromStart(srcPath string, p *graph.Plan, t float64, maxW int) []string {
	if p == nil {
		return nil
	}
	return stillArgs(srcPath, p, stillSeekFor(p, t, true), maxW)
}

// stillArgs assembles the still argv for a computed seek.
func stillArgs(srcPath string, p *graph.Plan, s stillSeek, maxW int) []string {
	var f strings.Builder
	f.WriteString(p.Filter)
	f.WriteString(";")
	f.WriteString(outLabel(p))
	f.WriteString("tpad=stop_mode=clone:stop_duration=" + formatFloat(s.pad))
	f.WriteString(",select='gte(t," + formatFloat(s.threshold) + ")'")
	if maxW > 0 {
		f.WriteString("," + previewScale(p, maxW))
	}
	f.WriteString("[outs]")

	input := stripSeekArgs(p.InputArgs)
	args := make([]string, 0, len(input)+18)
	if s.start > 0 {
		args = append(args, "-ss", formatFloat(s.start))
	}
	args = append(args, input...)
	args = append(args,
		"-i", srcPath,
		"-frames:v", "1",
		"-filter_complex", f.String(),
		"-map", "[outs]",
		"-c:v", "png", "-compression_level", "1",
		"-f", "image2pipe", "pipe:1",
	)
	return args
}

// ProxyArgs renders a low-res animated WebP preview of the first maxSeconds
// (0 = 10) at most maxW wide (0 = 360), fps <= 15, -q:v 60
// -compression_level 0 -loop 0, to outPath.
//
// The plan's input args (including any trim seek) are kept; -t is applied
// as an output option so it composes with an input-side -to and counts
// output seconds (i.e. after any speed change). fps=15 is inserted unless
// the plan already runs at <= 15 fps. The scale to maxW is wrapped in
// premultiply/unpremultiply when the plan has alpha (as graph does for the
// real render) so the preview shows no dark edge fringes the output lacks.
func ProxyArgs(srcPath string, p *graph.Plan, maxW int, maxSeconds float64, outPath string) []string {
	if p == nil {
		return nil
	}
	if maxW <= 0 {
		maxW = DefaultProxyMaxWidth
	}
	if !(maxSeconds > 0) {
		maxSeconds = DefaultProxyMaxSeconds
	}
	var f strings.Builder
	f.WriteString(p.Filter)
	f.WriteString(";")
	f.WriteString(outLabel(p))
	if !(p.FPS > 0 && p.FPS <= proxyMaxFPS) {
		f.WriteString("fps=" + strconv.Itoa(proxyMaxFPS) + ",")
	}
	f.WriteString(previewScale(p, maxW) + "[outp]")

	pix := "yuv420p"
	if p.HasAlpha {
		pix = "yuva420p"
	}
	args := make([]string, 0, len(p.InputArgs)+30)
	args = append(args, p.InputArgs...)
	args = append(args,
		"-i", srcPath,
		"-filter_complex", f.String(),
		"-map", "[outp]",
		"-an", "-sn", "-dn",
		"-t", formatFloat(maxSeconds),
		"-c:v", "libwebp_anim",
		"-lossless", "0",
		"-q:v", "60",
		"-compression_level", "0",
		"-pix_fmt", pix,
		"-loop", "0",
		"-map_metadata", "-1",
		"-f", "webp",
		outPath,
	)
	return args
}

// RawInputArgs returns "-f rawvideo -pix_fmt rgba -s WxH -r FPS -i m.Path".
// A Master without a frame rate (FPS <= 0) is read at 25 fps, the rawvideo
// demuxer's own default.
func RawInputArgs(m Master) []string {
	return []string{
		"-f", "rawvideo",
		"-pix_fmt", "rgba",
		"-s", strconv.Itoa(m.Width) + "x" + strconv.Itoa(m.Height),
		"-r", formatFloat(masterFPS(m)),
		"-i", m.Path,
	}
}

// GIFOptions controls the ffmpeg palette pipeline (DESIGN.md §4.2 GIF row).
// Zero values take the defaults noted.
type GIFOptions struct {
	// Colors is palettegen's max_colors, 2..256 (0 = 256). With HasAlpha the
	// count includes the reserved transparent slot and palettegen refuses
	// max_colors=2 ("only allowed without reserving a transparent color
	// slot", verified with FFmpeg 9.0.1), so the minimum is 3.
	Colors         int
	Dither         string // "bayer" (default, bayer_scale=BayerScale), "sierra2_4a", "floyd_steinberg", "none"
	BayerScale     int    // 0 = 3
	AlphaThreshold int    // 1..255 (0 = 128)
	Matte          string // RRGGBB (0 = "313338"); semi-transparent pixels are composited onto it before thresholding
	// Loop is the gif muxer's -loop: 0 = forever, N > 0 = NETSCAPE count N
	// (the animation plays N+1 times), -1 = no NETSCAPE block (plays once).
	Loop      int
	StatsMode string // palettegen stats_mode (0 = "diff")
	HasAlpha  bool   // when false, skip the matte/alphaextract chain and use reserve_transparent=0
}

// gifDithers lists every paletteuse dither mode ffmpeg accepts; anything
// else falls back to bayer.
var gifDithers = map[string]bool{
	"bayer": true, "heckbert": true, "floyd_steinberg": true, "sierra2": true,
	"sierra2_4a": true, "sierra3": true, "burkes": true, "atkinson": true, "none": true,
}

// gifStatsModes lists the accepted palettegen stats_mode values. "single"
// (one palette per frame) is deliberately excluded: it needs paletteuse
// new=1 and yields local colour tables, which DESIGN.md §5.3 forbids.
var gifStatsModes = map[string]bool{"full": true, "diff": true}

// normalized returns a copy with defaults applied and every field clamped
// to what ffmpeg accepts.
func (o GIFOptions) normalized() GIFOptions {
	minColors := MinColors
	if o.HasAlpha {
		minColors = MinColorsAlpha // one slot is the reserved transparent index
	}
	o.Colors = clampInt(o.Colors, minColors, 256, DefaultColors)
	// paletteuse accepts bayer_scale 0..5, but the zero value means "default"
	// here, so the reachable range is 1..5.
	o.BayerScale = clampInt(o.BayerScale, 1, 5, DefaultBayerScale)
	o.AlphaThreshold = clampInt(o.AlphaThreshold, 1, 255, DefaultAlphaThreshold)
	o.Matte = normalizeMatte(o.Matte)
	if o.Loop < -1 {
		o.Loop = -1 // gif muxer: -1 = no loop, 0 = forever, N = NETSCAPE count N
	} else if o.Loop > maxLoopCount {
		o.Loop = maxLoopCount
	}
	if !gifStatsModes[o.StatsMode] {
		o.StatsMode = DefaultStatsMode
	}
	if !gifDithers[o.Dither] {
		o.Dither = "bayer"
	}
	return o
}

// ditherArg renders the paletteuse dither option(s).
func (o GIFOptions) ditherArg() string {
	if o.Dither == "bayer" {
		return "bayer:bayer_scale=" + strconv.Itoa(o.BayerScale)
	}
	return o.Dither
}

// GIFArgs encodes the master to a GIF with a single global palette:
// [RawInputArgs] -filter_complex "<matte+threshold chain>;palettegen;paletteuse"
// -loop N -f gif outPath. Must produce: GCE on every frame, disposal
// 1/2 only, NETSCAPE loop, delays >= 2 cs. The delays follow from the
// master rate alone: the gif muxer rounds every pts to its 1/100 s timebase,
// so a master at <= 50 fps (graph.SnapFPS's GIF cap) never yields a delay
// below 2 cs and a 30 fps master gets 3,4,3 cs delays with an exact total.
func GIFArgs(m Master, o GIFOptions, outPath string) []string {
	o = o.normalized()
	args := RawInputArgs(m)
	args = append(args,
		"-filter_complex", gifFilter(m, o),
		"-map", "[out]",
		"-loop", strconv.Itoa(o.Loop),
		"-f", "gif",
		outPath,
	)
	return args
}

// gifFilter builds the palette filtergraph. With alpha, semi-transparent
// pixels are composited onto the matte and the alpha is thresholded to 1
// bit before quantisation, so the palette is computed on what Discord
// actually shows and edges never fringe. max_colors includes the reserved
// transparent slot, so it is passed unchanged (normalized keeps it >= 3
// with alpha).
func gifFilter(m Master, o GIFOptions) string {
	var b strings.Builder
	if o.HasAlpha {
		b.WriteString("[0:v]split[c][a];")
		b.WriteString("[a]alphaextract,lut=c0='gte(val," + strconv.Itoa(o.AlphaThreshold) + ")*255'[m];")
		b.WriteString("color=c=0x" + o.Matte + ":s=" + strconv.Itoa(m.Width) + "x" + strconv.Itoa(m.Height) + ":r=" + formatFloat(masterFPS(m)) + ",format=rgba[bg];")
		b.WriteString("[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];")
		b.WriteString("[f][m]alphamerge,split[p1][p2];")
		b.WriteString("[p1]palettegen=max_colors=" + strconv.Itoa(o.Colors) + ":reserve_transparent=1:stats_mode=" + o.StatsMode + "[pal];")
		b.WriteString("[p2][pal]paletteuse=dither=" + o.ditherArg() + ":diff_mode=rectangle:alpha_threshold=128[out]")
		return b.String()
	}
	b.WriteString("[0:v]split[p1][p2];")
	b.WriteString("[p1]palettegen=max_colors=" + strconv.Itoa(o.Colors) + ":reserve_transparent=0:stats_mode=" + o.StatsMode + "[pal];")
	b.WriteString("[p2][pal]paletteuse=dither=" + o.ditherArg() + ":diff_mode=rectangle[out]")
	return b.String()
}

// GifsicleOptions controls the post-pass. Zero values: OptimizeLevel 2,
// Careful true (set NoCareful to disable), Lossy 0 = off, Colors 0 = keep,
// Loop 0 = forever.
type GifsicleOptions struct {
	Lossy         int
	Colors        int
	OptimizeLevel int  // 1..3 (0 = 2). Never 3 for Discord targets.
	NoCareful     bool // default is --careful
	Unoptimize    bool // -U first (coalesce) — used by the re-encode fallback ladder
	Threads       int  // -j N (0 = omit)
	// Dither selects gifsicle's --dither method when Colors > 0 ("" = no
	// dithering; "o8" = ordered 8x8 as in DESIGN.md §4.2; other gifsicle
	// methods such as "ro64", "o3", "o4", "ordered", "halftone",
	// "floyd-steinberg" pass through unchecked).
	Dither string
	// Loop is the NETSCAPE loop count to write, with recipe.Output.Loop
	// semantics: 0 = forever (--loopcount=forever), N > 0 = --loopcount=N
	// (the animation plays N+1 times). Negative counts as 0. Callers pass
	// Output.Loop so the post-pass does not overwrite the count ffmpeg wrote.
	Loop int
}

// GifsicleArgs returns e.g. ["-O2","--careful","--lossy=40","--colors","128",
// "--loopcount=forever","in.gif","-o","out.gif"].
//
// gifsicle applies options positionally, so the order is fixed: -U first
// (coalesce before anything else), then -O<level>, --careful, --lossy=N,
// --colors N [--dither=M], -jN, and finally --loopcount=forever|N in -o out.
func GifsicleArgs(in, out string, o GifsicleOptions) []string {
	args := make([]string, 0, 12)
	if o.Unoptimize {
		args = append(args, "-U")
	}
	level := o.OptimizeLevel
	if level < 1 {
		level = DefaultGifsicleOptimize
	} else if level > 3 {
		level = 3
	}
	args = append(args, "-O"+strconv.Itoa(level))
	if !o.NoCareful {
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
	if o.Threads > 0 {
		args = append(args, "-j"+strconv.Itoa(o.Threads))
	}
	loop := "forever"
	if o.Loop > 0 {
		loop = strconv.Itoa(min(o.Loop, maxLoopCount))
	}
	args = append(args, "--loopcount="+loop, in, "-o", out)
	return args
}

// maxLoopCount is the largest count a NETSCAPE block / WebP ANIM chunk can
// hold (uint16).
const maxLoopCount = 65535

// WebPOptions controls libwebp_anim. Zero values: Quality 80,
// CompressionLevel 4, Loop 0 (forever).
type WebPOptions struct {
	Quality          int
	Lossless         bool
	CompressionLevel int
	// Loop has recipe.Output.Loop (GIF NETSCAPE) semantics: 0 = forever,
	// N > 0 = play N+1 times. The webp muxer's -loop is written verbatim into
	// the ANIM chunk's loop count, which is the number of PLAYS (0 =
	// infinite), so WebPArgs passes N+1. Negative counts as 0.
	Loop int
}

// WebPArgs encodes the master with the WebPAnimEncoder path only:
// [RawInputArgs] -c:v libwebp_anim -lossless 0|1 -q:v Q -compression_level L
// -pix_fmt yuva420p|bgra -loop P -map_metadata -1 -f webp outPath.
// (yuva420p for lossy, bgra for lossless; P = 0 for Loop 0, else Loop+1.)
//
// -q:v is omitted for lossless output (libwebp then uses its default
// effort). Lossy output uses yuv420p when the master has no alpha so the
// VP8X ALPHA flag is only set when frames really carry alpha (§5.3).
func WebPArgs(m Master, o WebPOptions, outPath string) []string {
	args := RawInputArgs(m)
	// A single-frame master becomes a plain still WebP: WebPAnimEncoder would
	// wrap one frame in VP8X+ANIM+ANMF, which Discord's lint (webp.anim-flag)
	// rightly rejects. The legacy libwebp encoder is fine for stills — the
	// ghost-trail bug only concerns animations.
	still := m.Frames == 1
	if still {
		args = append(args, "-frames:v", "1", "-c:v", "libwebp")
	} else {
		args = append(args, "-c:v", "libwebp_anim")
	}
	pix := "yuv420p"
	switch {
	case o.Lossless:
		args = append(args, "-lossless", "1")
		pix = "bgra"
	default:
		args = append(args, "-lossless", "0", "-q:v", strconv.Itoa(clampInt(o.Quality, 1, 100, DefaultWebPQuality)))
		if m.HasAlpha {
			pix = "yuva420p"
		}
	}
	// NETSCAPE count N → N+1 plays; 0 stays 0 (infinite).
	plays := 0
	if o.Loop > 0 {
		plays = min(o.Loop, maxLoopCount-1) + 1
	}
	args = append(args,
		"-compression_level", strconv.Itoa(clampInt(o.CompressionLevel, 0, 6, DefaultWebPCompressionLevel)),
		"-pix_fmt", pix,
	)
	if !still {
		args = append(args, "-loop", strconv.Itoa(plays))
	}
	args = append(args,
		"-map_metadata", "-1",
		"-f", "webp",
		outPath,
	)
	return args
}

// VerifyDecodeArgs decodes a file completely, discarding output, so a
// non-zero exit or stderr noise reveals corruption: -i path -f null -.
func VerifyDecodeArgs(path string) []string {
	return []string{"-i", path, "-f", "null", "-"}
}

// FrameCountArgs counts decodable frames: -i path -map 0:v:0 -f null - is
// not enough on its own; ffrun parses the final "frame=" from -progress.
// (Kept here so jobs has one place for argv.)
func FrameCountArgs(path string) []string {
	return []string{"-i", path, "-map", "0:v:0", "-f", "null", "-"}
}

// ProbeArgs returns ffprobe args producing JSON with format + streams:
// -v error -print_format json -show_format -show_streams path
func ProbeArgs(path string) []string {
	return []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path}
}

// AlphaScanArgs decodes up to maxFrames frames of path to raw 8-bit alpha
// (rgba → alphaextract → gray) on stdout so the caller can check whether any
// byte is < 255: -i path -frames:v N -vf format=rgba,alphaextract -f rawvideo
// -pix_fmt gray pipe:1
func AlphaScanArgs(path string, maxFrames int) []string {
	if maxFrames <= 0 {
		maxFrames = DefaultAlphaScanFrames
	}
	return []string{
		"-i", path,
		"-frames:v", strconv.Itoa(maxFrames),
		"-vf", "format=rgba,alphaextract",
		"-f", "rawvideo", "-pix_fmt", "gray",
		"pipe:1",
	}
}

// --- helpers ---------------------------------------------------------------

// outLabel returns the plan's output pad label, defaulting to "[out]".
func outLabel(p *graph.Plan) string {
	if p.OutLabel == "" {
		return DefaultOutLabel
	}
	return p.OutLabel
}

// masterFPS returns the master frame rate with the documented fallback.
func masterFPS(m Master) float64 {
	if m.FPS > 0 && !math.IsInf(m.FPS, 0) {
		return m.FPS
	}
	return fallbackFPS
}

// previewScale returns the filter stage that scales a preview (still or
// proxy) to at most maxW pixels wide, keeping aspect. With alpha the scale
// is wrapped exactly like graph's render-side scale (planar gbrap,
// premultiply → lanczos → unpremultiply → rgba): scaling straight alpha
// bleeds the (transparent-black) colour of see-through neighbours into edge
// pixels, which shows as a dark fringe over the light backdrops the preview
// is judged on. Alpha-less plans keep the plain scale.
func previewScale(p *graph.Plan, maxW int) string {
	scale := "scale=w='min(iw," + strconv.Itoa(maxW) + ")':h=-1:flags=lanczos"
	if !p.HasAlpha {
		return scale
	}
	return "format=gbrap,premultiply=inplace=1," + scale + ",unpremultiply=inplace=1,format=rgba"
}

// stillSeek is the resolved timing of one still render.
type stillSeek struct {
	start     float64 // -ss value in source seconds (0 = no seek)
	threshold float64 // select 'gte(t,threshold)' in output seconds after start
	pad       float64 // tpad stop_duration in output seconds
}

// stillSeekFor maps an output-time offset t to the seek StillArgs uses.
//
// The target source time is TrimStart + t*Speed, clamped to
// [TrimStart, srcEnd - stillEndMargin] where srcEnd is TrimEnd, else
// TrimStart + Duration*Speed when the duration is known. The output slot
// displayed at t is k = floor(tOut*FPS) (tOut = clamped t); the seek start
// is snapped down onto the slot grid, K = floor((target - back -
// TrimStart)*FPS/Speed) slots after TrimStart, so the fps stage after the
// seek emits slot j = k-K exactly where the render emits slot k. The select
// threshold sits half a slot before slot j (robust to float ties) and the
// tpad clones the last frame for j/FPS + stillPadSlack seconds so an early
// end of input still yields the (held) last frame. fromStart forces K = 0.
// A plan without FPS is treated as fallbackFPS.
func stillSeekFor(p *graph.Plan, t float64, fromStart bool) stillSeek {
	if !(t > 0) { // also catches NaN
		t = 0
	}
	speed := p.Speed
	if !(speed > 0) || math.IsInf(speed, 0) {
		speed = 1
	}
	fps := p.FPS
	if !(fps > 0) || math.IsInf(fps, 0) {
		fps = fallbackFPS
	}
	trimStart := math.Max(p.TrimStart, 0)

	// Wanted source time, kept inside the source.
	target := trimStart + t*speed
	if math.IsNaN(target) || math.IsInf(target, 0) {
		target = trimStart
	}
	srcEnd := 0.0
	switch {
	case p.TrimEnd > 0:
		srcEnd = p.TrimEnd
	case p.Duration > 0:
		srcEnd = trimStart + p.Duration*speed
	}
	if srcEnd > 0 {
		target = math.Min(target, math.Max(srcEnd-stillEndMargin, trimStart))
	}
	tOut := (target - trimStart) / speed // clamped output time

	// Seek-back: at least one source frame must lie in [start, target] and
	// the fps stage must see at least one whole output period of input.
	period := speed / fps // source seconds per output frame
	back := stillUnknownFPSSeekBack
	if p.SourceFPS > 0 && !math.IsInf(p.SourceFPS, 0) {
		back = math.Max(2/p.SourceFPS, stillMinSeekBack)
	}
	back += period
	if fromStart {
		back = math.Inf(1)
	}
	rawStart := math.Max(trimStart, target-back)

	// Snap onto the render's slot grid.
	slots := math.Floor((rawStart-trimStart)/period + stillSlotEpsilon)
	start := trimStart + slots*period
	slot := math.Floor(tOut*fps+stillSlotEpsilon) - slots // j: slot after start
	if slot < 0 {
		slot = 0
	}
	return stillSeek{
		start:     start,
		threshold: math.Max((slot-0.5)/fps, 0),
		pad:       slot/fps + stillPadSlack,
	}
}

// stripSeekArgs removes input-side seeking/duration options (and their
// values) from an InputArgs slice, keeping everything else in order.
func stripSeekArgs(in []string) []string {
	out := make([]string, 0, len(in))
	for i := 0; i < len(in); i++ {
		switch in[i] {
		case "-ss", "-to", "-t", "-sseof":
			i++ // skip the value as well
		default:
			out = append(out, in[i])
		}
	}
	return out
}

// normalizeMatte returns a lowercase RRGGBB for the GIF matte, dropping an
// alpha byte and falling back to the Discord dark default when invalid.
func normalizeMatte(s string) string {
	hex, err := recipe.NormalizeHex(s)
	if err != nil {
		return DefaultMatte
	}
	return hex[:6]
}

// clampInt returns def for v == 0, otherwise v clamped to [lo, hi].
func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	return min(max(v, lo), hi)
}

// formatFloat renders v with at most six decimals and no trailing zeros
// ("25", "33.333333", "2.5", "0.3"), the shape ffmpeg's -r/-ss/-t and
// filter options expect; ffmpeg turns such decimals into the closest
// rational (33.333333 → 33333333/1000000), which is exact enough for
// centisecond GIF delays and millisecond WebP durations.
func formatFloat(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	s := strconv.FormatFloat(v, 'f', 6, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}
