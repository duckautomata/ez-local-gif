package store

// Phase 5b: the AI matte memo on /data (docs/background-removal-proposal.md
// §4.4). Layout under <Root>/mattes:
//
//	<Root>/mattes/models.json                  the app's copy of the last sidecar ping (matte.SaveFacts; not the sweeper's)
//	<Root>/mattes/<clipKey>/                   one clip's matte sequence: %06d.png + matte.json (written by jobs)
//	<Root>/mattes/.tmp-<clipKey>-<rand>/       a pass in progress (renamed into place on success; junk after inProgressGrace)
//	<Root>/mattes/frames/<model>/<size>/<prec>/<weights8>-<proc>/<frameSHA>.png
//	                                           the frames store: one matte per distinct model input, shared by every clip
//
// The clip dirs are copies of frames-store files (not hard links), so the
// two are independently sweepable. Sweep's mattes class (listMattes +
// sweepMattes, called from Sweep):
//
//   - a clip dir is aged by its own mtime (the newer of the dir's and its
//     manifest's — TouchMatte refreshes both on every memo hit) under the
//     same TTL as results and blobs, and is removed whatever the TTL says
//     when its source blob (the manifest's src) no longer exists; a clip dir
//     without a readable manifest follows the results rule (in progress
//     while younger than inProgressGrace, junk after);
//   - a .tmp-* dir is in progress while its newest entry is younger than
//     inProgressGrace (a running pass keeps adding PNGs) and junk after: a
//     pass killed by a shutdown or SIGKILL leaves one;
//   - a frames-store file is aged by its own mtime (TouchMatteFrame refreshes
//     it on every hit); a dot-prefixed file there is the temp of an
//     interrupted write and junk after inProgressGrace; an empty frames dir
//     older than the grace is pruned;
//   - the size pass evicts mattes last (results, then blobs, then mattes,
//     oldest first): a matte is kilobytes to a few MB and the most expensive
//     thing on the disk to regenerate.
//
// Protect is what keeps a dir a render or preview is reading out of the
// sweeper's hands meanwhile: every delete path of Sweep — results and blobs
// included — skips a protected path or anything under one.

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
	"sync"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/matte"
)

const (
	// mattesDir is the matte memo root under Root.
	mattesDir = "mattes"
	// matteFramesDir is the frames store under mattesDir.
	matteFramesDir = "frames"
	// matteTmpPrefix names an in-progress clip dir under mattesDir
	// (".tmp-<key>-<rand>"), so the sweeper can tell it from a memo.
	matteTmpPrefix = ".tmp-"
	// matteTmpRandBytes is the random suffix length of MatteTmpDir (hex).
	matteTmpRandBytes = 6
)

// MatteDir returns <Root>/mattes/<key> (not created): the memo dir of one
// clip's matte sequence, key being the clip key (matte.ClipKey, a sha256
// hex). The key is sanitised like a scratch id, so a malformed one maps to
// a name that cannot escape the mattes dir.
func (s *Store) MatteDir(key string) string {
	return filepath.Join(s.Root, mattesDir, matteName(key))
}

// MatteFrameDir returns <Root>/mattes/frames/<model>/<size>/<prec>/
// <weights8>-<proc> (not created): the frames-store directory of the mattes
// one (model, input size, precision, weights, processing version) produces;
// weights8 is the first 8 hex characters of the pinned weights' sha256 and
// proc the sidecar's processingVersion. Each component is sanitised.
func (s *Store) MatteFrameDir(model string, size int, prec, weights8, proc string) string {
	return filepath.Join(s.Root, mattesDir, matteFramesDir,
		matteName(model), strconv.Itoa(size), matteName(prec), matteName(weights8)+"-"+matteName(proc))
}

// MatteTmpDir returns a fresh <Root>/mattes/.tmp-<key>-<rand> path (not
// created) for a matte pass to fill before it renames the dir to
// MatteDir(key); every call yields a new random suffix. A tmp dir older than
// inProgressGrace is junk to the sweeper (a pass killed by a shutdown leaves
// one).
func (s *Store) MatteTmpDir(key string) string {
	return filepath.Join(s.Root, mattesDir, matteTmpPrefix+matteName(key)+"-"+RandomID(matteTmpRandBytes))
}

// matteName sanitises one path component of the matte layout; an empty
// result maps to "invalid" so it can never collide with a real entry.
func matteName(s string) string {
	if safe := sanitizeID(s); safe != "" {
		return safe
	}
	return "invalid"
}

