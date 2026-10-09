package enc_test

// Phase 5b: the per-consumer matte input forms against a real ffmpeg (skips
// without one on PATH). A hand-built plan merges a numbered gray matte
// sequence (frame i is the flat value matteGray(i)) into a 30 fps test clip
// at 25 fps, so a frame's alpha names the matte it was paired with:
//
//   - the master (full sequence) carries matte i on frame i;
//   - a forward still's ONE unlooped PNG (pts 0) pairs with the selected
//     frame whatever the seek-back — the still equals the master's frame —
//     and the clamp covers a one-frame Plan.Frames overshoot;
//   - a reversed still / proxy after a CFR tail seek reads the sequence
//     from -start_number K+1 and equals the master / the unseeked proxy;
//   - bounced and [reverse, bounce] stills read the full sequence and equal
//     the master's frame in both halves;
//   - MatteSourceArgs streams exactly frames x N x N x 3 bytes.

import (
	"bytes"
	"image"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
)

const (
	matteFPS    = 25
	matteFrames = clipSeconds * matteFPS // 100: fps=25 over the 4 s clip
	// matteMergeChain is the compiler's opaque-frame merge of input 1 at the
	// clip size (spec §5.2), without the terminal label.
	matteMergeChain = "[0:v]fps=25,format=rgba[m1];[1:v]format=gray,scale=160:120:flags=bicubic[m1a];[m1][m1a]alphamerge,format=rgba"
)

// matteGray is the flat value of matte i (0-based): unique for i < 216.
func matteGray(i int) uint8 { return uint8(40 + i) }

// grayMattes writes n flat gray mattes 000001.png … to dir and returns the
// image2 pattern path.
func grayMattes(t *testing.T, dir string, n int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		img := image.NewGray(image.Rect(0, 0, clipW, clipH))
		for k := range img.Pix {
			img.Pix[k] = matteGray(i)
		}
		writePNG(t, filepath.Join(dir, matteFile(i+1)), img)
	}
	return filepath.Join(dir, "%06d.png")
}

// matteFile is the file name of matte n (1-based).
func matteFile(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 6-len(s)) + s + ".png"
}

// mattePlan is the hand-built plan of the 30 fps clip at 25 fps merged with
// the matte sequence at pattern, with tail appended to the merge chain
// (e.g. ",reverse") and the flags the compiler would set.
func mattePlan(pattern, tail string, frames int) *graph.Plan {
	return &graph.Plan{
		Filter:    matteMergeChain + tail + "[out]",
		OutLabel:  "[out]",
		Width:     clipW,
		Height:    clipH,
		FPS:       matteFPS,
		HasAlpha:  true,
		Duration:  clipSeconds,
		Frames:    matteFrames,
		Speed:     1,
		SourceFPS: clipFPS,
		ExtraInputs: []graph.ExtraInput{{
			Source: 0,
			Path:   pattern,
			Args:   []string{"-f", "image2", "-framerate", "25", "-start_number", "1"},
			Matte:  &graph.MatteInput{Model: "isnet-anime", Size: 64, FPS: "25", Frames: frames},
		}},
	}
}

// alphaAt returns the alpha of pixel (x, y) of an rgba frame.
func alphaAt(frame []byte, x, y int) uint8 { return frame[(y*clipW+x)*4+3] }

// matteInputOf returns the matte input's args (after the main "-i", before
// the output options: a still's "-frames:v 1", else -filter_complex).
func matteInputOf(args []string) []string {
	in := slices.Index(args, "-i")
	end := slices.Index(args, "-frames:v")
	if end < 0 {
		end = slices.Index(args, "-filter_complex")
	}
	return args[in+2 : end]
}

