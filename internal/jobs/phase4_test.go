package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/fit"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// gifSourceInfo is the probe info of the animatedGIF/opaqueGIF test sources
// (12 frames of 40x30 at 8 cs = 12.5 fps).
func gifSourceInfo() recipe.ProbeInfo {
	return recipe.ProbeInfo{
		Format: "gif", Codec: "gif", PixFmt: "bgra", Width: 40, Height: 30,
		FPS: 12.5, Duration: 0.96, Frames: 12, Kind: recipe.KindAnimation,
	}
}

// putGIFSource stores a real GIF blob with matching probe info.
func putGIFSource(t *testing.T, st *store.Store, data []byte) string {
	t.Helper()
	b, err := st.PutBlob(bytes.NewReader(data), "anim.gif")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetBlobInfo(b.Hash, gifSourceInfo()); err != nil {
		t.Fatal(err)
	}
	return b.Hash
}

func op(kind, params string) recipe.Op {
	o := recipe.Op{Kind: kind}
	if params != "" {
		o.Params = json.RawMessage(params)
	}
	return o
}

// TestSubmitPhase4Validation: mp4/webm and the gifski encoder are refused
// for emote/sticker targets (and malformed encoder values everywhere) with
// ErrInvalidRecipe at Submit time, before any work starts; the allowed
// combinations pass validation.
func TestSubmitPhase4Validation(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	src := putSource(t, st, true)

	bad := []recipe.Output{
		{Format: "mp4", Target: "emote"},
		{Format: "mp4", Target: "sticker"},
		{Format: "webm", Target: "emote"},
		{Format: "webm", Target: "sticker"},
		{Format: "gif", Encoder: "gifski", Target: "emote"},
		{Format: "gif", Encoder: "gifski", Target: "sticker"},
		{Format: "webp", Encoder: "gifski"},
		{Format: "mp4", Encoder: "gifski"},
		{Format: "gif", Encoder: "zopfli"},
		{Format: "gif", Encoder: "gifski", Preset: "optimize"},
	}
	for _, out := range bad {
		r := recipe.Recipe{Sources: []string{src}, Output: out}
		if _, err := m.Submit(r); !errors.Is(err, ErrInvalidRecipe) {
			t.Errorf("Submit(%+v) err = %v, want ErrInvalidRecipe", out, err)
		}
	}

	good := []recipe.Output{
		{Format: "mp4"},
		{Format: "mp4", Target: "attachment"},
		{Format: "webm", Target: "attachment-500"},
		{Format: "gif", Encoder: "gifski"},
		{Format: "gif", Encoder: "GIFSKI", Target: "attachment"}, // case-folded
	}
	for _, out := range good {
		r := recipe.Recipe{Sources: []string{src}, Output: out}
		j, err := m.Submit(r)
		if err != nil {
			t.Errorf("Submit(%+v) err = %v, want accepted", out, err)
			continue
		}
		// The job itself fails later (fakeTools name no real ffmpeg); wait so
		// no goroutine outlives the test store.
		waitFinished(t, m, j.ID)
	}
}

