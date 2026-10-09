package jobs

// Phase 5c (build brief 2026-10-09; docs/background-removal-proposal.md,
// docs/reviews/background-removal-stabilise-and-guided-2026-10-09.md):
// on-demand AI mattes. This file holds what the phase added around the 5b
// pass in matte.go:
//
//   - the DEVICE PREFERENCE: the device matte passes run on when the sidecar
//     offers several ("cuda" / "cpu") — Options.MatteDevice at start, then
//     the persisted <data>/mattes/settings.json that SetMatteDevice (PUT
//     /api/matte/settings) writes; matteEffectiveDevice resolves it against
//     a ping (the preference when offered, else the sidecar's default). The
//     device is sent as device= on every sidecar request (wireDevice: never
//     to a pre-5c sidecar, which knows no such parameter) and picks the
//     per-device size / precision / msPerFrame of the identity (identityFor
//     in matte.go) — it is never itself part of a recipe or a memo key;
//   - the IDLE state (MattePendingIdle): a preview whose matte is not on
//     disk, with no pass in flight and no MatteEager mark, answers it at
//     once and starts nothing; the Compute matte button (an eager preview)
//     or a render starts the pass (matteMiss in matte.go);
//   - UnloadMatte: POST /v1/unload, everything (the SPA's leave-the-AI-mode
//     call), best effort;
//   - the DERIVED SEQUENCES under a clip memo (store.MatteDerivedDir): the
//     stabilised sequence "<clipdir>/stab-<mode>/" (enc.MatteStabiliseArgs,
//     deriveStabilised) and a tracker matte gated with its edge model
//     "<clipdir>/gated-<edge>-<edgeKey>-r3/" (enc.MatteGateArgs, deriveGated;
//     edgeKey = the edge memo's clip key, shortened — the tracker's key
//     leaves the edge out, so the gate's name carries the edge identity),
//     each derived once into "<name>.tmp" and renamed, under Protect,
//     deduped by path through a flight, bounded and logged;
//     fillMatteInputs points the plan at the derived dir
//     (resolvedMatte.SeqDir);
//   - the TRACKER PASS (runTrackPass) behind the guided model
//     (recipe.MatteModelSAM2Tiny, matte.KindTracker): the whole clip at the
//     tracking size (enc.TrackSize / MatteTrackSourceArgs) spooled to a
//     file under the pass's tmp dir (bounded by the frame cap and the body
//     cap), one Client.Track call with the prompts, N binary masks filed
//     as the clip memo (no frames store: the memo is keyed by the
//     canonical prompts, the tracker weights and the tracking size);
//   - MattePromptMask: the live overlay of the guided mode — one output
//     frame of the matte plan at the tracking size (enc.MatteTrackFrameArgs)
//     through Client.TrackFrame, memoised on scratch under
//     store.MattePromptsMemoDir.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// MattePendingIdle is the ErrMattePending.State of a preview (still /
// proxy) whose matte is not on disk, with no pass in flight and no eager
// mark on the ctx (MatteEager): nothing was started — the SPA shows "AI
// matte not computed — Compute" and the Compute matte button (an eager
// preview) or Render starts the pass. It replaces the 5b "deferred" state
// and its 90-s eager bound: a preview never starts a pass on its own.
const MattePendingIdle = "idle"

// MatteModelDeviceStatus is one offered model's live state on one device
// (MatteModelStatus.Devices[device]): what GET /api/matte publishes per
// device so the SPA's "Run on" select can show readiness and the estimate
// of the device it names. Size / Precision are the graph the sidecar runs
// for that device (isnet: fp16 1024 on cuda, fp32 512 on cpu; lite: fp32
// 1024 on both; the tracker: bf16 on cuda, fp32 on cpu at 1024) — the
// identity that enters the clip key.
type MatteModelDeviceStatus struct {
	State      string  `json:"state"`                // ready / loading / downloading / missing / unavailable
	Reason     string  `json:"reason,omitempty"`     // why missing / unavailable on this device
	Precision  string  `json:"precision,omitempty"`  // fp16 / fp32 / bf16
	Percent    int     `json:"percent,omitempty"`    // download / load progress for the transient states
	Size       int     `json:"size,omitempty"`       // the default input square on this device
	MsPerFrame float64 `json:"msPerFrame,omitempty"` // measured ms per frame at Size (0 = unknown)
	Resident   bool    `json:"resident"`             // a session is loaded on this device right now ("ready" alone means downloaded and self-tested: the first pass adds the model load, 1–20 s)
}

// Phase 5c tunables.
const (
	// matteSettingsName is the persisted device preference, under
	// <data>/mattes next to the facts file (the sweeper leaves regular files
	// there alone).
	matteSettingsName = "settings.json"
	// matteGateRadius is the band of the gate that bounds a per-frame matte
	// with the tracker's mask (enc.MatteGateArgs): alpha 255 inside
	// erode(mask, r), the per-frame matte inside the band, 0 outside
	// dilate(mask, r) — in TRACKING pixels (the sequence is at the tracking
	// size, long side <= 1024). 3 matched BiRefNet-lite's edges on the corpus
	// while removing the UI and flicker (the 2026-10-09 experiment).
	matteGateRadius = 3
	// matteDeriveBaseTimeout and matteDerivePerFrame bound one derive
	// (stabilise / gate: a cheap ffmpeg pass over gray PNGs, ~0.1 s per 45 ×
	// 720² frames): base + frames × per frame.
	matteDeriveBaseTimeout = 60 * time.Second
	matteDerivePerFrame    = 50 * time.Millisecond
	// matteTrackMaxBodyBytes is the sidecar's body cap of one POST
	// /v1/track (1 GiB: 607 frames at 1024×576), enforced app-side up-front
	// from the plan's frame count and at run time from the spooled bytes,
	// so the request is never sent to be refused with a 413.
	matteTrackMaxBodyBytes = int64(1) << 30
	// matteTrackEstimateFactor is the margin on a tracker pass's estimate:
	// the sidecar's msPerFrame comes from a small synthetic self-test and
	// under-estimates 720²+ clips by ~20 % (frame prep scales with the
	// source), plus the transfer of the whole clip in one request.
	matteTrackEstimateFactor = 1.3
	// matteTrackSpoolName is the raw rgb24 clip a tracker pass spools under
	// its tmp dir before the one POST (removed before the rename).
	matteTrackSpoolName = "frames.rgb"
	// promptMaskKeyVersion salts the prompt-mask memo key.
	promptMaskKeyVersion = "1"
)

