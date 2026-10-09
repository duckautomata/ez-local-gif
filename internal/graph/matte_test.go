package graph

// Phase 5b goldens: the matte op (docs/background-removal-proposal.md §5),
// CompileMatteInput, CompileDetectFor and the shared temporal prefix. The
// pixel-level checks against a real ffmpeg live in matte_ffmpeg_test.go.

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

func matte(p recipe.MatteParams) recipe.Op { return op(recipe.OpMatte, p) }

// matteIn is the ExtraInput compiler.matte registers for a matte op whose
// plan runs at fps (the text of the fps stage): Source 0, the image2 args,
// Matte with the resolved model, the op's size and the same fps text.
func matteIn(model string, size int, fps string) ExtraInput {
	return ExtraInput{
		Source: 0,
		Args:   []string{"-f", "image2", "-framerate", fps, "-start_number", "1"},
		Matte:  &MatteInput{Model: model, Size: size, FPS: fps},
	}
}

// matteBranch is the matte input's own chain: ffmpeg input k converted to
// gray and scaled to the current frame.
func matteBranch(k, w, h int) string {
	return "[" + strconv.Itoa(k) + ":v]format=gray,scale=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":flags=bicubic"
}

// matteOpaque is the merge mergeMatte emits for the n-th matte on OPAQUE
// frames reading input k: it joins the chain in place of a stage —
// "<chain so far>," + matteOpaque(…) + ",<rest>" — closing the chain into
// [mN], scaling the matte on its own chain and merging it as the alpha.
// The chain must already end in format=rgba (ensureRGBA emits it
// otherwise; the first golden spells that out).
func matteOpaque(n, k, w, h int) string {
	m := strconv.Itoa(n)
	return "format=rgba[m" + m + "];" +
		matteBranch(k, w, h) + "[m" + m + "a];" +
		"[m" + m + "][m" + m + "a]alphamerge"
}

// matteWrapped is the merge for the n-th matte on frames that already
// carry alpha: the keyKeepingAlpha shape with the matte branch in place of
// the keyed copy, multiplying the incoming alpha with the matte.
func matteWrapped(n, k, w, h int) string {
	m := strconv.Itoa(n)
	return "format=rgba,split[m" + m + "][m" + m + "m];" +
		"[m" + m + "m]alphaextract[m" + m + "a0];" +
		matteBranch(k, w, h) + "[m" + m + "a1];" +
		"[m" + m + "a0][m" + m + "a1]blend=all_mode=multiply[m" + m + "a];" +
		"[m" + m + "][m" + m + "a]alphamerge"
}

var (
	// A 12-bit RGB-decoded video with alpha (the gbrap12le alpha head, not
	// the native yuva one): the hoisted unpremultiply runs at 12 bits and
	// nothing converts to rgba until the matte merge asks for it.
	rgb12 = with(prores, func(p *recipe.ProbeInfo) { p.Codec, p.PixFmt, p.Bits = "exr", "gbrap12le", 12 })
	// An animated AVIF with alpha as the MAIN source: colour track v:2,
	// alpha v:3 (the one-frame primary item comes first).
	avifAnim = with(ovAVIFAnim, func(p *recipe.ProbeInfo) { p.Width, p.Height = 128, 96 })
)

// --- goldens ----------------------------------------------------------------

