// Package matte is the app side of the AI matte sidecar
// (docs/background-removal-proposal.md §7): an HTTP client for its
// endpoints (ping, matte, warm, unload and the Phase 5c track / track-frame
// pair), the memo keys jobs files mattes under (§4.2), the app's persisted
// copy of the sidecar's last successful ping (§4.1 step 1 — memo hits need
// no live sidecar) and the per-clip manifest (§4.1 step 8).
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

// Model kinds as reported by /v1/ping (ModelState.Kind, Phase 5c). A
// sidecar that predates the field reports "", which is a segmenter.
const (
	KindSegmenter = "segmenter" // a per-frame matting model (isnet-anime, birefnet-lite): POST /v1/matte
	KindTracker   = "tracker"   // a prompted video segmenter (sam2-tiny): POST /v1/track
)

// Ping is the body of GET /v1/ping (spec §7.2 plus defaultModel and the
// Phase 5c device fields). One sidecar process can serve several devices
// (the cuda image carries the CPU EP too): Devices lists them, Device /
// DefaultDevice is the one a request without device= runs on, and the
// per-model top-level fields mirror that default device while
// ModelState.Devices carries every device's own state.
type Ping struct {
	Protocol          int                   `json:"protocol"`
	Version           string                `json:"version"`                 // the sidecar image tag
	Instance          string                `json:"instance"`                // random id per sidecar start
	ProcessingVersion string                `json:"processingVersion"`       // keys the memo ("proc")
	Device            string                `json:"device"`                  // the default device: cuda | cpu | unavailable
	Reason            string                `json:"reason"`                  // why Device is unavailable
	Devices           []string              `json:"devices,omitempty"`       // every device the process offers, e.g. ["cuda","cpu"]; empty before Phase 5c
	DefaultDevice     string                `json:"defaultDevice,omitempty"` // the device a request without device= runs on (Device, when it is offered)
	DefaultModel      string                `json:"defaultModel"`            // the default model of the default device
	DefaultModels     map[string]string     `json:"defaultModels,omitempty"` // per device: {"cuda": "birefnet-lite", "cpu": "isnet-anime"}
	GPU               *GPUInfo              `json:"gpu"`                     // nil on CPU
	Models            map[string]ModelState `json:"models"`                  // the models MATTE_MODELS offers
	Busy              int                   `json:"busy"`                    // batches in flight (other app instances)
}

// GPUInfo describes the sidecar's GPU.
type GPUInfo struct {
	Name     string  `json:"name"`
	TotalGiB float64 `json:"totalGiB"`
	FreeGiB  float64 `json:"freeGiB"`
}

// ModelState is one entry of Ping.Models. State, Reason, Percent,
// Precision, Sizes, DefaultSize, MsPerFrame and Resident describe the model
// on the ping's DEFAULT device (the pre-5c shape, kept for compatibility);
// Devices holds the same facts per offered device (On picks one).
type ModelState struct {
	State       string                 `json:"state"`              // ready | loading | downloading | missing | unavailable
	Reason      string                 `json:"reason"`             // why missing / unavailable
	Percent     int                    `json:"percent"`            // download / load progress
	Weights     string                 `json:"weights"`            // sha256 of the pinned source file — a static pin, reported in every state
	GraphDigest string                 `json:"graphDigest"`        // sha256 of the loaded (derived) graph; forensics only
	Precision   string                 `json:"precision"`          // fp16 | fp32 of the graph the sidecar runs
	Sizes       []int                  `json:"sizes"`              // accepted input squares
	DefaultSize int                    `json:"defaultSize"`        // the size used when a request says 0
	MsPerFrame  map[string]float64     `json:"msPerFrame"`         // per size, as a decimal string key ("1024")
	Licence     string                 `json:"licence"`            // e.g. Apache-2.0
	LastError   string                 `json:"lastError"`          // last download / load failure
	Label       string                 `json:"label,omitempty"`    // UI label when the sidecar reports one ("Anime (fast)")
	Kind        string                 `json:"kind,omitempty"`     // KindSegmenter ("" too) | KindTracker
	Resident    bool                   `json:"resident,omitempty"` // a session is loaded on the default device right now
	Devices     map[string]DeviceState `json:"devices,omitempty"`  // per offered device; nil from a pre-5c sidecar
}

