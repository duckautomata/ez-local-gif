package store

// Phase 5c (build brief 2026-10-09): derived matte sequences under a clip
// dir and the guided mode's prompt-mask memo on scratch.
//
//	<Root>/mattes/<clipKey>/stab-<mode>/%06d.png        the clip's stabilised sequence (enc.MatteStabiliseArgs; jobs derives it)
//	<Root>/mattes/<clipKey>/gated-<edge>-<edgeKey>-r<r>/%06d.png  a tracker matte gated with the edge model (enc.MatteGateArgs; edgeKey = the edge memo's clip key, shortened)
//	<Root>/mattes/<clipKey>/<name>.tmp/                 a derive in progress (jobs renames it to <name> on success)
//	<Scratch>/matte-prompts/<key>.png                   the live prompt masks of the guided mode (jobs' memoWrite)
//
// A derived dir is part of its clip dir: it is counted in the clip's size,
// removed with the clip (TTL, dead source, size pass) and covered by a
// Protect of the clip dir — and a Protect of the derived dir alone keeps
// the clip too (isProtected looks both ways). The only rule of its own is
// for the ".tmp" sibling a derive writes into before the rename: it is in
// progress while its newest entry is younger than inProgressGrace — and so
// is the clip, which neither pass evicts meanwhile — and junk after (a
// derive killed by a shutdown leaves one), removed on its own without
// touching the clip. The derived dirs are never touched or aged
// separately: the clip is the unit (TouchMatte).
//
// The prompt-mask memo lives on scratch like the still memo and is bounded
// the same way: jobs' memoWrite evicts at write time with
// MattePromptsMaxEntries / MattePromptsMaxBytes, and Sweep applies the same
// bound on every run (SweepMattePrompts), oldest first by mtime (a hit
// touches the file), skipping protected files and the dot-named temps of a
// write in progress (junk after inProgressGrace).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MatteStabPrefix names a stabilised derived dir: "stab-<mode>".
	MatteStabPrefix = "stab-"
	// MatteGatedPrefix names a gated derived dir: "gated-<edge>-<edgeKey>-r<radius>".
	MatteGatedPrefix = "gated-"
	// MatteDerivedTmpSuffix is appended to a derived dir's name while a
	// derive writes into it (MatteDerivedTmpDir); the sweeper recognises an
	// abandoned one by it.
	MatteDerivedTmpSuffix = ".tmp"

	// MattePromptsDir is the prompt-mask memo directory name under Scratch
	// (MattePromptsMemoDir).
	MattePromptsDir = "matte-prompts"
	// MattePromptsMaxEntries bounds the prompt-mask memo's file count: a
	// session of clicking around a clip is a few dozen masks.
	MattePromptsMaxEntries = 256
	// MattePromptsMaxBytes bounds the prompt-mask memo in bytes: a binary
	// gray PNG at the tracking size (long side <= 1024) is a few KB to ~100
	// KB, so the count bound is what normally bites.
	MattePromptsMaxBytes = 32 << 20
)

// MatteStabName returns the name of the stabilised derived dir for mode
// ("light" / "strong"): "stab-<mode>", the component sanitised like every
// matte path element ("stab-invalid" for an empty mode).
func MatteStabName(mode string) string {
	return MatteStabPrefix + matteName(mode)
}

// MatteGatedKeyChars is how much of the edge memo's clip key MatteGatedName
// keeps: 12 hex characters (48 bits) tell apart every identity one clip
// can be gated with.
const MatteGatedKeyChars = 12

// MatteGatedName returns the name of the gated derived dir of a tracker
// matte: "gated-<edge>-<edgeKey>-r<radius>", edge being the RESOLVED edge
// model id (never the recipe's "" that means the device's default — that
// would alias two models under "gated-invalid-…"), edgeKey the edge memo's
// own clip key (its first MatteGatedKeyChars; "invalid" when empty) and
// radius the gate's band in tracking pixels. The key is part of the name
// because the tracker's clip key deliberately leaves the edge model out:
// an edge memo re-keyed by a sidecar upgrade (new weights or processing
// version) or by the device preference (another size / precision) must
// derive a NEW gate, never serve the one made from the old edge memo.
func MatteGatedName(edge, edgeKey string, radius int) string {
	if len(edgeKey) > MatteGatedKeyChars {
		edgeKey = edgeKey[:MatteGatedKeyChars]
	}
	return MatteGatedPrefix + matteName(edge) + "-" + matteName(edgeKey) + "-r" + strconv.Itoa(radius)
}

