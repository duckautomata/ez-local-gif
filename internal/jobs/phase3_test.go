package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Unit tests of the Phase 3 plumbing that need no ffmpeg: autocrop
// parameter handling and memo, source resolution rules, proxy bounds/keys,
// the font cache and the in-flight de-duplication. The real-ffmpeg checks
// live in phase3_e2e_test.go.

// autocropOp builds an autocrop op with the given params JSON.
func autocropOp(params string) recipe.Op {
	return recipe.Op{Kind: recipe.OpAutoCrop, Params: json.RawMessage(params)}
}

// putSequenceSource stores a two-frame PNG sequence with probe info and
// returns its hash.
func putSequenceSource(t *testing.T, st *store.Store) string {
	t.Helper()
	parts := []store.SequencePart{
		{Name: "a1.png", R: bytes.NewReader(solidPNG(t, 16, 12, color.NRGBA{R: 200, A: 255}))},
		{Name: "a2.png", R: bytes.NewReader(solidPNG(t, 16, 12, color.NRGBA{B: 200, A: 255}))},
	}
	b, err := st.PutSequence(parts)
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "image2", Codec: "png", PixFmt: "rgb24", Bits: 8, Width: 16, Height: 12, FPS: 10, Duration: 0.2, Frames: 2,
		Kind: recipe.KindSequence, Sequence: &recipe.SequenceInfo{Count: 2, Pattern: "%06d.png", DelayMS: 100}}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	return b.Hash
}