// TestCheckFastPathSpec: the pure half of the fast-path eligibility matrix.
func TestCheckFastPathSpec(t *testing.T) {
	info := gifSourceInfo()
	src := strings.Repeat("a", 64)
	base := func(out recipe.Output, ops ...recipe.Op) recipe.Recipe {
		return recipe.Recipe{Sources: []string{src}, Ops: ops, Output: out}
	}
	gif := recipe.Output{Format: "gif"}

	eligible := []struct {
		name string
		r    recipe.Recipe
	}{
		{"no ops", base(gif)},
		{"crop", base(gif, op("crop", `{"x":8,"y":8,"w":24,"h":16}`))},
		{"trim", base(gif, op("trim", `{"start":0.16,"end":0.64}`))},
		{"fps op source/2", base(gif, op("fps", `{"fps":6.25}`))},
		{"output fps", base(recipe.Output{Format: "gif", FPS: 6.25})},
		{"loop", base(recipe.Output{Format: "gif", Loop: 5})},
		{"attachment target", base(recipe.Output{Format: "gif", Target: "attachment"})},
		{"attachment-100 target", base(recipe.Output{Format: "gif", Target: "attachment-100"})},
		{"all combined", base(recipe.Output{Format: "gif", FPS: 6.25, Loop: 2, Target: "attachment"},
			op("trim", `{"start":0.16,"end":0.64}`), op("crop", `{"x":0,"y":0,"w":20,"h":20}`))},
	}
	for _, tc := range eligible {
		if _, ok := checkFastPathSpec(info, tc.r); !ok {
			t.Errorf("%s: want eligible", tc.name)
		}
	}

	notGIF := info
	notGIF.Codec = "h264"
	seq := info
	seq.Kind = recipe.KindSequence
	ineligible := []struct {
		name string
		info recipe.ProbeInfo
		r    recipe.Recipe
	}{
		{"non-gif source", notGIF, base(gif)},
		{"sequence source", seq, base(gif)},
		{"two sources", info, recipe.Recipe{Sources: []string{src, src}, Output: gif}},
		{"webp output", info, base(recipe.Output{Format: "webp"})},
		{"gifski encoder", info, base(recipe.Output{Format: "gif", Encoder: "gifski"})},
		{"fit budget", info, base(recipe.Output{Format: "gif", FitBytes: 1 << 20})},
		{"lossy", info, base(recipe.Output{Format: "gif", Lossy: 40})},
		{"colors", info, base(recipe.Output{Format: "gif", Colors: 64})},
		{"output canvas", info, base(recipe.Output{Format: "gif", Width: 128, Height: 128})},
		{"emote target", info, base(recipe.Output{Format: "gif", Target: "emote"})},
		{"sticker target", info, base(recipe.Output{Format: "gif", Target: "sticker"})},
		{"optimize preset", info, base(recipe.Output{Format: "gif", Preset: "optimize"})},
		{"resize op", info, base(gif, op("resize", `{"width":20}`))},
		{"reverse op", info, base(gif, op("reverse", ""))},
		{"bounce op", info, base(gif, op("bounce", ""))},
		{"speed op", info, base(gif, op("speed", `{"factor":2}`))},
		{"text op", info, base(gif, op("text", `{"text":"hi"}`))},
		{"two crops", info, base(gif, op("crop", `{"x":0,"y":0,"w":20,"h":20}`), op("crop", `{"x":0,"y":0,"w":10,"h":10}`))},
		{"two trims", info, base(gif, op("trim", `{"start":0}`), op("trim", `{"start":0.08}`))},
		{"crop out of bounds", info, base(gif, op("crop", `{"x":30,"y":0,"w":20,"h":20}`))},
		{"crop empty", info, base(gif, op("crop", `{"x":0,"y":0,"w":0,"h":10}`))},
		{"crop bad json", info, base(gif, op("crop", `{`))},
		{"negative trim", info, base(gif, op("trim", `{"start":-1}`))},
		{"fps op and output fps", info, base(recipe.Output{Format: "gif", FPS: 6.25}, op("fps", `{"fps":6.25}`))},
	}
	for _, tc := range ineligible {
		if _, ok := checkFastPathSpec(tc.info, tc.r); ok {
			t.Errorf("%s: want ineligible", tc.name)
		}
	}

	// The spec carries the reduced edits.
	s, ok := checkFastPathSpec(info, base(recipe.Output{Format: "gif", FPS: 6.25, Loop: 2},
		op("trim", `{"start":0.16,"end":0.64}`), op("crop", `{"x":8,"y":8,"w":24,"h":16}`)))
	if !ok {
		t.Fatal("combined recipe must be eligible")
	}
	if s.crop == nil || *s.crop != (enc.GifsicleCrop{X: 8, Y: 8, W: 24, H: 16}) {
		t.Errorf("crop = %+v", s.crop)
	}
	if !s.trimSet || s.trim.Start != 0.16 || s.trim.End != 0.64 {
		t.Errorf("trim = %+v (set %v)", s.trim, s.trimSet)
	}
	if s.wantFPS != 6.25 {
		t.Errorf("wantFPS = %v", s.wantFPS)
	}
}

