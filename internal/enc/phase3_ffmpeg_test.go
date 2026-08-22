package enc_test

// Real-ffmpeg checks of the Phase 3 argv (skipped without ffmpeg on PATH):
//
//   - a reversed plan's still is, at every output time, the very frame the
//     reversed master holds in that slot (seek and from-start variants);
//   - a still of a plan with overlay inputs shows the overlay frame and the
//     enable window the render shows at that time — the main input keeps
//     the render's clock (-itsoffset) so overlays start at 0 unseeked, for
//     gif/apng/webp/still-image overlays, looping or not, on forward and on
//     reversed bases;
//   - the animated proxy composites the overlays too;
//   - CropDetectArgs + ParseCropDetect find the exact alpha content box
//     (odd offsets, one-pixel content, thresholds, stills, trimmed image
//     sequences) and the cropdetect black-border box of opaque sources;
//   - CropDetectPlanArgs detects on the compiled picture: a green-screen
//     clip keyed by a chromakey/colorkey op resolves to the subject's union
//     box (the raw-source path sees an opaque picture and reports the full
//     frame), trimmed plans see the trimmed clip, single-frame plans need no
//     loop, and opaque plans measure the same black borders as the raw path;
//   - ParseCropDetect reads the detectors' lines only: a source whose
//     metadata tags record a "crop=…" command line (echoed by ffmpeg at info
//     level for the input and the null output) still resolves to the true
//     box, through both builders, for MOV tags and a GIF comment extension;
//   - a reversed plan on an animation source (a GIF with a 600 ms hold,
//     whose frames a seek cannot land between) shows the render's frame at
//     every slot, seeking and from-start variants alike (the seek used to
//     pick the wrong reversed frame by index);
//   - a trimmed animated WebP (FFmpeg 9's webp_anim demuxer decodes nothing
//     after an input seek) renders, stills, proxies and autocrop-detects
//     through the plan's filter-level trim: the master has (end-start)*fps
//     frames starting at the trim start, the stills match it at every slot,
//     the proxy decodes, and the detection finds the box;
//   - an UNTRIMMED animated WebP is SeekUnsafe: StillArgs decodes its frame
//     in one unseeked run (no -ss, the frame at t = 0.55 s of a 10 fps
//     numbered animation is source frame 5; the from-start retry is never
//     needed), the reversed stills match the reversed master, and the
//     reversed proxy is not seeked for its tail and decodes to the tail.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

const (
	ovSize   = 16 // overlay frame size
	ovFrames = 10 // numbered overlay frames (10 fps, 1 s)
)

// overlayRed is the red value of numbered overlay frame i.
func overlayRed(i int) uint8 { return uint8(20 * (i + 1)) }

// numberedGIF writes a 16x16 GIF of ovFrames solid frames at 10 fps whose
// red channel numbers the frame (overlayRed), looping forever.
func numberedGIF(t *testing.T, dir string) string {
	t.Helper()
	pal := make(color.Palette, 0, ovFrames)
	for i := 0; i < ovFrames; i++ {
		pal = append(pal, color.RGBA{R: overlayRed(i), A: 255})
	}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < ovFrames; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, ovSize, ovSize), pal)
		for j := range fr.Pix {
			fr.Pix[j] = uint8(i)
		}
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 10)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "numbered.gif")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// convert re-encodes src with the given output args (lossless formats only).
func convert(t *testing.T, ff, src, out string, args ...string) string {
	t.Helper()
	run(t, ff, append(append([]string{"-i", src}, args...), out))
	return out
}

// writePNG stores img as PNG at path.
func writePNG(t *testing.T, path string, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// alphaPNG writes a w x h PNG that is fully transparent except for box,
// which is white at the given alpha.
func alphaPNG(t *testing.T, path string, w, h int, box image.Rectangle, alpha uint8) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 255, 255, alpha})
		}
	}
	return writePNG(t, path, img)
}

// opaquePNG writes a w x h black PNG with a white box.
func opaquePNG(t *testing.T, path string, w, h int, box image.Rectangle) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBA{0, 0, 0, 255}
			if image.Pt(x, y).In(box) {
				c = color.NRGBA{255, 255, 255, 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return writePNG(t, path, img)
}

// runLog runs ffmpeg at info level (like ffrun.RunFFmpegLog) and returns its
// stderr; a non-zero exit is fatal.
func runLog(t *testing.T, ff string, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	full := append([]string{"-hide_banner", "-nostdin", "-y", "-loglevel", "info", "-nostats"}, args...)
	cmd := exec.CommandContext(ctx, ff, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(full, " "), err, stderr.String())
	}
	return stderr.String()
}

// withReverse turns a compiled forward plan into the reversed one the Phase
// 3 compiler emits: a reverse stage after the geometry, Reversed set.
func withReverse(p *graph.Plan) *graph.Plan {
	q := *p
	q.Filter = strings.TrimSuffix(p.Filter, "[out]") + ",reverse[out]"
	q.Reversed = true
	return &q
}

// rgbaAt returns the pixel at (x, y) of a w-wide RGBA frame.
func rgbaAt(frame []byte, w, x, y int) color.RGBA {
	i := (y*w + x) * 4
	return color.RGBA{frame[i], frame[i+1], frame[i+2], frame[i+3]}
}

// checkStills asserts that the seeking and the from-start still of p at
// every t is master frame min(floor(t*FPS), n-1).
func checkStills(t *testing.T, ff, src string, p *graph.Plan, frames [][]byte, ts []float64) {
	t.Helper()
	for _, tt := range ts {
		want := min(int(math.Floor(tt*p.FPS+1e-6)), len(frames)-1)
		for name, args := range map[string][]string{"seek": enc.StillArgs(src, p, tt, 0), "from start": enc.StillArgsFromStart(src, p, tt, 0)} {
			data := run(t, ff, args)
			if len(data) == 0 {
				t.Errorf("t=%v %s: no image (args %q)", tt, name, args)
				continue
			}
			// frameIndex reports the first identical master frame; an fps
			// resample can repeat a source frame in consecutive slots.
			img := decodePNG(t, data)
			if got := frameIndex(frames, img); got != want && !bytes.Equal(img.Pix, frames[want]) {
				t.Errorf("t=%v %s: still is master frame %d, want %d (args %q)", tt, name, got, want, args)
			}
		}
	}
}

func TestStillReversed(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	clip, info := cfrClip(t, ff, dir)
	cases := []struct {
		name string
		ops  []recipe.Op
		out  recipe.Output
	}{
		{"gif@30", nil, recipe.Output{Format: "gif"}},
		{"webp@10", nil, recipe.Output{Format: "webp", FPS: 10}},
		{"fps 33.333", []recipe.Op{op(recipe.OpFPS, recipe.FPSParams{FPS: 100.0 / 3})}, recipe.Output{Format: "gif"}},
		{"trim 1..3", []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 1, End: 3})}, recipe.Output{Format: "gif"}},
		{"speed 2", []recipe.Op{op(recipe.OpSpeed, recipe.SpeedParams{Factor: 2})}, recipe.Output{Format: "gif"}},
		{"slow motion 0.5 at 10 fps", []recipe.Op{op(recipe.OpSpeed, recipe.SpeedParams{Factor: 0.5})}, recipe.Output{Format: "webp", FPS: 10}},
		{"trim + speed + fit (emote)", []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 1, End: 3.5}), op(recipe.OpSpeed, recipe.SpeedParams{Factor: 1.5})}, recipe.Output{Format: "gif", Width: 64, Height: 64, FPS: 25}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fwd := compile(t, info, tc.ops, tc.out)
			p := withReverse(fwd)
			fwdFrames := renderMaster(t, ff, clip, fwd)
			frames := renderMaster(t, ff, clip, p)
			n := len(frames)
			if n != len(fwdFrames) {
				t.Fatalf("reversed master has %d frames, forward %d", n, len(fwdFrames))
			}
			for i := range frames {
				if !bytes.Equal(frames[i], fwdFrames[n-1-i]) {
					t.Fatalf("reversed frame %d is not forward frame %d", i, n-1-i)
				}
			}
			ts := []float64{0, 0.5 / p.FPS, 0.5, 1, p.Duration / 2, p.Duration - 0.01, (float64(n) - 0.5) / p.FPS, p.Duration, p.Duration + 3}
			checkStills(t, ff, clip, p, frames, ts)
		})
	}
}

