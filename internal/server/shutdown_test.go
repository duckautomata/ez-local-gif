package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// The test binary doubles as a stand-in ffmpeg/ffprobe: when fakeToolEnv is
// set it answers "-version" with a version line and otherwise behaves per
// fakeModeEnv — by default it drops a marker file (pid + argv) into
// $fakeDirEnv and blocks until it is killed (like a long master render or a
// slow probe), so tests can hold a real child process in flight; the other
// modes imitate an ffprobe that ran but did not like the file.
const (
	fakeToolEnv  = "EZLG_TEST_FAKE_TOOL"
	fakeDirEnv   = "EZLG_TEST_FAKE_DIR"
	fakeModeEnv  = "EZLG_TEST_FAKE_MODE"
	fakeToolMax  = 60 * time.Second
	fakeModeFail = "fail"    // exit 1 with an ffprobe-style complaint on stderr
	fakeModeJunk = "junk"    // exit 0 with non-JSON on stdout
	fakeModeNoV  = "novideo" // exit 0 with JSON that has no video stream
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeToolEnv) != "" {
		os.Exit(runFakeTool())
	}
	os.Exit(m.Run())
}

func runFakeTool() int {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Println("ffmpeg version fake-ezlg-test")
			return 0
		}
	}
	switch os.Getenv(fakeModeEnv) {
	case fakeModeFail:
		fmt.Fprintln(os.Stderr, "fake ffprobe: Invalid data found when processing input")
		return 1
	case fakeModeJunk:
		fmt.Println("this is not the JSON you are looking for")
		return 0
	case fakeModeNoV:
		fmt.Println(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"mp3"}],"format":{"format_name":"mp3","duration":"1.0"}}`)
		return 0
	}
	if dir := os.Getenv(fakeDirEnv); dir != "" {
		// Written under a dot-name and renamed into place so a reader never
		// sees a half-written marker (fakeMarkers ignores dot-files).
		f, err := os.CreateTemp(dir, ".started-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake tool: marker:", err)
			return 1
		}
		fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), strings.Join(os.Args[1:], "\n"))
		f.Close()
		final := filepath.Join(dir, strings.TrimPrefix(filepath.Base(f.Name()), "."))
		if err := os.Rename(f.Name(), final); err != nil {
			fmt.Fprintln(os.Stderr, "fake tool: marker:", err)
			return 1
		}
	}
	time.Sleep(fakeToolMax) // killed long before this
	return 0
}

// fakeMarkers lists the completed marker files in dir (one per blocking
// fake process started).
func fakeMarkers(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names
}

// fakeToolExe switches this test binary into fake-tool mode for the rest of
// the test and returns its path plus the marker directory that records
// every blocking fake process start (pid on the first line, then argv).
func fakeToolExe(t *testing.T, mode string) (exe, dir string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	t.Setenv(fakeToolEnv, "1")
	t.Setenv(fakeDirEnv, dir)
	t.Setenv(fakeModeEnv, mode)
	return exe, dir
}

// fakeFFmpeg points Tools.FFmpeg at this test binary in (blocking) fake mode
// and returns the marker directory.
func fakeFFmpeg(t *testing.T) (ffrun.Tools, string) {
	t.Helper()
	exe, dir := fakeToolExe(t, "")
	return ffrun.Tools{FFmpeg: exe}, dir
}

// fakeFFprobe points Tools.FFprobe at this test binary in the given fake
// mode ("" blocks like a slow probe) and returns the marker directory.
func fakeFFprobe(t *testing.T, mode string) (ffrun.Tools, string) {
	t.Helper()
	exe, dir := fakeToolExe(t, mode)
	return ffrun.Tools{FFprobe: exe}, dir
}

// fakeArgs returns the argv recorded by the first blocking fake process
// started in the marker directory.
func fakeArgs(t *testing.T, dir string) []string {
	t.Helper()
	names := fakeMarkers(dir)
	if len(names) == 0 {
		t.Fatalf("no fake process marker in %s", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines[1:] // line 0 is the pid
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fakeStarted(dir string) func() bool {
	return func() bool { return len(fakeMarkers(dir)) > 0 }
}

// TestShutdownCancelsRunningJob: a render whose ffmpeg is alive is cancelled
// by Shutdown, its scratch dir is removed, an open SSE stream receives the
// terminal "cancelled" event and ends, and new jobs are refused with 503.
func TestShutdownCancelsRunningJob(t *testing.T) {
	tools, marker := fakeFFmpeg(t)
	e := newEnvWithTools(t, Config{}, nil, tools)
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	rec := recipe.Recipe{Sources: []string{h}, Output: recipe.Output{Format: "gif", Width: 8, Height: 8}}

	resp, body := e.postJSON(t, "/api/jobs", rec)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create job: %d %s", resp.StatusCode, body)
	}
	var job jobs.Job
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "fake ffmpeg to start", fakeStarted(marker))
	scratch := filepath.Join(e.st.Scratch, job.ID)
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch dir missing while the render runs: %v", err)
	}
	if got, _ := e.jm.Get(job.ID); got.State != jobs.StateRunning {
		t.Fatalf("job should be running: %+v", got)
	}

	// A client is watching progress.
	sresp, err := http.Get(e.srv.URL + "/api/jobs/" + job.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != 200 {
		t.Fatalf("events status %d", sresp.StatusCode)
	}
	sseDone := make(chan []jobs.Event, 1)
	go func() { sseDone <- readSSE(t, sresp.Body) }()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := e.s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Shutdown took %s; the cancelled render should wind down in well under that", d)
	}

	got, ok := e.jm.Get(job.ID)
	if !ok || got.State != jobs.StateError || got.Error != "cancelled" {
		t.Errorf("job after Shutdown = %+v (ok=%v), want error \"cancelled\"", got, ok)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scratch dir %s survived Shutdown (stat err=%v)", scratch, err)
	}
	select {
	case evs := <-sseDone:
		if len(evs) == 0 {
			t.Fatal("SSE stream ended without events")
		}
		last := evs[len(evs)-1]
		if last.Type != jobs.EventError || last.Job.Error != "cancelled" {
			t.Errorf("SSE stream ended with %+v, want error \"cancelled\"", last)
		}
	case <-time.After(sseShutdownGrace + 5*time.Second):
		t.Fatal("SSE stream did not end after Shutdown")
	}

	// No new work is accepted while shutting down.
	resp, body = e.postJSON(t, "/api/jobs", rec)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("POST /api/jobs during shutdown = %d %s, want 503", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("503 without Retry-After")
	}
	// Reads still work during the drain.
	if gresp, _ := e.get(t, "/api/jobs/"+job.ID); gresp.StatusCode != 200 {
		t.Errorf("GET job during shutdown = %d", gresp.StatusCode)
	}
	// Idempotent.
	if err := e.s.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// TestShutdownCancelsWarmStill: the upload-time preview pre-warm runs under
// the server lifetime, so Shutdown kills its ffmpeg instead of orphaning it
// (and returns promptly rather than after the 60 s still timeout).
func TestShutdownCancelsWarmStill(t *testing.T) {
	tools, marker := fakeFFmpeg(t)
	e := newEnvWithTools(t, Config{}, nil, tools)
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	blob, err := e.st.GetBlob(h)
	if err != nil {
		t.Fatal(err)
	}

	e.s.warmStill(h, *blob.Info)
	waitFor(t, 10*time.Second, "pre-warm ffmpeg to start", fakeStarted(marker))

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := e.s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Shutdown took %s waiting for the pre-warm; it should have been cancelled", d)
	}
	// Nothing new is started once shutting down.
	e.s.warmStill(h, *blob.Info)
	if n := len(fakeMarkers(marker)); n != 1 {
		t.Errorf("warmStill after Shutdown started a process (%d markers)", n)
	}
}

// TestShutdownDeadline: when background work outlives the context, Shutdown
// reports ErrShuttingDown instead of hanging.
func TestShutdownDeadline(t *testing.T) {
	e := newEnvWithTools(t, Config{}, nil, ffrun.Tools{})
	block := make(chan struct{})
	if !e.s.spawn(func() { <-block }) {
		t.Fatal("spawn refused before Shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := e.s.Shutdown(ctx)
	if !errors.Is(err, ErrShuttingDown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown with stuck work = %v, want ErrShuttingDown wrapping DeadlineExceeded", err)
	}
	if e.s.spawn(func() {}) {
		t.Error("spawn accepted work after Shutdown began")
	}
	close(block)
	if err := e.s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown after work finished: %v", err)
	}
}
