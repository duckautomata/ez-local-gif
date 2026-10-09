package jobs

// Phase 5b: the matte pass itself (matte.go) — the batch writer, the
// up-front refusal arithmetic, the abandon-aware flight, the probe / status
// / facts state machine, and the pass end to end against an httptest fake
// sidecar (deterministic matte: 255 − luma of each model input) with a real
// ffmpeg producer. The pass tests skip without ffmpeg/ffprobe on PATH; the
// pipeline's use of the resolved mattes is matte_integration_test.go's.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// ---- unit: the batch writer --------------------------------------------------

// TestBatchWriterCutsExactFrames: the writer cuts a stream into exact
// frames whatever the chunking, numbers them from 1, reports a stream that
// ends mid-frame, and its first failure is sticky, cancels the producer and
// is what later writes return.
func TestBatchWriterCutsExactFrames(t *testing.T) {
	const fb = 6
	stream := make([]byte, 3*fb)
	for i := range stream {
		stream[i] = byte(i)
	}
	for _, chunks := range [][]int{{18}, {1, 5, 7, 2, 3}, {6, 6, 6}, {4, 4, 4, 4, 2}, {13, 5}} {
		var got [][]byte
		var slots []int
		bw := newBatchWriter(fb, 0, func(slot int, rgb []byte) error {
			slots = append(slots, slot)
			got = append(got, bytes.Clone(rgb))
			return nil
		}, nil)
		off := 0
		for _, n := range chunks {
			if w, err := bw.Write(stream[off : off+n]); err != nil || w != n {
				t.Fatalf("chunks %v: Write = %d, %v", chunks, w, err)
			}
			off += n
		}
		if err := bw.finish(); err != nil {
			t.Fatalf("chunks %v: finish: %v", chunks, err)
		}
		if bw.frames != 3 || len(got) != 3 {
			t.Fatalf("chunks %v: %d frames (%d callbacks), want 3", chunks, bw.frames, len(got))
		}
		for i, f := range got {
			if slots[i] != i+1 || !bytes.Equal(f, stream[i*fb:(i+1)*fb]) {
				t.Errorf("chunks %v: frame %d = slot %d %v", chunks, i, slots[i], f)
			}
		}
	}

	// A partial frame at the end is an error from finish, not a frame.
	bw := newBatchWriter(fb, 0, func(int, []byte) error { return nil }, nil)
	bw.Write(stream[:fb+2])
	if bw.frames != 1 {
		t.Errorf("partial: %d frames, want 1", bw.frames)
	}
	if err := bw.finish(); err == nil || !strings.Contains(err.Error(), "mid-frame") {
		t.Errorf("partial: finish = %v", err)
	}

	// The run-time cap: max+1 streamed frames fail with the refusal
	// message, cancel the producer and stick.
	cancelled := 0
	calls := 0
	bw = newBatchWriter(fb, 2, func(int, []byte) error { calls++; return nil }, func() { cancelled++ })
	_, err := bw.Write(stream)
	if !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "EZLG_MATTE_MAX_FRAMES") || !strings.Contains(err.Error(), "more than 2 frames") {
		t.Fatalf("cap: Write = %v", err)
	}
	if calls != 2 || bw.frames != 2 || cancelled != 1 {
		t.Errorf("cap: %d callbacks, %d frames, %d cancels", calls, bw.frames, cancelled)
	}
	if _, err2 := bw.Write(stream[:1]); err2 != err {
		t.Errorf("cap: a later write returned %v, want the sticky %v", err2, err)
	}
	if bw.failure() != err {
		t.Errorf("cap: failure() = %v", bw.failure())
	}

	// A callback error is the pass's error, also sticky and cancelling.
	boom := errors.New("boom")
	cancelled = 0
	bw = newBatchWriter(fb, 0, func(slot int, _ []byte) error {
		if slot == 2 {
			return boom
		}
		return nil
	}, func() { cancelled++ })
	if _, err := bw.Write(stream); err != boom || cancelled != 1 || bw.failure() != boom {
		t.Errorf("callback error: %v, %d cancels, failure %v", err, cancelled, bw.failure())
	}

	// fail() from another goroutine (the hard timeout) wins over nothing
	// and loses to an earlier failure.
	bw = newBatchWriter(fb, 0, func(int, []byte) error { return nil }, nil)
	late := errors.New("late")
	if got := bw.fail(late); got != late {
		t.Errorf("fail = %v", got)
	}
	if got := bw.fail(errors.New("later")); got != late {
		t.Errorf("second fail = %v, want the first", got)
	}
}

// ---- unit: refusal arithmetic -----------------------------------------------

// TestMatteRefusalArithmetic: the estimate is frames × (msPerFrame + 2 ms);
// the caps refuse with messages naming the knob; an unknown count is never
// refused up-front; the hard and batch timeouts follow the spec's formulas.
func TestMatteRefusalArithmetic(t *testing.T) {
	m := NewManager(newTestStore(t), fakeTools, Options{MatteMaxSeconds: 600, MatteMaxFrames: 3000})
	if got := matteEstimateMS(1800, 898); got != 1800*900 {
		t.Errorf("estimate = %d, want %d", got, 1800*900)
	}
	if got := matteEstimateMS(0, 898); got != 0 {
		t.Errorf("unknown count estimate = %d, want 0", got)
	}
	if got := matteEstimateMS(10, -5); got != 20 {
		t.Errorf("negative msPerFrame estimate = %d, want 20", got)
	}

	err := m.matteRefusal(1800, matteEstimateMS(1800, 898), matte.DeviceCPU, recipe.MatteModelBiRefNetLite)
	if !errors.Is(err, ErrInvalidRecipe) {
		t.Fatalf("over the seconds cap: %v", err)
	}
	for _, want := range []string{"1800 frames", "~27 min", "CPU", "600 s cap", "EZLG_MATTE_MAX_SECONDS", "pick the fast model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("seconds refusal %q lacks %q", err, want)
		}
	}
	if err := m.matteRefusal(1800, matteEstimateMS(1800, 898), matte.DeviceCPU, recipe.MatteModelDefault); err == nil || strings.Contains(err.Error(), "fast model") {
		t.Errorf("the default model's refusal must not suggest the fast model: %v", err)
	}
	if err := m.matteRefusal(3001, 0, matte.DeviceCUDA, recipe.MatteModelDefault); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "EZLG_MATTE_MAX_FRAMES") || !strings.Contains(err.Error(), "3001 frames") {
		t.Errorf("over the frames cap: %v", err)
	}
	if err := m.matteRefusal(3000, 600_000, matte.DeviceCUDA, recipe.MatteModelDefault); err != nil {
		t.Errorf("at both caps exactly: %v", err)
	}
	if err := m.matteRefusal(3000, 600_001, matte.DeviceCUDA, recipe.MatteModelDefault); err == nil {
		t.Error("one ms over the seconds cap must refuse")
	}
	if err := m.matteRefusal(0, 0, matte.DeviceCPU, recipe.MatteModelDefault); err != nil {
		t.Errorf("unknown count: %v", err)
	}

	if got := matteHardTimeout(600, 0); got != 1200*time.Second {
		t.Errorf("hard timeout (unknown) = %s", got)
	}
	if got := matteHardTimeout(600, 10_000); got != 90*time.Second {
		t.Errorf("hard timeout (10 s estimate) = %s, want 90s", got)
	}
	if got := matteHardTimeout(600, 10_000_000); got != 1200*time.Second {
		t.Errorf("hard timeout (huge estimate) = %s, want the 2x cap", got)
	}
	if got := matteBatchTimeout(8, 100, 0); got != 64*time.Second {
		t.Errorf("batch timeout = %s, want 64s", got)
	}
	if got := matteBatchTimeout(8, 100, 1); got != 128*time.Second {
		t.Errorf("batch timeout (busy 1) = %s, want 128s", got)
	}
	if got := matteBatchTimeout(8, 0, 100); got != 60*time.Second*time.Duration(1+matteBusyCap) {
		t.Errorf("batch timeout (busy capped) = %s", got)
	}
	if got := humanSeconds(1_620_000); got != "27 min" {
		t.Errorf("humanSeconds = %q", got)
	}
	if got := humanSeconds(12_400); got != "12 s" {
		t.Errorf("humanSeconds = %q", got)
	}
}