// overlayCase describes one overlay input for TestStillOverlayClock.
type overlayCase struct {
	name     string
	path     string
	args     []string
	loop     bool   // the overlay repeats (else it holds its last frame)
	still    bool   // a still image (no fps stage in its branch)
	enable   string // overlay enable expression ("" = always)
	reversed bool   // the base is played backwards
	needs9   bool   // needs FFmpeg 9 (webp_anim)
}

// overlayPlanFor builds the plan the Phase 3 compiler would emit for a 30
// fps base with one overlay at (0,0).
func overlayPlanFor(t *testing.T, info recipe.ProbeInfo, c overlayCase) *graph.Plan {
	t.Helper()
	p := compile(t, info, nil, recipe.Output{Format: "gif"})
	if p.Filter != "[0:v]fps=30:round=down,format=rgba[out]" {
		t.Fatalf("unexpected base filter %q", p.Filter)
	}
	base := "[0:v]fps=30:round=down,format=rgba"
	if c.reversed {
		base += ",reverse"
	}
	branch := "[1:v]format=rgba,fps=30[o]"
	if c.still {
		branch = "[1:v]format=rgba[o]"
	}
	enable := ""
	if c.enable != "" {
		enable = ":enable='" + c.enable + "'"
	}
	// An endless overlay (-stream_loop -1, a demuxer loop, -loop 1) needs
	// shortest=1 to end with the base; a play-once overlay must not have it
	// (the output would end with the overlay) — its last frame is repeated
	// by the default eof_action until the base ends.
	shortest := ""
	if c.loop || c.still {
		shortest = ":shortest=1"
	}
	p.Filter = base + "[b];" + branch + ";[b][o]overlay=x=0:y=0:format=auto" + shortest + enable + ",format=rgba[out]"
	p.Reversed = c.reversed
	dur := 1.0
	if c.still {
		dur = 0
	}
	p.ExtraInputs = []graph.ExtraInput{{Source: 1, Path: c.path, Args: c.args, Animated: !c.still, Duration: dur, Loop: c.loop}}
	return p
}

// expectedOverlay returns the overlay colour the render shows at output slot
// k of a 30 fps base: numbered frame floor(k/3) (10 fps overlay), modulo the
// frame count when looping, held at the last frame otherwise.
func expectedOverlay(c overlayCase, k int) color.RGBA {
	if c.still {
		return color.RGBA{0, 0, 200, 255}
	}
	i := k / 3
	if c.loop {
		i %= ovFrames
	} else {
		i = min(i, ovFrames-1)
	}
	return color.RGBA{overlayRed(i), 0, 0, 255}
}

// enabledAt reports whether the case's enable window covers output time t.
func enabledAt(c overlayCase, t float64) bool {
	return c.enable == "" || (t >= 1-1e-9 && t <= 3+1e-9)
}

func TestStillOverlayClock(t *testing.T) {
	ff := ffmpegOrSkip(t)
	major := ffmpegMajor(t, ff)
	dir := t.TempDir()
	clip, info := cfrClip(t, ff, dir)
	gifPath := numberedGIF(t, dir)
	apngPath := convert(t, ff, gifPath, filepath.Join(dir, "numbered.apng"), "-c:v", "apng", "-plays", "0")
	webpPath := convert(t, ff, gifPath, filepath.Join(dir, "numbered.webp"), "-c:v", "libwebp_anim", "-lossless", "1", "-loop", "0")
	stillImg := image.NewNRGBA(image.Rect(0, 0, ovSize, ovSize))
	for i := range stillImg.Pix {
		stillImg.Pix[i] = []uint8{0, 0, 200, 255}[i%4]
	}
	stillPath := writePNG(t, filepath.Join(dir, "solid.png"), stillImg)
	plain := compile(t, info, nil, recipe.Output{Format: "gif"})
	plainFrames := renderMaster(t, ff, clip, plain)

	loopArgs := []string{"-stream_loop", "-1"}
	window := "between(t,1,3)"
	cases := []overlayCase{
		{name: "gif looping, window 1..3", path: gifPath, args: loopArgs, loop: true, enable: window},
		{name: "gif played once holds its last frame", path: gifPath},
		{name: "apng looping, window 1..3", path: apngPath, args: loopArgs, loop: true, enable: window},
		// webp_anim cannot be seeked or -stream_loop'ed; the demuxer's own
		// loop (-ignore_loop 0) repeats the file's infinite ANIM loop.
		{name: "webp demuxer loop, window 1..3", path: webpPath, args: []string{"-ignore_loop", "0"}, loop: true, enable: window, needs9: true},
		{name: "still image -loop 1, window 1..3", path: stillPath, args: []string{"-loop", "1"}, still: true, enable: window},
		{name: "gif looping on a reversed base, window 1..3", path: gifPath, args: loopArgs, loop: true, enable: window, reversed: true},
		{name: "gif played once on a reversed base", path: gifPath, reversed: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.needs9 && major < 9 {
				t.Skip("decoding animated WebP needs FFmpeg 9")
			}
			p := overlayPlanFor(t, info, c)
			frames := renderMaster(t, ff, clip, p)
			n := len(frames)
			if n != len(plainFrames) {
				t.Fatalf("master has %d frames, plain render %d", n, len(plainFrames))
			}
			// The render itself must follow the overlay schedule, or the still
			// comparison below would be vacuous.
			for k, fr := range frames {
				tk := float64(k) / p.FPS
				plainK := k
				if c.reversed {
					plainK = n - 1 - k
				}
				if !enabledAt(c, tk) {
					if !bytes.Equal(fr, plainFrames[plainK]) {
						t.Errorf("slot %d (t=%.3f): overlay drawn outside its window", k, tk)
					}
					continue
				}
				if got, want := rgbaAt(fr, p.Width, 2, 2), expectedOverlay(c, k); got != want {
					t.Errorf("slot %d (t=%.3f): overlay pixel %v, want %v", k, tk, got, want)
				}
			}
			// Times before, at, just after and well inside the window edges,
			// plus the clip ends; the seeking still must agree with the render
			// at every one of them (its -itsoffset keeps the overlay clock and
			// the enable window on the render's time).
			ts := []float64{0, 0.5, 0.95, 0.983, 1, 1.017, 1.05, 1.5, 2, 2.37, 2.983, 3, 3.017, 3.5, 3.99, 4}
			checkStills(t, ff, clip, p, frames, ts)
		})
	}
}

func TestProxyWithOverlay(t *testing.T) {
	ff := ffmpegOrSkip(t)
	if ffmpegMajor(t, ff) < 9 {
		t.Skip("decoding animated WebP needs FFmpeg 9")
	}
	dir := t.TempDir()
	clip, info := cfrClip(t, ff, dir)
	c := overlayCase{path: numberedGIF(t, dir), args: []string{"-stream_loop", "-1"}, loop: true, enable: "between(t,1,3)"}
	p := overlayPlanFor(t, info, c)
	plain := compile(t, info, nil, recipe.Output{Format: "gif"})

	// The 160 px clip is narrower than the 360 px preview cap, so the proxy
	// keeps the plan's size.
	w, h := p.Width, p.Height
	decodeProxy := func(plan *graph.Plan, name string) [][]byte {
		out := filepath.Join(dir, name)
		run(t, ff, enc.ProxyArgs(clip, plan, 0, 0, out))
		pix := run(t, ff, []string{"-i", out, "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"})
		if len(pix) == 0 || len(pix)%(w*h*4) != 0 {
			t.Fatalf("%s decodes to %d bytes, not a multiple of %dx%dx4", name, len(pix), w, h)
		}
		var frames [][]byte
		for off := 0; off < len(pix); off += w * h * 4 {
			frames = append(frames, pix[off:off+w*h*4])
		}
		return frames
	}
	frames := decodeProxy(p, "overlay.webp")
	plainFrames := decodeProxy(plain, "plain.webp")
	if len(frames) < 59 || len(frames) != len(plainFrames) {
		t.Fatalf("proxy has %d frames (plain %d), want 60 (4 s at 15 fps)", len(frames), len(plainFrames))
	}
	near := func(a, b color.RGBA, tol int) bool {
		d := func(x, y uint8) int { return int(math.Abs(float64(x) - float64(y))) }
		return d(a.R, b.R) <= tol && d(a.G, b.G) <= tol && d(a.B, b.B) <= tol
	}
	// Frame 0 (t=0) lies outside the window: no overlay, same as the plain
	// proxy. Frame 22 (t=1.467) shows base slot 44 → overlay frame 4 (R=100).
	if got, want := rgbaAt(frames[0], w, 2, 2), rgbaAt(plainFrames[0], w, 2, 2); !near(got, want, 8) {
		t.Errorf("proxy frame 0 pixel %v, plain %v: overlay drawn outside its window", got, want)
	}
	if got := rgbaAt(frames[22], w, 2, 2); !near(got, color.RGBA{overlayRed(4), 0, 0, 255}, 16) {
		t.Errorf("proxy frame 22 pixel %v, want the overlay's frame 4 (R=%d) within lossy tolerance", got, overlayRed(4))
	}
}

