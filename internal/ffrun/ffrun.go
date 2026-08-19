// Package ffrun executes external tools (ffmpeg, ffprobe, gifsicle, ...) with
// context cancellation and ffmpeg progress parsing. It is the only package
// that spawns processes; everything else builds argv.
//
// Every run: stdin is /dev/null, stderr is kept in a bounded tail buffer
// that is attached to the returned error, the process (and, on Unix, its
// whole process group) is killed when ctx is cancelled, and Wait is bounded
// by exec.Cmd.WaitDelay so a straggling grandchild holding our pipes cannot
// hang the caller.
package ffrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// waitDelay bounds how long Wait blocks after cancellation or exit for
	// I/O pipes that are still open (see exec.Cmd.WaitDelay).
	waitDelay = 2 * time.Second
	// stderrTailBytes is the size of the per-run stderr ring buffer.
	stderrTailBytes = 64 << 10
	// stderrTailLines is how many trailing stderr lines an error carries.
	stderrTailLines = 40
	// versionTimeout bounds each tool's version probe in Versions.
	versionTimeout = 3 * time.Second
	// versionMaxBytes caps how much version output is buffered per tool.
	versionMaxBytes = 4 << 10
)

// ffmpegPrefix is prepended to every ffmpeg invocation.
var ffmpegPrefix = []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error"}

// ffmpegProgressArgs is appended when progress reporting is requested.
var ffmpegProgressArgs = []string{"-progress", "pipe:1", "-nostats", "-stats_period", "0.2"}

// Tools holds resolved binary paths. Empty string = not available.
type Tools struct {
	FFmpeg   string
	FFprobe  string
	Gifsicle string
	Gifski   string
	Img2webp string
	Webpinfo string
	Avifenc  string
	Avifdec  string
	Pngquant string
	Oxipng   string
}

// toolSpec describes one external tool: its PATH name (also the key used by
// Versions), the environment override, the args that print its version and
// where it lives in Tools.
type toolSpec struct {
	name    string
	env     string
	version []string
	field   func(*Tools) *string
}

// toolSpecs is the single source of truth for LookupTools and Versions.
var toolSpecs = []toolSpec{
	{"ffmpeg", "EZLG_FFMPEG", []string{"-version"}, func(t *Tools) *string { return &t.FFmpeg }},
	{"ffprobe", "EZLG_FFPROBE", []string{"-version"}, func(t *Tools) *string { return &t.FFprobe }},
	{"gifsicle", "EZLG_GIFSICLE", []string{"--version"}, func(t *Tools) *string { return &t.Gifsicle }},
	{"gifski", "EZLG_GIFSKI", []string{"--version"}, func(t *Tools) *string { return &t.Gifski }},
	{"img2webp", "EZLG_IMG2WEBP", []string{"-version"}, func(t *Tools) *string { return &t.Img2webp }},
	{"webpinfo", "EZLG_WEBPINFO", []string{"-version"}, func(t *Tools) *string { return &t.Webpinfo }},
	// libavif's CLIs take -V/--version; a single-dash -version is rejected
	// as an unknown option and only prints usage.
	{"avifenc", "EZLG_AVIFENC", []string{"--version"}, func(t *Tools) *string { return &t.Avifenc }},
	{"avifdec", "EZLG_AVIFDEC", []string{"--version"}, func(t *Tools) *string { return &t.Avifdec }},
	{"pngquant", "EZLG_PNGQUANT", []string{"--version"}, func(t *Tools) *string { return &t.Pngquant }},
	{"oxipng", "EZLG_OXIPNG", []string{"--version"}, func(t *Tools) *string { return &t.Oxipng }},
}

// LookupTools resolves each tool from the environment variable EZLG_<NAME>
// (e.g. EZLG_FFMPEG=/usr/local/bin/ffmpeg) or, failing that, from PATH via
// exec.LookPath. Missing tools are left empty; callers decide what is
// required (ffmpeg, ffprobe and gifsicle are required for Phase 1).
//
// An environment override is used verbatim (it is not checked for
// existence) so a misconfiguration surfaces as a clear exec error naming
// the path rather than as a silently missing tool.
func LookupTools() Tools {
	var t Tools
	for _, spec := range toolSpecs {
		dst := spec.field(&t)
		if v := strings.TrimSpace(os.Getenv(spec.env)); v != "" {
			*dst = v
			continue
		}
		if p, err := exec.LookPath(spec.name); err == nil {
			*dst = p
		}
	}
	return t
}

