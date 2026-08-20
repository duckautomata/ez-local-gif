package enc_test

// Real-tool checks of the Phase 2 argv: the indexed APNG pipeline (tile →
// pngquant → untile), avifenc/avifdec, gifsicle frame dropping, pngquant,
// oxipng, the JPEG flatten graphs, the frame writers and the variant frame
// count. Every test skips when its tools are not on PATH, so the suite
// passes on the host (ffmpeg only) and runs fully in the ezlg-dev image.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// toolOrSkip returns the binary or skips the test.
func toolOrSkip(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH", name)
	}
	return p
}

// runTool runs a non-ffmpeg tool; a non-zero exit is fatal.
func runTool(t *testing.T, bin string, args []string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return out
}

// softMaster writes a w x h RGBA master of frames frames at fps: a moving
// opaque bar on the left, a soft alpha gradient (alpha 16..240) in the
// middle and fully transparent pixels on the right, so palette/alpha
// handling is exercised end to end. Returns the Master and its bytes.
func softMaster(t *testing.T, dir string, w, h, frames int, fps float64) (enc.Master, []byte) {
	t.Helper()
	buf := make([]byte, 0, w*h*4*frames)
	for f := 0; f < frames; f++ {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				switch {
				case x >= w*3/4:
					buf = append(buf, 0, 0, 0, 0)
				case x >= w/2:
					a := byte(16 + (x-w/2)*224/max(w/4, 1))
					buf = append(buf, 200, 120, 30, a)
				case y == (f*3)%h:
					buf = append(buf, 255, 40, 40, 255)
				default:
					buf = append(buf, 40, 40, 255, 255)
				}
			}
		}
	}
	path := filepath.Join(dir, "frames.rgba")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return enc.Master{Path: path, Width: w, Height: h, FPS: fps, Frames: frames, HasAlpha: true}, buf
}

// --- PNG / APNG parsing -------------------------------------------------------------

type pngChunk struct {
	typ  string
	body []byte
}

// pngChunks splits a PNG/APNG into chunks (fatal on malformed data).
func pngChunks(t *testing.T, data []byte) []pngChunk {
	t.Helper()
	if len(data) < 8 || string(data[1:4]) != "PNG" {
		t.Fatalf("not a PNG (%d bytes)", len(data))
	}
	var out []pngChunk
	for off := 8; off+8 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if off+12+n > len(data) {
			t.Fatalf("chunk %s overruns the file", typ)
		}
		out = append(out, pngChunk{typ, data[off+8 : off+8+n]})
		off += 12 + n
	}
	return out
}

// apngInfo summarises the chunks the lint cares about.
type apngInfo struct {
	colorType      byte
	width, height  int
	plte, trns     bool
	frames, plays  int
	delays         [][2]int // fcTL delay_num/delay_den
	hasACTL        bool
	firstFCTLFirst bool // acTL/fcTL precede the first IDAT (first frame is an animation frame)
}

func parseAPNG(t *testing.T, data []byte) apngInfo {
	t.Helper()
	var info apngInfo
	seenIDAT := false
	for _, c := range pngChunks(t, data) {
		switch c.typ {
		case "IHDR":
			info.width = int(binary.BigEndian.Uint32(c.body))
			info.height = int(binary.BigEndian.Uint32(c.body[4:]))
			info.colorType = c.body[9]
		case "PLTE":
			info.plte = true
		case "tRNS":
			info.trns = true
		case "acTL":
			info.hasACTL = true
			info.frames = int(binary.BigEndian.Uint32(c.body))
			info.plays = int(binary.BigEndian.Uint32(c.body[4:]))
		case "fcTL":
			if !seenIDAT {
				info.firstFCTLFirst = true
			}
			info.delays = append(info.delays, [2]int{int(binary.BigEndian.Uint16(c.body[20:])), int(binary.BigEndian.Uint16(c.body[22:]))})
		case "IDAT":
			seenIDAT = true
		}
	}
	return info
}

// assertDelays checks every fcTL delay equals 1/fps (as a fraction).
func assertDelays(t *testing.T, info apngInfo, fps float64) {
	t.Helper()
	for i, d := range info.delays {
		if d[1] == 0 {
			t.Errorf("frame %d: delay_den 0", i)
			continue
		}
		if got := float64(d[0]) / float64(d[1]); math.Abs(got-1/fps) > 1e-6 {
			t.Errorf("frame %d: delay %d/%d = %v s, want 1/%v", i, d[0], d[1], got, fps)
		}
	}
}

// decodeRGBA decodes any file ffmpeg reads to straight RGBA raw bytes.
func decodeRGBA(t *testing.T, ff, path string) []byte {
	t.Helper()
	return run(t, ff, []string{"-i", path, "-vf", "format=rgba", "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1"})
}

// --- APNG ---------------------------------------------------------------------------

