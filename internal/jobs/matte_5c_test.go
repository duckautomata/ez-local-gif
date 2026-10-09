package jobs

// Phase 5c (matte_5c.go and the 5c parts of matte.go): the device
// preference (persisted, validated, sent, keyed per device), the status
// shape, the idle / join semantics, the unload call, the stabilise derive,
// the tracker pass with the fake tracker and the live prompt mask. The
// no-ffmpeg tests run everywhere; the derive / pass / mask tests skip
// without ffmpeg on PATH (realTools).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// newMatteRig5c is newMatteRig against the Phase 5c-shaped fake (two
// devices, the tracker).
func newMatteRig5c(t *testing.T, msPerFrame float64, opts Options) *matteRig {
	t.Helper()
	tools := realTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	st := newTestStore(t)
	f := newFakeSidecar5c(t, msPerFrame)
	opts.MatteURL = f.srv.URL
	if opts.Concurrency == 0 {
		opts.Concurrency = 2
	}
	e := &e2e{t: t, ctx: ctx, st: st, tools: tools, dir: t.TempDir()}
	e.m = NewManager(st, tools, opts)
	writeMatteFacts(t, st, &f.ping)
	return &matteRig{e2e: e, f: f}
}

// guidedOp is a sam2-tiny matte op with a box on frame 0 and the given
// extra params (`, "edge": "none"` etc.).
func guidedOp(extra string) recipe.Op {
	return matteOp(`{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.25,0.25,0.75,0.75]}]` + extra + `}`)
}

// thirdWeights is a third weights digest, for a sidecar upgrade that re-pins
// one model (testWeights / otherWeights are taken).
const thirdWeights = "3333333333333333333333333333333333333333333333333333333333333333"

// matteDetailOf returns the render.matte check's detail of rep ("" without one).
func matteDetailOf(rep *discordlint.Report) string {
	for _, c := range rep.Checks {
		if c.Rule == RuleRenderMatte {
			return c.Detail
		}
	}
	return ""
}

// grayAt reads the gray value of pixel (x, y) of a gray PNG (pngPix lifts
// it to R=G=B).
func grayAt(t *testing.T, data []byte, x, y int) uint8 {
	t.Helper()
	pix, w, _ := pngPix(t, data)
	return pix[(y*w+x)*4]
}

// readSettings reads the persisted device preference ("" and ok false
// when no file exists).
func readSettings(t *testing.T, st *store.Store) (dev string, ok bool) {
	t.Helper()
	data, err := os.ReadFile(matteSettingsPath(st))
	if err != nil {
		return "", false
	}
	var s matteSettings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("settings file: %v", err)
	}
	return s.Device, true
}

// ---- the device preference and the status -------------------------------------

// TestMatteDevicePreference: the preference is validated against the
// offered devices, persisted under <data>/mattes/settings.json (a later
// manager reads it), wins over EZLG_MATTE_DEVICE once set ("" included:
// the reset is a setting), and the status reports the effective device,
// the devices, the default models and the default for the effective
// device.
func TestMatteDevicePreference(t *testing.T) {
	st := newTestStore(t)
	f := newFakeSidecar5c(t, 3)
	m := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL})
	if _, err := m.probeMatte(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := m.MatteStatus()
	if s.Device != matte.DeviceCUDA || s.DefaultDevice != matte.DeviceCUDA || strings.Join(s.Devices, ",") != "cuda,cpu" || s.DefaultModel != recipe.MatteModelBiRefNetLite {
		t.Errorf("status = %+v", s)
	}
	if s.DefaultModels[matte.DeviceCPU] != recipe.MatteModelISNetAnime || s.DefaultModels[matte.DeviceCUDA] != recipe.MatteModelBiRefNetLite {
		t.Errorf("default models = %v", s.DefaultModels)
	}
	isnet, sam := s.Models[recipe.MatteModelISNetAnime], s.Models[recipe.MatteModelSAM2Tiny]
	if isnet.Kind != matte.KindSegmenter || isnet.Devices[matte.DeviceCPU].Precision != "fp32" || isnet.Devices[matte.DeviceCUDA].Precision != "fp16" || isnet.Devices[matte.DeviceCPU].MsPerFrame != 30 || isnet.Devices[matte.DeviceCUDA].Size != fakeMatteSize {
		t.Errorf("isnet status = %+v", isnet)
	}
	if sam.Kind != matte.KindTracker || sam.Label != "Guided (click to select)" || sam.Devices[matte.DeviceCUDA].Precision != "bf16" || sam.Devices[matte.DeviceCUDA].Size != 1024 {
		t.Errorf("tracker status = %+v", sam)
	}
	if lite := s.Models[recipe.MatteModelBiRefNetLite]; len(lite.Devices) != 1 || lite.Devices[matte.DeviceCUDA].State != matte.StateReady {
		t.Errorf("lite status = %+v", lite)
	}
	// Residency rides along per device and at the top level (the card tells
	// "ready, loads on the first Compute" from "loaded").
	if isnet.Resident || isnet.Devices[matte.DeviceCUDA].Resident || !sam.Resident || !sam.Devices[matte.DeviceCUDA].Resident || sam.Devices[matte.DeviceCPU].Resident {
		t.Errorf("resident flags: isnet %v/%v, sam %v/%v/%v", isnet.Resident, isnet.Devices[matte.DeviceCUDA].Resident, sam.Resident, sam.Devices[matte.DeviceCUDA].Resident, sam.Devices[matte.DeviceCPU].Resident)
	}
	if _, ok := readSettings(t, st); ok {
		t.Error("a settings file before any SetMatteDevice")
	}

	// cpu: persisted, effective, the default model follows.
	if err := m.SetMatteDevice(context.Background(), matte.DeviceCPU); err != nil {
		t.Fatalf("set cpu: %v", err)
	}
	if dev, ok := readSettings(t, st); !ok || dev != matte.DeviceCPU {
		t.Errorf("settings after cpu = %q, %v", dev, ok)
	}
	if s := m.MatteStatus(); s.Device != matte.DeviceCPU || s.DefaultDevice != matte.DeviceCUDA || s.DefaultModel != recipe.MatteModelISNetAnime {
		t.Errorf("status after cpu = device %s / default %s / model %s", s.Device, s.DefaultDevice, s.DefaultModel)
	}
	if later := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL}); later.matteDevicePref() != matte.DeviceCPU {
		t.Errorf("a later manager reads %q, want the persisted cpu", later.matteDevicePref())
	}
	// An unoffered device: 400 material naming the offered ones, nothing changed.
	if err := m.SetMatteDevice(context.Background(), "tpu"); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "offered: cuda, cpu") {
		t.Errorf("set tpu: %v", err)
	}
	if dev, _ := readSettings(t, st); dev != matte.DeviceCPU || m.matteDevicePref() != matte.DeviceCPU {
		t.Error("a refused device changed the preference")
	}
	// The reset: persisted as "", effective = the sidecar's default.
	if err := m.SetMatteDevice(context.Background(), ""); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if dev, ok := readSettings(t, st); !ok || dev != "" {
		t.Errorf("settings after the reset = %q, %v", dev, ok)
	}
	if s := m.MatteStatus(); s.Device != matte.DeviceCUDA || s.DefaultModel != recipe.MatteModelBiRefNetLite {
		t.Errorf("status after the reset = device %s / model %s", s.Device, s.DefaultModel)
	}

	// EZLG_MATTE_DEVICE is the startup value; a persisted "" wins over it.
	st2 := newTestStore(t)
	m2 := NewManager(st2, fakeTools, Options{MatteURL: f.srv.URL, MatteDevice: matte.DeviceCPU})
	m2.probeMatte(context.Background())
	if s := m2.MatteStatus(); s.Device != matte.DeviceCPU {
		t.Errorf("status under EZLG_MATTE_DEVICE=cpu = %s", s.Device)
	}
	if err := m2.SetMatteDevice(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if s := m2.MatteStatus(); s.Device != matte.DeviceCUDA {
		t.Errorf("a persisted reset did not win over the option: %s", s.Device)
	}
	// A preference the sidecar does not offer any more falls back to its
	// default (the facts changed under the setting).
	m2.mt.mu.Lock()
	m2.mt.pref = "rocm"
	m2.mt.mu.Unlock()
	if s := m2.MatteStatus(); s.Device != matte.DeviceCUDA {
		t.Errorf("an unoffered preference did not fall back: %s", s.Device)
	}

	// A pre-5c sidecar offers exactly its device.
	old := newFakeSidecar(t, testWeights, 3)
	m3 := NewManager(newTestStore(t), fakeTools, Options{MatteURL: old.srv.URL})
	m3.probeMatte(context.Background())
	if s := m3.MatteStatus(); s.Device != matte.DeviceCPU || strings.Join(s.Devices, ",") != "cpu" || s.DefaultModels[matte.DeviceCPU] != recipe.MatteModelISNetAnime || s.Models[recipe.MatteModelISNetAnime].Devices != nil || s.Models[recipe.MatteModelISNetAnime].Kind != "" {
		t.Errorf("pre-5c status = %+v", s)
	}
	if err := m3.SetMatteDevice(context.Background(), matte.DeviceCUDA); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "offered: cpu") {
		t.Errorf("pre-5c set cuda: %v", err)
	}
	if err := m3.SetMatteDevice(context.Background(), matte.DeviceCPU); err != nil {
		t.Errorf("pre-5c set cpu: %v", err)
	}
	// Validation falls back to the facts when the sidecar has not answered
	// this manager yet, and refuses with ErrMatteUnavailable when neither
	// exists; the reset always succeeds.
	cold := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL}) // st holds the facts of the 5c fake
	if err := cold.SetMatteDevice(context.Background(), matte.DeviceCPU); err != nil {
		t.Errorf("set cpu from the facts: %v", err)
	}
	plain := NewManager(newTestStore(t), fakeTools, Options{})
	if err := plain.SetMatteDevice(context.Background(), matte.DeviceCPU); !errors.Is(err, ErrMatteUnavailable) {
		t.Errorf("set cpu without a sidecar: %v", err)
	}
	if err := plain.SetMatteDevice(context.Background(), ""); err != nil {
		t.Errorf("reset without a sidecar: %v", err)
	}
	if dev, ok := readSettings(t, plain.st); !ok || dev != "" {
		t.Errorf("the reset was not persisted without a sidecar: %q, %v", dev, ok)
	}
}

