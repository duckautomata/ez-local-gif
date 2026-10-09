package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/probe"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// ---- capabilities -----------------------------------------------------------

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), capabilitiesTimeout)
	defer cancel()
	var versions map[string]string
	if s.jm != nil {
		versions = s.jm.ToolVersions()
	} else {
		versions = s.tools.Versions(ctx)
	}
	if versions == nil {
		versions = map[string]string{}
	}
	conc := 0
	// The render admission caps (jobs/scratch.go), published so the SPA can
	// show the master estimate against them before Render: maxMasterBytes
	// is the frame-master / reverse-buffer cap (jobs.Options.MaxMasterBytes
	// after its default and ceiling clamp), scratchBudgetBytes the scratch
	// byte budget a render's reserve (scratchFactor x master + headroom)
	// must fit — 0 there means unlimited or unknown, and both are 0 without
	// a manager, which the SPA reads as "no estimate verdict".
	var maxMaster, scratchBudget int64
	if s.jm != nil {
		conc = s.jm.Concurrency()
		maxMaster = s.jm.MaxMasterBytes()
		scratchBudget = s.jm.ScratchBudgetBytes()
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"tools":              versions,
		"targets":            targetNames(),
		"limits":             targetLimits(),
		"rulesVersion":       discordlint.RulesVersion,
		"version":            s.cfg.Version,
		"concurrency":        conc,
		"maxUploadBytes":     s.cfg.MaxUploadBytes,
		"maxMasterBytes":     maxMaster,
		"scratchBudgetBytes": scratchBudget,
		"formats":            outputFormats(),
		"features":           s.features(len(s.fonts(ctx)) > 0, versions),
	})
}

// targetNames lists every Discord target recipe.Output.Target accepts, in
// the linter's display order (emote, sticker, then the attachment tiers by
// cap) — the order a UI dropdown should use, which the limits map cannot
// carry.
func targetNames() []string {
	ts := discordlint.Targets()
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = string(t)
	}
	return names
}

// targetLimits maps every Discord target to its byte cap: the emote and
// sticker caps plus one entry per attachment tier.
func targetLimits() map[string]int64 {
	ts := discordlint.Targets()
	limits := make(map[string]int64, len(ts))
	for _, t := range ts {
		limits[string(t)] = discordlint.Limit(t)
	}
	return limits
}

// validateTarget rejects an Output.Target the linter does not know. Neither
// recipe.Validate nor jobs.Submit checks it, and an unknown string would
// render with no Discord rules and no cap (while a near miss such as
// "Emote" is plainly a mistake) and memoise its result under that string,
// so the API refuses it up front, naming the valid targets.
func validateTarget(target string) error {
	if discordlint.Valid(discordlint.Target(target)) {
		return nil
	}
	valid := make([]string, 0, len(discordlint.Targets()))
	for _, t := range discordlint.Targets() {
		valid = append(valid, discordlint.Describe(t))
	}
	return fmt.Errorf("unknown output.target %q; valid targets: %s; omit it for no Discord target", target, strings.Join(valid, ", "))
}

// outputFormats lists the recipe.Output formats this build renders, in the
// order the UI offers them (mp4/webm are the Phase 4 opaque video exports).
func outputFormats() []string {
	return []string{
		recipe.FormatGIF, recipe.FormatWebP, recipe.FormatAPNG, recipe.FormatAVIF,
		recipe.FormatPNG, recipe.FormatJPEG, recipe.FormatFrames,
		recipe.FormatMP4, recipe.FormatWebM,
	}
}

// features flags the capabilities the SPA gates its UI on. Phase 2: fit-to-
// size (Output.FitBytes), image-sequence uploads (several "file" parts) and
// the GIF→GIF optimiser path. Phase 3: background keying (the chromakey /
// colorkey ops), overlays (text / overlay ops, extra "sources"), the
// animated Play preview (POST /api/proxy) and — only when the container can
// enumerate faces, i.e. GET /api/fonts is non-empty — the font picker.
// Phase 4: the /input picker and the /output save (the job manager's
// startup directory checks, DESIGN.md §4.4) and the gifski HQ GIF encoder —
// flagged from versions (the capabilities tool map, ffrun.Tools.Versions),
// i.e. only when the resolved binary actually answered its version probe.
// A bare resolved path is not enough: an EZLG_GIFSKI override is used
// verbatim without an existence check (ffrun.LookupTools), so a typo'd path
// would otherwise advertise an encoder whose every render fails at exec
// time; the probe hides it instead. "feather" and "bounce" name the Phase 4
// op kinds explicitly so the SPA can gate those cards without inferring
// support from the formats list (web capabilities.svelte.ts
// phase4OpsOffered). Phase 5a: "morph" names the morph op kind
// (recipe.OpMorph — the 3x3 alpha close / grow cleanup hoisted into the
// keying group like feather) the same way, so the SPA can gate the
// Background card's Edge cleanup fold on older servers, which 400 the
// unknown kind. Phase 5b: "matte" is the AI matte op kind (recipe.OpMatte)
// AND a live condition — jobs.Manager.MatteEnabled: a matte sidecar is
// configured (EZLG_MATTE_URL, the compose matte / matte-gpu profiles) and
// answered its last probes. It is false on a plain install, so the SPA
// greys the Background card's AI mode with the reason GET /api/matte
// carries; POST /api/jobs refuses a matte op with 400 while it is false.
// Every flag but the five host-dependent ones (fonts, inputPick,
// outputSave, gifski, matte) is a property of this build.
func (s *Server) features(fonts bool, versions map[string]string) map[string]bool {
	inputPick, outputSave, matte := false, false, false
	if s.jm != nil {
		inputPick = s.jm.InputPickEnabled()
		outputSave = s.jm.OutputSaveEnabled()
		matte = s.jm.MatteEnabled()
	}
	return map[string]bool{
		"fit": true, "sequence": true, "optimize": true,
		"keying": true, "overlays": true, "proxy": true, "fonts": fonts,
		"feather": true, "bounce": true, "morph": true,
		"inputPick": inputPick, "outputSave": outputSave,
		"gifski": versions["gifski"] != "",
		"matte":  matte,
	}
}

