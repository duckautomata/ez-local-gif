package fit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake encoder -------------------------------------------------------

// call records one Encode invocation on a rung.
type call struct{ knob, attempt int }

// fake is a deterministic, process-free stand-in for the jobs encoder. Rungs
// are identified by their Label ("r0", "r1", …) so a test ladder's index is
// recoverable inside the encoder. The size model is size(idx, knob); delay,
// fail and block are optional hooks.
type fake struct {
	size  func(idx, knob int) int64
	delay func(idx int) time.Duration
	fail  func(idx, knob, attempt int) error
	block func(idx int) bool    // block until ctx is done (honours cancellation)
	gate  map[int]chan struct{} // rungs listed here wait until their channel is closed (or ctx ends)

	mu    sync.Mutex
	calls map[int][]call
	index map[string]int
}

func newFake(ladder []Rung, size func(idx, knob int) int64) *fake {
	f := &fake{size: size, calls: map[int][]call{}, index: map[string]int{}}
	for i, r := range ladder {
		f.index[r.Label] = i
	}
	return f
}

func (f *fake) encode(ctx context.Context, r Rung, knob, attempt int) (string, int64, error) {
	idx, ok := f.index[r.Label]
	if !ok {
		return "", 0, fmt.Errorf("unknown rung %q", r.Label)
	}
	f.mu.Lock()
	f.calls[idx] = append(f.calls[idx], call{knob, attempt})
	f.mu.Unlock()
	if f.block != nil && f.block(idx) {
		<-ctx.Done()
		return "", 0, ctx.Err()
	}
	if ch, ok := f.gate[idx]; ok {
		select {
		case <-ch:
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
	}
	if f.delay != nil {
		if err := sleepCtx(ctx, f.delay(idx)); err != nil {
			return "", 0, err
		}
	}
	if f.fail != nil {
		if err := f.fail(idx, knob, attempt); err != nil {
			return "", 0, err
		}
	}
	return fmt.Sprintf("scratch/r%d-a%d", idx, attempt), f.size(idx, knob), nil
}

func (f *fake) count(idx int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls[idx])
}

func (f *fake) knobs(idx int) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, 0, len(f.calls[idx]))
	for _, c := range f.calls[idx] {
		out = append(out, c.knob)
	}
	return out
}

func (f *fake) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += len(c)
	}
	return n
}

