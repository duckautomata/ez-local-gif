package graph_test

// Real-ffmpeg check of the GIF frame-rate rule behind graph.SnapFPS: a
// 30 fps source rendered through the master → GIFArgs pipeline must come out
// with 3,4,3 cs delays (ffmpeg's gif muxer rounds each pts to its 1/100 s
// timebase, Bresenham for free), an exact total duration and one GIF frame
// per master frame — so no 100/n snapping (30 → 33.333, which duplicated 1
// in 9 frames) is needed. It also shows why the cap is 50: 60 fps input
// yields 1 cs delays. Skips when ffmpeg is not on PATH.

import (
	"bytes"
	"context"
	"image/gif"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// runFF executes ffmpeg with the ffrun-style prefix; a non-zero exit is fatal.
func runFF(t *testing.T, ff string, args []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	full := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.CommandContext(ctx, ff, full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(full, " "), err, out)
	}
}

// testsrcClip encodes a seconds-long testsrc clip at rate fps (every frame
// distinct) with the native mpeg4 encoder and returns its path + probe info.
func testsrcClip(t *testing.T, ff, dir string, fps, seconds int) (string, recipe.ProbeInfo) {
	t.Helper()
	path := filepath.Join(dir, "clip"+strconv.Itoa(fps)+".mp4")
	runFF(t, ff, []string{
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=" + strconv.Itoa(fps) + ":duration=" + strconv.Itoa(seconds),
		"-c:v", "mpeg4", "-q:v", "3", path,
	})
	info := recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "mpeg4", PixFmt: "yuv420p", Bits: 8,
		Width: 64, Height: 48, FPS: float64(fps), Duration: float64(seconds), Frames: fps * seconds,
		Kind: recipe.KindVideo,
	}
	return path, info
}

// renderGIF runs the real pipeline (MasterArgs → GIFArgs) for the plan and
// returns the decoded GIF (delays are the GCE values in centiseconds).
func renderGIF(t *testing.T, ff, src string, p *graph.Plan) *gif.GIF {
	t.Helper()
	dir := t.TempDir()
	masterPath := filepath.Join(dir, "frames.rgba")
	runFF(t, ff, enc.MasterArgs(src, p, masterPath))
	data, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	frame := p.Width * p.Height * 4
	if len(data) == 0 || len(data)%frame != 0 {
		t.Fatalf("master is %d bytes, not a multiple of %dx%dx4", len(data), p.Width, p.Height)
	}
	m := enc.Master{Path: masterPath, Width: p.Width, Height: p.Height, FPS: p.FPS, Frames: len(data) / frame}
	out := filepath.Join(dir, "out.gif")
	runFF(t, ff, enc.GIFArgs(m, enc.GIFOptions{}, out))
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode gif: %v", err)
	}
	if len(g.Image) != m.Frames {
		t.Errorf("gif has %d frames, master has %d (a frame was dropped or duplicated)", len(g.Image), m.Frames)
	}
	return g
}

// TestFPSDropNeverLengthensClip is the J2 regression: an exactly-5.0 s
// source rendered through the master pipeline at the sticker ladder's
// fractional rates (16.7, 12.5 fps) must not come out longer than 5.0 s.
// The fps stage's round=down floors the frame count (83, 62) where the
// filter's default rounding rounded the end time up (84, 63 frames =
// 5.03/5.04 s) and silently broke the Discord sticker 5 s cap.
func TestFPSDropNeverLengthensClip(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	clip, info := testsrcClip(t, ff, dir, 25, 5)
	for _, tc := range []struct {
		fps    float64
		frames int
	}{
		{16.7, 83},
		{12.5, 62},
	} {
		p := compile(t, info, nil, recipe.Output{Format: "apng", FPS: tc.fps})
		if p.Frames != tc.frames {
			t.Errorf("%v fps: plan Frames = %d, want %d", tc.fps, p.Frames, tc.frames)
		}
		masterPath := filepath.Join(t.TempDir(), "frames.rgba")
		runFF(t, ff, enc.MasterArgs(clip, p, masterPath))
		data, err := os.ReadFile(masterPath)
		if err != nil {
			t.Fatal(err)
		}
		got := len(data) / (p.Width * p.Height * 4)
		if got != tc.frames {
			t.Errorf("%v fps: master has %d frames, plan predicted %d", tc.fps, got, tc.frames)
		}
		if dur := float64(got) / p.FPS; dur > 5.0+1e-9 {
			t.Errorf("%v fps: master plays %.3f s (> 5.0 s)", tc.fps, dur)
		}
	}
}