func TestCompileMatte(t *testing.T) {
	const isnet = recipe.MatteModelISNetAnime
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want Plan // OutLabel implied; Speed 0 means 1; SourceFPS 0 means the main source's
	}{
		{
			// The whole text once: the chain so far is closed into [m1]
			// behind an explicit format=rgba, the matte input (ffmpeg input
			// 1, read at the plan's rate from frame 1) is converted to gray
			// and scaled to the source frame, and alphamerge makes it the
			// alpha. Model "" resolves to the default; Size 0 is the server's.
			name: "opaque video: the matte becomes the alpha",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[m1];[1:v]format=gray,scale=1280:720:flags=bicubic[m1a];[m1][m1a]alphamerge,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			// The whole wrapped text once: the chain is closed into a split,
			// the incoming alpha is extracted, the matte scaled, the two
			// multiplied and the product merged back — the matte is
			// intersected with the GIF's transparency, never substituted.
			name: "transparent gif: the matte is multiplied into the source alpha",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{matte(recipe.MatteParams{Model: isnet})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down,format=rgba,split[m1][m1m];[m1m]alphaextract[m1a0];[1:v]format=gray,scale=480:270:flags=bicubic[m1a1];[m1a0][m1a1]blend=all_mode=multiply[m1a];[m1][m1a]alphamerge,format=rgba[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "20")},
			},
		},
		{
			// The native yuva head already ends the chain in format=rgba, so
			// the merge emits none of its own (ensureRGBA's rule).
			name: "prores: the yuva head's format=rgba is reused, the matte wrapped",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{matte(recipe.MatteParams{Model: recipe.MatteModelBiRefNetLite, Size: 1024})}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba,fps=30:round=down," + matteWrapped(1, 1, 1920, 1080) + ",format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
				ExtraInputs: []ExtraInput{matteIn(recipe.MatteModelBiRefNetLite, 1024, "30")},
			},
		},
		{
			// A > 8-bit RGB alpha source reaches alphamerge as rgba by the
			// merge's explicit conversion (after the 12-bit unpremultiply),
			// not by one the negotiator picks; alphaextract then yields
			// 8-bit gray like the matte branch.
			name: "gbrap12le source: explicit format=rgba in front of the split",
			srcs: []recipe.ProbeInfo{rgb12}, ops: []recipe.Op{unpremultiply(), matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=gbrap12le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,fps=30:round=down," + matteWrapped(1, 1, 1920, 1080) + ",format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "30")},
			},
		},
		{
			// Stack order: the matte's alpha is there when the key runs, so
			// the key goes through keyKeepingAlpha (its text unchanged).
			name: "matte then colorkey: the key is wrapped behind the merge",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{}), colorkey(recipe.ColorKeyParams{Color: "ffffff"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + keyWrapped(1, ckWhite) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			name: "colorkey then matte: the bare key's alpha is multiplied with the matte",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"}), matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckWhite + "," + matteWrapped(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			name: "chromakey, matte, colorkey: keys and matte in stack order, one input",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{chromakey(recipe.ChromaKeyParams{DespillOff: true}), matte(recipe.MatteParams{}), colorkey(recipe.ColorKeyParams{Color: "ffffff"})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + ckNoSpill + "," + matteWrapped(1, 1, 1280, 720) + "," + keyWrapped(1, ckWhite) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			// morph and feather act on the matte's alpha (they are skipped on
			// opaque frames, so their presence proves the merge set hasAlpha).
			name: "matte, morph, feather: cleanup and softening follow the merge",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{}), morph(recipe.MorphParams{Close: true}), feather(0)}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + morphClose + "," + featherDef + "[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			name: "morph and feather in front of the matte are skipped (no alpha yet), the matte still merges",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{morph(recipe.MorphParams{Close: true}), feather(2), matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			// Input indices: the matte is registered in the keying group, so
			// it is input 1 and the overlay registered later is input 2.
			name: "matte before an overlay: the matte is input 1, the overlay input 2",
			srcs: []recipe.ProbeInfo{h264, ovPNG}, ops: []recipe.Op{overlay(recipe.OverlayParams{Source: 1}), matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[b1];[2:v]format=rgba[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97"), {Source: 1, Args: []string{"-loop", "1"}}},
			},
		},
		{
			name: "matte, geometry, text and an overlay continue from the merged chain",
			srcs: []recipe.ProbeInfo{gifSrc, ovPNG},
			ops:  []recipe.Op{matte(recipe.MatteParams{}), crop(0, 0, 240, 270), text(recipe.TextParams{Text: "Hi"}), overlay(recipe.OverlayParams{Source: 1})},
			out:  webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down," + matteWrapped(1, 1, 480, 270) + ",crop=240:270:0:0:exact=1,format=rgba," + text1 + "[b1];[2:v]format=rgba[ov1];[b1][ov1]" + ovComposit + ",format=rgba[out]",
				Width:  240, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "20"), {Source: 1, Args: []string{"-loop", "1"}}},
				TextFiles:   []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "Hi"}},
			},
		},
		{
			// Dedupe: the same (model, size) is one input; the second merge
			// reads [1:v] again and is wrapped (the first merge's alpha).
			name: "two matte ops of one model and size share the input",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{}), matte(recipe.MatteParams{Model: isnet})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + matteWrapped(2, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			name: "two matte ops of different models or sizes are two inputs, in order of first use",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{Size: 512}), matte(recipe.MatteParams{Model: recipe.MatteModelBiRefNetLite}), matte(recipe.MatteParams{Size: 512})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + "," + matteWrapped(2, 2, 1280, 720) + "," + matteWrapped(3, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 512, "29.97"), matteIn(recipe.MatteModelBiRefNetLite, 0, "29.97")},
			},
		},
		{
			// The matte's -framerate is the plan's rate, not the source's:
			// Output.FPS 60 on a GIF snaps to 50 and the fps stage, the
			// input args and MatteInput.FPS all say "50".
			name: "Output-aware rate: a 60 fps GIF request snaps to 50 everywhere",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: recipe.Output{Format: "gif", FPS: 60},
			want: Plan{
				Filter: "[0:v]fps=50:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 50, HasAlpha: true, Duration: 10, Frames: 500,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "50")},
			},
		},
		{
			name: "trim, speed and fps op: the merge follows the temporal stages, the input rate is the fps op's",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{trim(1, 3), matte(recipe.MatteParams{}), speed(2), fps(12.5)}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1", "-to", "3"},
				Filter:    "[0:v]setpts=PTS/2,fps=12.5:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:     1280, Height: 720, FPS: 12.5, HasAlpha: true, Duration: 1, Frames: 12, Speed: 2,
				TrimStart: 1, TrimEnd: 3,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "12.5")},
			},
		},
		{
			name: "image sequence: the guarding head, the image2 rate on both inputs, the matte wrapped (transparent frames)",
			srcs: []recipe.ProbeInfo{pngSeq}, ops: []recipe.Op{delay(50), matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("20"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=20:round=down," + matteWrapped(1, 1, 200, 100) + ",format=rgba[out]",
				Width:  200, Height: 100, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60, SourceFPS: 20,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "20")},
			},
		},
		{
			name: "opaque mixed-size sequence: the transparent padding makes the merge a wrapped one at the normalised canvas",
			srcs: []recipe.ProbeInfo{with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true }), func(p *recipe.ProbeInfo) { p.HasAlpha = false })},
			ops:  []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "fps=10:round=down," + matteWrapped(1, 1, 200, 100) + ",format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "10")},
			},
		},
		{
			name: "animated avif main source: the alpha-stream head, the matte wrapped",
			srcs: []recipe.ProbeInfo{avifAnim}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v:2]format=rgba[c];[0:v:3]format=gray[a];[c][a]alphamerge,fps=10:round=down," + matteWrapped(1, 1, 128, 96) + ",format=rgba[out]",
				Width:  128, Height: 96, FPS: 10, HasAlpha: true, Duration: 2, Frames: 20,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "10")},
			},
		},
		{
			name: "webp_anim with a filter trim: the trim stage precedes the merge, no seek args",
			srcs: []recipe.ProbeInfo{webpAnim}, ops: []recipe.Op{trim(0.5, 1.5), matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]trim=start=0.5:end=1.5,setpts=PTS-STARTPTS,fps=10:round=down," + matteWrapped(1, 1, 64, 48) + ",format=rgba[out]",
				Width:  64, Height: 48, FPS: 10, HasAlpha: true, Duration: 1, Frames: 10,
				TrimStart: 0.5, TrimEnd: 1.5, FilterTrim: true,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "10")},
			},
		},
		{
			// A still has no fps stage: the chain is a bare [0:v], and the
			// merge's format=rgba is the stage that makes it a filterchain.
			// The input still declares the nominal rate (Plan.FPS).
			name: "opaque still: the merge off a bare [0:v]",
			srcs: []recipe.ProbeInfo{with(still, func(p *recipe.ProbeInfo) { p.HasAlpha, p.PixFmt = false, "rgb24" })}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]" + matteOpaque(1, 1, 800, 600) + ",format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "10")},
			},
		},
		{
			name: "transparent still: wrapped off a bare [0:v]",
			srcs: []recipe.ProbeInfo{still}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]" + matteWrapped(1, 1, 800, 600) + ",format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, HasAlpha: true, Frames: 1,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "10")},
			},
		},
		{
			// The merge precedes the reverse/bounce group: reversed and
			// mirrored frames carry their own mattes by construction.
			name: "matte, reverse and bounce: the merge sits in front of the reverse group",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{}), reverse(), bounce()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba,reverse," + bounceChain(1) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 20, Frames: 598,
				Reversed: true, Bounced: true,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
		{
			// Resolved is jobs' business (the recipe hash), never the text.
			name: "Resolved is ignored by the compiler",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{Resolved: &recipe.MatteResolved{Weights: "abc", Proc: "p1", Size: 1024, Precision: "fp16"}})}, out: webp(),
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
		})
	}
}

