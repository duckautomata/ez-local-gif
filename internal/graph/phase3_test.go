package graph

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// --- Phase 3 fixtures ---------------------------------------------------------

var (
	// Overlay assets, as the probe reports them.
	ovPNG = recipe.ProbeInfo{
		Format: "png_pipe", Codec: "png", PixFmt: "rgba", Bits: 8,
		Width: 64, Height: 32, Frames: 1, HasAlpha: true, IsStill: true, Kind: recipe.KindImage,
	}
	ovJPEG = recipe.ProbeInfo{
		Format: "jpeg_pipe", Codec: "mjpeg", PixFmt: "yuvj420p", Bits: 8,
		Width: 100, Height: 50, Frames: 1, IsStill: true, Kind: recipe.KindImage,
	}
	ovGIF = recipe.ProbeInfo{
		Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8,
		Width: 32, Height: 32, FPS: 10, Duration: 0.2, Frames: 2, HasAlpha: true, Kind: recipe.KindAnimation,
	}
	ovGIFStill = recipe.ProbeInfo{
		Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8,
		Width: 32, Height: 32, Frames: 1, HasAlpha: true, IsStill: true, Kind: recipe.KindImage,
	}
	ovWebP = recipe.ProbeInfo{
		Format: "webp_anim", Codec: "webp_anim", PixFmt: "argb", Bits: 8,
		Width: 32, Height: 32, FPS: 10, Duration: 1, Frames: 10, HasAlpha: true, Kind: recipe.KindAnimation,
	}
	ovAPNG = recipe.ProbeInfo{
		Format: "apng", Codec: "apng", PixFmt: "rgba", Bits: 8,
		Width: 40, Height: 40, FPS: 12, Duration: 4, Frames: 48, HasAlpha: true, Kind: recipe.KindAnimation,
	}
	ovMP4 = recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", PixFmt: "yuv420p", Bits: 8,
		Width: 320, Height: 180, FPS: 25, Duration: 2, Frames: 50, Kind: recipe.KindVideo,
	}
	ovProRes = recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p10le", Bits: 10,
		Width: 200, Height: 100, FPS: 30, Duration: 1, Frames: 30, HasAlpha: true, Kind: recipe.KindVideo, Premultiplied: true,
	}
	// Animated AVIF with alpha: the mov demuxer lists the primary item first,
	// the animation's colour track is v:2 and its alpha v:3.
	ovAVIFAnim = recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "av1", PixFmt: "yuv420p", Bits: 8,
		Width: 64, Height: 64, FPS: 10, Duration: 2, Frames: 20, HasAlpha: true, Kind: recipe.KindAnimation,
		ColorStream: 2, AlphaStream: 3,
	}
)

func chromakey(p recipe.ChromaKeyParams) recipe.Op { return op(recipe.OpChromaKey, p) }
func colorkey(p recipe.ColorKeyParams) recipe.Op   { return op(recipe.OpColorKey, p) }
func feather(r float64) recipe.Op                  { return op(recipe.OpFeather, recipe.FeatherParams{Radius: r}) }
func autocrop(p recipe.AutoCropParams) recipe.Op   { return op(recipe.OpAutoCrop, p) }
func reverse() recipe.Op                           { return op(recipe.OpReverse, nil) }
func text(p recipe.TextParams) recipe.Op           { return op(recipe.OpText, p) }
func overlay(p recipe.OverlayParams) recipe.Op     { return op(recipe.OpOverlay, p) }

// resolved is an autocrop op whose detection pass found x,y,w,h.
func resolved(x, y, w, h int) recipe.Op {
	return autocrop(recipe.AutoCropParams{Threshold: 8, Padding: 4, Resolved: &recipe.CropParams{X: x, Y: y, W: w, H: h}})
}

// Stage text shared by the goldens.
const (
	ckDefault = "format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05,despill=type=green:mix=0.6:expand=0.3"
	ckNoSpill = "format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05"
	ckWhite   = "format=rgba,colorkey=color=0xffffff:similarity=0.1:blend=0"
	// featherDef is the feather stage at the default radius 3: gbrap orders
	// the planes G,B,R,A, so planes=8 blurs only the alpha plane.
	featherDef = "format=gbrap,gblur=sigma=3:planes=8,format=rgba"
	ovComposit = "overlay=x=0:y=0:format=auto:shortest=1:eof_action=repeat"
	hold       = "tpad=stop_mode=clone:stop=-1"
	text1      = "drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=0:y=0"
	text2      = "drawtext=textfile=__EZLG_TEXT_2__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=0:y=0"
	// layerTail follows every text layer's drawtext (textLayerTail): the
	// layer's anti-aliased edges leave drawtext premultiplied and are made
	// straight before the mixer and the composite.
	layerTail = ",format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba"
)

// keyWrapped is the alpha-keeping wrapper keyKeepingAlpha emits for the
// n-th key applied to frames that already carry alpha (stages = the key's
// own stages, ending in format=rgba): it joins the chain in place of the
// bare stages — "<chain so far>," + keyWrapped(…) + ",<rest>" — closing the
// chain into a split, keying one copy, multiplying the two mattes and
// merging the product back as the keyed frame's alpha. The first golden in
// TestCompilePhase3 that uses it spells the text out in full.
func keyWrapped(n int, stages string) string {
	k := strconv.Itoa(n)
	return "split[k" + k + "][k" + k + "m];" +
		"[k" + k + "m]alphaextract[k" + k + "a0];" +
		"[k" + k + "]" + stages + ",split[k" + k + "k][k" + k + "km];" +
		"[k" + k + "km]alphaextract[k" + k + "a1];" +
		"[k" + k + "a0][k" + k + "a1]blend=all_mode=multiply[k" + k + "a];" +
		"[k" + k + "k][k" + k + "a]alphamerge"
}

// --- goldens ----------------------------------------------------------------