// TestMatteIdentityPerDevice: the identity (size, precision, rate) is the
// device's; a model the device does not offer and a size the tracker does
// not take are refused; the tracker's identity carries the canonical
// prompts, the edge identity resolves "" to the device's default per-frame
// model and "none" to nothing; the clip key differs per device and, for a
// tracker, per prompts and tracking size.
func TestMatteIdentityPerDevice(t *testing.T) {
	st := newTestStore(t)
	srcHash := putSource(t, st, true)
	src, _ := st.GetBlob(srcHash)
	f := newFakeSidecar5c(t, 3)
	writeMatteFacts(t, st, &f.ping)
	m := NewManager(st, fakeTools, Options{MatteURL: f.srv.URL})
	facts, err := m.loadMatteFacts()
	if err != nil {
		t.Fatal(err)
	}
	isnet := matteRequest{Model: recipe.MatteModelISNetAnime}
	cuda, err := identityFor(facts, isnet, matte.DeviceCUDA)
	if err != nil || cuda.Precision != "fp16" || cuda.Size != fakeMatteSize || cuda.MsPerFrame != 3 || cuda.Device != matte.DeviceCUDA || cuda.tracker() {
		t.Errorf("isnet on cuda = %+v, %v", cuda, err)
	}
	cpu, err := identityFor(facts, isnet, matte.DeviceCPU)
	if err != nil || cpu.Precision != "fp32" || cpu.MsPerFrame != 30 {
		t.Errorf("isnet on cpu = %+v, %v", cpu, err)
	}
	if _, err := identityFor(facts, matteRequest{Model: recipe.MatteModelBiRefNetLite}, matte.DeviceCPU); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "not offer model birefnet-lite on CPU") {
		t.Errorf("lite on cpu: %v", err)
	}
	if _, err := identityFor(facts, matteRequest{Model: recipe.MatteModelSAM2Tiny, Size: 16}, matte.DeviceCUDA); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("tracker with a size: %v", err)
	}
	tr, err := identityFor(facts, matteRequest{Model: recipe.MatteModelSAM2Tiny}, matte.DeviceCPU)
	if err != nil || !tr.tracker() || tr.Size != 0 || tr.Precision != "fp32" || tr.MsPerFrame != 60 || tr.Weights != trackerWeights {
		t.Errorf("tracker on cpu = %+v, %v", tr, err)
	}
	fps := "25"
	kc, kp := matteClipKey(src, nil, fps, cuda), matteClipKey(src, nil, fps, cpu)
	if kc == kp {
		t.Error("the clip key does not tell fp16 from fp32")
	}
	// The effective device keys: the preference picks the identity.
	if k := matteClipKey(src, nil, fps, cuda); k != kc {
		t.Error("matteClipKey is not pure")
	}

	// identityOf: prompts / edge on a per-frame model, a tracker without
	// prompts, the edge resolution.
	if _, err := identityOf(facts, matte.DeviceCUDA, matteRequest{Model: recipe.MatteModelISNetAnime, Edge: "none"}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("edge on a segmenter: %v", err)
	}
	if _, err := identityOf(facts, matte.DeviceCUDA, matteRequest{Model: recipe.MatteModelSAM2Tiny}); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "at least one prompt") {
		t.Errorf("tracker without prompts: %v", err)
	}
	box := &[4]float64{0.25, 0.25, 0.75, 0.75}
	prompts := []recipe.MattePrompt{{Frame: 0, Box: box}}
	req := matteRequest{Model: recipe.MatteModelSAM2Tiny, prompts: prompts}
	id, err := identityOf(facts, matte.DeviceCUDA, req)
	if err != nil || id.Prompts != "obj=1|f=0:box=0.2500,0.2500,0.7500,0.7500:pts=" || id.Edge == nil || id.Edge.Model != recipe.MatteModelBiRefNetLite || id.Edge.Weights != otherWeights {
		t.Errorf("tracker identity on cuda = %+v (edge %+v), %v", id, id.Edge, err)
	}
	if idc, err := identityOf(facts, matte.DeviceCPU, req); err != nil || idc.Edge == nil || idc.Edge.Model != recipe.MatteModelISNetAnime || idc.Edge.Precision != "fp32" {
		t.Errorf("tracker identity on cpu = %+v, %v", idc, err)
	}
	req.Edge = recipe.MatteEdgeNone
	if idn, err := identityOf(facts, matte.DeviceCUDA, req); err != nil || idn.Edge != nil {
		t.Errorf("edge none = %+v, %v", idn, err)
	}
	req.Edge = recipe.MatteModelSAM2Tiny
	if _, err := identityOf(facts, matte.DeviceCUDA, req); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("the tracker as its own edge: %v", err)
	}
	req.Edge = recipe.MatteModelBiRefNetLite
	if _, err := identityOf(facts, matte.DeviceCPU, req); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("an edge model the device does not offer: %v", err)
	}
	// The resolved form: tracker + edge identities.
	req.Edge = ""
	id, _ = identityOf(facts, matte.DeviceCUDA, req)
	res := id.resolved()
	if res.Tracker != trackerWeights || res.Edge != recipe.MatteModelBiRefNetLite || res.EdgeWeights != otherWeights || res.EdgeProc != "1" || res.Size != 0 || res.Precision != "bf16" {
		t.Errorf("resolved = %+v", res)
	}
	// The tracker key: prompts and tracking size are in it.
	id.TrackW, id.TrackH = 32, 32
	k1 := matteClipKey(src, nil, fps, id)
	id2 := id
	id2.Prompts = "obj=1|f=1:box=0.2500,0.2500,0.7500,0.7500:pts="
	id3 := id
	id3.TrackW = 64
	if k2, k3 := matteClipKey(src, nil, fps, id2), matteClipKey(src, nil, fps, id3); k1 == k2 || k1 == k3 || k2 == k3 {
		t.Error("the tracker key ignores the prompts or the tracking size")
	}
	// Submit's Resolved carries them, on the effective device.
	m.mt.mu.Lock()
	m.mt.pref, m.mt.prefSet = matte.DeviceCPU, true
	m.mt.mu.Unlock()
	ops, err := m.fillMatteResolved([]recipe.Op{guidedOp("")})
	if err != nil {
		t.Fatal(err)
	}
	if got := matteResolvedOf(t, ops); got == nil || got.Tracker != trackerWeights || got.Edge != recipe.MatteModelISNetAnime || got.Precision != "fp32" {
		t.Errorf("Submit-filled Resolved on cpu = %+v", got)
	}
	// Requests dedupe on the compiler's key.
	reqs, err := matteRequests([]recipe.Op{matteOp(`{"stabilise":"light","keep":["ff0000"]}`), matteOp(`{"stabilise":"light","keep":["ff0000","00ff00"]}`), matteOp(`{"stabilise":"strong"}`), matteOp("")})
	if err != nil || len(reqs) != 3 || reqs[0].keep != 2 || reqs[0].Stabilise != "light" || reqs[1].Stabilise != "strong" || reqs[2].Stabilise != "" {
		t.Errorf("requests = %+v, %v", reqs, err)
	}
}

