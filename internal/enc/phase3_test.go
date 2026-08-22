package enc

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// Phase 3 goldens: multi-input master/still/proxy argv, reversed-plan seeks,
// the autocrop detection argv and its parser, fc-list argv and its parser,
// and the nil-on-unusable-plan contract.

const (
	mainSrc  = "/data/blobs/ab/abcd"
	gifOv    = "/data/blobs/cd/cdef"
	pngOv    = "/data/blobs/ef/ef01"
	ovFilter = "[0:v]fps=25,format=rgba[b];[1:v]format=rgba,fps=25[o];[b][o]overlay=x=10:y=20:format=auto:shortest=1,format=rgba[c];[2:v]format=rgba[s];[c][s]overlay=x=0:y=0:format=auto:shortest=1:enable='between(t,1,2)',format=rgba[out]"
)

// overlayPlan is testPlan with a looping animated overlay (input 1) and a
// still-image overlay (input 2), as the Phase 3 compiler would emit them.
func overlayPlan() *graph.Plan {
	p := testPlan()
	p.Filter = ovFilter
	p.ExtraInputs = []graph.ExtraInput{
		{Source: 1, Path: gifOv, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 1.2, Loop: true},
		{Source: 2, Path: pngOv, Args: []string{"-loop", "1"}},
	}
	return p
}

// extraArgs is what every builder emits for overlayPlan's inputs.
var extraArgs = []string{"-stream_loop", "-1", "-i", gifOv, "-loop", "1", "-i", pngOv}

// stillOut is the tail every still argv ends with.
func stillOut(filter string) []string {
	return []string{
		"-frames:v", "1",
		"-filter_complex", filter,
		"-map", "[outs]",
		"-c:v", "png", "-compression_level", "1",
		"-f", "image2pipe", "pipe:1",
	}
}

// cat concatenates argv fragments.
func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestMasterArgs_ExtraInputs(t *testing.T) {
	got := MasterArgs(mainSrc, overlayPlan(), "frames.rgba")
	want := cat(
		[]string{"-ss", "1.5", "-to", "4", "-i", mainSrc},
		extraArgs,
		[]string{
			"-filter_complex", ovFilter,
			"-map", "[out]",
			"-an", "-sn", "-dn",
			"-f", "rawvideo", "-pix_fmt", "rgba",
			"frames.rgba",
		},
	)
	assertArgs(t, got, want)

	// An overlay without input options is just "-i path"; a sequence main
	// input still gets its pattern.
	p := overlayPlan()
	p.ExtraInputs = []graph.ExtraInput{{Source: 1, Path: gifOv, Animated: true, Duration: 1.2}}
	p.InputArgs = []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}
	p.InputPattern = "%06d.png"
	got = MasterArgs("/data/blobs/ab/abcd", p, "frames.rgba")
	if !reflect.DeepEqual(got[:13], []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0", "-i", "/data/blobs/ab/abcd/%06d.png", "-i", gifOv, "-filter_complex"}) {
		t.Errorf("sequence main + bare overlay: %q", got[:13])
	}
}

func TestStillArgs_ExtraInputs(t *testing.T) {
	// The overlay inputs are never seeked: the main input's -itsoffset keeps
	// its timestamps absolute, so the overlays' own clocks (starting at 0,
	// exactly as in the render) already read t at the selected frame.
	t.Run("seek", func(t *testing.T) {
		got := StillArgs(mainSrc, overlayPlan(), 1, 480)
		want := cat(
			[]string{"-ss", "2.34", "-itsoffset", "0.84", "-i", mainSrc},
			extraArgs,
			stillOut(stillFilter(ovFilter, "1.16", "0.98", alphaScale480)),
		)
		assertArgs(t, got, want)
	})
	t.Run("t=0", func(t *testing.T) {
		got := StillArgs(mainSrc, overlayPlan(), 0, 0)
		want := cat([]string{"-ss", "1.5", "-i", mainSrc}, extraArgs, stillOut(stillFilter(ovFilter, "1", "0", "")))
		assertArgs(t, got, want)
	})
	t.Run("from start", func(t *testing.T) {
		got := StillArgsFromStart(mainSrc, overlayPlan(), 1, 480)
		want := cat([]string{"-ss", "1.5", "-i", mainSrc}, extraArgs, stillOut(stillFilter(ovFilter, "2", "0.98", alphaScale480)))
		assertArgs(t, got, want)
	})
}

func TestProxyArgs_ExtraInputs(t *testing.T) {
	got := ProxyArgs(mainSrc, overlayPlan(), 0, 0, "proxy.webp")
	want := cat(
		[]string{"-ss", "1.5", "-to", "4", "-i", mainSrc},
		extraArgs,
		[]string{
			"-filter_complex", ovFilter + ";[out]fps=15," + alphaScale360 + "[outp]",
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
			"proxy.webp",
		},
	)
	assertArgs(t, got, want)
}

// TestBuildersRejectUnusablePlans: an unbound drawtext placeholder or an
// extra input without a path is a programming error every builder reports
// as nil; a bound plan (TextFiles still listed, no placeholder left) is fine.
func TestBuildersRejectUnusablePlans(t *testing.T) {
	all := func(p *graph.Plan) [][]string {
		return [][]string{
			MasterArgs(mainSrc, p, "f.rgba"),
			StillArgs(mainSrc, p, 1, 0),
			StillArgsFromStart(mainSrc, p, 1, 0),
			ProxyArgs(mainSrc, p, 0, 0, "p.webp"),
		}
	}
	unbound := testPlan()
	unbound.Filter = "[0:v]fps=25,format=rgba,drawtext=fontfile=/f.ttf:textfile=__EZLG_TEXT_0:fontsize=32[out]"
	unbound.TextFiles = []graph.TextFile{{Placeholder: "__EZLG_TEXT_0", Content: "hi"}}
	custom := testPlan()
	custom.Filter = "[0:v]fps=25,format=rgba,drawtext=textfile=TXTA[out]"
	custom.TextFiles = []graph.TextFile{{Placeholder: "TXTA", Content: "hi"}}
	noPath := overlayPlan()
	noPath.ExtraInputs[1].Path = ""
	for name, p := range map[string]*graph.Plan{"placeholder": unbound, "custom placeholder": custom, "overlay without path": noPath, "nil": nil} {
		for i, args := range all(p) {
			if args != nil {
				t.Errorf("%s: builder %d returned %q, want nil", name, i, args)
			}
		}
	}
	bound := testPlan()
	bound.Filter = "[0:v]fps=25,format=rgba,drawtext=textfile=/data/scratch/j1/t0.txt[out]"
	bound.TextFiles = []graph.TextFile{{Placeholder: "__EZLG_TEXT_0", Content: "hi"}} // a caller that kept the list
	for i, args := range all(bound) {
		if args == nil {
			t.Errorf("bound plan: builder %d returned nil", i)
		}
	}
}

