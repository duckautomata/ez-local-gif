package jobs

// Tool-free tests of the held-frame wiring (gif.noop-frame-disposal): they
// run everywhere `go test` runs — GitHub CI has neither ffmpeg nor gifsicle —
// by letting this test binary stand in for gifsicle (and ffmpeg) through the
// TestMain re-exec harness of preview_test.go. The real-tools counterparts
// are TestRenderGIFWithHolds / TestLintGIFRepairsUnsafeHolds /
// TestRenderFitRepairsLossyHolds in render_e2e_test.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/fit"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

const (
	// fakeGifsicleEnv switches the test binary into fake-gifsicle mode; its
	// value is the control directory (see runFakeGifsicle).
	fakeGifsicleEnv = "EZLG_JOBS_TEST_FAKE_GIFSICLE"
	// fakeOutEnv names the file the fake ffmpeg writes as its encode output.
	fakeOutEnv = "EZLG_JOBS_TEST_FAKE_OUT"
)

// isGifsicleArgv recognises a gifsicle command line of enc (every builder
// restates --loopcount and ends with "-o out"); ffmpeg's never match.
func isGifsicleArgv(args []string) bool {
	if len(args) < 3 || args[len(args)-2] != "-o" {
		return false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--loopcount=") {
			return true
		}
	}
	return false
}

// gifLeadInFrame is the transparent 1x1 lead-in frame of the hold repair
// (discordlint.PrependTransparentFrame): GCE (disposal 2, transparent index 0,
// no delay) + image descriptor + one pixel of LZW data.
var gifLeadInFrame = []byte{0x21, 0xF9, 0x04, 0x09, 0, 0, 0, 0, 0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0x02, 0x02, 0x44, 0x01, 0x00}

// gifsicleInputArg is the input file of an enc gifsicle argv: the argument
// before "-o out", or before the frame selection that follows it.
func gifsicleInputArg(args []string) string {
	i := len(args) - 3
	if i > 0 && strings.HasPrefix(args[i], "#") {
		i--
	}
	return args[i]
}

// fakeGifsicleStep names the pass an argv is: "U" (coalesce), "colors" (the
// ladder's palette rung) or "O" (any other optimise pass).
func fakeGifsicleStep(args []string) string {
	switch {
	case hasArg(args, "-U"):
		return "U"
	case hasArg(args, "--colors"):
		return "colors"
	}
	return "O"
}

// runFakeGifsicle is the fake tool. It records call N (in start order) as
// <dir>/call-N.args (one argument per line) plus a copy of its input file,
// call-N.in.gif, and then answers per control file of its step S:
//
//	S.fail  exit 1
//	S.hang  never finish (the test cancels the context)
//	S.gif   these bytes are the output
//	(none)  the input is copied to the output (without its first frame when
//	        the argv has the frame selection "#1-")
//
// It also stands in for gifski (tools.Gifski = the same binary): an argv with
// --quality and -o gets <dir>/gifski.gif as its output.
func runFakeGifsicle(dir string) int {
	args := os.Args[1:]
	if !isGifsicleArgv(args) {
		if i := slices.Index(args, "-o"); i >= 0 && i+1 < len(args) && hasArg(args, "--quality") {
			canned, err := os.ReadFile(filepath.Join(dir, "gifski.gif"))
			if err == nil {
				err = os.WriteFile(args[i+1], canned, 0o644)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "fake gifski:", err)
				return 1
			}
			return 0
		}
		fmt.Println("LCDF Gifsicle fake-ezlg-jobs-test")
		return 0
	}
	in, out := gifsicleInputArg(args), args[len(args)-1]
	data, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake gifsicle: input:", err)
		return 1
	}
	// The recorded input is the file as handed over; what is copied to the
	// output honours the frame selection "#1-", which drops the first frame —
	// the transparent lead-in frame of the hold repair, its only use.
	result := data
	if hasArg(args, "#1-") {
		result = bytes.Replace(data, gifLeadInFrame, nil, 1)
		if len(result) == len(data) {
			fmt.Fprintln(os.Stderr, "fake gifsicle: \"#1-\" without the lead-in frame in the input")
			return 1
		}
	}
	calls, _ := filepath.Glob(filepath.Join(dir, "call-*.args"))
	n := len(calls)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("call-%d.in.gif", n)), data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake gifsicle: record:", err)
		return 1
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".call-%d", n))
	if err := os.WriteFile(tmp, []byte(strings.Join(args, "\n")), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake gifsicle: record:", err)
		return 1
	}
	if err := os.Rename(tmp, filepath.Join(dir, fmt.Sprintf("call-%d.args", n))); err != nil {
		fmt.Fprintln(os.Stderr, "fake gifsicle: record:", err)
		return 1
	}
	step := fakeGifsicleStep(args)
	if _, err := os.Stat(filepath.Join(dir, step+".fail")); err == nil {
		fmt.Fprintln(os.Stderr, "fake gifsicle: told to fail")
		return 1
	}
	if _, err := os.Stat(filepath.Join(dir, step+".hang")); err == nil {
		time.Sleep(fakeToolMax)
		return 1
	}
	if canned, err := os.ReadFile(filepath.Join(dir, step+".gif")); err == nil {
		result = canned
	}
	if err := os.WriteFile(out, result, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake gifsicle: output:", err)
		return 1
	}
	return 0
}

// fakeGifsicle switches this test binary into fake-gifsicle mode for the rest
// of the test. ctl maps control file names ("U.gif", "O.fail", …) to their
// bytes. It returns the tools and the control directory.
func fakeGifsicle(t *testing.T, ctl map[string][]byte) (ffrun.Tools, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, data := range ctl {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeGifsicleEnv, dir)
	return ffrun.Tools{Gifsicle: exe}, dir
}

// fakeGifsicleCalls returns the argv of every recorded call, in order.
func fakeGifsicleCalls(t *testing.T, dir string) [][]string {
	t.Helper()
	var calls [][]string
	for n := 0; ; n++ {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("call-%d.args", n)))
		if err != nil {
			return calls
		}
		calls = append(calls, strings.Split(string(data), "\n"))
	}
}

