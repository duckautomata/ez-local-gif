package enc

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// assertArgs compares argv slices and prints an aligned diff on mismatch so
// a golden failure is readable at a glance.
func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	var b strings.Builder
	n := max(len(got), len(want))
	for i := 0; i < n; i++ {
		g, w := "<missing>", "<missing>"
		if i < len(got) {
			g = fmt.Sprintf("%q", got[i])
		}
		if i < len(want) {
			w = fmt.Sprintf("%q", want[i])
		}
		mark := "  "
		if g != w {
			mark = "!!"
		}
		fmt.Fprintf(&b, "%s [%2d] got  %s\n%s      want %s\n", mark, i, g, mark, w)
	}
	t.Errorf("argv mismatch (got %d args, want %d):\n%s", len(got), len(want), b.String())
}

// testPlan is a representative trimmed, alpha-carrying plan (a 25 fps source
// trimmed to 1.5..4 s, rendered at 25 fps).
func testPlan() *graph.Plan {
	return &graph.Plan{
		InputArgs: []string{"-ss", "1.5", "-to", "4"},
		Filter:    "[0:v]fps=25,format=rgba[out]",
		OutLabel:  "[out]",
		Width:     320,
		Height:    240,
		FPS:       25,
		HasAlpha:  true,
		Duration:  2.5,
		Frames:    62,
		TrimStart: 1.5,
		TrimEnd:   4,
		Speed:     1,
		SourceFPS: 25,
	}
}

func testMaster() Master {
	return Master{Path: "/dev/shm/ezl/job1/frames.rgba", Width: 320, Height: 240, FPS: 25, Frames: 62, HasAlpha: true}
}

func TestMasterArgs(t *testing.T) {
	got := MasterArgs("/data/blobs/ab/abcd.mov", testPlan(), "/dev/shm/ezl/job1/frames.rgba")
	want := []string{
		"-ss", "1.5", "-to", "4",
		"-i", "/data/blobs/ab/abcd.mov",
		"-filter_complex", "[0:v]fps=25,format=rgba[out]",
		"-map", "[out]",
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgba",
		"/dev/shm/ezl/job1/frames.rgba",
	}
	assertArgs(t, got, want)
}

func TestMasterArgs_NoInputArgsAndDefaultLabel(t *testing.T) {
	p := &graph.Plan{Filter: "[0:v]format=rgba[out]"}
	got := MasterArgs("in.gif", p, "frames.rgba")
	want := []string{
		"-i", "in.gif",
		"-filter_complex", "[0:v]format=rgba[out]",
		"-map", "[out]",
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgba",
		"frames.rgba",
	}
	assertArgs(t, got, want)
}

func TestMasterArgs_NilPlan(t *testing.T) {
	if got := MasterArgs("a", nil, "b"); got != nil {
		t.Fatalf("nil plan: got %v, want nil", got)
	}
	if got := StillArgs("a", nil, 0, 0); got != nil {
		t.Fatalf("nil plan: got %v, want nil", got)
	}
	if got := ProxyArgs("a", nil, 0, 0, "b"); got != nil {
		t.Fatalf("nil plan: got %v, want nil", got)
	}
}

// stillFilter builds the expected still filtergraph for testPlan-like plans:
// the plan filter, then tpad (clone for pad s) + select (gte(t,thr)) and,
// when scale is non-empty, the preview scale, all ending in [outs].
func stillFilter(plan, pad, thr, scale string) string {
	f := plan + ";[out]tpad=stop_mode=clone:stop_duration=" + pad + ",select='gte(t," + thr + ")'"
	if scale != "" {
		f += "," + scale
	}
	return f + "[outs]"
}

// Preview scale stages: alpha plans use the premultiplied chain graph.emitScale
// emits, alpha-less plans a bare lanczos scale.
const (
	alphaScale480 = "format=gbrap,premultiply=inplace=1,scale=w='min(iw,480)':h=-1:flags=lanczos,unpremultiply=inplace=1,format=rgba"
	alphaScale128 = "format=gbrap,premultiply=inplace=1,scale=w='min(iw,128)':h=-1:flags=lanczos,unpremultiply=inplace=1,format=rgba"
	plainScale480 = "scale=w='min(iw,480)':h=-1:flags=lanczos"
	plainScale360 = "scale=w='min(iw,360)':h=-1:flags=lanczos"
	alphaScale360 = "format=gbrap,premultiply=inplace=1,scale=w='min(iw,360)':h=-1:flags=lanczos,unpremultiply=inplace=1,format=rgba"
	plainScale200 = "scale=w='min(iw,200)':h=-1:flags=lanczos"
)