func TestAPNGArgsRGBA(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, raw := softMaster(t, dir, 32, 24, 10, 25)
	out := filepath.Join(dir, "rgba.apng")
	run(t, ff, enc.APNGArgs(m, enc.APNGOptions{Loop: 2}, out))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	info := parseAPNG(t, data)
	if info.colorType != 6 {
		t.Errorf("RGBA APNG colour type %d, want 6", info.colorType)
	}
	if !info.hasACTL || info.frames != 10 || info.plays != 3 {
		t.Errorf("acTL frames=%d plays=%d (present %v), want 10/3", info.frames, info.plays, info.hasACTL)
	}
	if !info.firstFCTLFirst {
		t.Errorf("first frame is not an fcTL frame")
	}
	assertDelays(t, info, 25)
	// Lossless: decoding gives the master back exactly.
	if got := decodeRGBA(t, ff, out); !bytes.Equal(got, raw) {
		t.Errorf("decoded RGBA APNG differs from the master (%d vs %d bytes)", len(got), len(raw))
	}
	// Variant: fps 12.5 → 5 frames at 2/25 s.
	vm := enc.VariantMaster(m, &enc.Variant{FPS: 12.5})
	out2 := filepath.Join(dir, "rgba12.apng")
	run(t, ff, enc.APNGArgs(m, enc.APNGOptions{Variant: &enc.Variant{FPS: 12.5}}, out2))
	data, _ = os.ReadFile(out2)
	info = parseAPNG(t, data)
	if info.frames != vm.Frames || info.frames != 5 || info.plays != 0 {
		t.Errorf("variant acTL frames=%d plays=%d, want %d/0", info.frames, info.plays, vm.Frames)
	}
	assertDelays(t, info, 12.5)
}

func TestIndexedAPNGPipeline(t *testing.T) {
	ff := ffmpegOrSkip(t)
	pq := toolOrSkip(t, "pngquant")
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 32, 24, 10, 25)

	for _, tc := range []struct {
		name string
		v    *enc.Variant
		loop int
	}{
		{"master rate", nil, 0},
		{"variant 12.5 fps, 16 px, loop 2", &enc.Variant{FPS: 12.5, Width: 16}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := enc.VariantMaster(m, tc.v)
			cols, rows := enc.TileGrid(vm.Frames, vm.Width, vm.Height)
			if cols*rows < vm.Frames {
				t.Fatalf("grid %dx%d < %d frames", cols, rows, vm.Frames)
			}
			sheet := filepath.Join(dir, "sheet.png")
			run(t, ff, enc.TileArgs(m, tc.v, cols, rows, sheet))
			sd, err := os.ReadFile(sheet)
			if err != nil {
				t.Fatal(err)
			}
			si := parseAPNG(t, sd)
			if si.width != cols*vm.Width || si.height != rows*vm.Height || si.colorType != 6 {
				t.Fatalf("sheet is %dx%d ct=%d, want %dx%d ct=6", si.width, si.height, si.colorType, cols*vm.Width, rows*vm.Height)
			}

			quant := filepath.Join(dir, "sheetq.png")
			runTool(t, pq, enc.PngquantArgs(sheet, quant, 64, false, 0))
			qd, err := os.ReadFile(quant)
			if err != nil {
				t.Fatal(err)
			}
			qi := parseAPNG(t, qd)
			if qi.colorType != 3 || !qi.plte || !qi.trns {
				t.Fatalf("pngquant sheet: ct=%d plte=%v trns=%v, want indexed with PLTE+tRNS", qi.colorType, qi.plte, qi.trns)
			}

			out := filepath.Join(dir, "indexed.apng")
			run(t, ff, enc.UntileAPNGArgs(quant, cols, rows, vm.Frames, vm.FPS, enc.APNGOptions{Loop: tc.loop}, out))
			od, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			info := parseAPNG(t, od)
			if info.colorType != 3 || !info.plte || !info.trns {
				t.Errorf("indexed APNG: ct=%d plte=%v trns=%v, want 3 with PLTE+tRNS", info.colorType, info.plte, info.trns)
			}
			if info.width != vm.Width || info.height != vm.Height {
				t.Errorf("indexed APNG is %dx%d, want %dx%d", info.width, info.height, vm.Width, vm.Height)
			}
			wantPlays := 0
			if tc.loop > 0 {
				wantPlays = tc.loop + 1
			}
			if !info.hasACTL || info.frames != vm.Frames || info.plays != wantPlays {
				t.Errorf("acTL frames=%d plays=%d, want %d/%d", info.frames, info.plays, vm.Frames, wantPlays)
			}
			if len(info.delays) != vm.Frames {
				t.Errorf("%d fcTL chunks, want %d", len(info.delays), vm.Frames)
			}
			if !info.firstFCTLFirst {
				t.Errorf("first frame is not an fcTL frame")
			}
			assertDelays(t, info, vm.FPS)

			// ffmpeg decodes it, with the right frame count, and the pixels are
			// exactly the quantised sheet's (no re-quantisation on the way).
			run(t, ff, enc.VerifyDecodeArgs(out))
			got := decodeRGBA(t, ff, out)
			if len(got) != vm.Width*vm.Height*4*vm.Frames {
				t.Fatalf("decoded %d bytes, want %d frames of %dx%d", len(got), vm.Frames, vm.Width, vm.Height)
			}
			ref := run(t, ff, []string{
				"-framerate", "1", "-i", quant,
				"-filter_complex", fmt.Sprintf("[0:v]untile=%dx%d,format=rgba[f]", cols, rows),
				"-map", "[f]", "-frames:v", strconv.Itoa(vm.Frames),
				"-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1",
			})
			if !bytes.Equal(got, ref) {
				t.Errorf("indexed APNG pixels differ from the quantised sheet")
			}
			// Alpha survived quantisation: some pixel is semi-transparent.
			semi := false
			for i := 3; i < len(got); i += 4 {
				if got[i] > 0 && got[i] < 255 {
					semi = true
					break
				}
			}
			if !semi {
				t.Errorf("no semi-transparent pixel survived the indexed path")
			}

			if ox, err := exec.LookPath("oxipng"); err == nil {
				before := len(od)
				runTool(t, ox, enc.OxipngArgs(out, 0))
				od2, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				i2 := parseAPNG(t, od2)
				if i2.colorType != 3 || !i2.hasACTL || i2.frames != vm.Frames || i2.plays != wantPlays || len(i2.delays) != vm.Frames {
					t.Errorf("oxipng broke the APNG: ct=%d acTL=%v frames=%d plays=%d fcTL=%d", i2.colorType, i2.hasACTL, i2.frames, i2.plays, len(i2.delays))
				}
				if len(od2) > before {
					t.Errorf("oxipng grew the file: %d → %d", before, len(od2))
				}
				if after := decodeRGBA(t, ff, out); !bytes.Equal(after, got) {
					t.Errorf("oxipng changed the pixels")
				}
			} else {
				t.Log("oxipng not on PATH; in-place optimisation not exercised")
			}
		})
	}
}