// ---- matte (Phase 5b) -----------------------------------------------------------

// matteProfileHint names the compose profile that starts the sidecar; every
// "AI matte is off" message ends with it so an operator knows what to run.
const matteProfileHint = "start the matte sidecar with \"docker compose --profile matte-gpu up -d\" (CPU: \"--profile matte\")"

// matteStatus is the manager's live view of the sidecar (jobs.MatteStatus:
// the last probe), or a disabled status on a server without a manager —
// Models is always an object, never null, so the SPA can iterate it.
func (s *Server) matteStatus() jobs.MatteStatus {
	if s.jm == nil {
		return jobs.MatteStatus{
			Reason:       "no job manager",
			DefaultModel: recipe.MatteModelDefault,
			Models:       map[string]jobs.MatteModelStatus{},
		}
	}
	st := s.jm.MatteStatus()
	if st.Models == nil {
		st.Models = map[string]jobs.MatteModelStatus{}
	}
	return st
}

// matteEnabled is features.matte: whether a matte pass can run right now.
func (s *Server) matteEnabled() bool {
	return s.jm != nil && s.jm.MatteEnabled()
}

// handleMatte answers GET /api/matte with the matte sidecar's state as of
// the app's last probe (jobs.Manager.MatteStatus — a snapshot, never a
// network round trip): {enabled, device, reason, gpu, defaultModel,
// models: {id: {label, state, percent, msPerFrame, reason, licence, sizes}},
// maxSeconds, maxFrames}, plus the Phase 5c fields (each omitted when the
// sidecar did not report it): "devices" (every device it offers — the
// "Run on" select shows when there are several), "defaultModels" (its
// default model per device), per model "kind" ("segmenter", or "tracker"
// for the guided model), "resident" (a session is loaded on the default
// device right now — "ready" alone means downloaded and self-tested, the
// first pass adds the model load) and "devices" {dev: {state, reason,
// precision, percent, size, msPerFrame, resident}} — that device's own
// readiness, estimate and residency.
// "device" is the EFFECTIVE device passes run on: the preference PUT
// /api/matte/settings set, else the sidecar's default; "defaultModel" is
// the default for it. The SPA polls it every few seconds while the AI mode
// is visible or a pass is pending, so it must never be cached.
func (s *Server) handleMatte(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, s.matteStatus())
}

// mattePendingResponse is the body of the 202 a still or proxy answers
// while its matte is not on disk yet (jobs.ErrMattePending): the pending
// fields — "pending" is always "matte" — flattened together with the
// GET /api/matte object (jobs.MatteStatus embedded, so its fields sit at
// the top level: enabled, device, reason, gpu, defaultModel, models,
// maxSeconds, maxFrames), which the SPA would otherwise have to fetch
// separately for the pill text. The names never collide: the status has no
// pending/state/done/total/percent/estimateMs.
type mattePendingResponse struct {
	Pending       string `json:"pending"`                 // "matte"
	State         string `json:"state"`                   // running / idle / loading / downloading (jobs.MattePending*; idle = nothing started, Phase 5c)
	Phase         string `json:"phase,omitempty"`         // "tracking" while a guided (tracker) pass's one POST is in flight (jobs.MattePhaseTracking: the masks arrive together at its end, so done stays 0 meanwhile); absent otherwise
	Done          int    `json:"done"`                    // frames matted so far
	Total         int    `json:"total"`                   // frames the pass will matte (0 = unknown)
	Percent       int    `json:"percent"`                 // the pass's or the download's progress, 0..100
	EstimateMS    int64  `json:"estimateMs"`              // the pass's estimated wall time (0 = unknown)
	PendingReason string `json:"pendingReason,omitempty"` // Phase 5d: why an idle answer can start nothing from here — a mask prompt whose edge matte is not computed ("compute the General matte first …", jobs.ErrMattePending.Reason); absent otherwise. Never the status's reason.
	jobs.MatteStatus
}

// mattePendingBody builds the 202 body for one pending error: the error's
// own figures plus the live status; the error's Device (the device the pass
// runs on) wins over the status's when it is set. The error's Reason
// (Phase 5d: an idle nothing from here can start — a mask prompt whose edge
// matte is not computed: "compute the General matte first …" — which the
// SPA shows under the panel) rides as its OWN field, pendingReason: the
// embedded status's reason stays the status's (why the feature or the
// device is off; "" on a healthy stack), because the SPA installs the 202's
// status object wholesale and every reader of status.reason would
// otherwise get the pending sentence until the next GET /api/matte.
func (s *Server) mattePendingBody(e *jobs.ErrMattePending) mattePendingResponse {
	body := mattePendingResponse{
		Pending:       "matte",
		State:         e.State,
		Phase:         e.Phase,
		Done:          e.Done,
		Total:         e.Total,
		Percent:       e.Percent,
		EstimateMS:    e.EstimateMS,
		PendingReason: e.Reason,
		MatteStatus:   s.matteStatus(),
	}
	if e.Device != "" {
		body.Device = e.Device
	}
	return body
}

// hasOp reports whether ops holds an op of the given kind.
func hasOp(ops []recipe.Op, kind string) bool {
	for _, op := range ops {
		if op.Kind == kind {
			return true
		}
	}
	return false
}

// matteOffMessage is the 400 text for a recipe with a matte op while
// features.matte is false: the manager's reason (no EZLG_MATTE_URL, the
// sidecar not answering, …) followed by the compose profile to start.
func (s *Server) matteOffMessage() string {
	reason := s.matteStatus().Reason
	if reason == "" {
		reason = "the matte sidecar is not available"
	}
	return "AI matte is off on this server: " + reason + " — " + matteProfileHint + ", or remove the matte op"
}

// matteUnavailableMessage is the text for a jobs.ErrMatteUnavailable from
// a render or preview: the manager's own message (it carries the sidecar's
// reason when there is one), then the profile hint unless it already names
// the profile.
func matteUnavailableMessage(err error) string {
	msg := errText(err)
	if strings.Contains(msg, "--profile") {
		return msg
	}
	return msg + " — " + matteProfileHint
}

// ---- matte settings / unload / prompt (Phase 5c) ------------------------------

