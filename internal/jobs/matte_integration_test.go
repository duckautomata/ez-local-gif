package jobs

// Phase 5b: the matte op's integration into the pipeline (render pre-stage,
// compile, autocrop, stills, proxies, Submit) — against memos written on
// disk under the persisted sidecar facts, without any sidecar. The unit
// tests need no ffmpeg; the pixel tests (TestRenderMatte*, TestStillMatte*,
// TestProxyMatteReversed, TestAutoCropMatteSubject) skip without one on
// PATH. A memo of flat gray mattes — matte i is the value matteGray(i) —
// lets every frame's alpha name the matte it was paired with.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

const (
	testWeights  = "f15622d853e8260172812b657053460e20806f04b9e05147d49af7bed31a6e99"
	otherWeights = "5600024376f572a557870a5eb0afb1e5961636bef4e1e22132025467d0f03333"
)

// matteOp builds a matte op with the given params JSON ("" = none).
func matteOp(params string) recipe.Op {
	if params == "" {
		return recipe.Op{Kind: recipe.OpMatte}
	}
	return recipe.Op{Kind: recipe.OpMatte, Params: json.RawMessage(params)}
}

// testPing is a sidecar ping offering isnet-anime at 64 / 32 px (fp32, CPU)
// with the given weights digest.
func testPing(weights string) *matte.Ping {
	return &matte.Ping{
		Protocol: matte.Protocol, Version: "test", Instance: "abc", ProcessingVersion: "1",
		Device: matte.DeviceCPU, DefaultModel: recipe.MatteModelISNetAnime,
		Models: map[string]matte.ModelState{
			recipe.MatteModelISNetAnime: {
				State: matte.StateReady, Weights: weights, GraphDigest: weights, Precision: "fp32",
				Sizes: []int{64, 32}, DefaultSize: 64, MsPerFrame: map[string]float64{"64": 1, "32": 1}, Label: "Anime (fast)",
			},
		},
	}
}

// writeMatteFacts persists p as the store's facts file — BEFORE NewManager,
// which loads it once (initMatte).
func writeMatteFacts(t *testing.T, st *store.Store, p *matte.Ping) {
	t.Helper()
	path := filepath.Join(filepath.Dir(st.MatteDir("facts")), matte.FactsName)
	if err := matte.SaveFacts(path, p); err != nil {
		t.Fatal(err)
	}
}

// matteGray is the flat value of matte i (0-based).
func matteGray(i int) uint8 { return uint8(40 + 5*i) }

// matteKeyFor computes the clip key the resolver files src's matte under for
// ops at the rate the plan for out runs at — the same inputs resolveMatte
// uses (the detection plan's MatteInput.FPS text, the facts' identity) — and
// returns it with that fps text and the matte plan's frame count.
func matteKeyFor(t *testing.T, src *store.Blob, ops []recipe.Op, out recipe.Output, p *matte.Ping, model string, reqSize int) (key, fps string, frames int) {
	t.Helper()
	det, err := graph.CompileDetectFor([]recipe.ProbeInfo{*src.Info}, ops, out)
	if err != nil {
		t.Fatalf("CompileDetectFor: %v", err)
	}
	for _, in := range det.ExtraInputs {
		if in.Matte != nil && in.Matte.Model == model && in.Matte.Size == reqSize {
			fps = in.Matte.FPS
		}
	}
	if fps == "" {
		t.Fatalf("no matte input for %s/%d in the detection plan: %+v", model, reqSize, det.ExtraInputs)
	}
	mplan, err := graph.CompileMatteInput([]recipe.ProbeInfo{*src.Info}, ops, out)
	if err != nil {
		t.Fatalf("CompileMatteInput: %v", err)
	}
	if got := graph.FPSText(mplan.FPS); got != fps {
		t.Fatalf("matte plan fps text %q, detection input %q", got, fps)
	}
	ms := p.Models[model]
	key = matte.ClipKey(matte.ClipKeyParts{
		Src: src.Hash, Temporal: matte.TemporalOps(ops), Probe: *src.Info, InfoVersion: store.InfoVersion,
		FPS: fps, Model: model, Size: ms.EffectiveSize(reqSize), Precision: ms.Precision, Weights: ms.Weights, Proc: p.ProcessingVersion,
	})
	return key, fps, mplan.Frames
}

// writeMatteMemo writes a complete memo for key: n w x h gray PNGs whose
// pixel (x, y) of frame i is gray(i, x, y), and the manifest. It returns
// the memo dir.
func writeMatteMemo(t *testing.T, st *store.Store, key, srcHash string, p *matte.Ping, model, fps string, n, w, h int, gray func(i, x, y int) uint8) string {
	t.Helper()
	dir := st.MatteDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		img := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.Pix[y*img.Stride+x] = gray(i, x, y)
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, matte.FrameFile(i+1)), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ms := p.Models[model]
	man := &matte.Manifest{
		Key: key, Src: srcHash, Model: model, Weights: ms.Weights, Proc: p.ProcessingVersion, GraphDigest: ms.GraphDigest,
		Precision: ms.Precision, Size: ms.DefaultSize, FPS: fps, Frames: n, Device: p.Device, MsPerFrame: 1,
	}
	if err := matte.WriteManifest(filepath.Join(dir, matte.ManifestName), man); err != nil {
		t.Fatal(err)
	}
	return dir
}

