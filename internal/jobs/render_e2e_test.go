package jobs

import (
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

// realTools returns the tools found on PATH or skips the test.
func realTools(t *testing.T) ffrun.Tools {
	t.Helper()
	tools := ffrun.LookupTools()
	if tools.FFmpeg == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			tools.FFmpeg = p
		}
	}
	if tools.FFprobe == "" {
		if p, err := exec.LookPath("ffprobe"); err == nil {
			tools.FFprobe = p
		}
	}
	if tools.Gifsicle == "" {
		if p, err := exec.LookPath("gifsicle"); err == nil {
			tools.Gifsicle = p
		}
	}
	if tools.FFmpeg == "" || tools.FFprobe == "" {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	return tools
}

// ffmpegMajor parses the major version from "ffmpeg version N.x ..." (0 if
// unknown). FFmpeg < 9 cannot decode animated WebP, which matters for the
// verify stage.
func ffmpegMajor(t *testing.T, tools ffrun.Tools) int {
	t.Helper()
	out, err := exec.Command(tools.FFmpeg, "-version").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			var major int
			for _, c := range fields[i+1] {
				if c < '0' || c > '9' {
					break
				}
				major = major*10 + int(c-'0')
			}
			return major
		}
	}
	return 0
}

// animatedGIF builds a 12-frame 40x30 GIF with a moving red square on a
// transparent background.
func animatedGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 0}, color.RGBA{220, 30, 30, 255}, color.RGBA{30, 30, 220, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < 12; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, 40, 30), pal)
		for y := 5; y < 25; y++ {
			for x := i*2 + 2; x < i*2+14; x++ {
				fr.SetColorIndex(x, y, 1)
			}
		}
		fr.SetColorIndex(1, 1, 2)
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 8) // 12.5 fps
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// opaqueGIF builds a 12-frame 40x30 GIF with a moving red square on a solid
// blue background and no transparent index at all.
func opaqueGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{30, 30, 220, 255}, color.RGBA{220, 30, 30, 255}, color.RGBA{240, 240, 240, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < 12; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, 40, 30), pal)
		for y := 5; y < 25; y++ {
			for x := i*2 + 2; x < i*2+14; x++ {
				fr.SetColorIndex(x, y, 1)
			}
		}
		fr.SetColorIndex(1, 1, 2)
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 8)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// runJob submits r and waits for the terminal event.
func runJob(t *testing.T, m *Manager, r recipe.Recipe) Job {
	t.Helper()
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	ch, unsub, _ := m.Subscribe(j.ID)
	defer unsub()
	evs := drain(t, ch)
	return evs[len(evs)-1].Job
}

// colourClass buckets a pixel as "red", "blue" or "other".
func colourClass(c color.Color) string {
	r, _, b, _ := c.RGBA()
	switch {
	case r > 0x8000 && b < 0x8000:
		return "red"
	case b > 0x8000 && r < 0x8000:
		return "blue"
	}
	return "other"
}