// TestMatteUnload: UnloadMatte posts /v1/unload with no filter; a plain
// install is a no-op; a sidecar that is gone is an error for the caller to
// log (never a panic, never a long wait).
func TestMatteUnload(t *testing.T) {
	f := newFakeSidecar5c(t, 3)
	m := NewManager(newTestStore(t), fakeTools, Options{MatteURL: f.srv.URL})
	if err := m.UnloadMatte(context.Background()); err != nil {
		t.Fatalf("unload: %v", err)
	}
	f.mu.Lock()
	unloads := append([]string(nil), f.unloads...)
	f.mu.Unlock()
	if len(unloads) != 1 || unloads[0] != "" {
		t.Errorf("unloads = %q, want one with no filter", unloads)
	}
	if err := NewManager(newTestStore(t), fakeTools, Options{}).UnloadMatte(context.Background()); err != nil {
		t.Errorf("unload without a sidecar: %v", err)
	}
	// While a pass is in flight nothing is sent (the pass would only reload
	// its model mid-way; the sidecar's TTL releases it later); once it is
	// done the unload goes through again.
	m.setMatteProgress("k1", func(p *matteProgress) { p.State = MattePendingRunning })
	if err := m.UnloadMatte(context.Background()); err != nil {
		t.Errorf("unload during a pass: %v", err)
	}
	f.mu.Lock()
	during := len(f.unloads)
	f.mu.Unlock()
	if during != 1 {
		t.Errorf("%d unloads after one during a pass, want the first only", during)
	}
	m.clearMatteProgress("k1")
	if err := m.UnloadMatte(context.Background()); err != nil {
		t.Errorf("unload after the pass: %v", err)
	}
	f.mu.Lock()
	after := len(f.unloads)
	f.mu.Unlock()
	if after != 2 {
		t.Errorf("%d unloads after the pass ended, want 2", after)
	}
	f.srv.Close()
	start := time.Now()
	if err := m.UnloadMatte(context.Background()); err == nil {
		t.Error("unload of a stopped sidecar succeeded")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("unload of a stopped sidecar hung")
	}
}

