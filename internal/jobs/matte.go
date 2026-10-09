package jobs

// Phase 5b: the AI matte pass (docs/background-removal-proposal.md §4, §6,
// §11, §12). A recipe's matte op (recipe.OpMatte) is served from a memoised
// image2 sequence under <data>/mattes/<clipKey>/ — one 8-bit gray PNG per
// master frame on the plan's output grid — and this file is everything
// that puts it there and finds it again:
//
//   - the SIDECAR STATE: a probe loop (RunMatteProbe, every 30 s) keeps the
//     last /v1/ping answer for MatteStatus / MatteEnabled (features.matte
//     flips off after matteProbeFailures consecutive misses, transitions
//     only are logged) and persists every successful answer as the FACTS
//     (<data>/mattes/models.json, matte.SaveFacts): per model the pinned
//     weights sha256, the sidecar's processingVersion, sizes, precision and
//     msPerFrame. Keys and MatteParams.Resolved are made from the facts
//     (loadMatteFacts / identityFor), never from a live answer, so a memo
//     hit needs no running sidecar;
//   - resolveMattes: the callers' entry point (previews inside compile, the
//     render pre-stage renderMattes, ResolveAutoCropFor) — lists the
//     stack's distinct (model, size) requests (matteRequests, the
//     compiler's dedupe), compiles the matte input plan
//     (graph.CompileMatteInput: its FPSText keys the memo), keys the memo
//     (matteClipKey), serves a hit, else refuses up-front from the frame
//     count and the persisted msPerFrame of the EFFECTIVE device (Phase
//     5c: the preference, matte_5c.go), and otherwise — only for an eager
//     preview or a render; a plain preview answers MattePendingIdle, or
//     follows a pass somebody else started — runs or joins the PASS under
//     m.mt.flight.doDetachedAbandon (one per clip key; cancelled when
//     nobody has waited for matteAbandonGrace). A preview waits
//     mattePreviewWait and then answers *ErrMattePending from the pass's
//     progress; a render waits under the job ctx, mirroring the progress
//     into the ctx's withMatteProgress listener. A tracker request
//     (the guided model) runs runTrackPass instead, the edge model's own
//     pass after it, and the gate / stabilise derives follow either
//     (resolvedMatte.SeqDir);
//   - runMattePass: waits for the model (loading / downloading are pending
//     states, missing / unavailable refuse with the sidecar's reason), then
//     streams enc.MatteSourceArgs (ffrun.RunFrom: read at the consumer's
//     pace, never cut by exec's WaitDelay) through a batchWriter that cuts
//     exact size×size×3-byte frames, hashes each (matte.FrameKey), copies
//     frames-store hits into the clip dir at once and POSTs the distinct
//     misses in batches of matteBatchFrames; every PNG is filed in the
//     frames store (tmp+rename) and under <tmpDir>/%06d.png; the manifest
//     is written and the tmp dir renamed into place atomically. A POST
//     failure, the run-time frame cap or the hard timeout cancels the
//     producer first (ffmpeg dies at once) and the plain error — never a
//     context error — reaches the waiters;
//   - the op-level plumbing the rest of jobs calls (render.go's
//     renderMattes pre-stage, phase3.go's compile, autocrop.go, still.go,
//     proxy.go, Submit): fillMatteInputs (Path / Frames / the string-exact
//     fps check), fillMatteResolved / stripMatteResolved /
//     checkMatteResolved (MatteParams.Resolved in the recipe hash),
//     matteKeySuffix (still / proxy / autocrop keys), checkMatteCount
//     (after renderMaster), protectMattes, applyMatteInfo (the render.matte
//     info check), withMatteProgress / matteProgressFn and the StageMatte
//     message helpers. These names and shapes are the contract the
//     integration files and matte_integration_test.go are written against.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Matte defaults (Options.MatteMaxSeconds / MatteMaxFrames when unset).
const (
	// DefaultMatteMaxSeconds caps the ESTIMATED wall time of one matte pass
	// (frames x (msPerFrame + 2 ms) from the sidecar's measured rate); a
	// clip over it is refused up-front with an ErrInvalidRecipe asking to
	// trim, lower the fps or pick the fast model. EZLG_MATTE_MAX_SECONDS.
	DefaultMatteMaxSeconds = 600
	// DefaultMatteMaxFrames caps the frames one matte pass may stream to the
	// sidecar — up-front from Plan.Frames when it is known, and at run time
	// from the frames actually streamed (so an unknown count is bounded
	// too). EZLG_MATTE_MAX_FRAMES.
	DefaultMatteMaxFrames = 3000
)

// Pass tunables (spec §4.1, §4.3).
const (
	// matteBatchFrames is the number of distinct frames per POST /v1/matte
	// (8 × 3 MB rgb24 at 1024², 8 × 786 KB at 512²: one batch in flight).
	matteBatchFrames = 8
	// matteLoadTimeout bounds how long a pass waits for a model that is
	// downloading or loading before it fails.
	matteLoadTimeout = 300 * time.Second
	// matteProbeFailures is the number of consecutive failed probes after
	// which features.matte flips off (one failure is "degraded", logged).
	matteProbeFailures = 3
	// matteProbeInterval is the cadence of RunMatteProbe.
	matteProbeInterval = 30 * time.Second
	// matteProbeTimeout bounds one GET /v1/ping (the sidecar answers it from
	// a snapshot, never behind the inference lock) and one POST /v1/warm.
	matteProbeTimeout = 5 * time.Second
	// mattePingMaxAge is how old a probe answer may be for a pass to reuse
	// it instead of pinging again before its first POST.
	mattePingMaxAge = 15 * time.Second
	// matteFrameOverheadMS is the per-frame decode + hash cost the estimate
	// adds to the sidecar's msPerFrame.
	matteFrameOverheadMS = 2.0
	// matteBatchBaseTimeout and matteBatchPerFrame make the per-batch HTTP
	// timeout: base + frames × matteBatchPerFrame × msPerFrame, scaled by
	// (1 + busy) for the batches another app instance has in the sidecar.
	matteBatchBaseTimeout = 60 * time.Second
	matteBatchPerFrame    = 5.0
	// matteBusyCap bounds the busy scaling of the batch timeout.
	matteBusyCap = 8
	// matteInstanceHistory is how many sidecar instance ids the probe keeps
	// to tell a restart (one change) from two sidecars answering in turn.
	matteInstanceHistory = 4
	// matteProgressPoll is the cadence at which a render mirrors a running
	// pass's progress onto its job.
	matteProgressPoll = 250 * time.Millisecond
)

// Waits the tests shorten (the spec's mattePreviewWait, matteAbandonGrace and
// the re-ping cadence while a model loads).
var (
	// mattePreviewWait is how long a still / proxy waits for a running pass
	// before answering ErrMattePending (on a GPU a 45-frame pass is ~1 s,
	// so GPU users mostly never see a pending state).
	mattePreviewWait = 2 * time.Second
	// matteAbandonGrace is how long a pass runs on with no waiter before it
	// is cancelled (a polling SPA re-joins every 500 ms, inside it).
	matteAbandonGrace = 5 * time.Second
	// matteLoadPoll is the re-ping cadence while a model is loading /
	// downloading.
	matteLoadPoll = 2 * time.Second
	// matteIdleConnTimeout closes the client's idle keep-alive connections
	// to the sidecar — UNDER the sidecar's 30 s per-connection socket
	// timeout, so a non-replayable batch POST never races the sidecar's
	// close of a pooled connection (initMatte).
	matteIdleConnTimeout = 20 * time.Second
)

// Matte pending states (ErrMattePending.State / the 202 body's "state").
// MattePendingIdle (matte_5c.go) is the fourth: nothing started.
const (
	// MattePendingRunning: the pass is streaming frames to the sidecar
	// (Done / Total / Percent are live).
	MattePendingRunning = "running"
	// MattePendingLoading: the sidecar is creating the model's session.
	MattePendingLoading = "loading"
	// MattePendingDownloading: the sidecar is fetching the model's weights
	// (Percent is the download's).
	MattePendingDownloading = "downloading"
)

// ErrMattePending is returned (as *ErrMattePending; use errors.As) by the
// still and proxy renderers when a recipe's matte is not on disk yet and the
// pass did not finish within the preview wait: nothing was started because
// the request was not eager (State MattePendingIdle — the Compute matte
// button or Render starts the pass), the pass runs ("running"), or it
// waits on the sidecar loading or downloading the model. The server maps
// it to 202 Accepted with these fields; the SPA keeps the picture on stage
// and re-requests. Renders never see it — they wait under the job ctx with
// StageMatte progress instead.
type ErrMattePending struct {
	State      string // one of the MattePending* constants
	Phase      string // "" (a per-frame pass) or MattePhaseTracking: the guided model's one-POST track, worded "tracking N frames" while it runs
	Done       int    // frames matted so far (running)
	Total      int    // frames the pass will matte (0 = unknown)
	Percent    int    // 0..100: the pass's progress, or the download's
	Device     string // "cuda" / "cpu" — the effective device, for the pill text
	EstimateMS int64  // the pass's estimated total wall time in ms (0 = unknown)
	Reason     string // idle only (Phase 5d): why nothing can start from here, for the SPA — a mask prompt whose edge matte is not computed ("compute the General matte first …"); "" for the plain idle
}

// MattePhaseTracking is the ErrMattePending.Phase / matteProgress.Phase of
// a tracker (guided) pass: the whole clip goes to the sidecar in ONE POST
// and the masks come back together at its end, so the frame count cannot
// tick meanwhile — the progress reads "tracking N frames" instead of
// "0/N" for the length of the request (the 202 body's "phase", the
// render's StageMatte line and the SPA's pill / Compute button).
const MattePhaseTracking = "tracking"

// Error renders the pending state the way the SPA's pill does ("AI matte
// 24/45 · GPU", "AI matte: tracking 45 frames · GPU", "AI matte: loading
// model", "AI matte not computed", …).
func (e *ErrMattePending) Error() string {
	dev := matteDeviceLabel(e.Device)
	switch e.State {
	case MattePendingIdle:
		if e.Reason != "" {
			return "jobs: AI matte not computed — " + e.Reason
		}
		if e.EstimateMS > 0 {
			return fmt.Sprintf("jobs: AI matte not computed (~%s on %s) — press Compute matte or Render", humanSeconds(e.EstimateMS), dev)
		}
		return fmt.Sprintf("jobs: AI matte not computed on %s — press Compute matte or Render", dev)
	case MattePendingLoading:
		return "jobs: AI matte pending: loading model"
	case MattePendingDownloading:
		return fmt.Sprintf("jobs: AI matte pending: downloading weights %d %%", e.Percent)
	default:
		if e.tracking() {
			return fmt.Sprintf("jobs: AI matte pending: tracking %d frames · %s", e.Total, dev)
		}
		if e.Total > 0 {
			return fmt.Sprintf("jobs: AI matte pending: %d/%d · %s", e.Done, e.Total, dev)
		}
		return fmt.Sprintf("jobs: AI matte pending: %d frames · %s", e.Done, dev)
	}
}

// tracking reports a running tracker pass whose masks have not started to
// arrive (the whole clip is in the one POST): worded "tracking N frames".
func (e *ErrMattePending) tracking() bool {
	return e.Phase == MattePhaseTracking && e.Total > 0 && e.Done < e.Total
}

// matteDeviceLabel is the UI word for a sidecar device ("cuda" → "GPU").
func matteDeviceLabel(device string) string {
	switch device {
	case matte.DeviceCUDA:
		return "GPU"
	case matte.DeviceCPU:
		return "CPU"
	case "":
		return "the matte service"
	}
	return device
}

// humanSeconds renders a millisecond estimate as "12 s" / "3 min".
func humanSeconds(ms int64) string {
	s := (ms + 500) / 1000
	if s >= 120 {
		return fmt.Sprintf("%d min", (s+30)/60)
	}
	return fmt.Sprintf("%d s", s)
}

// ErrMatteUnavailable is returned when a recipe needs a matte that is not on
// disk and no sidecar can produce it: the feature is off (Options.MatteURL
// empty, or the probe has never answered), the sidecar reports its device
// or the model unavailable / missing, or it speaks another protocol. The
// wrapped message carries the sidecar's own reason when there is one; the
// server answers 400 for POST /api/jobs and names the compose profile.
var ErrMatteUnavailable = errors.New("jobs: the matte service is not available")

// MatteStatus is the app's view of the matte sidecar from its last probe —
// what GET /api/matte publishes (every field is JSON-tagged for it) and what
// the 202 pending body carries. Enabled is MatteEnabled(); with the feature
// off only Enabled, Reason (why), MaxSeconds and MaxFrames are meaningful.
type MatteStatus struct {
	Enabled       bool                        `json:"enabled"`
	Device        string                      `json:"device"`                  // the EFFECTIVE device passes run on (Phase 5c: the preference, else the sidecar's default) — "cuda" / "cpu" / "unavailable" / "" (never probed)
	Devices       []string                    `json:"devices,omitempty"`       // Phase 5c: every device the sidecar offers ("cuda", "cpu"); the "Run on" select shows only when there are several
	DefaultDevice string                      `json:"defaultDevice,omitempty"` // Phase 5c: the device a request without a preference runs on (so a client can tell "preference = default" from "no preference")
	Reason        string                      `json:"reason,omitempty"`        // why the feature or the device is off, for the UI
	GPU           *matte.GPUInfo              `json:"gpu,omitempty"`
	DefaultModel  string                      `json:"defaultModel"`            // the id the UI preselects (the sidecar's default for the effective device)
	DefaultModels map[string]string           `json:"defaultModels,omitempty"` // Phase 5c: the sidecar's default model per device ("cuda" → birefnet-lite, "cpu" → isnet-anime)
	Models        map[string]MatteModelStatus `json:"models"`                  // the models the sidecar offers, by id
	MaxSeconds    int                         `json:"maxSeconds"`              // Options.MatteMaxSeconds as applied
	MaxFrames     int                         `json:"maxFrames"`               // Options.MatteMaxFrames as applied
}

