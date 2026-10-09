package enc

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// Phase 5b goldens: the per-consumer forms of a matte input (spec §5.5's
// table) in every builder, the producer argv (MatteSourceArgs), and the
// nil-without-Path contract. Every plan without a matte input keeps its
// existing goldens byte-identical (enc_test.go, phase3_test.go,
// phase4_test.go run unchanged).

const (
	matteDir = "/data/mattes/9df7bf02b4738b9d"
	matteSeq = matteDir + "/%06d.png"
	// matteFilter is testPlan's chain with an opaque-frame merge of input 1
	// (the compiler's shape; the builders pass it through untouched).
	matteFilter = "[0:v]fps=25,format=rgba[m1];[1:v]format=gray,scale=320:240:flags=bicubic[m1a];[m1][m1a]alphamerge,format=rgba[out]"
	// matteRevFilter / matteBounceFilter: the merge precedes the reverse /
	// bounce stages.
	matteRevFilter    = "[0:v]fps=25,format=rgba[m1];[1:v]format=gray,scale=320:240:flags=bicubic[m1a];[m1][m1a]alphamerge,format=rgba,reverse[out]"
	matteBounceFilter = "[0:v]fps=25,format=rgba[m1];[1:v]format=gray,scale=320:240:flags=bicubic[m1a];[m1][m1a]alphamerge,format=rgba,split[bf][br];[br]reverse[brr];[bf][brr]concat=n=2:v=1:a=0[out]"
)

// compilerMatteArgs is what the graph compiler puts in a matte input's Args.
var compilerMatteArgs = []string{"-f", "image2", "-framerate", "25", "-start_number", "1"}

// matteInput is a resolved matte input as jobs hands it to enc: Path filled,
// Frames from the memo's manifest (0 = unknown).
func matteInput(frames int) graph.ExtraInput {
	return graph.ExtraInput{
		Source: 0,
		Path:   matteSeq,
		Args:   append([]string(nil), compilerMatteArgs...),
		Matte:  &graph.MatteInput{Model: "isnet-anime", Size: 1024, FPS: "25", Frames: frames},
	}
}

// withMatte adds a resolved matte input to p and gives it filter.
func withMatte(p *graph.Plan, filter string, frames int) *graph.Plan {
	p.Filter = filter
	p.ExtraInputs = append(p.ExtraInputs, matteInput(frames))
	return p
}

// matteSeqArgs is the sequence form from file start.
func matteSeqArgs(start int) []string {
	return []string{"-f", "image2", "-framerate", "25", "-start_number", strconv.Itoa(start), "-i", matteSeq}
}

// matteOneArgs is the forward still's single unlooped PNG, file n.
func matteOneArgs(n int) []string {
	return []string{"-f", "image2", "-framerate", "25", "-i", matteDir + "/" + fmt.Sprintf("%06d.png", n)}
}

// noLoopBeforeFilter fails when "-loop" appears among the input options (the
// webp muxer's own "-loop 0" output option of ProxyArgs comes after
// -filter_complex and does not count).
func noLoopBeforeFilter(t *testing.T, args []string) {
	t.Helper()
	end := indexOf(args, "-filter_complex")
	if end < 0 {
		end = len(args)
	}
	for _, a := range args[:end] {
		if a == "-loop" {
			t.Errorf("matte input must never loop: %q", args)
			return
		}
	}
}

func TestMasterArgs_MatteInput(t *testing.T) {
	for _, frames := range []int{62, 0} {
		got := MasterArgs(mainSrc, withMatte(testPlan(), matteFilter, frames), "frames.rgba")
		want := cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, matteSeqArgs(1), []string{
			"-filter_complex", matteFilter,
			"-map", "[out]",
			"-an", "-sn", "-dn",
			"-f", "rawvideo", "-pix_fmt", "rgba",
			"frames.rgba",
		})
		assertArgs(t, got, want)
		noLoopBeforeFilter(t, got)
	}
}

