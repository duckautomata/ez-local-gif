package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

func seqParts(names []string, payloads []string) []SequencePart {
	parts := make([]SequencePart, len(names))
	for i := range names {
		parts[i] = SequencePart{Name: names[i], R: strings.NewReader(payloads[i])}
	}
	return parts
}

// seqHash computes the expected combined hash: sha256 over the ordered
// per-file sha256 digests.
func seqHash(payloads ...string) string {
	h := sha256.New()
	for _, p := range payloads {
		d := sha256.Sum256([]byte(p))
		h.Write(d[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestPutSequenceLayoutAndDedupe(t *testing.T) {
	st := newTestStore(t)
	names := []string{"C:\\frames\\frame_0009.PNG", "frame_0010.png", "frame_0011.png"}
	payloads := []string{"frame nine", "frame ten", "frame eleven"}

	b, err := st.PutSequence(seqParts(names, payloads))
	if err != nil {
		t.Fatalf("PutSequence: %v", err)
	}
	if b.Hash != seqHash(payloads...) {
		t.Errorf("hash = %s, want the hash of the ordered digests %s", b.Hash, seqHash(payloads...))
	}
	if b.Ext != SeqExt || b.Name != "frame_0009.PNG" || b.Size != int64(len("frame nine")+len("frame ten")+len("frame eleven")) {
		t.Errorf("blob = %+v", b)
	}
	if b.Path != filepath.Join(st.Root, "blobs", b.Hash+".seq") {
		t.Errorf("path = %q", b.Path)
	}
	if !b.IsSequence() {
		t.Error("IsSequence false")
	}
	// Renumbered from 1 with the shared (lowercased) extension.
	for i, want := range payloads {
		p := filepath.Join(b.Path, SequenceFrameName(i+1, "png"))
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Errorf("frame %d: %q (%v), want %q", i+1, got, err, want)
		}
	}
	entries, _ := os.ReadDir(b.Path)
	if len(entries) != 3 {
		t.Errorf("dir has %d entries, want 3", len(entries))
	}
	files, ext, err := SequenceFrames(b.Path)
	if err != nil || ext != "png" || len(files) != 3 || filepath.Base(files[0]) != "000001.png" || filepath.Base(files[2]) != "000003.png" {
		t.Errorf("SequenceFrames = %v %q %v", files, ext, err)
	}
	if SequencePattern(ext) != "%06d.png" {
		t.Errorf("pattern = %q", SequencePattern(ext))
	}
	// Staging is gone, blobs dir holds the dir + meta only.
	if tmpEntries, _ := os.ReadDir(filepath.Join(st.Root, "tmp")); len(tmpEntries) != 0 {
		t.Errorf("tmp not clean: %d entries", len(tmpEntries))
	}
	if blobEntries, _ := os.ReadDir(filepath.Join(st.Root, "blobs")); len(blobEntries) != 2 {
		t.Errorf("blobs dir has %d entries, want dir + meta", len(blobEntries))
	}

	// GetBlob round trip (Info nil until probed).
	got, err := st.GetBlob(b.Hash)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if got.Path != b.Path || got.Ext != SeqExt || got.Info != nil || got.Size != b.Size {
		t.Errorf("GetBlob = %+v", got)
	}
	info := recipe.ProbeInfo{Format: "image2", Codec: "png", PixFmt: "rgba", Bits: 8, Width: 8, Height: 6, FPS: 10, Duration: 0.3, Frames: 3,
		HasAlpha: true, Kind: recipe.KindSequence, Sequence: &recipe.SequenceInfo{Count: 3, Pattern: "%06d.png", DelayMS: 100}}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetBlob(b.Hash)
	if got.Info == nil || got.Info.Kind != recipe.KindSequence || got.Info.Sequence == nil || got.Info.Sequence.Pattern != "%06d.png" {
		t.Errorf("info round trip: %+v", got.Info)
	}

	// Same frames, same order, other names: dedupe returns the existing blob
	// (with its info) and leaves one directory.
	dup, err := st.PutSequence(seqParts([]string{"a.png", "b.png", "c.png"}, payloads))
	if err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	if dup.Hash != b.Hash || dup.Name != "frame_0009.PNG" || dup.Info == nil {
		t.Errorf("dedupe = %+v", dup)
	}
	if blobEntries, _ := os.ReadDir(filepath.Join(st.Root, "blobs")); len(blobEntries) != 2 {
		t.Errorf("blobs dir has %d entries after dedupe, want 2", len(blobEntries))
	}
	// Different order → different blob.
	other, err := st.PutSequence(seqParts(names, []string{payloads[1], payloads[0], payloads[2]}))
	if err != nil {
		t.Fatal(err)
	}
	if other.Hash == b.Hash {
		t.Error("reordered frames must not dedupe")
	}
	// A single file with the same bytes is not the same blob as a sequence.
	single, _ := st.PutBlob(strings.NewReader(payloads[0]), "frame.png")
	if single.Hash == b.Hash {
		t.Error("sequence hash collides with a file hash")
	}
}

func TestPutSequenceErrors(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.PutSequence(seqParts([]string{"a.png"}, []string{"x"})); err == nil {
		t.Error("single part accepted")
	}
	if _, err := st.PutSequence(nil); err == nil {
		t.Error("no parts accepted")
	}
	// Mixed extensions are rejected before anything is written.
	_, err := st.PutSequence(seqParts([]string{"a.png", "b.jpg", "c.png"}, []string{"1", "2", "3"}))
	if !errors.Is(err, ErrMixedSequence) {
		t.Errorf("mixed: err = %v, want ErrMixedSequence", err)
	}
	if !strings.Contains(err.Error(), "png") || !strings.Contains(err.Error(), "jpg") || !strings.Contains(err.Error(), "frame 2") {
		t.Errorf("mixed error lacks detail: %v", err)
	}
	// Too many parts: refused without touching the disk.
	many := make([]SequencePart, MaxSequenceFrames+1)
	for i := range many {
		many[i] = SequencePart{Name: "f.png", R: strings.NewReader("x")}
	}
	if _, err := st.PutSequence(many); err == nil || !strings.Contains(err.Error(), strconv.Itoa(MaxSequenceFrames)) {
		t.Errorf("too many: %v", err)
	}
	// An empty frame is an error and the staging dir is cleaned up.
	if _, err := st.PutSequence(seqParts([]string{"a.png", "b.png"}, []string{"data", ""})); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty frame: %v", err)
	}
	if _, err := st.PutSequence([]SequencePart{{Name: "a.png", R: strings.NewReader("x")}, {Name: "b.png"}}); err == nil {
		t.Error("nil reader accepted")
	}
	for _, d := range []string{"tmp", "blobs"} {
		if entries, _ := os.ReadDir(filepath.Join(st.Root, d)); len(entries) != 0 {
			names := []string{}
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("%s not clean after failed uploads: %v", d, names)
		}
	}
	// SequenceFrames on junk.
	if _, _, err := SequenceFrames(filepath.Join(st.Root, "nope")); err == nil {
		t.Error("missing dir accepted")
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "readme.txt"), []byte("x"), 0o644)
	if _, _, err := SequenceFrames(empty); err == nil {
		t.Error("dir without frames accepted")
	}
	mixed := t.TempDir()
	os.WriteFile(filepath.Join(mixed, "000001.png"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(mixed, "000002.jpg"), []byte("x"), 0o644)
	if _, _, err := SequenceFrames(mixed); !errors.Is(err, ErrMixedSequence) {
		t.Errorf("mixed dir: %v", err)
	}
}

// TestSequenceBlobLifecycle: touch, delete and sweep treat a sequence
// directory like a file blob (the whole dir goes, size is the sum of the
// frames).
func TestSequenceBlobLifecycle(t *testing.T) {
	st := newTestStore(t)
	payloads := []string{strings.Repeat("a", 300), strings.Repeat("b", 300)}
	seq, err := st.PutSequence(seqParts([]string{"1.png", "2.png"}, payloads))
	if err != nil {
		t.Fatal(err)
	}
	// Touch refreshes the dir mtime.
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(seq.Path, old, old)
	os.Chtimes(st.metaPath(seq.Hash), old, old)
	if err := st.TouchBlob(seq.Hash); err != nil {
		t.Fatalf("TouchBlob: %v", err)
	}
	if fi, _ := os.Stat(seq.Path); fi.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Errorf("sequence dir not touched: %v", fi.ModTime())
	}
	// Sweep by TTL keeps the touched sequence...
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(seq.Hash); err != nil {
		t.Errorf("touched sequence swept: %v", err)
	}
	// ...and removes an old one, frames included.
	os.Chtimes(seq.Path, old, old)
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(seq.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("old sequence survived: %v", err)
	}
	if _, err := os.Stat(seq.Path); !errors.Is(err, os.ErrNotExist) {
		t.Error("sequence dir still exists after sweep")
	}
	if _, err := os.Stat(st.metaPath(seq.Hash)); !errors.Is(err, os.ErrNotExist) {
		t.Error("sequence meta still exists after sweep")
	}

	// Size pass counts the frames: two sequences of 600 bytes, cap 700 →
	// the older one goes.
	a, _ := st.PutSequence(seqParts([]string{"1.png", "2.png"}, payloads))
	b, _ := st.PutSequence(seqParts([]string{"1.png", "2.png"}, []string{strings.Repeat("c", 300), strings.Repeat("d", 300)}))
	oldT := time.Now().Add(-5 * time.Hour)
	os.Chtimes(a.Path, oldT, oldT)
	os.Chtimes(st.metaPath(a.Hash), oldT, oldT)
	older := time.Now().Add(-3 * time.Hour)
	os.Chtimes(b.Path, older, older)
	os.Chtimes(st.metaPath(b.Hash), older, older)
	blobs, err := st.listBlobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, be := range blobs {
		if be.hash == a.Hash || be.hash == b.Hash {
			metaSize, _ := os.Stat(st.metaPath(be.hash))
			if be.size != 600+metaSize.Size() {
				t.Errorf("sequence %s size = %d, want 600 + meta %d", short(be.hash), be.size, metaSize.Size())
			}
		}
	}
	metaA, _ := os.Stat(st.metaPath(a.Hash))
	metaB, _ := os.Stat(st.metaPath(b.Hash))
	cap := 600 + metaB.Size() + metaA.Size()/2 // room for one sequence, not two
	if err := st.Sweep(context.Background(), 0, cap); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(a.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("older sequence should be evicted: %v", err)
	}
	if _, err := st.GetBlob(b.Hash); err != nil {
		t.Errorf("newer sequence evicted: %v", err)
	}

	// DeleteBlob removes the directory.
	if err := st.DeleteBlob(b.Hash); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}
	if _, err := os.Stat(b.Path); !errors.Is(err, os.ErrNotExist) {
		t.Error("DeleteBlob left the sequence dir")
	}
	if _, err := st.GetBlob(b.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted sequence still readable: %v", err)
	}
	if err := st.DeleteBlob(b.Hash); err != nil {
		t.Errorf("second delete: %v", err)
	}
	// A stray non-sequence directory in blobs/ is ignored by the sweeper.
	stray := filepath.Join(st.Root, "blobs", strings.Repeat("9", 64)+".junk")
	os.MkdirAll(stray, 0o755)
	os.WriteFile(filepath.Join(stray, "x"), []byte("x"), 0o644)
	os.Chtimes(stray, old, old)
	if err := st.Sweep(context.Background(), time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("sweeper removed a directory that is not a sequence blob")
	}
}