// ---- unit: the abandon-aware flight ------------------------------------------

// TestFlightAbandon: a detached run whose waiters all left is cancelled
// once the grace has passed; a waiter that comes back inside the grace
// keeps it alive; a waiter that joins a run the abandon timer has just
// cancelled runs it again instead of failing.
func TestFlightAbandon(t *testing.T) {
	const grace = 100 * time.Millisecond
	var g flight[int]

	// 1. Abandoned → cancelled after the grace.
	var mu sync.Mutex
	var cancelledAt time.Time
	started := make(chan struct{}, 1)
	fn := func(ctx context.Context) (int, error) {
		started <- struct{}{}
		<-ctx.Done()
		mu.Lock()
		cancelledAt = time.Now()
		mu.Unlock()
		return 0, ctx.Err()
	}
	wctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	left := time.Now()
	_, err := g.doDetachedAbandon(wctx, "a", grace, fn)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter: %v, want its own deadline", err)
	}
	<-started
	waitFor(t, 5*time.Second, "the abandoned run to be cancelled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !cancelledAt.IsZero()
	})
	mu.Lock()
	since := cancelledAt.Sub(left)
	mu.Unlock()
	if since < grace {
		t.Errorf("cancelled %s after the last waiter left, before the %s grace", since, grace)
	}
	waitFor(t, time.Second, "the call to be gone", func() bool { return !g.inFlight("a") })

	// 2. A waiter back inside the grace keeps the run alive; the result
	// reaches it.
	release := make(chan struct{})
	runs := 0
	fn2 := func(ctx context.Context) (int, error) {
		runs++
		select {
		case <-release:
			return 42, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	wctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	if _, err := g.doDetachedAbandon(wctx, "b", grace, fn2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first waiter: %v", err)
	}
	cancel()
	time.Sleep(grace / 3) // inside the grace
	done := make(chan int, 1)
	go func() {
		v, err := g.doDetachedAbandon(context.Background(), "b", grace, fn2)
		if err != nil {
			t.Errorf("second waiter: %v", err)
		}
		done <- v
	}()
	time.Sleep(2 * grace) // well past the first waiter's grace: the run must still be there
	if !g.inFlight("b") {
		t.Fatal("the run was cancelled although a waiter came back inside the grace")
	}
	close(release)
	if v := <-done; v != 42 || runs != 1 {
		t.Errorf("second waiter got %d from %d runs, want 42 from 1", v, runs)
	}

	// 3. Joining a run the timer has just cancelled: the context error is
	// not the waiter's — it runs fn again.
	runs = 0
	fn3 := func(ctx context.Context) (int, error) {
		runs++
		if runs == 1 {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond) // widen the window between the cancel and the return
			return 0, ctx.Err()
		}
		return 7, nil
	}
	wctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	g.doDetachedAbandon(wctx, "c", grace, fn3)
	cancel()
	time.Sleep(grace + 10*time.Millisecond) // the timer has fired; fn3 is in its sleep
	v, err := g.doDetachedAbandon(context.Background(), "c", grace, fn3)
	if err != nil || v != 7 || runs != 2 {
		t.Errorf("late joiner: %d, %v from %d runs; want 7 from 2", v, err, runs)
	}
}

// ---- the fake sidecar ---------------------------------------------------------

// fakeSidecar is an httptest matte sidecar: /v1/ping answers its ping
// (status pingStatus), /v1/matte answers 255 − luma gray PNGs for every
// frame (after delay, or failStatus instead), /v1/warm records the model.
type fakeSidecar struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	ping       matte.Ping
	pingStatus int
	delay      time.Duration
	failStatus int
	retry503   int // POSTs still to answer 503 "model loading" + retryAfterMs before serving
	posts      int
	frames     int
	pings      int
	warms      []string
	batches    [][][]byte // the rgb frames of every POST, in order
}

const fakeMatteSize = 16

