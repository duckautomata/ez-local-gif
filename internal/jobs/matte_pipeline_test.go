package jobs

// Phase 5b: the matte op through the pipeline's public surface —
// StillSources, Proxy, Submit / the render job, ResolveAutoCropFor — against
// the httptest fake sidecar of matte_test.go (deterministic matte: 255 − luma
// of every model input) and a real ffmpeg: the spec §13 cases the
// pass-level tests (matte_test.go) and the memo-fixture tests
// (matte_integration_test.go) leave to the whole pipeline — ten concurrent
// stills sharing one pass and answering pending after the preview wait,
// a loading sidecar or a stopped one serving a memo, a plain Play answering
// idle and the eager flag (the Compute matte button) starting the pass
// (Phase 5c), a render job that waits a loading model out
// with StageMatte progress, the job error naming a stopped sidecar, a
// cancelled job abandoning its pass, Concurrency = 1 with an AI render not
// blocking a plain one, the frames store serving an fps-upsampled clip at
// N/3 POSTs, the autocrop resolving to the matte's subject, a weights change
// re-keying stills and result keys — plus the rgba multiply and late-still
// time-band pixel/timing checks and the probe's cancellation rule. Every
// ffmpeg test skips without ffmpeg/ffprobe on PATH.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// fakeMatteOf is the alpha the fake sidecar gives a flat frame of colour
// (r, g, b): 255 − luma, exactly fakeMattePNG's pixel value.
func fakeMatteOf(r, g, b int) uint8 { return uint8(255 - (299*r+587*g+114*b)/1000) }

// distinctColour is the flat colour of frame n (0-based) of
// matteRig.clipDistinct (geq r='N*12':g='128':b='255-N*12').
func distinctColour(n int) (r, g, b int) { return n * 12, 128, 255 - n*12 }

// distinctMatte is the fake's matte of frame n of clipDistinct.
func distinctMatte(n int) uint8 { return fakeMatteOf(distinctColour(n)) }

// near8 reports whether a and b are within tol of each other.
func near8(a, b uint8, tol int) bool {
	d := int(a) - int(b)
	return d >= -tol && d <= tol
}

// flatInput is a size×size rgb24 model input of one colour.
func flatInput(size, r, g, b int) []byte {
	rgb := make([]byte, size*size*3)
	for i := 0; i < size*size; i++ {
		rgb[3*i], rgb[3*i+1], rgb[3*i+2] = byte(r), byte(g), byte(b)
	}
	return rgb
}

// waitJob polls the job until it is finished (like waitFinished, with its
// own timeout: a pass behind a slow fake sidecar outlasts testTimeout).
func waitJob(t *testing.T, m *Manager, id string, timeout time.Duration) Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j, ok := m.Get(id)
		if !ok {
			t.Fatalf("job %s vanished", id)
		}
		if j.IsFinished() {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish within %s", id, timeout)
	return Job{}
}

// framePixels reads pixel (x, y) of every frame of a frames export as
// straight RGBA, in order.
func (e *e2e) framePixels(fin Job, x, y int) [][4]byte {
	e.t.Helper()
	files := e.frameFiles(fin)
	sort.Slice(files, func(i, j int) bool { return files[i].Index < files[j].Index })
	out := make([][4]byte, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(e.st.ResultDir(fin.RecipeHash), f.Name))
		if err != nil {
			e.t.Fatal(err)
		}
		pix, w, _ := pngPix(e.t, data)
		off := (y*w + x) * 4
		out = append(out, [4]byte{pix[off], pix[off+1], pix[off+2], pix[off+3]})
	}
	return out
}

// stageMessages lists the messages of the events at stage, in order.
func stageMessages(evs []Event, stage string) []string {
	var msgs []string
	for _, ev := range evs {
		if ev.Job.Stage == stage {
			msgs = append(msgs, ev.Job.Message)
		}
	}
	return msgs
}

// ---- the probe (no ffmpeg) -------------------------------------------------------

// TestProbeIgnoresCallerCancellation: a probe whose CALLER's ctx ended (an
// abandoned pass pinging before its first POST, a cancelled job, the server
// shutting down) is not a failed probe: it neither counts towards
// matteProbeFailures nor touches the state, so three abandoned previews can
// never flip features.matte off.
func TestProbeIgnoresCallerCancellation(t *testing.T) {
	st := newTestStore(t)
	f := newFakeSidecar(t, testWeights, 1)
	m := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL})
	if _, err := m.probeMatte(context.Background()); err != nil || !m.MatteEnabled() {
		t.Fatalf("first probe: %v, enabled %v", err, m.MatteEnabled())
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i <= matteProbeFailures; i++ {
		if _, err := m.probeMatte(cctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("probe %d under a cancelled ctx: %v, want context.Canceled", i, err)
		}
	}
	if !m.MatteEnabled() {
		t.Fatal("cancelled probes flipped AI mattes off")
	}
	m.mt.mu.Lock()
	failures, liveErr := m.mt.failures, m.mt.liveErr
	m.mt.mu.Unlock()
	if failures != 0 || liveErr != "" {
		t.Errorf("cancelled probes were recorded: failures %d, liveErr %q", failures, liveErr)
	}
	if s := m.MatteStatus(); !s.Enabled || s.Reason != "" {
		t.Errorf("status after cancelled probes = %+v", s)
	}
	// The same through a pass's wait for the model (its cached answer
	// expired, so it pings): a cancelled ctx is the caller's context error,
	// not "sidecar unreachable".
	expireMattePing(m)
	if _, _, _, err := m.waitMatteReady(cctx, "k", recipe.MatteModelISNetAnime, matte.DeviceCPU); !errors.Is(err, context.Canceled) || errors.Is(err, ErrMatteUnavailable) {
		t.Errorf("waitMatteReady under a cancelled ctx: %v", err)
	}
	if !m.MatteEnabled() {
		t.Error("a cancelled wait flipped AI mattes off")
	}
	// A real failure still counts.
	f.srv.Close()
	for i := 0; i < matteProbeFailures; i++ {
		m.probeMatte(context.Background())
	}
	if m.MatteEnabled() {
		t.Error("three real failures left AI mattes on")
	}
}

