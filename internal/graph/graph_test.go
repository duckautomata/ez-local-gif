package graph

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// --- fixtures ---------------------------------------------------------------

var (
	// ProRes 4444 from DaVinci Resolve: 10-bit, alpha, premultiplied.
	prores = recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p10le", Bits: 10,
		Width: 1920, Height: 1080, FPS: 30, Duration: 4, Frames: 120,
		HasAlpha: true, Kind: recipe.KindVideo, Premultiplied: true,
	}
	// Plain H.264 MP4: 4:2:0, no alpha, NTSC frame rate.
	h264 = recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", PixFmt: "yuv420p", Bits: 8,
		Width: 1280, Height: 720, FPS: 30000.0 / 1001, Duration: 10, Frames: 300,
		Kind: recipe.KindVideo,
	}
	// Animated GIF with transparency.
	gifSrc = recipe.ProbeInfo{
		Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8,
		Width: 480, Height: 270, FPS: 20, Duration: 3, Frames: 60,
		HasAlpha: true, Kind: recipe.KindAnimation,
	}
	// VP9 WebM with alpha_mode=1 (needs libvpx-vp9 to decode alpha).
	vp9 = recipe.ProbeInfo{
		Format: "matroska,webm", Codec: "vp9", PixFmt: "yuv420p", Bits: 8,
		Width: 640, Height: 360, FPS: 30, Duration: 2, Frames: 60,
		HasAlpha: true, Kind: recipe.KindVideo,
	}
	// A single transparent PNG.
	still = recipe.ProbeInfo{
		Format: "png_pipe", Codec: "png", PixFmt: "rgba", Bits: 8,
		Width: 800, Height: 600, Frames: 1, HasAlpha: true, IsStill: true, Kind: recipe.KindImage,
	}
	// Uploaded PNG sequence (Phase 2): 60 transparent 200x100 frames at the
	// default 100 ms, probed as 10 fps / 6 s.
	pngSeq = recipe.ProbeInfo{
		Format: "image2", Codec: "png", PixFmt: "rgba", Bits: 8,
		Width: 200, Height: 100, FPS: 10, Duration: 6, Frames: 60,
		HasAlpha: true, Kind: recipe.KindSequence,
		Sequence: &recipe.SequenceInfo{Count: 60, Pattern: "%06d.png", DelayMS: 100},
	}
	// Still AVIF with alpha (Phase 2): ffmpeg's mov demuxer exposes the
	// colour as stream 0 (opaque yuv420p) and the alpha as a gray stream 1.
	avifAlpha = recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "av1", PixFmt: "yuv420p", Bits: 8,
		Width: 64, Height: 64, Frames: 1, HasAlpha: true, IsStill: true, Kind: recipe.KindImage,
		AlphaStream: 1,
	}
)

// with returns a copy of src with fn applied.
func with(src recipe.ProbeInfo, fn func(*recipe.ProbeInfo)) recipe.ProbeInfo {
	fn(&src)
	return src
}

// withSeq returns a copy of src whose (copied) SequenceInfo has fn applied.
func withSeq(src recipe.ProbeInfo, fn func(*recipe.SequenceInfo)) recipe.ProbeInfo {
	info := *src.Sequence
	fn(&info)
	src.Sequence = &info
	return src
}

// op builds an Op with JSON-encoded params (nil params → no params).
func op(kind string, params any) recipe.Op {
	if params == nil {
		return recipe.Op{Kind: kind}
	}
	b, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	return recipe.Op{Kind: kind, Params: b}
}

// rawOp builds an Op with literal JSON params.
func rawOp(kind, params string) recipe.Op {
	return recipe.Op{Kind: kind, Params: json.RawMessage(params)}
}

func trim(start, end float64) recipe.Op {
	return op(recipe.OpTrim, recipe.TrimParams{Start: start, End: end})
}
func crop(x, y, w, h int) recipe.Op {
	return op(recipe.OpCrop, recipe.CropParams{X: x, Y: y, W: w, H: h})
}
func resize(w, h int, fit string) recipe.Op {
	return op(recipe.OpResize, recipe.ResizeParams{Width: w, Height: h, Fit: fit})
}
func canvas(w, h int, color string) recipe.Op {
	return op(recipe.OpCanvas, recipe.CanvasParams{Width: w, Height: h, Color: color})
}
func fps(v float64) recipe.Op   { return op(recipe.OpFPS, recipe.FPSParams{FPS: v}) }
func speed(v float64) recipe.Op { return op(recipe.OpSpeed, recipe.SpeedParams{Factor: v}) }
func flip(h, v bool) recipe.Op {
	return op(recipe.OpFlip, recipe.FlipParams{Horizontal: h, Vertical: v})
}
func rotate(deg int) recipe.Op { return op(recipe.OpRotate, recipe.RotateParams{Degrees: deg}) }
func unpremultiply() recipe.Op { return op(recipe.OpUnpremultiply, nil) }
func delay(ms int) recipe.Op   { return op(recipe.OpDelay, recipe.DelayParams{MS: ms}) }

func webp() recipe.Output { return recipe.Output{Format: "webp"} }
func gif() recipe.Output  { return recipe.Output{Format: "gif"} }

// seqArgs are the image2 input args of a sequence read at fps, followed by
// any extra args. Every sequence carries "-f image2" (the demuxer is forced
// explicitly, so the open never depends on the pattern's extension and
// matches the probe's "-f image2" read) and -reinit_filter 0: frames may
// differ per frame in pixel format or size, and a graph rebuild loses frames
// (see sequenceHead).
func seqArgs(fps string, extra ...string) []string {
	return append([]string{"-f", "image2", "-framerate", fps, "-start_number", "1", "-reinit_filter", "0"}, extra...)
}

// Heads emitted in front of the normal chain. Every sequence chain starts
// with the guarding scale (seqHead200 for the 200x100 pngSeq fixture); mixed
// sizes add format=rgba and the transparent per-frame pad.
const (
	seqHead200   = "[0:v]scale=200:100:force_original_aspect_ratio=decrease:flags=lanczos,"
	mixedHead200 = seqHead200 + "format=rgba,pad=200:100:(ow-iw)/2:(oh-ih)/2:color=0x00000000:eval=frame,"
	alphaHead1   = "[0:v:0]format=rgba[c];[0:v:1]format=gray[a];[c][a]alphamerge,"
)

// --- Compile golden tests ---------------------------------------------------

