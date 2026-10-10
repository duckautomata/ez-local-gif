package graph_test

// Real-ffmpeg pixel checks of the Phase 5a keying changes (DESIGN.md §4.3;
// docs/background-removal-proposal.md §5.3, §5.4). External test package
// like the other *_ffmpeg_test.go files; skips when ffmpeg is not on PATH.
//
//   - chromakey emits its key color as the BT.601 limited-range YUV of the
//     RGB key (graph.LimitedYUV, yuv=1) behind an rgba pass into the pinned
//     yuva444p/bt470bg/tv format, so the EXACT screen color keys at
//     similarity 0.02 — for green, blue and two arbitrary colors, from an
//     RGB-decoded source, from bt709-tagged yuv420p sources (tv and full
//     range: the screen-capture MP4 cases) and from a bt709-tagged ProRes
//     4444 (an alpha source, through the key wrapper). Measured on the
//     2026-08 git build before the fix: an RGB key went through ffmpeg's
//     full-range macros (green → U 44 V 21 against the frame's 54/35), a
//     fixed 0.047 offset; and a bare format=yuva444p kept a bt709 source's
//     own matrix through the rgba pass (U 42 V 27, 0.04 off) because the
//     colorspace negotiation carries the tag across RGB links — neither
//     keyed below ~0.05. The pin fixes both; every source below yields the
//     601 values and keys at 0.02.
//   - morph close fills a 1-px hole in the matte, keeps a 3x3 hole and the
//     silhouette, and leaves the color planes byte-identical; grow 1 adds
//     exactly one ring (corners included: coordinates=255 is the full
//     8-neighbourhood).
//   - three stacked colorkeys intersect: each removes its own color and the
//     earlier mattes survive the later keys.

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

const (
	kpSize = 32 // keyProbe frame size
	kpX0   = 10 // subject square origin (both axes) …
	kpW    = 12 // … and side; 2-aligned so 4:2:0 chroma is exact
)

// keyProbe is a kpSize x kpSize opaque frame of the screen color with a
// kpW x kpW subject square at (kpX0, kpX0): the picture taggedYUVClip draws
// with lavfi, as a PNG.
func keyProbe(screen, subject color.NRGBA) *image.NRGBA {
	img := solid(kpSize, kpSize, screen)
	for y := kpX0; y < kpX0+kpW; y++ {
		for x := kpX0; x < kpX0+kpW; x++ {
			img.SetNRGBA(x, y, subject)
		}
	}
	return img
}

// hexNRGBA parses an RRGGBB color.
func hexNRGBA(t *testing.T, hex string) color.NRGBA {
	t.Helper()
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b); err != nil {
		t.Fatalf("color %q: %v", hex, err)
	}
	return color.NRGBA{R: r, G: g, B: b, A: 255}
}

// taggedYUVClip encodes the keyProbe picture (lavfi color + drawbox, so the
// clip is yuv-made end to end like a capture) as a 2-frame yuv420p clip
// tagged with colorspace cs and range rng ("tv" / "pc") — ffv1 in matroska:
// native and lossless in every build, and the container carries the tags so
// every decoded frame reports them (ffprobe -show_frames: color_space=bt709,
// color_range=tv|pc).
func taggedYUVClip(t *testing.T, ff, dir, name, screen, subject, cs, rng string) (string, recipe.ProbeInfo) {
	t.Helper()
	path := filepath.Join(dir, name+".mkv")
	runFF(t, ff, []string{
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=0x%s:s=%dx%d:r=10:d=0.2,drawbox=x=%d:y=%d:w=%d:h=%d:color=0x%s:t=fill",
			screen, kpSize, kpSize, kpX0, kpX0, kpW, kpW, subject),
		"-pix_fmt", "yuv420p", "-color_range", rng, "-colorspace", cs, "-color_primaries", "bt709", "-color_trc", "bt709",
		"-c:v", "ffv1", path,
	})
	info := recipe.ProbeInfo{
		Format: "matroska,webm", Codec: "ffv1", PixFmt: "yuv420p", Bits: 8,
		Width: kpSize, Height: kpSize, FPS: 10, Duration: 0.2, Frames: 2, Kind: recipe.KindVideo,
	}
	return path, info
}

