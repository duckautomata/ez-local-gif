package jobs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/probe"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Phase 2 end-to-end renders. Every test skips without ffmpeg/ffprobe; the
// ones that need gifsicle / pngquant / oxipng / avifenc skip on the host and
// run in the tools image (ezlg-dev).

const e2eTimeout = 5 * time.Minute

// buildClip renders a synthetic clip (testsrc2, optionally with a soft
// alpha gradient on the right half) as ProRes 4444 (alpha) or mpeg4
// (opaque) and returns its path.
func buildClip(t *testing.T, tools ffrun.Tools, dir, name string, w, h int, fps float64, dur float64, alpha bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	p := filepath.Join(dir, name)
	src := fmt.Sprintf("testsrc2=size=%dx%d:rate=%g:duration=%g", w, h, fps, dur)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi"}
	if alpha {
		src += fmt.Sprintf(",format=rgba,geq=r=r(X\\,Y):g=g(X\\,Y):b=b(X\\,Y):a=if(lt(X\\,%d)\\,255\\,128)", w/2)
		args = append(args, "-i", src, "-c:v", "prores_ks", "-profile:v", "4444", "-pix_fmt", "yuva444p10le", p)
	} else {
		args = append(args, "-i", src, "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", p)
	}
	if out, err := exec.CommandContext(ctx, tools.FFmpeg, args...).CombinedOutput(); err != nil {
		t.Skipf("cannot build %s with this ffmpeg: %v\n%s", name, err, out)
	}
	return p
}