func TestCompilePhase3(t *testing.T) {
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want Plan // OutLabel implied; Speed 0 means 1; SourceFPS 0 means the main source's
	}{
		// --- keying ---------------------------------------------------------
		{
			name: "chromakey defaults: yuva444p, green screen, despill green",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckDefault + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "chromakey blue screen with custom knobs despills blue",
			srcs: []recipe.ProbeInfo{h264},
			ops:  []recipe.Op{chromakey(recipe.ChromaKeyParams{Color: "#0000FF", Similarity: 0.3, Blend: 0.1, DespillMix: 0.5, DespillExpand: 0.2})},
			out:  webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=yuva444p,chromakey=color=0x0000ff:similarity=0.3:blend=0.1,despill=type=blue:mix=0.5:expand=0.2:green=0:blue=-1,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "chromakey with despill off",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{DespillOff: true})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "chromakey on a colour that is neither green- nor blue-dominant skips despill",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{Color: "ff00ff"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=yuva444p,chromakey=color=0xff00ff:similarity=0.2:blend=0.05,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "colorkey: rgba, eyedropper colour, default similarity 0.1 blend 0",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "313338"})}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,colorkey=color=0x313338:similarity=0.1:blend=0,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "colorkey with knobs",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "#FFFFFF", Similarity: 0.25, Blend: 0.125})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,colorkey=color=0xffffff:similarity=0.25:blend=0.125,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "keying runs at full resolution before any geometry op, whatever the op order",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{crop(0, 0, 640, 360), resize(320, 0, ""), chromakey(recipe.ChromaKeyParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckDefault + ",crop=640:360:0:0:exact=1,format=gbrap,premultiply=inplace=1,scale=320:180:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]",
				Width:  320, Height: 180, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			// The source carries alpha, so the key must not overwrite it:
			// the chain is split, the key runs on one copy and its matte is
			// multiplied with the incoming alpha (keyKeepingAlpha). Spelled
			// out in full here; the other goldens use keyWrapped.
			name: "keying an alpha source follows the hoisted unpremultiply and the temporal stages and keeps the source alpha",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{}), unpremultiply(), speed(2)}, out: webp(),
			want: Plan{
				Filter: "[0:v]setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba,setpts=PTS/2,fps=30:round=down,split[k1][k1m];" +
					"[k1m]alphaextract[k1a0];" +
					"[k1]format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05,despill=type=green:mix=0.6:expand=0.3,format=rgba,split[k1k][k1km];" +
					"[k1km]alphaextract[k1a1];" +
					"[k1a0][k1a1]blend=all_mode=multiply[k1a];" +
					"[k1k][k1a]alphamerge,format=rgba[out]",
				Width: 1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60, Speed: 2,
			},
		},
		{
			name: "two keys apply in order: the second sees the first's alpha and is wrapped",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"}), chromakey(recipe.ChromaKeyParams{DespillOff: true})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckWhite + "," + keyWrapped(1, ckNoSpill+",format=rgba") + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "colorkey on a transparent gif keeps the gif's alpha",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "313338"})}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down," + keyWrapped(1, "format=rgba,colorkey=color=0x313338:similarity=0.1:blend=0") + ",format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "two wrapped keys on an alpha source number their labels",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{}), colorkey(recipe.ColorKeyParams{Color: "ffffff"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down," + keyWrapped(1, ckDefault+",format=rgba") + "," + keyWrapped(2, ckWhite) + ",format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			name: "key on a transparent still: the split joins a bare [0:v] (no fps stage)",
			srcs: []recipe.ProbeInfo{still}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"})}, out: recipe.Output{Format: recipe.FormatPNG},
			want: Plan{
				Filter: "[0:v]" + keyWrapped(1, ckWhite) + ",format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "key on an alpha-stream source follows the merge head",
			srcs: []recipe.ProbeInfo{avifAlpha}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{})}, out: webp(),
			want: Plan{
				Filter: alphaHead1 + keyWrapped(1, ckDefault+",format=rgba") + ",format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "key on an opaque mixed-size sequence is wrapped: the normalising pad added transparent alpha",
			srcs: []recipe.ProbeInfo{with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true }), func(p *recipe.ProbeInfo) { p.HasAlpha = false })},
			ops:  []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"})}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "fps=10:round=down," + keyWrapped(1, ckWhite) + ",format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "wrapped key, then geometry, text and an overlay continue from the merged chain",
			srcs: []recipe.ProbeInfo{gifSrc, ovPNG},
			ops:  []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"}), crop(0, 0, 240, 270), text(recipe.TextParams{Text: "Hi"}), overlay(recipe.OverlayParams{Source: 1})},
			out:  webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down," + keyWrapped(1, ckWhite) + ",crop=240:270:0:0:exact=1,format=rgba," + text1 + "[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  240, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
				TextFiles:   []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hi"}},
			},
		},

		// --- feather --------------------------------------------------------
		{
			// gbrap orders the planes G,B,R,A, so planes=8 blurs only the
			// alpha plane; the stage follows the key wrapper it softens.
			name: "feather at the default radius follows a wrapped key on a prores source",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{}), feather(0)}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba,fps=30:round=down," + keyWrapped(1, ckDefault+",format=rgba") + "," + featherDef + "[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "colorkey then feather with an explicit radius",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"}), feather(1.5)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckWhite + ",format=gbrap,gblur=sigma=1.5:planes=8,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "feather on an alpha source without a key blurs the source alpha (radius 0 = the default 3)",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{feather(0)}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down," + featherDef + "[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			// Blurring a constant opaque plane is a no-op that would waste
			// two conversions, so the stage is dropped entirely.
			name: "feather on an opaque source without keys is skipped entirely",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{feather(5)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "feather ops interleave with the keys in stack order on an alpha source",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{feather(2), colorkey(recipe.ColorKeyParams{Color: "313338"}), feather(4)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down,format=gbrap,gblur=sigma=2:planes=8,format=rgba," +
					keyWrapped(1, "format=rgba,colorkey=color=0x313338:similarity=0.1:blend=0") +
					",format=gbrap,gblur=sigma=4:planes=8,format=rgba[out]",
				Width: 480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},
		{
			// The frame in front of the key is still opaque, so the first
			// feather has nothing to blur; the one after the key softens its
			// matte.
			name: "on an opaque source a feather before the key is skipped, one after it applies",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{feather(2), chromakey(recipe.ChromaKeyParams{DespillOff: true}), feather(2)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckNoSpill + ",format=gbrap,gblur=sigma=2:planes=8,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			// The radius is in SOURCE pixels: the stage precedes every
			// geometry op wherever it sits in the stack, so the softness
			// scales with the image.
			name: "feather is hoisted in front of the geometry wherever it sits in the stack",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{crop(0, 0, 240, 270), feather(1)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down,format=gbrap,gblur=sigma=1:planes=8,format=rgba,crop=240:270:0:0:exact=1,format=rgba[out]",
				Width:  240, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
			},
		},

		// --- autocrop -------------------------------------------------------
		{
			name: "resolved autocrop is a crop at its position",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{resolved(100, 50, 800, 600)}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba,fps=30:round=down,crop=800:600:100:50:exact=1,format=rgba[out]",
				Width:  800, Height: 600, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "autocrop resolved to the full frame emits nothing",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{resolved(0, 0, 1280, 720)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "autocrop sits in the geometry order like a crop",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{rotate(90), resolved(0, 0, 720, 1000)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,transpose=1,crop=720:1000:0:0:exact=1,format=rgba[out]",
				Width:  720, Height: 1000, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},

		// --- reverse --------------------------------------------------------
		{
			// The reverse filter holds every frame until EOF, so it runs after
			// the output fit (the frames it buffers are the output-sized ones
			// jobs' admitReversed / Options.MaxMasterBytes bounds), still in
			// front of the final-canvas ops, and always behind a format=rgba
			// that pins its buffer to 4 B/px.
			name: "reverse after the geometry and the output fit, on rgba; frames and duration unchanged",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{crop(0, 0, 1080, 1080), reverse()}, out: recipe.Output{Format: "webp", Width: 128, Height: 128},
			want: Plan{
				Filter: "[0:v]format=rgba,fps=30:round=down,crop=1080:1080:0:0:exact=1,format=gbrap,premultiply=inplace=1,scale=128:128:flags=lanczos,unpremultiply=inplace=1,format=rgba,reverse,format=rgba[out]",
				Width:  128, Height: 128, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
				Reversed: true,
			},
		},
		{
			name: "reverse sits after the contain fit's pad and before the text",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{reverse(), text(recipe.TextParams{Text: "GG"})}, out: recipe.Output{Format: "gif", Width: 128, Height: 128},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=128:72:flags=lanczos,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba,reverse,format=rgba," + text1 + ",format=rgba[out]",
				Width:  128, Height: 128, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				Reversed:  true,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "GG"}},
			},
		},
		{
			name: "reverse with trim and speed: trim stays a source-time seek; an opaque yuv420p clip is forced to rgba for the buffer",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{reverse(), trim(1, 3), speed(2)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1", "-to", "3"},
				Filter:    "[0:v]setpts=PTS/2,fps=29.97:round=down,format=rgba,reverse,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 1, Frames: 29,
				TrimStart: 1, TrimEnd: 3, Speed: 2, Reversed: true,
			},
		},
		{
			name: "reverse on a trimmed webp_anim: the trim stage stays first, the reverse follows the fps stage",
			srcs: []recipe.ProbeInfo{webpAnim}, ops: []recipe.Op{reverse(), trim(0.5, 1.5)}, out: webp(),
			want: Plan{
				Filter: "[0:v]trim=start=0.5:end=1.5,setpts=PTS-STARTPTS,fps=10:round=down,format=rgba,reverse,format=rgba[out]",
				Width:  64, Height: 48, FPS: 10, HasAlpha: true, Duration: 1, Frames: 10,
				TrimStart: 0.5, TrimEnd: 1.5, FilterTrim: true, Reversed: true,
			},
		},
		{
			name: "two reverse ops cancel out",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{reverse(), flip(true, false), reverse()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,hflip,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "reverse on a still is a no-op",
			srcs: []recipe.ProbeInfo{still}, ops: []recipe.Op{reverse()}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "reverse on an image sequence",
			srcs: []recipe.ProbeInfo{pngSeq}, ops: []recipe.Op{reverse()}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=rgba,reverse,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10, Reversed: true,
			},
		},

		// --- text -----------------------------------------------------------
		{
			name: "text defaults: DejaVu Sans 32 px white, top-left, whole clip",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{text(recipe.TextParams{Text: "Hi"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba," + text1 + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hi"}},
			},
		},
		{
			name: "text with every opaque option in place: border, box, spacing, anchor, time range",
			srcs: []recipe.ProbeInfo{h264},
			ops: []recipe.Op{text(recipe.TextParams{
				Text: "Hello\nWorld", Font: "DejaVu Sans Mono", Size: 48, Color: "#FF0000",
				Border: 3, BorderColor: "000000", Box: true, BoxColor: "FFFFFFFF", BoxPad: 12,
				X: 640, Y: 700, Anchor: "bc", Start: 0.5, End: 2, LineSpacing: -4,
			})},
			out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans Mono:fontsize=48:fontcolor=0xff0000:x=640-text_w/2:y=700-text_h:borderw=3:bordercolor=0x000000:box=1:boxcolor=0xffffffff:boxborderw=12:line_spacing=-4:enable='gte(t+0.0001,0.5)*lt(t+0.0001,2)',format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hello\nWorld"}},
			},
		},
		{
			// drawtext blends an RRGGBBAA colour into the canvas' alpha plane
			// too, so translucent elements are drawn on transparent layers —
			// one per group of adjacent elements sharing an alpha, in draw
			// order box → border → glyphs — and composited like image overlays
			// (textLayers). The enable window moves to the composites; every
			// layer repeats the op's textfile placeholder; elements not in a
			// layer are suppressed (no box, no borderw, fontcolor 0x00000000).
			name: "text with translucent box and glyphs and an opaque border: three layers",
			srcs: []recipe.ProbeInfo{h264},
			ops: []recipe.Op{text(recipe.TextParams{
				Text: "Hello\nWorld", Font: "DejaVu Sans Mono", Size: 48, Color: "#FF000080",
				Border: 3, BorderColor: "000000", Box: true, BoxColor: "FFFFFF40", BoxPad: 12,
				X: 640, Y: 700, Anchor: "bc", Start: 0.5, End: 2, LineSpacing: -4,
			})},
			out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans Mono:fontsize=48:fontcolor=0x00000000:x=640-text_w/2:y=700-text_h:box=1:boxcolor=0xffffff:boxborderw=12:line_spacing=-4" + layerTail + ",colorchannelmixer=aa=0.251[t1];" +
					"[b1][t1]overlay=format=auto:shortest=1:eof_action=repeat:enable='gte(t+0.0001,0.5)*lt(t+0.0001,2)'[b2];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans Mono:fontsize=48:fontcolor=0x00000000:x=640-text_w/2:y=700-text_h:borderw=3:bordercolor=0x000000:line_spacing=-4" + layerTail + "[t2];" +
					"[b2][t2]overlay=format=auto:shortest=1:eof_action=repeat:enable='gte(t+0.0001,0.5)*lt(t+0.0001,2)'[b3];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans Mono:fontsize=48:fontcolor=0xff0000:x=640-text_w/2:y=700-text_h:line_spacing=-4" + layerTail + ",colorchannelmixer=aa=0.502[t3];" +
					"[b3][t3]overlay=format=auto:shortest=1:eof_action=repeat:enable='gte(t+0.0001,0.5)*lt(t+0.0001,2)',format=rgba[out]",
				Width: 1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hello\nWorld"}},
			},
		},
		{
			name: "text box defaults (translucent box) and an open-ended start: a box layer and an opaque glyph layer",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{text(recipe.TextParams{Text: "x", Box: true, Start: 1.5, Anchor: "mc", X: 640, Y: 360})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0x00000000:x=640-text_w/2:y=360-text_h/2:box=1:boxcolor=0x000000:boxborderw=8" + layerTail + ",colorchannelmixer=aa=0.502[t1];" +
					"[b1][t1]overlay=format=auto:shortest=1:eof_action=repeat:enable='gte(t+0.0001,1.5)'[b2];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=640-text_w/2:y=360-text_h/2" + layerTail + "[t2];" +
					"[b2][t2]overlay=format=auto:shortest=1:eof_action=repeat:enable='gte(t+0.0001,1.5)',format=rgba[out]",
				Width: 1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "x"}},
			},
		},
		{
			name: "adjacent elements sharing an alpha share a layer: box and border at 80, glyphs opaque",
			srcs: []recipe.ProbeInfo{h264},
			ops:  []recipe.Op{text(recipe.TextParams{Text: "x", Box: true, BoxColor: "00ff0080", Border: 2, BorderColor: "0000ff80"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0x00000000:x=0:y=0:borderw=2:bordercolor=0x0000ff:box=1:boxcolor=0x00ff00:boxborderw=8" + layerTail + ",colorchannelmixer=aa=0.502[t1];" +
					"[b1][t1]overlay=format=auto:shortest=1:eof_action=repeat[b2];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=0:y=0" + layerTail + "[t2];" +
					"[b2][t2]overlay=format=auto:shortest=1:eof_action=repeat,format=rgba[out]",
				Width: 1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "x"}},
			},
		},
		{
			name: "translucent glyphs alone: one layer; a box whose colour is translucent but off stays out of the decision",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{text(recipe.TextParams{Text: "x", Color: "ffffff80", BoxColor: "00000080"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=0:y=0" + layerTail + ",colorchannelmixer=aa=0.502[t1];" +
					"[b1][t1]overlay=format=auto:shortest=1:eof_action=repeat,format=rgba[out]",
				Width: 1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "x"}},
			},
		},
		{
			name: "text layers on a still: the colour source runs at the plan rate and the base's single frame ends the composite",
			srcs: []recipe.ProbeInfo{still}, ops: []recipe.Op{text(recipe.TextParams{Text: "Hi", Box: true})}, out: recipe.Output{Format: recipe.FormatPNG},
			want: Plan{
				Filter: "[0:v]format=rgba[b1];" +
					"color=c=0x00000000:s=800x600:r=10,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0x00000000:x=0:y=0:box=1:boxcolor=0x000000:boxborderw=8" + layerTail + ",colorchannelmixer=aa=0.502[t1];" +
					"[b1][t1]overlay=format=auto:shortest=1:eof_action=repeat[b2];" +
					"color=c=0x00000000:s=800x600:r=10,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=0:y=0" + layerTail + "[t2];" +
					"[b2][t2]overlay=format=auto:shortest=1:eof_action=repeat,format=rgba[out]",
				Width: 800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hi"}},
			},
		},
		{
			name: "layered text after an overlay and before a second, in-place text op: labels and text files keep their order",
			srcs: []recipe.ProbeInfo{h264, ovPNG},
			ops:  []recipe.Op{overlay(recipe.OverlayParams{Source: 1}), text(recipe.TextParams{Text: "one", Color: "ff000080"}), text(recipe.TextParams{Text: "two"})},
			out:  webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + "[b2];" +
					"color=c=0x00000000:s=1280x720:r=29.97,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xff0000:x=0:y=0" + layerTail + ",colorchannelmixer=aa=0.502[t1];" +
					"[b2][t1]overlay=format=auto:shortest=1:eof_action=repeat," + text2 + ",format=rgba[out]",
				Width: 1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
				TextFiles:   []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "one"}, {Placeholder: "__EZLG_TEXT_2__", Content: "two"}},
			},
		},
		{
			name: "text is drawn on the output canvas after the fit, in output pixels",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{text(recipe.TextParams{Text: "GG", Anchor: "br", X: 128, Y: 128, Size: 16})},
			out: recipe.Output{Format: "gif", Width: 128, Height: 128, FPS: 25},
			want: Plan{
				Filter: "[0:v]format=rgba,fps=25:round=down,format=gbrap,premultiply=inplace=1,scale=128:72:flags=lanczos,unpremultiply=inplace=1,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=16:fontcolor=0xffffff:x=128-text_w:y=128-text_h,format=rgba[out]",
				Width:  128, Height: 128, FPS: 25, HasAlpha: true, Duration: 4, Frames: 100,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "GG"}},
			},
		},
		{
			name: "two text ops get their own files, in order",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{text(recipe.TextParams{Text: "one"}), text(recipe.TextParams{Text: "two"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba," + text1 + "," + text2 + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "one"}, {Placeholder: "__EZLG_TEXT_2__", Content: "two"}},
			},
		},
		{
			name: "text on a still",
			srcs: []recipe.ProbeInfo{still}, ops: []recipe.Op{text(recipe.TextParams{Text: "Hi"})}, out: recipe.Output{Format: recipe.FormatPNG},
			want: Plan{
				Filter: "[0:v]format=rgba," + text1 + ",format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hi"}},
			},
		},
		{
			name: "text colour with a trimmed hash and a colorkey before it",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "00ff00"}), text(recipe.TextParams{Text: "Hi", Color: " #ABCDEF "})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,colorkey=color=0x00ff00:similarity=0.1:blend=0,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xabcdef:x=0:y=0,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hi"}},
			},
		},

		// --- overlays -------------------------------------------------------
		{
			name: "still png overlay: -loop 1 input, natural size, top-left",
			srcs: []recipe.ProbeInfo{h264, ovPNG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "opaque jpeg overlay scaled by width, middle-right anchor",
			srcs: []recipe.ProbeInfo{h264, ovJPEG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Width: 50, Anchor: "mr", X: 1280, Y: 360})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,scale=50:25:flags=lanczos[ov1];[b1][ov1]overlay=x=1280-overlay_w:y=360-overlay_h/2:format=auto:shortest=1:eof_action=repeat,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "png overlay with exact size, opacity and a half-open time range uses the premultiplied scale chain",
			srcs: []recipe.ProbeInfo{h264, ovPNG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Width: 32, Height: 32, Opacity: 0.5, Start: 1, End: 2.5})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=gbrap,premultiply=inplace=1,scale=32:32:flags=lanczos,unpremultiply=inplace=1,format=rgba,colorchannelmixer=aa=0.5[ov1];[b1][ov1]" + ovComposit + ":enable='gte(t+0.0001,1)*lt(t+0.0001,2.5)',format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "looping gif overlay: -stream_loop -1, resampled to the plan rate, no hold",
			srcs: []recipe.ProbeInfo{h264, ovGIF}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, X: 10, Y: 20})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97[ov1];[b1][ov1]overlay=x=10:y=20:format=auto:shortest=1:eof_action=repeat,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 0.2, Loop: true}},
			},
		},
		{
			name: "non-looping gif overlay: no input args, the hold stage keeps the last frame",
			srcs: []recipe.ProbeInfo{h264, ovGIF}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, NoLoop: true})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Animated: true, Duration: 0.2}},
			},
		},
		{
			name: "looping animated webp: -ignore_loop 0 (the demuxer loops; -stream_loop hangs it) plus the hold stage",
			srcs: []recipe.ProbeInfo{h264, ovWebP}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: gif(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-ignore_loop", "0"}, Animated: true, Duration: 1, Loop: true}},
			},
		},
		{
			name: "looping apng: -ignore_loop 0 plus the hold stage",
			srcs: []recipe.ProbeInfo{h264, ovAPNG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-ignore_loop", "0"}, Animated: true, Duration: 4, Loop: true}},
			},
		},
		{
			name: "looping opaque mp4 overlay: -stream_loop -1, plain scale",
			srcs: []recipe.ProbeInfo{h264, ovMP4}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Height: 90})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97,scale=160:90:flags=lanczos[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 2, Loop: true}},
			},
		},
		{
			name: "vp9 alpha overlay forces libvpx-vp9 before the loop args",
			srcs: []recipe.ProbeInfo{h264, vp9}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-c:v", "libvpx-vp9", "-stream_loop", "-1"}, Animated: true, Duration: 2, Loop: true}},
			},
		},
		{
			name: "non-looping vp8 alpha overlay keeps only the decoder arg",
			srcs: []recipe.ProbeInfo{h264, with(vp9, func(p *recipe.ProbeInfo) { p.Codec = "vp8" })}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, NoLoop: true})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-c:v", "libvpx"}, Animated: true, Duration: 2}},
			},
		},
		{
			name: "one-frame gif still: the gif demuxer has no -loop, so no args and the hold stage",
			srcs: []recipe.ProbeInfo{h264, ovGIFStill}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1}},
			},
		},
		{
			// The overlay head follows the main source's rule (alphaHead): a
			// planar yuva source is unpremultiplied natively, then converted
			// to rgba once — never through gbrap10le.
			name: "premultiplied prores overlay is unpremultiplied natively first, then rgba",
			srcs: []recipe.ProbeInfo{h264, ovProRes}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba,fps=29.97[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 1, Loop: true}},
			},
		},
		{
			name: "a scaled yuva overlay keeps its format=rgba in front of the premultiplied scale chain",
			srcs: []recipe.ProbeInfo{h264, with(ovProRes, func(p *recipe.ProbeInfo) {
				p.IsStill, p.Frames, p.FPS, p.Duration, p.Kind = true, 1, 0, 0, recipe.KindImage
			})},
			ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Width: 100})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba,format=gbrap,premultiply=inplace=1,scale=100:50:flags=lanczos,unpremultiply=inplace=1,format=rgba," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1}},
			},
		},
		{
			name: "a premultiplied RGB overlay keeps the native-depth gbrap head and drops the redundant format=rgba before its scale",
			srcs: []recipe.ProbeInfo{h264, with(ovPNG, func(p *recipe.ProbeInfo) { p.PixFmt, p.Bits, p.Premultiplied = "rgba64be", 16, true })},
			ops:  []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Width: 32})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=gbrap,premultiply=inplace=1,scale=32:16:flags=lanczos,unpremultiply=inplace=1,format=rgba[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "still avif overlay merges its alpha stream; the mov demuxer has no -loop, so the hold stage",
			srcs: []recipe.ProbeInfo{h264, avifAlpha}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Width: 32})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v:0]format=rgba[ov1c];[1:v:1]format=gray[ov1a];[ov1c][ov1a]alphamerge,format=gbrap,premultiply=inplace=1,scale=32:32:flags=lanczos,unpremultiply=inplace=1,format=rgba," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1}},
			},
		},
		{
			name: "animated avif overlay addresses its animation tracks and loops with -stream_loop",
			srcs: []recipe.ProbeInfo{h264, ovAVIFAnim}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v:2]format=rgba[ov1c];[1:v:3]format=gray[ov1a];[ov1c][ov1a]alphamerge,format=rgba,fps=29.97[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 2, Loop: true}},
			},
		},
		{
			name: "the same source twice shares one input and two chains",
			srcs: []recipe.ProbeInfo{h264, ovPNG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1}), overlay(recipe.OverlayParams{Source: 1, X: 10, Y: 10})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + "[b2];[1:v]format=rgba[ov2];[b2][ov2]overlay=x=10:y=10:format=auto:shortest=1:eof_action=repeat,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "extra inputs are numbered in order of first use, not source order",
			srcs: []recipe.ProbeInfo{h264, ovPNG, ovGIF}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 2}), overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97[ov1];[b1][ov1]" + ovComposit + "[b2];[2:v]format=rgba[ov2];[b2][ov2]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{
					{Source: 2, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 0.2, Loop: true},
					{Source: 1, Args: []string{"-loop", "1"}},
				},
			},
		},
		{
			name: "text, overlay, text keep their order across the chains",
			srcs: []recipe.ProbeInfo{h264, ovPNG}, ops: []recipe.Op{text(recipe.TextParams{Text: "one"}), overlay(recipe.OverlayParams{Source: 1}), text(recipe.TextParams{Text: "two"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba," + text1 + "[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + "," + text2 + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
				TextFiles:   []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "one"}, {Placeholder: "__EZLG_TEXT_2__", Content: "two"}},
			},
		},
		{
			name: "overlay over an image-sequence main source",
			srcs: []recipe.ProbeInfo{pngSeq, ovPNG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down,format=rgba[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "animated overlay over a still main source yields one frame",
			srcs: []recipe.ProbeInfo{still, ovGIF}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba[b1];[1:v]format=rgba,fps=10[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 0.2, Loop: true}},
			},
		},
		{
			name: "overlay on an alpha-stream main source follows the merge head",
			srcs: []recipe.ProbeInfo{avifAlpha, ovPNG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, out: webp(),
			want: Plan{
				Filter: alphaHead1 + "format=rgba[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Frames: 1,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "overlay with an unknown duration reports 0",
			srcs: []recipe.ProbeInfo{h264, with(ovMP4, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 0, 0 })}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1, NoLoop: true})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[b1];[1:v]format=rgba,fps=29.97," + hold + "[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{Source: 1, Animated: true}},
			},
		},

		// --- everything at once ----------------------------------------------
		{
			name: "emote pipeline: unpremultiply, trim, chromakey, autocrop, reverse, fit 128, text, looping gif overlay",
			srcs: []recipe.ProbeInfo{prores, ovGIF},
			ops: []recipe.Op{
				unpremultiply(), trim(0.5, 3.5), chromakey(recipe.ChromaKeyParams{}), resolved(420, 0, 1080, 1080), reverse(),
				text(recipe.TextParams{Text: "GG", Anchor: "bc", X: 64, Y: 124, Size: 16}),
				overlay(recipe.OverlayParams{Source: 1, Anchor: "tr", X: 128, Y: 0, Width: 16}),
			},
			out: recipe.Output{Format: "gif", Width: 128, Height: 128, FPS: 25, Preset: "emote", Target: "emote"},
			want: Plan{
				InputArgs: []string{"-ss", "0.5", "-to", "3.5"},
				// The ProRes source carries alpha, so the chromakey is wrapped;
				// the reverse follows the output fit (the frames it buffers are
				// the 128x128 ones) and precedes the text and the overlay.
				Filter: "[0:v]setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba,fps=25:round=down," + keyWrapped(1, ckDefault+",format=rgba") +
					",crop=1080:1080:420:0:exact=1,format=gbrap,premultiply=inplace=1,scale=128:128:flags=lanczos,unpremultiply=inplace=1,format=rgba,reverse,format=rgba" +
					",drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=16:fontcolor=0xffffff:x=64-text_w/2:y=124-text_h[b1]" +
					";[1:v]format=rgba,fps=25,format=gbrap,premultiply=inplace=1,scale=16:16:flags=lanczos,unpremultiply=inplace=1,format=rgba[ov1]" +
					";[b1][ov1]overlay=x=128-overlay_w:y=0:format=auto:shortest=1:eof_action=repeat,format=rgba[out]",
				Width: 128, Height: 128, FPS: 25, HasAlpha: true, Duration: 3, Frames: 75,
				TrimStart: 0.5, TrimEnd: 3.5, Reversed: true,
				ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 0.2, Loop: true}},
				TextFiles:   []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "GG"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileWithSources(tc.srcs, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("CompileWithSources: %v", err)
			}
			want := tc.want
			want.OutLabel = "[out]"
			if want.Speed == 0 {
				want.Speed = 1
			}
			if want.SourceFPS == 0 {
				want.SourceFPS = tc.srcs[0].FPS
			}
			want.SourceVFR = tc.srcs[0].Kind == recipe.KindAnimation
			want.SeekUnsafe = seekUnsafeDemuxer(tc.srcs[0].Format)
			checkPlan3(t, got, &want)
		})
	}
}