// TestPutSequenceReclaimsOrphanDir: a <hash>.seq directory without its meta
// file (a crash between the staging rename and the meta write, or a partial
// delete) must not block re-uploads of the same frames forever — os.Rename
// refuses to move a directory onto an existing one, so PutSequence has to
// reclaim the orphan and retry.
func TestPutSequenceReclaimsOrphanDir(t *testing.T) {
	names := []string{"a.png", "b.png"}
	payloads := []string{"frame one", "frame two"}
	hash := seqHash(payloads...)

	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"complete orphan", func(t *testing.T, dir string) {
			t.Helper()
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "000001.png"), []byte("stale one"), 0o644)
			os.WriteFile(filepath.Join(dir, "000002.png"), []byte("stale two"), 0o644)
		}},
		{"partial orphan", func(t *testing.T, dir string) {
			t.Helper()
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "000002.png"), []byte("stale two"), 0o644)
		}},
		{"empty orphan", func(t *testing.T, dir string) {
			t.Helper()
			os.MkdirAll(dir, 0o755)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newTestStore(t)
			dir := filepath.Join(st.Root, "blobs", hash+".seq")
			c.setup(t, dir)

			b, err := st.PutSequence(seqParts(names, payloads))
			if err != nil {
				t.Fatalf("PutSequence over an orphan dir: %v", err)
			}
			if b.Hash != hash || b.Path != dir {
				t.Errorf("blob = %+v", b)
			}
			// The frames are the freshly uploaded ones, not the stale
			// leftovers, and the blob is fully usable.
			files, ext, err := SequenceFrames(dir)
			if err != nil || ext != "png" || len(files) != 2 {
				t.Fatalf("SequenceFrames = %v %q %v", files, ext, err)
			}
			for i, want := range payloads {
				got, err := os.ReadFile(files[i])
				if err != nil || string(got) != want {
					t.Errorf("frame %d = %q (%v), want %q", i+1, got, err, want)
				}
			}
			if got, err := st.GetBlob(hash); err != nil || !got.IsSequence() {
				t.Errorf("GetBlob after reclaim: %+v %v", got, err)
			}
		})
	}
}

