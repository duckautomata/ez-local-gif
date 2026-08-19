package enc_test

// End-to-end checks of the still/proxy argv against a real ffmpeg. This is
// an external test package so internal/enc itself stays stdlib-only and
// process-free; everything skips when ffmpeg is not on PATH.
//
// Covered: the still never comes back empty near the end of a CFR clip (the
// scrubber's maximum is t == Duration), the frame it returns is the very
// frame the render puts in that slot, StillArgsFromStart reaches into the
// held last frame of a VFR animation, still images work, and the preview
// scale does not darken anti-aliased alpha edges.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
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
	clipW, clipH  = 160, 120 // test clip size
	clipFPS       = 30       // test clip frame rate
	clipSeconds   = 4        // test clip length
	discSize      = 960      // alpha disc image size (scaled to 480 by the preview)
	edgeMinA      = 32       // anti-aliased edge pixels have alpha in [edgeMinA, edgeMaxA]
	edgeMaxA      = 224      //
	ffmpegTimeout = 60 * time.Second
)

// ffmpegOrSkip returns the ffmpeg binary or skips the test.
func ffmpegOrSkip(t *testing.T) string {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	return ff
}

// ffmpegMajor parses N from "ffmpeg version N.x ..." (0 if unknown).
func ffmpegMajor(t *testing.T, ff string) int {
	t.Helper()
	out, err := exec.Command(ff, "-version").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
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

// run executes ffmpeg with the ffrun-style prefix and returns stdout; a
// non-zero exit is fatal.
func run(t *testing.T, ff string, args []string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	full := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)
	cmd := exec.CommandContext(ctx, ff, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(full, " "), err, stderr.String())
	}
	return out
}

// cfrClip encodes a clipSeconds-long testsrc clip (every frame is distinct)
// with the native mpeg4 encoder, so seeking has real keyframes to land on.
func cfrClip(t *testing.T, ff, dir string) (string, recipe.ProbeInfo) {
	t.Helper()
	path := filepath.Join(dir, "clip.mp4")
	run(t, ff, []string{
		"-f", "lavfi", "-i", "testsrc=size=" + strconv.Itoa(clipW) + "x" + strconv.Itoa(clipH) + ":rate=" + strconv.Itoa(clipFPS) + ":duration=" + strconv.Itoa(clipSeconds),
		"-c:v", "mpeg4", "-q:v", "3", "-g", "12", path,
	})
	info := recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "mpeg4", PixFmt: "yuv420p", Bits: 8,
		Width: clipW, Height: clipH, FPS: clipFPS, Duration: clipSeconds, Frames: clipSeconds * clipFPS,
		Kind: recipe.KindVideo,
	}
	return path, info
}

// holdGIF writes a 3 s GIF: ten 100 ms frames whose red bar moves right,
// then a last frame (blue bar) held for 2 s — the frame timestamps end at
// 1.0 s although the animation lasts 3.0 s.
func holdGIF(t *testing.T, dir string) (string, recipe.ProbeInfo) {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 0}, color.RGBA{220, 30, 30, 255}, color.RGBA{30, 30, 220, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < 11; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, 40, 30), pal)
		idx := uint8(1)
		if i == 10 {
			idx = 2
		}
		for y := 5; y < 25; y++ {
			for x := i*2 + 2; x < i*2+14; x++ {
				fr.SetColorIndex(x, y, idx)
			}
		}
		g.Image = append(g.Image, fr)
		delay := 10 // centiseconds
		if i == 10 {
			delay = 200
		}
		g.Delay = append(g.Delay, delay)
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hold.gif")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{
		Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8,
		Width: 40, Height: 30, FPS: 11.0 / 3, Duration: 3, Frames: 11,
		HasAlpha: true, Kind: recipe.KindAnimation,
	}
	return path, info
}