func TestStillArgs(t *testing.T) {
	const src = "/data/blobs/ab/abcd.mov"
	const planFilter = "[0:v]fps=25,format=rgba[out]"
	stillTail := func(filter string) []string {
		return []string{
			"-i", src,
			"-frames:v", "1",
			"-filter_complex", filter,
			"-map", "[outs]",
			"-c:v", "png", "-compression_level", "1",
			"-f", "image2pipe", "pipe:1",
		}
	}
	// testPlan: 25 fps source and output, Speed 1 → one slot is 0.04 s; the
	// seek-back is max(2/25, 0.1) + 0.04 = 0.14 s, then snapped down onto the
	// slot grid TrimStart + k*0.04.

	tests := []struct {
		name string
		mod  func(p *graph.Plan)
		t    float64
		maxW int
		want []string
	}{
		{
			// t=1 → source 2.5 = slot 25; seek 2.36 snaps to 2.34 (slot 21), so
			// the wanted slot is 4 after the seek: threshold (4-0.5)/25 = 0.14,
			// pad 4/25 + 1 = 1.16.
			name: "seek a little before the target on the slot grid, scaled",
			t:    1, maxW: 480,
			want: append([]string{"-ss", "2.34"}, stillTail(stillFilter(planFilter, "1.16", "0.14", alphaScale480))...),
		},
		{
			name: "t=0 seeks to TrimStart and selects the first slot",
			t:    0, maxW: 480,
			want: append([]string{"-ss", "1.5"}, stillTail(stillFilter(planFilter, "1", "0", alphaScale480))...),
		},
		{
			name: "negative t clamps to TrimStart",
			t:    -3, maxW: 480,
			want: append([]string{"-ss", "1.5"}, stillTail(stillFilter(planFilter, "1", "0", alphaScale480))...),
		},
		{
			// Target clamps to 3.999 (slot 62, the last of 63); seek 3.859 snaps
			// to 3.82 (slot 58) → slot 4 after the seek. The frames from 3.82 on
			// exist, and tpad holds the last one if the input ends first.
			name: "t past the trim end clamps to just inside TrimEnd",
			t:    99, maxW: 480,
			want: append([]string{"-ss", "3.82"}, stillTail(stillFilter(planFilter, "1.16", "0.14", alphaScale480))...),
		},
		{
			// Source 3.5; slots are 0.08 s of source: back 0.18 → 3.32 snaps to
			// 3.26 (slot 22); wanted slot 25 → 3 after the seek.
			name: "speed 2 maps output time to source time",
			mod:  func(p *graph.Plan) { p.Speed = 2 },
			t:    1, maxW: 480,
			want: append([]string{"-ss", "3.26"}, stillTail(stillFilter(planFilter, "1.12", "0.1", alphaScale480))...),
		},
		{
			// Source 2; slots are 0.02 s of source: back 0.12 → 1.88 (slot 19);
			// wanted slot 25 → 6 after the seek.
			name: "speed 0.5 (slow motion) maps output time to source time",
			mod:  func(p *graph.Plan) { p.Speed = 0.5 },
			t:    1, maxW: 480,
			want: append([]string{"-ss", "1.88"}, stillTail(stillFilter(planFilter, "1.24", "0.22", alphaScale480))...),
		},
		{
			name: "zero Speed counts as 1",
			mod:  func(p *graph.Plan) { p.Speed = 0 },
			t:    1, maxW: 480,
			want: append([]string{"-ss", "2.34"}, stillTail(stillFilter(planFilter, "1.16", "0.14", alphaScale480))...),
		},
		{
			name: "maxW 0 skips the scale but keeps tpad/select and the [outs] label",
			t:    1, maxW: 0,
			want: append([]string{"-ss", "2.34"}, stillTail(stillFilter(planFilter, "1.16", "0.14", ""))...),
		},
		{
			name: "no alpha: plain scale",
			mod:  func(p *graph.Plan) { p.HasAlpha = false },
			t:    1, maxW: 480,
			want: append([]string{"-ss", "2.34"}, stillTail(stillFilter(planFilter, "1.16", "0.14", plainScale480))...),
		},
		{
			// TrimEnd 0 and Duration 0: nothing to clamp against, so the seek
			// follows t (12.11 → slot 302 = 12.08).
			name: "no trim, unknown duration: seek follows t unclamped",
			mod: func(p *graph.Plan) {
				p.InputArgs = nil
				p.TrimStart, p.TrimEnd, p.Duration = 0, 0, 0
			},
			t: 12.25, maxW: 480,
			want: append([]string{"-ss", "12.08"}, stillTail(stillFilter(planFilter, "1.16", "0.14", alphaScale480))...),
		},
		{
			// TrimEnd 0 but Duration 2.5: the source ends at 2.5, so t is clamped
			// to 2.499 (slot 62) and the seek to 2.32 (slot 58).
			name: "no trim, known duration: t is clamped to just inside the source",
			mod: func(p *graph.Plan) {
				p.InputArgs = nil
				p.TrimStart, p.TrimEnd = 0, 0
			},
			t: 12.25, maxW: 480,
			want: append([]string{"-ss", "2.32"}, stillTail(stillFilter(planFilter, "1.16", "0.14", alphaScale480))...),
		},
		{
			// Duration 4 s at 30 fps, plan at 33.333 fps (an fps op of 100/3;
			// gif no longer snaps to it, but any fractional rate exercises the
			// same arithmetic): t = Duration (the scrubber's maximum) clamps to
			// 3.999 = slot 133; back is 0.1 + 1/33.333, the seek snaps to slot
			// 128 = 3.840038 s, and the wanted slot is 5 after it. Six-decimal
			// seek/threshold text.
			name: "t = Duration on a CFR clip at a fractional rate seeks before the last frames",
			mod: func(p *graph.Plan) {
				p.InputArgs = nil
				p.Filter = "[0:v]fps=33.333,format=rgba[out]"
				p.TrimStart, p.TrimEnd, p.Duration, p.FPS, p.SourceFPS = 0, 0, 4, 33.333, 30
			},
			t: 4, maxW: 480,
			want: append([]string{"-ss", "3.840038"},
				stillTail(stillFilter("[0:v]fps=33.333,format=rgba[out]", "1.150002", "0.135001", alphaScale480))...),
		},
		{
			// A 4 fps plan needs a whole 0.25 s slot of input after the seek
			// or the fps stage rounds it to zero frames: back = 0.1 + 0.25.
			name: "low output fps widens the seek-back by one slot",
			mod: func(p *graph.Plan) {
				p.InputArgs = nil
				p.Filter = "[0:v]fps=4,format=rgba[out]"
				p.TrimStart, p.TrimEnd, p.Duration, p.FPS, p.SourceFPS = 0, 0, 4, 4, 30
			},
			t: 4, maxW: 480,
			want: append([]string{"-ss", "3.5"},
				stillTail(stillFilter("[0:v]fps=4,format=rgba[out]", "1.25", "0.125", alphaScale480))...),
		},
		{
			// Unknown source rate: 0.5 s back (+ one slot) → 1.96 → slot 11 =
			// 1.94; wanted slot 25 → 14 after the seek.
			name: "unknown source fps seeks half a second back",
			mod:  func(p *graph.Plan) { p.SourceFPS = 0 },
			t:    1, maxW: 480,
			want: append([]string{"-ss", "1.94"}, stillTail(stillFilter(planFilter, "1.56", "0.54", alphaScale480))...),
		},
		{
			// A low source rate widens the seek-back (2/5 s + one slot = 0.44):
			// 2.06 → slot 14 = 2.06; wanted slot 25 → 11 after the seek.
			name: "low source fps seeks two source frames back",
			mod:  func(p *graph.Plan) { p.SourceFPS = 5 },
			t:    1, maxW: 480,
			want: append([]string{"-ss", "2.06"}, stillTail(stillFilter(planFilter, "1.44", "0.42", alphaScale480))...),
		},
		{
			// t=0 with no trim: nothing to seek to, so -ss is omitted entirely
			// (FFmpeg 9's webp_anim demuxer returns nothing after any seek).
			name: "no trim and t=0 emits no -ss",
			mod: func(p *graph.Plan) {
				p.InputArgs = nil
				p.TrimStart, p.TrimEnd = 0, 0
			},
			t: 0, maxW: 480,
			want: stillTail(stillFilter(planFilter, "1", "0", alphaScale480)),
		},
		{
			// Source 2.0 → slot 12 (12.5 floors to 12); seek 1.86 (slot 9) →
			// slot 3 after the seek.
			name: "non-seek input args survive, -ss/-to/-t are stripped",
			mod: func(p *graph.Plan) {
				p.InputArgs = []string{"-ss", "1.5", "-c:v", "libvpx-vp9", "-t", "2.5", "-to", "4"}
			},
			t: 0.5, maxW: 128,
			want: append([]string{"-ss", "1.86", "-c:v", "libvpx-vp9"},
				stillTail(stillFilter(planFilter, "1.12", "0.1", alphaScale128))...),
		},
		{
			// TrimStart 0.1, no end, Duration 2.5 → source ends at 2.6; target
			// 0.3 (slot 5); 0.16 snaps to slot 1 = 0.14.
			name: "fractional trim start stays on the grid",
			mod:  func(p *graph.Plan) { p.TrimStart = 0.1; p.TrimEnd = 0; p.InputArgs = nil },
			t:    0.2, maxW: 480,
			want: append([]string{"-ss", "0.14"}, stillTail(stillFilter(planFilter, "1.16", "0.14", alphaScale480))...),
		},
		{
			// A still image plan (no fps stage, Duration 0, one frame): no seek,
			// first frame.
			name: "still image plan: no seek, first frame",
			mod: func(p *graph.Plan) {
				p.InputArgs = nil
				p.Filter = "[0:v]format=rgba[out]"
				p.TrimStart, p.TrimEnd, p.Duration, p.FPS, p.SourceFPS, p.Frames = 0, 0, 0, 10, 0, 1
			},
			t: 0, maxW: 480,
			want: stillTail(stillFilter("[0:v]format=rgba[out]", "1", "0", alphaScale480)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := testPlan()
			if tc.mod != nil {
				tc.mod(p)
			}
			assertArgs(t, StillArgs(src, p, tc.t, tc.maxW), tc.want)
		})
	}
}

