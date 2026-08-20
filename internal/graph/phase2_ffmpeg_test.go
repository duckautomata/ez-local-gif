package graph_test

// Real-ffmpeg checks of the Phase 2 source heads: image sequences (image2
// input args, the delay op, trim seeks, the per-frame format/size guard under
// -reinit_filter 0 and the mixed-size normalisation) and separate alpha
// streams (AVIF: alphamerge head, the hoisted unpremultiply right after it).
// External test package so internal/graph stays process-free. Skips when
// ffmpeg is not on PATH; the AVIF part also needs the libaom-av1 encoder and
// a software AV1 decoder.
//
// Why the sequence head looks the way it does (all verified on FFmpeg 9.0.1):
// fftools rebuilds the filtergraph whenever ANY decoded frame parameter
// changes — size, pixel format or colour range — and the rebuild loses the
// frame the fps filter is holding: six mixed frames rendered as six copies of
// the last one, both for mixed sizes and for same-size frames mixing
// rgba/rgb24/pal8 (which the probe reports as Mixed=false, since it only
// compares sizes). Every sequence therefore runs with -reinit_filter 0 behind
// a guarding scale: one graph sees every frame, scale reconfigures by itself
// and converts each frame to the negotiated format (a bare format=rgba is
// only a negotiation constraint — a mid-sequence rgb24 frame passed through
// it reinterpreted as rgba garbage), and it passes already-conforming frames
// through pixel-exact. For mixed sizes pad re-centres with eval=frame
// (otherwise the uncovered canvas is uninitialised memory), and premultiply
// reads the configured size (silent corruption), so the head scales straight
// alpha and the unpremultiply follows at the constant parameters.

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// seqFrameCount frames are written per test sequence.
const seqFrameCount = 6

// seqCanvas is the largest frame size of the test sequences.
const seqCanvas = 64

// writeSeqFrames writes frames 000001.png … 00000N.png into dir. Frame i
// (1-based) is sizes[(i-1)%len(sizes)] pixels, colour R = 40*i, G = 200,
// B = 0, alpha 128 on the left half and 255 on the right half, so frame
// identity (R), order, padding (alpha 0) and the alpha halves are all
// checkable from single pixels.
func writeSeqFrames(t *testing.T, dir string, sizes [][2]int) {
	t.Helper()
	for i := 1; i <= seqFrameCount; i++ {
		w, h := sizes[(i-1)%len(sizes)][0], sizes[(i-1)%len(sizes)][1]
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				a := uint8(255)
				if x < w/2 {
					a = 128
				}
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(40 * i), G: 200, A: a})
			}
		}
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("%06d.png", i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFmtFrames writes seqFrameCount same-size (seqCanvas square) frames
// whose PNG colour type — and so the decoded pixel format — varies per frame:
// kinds[(i-1)%len(kinds)] is "rgba" (truecolour + alpha, alpha 128 on the
// left half and 255 on the right), "rgb24" (opaque truecolour; Go's png
// encoder drops the alpha channel of a fully opaque image) or "pal8"
// (indexed). Colours follow writeSeqFrames (frame i: R = 40*i, G = 200,
// B = 0), so drops, dups and reordering show in single pixels.
func writeFmtFrames(t *testing.T, dir string, kinds []string) {
	t.Helper()
	for i := 1; i <= seqFrameCount; i++ {
		kind := kinds[(i-1)%len(kinds)]
		var img image.Image
		switch kind {
		case "rgba", "rgb24":
			m := image.NewNRGBA(image.Rect(0, 0, seqCanvas, seqCanvas))
			for y := 0; y < seqCanvas; y++ {
				for x := 0; x < seqCanvas; x++ {
					a := uint8(255)
					if kind == "rgba" && x < seqCanvas/2 {
						a = 128
					}
					m.SetNRGBA(x, y, color.NRGBA{R: uint8(40 * i), G: 200, A: a})
				}
			}
			img = m
		case "pal8":
			// Index 0 (the zero value of Pix) is the frame colour.
			img = image.NewPaletted(image.Rect(0, 0, seqCanvas, seqCanvas), color.Palette{
				color.NRGBA{R: uint8(40 * i), G: 200, A: 255},
				color.NRGBA{A: 255},
			})
		default:
			t.Fatalf("unknown frame kind %q", kind)
		}
		path := filepath.Join(dir, fmt.Sprintf("%06d.png", i))
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if got := pngPixFmt(t, path); got != kind {
			t.Fatalf("frame %d encoded as %s, want %s (Go png encoder behaviour changed?)", i, got, kind)
		}
	}
}

// pngPixFmt reports what ffmpeg will decode a PNG file as, from its colour
// type: "rgb24" (truecolour), "rgba" (truecolour + alpha) or "pal8" (indexed).
func pngPixFmt(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	switch cfg.ColorModel {
	case color.RGBAModel:
		return "rgb24"
	case color.NRGBAModel:
		return "rgba"
	}
	if _, ok := cfg.ColorModel.(color.Palette); ok {
		return "pal8"
	}
	return fmt.Sprintf("%T", cfg.ColorModel)
}

// seqInfo is what probe.ProbeSequence reports for a test sequence.
func seqInfo(mixed bool) recipe.ProbeInfo {
	return recipe.ProbeInfo{
		Format: "image2", Codec: "png", PixFmt: "rgba", Bits: 8,
		Width: seqCanvas, Height: seqCanvas, FPS: 10, Duration: float64(seqFrameCount) / 10, Frames: seqFrameCount,
		HasAlpha: true, Kind: recipe.KindSequence,
		Sequence: &recipe.SequenceInfo{Count: seqFrameCount, Pattern: "%06d.png", DelayMS: 100, Mixed: mixed},
	}
}

// renderFrames runs enc.MasterArgs for the plan (src is the source file, or
// the sequence directory for InputPattern plans) and returns every RGBA frame
// of the rawvideo master.
func renderFrames(t *testing.T, ff, src string, p *graph.Plan) [][]byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "frames.rgba")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, enc.MasterArgs(src, p, out)...)
	if o, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, o)
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
	for i := 0; i+frame <= len(data); i += frame {
		frames = append(frames, data[i:i+frame])
	}
	return frames
}