// TestRenderRotatedSource: a portrait phone-style MP4 (coded 160x90 with a
// 90 degree Display Matrix, as ffmpeg's -display_rotation writes it) must
// probe as the displayed 90x160 and every consumer must agree with that —
// the still is 90x160, the master feeds the encoders with the right stride
// (rows stay intact), a crop that is valid on the displayed size renders.
func TestRenderRotatedSource(t *testing.T) {
	tools := realTools(t)
	if v := ffmpegMajor(t, tools); v > 0 && v < 6 {
		t.Skipf("-display_rotation needs FFmpeg >= 6 (have %d)", v)
	}
	st := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Left half red, right half blue (the blue half blinks every 0.2 s so
	// the clip is not static: gifsicle -O2 would merge identical frames);
	// after the display rotation the halves are stacked vertically in a
	// 90x160 frame.
	src := filepath.Join(t.TempDir(), "portrait.mp4")
	cmd := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-noautorotate", "-display_rotation", "90",
		"-f", "lavfi", "-i", "color=c=red:s=160x90:r=10:d=1,drawbox=x=80:y=0:w=80:h=90:c=blue:t=fill:enable='lt(mod(t\\,0.4)\\,0.2)'",
		"-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build a rotated mp4 with this ffmpeg: %v\n%s", err, out)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := st.PutBlob(bytes.NewReader(data), "portrait.mp4")
	if err != nil {
		t.Fatal(err)
	}
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Width != 90 || info.Height != 160 || info.Kind != recipe.KindVideo {
		t.Fatalf("probe must report the displayed size 90x160: %+v", info)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, tools, Options{Concurrency: 1})

	// Still preview: the PNG has the plan's (= probe's) size.
	still, err := m.Still(ctx, blob.Hash, nil, recipe.Output{Format: "gif"}, 0.3, 0)
	if err != nil {
		t.Fatalf("Still: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(still))
	if err != nil {
		t.Fatalf("still is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 90 || b.Dy() != 160 {
		t.Errorf("still bounds = %dx%d, want 90x160", b.Dx(), b.Dy())
	}

	// GIF at source size.
	r := recipe.Recipe{Sources: []string{blob.Hash}, Ops: []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}}, Output: recipe.Output{Format: "gif", Target: "attachment"}}
	fin := runJob(t, m, r)
	if fin.State != StateDone {
		t.Fatalf("job failed: %s (stage %s)", fin.Error, fin.Stage)
	}
	f := fin.Result.Files[0]
	if f.Width != 90 || f.Height != 160 || f.Frames < 2 || f.Report == nil || !f.Report.OK {
		t.Errorf("gif facts = %+v", f)
	}
	gdata, err := os.ReadFile(filepath.Join(st.ResultDir(ResultKey(r)), f.Name))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(gdata))
	if err != nil {
		t.Fatalf("decode gif: %v", err)
	}
	if g.Config.Width != 90 || g.Config.Height != 160 || len(g.Image) < 2 {
		t.Fatalf("gif canvas = %dx%d, %d frames; want 90x160", g.Config.Width, g.Config.Height, len(g.Image))
	}
	fr := g.Image[0]
	if b := fr.Bounds(); b.Dx() != 90 || b.Dy() != 160 {
		t.Fatalf("gif frame 0 bounds = %v, want 90x160", b)
	}
	// Rows must be uniform (a wrong stride would interleave the halves) and
	// the two halves must differ; skip the band around the boundary where
	// chroma subsampling and dithering blend the colours.
	for y := 0; y < 160; y++ {
		if y >= 70 && y < 90 {
			continue
		}
		want := colourClass(fr.At(0, y))
		for x := 1; x < 90; x++ {
			if got := colourClass(fr.At(x, y)); got != want {
				t.Fatalf("row %d is not uniform: x=0 %s, x=%d %s (stride scrambled?)", y, want, x, got)
			}
		}
	}
	top, bottom := colourClass(fr.At(45, 10)), colourClass(fr.At(45, 150))
	if top == bottom || top == "other" || bottom == "other" {
		t.Errorf("halves: top %s, bottom %s; want one red, one blue", top, bottom)
	}

	// A crop that only makes sense on the displayed size compiles and renders.
	rc := recipe.Recipe{Sources: []string{blob.Hash},
		Ops:    []recipe.Op{{Kind: recipe.OpCrop, Params: json.RawMessage(`{"x":0,"y":0,"w":90,"h":120}`)}, {Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}},
		Output: recipe.Output{Format: "gif"}}
	fin = runJob(t, m, rc)
	if fin.State != StateDone {
		t.Fatalf("crop job failed: %s (stage %s)", fin.Error, fin.Stage)
	}
	if fc := fin.Result.Files[0]; fc.Width != 90 || fc.Height != 120 {
		t.Errorf("cropped gif = %dx%d, want 90x120", fc.Width, fc.Height)
	}
}

