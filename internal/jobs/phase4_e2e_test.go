package jobs

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Phase 4 end-to-end tests. MP4/WebM and the bounced previews need only
// ffmpeg (they run on the host); the gifsicle fast path and gifski need
// their tools and effectively run in the ezlg-dev image.

// videoManager wires a manager with the real tools.
func videoManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	return NewManager(st, realTools(t), Options{Concurrency: 1}), st
}

// primaryFile returns the FileKindOutput entry of a done job.
func primaryFile(t *testing.T, job Job) File {
	t.Helper()
	if job.State != StateDone {
		t.Fatalf("job state %s: %s", job.State, job.Error)
	}
	for _, f := range job.Result.Files {
		if f.Kind == FileKindOutput {
			return f
		}
	}
	t.Fatal("no primary output in the manifest")
	return File{}
}

// resultBytes reads a delivered result file.
func resultBytes(t *testing.T, st *store.Store, job Job, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(st.ResultDir(job.RecipeHash), name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRenderMP4E2E: a GIF source rendered as an opaque MP4 attachment — the
// container is a faststart MP4 (moov before mdat), odd crop dimensions are
// padded to even (never refused), the report passes, and the manifest
// carries video facts.
func TestRenderMP4E2E(t *testing.T) {
	m, st := videoManager(t)
	src := putGIFSource(t, st, opaqueGIF(t))

	r := recipe.Recipe{
		Sources: []string{src},
		Ops:     []recipe.Op{op("crop", `{"x":0,"y":0,"w":33,"h":27}`)}, // odd on purpose
		Output:  recipe.Output{Format: "mp4", Target: "attachment"},
	}
	job := runJob(t, m, r)
	f := primaryFile(t, job)
	if f.Name != "out.mp4" || f.Format != "mp4" {
		t.Fatalf("primary = %+v", f)
	}
	if f.Width%2 != 0 || f.Height%2 != 0 || f.Width < 33 || f.Height < 27 {
		t.Errorf("odd dims must be padded to even: %dx%d", f.Width, f.Height)
	}
	if f.Report == nil {
		t.Fatal("mp4 primary has no report")
	}
	if !f.Report.OK {
		t.Errorf("report not OK: %+v", f.Report.Checks)
	}
	if f.Report.HasAlpha {
		t.Error("a flattened mp4 must not report alpha")
	}
	data := resultBytes(t, st, job, f.Name)
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		t.Fatalf("not an mp4: % x", data[:min(12, len(data))])
	}
	moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat"))
	if moov < 0 || mdat < 0 || moov > mdat {
		t.Errorf("faststart: moov at %d, mdat at %d — moov must come first", moov, mdat)
	}
}

// TestRenderWebME2E: same pipeline through the VP9 tail.
func TestRenderWebME2E(t *testing.T) {
	m, st := videoManager(t)
	src := putGIFSource(t, st, opaqueGIF(t))

	r := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "webm", Target: "attachment"}}
	job := runJob(t, m, r)
	f := primaryFile(t, job)
	if f.Name != "out.webm" || f.Format != "webm" {
		t.Fatalf("primary = %+v", f)
	}
	if f.Report == nil || !f.Report.OK {
		t.Fatalf("report = %+v", f.Report)
	}
	data := resultBytes(t, st, job, f.Name)
	if len(data) < 4 || data[0] != 0x1A || data[1] != 0x45 || data[2] != 0xDF || data[3] != 0xA3 {
		t.Fatalf("not an EBML/WebM file: % x", data[:min(8, len(data))])
	}
}

// TestVideoFitE2E: the mp4 fit search runs the CRF knob and describes the
// winner in CRF terms.
func TestVideoFitE2E(t *testing.T) {
	m, _ := videoManager(t)
	st := m.st
	src := putGIFSource(t, st, opaqueGIF(t))

	r := recipe.Recipe{Sources: []string{src},
		Output: recipe.Output{Format: "mp4", FitBytes: 256 << 10, FitKeepSize: true, FitKeepFPS: true}}
	job := runJob(t, m, r)
	f := primaryFile(t, job)
	if f.Report == nil || !f.Report.OK {
		t.Fatalf("report = %+v", f.Report)
	}
	if f.Bytes > 256<<10 {
		t.Errorf("fit result %d bytes over the 256 KiB budget", f.Bytes)
	}
	if !strings.Contains(f.Desc, "crf ") {
		t.Errorf("fit desc %q must name the crf knob", f.Desc)
	}
}