func TestCompile(t *testing.T) {
	tests := []struct {
		name string
		src  recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want Plan // OutLabel is implied; Speed 0 means 1
	}{
		{
			name: "no ops: fps and format only",
			src:  prores, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=30:round=down,format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "gif source to gif keeps source fps",
			src:  gifSrc, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down,format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "trim + fps 30 for gif keeps 30 (no 100/n snapping)",
			src:  h264, ops: []recipe.Op{trim(1.5, 4), fps(30)}, out: gif(),
			want: Plan{
				InputArgs: []string{"-ss", "1.5", "-to", "4"},
				Filter:    "[0:v]fps=30:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 30, Duration: 2.5, Frames: 75,
				TrimStart: 1.5, TrimEnd: 4,
			},
		},
		{
			name: "speed is emitted before fps even when listed after it",
			src:  h264, ops: []recipe.Op{fps(25), speed(2)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/2,fps=25:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 25, Duration: 5, Frames: 125, Speed: 2,
			},
		},
		{
			name: "slow motion doubles duration and keeps full factor precision",
			src:  h264, ops: []recipe.Op{speed(0.5)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/0.5,fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 20, Frames: 599, Speed: 0.5,
			},
		},
		{
			name: "multiple speed ops multiply",
			src:  h264, ops: []recipe.Op{speed(2), speed(1.5)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/3,fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10.0 / 3, Frames: 99, Speed: 3,
			},
		},
		{
			name: "crop then resize contain with alpha uses the premultiplied scale chain",
			src:  prores, ops: []recipe.Op{crop(100, 50, 800, 600), resize(400, 400, "contain")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=30:round=down,crop=800:600:100:50:exact=1,format=gbrap,premultiply=inplace=1,scale=400:300:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  400, Height: 300, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "resize contain default fit when fit is empty",
			src:  h264, ops: []recipe.Op{resize(400, 400, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=400:225:flags=lanczos,format=rgba[out]",
				Width:  400, Height: 225, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "resize cover scales to cover then centre-crops",
			src:  h264, ops: []recipe.Op{resize(400, 400, "cover")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=711:400:flags=lanczos,crop=400:400:155:0:exact=1,format=rgba[out]",
				Width:  400, Height: 400, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "resize exact stretches",
			src:  h264, ops: []recipe.Op{resize(400, 400, "exact")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=400:400:flags=lanczos,format=rgba[out]",
				Width:  400, Height: 400, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "resize width only keeps aspect",
			src:  h264, ops: []recipe.Op{resize(640, 0, "cover")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=640:360:flags=lanczos,format=rgba[out]",
				Width:  640, Height: 360, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "resize height only keeps aspect (alpha chain)",
			src:  prores, ops: []recipe.Op{resize(0, 540, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=30:round=down,format=gbrap,premultiply=inplace=1,scale=960:540:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  960, Height: 540, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "resize to the current size is a no-op",
			src:  h264, ops: []recipe.Op{resize(1280, 720, "exact")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "canvas pads transparent and marks alpha",
			src:  h264, ops: []recipe.Op{canvas(1280, 1280, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,pad=1280:1280:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  1280, Height: 1280, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "canvas crops the larger dimension before padding",
			src:  h264, ops: []recipe.Op{canvas(1000, 1000, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,crop=1000:720:140:0:exact=1,format=rgba,pad=1000:1000:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  1000, Height: 1000, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "canvas smaller in both dimensions only crops",
			src:  h264, ops: []recipe.Op{canvas(640, 640, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,crop=640:640:320:40:exact=1,format=rgba[out]",
				Width:  640, Height: 640, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "canvas with opaque colour does not add alpha (gif keeps the NTSC rate)",
			src:  h264, ops: []recipe.Op{canvas(1280, 1280, "#313338")}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,pad=1280:1280:(ow-iw)/2:(oh-ih)/2:color=0x313338,format=rgba[out]",
				Width:  1280, Height: 1280, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "canvas with semi-transparent RRGGBBAA colour adds alpha",
			src:  h264, ops: []recipe.Op{canvas(1280, 1280, "FF000080")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,pad=1280:1280:(ow-iw)/2:(oh-ih)/2:color=0xff000080,format=rgba[out]",
				Width:  1280, Height: 1280, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "canvas equal to the frame is a no-op",
			src:  h264, ops: []recipe.Op{canvas(1280, 720, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "rotate 90 swaps dimensions",
			src:  h264, ops: []recipe.Op{rotate(90)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,transpose=1,format=rgba[out]",
				Width:  720, Height: 1280, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "rotate 180 is hflip+vflip",
			src:  h264, ops: []recipe.Op{rotate(180)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,hflip,vflip,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "rotate 270 swaps dimensions",
			src:  h264, ops: []recipe.Op{rotate(270)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,transpose=2,format=rgba[out]",
				Width:  720, Height: 1280, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "flip horizontal and vertical",
			src:  h264, ops: []recipe.Op{flip(true, true)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,hflip,vflip,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "flip vertical only",
			src:  h264, ops: []recipe.Op{flip(false, true)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,vflip,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "flip with neither direction is a no-op",
			src:  h264, ops: []recipe.Op{flip(false, false)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "geometry ops apply in order: crop after rotate uses the rotated frame",
			src:  h264, ops: []recipe.Op{rotate(90), crop(0, 0, 720, 1000)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,transpose=1,crop=720:1000:0:0:exact=1,format=rgba[out]",
				Width:  720, Height: 1000, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "geometry ops apply in order: crop after resize uses the scaled frame",
			src:  h264, ops: []recipe.Op{resize(640, 0, ""), crop(0, 0, 640, 200)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=640:360:flags=lanczos,crop=640:200:0:0:exact=1,format=rgba[out]",
				Width:  640, Height: 200, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "output fit contain 128x128 (emote): alpha scale + transparent pad",
			src:  prores, out: recipe.Output{Format: "gif", Width: 128, Height: 128, FPS: 25},
			want: Plan{
				Filter: "[0:v]fps=25:round=down,format=gbrap,premultiply=inplace=1,scale=128:72:flags=lanczos,unpremultiply=inplace=1,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  128, Height: 128, FPS: 25, HasAlpha: true, Duration: 4, Frames: 100,
			},
		},
		{
			name: "output fit contain on an opaque source introduces alpha",
			src:  h264, out: recipe.Output{Format: "webp", Width: 128, Height: 128},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=128:72:flags=lanczos,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  128, Height: 128, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "output fit with only width scales keeping aspect, no pad",
			src:  h264, out: recipe.Output{Format: "webp", Width: 480},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=480:270:flags=lanczos,format=rgba[out]",
				Width:  480, Height: 270, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "output fit with only height scales keeping aspect",
			src:  h264, out: recipe.Output{Format: "webp", Height: 360, Fit: "contain"},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=640:360:flags=lanczos,format=rgba[out]",
				Width:  640, Height: 360, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "output fit cover 320x320 (sticker) centre-crops",
			src:  h264, out: recipe.Output{Format: "gif", Width: 320, Height: 320, Fit: "cover", FPS: 20},
			want: Plan{
				Filter: "[0:v]fps=20:round=down,scale=569:320:flags=lanczos,crop=320:320:124:0:exact=1,format=rgba[out]",
				Width:  320, Height: 320, FPS: 20, Duration: 10, Frames: 200,
			},
		},
		{
			name: "output fit exact stretches",
			src:  h264, out: recipe.Output{Format: "webp", Width: 300, Height: 300, Fit: "exact"},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=300:300:flags=lanczos,format=rgba[out]",
				Width:  300, Height: 300, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "output fit matching the frame emits nothing",
			src:  still, out: recipe.Output{Format: "webp", Width: 800, Height: 600},
			want: Plan{
				Filter: "[0:v]format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "output fit follows the op stack",
			src:  prores, ops: []recipe.Op{crop(0, 0, 1080, 1080)}, out: recipe.Output{Format: "webp", Width: 128, Height: 128},
			want: Plan{
				Filter: "[0:v]fps=30:round=down,crop=1080:1080:0:0:exact=1,format=gbrap,premultiply=inplace=1,scale=128:128:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  128, Height: 128, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "unpremultiply is hoisted first at 10-bit",
			src:  prores, ops: []recipe.Op{crop(0, 0, 960, 1080), unpremultiply()}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=gbrap10le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,fps=30:round=down,crop=960:1080:0:0:exact=1,format=rgba[out]",
				Width:  960, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "unpremultiply at 12-bit (ProRes 4444 XQ)",
			src:  with(prores, func(p *recipe.ProbeInfo) { p.PixFmt, p.Bits = "yuva444p12le", 12 }),
			ops:  []recipe.Op{unpremultiply(), speed(2)}, out: gif(),
			want: Plan{
				Filter: "[0:v]format=gbrap12le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,setpts=PTS/2,fps=30:round=down,format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60, Speed: 2,
			},
		},
		{
			name: "unpremultiply at 8-bit uses gbrap",
			src:  gifSrc, ops: []recipe.Op{unpremultiply()}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,fps=20:round=down,format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "unpremultiply on a source without alpha is a no-op",
			src:  h264, ops: []recipe.Op{unpremultiply()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "vp9 alpha forces libvpx-vp9",
			src:  vp9, out: webp(),
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx-vp9"},
				Filter:    "[0:v]fps=30:round=down,format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60,
			},
		},
		{
			name: "vp9 alpha decoder args precede the trim seek args",
			src:  vp9, ops: []recipe.Op{trim(1, 0)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx-vp9", "-ss", "1"},
				Filter:    "[0:v]fps=30:round=down,format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 1, Frames: 30,
				TrimStart: 1,
			},
		},
		{
			name: "vp8 alpha forces libvpx",
			src:  with(vp9, func(p *recipe.ProbeInfo) { p.Codec = "vp8" }), out: webp(),
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx"},
				Filter:    "[0:v]fps=30:round=down,format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60,
			},
		},
		{
			name: "vp9 without alpha uses the native decoder",
			src:  with(vp9, func(p *recipe.ProbeInfo) { p.HasAlpha = false }), out: webp(),
			want: Plan{
				Filter: "[0:v]fps=30:round=down,format=rgba[out]",
				Width:  640, Height: 360, FPS: 30, Duration: 2, Frames: 60,
			},
		},
		{
			name: "trim start only",
			src:  prores, ops: []recipe.Op{trim(1, 0)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1"},
				Filter:    "[0:v]fps=30:round=down,format=rgba[out]",
				Width:     1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 3, Frames: 90,
				TrimStart: 1,
			},
		},
		{
			name: "trim end only",
			src:  prores, ops: []recipe.Op{trim(0, 2.5)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-to", "2.5"},
				Filter:    "[0:v]fps=30:round=down,format=rgba[out]",
				Width:     1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 2.5, Frames: 75,
				TrimEnd: 2.5,
			},
		},
		{
			name: "trim end at or beyond the source end reads to the end",
			src:  h264, ops: []recipe.Op{trim(2, 99)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "2"},
				Filter:    "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 8, Frames: 239,
				TrimStart: 2,
			},
		},
		{
			name: "trim times are rounded to milliseconds",
			src:  h264, ops: []recipe.Op{trim(0.12345, 1.98765)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "0.123", "-to", "1.988"},
				Filter:    "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 1.865, Frames: 55,
				TrimStart: 0.123, TrimEnd: 1.988,
			},
		},
		{
			name: "trim end <= 0 means to the end",
			src:  h264, ops: []recipe.Op{trim(1, -1)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1"},
				Filter:    "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 9, Frames: 269,
				TrimStart: 1,
			},
		},
		{
			name: "identity trim emits nothing",
			src:  still, ops: []recipe.Op{trim(0, 0)}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "multiple trims intersect in source time",
			src:  h264, ops: []recipe.Op{trim(1, 5), trim(2, 8)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "2", "-to", "5"},
				Filter:    "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 3, Frames: 89,
				TrimStart: 2, TrimEnd: 5,
			},
		},
		{
			name: "trim + speed: duration is trimmed length over speed",
			src:  h264, ops: []recipe.Op{trim(2, 6), speed(4)}, out: gif(),
			want: Plan{
				InputArgs: []string{"-ss", "2", "-to", "6"},
				Filter:    "[0:v]setpts=PTS/4,fps=29.97:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 1, Frames: 29,
				TrimStart: 2, TrimEnd: 6, Speed: 4,
			},
		},
		{
			name: "last fps op wins",
			src:  h264, ops: []recipe.Op{fps(10), fps(20)}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 20, Duration: 10, Frames: 200,
			},
		},
		{
			name: "fps op beats output fps",
			src:  h264, ops: []recipe.Op{fps(10)}, out: recipe.Output{Format: "webp", FPS: 25},
			want: Plan{
				Filter: "[0:v]fps=10:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 10, Duration: 10, Frames: 100,
			},
		},
		{
			name: "output fps is used as-is for gif (24 stays 24)",
			src:  h264, out: recipe.Output{Format: "gif", FPS: 24},
			want: Plan{
				Filter: "[0:v]fps=24:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 24, Duration: 10, Frames: 240,
			},
		},
		{
			name: "output fps above the gif cap is capped at 50",
			src:  h264, out: recipe.Output{Format: "gif", FPS: 60},
			want: Plan{
				Filter: "[0:v]fps=50:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 50, Duration: 10, Frames: 500,
			},
		},
		{
			name: "fractional fps for gif is rounded to 3 decimals, not snapped",
			src:  h264, ops: []recipe.Op{fps(100.0 / 3)}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=33.333:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 33.333, Duration: 10, Frames: 333,
			},
		},
		{
			name: "source fps is capped at 60 for webp",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.FPS = 120 }), out: webp(),
			want: Plan{
				Filter: "[0:v]fps=60:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 60, Duration: 10, Frames: 600,
			},
		},
		{
			name: "source fps is capped at 50 for gif",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.FPS = 120 }), out: gif(),
			want: Plan{
				Filter: "[0:v]fps=50:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 50, Duration: 10, Frames: 500,
			},
		},
		{
			name: "still image defaults to 10 fps, one frame, no duration (no fps filter: it would emit zero frames)",
			src:  still, out: gif(),
			want: Plan{
				Filter: "[0:v]format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "unknown source duration and frames stay 0",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 0, 0 }), out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97,
			},
		},
		{
			name: "duration falls back to frames/fps when only those are known",
			src:  with(gifSrc, func(p *recipe.ProbeInfo) { p.Duration = 0 }), out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down,format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "op params may be null",
			src:  h264, ops: []recipe.Op{rawOp(recipe.OpFlip, "null"), rawOp(recipe.OpUnpremultiply, `{"ignored":true}`)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "limits: 8192x4096 (exactly 32 megapixels) is accepted",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 1, 30 }),
			ops:  []recipe.Op{resize(8192, 4096, "exact")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=8192:4096:flags=lanczos,format=rgba[out]",
				Width:  8192, Height: 4096, FPS: 29.97, Duration: 1, Frames: 29,
			},
		},
		{
			name: "limits: speed 0.05 (the minimum) is accepted",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.Width, p.Height = 320, 180 }),
			ops:  []recipe.Op{speed(0.05)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/0.05,fps=29.97:round=down,format=rgba[out]",
				Width:  320, Height: 180, FPS: 29.97, Duration: 200, Frames: 5994, Speed: 0.05,
			},
		},
		{
			name: "limits: speed 100 (the maximum) is accepted",
			src:  h264, ops: []recipe.Op{speed(100)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/100,fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 0.1, Frames: 2, Speed: 100,
			},
		},
		{
			name: "limits: a master of exactly 8 GiB is accepted",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.Width, p.Height, p.FPS, p.Duration, p.Frames = 2048, 1024, 1, 1024, 1024 }),
			out:  webp(),
			want: Plan{
				Filter: "[0:v]fps=1:round=down,format=rgba[out]",
				Width:  2048, Height: 1024, FPS: 1, Duration: 1024, Frames: 1024,
			},
		},
		{
			name: "full emote pipeline: unpremultiply, trim, speed, crop, fit 128, gif 25 fps",
			src:  prores,
			ops:  []recipe.Op{unpremultiply(), trim(0.5, 3.5), crop(420, 0, 1080, 1080), speed(1.5)},
			out:  recipe.Output{Format: "gif", Width: 128, Height: 128, FPS: 25, Preset: "emote", Target: "emote"},
			want: Plan{
				InputArgs: []string{"-ss", "0.5", "-to", "3.5"},
				Filter:    "[0:v]format=gbrap10le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,setpts=PTS/1.5,fps=25:round=down,crop=1080:1080:420:0:exact=1,format=gbrap,premultiply=inplace=1,scale=128:128:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:     128, Height: 128, FPS: 25, HasAlpha: true, Duration: 2, Frames: 50,
				TrimStart: 0.5, TrimEnd: 3.5, Speed: 1.5,
			},
		},

		// --- Phase 2: image sequences -------------------------------------
		{
			name: "sequence: image2 framerate from the probed delay, one master frame per image",
			src:  pngSeq, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "sequence: delay op overrides the probed delay and retimes the source",
			src:  pngSeq, ops: []recipe.Op{delay(40)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("25"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=25:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 25, HasAlpha: true, Duration: 2.4, Frames: 60, SourceFPS: 25,
			},
		},
		{
			name: "sequence: last delay op wins",
			src:  pngSeq, ops: []recipe.Op{delay(100), delay(50)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("20"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=20:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60, SourceFPS: 20,
			},
		},
		{
			name: "sequence: delay op is hoisted wherever it sits in the stack",
			src:  pngSeq, ops: []recipe.Op{fps(10), resize(100, 0, ""), delay(200)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("5"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=gbrap,premultiply=inplace=1,scale=100:50:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  100, Height: 50, FPS: 10, HasAlpha: true, Duration: 12, Frames: 120, SourceFPS: 5,
			},
		},
		{
			name: "sequence: fractional framerate is rounded to 3 decimals",
			src:  pngSeq, ops: []recipe.Op{delay(33)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("30.303"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=30.303:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 30.303, HasAlpha: true, Duration: 60 / 30.303, Frames: 60, SourceFPS: 30.303,
			},
		},
		{
			name: "sequence: 1 ms delay is 1000 fps at the demuxer, capped by the output format",
			src:  pngSeq, ops: []recipe.Op{delay(1)}, out: gif(),
			want: Plan{
				InputArgs: seqArgs("1000"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=50:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 50, HasAlpha: true, Duration: 0.06, Frames: 3, SourceFPS: 1000,
			},
		},
		{
			name: "sequence: 60 s delay is the slowest rate",
			src:  pngSeq, ops: []recipe.Op{delay(60000)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("0.017"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=0.017:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 0.017, HasAlpha: true, Duration: 60 / 0.017, Frames: 60, SourceFPS: 0.017,
			},
		},
		{
			name: "sequence: probe without a delay defaults to 100 ms",
			src:  withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.DelayMS = 0 }), out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "sequence: probed delay other than 100 ms sets the rate",
			src:  with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.DelayMS = 250 }), func(p *recipe.ProbeInfo) { p.FPS, p.Duration = 4, 15 }),
			out:  webp(),
			want: Plan{
				InputArgs: seqArgs("4"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=4:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 4, HasAlpha: true, Duration: 15, Frames: 60, SourceFPS: 4,
			},
		},
		{
			name: "delay op on a non-sequence source is ignored",
			src:  h264, ops: []recipe.Op{delay(40)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "sequence: mixed sizes get the normalising head, -reinit_filter 0 and transparent padding",
			src:  with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true }), func(p *recipe.ProbeInfo) { p.HasAlpha = false; p.PixFmt = "rgb24" }),
			out:  webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "sequence: mixed head precedes the hoisted unpremultiply, which then runs at 8-bit",
			src:  with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true }), func(p *recipe.ProbeInfo) { p.Bits = 12 }),
			ops:  []recipe.Op{unpremultiply(), speed(2)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,setpts=PTS/2,fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 3, Frames: 30, SourceFPS: 10, Speed: 2,
			},
		},
		{
			name: "sequence: unpremultiply without a head keeps the native depth",
			src:  with(pngSeq, func(p *recipe.ProbeInfo) { p.Bits = 12 }),
			ops:  []recipe.Op{unpremultiply()}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "format=gbrap12le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "sequence: mixed head on an opaque sequence does not enable the unpremultiply op",
			src:  with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true }), func(p *recipe.ProbeInfo) { p.HasAlpha = false }),
			ops:  []recipe.Op{unpremultiply()}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "sequence: mixed head then geometry ops see the normalised size",
			src:  withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true }),
			ops:  []recipe.Op{crop(50, 0, 100, 100), rotate(90)}, out: recipe.Output{Format: "gif", Width: 64, Height: 64},
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "fps=10:round=down,crop=100:100:50:0:exact=1,transpose=1,format=gbrap,premultiply=inplace=1,scale=64:64:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "sequence: trim seeks in sequence time and speed shortens it",
			src:  pngSeq, ops: []recipe.Op{trim(1, 3), speed(2)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10", "-ss", "1", "-to", "3"), InputPattern: "%06d.png",
				Filter: seqHead200 + "setpts=PTS/2,fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 1, Frames: 10, SourceFPS: 10,
				TrimStart: 1, TrimEnd: 3, Speed: 2,
			},
		},
		{
			name: "sequence: trim uses the timeline of the delay op",
			src:  pngSeq, ops: []recipe.Op{delay(50), trim(1, 0)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("20", "-ss", "1"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=20:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 20, HasAlpha: true, Duration: 2, Frames: 40, SourceFPS: 20,
				TrimStart: 1,
			},
		},
		{
			name: "sequence: fps op resamples the images",
			src:  pngSeq, ops: []recipe.Op{fps(5)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=5:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 5, HasAlpha: true, Duration: 6, Frames: 30, SourceFPS: 10,
			},
		},
		{
			name: "sequence: output fps is honoured like for a video",
			src:  pngSeq, out: recipe.Output{Format: "webp", FPS: 30},
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=30:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 30, HasAlpha: true, Duration: 6, Frames: 180, SourceFPS: 10,
			},
		},
		{
			name: "sequence: unknown count falls back to the probed frame count",
			src:  withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Count = 0 }), out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "sequence: unknown count and frames leave duration unknown",
			src:  with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Count = 0 }), func(p *recipe.ProbeInfo) { p.Frames = 0 }),
			out:  webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, SourceFPS: 10,
			},
		},
		{
			name: "sequence: a single frame is compiled like a still (no fps filter)",
			src:  with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Count = 1 }), func(p *recipe.ProbeInfo) { p.Frames, p.Duration = 1, 0.1 }),
			ops:  []recipe.Op{speed(2)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "setpts=PTS/2,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Frames: 1, SourceFPS: 10, Speed: 2,
			},
		},

		// --- Phase 2: separate alpha stream ------------------------------
		{
			name: "alpha stream: a still AVIF merges colour and alpha in front of the chain",
			src:  avifAlpha, out: webp(),
			want: Plan{
				Filter: alphaHead1 + "format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "alpha stream: the hoisted unpremultiply follows the merge at 8-bit, then the alpha scale chain",
			src:  with(avifAlpha, func(p *recipe.ProbeInfo) { p.PixFmt, p.Bits = "yuv420p10le", 10 }),
			ops:  []recipe.Op{unpremultiply()}, out: recipe.Output{Format: "webp", Width: 32, Height: 32},
			want: Plan{
				Filter: alphaHead1 + "format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=gbrap,premultiply=inplace=1,scale=32:32:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  32, Height: 32, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "alpha stream: an animation keeps the fps stage after the merge",
			src: with(avifAlpha, func(p *recipe.ProbeInfo) {
				p.IsStill, p.Kind, p.FPS, p.Duration, p.Frames, p.AlphaStream = false, recipe.KindAnimation, 10, 2, 20, 3
			}),
			out: gif(),
			want: Plan{
				Filter: "[0:v:0]format=rgba[c];[0:v:3]format=gray[a];[c][a]alphamerge,fps=10:round=down,format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Duration: 2, Frames: 20,
			},
		},
		{
			// ffmpeg's mov demuxer lists the one-frame primary item first; the
			// animation's colour track is v:2 and its alpha v:3.
			name: "alpha stream: an animated AVIF addresses its colour track, not the primary item",
			src: with(avifAlpha, func(p *recipe.ProbeInfo) {
				p.IsStill, p.Kind, p.FPS, p.Duration, p.Frames = false, recipe.KindAnimation, 10, 2, 20
				p.ColorStream, p.AlphaStream = 2, 3
			}),
			out: gif(),
			want: Plan{
				Filter: "[0:v:2]format=rgba[c];[0:v:3]format=gray[a];[c][a]alphamerge,fps=10:round=down,format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Duration: 2, Frames: 20,
			},
		},
		{
			name: "colour stream without alpha: an opaque animated AVIF reads its track",
			src: with(avifAlpha, func(p *recipe.ProbeInfo) {
				p.IsStill, p.Kind, p.FPS, p.Duration, p.Frames = false, recipe.KindAnimation, 10, 2, 20
				p.ColorStream, p.AlphaStream, p.HasAlpha = 1, 0, false
			}),
			out: gif(),
			want: Plan{
				Filter: "[0:v:1]fps=10:round=down,format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, Duration: 2, Frames: 20,
			},
		},
		{
			name: "alpha stream: the plan has alpha even when the probe did not say so",
			src:  with(avifAlpha, func(p *recipe.ProbeInfo) { p.HasAlpha = false }), out: webp(),
			want: Plan{
				Filter: alphaHead1 + "format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "alpha stream: trim seek args still apply",
			src: with(avifAlpha, func(p *recipe.ProbeInfo) {
				p.IsStill, p.Kind, p.FPS, p.Duration, p.Frames = false, recipe.KindAnimation, 10, 2, 20
			}),
			ops: []recipe.Op{trim(0.5, 1.5)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "0.5", "-to", "1.5"},
				Filter:    alphaHead1 + "fps=10:round=down,format=rgba[out]",
				Width:     64, Height: 64, FPS: 10, HasAlpha: true, Duration: 1, Frames: 10,
				TrimStart: 0.5, TrimEnd: 1.5,
			},
		},

		// --- Phase 2: static / frames / apng / avif outputs ----------------
		{
			name: "png output compiles the full master (jobs takes frame 0)",
			src:  prores, ops: []recipe.Op{trim(1, 2)}, out: recipe.Output{Format: recipe.FormatPNG, Width: 128, Height: 128},
			want: Plan{
				InputArgs: []string{"-ss", "1", "-to", "2"},
				Filter:    "[0:v]fps=30:round=down,format=gbrap,premultiply=inplace=1,scale=128:72:flags=lanczos,unpremultiply=inplace=1,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:     128, Height: 128, FPS: 30, HasAlpha: true, Duration: 1, Frames: 30,
				TrimStart: 1, TrimEnd: 2,
			},
		},
		{
			name: "jpeg output of a still",
			src:  still, out: recipe.Output{Format: recipe.FormatJPEG, Quality: 90},
			want: Plan{
				Filter: "[0:v]format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "frames export with a jpeg frame format and a fit budget",
			src:  h264, out: recipe.Output{Format: recipe.FormatFrames, FrameFormat: recipe.FormatJPEG, FitBytes: 250000},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "apng output caps the rate at 60",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.FPS = 120 }), out: recipe.Output{Format: recipe.FormatAPNG, Width: 320, Height: 320},
			want: Plan{
				Filter: "[0:v]fps=60:round=down,scale=320:180:flags=lanczos,format=rgba,pad=320:320:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  320, Height: 320, FPS: 60, HasAlpha: true, Duration: 10, Frames: 600,
			},
		},
		{
			name: "avif output from a sequence",
			src:  pngSeq, out: recipe.Output{Format: recipe.FormatAVIF, Quality: 60, FrameFormat: recipe.FormatWebP},
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Compile(tc.src, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			want := tc.want
			want.OutLabel = "[out]"
			if want.Speed == 0 {
				want.Speed = 1
			}
			// SourceFPS is the probed rate verbatim (0 for stills) unless a
			// case states it (sequences report the image2 rate). TestSourceFPS
			// covers the rule itself.
			if want.SourceFPS == 0 {
				want.SourceFPS = tc.src.FPS
			}
			checkPlan(t, got, &want)
		})
	}
}