// --- reversed plans ------------------------------------------------------------

const reversedFilter = "[0:v]fps=25,format=rgba,reverse[out]"

// reversedPlan is testPlan played backwards.
func reversedPlan() *graph.Plan {
	p := testPlan()
	p.Filter = reversedFilter
	p.Reversed = true
	return p
}

// reversedStillFilter is the still filtergraph of a reversed plan: tpad,
// then a select by frame index.
func reversedStillFilter(plan, pad string, index int, scale string) string {
	f := plan + ";[out]tpad=stop_mode=clone:stop_duration=" + pad + ",select='gte(n," + strconv.Itoa(index) + ")'"
	if scale != "" {
		f += "," + scale
	}
	return f + "[outs]"
}

func TestStillArgs_Reversed(t *testing.T) {
	// testPlan reversed: 62 slots (0..61) of 0.04 s; output slot j shows
	// source slot 61-j. The seek lies the usual 0.14 s before the middle of
	// that source slot, snapped onto the grid, and the decode keeps -to 4.
	tests := []struct {
		name      string
		mod       func(p *graph.Plan)
		t         float64
		maxW      int
		fromStart bool
		want      []string
	}{
		{
			// j=25 → source slot 36 (2.94..2.98, middle 2.96); 2.82 is slot 33.
			name: "t=1 seeks before source slot 36 and selects reversed frame 25",
			t:    1, maxW: 480,
			want: cat([]string{"-ss", "2.82", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, alphaScale480))),
		},
		{
			// j=0 → source slot 61 (middle 3.96); 3.82 is slot 58.
			name: "t=0 shows the last source frame: seek near the trim end",
			t:    0, maxW: 0,
			want: cat([]string{"-ss", "3.82", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "1", 0, ""))),
		},
		{
			// j=62 capped at 61 → source slot 0: the seek-back reaches
			// TrimStart, so the whole trimmed clip is decoded.
			name: "t = Duration shows the first source frame: decode from TrimStart",
			t:    2.5, maxW: 0,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "3.44", 61, ""))),
		},
		{
			name: "t past the end clamps like t = Duration",
			t:    99, maxW: 0,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "3.44", 61, ""))),
		},
		{
			name: "negative t counts as 0",
			t:    -1, maxW: 0,
			want: cat([]string{"-ss", "3.82", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "1", 0, ""))),
		},
		{
			name: "from start decodes the whole trimmed clip and selects the same index",
			t:    1, maxW: 480, fromStart: true,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, alphaScale480))),
		},
		{
			// Speed 2: 31 slots of 0.08 s source; j=12 → source slot 18
			// (middle 1.5 + 18.5*0.08 = 2.98); back 0.18 → 2.8 → slot 16 = 2.78.
			name: "speed 2",
			mod:  func(p *graph.Plan) { p.Speed, p.Duration, p.Frames = 2, 1.25, 31 },
			t:    0.5, maxW: 0,
			want: cat([]string{"-ss", "2.78", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "1.48", 12, ""))),
		},
		{
			// No trim end: the decode runs to the source end (no -to); the
			// known duration still locates source slot 36 (middle 1.46 → seek
			// 1.32), and TrimStart 0 means no offset to undo.
			name: "no trim, known duration",
			mod:  func(p *graph.Plan) { p.InputArgs = nil; p.TrimStart, p.TrimEnd = 0, 0 },
			t:    1, maxW: 0,
			want: cat([]string{"-ss", "1.32", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			// Without a duration or frame count the source slot is unknown, so
			// the whole clip is decoded from its start (no -ss at 0) and the
			// wanted frame is still just the 25th reversed one.
			name: "no trim, unknown duration and frames: decode from the start",
			mod: func(p *graph.Plan) {
				p.InputArgs = nil
				p.TrimStart, p.TrimEnd, p.Duration, p.Frames = 0, 0, 0, 0
			},
			t: 1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			// A 30 fps source at 25 fps: 5 slots are 6 source frames, so the
			// seek snaps from slot 33 down to slot 30 (2.7 s) to keep the
			// render's source-frame phase (the -to cut is a duration from the
			// first decoded frame).
			name: "30 fps source at 25 fps snaps the seek to whole source frames",
			mod:  func(p *graph.Plan) { p.SourceFPS = 30 },
			t:    1, maxW: 0,
			want: cat([]string{"-ss", "2.7", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			// Speed 1.5 from 30 fps: a 0.06 s slot is 1.8 source frames, 5
			// slots are 9; j=12 → source slot 28 (middle 3.21), back 0.16 →
			// slot 25 → 3.0.
			name: "speed 1.5 from 30 fps aligns on 5 slots",
			mod: func(p *graph.Plan) {
				p.Speed, p.SourceFPS, p.Duration, p.Frames = 1.5, 30, 2.5/1.5, 41
			},
			t: 0.5, maxW: 0,
			want: cat([]string{"-ss", "3", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "1.48", 12, ""))),
		},
		{
			name: "unknown source rate decodes from TrimStart",
			mod:  func(p *graph.Plan) { p.SourceFPS = 0 },
			t:    1, maxW: 0,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			// 33.333 fps never aligns with 30 fps source frames within a
			// tenth of a microsecond, so the whole trimmed clip is decoded.
			name: "fractional rate decodes from TrimStart",
			mod: func(p *graph.Plan) {
				p.Filter = "[0:v]fps=33.333,format=rgba,reverse[out]"
				p.FPS, p.SourceFPS, p.Frames = 33.333, 30, 83
			},
			t: 1, maxW: 0,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter("[0:v]fps=33.333,format=rgba,reverse[out]", "1.99001", 33, ""))),
		},
		{
			name: "decoder options survive between the seek and the input",
			mod: func(p *graph.Plan) {
				p.InputArgs = []string{"-ss", "1.5", "-c:v", "libvpx-vp9", "-t", "2.5", "-to", "4"}
			},
			t: 1, maxW: 0,
			want: cat([]string{"-ss", "2.82", "-to", "4", "-c:v", "libvpx-vp9", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			// Overlays follow the main input unseeked; the reversed frames
			// carry the forward timestamps from the seek point, which is the
			// render's overlay clock at the selected frame.
			name: "extra inputs follow the main input",
			mod: func(p *graph.Plan) {
				p.ExtraInputs = []graph.ExtraInput{{Source: 1, Path: gifOv, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 1.2, Loop: true}}
			},
			t: 1, maxW: 0,
			want: cat([]string{"-ss", "2.82", "-to", "4", "-i", mainSrc, "-stream_loop", "-1", "-i", gifOv}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			// An animation source (gif/apng/webp): the seek can land inside a
			// hold and the -to cut would then shift by a source frame, so the
			// decode runs from TrimStart — the exact from-start form — even
			// though the nominal rates align.
			name: "animation source (SourceVFR) decodes from TrimStart",
			mod:  func(p *graph.Plan) { p.SourceVFR = true },
			t:    1, maxW: 0,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			name: "animation source at t=0 (the last source frame) also decodes from TrimStart",
			mod:  func(p *graph.Plan) { p.SourceVFR = true },
			t:    0, maxW: 0,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "1", 0, ""))),
		},
		{
			name: "untrimmed animation source decodes from the start without -ss",
			mod: func(p *graph.Plan) {
				p.SourceVFR = true
				p.InputArgs = nil
				p.TrimStart, p.TrimEnd = 0, 0
			},
			t: 1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
		{
			// An untrimmed animated WebP says so itself (SeekUnsafe): the same
			// argv, whatever SourceVFR says.
			name: "untrimmed animated WebP (SeekUnsafe) decodes from the start without -ss",
			mod: func(p *graph.Plan) {
				p.SeekUnsafe, p.SourceVFR = true, false
				p.InputArgs = nil
				p.TrimStart, p.TrimEnd = 0, 0
			},
			t: 1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(reversedFilter, "2", 25, ""))),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := reversedPlan()
			if tc.mod != nil {
				tc.mod(p)
			}
			got := StillArgs(mainSrc, p, tc.t, tc.maxW)
			if tc.fromStart {
				got = StillArgsFromStart(mainSrc, p, tc.t, tc.maxW)
			}
			assertArgs(t, got, tc.want)
		})
	}
}

