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
//     nil, as does a plan that is not ready to run (an unbound drawtext
//     placeholder, an overlay input without a path — see phase3.go).
//   - Phase 3 (phase3.go): overlay inputs, reversed plans and the autocrop /
//     font helpers.
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
//
// For an image-sequence plan (p.InputPattern != "") srcPath is the blob
// directory and the input becomes "-i <srcPath>/<InputPattern>" (the image2
// demuxer pattern; p.InputArgs carry its -framerate/-start_number). The
// same holds for StillArgs, StillArgsFromStart and ProxyArgs.
//
// Every p.ExtraInputs entry (Phase 3) follows the main input as
// "[Args...] -i Path"; a plan with an unbound drawtext placeholder or an
// extra input without Path yields nil (see phase3.go).
func MasterArgs(srcPath string, p *graph.Plan, outPath string) []string {
	if !planUsable(p) {
		return nil
	}
	args := make([]string, 0, len(p.InputArgs)+14)
	args = append(args, p.InputArgs...)
	args = append(args, "-i", inputPath(srcPath, p))
	args = append(args, extraInputArgs(p)...)
	args = append(args,
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
// [-ss S [-itsoffset S-TrimStart]] … -i src [extra inputs] -frames:v 1
// -filter_complex "<plan.Filter>;[out]tpad=…,select=…[,scale][outs]"
// -map [outs] -c:v png -compression_level 1 -f image2pipe pipe:1.
//
// t is an output-time offset: the wanted source time is TrimStart + t*Speed
// (Speed <= 0 counts as 1), clamped to >= TrimStart and to just inside the
// source range (TrimEnd, else TrimStart + Duration*Speed when the duration is
// known). The seek S lies a little BEFORE that target — at least
// 2/SourceFPS (0.5 s when unknown), never less than 0.1 s, plus one output
// frame period — and is snapped onto the plan's output frame grid
// (TrimStart + k*Speed/FPS), so the plan's fps stage produces the same frame
// slots as the real render; a `-ss` at or past the last decodable frame
// would otherwise yield no image at all near the clip end. "-itsoffset
// S-TrimStart" undoes the seek's re-basing of timestamps, so every frame
// carries the absolute output time the render gives it (the fps grid, any
// enable='gte(t+0.0001,S)*lt(t+0.0001,E)' window — the half-open [S, E) the
// graph emits, with its 1e-4 s tolerance — and the overlay inputs, which
// start at 0 like in the render, see phase3.go, all line up). The plan filter runs
// unchanged (its fps stage also gives tpad a frame rate) and is followed by
// tpad=stop_mode=clone (the last frame is held for t up to Duration and
// through the fps filter's rounding tail) and a select for the slot
// displayed at t, by its absolute time. When maxW > 0 the selected frame is
// scaled to at most maxW wide, wrapped in premultiply/unpremultiply when the
// plan has alpha (the same chain graph uses, so the preview shows no dark
// fringes the output does not have). -ss is omitted when S == 0, and
// -itsoffset when S == TrimStart.
//
// A plan whose main source cannot be seeked (p.SeekUnsafe: animated WebP —
// FFmpeg 9's webp_anim demuxer decodes nothing after any input seek) is not
// seeked at all, by either variant: no -ss, -itsoffset or -to, trimmed or
// not. A trim on such a source is a filter stage (p.FilterTrim; the graph
// compiler emits "trim=…,setpts=PTS-STARTPTS" instead of -ss/-to) that cuts
// the source and rebases the timestamps exactly like an input seek, so a
// decode from the file's start carries the render's own clock and the
// select by absolute time (or by index, reversed) holds unchanged; without
// a trim the file's clock is the render's already. StillArgs therefore
// decodes such a plan in one run (no empty result to retry from the start).
//
// A reversed plan (p.Reversed) is seeked before source time TrimStart +
// (Duration - t)*Speed the same way, keeps "-to TrimEnd", and selects the
// wanted frame by index (select='gte(n,j)', j = floor(t*FPS)): the plan's
// reverse stage hands out the reversed frames with the forward timestamps,
// so the j-th frame after the seek is the render's j-th frame — on CFR
// sources. A reversed plan on an animation source (p.SourceVFR: gif, apng,
// webp, avif) is decoded from TrimStart instead, see reversedSeekFor.
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
	if !planUsable(p) {
		return nil
	}
	return stillArgs(srcPath, p, stillSeekFor(p, t, false), maxW)
}

// StillArgsFromStart is StillArgs without the seek-back: it decodes from
// TrimStart (no -ss when that is 0) and lets the plan's fps stage and tpad
// carry every source frame — including a held last frame — up to t. It is
// exact (the frame slots are the render's own) but costs a decode of
// TrimStart..t (the whole trimmed clip for a reversed plan), so it is meant
// as the fallback when StillArgs produced no image.
func StillArgsFromStart(srcPath string, p *graph.Plan, t float64, maxW int) []string {
	if !planUsable(p) {
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
	if s.reversed {
		f.WriteString(",select='gte(n," + strconv.Itoa(s.index) + ")'")
	} else {
		f.WriteString(",select='gte(t," + formatFloat(s.threshold) + ")'")
	}
	if maxW > 0 {
		f.WriteString("," + previewScale(p, maxW))
	}
	f.WriteString("[outs]")

	input := stripSeekArgs(p.InputArgs)
	args := make([]string, 0, len(input)+22)
	if s.start > 0 {
		args = append(args, "-ss", formatFloat(s.start))
	}
	if s.offset > 0 {
		args = append(args, "-itsoffset", formatFloat(s.offset))
	}
	if s.end > s.start {
		args = append(args, "-to", formatFloat(s.end))
	}
	args = append(args, input...)
	args = append(args, "-i", inputPath(srcPath, p))
	args = append(args, extraInputArgs(p)...)
	args = append(args,
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
// The plan's input args (including any trim seek) are kept, and every
// extra input follows the main one unchanged; -t is applied as an output
// option so it composes with an input-side -to and counts output seconds
// (i.e. after any speed change). fps=15 is inserted unless the plan already
// runs at <= 15 fps or has exactly one frame (p.Frames == 1: a still main
// source, with or without an animated overlay — ffmpeg's fps filter needs a
// second timestamp and emits nothing at all for a single frame, after which
// libwebp_anim fails with "WebPAnimEncoderAssemble() failed"; Frames 0 is an
// unknown count and keeps the cap). The scale to maxW is wrapped in
// premultiply/unpremultiply when the plan has alpha (as graph does for the
// real render) so the preview shows no dark edge fringes the output lacks.
//
// A reversed plan (p.Reversed) shows the LAST maxSeconds of the trimmed
// source first, yet its reverse stage buffers every frame it is handed, so
// the main input is seeked to just before that tail (proxySeekFor): "-ss S
// [-to TrimEnd]" replaces the plan's own seek (every other input option is
// kept, as for the stills), S being the reversed stills' seek for output
// time maxSeconds — the usual seek-back before the source slot shown at that
// time, snapped onto the plan's slot grid and, for CFR video and image
// sequences, onto a whole number of source frames after TrimStart so the -to
// cut lands on the render's frame (see reversedSeekFor). The reverse stage
// stamps the forward timestamps, counted from the seek point, onto the
// reversed frames, so the proxy still starts at output t=0 with the render's
// own first frame, the overlay clocks and enable windows keep their absolute
// output times, and the -t cap stays. Seekable animation sources
// (p.SourceVFR: gif, apng, avif, trimmed or not) get a plain seek at the
// computed time — such a preview may be a frame off where the seek lands
// inside a hold. A plan whose demuxer decodes nothing after a seek
// (p.SeekUnsafe: animated WebP, which FilterTrim implies) is never seeked;
// a plan whose length is unknown, or whose tail reaches back to TrimStart
// anyway, decodes from TrimStart as a forward plan does.
func ProxyArgs(srcPath string, p *graph.Plan, maxW int, maxSeconds float64, outPath string) []string {
	if !planUsable(p) {
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
	if p.Frames != 1 && !(p.FPS > 0 && p.FPS <= proxyMaxFPS) {
		f.WriteString("fps=" + strconv.Itoa(proxyMaxFPS) + ",")
	}
	f.WriteString(previewScale(p, maxW) + "[outp]")

	pix := "yuv420p"
	if p.HasAlpha {
		pix = "yuva420p"
	}
	input := p.InputArgs
	var seek []string
	if s, ok := proxySeekFor(p, maxSeconds); ok {
		input = stripSeekArgs(p.InputArgs)
		seek = append(seek, "-ss", formatFloat(s.start))
		if s.end > s.start {
			seek = append(seek, "-to", formatFloat(s.end))
		}
	}
	args := make([]string, 0, len(seek)+len(input)+30)
	args = append(args, seek...)
	args = append(args, input...)
	args = append(args, "-i", inputPath(srcPath, p))
	args = append(args, extraInputArgs(p)...)
	args = append(args,
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
	// Variant (Phase 2) pre-filters the master (fps drop / downscale) before
	// the palette chain: see VariantFilter. nil = encode the master as-is.
	Variant *Variant
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
//
// With o.Variant the graph starts with "[0:v]<VariantFilter>[v];" and the
// palette chain reads [v]; the matte colour source takes the variant's size
// and rate. A nil or no-op variant leaves the graph exactly as before.
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
	prefix, in := variantPrefix(m, o.Variant)
	vm := VariantMaster(m, o.Variant)
	var b strings.Builder
	b.WriteString(prefix)
	if o.HasAlpha {
		b.WriteString(in + "split[c][a];")
		b.WriteString("[a]alphaextract,lut=c0='gte(val," + strconv.Itoa(o.AlphaThreshold) + ")*255'[m];")
		b.WriteString("color=c=0x" + o.Matte + ":s=" + strconv.Itoa(vm.Width) + "x" + strconv.Itoa(vm.Height) + ":r=" + formatFloat(masterFPS(vm)) + ",format=rgba[bg];")
		b.WriteString("[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];")
		b.WriteString("[f][m]alphamerge,split[p1][p2];")
		b.WriteString("[p1]palettegen=max_colors=" + strconv.Itoa(o.Colors) + ":reserve_transparent=1:stats_mode=" + o.StatsMode + "[pal];")
		b.WriteString("[p2][pal]paletteuse=dither=" + o.ditherArg() + ":diff_mode=rectangle:alpha_threshold=128[out]")
		return b.String()
	}
	b.WriteString(in + "split[p1][p2];")
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
	// Variant (Phase 2) pre-filters the master (fps drop / downscale): see
	// VariantFilter. nil = encode the master as-is.
	Variant *Variant
}

// WebPArgs encodes the master with the WebPAnimEncoder path only:
// [RawInputArgs] -c:v libwebp_anim -lossless 0|1 -q:v Q -compression_level L
// -pix_fmt yuva420p|bgra -loop P -map_metadata -1 -f webp outPath.
// (yuva420p for lossy, bgra for lossless; P = 0 for Loop 0, else Loop+1.)
//
// -q:v is omitted for lossless output (libwebp then uses its default
// effort). Lossy output uses yuv420p when the master has no alpha so the
// VP8X ALPHA flag is only set when frames really carry alpha (§5.3).
//
// With o.Variant, "-filter_complex [0:v]<VariantFilter>[v] -map [v]" follows
// the input (a nil or no-op variant adds nothing); the still decision then
// uses the variant's frame count (VariantMaster).
func WebPArgs(m Master, o WebPOptions, outPath string) []string {
	args := RawInputArgs(m)
	args = append(args, variantArgs(m, o.Variant)...)
	// A single-frame master becomes a plain still WebP: WebPAnimEncoder would
	// wrap one frame in VP8X+ANIM+ANMF, which Discord's lint (webp.anim-flag)
	// rightly rejects. The legacy libwebp encoder is fine for stills — the
	// ghost-trail bug only concerns animations.
	still := VariantMaster(m, o.Variant).Frames == 1
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

// inputPath returns the "-i" value for a plan: srcPath itself, or for an
// image-sequence plan "<srcPath>/<InputPattern>" (srcPath is then the blob
// directory; joined with "/" so the argv is identical on every host).
func inputPath(srcPath string, p *graph.Plan) string {
	if p.InputPattern == "" {
		return srcPath
	}
	return joinSlash(srcPath, p.InputPattern)
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
	start  float64 // -ss value in source seconds (0 = no seek)
	offset float64 // -itsoffset in source seconds: start - TrimStart, so timestamps stay absolute (0 = none; always 0 for reversed plans)
	end    float64 // -to value in source seconds (0 = none; reversed plans keep TrimEnd)
	pad    float64 // tpad stop_duration in output seconds
	// Forward plans select by absolute output time, reversed plans by
	// frame index after the seek (see StillArgs).
	reversed  bool
	threshold float64 // select 'gte(t,threshold)' in output seconds
	index     int     // select 'gte(n,index)'
}

// Reversed-still seek constants.
const (
	// maxStillIndex bounds the frame index a reversed still may select when
	// the plan has no duration or frame count to cap it (a t far past any
	// real clip); tpad would clone that many frames at most.
	maxStillIndex = 1 << 20
	// seekPhaseTolerance (seconds) is how far K output slots may be from a
	// whole number of source frames for a reversed still to seek there: a
	// tenth of the microsecond ffmpeg parses -ss/-to at, so the trim cut can
	// never land on the other side of a source frame than the render's.
	seekPhaseTolerance = 1e-7
	// maxPhaseSearch caps the search for the smallest aligned slot count (a
	// longer alignment period than 1000 slots is not worth seeking for).
	maxPhaseSearch = 1000
)

// stillGrid holds the plan facts the seek maths needs, with the defaults
// applied: a plan without FPS is treated as fallbackFPS, Speed <= 0 as 1.
type stillGrid struct {
	speed     float64 // speed factor
	fps       float64 // output frame rate
	trimStart float64 // first source second the render decodes
	srcEnd    float64 // source second the render stops at (0 = unknown)
	period    float64 // source seconds per output slot (speed/fps)
	back      float64 // seek-back in source seconds (see seekBefore)
	last      float64 // the render's last output slot (-1 = unknown)
}

// newStillGrid derives the grid from p. srcEnd is TrimEnd, else TrimStart +
// Duration*Speed when the duration is known. The last slot is Plan.Frames-1
// when the plan knows its frame count (exact for image sequences, where the
// speed stage truncates the end timestamp and Frames can be below
// floor(Duration*FPS)), else floor(Duration*FPS)-1 when the duration is
// known (the fps stage runs round=down, so no slot past that is rendered).
func newStillGrid(p *graph.Plan) stillGrid {
	g := stillGrid{speed: p.Speed, fps: p.FPS, trimStart: math.Max(p.TrimStart, 0), last: -1}
	if !(g.speed > 0) || math.IsInf(g.speed, 0) {
		g.speed = 1
	}
	if !(g.fps > 0) || math.IsInf(g.fps, 0) {
		g.fps = fallbackFPS
	}
	g.period = g.speed / g.fps
	switch {
	case p.TrimEnd > 0:
		g.srcEnd = p.TrimEnd
	case p.Duration > 0:
		g.srcEnd = g.trimStart + p.Duration*g.speed
	}
	// Seek-back: at least two source frames must lie in [start, target] and
	// the fps stage must see at least one whole output period of input.
	g.back = stillUnknownFPSSeekBack
	if p.SourceFPS > 0 && !math.IsInf(p.SourceFPS, 0) {
		g.back = math.Max(2/p.SourceFPS, stillMinSeekBack)
	}
	g.back += g.period
	durOut := p.Duration
	if !(durOut > 0) && p.TrimEnd > 0 {
		durOut = (p.TrimEnd - g.trimStart) / g.speed
	}
	switch {
	case p.Frames > 0:
		g.last = float64(p.Frames) - 1
	case durOut > 0 && !math.IsInf(durOut, 0):
		g.last = math.Floor(durOut*g.fps+stillSlotEpsilon) - 1
	}
	return g
}

// clampTarget keeps a wanted source time inside [TrimStart, srcEnd -
// stillEndMargin] (NaN/Inf become TrimStart).
func (g stillGrid) clampTarget(target float64) float64 {
	if math.IsNaN(target) || math.IsInf(target, 0) || target < g.trimStart {
		return g.trimStart
	}
	if g.srcEnd > 0 {
		target = math.Min(target, math.Max(g.srcEnd-stillEndMargin, g.trimStart))
	}
	return target
}

// capSlot caps an output slot at the render's last one: a t at the very end
// of the clip would otherwise select the slot past it and show a source
// frame the render's EOF flush drops.
func (g stillGrid) capSlot(slot float64) float64 {
	if g.last >= 0 && slot > g.last {
		return g.last
	}
	return slot
}

// seekBefore returns the seek start for a wanted source time and the number
// of output slots it lies after TrimStart: the start is the seek-back before
// the target (never before TrimStart), snapped DOWN onto the slot grid
// TrimStart + K*Speed/FPS so the fps stage after the seek fills the same
// slots as the render. fromStart forces TrimStart (K = 0).
func (g stillGrid) seekBefore(target float64, fromStart bool) (start, slots float64) {
	if fromStart {
		return g.trimStart, 0
	}
	rawStart := math.Max(g.trimStart, target-g.back)
	slots = math.Floor((rawStart-g.trimStart)/g.period + stillSlotEpsilon)
	return g.trimStart + slots*g.period, slots
}

// stillSeekFor maps an output-time offset t to the seek StillArgs uses
// (forward plans; reversed plans go through reversedSeekFor).
//
// The target source time is TrimStart + t*Speed, clamped into the source.
// The output slot displayed at t is k = floor(tOut*FPS) (tOut = clamped t),
// capped at the render's last slot; the seek start is snapped onto the slot
// grid K slots after TrimStart (seekBefore) and -itsoffset K*Speed/FPS keeps
// the timestamps absolute, so the fps stage after the seek emits slot k
// exactly where and WHEN the render emits it. The select threshold sits half
// a slot before slot k (robust to float ties) and the tpad clones the last
// frame for (k-K)/FPS + stillPadSlack seconds so an early end of input still
// yields the (held) last frame.
//
// A plan whose demuxer cannot seek (p.SeekUnsafe; FilterTrim implies it) is
// not seeked at all: the decode starts at the file's beginning and every
// frame already carries the render's absolute output time — a trim stage
// (p.FilterTrim) cuts the source at TrimStart..TrimEnd and rebases the
// timestamps exactly as an input seek would, and an untrimmed plan's clock
// is the render's as is — so the from-start slot maths apply, without the
// -ss (see unseeked).
func stillSeekFor(p *graph.Plan, t float64, fromStart bool) stillSeek {
	if p.Reversed {
		return reversedSeekFor(p, t, fromStart)
	}
	if !(t > 0) { // also catches NaN
		t = 0
	}
	g := newStillGrid(p)
	target := g.clampTarget(g.trimStart + t*g.speed)
	tOut := (target - g.trimStart) / g.speed // clamped output time
	abs := g.capSlot(math.Floor(tOut*g.fps + stillSlotEpsilon))
	start, slots := g.seekBefore(target, fromStart || seekUnsafe(p))
	slot := math.Max(abs-slots, 0) // slots between the seek and the wanted one
	s := stillSeek{
		start:     start,
		offset:    slots * g.period,
		threshold: math.Max((abs-0.5)/g.fps, 0),
		pad:       slot/g.fps + stillPadSlack,
	}
	if seekUnsafe(p) {
		return s.unseeked()
	}
	return s
}

// seekUnsafe reports whether p's main input must never be seeked: its
// demuxer decodes nothing after an input seek (graph.Plan.SeekUnsafe), or
// its trim is a filter stage (graph.Plan.FilterTrim, which the compiler only
// emits for such demuxers — checked separately so a hand-built plan that
// sets one flag alone is still handled).
func seekUnsafe(p *graph.Plan) bool {
	return p.SeekUnsafe || p.FilterTrim
}

// unseeked returns s without any input seek (no -ss, -itsoffset or -to): the
// form for plans whose demuxer cannot seek (seekUnsafe): the trim stage, if
// any, does the cutting.
func (s stillSeek) unseeked() stillSeek {
	s.start, s.offset, s.end = 0, 0, 0
	return s
}

// reversedSeekFor is stillSeekFor for a reversed plan. Output slot j =
// floor(t*FPS) (capped at the render's last slot) shows source slot k =
// last - j, so the seek lies the usual seek-back before the middle of slot
// k, snapped onto the grid, and the decode keeps the trim end. The plan's
// reverse stage then yields the frames from the seek to the end in reverse
// order with the forward timestamps: the wanted frame is the j-th one
// whatever K is (the render's j-th frame too), so it is selected by index
// and the timestamps are not offset.
//
// The end matters here (reverse counts from it), and ffmpeg cuts a seeked
// input at "-to" by a trim DURATION counted from the first frame decoded
// after the seek (fftools inserts trim=durationi=to-ss; verified), i.e. at
// TrimEnd + δ where δ is the distance from the seek point to the next
// source frame. The render's own cut is TrimEnd + δ(TrimStart), so the seek
// is additionally snapped to a whole number of source frames after
// TrimStart (alignedSlots) — 5 slots for a 30 fps source at 25 fps, every
// slot at equal rates — which keeps δ, and with it the decoded frame set,
// identical. Without such an alignment within seekPhaseTolerance (fractional
// rates, 29.97 fps sources, an unknown source rate) or without a known last
// slot (no duration, no frame count) the decode starts at TrimStart exactly
// like the render's, which is exact at the cost of the whole trimmed clip.
//
// Animation sources (p.SourceVFR: gif, apng, webp, avif) are never seeked
// either, whatever the rates: their frames carry individual delays, so
// Plan.SourceFPS is only ffmpeg's base-cadence estimate and the alignment
// above is meaningless, and their demuxers cannot seek into a hold —
// ffmpeg's accurate seek drops the held frame covering the seek point (its
// pts is before S), so the decode starts a whole hold after the seek and the
// frames after it no longer end at the render's last slot, while with a trim
// end the -to duration (counted from the first decoded frame) cuts one
// source frame later than the render's. Either way the j-th reversed frame
// is not the render's (verified on a 10 fps GIF with one 600 ms hold: 2 to
// 15 wrong stills per clip, none from TrimStart). The aligned seek stays for
// CFR video and image sequences.
//
// A plan whose demuxer cannot seek (p.SeekUnsafe, FilterTrim included) has
// nothing to seek or cut: the whole file is decoded, a trim stage yields
// TrimStart..TrimEnd from it, and the reverse stage counts from that cut
// (or from the file's end), so the index selection holds with no -ss/-to at
// all.
func reversedSeekFor(p *graph.Plan, t float64, fromStart bool) stillSeek {
	if !(t > 0) { // also catches NaN
		t = 0
	}
	g := newStillGrid(p)
	j := g.capSlot(math.Min(math.Floor(t*g.fps+stillSlotEpsilon), maxStillIndex))
	s := stillSeek{
		reversed: true,
		index:    int(j),
		pad:      j/g.fps + stillPadSlack,
		start:    g.trimStart,
	}
	if p.TrimEnd > 0 {
		s.end = p.TrimEnd
	}
	if seekUnsafe(p) {
		return s.unseeked()
	}
	if fromStart || p.SourceVFR {
		return s
	}
	s.start = g.reversedSeekStart(p.SourceFPS, j, true)
	return s
}

// reversedSeekStart returns the seek start (source seconds) a reversed plan
// is decoded from for reversed output slot j: the usual seek-back before the
// middle of source slot last - j, snapped down onto the slot grid
// (seekBefore) and, with align, onto a whole number of source frames after
// TrimStart (alignedSlots — see reversedSeekFor for why; without it the
// seek is the plain grid-snapped time). TrimStart when the last slot is
// unknown.
func (g stillGrid) reversedSeekStart(srcFPS, j float64, align bool) float64 {
	if g.last < 0 {
		return g.trimStart
	}
	k := g.last - j
	_, slots := g.seekBefore(g.clampTarget(g.trimStart+(k+0.5)*g.period), false)
	if align {
		slots = g.alignedSlots(srcFPS, slots)
	}
	return g.trimStart + slots*g.period
}

// proxySeekFor returns the input seek ProxyArgs applies to a reversed plan
// (see there) and whether there is one: the reversed stills' seek for output
// time maxSeconds (reversedSeekStart; aligned for CFR sources, plain for
// seekable animation sources — gif/apng/avif, trimmed or not), when it lies
// after TrimStart, with the trim end kept for -to. Forward plans, plans
// whose demuxer cannot seek (seekUnsafe: animated WebP, trimmed or not) and
// plans of unknown length are never seeked.
func proxySeekFor(p *graph.Plan, maxSeconds float64) (stillSeek, bool) {
	if !p.Reversed || seekUnsafe(p) {
		return stillSeek{}, false
	}
	g := newStillGrid(p)
	j := g.capSlot(math.Min(math.Floor(maxSeconds*g.fps+stillSlotEpsilon), maxStillIndex))
	start := g.reversedSeekStart(p.SourceFPS, j, !p.SourceVFR)
	if !(start > g.trimStart) {
		return stillSeek{}, false
	}
	s := stillSeek{reversed: true, start: start}
	if p.TrimEnd > 0 {
		s.end = p.TrimEnd
	}
	return s, true
}

// alignedSlots returns the largest K <= kmax for which K output slots span
// a whole number of source frames (within seekPhaseTolerance), or 0 when
// there is none up to maxPhaseSearch or the source rate is unknown.
func (g stillGrid) alignedSlots(srcFPS, kmax float64) float64 {
	if !(srcFPS > 0) || math.IsInf(srcFPS, 0) || kmax < 1 {
		return 0
	}
	step := 0.0
	for k := 1.0; k <= math.Min(kmax, maxPhaseSearch); k++ {
		if g.phaseError(srcFPS, k) < seekPhaseTolerance {
			step = k
			break
		}
	}
	if step == 0 {
		return 0
	}
	k := math.Floor(kmax/step) * step
	if g.phaseError(srcFPS, k) >= seekPhaseTolerance { // float drift over many steps
		return 0
	}
	return k
}

// phaseError is the distance in seconds from K output slots to the nearest
// whole number of source frames.
func (g stillGrid) phaseError(srcFPS, k float64) float64 {
	frames := k * g.period * srcFPS
	return math.Abs(frames-math.Round(frames)) / srcFPS
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
