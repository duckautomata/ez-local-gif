// Package store owns the on-disk layout (DESIGN.md §3):
//
//	<Root>/blobs/<sha256>.<ext>        uploaded files, content-addressed
//	<Root>/blobs/<sha256>.seq/         uploaded image sequences (directory blobs, see sequence.go)
//	<Root>/blobs/<sha256>.json         Blob metadata (name, size, uploaded, probe info)
//	<Root>/results/<recipeHash>/       encoded outputs + manifest.json
//	<Root>/tmp/                        upload staging (same filesystem as blobs)
//	<Root>/scratch/                    scratch fallback when the tmpfs is too small (DESIGN.md §9.9)
//	<Scratch>/<id>/                    per-job scratch (tmpfs), removed when done
//
// The store has no database: the filesystem is the data model. A TTL/size
// sweeper is the only maintenance.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// ErrNotImplemented is kept for API compatibility with the Phase-1 stubs
// (cmd/ezlg still filters it); nothing in this package returns it any more.
var ErrNotImplemented = errors.New("store: not implemented")

// ErrNotFound is returned when a blob or result does not exist.
var ErrNotFound = errors.New("store: not found")

// ManifestName is the file that marks a result dir as complete.
const ManifestName = "manifest.json"

// InfoVersion is stamped on a blob's meta file whenever SetBlobInfo stores
// probe info. GetBlob (and the PutBlob dedupe path) hide info stored under
// an older version — Blob.Info is nil, exactly as for a never-probed blob —
// so the upload path re-probes it and every "no probe info; upload it again"
// message stays true. Bump it whenever probe semantics change such that
// persisted ProbeInfo would be wrong:
//
//	0/absent: Phase 1 as first shipped
//	2:        Width/Height are the displayed (autorotated) size; animation
//	          FPS prefers r_frame_rate
//	3:        any source with an established frame count of 1 (a one-frame
//	          ProRes/H.264 MOV) is a still (IsStill, Kind image, FPS 0), not
//	          a video that the fps filter would render to nothing
//	4:        AVIF is described by its animation track (not the one-frame
//	          primary item libavif writes first) and carries AlphaStream;
//	          image sequences (Kind sequence, Info.Sequence) are new
//	5:        sequence HasAlpha considers GIF frames and colour-keyed
//	          (RGB/gray + tRNS) PNG frames whose stdlib colour model looks
//	          opaque; monochrome (yuv400/gray) AVIF animations are described
//	          by their track, not the one-frame primary item; sequences
//	          whose frames ffmpeg cannot read via the image2 pattern are
//	          rejected at probe time
//	6:        sequence FPS is graph.SequenceFPS(delay) (3 decimals, exactly
//	          what -framerate gets) and Duration = count/FPS, so probe facts
//	          match graph.Plan bit-for-bit (user Phase 2 review, bug 4)
const InfoVersion = 6

// MinScratchBytes is the smallest scratch filesystem New accepts quietly.
// Docker's default /dev/shm is 64 MiB, which holds about 160 frames of
// 320x320 RGBA: below this New logs loudly and, when <Root>/scratch sits on
// a larger filesystem, uses that instead (DESIGN.md §9.9).
const MinScratchBytes = 256 << 20

const (
	blobsDir   = "blobs"
	resultsDir = "results"
	tmpDir     = "tmp"
	scratchDir = "scratch"

	// maxExtLen bounds a sanitised file extension.
	maxExtLen = 8
	// maxNameLen bounds a sanitised original file name.
	maxNameLen = 200
	// defaultExt is used when the client file name has no usable extension.
	defaultExt = "bin"
	// metaExt is the extension of the blob meta file (<hash>.json). An
	// uploaded file whose own extension is "json" must never be stored under
	// it, or the payload and the meta would share one path.
	metaExt = "json"
	// jsonBlobExt is what SanitizeExt returns for an uploaded .json file.
	jsonBlobExt = "jsonfile"
	// seqBlobExt is what SanitizeExt returns for an uploaded .seq file. Ext
	// "seq" (SeqExt, sequence.go) means "Path is the frame directory"; a
	// plain file blob must never carry it, or Blob.IsSequence would lie.
	seqBlobExt = "seqfile"
	// defaultName is used when the client file name is empty after sanitising.
	defaultName = "upload"

	// inProgressGrace is how long a result dir without a manifest (or a
	// staging file under tmp) is assumed to be in use before Sweep removes it.
	inProgressGrace = time.Hour
	// blobGrace protects freshly uploaded blobs from the size-cap pass, and
	// blobs whose meta was written (upload / probe info stored) less than
	// this long ago from the TTL pass as well.
	blobGrace = time.Hour
)

