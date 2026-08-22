package jobs

// End-to-end check of the frame-index scrubber contract (contract A) on the
// Manager's Still path with real ffmpeg/ffprobe: the UI addresses frame i of
// N = Plan.Frames by asking for the still at its midpoint (i+0.5)/FPS, and
// for the LAST frame (i = N-1) that must yield that frame — not an error,
// not the frame before it — for CFR video, image sequences and stills;
// (N-1.5)/FPS yields the frame before when the two differ, and every t past
// the last midpoint (t == Duration and beyond) collapses onto the same memo
// entry. Skips without ffmpeg/ffprobe.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/probe"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// solidPNG encodes a w x h opaque PNG of one colour.
func solidPNG(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// masterFrames renders the plan's RGBA master and splits it into frames.
func masterFrames(t *testing.T, ctx context.Context, tools ffrun.Tools, srcPath string, p *graph.Plan) [][]byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "frames.rgba")
	if err := ffrun.RunFFmpeg(ctx, tools.FFmpeg, enc.MasterArgs(srcPath, p, out), nil); err != nil {
		t.Fatalf("master render: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	frame := p.Width * p.Height * 4
	if len(data) == 0 || len(data)%frame != 0 {
		t.Fatalf("master is %d bytes, not a multiple of %dx%dx4", len(data), p.Width, p.Height)
	}
	frames := make([][]byte, 0, len(data)/frame)
	for off := 0; off < len(data); off += frame {
		frames = append(frames, data[off:off+frame])
	}
	return frames
}

// pngPix decodes a still PNG to straight RGBA bytes.
func pngPix(t *testing.T, data []byte) (pix []byte, w, h int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode still (%d bytes): %v", len(data), err)
	}
	b := img.Bounds()
	n := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			n.Set(x, y, img.At(x, y))
		}
	}
	return n.Pix, b.Dx(), b.Dy()
}

// closestFrame returns the index of the master frame nearest to pix (sum of
// absolute channel differences) and that distance, so a 1:1 lanczos pass in
// the still chain cannot fail an exact comparison.
func closestFrame(frames [][]byte, pix []byte) (idx, dist int) {
	idx, dist = -1, math.MaxInt
	for i, f := range frames {
		if len(f) != len(pix) {
			continue
		}
		d := 0
		for j := range f {
			if f[j] > pix[j] {
				d += int(f[j] - pix[j])
			} else {
				d += int(pix[j] - f[j])
			}
		}
		if d < dist {
			idx, dist = i, d
		}
	}
	return idx, dist
}