// flatMemo writes a memo of n flat mattes (matteGray(i)) for src's stack.
func flatMemo(t *testing.T, st *store.Store, src *store.Blob, ops []recipe.Op, out recipe.Output, p *matte.Ping, n int) string {
	t.Helper()
	key, fps, _ := matteKeyFor(t, src, ops, out, p, recipe.MatteModelISNetAnime, 0)
	return writeMatteMemo(t, st, key, src.Hash, p, recipe.MatteModelISNetAnime, fps, n, src.Info.Width, src.Info.Height,
		func(i, x, y int) uint8 { return matteGray(i) })
}

// matteResolvedOf returns the Resolved identity of the first matte op.
func matteResolvedOf(t *testing.T, ops []recipe.Op) *recipe.MatteResolved {
	t.Helper()
	for _, op := range ops {
		if op.Kind != recipe.OpMatte {
			continue
		}
		var p recipe.MatteParams
		if err := json.Unmarshal(op.Params, &p); err != nil {
			t.Fatalf("matte params %s: %v", op.Params, err)
		}
		return p.Resolved
	}
	return nil
}

// ---- unit (no ffmpeg) ---------------------------------------------------------

// TestSubmitFillsMatteResolved: Submit hashes the matte identity from the
// persisted facts (never from the client) — other weights, other result
// key; without facts a matte recipe is ErrMatteUnavailable.
func TestSubmitFillsMatteResolved(t *testing.T) {
	st := newTestStore(t)
	src := putSource(t, st, true)
	writeMatteFacts(t, st, testPing(testWeights))
	m := NewManager(st, fakeTools, Options{})
	out := recipe.Output{Format: "gif"}

	j, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{matteOp("")}, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	res := matteResolvedOf(t, j.Recipe.Ops)
	want := recipe.MatteResolved{Weights: testWeights, Proc: "1", Size: 64, Precision: "fp32"}
	if res == nil || *res != want {
		t.Fatalf("job recipe Resolved = %+v, want %+v", res, want)
	}
	bare := ResultKey(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{matteOp("")}, Output: out})
	if j.RecipeHash == bare {
		t.Error("the result key ignores the resolved identity")
	}
	if j.RecipeHash != ResultKey(j.Recipe) {
		t.Error("the job's hash is not the hash of the filled recipe")
	}
	// A client-sent Resolved is stripped: the same job key as without it.
	forged := matteOp(`{"resolved":{"weights":"` + otherWeights + `","proc":"9","size":32,"precision":"fp16"}}`)
	j2, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{forged}, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if j2.RecipeHash != j.RecipeHash {
		t.Errorf("a client-sent Resolved changed the result key: %s vs %s", short(j2.RecipeHash), short(j.RecipeHash))
	}
	// A requested size is honoured and keyed; an unoffered one refused.
	j3, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{matteOp(`{"size":32}`)}, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if r := matteResolvedOf(t, j3.Recipe.Ops); r == nil || r.Size != 32 || j3.RecipeHash == j.RecipeHash {
		t.Errorf("size 32: Resolved %+v, key equal %v", r, j3.RecipeHash == j.RecipeHash)
	}
	if _, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{matteOp(`{"size":77}`)}, Output: out}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unoffered size: %v", err)
	}
	if _, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{matteOp(`{"model":"nope"}`)}, Output: out}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unoffered model: %v", err)
	}
	// Other facts, other identity, other key.
	st2 := newTestStore(t)
	src2 := putSource(t, st2, true)
	writeMatteFacts(t, st2, testPing(otherWeights))
	m2 := NewManager(st2, fakeTools, Options{})
	j4, err := m2.Submit(recipe.Recipe{Sources: []string{src2}, Ops: []recipe.Op{matteOp("")}, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if src2 != src {
		t.Fatal("the fixture source must hash alike in both stores")
	}
	if j4.RecipeHash == j.RecipeHash {
		t.Error("a weights change did not change the result key")
	}
	// No facts: unavailable (the server names the compose profile).
	st3 := newTestStore(t)
	src3 := putSource(t, st3, true)
	m3 := NewManager(st3, fakeTools, Options{})
	if _, err := m3.Submit(recipe.Recipe{Sources: []string{src3}, Ops: []recipe.Op{matteOp("")}, Output: out}); !errors.Is(err, ErrMatteUnavailable) {
		t.Errorf("no facts: %v", err)
	}
	for _, id := range []string{j.ID, j2.ID, j3.ID} {
		waitFinished(t, m, id)
	}
	waitFinished(t, m2, j4.ID)
}

// TestRenderMattePreStageBeforeSlot: a matte recipe resolves its mattes
// before taking a render slot — with every slot held, a recipe whose matte
// cannot be produced fails at once instead of queueing, and a plain recipe
// still queues.
func TestRenderMattePreStageBeforeSlot(t *testing.T) {
	st := newTestStore(t)
	src := putSource(t, st, true)
	writeMatteFacts(t, st, testPing(testWeights))
	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	j, err := m.Submit(recipe.Recipe{Sources: []string{src}, Ops: []recipe.Op{matteOp("")}, Output: recipe.Output{Format: "gif"}})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitFinished(t, m, j.ID)
	if fin.State != StateError || !strings.Contains(fin.Error, "matte") {
		t.Errorf("matte job with the slot held: %+v", fin)
	}
	if fin.Stage != StageMatte {
		t.Errorf("failed at stage %q, want %q", fin.Stage, StageMatte)
	}
	plain, err := m.Submit(recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if got, _ := m.Get(plain.ID); got.State != StateQueued {
		t.Errorf("plain job should queue behind the held slot: %+v", got)
	}
	m.Cancel(plain.ID)
	waitFinished(t, m, plain.ID)
}

// TestDetectionOpsAndKeyWithMatte: the matte op is a detection op; the
// autocrop key carries the detection rate and the resolved clip keys.
func TestDetectionOpsAndKeyWithMatte(t *testing.T) {
	src := strings.Repeat("a", 64)
	ops := []recipe.Op{matteOp(""), autocropOp(`{}`), {Kind: recipe.OpFeather, Params: json.RawMessage(`{"radius":1}`)}}
	pre, err := autocropDetectionOps(ops, 1)
	if err != nil || len(pre) != 2 || pre[0].Kind != recipe.OpMatte || pre[1].Kind != recipe.OpFeather {
		t.Fatalf("detection ops = %+v (%v)", pre, err)
	}
	k, err := autocropKey(src, pre, 1, "25", []string{"k1"})
	if err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]func() (string, error){
		"fps":        func() (string, error) { return autocropKey(src, pre, 1, "30", []string{"k1"}) },
		"matte key":  func() (string, error) { return autocropKey(src, pre, 1, "25", []string{"k2"}) },
		"no matte":   func() (string, error) { return autocropKey(src, pre, 1, "25", nil) },
		"two mattes": func() (string, error) { return autocropKey(src, pre, 1, "25", []string{"k1", "k2"}) },
	} {
		if k2, err := other(); err != nil || k2 == k {
			t.Errorf("key ignores %s (%v)", name, err)
		}
	}
	if autocropKeyVersion != "5" {
		t.Errorf("autocropKeyVersion = %q, want 5 (Phase 5b: detection rate + clip keys in the key)", autocropKeyVersion)
	}
	if !detectionOps[recipe.OpMatte] {
		t.Error("detectionOps lacks the matte op")
	}
}

// TestFillExtraInputsMatte: the compiled plan's matte input is pointed at
// the memo (path, frame count) after the string-exact fps check; overlay
// inputs are filled as before.
func TestFillExtraInputsMatte(t *testing.T) {
	st := newTestStore(t)
	src := putSource(t, st, true)
	blobs, err := NewManager(st, fakeTools, Options{}).lookupSources([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	s := &sources{hashes: []string{src}, blobs: blobs}
	ops := []recipe.Op{matteOp("")}
	plan, err := graph.CompileWithSources(s.infos(), ops, recipe.Output{Format: "gif"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraInputs) != 1 || plan.ExtraInputs[0].Matte == nil || plan.ExtraInputs[0].Source != 0 {
		t.Fatalf("plan inputs = %+v", plan.ExtraInputs)
	}
	fps := plan.ExtraInputs[0].Matte.FPS
	if fps != graph.FPSText(plan.FPS) || fps != "25" {
		t.Errorf("matte input fps text %q, FPSText %q", fps, graph.FPSText(plan.FPS))
	}
	rm := resolvedMatte{Model: recipe.MatteModelISNetAnime, ReqSize: 0, Size: 64, Precision: "fp32",
		Dir: st.MatteDir(strings.Repeat("c", 64)), ClipKey: strings.Repeat("c", 64), Manifest: &matte.Manifest{Weights: testWeights, Proc: "1", FPS: fps, Frames: 50}}
	if err := s.fillExtraInputs(plan, []resolvedMatte{rm}); err != nil {
		t.Fatal(err)
	}
	in := plan.ExtraInputs[0]
	if in.Path != filepath.Join(rm.Dir, matte.FramePattern) || in.Matte.Frames != 50 {
		t.Errorf("filled input = %+v", in)
	}
	// No resolved matte / another fps: the client's error, before any enc builder.
	plan2, _ := graph.CompileWithSources(s.infos(), ops, recipe.Output{Format: "gif"})
	if err := s.fillExtraInputs(plan2, nil); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("no mattes: %v", err)
	}
	stale := rm
	staleMan := *rm.Manifest
	staleMan.FPS = "15"
	stale.Manifest = &staleMan
	if err := s.fillExtraInputs(plan2, []resolvedMatte{stale}); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "15 fps") {
		t.Errorf("stale fps: %v", err)
	}
}

// TestCheckMatteResolved: the render refuses an identity other than the
// one Submit hashed.
func TestCheckMatteResolved(t *testing.T) {
	rm := resolvedMatte{Model: recipe.MatteModelISNetAnime, ReqSize: 0, Size: 64, Precision: "fp32", Manifest: &matte.Manifest{Weights: testWeights, Proc: "1", FPS: "25", Frames: 50}}
	ok := matteOp(`{"resolved":{"weights":"` + testWeights + `","proc":"1","size":64,"precision":"fp32"}}`)
	if err := checkMatteResolved([]recipe.Op{ok}, []resolvedMatte{rm}); err != nil {
		t.Errorf("matching identity: %v", err)
	}
	if err := checkMatteResolved([]recipe.Op{matteOp("")}, []resolvedMatte{rm}); err != nil {
		t.Errorf("no Resolved: %v", err)
	}
	changed := matteOp(`{"resolved":{"weights":"` + otherWeights + `","proc":"1","size":64,"precision":"fp32"}}`)
	if err := checkMatteResolved([]recipe.Op{changed}, []resolvedMatte{rm}); err == nil || !strings.Contains(err.Error(), "submit the render again") {
		t.Errorf("changed weights: %v", err)
	}
	if err := checkMatteResolved([]recipe.Op{ok}, nil); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("no resolved matte: %v", err)
	}
}

// TestPreviewKeysFoldMatte: still and proxy keys change with the resolved
// clip key, and only then.
func TestPreviewKeysFoldMatte(t *testing.T) {
	srcs := []string{strings.Repeat("a", 64)}
	ops := []recipe.Op{matteOp("")}
	out := recipe.Output{Format: "gif", FPS: 25}
	a := resolvedMatte{ClipKey: strings.Repeat("1", 64)}
	b := resolvedMatte{ClipKey: strings.Repeat("2", 64)}
	k0, _ := stillKey(srcs, ops, out, 0.5, 480)
	ka, _ := stillKeyFor(srcs, ops, out, 0.5, 480, []resolvedMatte{a})
	kb, _ := stillKeyFor(srcs, ops, out, 0.5, 480, []resolvedMatte{b})
	kn, _ := stillKeyFor(srcs, ops, out, 0.5, 480, nil)
	if k0 != kn || ka == k0 || kb == k0 || ka == kb {
		t.Errorf("still keys: plain %s nil %s a %s b %s", short(k0), short(kn), short(ka), short(kb))
	}
	p0, _ := proxyKey(srcs, ops, out, 360, 10)
	pa, _ := proxyKeyFor(srcs, ops, out, 360, 10, []resolvedMatte{a})
	pb, _ := proxyKeyFor(srcs, ops, out, 360, 10, []resolvedMatte{b})
	pn, _ := proxyKeyFor(srcs, ops, out, 360, 10, nil)
	if p0 != pn || pa == p0 || pb == p0 || pa == pb {
		t.Errorf("proxy keys: plain %s nil %s a %s b %s", short(p0), short(pn), short(pa), short(pb))
	}
}

// TestCropDetectMatteInputPlumbing: the detection pass hands enc a plan
// whose matte input is the memo's sequence at the detection rate (the fake
// ffmpeg records its argv), and the box is memoised under a key that
// carries the clip key.
func TestCropDetectMatteInputPlumbing(t *testing.T) {
	tools, marker := fakeFFmpegTools(t)
	st := newTestStore(t)
	src := putSource(t, st, true)
	ping := testPing(testWeights)
	writeMatteFacts(t, st, ping)
	m := NewManager(st, tools, Options{})
	blobs, err := m.lookupSources([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	ops := []recipe.Op{matteOp(""), autocropOp(`{"padding":2}`)}
	out := recipe.Output{Format: "gif", FPS: 25}
	key, fps, frames := matteKeyFor(t, blobs[0], ops, out, ping, recipe.MatteModelISNetAnime, 0)
	if frames != 50 {
		t.Fatalf("matte plan frames = %d, want 50", frames)
	}
	dir := writeMatteMemo(t, st, key, src, ping, recipe.MatteModelISNetAnime, fps, 50, 64, 48, func(i, x, y int) uint8 { return 255 })

	// Without the memo's facts nothing can be resolved; with them the
	// detection runs with the matte input.
	noFacts := NewManager(newTestStore(t), tools, Options{})
	if _, err := noFacts.ResolveAutoCropFor(context.Background(), src, ops, out); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unknown source in a bare store: %v", err)
	}
	releaseFakes(t, marker)
	got, err := m.ResolveAutoCropFor(context.Background(), src, ops, out)
	if err != nil {
		t.Fatalf("ResolveAutoCropFor: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}) {
		t.Errorf("resolved box = %+v", box)
	}
	if n := fakeMarkers(marker); n != 1 {
		t.Fatalf("%d detections ran, want 1", n)
	}
	argv := fakeArgv(t, marker)
	want := filepath.Join(dir, matte.FramePattern)
	if !strings.Contains(argv, "\n-i\n"+want+"\n") {
		t.Errorf("detection argv lacks the matte input %q:\n%s", want, argv)
	}
	if !strings.Contains(argv, "\n-framerate\n"+fps+"\n") {
		t.Errorf("detection argv lacks -framerate %s:\n%s", fps, argv)
	}
	// The memo key carries the clip key: other facts, another detection.
	pre, _ := autocropDetectionOps(ops, 1)
	k, _ := autocropKey(src, pre, 1, fps, []string{key})
	if _, err := os.Stat(filepath.Join(st.Scratch, autocropDir, k+".json")); err != nil {
		t.Errorf("box not memoised under the matte-salted key: %v", err)
	}
	// The memo serves the same stack without ffmpeg; a stack with a matte
	// op but no memo on disk is unavailable, not a detection.
	noFF := NewManager(st, ffrun.Tools{}, Options{})
	if _, err := noFF.ResolveAutoCropFor(context.Background(), src, ops, out); err != nil {
		t.Errorf("memo hit: %v", err)
	}
	if _, err := noFF.ResolveAutoCropFor(context.Background(), src, []recipe.Op{matteOp(`{"size":32}`), autocropOp(`{}`)}, out); !errors.Is(err, ErrMatteUnavailable) {
		t.Errorf("no memo for size 32: %v", err)
	}
	// The render resolves the autocrop from the ops Submit filled with the
	// matte's Resolved identity (fillMatteResolved) while the previews strip
	// it first: the detection ops — and so the autocrop key — must be the
	// same either way (autocropDetectionOps strips the identity), so a
	// render of the recipe the previews already detected reads their memo
	// instead of detecting again inside its render slot: the bare manager
	// (no ffmpeg) resolves it, and the fake's marker count stays at 1.
	filled, err := m.fillMatteResolved(ops)
	if err != nil {
		t.Fatalf("fillMatteResolved: %v", err)
	}
	if !bytes.Contains(filled[0].Params, []byte(`"resolved"`)) {
		t.Fatalf("fillMatteResolved left no identity in %s", filled[0].Params)
	}
	preFilled, err := autocropDetectionOps(filled, 1)
	if err != nil {
		t.Fatalf("autocropDetectionOps(filled): %v", err)
	}
	if kFilled, _ := autocropKey(src, preFilled, 1, fps, []string{key}); kFilled != k {
		t.Errorf("autocrop key of the render's Resolved ops %s != the previews' %s (detection ops %s)", kFilled, k, preFilled)
	}
	if _, err := noFF.ResolveAutoCropFor(context.Background(), src, filled, out); err != nil {
		t.Errorf("memo hit with the render's Resolved ops: %v", err)
	}
	if _, err := m.ResolveAutoCropFor(context.Background(), src, filled, out); err != nil {
		t.Errorf("ResolveAutoCropFor with the render's Resolved ops: %v", err)
	}
	if n := fakeMarkers(marker); n != 1 {
		t.Errorf("%d detections ran after the render's resolution, want 1 (the previews' memo)", n)
	}
}

// fakeArgv returns the recorded argv of the (single) fake ffmpeg run.
func fakeArgv(t *testing.T, marker string) string {
	t.Helper()
	entries, err := os.ReadDir(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || e.Name() == fakeRelease {
			continue
		}
		data, err := os.ReadFile(filepath.Join(marker, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	t.Fatal("no fake ffmpeg marker")
	return ""
}

// ---- real ffmpeg ------------------------------------------------------------

// matteE2E is the Phase 3 harness with the facts persisted before the
// manager loads them.
func matteE2E(t *testing.T, p *matte.Ping, opts Options) *e2e {
	t.Helper()
	tools := realTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	st := newTestStore(t)
	writeMatteFacts(t, st, p)
	return &e2e{t: t, ctx: ctx, st: st, tools: tools, m: NewManager(st, tools, opts), dir: t.TempDir()}
}

// opaqueClip is a 64x48 opaque clip of 10 frames at 10 fps (a red square
// over green, static).
const opaqueClip = "color=c=0x00FF00:s=64x48:r=10:d=1[bg];color=c=0xDC1E1E:s=24x24:r=10:d=1[fg];[bg][fg]overlay=16:12:format=rgb"

// frameAlphas reads the alpha of pixel (x, y) of every frame of a frames
// export, in order.
func (e *e2e) frameAlphas(fin Job, x, y int) []uint8 {
	e.t.Helper()
	files := e.frameFiles(fin)
	sort.Slice(files, func(i, j int) bool { return files[i].Index < files[j].Index })
	out := make([]uint8, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(e.st.ResultDir(fin.RecipeHash), f.Name))
		if err != nil {
			e.t.Fatal(err)
		}
		pix, w, _ := pngPix(e.t, data)
		out = append(out, pix[(y*w+x)*4+3])
	}
	return out
}

// TestRenderMatteFrames: the master carries matte i on frame i (opaque
// main: the matte IS the alpha), the result's report says which matte, the
// count check skips static renders and refuses a short memo (keeping it),
// and a bounce mirrors the mattes with the frames.
func TestRenderMatteFrames(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("clip.mov", opaqueClip, "rgb24")
	if clip.Info.HasAlpha || clip.Info.Frames != 10 {
		t.Fatalf("clip probe: %+v", clip.Info)
	}
	ops := []recipe.Op{matteOp("")}
	frames := recipe.Output{Format: recipe.FormatFrames, FPS: 10}
	flatMemo(t, e.st, clip, ops, frames, ping, 10)

	t.Run("frames carry their matte", func(t *testing.T) {
		fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: ops, Output: frames})
		got := e.frameAlphas(fin, 3, 3)
		if len(got) != 10 {
			t.Fatalf("%d frames, want 10", len(got))
		}
		for i, a := range got {
			if a != matteGray(i) {
				t.Errorf("frame %d alpha %d, want %d", i, a, matteGray(i))
			}
		}
		if r := matteResolvedOf(t, fin.Recipe.Ops); r == nil || r.Weights != testWeights {
			t.Errorf("result recipe Resolved = %+v", r)
		}
	})
	t.Run("report names the matte", func(t *testing.T) {
		out := recipe.Output{Format: recipe.FormatAPNG, FPS: 10}
		flatMemo(t, e.st, clip, ops, out, ping, 10)
		fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: ops, Output: out})
		rep := fin.Result.Files[0].Report
		if rep == nil || !rep.HasAlpha {
			t.Fatalf("report = %+v", rep)
		}
		found := false
		for _, c := range rep.Checks {
			if c.Rule == RuleRenderMatte {
				found = true
				if !c.OK || !strings.HasPrefix(c.Detail, "AI matte: "+recipe.MatteModelISNetAnime) || !strings.Contains(c.Detail, testWeights[:8]) {
					t.Errorf("render.matte check = %+v", c)
				}
			}
		}
		if !found {
			t.Errorf("no render.matte check in %+v", rep.Checks)
		}
	})
	t.Run("static render skips the count check", func(t *testing.T) {
		out := recipe.Output{Format: recipe.FormatPNG, FPS: 10}
		flatMemo(t, e.st, clip, ops, out, ping, 3) // short of the clip's 10
		fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: ops, Output: out})
		pix, _, _ := pngPix(t, e.resultBytes(fin.Recipe, fin.Result.Files[0].Name))
		if pix[3] != matteGray(0) {
			t.Errorf("png alpha %d, want %d", pix[3], matteGray(0))
		}
	})
	t.Run("count mismatch fails and keeps the memo", func(t *testing.T) {
		out := recipe.Output{Format: recipe.FormatWebP, FPS: 10}
		dir := flatMemo(t, e.st, clip, ops, out, ping, 9)
		fin := runJob(t, e.m, recipe.Recipe{Sources: []string{clip.Hash}, Ops: ops, Output: out})
		if fin.State != StateError || !strings.Contains(fin.Error, "9 frames") || !strings.Contains(fin.Error, "decoded to 10") {
			t.Fatalf("job = %s / %q", fin.State, fin.Error)
		}
		if _, err := os.Stat(filepath.Join(dir, matte.ManifestName)); err != nil {
			t.Errorf("memo dropped after the count mismatch: %v", err)
		}
	})
	t.Run("bounce mirrors the mattes", func(t *testing.T) {
		bops := []recipe.Op{matteOp(""), {Kind: recipe.OpBounce}}
		flatMemo(t, e.st, clip, bops, frames, ping, 10)
		fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: bops, Output: frames})
		got := e.frameAlphas(fin, 3, 3)
		if len(got) != 20 {
			t.Fatalf("%d frames, want 20", len(got))
		}
		for i, a := range got {
			want := matteGray(i)
			if i >= 10 {
				want = matteGray(19 - i)
			}
			if a != want {
				t.Errorf("bounced frame %d alpha %d, want %d", i, a, want)
			}
		}
	})
	t.Run("identity drift since submit fails the render", func(t *testing.T) {
		// The job carries the facts of its Submit; a manager that loaded
		// other facts (a new sidecar image) must not file a result under it.
		st2 := newTestStore(t)
		writeMatteFacts(t, st2, testPing(otherWeights))
		other := &e2e{t: t, ctx: e.ctx, st: st2, tools: e.tools, m: NewManager(st2, e.tools, Options{Concurrency: 1}), dir: t.TempDir()}
		clip2 := other.lavfi("clip.mov", opaqueClip, "rgb24")
		stale := matteOp(`{"resolved":{"weights":"` + testWeights + `","proc":"1","size":64,"precision":"fp32"}}`)
		// Submit strips a client identity, so build the job's recipe by hand
		// the way a submitted one looks and run the pipeline directly.
		flatMemo(t, st2, clip2, []recipe.Op{stale}, frames, testPing(otherWeights), 10)
		r := recipe.Recipe{Sources: []string{clip2.Hash}, Ops: []recipe.Op{stale}, Output: frames}
		j := &job{snap: Job{ID: "drift", RecipeHash: ResultKey(r), Recipe: r}, cancel: func() {}, subs: map[int]*subscriber{}}
		other.m.mu.Lock()
		other.m.jobs[j.snap.ID] = j
		other.m.mu.Unlock()
		_, err := other.m.render(e.ctx, j)
		if err == nil || !strings.Contains(err.Error(), "submit the render again") {
			t.Errorf("drifted identity: %v", err)
		}
	})
}

