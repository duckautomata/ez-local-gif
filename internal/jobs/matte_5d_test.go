package jobs

// Phase 5d: mask-prompted tracking (matte_5d.go) against the fake sidecar
// with a real ffmpeg — a guided op whose prompt is {frame, maskFrom:
// "edge"} resolves the edge model's memo first, derives the frame's matte
// at the tracking size under it, keys the tracker's memo with the mask's
// digest and sends the PNG as the track's trailing mask record; the live
// overlay (MattePromptMask) does the same when the edge memo is on disk
// and answers idle with a reason when it is not; the helpers.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// maskedOp is a guided op prompted with the edge matte of frame frame
// (plus extra params, e.g. `,"edge":"none"`).
func maskedOp(frame int, extra string) recipe.Op {
	return matteOp(`{"model":"sam2-tiny","prompts":[{"frame":` + strconv.Itoa(frame) + `,"maskFrom":"edge"}]` + extra + `}`)
}

// lastTrackMask returns the digest of the mask record of the fake's last
// track / track-frame request ("" when it carried none).
func (f *fakeSidecar) lastTrackMask() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.trackMasks) == 0 {
		return "<no track>"
	}
	return f.trackMasks[len(f.trackMasks)-1]
}

// lastTrackHeader returns the prompts header of the fake's last track /
// track-frame request.
func (f *fakeSidecar) lastTrackHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.trackPrompts) == 0 {
		return ""
	}
	return f.trackPrompts[len(f.trackPrompts)-1]
}

// allGray reports whether every pixel of the gray PNG data is v.
func allGray(t *testing.T, data []byte, v uint8) bool {
	t.Helper()
	pix, w, h := pngPix(t, data)
	for i := 0; i < w*h; i++ {
		if pix[i*4] != v {
			return false
		}
	}
	return true
}