// checkMatteInputs checks the matte inputs of a plan against the contract
// every consumer relies on: Source 0, Matte set, Frames 0 from the
// compiler, Args exactly the image2 triple with the plan's fps text, which
// is also MatteInput.FPS, and a read as "[k:v]" (k = position + 1) that
// converts to gray and scales to the plan's frame; no overlay carries Matte.
func checkMatteInputs(t *testing.T, p *Plan) {
	t.Helper()
	fps := fnum(p.FPS)
	for i, in := range p.ExtraInputs {
		if in.Matte == nil {
			if in.Source == 0 {
				t.Errorf("ExtraInputs[%d]: Source 0 without Matte: %+v", i, in)
			}
			continue
		}
		if in.Source != 0 || in.Path != "" || in.Animated || in.Loop || in.Duration != 0 {
			t.Errorf("ExtraInputs[%d]: a matte input must be Source 0 with the overlay fields zero: %+v", i, in)
		}
		if want := []string{"-f", "image2", "-framerate", fps, "-start_number", "1"}; !reflect.DeepEqual(in.Args, want) {
			t.Errorf("ExtraInputs[%d].Args %q, want %q", i, in.Args, want)
		}
		if in.Matte.FPS != fps || in.Matte.Frames != 0 || in.Matte.Model == "" || in.Matte.Size < 0 {
			t.Errorf("ExtraInputs[%d].Matte %+v, want FPS %q, Frames 0, a model, size >= 0", i, *in.Matte, fps)
		}
		// The scale's size is the pre-geometry frame, pinned by the goldens.
		if read := "[" + strconv.Itoa(i+1) + ":v]format=gray,scale="; !strings.Contains(p.Filter, read) {
			t.Errorf("ExtraInputs[%d] is not read as %s… in %s", i, read, p.Filter)
		}
	}
	if strings.Contains(p.Filter, "alphamerge=") {
		t.Errorf("alphamerge must carry no options (no shortest/eof_action): %s", p.Filter)
	}
}