func TestMatteInputForms(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	clip, _ := cfrClip(t, ff, dir)
	pattern := grayMattes(t, filepath.Join(dir, "m"), matteFrames)

	t.Run("master carries matte i on frame i", func(t *testing.T) {
		frames := renderMaster(t, ff, clip, mattePlan(pattern, "", matteFrames))
		if len(frames) != matteFrames {
			t.Fatalf("master has %d frames, want %d", len(frames), matteFrames)
		}
		for i, fr := range frames {
			for _, pt := range [][2]int{{0, 0}, {80, 60}, {159, 119}} {
				if a := alphaAt(fr, pt[0], pt[1]); a != matteGray(i) {
					t.Fatalf("master frame %d alpha %d at %v, want %d", i, a, pt, matteGray(i))
				}
			}
		}
	})

	t.Run("forward still: one unlooped PNG of the selected slot", func(t *testing.T) {
		p := mattePlan(pattern, "", matteFrames)
		frames := renderMaster(t, ff, clip, p)
		for _, tt := range []float64{0, 0.3, 1, 2.2, 3.5, 3.96, 4, 99} {
			j := int(math.Min(math.Floor(tt*matteFPS+1e-6), matteFrames-1))
			for _, fromStart := range []bool{false, true} {
				var args []string
				if fromStart {
					args = enc.StillArgsFromStart(clip, p, tt, 0)
				} else {
					args = enc.StillArgs(clip, p, tt, 0)
				}
				in := matteInputOf(args)
				want := []string{"-f", "image2", "-framerate", "25", "-i", filepath.Join(dir, "m", matteFile(j+1))}
				if !slices.Equal(in, want) {
					t.Fatalf("t=%v fromStart=%v: matte input %q, want %q", tt, fromStart, in, want)
				}
				img := decodePNG(t, run(t, ff, args))
				if got := frameIndex(frames, img); got != j {
					t.Errorf("t=%v fromStart=%v: still is master frame %d (alpha %d), want %d", tt, fromStart, got, img.Pix[3], j)
				}
			}
		}
	})

	t.Run("forward still: a one-frame Plan.Frames overshoot is clamped", func(t *testing.T) {
		// The estimate says 101 frames; the memo (and the clip) have 100: the
		// still at the end selects slot 100 (a tpad clone of frame 99) and
		// must read the LAST matte, not a missing 000101.png.
		p := mattePlan(pattern, "", matteFrames)
		p.Frames = matteFrames + 1
		frames := renderMaster(t, ff, clip, mattePlan(pattern, "", matteFrames))
		args := enc.StillArgs(clip, p, 99, 0)
		if in := matteInputOf(args); !slices.Contains(in, filepath.Join(dir, "m", matteFile(matteFrames))) {
			t.Fatalf("matte input %q, want the last file", in)
		}
		img := decodePNG(t, run(t, ff, args))
		if got := frameIndex(frames, img); got != matteFrames-1 {
			t.Errorf("still is master frame %d, want %d", got, matteFrames-1)
		}
	})

	t.Run("reversed still after a CFR tail seek: -start_number K+1", func(t *testing.T) {
		p := mattePlan(pattern, ",reverse", matteFrames)
		p.Reversed = true
		frames := renderMaster(t, ff, clip, p)
		if len(frames) != matteFrames {
			t.Fatalf("reversed master has %d frames, want %d", len(frames), matteFrames)
		}
		for j, fr := range frames {
			if a := alphaAt(fr, 80, 60); a != matteGray(matteFrames-1-j) {
				t.Fatalf("reversed master frame %d alpha %d, want %d", j, a, matteGray(matteFrames-1-j))
			}
		}
		seeks := 0
		for _, tt := range []float64{0, 0.5, 1, 2.2, 3.5, 3.9, 4} {
			j := int(math.Min(math.Floor(tt*matteFPS+1e-6), matteFrames-1))
			args := enc.StillArgs(clip, p, tt, 0)
			in := matteInputOf(args)
			k := slices.Index(in, "-start_number")
			if k < 0 || !slices.Contains(in, pattern) {
				t.Fatalf("t=%v: matte input %q, want the sequence", tt, in)
			}
			if slices.Contains(args, "-ss") {
				seeks++
				if in[k+1] == "1" {
					t.Errorf("t=%v: seeked (%q) but the sequence starts at 1", tt, args[:4])
				}
			} else if in[k+1] != "1" {
				t.Errorf("t=%v: unseeked but the sequence starts at %s", tt, in[k+1])
			}
			img := decodePNG(t, run(t, ff, args))
			if got := frameIndex(frames, img); got != j {
				t.Errorf("t=%v: still is reversed master frame %d (alpha %d), want %d (%q)", tt, got, img.Pix[3], j, in)
			}
		}
		if seeks == 0 {
			t.Error("no still was seeked: the CFR tail seek did not run")
		}
	})

	t.Run("bounced stills read the full sequence in both halves", func(t *testing.T) {
		for _, rev := range []bool{false, true} {
			tail := ",split[bf][br];[br]reverse[brr];[bf][brr]concat=n=2:v=1:a=0"
			if rev {
				tail = ",reverse" + tail
			}
			p := mattePlan(pattern, tail, matteFrames)
			p.Bounced, p.Bounces, p.Reversed = true, 1, rev
			p.Duration, p.Frames = 2*clipSeconds, 2*matteFrames
			frames := renderMaster(t, ff, clip, p)
			if len(frames) != 2*matteFrames {
				t.Fatalf("rev=%v: bounced master has %d frames, want %d", rev, len(frames), 2*matteFrames)
			}
			for _, tt := range []float64{0, 1, 3.96, 4, 5, 7.9} {
				j := int(math.Min(math.Floor(tt*matteFPS+1e-6), 2*matteFrames-1))
				args := enc.StillArgs(clip, p, tt, 0)
				in := matteInputOf(args)
				if k := slices.Index(in, "-start_number"); k < 0 || in[k+1] != "1" || !slices.Contains(in, pattern) {
					t.Fatalf("rev=%v t=%v: matte input %q, want the full sequence", rev, tt, in)
				}
				img := decodePNG(t, run(t, ff, args))
				if !bytes.Equal(img.Pix, frames[j]) {
					t.Errorf("rev=%v t=%v: still (alpha %d) is not bounced master frame %d (alpha %d)", rev, tt, img.Pix[3], j, alphaAt(frames[j], 0, 0))
				}
			}
		}
	})

	t.Run("reversed proxy tail: -start_number K+1 equals the unseeked proxy", func(t *testing.T) {
		p := mattePlan(pattern, ",reverse", matteFrames)
		p.Reversed = true
		out := filepath.Join(t.TempDir(), "proxy.webp")
		args := enc.ProxyArgs(clip, p, 0, 1.5, out)
		in := matteInputOf(args)
		k := slices.Index(in, "-start_number")
		if !slices.Contains(args, "-ss") || k < 0 || in[k+1] == "1" {
			t.Fatalf("proxy argv %q: want a tail seek with a shifted sequence", args[:slices.Index(args, "-filter_complex")])
		}
		run(t, ff, args)
		frames := decodeWebP(t, ff, out, clipW, clipH)
		base := filepath.Join(t.TempDir(), "base.webp")
		baseArgs := enc.ProxyArgs(clip, p, 0, 1.5, base)
		// The unseeked form: no seek, the full sequence, the same output.
		baseArgs = append(slices.Clone(p.InputArgs), baseArgs[slices.Index(baseArgs, "-i"):]...)
		bi := slices.Index(baseArgs, "-start_number")
		baseArgs[bi+1] = "1"
		run(t, ff, baseArgs)
		baseFrames := decodeWebP(t, ff, base, clipW, clipH)
		if len(frames) != len(baseFrames) || len(frames) == 0 {
			t.Fatalf("seeked proxy has %d frames, the unseeked one %d", len(frames), len(baseFrames))
		}
		for i := range frames {
			if !bytes.Equal(frames[i], baseFrames[i]) {
				t.Errorf("seeked proxy frame %d differs from the unseeked proxy's (alpha %d vs %d)", i, alphaAt(frames[i], 80, 60), alphaAt(baseFrames[i], 80, 60))
			}
		}
	})

	t.Run("forward proxy reads the full sequence", func(t *testing.T) {
		p := mattePlan(pattern, "", matteFrames)
		out := filepath.Join(t.TempDir(), "proxy.webp")
		run(t, ff, enc.ProxyArgs(clip, p, 0, 10, out))
		frames := decodeWebP(t, ff, out, clipW, clipH)
		if want := clipSeconds * 15; len(frames) < want-1 || len(frames) > want+1 {
			t.Errorf("proxy has %d frames, want about %d", len(frames), want)
		}
	})

	t.Run("MatteSourceArgs streams frames x N x N x 3 bytes", func(t *testing.T) {
		p := &graph.Plan{Filter: "[0:v]fps=25,format=rgba[out]", OutLabel: "[out]", Width: clipW, Height: clipH, FPS: matteFPS, Duration: clipSeconds, Frames: matteFrames, Speed: 1, SourceFPS: clipFPS}
		const size = 64
		out := run(t, ff, enc.MatteSourceArgs(clip, p, size))
		if len(out) != matteFrames*size*size*3 {
			t.Errorf("stream is %d bytes, want %d x %d x %d x 3 = %d", len(out), matteFrames, size, size, matteFrames*size*size*3)
		}
	})
}