// rawFrame decodes the first frame of clip — through vf when not empty — as
// pixfmt and returns its bytes.
func rawFrame(t *testing.T, ff, clip, vf, pixfmt string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "frame.raw")
	args := []string{"-i", clip, "-frames:v", "1"}
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	runFF(t, ff, append(args, "-f", "rawvideo", "-pix_fmt", pixfmt, out))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// yuv709 is the BT.709 Y'CbCr triple of an RGB color at tv or full range —
// what a bt709-tagged capture stores for it — so a test clip can be checked
// to carry the encoding its tag claims.
func yuv709(c color.NRGBA, full bool) (y, u, v int) {
	rf, gf, bf := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	l := 0.2126*rf + 0.7152*gf + 0.0722*bf
	cb, cr := (bf-l)/1.8556, (rf-l)/1.5748
	clamp := func(v float64) int { return min(max(int(math.Round(v)), 0), 255) } // full-range blue rounds to 256
	if full {
		return clamp(255 * l), clamp(128 + 255*cb), clamp(128 + 255*cr)
	}
	return clamp(16 + 219*l), clamp(128 + 224*cb), clamp(128 + 224*cr)
}

// chromaStage is the chromakey text the compiler emits for key (its
// LimitedYUV hex) at similarity sim and the default blend.
func chromaStage(yuvHex, sim string) string {
	return "format=rgba,format=yuva444p:color_spaces=bt470bg:color_ranges=tv,chromakey=color=0x" + yuvHex + ":similarity=" + sim + ":blend=0.05:yuv=1"
}

// checkKeyed asserts a keyProbe keyed at similarity 0.02: alpha 0 on every
// screen pixel at least margin px from the subject (chromakey averages a
// 3x3 neighbourhood, so the ring touching the subject is legitimately
// opaque; a lossy source needs a wider margin), alpha 255 and the subject
// color (tol per channel: yuv round trips, 4:2:0 chroma) on every subject
// pixel at least 2 px inside it.
func checkKeyed(t *testing.T, frames [][]byte, subject color.NRGBA, margin, tol int, name string) {
	t.Helper()
	if len(frames) != 2 {
		t.Fatalf("%s: %d frames, want 2", name, len(frames))
	}
	for i, f := range frames {
		bad := 0
		for y := 0; y < kpSize; y++ {
			for x := 0; x < kpSize; x++ {
				px := pixel(f, kpSize, x, y)
				inSubject := x >= kpX0+2 && x < kpX0+kpW-2 && y >= kpX0+2 && y < kpX0+kpW-2
				nearSubject := x >= kpX0-margin && x < kpX0+kpW+margin && y >= kpX0-margin && y < kpX0+kpW+margin
				var problem string
				switch {
				case inSubject && (px[3] != 255 || !near(px[0], subject.R, tol) || !near(px[1], subject.G, tol) || !near(px[2], subject.B, tol)):
					problem = fmt.Sprintf("subject (%d,%d) = %v, want opaque %v", x, y, px, subject)
				case !nearSubject && px[3] != 0:
					problem = fmt.Sprintf("screen (%d,%d) = %v, want alpha 0 (the exact key color did not key at similarity 0.02)", x, y, px)
				}
				if problem != "" {
					if bad++; bad <= 3 {
						t.Errorf("%s frame %d: %s", name, i, problem)
					}
				}
			}
		}
		if bad > 3 {
			t.Errorf("%s frame %d: %d bad pixels in all", name, i, bad)
		}
	}
}

func TestChromaKeyExactColorPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	out := recipe.Output{Format: "webp"}
	colors := []struct{ name, key, subject, yuv string }{
		{"green", "00ff00", "ff0000", "913622"},
		{"blue", "0000ff", "ff0000", "29f06e"},
		{"custom 1e3a8a", "1e3a8a", "ff0000", "42a76e"},
		{"custom facc82", "facc82", "00ff00", "c45999"},
	}
	keyOp := func(key string) []recipe.Op {
		return []recipe.Op{{Kind: recipe.OpChromaKey, Params: []byte(fmt.Sprintf(`{"color":"%s","similarity":0.02}`, key))}}
	}
	for _, c := range colors {
		key, subject := hexNRGBA(t, c.key), hexNRGBA(t, c.subject)
		t.Run(c.name+" from an RGB-decoded png source", func(t *testing.T) {
			clip, info := pngClip(t, ff, dir, "rgb-"+c.key, []image.Image{keyProbe(key, subject), keyProbe(key, subject)}, 10, false)
			p := compileSrcs(t, []recipe.ProbeInfo{info}, keyOp(c.key), out)
			if !p.HasAlpha || !strings.Contains(p.Filter, "fps=10:round=down,"+chromaStage(c.yuv, "0.02")) {
				t.Fatalf("plan: alpha %v filter %s", p.HasAlpha, p.Filter)
			}
			// What the pinned format gives the frame is the key the compiler
			// emitted (within 1: swscale's fixed point vs LimitedYUV).
			yuv := rawFrame(t, ff, clip, "format=rgba,format=yuva444p:color_spaces=bt470bg:color_ranges=tv", "yuva444p")
			var wy, wu, wv int
			fmt.Sscanf(c.yuv, "%02x%02x%02x", &wy, &wu, &wv)
			if gy, gu, gv := yuv[0], yuv[kpSize*kpSize], yuv[2*kpSize*kpSize]; !near(gy, byte(wy), 1) || !near(gu, byte(wu), 1) || !near(gv, byte(wv), 1) {
				t.Errorf("pinned yuva444p of the screen = Y%d U%d V%d, want the emitted key Y%d U%d V%d (+-1)", gy, gu, gv, wy, wu, wv)
			}
			checkKeyed(t, p3Render(t, ff, clip, p, nil, nil), subject, 2, 3, c.name+" rgb")
		})
		for _, rng := range []string{"tv", "pc"} {
			t.Run(c.name+" from a bt709-tagged yuv420p source, "+rng+" range", func(t *testing.T) {
				clip, info := taggedYUVClip(t, ff, dir, "bt709-"+rng+"-"+c.key, c.key, c.subject, "bt709", rng)
				// The clip must really store the bt709 encoding its tag
				// claims (+-3: lavfi's draw rounds), else this would exercise
				// nothing: a 601-made 709-tagged clip is a different bug.
				raw := rawFrame(t, ff, clip, "", "yuv420p")
				wy, wu, wv := yuv709(key, rng == "pc")
				if gy, gu, gv := raw[0], raw[kpSize*kpSize], raw[kpSize*kpSize+kpSize*kpSize/4]; !near(gy, byte(wy), 3) || !near(gu, byte(wu), 3) || !near(gv, byte(wv), 3) {
					t.Fatalf("test clip stores Y%d U%d V%d for the screen, want the bt709 %s-range encoding Y%d U%d V%d: the clip does not exercise a bt709 source", gy, gu, gv, rng, wy, wu, wv)
				}
				p := compileSrcs(t, []recipe.ProbeInfo{info}, keyOp(c.key), out)
				if !p.HasAlpha || !strings.Contains(p.Filter, "fps=10:round=down,"+chromaStage(c.yuv, "0.02")) {
					t.Fatalf("plan: alpha %v filter %s", p.HasAlpha, p.Filter)
				}
				checkKeyed(t, p3Render(t, ff, clip, p, nil, nil), subject, 2, 6, c.name+" bt709 "+rng)
			})
		}
	}

	t.Run("green from a bt709-tagged ProRes 4444 source: an alpha source, keyed through the wrapper", func(t *testing.T) {
		if !hasCodec(t, ff, "-encoders", "prores_ks") {
			t.Skip("ffmpeg has no prores_ks encoder")
		}
		green, red := hexNRGBA(t, "00ff00"), hexNRGBA(t, "ff0000")
		// The bottom four rows are transparent (screen color at alpha 0, so
		// the chroma around them stays uniform): the wrapper must keep them
		// at 0 while the opaque screen is keyed.
		img := keyProbe(green, red)
		for y := kpSize - 4; y < kpSize; y++ {
			for x := 0; x < kpSize; x++ {
				img.SetNRGBA(x, y, color.NRGBA{G: 255})
			}
		}
		png, mov := filepath.Join(dir, "pro.png"), filepath.Join(dir, "pro.mov")
		writePNG(t, png, img)
		runFF(t, ff, []string{"-loop", "1", "-framerate", "10", "-i", png, "-frames:v", "2",
			"-c:v", "prores_ks", "-profile:v", "4444", "-pix_fmt", "yuva444p10le",
			"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", mov})
		info := recipe.ProbeInfo{
			Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p12le", Bits: 12,
			Width: kpSize, Height: kpSize, FPS: 10, Duration: 0.2, Frames: 2, HasAlpha: true, Kind: recipe.KindVideo,
		}
		p := compileSrcs(t, []recipe.ProbeInfo{info}, keyOp("00ff00"), out)
		if !strings.Contains(p.Filter, "[0:v]format=rgba,fps=10:round=down,split[k1][k1m];[k1m]alphaextract[k1a0];[k1]"+chromaStage("913622", "0.02")) {
			t.Fatalf("filter: %s", p.Filter)
		}
		frames := p3Render(t, ff, mov, p, nil, nil)
		// ProRes is lossy: a 3 px margin around the subject's DCT ringing.
		checkKeyed(t, frames, red, 3, 12, "prores bt709")
		for i, f := range frames {
			if px := pixel(f, kpSize, 2, kpSize-2); px[3] != 0 {
				t.Errorf("frame %d: transparent source pixel %v, want alpha 0 through the wrapper", i, px)
			}
		}
	})
}