// putProbed stores a file as a blob with real probe info.
func putProbed(t *testing.T, st *store.Store, tools ffrun.Tools, path string) *store.Blob {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := st.PutBlob(bytes.NewReader(data), filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatalf("probe %s: %v", filepath.Base(path), err)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	blob.Info = &info
	return blob
}

// mustRender runs a recipe to completion and returns the final job.
func mustRender(t *testing.T, m *Manager, r recipe.Recipe) Job {
	t.Helper()
	j, err := m.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(e2eTimeout)
	for time.Now().Before(deadline) {
		got, ok := m.Get(j.ID)
		if !ok {
			t.Fatalf("job vanished")
		}
		if got.IsFinished() {
			if got.State != StateDone {
				t.Fatalf("job failed at stage %s: %s", got.Stage, got.Error)
			}
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job did not finish in %v", e2eTimeout)
	return Job{}
}

// resultFile reads a delivered file from the result dir.
func resultFile(t *testing.T, st *store.Store, r recipe.Recipe, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(st.ResultDir(ResultKey(r)), name))
	if err != nil {
		t.Fatalf("result file %s: %v", name, err)
	}
	return data
}

func logFiles(t *testing.T, res *Result) {
	t.Helper()
	for _, f := range res.Files {
		ok := "-"
		if f.Report != nil {
			ok = fmt.Sprint(f.Report.OK)
		}
		t.Logf("  %-12s kind=%-11s idx=%d %7d B %dx%d %d frames %.2f fps %.2fs ok=%s desc=%q", f.Name, f.Kind, f.Index, f.Bytes, f.Width, f.Height, f.Frames, f.FPS, f.Duration, ok, f.Desc)
	}
	t.Logf("  render %d ms", res.RenderMS)
}

// TestRenderFitEmoteGIF: a detailed 128x128 25 fps alpha clip does not fit
// the emote budget at the mildest rung; the fit engine must deliver a GIF
// <= 262144 bytes with a clean report and at least one alternative.
func TestRenderFitEmoteGIF(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	dir := t.TempDir()
	clip := buildClip(t, tools, dir, "emote.mov", 128, 128, 25, 6, true)
	blob := putProbed(t, st, tools, clip)
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Target: "emote", Width: 128, Height: 128, FitBytes: 262144}}
	fin := mustRender(t, m, r)
	res := fin.Result
	logFiles(t, res)
	if len(res.Files) < 2 {
		t.Fatalf("want primary + alternatives, got %d files", len(res.Files))
	}
	p := res.Files[0]
	if p.Name != "out.gif" || p.Format != "gif" || p.Kind != FileKindOutput || p.Index != 0 {
		t.Errorf("primary = %+v", p)
	}
	if p.Bytes > 262144 || p.Limit != 262144 {
		t.Errorf("primary %d bytes, limit %d", p.Bytes, p.Limit)
	}
	if p.Report == nil || !p.Report.OK || p.Report.Format != "gif" {
		t.Errorf("primary report = %+v", p.Report)
	}
	if !strings.HasPrefix(p.Desc, "fit at ") || !strings.Contains(p.Desc, "lossy") {
		t.Errorf("primary desc = %q", p.Desc)
	}
	if p.Width != 128 || p.Height != 128 || p.Frames < 2 || p.FPS <= 0 {
		t.Errorf("primary facts = %+v", p)
	}
	if !p.Report.HasAlpha {
		t.Error("alpha clip: primary report must carry alpha (master scan)")
	}
	for i, f := range res.Files[1:] {
		if f.Kind != FileKindAlternative || f.Index != i+1 || f.Name != fmt.Sprintf("alt%d.gif", i+1) {
			t.Errorf("alternative %d = %+v", i+1, f)
		}
		if f.Bytes > 262144 || f.Report == nil || !f.Report.OK || f.Desc == "" {
			t.Errorf("alternative %d over budget or not ok: %+v", i+1, f)
		}
		if f.Desc == p.Desc {
			t.Errorf("alternative %d has the primary's description", i+1)
		}
	}
	// Every listed file is on disk with the manifest's size; the fit scratch
	// dir is gone with the scratch.
	for _, f := range res.Files {
		if int64(len(resultFile(t, st, r, f.Name))) != f.Bytes {
			t.Errorf("%s size mismatch", f.Name)
		}
	}
	if _, err := os.Stat(filepath.Join(st.Scratch, fin.ID)); !os.IsNotExist(err) {
		t.Errorf("scratch dir left behind: %v", err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(resultFile(t, st, r, "out.gif")))
	if err != nil || len(g.Image) != p.Frames || g.Config.Width != 128 {
		t.Errorf("decode primary: %v (%d frames)", err, len(g.Image))
	}
	// Served from cache on resubmit, alternatives included.
	j2, err := m.Submit(r)
	if err != nil || j2.State != StateDone || j2.Result == nil || !j2.Result.Cached || len(j2.Result.Files) != len(res.Files) {
		t.Errorf("cached resubmit: %+v %v", j2, err)
	}
}

// TestRenderFitStickerAPNG: indexed APNG is the sticker's default rung; the
// winner must be <= 524288 bytes with an OK report (apng, or gif when the
// APNG rungs did not fit).
func TestRenderFitStickerAPNG(t *testing.T) {
	tools := realTools(t)
	if tools.Pngquant == "" {
		t.Skip("pngquant not on PATH (run in the tools image)")
	}
	st := newTestStore(t)
	clip := buildClip(t, tools, t.TempDir(), "sticker.mov", 320, 320, 25, 2, true)
	blob := putProbed(t, st, tools, clip)
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "apng", Target: "sticker", Width: 320, Height: 320, FitBytes: 524288}}
	fin := mustRender(t, m, r)
	res := fin.Result
	logFiles(t, res)
	p := res.Files[0]
	if p.Bytes > 524288 || p.Limit != 524288 || p.Report == nil || !p.Report.OK {
		t.Errorf("primary = %+v report %+v", p, p.Report)
	}
	if p.Format != "apng" && p.Format != "gif" {
		t.Errorf("primary format = %q", p.Format)
	}
	if p.Format == "apng" {
		if p.Name != "out.png" || p.Report.Format != "apng" {
			t.Errorf("apng primary = %+v", p)
		}
		// An "APNG · RGBA" winner must actually be truecolour bytes and an
		// indexed winner indexed — the desc and the file must agree.
		truecolor := strings.Contains(p.Desc, "RGBA")
		var indexed bool
		for _, c := range p.Report.Checks {
			if c.Rule == discordlint.RuleAPNGIndexed && c.OK {
				indexed = true
			}
		}
		if indexed == truecolor {
			t.Errorf("desc %q vs indexed=%v: %+v", p.Desc, indexed, p.Report.Checks)
		}
		img, err := png.Decode(bytes.NewReader(resultFile(t, st, r, "out.png")))
		if err != nil {
			t.Fatalf("decode primary png: %v", err)
		}
		if _, paletted := img.(*image.Paletted); paletted == truecolor {
			t.Errorf("desc %q but the first frame decodes as %T", p.Desc, img)
		}
	}
	if p.Width != 320 || p.Height != 320 || p.Frames < 2 || p.Duration > 5 {
		t.Errorf("primary facts = %+v", p)
	}
	if !strings.HasPrefix(p.Desc, "fit at ") {
		t.Errorf("desc = %q", p.Desc)
	}
	for _, f := range res.Files[1:] {
		if f.Kind != FileKindAlternative || f.Bytes > 524288 || f.Report == nil || !f.Report.OK {
			t.Errorf("alternative = %+v", f)
		}
	}
	// Files on disk.
	for _, f := range res.Files {
		if int64(len(resultFile(t, st, r, f.Name))) != f.Bytes {
			t.Errorf("%s size mismatch", f.Name)
		}
	}
}