// matteSettingsRequest is the body of PUT /api/matte/settings. Device is a
// pointer so a body without the key is told from "" (the reset): a client
// that misspells the key must not silently reset the preference.
type matteSettingsRequest struct {
	Device *string `json:"device"`
}

// maxMatteDeviceLen bounds a device name in a settings request.
const maxMatteDeviceLen = 32

// validMatteDevice reports whether dev is shaped like a sidecar device
// name: "" (the reset) or a short lowercase token such as "cuda" / "cpu".
// Which devices exist is the sidecar's business (GET /api/matte "devices",
// matteDeviceOffered); this only keeps junk out of the settings file and
// the error messages.
func validMatteDevice(dev string) bool {
	if len(dev) > maxMatteDeviceLen {
		return false
	}
	for _, c := range dev {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// matteDeviceOffered refuses a device the status snapshot says the sidecar
// does not offer — the specific 400 ("offered: cuda, cpu") before the
// manager is asked. "" (the reset) always passes, and so does any device
// while the snapshot lists none (no sidecar answer yet, or one that
// predates the devices list): the manager's own validation decides then.
func matteDeviceOffered(st jobs.MatteStatus, dev string) error {
	if dev == "" || len(st.Devices) == 0 || slices.Contains(st.Devices, dev) {
		return nil
	}
	return fmt.Errorf("device %q is not offered by the matte service (offered: %s); \"\" resets to its default", dev, strings.Join(st.Devices, ", "))
}

// handleMatteSettings answers PUT /api/matte/settings {"device": "cuda" |
// "cpu" | ""} — the server-side device preference for matte passes
// (jobs.Manager.SetMatteDevice: validated against the devices the sidecar
// offers, persisted under /data/mattes/settings.json; "" resets to the
// sidecar's default). The device is a preference of the install, never a
// recipe parameter, which is why it has an endpoint of its own. 400 for a
// body without "device", a malformed name or a device the sidecar does not
// offer; 503 naming the compose profile when there is no sidecar to
// validate against; 200 with the GET /api/matte object otherwise, whose
// "device" is now the effective one and "defaultModel" its default.
func (s *Server) handleMatteSettings(w http.ResponseWriter, r *http.Request) {
	var req matteSettingsRequest
	if !decodeJSON(w, r, &req, "matte settings") {
		return
	}
	if req.Device == nil {
		writeError(w, http.StatusBadRequest, `"device" is required: a device GET /api/matte lists ("cuda", "cpu"), or "" to reset to the matte service's default`)
		return
	}
	dev := *req.Device
	if !validMatteDevice(dev) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("device %q is not a device name; use one GET /api/matte lists (\"cuda\", \"cpu\") or \"\" to reset", dev))
		return
	}
	if err := matteDeviceOffered(s.matteStatus(), dev); err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	if s.jm == nil {
		writeError(w, http.StatusServiceUnavailable, "AI matte is off on this server: no job manager")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), matteSettingsTimeout)
	defer cancel()
	if err := s.jm.SetMatteDevice(ctx, dev); err != nil {
		matteSettingsError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, s.matteStatus())
}

// matteSettingsError answers a failed SetMatteDevice: a device the sidecar
// does not offer (the manager wraps jobs.ErrInvalidRecipe, as it does for
// an unoffered model) is 400; no sidecar to validate against
// (jobs.ErrMatteUnavailable) 503 naming the compose profile; a wait past
// the deadline 504; anything else (the settings file not writable) 500. A
// client that hung up gets nothing.
func matteSettingsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case r.Context().Err() != nil, errors.Is(err, context.Canceled):
		return // client went away; nobody is listening
	case errors.Is(err, jobs.ErrMatteUnavailable):
		writeError(w, http.StatusServiceUnavailable, matteUnavailableMessage(err))
	case errors.Is(err, jobs.ErrInvalidRecipe):
		writeError(w, http.StatusBadRequest, errText(err))
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "matte settings timed out")
	default:
		log.Printf("server: matte settings: %v", err)
		writeError(w, http.StatusInternalServerError, errText(err))
	}
}

