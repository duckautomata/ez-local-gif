package discordlint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// PNG / APNG chunk-level parser shared by LintAPNG and LintStatic("png").
// It reads the container structure only — IHDR, PLTE, tRNS, acTL, fcTL,
// fdAT, IDAT, IEND — and never inflates pixel data. Chunk CRCs are not
// verified (decoders differ on whether they care, and the encoders are
// ours). Structural problems are collected in pngFile.problems and reported
// through apng.container; parsePNG itself fails only when the data is not a
// PNG file at all.

// pngSignature is the 8-byte PNG file signature.
var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

// PNG colour types (IHDR byte 9).
const (
	pngGray      = 0
	pngRGB       = 2
	pngIndexed   = 3
	pngGrayAlpha = 4
	pngRGBA      = 6
)

// pngMaxDimension is the largest width/height the PNG spec allows (2^31-1).
const pngMaxDimension = 1<<31 - 1

// Fixed chunk payload sizes.
const (
	pngIHDRLen = 13
	pngACTLLen = 8
	pngFCTLLen = 26
)

// pngMaxAncillaryTypes caps how many distinct unknown chunk types are listed
// in the container detail; a hostile file can carry millions of distinct
// tiny chunks, and the count beyond the cap is reported instead.
const pngMaxAncillaryTypes = 32

// pngChunk is one raw chunk.
type pngChunk struct {
	typ    string
	data   []byte
	offset int // offset of the 4-byte length field within the file
}

// pngIHDR is the decoded image header.
type pngIHDR struct {
	width, height uint32
	bitDepth      byte
	colorType     byte
	compression   byte
	filter        byte
	interlace     byte
}

// pngACTL is the decoded animation control chunk.
type pngACTL struct {
	numFrames uint32
	numPlays  uint32 // 0 = loop forever
}

// apngFrame is one fcTL chunk, i.e. one animation frame.
type apngFrame struct {
	index         int // 0-based frame number in fcTL order
	chunk         int // index into pngFile.chunks
	seq           uint32
	width, height uint32
	x, y          uint32
	delayNum      uint16
	delayDen      uint16 // 0 means 100 (APNG spec)
	dispose       byte
	blend         byte
	isDefault     bool // the fcTL precedes IDAT: the default image is this frame
	dataChunks    int  // IDAT (default frame) or fdAT chunks carrying its pixels
}

// delayUS returns the frame delay in microseconds, rounded to nearest
// (delay_den 0 is read as 100 per the APNG spec).
func (fr *apngFrame) delayUS() int64 {
	den := int64(fr.delayDen)
	if den == 0 {
		den = 100
	}
	return (int64(fr.delayNum)*1_000_000 + den/2) / den
}

// delayMS returns the frame delay in milliseconds, rounded to nearest.
func (fr *apngFrame) delayMS() int {
	return int((fr.delayUS() + 500) / 1000)
}

// pngFile is a parsed PNG/APNG container.
type pngFile struct {
	size      int
	chunks    []pngChunk
	ihdr      *pngIHDR
	actl      *pngACTL
	plte      int // palette entries; -1 when there is no PLTE chunk
	trns      []byte
	hasTRNS   bool
	frames    []apngFrame
	idat      int // IDAT chunk count
	fdat      int // fdAT chunk count
	hasIEND   bool
	trailing  int      // bytes after IEND (ignored by decoders)
	ancillary []string // other chunk types seen, in order of first appearance (at most pngMaxAncillaryTypes)
	problems  []string // structural problems (apng.container)

	ancillarySeen map[string]struct{} // O(1) dedupe for ancillary
	ancillaryMore int                 // distinct unknown types beyond pngMaxAncillaryTypes

	nextSeq uint32 // expected next fcTL/fdAT sequence number
}

// parsePNG parses a PNG or APNG file. It returns an error only when data
// does not start with the PNG signature; everything else is collected as
// problems on the returned file.
func parsePNG(data []byte) (*pngFile, error) {
	if len(data) < len(pngSignature) || !bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return nil, errors.New("png: not a PNG file (bad signature)")
	}
	f := &pngFile{size: len(data), plte: -1}
	pos := len(pngSignature)
	for pos < len(data) {
		if len(data)-pos < 8 {
			f.problem("%d stray bytes after the last chunk at offset %d", len(data)-pos, pos)
			break
		}
		length := binary.BigEndian.Uint32(data[pos:])
		typ := string(data[pos+4 : pos+8])
		avail := len(data) - pos - 12 // payload bytes available once length, type and CRC are accounted for
		if avail < 0 || uint64(length) > uint64(avail) {
			f.problem("chunk %q at offset %d declares %d bytes but only %d remain (truncated file)", printableID(typ), pos, length, max(len(data)-pos-8, 0))
			break
		}
		c := pngChunk{typ: typ, data: data[pos+8 : pos+8+int(length)], offset: pos}
		f.chunks = append(f.chunks, c)
		pos += 12 + int(length)
		f.addChunk(len(f.chunks)-1, c)
		if f.hasIEND {
			f.trailing = len(data) - pos
			break
		}
	}
	f.finish()
	return f, nil
}

