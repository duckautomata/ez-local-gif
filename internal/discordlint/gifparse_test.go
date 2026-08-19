package discordlint

import (
	"bytes"
	"errors"
	"image/gif"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// allGIFFixtures returns every ffmpeg-made and synthetic GIF used by the
// round-trip tests.
func allGIFFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	files, err := filepath.Glob(filepath.Join("testdata", "*.gif"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 {
		t.Fatalf("expected at least 3 GIF fixtures in testdata, found %d", len(files))
	}
	for _, f := range files {
		out[filepath.Base(f)] = readFixture(t, filepath.Base(f))
	}
	out["syn_opaque"] = encodeFx(t, opaqueAnim())
	out["syn_alpha"] = encodeFx(t, alphaAnim())
	out["syn_opaque_frame0"] = encodeFx(t, opaqueFrame0Anim())
	noLoop := opaqueAnim()
	noLoop.loop = -1
	out["syn_noloop"] = encodeFx(t, noLoop)
	out["syn_comment"] = insertAfterGCT(encodeFx(t, opaqueAnim()), []byte{0x21, 0xFE, 0x05, 'h', 'e', 'l', 'l', 'o', 0x00})
	return out
}

func TestParseGIFRoundTripIsByteExact(t *testing.T) {
	for name, data := range allGIFFixtures(t) {
		t.Run(name, func(t *testing.T) {
			g, err := parseGIF(data)
			if err != nil {
				t.Fatalf("parseGIF: %v", err)
			}
			if !g.hasTrailer {
				t.Fatal("trailer not detected")
			}
			if out := g.encode(); !bytes.Equal(out, data) {
				t.Fatalf("re-encoded bytes differ (in %d bytes, out %d bytes)", len(data), len(out))
			}
			// The GIF must also survive image/gif, which validates the LZW
			// streams and block structure independently.
			decodeGIF(t, data)
		})
	}
}

func TestParseGIFStructureOfFFmpegAlpha(t *testing.T) {
	g, err := parseGIF(readFixture(t, "ff_alpha.gif"))
	if err != nil {
		t.Fatal(err)
	}
	if string(g.header[:]) != "GIF89a" || g.width != 64 || g.height != 64 {
		t.Errorf("header/LSD: %q %dx%d", g.header[:], g.width, g.height)
	}
	if !g.hasGCT() || g.gctColors() != 256 || len(g.gct) != 768 {
		t.Errorf("GCT: flag=%v colours=%d bytes=%d", g.hasGCT(), g.gctColors(), len(g.gct))
	}
	if g.bgIndex != 255 {
		t.Errorf("bg index %d, want 255 (ffmpeg writes the reserved transparent index)", g.bgIndex)
	}
	if len(g.blocks) != 21 {
		t.Fatalf("blocks: %d, want 21 (NETSCAPE + 10 x (GCE, image))", len(g.blocks))
	}
	app, ok := g.blocks[0].(*gifAppExt)
	if !ok || !app.isNetscape() {
		t.Fatalf("block 0: %T, want NETSCAPE app ext", g.blocks[0])
	}
	if n, ok := app.loopCount(); !ok || n != 0 {
		t.Errorf("loop count %d ok=%v, want 0", n, ok)
	}
	frames, dangling := g.frames()
	if len(frames) != 10 || len(dangling) != 0 {
		t.Fatalf("frames %d dangling %d", len(frames), len(dangling))
	}
	for i, f := range frames {
		if f.gce == nil {
			t.Fatalf("frame %d has no GCE", i)
		}
		if f.gce.disposal != 2 || !f.gce.transparent || f.gce.transIndex != 255 || f.gce.delayCS != 10 {
			t.Errorf("frame %d GCE: %+v", i, *f.gce)
		}
		if f.image.hasLCT() || f.image.interlaced() || f.image.minCodeSize != 8 {
			t.Errorf("frame %d image: packed=%02x mcs=%d", i, f.image.packed, f.image.minCodeSize)
		}
		if len(f.dupGCE) != 0 {
			t.Errorf("frame %d has duplicate GCEs", i)
		}
	}
	// ffmpeg crops transparent frames to their bounding box.
	if f0 := frames[0].image; f0.left != 13 || f0.top != 13 || f0.width != 39 || f0.height != 39 {
		t.Errorf("frame 0 rect: %d,%d %dx%d", f0.left, f0.top, f0.width, f0.height)
	}
}

func TestParseGIFErrors(t *testing.T) {
	good := encodeFx(t, opaqueAnim())
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "shorter than header"},
		{"short", []byte("GIF89a"), "shorter than header"},
		{"signature", append([]byte("PNG89a"), good[6:]...), "bad signature"},
		{"truncated GCT", good[:20], "truncated global colour table"},
		{"truncated image", good[:len(good)-10], "image"},
		{"unknown introducer", append(append([]byte(nil), good[:fxHeaderLen]...), 0x7F), "unknown block introducer 0x7F"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseGIF(c.data)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
			if _, _, err := LintGIF(c.data, TargetNone, true); err == nil {
				t.Fatal("LintGIF accepted an unparseable file")
			}
		})
	}
	// Truncation inside a block must be reported as an unexpected EOF.
	if _, err := parseGIF(good[:len(good)-10]); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated image error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestParseGIFTolerance(t *testing.T) {
	good := encodeFx(t, opaqueAnim())

	t.Run("missing trailer", func(t *testing.T) {
		g, err := parseGIF(good[:len(good)-1])
		if err != nil {
			t.Fatal(err)
		}
		if g.hasTrailer {
			t.Error("trailer reported present")
		}
		if frames, _ := g.frames(); len(frames) != 3 {
			t.Errorf("frames %d, want 3", len(frames))
		}
		if !bytes.Equal(g.encode(), good) {
			t.Error("encode should append the trailer and reproduce the good file")
		}
	})

	t.Run("bytes after trailer", func(t *testing.T) {
		junk := append(append([]byte(nil), good...), 'j', 'u', 'n', 'k')
		g, err := parseGIF(junk)
		if err != nil {
			t.Fatal(err)
		}
		if string(g.trailing) != "junk" {
			t.Errorf("trailing = %q", g.trailing)
		}
		if !bytes.Equal(g.encode(), junk) {
			t.Error("trailing bytes not preserved")
		}
	})

	t.Run("odd GCE size and unknown extension", func(t *testing.T) {
		// A 5-byte GCE payload and an extension with an unknown label are
		// kept verbatim.
		odd := insertAfterGCT(good, []byte{
			0x21, 0xF9, 0x05, 0x09, 0x0A, 0x00, 0x03, 0xAA, 0x00, // GCE, disposal 2, delay 10, transparent 3, extra byte
			0x21, 0x42, 0x02, 0x01, 0x02, 0x00, // unknown label 0x42
		})
		g, err := parseGIF(odd)
		if err != nil {
			t.Fatal(err)
		}
		gce, ok := g.blocks[0].(*gifGCE)
		if !ok {
			t.Fatalf("block 0 is %T", g.blocks[0])
		}
		if gce.disposal != 2 || gce.delayCS != 10 || !gce.transparent || gce.transIndex != 3 {
			t.Errorf("GCE fields: %+v", *gce)
		}
		if ext, ok := g.blocks[1].(*gifRawExt); !ok || ext.label != 0x42 {
			t.Errorf("block 1: %#v", g.blocks[1])
		}
		if !bytes.Equal(g.encode(), odd) {
			t.Error("odd blocks not preserved verbatim")
		}
		// Once modified the GCE is emitted in canonical 8-byte form.
		gce.setDisposal(1)
		var w bytes.Buffer
		gce.encode(&w)
		if want := []byte{0x21, 0xF9, 0x04, 0x05, 0x0A, 0x00, 0x03, 0x00}; !bytes.Equal(w.Bytes(), want) {
			t.Errorf("canonical GCE = % X, want % X", w.Bytes(), want)
		}
	})
}

