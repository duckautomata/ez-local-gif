package recipe

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMattePromptJSON: the matte op's prompt shape on the wire — maskFrom
// (Phase 5d) is written only when set and reads back; a prompt without it
// serialises exactly as before.
func TestMattePromptJSON(t *testing.T) {
	box := [4]float64{0.1, 0.2, 0.8, 0.9}
	p := MatteParams{Model: MatteModelSAM2Tiny, Prompts: []MattePrompt{
		{Frame: 0, Box: &box, Points: [][3]float64{{0.5, 0.5, 1}}},
		{Frame: 3, MaskFrom: MattePromptMaskEdge},
	}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"model":"sam2-tiny","prompts":[{"frame":0,"points":[[0.5,0.5,1]],"box":[0.1,0.2,0.8,0.9]},{"frame":3,"maskFrom":"edge"}]}`
	if string(data) != want {
		t.Errorf("MatteParams JSON:\n got %s\nwant %s", data, want)
	}
	var back MatteParams
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Prompts) != 2 || back.Prompts[1].MaskFrom != MattePromptMaskEdge || back.Prompts[1].Frame != 3 || back.Prompts[0].MaskFrom != "" {
		t.Errorf("round trip: %+v", back.Prompts)
	}
	// The SPA's own form of the mask prompt decodes to the same value.
	var spa MattePrompt
	if err := json.Unmarshal([]byte(`{"frame":3,"maskFrom":"edge"}`), &spa); err != nil || spa.Frame != 3 || spa.MaskFrom != "edge" || spa.Box != nil || spa.Points != nil {
		t.Errorf("SPA prompt: %v %+v", err, spa)
	}
	if MattePromptMaskEdge != "edge" {
		t.Errorf("MattePromptMaskEdge = %q", MattePromptMaskEdge)
	}
	// Canonical (the recipe hash's text) carries the field as any other.
	r := Recipe{Sources: []string{strings.Repeat("ab", 32)}, Ops: []Op{{Kind: OpMatte, Params: json.RawMessage(`{"model":"sam2-tiny","prompts":[{"frame":3,"maskFrom":"edge"}]}`)}}}
	canon, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canon), `"maskFrom":"edge"`) {
		t.Errorf("Canonical dropped maskFrom: %s", canon)
	}
}