// waitCalls blocks until rung idx has been encoded at least n times (safe
// from helper goroutines: it reports with Errorf and gives up after 5 s).
func (f *fake) waitCalls(t *testing.T, idx, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.count(idx) < n {
		if time.Now().After(deadline) {
			t.Errorf("rung %d never reached %d encodes (has %d)", idx, n, f.count(idx))
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// testLadder returns n rungs labelled r0..r(n-1).
func testLadder(n int) []Rung {
	out := make([]Rung, n)
	for i := range out {
		out[i] = Rung{FPS: 25 - float64(i), Colors: 256 >> i, Format: "gif", Label: fmt.Sprintf("r%d", i)}
	}
	return out
}

// expModel is the deterministic size model size = base(rung) * exp(-k*knob).
func expModel(bases []float64, k float64) func(idx, knob int) int64 {
	return func(idx, knob int) int64 {
		return int64(math.Round(bases[idx] * math.Exp(-k*float64(knob))))
	}
}

// gifKnob is KnobFor("gif"): 0..200, mild 30, harsh 150.
var gifKnob = Knob{Min: 0, Max: 200, Mild: 30, Harsh: 150, Name: KnobLossy}

// limits returns (limit, lower) exactly as Search computes them.
func limits(target int64, margin float64) (int64, int64) {
	if margin == 0 {
		margin = DefaultMargin
	}
	limit := int64(math.Floor(float64(target) * (1 - margin)))
	lower := int64(math.Ceil(float64(target) * (1 - margin - WindowWidth)))
	return limit, max(0, min(lower, limit))
}

func mustSearch(t *testing.T, req Request, f *fake) Result {
	t.Helper()
	res, err := Search(context.Background(), req, f.encode)
	if err != nil {
		t.Fatalf("Search: %v (result %+v)", err, res)
	}
	if res.Best == nil {
		t.Fatalf("Search returned nil Best without error")
	}
	return res
}

func labelsOf(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Rung.Label
	}
	return out
}

func assertUnder(t *testing.T, res Result, limit int64) {
	t.Helper()
	if res.Best != nil && res.Best.Bytes > limit {
		t.Errorf("Best %d bytes over limit %d", res.Best.Bytes, limit)
	}
	for _, a := range res.Alternatives {
		if a.Bytes > limit {
			t.Errorf("alternative %s %d bytes over limit %d", a.Rung.Label, a.Bytes, limit)
		}
	}
}

// ---- probes / early exit / skip ----------------------------------------

func TestSearch_MildFitEarlyExit(t *testing.T) {
	ladder := testLadder(5)
	// rung0 mild (lossy 30): 120000*e^-0.3 = 88898 <= limit 98000 → one probe.
	f := newFake(ladder, expModel([]float64{120000, 80000, 60000, 40000, 20000}, 0.01))
	req := Request{Target: 100000, Ladder: ladder, Knob: gifKnob}
	res := mustSearch(t, req, f)

	if res.Best.Rung.Label != "r0" || res.Best.Knob != 30 || res.Best.Bytes != 88898 {
		t.Fatalf("Best = %+v, want r0 @ lossy 30 / 88898 B", res.Best)
	}
	if got := res.Best.Desc; got != "fit at r0 · lossy 30" {
		t.Errorf("Desc = %q", got)
	}
	if res.Best.Path != "scratch/r0-a1" || res.Best.Format != "gif" {
		t.Errorf("Path/Format = %q/%q", res.Best.Path, res.Best.Format)
	}
	if f.count(0) != 1 {
		t.Errorf("rung0 encoded %d times, want 1 (early exit on mild fit)", f.count(0))
	}
	// Default Alternatives = 2 → rungs 1 and 2 run (mild fits), 3 and 4 never start.
	if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r1,r2" {
		t.Errorf("Alternatives = %v, want [r1 r2]", got)
	}
	if f.count(3) != 0 || f.count(4) != 0 {
		t.Errorf("rungs beyond winner+Alternatives were encoded: r3=%d r4=%d", f.count(3), f.count(4))
	}
	if res.Tried != 3 || res.Tried != f.total() {
		t.Errorf("Tried = %d (fake saw %d), want 3", res.Tried, f.total())
	}
	if len(res.Errors) != 0 || len(res.Skipped) != 0 {
		t.Errorf("Errors/Skipped = %v / %v, want none", res.Errors, res.Skipped)
	}
}

func TestSearch_HarshMissSkipsRung(t *testing.T) {
	ladder := testLadder(4)
	// rung0 harsh (lossy 150): 10e6*e^-1.5 = 2.23e6 > limit → skipped after 2 probes.
	f := newFake(ladder, expModel([]float64{10e6, 120000, 80000, 60000}, 0.01))
	req := Request{Target: 100000, Ladder: ladder, Knob: gifKnob}
	res := mustSearch(t, req, f)

	if res.Best.Rung.Label != "r1" {
		t.Fatalf("Best = %s, want r1", res.Best.Rung.Label)
	}
	if got := f.knobs(0); len(got) != 2 || got[0] != 30 || got[1] != 150 {
		t.Errorf("rung0 probes = %v, want [30 150]", got)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "r0" {
		t.Errorf("Skipped = %v, want [r0]", res.Skipped)
	}
	if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r2,r3" {
		t.Errorf("Alternatives = %v, want [r2 r3]", got)
	}
	if res.Tried != 5 {
		t.Errorf("Tried = %d, want 5 (2 + 1 + 1 + 1)", res.Tried)
	}
}

func TestSearch_SinglePointKnobSkipsWithoutSecondProbe(t *testing.T) {
	ladder := testLadder(2)
	f := newFake(ladder, expModel([]float64{10e6, 50000}, 0.01))
	req := Request{Target: 100000, Ladder: ladder, Knob: Knob{Min: 7, Max: 7, Name: "fixed"}}
	res := mustSearch(t, req, f)
	if f.count(0) != 1 || len(res.Skipped) != 1 {
		t.Errorf("rung0 calls=%d skipped=%v, want 1 call and a skip", f.count(0), res.Skipped)
	}
	if res.Best.Rung.Label != "r1" || res.Best.Knob != 7 {
		t.Errorf("Best = %+v", res.Best)
	}
}

// ---- secant --------------------------------------------------------------

func TestSearch_SecantConvergesOnLogLinearModel(t *testing.T) {
	ladder := testLadder(1)
	// base 200000: mild 148164 (over), harsh 44626 (under the window) → secant.
	// log-linear → first step hits the aim exactly: knob = ln(200000/97000)/0.01 = 72.4 → 72.
	f := newFake(ladder, expModel([]float64{200000}, 0.01))
	req := Request{Target: 100000, Ladder: ladder, Knob: gifKnob}
	res := mustSearch(t, req, f)

	limit, lower := limits(100000, 0)
	if res.Best.Bytes > limit || res.Best.Bytes < lower {
		t.Fatalf("Best %d bytes outside window [%d,%d]", res.Best.Bytes, lower, limit)
	}
	if got := f.knobs(0); len(got) != 3 || got[2] != 72 {
		t.Errorf("probes = %v, want [30 150 72]", got)
	}
	if res.Best.Knob != 72 || res.Tried != 3 {
		t.Errorf("Best.Knob = %d, Tried = %d; want 72, 3", res.Best.Knob, res.Tried)
	}
	if res.Best.Desc != "fit at r0 · lossy 72" {
		t.Errorf("Desc = %q", res.Best.Desc)
	}
}

func TestSearch_SecantWithinMaxIterOnCurvedModel(t *testing.T) {
	ladder := testLadder(1)
	// Convex in log space: size = base / (1 + knob)^1.6 (mild 300 KB over,
	// harsh 24 KB under; only knob 62 lands in the 2 % window) — regula falsi
	// needs several steps; Illinois keeps it from crawling.
	model := func(_, knob int) int64 {
		return int64(math.Round(7.3e7 / math.Pow(1+float64(knob), 1.6)))
	}
	limit, lower := limits(100000, 0)
	var prevBest int64
	for _, maxIter := range []int{1, 2, 3, 4, 5, 8} {
		f := newFake(ladder, model)
		req := Request{Target: 100000, Ladder: ladder, Knob: gifKnob, MaxIter: maxIter}
		res := mustSearch(t, req, f)
		if res.Best.Bytes > limit {
			t.Errorf("MaxIter %d: Best %d over limit %d", maxIter, res.Best.Bytes, limit)
		}
		if n := f.count(0); n > 2+maxIter {
			t.Errorf("MaxIter %d: %d encodes, want <= %d", maxIter, n, 2+maxIter)
		}
		if harsh := model(0, 150); res.Best.Bytes < harsh {
			t.Errorf("MaxIter %d: Best %d smaller than the harsh probe %d", maxIter, res.Best.Bytes, harsh)
		}
		// More iterations never make the answer worse (same probe prefix).
		if res.Best.Bytes < prevBest {
			t.Errorf("MaxIter %d: Best %d worse than with fewer iterations (%d)", maxIter, res.Best.Bytes, prevBest)
		}
		prevBest = res.Best.Bytes
		if maxIter >= 5 && res.Best.Bytes < lower {
			t.Errorf("MaxIter %d: Best %d below window floor %d (knobs %v)", maxIter, res.Best.Bytes, lower, f.knobs(0))
		}
		// Every probe after the first two stays strictly inside (mild, harsh).
		for _, k := range f.knobs(0)[min(2, f.count(0)):] {
			if k <= 30 || k >= 150 {
				t.Errorf("MaxIter %d: secant probe %d left the (30,150) bracket", maxIter, k)
			}
		}
		t.Logf("MaxIter %d: probes %v → best %d B @ lossy %d", maxIter, f.knobs(0), res.Best.Bytes, res.Best.Knob)
	}
}

func TestSearch_SecantStopsWhenBracketHasNoInteriorInteger(t *testing.T) {
	ladder := testLadder(1)
	// APNG-style knob 0..3 (mild 0, harsh 2): only knob 1 can be probed.
	model := func(_, knob int) int64 { return []int64{300000, 200000, 50000, 10000}[knob] }
	f := newFake(ladder, model)
	req := Request{Target: 100000, Ladder: ladder, Knob: Knob{Min: 0, Max: 3, Harsh: 2, Name: KnobColourStep}, MaxIter: 10}
	res := mustSearch(t, req, f)
	if got := f.knobs(0); len(got) != 3 || got[2] != 1 {
		t.Errorf("probes = %v, want [0 2 1]", got)
	}
	if res.Best.Knob != 2 || res.Best.Bytes != 50000 {
		t.Errorf("Best = %+v, want knob 2 / 50000 B", res.Best)
	}
	if res.Best.Desc != "fit at r0 · colour step 2" {
		t.Errorf("Desc = %q", res.Best.Desc)
	}
}

func TestSearch_KeepsBestUnderLimitOnNonMonotoneModel(t *testing.T) {
	ladder := testLadder(1)
	// Bumpy: a fitting probe that is smaller than an earlier fitting one must
	// not replace it as Best (largest size under the limit wins).
	model := func(_, knob int) int64 {
		base := 200000 * math.Exp(-0.01*float64(knob))
		return int64(base * (1 + 0.15*math.Sin(float64(knob)/7)))
	}
	f := newFake(ladder, model)
	req := Request{Target: 100000, Ladder: ladder, Knob: gifKnob, MaxIter: 6}
	res := mustSearch(t, req, f)
	limit, _ := limits(100000, 0)
	var bestSeen int64
	for _, k := range f.knobs(0) {
		if s := model(0, k); s <= limit && s > bestSeen {
			bestSeen = s
		}
	}
	if res.Best.Bytes != bestSeen {
		t.Errorf("Best = %d, want the largest fitting probe %d (knobs %v)", res.Best.Bytes, bestSeen, f.knobs(0))
	}
}

func TestSearch_MaxIterNegativeMeansNoSecant(t *testing.T) {
	ladder := testLadder(1)
	f := newFake(ladder, expModel([]float64{200000}, 0.01))
	req := Request{Target: 100000, Ladder: ladder, Knob: gifKnob, MaxIter: -1}
	res := mustSearch(t, req, f)
	if f.count(0) != 2 || res.Best.Knob != 150 {
		t.Errorf("calls=%d Best.Knob=%d, want 2 probes and the harsh knob", f.count(0), res.Best.Knob)
	}
}

// ---- margin window -------------------------------------------------------

func TestSearch_MarginWindow(t *testing.T) {
	ladder := testLadder(1)
	for _, margin := range []float64{0, 0.01, 0.05, 0.1, 0.25} {
		f := newFake(ladder, expModel([]float64{300000}, 0.01))
		req := Request{Target: 100000, Margin: margin, Ladder: ladder, Knob: gifKnob}
		res := mustSearch(t, req, f)
		limit, lower := limits(100000, margin)
		if res.Best.Bytes > limit {
			t.Errorf("margin %g: Best %d over limit %d", margin, res.Best.Bytes, limit)
		}
		if res.Best.Bytes < lower {
			t.Errorf("margin %g: Best %d below window floor %d (secant should land in window on a log-linear model)", margin, res.Best.Bytes, lower)
		}
		// A fitting probe in the window stops the rung: only one secant step.
		if n := f.count(0); n != 3 {
			t.Errorf("margin %g: %d encodes, want 3", margin, n)
		}
	}
}

func TestSearch_HarshInsideWindowStopsImmediately(t *testing.T) {
	ladder := testLadder(1)
	limit, lower := limits(100000, 0)
	model := func(_, knob int) int64 {
		if knob == 30 {
			return 500000
		}
		return (limit + lower) / 2 // harsh lands in the window
	}
	f := newFake(ladder, model)
	res := mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f)
	if f.count(0) != 2 || res.Best.Knob != 150 {
		t.Errorf("calls=%d knob=%d, want 2 / 150", f.count(0), res.Best.Knob)
	}
}

