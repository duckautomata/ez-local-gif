package discordlint

import (
	"encoding/binary"
	"math"
	"testing"
)

// Synthetic MP4 / WebM fixtures: hand-built byte slices covering the pure
// parsing paths so the video suite runs without ffmpeg (real encoder
// output lives in testdata/ff_2frame.mp4 / ff_2frame.webm).

// ---------------------------------------------------------------------------
// ISO-BMFF builders

func vbe16(v int) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, uint16(v)); return b }
func vbe32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// cat concatenates byte slices (payload assembly for mkBox / mkFullBox,
// which live in static_test.go).
func cat(parts ...[]byte) []byte {
	var p []byte
	for _, x := range parts {
		p = append(p, x...)
	}
	return p
}

// mp4Opts parameterises buildMP4; the zero value (with defaults applied)
// is a well-formed 2-frame 64x64 H.264 faststart MP4.
type mp4Opts struct {
	major     string   // ftyp major brand; default "isom"
	compat    []string // compatible brands; default isom,iso2,avc1,mp41
	noFtyp    bool
	noMoov    bool
	noMdat    bool
	moovLast  bool   // moov after mdat (no faststart)
	codec     string // stsd entry 4CC; default "avc1"
	noStsd    bool   // omit the stsd box (dims fall back to tkhd)
	w, h      int    // stsd dims; default 64x64
	tkhdW     int    // tkhd dims; default = w,h
	tkhdH     int
	tkhdV1    bool // build a version-1 tkhd (64-bit times/duration)
	noTkhd    bool
	timescale uint32   // default 1000
	duration  uint32   // default 200 (0.2 s)
	stts      []uint32 // count,delta pairs; default 2 samples of 100
	mdatLen   int      // default 8
	trailing  []byte   // raw bytes appended after the last box
}

func buildMP4(o mp4Opts) []byte {
	if o.major == "" {
		o.major = "isom"
	}
	if o.compat == nil {
		o.compat = []string{"isom", "iso2", "avc1", "mp41"}
	}
	if o.codec == "" {
		o.codec = "avc1"
	}
	if o.w == 0 && o.h == 0 {
		o.w, o.h = 64, 64
	}
	if o.tkhdW == 0 && o.tkhdH == 0 {
		o.tkhdW, o.tkhdH = o.w, o.h
	}
	if o.timescale == 0 {
		o.timescale = 1000
	}
	if o.duration == 0 {
		o.duration = 200
	}
	if o.stts == nil {
		o.stts = []uint32{2, 100}
	}
	if o.mdatLen == 0 {
		o.mdatLen = 8
	}

	ftypPayload := append([]byte(o.major), vbe32(0x200)...)
	for _, c := range o.compat {
		ftypPayload = append(ftypPayload, c...)
	}
	ftyp := mkBox("ftyp", ftypPayload)

	mvhd := mkFullBox("mvhd", cat(vbe32(0), vbe32(0), vbe32(o.timescale), vbe32(o.duration)))
	tkhd := mkFullBox("tkhd", cat(make([]byte, 72), vbe32(uint32(o.tkhdW)<<16), vbe32(uint32(o.tkhdH)<<16)))
	if o.tkhdV1 {
		// Version 1: verflags(4) + 64-bit times/duration + track_ID +
		// reserved + layer/group/volume/reserved (48 bytes of zeros here),
		// then the 36-byte identity matrix, then 16.16 width/height at
		// payload offset 88. A real identity matrix (not zeros) makes a
		// regression to offset 84 fail loudly (0x40000000>>16 = 16384).
		matrix := cat(
			vbe32(0x00010000), vbe32(0), vbe32(0),
			vbe32(0), vbe32(0x00010000), vbe32(0),
			vbe32(0), vbe32(0), vbe32(0x40000000),
		)
		tkhd = mkBox("tkhd", cat([]byte{1, 0, 0, 0}, make([]byte, 48), matrix,
			vbe32(uint32(o.tkhdW)<<16), vbe32(uint32(o.tkhdH)<<16)))
	}
	hdlr := mkFullBox("hdlr", cat(vbe32(0), []byte("vide"), make([]byte, 13)))

	entry := make([]byte, 28) // reserved(6)+dri(2)+predefined/reserved(16)+w(2)+h(2)
	copy(entry[24:], vbe16(o.w))
	copy(entry[26:], vbe16(o.h))
	stsd := mkFullBox("stsd", cat(vbe32(1), mkBox(o.codec, entry)))

	sttsPayload := []byte{}
	for _, v := range o.stts {
		sttsPayload = append(sttsPayload, vbe32(v)...)
	}
	stts := mkFullBox("stts", cat(vbe32(uint32(len(o.stts)/2)), sttsPayload))

	stblPayload := stts
	if !o.noStsd {
		stblPayload = cat(stsd, stts)
	}
	stbl := mkBox("stbl", stblPayload)
	minf := mkBox("minf", stbl)
	mdia := mkBox("mdia", cat(hdlr, minf))
	trakPayload := mdia
	if !o.noTkhd {
		trakPayload = cat(tkhd, mdia)
	}
	trak := mkBox("trak", trakPayload)
	moov := mkBox("moov", cat(mvhd, trak))
	mdat := mkBox("mdat", make([]byte, o.mdatLen))

	var out []byte
	if !o.noFtyp {
		out = append(out, ftyp...)
	}
	switch {
	case o.noMoov:
		if !o.noMdat {
			out = append(out, mdat...)
		}
	case o.noMdat:
		out = append(out, moov...)
	case o.moovLast:
		out = append(out, mdat...)
		out = append(out, moov...)
	default:
		out = append(out, moov...)
		out = append(out, mdat...)
	}
	return append(out, o.trailing...)
}