// gradientSequence stores n 320x320 PNG frames with far more than 256
// colours (smooth RGB + alpha gradients, shifting per frame): heavy in
// colours but light in bytes, so a truecolour APNG of it easily fits the
// sticker budget while pngquant-style indexing could never be lossless.
func gradientSequence(t *testing.T, st *store.Store, tools ffrun.Tools, n, delayMS int) *store.Blob {
	t.Helper()
	parts := make([]store.SequencePart, 0, n)
	for i := 0; i < n; i++ {
		img := image.NewNRGBA(image.Rect(0, 0, 320, 320))
		for y := 0; y < 320; y++ {
			for x := 0; x < 320; x++ {
				img.SetNRGBA(x, y, color.NRGBA{
					R: uint8((x + i*7) % 256),
					G: uint8(y % 256),
					B: uint8((x + y) % 256),
					A: uint8(255 - x*200/319),
				})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, store.SequencePart{Name: fmt.Sprintf("g_%03d.png", i+1), R: bytes.NewReader(buf.Bytes())})
	}
	blob, err := st.PutSequence(parts)
	if err != nil {
		t.Fatalf("PutSequence: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.ProbeSequence(ctx, tools, blob.Path, delayMS)
	if err != nil {
		t.Fatalf("ProbeSequence: %v", err)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	blob.Info = &info
	return blob
}

// TestRenderFitStickerRGBAAPNG: a sticker whose RGBA truecolour probe fits
// must deliver real truecolour bytes — before the fix the RGBA-labelled rung
// silently went through the pngquant pipeline, so the winner said
// "APNG · RGBA" while its bytes were indexed.
func TestRenderFitStickerRGBAAPNG(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob := gradientSequence(t, st, tools, 4, 100) // 10 fps, 0.4 s
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "apng", Target: "sticker", Width: 320, Height: 320, FitBytes: 524288}}
	fin := mustRender(t, m, r)
	res := fin.Result
	logFiles(t, res)
	p := res.Files[0]
	if p.Format != "apng" || p.Bytes > 524288 || p.Report == nil || !p.Report.OK {
		t.Fatalf("primary = %+v report %+v", p, p.Report)
	}
	if !strings.Contains(p.Desc, "RGBA") {
		t.Fatalf("desc = %q, want the RGBA probe rung to win", p.Desc)
	}
	for _, c := range p.Report.Checks {
		if c.Rule == discordlint.RuleAPNGIndexed && c.OK {
			t.Errorf("RGBA winner reports an indexed palette: %+v", c)
		}
	}
	img, err := png.Decode(bytes.NewReader(resultFile(t, st, r, "out.png")))
	if err != nil {
		t.Fatal(err)
	}
	if _, paletted := img.(*image.Paletted); paletted {
		t.Errorf("RGBA winner decodes as a paletted image (the indexed pipeline ran)")
	}
	if !p.Report.HasAlpha {
		t.Error("gradient alpha lost")
	}
}

// TestRenderFitEmoteGIFLossyZero: the fit search must not degrade below the
// user's own quality when it already fits — an emote GIF at lossy 0 whose
// mildest rung fits reports "lossy 0", not the knob's old default mild
// (lossy 30).
func TestRenderFitEmoteGIFLossyZero(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Target: "emote", FitBytes: 262144}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Bytes > 262144 || p.Report == nil || !p.Report.OK {
		t.Fatalf("primary = %+v report %+v", p, p.Report)
	}
	if !strings.HasPrefix(p.Desc, "fit at ") || !strings.Contains(p.Desc, "lossy 0") {
		t.Errorf("desc = %q, want the user's own lossy 0", p.Desc)
	}
}

// TestRenderFitWebPLossless: Output.Lossless is honoured by the fit search —
// when the lossless probe fits it wins and says so instead of being silently
// replaced by a lossy q80 encode.
func TestRenderFitWebPLossless(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "webp", Target: "attachment", Lossless: true, FitBytes: 1 << 20}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Format != "webp" || p.Bytes > 1<<20 || p.Report == nil || !p.Report.OK {
		t.Fatalf("primary = %+v report %+v", p, p.Report)
	}
	if p.Desc != "fit at lossless" {
		t.Errorf("desc = %q, want %q", p.Desc, "fit at lossless")
	}
}