// decodeWebP decodes an animated WebP (a proxy) into RGBA frames of w x h.
func decodeWebP(t *testing.T, ff, path string, w, h int) [][]byte {
	t.Helper()
	pix := run(t, ff, []string{"-i", path, "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"})
	if len(pix) == 0 || len(pix)%(w*h*4) != 0 {
		t.Fatalf("%s decodes to %d bytes, not a multiple of %dx%dx4", filepath.Base(path), len(pix), w, h)
	}
	var frames [][]byte
	for off := 0; off < len(pix); off += w * h * 4 {
		frames = append(frames, pix[off:off+w*h*4])
	}
	return frames
}

// TestProxyOfStillWithAnimatedOverlay: a still main source under an animated
// overlay compiles to a single-frame plan at the output rate (25 fps here,
// the Emote/Sticker presets' rate); its proxy must not run that one frame
// through fps=15 — the fps filter emits nothing for a lone frame and
// libwebp_anim then fails ("WebPAnimEncoderAssemble() failed", a 500 in the
// UI). The proxy decodes to exactly that one composited frame.
func TestProxyOfStillWithAnimatedOverlay(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	still := greenPNG(t, filepath.Join(dir, "still.png"), 40, 30, image.Rect(4, 4, 36, 26))
	gifPath := numberedGIF(t, dir)
	gifInfo := recipe.ProbeInfo{Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8, Width: ovSize, Height: ovSize, FPS: 10, Duration: 1, Frames: ovFrames, Kind: recipe.KindAnimation}
	for _, fps := range []float64{25, 10} {
		t.Run(fmt.Sprintf("fps %v", fps), func(t *testing.T) {
			p, err := graph.CompileWithSources([]recipe.ProbeInfo{stillInfo(40, 30), gifInfo},
				[]recipe.Op{op(recipe.OpOverlay, recipe.OverlayParams{Source: 1})}, recipe.Output{Format: "webp", FPS: fps})
			if err != nil {
				t.Fatalf("CompileWithSources: %v", err)
			}
			if p.Frames != 1 || p.FPS != fps || len(p.ExtraInputs) != 1 {
				t.Fatalf("still + animated overlay plan: Frames=%d FPS=%v extra=%d (%s)", p.Frames, p.FPS, len(p.ExtraInputs), p.Filter)
			}
			p.ExtraInputs[0].Path = gifPath
			out := filepath.Join(dir, fmt.Sprintf("still%v.webp", fps))
			args := enc.ProxyArgs(still, p, 0, 0, out)
			if strings.Contains(args[slices.Index(args, "-filter_complex")+1], "[out]fps=") {
				t.Errorf("single-frame plan proxy runs through an fps stage: %q", args)
			}
			run(t, ff, args)
			frames := decodeWebP(t, ff, out, p.Width, p.Height)
			if len(frames) != 1 {
				t.Fatalf("proxy has %d frames, want 1", len(frames))
			}
			// The overlay's first frame sits at (0,0) over the still.
			if got := rgbaAt(frames[0], p.Width, 2, 2); got.R < overlayRed(0)-12 || got.R > overlayRed(0)+12 || got.G > 40 {
				t.Errorf("proxy pixel (2,2) %v, want the overlay's frame 0 (R=%d) within lossy tolerance", got, overlayRed(0))
			}
		})
	}
}

// tailFrames is the frame count of TestProxyReversedTail's animations.
const tailFrames = 30

// tailColor numbers the frames of TestProxyReversedTail's animations with
// colours at least 40 apart per channel (six red levels by five blue levels):
// the proxy is lossy (-q:v 60, 4:2:0) and lands up to ~10 values off a flat
// colour, which would blur frameColor's 8-apart red steps together.
func tailColor(i int) color.RGBA {
	return color.RGBA{R: uint8(20 + 40*(i%6)), G: 0, B: uint8(10 + 50*(i/6)), A: 255}
}