// TestReversedSeekInvariants mirrors TestStillSeekInvariants for reversed
// plans: the index is the render's output slot, the seek never passes the
// source slot it needs, stays inside the trim, sits on the grid, and the
// decode keeps the trim end; no timestamp offset is ever applied.
func TestReversedSeekInvariants(t *testing.T) {
	plans := []graph.Plan{
		{FPS: 30, SourceFPS: 30, Duration: 4, Speed: 1, Reversed: true},
		{FPS: 25, SourceFPS: 30, Duration: 4, Speed: 1, Reversed: true},
		{FPS: 33.333, SourceFPS: 30, Duration: 4, Frames: 133, Speed: 1, Reversed: true},
		{FPS: 25, SourceFPS: 25, Duration: 1.25, Frames: 31, TrimStart: 1.5, TrimEnd: 4, Speed: 2, Reversed: true},
		{FPS: 25, SourceFPS: 30, Duration: 2.5 / 1.5, Frames: 41, TrimStart: 1, TrimEnd: 3.5, Speed: 1.5, Reversed: true},
		{FPS: 10, SourceFPS: 29.97, Duration: 40, Speed: 0.25, Reversed: true},
		{FPS: 20, SourceFPS: 10, Duration: 0.35, Frames: 6, Speed: 2, Reversed: true},
		{FPS: 15, Speed: 1, Reversed: true}, // nothing known
		// Animation sources: the rates would align, but a seek could land in
		// a hold, so they never seek.
		{FPS: 10, SourceFPS: 10, Duration: 3.5, Frames: 35, Speed: 1, Reversed: true, SourceVFR: true},
		{FPS: 25, SourceFPS: 10, Duration: 2.5, Frames: 62, TrimStart: 0.5, TrimEnd: 3, Speed: 1, Reversed: true, SourceVFR: true},
	}
	for _, p := range plans {
		g := newStillGrid(&p)
		seeks := 0
		for _, tt := range []float64{0, 0.001, 0.02, 0.5, 1, 1.234, 2.5, 3.999, 4, 10, 40, 99, math.NaN(), math.Inf(1), -3} {
			s := stillSeekFor(&p, tt, false)
			if !s.reversed || s.offset != 0 || s.end != p.TrimEnd {
				t.Errorf("%+v t=%v: got reversed=%v offset=%v end=%v", p, tt, s.reversed, s.offset, s.end)
			}
			tc := tt
			if !(tc > 0) {
				tc = 0
			}
			wantIndex := math.Min(math.Floor(tc*g.fps+stillSlotEpsilon), maxStillIndex)
			if g.last >= 0 {
				wantIndex = math.Min(wantIndex, g.last)
			}
			if float64(s.index) != wantIndex {
				t.Errorf("%+v t=%v: index %d, want %v", p, tt, s.index, wantIndex)
			}
			if s.pad < float64(s.index)/g.fps+stillPadSlack-1e-9 {
				t.Errorf("%+v t=%v: pad %v does not reach index %d + slack", p, tt, s.pad, s.index)
			}
			if s.start < g.trimStart-1e-9 {
				t.Errorf("%+v t=%v: start %v before TrimStart %v", p, tt, s.start, g.trimStart)
			}
			slots := (s.start - g.trimStart) / g.period
			if math.Abs(slots-math.Round(slots)) > 1e-6 {
				t.Errorf("%+v t=%v: start %v is not on the slot grid", p, tt, s.start)
			}
			if g.last < 0 {
				if s.start != g.trimStart {
					t.Errorf("%+v t=%v: unknown length must decode from TrimStart, got %v", p, tt, s.start)
				}
			} else if k := g.last - float64(s.index); math.Round(slots) > k {
				t.Errorf("%+v t=%v: seek slot %v is past the source slot %v it needs", p, tt, slots, k)
			}
			// A seek keeps the source-frame phase of TrimStart (so the -to
			// cut lands on the render's frame): the slots spanned are a whole
			// number of source frames within the tolerance.
			if s.start > g.trimStart {
				seeks++
				if err := g.phaseError(p.SourceFPS, math.Round(slots)); err >= seekPhaseTolerance {
					t.Errorf("%+v t=%v: %v slots are %.3g s off a whole source frame", p, tt, slots, err)
				}
			}
			if p.TrimEnd > 0 && s.start >= p.TrimEnd {
				t.Errorf("%+v t=%v: start %v not before TrimEnd %v", p, tt, s.start, p.TrimEnd)
			}
			fs := stillSeekFor(&p, tt, true)
			if fs.start != g.trimStart || fs.index != s.index || fs.end != s.end {
				t.Errorf("%+v t=%v: from-start %+v vs %+v", p, tt, fs, s)
			}
		}
		// Aligned rates do seek; fractional, NTSC and unknown ones, and every
		// animation source, decode from TrimStart.
		aligned := !p.SourceVFR && (p.SourceFPS == 30 && p.FPS != 33.333 || p.SourceFPS == 25 || p.SourceFPS == 10)
		if aligned && seeks == 0 {
			t.Errorf("%+v: never seeks although the rates align", p)
		}
		if !aligned && seeks > 0 {
			t.Errorf("%+v: seeks %d times although the rates do not align", p, seeks)
		}
	}
}

