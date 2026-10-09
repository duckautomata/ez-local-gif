package graph

// Phase 5c goldens: the matte op's Stabilise / Prompts / Edge params are
// validated and recorded on MatteInput without changing the filter text,
// CanonicalMattePrompts is deterministic, the keep colours emit one union
// wrapper each behind the merge (pixel-checked against a real ffmpeg in
// matte_keep_ffmpeg_test.go), and the guided-model rules (prompts, edge)
// are enforced.

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// keepUnion is the wrapper keepColour emits for the n-th keep colour of a
// recipe: it joins the chain in place of a stage — "<merge>," +
// keepUnion(…) + ",<rest>" — closing the merged frame into a three-way
// split, keying the colour on one copy into a mask (255 within the
// similarity), extracting the merged alpha from another, taking the
// maximum and merging it back.
func keepUnion(n int, hex, sim string) string {
	u := "u" + strconv.Itoa(n)
	return "split=3[" + u + "][" + u + "m][" + u + "e];" +
		"[" + u + "m]colorkey=color=0x" + hex + ":similarity=" + sim + ":blend=0,alphaextract,negate[" + u + "k];" +
		"[" + u + "e]alphaextract[" + u + "a];" +
		"[" + u + "a][" + u + "k]blend=all_mode=lighten[" + u + "x];" +
		"[" + u + "][" + u + "x]alphamerge"
}

func TestCanonicalMattePrompts(t *testing.T) {
	box := [4]float64{0.1, 0.2, 0.8, 0.9}
	prompts := []recipe.MattePrompt{
		{Frame: 7, Points: [][3]float64{{0.3, 0.4, 0}}},
		{Frame: 0, Box: &box, Points: [][3]float64{{0.5, 0.5, 1}, {0.25, 0.75, 1}}},
		{Frame: 7, Box: &[4]float64{0, 0, 1, 1}},
	}
	const want = "f0;p0.5000,0.5000,1;p0.2500,0.7500,1;b0.1000,0.2000,0.8000,0.9000|f7;p0.3000,0.4000,0|f7;b0.0000,0.0000,1.0000,1.0000"
	if got := CanonicalMattePrompts(prompts); got != want {
		t.Errorf("CanonicalMattePrompts:\n got %s\nwant %s", got, want)
	}
	// Sorting never reorders the caller's slice, and the result does not
	// depend on the input order (the stable sort keeps equal frames in
	// order, which is part of the text, so only distinct frames are swapped).
	if prompts[0].Frame != 7 {
		t.Errorf("CanonicalMattePrompts mutated its input")
	}
	swapped := []recipe.MattePrompt{prompts[1], prompts[0], prompts[2]}
	if got := CanonicalMattePrompts(swapped); got != want {
		t.Errorf("CanonicalMattePrompts depends on the input order:\n got %s\nwant %s", got, want)
	}
	if got := CanonicalMattePrompts(nil); got != "" {
		t.Errorf("CanonicalMattePrompts(nil) = %q, want \"\"", got)
	}
}

