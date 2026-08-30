package graph

// Phase 4 goldens: the bounce op (DESIGN §7, "ping-pong"). Each bounce
// closes the chain into a split, reverses one copy and concatenates
// forward-then-backward — the simple full 2N frames, doubling Frames and
// Duration — after the output fit, in the same group as reverse. Reverse
// ops in front of the first bounce keep their parity; reverse ops behind a
// bounce are dropped (a bounce yields a palindrome, which a reverse leaves
// bit-identical). The pixel-level checks against a real ffmpeg live in
// phase4_ffmpeg_test.go.

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

func bounce() recipe.Op { return op(recipe.OpBounce, nil) }

// bounceChain is the filter text of the N-th bounce op: it interrupts the
// chain ("…,format=rgba,split[fN][rN];…;[fN][rrN]concat…,") and the chain
// continues from the concat.
func bounceChain(n int) string {
	k := strconv.Itoa(n)
	return "format=rgba,split[f" + k + "][r" + k + "];" +
		"[r" + k + "]reverse[rr" + k + "];" +
		"[f" + k + "][rr" + k + "]concat=n=2:v=1:a=0"
}

func TestCompileBounce(t *testing.T) {
	tests := []struct {
		name string
		srcs []recipe.ProbeInfo
		ops  []recipe.Op
		out  recipe.Output
		want Plan // OutLabel implied; Speed 0 means 1; SourceFPS 0 means the main source's
	}{
		{
			name: "bounce alone doubles frames and duration",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{bounce()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + bounceChain(1) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 20, Frames: 598, Bounced: true,
			},
		},
		{
			// reverse(x) then bounce: the first half plays backwards, the
			// second forwards (9..0,0..9 on a 10-frame clip).
			name: "reverse then bounce keeps the reverse in front of the split",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{reverse(), bounce()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,format=rgba,reverse," + bounceChain(1) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 20, Frames: 598,
				Reversed: true, Bounced: true,
			},
		},
		{
			// bounce(x) then reverse: a bounce yields a palindrome, and the
			// reverse of a palindrome is the palindrome — the reverse op is
			// dropped and the plan matches a bare bounce.
			name: "bounce then reverse drops the reverse of the palindrome",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{bounce(), reverse()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + bounceChain(1) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 20, Frames: 598, Bounced: true,
			},
		},
		{
			name: "two bounce ops quadruple frames and duration",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{bounce(), bounce()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + bounceChain(1) + "," + bounceChain(2) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 40, Frames: 1196, Bounced: true,
			},
		},
		{
			name: "trim then bounce doubles the trimmed length; the trim stays a source-time seek",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{trim(1, 3), bounce()}, out: webp(),
			want: Plan{
				InputArgs: []string{"-ss", "1", "-to", "3"},
				Filter:    "[0:v]fps=29.97:round=down," + bounceChain(1) + ",format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 4, Frames: 118,
				TrimStart: 1, TrimEnd: 3, Bounced: true,
			},
		},
		{
			// Sticker-relevant: jobs' Discord sticker <= 5 s check reads
			// Plan.Duration, which already carries the doubling — a 2.6 s
			// trim bounces to 5.2 s and must fail that check downstream.
			name: "sticker maths: a 2.6 s trim bounces to 5.2 s",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{trim(0, 2.6), bounce()}, out: webp(),
			want: Plan{
				InputArgs: []string{"-to", "2.6"},
				Filter:    "[0:v]fps=29.97:round=down," + bounceChain(1) + ",format=rgba[out]",
				Width:     1280, Height: 720, FPS: 29.97, Duration: 5.2, Frames: 154,
				TrimEnd: 2.6, Bounced: true,
			},
		},
		{
			name: "bounce on a trimmed webp_anim: the filter trim stays first, the split follows the fps stage",
			srcs: []recipe.ProbeInfo{webpAnim}, ops: []recipe.Op{trim(0.5, 1.5), bounce()}, out: webp(),
			want: Plan{
				Filter: "[0:v]trim=start=0.5:end=1.5,setpts=PTS-STARTPTS,fps=10:round=down," + bounceChain(1) + ",format=rgba[out]",
				Width:  64, Height: 48, FPS: 10, HasAlpha: true, Duration: 2, Frames: 20,
				TrimStart: 0.5, TrimEnd: 1.5, FilterTrim: true, Bounced: true,
			},
		},
		{
			name: "bounce on an image sequence doubles the exact frame count",
			srcs: []recipe.ProbeInfo{pngSeq}, ops: []recipe.Op{bounce()}, out: webp(),
			want: Plan{
				InputArgs: seqArgs("10"), InputPattern: "%06d.png",
				Filter: seqHead200 + "fps=10:round=down," + bounceChain(1) + ",format=rgba[out]",
				Width:  200, Height: 100, FPS: 10, HasAlpha: true, Duration: 12, Frames: 120, SourceFPS: 10,
				Bounced: true,
			},
		},
		{
			// The final-canvas ops follow the bounce, so a text sits on the
			// doubled (output-time) clip, and the bounce follows the emote
			// fit's scale and pad exactly like reverse does.
			name: "bounce after the contain fit and before the text",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{bounce(), text(recipe.TextParams{Text: "GG"})}, out: recipe.Output{Format: "gif", Width: 128, Height: 128},
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down,scale=128:72:flags=lanczos,format=rgba,pad=128:128:(ow-iw)/2:(oh-ih)/2:color=0x00000000," + bounceChain(1) + ",format=rgba," + text1 + ",format=rgba[out]",
				Width:  128, Height: 128, FPS: 29.97, HasAlpha: true, Duration: 20, Frames: 598,
				Bounced:   true,
				TextFiles: []TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "GG"}},
			},
		},
		{
			// Stack order among {reverse, bounce}: only the parity in front
			// of the first bounce counts — reverse,reverse,bounce,reverse is
			// a bare bounce.
			name: "reverse pairs cancel and post-bounce reverses drop",
			srcs: []recipe.ProbeInfo{h264}, ops: []recipe.Op{reverse(), reverse(), bounce(), reverse()}, out: webp(),
			want: Plan{
				Filter: "[0:v]fps=29.97:round=down," + bounceChain(1) + ",format=rgba[out]",
				Width:  1280, Height: 720, FPS: 29.97, Duration: 20, Frames: 598, Bounced: true,
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
		})
	}
}