// TestDecodeIndicesMatchesImageGIF cross-checks the compress/lzw index
// analysis against image/gif's decoded pixel arrays.
func TestDecodeIndicesMatchesImageGIF(t *testing.T) {
	for name, data := range allGIFFixtures(t) {
		t.Run(name, func(t *testing.T) {
			g, err := parseGIF(data)
			if err != nil {
				t.Fatal(err)
			}
			ref := decodeGIF(t, data)
			frames, _ := g.frames()
			if len(frames) != len(ref.Image) {
				t.Fatalf("frames %d vs image/gif %d", len(frames), len(ref.Image))
			}
			for i, f := range frames {
				pix, err := decodeIndices(f.image)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				var want [256]bool
				for _, v := range ref.Image[i].Pix {
					want[v] = true
				}
				if pix.used != want || pix.pixels != len(ref.Image[i].Pix) {
					t.Errorf("frame %d: index usage differs from image/gif", i)
				}
			}
		})
	}
}

func TestDecodeIndicesRejectsBadData(t *testing.T) {
	g, err := parseGIF(encodeFx(t, opaqueAnim()))
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := g.frames()
	img := *frames[0].image
	img.minCodeSize = 12
	if _, err := decodeIndices(&img); err == nil {
		t.Error("minimum code size 12 accepted")
	}
	img = *frames[0].image
	img.data = [][]byte{{0x00}} // far too little data for 256 pixels
	if _, err := decodeIndices(&img); err == nil {
		t.Error("truncated LZW stream accepted")
	}
	img = *frames[0].image
	img.width, img.height = 0, 0
	pix, err := decodeIndices(&img)
	if err != nil || pix.pixels != 0 || !pix.usesOnly(0) {
		t.Errorf("empty frame: pix=%+v err=%v", pix, err)
	}
}

