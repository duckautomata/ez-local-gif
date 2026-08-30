package enc_test

// Real-tool checks of the Phase 4 argv: the MP4/WebM tails (flatten, even-dim
// pad, faststart, decodability), gifski (explicit frame list, fractional
// --fps) and the lossless gifsicle fast path (crop + frame range + drop with
// merged delays, open-ended "#a-" ranges, loop counts). Each test skips when
// its tool is not on PATH: the host runs the ffmpeg ones (libx264/libvpx are
// in the host build), the ezlg-dev image runs everything.

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
)

// videoTailChecks verifies what both video tails must produce for a 33x27
// (odd-dim) alpha master: even 34x28 frames, every frame present, opaque
// pixels, and the transparent region flattened onto the default matte.
func videoTailChecks(t *testing.T, ff, path string, frames int) {
	t.Helper()
	raw := decodeRGBA(t, ff, path)
	const w, h = 34, 28 // 33x27 padded to even
	frameBytes := w * h * 4
	if len(raw)%frameBytes != 0 {
		t.Fatalf("decoded %d bytes, not a multiple of %dx%dx4", len(raw), w, h)
	}
	if got := len(raw) / frameBytes; got != frames {
		t.Errorf("decoded %d frames, want %d", got, frames)
	}
	// The transparent right quarter of softMaster must be the matte 0x313338
	// (lossy: allow a wide tolerance), and everything opaque.
	px := func(x, y int) (r, g, b, a byte) {
		off := (y*w + x) * 4
		return raw[off], raw[off+1], raw[off+2], raw[off+3]
	}
	r, g, b, a := px(30, 5)
	if a != 255 {
		t.Errorf("pixel (30,5) alpha = %d, want 255 (opaque output)", a)
	}
	near := func(v byte, want int) bool { d := int(v) - want; return d >= -16 && d <= 16 }
	if !near(r, 0x31) || !near(g, 0x33) || !near(b, 0x38) {
		t.Errorf("transparent region flattened to (%d,%d,%d), want ≈ (49,51,56)", r, g, b)
	}
}

func TestMP4ArgsRealEncode(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 33, 27, 8, 12.5)
	out := filepath.Join(dir, "out.mp4")
	run(t, ff, enc.MP4Args(m, enc.MP4Options{}, out))

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat"))
	if moov < 0 || mdat < 0 || moov > mdat {
		t.Errorf("faststart: moov at %d, mdat at %d — moov must precede mdat", moov, mdat)
	}
	videoTailChecks(t, ff, out, 8)
}

func TestWebMArgsRealEncode(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 33, 27, 8, 12.5)
	out := filepath.Join(dir, "out.webm")
	run(t, ff, enc.WebMArgs(m, enc.WebMOptions{CRF: 40}, out))
	videoTailChecks(t, ff, out, 8)
}

// TestVideoColorTagsReal: both video tails must produce files whose colour
// metadata says bt709 (what players assume for untagged web video) and whose
// flattened matte round-trips to 0x313338 within ±2 per channel when decoded
// WITH the tagged matrix — the point of the matte flatten is matching
// Discord's dark background, and an untagged bt601-converted file would
// decode a few units off under a bt709 player.
func TestVideoColorTagsReal(t *testing.T) {
	ff := ffmpegOrSkip(t)
	fp := toolOrSkip(t, "ffprobe")
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 33, 27, 4, 12.5)
	for _, tc := range []struct {
		name string
		args func(out string) []string
	}{
		{"mp4", func(out string) []string { return enc.MP4Args(m, enc.MP4Options{}, out) }},
		{"webm", func(out string) []string { return enc.WebMArgs(m, enc.WebMOptions{}, out) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, "tags."+tc.name)
			run(t, ff, tc.args(out))
			probe := string(runTool(t, fp, []string{
				"-v", "error", "-select_streams", "v:0",
				"-show_entries", "stream=color_space,color_primaries,color_transfer",
				"-of", "default=noprint_wrappers=1", out,
			}))
			for _, want := range []string{"color_space=bt709", "color_primaries=bt709", "color_transfer=bt709"} {
				if !strings.Contains(probe, want) {
					t.Errorf("ffprobe: missing %q in:\n%s", want, probe)
				}
			}
			// Decode with the tagged matrix: the transparent right quarter of
			// softMaster (pixel (30,5) of the 34x28 padded frame) must be the
			// matte, near-exactly — the region is flat, so the codecs at the
			// default CRF preserve it.
			raw := run(t, ff, []string{
				"-i", out,
				"-vf", "scale=in_color_matrix=bt709:in_range=tv,format=rgba",
				"-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1",
			})
			const w = 34 // 33 padded to even
			off := (5*w + 30) * 4
			if len(raw) < off+4 {
				t.Fatalf("decoded only %d bytes", len(raw))
			}
			for i, want := range []int{0x31, 0x33, 0x38} {
				if d := int(raw[off+i]) - want; d < -2 || d > 2 {
					t.Errorf("matte channel %d = 0x%02x, want 0x%02x ± 2", i, raw[off+i], want)
				}
			}
		})
	}
}

