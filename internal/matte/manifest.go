package matte

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ManifestVersion is the file format of Manifest; ReadManifest refuses any
// other (an unreadable manifest drops the memo, spec §12).
const ManifestVersion = 1

// ManifestName is the manifest's file name inside a clip dir.
const ManifestName = "matte.json"

// FramePattern is the image2 pattern of a clip dir's PNGs, numbered from 1
// (-start_number 1): "000001.png" is the first frame of the plan's grid.
const FramePattern = "%06d.png"

// FrameFile is the file name of frame n (1-based) under FramePattern.
func FrameFile(n int) string { return fmt.Sprintf(FramePattern, n) }

// Manifest is <root>/mattes/<key>/matte.json (spec §4.1 step 8): what the
// sequence was made from and with. Weights/Proc are the key's (from the
// persisted facts); GraphDigest, Device and MsPerFrame are forensics.
type Manifest struct {
	V           int       `json:"v"`
	Key         string    `json:"key"`         // ClipKey
	Src         string    `json:"src"`         // the source blob hash
	Model       string    `json:"model"`       // model id
	Weights     string    `json:"weights"`     // pinned weights sha256
	Proc        string    `json:"proc"`        // sidecar processingVersion
	GraphDigest string    `json:"graphDigest"` // sha256 of the loaded graph
	Precision   string    `json:"precision"`   // fp16 / fp32
	Size        int       `json:"size"`        // input square
	FPS         string    `json:"fps"`         // the plan's fps text; jobs compares it string-exact
	Frames      int       `json:"frames"`      // PNGs in the dir, 000001.png .. FrameFile(Frames)
	Device      string    `json:"device"`      // cuda / cpu
	MsPerFrame  float64   `json:"msPerFrame"`  // the sidecar's figure at the time
	Created     time.Time `json:"created"`
}

// ErrManifestVersion is returned by ReadManifest for a file written under
// another ManifestVersion.
var ErrManifestVersion = fmt.Errorf("matte: manifest version is not %d", ManifestVersion)

// ReadManifest reads and validates a clip dir's manifest. A missing file is
// an error satisfying errors.Is(err, fs.ErrNotExist).
func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("matte: %s: %w", path, err)
	}
	if m.V != ManifestVersion {
		return nil, fmt.Errorf("%w: %s has v%d", ErrManifestVersion, path, m.V)
	}
	if m.Frames <= 0 {
		return nil, fmt.Errorf("matte: %s: frames %d", path, m.Frames)
	}
	return &m, nil
}

// WriteManifest writes m atomically (temp file beside path, then rename),
// stamping V = ManifestVersion and a zero Created with the current time.
// The caller's struct is not modified.
func WriteManifest(path string, m *Manifest) error {
	if m == nil {
		return fmt.Errorf("matte: WriteManifest: nil manifest")
	}
	c := *m
	c.V = ManifestVersion
	if c.Created.IsZero() {
		c.Created = now().UTC().Truncate(time.Second)
	}
	data, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}
