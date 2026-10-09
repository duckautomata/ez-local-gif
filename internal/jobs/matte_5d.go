package jobs

// Phase 5d (build brief 2026-10-09, "mask-prompted tracking"): a guided
// (tracker) matte op may prompt ONE frame with a MASK instead of (or
// before) clicks — recipe.MattePrompt.MaskFrom = "edge": the mask is the
// EDGE model's per-frame matte of that frame (the model MatteParams.Edge
// names, the device's default per-frame model for ""), the prototype's
// most reliable start ("pick a frame where General got it right and track
// from it", IoU 0.997). No mask bytes live in the recipe; this file is
// what jobs does with such a prompt:
//
//   - resolveMatte (matte.go) runs the TRACK's up-front refusals first
//     (trackRefusals: the body cap, the prompts' frame bound, the frame /
//     estimate caps — a clip the track refuses anyway never costs the
//     edge pass), then resolves the EDGE model's memo (its own pass under
//     the request's policy — a plain preview is idle until the Compute
//     button or a render runs it; it is needed for the gate anyway),
//     takes frame i's matte PNG from it, scales it to the
//     tracking size (enc.MatteMaskScaleArgs, derived once under the edge
//     memo as "<edgeDir>/mask-f<i>-<w>x<h>/000001.png" — maskPromptOf) and
//     only then keys the tracker's memo: the mask's digest
//     (matte.MaskDigestOf) enters the clip key through
//     FramePrompt.MaskDigest and the ":mask=m<digest>" marker of
//     matte.TrackPrompts.Canonical (withMaskDigest), so a tracker memo
//     names the mask that was actually sent and an edge memo re-keyed by
//     a sidecar upgrade re-tracks;
//   - runTrackPass (matte_5c.go) sends the PNG as the request's trailing
//     mask record (matte.Client.Track's mask argument — the client refuses
//     a digest that is not the bytes'); clampPromptFrames carries the mask
//     along when the prompt sits on the plan's phantom last slot;
//   - MattePromptMask (matte_5c.go) — the live overlay — supports a mask
//     prompt (FramePrompt.Mask on the frame asked for) when the edge
//     memo is on disk, reports the edge pass's own progress (running /
//     loading) while one is in flight for the clip, and answers
//     *ErrMattePending State idle with a Reason the SPA can show
//     ("compute the General matte first") when neither — it never starts
//     a pass (promptMaskEdge).
//
// The graph refuses a maskFrom prompt with edge "none" at compile time
// (validateMattePrompts); identityOf refuses it before any pass, and the
// two places here keep a belt.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// maskPromptPrefix names the derived dir of a mask prompt under the EDGE
// memo: "mask-f<frame>-<w>x<h>" (maskPromptName).
const maskPromptPrefix = "mask-f"

// maskPromptFrame reports the OUTPUT frame of the op's mask prompt
// (recipe.MattePrompt.MaskFrom != "") and whether there is one — the graph
// allows at most one per op (validateMattePrompts); the first wins here.
func maskPromptFrame(ps []recipe.MattePrompt) (int, bool) {
	for _, p := range ps {
		if p.MaskFrom != "" {
			return p.Frame, true
		}
	}
	return 0, false
}

// maskDigestOf is matte.MaskDigestOf for an optional mask: "" for nil (no
// mask record), so a prompt set without a mask keys exactly as before.
func maskDigestOf(mask []byte) string {
	if mask == nil {
		return ""
	}
	return matte.MaskDigestOf(mask)
}

// withMaskDigest returns tp with MaskDigest set to digest on every prompt
// that carries the mask (trackPromptsOf marks it from MaskFrom; one per
// set). The receiver is not modified. Callers fill it from the bytes they
// are about to send, before keying (Canonical) and before Track /
// TrackFrame, which refuse a digest that is not the bytes'.
func withMaskDigest(tp matte.TrackPrompts, digest string) matte.TrackPrompts {
	out := matte.TrackPrompts{Obj: tp.Obj, Prompts: make([]matte.FramePrompt, len(tp.Prompts))}
	for i, p := range tp.Prompts {
		if p.Mask {
			p.MaskDigest = digest
		}
		out.Prompts[i] = p
	}
	return out
}