// checkPlan3 is checkPlan plus the Phase 3 fields; the filter may hold
// several chains, the last of which must end in [out]. SourceVFR is expected
// true for every animation (gif/apng/webp/avif) main source and false
// otherwise; SeekUnsafe for every webp_anim main source, trimmed or not; the
// callers derive both from the main source's kind and format.
func checkPlan3(t *testing.T, got, want *Plan) {
	t.Helper()
	if got.Filter != want.Filter {
		t.Errorf("Filter\n got: %s\nwant: %s", got.Filter, want.Filter)
	}
	if !strings.HasSuffix(got.Filter, got.OutLabel) || got.OutLabel != "[out]" {
		t.Errorf("Filter must end with [out]: %s", got.Filter)
	}
	if !reflect.DeepEqual(got.InputArgs, want.InputArgs) {
		t.Errorf("InputArgs got %q want %q", got.InputArgs, want.InputArgs)
	}
	if got.InputPattern != want.InputPattern {
		t.Errorf("InputPattern got %q want %q", got.InputPattern, want.InputPattern)
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
	if got.Reversed != want.Reversed {
		t.Errorf("Reversed got %v want %v", got.Reversed, want.Reversed)
	}
	if got.Bounced != want.Bounced {
		t.Errorf("Bounced got %v want %v", got.Bounced, want.Bounced)
	}
	if got.SourceVFR != want.SourceVFR {
		t.Errorf("SourceVFR got %v want %v", got.SourceVFR, want.SourceVFR)
	}
	if got.FilterTrim != want.FilterTrim {
		t.Errorf("FilterTrim got %v want %v", got.FilterTrim, want.FilterTrim)
	}
	if got.SeekUnsafe != want.SeekUnsafe {
		t.Errorf("SeekUnsafe got %v want %v", got.SeekUnsafe, want.SeekUnsafe)
	}
	if got.FilterTrim && !got.SeekUnsafe {
		t.Errorf("FilterTrim implies SeekUnsafe: %+v", got)
	}
	if args := strings.Join(got.InputArgs, " "); got.FilterTrim && (!strings.Contains(got.Filter, "trim=start=") || strings.Contains(args, "-ss") || strings.Contains(args, "-to")) {
		t.Errorf("a FilterTrim plan must carry the trim stage and no -ss/-to: %q %s", got.InputArgs, got.Filter)
	} else if !got.FilterTrim && strings.Contains(got.Filter, "trim=") {
		t.Errorf("a trim stage without FilterTrim: %s", got.Filter)
	} else if got.SeekUnsafe && (strings.Contains(args, "-ss") || strings.Contains(args, "-to")) {
		t.Errorf("a SeekUnsafe plan must carry no -ss/-to: %q", got.InputArgs)
	}
	for _, f := range []struct {
		name      string
		got, want float64
	}{
		{"FPS", got.FPS, want.FPS}, {"Duration", got.Duration, want.Duration},
		{"TrimStart", got.TrimStart, want.TrimStart}, {"TrimEnd", got.TrimEnd, want.TrimEnd},
		{"Speed", got.Speed, want.Speed}, {"SourceFPS", got.SourceFPS, want.SourceFPS},
	} {
		if d := f.got - f.want; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s got %v want %v", f.name, f.got, f.want)
		}
	}
	if !reflect.DeepEqual(got.ExtraInputs, want.ExtraInputs) {
		t.Errorf("ExtraInputs\n got: %+v\nwant: %+v", got.ExtraInputs, want.ExtraInputs)
	}
	if !reflect.DeepEqual(got.TextFiles, want.TextFiles) {
		t.Errorf("TextFiles\n got: %+v\nwant: %+v", got.TextFiles, want.TextFiles)
	}
	for i, in := range got.ExtraInputs {
		if in.Path != "" {
			t.Errorf("ExtraInputs[%d].Path must be empty from the compiler, got %q", i, in.Path)
		}
		if !strings.Contains(got.Filter, "["+strconv.Itoa(i+1)+":v") {
			t.Errorf("ExtraInputs[%d] is never read as input %d in %s", i, i+1, got.Filter)
		}
	}
	for _, tf := range got.TextFiles {
		// Once per drawtext stage of the op: one in place, one per layer of a
		// translucent text op (textLayers).
		if n, layers := strings.Count(got.Filter, "textfile="+tf.Placeholder), strings.Count(got.Filter, "drawtext="); n < 1 || n > max(1, layers) {
			t.Errorf("placeholder %s occurs %d times (drawtext stages: %d) in %s", tf.Placeholder, n, layers, got.Filter)
		}
	}
}

