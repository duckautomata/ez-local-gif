package jobs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Phase 4 /input picker and /output save (phase4 design §3, DESIGN.md §4.4):
// two host directories the container may mount — Options.InputDir (read
// only; EZLG_INPUT, default /input) to pick sources from, and
// Options.OutputDir (EZLG_OUTPUT, default /output) to save result files to.
// Both are checked ONCE by NewManager (exists + readable / writable) and
// reported by InputPickEnabled / OutputSaveEnabled, which the server exposes
// as /api/capabilities features.inputPick / features.outputSave; the input
// listing itself is read per request.

// ErrNoInputDir is returned by ListInput/SourceFromInput when Options.InputDir
// was not a readable directory at startup (the server answers 503).
var ErrNoInputDir = errors.New("jobs: no input directory is available")

// ErrNoOutputDir is returned by SaveResult when Options.OutputDir was not a
// writable directory at startup (the server answers 503).
var ErrNoOutputDir = errors.New("jobs: no output directory is available")

// ErrInputTooLarge is returned by SourceFromInput for a file over
// Options.MaxUploadBytes (the server answers 413, exactly like an upload of
// the same bytes).
var ErrInputTooLarge = errors.New("jobs: input file is too large")

// MaxInputFiles caps the ListInput listing.
const MaxInputFiles = 500

// maxSaveAttempts bounds SaveResult's collision-safe naming loop.
const maxSaveAttempts = 10000

// inputExtensions are the file extensions (lowercase, without the dot)
// ListInput shows: what the upload path can decode — images ffmpeg reads
// and the common video containers.
var inputExtensions = map[string]bool{
	"gif": true, "webp": true, "png": true, "apng": true, "avif": true,
	"jpg": true, "jpeg": true, "bmp": true,
	"mp4": true, "m4v": true, "mov": true, "mkv": true, "webm": true,
	"avi": true, "mpg": true, "mpeg": true, "wmv": true, "flv": true,
	"ts": true, "mts": true, "m2ts": true,
}

// InputFile is one entry of ListInput. MTime marshals as an RFC 3339
// timestamp (time.Time's JSON encoding).
type InputFile struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// InputPickEnabled reports whether Options.InputDir was a readable directory
// when the manager was created (the /api/capabilities features.inputPick
// flag).
func (m *Manager) InputPickEnabled() bool { return m.inputPick }

// OutputSaveEnabled reports whether Options.OutputDir was a writable
// directory when the manager was created (features.outputSave).
func (m *Manager) OutputSaveEnabled() bool { return m.outputSave }

// dirReadable reports whether dir is a directory this process can list.
func dirReadable(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	_, err = os.ReadDir(dir)
	return err == nil
}

// dirWritable reports whether dir is a directory this process can write to
// (proved with a throw-away probe file, like store.New does for /data).
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	f, err := os.CreateTemp(dir, ".ezlg-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// ListInput lists the pickable files of Options.InputDir: regular files with
// a decodable extension and a non-zero size, flat (no recursion), sorted by
// name, capped at MaxInputFiles. ErrNoInputDir when the picker is off.
func (m *Manager) ListInput() ([]InputFile, error) {
	if !m.inputPick {
		return nil, ErrNoInputDir
	}
	entries, err := os.ReadDir(m.opts.InputDir)
	if err != nil {
		return nil, fmt.Errorf("jobs: list input dir: %w", err)
	}
	files := make([]InputFile, 0, min(len(entries), MaxInputFiles))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
		if !inputExtensions[ext] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		files = append(files, InputFile{Name: name, Size: info.Size(), MTime: info.ModTime().UTC()})
		if len(files) >= MaxInputFiles {
			break
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// SourceFromInput ingests one /input file into the blob store exactly like
// an upload of the same bytes (store.PutBlob: sha256 content address, so a
// file that was already uploaded dedupes onto the same hash). name must
// EXACTLY equal a ListInput entry — anything else, a traversal attempt
// ("../x") included, is ErrNotFound; a file over Options.MaxUploadBytes is
// ErrInputTooLarge (the same cap the HTTP upload path enforces with 413).
// The returned blob's Info is nil when the bytes were never probed; the
// server completes the ingest through its usual upload path (probe +
// SetBlobInfo + the source answer), exactly as it does for POST
// /api/sources/from-result.
func (m *Manager) SourceFromInput(name string) (*store.Blob, error) {
	if !m.inputPick {
		return nil, ErrNoInputDir
	}
	files, err := m.ListInput()
	if err != nil {
		return nil, err
	}
	listed := false
	for _, f := range files {
		if f.Name == name {
			listed = true
			break
		}
	}
	if !listed {
		return nil, fmt.Errorf("%w: %q is not a file in the input directory", ErrNotFound, name)
	}
	full := filepath.Join(m.opts.InputDir, name)
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q vanished from the input directory", ErrNotFound, name)
		}
		return nil, fmt.Errorf("jobs: open input file %q: %w", name, err)
	}
	defer f.Close()
	// Both checks run on the OPENED file (not the listing, which may be
	// stale): verifyOpenedInput guards the list→open race — a symlink or
	// another file swapped in after the ListInput check above — and the size
	// cap cannot be raced by a rewrite between list and pick.
	st, err := verifyOpenedInput(f, full)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not the listed regular file: %v", ErrNotFound, name, err)
	}
	if limit := m.opts.MaxUploadBytes; limit > 0 && st.Size() > limit {
		return nil, fmt.Errorf("%w: %q is %d bytes, over the %d byte upload limit", ErrInputTooLarge, name, st.Size(), limit)
	}
	blob, err := m.st.PutBlob(f, name)
	if err != nil {
		return nil, fmt.Errorf("jobs: ingest input file %q: %w", name, err)
	}
	return blob, nil
}

