package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Crop to content (DESIGN.md §4.3, recipe.OpAutoCrop). The detection pass
// runs ffmpeg over the main source through enc.CropDetectPlanArgs on the
// detection plan graph.CompileDetect compiles from the stack's detection
// ops — the source head, unpremultiply, trim/delay/speed/fps and,
// crucially, the keying and feather ops — so the box is found on the
// picture the crop is applied to: a green-screen clip with Background
// removal resolves to its subject, not to the full (opaque) frame the raw
// source shows, and a feathered edge grows the box by its faded ring. The box
// is read off the alpha plane when the plan's frames carry alpha (source
// alpha or keying; alpha at or above the threshold is content), off the
// picture otherwise (non-black borders, cropdetect). The raw detected box
// is memoised per (source, detection ops, threshold); on every read it is
// grown by the op's padding, clamped to the frame and written into the
// op's Resolved field — the padding is arithmetic, so changing it never
// re-detects — and the compiler then applies it like a crop. A clip with
// no content at all (nothing detected, or a non-positive box) resolves to
// the full frame, i.e. no crop.
//
// The detection follows the compiler, not the stack order: graph hoists
// the detection ops (detectionOps) in front of the geometry wherever they
// sit in the stack, so a keying or trim op BEHIND the autocrop still shapes
// the picture the crop is applied to and is read by the detection (and
// keys its memo) exactly as one in front of it. The detection sees the
// source frame, so the autocrop op must come before any op that changes
// the frame geometry (crop, resize, canvas, flip, rotate); a stack that
// violates this is an ErrInvalidRecipe. Ops of other kinds that the crop
// cannot see — reverse, text and image overlays (drawn on the output
// canvas after every crop) — neither reach the detection nor its memo key.

const (
	// autocropDir is the detection memo directory name under Scratch.
	autocropDir = "autocrop"
	// autocropKeyVersion salts the memo key; bump it when the detection
	// changes what it finds for the same inputs, or what the memo holds.
	// 2: detection on the compiled (keyed) picture instead of the raw
	// source. 3: detector lines only, metadata echoes ignored
	// (enc.ParseCropDetect); the memo holds the raw box, without padding.
	autocropKeyVersion = "3"
	// autocropTimeout bounds one detection pass. The pass runs detached
	// from the request that started it (flight.doDetached): a superseded
	// still or the still deadline does not kill it, so it completes for the
	// concurrent waiters and the next request, and this timeout is the only
	// thing that stops it.
	autocropTimeout = 5 * time.Minute
	// MaxAutoCropPadding bounds AutoCropParams.Padding (px per side).
	MaxAutoCropPadding = 1024
)

// cropDetectPrefix replaces ffrun.RunFFmpeg's prefix for the detection run:
// cropdetect reports at -loglevel info, which the render prefix's -loglevel
// error would silence; -nostats keeps the progress line out of stderr.
var cropDetectPrefix = []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "info", "-nostats"}

// geometryOps are the op kinds that change the frame before the autocrop
// could see it.
var geometryOps = map[string]bool{
	recipe.OpCrop: true, recipe.OpResize: true, recipe.OpCanvas: true, recipe.OpFlip: true, recipe.OpRotate: true,
}

// detectionOps are the op kinds that shape the picture the detection reads
// and the crop is applied to: the kinds graph.CompileDetect applies (the
// compiler runs them in front of the geometry wherever they sit in the
// stack, so their position relative to the autocrop does not matter).
// Feather belongs here exactly like keying: its alpha blur changes which
// pixels reach the threshold, growing the box by the soft edge's fade.
// Every other kind is either refused in front of the autocrop (geometryOps)
// or invisible to the crop (reverse only reorders frames; text and overlays
// are drawn on the output canvas after it) and is left out of the detection
// and its memo key.
var detectionOps = map[string]bool{
	recipe.OpDelay: true, recipe.OpUnpremultiply: true,
	recipe.OpTrim: true, recipe.OpSpeed: true, recipe.OpFPS: true,
	recipe.OpChromaKey: true, recipe.OpColorKey: true, recipe.OpFeather: true,
}