func TestStillArgsFromStart(t *testing.T) {
	const src = "/data/blobs/ab/abcd.gif"
	stillTail := func(filter string) []string {
		return []string{
			"-i", src,
			"-frames:v", "1",
			"-filter_complex", filter,
			"-map", "[outs]",
			"-c:v", "png", "-compression_level", "1",
			"-f", "image2pipe", "pipe:1",
		}
	}
	const planFilter = "[0:v]fps=25,format=rgba[out]"

	t.Run("trimmed plan seeks to TrimStart only", func(t *testing.T) {
		// t=1 → slot 25 counted from TrimStart: threshold 24.5/25 = 0.98,
		// pad 25/25 + 1 = 2.
		got := StillArgsFromStart(src, testPlan(), 1, 480)
		want := append([]string{"-ss", "1.5"}, stillTail(stillFilter(planFilter, "2", "0.98", alphaScale480))...)
		assertArgs(t, got, want)
	})

	t.Run("untrimmed plan decodes from the start without -ss", func(t *testing.T) {
		// A 3 s VFR GIF (10 x 0.1 s frames + a 2 s hold) at 10 fps: t=2.5 lies
		// in the hold; slot 25 → threshold 2.45, pad 3.5. The fps stage carries
		// the held frame to that slot, which the seeking StillArgs cannot.
		p := testPlan()
		p.InputArgs = nil
		p.Filter = "[0:v]fps=10,format=rgba[out]"
		p.TrimStart, p.TrimEnd, p.Duration, p.FPS, p.SourceFPS = 0, 0, 3, 10, 100.0/27
		got := StillArgsFromStart(src, p, 2.5, 480)
		want := stillTail(stillFilter("[0:v]fps=10,format=rgba[out]", "3.5", "2.45", alphaScale480))
		assertArgs(t, got, want)
	})

	t.Run("t beyond the duration still clamps", func(t *testing.T) {
		// Duration 2.5 → 2.499 → slot 62: threshold 61.5/25 = 2.46, pad 3.48.
		p := testPlan()
		p.InputArgs = nil
		p.TrimStart, p.TrimEnd = 0, 0
		got := StillArgsFromStart(src, p, 99, 0)
		want := stillTail(stillFilter(planFilter, "3.48", "2.46", ""))
		assertArgs(t, got, want)
	})

	t.Run("nil plan", func(t *testing.T) {
		if got := StillArgsFromStart(src, nil, 1, 480); got != nil {
			t.Fatalf("nil plan: got %v, want nil", got)
		}
	})
}