// fakeGifsicleInput decodes the input file call n was given.
func fakeGifsicleInput(t *testing.T, dir string, n int) *gif.GIF {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("call-%d.in.gif", n)))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("input of call %d: %v", n, err)
	}
	return g
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// assertRepairArgv pins the two passes of the hold repair: A coalesces with
// --disposal=background and no optimiser, nothing lossy; B is -O2 --careful
// with the caller's lossy; both restate the loop count.
func assertRepairArgv(t *testing.T, a, b []string, lossy, loop string) {
	t.Helper()
	if !hasArg(a, "-U") || !hasArg(a, "--disposal=background") || hasArgPrefix(a, "-O") || hasArgPrefix(a, "--lossy") || hasArg(a, "--colors") || !hasArg(a, loop) {
		t.Errorf("step A argv = %q", a)
	}
	if hasArg(b, "-U") || hasArg(b, "--disposal=background") || !hasArg(b, "-O2") || !hasArg(b, "--careful") || hasArg(b, "--colors") || !hasArg(b, loop) {
		t.Errorf("step B argv = %q", b)
	}
	if lossy == "" {
		if hasArgPrefix(b, "--lossy") {
			t.Errorf("step B argv = %q, want no --lossy", b)
		}
	} else if !hasArg(b, lossy) {
		t.Errorf("step B argv = %q, want %s", b, lossy)
	}
}

// coalescedHoldsGIF is what "gifsicle -U --disposal=background" plus the merge
// make of unsafeHoldsGIF: one full-canvas disposal-2 frame per pose.
func coalescedHoldsGIF(t *testing.T) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: 0, Config: image.Config{Width: 64, Height: 48, ColorModel: holdsPalette}}
	for _, pose := range holdPoses {
		fr := image.NewPaletted(image.Rect(0, 0, 64, 48), holdsPalette)
		fillRect(fr, pose, 1)
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 100)
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	return encodeTestGIF(t, g)
}

// unsafeHoldsLocalPaletteGIF is unsafeHoldsGIF with a local colour table on
// its last frame: holds AND gif.global-palette fail for a Discord target, so
// the ladder's --colors rung is worth running.
func unsafeHoldsLocalPaletteGIF(t *testing.T) []byte {
	t.Helper()
	g, err := gif.DecodeAll(bytes.NewReader(unsafeHoldsGIF(t)))
	if err != nil {
		t.Fatal(err)
	}
	last := g.Image[len(g.Image)-1]
	last.Palette = color.Palette{color.RGBA{0, 0, 0, 0}, color.RGBA{10, 200, 10, 255}}
	return encodeTestGIF(t, g)
}

// assertCoalesced checks that data is the coalesced file (step A's), not a
// re-optimised one: every frame full-canvas with disposal 2.
func assertCoalesced(t *testing.T, data []byte) {
	t.Helper()
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Image) != len(holdPoses) {
		t.Errorf("%d frames, want %d", len(g.Image), len(holdPoses))
	}
	full := image.Rect(0, 0, g.Config.Width, g.Config.Height)
	for k, fr := range g.Image {
		if fr.Bounds() != full || g.Disposal[k] != gif.DisposalBackground {
			t.Errorf("frame %d: bounds %v disposal %d — not the coalesced file", k, fr.Bounds(), g.Disposal[k])
		}
	}
}

func assertNoHoldScratch(t *testing.T, scratch string) {
	t.Helper()
	for _, pat := range []string{"holds*", "ladder-*"} {
		if left, _ := filepath.Glob(filepath.Join(scratch, pat)); len(left) != 0 {
			t.Errorf("scratch files left behind: %v", left)
		}
	}
}

func lintTestJob() *job {
	return &job{snap: Job{ID: "holds", State: StateRunning, Stage: StageLint}, cancel: func() {}, subs: map[int]*subscriber{}}
}

