package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

const testTimeout = 10 * time.Second

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(filepath.Join(root, "data"), filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// fakeTools names binaries that do not exist so no process ever starts but
// the "tool missing" short-circuits are not taken.
var fakeTools = ffrun.Tools{FFmpeg: "ffmpeg-does-not-exist-ezlg-test", FFprobe: "ffprobe-does-not-exist-ezlg-test"}

func putSource(t *testing.T, st *store.Store, withInfo bool) string {
	t.Helper()
	b, err := st.PutBlob(bytes.NewReader([]byte("not really a video")), "clip.mov")
	if err != nil {
		t.Fatal(err)
	}
	if withInfo {
		info := recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p10le",
			Bits: 10, Width: 64, Height: 48, FPS: 25, Duration: 2, Frames: 50, HasAlpha: true, Kind: recipe.KindVideo, Premultiplied: true}
		if err := st.SetBlobInfo(b.Hash, info); err != nil {
			t.Fatal(err)
		}
	}
	return b.Hash
}

// drain reads events until the channel closes (or the timeout hits) and
// returns them.
func drain(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var evs []Event
	deadline := time.After(testTimeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for events; got %d so far", len(evs))
		}
	}
}

func waitFinished(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		j, ok := m.Get(id)
		if !ok {
			t.Fatalf("job %s vanished", id)
		}
		if j.IsFinished() {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return Job{}
}

// TestResultKey: the on-disk result identity is the recipe hash salted with
// the pipeline/rules versions — stable for equal recipes, distinct from the
// bare recipe hash, and different for different recipes.
func TestResultKey(t *testing.T) {
	src := strings.Repeat("ab", 32)
	a := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif"}}
	b := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "webp"}}
	ka, kb := ResultKey(a), ResultKey(b)
	if !recipe.IsHash(ka) || !recipe.IsHash(kb) {
		t.Fatalf("keys are not sha256 hex: %q %q", ka, kb)
	}
	if ka == a.Hash() {
		t.Fatal("result key must not equal the bare recipe hash (no pipeline salt)")
	}
	if ka != ResultKey(a) {
		t.Fatal("result key is not stable")
	}
	if ka == kb {
		t.Fatal("different recipes share a result key")
	}
	if PipelineVersion == "" || discordlint.RulesVersion == "" {
		t.Fatal("pipeline/rules version must be set")
	}
}

func TestSubmitValidation(t *testing.T) {
	m := NewManager(newTestStore(t), fakeTools, Options{Concurrency: 1})
	if _, err := m.Submit(recipe.Recipe{Output: recipe.Output{Format: "gif"}}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("no sources: err = %v", err)
	}
	src := strings.Repeat("a", 64)
	if _, err := m.Submit(recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "mp4"}}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unsupported format: err = %v", err)
	}
	for _, f := range []string{"gif", "webp", "apng", "avif", "png", "jpeg", "frames"} {
		if !supportedFormats[f] {
			t.Errorf("format %q must be supported", f)
		}
	}
	if _, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{{Kind: "crop", Params: json.RawMessage(`{bad`)}}, Output: recipe.Output{Format: "gif"}}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("bad params json: err = %v", err)
	}
	if m.Concurrency() != 1 {
		t.Errorf("Concurrency = %d", m.Concurrency())
	}
	if _, ok := m.Get("nope"); ok {
		t.Error("Get of unknown id succeeded")
	}
	if m.Cancel("nope") {
		t.Error("Cancel of unknown id succeeded")
	}
	if _, _, ok := m.Subscribe("nope"); ok {
		t.Error("Subscribe of unknown id succeeded")
	}
}

func TestJobUnknownSourceFails(t *testing.T) {
	m := NewManager(newTestStore(t), fakeTools, Options{Concurrency: 1})
	r := recipe.Recipe{Sources: []string{strings.Repeat("b", 64)}, Output: recipe.Output{Format: "gif"}}
	j, err := m.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(j.ID) != 32 {
		t.Errorf("id = %q, want 32 hex chars", j.ID)
	}
	if j.State != StateQueued || j.RecipeHash != ResultKey(r) {
		t.Errorf("submitted job = %+v", j)
	}
	ch, cancel, ok := m.Subscribe(j.ID)
	if !ok {
		t.Fatal("Subscribe failed")
	}
	defer cancel()
	evs := drain(t, ch)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	last := evs[len(evs)-1]
	if last.Type != EventError || last.Job.State != StateError {
		t.Errorf("last event = %+v", last)
	}
	if !strings.Contains(last.Job.Error, "not found") {
		t.Errorf("error = %q, want a 'not found' message", last.Job.Error)
	}
	if last.Job.Finished.IsZero() {
		t.Error("Finished not set")
	}
	got, _ := m.Get(j.ID)
	if got.State != StateError || got.Error != last.Job.Error {
		t.Errorf("Get after finish = %+v", got)
	}
	// Subscribing after the fact yields the final snapshot and closes.
	ch2, cancel2, ok := m.Subscribe(j.ID)
	if !ok {
		t.Fatal("Subscribe #2 failed")
	}
	defer cancel2()
	evs2 := drain(t, ch2)
	if len(evs2) != 1 || evs2[0].Type != EventError {
		t.Errorf("late subscribe events = %+v", evs2)
	}
	if m.Cancel(j.ID) {
		t.Error("Cancel of a finished job returned true")
	}
}