// TestStillSeekInvariants checks the seek geometry that keeps the still from
// ever asking ffmpeg for a frame that cannot exist: the seek is never after
// the target, never before TrimStart, at least the seek-back plus one slot
// before the target unless TrimStart intervenes, on the slot grid, and the
// select threshold/pad cover the wanted slot.
func TestStillSeekInvariants(t *testing.T) {
	plans := []struct {
		name string
		p    graph.Plan
	}{
		{"cfr 30 → fractional 33.333 fps, 4 s", graph.Plan{FPS: 33.333, SourceFPS: 30, Duration: 4, Speed: 1}},
		{"cfr 29.97 → gif 29.97 (unsnapped), 10 s", graph.Plan{FPS: 29.97, SourceFPS: 30000.0 / 1001, Duration: 10, Speed: 1}},
		{"trim 1.5..4, speed 2, 25 fps", graph.Plan{FPS: 25, SourceFPS: 25, Duration: 1.25, TrimStart: 1.5, TrimEnd: 4, Speed: 2}},
		{"slow motion 0.25, 29.97 source, 10 fps", graph.Plan{FPS: 10, SourceFPS: 29.97, Duration: 40, Speed: 0.25}},
		{"unknown source fps and duration", graph.Plan{FPS: 15, Speed: 1}},
		{"low fps 2, source 5", graph.Plan{FPS: 2, SourceFPS: 5, Duration: 10, Speed: 1}},
	}
	for _, pl := range plans {
		t.Run(pl.name, func(t *testing.T) {
			p := pl.p
			speed := p.Speed
			period := speed / p.FPS
			for _, tt := range []float64{0, 0.001, 0.02, 0.5, 1, 1.234, 2.499, 2.5, 3.9, 3.999, 4, 10, 39.99, 40, 99} {
				s := stillSeekFor(&p, tt, false)
				trimStart := p.TrimStart
				target := trimStart + tt*speed
				srcEnd := p.TrimEnd
				if srcEnd == 0 && p.Duration > 0 {
					srcEnd = trimStart + p.Duration*speed
				}
				if srcEnd > 0 {
					target = math.Min(target, srcEnd-stillEndMargin)
				}
				if s.start < trimStart-1e-9 {
					t.Errorf("t=%v: start %v before TrimStart %v", tt, s.start, trimStart)
				}
				if s.start > target+1e-9 {
					t.Errorf("t=%v: start %v after target %v", tt, s.start, target)
				}
				back := stillUnknownFPSSeekBack
				if p.SourceFPS > 0 {
					back = math.Max(2/p.SourceFPS, stillMinSeekBack)
				}
				if s.start > trimStart+1e-9 && target-s.start < back+period-1e-9 {
					t.Errorf("t=%v: seek-back %v shorter than %v", tt, target-s.start, back+period)
				}
				slots := (s.start - trimStart) / period
				if math.Abs(slots-math.Round(slots)) > 1e-6 {
					t.Errorf("t=%v: start %v is not on the slot grid (%v slots)", tt, s.start, slots)
				}
				// The wanted slot (relative to the seek) is between the threshold
				// and threshold + one slot, and the pad reaches it.
				want := math.Floor((target-trimStart)/speed*p.FPS+stillSlotEpsilon) - math.Round(slots)
				if want < 0 {
					want = 0
				}
				slotT := want / p.FPS
				if slotT < s.threshold-1e-9 || slotT-s.threshold > 1/p.FPS+1e-9 {
					t.Errorf("t=%v: wanted slot at %v not covered by threshold %v", tt, slotT, s.threshold)
				}
				if s.pad < slotT+stillPadSlack-1e-9 {
					t.Errorf("t=%v: pad %v does not reach slot %v + slack", tt, s.pad, slotT)
				}
				if s.threshold < 0 || s.pad <= 0 {
					t.Errorf("t=%v: threshold %v pad %v", tt, s.threshold, s.pad)
				}
				// The from-start variant starts at TrimStart and selects the same
				// absolute slot.
				fs := stillSeekFor(&p, tt, true)
				if math.Abs(fs.start-trimStart) > 1e-9 {
					t.Errorf("t=%v: from-start seek %v != TrimStart %v", tt, fs.start, trimStart)
				}
				if math.Abs((fs.threshold-s.threshold)-slots/p.FPS) > 1e-6 {
					t.Errorf("t=%v: from-start threshold %v vs %v (+%v slots)", tt, fs.threshold, s.threshold, slots)
				}
			}
		})
	}
	// Garbage in: NaN/Inf/negative t and Speed, missing FPS never panic and
	// still give a usable seek.
	for _, tt := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -5} {
		s := stillSeekFor(&graph.Plan{Speed: math.Inf(1), TrimStart: -2}, tt, false)
		if s.start != 0 || s.threshold != 0 || s.pad != stillPadSlack {
			t.Errorf("t=%v: got %+v", tt, s)
		}
	}
}