// TestMatteTrackMaskPrompt: the pass. Nothing computed → idle; the edge
// alone computed (the user's Compute with General) → the masked op's plain
// preview is still idle but the mask prompt is derived under the edge
// memo; the render hits the edge memo, tracks ONCE with the mask record
// (its digest in the fake, in the manifest's prompts and in the clip key)
// and gates under the tracker's memo; a mask from another frame or clicks
// on the mask frame key differently; an eager preview starts the whole
// chain on a fresh clip; edge "none" and a frame past the clip are refused
// before any pass; the info line notes the mask.
func TestMatteTrackMaskPrompt(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{})
	clip := e.clipDistinct() // 32x32, 20 frames at 10 fps: the tracking size is 32x32
	out := recipe.Output{Format: "webp"}
	masked := []recipe.Op{maskedOp(0, "")}
	edgeOp := []recipe.Op{matteOp(`{"model":"birefnet-lite"}`)}
	var pending *ErrMattePending

	// 1. Nothing computed: idle, nothing ran.
	if _, err := e.m.resolveMattes(e.ctx, clip, masked, out, matteModePreview); !errors.As(err, &pending) || pending.State != MattePendingIdle {
		t.Fatalf("plain preview with nothing computed: %v, want idle", err)
	}
	if posts, _ := e.f.stats(); posts != 0 {
		t.Fatalf("%d edge POSTs after an idle preview", posts)
	}
	if tracks, _ := e.f.trackStats(); tracks != 0 {
		t.Fatalf("%d tracks after an idle preview", tracks)
	}

	// 2. The edge alone, then the masked op's plain preview: idle (no
	// tracker memo), but the mask prompt is derived from the edge memo.
	edge, err := e.m.resolveMattes(e.ctx, clip, edgeOp, out, matteModeRender)
	if err != nil {
		t.Fatalf("edge pass: %v", err)
	}
	edgeDir := edge[0].Dir
	if _, err := e.m.resolveMattes(e.ctx, clip, masked, out, matteModePreview); !errors.As(err, &pending) || pending.State != MattePendingIdle {
		t.Fatalf("plain preview with the edge computed: %v, want idle", err)
	}
	maskDir := store.MatteDerivedDir(edgeDir, maskPromptName(0, 32, 32))
	maskPNG, err := os.ReadFile(filepath.Join(maskDir, matte.FrameFile(1)))
	if err != nil {
		t.Fatalf("the mask prompt was not derived under the edge memo: %v", err)
	}
	if _, w, h := pngPix(t, maskPNG); w != 32 || h != 32 {
		t.Fatalf("mask prompt is %dx%d, want the tracking size 32x32", w, h)
	}
	if v := grayAt(t, maskPNG, 16, 16); !near8(v, distinctMatte(0), 1) || !allGray(t, maskPNG, v) {
		t.Fatalf("mask prompt centre %d, want the edge matte of frame 0 (%d), flat", v, distinctMatte(0))
	}
	digest := matte.MaskDigestOf(maskPNG)
	if tracks, _ := e.f.trackStats(); tracks != 0 {
		t.Fatalf("%d tracks before any render", tracks)
	}
	posts0, _ := e.f.stats()

	// 3. The render: the edge memo is hit, ONE track with the mask record.
	got, err := e.m.resolveMattes(e.ctx, clip, masked, out, matteModeRender)
	if err != nil {
		t.Fatalf("masked track pass: %v", err)
	}
	rm := got[0]
	if tracks, _ := e.f.trackStats(); tracks != 1 {
		t.Fatalf("%d tracks, want 1", tracks)
	}
	if posts, _ := e.f.stats(); posts != posts0 {
		t.Errorf("the edge pass ran again (%d → %d POSTs)", posts0, posts)
	}
	if got := e.f.lastTrackMask(); got != digest {
		t.Errorf("the track carried mask %q, want the derived file's %q", got, digest)
	}
	hdr := e.f.lastTrackHeader()
	if !strings.Contains(hdr, `"mask":{"frame":0}`) || !strings.Contains(hdr, `{"frame":0,"points":[],"box":null}`) {
		t.Errorf("prompts header = %s", hdr)
	}
	e.f.mu.Lock()
	bodyLen := len(e.f.trackBodies[0])
	e.f.mu.Unlock()
	if bodyLen != 20*32*32*3 {
		t.Errorf("the frames of the track are %d bytes, want 20 x 32x32 rgb24", bodyLen)
	}
	wantCanon := "obj=1|f=0:box=-:pts=:mask=m" + digest
	if rm.Manifest == nil || rm.Manifest.Prompts != wantCanon {
		t.Errorf("manifest prompts = %q, want %q", rm.Manifest.Prompts, wantCanon)
	}
	if rm.id.Prompts != wantCanon {
		t.Errorf("identity prompts = %q, want %q", rm.id.Prompts, wantCanon)
	}
	// The key names the mask: the same identity with the bare marker (no
	// digest) keys elsewhere.
	bare := rm.id
	bare.Prompts = trackPromptsOf([]recipe.MattePrompt{{Frame: 0, MaskFrom: recipe.MattePromptMaskEdge}}).Canonical()
	if bare.Prompts != "obj=1|f=0:box=-:pts=:mask=m" || matteClipKey(clip, masked, "10", bare) == rm.ClipKey {
		t.Errorf("the mask digest is not in the clip key (bare %q)", bare.Prompts)
	}
	// The masks: the fake echoes the mask thresholded — frame 0's edge
	// matte (151) is subject everywhere.
	for _, n := range []int{1, 10, 20} {
		data, err := os.ReadFile(filepath.Join(rm.Dir, matte.FrameFile(n)))
		if err != nil {
			t.Fatalf("mask %d: %v", n, err)
		}
		if !allGray(t, data, 255) {
			t.Errorf("mask %d is not all 255 (the echoed mask prompt)", n)
		}
	}
	// The gate reuses the edge resolution the mask came from.
	if rm.edge == nil || rm.edge.Dir != edgeDir || rm.Edge != recipe.MatteModelBiRefNetLite {
		t.Fatalf("edge of the masked resolution = %+v (want %s)", rm.edge, edgeDir)
	}
	gated := store.MatteDerivedDir(rm.Dir, store.MatteGatedName(recipe.MatteModelBiRefNetLite, rm.edge.ClipKey, matteGateRadius))
	if rm.SeqDir != gated || countSequence(gated) != 20 {
		t.Errorf("SeqDir = %q (%d frames), want the gate %q", rm.SeqDir, countSequence(rm.SeqDir), gated)
	}
	if res := rm.identity().resolved(); res.Tracker != trackerWeights || res.Edge != recipe.MatteModelBiRefNetLite || res.EdgeWeights != otherWeights {
		t.Errorf("resolved identity = %+v", res)
	}

	// 4. A plain preview hits; stabilise nests under the gate.
	if again, err := e.m.resolveMattes(e.ctx, clip, masked, out, matteModePreview); err != nil || again[0].ClipKey != rm.ClipKey {
		t.Fatalf("hit: %v", err)
	}
	stab := []recipe.Op{maskedOp(0, `,"stabilise":"light"`)}
	if s, err := e.m.resolveMattes(e.ctx, clip, stab, out, matteModePreview); err != nil || s[0].SeqDir != store.MatteDerivedDir(gated, "stab-light") || countSequence(s[0].SeqDir) != 20 {
		t.Errorf("gate + stabilise: %v, %+v", err, s)
	}
	if tracks, _ := e.f.trackStats(); tracks != 1 {
		t.Errorf("%d tracks after the hits, want 1", tracks)
	}

	// 5. The info line notes the mask prompt.
	rep := &discordlint.Report{}
	applyMatteInfo(rep, got)
	if d := matteDetailOf(rep); !strings.Contains(d, "guided (sam2-tiny) + edge birefnet-lite") || !strings.Contains(d, "1 prompted frame (mask from the edge matte)") {
		t.Errorf("render.matte detail = %q", d)
	}

	// 6. A mask from another frame: another digest, another key, another
	// track; frame 19's edge matte (109) is background everywhere.
	masked19 := []recipe.Op{maskedOp(19, "")}
	got19, err := e.m.resolveMattes(e.ctx, clip, masked19, out, matteModeRender)
	if err != nil {
		t.Fatalf("masked track pass from frame 19: %v", err)
	}
	mask19, err := os.ReadFile(filepath.Join(store.MatteDerivedDir(edgeDir, maskPromptName(19, 32, 32)), matte.FrameFile(1)))
	if err != nil {
		t.Fatalf("mask prompt of frame 19: %v", err)
	}
	if !near8(grayAt(t, mask19, 16, 16), distinctMatte(19), 1) {
		t.Errorf("mask prompt of frame 19 = %d, want %d", grayAt(t, mask19, 16, 16), distinctMatte(19))
	}
	if tracks, _ := e.f.trackStats(); tracks != 2 || e.f.lastTrackMask() != matte.MaskDigestOf(mask19) || got19[0].ClipKey == rm.ClipKey {
		t.Errorf("frame 19: %d tracks, mask %q (want %q), key equal %v", tracks, e.f.lastTrackMask(), matte.MaskDigestOf(mask19), got19[0].ClipKey == rm.ClipKey)
	}
	if data, _ := os.ReadFile(filepath.Join(got19[0].Dir, matte.FrameFile(1))); !allGray(t, data, 0) {
		t.Error("the mask of frame 19 (background everywhere) did not echo as all 0")
	}

	// 7. Clicks on the mask frame refine it: yet another key; the header
	// lists the points and the mask.
	refined := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":0,"maskFrom":"edge","points":[[0.5,0.5,0]]}]}`)}
	gotR, err := e.m.resolveMattes(e.ctx, clip, refined, out, matteModeRender)
	if err != nil {
		t.Fatalf("refined mask prompt: %v", err)
	}
	if tracks, _ := e.f.trackStats(); tracks != 3 || gotR[0].ClipKey == rm.ClipKey || e.f.lastTrackMask() != digest {
		t.Errorf("refined: %d tracks, key equal %v, mask %q", tracks, gotR[0].ClipKey == rm.ClipKey, e.f.lastTrackMask())
	}
	if hdr := e.f.lastTrackHeader(); !strings.Contains(hdr, `"points":[[0.5,0.5,0]]`) || !strings.Contains(hdr, `"mask":{"frame":0}`) {
		t.Errorf("refined header = %s", hdr)
	}
	if want := "obj=1|f=0:box=-:pts=0.5000,0.5000,0:mask=m" + digest; gotR[0].Manifest.Prompts != want {
		t.Errorf("refined manifest prompts = %q, want %q", gotR[0].Manifest.Prompts, want)
	}

	// 8. Refusals before any pass: edge "none" with a mask prompt; a mask
	// prompt past the clip.
	tracksBefore, _ := e.f.trackStats()
	if _, err := e.m.resolveMattes(e.ctx, clip, []recipe.Op{maskedOp(0, `,"edge":"none"`)}, out, matteModeRender); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), `edge is "none"`) {
		t.Errorf("edge none with a mask prompt: %v", err)
	}
	if _, err := e.m.fillMatteResolved([]recipe.Op{maskedOp(0, `,"edge":"none"`)}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("fillMatteResolved of edge none with a mask prompt: %v", err)
	}
	if _, err := e.m.resolveMattes(e.ctx, clip, []recipe.Op{maskedOp(25, "")}, out, matteModeRender); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "20 frames") {
		t.Errorf("a mask prompt past the clip: %v", err)
	}
	if tracks, _ := e.f.trackStats(); tracks != tracksBefore {
		t.Errorf("a refused op tracked (%d → %d)", tracksBefore, tracks)
	}
	// The slot clamp of the mask: frame 25 of a 20-frame memo is the held
	// last frame (19) — what clampPromptFrames moves such a prompt to.
	if png, d, err := e.m.maskPromptOf(e.ctx, &edge[0], 25, 32, 32); err != nil || d != matte.MaskDigestOf(mask19) || !bytes.Equal(png, mask19) {
		t.Errorf("mask of frame 25: %v, digest %q (want frame 19's %q)", err, d, matte.MaskDigestOf(mask19))
	}

	// 9. A fresh clip, an EAGER preview (the Compute button): the chain
	// starts on its own — the edge pass, the mask, the track — and the
	// preview ends up served.
	held := e.clipHeld()
	var served []resolvedMatte
	for i := 0; i < 40; i++ {
		served, err = e.m.resolveMattes(WithMatteEager(e.ctx, true), held, masked, out, matteModePreview)
		if err == nil {
			break
		}
		if !errors.As(err, &pending) || pending.State == MattePendingIdle {
			t.Fatalf("eager preview on a fresh clip: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || len(served) != 1 || served[0].edge == nil || served[0].Manifest == nil || !strings.Contains(served[0].Manifest.Prompts, ":mask=m") {
		t.Fatalf("eager preview never served: %v, %+v", err, served)
	}
	if posts, _ := e.f.stats(); posts <= posts0 {
		t.Error("the eager preview did not run the edge pass")
	}
}

// TestMattePromptMaskFrom: the live overlay with a mask prompt — idle with
// a reason (no pass, no warm) while the edge memo is missing; after the
// edge pass a mask whose record the fake received (digest of the derived
// file), memoised by that digest (a client-sent digest is ignored);
// clicks on the frame key separately; the prompt must be on the frame
// asked for; edge "none" is refused.
func TestMattePromptMaskFrom(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{})
	clip := e.clipDistinct()
	out := recipe.Output{Format: "gif", FPS: 10}
	ops := []recipe.Op{maskedOp(3, "")}
	prompts := matte.TrackPrompts{Obj: 1, Prompts: []matte.FramePrompt{{Frame: 3, Mask: true}}}
	srcs := []string{clip.Hash}

	// Before the edge pass: idle with a reason; nothing sent.
	var pending *ErrMattePending
	_, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 3, prompts)
	if !errors.As(err, &pending) || pending.State != MattePendingIdle || !strings.Contains(pending.Reason, "compute the General matte first") || pending.Device != matte.DeviceCUDA {
		t.Fatalf("mask prompt before the edge pass: %v (%+v)", err, pending)
	}
	if !strings.Contains(err.Error(), "compute the General matte first") {
		t.Errorf("idle error text = %q", err)
	}
	e.f.mu.Lock()
	warms, frames := len(e.f.warms), e.f.trackFrames
	e.f.mu.Unlock()
	if posts, _ := e.f.stats(); posts != 0 || warms != 0 || frames != 0 {
		t.Fatalf("the overlay started something: %d POSTs, %d warms, %d track/frame calls", posts, warms, frames)
	}

	// The edge pass (Compute with General).
	edge, err := e.m.resolveMattes(e.ctx, clip, []recipe.Op{matteOp(`{"model":"birefnet-lite"}`)}, out, matteModeRender)
	if err != nil {
		t.Fatalf("edge pass: %v", err)
	}
	png, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 3, prompts)
	if err != nil {
		t.Fatalf("mask prompt after the edge pass: %v", err)
	}
	if _, w, h := pngPix(t, png); w != 32 || h != 32 {
		t.Errorf("mask is %dx%d, want 32x32", w, h)
	}
	if !allGray(t, png, 255) { // frame 3's edge matte (145) is subject everywhere; the fake echoes it thresholded
		t.Error("the overlay mask is not the echoed mask prompt (all 255)")
	}
	maskPNG, err := os.ReadFile(filepath.Join(store.MatteDerivedDir(edge[0].Dir, maskPromptName(3, 32, 32)), matte.FrameFile(1)))
	if err != nil {
		t.Fatalf("the mask prompt was not derived under the edge memo: %v", err)
	}
	digest := matte.MaskDigestOf(maskPNG)
	if _, fr := e.f.trackStats(); fr != 1 || e.f.lastTrackMask() != digest {
		t.Errorf("%d track/frame calls, mask %q (want %q)", fr, e.f.lastTrackMask(), digest)
	}
	if hdr := e.f.lastTrackHeader(); !strings.Contains(hdr, `"mask":{"frame":3}`) || !strings.Contains(hdr, `{"frame":3,"points":[],"box":null}`) {
		t.Errorf("header = %s", hdr)
	}
	// A hit — and a client-sent digest changes nothing (the bytes decide).
	claimed := matte.TrackPrompts{Obj: 1, Prompts: []matte.FramePrompt{{Frame: 3, Mask: true, MaskDigest: "bogusbogusbo"}}}
	for _, p := range []matte.TrackPrompts{prompts, claimed} {
		if again, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 3, p); err != nil || !bytes.Equal(again, png) {
			t.Errorf("second ask (%+v): %v, same %v", p.Prompts[0], err, bytes.Equal(again, png))
		}
	}
	if _, fr := e.f.trackStats(); fr != 1 {
		t.Errorf("%d track/frame calls after the hits, want 1", fr)
	}
	// Clicks on the frame refine: another call, points and mask on the wire.
	refined := matte.TrackPrompts{Obj: 1, Prompts: []matte.FramePrompt{{Frame: 3, Mask: true, Points: [][3]float64{{0.5, 0.5, 0}}}}}
	if m2, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 3, refined); err != nil || !allGray(t, m2, 255) {
		t.Errorf("refined: %v", err)
	}
	if _, fr := e.f.trackStats(); fr != 2 || e.f.lastTrackMask() != digest || !strings.Contains(e.f.lastTrackHeader(), `"points":[[0.5,0.5,0]]`) {
		t.Errorf("refined: %d calls, mask %q, header %s", fr, e.f.lastTrackMask(), e.f.lastTrackHeader())
	}
	// Another frame: its own mask (frame 19's edge matte is background).
	if m19, err := e.m.MattePromptMask(e.ctx, srcs, []recipe.Op{maskedOp(19, "")}, out, 19, matte.TrackPrompts{Obj: 1, Prompts: []matte.FramePrompt{{Frame: 19, Mask: true}}}); err != nil || !allGray(t, m19, 0) {
		t.Errorf("frame 19: %v", err)
	}
	// Refusals.
	calls, _ := e.f.trackStats()
	if _, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 4, prompts); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "frame 3") {
		t.Errorf("mask prompt on another frame than asked: %v", err)
	}
	if _, err := e.m.MattePromptMask(e.ctx, srcs, []recipe.Op{maskedOp(3, `,"edge":"none"`)}, out, 3, prompts); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), `edge is "none"`) {
		t.Errorf("edge none: %v", err)
	}
	if _, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 25, matte.TrackPrompts{Obj: 1, Prompts: []matte.FramePrompt{{Frame: 25, Mask: true}}}); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "20 frames") {
		t.Errorf("frame past the clip: %v", err)
	}
	if after, _ := e.f.trackStats(); after != calls {
		t.Errorf("a refused ask called the sidecar (%d → %d)", calls, after)
	}
	// The overlay without a mask prompt is untouched: a box on the frame
	// sends no record.
	box := matte.TrackPrompts{Obj: 1, Prompts: []matte.FramePrompt{{Frame: 3, Box: &[4]float64{0.25, 0.25, 0.75, 0.75}}}}
	if _, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 3, box); err != nil || e.f.lastTrackMask() != "" || strings.Contains(e.f.lastTrackHeader(), `"mask"`) {
		t.Errorf("box prompt: %v, mask %q, header %s", err, e.f.lastTrackMask(), e.f.lastTrackHeader())
	}
}