// TestMatteTrackArithmetic: the tracker's up-front refusals and helpers.
func TestMatteTrackArithmetic(t *testing.T) {
	if err := trackBodyRefusal(606, 1024, 576); err != nil { // 1,072,300,032 B
		t.Errorf("606 frames at 1024x576 (under 1 GiB): %v", err)
	}
	if err := trackBodyRefusal(607, 1024, 576); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "trim the clip") { // 1,074,069,504 B
		t.Errorf("607 frames at 1024x576 (over 1 GiB): %v", err)
	}
	if err := trackBodyRefusal(0, 1024, 576); err != nil {
		t.Errorf("unknown count: %v", err)
	}
	tp := trackPromptsOf([]recipe.MattePrompt{{Frame: 3, Points: [][3]float64{{0.5, 0.5, 1}}}, {Frame: 0, Box: &[4]float64{0, 0, 1, 1}}})
	if tp.Obj != 1 || len(tp.Prompts) != 2 || tp.Prompts[0].Frame != 3 || tp.Prompts[1].Box == nil {
		t.Errorf("trackPromptsOf = %+v", tp)
	}
	if err := checkPromptFrames(tp, 4); err != nil {
		t.Errorf("frames 0 and 3 of 4: %v", err)
	}
	if err := checkPromptFrames(tp, 3); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "frame 3") || !strings.Contains(err.Error(), "3 frames") {
		t.Errorf("frame 3 of 3: %v", err)
	}
	if err := checkPromptFrames(tp, 0); err != nil {
		t.Errorf("unknown count: %v", err)
	}
	if got := matteTrackEstimateMS(10, 48); got != 650 { // 10 × (48 + 2) × 1.3
		t.Errorf("track estimate = %d, want 650", got)
	}
	if matteTrackEstimateMS(0, 48) != 0 {
		t.Error("an unknown count has an estimate")
	}
	a := promptMaskKey("k", 3, tp)
	if a != promptMaskKey("k", 3, trackPromptsOf([]recipe.MattePrompt{{Frame: 0, Box: &[4]float64{0, 0, 1, 1}}, {Frame: 3, Points: [][3]float64{{0.5, 0.5, 1}}}})) {
		t.Error("the prompt order changed the mask key")
	}
	if a == promptMaskKey("k", 4, tp) || a == promptMaskKey("j", 3, tp) {
		t.Error("the frame or the clip key is not in the mask key")
	}
	if (&ErrMattePending{State: MattePendingIdle, Device: matte.DeviceCUDA, EstimateMS: 12_000}).Error() != "jobs: AI matte not computed (~12 s on GPU) — press Compute matte or Render" {
		t.Errorf("idle message = %q", (&ErrMattePending{State: MattePendingIdle, Device: matte.DeviceCUDA, EstimateMS: 12_000}).Error())
	}
	if matteProgressMessage(ErrMattePending{State: MattePendingIdle}) != "not computed" {
		t.Error("idle progress message")
	}
	// A tracker pass reads "tracking N frames" until its masks arrive (the
	// whole clip is one POST), then counts like any pass.
	track := ErrMattePending{State: MattePendingRunning, Phase: MattePhaseTracking, Total: 45, Device: matte.DeviceCUDA}
	if got := matteProgressMessage(track); got != "tracking 45 frames · GPU" {
		t.Errorf("tracking progress message = %q", got)
	}
	if got := track.Error(); got != "jobs: AI matte pending: tracking 45 frames · GPU" {
		t.Errorf("tracking pending error = %q", got)
	}
	if got := matteProgressMessage(ErrMattePending{State: MattePendingRunning, Phase: MattePhaseTracking, Done: 45, Total: 45, Device: matte.DeviceCUDA}); got != "45/45 · GPU" {
		t.Errorf("tracking progress message at the end = %q", got)
	}
	if got := matteProgressMessage(ErrMattePending{State: MattePendingLoading, Phase: MattePhaseTracking, Total: 45}); got != "loading model" {
		t.Errorf("tracking while loading = %q", got)
	}
	if p := (matteProgress{State: MattePendingRunning, Phase: MattePhaseTracking, Total: 3}).pending(); p.Phase != MattePhaseTracking {
		t.Error("pending() drops the phase")
	}

	// clampPromptFrames: a prompt on the plan's phantom last slot (frame n
	// of an n-frame clip — the held last frame the scrubber, the still and
	// the live mask all showed) moves to n−1 and merges with that frame's
	// own prompts (points appended, one box); anything further past the
	// clip stays for checkPromptFrames.
	phantom := trackPromptsOf([]recipe.MattePrompt{{Frame: 20, Box: &[4]float64{0.1, 0.1, 0.5, 0.5}}, {Frame: 0, Points: [][3]float64{{0.5, 0.5, 1}}}})
	clamped, moved := clampPromptFrames(phantom, 20)
	if moved != 1 || len(clamped.Prompts) != 2 || clamped.Prompts[1].Frame != 19 || clamped.Prompts[1].Box == nil || clamped.Prompts[0].Frame != 0 {
		t.Errorf("clamp of the phantom slot = %+v (moved %d)", clamped.Prompts, moved)
	}
	if err := checkPromptFrames(clamped, 20); err != nil {
		t.Errorf("clamped prompts refused: %v", err)
	}
	both := trackPromptsOf([]recipe.MattePrompt{{Frame: 19, Points: [][3]float64{{0.2, 0.2, 1}}, Box: &[4]float64{0, 0, 1, 1}}, {Frame: 20, Points: [][3]float64{{0.7, 0.7, 0}}, Box: &[4]float64{0.1, 0.1, 0.5, 0.5}}})
	merged, moved := clampPromptFrames(both, 20)
	if moved != 1 || len(merged.Prompts) != 1 || merged.Prompts[0].Frame != 19 || len(merged.Prompts[0].Points) != 2 || merged.Prompts[0].Points[1][2] != 0 || *merged.Prompts[0].Box != [4]float64{0, 0, 1, 1} {
		t.Errorf("merge of frames 19 and 20 = %+v (moved %d)", merged.Prompts, moved)
	}
	if len(both.Prompts[0].Points) != 1 {
		t.Error("clampPromptFrames mutated its input")
	}
	if same, moved := clampPromptFrames(phantom, 21); moved != 0 || len(same.Prompts) != 2 || same.Prompts[0].Frame != 20 {
		t.Errorf("a prompt inside the clip was moved: %+v (moved %d)", same.Prompts, moved)
	}
	if _, moved := clampPromptFrames(phantom, 19); moved != 0 {
		t.Error("a prompt two frames past the clip was moved")
	} else if err := checkPromptFrames(phantom, 19); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("a prompt two frames past the clip: %v", err)
	}
	if w, h := enc.TrackSize(32, 32); w != 32 || h != 32 {
		t.Errorf("TrackSize(32, 32) = %dx%d", w, h)
	}
}

// ---- real ffmpeg --------------------------------------------------------------