// TouchMatte marks the memoised clip sequence for key as in use right now by
// setting the mtime of MatteDir(key) and of its manifest to the current
// time, so Sweep's TTL and size passes count from the last use rather than
// the pass that made it. Callers use it on every memo hit (a still, proxy or
// render served from the memo) and whenever a render touches the clip's
// source blob. A missing memo is not an error (it may have been swept
// meanwhile; the caller's own manifest read reports that).
func (s *Store) TouchMatte(key string) error {
	dir := s.MatteDir(key)
	now := time.Now()
	var errs []error
	for _, p := range []string{dir, filepath.Join(dir, matte.ManifestName)} {
		if err := os.Chtimes(p, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("store: touch matte %s: %w", matteName(key), err)
	}
	return nil
}

// TouchMatteFrame marks one frames-store file (a PNG under a MatteFrameDir)
// as used right now, so the sweeper ages it from the last hit rather than
// the pass that filed it. Callers use it on every frames-store hit. A
// missing file is not an error.
func (s *Store) TouchMatteFrame(path string) error {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: touch matte frame %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Protect marks dir (an absolute path under Root, e.g. a MatteDir) as in use:
// the sweeper never deletes a protected dir or anything under it until the
// returned release func has been called as many times as Protect was for
// that dir (a render protects its matte dir from the master render to the
// end of the job, a preview for the duration of its ffmpeg run). release is
// idempotent. The set is in-memory only — a restart clears it, which is
// fine: nothing is reading then.
//
// Protect before reading what the dir holds (the manifest, a result's
// files): the sweeper checks the set right before each delete, so a dir
// protected first is never removed, while one protected after the check may
// already be gone — the caller's read then reports a plain miss.
func (s *Store) Protect(dir string) (release func()) {
	dir = filepath.Clean(dir)
	s.protectMu.Lock()
	if s.protected == nil {
		s.protected = map[string]int{}
	}
	s.protected[dir]++
	s.protectMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.protectMu.Lock()
			defer s.protectMu.Unlock()
			if n := s.protected[dir]; n <= 1 {
				delete(s.protected, dir)
			} else {
				s.protected[dir] = n - 1
			}
		})
	}
}

// isProtected reports whether path, or any directory above it, is currently
// protected (Protect). Every delete path of Sweep consults it right before
// removing something. Both sides are cleaned absolute paths, so a plain
// prefix test (with the separator) is the containment check.
func (s *Store) isProtected(path string) bool {
	path = filepath.Clean(path)
	s.protectMu.Lock()
	defer s.protectMu.Unlock()
	for p := range s.protected {
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// anyProtected reports whether any of paths is protected (a blob entry lists
// its payload and meta file).
func (s *Store) anyProtected(paths []string) bool {
	for _, p := range paths {
		if s.isProtected(p) {
			return true
		}
	}
	return false
}

// matteKind classifies one entry of the mattes class.
type matteKind uint8

const (
	matteClip  matteKind = iota // <mattes>/<key>/: a memoised clip sequence
	matteTmp                    // <mattes>/.tmp-*: a pass in progress, or abandoned
	matteFrame                  // <mattes>/frames/…/<sha>.png: one frames-store file (or a temp beside one)
)

// matteEntry is one sweepable item of the mattes class.
type matteEntry struct {
	kind       matteKind
	path       string
	mtime      time.Time
	size       int64
	src        string // clip: the manifest's source blob hash ("" when unreadable)
	inProgress bool   // younger than inProgressGrace: a tmp dir, a clip without a readable manifest, a frames temp
	junk       bool   // the same kinds, older than inProgressGrace: removed whatever the TTL says
}

// listMattes lists the mattes class: every clip dir and tmp dir directly
// under <Root>/mattes and every file of the frames store. A missing mattes
// dir (a plain install that never ran a pass) lists as nothing. Regular
// files directly under mattes — models.json and its write temps — are not
// the sweeper's and are skipped, as is any other dot-dir.
func (s *Store) listMattes(now time.Time) ([]matteEntry, error) {
	root := filepath.Join(s.Root, mattesDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: sweep mattes: %w", err)
	}
	var out []matteEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dir := filepath.Join(root, name)
		switch {
		case name == matteFramesDir:
			out = append(out, s.listMatteFrames(dir, now)...)
			continue
		case strings.HasPrefix(name, "."):
			if !strings.HasPrefix(name, matteTmpPrefix) {
				continue
			}
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		m := matteEntry{path: dir}
		if strings.HasPrefix(name, matteTmpPrefix) {
			m.kind = matteTmp
			// A running pass keeps adding PNGs: age by the newest entry.
			m.mtime = dirNewestMtime(dir, info.ModTime())
			m.flagProgress(now)
		} else {
			m.kind = matteClip
			manifestPath := filepath.Join(dir, matte.ManifestName)
			if man, err := matte.ReadManifest(manifestPath); err == nil {
				m.src = man.Src
				m.mtime = info.ModTime()
				if mst, err := os.Stat(manifestPath); err == nil && mst.ModTime().After(m.mtime) {
					m.mtime = mst.ModTime()
				}
			} else {
				// Never a memo (jobs renames a complete dir into place and
				// drops one whose manifest it cannot read): the results
				// rule for a manifest-less dir.
				m.mtime = dirNewestMtime(dir, info.ModTime())
				m.flagProgress(now)
			}
		}
		m.size = dirSize(dir)
		out = append(out, m)
	}
	return out, nil
}

// flagProgress sets inProgress or junk from the entry's age against
// inProgressGrace.
func (m *matteEntry) flagProgress(now time.Time) {
	if now.Sub(m.mtime) < inProgressGrace {
		m.inProgress = true
	} else {
		m.junk = true
	}
}

// listMatteFrames lists every regular file of the frames store, aged by its
// own mtime. A dot-prefixed file is the temp of an interrupted tmp+rename
// write and follows the in-progress/junk rule instead.
func (s *Store) listMatteFrames(root string, now time.Time) []matteEntry {
	var out []matteEntry
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		m := matteEntry{kind: matteFrame, path: path, mtime: info.ModTime(), size: info.Size()}
		if strings.HasPrefix(d.Name(), ".") {
			m.flagProgress(now)
		}
		out = append(out, m)
		return nil
	})
	return out
}