// ---------------------------------------------------------------------------
// EBML builders

// ebmlIDBytes encodes an element id as stored (marker bit included): the
// big-endian bytes with leading zero bytes stripped.
func ebmlIDBytes(id uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, id)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return b[i:]
}

// mkEBML builds id + 8-byte size vint + payload (a non-minimal size
// encoding is valid EBML and keeps the builder trivial).
func mkEBML(id uint64, parts ...[]byte) []byte {
	var p []byte
	for _, x := range parts {
		p = append(p, x...)
	}
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(p))|1<<56) // 0x01 marker
	out := append(ebmlIDBytes(id), size...)
	return append(out, p...)
}

// mkEBMLUnknown builds id + the 8-byte unknown-size vint + payload.
func mkEBMLUnknown(id uint64, parts ...[]byte) []byte {
	var p []byte
	for _, x := range parts {
		p = append(p, x...)
	}
	out := append(ebmlIDBytes(id), 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	return append(out, p...)
}

// mkEBMLUint builds a minimally-encoded unsigned integer element.
func mkEBMLUint(id, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return mkEBML(id, b[i:])
}

// webmOpts parameterises buildWebM; the zero value (with defaults) is a
// well-formed 2-frame 64x64 V_VP9 WebM.
type webmOpts struct {
	docType        string // default "webm"
	noHeader       bool
	noSegment      bool
	unknownSegSize bool   // Segment with the unknown-size pattern (streamed output)
	codec          string // default "V_VP9"
	trackNum       uint64 // default 1
	trackType      uint64 // default 1 (video)
	w, h           int    // default 64x64; -1,-1 omits the Video element
	noDuration     bool
	duration       float64 // in ticks; default 200
	blocks         int     // SimpleBlocks in one cluster; default 2
	audioTrack     bool    // prepend an Opus audio TrackEntry
	trailing       []byte
}

func buildWebM(o webmOpts) []byte {
	if o.docType == "" {
		o.docType = "webm"
	}
	if o.codec == "" {
		o.codec = "V_VP9"
	}
	if o.trackNum == 0 {
		o.trackNum = 1
	}
	if o.trackType == 0 {
		o.trackType = 1
	}
	if o.w == 0 && o.h == 0 {
		o.w, o.h = 64, 64
	}
	if o.duration == 0 {
		o.duration = 200
	}
	if o.blocks == 0 {
		o.blocks = 2
	}

	header := mkEBML(ebmlIDHeader,
		mkEBMLUint(0x4286, 1), // EBMLVersion
		mkEBML(ebmlIDDocType, []byte(o.docType)),
	)

	infoParts := [][]byte{mkEBMLUint(ebmlIDTimestampScale, 1_000_000)}
	if !o.noDuration {
		f := make([]byte, 8)
		binary.BigEndian.PutUint64(f, math.Float64bits(o.duration))
		infoParts = append(infoParts, mkEBML(ebmlIDDuration, f))
	}
	info := mkEBML(ebmlIDInfo, infoParts...)

	videoParts := [][]byte{}
	if o.w >= 0 && o.h >= 0 {
		videoParts = append(videoParts, mkEBML(ebmlIDVideo,
			mkEBMLUint(ebmlIDPixelWidth, uint64(o.w)),
			mkEBMLUint(ebmlIDPixelHeight, uint64(o.h))))
	}
	entry := mkEBML(ebmlIDTrackEntry, append([][]byte{
		mkEBMLUint(ebmlIDTrackNumber, o.trackNum),
		mkEBMLUint(ebmlIDTrackType, o.trackType),
		mkEBML(ebmlIDCodecID, []byte(o.codec)),
	}, videoParts...)...)
	trackParts := [][]byte{}
	if o.audioTrack {
		trackParts = append(trackParts, mkEBML(ebmlIDTrackEntry,
			mkEBMLUint(ebmlIDTrackNumber, o.trackNum+1),
			mkEBMLUint(ebmlIDTrackType, 2),
			mkEBML(ebmlIDCodecID, []byte("A_OPUS"))))
	}
	tracks := mkEBML(ebmlIDTracks, append(trackParts, entry)...)

	clusterParts := [][]byte{mkEBMLUint(0xE7, 0)} // Timestamp
	for i := 0; i < o.blocks; i++ {
		block := append([]byte{byte(0x80 | o.trackNum)}, 0, byte(i), 0x80, 0xDE, 0xAD)
		clusterParts = append(clusterParts, mkEBML(ebmlIDSimpleBlock, block))
	}
	cluster := mkEBML(ebmlIDCluster, clusterParts...)

	segment := mkEBML(ebmlIDSegment, info, tracks, cluster)
	if o.unknownSegSize {
		segment = mkEBMLUnknown(ebmlIDSegment, info, tracks, cluster)
	}

	var out []byte
	if !o.noHeader {
		out = append(out, header...)
	}
	if !o.noSegment {
		out = append(out, segment...)
	}
	return append(out, o.trailing...)
}

// lintVideo runs LintVideo and fails the test on error.
func lintVideo(t testing.TB, format string, data []byte, target Target) Report {
	t.Helper()
	r, err := LintVideo(format, data, target)
	if err != nil {
		t.Fatalf("LintVideo(%s): %v", format, err)
	}
	return r
}
