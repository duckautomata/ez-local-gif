package jobs

// Phase 5c integration at the recipe level, without ffmpeg or a sidecar:
// the autocrop detection key's canonical matte op (stabilise / keep /
// prompts / edge join it, spelling-independently), the render.matte notes
// and the pipeline version stamp.

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// TestAutocropCanonicalMatteOp5c: the 5c params of a matte op join the
// detection ops' canonical form — spelled the same whatever the client's
// case, hash sign or prompt order — and so the autocrop key; Resolved never
// does, and a stack without them keys exactly as before.
func TestAutocropCanonicalMatteOp5c(t *testing.T) {
	src := strings.Repeat("a", 64)
	keyOf := func(t *testing.T, ops []recipe.Op) string {
		t.Helper()
		pre, err := autocropDetectionOps(ops, 1)
		if err != nil {
			t.Fatal(err)
		}
		k, err := autocropKey(src, pre, 1, "25", []string{"k1"})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	stack := func(params string) []recipe.Op { return []recipe.Op{matteOp(params), autocropOp(`{}`)} }

	plain := keyOf(t, stack(""))
	if keyOf(t, stack(`{}`)) != plain || keyOf(t, stack(`{"model":"isnet-anime"}`)) != plain {
		t.Error("the spellings of a bare matte op key differently")
	}
	if keyOf(t, stack(`{"resolved":{"weights":"`+testWeights+`","proc":"1","size":64,"precision":"fp32","tracker":"x","edge":"isnet-anime"}}`)) != plain {
		t.Error("a Resolved identity (5c fields included) changed the detection key")
	}
	// Each 5c param is its own picture.
	variants := map[string]string{
		"stabilise light":  `{"stabilise":"light"}`,
		"stabilise strong": `{"stabilise":"strong"}`,
		"keep":             `{"keep":["3a7bd5"]}`,
		"keep similarity":  `{"keep":["3a7bd5"],"keepSimilarity":0.2}`,
		"guided":           `{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.1,0.1,0.5,0.5]}]}`,
		"guided no edge":   `{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.1,0.1,0.5,0.5]}],"edge":"none"}`,
		"guided edge id":   `{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.1,0.1,0.5,0.5]}],"edge":"isnet-anime"}`,
		"guided other box": `{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.2,0.1,0.5,0.5]}]}`,
	}
	seen := map[string]string{"plain": plain}
	for name, params := range variants {
		k := keyOf(t, stack(params))
		for other, ok := range seen {
			if ok == k {
				t.Errorf("%q keys like %q", name, other)
			}
		}
		seen[name] = k
	}
	// Spelling-independent: keep colours normalise, prompts sort by frame.
	if keyOf(t, stack(`{"keep":["#3A7BD5"]}`)) != seen["keep"] {
		t.Error("keep colour case / hash sign changed the key")
	}
	ordered := `{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.1,0.1,0.5,0.5]},{"frame":7,"points":[[0.5,0.5,1]]}]}`
	swapped := `{"model":"sam2-tiny","prompts":[{"frame":7,"points":[[0.5,0.5,1]]},{"frame":0,"box":[0.1,0.1,0.5,0.5]}]}`
	if keyOf(t, stack(ordered)) != keyOf(t, stack(swapped)) {
		t.Error("the prompt order changed the key")
	}
	// The canonical op's text: lowercase hex, sorted prompts, no resolved.
	pre, _ := autocropDetectionOps([]recipe.Op{matteOp(`{"keep":["#3A7BD5","FFFFFF"],"stabilise":"light","resolved":{"weights":"w","proc":"1","size":64,"precision":"fp32"}}`), autocropOp(`{}`)}, 1)
	var p recipe.MatteParams
	if err := json.Unmarshal(pre[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Model != recipe.MatteModelDefault || p.Resolved != nil || p.Stabilise != "light" || len(p.Keep) != 2 || p.Keep[0] != "3a7bd5" || p.Keep[1] != "ffffff" {
		t.Errorf("canonical matte op = %s", pre[0].Params)
	}
	// An invalid keep colour is left for the compiler: still canonical, not dropped.
	bad := canonicalMatteParams(recipe.MatteParams{Keep: []string{"zz"}})
	if len(bad.Keep) != 1 || bad.Keep[0] != "zz" {
		t.Errorf("invalid keep colour rewritten: %+v", bad.Keep)
	}
	// Malformed params are returned untouched.
	raw := matteOp(`{"size":`)
	if got := canonicalMatteOp(0, raw); string(got.Params) != string(raw.Params) {
		t.Errorf("malformed op rewritten: %s", got.Params)
	}
}

// TestMatteRecipeNotes: the render.matte line's recipe notes and their
// word-wise dedupe against what the identity part already says.
func TestMatteRecipeNotes(t *testing.T) {
	cases := []struct {
		name string
		ops  []recipe.Op
		want []string
	}{
		{"plain", []recipe.Op{matteOp("")}, nil},
		{"no matte op", []recipe.Op{{Kind: recipe.OpTrim}}, nil},
		{"stabilise", []recipe.Op{matteOp(`{"stabilise":"light"}`)}, []string{"stabilise light"}},
		{"keep one", []recipe.Op{matteOp(`{"keep":["3a7bd5"]}`)}, []string{"keep 1 colour"}},
		{"keep two at a similarity", []recipe.Op{matteOp(`{"keep":["3a7bd5","ffffff"],"keepSimilarity":0.2}`)}, []string{"keep 2 colours (similarity 0.2)"}},
		{"guided default edge", []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.1,0.1,0.5,0.5]}]}`)},
			[]string{"guided (sam2-tiny) + edge (the device's default model) · 1 prompted frame"}},
		{"guided resolved edge", []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0.1,0.1,0.5,0.5]},{"frame":9,"points":[[0.5,0.5,1]]}],"stabilise":"strong","resolved":{"weights":"w","proc":"1","size":0,"precision":"bf16","tracker":"t","edge":"birefnet-lite","edgeWeights":"e","edgeProc":"1"}}`)},
			[]string{"guided (sam2-tiny) + edge birefnet-lite · 2 prompted frames", "stabilise strong"}},
		{"guided no edge", []recipe.Op{matteOp(`{"model":"sam2-tiny","edge":"none","prompts":[{"frame":0,"box":[0.1,0.1,0.5,0.5]}]}`)},
			[]string{"guided (sam2-tiny), tracker mask only · 1 prompted frame"}},
		{"two ops dedupe", []recipe.Op{matteOp(`{"stabilise":"light"}`), matteOp(`{"model":"birefnet-lite","stabilise":"light","keep":["000000"]}`)}, []string{"stabilise light", "keep 1 colour"}},
		{"malformed", []recipe.Op{matteOp(`{"size":`)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matteRecipeNotes(c.ops)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("notes = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("applied to the render.matte check", func(t *testing.T) {
		rep := &discordlint.Report{}
		ops := []recipe.Op{matteOp(`{"stabilise":"light","keep":["3a7bd5"]}`)}
		applyMatteRecipeNotes(rep, ops) // no render.matte check yet: nothing to append to
		if len(rep.Checks) != 0 {
			t.Fatalf("notes without a render.matte check: %+v", rep.Checks)
		}
		applyMatteInfo(rep, []resolvedMatte{{Model: recipe.MatteModelISNetAnime, Size: 64, Precision: "fp32"}})
		applyMatteRecipeNotes(rep, ops)
		if len(rep.Checks) != 1 || rep.Checks[0].Rule != RuleRenderMatte || !rep.Checks[0].OK {
			t.Fatalf("checks = %+v", rep.Checks)
		}
		d := rep.Checks[0].Detail
		if !strings.HasPrefix(d, "AI matte: isnet-anime 64 px fp32") || !strings.HasSuffix(d, " · stabilise light · keep 1 colour") {
			t.Errorf("detail = %q", d)
		}
		// Idempotent, and a word the identity part already carries is not repeated.
		applyMatteRecipeNotes(rep, ops)
		if rep.Checks[0].Detail != d {
			t.Errorf("a second application changed the detail: %q", rep.Checks[0].Detail)
		}
		rep2 := &discordlint.Report{Checks: []discordlint.Check{{Rule: RuleRenderMatte, Level: discordlint.LevelInfo, OK: true, Detail: "AI matte: isnet-anime 64 px · Stabilised light"}}}
		applyMatteRecipeNotes(rep2, ops)
		if got := rep2.Checks[0].Detail; got != "AI matte: isnet-anime 64 px · Stabilised light · keep 1 colour" {
			t.Errorf("dedupe: %q", got)
		}
		applyMatteRecipeNotes(nil, ops) // nil-safe
	})
}

