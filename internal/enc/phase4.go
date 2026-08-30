package enc

// Phase 4 builders (DESIGN.md §10 item 4 / phase4 design §2, §4, §5): the
// opaque MP4 (libx264) and WebM (libvpx-vp9) tails, the gifski HQ GIF
// encoder, and the lossless gifsicle fast path for GIF → GIF edits that need
// no re-encode. All pure argv, golden-tested; the tool-facing shapes are
// re-verified against the runtime image by phase4_tools_test.go whenever the
// tools are on PATH.
//
// MP4/WebM are OPAQUE exports: mp4 (yuv420p) cannot carry alpha and Discord
// ignores WebM alpha, so both tails flatten the RGBA master onto
// Output.Matte exactly like the JPEG tail does (matte underlay in the
// filter), then pad to even dimensions — libx264/libvpx refuse odd sizes at
// yuv420p — with the same matte as the pad colour.

import (
	"math"
	"strconv"
	"strings"
)

// Phase 4 defaults and bounds.
const (
	// DefaultX264CRF is the libx264 CRF used when MP4Options.CRF is unset
	// (the zero value). recipe.Output.Quality IS the CRF for the video
	// formats (jobs passes it verbatim into CRF) — there is no 1..100
	// mapping.
	DefaultX264CRF = 20
	// DefaultVP9CRF is the libvpx-vp9 CRF for the WebMOptions zero value
	// (also the docs/web contract's "quality 0 = CRF 30" for webm and the
	// fit knob's starting mild probe — keep the two in step with this).
	DefaultVP9CRF = 30
	// DefaultGifskiQuality is gifski's --quality when 0 is passed (gifski's
	// own default).
	DefaultGifskiQuality = 90
	// DefaultGifskiFPS is the --fps fallback for an unknown master rate
	// (gifski's own default).
	DefaultGifskiFPS = 20
	// MaxGifskiFPS is the largest --fps gifski accepts.
	MaxGifskiFPS = 100
	// maxX264CRF / maxVP9CRF bound the encoders' CRF scales.
	maxX264CRF = 51
	maxVP9CRF  = 63
)

// evenPad pads a frame up to even dimensions; the pad colour is filled with
// the matte so an added row/column is invisible on the flattened output.
const evenPad = "pad=ceil(iw/2)*2:ceil(ih/2)*2:color=0x"

// MP4Options controls the MP4 tail. Zero values: CRF 0 = DefaultX264CRF
// (20), Matte "313338", Variant nil.
type MP4Options struct {
	// CRF is the libx264 CRF 1..51 (capped): recipe.Output.Quality verbatim
	// per the docs/web contract — there is no 1..100 quality mapping — and
	// the fit engine's knob (the searched range is 12..40, see fit.KnobFor).
	// 0 = DefaultX264CRF.
	CRF     int
	Matte   string // RRGGBB flattened background and pad colour (0 = "313338")
	Variant *Variant
}

// crf resolves the effective libx264 CRF.
func (o MP4Options) crf() int {
	if o.CRF > 0 {
		return min(o.CRF, maxX264CRF)
	}
	return DefaultX264CRF
}

// WebMOptions controls the WebM tail. Zero values: CRF 0 = DefaultVP9CRF
// (30), Matte "313338", Variant nil.
type WebMOptions struct {
	// CRF is the libvpx-vp9 CRF 1..63 (capped): recipe.Output.Quality
	// verbatim per the docs/web contract — no 1..100 quality mapping — and
	// the fit engine's knob (the searched range is 15..55, see fit.KnobFor).
	// 0 = DefaultVP9CRF.
	CRF     int
	Matte   string // RRGGBB flattened background and pad colour (0 = "313338")
	Variant *Variant
}

// crf resolves the effective vp9 CRF.
func (o WebMOptions) crf() int {
	if o.CRF > 0 {
		return min(o.CRF, maxVP9CRF)
	}
	return DefaultVP9CRF
}