// ---- stills and proxies (real ffmpeg + the fake sidecar) ----------------------------

// TestStillsShareOneMattePass: ten concurrent stills of one recipe start ONE
// pass and each answers *ErrMattePending running once the preview wait (2 s)
// has passed; polling like the SPA then yields the picture, whose alpha is
// the fake's matte of the frame; the memo then serves the stills while the
// model is loading and while the sidecar is stopped; and a sidecar with other
// weights re-keys the still (the memoised PNG is not served: a new pass runs).
func TestStillsShareOneMattePass(t *testing.T) {
	e := newMatteRig(t, 1, Options{})
	e.f.set(func(f *fakeSidecar) { f.delay = 1200 * time.Millisecond }) // 3 batches ≈ 3.6 s > mattePreviewWait
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "gif", FPS: 10}
	const slot = 3 // t 0.35 at 10 fps

	type answer struct {
		err  error
		took time.Duration
	}
	answers := make([]answer, 10)
	var wg sync.WaitGroup
	eager := WithMatteEager(e.ctx, true) // the Compute matte button (Phase 5c: a plain still never starts a pass)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			_, err := e.m.StillSources(eager, []string{clip.Hash}, ops, out, 0.35, 0)
			answers[i] = answer{err: err, took: time.Since(start)}
		}(i)
	}
	wg.Wait()
	for i, a := range answers {
		var pending *ErrMattePending
		if !errors.As(a.err, &pending) {
			t.Fatalf("still %d: %v, want pending (the pass takes ~3.6 s, the wait is %s)", i, a.err, mattePreviewWait)
		}
		if pending.State != MattePendingRunning || pending.Total != 20 || pending.Device != matte.DeviceCPU {
			t.Errorf("still %d: pending %+v", i, pending)
		}
		if a.took < mattePreviewWait || a.took > mattePreviewWait+1500*time.Millisecond {
			t.Errorf("still %d answered pending after %s, want about the %s preview wait", i, a.took, mattePreviewWait)
		}
	}

	// Re-join every 500 ms like the SPA until the picture arrives.
	var png []byte
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		png, err = e.m.StillSources(e.ctx, []string{clip.Hash}, ops, out, 0.35, 0)
		if err == nil {
			break
		}
		var pending *ErrMattePending
		if !errors.As(err, &pending) {
			t.Fatalf("re-join: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pass never finished: %+v", pending)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("%d POSTs of %d frames across ten stills, want one pass (3 of 20)", posts, frames)
	}
	if memos, tmps := matteDirs(t, e.st); len(memos) != 1 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v", memos, tmps)
	}
	pix, _, _ := pngPix(t, png)
	r, g, b := distinctColour(slot)
	if !near8(pix[3], distinctMatte(slot), 1) || !near8(pix[0], uint8(r), 1) || !near8(pix[1], uint8(g), 1) || !near8(pix[2], uint8(b), 1) {
		t.Errorf("still pixel = %v, want colour (%d,%d,%d) with the fake's matte %d", pix[:4], r, g, b, distinctMatte(slot))
	}

	// The model loading: the memo serves the still without a pending state
	// or a warm-up; so does a stopped sidecar.
	e.f.setModel(func(ms *matte.ModelState) { ms.State = matte.StateLoading })
	e.m.probeMatte(e.ctx)
	still := func(t *testing.T, sl int) {
		t.Helper()
		data, err := e.m.StillSources(e.ctx, []string{clip.Hash}, ops, out, (float64(sl)+0.5)/10, 0)
		if err != nil {
			t.Fatalf("still %d: %v", sl, err)
		}
		if pix, _, _ := pngPix(t, data); !near8(pix[3], distinctMatte(sl), 1) {
			t.Errorf("still %d alpha %d, want %d", sl, pix[3], distinctMatte(sl))
		}
	}
	still(t, 5)
	e.f.mu.Lock()
	warms := len(e.f.warms)
	e.f.mu.Unlock()
	if warms != 0 {
		t.Errorf("a memo hit posted /v1/warm %d times", warms)
	}
	e.f.srv.Close()
	still(t, 7)
	if p, _ := e.f.stats(); p != 3 {
		t.Errorf("the memo hits posted (%d POSTs now)", p)
	}

	// A sidecar with other weights: the facts change on its first probe, the
	// clip key with them, and the still key folds the clip key in — the PNG
	// memoised under the old weights is not served, a new pass runs.
	other := newFakeSidecar(t, otherWeights, 1)
	m2 := NewManager(e.st, e.tools, Options{Concurrency: 2, MatteURL: other.srv.URL})
	if _, err := m2.probeMatte(e.ctx); err != nil {
		t.Fatal(err)
	}
	// A plain still is idle under the new identity (never served from the
	// old weights' memo); the eager one runs the new pass.
	var idle *ErrMattePending
	if _, err := m2.StillSources(e.ctx, []string{clip.Hash}, ops, out, 0.35, 0); !errors.As(err, &idle) || idle.State != MattePendingIdle {
		t.Fatalf("plain still under the new weights: %v, want idle", err)
	}
	data, err := m2.StillSources(WithMatteEager(e.ctx, true), []string{clip.Hash}, ops, out, 0.35, 0)
	if err != nil {
		t.Fatalf("still under the new weights: %v", err)
	}
	if posts, frames := other.stats(); posts != 3 || frames != 20 {
		t.Errorf("the new sidecar saw %d POSTs of %d frames, want a full pass (3 of 20): the still was served from a memo made with the old weights", posts, frames)
	}
	if pix, _, _ := pngPix(t, data); !near8(pix[3], distinctMatte(slot), 1) {
		t.Errorf("still alpha under the new weights %d, want %d", pix[3], distinctMatte(slot))
	}
	if memos, _ := matteDirs(t, e.st); len(memos) != 2 {
		t.Errorf("memo dirs %v, want one per weights", memos)
	}
}

