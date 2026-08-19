package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// The test binary doubles as a stand-in ffmpeg/ffprobe (see fakeTool): with
// fakeToolEnv set it answers "-version" and otherwise drops a marker file
// into $fakeDirEnv and blocks until killed, like a long render.
const (
	fakeToolEnv = "EZLG_TEST_FAKE_TOOL"
	fakeDirEnv  = "EZLG_TEST_FAKE_DIR"
	fakeToolMax = 60 * time.Second
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeToolEnv) != "" {
		os.Exit(fakeTool())
	}
	os.Exit(m.Run())
}

func fakeTool() int {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Println("ffmpeg version fake-ezlg-test")
			return 0
		}
	}
	if dir := os.Getenv(fakeDirEnv); dir != "" {
		f, err := os.CreateTemp(dir, "started-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake tool: marker:", err)
			return 1
		}
		fmt.Fprintf(f, "%d\n", os.Getpid())
		f.Close()
	}
	time.Sleep(fakeToolMax) // killed long before this
	return 0
}

// testConfig returns a serveConfig rooted in a temp dir with no sweeper and
// a drain long enough that only a genuinely stuck shutdown reaches it.
func testConfig(t *testing.T) serveConfig {
	t.Helper()
	root := t.TempDir()
	return serveConfig{
		dataRoot:  filepath.Join(root, "data"),
		scratch:   filepath.Join(root, "scratch"),
		maxUpload: 1 << 20,
		conc:      1,
		drain:     15 * time.Second,
	}
}