// TestPipelineVersion5c pins the Phase 5c result-key salt: the keep union
// and the derived sequences change what matte recipes render to.
func TestPipelineVersion5c(t *testing.T) {
	if PipelineVersion != "2026-10-09.1" {
		t.Errorf("PipelineVersion = %q, want 2026-10-09.1 (Phase 5c)", PipelineVersion)
	}
}

// ---- real ffmpeg ------------------------------------------------------------

// TestRenderMatteKeepColours: a keep colour comes back opaque where the
// matte had removed it (the union the compiler emits behind the merge),
// every other pixel keeps the matte's alpha, and the render.matte line
// names the keep colour. Skips without ffmpeg.
func TestRenderMatteKeepColours(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("clip.mov", opaqueClip, "rgb24")
	frames := recipe.Output{Format: recipe.FormatFrames, FPS: 10}
	// The memo is keyed by the temporal ops and the identity alone: the
	// plain op and the keep op share it.
	flatMemo(t, e.st, clip, []recipe.Op{matteOp("")}, frames, ping, 10)
	keep := []recipe.Op{matteOp(`{"keep":["#DC1E1E"]}`)}

	fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: keep, Output: frames})
	red := e.frameAlphas(fin, 28, 24) // inside the red square (16..40 x 12..36)
	green := e.frameAlphas(fin, 3, 3) // the green background
	if len(red) != 10 || len(green) != 10 {
		t.Fatalf("%d / %d frames, want 10", len(red), len(green))
	}
	for i := range red {
		if red[i] != 255 {
			t.Errorf("frame %d: kept colour alpha %d, want 255", i, red[i])
		}
		if green[i] != matteGray(i) {
			t.Errorf("frame %d: background alpha %d, want the matte's %d", i, green[i], matteGray(i))
		}
	}
	plain := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{matteOp("")}, Output: frames})
	if a := e.frameAlphas(plain, 28, 24); a[0] != matteGray(0) {
		t.Errorf("without keep the square's alpha is %d, want the matte's %d", a[0], matteGray(0))
	}

	t.Run("report names the keep colour", func(t *testing.T) {
		out := recipe.Output{Format: recipe.FormatAPNG, FPS: 10}
		flatMemo(t, e.st, clip, keep, out, ping, 10)
		fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: keep, Output: out})
		rep := fin.Result.Files[0].Report
		if rep == nil {
			t.Fatal("no report")
		}
		found := false
		for _, c := range rep.Checks {
			if c.Rule == RuleRenderMatte {
				found = true
				if !c.OK || !strings.HasPrefix(c.Detail, "AI matte: "+recipe.MatteModelISNetAnime) || !strings.Contains(c.Detail, "keep 1 colour") {
					t.Errorf("render.matte check = %+v", c)
				}
			}
		}
		if !found {
			t.Errorf("no render.matte check in %+v", rep.Checks)
		}
	})
}

