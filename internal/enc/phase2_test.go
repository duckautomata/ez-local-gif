package enc

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// rawTest is RawInputArgs(testMaster()).
var rawTest = []string{"-f", "rawvideo", "-pix_fmt", "rgba", "-s", "320x240", "-r", "25", "-i", "/dev/shm/ezl/job1/frames.rgba"}

// withRaw prepends rawTest to tail.
func withRaw(tail ...string) []string {
	return append(append([]string{}, rawTest...), tail...)
}

// Variant chains for testMaster (320x240 @ 25 fps, alpha).
const (
	vfFPS20        = "fps=20:round=down"
	vfScale160a    = "format=gbrap,premultiply=inplace=1,scale=160:120:flags=lanczos,unpremultiply=inplace=1,format=rgba"
	vfScale160     = "scale=160:120:flags=lanczos"
	vfFPS20Scale   = vfFPS20 + "," + vfScale160a
	vfFPS20ScaleNA = vfFPS20 + "," + vfScale160
)

func TestVariantFilter(t *testing.T) {
	m := testMaster()
	opaque := m
	opaque.HasAlpha = false
	still := m
	still.Frames = 1
	tests := []struct {
		name string
		m    Master
		v    *Variant
		want string
	}{
		{"nil variant", m, nil, ""},
		{"zero variant", m, &Variant{}, ""},
		{"fps only", m, &Variant{FPS: 20}, vfFPS20},
		{"fractional fps", m, &Variant{FPS: 100.0 / 6}, "fps=16.666667:round=down"},
		{"fps equal to master is a no-op", m, &Variant{FPS: 25}, ""},
		{"fps above master is a no-op", m, &Variant{FPS: 30}, ""},
		{"negative/NaN/Inf fps are no-ops", m, &Variant{FPS: math.NaN()}, ""},
		{"inf fps is a no-op", m, &Variant{FPS: math.Inf(1)}, ""},
		{"scale by width keeps aspect (premultiplied with alpha)", m, &Variant{Width: 160}, vfScale160a},
		{"scale by height keeps aspect", m, &Variant{Height: 120}, vfScale160a},
		{"scale both exact", m, &Variant{Width: 160, Height: 100}, "format=gbrap,premultiply=inplace=1,scale=160:100:flags=lanczos,unpremultiply=inplace=1,format=rgba"},
		{"scale without alpha is a plain lanczos", opaque, &Variant{Width: 160}, vfScale160},
		{"size equal to master is a no-op", m, &Variant{Width: 320, Height: 240}, ""},
		{"width equal to master is a no-op", m, &Variant{Width: 320}, ""},
		{"upscale is clamped to the master (no-op)", m, &Variant{Width: 640}, ""},
		{"upscale on one axis clamps that axis", m, &Variant{Width: 640, Height: 120}, "format=gbrap,premultiply=inplace=1,scale=320:120:flags=lanczos,unpremultiply=inplace=1,format=rgba"},
		{"negative sizes count as unset", m, &Variant{Width: -5, Height: -5}, ""},
		{"fps and scale", m, &Variant{FPS: 20, Width: 160}, vfFPS20Scale},
		{"fps and scale without alpha", opaque, &Variant{FPS: 20, Width: 160}, vfFPS20ScaleNA},
		{"odd aspect rounds half up and never below 1", Master{Width: 1000, Height: 3, FPS: 25}, &Variant{Width: 100}, "scale=100:1:flags=lanczos"},
		{"single-frame master skips the fps stage", still, &Variant{FPS: 10, Width: 160}, vfScale160a},
		{"a drop that would leave no frame is skipped (2 @ 100 → 10: 0.2 → 0)", Master{Width: 320, Height: 240, FPS: 100, Frames: 2}, &Variant{FPS: 10}, ""},
		{"a drop that floors to no frame is skipped (2 @ 25 → 10: 0.8 → 0)", Master{Width: 320, Height: 240, FPS: 25, Frames: 2}, &Variant{FPS: 10}, ""},
		{"a drop that leaves one frame is kept (3 @ 25 → 10: 1.2 → 1)", Master{Width: 320, Height: 240, FPS: 25, Frames: 3}, &Variant{FPS: 10}, "fps=10:round=down"},
		{"unknown frame count cannot be checked: fps stage kept", Master{Width: 320, Height: 240, FPS: 100}, &Variant{FPS: 10}, "fps=10:round=down"},
		{"master without fps: 25 is assumed, 20 is a drop", Master{Width: 320, Height: 240}, &Variant{FPS: 20}, vfFPS20},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VariantFilter(tc.m, tc.v); got != tc.want {
				t.Errorf("VariantFilter = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVariantMaster(t *testing.T) {
	m := testMaster() // 320x240, 25 fps, 62 frames
	tests := []struct {
		name string
		m    Master
		v    *Variant
		want Master
	}{
		{"nil", m, nil, m},
		{"zero", m, &Variant{}, m},
		{"fps 20: 62 frames * 20/25 = 49.6 → 49", m, &Variant{FPS: 20}, Master{Path: m.Path, Width: 320, Height: 240, FPS: 20, Frames: 49, HasAlpha: true}},
		{"fps 12.5: 31", m, &Variant{FPS: 12.5}, Master{Path: m.Path, Width: 320, Height: 240, FPS: 12.5, Frames: 31, HasAlpha: true}},
		{"fps 10: 24.8 → 24", m, &Variant{FPS: 10}, Master{Path: m.Path, Width: 320, Height: 240, FPS: 10, Frames: 24, HasAlpha: true}},
		{"half floors down: 5 frames 10→5 fps = 2.5 → 2", Master{Width: 16, Height: 16, FPS: 10, Frames: 5}, &Variant{FPS: 5}, Master{Width: 16, Height: 16, FPS: 5, Frames: 2}},
		{"unknown frames stay unknown", Master{Width: 16, Height: 16, FPS: 10}, &Variant{FPS: 5}, Master{Width: 16, Height: 16, FPS: 5}},
		{"scale only", m, &Variant{Width: 128}, Master{Path: m.Path, Width: 128, Height: 96, FPS: 25, Frames: 62, HasAlpha: true}},
		{"fps at master keeps everything", m, &Variant{FPS: 25}, m},
		{"still keeps its frame", Master{Width: 16, Height: 16, FPS: 25, Frames: 1}, &Variant{FPS: 5, Width: 8}, Master{Width: 8, Height: 8, FPS: 25, Frames: 1}},
		{"a drop to zero frames is not applied", Master{Width: 16, Height: 16, FPS: 100, Frames: 2}, &Variant{FPS: 10}, Master{Width: 16, Height: 16, FPS: 100, Frames: 2}},
		{"a drop that floors to zero frames is not applied", Master{Width: 16, Height: 16, FPS: 25, Frames: 2}, &Variant{FPS: 10}, Master{Width: 16, Height: 16, FPS: 25, Frames: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VariantMaster(tc.m, tc.v); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("VariantMaster = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestVariantNeverLengthens is the J2 guard at the model level: an fps drop
// must never make the clip longer than the master — the predicted playing
// time Frames/FPS after the variant must stay <= the master's. Before the
// fix fpsFrames rounded half up, so a 125-frame 5.0 s master at 16.7 fps
// predicted 84 frames = 5.03 s and sticker rungs blew the Discord 5 s cap.
func TestVariantNeverLengthens(t *testing.T) {
	for _, in := range []float64{10, 12.5, 16.8, 25, 29.97, 30, 50, 60} {
		for _, out := range []float64{4, 5, 10, 12.5, 15, 16.7, 20, 25, 33.333} {
			for frames := 1; frames <= 200; frames++ {
				m := Master{Width: 16, Height: 16, FPS: in, Frames: frames}
				vm := VariantMaster(m, &Variant{FPS: out})
				src := float64(frames) / in
				if got := float64(vm.Frames) / vm.FPS; got > src+1e-9 {
					t.Fatalf("%d frames @ %v fps dropped to %v fps: %d frames / %v fps = %v s > %v s",
						frames, in, out, vm.Frames, vm.FPS, got, src)
				}
			}
		}
	}
}

func TestGIFArgs_Variant(t *testing.T) {
	m := testMaster()
	gifTail := func(filter string) []string {
		return []string{"-filter_complex", filter, "-map", "[out]", "-loop", "0", "-f", "gif", "out.gif"}
	}
	t.Run("alpha: colour source takes the variant size and rate", func(t *testing.T) {
		got := GIFArgs(m, GIFOptions{HasAlpha: true, Variant: &Variant{FPS: 20, Width: 160}}, "out.gif")
		want := withRaw(gifTail(
			"[0:v]" + vfFPS20Scale + "[v];" +
				"[v]split[c][a];" +
				"[a]alphaextract,lut=c0='gte(val,128)*255'[m];" +
				"color=c=0x313338:s=160x120:r=20,format=rgba[bg];" +
				"[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];" +
				"[f][m]alphamerge,split[p1][p2];" +
				"[p1]palettegen=max_colors=256:reserve_transparent=1:stats_mode=diff[pal];" +
				"[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle:alpha_threshold=128[out]")...)
		assertArgs(t, got, want)
	})
	t.Run("no alpha: plain scale, split reads [v]", func(t *testing.T) {
		o := m
		o.HasAlpha = false
		got := GIFArgs(o, GIFOptions{Colors: 64, Variant: &Variant{FPS: 12.5, Width: 112, Height: 112}}, "out.gif")
		want := withRaw(gifTail(
			"[0:v]fps=12.5:round=down,scale=112:112:flags=lanczos[v];" +
				"[v]split[p1][p2];" +
				"[p1]palettegen=max_colors=64:reserve_transparent=0:stats_mode=diff[pal];" +
				"[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle[out]")...)
		assertArgs(t, got, want)
	})
	t.Run("no-op variant is byte-identical to no variant", func(t *testing.T) {
		plain := GIFArgs(m, GIFOptions{HasAlpha: true}, "out.gif")
		assertArgs(t, GIFArgs(m, GIFOptions{HasAlpha: true, Variant: &Variant{}}, "out.gif"), plain)
		assertArgs(t, GIFArgs(m, GIFOptions{HasAlpha: true, Variant: &Variant{FPS: 25, Width: 320}}, "out.gif"), plain)
		assertArgs(t, plain, withRaw(gifTail(gifAlphaFilter)...))
	})
}

func TestWebPArgs_Variant(t *testing.T) {
	m := testMaster()
	t.Run("variant inserts filter_complex+map after the input", func(t *testing.T) {
		got := WebPArgs(m, WebPOptions{Variant: &Variant{FPS: 20, Width: 160}}, "out.webp")
		want := withRaw(
			"-filter_complex", "[0:v]"+vfFPS20Scale+"[v]", "-map", "[v]",
			"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "80", "-compression_level", "4",
			"-pix_fmt", "yuva420p", "-loop", "0", "-map_metadata", "-1", "-f", "webp", "out.webp")
		assertArgs(t, got, want)
	})
	t.Run("no-op variant is byte-identical", func(t *testing.T) {
		assertArgs(t, WebPArgs(m, WebPOptions{Variant: &Variant{FPS: 30}}, "out.webp"), WebPArgs(m, WebPOptions{}, "out.webp"))
	})
	t.Run("a variant that leaves one frame is written as a still", func(t *testing.T) {
		three := Master{Path: m.Path, Width: 320, Height: 240, FPS: 25, Frames: 3, HasAlpha: true}
		got := WebPArgs(three, WebPOptions{Variant: &Variant{FPS: 10}}, "out.webp") // floor(3*10/25) = 1 frame
		want := withRaw(
			"-filter_complex", "[0:v]fps=10:round=down[v]", "-map", "[v]",
			"-frames:v", "1", "-c:v", "libwebp", "-lossless", "0", "-q:v", "80", "-compression_level", "4",
			"-pix_fmt", "yuva420p", "-map_metadata", "-1", "-f", "webp", "out.webp")
		assertArgs(t, got, want)
	})
}

func TestAPNGArgs(t *testing.T) {
	m := testMaster()
	tests := []struct {
		name string
		o    APNGOptions
		want []string
	}{
		{"defaults: RGBA, mixed, forever", APNGOptions{}, withRaw("-c:v", "apng", "-pred", "mixed", "-plays", "0", "-f", "apng", "out.apng")},
		{"loop 3 = 4 plays, paeth", APNGOptions{Loop: 3, Pred: "paeth"}, withRaw("-c:v", "apng", "-pred", "paeth", "-plays", "4", "-f", "apng", "out.apng")},
		{"loop 1 = 2 plays, none", APNGOptions{Loop: 1, Pred: "none"}, withRaw("-c:v", "apng", "-pred", "none", "-plays", "2", "-f", "apng", "out.apng")},
		{"negative loop = forever, bad pred falls back", APNGOptions{Loop: -2, Pred: "bogus"}, withRaw("-c:v", "apng", "-pred", "mixed", "-plays", "0", "-f", "apng", "out.apng")},
		{"loop above uint16 is clamped", APNGOptions{Loop: 100000}, withRaw("-c:v", "apng", "-pred", "mixed", "-plays", "65535", "-f", "apng", "out.apng")},
		{"variant", APNGOptions{Variant: &Variant{FPS: 20, Width: 160}}, withRaw("-filter_complex", "[0:v]"+vfFPS20Scale+"[v]", "-map", "[v]", "-c:v", "apng", "-pred", "mixed", "-plays", "0", "-f", "apng", "out.apng")},
		{"Colors is not an ffmpeg option (indexed path is tile/pngquant/untile)", APNGOptions{Colors: 64}, withRaw("-c:v", "apng", "-pred", "mixed", "-plays", "0", "-f", "apng", "out.apng")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, APNGArgs(m, tc.o, "out.apng"), tc.want)
		})
	}
}

func TestTileArgs(t *testing.T) {
	m := testMaster()
	opaque := m
	opaque.HasAlpha = false
	tests := []struct {
		name string
		m    Master
		v    *Variant
		c, r int
		want []string
	}{
		{"alpha: transparent padding", m, nil, 8, 8, withRaw("-filter_complex", "[0:v]tile=8x8:color=0x00000000[t]", "-map", "[t]", "-frames:v", "1", "-c:v", "png", "-compression_level", "1", "sheet.png")},
		{"opaque: tile default padding", opaque, nil, 8, 8, withRaw("-filter_complex", "[0:v]tile=8x8[t]", "-map", "[t]", "-frames:v", "1", "-c:v", "png", "-compression_level", "1", "sheet.png")},
		{"variant first", m, &Variant{FPS: 20, Width: 160}, 7, 8, withRaw("-filter_complex", "[0:v]"+vfFPS20Scale+",tile=7x8:color=0x00000000[t]", "-map", "[t]", "-frames:v", "1", "-c:v", "png", "-compression_level", "1", "sheet.png")},
		{"no-op variant", m, &Variant{}, 8, 8, withRaw("-filter_complex", "[0:v]tile=8x8:color=0x00000000[t]", "-map", "[t]", "-frames:v", "1", "-c:v", "png", "-compression_level", "1", "sheet.png")},
		{"zero grid is raised to 1x1", m, nil, 0, 0, withRaw("-filter_complex", "[0:v]tile=1x1:color=0x00000000[t]", "-map", "[t]", "-frames:v", "1", "-c:v", "png", "-compression_level", "1", "sheet.png")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, TileArgs(tc.m, tc.v, tc.c, tc.r, "sheet.png"), tc.want)
		})
	}
}

func TestTileGrid(t *testing.T) {
	tests := []struct {
		frames, w, h int
		cols, rows   int
	}{
		{1, 320, 320, 1, 1},
		{2, 320, 320, 2, 1},
		{4, 320, 320, 2, 2},
		{5, 320, 320, 3, 2},
		{10, 320, 320, 4, 3},
		{12, 320, 320, 4, 3},
		{62, 320, 240, 8, 8},
		{125, 320, 320, 12, 11},
		{1000, 320, 320, 32, 32},
		{0, 320, 320, 1, 1},
		{-3, 320, 320, 1, 1},
		// width cap: 2000 px frames allow 8 columns (16384/2000 = 8.19)
		{100, 2000, 100, 8, 13},
		// a frame wider than the cap still gets one column
		{7, 20000, 10, 1, 7},
		// no cap without a width
		{100, 0, 0, 10, 10},
	}
	for _, tc := range tests {
		c, r := TileGrid(tc.frames, tc.w, tc.h)
		if c != tc.cols || r != tc.rows {
			t.Errorf("TileGrid(%d, %d, %d) = %dx%d, want %dx%d", tc.frames, tc.w, tc.h, c, r, tc.cols, tc.rows)
		}
	}
}

// TestTileGridProperties: for every frame count and a range of frame widths
// the grid holds all frames, stays within the width cap, wastes less than a
// row, has no empty trailing column and is near-square when the cap allows.
func TestTileGridProperties(t *testing.T) {
	for _, w := range []int{1, 16, 128, 320, 480, 800, 1920, 4096, 16384, 20000} {
		for frames := 1; frames <= 1200; frames++ {
			c, r := TileGrid(frames, w, w)
			if c < 1 || r < 1 {
				t.Fatalf("TileGrid(%d,%d): %dx%d", frames, w, c, r)
			}
			if c*r < frames {
				t.Fatalf("TileGrid(%d,%d) = %dx%d holds %d < %d frames", frames, w, c, r, c*r, frames)
			}
			if c > 1 && c*w > MaxTileWidth {
				t.Fatalf("TileGrid(%d,%d) = %dx%d is %d px wide", frames, w, c, r, c*w)
			}
			if c*r-frames >= c { // a whole empty row would be a wasted row
				t.Fatalf("TileGrid(%d,%d) = %dx%d wastes %d cells (>= a row)", frames, w, c, r, c*r-frames)
			}
			if c*(r-1) >= frames {
				t.Fatalf("TileGrid(%d,%d) = %dx%d has an empty trailing row", frames, w, c, r)
			}
			if (c-1)*r >= frames {
				t.Fatalf("TileGrid(%d,%d) = %dx%d has an empty trailing column", frames, w, c, r)
			}
			if sq := int(math.Ceil(math.Sqrt(float64(frames)))); sq*w <= MaxTileWidth && (c > sq || r > sq+1) {
				t.Fatalf("TileGrid(%d,%d) = %dx%d is not near-square (sqrt %d)", frames, w, c, r, sq)
			}
		}
	}
}

func TestPngquantArgs(t *testing.T) {
	tests := []struct {
		name          string
		colors, speed int
		dither        bool
		want          []string
	}{
		{"defaults: ordered (nofs), 256, speed 3", 0, 0, false, []string{"--nofs", "256", "--speed", "3", "--force", "-o", "out.png", "in.png"}},
		{"64 colours speed 1", 64, 1, false, []string{"--nofs", "64", "--speed", "1", "--force", "-o", "out.png", "in.png"}},
		{"dither drops --nofs", 128, 0, true, []string{"128", "--speed", "3", "--force", "-o", "out.png", "in.png"}},
		{"clamps: colours 1 → 2, 999 → 256 (next), speed 20 → 11", 1, 20, false, []string{"--nofs", "2", "--speed", "11", "--force", "-o", "out.png", "in.png"}},
		{"colours 999 → 256", 999, 0, false, []string{"--nofs", "256", "--speed", "3", "--force", "-o", "out.png", "in.png"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, PngquantArgs("in.png", "out.png", tc.colors, tc.dither, tc.speed), tc.want)
		})
	}
}

func TestPngquantFileArgs(t *testing.T) {
	tests := []struct {
		name       string
		minQ, maxQ int
		want       string
	}{
		{"both zero = 70-100", 0, 0, "70-100"},
		{"explicit", 50, 90, "50-90"},
		{"min 0 explicit with max", 0, 60, "0-60"},
		{"max 0 = 100", 40, 0, "40-100"},
		{"min above max is lowered", 90, 60, "60-60"},
		{"clamped", -5, 150, "0-100"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, PngquantFileArgs("in.png", "out.png", tc.minQ, tc.maxQ),
				[]string{"--quality", tc.want, "--speed", "3", "--force", "-o", "out.png", "in.png"})
		})
	}
}

func TestUntileAPNGArgs(t *testing.T) {
	tail := func(plays, pred string) []string {
		return []string{"-c:v", "apng", "-pix_fmt", "pal8", "-pred", pred, "-plays", plays, "-f", "apng", "out.apng"}
	}
	tests := []struct {
		name              string
		cols, rows, frame int
		fps               float64
		o                 APNGOptions
		want              []string
	}{
		{
			"25 fps in a 4x3 sheet: input rate 25/12", 4, 3, 10, 25, APNGOptions{},
			append([]string{"-framerate", "25/12", "-i", "sheet.png", "-filter_complex", "[0:v]untile=4x3[f]", "-map", "[f]", "-frames:v", "10"}, tail("0", "mixed")...),
		},
		{
			"fractional fps keeps six decimals; loop 2 = 3 plays; paeth", 12, 11, 125, 100.0 / 6, APNGOptions{Loop: 2, Pred: "paeth"},
			append([]string{"-framerate", "16.666667/132", "-i", "sheet.png", "-filter_complex", "[0:v]untile=12x11[f]", "-map", "[f]", "-frames:v", "125"}, tail("3", "paeth")...),
		},
		{
			"12.5 fps", 8, 8, 62, 12.5, APNGOptions{},
			append([]string{"-framerate", "12.5/64", "-i", "sheet.png", "-filter_complex", "[0:v]untile=8x8[f]", "-map", "[f]", "-frames:v", "62"}, tail("0", "mixed")...),
		},
		{
			"zero fps falls back to 25, zero grid/frames to 1", 0, 0, 0, 0, APNGOptions{},
			append([]string{"-framerate", "25/1", "-i", "sheet.png", "-filter_complex", "[0:v]untile=1x1[f]", "-map", "[f]", "-frames:v", "1"}, tail("0", "mixed")...),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, UntileAPNGArgs("sheet.png", tc.cols, tc.rows, tc.frame, tc.fps, tc.o, "out.apng"), tc.want)
		})
	}
}

