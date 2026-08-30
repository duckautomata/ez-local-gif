package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// stillMemoVersion salts the still memo key (stillKey): bump it whenever
// enc.StillArgs / StillArgsFromStart or the still path here change what a
// (sources, ops, output, t, maxW) tuple renders to, or the memo keeps
// serving frames the old code produced. 1: reversed stills of VFR animation
// sources decode from TrimStart (enc reversedSeekFor honours
// Plan.SourceVFR) — the frames memoised before that fix were wrong.
const stillMemoVersion = "2026-08-22.1"

// Still renders a single preview frame (PNG bytes) for the recipe's op stack
// at time t seconds, at most maxW pixels wide (0 = 480). Results are
// memoised in scratch keyed by (recipe hash sans output, t, maxW). Fast
// path (~100 ms) — used for scrubbing, crop and eyedropper.
//
// Only the geometry/timing part of out (Format, Width, Height, Fit, FPS) is
// used, so quality-knob changes never invalidate the memo. t is an output
// (preview) time showing frame floor(t*FPS) of the plan: it is clamped to
// the last frame's midpoint (Frames-0.5)/FPS — or the plan's duration when
// the frame count is unknown, or 0 for a still source, whose only frame is
// at 0 whatever t the scrubber sends — and enc.StillArgs maps it to source
// time. A scrubber that addresses frames by index therefore asks for the
// midpoint (i+0.5)/FPS, which is robust to rounding on both sides and, for
// the last frame, yields that frame rather than an error or its predecessor.
//
// Rendering a still counts as using the source: the blob is touched so the
// store's sweeper measures its TTL from the last use.
//
// Still is StillSources for a single-source recipe.
func (m *Manager) Still(ctx context.Context, srcHash string, ops []recipe.Op, out recipe.Output, t float64, maxW int) ([]byte, error) {
	return m.StillSources(ctx, []string{srcHash}, ops, out, t, maxW)
}

// StillSources (Phase 3) is Still for a recipe with overlay sources: srcs[0]
// is the main source, srcs[1:] the overlay assets the ops reference by
// index. Every source is looked up and touched; the memo key covers all of
// them and the canonical ops (so the text of a text overlay is part of it).
// Autocrop ops are resolved first (ResolveAutoCrop), text overlays are
// written to a throw-away scratch dir for the render, and an animated
// overlay is seeked to the same output time as the main source
// (enc.StillArgs). An unknown main source is store.ErrNotFound (as for
// Still); a missing overlay source, a source without probe info or an image
// sequence in an overlay position is an ErrInvalidRecipe.
//
// Admission: a reversed or bounced plan is refused (ErrInvalidRecipe,
// naming EZLG_MAX_MASTER_BYTES) when its reverse/bounce stage would buffer
// more than Options.MaxMasterBytes — the whole (for bounce: doubled)
// trimmed clip in output-sized RGBA frames, as the render's master would
// (admitReversed). The ffmpeg run
// takes a preview slot (PreviewConcurrency) and concurrent requests for the
// same memo key share one run; a memo hit waits for neither.
func (m *Manager) StillSources(ctx context.Context, srcs []string, ops []recipe.Op, out recipe.Output, t float64, maxW int) ([]byte, error) {
	if maxW <= 0 {
		maxW = DefaultStillWidth
	}
	if len(srcs) == 0 {
		return nil, fmt.Errorf("%w: no sources", ErrInvalidRecipe)
	}
	if !recipe.IsHash(srcs[0]) {
		return nil, fmt.Errorf("%w: %q is not a source hash", store.ErrNotFound, srcs[0])
	}
	if _, err := m.st.GetBlob(srcs[0]); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: source %s", store.ErrNotFound, short(srcs[0]))
		}
		return nil, err
	}
	ops = stripAutoCropResolved(ops)
	s, err := m.resolveSources(srcs)
	if err != nil {
		return nil, err
	}
	subset := stillOutput(out)
	plan, err := m.compile(ctx, s, ops, subset)
	if err != nil {
		return nil, err
	}
	if err := m.admitReversed(plan, plan.Frames, "this still"); err != nil {
		return nil, err
	}
	t = clampStillTime(plan, t, s.main().Info.IsStill)

	key, err := stillKey(srcs, ops, subset, t, maxW)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	memoPath := filepath.Join(m.st.Scratch, stillsDir, key+".png")
	if data := readMemo(memoPath); data != nil {
		return data, nil
	}

	if m.tools.FFmpeg == "" {
		return nil, errors.New("ffmpeg is not available on this server")
	}
	return m.previews.do(ctx, key, func(ctx context.Context) ([]byte, error) {
		if data := readMemo(memoPath); data != nil {
			return data, nil // a previous leader finished while we waited
		}
		release, err := m.acquirePreview(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		if len(plan.TextFiles) > 0 {
			dir, cleanup, err := m.st.ScratchDir("still-" + store.RandomID(8))
			if err != nil {
				return nil, err
			}
			defer cleanup()
			if plan, err = bindTextFiles(plan, dir); err != nil {
				return nil, err
			}
		}
		srcPath := s.main().Path
		png, err := m.renderStill(ctx, enc.StillArgs(srcPath, plan, t, maxW))
		if err != nil {
			return nil, err
		}
		if len(png) == 0 {
			// The seek-back missed every frame (a long held last frame, see
			// enc.StillArgs): decode from the trim start instead.
			if png, err = m.renderStill(ctx, enc.StillArgsFromStart(srcPath, plan, t, maxW)); err != nil {
				return nil, err
			}
		}
		if len(png) == 0 {
			return nil, errors.New("still render produced no image (time outside the clip?)")
		}
		m.memoWrite(memoPath, png, MaxStills, m.opts.MaxStillsBytes)
		return png, nil
	})
}

