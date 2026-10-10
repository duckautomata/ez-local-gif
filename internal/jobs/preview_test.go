package jobs

// Preview admission, the preview semaphore / in-flight sharing, the
// byte-bounded memo directories and the detached autocrop pass. The
// process-level checks run against a fake ffmpeg: this test binary in
// fake-tool mode (TestMain), which records its argv in a marker file and
// blocks until a "release" file appears, then answers like ffmpeg would — a
// PNG on stdout for a still (image2pipe), a WebP file for a proxy, a bbox
// line on stderr for a crop detection.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

const (
	fakeToolEnv = "EZLG_JOBS_TEST_FAKE_TOOL"
	fakeDirEnv  = "EZLG_JOBS_TEST_FAKE_DIR"
	fakeRelease = "release"
	fakeToolMax = 60 * time.Second
	// fakePNG / fakeWebP are what the fake ffmpeg produces.
	fakePNG  = "\x89PNG\r\n\x1a\nfake-still"
	fakeWebP = "RIFF\x00\x00\x00\x00WEBPfake-proxy"
	// fakeBBoxLine is the detector line the fake prints for a detection run
	// (enc.ParseCropDetect reads "[Parsed_bbox_" lines): a 24x24 box at
	// (16,12).
	fakeBBoxLine = "[Parsed_bbox_2 @ 0x1] n:0 pts:0 pts_time:0 x1:16 x2:39 y1:12 y2:35 w:24 h:24 crop=24:24:16:12 crop=24:24:16:12\n"
)

func TestMain(m *testing.M) {
	// Fake gifsicle mode (holds_test.go). When both fakes are configured the
	// argv decides which tool this process stands in for.
	if dir := os.Getenv(fakeGifsicleEnv); dir != "" && (os.Getenv(fakeToolEnv) == "" || isGifsicleArgv(os.Args[1:])) {
		os.Exit(runFakeGifsicle(dir))
	}
	if os.Getenv(fakeToolEnv) != "" {
		os.Exit(runFakeFFmpeg())
	}
	os.Exit(m.Run())
}

// runFakeFFmpeg is the fake tool: marker, wait for the release file, answer.
func runFakeFFmpeg() int {
	args := os.Args[1:]
	for _, a := range args {
		if a == "-version" {
			fmt.Println("ffmpeg version fake-ezlg-jobs-test")
			return 0
		}
	}
	dir := os.Getenv(fakeDirEnv)
	f, err := os.CreateTemp(dir, ".started-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake ffmpeg: marker:", err)
		return 1
	}
	fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), strings.Join(args, "\n"))
	f.Close()
	if err := os.Rename(f.Name(), filepath.Join(dir, strings.TrimPrefix(filepath.Base(f.Name()), "."))); err != nil {
		fmt.Fprintln(os.Stderr, "fake ffmpeg: marker:", err)
		return 1
	}
	deadline := time.Now().Add(fakeToolMax)
	for {
		if _, err := os.Stat(filepath.Join(dir, fakeRelease)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "fake ffmpeg: never released")
			return 1
		}
		time.Sleep(5 * time.Millisecond)
	}
	switch {
	case hasArg(args, "pipe:1"):
		os.Stdout.WriteString(fakePNG)
	case hasArg(args, "null"):
		os.Stderr.WriteString(fakeBBoxLine)
	default:
		// fakeOutEnv names a file whose bytes are the encode's output (the
		// fake proxy otherwise).
		payload := []byte(fakeWebP)
		if src := os.Getenv(fakeOutEnv); src != "" {
			if payload, err = os.ReadFile(src); err != nil {
				fmt.Fprintln(os.Stderr, "fake ffmpeg: payload:", err)
				return 1
			}
		}
		if err := os.WriteFile(args[len(args)-1], payload, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fake ffmpeg: output:", err)
			return 1
		}
	}
	return 0
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// fakeFFmpegTools switches this test binary into fake-ffmpeg mode for the
// rest of the test and returns the tools plus the marker directory.
func fakeFFmpegTools(t *testing.T) (ffrun.Tools, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(fakeToolEnv, "1")
	t.Setenv(fakeDirEnv, dir)
	return ffrun.Tools{FFmpeg: exe}, dir
}

