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

// RunFrom runs an arbitrary tool and hands its stdout to read as a stream
// the CALLER consumes at its own pace: read must drain the reader to EOF,
// or return an error to abort the run. RunTo cannot serve a consumer that
// is slower than the tool — exec copies stdout into the writer in its own
// goroutine and bounds that copy by WaitDelay once the process has exited,
// so a writer blocked for longer (the matte pass POSTing a batch of frames
// to the sidecar while ffmpeg has already written its last frames into the
// pipe and left) loses the rest of the stream to exec.ErrWaitDelay. Here
// the pipe is read directly: the tool blocks on a full pipe or exits once
// the pipe holds its last bytes, and every byte reaches read. A read error
// aborts the run — the tool is killed and reaped, and the read error is
// returned as is. stderr handling, cancellation (the tool killed, ctx.Err()
// returned unwrapped once read has seen the stream end) and the exit error
// shape are RunTo's.
func RunFrom(ctx context.Context, bin string, args []string, read func(stdout io.Reader) error) error {
	stderr := &tailBuffer{max: stderrTailBytes}
	if bin == "" {
		return errors.New("ffrun: no binary path given")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name := toolName(bin)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	setSysProcAttr(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	readErr := read(stdout)
	if readErr != nil {
		// Stop the tool: with its stdout closed its next write fails, and
		// the kill covers a tool that never writes again.
		_ = stdout.Close()
		_ = killTree(cmd)
	}
	err = cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if err == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if tail := stderr.Tail(stderrTailLines); tail != "" {
		return fmt.Errorf("%s exited: %w\n%s", name, err, tail)
	}
	return fmt.Errorf("%s exited: %w", name, err)
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