func TestProxyArgs(t *testing.T) {
	const src = "/data/blobs/ab/abcd.mov"
	proxyTail := func(filter, pix, dur string) []string {
		return []string{
			"-i", src,
			"-filter_complex", filter,
			"-map", "[outp]",
			"-an", "-sn", "-dn",
			"-t", dur,
			"-c:v", "libwebp_anim",
			"-lossless", "0",
			"-q:v", "60",
			"-compression_level", "0",
			"-pix_fmt", pix,
			"-loop", "0",
			"-map_metadata", "-1",
			"-f", "webp",
			"proxy.webp",
		}
	}

	t.Run("defaults: 360 px, 10 s, fps capped at 15, alpha uses the premultiplied scale", func(t *testing.T) {
		got := ProxyArgs(src, testPlan(), 0, 0, "proxy.webp")
		want := append([]string{"-ss", "1.5", "-to", "4"},
			proxyTail("[0:v]fps=25,format=rgba[out];[out]fps=15,"+alphaScale360+"[outp]", "yuva420p", "10")...)
		assertArgs(t, got, want)
	})

	t.Run("explicit size/duration, low fps plan without alpha keeps the plain scale", func(t *testing.T) {
		p := testPlan()
		p.FPS = 12.5
		p.HasAlpha = false
		p.InputArgs = nil
		got := ProxyArgs(src, p, 200, 2.5, "proxy.webp")
		want := proxyTail("[0:v]fps=25,format=rgba[out];[out]"+plainScale200+"[outp]", "yuv420p", "2.5")
		assertArgs(t, got, want)
	})

	t.Run("exactly 15 fps needs no fps filter", func(t *testing.T) {
		p := testPlan()
		p.FPS = 15
		p.InputArgs = nil
		got := ProxyArgs(src, p, 0, 0, "proxy.webp")
		want := proxyTail("[0:v]fps=25,format=rgba[out];[out]"+alphaScale360+"[outp]", "yuva420p", "10")
		assertArgs(t, got, want)
	})

	t.Run("unknown plan fps still caps at 15", func(t *testing.T) {
		p := testPlan()
		p.FPS = 0
		p.InputArgs = nil
		got := ProxyArgs(src, p, 0, 0, "proxy.webp")
		want := proxyTail("[0:v]fps=25,format=rgba[out];[out]fps=15,"+alphaScale360+"[outp]", "yuva420p", "10")
		assertArgs(t, got, want)
	})

	t.Run("no alpha at 360 keeps the plain scale", func(t *testing.T) {
		p := testPlan()
		p.HasAlpha = false
		p.InputArgs = nil
		got := ProxyArgs(src, p, 0, 0, "proxy.webp")
		want := proxyTail("[0:v]fps=25,format=rgba[out];[out]fps=15,"+plainScale360+"[outp]", "yuv420p", "10")
		assertArgs(t, got, want)
	})
}

func TestRawInputArgs(t *testing.T) {
	tests := []struct {
		name string
		m    Master
		want []string
	}{
		{
			name: "integer fps",
			m:    testMaster(),
			want: []string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25", "-i", "/dev/shm/ezl/job1/frames.rgba"},
		},
		{
			name: "fractional fps keeps six decimals",
			m:    Master{Path: "frames.rgba", Width: 128, Height: 128, FPS: 100.0 / 3},
			want: []string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "128x128", "-r", "33.333333", "-i", "frames.rgba"},
		},
		{
			name: "short decimals stay short",
			m:    Master{Path: "frames.rgba", Width: 128, Height: 128, FPS: 12.5},
			want: []string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "128x128", "-r", "12.5", "-i", "frames.rgba"},
		},
		{
			name: "missing fps falls back to 25",
			m:    Master{Path: "frames.rgba", Width: 128, Height: 128},
			want: []string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "128x128", "-r", "25", "-i", "frames.rgba"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, RawInputArgs(tc.m), tc.want)
		})
	}
}

// gifAlphaFilter is the exact DESIGN.md §4.2 filtergraph with defaults.
const gifAlphaFilter = "[0:v]split[c][a];" +
	"[a]alphaextract,lut=c0='gte(val,128)*255'[m];" +
	"color=c=0x313338:s=320x240:r=25,format=rgba[bg];" +
	"[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];" +
	"[f][m]alphamerge,split[p1][p2];" +
	"[p1]palettegen=max_colors=256:reserve_transparent=1:stats_mode=diff[pal];" +
	"[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle:alpha_threshold=128[out]"