func TestCompileMatteErrors(t *testing.T) {
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		want string // substring of the error
	}{
		{"negative size", []recipe.ProbeInfo{h264}, []recipe.Op{matte(recipe.MatteParams{Size: -1})}, "op 0 (matte): size must be >= 0 (got -1)"},
		{"model with a path separator", []recipe.ProbeInfo{h264}, []recipe.Op{matte(recipe.MatteParams{Model: "../isnet"})}, `op 0 (matte): model "../isnet" is not a model id`},
		{"model with a space", []recipe.ProbeInfo{gifSrc}, []recipe.Op{matte(recipe.MatteParams{Model: "isnet anime"})}, `op 0 (matte): model "isnet anime" is not a model id`},
		{"malformed params keep the index", []recipe.ProbeInfo{h264}, []recipe.Op{crop(0, 0, 10, 10), rawOp(recipe.OpMatte, `{"size":"big"}`)}, "op 1 (matte): invalid params"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for name, compile := range map[string]func() (*Plan, error){
				"Compile":       func() (*Plan, error) { return CompileWithSources(tc.srcs, tc.ops, webp()) },
				"CompileDetect": func() (*Plan, error) { return CompileDetect(tc.srcs, tc.ops) },
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
			if _, err := CompileMatteInput(tc.srcs, tc.ops, webp()); err != nil {
				t.Errorf("CompileMatteInput must ignore the matte op: %v", err)
			}
		})
	}
}

// --- detection plans ----------------------------------------------------------