// --- AVIF ------------------------------------------------------------------------

// mp4Timing returns mvhd (timescale, duration) and the first mdhd
// (timescale, duration) of an ISOBMFF file.
func mp4Timing(t *testing.T, data []byte) (mvTS, mvDur, mdTS, mdDur uint64) {
	t.Helper()
	var walk func(d []byte)
	walk = func(d []byte) {
		for off := 0; off+8 <= len(d); {
			size := int(binary.BigEndian.Uint32(d[off:]))
			typ := string(d[off+4 : off+8])
			hdr := 8
			if size == 1 {
				size = int(binary.BigEndian.Uint64(d[off+8:]))
				hdr = 16
			}
			if size == 0 {
				size = len(d) - off
			}
			if size < hdr || off+size > len(d) {
				return
			}
			body := d[off+hdr : off+size]
			switch typ {
			case "moov", "trak", "mdia":
				walk(body)
			case "mvhd", "mdhd":
				var ts, dur uint64
				if body[0] == 1 {
					ts, dur = uint64(binary.BigEndian.Uint32(body[20:])), binary.BigEndian.Uint64(body[24:])
				} else {
					ts, dur = uint64(binary.BigEndian.Uint32(body[12:])), uint64(binary.BigEndian.Uint32(body[16:]))
				}
				if typ == "mvhd" {
					mvTS, mvDur = ts, dur
				} else if mdTS == 0 {
					mdTS, mdDur = ts, dur
				}
			}
			off += size
		}
	}
	walk(data)
	return
}