// TestSourceFPS checks that Plan.SourceFPS reports the probed source rate
// unsnapped and independent of the fps op / output fps, and 0 when unknown.
func TestSourceFPS(t *testing.T) {
	tests := []struct {
		name string
		src  recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want float64
	}{
		{"video keeps the probed rate", prores, nil, webp(), 30},
		{"ntsc rate is not snapped or rounded", h264, nil, gif(), 30000.0 / 1001},
		{"fps op does not change it", h264, []recipe.Op{fps(10)}, webp(), 30000.0 / 1001},
		{"output fps does not change it", h264, nil, recipe.Output{Format: "gif", FPS: 12}, 30000.0 / 1001},
		{"speed does not change it", h264, []recipe.Op{speed(2)}, webp(), 30000.0 / 1001},
		{"animation avg rate", gifSrc, nil, gif(), 20},
		{"still has no source rate", still, nil, gif(), 0},
		{"unknown rate stays 0", with(h264, func(p *recipe.ProbeInfo) { p.FPS = 0 }), nil, webp(), 0},
		{"infinite rate is treated as unknown", with(h264, func(p *recipe.ProbeInfo) { p.FPS = math.Inf(1) }), nil, webp(), 0},
		{"sequence reports the image2 rate", pngSeq, nil, webp(), 10},
		{"sequence delay op changes it", pngSeq, []recipe.Op{delay(40)}, webp(), 25},
		{"sequence rate ignores the probed fps", with(pngSeq, func(p *recipe.ProbeInfo) { p.FPS = 99 }), nil, webp(), 10},
		{"sequence output fps does not change it", pngSeq, nil, recipe.Output{Format: "gif", FPS: 30}, 10},
		{"sequence fps op does not change it", pngSeq, []recipe.Op{fps(3)}, webp(), 10},
		{"alpha-stream still has no source rate", avifAlpha, nil, webp(), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(tc.src, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if math.Abs(p.SourceFPS-tc.want) > 1e-9 {
				t.Errorf("SourceFPS = %v, want %v", p.SourceFPS, tc.want)
			}
		})
	}
}