func TestJobSourceWithoutInfoFails(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, fakeTools, Options{})
	hash := putSource(t, st, false)
	j, err := m.Submit(recipe.Recipe{Sources: []string{hash}, Output: recipe.Output{Format: "webp"}})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "no probe info") {
		t.Errorf("job = %+v", fin)
	}
}

func TestJobWithInfoFailsWithoutFFmpeg(t *testing.T) {
	st := newTestStore(t)
	hash := putSource(t, st, true)
	r := recipe.Recipe{Sources: []string{hash}, Ops: []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}}, Output: recipe.Output{Format: "gif", Width: 32, Height: 32}}

	// Empty tools: refused before any process.
	m := NewManager(st, ffrun.Tools{}, Options{})
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	fin := waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("empty tools job = %+v", fin)
	}

	// Fake ffmpeg path: compile succeeds (or reports a clear error), the
	// master render fails; either way the job errors with a message and
	// subscribers see done/error + close.
	m = NewManager(st, fakeTools, Options{})
	j, err = m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := m.Subscribe(j.ID)
	defer cancel()
	evs := drain(t, ch)
	last := evs[len(evs)-1]
	if last.Type != EventError || last.Job.State != StateError || last.Job.Error == "" {
		t.Errorf("last = %+v", last)
	}
	if last.Job.Started.IsZero() {
		t.Error("Started not set on a job that ran")
	}
	// Scratch dir was cleaned up.
	entries, _ := os.ReadDir(st.Scratch)
	for _, e := range entries {
		if e.Name() == j.ID {
			t.Errorf("scratch dir %s left behind", e.Name())
		}
	}
}

func TestCancelQueuedJob(t *testing.T) {
	st := newTestStore(t)
	hash := putSource(t, st, true)
	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	// Occupy the only render slot so the job stays queued.
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	j, err := m.Submit(recipe.Recipe{Sources: []string{hash}, Output: recipe.Output{Format: "gif"}})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := m.Subscribe(j.ID)
	defer cancel()
	first := <-ch
	if first.Type != EventProgress || first.Job.State != StateQueued {
		t.Errorf("first event = %+v", first)
	}
	time.Sleep(20 * time.Millisecond)
	if got, _ := m.Get(j.ID); got.State != StateQueued {
		t.Fatalf("job should still be queued: %+v", got)
	}
	if !m.Cancel(j.ID) {
		t.Fatal("Cancel returned false for a queued job")
	}
	evs := drain(t, ch)
	if len(evs) == 0 {
		t.Fatal("no events after cancel")
	}
	last := evs[len(evs)-1]
	if last.Type != EventError || last.Job.State != StateError || last.Job.Error != "cancelled" {
		t.Errorf("after cancel: %+v", last)
	}
	if m.Cancel(j.ID) {
		t.Error("second Cancel returned true")
	}
}

func TestSubmitCachedResult(t *testing.T) {
	st := newTestStore(t)
	hash := putSource(t, st, true)
	r := recipe.Recipe{Sources: []string{hash}, Output: recipe.Output{Format: "gif", Target: "emote"}}
	rh := ResultKey(r)

	// Pre-existing result on disk.
	stage := filepath.Join(t.TempDir(), "stage")
	os.MkdirAll(stage, 0o755)
	os.WriteFile(filepath.Join(stage, "out.gif"), []byte("GIF89a..."), 0o644)
	res := Result{RecipeHash: rh, Recipe: r, Files: []File{{Name: "out.gif", URL: "/out/" + rh + "/out.gif", Format: "gif", Bytes: 9, Width: 64, Height: 48, Frames: 50, FPS: 25, Duration: 2, Limit: 262144}},
		Created: time.Now().UTC(), RenderMS: 1234}
	man, _ := json.Marshal(res)
	os.WriteFile(filepath.Join(stage, store.ManifestName), man, 0o644)
	if err := st.CommitResult(rh, stage); err != nil {
		t.Fatal(err)
	}

	m := NewManager(st, ffrun.Tools{}, Options{})
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != StateDone || j.Stage != StageDone || j.Percent != 100 || j.Result == nil {
		t.Fatalf("cached submit = %+v", j)
	}
	if !j.Result.Cached || j.Result.RenderMS != 0 || j.Result.Files[0].URL != "/out/"+rh+"/out.gif" {
		t.Errorf("cached result = %+v", j.Result)
	}
	if got, ok := m.Get(j.ID); !ok || got.State != StateDone {
		t.Errorf("Get cached = %+v %v", got, ok)
	}
	ch, cancel, ok := m.Subscribe(j.ID)
	if !ok {
		t.Fatal("subscribe cached")
	}
	defer cancel()
	evs := drain(t, ch)
	if len(evs) != 1 || evs[0].Type != EventDone {
		t.Errorf("cached subscribe events = %+v", evs)
	}
	loaded, err := m.LoadResult(rh)
	if err != nil || loaded.RenderMS != 1234 || loaded.Cached {
		t.Errorf("LoadResult = %+v, %v", loaded, err)
	}
	if _, err := m.LoadResult(strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadResult unknown: %v", err)
	}
	if _, err := m.LoadResult("bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadResult invalid: %v", err)
	}
}