// TestVideoCRFIsMonotonic: a harsher CRF never yields a larger file — the
// property the fit search's secant on log(size) relies on.
func TestVideoCRFIsMonotonic(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 64, 48, 10, 25)
	size := func(name string, args []string) int64 {
		t.Helper()
		run(t, ff, args)
		st, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}
	mild := filepath.Join(dir, "mild.mp4")
	harsh := filepath.Join(dir, "harsh.mp4")
	if sm, sh := size(mild, enc.MP4Args(m, enc.MP4Options{CRF: 12}, mild)), size(harsh, enc.MP4Args(m, enc.MP4Options{CRF: 40}, harsh)); sh > sm {
		t.Errorf("x264: crf 40 (%d B) larger than crf 12 (%d B)", sh, sm)
	}
	mildW := filepath.Join(dir, "mild.webm")
	harshW := filepath.Join(dir, "harsh.webm")
	if sm, sh := size(mildW, enc.WebMArgs(m, enc.WebMOptions{CRF: 15}, mildW)), size(harshW, enc.WebMArgs(m, enc.WebMOptions{CRF: 55}, harshW)); sh > sm {
		t.Errorf("vp9: crf 55 (%d B) larger than crf 15 (%d B)", sh, sm)
	}
}

// writePNGFrames writes n solid-colour 40x30 PNG frames and returns their
// paths (zero-padded, ordered) and colours.
func writePNGFrames(t *testing.T, dir string, n int) ([]string, []color.NRGBA) {
	t.Helper()
	paths := make([]string, 0, n)
	colors := make([]color.NRGBA, 0, n)
	for i := 0; i < n; i++ {
		c := color.NRGBA{R: byte(40 * (i + 1)), G: byte(255 - 30*i), B: byte(20 + 25*i), A: 255}
		img := image.NewNRGBA(image.Rect(0, 0, 40, 30))
		for p := 0; p < len(img.Pix); p += 4 {
			img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = c.R, c.G, c.B, c.A
		}
		path := filepath.Join(dir, "f"+strconv.Itoa(10000+i)+".png")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		f.Close()
		paths = append(paths, path)
		colors = append(colors, c)
	}
	return paths, colors
}

// TestGifskiArgsReal: gifski accepts the explicit frame list and a
// fractional --fps, and yields one GIF frame per input PNG at the right
// delay (12.5 fps → 8 cs).
func TestGifskiArgsReal(t *testing.T) {
	bin := toolOrSkip(t, "gifski")
	dir := t.TempDir()
	frames, _ := writePNGFrames(t, dir, 5)
	out := filepath.Join(dir, "out.gif")
	runTool(t, bin, enc.GifskiArgs(frames, 12.5, 90, 0, out))

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gifski output does not decode: %v", err)
	}
	if len(g.Image) != 5 {
		t.Errorf("gifski wrote %d frames, want 5", len(g.Image))
	}
	for i, d := range g.Delay {
		if d != 8 {
			t.Errorf("frame %d delay = %d cs, want 8 (12.5 fps)", i, d)
		}
	}
	if g.LoopCount != 0 {
		t.Errorf("LoopCount = %d, want 0 (loop forever)", g.LoopCount)
	}
}