// nearestTail returns the tailColor frame whose colour is closest to c and
// that distance (the sum of the per-channel differences).
func nearestTail(c color.RGBA) (int, int) {
	best, bestD := -1, math.MaxInt
	for i := 0; i < tailFrames; i++ {
		w := tailColor(i)
		d := int(math.Abs(float64(c.R)-float64(w.R))) + int(math.Abs(float64(c.G)-float64(w.G))) + int(math.Abs(float64(c.B)-float64(w.B)))
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best, bestD
}

// TestProxyReversedTail: the proxy of a reversed plan longer than the preview
// seeks the main input to just before the tail it shows, and that seeked
// proxy is byte-for-byte the proxy the unseeked decode produced (the same
// frames reach the encoder with the same timestamps): on a CFR video at the
// source rate, on a 30 fps video resampled to 25 fps (the seek snaps to
// whole source frames), on a trimmed and on an untrimmed GIF (the plain
// animation-source seek) — and an animated WebP, whose demuxer decodes
// nothing after a seek (Plan.SeekUnsafe), is left alone, trimmed or not,
// and still decodes.
func TestProxyReversedTail(t *testing.T) {
	ff := ffmpegOrSkip(t)
	major := ffmpegMajor(t, ff)
	dir := t.TempDir()
	gifPath, gifInfo := coloredAnimation(t, filepath.Join(dir, "numbered.gif"), tailFrames, func(int) int { return 10 }, tailColor)
	movPath := convert(t, ff, gifPath, filepath.Join(dir, "numbered.mov"), "-r", "10", "-c:v", "png", "-pix_fmt", "rgb24")
	movInfo := gifInfo
	movInfo.Format, movInfo.Codec, movInfo.PixFmt, movInfo.Kind = "mov,mp4,m4a,3gp,3g2,mj2", "png", "rgb24", recipe.KindVideo
	webpPath := convert(t, ff, gifPath, filepath.Join(dir, "numbered.webp"), "-c:v", "libwebp_anim", "-lossless", "1", "-loop", "0")
	webpInfo := gifInfo
	webpInfo.Format, webpInfo.Codec, webpInfo.PixFmt = "webp_anim", "webp_anim", "argb"
	clip, clipInfo := cfrClip(t, ff, dir)
	rev := recipe.Op{Kind: recipe.OpReverse}

	cases := []struct {
		name       string
		src        string
		info       recipe.ProbeInfo
		ops        []recipe.Op
		out        recipe.Output
		maxSeconds float64
		seek       bool // the proxy must seek (else it must not)
		numbered   bool // frames are numbered: the tail is checked by colour
		needs9     bool
	}{
		{"png video at its 10 fps", movPath, movInfo, []recipe.Op{rev}, recipe.Output{Format: "gif"}, 1, true, true, false},
		{"png video trimmed 0.5..2.5", movPath, movInfo, []recipe.Op{trimOp(0.5, 2.5), rev}, recipe.Output{Format: "gif"}, 1, true, true, false},
		{"30 fps video at 25 fps (aligned seek)", clip, clipInfo, []recipe.Op{rev}, recipe.Output{Format: "webp", FPS: 25}, 1.5, true, false, false},
		{"30 fps video at speed 2", clip, clipInfo, []recipe.Op{op(recipe.OpSpeed, recipe.SpeedParams{Factor: 2}), rev}, recipe.Output{Format: "gif"}, 0.8, true, false, false},
		{"gif trimmed 0.5..3 (plain seek)", gifPath, gifInfo, []recipe.Op{trimOp(0.5, 3), rev}, recipe.Output{Format: "gif"}, 1, true, true, false},
		{"untrimmed gif (plain seek)", gifPath, gifInfo, []recipe.Op{rev}, recipe.Output{Format: "gif"}, 1, true, true, false},
		{"untrimmed animated webp (SeekUnsafe) is not seeked", webpPath, webpInfo, []recipe.Op{rev}, recipe.Output{Format: "gif"}, 1, false, true, true},
		{"trimmed animated webp (filter trim) is not seeked", webpPath, webpInfo, []recipe.Op{trimOp(0.5, 3), rev}, recipe.Output{Format: "gif"}, 1, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needs9 && major < 9 {
				t.Skip("decoding animated WebP needs FFmpeg 9")
			}
			p := compile(t, tc.info, tc.ops, tc.out)
			if !p.Reversed || p.Duration <= tc.maxSeconds {
				t.Fatalf("plan: Reversed=%v Duration=%v (%s)", p.Reversed, p.Duration, p.Filter)
			}
			out := filepath.Join(t.TempDir(), "proxy.webp")
			args := enc.ProxyArgs(tc.src, p, 0, tc.maxSeconds, out)
			in := slices.Index(args, "-i")
			seeked := slices.Index(args[:in], "-ss") >= 0 && !slices.Equal(args[:in], p.InputArgs)
			if seeked != tc.seek {
				t.Fatalf("proxy argv %q: seeked=%v, want %v (plan InputArgs %q)", args[:in+2], seeked, tc.seek, p.InputArgs)
			}
			run(t, ff, args)
			frames := decodeWebP(t, ff, out, p.Width, p.Height)
			want := int(math.Floor(tc.maxSeconds*math.Min(p.FPS, 15) + 1e-6))
			if len(frames) < want-1 || len(frames) > want+1 {
				t.Errorf("proxy has %d frames, want about %d", len(frames), want)
			}
			if seeked {
				// The unseeked form: the plan's own input args in front of
				// the same output options.
				base := filepath.Join(t.TempDir(), "base.webp")
				baseArgs := append(slices.Clone(p.InputArgs), args[in:]...)
				baseArgs[len(baseArgs)-1] = base
				run(t, ff, baseArgs)
				baseFrames := decodeWebP(t, ff, base, p.Width, p.Height)
				if len(baseFrames) != len(frames) {
					t.Fatalf("seeked proxy has %d frames, the unseeked one %d", len(frames), len(baseFrames))
				}
				for i := range frames {
					if !bytes.Equal(frames[i], baseFrames[i]) {
						t.Errorf("seeked proxy frame %d differs from the unseeked proxy's", i)
					}
				}
			}
			if !tc.numbered {
				return
			}
			// The tail, by number: proxy frame m shows source frame last - m
			// (the trim's last source frame first).
			last := gifInfo.Frames - 1
			if p.TrimEnd > 0 {
				last = int(math.Round(p.TrimEnd*10)) - 1
			}
			for m, fr := range frames {
				got, dist := nearestTail(rgbaAt(fr, p.Width, animBoxX+8, animBoxY+8))
				if got != last-m || dist > 16 {
					t.Errorf("proxy frame %d shows source frame %d (colour distance %d), want %d", m, got, dist, last-m)
				}
			}
		})
	}
}

// movingBoxSequence writes n transparent 160x120 frames with an opaque
// 60x40 box that jumps 20 px right every 5 frames (x = 20 + 20*floor(i/5)),
// 000001.png … for the image2 demuxer. The detection samples ~5 frames a
// second, so a box that stays put for 0.5 s is seen at every position.
func movingBoxSequence(t *testing.T, dir string, n int) string {
	t.Helper()
	seq := filepath.Join(dir, "seq")
	if err := os.MkdirAll(seq, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		x := 20 + 20*(i/5)
		alphaPNG(t, filepath.Join(seq, fmt.Sprintf("%06d.png", i+1)), 160, 120, image.Rect(x, 10, x+60, 50), 255)
	}
	return seq
}

func TestCropDetectReal(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	type box struct{ w, h, x, y int }
	detect := func(src string, inputArgs []string, alpha bool, threshold int) (box, bool) {
		t.Helper()
		w, h, x, y, ok := enc.ParseCropDetect(runLog(t, ff, enc.CropDetectArgs(src, inputArgs, alpha, threshold)))
		return box{w, h, x, y}, ok
	}
	expect := func(name string, got box, ok bool, want box, wantOK bool) {
		t.Helper()
		if ok != wantOK || (ok && got != want) {
			t.Errorf("%s: got %+v ok=%v, want %+v ok=%v", name, got, ok, want, wantOK)
		}
	}

	odd := alphaPNG(t, filepath.Join(dir, "odd.png"), 160, 120, image.Rect(21, 11, 82, 52), 255)
	faint := alphaPNG(t, filepath.Join(dir, "faint.png"), 160, 120, image.Rect(20, 10, 80, 50), 76)
	line := alphaPNG(t, filepath.Join(dir, "line.png"), 500, 300, image.Rect(100, 50, 101, 250), 255)
	corner := alphaPNG(t, filepath.Join(dir, "corner.png"), 500, 300, image.Rect(499, 299, 500, 300), 255)
	empty := alphaPNG(t, filepath.Join(dir, "empty.png"), 64, 48, image.Rectangle{}, 255)

	t.Run("alpha stills", func(t *testing.T) {
		// Exact boxes at odd offsets, for every threshold the content passes.
		for _, th := range []int{0, 1, 128, 255} {
			got, ok := detect(odd, nil, true, th)
			expect("odd box, threshold "+strconv.Itoa(th), got, ok, box{61, 41, 21, 11}, true)
		}
		// Looping the input (the former jobs wrapper for stills) changes
		// nothing: the sampler passes the first frame on its own.
		got, ok := detect(odd, []string{"-stream_loop", "24"}, true, 1)
		expect("odd box looped", got, ok, box{61, 41, 21, 11}, true)
		// alpha >= threshold counts: alpha 76 passes 76, not 77.
		got, ok = detect(faint, nil, true, 76)
		expect("faint at threshold 76", got, ok, box{60, 40, 20, 10}, true)
		got, ok = detect(faint, nil, true, 77)
		expect("faint at threshold 77", got, ok, box{}, false)
		// Thin and single-pixel content (cropdetect's line average would miss it).
		got, ok = detect(line, nil, true, 1)
		expect("1 px line", got, ok, box{1, 200, 100, 50}, true)
		got, ok = detect(corner, nil, true, 1)
		expect("corner pixel", got, ok, box{1, 1, 499, 299}, true)
		got, ok = detect(empty, nil, true, 1)
		expect("fully transparent", got, ok, box{}, false)
	})

	t.Run("alpha sequence: union over the trimmed frames", func(t *testing.T) {
		seq := movingBoxSequence(t, dir, 20)
		seqArgs := []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}
		pattern := seq + "/%06d.png"
		// All 20 frames (2 s at 10 fps): box positions 20, 40, 60, 80 → the
		// union spans x 20..139.
		got, ok := detect(pattern, seqArgs, true, 1)
		expect("whole sequence", got, ok, box{120, 40, 20, 10}, true)
		// Trimmed to 1..2 s (frames 11..20, i = 10..19): positions 60 and 80.
		got, ok = detect(pattern, append([]string{"-ss", "1", "-to", "2"}, seqArgs...), true, 1)
		expect("trimmed sequence", got, ok, box{80, 40, 60, 10}, true)
	})

	t.Run("opaque sources: cropdetect black borders", func(t *testing.T) {
		even := opaquePNG(t, filepath.Join(dir, "even.png"), 160, 120, image.Rect(20, 10, 80, 50))
		got, ok := detect(even, nil, false, 0)
		expect("even box still", got, ok, box{60, 40, 20, 10}, true)
		// cropdetect rounds to even: the box stays inside the content and
		// loses at most a pixel per side.
		oddOpaque := opaquePNG(t, filepath.Join(dir, "oddopaque.png"), 160, 120, image.Rect(21, 11, 82, 52))
		got, ok = detect(oddOpaque, nil, false, 0)
		if !ok || got.x < 21 || got.y < 11 || got.x+got.w > 82 || got.y+got.h > 52 || got.w < 59 || got.h < 39 {
			t.Errorf("odd opaque box: got %+v ok=%v, want within 61x41@21,11 by at most 1 px per side", got, ok)
		}
		flat := opaquePNG(t, filepath.Join(dir, "flat.png"), 64, 48, image.Rectangle{})
		got, ok = detect(flat, nil, false, 0)
		expect("flat black", got, ok, box{}, false)
		// A lossy video with a white box on black: within a couple of pixels.
		video := filepath.Join(dir, "box.mp4")
		run(t, ff, []string{"-f", "lavfi", "-i", "color=c=black:s=160x120:r=10:d=2", "-vf", "drawbox=x=20:y=10:w=60:h=40:c=white:t=fill", "-c:v", "mpeg4", "-q:v", "2", video})
		got, ok = detect(video, nil, false, 0)
		if !ok || got.x < 18 || got.x > 22 || got.y < 8 || got.y > 12 || got.w < 56 || got.w > 64 || got.h < 36 || got.h > 44 {
			t.Errorf("video box: got %+v ok=%v, want about 60x40@20,10", got, ok)
		}
	})
}

