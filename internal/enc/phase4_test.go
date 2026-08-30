package enc

import (
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// The Phase 4 flatten graph for the plain 320x240 25 fps test master: matte
// underlay, even-dim pad, then the tagged bt709/tv yuv420p conversion (the
// encoder must never fall back to swscale's untagged bt601 default).
const (
	mp4ColorConvert = ",scale=out_color_matrix=bt709:out_range=tv,format=yuv420p," +
		"setparams=colorspace=bt709:color_primaries=bt709:color_trc=bt709"
	mp4FlattenDefault = "color=c=0x313338:s=320x240:r=25,format=rgba[bg];" +
		"[bg][0:v]overlay=format=auto:shortest=1,pad=ceil(iw/2)*2:ceil(ih/2)*2:color=0x313338" + mp4ColorConvert + "[f]"
	mp4FlattenWhite = "color=c=0xffffff:s=320x240:r=25,format=rgba[bg];" +
		"[bg][0:v]overlay=format=auto:shortest=1,pad=ceil(iw/2)*2:ceil(ih/2)*2:color=0xffffff" + mp4ColorConvert + "[f]"
)

func TestMP4Args(t *testing.T) {
	m := testMaster()
	got := MP4Args(m, MP4Options{}, "/data/scratch/j1/out.mp4")
	want := []string{
		"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25",
		"-i", "/dev/shm/ezl/job1/frames.rgba",
		"-filter_complex", mp4FlattenDefault,
		"-map", "[f]",
		"-c:v", "libx264",
		"-crf", "20",
		"-preset", "slow",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-movflags", "+faststart",
		"-an",
		"-f", "mp4",
		"/data/scratch/j1/out.mp4",
	}
	assertArgs(t, got, want)
}

func TestMP4Args_MatteAndCRF(t *testing.T) {
	m := testMaster()
	got := MP4Args(m, MP4Options{CRF: 18, Matte: "FFFFFF"}, "out.mp4")
	want := []string{
		"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25",
		"-i", "/dev/shm/ezl/job1/frames.rgba",
		"-filter_complex", mp4FlattenWhite,
		"-map", "[f]",
		"-c:v", "libx264",
		"-crf", "18",
		"-preset", "slow",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-movflags", "+faststart",
		"-an",
		"-f", "mp4",
		"out.mp4",
	}
	assertArgs(t, got, want)

	// An invalid matte falls back to the Discord dark default.
	got = MP4Args(m, MP4Options{Matte: "zz"}, "out.mp4")
	if got[11] != mp4FlattenDefault {
		t.Errorf("invalid matte: filter = %q", got[11])
	}
}

// TestMP4Args_Variant: a fit rung (fps drop + downscale) is baked in before
// the flatten, and the matte colour source / pad follow the variant's
// size and rate. The pad keeps ODD variant sizes encodable (161 → 162).
func TestMP4Args_Variant(t *testing.T) {
	m := testMaster()
	got := MP4Args(m, MP4Options{CRF: 30, Variant: &Variant{FPS: 12.5, Width: 160}}, "out.mp4")
	wantFilter := "[0:v]fps=12.5:round=down,format=gbrap,premultiply=inplace=1,scale=160:120:flags=lanczos,unpremultiply=inplace=1,format=rgba[c];" +
		"color=c=0x313338:s=160x120:r=12.5,format=rgba[bg];" +
		"[bg][c]overlay=format=auto:shortest=1,pad=ceil(iw/2)*2:ceil(ih/2)*2:color=0x313338" + mp4ColorConvert + "[f]"
	want := []string{
		"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25",
		"-i", "/dev/shm/ezl/job1/frames.rgba",
		"-filter_complex", wantFilter,
		"-map", "[f]",
		"-c:v", "libx264",
		"-crf", "30",
		"-preset", "slow",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-movflags", "+faststart",
		"-an",
		"-f", "mp4",
		"out.mp4",
	}
	assertArgs(t, got, want)
}

func TestWebMArgs(t *testing.T) {
	m := testMaster()
	got := WebMArgs(m, WebMOptions{}, "/data/scratch/j1/out.webm")
	want := []string{
		"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25",
		"-i", "/dev/shm/ezl/job1/frames.rgba",
		"-filter_complex", mp4FlattenDefault,
		"-map", "[f]",
		"-c:v", "libvpx-vp9",
		"-crf", "30",
		"-b:v", "0",
		"-row-mt", "1",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-an",
		"-f", "webm",
		"/data/scratch/j1/out.webm",
	}
	assertArgs(t, got, want)
}

func TestWebMArgs_MatteAndCRF(t *testing.T) {
	m := testMaster()
	got := WebMArgs(m, WebMOptions{CRF: 45, Matte: "ffffff"}, "out.webm")
	want := []string{
		"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25",
		"-i", "/dev/shm/ezl/job1/frames.rgba",
		"-filter_complex", mp4FlattenWhite,
		"-map", "[f]",
		"-c:v", "libvpx-vp9",
		"-crf", "45",
		"-b:v", "0",
		"-row-mt", "1",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-an",
		"-f", "webm",
		"out.webm",
	}
	assertArgs(t, got, want)
}

// TestVideoCRF pins the CRF-verbatim contract: recipe.Output.Quality IS the
// CRF for mp4/webm (no 1..100 mapping), 0 falls back to the format default
// and out-of-range values are capped at the codec maximum.
func TestVideoCRF(t *testing.T) {
	if got := (MP4Options{}).crf(); got != DefaultX264CRF {
		t.Errorf("MP4Options default CRF = %d, want %d", got, DefaultX264CRF)
	}
	if got := (MP4Options{CRF: 26}).crf(); got != 26 {
		t.Errorf("MP4Options CRF = %d, want 26 (verbatim)", got)
	}
	if got := (MP4Options{CRF: 99}).crf(); got != 51 {
		t.Errorf("MP4Options CRF cap = %d, want 51", got)
	}
	if got := (MP4Options{CRF: -1}).crf(); got != DefaultX264CRF {
		t.Errorf("MP4Options negative CRF = %d, want %d", got, DefaultX264CRF)
	}
	if got := (WebMOptions{}).crf(); got != DefaultVP9CRF {
		t.Errorf("WebMOptions default CRF = %d, want %d", got, DefaultVP9CRF)
	}
	if got := (WebMOptions{CRF: 40}).crf(); got != 40 {
		t.Errorf("WebMOptions CRF = %d, want 40 (verbatim)", got)
	}
	if got := (WebMOptions{CRF: 99}).crf(); got != 63 {
		t.Errorf("WebMOptions CRF cap = %d, want 63", got)
	}
}

func TestGifskiArgs(t *testing.T) {
	frames := []string{"/tmp/f/00001.png", "/tmp/f/00002.png", "/tmp/f/00003.png"}
	got := GifskiArgs(frames, 33.333333, 0, 0, "out.gif")
	want := []string{
		"--fps", "33.333333",
		"--quality", "90",
		"-o", "out.gif",
		"/tmp/f/00001.png", "/tmp/f/00002.png", "/tmp/f/00003.png",
	}
	assertArgs(t, got, want)

	got = GifskiArgs(frames[:1], 0, 120, 128, "out.gif")
	want = []string{
		"--fps", "20",
		"--quality", "100",
		"--width", "128",
		"-o", "out.gif",
		"/tmp/f/00001.png",
	}
	assertArgs(t, got, want)

	// fps is capped at gifski's 100 fps limit; quality clamps at 1.
	got = GifskiArgs(frames[:1], 240, -3, 0, "out.gif")
	assertArgs(t, got, []string{"--fps", "100", "--quality", "1", "-o", "out.gif", "/tmp/f/00001.png"})

	if got := GifskiArgs(nil, 25, 90, 0, "out.gif"); got != nil {
		t.Errorf("no frames: got %v, want nil", got)
	}
}

func TestGifsicleFastPathArgs(t *testing.T) {
	delays := []int{10, 10, 5, 5, 10, 10}
	tests := []struct {
		name   string
		delays []int
		o      GifsicleFastPathOptions
		want   []string
	}{
		{
			name: "zero options: plain lossless re-optimise",
			o:    GifsicleFastPathOptions{},
			want: []string{"in.gif", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name: "crop only (no -U: gifsicle crops optimised frames in place)",
			o:    GifsicleFastPathOptions{Crop: &GifsicleCrop{X: 10, Y: 20, W: 64, H: 48}},
			want: []string{"--crop", "10,20+64x48", "in.gif", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name: "frame range implies -U",
			o:    GifsicleFastPathOptions{FrameStart: 2, FrameEnd: 4},
			want: []string{"-U", "in.gif", "#2-4", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name: "open-ended range",
			o:    GifsicleFastPathOptions{FrameStart: 3},
			want: []string{"-U", "in.gif", "#3-", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name: "end-only range",
			o:    GifsicleFastPathOptions{FrameEnd: 3},
			want: []string{"-U", "in.gif", "#0-3", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name:   "drop every 2nd with merged delays",
			delays: delays,
			o:      GifsicleFastPathOptions{DropEveryN: 2},
			want: []string{"-U", "in.gif",
				"--delay", "20", "#0", "--delay", "10", "#2", "--delay", "20", "#4",
				"-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			// The drop counts inside the trimmed range and the emitted indices
			// stay on the source grid: [1..4] = delays 10,5,5,10; every 2nd of
			// those dropped → kept #1 (10+5) and #3 (5+10).
			name:   "trim plus drop",
			delays: delays,
			o:      GifsicleFastPathOptions{FrameStart: 1, FrameEnd: 4, DropEveryN: 2},
			want: []string{"-U", "in.gif",
				"--delay", "15", "#1", "--delay", "15", "#3",
				"-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name: "drop without delays is ignored",
			o:    GifsicleFastPathOptions{DropEveryN: 2},
			want: []string{"in.gif", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name:   "everything combined",
			delays: delays,
			o: GifsicleFastPathOptions{
				Crop:       &GifsicleCrop{X: 0, Y: 0, W: 100, H: 80},
				FrameStart: 0, FrameEnd: 4,
				DropEveryN: 3,
				Loop:       5,
			},
			// [0..4] = 10,10,5,5,10; every 3rd dropped (index 2 of the range)
			// → kept #0 (10), #1 (10+5), #3 (5), #4 (10).
			want: []string{"-U", "--crop", "0,0+100x80", "in.gif",
				"--delay", "10", "#0", "--delay", "15", "#1", "--delay", "5", "#3", "--delay", "10", "#4",
				"-O2", "--careful", "--loopcount=5", "-o", "out.gif"},
		},
		{
			name: "loop count caps at uint16 and negative means forever",
			o:    GifsicleFastPathOptions{Loop: 100000},
			want: []string{"in.gif", "-O2", "--careful", "--loopcount=65535", "-o", "out.gif"},
		},
		{
			name: "invalid crop is skipped",
			o:    GifsicleFastPathOptions{Crop: &GifsicleCrop{X: 4, Y: 4, W: 0, H: 10}, Loop: -2},
			want: []string{"in.gif", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name: "inverted range collapses to the start frame",
			o:    GifsicleFastPathOptions{FrameStart: 4, FrameEnd: 2},
			want: []string{"-U", "in.gif", "#4-4", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			// The delay list doubles as the frame count: FrameEnd past the
			// 6-frame source is clamped to the last frame instead of handing
			// gifsicle a selection it exits non-zero on.
			name:   "FrameEnd past the last frame clamps when delays are known",
			delays: delays,
			o:      GifsicleFastPathOptions{FrameStart: 2, FrameEnd: 20},
			want:   []string{"-U", "in.gif", "#2-5", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			name:   "FrameStart past the last frame clamps when delays are known",
			delays: delays,
			o:      GifsicleFastPathOptions{FrameStart: 20},
			want:   []string{"-U", "in.gif", "#5-", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			// Without delays there is no frame count to clamp against: the
			// range is emitted verbatim (the caller must pass valid indices).
			name: "FrameEnd past the last frame is verbatim without delays",
			o:    GifsicleFastPathOptions{FrameStart: 2, FrameEnd: 20},
			want: []string{"-U", "in.gif", "#2-20", "-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, GifsicleFastPathArgs("in.gif", "out.gif", tc.delays, tc.o), tc.want)
		})
	}
}

// --- bounce guards (Phase 4) --------------------------------------------------

// bounceFilter stands in for the compiler's bounce chain (split + reverse +
// concat after the output fit).
const bounceFilter = "[0:v]fps=25,format=rgba,split[bf][br];[br]reverse[brr];[bf][brr]concat=n=2:v=1:a=0[out]"

// bouncedPlan is testPlan with a bounce op: Duration and Frames doubled, the
// trim still an input seek (the source is seekable).
func bouncedPlan() *graph.Plan {
	p := testPlan()
	p.Filter = bounceFilter
	p.Bounced = true
	p.Duration = 5 // 2 x 2.5
	p.Frames = 124 // 2 x 62
	return p
}

// TestBouncedPlansAreNeverSeeked: a bounced plan's stills decode exactly the
// render's input window — the plan's own "-ss TrimStart -to TrimEnd", never
// a seek-back past TrimStart, no -itsoffset — because the bounce stage
// buffers its input to EOF and mirrors it. The forward select is by absolute
// output time on the DOUBLED timeline (the mirrored half carries the
// continued timestamps), the reversed select by index; the from-start
// variant is the same argv. A bounced plan whose demuxer cannot seek stays
// entirely unseeked, and an untrimmed bounced plan carries no seek args at
// all.
func TestBouncedPlansAreNeverSeeked(t *testing.T) {
	trimSeek := []string{"-ss", "1.5", "-to", "4", "-i", mainSrc}
	tests := []struct {
		name      string
		mod       func(p *graph.Plan)
		t         float64
		maxW      int
		fromStart bool
		want      []string
	}{
		{
			// t=1 → slot 25 of the doubled grid: threshold 24.5/25, pad 25/25+1.
			name: "forward t=1 keeps the trim window only",
			t:    1, maxW: 480,
			want: cat(trimSeek, stillOut(stillFilter(bounceFilter, "2", "0.98", alphaScale480))),
		},
		{
			name: "forward t=0",
			t:    0, maxW: 0,
			want: cat(trimSeek, stillOut(stillFilter(bounceFilter, "1", "0", ""))),
		},
		{
			name: "from start is the same argv",
			t:    1, maxW: 480, fromStart: true,
			want: cat(trimSeek, stillOut(stillFilter(bounceFilter, "2", "0.98", alphaScale480))),
		},
		{
			// t=3 lies in the MIRRORED half (t >= 2.5): slot 75, not clamped
			// to the forward half's end.
			name: "forward t in the mirrored half",
			t:    3, maxW: 0,
			want: cat(trimSeek, stillOut(stillFilter(bounceFilter, "4", "2.98", ""))),
		},
		{
			// Clamped to the doubled render's last slot 123.
			name: "forward t past the doubled end clamps to the last slot",
			t:    99, maxW: 0,
			want: cat(trimSeek, stillOut(stillFilter(bounceFilter, "5.92", "4.9", ""))),
		},
		{
			// Probe reported neither duration nor frame count: the last-slot
			// fallback derives from TrimEnd and must describe the DOUBLED
			// output window — undoubled it would clamp slot 75 to the forward
			// half's last slot 61 (threshold 2.42) instead of 2.98.
			name: "mirrored half with only TrimEnd known is not clamped",
			mod:  func(p *graph.Plan) { p.Duration, p.Frames = 0, 0 },
			t:    3, maxW: 0,
			want: cat(trimSeek, stillOut(stillFilter(bounceFilter, "4", "2.98", ""))),
		},
		{
			// Same fallback, t past the end: clamps to the doubled window's
			// last slot 124 = floor(2*2.5*25)-1 (one past Frames-1 = 123; that
			// slot is a tpad clone of the last frame, pixel-identical).
			name: "past the end with only TrimEnd known clamps to the doubled last slot",
			mod:  func(p *graph.Plan) { p.Duration, p.Frames = 0, 0 },
			t:    99, maxW: 0,
			want: cat(trimSeek, stillOut(stillFilter(bounceFilter, "5.96", "4.94", ""))),
		},
		{
			// Reversed + bounced: index selection, still only the trim window.
			name: "reversed t=1",
			mod:  func(p *graph.Plan) { p.Reversed = true },
			t:    1, maxW: 480,
			want: cat(trimSeek, stillOut(reversedStillFilter(bounceFilter, "2", 25, alphaScale480))),
		},
		{
			name: "reversed from start is the same argv",
			mod:  func(p *graph.Plan) { p.Reversed = true },
			t:    1, maxW: 0, fromStart: true,
			want: cat(trimSeek, stillOut(reversedStillFilter(bounceFilter, "2", 25, ""))),
		},
		{
			// Reversed + bounced with only TrimEnd known: index 75 stays
			// uncapped (an undoubled fallback would cap j at 61).
			name: "reversed mirrored half with only TrimEnd known keeps the index",
			mod:  func(p *graph.Plan) { p.Reversed = true; p.Duration, p.Frames = 0, 0 },
			t:    3, maxW: 0,
			want: cat(trimSeek, stillOut(reversedStillFilter(bounceFilter, "4", 75, ""))),
		},
		{
			// A bounced animated WebP (SeekUnsafe + FilterTrim) is entirely
			// unseeked; the filter's trim stage does the cutting.
			name: "bounced SeekUnsafe plan stays unseeked",
			mod: func(p *graph.Plan) {
				p.InputArgs, p.SeekUnsafe, p.FilterTrim = nil, true, true
			},
			t: 1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(bounceFilter, "2", "0.98", ""))),
		},
		{
			name: "untrimmed bounced plan carries no seek args",
			mod: func(p *graph.Plan) {
				p.InputArgs, p.TrimStart, p.TrimEnd = nil, 0, 0
			},
			t: 1, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(bounceFilter, "2", "0.98", ""))),
		},
		{
			// Fully unknown length (no Duration/Frames/TrimEnd) and a huge t:
			// the SLOT is capped at maxStillIndex = 1<<20 (mirroring
			// reversedSeekFor) — capping seconds instead would let abs reach
			// t*fps and ask tpad/select to grind through tens of millions of
			// cloned frames: threshold (2^20 - 0.5)/25, pad 2^20/25 + 1.
			name: "unknown length huge t caps the slot at maxStillIndex",
			mod: func(p *graph.Plan) {
				p.InputArgs, p.TrimStart, p.TrimEnd = nil, 0, 0
				p.Duration, p.Frames = 0, 0
			},
			t: 1e9, maxW: 0,
			want: cat([]string{"-i", mainSrc}, stillOut(stillFilter(bounceFilter, "41944.04", "41943.02", ""))),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := bouncedPlan()
			if tc.mod != nil {
				tc.mod(p)
			}
			var got []string
			if tc.fromStart {
				got = StillArgsFromStart(mainSrc, p, tc.t, tc.maxW)
			} else {
				got = StillArgs(mainSrc, p, tc.t, tc.maxW)
			}
			assertArgs(t, got, tc.want)
		})
	}
}

// TestProxyArgs_BouncedNeverSeeked: the proxy of a bounced plan — reversed
// or not — passes the plan's InputArgs through unchanged (the trim seek
// stays; a reversed non-bounced plan would get a tail seek instead).
func TestProxyArgs_BouncedNeverSeeked(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		p := bouncedPlan()
		p.Reversed = reversed
		got := ProxyArgs(mainSrc, p, 0, 10, "p.webp")
		want := []string{
			"-ss", "1.5", "-to", "4",
			"-i", mainSrc,
			"-filter_complex", bounceFilter + ";[out]fps=15," + alphaScale360 + "[outp]",
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
		assertArgs(t, got, want)
	}
}