// MatteModelStatus is one offered model's live state in MatteStatus.Models.
// The top-level fields mirror the sidecar's DEFAULT device; Devices
// (Phase 5c) carries each offered device's own state and estimate.
type MatteModelStatus struct {
	Label      string                            `json:"label"`                // the UI label ("Anime (fast)", "General (precise)", "Guided (click to select)")
	Kind       string                            `json:"kind,omitempty"`       // Phase 5c: "segmenter" (per-frame matte) or "tracker" (the guided model); "" = segmenter
	State      string                            `json:"state"`                // ready / loading / downloading / missing / unavailable
	Percent    int                               `json:"percent,omitempty"`    // download / load progress for the transient states
	MsPerFrame float64                           `json:"msPerFrame,omitempty"` // the sidecar's measured ms per frame at its default size (0 = unknown)
	Reason     string                            `json:"reason,omitempty"`     // why the model is missing / unavailable
	Licence    string                            `json:"licence,omitempty"`
	Sizes      []int                             `json:"sizes,omitempty"`   // the input squares the sidecar accepts for it
	Resident   bool                              `json:"resident"`          // a session is loaded on the default device right now (Phase 5c: "ready" alone means downloaded and self-tested — the first pass adds the model load)
	Devices    map[string]MatteModelDeviceStatus `json:"devices,omitempty"` // Phase 5c: per offered device (matte_5c.go)
}

// RuleRenderMatte is the info-level check jobs appends to the primary
// report of a render whose recipe holds a matte op: which model, size,
// precision, weights and processing version the matte came from, the
// loaded graph's digest and the device / rate the pass ran at — forensics,
// never a verdict (spec §4.2). The SPA shows its detail as an "AI matte:"
// line.
const RuleRenderMatte = "render.matte"

// ---- state ------------------------------------------------------------------

// matteState is the manager's sidecar state (Manager.mt). mu guards every
// field but client and flight, which are set once.
type matteState struct {
	mu     sync.Mutex
	client *matte.Client // nil = no sidecar configured (Options.MatteURL "")

	// savedJSON is what the facts file holds (loaded at start), so a probe
	// that answers the same facts skips the rewrite.
	savedJSON []byte

	// The probe state: the last answer of any kind (a 503 body with
	// device "unavailable" counts for the UI's reason), when it arrived,
	// the last failure, the consecutive-failure count and the resulting
	// feature flag.
	live        *matte.Ping
	liveAt      time.Time
	liveErr     string
	failures    int
	probed      bool // at least one successful probe since start
	answered    bool // the LAST probe got a ping body (200, or the 503 device-unavailable answer)
	enabled     bool
	instances   []string // the last matteInstanceHistory instance ids
	alternating bool     // the two-sidecars warning is in force

	// The passes in flight, by clip key, and their progress for the 202
	// body / the render's StageMatte line.
	progress map[string]matteProgress
	flight   flight[matte.Manifest]

	// Phase 5c: the device preference (loadMatteSettings / SetMatteDevice;
	// prefSet tells a persisted "" reset from "never set", when
	// Options.MatteDevice applies) and the derives in flight, by derived
	// dir (deriveSequence).
	pref    string
	prefSet bool
	derives flight[string]
}

// matteProgress is one running pass's state for ErrMattePending and the
// render job's StageMatte message.
type matteProgress struct {
	State      string // MattePending* (never "idle" here)
	Phase      string // "" or MattePhaseTracking (a tracker pass)
	Done       int    // frames filed into the clip dir so far
	Total      int    // frames the pass will produce (0 = unknown)
	Percent    int    // 0..100 of the pass, or of the download
	Device     string
	EstimateMS int64
	Since      time.Time // when State was entered
}

// pending is the progress as the pending error / progress listener sees it.
func (p matteProgress) pending() ErrMattePending {
	return ErrMattePending{State: p.State, Phase: p.Phase, Done: p.Done, Total: p.Total, Percent: p.Percent, Device: p.Device, EstimateMS: p.EstimateMS}
}

// initMatte wires the sidecar client and seeds the facts bookkeeping
// (NewManager). A plain install (Options.MatteURL "") gets no client and
// probes nothing; a facts file from an earlier run with a sidecar still
// serves memo hits (loadMatteFacts reads it per request).
//
// The client gets a transport of its own whose idle keep-alive timeout
// (matteIdleConnTimeout) is UNDER the sidecar's per-connection socket
// timeout (30 s, SOCKET_TIMEOUT in sidecar/matte.py — its handler closes
// an idle connection after it): a batch POST is not replayable (an
// in-memory MultiReader body with no GetBody), so a POST that races the
// sidecar's FIN on a pooled connection it just closed — a pass whose
// frames all hit the frames store for over 30 s, then one miss — would
// fail as "sidecar unreachable" with the sidecar healthy. Closing idle
// connections first on our side means the pool never hands out one the
// sidecar is about to drop.
func (m *Manager) initMatte() {
	m.mt.progress = map[string]matteProgress{}
	if url := strings.TrimRight(m.opts.MatteURL, "/"); url != "" {
		m.mt.client = &matte.Client{BaseURL: url, HTTP: &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			IdleConnTimeout:     matteIdleConnTimeout,
			MaxIdleConnsPerHost: 2,
		}}}
	}
	f, err := matte.LoadFacts(matteFactsPath(m.st))
	switch {
	case err == nil:
		m.mt.savedJSON, _ = json.Marshal(&f.Ping)
	case errors.Is(err, fs.ErrNotExist):
	default:
		log.Printf("jobs: matte: facts file %s unreadable (%v); the next probe rewrites it", matteFactsPath(m.st), err)
	}
	m.loadMatteSettings()
}

// matteURL is the configured sidecar base URL ("" when none).
func (m *Manager) matteURL() string {
	if m.mt.client == nil {
		return ""
	}
	return m.mt.client.BaseURL
}

// matteFactsPath is the app's persisted copy of the sidecar's last
// successful ping, <Root>/mattes/models.json (matte.SaveFacts writes it,
// the store's sweeper leaves it alone). The mattes root is taken from the
// store's own layout (MatteDir) rather than spelled here.
func matteFactsPath(st *store.Store) string {
	return filepath.Join(filepath.Dir(st.MatteDir("facts")), matte.FactsName)
}

// loadMatteFacts reads the persisted facts. No facts ever written, or a
// file of another version, is ErrMatteUnavailable ("has not answered
// yet"); so is a ping of another protocol ("update the matte service").
func (m *Manager) loadMatteFacts() (*matte.Facts, error) {
	f, err := matte.LoadFacts(matteFactsPath(m.st))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, matte.ErrFactsVersion) {
			return nil, fmt.Errorf("%w: %s", ErrMatteUnavailable, m.noFactsReason())
		}
		return nil, fmt.Errorf("%w: reading its last answer: %v", ErrMatteUnavailable, err)
	}
	if f.Protocol != matte.Protocol {
		return nil, fmt.Errorf("%w: it speaks protocol %d, this server protocol %d — update the matte service", ErrMatteUnavailable, f.Protocol, matte.Protocol)
	}
	return f, nil
}

// noFactsReason explains a missing facts file.
func (m *Manager) noFactsReason() string {
	if m.mt.client == nil {
		return "AI mattes are off on this server (EZLG_MATTE_URL is empty) and the matte service has never answered"
	}
	return fmt.Sprintf("the matte service at %s has not answered yet — is the matte profile up?", m.matteURL())
}

// ---- status -----------------------------------------------------------------

// MatteStatus returns the sidecar's state as of the last probe (a snapshot;
// never blocks on the network). Without a sidecar (Options.MatteURL empty)
// it reports Enabled false with the reason and the caps.
func (m *Manager) MatteStatus() MatteStatus {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	s := &m.mt
	st := MatteStatus{
		Enabled:      s.enabled,
		DefaultModel: recipe.MatteModelDefault,
		Models:       map[string]MatteModelStatus{},
		MaxSeconds:   m.opts.MatteMaxSeconds,
		MaxFrames:    m.opts.MatteMaxFrames,
	}
	if s.client == nil {
		st.Reason = "no matte service is configured (EZLG_MATTE_URL is empty — start the matte or matte-gpu compose profile)"
		return st
	}
	if p := s.live; p != nil {
		st.GPU = p.GPU
		st.Devices = offeredDevices(p)
		if p.Device == matte.DeviceUnavailable {
			st.Device = matte.DeviceUnavailable
		} else {
			st.DefaultDevice = p.EffectiveDevice("")
			st.Device = effectiveDevice(p, s.prefLocked(m.opts))
		}
		if len(p.DefaultModels) > 0 {
			st.DefaultModels = make(map[string]string, len(p.DefaultModels))
			for dev, id := range p.DefaultModels {
				st.DefaultModels[dev] = id
			}
		} else if p.DefaultModel != "" && len(st.Devices) > 0 {
			st.DefaultModels = map[string]string{st.Devices[0]: p.DefaultModel}
		}
		if dm := p.DefaultModelFor(st.Device); dm != "" {
			st.DefaultModel = dm
		}
		for id, ms := range p.Models {
			st.Models[id] = matteModelStatus(id, ms)
		}
	}
	if !s.enabled {
		st.Reason = s.offReasonLocked()
	}
	return st
}

// MatteEnabled reports whether AI mattes can be produced right now: a
// sidecar is configured (Options.MatteURL), its probes answer (fewer than
// matteProbeFailures consecutive misses since the last answer) and its
// device is usable — features.matte in /api/capabilities. Previews of a
// recipe whose mattes are already on disk are served from the memo either
// way (the key needs no live sidecar); a RENDER needs the flag on — the
// server refuses POST /api/jobs with a matte op with 400 while it is off
// (the contract: a job must not be queued against a sidecar that is gone).
func (m *Manager) MatteEnabled() bool {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	return m.mt.enabled
}

// offReasonLocked says why the feature is off, for MatteStatus.Reason. A
// sidecar that answers with device "unavailable" (the matte-gpu profile on
// a box without nvidia-container-toolkit — a 503 ping whose body
// recordProbe keeps as the live answer without marking the sidecar probed)
// is reported FIRST: it did answer, and the headline must be its reason,
// not "has not answered yet" with the cause in brackets.
func (s *matteState) offReasonLocked() string {
	url := s.client.BaseURL
	switch {
	case s.answered && s.live != nil && s.live.Device == matte.DeviceUnavailable:
		if s.live.Reason != "" {
			return "the matte service's device is unavailable: " + s.live.Reason
		}
		return "the matte service's device is unavailable"
	case !s.probed:
		r := fmt.Sprintf("the matte service at %s has not answered yet — is the matte profile up?", url)
		if s.liveErr != "" {
			r += " (" + s.liveErr + ")"
		}
		return r
	case s.live != nil && s.live.Protocol != matte.Protocol:
		return fmt.Sprintf("update the matte service: it speaks protocol %d, this app protocol %d", s.live.Protocol, matte.Protocol)
	case s.failures >= matteProbeFailures:
		return fmt.Sprintf("the matte service at %s has not answered for %d probes (%s) — is the matte profile up?", url, s.failures, s.liveErr)
	case s.liveErr != "":
		return s.liveErr
	}
	return "the matte service is not available"
}

// matteModelStatus is the UI view of one model's ping entry.
func matteModelStatus(id string, ms matte.ModelState) MatteModelStatus {
	st := MatteModelStatus{
		Label:      ms.Label,
		State:      ms.State,
		Percent:    ms.Percent,
		MsPerFrame: ms.MsPerFrameAt(0),
		Reason:     ms.Reason,
		Licence:    ms.Licence,
		Sizes:      ms.Sizes,
		Resident:   ms.Resident,
	}
	if st.Label == "" {
		st.Label = matteModelLabel(id)
	}
	if st.Reason == "" && ms.LastError != "" && (ms.State == matte.StateMissing || ms.State == matte.StateUnavailable) {
		st.Reason = ms.LastError
	}
	// Phase 5c: the kind and the per-device states (a pre-5c sidecar
	// reports neither; the SPA then reads the top-level fields).
	st.Kind = ms.Kind
	if st.Kind == "" && id == recipe.MatteModelSAM2Tiny {
		st.Kind = matte.KindTracker
	}
	if len(ms.Devices) > 0 {
		st.Devices = make(map[string]MatteModelDeviceStatus, len(ms.Devices))
		for dev, d := range ms.Devices {
			ds := MatteModelDeviceStatus{State: d.State, Reason: d.Reason, Precision: d.Precision, Percent: d.Percent, Size: d.Size, MsPerFrame: d.MsPerFrameAt(0), Resident: d.Resident}
			if ds.Reason == "" && ms.LastError != "" && (d.State == matte.StateMissing || d.State == matte.StateUnavailable) {
				ds.Reason = ms.LastError
			}
			st.Devices[dev] = ds
		}
	}
	return st
}