func newFakeSidecar(t *testing.T, weights string, msPerFrame float64) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{t: t, pingStatus: http.StatusOK}
	f.ping = matte.Ping{
		Protocol: matte.Protocol, Version: "fake", Instance: "fake-1", ProcessingVersion: "1",
		Device: matte.DeviceCPU, DefaultModel: recipe.MatteModelISNetAnime,
		Models: map[string]matte.ModelState{
			recipe.MatteModelISNetAnime: {
				State: matte.StateReady, Weights: weights, GraphDigest: "g-" + weights, Precision: "fp32",
				Sizes: []int{fakeMatteSize}, DefaultSize: fakeMatteSize, MsPerFrame: map[string]float64{"16": msPerFrame},
				Licence: "Apache-2.0", Label: "Anime (fast)",
			},
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSidecar) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/ping":
		f.mu.Lock()
		f.pings++
		data, _ := json.Marshal(f.ping) // under the lock: Models is a shared map
		status, reason := f.pingStatus, f.ping.Reason
		f.mu.Unlock()
		p := matte.Ping{Reason: reason}
		body := map[string]any{}
		json.Unmarshal(data, &body)
		if status != http.StatusOK {
			body["error"] = p.Reason
		}
		out, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(out)
	case "/v1/warm":
		f.mu.Lock()
		f.warms = append(f.warms, r.URL.Query().Get("model"))
		f.mu.Unlock()
		w.Write([]byte(`{"model":"x","state":"loading"}`))
	case "/v1/matte":
		f.matte(w, r)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func (f *fakeSidecar) matte(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	size, frames := atoiOr(q.Get("size"), 0), atoiOr(q.Get("frames"), 0)
	body, err := io.ReadAll(r.Body)
	if err != nil || size <= 0 || frames <= 0 || int64(len(body)) != matte.BodyLength(frames, size) {
		http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.posts++
	f.frames += frames
	fb := size * size * 3
	batch := make([][]byte, frames)
	for i := range batch {
		batch[i] = bytes.Clone(body[i*fb : (i+1)*fb])
	}
	f.batches = append(f.batches, batch)
	delay, fail, retry := f.delay, f.failStatus, f.retry503 > 0
	if retry {
		f.retry503--
	}
	f.mu.Unlock()
	if fail != 0 {
		http.Error(w, `{"error":"fake failure"}`, fail)
		return
	}
	if retry {
		// The sidecar's "model loading" answer (a model unloaded by its TTL
		// or re-created after an OOM): the client retries after retryAfterMs.
		http.Error(w, `{"error":"model loading","retryAfterMs":200,"state":"loading","percent":0}`, http.StatusServiceUnavailable)
		return
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-ezlg-mattes")
	var out bytes.Buffer
	var hdr [4]byte
	for _, rgb := range batch {
		p := fakeMattePNG(rgb, size)
		binary.BigEndian.PutUint32(hdr[:], uint32(len(p)))
		out.Write(hdr[:])
		out.Write(p)
	}
	binary.BigEndian.PutUint32(hdr[:], 0)
	out.Write(hdr[:])
	w.Write(out.Bytes())
}

// fakeMattePNG is the fake's matte of one model input: 255 − luma per pixel
// as an 8-bit gray PNG.
func fakeMattePNG(rgb []byte, size int) []byte {
	img := image.NewGray(image.Rect(0, 0, size, size))
	for i := 0; i < size*size; i++ {
		r, g, b := int(rgb[3*i]), int(rgb[3*i+1]), int(rgb[3*i+2])
		img.Pix[i] = uint8(255 - (299*r+587*g+114*b)/1000)
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func (f *fakeSidecar) stats() (posts, frames int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.frames
}

func (f *fakeSidecar) setModel(mutate func(ms *matte.ModelState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ms := f.ping.Models[recipe.MatteModelISNetAnime]
	mutate(&ms)
	f.ping.Models[recipe.MatteModelISNetAnime] = ms
}

func (f *fakeSidecar) set(mutate func(f *fakeSidecar)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

func atoiOr(s string, def int) int {
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}

// shortMatteWaits shortens the pass's waits for the test's duration.
func shortMatteWaits(t *testing.T, preview, grace, poll time.Duration) {
	t.Helper()
	pw, ag, lp := mattePreviewWait, matteAbandonGrace, matteLoadPoll
	mattePreviewWait, matteAbandonGrace, matteLoadPoll = preview, grace, poll
	t.Cleanup(func() { mattePreviewWait, matteAbandonGrace, matteLoadPoll = pw, ag, lp })
}

// ---- the probe ----------------------------------------------------------------

// TestMatteProbeStateMachine: the probe persists the facts and flips
// features.matte on; a plain install never enables it; three consecutive
// failures flip it off (one leaves it on), a 503 device-unavailable answer
// surfaces its reason, a protocol mismatch asks for an update, and a
// returning sidecar turns it back on.
func TestMatteProbeStateMachine(t *testing.T) {
	st := newTestStore(t)
	plain := NewManager(st, fakeTools, Options{})
	if plain.MatteEnabled() {
		t.Fatal("a plain install must not enable AI mattes")
	}
	if s := plain.MatteStatus(); s.Enabled || !strings.Contains(s.Reason, "EZLG_MATTE_URL") || s.DefaultModel != recipe.MatteModelDefault || s.Models == nil || s.MaxSeconds != DefaultMatteMaxSeconds {
		t.Errorf("plain status = %+v", s)
	}
	if _, err := plain.probeMatte(context.Background()); !errors.Is(err, ErrMatteUnavailable) {
		t.Errorf("plain probe: %v", err)
	}

	f := newFakeSidecar(t, testWeights, 3)
	m := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL + "/", MatteMaxSeconds: 120, MatteMaxFrames: 500})
	if m.MatteEnabled() {
		t.Fatal("enabled before the first probe")
	}
	if s := m.MatteStatus(); s.Enabled || !strings.Contains(s.Reason, "has not answered yet") || s.MaxSeconds != 120 || s.MaxFrames != 500 {
		t.Errorf("unprobed status = %+v", s)
	}
	if _, err := m.loadMatteFacts(); !errors.Is(err, ErrMatteUnavailable) {
		t.Errorf("facts before any probe: %v", err)
	}

	ctx := context.Background()
	if _, err := m.probeMatte(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !m.MatteEnabled() {
		t.Fatal("not enabled after a good probe")
	}
	s := m.MatteStatus()
	ms, ok := s.Models[recipe.MatteModelISNetAnime]
	if !s.Enabled || s.Device != matte.DeviceCPU || s.Reason != "" || !ok || ms.Label != "Anime (fast)" || ms.State != matte.StateReady || ms.MsPerFrame != 3 || ms.Licence != "Apache-2.0" || len(ms.Sizes) != 1 {
		t.Errorf("status after a good probe = %+v", s)
	}
	facts, err := m.loadMatteFacts()
	if err != nil || facts.ProcessingVersion != "1" || facts.Models[recipe.MatteModelISNetAnime].Weights != testWeights {
		t.Fatalf("facts after the probe: %+v, %v", facts, err)
	}
	if _, err := os.Stat(matteFactsPath(st)); err != nil {
		t.Fatalf("facts file: %v", err)
	}
	// A manager started later loads the file's facts without a probe.
	later := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL})
	if fl, err := later.loadMatteFacts(); err != nil || fl.Models[recipe.MatteModelISNetAnime].Weights != testWeights {
		t.Errorf("facts on a later manager: %v", err)
	}

	// One failure: degraded, still on. Three: off, with the reason.
	f.srv.Close()
	for i := 1; i <= matteProbeFailures; i++ {
		if _, err := m.probeMatte(ctx); err == nil {
			t.Fatalf("probe %d of a closed sidecar succeeded", i)
		}
		if on := m.MatteEnabled(); on != (i < matteProbeFailures) {
			t.Errorf("after %d failures enabled = %v", i, on)
		}
	}
	if s := m.MatteStatus(); s.Enabled || !strings.Contains(s.Reason, "has not answered for 3 probes") {
		t.Errorf("status after 3 failures = %+v", s)
	}
	// The facts survive the outage: identity still resolves.
	if _, err := m.loadMatteFacts(); err != nil {
		t.Errorf("facts during the outage: %v", err)
	}

	// A sidecar back up with a 503 device-unavailable ping: counted as a
	// failure, but its reason and models reach the status.
	g := newFakeSidecar(t, testWeights, 3)
	g.set(func(f *fakeSidecar) {
		f.pingStatus = http.StatusServiceUnavailable
		f.ping.Device = matte.DeviceUnavailable
		f.ping.Reason = "CUDA EP not available — nvidia-container-toolkit?"
	})
	m2 := NewManager(st, fakeTools, Options{MatteURL: g.srv.URL})
	if _, err := m2.probeMatte(ctx); err == nil {
		t.Fatal("a 503 ping must be a failed probe")
	}
	// The headline is the sidecar's own reason — it DID answer — never
	// "has not answered yet" with the cause in brackets.
	if s := m2.MatteStatus(); s.Enabled || s.Device != matte.DeviceUnavailable || !strings.HasPrefix(s.Reason, "the matte service's device is unavailable: CUDA EP not available") || strings.Contains(s.Reason, "has not answered") || len(s.Models) != 1 {
		t.Errorf("status after a 503 ping = %+v", s)
	}
	// A sidecar that answered 503 and then vanished is "not answering"
	// again: the device reason is only the headline while it answers.
	h := newFakeSidecar(t, testWeights, 3)
	h.set(func(f *fakeSidecar) {
		f.pingStatus = http.StatusServiceUnavailable
		f.ping.Device = matte.DeviceUnavailable
		f.ping.Reason = "CUDA EP not available"
	})
	m3 := NewManager(st, fakeTools, Options{MatteURL: h.srv.URL})
	m3.probeMatte(ctx)
	if s := m3.MatteStatus(); !strings.HasPrefix(s.Reason, "the matte service's device is unavailable") {
		t.Errorf("status after a 503 ping (m3) = %+v", s)
	}
	h.srv.Close()
	m3.probeMatte(ctx)
	if s := m3.MatteStatus(); !strings.Contains(s.Reason, "has not answered yet") || strings.Contains(s.Reason, "device is unavailable") {
		t.Errorf("status after the 503 sidecar vanished = %+v", s)
	}
	// Back to a good answer: on again, device cpu.
	g.set(func(f *fakeSidecar) {
		f.pingStatus = http.StatusOK
		f.ping.Device = matte.DeviceCPU
		f.ping.Reason = ""
	})
	if _, err := m2.probeMatte(ctx); err != nil || !m2.MatteEnabled() {
		t.Errorf("recovery: %v, enabled %v", err, m2.MatteEnabled())
	}
	// Another protocol: off, asking for an update.
	g.set(func(f *fakeSidecar) { f.ping.Protocol = 2 })
	if _, err := m2.probeMatte(ctx); !errors.Is(err, ErrMatteUnavailable) {
		t.Errorf("protocol 2 probe: %v", err)
	}
	for i := 0; i < matteProbeFailures; i++ {
		m2.probeMatte(ctx)
	}
	if s := m2.MatteStatus(); s.Enabled || !strings.Contains(s.Reason, "update the matte service") {
		t.Errorf("status under protocol 2 = %+v", s)
	}
	// A protocol-2 answer is never persisted: the facts of the last good
	// answer survive it, so memo hits keep resolving.
	if fl, err := m2.loadMatteFacts(); err != nil || fl.Protocol != matte.Protocol {
		t.Errorf("facts under a protocol-2 sidecar: %+v, %v (want the last good answer)", fl, err)
	}
}

// expireMattePing makes the manager forget the age of its last probe
// answer, so the next pass pings afresh instead of reusing it (what the
// 30 s probe loop would have done meanwhile).
func expireMattePing(m *Manager) {
	m.mt.mu.Lock()
	m.mt.liveAt = time.Time{}
	m.mt.mu.Unlock()
}

// TestMatteSubmitRefusesWithoutFacts: Submit of a matte recipe on a plain
// install (no sidecar, no facts) is ErrMatteUnavailable, and with facts the
// Resolved identity enters the recipe hash.
func TestMatteSubmitRefusesWithoutFacts(t *testing.T) {
	st := newTestStore(t)
	src := putSource(t, st, true)
	m := NewManager(st, fakeTools, Options{})
	r := recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{matteOp("")}, Output: recipe.Output{Format: "webp"}}
	if _, err := m.Submit(r); !errors.Is(err, ErrMatteUnavailable) {
		t.Fatalf("Submit without facts: %v", err)
	}
	writeMatteFacts(t, st, testPing(testWeights))
	opsA, err := m.fillMatteResolved(r.Ops)
	if err != nil {
		t.Fatal(err)
	}
	writeMatteFacts(t, st, testPing(otherWeights))
	opsB, err := m.fillMatteResolved(r.Ops)
	if err != nil {
		t.Fatal(err)
	}
	ra, rb := r, r
	ra.Ops, rb.Ops = opsA, opsB
	if ra.Hash() == rb.Hash() || ResultKey(ra) == ResultKey(rb) {
		t.Error("the recipe hash must change with the weights")
	}
	if !strings.Contains(string(opsA[0].Params), testWeights) || !strings.Contains(string(opsB[0].Params), otherWeights) {
		t.Errorf("Resolved not filled: %s / %s", opsA[0].Params, opsB[0].Params)
	}
}

// TestMatteDeferredAndRefusedWithoutFFmpeg: a still over the eager bound is
// deferred before anything runs (no sidecar call, no pass), an eager one
// would start the pass, and the caps refuse before the deferral.
func TestMatteDeferredAndRefusedWithoutFFmpeg(t *testing.T) {
	st := newTestStore(t)
	srcHash := putSource(t, st, true) // 25 fps, 2 s → 50 frames
	src, _ := st.GetBlob(srcHash)
	f := newFakeSidecar(t, testWeights, 10_000) // 10 s per frame → 500 s for the clip
	m := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL, MatteMaxSeconds: 600})
	writeMatteFacts(t, st, &f.ping)
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	_, err := m.resolveMattes(context.Background(), src, ops, out, matteModePreview)
	var pending *ErrMattePending
	if !errors.As(err, &pending) || pending.State != MattePendingDeferred || pending.Total != 50 || pending.Device != matte.DeviceCPU || pending.EstimateMS != 50*10_002 {
		t.Fatalf("deferred still: %v (%+v)", err, pending)
	}
	if posts, _ := f.stats(); posts != 0 {
		t.Errorf("a deferred still posted %d batches", posts)
	}
	if !strings.Contains(err.Error(), "Play or Render") {
		t.Errorf("deferred message: %v", err)
	}
	// Eager: the pass starts (and fails here for want of ffmpeg — a plain
	// error, never a context error).
	eager := WithMatteEager(context.Background(), true)
	if _, err := m.resolveMattes(eager, src, ops, out, matteModePreview); err == nil || errors.As(err, &pending) || isContextError(err) {
		t.Errorf("eager still: %v", err)
	}
	// Over the caps: refused up-front, before the deferral.
	tight := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL, MatteMaxSeconds: 100})
	if _, err := tight.resolveMattes(context.Background(), src, ops, out, matteModePreview); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "EZLG_MATTE_MAX_SECONDS") {
		t.Errorf("over the seconds cap: %v", err)
	}
	few := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL, MatteMaxFrames: 10})
	if _, err := few.resolveMattes(context.Background(), src, ops, out, matteModePreview); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "EZLG_MATTE_MAX_FRAMES") {
		t.Errorf("over the frames cap: %v", err)
	}
	// An unoffered model / size is the client's mistake.
	if _, err := m.resolveMattes(context.Background(), src, []recipe.Op{matteOp(`{"model":"nope"}`)}, out, matteModePreview); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unoffered model: %v", err)
	}
	if _, err := m.resolveMattes(context.Background(), src, []recipe.Op{matteOp(`{"size":99}`)}, out, matteModePreview); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unoffered size: %v", err)
	}
	// Without a matte op nothing is read.
	if got, err := m.resolveMattes(context.Background(), src, nil, out, matteModePreview); got != nil || err != nil {
		t.Errorf("no matte op: %v, %v", got, err)
	}
}