// TestFramesNeverExceedDuration is the J2 guard: the planned frame count
// must never make the clip longer than the (trimmed, speed-adjusted) source
// — Frames/FPS <= Duration — matching the fps stage's round=down. Before the
// fix Frames was round(Duration*FPS), so a 5.0 s sticker clip at 16.7 fps
// planned 84 frames = 5.03 s and blew the Discord 5 s cap. The only
// exception is a clip shorter than one frame, where the >= 1 guard keeps a
// single frame.
func TestFramesNeverExceedDuration(t *testing.T) {
	for _, src := range []recipe.ProbeInfo{prores, h264, gifSrc, vp9, pngSeq} {
		for _, fps := range []float64{4, 5, 10, 12.5, 15, 16.7, 20, 25, 29.97, 30, 33.333, 50, 60} {
			p, err := Compile(src, nil, recipe.Output{Format: "webp", FPS: fps})
			if err != nil {
				t.Fatalf("Compile(%s, fps %v): %v", src.Codec, fps, err)
			}
			if p.Duration <= 0 || p.Frames <= 1 {
				continue
			}
			if got := float64(p.Frames) / p.FPS; got > p.Duration+1e-9 {
				t.Errorf("%s at %v fps: %d frames / %v fps = %v s > duration %v s", src.Codec, fps, p.Frames, p.FPS, got, p.Duration)
			}
		}
	}
	// The exactly-5.0 s sticker cases that motivated round=down: the 16.7 and
	// 12.5 fps ladder rungs must plan <= 5.0 s.
	five := with(h264, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 5, 150 })
	for _, fps := range []float64{16.7, 12.5} {
		p, err := Compile(five, nil, recipe.Output{Format: "apng", FPS: fps})
		if err != nil {
			t.Fatal(err)
		}
		if got := float64(p.Frames) / p.FPS; got > 5.0+1e-9 {
			t.Errorf("5.0 s at %v fps plans %d frames = %v s (> 5.0 s)", fps, p.Frames, got)
		}
	}
}