// --- error cases ------------------------------------------------------------

func TestCompilePhase3Errors(t *testing.T) {
	long := strings.Repeat("a", MaxFontNameLen+1)
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		want string // substring of the error
	}{
		// autocrop
		{"unresolved autocrop", []recipe.ProbeInfo{h264}, []recipe.Op{autocrop(recipe.AutoCropParams{Threshold: 8})}, "op 0 (autocrop): autocrop not resolved (jobs resolves it before compiling)"},
		{"autocrop resolved out of range", []recipe.ProbeInfo{h264}, []recipe.Op{resolved(1000, 0, 400, 400)}, "op 0 (autocrop): rectangle 400x400 at (1000,0) exceeds the 1280x720 frame"},
		{"autocrop resolved empty", []recipe.ProbeInfo{h264}, []recipe.Op{resolved(0, 0, 0, 0)}, "op 0 (autocrop): size must be at least 1x1"},
		{"autocrop threshold above 255", []recipe.ProbeInfo{h264}, []recipe.Op{autocrop(recipe.AutoCropParams{Threshold: 300, Resolved: &recipe.CropParams{W: 1, H: 1}})}, "op 0 (autocrop): threshold must be between 0 and 255 (got 300)"},
		{"autocrop padding above the maximum", []recipe.ProbeInfo{h264}, []recipe.Op{autocrop(recipe.AutoCropParams{Padding: 5000, Resolved: &recipe.CropParams{W: 1, H: 1}})}, "op 0 (autocrop): padding must be between 0 and 1024 px (got 5000)"},
		// keying
		{"chromakey colour with alpha", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{Color: "00ff0080"})}, `op 0 (chromakey): key colour "00ff0080" must be RRGGBB (no alpha)`},
		{"chromakey colour name", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{Color: "green"})}, `op 0 (chromakey): colour "green": want RRGGBB or RRGGBBAA`},
		{"chromakey similarity above 1", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{Similarity: 2})}, "op 0 (chromakey): similarity must be between 0.01 and 1 (got 2)"},
		{"chromakey similarity below the minimum", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{Similarity: 0.001})}, "op 0 (chromakey): similarity must be between 0.01 and 1 (got 0.001)"},
		{"chromakey blend negative", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{Blend: -0.1})}, "op 0 (chromakey): blend must be between 0 and 1 (got -0.1)"},
		{"chromakey despill mix above 1", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{DespillMix: 1.5})}, "op 0 (chromakey): despillMix must be between 0 and 1 (got 1.5)"},
		{"chromakey despill expand above 1", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{DespillExpand: 7})}, "op 0 (chromakey): despillExpand must be between 0 and 1 (got 7)"},
		{"colorkey without a colour", []recipe.ProbeInfo{h264}, []recipe.Op{colorkey(recipe.ColorKeyParams{})}, "op 0 (colorkey): colour is required"},
		{"colorkey bad colour", []recipe.ProbeInfo{h264}, []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "#12345"})}, `op 0 (colorkey): colour "12345"`},
		{"colorkey colour with alpha", []recipe.ProbeInfo{h264}, []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffffff"})}, "op 0 (colorkey): key colour"},
		{"colorkey blend above 1", []recipe.ProbeInfo{h264}, []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff", Blend: 1.01})}, "op 0 (colorkey): blend must be between 0 and 1 (got 1.01)"},
		{"keying invalid params json", []recipe.ProbeInfo{h264}, []recipe.Op{rawOp(recipe.OpChromaKey, `{"similarity":"lots"}`)}, "op 0 (chromakey): invalid params"},
		// feather (validated on the opaque h264 source, where the stage would
		// be skipped: the params are checked either way)
		{"feather radius above the maximum", []recipe.ProbeInfo{h264}, []recipe.Op{feather(51)}, "op 0 (feather): radius must be between 0.1 and 50 px (got 51)"},
		{"feather radius below the minimum", []recipe.ProbeInfo{h264}, []recipe.Op{feather(0.05)}, "op 0 (feather): radius must be between 0.1 and 50 px (got 0.05)"},
		{"feather radius negative", []recipe.ProbeInfo{h264}, []recipe.Op{feather(-3)}, "op 0 (feather): radius must be between 0.1 and 50 px (got -3)"},
		{"feather invalid params json", []recipe.ProbeInfo{h264}, []recipe.Op{rawOp(recipe.OpFeather, `{"radius":"soft"}`)}, "op 0 (feather): invalid params"},
		// text
		{"text empty", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{})}, "op 0 (text): text is required"},
		{"text whitespace only", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: " \n\t"})}, "op 0 (text): text is required"},
		{"font with punctuation", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Font: "Comic Sans!"})}, `op 0 (text): font "Comic Sans!" may only contain letters, digits, spaces and hyphens`},
		{"font with a filter separator", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Font: "Sans:fontsize=99"})}, `op 0 (text): font "Sans:fontsize=99" may only contain`},
		{"font with a quote", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Font: "Sans'"})}, "op 0 (text): font"},
		{"font with a path", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Font: "/usr/share/fonts/x.ttf"})}, "op 0 (text): font"},
		{"font too long", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Font: long})}, "op 0 (text): font name is too long (65 characters, maximum 64)"},
		{"text size negative", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Size: -1})}, "op 0 (text): size must be between 1 and 8192 px (got -1)"},
		{"text size above the maximum", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Size: 9000})}, "op 0 (text): size must be between 1 and 8192 px (got 9000)"},
		{"text bad colour", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Color: "red"})}, `op 0 (text): colour "red"`},
		{"text bad border colour", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", BorderColor: "zz"})}, `op 0 (text): border colour: colour "zz"`},
		{"text bad box colour", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", BoxColor: "#1234567"})}, `op 0 (text): box colour: colour "1234567"`},
		{"text border negative", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Border: -1})}, "op 0 (text): border must be between 0 and 256 px (got -1)"},
		{"text border above the maximum", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Border: 257})}, "op 0 (text): border must be between 0 and 256 px (got 257)"},
		{"text box pad above the maximum", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", BoxPad: 1025})}, "op 0 (text): boxPad must be between 0 and 1024 px (got 1025)"},
		{"text line spacing above the maximum", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", LineSpacing: -2000})}, "op 0 (text): lineSpacing must be between -1024 and 1024 px (got -2000)"},
		{"text bad anchor", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Anchor: "top"})}, `op 0 (text): anchor "top" must be one of tl, tc, tr, ml, mc, mr, bl, bc, br`},
		{"text anchor with unknown letters", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Anchor: "xx"})}, `op 0 (text): anchor "xx" must be one of`},
		{"text negative start", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Start: -1})}, "op 0 (text): start must be >= 0 (got -1)"},
		{"text negative end", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", End: -1})}, "op 0 (text): end must be >= 0 (got -1)"},
		{"text end before start", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Start: 2, End: 1})}, "op 0 (text): end (1 s) must be after start (2 s)"},
		{"text end equal to start", []recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "x", Start: 1, End: 1})}, "op 0 (text): end (1 s) must be after start (1 s)"},
		{"text invalid params json", []recipe.ProbeInfo{h264}, []recipe.Op{rawOp(recipe.OpText, `{"size":"big"}`)}, "op 0 (text): invalid params"},
		// overlay
		{"overlay of the main source", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 0})}, "op 0 (overlay): source must be >= 1 (source 0 is the main source; got 0)"},
		{"overlay negative source", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: -1})}, "op 0 (overlay): source must be >= 1"},
		{"overlay source out of range", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 2})}, "op 0 (overlay): source index 2 is out of range (the recipe has 2 source(s))"},
		{"overlay with a single-source recipe", []recipe.ProbeInfo{h264}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, "op 0 (overlay): source index 1 is out of range (the recipe has 1 source(s))"},
		{"overlay of an image sequence", []recipe.ProbeInfo{h264, pngSeq}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, "op 0 (overlay): source 1 is an image sequence; sequence overlays are not supported"},
		{"overlay source without a frame size", []recipe.ProbeInfo{h264, {Format: "png_pipe", IsStill: true}}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, "op 0 (overlay): source 1 has no usable frame size (0x0)"},
		{"overlay source with a bad alpha stream", []recipe.ProbeInfo{h264, with(ovPNG, func(p *recipe.ProbeInfo) { p.AlphaStream = -1 })}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1})}, "op 0 (overlay): source 1: source alpha stream index must be >= 0 (got -1)"},
		{"overlay negative size", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Width: -5})}, "op 0 (overlay): width and height must be >= 0 (got -5x0)"},
		{"overlay size above the maximum", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Height: 8193})}, "op 0 (overlay): width and height must be <= 8192 (got 0x8193)"},
		{"overlay size above 32 megapixels", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Width: 8192, Height: 8192})}, "op 0 (overlay): frame 8192x8192 exceeds the limits"},
		{"overlay opacity above 1", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Opacity: 1.5})}, "op 0 (overlay): opacity must be between 0 and 1 (got 1.5)"},
		{"overlay opacity negative", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Opacity: -0.5})}, "op 0 (overlay): opacity must be between 0 and 1 (got -0.5)"},
		{"overlay bad anchor", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Anchor: "centre"})}, `op 0 (overlay): anchor "centre" must be one of`},
		{"overlay end before start", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1, Start: 3, End: 2})}, "op 0 (overlay): end (2 s) must be after start (3 s)"},
		{"overlay loop conflict on a shared source", []recipe.ProbeInfo{h264, ovGIF}, []recipe.Op{overlay(recipe.OverlayParams{Source: 1}), overlay(recipe.OverlayParams{Source: 1, NoLoop: true})}, "op 1 (overlay): source 1 is already used by another overlay with a different loop setting"},
		{"overlay invalid params json", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{rawOp(recipe.OpOverlay, `{"source":"one"}`)}, "op 0 (overlay): invalid params"},
		{"error names the failing op index across phases", []recipe.ProbeInfo{h264, ovPNG}, []recipe.Op{chromakey(recipe.ChromaKeyParams{}), overlay(recipe.OverlayParams{Source: 1}), text(recipe.TextParams{})}, "op 2 (text): text is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := CompileWithSources(tc.srcs, tc.ops, webp())
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
	if _, err := CompileWithSources(nil, nil, webp()); err == nil || !strings.Contains(err.Error(), "no sources") {
		t.Errorf("no sources: %v", err)
	}
}