// videoColorConvert is the RGB → YUV tail of the video flatten graph: the
// rgba→yuv420p conversion is done INSIDE the graph with explicit bt709
// coefficients and tv range instead of by the encoder's auto-inserted
// swscale, whose bt601 default silently disagrees with the bt709 matrix
// players assume for untagged video — the flattened matte would decode a few
// units off Discord's dark background. The setparams stage stamps the frame
// properties (modern ffmpeg encoders take primaries/trc from the frames —
// scale only sets the matrix and range) and MP4Args/WebMArgs additionally tag
// the stream (-colorspace/-color_primaries/-color_trc bt709), so a tag-aware
// decode round-trips the matte exactly.
const videoColorConvert = ",scale=out_color_matrix=bt709:out_range=tv,format=yuv420p," +
	"setparams=colorspace=bt709:color_primaries=bt709:color_trc=bt709"

// videoFlattenFilter is the JPEG tail's flatten graph with the even-dim pad
// and the tagged bt709 yuv420p conversion instead of the yuvj420p one:
//
//	[0:v]<variant>[c];color=c=0xRRGGBB:s=WxH:r=FPS,format=rgba[bg];
//	[bg][c]overlay=format=auto:shortest=1,pad=ceil(iw/2)*2:ceil(ih/2)*2:color=0xRRGGBB,
//	scale=out_color_matrix=bt709:out_range=tv,format=yuv420p,
//	setparams=colorspace=bt709:color_primaries=bt709:color_trc=bt709[f]
//
// (without a variant the overlay reads [0:v] directly). WxH/FPS are the
// variant's, so the pad applies after any fit downscale; the final scale +
// format stage converts to yuv420p with bt709/tv (videoColorConvert), and
// the encoder's -pix_fmt yuv420p is then a no-op guard.
func videoFlattenFilter(m Master, v *Variant, matte string) string {
	prefix, in := "", "[0:v]"
	if f := VariantFilter(m, v); f != "" {
		prefix, in = "[0:v]"+f+"[c];", "[c]"
	}
	vm := VariantMaster(m, v)
	hex := normalizeMatte(matte)
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("color=c=0x" + hex + ":s=" + strconv.Itoa(vm.Width) + "x" + strconv.Itoa(vm.Height) + ":r=" + formatFloat(masterFPS(vm)) + ",format=rgba[bg];")
	b.WriteString("[bg]" + in + "overlay=format=auto:shortest=1," + evenPad + hex + videoColorConvert + "[f]")
	return b.String()
}

// MP4Args encodes the master as an opaque H.264 MP4 (Discord-playable
// attachment; DESIGN.md phase4 §2):
//
//	[RawInputArgs] -filter_complex <flatten+pad+bt709> -map [f] -c:v libx264
//	-crf K -preset slow -pix_fmt yuv420p -colorspace bt709 -color_primaries
//	bt709 -color_trc bt709 -movflags +faststart -an -f mp4 outPath
//
// The master is flattened onto o.Matte (videoFlattenFilter), padded to even
// dimensions and converted to bt709/tv yuv420p before the encoder; the
// -colorspace/-color_primaries/-color_trc trio tags the stream to match the
// conversion (videoColorConvert) so players decode with the same matrix. The
// frame rate rides in with RawInputArgs' -r (the variant's fps stage handles
// fit drops). K is o.CRF when > 0 (capped at 51), else DefaultX264CRF.
// +faststart moves the moov atom before mdat so Discord/browsers start
// playback while downloading. Loop counts do not apply to video.
func MP4Args(m Master, o MP4Options, outPath string) []string {
	args := RawInputArgs(m)
	return append(args,
		"-filter_complex", videoFlattenFilter(m, o.Variant, o.Matte),
		"-map", "[f]",
		"-c:v", "libx264",
		"-crf", strconv.Itoa(o.crf()),
		"-preset", "slow",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-movflags", "+faststart",
		"-an",
		"-f", "mp4",
		outPath,
	)
}

// WebMArgs encodes the master as an opaque VP9 WebM:
//
//	[RawInputArgs] -filter_complex <flatten+pad+bt709> -map [f] -c:v libvpx-vp9
//	-crf K -b:v 0 -row-mt 1 -pix_fmt yuv420p -colorspace bt709
//	-color_primaries bt709 -color_trc bt709 -an -f webm outPath
//
// Flatten/pad/fps and the bt709 conversion + tags as for MP4Args; -b:v 0
// selects constant-quality mode (CRF alone controls the size), -row-mt 1
// enables row-based multithreading. K is o.CRF when > 0 (capped at 63), else
// DefaultVP9CRF.
func WebMArgs(m Master, o WebMOptions, outPath string) []string {
	args := RawInputArgs(m)
	return append(args,
		"-filter_complex", videoFlattenFilter(m, o.Variant, o.Matte),
		"-map", "[f]",
		"-c:v", "libvpx-vp9",
		"-crf", strconv.Itoa(o.crf()),
		"-b:v", "0",
		"-row-mt", "1",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-an",
		"-f", "webm",
		outPath,
	)
}