// greenScreenClip writes a 2 s, 10 fps, opaque (rgb24, lossless PNG in MOV)
// 160x120 clip: pure green with a red 40x30 square that jumps 20 px right
// every 0.5 s (x = 20 + 20*floor(2t), y = 20: positions 20, 40, 60, 80), so
// the 5 fps detection sampler sees every position. Returns the path and the
// probe info the pipeline would record.
func greenScreenClip(t *testing.T, ff, dir string) (string, recipe.ProbeInfo) {
	t.Helper()
	path := filepath.Join(dir, "green.mov")
	run(t, ff, []string{
		"-f", "lavfi", "-i", "color=c=0x00FF00:s=160x120:r=10:d=2[bg];color=c=0xDC1E1E:s=40x30:r=10:d=2[fg];[bg][fg]overlay=x='20+20*floor(2*t)':y=20:format=rgb",
		"-c:v", "png", "-pix_fmt", "rgb24", path,
	})
	info := recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "png", PixFmt: "rgb24", Bits: 8,
		Width: 160, Height: 120, FPS: 10, Duration: 2, Frames: 20, Kind: recipe.KindVideo,
	}
	return path, info
}

// greenPNG writes a w x h pure-green opaque PNG with a red box.
func greenPNG(t *testing.T, path string, w, h int, box image.Rectangle) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBA{0, 255, 0, 255}
			if image.Pt(x, y).In(box) {
				c = color.NRGBA{220, 30, 30, 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return writePNG(t, path, img)
}

// stillInfo is the probe info of an opaque w x h PNG still.
func stillInfo(w, h int) recipe.ProbeInfo {
	return recipe.ProbeInfo{Format: "png_pipe", Codec: "png", PixFmt: "rgb24", Bits: 8, Width: w, Height: h, Frames: 1, IsStill: true, Kind: recipe.KindImage}
}

func TestCropDetectPlanReal(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	clip, info := greenScreenClip(t, ff, dir)
	type box struct{ w, h, x, y int }
	full := box{160, 120, 0, 0}
	// detectPlan compiles ops (as the detection plan in front of an autocrop)
	// and runs the plan builder with the plan's own alpha flag.
	detectPlan := func(src string, info recipe.ProbeInfo, ops []recipe.Op, threshold int) (box, bool, *graph.Plan) {
		t.Helper()
		p := compile(t, info, ops, recipe.Output{})
		w, h, x, y, ok := enc.ParseCropDetect(runLog(t, ff, enc.CropDetectPlanArgs(src, p, p.HasAlpha, threshold)))
		return box{w, h, x, y}, ok, p
	}
	expect := func(name string, got box, ok bool, want box, wantOK bool) {
		t.Helper()
		if ok != wantOK || (ok && got != want) {
			t.Errorf("%s: got %+v ok=%v, want %+v ok=%v", name, got, ok, want, wantOK)
		}
	}
	chroma := op(recipe.OpChromaKey, recipe.ChromaKeyParams{Color: "00ff00"})
	colorKey := op(recipe.OpColorKey, recipe.ColorKeyParams{Color: "00ff00", Similarity: 0.1})

	t.Run("raw source path: opaque green is all content", func(t *testing.T) {
		w, h, x, y, ok := enc.ParseCropDetect(runLog(t, ff, enc.CropDetectArgs(clip, nil, info.HasAlpha, 1)))
		expect("raw cropdetect", box{w, h, x, y}, ok, full, true)
		// The plan path without keying measures the same picture.
		got, ok, p := detectPlan(clip, info, nil, 1)
		if p.HasAlpha {
			t.Fatalf("unkeyed plan reports alpha: %s", p.Filter)
		}
		expect("unkeyed plan", got, ok, full, true)
	})

	// ring checks that got is want grown by exactly ring px on every side
	// (clamped to the frame): chromakey judges each pixel by its 3x3
	// neighbourhood, so the screen pixels touching the subject keep a little
	// alpha, which the alpha > 0 default threshold counts as content — the
	// render keeps those pixels too, so the crop must include them.
	ring := func(name string, got box, ok bool, want box, r int) {
		t.Helper()
		grown := box{min(want.w+2*r, 160-max(want.x-r, 0)), min(want.h+2*r, 120-max(want.y-r, 0)), max(want.x-r, 0), max(want.y-r, 0)}
		expect(name, got, ok, grown, true)
	}

	t.Run("keyed plan: the subject's union box", func(t *testing.T) {
		// Positions 20, 40, 60, 80 of a 40 px square: x 20..119.
		want := box{100, 30, 20, 20}
		got, ok, p := detectPlan(clip, info, []recipe.Op{colorKey}, 1)
		if !p.HasAlpha {
			t.Fatalf("colorkey plan reports no alpha: %s", p.Filter)
		}
		expect("colorkey (per pixel): exact", got, ok, want, true)
		got, ok, p = detectPlan(clip, info, []recipe.Op{chroma}, 1)
		if !p.HasAlpha {
			t.Fatalf("chromakey plan reports no alpha: %s", p.Filter)
		}
		ring("chromakey (3x3 neighbourhood): one soft pixel around", got, ok, want, 1)
		// The soft ring is well below full alpha: a hard threshold drops it.
		got, ok, _ = detectPlan(clip, info, []recipe.Op{chroma}, 255)
		expect("chromakey, threshold 255", got, ok, want, true)
		// Trimmed to [1, 2) s: positions 60 and 80 only → x 60..119.
		got, ok, _ = detectPlan(clip, info, []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 1, End: 2}), colorKey}, 1)
		expect("trimmed keyed clip", got, ok, box{60, 30, 60, 20}, true)
		got, ok, _ = detectPlan(clip, info, []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 1, End: 2}), chroma}, 1)
		ring("trimmed chromakeyed clip", got, ok, box{60, 30, 60, 20}, 1)
		// Speed and fps stages in front of the key change nothing about the box.
		got, ok, _ = detectPlan(clip, info, []recipe.Op{op(recipe.OpSpeed, recipe.SpeedParams{Factor: 2}), op(recipe.OpFPS, recipe.FPSParams{FPS: 5}), colorKey}, 1)
		expect("speed 2 + fps 5 + colorkey", got, ok, want, true)
	})

	t.Run("single-frame plans need no loop", func(t *testing.T) {
		// A keyed still: no fps stage in the plan, one sampled frame, exact
		// odd-offset box on the keyed alpha.
		still := greenPNG(t, filepath.Join(dir, "greenstill.png"), 160, 120, image.Rect(21, 11, 82, 52))
		got, ok, p := detectPlan(still, stillInfo(160, 120), []recipe.Op{colorKey}, 1)
		if strings.Contains(p.Filter, "fps=") || len(p.InputArgs) != 0 {
			t.Fatalf("still plan: args %q filter %s", p.InputArgs, p.Filter)
		}
		expect("colorkeyed still", got, ok, box{61, 41, 21, 11}, true)
		got, ok, _ = detectPlan(still, stillInfo(160, 120), []recipe.Op{chroma}, 1)
		ring("chromakeyed still", got, ok, box{61, 41, 21, 11}, 1)
		// An unkeyed green still is all content, like the raw path.
		got, ok, _ = detectPlan(still, stillInfo(160, 120), nil, 1)
		expect("unkeyed still", got, ok, full, true)
		// An opaque black-bordered still through the plan path: the same
		// cropdetect box as CropDetectArgs finds on the raw file.
		even := opaquePNG(t, filepath.Join(dir, "even2.png"), 160, 120, image.Rect(20, 10, 80, 50))
		got, ok, _ = detectPlan(even, stillInfo(160, 120), nil, 1)
		expect("opaque still", got, ok, box{60, 40, 20, 10}, true)
		w, h, x, y, rawOK := enc.ParseCropDetect(runLog(t, ff, enc.CropDetectArgs(even, nil, false, 1)))
		expect("opaque still, raw path", box{w, h, x, y}, rawOK, box{60, 40, 20, 10}, true)
	})

	t.Run("alpha sequence plan: union over the trimmed frames", func(t *testing.T) {
		seq := movingBoxSequence(t, dir, 20)
		seqInfo := recipe.ProbeInfo{
			Format: "image2", Codec: "png", PixFmt: "rgba", Bits: 8, Width: 160, Height: 120, FPS: 10, Duration: 2, Frames: 20,
			HasAlpha: true, Kind: recipe.KindSequence, Sequence: &recipe.SequenceInfo{Count: 20, Pattern: "%06d.png", DelayMS: 100},
		}
		got, ok, p := detectPlan(seq, seqInfo, nil, 1)
		if p.InputPattern != "%06d.png" {
			t.Fatalf("sequence plan: %+v", p)
		}
		expect("whole sequence", got, ok, box{120, 40, 20, 10}, true)
		got, ok, _ = detectPlan(seq, seqInfo, []recipe.Op{op(recipe.OpTrim, recipe.TrimParams{Start: 1, End: 2})}, 1)
		expect("trimmed sequence", got, ok, box{80, 40, 60, 10}, true)
	})
}