// checkPlan compares every Plan field, with a tolerance on floats.
func checkPlan(t *testing.T, got, want *Plan) {
	t.Helper()
	if got.Filter != want.Filter {
		t.Errorf("Filter\n got: %s\nwant: %s", got.Filter, want.Filter)
	}
	if !reflect.DeepEqual(got.InputArgs, want.InputArgs) {
		t.Errorf("InputArgs got %q want %q", got.InputArgs, want.InputArgs)
	}
	if got.OutLabel != want.OutLabel {
		t.Errorf("OutLabel got %q want %q", got.OutLabel, want.OutLabel)
	}
	if got.InputPattern != want.InputPattern {
		t.Errorf("InputPattern got %q want %q", got.InputPattern, want.InputPattern)
	}
	if !strings.HasSuffix(got.Filter, got.OutLabel) {
		t.Errorf("Filter must end with %s: %s", got.OutLabel, got.Filter)
	}
	// Either a single chain from [0:v] (or [0:v:N] for an explicit colour
	// stream), or the alpha-stream merge head
	// ("[0:v:C]…[c];[0:v:N]…[a];[c][a]alphamerge,") followed by one chain.
	chain := got.Filter
	if i := strings.Index(chain, "]alphamerge,"); strings.HasPrefix(chain, "[0:v:") && strings.Contains(chain, "]format=rgba[c];[0:v:") && i > 0 {
		chain = chain[i+len("]alphamerge,"):]
	} else if !strings.HasPrefix(chain, "[0:v]") && !strings.HasPrefix(chain, "[0:v:") {
		t.Errorf("Filter must start with [0:v], [0:v:N] or the alpha-stream head: %s", got.Filter)
	}
	if strings.Contains(chain, ";") {
		t.Errorf("Filter must be a single chain after the head: %s", got.Filter)
	}
	if got.Width != want.Width || got.Height != want.Height {
		t.Errorf("size got %dx%d want %dx%d", got.Width, got.Height, want.Width, want.Height)
	}
	if got.HasAlpha != want.HasAlpha {
		t.Errorf("HasAlpha got %v want %v", got.HasAlpha, want.HasAlpha)
	}
	if got.Frames != want.Frames {
		t.Errorf("Frames got %d want %d", got.Frames, want.Frames)
	}
	floats := []struct {
		name      string
		got, want float64
	}{
		{"FPS", got.FPS, want.FPS},
		{"Duration", got.Duration, want.Duration},
		{"TrimStart", got.TrimStart, want.TrimStart},
		{"TrimEnd", got.TrimEnd, want.TrimEnd},
		{"Speed", got.Speed, want.Speed},
		{"SourceFPS", got.SourceFPS, want.SourceFPS},
	}
	for _, f := range floats {
		if math.Abs(f.got-f.want) > 1e-9 {
			t.Errorf("%s got %v want %v", f.name, f.got, f.want)
		}
	}
}

