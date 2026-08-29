package jobs

// Real-ffmpeg checks of the Phase 3 pipeline (DESIGN.md §4.3): keying,
// text and image overlays, looping animated overlays, reverse, crop to
// content, the animated proxy and the font list. Every assertion is made on
// decoded pixels or frame hashes, never on exit status alone. Skips without
// ffmpeg/ffprobe, and — while the graph/enc Phase 3 pieces are built
// concurrently — when those still answer "not implemented".

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/probe"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// requireGraphPhase3 skips while graph.CompileWithSources still refuses
// overlay sources.
func requireGraphPhase3(t *testing.T) {
	t.Helper()
	infos := []recipe.ProbeInfo{
		{Width: 8, Height: 8, IsStill: true, Frames: 1, Kind: recipe.KindImage, Codec: "png", Format: "png_pipe"},
		{Width: 4, Height: 4, IsStill: true, Frames: 1, Kind: recipe.KindImage, Codec: "png", Format: "png_pipe"},
	}
	_, err := graph.CompileWithSources(infos, []recipe.Op{{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1}`)}}, recipe.Output{Format: "gif"})
	if errors.Is(err, graph.ErrNotImplemented) {
		t.Skip("graph.CompileWithSources is not implemented yet")
	}
}

// requireGraphOp skips while the graph does not compile the given op.
func requireGraphOp(t *testing.T, op recipe.Op) {
	t.Helper()
	src := recipe.ProbeInfo{Width: 8, Height: 8, FPS: 10, Duration: 1, Frames: 10, Kind: recipe.KindVideo, Codec: "png", Format: "mov", PixFmt: "rgb24"}
	if _, err := graph.Compile(src, []recipe.Op{op}, recipe.Output{Format: "gif"}); err != nil && strings.Contains(err.Error(), "unknown op kind") {
		t.Skipf("graph does not compile %s yet: %v", op.Kind, err)
	}
}

// requireCropDetect skips while enc.CropDetectArgs is a stub.
func requireCropDetect(t *testing.T) {
	t.Helper()
	if enc.CropDetectArgs("x.mov", nil, true, 1) == nil {
		t.Skip("enc.CropDetectArgs is not implemented yet")
	}
}

// e2e bundles what every Phase 3 end-to-end test needs.
type e2e struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	tools ffrun.Tools
	m     *Manager
	dir   string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	tools := realTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	st := newTestStore(t)
	return &e2e{t: t, ctx: ctx, st: st, tools: tools, m: NewManager(st, tools, Options{Concurrency: 2}), dir: t.TempDir()}
}

// store puts data into the blob store, probes it and records the info.
func (e *e2e) store(name string, data []byte) *store.Blob {
	e.t.Helper()
	blob, err := e.st.PutBlob(bytes.NewReader(data), name)
	if err != nil {
		e.t.Fatal(err)
	}
	info, err := probe.Probe(e.ctx, e.tools, blob.Path, 0)
	if err != nil {
		e.t.Fatalf("probe %s: %v", name, err)
	}
	if err := e.st.SetBlobInfo(blob.Hash, info); err != nil {
		e.t.Fatal(err)
	}
	blob.Info = &info
	return blob
}

// lavfi renders a lavfi graph losslessly (PNG frames in a MOV, pixFmt rgba
// or rgb24) and stores it as a probed source.
func (e *e2e) lavfi(name, graphText, pixFmt string) *store.Blob {
	e.t.Helper()
	out := filepath.Join(e.dir, name)
	cmd := exec.CommandContext(e.ctx, e.tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", graphText, "-c:v", "png", "-pix_fmt", pixFmt, out)
	if o, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("build %s: %v\n%s", name, err, o)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.store(name, data)
}

// opaqueGIFFrames builds a w x h GIF whose frames are the given solid
// colours at delayCS centiseconds each, looping forever.
func opaqueGIFFrames(t *testing.T, w, h, delayCS int, colours ...color.RGBA) []byte {
	t.Helper()
	pal := color.Palette{}
	for _, c := range colours {
		pal = append(pal, c)
	}
	g := &gif.GIF{LoopCount: 0}
	for i := range colours {
		fr := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				fr.SetColorIndex(x, y, uint8(i))
			}
		}
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, delayCS)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	red   = color.RGBA{220, 30, 30, 255}
	green = color.RGBA{30, 200, 30, 255}
)

// classify buckets a pixel: "transparent", "red", "green", "blue", "white"
// or "other".
func classify(c color.Color) string {
	r, g, b, a := c.RGBA()
	switch {
	case a < 0x2000:
		return "transparent"
	case r > 0x8000 && g < 0x8000 && b < 0x8000:
		return "red"
	case g > 0x8000 && r < 0x8000 && b < 0x8000:
		return "green"
	case b > 0x8000 && r < 0x8000 && g < 0x8000:
		return "blue"
	case r > 0xC000 && g > 0xC000 && b > 0xC000:
		return "white"
	}
	return "other"
}

// decodePNG decodes PNG bytes.
func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png (%d bytes): %v", len(data), err)
	}
	return img
}

// gifCanvas composites frames 0..idx of g the way a player does (frames
// are sub-rectangles; a transparent pixel leaves the canvas alone;
// "restore to background" clears the frame's rectangle afterwards) and
// returns the canvas as shown after frame idx.
func gifCanvas(g *gif.GIF, idx int) *image.NRGBA {
	canvas := image.NewNRGBA(image.Rect(0, 0, g.Config.Width, g.Config.Height))
	for k := 0; k <= idx && k < len(g.Image); k++ {
		fr := g.Image[k]
		b := fr.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := fr.At(x, y)
				if _, _, _, a := c.RGBA(); a == 0 {
					continue
				}
				canvas.Set(x, y, c)
			}
		}
		if k < idx && k < len(g.Disposal) && g.Disposal[k] == gif.DisposalBackground {
			for y := b.Min.Y; y < b.Max.Y; y++ {
				for x := b.Min.X; x < b.Max.X; x++ {
					canvas.SetNRGBA(x, y, color.NRGBA{})
				}
			}
		}
	}
	return canvas
}

// run submits r, waits, and fails the test unless the job succeeded.
func (e *e2e) run(r recipe.Recipe) Job {
	e.t.Helper()
	fin := runJob(e.t, e.m, r)
	if fin.State != StateDone {
		e.t.Fatalf("job failed: %s (stage %s)", fin.Error, fin.Stage)
	}
	return fin
}

// resultBytes reads a delivered file of r.
func (e *e2e) resultBytes(r recipe.Recipe, name string) []byte {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.st.ResultDir(ResultKey(r)), name))
	if err != nil {
		e.t.Fatal(err)
	}
	return data
}

// frameFiles returns the PNG frames of a frames-export result, in order.
func (e *e2e) frameFiles(fin Job) []File {
	e.t.Helper()
	var frames []File
	for _, f := range fin.Result.Files {
		if f.Kind == FileKindFrame {
			frames = append(frames, f)
		}
	}
	if len(frames) == 0 {
		e.t.Fatal("no frames delivered")
	}
	return frames
}

// The green-screen clip: a red 16x12 square moving right over pure green,
// 10 frames at 10 fps, opaque (rgb24).
const greenScreen = "color=c=0x00FF00:s=64x48:r=10:d=1[bg];color=c=0xDC1E1E:s=16x12:r=10:d=1[fg];[bg][fg]overlay=x='8+t*20':y=18:format=auto"

// greenSteps is the green-screen clip for the autocrop tests: the red 16x12
// square sits at x=8 for the first half second and at x=18 for the second
// (y=18), so the 5 fps detection sampler sees both positions and the union
// box is exactly 26x12 at (8,18); composited in RGB so the key colour is
// exact.
const greenSteps = "color=c=0x00FF00:s=64x48:r=10:d=1[bg];color=c=0xDC1E1E:s=16x12:r=10:d=1[fg];[bg][fg]overlay=x='8+10*floor(2*t)':y=18:format=rgb"

// Keying ops for the autocrop tests: colorkey judges each pixel on its own
// (an exact box on hard edges); chromakey judges the 3x3 neighbourhood, so
// the screen pixels touching the subject keep a little alpha and the
// default threshold (alpha > 0) counts them — one extra pixel per side.
var (
	colorKeyGreen  = recipe.Op{Kind: recipe.OpColorKey, Params: json.RawMessage(`{"color":"00ff00","similarity":0.1}`)}
	chromaKeyGreen = recipe.Op{Kind: recipe.OpChromaKey, Params: json.RawMessage(`{"color":"00ff00"}`)}
)

// TestRenderKeying: chromakey and colorkey turn the green background
// transparent and keep the red subject; the GIF reports alpha.
func TestRenderKeying(t *testing.T) {
	e := newE2E(t)
	clip := e.lavfi("green.mov", greenScreen, "rgb24")
	if clip.Info.HasAlpha {
		t.Fatalf("the green-screen clip must be opaque: %+v", clip.Info)
	}
	for _, op := range []recipe.Op{
		{Kind: recipe.OpChromaKey, Params: json.RawMessage(`{"color":"00ff00"}`)},
		{Kind: recipe.OpColorKey, Params: json.RawMessage(`{"color":"00ff00","similarity":0.1}`)},
	} {
		t.Run(op.Kind, func(t *testing.T) {
			requireGraphOp(t, op)
			r := recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{op}, Output: recipe.Output{Format: "gif", Target: "attachment"}}
			fin := e.run(r)
			f := fin.Result.Files[0]
			if f.Report == nil || !f.Report.OK || !f.Report.HasAlpha {
				t.Fatalf("report = %+v", f.Report)
			}
			g, err := gif.DecodeAll(bytes.NewReader(e.resultBytes(r, f.Name)))
			if err != nil {
				t.Fatal(err)
			}
			if len(g.Image) < 2 || g.Config.Width != 64 || g.Config.Height != 48 {
				t.Fatalf("gif: %d frames %dx%d", len(g.Image), g.Config.Width, g.Config.Height)
			}
			// Frame 0: square at x=8..24, y=18..30; frame 5 (t=0.5): x=18..34.
			for _, tc := range []struct {
				frame, x, y int
				want        string
			}{
				{0, 2, 2, "transparent"}, {0, 12, 24, "red"}, {0, 40, 24, "transparent"},
				{5, 26, 24, "red"}, {5, 10, 24, "transparent"}, {5, 60, 40, "transparent"},
			} {
				if tc.frame >= len(g.Image) {
					continue
				}
				if got := classify(gifCanvas(g, tc.frame).At(tc.x, tc.y)); got != tc.want {
					t.Errorf("frame %d pixel (%d,%d) = %s, want %s", tc.frame, tc.x, tc.y, got, tc.want)
				}
			}
		})
	}
}

// TestRenderFeatherSoftEdge: a feather op with a colorkey blurs ONLY the
// alpha plane, so the rendered frames carry intermediate alpha in a narrow
// band around the keyed subject — and nowhere else — while the same recipe
// without feather keys a hard, binary edge (colorkey with blend 0). Frame 0
// of a frames export is checked pixel by pixel; the subject there is the
// red 16x12 square at (8,18).
func TestRenderFeatherSoftEdge(t *testing.T) {
	e := newE2E(t)
	if v := ffmpegMajor(t, e.tools); v > 0 && v < 9 {
		t.Skipf("the feather e2e needs FFmpeg 9 (have %d)", v)
	}
	feather := recipe.Op{Kind: recipe.OpFeather, Params: json.RawMessage(`{"radius":2}`)}
	requireGraphOp(t, feather)
	steps := e.lavfi("steps.mov", greenSteps, "rgb24")
	frame0 := func(ops []recipe.Op) image.Image {
		t.Helper()
		r := recipe.Recipe{Sources: []string{steps.Hash}, Ops: ops, Output: recipe.Output{Format: "frames"}}
		fin := e.run(r)
		return decodePNG(t, e.resultBytes(r, e.frameFiles(fin)[0].Name))
	}
	alpha8 := func(img image.Image, x, y int) int {
		_, _, _, a := img.At(x, y).RGBA()
		return int(a >> 8)
	}
	// scan counts the pixels with intermediate alpha (strictly between 32
	// and 224) and, of those, the ones farther than 8 px from the subject
	// square (there must be none: the feather is a local edge effect).
	scan := func(img image.Image) (mid, far int) {
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				a := alpha8(img, x, y)
				if a <= 32 || a >= 224 {
					continue
				}
				mid++
				if x > 32 || y < 10 || y > 38 {
					far++
				}
			}
		}
		return mid, far
	}

	soft := frame0([]recipe.Op{colorKeyGreen, feather})
	if b := soft.Bounds(); b.Dx() != 64 || b.Dy() != 48 {
		t.Fatalf("feathered frame is %dx%d, want 64x48", b.Dx(), b.Dy())
	}
	mid, far := scan(soft)
	if mid == 0 {
		t.Error("feathered render has no intermediate alpha at the subject edge")
	}
	if far != 0 {
		t.Errorf("%d intermediate-alpha pixels far from the subject edge", far)
	}
	// The subject's centre stays (nearly) opaque red, the far screen fully
	// transparent: the blur fades the edge, it does not wash out the frame.
	if a := alpha8(soft, 16, 24); a < 224 {
		t.Errorf("subject centre alpha = %d, want >= 224", a)
	}
	if got := classify(soft.At(16, 24)); got != "red" {
		t.Errorf("subject centre = %s, want red", got)
	}
	if a := alpha8(soft, 56, 8); a > 32 {
		t.Errorf("far screen alpha = %d, want <= 32", a)
	}

	// Without feather the key edge is hard: no intermediate alpha anywhere.
	hard := frame0([]recipe.Op{colorKeyGreen})
	if mid, _ := scan(hard); mid != 0 {
		t.Errorf("unfeathered render has %d intermediate-alpha pixels, want none", mid)
	}
}

// TestRenderTextOverlay: drawtext via a text file changes pixels inside
// the text box and nowhere else.
func TestRenderTextOverlay(t *testing.T) {
	e := newE2E(t)
	textOp := recipe.Op{Kind: recipe.OpText, Params: json.RawMessage(`{"text":"HI","size":24,"color":"ffffff","x":4,"y":4}`)}
	requireGraphOp(t, textOp)
	clip := e.lavfi("blue.mov", "color=c=blue:s=64x48:r=10:d=1", "rgb24")
	r := recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{textOp}, Output: recipe.Output{Format: "png"}}
	fin := runJob(t, e.m, r)
	if fin.State != StateDone {
		if strings.Contains(strings.ToLower(fin.Error), "font") {
			t.Skipf("drawtext has no usable font on this host: %s", fin.Error)
		}
		t.Fatalf("job failed: %s (stage %s)", fin.Error, fin.Stage)
	}
	img := decodePNG(t, e.resultBytes(r, fin.Result.Files[0].Name))
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 48 {
		t.Fatalf("png is %dx%d", b.Dx(), b.Dy())
	}
	changed := 0
	for y := 4; y < 32; y++ {
		for x := 4; x < 44; x++ {
			if classify(img.At(x, y)) != "blue" {
				changed++
			}
		}
	}
	if changed < 20 {
		t.Errorf("only %d pixels changed inside the text box", changed)
	}
	for _, p := range []image.Point{{60, 44}, {2, 46}, {62, 2}} {
		if got := classify(img.At(p.X, p.Y)); got != "blue" {
			t.Errorf("pixel %v outside the text = %s, want blue", p, got)
		}
	}
	// The text file is gone with the job's scratch dir.
	entries, _ := os.ReadDir(e.st.Scratch)
	for _, en := range entries {
		if en.IsDir() && en.Name() != stillsDir && en.Name() != proxyDir && en.Name() != autocropDir {
			t.Errorf("scratch dir %s left behind", en.Name())
		}
	}
}

// TestRenderStillImageOverlay: a second uploaded blob (a red PNG) lands at
// its anchor in the render and in the still preview.
func TestRenderStillImageOverlay(t *testing.T) {
	requireGraphPhase3(t)
	e := newE2E(t)
	base := e.lavfi("blue.mov", "color=c=blue:s=64x48:r=10:d=1", "rgb24")
	ov := e.store("red.png", solidPNG(t, 16, 12, color.NRGBA{R: 220, G: 30, B: 30, A: 255}))
	if !ov.Info.IsStill {
		t.Fatalf("overlay must probe as a still: %+v", ov.Info)
	}
	ops := []recipe.Op{{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1,"x":40,"y":30}`)}}
	srcs := []string{base.Hash, ov.Hash}
	check := func(t *testing.T, what string, img image.Image) {
		t.Helper()
		for _, tc := range []struct {
			x, y int
			want string
		}{{47, 35, "red"}, {41, 31, "red"}, {54, 40, "red"}, {10, 10, "blue"}, {38, 28, "blue"}, {58, 44, "blue"}} {
			if got := classify(img.At(tc.x, tc.y)); got != tc.want {
				t.Errorf("%s: pixel (%d,%d) = %s, want %s", what, tc.x, tc.y, got, tc.want)
			}
		}
	}
	r := recipe.Recipe{Sources: srcs, Ops: ops, Output: recipe.Output{Format: "png"}}
	fin := e.run(r)
	check(t, "png render", decodePNG(t, e.resultBytes(r, fin.Result.Files[0].Name)))

	// Animated output too (gif, frame 3), and the source blobs are touched.
	// The base blinks a corner box (outside every checked pixel) so the clip
	// is not static: gifsicle's duplicate-frame merge would otherwise fold a
	// static clip into a single frame.
	blink := e.lavfi("blink.mov", "color=c=blue:s=64x48:r=10:d=1,drawbox=x=0:y=40:w=8:h=8:c=white:t=fill:enable='lt(mod(t\\,0.4)\\,0.2)'", "rgb24")
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(ov.Path, old, old)
	rg := recipe.Recipe{Sources: []string{blink.Hash, ov.Hash}, Ops: ops, Output: recipe.Output{Format: "gif", Target: "attachment"}}
	fin = e.run(rg)
	g, err := gif.DecodeAll(bytes.NewReader(e.resultBytes(rg, fin.Result.Files[0].Name)))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Image) < 4 {
		t.Fatalf("gif has %d frames", len(g.Image))
	}
	check(t, "gif frame 3", gifCanvas(g, 3))
	if st, err := os.Stat(ov.Path); err != nil || time.Since(st.ModTime()) > time.Hour {
		t.Errorf("overlay blob was not touched: %v %v", st.ModTime(), err)
	}

	// The still preview shows the overlay as well, and is memoised.
	still, err := e.m.StillSources(e.ctx, srcs, ops, recipe.Output{Format: "gif"}, 0.5, 0)
	if err != nil {
		t.Fatalf("StillSources: %v", err)
	}
	check(t, "still", decodePNG(t, still))
	again, err := e.m.StillSources(e.ctx, srcs, ops, recipe.Output{Format: "gif", Lossy: 50}, 0.5, 0)
	if err != nil || !bytes.Equal(again, still) {
		t.Errorf("memoised still differs: %v", err)
	}
	// A text overlay through the still path writes and removes its text file.
	textOp := recipe.Op{Kind: recipe.OpText, Params: json.RawMessage(`{"text":"HI","size":24,"color":"ffffff","x":4,"y":4}`)}
	stillText, err := e.m.StillSources(e.ctx, srcs, append(ops, textOp), recipe.Output{Format: "gif"}, 0.5, 0)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "font") {
			t.Logf("drawtext has no usable font on this host: %v", err)
		} else {
			t.Fatalf("still with text: %v", err)
		}
	} else {
		img := decodePNG(t, stillText)
		changed := 0
		for y := 4; y < 32; y++ {
			for x := 4; x < 44; x++ {
				if classify(img.At(x, y)) != "blue" {
					changed++
				}
			}
		}
		if changed < 20 {
			t.Errorf("still with text: only %d pixels changed inside the text box", changed)
		}
	}
	entries, _ := os.ReadDir(e.st.Scratch)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), "still-") {
			t.Errorf("still scratch dir %s left behind", en.Name())
		}
	}
}