// TestMaskPromptHelpers: the pure helpers of matte_5d.go and the mask on
// clampPromptFrames.
func TestMaskPromptHelpers(t *testing.T) {
	ps := []recipe.MattePrompt{{Frame: 2, Box: &[4]float64{0, 0, 1, 1}}, {Frame: 7, MaskFrom: recipe.MattePromptMaskEdge}}
	if f, ok := maskPromptFrame(ps); !ok || f != 7 {
		t.Errorf("maskPromptFrame = %d, %v", f, ok)
	}
	if _, ok := maskPromptFrame(ps[:1]); ok {
		t.Error("a box prompt counts as a mask prompt")
	}
	tp := trackPromptsOf(ps)
	if tp.Prompts[1].Mask != true || tp.Prompts[0].Mask || tp.Prompts[1].MaskDigest != "" {
		t.Errorf("trackPromptsOf = %+v", tp.Prompts)
	}
	if got := tp.Canonical(); got != "obj=1|f=2:box=0.0000,0.0000,1.0000,1.0000:pts=|f=7:box=-:pts=:mask=m" {
		t.Errorf("bare canonical = %q", got)
	}
	filled := withMaskDigest(tp, "0123456789ab")
	if filled.Prompts[1].MaskDigest != "0123456789ab" || filled.Prompts[0].MaskDigest != "" || tp.Prompts[1].MaskDigest != "" {
		t.Errorf("withMaskDigest = %+v (input %+v)", filled.Prompts, tp.Prompts)
	}
	if got := filled.Canonical(); !strings.HasSuffix(got, ":mask=m0123456789ab") {
		t.Errorf("filled canonical = %q", got)
	}
	if maskDigestOf(nil) != "" || maskDigestOf([]byte("x")) != matte.MaskDigestOf([]byte("x")) {
		t.Error("maskDigestOf")
	}
	if maskPromptName(3, 1024, 576) != "mask-f3-1024x576" {
		t.Errorf("maskPromptName = %q", maskPromptName(3, 1024, 576))
	}
	if matteShortLabel(recipe.MatteModelBiRefNetLite) != "General" || matteShortLabel(recipe.MatteModelISNetAnime) != "Anime" || matteShortLabel("x") != "x" {
		t.Error("matteShortLabel")
	}
	if maskPromptNote("obj=1|f=0:box=-:pts=:mask=mabc") != " (mask from the edge matte)" || maskPromptNote("obj=1|f=0:box=-:pts=") != "" {
		t.Error("maskPromptNote")
	}
	if got := (&ErrMattePending{State: MattePendingIdle, Reason: "compute the General matte first"}).Error(); got != "jobs: AI matte not computed — compute the General matte first" {
		t.Errorf("idle with a reason = %q", got)
	}
	// clampPromptFrames carries the mask (and its digest) onto the held
	// last frame, merging with that frame's own prompts.
	phantom := withMaskDigest(trackPromptsOf([]recipe.MattePrompt{{Frame: 20, MaskFrom: recipe.MattePromptMaskEdge}, {Frame: 19, Points: [][3]float64{{0.2, 0.2, 1}}}}), "deadbeefcafe")
	clamped, moved := clampPromptFrames(phantom, 20)
	if moved != 1 || len(clamped.Prompts) != 1 || clamped.Prompts[0].Frame != 19 || !clamped.Prompts[0].Mask || clamped.Prompts[0].MaskDigest != "deadbeefcafe" || len(clamped.Prompts[0].Points) != 1 {
		t.Errorf("clamp of a phantom mask prompt = %+v (moved %d)", clamped.Prompts, moved)
	}
	alone := withMaskDigest(trackPromptsOf([]recipe.MattePrompt{{Frame: 20, MaskFrom: recipe.MattePromptMaskEdge}}), "deadbeefcafe")
	if c, moved := clampPromptFrames(alone, 20); moved != 1 || len(c.Prompts) != 1 || c.Prompts[0].Frame != 19 || !c.Prompts[0].Mask || c.Prompts[0].MaskDigest != "deadbeefcafe" {
		t.Errorf("clamp of a lone phantom mask prompt = %+v (moved %d)", c.Prompts, moved)
	}
	if err := clamped.Validate(); err != nil {
		t.Errorf("clamped prompts: %v", err)
	}
}