// bounceGIF builds a 4-frame 32x24 GIF of solid frames red, green, blue,
// white at 10 fps.
func bounceGIF(t *testing.T) ([]byte, recipe.ProbeInfo) {
	t.Helper()
	colors := []color.RGBA{
		{220, 30, 30, 255}, {30, 200, 30, 255}, {30, 30, 220, 255}, {240, 240, 240, 255},
	}
	pal := make(color.Palette, len(colors))
	for i, c := range colors {
		pal[i] = c
	}
	g := &gif.GIF{LoopCount: 0}
	for i := range colors {
		fr := image.NewPaletted(image.Rect(0, 0, 32, 24), pal)
		draw.Draw(fr, fr.Bounds(), &image.Uniform{colors[i]}, image.Point{}, draw.Src)
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 10) // 10 fps
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "gif", Codec: "gif", PixFmt: "bgra", Width: 32, Height: 24,
		FPS: 10, Duration: 0.4, Frames: 4, Kind: recipe.KindAnimation}
	return buf.Bytes(), info
}

// stillClass decodes a still PNG and classifies its center pixel as one of
// bounceGIF's four frame colours.
func stillClass(t *testing.T, data []byte) string {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("still is not a PNG: %v", err)
	}
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	hi := func(v uint32) bool { return v > 0x8000 }
	switch {
	case hi(r) && hi(g) && hi(bl):
		return "white"
	case hi(r):
		return "red"
	case hi(g):
		return "green"
	case hi(bl):
		return "blue"
	}
	return "other"
}

// TestBouncedStillProxyE2E: a still of a bounced clip at a mirrored-half
// time shows the mirrored source frame (never a seek past the clip), and
// the bounced proxy renders.
func TestBouncedStillProxyE2E(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	m := NewManager(st, tools, Options{Concurrency: 1})
	data, info := bounceGIF(t)
	blob, err := st.PutBlob(bytes.NewReader(data), "bounce.gif")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ops := []recipe.Op{op("bounce", "")}
	out := recipe.Output{Format: "gif"}
	// The bounced output is frames 0,1,2,3,3,2,1,0 at 10 fps. Frame 6 (t =
	// 0.65 s) is source frame 1 (green) — inside the mirrored half, past the
	// source's own 0.4 s duration.
	still := func(t64 float64) string {
		pngData, err := m.Still(ctx, blob.Hash, ops, out, t64, 0)
		if err != nil {
			t.Fatalf("Still(%v): %v", t64, err)
		}
		return stillClass(t, pngData)
	}
	if got := still(0.65); got != "green" {
		t.Errorf("mirrored-half frame 6 = %s, want green (source frame 1)", got)
	}
	if got := still(0.45); got != "white" { // frame 4 = source frame 3
		t.Errorf("mirrored-half frame 4 = %s, want white (source frame 3)", got)
	}
	if got := still(0.05); got != "red" { // forward half unchanged
		t.Errorf("forward frame 0 = %s, want red", got)
	}
	if got := still(0.75); got != "red" { // last output frame = source frame 0
		t.Errorf("mirrored-half frame 7 = %s, want red (source frame 0)", got)
	}

	proxy, err := m.Proxy(ctx, []string{blob.Hash}, ops, out, 0, 0)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if len(proxy) < 12 || string(proxy[:4]) != "RIFF" || string(proxy[8:12]) != "WEBP" {
		t.Fatalf("bounced proxy is not a WebP (%d bytes)", len(proxy))
	}
}

// coalesceGIF flattens a decoded GIF into full frames (frames may be
// optimised partial rects with transparent unchanged pixels).
func coalesceGIF(g *gif.GIF) []*image.RGBA {
	bounds := image.Rect(0, 0, g.Config.Width, g.Config.Height)
	canvas := image.NewRGBA(bounds)
	frames := make([]*image.RGBA, 0, len(g.Image))
	for _, fr := range g.Image {
		draw.Draw(canvas, fr.Bounds(), fr, fr.Bounds().Min, draw.Over)
		cp := image.NewRGBA(bounds)
		draw.Draw(cp, bounds, canvas, bounds.Min, draw.Src)
		frames = append(frames, cp)
	}
	return frames
}

