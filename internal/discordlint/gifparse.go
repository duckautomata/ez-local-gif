package discordlint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Block-level GIF model. The parser keeps every byte it does not need to
// interpret (LZW data, colour tables, unknown extensions) verbatim, so a
// parsed file re-serialises byte-exact when nothing was changed and a fixed
// file differs from the input only in the bytes the fixer touched.

const (
	gifTrailer         = 0x3B
	gifExtensionIntro  = 0x21
	gifImageIntro      = 0x2C
	gifLabelGCE        = 0xF9
	gifLabelApp        = 0xFF
	gifLabelComment    = 0xFE
	gifLabelPlainText  = 0x01
	gifNetscapeAppID   = "NETSCAPE2.0"
	gifNetscapeLoopSub = 0x01 // sub-block id of the looping sub-block
)

// gifFile is a parsed GIF stream.
type gifFile struct {
	header    [6]byte // "GIF87a" / "GIF89a"
	width     uint16  // Logical Screen Descriptor
	height    uint16
	lsdPacked byte // GCT flag 0x80, colour resolution 0x70, sort 0x08, GCT size 0x07
	bgIndex   byte
	aspect    byte
	gct       []byte // raw Global Color Table (nil when the flag is clear)
	blocks    []gifBlock

	hasTrailer bool   // a 0x3B trailer was present
	trailing   []byte // bytes after the trailer, preserved verbatim
}

func (g *gifFile) hasGCT() bool { return g.lsdPacked&0x80 != 0 }

// gctColors returns the number of entries in the Global Color Table (0 if
// there is none).
func (g *gifFile) gctColors() int {
	if !g.hasGCT() {
		return 0
	}
	return 2 << (g.lsdPacked & 0x07)
}

// gifBlock is one top-level GIF data stream block (extension or image).
type gifBlock interface {
	encode(w *bytes.Buffer)
}

// gifImage is an Image Descriptor with its optional Local Color Table and
// LZW-compressed data. It is never modified, only kept or re-emitted.
type gifImage struct {
	left, top     uint16
	width, height uint16
	packed        byte // LCT flag 0x80, interlace 0x40, sort 0x20, LCT size 0x07
	lct           []byte
	minCodeSize   byte
	data          [][]byte // LZW data sub-blocks (each 1..255 bytes)
	raw           []byte   // exact bytes from 0x2C through the 0x00 terminator
}

func (b *gifImage) hasLCT() bool     { return b.packed&0x80 != 0 }
func (b *gifImage) interlaced() bool { return b.packed&0x40 != 0 }

// lctColors returns the number of entries in the Local Color Table (0 if
// there is none).
func (b *gifImage) lctColors() int {
	if !b.hasLCT() {
		return 0
	}
	return 2 << (b.packed & 0x07)
}

func (b *gifImage) encode(w *bytes.Buffer) { w.Write(b.raw) }

// gifGCE is a Graphic Control Extension. raw holds the original bytes when
// the block was read from the input and not modified; a nil raw means the
// canonical 8-byte form is emitted from the fields.
type gifGCE struct {
	disposal     byte // 0..7
	userInput    bool
	transparent  bool
	delayCS      uint16
	transIndex   byte
	reservedBits byte // packed & 0xE0, preserved as read
	raw          []byte
}

func (b *gifGCE) encode(w *bytes.Buffer) {
	if b.raw != nil {
		w.Write(b.raw)
		return
	}
	packed := b.reservedBits&0xE0 | (b.disposal&0x07)<<2
	if b.userInput {
		packed |= 0x02
	}
	if b.transparent {
		packed |= 0x01
	}
	w.Write([]byte{gifExtensionIntro, gifLabelGCE, 0x04, packed, byte(b.delayCS), byte(b.delayCS >> 8), b.transIndex, 0x00})
}

// setDisposal, setDelay and setTransparent modify the block and drop the
// raw form so the canonical encoding is used.
func (b *gifGCE) setDisposal(d byte) { b.disposal = d & 0x07; b.raw = nil }
func (b *gifGCE) setDelay(cs uint16) { b.delayCS = cs; b.raw = nil }
func (b *gifGCE) setTransparent(on bool, index byte) {
	b.transparent = on
	b.transIndex = index
	b.raw = nil
}