func TestSubmitCorruptManifestRerenders(t *testing.T) {
	st := newTestStore(t)
	hash := putSource(t, st, true)
	r := recipe.Recipe{Sources: []string{hash}, Output: recipe.Output{Format: "gif"}}
	dir := st.ResultDir(ResultKey(r))
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, store.ManifestName), []byte("{corrupt"), 0o644)

	m := NewManager(st, ffrun.Tools{}, Options{})
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != StateQueued {
		t.Errorf("corrupt manifest must not be served: %+v", j)
	}
	if st.HasResult(ResultKey(r)) {
		t.Error("corrupt result dir not removed")
	}
	waitFinished(t, m, j.ID)
}

func TestSubscriberDropsOldest(t *testing.T) {
	m := NewManager(newTestStore(t), ffrun.Tools{}, Options{})
	j := &job{snap: Job{ID: "x", State: StateRunning, Stage: StageMaster}, cancel: func() {}, subs: map[int]*subscriber{}}
	m.mu.Lock()
	m.jobs["x"] = j
	m.mu.Unlock()
	ch, cancel, ok := m.Subscribe("x")
	if !ok {
		t.Fatal("subscribe")
	}
	defer cancel()
	// Publish far more than the buffer without reading: must not block.
	total := subscriberBuffer * 3
	for i := 1; i <= total; i++ {
		pct := float64(i)
		m.update(j, true, func(s *Job) { s.Percent = pct })
	}
	// The channel holds the initial snapshot slot replaced by newer events;
	// the newest event must be the last one published.
	var last Event
	n := 0
	for {
		select {
		case ev := <-ch:
			last = ev
			n++
			continue
		default:
		}
		break
	}
	if n != subscriberBuffer {
		t.Errorf("buffered %d events, want %d", n, subscriberBuffer)
	}
	if last.Job.Percent != float64(total) {
		t.Errorf("newest event percent = %v, want %v", last.Job.Percent, total)
	}
	// Coalescing: rapid non-forced updates publish at most one per interval.
	for i := 0; i < 50; i++ {
		m.progress(j, 1, "tick")
	}
	n = 0
	for {
		select {
		case <-ch:
			n++
			continue
		default:
		}
		break
	}
	if n > 2 {
		t.Errorf("coalescing failed: %d progress events in a burst", n)
	}
	// Cancel closes the channel and later publishes are harmless.
	cancel()
	if _, ok := <-ch; ok {
		t.Error("channel not closed after cancel")
	}
	m.update(j, true, func(s *Job) { s.Percent = 99 })
	m.fail(j, errors.New("boom"))
	if got, _ := m.Get("x"); got.State != StateError || got.Error != "boom" {
		t.Errorf("after fail: %+v", got)
	}
}

func TestProgressFraction(t *testing.T) {
	cases := []struct {
		p        ffrun.Progress
		frames   int
		duration float64
		want     float64
	}{
		{ffrun.Progress{Frame: 50}, 100, 0, 0.5},
		{ffrun.Progress{Frame: 500}, 100, 0, 1},
		{ffrun.Progress{OutTimeMS: 1500}, 0, 3, 0.5},
		{ffrun.Progress{OutTimeMS: 1500}, 0, 0, 0},
		{ffrun.Progress{Done: true}, 0, 0, 1},
		{ffrun.Progress{Frame: -1}, 10, 0, 0},
	}
	for _, c := range cases {
		if got := progressFraction(c.p, c.frames, c.duration); got != c.want {
			t.Errorf("progressFraction(%+v,%d,%v) = %v, want %v", c.p, c.frames, c.duration, got, c.want)
		}
	}
}