func TestAVIFEncDec(t *testing.T) {
	ff := ffmpegOrSkip(t)
	ae := toolOrSkip(t, "avifenc")
	dir := t.TempDir()
	m, raw := softMaster(t, dir, 32, 24, 10, 10)
	run(t, ff, enc.PNGFramesArgs(m, nil, filepath.Join(dir, "f%05d.png"), 1))
	var frames []string
	for i := 1; i <= m.Frames; i++ {
		frames = append(frames, filepath.Join(dir, fmt.Sprintf("f%05d.png", i)))
	}

	t.Run("integral fps, infinite", func(t *testing.T) {
		out := filepath.Join(dir, "inf.avif")
		runTool(t, ae, enc.AVIFEncArgs(frames, 10, enc.AVIFOptions{}, out))
		data, _ := os.ReadFile(out)
		mvTS, mvDur, mdTS, mdDur := mp4Timing(t, data)
		if mdTS != 10 || mdDur != 10 {
			t.Errorf("mdhd %d/%d, want 10 frames at timescale 10", mdDur, mdTS)
		}
		if mvDur != math.MaxUint64 {
			t.Errorf("mvhd duration %d (ts %d), want UINT64_MAX for infinite repetition", mvDur, mvTS)
		}
	})
	t.Run("loop 3 = 4 plays", func(t *testing.T) {
		out := filepath.Join(dir, "loop3.avif")
		runTool(t, ae, enc.AVIFEncArgs(frames, 10, enc.AVIFOptions{Loop: 3}, out))
		data, _ := os.ReadFile(out)
		mvTS, mvDur, mdTS, mdDur := mp4Timing(t, data)
		if mvTS != mdTS || mvDur != 4*mdDur {
			t.Errorf("mvhd %d/%d vs mdhd %d/%d: want 4x the sequence", mvDur, mvTS, mdDur, mdTS)
		}
	})
	t.Run("fractional fps uses timescale 1000*fps", func(t *testing.T) {
		out := filepath.Join(dir, "frac.avif")
		runTool(t, ae, enc.AVIFEncArgs(frames, 12.5, enc.AVIFOptions{}, out))
		data, _ := os.ReadFile(out)
		_, _, mdTS, mdDur := mp4Timing(t, data)
		if mdTS != 12500 || mdDur != 10*1000 {
			t.Errorf("mdhd %d/%d, want 10000/12500 (12.5 fps)", mdDur, mdTS)
		}
		// ffprobe agrees on the rate.
		if fp, err := exec.LookPath("ffprobe"); err == nil {
			o, _ := exec.Command(fp, "-v", "error", "-select_streams", "v:2", "-show_entries", "stream=r_frame_rate", "-of", "csv=p=0", out).Output()
			if rate := strings.TrimSpace(string(o)); rate != "" && rate != "25/2" {
				t.Errorf("ffprobe r_frame_rate %q, want 25/2", rate)
			}
		}
	})
	t.Run("svt codec when available", func(t *testing.T) {
		ver, _ := exec.Command(ae, "--version").CombinedOutput()
		if !strings.Contains(string(ver), "svt") {
			t.Skip("avifenc without svt")
		}
		// SVT-AV1 needs >= MinSVTDim px per side: a 64x64 alpha master encodes…
		big := filepath.Join(dir, "svt")
		if err := os.MkdirAll(big, 0o755); err != nil {
			t.Fatal(err)
		}
		bm, _ := softMaster(t, big, enc.MinSVTDim, enc.MinSVTDim, 4, 10)
		run(t, ff, enc.PNGFramesArgs(bm, nil, filepath.Join(big, "f%05d.png"), 1))
		var bigFrames []string
		for i := 1; i <= bm.Frames; i++ {
			bigFrames = append(bigFrames, filepath.Join(big, fmt.Sprintf("f%05d.png", i)))
		}
		out := filepath.Join(dir, "svt.avif")
		runTool(t, ae, enc.AVIFEncArgs(bigFrames, 10, enc.AVIFOptions{Codec: "svt"}, out))
		data, err := os.ReadFile(out)
		if err != nil || len(data) == 0 {
			t.Fatalf("svt encode produced nothing: %v", err)
		}
		if _, _, mdTS, mdDur := mp4Timing(t, data); mdTS != 10 || mdDur != uint64(bm.Frames) {
			t.Errorf("svt mdhd %d/%d, want %d frames at timescale 10", mdDur, mdTS, bm.Frames)
		}
		// …and the 32x24 frames are refused (the floor MinSVTDim documents).
		small := filepath.Join(dir, "svt-small.avif")
		if o, err := exec.Command(ae, enc.AVIFEncArgs(frames, 10, enc.AVIFOptions{Codec: "svt"}, small)...).CombinedOutput(); err == nil {
			t.Errorf("svt encoded 32x24 frames; MinSVTDim=%d may be obsolete on this build\n%s", enc.MinSVTDim, o)
		}
	})
	t.Run("avifdec --index all writes alpha PNGs", func(t *testing.T) {
		ad := toolOrSkip(t, "avifdec")
		src := filepath.Join(dir, "inf.avif")
		if _, err := os.Stat(src); err != nil {
			runTool(t, ae, enc.AVIFEncArgs(frames, 10, enc.AVIFOptions{}, src))
		}
		outDir := filepath.Join(dir, "dec")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		runTool(t, ad, enc.AVIFDecArgs(src, outDir))
		for i := 0; i < m.Frames; i++ {
			if _, err := os.Stat(enc.AVIFDecFrame(outDir, i)); err != nil {
				t.Errorf("frame %d: %v", i, err)
			}
		}
		if _, err := os.Stat(enc.AVIFDecFrame(outDir, m.Frames)); err == nil {
			t.Errorf("avifdec wrote more than %d frames", m.Frames)
		}
		f, err := os.Open(enc.AVIFDecFrame(outDir, 0))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		img, err := png.Decode(f)
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != m.Width || img.Bounds().Dy() != m.Height {
			t.Fatalf("decoded frame is %v", img.Bounds())
		}
		// Soft alpha survives (lossy, so compare loosely against frame 0 of the master).
		var sumA, n float64
		semi := false
		for y := 0; y < m.Height; y++ {
			for x := 0; x < m.Width; x++ {
				_, _, _, a := color.NRGBAModel.Convert(img.At(x, y)).RGBA()
				a8 := float64(a >> 8)
				want := float64(raw[(y*m.Width+x)*4+3])
				sumA += math.Abs(a8 - want)
				n++
				if a8 > 8 && a8 < 247 {
					semi = true
				}
			}
		}
		if !semi {
			t.Errorf("no semi-transparent pixel in the decoded AVIF frame")
		}
		if mean := sumA / n; mean > 6 {
			t.Errorf("mean alpha error %.2f, want <= 6 (qalpha 90)", mean)
		}
	})
	t.Run("still", func(t *testing.T) {
		out := filepath.Join(dir, "still.avif")
		runTool(t, ae, enc.AVIFStillArgs(frames[0], enc.AVIFOptions{Quality: 70}, out))
		if ad, err := exec.LookPath("avifdec"); err == nil {
			outDir := filepath.Join(dir, "decstill")
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				t.Fatal(err)
			}
			runTool(t, ad, enc.AVIFDecArgs(out, outDir))
			if _, err := os.Stat(enc.AVIFDecFrame(outDir, 0)); err != nil {
				t.Errorf("still decode: %v", err)
			}
			if _, err := os.Stat(enc.AVIFDecFrame(outDir, 1)); err == nil {
				t.Errorf("still decode wrote a second frame")
			}
		}
	})
}