// pixel returns the RGBA bytes at x,y of a frame that is w pixels wide.
func pixel(frame []byte, w, x, y int) [4]byte {
	o := (y*w + x) * 4
	return [4]byte{frame[o], frame[o+1], frame[o+2], frame[o+3]}
}

// near reports whether got is within tol of want.
func near(got, want byte, tol int) bool {
	d := int(got) - int(want)
	return d >= -tol && d <= tol
}

// checkSeqFrame checks that frame is sequence frame n (1-based): R = 40*n
// and G = 200 at the canvas centre, i.e. no drop, dup or colour bleed.
func checkSeqFrame(t *testing.T, frame []byte, index, n int) {
	t.Helper()
	c := pixel(frame, seqCanvas, seqCanvas/2, seqCanvas/2)
	if !near(c[0], byte(40*n), 2) || !near(c[1], 200, 2) || c[2] != 0 {
		t.Errorf("frame %d: centre pixel %v, want sequence frame %d (R=%d G=200 B=0)", index, c, n, 40*n)
	}
}

func TestSequencePixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	uniformSizes := [][2]int{{seqCanvas, seqCanvas}}
	mixedSizes := [][2]int{{seqCanvas, seqCanvas}, {32, 48}, {48, 20}}
	out := recipe.Output{Format: "webp"}

	t.Run("uniform sequence: one master frame per image, in order", func(t *testing.T) {
		dir := t.TempDir()
		writeSeqFrames(t, dir, uniformSizes)
		p := compile(t, seqInfo(false), nil, out)
		if want := []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}; !reflect.DeepEqual(p.InputArgs, want) {
			t.Fatalf("InputArgs %q, want %q", p.InputArgs, want)
		}
		if want := "[0:v]scale=64:64:force_original_aspect_ratio=decrease:flags=lanczos,fps=10:round=down,format=rgba[out]"; p.Filter != want {
			t.Fatalf("filter %q, want %q", p.Filter, want)
		}
		if p.InputPattern != "%06d.png" || p.Frames != seqFrameCount {
			t.Fatalf("InputPattern %q Frames %d", p.InputPattern, p.Frames)
		}
		frames := renderFrames(t, ff, dir, p)
		if len(frames) != seqFrameCount {
			t.Fatalf("master has %d frames, want %d", len(frames), seqFrameCount)
		}
		for i, f := range frames {
			checkSeqFrame(t, f, i, i+1)
			if l, r := pixel(f, seqCanvas, 8, 32), pixel(f, seqCanvas, 56, 32); l[3] != 128 || r[3] != 255 {
				t.Errorf("frame %d: alpha halves %d/%d, want 128/255", i, l[3], r[3])
			}
		}
	})

	t.Run("delay op and trim pick frames on the retimed grid", func(t *testing.T) {
		dir := t.TempDir()
		writeSeqFrames(t, dir, uniformSizes)
		// 40 ms per frame = 25 fps; 0.08 s..0.2 s covers frames 3, 4, 5.
		ops := []recipe.Op{{Kind: recipe.OpDelay, Params: []byte(`{"ms":40}`)}, {Kind: recipe.OpTrim, Params: []byte(`{"start":0.08,"end":0.2}`)}}
		p := compile(t, seqInfo(false), ops, out)
		if want := []string{"-f", "image2", "-framerate", "25", "-start_number", "1", "-reinit_filter", "0", "-ss", "0.08", "-to", "0.2"}; !reflect.DeepEqual(p.InputArgs, want) {
			t.Fatalf("InputArgs %q, want %q", p.InputArgs, want)
		}
		if p.Frames != 3 || p.SourceFPS != 25 || p.FPS != 25 {
			t.Fatalf("plan Frames %d SourceFPS %v FPS %v, want 3/25/25", p.Frames, p.SourceFPS, p.FPS)
		}
		frames := renderFrames(t, ff, dir, p)
		if len(frames) != 3 {
			t.Fatalf("master has %d frames, want 3 (frames 3..5)", len(frames))
		}
		for i, f := range frames {
			checkSeqFrame(t, f, i, i+3)
		}
	})

	// GE-1 regression: same-size frames whose pixel format differs per frame.
	// The probe only compares sizes, so SequenceInfo.Mixed stays false; without
	// -reinit_filter 0 + the guarding scale head fftools rebuilt the graph at
	// every format change and the master came out as copies of the last frame
	// (or with frames dropped and duplicated).
	for _, tc := range []struct {
		name  string
		kinds []string
	}{
		{"rgba first, alternating rgb24", []string{"rgba", "rgb24"}},
		{"rgb24 first, alternating rgba", []string{"rgb24", "rgba"}},
		{"one pal8 frame among rgba", []string{"rgba", "rgba", "pal8", "rgba", "rgba", "rgba"}},
	} {
		t.Run("uniform size, mixed pixel formats: "+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFmtFrames(t, dir, tc.kinds)
			p := compile(t, seqInfo(false), nil, out)
			frames := renderFrames(t, ff, dir, p)
			if len(frames) != seqFrameCount {
				t.Fatalf("master has %d frames, want %d (did a format change rebuild the graph?)", len(frames), seqFrameCount)
			}
			for i, f := range frames {
				checkSeqFrame(t, f, i, i+1)
				// The guard must convert each frame, not reinterpret its
				// bytes: opaque frames stay alpha 255 everywhere, rgba
				// frames keep their 128/255 halves.
				l, r := pixel(f, seqCanvas, 8, 32), pixel(f, seqCanvas, 56, 32)
				if tc.kinds[i%len(tc.kinds)] == "rgba" {
					if l[3] != 128 || r[3] != 255 {
						t.Errorf("frame %d: alpha halves %d/%d, want 128/255", i, l[3], r[3])
					}
				} else if l[3] != 255 || r[3] != 255 {
					t.Errorf("frame %d: alpha %d/%d, want opaque (255/255)", i, l[3], r[3])
				}
			}
		})
	}

	t.Run("mixed sizes: every frame centred on the largest canvas, none dropped or duplicated", func(t *testing.T) {
		dir := t.TempDir()
		writeSeqFrames(t, dir, mixedSizes)
		p := compile(t, seqInfo(true), nil, out)
		if want := []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}; !reflect.DeepEqual(p.InputArgs, want) {
			t.Fatalf("InputArgs %q, want %q", p.InputArgs, want)
		}
		if !strings.HasPrefix(p.Filter, "[0:v]scale=64:64:force_original_aspect_ratio=decrease:flags=lanczos,format=rgba,pad=64:64:(ow-iw)/2:(oh-ih)/2:color=0x00000000:eval=frame,") {
			t.Fatalf("filter: %s", p.Filter)
		}
		frames := renderFrames(t, ff, dir, p)
		if len(frames) != seqFrameCount {
			t.Fatalf("master has %d frames, want %d", len(frames), seqFrameCount)
		}
		for i, f := range frames {
			checkSeqFrame(t, f, i, i+1)
			switch i % 3 {
			case 0: // 64x64: untouched
				if tl, br := pixel(f, seqCanvas, 0, 0), pixel(f, seqCanvas, 63, 63); tl[3] != 128 || br[3] != 255 {
					t.Errorf("frame %d: corners alpha %d/%d, want 128/255 (full-size frame must not be padded)", i, tl[3], br[3])
				}
			case 1: // 32x48 → 43x64, centred horizontally: transparent columns left and right
				if l, r := pixel(f, seqCanvas, 1, 32), pixel(f, seqCanvas, 62, 32); l[3] != 0 || r[3] != 0 {
					t.Errorf("frame %d: side padding alpha %d/%d, want 0/0 (pad must re-centre per frame)", i, l[3], r[3])
				}
				if c := pixel(f, seqCanvas, 20, 32); c[3] == 0 {
					t.Errorf("frame %d: content pixel is transparent", i)
				}
			case 2: // 48x20 → 64x27, centred vertically: transparent rows top and bottom
				if tp, bt := pixel(f, seqCanvas, 32, 5), pixel(f, seqCanvas, 32, 60); tp[3] != 0 || bt[3] != 0 {
					t.Errorf("frame %d: top/bottom padding alpha %d/%d, want 0/0 (pad must re-centre per frame)", i, tp[3], bt[3])
				}
				if c := pixel(f, seqCanvas, 32, 32); c[3] == 0 {
					t.Errorf("frame %d: content pixel is transparent", i)
				}
			}
		}
	})

	t.Run("mixed sizes: the hoisted unpremultiply runs after the head at the constant size", func(t *testing.T) {
		dir := t.TempDir()
		writeSeqFrames(t, dir, mixedSizes)
		p := compile(t, seqInfo(true), []recipe.Op{{Kind: recipe.OpUnpremultiply}}, out)
		if !strings.Contains(p.Filter, ":eval=frame,format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,") {
			t.Fatalf("filter: %s", p.Filter)
		}
		frames := renderFrames(t, ff, dir, p)
		if len(frames) != seqFrameCount {
			t.Fatalf("master has %d frames, want %d", len(frames), seqFrameCount)
		}
		// Frame 1 is full size: its left half (alpha 128) is divided by the
		// alpha, 40*255/128 = 80; its right half (alpha 255) is unchanged.
		l, r := pixel(frames[0], seqCanvas, 8, 32), pixel(frames[0], seqCanvas, 56, 32)
		if !near(l[0], 80, 2) || l[3] != 128 || !near(r[0], 40, 2) || r[3] != 255 {
			t.Errorf("frame 0 after unpremultiply: left %v right %v, want R 80/40 with alpha 128/255", l, r)
		}
		// And the later (rescaled) frames still arrive in order: a right-half
		// (alpha 255) content pixel keeps the frame's own R.
		for i, f := range frames {
			x := 56
			if i%3 == 1 { // 43 px wide, centred: content spans x 10..52
				x = 40
			}
			if c := pixel(f, seqCanvas, x, 32); !near(c[0], byte(40*(i+1)), 2) || c[3] != 255 {
				t.Errorf("frame %d: right-half pixel %v, want R=%d alpha 255", i, c, 40*(i+1))
			}
		}
	})
}