// ---- the pass (real ffmpeg + the fake sidecar) ---------------------------------

// matteRig is an e2e whose manager talks to a fake sidecar.
type matteRig struct {
	*e2e
	f *fakeSidecar
}

func newMatteRig(t *testing.T, msPerFrame float64, opts Options) *matteRig {
	t.Helper()
	tools := realTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	st := newTestStore(t)
	f := newFakeSidecar(t, testWeights, msPerFrame)
	opts.MatteURL = f.srv.URL
	if opts.Concurrency == 0 {
		opts.Concurrency = 2
	}
	e := &e2e{t: t, ctx: ctx, st: st, tools: tools, dir: t.TempDir()}
	e.m = NewManager(st, tools, opts)
	writeMatteFacts(t, st, &f.ping)
	return &matteRig{e2e: e, f: f}
}

// clipDistinct is a 32x32 20-frame clip at 10 fps whose frames are 20
// distinct flat colours — distinct model inputs even at the fake's 16x16,
// so every frame is a frames-store miss the first time and record i of
// the stream is slot i of the memo. (testsrc2 at this size collapses to
// 13 distinct inputs after the stretch.)
func (e *matteRig) clipDistinct() *store.Blob {
	return e.lavfi("clip.mov", "color=c=black:size=32x32:rate=10:duration=2,geq=r='N*12':g='128':b='255-N*12'", "rgb24")
}

