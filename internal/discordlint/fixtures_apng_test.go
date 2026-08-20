package discordlint

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// PNG/APNG test helpers: a chunk-level re-serialiser (with real CRCs, so
// the surgery output stays valid for other decoders), and a synthetic APNG
// builder with dummy pixel data (the linter never inflates IDAT/fdAT).

// rawChunk is a chunk as the tests manipulate it.
type rawChunk struct {
	typ  string
	data []byte
}

// encodeChunk serialises one chunk with a correct CRC.
func encodeChunk(c rawChunk) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(c.data)))
	out = append(out, c.typ...)
	out = append(out, c.data...)
	crc := crc32.NewIEEE()
	crc.Write([]byte(c.typ))
	crc.Write(c.data)
	return binary.BigEndian.AppendUint32(out, crc.Sum32())
}

// encodePNG serialises signature + chunks.
func encodePNG(chunks []rawChunk) []byte {
	out := append([]byte(nil), pngSignature...)
	for _, c := range chunks {
		out = append(out, encodeChunk(c)...)
	}
	return out
}

// splitChunks parses data into raw chunks with an independent minimal
// walker (it does not use parsePNG, so the round-trip test means something).
func splitChunks(t testing.TB, data []byte) []rawChunk {
	t.Helper()
	if !bytes.HasPrefix(data, pngSignature) {
		t.Fatal("not a PNG")
	}
	var out []rawChunk
	pos := len(pngSignature)
	for pos < len(data) {
		if len(data)-pos < 12 {
			t.Fatalf("stray %d bytes at %d", len(data)-pos, pos)
		}
		n := int(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		if pos+12+n > len(data) {
			t.Fatalf("chunk %s at %d overruns the file", typ, pos)
		}
		out = append(out, rawChunk{typ: typ, data: append([]byte(nil), data[pos+8:pos+8+n]...)})
		pos += 12 + n
	}
	return out
}

// chunkTypes lists the chunk types in order.
func chunkTypes(chunks []rawChunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.typ
	}
	return out
}

// nthChunk returns the index of the n-th (0-based) chunk of the given type.
func nthChunk(t testing.TB, chunks []rawChunk, typ string, n int) int {
	t.Helper()
	seen := 0
	for i, c := range chunks {
		if c.typ == typ {
			if seen == n {
				return i
			}
			seen++
		}
	}
	t.Fatalf("no %s chunk #%d", typ, n)
	return -1
}

// mutatePNG splits data, lets fn edit the chunk list and re-serialises it.
func mutatePNG(t testing.TB, data []byte, fn func(chunks []rawChunk) []rawChunk) []byte {
	t.Helper()
	return encodePNG(fn(splitChunks(t, data)))
}

// patchFCTL edits the fcTL of frame n (0-based, fcTL order) in place.
func patchFCTL(t testing.TB, data []byte, n int, fn func(fctl []byte)) []byte {
	t.Helper()
	return mutatePNG(t, data, func(chunks []rawChunk) []rawChunk {
		fn(chunks[nthChunk(t, chunks, "fcTL", n)].data)
		return chunks
	})
}

// patchAllFCTL edits every fcTL in place.
func patchAllFCTL(t testing.TB, data []byte, fn func(fctl []byte)) []byte {
	t.Helper()
	return mutatePNG(t, data, func(chunks []rawChunk) []rawChunk {
		for i := range chunks {
			if chunks[i].typ == "fcTL" {
				fn(chunks[i].data)
			}
		}
		return chunks
	})
}

// setDelay writes delay_num/delay_den into an fcTL payload.
func setDelay(fctl []byte, num, den uint16) {
	binary.BigEndian.PutUint16(fctl[20:22], num)
	binary.BigEndian.PutUint16(fctl[22:24], den)
}

// setFrameRect writes width/height/x/y into an fcTL payload.
func setFrameRect(fctl []byte, w, h, x, y uint32) {
	binary.BigEndian.PutUint32(fctl[4:8], w)
	binary.BigEndian.PutUint32(fctl[8:12], h)
	binary.BigEndian.PutUint32(fctl[12:16], x)
	binary.BigEndian.PutUint32(fctl[16:20], y)
}

// patchIHDRDims rewrites the IHDR width/height.
func patchIHDRDims(t testing.TB, data []byte, w, h uint32) []byte {
	t.Helper()
	return mutatePNG(t, data, func(chunks []rawChunk) []rawChunk {
		ihdr := chunks[nthChunk(t, chunks, "IHDR", 0)].data
		binary.BigEndian.PutUint32(ihdr[0:4], w)
		binary.BigEndian.PutUint32(ihdr[4:8], h)
		return chunks
	})
}

// patchACTL rewrites num_frames / num_plays.
func patchACTL(t testing.TB, data []byte, frames, plays uint32) []byte {
	t.Helper()
	return mutatePNG(t, data, func(chunks []rawChunk) []rawChunk {
		actl := chunks[nthChunk(t, chunks, "acTL", 0)].data
		binary.BigEndian.PutUint32(actl[0:4], frames)
		binary.BigEndian.PutUint32(actl[4:8], plays)
		return chunks
	})
}