// TestSweepRemovesMetaLessOrphans: a payload without its meta file is
// unreachable (getBlobLocked needs the meta) and, once older than blobGrace,
// junk — the sweeper must reclaim it even with the TTL pass disabled, or an
// orphaned sequence dir blocks re-uploads until PutSequence reclaims it.
func TestSweepRemovesMetaLessOrphans(t *testing.T) {
	st := newTestStore(t)
	old := time.Now().Add(-2 * blobGrace)

	orphanFile := filepath.Join(st.Root, "blobs", strings.Repeat("1", 64)+".gif")
	os.WriteFile(orphanFile, []byte("x"), 0o644)
	os.Chtimes(orphanFile, old, old)

	orphanDir := filepath.Join(st.Root, "blobs", strings.Repeat("2", 64)+".seq")
	os.MkdirAll(orphanDir, 0o755)
	os.WriteFile(filepath.Join(orphanDir, "000001.png"), []byte("x"), 0o644)
	os.Chtimes(orphanDir, old, old)

	// A meta-less payload inside the grace window is an in-flight upload
	// (payload and meta are written back-to-back): keep it.
	fresh := filepath.Join(st.Root, "blobs", strings.Repeat("3", 64)+".mov")
	os.WriteFile(fresh, []byte("x"), 0o644)

	// A complete old blob survives a sweep whose TTL pass is disabled.
	valid, err := st.PutBlob(strings.NewReader("real upload"), "clip.mov")
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(valid.Path, old, old)
	os.Chtimes(st.metaPath(valid.Hash), old, old)

	if err := st.Sweep(context.Background(), 0, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(orphanFile); !errors.Is(err, os.ErrNotExist) {
		t.Error("meta-less blob file survived the sweep")
	}
	if _, err := os.Stat(orphanDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("meta-less sequence dir survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh meta-less payload (in-flight upload) was swept")
	}
	if _, err := st.GetBlob(valid.Hash); err != nil {
		t.Errorf("complete blob swept with ttl disabled: %v", err)
	}
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// TestPutSequenceLargeCount streams a realistic number of tiny frames to
// check nothing is quadratic or leaks descriptors.
func TestPutSequenceLargeCount(t *testing.T) {
	st := newTestStore(t)
	const n = 300
	parts := make([]SequencePart, n)
	for i := range parts {
		parts[i] = SequencePart{Name: "f" + strconv.Itoa(i) + ".jpg", R: bytes.NewReader([]byte{byte(i), byte(i >> 8), 1})}
	}
	b, err := st.PutSequence(parts)
	if err != nil {
		t.Fatal(err)
	}
	files, ext, err := SequenceFrames(b.Path)
	if err != nil || ext != "jpg" || len(files) != n {
		t.Fatalf("frames = %d %q %v", len(files), ext, err)
	}
	if filepath.Base(files[n-1]) != "000300.jpg" {
		t.Errorf("last frame = %s", filepath.Base(files[n-1]))
	}
	if b.Size != 3*n {
		t.Errorf("size = %d", b.Size)
	}
}