// --- filter-trimmed plans (animated WebP) --------------------------------------

const (
	filterTrimFilter    = "[0:v]trim=start=1.5:end=4,setpts=PTS-STARTPTS,fps=25:round=down,format=rgba[out]"
	filterTrimReversed  = "[0:v]trim=start=1.5:end=4,setpts=PTS-STARTPTS,fps=25:round=down,format=rgba,reverse[out]"
	filterTrimEndOnly   = "[0:v]trim=start=0:end=4,setpts=PTS-STARTPTS,fps=25:round=down,format=rgba[out]"
	filterTrimToTheEnd  = "[0:v]trim=start=1.5,setpts=PTS-STARTPTS,fps=25:round=down,format=rgba,reverse[out]"
	filterTrimNoSeekMsg = "a filter-trimmed plan must not be seeked"
	seekUnsafeFilter    = "[0:v]fps=25:round=down,format=rgba[out]"
	seekUnsafeReversed  = "[0:v]fps=25:round=down,format=rgba,reverse[out]"
	seekUnsafeNoSeekMsg = "a SeekUnsafe plan must not be seeked"
)

// filterTrimPlan is testPlan as the compiler emits it for a trimmed
// webp_anim source (FFmpeg 9's webp_anim demuxer decodes nothing after any
// input seek): the trim is the first filter stage, InputArgs carry no
// -ss/-to, and the plan is SeekUnsafe (implied by FilterTrim).
func filterTrimPlan() *graph.Plan {
	p := testPlan()
	p.InputArgs = nil
	p.Filter = filterTrimFilter
	p.FilterTrim = true
	p.SeekUnsafe = true
	p.SourceVFR = true
	return p
}

// seekUnsafePlan is testPlan as the compiler emits it for an UNTRIMMED
// webp_anim source: a 4 s clip with no trim stage, no InputArgs and
// SeekUnsafe without FilterTrim.
func seekUnsafePlan() *graph.Plan {
	p := testPlan()
	p.InputArgs = nil
	p.Filter = seekUnsafeFilter
	p.TrimStart, p.TrimEnd = 0, 0
	p.Duration, p.Frames = 4, 100
	p.SeekUnsafe = true
	p.SourceVFR = true
	return p
}