// --- gifsicle ---------------------------------------------------------------------

// barGIF writes a frames-long 40x30 GIF whose red bar moves every frame,
// with the given centisecond delays, and returns its path.
func barGIF(t *testing.T, dir string, delays []int) string {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 0}, color.RGBA{220, 30, 30, 255}, color.RGBA{30, 30, 220, 255}, color.RGBA{30, 200, 30, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i, d := range delays {
		fr := image.NewPaletted(image.Rect(0, 0, 40, 30), pal)
		for y := 0; y < 30; y++ {
			for x := 0; x < 40; x++ {
				fr.SetColorIndex(x, y, 2)
			}
		}
		for y := 5; y < 25; y++ {
			for x := i*2 + 2; x < i*2+14; x++ {
				fr.SetColorIndex(x, y, uint8(1+i%2*2)) // red / green alternating
			}
		}
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, d)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bar.gif")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readGIF(t *testing.T, path string) *gif.GIF {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return g
}

func TestGifsicleOptimizeArgsOnDisk(t *testing.T) {
	gs := toolOrSkip(t, "gifsicle")
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	delays := []int{5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	src := barGIF(t, dir, delays)
	// Optimise first so the input has inter-frame diffs (the case -U exists for).
	opt := filepath.Join(dir, "opt.gif")
	runTool(t, gs, enc.GifsicleArgs(src, opt, enc.GifsicleOptions{}))
	if g := readGIF(t, opt); len(g.Image) != 10 {
		t.Fatalf("optimised input has %d frames", len(g.Image))
	}

	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("drop every %d", n), func(t *testing.T) {
			out := filepath.Join(dir, fmt.Sprintf("drop%d.gif", n))
			runTool(t, gs, enc.GifsicleOptimizeArgs(opt, out, delays, enc.GifsicleOptimizeOptions{DropEveryN: n, Careful: true}))
			g := readGIF(t, out)
			kept := enc.MergeDroppedDelays(delays, n)
			if len(g.Image) != len(kept) {
				t.Fatalf("%d frames, want %d", len(g.Image), len(kept))
			}
			for i, k := range kept {
				if g.Delay[i] != k.Delay {
					t.Errorf("frame %d (source #%d): delay %d cs, want %d", i, k.Index, g.Delay[i], k.Delay)
				}
			}
			if g.LoopCount != 0 {
				t.Errorf("loop count %d, want 0 (forever)", g.LoopCount)
			}
			// The kept frames render exactly like the source's (ffmpeg
			// composites both; select keeps source frames i with (i+1)%n != 0).
			sel := fmt.Sprintf("select='not(eq(mod(n+1\\,%d)\\,0))',format=rgb24", n)
			ref := run(t, ff, []string{"-i", src, "-vf", sel, "-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1"})
			got := run(t, ff, []string{"-i", out, "-vf", "format=rgb24", "-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1"})
			if len(got) != 40*30*3*len(kept) {
				t.Fatalf("decoded %d bytes, want %d frames", len(got), len(kept))
			}
			if !bytes.Equal(got, ref) {
				t.Errorf("kept frames differ from the source frames (were the optimised frames coalesced?)")
			}
		})
	}

	t.Run("lossy, colours, dither, loop", func(t *testing.T) {
		out := filepath.Join(dir, "lossy.gif")
		runTool(t, gs, enc.GifsicleOptimizeArgs(opt, out, delays, enc.GifsicleOptimizeOptions{Lossy: 40, Colors: 4, Dither: "o8", Loop: 3, Careful: true}))
		g := readGIF(t, out)
		if len(g.Image) != 10 {
			t.Errorf("%d frames, want 10", len(g.Image))
		}
		if g.LoopCount != 3 {
			t.Errorf("loop count %d, want 3", g.LoopCount)
		}
		for i, d := range delays {
			if g.Delay[i] != d {
				t.Errorf("frame %d delay %d, want %d (unchanged without dropping)", i, g.Delay[i], d)
			}
		}
		if pal, ok := g.Config.ColorModel.(color.Palette); ok && len(pal) > 4 {
			t.Errorf("global palette has %d entries, want <= 4", len(pal))
		}
	})

	t.Run("dropping plus colours and a loop count", func(t *testing.T) {
		out := filepath.Join(dir, "both.gif")
		runTool(t, gs, enc.GifsicleOptimizeArgs(opt, out, delays, enc.GifsicleOptimizeOptions{DropEveryN: 2, Colors: 8, Lossy: 20, Loop: 1, Careful: true}))
		g := readGIF(t, out)
		if len(g.Image) != 5 || g.LoopCount != 1 {
			t.Errorf("%d frames loop %d, want 5 / 1", len(g.Image), g.LoopCount)
		}
		for i, k := range enc.MergeDroppedDelays(delays, 2) {
			if g.Delay[i] != k.Delay {
				t.Errorf("frame %d: delay %d, want %d", i, g.Delay[i], k.Delay)
			}
		}
	})
}