// TestRenderAPNG covers the plain APNG paths: RGBA (no tools beyond ffmpeg)
// and indexed (pngquant), both looping forever with every source frame.
func TestRenderAPNG(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "apng", Target: "sticker"}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Name != "out.png" || p.Format != "apng" || p.Kind != FileKindOutput || p.Report == nil {
		t.Fatalf("apng primary = %+v", p)
	}
	if p.Report.Format != "apng" || p.Frames != 12 || !p.Report.LoopForever || !p.Report.OK {
		t.Errorf("apng report = %+v", p.Report)
	}
	if !p.Report.HasAlpha || p.Width != 40 || p.Height != 30 || p.FPS < 12 || p.FPS > 13 {
		t.Errorf("apng facts = %+v", p)
	}
	data := resultFile(t, st, r, "out.png")
	if !bytes.HasPrefix(data, []byte("\x89PNG")) || !bytes.Contains(data, []byte("acTL")) {
		t.Error("out.png is not an APNG")
	}
	// report.json on disk mirrors the manifest.
	raw, err := os.ReadFile(filepath.Join(st.ResultDir(ResultKey(r)), reportName))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk discordlint.Report
	if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk.Frames != 12 || onDisk.Format != "apng" {
		t.Errorf("report.json = %+v (%v)", onDisk, err)
	}

	if tools.Pngquant == "" {
		t.Log("pngquant not on PATH: indexed APNG path skipped")
		return
	}
	ri := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "apng", Target: "sticker", Colors: 64}}
	fin = mustRender(t, m, ri)
	logFiles(t, fin.Result)
	p = fin.Result.Files[0]
	if p.Format != "apng" || p.Frames != 12 || p.Report == nil || !p.Report.OK || !p.Report.HasAlpha {
		t.Errorf("indexed apng = %+v report %+v", p, p.Report)
	}
	var indexed bool
	for _, c := range p.Report.Checks {
		if c.Rule == discordlint.RuleAPNGIndexed && c.OK {
			indexed = true
			t.Logf("indexed: %s", c.Detail)
		}
	}
	if !indexed {
		t.Errorf("colours 64 did not yield an indexed apng: %+v", p.Report.Checks)
	}
}

// TestRenderAVIF: animated and still AVIF through avifenc (tools image
// only). The animated output is probed back: FFmpeg must see the animation
// track (not the one-frame primary item) and the alpha stream.
func TestRenderAVIF(t *testing.T) {
	tools := realTools(t)
	if tools.Avifenc == "" {
		t.Skip("avifenc not on PATH (run in the tools image)")
	}
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "avif", Target: "attachment", Quality: 50}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Name != "out.avif" || p.Format != "avif" || p.Frames != 12 || p.Report == nil || !p.Report.OK || !p.Report.HasAlpha {
		t.Errorf("avif = %+v report %+v", p, p.Report)
	}
	if p.Duration < 0.9 || p.Duration > 1.1 || p.FPS < 12 || p.FPS > 13 {
		t.Errorf("avif timing = %+v", p)
	}
	out := filepath.Join(st.ResultDir(ResultKey(r)), p.Name)
	back, err := probe.Probe(ctx, tools, out, 0)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	t.Logf("probe of out.avif: %+v", back)
	if back.Kind != recipe.KindAnimation || back.Frames != 12 || !back.HasAlpha || back.AlphaStream == 0 {
		t.Errorf("animated avif round trip: %+v", back)
	}

	// Still AVIF from a one-frame source (the GIF's first frame via trim is
	// not a still; use a PNG).
	img := image.NewNRGBA(image.Rect(0, 0, 48, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			img.Set(x, y, color.NRGBA{R: 200, G: 80, B: 40, A: uint8(255 - x*4)})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	pb, _ := st.PutBlob(bytes.NewReader(buf.Bytes()), "still.png")
	pinfo, err := probe.Probe(ctx, tools, pb.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(pb.Hash, pinfo)
	rs := recipe.Recipe{Sources: []string{pb.Hash}, Output: recipe.Output{Format: "avif", Target: "emote"}}
	fin = mustRender(t, m, rs)
	logFiles(t, fin.Result)
	p = fin.Result.Files[0]
	if p.Frames != 1 || p.Format != "avif" || p.Report == nil || !p.Report.OK || p.Width != 48 || p.Height != 32 || !p.Report.HasAlpha {
		t.Errorf("still avif = %+v report %+v", p, p.Report)
	}
	back, err = probe.Probe(ctx, tools, filepath.Join(st.ResultDir(ResultKey(rs)), p.Name), 0)
	if err != nil || !back.IsStill || !back.HasAlpha {
		t.Errorf("still avif round trip: %+v %v", back, err)
	}
}

// TestRenderStatic: png (plain and quantised) and jpeg from an animated
// source take its first frame.
func TestRenderStatic(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 2})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "png", Target: "sticker"}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Name != "out.png" || p.Format != "png" || p.Frames != 1 || p.Width != 40 || p.Height != 30 || p.Report == nil || !p.Report.OK || p.Report.Format != "png" {
		t.Errorf("png = %+v report %+v", p, p.Report)
	}
	if !p.Report.HasAlpha {
		t.Error("png of a transparent gif frame must carry alpha")
	}
	img, err := png.Decode(bytes.NewReader(resultFile(t, st, r, "out.png")))
	if err != nil || img.Bounds().Dx() != 40 {
		t.Errorf("decode png: %v", err)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("frame 0 corner should be transparent, alpha %d", a)
	}
	if tools.Pngquant != "" {
		// DESIGN §4.2: the default static PNG runs pngquant --quality 70-100,
		// so this trivially-quantisable frame must come out paletted.
		if _, paletted := img.(*image.Paletted); !paletted {
			t.Errorf("default png is not paletted (%T); the pngquant quality pass did not run", img)
		}
	}

	if tools.Pngquant != "" {
		rq := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "png", Target: "sticker", Colors: 8}}
		fin = mustRender(t, m, rq)
		logFiles(t, fin.Result)
		q := fin.Result.Files[0]
		if q.Frames != 1 || q.Report == nil || !q.Report.OK || !q.Report.HasAlpha {
			t.Errorf("quantised png = %+v report %+v", q, q.Report)
		}
		qimg, err := png.Decode(bytes.NewReader(resultFile(t, st, rq, "out.png")))
		if err != nil {
			t.Fatal(err)
		}
		if _, paletted := qimg.(*image.Paletted); !paletted {
			t.Errorf("pngquant output is not paletted (%T)", qimg)
		}
	}

	rj := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "jpeg", Quality: 85, Matte: "ffffff"}}
	fin = mustRender(t, m, rj)
	logFiles(t, fin.Result)
	j := fin.Result.Files[0]
	if j.Name != "out.jpg" || j.Format != "jpeg" || j.Frames != 1 || j.Report == nil || !j.Report.OK || j.Report.Format != "jpeg" || j.Report.HasAlpha {
		t.Errorf("jpeg = %+v report %+v", j, j.Report)
	}
	if data := resultFile(t, st, rj, "out.jpg"); !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
		t.Error("out.jpg is not a JPEG")
	}
	if j.Width != 40 || j.Height != 30 {
		t.Errorf("jpeg dims = %dx%d", j.Width, j.Height)
	}
}