// TestFilterTrimPlansAreNeverSeeked: the still builders emit no -ss,
// -itsoffset or -to for a plan whose demuxer cannot seek — a filter-trimmed
// plan (FilterTrim: the plan's trim stage cuts and rebases the clock) or an
// untrimmed SeekUnsafe plan (the file's clock is the render's) — forward or
// reversed, seeking or from-start variant, and select the same slot/index
// as the from-start maths; the master, proxy and detection builders pass
// the (seekless) InputArgs through, the proxy also for a reversed tail.
func TestFilterTrimPlansAreNeverSeeked(t *testing.T) {
	tests := []struct {
		name      string
		unsafe    bool // base plan: seekUnsafePlan (untrimmed) instead of filterTrimPlan
		mod       func(p *graph.Plan)
		t         float64
		maxW      int
		fromStart bool
		want      []string
	}{
		{
			// t=1 → slot 25 counted from the rebased start: threshold 24.5/25,
			// pad 25/25 + 1; no -ss, no -itsoffset.
			name: "forward t=1",
			t:    1, maxW: 480,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(filterTrimFilter, "2", "0.98", alphaScale480))),
		},
		{
			name: "forward t=0",
			t:    0, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(filterTrimFilter, "1", "0", ""))),
		},
		{
			name: "forward from start is the same argv",
			t:    1, maxW: 480, fromStart: true,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(filterTrimFilter, "2", "0.98", alphaScale480))),
		},
		{
			// Clamped to the render's last slot 61: threshold 60.5/25, pad 61/25 + 1.
			name: "forward t past the end clamps to the last slot",
			t:    99, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(filterTrimFilter, "3.44", "2.42", ""))),
		},
		{
			name: "speed 2 keeps the output-time slot maths",
			mod:  func(p *graph.Plan) { p.Speed, p.Duration, p.Frames = 2, 1.25, 31 },
			t:    0.5, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(filterTrimFilter, "1.48", "0.46", ""))),
		},
		{
			// Reversed: the index selection of the from-start form, without
			// -ss or -to (the trim stage already ends the clip at TrimEnd).
			name: "reversed t=1",
			mod:  func(p *graph.Plan) { p.Filter, p.Reversed = filterTrimReversed, true },
			t:    1, maxW: 480,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(filterTrimReversed, "2", 25, alphaScale480))),
		},
		{
			name: "reversed t=0",
			mod:  func(p *graph.Plan) { p.Filter, p.Reversed = filterTrimReversed, true },
			t:    0, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(filterTrimReversed, "1", 0, ""))),
		},
		{
			name: "reversed from start",
			mod:  func(p *graph.Plan) { p.Filter, p.Reversed = filterTrimReversed, true },
			t:    1, maxW: 0, fromStart: true,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(filterTrimReversed, "2", 25, ""))),
		},
		{
			// A CFR-style source with a filter trim (SourceVFR false) is not
			// seeked either: the flag alone decides.
			name: "reversed, FilterTrim without SourceVFR",
			mod:  func(p *graph.Plan) { p.Filter, p.Reversed, p.SourceVFR = filterTrimReversed, true, false },
			t:    1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(filterTrimReversed, "2", 25, ""))),
		},
		{
			// An end-only trim: TrimStart 0 never produced a -ss, but the -to
			// is gone as well.
			name: "end-only trim",
			mod:  func(p *graph.Plan) { p.Filter, p.TrimStart, p.Duration, p.Frames = filterTrimEndOnly, 0, 4, 100 },
			t:    1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(filterTrimEndOnly, "2", "0.98", ""))),
		},
		{
			name: "trim to the end, reversed",
			mod:  func(p *graph.Plan) { p.Filter, p.Reversed, p.TrimEnd = filterTrimToTheEnd, true, 0 },
			t:    1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(filterTrimToTheEnd, "2", 25, ""))),
		},
		{
			// Decoder options still precede the input; extra inputs follow it.
			name: "decoder options and extra inputs",
			mod: func(p *graph.Plan) {
				p.InputArgs = []string{"-c:v", "libvpx-vp9"}
				p.ExtraInputs = []graph.ExtraInput{{Source: 1, Path: gifOv, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 1.2, Loop: true}}
			},
			t: 1, maxW: 0,
			want: cat([]string{"-c:v", "libvpx-vp9", "-i", mainSrc, "-stream_loop", "-1", "-i", gifOv}, stillOut(stillFilter(filterTrimFilter, "2", "0.98", ""))),
		},
		{
			// FilterTrim alone (a plan built before SeekUnsafe existed) still
			// decides: no seek.
			name: "FilterTrim without SeekUnsafe",
			mod:  func(p *graph.Plan) { p.SeekUnsafe = false },
			t:    1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(filterTrimFilter, "2", "0.98", ""))),
		},
		// --- untrimmed SeekUnsafe plans (an animated WebP without a trim) ---
		{
			// t=1 → slot 25 from the file's start: threshold 24.5/25, pad
			// 25/25 + 1. A seekable plan would get "-ss 0.84 -itsoffset 0.84".
			name:   "untrimmed SeekUnsafe: forward t=1",
			unsafe: true,
			t:      1, maxW: 480,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(seekUnsafeFilter, "2", "0.98", alphaScale480))),
		},
		{
			name:   "untrimmed SeekUnsafe: forward from start is the same argv",
			unsafe: true,
			t:      1, maxW: 480, fromStart: true,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(seekUnsafeFilter, "2", "0.98", alphaScale480))),
		},
		{
			// Clamped to the render's last slot 99: threshold 98.5/25, pad 99/25 + 1.
			name:   "untrimmed SeekUnsafe: forward t past the end clamps to the last slot",
			unsafe: true,
			t:      99, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(seekUnsafeFilter, "4.96", "3.94", ""))),
		},
		{
			// Reversed without SourceVFR (the flag alone decides): a seekable
			// 25 fps plan would be seeked to 2.84 for its reversed frame 25.
			name:   "untrimmed SeekUnsafe: reversed t=1, without SourceVFR",
			unsafe: true,
			mod:    func(p *graph.Plan) { p.Filter, p.Reversed, p.SourceVFR = seekUnsafeReversed, true, false },
			t:      1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(seekUnsafeReversed, "2", 25, ""))),
		},
		{
			name:   "untrimmed SeekUnsafe: reversed from start",
			unsafe: true,
			mod:    func(p *graph.Plan) { p.Filter, p.Reversed = seekUnsafeReversed, true },
			t:      1, maxW: 480, fromStart: true,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(seekUnsafeReversed, "2", 25, alphaScale480))),
		},
		{
			name:   "untrimmed SeekUnsafe: reversed t=0",
			unsafe: true,
			mod:    func(p *graph.Plan) { p.Filter, p.Reversed = seekUnsafeReversed, true },
			t:      0, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(reversedStillFilter(seekUnsafeReversed, "1", 0, ""))),
		},
		{
			name:   "untrimmed SeekUnsafe: decoder options and extra inputs",
			unsafe: true,
			mod: func(p *graph.Plan) {
				p.InputArgs = []string{"-c:v", "libvpx-vp9"}
				p.ExtraInputs = []graph.ExtraInput{{Source: 1, Path: gifOv, Args: []string{"-stream_loop", "-1"}, Animated: true, Duration: 1.2, Loop: true}}
			},
			t: 1, maxW: 0,
			want: cat([]string{"-c:v", "libvpx-vp9", "-i", mainSrc, "-stream_loop", "-1", "-i", gifOv}, stillOut(stillFilter(seekUnsafeFilter, "2", "0.98", ""))),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filterTrimPlan()
			if tc.unsafe {
				p = seekUnsafePlan()
			}
			if tc.mod != nil {
				tc.mod(p)
			}
			got := StillArgs(mainSrc, p, tc.t, tc.maxW)
			if tc.fromStart {
				got = StillArgsFromStart(mainSrc, p, tc.t, tc.maxW)
			}
			assertArgs(t, got, tc.want)
		})
	}

	// The seek maths agree with the from-start variant at every t, forward
	// and reversed, trimmed (FilterTrim) and untrimmed (SeekUnsafe alone),
	// with the seek itself removed.
	for _, unsafe := range []bool{false, true} {
		for _, reversed := range []bool{false, true} {
			p, msg, rev := filterTrimPlan(), filterTrimNoSeekMsg, filterTrimReversed
			if unsafe {
				p, msg, rev = seekUnsafePlan(), seekUnsafeNoSeekMsg, seekUnsafeReversed
			}
			if reversed {
				p.Filter, p.Reversed = rev, true
			}
			for _, tt := range []float64{0, 0.001, 0.5, 1, 2.499, 2.5, 4, 99, -1, math.NaN(), math.Inf(1)} {
				s := stillSeekFor(p, tt, false)
				if s.start != 0 || s.offset != 0 || s.end != 0 {
					t.Errorf("unsafe=%v reversed=%v t=%v: %s, got start=%v offset=%v end=%v", unsafe, reversed, tt, msg, s.start, s.offset, s.end)
				}
				q := *p
				q.FilterTrim, q.SeekUnsafe = false, false
				fs := stillSeekFor(&q, tt, true)
				if s.reversed != fs.reversed || s.index != fs.index || s.threshold != fs.threshold || s.pad != fs.pad {
					t.Errorf("unsafe=%v reversed=%v t=%v: selection %+v differs from the from-start form %+v", unsafe, reversed, tt, s, fs)
				}
			}
		}
	}

	// The other builders just pass the plan's (seekless) InputArgs through:
	// the argv opens with the bare input — the proxy also for a reversed
	// plan longer than the preview, whose tail a seekable source would be
	// seeked to (longReversedPlan's shape: "-ss 19.84").
	longUnsafe := seekUnsafePlan()
	longUnsafe.Filter, longUnsafe.Reversed = seekUnsafeReversed, true
	longUnsafe.Duration, longUnsafe.Frames = 30, 750
	longTrimmed := filterTrimPlan()
	longTrimmed.Filter, longTrimmed.Reversed = filterTrimReversed, true
	longTrimmed.Duration, longTrimmed.Frames, longTrimmed.TrimEnd = 30, 750, 31.5
	for name, args := range map[string][]string{
		"MasterArgs":                        MasterArgs(mainSrc, filterTrimPlan(), "f.rgba"),
		"ProxyArgs":                         ProxyArgs(mainSrc, filterTrimPlan(), 0, 0, "p.webp"),
		"CropDetectPlanArgs":                CropDetectPlanArgs(mainSrc, filterTrimPlan(), true, 1),
		"ProxyArgs reversed long trimmed":   ProxyArgs(mainSrc, longTrimmed, 0, 10, "p.webp"),
		"MasterArgs untrimmed":              MasterArgs(mainSrc, seekUnsafePlan(), "f.rgba"),
		"ProxyArgs untrimmed":               ProxyArgs(mainSrc, seekUnsafePlan(), 0, 0, "p.webp"),
		"CropDetectPlanArgs untrimmed":      CropDetectPlanArgs(mainSrc, seekUnsafePlan(), true, 1),
		"ProxyArgs reversed long untrimmed": ProxyArgs(mainSrc, longUnsafe, 0, 10, "p.webp"),
	} {
		if len(args) < 2 || args[0] != "-i" || args[1] != mainSrc {
			t.Errorf("%s: %s; argv must start with the bare input, got %q", name, seekUnsafeNoSeekMsg, args)
		}
	}
	// The control: the same untrimmed shape on a seekable animation source
	// is seeked for its tail.
	gifLong := *longUnsafe
	gifLong.SeekUnsafe = false
	if args := ProxyArgs(mainSrc, &gifLong, 0, 10, "p.webp"); len(args) < 2 || args[0] != "-ss" || args[1] != "19.84" {
		t.Errorf("an untrimmed seekable animation source must get the plain tail seek, got %q", args)
	}
}