// Versions runs each available tool with its version flag and returns a
// map of tool name → first line of output (best effort; used by
// /api/capabilities).
//
// Every configured tool gets a key; the value is "" when the tool produced
// no output within 3 s (or could not be started). Tools are probed
// concurrently so the whole call is bounded by one timeout.
func (t Tools) Versions(ctx context.Context) map[string]string {
	out := make(map[string]string, len(toolSpecs))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, spec := range toolSpecs {
		bin := *spec.field(&t)
		if bin == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := versionLine(ctx, bin, spec.version)
			mu.Lock()
			out[spec.name] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// versionLine runs bin with args and returns the first non-empty line it
// prints (stdout preferred, then stderr), or "" on any failure.
func versionLine(ctx context.Context, bin string, args []string) string {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	stdout := &headBuffer{max: versionMaxBytes}
	stderr := &headBuffer{max: versionMaxBytes}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = waitDelay
	setSysProcAttr(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	_ = cmd.Run() // best effort: some tools exit non-zero after printing usage+version
	if l := firstLine(stdout.buf); l != "" {
		return l
	}
	return firstLine(stderr.buf)
}

// firstLine returns the first non-blank line of b, trimmed.
func firstLine(b []byte) string {
	for _, l := range bytes.Split(b, []byte{'\n'}) {
		if s := strings.TrimSpace(string(l)); s != "" {
			return s
		}
	}
	return ""
}

// Progress is one parsed block of `ffmpeg -progress pipe:1` output.
type Progress struct {
	Frame     int     // frames processed so far
	FPS       float64 // current encoding speed in frames/s
	OutTimeMS int64   // output timestamp in milliseconds (out_time_us / 1000)
	Speed     string  // e.g. "3.2x"
	Done      bool    // progress=end seen
}

// ErrNotImplemented is kept for API stability; nothing in this package
// returns it any more.
var ErrNotImplemented = errors.New("ffrun: not implemented")

// RunFFmpeg runs ffmpeg with args plus the standard prefix
// (-hide_banner -nostdin -y -loglevel error) and, when onProgress is non-nil,
// appends -progress pipe:1 -nostats -stats_period 0.2 and streams parsed
// progress blocks to onProgress. stdout must therefore not be used for output
// when onProgress is set (use RunOutput for image2pipe/pipe:1 cases). On
// failure the returned error includes the last ~40 lines of stderr.
// Cancelling ctx kills the process.
//
// onProgress is called synchronously from the goroutine draining ffmpeg's
// stdout, at most every stats period; the final call has Done set. It must
// not block for long or ffmpeg stalls on a full pipe.
func RunFFmpeg(ctx context.Context, ffmpeg string, args []string, onProgress func(Progress)) error {
	argv := make([]string, 0, len(ffmpegPrefix)+len(args)+len(ffmpegProgressArgs))
	argv = append(argv, ffmpegPrefix...)
	argv = append(argv, args...)
	var stdout io.Writer = io.Discard
	if onProgress != nil {
		argv = append(argv, ffmpegProgressArgs...)
		stdout = newProgressWriter(onProgress)
	}
	return RunTo(ctx, ffmpeg, argv, stdout)
}

// RunOutput runs an arbitrary tool and returns its stdout. stderr is
// captured and included in the error on non-zero exit. Cancelling ctx kills
// the process. Like exec.Cmd.Output, whatever stdout was captured before a
// failure is returned alongside the error.
func RunOutput(ctx context.Context, bin string, args []string) ([]byte, error) {
	var out bytes.Buffer
	err := RunTo(ctx, bin, args, &out)
	return out.Bytes(), err
}

// Run runs an arbitrary tool discarding stdout (stderr captured for errors).
func Run(ctx context.Context, bin string, args []string) error {
	return RunTo(ctx, bin, args, io.Discard)
}

// RunTo runs an arbitrary tool streaming its stdout into w (nil discards
// it), so large outputs — a rawvideo alpha scan, PNG frames on
// image2pipe — need not be buffered in memory. stderr handling,
// cancellation and error wrapping are the same as for RunOutput: on a
// non-zero exit the error reads "<tool> exited: <exit error>\n<stderr
// tail>", and errors.As still reaches the *exec.ExitError. When ctx is
// cancelled or times out, ctx.Err() is returned unwrapped.
func RunTo(ctx context.Context, bin string, args []string, w io.Writer) error {
	return runTo(ctx, bin, args, w)
}

// toolName is the short name used in error messages: the base name without
// a Windows-style extension, so messages read "ffmpeg exited: …" on every
// platform.
func toolName(bin string) string {
	name := filepath.Base(bin)
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".exe") {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

func runTo(ctx context.Context, bin string, args []string, w io.Writer) error {
	if bin == "" {
		return errors.New("ffrun: no binary path given")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name := toolName(bin)
	stderr := &tailBuffer{max: stderrTailBytes}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = w
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	setSysProcAttr(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	err := cmd.Wait()
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

// tailBuffer is an io.Writer that keeps only the last max bytes written.
// exec drives it from a single goroutine and the tail is read after Wait,
// but it is locked anyway so misuse can never race.
type tailBuffer struct {
	mu      sync.Mutex
	max     int
	buf     []byte
	dropped bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.max {
		b.buf = append(b.buf[:0], p[len(p)-b.max:]...)
		b.dropped = true
		return len(p), nil
	}
	if over := len(b.buf) + len(p) - b.max; over > 0 {
		n := copy(b.buf, b.buf[over:])
		b.buf = b.buf[:n]
		b.dropped = true
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// Tail returns the last n lines written (CR/LF trimmed), or "". When the
// buffer wrapped, the first line returned may be a partial one.
func (b *tailBuffer) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimLeft(strings.TrimRight(string(b.buf), "\r\n\t "), "\r\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	return strings.Join(lines, "\n")
}

// headBuffer is an io.Writer that keeps only the first max bytes written.
type headBuffer struct {
	max int
	buf []byte
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