// ---- the device preference ----------------------------------------------------

// matteSettings is the persisted device preference (<data>/mattes/
// settings.json): Device "" = the sidecar's default.
type matteSettings struct {
	Device string `json:"device"`
}

// matteSettingsPath is <Root>/mattes/settings.json.
func matteSettingsPath(st *store.Store) string {
	return filepath.Join(filepath.Dir(st.MatteDir("settings")), matteSettingsName)
}

// loadMatteSettings reads the persisted preference into the state
// (initMatte). No file means no preference was ever set: Options.MatteDevice
// applies. An unreadable file is logged and ignored the same way.
func (m *Manager) loadMatteSettings() {
	data, err := os.ReadFile(matteSettingsPath(m.st))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("jobs: matte: settings file %s unreadable (%v); EZLG_MATTE_DEVICE applies", matteSettingsPath(m.st), err)
		}
		return
	}
	var s matteSettings
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("jobs: matte: settings file %s malformed (%v); EZLG_MATTE_DEVICE applies", matteSettingsPath(m.st), err)
		return
	}
	m.mt.pref, m.mt.prefSet = s.Device, true
}

// prefLocked is the device preference in force: the persisted setting once
// one was written (its "" is a reset that overrides the option), else
// Options.MatteDevice. mt.mu must be held.
func (s *matteState) prefLocked(opts Options) string {
	if s.prefSet {
		return s.pref
	}
	return opts.MatteDevice
}

// matteDevicePref returns the device preference in force ("" = the
// sidecar's default).
func (m *Manager) matteDevicePref() string {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	return m.mt.prefLocked(m.opts)
}

// matteEffectiveDevice resolves the preference against a ping (the live
// answer or the persisted facts): the preferred device when the sidecar
// offers it, else the sidecar's default (Ping.EffectiveDevice: a
// preference the sidecar no longer offers — a cuda install moved to the cpu
// image — falls back rather than refusing every pass). The result is what
// MatteStatus.Device reports and what a pass runs on and keys its identity
// (size / precision per device) with.
func (m *Manager) matteEffectiveDevice(p *matte.Ping) string {
	return effectiveDevice(p, m.matteDevicePref())
}

// effectiveDevice is matteEffectiveDevice for an explicit preference.
func effectiveDevice(p *matte.Ping, pref string) string {
	if p == nil {
		return pref
	}
	if pref != "" && p.Offers(pref) {
		return pref
	}
	return p.EffectiveDevice("")
}

// wireDevice is the device= value a request to the sidecar carries for the
// effective device: the device itself for a sidecar that lists its devices
// (Phase 5c), "" (no parameter) for one that predates the list — it offers
// exactly its one device and would not know the parameter.
func wireDevice(p *matte.Ping, device string) string {
	if p == nil || len(p.Devices) == 0 {
		return ""
	}
	return device
}

// offeredDevices lists the devices a ping offers for the UI and error
// texts: the Phase 5c list, else the one device of a pre-5c sidecar when it
// is usable.
func offeredDevices(p *matte.Ping) []string {
	if p == nil {
		return nil
	}
	if len(p.Devices) > 0 {
		return slices.Clone(p.Devices)
	}
	if p.Device == matte.DeviceCUDA || p.Device == matte.DeviceCPU {
		return []string{p.Device}
	}
	return nil
}

// matteSnapshot returns the ping the preference is validated against: the
// last live answer when the sidecar has answered, else the persisted facts;
// nil when neither exists.
func (m *Manager) matteSnapshot() *matte.Ping {
	m.mt.mu.Lock()
	live, answered := m.mt.live, m.mt.answered
	m.mt.mu.Unlock()
	if live != nil && answered {
		return live
	}
	if f, err := m.loadMatteFacts(); err == nil {
		return &f.Ping
	}
	return live
}