// TestStillMatteFrames: a still shows the matte of the frame it selects —
// forward (one unlooped PNG), with a memo one frame short of the plan's
// estimate (clamped to the last matte), reversed, bounced and [reverse,
// bounce] — and the still memo is keyed by the matte.
func TestStillMatteFrames(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("clip.mov", opaqueClip, "rgb24")
	out := recipe.Output{Format: "gif", FPS: 10}
	stillAlpha := func(t *testing.T, ops []recipe.Op, i int) uint8 {
		t.Helper()
		data, err := e.m.StillSources(e.ctx, []string{clip.Hash}, ops, out, (float64(i)+0.5)/10, 0)
		if err != nil {
			t.Fatalf("still %d: %v", i, err)
		}
		pix, w, h := pngPix(t, data)
		if w != 64 || h != 48 {
			t.Fatalf("still is %dx%d", w, h)
		}
		return pix[(3*w+3)*4+3]
	}
	forward := []recipe.Op{matteOp("")}
	flatMemo(t, e.st, clip, forward, out, ping, 10)
	for _, i := range []int{0, 3, 9} {
		if a := stillAlpha(t, forward, i); a != matteGray(i) {
			t.Errorf("forward still %d alpha %d, want %d", i, a, matteGray(i))
		}
	}
	t.Run("memo short by one frame clamps to the last matte", func(t *testing.T) {
		short := []recipe.Op{matteOp(`{"size":32}`)}
		flatMemo32 := func(n int) {
			key, fps, _ := matteKeyFor(t, clip, short, out, ping, recipe.MatteModelISNetAnime, 32)
			writeMatteMemo(t, e.st, key, clip.Hash, ping, recipe.MatteModelISNetAnime, fps, n, 32, 32, func(i, x, y int) uint8 { return matteGray(i) })
		}
		flatMemo32(9)
		if a := stillAlpha(t, short, 9); a != matteGray(8) {
			t.Errorf("last still with a 9-matte memo: alpha %d, want the clamped %d", a, matteGray(8))
		}
		if a := stillAlpha(t, short, 2); a != matteGray(2) {
			t.Errorf("still 2 with a 9-matte memo: alpha %d, want %d", a, matteGray(2))
		}
	})
	t.Run("reversed", func(t *testing.T) {
		ops := []recipe.Op{matteOp(""), {Kind: recipe.OpReverse}}
		flatMemo(t, e.st, clip, ops, out, ping, 10)
		for _, i := range []int{0, 4, 9} {
			if a := stillAlpha(t, ops, i); a != matteGray(9-i) {
				t.Errorf("reversed still %d alpha %d, want %d", i, a, matteGray(9-i))
			}
		}
	})
	t.Run("bounced", func(t *testing.T) {
		ops := []recipe.Op{matteOp(""), {Kind: recipe.OpBounce}}
		flatMemo(t, e.st, clip, ops, out, ping, 10)
		for _, i := range []int{0, 7, 12, 19} {
			want := matteGray(i)
			if i >= 10 {
				want = matteGray(19 - i)
			}
			if a := stillAlpha(t, ops, i); a != want {
				t.Errorf("bounced still %d alpha %d, want %d", i, a, want)
			}
		}
	})
	t.Run("reverse then bounce", func(t *testing.T) {
		ops := []recipe.Op{matteOp(""), {Kind: recipe.OpReverse}, {Kind: recipe.OpBounce}}
		flatMemo(t, e.st, clip, ops, out, ping, 10)
		for _, i := range []int{0, 5, 14, 19} {
			want := matteGray(9 - i)
			if i >= 10 {
				want = matteGray(i - 10)
			}
			if a := stillAlpha(t, ops, i); a != want {
				t.Errorf("[reverse, bounce] still %d alpha %d, want %d", i, a, want)
			}
		}
	})
	t.Run("memo keyed by the matte", func(t *testing.T) {
		// The forward still of frame 3 is memoised; a memo made under other
		// weights is another still key, so nothing stale is served: with the
		// facts changed and no memo for them, the still is unavailable.
		st2 := newTestStore(t)
		writeMatteFacts(t, st2, testPing(otherWeights))
		m2 := NewManager(st2, e.tools, Options{})
		if _, err := m2.StillSources(e.ctx, []string{clip.Hash}, forward, out, 0.35, 0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("other store: %v", err) // the clip is not in st2 at all
		}
		a := resolvedMatte{ClipKey: strings.Repeat("1", 64)}
		b := resolvedMatte{ClipKey: strings.Repeat("2", 64)}
		ka, _ := stillKeyFor([]string{clip.Hash}, forward, stillOutput(out), 0.35, DefaultStillWidth, []resolvedMatte{a})
		kb, _ := stillKeyFor([]string{clip.Hash}, forward, stillOutput(out), 0.35, DefaultStillWidth, []resolvedMatte{b})
		if ka == kb {
			t.Error("still keys of two clip keys collide")
		}
	})
}