// MatteDerivedDir returns <clipDir>/<name>: the derived sequence of a clip
// memo (clipDir from MatteDir), name from MatteStabName / MatteGatedName.
// The name is sanitised, so it can never leave the clip dir.
func MatteDerivedDir(clipDir, name string) string {
	return filepath.Join(clipDir, matteName(name))
}

// MatteDerivedTmpDir returns <clipDir>/<name>.tmp: where a derive writes
// its frames before renaming the dir to MatteDerivedDir(clipDir, name). A
// derive runs under the clip dir's Protect; one abandoned by a crash is
// junk to the sweeper after inProgressGrace.
func MatteDerivedTmpDir(clipDir, name string) string {
	return MatteDerivedDir(clipDir, name) + MatteDerivedTmpSuffix
}

// IsMatteDerivedTmp reports whether name (one entry of a clip dir) is a
// derive's temp dir ("<name>.tmp" with a non-empty name).
func IsMatteDerivedTmp(name string) bool {
	return len(name) > len(MatteDerivedTmpSuffix) && strings.HasSuffix(name, MatteDerivedTmpSuffix)
}

// MattePromptsMemoDir returns <Scratch>/matte-prompts (not created): the
// prompt-mask memo directory jobs writes with memoWrite(path, png,
// MattePromptsMaxEntries, MattePromptsMaxBytes) and Sweep bounds the same
// way.
func (s *Store) MattePromptsMemoDir() string {
	return filepath.Join(s.Scratch, MattePromptsDir)
}

// listMatteDerivedTmps lists the derive temps directly under a clip dir
// (IsMatteDerivedTmp), each aged by its newest entry (a derive keeps adding
// PNGs) and flagged in progress or junk; tmpBytes is their total size, which
// the caller takes off the clip's own, and live reports whether any of them
// is still in progress (the clip is then in progress too).
func (s *Store) listMatteDerivedTmps(clipDir string, now time.Time) (out []matteEntry, tmpBytes int64, live bool) {
	entries, err := os.ReadDir(clipDir)
	if err != nil {
		return nil, 0, false
	}
	for _, e := range entries {
		if !e.IsDir() || !IsMatteDerivedTmp(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		dir := filepath.Join(clipDir, e.Name())
		m := matteEntry{kind: matteDerivedTmp, path: dir, size: dirSize(dir)}
		m.mtime = dirNewestMtime(dir, info.ModTime())
		m.flagProgress(now)
		tmpBytes += m.size
		live = live || m.inProgress
		out = append(out, m)
	}
	return out, tmpBytes, live
}

// SweepMattePrompts bounds the prompt-mask memo (MattePromptsMemoDir) to
// MattePromptsMaxEntries files holding at most MattePromptsMaxBytes, oldest
// by mtime first, and removes the dot-named temps of writes abandoned longer
// than inProgressGrace ago. Protected files are kept (and still counted). A
// missing memo dir is nothing to do. Sweep calls it on every run; jobs may
// call it after a write of its own.
func (s *Store) SweepMattePrompts(ctx context.Context) error {
	return s.sweepMattePrompts(ctx, time.Now(), MattePromptsMaxEntries, MattePromptsMaxBytes)
}

// sweepMattePrompts is SweepMattePrompts with explicit bounds (maxEntries <
// 0 = no count bound, maxBytes <= 0 = no byte bound).
func (s *Store) sweepMattePrompts(ctx context.Context, now time.Time, maxEntries int, maxBytes int64) error {
	dir := s.MattePromptsMemoDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("store: sweep matte prompts: %w", err)
	}
	type ent struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []ent
	var total int64
	var errs []error
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("store: sweep matte prompts: remove %s: %w", filepath.Base(path), err))
		}
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), ".") {
			// A write in progress (jobs' tmp+rename), or one abandoned.
			if now.Sub(info.ModTime()) > inProgressGrace && !s.isProtected(path) {
				remove(path)
			}
			continue
		}
		files = append(files, ent{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	within := func(n int, total int64) bool {
		return (maxEntries < 0 || n <= maxEntries) && (maxBytes <= 0 || total <= maxBytes)
	}
	if !within(len(files), total) {
		sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
		remaining := len(files)
		for _, f := range files {
			if within(remaining, total) {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if s.isProtected(f.path) {
				continue
			}
			remove(f.path)
			remaining--
			total -= f.size
		}
	}
	return errors.Join(errs...)
}