// SetMatteDevice sets the server-side device preference for matte passes
// ("cuda" / "cpu"; "" resets to the sidecar's default) — PUT
// /api/matte/settings. A named device is validated against the devices
// the sidecar offers (its last answer, else the persisted facts; an
// unoffered one is an ErrInvalidRecipe the server maps to 400, no answer
// at all an ErrMatteUnavailable); "" always succeeds. The preference is
// persisted under <data>/mattes/settings.json and wins over
// Options.MatteDevice from then on; the device never enters a recipe or a
// memo key. MatteStatus().Device reports the effective device.
func (m *Manager) SetMatteDevice(ctx context.Context, dev string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dev != "" {
		p := m.matteSnapshot()
		if p == nil {
			if m.mt.client == nil {
				return fmt.Errorf("%w: AI mattes are off on this server (EZLG_MATTE_URL is empty), so no device can be chosen", ErrMatteUnavailable)
			}
			return fmt.Errorf("%w: %s", ErrMatteUnavailable, m.noFactsReason())
		}
		if !p.Offers(dev) {
			offered := offeredDevices(p)
			if len(offered) == 0 {
				return fmt.Errorf("%w: the matte service does not offer device %q (it offers no usable device right now: %s)", ErrInvalidRecipe, dev, firstNonEmpty(p.Reason, "its device is unavailable"))
			}
			return fmt.Errorf("%w: the matte service does not offer device %q (offered: %s)", ErrInvalidRecipe, dev, strings.Join(offered, ", "))
		}
	}
	data, err := json.MarshalIndent(matteSettings{Device: dev}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(matteSettingsPath(m.st), data); err != nil {
		return fmt.Errorf("jobs: matte: persisting the device preference: %w", err)
	}
	m.mt.mu.Lock()
	prev := m.mt.prefLocked(m.opts)
	m.mt.pref, m.mt.prefSet = dev, true
	m.mt.mu.Unlock()
	if prev != dev {
		log.Printf("jobs: matte: device preference %q → %q (%s)", prev, dev, matteDeviceLabel(dev))
	}
	return nil
}

// writeFileAtomic writes data through a dot-named temp file beside path and
// a rename, creating the directory.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+store.RandomID(4))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// UnloadMatte asks the sidecar to release every resident model session
// now (POST /v1/unload, all models and devices) — POST /api/matte/unload,
// which the SPA calls when the Background card leaves the AI mode or is
// disabled, so nothing stays loaded between uses. Best-effort by contract:
// without a sidecar it is a no-op, and a failure is returned for the
// server to log (it answers 204 regardless — the sidecar's idle TTL
// releases the sessions anyway). While a pass is in flight (a render's,
// or an eager preview's — the card is often switched off right after
// pressing Render) nothing is sent: the pass would only reload the model
// it is using (a 503 "model loading" mid-pass, 2–20 s for General) and the
// TTL releases everything once it is done; the skip is logged.
func (m *Manager) UnloadMatte(ctx context.Context) error {
	if m.mt.client == nil {
		return nil
	}
	if n := m.mattePassesInFlight(); n > 0 {
		log.Printf("jobs: matte: unload skipped: %d matte pass(es) in flight (the sidecar's idle TTL releases the sessions later)", n)
		return nil
	}
	uctx, cancel := context.WithTimeout(ctx, matteProbeTimeout)
	defer cancel()
	if err := m.mt.client.Unload(uctx, "", ""); err != nil {
		return fmt.Errorf("jobs: matte: unload: %w", err)
	}
	log.Printf("jobs: matte: released the matte service's model sessions")
	return nil
}

// mattePassesInFlight counts the matte passes running right now (the
// progress entries: one per clip key from the start of runMattePass /
// runTrackPass to its end).
func (m *Manager) mattePassesInFlight() int {
	m.mt.mu.Lock()
	defer m.mt.mu.Unlock()
	return len(m.mt.progress)
}

// ---- derived sequences --------------------------------------------------------

// derivedComplete reports whether dir holds a derived sequence of frames
// files (its first and last PNG exist — a derive renames a complete dir
// into place, so either is as good as the count).
func derivedComplete(dir string, frames int) bool {
	if frames < 1 {
		return false
	}
	for _, n := range []int{1, frames} {
		if _, err := os.Stat(filepath.Join(dir, matte.FrameFile(n))); err != nil {
			return false
		}
	}
	return true
}

// matteDeriveTimeout bounds one derive of frames frames.
func matteDeriveTimeout(frames int) time.Duration {
	return matteDeriveBaseTimeout + time.Duration(frames)*matteDerivePerFrame
}

// deriveSequence derives <parent>/<name>/%06d.png with the argv builder
// (given the tmp dir to write into) unless it exists already: one ffmpeg
// run into <parent>/<name>.tmp (store.MatteDerivedTmpDir; a leftover of an
// interrupted derive is removed first), the file count checked against
// frames, then the rename into place. The run is deduped by the derived
// dir's path (mt.derives: concurrent previews of one clip derive once),
// held under Protect of parent and of every hold dir (the clip memo and,
// for a gate, the edge memo — a Protect of a derived dir keeps its clip
// dir too) from BEFORE the first existence check — a memo the sweeper
// removed between the caller's hit and here is reported as gone (a plain
// "retry" error, never an opaque ffmpeg failure) — bounded by
// matteDeriveTimeout and logged. The caller's ctx ending ends the derive
// (a plain error otherwise, never a context error).
func (m *Manager) deriveSequence(ctx context.Context, parent, name string, frames int, hold []string, argv func(tmp string) []string) (string, error) {
	dir := store.MatteDerivedDir(parent, name)
	for _, h := range append([]string{parent}, hold...) {
		release := m.st.Protect(h)
		defer release()
		if err := matteSequenceGone(h); err != nil {
			return "", err
		}
	}
	if derivedComplete(dir, frames) {
		return dir, nil
	}
	if m.tools.FFmpeg == "" {
		return "", errors.New("ffmpeg is not available on this server")
	}
	if frames < 1 {
		return "", fmt.Errorf("AI matte: cannot derive %s of a %d-frame memo", name, frames)
	}
	return m.mt.derives.do(ctx, dir, func(ctx context.Context) (string, error) {
		if derivedComplete(dir, frames) {
			return dir, nil // a previous leader finished while we waited
		}
		for _, h := range append([]string{parent}, hold...) {
			release := m.st.Protect(h) // the leader's own hold: it may outlive the caller that started it
			defer release()
		}
		tmp := store.MatteDerivedTmpDir(parent, name)
		_ = os.RemoveAll(tmp)
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			return "", fmt.Errorf("AI matte: deriving %s: %w", name, err)
		}
		ok := false
		defer func() {
			if !ok {
				_ = os.RemoveAll(tmp)
			}
		}()
		args := argv(tmp)
		if args == nil {
			return "", fmt.Errorf("AI matte: deriving %s: no argv for the derive", name)
		}
		started := time.Now()
		dctx, cancel := context.WithTimeout(ctx, matteDeriveTimeout(frames))
		err := ffrun.RunFFmpeg(dctx, m.tools.FFmpeg, args, nil)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("AI matte: deriving %s of %d frames did not finish within %s", name, frames, matteDeriveTimeout(frames))
			}
			return "", fmt.Errorf("AI matte: deriving %s: %w", name, err)
		}
		if n := countSequence(tmp); n != frames {
			return "", fmt.Errorf("AI matte: deriving %s yielded %d frames, the memo has %d — report this with the source", name, n, frames)
		}
		_ = os.RemoveAll(dir)
		if err := os.Rename(tmp, dir); err != nil {
			return "", fmt.Errorf("AI matte: deriving %s: %w", name, err)
		}
		ok = true
		log.Printf("jobs: matte: %s: derived %s (%d frames) in %s", short(filepath.Base(parent)), name, frames, time.Since(started).Round(time.Millisecond))
		return dir, nil
	})
}

