package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Phase 2: image-sequence blobs.
//
// An image sequence is stored as a directory blob:
//
//	<Root>/blobs/<hash>.seq/000001.<ext> … 00000N.<ext>
//	<Root>/blobs/<hash>.json              Blob meta (Ext "seq", Name = first file name, Info.Sequence set by the prober)
//
// hash = sha256 over the ordered per-file sha256 digests, so the same frames
// in the same order dedupe. Files are renumbered from 1 in the order given
// (the caller sorts them naturally by name first). All files must share one
// extension (the image2 demuxer pattern); mixed extensions are rejected with
// ErrMixedSequence. Blob.Path is the directory.

// SequencePart is one uploaded frame.
type SequencePart struct {
	Name string
	R    io.Reader
}

// ErrMixedSequence is returned when the parts do not share an extension.
var ErrMixedSequence = errors.New("store: image sequence files must share one extension")

// SeqExt is the Blob.Ext of sequence blobs.
const SeqExt = "seq"

// MaxSequenceFrames bounds an uploaded sequence.
const MaxSequenceFrames = 5000

// MinSequenceFrames is the smallest sequence PutSequence accepts (a single
// image is an ordinary blob).
const MinSequenceFrames = 2

// SequencePattern returns the image2 pattern for a sequence blob dir, e.g.
// "%06d.png" (the extension recorded in the meta).
func SequencePattern(ext string) string { return "%06d." + ext }

// SequenceFrameName returns the file name of frame n (1-based) inside a
// sequence blob dir: "000001.png".
func SequenceFrameName(n int, ext string) string { return fmt.Sprintf(SequencePattern(ext), n) }

// PutSequence streams the parts into a new sequence blob (idempotent on the
// combined hash) and returns its Blob (Info nil until probed). At least two
// parts are required; at most MaxSequenceFrames.
//
// The frames are written to a staging directory under <Root>/tmp while every
// part is hashed, then the directory is renamed into place (same filesystem,
// so a crash never leaves a half-written blob dir). An empty part is an error:
// it could never be decoded and would only break the whole sequence later.
func (s *Store) PutSequence(parts []SequencePart) (*Blob, error) {
	if len(parts) < MinSequenceFrames {
		return nil, fmt.Errorf("store: an image sequence needs at least %d frames, got %d", MinSequenceFrames, len(parts))
	}
	if len(parts) > MaxSequenceFrames {
		return nil, fmt.Errorf("store: an image sequence may have at most %d frames, got %d", MaxSequenceFrames, len(parts))
	}
	ext, err := sequenceExt(parts)
	if err != nil {
		return nil, err
	}

	staging, err := os.MkdirTemp(filepath.Join(s.Root, tmpDir), "seq-*")
	if err != nil {
		return nil, fmt.Errorf("store: create sequence staging: %w", err)
	}
	// Any early return discards the partial directory.
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(staging)
		}
	}()

	combined := sha256.New()
	var size int64
	for i, p := range parts {
		n, digest, err := writeSequenceFrame(filepath.Join(staging, SequenceFrameName(i+1, ext)), p.R)
		if err != nil {
			return nil, fmt.Errorf("store: sequence frame %d (%s): %w", i+1, SanitizeName(p.Name), err)
		}
		if n == 0 {
			return nil, fmt.Errorf("store: sequence frame %d (%s) is empty", i+1, SanitizeName(p.Name))
		}
		size += n
		combined.Write(digest)
	}
	hash := hex.EncodeToString(combined.Sum(nil))

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	// Idempotent path: the same frames in the same order are already stored.
	if existing, err := s.getBlobLocked(hash); err == nil {
		now := time.Now()
		_ = os.Chtimes(existing.Path, now, now)
		return existing, nil
	}

	final := s.blobPath(hash, SeqExt)
	if err := os.Rename(staging, final); err != nil {
		// os.Rename refuses to move a directory onto an existing one (even
		// an empty one, on every platform). Anything already at final is an
		// orphan — a crash between this rename and the meta write, or a
		// partially failed delete, left the dir behind without its meta
		// (getBlobLocked just said this hash has no meta, and nothing else
		// can create it while metaMu is held) — so reclaim it and retry
		// once; otherwise the same frame set could never be uploaded again.
		if _, statErr := os.Lstat(final); statErr != nil {
			return nil, fmt.Errorf("store: place sequence blob: %w", err)
		}
		if rmErr := os.RemoveAll(final); rmErr != nil {
			return nil, fmt.Errorf("store: place sequence blob: %w (reclaiming orphan dir: %v)", err, rmErr)
		}
		if err := os.Rename(staging, final); err != nil {
			return nil, fmt.Errorf("store: place sequence blob: %w", err)
		}
	}
	keep = true
	b := &Blob{
		Hash:     hash,
		Name:     SanitizeName(parts[0].Name),
		Ext:      SeqExt,
		Size:     size,
		Uploaded: time.Now().UTC(),
		Path:     final,
	}
	if err := s.writeMetaLocked(b); err != nil {
		os.RemoveAll(final)
		return nil, err
	}
	return b, nil
}

// sequenceExt returns the shared (sanitised) extension of the parts or
// ErrMixedSequence.
func sequenceExt(parts []SequencePart) (string, error) {
	ext := SanitizeExt(parts[0].Name)
	for i := 1; i < len(parts); i++ {
		if other := SanitizeExt(parts[i].Name); other != ext {
			return "", fmt.Errorf("%w: %q (frame 1) vs %q (frame %d)", ErrMixedSequence, ext, other, i+1)
		}
	}
	return ext, nil
}

// writeSequenceFrame streams r to path while hashing it and returns the byte
// count and the sha256 digest.
func writeSequenceFrame(path string, r io.Reader) (int64, []byte, error) {
	if r == nil {
		return 0, nil, errors.New("no data")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return 0, nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		f.Close()
		return 0, nil, err
	}
	if err := f.Close(); err != nil {
		return 0, nil, err
	}
	return n, h.Sum(nil), nil
}

// IsSequence reports whether the blob is an image-sequence directory.
func (b *Blob) IsSequence() bool { return b != nil && b.Ext == SeqExt }

// SequenceFrames lists the frame files of a sequence blob dir in order
// (000001.<ext> first) and returns them with the shared extension. It
// tolerates stray entries (anything not named like a frame is ignored).
func SequenceFrames(dir string) (files []string, ext string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("store: read sequence dir: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		dot := strings.IndexByte(name, '.')
		if dot != 6 || dot == len(name)-1 || !allDigits(name[:dot]) {
			continue
		}
		if ext == "" {
			ext = name[dot+1:]
		} else if name[dot+1:] != ext {
			return nil, "", fmt.Errorf("%w: %q vs %q in %s", ErrMixedSequence, ext, name[dot+1:], dir)
		}
		files = append(files, filepath.Join(dir, name))
	}
	// os.ReadDir returns entries sorted by name, and zero-padded numbers sort
	// numerically, so files is already in frame order.
	if len(files) == 0 {
		return nil, "", fmt.Errorf("store: %s holds no sequence frames", dir)
	}
	return files, ext, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
