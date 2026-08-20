package jobs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/fit"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Phase 2 unit tests: ladder/knob mapping, file bookkeeping and the
// optimiser's argument derivation, all with fakes (no tools).

func TestExtForAndHelpers(t *testing.T) {
	for in, want := range map[string]string{"gif": "gif", "webp": "webp", "apng": "png", "avif": "avif", "png": "png", "jpeg": "jpg"} {
		if got := extFor(in); got != want {
			t.Errorf("extFor(%q) = %q, want %q", in, got, want)
		}
	}
	args := oneFrameArgs([]string{"-f", "rawvideo", "-i", "in", "-map", "[out]", "out.rgba"})
	if strings.Join(args, " ") != "-f rawvideo -i in -map [out] -frames:v 1 out.rgba" {
		t.Errorf("oneFrameArgs = %q", args)
	}
	if got := oneFrameArgs(nil); got != nil {
		t.Errorf("oneFrameArgs(nil) = %v", got)
	}
	if !isOptimizePreset(recipe.Output{Preset: " Optimize "}) || isOptimizePreset(recipe.Output{Preset: "emote"}) {
		t.Error("isOptimizePreset")
	}
	// Fit runs hold candidates (and per-variant intermediates) on scratch
	// until the search returns, so they reserve one master more.
	for _, c := range []struct {
		in   recipe.Output
		want int64
	}{
		{recipe.Output{Format: "gif"}, 1}, {recipe.Output{Format: "gif", FitBytes: 1}, 2},
		{recipe.Output{Format: "webp", FitBytes: 1}, 2}, {recipe.Output{Format: "apng"}, 1},
		{recipe.Output{Format: "apng", Colors: 128}, 2}, {recipe.Output{Format: "apng", FitBytes: 1}, 3},
		{recipe.Output{Format: "avif"}, 2}, {recipe.Output{Format: "avif", FitBytes: 1}, 3},
		{recipe.Output{Format: "frames"}, 2}, {recipe.Output{Format: "png"}, 1},
		{recipe.Output{Format: "png", FitBytes: 1}, 2},
	} {
		if got := scratchFactor(c.in); got != c.want {
			t.Errorf("scratchFactor(%+v) = %d, want %d", c.in, got, c.want)
		}
	}
	for in, want := range map[string]string{"": "png", "PNG": "png", "jpeg": "jpeg", "jpg": "jpeg", "webp": "webp"} {
		got, err := frameFormatOf(recipe.Output{FrameFormat: in})
		if err != nil || got != want {
			t.Errorf("frameFormatOf(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := frameFormatOf(recipe.Output{FrameFormat: "bmp"}); !errors.Is(err, ErrInvalidRecipe) {
		t.Errorf("bmp frames: %v", err)
	}
	if !strings.Contains(tooManyFramesMsg(2500), "2500") || !strings.Contains(tooManyFramesMsg(2500), "trim or lower fps") {
		t.Errorf("tooManyFramesMsg = %q", tooManyFramesMsg(2500))
	}
	if !isLimitRule("apng.sticker") || !isLimitRule("static.emote-dims") || !isLimitRule(RuleFitTarget) || isLimitRule("apng.plays-forever") || isLimitRule("gif.disposal") {
		t.Error("isLimitRule classification")
	}
}

// TestFitSizeReporting: a candidate whose only failing LevelError rule is
// the plain byte cap (*.size-limit) must report its real size to the fit
// engine — reporting it as fitOverTarget poisons the secant bracket and makes
// the search converge far below the budget at a needlessly harsh knob. Any
// other failing LevelError rule (structural, duration/dimension) still
// reports fitOverTarget so the candidate can never be chosen.
func TestFitSizeReporting(t *testing.T) {
	const size = int64(300000)
	mk := func(checks ...discordlint.Check) *fitCandidate {
		rep := discordlint.Report{Checks: checks}
		return &fitCandidate{bytes: size, report: rep, ok: !hasErrorCheck(rep)}
	}
	structFail := discordlint.Check{Rule: "gif.global-palette", Level: discordlint.LevelError, OK: false}
	durFail := discordlint.Check{Rule: "apng.sticker", Level: discordlint.LevelError, OK: false}
	warn := discordlint.Check{Rule: "gif.frame-count", Level: discordlint.LevelWarn, OK: false}
	pass := discordlint.Check{Rule: "gif.size-limit", Level: discordlint.LevelError, OK: true}

	// Clean candidates (and mere warnings) report their real size.
	if got := reportedSize(mk(pass, warn)); got != size {
		t.Errorf("clean candidate = %d, want %d", got, size)
	}
	// A size-limit-only failure reports the real size, for every format's rule.
	for _, rule := range []string{"gif.size-limit", "webp.size-limit", "apng.size-limit", "static.size-limit"} {
		fail := discordlint.Check{Rule: rule, Level: discordlint.LevelError, OK: false}
		if got := reportedSize(mk(fail, warn)); got != size {
			t.Errorf("%s-only failure = %d, want the real size %d", rule, got, size)
		}
	}
	// Structural / duration failures are over target, alone or combined with
	// the byte cap.
	sizeFail := discordlint.Check{Rule: "gif.size-limit", Level: discordlint.LevelError, OK: false}
	for name, cand := range map[string]*fitCandidate{
		"structural":        mk(structFail),
		"duration":          mk(durFail),
		"size + structural": mk(sizeFail, structFail),
	} {
		if got := reportedSize(cand); got != fitOverTarget {
			t.Errorf("%s failure = %d, want fitOverTarget", name, got)
		}
	}
	if onlySizeLimitFailed(discordlint.Report{Checks: []discordlint.Check{pass, warn}}) {
		t.Error("onlySizeLimitFailed with no failing error check")
	}
	if !onlySizeLimitFailed(discordlint.Report{Checks: []discordlint.Check{sizeFail, pass}}) {
		t.Error("onlySizeLimitFailed(size-limit only)")
	}
	if onlySizeLimitFailed(discordlint.Report{Checks: []discordlint.Check{sizeFail, durFail}}) {
		t.Error("onlySizeLimitFailed must be false with another failing error rule")
	}

	// The engine target is the budget clamped to the Discord cap so an
	// over-cap candidate can never win now that it reports its real size.
	if got := fitTarget(262144, discordlint.TargetEmote); got != 262144 {
		t.Errorf("fitTarget(262144, emote) = %d", got)
	}
	if got := fitTarget(1<<20, discordlint.TargetEmote); got != 262144 {
		t.Errorf("fitTarget(1 MiB, emote) = %d, want the emote cap", got)
	}
	if got := fitTarget(1000, discordlint.TargetSticker); got != 1000 {
		t.Errorf("fitTarget(1000, sticker) = %d, want the smaller budget", got)
	}
	if got := fitTarget(1<<20, ""); got != 1<<20 {
		t.Errorf("fitTarget(1 MiB, no target) = %d, want the budget", got)
	}
}

func TestVariantMapping(t *testing.T) {
	master := enc.Master{Width: 320, Height: 240, FPS: 25, Frames: 50, HasAlpha: true}
	if v := variantFor(master, 0, 0, 0); v != nil {
		t.Errorf("identity variant = %+v", v)
	}
	if v := variantFor(master, 25, 320, 0); v != nil {
		t.Errorf("master values must be a nil variant: %+v", v)
	}
	if v := variantFor(master, 30, 400, 300); v != nil {
		t.Errorf("upscale/faster must be a nil variant: %+v", v)
	}
	v := variantFor(master, 12.5, 160, 0)
	if v == nil || v.FPS != 12.5 || v.Width != 160 || v.Height != 0 {
		t.Fatalf("variant = %+v", v)
	}
	if got := variantFrames(master, v); got != 25 {
		t.Errorf("frames at 12.5 fps = %d, want 25", got)
	}
	if w, h := variantDims(master, v); w != 160 || h != 120 {
		t.Errorf("dims = %dx%d", w, h)
	}
	if got := variantFPS(master, v); got != 12.5 {
		t.Errorf("fps = %v", got)
	}
	if got := variantFPS(master, nil); got != 25 {
		t.Errorf("master fps = %v", got)
	}
	// A still never gets an fps stage.
	still := enc.Master{Width: 64, Height: 64, FPS: 25, Frames: 1}
	if v := variantFor(still, 10, 0, 0); v != nil {
		t.Errorf("still fps variant = %+v", v)
	}
	if variantKey(nil) != "master" || variantKey(v) == variantKey(&enc.Variant{FPS: 12.5}) {
		t.Error("variantKey")
	}
	// Height-only variant keeps aspect.
	hv := variantFor(master, 0, 0, 120)
	if hv == nil || hv.Height != 120 {
		t.Fatalf("height variant = %+v", hv)
	}
	if w, h := variantDims(master, hv); w != 160 || h != 120 {
		t.Errorf("height variant dims = %dx%d", w, h)
	}
}

func TestKnobMapping(t *testing.T) {
	if qualityFromKnob(20) != 80 || qualityFromKnob(0) != 100 || qualityFromKnob(150) != 1 {
		t.Error("qualityFromKnob")
	}
	// The APNG colour search is floored at 64 (§5.4); explicit user bases
	// below 64 are kept as-is instead of halved further.
	r := fit.Rung{Colors: 256}
	for knob, want := range map[int]int{0: 256, 1: 128, 2: 64, 3: 64, 9: 64} {
		if got := apngColors(r, recipe.Output{}, knob); got != want {
			t.Errorf("apngColors(256, %d) = %d, want %d", knob, got, want)
		}
	}
	if got := apngColors(fit.Rung{}, recipe.Output{Colors: 64}, 1); got != 64 {
		t.Errorf("apngColors(request 64, step 1) = %d, want the 64 floor", got)
	}
	if got := apngColors(fit.Rung{}, recipe.Output{Colors: 32}, 2); got != 32 {
		t.Errorf("apngColors(request 32, step 2) = %d, want the user's own 32", got)
	}
	if got := apngColors(fit.Rung{}, recipe.Output{}, 0); got != 256 {
		t.Errorf("apngColors default = %d", got)
	}
	if got := knobDesc("gif", r, recipe.Output{}, 40); got != "lossy 40" {
		t.Errorf("gif desc = %q", got)
	}
	if got := knobDesc("apng", r, recipe.Output{}, 1); got != "palette 256 → 128" {
		t.Errorf("apng desc = %q", got)
	}
	if got := knobDesc("apng", r, recipe.Output{}, 0); got != "" {
		t.Errorf("apng desc at step 0 = %q, want none", got)
	}
	if got := knobDesc("webp", r, recipe.Output{}, 30); got != "q 70" {
		t.Errorf("webp desc = %q", got)
	}
	// png: 0 = keep full colour; the first step of a full-colour rung drops
	// to the default palette, later steps halve.
	for knob, want := range map[int]int{0: 0, 1: 256, 2: 128, 3: 64} {
		if got := pngColors(fit.Rung{}, recipe.Output{}, knob); got != want {
			t.Errorf("pngColors(full colour, %d) = %d, want %d", knob, got, want)
		}
	}
	if got := pngColors(fit.Rung{Colors: 128}, recipe.Output{}, 1); got != 64 {
		t.Errorf("pngColors(rung 128, step 1) = %d", got)
	}
	if got := pngColors(fit.Rung{}, recipe.Output{Colors: 64}, 1); got != 32 {
		t.Errorf("pngColors(request 64, step 1) = %d", got)
	}
	if got := pngColors(fit.Rung{}, recipe.Output{}, 9); got != 2 {
		t.Errorf("pngColors floor = %d, want 2", got)
	}
	if got := knobDesc("png", fit.Rung{}, recipe.Output{}, 0); got != "" {
		t.Errorf("png desc at step 0 = %q, want none", got)
	}
	if got := knobDesc("png", fit.Rung{}, recipe.Output{}, 1); got != "full colour → 256" {
		t.Errorf("png full-colour desc = %q", got)
	}
	if got := knobDesc("png", fit.Rung{Colors: 128}, recipe.Output{}, 1); got != "palette 128 → 64" {
		t.Errorf("png palette desc = %q", got)
	}
	if knobName("gif") != "lossy" || knobName("apng") != "colour step" || knobName("png") != "colour step" || knobName("avif") != "quality knob" {
		t.Error("knobName")
	}
	// A truecolour probe rung (RGBA APNG, lossless WebP) carries no knob desc.
	if got := knobDesc("webp", fit.Rung{Truecolor: true}, recipe.Output{}, 0); got != "" {
		t.Errorf("truecolor webp desc = %q, want none", got)
	}
	if got := knobDesc("apng", fit.Rung{Truecolor: true}, recipe.Output{}, 0); got != "" {
		t.Errorf("truecolor apng desc = %q, want none", got)
	}
	// fitKnob moves the mild probe onto the user's own settings so the fit
	// search never degrades below what already fits.
	if k := fitKnob("gif", recipe.Output{}); k.Mild != 0 || k.Harsh != 150 {
		t.Errorf("gif lossy 0 knob = %+v, want mild 0 (lossy 0)", k)
	}
	if k := fitKnob("gif", recipe.Output{Lossy: 80}); k.Mild != 80 || k.Harsh != 150 {
		t.Errorf("gif lossy 80 knob = %+v", k)
	}
	if k := fitKnob("gif", recipe.Output{Lossy: 180}); k.Mild != 180 || k.Harsh != 200 {
		t.Errorf("gif lossy 180 knob = %+v, want the harsh probe moved to max", k)
	}
	if k := fitKnob("webp", recipe.Output{}); k.Mild != 20 { // default q80
		t.Errorf("webp default knob = %+v", k)
	}
	if k := fitKnob("webp", recipe.Output{Quality: 95}); k.Mild != 5 {
		t.Errorf("webp q95 knob = %+v", k)
	}
	if k := fitKnob("webp", recipe.Output{Quality: 20}); k.Mild != 80 || k.Harsh != 90 {
		t.Errorf("webp q20 knob = %+v", k)
	}
	if k := fitKnob("avif", recipe.Output{}); k.Mild != 40 { // DefaultAVIFQuality 60
		t.Errorf("avif default knob = %+v", k)
	}
	if k := fitKnob("jpeg", recipe.Output{}); k.Mild != 10 { // enc.DefaultJPEGQuality 90
		t.Errorf("jpeg default knob = %+v", k)
	}
	if k := fitKnob("apng", recipe.Output{Colors: 64}); k.Mild != 0 {
		t.Errorf("apng knob = %+v", k)
	}
	if fitProgressPct(0) != pctEncodeStart || fitProgressPct(8) <= fitProgressPct(1) || fitProgressPct(1000) > pctEncodeEnd {
		t.Error("fitProgressPct shape")
	}
	if fitParallel() < 1 {
		t.Error("fitParallel")
	}
}

// TestFitLadderSelection pins which ladder each target/format gets and the
// knob map for mixed ladders.
func TestFitLadderSelection(t *testing.T) {
	master := enc.Master{Width: 128, Height: 128, FPS: 25, Frames: 75, HasAlpha: true}
	formatsOf := func(rungs []fit.Rung, request string) map[string]int {
		out := map[string]int{}
		for _, r := range rungs {
			out[effectiveFormat(r, request)]++
		}
		return out
	}

	emote := fitLadder("gif", recipe.Output{Format: "gif", Target: "emote", FitBytes: 262144}, master)
	if len(emote) < 3 || formatsOf(emote, "gif")["gif"] != len(emote) {
		t.Errorf("emote gif ladder = %+v", emote)
	}
	want := fit.EmoteGIF(25, 128, 128)
	if len(emote) != len(want) || emote[0].Label != want[0].Label {
		t.Errorf("emote gif ladder is not EmoteGIF: %+v vs %+v", emote, want)
	}
	// Emote AVIF uses the WebP geometry ladder; rungs carry no format so the
	// request format (avif) applies.
	avif := fitLadder("avif", recipe.Output{Format: "avif", Target: "emote", FitBytes: 1}, master)
	if len(avif) == 0 || formatsOf(avif, "avif")["avif"] != len(avif) {
		t.Errorf("emote avif ladder = %+v", avif)
	}
	if knobs := knobsFor(avif, "avif", recipe.Output{}); knobs["avif"].Name != fit.KnobQuality {
		t.Errorf("avif knob = %+v", knobs)
	}

	// Sticker APNG: apng rungs then gif rungs, knobs for both.
	stick := fitLadder("apng", recipe.Output{Format: "apng", Target: "sticker", FitBytes: 524288}, enc.Master{Width: 320, Height: 320, FPS: 25, Frames: 50, HasAlpha: true})
	f := formatsOf(stick, "apng")
	if f["apng"] == 0 || f["gif"] == 0 {
		t.Fatalf("sticker ladder formats = %v (%+v)", f, stick)
	}
	if effectiveFormat(stick[0], "apng") != "apng" || effectiveFormat(stick[len(stick)-1], "apng") != "gif" {
		t.Errorf("sticker ladder order: first %q, last %q", effectiveFormat(stick[0], "apng"), effectiveFormat(stick[len(stick)-1], "apng"))
	}
	knobs := knobsFor(stick, "apng", recipe.Output{Lossy: 40})
	if knobs["apng"].Name != fit.KnobColourStep || knobs["gif"].Name != fit.KnobLossy {
		t.Errorf("sticker knobs = %+v", knobs)
	}
	if knobs["gif"].Mild != 40 {
		t.Errorf("sticker gif knob must start at the user's lossy: %+v", knobs["gif"])
	}
	// Forced GIF sticker: GIF rungs only.
	stickGIF := fitLadder("gif", recipe.Output{Format: "gif", Target: "sticker", FitBytes: 524288}, enc.Master{Width: 320, Height: 320, FPS: 25, Frames: 50})
	if len(stickGIF) == 0 || formatsOf(stickGIF, "gif")["gif"] != len(stickGIF) {
		t.Errorf("forced gif sticker ladder = %+v", stickGIF)
	}

	// Generic for a chat webp; FitKeepFPS/FitKeepSize shrink it.
	gen := fitLadder("webp", recipe.Output{Format: "webp", FitBytes: 1}, enc.Master{Width: 480, Height: 270, FPS: 30, Frames: 90})
	kept := fitLadder("webp", recipe.Output{Format: "webp", FitBytes: 1, FitKeepFPS: true, FitKeepSize: true}, enc.Master{Width: 480, Height: 270, FPS: 30, Frames: 90})
	if len(gen) <= len(kept) {
		t.Errorf("generic ladder %d rungs, with keep flags %d", len(gen), len(kept))
	}
	for _, r := range kept {
		if r.FPS != 0 || r.Width != 0 || r.Height != 0 {
			t.Errorf("keep flags left a changing rung: %+v", r)
		}
	}
	// Emote presets honour the keep flags too.
	keptEmote := fitLadder("gif", recipe.Output{Format: "gif", Target: "emote", FitBytes: 1, FitKeepSize: true}, master)
	for _, r := range keptEmote {
		if r.Width != 0 || r.Height != 0 {
			t.Errorf("FitKeepSize left a downscale rung: %+v", r)
		}
	}

	// A still: fps rungs collapse, no duplicates.
	stillLadder := fitLadder("gif", recipe.Output{Format: "gif", Target: "emote", FitBytes: 1}, enc.Master{Width: 128, Height: 128, FPS: 25, Frames: 1})
	seen := map[string]bool{}
	for _, r := range stillLadder {
		if r.FPS != 0 {
			t.Errorf("still ladder has an fps rung: %+v", r)
		}
		key := fmt.Sprintf("%d|%d|%d|%s", r.Width, r.Height, r.Colors, r.Dither)
		if seen[key] {
			t.Errorf("duplicate rung %+v", r)
		}
		seen[key] = true
	}
	// A format without a preset ladder falls back to at least one rung.
	if l := fitLadder("jpeg", recipe.Output{Format: "jpeg", FitBytes: 1}, enc.Master{Width: 64, Height: 64, Frames: 1}); len(l) == 0 {
		t.Error("jpeg ladder empty")
	}
	// Static PNG fit (a supported format): the generic ladder with the
	// colour-step knob and no fps rungs.
	if !fitFormats["png"] {
		t.Error("png must be a fit format")
	}
	pngLadder := fitLadder("png", recipe.Output{Format: "png", FitBytes: 1}, enc.Master{Width: 128, Height: 128, FPS: 25, Frames: 1})
	if len(pngLadder) == 0 {
		t.Fatal("png ladder empty")
	}
	for _, r := range pngLadder {
		if r.FPS != 0 {
			t.Errorf("png ladder has an fps rung: %+v", r)
		}
	}
	if knobs := knobsFor(pngLadder, "png", recipe.Output{}); knobs["png"].Name != fit.KnobColourStep {
		t.Errorf("png knob = %+v", knobs)
	}

	// Output.Lossless prepends a single lossless probe as the mildest webp
	// rung; the lossy rungs follow for the fallback search.
	ll := fitLadder("webp", recipe.Output{Format: "webp", FitBytes: 1, Lossless: true}, enc.Master{Width: 480, Height: 270, FPS: 30, Frames: 90})
	if len(ll) < 2 || !ll[0].Truecolor || ll[0].Label != "lossless" || ll[0].Knob == nil {
		t.Fatalf("lossless webp ladder = %+v", ll)
	}
	for _, r := range ll[1:] {
		if r.Truecolor {
			t.Errorf("more than one lossless rung: %+v", r)
		}
	}

	// Without pngquant the indexed APNG rungs are dropped up front; the RGBA
	// probes and the GIF fallback stay.
	kept2, dropped := dropIndexedAPNGRungs(stick, "apng")
	if dropped == 0 || len(kept2) == 0 {
		t.Fatalf("dropIndexedAPNGRungs(sticker) = %d kept, %d dropped", len(kept2), dropped)
	}
	var rgba, gifRungs bool
	for _, r := range kept2 {
		if effectiveFormat(r, "apng") == "apng" {
			if !r.Truecolor {
				t.Errorf("indexed apng rung survived: %+v", r)
			}
			rgba = true
		}
		if effectiveFormat(r, "apng") == "gif" {
			gifRungs = true
		}
	}
	if !rgba || !gifRungs {
		t.Errorf("kept rungs lost the RGBA probes or the GIF fallback: %+v", kept2)
	}
}

// TestFitLadderClampsGenericColours: the generic ladder never quantises to
// MORE colours than the user asked for — colour rungs at or above
// Output.Colors collapse onto "as requested", palettes strictly below it
// survive, so the ladder stays monotone.
func TestFitLadderClampsGenericColours(t *testing.T) {
	master := enc.Master{Width: 480, Height: 270, FPS: 30, Frames: 90}
	clamped := fitLadder("gif", recipe.Output{Format: "gif", FitBytes: 1, Colors: 64}, master)
	for _, r := range clamped {
		if r.Colors != 0 {
			t.Errorf("rung %q keeps %d colours although the user asked for 64", r.Label, r.Colors)
		}
		if strings.Contains(r.Label, "colours") {
			t.Errorf("label %q still names a palette", r.Label)
		}
	}
	free := fitLadder("gif", recipe.Output{Format: "gif", FitBytes: 1}, master)
	if len(clamped) >= len(free) {
		t.Errorf("clamped ladder has %d rungs, unclamped %d — the pure colour rungs must collapse", len(clamped), len(free))
	}
	// A palette strictly below the user's is a real degrade step and stays.
	mid := fitLadder("apng", recipe.Output{Format: "apng", FitBytes: 1, Colors: 100}, master)
	var has64, has128 bool
	for _, r := range mid {
		switch r.Colors {
		case 64:
			has64 = true
			if r.Knob == nil {
				t.Errorf("kept colour rung lost its own colour-step knob: %+v", r)
			}
		case 128:
			has128 = true
		}
	}
	if !has64 || has128 {
		t.Errorf("colours 100: ladder = %+v (want the 64 rung, not the 128 one)", mid)
	}
}

func TestFitCandidatesBookkeeping(t *testing.T) {
	c := newFitCandidates()
	if c.smallest() != nil || c.count() != 0 {
		t.Error("empty")
	}
	sizeFail := discordlint.Report{Checks: []discordlint.Check{{Rule: "gif.size-limit", Level: discordlint.LevelError}}}
	structFail := discordlint.Report{Checks: []discordlint.Check{{Rule: "gif.global-palette", Level: discordlint.LevelError}}}
	c.add(&fitCandidate{path: "a", bytes: 300, report: sizeFail})
	c.add(&fitCandidate{path: "b", bytes: 100, report: structFail})
	c.add(&fitCandidate{path: "c", bytes: 200, report: sizeFail})
	if got := c.smallest(); got == nil || got.path != "c" {
		t.Errorf("smallest = %+v, want the smallest structurally sound one (c)", got)
	}
	if c.get("b") == nil || c.get("zzz") != nil || c.count() != 3 || len(c.paths()) != 3 {
		t.Error("get/count/paths")
	}
}

// fakeCandidateFile writes a small file for a fit candidate.
func fakeCandidateFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte{0xAB}, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFitDeliverables drives the result assembly with fake candidates: the
// winner becomes out.<ext>, the runner-ups alt1/alt2, files are moved out of
// the fit dir, and the no-fit path delivers the smallest attempt with a
// failing fit.target check.
func TestFitDeliverables(t *testing.T) {
	m := NewManager(newTestStore(t), ffrun.Tools{}, Options{})
	scratch := t.TempDir()
	fitDir := filepath.Join(scratch, fitDirName)
	os.MkdirAll(fitDir, 0o755)
	okRep := discordlint.Report{Format: "gif", OK: true, Checks: []discordlint.Check{{Rule: "gif.size-limit", Level: discordlint.LevelError, OK: true}}}
	cands := newFitCandidates()
	mk := func(name string, size int, rung fit.Rung, knob int, rep discordlint.Report) *fitCandidate {
		c := &fitCandidate{path: fakeCandidateFile(t, fitDir, name, size), format: "gif", bytes: int64(size), report: rep, rung: rung, knob: knob, ok: rep.OK}
		cands.add(c)
		return c
	}
	win := mk("c0001.gif", 900, fit.Rung{Label: "25 fps · 256 colours"}, 40, okRep)
	winPath := win.path
	a1 := mk("c0002.gif", 800, fit.Rung{Label: "20 fps · 128 colours"}, 30, okRep)
	a2 := mk("c0003.gif", 700, fit.Rung{Label: "16.7 fps · 128 colours"}, 20, okRep)
	mk("c0004.gif", 600, fit.Rung{Label: "12.5 fps"}, 10, okRep) // not delivered
	describe := func(c *fitCandidate) string { return "fit at " + c.rung.Label + fmt.Sprintf(" · lossy %d", c.knob) }

	best := &fit.Candidate{Path: winPath, Bytes: 900}
	alts := []fit.Candidate{{Path: a1.path}, {Path: a2.path}, {Path: winPath}}
	items, err := m.fitDeliverables(context.Background(), cands, best, alts, 1000, nil, nil, scratch, enc.Master{}, describe)
	if err != nil {
		t.Fatalf("fitDeliverables: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want primary + 2 alternatives", len(items))
	}
	if items[0].name != "out.gif" || items[0].kind != FileKindOutput || items[0].index != 0 || items[0].desc != "fit at 25 fps · 256 colours · lossy 40" {
		t.Errorf("primary = %+v", items[0])
	}
	if items[1].name != "alt1.gif" || items[1].kind != FileKindAlternative || items[1].index != 1 || !strings.HasPrefix(items[1].desc, "fit at 20 fps") {
		t.Errorf("alt1 = %+v", items[1])
	}
	if items[2].name != "alt2.gif" || items[2].index != 2 {
		t.Errorf("alt2 = %+v", items[2])
	}
	for _, it := range items {
		if filepath.Dir(it.path) != scratch {
			t.Errorf("%s not moved out of the fit dir: %s", it.name, it.path)
		}
		if fi, err := os.Stat(it.path); err != nil || fi.Size() == 0 {
			t.Errorf("%s missing: %v", it.name, err)
		}
		if it.report == nil || !it.report.OK || !it.verify {
			t.Errorf("%s report/verify: %+v", it.name, it)
		}
	}
	if _, err := os.Stat(winPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("winner still in the fit dir")
	}

	// No fit: the smallest attempt is delivered with a failing fit.target
	// check and a clear description.
	cands2 := newFitCandidates()
	fitDir2 := filepath.Join(scratch, "fit2")
	os.MkdirAll(fitDir2, 0o755)
	overRep := discordlint.Report{Format: "gif", Target: "emote", Checks: []discordlint.Check{{Rule: "gif.size-limit", Level: discordlint.LevelError, OK: false}}}
	big := &fitCandidate{path: fakeCandidateFile(t, fitDir2, "x1.gif", 5000), format: "gif", bytes: 5000, report: overRep, rung: fit.Rung{Label: "25 fps"}, knob: 100}
	small := &fitCandidate{path: fakeCandidateFile(t, fitDir2, "x2.gif", 3000), format: "gif", bytes: 3000, report: overRep, rung: fit.Rung{Label: "10 fps · 32 colours"}, knob: 200}
	cands2.add(big)
	cands2.add(small)
	items, err = m.fitDeliverables(context.Background(), cands2, nil, nil, 1024, []string{"25 fps", "20 fps"}, nil, scratch, enc.Master{}, describe)
	if err != nil {
		t.Fatalf("no-fit deliverables: %v", err)
	}
	if len(items) != 1 || items[0].name != "out.gif" || items[0].kind != FileKindOutput {
		t.Fatalf("no-fit items = %+v", items)
	}
	p := items[0]
	if fi, _ := os.Stat(p.path); fi == nil || fi.Size() != 3000 {
		t.Errorf("no-fit primary is not the smallest attempt: %+v", p)
	}
	for _, want := range []string{"cannot fit under 1 KiB", "smallest attempt is 2.9 KiB", "10 fps · 32 colours", "lossy 200", "2 rungs skipped"} {
		if !strings.Contains(p.desc, want) {
			t.Errorf("desc %q lacks %q", p.desc, want)
		}
	}
	if p.report.OK {
		t.Error("no-fit report must not be OK")
	}
	var found bool
	for _, c := range p.report.Checks {
		if c.Rule == RuleFitTarget && !c.OK && c.Level == discordlint.LevelError && strings.Contains(c.Detail, "cannot fit") {
			found = true
		}
	}
	if !found {
		t.Errorf("fit.target check missing: %+v", p.report.Checks)
	}
	// Nothing encoded at all: a clear error naming the failures.
	if _, err := m.fitDeliverables(context.Background(), newFitCandidates(), nil, nil, 1024, nil, []string{"rung 1: gifsicle exploded"}, scratch, enc.Master{}, describe); err == nil || !strings.Contains(err.Error(), "gifsicle exploded") {
		t.Errorf("no candidates: %v", err)
	}
}

// TestFitDeliverablesNamesBlockingRule: when every attempt is under the byte
// budget but fails a non-size Discord rule, the no-fit headline names the
// real rule instead of the contradictory "cannot fit under N KiB" (and no
// fit.target check is appended — the real failing check already stands).
func TestFitDeliverablesNamesBlockingRule(t *testing.T) {
	m := NewManager(newTestStore(t), ffrun.Tools{}, Options{})
	scratch := t.TempDir()
	fitDir := filepath.Join(scratch, fitDirName)
	os.MkdirAll(fitDir, 0o755)
	describe := func(c *fitCandidate) string { return "fit at " + c.rung.Label }

	rep := discordlint.Report{Format: "gif", Target: discordlint.TargetSticker, Checks: []discordlint.Check{
		{Rule: "gif.size-limit", Level: discordlint.LevelError, OK: true},
		{Rule: "gif.sticker", Level: discordlint.LevelError, OK: false, Detail: "sticker duration over 5 s (7.2 s)"},
	}}
	cands := newFitCandidates()
	cands.add(&fitCandidate{path: fakeCandidateFile(t, fitDir, "x1.gif", 1000), format: "gif", bytes: 1000, report: rep, rung: fit.Rung{Label: "25 fps"}})
	items, err := m.fitDeliverables(context.Background(), cands, nil, nil, 524288, nil, nil, scratch, enc.Master{}, describe)
	if err != nil {
		t.Fatal(err)
	}
	p := items[0]
	for _, want := range []string{"no candidate passes the Discord sticker rules", "sticker duration over 5 s", "smallest attempt is 1000 B", "25 fps"} {
		if !strings.Contains(p.desc, want) {
			t.Errorf("desc %q lacks %q", p.desc, want)
		}
	}
	if strings.Contains(p.desc, "cannot fit") {
		t.Errorf("desc %q still claims a size failure", p.desc)
	}
	if p.report.OK {
		t.Error("report must not be OK")
	}
	for _, c := range p.report.Checks {
		if c.Rule == RuleFitTarget {
			t.Errorf("fit.target check appended although the attempt is under the budget: %+v", c)
		}
	}

	// A candidate under the budget with no failing non-size rule (the margin
	// edge) keeps the size wording and the fit.target check.
	cands2 := newFitCandidates()
	clean := discordlint.Report{Format: "gif", OK: true, Checks: []discordlint.Check{{Rule: "gif.size-limit", Level: discordlint.LevelError, OK: true}}}
	cands2.add(&fitCandidate{path: fakeCandidateFile(t, fitDir, "x2.gif", 1000), format: "gif", bytes: 1000, report: clean, rung: fit.Rung{Label: "25 fps"}, ok: true})
	items, err = m.fitDeliverables(context.Background(), cands2, nil, nil, 1024, nil, nil, scratch, enc.Master{}, describe)
	if err != nil {
		t.Fatal(err)
	}
	p = items[0]
	if !strings.Contains(p.desc, "cannot fit under 1 KiB") {
		t.Errorf("margin edge desc = %q", p.desc)
	}
	var flagged bool
	for _, c := range p.report.Checks {
		if c.Rule == RuleFitTarget && !c.OK {
			flagged = true
		}
	}
	if !flagged || p.report.OK {
		t.Errorf("margin edge must carry the fit.target check: %+v", p.report.Checks)
	}
}

// TestParseGIFFacts: the LZW-free block walk agrees with image/gif on files
// Go can decode and still parses odd-but-valid files Go rejects (frame rect
// outside the logical screen, undecodable frame data).
func TestParseGIFFacts(t *testing.T) {
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i, delay := range []int{10, 0, 25} {
		fr := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
		fr.SetColorIndex(i, 0, uint8(i+1))
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, delay)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	facts, err := parseGIFFacts(buf.Bytes())
	if err != nil {
		t.Fatalf("parseGIFFacts: %v", err)
	}
	dec, err := gif.DecodeAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	wantColors := 0
	if p, ok := dec.Config.ColorModel.(color.Palette); ok {
		wantColors = len(p)
	}
	for _, fr := range dec.Image {
		wantColors = max(wantColors, len(fr.Palette))
	}
	if facts.frames != len(dec.Image) || facts.colors != wantColors {
		t.Errorf("facts = %+v, want %d frames, %d colours", facts, len(dec.Image), wantColors)
	}
	if fmt.Sprint(facts.delays) != fmt.Sprint(dec.Delay) {
		t.Errorf("delays = %v, want %v", facts.delays, dec.Delay)
	}

	// Odd but valid: frame rect outside the 4x4 logical screen, an unknown
	// extension label and junk LZW data — gifsicle copes, so must the walk.
	odd := []byte("GIF89a")
	odd = append(odd, 4, 0, 4, 0, 0x80, 0, 0)                // LSD: 4x4, GCT of 2
	odd = append(odd, 0, 0, 0, 255, 255, 255)                // GCT
	odd = append(odd, 0x21, 0xF9, 4, 0, 10, 0, 0, 0)         // GCE: delay 10 cs
	odd = append(odd, 0x21, 0x01, 2, 0xAA, 0xBB, 0)          // unknown-ish extension
	odd = append(odd, 0x2C, 2, 0, 2, 0, 4, 0, 4, 0, 0x81)    // image at (2,2), 4x4 — outside the screen; LCT of 4
	odd = append(odd, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12) // LCT
	odd = append(odd, 2, 3, 0x01, 0x02, 0x03, 0)             // LZW min code + junk sub-block
	odd = append(odd, 0x3B)
	facts, err = parseGIFFacts(odd)
	if err != nil {
		t.Fatalf("odd-but-valid gif: %v", err)
	}
	if facts.frames != 1 || len(facts.delays) != 1 || facts.delays[0] != 10 || facts.colors != 4 {
		t.Errorf("odd facts = %+v", facts)
	}
	if _, err := gif.DecodeAll(bytes.NewReader(odd)); err == nil {
		t.Error("image/gif accepted the odd file — the walk is no longer needed for it")
	}

	// Errors.
	for name, data := range map[string][]byte{
		"junk header":   []byte("GIF89a-not-really"),
		"too short":     []byte("GIF89a"),
		"no trailer":    odd[:len(odd)-1],
		"truncated gce": odd[:25],
		"truncated lct": odd[:50],
	} {
		if _, err := parseGIFFacts(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestAdmitOptimizeScratch: the optimize fit search reserves scratch for its
// concurrent candidates (and releases it); without a fit budget or a known
// source size nothing is reserved; a too-small budget is refused up-front.
func TestAdmitOptimizeScratch(t *testing.T) {
	st := newTestStore(t)
	b, err := st.PutBlob(bytes.NewReader(bytes.Repeat([]byte{0x47}, 1000)), "src.gif")
	if err != nil {
		t.Fatal(err)
	}
	j := &job{snap: Job{ID: "t"}, cancel: func() {}, subs: map[int]*subscriber{}}
	ctx := context.Background()
	m := NewManager(st, fakeTools, Options{ScratchBudgetBytes: -1}) // unlimited

	rel, err := m.admitOptimizeScratch(ctx, j, b.Path, recipe.Output{Format: "gif", Preset: "optimize"})
	if err != nil || m.scratch.Used() != 0 {
		t.Fatalf("no fit budget: err=%v used=%d", err, m.scratch.Used())
	}
	rel()

	rel, err = m.admitOptimizeScratch(ctx, j, b.Path, recipe.Output{Format: "gif", Preset: "optimize", FitBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1000) * int64(fitParallel()+1); m.scratch.Used() < want {
		t.Errorf("fit admission reserved %d, want at least %d", m.scratch.Used(), want)
	}
	rel()
	rel() // idempotent
	if m.scratch.Used() != 0 {
		t.Errorf("budget leaked: %d", m.scratch.Used())
	}

	small := NewManager(st, fakeTools, Options{ScratchBudgetBytes: 100})
	if _, err := small.admitOptimizeScratch(ctx, j, b.Path, recipe.Output{FitBytes: 1 << 20}); err == nil || !strings.Contains(err.Error(), "scratch filesystem") {
		t.Errorf("small budget: %v", err)
	}

	rel, err = m.admitOptimizeScratch(ctx, j, filepath.Join(t.TempDir(), "missing.gif"), recipe.Output{FitBytes: 1})
	if err != nil || m.scratch.Used() != 0 {
		t.Fatalf("missing source: err=%v used=%d", err, m.scratch.Used())
	}
	rel()
}

func TestFileForFacts(t *testing.T) {
	m := NewManager(newTestStore(t), ffrun.Tools{}, Options{})
	dir := t.TempDir()
	p := fakeCandidateFile(t, dir, "out.gif", 123)
	rep := &discordlint.Report{Width: 64, Height: 48, Frames: 20, DurationMS: 2000, OK: true}
	f, err := m.fileFor(strings.Repeat("a", 64), produced{path: p, name: "out.gif", format: "gif", kind: FileKindOutput, desc: "fit at x", report: rep, width: 1, height: 1, frames: 1, fps: 99}, discordlint.TargetEmote)
	if err != nil {
		t.Fatal(err)
	}
	if f.Width != 64 || f.Height != 48 || f.Frames != 20 || f.Duration != 2 || f.FPS != 10 || f.Bytes != 123 || f.Limit != 262144 || f.Kind != FileKindOutput || f.Desc != "fit at x" {
		t.Errorf("file = %+v", f)
	}
	if f.URL != "/out/"+strings.Repeat("a", 64)+"/out.gif" {
		t.Errorf("url = %q", f.URL)
	}
	// Without a report the encoder's facts stand; frames/archives carry no limit.
	f, _ = m.fileFor(strings.Repeat("a", 64), produced{path: p, name: "f00001.png", format: "png", kind: FileKindFrame, index: 1, width: 64, height: 48, frames: 1}, discordlint.TargetEmote)
	if f.Width != 64 || f.Frames != 1 || f.Limit != 0 || f.Index != 1 || f.Report != nil {
		t.Errorf("frame file = %+v", f)
	}
	if _, err := m.fileFor("h", produced{path: filepath.Join(dir, "missing")}, ""); err == nil {
		t.Error("missing file accepted")
	}
}

func TestWriteStoreZip(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i := 1; i <= 3; i++ {
		files = append(files, fakeCandidateFile(t, dir, fmt.Sprintf("f%05d.png", i), 100*i))
	}
	zp := filepath.Join(dir, "frames.zip")
	if err := writeStoreZip(zp, files); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(zp)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != 3 {
		t.Fatalf("%d entries", len(zr.File))
	}
	for i, zf := range zr.File {
		if zf.Name != fmt.Sprintf("f%05d.png", i+1) || zf.Method != zip.Store || zf.UncompressedSize64 != uint64(100*(i+1)) {
			t.Errorf("entry %d = %s method %d size %d", i, zf.Name, zf.Method, zf.UncompressedSize64)
		}
	}
	if err := writeStoreZip(filepath.Join(dir, "bad.zip"), []string{filepath.Join(dir, "nope.png")}); err == nil {
		t.Error("missing frame accepted")
	}
}

func TestDropEveryNAndOptimizeLadder(t *testing.T) {
	cases := []struct {
		src, want float64
		n         int
		err       bool
	}{
		{20, 0, 0, false}, {20, 20, 0, false}, {20, 25, 0, false},
		{20, 10, 2, false}, {20, 13.333, 3, false}, {20, 15, 4, false},
		{12.5, 6.25, 2, false}, {25, 12.5, 2, false},
		{20, 5, 0, true}, {20, 7, 0, true}, {20, 18, 0, true},
	}
	for _, c := range cases {
		n, err := dropEveryN(c.src, c.want)
		if (err != nil) != c.err || n != c.n {
			t.Errorf("dropEveryN(%v, %v) = %d, %v; want %d err=%v", c.src, c.want, n, err, c.n, c.err)
		}
		if err != nil && !strings.Contains(err.Error(), "10, 13.3, 15") {
			t.Errorf("error must list the reachable rates: %v", err)
		}
	}
	if keptFrames(12, 2) != 6 || keptFrames(12, 3) != 8 || keptFrames(12, 0) != 12 {
		t.Error("keptFrames")
	}
	if keptFraction(0) != 1 || keptFraction(2) != 0.5 || keptFraction(3) < 0.66 {
		t.Error("keptFraction")
	}
	if ordinal(2) != "2nd" || ordinal(3) != "3rd" || ordinal(4) != "4th" {
		t.Error("ordinal")
	}
	for in, want := range map[string]string{"": "o8", "bayer": "o8", "none": "", "floyd_steinberg": "floyd-steinberg", "sierra2_4a": "floyd-steinberg"} {
		if got := gifsicleDither(in); got != want {
			t.Errorf("gifsicleDither(%q) = %q, want %q", in, got, want)
		}
	}
	o := gifsicleOptimizeOptions(recipe.Output{Lossy: 30, Colors: 64, Loop: 2}, 30, 64, 2)
	if o.Lossy != 30 || o.Colors != 64 || o.Dither != "o8" || o.DropEveryN != 2 || o.Loop != 2 || !o.Careful {
		t.Errorf("options = %+v", o)
	}
	if d := optimizeDesc(o, 12); !strings.Contains(d, "lossy 30") || !strings.Contains(d, "64 colours") || !strings.Contains(d, "every 2nd frame dropped (6 of 12 kept)") {
		t.Errorf("desc = %q", d)
	}
	if d := optimizeDesc(enc.GifsicleOptimizeOptions{}, 12); d != "gifsicle -O2 (lossless)" {
		t.Errorf("lossless desc = %q", d)
	}

	// Ladder: base drop first, then harsher drops; source colours first.
	facts := gifFacts{frames: 12, colors: 256}
	rungs, drops := optimizeLadder(0, 0, facts, false)
	var labels []string
	for _, r := range rungs {
		labels = append(labels, fmt.Sprintf("%d/%d", drops[r.Label], r.Colors))
	}
	if strings.Join(labels, " ") != "0/0 0/128 0/64 3/0 3/128 3/64 2/0 2/128 2/64" {
		t.Errorf("ladder = %v", labels)
	}
	for _, r := range rungs {
		if r.Format != "gif" || r.Label == "" || r.FPS != 0 {
			t.Errorf("rung = %+v", r)
		}
	}
	// A base drop of 2 (the user asked for half the fps) leaves only harsher
	// colour rungs; 64 user colours remove the 128 rung.
	rungs, drops = optimizeLadder(2, 64, facts, false)
	labels = labels[:0]
	for _, r := range rungs {
		labels = append(labels, fmt.Sprintf("%d/%d", drops[r.Label], r.Colors))
	}
	if strings.Join(labels, " ") != "2/64" {
		t.Errorf("ladder(base 2, 64 colours) = %v", labels)
	}
	rungs, _ = optimizeLadder(3, 0, facts, false)
	if len(rungs) != 6 {
		t.Errorf("ladder(base 3) = %d rungs", len(rungs))
	}
	// A source with a small palette gets no colour rungs at all.
	rungs, _ = optimizeLadder(0, 0, gifFacts{frames: 12, colors: 4}, false)
	if len(rungs) != 3 {
		t.Errorf("ladder(4-colour source) = %d rungs, want the 3 drop rungs", len(rungs))
	}
	rungs, _ = optimizeLadder(0, 0, gifFacts{frames: 12, colors: 100}, false)
	if len(rungs) != 6 {
		t.Errorf("ladder(100-colour source) = %d rungs, want 3 drops x (src, 64)", len(rungs))
	}
	// FitKeepFPS: no frame-drop rungs beyond what the user asked for.
	rungs, drops = optimizeLadder(0, 0, facts, true)
	labels = labels[:0]
	for _, r := range rungs {
		labels = append(labels, fmt.Sprintf("%d/%d", drops[r.Label], r.Colors))
	}
	if strings.Join(labels, " ") != "0/0 0/128 0/64" {
		t.Errorf("ladder(keep fps) = %v", labels)
	}
	// The user's own drop (Output.FPS) is their request, not a fit rung, and
	// survives FitKeepFPS.
	rungs, drops = optimizeLadder(2, 0, facts, true)
	for _, r := range rungs {
		if drops[r.Label] != 2 {
			t.Errorf("ladder(keep fps, base 2) rung %q drops %d", r.Label, drops[r.Label])
		}
	}
}

// TestRenderOptimizeValidation: the optimiser refuses non-GIF sources, ops
// and non-GIF outputs with client errors before any tool runs.
func TestRenderOptimizeValidation(t *testing.T) {
	st := newTestStore(t)
	tools := fakeTools
	tools.Gifsicle = "gifsicle-does-not-exist-ezlg-test"
	m := NewManager(st, tools, Options{Concurrency: 1})

	mov := putSource(t, st, true) // prores
	fin := runJobSimple(t, m, recipe.Recipe{Sources: []string{mov}, Output: recipe.Output{Format: "gif", Preset: "optimize"}})
	if fin.State != StateError || !strings.Contains(fin.Error, "GIF sources only") || !strings.Contains(fin.Error, ErrInvalidRecipe.Error()) {
		t.Errorf("prores source: %+v", fin)
	}

	// A real GIF blob with probe info.
	g := &gif.GIF{LoopCount: 0}
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 0, 0, 255}}
	for i := 0; i < 4; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
		fr.SetColorIndex(i, 0, 1)
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	b, err := st.PutBlob(bytes.NewReader(buf.Bytes()), "anim.gif")
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlobInfo(b.Hash, recipe.ProbeInfo{Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8, Width: 8, Height: 8, FPS: 10, Duration: 0.4, Frames: 4, Kind: recipe.KindAnimation})

	fin = runJobSimple(t, m, recipe.Recipe{Sources: []string{b.Hash}, Ops: []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":5}`)}}, Output: recipe.Output{Format: "gif", Preset: "optimize"}})
	if fin.State != StateError || !strings.Contains(fin.Error, "cannot apply edit ops") {
		t.Errorf("ops: %+v", fin)
	}
	fin = runJobSimple(t, m, recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "webp", Preset: "optimize"}})
	if fin.State != StateError || !strings.Contains(fin.Error, "outputs GIF") {
		t.Errorf("webp output: %+v", fin)
	}
	fin = runJobSimple(t, m, recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "gif", Preset: "optimize", FPS: 3}})
	if fin.State != StateError || !strings.Contains(fin.Error, "not reachable") {
		t.Errorf("bad fps: %+v", fin)
	}
	// Valid request reaches gifsicle (which does not exist here).
	fin = runJobSimple(t, m, recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "gif", Preset: "optimize", FPS: 5, Lossy: 20}})
	if fin.State != StateError || !strings.Contains(fin.Error, "gifsicle") || strings.Contains(fin.Error, ErrInvalidRecipe.Error()) {
		t.Errorf("valid optimise request must reach gifsicle: %+v", fin)
	}
	// Without gifsicle at all: a clear message.
	m2 := NewManager(st, fakeTools, Options{Concurrency: 1})
	fin = runJobSimple(t, m2, recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "gif", Preset: "optimize"}})
	if fin.State != StateError || !strings.Contains(fin.Error, "needs gifsicle") {
		t.Errorf("no gifsicle: %+v", fin)
	}
	// readGIFFacts on junk and on the real file.
	if _, err := readGIFFacts(filepath.Join(t.TempDir(), "missing.gif")); err == nil {
		t.Error("missing gif accepted")
	}
	junk := filepath.Join(t.TempDir(), "junk.gif")
	os.WriteFile(junk, []byte("GIF89a-not-really"), 0o644)
	if _, err := readGIFFacts(junk); err == nil || !strings.Contains(err.Error(), "cannot be decoded") {
		t.Errorf("junk gif: %v", err)
	}
	facts, err := readGIFFacts(b.Path)
	if err != nil || facts.frames != 4 || len(facts.delays) != 4 || facts.delays[0] != 10 || facts.colors != 2 {
		t.Errorf("readGIFFacts = %+v %v", facts, err)
	}
}