// TestMergeHoldsInFile: the on-disk merge folds the 75-frame, 3-pose fixture
// into 3 frames of 1 s, and is a byte-exact no-op on anything it cannot read
// or parse.
func TestMergeHoldsInFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base.gif")
	if err := os.WriteFile(path, holdsGIF(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mergeHoldsInFile(path); got != 72 {
		t.Errorf("mergeHoldsInFile = %d, want 72", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(g.Delay) != "[100 100 100]" {
		t.Errorf("delays after the merge = %v, want [100 100 100]", g.Delay)
	}
	if n := assertOnePosePerFrame(t, data); n != 3 {
		t.Errorf("%d frames, want 3", n)
	}
	if got := mergeHoldsInFile(path); got != 0 {
		t.Errorf("second merge = %d, want 0", got)
	}

	garbage := []byte("GIF89a this is not a gif")
	bad := filepath.Join(dir, "bad.gif")
	if err := os.WriteFile(bad, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mergeHoldsInFile(bad); got != 0 {
		t.Errorf("garbage: merged %d", got)
	}
	if after, _ := os.ReadFile(bad); !bytes.Equal(after, garbage) {
		t.Errorf("garbage file was rewritten: %q", after)
	}
	if got := mergeHoldsInFile(filepath.Join(dir, "missing.gif")); got != 0 {
		t.Errorf("missing file: merged %d", got)
	}
}

// TestEncodeGIFAtMergesBeforeGifsicle pins the prevention: between ffmpeg and
// gifsicle the held frames are merged, so the optimiser never sees a hold run
// (and without gifsicle the merged base is the output).
func TestEncodeGIFAtMergesBeforeGifsicle(t *testing.T) {
	master := enc.Master{Path: "frames.rgba", Width: 64, Height: 48, FPS: 25, Frames: 75, HasAlpha: true}
	payload := filepath.Join(t.TempDir(), "ffmpeg-out.gif")
	if err := os.WriteFile(payload, holdsGIF(t), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, withGifsicle := range []bool{false, true} {
		t.Run(fmt.Sprintf("gifsicle=%v", withGifsicle), func(t *testing.T) {
			tools, dir := fakeFFmpegTools(t)
			releaseFakes(t, dir)
			t.Setenv(fakeOutEnv, payload)
			gdir := ""
			if withGifsicle {
				var gt ffrun.Tools
				gt, gdir = fakeGifsicle(t, nil)
				tools.Gifsicle = gt.Gifsicle
			}
			m := &Manager{tools: tools}
			scratch := t.TempDir()
			path, err := m.encodeGIFAt(ctx, nil, scratch, "-c1", master, enc.GIFOptions{}, enc.GifsicleOptions{Lossy: 40})
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if n := assertOnePosePerFrame(t, data); n != 3 {
				t.Errorf("%s has %d frames, want the 3 merged poses", filepath.Base(path), n)
			}
			if !withGifsicle {
				return
			}
			calls := fakeGifsicleCalls(t, gdir)
			if len(calls) != 1 || !hasArg(calls[0], "--lossy=40") {
				t.Fatalf("gifsicle calls = %q", calls)
			}
			if g := fakeGifsicleInput(t, gdir, 0); len(g.Image) != 3 {
				t.Errorf("gifsicle was handed %d frames: the held frames were not merged first", len(g.Image))
			}
		})
	}
}

// TestRepairGIFHoldsOrchestration drives repairGIFHolds with a fake gifsicle:
// the two passes and their order, the merge between them, the fallback to
// the coalesced file, the error when step A fails, cancellation, and a clean
// scratch directory throughout.
func TestRepairGIFHoldsOrchestration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	unsafe, flat := unsafeHoldsGIF(t), coalescedHoldsGIF(t)

	t.Run("merge between the passes", func(t *testing.T) {
		// The fake copies its input: step A "coalesces" the 75-frame fixture
		// to itself, so step B must be handed the merged 3 frames.
		tools, dir := fakeGifsicle(t, nil)
		m := &Manager{tools: tools}
		scratch := t.TempDir()
		var steps []string
		out, rep, err := m.repairGIFHolds(ctx, scratch, "-c7", holdsGIF(t), discordlint.TargetAttachment, enc.GifsicleOptions{Lossy: 80, Loop: 2}, func(s string) { steps = append(steps, s) })
		if err != nil {
			t.Fatal(err)
		}
		calls := fakeGifsicleCalls(t, dir)
		if len(calls) != 2 {
			t.Fatalf("%d gifsicle calls, want 2: %q", len(calls), calls)
		}
		assertRepairArgv(t, calls[0], calls[1], "--lossy=80", "--loopcount=2")
		if in := gifsicleInputArg(calls[0]); filepath.Base(in) != "holds-c7-in.gif" {
			t.Errorf("step A input = %s", in)
		}
		if a, b := calls[0][len(calls[0])-1], gifsicleInputArg(calls[1]); a != b || filepath.Base(a) != "holds-c7-flat.gif" {
			t.Errorf("step B reads %s, step A wrote %s", b, a)
		}
		// The clip shows the background, so step A gets the transparent lead-in
		// frame in front of the 75 frames — gifsicle decides from the first
		// frame whether the canvas is transparent at all — and drops it again
		// with the frame selection right after the input.
		g0 := fakeGifsicleInput(t, dir, 0)
		if len(g0.Image) != 76 || g0.Image[0].Rect != image.Rect(0, 0, 1, 1) || g0.Delay[0] != 0 || !isTransparent(g0.Image[0].At(0, 0)) {
			t.Errorf("step A was handed %d frames, first %v delay %d: want the 1x1 transparent lead-in + the 75 of the input", len(g0.Image), g0.Image[0].Rect, g0.Delay[0])
		}
		if i := slices.Index(calls[0], "#1-"); i < 1 || calls[0][i-1] != gifsicleInputArg(calls[0]) || hasArg(calls[1], "#1-") {
			t.Errorf("frame selection: step A %q, step B %q — want \"#1-\" right after step A's input only", calls[0], calls[1])
		}
		if g := fakeGifsicleInput(t, dir, 1); len(g.Image) != 3 {
			t.Errorf("step B was handed %d frames: the coalesced file was not merged", len(g.Image))
		}
		if n := assertOnePosePerFrame(t, out); n != 3 {
			t.Errorf("%d frames, want 3", n)
		}
		if chk := holdsCheck(t, &rep); !chk.OK || hasStructuralError(rep) {
			t.Errorf("report: %+v", rep.Checks)
		}
		if len(steps) != 2 || !strings.Contains(steps[0], "-U") || !strings.Contains(steps[1], "-O2") {
			t.Errorf("announced steps = %q", steps)
		}
		assertNoHoldScratch(t, scratch)
	})

	// Step B's output is only delivered when it is structurally sound;
	// otherwise the coalesced file is. For target none the hold rule is a
	// warning and still counts.
	for _, c := range []struct {
		name   string
		target discordlint.Target
		ctl    map[string][]byte
	}{
		{"B still unsafe", discordlint.TargetAttachment, map[string][]byte{"U.gif": flat, "O.gif": unsafe}},
		{"B still unsafe, target none", discordlint.TargetNone, map[string][]byte{"U.gif": flat, "O.gif": unsafe}},
		{"B exits non-zero", discordlint.TargetAttachment, map[string][]byte{"U.gif": flat, "O.fail": nil}},
		{"B writes garbage", discordlint.TargetAttachment, map[string][]byte{"U.gif": flat, "O.gif": []byte("not a gif")}},
	} {
		t.Run("fallback: "+c.name, func(t *testing.T) {
			tools, dir := fakeGifsicle(t, c.ctl)
			m := &Manager{tools: tools}
			scratch := t.TempDir()
			out, rep, err := m.repairGIFHolds(ctx, scratch, "", unsafe, c.target, enc.GifsicleOptions{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls := fakeGifsicleCalls(t, dir); len(calls) != 2 {
				t.Fatalf("%d gifsicle calls, want 2: %q", len(calls), calls)
			} else {
				assertRepairArgv(t, calls[0], calls[1], "", "--loopcount=forever")
			}
			assertCoalesced(t, out)
			assertOnePosePerFrame(t, out)
			if chk := holdsCheck(t, &rep); !chk.OK || hasStructuralError(rep) {
				t.Errorf("report of the coalesced file: %+v", rep.Checks)
			}
			assertNoHoldScratch(t, scratch)

			// The ladder-less paths deliver the same bytes and say so.
			lint, _, err := discordlint.LintGIF(unsafe, c.target, true)
			if err != nil {
				t.Fatal(err)
			}
			data, got, repaired := m.repairIfOnlyHolds(ctx, scratch, "-x", unsafe, lint, c.target, enc.GifsicleOptions{})
			if !repaired || !bytes.Equal(data, out) || hasStructuralError(got) {
				t.Errorf("repairIfOnlyHolds: repaired=%v, same bytes=%v, report %+v", repaired, bytes.Equal(data, out), got.Checks)
			}
			assertNoHoldScratch(t, scratch)
		})
	}

	t.Run("step A fails", func(t *testing.T) {
		tools, dir := fakeGifsicle(t, map[string][]byte{"U.fail": nil})
		m := &Manager{tools: tools}
		scratch := t.TempDir()
		if _, _, err := m.repairGIFHolds(ctx, scratch, "", unsafe, discordlint.TargetAttachment, enc.GifsicleOptions{}, nil); err == nil {
			t.Error("repairGIFHolds: no error although the coalesce pass failed")
		}
		if calls := fakeGifsicleCalls(t, dir); len(calls) != 1 {
			t.Errorf("gifsicle calls after a failed step A: %q", calls)
		}
		assertNoHoldScratch(t, scratch)
		// The callers keep what they had — for every target.
		for _, target := range []discordlint.Target{discordlint.TargetAttachment, discordlint.TargetNone} {
			lint, _, err := discordlint.LintGIF(unsafe, target, true)
			if err != nil {
				t.Fatal(err)
			}
			data, got, repaired := m.repairIfOnlyHolds(ctx, scratch, "", unsafe, lint, target, enc.GifsicleOptions{})
			if repaired || !bytes.Equal(data, unsafe) || holdsCheck(t, &got).OK {
				t.Errorf("target %q: repairIfOnlyHolds after a failed repair: repaired=%v report=%+v", target, repaired, got.Checks)
			}
			// A warn-level hold failure never makes a candidate not-ok.
			if wantOK := target == discordlint.TargetNone; !hasErrorCheck(got) != wantOK || got.OK != wantOK {
				t.Errorf("target %q: report OK=%v hasErrorCheck=%v", target, got.OK, hasErrorCheck(got))
			}
			data, got, err = m.lintGIF(ctx, lintTestJob(), scratch, unsafe, target, recipe.Output{Format: "gif"})
			if err != nil || !bytes.Equal(data, unsafe) || holdsCheck(t, &got).OK {
				t.Errorf("target %q: lintGIF after a failed repair: err=%v report=%+v", target, err, got.Checks)
			}
		}
		assertNoHoldScratch(t, scratch)
	})

	for _, step := range []string{"U", "O"} {
		t.Run("cancelled during step "+step, func(t *testing.T) {
			tools, dir := fakeGifsicle(t, map[string][]byte{"U.gif": flat, step + ".hang": nil})
			m := &Manager{tools: tools}
			scratch := t.TempDir()
			cctx, ccancel := context.WithCancel(ctx)
			defer ccancel()
			want := map[string]int{"U": 1, "O": 2}[step]
			go func() {
				deadline := time.Now().Add(fakeToolMax)
				for time.Now().Before(deadline) && cctx.Err() == nil {
					if n, _ := filepath.Glob(filepath.Join(dir, "call-*.args")); len(n) >= want {
						ccancel()
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			start := time.Now()
			_, _, err := m.lintGIF(cctx, lintTestJob(), scratch, unsafe, discordlint.TargetAttachment, recipe.Output{Format: "gif"})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("lintGIF error = %v, want context.Canceled", err)
			}
			if d := time.Since(start); d > fakeToolMax/2 {
				t.Errorf("cancellation took %v", d)
			}
			if calls := fakeGifsicleCalls(t, dir); len(calls) != want {
				t.Errorf("%d gifsicle calls, want %d", len(calls), want)
			}
			assertNoHoldScratch(t, scratch)
		})
	}
}

// TestGifLadderTagged pins the ladder as the gifski fit candidates use it
// (fitRun.encode): every scratch file of every rung carries the candidate's
// tag — candidates walk the ladder concurrently in one directory — the
// caller's loop count is restated without any --lossy, and the outcome says
// which rungs replaced the bytes (the hold repair feeds the description).
func TestGifLadderTagged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	flat := coalescedHoldsGIF(t)
	localOnly, err := gif.DecodeAll(bytes.NewReader(flat))
	if err != nil {
		t.Fatal(err)
	}
	// A local colour table (its third entry differs from the global one, so
	// image/gif writes it) that shows the global colours: the picture is the
	// canned flat file's, which the repair checks.
	localOnly.Image[2].Palette = color.Palette{holdsPalette[0], holdsPalette[1], color.RGBA{1, 2, 3, 255}}
	for _, c := range []struct {
		name   string
		src    []byte
		colors int // 0 = the default palette size
		ctl    map[string][]byte
		steps  string
		want   ladderOutcome
	}{
		{"the colours rung is enough", encodeTestGIF(t, localOnly), 64, map[string][]byte{"colors.gif": flat}, "colors", ladderOutcome{replaced: true}},
		{"colours, then the hold repair", unsafeHoldsLocalPaletteGIF(t), 0, map[string][]byte{"U.gif": flat}, "colors U O", ladderOutcome{replaced: true, holds: true}},
		// The repair doubles as the generic re-encode: it rescues a file the
		// colours rung could not, but a file without clear-only frames must
		// not be described as repaired for them.
		{"the repair rescues a file without holds", encodeTestGIF(t, localOnly), 0, map[string][]byte{"colors.fail": nil, "U.gif": flat}, "colors U O", ladderOutcome{replaced: true}},
		{"every rung fails", unsafeHoldsLocalPaletteGIF(t), 0, map[string][]byte{"colors.fail": nil, "U.fail": nil}, "colors U", ladderOutcome{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			const target, tag = discordlint.TargetAttachment, "-0007"
			first, fixed, err := discordlint.LintGIF(c.src, target, true)
			if err != nil {
				t.Fatal(err)
			}
			if !hasStructuralError(first) || !ladderTriesColors(first) {
				t.Fatalf("fixture: no structural failure for the colours rung: %+v", first.Checks)
			}
			tools, dir := fakeGifsicle(t, c.ctl)
			m := NewManager(newTestStore(t), tools, Options{Concurrency: 1})
			scratch := t.TempDir()
			data, rep, did, err := m.gifLadder(ctx, scratch, tag, fixed, first, target, c.colors, enc.GifsicleOptions{Loop: 3}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if did != c.want {
				t.Errorf("outcome = %+v, want %+v", did, c.want)
			}
			var steps []string
			for _, argv := range fakeGifsicleCalls(t, dir) {
				steps = append(steps, fakeGifsicleStep(argv))
				in, out := filepath.Base(gifsicleInputArg(argv)), filepath.Base(argv[len(argv)-1])
				if !strings.Contains(in, tag+"-") || !strings.Contains(out, tag+"-") {
					t.Errorf("%s pass reads %s and writes %s: both must carry the tag %q", steps[len(steps)-1], in, out, tag)
				}
				if hasArgPrefix(argv, "--lossy") || !hasArg(argv, "--loopcount=3") {
					t.Errorf("argv = %q, want no --lossy and --loopcount=3", argv)
				}
				if steps[len(steps)-1] == "colors" {
					want := c.colors
					if want == 0 {
						want = enc.DefaultColors
					}
					if i := slices.Index(argv, "--colors"); i < 0 || i+1 >= len(argv) || argv[i+1] != strconv.Itoa(want) {
						t.Errorf("colours rung argv = %q, want --colors %d", argv, want)
					}
				}
			}
			if got := strings.Join(steps, " "); got != c.steps {
				t.Errorf("gifsicle passes = %q, want %q", got, c.steps)
			}
			if c.want.replaced == hasStructuralError(rep) {
				t.Errorf("replaced=%v but the report's structural failure = %v: %+v", c.want.replaced, hasStructuralError(rep), rep.Checks)
			}
			if !c.want.replaced && !bytes.Equal(data, fixed) {
				t.Error("no rung succeeded, yet the bytes changed")
			}
			assertNoHoldScratch(t, scratch)
		})
	}
}

// TestFitEncodeGifskiLadderCall pins what fitRun.encode does with a gifski
// candidate (the real-tools counterpart, TestGifskiE2E, cannot see any of it
// and does not run in CI): the ladder gets the candidate's own tag on every
// scratch file, no --lossy (the knob is gifski's quality), Output.Colors and
// Output.Loop; the file on disk, the recorded size and the size the search
// sees are the ladder's bytes; the master's alpha scan is re-applied to the
// new report; and the description only mentions held frames when the ladder
// really repaired them. gifski is this binary too (runFakeGifsicle).
func TestFitEncodeGifskiLadderCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	flat := coalescedHoldsGIF(t)
	localOnly, err := gif.DecodeAll(bytes.NewReader(flat))
	if err != nil {
		t.Fatal(err)
	}
	// A local colour table (its third entry differs from the global one, so
	// image/gif writes it) that shows the global colours: the picture is the
	// canned flat file's, which the repair checks.
	localOnly.Image[2].Palette = color.Palette{holdsPalette[0], holdsPalette[1], color.RGBA{1, 2, 3, 255}}
	raw := encodeTestGIF(t, localOnly) // what "gifski" writes: a local colour table
	for _, c := range []struct {
		name      string
		ctl       map[string][]byte
		passes    int // ladder passes per candidate
		wantHolds bool
	}{
		{"the colours rung fixes it", map[string][]byte{"gifski.gif": raw, "colors.gif": flat}, 1, false},
		{"the colours pass makes clear-only frames", map[string][]byte{"gifski.gif": raw, "colors.gif": unsafeHoldsGIF(t), "U.gif": flat}, 3, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tools, dir := fakeGifsicle(t, c.ctl)
			tools.Gifski = tools.Gifsicle
			m := NewManager(newTestStore(t), tools, Options{Concurrency: 1})
			r := &fitRun{m: m, j: lintTestJob(), dir: t.TempDir(), target: discordlint.TargetAttachment, format: recipe.FormatGIF, cands: newFitCandidates(),
				master: enc.Master{Width: 64, Height: 48, FPS: 10, Frames: 3}, // HasAlpha false: the clip is opaque
				out:    recipe.Output{Format: "gif", Encoder: "gifski", Target: "attachment", Loop: 3, Colors: 64}}
			frames := &pngEntry{frames: []string{"f0.png"}}
			frames.once.Do(func() {}) // already rendered: no ffmpeg
			r.pngs.Store(variantKey(nil), frames)
			for _, id := range []string{"-0001-", "-0002-"} {
				before := len(fakeGifsicleCalls(t, dir))
				path, size, err := r.encode(ctx, fit.Rung{Label: "as requested"}, 30, 0)
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				cand := r.cands.get(path)
				if !bytes.Equal(got, flat) || size != int64(len(flat)) || cand.bytes != size {
					t.Errorf("candidate %s: %d bytes on disk, %d recorded, %d reported; want the ladder's %d", id, len(got), cand.bytes, size, len(flat))
				}
				if !cand.ok || hasStructuralError(cand.report) {
					t.Errorf("candidate %s not ok: %+v", id, cand.report.Checks)
				}
				// flat really uses its transparent index; the master is opaque.
				if cand.report.HasAlpha || !slices.ContainsFunc(cand.report.Checks, func(chk discordlint.Check) bool { return chk.Rule == RuleRenderAlpha }) {
					t.Errorf("candidate %s: the master's alpha scan was not re-applied to the ladder's report: HasAlpha=%v", id, cand.report.HasAlpha)
				}
				if desc := r.describe(cand); cand.holdsRepaired != c.wantHolds || strings.Contains(desc, holdRepairNote) != c.wantHolds {
					t.Errorf("candidate %s: holdsRepaired=%v desc %q, want the hold note = %v", id, cand.holdsRepaired, desc, c.wantHolds)
				}
				ladder := 0
				for _, argv := range fakeGifsicleCalls(t, dir)[before:] {
					out := filepath.Base(argv[len(argv)-1])
					if strings.HasPrefix(out, "gifski-loop") {
						continue // runGifski's loop pass
					}
					ladder++
					if in := filepath.Base(gifsicleInputArg(argv)); !strings.Contains(in, id) || !strings.Contains(out, id) {
						t.Errorf("ladder pass reads %s and writes %s: both must carry the tag %q", in, out, id)
					}
					if hasArgPrefix(argv, "--lossy") || !hasArg(argv, "--loopcount=3") {
						t.Errorf("ladder argv = %q, want no --lossy and --loopcount=3", argv)
					}
					if i := slices.Index(argv, "--colors"); fakeGifsicleStep(argv) == "colors" && (i+1 >= len(argv) || argv[i+1] != "64") {
						t.Errorf("colours rung argv = %q, want --colors 64 (Output.Colors)", argv)
					}
				}
				if ladder != c.passes {
					t.Errorf("candidate %s: %d ladder passes, want %d", id, ladder, c.passes)
				}
			}
			assertNoHoldScratch(t, r.dir)
		})
	}
}

// TestLintGIFLadderRungs pins which rungs lintGIF runs: the --colors rung is
// NOT run when gif.noop-frame-disposal is the only structural failure (a
// palette pass keeps the frame structure) — for a Discord target (error) and
// for target none (warning) alike — and IS run first when another structural
// rule failed too.
func TestLintGIFLadderRungs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	flat := coalescedHoldsGIF(t)
	for _, c := range []struct {
		name   string
		src    []byte
		target discordlint.Target
		steps  string
	}{
		{"holds only, attachment", unsafeHoldsGIF(t), discordlint.TargetAttachment, "U O"},
		{"holds only, target none", unsafeHoldsGIF(t), discordlint.TargetNone, "U O"},
		{"holds + local palette", unsafeHoldsLocalPaletteGIF(t), discordlint.TargetAttachment, "colors U O"},
	} {
		t.Run(c.name, func(t *testing.T) {
			first, _, err := discordlint.LintGIF(c.src, c.target, true)
			if err != nil {
				t.Fatal(err)
			}
			if wantColors := strings.HasPrefix(c.steps, "colors"); holdsCheck(t, &first).OK || ladderTriesColors(first) != wantColors || onlyHoldsFailed(first) == wantColors {
				t.Fatalf("fixture: ladderTriesColors=%v onlyHoldsFailed=%v checks %+v", ladderTriesColors(first), onlyHoldsFailed(first), first.Checks)
			}
			tools, dir := fakeGifsicle(t, map[string][]byte{"U.gif": flat})
			m := NewManager(newTestStore(t), tools, Options{Concurrency: 1})
			scratch := t.TempDir()
			data, rep, err := m.lintGIF(ctx, lintTestJob(), scratch, c.src, c.target, recipe.Output{Format: "gif", Lossy: 30, Loop: 3, Colors: 64})
			if err != nil {
				t.Fatal(err)
			}
			var steps []string
			calls := fakeGifsicleCalls(t, dir)
			for _, argv := range calls {
				steps = append(steps, fakeGifsicleStep(argv))
				if i := slices.Index(argv, "--colors"); steps[len(steps)-1] == "colors" && (i+1 >= len(argv) || argv[i+1] != "64" || !hasArg(argv, "--lossy=30")) {
					t.Errorf("colours rung argv = %q, want --colors 64 (Output.Colors) and --lossy=30", argv)
				}
			}
			if got := strings.Join(steps, " "); got != c.steps {
				t.Fatalf("gifsicle passes = %q, want %q (%q)", got, c.steps, calls)
			}
			assertRepairArgv(t, calls[len(calls)-2], calls[len(calls)-1], "--lossy=30", "--loopcount=3")
			if chk := holdsCheck(t, &rep); !chk.OK || hasStructuralError(rep) || !rep.OK {
				t.Errorf("report after the ladder: %+v", rep.Checks)
			}
			if n := assertOnePosePerFrame(t, data); n != 3 {
				t.Errorf("%d frames, want 3", n)
			}
			assertNoHoldScratch(t, scratch)
		})
	}
}

// mixedDisposalGIF is what ffmpeg's gif encoder makes of an alpha master that
// mixes frames with transparency (disposal 2) and fully opaque ones
// (disposal 1): complete full-canvas frames, one global palette.
func mixedDisposalGIF(t *testing.T) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: 0, Config: image.Config{Width: 64, Height: 48, ColorModel: holdsPalette}}
	for k, disposal := range []byte{gif.DisposalBackground, gif.DisposalNone, gif.DisposalBackground} {
		fr := image.NewPaletted(image.Rect(0, 0, 64, 48), holdsPalette)
		if disposal == gif.DisposalNone {
			fillRect(fr, fr.Rect, 1) // fully opaque
		} else {
			fillRect(fr, holdPoses[k%len(holdPoses)], 1)
		}
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 4)
		g.Disposal = append(g.Disposal, disposal)
	}
	return encodeTestGIF(t, g)
}

// leadInGIF is ffmpeg's GIF of an alpha fade-in: an entirely transparent
// frame (disposal 2), then fully opaque ones (disposal 1) to the end. Both
// disposals occur, yet the encoder renders it exactly.
func leadInGIF(t *testing.T) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: 0, Config: image.Config{Width: 64, Height: 48, ColorModel: holdsPalette}}
	for _, disposal := range []byte{gif.DisposalBackground, gif.DisposalNone, gif.DisposalNone} {
		fr := image.NewPaletted(image.Rect(0, 0, 64, 48), holdsPalette)
		if disposal == gif.DisposalNone {
			fillRect(fr, fr.Rect, 1)
		}
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 4)
		g.Disposal = append(g.Disposal, disposal)
	}
	return encodeTestGIF(t, g)
}