// matteModelLabel is the UI label of a shipped model id when the sidecar
// reports none; other ids show as themselves.
func matteModelLabel(id string) string {
	switch id {
	case recipe.MatteModelISNetAnime:
		return "Anime (fast)"
	case recipe.MatteModelBiRefNetLite:
		return "General (precise)"
	case recipe.MatteModelSAM2Tiny:
		return "Guided (click to select)"
	}
	return id
}

// ---- the probe --------------------------------------------------------------

// RunMatteProbe probes the sidecar (GET /v1/ping) now and every
// matteProbeInterval until ctx ends, keeping MatteStatus / MatteEnabled
// current and the persisted facts fresh. It returns at once when no sidecar
// is configured. main runs it in a goroutine next to the sweeper.
func (m *Manager) RunMatteProbe(ctx context.Context) {
	if m.mt.client == nil {
		return
	}
	t := time.NewTicker(matteProbeInterval)
	defer t.Stop()
	for {
		m.probeMatte(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// probeMatte performs one probe and records it (recordProbe). The returned
// ping is nil on failure; the error is the transport / status error, or an
// ErrMatteUnavailable for a protocol mismatch.
func (m *Manager) probeMatte(ctx context.Context) (*matte.Ping, error) {
	if m.mt.client == nil {
		return nil, fmt.Errorf("%w: no matte service is configured (EZLG_MATTE_URL is empty)", ErrMatteUnavailable)
	}
	pctx, cancel := context.WithTimeout(ctx, matteProbeTimeout)
	defer cancel()
	p, err := m.mt.client.Ping(pctx)
	if err != nil && ctx.Err() != nil {
		// The CALLER's ctx ended (an abandoned pass pinging before its first
		// POST or polling a loading model, a cancelled job, the server
		// shutting down): that says nothing about the sidecar, so it is
		// neither a failed probe towards matteProbeFailures nor a change of
		// state — three abandoned previews must not flip features.matte off.
		return nil, ctx.Err()
	}
	if err = m.recordProbe(p, err); err != nil {
		return nil, err
	}
	return p, nil
}

// pingMatte returns a probe answer at most maxAge old (0 = always a fresh
// one), probing when needed. Passes use it before their first POST.
func (m *Manager) pingMatte(ctx context.Context, maxAge time.Duration) (*matte.Ping, error) {
	if maxAge > 0 {
		m.mt.mu.Lock()
		p, at, errText := m.mt.live, m.mt.liveAt, m.mt.liveErr
		m.mt.mu.Unlock()
		if p != nil && errText == "" && time.Since(at) < maxAge {
			return p, nil
		}
	}
	return m.probeMatte(ctx)
}

// recordProbe folds one probe's outcome into the state: a successful ping
// becomes the live answer and the facts (persisted when changed) and
// clears the failure count; a failure counts towards matteProbeFailures,
// and when its body is a ping (the sidecar's 503 with device
// "unavailable") that body becomes the live answer so the UI shows the
// reason. Only transitions are logged. Returns the effective error (a
// protocol mismatch is reported as one).
func (m *Manager) recordProbe(p *matte.Ping, err error) error {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	s := &m.mt
	wasOn, prev := s.enabled, s.live
	if err == nil && p != nil && p.Protocol != matte.Protocol {
		err = fmt.Errorf("%w: update the matte service: it speaks protocol %d, this app protocol %d", ErrMatteUnavailable, p.Protocol, matte.Protocol)
	}
	if err == nil && p != nil {
		s.live, s.liveAt, s.liveErr = p, time.Now(), ""
		s.failures = 0
		s.probed, s.answered = true, true
		s.enabled = p.Device == matte.DeviceCUDA || p.Device == matte.DeviceCPU
		m.persistFactsLocked(p)
	} else {
		s.failures++
		s.liveAt = time.Now()
		s.answered = false
		if err != nil {
			s.liveErr = err.Error()
		}
		switch {
		case p != nil:
			s.live, s.answered = p, true // the protocol-mismatch answer, for the reason
		default:
			var se *matte.StatusError
			if errors.As(err, &se) {
				if bp := pingFromBody(se.Body); bp != nil {
					if bp.Reason == "" {
						bp.Reason = se.Message
					}
					s.live, s.answered = bp, true
				}
			}
		}
		if s.failures >= matteProbeFailures {
			s.enabled = false
		}
	}
	s.logTransitionsLocked(wasOn, prev, err)
	return err
}

// pingFromBody decodes a ping the sidecar sent with a non-200 status (the
// 503 device-unavailable answer carries the full ping plus "error").
func pingFromBody(body []byte) *matte.Ping {
	if len(body) == 0 {
		return nil
	}
	var p matte.Ping
	if json.Unmarshal(body, &p) != nil || p.Protocol == 0 {
		return nil
	}
	return &p
}

// persistFactsLocked writes the facts file when its content changed.
func (m *Manager) persistFactsLocked(p *matte.Ping) {
	data, err := json.Marshal(p)
	if err != nil || bytes.Equal(data, m.mt.savedJSON) {
		return
	}
	if err := matte.SaveFacts(matteFactsPath(m.st), p); err != nil {
		log.Printf("jobs: matte: cannot persist the sidecar facts to %s: %v", matteFactsPath(m.st), err)
		return
	}
	m.mt.savedJSON = data
}

// logTransitionsLocked logs what changed with this probe: the feature
// flag, a first failure while on, a sidecar restart (instance change),
// two sidecars answering in turn, and model / device state changes.
func (s *matteState) logTransitionsLocked(wasOn bool, prev *matte.Ping, err error) {
	url := s.client.BaseURL
	p := s.live
	switch {
	case s.enabled && !wasOn:
		log.Printf("jobs: matte: AI mattes on — %s: device %s%s, models %s", url, p.Device, gpuName(p), modelSummary(p))
	case !s.enabled && wasOn:
		log.Printf("jobs: matte: AI mattes off — %s", s.offReasonLocked())
	case err != nil && wasOn && s.failures == 1:
		log.Printf("jobs: matte: probe of %s failed (%v); AI mattes stay on for %d more probes", url, err, matteProbeFailures-1)
	}
	if err == nil && p != nil && p.Instance != "" {
		s.instances = append(s.instances, p.Instance)
		if len(s.instances) > matteInstanceHistory {
			s.instances = s.instances[len(s.instances)-matteInstanceHistory:]
		}
		n := len(s.instances)
		alternating := n >= 3 && s.instances[n-1] != s.instances[n-2] && s.instances[n-1] == s.instances[n-3]
		steady := n >= 3 && s.instances[n-1] == s.instances[n-2] && s.instances[n-2] == s.instances[n-3]
		switch {
		case alternating && !s.alternating:
			log.Printf("jobs: matte: warning: two matte sidecars answer at %s — start only one of the matte / matte-gpu profiles", url)
			s.alternating = true
		case s.alternating && steady:
			s.alternating = false
		case !s.alternating && prev != nil && prev.Instance != "" && prev.Instance != p.Instance:
			log.Printf("jobs: matte: sidecar restarted (instance %s → %s, version %s)", prev.Instance, p.Instance, p.Version)
		}
	}
	if err == nil && p != nil && prev != nil {
		if prev.Device != p.Device {
			log.Printf("jobs: matte: device %s → %s%s", prev.Device, p.Device, reasonSuffix(p.Reason))
		}
		for _, id := range sortedModelIDs(p) {
			ms := p.Models[id]
			if was, ok := prev.Models[id]; !ok || was.State != ms.State {
				log.Printf("jobs: matte: model %s: %s%s", id, ms.State, reasonSuffix(ms.Reason))
			}
		}
	}
}

func gpuName(p *matte.Ping) string {
	if p == nil || p.GPU == nil || p.GPU.Name == "" {
		return ""
	}
	return " (" + p.GPU.Name + ")"
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// sortedModelIDs lists a ping's model ids in order.
func sortedModelIDs(p *matte.Ping) []string {
	if p == nil {
		return nil
	}
	ids := make([]string, 0, len(p.Models))
	for id := range p.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// modelSummary renders "isnet-anime ready, birefnet-lite loading".
func modelSummary(p *matte.Ping) string {
	ids := sortedModelIDs(p)
	if len(ids) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+" "+p.Models[id].State)
	}
	return strings.Join(parts, ", ")
}

// offeredModels renders a ping's model ids ("none" without any).
func offeredModels(p *matte.Ping) string {
	ids := sortedModelIDs(p)
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// ---- progress ---------------------------------------------------------------

func (m *Manager) setMatteProgress(key string, fn func(p *matteProgress)) {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	p := m.mt.progress[key]
	prev := p.State
	fn(&p)
	if p.State != prev || p.Since.IsZero() {
		p.Since = time.Now()
	}
	m.mt.progress[key] = p
}

func (m *Manager) clearMatteProgress(key string) {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	delete(m.mt.progress, key)
}

// matteProgressFor returns a running pass's progress.
func (m *Manager) matteProgressFor(key string) (matteProgress, bool) {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	p, ok := m.mt.progress[key]
	return p, ok
}

// matteProgressKey is the context key of withMatteProgress.
type matteProgressKey struct{}

// withMatteProgress returns a ctx carrying fn, which a render-mode
// resolveMattes calls with the pass's live state (the same shape as the
// pending error: state, done/total/percent, device, estimate) as it waits —
// what the job's StageMatte messages are made of (render.go's
// renderMattes installs one). A ctx without one has no listener
// (matteProgressFn returns nil).
func withMatteProgress(ctx context.Context, fn func(ErrMattePending)) context.Context {
	return context.WithValue(ctx, matteProgressKey{}, fn)
}

// matteProgressFn returns the listener withMatteProgress stored (nil when
// none).
func matteProgressFn(ctx context.Context) func(ErrMattePending) {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(matteProgressKey{}).(func(ErrMattePending))
	return fn
}

// matteProgressMessage is the StageMatte message of a progress report: the
// SPA shows it after the stage label ("AI matte · 24/45 · GPU"; a tracker
// pass "AI matte · tracking 45 frames · GPU" until its masks arrive).
func matteProgressMessage(p ErrMattePending) string {
	dev := matteDeviceLabel(p.Device)
	switch p.State {
	case MattePendingLoading:
		return "loading model"
	case MattePendingDownloading:
		return fmt.Sprintf("downloading weights %d %%", p.Percent)
	case MattePendingIdle:
		return "not computed"
	}
	if p.tracking() {
		return fmt.Sprintf("tracking %d frames · %s", p.Total, dev)
	}
	if p.Total > 0 {
		return fmt.Sprintf("%d/%d · %s", p.Done, p.Total, dev)
	}
	return fmt.Sprintf("%d frames · %s", p.Done, dev)
}

// matteProgressFraction maps a progress report onto 0..1 of the matte band
// (the load states sit at 0).
func matteProgressFraction(p ErrMattePending) float64 {
	switch {
	case p.State != MattePendingRunning && p.State != "":
		return 0
	case p.Total > 0:
		return min(max(float64(p.Done)/float64(p.Total), 0), 1)
	case p.Percent > 0:
		return min(max(float64(p.Percent)/100, 0), 1)
	}
	return 0
}

// ---- requests and identity --------------------------------------------------

// matteMode says how resolveMattes waits for a matte that is not on disk.
type matteMode int

const (
	// matteModePreview: a still / proxy — wait mattePreviewWait for a running
	// pass, then answer *ErrMattePending (the server's 202); without
	// MatteEager(ctx) no pass is ever started (Phase 5c: MattePendingIdle
	// at once, unless one is in flight to follow).
	matteModePreview matteMode = iota
	// matteModeRender: a render's pre-stage — wait under the job ctx (the
	// model's load states bounded by matteLoadTimeout), mirroring the
	// pass's progress into the ctx's withMatteProgress listener.
	matteModeRender
)

// resolvedMatte is one matte a stack's op resolved to: a complete memo on
// disk (Dir = store.MatteDir(ClipKey), Manifest its matte.json) for the
// identity (Model, Size, Precision + the manifest's Weights/Proc). ReqSize
// is the op's Size as written (0 = the server's default), which the plan's
// MatteInput.Size repeats: findMatte pairs a plan's matte inputs with their
// resolution by (Model, ReqSize) — and, Phase 5c, findMatteInput by the
// op's Stabilise / Prompts / Edge as well (the compiler's dedupe key).
// SeqDir is the directory the plan reads: Dir itself, or a derived
// sequence under it ("<Dir>/stab-<mode>/", "<Dir>/gated-<edge>-<edgeKey>-r3/",
// "<Dir>/gated-<edge>-<edgeKey>-r3/stab-<mode>/") when the op asks for one (seqDir).
type resolvedMatte struct {
	Model     string // the resolved model id
	Size      int    // the effective input square (0 for a tracker: the manifest's TrackW x TrackH)
	ReqSize   int    // the op's requested size (0 = the server's default); see findMatte
	Precision string // fp16 / fp32 / bf16
	Dir       string // the memo dir holding %06d.png + matte.json
	Manifest  *matte.Manifest
	ClipKey   string // matte.ClipKey — what still/proxy/autocrop keys fold in

	// Phase 5c.
	Stabilise string // the op's stabilise mode ("" = off): the derived sequence SeqDir names
	Prompts   string // graph.CanonicalMattePrompts of the op's prompts ("" without): the MatteInput identity
	ReqEdge   string // the op's Edge as written ("" / "none" / a model id): the MatteInput identity
	Edge      string // the RESOLVED edge model id of a tracker matte ("" = none, or a segmenter)
	SeqDir    string // the directory the plan reads ("" = Dir); see seqDir
	Device    string // the effective device the pass ran / would run on
	Keep      int    // keep colours of the op (the most of the ops sharing this request), for the info line

	edge *resolvedMatte // the edge model's own resolution (its memo), when Edge is set
	id   matteIdentity  // the identity resolved from the facts (zero for a literal built by hand)
}

// identity is the resolved matte's identity (the fields the memo key and
// MatteParams.Resolved are made of): the one resolveMatte recorded, with
// the manifest's weights / proc, or — for a resolvedMatte built without
// one — the plain fields.
func (r *resolvedMatte) identity() matteIdentity {
	id := r.id
	if id.Model == "" {
		id = matteIdentity{Model: r.Model, Size: r.Size, Precision: r.Precision}
	}
	if r.Manifest != nil {
		id.Weights, id.Proc = r.Manifest.Weights, r.Manifest.Proc
	}
	return id
}

// seqDir is the directory holding the sequence the plan reads.
func (r *resolvedMatte) seqDir() string {
	if r.SeqDir != "" {
		return r.SeqDir
	}
	return r.Dir
}

// matteRequest is one distinct request a stack asks a matte for — the
// compiler's dedupe key (model, size, stabilise, canonical prompts, edge as
// written; graph's matteKey) plus what the pass needs of the op. Model is
// the resolved id (recipe.MatteModelDefault for ""); Size is the op's
// request (0 = the server's default for its device).
type matteRequest struct {
	Model     string
	Size      int
	Stabilise string // "" / light / strong
	Prompts   string // graph.CanonicalMattePrompts (the MatteInput identity text)
	Edge      string // the op's Edge as written

	prompts []recipe.MattePrompt // the op's prompts (a tracker)
	keep    int                  // keep colours (the info line only; the picture is the graph's)
}

// same reports whether two requests are one input of the plan (the
// compiler's dedupe).
func (r matteRequest) same(o matteRequest) bool {
	return r.Model == o.Model && r.Size == o.Size && r.Stabilise == o.Stabilise && r.Prompts == o.Prompts && r.Edge == o.Edge
}

// matteRequestOf is the request of one decoded matte op.
func matteRequestOf(p recipe.MatteParams) matteRequest {
	return matteRequest{Model: p.Model, Size: p.Size, Stabilise: p.Stabilise, Prompts: graph.CanonicalMattePrompts(p.Prompts), Edge: p.Edge, prompts: p.Prompts, keep: len(p.Keep)}
}

// matteIdentity is what the persisted facts say a (model, size) renders
// with on the effective device — the fields of recipe.MatteResolved plus
// the model id and the facts the memo key needs. It is made from
// /data/mattes/models.json only, never from a live answer, so a memo hit
// needs no running sidecar. Phase 5c: Device is where the pass runs (never
// part of a key — Size / Precision, which differ per device, are), Kind
// tells a tracker, whose identity adds the tracking size and the canonical
// prompts (matte.TrackPrompts.Canonical) and, with an edge, the edge
// model's own identity.
type matteIdentity struct {
	Model      string
	Size       int // the effective input square (0 for a tracker)
	Precision  string
	Weights    string
	Proc       string
	Kind       string  // matte.KindSegmenter ("" too) / matte.KindTracker
	Device     string  // the effective device
	MsPerFrame float64 // the device's measured rate at Size (0 = unknown)

	// Tracker only.
	TrackW, TrackH int            // enc.TrackSize of the plan's frame (set once the plan is known)
	Prompts        string         // matte.TrackPrompts.Canonical() of the op's prompts
	Edge           *matteIdentity // the edge model (nil = none)
}

// tracker reports whether the identity is a tracker's (the guided model).
func (id matteIdentity) tracker() bool { return id.Kind == matte.KindTracker }

// resolved is the recipe.MatteResolved form of the identity.
func (id matteIdentity) resolved() recipe.MatteResolved {
	r := recipe.MatteResolved{Weights: id.Weights, Proc: id.Proc, Size: id.Size, Precision: id.Precision}
	if id.tracker() {
		r.Tracker = id.Weights
		if id.Edge != nil {
			r.Edge, r.EdgeWeights, r.EdgeProc = id.Edge.Model, id.Edge.Weights, id.Edge.Proc
		}
	}
	return r
}

// hasMatteOp reports whether ops hold a matte op.
func hasMatteOp(ops []recipe.Op) bool {
	return slices.ContainsFunc(ops, func(op recipe.Op) bool { return op.Kind == recipe.OpMatte })
}

// decodeMatteOp decodes a matte op's params (nil / empty = defaults),
// dropping a client-sent Resolved; the model id's syntax is the compiler's
// to validate (graph.CompileWithSources), a malformed JSON body or a
// negative size is reported here.
func decodeMatteOp(idx int, op recipe.Op) (recipe.MatteParams, error) {
	var p recipe.MatteParams
	if len(bytes.TrimSpace(op.Params)) > 0 && !bytes.Equal(bytes.TrimSpace(op.Params), []byte("null")) {
		if err := json.Unmarshal(op.Params, &p); err != nil {
			return p, fmt.Errorf("%w: op %d (matte): invalid params: %v", ErrInvalidRecipe, idx, err)
		}
	}
	p.Resolved = nil
	if p.Model == "" {
		p.Model = recipe.MatteModelDefault
	}
	if p.Size < 0 {
		return p, fmt.Errorf("%w: op %d (matte): size must be >= 0 (got %d)", ErrInvalidRecipe, idx, p.Size)
	}
	return p, nil
}

// matteRequests returns the distinct requests of ops in order of first use
// — the same dedupe the compiler applies (one ExtraInput per distinct
// model / size / stabilise / prompts / edge; matteRequest.same); nil when
// the stack has no matte op. Ops sharing a request pool their keep count.
func matteRequests(ops []recipe.Op) ([]matteRequest, error) {
	var reqs []matteRequest
	for i, op := range ops {
		if op.Kind != recipe.OpMatte {
			continue
		}
		p, err := decodeMatteOp(i, op)
		if err != nil {
			return nil, err
		}
		r := matteRequestOf(p)
		if j := slices.IndexFunc(reqs, func(q matteRequest) bool { return q.same(r) }); j >= 0 {
			reqs[j].keep = max(reqs[j].keep, r.keep)
			continue
		}
		reqs = append(reqs, r)
	}
	return reqs, nil
}

// identityFor resolves one request against the facts on device (the
// effective device): the model must be one the sidecar offers
// (ErrInvalidRecipe otherwise) and offers ON that device, a requested size
// one the device accepts, and the facts must carry the weights digest, the
// processing version and a default size (an answer without them is
// ErrMatteUnavailable — nothing can be keyed from it). Size, Precision and
// MsPerFrame are the device's own (matte.ModelState.On: a pre-5c sidecar
// reports its one device's through the mirrored fields). A tracker
// (matte.KindTracker, or the guided model id) takes no size — its Size is
// 0 and the tracking size joins the identity once the plan is known.
func identityFor(f *matte.Facts, req matteRequest, device string) (matteIdentity, error) {
	ms, ok := f.Model(req.Model)
	if !ok {
		return matteIdentity{}, fmt.Errorf("%w: the matte service does not offer model %q (offered: %s)", ErrInvalidRecipe, req.Model, offeredModels(&f.Ping))
	}
	d, ok := ms.On(device)
	if !ok {
		return matteIdentity{}, fmt.Errorf("%w: the matte service does not offer model %s on %s (offered on: %s) — pick another model or device", ErrInvalidRecipe, req.Model, matteDeviceLabel(device), modelDevices(ms))
	}
	if ms.Weights == "" || f.ProcessingVersion == "" {
		return matteIdentity{}, fmt.Errorf("%w: its last answer carries no weights digest or processing version for model %s", ErrMatteUnavailable, req.Model)
	}
	id := matteIdentity{Model: req.Model, Precision: d.Precision, Weights: ms.Weights, Proc: f.ProcessingVersion, Kind: ms.Kind, Device: device}
	if ms.IsTracker() || req.Model == recipe.MatteModelSAM2Tiny {
		id.Kind = matte.KindTracker
		if req.Size != 0 {
			return matteIdentity{}, fmt.Errorf("%w: the guided model %s takes no input size (got %d): it tracks the clip at its own size", ErrInvalidRecipe, req.Model, req.Size)
		}
		id.MsPerFrame = d.MsPerFrameAt(0)
		return id, nil
	}
	if req.Size != 0 && !d.HasSize(req.Size) {
		return matteIdentity{}, fmt.Errorf("%w: model %s does not accept input size %d on %s (sizes: %v)", ErrInvalidRecipe, req.Model, req.Size, matteDeviceLabel(device), d.Sizes)
	}
	size := d.EffectiveSize(req.Size)
	if size <= 0 {
		return matteIdentity{}, fmt.Errorf("%w: its last answer reports no input size for model %s on %s", ErrMatteUnavailable, req.Model, matteDeviceLabel(device))
	}
	id.Size, id.MsPerFrame = size, d.MsPerFrameAt(size)
	return id, nil
}

// modelDevices lists the devices a model's facts report it on ("its one
// device" for a pre-5c answer).
func modelDevices(ms matte.ModelState) string {
	if len(ms.Devices) == 0 {
		return "its one device"
	}
	devs := make([]string, 0, len(ms.Devices))
	for dev := range ms.Devices {
		devs = append(devs, dev)
	}
	sort.Strings(devs)
	return strings.Join(devs, ", ")
}

// identityOf is identityFor plus the Phase 5c guided-model resolution: a
// tracker's identity carries the canonical prompts and the edge model's
// identity (edgeIdentityFor), and must have prompts; prompts or an edge on
// a per-frame model are the client's mistake (the compiler refuses them
// too — this is the check before any pass, which runs before compile).
func identityOf(f *matte.Facts, device string, req matteRequest) (matteIdentity, error) {
	id, err := identityFor(f, req, device)
	if err != nil {
		return id, err
	}
	if !id.tracker() {
		if req.Edge != "" || len(req.prompts) > 0 {
			return id, fmt.Errorf("%w: prompts and edge need the guided model %q (got model %q)", ErrInvalidRecipe, recipe.MatteModelSAM2Tiny, req.Model)
		}
		return id, nil
	}
	if len(req.prompts) == 0 {
		return id, fmt.Errorf("%w: the guided model %q needs at least one prompt (a box or a positive point)", ErrInvalidRecipe, req.Model)
	}
	tp := trackPromptsOf(req.prompts)
	if err := tp.Validate(); err != nil {
		return id, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	id.Prompts = tp.Canonical()
	edge, err := edgeIdentityFor(f, device, req.Edge)
	if err != nil {
		return id, err
	}
	if _, masked := maskPromptFrame(req.prompts); masked && edge == nil {
		// Phase 5d: the mask is the edge model's matte — no edge, no mask.
		return id, errMaskPromptNoEdge()
	}
	id.Edge = edge
	return id, nil
}

// edgeIdentityFor resolves a tracker op's Edge on device: nil for
// recipe.MatteEdgeNone; else the named per-frame model, or — for "" — the
// sidecar's default model of the device (Ping.DefaultModelFor; the
// recipe's default when that is unknown or is itself a tracker), as a
// plain segmenter identity at its default size. A tracker named as the
// edge is the client's mistake.
func edgeIdentityFor(f *matte.Facts, device, edge string) (*matteIdentity, error) {
	if edge == recipe.MatteEdgeNone {
		return nil, nil
	}
	model := edge
	if model == "" {
		model = f.DefaultModelFor(device)
		if ms, ok := f.Model(model); model == "" || !ok || ms.IsTracker() || model == recipe.MatteModelSAM2Tiny {
			model = recipe.MatteModelDefault
		}
	}
	ms, ok := f.Model(model)
	if !ok {
		return nil, fmt.Errorf("%w: the matte service does not offer edge model %q (offered: %s)", ErrInvalidRecipe, model, offeredModels(&f.Ping))
	}
	if ms.IsTracker() || model == recipe.MatteModelSAM2Tiny {
		return nil, fmt.Errorf("%w: the edge model must be a per-frame model or %q, not the tracker %q", ErrInvalidRecipe, recipe.MatteEdgeNone, model)
	}
	id, err := identityFor(f, matteRequest{Model: model}, device)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// matteClipKey is the memo key of src's matte for the temporal ops of ops at
// the plan rate fps (graph.FPSText(Plan.FPS)) under identity id (spec §4.2):
// matte.ClipKey over the stack's temporal ops, the probe facts the prefix
// compiles from under the current store.InfoVersion, the rate and the
// identity — for a tracker the canonical prompts and the tracking size as
// well (the key's track line). Neither the device nor PipelineVersion is
// part of it; nor is the edge model (its memo has a key of its own, the
// gated sequence is a derived dir under this one).
func matteClipKey(src *store.Blob, ops []recipe.Op, fps string, id matteIdentity) string {
	parts := matte.ClipKeyParts{
		Src:         src.Hash,
		Temporal:    matte.TemporalOps(ops),
		Probe:       *src.Info,
		InfoVersion: store.InfoVersion,
		FPS:         fps,
		Model:       id.Model,
		Size:        id.Size,
		Precision:   id.Precision,
		Weights:     id.Weights,
		Proc:        id.Proc,
	}
	if id.tracker() {
		parts.Prompts, parts.TrackW, parts.TrackH = id.Prompts, id.TrackW, id.TrackH
	}
	return matte.ClipKey(parts)
}

// readMatteMemo reads the memo of key under Protect (so the sweeper cannot
// remove it between the check and the read): the manifest must be readable,
// name a positive frame count and its first and last PNG must exist. ok is
// false for a miss or a memo short of its PNGs (the caller treats both as
// "run the pass", which renames a complete dir over it).
func readMatteMemo(st *store.Store, key string) (man *matte.Manifest, ok bool) {
	dir := st.MatteDir(key)
	release := st.Protect(dir)
	defer release()
	man, err := matte.ReadManifest(filepath.Join(dir, matte.ManifestName))
	if err != nil {
		return nil, false
	}
	for _, n := range []int{1, man.Frames} {
		if _, err := os.Stat(filepath.Join(dir, matte.FrameFile(n))); err != nil {
			return nil, false
		}
	}
	return man, true
}

// matteMemoHit is readMatteMemo plus the string-exact rate check and the
// touch: a memo made for another rate (impossible under the key, kept as
// the belt to the braces) is a miss the pass overwrites.
func (m *Manager) matteMemoHit(key, fps string) (*matte.Manifest, bool) {
	man, ok := readMatteMemo(m.st, key)
	if !ok {
		return nil, false
	}
	if man.Key != key || man.FPS != fps {
		log.Printf("jobs: matte: memo %s is for key %s at %s fps, not %s at %s; redoing it", short(key), short(man.Key), man.FPS, short(key), fps)
		return nil, false
	}
	if err := m.st.TouchMatte(key); err != nil {
		log.Printf("jobs: matte: %v", err)
	}
	return man, true
}

// ---- resolution -------------------------------------------------------------

// errMatteRekey is the pass's signal that the live sidecar's weights or
// processing version differ from the facts the key was made from (the
// sidecar was upgraded since the last probe); resolveMattes re-resolves
// with the facts the pass's ping just persisted.
var errMatteRekey = errors.New("jobs: matte: the sidecar's identity changed; re-keying")

// resolveMattes resolves every matte op of ops for the main source src at
// the rate the plan for out runs at: nil, nil when ops hold no matte op (no
// facts read, no file touched); otherwise one resolvedMatte per distinct
// request (model, size, stabilise, prompts, edge) in order of first use
// (the compiler's dedupe), each pointing at a COMPLETE memo on disk — and,
// Phase 5c, at the derived sequence the op asks for (SeqDir: stabilised
// and / or gated with the edge model, derived here on any request, eager
// or not — a cheap ffmpeg pass, no model run). Memo hits are served from
// the persisted facts alone (no live sidecar) and touched; a miss in mode
// mattePreview WITHOUT MatteEager(ctx) starts nothing and returns
// *ErrMattePending State idle at once — unless a pass for the clip is in
// flight (an eager preview's or a render's), which it follows for
// mattePreviewWait and then reports (running / loading / downloading); an
// eager preview (the Compute matte button) runs or joins the pass the
// same way; mode matteRender waits under ctx and reports progress through
// matteProgressFn(ctx) when the render installed a listener. out is the
// recipe's Output (previews pass their stillOutput subset: the plan's fps
// follows Output.Format / Output.FPS exactly as the render's).
//
// Errors: ErrInvalidRecipe for a malformed matte op, a model or size the
// facts do not cover (on the effective device), prompts / an edge on a
// per-frame model, a prompt on a frame past the clip, and the up-front
// refusal over Options.MatteMaxFrames / MatteMaxSeconds (frames ×
// (msPerFrame + 2 ms), a tracker's with matteTrackEstimateFactor on top)
// or the tracker's body cap (floats and comparisons only — nothing
// allocates from Plan.Frames); an error wrapping ErrMatteUnavailable when
// no facts exist yet or no sidecar can run the pass; the pass's own plain
// error when it fails; ctx.Err() when the caller's ctx ends. Callers that
// run ffmpeg over the result protect the dirs meanwhile (protectMattes).
func (m *Manager) resolveMattes(ctx context.Context, src *store.Blob, ops []recipe.Op, out recipe.Output, mode matteMode) ([]resolvedMatte, error) {
	reqs, err := matteRequests(ops)
	if err != nil || len(reqs) == 0 {
		return nil, err
	}
	if src == nil || src.Info == nil {
		return nil, fmt.Errorf("%w: the main source has no probe info", ErrInvalidRecipe)
	}
	// The matte input plan: the temporal prefix at the render's rate — its
	// FPS text keys the memo and is what the manifest must repeat.
	plan, err := graph.CompileMatteInput([]recipe.ProbeInfo{*src.Info}, ops, out)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	fps := graph.FPSText(plan.FPS)
	for attempt := 0; attempt < 2; attempt++ {
		facts, err := m.loadMatteFacts()
		if err != nil {
			return nil, err
		}
		mattes := make([]resolvedMatte, 0, len(reqs))
		rekey := false
		for _, req := range reqs {
			rm, err := m.resolveMatte(ctx, src, ops, plan, fps, req, facts, mode)
			if errors.Is(err, errMatteRekey) {
				rekey = true
				break
			}
			if err != nil {
				return nil, err
			}
			mattes = append(mattes, rm)
		}
		if !rekey {
			return mattes, nil
		}
	}
	return nil, fmt.Errorf("%w: the matte service changed its weights twice during one request", ErrMatteUnavailable)
}

// resolveMatte resolves one request (see resolveMattes): the identity on
// the effective device, the memo hit or the pass (matteMiss), then — Phase
// 5c — for a tracker with an edge the edge model's own resolution (a plain
// segmenter request on the same clip: its memo, or its pass under the same
// policy) and the gate derive, and for any op with a stabilise mode the
// stabilise derive on top; SeqDir names what the plan reads. Phase 5d: a
// tracker request with a MASK prompt (recipe.MattePrompt.MaskFrom) resolves
// the edge FIRST — the mask is the edge memo's matte of that frame scaled
// to the tracking size (resolveMaskPrompt) and its digest is part of the
// tracker's key — and the gate reuses that resolution.
func (m *Manager) resolveMatte(ctx context.Context, src *store.Blob, ops []recipe.Op, plan *graph.Plan, fps string, req matteRequest, facts *matte.Facts, mode matteMode) (resolvedMatte, error) {
	device := m.matteEffectiveDevice(&facts.Ping)
	id, err := identityOf(facts, device, req)
	if err != nil {
		return resolvedMatte{}, err
	}
	var (
		edge *resolvedMatte // the edge model's resolution, once made (the mask prompt, then the gate)
		mask []byte         // the mask prompt's PNG at the tracking size (nil without one)
	)
	if id.tracker() {
		id.TrackW, id.TrackH = enc.TrackSize(plan.Width, plan.Height)
		if id.TrackW < 1 || id.TrackH < 1 {
			return resolvedMatte{}, fmt.Errorf("%w: the source has no frame size", ErrInvalidRecipe)
		}
		if frame, has := maskPromptFrame(req.prompts); has {
			if edge, mask, err = m.resolveMaskPrompt(ctx, src, ops, plan, fps, id, frame, req.prompts, facts, mode); err != nil {
				return resolvedMatte{}, err
			}
			id.Prompts = withMaskDigest(trackPromptsOf(req.prompts), maskDigestOf(mask)).Canonical()
		}
	}
	key := matteClipKey(src, ops, fps, id)
	rm := resolvedMatte{
		Model: id.Model, Size: id.Size, ReqSize: req.Size, Precision: id.Precision, Dir: m.st.MatteDir(key), ClipKey: key,
		Stabilise: req.Stabilise, Prompts: req.Prompts, ReqEdge: req.Edge, Device: device, Keep: req.keep, id: id,
	}
	if id.Edge != nil {
		rm.Edge = id.Edge.Model
	}
	man, ok := m.matteMemoHit(key, fps)
	if !ok {
		if man, err = m.matteMiss(ctx, src, plan, fps, id, key, req.prompts, mask, mode); err != nil {
			return resolvedMatte{}, err
		}
	}
	rm.Manifest = man
	// The memo stays protected from here through the derives (the hit's own
	// Protect ended with the manifest read): a sweep in between would turn a
	// derive into an opaque ffmpeg failure instead of a "retry".
	release := m.st.Protect(rm.Dir)
	defer release()
	if err := matteSequenceGone(rm.Dir); err != nil {
		return resolvedMatte{}, err
	}
	seq := rm.Dir
	if id.Edge != nil {
		if edge == nil {
			e, err := m.resolveMatte(ctx, src, ops, plan, fps, matteRequest{Model: id.Edge.Model}, facts, mode)
			if err != nil {
				return resolvedMatte{}, err
			}
			edge = &e
		}
		if edge.Manifest.Frames != man.Frames {
			return resolvedMatte{}, fmt.Errorf("AI matte: the edge model's memo has %d frames, the tracker's %d — report this with the source", edge.Manifest.Frames, man.Frames)
		}
		rm.edge = edge
		if seq, err = m.deriveGated(ctx, rm.Dir, edge.Dir, id.Edge.Model, edge.ClipKey, man.Frames); err != nil {
			return resolvedMatte{}, err
		}
	}
	if req.Stabilise != "" {
		if seq, err = m.deriveStabilised(ctx, seq, req.Stabilise, man.Frames, rm.Dir); err != nil {
			return resolvedMatte{}, err
		}
	}
	rm.SeqDir = seq
	return rm, nil
}

// matteMiss handles a request whose memo is not on disk: the work's
// estimate and the up-front refusals (the caps; a tracker's body cap and
// prompt frames too), then the Phase 5c start policy — a plain preview
// (mode matteModePreview without MatteEager) follows a pass in flight
// (joinMatteFlight) or answers MattePendingIdle, never starting one; an
// eager preview or a render runs or joins the pass (runMatteFlight). mask
// is a tracker's mask prompt PNG at the tracking size (Phase 5d; nil
// without one — resolveMatte derived it and keyed id.Prompts with its
// digest).
func (m *Manager) matteMiss(ctx context.Context, src *store.Blob, plan *graph.Plan, fps string, id matteIdentity, key string, prompts []recipe.MattePrompt, mask []byte, mode matteMode) (*matte.Manifest, error) {
	if m.mt.client == nil {
		return nil, fmt.Errorf("%w: no AI matte on disk for this clip and AI mattes are off on this server (EZLG_MATTE_URL is empty)", ErrMatteUnavailable)
	}
	w := matteWork{src: src, plan: plan, fps: fps, id: id, dir: m.st.MatteDir(key), frames: plan.Frames, device: id.Device, prompts: prompts, mask: mask}
	if id.tracker() {
		w.estMS = matteTrackEstimateMS(w.frames, id.MsPerFrame)
		// The belt: a mask-prompted request ran these before its edge pass
		// (resolveMaskPrompt), every other tracker request runs them here.
		if err := m.trackRefusals(w.frames, id, prompts); err != nil {
			return nil, err
		}
	} else {
		w.estMS = matteEstimateMS(w.frames, id.MsPerFrame)
		if err := m.matteRefusal(w.frames, w.estMS, w.device, id.Model); err != nil {
			return nil, err
		}
	}
	if mode == matteModePreview && !MatteEager(ctx) {
		man, joined, err := m.joinMatteFlight(ctx, key, w)
		if !joined {
			return nil, &ErrMattePending{State: MattePendingIdle, Total: w.frames, Device: w.device, EstimateMS: w.estMS}
		}
		if err != nil {
			return nil, err
		}
		return &man, nil
	}
	man, err := m.runMatteFlight(ctx, mode, key, w)
	if err != nil {
		return nil, err
	}
	return &man, nil
}

// joinMatteFlight is a plain preview's way of following a pass: it joins
// the pass for key when one is in flight (flight.join: a waiter for
// mattePreviewWait, then *ErrMattePending from the pass's progress, like
// runMatteFlight's preview wait) and reports joined false — nothing to
// follow, nothing started — otherwise, including when the run it joined
// ended cancelled by the abandon timer.
func (m *Manager) joinMatteFlight(ctx context.Context, key string, w matteWork) (man matte.Manifest, joined bool, err error) {
	wctx, cancel := context.WithTimeout(ctx, mattePreviewWait)
	defer cancel()
	man, joined, err = m.mt.flight.join(wctx, key, matteAbandonGrace)
	switch {
	case !joined, err == nil:
		return man, joined, err
	case wctx.Err() != nil && ctx.Err() == nil:
		return matte.Manifest{}, true, m.mattePendingFor(key, w)
	case isContextError(err) && ctx.Err() == nil:
		return matte.Manifest{}, false, nil
	}
	return matte.Manifest{}, true, err
}

// matteEstimateMS is the pass's estimated wall time: frames × (msPerFrame +
// matteFrameOverheadMS), 0 when the count is unknown. "Up to": frames the
// store already holds cost nothing.
func matteEstimateMS(frames int, msPerFrame float64) int64 {
	if frames <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(frames) * (math.Max(msPerFrame, 0) + matteFrameOverheadMS)))
}

// matteRefusal is the up-front refusal (spec §4.1 step 4): more frames than
// Options.MatteMaxFrames, or an estimate over Options.MatteMaxSeconds, is an
// ErrInvalidRecipe naming the knob. An unknown count (frames 0) is never
// refused here: the run-time frame cap and the hard timeout bound it.
func (m *Manager) matteRefusal(frames int, estMS int64, device, model string) error {
	if frames <= 0 {
		return nil
	}
	if frames > m.opts.MatteMaxFrames {
		return fmt.Errorf("%w: an AI matte of %d frames is over this server's cap of %d frames (EZLG_MATTE_MAX_FRAMES) — trim the clip or lower the fps", ErrInvalidRecipe, frames, m.opts.MatteMaxFrames)
	}
	if estMS > int64(m.opts.MatteMaxSeconds)*1000 {
		advice := "trim the clip or lower the fps"
		if model != recipe.MatteModelDefault {
			advice = "trim the clip, lower the fps, or pick the fast model"
		}
		return fmt.Errorf("%w: an AI matte of %d frames would take up to ~%s on this server's %s — over its %d s cap (EZLG_MATTE_MAX_SECONDS); %s", ErrInvalidRecipe, frames, humanSeconds(estMS), matteDeviceLabel(device), m.opts.MatteMaxSeconds, advice)
	}
	return nil
}

// matteFrameCapError is the run-time cap's message (the batch writer aborts
// the pass at max+1 streamed frames).
func matteFrameCapError(max int) error {
	return fmt.Errorf("%w: an AI matte of more than %d frames is over this server's cap (EZLG_MATTE_MAX_FRAMES) — trim the clip or lower the fps", ErrInvalidRecipe, max)
}

// runMatteFlight runs or joins the pass for key and waits for it the way
// mode says. In preview mode the wait is bounded by mattePreviewWait and an
// expired wait becomes *ErrMattePending from the pass's progress; in render
// mode the wait is the caller's ctx and the pass's progress is mirrored
// into the ctx's withMatteProgress listener meanwhile.
func (m *Manager) runMatteFlight(ctx context.Context, mode matteMode, key string, w matteWork) (matte.Manifest, error) {
	wctx := ctx
	if mode == matteModePreview {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, mattePreviewWait)
		defer cancel()
	} else if fn := matteProgressFn(ctx); fn != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			t := time.NewTicker(matteProgressPoll)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					if p, ok := m.matteProgressFor(key); ok {
						fn(p.pending())
					}
				}
			}
		}()
	}
	man, err := m.mt.flight.doDetachedAbandon(wctx, key, matteAbandonGrace, func(pctx context.Context) (matte.Manifest, error) {
		if got, ok := m.matteMemoHit(key, w.fps); ok {
			return *got, nil // a previous leader finished while we waited
		}
		if w.id.tracker() {
			return m.runTrackPass(pctx, key, w)
		}
		return m.runMattePass(pctx, key, w)
	})
	if err != nil && mode == matteModePreview && wctx.Err() != nil && ctx.Err() == nil {
		return matte.Manifest{}, m.mattePendingFor(key, w)
	}
	return man, err
}