// TestMattePassSendsDevice: with a 5c sidecar the pass runs on the
// effective device — device= on every POST, the device's precision in the
// identity, the key and the manifest — and the preference moved makes a
// new memo under the other identity; a pre-5c sidecar never sees device=.
func TestMattePassSendsDevice(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{})
	clip := e.clipDistinct()
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "webp"}

	cuda, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err != nil {
		t.Fatalf("pass on the default device: %v", err)
	}
	rm := cuda[0]
	if rm.Device != matte.DeviceCUDA || rm.Precision != "fp16" || rm.Manifest.Device != matte.DeviceCUDA || rm.Manifest.Precision != "fp16" || rm.SeqDir != rm.Dir {
		t.Errorf("resolved on cuda = %+v (manifest %+v)", rm, rm.Manifest)
	}
	for _, d := range e.f.seenDevices() {
		if d != matte.DeviceCUDA {
			t.Errorf("a request carried device=%q, want cuda", d)
		}
	}
	if err := e.m.SetMatteDevice(e.ctx, matte.DeviceCPU); err != nil {
		t.Fatal(err)
	}
	before, _ := e.f.stats()
	cpu, err := e.m.resolveMattes(e.ctx, clip, ops, out, matteModeRender)
	if err != nil {
		t.Fatalf("pass on cpu: %v", err)
	}
	if cpu[0].ClipKey == rm.ClipKey || cpu[0].Precision != "fp32" || cpu[0].Manifest.Device != matte.DeviceCPU {
		t.Errorf("resolved on cpu = %+v", cpu[0])
	}
	if after, _ := e.f.stats(); after == before {
		t.Error("the cpu identity was served from the cuda memo")
	}
	seen := e.f.seenDevices()
	if seen[len(seen)-1] != matte.DeviceCPU {
		t.Errorf("the cpu pass sent device=%q", seen[len(seen)-1])
	}
	// The pending state names the effective device.
	if _, err := e.m.resolveMattes(e.ctx, clip, []recipe.Op{{Kind: recipe.OpTrim, Params: []byte(`{"start":1}`)}, matteOp("")}, out, matteModePreview); err == nil {
		t.Error("a trimmed plain preview found a memo")
	} else if p := new(ErrMattePending); !errors.As(err, &p) || p.State != MattePendingIdle || p.Device != matte.DeviceCPU || p.EstimateMS != 10*(10+2) {
		t.Errorf("idle on cpu: %v", err)
	}
	// Submit's identity follows the preference too.
	opsCPU, _ := e.m.fillMatteResolved(ops)
	if got := matteResolvedOf(t, opsCPU); got == nil || got.Precision != "fp32" {
		t.Errorf("Resolved on cpu = %+v", got)
	}

	// A pre-5c sidecar: no device parameter at all.
	old := newMatteRig(t, 1, Options{})
	if _, err := old.m.resolveMattes(old.ctx, old.clipDistinct(), ops, out, matteModeRender); err != nil {
		t.Fatal(err)
	}
	for _, d := range old.f.seenDevices() {
		if d != "" {
			t.Errorf("a pre-5c sidecar saw device=%q", d)
		}
	}
}

// TestMatteStabiliseDerived: a memo on disk (no sidecar) with a one-frame
// pop is derived into <clipdir>/stab-light/ on a plain preview — N files,
// the pop gone, the ends passed through —, reused by the next request (no
// second run, no temp left), and strong goes to its own dir; the plain op
// keeps reading the raw memo. The derive is memoised under the memo dir
// the store sweeps with the clip.
func TestMatteStabiliseDerived(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("clip.mov", opaqueClip, "rgb24")
	out := recipe.Output{Format: "gif", FPS: 10}
	light := []recipe.Op{matteOp(`{"stabilise":"light"}`)}
	key, fps, _ := matteKeyFor(t, clip, light, out, ping, recipe.MatteModelISNetAnime, 0)
	pop := func(i, x, y int) uint8 {
		if i == 4 {
			return 0 // a one-frame drop-out
		}
		return 200
	}
	dir := writeMatteMemo(t, e.st, key, clip.Hash, ping, recipe.MatteModelISNetAnime, fps, 10, 64, 48, pop)

	got, err := e.m.resolveMattes(e.ctx, clip, light, out, matteModePreview)
	if err != nil {
		t.Fatalf("resolve with stabilise light: %v", err)
	}
	rm := got[0]
	want := store.MatteDerivedDir(dir, store.MatteStabName(recipe.MatteStabiliseLight))
	if rm.Dir != dir || rm.SeqDir != want || rm.Stabilise != recipe.MatteStabiliseLight {
		t.Fatalf("resolved = %+v, want SeqDir %s", rm, want)
	}
	if n := countSequence(want); n != 10 {
		t.Errorf("%d derived frames, want 10", n)
	}
	for i, wantGray := range map[int]uint8{1: 200, 5: 200, 10: 200} {
		data, err := os.ReadFile(filepath.Join(want, matte.FrameFile(i)))
		if err != nil {
			t.Fatalf("derived frame %d: %v", i, err)
		}
		if g := grayAt(t, data, 10, 10); g != wantGray {
			t.Errorf("derived frame %d = %d, want %d (the pop removed, the ends passed through)", i, g, wantGray)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, matte.FrameFile(5))); grayAt(t, raw, 10, 10) != 0 {
		t.Error("the raw memo was rewritten")
	}
	if _, err := os.Stat(store.MatteDerivedTmpDir(dir, store.MatteStabName(recipe.MatteStabiliseLight))); err == nil {
		t.Error("the derive's temp dir was left behind")
	}
	// Reused: the files are the same ones.
	info1, _ := os.Stat(filepath.Join(want, matte.FrameFile(1)))
	time.Sleep(20 * time.Millisecond)
	if again, err := e.m.resolveMattes(e.ctx, clip, light, out, matteModePreview); err != nil || again[0].SeqDir != want {
		t.Fatalf("second resolve: %v", err)
	}
	if info2, _ := os.Stat(filepath.Join(want, matte.FrameFile(1))); !info2.ModTime().Equal(info1.ModTime()) {
		t.Error("the derived sequence was made again")
	}
	// Strong: its own dir, same count. Plain: the raw memo.
	strong := []recipe.Op{matteOp(`{"stabilise":"strong"}`)}
	if got, err := e.m.resolveMattes(e.ctx, clip, strong, out, matteModeRender); err != nil || got[0].SeqDir != store.MatteDerivedDir(dir, store.MatteStabName(recipe.MatteStabiliseStrong)) || countSequence(got[0].SeqDir) != 10 {
		t.Errorf("strong: %v, %+v", err, got)
	}
	if got, err := e.m.resolveMattes(e.ctx, clip, []recipe.Op{matteOp("")}, out, matteModePreview); err != nil || got[0].SeqDir != dir {
		t.Errorf("plain: %v, %+v", err, got)
	}
	// Two ops of one model with other modes resolve to two requests and
	// the plan's inputs each read their own sequence.
	both := []recipe.Op{matteOp(`{"stabilise":"light"}`), matteOp("")}
	got, err = e.m.resolveMattes(e.ctx, clip, both, out, matteModePreview)
	if err != nil || len(got) != 2 || got[0].SeqDir != want || got[1].SeqDir != dir {
		t.Fatalf("two modes: %v, %+v", err, got)
	}
	plan, err := graph.CompileWithSources([]recipe.ProbeInfo{*clip.Info}, both, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := fillMatteInputs(plan, got); err != nil {
		t.Fatal(err)
	}
	for _, in := range plan.ExtraInputs {
		wantDir := dir
		if in.Matte.Stabilise != "" {
			wantDir = want
		}
		if in.Path != filepath.Join(wantDir, matte.FramePattern) || in.Matte.Frames != 10 {
			t.Errorf("input %+v reads %s, want %s", in.Matte, in.Path, wantDir)
		}
	}
	if _, err := e.m.deriveStabilised(e.ctx, dir, "bogus", 10); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("bogus mode: %v", err)
	}
}