// --- gifski ------------------------------------------------------------------

// GifskiArgs builds the gifski argv for the HQ GIF encoder (phase4 design
// §4): ["--fps", F, "--quality", Q, ("--width", W)?, "-o", outPath,
// frames...]. gifski consumes PNG frames, so jobs materialises the master as
// per-frame PNGs first (PNGFramesArgs with -compression_level 1; any fit
// variant is baked into those frames, so no --width is needed for fit rungs
// — width > 0 is an extra client-side downscale and is passed as --width).
//
// frames is the ordered list of PNG file paths: gifski does NOT expand glob
// patterns itself on Linux (the shell usually does, and nothing here runs
// through a shell), so the files are passed one by one; zero-padded names
// keep gifski's own sorting identical to the given order. An empty list
// yields nil.
//
// quality is gifski's --quality 1..100 (0 = 90, gifski's default) — also the
// fit knob for the gifski path (searched as 100-quality, fit.KnobQuality).
// fps <= 0 or non-finite falls back to 20 (gifski's default) and is capped
// at 100 (gifski errors above); fractional rates are fine (--fps parses as a
// float, verified with gifski 1.34).
//
// gifski always writes an infinite NETSCAPE loop; a non-default
// recipe.Output.Loop is applied by the usual GifsicleArgs post-pass (which
// jobs runs on every GIF anyway).
func GifskiArgs(frames []string, fps float64, quality, width int, outPath string) []string {
	if len(frames) == 0 {
		return nil
	}
	if !(fps > 0) || math.IsInf(fps, 0) {
		fps = DefaultGifskiFPS
	}
	fps = math.Min(fps, MaxGifskiFPS)
	args := make([]string, 0, 9+len(frames))
	args = append(args,
		"--fps", formatFloat(fps),
		"--quality", strconv.Itoa(clampInt(quality, 1, 100, DefaultGifskiQuality)),
	)
	if width > 0 {
		args = append(args, "--width", strconv.Itoa(width))
	}
	args = append(args, "-o", outPath)
	return append(args, frames...)
}

// --- lossless gifsicle fast path ------------------------------------------------

// GifsicleCrop is a crop rectangle on the source GIF's logical screen
// (pixels; X/Y is the top-left corner).
type GifsicleCrop struct {
	X, Y, W, H int
}

// GifsicleFastPathOptions describes the edits the lossless GIF → GIF fast
// path supports (phase4 design §5). The zero value is a plain -O2 --careful
// re-optimisation with an infinite loop.
type GifsicleFastPathOptions struct {
	// Crop crops every frame to the rectangle (gifsicle --crop X,Y+WxH).
	// nil = no crop; a rectangle with W or H < 1 (or negative X/Y) is
	// ignored — the builders never fail.
	Crop *GifsicleCrop
	// FrameStart/FrameEnd select the inclusive frame range [a, b] on the
	// SOURCE frame grid (0-based). FrameStart < 0 counts as 0; FrameEnd <= 0
	// means "through the last frame" (so the zero value keeps every frame).
	FrameStart int
	FrameEnd   int
	// DropEveryN drops every n-th frame of the selected range (2 = every
	// 2nd, like GifsicleOptimizeOptions) with the dropped delays merged into
	// the preceding kept frame, so the total duration is preserved. < 2
	// keeps every frame; ignored when the delays are unknown (empty).
	DropEveryN int
	// Loop is the NETSCAPE count with recipe.Output.Loop semantics: 0 =
	// forever, N > 0 = count N (plays N+1 times); negative counts as 0.
	Loop int
}