// --- autocrop ----------------------------------------------------------------------

const (
	cropSampleStage = "select='isnan(prev_selected_t)+gte(round((t-prev_selected_t)*1000),200)'"
	cropTail        = "-an -f null -"
)

func TestCropDetectArgs(t *testing.T) {
	tests := []struct {
		name      string
		inputArgs []string
		alpha     bool
		threshold int
		want      []string
	}{
		{
			name:      "alpha source, trimmed, default threshold",
			inputArgs: []string{"-ss", "1.5", "-to", "4"},
			alpha:     true,
			want: []string{"-ss", "1.5", "-to", "4", "-i", "/data/blobs/ab/abcd",
				"-vf", cropSampleStage + ",format=rgba,alphaextract,lagfun=decay=1,bbox=min_val=0",
				"-an", "-f", "null", "-"},
		},
		{
			name:      "alpha threshold 128 → min_val 127 (alpha >= 128 counts)",
			alpha:     true,
			threshold: 128,
			want: []string{"-i", "/data/blobs/ab/abcd",
				"-vf", cropSampleStage + ",format=rgba,alphaextract,lagfun=decay=1,bbox=min_val=127",
				"-an", "-f", "null", "-"},
		},
		{
			name:      "alpha threshold clamps to 1..255",
			alpha:     true,
			threshold: 300,
			want: []string{"-i", "/data/blobs/ab/abcd",
				"-vf", cropSampleStage + ",format=rgba,alphaextract,lagfun=decay=1,bbox=min_val=254",
				"-an", "-f", "null", "-"},
		},
		{
			name:      "negative threshold counts as 1",
			alpha:     true,
			threshold: -7,
			want: []string{"-i", "/data/blobs/ab/abcd",
				"-vf", cropSampleStage + ",format=rgba,alphaextract,lagfun=decay=1,bbox=min_val=0",
				"-an", "-f", "null", "-"},
		},
		{
			name:      "opaque source: cropdetect black-border measure",
			inputArgs: []string{"-c:v", "libvpx-vp9"},
			threshold: 128, // ignored for opaque sources
			want: []string{"-c:v", "libvpx-vp9", "-i", "/data/blobs/ab/abcd",
				"-vf", cropSampleStage + ",cropdetect=limit=24:round=2:reset=0:skip=0",
				"-an", "-f", "null", "-"},
		},
		{
			name:      "sequence input args and pattern pass through",
			inputArgs: []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"},
			alpha:     true,
			threshold: 1,
			want: []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0", "-i", "/data/blobs/ab/abcd",
				"-vf", cropSampleStage + ",format=rgba,alphaextract,lagfun=decay=1,bbox=min_val=0",
				"-an", "-f", "null", "-"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := "/data/blobs/ab/abcd"
			assertArgs(t, CropDetectArgs(src, tc.inputArgs, tc.alpha, tc.threshold), tc.want)
		})
	}
	if !strings.HasSuffix(strings.Join(CropDetectArgs("x", nil, true, 1), " "), cropTail) {
		t.Error("detection argv must end in -an -f null -")
	}
}

// Detection plans as graph.CompileDetect emits them: the ops in front of the
// autocrop at the source frame size, ending in [out].
const (
	keyedFilter   = "[0:v]fps=10:round=down,format=yuva444p,chromakey=color=0x00ff00:similarity=0.2:blend=0.05,despill=type=green:mix=0.6:expand=0.3,format=rgba[out]"
	opaqueFilter  = "[0:v]fps=10:round=down,format=rgba[out]"
	stillFilter0  = "[0:v]format=rgba[out]"
	alphaDetector = ",format=rgba,alphaextract,lagfun=decay=1,bbox=min_val="
	opaqueDetect  = ",cropdetect=limit=24:round=2:reset=0:skip=0"
)

// detectPlan builds a detection plan with the given input args and filter.
func detectPlan(inputArgs []string, filter string, alpha bool) *graph.Plan {
	return &graph.Plan{InputArgs: inputArgs, Filter: filter, OutLabel: "[out]", Width: 160, Height: 120, FPS: 10, HasAlpha: alpha, Speed: 1}
}

