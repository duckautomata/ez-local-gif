package graph_test

// Real-ffmpeg pixel checks of the Phase 3 recipes: chromakey/colorkey +
// despill, reverse, drawtext (font=, colours, anchors, enable, text-file
// path escaping) and overlays (every anchor, looping gif/apng/webp/video,
// hold of a non-looping overlay, opacity, base alpha, timing). External test
// package so internal/graph stays process-free; skips when ffmpeg is not on
// PATH. The drawtext checks additionally need a working fontconfig (the
// ezlg-dev image has fonts-dejavu; the gyan Windows build crashes inside
// fontconfig, so they skip there and run in Docker).
//
// Multi-input plans are rendered with an argv built here from the plan
// (InputArgs, the main input, every ExtraInput's Args + "-i", the filter),
// independent of enc's builders, so these tests pin the graph contract alone.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// p3Render runs the plan with ffmpeg: main is the main input path, extra
// the paths of p.ExtraInputs in order, texts the text-file paths bound with
// BindTextFiles (nil when the plan has none). Returns the RGBA frames.
// Killed after 60 s so a looping input that never ends cannot hang the
// suite.
func p3Render(t *testing.T, ff, main string, p *graph.Plan, extra []string, texts []string) [][]byte {
	t.Helper()
	if len(extra) != len(p.ExtraInputs) {
		t.Fatalf("plan has %d extra inputs, %d paths given", len(p.ExtraInputs), len(extra))
	}
	if len(p.TextFiles) > 0 {
		bound, err := graph.BindTextFiles(p, texts)
		if err != nil {
			t.Fatalf("BindTextFiles: %v", err)
		}
		p = bound
	}
	out := filepath.Join(t.TempDir(), "frames.rgba")
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
	args = append(args, p.InputArgs...)
	args = append(args, "-i", main)
	for i, in := range p.ExtraInputs {
		args = append(args, in.Args...)
		args = append(args, "-i", extra[i])
	}
	args = append(args, "-filter_complex", p.Filter, "-map", p.OutLabel, "-an", "-f", "rawvideo", "-pix_fmt", "rgba", out)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if o, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, o)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	frame := p.Width * p.Height * 4
	if len(data) == 0 || len(data)%frame != 0 {
		t.Fatalf("master is %d bytes, not a multiple of %dx%dx4", len(data), p.Width, p.Height)
	}
	frames := make([][]byte, 0, len(data)/frame)
	for i := 0; i+frame <= len(data); i += frame {
		frames = append(frames, data[i:i+frame])
	}
	return frames
}

// compileSrcs wraps graph.CompileWithSources with a fatal error check.
func compileSrcs(t *testing.T, srcs []recipe.ProbeInfo, ops []recipe.Op, out recipe.Output) *graph.Plan {
	t.Helper()
	p, err := graph.CompileWithSources(srcs, ops, out)
	if err != nil {
		t.Fatalf("CompileWithSources: %v", err)
	}
	return p
}

// writePNG writes img as a PNG file.
func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// solid returns a w x h image filled with c.
func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// pngClip encodes frames (all the same size) as a lossless png-in-mov clip
// at fps and returns its path and probe info (alpha as given).
func pngClip(t *testing.T, ff, dir, name string, frames []image.Image, fps int, hasAlpha bool) (string, recipe.ProbeInfo) {
	t.Helper()
	seq := filepath.Join(dir, name+"-frames")
	if err := os.MkdirAll(seq, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, f := range frames {
		writePNG(t, filepath.Join(seq, fmt.Sprintf("%06d.png", i+1)), f)
	}
	path := filepath.Join(dir, name+".mov")
	runFF(t, ff, []string{"-f", "image2", "-framerate", fmt.Sprint(fps), "-i", filepath.Join(seq, "%06d.png"), "-c:v", "png", "-pix_fmt", "rgba", path})
	b := frames[0].Bounds()
	info := recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "png", PixFmt: "rgba", Bits: 8,
		Width: b.Dx(), Height: b.Dy(), FPS: float64(fps), Duration: float64(len(frames)) / float64(fps), Frames: len(frames),
		HasAlpha: hasAlpha, Kind: recipe.KindVideo,
	}
	return path, info
}

// stillInfo is the probe of a PNG still.
func stillInfo(w, h int, alpha bool) recipe.ProbeInfo {
	return recipe.ProbeInfo{Format: "png_pipe", Codec: "png", PixFmt: "rgba", Bits: 8, Width: w, Height: h, Frames: 1, HasAlpha: alpha, IsStill: true, Kind: recipe.KindImage}
}

// isRGBA reports whether a pixel is (r,g,b,a) within tol per channel.
func isRGBA(p [4]byte, r, g, b, a byte, tol int) bool {
	return near(p[0], r, tol) && near(p[1], g, tol) && near(p[2], b, tol) && near(p[3], a, tol)
}

// --- keying -----------------------------------------------------------------

// keyFrame is a 16x16 frame on a solid screen colour with an 8x8 square of
// subject colour at (4,4) and a 1 px ring around it blended half/half with
// the screen (what an anti-aliased edge looks like), all opaque.
func keyFrame(screen, subject color.NRGBA) *image.NRGBA {
	img := solid(16, 16, screen)
	ring := color.NRGBA{R: (uint8(int(screen.R)+int(subject.R)) / 2), G: uint8((int(screen.G) + int(subject.G)) / 2), B: uint8((int(screen.B) + int(subject.B)) / 2), A: 255}
	for y := 3; y <= 12; y++ {
		for x := 3; x <= 12; x++ {
			if x >= 4 && x <= 11 && y >= 4 && y <= 11 {
				img.SetNRGBA(x, y, subject)
			} else {
				img.SetNRGBA(x, y, ring)
			}
		}
	}
	return img
}

func TestChromaKeyPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	green := color.NRGBA{G: 255, A: 255}
	blue := color.NRGBA{B: 255, A: 255}
	red := color.NRGBA{R: 255, A: 255}
	greenClip, greenInfo := pngClip(t, ff, dir, "green", []image.Image{keyFrame(green, red), keyFrame(green, red)}, 10, false)
	blueClip, blueInfo := pngClip(t, ff, dir, "blue", []image.Image{keyFrame(blue, red), keyFrame(blue, red)}, 10, false)
	out := recipe.Output{Format: "webp"}

	check := func(t *testing.T, frames [][]byte, w int, wantG, wantB byte, name string) {
		t.Helper()
		if len(frames) != 2 {
			t.Fatalf("%d frames, want 2", len(frames))
		}
		for i, f := range frames {
			if bg := pixel(f, w, 0, 0); bg[3] != 0 {
				t.Errorf("%s frame %d: background alpha %d, want 0 (%v)", name, i, bg[3], bg)
			}
			if c := pixel(f, w, 8, 8); !isRGBA(c, 255, 0, 0, 255, 2) {
				t.Errorf("%s frame %d: subject pixel %v, want opaque red", name, i, c)
			}
			e := pixel(f, w, 3, 8)
			if e[3] != 255 || !near(e[0], 128, 3) {
				t.Errorf("%s frame %d: edge pixel %v, want alpha 255 and R ~128", name, i, e)
			}
			if !near(e[1], wantG, 6) || !near(e[2], wantB, 6) {
				t.Errorf("%s frame %d: edge pixel %v, want G ~%d B ~%d", name, i, e, wantG, wantB)
			}
		}
	}

	t.Run("green screen: background keyed, subject intact, despill pulls the green out of the edge", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{greenInfo}, []recipe.Op{{Kind: recipe.OpChromaKey}}, out)
		if !p.HasAlpha || !strings.Contains(p.Filter, "format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05,despill=type=green:mix=0.6:expand=0.3") {
			t.Fatalf("plan: alpha %v filter %s", p.HasAlpha, p.Filter)
		}
		// The (128,128,0) ring keeps alpha 255 (its chroma is far from the
		// screen) and despill drops its green to 76 (measured on 9.0.1).
		check(t, p3Render(t, ff, greenClip, p, nil, nil), 16, 76, 0, "despill")
	})
	t.Run("despill off leaves the green fringe", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{greenInfo}, []recipe.Op{{Kind: recipe.OpChromaKey, Params: []byte(`{"despillOff":true}`)}}, out)
		check(t, p3Render(t, ff, greenClip, p, nil, nil), 16, 128, 0, "no despill")
	})
	t.Run("blue screen despills blue", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{blueInfo}, []recipe.Op{{Kind: recipe.OpChromaKey, Params: []byte(`{"color":"0000ff"}`)}}, out)
		if !strings.Contains(p.Filter, "despill=type=blue:mix=0.6:expand=0.3:green=0:blue=-1,") {
			t.Fatalf("filter: %s", p.Filter)
		}
		check(t, p3Render(t, ff, blueClip, p, nil, nil), 16, 0, 76, "blue despill")
	})
	t.Run("keying happens before the crop, at full resolution", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{greenInfo}, []recipe.Op{{Kind: recipe.OpCrop, Params: []byte(`{"x":2,"y":2,"w":12,"h":12}`)}, {Kind: recipe.OpChromaKey}}, out)
		if p.Width != 12 || !strings.Contains(p.Filter, ",despill=type=green:mix=0.6:expand=0.3,crop=12:12:2:2:exact=1,") {
			t.Fatalf("plan: %dx%d %s", p.Width, p.Height, p.Filter)
		}
		frames := p3Render(t, ff, greenClip, p, nil, nil)
		if bg := pixel(frames[0], 12, 0, 0); bg[3] != 0 {
			t.Errorf("cropped background alpha %d, want 0", bg[3])
		}
		if c := pixel(frames[0], 12, 6, 6); !isRGBA(c, 255, 0, 0, 255, 2) {
			t.Errorf("cropped subject %v, want opaque red", c)
		}
	})
}

func TestColorKeyPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	green := color.NRGBA{G: 255, A: 255}
	red := color.NRGBA{R: 255, A: 255}
	clip, info := pngClip(t, ff, dir, "green", []image.Image{keyFrame(green, red), keyFrame(green, red)}, 10, false)
	out := recipe.Output{Format: "webp"}

	t.Run("exact colour keyed, edge and subject untouched", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{{Kind: recipe.OpColorKey, Params: []byte(`{"color":"#00FF00"}`)}}, out)
		if !p.HasAlpha || !strings.Contains(p.Filter, "format=rgba,colorkey=color=0x00ff00:similarity=0.1:blend=0,") {
			t.Fatalf("plan: alpha %v filter %s", p.HasAlpha, p.Filter)
		}
		frames := p3Render(t, ff, clip, p, nil, nil)
		if len(frames) != 2 {
			t.Fatalf("%d frames, want 2", len(frames))
		}
		for i, f := range frames {
			if bg := pixel(f, 16, 0, 0); bg[3] != 0 {
				t.Errorf("frame %d: background alpha %d, want 0", i, bg[3])
			}
			if c := pixel(f, 16, 8, 8); !isRGBA(c, 255, 0, 0, 255, 1) {
				t.Errorf("frame %d: subject %v, want opaque red", i, c)
			}
			if e := pixel(f, 16, 3, 8); !isRGBA(e, 128, 128, 0, 255, 1) {
				t.Errorf("frame %d: edge %v, want (128,128,0,255) — colorkey has no despill", i, e)
			}
		}
	})
	t.Run("blend softens the edge alpha", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{{Kind: recipe.OpColorKey, Params: []byte(`{"color":"00ff00","similarity":0.3,"blend":0.2}`)}}, out)
		f := p3Render(t, ff, clip, p, nil, nil)[0]
		if bg := pixel(f, 16, 0, 0); bg[3] != 0 {
			t.Errorf("background alpha %d, want 0", bg[3])
		}
		if e := pixel(f, 16, 3, 8); e[3] == 0 || e[3] == 255 {
			t.Errorf("edge alpha %d, want partial (blend)", e[3])
		}
		if c := pixel(f, 16, 8, 8); c[3] != 255 {
			t.Errorf("subject alpha %d, want 255", c[3])
		}
	})
}

