package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// newIOManager wires a manager with real input/output dirs.
func newIOManager(t *testing.T) (m *Manager, st *store.Store, inDir, outDir string) {
	t.Helper()
	st = newTestStore(t)
	inDir = t.TempDir()
	outDir = t.TempDir()
	m = NewManager(st, fakeTools, Options{Concurrency: 1, InputDir: inDir, OutputDir: outDir})
	return m, st, inDir, outDir
}

// commitFakeResult writes a manifest-complete result dir for hash with one
// primary out.gif carrying data.
func commitFakeResult(t *testing.T, st *store.Store, hash, srcHash string, data []byte) {
	t.Helper()
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "out.gif"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	res := Result{
		RecipeHash: hash,
		Recipe:     recipe.Recipe{Sources: []string{srcHash}, Output: recipe.Output{Format: "gif"}},
		Files: []File{
			{Name: "out.gif", Format: "gif", Kind: FileKindOutput},
		},
		Created: time.Now().UTC(),
	}
	man, err := json.Marshal(&res)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, store.ManifestName), man, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitResult(hash, staging); err != nil {
		t.Fatal(err)
	}
}

// TestIODisabled: without usable dirs every entry point answers the
// dedicated sentinel (the server maps it to 503) and the capability flags
// are off.
func TestIODisabled(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	if m.InputPickEnabled() || m.OutputSaveEnabled() {
		t.Error("features must be off without dirs")
	}
	if _, err := m.ListInput(); !errors.Is(err, ErrNoInputDir) {
		t.Errorf("ListInput err = %v", err)
	}
	if _, err := m.SourceFromInput("x.gif"); !errors.Is(err, ErrNoInputDir) {
		t.Errorf("SourceFromInput err = %v", err)
	}
	if _, err := m.SaveResult(hexHash("r"), "out.gif", ""); !errors.Is(err, ErrNoOutputDir) {
		t.Errorf("SaveResult err = %v", err)
	}

	// A configured but missing directory is the same as none.
	m2 := NewManager(st, fakeTools, Options{Concurrency: 1,
		InputDir:  filepath.Join(t.TempDir(), "missing-in"),
		OutputDir: filepath.Join(t.TempDir(), "missing-out")})
	if m2.InputPickEnabled() || m2.OutputSaveEnabled() {
		t.Error("missing dirs must disable the features")
	}
}