// TestProxyMatteEagerViaSidecar: a plain proxy (Play) with no memo and no
// pass in flight answers idle (no POST, Phase 5c) whatever the estimate;
// an eager request — the Compute matte button — starts the pass and, once
// it is done, plays the clip; the memo then serves a plain request.
func TestProxyMatteEagerViaSidecar(t *testing.T) {
	e := newMatteRig(t, 10_000, Options{}) // 10 s per frame → 200 s estimate: idle is not about the cost
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "gif", FPS: 10}

	_, err := e.m.Proxy(e.ctx, []string{clip.Hash}, ops, out, 360, 10)
	var pending *ErrMattePending
	if !errors.As(err, &pending) || pending.State != MattePendingIdle || pending.Total != 20 || pending.EstimateMS != 20*10_002 {
		t.Fatalf("plain Play: %v (%+v), want idle", err, pending)
	}
	if posts, _ := e.f.stats(); posts != 0 {
		t.Errorf("an idle proxy posted %d batches", posts)
	}
	eager := WithMatteEager(e.ctx, true)
	data, err := e.m.Proxy(eager, []string{clip.Hash}, ops, out, 360, 10)
	if err != nil {
		t.Fatalf("eager Play: %v", err)
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Errorf("proxy is not a WebP (%d bytes)", len(data))
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("eager Play posted %d batches of %d frames, want 3 of 20", posts, frames)
	}
	again, err := e.m.Proxy(e.ctx, []string{clip.Hash}, ops, out, 360, 10)
	if err != nil || !bytes.Equal(again, data) {
		t.Errorf("plain Play after the pass: %v, same bytes %v", err, bytes.Equal(again, data))
	}
	if posts, _ := e.f.stats(); posts != 3 {
		t.Errorf("the memo hit posted (%d POSTs now)", posts)
	}
}

// ---- render jobs ----------------------------------------------------------------