// alphaProbe is a 16x16 transparent frame with three opaque-or-not 4x4
// blocks: half-alpha red at (6,2), opaque green (the key colour) at (10,2)
// and opaque red at (2,10). Interior pixels of each block — and of the
// transparent background at (3,3) — have a uniform 3x3 neighbourhood, so
// chromakey's neighbourhood average reads one colour there.
func alphaProbe() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	block := func(x0, y0 int, c color.NRGBA) {
		for y := y0; y < y0+4; y++ {
			for x := x0; x < x0+4; x++ {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	block(6, 2, color.NRGBA{R: 255, A: 128})
	block(10, 2, color.NRGBA{G: 255, A: 255})
	block(2, 10, color.NRGBA{R: 255, A: 255})
	return img
}

// TestKeyKeepsAlphaPixels: ffmpeg's chromakey and colorkey assign the alpha
// plane from the colour distance alone, so a bare key on a source that
// already carries transparency turned every transparent pixel whose colour
// is not the key colour opaque ((0,0,0,0) → (0,0,0,255)) and hardened
// half-alpha edges to 255 (verified on FFmpeg 9.0.1 — the gyan build, the
// 2026-08 git build and the ezlg-dev image agree). The compiled wrapper
// (keyKeepingAlpha) intersects the incoming alpha with the key's matte:
// transparent stays 0, the half-alpha edge stays 128, the key colour goes
// to 0 and the opaque subject stays 255.
func TestKeyKeepsAlphaPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	out := recipe.Output{Format: "webp"}
	chroma := recipe.Op{Kind: recipe.OpChromaKey}
	colorGreen := recipe.Op{Kind: recipe.OpColorKey, Params: []byte(`{"color":"00ff00"}`)}
	wrapper := "split[k1][k1m];[k1m]alphaextract[k1a0];[k1]"

	// checkProbe asserts the four probe pixels of every frame; tol is the
	// colour tolerance (the chromakey chain's yuva444p round trip costs 1,
	// a lossy ProRes source a few more), atol the alpha tolerance.
	checkProbe := func(t *testing.T, frames [][]byte, name string, tol, atol int) {
		t.Helper()
		if len(frames) == 0 {
			t.Fatalf("%s: no frames", name)
		}
		for i, f := range frames {
			if px := pixel(f, 16, 3, 3); !near(px[3], 0, atol) {
				t.Errorf("%s frame %d: transparent pixel %v, want alpha 0 (the key overwrote the source alpha)", name, i, px)
			}
			if px := pixel(f, 16, 7, 3); !near(px[3], 128, atol) || !near(px[0], 255, tol) || !near(px[1], 0, tol) || !near(px[2], 0, tol) {
				t.Errorf("%s frame %d: half-alpha edge %v, want (255,0,0,128)", name, i, px)
			}
			if px := pixel(f, 16, 11, 3); !near(px[3], 0, atol) {
				t.Errorf("%s frame %d: key-colour pixel %v, want alpha 0 (keyed)", name, i, px)
			}
			if px := pixel(f, 16, 3, 11); !near(px[3], 255, atol) || !near(px[0], 255, tol) || !near(px[1], 0, tol) || !near(px[2], 0, tol) {
				t.Errorf("%s frame %d: subject pixel %v, want opaque red", name, i, px)
			}
		}
	}

	clip, info := pngClip(t, ff, dir, "alpha", []image.Image{alphaProbe(), alphaProbe()}, 10, true)
	for _, tc := range []struct {
		name string
		op   recipe.Op
		key  string // the bare key stages the wrapper must carry
	}{
		{"chromakey", chroma, "[k1]format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05,despill=type=green:mix=0.6:expand=0.3,format=rgba,split[k1k][k1km];"},
		{"colorkey", colorGreen, "[k1]format=rgba,colorkey=color=0x00ff00:similarity=0.1:blend=0,split[k1k][k1km];"},
	} {
		t.Run(tc.name+" on an rgba source keeps its alpha", func(t *testing.T) {
			p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{tc.op}, out)
			if !p.HasAlpha || !strings.Contains(p.Filter, "fps=10:round=down,"+wrapper) || !strings.Contains(p.Filter, tc.key) {
				t.Fatalf("plan: alpha %v filter %s", p.HasAlpha, p.Filter)
			}
			frames := p3Render(t, ff, clip, p, nil, nil)
			if len(frames) != 2 {
				t.Fatalf("%d frames, want 2", len(frames))
			}
			checkProbe(t, frames, tc.name, 2, 0)
		})
	}

	t.Run("chromakey on a 10-bit ProRes 4444 source (the yuva head converts to rgba before the split)", func(t *testing.T) {
		if !hasCodec(t, ff, "-encoders", "prores_ks") {
			t.Skip("ffmpeg has no prores_ks encoder")
		}
		png := filepath.Join(dir, "probe.png")
		writePNG(t, png, alphaProbe())
		mov := filepath.Join(dir, "probe.mov")
		proresClip(t, ff, png, mov)
		pinfo := recipe.ProbeInfo{
			Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p12le", Bits: 12,
			Width: 16, Height: 16, FPS: 10, Duration: float64(discFrames) / 10, Frames: discFrames,
			HasAlpha: true, Kind: recipe.KindVideo,
		}
		p := compileSrcs(t, []recipe.ProbeInfo{pinfo}, []recipe.Op{chroma}, out)
		if !strings.Contains(p.Filter, "[0:v]format=rgba,fps=10:round=down,"+wrapper) {
			t.Fatalf("filter: %s", p.Filter)
		}
		// The planar-YUV alpha head turns the tv-range 12-bit decode into
		// exact 8-bit rgba alpha, so the transparent pixel is exactly 0 and
		// the two mattes meet at 8 bits; only the colour is lossy.
		frames := p3Render(t, ff, mov, p, nil, nil)
		if len(frames) != discFrames {
			t.Fatalf("%d frames, want %d", len(frames), discFrames)
		}
		checkProbe(t, frames, "prores", 10, 1)
		for i, f := range frames {
			if px := pixel(f, 16, 3, 3); px[3] != 0 {
				t.Errorf("frame %d: transparent pixel %v, want alpha exactly 0 off the yuva head", i, px)
			}
		}
	})

	t.Run("a key on a wrapped key's output keeps the first matte", func(t *testing.T) {
		// Both keys on the transparent probe: the second wrapper (labels k2)
		// keys the opaque red subject and must leave the transparent
		// background and the already keyed green at 0.
		colorRed := recipe.Op{Kind: recipe.OpColorKey, Params: []byte(`{"color":"ff0000"}`)}
		p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{chroma, colorRed}, out)
		if !strings.Contains(p.Filter, "alphamerge,split[k2][k2m];[k2m]alphaextract[k2a0];[k2]format=rgba,colorkey=color=0xff0000") {
			t.Fatalf("filter: %s", p.Filter)
		}
		for i, f := range p3Render(t, ff, clip, p, nil, nil) {
			for _, pt := range [][2]int{{3, 3}, {11, 3}, {3, 11}, {7, 3}} {
				if px := pixel(f, 16, pt[0], pt[1]); px[3] != 0 {
					t.Errorf("frame %d: (%d,%d) = %v, want alpha 0", i, pt[0], pt[1], px)
				}
			}
		}
	})

	t.Run("stacked keys on an opaque green screen: the second key is wrapped and the keyed background stays transparent", func(t *testing.T) {
		green := color.NRGBA{G: 255, A: 255}
		red := color.NRGBA{R: 255, A: 255}
		screen, sinfo := pngClip(t, ff, dir, "screen", []image.Image{keyFrame(green, red), keyFrame(green, red)}, 10, false)
		// A second key of a colour that is not in the picture changes nothing:
		// the background keyed by the first stays transparent (a bare second
		// key brought it back to 255), the subject and its edge stay opaque.
		blue := recipe.Op{Kind: recipe.OpColorKey, Params: []byte(`{"color":"0000ff"}`)}
		p := compileSrcs(t, []recipe.ProbeInfo{sinfo}, []recipe.Op{chroma, blue}, out)
		if !strings.Contains(p.Filter, "fps=10:round=down,format=yuva444p,chromakey=") || !strings.Contains(p.Filter, "expand=0.3,"+wrapper+"format=rgba,colorkey=color=0x0000ff") {
			t.Fatalf("filter: %s", p.Filter)
		}
		for i, f := range p3Render(t, ff, screen, p, nil, nil) {
			if bg := pixel(f, 16, 0, 0); bg[3] != 0 {
				t.Errorf("frame %d: background %v, want alpha 0 (the second key brought the screen back)", i, bg)
			}
			if c := pixel(f, 16, 8, 8); !isRGBA(c, 255, 0, 0, 255, 2) {
				t.Errorf("frame %d: subject %v, want opaque red", i, c)
			}
			if e := pixel(f, 16, 3, 8); e[3] != 255 {
				t.Errorf("frame %d: edge %v, want alpha 255", i, e)
			}
		}
		// A second key of the subject's colour removes the subject and keeps
		// the background transparent.
		keyRed := recipe.Op{Kind: recipe.OpColorKey, Params: []byte(`{"color":"ff0000","similarity":0.05}`)}
		p = compileSrcs(t, []recipe.ProbeInfo{sinfo}, []recipe.Op{chroma, keyRed}, out)
		for i, f := range p3Render(t, ff, screen, p, nil, nil) {
			if bg := pixel(f, 16, 0, 0); bg[3] != 0 {
				t.Errorf("frame %d: background %v, want alpha 0", i, bg)
			}
			if c := pixel(f, 16, 8, 8); c[3] != 0 {
				t.Errorf("frame %d: subject %v, want alpha 0 (keyed by the second key)", i, c)
			}
		}
	})
}

// --- reverse ----------------------------------------------------------------

func TestReversePixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	const n = 6
	frames := make([]image.Image, n)
	for i := range frames {
		frames[i] = solid(16, 16, color.NRGBA{R: uint8(i + 1), G: 200, A: 255})
	}
	clip, info := pngClip(t, ff, dir, "idx", frames, 10, false)
	out := recipe.Output{Format: "webp"}
	rev := recipe.Op{Kind: recipe.OpReverse}
	index := func(f []byte, w int) int { return int(pixel(f, w, w/2, w/2)[0]) }

	cases := []struct {
		name string
		ops  []recipe.Op
		out  recipe.Output
		w    int
	}{
		{"whole clip", []recipe.Op{rev}, out, 16},
		{"trim 0.2..0.5 then reverse (frames 3..5 backwards)", []recipe.Op{{Kind: recipe.OpTrim, Params: []byte(`{"start":0.2,"end":0.5}`)}, rev}, out, 16},
		{"reverse then the output fit", []recipe.Op{rev}, recipe.Output{Format: "webp", Width: 8, Height: 8}, 8},
		{"fps 5 then reverse", []recipe.Op{{Kind: recipe.OpFPS, Params: []byte(`{"fps":5}`)}, rev}, out, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forward := compileSrcs(t, []recipe.ProbeInfo{info}, tc.ops[:len(tc.ops)-1], tc.out)
			reversed := compileSrcs(t, []recipe.ProbeInfo{info}, tc.ops, tc.out)
			if !reversed.Reversed || forward.Reversed || !strings.Contains(reversed.Filter, ",reverse,") {
				t.Fatalf("Reversed flags %v/%v, filter %s", forward.Reversed, reversed.Reversed, reversed.Filter)
			}
			if reversed.Frames != forward.Frames || reversed.Duration != forward.Duration {
				t.Errorf("reverse changed the plan: %d/%v vs %d/%v", reversed.Frames, reversed.Duration, forward.Frames, forward.Duration)
			}
			fw := p3Render(t, ff, clip, forward, nil, nil)
			rv := p3Render(t, ff, clip, reversed, nil, nil)
			if len(fw) != forward.Frames || len(rv) != reversed.Frames {
				t.Fatalf("rendered %d/%d frames, plan %d", len(fw), len(rv), forward.Frames)
			}
			for i := range rv {
				if want, got := index(fw[len(fw)-1-i], tc.w), index(rv[i], tc.w); got != want {
					t.Errorf("reversed frame %d is source frame %d, want %d", i, got, want)
				}
			}
			if len(fw) > 1 && index(fw[0], tc.w) >= index(fw[len(fw)-1], tc.w) {
				t.Errorf("forward render is not in order: %d..%d", index(fw[0], tc.w), index(fw[len(fw)-1], tc.w))
			}
		})
	}
}