// ---- no fit / errors -----------------------------------------------------

func TestSearch_ErrNoFit(t *testing.T) {
	ladder := testLadder(3)
	f := newFake(ladder, expModel([]float64{10e6, 9e6, 8e6}, 0.01))
	res, err := Search(context.Background(), Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f.encode)
	if !errors.Is(err, ErrNoFit) {
		t.Fatalf("err = %v, want ErrNoFit", err)
	}
	if res.Best != nil || len(res.Alternatives) != 0 {
		t.Errorf("Best/Alternatives populated on no-fit: %+v", res)
	}
	if strings.Join(res.Skipped, ",") != "r0,r1,r2" {
		t.Errorf("Skipped = %v, want all rungs in order", res.Skipped)
	}
	if res.Tried != 6 {
		t.Errorf("Tried = %d, want 6", res.Tried)
	}
}

func TestSearch_EncoderErrorAbortsOnlyThatRung(t *testing.T) {
	ladder := testLadder(3)
	f := newFake(ladder, expModel([]float64{120000, 110000, 100000}, 0.01))
	f.fail = func(idx, _, _ int) error {
		if idx == 0 {
			return errors.New("gifsicle exited 1")
		}
		return nil
	}
	res := mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f)
	if res.Best.Rung.Label != "r1" {
		t.Errorf("Best = %s, want r1", res.Best.Rung.Label)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "r0") || !strings.Contains(res.Errors[0], "gifsicle exited 1") {
		t.Errorf("Errors = %v", res.Errors)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("an erroring rung must not be reported as skipped: %v", res.Skipped)
	}
	if f.count(0) != 1 {
		t.Errorf("rung0 retried after error: %d calls", f.count(0))
	}
}

func TestSearch_ErrorDuringSecantKeepsBestSoFar(t *testing.T) {
	ladder := testLadder(1)
	f := newFake(ladder, expModel([]float64{200000}, 0.01))
	f.fail = func(_, _, attempt int) error {
		if attempt == 3 {
			return errors.New("disk full")
		}
		return nil
	}
	res := mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f)
	if res.Best.Knob != 150 {
		t.Errorf("Best.Knob = %d, want the harsh probe 150", res.Best.Knob)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "disk full") {
		t.Errorf("Errors = %v", res.Errors)
	}
	if f.count(0) != 3 {
		t.Errorf("calls = %d, want 3 (no retry after the error)", f.count(0))
	}
}

func TestSearch_EmptyOutputIsAnError(t *testing.T) {
	ladder := testLadder(1)
	f := newFake(ladder, func(int, int) int64 { return 0 })
	_, err := Search(context.Background(), Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f.encode)
	if !errors.Is(err, ErrNoFit) {
		t.Fatalf("err = %v, want ErrNoFit", err)
	}
}

func TestSearch_AllRungsFailReturnsErrNoFitWithErrors(t *testing.T) {
	ladder := testLadder(2)
	f := newFake(ladder, expModel([]float64{1000, 1000}, 0.01))
	f.fail = func(int, int, int) error { return errors.New("boom") }
	res, err := Search(context.Background(), Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f.encode)
	if !errors.Is(err, ErrNoFit) || len(res.Errors) != 2 || res.Tried != 2 {
		t.Errorf("err=%v errors=%v tried=%d", err, res.Errors, res.Tried)
	}
}

// ---- validation ----------------------------------------------------------