func TestCropDetectPlanArgs_MatteInput(t *testing.T) {
	// The detection plan carries the matte input at the detection plan's own
	// fps (jobs fills the Path before this builder runs): the full sequence,
	// like the master.
	p := withMatte(detectPlan([]string{"-ss", "1.5", "-to", "4"}, matteFilter, true), matteFilter, 62)
	got := CropDetectPlanArgs(mainSrc, p, true, 128)
	want := cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, matteSeqArgs(1), []string{
		"-filter_complex", matteFilter + ";[out]" + cropSampleStage + alphaDetector + "127[det]",
		"-map", "[det]", "-an", "-f", "null", "-",
	})
	assertArgs(t, got, want)
}

// TestStillArgs_MatteSinglePNG: a forward still reads ONE unlooped PNG, the
// file of the absolute output slot it selects (the same slot the seek
// maths choose: capped at the render's last slot), clamped to the memo's
// frame count when known — whatever the seek-back, from-start variant or
// seek-unsafe source.
func TestStillArgs_MatteSinglePNG(t *testing.T) {
	tests := []struct {
		name      string
		plan      func() *graph.Plan
		frames    int
		t         float64
		fromStart bool
		lead      []string // argv before "-i"
		file      int      // the PNG's 1-based number
		filter    string
	}{
		{
			// t=1 → slot 25 (seek 2.34 = slot 21, offset 0.84; see TestStillArgs).
			name: "t=1 reads the 26th matte",
			plan: testPlan, frames: 62, t: 1,
			lead: []string{"-ss", "2.34", "-itsoffset", "0.84"}, file: 26,
			filter: stillFilter(matteFilter, "1.16", "0.98", alphaScale480),
		},
		{
			name: "the slot is clamped to the memo's last frame",
			plan: testPlan, frames: 20, t: 1,
			lead: []string{"-ss", "2.34", "-itsoffset", "0.84"}, file: 20,
			filter: stillFilter(matteFilter, "1.16", "0.98", alphaScale480),
		},
		{
			name: "a one-frame memo always reads its only file",
			plan: testPlan, frames: 1, t: 1,
			lead: []string{"-ss", "2.34", "-itsoffset", "0.84"}, file: 1,
			filter: stillFilter(matteFilter, "1.16", "0.98", alphaScale480),
		},
		{
			name: "an unknown count leaves the slot unclamped",
			plan: testPlan, frames: 0, t: 1,
			lead: []string{"-ss", "2.34", "-itsoffset", "0.84"}, file: 26,
			filter: stillFilter(matteFilter, "1.16", "0.98", alphaScale480),
		},
		{
			name: "t=0 reads the first matte",
			plan: testPlan, frames: 62, t: 0,
			lead: []string{"-ss", "1.5"}, file: 1,
			filter: stillFilter(matteFilter, "1", "0", alphaScale480),
		},
		{
			// Slot 62 capped at the render's last slot 61 → file 62.
			name: "t past the end reads the last slot's matte",
			plan: testPlan, frames: 62, t: 99,
			lead: []string{"-ss", "3.82", "-itsoffset", "2.32"}, file: 62,
			filter: stillFilter(matteFilter, "1.12", "2.42", alphaScale480),
		},
		{
			// Plan.Frames (62) overshoots the decoded count (61) by one: the
			// capped slot 61 would name a missing file; the clamp reads the
			// last matte for the held last frame instead.
			name: "a one-frame estimate overshoot is clamped to the memo",
			plan: testPlan, frames: 61, t: 99,
			lead: []string{"-ss", "3.82", "-itsoffset", "2.32"}, file: 61,
			filter: stillFilter(matteFilter, "1.12", "2.42", alphaScale480),
		},
		{
			// Speed 2: the slot is an OUTPUT slot (25), not a source one.
			name: "speed 2 keeps the output slot",
			plan: func() *graph.Plan { p := testPlan(); p.Speed = 2; return p }, frames: 62, t: 1,
			lead: []string{"-ss", "3.26", "-itsoffset", "1.76"}, file: 26,
			filter: stillFilter(matteFilter, "1.12", "0.98", alphaScale480),
		},
		{
			name: "from start reads the same single PNG",
			plan: testPlan, frames: 62, t: 1, fromStart: true,
			lead: []string{"-ss", "1.5"}, file: 26,
			filter: stillFilter(matteFilter, "2", "0.98", alphaScale480),
		},
		{
			// An untrimmed animated WebP (SeekUnsafe): no seek at all, slot 25.
			name: "seek-unsafe forward plan: unseeked, one PNG",
			plan: seekUnsafePlan, frames: 100, t: 1,
			lead: nil, file: 26,
			filter: stillFilter(matteFilter, "2", "0.98", alphaScale480),
		},
		{
			name: "seek-unsafe from start is the same argv",
			plan: seekUnsafePlan, frames: 100, t: 1, fromStart: true,
			lead: nil, file: 26,
			filter: stillFilter(matteFilter, "2", "0.98", alphaScale480),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := withMatte(tc.plan(), matteFilter, tc.frames)
			var got []string
			if tc.fromStart {
				got = StillArgsFromStart(mainSrc, p, tc.t, 480)
			} else {
				got = StillArgs(mainSrc, p, tc.t, 480)
			}
			want := cat(tc.lead, []string{"-i", mainSrc}, matteOneArgs(tc.file), stillOut(tc.filter))
			assertArgs(t, got, want)
			noLoopBeforeFilter(t, got)
		})
	}
}