// --- morph ------------------------------------------------------------------

const mpSize = 24

// morphProbe is a transparent mpSize x mpSize frame with an opaque red 12x12
// square at [6,18), a 1-px hole at (9,9) and a 3x3 hole at [13,16)². The
// color bytes under the transparency are distinctive ((1,2,3) around,
// (7,77,177) in the holes) so "color planes untouched" covers them too.
func morphProbe() *image.NRGBA {
	img := solid(mpSize, mpSize, color.NRGBA{R: 1, G: 2, B: 3})
	for y := 6; y < 18; y++ {
		for x := 6; x < 18; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	hole := color.NRGBA{R: 7, G: 77, B: 177}
	img.SetNRGBA(9, 9, hole)
	for y := 13; y < 16; y++ {
		for x := 13; x < 16; x++ {
			img.SetNRGBA(x, y, hole)
		}
	}
	return img
}

// alphaPlane extracts the alpha bytes of a w x h RGBA frame.
func alphaPlane(f []byte, w, h int) []byte {
	a := make([]byte, w*h)
	for i := range a {
		a[i] = f[i*4+3]
	}
	return a
}

// morph3 is the reference 3x3 max (dilate) / min (erode) of a plane with
// edge replication, what ffmpeg's dilation / erosion compute with
// coordinates=255.
func morph3(a []byte, w, h int, dilate bool) []byte {
	out := make([]byte, len(a))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := a[y*w+x]
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					n := a[min(max(y+dy, 0), h-1)*w+min(max(x+dx, 0), w-1)]
					if dilate {
						v = max(v, n)
					} else {
						v = min(v, n)
					}
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

// morphCross is the reference 4-neighbour (cross) max / min, what ffmpeg's
// dilation / erosion compute with coordinates=90 (top, left, right, bottom).
func morphCross(a []byte, w, h int, dilate bool) []byte {
	out := make([]byte, len(a))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := a[y*w+x]
			for _, d := range [][2]int{{0, -1}, {-1, 0}, {1, 0}, {0, 1}} {
				n := a[min(max(y+d[1], 0), h-1)*w+min(max(x+d[0], 0), w-1)]
				if dilate {
					v = max(v, n)
				} else {
					v = min(v, n)
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

func TestMorphPixels(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	out := recipe.Output{Format: "webp"}
	const w, h = mpSize, mpSize
	frame := morphProbe()
	clip, info := pngClip(t, ff, dir, "morph", []image.Image{frame, frame}, 10, true)
	plain := p3Render(t, ff, clip, compileSrcs(t, []recipe.ProbeInfo{info}, nil, out), nil, nil)
	if len(plain) != 2 {
		t.Fatalf("%d plain frames, want 2", len(plain))
	}
	src := alphaPlane(plain[0], w, h)
	// Sanity: the decode carries the probe's alpha as drawn.
	if src[9*w+9] != 0 || src[14*w+14] != 0 || src[6*w+6] != 255 || src[5*w+5] != 0 || src[17*w+17] != 255 {
		t.Fatalf("decoded source alpha is not the probe's: hole %d, 3x3 hole %d, corner %d, outside %d", src[9*w+9], src[14*w+14], src[6*w+6], src[5*w+5])
	}
	dilate := func(a []byte) []byte { return morph3(a, w, h, true) }
	erode := func(a []byte) []byte { return morph3(a, w, h, false) }
	dilateX := func(a []byte) []byte { return morphCross(a, w, h, true) }
	at := func(a []byte, x, y int) byte { return a[y*w+x] }
	// stage wraps the alpha stages as compiler.morph emits them on this
	// rgba-less chain: the alpha alone is extracted, refined and merged back.
	stage := func(alpha string) string {
		return "format=rgba,split[e1][e1a];[e1a]alphaextract," + alpha + "[e1m];[e1][e1m]alphamerge,format=rgba"
	}

	cases := []struct {
		name   string
		params string
		stage  string
		want   func([]byte) []byte // nil: no exact reference, the facts decide
		facts  func(t *testing.T, a []byte)
	}{
		{
			"close fills the 1-px hole, keeps the 3x3 hole and the silhouette", `{"close":true}`,
			stage("dilation,erosion"),
			func(a []byte) []byte { return erode(dilate(a)) },
			func(t *testing.T, a []byte) {
				if at(a, 9, 9) != 255 {
					t.Errorf("1-px hole at (9,9) alpha %d, want 255 (filled)", at(a, 9, 9))
				}
				for _, pt := range [][2]int{{13, 13}, {14, 14}, {15, 15}, {14, 13}} {
					if v := at(a, pt[0], pt[1]); v != 0 {
						t.Errorf("3x3 hole pixel (%d,%d) alpha %d, want 0 (a close fills at most 1 px)", pt[0], pt[1], v)
					}
				}
				for _, pt := range [][2]int{{5, 5}, {5, 12}, {12, 5}, {18, 18}, {18, 12}} {
					if v := at(a, pt[0], pt[1]); v != 0 {
						t.Errorf("outside pixel (%d,%d) alpha %d, want 0 (close must not grow the silhouette)", pt[0], pt[1], v)
					}
				}
				for _, pt := range [][2]int{{6, 6}, {17, 17}, {6, 17}, {12, 6}} {
					if v := at(a, pt[0], pt[1]); v != 255 {
						t.Errorf("square pixel (%d,%d) alpha %d, want 255 (close must not shrink the silhouette)", pt[0], pt[1], v)
					}
				}
			},
		},
		{
			"grow 1 adds exactly one ring", `{"grow":1}`,
			stage("dilation=coordinates=255"),
			dilate,
			func(t *testing.T, a []byte) {
				for _, pt := range [][2]int{{5, 5}, {5, 12}, {18, 18}, {18, 5}, {12, 18}} {
					if v := at(a, pt[0], pt[1]); v != 255 {
						t.Errorf("ring pixel (%d,%d) alpha %d, want 255 (one ring, corners included)", pt[0], pt[1], v)
					}
				}
				for _, pt := range [][2]int{{4, 4}, {4, 12}, {19, 19}, {12, 19}} {
					if v := at(a, pt[0], pt[1]); v != 0 {
						t.Errorf("pixel (%d,%d) alpha %d, want 0 (exactly one ring)", pt[0], pt[1], v)
					}
				}
				if at(a, 9, 9) != 255 || at(a, 13, 13) != 255 || at(a, 14, 14) != 0 {
					t.Errorf("holes after grow 1: pinhole %d (want 255), 3x3 hole ring %d (want 255), its centre %d (want 0)", at(a, 9, 9), at(a, 13, 13), at(a, 14, 14))
				}
			},
		},
		{
			"close then grow 2: the second ring is a cross, the corner is cut", `{"close":true,"grow":2}`,
			stage("dilation,erosion,dilation=coordinates=255,dilation=coordinates=90"),
			func(a []byte) []byte { return dilateX(dilate(erode(dilate(a)))) },
			func(t *testing.T, a []byte) {
				if at(a, 4, 5) != 255 || at(a, 5, 4) != 255 || at(a, 4, 4) != 0 || at(a, 3, 5) != 0 || at(a, 14, 14) != 255 {
					t.Errorf("two rings: (4,5) %d (5,4) %d want 255, corner (4,4) %d and (3,5) %d want 0 (octagon), 3x3 hole centre %d want 255",
						at(a, 4, 5), at(a, 5, 4), at(a, 4, 4), at(a, 3, 5), at(a, 14, 14))
				}
			},
		},
		{
			"shrink 1 trims exactly one ring off the edge", `{"grow":-1}`,
			stage("erosion=coordinates=255"),
			erode,
			func(t *testing.T, a []byte) {
				for _, pt := range [][2]int{{6, 6}, {6, 12}, {17, 17}, {12, 17}} {
					if v := at(a, pt[0], pt[1]); v != 0 {
						t.Errorf("edge pixel (%d,%d) alpha %d, want 0 (one ring trimmed)", pt[0], pt[1], v)
					}
				}
				for _, pt := range [][2]int{{7, 7}, {16, 8}, {7, 12}, {12, 7}} {
					if v := at(a, pt[0], pt[1]); v != 255 {
						t.Errorf("pixel (%d,%d) alpha %d, want 255 (exactly one ring)", pt[0], pt[1], v)
					}
				}
				if at(a, 8, 8) != 0 || at(a, 10, 10) != 0 {
					t.Errorf("the 1-px hole widens to 3x3: (8,8) %d (10,10) %d, want 0", at(a, 8, 8), at(a, 10, 10))
				}
			},
		},
		{
			"soft edge 1.5: corners rounded, the outline stays crisp", `{"smooth":1.5}`,
			stage("gblur=sigma=1.5,lut=y=(val-128)*3+128"),
			nil,
			func(t *testing.T, a []byte) {
				if v := at(a, 2, 2); v != 0 {
					t.Errorf("far outside (2,2) alpha %d, want 0", v)
				}
				if v := at(a, 16, 8); v != 255 {
					t.Errorf("interior (16,8) alpha %d, want 255", v)
				}
				// the straight edge keeps its place: the pixel on it stays
				// mostly opaque, the one just outside mostly transparent
				if in, outside := at(a, 12, 6), at(a, 12, 5); in < 160 || outside > 96 {
					t.Errorf("edge mid (12,6) alpha %d want >= 160, outside (12,5) %d want <= 96", in, outside)
				}
				// the corner is rounded off: weaker than the straight edge
				if c, e := at(a, 6, 6), at(a, 12, 6); c >= e {
					t.Errorf("corner (6,6) alpha %d not below the edge mid %d: not rounded", c, e)
				}
				// the 1-px pinhole is smoothed away
				if v := at(a, 9, 9); v < 200 {
					t.Errorf("pinhole (9,9) alpha %d, want >= 200 (smoothed away)", v)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := compileSrcs(t, []recipe.ProbeInfo{info}, []recipe.Op{{Kind: recipe.OpMorph, Params: []byte(tc.params)}}, out)
			if !p.HasAlpha || !strings.Contains(p.Filter, "fps=10:round=down,"+tc.stage+"[out]") {
				t.Fatalf("plan: alpha %v filter %s", p.HasAlpha, p.Filter)
			}
			frames := p3Render(t, ff, clip, p, nil, nil)
			if len(frames) != 2 {
				t.Fatalf("%d frames, want 2", len(frames))
			}
			var want []byte
			if tc.want != nil {
				want = tc.want(src)
			}
			for i, f := range frames {
				got := alphaPlane(f, w, h)
				for j := range want {
					if got[j] != want[j] {
						t.Errorf("frame %d: alpha (%d,%d) = %d, want %d (reference 3x3 morphology)", i, j%w, j/w, got[j], want[j])
						break
					}
				}
				// only the alpha passes through a filter: every color byte —
				// under the transparency included — is the plain decode's.
				for j := 0; j < w*h; j++ {
					if f[j*4] != plain[i][j*4] || f[j*4+1] != plain[i][j*4+1] || f[j*4+2] != plain[i][j*4+2] {
						t.Errorf("frame %d: color (%d,%d) = %v, want %v untouched", i, j%w, j/w, pixel(f, w, j%w, j/w), pixel(plain[i], w, j%w, j/w))
						break
					}
				}
				if i == 0 {
					tc.facts(t, got)
				}
			}
		})
	}
}

// --- stacked colorkeys ------------------------------------------------------

func TestStackedColorKeysIntersect(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	out := recipe.Output{Format: "webp"}
	const w, h = 36, 24
	// Three opaque 12 px bands (red, green, blue) and a white 8x8 subject at
	// (14,8), straddling the red/green border.
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	bands := []color.NRGBA{{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, bands[x/12])
		}
	}
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	inSubject := func(x, y int) bool { return x >= 14 && x < 22 && y >= 8 && y < 16 }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if inSubject(x, y) {
				img.SetNRGBA(x, y, white)
			}
		}
	}
	clip, info := pngClip(t, ff, dir, "bands", []image.Image{img, img}, 10, false)
	keys := func(colors ...string) []recipe.Op {
		ops := make([]recipe.Op, 0, len(colors))
		for _, c := range colors {
			ops = append(ops, recipe.Op{Kind: recipe.OpColorKey, Params: []byte(`{"color":"` + c + `"}`)})
		}
		return ops
	}
	// check asserts the alpha of every band pixel (keyed[band]) and the
	// opaque white subject.
	check := func(t *testing.T, frames [][]byte, keyed [3]bool, name string) {
		t.Helper()
		if len(frames) != 2 {
			t.Fatalf("%s: %d frames, want 2", name, len(frames))
		}
		for i, f := range frames {
			bad := 0
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					px := pixel(f, w, x, y)
					var problem string
					switch {
					case inSubject(x, y) && !isRGBA(px, 255, 255, 255, 255, 1):
						problem = fmt.Sprintf("subject (%d,%d) = %v, want opaque white", x, y, px)
					case !inSubject(x, y) && keyed[x/12] && px[3] != 0:
						problem = fmt.Sprintf("band %d pixel (%d,%d) = %v, want alpha 0 (keyed)", x/12, x, y, px)
					case !inSubject(x, y) && !keyed[x/12] && !isRGBA(px, bands[x/12].R, bands[x/12].G, bands[x/12].B, 255, 1):
						problem = fmt.Sprintf("band %d pixel (%d,%d) = %v, want the opaque band color (not keyed)", x/12, x, y, px)
					}
					if problem != "" {
						if bad++; bad <= 3 {
							t.Errorf("%s frame %d: %s", name, i, problem)
						}
					}
				}
			}
			if bad > 3 {
				t.Errorf("%s frame %d: %d bad pixels in all", name, i, bad)
			}
		}
	}

	t.Run("three stacked colorkeys remove their three colors and keep the subject", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{info}, keys("ff0000", "00ff00", "0000ff"), out)
		// The first key is bare, the second and third are wrapped on the
		// previous matte (default similarity 0.08, blend 0).
		for _, want := range []string{
			"fps=10:round=down,format=rgba,colorkey=color=0xff0000:similarity=0.08:blend=0,split[k1][k1m];[k1m]alphaextract[k1a0];[k1]format=rgba,colorkey=color=0x00ff00:similarity=0.08:blend=0,split[k1k][k1km];",
			"[k1k][k1a]alphamerge,split[k2][k2m];[k2m]alphaextract[k2a0];[k2]format=rgba,colorkey=color=0x0000ff:similarity=0.08:blend=0,split[k2k][k2km];",
			"[k2k][k2a]alphamerge,format=rgba[out]",
		} {
			if !strings.Contains(p.Filter, want) {
				t.Fatalf("filter lacks %q:\n%s", want, p.Filter)
			}
		}
		check(t, p3Render(t, ff, clip, p, nil, nil), [3]bool{true, true, true}, "three keys")
	})
	t.Run("each key removes only its own color: two keys leave the third band opaque", func(t *testing.T) {
		p := compileSrcs(t, []recipe.ProbeInfo{info}, keys("ff0000", "00ff00"), out)
		check(t, p3Render(t, ff, clip, p, nil, nil), [3]bool{true, true, false}, "red+green")
		p = compileSrcs(t, []recipe.ProbeInfo{info}, keys("0000ff", "ff0000"), out)
		check(t, p3Render(t, ff, clip, p, nil, nil), [3]bool{true, false, true}, "blue+red")
	})
}