// --- drawtext ---------------------------------------------------------------

// fontconfigOrSkip skips when drawtext's fontconfig font= option cannot be
// used with this ffmpeg (the gyan Windows build aborts inside fontconfig;
// the ezlg-dev image works).
func fontconfigOrSkip(t *testing.T, ff string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ff, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=16x16:r=10:d=0.1",
		"-vf", "drawtext=font=Sans:text=x:fontsize=8", "-frames:v", "1", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Skipf("drawtext font= is unusable with this ffmpeg (%v): %s", err, out)
	}
}

// countRed counts the red-ish pixels (R > 100, B < 100) of a frame inside
// the rectangle [x0,x1) x [y0,y1).
func countRed(f []byte, w, x0, y0, x1, y1 int) int {
	n := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if p := pixel(f, w, x, y); p[0] > 100 && p[2] < 100 {
				n++
			}
		}
	}
	return n
}

// textDir returns a directory whose name needs escaping (space and quote;
// on Windows the drive colon too) for the text files, and the path of the
// i-th text file in it.
func textDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "job 'q'")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDrawTextPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	fontconfigOrSkip(t, ff)
	dir := t.TempDir()
	blue := color.NRGBA{B: 255, A: 255}
	var blueFrames []image.Image
	for i := 0; i < 5; i++ {
		blueFrames = append(blueFrames, solid(64, 32, blue))
	}
	clip, info := pngClip(t, ff, dir, "blue", blueFrames, 10, false)
	clear, clearInfo := pngClip(t, ff, dir, "clear", []image.Image{solid(64, 32, color.NRGBA{})}, 10, true)
	out := recipe.Output{Format: "webp"}
	const w = 64

	render := func(t *testing.T, src string, src0 recipe.ProbeInfo, ops []recipe.Op) (*graph.Plan, [][]byte) {
		t.Helper()
		p := compileSrcs(t, []recipe.ProbeInfo{src0}, ops, out)
		td := textDir(t)
		paths := make([]string, len(p.TextFiles))
		for i, tf := range p.TextFiles {
			paths[i] = filepath.Join(td, fmt.Sprintf("t%d.txt", i+1))
			if err := os.WriteFile(paths[i], []byte(tf.Content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return p, p3Render(t, ff, src, p, nil, paths)
	}
	textOp := func(params string) recipe.Op { return recipe.Op{Kind: recipe.OpText, Params: []byte(params)} }

	t.Run("red text at the top-left through a text file in an escaped path", func(t *testing.T) {
		p, frames := render(t, clip, info, []recipe.Op{textOp(`{"text":"Hi","color":"ff0000","size":20,"x":2,"y":2}`)})
		if !strings.Contains(p.Filter, "drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=20:fontcolor=0xff0000:x=2:y=2") {
			t.Fatalf("filter: %s", p.Filter)
		}
		if len(frames) != 5 {
			t.Fatalf("%d frames, want 5", len(frames))
		}
		for i, f := range frames {
			if n := countRed(f, w, 0, 0, 32, 32); n < 20 {
				t.Errorf("frame %d: %d red pixels in the text region, want >= 20 (nothing drawn?)", i, n)
			}
			if n := countRed(f, w, 40, 0, 64, 32); n != 0 {
				t.Errorf("frame %d: %d red pixels far from the text", i, n)
			}
		}
	})
	t.Run("bottom-right anchor lands in the bottom-right", func(t *testing.T) {
		_, frames := render(t, clip, info, []recipe.Op{textOp(`{"text":"Hi","color":"ff0000","size":20,"x":64,"y":32,"anchor":"br"}`)})
		f := frames[0]
		if n := countRed(f, w, 32, 8, 64, 32); n < 20 {
			t.Errorf("%d red pixels in the bottom-right, want >= 20", n)
		}
		if n := countRed(f, w, 0, 0, 32, 8); n != 0 {
			t.Errorf("%d red pixels in the top-left", n)
		}
	})
	maxA := func(f []byte) byte {
		var m byte
		for i := 3; i < len(f); i += 4 {
			m = max(m, f[i])
		}
		return m
	}
	const layerTail = ",format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba"
	t.Run("RRGGBBAA colour renders at exactly its alpha over a clear base", func(t *testing.T) {
		p, half := render(t, clear, clearInfo, []recipe.Op{textOp(`{"text":"Hi","color":"ff000080","size":20,"x":2,"y":2}`)})
		if !strings.Contains(p.Filter, "[b1];color=c=0x00000000:s=64x32:r=10,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=20:fontcolor=0xff0000:x=2:y=2"+layerTail+",colorchannelmixer=aa=0.502[t1];[b1][t1]overlay=format=auto:shortest=1:eof_action=repeat,format=rgba[out]") {
			t.Fatalf("filter: %s", p.Filter)
		}
		_, full := render(t, clear, clearInfo, []recipe.Op{textOp(`{"text":"Hi","color":"ff0000","size":20,"x":2,"y":2}`)})
		// The layer is drawn opaque and its alpha scaled by 128/255, so a
		// fully covered glyph pixel lands at 128 (was a*a/255 = 64 when
		// drawtext blended the alpha plane itself).
		if a := maxA(half[0]); !near(a, 128, 2) {
			t.Errorf("max alpha %d with fontcolor 0xff000080, want 128", a)
		}
		if a := maxA(full[0]); a != 255 {
			t.Errorf("max alpha %d with an opaque colour, want 255", a)
		}
	})
	t.Run("translucent white glyphs over a clear base keep their colour, edges included", func(t *testing.T) {
		// The layer leaves drawtext premultiplied (white at coverage c is
		// (255c,255c,255c,255c)); the layer tail makes it straight, so every
		// glyph pixel — anti-aliased edges too — is pure white at its
		// coverage, not darkened towards the transparent base's black (what
		// in-place drawtext left there: a dark fringe).
		_, frames := render(t, clear, clearInfo, []recipe.Op{textOp(`{"text":"Hi","color":"ffffff80","size":24,"x":2,"y":2}`)})
		f := frames[0]
		if a := maxA(f); !near(a, 128, 2) {
			t.Errorf("max alpha %d, want 128", a)
		}
		covered, edges := 0, 0
		for i := 0; i+3 < len(f); i += 4 {
			switch a := f[i+3]; {
			case a == 0:
				continue
			case a >= 100:
				covered++
			default:
				edges++
			}
			if f[i] < 250 || f[i+1] < 250 || f[i+2] < 250 {
				t.Errorf("pixel %d: %v under a white glyph, want (255,255,255,a)", i/4, f[i:i+4])
				break
			}
		}
		if covered < 4 || edges < 10 {
			t.Errorf("%d covered and %d edge glyph pixels, want >= 4 / >= 10", covered, edges)
		}
	})
	t.Run("default box on an opaque base: the canvas stays opaque, the box is half black, the glyphs match in-place drawtext", func(t *testing.T) {
		// The box layer (alpha 128) composites over opaque blue: every pixel
		// keeps alpha 255 and a box pixel away from the glyphs is
		// (0,0,127). In-place drawtext turned the box pixels translucent
		// (alpha 255*(1-0.5)+0.5*128 = 191).
		p, frames := render(t, clip, info, []recipe.Op{textOp(`{"text":"Hi","color":"ff0000","size":20,"x":2,"y":2,"box":true}`)})
		if !strings.Contains(p.Filter, "[b1];color=c=0x00000000:s=64x32:r=10,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=20:fontcolor=0x00000000:x=2:y=2:box=1:boxcolor=0x000000:boxborderw=8"+layerTail+",colorchannelmixer=aa=0.502[t1];[b1][t1]overlay=format=auto:shortest=1:eof_action=repeat[b2];color=c=0x00000000:s=64x32:r=10,format=rgba,drawtext=textfile=__EZLG_TEXT_1__:expansion=none:font=DejaVu Sans:fontsize=20:fontcolor=0xff0000:x=2:y=2"+layerTail+"[t2];[b2][t2]overlay=format=auto:shortest=1:eof_action=repeat,format=rgba[out]") {
			t.Fatalf("filter: %s", p.Filter)
		}
		if len(frames) != 5 {
			t.Fatalf("%d frames, want 5", len(frames))
		}
		for i, f := range frames {
			for j := 3; j < len(f); j += 4 {
				if f[j] != 255 {
					t.Errorf("frame %d: pixel %d alpha %d, want 255 (the box made the opaque canvas translucent)", i, j/4, f[j])
					break
				}
			}
			// (0,0) lies inside the box (padding 8 around the text at 2,2)
			// and outside every glyph.
			if px := pixel(f, w, 0, 0); !isRGBA(px, 0, 0, 127, 255, 2) {
				t.Errorf("frame %d: box pixel %v, want (0,0,127,255)", i, px)
			}
			if px := pixel(f, w, 63, 0); !isBlue(px) || px[3] != 255 {
				t.Errorf("frame %d: pixel outside the box %v, want opaque blue", i, px)
			}
			if n := countRed(f, w, 0, 0, 32, 32); n < 20 {
				t.Errorf("frame %d: %d red glyph pixels, want >= 20", i, n)
			}
		}
		// Half black over blue is (0,0,127), so the same text with an OPAQUE
		// box of that colour, which drawtext draws in place, must render
		// pixel for pixel the same — anti-aliased glyph edges included (a
		// straight composite of the premultiplied layer darkened them by the
		// coverage a second time: (15,15,111) where in place gives (61,61,127)).
		q, inPlace := render(t, clip, info, []recipe.Op{textOp(`{"text":"Hi","color":"ff0000","size":20,"x":2,"y":2,"box":true,"boxColor":"00007f"}`)})
		if strings.Contains(q.Filter, "[t1]") || !strings.Contains(q.Filter, ":box=1:boxcolor=0x00007f:boxborderw=8,format=rgba[out]") {
			t.Fatalf("opaque box must draw in place: %s", q.Filter)
		}
		for i := 0; i+3 < len(inPlace[0]); i += 4 {
			a, b := frames[0][i:i+4], inPlace[0][i:i+4]
			for k := 0; k < 4; k++ {
				if !near(a[k], b[k], 2) {
					t.Errorf("pixel %d: layered %v, in place %v", i/4, a, b)
					i = len(inPlace[0])
					break
				}
			}
		}
	})
	t.Run("default box over a half-clear base: alpha 128 over clear, 255 over opaque", func(t *testing.T) {
		half := image.NewNRGBA(image.Rect(0, 0, 64, 32))
		for y := 0; y < 32; y++ {
			for x := 32; x < 64; x++ {
				half.SetNRGBA(x, y, blue)
			}
		}
		halfClip, halfInfo := pngClip(t, ff, dir, "half", []image.Image{half}, 10, true)
		// A box padded by 30 px around a small centred text covers the whole
		// 64x32 frame (clipped by drawtext).
		_, frames := render(t, halfClip, halfInfo, []recipe.Op{textOp(`{"text":"Hi","size":10,"x":32,"y":16,"anchor":"mc","box":true,"boxPad":30}`)})
		f := frames[0]
		if px := pixel(f, w, 2, 2); !isRGBA(px, 0, 0, 0, 128, 2) {
			t.Errorf("box over clear: %v, want (0,0,0,128)", px)
		}
		if px := pixel(f, w, 61, 2); !isRGBA(px, 0, 0, 127, 255, 2) {
			t.Errorf("box over opaque blue: %v, want (0,0,127,255)", px)
		}
	})
	t.Run("text layers on a still base yield its single frame", func(t *testing.T) {
		bluePNG := filepath.Join(dir, "blue16.png")
		writePNG(t, bluePNG, solid(16, 16, blue))
		// drawtext's y=0 is the top of the glyph box itself, so the "x" at
		// (4,4) covers (4..8, 4..7) and the box, padded by 2, starts at (2,2).
		p := compileSrcs(t, []recipe.ProbeInfo{stillInfo(16, 16, false)}, []recipe.Op{textOp(`{"text":"x","size":8,"x":4,"y":4,"box":true,"boxPad":2}`)}, recipe.Output{Format: recipe.FormatPNG})
		if p.Frames != 1 || !strings.Contains(p.Filter, "[b1];color=c=0x00000000:s=16x16:r=10,format=rgba,drawtext=") {
			t.Fatalf("plan: frames %d filter %s", p.Frames, p.Filter)
		}
		td := textDir(t)
		path := filepath.Join(td, "t1.txt")
		if err := os.WriteFile(path, []byte(p.TextFiles[0].Content), 0o644); err != nil {
			t.Fatal(err)
		}
		frames := p3Render(t, ff, bluePNG, p, nil, []string{path})
		if len(frames) != 1 {
			t.Fatalf("%d frames, want 1 (the infinite colour source must not extend a still)", len(frames))
		}
		if px := pixel(frames[0], 16, 15, 15); !isBlue(px) || px[3] != 255 {
			t.Errorf("corner %v, want opaque blue", px)
		}
		if px := pixel(frames[0], 16, 2, 2); !isRGBA(px, 0, 0, 127, 255, 2) {
			t.Errorf("box pixel %v, want (0,0,127,255)", px)
		}
	})
	t.Run("time range gates the frames: start inclusive, end exclusive", func(t *testing.T) {
		// The window [0.1, 0.4) on frames at t = 0, 0.1, 0.2, 0.3, 0.4: the
		// frame AT the start is drawn, the frame AT the end is not (the UI's
		// "from scrubber" end is the start of the next frame). between()
		// would draw frame 4 too (verified on FFmpeg 9.0.1).
		p, frames := render(t, clip, info, []recipe.Op{textOp(`{"text":"Hi","color":"ff0000","size":20,"x":2,"y":2,"start":0.1,"end":0.4}`)})
		if !strings.Contains(p.Filter, ":enable='gte(t+0.0001,0.1)*lt(t+0.0001,0.4)'") {
			t.Fatalf("filter: %s", p.Filter)
		}
		if len(frames) != 5 {
			t.Fatalf("%d frames, want 5", len(frames))
		}
		for i, f := range frames {
			want := i >= 1 && i <= 3
			if got := countRed(f, w, 0, 0, 64, 32) > 0; got != want {
				t.Errorf("frame %d (t=%.1f): text drawn %v, want %v", i, float64(i)/10, got, want)
			}
		}
	})
	t.Run("a frame-grid window at 30 fps keeps its first frame and drops its end frame", func(t *testing.T) {
		// The UI's "from scrubber" bounds are round6(k/fps): at 30 fps
		// 2/30 → 0.066667 lies just ABOVE frame 2's raw timestamp
		// 0.0666666…, so a plain gte(t,S) skipped frame 2 and lt(t,E) drew
		// the frame at an end of 5/30 → 0.166667. The tolerance in the
		// expression absorbs the rounding (verified on FFmpeg 9.0.1).
		var frames30 []image.Image
		for i := 0; i < 8; i++ {
			frames30 = append(frames30, solid(64, 32, blue))
		}
		clip30, info30 := pngClip(t, ff, dir, "blue30", frames30, 30, false)
		for _, tc := range []struct {
			window     string
			start, end int // drawn frames [start, end)
		}{
			{`"start":0.066667,"end":0.2`, 2, 6},
			{`"start":0.1,"end":0.166667`, 3, 5},
			{`"start":0.066667`, 2, 8},
		} {
			p, frames := render(t, clip30, info30, []recipe.Op{textOp(`{"text":"Hi","color":"ff0000","size":20,"x":2,"y":2,` + tc.window + `}`)})
			if len(frames) != 8 {
				t.Fatalf("%s: %d frames, want 8 (filter %s)", tc.window, len(frames), p.Filter)
			}
			for i, f := range frames {
				want := i >= tc.start && i < tc.end
				if got := countRed(f, w, 0, 0, 64, 32) > 0; got != want {
					t.Errorf("window %s: frame %d (t=%d/30): text drawn %v, want %v", tc.window, i, i, got, want)
				}
			}
		}
	})
	t.Run("box, border and two lines; a second text op draws too", func(t *testing.T) {
		_, frames := render(t, clip, info, []recipe.Op{
			textOp(`{"text":"Hi\nYo","color":"ff0000","size":10,"x":2,"y":2,"box":true,"boxColor":"00ff00","boxPad":2,"border":1,"borderColor":"000000","lineSpacing":2}`),
			textOp(`{"text":"Yo","color":"ff0000","size":10,"x":64,"y":32,"anchor":"br"}`),
		})
		f := frames[0]
		green := 0
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				if p := pixel(f, w, x, y); p[1] > 200 && p[0] < 50 && p[2] < 50 {
					green++
				}
			}
		}
		if green < 10 {
			t.Errorf("%d green box pixels, want >= 10", green)
		}
		if n := countRed(f, w, 0, 0, 32, 32); n < 10 {
			t.Errorf("%d red text pixels in the first text, want >= 10", n)
		}
		if n := countRed(f, w, 40, 16, 64, 32); n < 10 {
			t.Errorf("%d red text pixels in the second text, want >= 10", n)
		}
	})
	t.Run("text is printed verbatim (no expansion)", func(t *testing.T) {
		// "%{n}" would expand to the frame number and "\" vanish under
		// drawtext's default expansion; with expansion=none the frames look
		// identical and contain the literal characters.
		_, frames := render(t, clip, info, []recipe.Op{textOp(`{"text":"a\\b %{n}","color":"ff0000","size":12,"x":2,"y":2}`)})
		for i := 1; i < len(frames); i++ {
			if !bytes.Equal(frames[i], frames[0]) {
				t.Errorf("frame %d differs from frame 0: a %%{n} was expanded", i)
			}
		}
	})
}

