package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	st, err := New(filepath.Join(root, "data"), filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st
}

func TestNewCreatesLayout(t *testing.T) {
	st := newTestStore(t)
	for _, d := range []string{"blobs", "results", "tmp"} {
		if fi, err := os.Stat(filepath.Join(st.Root, d)); err != nil || !fi.IsDir() {
			t.Errorf("missing dir %s: %v", d, err)
		}
	}
	if fi, err := os.Stat(st.Scratch); err != nil || !fi.IsDir() {
		t.Errorf("missing scratch: %v", err)
	}
}

func TestNewScratchFallback(t *testing.T) {
	root := t.TempDir()
	// A regular file where the scratch dir should be makes MkdirAll fail.
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := New(filepath.Join(root, "data"), filepath.Join(blocker, "scratch"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := filepath.Join(os.TempDir(), "ezl")
	if st.Scratch != want {
		t.Errorf("scratch = %q, want fallback %q", st.Scratch, want)
	}
}

func TestPutBlobDedupeAndMeta(t *testing.T) {
	st := newTestStore(t)
	content := []byte("hello blob content")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	b1, err := st.PutBlob(bytes.NewReader(content), "C:\\Users\\me\\My Clip.MOV")
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if b1.Hash != wantHash {
		t.Errorf("hash = %s, want %s", b1.Hash, wantHash)
	}
	if b1.Ext != "mov" {
		t.Errorf("ext = %q, want mov", b1.Ext)
	}
	if b1.Name != "My Clip.MOV" {
		t.Errorf("name = %q", b1.Name)
	}
	if b1.Size != int64(len(content)) {
		t.Errorf("size = %d", b1.Size)
	}
	if b1.Path != filepath.Join(st.Root, "blobs", wantHash+".mov") {
		t.Errorf("path = %q", b1.Path)
	}
	got, err := os.ReadFile(b1.Path)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("blob file content mismatch: %v", err)
	}

	// Second upload of the same bytes under a different name is a no-op that
	// returns the existing meta.
	b2, err := st.PutBlob(bytes.NewReader(content), "other.gif")
	if err != nil {
		t.Fatalf("PutBlob dup: %v", err)
	}
	if b2.Hash != b1.Hash || b2.Ext != "mov" || b2.Name != "My Clip.MOV" {
		t.Errorf("dedupe returned %+v, want the first blob", b2)
	}
	entries, _ := os.ReadDir(filepath.Join(st.Root, "blobs"))
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("blobs dir has %d entries, want 2 (blob + meta): %v", len(entries), names)
	}
	// Temp dir must be clean.
	tmpEntries, _ := os.ReadDir(filepath.Join(st.Root, "tmp"))
	if len(tmpEntries) != 0 {
		t.Errorf("tmp dir not clean: %d entries", len(tmpEntries))
	}

	// Meta round trip.
	info := recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444",
		PixFmt: "yuva444p10le", Bits: 10, Width: 320, Height: 240, FPS: 30, Duration: 3, Frames: 90,
		HasAlpha: true, Kind: recipe.KindVideo, Premultiplied: true}
	if err := st.SetBlobInfo(b1.Hash, info); err != nil {
		t.Fatalf("SetBlobInfo: %v", err)
	}
	b3, err := st.GetBlob(b1.Hash)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if b3.Info == nil || *b3.Info != info {
		t.Errorf("info round trip: got %+v", b3.Info)
	}
	if b3.Path != b1.Path || b3.Name != b1.Name || b3.Size != b1.Size {
		t.Errorf("GetBlob mismatch: %+v vs %+v", b3, b1)
	}

	// Re-uploading after info is set keeps the info.
	b4, err := st.PutBlob(bytes.NewReader(content), "again.mov")
	if err != nil {
		t.Fatal(err)
	}
	if b4.Info == nil || b4.Info.Codec != "prores" {
		t.Errorf("dedupe lost info: %+v", b4.Info)
	}
}

