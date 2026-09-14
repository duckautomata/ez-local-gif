package jobs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Scratch admission (DESIGN.md §4.1 frame-master cap, §9.9 tmpfs sizing).
//
// The RGBA master of a render is frames x width x height x 4 bytes on the
// scratch tmpfs. Its size is known from the compiled plan before ffmpeg
// starts, so a render that could never fit is refused up-front with an
// actionable message instead of failing minutes later with ENOSPC — and
// taking every concurrent render on the same tmpfs down with it. Renders
// that do fit reserve their estimate from a byte budget (the size of the
// scratch filesystem) so concurrent jobs cannot collectively overflow it.
//
// This is the ONLY frame-master cap (2026-09-13): the graph reports
// Plan.Width/Height/Frames and never refuses a plan for its frame count —
// an earlier 8 GiB compile-time cap there refused the still and proxy of an
// untrimmed 4K clip before it could be trimmed/cropped/resized in-app,
// although neither builds a master. Here the cap is applied to what is
// actually allocated: the master of a render (admitScratch, at the output
// size, so a trimmed and fitted render of a huge source is small) and the
// in-RAM buffer of ffmpeg's reverse filter for reversed/bounced previews
// and static reversed renders (admitReversed). Forward stills and proxies
// are never gated on the frame count: a still seeks one frame, a proxy
// streams a head bounded by MaxProxySeconds / MaxProxyWidth / 15 fps. The
// blind spot every byte cap shares: a source whose frame count is unknown
// (Frames 0) passes with need == 0 and is bounded only by the ENOSPC
// mapping and the preview timeouts.

const (
	// DefaultMaxMasterBytes is Options.MaxMasterBytes when unset: 2 GiB, the
	// DESIGN.md §4.1 frame-master cap (the streaming bypass for larger
	// outputs — frames straight into the encoder, no master — is a later
	// item).
	DefaultMaxMasterBytes = 2 << 30

	// MaxMasterBytesCeiling is the largest Options.MaxMasterBytes NewManager
	// accepts (MaxInt64/16 = 512 PiB; larger values are logged and clamped).
	// The scratch reservation of an admitted render is need + max(need/8,
	// 8 MiB) + (factor-1)*need with factor <= 3 (scratchFactor), i.e. at
	// most 3.125*need, and need <= MaxMasterBytes — so with the cap at or
	// under MaxInt64/16 the reserve can never overflow into a non-positive
	// value that reserveScratch would treat as "nothing to reserve" and
	// skip the budget. No real host has 512 PiB of RAM or tmpfs, so the
	// clamp costs an operator nothing.
	MaxMasterBytesCeiling = math.MaxInt64 / 16

	// scratchHeadroomMin/Div size the extra scratch reserved next to the
	// master for the encoder outputs (base.gif, opt.gif, enc.webp, the
	// gifsicle ladder copies, final.*): the larger of need/8 and 8 MiB.
	scratchHeadroomMin = 8 << 20
	scratchHeadroomDiv = 8
)

// masterBytes estimates the RGBA master size of plan; 0 when the frame
// count is unknown (no source duration). 4 B/px is exact for the master
// (rawvideo rgba) and for the reverse stage's buffer too: the graph pins the
// frames to rgba right in front of "reverse" ("format=rgba,reverse"), so a
// reversed render never holds more than Frames x Width x Height x 4 in
// memory whatever depth the chain carried before it — for an animated
// render the master estimate therefore covers the reverse buffer and
// admitScratch alone suffices. That equality breaks for the static formats
// (png/jpeg): render.go cuts their plan to one frame (oneFramePlan) before
// admitScratch, but "-frames:v 1" shortens the encode, not the decode, so
// a reverse filter still buffers the whole clip — reversed static renders
// are admitted by admitReversed on the pre-cut plan first (a bounce alone
// is not: its first output frame is the forward branch's, and the run ends
// before its reverse branch has buffered anything — see render.go). Bounce
// ops (Phase 4) need no extra factor here: graph doubles Plan.Frames (and
// Duration) per bounce, so the estimate already measures the doubled
// output — and each bounce's in-graph reverse branch buffers only the
// pre-bounce half of it.
func masterBytes(p *graph.Plan) int64 {
	if p == nil {
		return 0
	}
	return frameBytes(p, p.Frames)
}