// TestRenderAnimatedOverlayLoops: a 2-frame GIF overlay (red, green at 10
// fps) over a 3 s clip keeps alternating until the last frame.
func TestRenderAnimatedOverlayLoops(t *testing.T) {
	requireGraphPhase3(t)
	e := newE2E(t)
	base := e.lavfi("blue3s.mov", "color=c=blue:s=64x48:r=10:d=3", "rgb24")
	ov := e.store("blink.gif", opaqueGIFFrames(t, 16, 12, 10, red, green))
	if ov.Info.Frames != 2 || ov.Info.IsStill {
		t.Fatalf("overlay probe: %+v", ov.Info)
	}
	r := recipe.Recipe{Sources: []string{base.Hash, ov.Hash},
		Ops:    []recipe.Op{{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1,"x":8,"y":8}`)}},
		Output: recipe.Output{Format: "frames"}}
	fin := e.run(r)
	frames := e.frameFiles(fin)
	if len(frames) != 30 {
		t.Fatalf("%d frames, want 30 (the base clip's length, not the overlay's)", len(frames))
	}
	var seq []string
	for _, f := range frames {
		img := decodePNG(t, e.resultBytes(r, f.Name))
		seq = append(seq, classify(img.At(12, 12)))
		if got := classify(img.At(40, 40)); got != "blue" {
			t.Errorf("%s: base pixel = %s", f.Name, got)
		}
	}
	t.Logf("overlay pixel per frame: %s", strings.Join(seq, ","))
	for i, c := range seq {
		if c != "red" && c != "green" {
			t.Errorf("frame %d: overlay pixel is %s (the overlay stopped?)", i+1, c)
		}
		if i > 0 && seq[i] == seq[i-1] {
			t.Errorf("frames %d and %d both show %s: the overlay does not alternate", i, i+1, c)
		}
	}
}

// TestRenderReverse: the frame export of a reversed clip is the forward
// export backwards (frame hashes).
func TestRenderReverse(t *testing.T) {
	e := newE2E(t)
	rev := recipe.Op{Kind: recipe.OpReverse}
	requireGraphOp(t, rev)
	clip := e.lavfi("testsrc.mov", "testsrc=size=32x24:rate=10:duration=1", "rgb24")
	hashes := func(r recipe.Recipe) []string {
		t.Helper()
		fin := e.run(r)
		var out []string
		for _, f := range e.frameFiles(fin) {
			img := decodePNG(t, e.resultBytes(r, f.Name))
			n := image.NewNRGBA(img.Bounds())
			for y := 0; y < n.Rect.Dy(); y++ {
				for x := 0; x < n.Rect.Dx(); x++ {
					n.Set(x, y, img.At(x, y))
				}
			}
			sum := sha256.Sum256(n.Pix)
			out = append(out, fmt.Sprintf("%x", sum[:6]))
		}
		return out
	}
	fwd := hashes(recipe.Recipe{Sources: []string{clip.Hash}, Output: recipe.Output{Format: "frames"}})
	back := hashes(recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{rev}, Output: recipe.Output{Format: "frames"}})
	if len(fwd) != 10 || len(back) != len(fwd) {
		t.Fatalf("%d forward frames, %d reversed", len(fwd), len(back))
	}
	if fwd[0] == fwd[9] {
		t.Fatal("testsrc frames are not distinct")
	}
	for i := range fwd {
		if back[i] != fwd[len(fwd)-1-i] {
			t.Errorf("reversed frame %d = %s, want forward frame %d = %s", i+1, back[i], len(fwd)-i, fwd[len(fwd)-1-i])
		}
	}
}

// Alpha clips for the autocrop tests: a red 24x24 square at (16,12) on a
// transparent 64x48 canvas, and a fully transparent one.
const (
	alphaSquare = "color=c=0x00000000:s=64x48:r=10:d=1,format=rgba[bg];color=c=0xDC1E1E:s=24x24:r=10:d=1,format=rgba[fg];[bg][fg]overlay=16:12:format=auto"
	alphaEmpty  = "color=c=0x00000000:s=64x48:r=10:d=1,format=rgba"
	blackBorder = "color=c=black:s=64x48:r=10:d=1[bg];color=c=0xDC1E1E:s=24x24:r=10:d=1[fg];[bg][fg]overlay=16:12:format=auto"
)

// resolvedBox returns the Resolved box of the autocrop op in ops.
func resolvedBox(t *testing.T, ops []recipe.Op) recipe.CropParams {
	t.Helper()
	for _, op := range ops {
		if op.Kind != recipe.OpAutoCrop {
			continue
		}
		var p recipe.AutoCropParams
		if err := json.Unmarshal(op.Params, &p); err != nil || p.Resolved == nil {
			t.Fatalf("autocrop not resolved: %s (%v)", op.Params, err)
		}
		return *p.Resolved
	}
	t.Fatal("no autocrop op")
	return recipe.CropParams{}
}

// alphaSquarePNG is the still counterpart of alphaSquare: a transparent
// 64x48 PNG with an opaque 24x24 square of colour c at (16,12).
func alphaSquarePNG(t *testing.T, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 12; y < 36; y++ {
		for x := 16; x < 40; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// storeSequence stores frames as an image sequence at delayMS per frame,
// probes it (probe.ProbeSequence, as the upload path does) and records the
// info.
func (e *e2e) storeSequence(name string, delayMS int, frames ...[]byte) *store.Blob {
	e.t.Helper()
	parts := make([]store.SequencePart, len(frames))
	for i, f := range frames {
		parts[i] = store.SequencePart{Name: fmt.Sprintf("%s%02d.png", name, i+1), R: bytes.NewReader(f)}
	}
	blob, err := e.st.PutSequence(parts)
	if err != nil {
		e.t.Fatal(err)
	}
	info, err := probe.ProbeSequence(e.ctx, e.tools, blob.Path, delayMS)
	if err != nil {
		e.t.Fatalf("probe sequence %s: %v", name, err)
	}
	if err := e.st.SetBlobInfo(blob.Hash, info); err != nil {
		e.t.Fatal(err)
	}
	blob.Info = &info
	return blob
}

// stillFrom stores the first frame of a lavfi graph as a PNG still.
func (e *e2e) stillFrom(name, graphText string) *store.Blob {
	e.t.Helper()
	out := filepath.Join(e.dir, name)
	cmd := exec.CommandContext(e.ctx, e.tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", graphText, "-frames:v", "1", out)
	if o, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("build still %s: %v\n%s", name, err, o)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.store(name, data)
}

// TestAutoCropDetection: the content box is found on the alpha plane (with
// padding and clamping), on the picture for opaque sources, for stills
// without any loop, for image-sequence sources (the image2 pattern joined
// exactly once), on the KEYED picture when a keying op is in the stack —
// in front of the autocrop or behind it, since the compiler hoists keying
// and the temporal ops in front of the geometry either way — (a
// green-screen clip resolves to its subject, not the full frame), and the
// full frame comes back for an empty clip; the memo makes the second call
// free.
func TestAutoCropDetection(t *testing.T) {
	requireCropDetect(t)
	e := newE2E(t)
	square := e.lavfi("square.mov", alphaSquare, "rgba")
	if !square.Info.HasAlpha {
		t.Fatalf("square clip must have alpha: %+v", square.Info)
	}
	empty := e.lavfi("empty.mov", alphaEmpty, "rgba")
	border := e.lavfi("border.mov", blackBorder, "rgb24")
	green := e.lavfi("green.mov", greenScreen, "rgb24")
	steps := e.lavfi("steps.mov", greenSteps, "rgb24")
	if steps.Info.HasAlpha || steps.Info.Width != 64 || steps.Info.Height != 48 {
		t.Fatalf("the green-screen clip must be opaque 64x48: %+v", steps.Info)
	}
	still := e.stillFrom("still.png", alphaSquare)
	if !still.Info.IsStill || !still.Info.HasAlpha {
		t.Fatalf("still probe: %+v", still.Info)
	}
	greenStill := e.stillFrom("greenstill.png", greenSteps)
	if !greenStill.Info.IsStill || greenStill.Info.HasAlpha {
		t.Fatalf("green still probe: %+v", greenStill.Info)
	}
	// A 3-frame alpha PNG sequence of the same square (100 ms per frame).
	redSq := alphaSquarePNG(t, color.NRGBA{R: 220, G: 30, B: 30, A: 255})
	blueSq := alphaSquarePNG(t, color.NRGBA{R: 30, G: 30, B: 220, A: 255})
	seq := e.storeSequence("sq", 100, redSq, blueSq, redSq)
	if !seq.IsSequence() || seq.Info.Kind != recipe.KindSequence || !seq.Info.HasAlpha || seq.Info.Width != 64 || seq.Info.Height != 48 || seq.Info.Sequence == nil {
		t.Fatalf("sequence probe: %+v", seq.Info)
	}

	pad2 := autocropOp(`{"padding":2}`)
	full := recipe.CropParams{X: 0, Y: 0, W: 64, H: 48}
	trim05 := recipe.Op{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.5}`)}
	for _, tc := range []struct {
		name string
		src  *store.Blob
		ops  []recipe.Op
		want recipe.CropParams
	}{
		{"alpha clip, padding 2", square, []recipe.Op{pad2}, recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}},
		{"alpha clip, no padding, after unpremultiply", square, []recipe.Op{{Kind: recipe.OpUnpremultiply}, autocropOp(`{}`)}, recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}},
		{"alpha clip, trimmed", square, []recipe.Op{trim05, pad2}, recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}},
		{"alpha clip, huge padding clamps", square, []recipe.Op{autocropOp(`{"padding":100}`)}, full},
		{"empty clip is the full frame", empty, []recipe.Op{pad2}, full},
		{"opaque clip with black borders", border, []recipe.Op{autocropOp(`{}`)}, recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}},
		{"opaque clip without black borders", green, []recipe.Op{autocropOp(`{}`)}, full},
		{"still image with alpha", still, []recipe.Op{pad2}, recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}},
		// An image sequence: the detection opens "<blobDir>/<pattern>" like
		// every other builder (joining the pattern twice used to fail every
		// sequence autocrop); the image2 InputArgs are exercised with a
		// delay and a trim too.
		{"image sequence with alpha, padding 2", seq, []recipe.Op{pad2}, recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}},
		{"image sequence, delayed and trimmed", seq, []recipe.Op{{Kind: recipe.OpDelay, Params: json.RawMessage(`{"ms":40}`)}, {Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.04}`)}, autocropOp(`{}`)}, recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}},
		// Keying in the stack: the detection sees the keyed picture. Without
		// it the opaque green clip is all content.
		{"green-screen clip without keying: the full frame", steps, []recipe.Op{autocropOp(`{}`)}, full},
		{"green-screen clip, colorkey then autocrop: the subject's box", steps, []recipe.Op{colorKeyGreen, autocropOp(`{}`)}, recipe.CropParams{X: 8, Y: 18, W: 26, H: 12}},
		{"green-screen clip, colorkey, padding 2", steps, []recipe.Op{colorKeyGreen, pad2}, recipe.CropParams{X: 6, Y: 16, W: 30, H: 16}},
		{"green-screen clip, chromakey then autocrop: the subject plus its soft edge", steps, []recipe.Op{chromaKeyGreen, autocropOp(`{}`)}, recipe.CropParams{X: 7, Y: 17, W: 28, H: 14}},
		{"green-screen clip, chromakey, hard threshold: the subject's box", steps, []recipe.Op{chromaKeyGreen, autocropOp(`{"threshold":255}`)}, recipe.CropParams{X: 8, Y: 18, W: 26, H: 12}},
		{"green-screen clip, trimmed and keyed: the second position only", steps, []recipe.Op{trim05, colorKeyGreen, autocropOp(`{}`)}, recipe.CropParams{X: 18, Y: 18, W: 16, H: 12}},
		{"green-screen clip, keyed after unpremultiply and speed", steps, []recipe.Op{{Kind: recipe.OpUnpremultiply}, {Kind: recipe.OpSpeed, Params: json.RawMessage(`{"factor":2}`)}, colorKeyGreen, autocropOp(`{}`)}, recipe.CropParams{X: 8, Y: 18, W: 26, H: 12}},
		{"green-screen still, keyed: no loop needed", greenStill, []recipe.Op{colorKeyGreen, autocropOp(`{}`)}, recipe.CropParams{X: 8, Y: 18, W: 16, H: 12}},
		{"green-screen still without keying: the full frame", greenStill, []recipe.Op{autocropOp(`{}`)}, full},
		// Keying / trim BEHIND the autocrop: the compiler hoists them in
		// front of the geometry, so the render is keyed / trimmed and the
		// detection must read the same picture (not the raw full frame).
		{"green-screen clip, colorkey after autocrop: the subject's box", steps, []recipe.Op{autocropOp(`{}`), colorKeyGreen}, recipe.CropParams{X: 8, Y: 18, W: 26, H: 12}},
		{"green-screen clip, trim after autocrop: the second position only", steps, []recipe.Op{colorKeyGreen, autocropOp(`{}`), trim05}, recipe.CropParams{X: 18, Y: 18, W: 16, H: 12}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.m.ResolveAutoCrop(e.ctx, tc.src.Hash, tc.ops)
			if err != nil {
				t.Fatalf("ResolveAutoCrop: %v", err)
			}
			if len(got) != len(tc.ops) {
				t.Fatalf("%d ops back, want %d", len(got), len(tc.ops))
			}
			if box := resolvedBox(t, got); box != tc.want {
				t.Errorf("resolved = %+v, want %+v", box, tc.want)
			}
		})
	}

	// Feather in the stack: the alpha blur fades the keyed edge outwards, so
	// at threshold 1 the detected box covers the keyed box and grows past it
	// by roughly the feather radius. [autocrop, feather] and [feather,
	// autocrop] are one detection (the compiler hoists feather with the
	// keying), so the second order resolves from the first one's memo — no
	// ffmpeg — exactly like the keying-behind-autocrop cases above.
	t.Run("green-screen clip, keyed and feathered: the box grows", func(t *testing.T) {
		feather := recipe.Op{Kind: recipe.OpFeather, Params: json.RawMessage(`{"radius":2}`)}
		requireGraphOp(t, feather)
		base := recipe.CropParams{X: 8, Y: 18, W: 26, H: 12} // the unfeathered keyed box (asserted above)
		got, err := e.m.ResolveAutoCrop(e.ctx, steps.Hash, []recipe.Op{colorKeyGreen, feather, autocropOp(`{}`)})
		if err != nil {
			t.Fatalf("ResolveAutoCrop: %v", err)
		}
		box := resolvedBox(t, got)
		if box.X > base.X || box.Y > base.Y || box.X+box.W < base.X+base.W || box.Y+box.H < base.Y+base.H {
			t.Errorf("feathered box %+v does not cover the keyed box %+v on every side", box, base)
		}
		if box.W*box.H <= base.W*base.H {
			t.Errorf("feathered box %+v is not larger than the keyed box %+v", box, base)
		}
		noFF := NewManager(e.st, ffrun.Tools{}, Options{})
		behind, err := noFF.ResolveAutoCrop(e.ctx, steps.Hash, []recipe.Op{colorKeyGreen, autocropOp(`{}`), feather})
		if err != nil {
			t.Fatalf("feather behind the autocrop must hit the same memo: %v", err)
		}
		if b := resolvedBox(t, behind); b != box {
			t.Errorf("feather behind the autocrop resolved %+v, want %+v", b, box)
		}
		// The unfeathered stack keeps its own memo entry: same box as before.
		plain, err := noFF.ResolveAutoCrop(e.ctx, steps.Hash, []recipe.Op{colorKeyGreen, autocropOp(`{}`)})
		if err != nil {
			t.Fatalf("unfeathered memo hit without ffmpeg: %v", err)
		}
		if b := resolvedBox(t, plain); b != base {
			t.Errorf("unfeathered box changed to %+v, want %+v", b, base)
		}
	})

	// Memo: the entries exist, and a manager without any ffmpeg resolves
	// the same ops from them.
	entries, err := os.ReadDir(filepath.Join(e.st.Scratch, autocropDir))
	if err != nil || len(entries) < 8 {
		t.Fatalf("autocrop memo dir: %d entries (%v)", len(entries), err)
	}
	noFF := NewManager(e.st, ffrun.Tools{}, Options{})
	got, err := noFF.ResolveAutoCrop(e.ctx, square.Hash, []recipe.Op{pad2})
	if err != nil {
		t.Fatalf("memo hit without ffmpeg: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}) {
		t.Errorf("memoised box = %+v", box)
	}
	got, err = noFF.ResolveAutoCrop(e.ctx, steps.Hash, []recipe.Op{colorKeyGreen, autocropOp(`{}`)})
	if err != nil {
		t.Fatalf("keyed memo hit without ffmpeg: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 8, Y: 18, W: 26, H: 12}) {
		t.Errorf("memoised keyed box = %+v", box)
	}
	// The keying op behind the autocrop is the same detection (same memo).
	got, err = noFF.ResolveAutoCrop(e.ctx, steps.Hash, []recipe.Op{autocropOp(`{}`), colorKeyGreen})
	if err != nil {
		t.Fatalf("keyed-behind memo hit without ffmpeg: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 8, Y: 18, W: 26, H: 12}) {
		t.Errorf("memoised keyed-behind box = %+v", box)
	}
	got, err = noFF.ResolveAutoCrop(e.ctx, seq.Hash, []recipe.Op{pad2})
	if err != nil {
		t.Fatalf("sequence memo hit without ffmpeg: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}) {
		t.Errorf("memoised sequence box = %+v", box)
	}
	// A different padding is the same detection (the memo holds the raw box
	// and the padding is arithmetic on read): served without ffmpeg, padded
	// as asked. A different key colour is a different detection: no memo,
	// so no ffmpeg.
	got, err = noFF.ResolveAutoCrop(e.ctx, square.Hash, []recipe.Op{autocropOp(`{"padding":3}`)})
	if err != nil {
		t.Fatalf("a new padding must be served from the memo: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 13, Y: 9, W: 30, H: 30}) {
		t.Errorf("memoised box at padding 3 = %+v", box)
	}
	rawKey, _ := autocropKey(square.Hash, nil, 1)
	if raw, ok := readAutocropMemo(filepath.Join(e.st.Scratch, autocropDir, rawKey+".json"), square.Info); !ok || raw != (recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}) {
		t.Errorf("memo entry = %+v (%v), want the raw unpadded box", raw, ok)
	}
	blueKey := recipe.Op{Kind: recipe.OpColorKey, Params: json.RawMessage(`{"color":"0000ff","similarity":0.1}`)}
	if _, err := noFF.ResolveAutoCrop(e.ctx, steps.Hash, []recipe.Op{blueKey, autocropOp(`{}`)}); err == nil {
		t.Error("a different key colour was served from the memo")
	}
}

// parseLastCrop extracts w, h, x, y from the last "crop=w:h:x:y" line a
// detector logged — "[Parsed_bbox_" / "[Parsed_cropdetect_" lines only, as
// enc.ParseCropDetect reads them; the indented metadata echoes are skipped
// — in a cropdetect stderr (a local stand-in for enc.ParseCropDetect).
func parseLastCrop(stderr string) (w, h, x, y int, ok bool) {
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.HasPrefix(line, "[Parsed_bbox_") && !strings.HasPrefix(line, "[Parsed_cropdetect_") {
			continue
		}
		if i := strings.LastIndex(line, "crop="); i >= 0 {
			if n, err := fmt.Sscanf(strings.TrimSpace(line[i+len("crop="):]), "%d:%d:%d:%d", &w, &h, &x, &y); err == nil && n == 4 {
				ok = true
			}
		}
	}
	return w, h, x, y, ok
}

// TestCropDetectPlumbing verifies, against real ffmpeg, the parts of the
// detection run that jobs owns: the -loglevel info prefix that lets
// cropdetect's lines reach the captured stderr through a filter_complex
// with a mapped null output (the CropDetectPlanArgs shape), the negative
// box cropdetect prints for an empty clip, and the single-frame facts the
// plan path relies on — a still image through a plan without an fps stage
// reports its box from the sampler's first frame without any loop wrapper,
// whereas an fps stage in front of the detector would emit nothing for it
// (which is why graph.CompileDetect, like Compile, skips fps for stills).
// The chains are built by hand so the check does not depend on enc.
func TestCropDetectPlumbing(t *testing.T) {
	e := newE2E(t)
	square := e.lavfi("square.mov", alphaSquare, "rgba")
	empty := e.lavfi("empty.mov", alphaEmpty, "rgba")
	stillPath := filepath.Join(e.dir, "still.png")
	cmd := exec.CommandContext(e.ctx, e.tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", alphaSquare, "-frames:v", "1", stillPath)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build still: %v\n%s", err, o)
	}
	const (
		sampler  = "select='isnan(prev_selected_t)+gte(round((t-prev_selected_t)*1000),200)'"
		detector = ",alphaextract,cropdetect=limit=1:round=2:reset=0:skip=0[det]"
	)
	detect := func(src, head string, inputArgs ...string) recipe.CropParams {
		t.Helper()
		args := append(append(append([]string{}, cropDetectPrefix...), inputArgs...),
			"-i", src, "-filter_complex", "[0:v]format=rgba[out];[out]"+head+detector, "-map", "[det]", "-an", "-f", "null", "-")
		_, stderr, err := ffrun.RunCapture(e.ctx, e.tools.FFmpeg, args)
		if err != nil {
			t.Fatalf("cropdetect run: %v", err)
		}
		w, h, x, y, ok := parseLastCrop(string(stderr))
		t.Logf("%s %s %v: crop=%d:%d:%d:%d ok=%v", filepath.Base(src), head, inputArgs, w, h, x, y, ok)
		return contentBox(w, h, x, y, ok, 2, 64, 48)
	}
	want := recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}
	full := recipe.CropParams{W: 64, H: 48}
	if got := detect(square.Path, sampler); got != want {
		t.Errorf("alpha clip: %+v, want %+v", got, want)
	}
	if got := detect(square.Path, sampler, "-ss", "0.5"); got != want {
		t.Errorf("trimmed alpha clip: %+v, want %+v", got, want)
	}
	if got := detect(empty.Path, sampler); got != full {
		t.Errorf("empty clip: %+v, want the full frame", got)
	}
	if got := detect(stillPath, sampler); got != want {
		t.Errorf("still through the sampler, no loop: %+v, want %+v", got, want)
	}
	if got := detect(stillPath, "fps=5"); got != full {
		t.Errorf("still behind an fps stage: %+v, want the full frame (no box printed)", got)
	}
}

// TestRenderAutoCropKeyed: Background removal + Crop to content crops the
// render (and the still) to the keyed subject plus the padding; the same
// clip without keying keeps its full frame.
func TestRenderAutoCropKeyed(t *testing.T) {
	requireCropDetect(t)
	e := newE2E(t)
	steps := e.lavfi("steps.mov", greenSteps, "rgb24")
	if steps.Info.HasAlpha {
		t.Fatalf("the green-screen clip must be opaque: %+v", steps.Info)
	}
	pad2 := autocropOp(`{"padding":2}`)
	for _, tc := range []struct {
		name    string
		ops     []recipe.Op
		w, h    int
		subject image.Point // a subject (red) pixel in output coordinates
		keyed   bool        // the corner pixel is keyed screen (transparent), else green
	}{
		// 26x12 at (8,18) + 2 px: 30x16 at (6,16); the subject fills the
		// middle, the padding ring is keyed screen.
		{"colorkey + autocrop", []recipe.Op{colorKeyGreen, pad2}, 30, 16, image.Pt(15, 8), true},
		// chromakey's soft ring adds a pixel per side.
		{"chromakey + autocrop", []recipe.Op{chromaKeyGreen, pad2}, 32, 18, image.Pt(16, 9), true},
		{"autocrop alone: the full frame", []recipe.Op{pad2}, 64, 48, image.Pt(12, 24), false},
		// The keying op behind the autocrop: the compiler keys before the
		// crop either way, so the render is the same keyed 30x16 — not the
		// keyed full frame a detection on the raw picture would leave.
		{"autocrop + colorkey (keying behind the autocrop)", []recipe.Op{pad2, colorKeyGreen}, 30, 16, image.Pt(15, 8), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := recipe.Recipe{Sources: []string{steps.Hash}, Ops: tc.ops, Output: recipe.Output{Format: "gif", Target: "attachment"}}
			fin := e.run(r)
			f := fin.Result.Files[0]
			if f.Width != tc.w || f.Height != tc.h {
				t.Errorf("rendered %dx%d, want %dx%d", f.Width, f.Height, tc.w, tc.h)
			}
			g, err := gif.DecodeAll(bytes.NewReader(e.resultBytes(r, f.Name)))
			if err != nil {
				t.Fatal(err)
			}
			if g.Config.Width != tc.w || g.Config.Height != tc.h {
				t.Errorf("gif canvas %dx%d, want %dx%d", g.Config.Width, g.Config.Height, tc.w, tc.h)
			}
			c := gifCanvas(g, 0)
			if got := classify(c.At(tc.subject.X, tc.subject.Y)); got != "red" {
				t.Errorf("subject pixel %v = %s, want red", tc.subject, got)
			}
			corner, want := classify(c.At(0, 0)), "green"
			if tc.keyed {
				want = "transparent"
			}
			if corner != want {
				t.Errorf("corner pixel = %s, want %s", corner, want)
			}
			still, err := e.m.Still(e.ctx, steps.Hash, tc.ops, recipe.Output{Format: "gif"}, 0.25, 0)
			if err != nil {
				t.Fatalf("Still: %v", err)
			}
			if b := decodePNG(t, still).Bounds(); b.Dx() != tc.w || b.Dy() != tc.h {
				t.Errorf("still is %dx%d, want %dx%d", b.Dx(), b.Dy(), tc.w, tc.h)
			}
		})
	}
}

// TestRenderAutoCrop: the resolved crop is what the render, the still and
// the proxy produce (content box + padding).
func TestRenderAutoCrop(t *testing.T) {
	requireCropDetect(t)
	e := newE2E(t)
	pad2 := autocropOp(`{"padding":2}`)
	src := recipe.ProbeInfo{Width: 64, Height: 48, FPS: 10, Duration: 1, Frames: 10, HasAlpha: true, Kind: recipe.KindVideo, Codec: "png", Format: "mov", PixFmt: "rgba"}
	resolved := recipe.Op{Kind: recipe.OpAutoCrop, Params: json.RawMessage(`{"padding":2,"resolved":{"x":14,"y":10,"w":28,"h":28}}`)}
	if _, err := graph.Compile(src, []recipe.Op{resolved}, recipe.Output{Format: "gif"}); err != nil && strings.Contains(err.Error(), "unknown op kind") {
		t.Skipf("graph does not compile autocrop yet: %v", err)
	}
	square := e.lavfi("square.mov", alphaSquare, "rgba")
	r := recipe.Recipe{Sources: []string{square.Hash}, Ops: []recipe.Op{pad2}, Output: recipe.Output{Format: "gif", Target: "attachment"}}
	fin := e.run(r)
	f := fin.Result.Files[0]
	if f.Width != 28 || f.Height != 28 {
		t.Errorf("rendered %dx%d, want 28x28", f.Width, f.Height)
	}
	g, err := gif.DecodeAll(bytes.NewReader(e.resultBytes(r, f.Name)))
	if err != nil {
		t.Fatal(err)
	}
	c := gifCanvas(g, 0)
	if got := classify(c.At(14, 14)); got != "red" {
		t.Errorf("centre pixel = %s, want red", got)
	}
	if got := classify(c.At(0, 0)); got != "transparent" {
		t.Errorf("padding pixel = %s, want transparent", got)
	}
	// The manifest's recipe keeps the client's op (no resolved box): a
	// resubmit with a client-supplied box hits the same result.
	if bytes.Contains(fin.Result.Recipe.Ops[0].Params, []byte("resolved")) {
		t.Errorf("manifest recipe carries the resolved box: %s", fin.Result.Recipe.Ops[0].Params)
	}
	j2, err := e.m.Submit(recipe.Recipe{Sources: []string{square.Hash}, Ops: []recipe.Op{resolved}, Output: r.Output})
	if err != nil || j2.State != StateDone || !j2.Result.Cached {
		t.Errorf("resubmit with a client box: %+v %v", j2, err)
	}

	still, err := e.m.Still(e.ctx, square.Hash, []recipe.Op{pad2}, recipe.Output{Format: "gif"}, 0.5, 0)
	if err != nil {
		t.Fatalf("Still: %v", err)
	}
	if b := decodePNG(t, still).Bounds(); b.Dx() != 28 || b.Dy() != 28 {
		t.Errorf("still is %dx%d, want 28x28", b.Dx(), b.Dy())
	}
	proxy, err := e.m.Proxy(e.ctx, []string{square.Hash}, []recipe.Op{pad2}, recipe.Output{Format: "webp"}, 0, 0)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	rep, err := discordlint.LintWebP(proxy, "")
	if err != nil || rep.Width != 28 || rep.Height != 28 {
		t.Errorf("proxy = %dx%d (%v)", rep.Width, rep.Height, err)
	}
}

// TestProxy: the Play preview is an animated, alpha-carrying WebP, bounded
// in width and length, memoised, and its scratch dir is removed.
func TestProxy(t *testing.T) {
	e := newE2E(t)
	if v := ffmpegMajor(t, e.tools); v > 0 && v < 9 {
		t.Skipf("animated WebP needs FFmpeg 9 (have %d)", v)
	}
	src := e.store("square.gif", animatedGIF(t)) // 40x30, 12 frames at 12.5 fps, transparent
	out := recipe.Output{Format: "webp", Target: "emote", Quality: 30}
	data, err := e.m.Proxy(e.ctx, []string{src.Hash}, nil, out, 0, 0)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		t.Fatalf("not a WebP: %q", data[:min(len(data), 12)])
	}
	rep, err := discordlint.LintWebP(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Frames < 2 || !rep.HasAlpha || rep.Width != 40 || rep.Height != 30 {
		t.Errorf("proxy report: frames %d alpha %v %dx%d", rep.Frames, rep.HasAlpha, rep.Width, rep.Height)
	}
	if !rep.LoopForever {
		t.Error("proxy does not loop")
	}
	// Memoised: identical bytes, one entry, quality knobs ignored.
	again, err := e.m.Proxy(e.ctx, []string{src.Hash}, nil, recipe.Output{Format: "webp"}, 0, 0)
	if err != nil || !bytes.Equal(again, data) {
		t.Errorf("memoised proxy differs: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(e.st.Scratch, proxyDir))
	if len(entries) != 1 {
		t.Errorf("proxy memo has %d entries, want 1", len(entries))
	}
	// Width and length bounds.
	small, err := e.m.Proxy(e.ctx, []string{src.Hash}, nil, out, 20, 0.3)
	if err != nil {
		t.Fatalf("small proxy: %v", err)
	}
	srep, err := discordlint.LintWebP(small, "")
	if err != nil {
		t.Fatal(err)
	}
	if srep.Width != 20 || srep.Frames >= rep.Frames || srep.Frames < 2 {
		t.Errorf("bounded proxy: %dx%d, %d frames (full has %d)", srep.Width, srep.Height, srep.Frames, rep.Frames)
	}
	// Cancellation is honoured and leaves no scratch dir behind.
	cancelled, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, err := e.m.Proxy(cancelled, []string{src.Hash}, nil, out, 100, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled proxy: %v", err)
	}
	scratch, _ := os.ReadDir(e.st.Scratch)
	for _, en := range scratch {
		if strings.HasPrefix(en.Name(), "proxy-") {
			t.Errorf("proxy scratch dir %s left behind", en.Name())
		}
	}
}

// TestFontsFromFcList: in the tools image fc-list lists the DejaVu faces.
func TestFontsFromFcList(t *testing.T) {
	e := newE2E(t)
	if e.tools.FcList == "" {
		t.Skip("fc-list not on PATH")
	}
	fonts := e.m.Fonts(e.ctx)
	if len(fonts) == 0 {
		if enc.FcListArgs() == nil {
			t.Skip("enc.FcListArgs is not implemented yet")
		}
		t.Fatal("no fonts listed")
	}
	found := false
	for _, f := range fonts {
		if f.Family == "" || f.File == "" {
			t.Errorf("font without family/file: %+v", f)
		}
		if f.Family == "DejaVu Sans" {
			found = true
		}
	}
	if !found {
		t.Errorf("DejaVu Sans missing from %d fonts", len(fonts))
	}
	again := e.m.Fonts(e.ctx)
	if len(again) != len(fonts) {
		t.Errorf("cached font list differs: %d vs %d", len(again), len(fonts))
	}
}

// TestStillAnimatedOverlayAtTime: the still of a multi-source recipe at
// t > 0 shows the overlay frame of that time (a 1 fps red/green GIF over a
// 3 s clip: red in [0,1), green in [1,2), red again in [2,3) as it loops).
func TestStillAnimatedOverlayAtTime(t *testing.T) {
	requireGraphPhase3(t)
	e := newE2E(t)
	base := e.lavfi("blue3s.mov", "color=c=blue:s=64x48:r=10:d=3", "rgb24")
	ov := e.store("slow.gif", opaqueGIFFrames(t, 16, 12, 100, red, green))
	srcs := []string{base.Hash, ov.Hash}
	ops := []recipe.Op{{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1,"x":8,"y":8}`)}}
	for _, tc := range []struct {
		t    float64
		want string
	}{{0.5, "red"}, {1.5, "green"}, {2.5, "red"}} {
		still, err := e.m.StillSources(e.ctx, srcs, ops, recipe.Output{Format: "gif"}, tc.t, 0)
		if err != nil {
			t.Fatalf("StillSources t=%v: %v", tc.t, err)
		}
		img := decodePNG(t, still)
		if got := classify(img.At(12, 12)); got != tc.want {
			t.Errorf("t=%v: overlay pixel = %s, want %s", tc.t, got, tc.want)
		}
		if got := classify(img.At(40, 40)); got != "blue" {
			t.Errorf("t=%v: base pixel = %s", tc.t, got)
		}
	}
}