// fakeMarkers lists the fake processes started so far (one marker each).
func fakeMarkers(dir string) int {
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") && e.Name() != fakeRelease {
			n++
		}
	}
	return n
}

// releaseFakes lets every fake process (running or yet to start) finish.
func releaseFakes(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fakeRelease), []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMemoByteEviction(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	put := func(name string, size int, age int) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte{1}, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(age) * time.Minute)
		os.Chtimes(p, mt, mt)
	}
	names := func() string {
		entries, _ := os.ReadDir(dir)
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return strings.Join(out, ",")
	}
	for i := 0; i < 5; i++ {
		put(fmt.Sprintf("e%d.png", i), 100, i) // e0 oldest
	}
	put(".memo-tmp", 1000, 0) // a write in progress is never counted or evicted
	// Within both bounds: nothing happens.
	if err := evictOldest(dir, 5, 500); err != nil || names() != ".memo-tmp,e0.png,e1.png,e2.png,e3.png,e4.png" {
		t.Fatalf("within bounds: %v %s", err, names())
	}
	// Byte bound only: the oldest go until the rest fit.
	if err := evictOldest(dir, -1, 250); err != nil || names() != ".memo-tmp,e3.png,e4.png" {
		t.Fatalf("byte bound: %v %s", err, names())
	}
	// Count bound only.
	if err := evictOldest(dir, 1, 0); err != nil || names() != ".memo-tmp,e4.png" {
		t.Fatalf("count bound: %v %s", err, names())
	}
	// Everything.
	if err := evictOldest(dir, 0, 0); err != nil || names() != ".memo-tmp" {
		t.Fatalf("keep nothing by count: %v %s", err, names())
	}

	// memoWrite evicts BEFORE writing so the directory never exceeds its
	// bounds, by bytes and by count, and the new entry is the one kept.
	st := newTestStore(t)
	m := NewManager(st, ffrun.Tools{}, Options{MaxStillsBytes: 250})
	if m.MaxStillsBytes() != 250 || m.MaxProxyBytes() != DefaultMaxProxyBytes {
		t.Errorf("memo bounds = %d / %d", m.MaxStillsBytes(), m.MaxProxyBytes())
	}
	memo := filepath.Join(st.Scratch, stillsDir)
	total := func() (n int, size int64) {
		entries, _ := os.ReadDir(memo)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !strings.HasPrefix(e.Name(), ".") {
				n++
				size += info.Size()
			}
		}
		return n, size
	}
	for i := 0; i < 6; i++ {
		m.memoWrite(filepath.Join(memo, fmt.Sprintf("k%d.png", i)), bytes.Repeat([]byte{2}, 100), MaxStills, m.opts.MaxStillsBytes)
		if n, size := total(); size > 250 || n > 2 {
			t.Fatalf("after write %d: %d entries, %d bytes (bound 250)", i, n, size)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(filepath.Join(memo, fmt.Sprintf("k%d.png", i)), mt, mt)
	}
	if _, err := os.Stat(filepath.Join(memo, "k5.png")); err != nil {
		t.Errorf("the newest entry was evicted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(memo, "k0.png")); err == nil {
		t.Error("the oldest entry survived")
	}
	// The count bound: keep entries at most, the newest among them.
	for i := 0; i < 4; i++ {
		m.memoWrite(filepath.Join(memo, fmt.Sprintf("c%d.png", i)), []byte{3}, 2, 0)
	}
	if n, _ := total(); n != 2 {
		t.Errorf("count bound: %d entries, want 2", n)
	}
	// An entry larger than the byte bound still lands (alone).
	m.memoWrite(filepath.Join(memo, "big.png"), bytes.Repeat([]byte{4}, 400), MaxStills, 250)
	if n, size := total(); n != 1 || size != 400 {
		t.Errorf("oversize entry: %d entries, %d bytes", n, size)
	}
}

// TestPreviewAdmissionReversed: a reversed plan whose reverse stage would
// buffer more than MaxMasterBytes is refused by Still and Proxy before any
// ffmpeg starts (ErrInvalidRecipe naming EZLG_MAX_MASTER_BYTES); the same
// plan without the reverse reaches ffmpeg. The still counts the whole
// trimmed clip, the proxy only the tail enc.ProxyArgs seeks to.
func TestPreviewAdmissionReversed(t *testing.T) {
	st := newTestStore(t)
	b, err := st.PutBlob(bytes.NewReader([]byte("a 1080p clip, allegedly")), "big.mp4")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", PixFmt: "yuv420p", Bits: 8,
		Width: 1920, Height: 1080, FPS: 30, Duration: 20, Frames: 600, Kind: recipe.KindVideo}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	src := b.Hash
	reverse := []recipe.Op{{Kind: recipe.OpReverse}}
	out := recipe.Output{Format: "gif"}
	ctx := context.Background()

	// The plan compiles (the graph caps no frame count) at 4.6 GiB of
	// reverse buffer, over the jobs 2 GiB default; the proxy's tail (about
	// 10 s + the seek-back at 30 fps) is 2.4 GiB, over the default too.
	plan, err := graph.Compile(info, reverse, stillOutput(out))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !plan.Reversed || plan.Frames != 600 {
		t.Fatalf("plan = %+v", plan)
	}
	if masterBytes(plan) <= DefaultMaxMasterBytes {
		t.Fatalf("master %d must exceed the default cap", masterBytes(plan))
	}
	tail := proxyBufferFrames(plan, enc.ProxyArgs(b.Path, plan, 360, 10, "p.webp"))
	if tail <= 0 || tail >= plan.Frames || tail < 300 || tail > 320 {
		t.Fatalf("proxy tail = %d frames, want about 10 s at 30 fps plus the seek-back (enc.ProxyArgs = %q)", tail, enc.ProxyArgs(b.Path, plan, 360, 10, "p.webp"))
	}
	if got := proxyBufferFrames(plan, []string{"-i", "x"}); got != plan.Frames {
		t.Errorf("unseeked argv must count the whole plan: %d", got)
	}
	if got := proxyBufferFrames(plan, []string{"-ss", "19", "-to", "20", "-i", "x"}); got < 30 || got > 32 {
		t.Errorf("a 1 s tail = %d frames, want 31", got)
	}
	if got := proxyBufferFrames(&graph.Plan{Width: 8, Height: 8}, []string{"-ss", "1", "-i", "x"}); got != 0 {
		t.Errorf("unknown frame count = %d, want 0", got)
	}
	if got := proxyBufferFrames(plan, []string{"-c:v", "libvpx", "-i", "x", "-ss", "5"}); got != plan.Frames {
		t.Errorf("an -ss after the input is an output option, not a seek: %d", got)
	}

	m := NewManager(st, fakeTools, Options{})
	check := func(t *testing.T, what string, err error, refused bool) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		if refused {
			if !errors.Is(err, ErrInvalidRecipe) {
				t.Errorf("%s: %v (want ErrInvalidRecipe)", what, err)
			}
			for _, want := range []string{"EZLG_MAX_MASTER_BYTES", "reverse buffer", "trim the clip", "1920x1080"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error %q lacks %q", what, err, want)
				}
			}
			if strings.Contains(err.Error(), "ffmpeg") {
				t.Errorf("%s: ffmpeg was spawned: %v", what, err)
			}
			return
		}
		if errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "ffmpeg") {
			t.Errorf("%s: %v (want the fake ffmpeg to be reached)", what, err)
		}
	}
	_, err = m.Still(ctx, src, reverse, out, 0.5, 0)
	check(t, "reversed still", err, true)
	_, err = m.Proxy(ctx, []string{src}, reverse, out, 0, 0)
	check(t, "reversed proxy", err, true)
	_, err = m.Still(ctx, src, nil, out, 0.5, 0)
	check(t, "forward still", err, false)
	_, err = m.Proxy(ctx, []string{src}, nil, out, 0, 0)
	check(t, "forward proxy", err, false)
	if entries, _ := os.ReadDir(st.Scratch); len(entries) != 0 {
		t.Errorf("scratch has %d entries after refused/failed previews", len(entries))
	}

	// A cap between the proxy's tail and the whole clip: the render and the
	// still (whole clip) are refused, the proxy (tail) is admitted.
	m3 := NewManager(st, fakeTools, Options{MaxMasterBytes: 3 << 30})
	_, err = m3.Still(ctx, src, reverse, out, 0.5, 0)
	check(t, "reversed still at 3 GiB", err, true)
	_, err = m3.Proxy(ctx, []string{src}, reverse, out, 0, 0)
	check(t, "reversed proxy at 3 GiB", err, false)
	j, err := m3.Submit(recipe.Recipe{Sources: []string{src}, Ops: reverse, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if fin := waitFinished(t, m3, j.ID); fin.State != StateError || !strings.Contains(fin.Error, "EZLG_MAX_MASTER_BYTES") {
		t.Errorf("render at 3 GiB: %+v", fin)
	}
	// A longer proxy shows more of the tail: at 30 s (the cap) the tail is
	// the whole clip again.
	_, err = m3.Proxy(ctx, []string{src}, reverse, out, 0, 30)
	check(t, "reversed 30 s proxy at 3 GiB", err, true)
	// Trimmed to 5 s the clip fits every cap.
	trimmed := []recipe.Op{{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":10,"end":15}`)}, {Kind: recipe.OpReverse}}
	_, err = m.Still(ctx, src, trimmed, out, 0.5, 0)
	check(t, "trimmed reversed still", err, false)
	_, err = m.Proxy(ctx, []string{src}, trimmed, out, 0, 0)
	check(t, "trimmed reversed proxy", err, false)
}

// bigSource stores a blob probed as the 2026-09-13 report's source — a
// 2560x1440 30 fps 23.47 s H.264 clip, 704 frames = 9.7 GiB of RGBA — and
// returns its hash. Nothing decodes it: the tests below only need a plan
// whose untrimmed master is far over the 2 GiB default.
func bigSource(t *testing.T, st *store.Store) string {
	t.Helper()
	b, err := st.PutBlob(bytes.NewReader([]byte("a 1440p clip, allegedly")), "big1440.mp4")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", PixFmt: "yuv420p", Bits: 8,
		Width: 2560, Height: 1440, FPS: 30, Duration: 23.4667, Frames: 704, Kind: recipe.KindVideo}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	return b.Hash
}

// checkAdmission asserts on the error of a still/proxy call: refused means
// ErrInvalidRecipe naming EZLG_MAX_MASTER_BYTES (and each extra want) with
// no ffmpeg spawned; otherwise the fake ffmpeg must have been reached (the
// error mentions ffmpeg and is not an ErrInvalidRecipe).
func checkAdmission(t *testing.T, what string, err error, refused bool, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	if refused {
		if !errors.Is(err, ErrInvalidRecipe) {
			t.Errorf("%s: %v (want ErrInvalidRecipe)", what, err)
		}
		for _, want := range append([]string{"EZLG_MAX_MASTER_BYTES"}, wants...) {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q lacks %q", what, err, want)
			}
		}
		if strings.Contains(err.Error(), "ffmpeg") {
			t.Errorf("%s: ffmpeg was spawned: %v", what, err)
		}
		return
	}
	if errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("%s: %v (want the fake ffmpeg to be reached)", what, err)
	}
}