// solidGIF encodes a full-frame solid-colour GIF with the given delays.
func solidGIF(t *testing.T, path string, w, h int, delays []int) []color.NRGBA {
	t.Helper()
	g := &gif.GIF{LoopCount: 0}
	colors := make([]color.NRGBA, 0, len(delays))
	for i, d := range delays {
		c := color.NRGBA{R: byte(30 * (i + 1)), G: byte(200 - 25*i), B: byte(15 + 30*i), A: 255}
		pal := color.Palette{color.NRGBA{A: 255}, c}
		img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for p := range img.Pix {
			img.Pix[p] = 1
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, d)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
		colors = append(colors, c)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gif.EncodeAll(f, g); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return colors
}

// TestGifsicleFastPathReal: crop + trim + drop-every-2 keeps source frames
// #1 and #3 with merged delays, cropped to the rect, pixels untouched, loop
// count written.
func TestGifsicleFastPathReal(t *testing.T) {
	bin := toolOrSkip(t, "gifsicle")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.gif")
	delays := []int{10, 10, 5, 5, 10, 10}
	colors := solidGIF(t, in, 40, 30, delays)

	out := filepath.Join(dir, "out.gif")
	runTool(t, bin, enc.GifsicleFastPathArgs(in, out, delays, enc.GifsicleFastPathOptions{
		Crop:       &enc.GifsicleCrop{X: 4, Y: 2, W: 20, H: 16},
		FrameStart: 1, FrameEnd: 4,
		DropEveryN: 2,
		Loop:       5,
	}))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("fast-path output does not decode: %v", err)
	}
	if g.Config.Width != 20 || g.Config.Height != 16 {
		t.Errorf("logical screen = %dx%d, want 20x16", g.Config.Width, g.Config.Height)
	}
	if len(g.Image) != 2 {
		t.Fatalf("kept %d frames, want 2 (source #1 and #3)", len(g.Image))
	}
	if g.Delay[0] != 15 || g.Delay[1] != 15 {
		t.Errorf("delays = %v, want [15 15] (dropped delays merged)", g.Delay)
	}
	if g.LoopCount != 5 {
		t.Errorf("LoopCount = %d, want 5", g.LoopCount)
	}
	// Lossless: the first kept frame is source frame 1's colour, bit-exact.
	r0, g0, b0, _ := g.Image[0].At(g.Image[0].Rect.Min.X, g.Image[0].Rect.Min.Y).RGBA()
	want := colors[1]
	if byte(r0>>8) != want.R || byte(g0>>8) != want.G || byte(b0>>8) != want.B {
		t.Errorf("frame 0 pixel = (%d,%d,%d), want (%d,%d,%d)", r0>>8, g0>>8, b0>>8, want.R, want.G, want.B)
	}
}

// TestGifsicleFastPathClampedRange: a FrameEnd past the last frame is
// clamped to it when the delay list is given (gifsicle would exit non-zero
// on the unclamped "#2-50" selection).
func TestGifsicleFastPathClampedRange(t *testing.T) {
	bin := toolOrSkip(t, "gifsicle")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.gif")
	delays := []int{10, 10, 5, 5, 10, 10}
	solidGIF(t, in, 40, 30, delays)

	out := filepath.Join(dir, "out.gif")
	runTool(t, bin, enc.GifsicleFastPathArgs(in, out, delays, enc.GifsicleFastPathOptions{FrameStart: 2, FrameEnd: 50}))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("clamped-range output does not decode: %v", err)
	}
	if len(g.Image) != 4 {
		t.Errorf("kept %d frames, want 4 (source #2-#5)", len(g.Image))
	}
}

// TestGifsicleFastPathOpenRange: gifsicle accepts the open-ended "#a-"
// selection and keeps the delays of the untouched frames.
func TestGifsicleFastPathOpenRange(t *testing.T) {
	bin := toolOrSkip(t, "gifsicle")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.gif")
	delays := []int{10, 10, 5, 5, 10, 10}
	solidGIF(t, in, 40, 30, delays)

	out := filepath.Join(dir, "out.gif")
	runTool(t, bin, enc.GifsicleFastPathArgs(in, out, delays, enc.GifsicleFastPathOptions{FrameStart: 3}))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open-range output does not decode: %v", err)
	}
	if len(g.Image) != 3 {
		t.Errorf("kept %d frames, want 3 (source #3-#5)", len(g.Image))
	}
	if len(g.Delay) == 3 && (g.Delay[0] != 5 || g.Delay[1] != 10 || g.Delay[2] != 10) {
		t.Errorf("delays = %v, want [5 10 10]", g.Delay)
	}
}