// TestRenderOnSmallScratch is opt-in: set EZLG_TEST_SCRATCH to a directory
// on a real, small tmpfs — e.g.
//
//	docker run --rm --shm-size=300m -e EZLG_TEST_SCRATCH=/dev/shm/ezl-test … go test -run SmallScratch ./internal/jobs/
//
// It sizes two renders so that each fits the tmpfs alone but not together
// (the reproduced "each fits, both fail" ENOSPC case): both must succeed,
// the second after waiting for the first, and a render larger than the
// whole tmpfs must be refused up-front.
func TestRenderOnSmallScratch(t *testing.T) {
	scratchEnv := os.Getenv("EZLG_TEST_SCRATCH")
	if scratchEnv == "" {
		t.Skip("EZLG_TEST_SCRATCH not set")
	}
	tools := realTools(t)
	root := t.TempDir()
	st, err := store.New(filepath.Join(root, "data"), scratchEnv)
	if err != nil {
		t.Fatal(err)
	}
	total, ok := st.ScratchTotal()
	if !ok || total <= 0 {
		t.Skipf("scratch size unknown for %s", st.Scratch)
	}
	if total > 1<<30 {
		t.Skipf("scratch %s is %s; this test wants a small tmpfs (<= 1 GiB)", st.Scratch, humanBytes(total))
	}
	m := NewManager(st, tools, Options{Concurrency: 2})
	if m.ScratchBudgetBytes() != total {
		t.Fatalf("budget = %d, want the tmpfs size %d", m.ScratchBudgetBytes(), total)
	}
	// One master ≈ 53% of the tmpfs (reserve ≈ 60%): two cannot coexist.
	const side = 320
	frameBytes := int64(side * side * 4)
	frames := int(total * 53 / 100 / frameBytes)
	if frames < 4 {
		t.Fatalf("tmpfs too small for the test: %d frames", frames)
	}
	const fps = 25
	dur := float64(frames) / fps
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	src := filepath.Join(root, "clip.mp4")
	cmd := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%d:duration=%.3f", side, side, fps, dur),
		"-c:v", "mpeg4", "-q:v", "4", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build clip: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(src)
	blob, err := st.PutBlob(bytes.NewReader(data), "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	t.Logf("tmpfs %s: %d frames of %dx%d = %s per master (reserve %s)", humanBytes(total), frames, side, side,
		humanBytes(int64(frames)*frameBytes), humanBytes(scratchReserve(int64(frames)*frameBytes)))

	// Two different recipes (so neither is served from the other's cache)
	// submitted together.
	recipes := []recipe.Recipe{
		{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Lossy: 20}},
		{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "webp", Quality: 60}},
	}
	type outcome struct {
		job    Job
		waited bool
	}
	results := make(chan outcome, len(recipes))
	for _, r := range recipes {
		j, err := m.Submit(r)
		if err != nil {
			t.Fatal(err)
		}
		ch, unsub, _ := m.Subscribe(j.ID)
		go func() {
			defer unsub()
			waited := false
			var last Event
			for ev := range ch {
				if strings.Contains(ev.Job.Message, "waiting for scratch space") {
					waited = true
				}
				last = ev
			}
			results <- outcome{last.Job, waited}
		}()
	}
	waited := 0
	for range recipes {
		select {
		case o := <-results:
			if o.job.State != StateDone {
				t.Errorf("job %s failed: %s (stage %s)", o.job.ID, o.job.Error, o.job.Stage)
			} else {
				t.Logf("job %s done in %d ms (waited=%v)", o.job.ID, o.job.Result.RenderMS, o.waited)
			}
			if o.waited {
				waited++
			}
		case <-ctx.Done():
			t.Fatal("timed out")
		}
	}
	if waited != 1 {
		t.Errorf("%d jobs waited for scratch space, want exactly 1", waited)
	}
	if m.scratch.Used() != 0 {
		t.Errorf("budget leaked: %d", m.scratch.Used())
	}

	// A render larger than the whole tmpfs (or the master cap) is refused
	// before ffmpeg starts.
	big := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Width: side * 2, Height: side * 2, Fit: "exact"}}
	fin := runJob(t, m, big)
	if fin.State != StateError || !strings.Contains(fin.Error, "frame master would need") || strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("oversized render: %+v", fin)
	}
	t.Logf("oversized render refused: %s", fin.Error)
}