// --- animation sources ----------------------------------------------------------

// Numbered animation geometry: animW x animH frames, black with a box at
// (animBoxX, animBoxY) of animBoxW x animBoxH (even values, so cropdetect's
// round=2 reports it exactly).
const (
	animW, animH       = 40, 30
	animBoxX, animBoxY = 4, 4
	animBoxW, animBoxH = 32, 22
)

// frameColor is the box colour of numbered animation frame i: the red
// channel numbers the frame, the green keeps the box far above cropdetect's
// black limit whatever the number.
func frameColor(i int) color.RGBA { return color.RGBA{R: uint8(8 * (i + 1)), G: 200, B: 0, A: 255} }

// numberedAnimation writes an n-frame (n <= 31) GIF of frameColor boxes on
// black, delayCS(i) centiseconds per frame, looping forever, and returns the
// probe info the pipeline records for such a file: Kind animation, FPS =
// ffmpeg's base-cadence estimate (100/delayCS(0)), Duration the sum of the
// delays, Frames n.
func numberedAnimation(t *testing.T, path string, n int, delayCS func(i int) int) (string, recipe.ProbeInfo) {
	t.Helper()
	if n > 31 {
		t.Fatalf("numberedAnimation: %d frames exceed the red-channel numbering", n)
	}
	return coloredAnimation(t, path, n, delayCS, frameColor)
}

// coloredAnimation is numberedAnimation with the box colour of frame i given
// by colorOf (every frame's colour must be distinct).
func coloredAnimation(t *testing.T, path string, n int, delayCS func(i int) int, colorOf func(i int) color.RGBA) (string, recipe.ProbeInfo) {
	t.Helper()
	pal := make(color.Palette, 0, n+1)
	pal = append(pal, color.RGBA{0, 0, 0, 255})
	for i := 0; i < n; i++ {
		pal = append(pal, colorOf(i))
	}
	g := &gif.GIF{LoopCount: 0}
	total := 0
	for i := 0; i < n; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, animW, animH), pal)
		for y := animBoxY; y < animBoxY+animBoxH; y++ {
			for x := animBoxX; x < animBoxX+animBoxW; x++ {
				fr.SetColorIndex(x, y, uint8(i+1))
			}
		}
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, delayCS(i))
		g.Disposal = append(g.Disposal, gif.DisposalNone)
		total += delayCS(i)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{
		Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8,
		Width: animW, Height: animH, FPS: 100 / float64(delayCS(0)), Duration: float64(total) / 100, Frames: n,
		Kind: recipe.KindAnimation,
	}
	return path, info
}

// msRound rounds a still time to milliseconds the way jobs.clampStillTime
// does before a still is rendered.
func msRound(t float64) float64 { return float64(int64(t*1000+0.5)) / 1000 }

// slotMidpoints returns the (ms-rounded) midpoint of every one of the n
// output slots of p — what the scrubber sends for each frame.
func slotMidpoints(p *graph.Plan, n int) []float64 {
	ts := make([]float64, 0, n)
	for k := 0; k < n; k++ {
		ts = append(ts, msRound((float64(k)+0.5)/p.FPS))
	}
	return ts
}

// trimOp builds a trim op.
func trimOp(start, end float64) recipe.Op {
	return op(recipe.OpTrim, recipe.TrimParams{Start: start, End: end})
}

// holdDelays is the delay schedule of TestStillReversedVFR's GIF: 100 ms
// frames with frame 12 held for 600 ms, so the source is variable-rate with
// a base cadence of 10 fps (the probe's estimate) and a hold a seek can land
// inside.
func holdDelays(i int) int {
	if i == 12 {
		return 60
	}
	return 10
}

func TestStillReversedVFR(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	src, info := numberedAnimation(t, filepath.Join(dir, "hold.gif"), 30, holdDelays)
	rev := recipe.Op{Kind: recipe.OpReverse}
	cases := []struct {
		name string
		ops  []recipe.Op
		out  recipe.Output
	}{
		{"gif@10 no trim", nil, recipe.Output{Format: "gif"}},
		{"gif@10 trim 0.5..3", []recipe.Op{trimOp(0.5, 3)}, recipe.Output{Format: "gif"}},
		{"webp@25 trim 0.5..3", []recipe.Op{trimOp(0.5, 3)}, recipe.Output{Format: "webp", FPS: 25}},
		{"gif@10 trim 1..3.2", []recipe.Op{trimOp(1, 3.2)}, recipe.Output{Format: "gif"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fwd := compile(t, info, tc.ops, tc.out)
			p := compile(t, info, append(slices.Clone(tc.ops), rev), tc.out)
			if !p.Reversed || !p.SourceVFR {
				t.Fatalf("reversed plan of an animation source: Reversed=%v SourceVFR=%v (%s)", p.Reversed, p.SourceVFR, p.Filter)
			}
			fwdFrames := renderMaster(t, ff, src, fwd)
			frames := renderMaster(t, ff, src, p)
			n := len(frames)
			if n != len(fwdFrames) {
				t.Fatalf("reversed master has %d frames, forward %d", n, len(fwdFrames))
			}
			for i := range frames {
				if !bytes.Equal(frames[i], fwdFrames[n-1-i]) {
					t.Fatalf("reversed frame %d is not forward frame %d", i, n-1-i)
				}
			}
			if n != p.Frames {
				t.Logf("master has %d frames, plan expected %d", n, p.Frames)
			}
			// Every slot's midpoint, as the scrubber sends them; the seeking
			// variant must be as exact as the from-start one.
			checkStills(t, ff, src, p, frames, slotMidpoints(p, n))
			// The forward control of the same recipe.
			checkStills(t, ff, src, fwd, fwdFrames, slotMidpoints(fwd, n))
		})
	}
}