// Blob is an uploaded file.
type Blob struct {
	Hash     string            `json:"hash"`
	Name     string            `json:"name"` // original file name (sanitised)
	Ext      string            `json:"ext"`  // lowercase extension without dot, e.g. "mov"
	Size     int64             `json:"size"`
	Uploaded time.Time         `json:"uploaded"`
	Info     *recipe.ProbeInfo `json:"info,omitempty"`
	// InfoVersion is the InfoVersion under which Info was stored (0 for
	// meta files that predate the field). Readers only ever see Info when it
	// matches the current InfoVersion.
	InfoVersion int    `json:"infoVersion,omitempty"`
	Path        string `json:"-"` // absolute path of the blob file (the frame directory for Ext "seq")
}

// Store is safe for concurrent use.
type Store struct {
	Root    string // e.g. /data
	Scratch string // e.g. /dev/shm/ezl (falls back to os.TempDir()/ezl if not writable, <Root>/scratch if the tmpfs is tiny)

	// metaMu serialises read-modify-write cycles on blob meta files so that
	// a concurrent duplicate upload cannot clobber freshly stored probe info.
	metaMu sync.Mutex
}

// New creates the directory layout and returns a Store. When scratch cannot
// be created or written to, it falls back to os.TempDir()/ezl and logs the
// fallback. When scratch is usable but sits on a filesystem smaller than
// MinScratchBytes (a Docker default 64 MiB /dev/shm), New logs loudly and
// prefers <root>/scratch if that is on a larger filesystem.
func New(root, scratch string) (*Store, error) {
	if root == "" {
		return nil, errors.New("store: empty root")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("store: resolve root %q: %w", root, err)
	}
	for _, d := range []string{blobsDir, resultsDir, tmpDir} {
		if err := os.MkdirAll(filepath.Join(absRoot, d), 0o755); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", d, err)
		}
	}
	// Prove every data directory is writable by this process now, not at
	// the first upload: a /data volume that was populated by a different uid
	// (e.g. the root dev stack, or a bind mount created by root) is the
	// classic way to get a confusing 500 later.
	for _, d := range []string{blobsDir, resultsDir, tmpDir} {
		if err := probeWritable(filepath.Join(absRoot, d)); err != nil {
			return nil, fmt.Errorf("store: %s is not writable by uid %d (%v); fix ownership, e.g. "+
				"docker compose run --rm --user root --entrypoint chown app -R ezlg:ezlg /data",
				filepath.Join(absRoot, d), os.Getuid(), err)
		}
	}

	scratchPath, err := prepareScratch(scratch)
	if err != nil {
		fallback := filepath.Join(os.TempDir(), "ezl")
		log.Printf("store: scratch %q unusable (%v); falling back to %s", scratch, err, fallback)
		scratchPath, err = prepareScratch(fallback)
		if err != nil {
			return nil, fmt.Errorf("store: scratch fallback %q unusable: %w", fallback, err)
		}
	}
	scratchPath = chooseScratch(absRoot, scratchPath)
	return &Store{Root: absRoot, Scratch: scratchPath}, nil
}