// TestTrimFrameRange: trim bounds must land on the source frame grid within
// graph.FrameTolerance; the [Start, End) window maps onto inclusive source
// frame indices, End open or past the last frame keeps the tail.
func TestTrimFrameRange(t *testing.T) {
	const fps = 12.5 // 80 ms frames
	tests := []struct {
		name       string
		start, end float64
		a, b       int
		ok         bool
	}{
		{"whole clip", 0, 0, 0, 0, true},
		{"grid trim", 0.16, 0.64, 2, 7, true},
		{"open end", 0.24, 0, 3, 0, true},
		{"end past last frame", 0.16, 5, 2, 0, true},
		{"end at exactly the clip end", 0.16, 0.96, 2, 0, true},
		{"off-grid start", 0.20, 0.64, 0, 0, false},
		{"off-grid end", 0.16, 0.50, 0, 0, false},
		{"empty window", 0.16, 0.16, 0, 0, false},
		{"single frame 0", 0, 0.08, 0, 0, false}, // FrameEnd 0 would mean open
		{"start past clip", 1.60, 0, 0, 0, false},
	}
	for _, tc := range tests {
		a, b, ok := trimFrameRange(recipe.TrimParams{Start: tc.start, End: tc.end}, fps, 12)
		if a != tc.a || b != tc.b || ok != tc.ok {
			t.Errorf("%s: trimFrameRange(%v, %v) = (%d, %d, %v), want (%d, %d, %v)",
				tc.name, tc.start, tc.end, a, b, ok, tc.a, tc.b, tc.ok)
		}
	}

	// The µs-rounded seek times the UI produces stay on the grid: 2/30 s at
	// 6 decimals is 0.066667, which is within graph.FrameTolerance of frame 2.
	if i, ok := gridIndex(0.066667, 30); !ok || i != 2 {
		t.Errorf("gridIndex(0.066667, 30) = (%d, %v), want (2, true)", i, ok)
	}
	if _, ok := gridIndex(0.07, 30); ok {
		t.Error("gridIndex(0.07, 30) must be off-grid")
	}
	_ = graph.FrameTolerance // documented dependency
}

