package ffrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// needSh skips tests that drive /bin/sh (the process tests are Unix-only;
// the runtime image is Linux).
func needSh(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test; skipped on windows")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	return sh
}

// writeScript creates an executable shell script named name in dir.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLookupTools_EnvOverrideAndPath(t *testing.T) {
	dir := t.TempDir()
	name := "gifsicle"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	onPath := filepath.Join(dir, name)
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("EZLG_FFMPEG", "/opt/fake/ffmpeg")
	t.Setenv("EZLG_FFPROBE", "  /opt/fake/ffprobe  ") // trimmed
	for _, spec := range toolSpecs {
		if spec.env != "EZLG_FFMPEG" && spec.env != "EZLG_FFPROBE" {
			t.Setenv(spec.env, "")
		}
	}

	tools := LookupTools()
	if tools.FFmpeg != "/opt/fake/ffmpeg" {
		t.Errorf("FFmpeg = %q, want env override", tools.FFmpeg)
	}
	if tools.FFprobe != "/opt/fake/ffprobe" {
		t.Errorf("FFprobe = %q, want trimmed env override", tools.FFprobe)
	}
	if tools.Gifsicle != onPath {
		t.Errorf("Gifsicle = %q, want %q from PATH", tools.Gifsicle, onPath)
	}
	for _, spec := range toolSpecs {
		switch spec.name {
		case "ffmpeg", "ffprobe", "gifsicle":
			continue
		}
		if got := *spec.field(&tools); got != "" {
			t.Errorf("%s = %q, want empty (not on PATH, no env)", spec.name, got)
		}
	}
}

func TestLookupTools_CoversEveryField(t *testing.T) {
	// Every Tools field must be reachable through toolSpecs, otherwise a
	// tool could never be resolved.
	var probe Tools
	for _, spec := range toolSpecs {
		*spec.field(&probe) = "x"
	}
	if probe != (Tools{"x", "x", "x", "x", "x", "x", "x", "x", "x", "x"}) {
		t.Fatalf("toolSpecs does not cover every Tools field: %+v", probe)
	}
}

func TestVersions(t *testing.T) {
	needSh(t)
	dir := t.TempDir()
	tools := Tools{
		FFmpeg:   writeScript(t, dir, "ffmpeg", "echo 'ffmpeg version 9.0.1-fake Copyright'\necho 'built with gcc'\n"),
		Gifsicle: writeScript(t, dir, "gifsicle", "echo 'LCDF Gifsicle 1.96' >&2\n"),
		Oxipng:   writeScript(t, dir, "oxipng", "exit 0\n"),
		Avifenc:  writeScript(t, dir, "avifenc", "test \"$1\" = --version || exit 2\necho\necho 'Version: 1.2.1 (aom [enc/dec]:3.12.0)'\n"),
		Gifski:   writeScript(t, dir, "gifski", "sleep 10\necho late\n"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := tools.Versions(ctx)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("Versions took %v; hanging tool was not bounded", el)
	}
	want := map[string]string{
		"ffmpeg":   "ffmpeg version 9.0.1-fake Copyright",
		"gifsicle": "LCDF Gifsicle 1.96",
		"oxipng":   "",
		"avifenc":  "Version: 1.2.1 (aom [enc/dec]:3.12.0)",
		"gifski":   "",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %v, want %d %v", len(got), got, len(want), want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("Versions[%q] = %q (present %v), want %q", k, g, ok, v)
		}
	}
}

func TestVersions_NoToolsNoWork(t *testing.T) {
	got := Tools{}.Versions(context.Background())
	if len(got) != 0 {
		t.Fatalf("got %v, want empty map", got)
	}
}

func TestRunOutput_Success(t *testing.T) {
	sh := needSh(t)
	out, err := RunOutput(context.Background(), sh, []string{"-c", "printf hello; echo warning >&2"})
	if err != nil {
		t.Fatalf("RunOutput: %v", err)
	}
	if string(out) != "hello" {
		t.Fatalf("stdout = %q, want %q", out, "hello")
	}
}

func TestRun_ExitErrorCarriesStderrTail(t *testing.T) {
	sh := needSh(t)
	err := Run(context.Background(), sh, []string{"-c", "echo first >&2; echo boom >&2; exit 3"})
	if err == nil {
		t.Fatal("expected an error")
	}
	want := "sh exited: exit status 3\nfirst\nboom"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("errors.As ExitError failed or wrong code: %v", err)
	}
}

func TestRun_ExitErrorWithoutStderr(t *testing.T) {
	sh := needSh(t)
	err := Run(context.Background(), sh, []string{"-c", "exit 4"})
	if err == nil || err.Error() != "sh exited: exit status 4" {
		t.Fatalf("error = %v, want %q", err, "sh exited: exit status 4")
	}
}