func TestOxipngArgs(t *testing.T) {
	assertArgs(t, OxipngArgs("out.apng", 0), []string{"-o", "2", "--strip", "safe", "--quiet", "out.apng"})
	assertArgs(t, OxipngArgs("out.png", 4), []string{"-o", "4", "--strip", "safe", "--quiet", "out.png"})
	assertArgs(t, OxipngArgs("out.png", 9), []string{"-o", "6", "--strip", "safe", "--quiet", "out.png"})
	assertArgs(t, OxipngArgs("out.png", -1), []string{"-o", "0", "--strip", "safe", "--quiet", "out.png"})
}

func TestFrameWriters(t *testing.T) {
	m := testMaster()
	v := &Variant{FPS: 20, Width: 160}
	vArgs := []string{"-filter_complex", "[0:v]" + vfFPS20Scale + "[v]", "-map", "[v]"}

	t.Run("png frames, deliverable level", func(t *testing.T) {
		assertArgs(t, PNGFramesArgs(m, nil, "/tmp/j/f%05d.png", 6),
			withRaw("-c:v", "png", "-compression_level", "6", "-start_number", "1", "/tmp/j/f%05d.png"))
	})
	t.Run("png frames with variant, temp level, clamps", func(t *testing.T) {
		assertArgs(t, PNGFramesArgs(m, v, "f%05d.png", 1),
			withRaw(append(vArgs, "-c:v", "png", "-compression_level", "1", "-start_number", "1", "f%05d.png")...))
		assertArgs(t, PNGFramesArgs(m, nil, "f%05d.png", 12),
			withRaw("-c:v", "png", "-compression_level", "9", "-start_number", "1", "f%05d.png"))
		assertArgs(t, PNGFramesArgs(m, nil, "f%05d.png", -1),
			withRaw("-c:v", "png", "-compression_level", "0", "-start_number", "1", "f%05d.png"))
	})
	t.Run("webp frames", func(t *testing.T) {
		assertArgs(t, WebPFramesArgs(m, nil, "f%05d.webp"),
			withRaw("-c:v", "libwebp", "-lossless", "1", "-compression_level", "4", "-start_number", "1", "f%05d.webp"))
		assertArgs(t, WebPFramesArgs(m, v, "f%05d.webp"),
			withRaw(append(vArgs, "-c:v", "libwebp", "-lossless", "1", "-compression_level", "4", "-start_number", "1", "f%05d.webp")...))
	})
	t.Run("jpeg frames: flatten onto the matte, default quality 90 → q 5", func(t *testing.T) {
		assertArgs(t, JPEGFramesArgs(m, nil, "", 0, "f%05d.jpg"),
			withRaw("-filter_complex",
				"color=c=0x313338:s=320x240:r=25,format=rgba[bg];[bg][0:v]overlay=format=auto:shortest=1,format=yuvj420p[f]",
				"-map", "[f]", "-c:v", "mjpeg", "-q:v", "5", "-start_number", "1", "f%05d.jpg"))
	})
	t.Run("jpeg frames with variant: matte source takes the variant geometry", func(t *testing.T) {
		assertArgs(t, JPEGFramesArgs(m, v, "#FFFFFF", 100, "f%05d.jpg"),
			withRaw("-filter_complex",
				"[0:v]"+vfFPS20Scale+"[c];color=c=0xffffff:s=160x120:r=20,format=rgba[bg];[bg][c]overlay=format=auto:shortest=1,format=yuvj420p[f]",
				"-map", "[f]", "-c:v", "mjpeg", "-q:v", "2", "-start_number", "1", "f%05d.jpg"))
	})
}