// TestFastPathFor: the manager half — real GIF facts, frame-grid trim,
// drop-every-N fps rates, gifsicle availability.
func TestFastPathFor(t *testing.T) {
	st := newTestStore(t)
	tools := fakeTools
	tools.Gifsicle = "gifsicle-does-not-exist-ezlg-test"
	m := NewManager(st, tools, Options{Concurrency: 1})
	src := putGIFSource(t, st, animatedGIF(t))
	blob, err := st.GetBlob(src)
	if err != nil {
		t.Fatal(err)
	}

	r := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", FPS: 6.25, Loop: 3},
		Ops: []recipe.Op{op("trim", `{"start":0.16,"end":0.64}`), op("crop", `{"x":8,"y":8,"w":24,"h":16}`)}}
	fp, ok := m.fastPathFor(blob, r)
	if !ok {
		t.Fatal("want eligible")
	}
	if fp.opts.FrameStart != 2 || fp.opts.FrameEnd != 7 || fp.opts.DropEveryN != 2 || fp.opts.Loop != 3 {
		t.Errorf("opts = %+v", fp.opts)
	}
	if fp.opts.Crop == nil || fp.opts.Crop.W != 24 {
		t.Errorf("crop = %+v", fp.opts.Crop)
	}
	if len(fp.facts.delays) != 12 || fp.facts.delays[0] != 8 {
		t.Errorf("facts = %+v", fp.facts)
	}

	// An fps that is not (n-1)/n of the source for n in 2..4 is not eligible.
	bad := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", FPS: 5}}
	if _, ok := m.fastPathFor(blob, bad); ok {
		t.Error("fps 5 of a 12.5 fps source must not be eligible")
	}
	// An fps ABOVE the source is realised by duplicating frames on the
	// decode path — never lossless.
	bad = recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", FPS: 30}}
	if _, ok := m.fastPathFor(blob, bad); ok {
		t.Error("fps above the source must not be eligible")
	}
	// The source rate itself is a keep-every-frame no-op.
	keep := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", FPS: 12.5}}
	if fp, ok := m.fastPathFor(blob, keep); !ok || fp.opts.DropEveryN != 0 {
		t.Errorf("fps == source must be a keep (%+v, %v)", fp, ok)
	}
	// An off-grid trim is not eligible.
	bad = recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif"},
		Ops: []recipe.Op{op("trim", `{"start":0.1}`)}}
	if _, ok := m.fastPathFor(blob, bad); ok {
		t.Error("off-grid trim must not be eligible")
	}
	// A near-miss fps — inside the optimize preset's 5 % window but beyond
	// fastPathFPSTolerance — must fall through to the decode pipeline's
	// exact resample instead of silently keeping/dropping frames.
	bad = recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", FPS: 12.4}}
	if _, ok := m.fastPathFor(blob, bad); ok {
		t.Error("fps 12.4 on a 12.5 fps source must not be a fast-path keep")
	}
	bad = recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", FPS: 6.4}}
	if _, ok := m.fastPathFor(blob, bad); ok {
		t.Error("fps 6.4 on a 12.5 fps source must not be a fast-path drop-every-2")
	}
	// Without gifsicle the fast path never triggers.
	m2 := NewManager(st, fakeTools, Options{Concurrency: 1})
	if _, ok := m2.fastPathFor(blob, r); ok {
		t.Error("no gifsicle → not eligible")
	}
}

// variableDelayGIF builds a 12-frame 40x30 GIF whose first 4 frames hold
// 100 cs (1 s) and the rest 10 cs — the held-first-frames shape ffmpeg
// probes at its base cadence (r_frame_rate 10), which is NOT a playback
// grid for this file.
func variableDelayGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{220, 30, 30, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < 12; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, 40, 30), pal)
		fr.SetColorIndex(i*3, 5, 1)
		g.Image = append(g.Image, fr)
		d := 10
		if i < 4 {
			d = 100
		}
		g.Delay = append(g.Delay, d)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestFastPathForVariableDelays: timeline edits (trim, fps) on a
// variable-delay GIF must never take the fast path — index math at the
// probed base cadence would keep the wrong content window — while
// crop/loop-only recipes (gifsicle preserves each frame's own delay) stay
// eligible.
func TestFastPathForVariableDelays(t *testing.T) {
	st := newTestStore(t)
	tools := fakeTools
	tools.Gifsicle = "gifsicle-does-not-exist-ezlg-test"
	m := NewManager(st, tools, Options{Concurrency: 1})
	b, err := st.PutBlob(bytes.NewReader(variableDelayGIF(t)), "vfr.gif")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{
		Format: "gif", Codec: "gif", PixFmt: "bgra", Width: 40, Height: 30,
		FPS: 10, Duration: 4.8, Frames: 12, Kind: recipe.KindAnimation,
	}
	if err := st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	blob, err := st.GetBlob(b.Hash)
	if err != nil {
		t.Fatal(err)
	}
	src := b.Hash

	// A trim that lands exactly on the probed 10 fps grid (start 0.4 s →
	// frame 4) is still not lossless: the file's real playback puts 0.4 s
	// inside the first held frame.
	trim := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif"},
		Ops: []recipe.Op{op("trim", `{"start":0.4}`)}}
	if _, ok := m.fastPathFor(blob, trim); ok {
		t.Error("on-grid trim of a variable-delay GIF must not be eligible")
	}
	// Likewise a drop-every-N: dropping every 2nd frame of a variable-delay
	// GIF is not the fps filter's timestamp resample.
	fps := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", FPS: 5}}
	if _, ok := m.fastPathFor(blob, fps); ok {
		t.Error("fps drop on a variable-delay GIF must not be eligible")
	}
	// Crop/loop-only recipes keep every frame with its own delay: eligible.
	crop := recipe.Recipe{Sources: []string{src}, Output: recipe.Output{Format: "gif", Loop: 3},
		Ops: []recipe.Op{op("crop", `{"x":8,"y":8,"w":24,"h":16}`)}}
	if fp, ok := m.fastPathFor(blob, crop); !ok {
		t.Error("crop/loop-only on a variable-delay GIF must stay eligible")
	} else if fp.opts.DropEveryN != 0 || fp.opts.FrameStart != 0 || fp.opts.FrameEnd != 0 {
		t.Errorf("crop/loop-only opts = %+v", fp.opts)
	}
}