// TestRenderFitPNG: Output.FitBytes is honoured for static PNG (it used to
// be silently ignored while the result card claimed a fit search ran): a
// generous budget fits at the mildest rung with a "fit at" desc and
// alternatives, an impossible budget delivers the smallest attempt with a
// failing fit.target check.
func TestRenderFitPNG(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 1})

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "png", Target: "sticker", FitBytes: 1 << 20}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Name != "out.png" || p.Format != "png" || p.Kind != FileKindOutput || p.Frames != 1 {
		t.Errorf("primary = %+v", p)
	}
	if p.Bytes > 1<<20 || p.Report == nil || !p.Report.OK || p.Report.Format != "png" {
		t.Errorf("primary %d bytes, report %+v", p.Bytes, p.Report)
	}
	if !strings.HasPrefix(p.Desc, "fit at ") {
		t.Errorf("desc = %q, want a fit description", p.Desc)
	}
	if !p.Report.HasAlpha {
		t.Error("png of a transparent gif frame must carry alpha")
	}
	img, err := png.Decode(bytes.NewReader(resultFile(t, st, r, "out.png")))
	if err != nil || img.Bounds().Dx() != 40 || img.Bounds().Dy() != 30 {
		t.Errorf("decode primary png: %v", err)
	}
	if len(fin.Result.Files) < 2 {
		t.Errorf("want at least one alternative, got %d files", len(fin.Result.Files))
	}
	for i, f := range fin.Result.Files[1:] {
		if f.Kind != FileKindAlternative || f.Index != i+1 || f.Bytes > 1<<20 || f.Report == nil {
			t.Errorf("alternative %d = %+v", i+1, f)
		}
	}

	// An impossible budget delivers the smallest attempt, flagged.
	ri := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "png", Target: "sticker", FitBytes: 64}}
	fin = mustRender(t, m, ri)
	logFiles(t, fin.Result)
	p = fin.Result.Files[0]
	if p.Report == nil || p.Report.OK || !strings.HasPrefix(p.Desc, "cannot fit under 64 B") || p.Bytes <= 64 {
		t.Errorf("impossible fit = %+v report %+v", p, p.Report)
	}
	var flagged bool
	for _, c := range p.Report.Checks {
		if c.Rule == RuleFitTarget && !c.OK && c.Level == discordlint.LevelError {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("fit.target check missing: %+v", p.Report.Checks)
	}
}