func TestScanMasterAlpha(t *testing.T) {
	dir := t.TempDir()
	// 3 chunks + a partial one, all opaque.
	size := masterAlphaChunk*3 + 4*1000
	buf := bytes.Repeat([]byte{0x10, 0x20, 0x30, 0xFF}, size/4)
	p := filepath.Join(dir, "opaque.rgba")
	os.WriteFile(p, buf, 0o644)
	if has, err := scanMasterAlpha(p); err != nil || has {
		t.Errorf("opaque: has=%v err=%v", has, err)
	}
	// One translucent pixel just past a chunk boundary.
	buf[masterAlphaChunk*2+7] = 0x80
	p2 := filepath.Join(dir, "alpha.rgba")
	os.WriteFile(p2, buf, 0o644)
	if has, err := scanMasterAlpha(p2); err != nil || !has {
		t.Errorf("alpha: has=%v err=%v", has, err)
	}
	// A non-alpha byte below 255 must not count.
	buf[masterAlphaChunk*2+7] = 0xFF
	buf[masterAlphaChunk*2+6] = 0x00
	p3 := filepath.Join(dir, "colour.rgba")
	os.WriteFile(p3, buf, 0o644)
	if has, _ := scanMasterAlpha(p3); has {
		t.Error("colour byte counted as alpha")
	}
	if _, err := scanMasterAlpha(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestStripOptimizeFlag(t *testing.T) {
	in := []string{"-U", "-O2", "--careful", "--loopcount=forever", "in.gif", "-o", "out.gif"}
	got := stripOptimizeFlag(in)
	want := []string{"-U", "--careful", "--loopcount=forever", "in.gif", "-o", "out.gif"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %q", got)
	}
	// -o and file names must survive.
	if strings.Join(stripOptimizeFlag([]string{"--optimize=3", "-o", "x-O2.gif"}), " ") != "-o x-O2.gif" {
		t.Error("stripped too much")
	}
}

// TestGifsicleLoopWiring pins the contract jobs relies on: Output.Loop goes
// into enc.GifsicleOptions.Loop, which enc renders as --loopcount=forever
// (0) or --loopcount=N — and the ladder's -U rung, which strips -O, keeps
// it. (The end-to-end count check is TestRenderGIFLoopCount.)
func TestGifsicleLoopWiring(t *testing.T) {
	forever := enc.GifsicleArgs("in.gif", "out.gif", enc.GifsicleOptions{Lossy: 40, Colors: 128, Loop: 0})
	if !slices.Contains(forever, "--loopcount=forever") {
		t.Errorf("loop 0: %q", forever)
	}
	three := enc.GifsicleArgs("in.gif", "out.gif", enc.GifsicleOptions{Lossy: 40, Colors: 128, Loop: 3})
	if !slices.Contains(three, "--loopcount=3") || slices.Contains(three, "--loopcount=forever") {
		t.Errorf("loop 3: %q", three)
	}
	if idx := slices.Index(three, "--loopcount=3"); idx < 0 || three[idx+1] != "in.gif" {
		t.Errorf("loop flag not before the input: %q", three)
	}
	un := stripOptimizeFlag(enc.GifsicleArgs("i", "o", enc.GifsicleOptions{Unoptimize: true, Loop: 5}))
	if !slices.Contains(un, "--loopcount=5") || slices.Contains(un, "-O2") {
		t.Errorf("-U rung: %q", un)
	}
}

func TestApplyMasterAlpha(t *testing.T) {
	// Structural flag agrees with the master: nothing to say.
	rep := discordlint.Report{HasAlpha: true, Checks: []discordlint.Check{{Rule: "gif.gce-every-frame", Level: discordlint.LevelError, OK: true}}, OK: true}
	applyMasterAlpha(&rep, enc.Master{HasAlpha: true})
	if !rep.HasAlpha || len(rep.Checks) != 1 {
		t.Errorf("agree(true): %+v", rep)
	}
	rep = discordlint.Report{HasAlpha: false, OK: true}
	applyMasterAlpha(&rep, enc.Master{HasAlpha: false})
	if rep.HasAlpha || len(rep.Checks) != 0 {
		t.Errorf("agree(false): %+v", rep)
	}
	// Frame-diff optimised opaque animation: structural true, master false →
	// false, with the structural verdict kept in an info check; OK unchanged.
	rep = discordlint.Report{HasAlpha: true, Checks: []discordlint.Check{{Rule: "gif.global-palette", Level: discordlint.LevelError, OK: true}}, OK: true}
	applyMasterAlpha(&rep, enc.Master{HasAlpha: false})
	if rep.HasAlpha {
		t.Error("opaque master must win over the structural flag")
	}
	if len(rep.Checks) != 2 || rep.Checks[1].Rule != RuleRenderAlpha || rep.Checks[1].Level != discordlint.LevelInfo || !rep.Checks[1].OK {
		t.Fatalf("checks = %+v", rep.Checks)
	}
	if !strings.Contains(rep.Checks[1].Detail, "opaque") || !strings.Contains(rep.Checks[1].Detail, "structural") {
		t.Errorf("detail = %q", rep.Checks[1].Detail)
	}
	if !rep.OK {
		t.Error("an info check must not fail the report")
	}
	// The other direction (alpha thresholded away in the file) is reported too.
	rep = discordlint.Report{HasAlpha: false, OK: true}
	applyMasterAlpha(&rep, enc.Master{HasAlpha: true})
	if !rep.HasAlpha || len(rep.Checks) != 1 || rep.Checks[0].Rule != RuleRenderAlpha || !strings.Contains(rep.Checks[0].Detail, "carry alpha") {
		t.Errorf("master alpha, structural none: %+v", rep)
	}
	// Report JSON round trip keeps the check (it is written to report.json).
	data, _ := json.Marshal(rep)
	var back discordlint.Report
	if err := json.Unmarshal(data, &back); err != nil || len(back.Checks) != 1 || back.Checks[0].Rule != RuleRenderAlpha {
		t.Errorf("round trip: %+v %v", back, err)
	}
}

func TestClampStillTime(t *testing.T) {
	// Frame count known: t is capped at the last frame's midpoint
	// (49.5/25 = 1.98), so t == Duration and anything beyond map to the last
	// frame's own memo entry.
	anim := &graph.Plan{Duration: 2, Frames: 50, FPS: 25}
	// Frame count unknown (only a duration): the old cap at Duration.
	durOnly := &graph.Plan{Duration: 2}
	// 34 frames at 33 ms: the midpoint of the last frame rounds to 1.106 ms,
	// which still maps to frame 33 (1.106*30.303 = 33.5).
	seq34 := &graph.Plan{Duration: 34 / 30.303, Frames: 34, FPS: 30.303}
	// 7 sequence frames at speed 2 resampled to 20 fps: 6 master frames even
	// though Duration*FPS is 7 — the cap follows Frames, not the duration.
	trunc := &graph.Plan{Duration: 0.35, Frames: 6, FPS: 20, Speed: 2}
	cases := []struct {
		t     float64
		still bool
		plan  *graph.Plan
		want  float64
	}{
		{0.5, false, anim, 0.5},
		{-1, false, anim, 0},
		{2.5, false, anim, 1.98},
		{2, false, anim, 1.98},
		{1.98, false, anim, 1.98},
		{1.979, false, anim, 1.979},
		{0.12345, false, anim, 0.123},
		{0.9996, false, anim, 1},
		{2.5, false, durOnly, 2},
		{0.5, false, durOnly, 0.5},
		{33.5 / 30.303, false, seq34, 1.106},
		{34 / 30.303, false, seq34, 1.106},
		{99, false, seq34, 1.106},
		{0.34, false, trunc, 0.275},
		{5.5 / 20, false, trunc, 0.275},
		{4.5 / 20, false, trunc, 0.225},
		// Unknown duration: only the lower bound and rounding apply.
		{7.25, false, &graph.Plan{}, 7.25},
		// Still source: every t is the one frame at 0 (the plan's duration is
		// 0 and would not clamp on its own).
		{0, true, &graph.Plan{Frames: 1}, 0},
		{3.7, true, &graph.Plan{Frames: 1}, 0},
		{-4, true, &graph.Plan{Frames: 1}, 0},
		// A one-frame plan from a non-still source: same thing.
		{1.5, false, &graph.Plan{Duration: 0.04, Frames: 1}, 0},
	}
	for _, c := range cases {
		if got := clampStillTime(c.plan, c.t, c.still); got != c.want {
			t.Errorf("clampStillTime(%+v, %v, still=%v) = %v, want %v", c.plan, c.t, c.still, got, c.want)
		}
	}
	// NaN → 0.
	if got := clampStillTime(anim, math.NaN(), false); got != 0 {
		t.Errorf("NaN → %v", got)
	}
	// The midpoint of the last frame maps to that frame (floor(t*FPS) ==
	// Frames-1) for every rate the output can run at, after the millisecond
	// rounding — the contract the frame-index scrubber relies on.
	for _, fps := range []float64{0.017, 1, 4, 10, 12.5, 16.7, 23.976, 25, 29.97, 30, 30.303, 33.333, 50, 60} {
		for _, frames := range []int{2, 3, 7, 34, 299, 600} {
			p := &graph.Plan{Duration: float64(frames) / fps, Frames: frames, FPS: fps}
			for _, tt := range []float64{(float64(frames) - 0.5) / fps, p.Duration, p.Duration + 1, 1e9} {
				got := clampStillTime(p, tt, false)
				if slot := int(math.Floor(got*fps + 1e-6)); slot != frames-1 {
					t.Errorf("fps %v, %d frames, t=%v: clamped to %v = slot %d, want the last frame %d", fps, frames, tt, got, slot, frames-1)
				}
			}
			// And the frame before it stays distinct.
			if got := clampStillTime(p, (float64(frames)-1.5)/fps, false); int(math.Floor(got*fps+1e-6)) != frames-2 {
				t.Errorf("fps %v, %d frames: (N-1.5)/fps clamped to %v, not frame %d", fps, frames, got, frames-2)
			}
		}
	}
}

// TestStillClampsStillSourceToZero: for a still source every t (and maxW
// default) collapses onto the same memo key, so a memoised frame at t=0
// serves a scrub to any position without ffmpeg.
func TestStillClampsStillSourceToZero(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, ffrun.Tools{}, Options{})
	b, err := st.PutBlob(bytes.NewReader([]byte("one frame prores")), "frame.mov")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p10le",
		Bits: 10, Width: 64, Height: 48, Frames: 1, IsStill: true, HasAlpha: true, Kind: recipe.KindImage, Premultiplied: true}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out := recipe.Output{Format: "gif"}
	// No ffmpeg and no memo yet: fails on the tool, proving the memo path is
	// consulted with the clamped key below.
	if _, err := m.Still(ctx, b.Hash, nil, out, 5, 0); err == nil || !strings.Contains(err.Error(), "ffmpeg") {
		t.Fatalf("no ffmpeg: %v", err)
	}
	dir := filepath.Join(st.Scratch, stillsDir)
	os.MkdirAll(dir, 0o755)
	key, _ := stillKey(b.Hash, nil, stillOutput(out), 0, DefaultStillWidth)
	os.WriteFile(filepath.Join(dir, key+".png"), []byte("FRAME0"), 0o644)
	for _, tt := range []float64{0, 0.5, 5, 123.4, -2} {
		got, err := m.Still(ctx, b.Hash, nil, out, tt, 0)
		if err != nil || string(got) != "FRAME0" {
			t.Errorf("t=%v: %q %v (want the t=0 memo)", tt, got, err)
		}
	}
	// A still counts as a use: the blob mtime is refreshed.
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(b.Path, old, old)
	if _, err := m.Still(ctx, b.Hash, nil, out, 1, 0); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(b.Path); fi.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Errorf("still did not touch the source blob: mtime %v", fi.ModTime())
	}
}

