package jobs

// Phase 5c through the pipeline's public surface — compile (the previews'
// entry), StillSources, Submit / the render job — against a memo on disk
// with no sidecar at all and against the Phase 5c fake sidecar with its
// tracker: the keep-colour op compiling through compile() into the union
// wrapper over the memo's input (no ffmpeg); the stabilised sequence derived
// ONCE by a plain still and reused by the render, the one-frame pop gone
// from the still's alpha and from the delivered WebP's while the plain op
// keeps the pop, keep colours showing on a still at once (ffmpeg); a guided
// render job running the tracker pass, the edge pass and the gate with no
// eager mark anywhere — the delivered frames' alpha reading core 255 / band
// = the edge matte / outside 0 —, a plain still following the running pass
// instead of answering idle, the job's Resolved identity and report, and the
// device preference moving the next render's identity, edge default and
// device= (ffmpeg + the fake). The ffmpeg tests skip without ffmpeg/ffprobe
// on PATH (realTools), the WebP one without FFmpeg 9.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// ---- no ffmpeg ----------------------------------------------------------------

// TestMatteKeepCompilesThroughCompile: a matte op with keep colours
// resolves to the same memo as the plain op (keep is picture-only: not in
// the clip key) and compiles, through the previews' compile, into the
// graph's union wrapper — one colorkey leg per colour at the op's
// similarity, lighten-blended into the merged alpha — over the memo's
// input, with the keep count on the resolution for the info line; a
// malformed keep is the compiler's refusal after the matte resolved.
func TestMatteKeepCompilesThroughCompile(t *testing.T) {
	st := newTestStore(t)
	ping := testPing(testWeights)
	writeMatteFacts(t, st, ping)
	srcHash := putSource(t, st, true) // 64x48, 25 fps, 2 s
	m := NewManager(st, fakeTools, Options{})
	s, err := m.resolveSources([]string{srcHash})
	if err != nil {
		t.Fatal(err)
	}
	src := s.main()
	ctx := context.Background()
	out := recipe.Output{Format: "gif", FPS: 10}
	keep := []recipe.Op{matteOp(`{"keep":["#DC1E1E","00ff00"],"keepSimilarity":0.2}`)}
	key, fps, frames := matteKeyFor(t, src, keep, out, ping, recipe.MatteModelISNetAnime, 0)
	if frames != 20 {
		t.Fatalf("matte plan frames = %d, want 20 (2 s at 10 fps)", frames)
	}
	dir := writeMatteMemo(t, st, key, src.Hash, ping, recipe.MatteModelISNetAnime, fps, frames, 8, 6, func(i, x, y int) uint8 { return 2 })

	plan, mattes, err := m.compile(ctx, s, keep, out)
	if err != nil {
		t.Fatalf("compile with keep colours: %v", err)
	}
	if len(mattes) != 1 || mattes[0].ClipKey != key || mattes[0].Keep != 2 || mattes[0].SeqDir != mattes[0].Dir || mattes[0].Manifest == nil {
		t.Fatalf("resolved = %+v", mattes)
	}
	for _, want := range []string{
		"colorkey=color=0xdc1e1e:similarity=0.2:blend=0,alphaextract,negate",
		"colorkey=color=0x00ff00:similarity=0.2:blend=0,alphaextract,negate",
		"blend=all_mode=lighten",
		"alphamerge",
	} {
		if !strings.Contains(plan.Filter, want) {
			t.Errorf("filter lacks %q:\n%s", want, plan.Filter)
		}
	}
	if n := strings.Count(plan.Filter, "colorkey="); n != 2 {
		t.Errorf("%d colorkey legs, want one per keep colour (2):\n%s", n, plan.Filter)
	}
	if len(plan.ExtraInputs) != 1 || plan.ExtraInputs[0].Matte == nil {
		t.Fatalf("plan inputs = %+v, want the one matte input", plan.ExtraInputs)
	}
	if in := plan.ExtraInputs[0]; in.Path != filepath.Join(dir, matte.FramePattern) || in.Matte.Frames != frames || in.Matte.Model != recipe.MatteModelISNetAnime {
		t.Errorf("matte input = %+v, want the memo %s with %d frames", in, dir, frames)
	}

	// The plain op shares the memo and compiles without a wrapper; the
	// default similarity is the graph's (0.08) when the op names none.
	plain, pm, err := m.compile(ctx, s, []recipe.Op{matteOp("")}, out)
	if err != nil || len(pm) != 1 || pm[0].ClipKey != key || pm[0].Keep != 0 || strings.Contains(plain.Filter, "colorkey") {
		t.Errorf("plain op: %v, resolved %+v, colorkey in filter %v", err, pm, strings.Contains(plain.Filter, "colorkey"))
	}
	one, _, err := m.compile(ctx, s, []recipe.Op{matteOp(`{"keep":["3a7bd5"]}`)}, out)
	if err != nil || !strings.Contains(one.Filter, "colorkey=color=0x3a7bd5:similarity=0.08:blend=0") {
		t.Errorf("one colour at the default similarity: %v\n%s", err, one.Filter)
	}
	// Stabilise + keep: the memo hit is followed by the derive, which is the
	// only thing that fails here (fakeTools' ffmpeg does not exist) — the
	// keep colours never stop a compile.
	if _, _, err := m.compile(ctx, s, []recipe.Op{matteOp(`{"keep":["3a7bd5"],"stabilise":"light"}`)}, out); err == nil || !strings.Contains(err.Error(), "deriving stab-light") || errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("keep + stabilise with a broken ffmpeg: %v, want the derive's own error", err)
	}
	// Malformed keep params are the client's mistake, reported by the
	// compiler after the matte resolved (the memo hit needs no keep).
	for _, bad := range []string{`{"keep":["zz"]}`, `{"keep":["ffffff80"]}`, `{"keep":["1","2","3","4","5","6","7"]}`, `{"keepSimilarity":2}`} {
		if _, _, err := m.compile(ctx, s, []recipe.Op{matteOp(bad)}, out); !errors.Is(err, ErrInvalidRecipe) {
			t.Errorf("compile of %s: %v, want ErrInvalidRecipe", bad, err)
		}
	}
	// The preview keys fold the clip key and the ops: a keep op and the
	// plain op share the memo but never a memoised still.
	ka, _ := stillKeyFor([]string{srcHash}, keep, stillOutput(out), 0.35, DefaultStillWidth, mattes)
	kb, _ := stillKeyFor([]string{srcHash}, []recipe.Op{matteOp("")}, stillOutput(out), 0.35, DefaultStillWidth, pm)
	if ka == kb {
		t.Error("the still key ignores the keep colours")
	}
}