// matteSequenceGone reports a memo or derived dir the sweeper removed
// meanwhile (its first frame file is missing) as the same "retry" error
// protectMattes gives; nil while it is there. Checked under Protect.
func matteSequenceGone(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, matte.FrameFile(1))); err != nil {
		return fmt.Errorf("the AI matte memo %s was removed from disk meanwhile — retry", short(filepath.Base(dir)))
	}
	return nil
}

// countSequence counts the %06d.png files directly in dir.
func countSequence(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && len(name) == 10 && strings.HasSuffix(name, ".png") {
			if _, err := strconv.Atoi(name[:6]); err == nil {
				n++
			}
		}
	}
	return n
}

// deriveStabilised derives the stabilised sequence of the sequence in
// parent (a clip memo, or a gated derived dir under one): "<parent>/
// stab-<mode>/" through enc.MatteStabiliseArgs (mode light / strong), N
// frames in, N out. hold names further dirs to protect meanwhile.
func (m *Manager) deriveStabilised(ctx context.Context, parent, mode string, frames int, hold ...string) (string, error) {
	if mode != recipe.MatteStabiliseLight && mode != recipe.MatteStabiliseStrong {
		return "", fmt.Errorf("%w: stabilise must be %q or %q (got %q)", ErrInvalidRecipe, recipe.MatteStabiliseLight, recipe.MatteStabiliseStrong, mode)
	}
	return m.deriveSequence(ctx, parent, store.MatteStabName(mode), frames, hold, func(tmp string) []string {
		return enc.MatteStabiliseArgs(parent, tmp, frames, mode)
	})
}

// deriveGated derives the gated sequence of a tracker matte: "<trackDir>/
// gated-<edge>-<edgeKey>-r3/" from the tracker's binary masks (trackDir,
// at the tracking size) and the edge model's memo (edgeDir, at its model
// square, scaled inside the chain) through enc.MatteGateArgs; the result
// is at the tracking size. edge is the RESOLVED edge model id and edgeKey
// the edge memo's clip key: both name the dir (store.MatteGatedName), so
// an edge memo re-keyed by a sidecar upgrade or the device preference
// derives a fresh gate instead of serving the stale one.
func (m *Manager) deriveGated(ctx context.Context, trackDir, edgeDir, edge, edgeKey string, frames int) (string, error) {
	return m.deriveSequence(ctx, trackDir, store.MatteGatedName(edge, edgeKey, matteGateRadius), frames, []string{edgeDir}, func(tmp string) []string {
		return enc.MatteGateArgs(trackDir, edgeDir, tmp, frames, matteGateRadius)
	})
}

// ---- the tracker pass ------------------------------------------------------------

// trackPromptsOf converts a matte op's prompts (recipe.MattePrompt, the
// OUTPUT frame index with coordinates in 0..1 of the source frame) into the
// client's TrackPrompts — the same shapes, one object. A MaskFrom prompt
// (Phase 5d) becomes a Mask prompt with an EMPTY digest: the caller fills
// it from the mask bytes it sends (withMaskDigest) before keying and before
// Track / TrackFrame.
func trackPromptsOf(ps []recipe.MattePrompt) matte.TrackPrompts {
	tp := matte.TrackPrompts{Obj: 1}
	for _, p := range ps {
		tp.Prompts = append(tp.Prompts, matte.FramePrompt{Frame: p.Frame, Points: p.Points, Box: p.Box, Mask: p.MaskFrom != ""})
	}
	return tp
}

// checkPromptFrames refuses a prompt on a frame the clip does not have
// (frames is the clip's frame count on the plan's grid; 0 = unknown, not
// checked): the graph validates the prompts' shape, the frame bound is
// only known here.
func checkPromptFrames(tp matte.TrackPrompts, frames int) error {
	if frames <= 0 {
		return nil
	}
	for _, p := range tp.Prompts {
		if p.Frame >= frames {
			return fmt.Errorf("%w: a prompt is on frame %d but the clip has %d frames (0..%d) at this trim and rate — prompt a frame inside it", ErrInvalidRecipe, p.Frame, frames, frames-1)
		}
	}
	return nil
}