// TestRenderJobMatteViaSidecar: a render job of a matte recipe waits a
// loading model out at StageMatte (the job line shows the load state, then
// the frames; the percent never goes backwards), delivers frames whose alpha
// is the fake's matte of each frame, is served from the cache the second
// time, and a sidecar with other weights makes it a new result under a new
// key with a new pass.
func TestRenderJobMatteViaSidecar(t *testing.T) {
	shortMatteWaits(t, mattePreviewWait, matteAbandonGrace, 50*time.Millisecond)
	e := newMatteRig(t, 1, Options{Concurrency: 2})
	e.f.setModel(func(ms *matte.ModelState) { ms.State = matte.StateLoading })
	clip := e.clipDistinct()
	r := recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{matteOp("")}, Output: recipe.Output{Format: recipe.FormatFrames, FPS: 10}}
	go func() {
		time.Sleep(700 * time.Millisecond)
		e.f.setModel(func(ms *matte.ModelState) { ms.State = matte.StateReady })
	}()
	j, err := e.m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	ch, unsub, _ := e.m.Subscribe(j.ID)
	defer unsub()
	evs := drain(t, ch)
	fin := evs[len(evs)-1].Job
	if fin.State != StateDone {
		t.Fatalf("job: %s %q (stage %s)", fin.State, fin.Error, fin.Stage)
	}
	msgs := stageMessages(evs, StageMatte)
	if len(msgs) == 0 {
		t.Fatalf("no event at stage %q in %d events", StageMatte, len(evs))
	}
	sawLoading, sawFrames := false, false
	for _, msg := range msgs {
		sawLoading = sawLoading || strings.Contains(msg, "loading model")
		sawFrames = sawFrames || strings.Contains(msg, "/20 · CPU")
	}
	if !sawLoading || !sawFrames {
		t.Errorf("matte stage messages %q: want the load state and the frame progress", msgs)
	}
	last := -1.0
	for i, ev := range evs {
		if ev.Job.Percent < last {
			t.Errorf("event %d (%s %q): percent %.1f after %.1f", i, ev.Job.Stage, ev.Job.Message, ev.Job.Percent, last)
		}
		last = ev.Job.Percent
		if ev.Job.Stage == StageMatte && (ev.Job.Percent < pctMatteStart || ev.Job.Percent > pctMatteEnd) {
			t.Errorf("event %d: matte stage at %.1f %%, want within %v..%v", i, ev.Job.Percent, pctMatteStart, pctMatteEnd)
		}
	}
	if fin.Started.IsZero() || fin.Finished.Before(fin.Started) {
		t.Errorf("job times: started %v, finished %v", fin.Started, fin.Finished)
	}
	e.f.mu.Lock()
	warms := len(e.f.warms)
	e.f.mu.Unlock()
	if warms == 0 {
		t.Error("no /v1/warm while the model loaded")
	}
	got := e.framePixels(fin, 3, 3)
	if len(got) != 20 {
		t.Fatalf("%d frames, want 20", len(got))
	}
	for i, px := range got {
		r, g, b := distinctColour(i)
		if !near8(px[3], distinctMatte(i), 1) || !near8(px[0], uint8(r), 1) || !near8(px[1], uint8(g), 1) || !near8(px[2], uint8(b), 1) {
			t.Errorf("frame %d = %v, want colour (%d,%d,%d) with the fake's matte %d", i, px, r, g, b, distinctMatte(i))
		}
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("%d POSTs of %d frames, want 3 of 20", posts, frames)
	}

	// Same recipe again: the cached result, no pass.
	j2, err := e.m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	if j2.State != StateDone || j2.Result == nil || !j2.Result.Cached || j2.RecipeHash != fin.RecipeHash {
		t.Errorf("second submit = %s cached %v hash equal %v", j2.State, j2.Result != nil && j2.Result.Cached, j2.RecipeHash == fin.RecipeHash)
	}
	if posts, _ := e.f.stats(); posts != 3 {
		t.Errorf("the cached result posted (%d POSTs now)", posts)
	}

	// The sidecar upgraded (other weights): the next probe rewrites the
	// facts, Submit hashes the new identity — a new result key, rendered
	// with a new pass under the new weights.
	e.f.setModel(func(ms *matte.ModelState) { ms.Weights = otherWeights; ms.GraphDigest = "g-" + otherWeights })
	if _, err := e.m.probeMatte(e.ctx); err != nil {
		t.Fatal(err)
	}
	j3, err := e.m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	if j3.RecipeHash == fin.RecipeHash {
		t.Fatal("a weights change did not change the result key")
	}
	if res := matteResolvedOf(t, j3.Recipe.Ops); res == nil || res.Weights != otherWeights {
		t.Errorf("job recipe Resolved = %+v, want the new weights", res)
	}
	fin3 := waitJob(t, e.m, j3.ID, 30*time.Second)
	if fin3.State != StateDone {
		t.Fatalf("job under the new weights: %s %q", fin3.State, fin3.Error)
	}
	if posts, frames := e.f.stats(); posts != 6 || frames != 40 {
		t.Errorf("%d POSTs of %d frames after the weights change, want a second full pass (6 of 40)", posts, frames)
	}
	if got := e.framePixels(fin3, 3, 3); len(got) != 20 || !near8(got[4][3], distinctMatte(4), 1) {
		t.Errorf("frames under the new weights: %d, frame 4 = %v", len(got), got[min(4, len(got)-1)])
	}
}

// TestRenderJobMatteSidecarDown: with the memo not on disk and the sidecar
// stopped, the render job fails at the matte stage with an error naming the
// sidecar and the profile, leaving no tmp dir; the facts are still there, so
// a later sidecar at the same facts picks the recipe up.
func TestRenderJobMatteSidecarDown(t *testing.T) {
	e := newMatteRig(t, 1, Options{Concurrency: 2})
	clip := e.clipDistinct()
	r := recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{matteOp("")}, Output: recipe.Output{Format: "webp", FPS: 10}}
	e.f.srv.Close()
	fin := runJob(t, e.m, r)
	if fin.State != StateError || fin.Stage != StageMatte {
		t.Fatalf("job = %s at stage %s: %q", fin.State, fin.Stage, fin.Error)
	}
	for _, want := range []string{"unreachable", "matte profile", e.f.srv.URL} {
		if !strings.Contains(fin.Error, want) {
			t.Errorf("job error %q lacks %q", fin.Error, want)
		}
	}
	if memos, tmps := matteDirs(t, e.st); len(memos) != 0 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v after the failed job", memos, tmps)
	}
	// Submit itself still accepts the recipe (the facts exist); the
	// pipeline, not the API, reports the outage.
	j2, err := e.m.Submit(r)
	if err != nil {
		t.Fatalf("Submit with the sidecar down: %v", err)
	}
	if fin2 := waitFinished(t, e.m, j2.ID); fin2.State != StateError || fin2.Stage != StageMatte {
		t.Errorf("second job = %s at stage %s: %q", fin2.State, fin2.Stage, fin2.Error)
	}
}