// handleMatteUnload answers POST /api/matte/unload: the sidecar is asked to
// release every resident model session now (jobs.Manager.UnloadMatte —
// the SPA calls it when the Background card leaves the AI mode or is
// disabled, so nothing stays loaded between uses). Best effort by
// contract: the answer is 204 whether or not a sidecar was there to ask —
// a failure is logged, never reported, since the sidecar's idle TTL
// releases the models anyway.
func (s *Server) handleMatteUnload(w http.ResponseWriter, r *http.Request) {
	if s.jm != nil {
		ctx, cancel := context.WithTimeout(r.Context(), matteUnloadTimeout)
		defer cancel()
		if err := s.jm.UnloadMatte(ctx); err != nil && r.Context().Err() == nil {
			log.Printf("server: matte unload: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// promptRequest is the body of POST /api/matte/prompt: the preview
// request's sources, ops and output (the recipe whose matte plan the frame
// is taken from; t / maxW / maxSeconds / eager are ignored), the OUTPUT
// frame index to mask and the prompts. Prompts is the matte op's own
// "prompts" array ([{frame, points: [[x, y, label]], box: [x0, y0, x1,
// y1], maskFrom: "edge"}], the value the SPA serialises into the op — one
// state feeds both) or, equivalently, a {obj, prompts: [...]} object
// (matte.TrackPrompts, whose flag "mask" marks the mask prompt as maskFrom
// does); decodePrompts tells them apart. Only the prompts of Frame are used.
type promptRequest struct {
	previewRequest
	Frame   int             `json:"frame"`
	Prompts json.RawMessage `json:"prompts"`
}

// promptWire is one frame prompt as a prompt request carries it: a
// matte.FramePrompt plus the recipe's maskFrom (recipe.MattePrompt, Phase
// 5d). The SPA posts the matte op's prompts array, whose mask prompt is
// {frame, maskFrom: "edge"}, and the client flag "mask" of the same prompt
// is read too: either marks the frame's mask prompt (the mask itself is
// never the client's — jobs takes it from the edge model's memo and fills
// the digest). A maskFrom other than "" / recipe.MattePromptMaskEdge is
// refused, as the graph refuses it in a recipe.
type promptWire struct {
	matte.FramePrompt
	MaskFrom string `json:"maskFrom"`
}

// prompt is the matte client's prompt for the wire one; i is its index in
// the array, for the message.
func (p promptWire) prompt(i int) (matte.FramePrompt, error) {
	fp := p.FramePrompt
	switch p.MaskFrom {
	case "":
	case recipe.MattePromptMaskEdge:
		fp.Mask = true
	default:
		return fp, fmt.Errorf("invalid prompts: prompts[%d] (frame %d): maskFrom must be %q (the edge model's matte of the frame) or absent, got %q", i, fp.Frame, recipe.MattePromptMaskEdge, p.MaskFrom)
	}
	return fp, nil
}

// decodePrompts reads the "prompts" field of a prompt request in either
// shape: a bare array of frame prompts (the recipe's matte op form; the one
// tracked object is implied) or a matte.TrackPrompts object. In both a
// prompt's maskFrom ("edge") or mask (true) marks it as the frame's mask
// prompt (promptWire).
func decodePrompts(raw json.RawMessage) (matte.TrackPrompts, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || string(data) == "null" {
		return matte.TrackPrompts{}, errors.New("prompts is required: the matte op's prompts array [{frame, points, box, maskFrom}]")
	}
	var (
		obj  int
		wire []promptWire
	)
	switch data[0] {
	case '[':
		if err := json.Unmarshal(data, &wire); err != nil {
			return matte.TrackPrompts{}, fmt.Errorf("invalid prompts: %w", err)
		}
		obj = 1
	case '{':
		var tp struct {
			Obj     int          `json:"obj"`
			Prompts []promptWire `json:"prompts"`
		}
		if err := json.Unmarshal(data, &tp); err != nil {
			return matte.TrackPrompts{}, fmt.Errorf("invalid prompts: %w", err)
		}
		obj, wire = tp.Obj, tp.Prompts
	default:
		return matte.TrackPrompts{}, errors.New("prompts must be an array of frame prompts [{frame, points, box, maskFrom}] or a {obj, prompts} object")
	}
	out := matte.TrackPrompts{Obj: obj}
	for i, p := range wire {
		fp, err := p.prompt(i)
		if err != nil {
			return matte.TrackPrompts{}, err
		}
		out.Prompts = append(out.Prompts, fp)
	}
	return out, nil
}

// matteOpModel finds the recipe's matte op and returns its model id ("" =
// the default): an error without one, or when its params do not decode.
func matteOpModel(ops []recipe.Op) (string, error) {
	for i, op := range ops {
		if op.Kind != recipe.OpMatte {
			continue
		}
		var p recipe.MatteParams
		if len(op.Params) > 0 {
			if err := json.Unmarshal(op.Params, &p); err != nil {
				return "", fmt.Errorf("op %d (matte): invalid params: %w", i, err)
			}
		}
		return p.Model, nil
	}
	return "", fmt.Errorf("ops must include a matte op whose model is the guided (tracker) model %q", recipe.MatteModelSAM2Tiny)
}

// trackerModel reports whether model is a tracker — the guided model the
// prompt mask comes from: recipe.MatteModelSAM2Tiny by id, or any model
// the status lists with kind "tracker". Never "": the default is a
// per-frame segmenter on every device.
func trackerModel(model string, st jobs.MatteStatus) bool {
	if model == "" {
		return false
	}
	if model == recipe.MatteModelSAM2Tiny {
		return true
	}
	return st.Models[model].Kind == matte.KindTracker
}

// handleMattePrompt answers POST /api/matte/prompt {src | sources, ops,
// output, frame, prompts} with the guided model's mask of one output frame
// under that frame's prompts (jobs.Manager.MattePromptMask: the frame is
// rendered from the matte plan at the tracking size and the sidecar's
// tracker answers at once from its image predictor) — the live overlay the
// SPA draws after every click or box. The answer is image/png (an 8-bit
// gray mask; private, cacheable like a still), or the same 202 pending body
// as /api/still while the tracker is loading or downloading. 400 when the
// recipe's matte op is missing or its model is not a tracker, when
// "prompts" is missing or malformed (an unknown maskFrom included), when
// frame holds no prompts, or when its prompts have no box, no positive
// point and no mask (the sidecar would refuse them — one negative click
// selects nothing); 404 for an unknown source,
// 409 for one not probed yet; 503 naming the compose profile when no
// sidecar can answer.
func (s *Server) handleMattePrompt(w http.ResponseWriter, r *http.Request) {
	var req promptRequest
	if !decodeJSON(w, r, &req, "prompt request") {
		return
	}
	srcs, err := req.sourceList()
	if err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	if req.Frame < 0 {
		writeError(w, http.StatusBadRequest, "frame must be an output frame index (0 or more)")
		return
	}
	model, err := matteOpModel(req.Ops)
	if err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	if !trackerModel(model, s.matteStatus()) {
		if model == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("the matte op uses the default model, a per-frame segmenter: the live mask needs the guided model (\"model\": %q)", recipe.MatteModelSAM2Tiny))
		} else {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("matte model %q is not a tracker: the live mask needs the guided model (%q)", model, recipe.MatteModelSAM2Tiny))
		}
		return
	}
	all, err := decodePrompts(req.Prompts)
	if err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	prompts := all.ForFrame(req.Frame)
	if len(prompts.Prompts) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("no prompts on frame %d: the mask is of the prompted frame (draw a box or add a positive point on it)", req.Frame))
		return
	}
	if err := prompts.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	if !s.checkSources(w, srcs, http.StatusNotFound) {
		return
	}
	if s.jm == nil {
		writeError(w, http.StatusServiceUnavailable, "AI matte is off on this server: no job manager")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), stillTimeout)
	defer cancel()
	png, err := s.jm.MattePromptMask(ctx, srcs, req.Ops, req.Output, req.Frame, prompts)
	if err != nil {
		s.previewError(w, r, err, http.StatusNotFound, "prompt mask")
		return
	}
	writePreview(w, "image/png", png)
}

// ---- fonts --------------------------------------------------------------------