func TestStripAutoCropResolved(t *testing.T) {
	plain := []recipe.Op{{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":1}`)}, autocropOp(`{"threshold":8,"padding":2}`)}
	if got := stripAutoCropResolved(plain); len(got) != 2 || &got[0] != &plain[0] {
		t.Error("ops without a resolved box must be returned as the same slice")
	}
	withBox := []recipe.Op{plain[0], autocropOp(`{"threshold":8,"padding":2,"resolved":{"x":1,"y":2,"w":3,"h":4}}`), {Kind: recipe.OpReverse}}
	orig := string(withBox[1].Params)
	got := stripAutoCropResolved(withBox)
	if string(withBox[1].Params) != orig {
		t.Error("input slice was modified")
	}
	if len(got) != 3 || got[0].Kind != recipe.OpTrim || got[2].Kind != recipe.OpReverse {
		t.Fatalf("stripped ops = %+v", got)
	}
	var p recipe.AutoCropParams
	if err := json.Unmarshal(got[1].Params, &p); err != nil || p.Resolved != nil || p.Threshold != 8 || p.Padding != 2 {
		t.Errorf("stripped params = %s (%v)", got[1].Params, err)
	}
	// Malformed params are left for the compiler to report.
	bad := []recipe.Op{autocropOp(`{"resolved":`)}
	if got := stripAutoCropResolved(bad); string(got[0].Params) != `{"resolved":` {
		t.Errorf("malformed params changed: %s", got[0].Params)
	}

	// Submit hashes the stripped recipe: a client-supplied box never changes
	// the result key or reaches the job's recipe.
	st := newTestStore(t)
	src := putSource(t, st, true)
	m := NewManager(st, ffrun.Tools{}, Options{})
	r := recipe.Recipe{Sources: []string{src}, Ops: withBox, Output: recipe.Output{Format: "gif"}}
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	want := recipe.Recipe{Sources: []string{src}, Ops: got, Output: r.Output}
	if j.RecipeHash != ResultKey(want) || j.RecipeHash == ResultKey(r) {
		t.Errorf("submitted key %s; want the stripped recipe's %s (raw %s)", short(j.RecipeHash), short(ResultKey(want)), short(ResultKey(r)))
	}
	if bytes.Contains(j.Recipe.Ops[1].Params, []byte("resolved")) {
		t.Errorf("job recipe still carries the client box: %s", j.Recipe.Ops[1].Params)
	}
	waitFinished(t, m, j.ID)
}

func TestDecodeAutoCropAndDetectionOps(t *testing.T) {
	p, err := decodeAutoCrop(0, autocropOp(``))
	if err != nil || p.Threshold != 1 || p.Padding != 0 || p.Resolved != nil {
		t.Errorf("defaults: %+v %v", p, err)
	}
	p, err = decodeAutoCrop(0, autocropOp(`{"threshold":128,"padding":4,"resolved":{"x":1,"y":1,"w":1,"h":1}}`))
	if err != nil || p.Threshold != 128 || p.Padding != 4 || p.Resolved != nil {
		t.Errorf("client box must be dropped: %+v %v", p, err)
	}
	for _, bad := range []string{`{"threshold":300}`, `{"threshold":-1}`, `{"padding":-1}`, `{"padding":2000}`, `{bad`} {
		if _, err := decodeAutoCrop(3, autocropOp(bad)); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "op 3") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}

	// The detection reads every op of the stack that shapes the picture the
	// crop is applied to (in stack order) — the keying ops above all —
	// wherever it sits relative to the autocrop, because the compiler hoists
	// these kinds in front of the geometry; and it skips what the crop
	// cannot see: reverse, text, overlays, and geometry behind the autocrop.
	kinds := func(ops []recipe.Op) string {
		var out []string
		for _, op := range ops {
			out = append(out, op.Kind)
		}
		return strings.Join(out, ",")
	}
	ops := []recipe.Op{
		{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.5}`)},
		{Kind: recipe.OpUnpremultiply},
		{Kind: recipe.OpChromaKey, Params: json.RawMessage(`{"color":"00ff00"}`)},
		{Kind: recipe.OpDelay, Params: json.RawMessage(`{"ms":40}`)},
		{Kind: recipe.OpText, Params: json.RawMessage(`{"text":"x"}`)},
		{Kind: recipe.OpReverse},
		autocropOp(`{}`), // index 6
		{Kind: recipe.OpSpeed, Params: json.RawMessage(`{"factor":2}`)},
		{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1}`)},
		{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)},
		{Kind: recipe.OpColorKey, Params: json.RawMessage(`{"color":"0000ff"}`)},
		{Kind: recipe.OpFeather, Params: json.RawMessage(`{"radius":2}`)},
		{Kind: recipe.OpResize, Params: json.RawMessage(`{"width":32}`)},
	}
	pre, err := autocropDetectionOps(ops, 6)
	want := strings.Join([]string{recipe.OpTrim, recipe.OpUnpremultiply, recipe.OpChromaKey, recipe.OpDelay, recipe.OpSpeed, recipe.OpFPS, recipe.OpColorKey, recipe.OpFeather}, ",")
	if err != nil || kinds(pre) != want {
		t.Errorf("detection ops = %v (%v); want %v", kinds(pre), err, want)
	}
	if len(pre) > 2 && string(pre[2].Params) != `{"color":"00ff00"}` {
		t.Errorf("keying op params not carried: %s", pre[2].Params)
	}
	if pre, err := autocropDetectionOps([]recipe.Op{autocropOp(`{}`)}, 0); err != nil || len(pre) != 0 {
		t.Errorf("autocrop alone: %+v %v", pre, err)
	}
	// A keying op behind the autocrop is read like one in front of it — the
	// render is keyed either way — and both orders key the same memo entry.
	colorKey := recipe.Op{Kind: recipe.OpColorKey, Params: json.RawMessage(`{"color":"00ff00"}`)}
	after, err := autocropDetectionOps([]recipe.Op{autocropOp(`{}`), colorKey}, 0)
	if err != nil || kinds(after) != recipe.OpColorKey || string(after[0].Params) != string(colorKey.Params) {
		t.Errorf("keying behind the autocrop: %+v %v", after, err)
	}
	before, err := autocropDetectionOps([]recipe.Op{colorKey, autocropOp(`{}`)}, 1)
	if err != nil || kinds(before) != recipe.OpColorKey {
		t.Errorf("keying in front of the autocrop: %+v %v", before, err)
	}
	src := strings.Repeat("a", 64)
	kAfter, err1 := autocropKey(src, after, 1)
	kBefore, err2 := autocropKey(src, before, 1)
	if err1 != nil || err2 != nil || kAfter != kBefore {
		t.Errorf("[autocrop, colorkey] and [colorkey, autocrop] must share a memo key: %s vs %s (%v %v)", short(kAfter), short(kBefore), err1, err2)
	}
	if kNone, _ := autocropKey(src, nil, 1); kNone == kAfter {
		t.Error("[autocrop, colorkey] shares the unkeyed stack's memo key")
	}
	// Geometry in front of the autocrop is refused; behind it, it is fine
	// (the crop is applied first) and stays out of the detection.
	for _, kind := range []string{recipe.OpCrop, recipe.OpResize, recipe.OpCanvas, recipe.OpFlip, recipe.OpRotate} {
		_, err := autocropDetectionOps([]recipe.Op{{Kind: recipe.OpTrim}, {Kind: kind}, autocropOp(`{}`)}, 2)
		if !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), "op 2") {
			t.Errorf("%s before autocrop: err = %v", kind, err)
		}
		pre, err := autocropDetectionOps([]recipe.Op{autocropOp(`{}`), {Kind: kind}, {Kind: recipe.OpTrim}}, 0)
		if err != nil || kinds(pre) != recipe.OpTrim {
			t.Errorf("%s behind autocrop: %+v %v", kind, pre, err)
		}
	}
}