// runJobSimple submits and waits (no subscription).
func runJobSimple(t *testing.T, m *Manager, r recipe.Recipe) Job {
	t.Helper()
	j, err := m.Submit(r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return waitFinished(t, m, j.ID)
}

// TestFramesCapBeforeMaster: a frame export whose plan exceeds the cap is
// refused before ffmpeg runs.
func TestFramesCapBeforeMaster(t *testing.T) {
	st := newTestStore(t)
	b, _ := st.PutBlob(bytes.NewReader([]byte("long clip")), "long.mp4")
	st.SetBlobInfo(b.Hash, recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", PixFmt: "yuv420p", Bits: 8, Width: 64, Height: 48, FPS: 30, Duration: 120, Frames: 3600, Kind: recipe.KindVideo})
	m := NewManager(st, fakeTools, Options{Concurrency: 1})
	fin := runJobSimple(t, m, recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "frames"}})
	if fin.State != StateError || !strings.Contains(fin.Error, "too many frames") || strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("frames cap: %+v", fin)
	}
	// A static output of the same clip only needs one frame: admitted
	// (then fails on the fake ffmpeg), even under a tiny master cap.
	m = NewManager(st, fakeTools, Options{Concurrency: 1, MaxMasterBytes: 64 * 48 * 4})
	fin = runJobSimple(t, m, recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "png"}})
	if fin.State != StateError || !strings.Contains(fin.Error, "ffmpeg") {
		t.Errorf("static one-frame master: %+v", fin)
	}
	fin = runJobSimple(t, m, recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "gif"}})
	if fin.State != StateError || !strings.Contains(fin.Error, "frame master would need") {
		t.Errorf("animated output must still hit the cap: %+v", fin)
	}
}