// ---- ffmpeg: the stabilised sequence through stills and a render ---------------

// webpAlphas decodes the delivered animated WebP of fin with ffmpeg (FFmpeg
// 9's animated WebP demuxer; the caller checks the version) at the source's
// 64x48 and returns the alpha of pixel (x, y) of every frame, in order.
func (e *e2e) webpAlphas(fin Job, x, y int) []uint8 {
	e.t.Helper()
	name := ""
	for _, f := range fin.Result.Files {
		if f.Kind == FileKindOutput {
			name = f.Name
			break
		}
	}
	if name == "" {
		e.t.Fatalf("no output file in %+v", fin.Result.Files)
	}
	path := filepath.Join(e.st.ResultDir(fin.RecipeHash), name)
	raw, err := ffrun.RunOutput(e.ctx, e.tools.FFmpeg, append(append([]string{}, ffmpegPrefix...),
		"-i", path, "-vf", "scale=64:48:flags=neighbor", "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"))
	if err != nil {
		e.t.Fatalf("decode %s: %v", name, err)
	}
	const frame = 64 * 48 * 4
	if len(raw) == 0 || len(raw)%frame != 0 {
		e.t.Fatalf("%s decoded to %d bytes, not whole 64x48 rgba frames", name, len(raw))
	}
	var out []uint8
	for off := 0; off < len(raw); off += frame {
		out = append(out, raw[off+(y*64+x)*4+3])
	}
	return out
}