// TestPreviewAdmissionForwardLargeMaster (2026-09-13 report): a source
// whose untrimmed master is far over the cap — 2560x1440 x 704 frames =
// 9.7 GiB — must still preview so it can be trimmed/cropped/fitted in-app.
// The graph no longer refuses such a plan at compile time, and forward
// stills/proxies build no master, so Still and Proxy of the untouched clip
// reach ffmpeg; only Submit — the render, which would build the master —
// is refused, naming the knob. With the editing the report wanted (trim to
// 3 s, crop, gif at 128x128) the render's master is 90 x 128 x 128 x 4 =
// 5.6 MiB and its admission passes too; the only failure left is the fake
// ffmpeg. No refusal or failure leaves anything on scratch.
func TestPreviewAdmissionForwardLargeMaster(t *testing.T) {
	st := newTestStore(t)
	src := bigSource(t, st)
	out := recipe.Output{Format: "gif"}
	ctx := context.Background()
	m := NewManager(st, fakeTools, Options{Concurrency: 1})

	// Sanity on the plan the manager compiles: 9.7 GiB, over the default.
	plan, err := graph.Compile(mustInfo(t, st, src), nil, stillOutput(out))
	if err != nil {
		t.Fatalf("compile: %v (the graph must not cap the frame count)", err)
	}
	if plan.Frames != 704 || plan.Width != 2560 || plan.Height != 1440 {
		t.Fatalf("plan = %dx%d x %d frames", plan.Width, plan.Height, plan.Frames)
	}
	if got := masterBytes(plan); got != 10380902400 || got <= DefaultMaxMasterBytes || humanBytes(got) != "9.7 GiB" {
		t.Fatalf("masterBytes = %d (%s), want 10380902400 (9.7 GiB), over the default cap", got, humanBytes(got))
	}

	_, err = m.Still(ctx, src, nil, out, 0.5, 0)
	checkAdmission(t, "forward still of the untrimmed clip", err, false)
	_, err = m.Proxy(ctx, []string{src}, nil, out, 0, 0)
	checkAdmission(t, "forward proxy of the untrimmed clip", err, false)

	j, err := m.Submit(recipe.Recipe{Sources: []string{src}, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitFinished(t, m, j.ID)
	if fin.State != StateError {
		t.Fatalf("render of the untrimmed clip: %+v, want the cap refusal", fin)
	}
	for _, want := range []string{"frame master would need", "9.7 GiB", "704 frames of 2560x1440", "EZLG_MAX_MASTER_BYTES", ErrInvalidRecipe.Error()} {
		if !strings.Contains(fin.Error, want) {
			t.Errorf("render error %q lacks %q", fin.Error, want)
		}
	}
	if strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("render of the untrimmed clip spawned ffmpeg: %q", fin.Error)
	}

	// The report's intended edit: trim + crop, fitted to an emote.
	edited := []recipe.Op{
		{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0,"end":3}`)},
		{Kind: recipe.OpCrop, Params: json.RawMessage(`{"x":0,"y":0,"w":800,"h":600}`)},
	}
	emote := recipe.Output{Format: "gif", Width: 128, Height: 128}
	if p, err := graph.Compile(mustInfo(t, st, src), edited, emote); err != nil {
		t.Fatalf("compile edited: %v", err)
	} else if p.Frames != 90 || p.Width != 128 || p.Height != 128 || masterBytes(p) > DefaultMaxMasterBytes {
		t.Fatalf("edited plan = %dx%d x %d frames (%s)", p.Width, p.Height, p.Frames, humanBytes(masterBytes(p)))
	}
	_, err = m.Still(ctx, src, edited, emote, 0.5, 0)
	checkAdmission(t, "edited still", err, false)
	_, err = m.Proxy(ctx, []string{src}, edited, emote, 0, 0)
	checkAdmission(t, "edited proxy", err, false)
	j, err = m.Submit(recipe.Recipe{Sources: []string{src}, Ops: edited, Output: emote})
	if err != nil {
		t.Fatal(err)
	}
	if fin := waitFinished(t, m, j.ID); fin.State != StateError || !strings.Contains(fin.Error, "ffmpeg") || strings.Contains(fin.Error, "EZLG_MAX_MASTER_BYTES") {
		t.Errorf("edited render should pass admission and reach the fake ffmpeg: %+v", fin)
	}
	if m.scratch.Used() != 0 {
		t.Errorf("budget not released: %d", m.scratch.Used())
	}
	if entries, _ := os.ReadDir(st.Scratch); len(entries) != 0 {
		t.Errorf("scratch has %d entries after refused/failed previews and renders", len(entries))
	}
}

// TestStaticReversedRenderAdmission: a png/jpeg render is cut to one frame
// (oneFramePlan) before its scratch admission, but "-frames:v 1" shortens
// the encode, not the decode — a reversed static export still buffers the
// whole trimmed clip in ffmpeg's reverse filter. With the graph's
// compile-time cap gone that buffer must be admitted here: the untrimmed
// 1440p clip's reversed PNG (704 frames = 9.7 GiB) and its reversed-then-
// bounced PNG ([reverse, bounce]: the reverse consumes the clip before the
// split sees a frame; the doubled 1408 frames are the conservative bound —
// 19 GiB) are refused naming the knob and the reverse buffer without an
// ffmpeg run; the reverse trimmed to 3 s (90 frames = 1.3 GiB) is admitted
// and reaches the fake ffmpeg. A bounce ALONE buffers nothing for a static
// render — the first output frame is the forward branch's and -frames:v 1
// ends the run before the reverse branch fills (render.go) — so the
// untrimmed bounced PNG (1408 doubled frames, 19 GiB were it checked) and
// the bounce trimmed to 3 s reach ffmpeg like a forward static export of
// the untrimmed clip (one frame of master, no buffer).
func TestStaticReversedRenderAdmission(t *testing.T) {
	st := newTestStore(t)
	src := bigSource(t, st)
	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	png := recipe.Output{Format: "png"}
	trim3 := recipe.Op{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0,"end":3}`)}

	// render submits ops as a png recipe; frames "" means admitted (the
	// fake ffmpeg is reached), otherwise the refusal must name that count.
	render := func(t *testing.T, what string, ops []recipe.Op, frames string) {
		t.Helper()
		j, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: ops, Output: png})
		if err != nil {
			t.Fatal(err)
		}
		fin := waitFinished(t, m, j.ID)
		if fin.State != StateError {
			t.Fatalf("%s: %+v", what, fin)
		}
		if frames != "" {
			for _, want := range []string{"EZLG_MAX_MASTER_BYTES", "reverse buffer", "this render", frames + " frames of 2560x1440", "trim the clip", ErrInvalidRecipe.Error()} {
				if !strings.Contains(fin.Error, want) {
					t.Errorf("%s: error %q lacks %q", what, fin.Error, want)
				}
			}
			if strings.Contains(fin.Error, "ffmpeg") {
				t.Errorf("%s: ffmpeg was spawned: %q", what, fin.Error)
			}
			if _, err := os.Stat(filepath.Join(st.Scratch, j.ID)); !os.IsNotExist(err) {
				t.Errorf("%s: scratch dir created for a refused render: %v", what, err)
			}
		} else if !strings.Contains(fin.Error, "ffmpeg") || strings.Contains(fin.Error, "EZLG_MAX_MASTER_BYTES") {
			t.Errorf("%s: should pass admission and reach the fake ffmpeg: %+v", what, fin)
		}
		if m.scratch.Used() != 0 {
			t.Errorf("%s: budget leaked: %d", what, m.scratch.Used())
		}
	}
	render(t, "reversed png of the untrimmed clip", []recipe.Op{{Kind: recipe.OpReverse}}, "704")
	render(t, "reversed then bounced png of the untrimmed clip", []recipe.Op{{Kind: recipe.OpReverse}, {Kind: recipe.OpBounce}}, "1408")
	render(t, "reversed png trimmed to 3 s", []recipe.Op{trim3, {Kind: recipe.OpReverse}}, "")
	render(t, "bounced png of the untrimmed clip", []recipe.Op{{Kind: recipe.OpBounce}}, "")
	render(t, "bounced png trimmed to 3 s", []recipe.Op{trim3, {Kind: recipe.OpBounce}}, "")
	render(t, "forward png of the untrimmed clip", nil, "")
	if entries, _ := os.ReadDir(st.Scratch); len(entries) != 0 {
		t.Errorf("scratch has %d entries after the renders", len(entries))
	}
}