// TestFillMatteInputsDerived: a matte input whose op asks for a stabilised
// sequence reads <clipdir>/stab-<mode>/%06d.png with the manifest's frame
// count; a plain op reads the raw memo; two ops of one model with other
// stabilise modes share the resolution and read their own sequences.
func TestFillMatteInputsDerived(t *testing.T) {
	st := newTestStore(t)
	src := putSource(t, st, true)
	blobs, err := NewManager(st, fakeTools, Options{}).lookupSources([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	s := &sources{hashes: []string{src}, blobs: blobs}
	ops := []recipe.Op{matteOp(`{"stabilise":"light"}`), matteOp(`{"stabilise":"strong"}`), matteOp("")}
	plan, err := graph.CompileWithSources(s.infos(), ops, recipe.Output{Format: "gif"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraInputs) != 3 {
		t.Fatalf("plan inputs = %+v, want one per stabilise mode", plan.ExtraInputs)
	}
	dir := st.MatteDir(strings.Repeat("c", 64))
	rm := resolvedMatte{Model: recipe.MatteModelISNetAnime, Size: 64, Precision: "fp32", Dir: dir, ClipKey: strings.Repeat("c", 64),
		Manifest: &matte.Manifest{Weights: testWeights, Proc: "1", FPS: plan.ExtraInputs[0].Matte.FPS, Frames: 50}}
	if err := s.fillExtraInputs(plan, []resolvedMatte{rm}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		recipe.MatteStabiliseLight:  filepath.Join(dir, "stab-light", matte.FramePattern),
		recipe.MatteStabiliseStrong: filepath.Join(dir, "stab-strong", matte.FramePattern),
		"":                          filepath.Join(dir, matte.FramePattern),
	}
	for _, in := range plan.ExtraInputs {
		if in.Matte == nil {
			t.Fatalf("overlay input in a matte-only stack: %+v", in)
		}
		if in.Path != want[in.Matte.Stabilise] || in.Matte.Frames != 50 {
			t.Errorf("stabilise %q: input = %+v, want path %s", in.Matte.Stabilise, in, want[in.Matte.Stabilise])
		}
	}
	// The directory the input reads from is the store's derived-dir layout
	// (what the sweeper counts with the clip and Protect covers).
	if got := matteInputDir(&rm, &graph.MatteInput{Stabilise: "light"}); got != store.MatteDerivedDir(dir, store.MatteStabName("light")) {
		t.Errorf("matteInputDir = %s", got)
	}

	// The resolution's own sequence wins: one resolved matte per request
	// (the compiler's dedupe key), paired by stabilise / prompts / edge,
	// each pointing at the sequence resolveMatte derived for it.
	light, strong := rm, rm
	light.Stabilise, light.SeqDir = recipe.MatteStabiliseLight, filepath.Join(dir, "gated-isnet-anime-r3", "stab-light")
	strong.Stabilise, strong.SeqDir = recipe.MatteStabiliseStrong, filepath.Join(dir, "stab-strong")
	plan2, _ := graph.CompileWithSources(s.infos(), ops, recipe.Output{Format: "gif"})
	if err := s.fillExtraInputs(plan2, []resolvedMatte{strong, light, rm}); err != nil {
		t.Fatal(err)
	}
	want[recipe.MatteStabiliseLight] = filepath.Join(light.SeqDir, matte.FramePattern)
	for _, in := range plan2.ExtraInputs {
		if in.Path != want[in.Matte.Stabilise] {
			t.Errorf("stabilise %q paired with %s, want %s", in.Matte.Stabilise, in.Path, want[in.Matte.Stabilise])
		}
	}
	if got := findMatteInput([]resolvedMatte{strong, light}, &graph.MatteInput{Model: recipe.MatteModelISNetAnime, Stabilise: "light"}); got == nil || got.SeqDir != light.SeqDir {
		t.Errorf("findMatteInput(light) = %+v", got)
	}
	if got := findMatteInput([]resolvedMatte{strong}, &graph.MatteInput{Model: recipe.MatteModelISNetAnime}); got == nil || got.SeqDir != strong.SeqDir {
		t.Errorf("findMatteInput falls back to the model's resolution: %+v", got)
	}
	if got := findMatteInput([]resolvedMatte{strong}, &graph.MatteInput{Model: recipe.MatteModelBiRefNetLite}); got != nil {
		t.Errorf("findMatteInput of another model = %+v", got)
	}
}

// writeDerivedMatte writes a derived sequence of n flat gray mattes under
// the memo dir (the layout matte.go's derivation produces: <clipdir>/<name>/
// %06d.png) and returns its dir.
func writeDerivedMatte(t *testing.T, clipDir, name string, n, w, h int, gray func(i int) uint8) string {
	t.Helper()
	dir := store.MatteDerivedDir(clipDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		img := image.NewGray(image.Rect(0, 0, w, h))
		for p := range img.Pix {
			img.Pix[p] = gray(i)
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, matte.FrameFile(i+1)), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRenderMatteStabilisedSequence: a matte op with a stabilise mode reads
// the derived sequence under the memo dir — the render's frames, a still
// and the render.matte line all follow it, while the plain op keeps
// reading the raw memo. The derived dir is written by hand here (its
// derivation is matte.go's; this is the consumers' wiring). Skips without
// ffmpeg.
func TestRenderMatteStabilisedSequence(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	clip := e.lavfi("clip.mov", opaqueClip, "rgb24")
	frames := recipe.Output{Format: recipe.FormatFrames, FPS: 10}
	stab := []recipe.Op{matteOp(`{"stabilise":"light"}`)}
	dir := flatMemo(t, e.st, clip, stab, frames, ping, 10)
	derived := func(i int) uint8 { return matteGray(i) + 100 }
	writeDerivedMatte(t, dir, store.MatteStabName(recipe.MatteStabiliseLight), 10, 64, 48, derived)

	fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: stab, Output: frames})
	got := e.frameAlphas(fin, 3, 3)
	if len(got) != 10 {
		t.Fatalf("%d frames, want 10", len(got))
	}
	for i, a := range got {
		if a != derived(i) {
			t.Errorf("frame %d alpha %d, want the stabilised %d", i, a, derived(i))
		}
	}
	plain := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: []recipe.Op{matteOp("")}, Output: frames})
	if a := e.frameAlphas(plain, 3, 3); a[4] != matteGray(4) {
		t.Errorf("the plain op reads alpha %d, want the raw memo's %d", a[4], matteGray(4))
	}

	t.Run("still reads the derived sequence", func(t *testing.T) {
		out := recipe.Output{Format: "gif", FPS: 10}
		dir := flatMemo(t, e.st, clip, stab, out, ping, 10)
		writeDerivedMatte(t, dir, store.MatteStabName(recipe.MatteStabiliseLight), 10, 64, 48, derived)
		data, err := e.m.StillSources(e.ctx, []string{clip.Hash}, stab, out, 3.5/10, 0)
		if err != nil {
			t.Fatal(err)
		}
		pix, w, _ := pngPix(t, data)
		if a := pix[(3*w+3)*4+3]; a != derived(3) {
			t.Errorf("still 3 alpha %d, want the stabilised %d", a, derived(3))
		}
	})
	t.Run("report names the mode", func(t *testing.T) {
		out := recipe.Output{Format: recipe.FormatAPNG, FPS: 10}
		dir := flatMemo(t, e.st, clip, stab, out, ping, 10)
		writeDerivedMatte(t, dir, store.MatteStabName(recipe.MatteStabiliseLight), 10, 64, 48, derived)
		fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: stab, Output: out})
		rep := fin.Result.Files[0].Report
		if rep == nil {
			t.Fatal("no report")
		}
		found := false
		for _, c := range rep.Checks {
			if c.Rule == RuleRenderMatte {
				found = true
				if !c.OK || !strings.Contains(c.Detail, "stabilise light") {
					t.Errorf("render.matte check = %+v", c)
				}
			}
		}
		if !found {
			t.Errorf("no render.matte check in %+v", rep.Checks)
		}
	})
}