// maskPromptName is the derived dir under the edge memo that holds the
// mask prompt of output frame frame (0-based) at the tracking size w x h:
// "mask-f<frame>-<w>x<h>", one 000001.png inside (store.MatteDerivedDir
// sanitises it like every derived name). The tracking size is in the name
// because the same edge memo serves every trim / rate of the clip that
// keys to it, and a source scaled to another tracking size needs its own
// mask.
func maskPromptName(frame, w, h int) string {
	return maskPromptPrefix + strconv.Itoa(frame) + "-" + strconv.Itoa(w) + "x" + strconv.Itoa(h)
}

// maskPromptOf returns the mask prompt for output frame frame of the clip:
// the edge memo's matte of that frame (its FrameFile(slot+1), 8-bit gray at
// the model square) scaled to w x h — the tracking size — through
// enc.MatteMaskScaleArgs, derived ONCE under the edge memo
// ("<edge.Dir>/mask-f<slot>-<w>x<h>/000001.png": deriveSequence's Protect,
// dedupe, tmp + rename and count check, so the bytes — and the digest the
// tracker's key carries — are stable for the memo's lifetime), then read.
// slot is frame clamped to the memo's last frame (edge.Manifest.Frames−1):
// a prompt on the plan's phantom last slot is a prompt on the held last
// frame (clampPromptFrames moves it there for the track). The edge dir
// stays protected from the derive through the read. Returns the PNG and
// its matte.MaskDigestOf.
func (m *Manager) maskPromptOf(ctx context.Context, edge *resolvedMatte, frame, w, h int) (png []byte, digest string, err error) {
	if edge == nil || edge.Manifest == nil || edge.Manifest.Frames < 1 {
		return nil, "", fmt.Errorf("AI matte: the mask prompt's edge matte has no frames")
	}
	if frame < 0 {
		return nil, "", fmt.Errorf("%w: the mask prompt's frame must be >= 0 (got %d)", ErrInvalidRecipe, frame)
	}
	if w < 1 || h < 1 {
		return nil, "", fmt.Errorf("%w: the source has no frame size", ErrInvalidRecipe)
	}
	slot := min(frame, edge.Manifest.Frames-1)
	release := m.st.Protect(edge.Dir)
	defer release()
	src := filepath.Join(edge.Dir, matte.FrameFile(slot+1))
	dir, err := m.deriveSequence(ctx, edge.Dir, maskPromptName(slot, w, h), 1, nil, func(tmp string) []string {
		return enc.MatteMaskScaleArgs(src, tmp, w, h)
	})
	if err != nil {
		return nil, "", err
	}
	png, err = os.ReadFile(filepath.Join(dir, matte.FrameFile(1)))
	if err != nil {
		return nil, "", fmt.Errorf("AI matte: reading the mask prompt of frame %d: %w", slot, err)
	}
	if len(png) == 0 {
		return nil, "", fmt.Errorf("AI matte: the mask prompt of frame %d is empty — report this with the source", slot)
	}
	return png, matte.MaskDigestOf(png), nil
}

// resolveMaskPrompt is resolveMatte's Phase 5d step for a tracker request
// whose prompts (prompts, the op's) carry a mask prompt on output frame
// frame: the EDGE model's own resolution on the same clip (its memo, or
// its pass under the request's policy — the same call the gate makes
// later, done first because the tracker's key needs the mask) and the
// mask prompt derived from it (maskPromptOf). A mask prompt without an
// edge model (edge "none") or on a frame past the clip is an
// ErrInvalidRecipe before any pass, and so is anything the TRACK would
// refuse up front (trackRefusals: the body cap, another prompt's frame,
// the frame / estimate caps) — a clip the track refuses anyway must not
// cost the edge model's whole per-frame pass first (matteMiss repeats the
// refusals on the tracker's miss, as for every tracker request). Returns
// the edge's resolution (for the gate) and the mask PNG.
func (m *Manager) resolveMaskPrompt(ctx context.Context, src *store.Blob, ops []recipe.Op, plan *graph.Plan, fps string, id matteIdentity, frame int, prompts []recipe.MattePrompt, facts *matte.Facts, mode matteMode) (*resolvedMatte, []byte, error) {
	if id.Edge == nil {
		return nil, nil, errMaskPromptNoEdge()
	}
	if plan.Frames > 0 && frame >= plan.Frames {
		return nil, nil, fmt.Errorf("%w: the mask prompt is on frame %d but the clip has %d frames (0..%d) at this trim and rate — prompt a frame inside it", ErrInvalidRecipe, frame, plan.Frames, plan.Frames-1)
	}
	if err := m.trackRefusals(plan.Frames, id, prompts); err != nil {
		return nil, nil, err
	}
	edge, err := m.resolveMatte(ctx, src, ops, plan, fps, matteRequest{Model: id.Edge.Model}, facts, mode)
	if err != nil {
		return nil, nil, err
	}
	png, _, err := m.maskPromptOf(ctx, &edge, frame, id.TrackW, id.TrackH)
	if err != nil {
		return nil, nil, err
	}
	return &edge, png, nil
}