// mustInfo returns the probe info stored for src.
func mustInfo(t *testing.T, st *store.Store, src string) recipe.ProbeInfo {
	t.Helper()
	b, err := st.GetBlob(src)
	if err != nil {
		t.Fatal(err)
	}
	if b.Info == nil {
		t.Fatalf("source %s has no probe info", src)
	}
	return *b.Info
}

// TestPreviewSemaphoreAndFlight: at most PreviewConcurrency still/proxy
// ffmpeg runs are in flight (the third waits), identical requests share one
// run, every waiter gets the bytes, and a memo hit never waits for a slot.
func TestPreviewSemaphoreAndFlight(t *testing.T) {
	tools, marker := fakeFFmpegTools(t)
	st := newTestStore(t)
	src := putSource(t, st, true) // 64x48, 25 fps, 2 s
	m := NewManager(st, tools, Options{Concurrency: 1})
	if m.PreviewConcurrency() != 2 {
		t.Fatalf("PreviewConcurrency = %d, want max(2, 1)", m.PreviewConcurrency())
	}
	if m2 := NewManager(st, tools, Options{Concurrency: 5}); m2.PreviewConcurrency() != 5 {
		t.Errorf("PreviewConcurrency at concurrency 5 = %d", m2.PreviewConcurrency())
	}
	out := recipe.Output{Format: "gif"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type result struct {
		data []byte
		err  error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	still := func(tt float64) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := m.Still(ctx, src, nil, out, tt, 0)
			results <- result{data, err}
		}()
	}
	proxy := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := m.Proxy(ctx, []string{src}, nil, out, 0, 0)
			results <- result{data, err}
		}()
	}
	// Three distinct keys (two stills, a proxy) and two duplicates of the
	// first still.
	still(0.1)
	still(0.5)
	proxy()
	still(0.1)
	still(0.1)
	waitFor(t, 10*time.Second, "two fake ffmpegs", func() bool { return fakeMarkers(marker) >= 2 })
	time.Sleep(200 * time.Millisecond)
	if n := fakeMarkers(marker); n != 2 {
		t.Fatalf("%d ffmpegs started with a preview semaphore of 2", n)
	}
	releaseFakes(t, marker)
	wg.Wait()
	close(results)
	var pngs, webps int
	for r := range results {
		switch {
		case r.err != nil:
			t.Errorf("preview: %v", r.err)
		case string(r.data) == fakePNG:
			pngs++
		case string(r.data) == fakeWebP:
			webps++
		default:
			t.Errorf("preview returned %q", r.data)
		}
	}
	if pngs != 4 || webps != 1 {
		t.Errorf("%d stills and %d proxies answered, want 4 and 1", pngs, webps)
	}
	if n := fakeMarkers(marker); n != 3 {
		t.Errorf("%d ffmpegs ran, want 3 (three keys; the duplicates shared the first still's run)", n)
	}
	stills, _ := os.ReadDir(filepath.Join(st.Scratch, stillsDir))
	proxies, _ := os.ReadDir(filepath.Join(st.Scratch, proxyDir))
	if len(stills) != 2 || len(proxies) != 1 {
		t.Errorf("memo: %d stills, %d proxies", len(stills), len(proxies))
	}
	// Memo hits never take a slot: with the semaphore full they still
	// answer at once.
	for i := 0; i < m.PreviewConcurrency(); i++ {
		m.previewSem <- struct{}{}
	}
	hit, hitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer hitCancel()
	if data, err := m.Still(hit, src, nil, out, 0.1, 0); err != nil || string(data) != fakePNG {
		t.Errorf("memo hit with a full semaphore: %q %v", data, err)
	}
	if data, err := m.Proxy(hit, []string{src}, nil, out, 0, 0); err != nil || string(data) != fakeWebP {
		t.Errorf("proxy memo hit with a full semaphore: %q %v", data, err)
	}
	// A miss waits for a slot and honours its own cancellation while it does.
	miss, missCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer missCancel()
	if _, err := m.Still(miss, src, nil, out, 0.9, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("miss with a full semaphore: %v, want its own deadline", err)
	}
	if n := fakeMarkers(marker); n != 3 {
		t.Errorf("a waiting miss started an ffmpeg: %d", n)
	}
	for i := 0; i < m.PreviewConcurrency(); i++ {
		<-m.previewSem
	}
}

