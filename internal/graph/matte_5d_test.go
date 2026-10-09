package graph

// Phase 5d: the mask prompt (recipe.MattePrompt.MaskFrom) — the canonical
// "m<source>" token, a mask-only prompt compiling on the tracker, the mask
// changing the input's identity (two sequences), and the refusals (in
// TestCompileMatte5cErrors).

import (
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

func TestCanonicalMattePromptsMask(t *testing.T) {
	box := [4]float64{0.1, 0.2, 0.8, 0.9}
	prompts := []recipe.MattePrompt{
		{Frame: 7, Points: [][3]float64{{0.3, 0.4, 0}}},
		{Frame: 3, MaskFrom: recipe.MattePromptMaskEdge},
		{Frame: 0, Box: &box},
	}
	// The mask token follows the frame index and precedes the frame's
	// points and box; a prompt without a mask is the 5c text.
	const want = "f0;b0.1000,0.2000,0.8000,0.9000|f3;medge|f7;p0.3000,0.4000,0"
	if got := CanonicalMattePrompts(prompts); got != want {
		t.Errorf("CanonicalMattePrompts:\n got %s\nwant %s", got, want)
	}
	refined := []recipe.MattePrompt{{Frame: 3, MaskFrom: recipe.MattePromptMaskEdge, Box: &box, Points: [][3]float64{{0.5, 0.5, 0}}}}
	if got, w := CanonicalMattePrompts(refined), "f3;medge;p0.5000,0.5000,0;b0.1000,0.2000,0.8000,0.9000"; got != w {
		t.Errorf("mask with refinements:\n got %s\nwant %s", got, w)
	}
	// The source is what the token carries (the digest of the mask used is
	// the matte client's, in the clip key), and a mask changes the text.
	plain := []recipe.MattePrompt{{Frame: 3}}
	if CanonicalMattePrompts(plain) == CanonicalMattePrompts(prompts[1:2]) {
		t.Error("the mask left the canonical text alone")
	}
	if strings.Contains(CanonicalMattePrompts(prompts[2:]), "m") {
		t.Errorf("a box-only prompt grew a mask token: %s", CanonicalMattePrompts(prompts[2:]))
	}
}

func TestCompileMattePromptMask(t *testing.T) {
	const sam2 = recipe.MatteModelSAM2Tiny
	box := [4]float64{0.1, 0.2, 0.8, 0.9}
	masked := []recipe.MattePrompt{{Frame: 3, MaskFrom: recipe.MattePromptMaskEdge}}
	// A mask-only prompt is a positive prompt: the op compiles with the
	// default edge and with a named per-frame edge model, the filter text is
	// the plain matte's, and the input records the canonical prompts.
	for _, edge := range []string{"", recipe.MatteModelBiRefNetLite, recipe.MatteModelISNetAnime} {
		p := mustCompile(t, h264, []recipe.Op{matte(recipe.MatteParams{Model: sam2, Prompts: masked, Edge: edge})}, webp())
		plain := mustCompile(t, h264, []recipe.Op{matte(recipe.MatteParams{})}, webp())
		if p.Filter != plain.Filter {
			t.Errorf("edge %q: the mask prompt changed the filter text:\n got %s\nwant %s", edge, p.Filter, plain.Filter)
		}
		if len(p.ExtraInputs) != 1 || p.ExtraInputs[0].Matte == nil {
			t.Fatalf("edge %q: want one matte input, got %+v", edge, p.ExtraInputs)
		}
		if m := p.ExtraInputs[0].Matte; m.Model != sam2 || m.Edge != edge || m.Prompts != "f3;medge" {
			t.Errorf("edge %q: MatteInput %+v", edge, *m)
		}
	}
	// The mask beside clicks and a box on its frame, and prompts on other frames.
	full := []recipe.MattePrompt{
		{Frame: 12, Points: [][3]float64{{0.9, 0.1, 0}}},
		{Frame: 3, MaskFrom: recipe.MattePromptMaskEdge, Points: [][3]float64{{0.5, 0.5, 0}}, Box: &box},
	}
	fp := mustCompile(t, gifSrc, []recipe.Op{matte(recipe.MatteParams{Model: sam2, Prompts: full, Stabilise: recipe.MatteStabiliseLight})}, webp())
	if m := fp.ExtraInputs[0].Matte; m.Prompts != CanonicalMattePrompts(full) || !strings.Contains(m.Prompts, "f3;medge;p0.5000,0.5000,0;b") || m.Stabilise != recipe.MatteStabiliseLight {
		t.Errorf("MatteInput of the full set: %+v", *m)
	}
	// Two ops that differ only in the mask read two sequences (the mask is
	// part of the input's identity); identical mask ops share one.
	two := mustCompile(t, h264, []recipe.Op{
		matte(recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 3, Box: &box}}}),
		matte(recipe.MatteParams{Model: sam2, Prompts: []recipe.MattePrompt{{Frame: 3, Box: &box, MaskFrom: recipe.MattePromptMaskEdge}}}),
	}, webp())
	if len(two.ExtraInputs) != 2 {
		t.Errorf("a mask must not dedupe against the same box without it: %d inputs", len(two.ExtraInputs))
	}
	same := mustCompile(t, h264, []recipe.Op{
		matte(recipe.MatteParams{Model: sam2, Prompts: masked}),
		matte(recipe.MatteParams{Model: sam2, Prompts: masked}),
	}, webp())
	if len(same.ExtraInputs) != 1 {
		t.Errorf("identical mask ops must share one input: %d inputs", len(same.ExtraInputs))
	}
	// The detection plan compiles the same op (the autocrop sees the matte);
	// the matte input plan ignores it.
	if _, err := CompileDetectFor([]recipe.ProbeInfo{h264}, []recipe.Op{matte(recipe.MatteParams{Model: sam2, Prompts: masked})}, webp()); err != nil {
		t.Errorf("CompileDetectFor: %v", err)
	}
	if in, err := CompileMatteInput([]recipe.ProbeInfo{h264}, []recipe.Op{matte(recipe.MatteParams{Model: sam2, Prompts: masked})}, webp()); err != nil || len(in.ExtraInputs) != 0 {
		t.Errorf("CompileMatteInput: %v %+v", err, in)
	}
}