// TestRenderMaskPromptJobViaSidecar: a render job (Submit, the real job
// path) of a guided op prompted with the edge matte of frame 0 — no eager
// mark anywhere: the job runs the edge pass, derives the mask, tracks with
// the mask record and gates; the mask of frame 0 is subject everywhere, so
// the tracker's echo is a full mask and every delivered frame is opaque
// with its own colour; the job's recipe carries the tracker and edge
// identities, the report's line notes the mask, and a plain still then
// hits the memo (no new track, no new POST).
func TestRenderMaskPromptJobViaSidecar(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{Concurrency: 2})
	clip := e.clipDistinct()
	masked := []recipe.Op{maskedOp(0, "")}
	still := recipe.Output{Format: "gif", FPS: 10}

	var pending *ErrMattePending
	if _, err := e.m.StillSources(e.ctx, []string{clip.Hash}, masked, still, 0.35, 0); !errors.As(err, &pending) || pending.State != MattePendingIdle {
		t.Fatalf("plain still before any pass: %v, want idle", err)
	}
	fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: masked, Output: recipe.Output{Format: recipe.FormatFrames, FPS: 10}})
	if tr, _ := e.f.trackStats(); tr != 1 {
		t.Errorf("%d tracks, want 1", tr)
	}
	if posts, fr := e.f.stats(); posts != 3 || fr != 20 {
		t.Errorf("%d POSTs of %d frames; want the edge pass (3 of 20)", posts, fr)
	}
	if got := e.f.lastTrackMask(); got == "" || !strings.Contains(e.f.lastTrackHeader(), `"mask":{"frame":0}`) {
		t.Errorf("the job's track carried mask %q, header %s", got, e.f.lastTrackHeader())
	}
	pix := e.framePixels(fin, 16, 16)
	corner := e.framePixels(fin, 1, 1)
	if len(pix) != 20 {
		t.Fatalf("%d frames, want 20", len(pix))
	}
	for i := range pix {
		rr, gg, bb := distinctColour(i)
		if pix[i][3] != 255 || corner[i][3] != 255 || !near8(pix[i][0], uint8(rr), 1) || !near8(pix[i][1], uint8(gg), 1) || !near8(pix[i][2], uint8(bb), 1) {
			t.Errorf("frame %d = %v / corner %v, want colour (%d,%d,%d) opaque everywhere (a full mask)", i, pix[i], corner[i], rr, gg, bb)
		}
	}
	res := matteResolvedOf(t, fin.Recipe.Ops)
	if res == nil || res.Tracker != trackerWeights || res.Edge != recipe.MatteModelBiRefNetLite || res.EdgeWeights != otherWeights {
		t.Errorf("job recipe Resolved = %+v", res)
	}
	apng := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: masked, Output: recipe.Output{Format: recipe.FormatAPNG, FPS: 10}})
	if rep := apng.Result.Files[0].Report; rep == nil || !strings.Contains(matteDetailOf(rep), "1 prompted frame (mask from the edge matte)") {
		t.Errorf("APNG report: %+v", rep)
	}
	if _, err := e.m.StillSources(e.ctx, []string{clip.Hash}, masked, still, 0.35, 0); err != nil {
		t.Errorf("plain still after the render: %v", err)
	}
	if tr, _ := e.f.trackStats(); tr != 1 {
		t.Errorf("%d tracks after the memo hits, want 1", tr)
	}
	if p, _ := e.f.stats(); p != 3 {
		t.Errorf("%d POSTs after the memo hits, want 3", p)
	}
}