// sweepMattesAge is the age pass of the mattes class: junk goes whatever
// the TTL says, in-progress entries stay, the rest expire under the TTL, and
// a clip whose source blob is gone (checked now, after the blob age pass) is
// dead weight. Protected paths are kept (and still counted). It returns the
// kept entries.
func (s *Store) sweepMattesAge(ctx context.Context, now time.Time, ttl time.Duration, mattes []matteEntry, note func(error)) ([]matteEntry, error) {
	alive := map[string]bool{}
	var kept []matteEntry
	for _, m := range mattes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remove := false
		switch {
		case m.junk:
			remove = true
		case m.inProgress:
		case ttl > 0 && now.Sub(m.mtime) > ttl:
			remove = true
		case m.kind == matteClip && !s.blobAlive(m.src, alive):
			remove = true
		}
		if remove && !s.isProtected(m.path) {
			note(removeMatte(m))
			continue
		}
		kept = append(kept, m)
	}
	return kept, nil
}

// sweepMattesSize is the size pass of the mattes class, run after the
// results and blobs passes. Clips whose source blob the blob pass just
// evicted go first whatever the cap says (dead weight, as in the age pass);
// then, while still over the cap, the oldest sweepable entry goes. total is
// kept current.
func (s *Store) sweepMattesSize(ctx context.Context, mattes []matteEntry, total *int64, maxBytes int64, evictedBlobs map[string]bool, note func(error)) error {
	var live []matteEntry
	for _, m := range mattes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.kind == matteClip && evictedBlobs[m.src] && !s.isProtected(m.path) {
			note(removeMatte(m))
			*total -= m.size
			continue
		}
		live = append(live, m)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].mtime.Before(live[j].mtime) })
	for _, m := range live {
		if *total <= maxBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.inProgress || s.isProtected(m.path) {
			continue
		}
		note(removeMatte(m))
		*total -= m.size
	}
	return nil
}

// blobAlive reports whether the blob hash is still served by the store
// (GetBlob succeeds), memoising per sweep in cache: many clips share one
// source. An I/O error other than not-found counts as alive — nothing is
// deleted on a transient read failure.
func (s *Store) blobAlive(hash string, cache map[string]bool) bool {
	if v, ok := cache[hash]; ok {
		return v
	}
	_, err := s.GetBlob(hash)
	v := err == nil || !errors.Is(err, ErrNotFound)
	cache[hash] = v
	return v
}

// removeMatte deletes one entry (a dir tree or a single file).
func removeMatte(m matteEntry) error {
	if err := os.RemoveAll(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: sweep mattes: remove %s: %w", m.path, err)
	}
	return nil
}

// pruneMatteFrameDirs removes empty directories of the frames store that
// are older than inProgressGrace (a sidecar upgrade leaves the previous
// weights' tree behind once its files have aged out). A just-created empty
// dir is young — jobs' MkdirAll then tmp+rename race is left alone — and a
// dir emptied by this very sweep carries the removal's mtime, so it waits
// for a later sweep. Deepest first; protected dirs are kept.
func (s *Store) pruneMatteFrameDirs(ctx context.Context, now time.Time) error {
	root := filepath.Join(s.Root, mattesDir, matteFramesDir)
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == root {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil || len(dirs) == 0 {
		return nil
	}
	var errs []error
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := dirs[i]
		info, err := os.Stat(dir)
		if err != nil || now.Sub(info.ModTime()) < inProgressGrace || s.isProtected(dir) {
			continue
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) > 0 {
			continue
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("store: sweep mattes: prune %s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}