func TestGIFArgs(t *testing.T) {
	raw := []string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25", "-i", "/dev/shm/ezl/job1/frames.rgba"}
	gifTail := func(filter, loop string) []string {
		return []string{"-filter_complex", filter, "-map", "[out]", "-loop", loop, "-f", "gif", "out.gif"}
	}

	tests := []struct {
		name string
		m    Master
		o    GIFOptions
		want []string
	}{
		{
			name: "defaults with alpha (Discord-safe)",
			m:    testMaster(),
			o:    GIFOptions{HasAlpha: true},
			want: append(append([]string{}, raw...), gifTail(gifAlphaFilter, "0")...),
		},
		{
			name: "defaults without alpha",
			m:    testMaster(),
			o:    GIFOptions{},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[p1][p2];"+
					"[p1]palettegen=max_colors=256:reserve_transparent=0:stats_mode=diff[pal];"+
					"[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle[out]", "0")...),
		},
		{
			name: "explicit options with alpha",
			m:    testMaster(),
			o: GIFOptions{
				Colors: 128, Dither: "sierra2_4a", AlphaThreshold: 180, Matte: "#FFFFFF",
				Loop: 3, StatsMode: "full", HasAlpha: true,
			},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[c][a];"+
					"[a]alphaextract,lut=c0='gte(val,180)*255'[m];"+
					"color=c=0xffffff:s=320x240:r=25,format=rgba[bg];"+
					"[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];"+
					"[f][m]alphamerge,split[p1][p2];"+
					"[p1]palettegen=max_colors=128:reserve_transparent=1:stats_mode=full[pal];"+
					"[p2][pal]paletteuse=dither=sierra2_4a:diff_mode=rectangle:alpha_threshold=128[out]", "3")...),
		},
		{
			name: "explicit options without alpha, floyd_steinberg, custom bayer scale ignored",
			m:    testMaster(),
			o:    GIFOptions{Colors: 64, Dither: "floyd_steinberg", BayerScale: 5, Loop: 1},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[p1][p2];"+
					"[p1]palettegen=max_colors=64:reserve_transparent=0:stats_mode=diff[pal];"+
					"[p2][pal]paletteuse=dither=floyd_steinberg:diff_mode=rectangle[out]", "1")...),
		},
		{
			name: "bayer with explicit scale and dither none",
			m:    testMaster(),
			o:    GIFOptions{Dither: "bayer", BayerScale: 1},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[p1][p2];"+
					"[p1]palettegen=max_colors=256:reserve_transparent=0:stats_mode=diff[pal];"+
					"[p2][pal]paletteuse=dither=bayer:bayer_scale=1:diff_mode=rectangle[out]", "0")...),
		},
		{
			name: "out-of-range values are clamped, bad enums fall back",
			m:    Master{Path: "/dev/shm/ezl/job1/frames.rgba", Width: 320, Height: 240, FPS: 25},
			o: GIFOptions{
				Colors: 999, Dither: "bogus", BayerScale: 9, AlphaThreshold: 300,
				Matte: "not-a-colour", Loop: -5, StatsMode: "weird", HasAlpha: true,
			},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[c][a];"+
					"[a]alphaextract,lut=c0='gte(val,255)*255'[m];"+
					"color=c=0x313338:s=320x240:r=25,format=rgba[bg];"+
					"[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];"+
					"[f][m]alphamerge,split[p1][p2];"+
					"[p1]palettegen=max_colors=256:reserve_transparent=1:stats_mode=diff[pal];"+
					"[p2][pal]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle:alpha_threshold=128[out]", "-1")...),
		},
		{
			// palettegen rejects max_colors=2 with reserve_transparent=1 (the
			// transparent slot needs room), so the alpha minimum is 3.
			name: "low clamps and RRGGBBAA matte: alpha keeps room for the transparent slot",
			m:    Master{Path: "/dev/shm/ezl/job1/frames.rgba", Width: 320, Height: 240, FPS: 25},
			o:    GIFOptions{Colors: 1, AlphaThreshold: -4, Matte: "AbCdEf80", Dither: "none", HasAlpha: true},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[c][a];"+
					"[a]alphaextract,lut=c0='gte(val,1)*255'[m];"+
					"color=c=0xabcdef:s=320x240:r=25,format=rgba[bg];"+
					"[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];"+
					"[f][m]alphamerge,split[p1][p2];"+
					"[p1]palettegen=max_colors=3:reserve_transparent=1:stats_mode=diff[pal];"+
					"[p2][pal]paletteuse=dither=none:diff_mode=rectangle:alpha_threshold=128[out]", "0")...),
		},
		{
			name: "2 colours with alpha is raised to 3",
			m:    testMaster(),
			o:    GIFOptions{Colors: 2, HasAlpha: true},
			want: append(append([]string{}, raw...), gifTail(
				strings.Replace(gifAlphaFilter, "max_colors=256", "max_colors=3", 1), "0")...),
		},
		{
			name: "3 colours with alpha passes through",
			m:    testMaster(),
			o:    GIFOptions{Colors: 3, HasAlpha: true},
			want: append(append([]string{}, raw...), gifTail(
				strings.Replace(gifAlphaFilter, "max_colors=256", "max_colors=3", 1), "0")...),
		},
		{
			name: "2 colours without alpha is allowed",
			m:    testMaster(),
			o:    GIFOptions{Colors: 2},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[p1][p2];"+
					"[p1]palettegen=max_colors=2:reserve_transparent=0:stats_mode=diff[pal];"+
					"[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle[out]", "0")...),
		},
		{
			name: "loop count above 65535 is clamped",
			m:    testMaster(),
			o:    GIFOptions{Loop: 70000},
			want: append(append([]string{}, raw...), gifTail(
				"[0:v]split[p1][p2];"+
					"[p1]palettegen=max_colors=256:reserve_transparent=0:stats_mode=diff[pal];"+
					"[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle[out]", "65535")...),
		},
		{
			name: "fractional fps is written identically in -r and the colour source",
			m:    Master{Path: "/dev/shm/ezl/job1/frames.rgba", Width: 128, Height: 128, FPS: 100.0 / 3, HasAlpha: true},
			o:    GIFOptions{HasAlpha: true},
			want: append([]string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "128x128", "-r", "33.333333", "-i", "/dev/shm/ezl/job1/frames.rgba"},
				gifTail(strings.NewReplacer("s=320x240:r=25", "s=128x128:r=33.333333").Replace(gifAlphaFilter), "0")...),
		},
		{
			name: "NTSC fps (what a gif plan of a 29.97 source now carries) is written as 29.97",
			m:    Master{Path: "/dev/shm/ezl/job1/frames.rgba", Width: 128, Height: 128, FPS: 29.97, HasAlpha: true},
			o:    GIFOptions{HasAlpha: true},
			want: append([]string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "128x128", "-r", "29.97", "-i", "/dev/shm/ezl/job1/frames.rgba"},
				gifTail(strings.NewReplacer("s=320x240:r=25", "s=128x128:r=29.97").Replace(gifAlphaFilter), "0")...),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, GIFArgs(tc.m, tc.o, "out.gif"), tc.want)
		})
	}
}