// TestMattePromptMaskWhileEdgePassRuns: the live overlay with a mask
// prompt while the EDGE model's pass for the clip is in flight (the
// user's Compute with General, or a guided Compute / render that runs the
// edge pass first) reports that pass's progress — running, the pass's
// device and total, no reason — never "idle" with "compute the General
// matte first" during the very pass the user started; nothing is sent to
// the tracker meanwhile, and once the pass is done the ask serves the
// mask.
func TestMattePromptMaskWhileEdgePassRuns(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{})
	e.f.set(func(f *fakeSidecar) { f.delay = 400 * time.Millisecond }) // 3 batches ≈ 1.2 s
	clip := e.clipDistinct()
	out := recipe.Output{Format: "gif", FPS: 10}
	ops := []recipe.Op{maskedOp(3, "")}
	prompts := matte.TrackPrompts{Obj: 1, Prompts: []matte.FramePrompt{{Frame: 3, Mask: true}}}
	srcs := []string{clip.Hash}

	done := make(chan error, 1)
	go func() {
		_, err := e.m.resolveMattes(e.ctx, clip, []recipe.Op{matteOp(`{"model":"birefnet-lite"}`)}, out, matteModeRender)
		done <- err
	}()
	// The pass's progress is set before its first POST: once the fake has
	// one, the pass is running (and has ≥ 2 slow batches to go).
	deadline := time.Now().Add(10 * time.Second)
	for {
		if posts, _ := e.f.stats(); posts >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the edge pass never reached the sidecar")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var pending *ErrMattePending
	_, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 3, prompts)
	if !errors.As(err, &pending) || pending.State != MattePendingRunning || pending.Device != matte.DeviceCUDA || pending.Total != 20 || pending.Reason != "" {
		t.Fatalf("mask prompt while the edge pass runs: %v (%+v), want running on cuda, 20 frames, no reason", err, pending)
	}
	if _, fr := e.f.trackStats(); fr != 0 {
		t.Errorf("%d track/frame calls while the edge pass runs, want none", fr)
	}
	if err := <-done; err != nil {
		t.Fatalf("edge pass: %v", err)
	}
	e.f.set(func(f *fakeSidecar) { f.delay = 0 })
	png, err := e.m.MattePromptMask(e.ctx, srcs, ops, out, 3, prompts)
	if err != nil || !allGray(t, png, 255) {
		t.Fatalf("mask prompt after the edge pass: %v", err)
	}
	if _, fr := e.f.trackStats(); fr != 1 {
		t.Errorf("%d track/frame calls after the pass, want 1", fr)
	}
}