func TestMJPEGQ(t *testing.T) {
	for _, tc := range []struct{ q, want int }{{0, 5}, {100, 2}, {90, 5}, {75, 9}, {50, 17}, {25, 24}, {1, 31}, {-3, 31}, {250, 2}} {
		if got := mjpegQ(tc.q); got != tc.want {
			t.Errorf("mjpegQ(%d) = %d, want %d", tc.q, got, tc.want)
		}
	}
	// Monotone: better quality never gives a worse -q:v.
	prev := mjpegQ(1)
	for q := 2; q <= 100; q++ {
		if cur := mjpegQ(q); cur > prev {
			t.Fatalf("mjpegQ(%d)=%d > mjpegQ(%d)=%d", q, cur, q-1, prev)
		} else {
			prev = cur
		}
	}
}

func TestStills(t *testing.T) {
	m := testMaster()
	t.Run("png still", func(t *testing.T) {
		assertArgs(t, PNGStillArgs(m, StillOptions{}, "still.png"),
			withRaw("-frames:v", "1", "-c:v", "png", "-compression_level", "6", "still.png"))
		assertArgs(t, PNGStillArgs(m, StillOptions{Variant: &Variant{Width: 128}}, "still.png"),
			withRaw("-filter_complex", "[0:v]format=gbrap,premultiply=inplace=1,scale=128:96:flags=lanczos,unpremultiply=inplace=1,format=rgba[v]", "-map", "[v]",
				"-frames:v", "1", "-c:v", "png", "-compression_level", "6", "still.png"))
	})
	t.Run("jpeg still", func(t *testing.T) {
		assertArgs(t, JPEGStillArgs(m, StillOptions{Quality: 75, Matte: "abcdef"}, "still.jpg"),
			withRaw("-filter_complex",
				"color=c=0xabcdef:s=320x240:r=25,format=rgba[bg];[bg][0:v]overlay=format=auto:shortest=1,format=yuvj420p[f]",
				"-map", "[f]", "-frames:v", "1", "-c:v", "mjpeg", "-q:v", "9", "still.jpg"))
		assertArgs(t, JPEGStillArgs(m, StillOptions{Matte: "bad", Variant: &Variant{Width: 160}}, "still.jpg"),
			withRaw("-filter_complex",
				"[0:v]"+vfScale160a+"[c];color=c=0x313338:s=160x120:r=25,format=rgba[bg];[bg][c]overlay=format=auto:shortest=1,format=yuvj420p[f]",
				"-map", "[f]", "-frames:v", "1", "-c:v", "mjpeg", "-q:v", "5", "still.jpg"))
	})
}