// mattePendingFor builds the pending error from the running pass's
// progress (a pass between stages reads as running at 0).
func (m *Manager) mattePendingFor(key string, w matteWork) *ErrMattePending {
	p, ok := m.matteProgressFor(key)
	if !ok {
		p = matteProgress{State: MattePendingRunning, Total: w.frames, Device: w.device, EstimateMS: w.estMS}
	}
	e := p.pending()
	return &e
}

// ---- the pass ---------------------------------------------------------------

// matteWork is what one pass needs.
type matteWork struct {
	src     *store.Blob
	plan    *graph.Plan // the matte input plan (graph.CompileMatteInput)
	fps     string      // graph.FPSText(plan.FPS): the key's and the manifest's rate text
	id      matteIdentity
	dir     string               // the memo dir to rename into
	frames  int                  // the plan's pre-bounce frame count (0 = unknown)
	estMS   int64                // the estimate (0 = unknown)
	device  string               // the effective device the pass runs on (id.Device)
	prompts []recipe.MattePrompt // a tracker's prompts
	mask    []byte               // Phase 5d: a tracker's mask prompt PNG at the tracking size (nil without one); its digest is in id.Prompts
}

// runMattePass produces the clip's matte sequence (spec §4.1 steps 5–9)
// under the flight's ctx: waits for the model, streams the producer through
// the batch writer, files every matte, writes the manifest and renames the
// tmp dir into place. Internal timeouts and sidecar failures surface as
// plain errors (never context errors), so the flight never retries them;
// only ctx itself ending — the abandon timer — yields ctx.Err().
func (m *Manager) runMattePass(ctx context.Context, key string, w matteWork) (man matte.Manifest, err error) {
	if m.tools.FFmpeg == "" {
		return man, errors.New("ffmpeg is not available on this server")
	}
	m.setMatteProgress(key, func(p *matteProgress) {
		*p = matteProgress{State: MattePendingRunning, Total: w.frames, Device: w.device, EstimateMS: w.estMS}
	})
	defer m.clearMatteProgress(key)
	started := time.Now()

	ping, ms, d, err := m.waitMatteReady(ctx, key, w.id.Model, w.device)
	if err != nil {
		return man, err
	}
	if ms.Weights != w.id.Weights || ping.ProcessingVersion != w.id.Proc {
		return man, errMatteRekey
	}
	if ms.IsTracker() {
		return man, fmt.Errorf("%w: model %s is a tracker (the guided model): it needs prompts", ErrInvalidRecipe, w.id.Model)
	}
	if !d.HasSize(w.id.Size) {
		return man, fmt.Errorf("%w: model %s does not accept input size %d on %s (sizes: %v)", ErrInvalidRecipe, w.id.Model, w.id.Size, matteDeviceLabel(w.device), d.Sizes)
	}
	device := w.device
	msPerFrame := d.MsPerFrameAt(w.id.Size)
	m.setMatteProgress(key, func(p *matteProgress) {
		p.State, p.Device = MattePendingRunning, device
		if p.EstimateMS == 0 {
			p.EstimateMS = matteEstimateMS(w.frames, msPerFrame)
		}
	})

	argv := enc.MatteSourceArgs(w.src.Path, w.plan, w.id.Size)
	if argv == nil {
		return man, errors.New("AI matte: the matte input plan is not usable")
	}
	argv = append(slices.Clone(ffmpegPrefix), argv...)
	frameDir := m.st.MatteFrameDir(w.id.Model, w.id.Size, w.id.Precision, weights8(w.id.Weights), w.id.Proc)
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		return man, fmt.Errorf("AI matte: frames store: %w", err)
	}
	tmp := m.st.MatteTmpDir(key)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return man, fmt.Errorf("AI matte: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	pass := &mattePass{
		m: m, key: key, w: w, tmp: tmp, frameDir: frameDir,
		pctx: pctx, msPerFrame: msPerFrame, busy: ping.Busy, device: wireDevice(ping, device),
		pending: map[string][]int{}, filed: map[string]bool{},
	}
	bw := newBatchWriter(w.id.Size*w.id.Size*3, m.opts.MatteMaxFrames, pass.onFrame, pcancel)
	hard := matteHardTimeout(m.opts.MatteMaxSeconds, w.estMS)
	timer := time.AfterFunc(hard, func() {
		bw.fail(fmt.Errorf("AI matte failed: the pass did not finish within %s (its hard timeout) — trim the clip or raise EZLG_MATTE_MAX_SECONDS", hard))
	})
	// The producer is read HERE, at the consumer's pace (ffrun.RunFrom):
	// RunTo's exec copier is bounded by exec's WaitDelay (2 s) once ffmpeg
	// has exited, and ffmpeg exits the moment the pipe holds its last
	// frames — a batch writer blocked in a POST for longer (CPU lite at
	// seconds per frame, a busy sidecar, a session re-created inside the
	// request, a 503 retry) would lose the rest of the stream to
	// exec.ErrWaitDelay.
	runErr := ffrun.RunFrom(pctx, m.tools.FFmpeg, argv, func(stdout io.Reader) error {
		_, err := io.Copy(bw, stdout)
		return err
	})
	timer.Stop()
	if ferr := bw.failure(); ferr != nil {
		return man, ferr
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return man, ctx.Err()
		}
		return man, fmt.Errorf("AI matte: decoding the clip failed: %w", runErr)
	}
	if err := bw.finish(); err != nil {
		return man, err
	}
	if err := pass.flush(); err != nil {
		return man, err
	}
	n := bw.frames
	if n == 0 {
		return man, errors.New("AI matte: the clip decoded to no frames")
	}
	if len(pass.pending) > 0 {
		return man, fmt.Errorf("AI matte: %d frames were never answered by the matte service", len(pass.pending))
	}

	man = matte.Manifest{
		Key: key, Src: w.src.Hash, Model: w.id.Model,
		Weights: w.id.Weights, Proc: w.id.Proc, GraphDigest: ms.GraphDigest, Precision: w.id.Precision,
		Size: w.id.Size, FPS: w.fps, Frames: n, Device: device, MsPerFrame: msPerFrame,
	}
	if err := matte.WriteManifest(filepath.Join(tmp, matte.ManifestName), &man); err != nil {
		return man, fmt.Errorf("AI matte: %w", err)
	}
	if got, hit := m.matteMemoHit(key, w.fps); hit {
		// Another instance of the app filed the same clip meanwhile.
		return *got, nil
	}
	_ = os.RemoveAll(w.dir) // never a usable memo (dropped above, or junk)
	if err := os.Rename(tmp, w.dir); err != nil {
		return man, fmt.Errorf("AI matte: %w", err)
	}
	ok = true
	log.Printf("jobs: matte: %s: %d frames (%d posted, %d from the store) in %s on %s (%s %s %d²)",
		short(key), n, pass.posted, n-pass.posted, time.Since(started).Round(time.Millisecond), device, w.id.Model, w.id.Precision, w.id.Size)
	return man, nil
}