// fonts lists the drawtext-usable faces of this server: what the job
// manager found with fc-list, cached for its lifetime. Never nil, so the
// JSON is always an array.
func (s *Server) fonts(ctx context.Context) []enc.Font {
	if s.jm == nil {
		return []enc.Font{}
	}
	if fonts := s.jm.Fonts(ctx); fonts != nil {
		return fonts
	}
	return []enc.Font{}
}

// handleFonts answers GET /api/fonts with {"fonts": [...]} — empty when
// fc-list is not available (drawtext then still resolves the bundled
// families by name). The list only changes with a restart (the manager
// caches it), but it is small, so clients are asked to revalidate.
func (s *Server) handleFonts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), fontsTimeout)
	defer cancel()
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"fonts": s.fonts(ctx)})
}

// ---- upload / sources ---------------------------------------------------------

// handleUpload streams the multipart body (see upload.go): one "file" part
// becomes a blob source, several become an image-sequence source. The
// rules for several parts are enforced while reading, so a rejected upload
// stops early and leaves nothing behind.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > s.cfg.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds the %d byte limit", s.cfg.MaxUploadBytes))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data with a \"file\" field: "+errText(err))
		return
	}

	up, err := s.readUpload(mr)
	defer up.close()
	if err != nil {
		var ue *uploadError
		if errors.As(err, &ue) {
			writeError(w, ue.Status, ue.Msg)
			return
		}
		s.uploadReadError(w, err)
		return
	}
	if !up.hasFirst {
		writeError(w, http.StatusBadRequest, "multipart body has no \"file\" field")
		return
	}
	if up.isSequence() {
		s.finishSequenceUpload(w, r, up)
		return
	}
	if up.firstSize == 0 {
		writeError(w, http.StatusBadRequest, "uploaded file is empty")
		return
	}
	blob, err := s.storeFirstPart(up)
	if err != nil {
		log.Printf("server: store upload: %v", err)
		writeError(w, http.StatusInternalServerError, "upload failed: "+errText(err))
		return
	}
	s.answerSource(w, r, blob, s.probeFile)
}

// finishSequenceUpload turns the staged parts of a multi-file upload into a
// sequence blob, probes it at the requested delay and answers with the
// source. The staged files (the first part included) never entered the blob
// store, so nothing has to be deleted from it on any path.
func (s *Server) finishSequenceUpload(w http.ResponseWriter, r *http.Request, up *upload) {
	parts, closeParts, err := up.sequenceParts()
	if err != nil {
		log.Printf("server: upload sequence: %v", err)
		writeError(w, http.StatusInternalServerError, "upload failed: "+errText(err))
		return
	}
	blob, err := s.st.PutSequence(parts)
	closeParts()
	if err != nil {
		if errors.Is(err, store.ErrMixedSequence) {
			writeError(w, http.StatusBadRequest, errText(err))
			return
		}
		log.Printf("server: store image sequence: %v", err)
		writeError(w, http.StatusInternalServerError, "storing the image sequence failed: "+errText(err))
		return
	}
	s.answerSource(w, r, blob, func(ctx context.Context, b *store.Blob) (recipe.ProbeInfo, error) {
		return probe.ProbeSequence(ctx, s.tools, b.Path, up.delayMS)
	})
}

// prober describes one blob (a file or a sequence dir) for answerSource.
type prober func(ctx context.Context, b *store.Blob) (recipe.ProbeInfo, error)

// probeFile is the prober for ordinary (single-file) blobs.
func (s *Server) probeFile(ctx context.Context, b *store.Blob) (recipe.ProbeInfo, error) {
	return probe.Probe(ctx, s.tools, b.Path, probeScanFrames)
}

// answerSource completes every path that makes a blob a source: a blob
// without probe info is probed (under probeTimeout) and the info stored,
// then the source is written. Probe failures go through probeFailed.
func (s *Server) answerSource(w http.ResponseWriter, r *http.Request, blob *store.Blob, pr prober) {
	if blob.Info == nil {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		info, err := pr(ctx, blob)
		if err != nil {
			s.probeFailed(w, r, blob.Hash, err)
			return
		}
		if err := s.st.SetBlobInfo(blob.Hash, info); err != nil {
			log.Printf("server: store probe info for %s: %v", blob.Hash, err)
			writeError(w, http.StatusInternalServerError, "failed to store probe info: "+errText(err))
			return
		}
		blob.Info = &info
		s.warmStill(blob.Hash, info)
	}
	writeJSON(w, http.StatusOK, sourceOf(blob))
}

// probeFailed answers an upload whose probe failed. Only a file that ffprobe
// ran on but could not make sense of is the client's fault: that blob is
// dead weight and is dropped (422) so the sweeper never has to and a retry
// re-probes from scratch. Everything else — ffprobe missing or not
// executable (500), the probe timing out (504) — is the server's problem;
// the blob is kept, unprobed, so a re-upload dedupes it and probes again. A
// client that hung up mid-probe gets no answer at all.
func (s *Server) probeFailed(w http.ResponseWriter, r *http.Request, hash string, err error) {
	switch {
	case r.Context().Err() != nil, errors.Is(err, context.Canceled):
		return // client went away; nobody is listening
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("server: probe %s: timed out after %s", hash, probeTimeout)
		writeError(w, http.StatusGatewayTimeout, "probing the upload timed out")
	case unreadableSource(err):
		s.discardBlob(hash)
		writeError(w, http.StatusUnprocessableEntity, "cannot read this file as an image or video: "+errText(err))
	default:
		log.Printf("server: probe %s: %v", hash, err)
		writeError(w, http.StatusInternalServerError, "probe failed: "+errText(err))
	}
}