// discPNG writes a discSize x discSize PNG of a white anti-aliased disc on
// fully transparent black (straight alpha), i.e. exactly the picture whose
// edges go grey when scaled without premultiplying.
func discPNG(t *testing.T, dir string) (string, recipe.ProbeInfo) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, discSize, discSize))
	const cx, cy, r = discSize/2 - 0.5, discSize/2 - 0.5, discSize/2 - 40.5
	for y := 0; y < discSize; y++ {
		for x := 0; x < discSize; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			cov := r + 0.5 - math.Hypot(dx, dy) // 1 px linear ramp across the edge
			cov = min(max(cov, 0), 1)
			a := uint8(cov*255 + 0.5)
			v := uint8(255)
			if a == 0 {
				v = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "disc.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{
		Format: "png_pipe", Codec: "png", PixFmt: "rgba", Bits: 8,
		Width: discSize, Height: discSize, Frames: 1, HasAlpha: true, IsStill: true, Kind: recipe.KindImage,
	}
	return path, info
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

// decodePNG decodes ffmpeg's PNG output into straight-alpha RGBA bytes.
func decodePNG(t *testing.T, data []byte) *image.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png (%d bytes): %v", len(data), err)
	}
	if n, ok := img.(*image.NRGBA); ok {
		return n
	}
	b := img.Bounds()
	n := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			n.Set(x, y, img.At(x, y))
		}
	}
	return n
}

// renderMaster runs enc.MasterArgs and returns the RGBA master frames.
func renderMaster(t *testing.T, ff, src string, p *graph.Plan) [][]byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "frames.rgba")
	run(t, ff, enc.MasterArgs(src, p, out))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	frame := p.Width * p.Height * 4
	if len(data) == 0 || len(data)%frame != 0 {
		t.Fatalf("master is %d bytes, not a multiple of %dx%dx4", len(data), p.Width, p.Height)
	}
	frames := make([][]byte, 0, len(data)/frame)
	for off := 0; off < len(data); off += frame {
		frames = append(frames, data[off:off+frame])
	}
	return frames
}

// stillPNG runs StillArgs and returns the PNG bytes (possibly empty).
func stillPNG(t *testing.T, ff, src string, p *graph.Plan, tt float64, maxW int) []byte {
	t.Helper()
	return run(t, ff, enc.StillArgs(src, p, tt, maxW))
}

// frameIndex returns which master frame equals img (or -1).
func frameIndex(frames [][]byte, img *image.NRGBA) int {
	for i, f := range frames {
		if bytes.Equal(f, img.Pix) {
			return i
		}
	}
	return -1
}

func TestStillEndOfClip(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	clip, info := cfrClip(t, ff, dir)

	// The output frame rates that matter: gif and webp at the source's 30 fps
	// (gif no longer snaps to 100/n), a fractional 33.333 fps op, low fps
	// (where the fps stage rounds a short tail to zero frames), and a 4 fps
	// plan whose slot is longer than the default seek-back.
	for _, tc := range []struct {
		ops []recipe.Op
		out recipe.Output
	}{
		{nil, recipe.Output{Format: "gif"}},
		{nil, recipe.Output{Format: "webp"}},
		{[]recipe.Op{op(recipe.OpFPS, recipe.FPSParams{FPS: 100.0 / 3})}, recipe.Output{Format: "gif"}},
		{nil, recipe.Output{Format: "gif", FPS: 10}},
		{nil, recipe.Output{Format: "gif", FPS: 5}},
		{nil, recipe.Output{Format: "gif", FPS: 4}},
	} {
		out := tc.out
		p := compile(t, info, tc.ops, out)
		name := out.Format + "@" + strconv.FormatFloat(p.FPS, 'f', -1, 64)
		t.Run(name, func(t *testing.T) {
			frames := renderMaster(t, ff, clip, p)
			if len(frames) != p.Frames {
				t.Logf("master has %d frames, plan expected %d", len(frames), p.Frames)
			}
			// t == Duration is what the scrubber's maximum sends; the rest walk
			// back through the last few source frames and one mid-clip point.
			for _, tt := range []float64{clipSeconds, 3.999, 3.99, 3.97, 3.96, 3.95, 3.9, 2.5, 2, 0.5, 0} {
				pngData := stillPNG(t, ff, clip, p, tt, 0)
				if len(pngData) == 0 {
					t.Errorf("t=%v: still produced no image (args %q)", tt, enc.StillArgs(clip, p, tt, 0))
					continue
				}
				img := decodePNG(t, pngData)
				if img.Bounds().Dx() != p.Width || img.Bounds().Dy() != p.Height {
					t.Errorf("t=%v: still is %v, want %dx%d", tt, img.Bounds(), p.Width, p.Height)
				}
				// The still must be the frame the render shows at t: master
				// slot floor(t*FPS), or the last frame when t is at/after it.
				want := int(math.Floor(math.Min(tt, clipSeconds-0.001)*p.FPS + 1e-6))
				if want > len(frames)-1 {
					want = len(frames) - 1
				}
				got := frameIndex(frames, img)
				if got != want {
					t.Errorf("t=%v: still is master frame %d, want %d (args %q)", tt, got, want, enc.StillArgs(clip, p, tt, 0))
				}
			}
		})
	}
}