// TestRenderOpaqueSourceReportsNoAlpha: an opaque source renders to files
// whose report says hasAlpha=false, whatever transparency the frame-diff
// optimised encoders used structurally (the linter's flag); when they
// disagree the structural verdict is kept in a render.alpha info check.
func TestRenderOpaqueSourceReportsNoAlpha(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(opaqueGIF(t)), "opaque.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.HasAlpha || info.Kind != recipe.KindAnimation {
		t.Fatalf("opaque gif probe: %+v", info)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, tools, Options{Concurrency: 2})
	for _, out := range []recipe.Output{
		{Format: "gif", Target: "attachment", Lossy: 30},
		{Format: "gif"},
		{Format: "webp", Quality: 70, Target: "attachment"},
		{Format: "webp", Lossless: true},
	} {
		t.Run(out.Format+"-"+string(out.Target)+fmt.Sprintf("-lossless=%v", out.Lossless), func(t *testing.T) {
			r := recipe.Recipe{Sources: []string{blob.Hash}, Ops: []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}}, Output: out}
			fin := runJob(t, m, r)
			if fin.State != StateDone {
				if out.Format == "webp" && fin.Stage == StageVerify && ffmpegMajor(t, tools) < 9 {
					t.Skipf("animated WebP needs FFmpeg 9 to decode: %s", fin.Error)
				}
				t.Fatalf("job failed: %s (stage %s)", fin.Error, fin.Stage)
			}
			f := fin.Result.Files[0]
			if f.Report == nil {
				t.Fatal("no report")
			}
			if f.Report.HasAlpha {
				t.Errorf("opaque source reported hasAlpha=true: %+v", f.Report)
			}
			var structural *discordlint.Check
			for i := range f.Report.Checks {
				if f.Report.Checks[i].Rule == RuleRenderAlpha {
					structural = &f.Report.Checks[i]
				}
			}
			if structural != nil {
				t.Logf("%s: linter's structural flag was true; kept as %s (%s)", f.Name, structural.Rule, structural.Detail)
				if structural.Level != discordlint.LevelInfo || !structural.OK {
					t.Errorf("render.alpha must be an OK info check: %+v", structural)
				}
			}
			// report.json on disk agrees with the manifest.
			raw, err := os.ReadFile(filepath.Join(st.ResultDir(ResultKey(r)), "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var onDisk discordlint.Report
			if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk.HasAlpha {
				t.Errorf("report.json: hasAlpha=%v (%v)", onDisk.HasAlpha, err)
			}
			if !f.Report.OK {
				t.Errorf("report not OK: %+v", f.Report.Checks)
			}
		})
	}
}

// TestRenderGIFLoopCount: Output.Loop reaches the file. With no Discord
// target a loop count N is kept through ffmpeg (-loop N), gifsicle
// (--loopcount=N, restated by jobs; without it gifsicle would write
// forever) and the linter (which only asks for the NETSCAPE block to exist);
// a Discord target always ends up looping forever.
func TestRenderGIFLoopCount(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, tools, Options{Concurrency: 1})
	t.Logf("gifsicle: %q", tools.Gifsicle)
	loopOf := func(t *testing.T, r recipe.Recipe) int {
		t.Helper()
		fin := runJob(t, m, r)
		if fin.State != StateDone {
			t.Fatalf("job failed: %s (stage %s)", fin.Error, fin.Stage)
		}
		data, err := os.ReadFile(filepath.Join(st.ResultDir(ResultKey(r)), fin.Result.Files[0].Name))
		if err != nil {
			t.Fatal(err)
		}
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(g.Image) < 2 {
			t.Fatalf("%d frames", len(g.Image))
		}
		if fin.Result.Files[0].Report.LoopForever != (g.LoopCount == 0) {
			t.Errorf("report.loopForever=%v but NETSCAPE count is %d", fin.Result.Files[0].Report.LoopForever, g.LoopCount)
		}
		return g.LoopCount
	}
	ops := []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}}
	if n := loopOf(t, recipe.Recipe{Sources: []string{blob.Hash}, Ops: ops, Output: recipe.Output{Format: "gif", Loop: 3}}); n != 3 {
		t.Errorf("custom loop 3 → NETSCAPE count %d", n)
	}
	if n := loopOf(t, recipe.Recipe{Sources: []string{blob.Hash}, Ops: ops, Output: recipe.Output{Format: "gif"}}); n != 0 {
		t.Errorf("default loop → NETSCAPE count %d, want 0 (forever)", n)
	}
	if n := loopOf(t, recipe.Recipe{Sources: []string{blob.Hash}, Ops: ops, Output: recipe.Output{Format: "gif", Target: "attachment", Loop: 3}}); n != 0 {
		t.Errorf("Discord target with loop 3 → NETSCAPE count %d, want 0 (forever)", n)
	}
	// Lossy triggers the gifsicle pass when it is available.
	if n := loopOf(t, recipe.Recipe{Sources: []string{blob.Hash}, Ops: ops, Output: recipe.Output{Format: "gif", Loop: 2, Lossy: 40, Colors: 32}}); n != 2 {
		t.Errorf("custom loop 2 with lossy/colors → NETSCAPE count %d", n)
	}
}