// unreadableSource reports whether a probe error means "ffprobe ran and
// could not read this file" — it exited non-zero on the file, its output
// was not the JSON it prints for anything it can open, it found no video
// stream, or the stream it found has no dimensions (garbage behind an image
// extension: the image2 demuxer trusts the name and the decoder gives up) —
// as opposed to ffprobe itself failing to run.
func unreadableSource(err error) bool {
	var (
		exit   *exec.ExitError
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	return errors.Is(err, probe.ErrNoVideo) ||
		errors.As(err, &exit) ||
		errors.As(err, &syntax) ||
		errors.As(err, &typ) ||
		strings.Contains(err.Error(), noDimensionsMarker)
}

// noDimensionsMarker is the phrase package probe uses for a video stream
// whose width or height is 0 ("video stream has no dimensions (0x0)",
// "frame 1 has no dimensions (0x0)"); it has no sentinel of its own.
const noDimensionsMarker = "has no dimensions"

// warmStill pre-renders the first preview frame in the background
// (DESIGN.md §8: still pre-warmed the moment the upload lands) so the UI's
// first /api/still hits the memo. The request therefore mirrors what the
// SPA sends for a fresh source — the default preset's geometry (none), t=0,
// 480 px wide, and the unpremultiply op it turns on for a premultiplied
// source (state.svelte.ts defaultOps) — since anything else would key a
// different memo entry. Best effort, errors are ignored. The render runs
// under the server lifetime, so Shutdown kills it rather than orphaning an
// ffmpeg.
func (s *Server) warmStill(hash string, info recipe.ProbeInfo) {
	if s.jm == nil || s.tools.FFmpeg == "" {
		return
	}
	ops, out, t, maxW := warmStillRequest(info)
	s.spawn(func() {
		ctx, cancel := context.WithTimeout(s.ctx, stillTimeout)
		defer cancel()
		_, _ = s.jm.Still(ctx, hash, ops, out, t, maxW)
	})
}

// warmStillRequest is the still the SPA asks for first after an upload.
func warmStillRequest(info recipe.ProbeInfo) (ops []recipe.Op, out recipe.Output, t float64, maxW int) {
	if info.Premultiplied {
		ops = []recipe.Op{{Kind: recipe.OpUnpremultiply}}
	}
	return ops, recipe.Output{Format: "gif"}, 0, jobs.DefaultStillWidth
}

// discardBlob removes a blob that turned out to be unusable (best effort).
func (s *Server) discardBlob(hash string) {
	if err := s.st.DeleteBlob(hash); err != nil {
		log.Printf("server: discard blob %s: %v", hash, err)
	}
}

// uploadReadError maps body-read failures to 413 (limit hit) or 400/500.
func (s *Server) uploadReadError(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds the %d byte limit", s.cfg.MaxUploadBytes))
		return
	}
	// A client that hangs up mid-upload is not a server error.
	if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "multipart:") {
		writeError(w, http.StatusBadRequest, "malformed or truncated upload: "+errText(err))
		return
	}
	log.Printf("server: upload: %v", err)
	writeError(w, http.StatusInternalServerError, "upload failed: "+errText(err))
}

func sourceOf(b *store.Blob) recipe.Source {
	src := recipe.Source{Hash: b.Hash, Name: b.Name, Size: b.Size}
	if b.Info != nil {
		src.Info = *b.Info
	}
	return src
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !recipe.IsHash(hash) {
		writeError(w, http.StatusNotFound, "not a source hash")
		return
	}
	blob, err := s.st.GetBlob(hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown source")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	if blob.Info == nil {
		writeError(w, http.StatusConflict, "source has not been probed yet; upload it again")
		return
	}
	writeJSON(w, http.StatusOK, sourceOf(blob))
}

// fromResultRequest is the body of POST /api/sources/from-result.
type fromResultRequest struct {
	RecipeHash string `json:"recipeHash"`
	Name       string `json:"name"`
}

// handleSourceFromResult makes a rendered result file a source of its own
// ("edit as source"): the named file of the result — one the manifest lists,
// not the manifest or report — is copied into the blob store under its
// result file name, probed like an upload and answered as a recipe.Source.
// A file that is already a blob dedupes, so chaining is free.
func (s *Server) handleSourceFromResult(w http.ResponseWriter, r *http.Request) {
	var req fromResultRequest
	if !decodeJSON(w, r, &req, "from-result request") {
		return
	}
	if !recipe.IsHash(req.RecipeHash) {
		writeError(w, http.StatusBadRequest, "recipeHash must be a recipe hash")
		return
	}
	if !validResultName(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be a plain result file name")
		return
	}
	res, err := s.jm.LoadResult(req.RecipeHash)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no result for this recipe")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	file := resultFile(res, req.Name)
	if file == nil {
		writeError(w, http.StatusNotFound, "no such file in this result")
		return
	}
	if file.Kind == jobs.FileKindArchive || strings.EqualFold(path.Ext(req.Name), ".zip") {
		writeError(w, http.StatusBadRequest, "an archive cannot be used as a source; pick a frame or an output file")
		return
	}
	full := filepath.Join(s.st.ResultDir(req.RecipeHash), req.Name)
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "result file is missing")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "result file is missing")
		return
	}
	blob, err := s.st.PutBlob(f, req.Name)
	if err != nil {
		log.Printf("server: from-result %s/%s: %v", req.RecipeHash, req.Name, err)
		writeError(w, http.StatusInternalServerError, "copying the result into the store failed: "+errText(err))
		return
	}
	if blob.Size == 0 {
		s.discardBlob(blob.Hash)
		writeError(w, http.StatusUnprocessableEntity, "result file is empty")
		return
	}
	s.answerSource(w, r, blob, s.probeFile)
}

// resultFile returns the manifest entry named name, or nil.
func resultFile(res *jobs.Result, name string) *jobs.File {
	for i := range res.Files {
		if res.Files[i].Name == name {
			return &res.Files[i]
		}
	}
	return nil
}

// ---- previews: still + proxy ------------------------------------------------

// previewRequest is the body of POST /api/still and POST /api/proxy: the
// recipe's sources and op stack plus the preview's own knobs. "src" (the
// one main source, Phase 1) and "sources" (the recipe's list, main source
// first, overlay assets after it — Phase 3) are alternatives; when both are
// given they must name the same main source. T is the still's output time,
// MaxSeconds the proxy's length cap; each endpoint ignores the other's.
// Eager says the user asked for the AI matte pass explicitly — the Compute
// matte button, or a render-bound preview — and is what starts a pass
// (jobs.WithMatteEager): since Phase 5c a preview never starts one on its
// own, so a plain still or proxy whose matte is not on disk, with no pass
// in flight, answers 202 "idle" and keeps the last picture (Play behaves
// like a still; a render always runs the pass).
type previewRequest struct {
	Src        string        `json:"src"`
	Sources    []string      `json:"sources"`
	Ops        []recipe.Op   `json:"ops"`
	Output     recipe.Output `json:"output"`
	T          float64       `json:"t"`
	MaxW       int           `json:"maxW"`
	MaxSeconds float64       `json:"maxSeconds"`
	Eager      bool          `json:"eager"`
}