func TestSearch_Validation(t *testing.T) {
	ladder := testLadder(1)
	f := newFake(ladder, expModel([]float64{1000}, 0.01))
	cases := []struct {
		name string
		req  Request
		enc  EncodeFunc
	}{
		{"nil encode", Request{Target: 1000, Ladder: ladder, Knob: gifKnob}, nil},
		{"empty ladder", Request{Target: 1000, Knob: gifKnob}, f.encode},
		{"zero target", Request{Ladder: ladder, Knob: gifKnob}, f.encode},
		{"negative target", Request{Target: -5, Ladder: ladder, Knob: gifKnob}, f.encode},
		{"margin >= 1", Request{Target: 1000, Margin: 1, Ladder: ladder, Knob: gifKnob}, f.encode},
		{"negative margin", Request{Target: 1000, Margin: -0.1, Ladder: ladder, Knob: gifKnob}, f.encode},
		{"knob min > max", Request{Target: 1000, Ladder: ladder, Knob: Knob{Min: 10, Max: 5}}, f.encode},
		{"zero knob", Request{Target: 1000, Ladder: ladder}, f.encode},
		{"zero knob with unmatched override", Request{Target: 1000, Ladder: ladder, Knobs: map[string]Knob{"webp": gifKnob}}, f.encode},
		{"knob harsh < mild", Request{Target: 1000, Ladder: ladder, Knob: Knob{Min: 0, Max: 100, Mild: 80, Harsh: 20}}, f.encode},
		{"bad override", Request{Target: 1000, Ladder: ladder, Knob: gifKnob, Knobs: map[string]Knob{"gif": {Min: 3, Max: 1}}}, f.encode},
		{"tiny target", Request{Target: 1, Margin: 0.5, Ladder: ladder, Knob: gifKnob}, f.encode},
	}
	for _, c := range cases {
		_, err := Search(context.Background(), c.req, c.enc)
		if err == nil || errors.Is(err, ErrNoFit) {
			t.Errorf("%s: err = %v, want a validation error", c.name, err)
		}
	}
	if f.total() != 0 {
		t.Errorf("validation failures must not encode anything (%d calls)", f.total())
	}
}

// ---- scheduling / parallelism ---------------------------------------------

func TestSearch_DoesNotStartBeyondWinnerPlusAlternatives(t *testing.T) {
	ladder := testLadder(8)
	bases := []float64{10e6, 300000, 200000, 150000, 120000, 90000, 70000, 50000}
	for _, alts := range []int{1, 2, 3} {
		f := newFake(ladder, expModel(bases, 0.01))
		res := mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Alternatives: alts}, f)
		if res.Best.Rung.Label != "r1" {
			t.Fatalf("alts %d: Best = %s, want r1", alts, res.Best.Rung.Label)
		}
		if len(res.Alternatives) != alts {
			t.Errorf("alts %d: got %d alternatives %v", alts, len(res.Alternatives), labelsOf(res.Alternatives))
		}
		for i := 2 + alts; i < len(ladder); i++ {
			if f.count(i) != 0 {
				t.Errorf("alts %d: rung %d was encoded (%d calls) beyond the window", alts, i, f.count(i))
			}
		}
		// rung0 skipped (2); rungs 1..3 take 3 each (mild, harsh, one secant
		// step); rung4 fits at mild (1).
		want := map[int]int{1: 8, 2: 11, 3: 12}[alts]
		if res.Tried != want {
			t.Errorf("alts %d: Tried = %d, want %d (knobs: %v %v %v)", alts, res.Tried, want, f.knobs(1), f.knobs(2), f.knobs(3))
		}
	}
}

func TestSearch_AlternativesOrderAndCount(t *testing.T) {
	ladder := testLadder(6)
	// rung0 fits; rung1 skipped (huge); rungs 2.. fit → only r2 is an alternative
	// (the window counts rungs, not fits).
	f := newFake(ladder, expModel([]float64{120000, 10e6, 80000, 70000, 60000, 50000}, 0.01))
	res := mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f)
	if res.Best.Rung.Label != "r0" {
		t.Fatalf("Best = %s", res.Best.Rung.Label)
	}
	if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r2" {
		t.Errorf("Alternatives = %v, want [r2]", got)
	}
	if strings.Join(res.Skipped, ",") != "r1" {
		t.Errorf("Skipped = %v", res.Skipped)
	}
	if f.count(3) != 0 {
		t.Errorf("r3 encoded outside the window")
	}
}

func TestSearch_ParallelMilderRungWinsLate(t *testing.T) {
	ladder := testLadder(6)
	f := newFake(ladder, expModel([]float64{120000, 110000, 100000, 90000, 80000, 70000}, 0.01))
	// Gates make the completion order deterministic: r1 first, then r2 and
	// r3, and the mildest rung r0 last.
	f.gate = map[int]chan struct{}{}
	for i := range 4 {
		f.gate[i] = make(chan struct{})
	}
	done := make(chan Result, 1)
	go func() {
		done <- mustSearchBG(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: 3}, f)
	}()
	f.waitCalls(t, 2, 1) // r0, r1, r2 started (Parallel 3)
	close(f.gate[1])     // r1 fits → window [1..3] → r3 starts
	f.waitCalls(t, 3, 1)
	close(f.gate[2])
	close(f.gate[3])
	time.Sleep(20 * time.Millisecond) // let r2/r3 complete; nothing else may start
	if f.count(4) != 0 || f.count(5) != 0 {
		t.Errorf("rungs beyond the window were encoded: r4=%d r5=%d", f.count(4), f.count(5))
	}
	close(f.gate[0]) // r0 fits last and still wins

	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Search hung")
	}
	if res.Best == nil || res.Best.Rung.Label != "r0" {
		t.Fatalf("Best = %+v, want the mildest rung even though it finished last", res.Best)
	}
	if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r1,r2" {
		t.Errorf("Alternatives = %v, want [r1 r2] (r3 completed but fell outside the final window)", got)
	}
	if f.count(4) != 0 || f.count(5) != 0 {
		t.Errorf("rungs beyond any window were encoded: r4=%d r5=%d", f.count(4), f.count(5))
	}
	if res.Tried != 4 {
		t.Errorf("Tried = %d, want 4", res.Tried)
	}
}

func TestSearch_CancelsRungsOutsideWindow(t *testing.T) {
	ladder := testLadder(4)
	f := newFake(ladder, expModel([]float64{120000, 110000, 100000, 90000}, 0.01))
	f.delay = func(idx int) time.Duration {
		if idx == 0 {
			return 0
		}
		return 20 * time.Millisecond
	}
	f.block = func(idx int) bool { return idx == 3 } // r3 only returns when its ctx is cancelled
	done := make(chan Result, 1)
	go func() {
		done <- mustSearchBG(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: 4}, f)
	}()
	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Search hung: the rung outside the window was not cancelled")
	}
	if res.Best == nil || res.Best.Rung.Label != "r0" {
		t.Fatalf("Best = %+v", res.Best)
	}
	if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r1,r2" {
		t.Errorf("Alternatives = %v", got)
	}
	if f.count(3) != 1 {
		t.Errorf("r3 calls = %d, want 1 (started in the first burst, then cancelled)", f.count(3))
	}
	if len(res.Errors) != 0 {
		t.Errorf("a scheduler-cancelled rung must not be reported as an error: %v", res.Errors)
	}
	if res.Tried != 4 {
		t.Errorf("Tried = %d, want 4 (cancelled encodes still count)", res.Tried)
	}
}