// --- reverse placement ------------------------------------------------------

// TestReverseAfterOutputFit: ffmpeg's reverse filter holds every frame it
// receives until EOF, so it must run after the output fit — the only shrink
// on the UI's Emote/Sticker preset path, which sets Output.Width/Height and
// no resize op. jobs' reverse-buffer admission (admitReversed, Options.
// MaxMasterBytes) measures the post-fit Plan.Width x Height, so a reverse in
// front of the fit buffered source-sized frames the estimate never saw (a
// 1080p 60 s ProRes clip fit to 128 px: ~28 GiB for a 0.11 GiB master).
// After the fit the buffered frame is exactly Plan.Width x Height and the
// estimate bounds the reverse buffer too. The graph itself caps nothing by
// frame count: the untrimmed clip compiles and reports its size.
func TestReverseAfterOutputFit(t *testing.T) {
	long := with(prores, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 60, 1800 })
	emote := recipe.Output{Format: "gif", Width: 128, Height: 128, FPS: 30}

	// Sanity: the same clip without the output fit compiles too (7.4 GiB of
	// reverse buffer at 1080p) — the graph reports Width/Height/Frames and
	// leaves the refusal to jobs, so what the fit case relies on is the
	// post-fit size those fields report.
	if p, err := Compile(long, []recipe.Op{reverse()}, recipe.Output{Format: "gif", FPS: 30}); err != nil {
		t.Fatalf("1080p x 1800 frames without a fit: %v, want a plan (the cap is jobs')", err)
	} else if p.Width != 1920 || p.Height != 1080 || p.Frames != 1800 || !p.Reversed {
		t.Fatalf("1080p x 1800 frames without a fit: %dx%d x %d frames reversed %v, want 1920x1080 x 1800 true", p.Width, p.Height, p.Frames, p.Reversed)
	}

	cases := []struct {
		name string
		ops  []recipe.Op
		out  recipe.Output
	}{
		{"emote preset: output fit only", []recipe.Op{reverse()}, emote},
		{"output width only", []recipe.Op{reverse()}, recipe.Output{Format: "webp", Width: 128}},
		{"cover fit", []recipe.Op{reverse()}, recipe.Output{Format: "webp", Width: 128, Height: 128, Fit: "cover"}},
		{"exact fit", []recipe.Op{reverse()}, recipe.Output{Format: "webp", Width: 128, Height: 128, Fit: "exact"}},
		{"resize op and output fit", []recipe.Op{resize(640, 0, ""), reverse()}, emote},
		{"reverse first in the stack, geometry after it", []recipe.Op{reverse(), crop(0, 0, 1080, 1080), flip(true, false)}, emote},
		{"keyed, trimmed, reversed emote", []recipe.Op{chromakey(recipe.ChromaKeyParams{}), trim(0, 10), reverse()}, emote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(long, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if !p.Reversed || p.Width != 128 {
				t.Fatalf("plan: reversed %v %dx%d", p.Reversed, p.Width, p.Height)
			}
			rev, scale := strings.Index(p.Filter, ",reverse"), strings.LastIndex(p.Filter, "scale=")
			if rev < 0 || scale < 0 || rev < scale {
				t.Errorf("reverse must follow the output fit's scale (it buffers the frames it sees):\n%s", p.Filter)
			}
			if pad := strings.LastIndex(p.Filter, "pad="); pad >= 0 && rev < pad {
				t.Errorf("reverse must follow the fit's pad:\n%s", p.Filter)
			}
			// Nothing after the reverse changes the frame size: the tail is
			// the terminal format=rgba only; and the reverse buffers rgba
			// (4 B/px, what jobs' master estimate assumes), so a format=rgba
			// sits right in front of it.
			if tail := p.Filter[rev:]; tail != ",reverse,format=rgba[out]" {
				t.Errorf("stages after the reverse %q; want only the terminal format", tail)
			}
			if !strings.HasSuffix(p.Filter[:rev], ",format=rgba") {
				t.Errorf("reverse must be preceded by format=rgba (its buffer is estimated at 4 B/px):\n%s", p.Filter)
			}
		})
	}

	// Final-canvas ops still follow the reverse (their timing is in reversed
	// output time), and the fit precedes it.
	p, err := CompileWithSources([]recipe.ProbeInfo{long, ovPNG}, []recipe.Op{reverse(), text(recipe.TextParams{Text: "x"}), overlay(recipe.OverlayParams{Source: 1})}, emote)
	if err != nil {
		t.Fatalf("Compile with text and overlay: %v", err)
	}
	rev := strings.Index(p.Filter, ",reverse,")
	if rev < 0 || rev < strings.Index(p.Filter, "scale=") || rev > strings.Index(p.Filter, "drawtext") || rev > strings.Index(p.Filter, "overlay=") {
		t.Errorf("reverse must sit between the output fit and the final-canvas ops:\n%s", p.Filter)
	}
}