func TestCropDetectPlanArgs(t *testing.T) {
	src := "/data/blobs/ab/abcd"
	tests := []struct {
		name      string
		plan      *graph.Plan
		alpha     bool
		threshold int
		want      []string
	}{
		{
			name:  "keyed opaque source: the chromakey stage precedes the alpha detector",
			plan:  detectPlan([]string{"-ss", "1.5", "-to", "4"}, keyedFilter, true),
			alpha: true,
			want: []string{"-ss", "1.5", "-to", "4", "-i", src,
				"-filter_complex", keyedFilter + ";[out]" + cropSampleStage + alphaDetector + "0[det]",
				"-map", "[det]", "-an", "-f", "null", "-"},
		},
		{
			name:      "alpha threshold 128 → min_val 127, as for the raw-source builder",
			plan:      detectPlan(nil, keyedFilter, true),
			alpha:     true,
			threshold: 128,
			want: []string{"-i", src,
				"-filter_complex", keyedFilter + ";[out]" + cropSampleStage + alphaDetector + "127[det]",
				"-map", "[det]", "-an", "-f", "null", "-"},
		},
		{
			name:      "threshold clamps to 1..255",
			plan:      detectPlan(nil, keyedFilter, true),
			alpha:     true,
			threshold: 999,
			want: []string{"-i", src,
				"-filter_complex", keyedFilter + ";[out]" + cropSampleStage + alphaDetector + "254[det]",
				"-map", "[det]", "-an", "-f", "null", "-"},
		},
		{
			name:      "opaque plan: cropdetect on the plan's rgba frames",
			plan:      detectPlan([]string{"-c:v", "libvpx-vp9"}, opaqueFilter, false),
			threshold: 128, // ignored
			want: []string{"-c:v", "libvpx-vp9", "-i", src,
				"-filter_complex", opaqueFilter + ";[out]" + cropSampleStage + opaqueDetect + "[det]",
				"-map", "[det]", "-an", "-f", "null", "-"},
		},
		{
			name:  "still plan: no fps stage, no loop args",
			plan:  detectPlan(nil, stillFilter0, true),
			alpha: true,
			want: []string{"-i", src,
				"-filter_complex", stillFilter0 + ";[out]" + cropSampleStage + alphaDetector + "0[det]",
				"-map", "[det]", "-an", "-f", "null", "-"},
		},
		{
			name: "image sequence: pattern joined to the blob dir, image2 args first",
			plan: func() *graph.Plan {
				p := detectPlan([]string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0", "-ss", "1", "-to", "2"}, "[0:v]scale=160:120:force_original_aspect_ratio=decrease:flags=lanczos,fps=10:round=down,format=rgba[out]", true)
				p.InputPattern = "%06d.png"
				return p
			}(),
			alpha:     true,
			threshold: 1,
			want: []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0", "-ss", "1", "-to", "2", "-i", src + "/%06d.png",
				"-filter_complex", "[0:v]scale=160:120:force_original_aspect_ratio=decrease:flags=lanczos,fps=10:round=down,format=rgba[out];[out]" + cropSampleStage + alphaDetector + "0[det]",
				"-map", "[det]", "-an", "-f", "null", "-"},
		},
		{
			name:  "extra inputs follow the main input, as for MasterArgs",
			plan:  overlayPlan(),
			alpha: true,
			want: cat([]string{"-ss", "1.5", "-to", "4", "-i", src}, extraArgs, []string{
				"-filter_complex", ovFilter + ";[out]" + cropSampleStage + alphaDetector + "0[det]",
				"-map", "[det]", "-an", "-f", "null", "-"}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, CropDetectPlanArgs(src, tc.plan, tc.alpha, tc.threshold), tc.want)
		})
	}
	// The detector chains are byte-identical to the raw-source builder's.
	for _, alpha := range []bool{true, false} {
		raw := CropDetectArgs(src, nil, alpha, 77)
		planned := CropDetectPlanArgs(src, detectPlan(nil, stillFilter0, alpha), alpha, 77)
		if got, want := planned[3], stillFilter0+";[out]"+raw[3]+"[det]"; got != want {
			t.Errorf("alpha=%v: plan filter %q, want %q", alpha, got, want)
		}
	}
	// Unusable plans (unbound text, pathless overlay input, nil) yield nil.
	unbound := detectPlan(nil, "[0:v]drawtext=textfile=__EZLG_TEXT_1__:fontsize=12,format=rgba[out]", true)
	unbound.TextFiles = []graph.TextFile{{Placeholder: "__EZLG_TEXT_1__", Content: "x"}}
	pathless := overlayPlan()
	pathless.ExtraInputs[1].Path = ""
	for name, p := range map[string]*graph.Plan{"unbound text": unbound, "pathless overlay": pathless, "nil": nil} {
		if got := CropDetectPlanArgs(src, p, true, 1); got != nil {
			t.Errorf("%s: got %q, want nil", name, got)
		}
	}
}

// Real stderr excerpts (FFmpeg 9.0.1) for ParseCropDetect.
const (
	bboxLog = "[Parsed_bbox_3 @ 0000022472d14480] n:0 pts:0 pts_time:0 x1:21 x2:81 y1:11 y2:51 w:61 h:41 crop=61:41:21:11 drawbox=21:11:61:41\n" +
		"[Parsed_bbox_3 @ 0000022472d14480] n:1 pts:6 pts_time:0.2\n" + // an empty frame prints no box
		"[Parsed_bbox_3 @ 0000022472d14480] n:2 pts:12 pts_time:0.4 x1:40 x2:99 y1:10 y2:49 w:60 h:40 crop=60:40:40:10 drawbox=40:10:60:40\n" +
		"[out#0/null @ 000001f820023d80] video:2KiB audio:0KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: unknown\n"
	cropdetectLog = "Input #0, png_pipe, from 'opaque.png':\n" +
		"  Duration: N/A, bitrate: N/A\n" +
		"[Parsed_cropdetect_1 @ 000001f820050340] x1:159 x2:0 y1:119 y2:0 w:-158 h:-118 x:160 y:120 pts:0 t:0.000000 limit:24.000000 crop=-158:-118:160:120\n" +
		"[Parsed_cropdetect_1 @ 000001f820050340] x1:20 x2:79 y1:10 y2:49 w:60 h:40 x:20 y:10 pts:3 t:0.600000 limit:24.000000 crop=60:40:20:10\n" +
		"[Parsed_cropdetect_1 @ 000001f820050340] x1:0 x2:107 y1:10 y2:127 w:108 h:118 x:0 y:10 pts:4 t:0.800000 limit:24.000000 crop=108:118:0:10\n"
	// A source whose tags record an ffmpeg command line: at info level the
	// tags are echoed in the Input #0 block and again in the Output #0 block
	// (the null muxer inherits the global metadata), around the detector's
	// own lines. Neither echo is a detector line.
	taggedLog = "Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'tagged.mov':\n" +
		"  Metadata:\n" +
		"    major_brand     : qt  \n" +
		"    comment         : made with ffmpeg -vf crop=1:1:0:0\n" +
		"    title           : crop=9999:9999:0:0\n" +
		"  Duration: 00:00:02.00, start: 0.000000, bitrate: 68 kb/s\n" +
		"  Stream #0:0[0x1]: Video: png (png / 0x20676E70), rgb24(pc, gbr/unknown/unknown, progressive), 160x120, 10 fps, 10 tbr, 10240 tbn (default)\n" +
		"Stream mapping:\n" +
		"  Stream #0:0 (png) -> select:default\n" +
		"  cropdetect:default -> Stream #0:0 (wrapped_avframe)\n" +
		"Output #0, null, to 'pipe:':\n" +
		"  Metadata:\n" +
		"    comment         : made with ffmpeg -vf crop=1:1:0:0\n" +
		"    title           : crop=9999:9999:0:0\n" +
		"    encoder         : Lavf62.3.100\n" +
		"  Stream #0:0: Video: wrapped_avframe, rgb24(pc, gbr/unknown/unknown, progressive), 160x120, q=2-31, 200 kb/s, 10 fps, 10 tbn\n" +
		"[Parsed_cropdetect_1 @ 000001f820050340] x1:20 x2:79 y1:10 y2:49 w:60 h:40 x:20 y:10 pts:0 t:0.000000 limit:24.000000 crop=60:40:20:10\n" +
		"[Parsed_cropdetect_1 @ 000001f820050340] x1:20 x2:79 y1:10 y2:49 w:60 h:40 x:20 y:10 pts:2 t:0.200000 limit:24.000000 crop=60:40:20:10\n" +
		"[out#0/null @ 000001f820023d80] video:2KiB audio:0KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: unknown\n"
)