func TestCompileDetectForMatte(t *testing.T) {
	const isnet = recipe.MatteModelISNetAnime
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want Plan
	}{
		{
			// The detection reads the box off the matte's alpha; the input
			// is registered exactly like the render's (Path "" for jobs to
			// fill before CropDetectPlanArgs) at the render's rate.
			name: "matte + autocrop at the preset's 25 fps: the detection plan carries the matte input at 25",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{}), autocrop(recipe.AutoCropParams{Threshold: 8})}, out: recipe.Output{Format: "gif", FPS: 25, Width: 128, Height: 128, Fit: "contain"},
			want: Plan{
				Filter: "[0:v]fps=25:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 25, HasAlpha: true, Duration: 10, Frames: 250,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "25")},
			},
		},
		{
			name: "matte behind the autocrop, with a morph: hoisted into the detection plan",
			srcs: []recipe.ProbeInfo{gifSrc}, ops: []recipe.Op{autocrop(recipe.AutoCropParams{}), resize(128, 0, ""), matte(recipe.MatteParams{Model: recipe.MatteModelBiRefNetLite}), morph(recipe.MorphParams{Close: true})}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=20:round=down," + matteWrapped(1, 1, 480, 270) + "," + morphClose + "[out]",
				Width:  480, Height: 270, FPS: 20, HasAlpha: true, Duration: 3, Frames: 60,
				ExtraInputs: []ExtraInput{matteIn(recipe.MatteModelBiRefNetLite, 0, "20")},
			},
		},
		{
			// A GIF output caps the rate at 50: the render, the detection
			// and the matte input all run at 50 for a 60 fps request.
			name: "Output.FPS 60 on a gif: snapped to 50 like the render",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: recipe.Output{Format: "gif", FPS: 60},
			want: Plan{
				Filter: "[0:v]fps=50:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 50, HasAlpha: true, Duration: 10, Frames: 500,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "50")},
			},
		},
		{
			name: "the fps op wins over Output.FPS, as in the render",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{fps(15), chromakey(recipe.ChromaKeyParams{DespillOff: true})}, out: recipe.Output{Format: "webp", FPS: 25},
			want: Plan{
				Filter: "[0:v]fps=15:round=down," + ckNoSpill + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 15, HasAlpha: true, Duration: 10, Frames: 150,
			},
		},
		{
			// Only Format and FPS of the Output reach a detection plan: the
			// size/fit are the geometry's, and an invalid FrameFormat (which
			// CompileWithSources would refuse) is not validated here.
			name: "the rest of the Output is ignored",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: recipe.Output{Format: "webp", Width: 9000, Height: 9000, Fit: "cover", FrameFormat: "bmp", FitBytes: -1},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + matteOpaque(1, 1, 1280, 720) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, HasAlpha: true, Duration: 10, Frames: 299,
				ExtraInputs: []ExtraInput{matteIn(isnet, 0, "29.97")},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileDetectFor(tc.srcs, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("CompileDetectFor: %v", err)
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
			if len(got.TextFiles) != 0 || got.Reversed || got.Bounced || got.Bounces != 0 {
				t.Errorf("detection plan must have no text files, reverse or bounce: %+v", got)
			}
			for _, in := range got.ExtraInputs {
				if in.Matte == nil {
					t.Errorf("detection plan must hold matte inputs only: %+v", in)
				}
			}
		})
	}
	// CompileDetect is CompileDetectFor with an empty Output: no Output.FPS,
	// so the source rate — the pre-5b behaviour, every existing golden.
	a, err := CompileDetect([]recipe.ProbeInfo{h264}, []recipe.Op{matte(recipe.MatteParams{})})
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompileDetectFor([]recipe.ProbeInfo{h264}, []recipe.Op{matte(recipe.MatteParams{})}, recipe.Output{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || a.FPS != 29.97 || a.ExtraInputs[0].Matte.FPS != "29.97" {
		t.Errorf("CompileDetect must equal CompileDetectFor with an empty Output at the source rate:\n%+v\n%+v", a, b)
	}
}

// --- matte input plans --------------------------------------------------------

func TestCompileMatteInput(t *testing.T) {
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want Plan
	}{
		{
			// Everything but the temporal kinds is ignored, params unread:
			// the matte op itself, keys, morph, feather, geometry, reverse,
			// bounce, text, overlays, an autocrop, a malformed crop, an
			// unknown kind. The trim's seek args are the render's.
			name: "video: the temporal prefix only, the render's seek args",
			srcs: []recipe.ProbeInfo{h264, ovPNG},
			ops: []recipe.Op{
				trim(1, 3), matte(recipe.MatteParams{Size: -5}), chromakey(recipe.ChromaKeyParams{}), colorkey(recipe.ColorKeyParams{}),
				morph(recipe.MorphParams{}), feather(999), crop(0, 0, 640, 360), resize(320, 0, ""), reverse(), bounce(),
				text(recipe.TextParams{Text: "x"}), overlay(recipe.OverlayParams{Source: 1}), autocrop(recipe.AutoCropParams{}),
				rawOp(recipe.OpCrop, `{bad`), {Kind: "sparkle"},
			},
			out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1", "-to", "3"},
				Filter:    "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 2, Frames: 59,
				TrimStart: 1, TrimEnd: 3,
			},
		},
		{
			name: "Output-aware rate: Output.FPS 60 on a gif snaps to 50 (the render's grid)",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: recipe.Output{Format: "gif", FPS: 60},
			want: Plan{
				Filter: "[0:v]fps=50:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 50, Duration: 10, Frames: 500,
			},
		},
		{
			name: "the fps op wins over Output.FPS; speed precedes it",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{speed(2), fps(15)}, out: recipe.Output{Format: "webp", FPS: 25},
			want: Plan{
				Filter: "[0:v]setpts=PTS/2,fps=15:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 15, Duration: 5, Frames: 75, Speed: 2,
			},
		},
		{
			// The source alpha is kept (the model sees the stored colour of
			// transparent pixels; the intersection masks them again) and the
			// native yuva head is the render's.
			name: "prores: the native yuva head with the hoisted unpremultiply",
			srcs: []recipe.ProbeInfo{prores}, ops: []recipe.Op{matte(recipe.MatteParams{}), unpremultiply()}, out: webp(),
			want: Plan{
				Filter: "[0:v]setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba,fps=30:round=down,format=rgba[out]",
				Width:  1920, Height: 1080, FPS: 30, HasAlpha: true, Duration: 4, Frames: 120,
			},
		},
		{
			name: "image sequence with a delay op: the image2 args, the guarding head",
			srcs: []recipe.ProbeInfo{pngSeq}, ops: []recipe.Op{delay(50), matte(recipe.MatteParams{}), trim(1, 2)}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("20", "-ss", "1", "-to", "2"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=20:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 20, HasAlpha: true, Duration: 1, Frames: 20, SourceFPS: 20,
				TrimStart: 1, TrimEnd: 2,
			},
		},
		{
			name: "mixed-size sequence: the normalised canvas is the frame the model sees",
			srcs: []recipe.ProbeInfo{withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Mixed = true })}, ops: nil, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: mixedHead200 + "fps=10:round=down,format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 6, Frames: 60, SourceFPS: 10,
			},
		},
		{
			name: "avif alpha stream: the merge head",
			srcs: []recipe.ProbeInfo{avifAnim}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v:2]format=rgba[c];[0:v:3]format=gray[a];[c][a]alphamerge,fps=10:round=down,format=rgba[out]",
				Width:  128, Height: 96, FPS: 10, HasAlpha: true, Duration: 2, Frames: 20,
			},
		},
		{
			name: "webp_anim: the filter trim, no seek args",
			srcs: []recipe.ProbeInfo{webpAnim}, ops: []recipe.Op{trim(0.5, 1.5), matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]trim=start=0.5:end=1.5,setpts=PTS-STARTPTS,fps=10:round=down,format=rgba[out]",
				Width:  64, Height: 48, FPS: 10, HasAlpha: true, Duration: 1, Frames: 10,
				TrimStart: 0.5, TrimEnd: 1.5, FilterTrim: true,
			},
		},
		{
			// A bounce doubles the RENDER's frames; the matte plan yields the
			// pre-bounce frames once (jobs compares manifest.frames x
			// 2^Plan.Bounces with the master).
			name: "bounce: ignored, the plan yields the pre-bounce frames",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{bounce(), matte(recipe.MatteParams{}), bounce()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 10, Frames: 299,
			},
		},
		{
			name: "vp9 alpha: the decoder forcing stays in the input args",
			srcs: []recipe.ProbeInfo{vp9}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				InputArgs: []string{"-c:v", "libvpx-vp9"},
				Filter:    "[0:v]format=rgba,fps=30:round=down,format=rgba[out]",
				Width:     640, Height: 360, FPS: 30, HasAlpha: true, Duration: 2, Frames: 60,
			},
		},
		{
			name: "opaque still: one frame, no fps stage, the nominal rate",
			srcs: []recipe.ProbeInfo{with(still, func(p *recipe.ProbeInfo) { p.HasAlpha, p.PixFmt = false, "rgb24" })}, ops: []recipe.Op{matte(recipe.MatteParams{})}, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba[out]",
				Width:  800, Height: 600, FPS: 10, Frames: 1,
			},
		},
		{
			// No frame-size limit: the sidecar sees the stretched square and
			// the detection-like plan never materialises a master.
			name: "oversized source: no frame-size limit",
			srcs: []recipe.ProbeInfo{with(still, func(p *recipe.ProbeInfo) { p.Width, p.Height = 9000, 9000 })}, ops: nil, out: webp(),
			want: Plan{
				Filter: "[0:v]format=rgba[out]",
				Width:  9000, Height: 9000, FPS: 10, HasAlpha: true, Frames: 1,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompileMatteInput(tc.srcs, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("CompileMatteInput: %v", err)
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
			if len(got.ExtraInputs) != 0 || len(got.TextFiles) != 0 || got.Reversed || got.Bounced || got.Bounces != 0 {
				t.Errorf("matte input plan must have no extra inputs, text files, reverse or bounce: %+v", got)
			}
			if !strings.HasSuffix(got.Filter, "format=rgba[out]") {
				t.Errorf("matte input plan must end in format=rgba: %s", got.Filter)
			}
			if got.Width != tc.srcs[0].Width || got.Height != tc.srcs[0].Height {
				t.Errorf("matte input plan is %dx%d, want the source frame %dx%d", got.Width, got.Height, tc.srcs[0].Width, tc.srcs[0].Height)
			}
			for _, stage := range []string{"chromakey", "colorkey", "alphamerge,", "alphaextract", "gblur", "dilation", "crop=", "transpose", "hflip", "vflip", "reverse", "concat", "drawtext", "overlay", "split"} {
				if tc.srcs[0].AlphaStream > 0 && stage == "alphamerge," {
					continue // the alpha-stream head's own merge
				}
				if strings.Contains(got.Filter, stage) {
					t.Errorf("matte input plan must not contain %q: %s", stage, got.Filter)
				}
			}
		})
	}
}