// chooseScratch keeps scratch unless its filesystem is smaller than
// MinScratchBytes; then it warns and switches to <root>/scratch when that is
// on a larger filesystem (disk-backed and slower, but a 64 MiB tmpfs fails
// almost every render with ENOSPC).
func chooseScratch(root, scratch string) string {
	_, total, ok := fsSpace(scratch)
	if !ok || !scratchTooSmall(total) {
		return scratch
	}
	log.Printf("store: WARNING scratch %s is on a %s filesystem (Docker's default /dev/shm is 64 MiB); "+
		"set shm_size: 4g in compose.yaml or point EZLG_SCRATCH at a larger tmpfs — see README", scratch, humanBytes(total))
	alt, err := prepareScratch(filepath.Join(root, scratchDir))
	if err != nil {
		log.Printf("store: %s/%s is not usable as scratch (%v); keeping %s", root, scratchDir, err, scratch)
		return scratch
	}
	if _, altTotal, ok := fsSpace(alt); ok && altTotal <= total {
		log.Printf("store: %s is not larger (%s); keeping %s", alt, humanBytes(altTotal), scratch)
		return scratch
	}
	log.Printf("store: using disk-backed %s for job scratch instead (slower than tmpfs)", alt)
	return alt
}

// scratchTooSmall reports whether a scratch filesystem of total bytes is
// below MinScratchBytes.
func scratchTooSmall(total int64) bool { return total < MinScratchBytes }

// ScratchFree returns the bytes currently free on the scratch filesystem;
// ok is false when the platform cannot tell.
func (s *Store) ScratchFree() (free int64, ok bool) {
	free, _, ok = fsSpace(s.Scratch)
	return free, ok
}

// ScratchTotal returns the size of the scratch filesystem (the tmpfs
// shm_size when scratch is /dev/shm); ok is false when the platform cannot
// tell.
func (s *Store) ScratchTotal() (total int64, ok bool) {
	_, total, ok = fsSpace(s.Scratch)
	return total, ok
}

// humanBytes renders n as "64 MiB" / "3.9 GiB" / "12 KiB" for log lines.
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