// TestUniformDelays: the constant-grid gate of the fast path's timeline
// edits.
func TestUniformDelays(t *testing.T) {
	if !uniformDelays([]int{8, 8, 8}, 12.5) {
		t.Error("uniform 8 cs at 12.5 fps must pass")
	}
	if !uniformDelays([]int{10, 10}, 10) {
		t.Error("uniform 10 cs at 10 fps must pass")
	}
	if uniformDelays([]int{100, 100, 10}, 10) {
		t.Error("variable delays must fail")
	}
	if uniformDelays(nil, 12.5) {
		t.Error("no delays must fail")
	}
	if uniformDelays([]int{8, 8}, 0) {
		t.Error("unknown probed rate must fail")
	}
	if uniformDelays([]int{0, 0}, 10) {
		t.Error("0 cs delays (ffmpeg substitutes its 10 fps default) must fail")
	}
	if uniformDelays([]int{1, 1}, 10) {
		t.Error("1 cs delays (clamped to ffmpeg's 10 fps default, really 100 fps) must fail")
	}
	if uniformDelays([]int{8, 8}, 10) {
		t.Error("a delay table disagreeing with the probed rate must fail")
	}
}

// TestFitKnobPhase4: the video CRF knobs put the mild probe at the user's
// own quality (which IS the CRF for video); the gifski knob searches
// 100-quality.
func TestFitKnobPhase4(t *testing.T) {
	if k := fitKnob(recipe.FormatMP4, recipe.Output{}); k.Name != fit.KnobCRF || k.Mild != enc.DefaultX264CRF || k.Min != 12 || k.Max != 40 {
		t.Errorf("mp4 default knob = %+v", k)
	}
	if k := fitKnob(recipe.FormatMP4, recipe.Output{Quality: 23}); k.Mild != 23 {
		t.Errorf("mp4 crf-23 knob = %+v (Quality IS the CRF)", k)
	}
	if k := fitKnob(recipe.FormatMP4, recipe.Output{Quality: 51}); k.Mild != 40 || k.Harsh != 40 {
		t.Errorf("mp4 crf-51 knob = %+v (mild must clamp into the searched range)", k)
	}
	if k := fitKnob(recipe.FormatMP4, recipe.Output{Quality: 1}); k.Mild != 1 || k.Min != 1 {
		t.Errorf("mp4 crf-1 knob = %+v (an explicit mild CRF extends Min, never clamps up)", k)
	}
	if k := fitKnob(recipe.FormatWebM, recipe.Output{}); k.Name != fit.KnobCRF || k.Mild != enc.DefaultVP9CRF || k.Min != 15 || k.Max != 55 {
		t.Errorf("webm default knob = %+v", k)
	}
	if k := fitKnob(recipe.FormatWebM, recipe.Output{Quality: 10}); k.Mild != 10 || k.Min != 10 {
		t.Errorf("webm crf-10 knob = %+v (an explicit mild CRF extends Min, never clamps up)", k)
	}
	if k := fitKnob(recipe.FormatGIF, recipe.Output{Encoder: "gifski"}); k.Name != fit.KnobQuality || k.Mild != 10 || k.Max != 99 {
		t.Errorf("gifski default knob = %+v", k)
	}
	if k := fitKnob(recipe.FormatGIF, recipe.Output{Encoder: "gifski", Quality: 40}); k.Mild != 60 || k.Harsh != 70 {
		t.Errorf("gifski q40 knob = %+v", k)
	}
	if k := fitKnob(recipe.FormatGIF, recipe.Output{Encoder: "gifski", Quality: 10}); k.Mild != 90 || k.Harsh != 99 {
		t.Errorf("gifski q10 knob = %+v (harsh must move past the mild probe)", k)
	}
	// The plain gif knob is untouched by the gifski branch.
	if k := fitKnob(recipe.FormatGIF, recipe.Output{Lossy: 80}); k.Name != fit.KnobLossy || k.Mild != 80 {
		t.Errorf("gif lossy knob = %+v", k)
	}
}