func TestAVIFEncArgs(t *testing.T) {
	frames := []string{"/tmp/j/f00001.png", "/tmp/j/f00002.png", "/tmp/j/f00003.png"}
	common := []string{"-j", "all", "-s", "8", "-q", "60", "--qalpha", "90", "-y", "420"}
	tests := []struct {
		name string
		fps  float64
		o    AVIFOptions
		want []string
	}{
		{
			"defaults, integral fps, forever", 25, AVIFOptions{},
			append(append(append([]string{}, common...), "--fps", "25", "--repetition-count", "infinite"), append(frames, "out.avif")...),
		},
		{
			"fractional fps → timescale/duration; loop 3 = 3 repetitions (4 plays)", 12.5, AVIFOptions{Loop: 3},
			append(append(append([]string{}, common...), "--timescale", "12500", "--duration", "1000", "--repetition-count", "3"), append(frames, "out.avif")...),
		},
		{
			"29.97 is exact", 29.97, AVIFOptions{},
			append(append(append([]string{}, common...), "--timescale", "29970", "--duration", "1000", "--repetition-count", "infinite"), append(frames, "out.avif")...),
		},
		{
			"100/3 rounds to 33333", 100.0 / 3, AVIFOptions{},
			append(append(append([]string{}, common...), "--timescale", "33333", "--duration", "1000", "--repetition-count", "infinite"), append(frames, "out.avif")...),
		},
		{
			"explicit options and svt", 30, AVIFOptions{Quality: 45, AlphaQuality: 100, Speed: 6, YUV: "444", Codec: "svt", Loop: 1},
			append([]string{"-j", "all", "-s", "6", "-q", "45", "--qalpha", "100", "-y", "444", "-c", "svt", "--fps", "30", "--repetition-count", "1"}, append(frames, "out.avif")...),
		},
		{
			"clamps and bad enums", 0, AVIFOptions{Quality: 150, AlphaQuality: -2, Speed: 99, YUV: "4:2:0", Codec: "bogus", Loop: -1},
			append([]string{"-j", "all", "-s", "10", "-q", "100", "--qalpha", "0", "-y", "420", "--fps", "25", "--repetition-count", "infinite"}, append(frames, "out.avif")...),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, AVIFEncArgs(frames, tc.fps, tc.o, "out.avif"), tc.want)
		})
	}
	if got := AVIFEncArgs(nil, 25, AVIFOptions{}, "out.avif"); got != nil {
		t.Errorf("no frames: got %q, want nil", got)
	}
}