// --- Compile error cases ----------------------------------------------------

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name string
		src  recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want string // substring of the error
	}{
		{"unknown op kind", h264, []recipe.Op{op("blur", nil)}, webp(), `op 0: unknown op kind "blur"`},
		{"invalid params json", h264, []recipe.Op{rawOp(recipe.OpCrop, `{"w":"wide"}`)}, webp(), "op 0 (crop): invalid params"},
		{"malformed params json", h264, []recipe.Op{rawOp(recipe.OpFPS, `{`)}, webp(), "op 0 (fps): invalid params"},
		{"crop out of range", h264, []recipe.Op{crop(1000, 0, 400, 400)}, webp(), "op 0 (crop): rectangle 400x400 at (1000,0) exceeds the 1280x720 frame"},
		{"crop out of range after resize", h264, []recipe.Op{resize(640, 0, ""), crop(0, 0, 640, 361)}, webp(), "op 1 (crop): rectangle 640x361 at (0,0) exceeds the 640x360 frame"},
		{"crop zero size", h264, []recipe.Op{crop(0, 0, 0, 100)}, webp(), "op 0 (crop): size must be at least 1x1"},
		{"crop negative offset", h264, []recipe.Op{crop(-1, 0, 100, 100)}, webp(), "op 0 (crop): offset must be >= 0"},
		{"speed zero", h264, []recipe.Op{speed(0)}, webp(), "op 0 (speed): factor must be > 0 (got 0)"},
		{"speed negative", h264, []recipe.Op{speed(-2)}, webp(), "op 0 (speed): factor must be > 0"},
		{"speed missing params", h264, []recipe.Op{op(recipe.OpSpeed, nil)}, webp(), "op 0 (speed): factor must be > 0"},
		{"speed below the minimum", h264, []recipe.Op{speed(0.01)}, webp(), "op 0 (speed): factor must be between 0.05 and 100 (got 0.01)"},
		{"speed above the maximum", h264, []recipe.Op{speed(101)}, webp(), "op 0 (speed): factor must be between 0.05 and 100 (got 101)"},
		{"combined speed below the minimum", h264, []recipe.Op{speed(0.1), speed(0.1)}, webp(), "combined speed factor 0.010000000000000002 must be between 0.05 and 100"},
		{"combined speed above the maximum", h264, []recipe.Op{speed(50), speed(4)}, gif(), "combined speed factor 200 must be between 0.05 and 100"},
		{"rotate 45", h264, []recipe.Op{rotate(45)}, webp(), "op 0 (rotate): degrees must be 90, 180 or 270 (got 45)"},
		{"rotate 0", h264, []recipe.Op{rotate(0)}, webp(), "op 0 (rotate): degrees must be 90, 180 or 270"},
		{"trim end equal to start", h264, []recipe.Op{trim(3, 3)}, webp(), "op 0 (trim): end (3 s) must be after start (3 s)"},
		{"trim end before start", h264, []recipe.Op{trim(3, 1)}, webp(), "op 0 (trim): end (1 s) must be after start (3 s)"},
		{"trim negative start", h264, []recipe.Op{trim(-1, 2)}, webp(), "op 0 (trim): start must be >= 0"},
		{"trim start beyond source end", h264, []recipe.Op{trim(10, 0)}, webp(), "trim start (10 s) is at or beyond the end of the source (10 s)"},
		{"trim on a still", still, []recipe.Op{trim(1, 0)}, webp(), "op 0 (trim): the source is a still image"},
		{"trims do not overlap", h264, []recipe.Op{trim(5, 8), trim(1, 3)}, webp(), "trim ranges do not overlap"},
		{"fps zero", h264, []recipe.Op{fps(0)}, webp(), "op 0 (fps): fps must be > 0"},
		{"fps negative", h264, []recipe.Op{fps(-5)}, gif(), "op 0 (fps): fps must be > 0"},
		{"resize without dimensions", h264, []recipe.Op{resize(0, 0, "")}, webp(), "op 0 (resize): width or height is required"},
		{"resize negative", h264, []recipe.Op{resize(-10, 0, "")}, webp(), "op 0 (resize): width and height must be >= 0"},
		{"resize unknown fit", h264, []recipe.Op{resize(100, 100, "stretch")}, webp(), `op 0 (resize): fit "stretch" must be one of contain, cover, exact`},
		{"canvas zero size", h264, []recipe.Op{canvas(0, 100, "")}, webp(), "op 0 (canvas): size must be at least 1x1"},
		{"canvas bad colour", h264, []recipe.Op{canvas(100, 100, "red")}, webp(), `op 0 (canvas): colour "red"`},
		{"output negative width", h264, nil, recipe.Output{Format: "webp", Width: -1}, "output: width and height must be >= 0"},
		{"output unknown fit", h264, nil, recipe.Output{Format: "webp", Width: 100, Height: 100, Fit: "fill"}, `output: fit "fill" must be one of contain, cover, exact`},
		{"output negative fps", h264, nil, recipe.Output{Format: "webp", FPS: -1}, "output fps must be >= 0"},
		{"source without size", recipe.ProbeInfo{}, nil, webp(), "source has no usable frame size"},
		{"error names the failing op index", h264, []recipe.Op{flip(true, false), rotate(90), crop(0, 0, 5000, 5)}, webp(), "op 2 (crop)"},

		// Upper bounds.
		{"resize width above 8192", h264, []recipe.Op{resize(8193, 0, "")}, webp(), "op 0 (resize): width and height must be <= 8192 (got 8193x0)"},
		{"resize height above 8192", h264, []recipe.Op{resize(0, 9000, "")}, webp(), "op 0 (resize): width and height must be <= 8192 (got 0x9000)"},
		{"resize result above 32 megapixels", h264, []recipe.Op{resize(8000, 8000, "exact")}, webp(), "op 0 (resize): frame 8000x8000 exceeds the limits (8192 px per side, 32 megapixels)"},
		{"resize by width on an extreme aspect blows the height", with(h264, func(p *recipe.ProbeInfo) { p.Width, p.Height = 10, 4000 }), []recipe.Op{resize(1000, 0, "")}, webp(), "op 0 (resize): frame 1000x400000 exceeds the limits"},
		{"canvas width above 8192", h264, []recipe.Op{canvas(8193, 100, "")}, webp(), "op 0 (canvas): width and height must be <= 8192 (got 8193x100)"},
		{"canvas above 32 megapixels", h264, []recipe.Op{canvas(8192, 8192, "")}, webp(), "op 0 (canvas): frame 8192x8192 exceeds the limits (8192 px per side, 32 megapixels)"},
		{"output width above 8192", h264, nil, recipe.Output{Format: "webp", Width: 8193}, "output: width and height must be <= 8192 (got 8193x0)"},
		{"output height above 8192", h264, nil, recipe.Output{Format: "webp", Height: 10000, Fit: "cover"}, "output: width and height must be <= 8192 (got 0x10000)"},
		{"output fit exact above 32 megapixels", h264, nil, recipe.Output{Format: "webp", Width: 8192, Height: 4097, Fit: "exact"}, "output: frame 8192x4097 exceeds the limits (8192 px per side, 32 megapixels)"},
		{"oversized source without a resize", with(still, func(p *recipe.ProbeInfo) { p.Width, p.Height = 12000, 8000 }), nil, webp(), "output frame 12000x8000 exceeds the limits (8192 px per side, 32 megapixels); add a resize"},
		{"source wider than 8192 without a resize", with(h264, func(p *recipe.ProbeInfo) { p.Width, p.Height = 9000, 100 }), []recipe.Op{flip(true, false)}, gif(), "output frame 9000x100 exceeds the limits"},
		{"master above 8 GiB", with(h264, func(p *recipe.ProbeInfo) { p.Width, p.Height, p.Duration, p.Frames = 3840, 2160, 300, 9000 }), nil, webp(), "expected master (3840x2160 x 8991 frames = 277.8 GiB) exceeds the 8 GiB limit; trim, lower the fps or resize"},

		// Phase 2: sequences, delay op, alpha stream, output options.
		{"delay zero", pngSeq, []recipe.Op{delay(0)}, webp(), "op 0 (delay): ms must be between 1 and 60000 (got 0)"},
		{"delay negative", pngSeq, []recipe.Op{delay(-40)}, webp(), "op 0 (delay): ms must be between 1 and 60000 (got -40)"},
		{"delay above the maximum", pngSeq, []recipe.Op{delay(60001)}, gif(), "op 0 (delay): ms must be between 1 and 60000 (got 60001)"},
		{"delay missing params", pngSeq, []recipe.Op{op(recipe.OpDelay, nil)}, webp(), "op 0 (delay): ms must be between 1 and 60000 (got 0)"},
		{"delay invalid params json", pngSeq, []recipe.Op{rawOp(recipe.OpDelay, `{"ms":"slow"}`)}, webp(), "op 0 (delay): invalid params"},
		{"every delay op is validated, not just the winner", pngSeq, []recipe.Op{delay(40), delay(0)}, webp(), "op 1 (delay): ms must be between 1 and 60000 (got 0)"},
		{"sequence without sequence info", with(pngSeq, func(p *recipe.ProbeInfo) { p.Sequence = nil }), nil, webp(), "image sequence source has no sequence info"},
		{"sequence pattern without a frame number", withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Pattern = "frame.png" }), nil, webp(), `image sequence pattern "frame.png" is not an image2 pattern`},
		{"sequence with an empty pattern", withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Pattern = "" }), nil, webp(), `image sequence pattern "" is not an image2 pattern`},
		{"sequence with an alpha stream", with(pngSeq, func(p *recipe.ProbeInfo) { p.AlphaStream = 1 }), nil, webp(), "image sequence sources cannot carry a separate alpha stream"},
		{"sequence probed delay above the maximum", withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.DelayMS = 70000 }), nil, webp(), "sequence delay 70000 ms exceeds the 60000 ms maximum"},
		{"trim on a one-frame sequence", with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Count = 1 }), func(p *recipe.ProbeInfo) { p.Frames = 1 }), []recipe.Op{trim(0.05, 0)}, webp(), "op 0 (trim): the source has a single frame and cannot be trimmed"},
		{"trim start beyond the sequence end", pngSeq, []recipe.Op{trim(6, 0)}, webp(), "trim start (6 s) is at or beyond the end of the source (6 s)"},
		{"trim start beyond the retimed sequence end", pngSeq, []recipe.Op{delay(40), trim(3, 0)}, webp(), "trim start (3 s) is at or beyond the end of the source (2.4 s)"},
		{"negative alpha stream index", with(h264, func(p *recipe.ProbeInfo) { p.AlphaStream = -1 }), nil, webp(), "source alpha stream index must be >= 0 (got -1)"},
		{"unknown frame format", h264, nil, recipe.Output{Format: recipe.FormatFrames, FrameFormat: "gif"}, `output: frame format "gif" must be one of png, jpeg, webp`},
		{"frame format is case-sensitive", h264, nil, recipe.Output{Format: recipe.FormatFrames, FrameFormat: "PNG"}, `output: frame format "PNG" must be one of png, jpeg, webp`},
		{"frame format jpg is not an alias", h264, nil, recipe.Output{Format: recipe.FormatFrames, FrameFormat: "jpg"}, `output: frame format "jpg" must be one of png, jpeg, webp`},
		{"negative fit bytes", h264, nil, recipe.Output{Format: "gif", FitBytes: -1}, "output: fitBytes must be >= 0 (got -1)"},
		{"sequence master above 8 GiB", with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Count = 5000 }), func(p *recipe.ProbeInfo) { p.Width, p.Height, p.Frames = 8192, 4096, 5000 }), nil, webp(), "exceeds the 8 GiB limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(tc.src, tc.ops, tc.out)
			if err == nil {
				t.Fatalf("expected error containing %q, got plan %+v", tc.want, p)
			}
			if !strings.HasPrefix(err.Error(), "graph: ") {
				t.Errorf("error should be prefixed with \"graph: \": %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// --- SnapFPS ----------------------------------------------------------------

func TestSnapFPS(t *testing.T) {
	tests := []struct {
		format string
		fps    float64
		want   float64
	}{
		// GIF: a plain cap at 50, nothing else is snapped.
		{"gif", 60, 50},
		{"gif", 100, 50},
		{"gif", 50.001, 50},
		{"gif", 50, 50},
		{"gif", 40, 40},
		{"gif", 33.3333333, 33.333},
		{"gif", 30, 30},
		{"gif", 29.97002997, 29.97},
		{"gif", 25, 25},
		{"gif", 24, 24},
		{"gif", 23.976, 23.976},
		{"gif", 20, 20},
		{"gif", 16.7, 16.7},
		{"gif", 15, 15},
		{"gif", 12.5, 12.5},
		{"gif", 12, 12},
		{"gif", 10, 10},
		{"gif", 5, 5},
		{"gif", 1, 1},
		{"gif", 0.5, 0.5},
		{"gif", 0.0004, 0}, // below 3-decimal precision (Compile rejects it earlier via minFPS)
		{"gif", 0, 0},
		{"gif", -5, 0},
		{"gif", math.NaN(), 0},
		{"gif", math.Inf(1), 50},
		{"GIF", 60, 50},
		{" gif ", 60, 50},
		// Everything else: cap at 60.
		{"webp", 30, 30},
		{"webp", 29.97002997, 29.97},
		{"webp", 60, 60},
		{"webp", 90, 60},
		{"webp", 15, 15},
		{"webp", 0, 0},
		{"webp", -1, 0},
		{"webp", math.Inf(1), 60},
		{"apng", 120, 60},
		{"", 24, 24},
	}
	for _, tc := range tests {
		got := SnapFPS(tc.format, tc.fps)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("SnapFPS(%q, %v) = %v, want %v", tc.format, tc.fps, got, tc.want)
		}
	}
	if MaxGIFFPS != 50 || MaxFPS != 60 {
		t.Errorf("caps changed: MaxGIFFPS=%v MaxFPS=%v (DESIGN.md §4.1 says 50/60)", MaxGIFFPS, MaxFPS)
	}
}

// --- SequenceFPS ------------------------------------------------------------

func TestSequenceFPS(t *testing.T) {
	tests := []struct {
		delayMS int
		want    float64
	}{
		{100, 10},
		{40, 25},
		{50, 20},
		{33, 30.303},
		{3, 333.333},
		{1, 1000},
		{60000, 0.017},
		{16, 62.5},
		{0, 10},  // default 100 ms
		{-5, 10}, // default 100 ms
	}
	for _, tc := range tests {
		if got := SequenceFPS(tc.delayMS); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("SequenceFPS(%d) = %v, want %v", tc.delayMS, got, tc.want)
		}
	}
	if MinDelayMS != 1 || MaxDelayMS != 60000 || DefaultDelayMS != 100 {
		t.Errorf("delay bounds changed: %d..%d default %d (recipe.DelayParams says 1..60000, default 100)", MinDelayMS, MaxDelayMS, DefaultDelayMS)
	}
}

