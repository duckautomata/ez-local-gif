package ffrun

import (
	"reflect"
	"strings"
	"testing"
)

// realSample was captured from `ffmpeg -f lavfi -i testsrc=size=64x64:rate=5
// -frames:v 5 -f null - -progress pipe:1 -nostats -stats_period 0.2`.
const realSample = `frame=5
fps=0.00
stream_0_0_q=-0.0
bitrate=N/A
total_size=N/A
out_time_us=1000000
out_time_ms=1000000
out_time=00:00:01.000000
dup_frames=0
drop_frames=0
speed= 424x
progress=end
`

func collect(t *testing.T, text string) []Progress {
	t.Helper()
	var got []Progress
	if err := ParseProgress(strings.NewReader(text), func(p Progress) { got = append(got, p) }); err != nil {
		t.Fatalf("ParseProgress: %v", err)
	}
	return got
}

func TestParseProgress_RealSample(t *testing.T) {
	got := collect(t, realSample)
	want := []Progress{{Frame: 5, FPS: 0, OutTimeMS: 1000, Speed: "424x", Done: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseProgress_MultiBlock(t *testing.T) {
	text := "frame=0\nfps=0.00\nout_time_us=N/A\nout_time_ms=N/A\nout_time=N/A\nspeed=N/A\nprogress=continue\n" +
		"frame=25\nfps=24.50\nout_time_us=1000000\nout_time_ms=1000000\nout_time=00:00:01.000000\nspeed=0.98x\nprogress=continue\n" +
		"frame=62\nfps=30.10\nout_time_us=2480000\nout_time_ms=2480000\nout_time=00:00:02.480000\nspeed=1.2x\nprogress=end\n"
	got := collect(t, text)
	want := []Progress{
		{Frame: 0, FPS: 0, OutTimeMS: 0, Speed: "N/A", Done: false},
		{Frame: 25, FPS: 24.5, OutTimeMS: 1000, Speed: "0.98x", Done: false},
		{Frame: 62, FPS: 30.1, OutTimeMS: 2480, Speed: "1.2x", Done: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseProgress_NAKeepsPreviousValues(t *testing.T) {
	text := "frame=10\nfps=5.0\nout_time_us=400000\nspeed=1x\nprogress=continue\n" +
		"frame=N/A\nfps=N/A\nout_time_us=N/A\nspeed=N/A\nprogress=end\n"
	got := collect(t, text)
	want := []Progress{
		{Frame: 10, FPS: 5, OutTimeMS: 400, Speed: "1x"},
		{Frame: 10, FPS: 5, OutTimeMS: 400, Speed: "N/A", Done: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseProgress_OutTimeVariants(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int64 // OutTimeMS
	}{
		{
			name: "out_time_us wins over everything",
			text: "out_time_us=1500000\nout_time_ms=999\nout_time=00:00:00.001000\nprogress=continue\n",
			want: 1500,
		},
		{
			name: "older build: out_time_ms only, is really microseconds",
			text: "frame=3\nout_time_ms=2500000\nprogress=continue\n",
			want: 2500,
		},
		{
			name: "out_time_ms as microseconds confirmed by out_time",
			text: "out_time_ms=2500000\nout_time=00:00:02.500000\nprogress=continue\n",
			want: 2500,
		},
		{
			name: "hypothetical build where out_time_ms is real milliseconds",
			text: "out_time_ms=2500\nout_time=00:00:02.500000\nprogress=continue\n",
			want: 2500,
		},
		{
			name: "only the human-readable out_time",
			text: "out_time=01:02:03.250000\nprogress=end\n",
			want: (1*3600+2*60+3)*1000 + 250,
		},
		{
			name: "negative placeholder timestamps are ignored",
			text: "out_time_us=-9223372036854775808\nout_time_ms=-9223372036854775808\nout_time=-2562047788:00:54.775808\nprogress=continue\n",
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(t, tc.text)
			if len(got) != 1 {
				t.Fatalf("got %d blocks, want 1", len(got))
			}
			if got[0].OutTimeMS != tc.want {
				t.Fatalf("OutTimeMS = %d, want %d", got[0].OutTimeMS, tc.want)
			}
		})
	}
}

func TestParseProgress_IgnoresGarbage(t *testing.T) {
	text := "\n\nnot a pair\n=\nframe=abc\nframe=7\n  speed =  2.5x \nprogress=continue\n"
	got := collect(t, text)
	want := []Progress{{Frame: 7, Speed: "2.5x"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseProgress_NoBlockWithoutTerminator(t *testing.T) {
	if got := collect(t, "frame=7\nfps=1.0\n"); len(got) != 0 {
		t.Fatalf("emitted %d blocks for an unterminated block, want 0", len(got))
	}
}

func TestParseOutTime(t *testing.T) {
	tests := []struct {
		in string
		us int64
		ok bool
	}{
		{"00:00:01.000000", 1_000_000, true},
		{"00:01:00.500000", 60_500_000, true},
		{"10:00:00.000001", 36_000_000_001, true},
		{"00:00:00", 0, true},
		{"", 0, false},
		{"N/A", 0, false},
		{"-00:00:01.000000", 0, false},
		{"1:2", 0, false},
		{"aa:bb:cc", 0, false},
	}
	for _, tc := range tests {
		us, ok := parseOutTime(tc.in)
		if us != tc.us || ok != tc.ok {
			t.Errorf("parseOutTime(%q) = (%d, %v), want (%d, %v)", tc.in, us, ok, tc.us, tc.ok)
		}
	}
}

// TestProgressWriter_SplitWrites feeds the same text in awkward chunks and
// expects identical blocks: exec may hand the writer any byte boundaries.
func TestProgressWriter_SplitWrites(t *testing.T) {
	text := "frame=25\nfps=24.50\nout_time_us=1000000\nspeed=0.98x\nprogress=continue\n" +
		"frame=62\nout_time_us=2480000\nspeed=1.2x\nprogress=end\n"
	want := collect(t, text)
	for _, chunk := range []int{1, 2, 3, 7, 16, 1000} {
		var got []Progress
		w := newProgressWriter(func(p Progress) { got = append(got, p) })
		for i := 0; i < len(text); i += chunk {
			end := min(i+chunk, len(text))
			if n, err := w.Write([]byte(text[i:end])); err != nil || n != end-i {
				t.Fatalf("Write returned (%d, %v)", n, err)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("chunk %d: got %+v\nwant %+v", chunk, got, want)
		}
		if w.pending != nil {
			t.Fatalf("chunk %d: pending buffer not released: %q", chunk, w.pending)
		}
	}
}

func TestProgressWriter_BoundsUnterminatedLine(t *testing.T) {
	w := newProgressWriter(func(Progress) {})
	junk := strings.Repeat("x", 3*maxProgressLine)
	if _, err := w.Write([]byte(junk)); err != nil {
		t.Fatal(err)
	}
	if len(w.pending) > maxProgressLine {
		t.Fatalf("pending grew to %d bytes, cap is %d", len(w.pending), maxProgressLine)
	}
}