// TestRenderOneFrameMOV: a one-frame MOV (an NLE "export frame") probes as
// a still and renders like one — the master skips the fps filter and yields
// exactly one frame, the still preview answers any t, and the outputs are
// single-frame files.
func TestRenderOneFrameMOV(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src := filepath.Join(t.TempDir(), "frame.mov")
	cmd := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=64x48:r=25:d=1,drawbox=x=32:y=0:w=32:h=48:c=blue:t=fill",
		"-frames:v", "1", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build a one-frame mov: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(src)
	blob, err := st.PutBlob(bytes.NewReader(data), "frame.mov")
	if err != nil {
		t.Fatal(err)
	}
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !info.IsStill || info.Kind != recipe.KindImage || info.Frames != 1 || info.FPS != 0 {
		t.Fatalf("one-frame mov must probe as a still: %+v", info)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, tools, Options{Concurrency: 1})

	// Still preview at any t.
	for _, tt := range []float64{0, 0.5, 7} {
		png1, err := m.Still(ctx, blob.Hash, nil, recipe.Output{Format: "gif"}, tt, 0)
		if err != nil {
			t.Fatalf("Still t=%v: %v", tt, err)
		}
		img, err := png.Decode(bytes.NewReader(png1))
		if err != nil {
			t.Fatalf("still t=%v is not a PNG: %v", tt, err)
		}
		if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 48 {
			t.Errorf("still t=%v bounds = %dx%d", tt, b.Dx(), b.Dy())
		}
		if l, r := colourClass(img.At(10, 24)), colourClass(img.At(54, 24)); l != "red" || r != "blue" {
			t.Errorf("still t=%v colours: left %s right %s", tt, l, r)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(st.Scratch, stillsDir))
	if len(entries) != 1 {
		t.Errorf("stills memo has %d entries, want 1 (every t maps to the same frame)", len(entries))
	}

	// GIF and WebP renders: one frame each (same aspect as the source, so no
	// transparent padding enters the master).
	for _, out := range []recipe.Output{{Format: "gif", Target: "attachment", Width: 32, Height: 24}, {Format: "webp", Width: 32, Height: 24}} {
		fin := runJob(t, m, recipe.Recipe{Sources: []string{blob.Hash}, Output: out})
		if fin.State != StateDone {
			t.Fatalf("%s job failed: %s (stage %s)", out.Format, fin.Error, fin.Stage)
		}
		f := fin.Result.Files[0]
		if f.Frames != 1 || f.Report == nil || f.Report.Frames != 1 || !f.Report.OK {
			t.Errorf("%s: %+v", out.Format, f)
		}
		if f.Report.HasAlpha {
			t.Errorf("%s: opaque still reported alpha", out.Format)
		}
	}
	// A square emote from the 4:3 still pads with transparency: the master
	// scan (not the structural flag) says so.
	fin := runJob(t, m, recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Target: "emote", Width: 32, Height: 32}})
	if fin.State != StateDone {
		t.Fatalf("emote job failed: %s (stage %s)", fin.Error, fin.Stage)
	}
	if f := fin.Result.Files[0]; f.Frames != 1 || f.Width != 32 || f.Height != 32 || f.Report == nil || !f.Report.HasAlpha {
		t.Errorf("padded emote: %+v", f)
	}
}

