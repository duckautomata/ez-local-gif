package graph_test

// Pixel-level check of the alpha handling in compiled graphs against a real
// ffmpeg. This is an external test package so internal/graph itself stays
// stdlib-only and process-free; the test skips when ffmpeg is not on PATH
// (the plain golang image) or is older than 8 (no alpha_mode negotiation).
//
// Background (FFmpeg >= 8): libavfilter negotiates AVFrame.alpha_mode across
// links. unpremultiply requires premultiplied input; decoders tag ProRes
// frames unspecified, so without an explicit tag the negotiator inserts
// premultiply_dynamic in front and the two cancel out. The compiler emits
// setparams=alpha_mode=premultiplied before the hoisted unpremultiply; this
// test proves the emitted graph really divides the alpha out.

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

const (
	discSize   = 64  // clip is discSize x discSize
	discFrames = 4   // frames encoded (10 fps)
	edgeMinA   = 32  // anti-aliased edge pixels have alpha in [edgeMinA, edgeMaxA]
	edgeMaxA   = 224 //
)

// ffmpegOrSkip returns the ffmpeg binary or skips the test.
func ffmpegOrSkip(t *testing.T) string {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out, err := exec.Command(ff, "-version").Output()
	if err != nil {
		t.Skipf("ffmpeg -version: %v", err)
	}
	if major := ffmpegMajor(string(out)); major > 0 && major < 8 {
		t.Skipf("alpha_mode negotiation needs FFmpeg >= 8 (have %d)", major)
	}
	return ff
}

// ffmpegMajor parses N from "ffmpeg version N.x ..." (0 if unknown, e.g. a
// git build with no leading number).
func ffmpegMajor(version string) int {
	fields := strings.Fields(version)
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			digits := strings.TrimLeftFunc(fields[i+1], func(r rune) bool { return r < '0' || r > '9' })
			end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
			if end >= 0 {
				digits = digits[:end]
			}
			n, _ := strconv.Atoi(digits)
			return n
		}
	}
	return 0
}

// discPNG writes a discSize x discSize PNG of a disc in colour c with a soft
// (anti-aliased) 1 px edge. premultiplied stores c*alpha in the colour
// channels (what a Resolve "Alpha Mode: Premultiplied" ProRes 4444 export
// carries: matted onto black); otherwise the colour is stored straight.
func discPNG(t *testing.T, path string, c uint8, premultiplied bool) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, discSize, discSize))
	const cx, cy, r = discSize/2 - 0.5, discSize/2 - 0.5, discSize/2 - 8.5
	for y := 0; y < discSize; y++ {
		for x := 0; x < discSize; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			cov := r + 0.5 - math.Hypot(dx, dy) // 1 px linear ramp across the edge
			cov = min(max(cov, 0), 1)
			a := uint8(cov*255 + 0.5)
			v := c
			if premultiplied {
				v = uint8(float64(c)*float64(a)/255 + 0.5)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// proresClip encodes pngPath as a ProRes 4444 clip (prores_ks,
// yuva444p10le; ffmpeg's decoder reports it as yuva444p12le). ProRes carries
// no alpha-mode tag, so the frames decode as alpha_mode=unspecified whether
// or not the pixels were premultiplied.
func proresClip(t *testing.T, ff, pngPath, movPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-loop", "1", "-framerate", "10", "-i", pngPath, "-frames:v", strconv.Itoa(discFrames),
		"-c:v", "prores_ks", "-profile:v", "4444", "-pix_fmt", "yuva444p10le", movPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encode prores: %v\n%s", err, out)
	}
}

// proresInfo is what probe would report for the clip (bit depth from the
// decoder's yuva444p12le, Premultiplied = prores heuristic).
var proresInfo = recipe.ProbeInfo{
	Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p12le", Bits: 12,
	Width: discSize, Height: discSize, FPS: 10, Duration: float64(discFrames) / 10, Frames: discFrames,
	HasAlpha: true, Kind: recipe.KindVideo, Premultiplied: true,
}

// renderMaster runs enc.MasterArgs for the plan and returns the first RGBA
// frame of the rawvideo master.
func renderMaster(t *testing.T, ff, src string, p *graph.Plan) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "frames.rgba")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, enc.MasterArgs(src, p, out)...)
	if o, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, o)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	frame := p.Width * p.Height * 4
	if len(data) < frame || len(data)%frame != 0 {
		t.Fatalf("master is %d bytes, not a multiple of %dx%dx4", len(data), p.Width, p.Height)
	}
	return data[:frame]
}