// frameBytes is masterBytes for frames output-sized RGBA frames of plan; 0
// when the count or the frame size is unknown. The product is overflow-safe
// on every factor and saturates at math.MaxInt64, so an absurd plan is
// refused by the cap instead of wrapping into a small or negative number
// that admission would wave through. A saturated count is not the only
// way there: a crafted MP4 (mvhd timescale 1, duration 2^32-1) probes to a
// Duration of 4.29e9 s and at 60 fps plans 2.6e11 frames — well below
// MaxInt, but 8192 x 4096 x 4 x 2.6e11 is past int64 — and graph's bounce
// doubling pins Frames at math.MaxInt, where even Width alone overflows.
// bits.Mul64 on the running product keeps every step exact: a non-zero
// high word or a low word past MaxInt64 means the true product does not
// fit an int64.
func frameBytes(p *graph.Plan, frames int) int64 {
	if p == nil || frames <= 0 || p.Width <= 0 || p.Height <= 0 {
		return 0
	}
	n := uint64(frames)
	for _, f := range [...]uint64{uint64(p.Width), uint64(p.Height), 4} {
		hi, lo := bits.Mul64(n, f)
		if hi != 0 || lo > math.MaxInt64 {
			return math.MaxInt64
		}
		n = lo
	}
	return int64(n)
}

// masterCapError is the ErrInvalidRecipe a render or preview gets when what
// (e.g. "the frame master") would need more than Options.MaxMasterBytes:
// the figures, the limit, what to do about it and the variable that raises
// it. The message is shared by the render's admission (admitScratch) and
// the preview admission of reversed plans (admitReversed).
func (m *Manager) masterCapError(what string, need int64, frames, width, height int) error {
	return fmt.Errorf("%w: %s would need %s (%d frames of %dx%d RGBA); the limit is %s — trim the clip, lower the fps or resize the output (or raise EZLG_MAX_MASTER_BYTES)",
		ErrInvalidRecipe, what, humanBytes(need), frames, width, height, humanBytes(m.opts.MaxMasterBytes))
}

// admitReversed refuses a still/proxy of a reversed OR bounced plan — or a
// static (png/jpeg) render of a reversed one — whose reverse/bounce stage
// would buffer more than Options.MaxMasterBytes: the reverse filter holds
// every output-sized RGBA frame it is handed in memory until EOF, which is
// frames frames — the whole trimmed clip for a still and for a static
// render (whose "-frames:v 1" cuts the encode, not the decode), the tail
// the proxy's seek leaves for a reversed proxy (proxyBufferFrames). A
// bounced plan (Phase 4) is treated the same with frames = Plan.Frames for
// a still or proxy: its split/reverse branch buffers the pre-bounce half of
// the (already doubled) frame count, so the doubled count is a conservative
// bound — a still at t >= D really is served by that branch, after it has
// buffered the whole forward pass — and bounced previews are never seeked
// (enc), so no tail estimate applies. A bounce-only static render is not
// gated here (render.go: its first frame is the forward branch's, and the
// run ends before the reverse branch fills). The preview endpoints hand the
// plan to ffmpeg straight away, without the render path's scratch
// admission, and the static render's scratch admission sees a one-frame
// plan, so without this check a reversed 1080p clip that an animated
// render refuses up-front would still be decoded for its preview or PNG
// export — and, the graph applying no frame-count cap of its own, that
// buffer would be bounded by nothing but host RAM. Forward plans buffer
// nothing and are never gated on the frame count; an unknown frame count
// (0) cannot be checked; what names the preview/render for the message.
func (m *Manager) admitReversed(plan *graph.Plan, frames int, what string) error {
	if plan == nil || (!plan.Reversed && !plan.Bounced) {
		return nil
	}
	need := frameBytes(plan, frames)
	if need == 0 || need <= m.opts.MaxMasterBytes {
		return nil
	}
	return m.masterCapError("the reverse buffer of "+what, need, frames, plan.Width, plan.Height)
}