// --- pngquant / oxipng on stills ----------------------------------------------------

func TestPngquantFileAndOxipng(t *testing.T) {
	ff := ffmpegOrSkip(t)
	pq := toolOrSkip(t, "pngquant")
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 64, 48, 3, 10)
	still := filepath.Join(dir, "still.png")
	run(t, ff, enc.PNGStillArgs(m, enc.StillOptions{}, still))
	sd, _ := os.ReadFile(still)
	if si := parseAPNG(t, sd); si.colorType != 6 || si.width != 64 || si.height != 48 || si.hasACTL {
		t.Fatalf("still: ct=%d %dx%d acTL=%v", si.colorType, si.width, si.height, si.hasACTL)
	}
	quant := filepath.Join(dir, "stillq.png")
	runTool(t, pq, enc.PngquantFileArgs(still, quant, 0, 0))
	qd, _ := os.ReadFile(quant)
	qi := parseAPNG(t, qd)
	if qi.colorType != 3 || !qi.plte || !qi.trns {
		t.Errorf("pngquant still: ct=%d plte=%v trns=%v", qi.colorType, qi.plte, qi.trns)
	}
	if ox, err := exec.LookPath("oxipng"); err == nil {
		runTool(t, ox, enc.OxipngArgs(quant, 0))
		od, _ := os.ReadFile(quant)
		if len(od) > len(qd) {
			t.Errorf("oxipng grew %d → %d", len(qd), len(od))
		}
		if oi := parseAPNG(t, od); oi.colorType != 3 {
			t.Errorf("oxipng changed the colour type to %d", oi.colorType)
		}
		run(t, ff, enc.VerifyDecodeArgs(quant))
	}
	// The still with a variant scales (premultiplied) to 32 px.
	small := filepath.Join(dir, "small.png")
	run(t, ff, enc.PNGStillArgs(m, enc.StillOptions{Variant: &enc.Variant{Width: 32}}, small))
	sd, _ = os.ReadFile(small)
	if si := parseAPNG(t, sd); si.width != 32 || si.height != 24 {
		t.Errorf("small still is %dx%d, want 32x24", si.width, si.height)
	}
}

// --- JPEG flatten ------------------------------------------------------------------