func (f *pngFile) problem(format string, args ...any) {
	f.problems = append(f.problems, fmt.Sprintf(format, args...))
}

// addChunk interprets chunk i.
func (f *pngFile) addChunk(i int, c pngChunk) {
	switch c.typ {
	case "IHDR":
		f.addIHDR(i, c)
	case "PLTE":
		f.addPLTE(c)
	case "tRNS":
		if f.hasTRNS {
			f.problem("duplicate tRNS chunk")
			return
		}
		if f.idat > 0 {
			f.problem("tRNS chunk after the first IDAT")
		}
		f.hasTRNS = true
		f.trns = c.data
	case "acTL":
		f.addACTL(c)
	case "fcTL":
		f.addFCTL(i, c)
	case "fdAT":
		f.addFDAT(i, c)
	case "IDAT":
		if f.fdat > 0 {
			f.problem("IDAT chunk after fdAT data")
		}
		f.idat++
		if n := len(f.frames); n > 0 && f.frames[n-1].isDefault {
			f.frames[n-1].dataChunks++ // the default image's pixels belong to its fcTL
		}
	case "IEND":
		f.hasIEND = true
	default:
		if _, seen := f.ancillarySeen[c.typ]; seen {
			return
		}
		if f.ancillarySeen == nil {
			f.ancillarySeen = make(map[string]struct{})
		}
		f.ancillarySeen[c.typ] = struct{}{}
		if len(f.ancillary) < pngMaxAncillaryTypes {
			f.ancillary = append(f.ancillary, c.typ)
		} else {
			f.ancillaryMore++
		}
	}
}

func (f *pngFile) addIHDR(i int, c pngChunk) {
	if f.ihdr != nil {
		f.problem("duplicate IHDR chunk")
		return
	}
	if i != 0 {
		f.problem("IHDR is chunk %d, not the first", i)
	}
	if len(c.data) != pngIHDRLen {
		f.problem("IHDR payload is %d bytes, need %d", len(c.data), pngIHDRLen)
		return
	}
	h := &pngIHDR{
		width:       binary.BigEndian.Uint32(c.data[0:4]),
		height:      binary.BigEndian.Uint32(c.data[4:8]),
		bitDepth:    c.data[8],
		colorType:   c.data[9],
		compression: c.data[10],
		filter:      c.data[11],
		interlace:   c.data[12],
	}
	f.ihdr = h
	if h.width == 0 || h.height == 0 || h.width > pngMaxDimension || h.height > pngMaxDimension {
		f.problem("IHDR dimensions %dx%d are outside 1..%d", h.width, h.height, pngMaxDimension)
	}
	if !pngValidDepth(h.colorType, h.bitDepth) {
		f.problem("colour type %d with bit depth %d is not a valid PNG combination", h.colorType, h.bitDepth)
	}
	if h.compression != 0 || h.filter != 0 {
		f.problem("IHDR compression/filter method %d/%d, want 0/0", h.compression, h.filter)
	}
	if h.interlace > 1 {
		f.problem("IHDR interlace method %d, want 0 or 1", h.interlace)
	}
}

// pngValidDepth reports whether the colour type / bit depth pair is one the
// PNG spec allows.
func pngValidDepth(colorType, depth byte) bool {
	switch colorType {
	case pngGray:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8 || depth == 16
	case pngIndexed:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8
	case pngRGB, pngGrayAlpha, pngRGBA:
		return depth == 8 || depth == 16
	}
	return false
}

func (f *pngFile) addPLTE(c pngChunk) {
	if f.plte >= 0 {
		f.problem("duplicate PLTE chunk")
		return
	}
	if f.idat > 0 {
		f.problem("PLTE chunk after the first IDAT")
	}
	// For indexed colour the spec requires tRNS after PLTE; libpng silently
	// discards a tRNS it sees first, so the palette alpha is lost.
	if f.hasTRNS && f.ihdr != nil && f.ihdr.colorType == pngIndexed {
		f.problem("tRNS chunk appears before PLTE; decoders silently discard the palette alpha — move tRNS after PLTE")
	}
	if len(c.data) == 0 || len(c.data)%3 != 0 || len(c.data) > 3*256 {
		f.problem("PLTE payload is %d bytes; need a multiple of 3 up to 768", len(c.data))
		f.plte = 0
		return
	}
	f.plte = len(c.data) / 3
}