func TestContentBox(t *testing.T) {
	full := recipe.CropParams{W: 64, H: 48}
	for _, tc := range []struct {
		name          string
		w, h, x, y    int
		ok            bool
		pad           int
		want          recipe.CropParams
		wantFullFrame bool
	}{
		{name: "no detection", ok: false, wantFullFrame: true},
		{name: "negative box (nothing above the limit)", w: -62, h: -46, x: 64, y: 48, ok: true, wantFullFrame: true},
		{name: "zero box", w: 0, h: 10, x: 1, y: 1, ok: true, wantFullFrame: true},
		{name: "plain", w: 24, h: 24, x: 16, y: 12, ok: true, want: recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}},
		{name: "padded", w: 24, h: 24, x: 16, y: 12, ok: true, pad: 2, want: recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}},
		{name: "padding clamps to the frame", w: 24, h: 24, x: 16, y: 12, ok: true, pad: 100, wantFullFrame: true},
		{name: "box past the edge is clamped", w: 10, h: 10, x: 60, y: 40, ok: true, want: recipe.CropParams{X: 60, Y: 40, W: 4, H: 8}},
		{name: "box entirely outside", w: 10, h: 10, x: 70, y: 50, ok: true, wantFullFrame: true},
	} {
		got := contentBox(tc.w, tc.h, tc.x, tc.y, tc.ok, tc.pad, 64, 48)
		want := tc.want
		if tc.wantFullFrame {
			want = full
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, want)
		}
	}
}

func TestAutocropKey(t *testing.T) {
	src := strings.Repeat("a", 64)
	trim := recipe.Op{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":1,"end":2}`)}
	trimSpaced := recipe.Op{Kind: recipe.OpTrim, Params: json.RawMessage(`{ "end": 2, "start": 1 }`)}
	k, err := autocropKey(src, []recipe.Op{trim}, 1)
	if err != nil || !recipe.IsHash(k) {
		t.Fatalf("key %q %v", k, err)
	}
	if k2, _ := autocropKey(src, []recipe.Op{trimSpaced}, 1); k2 != k {
		t.Error("key depends on params formatting")
	}
	chroma := recipe.Op{Kind: recipe.OpChromaKey, Params: json.RawMessage(`{"color":"00ff00"}`)}
	feather := recipe.Op{Kind: recipe.OpFeather, Params: json.RawMessage(`{"radius":2}`)}
	for name, other := range map[string]func() (string, error){
		"threshold": func() (string, error) { return autocropKey(src, []recipe.Op{trim}, 2) },
		"ops":       func() (string, error) { return autocropKey(src, nil, 1) },
		"source":    func() (string, error) { return autocropKey(strings.Repeat("b", 64), []recipe.Op{trim}, 1) },
		"keying":    func() (string, error) { return autocropKey(src, []recipe.Op{trim, chroma}, 1) },
		"feather":   func() (string, error) { return autocropKey(src, []recipe.Op{trim, feather}, 1) },
		"key colour": func() (string, error) {
			return autocropKey(src, []recipe.Op{trim, {Kind: recipe.OpChromaKey, Params: json.RawMessage(`{"color":"0000ff"}`)}}, 1)
		},
		"key kind": func() (string, error) {
			return autocropKey(src, []recipe.Op{trim, {Kind: recipe.OpColorKey, Params: json.RawMessage(`{"color":"00ff00"}`)}}, 1)
		},
	} {
		if k2, err := other(); err != nil || k2 == k {
			t.Errorf("key ignores %s (%v)", name, err)
		}
	}
	if _, err := autocropKey(src, []recipe.Op{{Kind: recipe.OpTrim, Params: json.RawMessage(`{bad`)}}, 1); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("bad params: %v", err)
	}
	// The feather radius is in the key too (a wider feather is a wider box).
	kFeather, _ := autocropKey(src, []recipe.Op{trim, feather}, 1)
	if k2, err := autocropKey(src, []recipe.Op{trim, {Kind: recipe.OpFeather, Params: json.RawMessage(`{"radius":5}`)}}, 1); err != nil || k2 == kFeather {
		t.Errorf("key ignores the feather radius (%v)", err)
	}
	// The key is versioned (a bump discards memo entries the old detection
	// wrote) and carries no padding: that is arithmetic on the raw box.
	if autocropKeyVersion == "" {
		t.Error("autocropKeyVersion must not be empty")
	}
	for _, tc := range []struct {
		name          string
		raw           recipe.CropParams
		pad, fw, fh   int
		want          recipe.CropParams
		wantFullFrame bool
	}{
		{name: "raw box, padding 2", raw: recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}, pad: 2, fw: 64, fh: 48, want: recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}},
		{name: "raw box, padding 3", raw: recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}, pad: 3, fw: 64, fh: 48, want: recipe.CropParams{X: 13, Y: 9, W: 30, H: 30}},
		{name: "empty raw box is the full frame", raw: recipe.CropParams{}, pad: 2, fw: 64, fh: 48, wantFullFrame: true},
		{name: "negative raw box is the full frame", raw: recipe.CropParams{X: 64, Y: 48, W: -62, H: -46}, pad: 0, fw: 64, fh: 48, wantFullFrame: true},
	} {
		got := padBox(tc.raw, tc.pad, tc.fw, tc.fh)
		want := tc.want
		if tc.wantFullFrame {
			want = recipe.CropParams{W: tc.fw, H: tc.fh}
		}
		if got != want {
			t.Errorf("%s: padBox = %+v, want %+v", tc.name, got, want)
		}
	}
	if got := rawBox(-62, -46, 64, 48, true); got != (recipe.CropParams{}) {
		t.Errorf("rawBox of a negative detection = %+v, want empty", got)
	}
	if got := rawBox(24, 24, 16, 12, false); got != (recipe.CropParams{}) {
		t.Errorf("rawBox without a detection = %+v, want empty", got)
	}
	if got := rawBox(24, 24, 16, 12, true); got != (recipe.CropParams{X: 16, Y: 12, W: 24, H: 24}) {
		t.Errorf("rawBox = %+v", got)
	}
}