func TestStillTrimAndSpeed(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	clip, info := cfrClip(t, ff, dir)

	tests := []struct {
		name string
		ops  []recipe.Op
		out  recipe.Output
		ts   []float64
	}{
		{
			// Trim to the end of the source: TrimEnd becomes 0 and Duration 3;
			// t=3 is the scrubber's maximum.
			name: "trim 1..4 (to the end)",
			ops:  []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 1, End: 4})},
			out:  recipe.Output{Format: "gif"},
			ts:   []float64{3, 2.99, 1.5, 0},
		},
		{
			name: "trim 0.5..2.5",
			ops:  []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 0.5, End: 2.5})},
			out:  recipe.Output{Format: "webp"},
			ts:   []float64{2, 1.999, 1.97, 1, 0},
		},
		{
			name: "speed 2 (duration 2)",
			ops:  []recipe.Op{op(recipe.OpSpeed, recipe.SpeedParams{Factor: 2})},
			out:  recipe.Output{Format: "gif"},
			ts:   []float64{2, 1.99, 1, 0},
		},
		{
			name: "speed 0.5 (duration 8)",
			ops:  []recipe.Op{op(recipe.OpSpeed, recipe.SpeedParams{Factor: 0.5})},
			out:  recipe.Output{Format: "webp", FPS: 10},
			ts:   []float64{8, 7.99, 7.9, 4, 0},
		},
		{
			name: "trim + speed + fit (emote)",
			ops:  []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 1, End: 3.5}), op(recipe.OpSpeed, recipe.SpeedParams{Factor: 1.5})},
			out:  recipe.Output{Format: "gif", Width: 64, Height: 64, FPS: 25},
			ts:   []float64{5.0 / 3, 1.66, 1.6, 0.8, 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := compile(t, info, tc.ops, tc.out)
			frames := renderMaster(t, ff, clip, p)
			for _, tt := range tc.ts {
				pngData := stillPNG(t, ff, clip, p, tt, 0)
				if len(pngData) == 0 {
					t.Errorf("t=%v: still produced no image (args %q)", tt, enc.StillArgs(clip, p, tt, 0))
					continue
				}
				img := decodePNG(t, pngData)
				want := int(math.Floor(math.Min(tt, p.Duration-0.001/p.Speed)*p.FPS + 1e-6))
				if want > len(frames)-1 {
					want = len(frames) - 1
				}
				got := frameIndex(frames, img)
				if got != want {
					t.Errorf("t=%v: still is master frame %d, want %d (args %q)", tt, got, want, enc.StillArgs(clip, p, tt, 0))
				}
			}
		})
	}
}

func TestStillFromStartReachesHeldFrame(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	src, info := holdGIF(t, dir)
	p := compile(t, info, nil, recipe.Output{Format: "gif", FPS: 10})
	frames := renderMaster(t, ff, src, p)
	if len(frames) < 25 {
		t.Fatalf("master has %d frames, want the 2 s hold carried to ~30", len(frames))
	}
	last := frames[len(frames)-1]

	for _, tt := range []float64{1.05, 1.5, 2.5, 2.99, 3} {
		// The seeking still may or may not find a frame inside the hold
		// (the demuxer cannot seek and the last frame's pts is 1.0); when it
		// does, it must be the held frame.
		if fast := stillPNG(t, ff, src, p, tt, 0); len(fast) > 0 {
			if img := decodePNG(t, fast); !bytes.Equal(img.Pix, last) {
				t.Errorf("t=%v: seeking still returned master frame %d, want the held last frame", tt, frameIndex(frames, img))
			}
		} else {
			t.Logf("t=%v: seeking still found no frame inside the hold (expected; callers retry from the start)", tt)
		}
		// The from-start variant must always return the held frame.
		data := run(t, ff, enc.StillArgsFromStart(src, p, tt, 0))
		if len(data) == 0 {
			t.Fatalf("t=%v: StillArgsFromStart produced no image (args %q)", tt, enc.StillArgsFromStart(src, p, tt, 0))
		}
		if img := decodePNG(t, data); !bytes.Equal(img.Pix, last) {
			t.Errorf("t=%v: StillArgsFromStart returned master frame %d, want the held last frame", tt, frameIndex(frames, img))
		}
	}
	// Before the hold both variants agree with the render.
	for _, tt := range []float64{0, 0.35, 0.95} {
		want := int(math.Floor(tt*p.FPS + 1e-6))
		for name, args := range map[string][]string{"seek": enc.StillArgs(src, p, tt, 0), "from start": enc.StillArgsFromStart(src, p, tt, 0)} {
			data := run(t, ff, args)
			if len(data) == 0 {
				t.Errorf("t=%v %s: no image", tt, name)
				continue
			}
			if got := frameIndex(frames, decodePNG(t, data)); got != want {
				t.Errorf("t=%v %s: master frame %d, want %d", tt, name, got, want)
			}
		}
	}
}

