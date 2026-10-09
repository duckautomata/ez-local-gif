package ffrun

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// RunCapture returns stderr on success (what RunOutput cannot), keeps the
// stdout/stderr split and still carries the stderr tail in the error.
func TestRunCapture_StderrOnSuccess(t *testing.T) {
	sh := needSh(t)
	stdout, stderr, err := RunCapture(context.Background(), sh, []string{"-c", "printf out; echo 'crop=24:24:16:12' >&2; echo second >&2"})
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}
	if string(stdout) != "out" {
		t.Errorf("stdout = %q, want %q", stdout, "out")
	}
	if string(stderr) != "crop=24:24:16:12\nsecond\n" {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRunCapture_FailureKeepsStderrAndError(t *testing.T) {
	sh := needSh(t)
	stdout, stderr, err := RunCapture(context.Background(), sh, []string{"-c", "printf partial; echo boom >&2; exit 2"})
	if err == nil {
		t.Fatal("expected an error")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Errorf("error does not carry the exit status: %v", err)
	}
	if !strings.HasSuffix(err.Error(), "\nboom") {
		t.Errorf("error lacks the stderr tail: %q", err.Error())
	}
	if string(stdout) != "partial" || string(stderr) != "boom\n" {
		t.Errorf("captured stdout %q stderr %q", stdout, stderr)
	}
}

func TestRunCapture_StderrIsBounded(t *testing.T) {
	sh := needSh(t)
	// ~140 KB of stderr at exit 0: only the last 64 KiB survive, ending with
	// the final line.
	script := "i=0; while [ $i -lt 10000 ]; do echo \"stderr line $i\" >&2; i=$((i+1)); done"
	_, stderr, err := RunCapture(context.Background(), sh, []string{"-c", script})
	if err != nil {
		t.Fatal(err)
	}
	if len(stderr) > stderrTailBytes {
		t.Errorf("stderr tail is %d bytes, cap is %d", len(stderr), stderrTailBytes)
	}
	if !strings.HasSuffix(string(stderr), "stderr line 9999\n") {
		t.Errorf("tail does not end with the last line: %q", stderr[max(0, len(stderr)-40):])
	}
}

func TestRunCapture_MissingBinaryAndCancelledContext(t *testing.T) {
	if _, _, err := RunCapture(context.Background(), "", nil); err == nil {
		t.Error("empty binary path accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := RunCapture(ctx, "does-not-matter", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v", err)
	}
}

// slowWriter sleeps past WaitDelay on its first write, then accepts
// everything: what the matte pass's batch writer does while it POSTs a
// batch to a slow sidecar.
type slowWriter struct {
	strings.Builder
	slept bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if !w.slept {
		w.slept = true
		time.Sleep(waitDelay + 500*time.Millisecond)
	}
	return w.Builder.Write(p)
}

// TestRunFrom_SlowConsumerKeepsEveryByte: a consumer that blocks longer
// than WaitDelay after the tool has written everything and exited still
// receives the whole stream — where RunTo loses the rest to
// exec.ErrWaitDelay (the contrast that makes the fix observable).
func TestRunFrom_SlowConsumerKeepsEveryByte(t *testing.T) {
	sh := needSh(t)
	script := "i=0; while [ $i -lt 2000 ]; do echo $i; i=$((i+1)); done" // ~9 KB: fits the pipe, sh exits at once
	check := func(t *testing.T, got string) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(got), "\n")
		if len(lines) != 2000 || lines[0] != "0" || lines[1999] != "1999" {
			t.Fatalf("streamed %d lines (first %q, last %q), want 2000", len(lines), lines[0], lines[len(lines)-1])
		}
	}
	var slow slowWriter
	err := RunFrom(context.Background(), sh, []string{"-c", script}, func(r io.Reader) error {
		_, err := io.Copy(&slow, r)
		return err
	})
	if err != nil {
		t.Fatalf("RunFrom: %v", err)
	}
	check(t, slow.String())

	var viaRunTo slowWriter
	if err := RunTo(context.Background(), sh, []string{"-c", script}, &viaRunTo); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("RunTo with the same slow writer: %v, want exec.ErrWaitDelay (the case RunFrom exists for)", err)
	}
}

// TestRunFrom_ReadErrorAbortsPromptly: a read callback that gives up ends
// the run at once with its own error, the tool killed, not after the tool
// would have finished on its own.
func TestRunFrom_ReadErrorAbortsPromptly(t *testing.T) {
	sh := needSh(t)
	boom := errors.New("boom")
	start := time.Now()
	err := RunFrom(context.Background(), sh, []string{"-c", "echo start; sleep 5; echo done"}, func(r io.Reader) error {
		buf := make([]byte, 64)
		if _, err := r.Read(buf); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read error", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("abort took %v, want well under the tool's 5 s", el)
	}
}

// TestRunFrom_CancelAndExitError: cancelling ctx kills the tool and
// surfaces ctx.Err(); a non-zero exit carries the stderr tail like RunTo.
func TestRunFrom_CancelAndExitError(t *testing.T) {
	sh := needSh(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := RunFrom(ctx, sh, []string{"-c", "sleep 5; echo done"}, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("cancellation took %v", el)
	}
	err = RunFrom(context.Background(), sh, []string{"-c", "echo partial; echo oops >&2; exit 3"}, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !strings.Contains(err.Error(), "oops") || !strings.HasPrefix(err.Error(), "sh exited: ") {
		t.Fatalf("exit error = %v", err)
	}
	if err := RunFrom(context.Background(), "", nil, func(io.Reader) error { return nil }); err == nil {
		t.Error("empty binary path accepted")
	}
}