// TestFastPathE2E: eligible GIF → GIF recipes are rendered by gifsicle
// alone — the desc says so, the report passes, and the kept frames carry the
// source pixels (byte-level: crop geometry, trim/drop frame counts, merged
// delays).
func TestFastPathE2E(t *testing.T) {
	tools := realTools(t)
	if tools.Gifsicle == "" {
		t.Skip("gifsicle not on PATH")
	}
	st := newTestStore(t)
	m := NewManager(st, tools, Options{Concurrency: 1})
	srcData := opaqueGIF(t)
	src := putGIFSource(t, st, srcData)

	t.Run("crop only", func(t *testing.T) {
		r := recipe.Recipe{Sources: []string{src},
			Ops:    []recipe.Op{op("crop", `{"x":8,"y":8,"w":24,"h":16}`)},
			Output: recipe.Output{Format: "gif", Target: "attachment"}}
		job := runJob(t, m, r)
		f := primaryFile(t, job)
		if f.Desc != FastPathDesc {
			t.Fatalf("desc = %q, want %q", f.Desc, FastPathDesc)
		}
		if f.Report == nil || !f.Report.OK {
			t.Fatalf("report = %+v", f.Report)
		}
		data := resultBytes(t, st, job, f.Name)
		if int64(len(data)) > int64(len(srcData)) {
			t.Errorf("cropped fast-path output (%d B) larger than the source (%d B)", len(data), len(srcData))
		}
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if g.Config.Width != 24 || g.Config.Height != 16 || len(g.Image) != 12 {
			t.Fatalf("logical screen %dx%d, %d frames; want 24x16, 12", g.Config.Width, g.Config.Height, len(g.Image))
		}
		// The source's moving red square (x = i*2+2..i*2+14, y = 5..25) maps
		// into crop coords: its center column is at x = i*2, y = 7.
		frames := coalesceGIF(g)
		for i, fr := range frames {
			x := i * 2
			if x >= g.Config.Width {
				break
			}
			if got := colourClass(fr.At(x, 7)); got != "red" {
				t.Errorf("frame %d: pixel (%d, 7) = %s, want red (pixels must be untouched)", i, x, got)
			}
		}
	})

	t.Run("trim and drop", func(t *testing.T) {
		// Trim to frames 2..7 (0.16..0.64 s of the 12.5 fps source), then
		// drop every 2nd of the range: frames 2,4,6 remain at 16 cs each.
		r := recipe.Recipe{Sources: []string{src},
			Ops:    []recipe.Op{op("trim", `{"start":0.16,"end":0.64}`)},
			Output: recipe.Output{Format: "gif", FPS: 6.25}}
		job := runJob(t, m, r)
		f := primaryFile(t, job)
		if f.Desc != FastPathDesc {
			t.Fatalf("desc = %q, want %q", f.Desc, FastPathDesc)
		}
		g, err := gif.DecodeAll(bytes.NewReader(resultBytes(t, st, job, f.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Image) != 3 {
			t.Fatalf("%d frames, want 3 (6 trimmed, every 2nd dropped)", len(g.Image))
		}
		for i, d := range g.Delay {
			if d != 16 {
				t.Errorf("frame %d delay = %d cs, want 16 (8 + the dropped frame's 8)", i, d)
			}
		}
	})

	t.Run("loop count", func(t *testing.T) {
		r := recipe.Recipe{Sources: []string{src},
			Output: recipe.Output{Format: "gif", Loop: 5}}
		job := runJob(t, m, r)
		f := primaryFile(t, job)
		if f.Desc != FastPathDesc {
			t.Fatalf("desc = %q, want %q", f.Desc, FastPathDesc)
		}
		g, err := gif.DecodeAll(bytes.NewReader(resultBytes(t, st, job, f.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if g.LoopCount != 5 {
			t.Errorf("loop count = %d, want 5", g.LoopCount)
		}
	})

	t.Run("ineligible recipes take the decode path", func(t *testing.T) {
		r := recipe.Recipe{Sources: []string{src},
			Ops:    []recipe.Op{op("resize", `{"width":20}`)},
			Output: recipe.Output{Format: "gif"}}
		job := runJob(t, m, r)
		f := primaryFile(t, job)
		if f.Desc == FastPathDesc {
			t.Fatalf("a resize must not take the fast path")
		}
	})
}

// TestGifskiE2E: Output.Encoder "gifski" renders through gifski; the desc
// names it and the delivered GIF decodes with every frame.
func TestGifskiE2E(t *testing.T) {
	tools := realTools(t)
	if tools.Gifski == "" {
		t.Skip("gifski not on PATH")
	}
	st := newTestStore(t)
	m := NewManager(st, tools, Options{Concurrency: 1})
	src := putGIFSource(t, st, opaqueGIF(t))

	r := recipe.Recipe{Sources: []string{src},
		Output: recipe.Output{Format: "gif", Encoder: "gifski", Quality: 90, Target: "attachment"}}
	job := runJob(t, m, r)
	f := primaryFile(t, job)
	if !strings.Contains(f.Desc, "gifski") {
		t.Errorf("desc = %q, must name gifski", f.Desc)
	}
	if f.Report == nil || !f.Report.OK {
		t.Fatalf("report = %+v", f.Report)
	}
	g, err := gif.DecodeAll(bytes.NewReader(resultBytes(t, st, job, f.Name)))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Image) != 12 {
		t.Errorf("%d frames, want 12", len(g.Image))
	}
	if g.LoopCount != 0 {
		t.Errorf("loop count = %d, want 0 (forever)", g.LoopCount)
	}

	if tools.Gifsicle != "" {
		// No Discord target here: the linter forces a Discord target's loop
		// count to forever, so a finite count only survives target "".
		r.Output.Loop = 4
		r.Output.Target = ""
		job = runJob(t, m, r)
		f = primaryFile(t, job)
		g, err = gif.DecodeAll(bytes.NewReader(resultBytes(t, st, job, f.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if g.LoopCount != 4 {
			t.Errorf("restated loop count = %d, want 4", g.LoopCount)
		}
	}
}
