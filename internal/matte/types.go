// Package matte is the app side of the AI matte sidecar
// (docs/background-removal-proposal.md §7): an HTTP client for its three
// endpoints, the memo keys jobs files mattes under (§4.2), the app's
// persisted copy of the sidecar's last successful ping (§4.1 step 1 — memo
// hits need no live sidecar) and the per-clip manifest (§4.1 step 8).
//
// The package is stdlib-only and process-free: it speaks plain HTTP/1.1
// to the sidecar and reads/writes small JSON files, nothing else. It never
// decodes a PNG — matte records are opaque bytes handed to the caller.
package matte

// Protocol is the sidecar protocol this client speaks. A ping reporting a
// different Protocol is "update the matte service" (jobs decides; the
// client only reports what it read).
const Protocol = 1

// Model states as reported by /v1/ping (spec §7.2).
const (
	StateReady       = "ready"
	StateLoading     = "loading"
	StateDownloading = "downloading"
	StateMissing     = "missing"
	StateUnavailable = "unavailable"
)

// Devices as reported by /v1/ping.
const (
	DeviceCUDA        = "cuda"
	DeviceCPU         = "cpu"
	DeviceUnavailable = "unavailable"
)

// Ping is the body of GET /v1/ping (spec §7.2 plus defaultModel).
type Ping struct {
	Protocol          int                   `json:"protocol"`
	Version           string                `json:"version"`           // the sidecar image tag
	Instance          string                `json:"instance"`          // random id per sidecar start
	ProcessingVersion string                `json:"processingVersion"` // keys the memo ("proc")
	Device            string                `json:"device"`            // cuda | cpu | unavailable
	Reason            string                `json:"reason"`            // why Device is unavailable
	DefaultModel      string                `json:"defaultModel"`      // MATTE_DEFAULT_MODEL
	GPU               *GPUInfo              `json:"gpu"`               // nil on CPU
	Models            map[string]ModelState `json:"models"`            // the models MATTE_MODELS offers
	Busy              int                   `json:"busy"`              // batches in flight (other app instances)
}

// GPUInfo describes the sidecar's GPU.
type GPUInfo struct {
	Name     string  `json:"name"`
	TotalGiB float64 `json:"totalGiB"`
	FreeGiB  float64 `json:"freeGiB"`
}

// ModelState is one entry of Ping.Models.
type ModelState struct {
	State       string             `json:"state"`           // ready | loading | downloading | missing | unavailable
	Reason      string             `json:"reason"`          // why missing / unavailable
	Percent     int                `json:"percent"`         // download / load progress
	Weights     string             `json:"weights"`         // sha256 of the pinned source ONNX — a static pin, reported in every state
	GraphDigest string             `json:"graphDigest"`     // sha256 of the loaded (derived) graph; forensics only
	Precision   string             `json:"precision"`       // fp16 | fp32 of the graph the sidecar runs
	Sizes       []int              `json:"sizes"`           // accepted input squares
	DefaultSize int                `json:"defaultSize"`     // the size used when a request says 0
	MsPerFrame  map[string]float64 `json:"msPerFrame"`      // per size, as a decimal string key ("1024")
	Licence     string             `json:"licence"`         // e.g. Apache-2.0
	LastError   string             `json:"lastError"`       // last download / load failure
	Label       string             `json:"label,omitempty"` // UI label when the sidecar reports one ("Anime (fast)")
}

// Model returns the state of model id and whether the sidecar offers it.
func (p *Ping) Model(id string) (ModelState, bool) {
	if p == nil || p.Models == nil {
		return ModelState{}, false
	}
	s, ok := p.Models[id]
	return s, ok
}

// EffectiveSize maps a request size to the one the sidecar will run: 0 is
// the model's DefaultSize, anything else is returned as given (validate it
// with HasSize).
func (s ModelState) EffectiveSize(size int) int {
	if size == 0 {
		return s.DefaultSize
	}
	return size
}

// HasSize reports whether size is one of the model's accepted input squares.
func (s ModelState) HasSize(size int) bool {
	for _, v := range s.Sizes {
		if v == size {
			return true
		}
	}
	return false
}

// MsPerFrameAt returns the measured milliseconds per frame at size (0 = the
// default size), or 0 when the sidecar has not reported one.
func (s ModelState) MsPerFrameAt(size int) float64 {
	size = s.EffectiveSize(size)
	if size <= 0 || s.MsPerFrame == nil {
		return 0
	}
	return s.MsPerFrame[itoa(size)]
}