func TestCompileMatte5cParams(t *testing.T) {
	const isnet = recipe.MatteModelISNetAnime
	box := [4]float64{0.1, 0.2, 0.8, 0.9}
	prompts := []recipe.MattePrompt{{Frame: 3, Box: &box}}
	p := mustCompile(t, h264, []recipe.Op{matte(recipe.MatteParams{
		Model: recipe.MatteModelSAM2Tiny, Stabilise: recipe.MatteStabiliseLight,
		Prompts: prompts, Edge: recipe.MatteEdgeNone,
	})}, webp())
	// The filter text is the 5b one: the params only pick the sequence
	// (the tracker needs its prompts; nothing else differs from a plain
	// per-frame matte).
	base := mustCompile(t, h264, []recipe.Op{matte(recipe.MatteParams{Model: recipe.MatteModelSAM2Tiny, Prompts: prompts})}, webp())
	plain := mustCompile(t, h264, []recipe.Op{matte(recipe.MatteParams{})}, webp())
	if p.Filter != base.Filter || p.Filter != plain.Filter {
		t.Errorf("5c params changed the filter text:\n got %s\nbase %s\nplain %s", p.Filter, base.Filter, plain.Filter)
	}
	if len(p.ExtraInputs) != 1 || p.ExtraInputs[0].Matte == nil {
		t.Fatalf("want one matte input, got %+v", p.ExtraInputs)
	}
	m := p.ExtraInputs[0].Matte
	if m.Model != recipe.MatteModelSAM2Tiny || m.Stabilise != recipe.MatteStabiliseLight || m.Edge != recipe.MatteEdgeNone || m.Prompts != CanonicalMattePrompts(prompts) {
		t.Errorf("MatteInput did not record the 5c params: %+v", *m)
	}

	// A full prompt set — boxes, keep and remove points, unsorted frames, a
	// prompt made of points only — compiles and is recorded canonically.
	full := []recipe.MattePrompt{
		{Frame: 12, Points: [][3]float64{{0.5, 0.5, 1}, {0.9, 0.1, 0}}},
		{Frame: 0, Box: &box, Points: [][3]float64{{0.2, 0.2, 0}}},
		{Frame: 4, Points: [][3]float64{{0.4, 0.6, 0}}}, // removals only on this frame are fine: the set has positives
	}
	fp := mustCompile(t, gifSrc, []recipe.Op{matte(recipe.MatteParams{Model: recipe.MatteModelSAM2Tiny, Prompts: full, Edge: recipe.MatteModelBiRefNetLite})}, webp())
	if fm := fp.ExtraInputs[0].Matte; fm.Prompts != CanonicalMattePrompts(full) || fm.Edge != recipe.MatteModelBiRefNetLite || fm.Stabilise != "" {
		t.Errorf("MatteInput of the full prompt set: %+v", *fm)
	}

	// Two matte ops that differ only in a 5c param read two sequences.
	two := mustCompile(t, h264, []recipe.Op{
		matte(recipe.MatteParams{Model: isnet}),
		matte(recipe.MatteParams{Model: isnet, Stabilise: recipe.MatteStabiliseStrong}),
	}, webp())
	if len(two.ExtraInputs) != 2 {
		t.Errorf("stabilise must not dedupe against the raw sequence: %d inputs", len(two.ExtraInputs))
	}
	same := mustCompile(t, h264, []recipe.Op{
		matte(recipe.MatteParams{Model: isnet, Stabilise: recipe.MatteStabiliseLight}),
		matte(recipe.MatteParams{Model: isnet, Stabilise: recipe.MatteStabiliseLight}),
	}, webp())
	if len(same.ExtraInputs) != 1 {
		t.Errorf("identical ops must share one input: %d inputs", len(same.ExtraInputs))
	}
}

// --- keep colours -----------------------------------------------------------