func TestResolveAutoCropMemoAndErrors(t *testing.T) {
	st := newTestStore(t)
	src := putSource(t, st, true) // 64x48, alpha
	m := NewManager(st, fakeTools, Options{})
	ctx := context.Background()

	plain := []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":10}`)}}
	if got, err := m.ResolveAutoCrop(ctx, src, plain); err != nil || len(got) != 1 || &got[0] != &plain[0] {
		t.Errorf("no autocrop op: %+v %v (want the ops themselves)", got, err)
	}
	if _, err := m.ResolveAutoCrop(ctx, strings.Repeat("e", 64), []recipe.Op{autocropOp(`{}`)}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("unknown source: %v", err)
	}
	if _, err := m.ResolveAutoCrop(ctx, src, []recipe.Op{autocropOp(`{}`), autocropOp(`{}`)}); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "only one") {
		t.Errorf("two autocrops: %v", err)
	}
	if _, err := m.ResolveAutoCrop(ctx, src, []recipe.Op{{Kind: recipe.OpCrop, Params: json.RawMessage(`{"x":0,"y":0,"w":8,"h":8}`)}, autocropOp(`{}`)}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("crop before autocrop: %v", err)
	}
	// No memo and no ffmpeg: the detection fails (a server-side error, not
	// the client's).
	ops := []recipe.Op{{Kind: recipe.OpUnpremultiply}, autocropOp(`{"threshold":8,"padding":2}`), {Kind: recipe.OpResize, Params: json.RawMessage(`{"width":32}`)}}
	if _, err := m.ResolveAutoCrop(ctx, src, ops); err == nil || errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("detection without ffmpeg: %v", err)
	}
	// A memoised box is used without any process. The memo holds the RAW
	// detected box (unpadded, unclamped); its key covers the detection ops
	// in front of the autocrop (here the unpremultiply) and the threshold,
	// not the resize behind it and not the padding, which is applied on
	// read.
	key, err := autocropKey(src, []recipe.Op{{Kind: recipe.OpUnpremultiply}}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if other, _ := autocropKey(src, nil, 8); other == key {
		t.Fatal("the unpremultiply op is not in the key")
	}
	memo := filepath.Join(st.Scratch, autocropDir, key+".json")
	os.MkdirAll(filepath.Dir(memo), 0o755)
	if err := os.WriteFile(memo, []byte(`{"x":16,"y":12,"w":24,"h":24}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := m.ResolveAutoCrop(ctx, src, ops)
	if err != nil {
		t.Fatalf("memo hit: %v", err)
	}
	if len(got) != 3 || got[0].Kind != recipe.OpUnpremultiply || got[2].Kind != recipe.OpResize {
		t.Fatalf("resolved ops = %+v", got)
	}
	var p recipe.AutoCropParams
	if err := json.Unmarshal(got[1].Params, &p); err != nil || p.Resolved == nil || *p.Resolved != (recipe.CropParams{X: 14, Y: 10, W: 28, H: 28}) || p.Threshold != 8 || p.Padding != 2 {
		t.Errorf("resolved params = %s (%v)", got[1].Params, err)
	}
	if bytes.Contains(ops[1].Params, []byte("resolved")) {
		t.Error("input ops were modified")
	}
	// Another padding is the same detection: served from the same memo,
	// padded differently; a huge one clamps to the frame.
	for pad, want := range map[int]recipe.CropParams{0: {X: 16, Y: 12, W: 24, H: 24}, 3: {X: 13, Y: 9, W: 30, H: 30}, 100: {W: 64, H: 48}} {
		padded := []recipe.Op{{Kind: recipe.OpUnpremultiply}, autocropOp(fmt.Sprintf(`{"threshold":8,"padding":%d}`, pad))}
		got, err := m.ResolveAutoCrop(ctx, src, padded)
		if err != nil {
			t.Fatalf("padding %d must hit the memo: %v", pad, err)
		}
		var p recipe.AutoCropParams
		if err := json.Unmarshal(got[1].Params, &p); err != nil || p.Resolved == nil || *p.Resolved != want {
			t.Errorf("padding %d: resolved = %s (%v), want %+v", pad, got[1].Params, err, want)
		}
	}
	// A memoised "nothing detected" (empty box) resolves to the full frame.
	emptyKey, _ := autocropKey(src, []recipe.Op{{Kind: recipe.OpUnpremultiply}}, 200)
	if err := os.WriteFile(filepath.Join(st.Scratch, autocropDir, emptyKey+".json"), []byte(`{"x":0,"y":0,"w":0,"h":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = m.ResolveAutoCrop(ctx, src, []recipe.Op{{Kind: recipe.OpUnpremultiply}, autocropOp(`{"threshold":200,"padding":2}`)})
	if err != nil {
		t.Fatalf("empty memo hit: %v", err)
	}
	if err := json.Unmarshal(got[1].Params, &p); err != nil || p.Resolved == nil || *p.Resolved != (recipe.CropParams{W: 64, H: 48}) {
		t.Errorf("empty memo: resolved = %s (%v), want the full frame", got[1].Params, err)
	}
	// A trim or keying op anywhere in the stack keys a different detection:
	// the memo above must not serve it — the compiler hoists those kinds in
	// front of the geometry, so one BEHIND the autocrop shapes the rendered
	// picture just the same. (Ops the crop cannot see — a text overlay in
	// front of the autocrop — do not change the key.)
	trimmed := []recipe.Op{{Kind: recipe.OpUnpremultiply}, {Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.5}`)}, autocropOp(`{"threshold":8,"padding":2}`)}
	if _, err := m.ResolveAutoCrop(ctx, src, trimmed); err == nil {
		t.Error("memo served a differently trimmed clip")
	}
	keyed := []recipe.Op{{Kind: recipe.OpUnpremultiply}, {Kind: recipe.OpChromaKey, Params: json.RawMessage(`{"color":"00ff00"}`)}, autocropOp(`{"threshold":8,"padding":2}`)}
	if _, err := m.ResolveAutoCrop(ctx, src, keyed); err == nil {
		t.Error("memo of the unkeyed stack served the keyed one")
	}
	keyedBehind := []recipe.Op{{Kind: recipe.OpUnpremultiply}, autocropOp(`{"threshold":8,"padding":2}`), {Kind: recipe.OpChromaKey, Params: json.RawMessage(`{"color":"00ff00"}`)}}
	if _, err := m.ResolveAutoCrop(ctx, src, keyedBehind); err == nil {
		t.Error("memo of the unkeyed stack served a stack keyed behind the autocrop")
	}
	trimmedBehind := []recipe.Op{{Kind: recipe.OpUnpremultiply}, autocropOp(`{"threshold":8,"padding":2}`), {Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.5}`)}}
	if _, err := m.ResolveAutoCrop(ctx, src, trimmedBehind); err == nil {
		t.Error("memo served a clip trimmed behind the autocrop")
	}
	// The unpremultiply behind the autocrop is the same detection (the
	// compiler hoists it): the memo serves it.
	unpremultiplyBehind := []recipe.Op{autocropOp(`{"threshold":8,"padding":2}`), {Kind: recipe.OpUnpremultiply}}
	if got, err := m.ResolveAutoCrop(ctx, src, unpremultiplyBehind); err != nil || len(got) != 2 || !bytes.Contains(got[0].Params, []byte(`"resolved"`)) {
		t.Errorf("unpremultiply behind the autocrop must hit the same memo: %+v %v", got, err)
	}
	withText := []recipe.Op{{Kind: recipe.OpUnpremultiply}, {Kind: recipe.OpText, Params: json.RawMessage(`{"text":"hi"}`)}, autocropOp(`{"threshold":8,"padding":2}`), {Kind: recipe.OpResize, Params: json.RawMessage(`{"width":32}`)}}
	if got, err := m.ResolveAutoCrop(ctx, src, withText); err != nil || len(got) != 4 || !bytes.Contains(got[2].Params, []byte(`"resolved"`)) {
		t.Errorf("text in front of the autocrop must hit the same memo: %+v %v", got, err)
	}
	// A stale memo (box outside the frame) is ignored.
	if err := os.WriteFile(memo, []byte(`{"x":60,"y":40,"w":28,"h":28}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ResolveAutoCrop(ctx, src, ops); err == nil {
		t.Error("stale memo was used")
	}
	// Corrupt memo likewise.
	os.WriteFile(memo, []byte(`{corrupt`), 0o644)
	if _, err := m.ResolveAutoCrop(ctx, src, ops); err == nil {
		t.Error("corrupt memo was used")
	}
}

func TestOverlaySourceRules(t *testing.T) {
	st := newTestStore(t)
	main := putSource(t, st, true)
	seq := putSequenceSource(t, st)
	noInfoBlob, err := st.PutBlob(bytes.NewReader([]byte("an unprobed overlay")), "ov.png")
	if err != nil {
		t.Fatal(err)
	}
	noInfo := noInfoBlob.Hash
	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	ctx := context.Background()

	if _, err := m.resolveSources(nil); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("no sources: %v", err)
	}
	_, err = m.resolveSources([]string{main, seq})
	if !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "image sequence") || !strings.Contains(err.Error(), "source 1") {
		t.Errorf("sequence overlay: %v", err)
	}
	// A sequence is fine as the main source.
	if s, err := m.resolveSources([]string{seq, main}); err != nil || len(s.blobs) != 2 || s.main().Hash != seq {
		t.Errorf("sequence main: %v", err)
	}
	if _, err := m.resolveSources([]string{main, noInfo}); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "no probe info") {
		t.Errorf("overlay without info: %v", err)
	}
	if _, err := m.resolveSources([]string{main, strings.Repeat("f", 64)}); !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing overlay: %v", err)
	}

	// Every entry point refuses the sequence overlay with the client error.
	r := recipe.Recipe{Sources: []string{main, seq}, Output: recipe.Output{Format: "gif"}}
	if fin := runJob(t, m, r); fin.State != StateError || !strings.Contains(fin.Error, "image sequence") {
		t.Errorf("render: %+v", fin)
	}
	if _, err := m.StillSources(ctx, []string{main, seq}, nil, recipe.Output{Format: "gif"}, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("still: %v", err)
	}
	if _, err := m.Proxy(ctx, []string{main, seq}, nil, recipe.Output{Format: "gif"}, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("proxy: %v", err)
	}
	// The main-source contract of Still is kept: unknown main = not found,
	// missing overlay = invalid recipe, no sources = invalid recipe.
	if _, err := m.StillSources(ctx, nil, nil, recipe.Output{}, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("still without sources: %v", err)
	}
	if _, err := m.StillSources(ctx, []string{strings.Repeat("0", 64), main}, nil, recipe.Output{}, 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("still unknown main: %v", err)
	}
	if _, err := m.StillSources(ctx, []string{main, strings.Repeat("0", 64)}, nil, recipe.Output{}, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("still unknown overlay: %v", err)
	}
	if _, err := m.Proxy(ctx, []string{strings.Repeat("0", 64)}, nil, recipe.Output{}, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("proxy unknown source: %v", err)
	}
	if _, err := m.Proxy(ctx, nil, nil, recipe.Output{}, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("proxy without sources: %v", err)
	}
}

func TestOptimizeRejectsMultipleSources(t *testing.T) {
	st := newTestStore(t)
	b, err := st.PutBlob(bytes.NewReader(animatedGIF(t)), "square.gif")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "gif", Codec: "gif", PixFmt: "bgra", Width: 40, Height: 30, FPS: 12.5, Duration: 0.96, Frames: 12, HasAlpha: true, Kind: recipe.KindAnimation}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	other := putSource(t, st, true)
	tools := fakeTools
	tools.Gifsicle = "gifsicle-does-not-exist-ezlg-test"
	m := NewManager(st, tools, Options{Concurrency: 1})
	r := recipe.Recipe{Sources: []string{b.Hash, other}, Output: recipe.Output{Format: "gif", Preset: PresetOptimize}}
	fin := runJob(t, m, r)
	if fin.State != StateError || !strings.Contains(fin.Error, "single GIF source") {
		t.Errorf("optimize with two sources: %+v", fin)
	}
}

func TestProxyBoundsKeyAndMemo(t *testing.T) {
	if w, s := proxyBounds(0, 0); w != 360 || s != 10 {
		t.Errorf("defaults = %d, %v", w, s)
	}
	if w, s := proxyBounds(5000, 900); w != MaxProxyWidth || s != MaxProxySeconds {
		t.Errorf("caps = %d, %v", w, s)
	}
	if w, s := proxyBounds(100, math.NaN()); w != 100 || s != 10 {
		t.Errorf("NaN seconds = %d, %v", w, s)
	}
	srcs := []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
	ops := []recipe.Op{{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1,"x":2,"y":3}`)}}
	out := recipe.Output{Format: "webp", Width: 128}
	k, err := proxyKey(srcs, ops, out, 360, 10)
	if err != nil || !recipe.IsHash(k) {
		t.Fatalf("key %q %v", k, err)
	}
	if k2, _ := proxyKey(srcs, []recipe.Op{{Kind: recipe.OpOverlay, Params: json.RawMessage(`{ "y":3, "x":2, "source":1 }`)}}, out, 360, 10); k2 != k {
		t.Error("key depends on params formatting")
	}
	if k2, _ := proxyKey(srcs, ops, out, 240, 10); k2 == k {
		t.Error("key ignores maxW")
	}
	if k2, _ := proxyKey(srcs, ops, out, 360, 5); k2 == k {
		t.Error("key ignores maxSeconds")
	}
	if k2, _ := proxyKey(srcs[:1], ops, out, 360, 10); k2 == k {
		t.Error("key ignores the overlay source")
	}
	if k2, _ := proxyKey(srcs, []recipe.Op{{Kind: recipe.OpText, Params: json.RawMessage(`{"text":"hello"}`)}}, out, 360, 10); k2 == k {
		t.Error("key ignores the ops")
	}
	if sk, _ := stillKey(srcs, ops, out, 0, 360); sk == k {
		t.Error("proxy and still keys collide")
	}
	// Both memo keys are salted with a version: a bump discards every entry
	// the previous enc/jobs code rendered (reversed tails, single-frame
	// plans, reversed VFR stills).
	if k2, _ := proxyKeyV(srcs, ops, out, 360, 10, proxyMemoVersion+"x"); k2 == k {
		t.Error("proxy key ignores proxyMemoVersion")
	}
	if k2, _ := proxyKeyV(srcs, ops, out, 360, 10, proxyMemoVersion); k2 != k {
		t.Error("proxyKey is not proxyKeyV at proxyMemoVersion")
	}
	sk, _ := stillKey(srcs, ops, out, 0.5, 360)
	if sk2, _ := stillKeyV(srcs, ops, out, 0.5, 360, stillMemoVersion+"x"); sk2 == sk {
		t.Error("still key ignores stillMemoVersion")
	}
	if sk2, _ := stillKeyV(srcs, ops, out, 0.5, 360, stillMemoVersion); sk2 != sk {
		t.Error("stillKey is not stillKeyV at stillMemoVersion")
	}
	if proxyMemoVersion == "" || stillMemoVersion == "" {
		t.Error("memo versions must not be empty")
	}

	// A memoised proxy is served without ffmpeg; without one the render
	// fails on the missing tool.
	st := newTestStore(t)
	src := putSource(t, st, true)
	m := NewManager(st, ffrun.Tools{}, Options{})
	ctx := context.Background()
	if _, err := m.Proxy(ctx, []string{src}, nil, out, 0, 0); err == nil || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("no ffmpeg: %v", err)
	}
	key, _ := proxyKey([]string{src}, nil, stillOutput(out), 360, 10)
	memo := filepath.Join(st.Scratch, proxyDir, key+".webp")
	os.MkdirAll(filepath.Dir(memo), 0o755)
	os.WriteFile(memo, []byte("RIFFWEBP"), 0o644)
	got, err := m.Proxy(ctx, []string{src}, nil, recipe.Output{Format: "webp", Width: 128, Quality: 30, Target: "emote"}, 0, 0)
	if err != nil || string(got) != "RIFFWEBP" {
		t.Errorf("memo hit (quality knobs must not matter): %q %v", got, err)
	}
	if _, err := m.Proxy(ctx, []string{src}, []recipe.Op{{Kind: "nonsense"}}, out, 0, 0); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("bad op: %v", err)
	}
}