// renderStill runs one still argv (ffmpegPrefix + args) and returns the
// PNG bytes ffmpeg wrote to stdout.
func (m *Manager) renderStill(ctx context.Context, args []string) ([]byte, error) {
	argv := append(append(make([]string, 0, len(ffmpegPrefix)+len(args)), ffmpegPrefix...), args...)
	png, err := ffrun.RunOutput(ctx, m.tools.FFmpeg, argv)
	if err != nil {
		return nil, fmt.Errorf("still render: %w", err)
	}
	return png, nil
}

// stillOutput keeps only the fields of out that change preview geometry.
func stillOutput(out recipe.Output) recipe.Output {
	return recipe.Output{
		Format: out.Format,
		Width:  out.Width,
		Height: out.Height,
		Fit:    out.Fit,
		FPS:    out.FPS,
	}
}

// clampStillTime bounds t to [0, (Frames-0.5)/FPS] — the midpoint of the
// plan's last frame — when the frame count is known (every later t shows
// that same frame, so they collapse onto one memo entry and none can land
// past the render's last slot), else to [0, plan.Duration] when only the
// duration is, and rounds it to milliseconds so scrub positions collapse
// onto memo keys (half a millisecond never crosses a frame boundary from a
// midpoint: frames are >= 16.7 ms at the 60 fps cap). A still source (or a
// plan with a single frame) has its only frame at 0, so every t maps there:
// the plan's duration is 0 for stills and would not clamp, and a seek past
// the one frame would yield no image.
func clampStillTime(plan *graph.Plan, t float64, still bool) float64 {
	if still || plan.Frames == 1 {
		return 0
	}
	if t < 0 || t != t { // negative or NaN
		t = 0
	}
	switch {
	case plan.Frames > 1 && plan.FPS > 0:
		if last := (float64(plan.Frames) - 0.5) / plan.FPS; t > last {
			t = last
		}
	case plan.Duration > 0 && t > plan.Duration:
		t = plan.Duration
	}
	return float64(int64(t*1000+0.5)) / 1000
}

// stillKey hashes (stillMemoVersion, every source, canonical ops, geometry
// output, t, maxW).
func stillKey(srcs []string, ops []recipe.Op, out recipe.Output, t float64, maxW int) (string, error) {
	return stillKeyV(srcs, ops, out, t, maxW, stillMemoVersion)
}

// stillKeyV is stillKey with an explicit version salt.
func stillKeyV(srcs []string, ops []recipe.Op, out recipe.Output, t float64, maxW int, version string) (string, error) {
	canon, err := recipe.Recipe{Sources: srcs, Ops: ops, Output: out}.Canonical()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("still|" + version + "\n"))
	h.Write(canon)
	h.Write([]byte("|t=" + strconv.FormatFloat(t, 'f', 3, 64) + "|w=" + strconv.Itoa(maxW)))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readMemo returns a memoised preview file (nil when absent or empty) and
// touches it so eviction keeps recently used entries (best effort).
func readMemo(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return data
}

// memoWrite stores data at path (atomically), first evicting the oldest
// entries of its directory so that the write leaves at most keep entries
// holding at most keepBytes (0 = no byte bound) — the memo dirs live on the
// scratch tmpfs, whose space the renders are budgeted against, so an entry
// is never written beyond the bound and removed afterwards. A write that
// still hits ENOSPC (a foreign tenant of the tmpfs, a render of unknown
// length) empties the directory and retries once: the memo is a cache.
// Failures are logged, never fatal.
func (m *Manager) memoWrite(path string, data []byte, keep int, keepBytes int64) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("jobs: memo dir %s: %v", filepath.Base(dir), err)
		return
	}
	m.stillMu.Lock()
	defer m.stillMu.Unlock()
	keepCount, room := max(keep-1, 0), int64(0)
	if keepBytes > 0 {
		room = keepBytes - int64(len(data))
		if room <= 0 {
			keepCount, room = 0, 0 // larger than the whole bound: it lands alone
		}
	}
	if err := evictOldest(dir, keepCount, room); err != nil {
		log.Printf("jobs: memo evict %s: %v", filepath.Base(dir), err)
	}
	err := writeMemoFile(dir, path, data)
	if err != nil && isNoSpace(err) {
		if evErr := evictOldest(dir, 0, 0); evErr != nil {
			log.Printf("jobs: memo evict %s: %v", filepath.Base(dir), evErr)
		}
		err = writeMemoFile(dir, path, data)
	}
	if err != nil {
		log.Printf("jobs: memo %s: %v", filepath.Base(dir), err)
	}
}

// writeMemoFile writes data to path through a temp file in dir and a rename.
func writeMemoFile(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".memo-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// evictOldest removes the oldest regular files in dir until at most keep
// remain (< 0 = no count bound; 0 = remove them all) holding at most
// keepBytes in total (<= 0 = no byte bound); temp files being written
// (dot-names) are skipped.
func evictOldest(dir string, keep int, keepBytes int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type ent struct {
		name  string
		size  int64
		mtime time.Time
	}
	files := make([]ent, 0, len(entries))
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name()[0] == '.' {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, ent{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	within := func(n int, total int64) bool {
		return (keep < 0 || n <= keep) && (keepBytes <= 0 || total <= keepBytes)
	}
	if within(len(files), total) {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	var errs []error
	remaining := len(files)
	for _, f := range files {
		if within(remaining, total) {
			break
		}
		if err := os.Remove(filepath.Join(dir, f.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		remaining--
		total -= f.size
	}
	return errors.Join(errs...)
}