// TestJobTouchesSourceBlob: looking a source up for a render refreshes its
// mtime so the sweeper's TTL runs from the last use.
func TestJobTouchesSourceBlob(t *testing.T) {
	st := newTestStore(t)
	hash := putSource(t, st, true)
	b, _ := st.GetBlob(hash)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(b.Path, old, old)
	os.Chtimes(filepath.Join(filepath.Dir(b.Path), hash+".json"), old, old)

	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	j, err := m.Submit(recipe.Recipe{Sources: []string{hash}, Output: recipe.Output{Format: "gif"}})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "ffmpeg") {
		t.Fatalf("job should reach the fake ffmpeg: %+v", fin)
	}
	fi, err := os.Stat(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Errorf("source blob not touched by the render: mtime %v", fi.ModTime())
	}
	// And so a TTL sweep right after keeps it.
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(hash); err != nil {
		t.Errorf("used source swept: %v", err)
	}
	// lookupSources on a missing blob still reports not found (touch is not
	// an error path).
	if _, err := m.lookupSources([]string{strings.Repeat("f", 64)}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("missing source: %v", err)
	}
}

func TestStructuralErrorClassification(t *testing.T) {
	rep := discordlint.Report{Checks: []discordlint.Check{
		{Rule: "gif.size-limit", Level: discordlint.LevelError, OK: false},
		{Rule: "gif.sticker-dims", Level: discordlint.LevelError, OK: false},
		{Rule: "gif.first-frame-visible", Level: discordlint.LevelWarn, OK: false},
		{Rule: "gif.global-palette", Level: discordlint.LevelError, OK: true},
	}}
	if hasStructuralError(rep) {
		t.Error("limit/warn failures classified as structural")
	}
	rep.Checks = append(rep.Checks, discordlint.Check{Rule: "gif.global-palette", Level: discordlint.LevelError, OK: false})
	if !hasStructuralError(rep) {
		t.Error("local palette failure not structural")
	}
}