func TestCompileMatteKeep(t *testing.T) {
	const isnet = recipe.MatteModelISNetAnime
	box := [4]float64{0.1, 0.2, 0.8, 0.9}
	prompts := []recipe.MattePrompt{{Frame: 3, Box: &box}}
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want Plan // OutLabel implied; Speed 0 means 1; SourceFPS 0 means the main source's
	}{
		{
			// The whole text once: the merge as before, then the union — the
			// merged frame split three ways, the colour keyed on one copy and
			// its alpha negated into the keep mask, the merged alpha
			// extracted from another, the maximum merged back. The input is
			// the plain one: Keep is picture, not sequence.
			name: "one keep colour follows the opaque merge",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{Keep: []string{"0a14c8"}})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + keepUnion(1, "0a14c8", "0.08") + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			// One wrapper per colour, in the order given, all at the op's
			// similarity; '#' and upper case are normalised like a key's
			// colour; the wrapped merge (a transparent source) is the same
			// rgba frame to the union.
			name: "two colours at a custom similarity behind the wrapped merge",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{matte(recipe.MatteParams{Keep: []string{"#0A14C8", "C80A0A"}, KeepSimilarity: 0.15})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down," + matteWrapped(1, 1, 480, 270) + "," + keepUnion(1, "0a14c8", "0.15") + "," + keepUnion(2, "c80a0a", "0.15") + ",format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "20")},
			},
		},
		{
			// Stack order: the cleanup acts on the union's alpha.
			name: "keep, then morph and feather follow the union",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{Keep: []string{"ffffff"}}), morph(recipe.MorphParams{Close: true}), feather(0)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + keepUnion(1, "ffffff", "0.08") + "," + morphClose + "," + featherDef + "[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			name: "a colorkey behind the keep is wrapped: the union's alpha is intersected, not overwritten",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{Keep: []string{"0a14c8"}}), colorkey(recipe.ColorKeyParams{Color: "ffffff"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + keepUnion(1, "0a14c8", "0.08") + "," + keyWrapped(1, ckWhite) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			// Keep is not part of the dedupe key (it changes the keyed
			// picture, not the sequence): both ops read input 1, the second
			// merge is wrapped behind the first union, the wrappers are
			// numbered across the recipe.
			name: "two matte ops with different keeps share one input",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{Keep: []string{"0a14c8"}}), matte(recipe.MatteParams{Keep: []string{"c80a0a"}})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + keepUnion(1, "0a14c8", "0.08") + "," + matteWrapped(2, 1, 1280, 720) + "," + keepUnion(2, "c80a0a", "0.08") + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			// The guided model with every 5c param: the input carries the
			// sequence-picking ones, the union follows the merge as for any
			// model.
			name: "keep on the guided model with stabilise and edge",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{Model: recipe.MatteModelSAM2Tiny, Prompts: prompts, Stabilise: recipe.MatteStabiliseLight, Edge: recipe.MatteEdgeNone, Keep: []string{"0a14c8"}})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + keepUnion(1, "0a14c8", "0.08") + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{{
					Source: 0,
					Args:   []string{"-f", "image2", "-framerate", "29.97", "-start_number", "1"},
					Matte:  &MatteInput{Model: recipe.MatteModelSAM2Tiny, FPS: "29.97", Stabilise: recipe.MatteStabiliseLight, Prompts: CanonicalMattePrompts(prompts), Edge: recipe.MatteEdgeNone},
				}},
			},
		},
		{
			// The knob is validated whatever the colours; without colours
			// nothing is emitted (no wrapper, the plain merge).
			name: "a keepSimilarity without colours emits nothing",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{KeepSimilarity: 0.5})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileWithSources(tc.srcs, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("CompileWithSources: %v", err)
			}
			want := tc.want
			want.OutLabel = "[out]"
			if want.Speed == 0 {
				want.Speed = 1
			}
			if want.SourceFPS == 0 {
				want.SourceFPS = tc.srcs[0].FPS
			}
			want.SourceVFR = tc.srcs[0].Kind == recipe.KindAnimation
			want.SeekUnsafe = seekUnsafeDemuxer(tc.srcs[0].Format)
			checkPlan3(t, got, &want)
			checkMatteInputs(t, got)
			if !reflect.DeepEqual(got.ExtraInputs, want.ExtraInputs) {
				t.Errorf("ExtraInputs\n got: %+v\nwant: %+v", got.ExtraInputs, want.ExtraInputs)
			}
			// The detection plan applies the matte op like the render, union
			// included (a kept colour is content the autocrop must see);
			// the matte input plan ignores the op's params altogether.
			det, err := CompileDetectFor(tc.srcs, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("CompileDetectFor: %v", err)
			}
			if keeps := strings.Count(det.Filter, "]alphaextract,negate["); keeps != strings.Count(got.Filter, "]alphaextract,negate[") {
				t.Errorf("detection plan has %d keep wrappers, the render %d:\n%s", keeps, strings.Count(got.Filter, "]alphaextract,negate["), det.Filter)
			}
			in, err := CompileMatteInput(tc.srcs, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("CompileMatteInput: %v", err)
			}
			if strings.Contains(in.Filter, "colorkey") || strings.Contains(in.Filter, "alphamerge") || len(in.ExtraInputs) != 0 {
				t.Errorf("the matte input plan must not key or merge: %s %+v", in.Filter, in.ExtraInputs)
			}
		})
	}
}

// --- errors -----------------------------------------------------------------