// TestRenderJobMatteCancelAbandonsPass: cancelling a render job during its
// pass ends the job at once; the pass, with no waiter left, is cancelled
// after the abandon grace (no memo, no tmp dir, fewer than all its POSTs),
// features.matte stays on, and the next job of the recipe runs a fresh pass
// to completion.
func TestRenderJobMatteCancelAbandonsPass(t *testing.T) {
	shortMatteWaits(t, mattePreviewWait, 300*time.Millisecond, 50*time.Millisecond)
	e := newMatteRig(t, 1, Options{Concurrency: 2})
	e.f.set(func(f *fakeSidecar) { f.delay = 800 * time.Millisecond }) // 3 batches ≈ 2.4 s
	if _, err := e.m.probeMatte(e.ctx); err != nil || !e.m.MatteEnabled() {
		t.Fatalf("probe: %v", err)
	}
	clip := e.clipDistinct()
	r := recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{matteOp("")}, Output: recipe.Output{Format: "webp", FPS: 10}}
	j, err := e.m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	var key string
	waitFor(t, 10*time.Second, "the job's pass to start", func() bool {
		e.m.mt.mu.Lock()
		defer e.m.mt.mu.Unlock()
		for k := range e.m.mt.progress {
			key = k
		}
		return key != ""
	})
	if got, _ := e.m.Get(j.ID); got.Stage != StageMatte || got.State != StateRunning {
		t.Fatalf("job while the pass runs: %s at %s", got.State, got.Stage)
	}
	cancelled := time.Now()
	if !e.m.Cancel(j.ID) {
		t.Fatal("Cancel returned false")
	}
	fin := waitFinished(t, e.m, j.ID)
	if fin.State != StateError || fin.Error != "cancelled" {
		t.Errorf("cancelled job = %s %q", fin.State, fin.Error)
	}
	if took := time.Since(cancelled); took > 2*time.Second {
		t.Errorf("the job took %s to end after Cancel", took)
	}
	waitFor(t, 15*time.Second, "the abandoned pass to end", func() bool { return !e.m.mt.flight.inFlight(key) })
	if _, ok := e.m.matteProgressFor(key); ok {
		t.Error("progress left behind by the abandoned pass")
	}
	if posts, _ := e.f.stats(); posts >= 3 {
		t.Errorf("%d POSTs: the abandoned pass ran to the end", posts)
	}
	if memos, tmps := matteDirs(t, e.st); len(memos) != 0 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v after the abandon", memos, tmps)
	}
	if !e.m.MatteEnabled() {
		t.Error("the cancelled job flipped AI mattes off")
	}
	e.f.set(func(f *fakeSidecar) { f.delay = 0 })
	fin2 := waitJob(t, e.m, mustSubmit(t, e.m, r).ID, 30*time.Second)
	if fin2.State != StateDone {
		t.Fatalf("job after the abandon: %s %q", fin2.State, fin2.Error)
	}
	if memos, _ := matteDirs(t, e.st); len(memos) != 1 {
		t.Errorf("memo dirs %v, want 1", memos)
	}
}

// mustSubmit submits r or fails the test.
func mustSubmit(t *testing.T, m *Manager, r recipe.Recipe) Job {
	t.Helper()
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// TestMattePassOutlivesFFmpeg: a sidecar slower than ffmpeg — its POST
// takes longer than exec's 2 s WaitDelay after ffmpeg has written the
// whole (small) clip into the pipe and exited — still gets every frame:
// the pass reads the producer at its own pace (ffrun.RunFrom), so nothing
// is lost to exec.ErrWaitDelay. CPU lite at seconds per frame, a busy
// sidecar or a session re-created inside the request are this case.
func TestMattePassOutlivesFFmpeg(t *testing.T) {
	e := newMatteRig(t, 1, Options{})
	e.f.set(func(f *fakeSidecar) { f.delay = 2500 * time.Millisecond }) // > ffrun's WaitDelay
	// The first POST (frames 1-8, sent from inside the writer while the
	// 15 KB clip already sits in the pipe and ffmpeg has exited) is the
	// slow one; the later batches need not be.
	go func() {
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if posts, _ := e.f.stats(); posts >= 1 {
				e.f.set(func(f *fakeSidecar) { f.delay = 0 })
				return
			}
		}
	}()
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}
	start := time.Now()
	mattes, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if took := time.Since(start); took < 2500*time.Millisecond {
		t.Errorf("the pass took %s: the sidecar was not slower than ffmpeg's WaitDelay, so this proved nothing", took)
	}
	if err != nil {
		t.Fatalf("pass behind a slow sidecar: %v", err)
	}
	if mattes[0].Manifest.Frames != 20 {
		t.Errorf("manifest has %d frames, want 20", mattes[0].Manifest.Frames)
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("%d POSTs of %d frames, want 3 of 20", posts, frames)
	}
	for i := 0; i < 20; i++ {
		r, g, b := distinctColour(i)
		want := fakeMattePNG(flatInput(fakeMatteSize, r, g, b), fakeMatteSize)
		if got, err := os.ReadFile(filepath.Join(mattes[0].Dir, matte.FrameFile(i+1))); err != nil || !bytes.Equal(got, want) {
			t.Errorf("slot %d is not the fake's matte of frame %d (%v)", i+1, i, err)
		}
	}
}