// newGCE returns a canonical GCE (disposal 1, opaque).
func newGCE(delayCS uint16) *gifGCE {
	return &gifGCE{disposal: 1, delayCS: delayCS}
}

// gifAppExt is an Application Extension (0x21 0xFF). sub holds the data
// sub-blocks including the leading identifier block.
type gifAppExt struct {
	sub [][]byte
}

// id returns the application identifier + authentication code (normally 11
// bytes) or "" if the first sub-block is malformed.
func (b *gifAppExt) id() string {
	if len(b.sub) == 0 || len(b.sub[0]) != 11 {
		return ""
	}
	return string(b.sub[0])
}

func (b *gifAppExt) isNetscape() bool { return b.id() == gifNetscapeAppID }

// loopCount returns the NETSCAPE2.0 loop count and whether a well-formed
// looping sub-block (id 1, 3 bytes) was found.
func (b *gifAppExt) loopCount() (uint16, bool) {
	for _, s := range b.sub[min(1, len(b.sub)):] {
		if len(s) >= 3 && s[0] == gifNetscapeLoopSub {
			return binary.LittleEndian.Uint16(s[1:3]), true
		}
	}
	return 0, false
}

// setLoopCount rewrites the looping sub-block (adding one if missing).
// Sub-block slices alias the input, so the modified one is copied first.
func (b *gifAppExt) setLoopCount(n uint16) {
	for i, s := range b.sub {
		if i == 0 || len(s) < 3 || s[0] != gifNetscapeLoopSub {
			continue
		}
		c := append([]byte(nil), s...)
		binary.LittleEndian.PutUint16(c[1:3], n)
		b.sub[i] = c
		return
	}
	loop := []byte{gifNetscapeLoopSub, byte(n), byte(n >> 8)}
	if len(b.sub) == 0 {
		b.sub = [][]byte{[]byte(gifNetscapeAppID)}
	}
	b.sub = append(b.sub[:1], append([][]byte{loop}, b.sub[1:]...)...)
}

// newNetscapeLoop returns a canonical NETSCAPE2.0 looping block.
func newNetscapeLoop(count uint16) *gifAppExt {
	return &gifAppExt{sub: [][]byte{
		[]byte(gifNetscapeAppID),
		{gifNetscapeLoopSub, byte(count), byte(count >> 8)},
	}}
}

func (b *gifAppExt) encode(w *bytes.Buffer) {
	w.WriteByte(gifExtensionIntro)
	w.WriteByte(gifLabelApp)
	writeSubBlocks(w, b.sub)
}

// gifRawExt is any other extension (comment, plain text, unknown label),
// kept verbatim.
type gifRawExt struct {
	label byte
	raw   []byte // exact bytes from 0x21 through the 0x00 terminator
}

func (b *gifRawExt) encode(w *bytes.Buffer) { w.Write(b.raw) }

func writeSubBlocks(w *bytes.Buffer, subs [][]byte) {
	for _, s := range subs {
		// Sub-blocks longer than 255 bytes cannot come from the parser; the
		// only synthesised ones are 3 and 11 bytes.
		w.WriteByte(byte(len(s)))
		w.Write(s)
	}
	w.WriteByte(0x00)
}

// encode serialises the file. A trailer is always written.
func (g *gifFile) encode() []byte {
	var w bytes.Buffer
	w.Write(g.header[:])
	var lsd [7]byte
	binary.LittleEndian.PutUint16(lsd[0:2], g.width)
	binary.LittleEndian.PutUint16(lsd[2:4], g.height)
	lsd[4] = g.lsdPacked
	lsd[5] = g.bgIndex
	lsd[6] = g.aspect
	w.Write(lsd[:])
	w.Write(g.gct)
	for _, b := range g.blocks {
		b.encode(&w)
	}
	w.WriteByte(gifTrailer)
	w.Write(g.trailing)
	return w.Bytes()
}