func (f *pngFile) addACTL(c pngChunk) {
	if f.actl != nil {
		f.problem("duplicate acTL chunk")
		return
	}
	if f.idat > 0 {
		f.problem("acTL chunk after the first IDAT; decoders treat the file as a plain PNG")
	}
	if len(c.data) != pngACTLLen {
		f.problem("acTL payload is %d bytes, need %d", len(c.data), pngACTLLen)
		return
	}
	f.actl = &pngACTL{
		numFrames: binary.BigEndian.Uint32(c.data[0:4]),
		numPlays:  binary.BigEndian.Uint32(c.data[4:8]),
	}
	if f.actl.numFrames == 0 {
		f.problem("acTL declares 0 frames")
	}
}

func (f *pngFile) addFCTL(i int, c pngChunk) {
	if len(c.data) != pngFCTLLen {
		f.problem("fcTL at chunk %d is %d bytes, need %d", i, len(c.data), pngFCTLLen)
		return
	}
	f.closeFrame()
	fr := apngFrame{
		index:     len(f.frames),
		chunk:     i,
		seq:       binary.BigEndian.Uint32(c.data[0:4]),
		width:     binary.BigEndian.Uint32(c.data[4:8]),
		height:    binary.BigEndian.Uint32(c.data[8:12]),
		x:         binary.BigEndian.Uint32(c.data[12:16]),
		y:         binary.BigEndian.Uint32(c.data[16:20]),
		delayNum:  binary.BigEndian.Uint16(c.data[20:22]),
		delayDen:  binary.BigEndian.Uint16(c.data[22:24]),
		dispose:   c.data[24],
		blend:     c.data[25],
		isDefault: f.idat == 0,
	}
	if fr.isDefault && len(f.frames) > 0 {
		f.problem("more than one fcTL before the first IDAT")
	}
	// libpng(-apng) rejects files with out-of-range dispose_op/blend_op
	// outright, so they are container errors, not just oddities.
	if fr.dispose > 2 {
		f.problem("frame %d fcTL dispose_op %d is invalid (0 = none, 1 = background, 2 = previous); libpng rejects the file", fr.index, fr.dispose)
	}
	if fr.blend > 1 {
		f.problem("frame %d fcTL blend_op %d is invalid (0 = source, 1 = over); libpng rejects the file", fr.index, fr.blend)
	}
	f.checkSeq("fcTL", fr.seq)
	f.frames = append(f.frames, fr)
}

func (f *pngFile) addFDAT(i int, c pngChunk) {
	f.fdat++
	if len(c.data) < 4 {
		f.problem("fdAT at chunk %d is %d bytes, need at least 4", i, len(c.data))
		return
	}
	f.checkSeq("fdAT", binary.BigEndian.Uint32(c.data[0:4]))
	if len(f.frames) == 0 {
		f.problem("fdAT at chunk %d with no preceding fcTL", i)
		return
	}
	last := &f.frames[len(f.frames)-1]
	if last.isDefault {
		f.problem("fdAT at chunk %d follows the default image's fcTL; a new fcTL is required", i)
		return
	}
	last.dataChunks++
}

// checkSeq verifies the shared fcTL/fdAT sequence numbering (consecutive
// from 0 in stream order) and resynchronises after a mismatch so one gap is
// reported once.
func (f *pngFile) checkSeq(kind string, seq uint32) {
	if seq != f.nextSeq {
		f.problem("%s sequence number %d, expected %d", kind, seq, f.nextSeq)
	}
	f.nextSeq = seq + 1
}

// closeFrame checks that the frame being completed has pixel data before the
// next fcTL (or IEND) starts a new one.
func (f *pngFile) closeFrame() {
	if len(f.frames) == 0 {
		return
	}
	last := &f.frames[len(f.frames)-1]
	if last.dataChunks == 0 {
		if last.isDefault {
			f.problem("frame %d (fcTL before IDAT) has no IDAT data", last.index)
		} else {
			f.problem("frame %d has no fdAT data", last.index)
		}
	}
}

// finish runs the whole-file checks once every chunk has been seen.
func (f *pngFile) finish() {
	f.closeFrame()
	switch {
	case len(f.chunks) == 0:
		f.problem("no chunks after the PNG signature")
	case f.ihdr == nil:
		f.problem("no IHDR chunk")
	}
	if f.idat == 0 {
		f.problem("no IDAT chunk")
	}
	if !f.hasIEND {
		f.problem("no IEND chunk")
	}
	if f.ihdr != nil {
		f.checkPalette()
	}
	switch {
	case f.actl != nil:
		if n := uint32(len(f.frames)); f.actl.numFrames != n {
			f.problem("acTL declares %s but the file has %s", plural(int(f.actl.numFrames), "frame"), plural(int(n), "fcTL chunk"))
		}
	case len(f.frames) > 0 || f.fdat > 0:
		f.problem("fcTL/fdAT chunks without an acTL chunk; decoders show a plain PNG")
	}
}