// TestMatteStabilisePipelineDerivesOnce: with a memo on disk that drops
// out for one frame (and no sidecar at all), a PLAIN still with stabilise
// light derives <clipdir>/stab-light/ — a derive is a cheap ffmpeg pass,
// never a model run, so it needs no eager mark — and shows the pop
// removed; the plain op's still keeps the pop; keep colours show at once on
// a still (in-graph) with and without stabilise; the render then reads the
// still's derived sequence (the files untouched: derived once) and the
// delivered WebP's alpha has no pop, while the plain op's WebP keeps it;
// the report names the mode.
func TestMatteStabilisePipelineDerivesOnce(t *testing.T) {
	ping := testPing(testWeights)
	e := matteE2E(t, ping, Options{Concurrency: 2})
	if ffmpegMajor(t, e.tools) < 9 {
		t.Skip("decoding the delivered animated WebP needs FFmpeg 9")
	}
	// A red square moving right over green: no two frames alike, so the
	// WebP encoder merges none and the decode gives one frame per frame.
	clip := e.lavfi("clip.mov", greenScreen, "rgb24")
	out := recipe.Output{Format: recipe.FormatWebP, FPS: 10}
	light := []recipe.Op{matteOp(`{"stabilise":"light"}`)}
	plain := []recipe.Op{matteOp("")}
	key, fps, _ := matteKeyFor(t, clip, light, out, ping, recipe.MatteModelISNetAnime, 0)
	const pop = 4 // the frame the matte drops out on
	dir := writeMatteMemo(t, e.st, key, clip.Hash, ping, recipe.MatteModelISNetAnime, fps, 10, 64, 48, func(i, x, y int) uint8 {
		if i == pop {
			return 0
		}
		return 200
	})
	derived := store.MatteDerivedDir(dir, store.MatteStabName(recipe.MatteStabiliseLight))
	stillAlpha := func(t *testing.T, ops []recipe.Op, i, x, y int) uint8 {
		t.Helper()
		data, err := e.m.StillSources(e.ctx, []string{clip.Hash}, ops, out, (float64(i)+0.5)/10, 0) // plain: no eager mark
		if err != nil {
			t.Fatalf("still %d of %s: %v", i, ops[0].Params, err)
		}
		pix, w, h := pngPix(t, data)
		if w != 64 || h != 48 {
			t.Fatalf("still is %dx%d", w, h)
		}
		return pix[(y*w+x)*4+3]
	}

	// A plain still derives and shows the pop removed.
	if a := stillAlpha(t, light, pop, 3, 3); a != 200 {
		t.Errorf("stabilised still at the pop: alpha %d, want 200 (the median of 200, 0, 200)", a)
	}
	if n := countSequence(derived); n != 10 {
		t.Fatalf("%d derived frames under %s, want 10", n, derived)
	}
	if _, err := os.Stat(store.MatteDerivedTmpDir(dir, store.MatteStabName(recipe.MatteStabiliseLight))); err == nil {
		t.Error("the derive's temp dir was left behind")
	}
	first, err := os.Stat(filepath.Join(derived, matte.FrameFile(1)))
	if err != nil {
		t.Fatal(err)
	}
	if a := stillAlpha(t, light, 0, 3, 3); a != 200 {
		t.Errorf("stabilised still 0: alpha %d, want 200 (the first frame passes through)", a)
	}
	// The plain op keeps the pop.
	if a := stillAlpha(t, plain, pop, 3, 3); a != 0 {
		t.Errorf("plain still at the pop: alpha %d, want the memo's 0", a)
	}
	// Keep colours show at once on a still: at t 0.4 the red square sits at
	// x 16..32, y 18..30, and is forced opaque while the background keeps
	// the matte's value — the pop without stabilise, 200 with it.
	keep := []recipe.Op{matteOp(`{"keep":["#DC1E1E"]}`)}
	if sq, bg := stillAlpha(t, keep, pop, 20, 22), stillAlpha(t, keep, pop, 3, 3); sq != 255 || bg != 0 {
		t.Errorf("keep still at the pop: square %d / background %d, want 255 / 0", sq, bg)
	}
	both := []recipe.Op{matteOp(`{"keep":["#DC1E1E"],"stabilise":"light"}`)}
	if sq, bg := stillAlpha(t, both, pop, 20, 22), stillAlpha(t, both, pop, 3, 3); sq != 255 || bg != 200 {
		t.Errorf("keep + stabilise still at the pop: square %d / background %d, want 255 / 200", sq, bg)
	}
	if n := countSequence(derived); n != 10 {
		t.Errorf("%d derived frames after the keep stills, want the same 10", n)
	}

	// The render reads what the still derived — the files untouched — and
	// the delivered WebP has no pop.
	time.Sleep(20 * time.Millisecond)
	fin := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: light, Output: out})
	if again, err := os.Stat(filepath.Join(derived, matte.FrameFile(1))); err != nil || !again.ModTime().Equal(first.ModTime()) {
		t.Errorf("the render derived the sequence again (%v)", err)
	}
	if _, tmps := matteDirs(t, e.st); len(tmps) != 0 {
		t.Errorf("tmp dirs after the render: %v", tmps)
	}
	alphas := e.webpAlphas(fin, 3, 3)
	if len(alphas) < 8 {
		t.Fatalf("the stabilised WebP decoded to %d frames, want 10", len(alphas))
	}
	for i, a := range alphas {
		if !near8(a, 200, 3) {
			t.Errorf("stabilised WebP frame %d: alpha %d, want ~200 (no pop)", i, a)
		}
	}
	rep := fin.Result.Files[0].Report
	if rep == nil {
		t.Fatal("no report on the WebP")
	}
	found := false
	for _, c := range rep.Checks {
		if c.Rule == RuleRenderMatte {
			found = true
			if !c.OK || !strings.Contains(c.Detail, "stabilise light") || !strings.Contains(c.Detail, "cpu") {
				t.Errorf("render.matte check = %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("no render.matte check in %+v", rep.Checks)
	}
	// Without stabilise the pop is delivered.
	raw := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: plain, Output: out})
	alphas = e.webpAlphas(raw, 3, 3)
	if len(alphas) != 10 {
		t.Fatalf("the plain WebP decoded to %d frames, want 10", len(alphas))
	}
	if !near8(alphas[pop], 0, 3) || !near8(alphas[pop-1], 200, 3) || !near8(alphas[pop+1], 200, 3) {
		t.Errorf("plain WebP alphas around the pop = %v, want 200, 0, 200 at frames %d..%d", alphas[pop-1:pop+2], pop-1, pop+1)
	}
}