// TestStillArgs_MatteReversed: a reversed plan reads the sequence — from
// "-start_number K+1" when its CFR tail seek skipped K output slots (the
// main's frames after the seek carry timestamps from 0), from frame 1 when
// the decode starts at TrimStart (VFR source, filter trim, from-start, an
// unknown last slot).
func TestStillArgs_MatteReversed(t *testing.T) {
	long := func() *graph.Plan { return longReversedPlan() }
	long30 := func() *graph.Plan { p := longReversedPlan(); p.SourceFPS = 30; return p }
	tests := []struct {
		name      string
		plan      func() *graph.Plan
		frames    int
		t         float64
		fromStart bool
		lead      []string
		start     int // the sequence's -start_number
		index     int
		pad       string
	}{
		{
			// j=250 → source slot 499; the seek-back lands on slot 496, which
			// a 30 fps source at 25 fps snaps down to the aligned 495 (5 slots
			// = 6 frames): -ss 21.3, mattes from 496.
			name: "30 fps source at 25 fps: the aligned slot count, not the time",
			plan: long30, frames: 750, t: 10,
			lead: []string{"-ss", "21.3", "-to", "31.5"}, start: 496, index: 250, pad: "11",
		},
		{
			name: "25 fps source: every slot is aligned, K = 496",
			plan: long, frames: 750, t: 10,
			lead: []string{"-ss", "21.34", "-to", "31.5"}, start: 497, index: 250, pad: "11",
		},
		{
			// testPlan reversed, t=1: seek 2.82 = slot 33 (see TestStillArgs_Reversed).
			name: "short clip: K = 33",
			plan: reversedPlan, frames: 62, t: 1,
			lead: []string{"-ss", "2.82", "-to", "4"}, start: 34, index: 25, pad: "2",
		},
		{
			name: "the seek-back reaching TrimStart reads from frame 1",
			plan: reversedPlan, frames: 62, t: 2.5,
			lead: []string{"-ss", "1.5", "-to", "4"}, start: 1, index: 61, pad: "3.44",
		},
		{
			name: "from start reads from frame 1",
			plan: reversedPlan, frames: 62, t: 1, fromStart: true,
			lead: []string{"-ss", "1.5", "-to", "4"}, start: 1, index: 25, pad: "2",
		},
		{
			name: "an animation source (VFR) is decoded from TrimStart: frame 1",
			plan: func() *graph.Plan { p := long30(); p.SourceVFR = true; return p }, frames: 750, t: 10,
			lead: []string{"-ss", "1.5", "-to", "31.5"}, start: 1, index: 250, pad: "11",
		},
		{
			name: "a filter-trimmed plan is never seeked: frame 1",
			plan: func() *graph.Plan { p := filterTrimPlan(); p.Reversed = true; return p }, frames: 62, t: 1,
			lead: nil, start: 1, index: 25, pad: "2",
		},
		{
			// No duration, frame count or trim end: the last slot is unknown,
			// the decode starts at TrimStart.
			name: "unknown last slot: frame 1",
			plan: func() *graph.Plan {
				p := reversedPlan()
				p.InputArgs = []string{"-ss", "1.5"}
				p.Duration, p.Frames, p.TrimEnd = 0, 0, 0
				return p
			}, frames: 0, t: 1,
			lead: []string{"-ss", "1.5"}, start: 1, index: 25, pad: "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := withMatte(tc.plan(), matteRevFilter, tc.frames)
			var got []string
			if tc.fromStart {
				got = StillArgsFromStart(mainSrc, p, tc.t, 0)
			} else {
				got = StillArgs(mainSrc, p, tc.t, 0)
			}
			want := cat(tc.lead, []string{"-i", mainSrc}, matteSeqArgs(tc.start), stillOut(reversedStillFilter(matteRevFilter, tc.pad, tc.index, "")))
			assertArgs(t, got, want)
			noLoopBeforeFilter(t, got)
		})
	}
}