// checkPalette validates PLTE/tRNS against the colour type.
func (f *pngFile) checkPalette() {
	h := f.ihdr
	switch h.colorType {
	case pngIndexed:
		if f.plte < 0 {
			f.problem("indexed colour (type 3) without a PLTE chunk")
		} else if f.hasTRNS && len(f.trns) > f.plte {
			f.problem("tRNS has %d entries but PLTE only %d", len(f.trns), f.plte)
		}
	case pngGray:
		if f.hasTRNS && len(f.trns) != 2 {
			f.problem("tRNS for greyscale must be 2 bytes, got %d", len(f.trns))
		}
	case pngRGB:
		if f.hasTRNS && len(f.trns) != 6 {
			f.problem("tRNS for truecolour must be 6 bytes, got %d", len(f.trns))
		}
	case pngGrayAlpha, pngRGBA:
		if f.hasTRNS {
			f.problem("tRNS chunk is not allowed with colour type %d (alpha channel present)", h.colorType)
		}
	}
}

// animated reports whether the file carries an animation control chunk.
func (f *pngFile) animated() bool { return f.actl != nil }

// hasAlpha is structural: an alpha channel (colour type 4/6) or a tRNS chunk
// (palette alpha, or a single transparent colour for grey/RGB).
func (f *pngFile) hasAlpha() bool {
	if f.hasTRNS {
		return true
	}
	return f.ihdr != nil && (f.ihdr.colorType == pngGrayAlpha || f.ihdr.colorType == pngRGBA)
}

// dims returns IHDR width/height (0, 0 without an IHDR).
func (f *pngFile) dims() (int, int) {
	if f.ihdr == nil {
		return 0, 0
	}
	return int(f.ihdr.width), int(f.ihdr.height)
}

// frameCount is the number of animation frames as declared by acTL, or 1
// for a plain PNG.
func (f *pngFile) frameCount() int {
	if f.actl == nil {
		return 1
	}
	return int(f.actl.numFrames)
}

// loopForever reports whether the animation loops forever (acTL num_plays
// 0); a plain PNG has nothing to loop and counts as forever.
func (f *pngFile) loopForever() bool {
	return f.actl == nil || f.actl.numPlays == 0
}

// timing returns the total animation duration and the shortest frame delay
// in microseconds (0, 0 for a plain PNG).
func (f *pngFile) timing() (totalUS, minUS int64) {
	minUS = -1
	for i := range f.frames {
		d := f.frames[i].delayUS()
		totalUS += d
		if minUS < 0 || d < minUS {
			minUS = d
		}
	}
	return totalUS, max(minUS, 0)
}

// colourDescription words the pixel format for details, e.g. "RGBA 8-bit
// (colour type 6)" or "indexed 8-bit (colour type 3), 256-entry palette,
// tRNS with 64 entries".
func (f *pngFile) colourDescription() string {
	h := f.ihdr
	if h == nil {
		return "unknown pixel format (no IHDR)"
	}
	var name string
	switch h.colorType {
	case pngGray:
		name = "greyscale"
	case pngRGB:
		name = "RGB"
	case pngIndexed:
		name = "indexed"
	case pngGrayAlpha:
		name = "greyscale+alpha"
	case pngRGBA:
		name = "RGBA"
	default:
		name = "unknown colour type"
	}
	s := fmt.Sprintf("%s %d-bit (colour type %d)", name, h.bitDepth, h.colorType)
	if h.colorType == pngIndexed {
		if f.plte >= 0 {
			s += fmt.Sprintf(", %d-entry palette", f.plte)
		} else {
			s += ", no PLTE"
		}
	}
	switch {
	case f.hasTRNS && h.colorType == pngIndexed && len(f.trns) == 1:
		s += ", tRNS with 1 entry (8-bit alpha)"
	case f.hasTRNS && h.colorType == pngIndexed:
		s += fmt.Sprintf(", tRNS with %d entries (8-bit alpha)", len(f.trns))
	case f.hasTRNS:
		s += ", tRNS (one transparent colour)"
	}
	if h.interlace == 1 {
		s += ", Adam7 interlaced"
	}
	return s
}

// roundUSToMS converts microseconds to whole milliseconds, rounding to
// nearest.
func roundUSToMS(us int64) int {
	return int((us + 500) / 1000)
}