func TestCompileMatteInputErrors(t *testing.T) {
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want string
	}{
		{"no sources", nil, nil, webp(), "no sources"},
		{"source without a frame size", []recipe.ProbeInfo{{Format: "png_pipe", IsStill: true}}, nil, webp(), "source has no usable frame size (0x0)"},
		{"trim end before start", []recipe.ProbeInfo{h264}, []recipe.Op{trim(3, 1)}, webp(), "op 0 (trim): end (1 s) must be after start (3 s)"},
		{"speed out of range", []recipe.ProbeInfo{h264}, []recipe.Op{matte(recipe.MatteParams{}), speed(0)}, webp(), "op 1 (speed): factor must be > 0"},
		{"fps op zero", []recipe.ProbeInfo{h264}, []recipe.Op{fps(0)}, webp(), "op 0 (fps): fps must be > 0"},
		{"negative output fps", []recipe.ProbeInfo{h264}, nil, recipe.Output{Format: "gif", FPS: -1}, "output fps must be >= 0"},
		{"sequence delay out of range", []recipe.ProbeInfo{pngSeq}, []recipe.Op{delay(0)}, webp(), "op 0 (delay): ms must be between 1 and 60000 (got 0)"},
		{"malformed temporal params", []recipe.ProbeInfo{h264}, []recipe.Op{crop(0, 0, 10, 10), rawOp(recipe.OpTrim, `{"start":"soon"}`)}, webp(), "op 1 (trim): invalid params"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := CompileMatteInput(tc.srcs, tc.ops, tc.out)
			if err == nil {
				t.Fatalf("expected error containing %q, got plan %+v", tc.want, p)
			}
			if !strings.HasPrefix(err.Error(), "graph: ") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// --- the shared temporal prefix ------------------------------------------------

// stages splits a filter into its stage list: the "[out]" label is
// stripped and the text split on ",", so the first element carries the
// input label (or the alpha-stream head's own chains) and a chain boundary
// shows up as a label suffix on a stage ("format=rgba[m1];[1:v]format=gray").
func stages(filter string) []string {
	return strings.Split(strings.TrimSuffix(filter, "[out]"), ",")
}

// leading returns the first n stages of a plan with the merge's chain
// boundary cut off the last one it may land on ("format=rgba[m1];[1:v]
// format=gray" → "format=rgba"): where the matte plan ends in [out] the
// render closes the same stage into the merge's label.
func leading(plan *Plan, n int) []string {
	got := stages(plan.Filter)
	if len(got) < n {
		return got
	}
	got = append([]string{}, got[:n]...)
	for i, s := range got {
		if j := strings.Index(s, "[m"); j >= 0 {
			got[i] = s[:j]
		}
	}
	return got
}

// TestMattePrefixProperty: the matte input plan's stages minus its terminal
// format=rgba are the render plan's leading stages (and the detection
// plan's), and the three plans share InputArgs, InputPattern, FPS, the
// input's -framerate / MatteInput.FPS and the source frame — the "same
// frames" guarantee the matte memo rests on, for six stacks: video,
// sequence, AVIF alpha stream, yuva ProRes, FilterTrim WebP, bounce. The
// assertion is on stage lists, not filter bytes: the render's chain
// continues into the merge's split where the matte plan ends in [out].
func TestMattePrefixProperty(t *testing.T) {
	m := matte(recipe.MatteParams{})
	stacks := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op // without the matte op; m is inserted at pos
		pos  int
		out  recipe.Output
	}{
		{"video: trim, speed, fps op, chromakey in front of the matte, geometry, overlay", []recipe.ProbeInfo{h264, ovPNG},
			[]recipe.Op{trim(1, 3), speed(2), chromakey(recipe.ChromaKeyParams{}), fps(12), crop(0, 0, 640, 360), resize(128, 0, ""), overlay(recipe.OverlayParams{Source: 1})}, 3, recipe.Output{Format: "gif", FPS: 25}},
		{"sequence: delay, trim, unpremultiply, morph, feather", []recipe.ProbeInfo{pngSeq},
			[]recipe.Op{delay(40), trim(0.5, 2), unpremultiply(), morph(recipe.MorphParams{Close: true}), feather(2)}, 3, recipe.Output{Format: "webp", FPS: 15}},
		{"avif alpha stream: colorkey after the matte, Output.FPS", []recipe.ProbeInfo{avifAnim},
			[]recipe.Op{colorkey(recipe.ColorKeyParams{Color: "ffffff"}), resolved(0, 0, 100, 80)}, 0, recipe.Output{Format: "gif", FPS: 60}},
		{"yuva prores: unpremultiply, speed, Output.FPS, text", []recipe.ProbeInfo{prores},
			[]recipe.Op{unpremultiply(), speed(0.5), text(recipe.TextParams{Text: "x"})}, 2, recipe.Output{Format: "webp", FPS: 25, Width: 128, Height: 128, Fit: "contain"}},
		{"webp_anim filter trim: reverse behind the matte", []recipe.ProbeInfo{webpAnim},
			[]recipe.Op{trim(0.5, 2.5), reverse()}, 1, recipe.Output{Format: "apng"}},
		{"bounce: two bounces and a reverse, the fps op", []recipe.ProbeInfo{h264},
			[]recipe.Op{bounce(), fps(10), reverse(), bounce()}, 1, recipe.Output{Format: "webp"}},
		{"opaque still: no fps stage at all", []recipe.ProbeInfo{with(still, func(p *recipe.ProbeInfo) { p.HasAlpha = false })},
			[]recipe.Op{canvas(900, 700, "")}, 0, recipe.Output{Format: "png"}},
	}
	for _, s := range stacks {
		t.Run(s.name, func(t *testing.T) {
			ops := append(append(append([]recipe.Op{}, s.ops[:s.pos]...), m), s.ops[s.pos:]...)
			render, err := CompileWithSources(s.srcs, ops, s.out)
			if err != nil {
				t.Fatalf("CompileWithSources: %v", err)
			}
			detect, err := CompileDetectFor(s.srcs, ops, s.out)
			if err != nil {
				t.Fatalf("CompileDetectFor: %v", err)
			}
			for _, pass := range []struct {
				name string
				ops  []recipe.Op
			}{{"whole stack", ops}, {"temporal ops only", temporalOnly(ops)}} {
				in, err := CompileMatteInput(s.srcs, pass.ops, s.out)
				if err != nil {
					t.Fatalf("CompileMatteInput(%s): %v", pass.name, err)
				}
				prefix := stages(in.Filter)
				if n := len(prefix); n > 1 && prefix[n-1] == "format=rgba" {
					prefix = prefix[:n-1]
				}
				for _, p := range []struct {
					name string
					plan *Plan
				}{{"render", render}, {"detection", detect}} {
					if got := leading(p.plan, len(prefix)); !reflect.DeepEqual(got, prefix) {
						t.Errorf("%s (%s): the matte plan's stages are not a prefix\nmatte:  %q\nplan:   %q\nfilter: %s", p.name, pass.name, prefix, got, p.plan.Filter)
					}
					if !reflect.DeepEqual(p.plan.InputArgs, in.InputArgs) || p.plan.InputPattern != in.InputPattern {
						t.Errorf("%s (%s): InputArgs %q / %q differ from the matte plan's %q / %q", p.name, pass.name, p.plan.InputArgs, p.plan.InputPattern, in.InputArgs, in.InputPattern)
					}
					if p.plan.FPS != in.FPS || p.plan.TrimStart != in.TrimStart || p.plan.TrimEnd != in.TrimEnd || p.plan.Speed != in.Speed || p.plan.FilterTrim != in.FilterTrim || p.plan.SeekUnsafe != in.SeekUnsafe {
						t.Errorf("%s (%s): temporal facts differ from the matte plan's:\n%+v\n%+v", p.name, pass.name, p.plan, in)
					}
					var mattes int
					for _, x := range p.plan.ExtraInputs {
						if x.Matte == nil {
							continue
						}
						mattes++
						if x.Matte.FPS != fnum(in.FPS) || x.Args[3] != fnum(in.FPS) {
							t.Errorf("%s (%s): the matte input runs at %q / %q, the matte plan at %q", p.name, pass.name, x.Matte.FPS, x.Args[3], fnum(in.FPS))
						}
					}
					if mattes != 1 {
						t.Errorf("%s (%s): %d matte inputs, want 1", p.name, pass.name, mattes)
					}
				}
				if in.Width != s.srcs[0].Width || in.Height != s.srcs[0].Height || detect.Width != in.Width || detect.Height != in.Height {
					t.Errorf("frame sizes: matte %dx%d, detection %dx%d, source %dx%d", in.Width, in.Height, detect.Width, detect.Height, s.srcs[0].Width, s.srcs[0].Height)
				}
				// The merge scales the matte to the frame the matte plan
				// yields (the source frame, before any geometry).
				if scale := "scale=" + strconv.Itoa(in.Width) + ":" + strconv.Itoa(in.Height) + ":flags=bicubic[m1"; !strings.Contains(render.Filter, scale) {
					t.Errorf("render does not scale the matte to the matte plan's frame (%s): %s", scale, render.Filter)
				}
				// The render's frame count is the matte plan's times 2 per
				// bounce (what jobs' count check divides by).
				if want := in.Frames << render.Bounces; render.Frames != want || in.Frames == 0 {
					t.Errorf("render plans %d frames, matte plan %d x 2^%d = %d", render.Frames, in.Frames, render.Bounces, want)
				}
			}
		})
	}
}

// temporalOnly is jobs' view of the stack when it keys the memo: the
// temporal kinds in stack order, nothing else.
func temporalOnly(ops []recipe.Op) []recipe.Op {
	var out []recipe.Op
	for _, o := range ops {
		if temporalOps[o.Kind] {
			out = append(out, o)
		}
	}
	return out
}

// TestDetectOpsHoistMatte: a matte op anywhere in the stack reaches the
// detection plan (jobs' autocropDetectionOps collects detectOps kinds from
// the whole stack), like morph and feather.
func TestDetectOpsHoistMatte(t *testing.T) {
	for _, kind := range []string{recipe.OpMatte, recipe.OpMorph, recipe.OpFeather, recipe.OpChromaKey, recipe.OpColorKey, recipe.OpDelay, recipe.OpUnpremultiply, recipe.OpTrim, recipe.OpSpeed, recipe.OpFPS} {
		if !detectOps[kind] {
			t.Errorf("detectOps lacks %s", kind)
		}
	}
	for _, kind := range []string{recipe.OpDelay, recipe.OpUnpremultiply, recipe.OpTrim, recipe.OpSpeed, recipe.OpFPS} {
		if !temporalOps[kind] {
			t.Errorf("temporalOps lacks %s", kind)
		}
	}
	for kind := range temporalOps {
		if !detectOps[kind] {
			t.Errorf("temporalOps has %s but detectOps does not", kind)
		}
	}
	for _, kind := range []string{recipe.OpMatte, recipe.OpMorph, recipe.OpFeather, recipe.OpChromaKey, recipe.OpColorKey, recipe.OpCrop, recipe.OpResize, recipe.OpReverse, recipe.OpBounce, recipe.OpText, recipe.OpOverlay, recipe.OpAutoCrop} {
		if temporalOps[kind] {
			t.Errorf("temporalOps must not contain %s", kind)
		}
	}
	p := mustDetect(t, h264, []recipe.Op{autocrop(recipe.AutoCropParams{}), resize(64, 0, ""), matte(recipe.MatteParams{})})
	if len(p.ExtraInputs) != 1 || p.ExtraInputs[0].Matte == nil || !strings.Contains(p.Filter, "[1:v]format=gray") || !p.HasAlpha {
		t.Errorf("matte behind the autocrop must be hoisted into the detection plan: %+v", p)
	}
}

// TestPlanBounces: Plan.Bounces counts the bounce ops and Bounced mirrors
// it, on every entry point.
func TestPlanBounces(t *testing.T) {
	for n := 0; n <= 2; n++ {
		ops := append(bounceN(n), matte(recipe.MatteParams{}))
		p := mustCompile(t, h264, ops, webp())
		if p.Bounces != n || p.Bounced != (n > 0) {
			t.Errorf("%d bounces: Bounces %d Bounced %v", n, p.Bounces, p.Bounced)
		}
		if want := 299 << n; p.Frames != want {
			t.Errorf("%d bounces: %d frames, want %d", n, p.Frames, want)
		}
		for name, plan := range map[string]*Plan{"detection": mustDetect(t, h264, ops), "matte input": mustMatteInput(t, h264, ops, webp())} {
			if plan.Bounces != 0 || plan.Bounced {
				t.Errorf("%d bounces: %s plan reports Bounces %d Bounced %v", n, name, plan.Bounces, plan.Bounced)
			}
		}
	}
}

func mustMatteInput(t *testing.T, src recipe.ProbeInfo, ops []recipe.Op, out recipe.Output) *Plan {
	t.Helper()
	p, err := CompileMatteInput([]recipe.ProbeInfo{src}, ops, out)
	if err != nil {
		t.Fatalf("CompileMatteInput: %v", err)
	}
	return p
}
