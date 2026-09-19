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
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
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
//	(none)  the input is copied to the output
func runFakeGifsicle(dir string) int {
	args := os.Args[1:]
	if !isGifsicleArgv(args) {
		fmt.Println("LCDF Gifsicle fake-ezlg-jobs-test")
		return 0
	}
	in, out := args[len(args)-3], args[len(args)-1]
	data, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake gifsicle: input:", err)
		return 1
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
		data = canned
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
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
		if in := calls[0][len(calls[0])-3]; filepath.Base(in) != "holds-c7-in.gif" {
			t.Errorf("step A input = %s", in)
		}
		if a, b := calls[0][len(calls[0])-1], calls[1][len(calls[1])-3]; a != b || filepath.Base(a) != "holds-c7-flat.gif" {
			t.Errorf("step B reads %s, step A wrote %s", b, a)
		}
		if g := fakeGifsicleInput(t, dir, 0); len(g.Image) != 75 {
			t.Errorf("step A was handed %d frames, want the 75 of the input", len(g.Image))
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
			data, rep, err := m.lintGIF(ctx, lintTestJob(), scratch, c.src, c.target, recipe.Output{Format: "gif", Lossy: 30, Loop: 3})
			if err != nil {
				t.Fatal(err)
			}
			var steps []string
			calls := fakeGifsicleCalls(t, dir)
			for _, argv := range calls {
				steps = append(steps, fakeGifsicleStep(argv))
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