// TestStillArgs_MatteBounced: a bounced plan reads the full sequence,
// forward (even for a slot in the mirrored half — the merge precedes the
// bounce, so the mirrored frames carry their own mattes) and reversed, by
// either variant.
func TestStillArgs_MatteBounced(t *testing.T) {
	trimSeek := []string{"-ss", "1.5", "-to", "4", "-i", mainSrc}
	tests := []struct {
		name      string
		reversed  bool
		t         float64
		fromStart bool
		filter    string
	}{
		{name: "forward t=1", t: 1, filter: stillFilter(matteBounceFilter, "2", "0.98", "")},
		{name: "forward t in the mirrored half (slot 75)", t: 3, filter: stillFilter(matteBounceFilter, "4", "2.98", "")},
		{name: "forward from start", t: 1, fromStart: true, filter: stillFilter(matteBounceFilter, "2", "0.98", "")},
		{name: "reversed t=1", reversed: true, t: 1, filter: reversedStillFilter(matteBounceFilter, "2", 25, "")},
		{name: "reversed from start", reversed: true, t: 1, fromStart: true, filter: reversedStillFilter(matteBounceFilter, "2", 25, "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := withMatte(bouncedPlan(), matteBounceFilter, 62) // the memo holds the pre-bounce frames
			p.Reversed = tc.reversed
			var got []string
			if tc.fromStart {
				got = StillArgsFromStart(mainSrc, p, tc.t, 0)
			} else {
				got = StillArgs(mainSrc, p, tc.t, 0)
			}
			want := cat(trimSeek, matteSeqArgs(1), stillOut(tc.filter))
			assertArgs(t, got, want)
		})
	}
}