func TestRun_StderrTailIsBounded(t *testing.T) {
	sh := needSh(t)
	// ~140 KB of stderr, well past the 64 KB ring buffer.
	script := "i=0; while [ $i -lt 10000 ]; do echo \"stderr line $i\" >&2; i=$((i+1)); done; exit 1"
	err := Run(context.Background(), sh, []string{"-c", script})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	head, tail, ok := strings.Cut(msg, "\n")
	if !ok || head != "sh exited: exit status 1" {
		t.Fatalf("unexpected first line %q", head)
	}
	lines := strings.Split(tail, "\n")
	if len(lines) != stderrTailLines {
		t.Fatalf("tail has %d lines, want %d", len(lines), stderrTailLines)
	}
	if lines[0] != "stderr line 9960" || lines[len(lines)-1] != "stderr line 9999" {
		t.Fatalf("tail window wrong: first %q last %q", lines[0], lines[len(lines)-1])
	}
}

func TestRun_CancelKillsPromptly(t *testing.T) {
	sh := needSh(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	// "sleep 5; echo done" forces sh to fork sleep as a child, so this also
	// proves the whole process group is killed (otherwise the orphaned
	// sleep would keep our stdout pipe open until it finished).
	out, err := RunOutput(ctx, sh, []string{"-c", "sleep 5; echo done"})
	el := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if el > 2*time.Second {
		t.Fatalf("cancellation took %v, want well under 2s", el)
	}
	if len(out) != 0 {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestRun_DeadlineExceeded(t *testing.T) {
	sh := needSh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Run(ctx, sh, []string{"-c", "sleep 5"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout took %v", el)
	}
}

func TestRun_AlreadyCancelledContext(t *testing.T) {
	sh := needSh(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, sh, []string{"-c", "echo should-not-run"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRun_MissingBinary(t *testing.T) {
	err := Run(context.Background(), filepath.Join(t.TempDir(), "no-such-tool"), nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.HasPrefix(err.Error(), "no-such-tool: ") {
		t.Fatalf("error should name the tool: %v", err)
	}
	if err := Run(context.Background(), "", nil); err == nil {
		t.Fatal("empty binary path must fail")
	}
}

func TestRunTo_StreamsLargeStdout(t *testing.T) {
	sh := needSh(t)
	var got strings.Builder
	err := RunTo(context.Background(), sh, []string{"-c", "i=0; while [ $i -lt 20000 ]; do echo $i; i=$((i+1)); done"}, &got)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(got.String()), "\n")
	if len(lines) != 20000 || lines[0] != "0" || lines[19999] != "19999" {
		t.Fatalf("streamed %d lines (first %q, last %q)", len(lines), lines[0], lines[len(lines)-1])
	}
}

// TestRun_GrandchildHoldingPipeIsBounded: a helper that escapes the process
// group and keeps our stdout open must not hang Wait past WaitDelay.
func TestRun_GrandchildHoldingPipeIsBounded(t *testing.T) {
	sh := needSh(t)
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	start := time.Now()
	err := Run(context.Background(), sh, []string{"-c", "setsid sleep 4 & echo started"})
	el := time.Since(start)
	if el > waitDelay+2*time.Second {
		t.Fatalf("Run took %v; WaitDelay did not bound the straggler", el)
	}
	if err == nil {
		t.Fatalf("expected ErrWaitDelay-derived error when a grandchild keeps the pipe open (took %v)", el)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("err = %v, want it to wrap exec.ErrWaitDelay", err)
	}
}

// fakeFFmpeg writes a script that echoes its argv to stderr (one arg per
// line), prints the given stdout, and exits with code.
func fakeFFmpeg(t *testing.T, stdout string, code int) string {
	t.Helper()
	body := fmt.Sprintf("for a in \"$@\"; do echo \"$a\" >&2; done\nprintf '%%s' '%s'\nexit %d\n", stdout, code)
	return writeScript(t, t.TempDir(), "ffmpeg", body)
}

func TestRunFFmpeg_ArgvPrefixAndProgressSuffix(t *testing.T) {
	needSh(t)
	ff := fakeFFmpeg(t, "", 1)
	args := []string{"-i", "in.mov", "-f", "null", "-"}

	err := RunFFmpeg(context.Background(), ff, args, nil)
	if err == nil {
		t.Fatal("expected exit error")
	}
	_, argv, _ := strings.Cut(err.Error(), "\n")
	want := strings.Join([]string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-i", "in.mov", "-f", "null", "-"}, "\n")
	if argv != want {
		t.Fatalf("argv without progress:\n%s\nwant:\n%s", argv, want)
	}

	err = RunFFmpeg(context.Background(), ff, args, func(Progress) {})
	if err == nil {
		t.Fatal("expected exit error")
	}
	if !strings.HasPrefix(err.Error(), "ffmpeg exited: exit status 1\n") {
		t.Fatalf("unexpected error prefix: %q", err.Error())
	}
	_, argv, _ = strings.Cut(err.Error(), "\n")
	want += "\n" + strings.Join([]string{"-progress", "pipe:1", "-nostats", "-stats_period", "0.2"}, "\n")
	if argv != want {
		t.Fatalf("argv with progress:\n%s\nwant:\n%s", argv, want)
	}
}

func TestRunFFmpeg_ProgressCallbacksFromFake(t *testing.T) {
	needSh(t)
	blocks := "frame=2\nfps=10.0\nout_time_us=80000\nspeed=1x\nprogress=continue\n" +
		"frame=5\nfps=12.5\nout_time_us=200000\nspeed=1.1x\nprogress=end\n"
	ff := fakeFFmpeg(t, blocks, 0)
	var got []Progress
	if err := RunFFmpeg(context.Background(), ff, []string{"-i", "x"}, func(p Progress) { got = append(got, p) }); err != nil {
		t.Fatalf("RunFFmpeg: %v", err)
	}
	want := []Progress{
		{Frame: 2, FPS: 10, OutTimeMS: 80, Speed: "1x"},
		{Frame: 5, FPS: 12.5, OutTimeMS: 200, Speed: "1.1x", Done: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d blocks %+v, want %+v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestRunFFmpeg_Real drives a real ffmpeg when one is on PATH (the tools
// image); the plain golang image skips it.
func TestRunFFmpeg_Real(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var got []Progress
	args := []string{"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-frames:v", "5", "-f", "null", "-"}
	if err := RunFFmpeg(ctx, ff, args, func(p Progress) { got = append(got, p) }); err != nil {
		t.Fatalf("RunFFmpeg: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no progress blocks received")
	}
	last := got[len(got)-1]
	if !last.Done {
		t.Fatalf("last block not Done: %+v", last)
	}
	if last.Frame != 5 {
		t.Fatalf("final frame = %d, want 5 (%+v)", last.Frame, last)
	}
	if last.OutTimeMS != 1000 {
		t.Fatalf("final OutTimeMS = %d, want 1000 (5 frames at 5 fps)", last.OutTimeMS)
	}
	for _, p := range got[:len(got)-1] {
		if p.Done {
			t.Fatalf("Done set before the final block: %+v", got)
		}
	}

	// A failing invocation must surface ffmpeg's own diagnostics.
	err = RunFFmpeg(ctx, ff, []string{"-i", filepath.Join(t.TempDir(), "missing.mov"), "-f", "null", "-"}, nil)
	if err == nil {
		t.Fatal("expected an error for a missing input")
	}
	if !strings.HasPrefix(err.Error(), "ffmpeg exited: ") || !strings.Contains(err.Error(), "missing.mov") {
		t.Fatalf("error should carry the stderr tail: %v", err)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 16}
	for _, s := range []string{"aaaa\n", "bbbb\n", "cccc\n", "dddd\n", "eeee\n"} {
		if _, err := b.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	// 25 bytes written into 16: the oldest bytes fall off the front, leaving
	// a partial first line.
	if got := string(b.buf); got != "\ncccc\ndddd\neeee\n" || !b.dropped {
		t.Fatalf("buf = %q dropped=%v", got, b.dropped)
	}
	if got := b.Tail(2); got != "dddd\neeee" {
		t.Fatalf("Tail(2) = %q", got)
	}
	if got := b.Tail(10); got != "cccc\ndddd\neeee" {
		t.Fatalf("Tail(10) = %q", got)
	}
	if _, err := b.Write([]byte("f\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := b.Tail(2); got != "eeee\nf" {
		t.Fatalf("Tail(2) after CRLF write = %q", got)
	}
	// One write larger than the buffer keeps only its end.
	if _, err := b.Write([]byte(strings.Repeat("x", 40) + "tail")); err != nil {
		t.Fatal(err)
	}
	if got := b.Tail(5); got != "xxxxxxxxxxxxtail" {
		t.Fatalf("oversized write: %q", got)
	}
	empty := &tailBuffer{max: 16}
	if got := empty.Tail(3); got != "" {
		t.Fatalf("empty Tail = %q", got)
	}
	_, _ = empty.Write([]byte("\r\n\r\n"))
	if got := empty.Tail(3); got != "" {
		t.Fatalf("whitespace-only Tail = %q", got)
	}
}