// errMaskPromptNoEdge is the refusal of a maskFrom "edge" prompt on an op
// whose edge is "none": no edge model, no matte to take the mask from
// (the graph's validateMattePrompts says the same at compile time).
func errMaskPromptNoEdge() error {
	return fmt.Errorf("%w: a maskFrom %q prompt needs an edge model, but edge is %q (the mask is the edge model's matte of the frame) — pick an edge model or draw a box", ErrInvalidRecipe, recipe.MattePromptMaskEdge, recipe.MatteEdgeNone)
}

// promptMaskEdge is MattePromptMask's edge resolution for a mask prompt:
// the edge model of the op (edgeReq as written: "" = the device's default
// per-frame model; "none" is refused) and its memo for the clip at the
// plan rate fps — a hit on disk, served from the facts alone; while that
// model's pass for the clip is IN FLIGHT (the Compute button or a render
// started it — a guided Compute with a mask prompt runs the edge pass
// first) the pass's own progress as *ErrMattePending (running / loading,
// what mattePendingFor reports to a preview: the overlay shows the pill
// and retries, never "switch the Model and press Compute" during the very
// pass the user started); otherwise *ErrMattePending State idle with a
// Reason for the SPA ("compute the General matte first"): the live
// overlay NEVER starts a pass (the Compute button does, with the
// per-frame model selected). The resolution returned carries the memo's
// dir and manifest (what maskPromptOf reads).
func (m *Manager) promptMaskEdge(src *store.Blob, ops []recipe.Op, fps, edgeReq string, facts *matte.Facts, device string) (*resolvedMatte, error) {
	eid, err := edgeIdentityFor(facts, device, edgeReq)
	if err != nil {
		return nil, err
	}
	if eid == nil {
		return nil, errMaskPromptNoEdge()
	}
	key := matteClipKey(src, ops, fps, *eid)
	man, ok := m.matteMemoHit(key, fps)
	if !ok {
		if p, running := m.matteProgressFor(key); running {
			e := p.pending()
			if e.Device == "" {
				e.Device = device
			}
			return nil, &e
		}
		return nil, &ErrMattePending{
			State: MattePendingIdle, Device: device,
			Reason: fmt.Sprintf("compute the %s matte first (the mask prompt is its matte of this frame)", matteShortLabel(eid.Model)),
		}
	}
	return &resolvedMatte{
		Model: eid.Model, Size: eid.Size, Precision: eid.Precision, Dir: m.st.MatteDir(key),
		Manifest: man, ClipKey: key, Device: device, id: *eid,
	}, nil
}

// matteShortLabel is matteModelLabel without its parenthetical ("General
// (precise)" → "General"), for a sentence.
func matteShortLabel(id string) string {
	label := matteModelLabel(id)
	if i := strings.Index(label, " ("); i > 0 {
		return label[:i]
	}
	return label
}

// maskPromptNote is the render.matte info line's note on a tracker memo
// whose canonical prompts carry a mask marker (matte.TrackPrompts.Canonical:
// ":mask=m<digest>").
func maskPromptNote(canonical string) string {
	if strings.Contains(canonical, ":mask=m") {
		return " (mask from the edge matte)"
	}
	return ""
}
