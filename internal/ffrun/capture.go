package ffrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// RunCapture runs an arbitrary tool and returns its stdout in full and the
// tail of its stderr (the last 64 KiB, as kept for error messages) whether
// or not the run succeeded. It exists for tools that report their result on
// stderr at exit status 0 — ffmpeg's cropdetect prints its "crop=" lines at
// -loglevel info — which RunOutput's error-only stderr cannot carry.
// Cancellation, the process-group kill and the error shape are those of
// RunTo; on failure the captured stdout and stderr tail are returned
// alongside the error (which still carries the tail's last lines).
func RunCapture(ctx context.Context, bin string, args []string) (stdout, stderr []byte, err error) {
	var out bytes.Buffer
	tail, err := runProcess(ctx, bin, args, &out)
	return out.Bytes(), tail.Bytes(), err
}

// runProcess starts bin with args, streams stdout into w (nil discards it)
// and keeps the stderr tail. The tail buffer is returned in every case so
// callers that want stderr on success can read it; the error contract is
// RunTo's.
func runProcess(ctx context.Context, bin string, args []string, w io.Writer) (*tailBuffer, error) {
	stderr := &tailBuffer{max: stderrTailBytes}
	if bin == "" {
		return stderr, errors.New("ffrun: no binary path given")
	}
	if err := ctx.Err(); err != nil {
		return stderr, err
	}
	name := toolName(bin)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = w
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	setSysProcAttr(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }

	if err := cmd.Start(); err != nil {
		return stderr, fmt.Errorf("%s: %w", name, err)
	}
	err := cmd.Wait()
	if err == nil {
		return stderr, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return stderr, cerr
	}
	if tail := stderr.Tail(stderrTailLines); tail != "" {
		return stderr, fmt.Errorf("%s exited: %w\n%s", name, err, tail)
	}
	return stderr, fmt.Errorf("%s exited: %w", name, err)
}

// Bytes returns a copy of everything the buffer still holds (the last max
// bytes written).
func (b *tailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf)
}