// TestEncodeGIFAtReencodesMixedClips pins the two-stage alpha encode without
// real tools: every alpha master is encoded with "-gifflags -offsetting"; only
// when that output mixes frames with transparency (disposal 2) and fully
// opaque ones (disposal 0/1) in a way ffmpeg renders wrong
// (discordlint.GIFNeedsCompleteFrames) is it encoded again with "-gifflags
// -offsetting-transdiff", and every frame of THAT output gets disposal 2.
// Uniform outputs, a transparent lead-in and opaque masters are encoded once;
// so is a clip the caller already knows to be mixed (the fit search presets
// CompleteFrames).
func TestEncodeGIFAtReencodesMixedClips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	master := enc.Master{Path: "frames.rgba", Width: 64, Height: 48, FPS: 25, Frames: 3, HasAlpha: true}
	for _, c := range []struct {
		name         string
		payload      []byte
		alpha        bool
		preset       bool     // GIFOptions.CompleteFrames set by the caller
		wantFlags    []string // -gifflags value of each ffmpeg call, in order ("" = none)
		wantAll2     bool
		wantComplete bool
	}{
		{"mixed alpha clip", mixedDisposalGIF(t), true, false, []string{"-offsetting", "-offsetting-transdiff"}, true, true},
		{"mixed alpha clip, known to the caller", mixedDisposalGIF(t), true, true, []string{"-offsetting-transdiff"}, true, true},
		{"every frame transparent", coalescedHoldsGIF(t), true, false, []string{"-offsetting"}, true, false},
		{"transparent lead-in, then opaque", leadInGIF(t), true, false, []string{"-offsetting"}, false, false},
		{"opaque master", mixedDisposalGIF(t), false, false, []string{""}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload := filepath.Join(t.TempDir(), "ffmpeg-out.gif")
			if err := os.WriteFile(payload, c.payload, 0o644); err != nil {
				t.Fatal(err)
			}
			tools, dir := fakeFFmpegTools(t)
			releaseFakes(t, dir)
			t.Setenv(fakeOutEnv, payload)
			m := &Manager{tools: tools}
			path, complete, err := m.encodeGIFMixed(ctx, nil, t.TempDir(), "-c1", master, enc.GIFOptions{HasAlpha: c.alpha, CompleteFrames: c.preset}, enc.GifsicleOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if complete != c.wantComplete {
				t.Errorf("complete = %v, want %v", complete, c.wantComplete)
			}
			// One marker per ffmpeg call: "<pid>\n<argv, one per line>".
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			type call struct {
				at    time.Time
				flags string
			}
			var calls []call
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".") || e.Name() == fakeRelease {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				info, err := e.Info()
				if err != nil {
					t.Fatal(err)
				}
				argv := strings.Split(strings.TrimSpace(string(data)), "\n")
				flags := ""
				if i := slices.Index(argv, "-gifflags"); i >= 0 && i+1 < len(argv) {
					flags = argv[i+1]
				}
				calls = append(calls, call{info.ModTime(), flags})
			}
			slices.SortStableFunc(calls, func(a, b call) int { return a.at.Compare(b.at) })
			var got []string
			for _, cl := range calls {
				got = append(got, cl.flags)
			}
			if len(got) == 2 && got[0] != "-offsetting" { // equal mtimes: the order is known anyway
				slices.Reverse(got)
			}
			if !slices.Equal(got, c.wantFlags) {
				t.Errorf("ffmpeg -gifflags per call = %q, want %q", got, c.wantFlags)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			counts, err := discordlint.GIFDisposals(data)
			if err != nil {
				t.Fatal(err)
			}
			if all2 := counts[0]+counts[1]+counts[3] == 0 && counts[2] > 0; all2 != c.wantAll2 {
				t.Errorf("disposals of the delivered base %v: every frame disposed = %v, want %v", counts, all2, c.wantAll2)
			}
		})
	}
}