// --- overlays ---------------------------------------------------------------

// twoFrameGIF writes a 4x4 two-frame gif (red, then lime; 100 ms each).
// loop false omits the NETSCAPE loop block (play once).
func twoFrameGIF(t *testing.T, path string, loop bool) {
	t.Helper()
	pal := color.Palette{color.NRGBA{R: 255, A: 255}, color.NRGBA{G: 255, A: 255}}
	g := &gif.GIF{LoopCount: 0}
	if !loop {
		g.LoopCount = -1
	}
	for i := 0; i < 2; i++ {
		img := image.NewPaletted(image.Rect(0, 0, 4, 4), pal)
		for k := range img.Pix {
			img.Pix[k] = uint8(i)
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gif.EncodeAll(f, g); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

var (
	gifInfo = recipe.ProbeInfo{Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8, Width: 4, Height: 4, FPS: 10, Duration: 0.2, Frames: 2, HasAlpha: false, Kind: recipe.KindAnimation}
	// isRed/isLime classify the two overlay frames.
	isRed  = func(p [4]byte) bool { return p[0] > 200 && p[1] < 50 && p[2] < 50 && p[3] == 255 }
	isLime = func(p [4]byte) bool { return p[1] > 200 && p[0] < 50 && p[2] < 50 && p[3] == 255 }
	isBlue = func(p [4]byte) bool { return p[2] > 200 && p[0] < 50 && p[1] < 50 }
)

func TestOverlayPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	blue := color.NRGBA{B: 255, A: 255}
	var base6 []image.Image
	for i := 0; i < 6; i++ {
		base6 = append(base6, solid(16, 16, blue))
	}
	base, baseInfo := pngClip(t, ff, dir, "base", base6, 10, false)
	red := filepath.Join(dir, "red.png")
	writePNG(t, red, solid(2, 2, color.NRGBA{R: 255, A: 255}))
	redInfo := stillInfo(2, 2, false)
	gifLoop, gifOnce := filepath.Join(dir, "loop.gif"), filepath.Join(dir, "once.gif")
	twoFrameGIF(t, gifLoop, true)
	twoFrameGIF(t, gifOnce, false)
	out := recipe.Output{Format: "webp"}
	ovOp := func(params string) recipe.Op { return recipe.Op{Kind: recipe.OpOverlay, Params: []byte(params)} }

	t.Run("a 2x2 still at every anchor", func(t *testing.T) {
		// anchor → (x, y) of the op and the expected top-left corner of the
		// red block on the 16x16 canvas.
		cases := []struct {
			anchor    string
			x, y      int
			left, top int
		}{
			{"tl", 3, 4, 3, 4}, {"tc", 8, 4, 7, 4}, {"tr", 16, 0, 14, 0},
			{"ml", 0, 8, 0, 7}, {"mc", 8, 8, 7, 7}, {"mr", 16, 8, 14, 7},
			{"bl", 0, 16, 0, 14}, {"bc", 8, 16, 7, 14}, {"br", 16, 16, 14, 14},
		}
		for _, tc := range cases {
			t.Run(tc.anchor, func(t *testing.T) {
				p := compileSrcs(t, []recipe.ProbeInfo{baseInfo, redInfo}, []recipe.Op{ovOp(fmt.Sprintf(`{"source":1,"x":%d,"y":%d,"anchor":%q}`, tc.x, tc.y, tc.anchor))}, out)
				if want := []string{"-loop", "1"}; len(p.ExtraInputs) != 1 || strings.Join(p.ExtraInputs[0].Args, " ") != strings.Join(want, " ") {
					t.Fatalf("ExtraInputs: %+v", p.ExtraInputs)
				}
				frames := p3Render(t, ff, base, p, []string{red}, nil)
				if len(frames) != 6 {
					t.Fatalf("%d frames, want 6", len(frames))
				}
				for i, f := range frames {
					for _, d := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
						if px := pixel(f, 16, tc.left+d[0], tc.top+d[1]); !isRed(px) {
							t.Errorf("frame %d: (%d,%d) = %v, want red", i, tc.left+d[0], tc.top+d[1], px)
						}
					}
					for _, o := range [][2]int{{tc.left - 1, tc.top}, {tc.left + 2, tc.top}, {tc.left, tc.top - 1}, {tc.left, tc.top + 2}} {
						if o[0] < 0 || o[1] < 0 || o[0] >= 16 || o[1] >= 16 {
							continue
						}
						if px := pixel(f, 16, o[0], o[1]); !isBlue(px) {
							t.Errorf("frame %d: (%d,%d) = %v outside the overlay, want blue", i, o[0], o[1], px)
						}
					}
				}
			})
		}
	})

	t.Run("looping gif keeps alternating to the end of the base", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{baseInfo, gifInfo}, []recipe.Op{ovOp(`{"source":1,"x":3,"y":4}`)}, out)
		if in := p.ExtraInputs[0]; !in.Loop || !in.Animated || strings.Join(in.Args, " ") != "-stream_loop -1" {
			t.Fatalf("ExtraInputs: %+v", p.ExtraInputs)
		}
		for _, g := range []string{gifLoop, gifOnce} { // -stream_loop ignores the file's own loop count
			frames := p3Render(t, ff, base, p, []string{g}, nil)
			if len(frames) != 6 {
				t.Fatalf("%s: %d frames, want 6", filepath.Base(g), len(frames))
			}
			for i, f := range frames {
				px := pixel(f, 16, 3, 4)
				if want := i%2 == 0; isRed(px) != want || isLime(px) == want {
					t.Errorf("%s frame %d: %v, want %s", filepath.Base(g), i, px, map[bool]string{true: "red", false: "lime"}[want])
				}
			}
		}
	})
	t.Run("non-looping gif holds its last frame", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{baseInfo, gifInfo}, []recipe.Op{ovOp(`{"source":1,"x":3,"y":4,"noLoop":true}`)}, out)
		if in := p.ExtraInputs[0]; in.Loop || len(in.Args) != 0 || !strings.Contains(p.Filter, ",tpad=stop_mode=clone:stop=-1[ov1]") {
			t.Fatalf("plan: %+v %s", p.ExtraInputs, p.Filter)
		}
		frames := p3Render(t, ff, base, p, []string{gifLoop}, nil) // the NETSCAPE loop block is ignored by default
		if len(frames) != 6 {
			t.Fatalf("%d frames, want 6 (shortest=1 without the hold would cut the output at the overlay's end)", len(frames))
		}
		for i, f := range frames {
			px := pixel(f, 16, 3, 4)
			if (i == 0 && !isRed(px)) || (i > 0 && !isLime(px)) {
				t.Errorf("frame %d: %v, want red then lime held", i, px)
			}
		}
	})
	t.Run("looping apng, animated webp and a video through their own input args", func(t *testing.T) {
		two := filepath.Join(dir, "two-frames")
		if err := os.MkdirAll(two, 0o755); err != nil {
			t.Fatal(err)
		}
		writePNG(t, filepath.Join(two, "000001.png"), solid(4, 4, color.NRGBA{R: 255, A: 255}))
		writePNG(t, filepath.Join(two, "000002.png"), solid(4, 4, color.NRGBA{G: 255, A: 255}))
		pattern := filepath.Join(two, "%06d.png")
		cases := []struct {
			name, path, encoder string
			args                []string
			info                recipe.ProbeInfo
			wantArgs            string
		}{
			{"apng", filepath.Join(dir, "two.apng"), "apng", []string{"-c:v", "apng", "-plays", "0"},
				with(gifInfo, func(p *recipe.ProbeInfo) { p.Format, p.Codec, p.PixFmt = "apng", "apng", "rgba" }), "-ignore_loop 0"},
			{"webp", filepath.Join(dir, "two.webp"), "libwebp_anim", []string{"-c:v", "libwebp_anim", "-lossless", "1", "-loop", "0"},
				with(gifInfo, func(p *recipe.ProbeInfo) { p.Format, p.Codec, p.PixFmt = "webp_anim", "webp_anim", "argb" }), "-ignore_loop 0"},
			{"video", filepath.Join(dir, "two.mov"), "png", []string{"-c:v", "png"},
				with(gifInfo, func(p *recipe.ProbeInfo) {
					p.Format, p.Codec, p.PixFmt, p.Kind = "mov,mp4,m4a,3gp,3g2,mj2", "png", "rgb24", recipe.KindVideo
				}), "-stream_loop -1"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if !hasCodec(t, ff, "-encoders", tc.encoder) {
					t.Skipf("ffmpeg has no %s encoder", tc.encoder)
				}
				runFF(t, ff, append([]string{"-f", "image2", "-framerate", "10", "-i", pattern}, append(tc.args, tc.path)...))
				p := compileSrcs(t, []recipe.ProbeInfo{baseInfo, tc.info}, []recipe.Op{ovOp(`{"source":1,"x":3,"y":4}`)}, out)
				if got := strings.Join(p.ExtraInputs[0].Args, " "); got != tc.wantArgs {
					t.Fatalf("args %q, want %q", got, tc.wantArgs)
				}
				frames := p3Render(t, ff, base, p, []string{tc.path}, nil)
				if len(frames) != 6 {
					t.Fatalf("%d frames, want 6", len(frames))
				}
				for i, f := range frames {
					px := pixel(f, 16, 3, 4)
					if want := i%2 == 0; isRed(px) != want || isLime(px) == want {
						t.Errorf("frame %d: %v, want %s", i, px, map[bool]string{true: "red", false: "lime"}[want])
					}
				}
			})
		}
	})
	t.Run("opacity 0.5 blends, a time range gates, and the base alpha survives", func(t *testing.T) {
		var frames6 []image.Image
		for i := 0; i < 6; i++ {
			img := solid(16, 16, blue)
			for y := 0; y < 16; y++ {
				for x := 0; x < 8; x++ {
					img.SetNRGBA(x, y, color.NRGBA{B: 255, A: 128})
				}
			}
			frames6 = append(frames6, img)
		}
		alphaBase, alphaInfo := pngClip(t, ff, dir, "abase", frames6, 10, true)
		p := compileSrcs(t, []recipe.ProbeInfo{alphaInfo, redInfo}, []recipe.Op{ovOp(`{"source":1,"x":10,"y":4,"opacity":0.5,"start":0.2,"end":0.4}`)}, out)
		if !strings.Contains(p.Filter, ",colorchannelmixer=aa=0.5[ov1]") || !strings.Contains(p.Filter, ":enable='gte(t+0.0001,0.2)*lt(t+0.0001,0.4)'") || !p.HasAlpha {
			t.Fatalf("plan: alpha %v %s", p.HasAlpha, p.Filter)
		}
		frames := p3Render(t, ff, alphaBase, p, []string{red}, nil)
		if len(frames) != 6 {
			t.Fatalf("%d frames, want 6", len(frames))
		}
		for i, f := range frames {
			// [0.2, 0.4) on t = 0, 0.1, …, 0.5: frames 2 and 3; the frame at
			// t = 0.4 (the window's end) is not composited.
			px := pixel(f, 16, 10, 4)
			if on := i == 2 || i == 3; on {
				if !isRGBA(px, 128, 0, 128, 255, 3) {
					t.Errorf("frame %d: %v, want a 50%% red/blue blend", i, px)
				}
			} else if !isRGBA(px, 0, 0, 255, 255, 1) {
				t.Errorf("frame %d: %v, want plain blue (overlay disabled)", i, px)
			}
			if l := pixel(f, 16, 2, 2); !isRGBA(l, 0, 0, 255, 128, 1) {
				t.Errorf("frame %d: base pixel %v lost its alpha 128", i, l)
			}
		}
	})
	t.Run("scaled overlay covers its box", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{baseInfo, redInfo}, []recipe.Op{ovOp(`{"source":1,"x":3,"y":4,"width":4,"height":4}`)}, out)
		if !strings.Contains(p.Filter, "scale=4:4:flags=lanczos") {
			t.Fatalf("filter: %s", p.Filter)
		}
		f := p3Render(t, ff, base, p, []string{red}, nil)[0]
		for y := 4; y < 8; y++ {
			for x := 3; x < 7; x++ {
				if px := pixel(f, 16, x, y); !isRed(px) {
					t.Errorf("(%d,%d) = %v, want red", x, y, px)
				}
			}
		}
		if px := pixel(f, 16, 7, 4); !isBlue(px) {
			t.Errorf("(7,4) = %v, want blue", px)
		}
	})
	t.Run("a 10 fps gif over a 30 fps output shows each overlay frame three times", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{baseInfo, gifInfo}, []recipe.Op{ovOp(`{"source":1,"x":3,"y":4}`)}, recipe.Output{Format: "webp", FPS: 30})
		if !strings.Contains(p.Filter, "[1:v]format=rgba,fps=30[ov1]") || p.Frames != 18 {
			t.Fatalf("plan: %d frames, %s", p.Frames, p.Filter)
		}
		frames := p3Render(t, ff, base, p, []string{gifLoop}, nil)
		if len(frames) != 18 {
			t.Fatalf("%d frames, want 18", len(frames))
		}
		for i, f := range frames {
			px := pixel(f, 16, 3, 4)
			if want := (i/3)%2 == 0; isRed(px) != want || isLime(px) == want {
				t.Errorf("frame %d: %v, want %s", i, px, map[bool]string{true: "red", false: "lime"}[want])
			}
		}
	})
	t.Run("still over still", func(t *testing.T) {
		bluePNG := filepath.Join(dir, "blue.png")
		writePNG(t, bluePNG, solid(16, 16, blue))
		p := compileSrcs(t, []recipe.ProbeInfo{stillInfo(16, 16, false), redInfo}, []recipe.Op{ovOp(`{"source":1,"x":3,"y":4}`)}, recipe.Output{Format: recipe.FormatPNG})
		frames := p3Render(t, ff, bluePNG, p, []string{red}, nil)
		if len(frames) != 1 || p.Frames != 1 {
			t.Fatalf("%d frames (plan %d), want 1", len(frames), p.Frames)
		}
		if px := pixel(frames[0], 16, 3, 4); !isRed(px) {
			t.Errorf("(3,4) = %v, want red", px)
		}
	})
}