// putSource's info (64x48, 25 fps, 2 s = 50 frames) compiles to a
// 50*64*48*4 = 614400-byte master with the default output.
const testMasterBytes = 50 * 64 * 48 * 4

func TestMasterEstimate(t *testing.T) {
	if masterBytes(nil) != 0 {
		t.Error("nil plan")
	}
	p := &graph.Plan{Width: 64, Height: 48, Frames: 50}
	if got := masterBytes(p); got != testMasterBytes {
		t.Errorf("masterBytes = %d, want %d", got, testMasterBytes)
	}
	if got := masterBytes(&graph.Plan{Width: 64, Height: 48}); got != 0 {
		t.Errorf("unknown frames must give 0, got %d", got)
	}
	// 1080p30 x 20 s: the finding's default-path scenario exceeds 2 GiB.
	if got := masterBytes(&graph.Plan{Width: 1920, Height: 1080, Frames: 600}); got <= DefaultMaxMasterBytes {
		t.Errorf("600 frames of 1080p = %d must exceed the %d cap", got, DefaultMaxMasterBytes)
	}
	if scratchReserve(0) != 0 {
		t.Error("reserve of unknown must be 0")
	}
	if got := scratchReserve(testMasterBytes); got != testMasterBytes+scratchHeadroomMin {
		t.Errorf("small reserve = %d", got)
	}
	if got := scratchReserve(1 << 30); got != (1<<30)+(1<<30)/scratchHeadroomDiv {
		t.Errorf("large reserve = %d", got)
	}
	for n, want := range map[int64]string{0: "0 B", 614400: "600 KiB", 600000: "586 KiB", 2 << 30: "2 GiB", 4980736000: "4.6 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestByteBudget(t *testing.T) {
	b := newByteBudget(100)
	rel1, err := b.acquire(context.Background(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if b.Used() != 60 {
		t.Errorf("used = %d", b.Used())
	}
	if b.tryAcquire(50) {
		t.Fatal("tryAcquire over the limit succeeded")
	}
	if _, err := b.acquire(context.Background(), 101); err == nil {
		t.Error("acquire beyond the total must fail immediately")
	}
	// A second acquire blocks until the first releases.
	got := make(chan error, 1)
	go func() {
		rel, err := b.acquire(context.Background(), 60)
		if err == nil {
			rel()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("second acquire returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	rel1()
	rel1() // idempotent
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second acquire: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("second acquire never woke up")
	}
	if b.Used() != 0 {
		t.Errorf("used after releases = %d", b.Used())
	}
	// Cancellation while waiting.
	relHold, _ := b.acquire(context.Background(), 100)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := b.acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled acquire: %v", err)
	}
	relHold()
	// Unlimited budget never blocks.
	u := newByteBudget(0)
	if rel, err := u.acquire(context.Background(), 1<<40); err != nil {
		t.Errorf("unlimited: %v", err)
	} else {
		rel()
	}
	if rel, err := u.acquire(context.Background(), 0); err != nil {
		t.Errorf("zero: %v", err)
	} else {
		rel()
	}
}

func TestMasterCapRejectsBeforeFFmpeg(t *testing.T) {
	st := newTestStore(t)
	hash := putSource(t, st, true)
	r := recipe.Recipe{Sources: []string{hash}, Output: recipe.Output{Format: "gif"}}

	// Cap just below the estimate: refused with an actionable message,
	// no process spawned, no scratch dir created.
	m := NewManager(st, fakeTools, Options{Concurrency: 1, MaxMasterBytes: testMasterBytes - 1})
	if m.MaxMasterBytes() != testMasterBytes-1 {
		t.Errorf("MaxMasterBytes = %d", m.MaxMasterBytes())
	}
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	fin := waitFinished(t, m, j.ID)
	if fin.State != StateError {
		t.Fatalf("job = %+v", fin)
	}
	for _, want := range []string{"frame master would need 600 KiB", "50 frames of 64x48", "trim the clip", "EZLG_MAX_MASTER_BYTES", ErrInvalidRecipe.Error()} {
		if !strings.Contains(fin.Error, want) {
			t.Errorf("error %q lacks %q", fin.Error, want)
		}
	}
	if strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("ffmpeg was spawned: %q", fin.Error)
	}
	if _, err := os.Stat(filepath.Join(st.Scratch, j.ID)); !os.IsNotExist(err) {
		t.Errorf("scratch dir created for a refused render: %v", err)
	}
	if m.scratch.Used() != 0 {
		t.Errorf("budget leaked: %d", m.scratch.Used())
	}

	// Cap exactly at the estimate: admitted (then fails on the fake ffmpeg).
	m = NewManager(st, fakeTools, Options{Concurrency: 1, MaxMasterBytes: testMasterBytes})
	j, _ = m.Submit(r)
	fin = waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("job at the cap should reach ffmpeg: %+v", fin)
	}
	if m.scratch.Used() != 0 {
		t.Errorf("budget not released after failure: %d", m.scratch.Used())
	}

	// A scratch filesystem smaller than the reservation is refused up-front.
	m = NewManager(st, fakeTools, Options{Concurrency: 1, ScratchBudgetBytes: testMasterBytes})
	if m.ScratchBudgetBytes() != testMasterBytes {
		t.Errorf("ScratchBudgetBytes = %d", m.ScratchBudgetBytes())
	}
	j, _ = m.Submit(r)
	fin = waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "scratch filesystem") || !strings.Contains(fin.Error, "shm_size") || strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("small scratch: %+v", fin)
	}
	if !errors.Is(fmt.Errorf("%w: x", ErrInvalidRecipe), ErrInvalidRecipe) || !strings.Contains(fin.Error, ErrInvalidRecipe.Error()) {
		t.Errorf("small scratch must be a client error: %q", fin.Error)
	}

	// Unknown length (no duration/frames): admitted without a reservation.
	b, _ := st.PutBlob(bytes.NewReader([]byte("unknown length clip")), "x.webm")
	st.SetBlobInfo(b.Hash, recipe.ProbeInfo{Format: "matroska,webm", Codec: "vp9", PixFmt: "yuv420p", Bits: 8, Width: 4000, Height: 4000, FPS: 30, Kind: recipe.KindVideo})
	m = NewManager(st, fakeTools, Options{Concurrency: 1, MaxMasterBytes: 1})
	j, _ = m.Submit(recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "gif"}})
	fin = waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("unknown-length job should reach ffmpeg: %+v", fin)
	}
}