// weights8 is the frames-store component of a weights sha256.
func weights8(weights string) string {
	if len(weights) > 8 {
		return weights[:8]
	}
	return weights
}

// matteHardTimeout is the pass's hard timeout: min(2 × MatteMaxSeconds, 3 ×
// estimate + 60 s); the cap alone when the estimate is unknown.
func matteHardTimeout(maxSeconds int, estMS int64) time.Duration {
	capT := 2 * time.Duration(maxSeconds) * time.Second
	if estMS <= 0 {
		return capT
	}
	return min(capT, 3*time.Duration(estMS)*time.Millisecond+60*time.Second)
}

// matteBatchTimeout is one POST's timeout: 60 s + frames × 5 × msPerFrame,
// scaled by the batches the sidecar reports in flight for others.
func matteBatchTimeout(frames int, msPerFrame float64, busy int) time.Duration {
	t := matteBatchBaseTimeout + time.Duration(float64(frames)*matteBatchPerFrame*math.Max(msPerFrame, 0))*time.Millisecond
	return t * time.Duration(1+min(max(busy, 0), matteBusyCap))
}

// waitMatteReady pings the sidecar (reusing an answer up to
// mattePingMaxAge old) and returns once model is ready to run on device
// (matteReadiness classifies the state). Loading and downloading are
// pending states: the pass's progress shows them (a preview answers 202
// with them), /v1/warm is posted once for that device so the load is under
// way, and the state is re-polled every matteLoadPoll for up to
// matteLoadTimeout; a model "missing" with no reason (its download not
// started yet — queued behind another model's load at startup) is pending
// the same way. A device or model that is unavailable, a model missing
// AFTER a failed download (reason / lastError set), an unreachable sidecar
// or another protocol refuse with ErrMatteUnavailable and the sidecar's
// own reason.
func (m *Manager) waitMatteReady(ctx context.Context, key, model, device string) (*matte.Ping, matte.ModelState, matte.DeviceState, error) {
	deadline := time.Now().Add(matteLoadTimeout)
	warmed := false
	maxAge := mattePingMaxAge
	for {
		p, ms, d, pending, err := m.matteReadinessAged(ctx, model, device, maxAge)
		maxAge = 0
		if err != nil {
			return nil, ms, d, err
		}
		if pending == "" {
			return p, ms, d, nil
		}
		if !warmed {
			warmed = true
			wctx, cancel := context.WithTimeout(ctx, matteProbeTimeout)
			if err := m.mt.client.Warm(wctx, model, wireDevice(p, device)); err != nil {
				log.Printf("jobs: matte: warm %s: %v", model, err)
			}
			cancel()
		}
		m.setMatteProgress(key, func(pr *matteProgress) {
			pr.State, pr.Percent, pr.Device = pending, d.Percent, device
		})
		if time.Now().After(deadline) {
			return nil, ms, d, fmt.Errorf("AI matte failed: the matte service did not finish loading model %s within %s (state %s)", model, matteLoadTimeout, d.State)
		}
		select {
		case <-ctx.Done():
			return nil, ms, d, ctx.Err()
		case <-time.After(matteLoadPoll):
		}
	}
}