// TestProxyArgs_Matte: the proxy reads the full sequence — forward, bounced,
// seek-unsafe or a reversed tail that reaches TrimStart — and from
// "-start_number K+1" when its reversed tail seek skips K slots (aligned
// for CFR sources, the plain grid-snapped K for seekable animations).
func TestProxyArgs_Matte(t *testing.T) {
	proxyTail := func(filter string) []string {
		return []string{
			"-filter_complex", filter + ";[out]fps=15," + alphaScale360 + "[outp]",
			"-map", "[outp]",
			"-an", "-sn", "-dn",
			"-t", "10",
			"-c:v", "libwebp_anim",
			"-lossless", "0",
			"-q:v", "60",
			"-compression_level", "0",
			"-pix_fmt", "yuva420p",
			"-loop", "0",
			"-map_metadata", "-1",
			"-f", "webp",
			"p.webp",
		}
	}
	tests := []struct {
		name   string
		plan   func() *graph.Plan
		filter string
		frames int
		lead   []string
		start  int
	}{
		{
			name: "forward: the full sequence",
			plan: testPlan, filter: matteFilter, frames: 62,
			lead: []string{"-ss", "1.5", "-to", "4"}, start: 1,
		},
		{
			// 5 output slots are 6 source frames: slot 496 snaps to 495.
			name: "reversed CFR tail, 30 fps source at 25 fps: K = 495",
			plan: func() *graph.Plan { p := longReversedPlan(); p.SourceFPS = 30; return p }, filter: matteRevFilter, frames: 750,
			lead: []string{"-ss", "21.3", "-to", "31.5"}, start: 496,
		},
		{
			name: "reversed CFR tail, 25 fps source: K = 496",
			plan: longReversedPlan, filter: matteRevFilter, frames: 750,
			lead: []string{"-ss", "21.34", "-to", "31.5"}, start: 497,
		},
		{
			// A seekable animation gets the plain grid-snapped seek (no
			// source-frame alignment): K = 496 even at a 30 fps base cadence.
			name: "reversed VFR tail: the plain slot count",
			plan: func() *graph.Plan { p := longReversedPlan(); p.SourceFPS, p.SourceVFR = 30, true; return p }, filter: matteRevFilter, frames: 750,
			lead: []string{"-ss", "21.34", "-to", "31.5"}, start: 497,
		},
		{
			name: "a reversed tail reaching TrimStart: the full sequence",
			plan: reversedPlan, filter: matteRevFilter, frames: 62,
			lead: []string{"-ss", "1.5", "-to", "4"}, start: 1,
		},
		{
			name: "bounced forward: the full sequence",
			plan: bouncedPlan, filter: matteBounceFilter, frames: 62,
			lead: []string{"-ss", "1.5", "-to", "4"}, start: 1,
		},
		{
			name: "bounced reversed: the full sequence",
			plan: func() *graph.Plan { p := bouncedPlan(); p.Reversed = true; return p }, filter: matteBounceFilter, frames: 62,
			lead: []string{"-ss", "1.5", "-to", "4"}, start: 1,
		},
		{
			name: "seek-unsafe reversed (animated WebP): unseeked, the full sequence",
			plan: func() *graph.Plan { p := seekUnsafePlan(); p.Reversed = true; return p }, filter: matteRevFilter, frames: 100,
			lead: nil, start: 1,
		},
		{
			name: "seek-unsafe forward: the full sequence",
			plan: seekUnsafePlan, filter: matteFilter, frames: 100,
			lead: nil, start: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := withMatte(tc.plan(), tc.filter, tc.frames)
			got := ProxyArgs(mainSrc, p, 0, 10, "p.webp")
			want := cat(tc.lead, []string{"-i", mainSrc}, matteSeqArgs(tc.start), proxyTail(tc.filter))
			assertArgs(t, got, want)
			noLoopBeforeFilter(t, got)
		})
	}
}

