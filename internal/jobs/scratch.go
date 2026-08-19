package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// Scratch admission (DESIGN.md §4.1 frame cap, §9.9 tmpfs sizing).
//
// The RGBA master of a render is frames x width x height x 4 bytes on the
// scratch tmpfs. Its size is known from the compiled plan before ffmpeg
// starts, so a render that could never fit is refused up-front with an
// actionable message instead of failing minutes later with ENOSPC — and
// taking every concurrent render on the same tmpfs down with it. Renders
// that do fit reserve their estimate from a byte budget (the size of the
// scratch filesystem) so concurrent jobs cannot collectively overflow it.

const (
	// DefaultMaxMasterBytes is Options.MaxMasterBytes when unset: 2 GiB, the
	// DESIGN.md §4.1 frame-master cap (the streaming bypass for larger
	// sources is Phase 2).
	DefaultMaxMasterBytes = 2 << 30

	// scratchHeadroomMin/Div size the extra scratch reserved next to the
	// master for the encoder outputs (base.gif, opt.gif, enc.webp, the
	// gifsicle ladder copies, final.*): the larger of need/8 and 8 MiB.
	scratchHeadroomMin = 8 << 20
	scratchHeadroomDiv = 8
)

// masterBytes estimates the RGBA master size of plan; 0 when the frame
// count is unknown (no source duration).
func masterBytes(p *graph.Plan) int64 {
	if p == nil || p.Frames <= 0 || p.Width <= 0 || p.Height <= 0 {
		return 0
	}
	return int64(p.Frames) * int64(p.Width) * int64(p.Height) * 4
}

// scratchReserve is what a render reserves from the scratch budget: the
// master plus headroom for the encoded outputs. 0 stays 0 (unknown).
func scratchReserve(need int64) int64 {
	if need <= 0 {
		return 0
	}
	return need + max(need/scratchHeadroomDiv, scratchHeadroomMin)
}

// byteBudget is a counting semaphore over bytes: acquire blocks until the
// requested amount fits under the limit (or ctx is done); release returns
// it. limit <= 0 means unlimited.
type byteBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
	wake  chan struct{} // closed and replaced on every release
}

func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{limit: limit, wake: make(chan struct{})}
}

// Limit returns the budget size (0 = unlimited).
func (b *byteBudget) Limit() int64 { return b.limit }

// Used returns the bytes currently reserved.
func (b *byteBudget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// tryAcquire reserves n bytes without blocking; ok is false when they do
// not fit right now.
func (b *byteBudget) tryAcquire(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit > 0 && b.used+n > b.limit {
		return false
	}
	b.used += n
	return true
}

// acquire reserves n bytes, waiting for other holders to release when
// necessary. n > limit can never succeed and is reported as an error at
// once (callers check the limit first for a friendlier message). The
// returned release is idempotent.
func (b *byteBudget) acquire(ctx context.Context, n int64) (release func(), err error) {
	if n <= 0 {
		return func() {}, nil
	}
	if b.limit > 0 && n > b.limit {
		return nil, fmt.Errorf("scratch budget: %d bytes requested, %d available in total", n, b.limit)
	}
	for {
		b.mu.Lock()
		if b.limit <= 0 || b.used+n <= b.limit {
			b.used += n
			b.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { b.release(n) }) }, nil
		}
		wake := b.wake
		b.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	close(b.wake)
	b.wake = make(chan struct{})
	b.mu.Unlock()
}

// admitScratch checks the plan's master estimate against the per-render cap
// and the scratch filesystem, then reserves it from the budget (waiting for
// concurrent renders when needed, cancellable). It returns the release func
// (idempotent; call after the scratch dir is removed).
func (m *Manager) admitScratch(ctx context.Context, j *job, plan *graph.Plan) (release func(), err error) {
	need := masterBytes(plan)
	if need == 0 {
		return func() {}, nil // unknown length: only the ENOSPC mapping can help
	}
	desc := fmt.Sprintf("the frame master would need %s (%d frames of %dx%d RGBA)", humanBytes(need), plan.Frames, plan.Width, plan.Height)
	if need > m.opts.MaxMasterBytes {
		return nil, fmt.Errorf("%w: %s; the limit is %s — trim the clip, lower the fps or resize the output (or raise EZLG_MAX_MASTER_BYTES)",
			ErrInvalidRecipe, desc, humanBytes(m.opts.MaxMasterBytes))
	}
	reserve := scratchReserve(need)
	if limit := m.scratch.Limit(); limit > 0 && reserve > limit {
		return nil, fmt.Errorf("%w: %s but the scratch filesystem %s holds only %s — trim the clip, lower the fps or resize the output (or raise shm_size / point EZLG_SCRATCH at a larger filesystem)",
			ErrInvalidRecipe, desc, m.st.Scratch, humanBytes(limit))
	}
	if !m.scratch.tryAcquire(reserve) {
		m.setStage(j, StageProbe, 0, fmt.Sprintf("waiting for scratch space (%s needed, %s reserved by other renders)", humanBytes(reserve), humanBytes(m.scratch.Used())))
		release, err = m.scratch.acquire(ctx, reserve)
		if err != nil {
			return nil, err
		}
	} else {
		var once sync.Once
		release = func() { once.Do(func() { m.scratch.release(reserve) }) }
	}
	// Live check: something outside the budget (a foreign tenant of the
	// tmpfs, an unknown-length render) may have eaten the space.
	if free, ok := m.st.ScratchFree(); ok && free < reserve {
		release()
		return nil, fmt.Errorf("scratch %s has only %s free but %s (plus room for the encoded output) — retry when other renders finish, or raise shm_size",
			m.st.Scratch, humanBytes(free), desc)
	}
	return release, nil
}

// isNoSpace reports whether err is (or carries in its ffmpeg/gifsicle
// stderr tail) an out-of-space failure.
func isNoSpace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space left on device") || strings.Contains(s, "enospc")
}

// describeNoSpace turns an ENOSPC failure into an actionable message; other
// errors pass through unchanged.
func (m *Manager) describeNoSpace(err error) error {
	if !isNoSpace(err) {
		return err
	}
	return fmt.Errorf("scratch %s is full (no space left on device): trim the clip, lower the fps or resize the output, or raise shm_size / point EZLG_SCRATCH at a larger filesystem — %w", m.st.Scratch, err)
}

// humanBytes renders n as "64 MiB" / "3.9 GiB" for messages.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	if v >= 10 || v == float64(int64(v)) {
		return fmt.Sprintf("%.0f %s", v, suffix)
	}
	return fmt.Sprintf("%.1f %s", v, suffix)
}