// TestGIFBlockHelpers covers the small mutation helpers.
func TestGIFBlockHelpers(t *testing.T) {
	app := newNetscapeLoop(0)
	if !app.isNetscape() {
		t.Error("newNetscapeLoop is not NETSCAPE")
	}
	if n, ok := app.loopCount(); !ok || n != 0 {
		t.Errorf("loop %d ok=%v", n, ok)
	}
	var w bytes.Buffer
	app.encode(&w)
	want := append([]byte{0x21, 0xFF, 0x0B}, []byte("NETSCAPE2.0")...)
	want = append(want, 0x03, 0x01, 0x00, 0x00, 0x00)
	if !bytes.Equal(w.Bytes(), want) {
		t.Errorf("encode = % X, want % X", w.Bytes(), want)
	}
	app.setLoopCount(7)
	if n, _ := app.loopCount(); n != 7 {
		t.Errorf("setLoopCount: %d", n)
	}
	// A NETSCAPE block without a loop sub-block gets one.
	bare := &gifAppExt{sub: [][]byte{[]byte("NETSCAPE2.0")}}
	if _, ok := bare.loopCount(); ok {
		t.Error("bare block reports a loop count")
	}
	bare.setLoopCount(0)
	if n, ok := bare.loopCount(); !ok || n != 0 {
		t.Errorf("after setLoopCount: %d ok=%v", n, ok)
	}

	g := &gifFile{}
	g.insertBlock(0, newGCE(10))
	g.insertBlock(1, newGCE(20))
	g.insertBlock(0, newGCE(5))
	g.removeBlocks([]int{1})
	if len(g.blocks) != 2 || g.blocks[0].(*gifGCE).delayCS != 5 || g.blocks[1].(*gifGCE).delayCS != 20 {
		t.Errorf("insert/remove: %+v", g.blocks)
	}
	// image/gif reads the canonical GCE we synthesise.
	full := encodeFx(t, opaqueAnim())
	if _, err := gif.DecodeAll(bytes.NewReader(full)); err != nil {
		t.Fatal(err)
	}
}