// TestMaskPromptRefusesBeforeTheEdgePass: what the TRACK refuses up front
// — another prompt's frame past the clip, the tracker's estimate over the
// server's cap — refuses a mask-prompted request BEFORE the edge model's
// pass (box / point prompts refuse before any pass; a mask prompt must
// not cost a whole General pass first): zero POSTs, zero tracks.
func TestMaskPromptRefusesBeforeTheEdgePass(t *testing.T) {
	e := newMatteRig5c(t, 1, Options{MatteMaxSeconds: 1})
	clip := e.clipDistinct() // 20 frames
	out := recipe.Output{Format: "webp"}
	noPass := func(what string) {
		t.Helper()
		posts, _ := e.f.stats()
		tracks, frames := e.f.trackStats()
		if posts != 0 || tracks != 0 || frames != 0 {
			t.Fatalf("%s: the sidecar saw %d POSTs, %d tracks, %d track/frame calls — the refusal must come before the edge pass", what, posts, tracks, frames)
		}
	}

	// Another prompt's frame past the clip (the mask prompt's own frame is
	// fine).
	past := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":0,"maskFrom":"edge"},{"frame":25,"box":[0.1,0.1,0.5,0.5]}]}`)}
	if _, err := e.m.resolveMattes(e.ctx, clip, past, out, matteModeRender); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "frame 25") || !strings.Contains(err.Error(), "20 frames") {
		t.Fatalf("a box prompt past the clip beside a mask prompt: %v", err)
	}
	noPass("prompt past the clip")

	// The tracker's estimate over the 1 s cap (20 × ~1 s × 1.3) while the
	// edge model's (20 × 3 ms) is under it.
	e.f.set(func(f *fakeSidecar) {
		ms := f.ping.Models[recipe.MatteModelSAM2Tiny]
		d := ms.Devices[matte.DeviceCUDA]
		d.MsPerFrame = map[string]float64{"1024": 1000}
		ms.Devices[matte.DeviceCUDA] = d
		ms.MsPerFrame = map[string]float64{"1024": 1000}
		f.ping.Models[recipe.MatteModelSAM2Tiny] = ms
	})
	writeMatteFacts(t, e.st, &e.f.ping)
	if _, err := e.m.resolveMattes(e.ctx, clip, []recipe.Op{maskedOp(0, "")}, out, matteModeRender); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "EZLG_MATTE_MAX_SECONDS") {
		t.Fatalf("a mask prompt on a clip over the tracker's estimate cap: %v", err)
	}
	noPass("estimate over the cap")
	// An eager preview is refused the same way (never idle, never a pass).
	if _, err := e.m.resolveMattes(WithMatteEager(e.ctx, true), clip, []recipe.Op{maskedOp(0, "")}, out, matteModePreview); !errors.Is(err, ErrInvalidRecipe) {
		t.Fatalf("eager preview over the cap: %v", err)
	}
	noPass("eager preview over the cap")
}