// startServer runs runServer on a loopback listener and returns the base
// URL, the cancel that plays the role of SIGTERM, and a wait that reports
// runServer's result (ok=false if it has not returned within timeout).
func startServer(t *testing.T, cfg serveConfig) (base string, cancel context.CancelFunc, wait func(timeout time.Duration) (err error, ok bool)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelFn := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var runErr error
	go func() {
		runErr = runServer(ctx, cfg, ln)
		close(finished)
	}()
	wait = func(timeout time.Duration) (error, bool) {
		select {
		case <-finished:
			return runErr, true
		case <-time.After(timeout):
			return nil, false
		}
	}
	base = "http://" + ln.Addr().String()
	t.Cleanup(func() {
		cancelFn()
		if _, ok := wait(cfg.drain + 5*time.Second); !ok {
			t.Error("runServer did not return after cancel")
		}
	})
	waitFor(t, 10*time.Second, "server to answer /healthz", func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return base, cancelFn, wait
}

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

func dirHasEntries(dir string) func() bool {
	return func() bool {
		entries, _ := os.ReadDir(dir)
		return len(entries) > 0
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.NRGBA{R: 10, G: 200, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestRunServerDrainsInFlightUpload: the shutdown signal arrives while an
// upload is streaming in; the upload must still complete with 200 and only
// then may runServer return — and it must return well before the drain
// timeout, since nothing else is in flight.
func TestRunServerDrainsInFlightUpload(t *testing.T) {
	tools := ffrun.LookupTools()
	if tools.FFmpeg == "" || tools.FFprobe == "" {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	cfg := testConfig(t)
	base, cancel, wait := startServer(t, cfg)
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	// The multipart body arrives in two halves; the second is only written
	// after the shutdown signal.
	data := tinyPNG(t)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	release := make(chan struct{})
	go func() {
		defer pw.Close()
		fw, err := mw.CreateFormFile("file", "tiny.png")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := fw.Write(data[:len(data)/2]); err != nil {
			return
		}
		<-release
		fw.Write(data[len(data)/2:])
		mw.Close()
	}()
	type result struct {
		resp *http.Response
		body []byte
		err  error
		at   time.Time
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := client.Post(base+"/api/upload", mw.FormDataContentType(), pr)
		var body []byte
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		resCh <- result{resp, body, err, time.Now()}
	}()

	// The handler is streaming the body once the store's temp part exists.
	waitFor(t, 10*time.Second, "upload to start streaming", dirHasEntries(filepath.Join(cfg.dataRoot, "tmp")))

	signalled := time.Now()
	cancel() // SIGTERM
	if err, ok := wait(300 * time.Millisecond); ok {
		t.Fatalf("runServer returned (%v) while an upload was in flight", err)
	}
	close(release)

	var res result
	select {
	case res = <-resCh:
	case <-time.After(cfg.drain + 5*time.Second):
		t.Fatal("upload never completed")
	}
	if res.err != nil {
		t.Fatalf("upload during shutdown failed: %v", res.err)
	}
	if res.resp.StatusCode != 200 {
		t.Fatalf("upload during shutdown = %d %s, want 200", res.resp.StatusCode, res.body)
	}
	var src recipe.Source
	if err := json.Unmarshal(res.body, &src); err != nil || src.Hash == "" {
		t.Fatalf("upload response %q: %v", res.body, err)
	}

	err, ok := wait(cfg.drain + 5*time.Second)
	if !ok {
		t.Fatal("runServer did not return")
	}
	returned := time.Now()
	if err != nil {
		t.Errorf("runServer = %v, want nil after a clean drain", err)
	}
	if returned.Before(res.at) {
		t.Error("runServer returned before the in-flight upload got its response")
	}
	if d := returned.Sub(signalled); d > cfg.drain/2 {
		t.Errorf("shutdown took %s with only a tiny upload in flight; the drain timeout should not be burned", d)
	}
	// The listener is gone.
	if resp, err := client.Get(base + "/healthz"); err == nil {
		resp.Body.Close()
		t.Error("server still answering after runServer returned")
	}
	// The upload landed (no stray temp part) and the blob is on disk.
	if entries, _ := os.ReadDir(filepath.Join(cfg.dataRoot, "tmp")); len(entries) != 0 {
		t.Errorf("upload temp parts left behind: %d", len(entries))
	}
	st, err := store.New(cfg.dataRoot, cfg.scratch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(src.Hash); err != nil {
		t.Errorf("uploaded blob missing after shutdown: %v", err)
	}
}

// TestRunServerCancelsRenderOnShutdown: a render whose ffmpeg is alive when
// the signal arrives is cancelled, its ffmpeg killed and scratch dir
// removed, and a client watching its progress stream gets the terminal
// "cancelled" event; runServer returns long before the drain timeout.
func TestRunServerCancelsRenderOnShutdown(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := t.TempDir()
	t.Setenv(fakeToolEnv, "1")
	t.Setenv(fakeDirEnv, marker)
	t.Setenv("EZLG_FFMPEG", exe)
	t.Setenv("EZLG_FFPROBE", exe)
	cfg := testConfig(t)

	// Seed a probed source in the store the server is about to open.
	st, err := store.New(cfg.dataRoot, cfg.scratch)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := st.PutBlob(strings.NewReader("GIF89a not really a gif"), "a.gif")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8, Width: 16, Height: 12, FPS: 10, Duration: 0.6, Frames: 6, Kind: recipe.KindAnimation, HasAlpha: true}
	if err := st.SetBlobInfo(blob.Hash, info); err != nil {
		t.Fatal(err)
	}

	base, cancel, wait := startServer(t, cfg)
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	rec := recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif", Width: 8, Height: 8}}
	body, _ := json.Marshal(rec)
	resp, err := client.Post(base+"/api/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create job: %d %s", resp.StatusCode, out)
	}
	var job jobs.Job
	if err := json.Unmarshal(out, &job); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "fake ffmpeg to start", dirHasEntries(marker))
	scratch := filepath.Join(st.Scratch, job.ID)
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch dir missing while the render runs: %v", err)
	}

	// A client is watching progress.
	sresp, err := client.Get(base + "/api/jobs/" + job.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	sseCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(sresp.Body) // ends when the server closes the stream
		sseCh <- string(b)
	}()

	signalled := time.Now()
	cancel() // SIGTERM
	err, ok := wait(cfg.drain + 5*time.Second)
	if !ok {
		t.Fatal("runServer did not return")
	}
	if err != nil {
		t.Errorf("runServer = %v", err)
	}
	if d := time.Since(signalled); d > cfg.drain/2 {
		t.Errorf("shutdown took %s; the cancelled render should not burn the drain timeout", d)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scratch dir %s survived shutdown (stat err=%v)", scratch, err)
	}
	select {
	case sse := <-sseCh:
		if !strings.Contains(sse, "event: error") || !strings.Contains(sse, `"cancelled"`) {
			t.Errorf("progress stream did not end with the cancelled event:\n%s", sse)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("progress stream did not end")
	}
}