// mustSearchBG is mustSearch for goroutines (t.Fatal is not allowed there).
func mustSearchBG(t *testing.T, req Request, f *fake) Result {
	res, err := Search(context.Background(), req, f.encode)
	if err != nil {
		t.Errorf("Search: %v", err)
	}
	return res
}

// TestSearch_AlternativesWindowIgnoresEarlyHarshFinishers pins Result
// determinism under an adversarial completion order: every harsh rung
// finishes before the winner, so its outcome was recorded under the earlier,
// wider window — yet rungs outside the final winner+Alternatives window must
// contribute no candidate, error or skip. Alternatives must be exactly [r1].
func TestSearch_AlternativesWindowIgnoresEarlyHarshFinishers(t *testing.T) {
	ladder := testLadder(6)
	// r0 wins (gated so it finishes last); final window = [r1, r2].
	// r1 fits (inside), r2 skips (inside), r3 errors (outside), r4 skips
	// (outside), r5 fits (outside — the stale candidate the old code leaked).
	f := newFake(ladder, expModel([]float64{120000, 110000, 10e6, 100000, 10e6, 90000}, 0.01))
	f.fail = func(idx, _, _ int) error {
		if idx == 3 {
			return errors.New("boom")
		}
		return nil
	}
	f.gate = map[int]chan struct{}{0: make(chan struct{})}
	done := make(chan Result, 1)
	go func() {
		done <- mustSearchBG(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: 6}, f)
	}()
	// All harsh rungs have encoded before the winner is released; their
	// encodes are ctx-insensitive, so their outcomes are already decided.
	f.waitCalls(t, 1, 1)
	f.waitCalls(t, 2, 2)
	f.waitCalls(t, 3, 1)
	f.waitCalls(t, 4, 2)
	f.waitCalls(t, 5, 1)
	close(f.gate[0])

	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Search hung")
	}
	if res.Best == nil || res.Best.Rung.Label != "r0" {
		t.Fatalf("Best = %+v, want r0", res.Best)
	}
	if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r1" {
		t.Errorf("Alternatives = %v, want [r1] (r5 completed first but lies outside the final window)", got)
	}
	if strings.Join(res.Skipped, ",") != "r2" {
		t.Errorf("Skipped = %v, want [r2] (r4's skip lies outside the final window)", res.Skipped)
	}
	if len(res.Errors) != 0 {
		t.Errorf("Errors = %v, want none (r3's error lies outside the final window)", res.Errors)
	}
	if res.Tried != 8 || f.total() != 8 {
		t.Errorf("Tried = %d (encodes %d), want 8 (out-of-window encodes still count)", res.Tried, f.total())
	}
}

func TestSearch_ParallelRace(t *testing.T) {
	ladder := testLadder(8)
	bases := []float64{500000, 300000, 200000, 150000, 120000, 90000, 70000, 50000}
	rng := rand.New(rand.NewSource(7))
	for round := range 12 {
		delays := make([]time.Duration, len(ladder))
		for i := range delays {
			delays[i] = time.Duration(rng.Intn(4)) * time.Millisecond
		}
		f := newFake(ladder, expModel(bases, 0.01))
		f.delay = func(idx int) time.Duration { return delays[idx] }
		par := 1 + round%5
		res := mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: par}, f)
		limit, _ := limits(100000, 0)
		assertUnder(t, res, limit)
		if res.Best.Rung.Label != "r1" {
			t.Errorf("round %d par %d: Best = %s, want r1 (r0 skips)", round, par, res.Best.Rung.Label)
		}
		if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r2,r3" {
			t.Errorf("round %d par %d: Alternatives = %v, want [r2 r3]", round, par, got)
		}
		if strings.Join(res.Skipped, ",") != "r0" {
			t.Errorf("round %d: Skipped = %v", round, res.Skipped)
		}
		if res.Tried != f.total() {
			t.Errorf("round %d: Tried %d != encodes %d", round, res.Tried, f.total())
		}
		// r0: 2, r1..r3: 3 each (mild, harsh, one secant step), r4..r7: at most 1 each.
		if res.Tried < 11 || res.Tried > 15 {
			t.Errorf("round %d par %d: Tried = %d, want 11..15", round, par, res.Tried)
		}
		for i := 4; i < 8; i++ {
			if f.count(i) > 1 {
				t.Errorf("round %d: rung %d encoded %d times", round, i, f.count(i))
			}
		}
	}
}

func TestSearch_ContextCancelAbortsPromptly(t *testing.T) {
	ladder := testLadder(6)
	f := newFake(ladder, expModel([]float64{120000, 110000, 100000, 90000, 80000, 70000}, 0.01))
	f.block = func(int) bool { return true }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := Search(ctx, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: 3}, f.encode)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrNoFit) {
		t.Errorf("cancellation must not be reported as ErrNoFit")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Search took %v after cancel", d)
	}
	if res.Best != nil || len(res.Errors) != 0 {
		t.Errorf("cancelled search reported Best=%v Errors=%v", res.Best, res.Errors)
	}
	if res.Tried != 3 || f.total() != 3 {
		t.Errorf("Tried = %d (encodes %d), want exactly the Parallel=3 first burst", res.Tried, f.total())
	}
}

func TestSearch_PreCancelledContextEncodesNothing(t *testing.T) {
	ladder := testLadder(2)
	f := newFake(ladder, expModel([]float64{1000, 1000}, 0.01))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Search(ctx, Request{Target: 100000, Ladder: ladder, Knob: gifKnob}, f.encode)
	if !errors.Is(err, context.Canceled) || f.total() != 0 {
		t.Errorf("err=%v encodes=%d", err, f.total())
	}
}