// decodeJPEG returns the JPEG's pixel at (x, y) as NRGBA.
func jpegAt(t *testing.T, path string, x, y int) color.NRGBA {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

func TestJPEGFlatten(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 32, 24, 6, 10)

	t.Run("frames on the default matte", func(t *testing.T) {
		run(t, ff, enc.JPEGFramesArgs(m, nil, "", 0, filepath.Join(dir, "f%05d.jpg")))
		files, _ := filepath.Glob(filepath.Join(dir, "f*.jpg"))
		if len(files) != m.Frames {
			t.Fatalf("%d jpegs, want %d: %v", len(files), m.Frames, files)
		}
		if _, err := os.Stat(filepath.Join(dir, "f00001.jpg")); err != nil {
			t.Errorf("numbering does not start at 1: %v", err)
		}
		// Transparent region (x >= 24) is the Discord dark matte, within JPEG error.
		px := jpegAt(t, filepath.Join(dir, "f00001.jpg"), 30, 12)
		if d := absDiff(px.R, 0x31) + absDiff(px.G, 0x33) + absDiff(px.B, 0x38); d > 18 {
			t.Errorf("transparent area is %v, want ~#313338", px)
		}
		// Opaque blue area stays blue.
		if px := jpegAt(t, filepath.Join(dir, "f00001.jpg"), 4, 12); px.B < 200 || px.R > 80 {
			t.Errorf("opaque area is %v, want blue", px)
		}
	})
	t.Run("still on white with a variant", func(t *testing.T) {
		out := filepath.Join(dir, "still.jpg")
		run(t, ff, enc.JPEGStillArgs(m, enc.StillOptions{Matte: "FFFFFF", Quality: 95, Variant: &enc.Variant{Width: 16}}, out))
		f, err := os.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cfg, err := jpeg.DecodeConfig(f)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width != 16 || cfg.Height != 12 {
			t.Errorf("still is %dx%d, want 16x12", cfg.Width, cfg.Height)
		}
		if px := jpegAt(t, out, 15, 6); px.R < 235 || px.G < 235 || px.B < 235 {
			t.Errorf("transparent area is %v, want ~white", px)
		}
	})
	t.Run("frames with an fps variant", func(t *testing.T) {
		sub := filepath.Join(dir, "v")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		v := &enc.Variant{FPS: 5}
		run(t, ff, enc.JPEGFramesArgs(m, v, "", 50, filepath.Join(sub, "f%05d.jpg")))
		files, _ := filepath.Glob(filepath.Join(sub, "f*.jpg"))
		if want := enc.VariantMaster(m, v).Frames; len(files) != want {
			t.Errorf("%d jpegs, want %d", len(files), want)
		}
	})
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// --- frame writers and variants ----------------------------------------------------

func TestFrameWritersOnDisk(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, raw := softMaster(t, dir, 32, 24, 6, 10)

	t.Run("png frames are the master, lossless, numbered from 1", func(t *testing.T) {
		run(t, ff, enc.PNGFramesArgs(m, nil, filepath.Join(dir, "p%05d.png"), 6))
		for i := 1; i <= m.Frames; i++ {
			f, err := os.Open(filepath.Join(dir, fmt.Sprintf("p%05d.png", i)))
			if err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
			img, err := png.Decode(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			n, ok := img.(*image.NRGBA)
			if !ok {
				t.Fatalf("frame %d decodes as %T, want NRGBA", i, img)
			}
			want := raw[(i-1)*32*24*4 : i*32*24*4]
			if !bytes.Equal(n.Pix, want) {
				t.Errorf("frame %d differs from the master", i)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("p%05d.png", m.Frames+1))); err == nil {
			t.Errorf("an extra frame was written")
		}
	})
	t.Run("webp frames are lossless stills with alpha", func(t *testing.T) {
		run(t, ff, enc.WebPFramesArgs(m, nil, filepath.Join(dir, "w%05d.webp")))
		files, _ := filepath.Glob(filepath.Join(dir, "w*.webp"))
		if len(files) != m.Frames {
			t.Fatalf("%d webps, want %d", len(files), m.Frames)
		}
		data, err := os.ReadFile(filepath.Join(dir, "w00001.webp"))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 16 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			t.Fatalf("not a WebP")
		}
		// A lossless still is a bare VP8L (alpha inside) or VP8X+…+VP8L; never VP8 (lossy) or ANIM.
		if bytes.Contains(data, []byte("ANIM")) {
			t.Errorf("frame WebP carries an ANIM chunk")
		}
		if !bytes.Contains(data, []byte("VP8L")) {
			t.Errorf("frame WebP is not lossless (no VP8L chunk): %q", data[12:16])
		}
		if ffmpegMajor(t, ff) >= 9 {
			if got := decodeRGBA(t, ff, filepath.Join(dir, "w00001.webp")); !bytes.Equal(got, raw[:32*24*4]) {
				t.Errorf("lossless WebP frame differs from the master frame")
			}
		}
	})
	t.Run("variant frame count matches VariantMaster", func(t *testing.T) {
		sub := filepath.Join(dir, "v")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		v := &enc.Variant{FPS: 4, Width: 16}
		run(t, ff, enc.PNGFramesArgs(m, v, filepath.Join(sub, "f%05d.png"), 1))
		files, _ := filepath.Glob(filepath.Join(sub, "f*.png"))
		vm := enc.VariantMaster(m, v)
		if len(files) != vm.Frames {
			t.Errorf("%d frames, want %d", len(files), vm.Frames)
		}
		f, err := os.Open(filepath.Join(sub, "f00001.png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cfg, err := png.DecodeConfig(f)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width != vm.Width || cfg.Height != vm.Height || cfg.Width != 16 || cfg.Height != 12 {
			t.Errorf("variant frame is %dx%d, want %dx%d", cfg.Width, cfg.Height, vm.Width, vm.Height)
		}
	})
}

// TestVariantFramesMatchFFmpeg pins VariantMaster's frame-count model
// (floor, matching the fps stage's round=down) to the fps filter's real
// behaviour across integral and fractional rate pairs, and checks the J2
// property on every case: an fps drop never lengthens the clip.
func TestVariantFramesMatchFFmpeg(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	cases := []struct {
		frames int
		in     float64
		out    float64
	}{
		{5, 10, 5}, {6, 10, 5}, {7, 10, 5}, {62, 25, 20}, {62, 25, 12.5}, {62, 25, 100.0 / 6},
		{62, 25, 10}, {7, 30, 12.5}, {90, 30, 25}, {2, 25, 10}, {3, 25, 10}, {30, 29.97, 10},
		{10, 50, 33.333}, {1, 25, 10}, {11, 100.0 / 3, 10},
		// The exactly-5.0 s sticker rungs (J2): 83 and 62 frames — with the
		// filter's default rounding they came out as 84/63 = 5.03/5.04 s.
		{125, 25, 16.7}, {125, 25, 12.5}, {150, 30, 16.7},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%d@%g→%g", tc.frames, tc.in, tc.out)
		t.Run(name, func(t *testing.T) {
			sub := filepath.Join(dir, strings.NewReplacer("@", "_", "→", "_", ".", "_", "/", "_").Replace(name))
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			m, _ := softMaster(t, sub, 4, 4, tc.frames, tc.in)
			v := &enc.Variant{FPS: tc.out}
			run(t, ff, enc.PNGFramesArgs(m, v, filepath.Join(sub, "f%05d.png"), 0))
			files, _ := filepath.Glob(filepath.Join(sub, "f*.png"))
			vm := enc.VariantMaster(m, v)
			if len(files) != vm.Frames {
				t.Errorf("ffmpeg wrote %d frames, VariantMaster predicts %d", len(files), vm.Frames)
			}
			// J2: the written frames at the variant rate must not play longer
			// than the master.
			if got, src := float64(len(files))/vm.FPS, float64(tc.frames)/tc.in; got > src+1e-9 {
				t.Errorf("fps drop lengthened the clip: %d frames / %v fps = %v s > %v s", len(files), vm.FPS, got, src)
			}
		})
	}
}

func TestGIFAndWebPVariantsOnDisk(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, _ := softMaster(t, dir, 32, 24, 10, 25)
	v := &enc.Variant{FPS: 12.5, Width: 16}
	vm := enc.VariantMaster(m, v)

	out := filepath.Join(dir, "v.gif")
	run(t, ff, enc.GIFArgs(m, enc.GIFOptions{HasAlpha: true, Variant: v}, out))
	g := readGIF(t, out)
	if len(g.Image) != vm.Frames || g.Config.Width != vm.Width || g.Config.Height != vm.Height {
		t.Errorf("gif: %d frames %dx%d, want %d frames %dx%d", len(g.Image), g.Config.Width, g.Config.Height, vm.Frames, vm.Width, vm.Height)
	}
	for i, d := range g.Delay {
		if d != 8 { // 1/12.5 s = 8 cs
			t.Errorf("gif frame %d delay %d cs, want 8", i, d)
		}
	}
	// The opaque variant keeps working too.
	op := m
	op.HasAlpha = false
	run(t, ff, enc.GIFArgs(op, enc.GIFOptions{Variant: v}, out))
	if g := readGIF(t, out); len(g.Image) != vm.Frames {
		t.Errorf("opaque gif: %d frames, want %d", len(g.Image), vm.Frames)
	}

	if ffmpegMajor(t, ff) < 9 {
		t.Skip("decoding animated WebP needs FFmpeg 9")
	}
	wout := filepath.Join(dir, "v.webp")
	run(t, ff, enc.WebPArgs(m, enc.WebPOptions{Variant: v}, wout))
	pix := decodeRGBA(t, ff, wout)
	if len(pix) != vm.Width*vm.Height*4*vm.Frames {
		t.Errorf("webp decodes to %d bytes, want %d frames of %dx%d", len(pix), vm.Frames, vm.Width, vm.Height)
	}
}

// TestSequenceInputOnDisk: an image sequence read through SequenceInputArgs
// / an InputPattern plan yields one master frame per file.
func TestSequenceInputOnDisk(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m, raw := softMaster(t, dir, 32, 24, 6, 10)
	seq := filepath.Join(dir, "seq")
	if err := os.MkdirAll(seq, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, ff, enc.PNGFramesArgs(m, nil, filepath.Join(seq, "%06d.png"), 1))

	// Direct: the demuxer args feed a rawvideo dump.
	args := append(enc.SequenceInputArgs(seq, "%06d.png", 10), "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1")
	if got := run(t, ff, args); !bytes.Equal(got, raw) {
		t.Errorf("sequence read back %d bytes, want the %d-byte master", len(got), len(raw))
	}

	// SP-1: "-f image2" makes the open independent of the pattern's extension
	// (the demuxer content-probes the first frame). The same PNG bytes behind
	// a name ffmpeg maps no demuxer to must still render — without the forced
	// demuxer the input fails to open at all, which is how .bin/.gif patterns
	// rendered nothing while the probe (which passes -f image2) succeeded.
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= m.Frames; i++ {
		frame, err := os.ReadFile(filepath.Join(seq, fmt.Sprintf("%06d.png", i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, fmt.Sprintf("%06d.bin", i)), frame, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binArgs := append(enc.SequenceInputArgs(bin, "%06d.bin", 10), "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1")
	if got := run(t, ff, binArgs); !bytes.Equal(got, raw) {
		t.Errorf(".bin sequence read back %d bytes, want the %d-byte master (-f image2 missing?)", len(got), len(raw))
	}

	// Through a plan mirroring graph.Compile's sequence output: InputArgs carry
	// -f image2/-framerate/-start_number/-reinit_filter, the filter starts
	// with the guarding scale head (a pass-through for this uniform sequence),
	// InputPattern the name.
	p := &graph.Plan{
		InputArgs:    []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"},
		Filter:       "[0:v]scale=32:24:force_original_aspect_ratio=decrease:flags=lanczos,fps=10:round=down,format=rgba[out]",
		OutLabel:     "[out]",
		Width:        32,
		Height:       24,
		FPS:          10,
		HasAlpha:     true,
		Duration:     0.6,
		Frames:       6,
		Speed:        1,
		SourceFPS:    10,
		InputPattern: "%06d.png",
	}
	master := filepath.Join(dir, "seq.rgba")
	run(t, ff, enc.MasterArgs(seq, p, master))
	got, err := os.ReadFile(master)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("plan master %d bytes, want %d", len(got), len(raw))
	}
	// Still at t=0.35 (slot 3 = file 000004) and the from-start variant agree.
	for name, a := range map[string][]string{"seek": enc.StillArgs(seq, p, 0.35, 0), "from start": enc.StillArgsFromStart(seq, p, 0.35, 0)} {
		data := run(t, ff, a)
		if len(data) == 0 {
			t.Errorf("%s: no still (args %q)", name, a)
			continue
		}
		img := decodePNG(t, data)
		if !bytes.Equal(img.Pix, raw[3*32*24*4:4*32*24*4]) {
			t.Errorf("%s: still is not frame 3", name)
		}
	}
}