// TestMatteSlotIsStoredNotDerived: the slot count a reversed tail seek was
// snapped to is the same value for the proxy and the still at the same
// output time, equals the seek start on the grid, and is the aligned count
// (a multiple of the 5-slot period of a 30 fps source at 25 fps).
func TestMatteSlotIsStoredNotDerived(t *testing.T) {
	for _, srcFPS := range []float64{25, 30, 60, 24} {
		p := longReversedPlan()
		p.SourceFPS = srcFPS
		for _, tt := range []float64{10, 2.5, 7.3} {
			s, ok := proxySeekFor(p, tt)
			still := reversedSeekFor(p, tt, false)
			if !ok {
				t.Fatalf("srcFPS %v t=%v: no proxy seek", srcFPS, tt)
			}
			if s.slot != still.slot || s.start != still.start {
				t.Errorf("srcFPS %v t=%v: proxy (start %v, K %d) vs still (start %v, K %d)", srcFPS, tt, s.start, s.slot, still.start, still.slot)
			}
			if got := p.TrimStart + float64(s.slot)*p.Speed/p.FPS; math.Abs(got-s.start) > 1e-9 {
				t.Errorf("srcFPS %v t=%v: K %d is not on the grid at start %v (%v)", srcFPS, tt, s.slot, s.start, got)
			}
			if frames := float64(s.slot) * srcFPS / p.FPS; math.Abs(frames-math.Round(frames)) > 1e-6 {
				t.Errorf("srcFPS %v t=%v: K %d is not a whole number of source frames (%v)", srcFPS, tt, s.slot, frames)
			}
		}
	}
	// Forward stills store the absolute slot they select.
	for _, tc := range []struct {
		t    float64
		slot int
	}{{0, 0}, {1, 25}, {2.49, 62 - 1}, {99, 61}} {
		if s := stillSeekFor(testPlan(), tc.t, false); s.slot != tc.slot {
			t.Errorf("t=%v: slot %d, want %d", tc.t, s.slot, tc.slot)
		}
		if s := stillSeekFor(testPlan(), tc.t, true); s.slot != tc.slot {
			t.Errorf("t=%v from start: slot %d, want %d", tc.t, s.slot, tc.slot)
		}
	}
}

func TestMatteSourceArgs(t *testing.T) {
	const planFilter = "[0:v]fps=25,format=rgba[out]"
	tail := func(filter string, size string) []string {
		return []string{
			"-filter_complex", filter + ";[out]format=rgb24,scale=" + size + ":" + size + ":flags=bicubic[mi]",
			"-map", "[mi]",
			"-an", "-sn", "-dn",
			"-f", "rawvideo", "-pix_fmt", "rgb24",
			"pipe:1",
		}
	}
	t.Run("video plan", func(t *testing.T) {
		p := testPlan()
		p.Filter = planFilter
		got := MatteSourceArgs(mainSrc, p, 1024)
		want := cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, tail(planFilter, "1024"))
		assertArgs(t, got, want)
	})
	t.Run("image sequence plan joins the pattern", func(t *testing.T) {
		p := testPlan()
		p.Filter = planFilter
		p.InputArgs = []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}
		p.InputPattern = "%06d.png"
		got := MatteSourceArgs("/data/blobs/ab/abcd", p, 512)
		want := cat(p.InputArgs, []string{"-i", "/data/blobs/ab/abcd/%06d.png"}, tail(planFilter, "512"))
		assertArgs(t, got, want)
	})
	t.Run("default out label", func(t *testing.T) {
		p := &graph.Plan{Filter: "[0:v]format=rgba[out]"}
		got := MatteSourceArgs("in.gif", p, 320)
		want := cat([]string{"-i", "in.gif"}, tail("[0:v]format=rgba[out]", "320"))
		assertArgs(t, got, want)
	})
	t.Run("nil, unusable plan or no size yields nil", func(t *testing.T) {
		if got := MatteSourceArgs(mainSrc, nil, 1024); got != nil {
			t.Errorf("nil plan: %q", got)
		}
		for _, size := range []int{0, -1} {
			if got := MatteSourceArgs(mainSrc, testPlan(), size); got != nil {
				t.Errorf("size %d: %q", size, got)
			}
		}
		unbound := testPlan()
		unbound.Filter = "[0:v]fps=25,format=rgba,drawtext=fontfile=/f.ttf:textfile=__EZLG_TEXT_0:fontsize=32[out]"
		if got := MatteSourceArgs(mainSrc, unbound, 1024); got != nil {
			t.Errorf("unbound placeholder: %q", got)
		}
	})
}

