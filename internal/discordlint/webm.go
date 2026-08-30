package discordlint

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

// WebM (Matroska/EBML) probe for LintVideo. A minimal, iterative EBML
// walker: element IDs and sizes are variable-length integers; an element
// with the all-ones "unknown size" pattern (ffmpeg's streaming Segment)
// extends to the end of its region. Best-effort throughout — a malformed
// header stops the walk of that region, and whatever parsed so far stands.

// EBML element ids (as stored, marker bit included).
const (
	ebmlIDHeader         = 0x1A45DFA3 // EBML
	ebmlIDDocType        = 0x4282
	ebmlIDSegment        = 0x18538067
	ebmlIDInfo           = 0x1549A966
	ebmlIDTimestampScale = 0x2AD7B1 // ns per timestamp tick, default 1e6
	ebmlIDDuration       = 0x4489   // float, in ticks
	ebmlIDTracks         = 0x1654AE6B
	ebmlIDTrackEntry     = 0xAE
	ebmlIDTrackNumber    = 0xD7
	ebmlIDTrackType      = 0x83 // 1 = video
	ebmlIDCodecID        = 0x86
	ebmlIDVideo          = 0xE0
	ebmlIDPixelWidth     = 0xB0
	ebmlIDPixelHeight    = 0xBA
	ebmlIDCluster        = 0x1F43B675
	ebmlIDSimpleBlock    = 0xA3
	ebmlIDBlockGroup     = 0xA0
	ebmlIDBlock          = 0xA1
)

// probeWebM parses the EBML header and the Segment's Info, Tracks and
// Clusters (SimpleBlock/Block counting for the video track).
func probeWebM(data []byte) videoInfo {
	v := videoInfo{}
	top, consumed := ebmlChildren(data)
	var header, segment *ebmlEl
	for i := range top {
		switch top[i].id {
		case ebmlIDHeader:
			if header == nil {
				header = &top[i]
			}
		case ebmlIDSegment:
			if segment == nil {
				segment = &top[i]
			}
		}
	}

	// Container structure. The EBML DocType defaults to "matroska" when
	// the element is absent, which is exactly what .webm must not be.
	docType := ""
	switch {
	case header == nil:
		v.problems = append(v.problems, "no EBML header (not a WebM/Matroska file)")
	default:
		docType = "matroska"
		hdr, _ := ebmlChildren(header.data)
		for _, e := range hdr {
			if e.id == ebmlIDDocType {
				docType = string(e.data)
			}
		}
		if docType != "webm" {
			v.problems = append(v.problems, fmt.Sprintf("EBML DocType %q is not \"webm\"", docType))
		}
	}
	if segment == nil {
		v.problems = append(v.problems, "no Segment element")
	}
	if trailing := len(data) - consumed; trailing > 0 {
		v.problems = append(v.problems, fmt.Sprintf("%d bytes at the end did not parse as EBML elements (truncated file?)", trailing))
	}
	if segment == nil {
		v.codecNote = "no Segment element"
		return v
	}

	segEls, _ := ebmlChildren(segment.data)
	clusters := 0
	timestampScale := uint64(1_000_000) // ns per tick (Matroska default)
	durationTicks := 0.0
	var track webmTrack
	for _, e := range segEls {
		switch e.id {
		case ebmlIDInfo:
			info, _ := ebmlChildren(e.data)
			for _, c := range info {
				switch c.id {
				case ebmlIDTimestampScale:
					if s := ebmlUint(c.data); s > 0 {
						timestampScale = s
					}
				case ebmlIDDuration:
					if f, ok := ebmlFloat(c.data); ok {
						durationTicks = f
					}
				}
			}
		case ebmlIDTracks:
			if track.codecID == "" && track.number == 0 {
				track = webmVideoTrack(e.data)
			}
		case ebmlIDCluster:
			clusters++
		}
	}
	v.container = fmt.Sprintf("WebM container OK (EBML DocType webm; Segment with %s)", plural(clusters, "cluster"))
	if durationTicks > 0 && float64(timestampScale) > 0 {
		v.durationMS = clampInt31(int64(durationTicks * float64(timestampScale) / 1e6))
	}

	// Codec and dimensions from the video TrackEntry.
	switch {
	case track.number == 0:
		v.codecNote = "no video track in Segment/Tracks"
	case track.codecID == "":
		v.codecNote = "video track has no CodecID"
	default:
		v.codec = track.codecID
		v.codecOK = track.codecID == "V_VP9"
	}
	v.width, v.height = track.width, track.height

	// Frame count: video-track SimpleBlocks/Blocks across the clusters.
	// Lacing (rare for video) would undercount — best effort.
	if track.number > 0 {
		frames := 0
		for _, e := range segEls {
			if e.id != ebmlIDCluster {
				continue
			}
			children, _ := ebmlChildren(e.data)
			for _, c := range children {
				block := c.data
				if c.id == ebmlIDBlockGroup {
					block = nil
					group, _ := ebmlChildren(c.data)
					for _, g := range group {
						if g.id == ebmlIDBlock {
							block = g.data
							break
						}
					}
				} else if c.id != ebmlIDSimpleBlock {
					continue
				}
				if n, sz, _ := ebmlVint(block, false); sz > 0 && n == track.number {
					frames++
				}
			}
		}
		v.frames = frames
	}
	return v
}