// TestMattePostRetriesLoading503: a batch the sidecar answers with 503
// "model loading" + retryAfterMs (its TTL unloaded the model, or an OOM
// made it release another one) is retried after that delay until it is
// served — the pass completes with every frame; a 503 without a retry
// delay is the pass's failure.
func TestMattePostRetriesLoading503(t *testing.T) {
	e := newMatteRig(t, 1, Options{})
	e.f.set(func(f *fakeSidecar) { f.retry503 = 2 })
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}
	start := time.Now()
	mattes, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err != nil {
		t.Fatalf("pass with two 503 answers: %v", err)
	}
	if took := time.Since(start); took < 400*time.Millisecond {
		t.Errorf("the pass took %s: the two retryAfterMs waits of 200 ms were not honoured", took)
	}
	if mattes[0].Manifest.Frames != 20 {
		t.Errorf("manifest has %d frames, want 20", mattes[0].Manifest.Frames)
	}
	// The first batch was POSTed three times (two 503s, then served), the
	// other two once: 5 POSTs carrying 8+8+8 + 8+4 frames.
	if posts, frames := e.f.stats(); posts != 5 || frames != 36 {
		t.Errorf("%d POSTs of %d frames, want 5 of 36 (the first batch re-sent twice)", posts, frames)
	}
	for i := 0; i < 20; i++ {
		r, g, b := distinctColour(i)
		want := fakeMattePNG(flatInput(fakeMatteSize, r, g, b), fakeMatteSize)
		if got, err := os.ReadFile(filepath.Join(mattes[0].Dir, matte.FrameFile(i+1))); err != nil || !bytes.Equal(got, want) {
			t.Errorf("slot %d is not the fake's matte of frame %d (%v)", i+1, i, err)
		}
	}
	// A 503 that names no retry delay is a failure like any other status
	// (on a clip whose frames the store does not hold yet).
	e.f.set(func(f *fakeSidecar) { f.failStatus = http.StatusServiceUnavailable })
	if _, err := e.m.resolveMattes(e.ctx, e.clipHeld(), ops, out, matteModeRender); err == nil || !strings.Contains(err.Error(), "HTTP 503") || isContextError(err) {
		t.Errorf("503 without retryAfterMs: %v", err)
	}
	if _, tmps := matteDirs(t, e.st); len(tmps) != 0 {
		t.Errorf("tmp dirs left by the failed pass: %v", tmps)
	}
}

// TestMatteHardTimeoutEndsThePass: a pass that outlives its hard timeout
// (min(2 x EZLG_MATTE_MAX_SECONDS, 3 x estimate + 60 s)) ends there with a
// plain error naming the knob — ffmpeg and the POST in flight cut off, the
// tmp dir removed — rather than running on behind a sidecar that has
// slowed to a crawl.
func TestMatteHardTimeoutEndsThePass(t *testing.T) {
	e := newMatteRig(t, 1, Options{MatteMaxSeconds: 1})                 // hard timeout 2 s; the 60 ms estimate is well under the 1 s cap
	e.f.set(func(f *fakeSidecar) { f.delay = 1500 * time.Millisecond }) // 3 batches ≈ 4.5 s
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}
	start := time.Now()
	_, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	took := time.Since(start)
	if err == nil || isContextError(err) || !strings.Contains(err.Error(), "hard timeout") || !strings.Contains(err.Error(), "EZLG_MATTE_MAX_SECONDS") {
		t.Fatalf("pass over its hard timeout: %v", err)
	}
	if took < 2*time.Second || took > 4*time.Second {
		t.Errorf("the pass ended after %s, want about its 2 s hard timeout", took)
	}
	if posts, _ := e.f.stats(); posts > 2 {
		t.Errorf("%d POSTs: the pass ran to the end", posts)
	}
	if memos, tmps := matteDirs(t, e.st); len(memos) != 0 || len(tmps) != 0 {
		t.Errorf("memo dirs %v, tmp dirs %v after the timeout", memos, tmps)
	}
	if !e.m.MatteEnabled() && e.m.MatteStatus().Reason != "" {
		// Never probed by this manager on its own; the pass's ping enabled it.
		t.Errorf("status after the timeout: %+v", e.m.MatteStatus())
	}
}

// TestAIRenderDoesNotBlockPlainRender: with one render slot, an AI render
// waiting on a slow sidecar holds no slot — a plain render submitted
// meanwhile completes while the AI render is still at its matte stage, and
// the AI render then completes too.
func TestAIRenderDoesNotBlockPlainRender(t *testing.T) {
	e := newMatteRig(t, 1, Options{Concurrency: 1})
	e.f.set(func(f *fakeSidecar) { f.delay = 1500 * time.Millisecond }) // 3 batches ≈ 4.5 s
	clip := e.clipDistinct()
	out := recipe.Output{Format: "webp", FPS: 10}
	ai := mustSubmit(t, e.m, recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{matteOp("")}, Output: out})
	waitFor(t, 10*time.Second, "the AI render to reach its matte stage", func() bool {
		j, _ := e.m.Get(ai.ID)
		return j.State == StateRunning && j.Stage == StageMatte
	})
	plainStart := time.Now()
	plain := runJob(t, e.m, recipe.Recipe{Sources: []string{clip.Hash}, Output: out})
	plainTook := time.Since(plainStart)
	if plain.State != StateDone {
		t.Fatalf("plain render: %s %q", plain.State, plain.Error)
	}
	if j, _ := e.m.Get(ai.ID); j.IsFinished() {
		t.Fatalf("the AI render finished (%s) before the plain one did (%s): the slow sidecar held nothing", j.State, plainTook)
	} else if j.Stage != StageMatte {
		t.Errorf("the AI render is at stage %s while the plain one ran, want %s", j.Stage, StageMatte)
	}
	fin := waitJob(t, e.m, ai.ID, 60*time.Second)
	if fin.State != StateDone {
		t.Fatalf("AI render: %s %q", fin.State, fin.Error)
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("%d POSTs of %d frames, want 3 of 20", posts, frames)
	}
}