// ResolveAutoCrop runs the content-box detection for the main source of r
// (enc.CropDetectPlanArgs over the detection plan of the stack's detection
// ops — trimmed, keyed, feathered —, alpha plane when those frames carry
// alpha) and
// returns the ops with every OpAutoCrop carrying a Resolved
// crop: the detected box grown by Padding on each side and clamped to the
// source frame; a fully transparent/flat clip resolves to the full frame.
// Non-autocrop ops are returned unchanged (the slice itself when there is
// no autocrop op). The raw box is memoised on disk under
// <Scratch>/autocrop/<key>.json for the store's lifetime; the padding is
// applied on read.
func (m *Manager) ResolveAutoCrop(ctx context.Context, srcHash string, ops []recipe.Op) ([]recipe.Op, error) {
	if !hasAutoCrop(ops) {
		return ops, nil
	}
	blobs, err := m.lookupSources([]string{srcHash})
	if err != nil {
		return nil, err
	}
	return m.resolveAutoCrop(ctx, blobs[0], ops)
}

// hasAutoCrop reports whether ops contain an autocrop op.
func hasAutoCrop(ops []recipe.Op) bool {
	return slices.ContainsFunc(ops, func(op recipe.Op) bool { return op.Kind == recipe.OpAutoCrop })
}

// resolveAutoCrop is ResolveAutoCrop for a looked-up main source.
func (m *Manager) resolveAutoCrop(ctx context.Context, src *store.Blob, ops []recipe.Op) ([]recipe.Op, error) {
	idx := -1
	for i, op := range ops {
		if op.Kind != recipe.OpAutoCrop {
			continue
		}
		if idx >= 0 {
			return nil, fmt.Errorf("%w: op %d (autocrop): only one autocrop op is allowed (op %d is one too)", ErrInvalidRecipe, i, idx)
		}
		idx = i
	}
	if idx < 0 {
		return ops, nil
	}
	params, err := decodeAutoCrop(idx, ops[idx])
	if err != nil {
		return nil, err
	}
	pre, err := autocropDetectionOps(ops, idx)
	if err != nil {
		return nil, err
	}
	raw, err := m.detectContentBox(ctx, src, pre, params.Threshold)
	if err != nil {
		return nil, err
	}
	box := padBox(raw, params.Padding, src.Info.Width, src.Info.Height)
	params.Resolved = &box
	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode autocrop params: %w", err)
	}
	out := slices.Clone(ops)
	out[idx] = recipe.Op{Kind: recipe.OpAutoCrop, Params: rawParams}
	return out, nil
}

// decodeAutoCrop decodes and validates an autocrop op's params, applying
// the documented defaults (Threshold 0 = 1) and dropping a client-supplied
// Resolved box.
func decodeAutoCrop(idx int, op recipe.Op) (recipe.AutoCropParams, error) {
	var p recipe.AutoCropParams
	if len(op.Params) > 0 {
		if err := json.Unmarshal(op.Params, &p); err != nil {
			return p, fmt.Errorf("%w: op %d (autocrop): invalid params: %v", ErrInvalidRecipe, idx, err)
		}
	}
	p.Resolved = nil
	if p.Threshold == 0 {
		p.Threshold = 1
	}
	if p.Threshold < 1 || p.Threshold > 255 {
		return p, fmt.Errorf("%w: op %d (autocrop): threshold must be between 1 and 255 (got %d)", ErrInvalidRecipe, idx, p.Threshold)
	}
	if p.Padding < 0 || p.Padding > MaxAutoCropPadding {
		return p, fmt.Errorf("%w: op %d (autocrop): padding must be between 0 and %d px (got %d)", ErrInvalidRecipe, idx, MaxAutoCropPadding, p.Padding)
	}
	return p, nil
}