// TestMatteTrackPass: the guided model through the fake tracker — a plain
// preview is idle, the pass streams the whole clip at the tracking size in
// ONE /v1/track with the prompts and files the masks as the clip memo
// (manifest with prompts and tracking size, no spool left), a plain preview
// then hits it; stabilise derives under it; an edge model runs its own
// pass and the gate derives under the tracker's memo (core 255, band = the
// edge matte, outside 0), with stabilise nested under the gate; a prompt
// past the clip and other prompts are refused / re-keyed; the render.matte
// line names the guided model, its size and the edge.
func TestMatteTrackPass(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{})
	clip := e.clipDistinct() // 32x32, 20 frames at 10 fps: the tracking size is 32x32
	out := recipe.Output{Format: "webp"}
	none := []recipe.Op{guidedOp(`,"edge":"none"`)}

	var pending *ErrMattePending
	if _, err := e.m.resolveMattes(e.ctx, clip, none, out, matteModePreview); !errors.As(err, &pending) || pending.State != MattePendingIdle {
		t.Fatalf("plain preview of a guided op: %v, want idle", err)
	}
	got, err := e.m.resolveMattes(e.ctx, clip, none, out, matteModeRender)
	if err != nil {
		t.Fatalf("track pass: %v", err)
	}
	rm := got[0]
	man := rm.Manifest
	if rm.Model != recipe.MatteModelSAM2Tiny || rm.Size != 0 || rm.Precision != "bf16" || rm.Device != matte.DeviceCUDA || rm.Edge != "" || rm.SeqDir != rm.Dir || man == nil {
		t.Fatalf("resolved = %+v", rm)
	}
	if man.Frames != 20 || man.TrackW != 32 || man.TrackH != 32 || man.Size != 0 || man.Precision != "bf16" || man.Device != matte.DeviceCUDA || man.Weights != trackerWeights || man.Prompts != "obj=1|f=0:box=0.2500,0.2500,0.7500,0.7500:pts=" {
		t.Errorf("manifest = %+v", man)
	}
	e.f.mu.Lock()
	tracks, sizes, hdrs, bodies := e.f.tracks, append([][2]int(nil), e.f.trackSizes...), append([]string(nil), e.f.trackPrompts...), e.f.trackBodies
	e.f.mu.Unlock()
	if tracks != 1 || len(sizes) != 1 || sizes[0] != [2]int{32, 32} {
		t.Fatalf("%d tracks of %v, want one at 32x32", tracks, sizes)
	}
	if !strings.Contains(hdrs[0], `"frame":0`) || !strings.Contains(hdrs[0], `"box":[0.25,0.25,0.75,0.75]`) || !strings.Contains(hdrs[0], `"obj":1`) {
		t.Errorf("prompts header = %s", hdrs[0])
	}
	if len(bodies[0]) != 20*32*32*3 {
		t.Fatalf("track body is %d bytes, want 20 frames of 32x32 rgb24", len(bodies[0]))
	}
	for i := 0; i < 20; i++ {
		r, g, b := distinctColour(i)
		px := bodies[0][i*32*32*3:]
		if !near8(px[0], uint8(r), 1) || !near8(px[1], uint8(g), 1) || !near8(px[2], uint8(b), 1) {
			t.Errorf("streamed frame %d starts (%d,%d,%d), want (%d,%d,%d)", i, px[0], px[1], px[2], r, g, b)
		}
	}
	for _, n := range []int{1, 10, 20} {
		data, err := os.ReadFile(filepath.Join(rm.Dir, matte.FrameFile(n)))
		if err != nil {
			t.Fatalf("mask %d: %v", n, err)
		}
		if grayAt(t, data, 16, 16) != 255 || grayAt(t, data, 2, 2) != 0 {
			t.Errorf("mask %d: centre %d / corner %d, want 255 / 0", n, grayAt(t, data, 16, 16), grayAt(t, data, 2, 2))
		}
	}
	if _, err := os.Stat(filepath.Join(rm.Dir, matteTrackSpoolName)); err == nil {
		t.Error("the spool file was left in the memo")
	}
	if entries, _ := os.ReadDir(filepath.Dir(e.st.MatteFrameDir("x", 1, "p", "w", "1"))); len(entries) != 0 {
		t.Errorf("a tracker pass filed into the frames store: %v", entries)
	}
	if d := e.f.seenDevices(); d[len(d)-1] != matte.DeviceCUDA {
		t.Errorf("the track sent device=%q", d[len(d)-1])
	}
	if _, ok := e.m.matteProgressFor(rm.ClipKey); ok {
		t.Error("progress left behind")
	}
	// A plain preview now hits the memo; stabilise derives under it.
	if again, err := e.m.resolveMattes(e.ctx, clip, none, out, matteModePreview); err != nil || again[0].ClipKey != rm.ClipKey {
		t.Fatalf("hit: %v", err)
	}
	stab := []recipe.Op{guidedOp(`,"edge":"none","stabilise":"light"`)}
	if s, err := e.m.resolveMattes(e.ctx, clip, stab, out, matteModePreview); err != nil || s[0].SeqDir != store.MatteDerivedDir(rm.Dir, "stab-light") || countSequence(s[0].SeqDir) != 20 {
		t.Errorf("stabilised tracker matte: %v, %+v", err, s)
	}
	if tr, _ := e.f.trackStats(); tr != 1 {
		t.Errorf("%d tracks after the hits, want 1", tr)
	}

	// The edge: a plain preview is idle (the tracker memo is there, the
	// edge model's is not); the eager one runs the edge pass (birefnet-lite,
	// the cuda default) and derives the gate.
	edge := []recipe.Op{guidedOp("")}
	if _, err := e.m.resolveMattes(e.ctx, clip, edge, out, matteModePreview); !errors.As(err, &pending) || pending.State != MattePendingIdle {
		t.Fatalf("plain preview with an edge: %v, want idle", err)
	}
	if tr, _ := e.f.trackStats(); tr != 1 {
		t.Fatal("an idle preview tracked again")
	}
	got, err = e.m.resolveMattes(WithMatteEager(e.ctx, true), clip, edge, out, matteModePreview)
	if err != nil {
		t.Fatalf("eager preview with an edge: %v", err)
	}
	g := got[0]
	if g.ClipKey != rm.ClipKey || g.Edge != recipe.MatteModelBiRefNetLite || g.edge == nil || g.edge.Model != recipe.MatteModelBiRefNetLite || g.edge.Manifest == nil || g.edge.Manifest.Frames != 20 {
		t.Fatalf("gated resolution = %+v (edge %+v)", g, g.edge)
	}
	// The gate is named by the edge memo's identity (its clip key), never
	// by the model id alone: the tracker's key leaves the edge out.
	gated := store.MatteDerivedDir(rm.Dir, store.MatteGatedName(recipe.MatteModelBiRefNetLite, g.edge.ClipKey, matteGateRadius))
	if g.SeqDir != gated || !strings.Contains(filepath.Base(gated), g.edge.ClipKey[:store.MatteGatedKeyChars]) {
		t.Fatalf("gated SeqDir = %q, want %q (named by the edge key)", g.SeqDir, gated)
	}
	if posts, frames := e.f.stats(); posts != 3 || frames != 20 {
		t.Errorf("edge pass: %d POSTs of %d frames; want 3 of 20", posts, frames)
	}
	if tr, _ := e.f.trackStats(); tr != 1 {
		t.Errorf("%d tracks after the edge pass, want the one", tr)
	}
	if n := countSequence(gated); n != 20 {
		t.Fatalf("%d gated frames, want 20", n)
	}
	// The gate on frame 1: core (16,16) → 255; the band (9,9) → the edge
	// matte (the fake's 255 − luma of frame 0); outside (2,2) → 0.
	data, _ := os.ReadFile(filepath.Join(gated, matte.FrameFile(1)))
	if c, band, o := grayAt(t, data, 16, 16), grayAt(t, data, 9, 9), grayAt(t, data, 2, 2); c != 255 || !near8(band, distinctMatte(0), 1) || o != 0 {
		t.Errorf("gate frame 1: core %d, band %d (want %d), outside %d", c, band, distinctMatte(0), o)
	}
	if res := g.identity().resolved(); res.Tracker != trackerWeights || res.Edge != recipe.MatteModelBiRefNetLite || res.EdgeWeights != otherWeights {
		t.Errorf("resolved identity = %+v", res)
	}
	// Stabilise nests under the gate.
	both := []recipe.Op{guidedOp(`,"stabilise":"light"`)}
	if s, err := e.m.resolveMattes(e.ctx, clip, both, out, matteModePreview); err != nil || s[0].SeqDir != store.MatteDerivedDir(gated, "stab-light") || countSequence(s[0].SeqDir) != 20 {
		t.Errorf("gate + stabilise: %v, %+v", err, s)
	}
	// The plan reads the gated sequence and a render's info line names it all.
	plan, err := graph.CompileWithSources([]recipe.ProbeInfo{*clip.Info}, edge, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := fillMatteInputs(plan, got); err != nil {
		t.Fatal(err)
	}
	if in := plan.ExtraInputs[0]; in.Path != filepath.Join(gated, matte.FramePattern) || in.Matte.Frames != 20 {
		t.Errorf("plan input = %+v", in)
	}
	if res := checkMatteResolved(func() []recipe.Op { o, _ := e.m.fillMatteResolved(edge); return o }(), got); res != nil {
		t.Errorf("checkMatteResolved: %v", res)
	}
	rep := &discordlint.Report{}
	applyMatteInfo(rep, got)
	d := ""
	for _, c := range rep.Checks {
		if c.Rule == RuleRenderMatte {
			d = c.Detail
		}
	}
	if !strings.Contains(d, "guided (sam2-tiny) + edge birefnet-lite · 32x32 bf16") || !strings.Contains(d, "edge weights "+short(otherWeights)) || !strings.Contains(d, "1 prompted frame") || !strings.Contains(d, "cuda") {
		t.Errorf("render.matte detail = %q", d)
	}
	// The recipe notes do not repeat the guided head the detail carries.
	applyMatteRecipeNotes(rep, func() []recipe.Op { o, _ := e.m.fillMatteResolved(edge); return o }())
	if n := strings.Count(strings.ToLower(matteDetailOf(rep)), "guided"); n != 1 {
		t.Errorf("render.matte detail names guided %d times: %q", n, matteDetailOf(rep))
	}

	// A STALE GATE: the edge model's weights change (a sidecar upgrade
	// re-pins birefnet-lite; the tracker stays) — the edge memo is re-keyed
	// and its pass redone, and the gate must be derived AGAIN under the new
	// edge key, never served from the old edge memo's gate.
	e.f.setModelOf(recipe.MatteModelBiRefNetLite, func(ms *matte.ModelState) { ms.Weights = thirdWeights; ms.GraphDigest = "g-" + thirdWeights })
	e.f.set(func(f *fakeSidecar) { f.ping.Instance = "fake-2" })
	if _, err := e.m.probeMatte(e.ctx); err != nil { // the probe persists the new facts (as the next scheduled one would)
		t.Fatal(err)
	}
	regated, err := e.m.resolveMattes(e.ctx, clip, edge, out, matteModeRender)
	if err != nil {
		t.Fatalf("resolve after the edge weights changed: %v", err)
	}
	rg := regated[0]
	if rg.ClipKey != rm.ClipKey || rg.edge == nil || rg.edge.ClipKey == g.edge.ClipKey || rg.edge.Manifest == nil || rg.edge.Manifest.Weights != thirdWeights {
		t.Fatalf("re-keyed edge resolution = %+v (edge %+v)", rg, rg.edge)
	}
	if rg.SeqDir == gated || rg.SeqDir != store.MatteDerivedDir(rm.Dir, store.MatteGatedName(recipe.MatteModelBiRefNetLite, rg.edge.ClipKey, matteGateRadius)) || countSequence(rg.SeqDir) != 20 {
		t.Errorf("the new edge memo served the old gate: SeqDir %q (old %q, %d frames)", rg.SeqDir, gated, countSequence(rg.SeqDir))
	}
	if posts, frames := e.f.stats(); posts != 6 || frames != 40 {
		t.Errorf("edge passes: %d POSTs of %d frames; want 6 of 40 (the edge pass redone under the new weights)", posts, frames)
	}
	if tr, _ := e.f.trackStats(); tr != 1 {
		t.Errorf("%d tracks after the edge re-key, want the one", tr)
	}
	if res := rg.identity().resolved(); res.EdgeWeights != thirdWeights || res.Tracker != trackerWeights {
		t.Errorf("re-keyed resolved identity = %+v", res)
	}

	// Refusals and re-keys.
	past := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":25,"box":[0.25,0.25,0.75,0.75]}],"edge":"none"}`)}
	if _, err := e.m.resolveMattes(e.ctx, clip, past, out, matteModeRender); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "20 frames") {
		t.Errorf("a prompt past the clip: %v", err)
	}
	other := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":2,"points":[[0.5,0.5,1]]}],"edge":"none"}`)}
	o, err := e.m.resolveMattes(e.ctx, clip, other, out, matteModeRender)
	if tr, _ := e.f.trackStats(); err != nil || o[0].ClipKey == rm.ClipKey || tr != 2 {
		t.Errorf("other prompts: %v, key equal %v, %d tracks", err, err == nil && o[0].ClipKey == rm.ClipKey, tr)
	}
	// A 503 "model loading" on the track is retried.
	e.f.set(func(f *fakeSidecar) { f.retryTrack503 = 1 })
	retry := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":2,"points":[[0.25,0.25,1]]}],"edge":"none"}`)}
	if _, err := e.m.resolveMattes(e.ctx, clip, retry, out, matteModeRender); err != nil {
		t.Errorf("track after a 503: %v", err)
	}
	// A failed track leaves no memo and no temp.
	e.f.set(func(f *fakeSidecar) { f.trackFail = 500 })
	fail := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":2,"points":[[0.75,0.75,1]]}],"edge":"none"}`)}
	if _, err := e.m.resolveMattes(e.ctx, clip, fail, out, matteModeRender); err == nil || !strings.Contains(err.Error(), "HTTP 500") || isContextError(err) {
		t.Errorf("failed track: %v", err)
	}
	if _, tmps := matteDirs(t, e.st); len(tmps) != 0 {
		t.Errorf("temp dirs after a failed track: %v", tmps)
	}
}