// DeviceState is a model's state on one device (ModelState.Devices): the
// device's own graph choice (isnet: fp16 1024 on cuda, fp32 512 on cpu),
// its measured speed and whether a session is resident there.
type DeviceState struct {
	State      string             `json:"state"`      // ready | loading | downloading | missing | unavailable
	Reason     string             `json:"reason"`     // why missing / unavailable on this device
	Percent    int                `json:"percent"`    // download / load progress
	Precision  string             `json:"precision"`  // fp16 | fp32 of the graph run on this device
	Size       int                `json:"size"`       // the default input square on this device (a request's size 0)
	Sizes      []int              `json:"sizes"`      // accepted input squares on this device
	MsPerFrame map[string]float64 `json:"msPerFrame"` // per size, as a decimal string key ("1024")
	Resident   bool               `json:"resident"`   // a session is loaded on this device right now
}

// Model returns the state of model id and whether the sidecar offers it.
func (p *Ping) Model(id string) (ModelState, bool) {
	if p == nil || p.Models == nil {
		return ModelState{}, false
	}
	s, ok := p.Models[id]
	return s, ok
}

// EffectiveDevice resolves a request device: "" (the default) is the
// sidecar's DefaultDevice — Device for a pre-5c sidecar that reports no
// DefaultDevice — and anything else is returned as given (validate it with
// Offers).
func (p *Ping) EffectiveDevice(device string) string {
	if device != "" || p == nil {
		return device
	}
	if p.DefaultDevice != "" {
		return p.DefaultDevice
	}
	return p.Device
}

// Offers reports whether the sidecar runs requests on device, "" standing
// for the default device (EffectiveDevice). A 5c sidecar lists its devices;
// a pre-5c one offers exactly its Device when that is cuda or cpu.
func (p *Ping) Offers(device string) bool {
	if p == nil {
		return false
	}
	device = p.EffectiveDevice(device)
	if len(p.Devices) > 0 {
		for _, d := range p.Devices {
			if d == device {
				return true
			}
		}
		return false
	}
	return device != "" && device == p.Device && (device == DeviceCUDA || device == DeviceCPU)
}

// DefaultModelFor returns the sidecar's default model on device ("" = the
// default device): DefaultModels' entry, else DefaultModel (a pre-5c
// sidecar, or an unknown device).
func (p *Ping) DefaultModelFor(device string) string {
	if p == nil {
		return ""
	}
	if m := p.DefaultModels[p.EffectiveDevice(device)]; m != "" {
		return m
	}
	return p.DefaultModel
}

// IsTracker reports whether the model is a prompted video segmenter
// (KindTracker) — used through Client.Track rather than Client.Matte.
func (s ModelState) IsTracker() bool { return s.Kind == KindTracker }

// On returns the model's state on device and whether the sidecar reports
// one. "" is the default device, answered from the mirrored top-level
// fields; so is any device when the sidecar predates the per-device map
// (nil Devices — validate the device with Ping.Offers first). A named
// device missing from a reported map is not offered for this model.
func (s ModelState) On(device string) (DeviceState, bool) {
	if device == "" || s.Devices == nil {
		return s.Default(), true
	}
	d, ok := s.Devices[device]
	return d, ok
}

// Default is the model's state on the ping's default device, read from
// the top-level fields (the pre-5c shape every sidecar keeps reporting).
func (s ModelState) Default() DeviceState {
	return DeviceState{State: s.State, Reason: s.Reason, Percent: s.Percent, Precision: s.Precision,
		Size: s.DefaultSize, Sizes: s.Sizes, MsPerFrame: s.MsPerFrame, Resident: s.Resident}
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
func (s ModelState) HasSize(size int) bool { return hasSize(s.Sizes, size) }

// MsPerFrameAt returns the measured milliseconds per frame at size (0 = the
// default size), or 0 when the sidecar has not reported one.
func (s ModelState) MsPerFrameAt(size int) float64 {
	return msPerFrameAt(s.MsPerFrame, s.EffectiveSize(size))
}

// EffectiveSize maps a request size to the one this device will run: 0 is
// the device's default Size, anything else is returned as given.
func (d DeviceState) EffectiveSize(size int) int {
	if size == 0 {
		return d.Size
	}
	return size
}

// HasSize reports whether size is one of the device's accepted input squares.
func (d DeviceState) HasSize(size int) bool { return hasSize(d.Sizes, size) }

// MsPerFrameAt returns the device's measured milliseconds per frame at size
// (0 = its default size), or 0 when the sidecar has not reported one.
func (d DeviceState) MsPerFrameAt(size int) float64 {
	return msPerFrameAt(d.MsPerFrame, d.EffectiveSize(size))
}

func hasSize(sizes []int, size int) bool {
	for _, v := range sizes {
		if v == size {
			return true
		}
	}
	return false
}

func msPerFrameAt(ms map[string]float64, size int) float64 {
	if size <= 0 || ms == nil {
		return 0
	}
	return ms[itoa(size)]
}