func TestScratchAdmissionWaits(t *testing.T) {
	st := newTestStore(t)
	hash := putSource(t, st, true)
	r := recipe.Recipe{Sources: []string{hash}, Output: recipe.Output{Format: "gif"}}
	reserve := scratchReserve(testMasterBytes)
	// Room for exactly one render; occupy it by hand so the job must wait.
	m := NewManager(st, fakeTools, Options{Concurrency: 2, ScratchBudgetBytes: reserve + reserve/2})
	if !m.scratch.tryAcquire(reserve) {
		t.Fatal("manual reservation failed")
	}
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	ch, unsub, _ := m.Subscribe(j.ID)
	defer unsub()
	deadline := time.After(testTimeout)
	waiting := false
	for !waiting {
		select {
		case ev := <-ch:
			if strings.Contains(ev.Job.Message, "waiting for scratch space") {
				waiting = true
			}
			if ev.Job.IsFinished() {
				t.Fatalf("job finished without waiting: %+v", ev.Job)
			}
		case <-deadline:
			t.Fatal("job never reported waiting for scratch space")
		}
	}
	time.Sleep(30 * time.Millisecond)
	if got, _ := m.Get(j.ID); got.IsFinished() {
		t.Fatalf("job ran although the budget was full: %+v", got)
	}
	// Cancelling a waiting job releases it.
	j2, _ := m.Submit(r)
	time.Sleep(30 * time.Millisecond)
	if !m.Cancel(j2.ID) {
		t.Fatal("cancel of waiting job")
	}
	if fin := waitFinished(t, m, j2.ID); fin.Error != "cancelled" {
		t.Errorf("cancelled waiter: %+v", fin)
	}
	// Releasing the manual reservation lets the first job proceed (to the
	// fake ffmpeg), after which the budget is fully released again.
	m.scratch.release(reserve)
	fin := waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("job after wait: %+v", fin)
	}
	if m.scratch.Used() != 0 {
		t.Errorf("budget leaked: %d", m.scratch.Used())
	}
}