// hexHash derives a deterministic recipe-hash-shaped string from s.
func hexHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestListInput: decodable extensions only, regular files only, no zero-byte
// files, sorted by name.
func TestListInput(t *testing.T) {
	m, _, inDir, _ := newIOManager(t)
	if !m.InputPickEnabled() {
		t.Fatal("input picker must be on")
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(inDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b-clip.mov", []byte("mov bytes"))
	write("a-anim.gif", []byte("GIF89a..."))
	write("notes.txt", []byte("not decodable"))
	write("empty.gif", nil)
	if err := os.MkdirAll(filepath.Join(inDir, "sub.gif"), 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := m.ListInput()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "a-anim.gif" || files[1].Name != "b-clip.mov" {
		t.Fatalf("listing = %+v", files)
	}
	if files[0].Size != 9 || files[0].MTime.IsZero() {
		t.Errorf("entry facts = %+v", files[0])
	}
}

// TestSourceFromInput: exact-name ingest through store.PutBlob (sha256
// dedupe onto an already-uploaded blob), traversal and unknown names are
// ErrNotFound.
func TestSourceFromInput(t *testing.T) {
	m, st, inDir, _ := newIOManager(t)
	data := animatedGIF(t)
	if err := os.WriteFile(filepath.Join(inDir, "anim.gif"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	blob, err := m.SourceFromInput("anim.gif")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if blob.Hash != hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %s, want the content hash", blob.Hash)
	}
	if blob.Info != nil {
		t.Error("a fresh ingest has no probe info yet (the server probes it)")
	}

	// The same bytes uploaded first dedupe onto the same blob, probe info
	// intact.
	st2 := newTestStore(t)
	m2 := NewManager(st2, fakeTools, Options{Concurrency: 1, InputDir: inDir})
	uploaded, err := st2.PutBlob(bytes.NewReader(data), "upload.gif")
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.SetBlobInfo(uploaded.Hash, gifSourceInfo()); err != nil {
		t.Fatal(err)
	}
	again, err := m2.SourceFromInput("anim.gif")
	if err != nil {
		t.Fatal(err)
	}
	if again.Hash != uploaded.Hash {
		t.Errorf("dedupe: %s != %s", again.Hash, uploaded.Hash)
	}
	if again.Info == nil {
		t.Error("deduped blob keeps its probe info")
	}

	for _, name := range []string{"../anim.gif", "..\\anim.gif", "nope.gif", "anim.gif/", ""} {
		if _, err := m.SourceFromInput(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("SourceFromInput(%q) err = %v, want ErrNotFound", name, err)
		}
	}
	_ = st
}

// TestSourceFromInputTooLarge: Options.MaxUploadBytes caps the pick exactly
// like the HTTP upload path — an over-limit file is ErrInputTooLarge and
// nothing is ingested; a file at/under the cap ingests normally, and a
// manager without the cap (0) ingests anything.
func TestSourceFromInputTooLarge(t *testing.T) {
	st := newTestStore(t)
	inDir := t.TempDir()
	data := animatedGIF(t)
	if err := os.WriteFile(filepath.Join(inDir, "anim.gif"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, fakeTools, Options{Concurrency: 1, InputDir: inDir, MaxUploadBytes: int64(len(data)) - 1})
	_, err := m.SourceFromInput("anim.gif")
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("over-limit pick err = %v, want ErrInputTooLarge", err)
	}
	sum := sha256.Sum256(data)
	if _, err := st.GetBlob(hex.EncodeToString(sum[:])); err == nil {
		t.Error("over-limit pick must not ingest the file")
	}

	at := NewManager(st, fakeTools, Options{Concurrency: 1, InputDir: inDir, MaxUploadBytes: int64(len(data))})
	if _, err := at.SourceFromInput("anim.gif"); err != nil {
		t.Errorf("pick at exactly the cap err = %v", err)
	}
	uncapped := NewManager(st, fakeTools, Options{Concurrency: 1, InputDir: inDir})
	if _, err := uncapped.SourceFromInput("anim.gif"); err != nil {
		t.Errorf("uncapped pick err = %v", err)
	}
}

// TestVerifyOpenedInput: the list→open race guard of SourceFromInput. A
// plain regular file passes; a path whose entry is (or became) a symlink is
// rejected even though the opened handle points at a perfectly regular
// target; a file swapped for another one after the open fails the
// os.SameFile identity check.
func TestVerifyOpenedInput(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "plain.gif")
	if err := os.WriteFile(regular, []byte("GIF89a bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("regular file passes", func(t *testing.T) {
		f, err := os.Open(regular)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		st, err := verifyOpenedInput(f, regular)
		if err != nil {
			t.Fatalf("verify err = %v", err)
		}
		if st.Size() != int64(len("GIF89a bytes")) {
			t.Errorf("size = %d", st.Size())
		}
	})

	t.Run("symlink is rejected", func(t *testing.T) {
		link := filepath.Join(dir, "link.gif")
		if err := os.Symlink(regular, link); err != nil {
			// Windows without Developer Mode / privilege cannot create
			// symlinks; the Lstat branch is then covered by the swap case.
			t.Skipf("cannot create symlinks here: %v", err)
		}
		f, err := os.Open(link) // follows the link to the regular target
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := verifyOpenedInput(f, link); err == nil {
			t.Fatal("a symlinked entry must be rejected")
		}
	})

	t.Run("swapped file is rejected", func(t *testing.T) {
		// A real swap (rename over the still-open victim) is not portable —
		// Windows denies replacing a file with an open handle — so the
		// post-swap state is staged directly: the handle holds one regular
		// file while the path names another. Same os.SameFile branch.
		other := filepath.Join(dir, "other.gif")
		if err := os.WriteFile(other, []byte("impostor"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(regular)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := verifyOpenedInput(f, other); err == nil {
			t.Fatal("a file swapped in after the open must be rejected")
		}
	})
}

// TestSourceFromInputSymlink: end to end, a symlink in /input is never
// pickable — the listing filter hides it, so the pick is ErrNotFound.
func TestSourceFromInputSymlink(t *testing.T) {
	m, _, inDir, _ := newIOManager(t)
	target := filepath.Join(t.TempDir(), "secret.gif")
	if err := os.WriteFile(target, animatedGIF(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(inDir, "sneaky.gif")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if files, err := m.ListInput(); err != nil || len(files) != 0 {
		t.Errorf("listing = %v, %v (symlinks are never listed)", files, err)
	}
	if _, err := m.SourceFromInput("sneaky.gif"); !errors.Is(err, ErrNotFound) {
		t.Errorf("picking a symlink err = %v, want ErrNotFound", err)
	}
}

// TestSaveResult: default naming from the main source (the ?dl=1 download
// name), explicit base names, collision-safe suffixes, byte-identical
// copies, unknown result/file errors.
func TestSaveResult(t *testing.T) {
	m, st, _, outDir := newIOManager(t)
	src := putSource(t, st, true) // blob name clip.mov → stem "clip"
	hash := hexHash("result-1")
	payload := []byte("GIF89a fake result bytes")
	commitFakeResult(t, st, hash, src, payload)

	name, err := m.SaveResult(hash, "out.gif", "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "clip.gif" {
		t.Errorf("default save name = %q, want clip.gif (the download name)", name)
	}
	got, err := os.ReadFile(filepath.Join(outDir, name))
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("saved bytes differ (err %v)", err)
	}

	// A second identical save must land under a different name.
	name2, err := m.SaveResult(hash, "out.gif", "")
	if err != nil {
		t.Fatal(err)
	}
	if name2 != "clip-2.gif" {
		t.Errorf("second save name = %q, want clip-2.gif", name2)
	}
	if got, err := os.ReadFile(filepath.Join(outDir, name2)); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("second saved bytes differ (err %v)", err)
	}

	// Explicit base names, with or without the extension.
	if name, err = m.SaveResult(hash, "out.gif", "myemote"); err != nil || name != "myemote.gif" {
		t.Errorf("explicit base: %q, %v", name, err)
	}
	if name, err = m.SaveResult(hash, "out.gif", "myemote.gif"); err != nil || name != "myemote-2.gif" {
		t.Errorf("explicit base with ext: %q, %v", name, err)
	}

	// Unknown file / result.
	if _, err := m.SaveResult(hash, "manifest.json", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("manifest.json must not be saveable: %v", err)
	}
	if _, err := m.SaveResult(hash, "nope.gif", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown file err = %v", err)
	}
	if _, err := m.SaveResult(hexHash("no-such-result"), "out.gif", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown result err = %v", err)
	}
	if _, err := m.SaveResult("not-a-hash", "out.gif", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed hash err = %v", err)
	}
}