// clipHeld is a 32x32 20-frame clip at 10 fps of 5 flat colours held 4
// frames each (none shared with clipDistinct), for the dedupe path.
func (e *matteRig) clipHeld() *store.Blob {
	return e.lavfi("held.mov", "color=c=black:size=32x32:rate=10:duration=2,geq=r='floor(N/4)*50':g='64':b='255-floor(N/4)*50'", "rgb24")
}

// matteDirs lists the memo dirs (not tmp, not frames) under <data>/mattes.
func matteDirs(t *testing.T, st *store.Store) (memos, tmps []string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(st.MatteDir("x")))
	if err != nil {
		return nil, nil
	}
	for _, en := range entries {
		switch {
		case !en.IsDir() || en.Name() == "frames":
		case strings.HasPrefix(en.Name(), ".tmp-"):
			tmps = append(tmps, en.Name())
		case !strings.HasPrefix(en.Name(), "."):
			memos = append(memos, en.Name())
		}
	}
	return memos, tmps
}

// TestMattePassMemoAndFramesStore: a miss runs the pass (3 POSTs of 8+8+4
// distinct frames), files the mattes the fake computed, writes the manifest
// and renames the dir into place; a second request is a hit without a
// POST; the sidecar stopped, the memo is still served; a trimmed
// re-request is a new clip key whose frames all hit the store (0 POSTs).
func TestMattePassMemoAndFramesStore(t *testing.T) {
	e := newMatteRig(t, 1, Options{})
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	mattes, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(mattes) != 1 {
		t.Fatalf("%d mattes, want 1", len(mattes))
	}
	rm := mattes[0]
	if rm.Model != recipe.MatteModelISNetAnime || rm.Size != fakeMatteSize || rm.ReqSize != 0 || rm.Precision != "fp32" || rm.Manifest == nil || rm.Manifest.Frames != 20 || rm.Manifest.Weights != testWeights || rm.Manifest.Proc != "1" || rm.Manifest.Device != matte.DeviceCPU || rm.Manifest.Key != rm.ClipKey || rm.Manifest.FPS != "10" {
		t.Fatalf("resolved = %+v (manifest %+v)", rm, rm.Manifest)
	}
	posts, frames := e.f.stats()
	if posts != 3 || frames != 20 {
		t.Errorf("%d POSTs of %d frames, want 3 of 20", posts, frames)
	}
	for i := 1; i <= 20; i++ {
		if _, err := os.Stat(filepath.Join(rm.Dir, matte.FrameFile(i))); err != nil {
			t.Errorf("frame %d: %v", i, err)
		}
	}
	// The mattes on disk are the fake's answers for the frames it received,
	// in order (frame i of the clip = record i of the stream).
	var want [][]byte
	for _, b := range e.f.batches {
		for _, rgb := range b {
			want = append(want, fakeMattePNG(rgb, fakeMatteSize))
		}
	}
	for i, w := range want {
		got, _ := os.ReadFile(filepath.Join(rm.Dir, matte.FrameFile(i+1)))
		if !bytes.Equal(got, w) {
			t.Errorf("frame %d on disk differs from the sidecar's answer", i+1)
		}
	}
	memos, tmps := matteDirs(t, e.st)
	if len(memos) != 1 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v", memos, tmps)
	}
	storeDir := e.st.MatteFrameDir(recipe.MatteModelISNetAnime, fakeMatteSize, "fp32", testWeights[:8], "1")
	if entries, err := os.ReadDir(storeDir); err != nil || len(entries) != 20 {
		t.Errorf("frames store holds %d files (%v), want 20", len(entries), err)
	}
	if _, ok := e.m.matteProgressFor(rm.ClipKey); ok {
		t.Error("progress left behind after the pass")
	}

	// A hit: no POST, the memo touched, same manifest; preview mode too.
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(rm.Dir, old, old)
	again, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview)
	if err != nil || len(again) != 1 || again[0].ClipKey != rm.ClipKey || again[0].Manifest.Frames != 20 {
		t.Fatalf("hit: %v, %+v", err, again)
	}
	if p, _ := e.f.stats(); p != 3 {
		t.Errorf("a hit posted (%d POSTs now)", p)
	}
	if info, _ := os.Stat(rm.Dir); time.Since(info.ModTime()) > time.Hour {
		t.Error("the hit did not touch the memo")
	}

	// Sidecar gone: still served from the memo (identity from the facts).
	e.f.srv.Close()
	for i := 0; i < matteProbeFailures; i++ {
		e.m.probeMatte(e.ctx)
	}
	if e.m.MatteEnabled() {
		t.Error("enabled with the sidecar gone")
	}
	if got, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview); err != nil || got[0].ClipKey != rm.ClipKey {
		t.Fatalf("hit with the sidecar down: %v", err)
	}
	// A miss with the sidecar down is unavailable, naming it.
	trimmed := []recipe.Op{{Kind: recipe.OpTrim, Params: []byte(`{"start":0.5}`)}, matteOp("")}
	if _, err := e.m.resolveMattes(e.ctx, clip, trimmed, out, matteModeRender); !errors.Is(err, ErrMatteUnavailable) || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("miss with the sidecar down: %v", err)
	}
	if _, tmps := matteDirs(t, e.st); len(tmps) != 0 {
		t.Errorf("tmp dirs left by the failed pass: %v", tmps)
	}

	// The sidecar back (a new instance at a new URL): the trimmed clip is
	// a new key whose 15 frames all hit the frames store — 0 POSTs.
	g := newFakeSidecar(t, testWeights, 1)
	m2 := NewManager(e.st, e.tools, Options{Concurrency: 2, MatteURL: g.srv.URL})
	got, err := m2.resolveMattes(e.ctx, clip, trimmed, out, matteModeRender)
	if err != nil {
		t.Fatalf("trimmed pass: %v", err)
	}
	if got[0].ClipKey == rm.ClipKey || got[0].Manifest.Frames != 15 {
		t.Errorf("trimmed: key %s (orig %s), %d frames", short(got[0].ClipKey), short(rm.ClipKey), got[0].Manifest.Frames)
	}
	if p, fr := g.stats(); p != 0 || fr != 0 {
		t.Errorf("the trimmed re-request posted %d batches of %d frames, want 0 (frames-store hits)", p, fr)
	}
	// Its frame 1 is the original's frame 6 (trim 0.5 s at 10 fps).
	a, _ := os.ReadFile(filepath.Join(got[0].Dir, matte.FrameFile(1)))
	b, _ := os.ReadFile(filepath.Join(rm.Dir, matte.FrameFile(6)))
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Error("the trimmed memo's first matte is not the original's sixth")
	}
	if memos, _ := matteDirs(t, e.st); len(memos) != 2 {
		t.Errorf("memo dirs %v, want 2", memos)
	}

	// A held pose rides on the first frame's answer: 5 distinct inputs in
	// 20 frames → one POST of 5, 20 slots, the 4 slots of a pose
	// byte-identical, no second request for a repeated frame.
	held := e.clipHeld()
	hp, hf := g.stats()
	got, err = m2.resolveMattes(e.ctx, held, ops, out, matteModeRender)
	if err != nil || got[0].Manifest.Frames != 20 {
		t.Fatalf("held clip: %v", err)
	}
	if p, fr := g.stats(); p-hp != 1 || fr-hf != 5 {
		t.Errorf("held clip: %d POSTs of %d frames, want 1 of 5", p-hp, fr-hf)
	}
	for pose := 0; pose < 5; pose++ {
		first, _ := os.ReadFile(filepath.Join(got[0].Dir, matte.FrameFile(4*pose+1)))
		for k := 2; k <= 4; k++ {
			if other, _ := os.ReadFile(filepath.Join(got[0].Dir, matte.FrameFile(4*pose+k))); len(first) == 0 || !bytes.Equal(first, other) {
				t.Errorf("pose %d: slot %d differs from slot %d", pose, 4*pose+k, 4*pose+1)
			}
		}
	}
	if entries, _ := os.ReadDir(storeDir); len(entries) != 25 {
		t.Errorf("frames store holds %d files, want 25 (20 + 5)", len(entries))
	}
}