func TestSearch_CancelledResultKeepsCompletedCandidates(t *testing.T) {
	ladder := testLadder(3)
	f := newFake(ladder, expModel([]float64{120000, 110000, 100000}, 0.01))
	ctx, cancel := context.WithCancel(context.Background())
	f.block = func(idx int) bool { return idx == 1 } // r1 never finishes; r0 fits instantly
	f.delay = func(idx int) time.Duration {
		if idx == 2 {
			return 10 * time.Millisecond
		}
		return 0
	}
	go func() {
		f.waitCalls(t, 2, 1)
		time.Sleep(100 * time.Millisecond) // r2's 10 ms delay has long passed
		cancel()
	}()
	res, err := Search(ctx, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: 3}, f.encode)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if res.Best == nil || res.Best.Rung.Label != "r0" {
		t.Errorf("completed winner lost on cancel: %+v", res.Best)
	}
	if got := labelsOf(res.Alternatives); strings.Join(got, ",") != "r2" {
		t.Errorf("Alternatives = %v, want [r2] (r1 was cut off)", got)
	}
}

// ---- attempts / desc / knobs -------------------------------------------

func TestSearch_AttemptNumbersPerRung(t *testing.T) {
	ladder := testLadder(2)
	f := newFake(ladder, expModel([]float64{200000, 200000}, 0.01))
	mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: 2}, f)
	for idx := range 2 {
		f.mu.Lock()
		calls := f.calls[idx]
		f.mu.Unlock()
		for i, c := range calls {
			if c.attempt != i+1 {
				t.Errorf("rung %d call %d has attempt %d", idx, i, c.attempt)
			}
		}
	}
}

func TestSearch_DescAndFormat(t *testing.T) {
	cases := []struct {
		name   string
		rung   Rung
		knob   Knob
		format string
		want   string
		wantF  string
	}{
		{"gif label", Rung{Label: "20 fps · 128 colours · 128 px", Format: "gif"}, KnobFor("gif"), "", "fit at 20 fps · 128 colours · 128 px · lossy 30", "gif"},
		{"webp quality inverted", Rung{Label: "25 fps · 128 px", Format: "webp"}, KnobFor("webp"), "", "fit at 25 fps · 128 px · quality 80", "webp"},
		{"apng colour step", Rung{Label: "APNG · 25 fps · 256 colours", Format: "apng"}, KnobFor("apng"), "", "fit at APNG · 25 fps · 256 colours · colour step 0", "apng"},
		{"unnamed knob", Rung{Label: "x"}, Knob{Min: 1, Max: 9, Mild: 4}, "avif", "fit at x · knob 4", "avif"},
		{"label from fields", Rung{FPS: 12.5, Width: 112, Height: 112, Colors: 64, Format: "gif"}, KnobFor("gif"), "", "fit at GIF · 12.5 fps · 64 colours · 112 px · lossy 30", "gif"},
		{"bare rung", Rung{}, KnobFor("gif"), "gif", "fit at master settings · lossy 30", "gif"},
	}
	for _, c := range cases {
		ladder := []Rung{c.rung}
		f := &fake{size: func(int, int) int64 { return 1000 }, calls: map[int][]call{}, index: map[string]int{c.rung.Label: 0}}
		res, err := Search(context.Background(), Request{Target: 100000, Ladder: ladder, Knob: c.knob, Format: c.format}, f.encode)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.Best.Desc != c.want {
			t.Errorf("%s: Desc = %q, want %q", c.name, res.Best.Desc, c.want)
		}
		if res.Best.Format != c.wantF {
			t.Errorf("%s: Format = %q, want %q", c.name, res.Best.Format, c.wantF)
		}
	}
}

func TestSearch_PerFormatKnobOverrides(t *testing.T) {
	// A sticker-style ladder: apng rungs (colour steps) that never fit, then
	// gif rungs searched over lossy.
	ladder := []Rung{
		{FPS: 25, Colors: 256, Format: "apng", Label: "r0"},
		{FPS: 10, Colors: 64, Format: "apng", Label: "r1"},
		{FPS: 25, Colors: 256, Format: "gif", Label: "r2"},
		{FPS: 20, Colors: 128, Format: "gif", Label: "r3"},
	}
	var mu sync.Mutex
	seen := map[string][]int{}
	size := func(idx, knob int) int64 {
		if idx < 2 {
			return 10e6 // apng never fits
		}
		return int64(200000 * math.Exp(-0.01*float64(knob)))
	}
	f := newFake(ladder, size)
	enc := func(ctx context.Context, r Rung, knob, attempt int) (string, int64, error) {
		mu.Lock()
		seen[r.Format] = append(seen[r.Format], knob)
		mu.Unlock()
		return f.encode(ctx, r, knob, attempt)
	}
	req := Request{
		Target: 100000, Ladder: ladder,
		Knob:  KnobFor("apng"),
		Knobs: map[string]Knob{"gif": KnobFor("gif")},
	}
	res, err := Search(context.Background(), req, enc)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range seen["apng"] {
		if k < 0 || k > 3 {
			t.Errorf("apng rung probed knob %d outside 0..3", k)
		}
	}
	for _, k := range seen["gif"] {
		if k < 30 || k > 150 {
			t.Errorf("gif rung probed knob %d outside the gif mild..harsh bracket", k)
		}
	}
	if res.Best.Rung.Label != "r2" || !strings.HasSuffix(res.Best.Desc, "lossy "+fmt.Sprint(res.Best.Knob)) {
		t.Errorf("Best = %+v", res.Best)
	}
	if strings.Join(res.Skipped, ",") != "r0,r1" {
		t.Errorf("Skipped = %v", res.Skipped)
	}
}

func TestSearch_KnobDefaultsMildMinHarshMax(t *testing.T) {
	ladder := testLadder(1)
	f := newFake(ladder, expModel([]float64{200000}, 0.01))
	mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: Knob{Min: 10, Max: 90}}, f)
	if got := f.knobs(0); got[0] != 10 || got[1] != 90 {
		t.Errorf("probes = %v, want mild=Min 10 then harsh=Max 90", got)
	}
	// Probes outside [Min, Max] are clamped.
	f = newFake(ladder, expModel([]float64{200000}, 0.01))
	mustSearch(t, Request{Target: 100000, Ladder: ladder, Knob: Knob{Min: 10, Max: 90, Mild: 5, Harsh: 200}}, f)
	if got := f.knobs(0); got[0] != 10 || got[1] != 90 {
		t.Errorf("clamped probes = %v, want [10 90 …]", got)
	}
}

// TestSearch_KnobsLookupUsesEffectiveFormat: a rung with Format "" resolves
// to Request.Format for Candidate.Format, and the same effective format must
// select the Request.Knobs override — here the gif override is the only knob
// (Request.Knob is zero), so the rung probes the gif mild point instead of
// silently searching the single point 0.
func TestSearch_KnobsLookupUsesEffectiveFormat(t *testing.T) {
	ladder := []Rung{{Label: "r0"}} // Format "" → effective format "gif"
	f := newFake(ladder, expModel([]float64{120000}, 0.01))
	req := Request{
		Target: 100000, Ladder: ladder, Format: "gif",
		Knobs: map[string]Knob{"gif": gifKnob},
	}
	res := mustSearch(t, req, f)
	if got := f.knobs(0); len(got) != 1 || got[0] != 30 {
		t.Errorf("probes = %v, want the gif override's mild probe [30]", got)
	}
	if res.Best.Knob != 30 || res.Best.Format != "gif" || res.Best.Desc != "fit at r0 · lossy 30" {
		t.Errorf("Best = %+v", res.Best)
	}
}