// TestProxyMatteReversed: the reversed proxy's frames carry their own
// mattes in reversed order (the merge precedes the reverse stage).
func TestProxyMatteReversed(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	if ffmpegMajor(t, e.tools) < 9 {
		t.Skip("decoding the animated WebP proxy needs FFmpeg 9")
	}
	clip := e.lavfi("clip.mov", opaqueClip, "rgb24")
	out := recipe.Output{Format: "gif", FPS: 10}
	for name, ops := range map[string][]recipe.Op{
		"forward":  {matteOp("")},
		"reversed": {matteOp(""), {Kind: recipe.OpReverse}},
	} {
		t.Run(name, func(t *testing.T) {
			flatMemo(t, e.st, clip, ops, out, ping, 10)
			data, err := e.m.Proxy(e.ctx, []string{clip.Hash}, ops, out, 360, 10)
			if err != nil {
				t.Fatalf("proxy: %v", err)
			}
			path := filepath.Join(e.dir, name+".webp")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			raw, err := ffrun.RunOutput(e.ctx, e.tools.FFmpeg, append(append([]string{}, ffmpegPrefix...),
				"-i", path, "-vf", "scale=64:48:flags=neighbor", "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"))
			if err != nil {
				t.Fatalf("decode proxy: %v", err)
			}
			const frame = 64 * 48 * 4
			if len(raw) < frame*8 || len(raw)%frame != 0 {
				t.Fatalf("proxy decoded to %d bytes (%d frames)", len(raw), len(raw)/frame)
			}
			var got []int
			for off := 0; off < len(raw); off += frame {
				a := raw[off+(3*64+3)*4+3]
				// Nearest matte value (lossy WebP may wobble the alpha a little).
				best, dist := -1, 256
				for i := 0; i < 10; i++ {
					d := int(a) - int(matteGray(i))
					if d < 0 {
						d = -d
					}
					if d < dist {
						best, dist = i, d
					}
				}
				if dist > 3 {
					t.Fatalf("frame %d alpha %d is no matte value", off/frame, a)
				}
				got = append(got, best)
			}
			for k := 1; k < len(got); k++ {
				if name == "reversed" && got[k] > got[k-1] {
					t.Fatalf("reversed proxy mattes not descending: %v", got)
				}
				if name == "forward" && got[k] < got[k-1] {
					t.Fatalf("forward proxy mattes not ascending: %v", got)
				}
			}
			if got[0] != map[string]int{"forward": 0, "reversed": 9}[name] {
				t.Errorf("%s proxy starts at matte %d: %v", name, got[0], got)
			}
		})
	}
}