// TestRenderFrames: every frame as png/jpeg/webp plus frames.zip.
func TestRenderFrames(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 1})

	for _, ff := range []string{"", "jpeg", "webp"} {
		t.Run("format="+ff, func(t *testing.T) {
			r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "frames", FrameFormat: ff}}
			fin := mustRender(t, m, r)
			res := fin.Result
			logFiles(t, res)
			wantExt := map[string]string{"": "png", "jpeg": "jpg", "webp": "webp"}[ff]
			wantFormat := map[string]string{"": "png", "jpeg": "jpeg", "webp": "webp"}[ff]
			if len(res.Files) != 14 {
				t.Fatalf("%d files, want zip + delays.json + 12 frames", len(res.Files))
			}
			arc := res.Files[0]
			if arc.Name != "frames.zip" || arc.Kind != FileKindArchive || arc.Format != "zip" || arc.Frames != 12 || arc.Bytes == 0 || arc.Report != nil {
				t.Errorf("archive = %+v", arc)
			}
			del := res.Files[1]
			if del.Name != framesDelaysName || del.Kind != "" || del.Format != "json" || del.Desc != "per-frame timing" || del.Bytes == 0 || del.Report != nil {
				t.Errorf("delays = %+v", del)
			}
			var timings []frameTiming
			if err := json.Unmarshal(resultFile(t, st, r, framesDelaysName), &timings); err != nil {
				t.Fatalf("delays.json: %v", err)
			}
			if len(timings) != 12 {
				t.Fatalf("delays.json has %d entries", len(timings))
			}
			for i, tm := range timings { // 12.5 fps = 80 ms per frame
				if tm.Index != i+1 || tm.TMs != i*80 || tm.DurationMs != 80 {
					t.Errorf("timing %d = %+v", i, tm)
				}
			}
			for i, f := range res.Files[2:] {
				if f.Kind != FileKindFrame || f.Index != i+1 || f.Name != fmt.Sprintf("f%05d.%s", i+1, wantExt) || f.Format != wantFormat {
					t.Errorf("frame %d = %+v", i+1, f)
				}
				if f.Width != 40 || f.Height != 30 || f.Frames != 1 || f.Bytes == 0 || f.Limit != 0 || f.Report != nil {
					t.Errorf("frame %d facts = %+v", i+1, f)
				}
				wantDesc := fmt.Sprintf("frame %d (%.2f s)", i+1, float64(i)/12.5)
				if f.Desc != wantDesc {
					t.Errorf("frame %d desc = %q, want %q", i+1, f.Desc, wantDesc)
				}
				if int64(len(resultFile(t, st, r, f.Name))) != f.Bytes {
					t.Errorf("%s size mismatch", f.Name)
				}
			}
			zr, err := zip.NewReader(bytes.NewReader(resultFile(t, st, r, "frames.zip")), arc.Bytes)
			if err != nil {
				t.Fatalf("zip: %v", err)
			}
			if len(zr.File) != 13 || zr.File[0].Name != "f00001."+wantExt || zr.File[0].Method != zip.Store {
				t.Errorf("zip entries = %d, first %s method %d", len(zr.File), zr.File[0].Name, zr.File[0].Method)
			}
			if zr.File[12].Name != framesDelaysName {
				t.Errorf("zip lacks %s as its last entry: %s", framesDelaysName, zr.File[12].Name)
			}
			if ff == "" {
				img, err := png.Decode(bytes.NewReader(resultFile(t, st, r, "f00003.png")))
				if err != nil || img.Bounds().Dx() != 40 || img.Bounds().Dy() != 30 {
					t.Errorf("frame png: %v", err)
				}
			}
		})
	}
}

