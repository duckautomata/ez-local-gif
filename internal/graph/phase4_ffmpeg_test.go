package graph_test

// Real-ffmpeg pixel checks of the Phase 4 bounce op: a numbered clip is
// bounced and the rendered master's frame identities are read back off the
// rawvideo output — the sequence must be the forward frames followed by
// their exact mirror (the full 2N, turnaround frame duplicated, as
// documented), reverse must compose with bounce in stack order, and the
// plan's doubled Frames must match what ffmpeg actually emits. Skips when
// ffmpeg is not on PATH (see ffmpegOrSkip).

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

func TestBouncePixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	const n = 10
	frames := make([]image.Image, n)
	for i := range frames {
		frames[i] = solid(16, 16, color.NRGBA{R: uint8(i + 1), G: 200, A: 255})
	}
	clip, info := pngClip(t, ff, dir, "idx", frames, 10, false)
	out := recipe.Output{Format: "webp"}
	bounceOp := recipe.Op{Kind: recipe.OpBounce}
	revOp := recipe.Op{Kind: recipe.OpReverse}
	// index reads a frame's source identity (0-based) off its solid red
	// channel; w is the frame width.
	index := func(f []byte, w int) int { return int(pixel(f, w, w/2, w/2)[0]) - 1 }

	// fwd/bwd build expected identity sequences a..b inclusive (either way).
	seq := func(a, b int) []int {
		var s []int
		if a <= b {
			for i := a; i <= b; i++ {
				s = append(s, i)
			}
		} else {
			for i := a; i >= b; i-- {
				s = append(s, i)
			}
		}
		return s
	}
	cat := func(parts ...[]int) []int {
		var s []int
		for _, p := range parts {
			s = append(s, p...)
		}
		return s
	}

	cases := []struct {
		name     string
		ops      []recipe.Op
		out      recipe.Output
		w        int
		want     []int
		reversed bool
	}{
		{"bounce: 0..9,9..0", []recipe.Op{bounceOp}, out, 16, cat(seq(0, 9), seq(9, 0)), false},
		{"reverse then bounce: 9..0,0..9", []recipe.Op{revOp, bounceOp}, out, 16, cat(seq(9, 0), seq(0, 9)), true},
		{"bounce then reverse: the palindrome is unchanged", []recipe.Op{bounceOp, revOp}, out, 16, cat(seq(0, 9), seq(9, 0)), false},
		// bounce(P) = P ++ reverse(P), and the reverse of the palindrome P
		// is P itself: the palindrome repeats.
		{"two bounces: the palindrome repeated", []recipe.Op{bounceOp, bounceOp}, out, 16, cat(seq(0, 9), seq(9, 0), seq(0, 9), seq(9, 0)), false},
		{"trim 0.2..0.5 then bounce: 2..4,4..2", []recipe.Op{{Kind: recipe.OpTrim, Params: []byte(`{"start":0.2,"end":0.5}`)}, bounceOp}, out, 16, cat(seq(2, 4), seq(4, 2)), false},
		{"bounce after the output fit", []recipe.Op{bounceOp}, recipe.Output{Format: "webp", Width: 8, Height: 8}, 8, cat(seq(0, 9), seq(9, 0)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := compileSrcs(t, []recipe.ProbeInfo{info}, tc.ops, tc.out)
			if !p.Bounced || p.Reversed != tc.reversed || !strings.Contains(p.Filter, "]concat=n=2:v=1:a=0") {
				t.Fatalf("plan: Bounced %v Reversed %v filter %s", p.Bounced, p.Reversed, p.Filter)
			}
			if p.Frames != len(tc.want) {
				t.Fatalf("plan Frames %d, want %d", p.Frames, len(tc.want))
			}
			if want := float64(len(tc.want)) / 10; p.Duration < want-1e-9 || p.Duration > want+1e-9 {
				t.Fatalf("plan Duration %v, want %v", p.Duration, want)
			}
			fr := p3Render(t, ff, clip, p, nil, nil)
			if len(fr) != len(tc.want) {
				t.Fatalf("rendered %d frames, plan/want %d", len(fr), len(tc.want))
			}
			for i, f := range fr {
				if got := index(f, tc.w); got != tc.want[i] {
					t.Errorf("frame %d is source frame %d, want %d", i, got, tc.want[i])
				}
			}
		})
	}
}