// TestProxyStillWithOverlay: the Play preview of a still main source with
// an animated overlay at an output fps above the proxy cap renders as an
// animation (enc.ProxyArgs drops the fps=15 stage for single-frame plans —
// ffmpeg's fps filter emits nothing for one frame and libwebp_anim then
// fails to assemble), and so does a still overlay (the -loop 1 path).
func TestProxyStillWithOverlay(t *testing.T) {
	requireGraphPhase3(t)
	e := newE2E(t)
	if v := ffmpegMajor(t, e.tools); v > 0 && v < 9 {
		t.Skipf("animated WebP needs FFmpeg 9 (have %d)", v)
	}
	base := e.store("still.png", solidPNG(t, 64, 48, color.NRGBA{R: 30, G: 30, B: 220, A: 255}))
	if !base.Info.IsStill {
		t.Fatalf("main source must probe as a still: %+v", base.Info)
	}
	colours := make([]color.RGBA, 0, 10)
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			colours = append(colours, red)
		} else {
			colours = append(colours, green)
		}
	}
	ov := e.store("blink.gif", opaqueGIFFrames(t, 16, 12, 10, colours...))
	if ov.Info.IsStill || ov.Info.Frames != 10 {
		t.Fatalf("overlay probe: %+v", ov.Info)
	}
	stillOv := e.store("red.png", solidPNG(t, 16, 12, color.NRGBA{R: 220, G: 30, B: 30, A: 255}))
	ops := []recipe.Op{{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1,"x":8,"y":8}`)}}
	out := recipe.Output{Format: "webp", Target: "emote", FPS: 25}
	for _, tc := range []struct {
		name    string
		overlay *store.Blob
	}{{"animated overlay", ov}, {"still overlay", stillOv}} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := e.m.Proxy(e.ctx, []string{base.Hash, tc.overlay.Hash}, ops, out, 0, 0)
			if err != nil {
				t.Fatalf("Proxy: %v", err)
			}
			rep, err := discordlint.LintWebP(data, "")
			if err != nil {
				t.Fatalf("lint: %v", err)
			}
			if rep.Width != 64 || rep.Height != 48 || rep.Frames < 1 {
				t.Errorf("proxy report: %dx%d, %d frames", rep.Width, rep.Height, rep.Frames)
			}
		})
	}
}