// TestSearch_ZeroKnobRejected: a rung that resolves to the zero-valued knob
// (Min == Max == 0, no name — an uninitialised Request, not a deliberate
// single-point search) is a validation error before anything encodes; a
// per-rung override rescues a zero Request.Knob.
func TestSearch_ZeroKnobRejected(t *testing.T) {
	ladder := testLadder(1)
	f := newFake(ladder, expModel([]float64{1000}, 0.01))
	_, err := Search(context.Background(), Request{Target: 1000, Ladder: ladder}, f.encode)
	if err == nil || errors.Is(err, ErrNoFit) || !strings.Contains(err.Error(), "zero knob") {
		t.Errorf("err = %v, want a zero-knob validation error", err)
	}
	if f.total() != 0 {
		t.Errorf("zero-knob request still encoded %d times", f.total())
	}

	over := []Rung{{Format: "gif", Label: "r0", Knob: &Knob{Min: 0, Max: 10, Mild: 2, Harsh: 8, Name: "crf"}}}
	f2 := newFake(over, expModel([]float64{90000}, 0.01))
	res := mustSearch(t, Request{Target: 100000, Ladder: over}, f2)
	if got := f2.knobs(0); len(got) != 1 || got[0] != 2 {
		t.Errorf("probes = %v, want the rung override's mild probe [2]", got)
	}
	if res.Best.Desc != "fit at r0 · crf 2" {
		t.Errorf("Desc = %q", res.Best.Desc)
	}
}

// ---- panic boundary --------------------------------------------------------

// TestSearch_EncoderPanicRepanicsInCaller: a panic inside EncodeFunc happens
// on a rung goroutine, where no caller recover can see it; Search must catch
// it there, cancel and drain the other rungs, and re-panic a *PanicError in
// the caller's goroutine (where e.g. the job runner's recover turns it into
// a job error) instead of killing the process.
func TestSearch_EncoderPanicRepanicsInCaller(t *testing.T) {
	ladder := testLadder(4)
	f := newFake(ladder, expModel([]float64{200000, 200000, 200000, 200000}, 0.01))
	f.gate = map[int]chan struct{}{0: make(chan struct{})} // r0 blocks until cancelled
	enc := func(ctx context.Context, r Rung, knob, attempt int) (string, int64, error) {
		if r.Label == "r1" {
			panic("boom")
		}
		return f.encode(ctx, r, knob, attempt)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		Search(context.Background(), Request{Target: 100000, Ladder: ladder, Knob: gifKnob, Parallel: 2}, enc)
		t.Error("Search returned instead of re-panicking")
	}()
	pe, ok := recovered.(*PanicError)
	if !ok {
		t.Fatalf("recovered %T (%v), want *PanicError", recovered, recovered)
	}
	if pe.Value != "boom" || len(pe.Stack) == 0 {
		t.Errorf("PanicError = {%v, %d stack bytes}, want the original value and a stack", pe.Value, len(pe.Stack))
	}
	if !strings.Contains(pe.Error(), "boom") || !strings.Contains(pe.Error(), "goroutine") {
		t.Errorf("Error() = %q", pe.Error())
	}
	// The panic cancelled the search: the blocked r0 was drained (its gate
	// never opened — only its context ended), r2/r3 never started.
	if f.count(0) != 1 {
		t.Errorf("r0 encodes = %d, want 1", f.count(0))
	}
	if f.count(2) != 0 || f.count(3) != 0 {
		t.Errorf("rungs after the panic were started: r2=%d r3=%d", f.count(2), f.count(3))
	}
}

// ---- per-rung knob overrides -------------------------------------------------

// TestSearch_PerRungKnobOverride: a rung's own Knob wins over Request.Knobs
// and Request.Knob, bounding its probes; a single-point override probes once
// and skips; an invalid override fails validation before anything encodes.
func TestSearch_PerRungKnobOverride(t *testing.T) {
	ladder := []Rung{
		{Colors: 128, Format: "apng", Label: "r0", Knob: &Knob{Min: 0, Max: 1, Name: KnobColourStep}},
		{Colors: 64, Format: "apng", Label: "r1", Knob: &Knob{Min: 0, Max: 0, Name: KnobColourStep}},
		{Colors: 256, Format: "apng", Label: "r2"}, // falls back to Request.Knob
	}
	// r0 and r1 never fit; r2 fits at its harsh probe.
	size := func(idx, knob int) int64 {
		if idx == 2 {
			return []int64{300000, 200000, 90000}[knob]
		}
		return 10e6
	}
	f := newFake(ladder, size)
	res, err := Search(context.Background(), Request{Target: 100000, Ladder: ladder, Knob: KnobFor("apng")}, f.encode)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.knobs(0); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("r0 probes = %v, want [0 1] (bounded by the rung's knob)", got)
	}
	if got := f.knobs(1); len(got) != 1 || got[0] != 0 {
		t.Errorf("r1 probes = %v, want the single point [0]", got)
	}
	for _, k := range f.knobs(2) {
		if k < 0 || k > 2 {
			t.Errorf("r2 probed knob %d outside the request knob 0..2", k)
		}
	}
	if strings.Join(res.Skipped, ",") != "r0,r1" || res.Best.Rung.Label != "r2" {
		t.Errorf("Skipped = %v, Best = %+v", res.Skipped, res.Best)
	}

	// An invalid override is a validation error before any encode.
	bad := []Rung{{Label: "x", Knob: &Knob{Min: 3, Max: 1}}}
	f2 := newFake(bad, func(int, int) int64 { return 1 })
	if _, err := Search(context.Background(), Request{Target: 1000, Ladder: bad, Knob: gifKnob}, f2.encode); err == nil || errors.Is(err, ErrNoFit) {
		t.Errorf("bad per-rung knob: err = %v, want a validation error", err)
	}
	if f2.total() != 0 {
		t.Errorf("bad override still encoded %d times", f2.total())
	}
}

// ---- sticker RGBA probes -------------------------------------------------------