func TestCompileBounceErrors(t *testing.T) {
	seq1 := with(withSeq(pngSeq, func(s *recipe.SequenceInfo) { s.Count = 1 }), func(p *recipe.ProbeInfo) { p.Frames, p.Duration = 1, 0.1 })
	tests := []struct {
		name string
		src  recipe.ProbeInfo
		ops  []recipe.Op
		want string // substring of the error
	}{
		{"bounce on a still", still, []recipe.Op{bounce()}, "op 0 (bounce): the source is a still image and cannot be bounced"},
		{"bounce on a one-frame sequence", seq1, []recipe.Op{bounce()}, "op 0 (bounce): the source has a single frame and cannot be bounced"},
		{"the op index names the bounce", h264, []recipe.Op{bounce(), op("sparkle", nil)}, `op 1: unknown op kind "sparkle"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.src, tc.ops, webp())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile: %v, want %q", err, tc.want)
			}
		})
	}
}

// bounceN builds n bounce ops.
func bounceN(n int) []recipe.Op {
	ops := make([]recipe.Op, n)
	for i := range ops {
		ops[i] = bounce()
	}
	return ops
}

// TestBounceCap: MaxBounces bounds the bounce ops per recipe. Each bounce
// nests another split/reverse/concat stage whose reverse branch buffers a
// full copy of the clip, so the structure grows exponentially — and for a
// source whose frame count is unknown (Frames 0) no byte cap can see that.
// Exactly MaxBounces still compile on a small source; one more is refused,
// naming the offending op.
func TestBounceCap(t *testing.T) {
	// pngSeq: 60 frames of 200x100 → 60<<MaxBounces doubled frames, well
	// under the master cap.
	p, err := Compile(pngSeq, bounceN(MaxBounces), webp())
	if err != nil {
		t.Fatalf("%d bounces: %v", MaxBounces, err)
	}
	if want := 60 << MaxBounces; p.Frames != want {
		t.Fatalf("%d bounces: Frames %d, want %d", MaxBounces, p.Frames, want)
	}

	_, err = Compile(pngSeq, bounceN(MaxBounces+1), webp())
	want := fmt.Sprintf("op %d (bounce): at most %d bounce ops per recipe", MaxBounces, MaxBounces)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%d bounces: %v, want %q", MaxBounces+1, err, want)
	}
}

// TestBounceFramesSaturate: assemble's per-bounce Frames doubling saturates
// at MaxInt instead of wrapping. A crafted container can probe to enough
// frames that even MaxBounces doublings overflow int — pre-fix, ~3e18
// frames doubled 8 times wrapped negative, finish's MaxMasterBytes byte
// check saw a negative product and admitted the plan (and Frames 0 reads as
// "unknown" everywhere downstream). Saturated, the master cap rejects it.
func TestBounceFramesSaturate(t *testing.T) {
	// ~3e18 frames at 29.97 fps; 8 doublings would be ~7.7e20, past MaxInt.
	src := with(h264, func(p *recipe.ProbeInfo) { p.Duration = 1e17 })
	_, err := Compile(src, bounceN(MaxBounces), webp())
	if err == nil || !strings.Contains(err.Error(), "exceeds the 8 GiB limit") {
		t.Fatalf("saturated bounce: %v, want the master cap, not a wrapped frame count", err)
	}
}

// TestBounceMasterCap: the MaxMasterBytes check runs on the doubled frame
// count (assemble doubles Frames per bounce before finish measures the
// master), so a large source that fits plain — and with one bounce — fails
// with two, and the emote fit brings even the quadrupled clip back under.
func TestBounceMasterCap(t *testing.T) {
	// 1920x1080 x 300 frames = 2.3 GiB plain, 4.6 GiB after one bounce,
	// 9.3 GiB after two — past the 8 GiB cap.
	src := with(prores, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 10, 300 })

	p, err := Compile(src, []recipe.Op{bounce()}, webp())
	if err != nil {
		t.Fatalf("one bounce: %v", err)
	}
	if p.Frames != 600 || !p.Bounced {
		t.Fatalf("one bounce: Frames %d Bounced %v, want 600 true", p.Frames, p.Bounced)
	}

	_, err = Compile(src, []recipe.Op{bounce(), bounce()}, webp())
	if err == nil || !strings.Contains(err.Error(), "exceeds the 8 GiB limit") || !strings.Contains(err.Error(), "1200 frames") {
		t.Fatalf("two bounces: %v, want the master cap on the quadrupled 1200 frames", err)
	}

	p, err = Compile(src, []recipe.Op{bounce(), bounce()}, recipe.Output{Format: "webp", Width: 128, Height: 128})
	if err != nil {
		t.Fatalf("two bounces with the emote fit: %v", err)
	}
	if p.Frames != 1200 || p.Width != 128 {
		t.Fatalf("two bounces with the emote fit: Frames %d %dx%d", p.Frames, p.Width, p.Height)
	}
}

// TestBounceAfterOutputFit mirrors TestReverseAfterOutputFit: the bounce's
// split/reverse branch buffers every frame it sees until EOF, so it must
// run after the output fit — then the buffered frame is exactly Plan.Width x
// Plan.Height at 4 B/px (the leading format=rgba pins it) and the
// MaxMasterBytes check, which sees the doubled Frames, bounds the buffer
// too. The final-canvas ops still follow it.
func TestBounceAfterOutputFit(t *testing.T) {
	long := with(prores, func(p *recipe.ProbeInfo) { p.Duration, p.Frames = 60, 1800 })
	emote := recipe.Output{Format: "gif", Width: 128, Height: 128, FPS: 30}

	// Sanity: without a fit the doubled master is over the cap.
	if _, err := Compile(long, []recipe.Op{bounce()}, recipe.Output{Format: "gif", FPS: 30}); err == nil || !strings.Contains(err.Error(), "exceeds the 8 GiB limit") {
		t.Fatalf("1080p x 3600 frames without a fit: %v, want the master cap", err)
	}

	cases := []struct {
		name string
		ops  []recipe.Op
		out  recipe.Output
	}{
		{"emote preset: output fit only", []recipe.Op{bounce()}, emote},
		{"cover fit", []recipe.Op{bounce()}, recipe.Output{Format: "webp", Width: 128, Height: 128, Fit: "cover"}},
		{"bounce first in the stack, geometry after it", []recipe.Op{bounce(), crop(0, 0, 1080, 1080), flip(true, false)}, emote},
		{"keyed, trimmed, reversed and bounced emote", []recipe.Op{chromakey(recipe.ChromaKeyParams{}), trim(0, 10), reverse(), bounce()}, emote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(long, tc.ops, tc.out)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if !p.Bounced || p.Width != 128 {
				t.Fatalf("plan: bounced %v %dx%d", p.Bounced, p.Width, p.Height)
			}
			split, scale := strings.Index(p.Filter, ",format=rgba,split[f1][r1]"), strings.LastIndex(p.Filter, "scale=")
			if split < 0 || scale < 0 || split < scale {
				t.Errorf("the bounce split must follow the output fit's scale (it buffers the frames it sees):\n%s", p.Filter)
			}
			if pad := strings.LastIndex(p.Filter, "pad="); pad >= 0 && split < pad {
				t.Errorf("the bounce split must follow the fit's pad:\n%s", p.Filter)
			}
			// Nothing after the concat changes the frame size: the tail is
			// the terminal format=rgba only.
			if !strings.HasSuffix(p.Filter, ";[f1][rr1]concat=n=2:v=1:a=0,format=rgba[out]") {
				t.Errorf("the chain after the concat must hold only the terminal format:\n%s", p.Filter)
			}
		})
	}

	// Final-canvas ops still follow the bounce (their timing is in the
	// doubled output time), and the fit precedes it.
	p, err := CompileWithSources([]recipe.ProbeInfo{long, ovPNG}, []recipe.Op{bounce(), text(recipe.TextParams{Text: "x"}), overlay(recipe.OverlayParams{Source: 1})}, emote)
	if err != nil {
		t.Fatalf("Compile with text and overlay: %v", err)
	}
	cat := strings.Index(p.Filter, "]concat=n=2:v=1:a=0,")
	if cat < 0 || cat < strings.Index(p.Filter, "scale=") || cat > strings.Index(p.Filter, "drawtext") || cat > strings.Index(p.Filter, "overlay=") {
		t.Errorf("the bounce must sit between the output fit and the final-canvas ops:\n%s", p.Filter)
	}
}

// TestDetectIgnoresBounce: CompileDetect (the autocrop detection pass) only
// compiles the stages in front of the geometry; a bounce op is ignored like
// reverse — the content box of a bounced clip is the box of the forward
// half, so the detection needs no doubling ("bounce invisible to detection",
// phase4 design §7).
func TestDetectIgnoresBounce(t *testing.T) {
	p, err := CompileDetect([]recipe.ProbeInfo{h264}, []recipe.Op{trim(1, 3), bounce(), fps(10)})
	if err != nil {
		t.Fatalf("CompileDetect: %v", err)
	}
	if p.Bounced {
		t.Errorf("detection plan must not be Bounced: %+v", p)
	}
	if strings.Contains(p.Filter, "split") || strings.Contains(p.Filter, "concat") {
		t.Errorf("detection plan must not carry the bounce stages: %s", p.Filter)
	}
	if p.Duration != 2 || p.Frames != 20 {
		t.Errorf("detection plan must not double: Duration %v Frames %d, want 2 s / 20", p.Duration, p.Frames)
	}
}
