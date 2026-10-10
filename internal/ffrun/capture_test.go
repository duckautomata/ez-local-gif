package ffrun

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
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