// TestMattePreviewPendingAndSingleFlight: ten concurrent previews of one
// recipe share ONE pass; past the preview wait each answers
// *ErrMattePending running with the pass's progress, and the pass goes on
// to completion while the SPA keeps re-joining.
func TestMattePreviewPendingAndSingleFlight(t *testing.T) {
	shortMatteWaits(t, 150*time.Millisecond, 2*time.Second, 50*time.Millisecond)
	e := newMatteRig(t, 1, Options{})
	e.f.set(func(f *fakeSidecar) { f.delay = 300 * time.Millisecond }) // 3 batches ≈ 0.9 s
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview)
		}(i)
	}
	wg.Wait()
	pendings := 0
	for i, err := range errs {
		var pending *ErrMattePending
		switch {
		case err == nil:
		case errors.As(err, &pending):
			pendings++
			if pending.State != MattePendingRunning || pending.Total != 20 || pending.Device != matte.DeviceCPU {
				t.Errorf("preview %d: pending %+v", i, pending)
			}
		default:
			t.Errorf("preview %d: %v", i, err)
		}
	}
	if pendings == 0 {
		t.Error("no preview answered pending after the wait")
	}
	// Keep re-joining like the SPA until the memo is there.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview)
		if err == nil {
			if got[0].Manifest.Frames != 20 {
				t.Errorf("frames = %d", got[0].Manifest.Frames)
			}
			break
		}
		var pending *ErrMattePending
		if !errors.As(err, &pending) {
			t.Fatalf("re-join: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pass never finished: %+v", pending)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("%d POSTs of %d frames across ten previews, want one pass (3 of 20)", posts, frames)
	}
}