// --- AVIF with a separate alpha stream ---------------------------------------

// hasCodec reports whether `ffmpeg -encoders|-decoders` lists name.
func hasCodec(t *testing.T, ff, list, name string) bool {
	t.Helper()
	out, err := exec.Command(ff, "-hide_banner", list).Output()
	if err != nil {
		t.Fatalf("ffmpeg %s: %v", list, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			return true
		}
	}
	return false
}

// avifWithAlpha encodes pngPath as a still AVIF through ffmpeg's avif muxer
// two-stream path: stream 0 = colour (yuv420p), stream 1 = alpha (gray), the
// layout the mov demuxer exposes as [0:v:0] / [0:v:1]. The extracted alpha
// plane inherits the PNG's RGB (identity) matrix tag, which libaom refuses
// for a 4:2:0/monochrome stream, so it is retagged bt709 full range first.
func avifWithAlpha(t *testing.T, ff, pngPath, avifPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", pngPath, "-frames:v", "1",
		"-filter_complex", "[0:v]split[c][a];[a]alphaextract,setparams=colorspace=bt709:range=pc[al]",
		"-map", "[c]", "-map", "[al]",
		"-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "10",
		"-pix_fmt:v:0", "yuv420p", "-pix_fmt:v:1", "gray",
		"-f", "avif", avifPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encode avif: %v\n%s", err, out)
	}
}