// TestEnableWindowFrameGrid: a window whose bounds come from the frame grid
// the way the UI's "from scrubber" buttons produce them (round6(k/fps)) must
// cover exactly frames k..m-1. At 30 fps round6(2/30) = 0.066667 lies just
// above frame 2's raw timestamp 0.0666666…, so the plain gte(t,S) the graph
// once emitted skipped frame 2, and lt(t,0.166667) drew frame 5; the
// tolerance in enableExpr fixes both (verified on FFmpeg 9.0.1). Overlays
// are used because they run wherever ffmpeg does (drawtext needs fontconfig;
// TestDrawTextPixels has the same check for it).
func TestEnableWindowFrameGrid(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	blue := color.NRGBA{B: 255, A: 255}
	var base8 []image.Image
	for i := 0; i < 8; i++ {
		base8 = append(base8, solid(16, 16, blue))
	}
	base, baseInfo := pngClip(t, ff, dir, "base30", base8, 30, false)
	red := filepath.Join(dir, "red.png")
	writePNG(t, red, solid(2, 2, color.NRGBA{R: 255, A: 255}))
	redInfo := stillInfo(2, 2, false)
	cases := []struct {
		name, window, enable string
		start, end           int // composited frames [start, end)
	}{
		{"[2/30, 6/30)", `"start":0.066667,"end":0.2`, "enable='gte(t+0.0001,0.066667)*lt(t+0.0001,0.2)'", 2, 6},
		{"[3/30, 5/30): frame 5 is not drawn", `"start":0.1,"end":0.166667`, "enable='gte(t+0.0001,0.1)*lt(t+0.0001,0.166667)'", 3, 5},
		{"[2/30, end)", `"start":0.066667`, "enable='gte(t+0.0001,0.066667)'", 2, 8},
		{"[0, 5/30)", `"end":0.166667`, "enable='gte(t+0.0001,0)*lt(t+0.0001,0.166667)'", 0, 5},
		{"unrounded 2/30 and 6/30 compile to the same window", `"start":0.06666666666666667,"end":0.2`, "enable='gte(t+0.0001,0.066667)*lt(t+0.0001,0.2)'", 2, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := compileSrcs(t, []recipe.ProbeInfo{baseInfo, redInfo}, []recipe.Op{{Kind: recipe.OpOverlay, Params: []byte(`{"source":1,"x":3,"y":4,` + tc.window + `}`)}}, recipe.Output{Format: "webp"})
			if p.FPS != 30 || p.Frames != 8 || !strings.Contains(p.Filter, tc.enable) {
				t.Fatalf("plan: fps %v frames %d filter %s", p.FPS, p.Frames, p.Filter)
			}
			frames := p3Render(t, ff, base, p, []string{red}, nil)
			if len(frames) != 8 {
				t.Fatalf("%d frames, want 8", len(frames))
			}
			for i, f := range frames {
				px := pixel(f, 16, 3, 4)
				if want := i >= tc.start && i < tc.end; isRed(px) != want || isBlue(px) == want {
					t.Errorf("frame %d (t = %d/30): %v, overlay drawn %v, want %v", i, i, px, isRed(px), want)
				}
			}
		})
	}
}