// TestSearch_StickerLadderRGBAProbes drives the real sticker ladder through
// a fake encoder: the RGBA truecolour rungs are probed exactly once each
// (mildest first), win with a knob-less Desc when they fit, and otherwise
// skip so the indexed rungs take over (DESIGN.md §5.4/§9a).
func TestSearch_StickerLadderRGBAProbes(t *testing.T) {
	ladder := StickerAPNGThenGIF(30, 320, 320)
	req := func() Request {
		return Request{
			Target: 524288, Ladder: ladder, Format: "apng",
			Knob:  KnobFor("apng"),
			Knobs: map[string]Knob{"gif": KnobFor("gif")},
		}
	}
	rgba := 0
	for _, r := range ladder {
		if r.Truecolor {
			rgba++
		}
	}
	if rgba != 4 {
		t.Fatalf("sticker ladder has %d RGBA rungs, want 4", rgba)
	}

	// (a) The first RGBA probe fits: one encode, it wins, Desc has no knob.
	f := newFake(ladder, func(idx, knob int) int64 { return 400000 })
	res := mustSearch(t, req(), f)
	if !res.Best.Rung.Truecolor || res.Best.Rung.Label != "APNG · RGBA · 25 fps · 320 px" {
		t.Fatalf("Best = %+v", res.Best)
	}
	if res.Best.Desc != "fit at APNG · RGBA · 25 fps · 320 px" {
		t.Errorf("Desc = %q (a truecolour rung carries no knob clause)", res.Best.Desc)
	}
	if f.count(0) != 1 {
		t.Errorf("winning RGBA rung encoded %d times, want 1", f.count(0))
	}

	// (b) RGBA never fits: each RGBA rung costs exactly one encode, all are
	// skipped, and the mildest indexed rung wins.
	f = newFake(ladder, func(idx, knob int) int64 {
		if ladder[idx].Truecolor {
			return 10e6
		}
		return 400000
	})
	res = mustSearch(t, req(), f)
	if res.Best.Rung.Truecolor || res.Best.Rung.Label != "APNG · 25 fps · 256 colours · 320 px" {
		t.Fatalf("Best = %+v", res.Best)
	}
	for i, r := range ladder {
		if r.Truecolor && f.count(i) != 1 {
			t.Errorf("RGBA rung %d encoded %d times, want exactly 1 (single-point knob)", i, f.count(i))
		}
	}
	if len(res.Skipped) != rgba {
		t.Errorf("Skipped = %v, want exactly the %d RGBA rungs", res.Skipped, rgba)
	}
	for _, s := range res.Skipped {
		if !strings.Contains(s, "RGBA") {
			t.Errorf("skipped rung %q is not an RGBA rung", s)
		}
	}
}

// ---- property test ---------------------------------------------------------

// TestSearch_Properties drives random ladders, models, targets, margins,
// parallelism and failure injection through Search and checks the invariants
// that must always hold: nothing over Target*(1-Margin), Best is the mildest
// rung whose mild or harsh probe fits (milder rungs always finish), the
// alternatives are harsher than Best, mildest first and bounded, Tried is
// bounded, and ErrNoFit ⇔ Best == nil.
func TestSearch_Properties(t *testing.T) {
	rng := rand.New(rand.NewSource(20260819))
	for iter := range 400 {
		n := 1 + rng.Intn(6)
		ladder := testLadder(n)
		bases := make([]float64, n)
		bases[0] = 1000 + rng.Float64()*5e6
		for i := 1; i < n; i++ {
			bases[i] = bases[i-1] * (0.3 + 0.75*rng.Float64()) // mostly decreasing, sometimes not
		}
		k := 0.003 + rng.Float64()*0.03
		wobble := 0.0
		if rng.Intn(3) == 0 {
			wobble = 0.3 * rng.Float64()
		}
		model := func(idx, knob int) int64 {
			s := bases[idx] * math.Exp(-k*float64(knob))
			s *= 1 + wobble*math.Sin(float64(knob*(idx+3))/5)
			return max(1, int64(s))
		}
		target := 1000 + rng.Int63n(600000)
		margin := 0.0
		if rng.Intn(2) == 0 {
			margin = rng.Float64() * 0.2
		}
		failEvery := 0
		if rng.Intn(4) == 0 {
			failEvery = 2 + rng.Intn(5)
		}
		f := newFake(ladder, model)
		f.fail = func(idx, knob, attempt int) error {
			if failEvery > 0 && (idx*7+attempt)%failEvery == 0 {
				return errors.New("injected")
			}
			return nil
		}
		req := Request{
			Target: target, Margin: margin, Ladder: ladder, Knob: gifKnob,
			MaxIter: rng.Intn(8), Parallel: rng.Intn(5), Alternatives: rng.Intn(4),
		}
		if rng.Intn(2) == 0 {
			req.Knob = Knob{Min: 0, Max: 3 + rng.Intn(100), Mild: rng.Intn(3), Name: "k"}
		}
		res, err := Search(context.Background(), req, f.encode)
		if err != nil && !errors.Is(err, ErrNoFit) {
			t.Fatalf("iter %d: unexpected error %v (req %+v)", iter, err, req)
		}
		limit, _ := limits(target, margin)
		assertUnder(t, res, limit)
		if (err != nil) != (res.Best == nil) {
			t.Errorf("iter %d: err=%v Best=%v", iter, err, res.Best)
		}
		maxIter := req.MaxIter
		if maxIter == 0 {
			maxIter = DefaultMaxIter
		}
		if res.Tried != f.total() || res.Tried > n*(2+maxIter) {
			t.Errorf("iter %d: Tried=%d encodes=%d bound=%d", iter, res.Tried, f.total(), n*(2+maxIter))
		}
		alts := req.Alternatives
		if alts == 0 {
			alts = DefaultAlternatives
		}
		if len(res.Alternatives) > alts {
			t.Errorf("iter %d: %d alternatives > %d", iter, len(res.Alternatives), alts)
		}
		if res.Best != nil {
			bestIdx := f.index[res.Best.Rung.Label]
			prev := bestIdx
			for _, a := range res.Alternatives {
				ai := f.index[a.Rung.Label]
				if ai <= prev {
					t.Errorf("iter %d: alternatives not strictly harsher / ordered: best %d, alts %v", iter, bestIdx, labelsOf(res.Alternatives))
				}
				prev = ai
			}
			// Without failures, Best must be the mildest rung whose mild or
			// harsh probe fits (rung-level outcome is decided by those two).
			if failEvery == 0 {
				kn, _ := normalizeKnob(req.Knob)
				want := -1
				for i := range n {
					if model(i, kn.Mild) <= limit || model(i, kn.Harsh) <= limit {
						want = i
						break
					}
				}
				if want != bestIdx {
					t.Errorf("iter %d: Best rung %d, want %d", iter, bestIdx, want)
				}
			}
		}
	}
}