func TestGifsicleArgs(t *testing.T) {
	tests := []struct {
		name string
		o    GifsicleOptions
		want []string
	}{
		{
			name: "defaults",
			o:    GifsicleOptions{},
			want: []string{"-O2", "--careful", "--loopcount=forever", "in.gif", "-o", "out.gif"},
		},
		{
			name: "doc example",
			o:    GifsicleOptions{Lossy: 40, Colors: 128},
			want: []string{"-O2", "--careful", "--lossy=40", "--colors", "128", "--loopcount=forever", "in.gif", "-o", "out.gif"},
		},
		{
			name: "everything on, order fixed",
			o:    GifsicleOptions{Lossy: 80, Colors: 64, OptimizeLevel: 3, NoCareful: true, Unoptimize: true, Threads: 4, Dither: "o8"},
			want: []string{"-U", "-O3", "--lossy=80", "--colors", "64", "--dither=o8", "-j4", "--loopcount=forever", "in.gif", "-o", "out.gif"},
		},
		{
			name: "coalesce-only fallback rung",
			o:    GifsicleOptions{Unoptimize: true, OptimizeLevel: 1},
			want: []string{"-U", "-O1", "--careful", "--loopcount=forever", "in.gif", "-o", "out.gif"},
		},
		{
			name: "clamps: level > 3, lossy > 200, colours > 256; dither without colours is dropped",
			o:    GifsicleOptions{OptimizeLevel: 7, Lossy: 500, Colors: 1000, Dither: "o8"},
			want: []string{"-O3", "--careful", "--lossy=200", "--colors", "256", "--dither=o8", "--loopcount=forever", "in.gif", "-o", "out.gif"},
		},
		{
			name: "negative values behave like unset",
			o:    GifsicleOptions{OptimizeLevel: -1, Lossy: -3, Colors: -9, Threads: -2, Loop: -1},
			want: []string{"-O2", "--careful", "--loopcount=forever", "in.gif", "-o", "out.gif"},
		},
		{
			name: "loop N writes --loopcount=N (NETSCAPE count, plays N+1 times)",
			o:    GifsicleOptions{Loop: 3},
			want: []string{"-O2", "--careful", "--loopcount=3", "in.gif", "-o", "out.gif"},
		},
		{
			name: "loop 1 (play twice)",
			o:    GifsicleOptions{Loop: 1, Lossy: 40, Colors: 128},
			want: []string{"-O2", "--careful", "--lossy=40", "--colors", "128", "--loopcount=1", "in.gif", "-o", "out.gif"},
		},
		{
			name: "loop above 65535 is clamped",
			o:    GifsicleOptions{Loop: 100000},
			want: []string{"-O2", "--careful", "--loopcount=65535", "in.gif", "-o", "out.gif"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, GifsicleArgs("in.gif", "out.gif", tc.o), tc.want)
		})
	}
}

func TestWebPArgs(t *testing.T) {
	raw := []string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25", "-i", "/dev/shm/ezl/job1/frames.rgba"}
	tests := []struct {
		name string
		m    Master
		o    WebPOptions
		want []string
	}{
		{
			name: "defaults, lossy with alpha",
			m:    testMaster(),
			o:    WebPOptions{},
			want: append(append([]string{}, raw...),
				"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "80", "-compression_level", "4",
				"-pix_fmt", "yuva420p", "-loop", "0", "-map_metadata", "-1", "-f", "webp", "out.webp"),
		},
		{
			// Loop 5 = NETSCAPE count 5 = 6 plays; the webp ANIM loop count is
			// the number of plays, so -loop 6.
			name: "explicit quality/level/loop, lossy without alpha: loop N becomes N+1 plays",
			m:    Master{Path: "/dev/shm/ezl/job1/frames.rgba", Width: 320, Height: 240, FPS: 25},
			o:    WebPOptions{Quality: 60, CompressionLevel: 2, Loop: 5},
			want: append(append([]string{}, raw...),
				"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "60", "-compression_level", "2",
				"-pix_fmt", "yuv420p", "-loop", "6", "-map_metadata", "-1", "-f", "webp", "out.webp"),
		},
		{
			name: "lossless: bgra, no -q:v; loop 2 = 3 plays",
			m:    testMaster(),
			o:    WebPOptions{Lossless: true, Quality: 90, CompressionLevel: 6, Loop: 2},
			want: append(append([]string{}, raw...),
				"-c:v", "libwebp_anim", "-lossless", "1", "-compression_level", "6",
				"-pix_fmt", "bgra", "-loop", "3", "-map_metadata", "-1", "-f", "webp", "out.webp"),
		},
		{
			name: "loop 1 (play twice) is -loop 2",
			m:    testMaster(),
			o:    WebPOptions{Loop: 1},
			want: append(append([]string{}, raw...),
				"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "80", "-compression_level", "4",
				"-pix_fmt", "yuva420p", "-loop", "2", "-map_metadata", "-1", "-f", "webp", "out.webp"),
		},
		{
			name: "clamps: quality > 100, level > 6, negative loop stays infinite",
			m:    testMaster(),
			o:    WebPOptions{Quality: 150, CompressionLevel: 9, Loop: -1},
			want: append(append([]string{}, raw...),
				"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "100", "-compression_level", "6",
				"-pix_fmt", "yuva420p", "-loop", "0", "-map_metadata", "-1", "-f", "webp", "out.webp"),
		},
		{
			name: "loop count is clamped to the ANIM chunk's uint16",
			m:    testMaster(),
			o:    WebPOptions{Loop: 100000},
			want: append(append([]string{}, raw...),
				"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "80", "-compression_level", "4",
				"-pix_fmt", "yuva420p", "-loop", "65535", "-map_metadata", "-1", "-f", "webp", "out.webp"),
		},
		{
			name: "single-frame master: still libwebp encoder, no -loop",
			m:    Master{Path: "/dev/shm/ezl/job1/frames.rgba", Width: 320, Height: 240, FPS: 25, Frames: 1, HasAlpha: true},
			o:    WebPOptions{Quality: 90},
			want: append(append([]string{}, raw...),
				"-frames:v", "1", "-c:v", "libwebp", "-lossless", "0", "-q:v", "90", "-compression_level", "4",
				"-pix_fmt", "yuva420p", "-map_metadata", "-1", "-f", "webp", "out.webp"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, WebPArgs(tc.m, tc.o, "out.webp"), tc.want)
		})
	}
}