// clampPromptFrames moves a prompt on frame n (the clip decoded to n frames)
// onto frame n−1 and reports how many it moved: Plan.Frames overshoots the
// decoded count by one for VFR animations and container durations, the
// SPA's planFrames mirrors it, so the scrubber offers that last slot — and
// the forward still and the live prompt mask both show the HELD LAST FRAME
// there (enc clamps the slot, MatteTrackFrameArgs seeks the last real one).
// A prompt drawn on that picture is a prompt on frame n−1, never a mistake
// to refuse after the clip was spooled. Two prompts that land on one frame
// are merged the way the sidecar merges a frame's prompts (points
// appended; the frame's own box wins, else the moved one's — one box per
// frame; a mask prompt (Phase 5d) moves with its digest — the mask was
// taken from the held last frame too, maskPromptOf). Anything further past
// the clip stays for checkPromptFrames.
func clampPromptFrames(tp matte.TrackPrompts, n int) (matte.TrackPrompts, int) {
	if n < 1 {
		return tp, 0
	}
	moved := 0
	for _, p := range tp.Prompts {
		if p.Frame == n {
			moved++
		}
	}
	if moved == 0 {
		return tp, 0
	}
	out := matte.TrackPrompts{Obj: tp.Obj}
	last := -1 // the index in out of the frame n−1 entry the moved prompts merge into
	for _, p := range tp.Prompts {
		if p.Frame == n {
			continue
		}
		if p.Frame == n-1 && last < 0 {
			last = len(out.Prompts)
		}
		out.Prompts = append(out.Prompts, p)
	}
	for _, p := range tp.Prompts {
		if p.Frame != n {
			continue
		}
		if last < 0 {
			p.Frame = n - 1
			last = len(out.Prompts)
			out.Prompts = append(out.Prompts, p)
			continue
		}
		l := &out.Prompts[last]
		l.Points = append(slices.Clone(l.Points), p.Points...)
		if l.Box == nil {
			l.Box = p.Box
		}
		if p.Mask && !l.Mask {
			l.Mask, l.MaskDigest = true, p.MaskDigest
		}
	}
	return out, moved
}

// trackBodyRefusal is the up-front body cap of a tracker pass (the
// sidecar's 1 GiB per POST /v1/track): frames × w × h × 3 over it is an
// ErrInvalidRecipe naming the remedy. An unknown count is bounded at run
// time (trackSpool).
func trackBodyRefusal(frames, w, h int) error {
	if frames <= 0 {
		return nil
	}
	if body := matte.TrackBodyLength(frames, w, h); body > matteTrackMaxBodyBytes {
		return fmt.Errorf("%w: a guided matte of %d frames at %dx%d is %s of frames, over the matte service's %s per track — trim the clip or lower the fps", ErrInvalidRecipe, frames, w, h, humanBytes(body), humanBytes(matteTrackMaxBodyBytes))
	}
	return nil
}

// trackRefusals is a tracker pass's up-front refusals in one place, in the
// order they always ran: the body cap (trackBodyRefusal), the prompts'
// frame bound (checkPromptFrames) and the server's frame / estimate caps
// (matteRefusal with the tracker's estimate, matteTrackEstimateMS).
// matteMiss runs them on every miss; a mask-prompted request (Phase 5d)
// runs them BEFORE the edge model's pass the mask needs
// (resolveMaskPrompt), so a clip the track would refuse anyway never costs
// that pass first — box / point prompts refuse before any pass, and so
// must a mask prompt. frames is the plan's count (0 = unknown: nothing is
// refused here, the run-time caps bound it); id carries the tracking size,
// the device and the rate.
func (m *Manager) trackRefusals(frames int, id matteIdentity, prompts []recipe.MattePrompt) error {
	if err := trackBodyRefusal(frames, id.TrackW, id.TrackH); err != nil {
		return err
	}
	if err := checkPromptFrames(trackPromptsOf(prompts), frames); err != nil {
		return err
	}
	return m.matteRefusal(frames, matteTrackEstimateMS(frames, id.MsPerFrame), id.Device, id.Model)
}

// trackSpool is the batch writer's sink of a tracker pass: every complete
// frame is appended to the spool file (no copy in memory), counted against
// the body cap.
type trackSpool struct {
	f     *os.File
	bytes int64
}

func (s *trackSpool) frame(_ int, rgb []byte) error {
	if s.bytes+int64(len(rgb)) > matteTrackMaxBodyBytes {
		return fmt.Errorf("%w: a guided matte of more than %s of frames is over the matte service's cap per track — trim the clip or lower the fps", ErrInvalidRecipe, humanBytes(matteTrackMaxBodyBytes))
	}
	if _, err := s.f.Write(rgb); err != nil {
		return fmt.Errorf("AI matte: spooling the clip: %w", err)
	}
	s.bytes += int64(len(rgb))
	return nil
}

