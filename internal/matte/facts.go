package matte

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FactsVersion is the file format of Facts; LoadFacts refuses any other.
const FactsVersion = 1

// FactsName is the file the app keeps its facts in, under <root>/mattes/.
const FactsName = "models.json"

// Facts is the app's persisted copy of the last successful /v1/ping
// (spec §4.1 step 1): per model the pinned weights sha256, the sidecar's
// processingVersion, sizes, precision and msPerFrame per device. ClipKey
// and MatteParams.Resolved are made from these, never from a live answer,
// so a memo hit needs no running sidecar — a restart, a download, a 20 s
// session create or an operator who stopped the sidecar never blacks out
// previews or renders whose mattes exist. Rewritten on every probe that
// answers.
type Facts struct {
	V     int       `json:"v"`
	Saved time.Time `json:"saved"` // when the ping was answered (UTC)
	Ping            // the answer itself, flattened into the file
}

// ErrFactsVersion is returned by LoadFacts for a file written under another
// FactsVersion (treated like a missing file: the next probe rewrites it).
var ErrFactsVersion = fmt.Errorf("matte: facts file version is not %d", FactsVersion)

// LoadFacts reads the facts file. A missing file is an error satisfying
// errors.Is(err, fs.ErrNotExist) — "the matte service has not answered yet".
func LoadFacts(path string) (*Facts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Facts
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("matte: %s: %w", path, err)
	}
	if f.V != FactsVersion {
		return nil, fmt.Errorf("%w: %s has v%d", ErrFactsVersion, path, f.V)
	}
	return &f, nil
}

// SaveFacts writes p as the facts file, atomically (temp file in the same
// directory, then rename), creating the directory when needed.
func SaveFacts(path string, p *Ping) error {
	if p == nil {
		return fmt.Errorf("matte: SaveFacts: nil ping")
	}
	f := Facts{V: FactsVersion, Saved: now().UTC().Truncate(time.Second), Ping: *p}
	data, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// now is time.Now, replaceable by tests.
var now = time.Now

// writeAtomic writes data to a temp file beside path and renames it over
// path, creating the parent directory. The temp name starts with the
// target's base name, never ".tmp-", which is the sweeper's abandoned-pass
// prefix under <root>/mattes (spec §4.4).
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
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
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