func TestRenderEndToEnd(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	blob, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := probe.Probe(ctx, tools, blob.Path, 0)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Kind != recipe.KindAnimation || info.Width != 40 || info.Height != 30 || !info.HasAlpha {
		t.Fatalf("probe info: %+v", info)
	}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}

	m := NewManager(st, tools, Options{Concurrency: 2})
	for _, tc := range []struct {
		name string
		out  recipe.Output
	}{
		{"gif-emote", recipe.Output{Format: "gif", Width: 32, Height: 32, Target: "emote", Lossy: 30}},
		{"webp", recipe.Output{Format: "webp", Width: 40, Height: 30, Quality: 70, Target: "attachment"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := recipe.Recipe{Sources: []string{blob.Hash}, Ops: []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}}, Output: tc.out}
			j, err := m.Submit(r)
			if err != nil {
				t.Fatal(err)
			}
			ch, unsub, _ := m.Subscribe(j.ID)
			defer unsub()
			evs := drain(t, ch)
			last := evs[len(evs)-1]
			if last.Type != EventDone {
				if tc.out.Format == "webp" && last.Job.Stage == StageVerify && ffmpegMajor(t, tools) < 9 {
					t.Skipf("animated WebP needs FFmpeg 9 to decode (have %d): %s", ffmpegMajor(t, tools), last.Job.Error)
				}
				t.Fatalf("job failed: %s (stage %s)", last.Job.Error, last.Job.Stage)
			}
			// Stages progressed in order and percent never went backwards.
			prevPct := -1.0
			seen := map[string]bool{}
			for _, ev := range evs {
				if ev.Job.Percent < prevPct {
					t.Errorf("percent went backwards: %v → %v", prevPct, ev.Job.Percent)
				}
				prevPct = ev.Job.Percent
				seen[ev.Job.Stage] = true
			}
			for _, stg := range []string{StageMaster, StageEncode, StageLint, StageVerify, StageDone} {
				if !seen[stg] {
					t.Errorf("stage %q never reported", stg)
				}
			}
			res := last.Job.Result
			if res == nil || len(res.Files) != 1 {
				t.Fatalf("result: %+v", res)
			}
			f := res.Files[0]
			t.Logf("%s: %d bytes %dx%d %d frames %.2f fps %.2fs limit %d report.ok=%v checks=%d (render %d ms)",
				f.Name, f.Bytes, f.Width, f.Height, f.Frames, f.FPS, f.Duration, f.Limit, f.Report != nil && f.Report.OK, len(f.Report.Checks), res.RenderMS)
			if f.Name != "out."+tc.out.Format || f.URL != "/out/"+ResultKey(r)+"/"+f.Name || f.Format != tc.out.Format {
				t.Errorf("file = %+v", f)
			}
			if f.Width != tc.out.Width || f.Height != tc.out.Height {
				t.Errorf("dims = %dx%d, want %dx%d", f.Width, f.Height, tc.out.Width, tc.out.Height)
			}
			if f.Frames < 2 || f.FPS <= 0 || f.Duration <= 0 || f.Bytes <= 0 {
				t.Errorf("file facts = %+v", f)
			}
			if f.Report == nil || f.Report.Format != tc.out.Format || !f.Report.OK {
				t.Errorf("report = %+v", f.Report)
			}
			if f.Limit == 0 {
				t.Error("limit not set for a Discord target")
			}
			if res.Cached || res.RenderMS <= 0 || res.RecipeHash != ResultKey(r) {
				t.Errorf("manifest = %+v", res)
			}
			// On disk: file, report.json, manifest.json.
			dir := st.ResultDir(ResultKey(r))
			data, err := os.ReadFile(filepath.Join(dir, f.Name))
			if err != nil || int64(len(data)) != f.Bytes {
				t.Errorf("result file: %v (%d bytes, want %d)", err, len(data), f.Bytes)
			}
			if _, err := os.Stat(filepath.Join(dir, "report.json")); err != nil {
				t.Errorf("report.json missing: %v", err)
			}
			loaded, err := m.LoadResult(ResultKey(r))
			if err != nil || loaded.Files[0].Bytes != f.Bytes {
				t.Errorf("LoadResult: %+v %v", loaded, err)
			}
			// Second submit is served from cache.
			j2, err := m.Submit(r)
			if err != nil || j2.State != StateDone || j2.Result == nil || !j2.Result.Cached {
				t.Errorf("cached resubmit: %+v %v", j2, err)
			}
			// Scratch cleaned.
			if _, err := os.Stat(filepath.Join(st.Scratch, j.ID)); !os.IsNotExist(err) {
				t.Errorf("scratch dir left behind: %v", err)
			}
		})
	}

	// Still frames: rendered PNG, memoised on the second call.
	png1, err := m.Still(ctx, blob.Hash, nil, recipe.Output{Format: "gif", Width: 32, Height: 32}, 0.3, 64)
	if err != nil {
		t.Fatalf("Still: %v", err)
	}
	if !bytes.HasPrefix(png1, []byte("\x89PNG")) {
		t.Errorf("still is not a PNG: %q", png1[:min(len(png1), 8)])
	}
	entries, _ := os.ReadDir(filepath.Join(st.Scratch, stillsDir))
	if len(entries) != 1 {
		t.Errorf("stills memo has %d entries, want 1", len(entries))
	}
	png2, err := m.Still(ctx, blob.Hash, nil, recipe.Output{Format: "gif", Width: 32, Height: 32}, 0.3, 64)
	if err != nil || !bytes.Equal(png1, png2) {
		t.Errorf("memoised still differs: %v", err)
	}
	// Bad recipe through Still is a client error.
	if _, err := m.Still(ctx, blob.Hash, []recipe.Op{{Kind: recipe.OpCrop, Params: json.RawMessage(`{"x":0,"y":0,"w":0,"h":0}`)}}, recipe.Output{}, 0, 0); err == nil {
		t.Error("zero crop accepted")
	}
	if _, err := os.Stat(filepath.Join(st.ResultDir(ResultKey(recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Width: 32, Height: 32, Target: "emote", Lossy: 30}, Ops: []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}}})), store.ManifestName)); err != nil {
		t.Errorf("gif manifest missing on disk: %v", err)
	}
}
