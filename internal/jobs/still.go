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

// Still renders a single preview frame (PNG bytes) for the recipe's op stack
// at time t seconds, at most maxW pixels wide (0 = 480). Results are
// memoised in scratch keyed by (recipe hash sans output, t, maxW). Fast
// path (~100 ms) — used for scrubbing, crop and eyedropper.
//
// Only the geometry/timing part of out (Format, Width, Height, Fit, FPS) is
// used, so quality-knob changes never invalidate the memo. t is an output
// (preview) time; it is clamped to the plan's duration (to 0 for a still
// source, whose only frame is at 0 whatever t the scrubber sends) and
// enc.StillArgs maps it to source time.
//
// Rendering a still counts as using the source: the blob is touched so the
// store's sweeper measures its TTL from the last use.
func (m *Manager) Still(ctx context.Context, srcHash string, ops []recipe.Op, out recipe.Output, t float64, maxW int) ([]byte, error) {
	if maxW <= 0 {
		maxW = DefaultStillWidth
	}
	if !recipe.IsHash(srcHash) {
		return nil, fmt.Errorf("%w: %q is not a source hash", store.ErrNotFound, srcHash)
	}
	blob, err := m.st.GetBlob(srcHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: source %s", store.ErrNotFound, short(srcHash))
		}
		return nil, err
	}
	if blob.Info == nil {
		return nil, fmt.Errorf("%w: source %s has no probe info; upload it again", ErrInvalidRecipe, short(srcHash))
	}
	if err := m.st.TouchBlob(srcHash); err != nil {
		log.Printf("jobs: touch source %s: %v", short(srcHash), err)
	}
	subset := stillOutput(out)
	plan, err := graph.Compile(*blob.Info, ops, subset)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	t = clampStillTime(plan, t, blob.Info.IsStill)

	key, err := stillKey(srcHash, ops, subset, t, maxW)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	memoPath := filepath.Join(m.st.Scratch, stillsDir, key+".png")
	if data, err := os.ReadFile(memoPath); err == nil && len(data) > 0 {
		now := time.Now()
		_ = os.Chtimes(memoPath, now, now) // LRU touch (best effort)
		return data, nil
	}

	if m.tools.FFmpeg == "" {
		return nil, errors.New("ffmpeg is not available on this server")
	}
	args := append(append([]string{}, ffmpegPrefix...), enc.StillArgs(blob.Path, plan, t, maxW)...)
	png, err := ffrun.RunOutput(ctx, m.tools.FFmpeg, args)
	if err != nil {
		return nil, fmt.Errorf("still render: %w", err)
	}
	if len(png) == 0 {
		return nil, errors.New("still render produced no image (time outside the clip?)")
	}
	m.memoStill(memoPath, png)
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

// clampStillTime bounds t to [0, plan.Duration] (when the duration is known)
// and rounds it to milliseconds so scrub positions collapse onto memo keys.
// A still source (or a plan with a single frame) has its only frame at 0, so
// every t maps there: the plan's duration is 0 for stills and would not
// clamp, and a seek past the one frame would yield no image.
func clampStillTime(plan *graph.Plan, t float64, still bool) float64 {
	if still || plan.Frames == 1 {
		return 0
	}
	if t < 0 || t != t { // negative or NaN
		t = 0
	}
	if plan.Duration > 0 && t > plan.Duration {
		t = plan.Duration
	}
	return float64(int64(t*1000+0.5)) / 1000
}

// stillKey hashes (source, canonical ops, geometry output, t, maxW).
func stillKey(srcHash string, ops []recipe.Op, out recipe.Output, t float64, maxW int) (string, error) {
	canon, err := recipe.Recipe{Sources: []string{srcHash}, Ops: ops, Output: out}.Canonical()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(canon)
	h.Write([]byte("|t=" + strconv.FormatFloat(t, 'f', 3, 64) + "|w=" + strconv.Itoa(maxW)))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// memoStill stores png at path (atomically) and evicts the oldest entries
// beyond MaxStills. Failures are logged, never fatal.
func (m *Manager) memoStill(path string, png []byte) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("jobs: stills dir: %v", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".still-*")
	if err != nil {
		log.Printf("jobs: still memo: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(png); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		log.Printf("jobs: still memo write: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		log.Printf("jobs: still memo rename: %v", err)
		return
	}
	m.stillMu.Lock()
	defer m.stillMu.Unlock()
	if err := evictOldest(dir, MaxStills); err != nil {
		log.Printf("jobs: still memo evict: %v", err)
	}
}

// evictOldest removes the oldest regular files in dir until at most keep
// remain (temp files being written are skipped).
func evictOldest(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type ent struct {
		name  string
		mtime time.Time
	}
	files := make([]ent, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name()[0] == '.' {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, ent{e.Name(), info.ModTime()})
	}
	if len(files) <= keep {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	var errs []error
	for _, f := range files[:len(files)-keep] {
		if err := os.Remove(filepath.Join(dir, f.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