// scratchReserve is what a render reserves from the scratch budget: the
// master plus headroom for the encoded outputs. 0 stays 0 (unknown).
func scratchReserve(need int64) int64 {
	if need <= 0 {
		return 0
	}
	return need + max(need/scratchHeadroomDiv, scratchHeadroomMin)
}

// scratchFactor is how many master-sized chunks of scratch a render may
// need on top of the headroom: 1 for outputs encoded straight from the
// master (gif, webp, RGBA apng, static); +1 when PNG intermediates of every
// frame are written next to it — the indexed-APNG tile sheet, the PNG
// frames avifenc reads, or the frame export's images plus their zip copy
// (both compressed, together bounded by about one master); and +1 more for
// a fit search, whose ladder keeps up to fitParallel() candidates plus the
// per-variant sheet/frame intermediates alive until the search returns and
// renderFit removes its directory.
func scratchFactor(out recipe.Output) int64 {
	format := strings.ToLower(out.Format)
	f := int64(1)
	switch format {
	case recipe.FormatAVIF, recipe.FormatFrames:
		f = 2
	case recipe.FormatAPNG:
		if out.Colors > 0 || out.FitBytes > 0 {
			f = 2
		}
	case recipe.FormatGIF:
		if isGifskiOutput(out) {
			f = 2 // gifski reads PNG frames of the whole master
		}
	}
	if out.FitBytes > 0 && fitFormats[format] {
		f++
	}
	return f
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
// concurrent renders when needed, cancellable). factor (>= 1, see
// scratchFactor) multiplies the reservation for outputs that write PNG
// intermediates of every frame next to the master. It returns the release
// func (idempotent; call after the scratch dir is removed).
func (m *Manager) admitScratch(ctx context.Context, j *job, plan *graph.Plan, factor int64) (release func(), err error) {
	need := masterBytes(plan)
	if need == 0 {
		return func() {}, nil // unknown length: only the ENOSPC mapping can help
	}
	desc := fmt.Sprintf("the frame master would need %s (%d frames of %dx%d RGBA)", humanBytes(need), plan.Frames, plan.Width, plan.Height)
	if need > m.opts.MaxMasterBytes {
		return nil, m.masterCapError("the frame master", need, plan.Frames, plan.Width, plan.Height)
	}
	reserve := scratchReserve(need)
	if factor > 1 {
		reserve += (factor - 1) * need
		desc += fmt.Sprintf(" plus %d× as much for the PNG intermediates / fit candidates this render writes", factor-1)
	}
	return m.reserveScratch(ctx, j, reserve, desc)
}

// reserveScratch reserves bytes from the scratch budget (waiting for
// concurrent renders when needed, cancellable) and live-checks the
// filesystem; desc describes the need for error messages. The returned
// release is idempotent.
func (m *Manager) reserveScratch(ctx context.Context, j *job, reserve int64, desc string) (release func(), err error) {
	if reserve <= 0 {
		return func() {}, nil
	}
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

// admitOptimizeScratch reserves scratch for the optimize preset's fit search:
// unlike a decode render it has no plan or master, but the search can hold up
// to fitParallel() candidates at once (each at most about the source's size —
// the optimiser only ever shrinks) plus the delivered files. Without a fit
// budget the path writes a single output and needs no reservation beyond the
// ENOSPC mapping.
func (m *Manager) admitOptimizeScratch(ctx context.Context, j *job, srcPath string, out recipe.Output) (func(), error) {
	if out.FitBytes <= 0 {
		return func() {}, nil
	}
	fi, err := os.Stat(srcPath)
	if err != nil || fi.Size() <= 0 {
		return func() {}, nil // unknown size: only the ENOSPC mapping can help
	}
	need := fi.Size() * int64(fitParallel()+1)
	desc := fmt.Sprintf("the optimize fit search may hold %s of candidates (%d concurrent attempts on the %s source)",
		humanBytes(need), fitParallel(), humanBytes(fi.Size()))
	return m.reserveScratch(ctx, j, scratchReserve(need), desc)
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