func TestFilterTrimWebP(t *testing.T) {
	ff := ffmpegOrSkip(t)
	if ffmpegMajor(t, ff) < 9 {
		t.Skip("decoding animated WebP needs FFmpeg 9")
	}
	dir := t.TempDir()
	gifPath, gifInfo := numberedAnimation(t, filepath.Join(dir, "numbered.gif"), 30, func(int) int { return 10 })
	src := convert(t, ff, gifPath, filepath.Join(dir, "numbered.webp"), "-c:v", "libwebp_anim", "-lossless", "1", "-loop", "0")
	info := gifInfo
	info.Format, info.Codec, info.PixFmt = "webp_anim", "webp_anim", "argb"
	// The premise: this demuxer decodes nothing after an input seek (the
	// plan must therefore trim in the filter; the checks below hold either
	// way).
	if out := run(t, ff, []string{"-ss", "0.5", "-i", src, "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"}); len(out) != 0 {
		t.Logf("this ffmpeg decodes %d bytes of the webp after -ss 0.5", len(out))
	}
	rev := recipe.Op{Kind: recipe.OpReverse}
	cases := []struct {
		name   string
		ops    []recipe.Op
		out    recipe.Output
		frames int // master frames: (end - start) * fps
		first  int // source frame shown by master frame 0
	}{
		{"trim 0.5..1.5", []recipe.Op{trimOp(0.5, 1.5)}, recipe.Output{Format: "gif"}, 10, 5},
		{"trim 1 to the end", []recipe.Op{trimOp(1, 3)}, recipe.Output{Format: "gif"}, 20, 10},
		{"trim 0..1.5 (end only)", []recipe.Op{trimOp(0, 1.5)}, recipe.Output{Format: "gif"}, 15, 0},
		{"trim 0.5..2.5 reversed", []recipe.Op{trimOp(0.5, 2.5), rev}, recipe.Output{Format: "gif"}, 20, 24},
		// Speed 2 pairs the source frames (5,6), (7,8), … onto the 10 fps
		// grid and the fps stage keeps the later of each pair, as it does
		// after an input seek (see TestStillReachesLastFrameOfSequence).
		{"trim 0.5..2.5 at speed 2", []recipe.Op{trimOp(0.5, 2.5), op(recipe.OpSpeed, recipe.SpeedParams{Factor: 2})}, recipe.Output{Format: "gif"}, 10, 6},
		{"trim 0.5..1.5 resampled to 25 fps", []recipe.Op{trimOp(0.5, 1.5)}, recipe.Output{Format: "webp", FPS: 25}, 25, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := compile(t, info, tc.ops, tc.out)
			if !p.FilterTrim || slices.Contains(p.InputArgs, "-ss") || slices.Contains(p.InputArgs, "-to") || !strings.Contains(p.Filter, "trim=start=") {
				t.Fatalf("a trimmed webp_anim plan must trim in the filter: FilterTrim=%v InputArgs=%q filter %s", p.FilterTrim, p.InputArgs, p.Filter)
			}
			frames := renderMaster(t, ff, src, p)
			if len(frames) != tc.frames {
				t.Fatalf("master has %d frames, want %d (plan %d)", len(frames), tc.frames, p.Frames)
			}
			if got, want := rgbaAt(frames[0], p.Width, 10, 10), frameColor(tc.first); got != want {
				t.Errorf("master frame 0 pixel %v, want source frame %d (%v)", got, tc.first, want)
			}
			ts := append(slotMidpoints(p, len(frames)), 0, p.Duration, p.Duration+3)
			checkStills(t, ff, src, p, frames, ts)

			// The proxy decodes to the plan's frames (capped at 15 fps).
			out := filepath.Join(t.TempDir(), "proxy.webp")
			run(t, ff, enc.ProxyArgs(src, p, 0, 0, out))
			pix := run(t, ff, []string{"-i", out, "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"})
			frameBytes := p.Width * p.Height * 4
			if len(pix) == 0 || len(pix)%frameBytes != 0 {
				t.Fatalf("proxy decodes to %d bytes, not a multiple of %dx%dx4", len(pix), p.Width, p.Height)
			}
			got, want := len(pix)/frameBytes, len(frames)
			if p.FPS > 15 {
				want = int(math.Floor(p.Duration*15 + 1e-6))
			}
			if got < want-1 || got > want+1 {
				t.Errorf("proxy has %d frames, want about %d", got, want)
			}

			// The autocrop detection plan sees the trimmed frames and finds
			// the box (it used to see no frame at all and report nothing).
			dp, err := graph.CompileDetect([]recipe.ProbeInfo{info}, tc.ops)
			if err != nil {
				t.Fatalf("CompileDetect: %v", err)
			}
			if !dp.FilterTrim {
				t.Fatalf("detection plan does not trim in the filter: %q %s", dp.InputArgs, dp.Filter)
			}
			w, h, x, y, ok := enc.ParseCropDetect(runLog(t, ff, enc.CropDetectPlanArgs(src, dp, dp.HasAlpha, 1)))
			if !ok || w != animBoxW || h != animBoxH || x != animBoxX || y != animBoxY {
				t.Errorf("detection box %d:%d:%d:%d ok=%v, want %d:%d:%d:%d", w, h, x, y, ok, animBoxW, animBoxH, animBoxX, animBoxY)
			}
		})
	}
}

// TestSeekUnsafeWebPUntrimmed: an untrimmed animated WebP plan is SeekUnsafe
// without FilterTrim, so StillArgs decodes its frame in ONE unseeked run (it
// used to emit -ss, get nothing from the webp_anim demuxer and leave jobs to
// retry with StillArgsFromStart), the reversed stills match the reversed
// master, and the reversed proxy of such a plan is not seeked for its tail
// (a seekable source would be) yet decodes to that tail.
func TestSeekUnsafeWebPUntrimmed(t *testing.T) {
	ff := ffmpegOrSkip(t)
	if ffmpegMajor(t, ff) < 9 {
		t.Skip("decoding animated WebP needs FFmpeg 9")
	}
	dir := t.TempDir()
	gifPath, gifInfo := numberedAnimation(t, filepath.Join(dir, "numbered.gif"), 30, func(int) int { return 10 })
	src := convert(t, ff, gifPath, filepath.Join(dir, "numbered.webp"), "-c:v", "libwebp_anim", "-lossless", "1", "-loop", "0")
	info := gifInfo
	info.Format, info.Codec, info.PixFmt = "webp_anim", "webp_anim", "argb"
	out := recipe.Output{Format: "gif"}
	noSeek := func(t *testing.T, what string, args []string) {
		t.Helper()
		main := slices.Index(args, "-i")
		if main < 0 {
			t.Fatalf("%s: no input in %q", what, args)
		}
		for _, opt := range []string{"-ss", "-itsoffset", "-to"} {
			if slices.Contains(args[:main], opt) {
				t.Fatalf("%s seeks a SeekUnsafe plan (%s): %q", what, opt, args)
			}
		}
	}

	p := compile(t, info, nil, out)
	if !p.SeekUnsafe || p.FilterTrim || len(p.InputArgs) != 0 || strings.Contains(p.Filter, "trim=") {
		t.Fatalf("an untrimmed webp_anim plan must be SeekUnsafe without a trim: SeekUnsafe=%v FilterTrim=%v InputArgs=%q filter %s", p.SeekUnsafe, p.FilterTrim, p.InputArgs, p.Filter)
	}
	frames := renderMaster(t, ff, src, p)
	if len(frames) != 30 {
		t.Fatalf("master has %d frames, want 30 (plan %d)", len(frames), p.Frames)
	}
	// t = 0.55 s is slot 5 of the 10 fps grid, source frame 5: one run, no
	// seek, the right frame.
	args := enc.StillArgs(src, p, 0.55, 0)
	noSeek(t, "StillArgs", args)
	data := run(t, ff, args)
	if len(data) == 0 {
		t.Fatalf("StillArgs produced no image in one run (args %q)", args)
	}
	img := decodePNG(t, data)
	if got, want := rgbaAt(img.Pix, p.Width, 10, 10), frameColor(5); got != want || frameIndex(frames, img) != 5 {
		t.Errorf("still at t=0.55 is master frame %d with box pixel %v, want frame 5 (%v)", frameIndex(frames, img), got, want)
	}
	checkStills(t, ff, src, p, frames, append(slotMidpoints(p, len(frames)), 0, p.Duration, p.Duration+3))

	// Reversed: the stills select by index from an unseeked decode, and the
	// proxy of a 1 s tail — which a seekable source would be seeked to at
	// about 1.8 s — decodes the whole clip and shows the last frame first.
	rp := compile(t, info, []recipe.Op{{Kind: recipe.OpReverse}}, out)
	if !rp.SeekUnsafe || !rp.Reversed {
		t.Fatalf("reversed plan: SeekUnsafe=%v Reversed=%v", rp.SeekUnsafe, rp.Reversed)
	}
	rframes := renderMaster(t, ff, src, rp)
	if len(rframes) != 30 || !bytes.Equal(rframes[0], frames[29]) {
		t.Fatalf("reversed master has %d frames, first one is master frame %d", len(rframes), frameIndex(frames, &image.NRGBA{Pix: rframes[0]}))
	}
	checkStills(t, ff, src, rp, rframes, append(slotMidpoints(rp, len(rframes)), 0, rp.Duration))
	proxy := filepath.Join(t.TempDir(), "proxy.webp")
	pargs := enc.ProxyArgs(src, rp, 0, 1, proxy)
	noSeek(t, "ProxyArgs", pargs)
	if gifArgs := enc.ProxyArgs(gifPath, compile(t, gifInfo, []recipe.Op{{Kind: recipe.OpReverse}}, out), 0, 1, proxy); gifArgs[0] != "-ss" {
		t.Fatalf("the control (the same clip as a GIF) is not seeked for its tail: %q", gifArgs)
	}
	run(t, ff, pargs)
	pix := run(t, ff, []string{"-i", proxy, "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"})
	frameBytes := rp.Width * rp.Height * 4
	if len(pix) == 0 || len(pix)%frameBytes != 0 {
		t.Fatalf("proxy decodes to %d bytes, not a multiple of %dx%dx4", len(pix), rp.Width, rp.Height)
	}
	if got := len(pix) / frameBytes; got < 9 || got > 11 {
		t.Errorf("proxy has %d frames, want about 10 (1 s at 10 fps)", got)
	}
	// Lossy yuva420p: the box colour survives within a few steps, and the
	// reversed first frame (29: R 240) is nowhere near frame 0 (R 8).
	if got, want := rgbaAt(pix, rp.Width, 10, 10), frameColor(29); math.Abs(float64(got.R)-float64(want.R)) > 16 || math.Abs(float64(got.G)-float64(want.G)) > 16 {
		t.Errorf("proxy frame 0 box pixel %v, want about source frame 29 (%v)", got, want)
	}
}

