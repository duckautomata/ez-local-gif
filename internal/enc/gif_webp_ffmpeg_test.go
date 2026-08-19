package enc_test

// Real-ffmpeg checks of the GIF/WebP encoder argv facts the golden tests
// encode: palettegen refuses max_colors=2 with reserve_transparent=1 (so
// GIFOptions.Colors is clamped to >= 3 with alpha), the gif muxer's -loop is
// the NETSCAPE count and the webp muxer's -loop is written verbatim into
// the ANIM chunk (so WebPArgs passes N+1 plays for Output.Loop N). Skips
// when ffmpeg is not on PATH. The master is synthesised in Go, so this
// needs no decoder beyond ffmpeg itself.

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/gif"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
)

// rgbaMaster writes a frames-long 16x16 RGBA master whose left half is a
// moving opaque colour bar and whose right half is fully transparent.
func rgbaMaster(t *testing.T, dir string, frames int) enc.Master {
	t.Helper()
	const w, h = 16, 16
	buf := make([]byte, 0, w*h*4*frames)
	for f := 0; f < frames; f++ {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				switch {
				case x >= w/2:
					buf = append(buf, 0, 0, 0, 0) // transparent
				case y == (f*3)%h:
					buf = append(buf, 255, 40, 40, 255) // moving red bar
				default:
					buf = append(buf, 40, 40, 255, 255) // blue
				}
			}
		}
	}
	path := filepath.Join(dir, "frames.rgba")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return enc.Master{Path: path, Width: w, Height: h, FPS: 10, Frames: frames, HasAlpha: true}
}

// argIndex returns the index of s in args (or -1).
func argIndex(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

// tryFF runs ffmpeg and returns its combined output and error (not fatal).
func tryFF(ff string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	full := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)
	return exec.CommandContext(ctx, ff, full...).CombinedOutput()
}

func TestGIFArgsAlphaMinColors(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m := rgbaMaster(t, dir, 4)

	out := filepath.Join(dir, "c2.gif")
	args := enc.GIFArgs(m, enc.GIFOptions{Colors: 2, HasAlpha: true}, out)
	filter := args[argIndex(args, "-filter_complex")+1]
	if !strings.Contains(filter, "palettegen=max_colors=3:reserve_transparent=1") {
		t.Fatalf("Colors 2 with alpha must be raised to max_colors=3: %s", filter)
	}
	if o, err := tryFF(ff, args); err != nil {
		t.Fatalf("GIFArgs(Colors 2, alpha) failed: %v\n%s", err, o)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Image) != 4 {
		t.Errorf("got %d frames, want 4", len(g.Image))
	}
	// max_colors=3 = 2 colours + the transparent slot: at most 3 distinct
	// palette indices are used across all frames (the encoder always writes a
	// full 256-entry table, so count the indices actually referenced).
	used := map[uint8]bool{}
	for _, fr := range g.Image {
		for _, idx := range fr.Pix {
			used[idx] = true
		}
	}
	if len(used) > 3 {
		t.Errorf("%d distinct palette indices used, want <= 3 (max_colors=3 incl. transparent)", len(used))
	}

	// The clamp is load-bearing: max_colors=2 with reserve_transparent=1 is
	// rejected by palettegen (FFmpeg 9.0.1: "max_colors=2 is only allowed
	// without reserving a transparent color slot").
	bad := append([]string{}, args...)
	bad[argIndex(bad, "-filter_complex")+1] = strings.Replace(filter, "max_colors=3", "max_colors=2", 1)
	if o, err := tryFF(ff, bad); err == nil {
		t.Errorf("max_colors=2 with reserve_transparent=1 unexpectedly succeeded; the >= 3 clamp may be unnecessary on this ffmpeg\n%s", o)
	} else {
		t.Logf("max_colors=2 + reserve_transparent=1 rejected as expected: %s", strings.TrimSpace(string(o)))
	}

	// Without alpha, 2 colours is fine.
	m2 := m
	m2.HasAlpha = false
	out2 := filepath.Join(dir, "c2-opaque.gif")
	if o, err := tryFF(ff, enc.GIFArgs(m2, enc.GIFOptions{Colors: 2}, out2)); err != nil {
		t.Fatalf("GIFArgs(Colors 2, no alpha) failed: %v\n%s", err, o)
	}
}

func TestLoopCountsOnDisk(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	m := rgbaMaster(t, dir, 4)

	// GIF: -loop N is the NETSCAPE count (Go's decoder reports it as-is;
	// 0 = forever).
	for _, loop := range []int{0, 3} {
		out := filepath.Join(dir, "loop.gif")
		if o, err := tryFF(ff, enc.GIFArgs(m, enc.GIFOptions{Loop: loop, HasAlpha: true}, out)); err != nil {
			t.Fatalf("GIFArgs(Loop %d): %v\n%s", loop, err, o)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if g.LoopCount != loop {
			t.Errorf("GIF Loop %d: NETSCAPE count on disk = %d", loop, g.LoopCount)
		}
	}

	// WebP: the ANIM chunk's loop count is the number of plays, so Output.Loop
	// N (play N+1 times) is written as N+1 and 0 stays 0.
	for _, tc := range []struct{ loop, wantPlays int }{{0, 0}, {3, 4}, {1, 2}} {
		out := filepath.Join(dir, "loop.webp")
		if o, err := tryFF(ff, enc.WebPArgs(m, enc.WebPOptions{Loop: tc.loop}, out)); err != nil {
			t.Fatalf("WebPArgs(Loop %d): %v\n%s", tc.loop, err, o)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		plays, ok := webpANIMLoopCount(data)
		if !ok {
			t.Fatalf("WebP Loop %d: no ANIM chunk in %d bytes", tc.loop, len(data))
		}
		if plays != tc.wantPlays {
			t.Errorf("WebP Loop %d: ANIM loop count on disk = %d plays, want %d", tc.loop, plays, tc.wantPlays)
		}
	}
}

// webpANIMLoopCount walks the RIFF chunks of a WebP and returns the ANIM
// chunk's loop count (bytes 4..5 after the background colour).
func webpANIMLoopCount(data []byte) (int, bool) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, false
	}
	for off := 12; off+8 <= len(data); {
		fourcc := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4:]))
		body := off + 8
		if body+size > len(data) {
			return 0, false
		}
		if fourcc == "ANIM" && size >= 6 {
			return int(binary.LittleEndian.Uint16(data[body+4:])), true
		}
		off = body + size + size%2 // chunks are padded to even sizes
	}
	return 0, false
}