// GifsicleFastPathArgs is the argv half of the lossless GIF → GIF fast path:
// the source GIF is trimmed, cropped and frame-dropped by gifsicle alone —
// no decode to the RGBA master, no palette pass, pixels untouched. delays is
// the source's per-frame delay list in centiseconds (from discordlint's GIF
// parser or image/gif), needed to merge delays when dropping; it may be nil
// when DropEveryN is off. Shape (gifsicle applies options positionally):
//
//	[-U] [--crop X,Y+WxH] in [frame selections] -O2 --careful --loopcount=forever|N -o out
//
// Frame selections: with DropEveryN the kept frames of [FrameStart,
// FrameEnd] are written as "--delay D #i" pairs (source indices; D is the
// frame's own delay plus those of the dropped frames that follow it, capped
// at 65535 cs — MergeDroppedDelays over the range's delays). Without a drop
// a plain "#a-b" range is emitted ("#a-" when FrameEnd is open), or nothing
// at all when the range keeps every frame — gifsicle then preserves the
// delays itself. Any frame selection implies -U (gifsicle deletes frames
// from an optimised GIF without coalescing, so kept frames would lose the
// pixels the deleted ones carried; -O2 re-optimises afterwards); a crop
// alone does not (gifsicle crops optimised frames correctly in place).
//
// SUPPORTED edits: crop (any rect), a trim whose bounds land on the source
// frame grid, an fps drop reachable by dropping every 2nd..4th frame — the
// kept rate is (n-1)/n of the source's, i.e. 1/2, 2/3 or 3/4, NOT source/n —
// and the loop count; nothing else. Resize, keying, feather, overlays, text, reverse, bounce, canvas,
// flip/rotate, speed and unpremultiply all need the decode path. The
// ELIGIBILITY decision (which recipes may take this path) lives in
// internal/jobs; this builder only renders the argv and silently clamps
// out-of-range values where it can: a non-nil delays list doubles as the
// source frame count, and FrameStart/FrameEnd are clamped to its last index
// (gifsicle exits non-zero on a selection past the last frame). With nil
// delays there is nothing to clamp against, so FrameStart/FrameEnd must
// already be valid source indices — jobs always passes the parsed delay
// list. The output still goes through LintGIF as usual.
func GifsicleFastPathArgs(in, out string, delays []int, o GifsicleFastPathOptions) []string {
	a := max(o.FrameStart, 0)
	openEnd := o.FrameEnd <= 0
	b := o.FrameEnd
	if !openEnd && b < a {
		b = a
	}
	if last := len(delays) - 1; last >= 0 {
		// The delay list, when given, is also the frame count: honour the
		// documented silent clamping (a "#a-b" past the last frame would be
		// a hard gifsicle error, not a clamp).
		a = min(a, last)
		if !openEnd {
			b = min(b, last)
		}
	}

	// The kept-frame selections, if any.
	var sel []string
	if o.DropEveryN >= 2 && len(delays) > 0 {
		lo := min(a, len(delays)-1)
		hi := len(delays) - 1
		if !openEnd {
			hi = min(b, hi)
		}
		for _, f := range MergeDroppedDelays(delays[lo:hi+1], o.DropEveryN) {
			sel = append(sel, "--delay", strconv.Itoa(f.Delay), "#"+strconv.Itoa(lo+f.Index))
		}
	} else if a > 0 || !openEnd {
		r := "#" + strconv.Itoa(a) + "-"
		if !openEnd {
			r += strconv.Itoa(b)
		}
		sel = append(sel, r)
	}

	args := make([]string, 0, 10+len(sel))
	if len(sel) > 0 {
		args = append(args, "-U")
	}
	if c := o.Crop; c != nil && c.W >= 1 && c.H >= 1 && c.X >= 0 && c.Y >= 0 {
		args = append(args, "--crop", strconv.Itoa(c.X)+","+strconv.Itoa(c.Y)+"+"+strconv.Itoa(c.W)+"x"+strconv.Itoa(c.H))
	}
	args = append(args, in)
	args = append(args, sel...)
	args = append(args, "-O"+strconv.Itoa(DefaultGifsicleOptimize), "--careful")
	loop := "forever"
	if o.Loop > 0 {
		loop = strconv.Itoa(min(o.Loop, maxLoopCount))
	}
	return append(args, "--loopcount="+loop, "-o", out)
}