// TestFitLadderVideo: DESIGN §5.4 — video fits as rendered, one rung, the
// secant search on the CRF knob alone; no fps/size/colour rungs.
func TestFitLadderVideo(t *testing.T) {
	master := enc.Master{Width: 1920, Height: 1080, FPS: 30, Frames: 300}
	for _, format := range []string{recipe.FormatMP4, recipe.FormatWebM} {
		rungs := fitLadder(format, recipe.Output{Format: format, FitBytes: 1}, master)
		if len(rungs) != 1 {
			t.Fatalf("%s ladder = %+v (want a single as-rendered rung)", format, rungs)
		}
		r := rungs[0]
		if r.FPS != 0 || r.Width != 0 || r.Height != 0 || r.Colors != 0 {
			t.Errorf("%s rung changes the render: %+v", format, r)
		}
		if effectiveFormat(r, format) != format {
			t.Errorf("%s rung format = %q", format, r.Format)
		}
	}
}

// TestGifskiLadderDropsColourRungs: the generic gif ladder's palette rungs
// collapse for the gifski encoder (it quantises itself).
func TestGifskiLadderDropsColourRungs(t *testing.T) {
	master := enc.Master{Width: 320, Height: 240, FPS: 25, Frames: 100}
	out := recipe.Output{Format: "gif", Encoder: "gifski"}
	rungs := fitLadder(recipe.FormatGIF, out, master)
	if len(rungs) == 0 {
		t.Fatal("no rungs")
	}
	for _, r := range rungs {
		if r.Colors != 0 || r.Dither != "" {
			t.Errorf("gifski rung carries palette settings: %+v", r)
		}
	}
	plain := fitLadder(recipe.FormatGIF, recipe.Output{Format: "gif"}, master)
	if len(rungs) >= len(plain) {
		t.Errorf("gifski ladder (%d rungs) must be smaller than the palette ladder (%d): colour rungs collapse", len(rungs), len(plain))
	}
}

// TestKnobDescPhase4: fit candidate descriptions in the new formats' terms.
func TestKnobDescPhase4(t *testing.T) {
	if got := knobDesc(recipe.FormatMP4, fit.Rung{}, recipe.Output{Format: "mp4"}, 23); got != "crf 23" {
		t.Errorf("mp4 desc = %q", got)
	}
	if got := knobDesc(recipe.FormatWebM, fit.Rung{}, recipe.Output{Format: "webm"}, 40); got != "crf 40" {
		t.Errorf("webm desc = %q", got)
	}
	if got := knobDesc(recipe.FormatGIF, fit.Rung{}, recipe.Output{Format: "gif", Encoder: "gifski"}, 30); got != "gifski quality 70" {
		t.Errorf("gifski desc = %q", got)
	}
	if got := knobName(recipe.FormatMP4, recipe.Output{}); got != "crf" {
		t.Errorf("mp4 knob name = %q", got)
	}
}