// --- keying alpha sources ---------------------------------------------------

// TestKeyOnAlphaSourcesIsWrapped: ffmpeg's chromakey and colorkey overwrite
// the alpha plane, so every key applied to frames that already carry alpha
// — whatever put it there — goes through keyKeepingAlpha, while keys on
// opaque frames stay bare. Both Compile and CompileDetect (jobs' autocrop
// detection reads the box off that alpha) must agree.
func TestKeyOnAlphaSourcesIsWrapped(t *testing.T) {
	opaqueStill := with(still, func(p *recipe.ProbeInfo) { p.HasAlpha, p.PixFmt = false, "rgb24" })
	opaqueSeq := with(pngSeq, func(p *recipe.ProbeInfo) { p.HasAlpha = false })
	mixedOpaqueSeq := withSeq(opaqueSeq, func(s *recipe.SequenceInfo) { s.Mixed = true })
	keys := map[string]recipe.Op{
		"chromakey": chromakey(recipe.ChromaKeyParams{}),
		"colorkey":  colorkey(recipe.ColorKeyParams{Color: "00ff00"}),
	}
	sources := []struct {
		name    string
		src     recipe.ProbeInfo
		wrapped bool
	}{
		{"prores 4444 (source alpha)", prores, true},
		{"transparent gif", gifSrc, true},
		{"vp9 alpha", vp9, true},
		{"transparent still", still, true},
		{"transparent sequence", pngSeq, true},
		{"avif with a separate alpha stream", avifAlpha, true},
		{"opaque mixed-size sequence (transparent padding)", mixedOpaqueSeq, true},
		{"h264", h264, false},
		{"opaque still", opaqueStill, false},
		{"opaque uniform sequence", opaqueSeq, false},
	}
	for _, s := range sources {
		for kind, key := range keys {
			t.Run(s.name+"/"+kind, func(t *testing.T) {
				for _, p := range []*Plan{mustCompile(t, s.src, []recipe.Op{key}, webp()), mustDetect(t, s.src, []recipe.Op{key})} {
					if !p.HasAlpha {
						t.Errorf("keyed plan reports no alpha: %s", p.Filter)
					}
					got := strings.Contains(p.Filter, "split[k1][k1m];[k1m]alphaextract[k1a0];[k1]") &&
						strings.Contains(p.Filter, "[k1a0][k1a1]blend=all_mode=multiply[k1a];[k1k][k1a]alphamerge,")
					if got != s.wrapped {
						t.Errorf("wrapped %v, want %v:\n%s", got, s.wrapped, p.Filter)
					}
					if !s.wrapped && strings.Contains(p.Filter, "alphaextract") {
						t.Errorf("opaque frames must get the bare key:\n%s", p.Filter)
					}
					if !strings.HasSuffix(p.Filter, ",format=rgba[out]") {
						t.Errorf("filter must end in the terminal format: %s", p.Filter)
					}
				}
			})
		}
	}
	// A second key always sees alpha (the first key's), whatever the source.
	p := mustCompile(t, h264, []recipe.Op{keys["chromakey"], keys["colorkey"]}, webp())
	if strings.Count(p.Filter, "alphaextract") != 2 || !strings.Contains(p.Filter, ckDefault+",split[k1][k1m]") {
		t.Errorf("second key on an opaque source must be wrapped (once):\n%s", p.Filter)
	}
	p = mustCompile(t, gifSrc, []recipe.Op{keys["chromakey"], keys["colorkey"]}, webp())
	if strings.Count(p.Filter, "alphaextract") != 4 || !strings.Contains(p.Filter, "alphamerge,split[k2][k2m]") {
		t.Errorf("two keys on an alpha source must both be wrapped:\n%s", p.Filter)
	}
}