// insertBlock inserts b at position at (0 <= at <= len(blocks)).
func (g *gifFile) insertBlock(at int, b gifBlock) {
	g.blocks = append(g.blocks, nil)
	copy(g.blocks[at+1:], g.blocks[at:])
	g.blocks[at] = b
}

// removeBlocks deletes the blocks at the given indices.
func (g *gifFile) removeBlocks(indices []int) {
	if len(indices) == 0 {
		return
	}
	drop := make(map[int]bool, len(indices))
	for _, i := range indices {
		drop[i] = true
	}
	kept := g.blocks[:0]
	for i, b := range g.blocks {
		if !drop[i] {
			kept = append(kept, b)
		}
	}
	for i := len(kept); i < len(g.blocks); i++ {
		g.blocks[i] = nil
	}
	g.blocks = kept
}

// gifFrame associates an image with its effective Graphic Control
// Extension: the last GCE seen since the previous image (browsers use the
// last one; giflib the first — duplicates are reported).
type gifFrame struct {
	index      int
	image      *gifImage
	imageBlock int     // index into gifFile.blocks
	gce        *gifGCE // nil when the frame has no GCE
	gceBlock   int     // index into gifFile.blocks, -1 when nil
	dupGCE     []int   // further GCE blocks in the same run (before gceBlock)
}

// frames pairs images with their GCEs. dangling lists GCE blocks after the
// last image (they apply to nothing).
func (g *gifFile) frames() (frames []gifFrame, dangling []int) {
	var run []int
	for i, b := range g.blocks {
		switch b := b.(type) {
		case *gifGCE:
			run = append(run, i)
		case *gifImage:
			f := gifFrame{index: len(frames), image: b, imageBlock: i, gceBlock: -1}
			if len(run) > 0 {
				f.gceBlock = run[len(run)-1]
				f.gce = g.blocks[f.gceBlock].(*gifGCE)
				f.dupGCE = run[:len(run)-1]
			}
			frames = append(frames, f)
			run = nil
		}
	}
	return frames, run
}

// firstImageBlock returns the block index of the first image (or
// len(blocks) when there is none).
func (g *gifFile) firstImageBlock() int {
	for i, b := range g.blocks {
		if _, ok := b.(*gifImage); ok {
			return i
		}
	}
	return len(g.blocks)
}

// parseGIF parses data into a gifFile. It is tolerant of a missing trailer,
// bytes after the trailer, unusual GCE sizes and unknown extension labels,
// but returns an error for truncated blocks or unknown block introducers
// (nothing sensible can be rebuilt from those).
func parseGIF(data []byte) (*gifFile, error) {
	if len(data) < 13 {
		return nil, errors.New("gif: file shorter than header + logical screen descriptor")
	}
	g := &gifFile{}
	copy(g.header[:], data[:6])
	if h := string(g.header[:]); h != "GIF87a" && h != "GIF89a" {
		return nil, fmt.Errorf("gif: bad signature %q", h)
	}
	g.width = binary.LittleEndian.Uint16(data[6:8])
	g.height = binary.LittleEndian.Uint16(data[8:10])
	g.lsdPacked = data[10]
	g.bgIndex = data[11]
	g.aspect = data[12]
	r := &gifReader{data: data, pos: 13}
	if g.hasGCT() {
		n := 3 * g.gctColors()
		gct, err := r.take(n)
		if err != nil {
			return nil, fmt.Errorf("gif: truncated global colour table: %w", err)
		}
		g.gct = gct
	}
	for {
		if r.pos >= len(data) {
			return g, nil // missing trailer: tolerated, reported by the linter
		}
		intro := data[r.pos]
		switch intro {
		case gifTrailer:
			g.hasTrailer = true
			g.trailing = data[r.pos+1:]
			return g, nil
		case gifExtensionIntro:
			b, err := r.extension()
			if err != nil {
				return nil, fmt.Errorf("gif: extension at offset %d: %w", r.pos, err)
			}
			g.blocks = append(g.blocks, b)
		case gifImageIntro:
			b, err := r.image()
			if err != nil {
				return nil, fmt.Errorf("gif: image %d at offset %d: %w", countImages(g.blocks), r.pos, err)
			}
			g.blocks = append(g.blocks, b)
		default:
			return nil, fmt.Errorf("gif: unknown block introducer 0x%02X at offset %d", intro, r.pos)
		}
	}
}