// gifDelays simulates ffmpeg's gif muxer for n frames at fps: every frame's
// pts is rounded to the nearest centisecond (1/100 s timebase, round to
// nearest) and each frame's delay is the distance to the next rounded pts.
func gifDelays(fps float64, n int) []int {
	pts := func(i int) int { return int(math.Round(100 * float64(i) / fps)) }
	delays := make([]int, n)
	for i := range delays {
		delays[i] = pts(i+1) - pts(i)
	}
	return delays
}

func TestSnapFPSGifIsAlwaysCentisecondDelay(t *testing.T) {
	// The property the 50 fps cap protects: every GIF rate SnapFPS returns
	// yields centisecond delays >= 2 (browsers clamp <= 1 cs to 10 cs) once
	// ffmpeg's gif muxer rounds the pts, the rounded delays add up to the
	// exact duration (no drift), and snapping is idempotent.
	for fps := 0.5; fps <= 200; fps += 0.25 {
		got := SnapFPS("gif", fps)
		if got <= 0 || got > MaxGIFFPS {
			t.Fatalf("SnapFPS(gif, %v) = %v", fps, got)
		}
		const frames = 600
		total := 0
		for i, d := range gifDelays(got, frames) {
			if d < 2 {
				t.Errorf("SnapFPS(gif, %v) = %v: frame %d has a %d cs delay (< 2 cs)", fps, got, i, d)
				break
			}
			total += d
		}
		if want := math.Round(100 * frames / got); math.Abs(float64(total)-want) > 1 {
			t.Errorf("SnapFPS(gif, %v) = %v: %d frames sum to %d cs, want %v", fps, got, frames, total, want)
		}
		if again := SnapFPS("gif", got); math.Abs(again-got) > 1e-9 {
			t.Errorf("SnapFPS(gif, %v) = %v but SnapFPS of that = %v (not idempotent)", fps, got, again)
		}
	}
	// And the counter-example the cap exists for: 60 fps would produce 1 cs
	// delays.
	short := 0
	for _, d := range gifDelays(60, 60) {
		if d < 2 {
			short++
		}
	}
	if short == 0 {
		t.Errorf("60 fps should yield some 1 cs delays (the reason MaxGIFFPS is 50)")
	}
	// The 30 fps case that motivated dropping the 100/n snap: 3,4,3 cs
	// delays, exact total, one delay per master frame.
	if got := gifDelays(30, 6); !reflect.DeepEqual(got, []int{3, 4, 3, 3, 4, 3}) {
		t.Errorf("30 fps delays = %v, want [3 4 3 3 4 3]", got)
	}
}