func TestParseCropDetect(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		w, h, x, y int
		ok         bool
	}{
		// 61x41@21,11 ∪ 60x40@40,10 = x 21..99, y 10..51.
		{"bbox lines: union of every box, empty frames skipped", bboxLog, 79, 42, 21, 10, true},
		{"cropdetect lines: negative box skipped, accumulated union", cropdetectLog, 108, 118, 0, 10, true},
		{"CRLF endings", strings.ReplaceAll(bboxLog, "\n", "\r\n"), 79, 42, 21, 10, true},
		{"last line without a newline", strings.TrimSuffix(bboxLog, "\n"), 79, 42, 21, 10, true},
		{"only negative boxes (nothing above the limit)", "[Parsed_cropdetect_2 @ 0x1] x1:63 x2:0 y1:47 y2:0 w:-62 h:-46 x:64 y:48 pts:2 t:0.4 limit:24.000000 crop=-62:-46:64:48\n", 0, 0, 0, 0, false},
		{"no crop field at all", "[Parsed_bbox_3 @ 0x1] n:0 pts:0 pts_time:0\nframe=    5 fps=0.0\n", 0, 0, 0, 0, false},
		{"empty", "", 0, 0, 0, 0, false},
		{"malformed fields are ignored", "[Parsed_bbox_1 @ 0x1] crop=a:b:c:d\n[Parsed_bbox_1 @ 0x1] crop=1:2:3\n[Parsed_bbox_1 @ 0x1] crop=0:5:1:1\n[Parsed_bbox_1 @ 0x1] crop=4:0:1:1\n[Parsed_bbox_1 @ 0x1] crop=4:5:-1:1\n[Parsed_bbox_1 @ 0x1] crop=4:5:1:-1\n[Parsed_bbox_1 @ 0x1] crop=\n[Parsed_bbox_1 @ 0x1] n:7 crop=8:6:2:3 tail\n", 8, 6, 2, 3, true},
		{"a single pixel", "[Parsed_bbox_1 @ 0x1] n:0 pts:0 pts_time:0 x1:499 x2:499 y1:299 y2:299 w:1 h:1 crop=1:1:499:299 drawbox=499:299:1:1\n", 1, 1, 499, 299, true},
		// Only the detectors' own lines are read: a crop field on any other
		// line — a metadata tag echo, a bare line, an indented copy of a
		// detector line — never reaches the union.
		{"metadata echoes around the detector lines are ignored", taggedLog, 60, 40, 20, 10, true},
		{"metadata echoes alone find no box", strings.ReplaceAll(taggedLog, "[Parsed_cropdetect_1", "[Parsed_cropdetect_1 (not at column 0)\n  "), 0, 0, 0, 0, false},
		{"a bare crop field is not a detector line", " crop=8:6:2:3 tail\ncrop=8:6:2:3\n", 0, 0, 0, 0, false},
		{"bbox echo in a tag and a real bbox line", "    comment         : x1:0 x2:0 y1:0 y2:0 w:1 h:1 crop=1:1:0:0 drawbox=0:0:1:1\n[Parsed_bbox_1 @ 0x1] n:0 pts:0 pts_time:0 x1:21 x2:81 y1:11 y2:51 w:61 h:41 crop=61:41:21:11 drawbox=21:11:61:41\n", 61, 41, 21, 11, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, h, x, y, ok := ParseCropDetect(tc.in)
			if w != tc.w || h != tc.h || x != tc.x || y != tc.y || ok != tc.ok {
				t.Errorf("got %d:%d:%d:%d ok=%v, want %d:%d:%d:%d ok=%v", w, h, x, y, ok, tc.w, tc.h, tc.x, tc.y, tc.ok)
			}
		})
	}
}

// --- fonts --------------------------------------------------------------------------

func TestFcListArgs(t *testing.T) {
	assertArgs(t, FcListArgs(), []string{"--format", `%{family[0]}\t%{style[0]}\t%{file}\n`})
	// The escapes are literal backslash sequences for fontconfig, not Go
	// control characters.
	if strings.ContainsAny(FcListArgs()[1], "\t\n") {
		t.Error("format string must carry literal \\t and \\n")
	}
}

func TestParseFcList(t *testing.T) {
	in := strings.Join([]string{
		"DejaVu Serif\tBook\t/usr/share/fonts/truetype/dejavu/DejaVuSerif.ttf",
		"DejaVu Sans Mono\tBold Oblique\t/usr/share/fonts/truetype/dejavu/DejaVuSansMono-BoldOblique.ttf",
		"DejaVu Sans\tBook\t/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"DejaVu Sans\tBold\t/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
		"Noto Sans,Noto Sans Regular\tRegular,Normal\t/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf", // aliases
		"Noto Sans CJK JP,Noto Sans CJK JP Regular\tRegular\t/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
		"Font:Colon\tRegular\t/fonts/colon.ttf",        // rejected: ':'
		"Under_score\tRegular\t/fonts/under.ttf",       // rejected: '_'
		"ＭＳ ゴシック\tRegular\t/fonts/msgothic.ttc",        // rejected: non-ASCII
		"Dotted.Name\tRegular\t/fonts/dot.ttf",         // rejected: '.'
		"DejaVu Sans\tBook\t/fonts/dup/DejaVuSans.ttf", // duplicate face: first wins
		"Weird\t\t/fonts/weird.ttf",                    // no style → Regular
		"NoFile\tRegular\t",                            // rejected: no file
		"Only two fields\tRegular",                     // rejected: malformed
		"\tRegular\t/fonts/nofamily.ttf",               // rejected: no family
		" Spaced \tBold\t/fonts/spaced.ttf",            // trimmed
		"",
		"Crlf-Face\tItalic\t/fonts/crlf.ttf\r",
	}, "\n")
	got := ParseFcList(in)
	want := []Font{
		{"Crlf-Face", "Italic", "/fonts/crlf.ttf"},
		{"DejaVu Sans", "Bold", "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"},
		{"DejaVu Sans", "Book", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"},
		{"DejaVu Sans Mono", "Bold Oblique", "/usr/share/fonts/truetype/dejavu/DejaVuSansMono-BoldOblique.ttf"},
		{"DejaVu Serif", "Book", "/usr/share/fonts/truetype/dejavu/DejaVuSerif.ttf"},
		{"Noto Sans", "Regular", "/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf"},
		{"Noto Sans CJK JP", "Regular", "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"},
		{"Spaced", "Bold", "/fonts/spaced.ttf"},
		{"Weird", "Regular", "/fonts/weird.ttf"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseFcList:\n got %+v\nwant %+v", got, want)
	}
	if got := ParseFcList(""); len(got) != 0 {
		t.Errorf("empty input: %+v", got)
	}
	if got := ParseFcList("garbage\n\n:style=Bold\n"); len(got) != 0 {
		t.Errorf("garbage input: %+v", got)
	}
}