func TestAVIFStillAndDec(t *testing.T) {
	assertArgs(t, AVIFStillArgs("in.png", AVIFOptions{}, "out.avif"),
		[]string{"-j", "all", "-s", "8", "-q", "60", "--qalpha", "90", "-y", "420", "in.png", "out.avif"})
	assertArgs(t, AVIFStillArgs("in.png", AVIFOptions{Quality: 80, Speed: 6, Codec: "aom"}, "out.avif"),
		[]string{"-j", "all", "-s", "6", "-q", "80", "--qalpha", "90", "-y", "420", "-c", "aom", "in.png", "out.avif"})
	assertArgs(t, AVIFDecArgs("/data/blobs/ab/abcd", "/tmp/j/frames"),
		[]string{"-j", "all", "--index", "all", "-d", "8", "/data/blobs/ab/abcd", "/tmp/j/frames/frame.png"})
	assertArgs(t, AVIFDecArgs("in.avif", "/tmp/j/frames/"),
		[]string{"-j", "all", "--index", "all", "-d", "8", "in.avif", "/tmp/j/frames/frame.png"})
	if got, want := AVIFDecFrame("/tmp/j/frames", 0), "/tmp/j/frames/frame-0000000000.png"; got != want {
		t.Errorf("AVIFDecFrame(0) = %q, want %q", got, want)
	}
	if got, want := AVIFDecFrame("/tmp/j/frames", 41), "/tmp/j/frames/frame-0000000041.png"; got != want {
		t.Errorf("AVIFDecFrame(41) = %q, want %q", got, want)
	}
	if got, want := AVIFDecFrame("", 1), "frame-0000000001.png"; got != want {
		t.Errorf("AVIFDecFrame(\"\",1) = %q, want %q", got, want)
	}
}