func TestGIFDelaysAtCappedRates(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()

	t.Run("30 fps master gets 3,4,3 cs delays with an exact total", func(t *testing.T) {
		clip, info := testsrcClip(t, ff, dir, 30, 2)
		p := compile(t, info, nil, recipe.Output{Format: "gif"})
		if p.FPS != 30 {
			t.Fatalf("plan FPS = %v, want 30 (SnapFPS must not snap 30 fps gif)", p.FPS)
		}
		g := renderGIF(t, ff, clip, p)
		if len(g.Delay) != 60 {
			t.Fatalf("got %d frames, want 60 (2 s at 30 fps)", len(g.Delay))
		}
		total := 0
		for i, d := range g.Delay {
			total += d
			if d != 3 && d != 4 {
				t.Errorf("frame %d: delay %d cs, want 3 or 4", i, d)
			}
			// Bresenham: every three consecutive frames span exactly 10 cs.
			if i%3 == 2 && g.Delay[i-2]+g.Delay[i-1]+d != 10 {
				t.Errorf("frames %d..%d: delays %v sum to %d cs, want 10", i-2, i, g.Delay[i-2:i+1], g.Delay[i-2]+g.Delay[i-1]+d)
			}
		}
		if total != 200 {
			t.Errorf("total duration %d cs, want exactly 200", total)
		}
		threes, fours := 0, 0
		for _, d := range g.Delay {
			if d == 3 {
				threes++
			} else {
				fours++
			}
		}
		if threes != 40 || fours != 20 {
			t.Errorf("delay mix: %d x 3 cs, %d x 4 cs; want 40 and 20", threes, fours)
		}
		t.Logf("30 fps: delays %v...", g.Delay[:9])
	})

	t.Run("50 fps (the cap) gets uniform 2 cs delays", func(t *testing.T) {
		clip, info := testsrcClip(t, ff, dir, 50, 1)
		p := compile(t, info, nil, recipe.Output{Format: "gif"})
		if p.FPS != 50 {
			t.Fatalf("plan FPS = %v, want 50", p.FPS)
		}
		g := renderGIF(t, ff, clip, p)
		if len(g.Delay) != 50 {
			t.Fatalf("got %d frames, want 50", len(g.Delay))
		}
		for i, d := range g.Delay {
			if d != 2 {
				t.Errorf("frame %d: delay %d cs, want 2", i, d)
			}
		}
	})

	t.Run("60 fps source is capped at 50 for gif, so no delay is below 2 cs", func(t *testing.T) {
		clip, info := testsrcClip(t, ff, dir, 60, 1)
		p := compile(t, info, nil, recipe.Output{Format: "gif"})
		if p.FPS != 50 {
			t.Fatalf("plan FPS = %v, want 50 (60 fps source capped)", p.FPS)
		}
		g := renderGIF(t, ff, clip, p)
		if len(g.Delay) != 50 {
			t.Fatalf("got %d frames, want 50", len(g.Delay))
		}
		for i, d := range g.Delay {
			if d < 2 {
				t.Errorf("frame %d: delay %d cs (< 2 cs)", i, d)
			}
		}
		// Counter-example (bypassing SnapFPS): a 60 fps master really does
		// produce 1 cs delays, which is why the cap exists.
		m := enc.Master{Path: filepath.Join(t.TempDir(), "raw60.rgba"), Width: 64, Height: 48, FPS: 60}
		runFF(t, ff, []string{"-i", clip, "-vf", "format=rgba", "-f", "rawvideo", "-pix_fmt", "rgba", m.Path})
		out := filepath.Join(t.TempDir(), "sixty.gif")
		runFF(t, ff, enc.GIFArgs(m, enc.GIFOptions{}, out))
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		g60, err := gif.DecodeAll(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		short := 0
		for _, d := range g60.Delay {
			if d < 2 {
				short++
			}
		}
		if short == 0 {
			t.Errorf("an uncapped 60 fps master produced no 1 cs delays (%v...); the 50 fps cap may be unnecessary", g60.Delay[:min(9, len(g60.Delay))])
		}
		t.Logf("uncapped 60 fps: %d of %d delays are 1 cs (%v...)", short, len(g60.Delay), g60.Delay[:min(9, len(g60.Delay))])
	})
}
