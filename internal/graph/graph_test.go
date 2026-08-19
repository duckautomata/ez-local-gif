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
)

// with returns a copy of src with fn applied.
func with(src recipe.ProbeInfo, fn func(*recipe.ProbeInfo)) recipe.ProbeInfo {
	fn(&src)
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

func webp() recipe.Output { return recipe.Output{Format: "webp"} }
func gif() recipe.Output  { return recipe.Output{Format: "gif"} }

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
				Filter: "[0:v]fps=30,format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "gif source to gif keeps source fps",
			src:  gifSrc, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20,format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "trim + fps 30 for gif keeps 30 (no 100/n snapping)",
			src:  h264, ops: []recipe.Op{trim(1.5, 4), fps(30)}, out: gif(),
			want: Plan{
				InputArgs: []string{"-ss", "1.5", "-to", "4"},
				Filter:    "[0:v]fps=30,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 30, Duration: 2.5, Frames: 75,
				TrimStart: 1.5, TrimEnd: 4,
			},
		},
		{
			name: "speed is emitted before fps even when listed after it",
			src:  h264, ops: []recipe.Op{fps(25), speed(2)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/2,fps=25,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 25, Duration: 5, Frames: 125, Speed: 2,
			},
		},
		{
			name: "slow motion doubles duration and keeps full factor precision",
			src:  h264, ops: []recipe.Op{speed(0.5)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/0.5,fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 20, Frames: 599, Speed: 0.5,
			},
		},
		{
			name: "multiple speed ops multiply",
			src:  h264, ops: []recipe.Op{speed(2), speed(1.5)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/3,fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10.0 / 3, Frames: 100, Speed: 3,
			},
		},
		{
			name: "crop then resize contain with alpha uses the premultiplied scale chain",
			src:  prores, ops: []recipe.Op{crop(100, 50, 800, 600), resize(400, 400, "contain")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=30,crop=800:600:100:50:exact=1,format=gbrap,premultiply=inplace=1,scale=400:300:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  400, Height: 300, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "resize contain default fit when fit is empty",
			src:  h264, ops: []recipe.Op{resize(400, 400, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=400:225:flags=lanczos,format=rgba[out]",
				Width:  400, Height: 225, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "resize cover scales to cover then centre-crops",
			src:  h264, ops: []recipe.Op{resize(400, 400, "cover")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=711:400:flags=lanczos,crop=400:400:155:0:exact=1,format=rgba[out]",
				Width:  400, Height: 400, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "resize exact stretches",
			src:  h264, ops: []recipe.Op{resize(400, 400, "exact")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=400:400:flags=lanczos,format=rgba[out]",
				Width:  400, Height: 400, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "resize width only keeps aspect",
			src:  h264, ops: []recipe.Op{resize(640, 0, "cover")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=640:360:flags=lanczos,format=rgba[out]",
				Width:  640, Height: 360, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "resize height only keeps aspect (alpha chain)",
			src:  prores, ops: []recipe.Op{resize(0, 540, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=30,format=gbrap,premultiply=inplace=1,scale=960:540:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  960, Height: 540, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "resize to the current size is a no-op",
			src:  h264, ops: []recipe.Op{resize(1280, 720, "exact")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "canvas pads transparent and marks alpha",
			src:  h264, ops: []recipe.Op{canvas(1280, 1280, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba,pad=1280:1280:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  1280, Height: 1280, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 300,
			},
		},
		{
			name: "canvas crops the larger dimension before padding",
			src:  h264, ops: []recipe.Op{canvas(1000, 1000, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,crop=1000:720:140:0:exact=1,format=rgba,pad=1000:1000:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  1000, Height: 1000, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 300,
			},
		},
		{
			name: "canvas smaller in both dimensions only crops",
			src:  h264, ops: []recipe.Op{canvas(640, 640, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,crop=640:640:320:40:exact=1,format=rgba[out]",
				Width:  640, Height: 640, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "canvas with opaque colour does not add alpha (gif keeps the NTSC rate)",
			src:  h264, ops: []recipe.Op{canvas(1280, 1280, "#313338")}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba,pad=1280:1280:(ow-iw)/2:(oh-ih)/2:color=0x313338,format=rgba[out]",
				Width:  1280, Height: 1280, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "canvas with semi-transparent RRGGBBAA colour adds alpha",
			src:  h264, ops: []recipe.Op{canvas(1280, 1280, "FF000080")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba,pad=1280:1280:(ow-iw)/2:(oh-ih)/2:color=0xff000080,format=rgba[out]",
				Width:  1280, Height: 1280, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 300,
			},
		},
		{
			name: "canvas equal to the frame is a no-op",
			src:  h264, ops: []recipe.Op{canvas(1280, 720, "")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "rotate 90 swaps dimensions",
			src:  h264, ops: []recipe.Op{rotate(90)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,transpose=1,format=rgba[out]",
				Width:  720, Height: 1280, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "rotate 180 is hflip+vflip",
			src:  h264, ops: []recipe.Op{rotate(180)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,hflip,vflip,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "rotate 270 swaps dimensions",
			src:  h264, ops: []recipe.Op{rotate(270)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,transpose=2,format=rgba[out]",
				Width:  720, Height: 1280, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "flip horizontal and vertical",
			src:  h264, ops: []recipe.Op{flip(true, true)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,hflip,vflip,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "flip vertical only",
			src:  h264, ops: []recipe.Op{flip(false, true)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,vflip,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "flip with neither direction is a no-op",
			src:  h264, ops: []recipe.Op{flip(false, false)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "geometry ops apply in order: crop after rotate uses the rotated frame",
			src:  h264, ops: []recipe.Op{rotate(90), crop(0, 0, 720, 1000)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,transpose=1,crop=720:1000:0:0:exact=1,format=rgba[out]",
				Width:  720, Height: 1000, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "geometry ops apply in order: crop after resize uses the scaled frame",
			src:  h264, ops: []recipe.Op{resize(640, 0, ""), crop(0, 0, 640, 200)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=640:360:flags=lanczos,crop=640:200:0:0:exact=1,format=rgba[out]",
				Width:  640, Height: 200, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "output fit contain 128x128 (emote): alpha scale + transparent pad",
			src:  prores, out: recipe.Output{Format: "gif", Width: 128, Height: 128, FPS: 25},
			want: Plan{
				Filter: "[0:v]fps=25,format=gbrap,premultiply=inplace=1,scale=128:72:flags=lanczos,unpremultiply=inplace=1,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  128, Height: 128, FPS: 25, HasAlpha: true, Duration: 4, Frames: 100,
			},
		},
		{
			name: "output fit contain on an opaque source introduces alpha",
			src:  h264, out: recipe.Output{Format: "webp", Width: 128, Height: 128},
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=128:72:flags=lanczos,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba[out]",
				Width:  128, Height: 128, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 300,
			},
		},
		{
			name: "output fit with only width scales keeping aspect, no pad",
			src:  h264, out: recipe.Output{Format: "webp", Width: 480},
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=480:270:flags=lanczos,format=rgba[out]",
				Width:  480, Height: 270, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "output fit with only height scales keeping aspect",
			src:  h264, out: recipe.Output{Format: "webp", Height: 360, Fit: "contain"},
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=640:360:flags=lanczos,format=rgba[out]",
				Width:  640, Height: 360, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "output fit cover 320x320 (sticker) centre-crops",
			src:  h264, out: recipe.Output{Format: "gif", Width: 320, Height: 320, Fit: "cover", FPS: 20},
			want: Plan{
				Filter: "[0:v]fps=20,scale=569:320:flags=lanczos,crop=320:320:124:0:exact=1,format=rgba[out]",
				Width:  320, Height: 320, FPS: 20, Duration: 10, Frames: 200,
			},
		},
		{
			name: "output fit exact stretches",
			src:  h264, out: recipe.Output{Format: "webp", Width: 300, Height: 300, Fit: "exact"},
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=300:300:flags=lanczos,format=rgba[out]",
				Width:  300, Height: 300, FPS: 29.97, Duration: 10, Frames: 300,
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
				Filter: "[0:v]fps=30,crop=1080:1080:0:0:exact=1,format=gbrap,premultiply=inplace=1,scale=128:128:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  128, Height: 128, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "unpremultiply is hoisted first at 10-bit",
			src:  prores, ops: []recipe.Op{crop(0, 0, 960, 1080), unpremultiply()}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=gbrap10le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,fps=30,crop=960:1080:0:0:exact=1,format=rgba[out]",
				Width:  960, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "unpremultiply at 12-bit (ProRes 4444 XQ)",
			src:  with(prores, func(p *recipe.ProbeInfo) { p.PixFmt, p.Bits = "yuva444p12le", 12 }),
			ops:  []recipe.Op{unpremultiply(), speed(2)}, out: gif(),
			want: Plan{
				Filter: "[0:v]format=gbrap12le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,setpts=PTS/2,fps=30,format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60, Speed: 2,
			},
		},
		{
			name: "unpremultiply at 8-bit uses gbrap",
			src:  gifSrc, ops: []recipe.Op{unpremultiply()}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,fps=20,format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "unpremultiply on a source without alpha is a no-op",
			src:  h264, ops: []recipe.Op{unpremultiply()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "vp9 alpha forces libvpx-vp9",
			src:  vp9, out: webp(),
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx-vp9"},
				Filter:    "[0:v]fps=30,format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60,
			},
		},
		{
			name: "vp9 alpha decoder args precede the trim seek args",
			src:  vp9, ops: []recipe.Op{trim(1, 0)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx-vp9", "-ss", "1"},
				Filter:    "[0:v]fps=30,format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 1, Frames: 30,
				TrimStart: 1,
			},
		},
		{
			name: "vp8 alpha forces libvpx",
			src:  with(vp9, func(p *recipe.ProbeInfo) { p.Codec = "vp8" }), out: webp(),
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx"},
				Filter:    "[0:v]fps=30,format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60,
			},
		},
		{
			name: "vp9 without alpha uses the native decoder",
			src:  with(vp9, func(p *recipe.ProbeInfo) { p.HasAlpha = false }), out: webp(),
			want: Plan{
				Filter: "[0:v]fps=30,format=rgba[out]",
				Width:  640, Height: 360, FPS: 30, Duration: 2, Frames: 60,
			},
		},
		{
			name: "trim start only",
			src:  prores, ops: []recipe.Op{trim(1, 0)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1"},
				Filter:    "[0:v]fps=30,format=rgba[out]",
				Width:     1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 3, Frames: 90,
				TrimStart: 1,
			},
		},
		{
			name: "trim end only",
			src:  prores, ops: []recipe.Op{trim(0, 2.5)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-to", "2.5"},
				Filter:    "[0:v]fps=30,format=rgba[out]",
				Width:     1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 2.5, Frames: 75,
				TrimEnd: 2.5,
			},
		},
		{
			name: "trim end at or beyond the source end reads to the end",
			src:  h264, ops: []recipe.Op{trim(2, 99)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "2"},
				Filter:    "[0:v]fps=29.97,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 8, Frames: 240,
				TrimStart: 2,
			},
		},
		{
			name: "trim times are rounded to milliseconds",
			src:  h264, ops: []recipe.Op{trim(0.12345, 1.98765)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "0.123", "-to", "1.988"},
				Filter:    "[0:v]fps=29.97,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 1.865, Frames: 56,
				TrimStart: 0.123, TrimEnd: 1.988,
			},
		},
		{
			name: "trim end <= 0 means to the end",
			src:  h264, ops: []recipe.Op{trim(1, -1)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1"},
				Filter:    "[0:v]fps=29.97,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 9, Frames: 270,
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
				Filter:    "[0:v]fps=29.97,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 3, Frames: 90,
				TrimStart: 2, TrimEnd: 5,
			},
		},
		{
			name: "trim + speed: duration is trimmed length over speed",
			src:  h264, ops: []recipe.Op{trim(2, 6), speed(4)}, out: gif(),
			want: Plan{
				InputArgs: []string{"-ss", "2", "-to", "6"},
				Filter:    "[0:v]setpts=PTS/4,fps=29.97,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 1, Frames: 30,
				TrimStart: 2, TrimEnd: 6, Speed: 4,
			},
		},
		{
			name: "last fps op wins",
			src:  h264, ops: []recipe.Op{fps(10), fps(20)}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 20, Duration: 10, Frames: 200,
			},
		},
		{
			name: "fps op beats output fps",
			src:  h264, ops: []recipe.Op{fps(10)}, out: recipe.Output{Format: "webp", FPS: 25},
			want: Plan{
				Filter: "[0:v]fps=10,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 10, Duration: 10, Frames: 100,
			},
		},
		{
			name: "output fps is used as-is for gif (24 stays 24)",
			src:  h264, out: recipe.Output{Format: "gif", FPS: 24},
			want: Plan{
				Filter: "[0:v]fps=24,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 24, Duration: 10, Frames: 240,
			},
		},
		{
			name: "output fps above the gif cap is capped at 50",
			src:  h264, out: recipe.Output{Format: "gif", FPS: 60},
			want: Plan{
				Filter: "[0:v]fps=50,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 50, Duration: 10, Frames: 500,
			},
		},
		{
			name: "fractional fps for gif is rounded to 3 decimals, not snapped",
			src:  h264, ops: []recipe.Op{fps(100.0 / 3)}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=33.333,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 33.333, Duration: 10, Frames: 333,
			},
		},
		{
			name: "source fps is capped at 60 for webp",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.FPS = 120 }), out: webp(),
			want: Plan{
				Filter: "[0:v]fps=60,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 60, Duration: 10, Frames: 600,
			},
		},
		{
			name: "source fps is capped at 50 for gif",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.FPS = 120 }), out: gif(),
			want: Plan{
				Filter: "[0:v]fps=50,format=rgba[out]",
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
				Filter: "[0:v]fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97,
			},
		},
		{
			name: "duration falls back to frames/fps when only those are known",
			src:  with(gifSrc, func(p *recipe.ProbeInfo) { p.Duration = 0 }), out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20,format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "op params may be null",
			src:  h264, ops: []recipe.Op{rawOp(recipe.OpFlip, "null"), rawOp(recipe.OpUnpremultiply, `{"ignored":true}`)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 300,
			},
		},
		{
			name: "limits: 8192x4096 (exactly 32 megapixels) is accepted",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 1, 30 }),
			ops:  []recipe.Op{resize(8192, 4096, "exact")}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97,scale=8192:4096:flags=lanczos,format=rgba[out]",
				Width:  8192, Height: 4096, FPS: 29.97, Duration: 1, Frames: 30,
			},
		},
		{
			name: "limits: speed 0.05 (the minimum) is accepted",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.Width, p.Height = 320, 180 }),
			ops:  []recipe.Op{speed(0.05)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/0.05,fps=29.97,format=rgba[out]",
				Width:  320, Height: 180, FPS: 29.97, Duration: 200, Frames: 5994, Speed: 0.05,
			},
		},
		{
			name: "limits: speed 100 (the maximum) is accepted",
			src:  h264, ops: []recipe.Op{speed(100)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setpts=PTS/100,fps=29.97,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 0.1, Frames: 3, Speed: 100,
			},
		},
		{
			name: "limits: a master of exactly 8 GiB is accepted",
			src:  with(h264, func(p *recipe.ProbeInfo) { p.Width, p.Height, p.FPS, p.Duration, p.Frames = 2048, 1024, 1, 1024, 1024 }),
			out:  webp(),
			want: Plan{
				Filter: "[0:v]fps=1,format=rgba[out]",
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
				Filter:    "[0:v]format=gbrap10le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,setpts=PTS/1.5,fps=25,crop=1080:1080:420:0:exact=1,format=gbrap,premultiply=inplace=1,scale=128:128:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:     128, Height: 128, FPS: 25, HasAlpha: true, Duration: 2, Frames: 50,
				TrimStart: 0.5, TrimEnd: 3.5, Speed: 1.5,
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
			// SourceFPS is the probed rate verbatim (0 for stills); the cases
			// above leave it implied. TestSourceFPS covers the rule itself.
			want.SourceFPS = tc.src.FPS
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
	if !strings.HasPrefix(got.Filter, "[0:v]") || !strings.HasSuffix(got.Filter, got.OutLabel) {
		t.Errorf("Filter must start with [0:v] and end with %s: %s", got.OutLabel, got.Filter)
	}
	if strings.Contains(got.Filter, ";") {
		t.Errorf("Filter must be a single chain: %s", got.Filter)
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