// runTrackPass produces a tracker (guided) matte's clip memo under the
// flight's ctx, the twin of runMattePass for a model of matte.KindTracker:
// waits for the tracker on the pass's device, streams the matte input plan
// at the tracking size (enc.MatteTrackSourceArgs through the batch writer,
// which cuts exact w*h*3-byte frames and bounds the count) into a spool
// file under the pass's tmp dir (bounded by the body cap), checks the
// prompts' frames against the count, POSTs the whole clip ONCE with the
// prompts (Client.Track; a 503 "model loading" / "out of memory" with a
// retry delay is retried like a batch, the spool re-read per attempt) and
// files the N masks under <tmp>/%06d.png, then the manifest (Prompts /
// TrackW / TrackH recorded) and the rename. No frames store: a tracker's
// masks depend on the whole clip and the prompts, never on one frame.
// Errors are plain (never context errors) unless ctx itself ended.
func (m *Manager) runTrackPass(ctx context.Context, key string, w matteWork) (man matte.Manifest, err error) {
	if m.tools.FFmpeg == "" {
		return man, errors.New("ffmpeg is not available on this server")
	}
	m.setMatteProgress(key, func(p *matteProgress) {
		*p = matteProgress{State: MattePendingRunning, Phase: MattePhaseTracking, Total: w.frames, Device: w.device, EstimateMS: w.estMS}
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
	if !ms.IsTracker() {
		return man, fmt.Errorf("%w: model %s is not a tracker (the guided model)", ErrInvalidRecipe, w.id.Model)
	}
	tw, th := w.id.TrackW, w.id.TrackH
	if tw < 1 || th < 1 {
		return man, errors.New("AI matte: no tracking size for the clip")
	}
	msPerFrame := d.MsPerFrameAt(0)
	m.setMatteProgress(key, func(p *matteProgress) {
		p.State = MattePendingRunning
		if p.EstimateMS == 0 {
			p.EstimateMS = matteTrackEstimateMS(w.frames, msPerFrame)
		}
	})
	// Phase 5d: the mask prompt's digest is the bytes' (the client refuses
	// any other), and matches the key resolveMatte made (same bytes).
	prompts := withMaskDigest(trackPromptsOf(w.prompts), maskDigestOf(w.mask))
	if err := prompts.Validate(); err != nil {
		return man, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	if _, has := prompts.MaskFrame(); has != (w.mask != nil) {
		return man, fmt.Errorf("AI matte: a mask prompt without its mask (or the reverse) reached the track — report this with the source")
	}

	argv := enc.MatteTrackSourceArgs(w.src.Path, w.plan, tw, th)
	if argv == nil {
		return man, errors.New("AI matte: the matte input plan is not usable")
	}
	argv = append(slices.Clone(ffmpegPrefix), argv...)
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
	spoolPath := filepath.Join(tmp, matteTrackSpoolName)
	sf, err := os.Create(spoolPath)
	if err != nil {
		return man, fmt.Errorf("AI matte: spooling the clip: %w", err)
	}
	spool := &trackSpool{f: sf}

	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	bw := newBatchWriter(tw*th*3, m.opts.MatteMaxFrames, spool.frame, pcancel)
	hard := matteHardTimeout(m.opts.MatteMaxSeconds, w.estMS)
	timer := time.AfterFunc(hard, func() {
		bw.fail(fmt.Errorf("AI matte failed: the pass did not finish within %s (its hard timeout) — trim the clip or raise EZLG_MATTE_MAX_SECONDS", hard))
	})
	runErr := ffrun.RunFrom(pctx, m.tools.FFmpeg, argv, func(stdout io.Reader) error {
		_, err := io.Copy(bw, stdout)
		return err
	})
	closeErr := sf.Close()
	if ferr := bw.failure(); ferr != nil {
		timer.Stop()
		return man, ferr
	}
	if runErr != nil {
		timer.Stop()
		if ctx.Err() != nil {
			return man, ctx.Err()
		}
		return man, fmt.Errorf("AI matte: decoding the clip failed: %w", runErr)
	}
	if err := bw.finish(); err != nil {
		timer.Stop()
		return man, err
	}
	if closeErr != nil {
		timer.Stop()
		return man, fmt.Errorf("AI matte: spooling the clip: %w", closeErr)
	}
	n := bw.frames
	if n == 0 {
		timer.Stop()
		return man, errors.New("AI matte: the clip decoded to no frames")
	}
	if clamped, moved := clampPromptFrames(prompts, n); moved > 0 {
		log.Printf("jobs: matte: %s: %d prompt(s) on frame %d moved to frame %d — the plan's %d frames overshoot the clip's %d (the held last frame)", short(key), moved, n, n-1, w.frames, n)
		prompts = clamped
	}
	if err := checkPromptFrames(prompts, n); err != nil {
		timer.Stop()
		return man, err
	}
	m.setMatteProgress(key, func(p *matteProgress) { p.Total = n })

	// The one POST. The hard timer keeps running over it (bw.fail cancels
	// pctx, which the request runs under).
	done := 0
	post := func() error {
		timeout := matteBatchTimeout(n, msPerFrame*matteTrackEstimateFactor, ping.Busy)
		deadline := time.Now().Add(matteLoadTimeout)
		for {
			body, err := os.Open(spoolPath)
			if err != nil {
				return fmt.Errorf("AI matte: spooling the clip: %w", err)
			}
			bctx, cancel := context.WithTimeout(pctx, timeout)
			i := 0
			err = m.mt.client.Track(bctx, w.id.Model, wireDevice(ping, w.device), tw, th, n, prompts, body, func(png []byte) error {
				if i >= n {
					return fmt.Errorf("%w: more records than frames", matte.ErrMissingTerminator)
				}
				i++
				if err := os.WriteFile(filepath.Join(tmp, matte.FrameFile(i)), png, 0o644); err != nil {
					return fmt.Errorf("AI matte: %w", err)
				}
				done = i
				m.setMatteProgress(key, func(p *matteProgress) {
					p.Done, p.Percent = i, min(100, i*100/n)
				})
				return nil
			}, w.mask)
			cancel()
			body.Close()
			if err == nil {
				return nil
			}
			if ferr := bw.failure(); ferr != nil {
				return ferr // the hard timeout
			}
			if pctx.Err() != nil {
				return pctx.Err()
			}
			var se *matte.StatusError
			switch {
			case errors.As(err, &se) && se.Status == 503 && se.RetryAfterMS > 0 && time.Now().Add(se.RetryAfter()).Before(deadline):
				m.setMatteProgress(key, func(pr *matteProgress) { pr.State = MattePendingLoading })
				select {
				case <-pctx.Done():
					return pctx.Err()
				case <-time.After(se.RetryAfter()):
				}
				m.setMatteProgress(key, func(pr *matteProgress) { pr.State = MattePendingRunning })
				continue
			case errors.As(err, &se) && se.Status == 400:
				return fmt.Errorf("%w: the matte service refused the prompts: %s", ErrInvalidRecipe, se.Message)
			case errors.As(err, &se):
				return fmt.Errorf("AI matte failed: the matte service answered HTTP %d: %s", se.Status, se.Message)
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("AI matte failed: the matte service did not answer the track of %d frames within %s", n, timeout)
			case errors.Is(err, matte.ErrTruncated), errors.Is(err, matte.ErrShortCount), errors.Is(err, matte.ErrMissingTerminator), errors.Is(err, matte.ErrRecordTooLarge):
				return fmt.Errorf("AI matte failed: %v", err)
			default:
				return fmt.Errorf("AI matte failed: sidecar unreachable at %s — is the matte profile up? (%v)", m.matteURL(), err)
			}
		}
	}
	err = post()
	timer.Stop()
	if err != nil {
		if ferr := bw.failure(); ferr != nil {
			return man, ferr
		}
		if ctx.Err() != nil {
			return man, ctx.Err()
		}
		return man, err
	}
	if done != n {
		return man, fmt.Errorf("AI matte: the matte service answered %d of %d frames", done, n)
	}
	_ = os.Remove(spoolPath)

	man = matte.Manifest{
		Key: key, Src: w.src.Hash, Model: w.id.Model,
		Weights: w.id.Weights, Proc: w.id.Proc, GraphDigest: ms.GraphDigest, Precision: w.id.Precision,
		Size: 0, FPS: w.fps, Frames: n, Device: w.device, MsPerFrame: msPerFrame,
		Prompts: w.id.Prompts, TrackW: tw, TrackH: th,
	}
	if err := matte.WriteManifest(filepath.Join(tmp, matte.ManifestName), &man); err != nil {
		return man, fmt.Errorf("AI matte: %w", err)
	}
	if got, hit := m.matteMemoHit(key, w.fps); hit {
		return *got, nil // another instance of the app filed the same clip meanwhile
	}
	_ = os.RemoveAll(w.dir)
	if err := os.Rename(tmp, w.dir); err != nil {
		return man, fmt.Errorf("AI matte: %w", err)
	}
	ok = true
	maskNote := ""
	if f, has := prompts.MaskFrame(); has {
		maskNote = fmt.Sprintf(", a mask prompt on frame %d (%s)", f, maskDigestOf(w.mask))
	}
	log.Printf("jobs: matte: %s: tracked %d frames at %dx%d in %s on %s (%s %s, %d prompted frames%s)",
		short(key), n, tw, th, time.Since(started).Round(time.Millisecond), w.device, w.id.Model, w.id.Precision, len(prompts.Prompts), maskNote)
	return man, nil
}

// matteTrackEstimateMS is the tracker pass's estimate: the segmenter's
// arithmetic with matteTrackEstimateFactor on top.
func matteTrackEstimateMS(frames int, msPerFrame float64) int64 {
	if frames <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(matteEstimateMS(frames, msPerFrame)) * matteTrackEstimateFactor))
}

// ---- the live prompt mask -------------------------------------------------------

// promptMaskKey names a prompt mask on scratch: the tracker's clip key made
// WITHOUT the prompts (the clip, the plan rate, the tracker identity and
// the tracking size), the output frame and the canonical prompts of that
// frame.
func promptMaskKey(clipKey string, frame int, prompts matte.TrackPrompts) string {
	sum := sha256.Sum256([]byte("matte-prompt|" + promptMaskKeyVersion + "\n" + clipKey + "\nframe=" + strconv.Itoa(frame) + "|" + prompts.Canonical()))
	return hex.EncodeToString(sum[:])
}

// MattePromptMask renders output frame `frame` of the matte plan of (srcs,
// ops, out) at the tracking size (enc.TrackSize) and asks the sidecar's
// tracker (the guided model, recipe.MatteModelSAM2Tiny — any model the
// facts list as a tracker) for that frame's mask under prompts (the prompts
// of that frame only, validated by the caller) — the live overlay behind
// POST /api/matte/prompt; the answer is one 8-bit gray PNG at the tracking
// size. srcs are the blob hashes of recipe.Sources (srcs[0] the main
// source), frame the OUTPUT frame index on the plan's grid. Memoised per
// (clip key without prompts, frame, prompts) on scratch
// (store.MattePromptsMemoDir, bounded like the still memo). A loading or
// downloading tracker is reported as *ErrMattePending (the server's 202)
// after /v1/warm; a recipe without a tracker matte op, a frame past the
// clip or prompts the sidecar refuses are ErrInvalidRecipe; no sidecar is
// ErrMatteUnavailable; an unknown source store.ErrNotFound.
//
// Phase 5d — a MASK prompt: a prompt of the set with FramePrompt.Mask (the
// SPA's {frame, maskFrom: "edge"}; it must be on frame) means "this
// frame's mask is the edge model's matte of it" — the op's Edge ("" = the
// device's default per-frame model; "none" is ErrInvalidRecipe). When that
// model's memo for the clip is on disk, frame's matte is scaled to the
// tracking size (maskPromptOf, derived once under the edge memo), its
// digest fills the prompt's MaskDigest (so the memo key names it) and the
// PNG rides as the request's mask record; when it is NOT computed yet the
// answer is *ErrMattePending State idle with a Reason the SPA shows
// ("compute the General matte first …") — the overlay never starts a pass.
// Any client-sent MaskDigest is ignored (the bytes decide).
func (m *Manager) MattePromptMask(ctx context.Context, srcs []string, ops []recipe.Op, out recipe.Output, frame int, prompts matte.TrackPrompts) ([]byte, error) {
	if len(srcs) == 0 {
		return nil, fmt.Errorf("%w: no source", ErrInvalidRecipe)
	}
	if frame < 0 {
		return nil, fmt.Errorf("%w: frame must be >= 0 (got %d)", ErrInvalidRecipe, frame)
	}
	if err := prompts.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	reqs, err := matteRequests(ops)
	if err != nil {
		return nil, err
	}
	facts, err := m.loadMatteFacts()
	if err != nil {
		return nil, err
	}
	var req *matteRequest
	for i := range reqs {
		if ms, ok := facts.Model(reqs[i].Model); reqs[i].Model == recipe.MatteModelSAM2Tiny || (ok && ms.IsTracker()) {
			req = &reqs[i]
			break
		}
	}
	if req == nil {
		return nil, fmt.Errorf("%w: the recipe has no guided (tracker) matte op — the live mask needs model %q", ErrInvalidRecipe, recipe.MatteModelSAM2Tiny)
	}
	src, err := m.st.GetBlob(srcs[0])
	if err != nil {
		return nil, err
	}
	if src.Info == nil {
		return nil, fmt.Errorf("%w: the main source has no probe info", ErrInvalidRecipe)
	}
	device := m.matteEffectiveDevice(&facts.Ping)
	id, err := identityFor(facts, matteRequest{Model: req.Model}, device)
	if err != nil {
		return nil, err
	}
	if !id.tracker() {
		return nil, fmt.Errorf("%w: model %s is not a tracker (the guided model)", ErrInvalidRecipe, req.Model)
	}
	plan, err := graph.CompileMatteInput([]recipe.ProbeInfo{*src.Info}, ops, out)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	if plan.Frames > 0 && frame >= plan.Frames {
		return nil, fmt.Errorf("%w: frame %d is past the clip's %d frames (0..%d) at this trim and rate", ErrInvalidRecipe, frame, plan.Frames, plan.Frames-1)
	}
	id.TrackW, id.TrackH = enc.TrackSize(plan.Width, plan.Height)
	if id.TrackW < 1 || id.TrackH < 1 {
		return nil, fmt.Errorf("%w: the source has no frame size", ErrInvalidRecipe)
	}
	fps := graph.FPSText(plan.FPS)
	var mask []byte
	if mf, has := prompts.MaskFrame(); has {
		if mf != frame {
			return nil, fmt.Errorf("%w: the mask prompt is on frame %d but the mask asked for is frame %d's — send that frame's prompts", ErrInvalidRecipe, mf, frame)
		}
		edge, err := m.promptMaskEdge(src, ops, fps, req.Edge, facts, device)
		if err != nil {
			return nil, err
		}
		png, digest, err := m.maskPromptOf(ctx, edge, frame, id.TrackW, id.TrackH)
		if err != nil {
			return nil, err
		}
		mask, prompts = png, withMaskDigest(prompts, digest)
	}
	key := promptMaskKey(matteClipKey(src, ops, fps, id), frame, prompts)
	path := filepath.Join(m.st.MattePromptsMemoDir(), key+".png")
	if data := readMemo(path); data != nil {
		return data, nil
	}
	if m.mt.client == nil {
		return nil, fmt.Errorf("%w: AI mattes are off on this server (EZLG_MATTE_URL is empty)", ErrMatteUnavailable)
	}
	if m.tools.FFmpeg == "" {
		return nil, errors.New("ffmpeg is not available on this server")
	}
	ping, ms, d, pending, err := m.matteReadiness(ctx, id.Model, device)
	if err != nil {
		return nil, err
	}
	if pending != "" {
		wctx, cancel := context.WithTimeout(ctx, matteProbeTimeout)
		if err := m.mt.client.Warm(wctx, id.Model, wireDevice(ping, device)); err != nil {
			log.Printf("jobs: matte: warm %s: %v", id.Model, err)
		}
		cancel()
		return nil, &ErrMattePending{State: pending, Percent: d.Percent, Device: device}
	}
	if ms.Weights != id.Weights || ping.ProcessingVersion != id.Proc {
		// The sidecar was upgraded since the facts were written (the ping
		// just persisted the new ones): the overlay is keyed by them.
		return nil, fmt.Errorf("%w: the matte service changed its weights; retry", ErrMatteUnavailable)
	}

	argv := enc.MatteTrackFrameArgs(src.Path, plan, id.TrackW, id.TrackH, frame)
	if argv == nil {
		return nil, fmt.Errorf("%w: the matte input plan is not usable for a prompt still", ErrInvalidRecipe)
	}
	release, err := m.acquirePreview(ctx)
	if err != nil {
		return nil, err
	}
	rgb, err := ffrun.RunOutput(ctx, m.tools.FFmpeg, append(slices.Clone(ffmpegPrefix), argv...))
	release()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("prompt still: %w", err)
	}
	if want := matte.TrackBodyLength(1, id.TrackW, id.TrackH); int64(len(rgb)) != want {
		return nil, fmt.Errorf("prompt still: ffmpeg wrote %d bytes, want %d for one %dx%d rgb24 frame", len(rgb), want, id.TrackW, id.TrackH)
	}
	tctx, cancel := context.WithTimeout(ctx, matteBatchTimeout(1, d.MsPerFrameAt(0)*matteTrackEstimateFactor, ping.Busy))
	png, err := m.mt.client.TrackFrame(tctx, id.Model, wireDevice(ping, device), id.TrackW, id.TrackH, prompts, rgb, mask)
	cancel()
	if err != nil {
		var se *matte.StatusError
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.As(err, &se) && se.Status == 503 && se.RetryAfterMS > 0:
			return nil, &ErrMattePending{State: MattePendingLoading, Device: device}
		case errors.As(err, &se) && se.Status == 400:
			return nil, fmt.Errorf("%w: the matte service refused the prompts: %s", ErrInvalidRecipe, se.Message)
		case errors.As(err, &se):
			return nil, fmt.Errorf("prompt mask failed: the matte service answered HTTP %d: %s", se.Status, se.Message)
		case errors.Is(err, context.DeadlineExceeded):
			return nil, errors.New("prompt mask failed: the matte service did not answer in time")
		case errors.Is(err, matte.ErrTruncated), errors.Is(err, matte.ErrRecordTooLarge):
			return nil, fmt.Errorf("prompt mask failed: %v", err)
		}
		return nil, fmt.Errorf("%w: sidecar unreachable at %s — is the matte profile up? (%v)", ErrMatteUnavailable, m.matteURL(), err)
	}
	m.memoWrite(path, png, store.MattePromptsMaxEntries, store.MattePromptsMaxBytes)
	return png, nil
}