// TestVideoFactsAndFlattened: helper behaviour the manifest depends on.
func TestVideoFactsAndFlattened(t *testing.T) {
	var item produced
	fillVideoFacts(&item, enc.Master{Width: 33, Height: 27, FPS: 12.5, Frames: 25})
	if item.width != 34 || item.height != 28 || item.frames != 25 || item.duration != 2 {
		t.Errorf("video facts = %+v", item)
	}
	for _, f := range []string{"jpeg", "mp4", "webm"} {
		if !flattenedFormat(f) {
			t.Errorf("flattenedFormat(%s) = false", f)
		}
	}
	for _, f := range []string{"gif", "webp", "apng", "avif", "png", "frames"} {
		if flattenedFormat(f) {
			t.Errorf("flattenedFormat(%s) = true", f)
		}
	}
}

// TestBouncedPreviewAdmission: a bounced plan is admitted like a reversed
// one — a buffer over MaxMasterBytes is refused with the actionable message.
func TestBouncedPreviewAdmission(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, fakeTools, Options{Concurrency: 1, MaxMasterBytes: 1 << 20})
	big := &graph.Plan{Width: 1920, Height: 1080, Frames: 600, Bounced: true}
	err := m.admitReversed(big, big.Frames, "this still")
	if err == nil || !errors.Is(err, ErrInvalidRecipe) || !strings.Contains(err.Error(), "EZLG_MAX_MASTER_BYTES") {
		t.Errorf("bounced admission err = %v", err)
	}
	small := &graph.Plan{Width: 32, Height: 32, Frames: 10, Bounced: true}
	if err := m.admitReversed(small, small.Frames, "this still"); err != nil {
		t.Errorf("small bounced plan refused: %v", err)
	}
	forward := &graph.Plan{Width: 1920, Height: 1080, Frames: 600}
	if err := m.admitReversed(forward, forward.Frames, "this still"); err != nil {
		t.Errorf("forward plan must not be admission-checked: %v", err)
	}
}

// TestGifskiQualityAndEncoder: small helper contracts.
func TestGifskiQualityAndEncoder(t *testing.T) {
	if gifskiQuality(0) != enc.DefaultGifskiQuality || gifskiQuality(101) != 100 || gifskiQuality(-3) != enc.DefaultGifskiQuality || gifskiQuality(55) != 55 {
		t.Error("gifskiQuality")
	}
	if !isGifskiOutput(recipe.Output{Encoder: " GifSki "}) || isGifskiOutput(recipe.Output{}) {
		t.Error("isGifskiOutput")
	}
}

// TestRunGifskiLoopNeedsGifsicle: a finite Output.Loop on the gifski path is
// refused when gifsicle is missing — gifski always writes an infinite loop
// and only a gifsicle pass can restate it, so silently delivering the file
// would misstate the recipe (and even pass the lint, which likes forever).
// The refusal fires before gifski is invoked; loop-forever passes the guard
// and fails only at exec on the deliberately bogus gifski path, proving the
// guard gates on the loop count alone.
func TestRunGifskiLoopNeedsGifsicle(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "no-such-gifski")
	m := NewManager(newTestStore(t), ffrun.Tools{Gifski: bogus}, Options{})
	out := filepath.Join(t.TempDir(), "out.gif")
	frames := []string{"f1.png"}

	err := m.runGifski(context.Background(), t.TempDir(), "", frames, 12.5, 90, 3, out)
	if err == nil || !strings.Contains(err.Error(), "gifsicle") {
		t.Fatalf("finite loop without gifsicle err = %v, want a refusal naming gifsicle", err)
	}

	err = m.runGifski(context.Background(), t.TempDir(), "", frames, 12.5, 90, 0, out)
	if err == nil || strings.Contains(err.Error(), "gifsicle") {
		t.Fatalf("loop-forever err = %v, want a gifski exec error, not the gifsicle refusal", err)
	}
}