// prepareScratch creates dir and proves it is writable by creating and
// removing a probe file. It returns the absolute path.
func prepareScratch(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	if err := probeWritable(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// probeWritable creates, writes and removes a temp file in dir.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.WriteString("ok"); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// PutBlob streams r to a temp file while hashing, then renames it to
// blobs/<sha256>.<ext> (idempotent: an existing blob is kept and its meta
// returned). name is the client file name; its extension is used.
func (s *Store) PutBlob(r io.Reader, name string) (*Blob, error) {
	ext := SanitizeExt(name)
	cleanName := SanitizeName(name)

	tmp, err := os.CreateTemp(filepath.Join(s.Root, tmpDir), "upload-*.part")
	if err != nil {
		return nil, fmt.Errorf("store: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// On every early return the partial file must go away.
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("store: write upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("store: close upload: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	// Idempotent path: the blob is already known (possibly with probe info).
	// Touch it so a re-upload extends its TTL.
	if existing, err := s.getBlobLocked(hash); err == nil {
		os.Remove(tmpPath)
		now := time.Now()
		_ = os.Chtimes(existing.Path, now, now)
		return existing, nil
	}

	final := s.blobPath(hash, ext)
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("store: place blob: %w", err)
	}
	b := &Blob{
		Hash:     hash,
		Name:     cleanName,
		Ext:      ext,
		Size:     size,
		Uploaded: time.Now().UTC(),
		Path:     final,
	}
	if err := s.writeMetaLocked(b); err != nil {
		os.Remove(final)
		return nil, err
	}
	return b, nil
}

// GetBlob returns the blob for hash or ErrNotFound.
func (s *Store) GetBlob(hash string) (*Blob, error) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.getBlobLocked(hash)
}

// getBlobLocked reads the meta file and verifies the blob file exists.
func (s *Store) getBlobLocked(hash string) (*Blob, error) {
	if !recipe.IsHash(hash) {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(s.metaPath(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: read meta %s: %w", hash, err)
	}
	var b Blob
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("store: decode meta %s: %w", hash, err)
	}
	if b.Hash == "" {
		b.Hash = hash
	}
	// A meta that names the meta file's own extension as the blob's (a
	// legacy upload of a .json file, whose payload the meta write clobbered)
	// must not resolve to the meta file itself: map it like SanitizeExt does,
	// so the missing payload surfaces as ErrNotFound and gets re-uploaded.
	b.Ext = normalizeExt(b.Ext)
	b.Path = s.blobPath(hash, b.Ext)
	st, err := os.Stat(b.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: stat blob %s: %w", hash, err)
	}
	// Ext "seq" promises that Path is the frame directory (sequence.go). A
	// meta whose ext and payload shape disagree — a legacy plain-file upload
	// named "*.seq" from before SanitizeExt reserved the extension, or a
	// stray directory where a file should be — must not surface as the wrong
	// kind: report it missing so the upload path stores it again truthfully.
	if st.IsDir() != b.IsSequence() {
		return nil, ErrNotFound
	}
	if b.Info != nil && b.InfoVersion != InfoVersion {
		// Probed under older semantics (e.g. pre-autorotate dimensions):
		// present it as never probed so the caller re-probes.
		b.Info = nil
		b.InfoVersion = 0
	}
	return &b, nil
}

// SetBlobInfo stores probe info in the blob's meta file (stamped with the
// current InfoVersion).
func (s *Store) SetBlobInfo(hash string, info recipe.ProbeInfo) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	b, err := s.getBlobLocked(hash)
	if err != nil {
		return err
	}
	b.Info = &info
	b.InfoVersion = InfoVersion
	return s.writeMetaLocked(b)
}

// TouchBlob marks the blob as in use right now by setting the blob file's
// mtime to the current time, so Sweep's TTL and size passes count from the
// last use rather than the upload. Callers use it whenever a blob becomes a
// job source or a still is rendered from it. Unknown hashes are not an
// error (the blob may have been swept meanwhile; the caller's own lookup
// reports that).
func (s *Store) TouchBlob(hash string) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	b, err := s.getBlobLocked(hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	now := time.Now()
	if err := os.Chtimes(b.Path, now, now); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: touch blob %s: %w", hash, err)
	}
	return nil
}

// DeleteBlob removes a blob (file or sequence directory) and its meta file.
// Unknown hashes are not an error (idempotent).
func (s *Store) DeleteBlob(hash string) error {
	if !recipe.IsHash(hash) {
		return nil
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	var errs []error
	if b, err := s.getBlobLocked(hash); err == nil {
		if err := os.RemoveAll(b.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := os.Remove(s.metaPath(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("store: delete blob %s: %w", hash, err)
	}
	return nil
}

// writeMetaLocked writes the meta JSON atomically (temp file + rename).
func (s *Store) writeMetaLocked(b *Blob) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode meta %s: %w", b.Hash, err)
	}
	if err := writeFileAtomic(s.metaPath(b.Hash), data, filepath.Join(s.Root, tmpDir)); err != nil {
		return fmt.Errorf("store: write meta %s: %w", b.Hash, err)
	}
	return nil
}

// writeFileAtomic writes data to a temp file in tmpDir and renames it over
// dst. tmpDir must be on the same filesystem as dst.
func writeFileAtomic(dst string, data []byte, tmpDir string) error {
	f, err := os.CreateTemp(tmpDir, ".meta-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (s *Store) blobPath(hash, ext string) string {
	return filepath.Join(s.Root, blobsDir, hash+"."+ext)
}

func (s *Store) metaPath(hash string) string {
	return filepath.Join(s.Root, blobsDir, hash+".json")
}

// ResultDir returns <Root>/results/<recipeHash> (not created). Callers must
// pass a validated hash (recipe.IsHash); anything else maps to a name that
// can never collide with a real result.
func (s *Store) ResultDir(recipeHash string) string {
	if !recipe.IsHash(recipeHash) {
		recipeHash = "invalid"
	}
	return filepath.Join(s.Root, resultsDir, recipeHash)
}

// HasResult reports whether <ResultDir>/manifest.json exists.
func (s *Store) HasResult(recipeHash string) bool {
	if !recipe.IsHash(recipeHash) {
		return false
	}
	st, err := os.Stat(filepath.Join(s.ResultDir(recipeHash), ManifestName))
	return err == nil && st.Mode().IsRegular()
}

// CommitResult atomically moves the contents of stagingDir into
// ResultDir(recipeHash) (rename of the whole dir when possible), so a result
// is either complete or absent. If a result already exists (a concurrent
// render of the same recipe won), the existing one is kept and stagingDir is
// discarded. stagingDir is always consumed (removed) on success.
func (s *Store) CommitResult(recipeHash, stagingDir string) error {
	if !recipe.IsHash(recipeHash) {
		return fmt.Errorf("store: commit: %q is not a recipe hash", recipeHash)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, ManifestName)); err != nil {
		return fmt.Errorf("store: commit %s: staging has no %s: %w", recipeHash, ManifestName, err)
	}
	dst := s.ResultDir(recipeHash)
	if s.HasResult(recipeHash) {
		return os.RemoveAll(stagingDir)
	}

	// Fast path: rename the whole directory (same filesystem).
	if err := os.Rename(stagingDir, dst); err == nil {
		return nil
	} else if s.HasResult(recipeHash) {
		// Lost a race against another render of the same recipe.
		return os.RemoveAll(stagingDir)
	}

	// Slow path (scratch on tmpfs, results on /data): copy into a temp dir
	// on the results filesystem, then rename that into place.
	tmp, err := os.MkdirTemp(filepath.Join(s.Root, resultsDir), ".staging-"+recipeHash[:12]+"-*")
	if err != nil {
		return fmt.Errorf("store: commit %s: create staging: %w", recipeHash, err)
	}
	if err := copyTree(stagingDir, tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("store: commit %s: copy: %w", recipeHash, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.RemoveAll(tmp)
		if s.HasResult(recipeHash) {
			return os.RemoveAll(stagingDir)
		}
		return fmt.Errorf("store: commit %s: rename into place: %w", recipeHash, err)
	}
	return os.RemoveAll(stagingDir)
}

// copyTree copies the regular files (recursively) of src into dst, which
// must exist. Symlinks are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			return copyFile(path, target)
		default:
			return nil
		}
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ScratchDir creates <Scratch>/<id> and returns it with a cleanup func.
func (s *Store) ScratchDir(id string) (string, func(), error) {
	safe := sanitizeID(id)
	if safe == "" {
		return "", nil, fmt.Errorf("store: scratch id %q is empty after sanitising", id)
	}
	dir := filepath.Join(s.Scratch, safe)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("store: create scratch %s: %w", dir, err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("store: remove scratch %s: %v", dir, err)
		}
	}
	return dir, cleanup, nil
}

// Sweep deletes blobs and results older than ttl (ttl <= 0 disables the age
// pass), then oldest results — and, if still over, blobs older than an hour —
// until total size <= maxBytes (0 = no cap). It never deletes a result dir
// that is being written (no manifest yet and mtime < 1 h ago); abandoned
// upload temp files, manifest-less result dirs older than an hour, and blob
// payloads whose meta file is missing (unreachable orphans from a crash or
// partial delete) older than blobGrace are removed as junk whatever ttl says.
//
// A blob's age is the newer of its payload and meta mtimes: TouchBlob
// (render / still) refreshes the payload, PutBlob / SetBlobInfo the meta. A
// blob whose meta was written less than an hour ago (just uploaded or just
// probed, i.e. about to be used) is never deleted by either pass, whatever
// ttl says.
func (s *Store) Sweep(ctx context.Context, ttl time.Duration, maxBytes int64) error {
	now := time.Now()
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	note(s.sweepTmp(ctx, now))
	if err := ctx.Err(); err != nil {
		return err
	}

	results, err := s.listResults(now)
	if err != nil {
		return err
	}
	blobs, err := s.listBlobs()
	if err != nil {
		return err
	}

	// Age pass.
	var keptResults []resultEntry
	for _, r := range results {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.junk || (ttl > 0 && now.Sub(r.mtime) > ttl) {
			note(os.RemoveAll(r.path))
			continue
		}
		keptResults = append(keptResults, r)
	}
	var keptBlobs []blobEntry
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A payload without its meta file is unreachable (getBlobLocked
		// needs the meta) and, once past blobGrace (an in-flight upload
		// writes payload and meta back-to-back under metaMu), junk whatever
		// ttl says — left behind, it would also block PutSequence re-uploads
		// of the same frames until reclaimed.
		if b.metaMtime.IsZero() && now.Sub(b.mtime) > blobGrace {
			note(s.removeOrphanBlob(b))
			continue
		}
		if ttl > 0 && now.Sub(b.mtime) > ttl && !b.metaFresh(now) {
			note(s.removeBlobFiles(b))
			continue
		}
		keptBlobs = append(keptBlobs, b)
	}

	// Size pass.
	if maxBytes > 0 {
		total := int64(0)
		for _, r := range keptResults {
			total += r.size
		}
		for _, b := range keptBlobs {
			total += b.size
		}
		sort.Slice(keptResults, func(i, j int) bool { return keptResults[i].mtime.Before(keptResults[j].mtime) })
		for _, r := range keptResults {
			if total <= maxBytes {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if r.inProgress {
				continue
			}
			note(os.RemoveAll(r.path))
			total -= r.size
		}
		sort.Slice(keptBlobs, func(i, j int) bool { return keptBlobs[i].mtime.Before(keptBlobs[j].mtime) })
		for _, b := range keptBlobs {
			if total <= maxBytes {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if now.Sub(b.mtime) < blobGrace || b.metaFresh(now) {
				continue
			}
			note(s.removeBlobFiles(b))
			total -= b.size
		}
	}
	return errors.Join(errs...)
}

// sweepTmp removes abandoned upload/meta temp files older than the grace
// period.
func (s *Store) sweepTmp(ctx context.Context, now time.Time) error {
	entries, err := os.ReadDir(filepath.Join(s.Root, tmpDir))
	if err != nil {
		return fmt.Errorf("store: sweep tmp: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > inProgressGrace {
			errs = append(errs, os.RemoveAll(filepath.Join(s.Root, tmpDir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

type resultEntry struct {
	path       string
	mtime      time.Time
	size       int64
	inProgress bool // no manifest, younger than the grace period
	junk       bool // no manifest, older than the grace period
}

// listResults walks the results directory. Result age is the manifest's
// mtime when present, else the directory's.
func (s *Store) listResults(now time.Time) ([]resultEntry, error) {
	root := filepath.Join(s.Root, resultsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("store: sweep results: %w", err)
	}
	var out []resultEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		r := resultEntry{path: dir}
		if mst, err := os.Stat(filepath.Join(dir, ManifestName)); err == nil {
			r.mtime = mst.ModTime()
		} else {
			dst, err := e.Info()
			if err != nil {
				continue
			}
			r.mtime = dirNewestMtime(dir, dst.ModTime())
			if now.Sub(r.mtime) < inProgressGrace {
				r.inProgress = true
			} else {
				r.junk = true
			}
		}
		r.size = dirSize(dir)
		out = append(out, r)
	}
	return out, nil
}

// dirNewestMtime returns the newest mtime among dir itself and its direct
// children (a dir being written keeps receiving new files).
func dirNewestMtime(dir string, base time.Time) time.Time {
	newest := base
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

type blobEntry struct {
	hash      string
	files     []string  // blob file (or sequence dir) + meta file (whichever exist)
	mtime     time.Time // newest of the files (last upload / probe / use)
	metaMtime time.Time // the meta file's own mtime (zero when absent)
	size      int64
}

// metaFresh reports whether the meta file was written within blobGrace: the
// blob was just uploaded or probed and is about to be used.
func (b blobEntry) metaFresh(now time.Time) bool {
	return !b.metaMtime.IsZero() && now.Sub(b.metaMtime) < blobGrace
}

// listBlobs groups blob payloads (files, or <hash>.seq directories for image
// sequences) and meta files by hash. A sequence dir counts with the total
// size of its frames and its own mtime (TouchBlob refreshes that, as it does
// a file's; the frames inside are never touched after the upload).
func (s *Store) listBlobs() ([]blobEntry, error) {
	root := filepath.Join(s.Root, blobsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("store: sweep blobs: %w", err)
	}
	byHash := map[string]*blobEntry{}
	for _, e := range entries {
		name := e.Name()
		dot := strings.IndexByte(name, '.')
		hash, ext := name, ""
		if dot >= 0 {
			hash, ext = name[:dot], name[dot+1:]
		}
		if !recipe.IsHash(hash) {
			continue
		}
		if e.IsDir() && ext != SeqExt {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		be := byHash[hash]
		if be == nil {
			be = &blobEntry{hash: hash}
			byHash[hash] = be
		}
		full := filepath.Join(root, name)
		be.files = append(be.files, full)
		if e.IsDir() {
			be.size += dirSize(full)
		} else {
			be.size += info.Size()
		}
		if info.ModTime().After(be.mtime) {
			be.mtime = info.ModTime()
		}
		if ext == metaExt {
			be.metaMtime = info.ModTime()
		}
	}
	out := make([]blobEntry, 0, len(byHash))
	for _, be := range byHash {
		out = append(out, *be)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].hash < out[j].hash })
	return out, nil
}

// removeOrphanBlob deletes a payload that was listed without a meta file,
// re-checking under metaMu that no meta appeared meanwhile (a concurrent
// PutSequence/PutBlob may have just reclaimed and legitimised the path).
func (s *Store) removeOrphanBlob(b blobEntry) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if _, err := os.Stat(s.metaPath(b.hash)); err == nil {
		return nil
	}
	var errs []error
	for _, f := range b.files {
		if err := os.RemoveAll(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeBlobFiles deletes every path of the entry (a sequence dir with its
// frames included).
func (s *Store) removeBlobFiles(b blobEntry) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	var errs []error
	for _, f := range b.files {
		if err := os.RemoveAll(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SanitizeExt extracts the extension of a client file name and normalises it:
// lowercase, alphanumeric only, at most 8 characters; "bin" when nothing
// usable remains. It never returns "json" — that is the meta file's
// extension, so an uploaded .json file is stored as <hash>.jsonfile and its
// payload and meta stay separate — and never "seq", which is reserved for
// sequence directory blobs (sequence.go), so an uploaded .seq file is stored
// as <hash>.seqfile and Blob.IsSequence stays truthful.
func SanitizeExt(name string) string {
	base := lastPathElement(name)
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 || dot == len(base)-1 {
		return defaultExt
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base[dot+1:]) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	ext := b.String()
	if ext == "" || len(ext) > maxExtLen {
		return defaultExt
	}
	if ext == SeqExt {
		// Only PutSequence may mint Ext "seq" (getBlobLocked keeps it for
		// genuine sequence metas, so the remap cannot live in normalizeExt).
		return seqBlobExt
	}
	return normalizeExt(ext)
}

// normalizeExt maps a stored/sanitised extension onto one a blob file may
// carry: empty → "bin", the meta extension → "jsonfile".
func normalizeExt(ext string) string {
	switch ext {
	case "":
		return defaultExt
	case metaExt:
		return jsonBlobExt
	}
	return ext
}

// SanitizeName reduces a client file name to its last path element, strips
// control characters and path separators, and bounds its length.
func SanitizeName(name string) string {
	base := lastPathElement(name)
	var b strings.Builder
	for _, r := range base {
		if unicode.IsControl(r) || r == '/' || r == '\\' || r == unicode.ReplacementChar {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return defaultName
	}
	if len(out) > maxNameLen {
		// Cut on a rune boundary.
		cut := maxNameLen
		for cut > 0 && !isRuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// lastPathElement returns the part after the last '/' or '\'.
func lastPathElement(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// sanitizeID keeps [A-Za-z0-9_-] of id.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RandomID returns n random bytes as lowercase hex (used for scratch and
// staging names).
func RandomID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand never fails on supported platforms; fall back to time.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