// TestAutoCropDetachedLeader: the request that starts a detection going
// away (a superseded still) does not kill the pass — it completes, writes
// the memo and the next request is served from it without a second ffmpeg.
func TestAutoCropDetachedLeader(t *testing.T) {
	tools, marker := fakeFFmpegTools(t)
	st := newTestStore(t)
	src := putSource(t, st, true) // 64x48 with alpha: the bbox detector
	m := NewManager(st, tools, Options{})
	ops := []recipe.Op{autocropOp(`{"padding":2}`)}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := m.ResolveAutoCrop(leaderCtx, src, ops)
		leaderDone <- err
	}()
	waitFor(t, 10*time.Second, "the detection to start", func() bool { return fakeMarkers(marker) == 1 })
	// A waiter that joins before the leader leaves gets the result.
	waiterDone := make(chan recipe.CropParams, 1)
	go func() {
		got, err := m.ResolveAutoCrop(context.Background(), src, ops)
		if err != nil {
			t.Errorf("waiter: %v", err)
			waiterDone <- recipe.CropParams{}
			return
		}
		waiterDone <- resolvedBox(t, got)
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("leader: %v, want its own cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled leader did not return")
	}
	time.Sleep(50 * time.Millisecond)
	if n := fakeMarkers(marker); n != 1 {
		t.Fatalf("%d detections started after the leader left, want the one still running", n)
	}
	key, _ := autocropKey(src, nil, 1)
	memo := filepath.Join(st.Scratch, autocropDir, key+".json")
	if _, err := os.Stat(memo); err == nil {
		t.Fatal("memo written before the detection finished")
	}
	releaseFakes(t, marker)
	waitFor(t, 10*time.Second, "the detached pass to write the memo", func() bool { _, err := os.Stat(memo); return err == nil })
	want := recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}
	if box := <-waiterDone; box != want {
		t.Errorf("waiter's box = %+v, want %+v", box, want)
	}
	// The next request — even on a manager without ffmpeg — is served from
	// the memo; the raw box is what is stored.
	if raw, ok := readAutocropMemo(memo, &recipe.ProbeInfo{Width: 64, Height: 48}); !ok || raw != (recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}) {
		t.Errorf("memo = %+v (%v), want the raw box", raw, ok)
	}
	noFF := NewManager(st, ffrun.Tools{}, Options{})
	got, err := noFF.ResolveAutoCrop(context.Background(), src, ops)
	if err != nil {
		t.Fatalf("memo hit: %v", err)
	}
	if box := resolvedBox(t, got); box != want {
		t.Errorf("memoised box = %+v, want %+v", box, want)
	}
	if n := fakeMarkers(marker); n != 1 {
		t.Errorf("%d detections ran in total, want 1", n)
	}
}