// sourceList resolves src/sources into the recipe's source list, every
// entry a well-formed hash.
func (p *previewRequest) sourceList() ([]string, error) {
	srcs := p.Sources
	switch {
	case len(srcs) == 0 && p.Src == "":
		return nil, errors.New("src (one source hash) or sources (the recipe's hashes) is required")
	case len(srcs) == 0:
		srcs = []string{p.Src}
	case p.Src != "" && p.Src != srcs[0]:
		return nil, errors.New("src and sources[0] name different main sources")
	}
	for i, h := range srcs {
		if recipe.IsHash(h) {
			continue
		}
		if i == 0 {
			return nil, errors.New("src must be a source hash")
		}
		return nil, fmt.Errorf("sources[%d] must be a source hash", i)
	}
	return srcs, nil
}

// checkSources verifies that every source names a blob with probe info,
// answering the request otherwise: a missing blob gets missingStatus (404
// for stills, whose unknown main source has always been a 404; 400 for
// proxies and jobs, where the recipe is the client's claim), an unprobed
// one 409 — both naming the index, which is how overlay ops refer to
// sources. Reports whether the request may proceed.
func (s *Server) checkSources(w http.ResponseWriter, srcs []string, missingStatus int) bool {
	for i, h := range srcs {
		blob, err := s.st.GetBlob(h)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, missingStatus, fmt.Sprintf("source %d (%s…) is not uploaded", i, h[:12]))
				return false
			}
			writeError(w, http.StatusInternalServerError, errText(err))
			return false
		}
		if blob.Info == nil {
			writeError(w, http.StatusConflict, fmt.Sprintf("source %d (%s…) has not been probed yet; upload it again", i, h[:12]))
			return false
		}
	}
	return true
}

// handleStill answers POST /api/still with one preview frame
// (jobs.Manager.StillSources: srcs[0] is the main source, the rest the
// blobs behind the recipe's overlay sources).
func (s *Server) handleStill(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if !decodeJSON(w, r, &req, "still request") {
		return
	}
	srcs, err := req.sourceList()
	if err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	if !s.checkSources(w, srcs, http.StatusNotFound) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), stillTimeout)
	defer cancel()
	ctx = jobs.WithMatteEager(ctx, req.Eager)
	png, err := s.jm.StillSources(ctx, srcs, req.Ops, req.Output, req.T, req.MaxW)
	if err != nil {
		s.previewError(w, r, err, http.StatusNotFound, "still render")
		return
	}
	writePreview(w, "image/png", png)
}

// handleProxy answers POST /api/proxy with the animated low-resolution WebP
// preview of the op stack (jobs.Manager.Proxy): what the UI's Play button
// shows. Same request shape, guard and cancellation as /api/still; a
// missing source is the client's mistake here (400).
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if !decodeJSON(w, r, &req, "proxy request") {
		return
	}
	srcs, err := req.sourceList()
	if err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	if req.MaxW < 0 || req.MaxSeconds < 0 {
		writeError(w, http.StatusBadRequest, "maxW and maxSeconds must not be negative (0 = default)")
		return
	}
	if !s.checkSources(w, srcs, http.StatusBadRequest) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()
	ctx = jobs.WithMatteEager(ctx, req.Eager)
	webp, err := s.jm.Proxy(ctx, srcs, req.Ops, req.Output, req.MaxW, req.MaxSeconds)
	if err != nil {
		s.previewError(w, r, err, http.StatusBadRequest, "proxy render")
		return
	}
	writePreview(w, "image/webp", webp)
}

// previewError answers a failed still/proxy render (and a failed prompt
// mask, which is a still of the matte plan): a matte that is not on disk
// yet (Phase 5b, jobs.ErrMattePending — the pass runs, nothing was started
// because the request was not eager ("idle", Phase 5c), or it waits on the
// model loading / downloading) is a 202 Accepted carrying
// mattePendingResponse, which the SPA polls on; a matte no sidecar can
// produce (jobs.ErrMatteUnavailable) is a 503 naming the compose profile;
// then an unknown source gets missingStatus, a recipe the compiler refused
// 400, a render past its deadline 504, anything else 500. A client that
// hung up gets nothing. The two matte cases come first: jobs may wrap
// either together with ErrInvalidRecipe, and the pending one is not an
// error at all.
func (s *Server) previewError(w http.ResponseWriter, r *http.Request, err error, missingStatus int, what string) {
	var pending *jobs.ErrMattePending
	switch {
	case r.Context().Err() != nil, errors.Is(err, context.Canceled):
		return // client went away; nobody is listening
	case errors.As(err, &pending):
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusAccepted, s.mattePendingBody(pending))
	case errors.Is(err, jobs.ErrMatteUnavailable):
		writeError(w, http.StatusServiceUnavailable, matteUnavailableMessage(err))
	case errors.Is(err, store.ErrNotFound):
		writeError(w, missingStatus, errText(err))
	case errors.Is(err, jobs.ErrInvalidRecipe):
		writeError(w, http.StatusBadRequest, errText(err))
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, what+" timed out")
	default:
		writeError(w, http.StatusInternalServerError, errText(err))
	}
}