func TestStillFrameIndexContract(t *testing.T) {
	tools := realTools(t)
	st := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	m := NewManager(st, tools, Options{Concurrency: 1})
	out := recipe.Output{Format: "webp"}
	dir := t.TempDir()

	type source struct {
		name string
		hash string
		path string // blob file, or the sequence directory
		ops  []recipe.Op
	}
	var sources []source
	setInfo := func(b *store.Blob, info recipe.ProbeInfo) {
		t.Helper()
		if err := st.SetBlobInfo(b.Hash, info); err != nil {
			t.Fatal(err)
		}
	}

	// CFR video: 12 distinct testsrc frames at 10 fps.
	clip := filepath.Join(dir, "clip.mp4")
	cmd := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=32x24:rate=10:duration=1.2",
		"-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", clip)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build clip: %v\n%s", err, o)
	}
	clipData, err := os.ReadFile(clip)
	if err != nil {
		t.Fatal(err)
	}
	clipBlob, err := st.PutBlob(bytes.NewReader(clipData), "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	clipInfo, err := probe.Probe(ctx, tools, clipBlob.Path, 0)
	if err != nil {
		t.Fatalf("probe clip: %v", err)
	}
	if clipInfo.Kind != recipe.KindVideo || clipInfo.Frames != 12 || clipInfo.FPS != 10 {
		t.Fatalf("clip probe: %+v", clipInfo)
	}
	setInfo(clipBlob, clipInfo)
	sources = append(sources,
		source{"cfr video", clipBlob.Hash, clipBlob.Path, nil},
		// Trimmed from the frame-index scrubber to frames 2..5: start 2/10,
		// end (5+1)/10.
		source{"cfr video trimmed to frames 2..5", clipBlob.Hash, clipBlob.Path, []recipe.Op{{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.2,"end":0.6}`)}}},
		// Resampled: 1.2 s at 24 fps are 28 master frames, the last two both
		// copies of source frame 11.
		source{"cfr video at 24 fps", clipBlob.Hash, clipBlob.Path, []recipe.Op{{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":24}`)}}},
	)

	// Image sequence: 7 frames of distinct solid colours at 100 ms.
	var parts []store.SequencePart
	for i := 1; i <= 7; i++ {
		parts = append(parts, store.SequencePart{Name: fmt.Sprintf("frame%d.png", i), R: bytes.NewReader(solidPNG(t, 16, 12, color.NRGBA{R: uint8(30 * i), G: 90, B: 160, A: 255}))})
	}
	seqBlob, err := st.PutSequence(parts)
	if err != nil {
		t.Fatal(err)
	}
	seqInfo, err := probe.ProbeSequence(ctx, tools, seqBlob.Path, 100)
	if err != nil {
		t.Fatalf("probe sequence: %v", err)
	}
	if seqInfo.Kind != recipe.KindSequence || seqInfo.Frames != 7 || seqInfo.FPS != 10 {
		t.Fatalf("sequence probe: %+v", seqInfo)
	}
	setInfo(seqBlob, seqInfo)
	sources = append(sources,
		source{"sequence", seqBlob.Hash, seqBlob.Path, nil},
		// 33 ms per frame (the review's retiming).
		source{"sequence at 33 ms", seqBlob.Hash, seqBlob.Path, []recipe.Op{{Kind: recipe.OpDelay, Params: json.RawMessage(`{"ms":33}`)}}},
		// Speed 2: 3 master frames (sources 2, 4, 6; the speed stage
		// truncates the end timestamp, see graph.sequenceFrames).
		source{"sequence at speed 2", seqBlob.Hash, seqBlob.Path, []recipe.Op{{Kind: recipe.OpSpeed, Params: json.RawMessage(`{"factor":2}`)}}},
		// Trimmed from the scrubber to frames 3..5.
		source{"sequence trimmed to frames 3..5", seqBlob.Hash, seqBlob.Path, []recipe.Op{{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0.3,"end":0.6}`)}}},
	)

	// A still image.
	stillBlob, err := st.PutBlob(bytes.NewReader(solidPNG(t, 20, 10, color.NRGBA{R: 200, G: 40, B: 40, A: 255})), "one.png")
	if err != nil {
		t.Fatal(err)
	}
	stillInfo, err := probe.Probe(ctx, tools, stillBlob.Path, 0)
	if err != nil {
		t.Fatalf("probe still: %v", err)
	}
	if !stillInfo.IsStill {
		t.Fatalf("still probe: %+v", stillInfo)
	}
	setInfo(stillBlob, stillInfo)
	sources = append(sources, source{"still image", stillBlob.Hash, stillBlob.Path, nil})

	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			blob, err := st.GetBlob(src.hash)
			if err != nil {
				t.Fatal(err)
			}
			p, err := graph.Compile(*blob.Info, src.ops, stillOutput(out))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			frames := masterFrames(t, ctx, tools, src.path, p)
			n := p.Frames
			if len(frames) != n {
				t.Fatalf("master has %d frames, plan says %d (the scrubber's N must be the render's)", len(frames), n)
			}
			fps := p.FPS
			still := func(tt float64) ([]byte, []byte) {
				t.Helper()
				data, err := m.Still(ctx, src.hash, src.ops, out, tt, 0)
				if err != nil {
					t.Fatalf("Still t=%v: %v", tt, err)
				}
				pix, w, h := pngPix(t, data)
				if w != p.Width || h != p.Height {
					t.Fatalf("Still t=%v: %dx%d, want %dx%d", tt, w, h, p.Width, p.Height)
				}
				return data, pix
			}
			// check asserts the still at tt shows master frame want — by
			// content, so a resample that left two master frames identical
			// (copies of one source frame) passes for either index.
			check := func(tt float64, want int) []byte {
				t.Helper()
				data, pix := still(tt)
				idx, dist := closestFrame(frames, pix)
				if idx < 0 || !bytes.Equal(frames[idx], frames[want]) {
					t.Errorf("t=%v: still is master frame %d (distance %d), want %d of %d", tt, idx, dist, want, n)
				}
				return data
			}
			lastMid := (float64(n) - 0.5) / fps
			last := check(lastMid, n-1)
			if n >= 2 {
				prev := check((float64(n)-1.5)/fps, n-2)
				if bytes.Equal(frames[n-1], frames[n-2]) {
					t.Logf("the last two master frames are identical (resample); both midpoints show that frame")
				} else if bytes.Equal(last, prev) {
					t.Errorf("the stills at (N-0.5)/fps and (N-1.5)/fps are the same image although the frames differ")
				}
			}
			// Everything past the last midpoint is the last frame, served from
			// the same memo entry (byte-identical PNG).
			for _, tt := range []float64{p.Duration, p.Duration + 5, float64(n) / fps, 1e6} {
				if data, _ := still(tt); !bytes.Equal(data, last) {
					t.Errorf("t=%v: not the memoised last-frame still", tt)
				}
			}
			check(0.5/fps, 0)
			check(0, 0)
		})
	}
}