// ---- the frames store and the autocrop ---------------------------------------------

// TestMatteFPSUpsampleHitsFramesStore: a 3× fps-upsampled clip streams 60
// frames of 20 distinct model inputs — N/3 POSTed frames, every triplet of
// slots the one answer — and the clip at its own rate afterwards POSTs
// nothing (every frame is in the store).
func TestMatteFPSUpsampleHitsFramesStore(t *testing.T) {
	e := newMatteRig(t, 1, Options{})
	clip := e.clipDistinct()
	out := recipe.Output{Format: "webp"}
	up := []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":30}`)}, matteOp("")}
	mattes, err := e.m.resolveMattes(e.ctx, clip, up, out, matteModeRender)
	if err != nil {
		t.Fatalf("upsampled pass: %v", err)
	}
	man := mattes[0].Manifest
	if man.Frames != 60 || man.FPS != "30" {
		t.Fatalf("manifest = %d frames at %s fps, want 60 at 30", man.Frames, man.FPS)
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("%d POSTs of %d frames for 60 slots, want 3 of 20 (N/3)", posts, frames)
	}
	for k := 0; k < 20; k++ {
		r, g, b := distinctColour(k)
		want := fakeMattePNG(flatInput(fakeMatteSize, r, g, b), fakeMatteSize)
		for j := 1; j <= 3; j++ {
			got, err := os.ReadFile(filepath.Join(mattes[0].Dir, matte.FrameFile(3*k+j)))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("slot %d is not the fake's matte of source frame %d (%v)", 3*k+j, k, err)
			}
		}
	}
	plain := []recipe.Op{matteOp("")}
	got, err := e.m.resolveMattes(e.ctx, clip, plain, out, matteModeRender)
	if err != nil || got[0].Manifest.Frames != 20 || got[0].ClipKey == mattes[0].ClipKey {
		t.Fatalf("pass at the source rate: %v, %+v", err, got)
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("the source-rate pass posted (%d POSTs of %d frames now), want every frame from the store", posts, frames)
	}
	for k := 0; k < 20; k++ {
		a, _ := os.ReadFile(filepath.Join(got[0].Dir, matte.FrameFile(k+1)))
		b, _ := os.ReadFile(filepath.Join(mattes[0].Dir, matte.FrameFile(3*k+1)))
		if len(a) == 0 || !bytes.Equal(a, b) {
			t.Errorf("source-rate slot %d differs from upsampled slot %d", k+1, 3*k+1)
		}
	}
}

// TestAutoCropMatteSubjectViaSidecar: crop to content on a matted clip
// resolves to the subject the sidecar's matte marks (a dark square over
// white → 255 − luma is the square), through a real pass, and the render
// crops to it with the matte as its alpha.
func TestAutoCropMatteSubjectViaSidecar(t *testing.T) {
	requireCropDetect(t)
	e := newMatteRig(t, 1, Options{Concurrency: 2})
	// A 64 px model input over a 64x64 clip: no stretch, the matte is pixel
	// exact.
	e.f.setModel(func(ms *matte.ModelState) {
		ms.Sizes, ms.DefaultSize, ms.MsPerFrame = []int{64}, 64, map[string]float64{"64": 1}
	})
	writeMatteFacts(t, e.st, &e.f.ping)
	clip := e.lavfi("subject.mov", "color=c=white:s=64x64:r=10:d=0.5[bg];color=c=black:s=32x32:r=10:d=0.5[fg];[bg][fg]overlay=16:16:format=rgb", "rgb24")
	if clip.Info.Frames != 5 || clip.Info.HasAlpha {
		t.Fatalf("clip probe: %+v", clip.Info)
	}
	out := recipe.Output{Format: "gif", FPS: 10}
	ops := []recipe.Op{matteOp(""), autocropOp(`{}`)}
	// A plain request is idle (nothing on disk, no pass); the eager one
	// runs the pass the detection then reads.
	var pending *ErrMattePending
	if _, err := e.m.ResolveAutoCropFor(e.ctx, clip.Hash, ops, out); !errors.As(err, &pending) || pending.State != MattePendingIdle {
		t.Fatalf("plain ResolveAutoCropFor: %v, want idle", err)
	}
	got, err := e.m.ResolveAutoCropFor(WithMatteEager(e.ctx, true), clip.Hash, ops, out)
	if err != nil {
		t.Fatalf("ResolveAutoCropFor: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 16, Y: 16, W: 32, H: 32}) {
		t.Errorf("resolved box = %+v, want the square's 32x32 at (16,16)", box)
	}
	if posts, frames := e.f.stats(); posts != 1 || frames != 1 {
		t.Errorf("%d POSTs of %d frames for a static 5-frame clip, want 1 of 1 (held frames ride on one answer)", posts, frames)
	}
	plain, err := e.m.ResolveAutoCropFor(e.ctx, clip.Hash, []recipe.Op{autocropOp(`{}`)}, out)
	if err != nil {
		t.Fatal(err)
	}
	if box := resolvedBox(t, plain); box != (recipe.CropParams{W: 64, H: 64}) {
		t.Errorf("plain box = %+v, want the full frame", box)
	}
	frames := recipe.Output{Format: recipe.FormatFrames, FPS: 10}
	fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: ops, Output: frames})
	if f := e.frameFiles(fin)[0]; f.Width != 32 || f.Height != 32 {
		t.Errorf("rendered frame %dx%d, want 32x32", f.Width, f.Height)
	}
	px := e.framePixels(fin, 1, 1)
	if len(px) != 5 || px[0] != [4]byte{0, 0, 0, 255} {
		t.Errorf("cropped frames: %d, first pixel %v (want 5 frames of the opaque black square)", len(px), px[0])
	}
}