// resizeCanvas makes an APNG fixture w x h: IHDR and the default frame's
// fcTL (which must cover the canvas) are rewritten; the other frames keep
// their (smaller) rectangles.
func resizeCanvas(t testing.TB, data []byte, w, h uint32) []byte {
	t.Helper()
	out := patchIHDRDims(t, data, w, h)
	return patchFCTL(t, out, 0, func(fctl []byte) { setFrameRect(fctl, w, h, 0, 0) })
}

// --- synthetic APNG builder -------------------------------------------------

// synthFrame describes one fcTL of a synthetic APNG.
type synthFrame struct {
	w, h, x, y uint32
	num, den   uint16
	dispose    byte
	blend      byte
}

// synthAPNG describes a synthetic (A)PNG. Pixel payloads are dummies.
type synthAPNG struct {
	w, h        uint32
	colorType   byte
	bitDepth    byte // 0 → 8
	plte        int  // palette entries (0 = no PLTE)
	trns        int  // tRNS bytes (0 = no tRNS)
	animated    bool // write acTL (+ fcTL/fdAT)
	plays       uint32
	frames      []synthFrame // animation frames; frames[0] is the default image unless hideDefault
	hideDefault bool         // the IDAT image gets no fcTL (not part of the animation)
}

// fcTLPayload builds a 26-byte fcTL payload.
func fcTLPayload(seq uint32, f synthFrame) []byte {
	p := binary.BigEndian.AppendUint32(nil, seq)
	p = binary.BigEndian.AppendUint32(p, f.w)
	p = binary.BigEndian.AppendUint32(p, f.h)
	p = binary.BigEndian.AppendUint32(p, f.x)
	p = binary.BigEndian.AppendUint32(p, f.y)
	p = binary.BigEndian.AppendUint16(p, f.num)
	p = binary.BigEndian.AppendUint16(p, f.den)
	return append(p, f.dispose, f.blend)
}

// chunks builds the chunk list.
func (s synthAPNG) chunks() []rawChunk {
	depth := s.bitDepth
	if depth == 0 {
		depth = 8
	}
	ihdr := binary.BigEndian.AppendUint32(nil, s.w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, s.h)
	ihdr = append(ihdr, depth, s.colorType, 0, 0, 0)
	out := []rawChunk{{"IHDR", ihdr}}
	if s.plte > 0 {
		out = append(out, rawChunk{"PLTE", bytes.Repeat([]byte{1, 2, 3}, s.plte)})
	}
	if s.trns > 0 {
		out = append(out, rawChunk{"tRNS", bytes.Repeat([]byte{0x80}, s.trns)})
	}
	dummy := []byte{0x78, 0x9C, 0x63, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01} // a tiny zlib stream; never inflated
	if !s.animated {
		out = append(out, rawChunk{"IDAT", dummy}, rawChunk{"IEND", nil})
		return out
	}
	actl := binary.BigEndian.AppendUint32(nil, uint32(len(s.frames)))
	actl = binary.BigEndian.AppendUint32(actl, s.plays)
	out = append(out, rawChunk{"acTL", actl})
	seq := uint32(0)
	rest := s.frames
	if !s.hideDefault && len(s.frames) > 0 {
		out = append(out, rawChunk{"fcTL", fcTLPayload(seq, s.frames[0])})
		seq++
		rest = s.frames[1:]
	}
	out = append(out, rawChunk{"IDAT", dummy})
	for _, f := range rest {
		out = append(out, rawChunk{"fcTL", fcTLPayload(seq, f)})
		seq++
		out = append(out, rawChunk{"fdAT", append(binary.BigEndian.AppendUint32(nil, seq), dummy...)})
		seq++
	}
	return append(out, rawChunk{"IEND", nil})
}

func (s synthAPNG) bytes() []byte { return encodePNG(s.chunks()) }

// goodSynthAPNG is a compliant 64x64 RGBA 4-frame animation at 10 fps.
func goodSynthAPNG() synthAPNG {
	return synthAPNG{
		w: 64, h: 64, colorType: pngRGBA, animated: true, plays: 0,
		frames: []synthFrame{
			{w: 64, h: 64, num: 1, den: 10},
			{w: 32, h: 32, x: 8, y: 8, num: 1, den: 10, blend: 1},
			{w: 64, h: 16, x: 0, y: 48, num: 1, den: 10, dispose: 1},
			{w: 64, h: 64, num: 1, den: 10},
		},
	}
}

// lintAPNG runs LintAPNG and fails the test on error.
func lintAPNG(t testing.TB, data []byte, target Target) Report {
	t.Helper()
	r, err := LintAPNG(data, target)
	if err != nil {
		t.Fatalf("LintAPNG: %v", err)
	}
	return r
}

// lintStatic runs LintStatic and fails the test on error.
func lintStatic(t testing.TB, format string, data []byte, target Target) Report {
	t.Helper()
	r, err := LintStatic(format, data, target)
	if err != nil {
		t.Fatalf("LintStatic: %v", err)
	}
	return r
}