// edgeStats scans an RGBA frame for anti-aliased edge pixels (alpha in
// [edgeMinA, edgeMaxA]) and returns their count and the min/max red value.
func edgeStats(frame []byte) (n, minR, maxR int) {
	minR = 256
	for i := 0; i+3 < len(frame); i += 4 {
		a := int(frame[i+3])
		if a < edgeMinA || a > edgeMaxA {
			continue
		}
		r := int(frame[i])
		n++
		minR, maxR = min(minR, r), max(maxR, r)
	}
	return n, minR, maxR
}

func TestUnpremultiplyPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()

	// Premultiplied white disc: edge pixels are stored R=G=B=A.
	discPNG(t, filepath.Join(dir, "pre.png"), 255, true)
	pre := filepath.Join(dir, "pre.mov")
	proresClip(t, ff, filepath.Join(dir, "pre.png"), pre)
	// Straight grey disc: edge pixels are stored R=G=B=128 whatever alpha is.
	discPNG(t, filepath.Join(dir, "straight.png"), 128, false)
	straight := filepath.Join(dir, "straight.mov")
	proresClip(t, ff, filepath.Join(dir, "straight.png"), straight)

	unpre := []recipe.Op{{Kind: recipe.OpUnpremultiply}}
	fit32 := recipe.Output{Format: "webp", Width: 32, Height: 32}

	// Sanity: decoded as-is, the premultiplied edge is dark (R ~ A). This is
	// what the toggle must undo, and it proves the assertions below can fail.
	plain := compile(t, proresInfo, nil, recipe.Output{Format: "webp"})
	if n, minR, maxR := edgeStats(renderMaster(t, ff, pre, plain)); n < 50 || maxR > 240 {
		t.Fatalf("premultiplied source without unpremultiply: %d edge pixels, R in [%d,%d]; expected dark edges (R ~ A)", n, minR, maxR)
	}

	t.Run("hoisted unpremultiply restores straight colour", func(t *testing.T) {
		p := compile(t, proresInfo, unpre, recipe.Output{Format: "webp"})
		if !strings.Contains(p.Filter, "setparams=alpha_mode=premultiplied,unpremultiply=inplace=1") {
			t.Fatalf("filter: %s", p.Filter)
		}
		n, minR, maxR := edgeStats(renderMaster(t, ff, pre, p))
		t.Logf("%d edge pixels, R in [%d,%d] (filter %s)", n, minR, maxR, p.Filter)
		if n < 50 {
			t.Fatalf("only %d anti-aliased edge pixels (alpha in [%d,%d])", n, edgeMinA, edgeMaxA)
		}
		if minR <= 240 {
			t.Errorf("edge pixel R=%d after unpremultiply, want > 240 (the toggle is a no-op: FFmpeg auto-inserted premultiply_dynamic?)", minR)
		}
	})

	t.Run("hoisted unpremultiply then premultiplied scale keeps white edges", func(t *testing.T) {
		p := compile(t, proresInfo, unpre, fit32)
		if !strings.Contains(p.Filter, "format=gbrap,premultiply=inplace=1,scale=32:32:flags=lanczos,unpremultiply=inplace=1") {
			t.Fatalf("filter: %s", p.Filter)
		}
		n, minR, maxR := edgeStats(renderMaster(t, ff, pre, p))
		t.Logf("%d edge pixels, R in [%d,%d] (filter %s)", n, minR, maxR, p.Filter)
		if n < 20 {
			t.Fatalf("only %d anti-aliased edge pixels after scaling", n)
		}
		if minR <= 240 {
			t.Errorf("edge pixel R=%d after unpremultiply+scale, want > 240", minR)
		}
	})

	t.Run("premultiplied scale on an untagged straight source is neutral", func(t *testing.T) {
		// No unpremultiply op: the stream reaches premultiply as
		// alpha_mode=unspecified, which must be accepted as straight (no
		// auto-inserted conversion). A wrongly inserted unpremultiply would
		// blow the grey edge (128*255/A) out towards white.
		p := compile(t, proresInfo, nil, fit32)
		if !strings.Contains(p.Filter, "format=gbrap,premultiply=inplace=1,scale=32:32:flags=lanczos,unpremultiply=inplace=1") {
			t.Fatalf("filter: %s", p.Filter)
		}
		n, minR, maxR := edgeStats(renderMaster(t, ff, straight, p))
		t.Logf("%d edge pixels, R in [%d,%d] (filter %s)", n, minR, maxR, p.Filter)
		if n < 20 {
			t.Fatalf("only %d anti-aliased edge pixels after scaling", n)
		}
		if minR < 118 || maxR > 138 {
			t.Errorf("grey edge R in [%d,%d] after the scale chain, want ~128 (+-10)", minR, maxR)
		}
	})
}

// compile wraps graph.Compile with a fatal error check.
func compile(t *testing.T, src recipe.ProbeInfo, ops []recipe.Op, out recipe.Output) *graph.Plan {
	t.Helper()
	p, err := graph.Compile(src, ops, out)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}