// TestMattePromptMask: the live overlay — one frame of the matte plan at
// the tracking size through /v1/track/frame with that frame's prompts,
// memoised on scratch (a second ask is a hit, other prompts another
// call); a loading tracker is pending after a warm-up; a frame past the
// clip, a non-tracker op and no prompts are refused.
func TestMattePromptMask(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{})
	clip := e.clipDistinct()
	out := recipe.Output{Format: "gif", FPS: 10}
	ops := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.25,0.25,0.75,0.75]},{"frame":3,"box":[0.5,0.5,1,1]}]}`)}
	all := trackPromptsOf([]recipe.MattePrompt{{Frame: 0, Box: &[4]float64{0.25, 0.25, 0.75, 0.75}}, {Frame: 3, Box: &[4]float64{0.5, 0.5, 1, 1}}})
	prompts := all.ForFrame(3)

	png, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 3, prompts)
	if err != nil {
		t.Fatalf("prompt mask: %v", err)
	}
	if _, w, h := pngPix(t, png); w != 32 || h != 32 {
		t.Errorf("mask is %dx%d, want 32x32", w, h)
	}
	if grayAt(t, png, 24, 24) != 255 || grayAt(t, png, 4, 4) != 0 {
		t.Errorf("mask: inside %d / outside %d", grayAt(t, png, 24, 24), grayAt(t, png, 4, 4))
	}
	e.f.mu.Lock()
	calls, hdr, sizes := e.f.trackFrames, e.f.trackPrompts[len(e.f.trackPrompts)-1], e.f.trackSizes
	e.f.mu.Unlock()
	if calls != 1 || sizes[0] != [2]int{32, 32} || !strings.Contains(hdr, `"frame":3`) || strings.Contains(hdr, `"frame":0`) {
		t.Errorf("track/frame calls %d, size %v, header %s", calls, sizes, hdr)
	}
	if d := e.f.seenDevices(); d[len(d)-1] != matte.DeviceCUDA {
		t.Errorf("the mask sent device=%q", d[len(d)-1])
	}
	entries, _ := os.ReadDir(e.st.MattePromptsMemoDir())
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".png") {
		t.Errorf("prompt memo = %v, want one png", entries)
	}
	// A hit, then other prompts.
	if again, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 3, prompts); err != nil || !bytes.Equal(again, png) {
		t.Errorf("second ask: %v, same %v", err, bytes.Equal(again, png))
	}
	if _, fr := e.f.trackStats(); fr != 1 {
		t.Errorf("%d track/frame calls after a memo hit, want 1", fr)
	}
	moved := trackPromptsOf([]recipe.MattePrompt{{Frame: 3, Box: &[4]float64{0, 0, 0.5, 0.5}}})
	m2, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 3, moved)
	if _, fr := e.f.trackStats(); err != nil || grayAt(t, m2, 4, 4) != 255 || grayAt(t, m2, 24, 24) != 0 || fr != 2 {
		t.Errorf("moved box: %v, %d calls", err, fr)
	}
	// Refusals.
	if _, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 25, all.ForFrame(3)); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "20 frames") {
		t.Errorf("frame past the clip: %v", err)
	}
	if _, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, []recipe.Op{matteOp("")}, out, 3, prompts); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("non-tracker op: %v", err)
	}
	if _, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 3, matte.TrackPrompts{}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("no prompts: %v", err)
	}
	if _, err := e.m.MattePromptMask(e.ctx, []string{strings.Repeat("0", 64)}, ops, out, 3, prompts); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown source: %v", err)
	}
	// Loading: pending after a warm-up, nothing memoised.
	e.f.setModelOf(recipe.MatteModelSAM2Tiny, func(ms *matte.ModelState) { ms.State = matte.StateLoading })
	e.m.probeMatte(e.ctx)
	e.f.mu.Lock()
	warms := len(e.f.warms)
	e.f.mu.Unlock()
	fresh := trackPromptsOf([]recipe.MattePrompt{{Frame: 3, Box: &[4]float64{0.1, 0.1, 0.4, 0.4}}})
	var pending *ErrMattePending
	if _, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 3, fresh); !errors.As(err, &pending) || pending.State != MattePendingLoading || pending.Device != matte.DeviceCUDA {
		t.Errorf("mask while loading: %v", err)
	}
	e.f.mu.Lock()
	warmed := len(e.f.warms) > warms
	e.f.mu.Unlock()
	if !warmed {
		t.Error("no warm-up for a loading tracker")
	}
	e.f.setModelOf(recipe.MatteModelSAM2Tiny, func(ms *matte.ModelState) { ms.State = matte.StateReady })
	e.m.probeMatte(e.ctx)
	// A 503 with a retry delay on the frame is pending too.
	e.f.set(func(f *fakeSidecar) { f.retryTrack503 = 1 })
	if _, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 3, fresh); !errors.As(err, &pending) || pending.State != MattePendingLoading {
		t.Errorf("mask under a 503: %v", err)
	}
	if m3, err := e.m.MattePromptMask(e.ctx, []string{clip.Hash}, ops, out, 3, fresh); err != nil || grayAt(t, m3, 8, 8) != 255 {
		t.Errorf("mask after the 503: %v", err)
	}
}