// avifInfo is what probe reports for the still AVIF with alpha.
var avifInfo = recipe.ProbeInfo{
	Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "av1", PixFmt: "yuv420p", Bits: 8,
	Width: discSize, Height: discSize, Frames: 1, HasAlpha: true, IsStill: true, Kind: recipe.KindImage,
	AlphaStream: 1,
}

// edgeStatsWindow is edgeStats with a custom alpha window.
func edgeStatsWindow(frame []byte, minA, maxA int) (n, minR, maxR int) {
	minR = 256
	for i := 0; i+3 < len(frame); i += 4 {
		a := int(frame[i+3])
		if a < minA || a > maxA {
			continue
		}
		r := int(frame[i])
		n++
		minR, maxR = min(minR, r), max(maxR, r)
	}
	return n, minR, maxR
}

func TestAlphaStreamAVIFPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	if !hasCodec(t, ff, "-encoders", "libaom-av1") {
		t.Skip("ffmpeg has no libaom-av1 encoder")
	}
	if !hasCodec(t, ff, "-decoders", "libdav1d") && !hasCodec(t, ff, "-decoders", "libaom-av1") {
		t.Skip("ffmpeg has no software AV1 decoder")
	}
	dir := t.TempDir()

	// Straight grey disc: the colour plane is uniformly 128 (also under the
	// transparent pixels), only the alpha carries the disc — lossy-safe.
	straight := filepath.Join(dir, "straight.avif")
	discPNG(t, filepath.Join(dir, "straight.png"), 128, false)
	avifWithAlpha(t, ff, filepath.Join(dir, "straight.png"), straight)
	// Premultiplied white disc: the colour plane is the disc matted on black
	// (edge R ~ A), what the unpremultiply toggle must undo.
	pre := filepath.Join(dir, "pre.avif")
	discPNG(t, filepath.Join(dir, "pre.png"), 255, true)
	avifWithAlpha(t, ff, filepath.Join(dir, "pre.png"), pre)

	unpre := []recipe.Op{{Kind: recipe.OpUnpremultiply}}
	webpOut := recipe.Output{Format: "webp"}

	t.Run("alphamerge head brings the alpha stream into the master", func(t *testing.T) {
		p := compile(t, avifInfo, nil, webpOut)
		if p.Filter != "[0:v:0]format=rgba[c];[0:v:1]format=gray[a];[c][a]alphamerge,format=rgba[out]" {
			t.Fatalf("filter: %s", p.Filter)
		}
		frames := renderFrames(t, ff, straight, p)
		if len(frames) != 1 {
			t.Fatalf("master has %d frames, want 1", len(frames))
		}
		f := frames[0]
		corner, centre := pixel(f, discSize, 0, 0), pixel(f, discSize, discSize/2, discSize/2)
		if corner[3] > 16 || centre[3] < 240 {
			t.Errorf("alpha corner %d centre %d, want ~0 / ~255 (the alpha stream was not merged)", corner[3], centre[3])
		}
		if n, _, _ := edgeStats(f); n < 20 {
			t.Errorf("only %d anti-aliased edge pixels", n)
		}
		for i := 0; i+3 < len(f); i += 4 {
			if !near(f[i], 128, 12) {
				t.Errorf("pixel %d: R=%d, want ~128 everywhere (colour must come from stream 0 untouched)", i/4, f[i])
				break
			}
		}
		// Control: without the head the master is the opaque colour stream.
		plain := compile(t, with(avifInfo, func(p *recipe.ProbeInfo) { p.AlphaStream = 0 }), nil, webpOut)
		for i, f := 3, renderFrames(t, ff, straight, plain)[0]; i < len(f); i += 4 {
			if f[i] != 255 {
				t.Fatalf("without the alphamerge head the master should be opaque, pixel %d alpha %d", i/4, f[i])
			}
		}
	})

	t.Run("hoisted unpremultiply right after the merge restores straight colour", func(t *testing.T) {
		// Sanity: merged as-is the premultiplied edge is dark (R ~ A).
		asIs := renderFrames(t, ff, pre, compile(t, avifInfo, nil, webpOut))[0]
		if n, _, maxR := edgeStatsWindow(asIs, 64, 192); n < 20 || maxR > 230 {
			t.Fatalf("premultiplied avif without unpremultiply: %d edge pixels, max R %d; expected dark edges (R ~ A)", n, maxR)
		}
		p := compile(t, avifInfo, unpre, webpOut)
		if !strings.HasPrefix(p.Filter, "[0:v:0]format=rgba[c];[0:v:1]format=gray[a];[c][a]alphamerge,format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,") {
			t.Fatalf("filter: %s", p.Filter)
		}
		f := renderFrames(t, ff, pre, p)[0]
		n, minR, maxR := edgeStatsWindow(f, 64, 192)
		t.Logf("%d edge pixels, R in [%d,%d] (filter %s)", n, minR, maxR, p.Filter)
		if n < 20 {
			t.Fatalf("only %d anti-aliased edge pixels", n)
		}
		if minR <= 200 {
			t.Errorf("edge pixel R=%d after unpremultiply, want > 200 (the toggle is a no-op after alphamerge?)", minR)
		}
	})

	t.Run("merge, unpremultiply, then the premultiplied scale chain", func(t *testing.T) {
		p := compile(t, avifInfo, unpre, recipe.Output{Format: "webp", Width: 32, Height: 32})
		if !strings.HasSuffix(p.Filter, "unpremultiply=inplace=1,format=gbrap,premultiply=inplace=1,scale=32:32:flags=lanczos,unpremultiply=inplace=1,format=rgba[out]") {
			t.Fatalf("filter: %s", p.Filter)
		}
		frames := renderFrames(t, ff, pre, p)
		if len(frames) != 1 {
			t.Fatalf("master has %d frames, want 1", len(frames))
		}
		n, minR, maxR := edgeStatsWindow(frames[0], 64, 192)
		t.Logf("%d edge pixels after scaling, R in [%d,%d]", n, minR, maxR)
		if n < 8 {
			t.Fatalf("only %d anti-aliased edge pixels after scaling", n)
		}
		if minR <= 200 {
			t.Errorf("edge pixel R=%d after unpremultiply+scale, want > 200", minR)
		}
	})
}

// with returns a copy of src with fn applied (external-package twin of the
// helper in graph_test.go).
func with(src recipe.ProbeInfo, fn func(*recipe.ProbeInfo)) recipe.ProbeInfo {
	fn(&src)
	return src
}