// matteReadiness is matteReadinessAged with the pass's reuse window
// (mattePingMaxAge).
func (m *Manager) matteReadiness(ctx context.Context, model, device string) (*matte.Ping, matte.ModelState, matte.DeviceState, string, error) {
	return m.matteReadinessAged(ctx, model, device, mattePingMaxAge)
}

// matteReadinessAged pings the sidecar (an answer up to maxAge old reused)
// and classifies model's state ON device: pending is "" when the model is
// ready there, else the MattePending* state to wait in (loading,
// downloading — a bare "missing", a download not started yet, counts as
// downloading); err refuses. A sidecar that no longer offers device — the
// facts the device was chosen from are stale and the ping just persisted
// the new ones — is errMatteRekey, so resolveMattes resolves again.
//
// "missing" with a reason or a lastError is a FAILED download (the sidecar
// retries it every 10 min on its own) and refuses with that text. A bare
// "missing" is a download that has not started yet — the sidecar has one
// sequential loader thread, so at startup the second model stays "missing"
// with no reason for the whole time the first one downloads, derives and
// self-tests, and /v1/warm is a no-op while it is queued — and that is
// pending (the sidecar's own gate answers 503 "model loading" with state
// downloading for it), bounded by matteLoadTimeout like loading /
// downloading, however many polls it takes.
func (m *Manager) matteReadinessAged(ctx context.Context, model, device string, maxAge time.Duration) (p *matte.Ping, ms matte.ModelState, d matte.DeviceState, pending string, err error) {
	p, err = m.pingMatte(ctx, maxAge)
	if err != nil {
		var se *matte.StatusError
		switch {
		case ctx.Err() != nil:
			// The pass was abandoned / the job cancelled mid-ping: a
			// context error, which the flight knows not to hand to a
			// waiter whose own ctx is alive (it re-runs the pass).
			return nil, ms, d, "", ctx.Err()
		case errors.Is(err, ErrMatteUnavailable):
			return nil, ms, d, "", err
		case errors.As(err, &se):
			// The sidecar answered (a 503 with device "unavailable"
			// carries its reason as the message).
			return nil, ms, d, "", fmt.Errorf("%w: the matte service at %s is not usable: %s", ErrMatteUnavailable, m.matteURL(), se.Message)
		}
		return nil, ms, d, "", fmt.Errorf("%w: AI matte failed: sidecar unreachable at %s — is the matte profile up? (%v)", ErrMatteUnavailable, m.matteURL(), err)
	}
	if p.Device == matte.DeviceUnavailable {
		return nil, ms, d, "", fmt.Errorf("%w: the matte service's device is unavailable: %s", ErrMatteUnavailable, p.Reason)
	}
	if !p.Offers(device) {
		return nil, ms, d, "", errMatteRekey
	}
	ms, ok := p.Model(model)
	if !ok {
		return nil, ms, d, "", fmt.Errorf("%w: the matte service does not offer model %q (offered: %s)", ErrInvalidRecipe, model, offeredModels(p))
	}
	d, ok = ms.On(device)
	if !ok {
		return nil, ms, d, "", fmt.Errorf("%w: the matte service does not offer model %s on %s (offered on: %s)", ErrInvalidRecipe, model, matteDeviceLabel(device), modelDevices(ms))
	}
	switch d.State {
	case matte.StateReady:
		return p, ms, d, "", nil
	case matte.StateUnavailable:
		return nil, ms, d, "", fmt.Errorf("%w: matte model %s is unavailable on %s: %s", ErrMatteUnavailable, model, matteDeviceLabel(device), firstNonEmpty(d.Reason, ms.LastError, "see the matte service's log"))
	case matte.StateMissing:
		if d.Reason != "" || ms.LastError != "" {
			return nil, ms, d, "", fmt.Errorf("%w: matte model %s is missing: %s", ErrMatteUnavailable, model, firstNonEmpty(d.Reason, ms.LastError, "its weights were not downloaded"))
		}
		return p, ms, d, MattePendingDownloading, nil
	case matte.StateLoading:
		return p, ms, d, MattePendingLoading, nil
	case matte.StateDownloading:
		return p, ms, d, MattePendingDownloading, nil
	}
	return nil, ms, d, "", fmt.Errorf("%w: matte model %s is in an unknown state %q — update the matte service", ErrMatteUnavailable, model, d.State)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// mattePass is the per-pass state behind the batch writer: the frames
// store lookup, the pending batch and the filing of answers.
type mattePass struct {
	m          *Manager
	key        string
	w          matteWork
	tmp        string // the clip dir being filled
	frameDir   string // the frames store for this (model, size, prec, weights, proc)
	pctx       context.Context
	msPerFrame float64
	busy       int
	device     string // the device= of every POST (wireDevice: "" for a pre-5c sidecar)

	done    int              // frames filed into tmp so far
	posted  int              // frames POSTed so far
	pending map[string][]int // hash → slots waiting for the batch that carries it
	filed   map[string]bool  // hashes filed in the frames store during this pass
	batch   []matteFrame     // distinct misses waiting for the next POST
}

// matteFrame is one distinct miss of the pending batch.
type matteFrame struct {
	hash string
	rgb  []byte
}

// onFrame is the batch writer's callback: hashes the frame, serves a
// frames-store hit at once and queues a miss for the next POST (a frame
// repeated in the clip rides on the first one's answer).
func (p *mattePass) onFrame(slot int, rgb []byte) error {
	h := matte.FrameKey(rgb)
	storePath := filepath.Join(p.frameDir, h+".png")
	if p.filed[h] {
		return p.copySlot(storePath, slot)
	}
	if _, err := os.Stat(storePath); err == nil {
		if err := p.m.st.TouchMatteFrame(storePath); err != nil {
			log.Printf("jobs: matte: %v", err)
		}
		p.filed[h] = true
		return p.copySlot(storePath, slot)
	}
	if slots, ok := p.pending[h]; ok {
		p.pending[h] = append(slots, slot)
		return nil
	}
	p.pending[h] = []int{slot}
	p.batch = append(p.batch, matteFrame{hash: h, rgb: bytes.Clone(rgb)})
	if len(p.batch) >= matteBatchFrames {
		return p.post()
	}
	return nil
}

// flush POSTs the last partial batch.
func (p *mattePass) flush() error {
	if len(p.batch) == 0 {
		return nil
	}
	return p.post()
}

// post sends the pending batch, retrying a 503 that names a retry delay
// (model loading, out of memory after another model was released) until
// matteLoadTimeout, and files every answered matte. Any other failure is
// the pass's: a plain error naming the sidecar.
func (p *mattePass) post() error {
	frames := len(p.batch)
	timeout := matteBatchTimeout(frames, p.msPerFrame, p.busy)
	deadline := time.Now().Add(matteLoadTimeout)
	for {
		bctx, cancel := context.WithTimeout(p.pctx, timeout)
		i := 0
		err := p.m.mt.client.Matte(bctx, p.w.id.Model, p.device, p.w.id.Size, frames, p.body(), func(png []byte) error {
			if i >= frames {
				return fmt.Errorf("%w: more records than frames", matte.ErrMissingTerminator)
			}
			f := p.batch[i]
			i++
			return p.file(f.hash, png)
		})
		cancel()
		if err == nil {
			break
		}
		if p.pctx.Err() != nil {
			return p.pctx.Err()
		}
		var se *matte.StatusError
		switch {
		case errors.As(err, &se) && se.Status == 503 && se.RetryAfterMS > 0 && time.Now().Add(se.RetryAfter()).Before(deadline):
			p.m.setMatteProgress(p.key, func(pr *matteProgress) { pr.State = MattePendingLoading })
			select {
			case <-p.pctx.Done():
				return p.pctx.Err()
			case <-time.After(se.RetryAfter()):
			}
			p.m.setMatteProgress(p.key, func(pr *matteProgress) { pr.State = MattePendingRunning })
			continue
		case errors.As(err, &se):
			return fmt.Errorf("AI matte failed: the matte service answered HTTP %d: %s", se.Status, se.Message)
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("AI matte failed: the matte service did not answer a batch of %d frames within %s", frames, timeout)
		case errors.Is(err, matte.ErrTruncated), errors.Is(err, matte.ErrShortCount), errors.Is(err, matte.ErrMissingTerminator), errors.Is(err, matte.ErrRecordTooLarge):
			return fmt.Errorf("AI matte failed: %v", err)
		default:
			return fmt.Errorf("AI matte failed: sidecar unreachable at %s — is the matte profile up? (%v)", p.m.matteURL(), err)
		}
	}
	p.posted += frames
	p.batch = p.batch[:0]
	return nil
}

// body is the batch's request body: the frames' rgb24 bytes back to back
// (a fresh reader per attempt).
func (p *mattePass) body() io.Reader {
	rs := make([]io.Reader, len(p.batch))
	for i, f := range p.batch {
		rs[i] = bytes.NewReader(f.rgb)
	}
	return io.MultiReader(rs...)
}

// file stores one answered matte in the frames store and copies it to
// every slot waiting for its hash.
func (p *mattePass) file(hash string, png []byte) error {
	storePath := filepath.Join(p.frameDir, hash+".png")
	if !p.filed[hash] {
		if err := writeFrameStore(storePath, png); err != nil {
			return fmt.Errorf("AI matte: frames store: %w", err)
		}
		p.filed[hash] = true
	}
	slots := p.pending[hash]
	delete(p.pending, hash)
	for _, slot := range slots {
		if err := p.writeSlot(slot, png); err != nil {
			return err
		}
	}
	return nil
}

// copySlot copies a frames-store file to the clip dir's slot.
func (p *mattePass) copySlot(storePath string, slot int) error {
	data, err := os.ReadFile(storePath)
	if err != nil {
		return fmt.Errorf("AI matte: frames store: %w", err)
	}
	return p.writeSlot(slot, data)
}

// writeSlot writes one matte under <tmp>/%06d.png and advances the
// progress.
func (p *mattePass) writeSlot(slot int, png []byte) error {
	if err := os.WriteFile(filepath.Join(p.tmp, matte.FrameFile(slot)), png, 0o644); err != nil {
		return fmt.Errorf("AI matte: %w", err)
	}
	p.done++
	done, total := p.done, p.w.frames
	p.m.setMatteProgress(p.key, func(pr *matteProgress) {
		pr.Done = done
		if total > 0 {
			pr.Percent = min(100, done*100/total)
		}
	})
	return nil
}

// writeFrameStore files png at path (tmp + rename; a dot-prefixed temp
// beside it, which the sweeper treats as an interrupted write). An
// existing file is left alone: the hash names its content.
func writeFrameStore(path string, png []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	dir, base := filepath.Split(path)
	tmp := filepath.Join(dir, "."+base+"."+store.RandomID(4))
	if err := os.WriteFile(tmp, png, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		if _, serr := os.Stat(path); serr == nil {
			return nil // a concurrent pass filed the same frame
		}
		return err
	}
	return nil
}

// batchWriter cuts a rawvideo stream into exact frameBytes-long frames
// (Content-Length arithmetic, never decoding), counts them against the
// run-time frame cap and hands each complete frame to onFrame. Its first
// failure — onFrame's error, the cap, or one injected with fail (the hard
// timeout) — is sticky: it cancels the producer's ctx at once (so RunFrom's
// cmd.Cancel kills ffmpeg instead of waiting for its next EPIPE) and every
// later Write returns it; failure() reports it after RunFrom returns. The
// writer runs in the pass's own goroutine (ffrun.RunFrom reads the pipe at
// the writer's pace), so a Write blocked in a POST for longer than exec's
// WaitDelay after ffmpeg has exited loses nothing.
type batchWriter struct {
	frameBytes int
	maxFrames  int // 0 = no cap
	onFrame    func(slot int, rgb []byte) error
	cancel     context.CancelFunc

	buf    []byte // a partial frame
	frames int    // complete frames seen (= the slot of the last one)

	mu  sync.Mutex
	err error
}

func newBatchWriter(frameBytes, maxFrames int, onFrame func(int, []byte) error, cancel context.CancelFunc) *batchWriter {
	return &batchWriter{frameBytes: frameBytes, maxFrames: maxFrames, onFrame: onFrame, cancel: cancel, buf: make([]byte, 0, frameBytes)}
}

// Write implements io.Writer for the producer's stdout.
func (w *batchWriter) Write(p []byte) (int, error) {
	if err := w.failure(); err != nil {
		return 0, err
	}
	n := len(p)
	for len(p) > 0 {
		if len(w.buf) == 0 && len(p) >= w.frameBytes {
			if err := w.frame(p[:w.frameBytes]); err != nil {
				return 0, err
			}
			p = p[w.frameBytes:]
			continue
		}
		take := min(w.frameBytes-len(w.buf), len(p))
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		if len(w.buf) == w.frameBytes {
			err := w.frame(w.buf)
			w.buf = w.buf[:0]
			if err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// frame counts one complete frame and hands it on.
func (w *batchWriter) frame(rgb []byte) error {
	if w.maxFrames > 0 && w.frames+1 > w.maxFrames {
		return w.fail(matteFrameCapError(w.maxFrames))
	}
	w.frames++
	if err := w.onFrame(w.frames, rgb); err != nil {
		return w.fail(err)
	}
	return nil
}

// fail records the first failure and cancels the producer; it returns the
// failure in force (safe from any goroutine).
func (w *batchWriter) fail(err error) error {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	err = w.err
	w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
	return err
}

// failure returns the recorded failure, if any.
func (w *batchWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// finish checks that the stream ended on a frame boundary.
func (w *batchWriter) finish() error {
	if len(w.buf) != 0 {
		return fmt.Errorf("AI matte: the clip's frame stream ended mid-frame (%d of %d bytes)", len(w.buf), w.frameBytes)
	}
	return nil
}

// ---- op-level plumbing ------------------------------------------------------

// stripMatteResolved returns ops with the Resolved identity removed from
// every matte op that carries one: jobs resolves it from the persisted
// facts itself (fillMatteResolved), so a client-sent identity never
// reaches a key or the recipe hash — the twin of stripAutoCropResolved.
// The slice is returned unchanged when nothing was stripped; otherwise a
// copy. Malformed params are left for the compiler to report.
func stripMatteResolved(ops []recipe.Op) []recipe.Op {
	var out []recipe.Op
	for i, op := range ops {
		if op.Kind != recipe.OpMatte || !bytes.Contains(op.Params, []byte("resolved")) {
			continue
		}
		var p recipe.MatteParams
		if err := json.Unmarshal(op.Params, &p); err != nil || p.Resolved == nil {
			continue
		}
		p.Resolved = nil
		raw, err := json.Marshal(p)
		if err != nil {
			continue
		}
		if out == nil {
			out = slices.Clone(ops)
		}
		out[i] = recipe.Op{Kind: op.Kind, Params: raw}
	}
	if out == nil {
		return ops
	}
	return out
}

// fillMatteResolved returns ops with every matte op's Resolved identity set
// from the persisted facts (a client-sent one is stripped first), so the
// recipe hash — and with it ResultKey — carries the weights / processing
// version / size / precision the render will use (spec §11): pulling a new
// sidecar image can never serve a cached result made with the old weights.
// Ops without a matte op are returned as is. Submit calls it before hashing;
// no facts yet is ErrMatteUnavailable, an unoffered model ErrInvalidRecipe.
func (m *Manager) fillMatteResolved(ops []recipe.Op) ([]recipe.Op, error) {
	ops = stripMatteResolved(ops)
	if !hasMatteOp(ops) {
		return ops, nil
	}
	facts, err := m.loadMatteFacts()
	if err != nil {
		return nil, err
	}
	device := m.matteEffectiveDevice(&facts.Ping)
	out := slices.Clone(ops)
	for i, op := range out {
		if op.Kind != recipe.OpMatte {
			continue
		}
		p, err := decodeMatteOp(i, op)
		if err != nil {
			return nil, err
		}
		id, err := identityOf(facts, device, matteRequestOf(p))
		if err != nil {
			return nil, err
		}
		var raw recipe.MatteParams
		if len(bytes.TrimSpace(op.Params)) > 0 && !bytes.Equal(bytes.TrimSpace(op.Params), []byte("null")) {
			_ = json.Unmarshal(op.Params, &raw) // validated by decodeMatteOp
		}
		res := id.resolved()
		raw.Resolved = &res
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("encode matte params: %w", err)
		}
		out[i] = recipe.Op{Kind: recipe.OpMatte, Params: data}
	}
	return out, nil
}

// checkMatteResolved compares the identity a job's recipe carries (filled by
// Submit) with what the render resolved: the facts may have changed between
// the two (a probe rewrote models.json with a new sidecar's weights), and a
// result filed under the submitted hash must not be made with another
// identity — the job fails asking for a fresh submit. Ops without a
// Resolved identity are not checked. Each op is paired with its own
// resolution the way the plan's inputs are (findMatteFor: model, size and
// the Phase 5c params — two guided ops of one model with different edge
// models resolve two identities that differ in the edge fields).
func checkMatteResolved(ops []recipe.Op, mattes []resolvedMatte) error {
	for i, op := range ops {
		if op.Kind != recipe.OpMatte {
			continue
		}
		var p recipe.MatteParams
		if err := json.Unmarshal(op.Params, &p); err != nil || p.Resolved == nil {
			continue
		}
		if p.Model == "" {
			p.Model = recipe.MatteModelDefault
		}
		rm := findMatteFor(mattes, matteRequestOf(p))
		if rm == nil {
			return fmt.Errorf("%w: op %d (matte): no resolved AI matte for model %s", ErrInvalidRecipe, i, p.Model)
		}
		got := rm.identity().resolved()
		if got != *p.Resolved {
			return fmt.Errorf("the matte service changed since this render was submitted (model %s: weights %s/%s, proc %s/%s, size %d/%d, %s/%s) — submit the render again",
				p.Model, short(p.Resolved.Weights), short(got.Weights), p.Resolved.Proc, got.Proc, p.Resolved.Size, got.Size, p.Resolved.Precision, got.Precision)
		}
	}
	return nil
}

// findMatte returns the resolved matte of a (model, requested size) — the
// op's request, 0 = the server's default — or nil. A request is matched on
// the size the op asked for when the resolution recorded it (ReqSize), else
// on the effective size (any resolved matte of the model serves a request
// for the default).
func findMatte(mattes []resolvedMatte, model string, reqSize int) *resolvedMatte {
	for i := range mattes {
		rm := &mattes[i]
		if rm.Model != model {
			continue
		}
		switch {
		case rm.ReqSize == reqSize:
			return rm
		case reqSize != 0 && rm.Size == reqSize:
			return rm
		}
	}
	if reqSize == 0 {
		for i := range mattes {
			if mattes[i].Model == model {
				return &mattes[i]
			}
		}
	}
	return nil
}

// matteClipKeys lists the clip keys of mattes in order (what the still,
// proxy and autocrop keys fold in).
func matteClipKeys(mattes []resolvedMatte) []string {
	keys := make([]string, 0, len(mattes))
	for _, rm := range mattes {
		keys = append(keys, rm.ClipKey)
	}
	return keys
}

// matteKeySuffix is the text the preview and autocrop keys append for the
// resolved mattes of a stack ("" without any): the clip keys carry weights
// and proc from the persisted facts, so a weight re-pin or sidecar update
// can never serve a stale memoised still, proxy or box.
func matteKeySuffix(mattes []resolvedMatte) string {
	if len(mattes) == 0 {
		return ""
	}
	return "|matte=" + strings.Join(matteClipKeys(mattes), ",")
}

// fillMatteInputs points every matte input of p (ExtraInput.Matte != nil,
// Source 0) at its memo: Path = <MatteDir>/%06d.png and Matte.Frames from
// the manifest, after the string-exact check that the memo was produced at
// the plan's rate (MatteInput.FPS == manifest fps; with the Output-aware
// matte plan it never differs — the belt to the prefix test's braces). A
// matte input without a resolved matte for its model / size is an
// ErrInvalidRecipe (spec §5.6).
func fillMatteInputs(p *graph.Plan, mattes []resolvedMatte) error {
	for i := range p.ExtraInputs {
		in := &p.ExtraInputs[i]
		if in.Matte == nil {
			continue
		}
		rm := findMatteInput(mattes, in.Matte) // by model, size and the op's 5c params (matte_recipe.go)
		if rm == nil || rm.Manifest == nil {
			return fmt.Errorf("%w: no resolved AI matte for model %s (size %d)", ErrInvalidRecipe, in.Matte.Model, in.Matte.Size)
		}
		if rm.Manifest.FPS != in.Matte.FPS {
			return fmt.Errorf("%w: the AI matte of model %s was produced at %s fps, the plan runs at %s — stale resolution", ErrInvalidRecipe, in.Matte.Model, rm.Manifest.FPS, in.Matte.FPS)
		}
		in.Path = filepath.Join(matteInputDir(rm, in.Matte), matte.FramePattern) // the raw memo, or the derived sequence the op asks for (Phase 5c)
		in.Matte.Frames = rm.Manifest.Frames
	}
	return nil
}

// checkMatteCount is the post-renderMaster check (spec §12): every matte's
// frame count, doubled per bounce op (the merge precedes the bounce stages,
// so the master mirrors the mattes with its frames), must equal the
// master's. The caller skips it for static renders (the master is cut to
// one frame). A mismatch fails the render and KEEPS the memo: the producer
// decodes the same prefix and would reproduce it.
func checkMatteCount(p *graph.Plan, mattes []resolvedMatte, frames int) error {
	for _, rm := range mattes {
		if rm.Manifest == nil {
			continue
		}
		want := rm.Manifest.Frames
		if p.Bounces > 0 && p.Bounces < 31 {
			want <<= p.Bounces
		}
		if want == frames {
			continue
		}
		detail := ""
		if p.Bounces > 0 {
			detail = fmt.Sprintf(" (%d x 2^%d bounce)", rm.Manifest.Frames, p.Bounces)
		}
		return fmt.Errorf("AI matte (%s) has %d frames%s but the clip decoded to %d — report this with the source", rm.Model, want, detail, frames)
	}
	return nil
}

// protectMattes keeps every resolved matte's dir out of the sweeper's hands
// until release is called (idempotent): a render holds it from the matte
// pre-stage to the end of the job, a preview for its ffmpeg run. Each dir
// is checked to still exist once protected (the pass's rename is atomic, so
// a dir either is a complete memo or is gone): a swept one is an error
// asking to retry.
func protectMattes(st *store.Store, mattes []resolvedMatte) (release func(), err error) {
	releases := make([]func(), 0, len(mattes))
	release = func() {
		for _, r := range releases {
			r()
		}
	}
	for _, rm := range mattes {
		if rm.Dir == "" {
			continue
		}
		releases = append(releases, st.Protect(rm.Dir))
		if _, err := os.Stat(filepath.Join(rm.Dir, matte.ManifestName)); err != nil {
			release()
			return func() {}, fmt.Errorf("the AI matte of model %s was removed from disk meanwhile — retry", rm.Model)
		}
	}
	return release, nil
}

// applyMatteInfo appends the render.matte info check to rep (nil-safe):
// the identity and provenance of every matte the render merged ("AI matte:
// isnet-anime 1024 px fp16 · weights f15622d8… · proc 1 · graph 3a1b… · 45
// frames · cuda 18.4 ms/frame"; a guided matte reads "guided (sam2-tiny) +
// edge birefnet-lite · 1024x576 bf16 · weights … · proc 1 · 45 frames ·
// cuda 48.0 ms/frame · edge weights … · 2 prompted frames", or "guided
// (sam2-tiny), tracker mask only · …" without an edge — the "guided (…) +
// edge <model>" form the SPA's Result card and the recipe notes use). What
// the op asked beyond the identity — stabilise, keep colours — is worded
// from the recipe by applyMatteRecipeNotes (matte_recipe.go), which the
// render applies right after this; the integration test greps "stabilise
// light", "keep" and the tracker id out of the finished line.
func applyMatteInfo(rep *discordlint.Report, mattes []resolvedMatte) {
	if rep == nil || len(mattes) == 0 {
		return
	}
	parts := make([]string, 0, len(mattes))
	for _, rm := range mattes {
		man := rm.Manifest
		guided := rm.id.tracker() || (man != nil && man.TrackW > 0)
		var s string
		if guided {
			s = fmt.Sprintf("guided (%s)", rm.Model)
			switch {
			case rm.edge != nil:
				s += " + edge " + rm.edge.Model
			case rm.Edge != "":
				s += " + edge " + rm.Edge
			default:
				s += ", tracker mask only"
			}
			if man != nil && man.TrackW > 0 {
				s += fmt.Sprintf(" · %dx%d", man.TrackW, man.TrackH)
			}
		} else {
			s = fmt.Sprintf("%s %d px", rm.Model, rm.Size)
		}
		if rm.Precision != "" {
			s += " " + rm.Precision
		}
		if man != nil {
			s += " · weights " + short(man.Weights) + " · proc " + man.Proc
			if man.GraphDigest != "" {
				s += " · graph " + short(man.GraphDigest)
			}
			s += fmt.Sprintf(" · %d frames", man.Frames)
			if man.Device != "" {
				s += " · " + man.Device
				if man.MsPerFrame > 0 {
					s += " " + strconv.FormatFloat(man.MsPerFrame, 'f', 1, 64) + " ms/frame"
				}
			}
		}
		if rm.edge != nil && rm.edge.Manifest != nil {
			s += " · edge weights " + short(rm.edge.Manifest.Weights)
		}
		if guided && man != nil && man.Prompts != "" {
			if n := strings.Count(man.Prompts, "|f="); n == 1 {
				s += " · 1 prompted frame"
			} else if n > 1 {
				s += fmt.Sprintf(" · %d prompted frames", n)
			}
			s += maskPromptNote(man.Prompts)
		}
		parts = append(parts, s)
	}
	rep.Checks = append(rep.Checks, discordlint.Check{
		Rule:   RuleRenderMatte,
		Level:  discordlint.LevelInfo,
		OK:     true,
		Detail: "AI matte: " + strings.Join(parts, "; "),
	})
}