// webmTrack is the parsed video TrackEntry.
type webmTrack struct {
	number        uint64
	codecID       string
	width, height int
}

// webmVideoTrack finds the first TrackEntry with TrackType 1 (video) — or,
// failing an explicit type, one carrying a Video element — inside a Tracks
// payload.
func webmVideoTrack(tracks []byte) webmTrack {
	entries, _ := ebmlChildren(tracks)
	for _, e := range entries {
		if e.id != ebmlIDTrackEntry {
			continue
		}
		var t webmTrack
		trackType := uint64(0)
		hasVideo := false
		children, _ := ebmlChildren(e.data)
		for _, c := range children {
			switch c.id {
			case ebmlIDTrackNumber:
				t.number = ebmlUint(c.data)
			case ebmlIDTrackType:
				trackType = ebmlUint(c.data)
			case ebmlIDCodecID:
				t.codecID = string(c.data)
			case ebmlIDVideo:
				hasVideo = true
				video, _ := ebmlChildren(c.data)
				for _, d := range video {
					switch d.id {
					case ebmlIDPixelWidth:
						t.width = clampInt31(int64(ebmlUint(d.data)))
					case ebmlIDPixelHeight:
						t.height = clampInt31(int64(ebmlUint(d.data)))
					}
				}
			}
		}
		if trackType == 1 || (trackType == 0 && hasVideo) {
			return t
		}
	}
	return webmTrack{}
}

// ebmlEl is one parsed EBML element.
type ebmlEl struct {
	id   uint64
	data []byte
}

// ebmlChildren splits region b into elements and reports how many bytes
// parsed. It stops at the first malformed or overrunning header; an
// unknown-size element takes the rest of the region.
func ebmlChildren(b []byte) ([]ebmlEl, int) {
	var out []ebmlEl
	pos := 0
	for pos < len(b) {
		id, n, _ := ebmlVint(b[pos:], true)
		if n == 0 {
			break
		}
		size, m, unknown := ebmlVint(b[pos+n:], false)
		if m == 0 {
			break
		}
		start := pos + n + m
		if unknown {
			out = append(out, ebmlEl{id: id, data: b[start:]})
			return out, len(b)
		}
		if size > uint64(len(b)-start) {
			break
		}
		out = append(out, ebmlEl{id: id, data: b[start : start+int(size)]})
		pos = start + int(size)
	}
	return out, pos
}

// ebmlVint reads a variable-length integer at the start of b. keepMarker
// retains the length-marker bit (element IDs are matched with it); size
// vints strip it. Returns the value, the encoded length (0 when malformed
// or truncated) and — for stripped reads — whether every value bit is set
// (the "unknown size" pattern).
func ebmlVint(b []byte, keepMarker bool) (val uint64, n int, allOnes bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if n > len(b) {
		return 0, 0, false
	}
	val = uint64(b[0])
	if !keepMarker {
		val &= 1<<(8-n) - 1
	}
	for i := 1; i < n; i++ {
		val = val<<8 | uint64(b[i])
	}
	if !keepMarker {
		allOnes = val == 1<<(7*n)-1
	}
	return val, n, allOnes
}

// ebmlUint decodes a big-endian EBML unsigned integer (0–8 bytes).
func ebmlUint(b []byte) uint64 {
	if len(b) > 8 {
		return 0
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// ebmlFloat decodes an EBML float (4 or 8 bytes, big-endian).
func ebmlFloat(b []byte) (float64, bool) {
	switch len(b) {
	case 4:
		f := math.Float32frombits(binary.BigEndian.Uint32(b))
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return 0, false
		}
		return float64(f), true
	case 8:
		f := math.Float64frombits(binary.BigEndian.Uint64(b))
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