// buildSequence writes n PNG frames (moving square, alpha) as a store
// sequence and probes it.
func buildSequence(t *testing.T, st *store.Store, tools ffrun.Tools, n int, delayMS int) *store.Blob {
	t.Helper()
	parts := make([]store.SequencePart, 0, n)
	for i := 0; i < n; i++ {
		img := image.NewNRGBA(image.Rect(0, 0, 32, 24))
		for y := 4; y < 20; y++ {
			for x := i*2 + 2; x < i*2+12 && x < 32; x++ {
				img.Set(x, y, color.NRGBA{R: 220, G: 30, B: 30, A: 255})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, store.SequencePart{Name: fmt.Sprintf("frame_%03d.png", i+1), R: bytes.NewReader(buf.Bytes())})
	}
	blob, err := st.PutSequence(parts)
	if err != nil {
		t.Fatalf("PutSequence: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.ProbeSequence(ctx, tools, blob.Path, delayMS)
	if err != nil {
		t.Fatalf("ProbeSequence: %v", err)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	blob.Info = &info
	return blob
}

// TestRenderSequenceSource: an uploaded PNG sequence renders to GIF (and
// WebP) at its delay rate and serves stills.
func TestRenderSequenceSource(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob := buildSequence(t, st, tools, 8, 100)
	if blob.Info.Kind != recipe.KindSequence || blob.Info.Frames != 8 || blob.Info.FPS != 10 || !blob.Info.HasAlpha {
		t.Fatalf("sequence probe: %+v", blob.Info)
	}
	m := NewManager(st, tools, Options{Concurrency: 1})
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()

	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Target: "attachment"}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Frames != 8 || p.Width != 32 || p.Height != 24 || p.Report == nil || !p.Report.OK || !p.Report.HasAlpha {
		t.Errorf("sequence gif = %+v report %+v", p, p.Report)
	}
	if p.FPS < 9.5 || p.FPS > 10.5 || p.Duration < 0.75 || p.Duration > 0.85 {
		t.Errorf("sequence gif timing = %+v", p)
	}
	g, err := gif.DecodeAll(bytes.NewReader(resultFile(t, st, r, "out.gif")))
	if err != nil || len(g.Image) != 8 {
		t.Errorf("decode: %v (%d frames)", err, len(g.Image))
	}
	// The delay op overrides the rate (20 fps → 0.4 s).
	rd := recipe.Recipe{Sources: []string{blob.Hash}, Ops: []recipe.Op{{Kind: recipe.OpDelay, Params: json.RawMessage(`{"ms":50}`)}}, Output: recipe.Output{Format: "webp"}}
	fin = mustRender(t, m, rd)
	logFiles(t, fin.Result)
	if w := fin.Result.Files[0]; w.Frames != 8 || w.FPS < 19 || w.FPS > 21 {
		t.Errorf("delay op webp = %+v", w)
	}
	// Stills straight from the sequence directory.
	for _, tt := range []float64{0, 0.35, 0.7} {
		data, err := m.Still(ctx, blob.Hash, nil, recipe.Output{Format: "gif"}, tt, 0)
		if err != nil {
			t.Fatalf("Still t=%v: %v", tt, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 24 {
			t.Errorf("still t=%v: %v", tt, err)
		}
	}
	// The square moves: frame 0's still and frame 7's still differ.
	s0, _ := m.Still(ctx, blob.Hash, nil, recipe.Output{Format: "gif"}, 0, 0)
	s7, _ := m.Still(ctx, blob.Hash, nil, recipe.Output{Format: "gif"}, 0.7, 0)
	if bytes.Equal(s0, s7) {
		t.Error("stills at 0 s and 0.7 s are identical; the sequence is not being scrubbed")
	}
}

// TestRenderOptimize: the gifsicle-only path keeps the source's frames and
// merges delays when dropping every 2nd frame; with a fit budget it
// delivers alternatives.
func TestRenderOptimize(t *testing.T) {
	tools := realTools(t)
	if tools.Gifsicle == "" {
		t.Skip("gifsicle not on PATH (run in the tools image)")
	}
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif") // 12 frames, 8 cs = 12.5 fps
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(blob.Hash, info)
	m := NewManager(st, tools, Options{Concurrency: 1})

	// Lossless pass-through: every frame, delays kept.
	r := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Preset: "optimize", Target: "attachment"}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	p := fin.Result.Files[0]
	if p.Frames != 12 || p.Report == nil || !p.Report.OK || p.Format != "gif" || !strings.HasPrefix(p.Desc, "gifsicle") {
		t.Errorf("optimize = %+v report %+v", p, p.Report)
	}
	g, err := gif.DecodeAll(bytes.NewReader(resultFile(t, st, r, "out.gif")))
	if err != nil || len(g.Image) != 12 || g.Delay[0] != 8 {
		t.Errorf("decode: %v frames=%d delay0=%v", err, len(g.Image), g.Delay)
	}
	// Lossy + half the frame rate: 6 frames, delays doubled to 16 cs.
	rh := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Preset: "optimize", Target: "attachment", Lossy: 30, FPS: 6.25}}
	fin = mustRender(t, m, rh)
	logFiles(t, fin.Result)
	p = fin.Result.Files[0]
	if p.Frames != 6 || p.Report == nil || !p.Report.OK || !strings.Contains(p.Desc, "every 2nd frame dropped") || !strings.Contains(p.Desc, "lossy 30") {
		t.Errorf("optimize drop = %+v", p)
	}
	g, err = gif.DecodeAll(bytes.NewReader(resultFile(t, st, rh, "out.gif")))
	if err != nil || len(g.Image) != 6 {
		t.Fatalf("decode dropped: %v frames=%d", err, len(g.Image))
	}
	for i, d := range g.Delay {
		if d != 16 {
			t.Errorf("frame %d delay = %d cs, want 16 (merged)", i, d)
		}
	}
	if p.Duration < 0.9 || p.Duration > 1.0 {
		t.Errorf("duration after drop = %v, want ~0.96 s", p.Duration)
	}
	// Fit: a generous budget fits at the first rung, with alternatives.
	rf := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Preset: "optimize", Target: "attachment", FitBytes: 1 << 20}}
	fin = mustRender(t, m, rf)
	logFiles(t, fin.Result)
	p = fin.Result.Files[0]
	if p.Frames != 12 || !strings.HasPrefix(p.Desc, "fit at all frames · source colours · lossy") || p.Report == nil || !p.Report.OK {
		t.Errorf("optimize fit = %+v", p)
	}
	if len(fin.Result.Files) < 2 || fin.Result.Files[1].Kind != FileKindAlternative {
		t.Errorf("optimize fit alternatives = %+v", fin.Result.Files)
	}
	// An impossible budget delivers the smallest attempt, flagged.
	ri := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Preset: "optimize", FitBytes: 64}}
	fin = mustRender(t, m, ri)
	logFiles(t, fin.Result)
	p = fin.Result.Files[0]
	if p.Report == nil || p.Report.OK || !strings.HasPrefix(p.Desc, "cannot fit under 64 B") || p.Bytes <= 64 {
		t.Errorf("impossible fit = %+v report %+v", p, p.Report)
	}
	var flagged bool
	for _, c := range p.Report.Checks {
		if c.Rule == RuleFitTarget && !c.OK {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("fit.target check missing: %+v", p.Report.Checks)
	}
}