// ---- ffmpeg + the fake tracker: a guided render job --------------------------------

// TestRenderGuidedJobViaSidecar: a render job of a guided (sam2-tiny) op
// runs the tracker pass, the edge model's pass and the gate with no eager
// mark anywhere (a render is explicit intent); a plain still before it is
// idle and starts nothing, a plain still DURING the track follows the
// running pass (never idle, never a track of its own); the delivered frames
// are gated — core 255, band = the edge matte, outside 0 — on every frame;
// the job's recipe carries the tracker and edge identities; the memo then
// serves a plain still and a second render (the report naming the guided
// model, its size, the edge and the device); and the device preference
// moved to cpu makes the next render a new key under the cpu identity
// (fp32, the cpu edge default, device=cpu on the wire).
func TestRenderGuidedJobViaSidecar(t *testing.T) {
	shortMatteWaits(t, 200*time.Millisecond, 2*time.Second, 50*time.Millisecond)
	e := newMatteRig5c(t, 1, Options{Concurrency: 2})
	e.f.set(func(f *fakeSidecar) { f.trackDelay = 1200 * time.Millisecond })
	clip := e.clipDistinct() // 32x32, 20 frames at 10 fps: tracked at 32x32 — box [8,24), core [11,21), outer [5,27)
	frames := recipe.Output{Format: recipe.FormatFrames, FPS: 10}
	still := recipe.Output{Format: "gif", FPS: 10} // the same plan rate: the same clip key
	guided := []recipe.Op{guidedOp("")}            // edge "" = the device's default per-frame model (birefnet-lite on cuda)
	r := recipe.Recipe{Sources: []string{clip.Hash}, Ops: guided, Output: frames}
	const core, band, outside = 16, 9, 2

	var pending *ErrMattePending
	if _, err := e.m.StillSources(e.ctx, []string{clip.Hash}, guided, still, 0.35, 0); !errors.As(err, &pending) || pending.State != MattePendingIdle || pending.Device != matte.DeviceCUDA {
		t.Fatalf("plain still before any pass: %v, want idle on cuda", err)
	}
	if tr, fr := e.f.trackStats(); tr != 0 || fr != 0 {
		t.Fatalf("an idle still tracked (%d tracks, %d frame masks)", tr, fr)
	}

	// The render starts the pass; a plain still arriving while the track
	// is being served follows it.
	j, err := e.m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	ch, unsub, _ := e.m.Subscribe(j.ID)
	defer unsub()
	waitFor(t, 20*time.Second, "the track request to reach the sidecar", func() bool { tr, _ := e.f.trackStats(); return tr >= 1 })
	start := time.Now()
	_, err = e.m.StillSources(e.ctx, []string{clip.Hash}, guided, still, 0.35, 0)
	took := time.Since(start)
	if !errors.As(err, &pending) || pending.State == MattePendingIdle || pending.Device != matte.DeviceCUDA {
		t.Fatalf("plain still during the track: %v, want pending running on cuda", err)
	}
	if took < mattePreviewWait || took > mattePreviewWait+1500*time.Millisecond {
		t.Errorf("the still answered pending after %s, want about the %s preview wait", took, mattePreviewWait)
	}
	evs := drain(t, ch)
	fin := evs[len(evs)-1].Job
	if fin.State != StateDone {
		t.Fatalf("job: %s %q (stage %s)", fin.State, fin.Error, fin.Stage)
	}
	msgs := stageMessages(evs, StageMatte)
	if len(msgs) == 0 {
		t.Fatalf("no event at stage %q in %d events", StageMatte, len(evs))
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "GPU") {
		t.Errorf("matte stage messages %q name no device", msgs)
	}
	tr, _ := e.f.trackStats()
	posts, fr := e.f.stats()
	if tr != 1 || posts != 3 || fr != 20 {
		t.Errorf("%d tracks, %d POSTs of %d frames; want 1 track + the edge pass (3 of 20)", tr, posts, fr)
	}
	for _, d := range e.f.seenDevices() {
		if d != matte.DeviceCUDA {
			t.Errorf("a request carried device=%q, want cuda", d)
		}
	}
	// The delivered frames are gated.
	cores, bands, outs := e.framePixels(fin, core, core), e.framePixels(fin, band, band), e.framePixels(fin, outside, outside)
	if len(cores) != 20 {
		t.Fatalf("%d frames, want 20", len(cores))
	}
	for i := range cores {
		rr, gg, bb := distinctColour(i)
		if cores[i][3] != 255 || !near8(cores[i][0], uint8(rr), 1) || !near8(cores[i][1], uint8(gg), 1) || !near8(cores[i][2], uint8(bb), 1) {
			t.Errorf("frame %d core = %v, want colour (%d,%d,%d) opaque", i, cores[i], rr, gg, bb)
		}
		if !near8(bands[i][3], distinctMatte(i), 1) {
			t.Errorf("frame %d band alpha %d, want the edge matte %d", i, bands[i][3], distinctMatte(i))
		}
		if outs[i][3] != 0 {
			t.Errorf("frame %d outside alpha %d, want 0", i, outs[i][3])
		}
	}
	res := matteResolvedOf(t, fin.Recipe.Ops)
	if res == nil || res.Tracker != trackerWeights || res.Edge != recipe.MatteModelBiRefNetLite || res.EdgeWeights != otherWeights || res.EdgeProc != "1" || res.Precision != "bf16" || res.Size != 0 {
		t.Errorf("job recipe Resolved = %+v", res)
	}

	// The memo serves a plain still (gated too) and a second render whose
	// report names it all; no new track, no new POST.
	data, err := e.m.StillSources(e.ctx, []string{clip.Hash}, guided, still, 0.35, 0)
	if err != nil {
		t.Fatalf("plain still after the render: %v", err)
	}
	pix, w, _ := pngPix(t, data)
	if a, o := pix[(core*w+core)*4+3], pix[(outside*w+outside)*4+3]; a != 255 || o != 0 {
		t.Errorf("still after the render: core alpha %d / outside %d, want 255 / 0", a, o)
	}
	apng := e.run(recipe.Recipe{Sources: []string{clip.Hash}, Ops: guided, Output: recipe.Output{Format: recipe.FormatAPNG, FPS: 10}})
	rep := apng.Result.Files[0].Report
	if rep == nil {
		t.Fatal("no report on the APNG")
	}
	detail := ""
	for _, c := range rep.Checks {
		if c.Rule == RuleRenderMatte {
			detail = c.Detail
		}
	}
	for _, want := range []string{"guided (sam2-tiny) + edge birefnet-lite · 32x32 bf16", "edge weights " + short(otherWeights), "1 prompted frame", "cuda"} {
		if !strings.Contains(detail, want) {
			t.Errorf("render.matte detail %q lacks %q", detail, want)
		}
	}
	if tr, _ := e.f.trackStats(); tr != 1 {
		t.Errorf("%d tracks after the memo hits, want 1", tr)
	}
	if p, _ := e.f.stats(); p != 3 {
		t.Errorf("%d POSTs after the memo hits, want 3", p)
	}

	// The preference moved to cpu: the next render is a new key under the
	// cpu identity, its edge the cpu default, tracked and matted on cpu.
	e.f.set(func(f *fakeSidecar) { f.trackDelay = 0 })
	if err := e.m.SetMatteDevice(e.ctx, matte.DeviceCPU); err != nil {
		t.Fatal(err)
	}
	j2, err := e.m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	if j2.RecipeHash == fin.RecipeHash {
		t.Fatal("the device preference did not change the result key (the identity differs per device)")
	}
	if res2 := matteResolvedOf(t, j2.Recipe.Ops); res2 == nil || res2.Precision != "fp32" || res2.Edge != recipe.MatteModelISNetAnime || res2.EdgeWeights != testWeights || res2.Tracker != trackerWeights {
		t.Errorf("Resolved on cpu = %+v", res2)
	}
	fin2 := waitJob(t, e.m, j2.ID, 60*time.Second)
	if fin2.State != StateDone {
		t.Fatalf("job on cpu: %s %q (stage %s)", fin2.State, fin2.Error, fin2.Stage)
	}
	if tr, _ := e.f.trackStats(); tr != 2 {
		t.Errorf("%d tracks after the cpu render, want 2", tr)
	}
	if p, fr := e.f.stats(); p != 6 || fr != 40 {
		t.Errorf("%d POSTs of %d frames after the cpu render, want the isnet edge pass on top (6 of 40)", p, fr)
	}
	seen := e.f.seenDevices()
	for _, d := range seen[len(seen)-4:] { // the track and the three edge batches
		if d != matte.DeviceCPU {
			t.Errorf("a cpu-render request carried device=%q", d)
		}
	}
	cores2, outs2 := e.framePixels(fin2, core, core), e.framePixels(fin2, outside, outside)
	if len(cores2) != 20 || cores2[7][3] != 255 || outs2[7][3] != 0 {
		t.Errorf("cpu render frame 7: core %v / outside %v", cores2[min(7, len(cores2)-1)], outs2[min(7, len(outs2)-1)])
	}
	if memos, tmps := matteDirs(t, e.st); len(memos) != 4 || len(tmps) != 0 {
		t.Errorf("memo dirs %v (want the two tracker memos and the two edge memos), tmp dirs %v", memos, tmps)
	}
}