// TestMatteLoadingThenReady: a model that is loading is a pending state for
// a preview (with /v1/warm posted) and a bounded wait for a render, whose
// progress listener sees the loading state before the frames.
func TestMatteLoadingThenReady(t *testing.T) {
	shortMatteWaits(t, 100*time.Millisecond, 2*time.Second, 30*time.Millisecond)
	e := newMatteRig(t, 1, Options{})
	e.f.setModel(func(ms *matte.ModelState) { ms.State = matte.StateLoading })
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	_, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview)
	var pending *ErrMattePending
	if !errors.As(err, &pending) || pending.State != MattePendingLoading {
		t.Fatalf("preview while loading: %v", err)
	}
	e.f.mu.Lock()
	warms := len(e.f.warms)
	e.f.mu.Unlock()
	if warms == 0 {
		t.Error("no /v1/warm while the model loads")
	}
	// Downloading shows its percentage.
	e.f.setModel(func(ms *matte.ModelState) { ms.State = matte.StateDownloading; ms.Percent = 43 })
	time.Sleep(100 * time.Millisecond)
	if _, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview); !errors.As(err, &pending) || pending.State != MattePendingDownloading || pending.Percent != 43 {
		t.Errorf("preview while downloading: %v", err)
	}
	// Ready after a moment: a render waits it out, its listener having
	// seen the load state first.
	go func() {
		time.Sleep(700 * time.Millisecond) // a few progress polls (matteProgressPoll) in the load state
		e.f.setModel(func(ms *matte.ModelState) { ms.State = matte.StateReady; ms.Percent = 0 })
	}()
	var mu sync.Mutex
	var states []string
	ctx := withMatteProgress(e.ctx, func(p ErrMattePending) {
		mu.Lock()
		if n := len(states); n == 0 || states[n-1] != p.State {
			states = append(states, p.State)
		}
		mu.Unlock()
	})
	got, err := e.m.resolveMattes(ctx, clip, ops, out, matteModeRender)
	if err != nil || got[0].Manifest.Frames != 20 {
		t.Fatalf("render after loading: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) == 0 || states[0] != MattePendingDownloading && states[0] != MattePendingLoading {
		t.Errorf("render progress states = %v, want the load state first", states)
	}
	// An unavailable model refuses with the sidecar's reason; a missing one
	// after a failed download too. (A pass reuses a probe answer up to 15 s
	// old, so the state change is probed first, as the loop would.)
	e.f.setModel(func(ms *matte.ModelState) {
		ms.State = matte.StateUnavailable
		ms.Reason = "needs about 7 GB of free GPU memory"
	})
	e.m.probeMatte(e.ctx)
	trimmed := []recipe.Op{{Kind: recipe.OpTrim, Params: []byte(`{"start":1}`)}, matteOp("")}
	if _, err := e.m.resolveMattes(e.ctx, clip, trimmed, out, matteModeRender); !errors.Is(err, ErrMatteUnavailable) || !strings.Contains(err.Error(), "7 GB") {
		t.Errorf("unavailable model: %v", err)
	}
	e.f.setModel(func(ms *matte.ModelState) {
		ms.State = matte.StateMissing
		ms.Reason = ""
		ms.LastError = "download failed: 404"
	})
	e.m.probeMatte(e.ctx)
	if _, err := e.m.resolveMattes(e.ctx, clip, trimmed, out, matteModeRender); !errors.Is(err, ErrMatteUnavailable) || !strings.Contains(err.Error(), "download failed") {
		t.Errorf("missing model: %v", err)
	}
	if memos, tmps := matteDirs(t, e.st); len(memos) != 1 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v after the refusals", memos, tmps)
	}
}

// TestMatteMissingWhileQueuedIsPending: a model the sidecar reports
// "missing" with no reason and no lastError — its download not started
// yet, queued behind another model's load at startup (the sidecar has one
// sequential loader thread and /v1/warm is a no-op while queued) — is a
// pending download for EVERY poll, not a refusal on the second one: a
// preview answers 202 state downloading for as long as it stays so, a
// render waits it out through missing → downloading → ready, and
// /v1/warm was posted.
func TestMatteMissingWhileQueuedIsPending(t *testing.T) {
	shortMatteWaits(t, 100*time.Millisecond, 2*time.Second, 30*time.Millisecond)
	e := newMatteRig(t, 1, Options{})
	e.f.setModel(func(ms *matte.ModelState) {
		ms.State, ms.Reason, ms.LastError, ms.Percent = matte.StateMissing, "", "", 0
	})
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	_, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview)
	var pending *ErrMattePending
	if !errors.As(err, &pending) || pending.State != MattePendingDownloading {
		t.Fatalf("preview while missing (queued): %v", err)
	}
	e.f.mu.Lock()
	pings0, warms := e.f.pings, len(e.f.warms)
	e.f.mu.Unlock()
	if warms == 0 {
		t.Error("no /v1/warm for a queued model")
	}
	// Several more polls in the bare "missing" state: still pending.
	time.Sleep(200 * time.Millisecond)
	if _, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview); !errors.As(err, &pending) || pending.State != MattePendingDownloading {
		t.Fatalf("preview after more polls in the missing state: %v (want still pending)", err)
	}
	e.f.mu.Lock()
	polls := e.f.pings - pings0
	e.f.mu.Unlock()
	if polls < 3 {
		t.Fatalf("%d re-pings in the missing state, want at least 3 for the test to mean anything", polls)
	}
	// The download starts, then the model is ready: a render waits it out,
	// its listener having seen the download state first.
	go func() {
		time.Sleep(150 * time.Millisecond)
		e.f.setModel(func(ms *matte.ModelState) { ms.State, ms.Percent = matte.StateDownloading, 30 })
		time.Sleep(150 * time.Millisecond)
		e.f.setModel(func(ms *matte.ModelState) { ms.State, ms.Percent = matte.StateReady, 0 })
	}()
	var mu sync.Mutex
	var states []string
	ctx := withMatteProgress(e.ctx, func(p ErrMattePending) {
		mu.Lock()
		if n := len(states); n == 0 || states[n-1] != p.State {
			states = append(states, p.State)
		}
		mu.Unlock()
	})
	got, err := e.m.resolveMattes(ctx, clip, ops, out, matteModeRender)
	if err != nil || got[0].Manifest.Frames != 20 {
		t.Fatalf("render after the queued download: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) == 0 || states[0] != MattePendingDownloading {
		t.Errorf("render progress states = %v, want downloading first", states)
	}
	// A failed download is still a refusal with the sidecar's text.
	e.f.setModel(func(ms *matte.ModelState) {
		ms.State, ms.Reason, ms.LastError = matte.StateMissing, "download failed: 404", "download failed: 404"
	})
	e.m.probeMatte(e.ctx)
	trimmed := []recipe.Op{{Kind: recipe.OpTrim, Params: []byte(`{"start":1}`)}, matteOp("")}
	if _, err := e.m.resolveMattes(e.ctx, clip, trimmed, out, matteModeRender); !errors.Is(err, ErrMatteUnavailable) || !strings.Contains(err.Error(), "download failed") {
		t.Errorf("missing after a failed download: %v", err)
	}
}

// TestMattePostFailureFailsThePass: a failing POST ends the pass at once
// with the sidecar's answer (a plain error), leaves no tmp dir and no
// memo, and a later pass starts from the frames store.
func TestMattePostFailureFailsThePass(t *testing.T) {
	e := newMatteRig(t, 1, Options{})
	e.f.set(func(f *fakeSidecar) { f.failStatus = http.StatusInternalServerError })
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	started := time.Now()
	_, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err == nil || isContextError(err) || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "fake failure") {
		t.Fatalf("failing POST: %v", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the failed pass took %s", took)
	}
	if posts, _ := e.f.stats(); posts != 1 {
		t.Errorf("%d POSTs after the first failed, want 1", posts)
	}
	memos, tmps := matteDirs(t, e.st)
	if len(memos) != 0 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v after a failed pass", memos, tmps)
	}
	if _, err := os.Stat(clip.Path); err != nil {
		t.Errorf("source blob: %v", err)
	}
	// 507 is the OOM verdict, with the sidecar's text.
	e.f.set(func(f *fakeSidecar) { f.failStatus = http.StatusInsufficientStorage })
	if _, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender); err == nil || !strings.Contains(err.Error(), "HTTP 507") {
		t.Errorf("507: %v", err)
	}
	// Working again: the pass completes.
	e.f.set(func(f *fakeSidecar) { f.failStatus = 0 })
	if got, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender); err != nil || got[0].Manifest.Frames != 20 {
		t.Fatalf("recovered pass: %v", err)
	}
}