// TestAutoCropMatteSubject: crop to content on a matted clip resolves to the
// box the matte marks (the full-frame opaque source would otherwise give the
// whole frame).
func TestAutoCropMatteSubject(t *testing.T) {
	requireCropDetect(t)
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("clip.mov", opaqueClip, "rgb24")
	out := recipe.Output{Format: "gif", FPS: 10}
	ops := []recipe.Op{matteOp(""), autocropOp(`{}`)}
	boxMemo := func(out recipe.Output) {
		key, fps, _ := matteKeyFor(t, clip, ops, out, ping, recipe.MatteModelISNetAnime, 0)
		writeMatteMemo(t, e.st, key, clip.Hash, ping, recipe.MatteModelISNetAnime, fps, 10, 64, 48, func(i, x, y int) uint8 {
			if x >= 20 && x < 44 && y >= 8 && y < 40 {
				return 255
			}
			return 0
		})
	}
	boxMemo(out)
	got, err := e.m.ResolveAutoCropFor(e.ctx, clip.Hash, ops, out)
	if err != nil {
		t.Fatalf("ResolveAutoCropFor: %v", err)
	}
	if box := resolvedBox(t, got); box != (recipe.CropParams{X: 20, Y: 8, W: 24, H: 32}) {
		t.Errorf("resolved box = %+v, want the matte's 24x32 at (20,8)", box)
	}
	// Without the matte the opaque clip is its whole frame.
	plain, err := e.m.ResolveAutoCropFor(e.ctx, clip.Hash, []recipe.Op{autocropOp(`{}`)}, out)
	if err != nil {
		t.Fatal(err)
	}
	if box := resolvedBox(t, plain); box != (recipe.CropParams{W: 64, H: 48}) {
		t.Errorf("plain box = %+v, want the full frame", box)
	}
	// And the render through compile crops to it (the box is served from
	// the autocrop memo, keyed by the clip key).
	frames := recipe.Output{Format: recipe.FormatFrames, FPS: 10}
	boxMemo(frames)
	fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: ops, Output: frames})
	if f := e.frameFiles(fin)[0]; f.Width != 24 || f.Height != 32 {
		t.Errorf("rendered frame %dx%d, want 24x32", f.Width, f.Height)
	}
	if got := e.frameAlphas(fin, 0, 0); len(got) != 10 || got[0] != 255 {
		t.Errorf("cropped frames: %d, first alpha %v (want 10 frames, fully opaque inside the box)", len(got), got)
	}
}