func TestNoSpaceMapping(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, fakeTools, Options{})
	plain := errors.New("gif encode: ffmpeg exited: exit status 1")
	if isNoSpace(plain) || m.describeNoSpace(plain) != plain {
		t.Error("ordinary errors must pass through unchanged")
	}
	if isNoSpace(nil) || m.describeNoSpace(nil) != nil {
		t.Error("nil")
	}
	tail := errors.New("master render: ffmpeg exited: exit status 228\n[out#0/rawvideo @ 0x1] Error writing trailer: No space left on device\n[out#0/rawvideo @ 0x1] Error closing file: No space left on device")
	if !isNoSpace(tail) {
		t.Error("stderr tail with ENOSPC not detected")
	}
	got := m.describeNoSpace(tail)
	if !strings.Contains(got.Error(), "scratch "+st.Scratch+" is full") || !strings.Contains(got.Error(), "shm_size") || !errors.Is(got, tail) {
		t.Errorf("described = %v", got)
	}
	perr := &os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}
	if !isNoSpace(fmt.Errorf("write linted output: %w", perr)) {
		t.Error("wrapped syscall.ENOSPC not detected")
	}
}

func TestStillKeyAndEvict(t *testing.T) {
	src := strings.Repeat("c", 64)
	ops := []recipe.Op{{Kind: recipe.OpCrop, Params: json.RawMessage(`{"x":1,"y":2,"w":3,"h":4}`)}}
	out := recipe.Output{Format: "gif", Width: 128}
	k1, err := stillKey(src, ops, out, 1.5, 480)
	if err != nil {
		t.Fatal(err)
	}
	k1b, _ := stillKey(src, []recipe.Op{{Kind: recipe.OpCrop, Params: json.RawMessage(`{ "h":4, "w":3, "y":2, "x":1 }`)}}, out, 1.5, 480)
	if k1 != k1b {
		t.Error("key depends on params formatting")
	}
	if k2, _ := stillKey(src, ops, out, 1.6, 480); k2 == k1 {
		t.Error("key ignores t")
	}
	if k3, _ := stillKey(src, ops, out, 1.5, 320); k3 == k1 {
		t.Error("key ignores maxW")
	}
	if k4, _ := stillKey(src, ops, recipe.Output{Format: "gif", Width: 64}, 1.5, 480); k4 == k1 {
		t.Error("key ignores output geometry")
	}
	if _, err := stillKey(src, []recipe.Op{{Kind: "crop", Params: json.RawMessage(`{bad`)}}, out, 0, 0); err == nil {
		t.Error("bad params accepted")
	}
	// Quality knobs are not part of the still output.
	full := recipe.Output{Format: "gif", Width: 128, Lossy: 80, Colors: 64, Quality: 50, Target: "emote"}
	if stillOutput(full) != out {
		t.Errorf("stillOutput = %+v", stillOutput(full))
	}

	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, strings.Repeat("k", 3)+string(rune('a'+i))+".png")
		os.WriteFile(p, []byte{1}, 0o644)
		mt := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, mt, mt)
	}
	os.WriteFile(filepath.Join(dir, ".still-tmp"), []byte{1}, 0o644)
	if err := evictOldest(dir, 3); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != ".still-tmp,kkkf.png,kkkg.png,kkkh.png" {
		t.Errorf("after evict: %v", names)
	}
}

func TestStillErrors(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, ffrun.Tools{}, Options{})
	ctx := context.Background()
	if _, err := m.Still(ctx, "nope", nil, recipe.Output{}, 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bad hash: %v", err)
	}
	if _, err := m.Still(ctx, strings.Repeat("d", 64), nil, recipe.Output{}, 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown hash: %v", err)
	}
	noInfo := putSource(t, st, false)
	if _, err := m.Still(ctx, noInfo, nil, recipe.Output{}, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("no info: %v", err)
	}
	// With info but no ffmpeg: compile errors are client errors, and a
	// compilable request fails on the missing tool.
	withInfo := putSource(t, st, true)
	_, err := m.Still(ctx, withInfo, []recipe.Op{{Kind: "nonsense-op"}}, recipe.Output{Format: "gif"}, 0, 0)
	if err == nil {
		t.Fatal("unknown op accepted")
	}
	if !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unknown op: %v (want ErrInvalidRecipe)", err)
	}
	_, err = m.Still(ctx, withInfo, nil, recipe.Output{Format: "gif"}, 0.5, 0)
	if err == nil || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("no ffmpeg: %v", err)
	}
	// A memoised file short-circuits everything (no ffmpeg needed).
	dir := filepath.Join(st.Scratch, stillsDir)
	os.MkdirAll(dir, 0o755)
	key, _ := stillKey(withInfo, nil, stillOutput(recipe.Output{Format: "gif"}), 0.5, DefaultStillWidth)
	os.WriteFile(filepath.Join(dir, key+".png"), []byte("PNGDATA"), 0o644)
	got, err := m.Still(ctx, withInfo, nil, recipe.Output{Format: "gif"}, 0.5, 0)
	if err != nil || string(got) != "PNGDATA" {
		t.Errorf("memo hit: %q %v", got, err)
	}
}