// TestRepairGIFHoldsChecksThePicture pins the two halves of the repair's
// safety net without real tools. gifsicle decides from the FIRST frame whether
// the canvas is transparent at all, and gives up on local colour tables — both
// at exit 0 — so (1) a clip that shows the background anywhere is handed over
// with a transparent 1x1 lead-in frame that the frame selection "#1-" drops
// again, and (2) whatever comes back is played and compared with the input: a
// coalesce that changed the picture is an error, and the callers keep the
// bytes and the failing report they had.
func TestRepairGIFHoldsChecksThePicture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mixed := opaqueFirstHoldsGIF(t)
	good, painted := opaqueFirstHoldsFlat(t, false), opaqueFirstHoldsFlat(t, true)
	const target = discordlint.TargetAttachment
	lint, fixed, err := discordlint.LintGIF(mixed, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if !onlyHoldsFailed(lint) {
		t.Fatalf("fixture must fail %s only: %+v", discordlint.RuleGIFNoopFrameDisposal, lint.Checks)
	}

	t.Run("the lead-in frame, and an exact result", func(t *testing.T) {
		tools, dir := fakeGifsicle(t, map[string][]byte{"U.gif": good})
		m := &Manager{tools: tools}
		scratch := t.TempDir()
		out, rep, err := m.repairGIFHolds(ctx, scratch, "-c3", mixed, target, enc.GifsicleOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		calls := fakeGifsicleCalls(t, dir)
		if len(calls) != 2 {
			t.Fatalf("%d gifsicle calls, want 2: %q", len(calls), calls)
		}
		if i := slices.Index(calls[0], "#1-"); i < 1 || calls[0][i-1] != gifsicleInputArg(calls[0]) || hasArg(calls[1], "#1-") {
			t.Errorf("frame selection: step A %q, step B %q", calls[0], calls[1])
		}
		handed, err := os.ReadFile(filepath.Join(dir, "call-0.in.gif"))
		if err != nil {
			t.Fatal(err)
		}
		if want, err := discordlint.PrependTransparentFrame(mixed); err != nil || !bytes.Equal(handed, want) {
			t.Errorf("step A was not handed the input with the lead-in frame (err %v)", err)
		}
		if hasStructuralError(rep) {
			t.Errorf("report: %+v", rep.Checks)
		}
		assertOpaqueFirstHolds(t, out, true)
		assertNoHoldScratch(t, scratch)
	})

	t.Run("an opaque clip gets no lead-in frame", func(t *testing.T) {
		tools, dir := fakeGifsicle(t, nil)
		m := &Manager{tools: tools}
		// With a global colour table, so that it is the ShowsBackground gate
		// that keeps the lead-in out, not PrependTransparentFrame's refusal.
		og, err := gif.DecodeAll(bytes.NewReader(opaqueGIF(t)))
		if err != nil {
			t.Fatal(err)
		}
		og.Config.ColorModel = og.Image[0].Palette
		opaque := encodeTestGIF(t, og)
		if _, err := discordlint.PrependTransparentFrame(opaque); err != nil {
			t.Fatalf("fixture must accept a lead-in frame: %v", err)
		}
		if _, _, err := m.repairGIFHolds(ctx, t.TempDir(), "", opaque, target, enc.GifsicleOptions{}, nil); err != nil {
			t.Fatal(err)
		}
		calls := fakeGifsicleCalls(t, dir)
		if len(calls) == 0 || hasArg(calls[0], "#1-") {
			t.Fatalf("calls = %q, want a coalesce without a frame selection", calls)
		}
		if handed, err := os.ReadFile(filepath.Join(dir, "call-0.in.gif")); err != nil || !bytes.Equal(handed, opaque) {
			t.Errorf("step A was not handed the input as it is (err %v)", err)
		}
	})

	t.Run("a coalesce that changed the picture is refused", func(t *testing.T) {
		// What gifsicle -U writes without the lead-in: every frame opaque.
		tools, dir := fakeGifsicle(t, map[string][]byte{"U.gif": painted})
		m := NewManager(newTestStore(t), tools, Options{Concurrency: 1})
		scratch := t.TempDir()
		if _, _, err := m.repairGIFHolds(ctx, scratch, "", mixed, target, enc.GifsicleOptions{}, nil); err == nil || !strings.Contains(err.Error(), "picture changed") {
			t.Fatalf("err = %v, want the coalesce refused because the picture changed", err)
		}
		if calls := fakeGifsicleCalls(t, dir); len(calls) != 1 {
			t.Errorf("%d gifsicle calls, want the coalesce only: %q", len(calls), calls)
		}
		assertNoHoldScratch(t, scratch)
		// The callers keep what they had, with the hold rule still failing.
		data, rep, repaired := m.repairIfOnlyHolds(ctx, scratch, "-x", fixed, lint, target, enc.GifsicleOptions{})
		if repaired || !bytes.Equal(data, fixed) || holdsCheck(t, &rep).OK {
			t.Errorf("repairIfOnlyHolds: repaired=%v, same bytes=%v, hold check %+v", repaired, bytes.Equal(data, fixed), holdsCheck(t, &rep))
		}
		data, rep, err := m.lintGIF(ctx, lintTestJob(), scratch, mixed, target, recipe.Output{Format: "gif"})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, fixed) || holdsCheck(t, &rep).OK || rep.OK {
			t.Errorf("lintGIF: same bytes=%v, report OK=%v, hold check %+v — want the unrepaired file with the failing check", bytes.Equal(data, fixed), rep.OK, holdsCheck(t, &rep))
		}
		assertNoHoldScratch(t, scratch)
	})

	t.Run("a lossless step B that changed the picture falls back to step A", func(t *testing.T) {
		tools, _ := fakeGifsicle(t, map[string][]byte{"U.gif": good, "O.gif": painted})
		m := &Manager{tools: tools}
		out, _, err := m.repairGIFHolds(ctx, t.TempDir(), "", mixed, target, enc.GifsicleOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertOpaqueFirstHolds(t, out, true)
		// With a lossy knob step B cannot be compared and is taken as it is.
		out, _, err = m.repairGIFHolds(ctx, t.TempDir(), "", mixed, target, enc.GifsicleOptions{Lossy: 40}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if same, _, err := discordlint.SameGIFAnimation(painted, out); err != nil || !same {
			t.Errorf("lossy: step B's bytes were not delivered (err %v)", err)
		}
	})
}

// reserveLastDisposal gives the last frame of a GIF the reserved disposal 4,
// which decoders read differently: discordlint.PlayGIF refuses such a file.
func reserveLastDisposal(t *testing.T, data []byte) []byte {
	t.Helper()
	out := bytes.Clone(data)
	i := bytes.LastIndex(out, []byte{0x21, 0xF9, 0x04})
	if i < 0 {
		t.Fatal("fixture: no graphic control extension")
	}
	out[i+3] = out[i+3]&^0x1C | 4<<2
	if _, err := discordlint.PlayGIF(out); err == nil {
		t.Fatal("fixture: still playable")
	}
	return out
}

// TestRepairGIFHoldsUncheckable pins the edges of the check: an input that
// cannot be played is repaired unchecked (with the lead-in frame — nothing
// says it is opaque), a RESULT that cannot be played is refused, a file
// without a global colour table gets no lead-in frame but is still judged,
// and a result that differs in colour only is refused like any other.
func TestRepairGIFHoldsUncheckable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mixed := opaqueFirstHoldsGIF(t)
	good := opaqueFirstHoldsFlat(t, false)
	const target = discordlint.TargetAttachment

	t.Run("an unplayable input is repaired unchecked, with the lead-in frame", func(t *testing.T) {
		in := reserveLastDisposal(t, mixed)
		tools, dir := fakeGifsicle(t, map[string][]byte{"U.gif": good})
		m := &Manager{tools: tools}
		scratch := t.TempDir()
		if _, _, err := m.repairGIFHolds(ctx, scratch, "", in, target, enc.GifsicleOptions{}, nil); err != nil {
			t.Fatalf("err = %v, want the repair to go through unchecked", err)
		}
		calls := fakeGifsicleCalls(t, dir)
		if len(calls) != 2 {
			t.Fatalf("%d gifsicle calls, want 2: %q", len(calls), calls)
		}
		if i := slices.Index(calls[0], "#1-"); i < 1 || calls[0][i-1] != gifsicleInputArg(calls[0]) {
			t.Errorf("step A argv = %q, want the frame selection after the input", calls[0])
		}
		handed, err := os.ReadFile(filepath.Join(dir, "call-0.in.gif"))
		if err != nil {
			t.Fatal(err)
		}
		if want, err := discordlint.PrependTransparentFrame(in); err != nil || !bytes.Equal(handed, want) {
			t.Errorf("step A was not handed the input with the lead-in frame (err %v)", err)
		}
		assertNoHoldScratch(t, scratch)
	})

	t.Run("a result that cannot be played is refused", func(t *testing.T) {
		tools, dir := fakeGifsicle(t, map[string][]byte{"U.gif": reserveLastDisposal(t, good)})
		m := &Manager{tools: tools}
		scratch := t.TempDir()
		if _, _, err := m.repairGIFHolds(ctx, scratch, "", mixed, target, enc.GifsicleOptions{}, nil); err == nil || !strings.Contains(err.Error(), "cannot be played") {
			t.Fatalf("err = %v, want the unplayable coalesce refused", err)
		}
		if calls := fakeGifsicleCalls(t, dir); len(calls) != 1 {
			t.Errorf("%d gifsicle calls, want the coalesce only", len(calls))
		}
		assertNoHoldScratch(t, scratch)
	})

	t.Run("no global colour table: no lead-in frame, still judged", func(t *testing.T) {
		g, err := gif.DecodeAll(bytes.NewReader(mixed))
		if err != nil {
			t.Fatal(err)
		}
		g.Config = image.Config{Width: 64, Height: 48} // no ColorModel: image/gif writes local tables only
		in := encodeTestGIF(t, g)
		if _, err := discordlint.PrependTransparentFrame(in); err == nil {
			t.Fatal("fixture: a lead-in frame is possible, so this is not the no-global-table case")
		}
		// The fake copies its input: the "coalesce" shows the same picture.
		tools, dir := fakeGifsicle(t, nil)
		m := &Manager{tools: tools}
		if _, _, err := m.repairGIFHolds(ctx, t.TempDir(), "", in, target, enc.GifsicleOptions{}, nil); err != nil {
			t.Fatalf("err = %v, want the repair to carry on without the lead-in frame", err)
		}
		calls := fakeGifsicleCalls(t, dir)
		if len(calls) == 0 || hasArg(calls[0], "#1-") {
			t.Fatalf("calls = %q, want a coalesce without a frame selection", calls)
		}
		if handed, err := os.ReadFile(filepath.Join(dir, "call-0.in.gif")); err != nil || !bytes.Equal(handed, in) {
			t.Errorf("step A was not handed the input as it is (err %v)", err)
		}
		// …and a wrong result is still refused.
		tools, _ = fakeGifsicle(t, map[string][]byte{"U.gif": opaqueFirstHoldsFlat(t, true)})
		m = &Manager{tools: tools}
		if _, _, err := m.repairGIFHolds(ctx, t.TempDir(), "", in, target, enc.GifsicleOptions{}, nil); err == nil || !strings.Contains(err.Error(), "picture changed") {
			t.Errorf("err = %v, want the changed picture refused", err)
		}
	})

	t.Run("a result that differs in colour only is refused", func(t *testing.T) {
		g, err := gif.DecodeAll(bytes.NewReader(good))
		if err != nil {
			t.Fatal(err)
		}
		pal := append(color.Palette(nil), holdsPalette...)
		pal[1] = color.RGBA{10, 200, 10, 255} // the sprite, green instead of red
		g.Config.ColorModel = pal
		for _, fr := range g.Image {
			fr.Palette = pal
		}
		tools, _ := fakeGifsicle(t, map[string][]byte{"U.gif": encodeTestGIF(t, g)})
		m := &Manager{tools: tools}
		if _, _, err := m.repairGIFHolds(ctx, t.TempDir(), "", mixed, target, enc.GifsicleOptions{}, nil); err == nil || !strings.Contains(err.Error(), "differs in colour") {
			t.Errorf("err = %v, want the recoloured coalesce refused", err)
		}
	})
}