// TestRenderAVIFSource: a still AVIF with alpha (avifenc) is a working
// source: the probe names its alpha stream, the still preview and the
// master carry the alpha. The animated case documents the stream-layout
// gap (see notes): the primary item decodes first.
func TestRenderAVIFSource(t *testing.T) {
	tools := realTools(t)
	if tools.Avifenc == "" {
		t.Skip("avifenc not on PATH (run in the tools image)")
	}
	st := newTestStore(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=1,format=rgba,geq=r=r(X\\,Y):g=g(X\\,Y):b=b(X\\,Y):a=if(lt(X\\,32)\\,255\\,128)",
		"-c:v", "png", filepath.Join(dir, "f%03d.png")).CombinedOutput(); err != nil {
		t.Fatalf("frames: %v\n%s", err, out)
	}
	frames, _ := filepath.Glob(filepath.Join(dir, "f*.png"))
	still := filepath.Join(dir, "still.avif")
	if out, err := exec.CommandContext(ctx, tools.Avifenc, "-j", "all", "-s", "6", "-q", "60", "--qalpha", "90", "-y", "420", frames[0], still).CombinedOutput(); err != nil {
		t.Skipf("avifenc still: %v\n%s", err, out)
	}
	anim := filepath.Join(dir, "anim.avif")
	args := append([]string{"-j", "all", "-s", "8", "-q", "60", "--qalpha", "90", "-y", "420", "--fps", "10", "--repetition-count", "infinite"}, frames...)
	if out, err := exec.CommandContext(ctx, tools.Avifenc, append(args, anim)...).CombinedOutput(); err != nil {
		t.Skipf("avifenc anim: %v\n%s", err, out)
	}
	m := NewManager(st, tools, Options{Concurrency: 1})

	sb := putProbed(t, st, tools, still)
	if !sb.Info.IsStill || !sb.Info.HasAlpha || sb.Info.AlphaStream != 1 {
		t.Fatalf("still avif probe: %+v", sb.Info)
	}
	data, err := m.Still(ctx, sb.Hash, nil, recipe.Output{Format: "webp"}, 0, 0)
	if err != nil {
		t.Fatalf("Still: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(48, 24).RGBA(); a >= 0xffff || a == 0 {
		t.Errorf("still preview lost the soft alpha: alpha %d at the right half", a)
	}
	if _, _, _, a := img.At(10, 24).RGBA(); a != 0xffff {
		t.Errorf("still preview left half should be opaque: %d", a)
	}
	r := recipe.Recipe{Sources: []string{sb.Hash}, Output: recipe.Output{Format: "webp", Target: "emote"}}
	fin := mustRender(t, m, r)
	logFiles(t, fin.Result)
	if p := fin.Result.Files[0]; p.Frames != 1 || p.Report == nil || !p.Report.OK || !p.Report.HasAlpha {
		t.Errorf("webp from still avif = %+v report %+v", p, p.Report)
	}

	ab := putProbed(t, st, tools, anim)
	t.Logf("animated avif probe: %+v", ab.Info)
	// ffmpeg's mov demuxer lists the one-frame primary item (v:0, alpha
	// v:1) before the animation track (colour v:2, alpha v:3); the probe
	// must point the graph at the track.
	if ab.Info.Kind != recipe.KindAnimation || ab.Info.Frames != 10 || ab.Info.ColorStream != 2 || ab.Info.AlphaStream != 3 {
		t.Errorf("animated avif probe: %+v", ab.Info)
	}
	ra := recipe.Recipe{Sources: []string{ab.Hash}, Output: recipe.Output{Format: "webp", Target: "attachment"}}
	fin = mustRender(t, m, ra)
	logFiles(t, fin.Result)
	// libwebp's AnimEncoder merges identical neighbouring frames (the
	// synthetic clip has a few), so assert "animated, full duration, alpha"
	// rather than an exact frame count.
	if p := fin.Result.Files[0]; p.Frames < 2 || p.Duration < 0.9 || p.Duration > 1.1 || p.Report == nil || !p.Report.OK || !p.Report.HasAlpha {
		t.Errorf("animated AVIF source → webp: want an animated 1 s webp with alpha, got %+v report %+v", p, p.Report)
	}
}