func TestStillAndProxyOfStillImageKeepAlphaEdgesBright(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	src, info := discPNG(t, dir)
	p := compile(t, info, nil, recipe.Output{Format: "webp"})
	if !p.HasAlpha || p.Frames != 1 {
		t.Fatalf("plan: %+v", p)
	}

	// edgeStats returns the number of anti-aliased edge pixels and their
	// min/max red value in a straight-alpha RGBA buffer.
	edgeStats := func(pix []byte) (n, minR, maxR int) {
		minR = 256
		for i := 0; i+3 < len(pix); i += 4 {
			a := int(pix[i+3])
			if a < edgeMinA || a > edgeMaxA {
				continue
			}
			n++
			minR, maxR = min(minR, int(pix[i])), max(maxR, int(pix[i]))
		}
		return n, minR, maxR
	}

	t.Run("still at 480 px", func(t *testing.T) {
		data := stillPNG(t, ff, src, p, 0, 480)
		if len(data) == 0 {
			t.Fatalf("still of a still image produced no image (args %q)", enc.StillArgs(src, p, 0, 480))
		}
		img := decodePNG(t, data)
		if img.Bounds().Dx() != 480 || img.Bounds().Dy() != 480 {
			t.Fatalf("still is %v, want 480x480", img.Bounds())
		}
		n, minR, maxR := edgeStats(img.Pix)
		t.Logf("still: %d edge pixels, R in [%d,%d]", n, minR, maxR)
		if n < 100 {
			t.Fatalf("only %d anti-aliased edge pixels", n)
		}
		if minR < 240 {
			t.Errorf("edge pixel R=%d in the still, want >= 240 (straight-alpha scale halo)", minR)
		}
	})

	t.Run("still image at t > 0 still returns the frame", func(t *testing.T) {
		if data := stillPNG(t, ff, src, p, 0.5, 64); len(data) == 0 {
			t.Errorf("no image (args %q)", enc.StillArgs(src, p, 0.5, 64))
		}
	})

	t.Run("proxy at 360 px", func(t *testing.T) {
		if ffmpegMajor(t, ff) < 9 {
			t.Skip("decoding animated WebP needs FFmpeg 9")
		}
		out := filepath.Join(dir, "proxy.webp")
		run(t, ff, enc.ProxyArgs(src, p, 0, 0, out))
		// Decode the proxy back to straight RGBA.
		pix := run(t, ff, []string{"-i", out, "-frames:v", "1", "-vf", "format=rgba", "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"})
		if len(pix) != 360*360*4 {
			t.Fatalf("proxy decodes to %d bytes, want 360x360x4", len(pix))
		}
		n, minR, maxR := edgeStats(pix)
		t.Logf("proxy: %d edge pixels, R in [%d,%d]", n, minR, maxR)
		if n < 100 {
			t.Fatalf("only %d anti-aliased edge pixels", n)
		}
		// Lossy yuva420p, so allow some loss — but nowhere near the ~170 a
		// straight-alpha scale produces.
		if minR < 220 {
			t.Errorf("edge pixel R=%d in the proxy, want >= 220 (straight-alpha scale halo)", minR)
		}
	})
}

// op builds an Op with JSON-encoded params.
func op(kind string, params any) recipe.Op {
	b, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	return recipe.Op{Kind: kind, Params: b}
}