// autocropDetectionOps returns the ops of the WHOLE stack that shape the
// picture the detection reads (detectionOps: trim, delay, speed, fps,
// unpremultiply, the keying ops and feather, in stack order) and refuses
// geometry ops in front of the autocrop, whose frame the detection could
// not see. Detection ops behind the autocrop count too: the compiler hoists
// them in front of the geometry regardless of where they sit, so [autocrop,
// colorkey] renders the keyed picture and must be cropped by a box found on
// the keyed picture (and [autocrop, feather] by a box found on the
// feathered one). Geometry ops behind the autocrop are fine (the crop is
// applied first). ops is the full stack, idx the autocrop's index.
func autocropDetectionOps(ops []recipe.Op, idx int) ([]recipe.Op, error) {
	var pre []recipe.Op
	for i, op := range ops {
		switch {
		case i < idx && geometryOps[op.Kind]:
			return nil, fmt.Errorf("%w: op %d (autocrop): crop to content must come before op %d (%s), which changes the frame", ErrInvalidRecipe, idx, i, op.Kind)
		case detectionOps[op.Kind]:
			pre = append(pre, op)
		}
	}
	return pre, nil
}

// autocropKey identifies a detection: the source, the canonical detection
// ops of the stack (autocropDetectionOps — so a keyed and an unkeyed stack
// never share a box, wherever the keying op sits) and the threshold. The
// padding is not part of it: it is applied to the memoised raw box on
// read (padBox).
func autocropKey(srcHash string, pre []recipe.Op, threshold int) (string, error) {
	canon, err := recipe.Recipe{Sources: []string{srcHash}, Ops: pre}.Canonical()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	h := sha256.New()
	h.Write([]byte("autocrop|" + autocropKeyVersion + "\n"))
	h.Write(canon)
	h.Write([]byte("\nt=" + strconv.Itoa(threshold)))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// detectContentBox returns the (memoised) raw content box of src for the
// detection ops pre: the detector's union box as found, unpadded and
// unclamped, with W or H <= 0 when nothing was detected (padBox turns both
// into the crop). Concurrent requests for the same key share one detection
// pass (m.autocrop), which runs detached from the request that started it
// (flight.doDetached, bounded by autocropTimeout): a caller whose ctx ends
// returns its ctx error while the pass carries on and writes the memo for
// the next request.
func (m *Manager) detectContentBox(ctx context.Context, src *store.Blob, pre []recipe.Op, threshold int) (recipe.CropParams, error) {
	key, err := autocropKey(src.Hash, pre, threshold)
	if err != nil {
		return recipe.CropParams{}, err
	}
	memo := filepath.Join(m.st.Scratch, autocropDir, key+".json")
	if box, ok := readAutocropMemo(memo, src.Info); ok {
		return box, nil
	}
	return m.autocrop.doDetached(ctx, key, func(ctx context.Context) (recipe.CropParams, error) {
		if box, ok := readAutocropMemo(memo, src.Info); ok {
			return box, nil // a previous leader finished while we waited for the slot
		}
		box, err := m.runCropDetect(ctx, src, pre, threshold)
		if err != nil {
			return box, err
		}
		writeAutocropMemo(memo, box)
		return box, nil
	})
}

// runCropDetect runs the detection pass (bounded by autocropTimeout) and
// returns the raw box (rawBox). Only the caller's own cancellation or
// deadline surfaces as a context error; the internal timeout is reported
// as a plain error so flight never mistakes it for an aborted request and
// retries it — and under the detached ctx detectContentBox hands it, the
// timeout is the only way the pass ends early.
//
// The pass runs the detection plan (graph.CompileDetect: the source head,
// trim/delay/speed/fps and the keying ops, at the source frame size) and
// reads the box off its frames (enc.CropDetectPlanArgs) — the alpha plane
// when the plan reports alpha, which keying sets, so a keyed clip is
// cropped to its subject. The "-i" value handed to enc is src.Path as is:
// the blob file, or the blob directory of an image sequence — enc joins
// plan.InputPattern itself, exactly as for renderMaster, StillArgs and
// ProxyArgs, so the pattern must not be joined here. A still image needs no
// loop wrapper here: the plan has no fps stage for a single frame and enc's
// sampler passes the first frame on its own (verified in enc's
// TestCropDetectPlanReal and in TestCropDetectPlumbing).
func (m *Manager) runCropDetect(ctx context.Context, src *store.Blob, pre []recipe.Op, threshold int) (recipe.CropParams, error) {
	info := src.Info
	if m.tools.FFmpeg == "" {
		return recipe.CropParams{}, errors.New("ffmpeg is not available on this server")
	}
	plan, err := graph.CompileDetect([]recipe.ProbeInfo{*info}, pre)
	if err != nil {
		return recipe.CropParams{}, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	args := enc.CropDetectPlanArgs(src.Path, plan, plan.HasAlpha, threshold)
	if len(args) == 0 {
		return recipe.CropParams{}, errors.New("crop to content is not available in this build")
	}
	argv := append(slices.Clone(cropDetectPrefix), args...)
	tctx, cancel := context.WithTimeout(ctx, autocropTimeout)
	defer cancel()
	_, stderr, err := ffrun.RunCapture(tctx, m.tools.FFmpeg, argv)
	if err != nil {
		if ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded) {
			return recipe.CropParams{}, fmt.Errorf("crop to content: detection did not finish within %s; trim the clip first", autocropTimeout)
		}
		return recipe.CropParams{}, fmt.Errorf("crop to content: detection failed: %w", err)
	}
	w, h, x, y, ok := enc.ParseCropDetect(string(stderr))
	return rawBox(w, h, x, y, ok), nil
}

// rawBox is the detector's box as the memo holds it: the box as found, or
// an empty one (W = H = 0) when nothing was detected — cropdetect prints
// "crop=-62:-46:64:48" for a clip with nothing above the limit, and such a
// non-positive box means "no content" too.
func rawBox(w, h, x, y int, ok bool) recipe.CropParams {
	if !ok || w <= 0 || h <= 0 {
		return recipe.CropParams{}
	}
	return recipe.CropParams{X: x, Y: y, W: w, H: h}
}

// padBox turns a raw box into the resolved crop: grown by pad on every
// side and clamped to the fw x fh frame (contentBox); an empty raw box is
// the full frame.
func padBox(raw recipe.CropParams, pad, fw, fh int) recipe.CropParams {
	return contentBox(raw.W, raw.H, raw.X, raw.Y, true, pad, fw, fh)
}

// contentBox grows a detected box by pad on every side and clamps it to the
// fw x fh frame. No detection (ok false) or a non-positive box — cropdetect
// prints "crop=-62:-46:64:48" for a clip with nothing above the limit — is
// the full frame; so is a box that ends up outside the frame.
func contentBox(w, h, x, y int, ok bool, pad, fw, fh int) recipe.CropParams {
	full := recipe.CropParams{X: 0, Y: 0, W: fw, H: fh}
	if !ok || w <= 0 || h <= 0 {
		return full
	}
	x0, y0 := max(x-pad, 0), max(y-pad, 0)
	x1, y1 := min(x+w+pad, fw), min(y+h+pad, fh)
	if x1 <= x0 || y1 <= y0 {
		return full
	}
	return recipe.CropParams{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// validBox reports whether box is a usable crop of the info's frame.
func validBox(box recipe.CropParams, info *recipe.ProbeInfo) bool {
	return box.W >= 1 && box.H >= 1 && box.X >= 0 && box.Y >= 0 &&
		box.X+box.W <= info.Width && box.Y+box.H <= info.Height
}

// readAutocropMemo loads a memoised raw box; ok is false when there is none
// or a detected box does not fit the source's frame (a stale or corrupt
// entry is ignored and overwritten by the next detection). An empty box
// (nothing detected) is a valid entry.
func readAutocropMemo(path string, info *recipe.ProbeInfo) (recipe.CropParams, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return recipe.CropParams{}, false
	}
	var box recipe.CropParams
	if err := json.Unmarshal(data, &box); err != nil {
		return recipe.CropParams{}, false
	}
	if box.W <= 0 || box.H <= 0 {
		return recipe.CropParams{}, true
	}
	if !validBox(box, info) {
		return recipe.CropParams{}, false
	}
	return box, true
}

// writeAutocropMemo stores box at path atomically (temp file + rename).
// Failures are logged, never fatal: the detection result is still returned.
func writeAutocropMemo(path string, box recipe.CropParams) {
	data, err := json.Marshal(box)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("jobs: autocrop memo dir: %v", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".autocrop-*")
	if err != nil {
		log.Printf("jobs: autocrop memo: %v", err)
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		log.Printf("jobs: autocrop memo write: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		log.Printf("jobs: autocrop memo rename: %v", err)
	}
}