// TestBuildersRejectMatteInputWithoutPath: until jobs resolved the memo a
// matte input has no Path and every builder yields nil, like for an
// overlay without one.
func TestBuildersRejectMatteInputWithoutPath(t *testing.T) {
	p := withMatte(testPlan(), matteFilter, 62)
	p.ExtraInputs[0].Path = ""
	all := [][]string{
		MasterArgs(mainSrc, p, "f.rgba"),
		StillArgs(mainSrc, p, 1, 0),
		StillArgsFromStart(mainSrc, p, 1, 0),
		ProxyArgs(mainSrc, p, 0, 0, "p.webp"),
		CropDetectPlanArgs(mainSrc, p, true, 128),
		MatteSourceArgs(mainSrc, p, 1024),
	}
	for i, args := range all {
		if args != nil {
			t.Errorf("builder %d returned %q, want nil", i, args)
		}
	}
}

// TestExtraInputs_MatteBeforeOverlays: the compiler registers the matte
// input in keying, before the overlays of finalCanvas; the builders emit
// the inputs in that order (ffmpeg input index = position + 1) with each
// in its own form.
func TestExtraInputs_MatteBeforeOverlays(t *testing.T) {
	plan := func() *graph.Plan {
		p := overlayPlan()
		p.ExtraInputs = append([]graph.ExtraInput{matteInput(62)}, p.ExtraInputs...)
		return p
	}
	// filter text is irrelevant to the input order; overlayPlan's is kept.
	master := MasterArgs(mainSrc, plan(), "frames.rgba")
	assertArgs(t, master, cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, matteSeqArgs(1), extraArgs, []string{
		"-filter_complex", ovFilter,
		"-map", "[out]",
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgba",
		"frames.rgba",
	}))
	still := StillArgs(mainSrc, plan(), 1, 480)
	assertArgs(t, still, cat([]string{"-ss", "2.34", "-itsoffset", "0.84", "-i", mainSrc}, matteOneArgs(26), extraArgs, stillOut(stillFilter(ovFilter, "1.16", "0.98", alphaScale480))))
}

func TestMatteInputArgsStripsLoopAndStartNumber(t *testing.T) {
	// Whatever a hand-built plan puts in Args, a matte input never loops
	// and its -start_number is the consumer's.
	e := matteInput(10)
	e.Args = []string{"-f", "image2", "-loop", "1", "-framerate", "25", "-start_number", "9"}
	assertArgs(t, matteInputArgs(e, matteArgs{}), matteSeqArgs(1))
	assertArgs(t, matteInputArgs(e, matteArgs{slot: 4}), matteSeqArgs(5))
	assertArgs(t, matteInputArgs(e, matteArgs{single: true, slot: 4}), matteOneArgs(5))
	// Clamps: below 0, above Frames-1; Frames 0 leaves it.
	assertArgs(t, matteInputArgs(e, matteArgs{single: true, slot: -3}), matteOneArgs(1))
	assertArgs(t, matteInputArgs(e, matteArgs{single: true, slot: 40}), matteOneArgs(10))
	assertArgs(t, matteInputArgs(e, matteArgs{slot: 40}), matteSeqArgs(10))
	e.Matte.Frames = 0
	assertArgs(t, matteInputArgs(e, matteArgs{single: true, slot: 40}), matteOneArgs(41))
	// No Args at all: just the form.
	e.Args = nil
	assertArgs(t, matteInputArgs(e, matteArgs{}), []string{"-start_number", "1", "-i", matteSeq})
}

func TestMatteFramePath(t *testing.T) {
	tests := []struct {
		path string
		n    int
		want string
	}{
		{"/data/mattes/k/%06d.png", 26, "/data/mattes/k/000026.png"},
		{`C:\data\mattes\k\%06d.png`, 7, `C:\data\mattes\k\000007.png`},
		{"%06d.png", 1, "000001.png"},
		{"/x/%d.png", 123, "/x/123.png"},
		{"/x/frame.png", 7, "/x/frame.png"}, // already a single file
	}
	for _, tc := range tests {
		if got := matteFramePath(tc.path, tc.n); got != tc.want {
			t.Errorf("matteFramePath(%q, %d) = %q, want %q", tc.path, tc.n, got, tc.want)
		}
	}
}