// --- CompileDetect ----------------------------------------------------------

// TestCompileDetectPixels renders a detection plan: the trimmed, keyed
// frames at the source size — what jobs' bbox stage must see to find the
// content box the crop then applies to — with the geometry, reverse and
// final-canvas ops of the prefix ignored.
func TestCompileDetectPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	green := color.NRGBA{G: 255, A: 255}
	var frames []image.Image
	for i := 0; i < 6; i++ {
		// The subject's blue channel carries the frame index.
		frames = append(frames, keyFrame(green, color.NRGBA{R: 255, B: uint8(10 * (i + 1)), A: 255}))
	}
	clip, info := pngClip(t, ff, dir, "detect", frames, 10, false)
	ops := []recipe.Op{
		{Kind: recipe.OpTrim, Params: []byte(`{"start":0.2,"end":0.5}`)},
		{Kind: recipe.OpChromaKey},
		{Kind: recipe.OpCrop, Params: []byte(`{"x":4,"y":4,"w":8,"h":8}`)}, // ignored: not a detection stage
		{Kind: recipe.OpReverse},                                           // ignored
		{Kind: recipe.OpText, Params: []byte(`{"text":"x"}`)},              // ignored
	}
	p, err := graph.CompileDetect([]recipe.ProbeInfo{info}, ops)
	if err != nil {
		t.Fatalf("CompileDetect: %v", err)
	}
	if p.Width != 16 || p.Height != 16 || !p.HasAlpha || p.Frames != 3 || p.Reversed || len(p.ExtraInputs) != 0 || len(p.TextFiles) != 0 {
		t.Fatalf("plan: %dx%d alpha %v frames %d reversed %v extras %d texts %d", p.Width, p.Height, p.HasAlpha, p.Frames, p.Reversed, len(p.ExtraInputs), len(p.TextFiles))
	}
	if want := "[0:v]fps=10:round=down,format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05,despill=type=green:mix=0.6:expand=0.3,format=rgba[out]"; p.Filter != want {
		t.Fatalf("filter\n got: %s\nwant: %s", p.Filter, want)
	}
	got := p3Render(t, ff, clip, p, nil, nil)
	if len(got) != 3 {
		t.Fatalf("%d frames, want 3 (trim 0.2..0.5 at 10 fps)", len(got))
	}
	for i, f := range got {
		// Source frames 2..4 in order, keyed, at the source size: the
		// background is transparent, the subject opaque at its source
		// position.
		if bg := pixel(f, 16, 0, 0); bg[3] != 0 {
			t.Errorf("frame %d: background alpha %d, want 0 (keyed)", i, bg[3])
		}
		if px := pixel(f, 16, 8, 8); !isRGBA(px, 255, 0, byte(10*(i+3)), 255, 4) {
			t.Errorf("frame %d: subject pixel %v, want (255,0,%d,255) — forward order, trimmed", i, px, 10*(i+3))
		}
		if e := pixel(f, 16, 3, 8); e[3] != 255 {
			t.Errorf("frame %d: edge pixel %v, want alpha 255", i, e)
		}
	}
}

