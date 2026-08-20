package fit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"sync/atomic"
)

// Defaults applied by Search for zero Request fields, and the width of the
// stop window below the margin limit (DESIGN.md §5.4: "1–2 % margin").
const (
	DefaultMargin       = 0.02
	DefaultMaxIter      = 5
	DefaultParallel     = 1
	DefaultAlternatives = 2
	// WindowWidth is the fraction of Target below Target*(1-Margin) that
	// counts as "close enough": a candidate inside
	// [Target*(1-Margin-WindowWidth), Target*(1-Margin)] ends its rung.
	WindowWidth = 0.02
)

// searcher holds the resolved request for one Search call.
type searcher struct {
	req       Request
	encode    EncodeFunc
	limit     int64   // largest acceptable size: floor(Target*(1-Margin))
	lower     int64   // window floor: ceil(Target*(1-Margin-WindowWidth)), clamped to [0, limit]
	logAim    float64 // log of the window centre — what the secant aims at
	maxIter   int
	parallel  int
	alts      int
	knob      Knob            // default knob, normalised
	knobs     map[string]Knob // per-format overrides, normalised
	rungKnobs []Knob          // resolved knob per ladder index (Rung.Knob > Knobs[format] > Knob)
	tried     atomic.Int64
}

func newSearcher(req Request, encode EncodeFunc) (*searcher, error) {
	if encode == nil {
		return nil, errors.New("fit: encode function is nil")
	}
	if len(req.Ladder) == 0 {
		return nil, errors.New("fit: ladder is empty")
	}
	if req.Target <= 0 {
		return nil, fmt.Errorf("fit: target must be positive, got %d", req.Target)
	}
	margin := req.Margin
	if margin == 0 {
		margin = DefaultMargin
	}
	if margin < 0 || margin >= 1 {
		return nil, fmt.Errorf("fit: margin %g is outside [0, 1)", req.Margin)
	}
	target := float64(req.Target)
	limit := int64(math.Floor(target * (1 - margin)))
	if limit < 1 {
		return nil, fmt.Errorf("fit: target %d with margin %g leaves no budget", req.Target, margin)
	}
	lower := int64(math.Ceil(target * (1 - margin - WindowWidth)))
	lower = max(0, min(lower, limit))

	knob, err := normalizeKnob(req.Knob)
	if err != nil {
		return nil, err
	}
	knobs := make(map[string]Knob, len(req.Knobs))
	for format, k := range req.Knobs {
		nk, err := normalizeKnob(k)
		if err != nil {
			return nil, fmt.Errorf("%w (override for format %q)", err, format)
		}
		knobs[format] = nk
	}

	s := &searcher{
		req:      req,
		encode:   encode,
		limit:    limit,
		lower:    lower,
		logAim:   math.Log(math.Max(1, float64(lower+limit)/2)),
		maxIter:  req.MaxIter,
		parallel: req.Parallel,
		alts:     req.Alternatives,
		knob:     knob,
		knobs:    knobs,
	}
	switch {
	case s.maxIter == 0:
		s.maxIter = DefaultMaxIter
	case s.maxIter < 0:
		s.maxIter = 0
	}
	if s.parallel <= 0 {
		s.parallel = DefaultParallel
	}
	switch {
	case s.alts == 0:
		s.alts = DefaultAlternatives
	case s.alts < 0:
		s.alts = 0
	}
	s.rungKnobs = make([]Knob, len(req.Ladder))
	for i, r := range req.Ladder {
		format := s.formatOf(r)
		raw, k := req.Knob, knob
		if o, ok := knobs[format]; ok {
			raw, k = req.Knobs[format], o
		}
		if r.Knob != nil {
			raw = *r.Knob
			nk, err := normalizeKnob(raw)
			if err != nil {
				return nil, fmt.Errorf("%w (override for rung %q)", err, labelOf(r))
			}
			k = nk
		}
		// A zero-valued knob (Min == Max == 0 with no name) is an
		// uninitialised Request, not a deliberate single-point search: fail
		// loudly instead of silently probing knob 0 once per rung.
		if raw == (Knob{}) {
			return nil, fmt.Errorf("fit: rung %q resolves to a zero knob: set Request.Knob (e.g. KnobFor(%q)), a Request.Knobs entry for the rung's format, or Rung.Knob", labelOf(r), format)
		}
		s.rungKnobs[i] = k
	}
	return s, nil
}