func TestFontsWithoutFcList(t *testing.T) {
	m := NewManager(newTestStore(t), ffrun.Tools{}, Options{})
	ctx := context.Background()
	if fonts := m.Fonts(ctx); len(fonts) != 0 {
		t.Errorf("fonts without fc-list = %+v", fonts)
	}
	if !m.fontsDone {
		t.Error("a missing fc-list must be cached as 'no fonts'")
	}
	// A broken fc-list is not cached: the next call retries.
	m = NewManager(newTestStore(t), ffrun.Tools{FcList: "fc-list-does-not-exist-ezlg-test"}, Options{})
	if fonts := m.Fonts(ctx); len(fonts) != 0 || m.fontsDone {
		t.Errorf("broken fc-list: fonts=%v done=%v", fonts, m.fontsDone)
	}
}

func TestFlightDedupesAndRecoversFromLeaderCancel(t *testing.T) {
	var g flight[int]
	var calls atomic.Int32
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		calls.Add(1)
		select {
		case <-release:
			return 42, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	const n = 8
	var wg sync.WaitGroup
	results := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = g.do(context.Background(), "k", fn)
		}()
	}
	// Let every goroutine join before the leader finishes.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != 42 {
			t.Errorf("call %d: %d %v", i, results[i], errs[i])
		}
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("fn ran %d times, want 1", c)
	}
	// Another key after the first finished runs fn again.
	if v, err := g.do(context.Background(), "k2", fn); err != nil || v != 42 || calls.Load() != 2 {
		t.Errorf("second key: %d %v (calls %d)", v, err, calls.Load())
	}

	// The leader's request is aborted: a waiter with a live ctx re-runs fn
	// and still gets the value.
	var g2 flight[int]
	calls.Store(0)
	started := make(chan struct{}, 4)
	release2 := make(chan struct{})
	fn2 := func(ctx context.Context) (int, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release2:
			return 7, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := g2.do(leaderCtx, "k", fn2)
		leaderDone <- err
	}()
	<-started
	waiterDone := make(chan int, 1)
	go func() {
		v, err := g2.do(context.Background(), "k", fn2)
		if err != nil {
			t.Errorf("waiter: %v", err)
		}
		waiterDone <- v
	}()
	time.Sleep(20 * time.Millisecond) // the waiter has joined
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader: %v", err)
	}
	<-started // the waiter became the leader
	close(release2)
	if v := <-waiterDone; v != 7 {
		t.Errorf("waiter got %d", v)
	}
	if c := calls.Load(); c != 2 {
		t.Errorf("fn ran %d times, want 2 (leader + retry)", c)
	}

	// A waiter whose own ctx ends returns at once while the leader runs on.
	var g3 flight[int]
	release3 := make(chan struct{})
	fn3 := func(ctx context.Context) (int, error) {
		<-release3
		return 1, nil
	}
	go g3.do(context.Background(), "k", fn3)
	time.Sleep(10 * time.Millisecond)
	wctx, wcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer wcancel()
	if _, err := g3.do(wctx, "k", fn3); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter timeout: %v", err)
	}
	close(release3)
}