func TestSimpleBuilders(t *testing.T) {
	assertArgs(t, VerifyDecodeArgs("out.gif"), []string{"-i", "out.gif", "-f", "null", "-"})
	assertArgs(t, FrameCountArgs("out.webp"), []string{"-i", "out.webp", "-map", "0:v:0", "-f", "null", "-"})
	assertArgs(t, ProbeArgs("/data/blobs/ab/abcd.mov"),
		[]string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "/data/blobs/ab/abcd.mov"})
	assertArgs(t, AlphaScanArgs("in.gif", 0),
		[]string{"-i", "in.gif", "-frames:v", "60", "-vf", "format=rgba,alphaextract", "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1"})
	assertArgs(t, AlphaScanArgs("in.gif", 8),
		[]string{"-i", "in.gif", "-frames:v", "8", "-vf", "format=rgba,alphaextract", "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1"})
}

func TestFormatFloat(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{25, "25"},
		{12.5, "12.5"},
		{100.0 / 3, "33.333333"},
		{0.1 + 0.2, "0.3"},
		{2.000000004, "2"},
		{0, "0"},
		{-0.0000001, "0"},
		{29.97, "29.97"},
	}
	for _, tc := range tests {
		if got := formatFloat(tc.in); got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripSeekArgs(t *testing.T) {
	tests := []struct {
		in, want []string
	}{
		{nil, []string{}},
		{[]string{"-ss", "1", "-to", "2"}, []string{}},
		{[]string{"-c:v", "libvpx-vp9", "-ss", "1"}, []string{"-c:v", "libvpx-vp9"}},
		{[]string{"-ss", "1", "-t", "3", "-sseof", "-2", "-stream_loop", "-1"}, []string{"-stream_loop", "-1"}},
		{[]string{"-ss"}, []string{}}, // dangling flag never panics
	}
	for _, tc := range tests {
		if got := stripSeekArgs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("stripSeekArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFilterQuoting guards the single-quote shape ffmpeg needs so that
// min(iw,N) inside a filter option is parsed as an expression and not
// split at the comma; a shell is never involved so no other quoting exists.
func TestFilterQuoting(t *testing.T) {
	args := StillArgs("src.mov", testPlan(), 0, 480)
	filter := args[indexOf(args, "-filter_complex")+1]
	if !strings.Contains(filter, "scale=w='min(iw,480)':h=-1") {
		t.Fatalf("still filter lost its quoting: %s", filter)
	}
	if !strings.Contains(filter, ",select='gte(t,0)',") {
		t.Fatalf("still select expression lost its quoting: %s", filter)
	}
	if strings.ContainsAny(filter, "\"") {
		t.Fatalf("still filter must not contain double quotes: %s", filter)
	}
	// The alpha still scales premultiplied and hands rgba to the encoder.
	if !strings.Contains(filter, "premultiply=inplace=1,scale=") {
		t.Fatalf("alpha still must scale premultiplied: %s", filter)
	}
	if !strings.HasSuffix(filter, "unpremultiply=inplace=1,format=rgba[outs]") {
		t.Fatalf("alpha still must end in unpremultiply,format=rgba[outs]: %s", filter)
	}
	// Every still filter maps [outs] and puts tpad/select after the plan's
	// own output pad.
	if !strings.Contains(filter, "[out];[out]tpad=stop_mode=clone:stop_duration=") || args[indexOf(args, "-map")+1] != "[outs]" {
		t.Fatalf("still filter shape: %s (map %s)", filter, args[indexOf(args, "-map")+1])
	}
	gif := GIFArgs(testMaster(), GIFOptions{HasAlpha: true}, "o.gif")
	filter = gif[indexOf(gif, "-filter_complex")+1]
	if !strings.Contains(filter, "lut=c0='gte(val,128)*255'") {
		t.Fatalf("gif filter lost its quoting: %s", filter)
	}
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}