// formatOf resolves the effective format of a rung.
func (s *searcher) formatOf(r Rung) string {
	if r.Format != "" {
		return r.Format
	}
	return s.req.Format
}

// fits reports whether a sample is under the margin limit; inWindow whether
// it is also close enough to stop refining.
func (s *searcher) fits(p sample) bool { return p.bytes <= s.limit }

func (s *searcher) inWindow(p sample) bool { return p.bytes <= s.limit && p.bytes >= s.lower }

// dist is the signed log-distance of a size from the aim (> 0 = too big).
func (s *searcher) dist(bytes int64) float64 {
	return math.Log(math.Max(1, float64(bytes))) - s.logAim
}

// rungOutcome is what one rung's goroutine reports back.
type rungOutcome struct {
	idx       int
	cand      *Candidate  // best candidate under the limit, nil if none
	skipped   bool        // harsh probe still over the limit
	errs      []string    // encoder failures (labelled)
	cancelled bool        // the rung's context ended before it finished; nothing is recorded
	panicked  *PanicError // the EncodeFunc panicked; run re-panics it in the caller's goroutine
}

// run is the scheduler: starts rungs mildest first under the parallelism
// bound, tracks the mildest fit and the alternatives window, cancels rungs
// that fall outside it, and assembles the Result.
func (s *searcher) run(ctx context.Context) (Result, error) {
	// stop cancels every rung when an EncodeFunc panic was caught, so the
	// running rungs drain and the panic can be re-raised on this goroutine
	// (a Go panic is only recoverable where it happened — without this, an
	// encoder panic on a rung goroutine would kill the whole process).
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	n := len(s.req.Ladder)
	results := make(chan rungOutcome, n) // buffered: rung goroutines never block on send
	cancels := make(map[int]context.CancelFunc, s.parallel)
	cands := make([]*Candidate, n)
	rungErrs := make([][]string, n)
	skipped := make([]bool, n)
	bestIdx := -1
	var panicked *PanicError

	// window is the harshest rung index that may still run: every rung
	// until a fit is known, then the winner plus Alternatives more.
	window := func() int {
		if bestIdx < 0 {
			return n - 1
		}
		return min(n-1, bestIdx+s.alts)
	}

	next, running := 0, 0
	for {
		for running < s.parallel && next <= window() && ctx.Err() == nil {
			rctx, cancel := context.WithCancel(ctx)
			cancels[next] = cancel
			go func(idx int, r Rung) {
				var o rungOutcome
				defer func() {
					if p := recover(); p != nil {
						o = rungOutcome{idx: idx, panicked: &PanicError{Value: p, Stack: debug.Stack()}}
					}
					results <- o
				}()
				o = s.searchRung(rctx, idx, r)
			}(next, s.req.Ladder[next])
			next++
			running++
		}
		if running == 0 {
			break
		}
		o := <-results
		running--
		cancels[o.idx]()
		delete(cancels, o.idx)
		if o.panicked != nil {
			if panicked == nil {
				panicked = o.panicked
			}
			stop() // no new rungs start; the running ones are cancelled and drain
			continue
		}
		if o.cancelled {
			continue
		}
		rungErrs[o.idx] = o.errs
		skipped[o.idx] = o.skipped
		if o.cand == nil {
			continue
		}
		cands[o.idx] = o.cand
		if bestIdx < 0 || o.idx < bestIdx {
			bestIdx = o.idx
			for idx, cancel := range cancels {
				if idx > window() {
					cancel()
				}
			}
		}
	}

	if panicked != nil {
		// Every rung has drained; re-panic on the caller's goroutine so its
		// recover (e.g. the job runner's) can turn this into an error.
		panic(panicked)
	}

	// The result is assembled only from rungs inside the final winner +
	// Alternatives window: a harsher rung that completed early (before the
	// window shrank past it) still contributes no candidate, error or skip,
	// so the same request yields the same Result regardless of goroutine
	// completion order. Its Encode calls still count in Tried.
	res := Result{Tried: int(s.tried.Load())}
	last := window()
	for i := 0; i <= last; i++ {
		res.Errors = append(res.Errors, rungErrs[i]...)
		if skipped[i] {
			res.Skipped = append(res.Skipped, labelOf(s.req.Ladder[i]))
		}
	}
	if bestIdx >= 0 {
		res.Best = cands[bestIdx]
		for i := bestIdx + 1; i <= last; i++ {
			if cands[i] != nil {
				res.Alternatives = append(res.Alternatives, *cands[i])
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("fit: search aborted: %w", err)
	}
	if res.Best == nil {
		return res, ErrNoFit
	}
	return res, nil
}

// sample is one encode's outcome.
type sample struct {
	knob  int
	bytes int64
	path  string
}

// searchRung runs the mild/harsh probes and the secant for one rung.
func (s *searcher) searchRung(ctx context.Context, idx int, r Rung) rungOutcome {
	out := rungOutcome{idx: idx}
	k := s.rungKnobs[idx]
	label := labelOf(r)
	attempt := 0

	// probe encodes at knob; ok is false on failure or cancellation (both
	// are recorded on out, cancellation silently).
	probe := func(knob int) (sample, bool) {
		attempt++
		s.tried.Add(1)
		path, bytes, err := s.encode(ctx, r, knob, attempt)
		if err == nil && bytes <= 0 {
			err = errors.New("encoder produced an empty file")
		}
		if err != nil {
			if ctx.Err() != nil {
				out.cancelled = true
			} else {
				out.errs = append(out.errs, fmt.Sprintf("%s (%s %d): %v", label, k.Name, knob, err))
			}
			return sample{}, false
		}
		return sample{knob: knob, bytes: bytes, path: path}, true
	}

	mild, ok := probe(k.Mild)
	if !ok {
		return out
	}
	if s.fits(mild) {
		out.cand = s.candidate(r, k, mild)
		return out
	}
	if k.Harsh == k.Mild {
		out.skipped = true
		return out
	}
	harsh, ok := probe(k.Harsh)
	if !ok {
		return out
	}
	if !s.fits(harsh) {
		out.skipped = true
		return out
	}
	best := s.refine(probe, mild, harsh)
	if out.cancelled {
		return out
	}
	out.cand = s.candidate(r, k, best)
	return out
}

// refine runs the secant between an over-budget probe (lo, milder knob)
// and an under-budget one (hi, harsher knob): a bracketed regula falsi on
// log(size) with Illinois damping, rounded to integer knobs strictly inside
// the bracket. It returns the best candidate seen (largest size under the
// limit; ties → milder knob) and stops as soon as one lands in the window,
// when the bracket has no interior integers left, after maxIter steps, or
// on an encode failure.
func (s *searcher) refine(probe func(int) (sample, bool), lo, hi sample) sample {
	best := hi
	if s.inWindow(hi) {
		return best
	}
	fLo, fHi := s.dist(lo.bytes), s.dist(hi.bytes) // fLo > 0 > fHi
	side := 0                                      // which end the last probe replaced: -1 = hi, +1 = lo
	for it := 0; it < s.maxIter && hi.knob-lo.knob > 1; it++ {
		p, ok := probe(secantStep(lo.knob, hi.knob, fLo, fHi))
		if !ok {
			break
		}
		if !s.fits(p) {
			lo, fLo = p, s.dist(p.bytes)
			if side == +1 {
				fHi /= 2
			}
			side = +1
			continue
		}
		if p.bytes > best.bytes || (p.bytes == best.bytes && p.knob < best.knob) {
			best = p
		}
		if s.inWindow(p) {
			return best
		}
		hi, fHi = p, s.dist(p.bytes)
		if side == -1 {
			fLo /= 2
		}
		side = -1
	}
	return best
}

// secantStep interpolates the knob where the log-distance crosses zero
// between (loKnob, fLo > 0) and (hiKnob, fHi < 0), rounded and clamped to
// the integers strictly inside the bracket (the caller guarantees
// hiKnob - loKnob > 1). Degenerate slopes fall back to bisection.
func secantStep(loKnob, hiKnob int, fLo, fHi float64) int {
	span := float64(hiKnob - loKnob)
	t := 0.5
	if d := fLo - fHi; d > 0 && !math.IsInf(d, 0) && !math.IsNaN(d) {
		t = fLo / d
	}
	est := loKnob + int(math.Round(t*span))
	return min(max(est, loKnob+1), hiKnob-1)
}

// candidate builds the Candidate for a fitting sample. Truecolour rungs get
// no knob clause in Desc (their single-point knob changes nothing).
func (s *searcher) candidate(r Rung, k Knob, p sample) *Candidate {
	desc := "fit at " + labelOf(r)
	if !r.Truecolor {
		desc += " · " + describeKnob(k, p.knob)
	}
	return &Candidate{
		Rung:   r,
		Knob:   p.knob,
		Bytes:  p.bytes,
		Path:   p.path,
		Format: s.formatOf(r),
		Desc:   desc,
	}
}