// --- everything at once -----------------------------------------------------

func TestPhase3CombinedPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	// Six green-screen frames: a red 8x8 square at (4,4) whose blue channel
	// carries the frame index (so the order is readable after keying and
	// cropping), half-blended ring around it.
	var frames []image.Image
	for i := 0; i < 6; i++ {
		frames = append(frames, keyFrame(color.NRGBA{G: 255, A: 255}, color.NRGBA{R: 255, B: uint8(10 * (i + 1)), A: 255}))
	}
	clip, info := pngClip(t, ff, dir, "combo", frames, 10, false)
	gifPath := filepath.Join(dir, "loop.gif")
	twoFrameGIF(t, gifPath, true)
	ops := []recipe.Op{
		{Kind: recipe.OpChromaKey},
		{Kind: recipe.OpAutoCrop, Params: []byte(`{"threshold":8,"resolved":{"x":4,"y":4,"w":8,"h":8}}`)},
		{Kind: recipe.OpReverse},
		{Kind: recipe.OpOverlay, Params: []byte(`{"source":1,"x":6,"y":6}`)},
	}
	p := compileSrcs(t, []recipe.ProbeInfo{info, gifInfo}, ops, recipe.Output{Format: "gif"})
	if p.Width != 8 || p.Height != 8 || p.Frames != 6 || !p.Reversed || !p.HasAlpha {
		t.Fatalf("plan: %dx%d frames %d reversed %v alpha %v", p.Width, p.Height, p.Frames, p.Reversed, p.HasAlpha)
	}
	got := p3Render(t, ff, clip, p, []string{gifPath}, nil)
	if len(got) != 6 {
		t.Fatalf("%d frames, want 6", len(got))
	}
	for i, f := range got {
		// Keyed + cropped to the square: every pixel outside the overlay is
		// the opaque subject, in reverse order.
		if px := pixel(f, 8, 2, 2); !isRGBA(px, 255, 0, byte(10*(6-i)), 255, 4) {
			t.Errorf("frame %d: subject pixel %v, want (255,0,%d,255) — reversed order", i, px, 10*(6-i))
		}
		if px := pixel(f, 8, 0, 7); px[3] != 255 {
			t.Errorf("frame %d: corner alpha %d, want 255 (autocrop should have removed the keyed background)", i, px[3])
		}
		ov := pixel(f, 8, 7, 7)
		if want := i%2 == 0; isRed(ov) != want || isLime(ov) == want {
			t.Errorf("frame %d: overlay pixel %v, want %s", i, ov, map[bool]string{true: "red", false: "lime"}[want])
		}
	}
}