// TestCheckMatteResolvedPairsByRequest: a job's matte ops are paired with
// their resolutions by the whole request (model, size, stabilise, prompts,
// edge as written), not by model and size alone — two guided ops of one
// model that differ in the edge model resolve two identities (Edge /
// EdgeWeights / EdgeProc differ) and each must be compared with its own,
// or a render fails claiming the matte service changed although nothing
// did. A literal without the 5c fields still matches the 5b way.
func TestCheckMatteResolvedPairsByRequest(t *testing.T) {
	prompts := `[{"frame":0,"box":[0.25,0.25,0.75,0.75]}]`
	canon := graph.CanonicalMattePrompts([]recipe.MattePrompt{{Frame: 0, Box: &[4]float64{0.25, 0.25, 0.75, 0.75}}})
	tracker := matteIdentity{Model: recipe.MatteModelSAM2Tiny, Precision: "bf16", Weights: trackerWeights, Proc: "1", Kind: matte.KindTracker, Prompts: canon}
	withEdge := tracker
	withEdge.Edge = &matteIdentity{Model: recipe.MatteModelBiRefNetLite, Size: 16, Precision: "fp32", Weights: otherWeights, Proc: "1"}
	man := &matte.Manifest{Weights: trackerWeights, Proc: "1", Frames: 20}
	mattes := []resolvedMatte{
		{Model: recipe.MatteModelSAM2Tiny, Precision: "bf16", Prompts: canon, ReqEdge: "", Edge: recipe.MatteModelBiRefNetLite, Manifest: man, id: withEdge},
		{Model: recipe.MatteModelSAM2Tiny, Precision: "bf16", Prompts: canon, ReqEdge: recipe.MatteEdgeNone, Manifest: man, id: tracker},
	}
	resolvedJSON := func(id matteIdentity) string {
		b, err := json.Marshal(id.resolved())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	ops := []recipe.Op{
		matteOp(`{"model":"sam2-tiny","prompts":` + prompts + `,"resolved":` + resolvedJSON(withEdge) + `}`),
		matteOp(`{"model":"sam2-tiny","prompts":` + prompts + `,"edge":"none","resolved":` + resolvedJSON(tracker) + `}`),
	}
	if err := checkMatteResolved(ops, mattes); err != nil {
		t.Errorf("two guided ops differing in the edge: %v", err)
	}
	// The pairing is by request: the ops swapped still pass.
	if err := checkMatteResolved([]recipe.Op{ops[1], ops[0]}, mattes); err != nil {
		t.Errorf("the ops in the other order: %v", err)
	}
	// A real change is still caught: the edge weights of the first op's
	// resolution moved.
	moved := withEdge
	moved.Edge = &matteIdentity{Model: recipe.MatteModelBiRefNetLite, Size: 16, Precision: "fp32", Weights: testWeights, Proc: "1"}
	stale := []recipe.Op{matteOp(`{"model":"sam2-tiny","prompts":` + prompts + `,"resolved":` + resolvedJSON(moved) + `}`)}
	if err := checkMatteResolved(stale, mattes); err == nil || !strings.Contains(err.Error(), "changed since this render was submitted") {
		t.Errorf("a changed edge identity: %v", err)
	}
	// Literals without the 5c fields pair the 5b way (model and size).
	plain := []resolvedMatte{{Model: recipe.MatteModelISNetAnime, Size: 16, Precision: "fp32", Manifest: &matte.Manifest{Weights: testWeights, Proc: "1"}}}
	if rm := findMatteFor(plain, matteRequest{Model: recipe.MatteModelISNetAnime, Stabilise: recipe.MatteStabiliseLight}); rm == nil {
		t.Error("a stabilised request did not fall back to the 5b match")
	}
	if rm := findMatteFor(mattes, matteRequest{Model: recipe.MatteModelSAM2Tiny, Prompts: canon, Edge: recipe.MatteEdgeNone}); rm == nil || rm.ReqEdge != recipe.MatteEdgeNone {
		t.Errorf("findMatteFor(edge none) = %+v", rm)
	}
}