func TestCompileMatte5cErrors(t *testing.T) {
	const sam2 = recipe.MatteModelSAM2Tiny
	box := [4]float64{0.1, 0.2, 0.8, 0.9}
	okPrompt := []recipe.MattePrompt{{Frame: 0, Box: &box}}
	many := make([]recipe.MattePrompt, MaxMattePrompts+1)
	for i := range many {
		many[i] = recipe.MattePrompt{Frame: i, Box: &box}
	}
	sevenKeeps := make([]string, recipe.MaxMatteKeep+1)
	for i := range sevenKeeps {
		sevenKeeps[i] = "ffffff"
	}
	tests := []struct {
		name string
		p    recipe.MatteParams
		want string // substring of the error, after "graph: "
	}{
		// keep
		{"seven keep colours", recipe.MatteParams{Keep: sevenKeeps}, "op 0 (matte): at most 6 keep colours (got 7)"},
		{"blank keep colour", recipe.MatteParams{Keep: []string{"ffffff", " "}}, "op 0 (matte): keep[1]: colour is required (RRGGBB"},
		{"keep colour not hex", recipe.MatteParams{Keep: []string{"zzzzzz"}}, `op 0 (matte): keep[0]: colour "zzzzzz": not hex`},
		{"keep colour with alpha", recipe.MatteParams{Keep: []string{"ffffff80"}}, `op 0 (matte): keep[0]: key colour "ffffff80" must be RRGGBB (no alpha)`},
		{"keepSimilarity below the floor", recipe.MatteParams{Keep: []string{"ffffff"}, KeepSimilarity: 0.005}, "op 0 (matte): keepSimilarity must be between 0.01 and 1 (got 0.005)"},
		{"keepSimilarity above 1, even without colours", recipe.MatteParams{KeepSimilarity: 1.5}, "op 0 (matte): keepSimilarity must be between 0.01 and 1 (got 1.5)"},
		// stabilise / edge
		{"stabilise", recipe.MatteParams{Stabilise: "medium"}, `op 0 (matte): stabilise must be "", "light" or "strong" (got "medium")`},
		{"edge not an id", recipe.MatteParams{Model: sam2, Prompts: okPrompt, Edge: "no such/model"}, `op 0 (matte): edge must be "", "none" or a model id (got "no such/model")`},
		{"edge on a per-frame model", recipe.MatteParams{Model: recipe.MatteModelBiRefNetLite, Edge: recipe.MatteEdgeNone}, `op 0 (matte): edge needs the guided model "sam2-tiny" (got model "birefnet-lite")`},
		{"the tracker as its own edge", recipe.MatteParams{Model: sam2, Prompts: okPrompt, Edge: sam2}, `op 0 (matte): edge must be a per-frame model or "none", not the tracker "sam2-tiny" itself`},
		// prompts
		{"prompts on a per-frame model", recipe.MatteParams{Model: recipe.MatteModelISNetAnime, Prompts: okPrompt}, `op 0 (matte): prompts need the guided model "sam2-tiny" (got model "isnet-anime")`},
		{"prompts on the default model", recipe.MatteParams{Prompts: okPrompt}, `op 0 (matte): prompts need the guided model "sam2-tiny" (got model "isnet-anime")`},
		{"guided without prompts", recipe.MatteParams{Model: sam2}, `op 0 (matte): the guided model "sam2-tiny" needs at least one prompt`},
		{"too many prompts", recipe.MatteParams{Model: sam2, Prompts: many}, "op 0 (matte): at most 32 prompts (got 33)"},
		{"negative frame", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: -1, Box: &box}}}, "op 0 (matte): prompts[0]: frame must be >= 0 (got -1)"},
		{"empty prompt", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Box: &box}, {Frame: 3}}}, "op 0 (matte): prompts[1]: a prompt needs a box, at least one point or a mask (maskFrom)"},
		{"box out of range", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Box: &[4]float64{0, 0, 1.5, 1}}}}, "op 0 (matte): prompts[0]: box[2] must be between 0 and 1 (got 1.5)"},
		{"inverted box", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Box: &[4]float64{0.8, 0.2, 0.1, 0.9}}}}, "op 0 (matte): prompts[0]: box must have x0 < x1 and y0 < y1 (got 0.8,0.2,0.1,0.9)"},
		{"empty box", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Box: &[4]float64{0.5, 0.2, 0.5, 0.9}}}}, "op 0 (matte): prompts[0]: box must have x0 < x1 and y0 < y1 (got 0.5,0.2,0.5,0.9)"},
		{"point out of range", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Points: [][3]float64{{1.2, 0.5, 1}}}}}, "op 0 (matte): prompts[0]: points[0] must be between 0 and 1 (got 1.2,0.5)"},
		{"negative point coordinate", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Points: [][3]float64{{0.5, -0.1, 1}}}}}, "op 0 (matte): prompts[0]: points[0] must be between 0 and 1 (got 0.5,-0.1)"},
		{"bad label", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Points: [][3]float64{{0.5, 0.5, 2}}}}}, "op 0 (matte): prompts[0]: points[0] label must be 0 (remove) or 1 (keep) (got 2)"},
		{"fractional label", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Points: [][3]float64{{0.5, 0.5, 0.5}}}}}, "op 0 (matte): prompts[0]: points[0] label must be 0 (remove) or 1 (keep) (got 0.5)"},
		{"only removals", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, Points: [][3]float64{{0.5, 0.5, 0}}}, {Frame: 2, Points: [][3]float64{{0.1, 0.1, 0}}}}}, "op 0 (matte): prompts: at least one box, positive (keep) point or mask is needed"},
		// maskFrom (Phase 5d)
		{"maskFrom unknown", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, MaskFrom: "sam"}}}, `op 0 (matte): prompts[0]: maskFrom must be "" or "edge" (got "sam")`},
		{"maskFrom with edge none", recipe.MatteParams{Model: sam2, Edge: recipe.MatteEdgeNone, Prompts: []recipe.MattePrompt{{Frame: 0, MaskFrom: recipe.MattePromptMaskEdge}}}, `op 0 (matte): prompts[0]: maskFrom "edge" needs an edge model, but edge is "none"`},
		{"two mask prompts", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, MaskFrom: recipe.MattePromptMaskEdge}, {Frame: 4, Box: &box, MaskFrom: recipe.MattePromptMaskEdge}}}, "op 0 (matte): prompts[1]: a second mask prompt (prompts[0] has one): one mask per matte op"},
		{"maskFrom on a per-frame model", recipe.MatteParams{Model: recipe.MatteModelBiRefNetLite, Prompts: []recipe.MattePrompt{{Frame: 0, MaskFrom: recipe.MattePromptMaskEdge}}}, `op 0 (matte): prompts need the guided model "sam2-tiny" (got model "birefnet-lite")`},
		{"maskFrom with a bad box still checks the box", recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 0, MaskFrom: recipe.MattePromptMaskEdge, Box: &[4]float64{0.8, 0.2, 0.1, 0.9}}}}, "op 0 (matte): prompts[0]: box must have x0 < x1 and y0 < y1 (got 0.8,0.2,0.1,0.9)"},
		// a later error still names the op by its index
		{"index kept", recipe.MatteParams{Keep: []string{"nope"}}, "op 1 (matte): keep[0]: colour \"nope\": want RRGGBB or RRGGBBAA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ops := []recipe.Op{matte(tc.p)}
			if strings.HasPrefix(tc.want, "op 1") {
				ops = []recipe.Op{crop(0, 0, 10, 10), matte(tc.p)}
			}
			for name, compile := range map[string]func() (*Plan, error){
				"Compile":       func() (*Plan, error) { return CompileWithSources([]recipe.ProbeInfo{h264}, ops, webp()) },
				"CompileDetect": func() (*Plan, error) { return CompileDetect([]recipe.ProbeInfo{h264}, ops) },
			} {
				p, err := compile()
				if err == nil {
					t.Fatalf("%s: expected error containing %q, got plan %+v", name, tc.want, p)
				}
				if !strings.HasPrefix(err.Error(), "graph: ") || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: error %q does not contain %q", name, err.Error(), tc.want)
				}
			}
			// The matte input plan ignores the matte op (params unread).
			if _, err := CompileMatteInput([]recipe.ProbeInfo{h264}, ops, webp()); err != nil {
				t.Errorf("CompileMatteInput must ignore the matte op: %v", err)
			}
		})
	}
}