func countImages(blocks []gifBlock) int {
	n := 0
	for _, b := range blocks {
		if _, ok := b.(*gifImage); ok {
			n++
		}
	}
	return n
}

// gifReader is a cursor over the input bytes. Returned slices alias data.
type gifReader struct {
	data []byte
	pos  int
}

func (r *gifReader) take(n int) ([]byte, error) {
	if r.pos+n > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	s := r.data[r.pos : r.pos+n]
	r.pos += n
	return s, nil
}

// subBlocks reads data sub-blocks up to and including the 0x00 terminator.
func (r *gifReader) subBlocks() ([][]byte, error) {
	var out [][]byte
	for {
		if r.pos >= len(r.data) {
			return nil, fmt.Errorf("sub-blocks: %w", io.ErrUnexpectedEOF)
		}
		n := int(r.data[r.pos])
		r.pos++
		if n == 0 {
			return out, nil
		}
		s, err := r.take(n)
		if err != nil {
			return nil, fmt.Errorf("sub-block of %d bytes: %w", n, err)
		}
		out = append(out, s)
	}
}

// extension parses one 0x21-introduced block starting at r.pos.
func (r *gifReader) extension() (gifBlock, error) {
	start := r.pos
	if r.pos+2 > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	label := r.data[r.pos+1]
	r.pos += 2
	subs, err := r.subBlocks()
	if err != nil {
		return nil, fmt.Errorf("label 0x%02X: %w", label, err)
	}
	raw := r.data[start:r.pos]
	switch label {
	case gifLabelGCE:
		return parseGCE(subs, raw), nil
	case gifLabelApp:
		return &gifAppExt{sub: subs}, nil
	default:
		return &gifRawExt{label: label, raw: raw}, nil
	}
}

// parseGCE interprets the first sub-block of a Graphic Control Extension.
// A malformed (short) block yields zero fields; raw is kept so it can still
// round-trip until the fixer touches it.
func parseGCE(subs [][]byte, raw []byte) *gifGCE {
	b := &gifGCE{raw: raw}
	if len(subs) == 0 || len(subs[0]) < 4 {
		return b
	}
	p := subs[0]
	b.reservedBits = p[0] & 0xE0
	b.disposal = (p[0] >> 2) & 0x07
	b.userInput = p[0]&0x02 != 0
	b.transparent = p[0]&0x01 != 0
	b.delayCS = binary.LittleEndian.Uint16(p[1:3])
	b.transIndex = p[3]
	return b
}

// image parses one image descriptor + LCT + LZW data starting at r.pos.
func (r *gifReader) image() (*gifImage, error) {
	start := r.pos
	d, err := r.take(10)
	if err != nil {
		return nil, fmt.Errorf("image descriptor: %w", err)
	}
	b := &gifImage{
		left:   binary.LittleEndian.Uint16(d[1:3]),
		top:    binary.LittleEndian.Uint16(d[3:5]),
		width:  binary.LittleEndian.Uint16(d[5:7]),
		height: binary.LittleEndian.Uint16(d[7:9]),
		packed: d[9],
	}
	if b.hasLCT() {
		lct, err := r.take(3 * b.lctColors())
		if err != nil {
			return nil, fmt.Errorf("local colour table: %w", err)
		}
		b.lct = lct
	}
	mcs, err := r.take(1)
	if err != nil {
		return nil, fmt.Errorf("LZW minimum code size: %w", err)
	}
	b.minCodeSize = mcs[0]
	if b.data, err = r.subBlocks(); err != nil {
		return nil, fmt.Errorf("image data: %w", err)
	}
	b.raw = r.data[start:r.pos]
	return b, nil
}