func TestGifsicleOptimizeArgs(t *testing.T) {
	ten := []int{10, 10, 10, 10, 10, 10, 10, 10, 10, 10}
	tests := []struct {
		name   string
		delays []int
		o      GifsicleOptimizeOptions
		want   []string
	}{
		{
			"defaults: -O2, forever", ten, GifsicleOptimizeOptions{},
			[]string{"in.gif", "-O2", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			"everything, order fixed", ten, GifsicleOptimizeOptions{Lossy: 80, Colors: 64, Dither: "o8", Loop: 3, Unoptimize: true, Careful: true},
			[]string{"-U", "in.gif", "-O2", "--careful", "--lossy=80", "--colors", "64", "--dither=o8", "--loopcount=3", "-o", "out.gif"},
		},
		{
			"drop every 2nd: -U forced, --delay before each kept frame, delays merged", ten, GifsicleOptimizeOptions{DropEveryN: 2, Careful: true},
			[]string{"-U", "in.gif",
				"--delay", "20", "#0", "--delay", "20", "#2", "--delay", "20", "#4", "--delay", "20", "#6", "--delay", "20", "#8",
				"-O2", "--careful", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			"drop every 3rd with distinct delays: #2 folds into #1, #5 into #4, #8 into #7; #9 kept",
			[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, GifsicleOptimizeOptions{DropEveryN: 3, Lossy: 30},
			[]string{"-U", "in.gif",
				"--delay", "1", "#0", "--delay", "5", "#1", "--delay", "4", "#3", "--delay", "11", "#4", "--delay", "7", "#6", "--delay", "17", "#7", "--delay", "10", "#9",
				"-O2", "--lossy=30", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			"drop every 2nd of an odd count keeps the last frame", []int{5, 6, 7}, GifsicleOptimizeOptions{DropEveryN: 2},
			[]string{"-U", "in.gif", "--delay", "11", "#0", "--delay", "7", "#2", "-O2", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			"DropEveryN without delays is ignored", nil, GifsicleOptimizeOptions{DropEveryN: 2},
			[]string{"in.gif", "-O2", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			"DropEveryN 1 keeps every frame", ten, GifsicleOptimizeOptions{DropEveryN: 1},
			[]string{"in.gif", "-O2", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			"clamps: lossy 200, colours 256, loop 65535; dither without colours dropped; negative loop forever",
			nil, GifsicleOptimizeOptions{Lossy: 999, Colors: 999, Dither: "o8", Loop: 100000},
			[]string{"in.gif", "-O2", "--lossy=200", "--colors", "256", "--dither=o8", "--loopcount=65535", "-o", "out.gif"},
		},
		{
			"dither without colours is dropped, negative loop is forever", nil, GifsicleOptimizeOptions{Dither: "o8", Loop: -1, Colors: 0},
			[]string{"in.gif", "-O2", "--loopcount=forever", "-o", "out.gif"},
		},
		{
			"a single-frame GIF with DropEveryN keeps its frame", []int{10}, GifsicleOptimizeOptions{DropEveryN: 2},
			[]string{"-U", "in.gif", "--delay", "10", "#0", "-O2", "--loopcount=forever", "-o", "out.gif"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertArgs(t, GifsicleOptimizeArgs("in.gif", "out.gif", tc.delays, tc.o), tc.want)
		})
	}
}

func TestMergeDroppedDelays(t *testing.T) {
	tests := []struct {
		name   string
		delays []int
		n      int
		want   []KeptFrame
	}{
		{"n 0 keeps all", []int{1, 2, 3}, 0, []KeptFrame{{0, 1}, {1, 2}, {2, 3}}},
		{"n 1 keeps all", []int{1, 2, 3}, 1, []KeptFrame{{0, 1}, {1, 2}, {2, 3}}},
		{"n 2", []int{1, 2, 3, 4}, 2, []KeptFrame{{0, 3}, {2, 7}}},
		{"n 2 odd", []int{1, 2, 3}, 2, []KeptFrame{{0, 3}, {2, 3}}},
		{"n 3", []int{1, 2, 3, 4, 5, 6, 7}, 3, []KeptFrame{{0, 1}, {1, 5}, {3, 4}, {4, 11}, {6, 7}}},
		{"n larger than the count", []int{1, 2}, 5, []KeptFrame{{0, 1}, {1, 2}}},
		{"negative delays count as 0, sums cap at 65535", []int{65530, -4, 10}, 2, []KeptFrame{{0, 65530}, {2, 10}}},
		{"cap", []int{65530, 10}, 2, []KeptFrame{{0, 65535}}},
		{"empty", nil, 2, []KeptFrame{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeDroppedDelays(tc.delays, tc.n); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MergeDroppedDelays(%v, %d) = %v, want %v", tc.delays, tc.n, got, tc.want)
			}
		})
	}
	// Total duration is preserved for every n.
	delays := []int{3, 4, 3, 4, 3, 4, 3, 4, 3, 4, 3}
	sum := 0
	for _, d := range delays {
		sum += d
	}
	for n := 0; n <= 12; n++ {
		got := 0
		for _, k := range MergeDroppedDelays(delays, n) {
			got += k.Delay
		}
		if got != sum {
			t.Errorf("n=%d: merged total %d, want %d", n, got, sum)
		}
	}
}

func TestSequenceInputArgs(t *testing.T) {
	assertArgs(t, SequenceInputArgs("/data/blobs/ab/abcd", "%06d.png", 10),
		[]string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-i", "/data/blobs/ab/abcd/%06d.png"})
	assertArgs(t, SequenceInputArgs("/data/blobs/ab/abcd/", "f%04d.png", 1000.0/33),
		[]string{"-f", "image2", "-framerate", "30.30303", "-start_number", "1", "-i", "/data/blobs/ab/abcd/f%04d.png"})
	assertArgs(t, SequenceInputArgs("seq", "%06d.png", 0),
		[]string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-i", "seq/%06d.png"})
	assertArgs(t, SequenceInputArgs("", "%06d.png", 12.5),
		[]string{"-f", "image2", "-framerate", "12.5", "-start_number", "1", "-i", "%06d.png"})
}

// TestInputPattern: plans of image-sequence sources point ffmpeg at
// "<blobDir>/<pattern>" in every plan-driven builder.
func TestInputPattern(t *testing.T) {
	p := testPlan()
	p.InputArgs = []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}
	p.Filter = "[0:v]fps=10:round=down,format=rgba[out]"
	p.FPS, p.SourceFPS, p.TrimStart, p.TrimEnd, p.Duration = 10, 10, 0, 0, 1.2
	p.InputPattern = "%06d.png"
	const dir = "/data/blobs/ab/abcd"

	assertArgs(t, MasterArgs(dir, p, "frames.rgba"), []string{
		"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0",
		"-i", dir + "/%06d.png",
		"-filter_complex", "[0:v]fps=10:round=down,format=rgba[out]",
		"-map", "[out]",
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgba",
		"frames.rgba",
	})
	// t=0.5 → slot 5; seek-back max(2/10,0.1)+0.1 = 0.3 → 0.2 (slot 2) → 3 slots after the seek.
	assertArgs(t, StillArgs(dir, p, 0.5, 0), []string{
		"-ss", "0.2",
		"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0",
		"-i", dir + "/%06d.png",
		"-frames:v", "1",
		"-filter_complex", stillFilter("[0:v]fps=10:round=down,format=rgba[out]", "1.3", "0.25", ""),
		"-map", "[outs]",
		"-c:v", "png", "-compression_level", "1",
		"-f", "image2pipe", "pipe:1",
	})
	assertArgs(t, StillArgsFromStart(dir, p, 0.5, 0), []string{
		"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0",
		"-i", dir + "/%06d.png",
		"-frames:v", "1",
		"-filter_complex", stillFilter("[0:v]fps=10:round=down,format=rgba[out]", "1.5", "0.45", ""),
		"-map", "[outs]",
		"-c:v", "png", "-compression_level", "1",
		"-f", "image2pipe", "pipe:1",
	})
	proxy := ProxyArgs(dir+"/", p, 0, 0, "proxy.webp")
	if got := proxy[indexOf(proxy, "-i")+1]; got != dir+"/%06d.png" {
		t.Errorf("ProxyArgs input = %q", got)
	}
	if !reflect.DeepEqual(proxy[:8], []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}) {
		t.Errorf("ProxyArgs keeps the sequence input args: %q", proxy[:10])
	}
	// Without a pattern nothing changes.
	q := testPlan()
	if got := MasterArgs("/data/blobs/ab/abcd.mov", q, "f")[5]; got != "/data/blobs/ab/abcd.mov" {
		t.Errorf("plain plan input = %q", got)
	}
}

func TestJoinSlash(t *testing.T) {
	for _, tc := range []struct{ dir, name, want string }{
		{"/a/b", "c", "/a/b/c"},
		{"/a/b/", "c", "/a/b/c"},
		{`C:\a\b\`, "c", `C:\a\b/c`},
		{"", "c", "c"},
	} {
		if got := joinSlash(tc.dir, tc.name); got != tc.want {
			t.Errorf("joinSlash(%q,%q) = %q, want %q", tc.dir, tc.name, got, tc.want)
		}
	}
}

// TestVariantGraphsStayParseable guards the label plumbing: every graph that
// takes a variant has exactly one "[v]" producer/consumer pair and no label
// is left dangling.
func TestVariantGraphsStayParseable(t *testing.T) {
	m := testMaster()
	v := &Variant{FPS: 20, Width: 160}
	graphs := map[string][]string{
		"gif":   GIFArgs(m, GIFOptions{HasAlpha: true, Variant: v}, "o"),
		"webp":  WebPArgs(m, WebPOptions{Variant: v}, "o"),
		"apng":  APNGArgs(m, APNGOptions{Variant: v}, "o"),
		"tile":  TileArgs(m, v, 4, 4, "o"),
		"png":   PNGFramesArgs(m, v, "o", 1),
		"jpeg":  JPEGFramesArgs(m, v, "", 0, "o"),
		"webpf": WebPFramesArgs(m, v, "o"),
		"pngs":  PNGStillArgs(m, StillOptions{Variant: v}, "o"),
		"jpegs": JPEGStillArgs(m, StillOptions{Variant: v}, "o"),
	}
	for name, args := range graphs {
		i := indexOf(args, "-filter_complex")
		if i < 0 {
			t.Errorf("%s: no -filter_complex", name)
			continue
		}
		f := args[i+1]
		if !strings.HasPrefix(f, "[0:v]"+vfFPS20Scale) {
			t.Errorf("%s: graph does not start with the variant: %s", name, f)
		}
		mapped := args[indexOf(args, "-map")+1]
		if !strings.Contains(f, mapped) {
			t.Errorf("%s: -map %s not produced by %s", name, mapped, f)
		}
		// [v] is either mapped straight out (produced once) or produced once
		// and consumed once inside the graph.
		want := 2
		if mapped == "[v]" {
			want = 1
		}
		if n := strings.Count(f, "[v]"); n != want && n != 0 {
			t.Errorf("%s: [v] appears %d times, want %d: %s", name, n, want, f)
		}
	}
}

func TestDefaultsAreDocumented(t *testing.T) {
	// Spot checks that the exported defaults match DESIGN.md §4.2.
	if DefaultAVIFQuality != 60 || DefaultAVIFAlphaQuality != 90 || DefaultAVIFSpeed != 8 || DefaultAVIFYUV != "420" {
		t.Errorf("AVIF defaults drifted from §4.2: q=%d qalpha=%d s=%d y=%s", DefaultAVIFQuality, DefaultAVIFAlphaQuality, DefaultAVIFSpeed, DefaultAVIFYUV)
	}
	if DefaultPngquantSpeed != 3 || DefaultOxipngLevel != 2 || DefaultAPNGPred != "mixed" {
		t.Errorf("pngquant/oxipng/apng defaults drifted: %d %d %s", DefaultPngquantSpeed, DefaultOxipngLevel, DefaultAPNGPred)
	}
	if strconv.Itoa(MaxTileWidth) != "16384" {
		t.Errorf("MaxTileWidth = %d", MaxTileWidth)
	}
	if s := AVIFDecFrame("d", 3); s != "d/frame-0000000003.png" {
		t.Errorf("AVIFDecPattern: %s", s)
	}
}