func TestBindTextFilesWritesVerbatim(t *testing.T) {
	p := &graph.Plan{Filter: "[0:v]drawtext=textfile=TXT_1:fontsize=12,drawtext=textfile=TXT_2[out]", OutLabel: "[out]",
		TextFiles: []graph.TextFile{{Placeholder: "TXT_1", Content: "héllo\nwörld "}, {Placeholder: "TXT_2", Content: "a:b\\c'd"}}}
	dir := t.TempDir()
	bound, err := bindTextFiles(p, dir)
	if err != nil {
		if strings.Contains(err.Error(), "not implemented") {
			t.Skipf("graph.BindTextFiles is not implemented yet: %v", err)
		}
		t.Fatal(err)
	}
	for i, tf := range p.TextFiles {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("t%d.txt", i+1)))
		if err != nil || string(data) != tf.Content {
			t.Errorf("t%d.txt = %q (%v), want %q", i+1, data, err, tf.Content)
		}
	}
	if bound == p || bound.TextFiles != nil || strings.Contains(bound.Filter, "TXT_") {
		t.Errorf("bound plan = %+v", bound)
	}
	if !strings.Contains(p.Filter, "TXT_1") || len(p.TextFiles) != 2 {
		t.Error("the input plan was modified")
	}
	// Without text files the plan is returned as is and nothing is written.
	empty := t.TempDir()
	q := &graph.Plan{Filter: "[0:v]format=rgba[out]"}
	if got, err := bindTextFiles(q, empty); err != nil || got != q {
		t.Errorf("no text files: %v %v", got, err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("files written for a plan without text: %d", len(entries))
	}
}