// --- number formatting ------------------------------------------------------

func TestFnum(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{-0.0001, "0"},
		{1, "1"},
		{1.5, "1.5"},
		{2.0, "2"},
		{0.1234, "0.123"},
		{0.1235, "0.124"},
		{33.33333, "33.333"},
		{100.0 / 3, "33.333"},
		{100.0 / 6, "16.667"},
		{100.0 / 7, "14.286"},
		{29.97002997, "29.97"},
		{1234567.891, "1234567.891"},
		{0.0000001, "0"},
	}
	for _, tc := range tests {
		if got := fnum(tc.in); got != tc.want {
			t.Errorf("fnum(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := fexact(1.0 / 3); got != "0.3333333333333333" {
		t.Errorf("fexact(1/3) = %q", got)
	}
	if got := fexact(1e-7); strings.ContainsAny(got, "eE") {
		t.Errorf("fexact must not use scientific notation: %q", got)
	}
}

// --- size math --------------------------------------------------------------

func TestScaledSize(t *testing.T) {
	tests := []struct {
		curW, curH, w, h int
		fit              string
		wantW, wantH     int
	}{
		// contain: largest size inside the box, aspect kept
		{1920, 1080, 128, 128, fitContain, 128, 72},
		{1080, 1920, 128, 128, fitContain, 72, 128},
		{1000, 1000, 128, 64, fitContain, 64, 64},
		{800, 600, 400, 400, fitContain, 400, 300},
		{3, 2, 5, 5, fitContain, 5, 3},
		// cover: smallest size covering the box, aspect kept
		{1920, 1080, 128, 128, fitCover, 228, 128},
		{1080, 1920, 128, 128, fitCover, 128, 228},
		{800, 600, 400, 400, fitCover, 533, 400},
		{3, 2, 5, 5, fitCover, 8, 5},
		// exact
		{1920, 1080, 128, 128, fitExact, 128, 128},
		// one dimension only (fit ignored)
		{1920, 1080, 480, 0, fitCover, 480, 270},
		{1920, 1080, 0, 270, fitExact, 480, 270},
		// never below 1x1
		{4000, 1, 2, 0, fitContain, 2, 1},
		{1, 4000, 0, 2, fitContain, 1, 2},
	}
	for _, tc := range tests {
		gw, gh := scaledSize(tc.curW, tc.curH, tc.w, tc.h, tc.fit)
		if gw != tc.wantW || gh != tc.wantH {
			t.Errorf("scaledSize(%dx%d → %dx%d %s) = %dx%d, want %dx%d",
				tc.curW, tc.curH, tc.w, tc.h, tc.fit, gw, gh, tc.wantW, tc.wantH)
		}
	}
}

func TestPadColor(t *testing.T) {
	tests := []struct {
		in          string
		want        string
		transparent bool
		wantErr     bool
	}{
		{"", "0x00000000", true, false},
		{"  ", "0x00000000", true, false},
		{"#313338", "0x313338", false, false},
		{"FFFFFF", "0xffffff", false, false},
		{"313338ff", "0x313338ff", false, false},
		{"31333880", "0x31333880", true, false},
		{"00000000", "0x00000000", true, false},
		{"red", "", false, true},
		{"#12345", "", false, true},
	}
	for _, tc := range tests {
		got, tr, err := padColor(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("padColor(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want || tr != tc.transparent {
			t.Errorf("padColor(%q) = %q,%v want %q,%v", tc.in, got, tr, tc.want, tc.transparent)
		}
	}
}