// TestMatteRunTimeFrameCap: a source whose frame count the plan cannot know
// is not refused up-front; the batch writer stops the pass at the cap with
// the refusal's message, leaving no tmp dir.
func TestMatteRunTimeFrameCap(t *testing.T) {
	e := newMatteRig(t, 1, Options{MatteMaxFrames: 5})
	clip := e.clipDistinct()
	info := *clip.Info
	info.Duration, info.Frames = 0, 0 // unknown length
	if err := e.st.SetBlobInfo(clip.Hash, info); err != nil {
		t.Fatal(err)
	}
	clip.Info = &info
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}
	_, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "more than 5 frames") || isContextError(err) {
		t.Fatalf("run-time cap: %v", err)
	}
	if memos, tmps := matteDirs(t, e.st); len(memos) != 0 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v after the cap", memos, tmps)
	}
	if posts, _ := e.f.stats(); posts != 0 {
		t.Errorf("%d POSTs before the cap (5 < a batch of 8)", posts)
	}
}

// TestMatteAbandonCancelsPass: a preview that leaves and never comes back
// lets the pass run only through the abandon grace; then ffmpeg and the
// POSTs stop, the tmp dir is removed and the progress cleared.
func TestMatteAbandonCancelsPass(t *testing.T) {
	shortMatteWaits(t, 50*time.Millisecond, 150*time.Millisecond, 30*time.Millisecond)
	e := newMatteRig(t, 1, Options{})
	e.f.set(func(f *fakeSidecar) { f.delay = 400 * time.Millisecond }) // 3 batches ≈ 1.2 s; cancelled before
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	_, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview)
	var pending *ErrMattePending
	if !errors.As(err, &pending) {
		t.Fatalf("preview: %v", err)
	}
	key := ""
	e.m.mt.mu.Lock()
	for k := range e.m.mt.progress {
		key = k
	}
	e.m.mt.mu.Unlock()
	if key == "" {
		t.Fatal("no pass in progress after the preview left")
	}
	waitFor(t, 10*time.Second, "the abandoned pass to end", func() bool { return !e.m.mt.flight.inFlight(key) })
	if _, ok := e.m.matteProgressFor(key); ok {
		t.Error("progress left behind by the abandoned pass")
	}
	posts, _ := e.f.stats()
	if posts >= 3 {
		t.Errorf("%d POSTs: the abandoned pass ran to the end", posts)
	}
	memos, tmps := matteDirs(t, e.st)
	if len(memos) != 0 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v after the abandon", memos, tmps)
	}
	// The next request starts a fresh pass that completes. A batch the
	// cancel cut off mid-flight was never answered, so it is posted again;
	// anything answered before is in the store and is not.
	e.f.set(func(f *fakeSidecar) { f.delay = 0 })
	got, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err != nil || got[0].Manifest.Frames != 20 {
		t.Fatalf("fresh pass: %v", err)
	}
	if _, frames := e.f.stats(); frames < 20 || frames > 20+matteBatchFrames {
		t.Errorf("%d frames posted in total, want 20 plus at most the one batch the cancel cut off", frames)
	}
}

// TestMatteWeightsChangeRekeys: the facts' weights are the key — a sidecar
// that answers other weights than the persisted facts re-keys the memo
// through the pass (the facts are rewritten, the memo filed under the new
// key, the old one untouched), and a resolved recipe hashes differently.
func TestMatteWeightsChangeRekeys(t *testing.T) {
	e := newMatteRig(t, 1, Options{})
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}
	first, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err != nil {
		t.Fatal(err)
	}
	opsA, _ := e.m.fillMatteResolved(ops)

	// The sidecar was upgraded: it now reports other weights, the facts
	// file still says the old ones.
	e.f.setModel(func(ms *matte.ModelState) { ms.Weights = otherWeights; ms.GraphDigest = "g-" + otherWeights })
	e.f.set(func(f *fakeSidecar) { f.ping.Instance = "fake-2" })
	// A hit under the old facts is still a hit (the memo exists).
	if got, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModePreview); err != nil || got[0].ClipKey != first[0].ClipKey {
		t.Fatalf("hit under the old facts: %v", err)
	}
	// A miss meets the live sidecar (its last answer is older than the
	// pass's 15 s reuse window here), which re-keys: the pass sees other
	// weights than its key was made from, the probe persists them, and the
	// memo lands under the new identity.
	expireMattePing(e.m)
	trimmed := []recipe.Op{{Kind: recipe.OpTrim, Params: []byte(`{"start":1}`)}, matteOp("")}
	got, err := e.m.resolveMattes(e.ctx, clip, trimmed, out, matteModeRender)
	if err != nil {
		t.Fatalf("re-keyed pass: %v", err)
	}
	if got[0].Manifest.Weights != otherWeights || got[0].Manifest.Frames != 10 {
		t.Errorf("re-keyed manifest = %+v", got[0].Manifest)
	}
	facts, err := e.m.loadMatteFacts()
	if err != nil || facts.Models[recipe.MatteModelISNetAnime].Weights != otherWeights {
		t.Errorf("facts after the re-key: %+v, %v", facts, err)
	}
	opsB, _ := e.m.fillMatteResolved(ops)
	ra := recipe.Recipe{Sources: []string{clip.Hash}, Ops: opsA, Output: out}
	rb := recipe.Recipe{Sources: []string{clip.Hash}, Ops: opsB, Output: out}
	if ra.Hash() == rb.Hash() {
		t.Error("the recipe hash did not change with the weights")
	}
	// The untrimmed clip under the new facts is a miss — a new memo beside
	// the old one. The frames store is per weights, so the old mattes do
	// not serve it: frames 1–10 are posted (8 + 2), frames 11–20 come from
	// what the trimmed pass just filed under the new weights.
	before, beforeFrames := e.f.stats()
	again, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err != nil || again[0].ClipKey == first[0].ClipKey {
		t.Fatalf("untrimmed under the new weights: %v", err)
	}
	after, afterFrames := e.f.stats()
	if after-before != 2 || afterFrames-beforeFrames != 10 {
		t.Errorf("%d POSTs of %d frames for the new weights, want 2 of 10 (the rest from the new store)", after-before, afterFrames-beforeFrames)
	}
	if memos, _ := matteDirs(t, e.st); len(memos) != 3 {
		t.Errorf("memo dirs %v, want 3", memos)
	}
	if _, err := os.Stat(filepath.Join(first[0].Dir, matte.ManifestName)); err != nil {
		t.Error("the old memo was removed")
	}
}