func mustCompile(t *testing.T, src recipe.ProbeInfo, ops []recipe.Op, out recipe.Output) *Plan {
	t.Helper()
	p, err := Compile(src, ops, out)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

func mustDetect(t *testing.T, src recipe.ProbeInfo, ops []recipe.Op) *Plan {
	t.Helper()
	p, err := CompileDetect([]recipe.ProbeInfo{src}, ops)
	if err != nil {
		t.Fatalf("CompileDetect: %v", err)
	}
	return p
}

// --- helpers ----------------------------------------------------------------

func TestAnchorExprs(t *testing.T) {
	tests := []struct {
		anchor string
		x, y   int
		wantX  string
		wantY  string
	}{
		{"", 10, 20, "10", "20"},
		{"tl", 10, 20, "10", "20"},
		{"tc", 10, 20, "10-text_w/2", "20"},
		{"tr", 10, 20, "10-text_w", "20"},
		{"ml", 10, 20, "10", "20-text_h/2"},
		{"mc", 0, 0, "0-text_w/2", "0-text_h/2"},
		{"mr", -5, 20, "-5-text_w", "20-text_h/2"},
		{"bl", 10, 20, "10", "20-text_h"},
		{"bc", 10, 20, "10-text_w/2", "20-text_h"},
		{"br", 128, 128, "128-text_w", "128-text_h"},
		{" BR ", 1, 2, "1-text_w", "2-text_h"},
	}
	for _, tc := range tests {
		x, y, err := anchorExprs(tc.anchor, tc.x, tc.y, "text_w", "text_h")
		if err != nil {
			t.Errorf("anchorExprs(%q): %v", tc.anchor, err)
			continue
		}
		if x != tc.wantX || y != tc.wantY {
			t.Errorf("anchorExprs(%q, %d, %d) = %q, %q; want %q, %q", tc.anchor, tc.x, tc.y, x, y, tc.wantX, tc.wantY)
		}
	}
	for _, bad := range []string{"t", "tlx", "xx", "tx", "xl", "top-left"} {
		if _, _, err := anchorExprs(bad, 0, 0, "w", "h"); err == nil {
			t.Errorf("anchorExprs(%q) should fail", bad)
		}
	}
	if x, y, _ := anchorExprs("mc", 8, 8, "overlay_w", "overlay_h"); x != "8-overlay_w/2" || y != "8-overlay_h/2" {
		t.Errorf("overlay variables: %s %s", x, y)
	}
}

func TestEnableExpr(t *testing.T) {
	tests := []struct {
		start, end float64
		want       string
	}{
		{0, 0, ""},
		{1.5, 0, "'gte(t+0.0001,1.5)'"},
		// A bounded window is half-open, [S, E): gte for the start, lt for
		// the end — never between(), which includes the frame at E. The
		// frame time gets the enableTolerance slack so a microsecond-rounded
		// bound taken from the frame grid keeps its frame.
		{0, 2, "'gte(t+0.0001,0)*lt(t+0.0001,2)'"},
		{0.5, 2, "'gte(t+0.0001,0.5)*lt(t+0.0001,2)'"},
		{2.0 / 30, 6.0 / 30, "'gte(t+0.0001,0.066667)*lt(t+0.0001,0.2)'"},
		{1.0 / 29.97, 0, "'gte(t+0.0001,0.033367)'"},
		{0.0000001, 0, ""}, // rounds to 0 at microsecond precision
		{0, 0.0000004, ""}, // so does an end that small: the whole clip
	}
	for _, tc := range tests {
		got, err := enableExpr(tc.start, tc.end)
		if err != nil || got != tc.want {
			t.Errorf("enableExpr(%v, %v) = %q, %v; want %q", tc.start, tc.end, got, err, tc.want)
		}
		if strings.Contains(got, "between") {
			t.Errorf("enableExpr(%v, %v) = %q uses between(), which is inclusive at the end", tc.start, tc.end, got)
		}
		if got != "" && (strings.Contains(got, "(t,") || strings.Contains(got, "(n")) {
			t.Errorf("enableExpr(%v, %v) = %q compares the raw t (or n): a rounded-up frame-grid bound would skip its frame", tc.start, tc.end, got)
		}
	}
	// The tolerance swallows the microsecond rounding of a frame-grid bound
	// (at most 5e-7 s) and stays well inside a frame period at the 60 fps
	// cap (1/60 s), so a neighbouring frame can never slip into the window.
	if enableTolerance <= 5e-7 || enableTolerance >= 1.0/MaxFPS/2 {
		t.Errorf("enableTolerance %v must lie well between the µs rounding (5e-7) and half a frame at MaxFPS (%v)", enableTolerance, 1.0/MaxFPS/2)
	}
	for _, bad := range [][2]float64{{-1, 0}, {0, -1}, {2, 1}, {1, 1}, {math.NaN(), 0}, {0, math.Inf(1)}} {
		if got, err := enableExpr(bad[0], bad[1]); err == nil {
			t.Errorf("enableExpr(%v, %v) = %q, want an error", bad[0], bad[1], got)
		}
	}
}

// --- CompileDetect ----------------------------------------------------------

func TestCompileDetect(t *testing.T) {
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		want Plan // OutLabel implied; Speed 0 means 1; SourceFPS 0 means the main source's
	}{
		{
			name: "chromakey + trim prefix: the keying chain at source size, no scale or pad",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{trim(1, 3), chromakey(recipe.ChromaKeyParams{})},
			want: Plan{
				InputArgs: []string{"-ss", "1", "-to", "3"},
				Filter:    "[0:v]fps=29.97:round=down," + ckDefault + ",format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 2, Frames: 59,
				TrimStart: 1, TrimEnd: 3,
			},
		},
		{
			name: "no ops: the bare decode at the source rate",
			srcs: []recipe.ProbeInfo{h264}, ops: nil,
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "every other kind is ignored: geometry, autocrop, reverse, bounce, text, overlay",
			srcs: []recipe.ProbeInfo{h264, ovPNG},
			ops: []recipe.Op{
				crop(0, 0, 640, 360), resize(320, 0, ""), canvas(400, 400, ""), flip(true, false), rotate(90),
				resolved(0, 0, 100, 100), autocrop(recipe.AutoCropParams{}), reverse(), bounce(),
				text(recipe.TextParams{Text: "x"}), overlay(recipe.OverlayParams{Source: 1}),
				colorkey(recipe.ColorKeyParams{Color: "ffffff"}),
			},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,colorkey=color=0xffffff:similarity=0.1:blend=0,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "ignored ops are not decoded: malformed params and unknown kinds pass",
			srcs: []recipe.ProbeInfo{h264},
			ops:  []recipe.Op{rawOp(recipe.OpText, `{bad`), {Kind: "sparkle"}, rawOp(recipe.OpCrop, `{"w":"wide"}`), trim(0.5, 0)},
			want: Plan{
				InputArgs: []string{"-ss", "0.5"},
				Filter:    "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 9.5, Frames: 284,
				TrimStart: 0.5,
			},
		},
		{
			// The detection keys exactly like the render: an alpha source's key
			// is wrapped so the source alpha survives (jobs then reads the box
			// off the alpha plane, which a bare key would have filled with 255).
			name: "prores: the native yuva head (hoisted unpremultiply, one format=rgba), speed and the fps op in stage order, then the wrapped key",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{fps(15), chromakey(recipe.ChromaKeyParams{DespillOff: true}), unpremultiply(), speed(2)},
			want: Plan{
				Filter: "[0:v]setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba,setpts=PTS/2,fps=15:round=down," + keyWrapped(1, ckNoSpill+",format=rgba") + ",format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 15, HasAlpha: true, Duration: 2, Frames: 30, Speed: 2,
			},
		},
		{
			name: "two keys apply in order: the second is wrapped",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"}), crop(0, 0, 10, 10), chromakey(recipe.ChromaKeyParams{DespillOff: true})},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckWhite + "," + keyWrapped(1, ckNoSpill+",format=rgba") + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
			},
		},
		{
			name: "vp9 alpha: the decoder forcing stays in the input args, the yuva head converts to rgba, the key is wrapped",
			srcs: []recipe.ProbeInfo{vp9}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{})},
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx-vp9"},
				Filter:    "[0:v]format=rgba,fps=30:round=down," + keyWrapped(1, ckDefault+",format=rgba") + ",format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60,
			},
		},
		{
			name: "image sequence with a delay op keeps the image2 head and rate",
			srcs: []recipe.ProbeInfo{pngSeq}, ops: []recipe.Op{delay(50), crop(0, 0, 10, 10)},
			want: Plan{
				InputArgs: seqArgs("20"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=20:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60, SourceFPS: 20,
			},
		},
		{
			name: "mixed-size sequence keeps its normalising head (the frames the crop applies to)",
			srcs: []recipe.ProbeInfo{withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true })}, ops: []recipe.Op{trim(1, 2)},
			want: Plan{
				InputArgs: seqArgs("10", "-ss", "1", "-to", "2"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 1, Frames: 10, SourceFPS: 10,
				TrimStart: 1, TrimEnd: 2,
			},
		},
		{
			name: "transparent still: no fps filter, one frame, the key wrapped off a bare [0:v]",
			srcs: []recipe.ProbeInfo{still}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"})},
			want: Plan{
				Filter: "[0:v]" + keyWrapped(1, ckWhite) + ",format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "opaque still: the key stays bare",
			srcs: []recipe.ProbeInfo{with(still, func(p *recipe.ProbeInfo) { p.HasAlpha, p.PixFmt = false, "rgb24" })}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"})},
			want: Plan{
				Filter: "[0:v]" + ckWhite + ",format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			name: "alpha-stream source keeps its merge head",
			srcs: []recipe.ProbeInfo{avifAlpha}, ops: nil,
			want: Plan{
				Filter: alphaHead1 + "format=rgba[out]",
				Width:  64, Height: 64, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
		{
			// A feather changes which alpha exceeds the autocrop threshold,
			// so the detection plan carries the gblur stage exactly like the
			// render (the unresolved autocrop itself is ignored, as always).
			name: "feather after a wrapped key is part of the detection plan",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{DespillOff: true}), feather(2), autocrop(recipe.AutoCropParams{Threshold: 8})},
			want: Plan{
				Filter: "[0:v]format=rgba,fps=30:round=down," + keyWrapped(1, ckNoSpill+",format=rgba") + ",format=gbrap,gblur=sigma=2:planes=8,format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "feather on an opaque source is skipped in the detection plan too",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{feather(3)},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileDetect(tc.srcs, tc.ops)
			if err != nil {
				t.Fatalf("CompileDetect: %v", err)
			}
			want := tc.want
			want.OutLabel = "[out]"
			if want.Speed == 0 {
				want.Speed = 1
			}
			if want.SourceFPS == 0 {
				want.SourceFPS = tc.srcs[0].FPS
			}
			want.SourceVFR = tc.srcs[0].Kind == recipe.KindAnimation
			want.SeekUnsafe = seekUnsafeDemuxer(tc.srcs[0].Format)
			checkPlan3(t, got, &want)
			if len(got.ExtraInputs) != 0 || len(got.TextFiles) != 0 || got.Reversed || got.Bounced {
				t.Errorf("detection plan must have no extra inputs, text files, reverse or bounce: %+v", got)
			}
			for _, stage := range []string{"scale=", "crop=", "pad=", "transpose", "hflip", "vflip", "reverse", "drawtext", "overlay"} {
				if tc.srcs[0].Sequence != nil && (stage == "scale=" || stage == "pad=") {
					continue // the sequence head's guarding scale / normalising pad
				}
				if strings.Contains(got.Filter, stage) {
					t.Errorf("detection plan must not contain %q: %s", stage, got.Filter)
				}
			}
		})
	}
}