// argAfter returns the argument following the first flag in args ("" when
// absent).
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestCropDetectInputPathMatchesMaster: the detection pass hands enc the
// bare blob path like every other builder (renderMaster, StillArgs,
// ProxyArgs), so an image sequence's "-i" is "<blobDir>/<pattern>" exactly
// once — enc joins plan.InputPattern itself. (jobs used to join it as well
// and asked ffmpeg for "<dir>.seq/%06d.png/%06d.png": every autocrop on a
// sequence failed with "could find no file or sequence".)
func TestCropDetectInputPathMatchesMaster(t *testing.T) {
	st := newTestStore(t)
	seq := putSequenceSource(t, st)
	m := NewManager(st, fakeTools, Options{})
	blobs, err := m.lookupSources([]string{seq})
	if err != nil {
		t.Fatal(err)
	}
	b := blobs[0]
	if !b.IsSequence() {
		t.Fatalf("blob %s is not a sequence", b.Path)
	}
	for name, ops := range map[string][]recipe.Op{
		"plain": nil,
		// 2 frames at 40 ms: 0.08 s long, the trim skips the first frame.
		"trimmed": {{Kind: recipe.OpDelay, Params: json.RawMessage(`{"ms":40}`)}, {Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.04}`)}},
	} {
		plan, err := graph.CompileDetect([]recipe.ProbeInfo{*b.Info}, ops)
		if err != nil {
			t.Fatalf("%s: CompileDetect: %v", name, err)
		}
		if plan.InputPattern == "" {
			t.Fatalf("%s: the detect plan of a sequence has no InputPattern: %+v", name, plan)
		}
		detect := argAfter(enc.CropDetectPlanArgs(b.Path, plan, plan.HasAlpha, 1), "-i")
		master := argAfter(enc.MasterArgs(b.Path, plan, "x"), "-i")
		if detect == "" || detect != master {
			t.Errorf("%s: detection -i %q, master -i %q", name, detect, master)
		}
		if n := strings.Count(detect, plan.InputPattern); n != 1 {
			t.Errorf("%s: detection -i %q contains the pattern %q %d times, want once", name, detect, plan.InputPattern, n)
		}
		if !strings.HasPrefix(detect, strings.TrimRight(b.Path, `/\`)+"/") {
			t.Errorf("%s: detection -i %q is not under the blob dir %q", name, detect, b.Path)
		}
	}
}