// TestWriteStagingMultiple: several produced files land under their names,
// report.json mirrors the primary's report, and manifest.json lists them.
func TestWriteStagingMultiple(t *testing.T) {
	scratch := t.TempDir()
	p1 := fakeCandidateFile(t, scratch, "final.gif", 10)
	p2 := fakeCandidateFile(t, scratch, "alt1.gif", 20)
	rep := &discordlint.Report{Format: "gif", OK: true}
	res := &Result{RecipeHash: strings.Repeat("b", 64), Files: []File{{Name: "out.gif"}, {Name: "alt1.gif"}}}
	staging := filepath.Join(scratch, "result")
	items := []produced{{path: p1, name: "out.gif", report: rep}, {path: p2, name: "alt1.gif"}}
	if err := writeStaging(staging, items, res); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"out.gif", "alt1.gif", reportName, "manifest.json"} {
		if _, err := os.Stat(filepath.Join(staging, n)); err != nil {
			t.Errorf("%s missing: %v", n, err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(staging, "manifest.json"))
	got, err := decodeResult(raw)
	if err != nil || len(got.Files) != 2 {
		t.Errorf("manifest = %+v %v", got, err)
	}
	// A file already in staging is left alone.
	p3 := fakeCandidateFile(t, staging, "f00001.png", 5)
	if err := writeStaging(staging, []produced{{path: p3, name: "f00001.png"}}, res); err != nil {
		t.Fatal(err)
	}
}