// writePreview sends a rendered preview. It is private to the requester
// and may be cached for an hour: the SPA asks again for every op change
// anyway, and the memo behind the render is what makes repeats cheap.
func writePreview(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---- jobs ---------------------------------------------------------------------

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown() {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "server is shutting down; retry shortly")
		return
	}
	var rec recipe.Recipe
	if !decodeJSON(w, r, &rec, "recipe JSON") {
		return
	}
	if err := rec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	if err := validateTarget(rec.Output.Target); err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	// Phase 5b: a matte op needs the sidecar. With features.matte false —
	// no EZLG_MATTE_URL, or the sidecar stopped answering — the recipe is
	// refused up front, before any source is looked at, naming the compose
	// profile to start (the SPA greys the AI mode on the same flag; this
	// guards scripted clients and a sidecar that went away mid-session).
	if hasOp(rec.Ops, recipe.OpMatte) && !s.matteEnabled() {
		writeError(w, http.StatusBadRequest, s.matteOffMessage())
		return
	}
	// Every source — the main one and the overlay assets after it — must be
	// an uploaded, probed blob before the render is queued.
	if !s.checkSources(w, rec.Sources, http.StatusBadRequest) {
		return
	}
	job, err := s.jm.Submit(rec)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrMatteUnavailable):
			// Submit resolves the matte identity from the sidecar's persisted
			// facts; none yet (or the model refused) is the same 400 as above.
			writeError(w, http.StatusBadRequest, matteUnavailableMessage(err))
		case errors.Is(err, jobs.ErrInvalidRecipe):
			writeError(w, http.StatusBadRequest, errText(err))
		default:
			writeError(w, http.StatusInternalServerError, errText(err))
		}
		return
	}
	s.watchJob(job)
	writeJSON(w, http.StatusAccepted, job)
}

// watchJob ties an accepted job to the server lifetime: a tracked goroutine
// follows the job's events and, if Shutdown begins first, cancels the job
// and keeps waiting until the manager closes the subscription — which it
// does only after the pipeline goroutine has returned, i.e. after its
// ffmpeg/gifsicle were killed and its scratch dir removed. Shutdown's
// bg.Wait therefore covers every render this server accepted.
func (s *Server) watchJob(job jobs.Job) {
	if job.IsFinished() {
		return // served from cache: nothing runs
	}
	ch, unsubscribe, ok := s.jm.Subscribe(job.ID)
	if !ok {
		return
	}
	tracked := s.spawn(func() {
		defer unsubscribe()
		closing := s.ctx.Done()
		for {
			select {
			case _, open := <-ch:
				if !open {
					return
				}
			case <-closing:
				closing = nil
				s.jm.Cancel(job.ID)
			}
		}
	})
	if !tracked {
		// Shutdown began between the 503 check and Submit: nothing will
		// wait for this job, so stop it before it starts any process.
		unsubscribe()
		s.jm.Cancel(job.ID)
	}
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jm.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.jm.Get(id); !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	s.jm.Cancel(id) // false when already finished: still idempotent 204
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.jm.Get(id); !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	rc := http.NewResponseController(w)
	ch, cancel, ok := s.jm.Subscribe(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		log.Printf("server: sse: response writer does not support flushing: %v", err)
		return
	}

	ticker := time.NewTicker(ssePingInterval)
	defer ticker.Stop()
	ctx := r.Context()
	// On Shutdown the job is being cancelled; keep forwarding for a short
	// grace so the client gets the terminal "cancelled" event, then end the
	// stream so it cannot hold the HTTP drain up.
	closing := s.ctx.Done()
	var grace <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-closing:
			closing = nil
			grace = time.After(sseShutdownGrace)
		case <-grace:
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				log.Printf("server: sse: encode event: %v", err)
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
				return
			}
			_ = rc.Flush()
			if ev.Type == jobs.EventDone || ev.Type == jobs.EventError {
				return
			}
		}
	}
}

// ---- results ------------------------------------------------------------------

func (s *Server) handleGetResult(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !recipe.IsHash(hash) {
		writeError(w, http.StatusNotFound, "not a recipe hash")
		return
	}
	res, err := s.jm.LoadResult(hash)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no result for this recipe")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// validResultName accepts plain file names only: no separators, no dot
// segments, printable ASCII subset.
func validResultName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleOutFile(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	name := r.PathValue("name")
	if !recipe.IsHash(hash) || !validResultName(name) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	dir := s.st.ResultDir(hash)
	if !s.st.HasResult(hash) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	full := filepath.Join(dir, name)
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	if ct := resultContentType(name); ct != "" {
		h.Set("Content-Type", ct)
	}
	if r.URL.Query().Get("dl") == "1" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": s.downloadName(hash, name)}))
	}
	http.ServeFile(w, r, full)
}

// resultContentTypes pins the Content-Type of every kind of result file
// rather than leaving it to the host's mime tables (Windows calls a zip
// "application/x-zip-compressed", a slim container image has no
// /etc/mime.types and Go's built-in table knows neither .zip nor .apng).
var resultContentTypes = map[string]string{
	".gif":  "image/gif",
	".webp": "image/webp",
	".png":  "image/png",
	".apng": "image/apng",
	".avif": "image/avif",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".zip":  "application/zip",
	".json": "application/json; charset=utf-8",
}

// resultContentType returns the Content-Type for a result file name, or ""
// when the extension is not one the pipeline writes (http.ServeFile then
// decides).
func resultContentType(name string) string {
	return resultContentTypes[strings.ToLower(path.Ext(name))]
}

// downloadName derives a friendly attachment name from the recipe's main
// source. Only the primary output is named after the source alone
// ("myclip.gif"); every other file keeps its own stem behind the source so
// sibling downloads stay distinct: "myclip-f00012.png" for an extracted
// frame, "myclip-alt1.gif" for a fit-search alternative, "myclip-frames.zip"
// for the frame archive and "myclip-delays.json" / "myclip-report.json" for
// sidecars the manifest does not list. Falls back to the stored file name
// when the source is unknown.
func (s *Server) downloadName(hash, name string) string {
	res, err := s.jm.LoadResult(hash)
	if err != nil || len(res.Recipe.Sources) == 0 {
		return name
	}
	blob, err := s.st.GetBlob(res.Recipe.Sources[0])
	if err != nil {
		return name
	}
	base := strings.TrimSuffix(blob.Name, path.Ext(blob.Name))
	base = strings.TrimSpace(base)
	if base == "" {
		return name
	}
	for _, f := range res.Files {
		if f.Name == name {
			if f.Kind == "" || f.Kind == jobs.FileKindOutput {
				return base + path.Ext(name)
			}
			break
		}
	}
	return base + "-" + name
}