func TestBlobInfoVersion(t *testing.T) {
	st := newTestStore(t)
	content := []byte("rotated phone clip")
	b, err := st.PutBlob(bytes.NewReader(content), "portrait.mov")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", PixFmt: "yuv420p", Bits: 8,
		Width: 1920, Height: 1080, FPS: 30, Duration: 2, Frames: 60, Kind: recipe.KindVideo}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBlob(b.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Info == nil || got.InfoVersion != InfoVersion {
		t.Fatalf("fresh info: %+v (version %d)", got.Info, got.InfoVersion)
	}
	// The meta file carries the version.
	raw, _ := os.ReadFile(st.metaPath(b.Hash))
	if !strings.Contains(string(raw), `"infoVersion": `+strconv.Itoa(InfoVersion)) {
		t.Errorf("meta lacks infoVersion: %s", raw)
	}

	// Rewrite the meta as an older server would have (info without a
	// version): the info must be hidden so the upload path re-probes.
	stale := *got
	stale.InfoVersion = 0
	data, _ := json.MarshalIndent(&stale, "", "  ")
	if err := os.WriteFile(st.metaPath(b.Hash), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetBlob(b.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Info != nil || got.InfoVersion != 0 {
		t.Errorf("stale info must be hidden: %+v (version %d)", got.Info, got.InfoVersion)
	}
	if got.Name != "portrait.mov" || got.Size != int64(len(content)) {
		t.Errorf("other meta lost: %+v", got)
	}
	// Dedupe upload also hides it, so the caller probes again...
	dup, err := st.PutBlob(bytes.NewReader(content), "portrait.mov")
	if err != nil {
		t.Fatal(err)
	}
	if dup.Info != nil {
		t.Errorf("dedupe returned stale info: %+v", dup.Info)
	}
	// ...and storing new info stamps the current version.
	info.Width, info.Height = 1080, 1920
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetBlob(b.Hash)
	if got.Info == nil || *got.Info != info || got.InfoVersion != InfoVersion {
		t.Errorf("re-probed info: %+v (version %d)", got.Info, got.InfoVersion)
	}
	// A future version is stale as well (downgrade safety).
	stale = *got
	stale.InfoVersion = InfoVersion + 1
	data, _ = json.MarshalIndent(&stale, "", "  ")
	os.WriteFile(st.metaPath(b.Hash), data, 0o644)
	if got, _ = st.GetBlob(b.Hash); got.Info != nil {
		t.Error("info from a newer version must be hidden")
	}
}

func TestScratchSpace(t *testing.T) {
	st := newTestStore(t)
	free, okFree := st.ScratchFree()
	total, okTotal := st.ScratchTotal()
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if !okFree || !okTotal {
			t.Fatalf("fs space unavailable on %s", runtime.GOOS)
		}
		if free <= 0 || total <= 0 || free > total {
			t.Errorf("free/total = %d/%d", free, total)
		}
	} else if okFree || okTotal {
		t.Errorf("unexpected fs space support on %s", runtime.GOOS)
	}
	// A temp dir on a normal disk is never "too small", so New keeps it.
	if filepath.Base(st.Scratch) != "scratch" || filepath.Dir(st.Scratch) != filepath.Dir(st.Root) {
		t.Errorf("scratch moved unexpectedly: %s", st.Scratch)
	}
	if !scratchTooSmall(64<<20) || scratchTooSmall(MinScratchBytes) || scratchTooSmall(4<<30) {
		t.Error("scratchTooSmall threshold")
	}
	// chooseScratch with an unknown/adequate filesystem is the identity.
	if got := chooseScratch(st.Root, st.Scratch); got != st.Scratch {
		t.Errorf("chooseScratch = %q, want %q", got, st.Scratch)
	}
	if _, err := os.Stat(filepath.Join(st.Root, "scratch")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("<root>/scratch created although the tmpfs was fine: %v", err)
	}
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1 KiB", 1536: "1.5 KiB", 64 << 20: "64 MiB",
		4 << 30: "4 GiB", 3<<30 + 900<<20: "3.9 GiB", 2 << 40: "2 TiB", 5 << 50: "5 PiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestNewOnRealScratch is opt-in: set EZLG_TEST_SCRATCH to a directory on a
// real tmpfs (e.g. docker run --shm-size=64m -e EZLG_TEST_SCRATCH=/dev/shm/ezl-test)
// to check that a too-small tmpfs is swapped for <root>/scratch and an
// adequate one is kept.
func TestNewOnRealScratch(t *testing.T) {
	scratch := os.Getenv("EZLG_TEST_SCRATCH")
	if scratch == "" {
		t.Skip("EZLG_TEST_SCRATCH not set")
	}
	_, total, ok := fsSpace(scratch)
	if !ok {
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, total, ok = fsSpace(scratch); !ok {
			t.Skipf("fs space unavailable for %s", scratch)
		}
	}
	root := filepath.Join(t.TempDir(), "data")
	st, err := New(root, scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scratch %s (%s) → %s", scratch, humanBytes(total), st.Scratch)
	abs, _ := filepath.Abs(scratch)
	if scratchTooSmall(total) {
		if st.Scratch != filepath.Join(st.Root, "scratch") {
			t.Errorf("small tmpfs (%s) kept: %s", humanBytes(total), st.Scratch)
		}
	} else if st.Scratch != abs {
		t.Errorf("adequate tmpfs (%s) replaced by %s", humanBytes(total), st.Scratch)
	}
	if got, ok := st.ScratchTotal(); !ok || got <= 0 {
		t.Errorf("ScratchTotal = %d %v", got, ok)
	}
}

func TestDeleteBlob(t *testing.T) {
	st := newTestStore(t)
	b, err := st.PutBlob(strings.NewReader("bytes"), "x.gif")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBlob(b.Hash); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}
	if _, err := st.GetBlob(b.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob still readable: %v", err)
	}
	if _, err := os.Stat(b.Path); !errors.Is(err, os.ErrNotExist) {
		t.Error("blob file still exists")
	}
	if err := st.DeleteBlob(b.Hash); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if err := st.DeleteBlob("junk"); err != nil {
		t.Errorf("delete invalid hash: %v", err)
	}
}

func TestGetBlobNotFound(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.GetBlob(strings.Repeat("a", 64)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetBlob("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad hash: err = %v, want ErrNotFound", err)
	}
	if err := st.SetBlobInfo(strings.Repeat("b", 64), recipe.ProbeInfo{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetBlobInfo unknown: %v", err)
	}
}

func TestSanitizeExtAndName(t *testing.T) {
	cases := []struct{ in, ext, name string }{
		{"clip.MOV", "mov", "clip.MOV"},
		{"clip", "bin", "clip"},
		{"clip.", "bin", "clip."},
		{".gitignore", "bin", ".gitignore"},
		{".env", "env", ".env"},
		{"a.tar.gz", "gz", "a.tar.gz"},
		{"weird.G!F", "gf", "weird.G!F"},
		{"toolong.abcdefghij", "bin", "toolong.abcdefghij"},
		{"/tmp/../x/y.png", "png", "y.png"},
		{"dir\\sub\\z.WebP", "webp", "z.WebP"},
		{"", "bin", "upload"},
		{"..", "bin", "upload"},
		{"ctrl\x00char.gif", "gif", "ctrlchar.gif"},
		{"héllo.gif", "gif", "héllo.gif"},
		// The meta file's own extension is never handed out for a payload.
		{"recipe.json", "jsonfile", "recipe.json"},
		{"RECIPE.JSON", "jsonfile", "RECIPE.JSON"},
		{"weird.j.s.o.n", "n", "weird.j.s.o.n"},
		{"x.jsonl", "jsonl", "x.jsonl"},
		// "seq" is the sequence-directory contract (Blob.IsSequence); a plain
		// upload never gets it.
		{"frames.seq", "seqfile", "frames.seq"},
		{"FRAMES.SEQ", "seqfile", "FRAMES.SEQ"},
		{".seq", "seqfile", ".seq"},
		{"x.seq2", "seq2", "x.seq2"},
	}
	for _, c := range cases {
		if got := SanitizeExt(c.in); got != c.ext {
			t.Errorf("SanitizeExt(%q) = %q, want %q", c.in, got, c.ext)
		}
		if got := SanitizeName(c.in); got != c.name {
			t.Errorf("SanitizeName(%q) = %q, want %q", c.in, got, c.name)
		}
	}
	long := strings.Repeat("x", 300) + ".gif"
	if got := SanitizeName(long); len(got) > maxNameLen {
		t.Errorf("long name not bounded: %d", len(got))
	}
}

// TestPutBlobJSONKeepsPayloadAndMeta: an uploaded .json file must not land
// on <hash>.json, which is where the blob's meta lives — the meta write would
// clobber the payload and GetBlob would hand the meta back as the file.
func TestPutBlobJSONKeepsPayloadAndMeta(t *testing.T) {
	st := newTestStore(t)
	payload := []byte(`{"not":"the meta","frames":[1,2,3]}`)
	b, err := st.PutBlob(bytes.NewReader(payload), "recipe.json")
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if b.Ext != "jsonfile" {
		t.Errorf("ext = %q, want jsonfile", b.Ext)
	}
	if b.Path == st.metaPath(b.Hash) {
		t.Fatalf("blob path %s collides with the meta path", b.Path)
	}
	if b.Name != "recipe.json" {
		t.Errorf("name = %q (the original name must survive)", b.Name)
	}
	// Payload intact, meta separate and parseable as a Blob.
	got, err := os.ReadFile(b.Path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("payload: %q (%v), want %q", got, err, payload)
	}
	raw, err := os.ReadFile(st.metaPath(b.Hash))
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	var meta Blob
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Hash != b.Hash || meta.Ext != "jsonfile" {
		t.Errorf("meta = %s (%v)", raw, err)
	}
	// Round trip through GetBlob (+ probe info) still points at the payload.
	if err := st.SetBlobInfo(b.Hash, recipe.ProbeInfo{Format: "png_pipe", Codec: "png", Width: 1, Height: 1, Frames: 1, IsStill: true, Kind: recipe.KindImage}); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetBlob(b.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if again.Path != b.Path || again.Info == nil {
		t.Errorf("GetBlob = %+v", again)
	}
	got, _ = os.ReadFile(again.Path)
	if !bytes.Equal(got, payload) {
		t.Errorf("payload after SetBlobInfo: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(st.Root, "blobs"))
	if len(entries) != 2 {
		t.Errorf("blobs dir has %d entries, want payload + meta", len(entries))
	}
	// Dedupe upload under a different name keeps the stored ext.
	dup, err := st.PutBlob(bytes.NewReader(payload), "other.json")
	if err != nil || dup.Ext != "jsonfile" || dup.Path != b.Path {
		t.Errorf("dedupe = %+v (%v)", dup, err)
	}
	// A legacy meta that claims ext "json" (its payload was clobbered by the
	// meta itself) must not resolve to the meta file: it is simply not found,
	// so the upload path stores it again.
	stale := *again
	stale.Ext = "json"
	stale.Info = nil
	data, _ := json.MarshalIndent(&stale, "", "  ")
	os.WriteFile(st.metaPath(b.Hash), data, 0o644)
	os.Remove(b.Path)
	if _, err := st.GetBlob(b.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("legacy json ext: err = %v, want ErrNotFound", err)
	}
	re, err := st.PutBlob(bytes.NewReader(payload), "recipe.json")
	if err != nil || re.Ext != "jsonfile" || re.Path != b.Path {
		t.Errorf("re-upload = %+v (%v)", re, err)
	}
	if got, _ := os.ReadFile(b.Path); !bytes.Equal(got, payload) {
		t.Errorf("re-uploaded payload: %q", got)
	}
}

// TestPutBlobSeqStaysAFileBlob: Ext "seq" means "Path is the frame directory"
// (sequence.go), so a plain upload named "*.seq" must be stored under another
// extension — otherwise Blob.IsSequence would report true for a regular file.
func TestPutBlobSeqStaysAFileBlob(t *testing.T) {
	st := newTestStore(t)
	payload := []byte("not a frame directory")
	b, err := st.PutBlob(bytes.NewReader(payload), "frames.seq")
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if b.Ext != "seqfile" {
		t.Errorf("ext = %q, want seqfile", b.Ext)
	}
	if b.IsSequence() {
		t.Error("IsSequence() true for a plain file upload")
	}
	if want := filepath.Join(st.Root, "blobs", b.Hash+".seqfile"); b.Path != want {
		t.Errorf("path = %q, want %q", b.Path, want)
	}
	got, err := st.GetBlob(b.Hash)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if got.IsSequence() || got.Ext != "seqfile" || got.Path != b.Path {
		t.Errorf("GetBlob = %+v", got)
	}
	if data, err := os.ReadFile(got.Path); err != nil || !bytes.Equal(data, payload) {
		t.Errorf("payload: %q (%v)", data, err)
	}
	// A legacy meta claiming Ext "seq" for a plain file (an upload from before
	// SanitizeExt reserved the extension) must not surface as a sequence: the
	// payload shape disagrees, so the blob is simply not found and the upload
	// path stores it again under the truthful extension.
	stale := *got
	stale.Ext = "seq"
	data, _ := json.MarshalIndent(&stale, "", "  ")
	if err := os.WriteFile(st.metaPath(b.Hash), data, 0o644); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(st.Root, "blobs", b.Hash+".seq")
	if err := os.Rename(b.Path, legacyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(b.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("legacy seq-ext file blob: err = %v, want ErrNotFound", err)
	}
	re, err := st.PutBlob(bytes.NewReader(payload), "frames.seq")
	if err != nil || re.Ext != "seqfile" || re.IsSequence() || re.Path != b.Path {
		t.Errorf("re-upload = %+v (%v)", re, err)
	}
	if data, _ := os.ReadFile(b.Path); !bytes.Equal(data, payload) {
		t.Errorf("re-uploaded payload: %q", data)
	}
}

func TestTouchBlob(t *testing.T) {
	st := newTestStore(t)
	b, err := st.PutBlob(strings.NewReader("touch me"), "clip.mov")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(b.Path, old, old)
	os.Chtimes(st.metaPath(b.Hash), old, old)
	before := time.Now().Add(-time.Minute)
	if err := st.TouchBlob(b.Hash); err != nil {
		t.Fatalf("TouchBlob: %v", err)
	}
	fi, err := os.Stat(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Before(before) {
		t.Errorf("blob mtime %v not refreshed", fi.ModTime())
	}
	// The meta is left alone (its mtime means "uploaded / probed").
	if mfi, _ := os.Stat(st.metaPath(b.Hash)); mfi.ModTime().After(before) {
		t.Errorf("meta mtime %v was touched", mfi.ModTime())
	}
	// Unknown / invalid hashes are not errors.
	if err := st.TouchBlob(strings.Repeat("e", 64)); err != nil {
		t.Errorf("unknown hash: %v", err)
	}
	if err := st.TouchBlob("nope"); err != nil {
		t.Errorf("invalid hash: %v", err)
	}
	// A touched blob survives a TTL sweep that would otherwise remove it.
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(b.Hash); err != nil {
		t.Errorf("touched blob swept: %v", err)
	}
}

// TestSweepKeepsFreshMeta: a blob whose meta was written less than an hour
// ago (just uploaded, or just probed) is never removed by the TTL pass — not
// even under a TTL shorter than that hour — nor by the size pass.
func TestSweepKeepsFreshMeta(t *testing.T) {
	st := newTestStore(t)
	old := time.Now().Add(-3 * time.Hour)

	// Payload old, meta fresh: kept.
	fresh, _ := st.PutBlob(strings.NewReader("payload old, meta fresh"), "a.gif")
	os.Chtimes(fresh.Path, old, old)
	// Payload old, meta old: swept.
	stale, _ := st.PutBlob(strings.NewReader("payload old, meta old"), "b.gif")
	os.Chtimes(stale.Path, old, old)
	os.Chtimes(st.metaPath(stale.Hash), old, old)

	if err := st.Sweep(context.Background(), time.Minute, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := st.GetBlob(fresh.Hash); err != nil {
		t.Errorf("blob with fresh meta swept by TTL: %v", err)
	}
	if _, err := st.GetBlob(stale.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob with old meta survived: %v", err)
	}
	// Size pass with a cap of 1 byte: the fresh-meta blob still survives.
	if err := st.Sweep(context.Background(), 0, 1); err != nil {
		t.Fatalf("Sweep size: %v", err)
	}
	if _, err := st.GetBlob(fresh.Hash); err != nil {
		t.Errorf("blob with fresh meta swept by size: %v", err)
	}
	// Once the meta is old too, TTL removes it.
	os.Chtimes(st.metaPath(fresh.Hash), old, old)
	if err := st.Sweep(context.Background(), time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBlob(fresh.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob with old payload+meta survived: %v", err)
	}
	// metaFresh itself.
	now := time.Now()
	if (blobEntry{}).metaFresh(now) {
		t.Error("entry without meta reported fresh")
	}
	if !(blobEntry{metaMtime: now.Add(-time.Minute)}).metaFresh(now) || (blobEntry{metaMtime: now.Add(-2 * time.Hour)}).metaFresh(now) {
		t.Error("metaFresh threshold")
	}
}

func TestResultCommit(t *testing.T) {
	st := newTestStore(t)
	hash := strings.Repeat("c", 64)

	if st.HasResult(hash) {
		t.Fatal("HasResult true before commit")
	}
	if st.HasResult("nope") {
		t.Fatal("HasResult true for invalid hash")
	}
	if got, want := st.ResultDir(hash), filepath.Join(st.Root, "results", hash); got != want {
		t.Errorf("ResultDir = %q, want %q", got, want)
	}

	// Staging without a manifest is rejected.
	stage0 := filepath.Join(t.TempDir(), "s0")
	os.MkdirAll(stage0, 0o755)
	os.WriteFile(filepath.Join(stage0, "out.gif"), []byte("GIF89a"), 0o644)
	if err := st.CommitResult(hash, stage0); err == nil {
		t.Fatal("commit without manifest succeeded")
	}
	if st.HasResult(hash) {
		t.Fatal("partial result became visible")
	}

	// Real commit from a scratch dir.
	stage, cleanup, err := st.ScratchDir("job-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	os.WriteFile(filepath.Join(stage, "out.gif"), []byte("GIF89a-first"), 0o644)
	os.WriteFile(filepath.Join(stage, ManifestName), []byte(`{"v":1}`), 0o644)
	if err := st.CommitResult(hash, stage); err != nil {
		t.Fatalf("CommitResult: %v", err)
	}
	if !st.HasResult(hash) {
		t.Fatal("HasResult false after commit")
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging dir still exists after commit: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(st.ResultDir(hash), "out.gif"))
	if string(got) != "GIF89a-first" {
		t.Errorf("committed content = %q", got)
	}

	// A second commit for the same hash keeps the existing result and
	// consumes the new staging dir.
	stage2 := filepath.Join(t.TempDir(), "s2")
	os.MkdirAll(stage2, 0o755)
	os.WriteFile(filepath.Join(stage2, "out.gif"), []byte("GIF89a-second"), 0o644)
	os.WriteFile(filepath.Join(stage2, ManifestName), []byte(`{"v":2}`), 0o644)
	if err := st.CommitResult(hash, stage2); err != nil {
		t.Fatalf("CommitResult #2: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(st.ResultDir(hash), "out.gif"))
	if string(got) != "GIF89a-first" {
		t.Errorf("second commit replaced result: %q", got)
	}
	if _, err := os.Stat(stage2); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging #2 not consumed: %v", err)
	}
	// No leftover .staging-* dirs.
	entries, _ := os.ReadDir(filepath.Join(st.Root, "results"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Errorf("leftover staging dir %s", e.Name())
		}
	}
}

func TestCopyTreeCommitPath(t *testing.T) {
	// Exercise the copy fallback directly (rename across filesystems cannot
	// be forced in a unit test).
	src := t.TempDir()
	dst := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("BB"), 0o644)
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "A" {
		t.Errorf("a.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); string(got) != "BB" {
		t.Errorf("sub/b.txt = %q", got)
	}
}

func TestScratchDir(t *testing.T) {
	st := newTestStore(t)
	dir, cleanup, err := st.ScratchDir("../evil/../id")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != st.Scratch {
		t.Errorf("scratch dir %q escaped %q", dir, st.Scratch)
	}
	if filepath.Base(dir) != "evilid" {
		t.Errorf("sanitised id = %q", filepath.Base(dir))
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	cleanup()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cleanup left %s", dir)
	}
	if _, _, err := st.ScratchDir("///"); err == nil {
		t.Error("empty id accepted")
	}
}

// putResult writes a complete result dir with the given size and mtime.
func putResult(t *testing.T, st *Store, hash string, size int, mtime time.Time) {
	t.Helper()
	dir := st.ResultDir(hash)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "out.gif"), bytes.Repeat([]byte("x"), size), 0o644)
	os.WriteFile(filepath.Join(dir, ManifestName), []byte("{}"), 0o644)
	for _, f := range []string{"out.gif", ManifestName} {
		os.Chtimes(filepath.Join(dir, f), mtime, mtime)
	}
	os.Chtimes(dir, mtime, mtime)
}

func TestSweepTTL(t *testing.T) {
	st := newTestStore(t)
	old := time.Now().Add(-48 * time.Hour)

	oldBlob, _ := st.PutBlob(strings.NewReader("old blob"), "old.gif")
	os.Chtimes(oldBlob.Path, old, old)
	os.Chtimes(st.metaPath(oldBlob.Hash), old, old)
	newBlob, _ := st.PutBlob(strings.NewReader("new blob"), "new.gif")

	oldRes := strings.Repeat("1", 64)
	newRes := strings.Repeat("2", 64)
	putResult(t, st, oldRes, 10, old)
	putResult(t, st, newRes, 10, time.Now())

	// In-progress dir: no manifest, fresh.
	inProg := st.ResultDir(strings.Repeat("3", 64))
	os.MkdirAll(inProg, 0o755)
	os.WriteFile(filepath.Join(inProg, "out.gif"), []byte("partial"), 0o644)
	// Junk dir: no manifest, ancient.
	junk := st.ResultDir(strings.Repeat("4", 64))
	os.MkdirAll(junk, 0o755)
	os.WriteFile(filepath.Join(junk, "out.gif"), []byte("partial"), 0o644)
	os.Chtimes(filepath.Join(junk, "out.gif"), old, old)
	os.Chtimes(junk, old, old)
	// Abandoned upload temp file.
	tmpOld := filepath.Join(st.Root, "tmp", "upload-abandoned.part")
	os.WriteFile(tmpOld, []byte("x"), 0o644)
	os.Chtimes(tmpOld, old, old)
	tmpNew := filepath.Join(st.Root, "tmp", "upload-live.part")
	os.WriteFile(tmpNew, []byte("x"), 0o644)

	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := st.GetBlob(oldBlob.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("old blob survived: %v", err)
	}
	if _, err := os.Stat(st.metaPath(oldBlob.Hash)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old blob meta survived")
	}
	if _, err := st.GetBlob(newBlob.Hash); err != nil {
		t.Errorf("new blob deleted: %v", err)
	}
	if st.HasResult(oldRes) {
		t.Error("old result survived")
	}
	if !st.HasResult(newRes) {
		t.Error("new result deleted")
	}
	if _, err := os.Stat(inProg); err != nil {
		t.Error("in-progress result dir deleted")
	}
	if _, err := os.Stat(junk); !errors.Is(err, os.ErrNotExist) {
		t.Error("junk result dir survived")
	}
	if _, err := os.Stat(tmpOld); !errors.Is(err, os.ErrNotExist) {
		t.Error("abandoned tmp file survived")
	}
	if _, err := os.Stat(tmpNew); err != nil {
		t.Error("live tmp file deleted")
	}
}

func TestSweepSize(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	hashes := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	// Oldest first: a (3 h), b (2 h), c (1 h); 100 bytes each (+2 bytes manifest).
	for i, h := range hashes {
		putResult(t, st, h, 100, now.Add(-time.Duration(3-i)*time.Hour))
	}
	// A blob that is old enough to be evictable (200 bytes) and a fresh one.
	oldBlob, _ := st.PutBlob(bytes.NewReader(bytes.Repeat([]byte("o"), 200)), "old.bin")
	oldT := now.Add(-5 * time.Hour)
	os.Chtimes(oldBlob.Path, oldT, oldT)
	os.Chtimes(st.metaPath(oldBlob.Hash), oldT, oldT)
	freshBlob, _ := st.PutBlob(bytes.NewReader(bytes.Repeat([]byte("f"), 200)), "fresh.bin")

	// TTL disabled (0); cap tight enough to need the two oldest results gone.
	// Totals: results 3*102 = 306, blobs 2*(200+meta). Cap = leaves c + blobs.
	blobMetaSize := func(h string) int64 {
		fi, _ := os.Stat(st.metaPath(h))
		return fi.Size()
	}
	blobsTotal := 400 + blobMetaSize(oldBlob.Hash) + blobMetaSize(freshBlob.Hash)
	cap := blobsTotal + 102
	if err := st.Sweep(context.Background(), 0, cap); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if st.HasResult(hashes[0]) || st.HasResult(hashes[1]) {
		t.Error("oldest results not evicted")
	}
	if !st.HasResult(hashes[2]) {
		t.Error("newest result evicted")
	}
	if _, err := st.GetBlob(oldBlob.Hash); err != nil {
		t.Error("blob evicted although results sufficed")
	}

	// Now a cap smaller than the blobs alone: evict old blob, keep fresh one.
	if err := st.Sweep(context.Background(), 0, 150); err != nil {
		t.Fatalf("Sweep #2: %v", err)
	}
	if st.HasResult(hashes[2]) {
		t.Error("last result should be evicted under a tiny cap")
	}
	if _, err := st.GetBlob(oldBlob.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("old blob should be evicted: %v", err)
	}
	if _, err := st.GetBlob(freshBlob.Hash); err != nil {
		t.Errorf("fresh blob must survive the size pass: %v", err)
	}
}

func TestSweepCancelled(t *testing.T) {
	st := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := st.Sweep(ctx, time.Hour, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Sweep with cancelled ctx: %v", err)
	}
}