// ---- pixels and timing (memo fixtures, no sidecar) -------------------------------

// TestRenderMatteMultipliesSourceAlpha: on an alpha-carrying main the matte
// is multiplied into the source alpha (never substituted) and the colour is
// untouched: half-transparent pixels end at a0 × matte / 255.
func TestRenderMatteMultipliesSourceAlpha(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("half.mov", "color=c=black:s=64x48:r=10:d=1,format=rgba,geq=r='48':g='96':b='192':a='128'", "rgba")
	if !clip.Info.HasAlpha || clip.Info.Frames != 10 {
		t.Fatalf("clip probe: %+v", clip.Info)
	}
	frames := recipe.Output{Format: recipe.FormatFrames, FPS: 10}
	base := e.framePixels(e.run(recipe.Recipe{Sources: []string{clip.Hash}, Output: frames}), 3, 3)
	if len(base) != 10 || base[0][3] < 120 || base[0][3] > 136 {
		t.Fatalf("baseline frames: %d, pixel %v (want a half-transparent source)", len(base), base[0])
	}
	ops := []recipe.Op{matteOp("")}
	flatMemo(t, e.st, clip, ops, frames, ping, 10)
	got := e.framePixels(e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: ops, Output: frames}), 3, 3)
	if len(got) != 10 {
		t.Fatalf("%d frames, want 10", len(got))
	}
	for i, px := range got {
		a0 := int(base[i][3])
		want := uint8((a0*int(matteGray(i)) + 127) / 255)
		if !near8(px[3], want, 1) {
			t.Errorf("frame %d alpha %d, want %d (%d x matte %d / 255)", i, px[3], want, a0, matteGray(i))
		}
		for c := 0; c < 3; c++ {
			if !near8(px[c], base[i][c], 1) {
				t.Errorf("frame %d colour %v, want the source's %v", i, px[:3], base[i][:3])
			}
		}
	}
}

// TestLateStillTimeBand: a still at t >= 30 s of a 50 fps clip with a matte
// lands in the same time band as the matte-less still — the forward still
// reads ONE PNG, never a looped sequence (spec §5.5: "-loop 1" would cost a
// late still seconds).
func TestLateStillTimeBand(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("long.mov", "color=c=0x4080C0:s=32x32:r=50:d=32", "rgb24")
	out := recipe.Output{Format: "gif", FPS: 50}
	ops := []recipe.Op{matteOp("")}
	_, _, n := matteKeyFor(t, clip, ops, out, ping, recipe.MatteModelISNetAnime, 0)
	if n < 1550 {
		t.Fatalf("matte plan has %d frames, want ~1600", n)
	}
	flatMemo(t, e.st, clip, ops, out, ping, n)
	best := func(ops []recipe.Op, at float64) (time.Duration, []byte) {
		t.Helper()
		var fastest time.Duration
		var last []byte
		for k := 0; k < 3; k++ {
			start := time.Now()
			data, err := e.m.StillSources(e.ctx, []string{clip.Hash}, ops, out, at+float64(k)*0.02, 0)
			took := time.Since(start)
			if err != nil {
				t.Fatalf("still at %.2f: %v", at+float64(k)*0.02, err)
			}
			if k == 0 || took < fastest {
				fastest = took
			}
			last = data
		}
		return fastest, last
	}
	plain, _ := best(nil, 30.5)
	matted, png := best(ops, 30.5)
	t.Logf("late still: plain %s, with a matte %s", plain, matted)
	if matted > 3*plain+750*time.Millisecond {
		t.Errorf("a late still with a matte took %s, the plain one %s: not the same time band", matted, plain)
	}
	// And it shows the matte of its slot (30.54 s → slot 1527).
	if pix, _, _ := pngPix(t, png); pix[3] != matteGray(1527) {
		t.Errorf("late still alpha %d, want matte %d", pix[3], matteGray(1527))
	}
}