// Tags recording an ffmpeg command line with a crop filter, as tools that
// stamp their invocation into the container do (or as anyone can set): the
// first would pull a naive union to the top-left corner, the second would
// clamp the box to the full frame.
const (
	tagComment = "made with ffmpeg -vf crop=1:1:0:0"
	tagTitle   = "crop=9999:9999:0:0"
)

// taggedMOV writes the 10 frames of a lossless (png-in-MOV) 1 s clip showing
// src (a PNG still; alpha keeps its rgba, otherwise rgb24) with the comment
// and title tags above.
func taggedMOV(t *testing.T, ff, src, path string, alpha bool) string {
	t.Helper()
	pix := "rgb24"
	if alpha {
		pix = "rgba"
	}
	run(t, ff, []string{"-loop", "1", "-i", src, "-t", "1", "-r", "10", "-c:v", "png", "-pix_fmt", pix,
		"-metadata", "comment=" + tagComment, "-metadata", "title=" + tagTitle, path})
	return path
}

// commentGIF writes a 1 s, 10 fps opaque GIF (black with a white box) and
// splices a GIF comment extension carrying tagComment right after the global
// colour table — the gif demuxer exports the extension as the "comment" tag,
// so the app's primary input type echoes it exactly like a MOV does.
func commentGIF(t *testing.T, path string, w, h int, box image.Rectangle) string {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < 10; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := box.Min.Y; y < box.Max.Y; y++ {
			for x := box.Min.X; x < box.Max.X; x++ {
				fr.SetColorIndex(x, y, 1)
			}
		}
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 10)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) < 13 || string(b[:6]) != "GIF89a" {
		t.Fatalf("image/gif wrote %q, not a GIF89a header", b[:min(6, len(b))])
	}
	cut := 13 // header + logical screen descriptor
	if packed := b[10]; packed&0x80 != 0 {
		cut += 3 << ((packed & 7) + 1) // global colour table
	}
	ext := append([]byte{0x21, 0xFE, byte(len(tagComment))}, tagComment...)
	ext = append(ext, 0)
	if err := os.WriteFile(path, slices.Concat(b[:cut], ext, b[cut:]), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCropDetectIgnoresTagEchoes(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	type box struct{ w, h, x, y int }
	videoInfo := func(format, codec, pix string, alpha bool) recipe.ProbeInfo {
		kind := recipe.KindVideo
		if format == "gif" {
			kind = recipe.KindAnimation
		}
		return recipe.ProbeInfo{Format: format, Codec: codec, PixFmt: pix, Bits: 8, Width: 160, Height: 120, FPS: 10, Duration: 1, Frames: 10, HasAlpha: alpha, Kind: kind}
	}
	// detect runs both builders on src and returns their boxes; the raw log
	// must carry the echoed tags, or the case would prove nothing.
	detect := func(name, src string, info recipe.ProbeInfo, echoes ...string) (raw, planned box) {
		t.Helper()
		log := runLog(t, ff, enc.CropDetectArgs(src, nil, info.HasAlpha, 1))
		for _, echo := range echoes {
			if !strings.Contains(log, echo) {
				t.Fatalf("%s: ffmpeg's info log does not echo the tag %q; the fixture no longer reproduces the scenario:\n%s", name, echo, log)
			}
		}
		w, h, x, y, ok := enc.ParseCropDetect(log)
		if !ok {
			t.Fatalf("%s: raw path found no box:\n%s", name, log)
		}
		raw = box{w, h, x, y}
		p := compile(t, info, nil, recipe.Output{})
		if p.HasAlpha != info.HasAlpha {
			t.Fatalf("%s: plan alpha %v, want %v (%s)", name, p.HasAlpha, info.HasAlpha, p.Filter)
		}
		w, h, x, y, ok = enc.ParseCropDetect(runLog(t, ff, enc.CropDetectPlanArgs(src, p, p.HasAlpha, 1)))
		if !ok {
			t.Fatalf("%s: plan path found no box", name)
		}
		return raw, box{w, h, x, y}
	}
	expect := func(name string, raw, planned, want box) {
		t.Helper()
		if raw != want {
			t.Errorf("%s: raw path box %+v, want %+v", name, raw, want)
		}
		if planned != want {
			t.Errorf("%s: plan path box %+v, want %+v", name, planned, want)
		}
	}

	opaque := opaquePNG(t, filepath.Join(dir, "opaque.png"), 160, 120, image.Rect(20, 10, 80, 50))
	mov := taggedMOV(t, ff, opaque, filepath.Join(dir, "tagged.mov"), false)
	raw, planned := detect("opaque MOV", mov, videoInfo("mov,mp4,m4a,3gp,3g2,mj2", "png", "rgb24", false), tagComment, tagTitle)
	expect("opaque MOV (cropdetect)", raw, planned, box{60, 40, 20, 10})

	alpha := alphaPNG(t, filepath.Join(dir, "alpha.png"), 160, 120, image.Rect(21, 11, 82, 52), 255)
	amov := taggedMOV(t, ff, alpha, filepath.Join(dir, "tagged-alpha.mov"), true)
	raw, planned = detect("alpha MOV", amov, videoInfo("mov,mp4,m4a,3gp,3g2,mj2", "png", "rgba", true), tagComment, tagTitle)
	expect("alpha MOV (bbox)", raw, planned, box{61, 41, 21, 11})

	gifPath := commentGIF(t, filepath.Join(dir, "tagged.gif"), 160, 120, image.Rect(20, 10, 80, 50))
	raw, planned = detect("GIF comment extension", gifPath, videoInfo("gif", "gif", "bgra", false), tagComment)
	expect("GIF comment extension (cropdetect)", raw, planned, box{60, 40, 20, 10})
}