// verifyOpenedInput guards SourceFromInput's list→open window: ListInput
// only lists regular files (symlinks are filtered by Type().IsRegular()),
// but on a writable /input mount the entry can be swapped for a symlink —
// e.g. into /data/blobs — between the listing and the Open, and os.Open
// follows symlinks (O_NOFOLLOW is not portable to Windows, so the handle is
// verified after the fact instead). The opened handle must be a regular
// file, and a fresh Lstat of path must still be a regular file (a symlink
// swapped in before the open fails here — the link itself is not regular)
// that IS the file the handle holds (a swap after the open fails the
// os.SameFile check). Fails closed on every race; returns the handle's
// FileInfo for the size cap.
func verifyOpenedInput(f *os.File, path string) (os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("the opened file is not a regular file")
	}
	li, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, errors.New("the name no longer names a regular file (a symlink?)")
	}
	if !os.SameFile(li, fi) {
		return nil, errors.New("the file changed between listing and open")
	}
	return fi, nil
}

// SaveResult copies one result file into Options.OutputDir ("Save to
// /output") and returns the name it was written under. file must be a name
// the result's manifest lists (so manifest.json/report.json cannot be
// saved); name is an optional base name WITHOUT the extension (sanitised;
// "" derives the same friendly name a ?dl=1 download gets: the main
// source's stem for the primary output, "<stem>-<file>" for everything
// else). The write is collision-safe — base.ext, base-2.ext, base-3.ext, …
// — and race-free (O_EXCL), so two identical saves return two different
// names. Errors: ErrNoOutputDir when saving is off, ErrNotFound for an
// unknown result or file.
func (m *Manager) SaveResult(recipeHash, file, name string) (string, error) {
	if !m.outputSave {
		return "", ErrNoOutputDir
	}
	res, err := m.LoadResult(recipeHash)
	if err != nil {
		return "", err
	}
	var found *File
	for i := range res.Files {
		if res.Files[i].Name == file {
			found = &res.Files[i]
			break
		}
	}
	if found == nil {
		return "", fmt.Errorf("%w: this result has no file %q", ErrNotFound, file)
	}
	src := filepath.Join(m.st.ResultDir(recipeHash), file)
	if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("%w: result file %q is missing", ErrNotFound, file)
	}

	final := m.defaultSaveName(res, file, found)
	if base := strings.TrimSpace(store.SanitizeName(name)); name != "" && base != "" {
		// The caller's base may or may not carry the extension already
		// ("myclip" and "myclip.gif" both save as myclip.gif).
		if e := path.Ext(base); e != "" && strings.EqualFold(e, path.Ext(file)) {
			base = strings.TrimSuffix(base, e)
		}
		if base != "" {
			final = base + path.Ext(file)
		}
	}
	ext := path.Ext(final)
	stem := strings.TrimSuffix(final, ext)
	if stem == "" {
		stem = strings.TrimSuffix(file, ext)
	}
	return m.writeCollisionSafe(src, stem, ext)
}

// defaultSaveName mirrors the server's ?dl=1 download naming: the main
// source's stem alone for the primary output ("myclip.gif"), the stem in
// front of the result name for every other file ("myclip-alt1.gif",
// "myclip-f00012.png"); the result file name itself when the source is
// unknown.
func (m *Manager) defaultSaveName(res *Result, file string, f *File) string {
	if len(res.Recipe.Sources) == 0 {
		return file
	}
	blob, err := m.st.GetBlob(res.Recipe.Sources[0])
	if err != nil {
		return file
	}
	stem := strings.TrimSpace(strings.TrimSuffix(blob.Name, path.Ext(blob.Name)))
	if stem == "" {
		return file
	}
	if f.Kind == "" || f.Kind == FileKindOutput {
		return stem + path.Ext(file)
	}
	return stem + "-" + file
}

// writeCollisionSafe copies src into the output dir as stem+ext, or
// stem-2+ext, stem-3+ext, … when the name is taken; O_EXCL makes the claim
// atomic against concurrent saves. It returns the final name.
func (m *Manager) writeCollisionSafe(src, stem, ext string) (string, error) {
	for i := 1; i <= maxSaveAttempts; i++ {
		name := stem + ext
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(m.opts.OutputDir, name)
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", fmt.Errorf("jobs: save %q: %w", name, err)
		}
		in, err := os.Open(src)
		if err != nil {
			out.Close()
			os.Remove(dst)
			return "", fmt.Errorf("jobs: save %q: %w", name, err)
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
			return "", fmt.Errorf("jobs: save %q: %w", name, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("jobs: could not find a free name for %s%s after %d attempts", stem, ext, maxSaveAttempts)
}