func TestCompileDetectErrors(t *testing.T) {
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		want string // substring of the error
	}{
		{"no sources", nil, nil, "no sources"},
		{"source without a frame size", []recipe.ProbeInfo{{Format: "png_pipe", IsStill: true}}, nil, "source has no usable frame size (0x0)"},
		{"bad alpha stream", []recipe.ProbeInfo{with(h264, func(p *recipe.ProbeInfo) { p.AlphaStream = -1 })}, nil, "source alpha stream index must be >= 0"},
		{"trim end before start", []recipe.ProbeInfo{h264}, []recipe.Op{trim(3, 1)}, "op 0 (trim): end (1 s) must be after start (3 s)"},
		{"trim on a still", []recipe.ProbeInfo{still}, []recipe.Op{trim(1, 2)}, "op 0 (trim): the source is a still image and cannot be trimmed"},
		{"speed out of range", []recipe.ProbeInfo{h264}, []recipe.Op{speed(0)}, "op 0 (speed): factor must be > 0"},
		{"fps op zero", []recipe.ProbeInfo{h264}, []recipe.Op{fps(0)}, "op 0 (fps): fps must be > 0"},
		{"chromakey similarity above 1", []recipe.ProbeInfo{h264}, []recipe.Op{chromakey(recipe.ChromaKeyParams{Similarity: 2})}, "op 0 (chromakey): similarity must be between 0.01 and 1 (got 2)"},
		{"colorkey without a colour", []recipe.ProbeInfo{h264}, []recipe.Op{colorkey(recipe.ColorKeyParams{})}, "op 0 (colorkey): colour is required"},
		// Errors name the op by its index in the given slice, ignored ops included.
		{"malformed key params keep the recipe index", []recipe.ProbeInfo{h264}, []recipe.Op{crop(0, 0, 10, 10), rawOp(recipe.OpChromaKey, `{"similarity":"lots"}`)}, "op 1 (chromakey): invalid params"},
		{"sequence without sequence info", []recipe.ProbeInfo{with(pngSeq, func(p *recipe.ProbeInfo) { p.Sequence = nil })}, nil, "image sequence source has no sequence info"},
		{"sequence delay out of range", []recipe.ProbeInfo{pngSeq}, []recipe.Op{delay(0)}, "op 0 (delay): ms must be between 1 and 60000 (got 0)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := CompileDetect(tc.srcs, tc.ops)
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

// TestCompileDetectSkipsRenderLimits: the detection never materialises a
// master and cannot shrink the source, so the render's frame-size limits
// (MaxDim, MaxPixels — the only limits Compile applies; frame counts are
// jobs' admission, which detection plans never reach) do not apply to it: a
// source that only a later resize brings under them must still be croppable
// to its content. A merely long clip compiles through both entry points.
func TestCompileDetectSkipsRenderLimits(t *testing.T) {
	long := with(h264, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 3600, 108000 }) // 1280x720x4 x 107892 frames = 370 GiB
	if p, err := Compile(long, nil, webp()); err != nil {
		t.Fatalf("Compile of the long clip: %v, want a plan (the graph caps no frame count)", err)
	} else if p.Frames != 107892 || p.Width != 1280 || p.Height != 720 {
		t.Fatalf("Compile of the long clip: %d frames %dx%d, want 107892 1280x720", p.Frames, p.Width, p.Height)
	}
	p, err := CompileDetect([]recipe.ProbeInfo{long}, []recipe.Op{chromakey(recipe.ChromaKeyParams{})})
	if err != nil {
		t.Fatalf("CompileDetect of the long clip: %v", err)
	}
	if p.Frames != 107892 || p.Width != 1280 || !p.HasAlpha {
		t.Errorf("long clip plan: %d frames %dx%d alpha %v", p.Frames, p.Width, p.Height, p.HasAlpha)
	}
	// The same recipe renders once the autocrop is followed by a resize.
	if _, err := Compile(long, []recipe.Op{chromakey(recipe.ChromaKeyParams{}), resolved(0, 0, 640, 360), resize(128, 0, "")}, webp()); err != nil {
		t.Errorf("Compile with a resize after the autocrop: %v", err)
	}

	huge := with(still, func(p *recipe.ProbeInfo) { p.Width, p.Height = 9000, 9000 })
	if _, err := Compile(huge, nil, webp()); err == nil || !strings.Contains(err.Error(), "exceeds the limits") {
		t.Fatalf("Compile of the oversized still: %v", err)
	}
	p, err = CompileDetect([]recipe.ProbeInfo{huge}, nil)
	if err != nil {
		t.Fatalf("CompileDetect of the oversized still: %v", err)
	}
	if p.Width != 9000 || p.Height != 9000 || p.Frames != 1 || p.Filter != "[0:v]format=rgba[out]" {
		t.Errorf("oversized still plan: %dx%d frames %d %s", p.Width, p.Height, p.Frames, p.Filter)
	}
}

func TestDespillType(t *testing.T) {
	for hex, want := range map[string]string{
		"00ff00": "green", "10c020": "green", "0000ff": "blue", "2040ff": "blue",
		"ff0000": "", "ff00ff": "", "00ffff": "", "808080": "", "000000": "", "ffffff": "",
	} {
		if got := despillType(hex); got != want {
			t.Errorf("despillType(%s) = %q, want %q", hex, got, want)
		}
	}
}

func TestOverlayArgs(t *testing.T) {
	tests := []struct {
		name         string
		src          recipe.ProbeInfo
		loop         bool
		wantArgs     []string
		wantInfinite bool
	}{
		{"png still", ovPNG, false, []string{"-loop", "1"}, true},
		{"jpeg still", ovJPEG, false, []string{"-loop", "1"}, true},
		{"still webp (webp_pipe)", with(ovPNG, func(p *recipe.ProbeInfo) { p.Format, p.Codec = "webp_pipe", "webp" }), false, []string{"-loop", "1"}, true},
		{"image2 still", with(ovPNG, func(p *recipe.ProbeInfo) { p.Format = "image2" }), false, []string{"-loop", "1"}, true},
		{"one-frame gif", ovGIFStill, false, nil, false},
		{"one-frame apng", with(ovGIFStill, func(p *recipe.ProbeInfo) { p.Format, p.Codec = "apng", "apng" }), false, nil, false},
		{"still avif (mov demuxer)", avifAlpha, false, nil, false},
		{"still with an unknown format", with(ovPNG, func(p *recipe.ProbeInfo) { p.Format = "" }), false, nil, false},
		{"looping gif", ovGIF, true, []string{"-stream_loop", "-1"}, true},
		{"non-looping gif", ovGIF, false, nil, false},
		{"looping animated webp", ovWebP, true, []string{"-ignore_loop", "0"}, false},
		{"looping animated webp (old webp name)", with(ovWebP, func(p *recipe.ProbeInfo) { p.Format = "webp" }), true, []string{"-ignore_loop", "0"}, false},
		{"looping apng", ovAPNG, true, []string{"-ignore_loop", "0"}, false},
		{"non-looping apng", ovAPNG, false, nil, false},
		{"looping mp4", ovMP4, true, []string{"-stream_loop", "-1"}, true},
		{"looping webm vp9 alpha", vp9, true, []string{"-c:v", "libvpx-vp9", "-stream_loop", "-1"}, true},
		{"non-looping webm vp9 alpha", vp9, false, []string{"-c:v", "libvpx-vp9"}, false},
		{"looping opaque vp9", with(vp9, func(p *recipe.ProbeInfo) { p.HasAlpha = false }), true, []string{"-stream_loop", "-1"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			animated := tc.src.Frames > 1 || !tc.src.IsStill
			args, infinite := overlayArgs(tc.src, animated, tc.loop && animated)
			if !reflect.DeepEqual(args, tc.wantArgs) || infinite != tc.wantInfinite {
				t.Errorf("overlayArgs = %q, %v; want %q, %v", args, infinite, tc.wantArgs, tc.wantInfinite)
			}
		})
	}
	for format, want := range map[string]string{"mov,mp4,m4a,3gp,3g2,mj2": "mov", "gif": "gif", " WEBP_ANIM ": "webp_anim", "": ""} {
		if got := demuxerName(format); got != want {
			t.Errorf("demuxerName(%q) = %q, want %q", format, got, want)
		}
	}
}

// --- BindTextFiles ----------------------------------------------------------

func TestEscapeFilterPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/tmp/ezlg/job/t1.txt", "/tmp/ezlg/job/t1.txt"},
		{`C:\a\t.txt`, `C\\:\\\\a\\\\t.txt`},
		{`C:\Users\user\x y.txt`, `C\\:\\\\Users\\\\user\\\\x y.txt`},
		{"dir with space/t 'q'.txt", `dir with space/t \\\'q\\\'.txt`},
		{"a,b;c[d]", `a\,b\;c\[d\]`},
		{"k=v", "k=v"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := EscapeFilterPath(tc.in); got != tc.want {
			t.Errorf("EscapeFilterPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBindTextFiles(t *testing.T) {
	p := &Plan{
		InputArgs:   []string{"-ss", "1"},
		Filter:      "[0:v]fps=10:round=down,format=rgba," + text1 + "[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit + "," + text2 + ",format=rgba[out]",
		OutLabel:    "[out]",
		ExtraInputs: []ExtraInput{{Source: 1, Args: []string{"-loop", "1"}}},
		TextFiles:   []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "one"}, {Placeholder: "__EZLG_TEXT_2__", Content: "two"}},
	}
	orig := *p
	orig.Filter = p.Filter
	paths := []string{`C:\s\t1.txt`, "/data/scratch/job/t 'q'.txt"}
	bound, err := BindTextFiles(p, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := "[0:v]fps=10:round=down,format=rgba,drawtext=textfile=C\\\\:\\\\\\\\s\\\\\\\\t1.txt:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=0:y=0[b1];[1:v]format=rgba[ov1];[b1][ov1]" + ovComposit +
		",drawtext=textfile=/data/scratch/job/t \\\\\\'q\\\\\\'.txt:expansion=none:font=DejaVu Sans:fontsize=32:fontcolor=0xffffff:x=0:y=0,format=rgba[out]"
	if bound.Filter != want {
		t.Errorf("bound filter\n got: %s\nwant: %s", bound.Filter, want)
	}
	if bound == p || bound.TextFiles != nil || strings.Contains(bound.Filter, "__EZLG_TEXT_") {
		t.Errorf("bound plan must be a placeholder-free copy without TextFiles: %+v", bound)
	}
	if bound.OutLabel != "[out]" || !reflect.DeepEqual(bound.InputArgs, p.InputArgs) || !reflect.DeepEqual(bound.ExtraInputs, p.ExtraInputs) {
		t.Errorf("bound plan lost fields: %+v", bound)
	}
	// The copy shares no slices with the original.
	bound.InputArgs[0] = "-to"
	bound.ExtraInputs[0].Args[0] = "-x"
	if p.InputArgs[0] != "-ss" || p.ExtraInputs[0].Args[0] != "-loop" {
		t.Errorf("BindTextFiles shares slices with its input")
	}
	if p.Filter != orig.Filter || len(p.TextFiles) != 2 {
		t.Errorf("input plan modified: %+v", p)
	}

	// Errors.
	if _, err := BindTextFiles(p, paths[:1]); err == nil || !strings.Contains(err.Error(), "1 path(s) for 2 text file(s)") {
		t.Errorf("count mismatch: %v", err)
	}
	if _, err := BindTextFiles(p, []string{paths[0], ""}); err == nil || !strings.Contains(err.Error(), "text file 2 has an empty path") {
		t.Errorf("empty path: %v", err)
	}
	if _, err := BindTextFiles(nil, nil); err == nil {
		t.Error("nil plan must fail")
	}
	q := &Plan{Filter: "[0:v]format=rgba[out]", TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "x"}}}
	if _, err := BindTextFiles(q, []string{"/t.txt"}); err == nil || !strings.Contains(err.Error(), `placeholder "__EZLG_TEXT_1__" of text file 1 is not in the filter`) {
		t.Errorf("missing placeholder: %v", err)
	}
	// No text files: a plain copy.
	r := &Plan{Filter: "[0:v]format=rgba[out]", OutLabel: "[out]"}
	got, err := BindTextFiles(r, nil)
	if err != nil || got == r || got.Filter != r.Filter {
		t.Errorf("no text files: %+v %v", got, err)
	}
	// A compiled plan binds end to end.
	c, err := CompileWithSources([]recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "a\nb"})}, webp())
	if err != nil {
		t.Fatal(err)
	}
	b, err := BindTextFiles(c, []string{"/data/scratch/j/t1.txt"})
	if err != nil || !strings.Contains(b.Filter, "drawtext=textfile=/data/scratch/j/t1.txt:expansion=none") {
		t.Errorf("compiled plan: %v %s", err, b.Filter)
	}
	// A translucent text op repeats its placeholder on every layer; all of
	// them are bound to the one file.
	c, err = CompileWithSources([]recipe.ProbeInfo{h264}, []recipe.Op{text(recipe.TextParams{Text: "a", Box: true, Color: "ff000080", Border: 1})}, webp())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TextFiles) != 1 || strings.Count(c.Filter, "textfile=__EZLG_TEXT_1__") != 3 {
		t.Fatalf("layered text op: %d text files, filter %s", len(c.TextFiles), c.Filter)
	}
	b, err = BindTextFiles(c, []string{"/data/scratch/j/t1.txt"})
	if err != nil || strings.Count(b.Filter, "textfile=/data/scratch/j/t1.txt:expansion=none") != 3 || strings.Contains(b.Filter, "__EZLG_TEXT_") {
		t.Errorf("layered plan: %v %s", err, b.Filter)
	}
}
