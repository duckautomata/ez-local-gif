package discordlint

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// MP4 (ISO-BMFF) probe for LintVideo. Reuses the generic box walker in
// static.go (isoBoxes/isoBoxesConsumed/findBox/fullBoxPayload, shared with
// the AVIF probe). Everything is best-effort: a box that does not parse
// simply leaves its videoInfo field at zero.

// mp4Brands are the ftyp brands (major or compatible) accepted as "an MP4
// Discord will treat as video/mp4". ffmpeg's default movflags write
// isom + isomiso2avc1mp41.
var mp4Brands = map[string]bool{
	"isom": true, "iso2": true, "iso3": true, "iso4": true, "iso5": true,
	"iso6": true, "mp41": true, "mp42": true, "avc1": true, "dash": true,
}

// probeMP4 walks the top-level boxes and the first video trak of moov.
func probeMP4(data []byte) videoInfo {
	v := videoInfo{}
	boxes, consumed := isoBoxesConsumed(data)
	var ftyp, moov *isoBox
	moovIdx, mdatIdx := -1, -1
	for i := range boxes {
		switch boxes[i].typ {
		case "ftyp":
			if ftyp == nil {
				ftyp = &boxes[i]
			}
		case "moov":
			if moovIdx < 0 {
				moovIdx, moov = i, &boxes[i]
			}
		case "mdat":
			if mdatIdx < 0 {
				mdatIdx = i
			}
		}
	}

	// Container structure.
	brand := ""
	switch {
	case ftyp == nil:
		v.problems = append(v.problems, "no ftyp box (not an MP4 file)")
	default:
		brands := ftypBrands(ftyp.payload)
		brand = strings.Join(brands, ",")
		ok := false
		for _, b := range brands {
			ok = ok || mp4Brands[b]
		}
		if !ok {
			v.problems = append(v.problems, fmt.Sprintf("ftyp brands %s include none of the MP4 brands (isom/iso2…iso6/mp41/mp42/avc1/dash)", brand))
		}
	}
	if moovIdx < 0 {
		v.problems = append(v.problems, "no moov box (movie metadata missing)")
	}
	if mdatIdx < 0 {
		v.problems = append(v.problems, "no mdat box (no media data)")
	}
	if trailing := len(data) - consumed; trailing > 0 {
		v.problems = append(v.problems, fmt.Sprintf("%d bytes at the end did not parse as a top-level box (truncated file?)", trailing))
	}
	v.container = fmt.Sprintf("MP4 container OK (ftyp %s; %d top-level boxes, moov and mdat)", brand, len(boxes))
	if moovIdx >= 0 && mdatIdx >= 0 {
		if moovIdx < mdatIdx {
			v.faststart = 1
		} else {
			v.faststart = -1
		}
	}

	// Movie metadata.
	if moov == nil {
		v.codecNote = "no moov box"
		return v
	}
	moovBoxes := isoBoxes(moov.payload)
	if ts, dur, ok := mp4MVHD(findBox(moovBoxes, "mvhd")); ok && ts > 0 {
		v.durationMS = clampInt31(int64(dur) * 1000 / int64(ts))
	}
	trak := mp4VideoTrak(moovBoxes)
	if trak == nil {
		v.codecNote = "no video track in moov"
		return v
	}
	trak.fill(&v)
	return v
}

// ftypBrands returns major brand + compatible brands.
func ftypBrands(p []byte) []string {
	if len(p) < 4 {
		return nil
	}
	brands := []string{string(p[0:4])}
	for pos := 8; pos+4 <= len(p); pos += 4 {
		brands = append(brands, string(p[pos:pos+4]))
	}
	return brands
}

// mp4MVHD reads timescale and duration from an mvhd box (nil-safe).
func mp4MVHD(b *isoBox) (timescale uint32, duration uint64, ok bool) {
	if b == nil || len(b.payload) < 1 {
		return 0, 0, false
	}
	p := b.payload
	if p[0] == 1 { // version 1: 64-bit times
		if len(p) < 32 {
			return 0, 0, false
		}
		return binary.BigEndian.Uint32(p[20:24]), binary.BigEndian.Uint64(p[24:32]), true
	}
	if len(p) < 20 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint32(p[12:16]), uint64(binary.BigEndian.Uint32(p[16:20])), true
}

// mp4Trak is the parsed skeleton of one video trak.
type mp4Trak struct {
	entryType     string // stsd sample entry 4CC ("avc1", "vp09", …)
	width, height int    // stsd coded size, falling back to tkhd presentation size
	frames        int    // stts sample count sum
}

// mp4VideoTrak finds the first trak whose hdlr handler is "vide" and
// parses its stsd/stts. When no trak carries a hdlr (or none is "vide"),
// nil is returned.
func mp4VideoTrak(moovBoxes []isoBox) *mp4Trak {
	for _, b := range moovBoxes {
		if b.typ != "trak" {
			continue
		}
		trakBoxes := isoBoxes(b.payload)
		mdia := findBox(trakBoxes, "mdia")
		if mdia == nil {
			continue
		}
		mdiaBoxes := isoBoxes(mdia.payload)
		hdlr := fullBoxPayload(findBox(mdiaBoxes, "hdlr"))
		if len(hdlr) < 8 || string(hdlr[4:8]) != "vide" {
			continue
		}
		t := &mp4Trak{}
		if minf := findBox(mdiaBoxes, "minf"); minf != nil {
			if stbl := findBox(isoBoxes(minf.payload), "stbl"); stbl != nil {
				stblBoxes := isoBoxes(stbl.payload)
				t.parseSTSD(findBox(stblBoxes, "stsd"))
				t.frames = mp4STTSFrames(findBox(stblBoxes, "stts"))
			}
		}
		if t.width == 0 || t.height == 0 {
			t.width, t.height = mp4TKHDDims(findBox(trakBoxes, "tkhd"))
		}
		return t
	}
	return nil
}

// parseSTSD reads the first sample entry: its 4CC and the coded
// width/height of a VisualSampleEntry (offsets 24/26 of the entry body).
func (t *mp4Trak) parseSTSD(stsd *isoBox) {
	body := fullBoxPayload(stsd)
	if len(body) < 4 {
		return
	}
	entries := isoBoxes(body[4:]) // after the 4-byte entry_count
	if len(entries) == 0 {
		return
	}
	t.entryType = entries[0].typ
	if p := entries[0].payload; len(p) >= 28 {
		t.width = int(binary.BigEndian.Uint16(p[24:26]))
		t.height = int(binary.BigEndian.Uint16(p[26:28]))
	}
}

// mp4STTSFrames sums the sample counts of an stts box (0 when absent or
// malformed; capped so hostile counts stay sane).
func mp4STTSFrames(stts *isoBox) int {
	body := fullBoxPayload(stts)
	if len(body) < 4 {
		return 0
	}
	n := int(binary.BigEndian.Uint32(body[0:4]))
	var frames int64
	for i := 0; i < n && 4+i*8+8 <= len(body); i++ {
		frames += int64(binary.BigEndian.Uint32(body[4+i*8:]))
		if frames > 1<<31-1 {
			return 1<<31 - 1
		}
	}
	return int(frames)
}

// mp4TKHDDims reads the 16.16 fixed-point presentation size at the end of
// a tkhd box (nil-safe; 0,0 when absent or malformed).
func mp4TKHDDims(b *isoBox) (w, h int) {
	if b == nil || len(b.payload) < 1 {
		return 0, 0
	}
	p := b.payload
	off := 76 // version 0: v/f(4) + 32 + reserved/layer/… (36) + matrix (36)
	if p[0] == 1 {
		off = 88 // 64-bit creation/modification/duration: +12 over version 0
	}
	if len(p) < off+8 {
		return 0, 0
	}
	return int(binary.BigEndian.Uint32(p[off:]) >> 16), int(binary.BigEndian.Uint32(p[off+4:]) >> 16)
}

// mp4Codec fills codec/codecOK/codecNote from the sample entry 4CC.
func (t *mp4Trak) fill(v *videoInfo) {
	v.width, v.height = t.width, t.height
	v.frames = t.frames
	switch t.entryType {
	case "":
		v.codecNote = "no stsd sample entry"
		return
	case "avc1", "avc3":
		v.codec, v.codecOK = "H.264 ("+t.entryType+" sample entry)", true
	case "hev1", "hvc1":
		v.codec = "HEVC (" + t.entryType + ")"
	case "vp09":
		v.codec = "VP9 (vp09)"
	case "av01":
		v.codec = "AV1 (av01)"
	case "mp4v":
		v.codec = "MPEG-4 Visual (mp4v)"
	default:
		v.codec = fmt.Sprintf("%q", t.entryType)
	}
}

// clampInt31 keeps a parsed count (milliseconds, pixels) non-negative and
// within int range on every platform.
func clampInt31(n int64) int {
	if n < 0 {
		return 0
	}
	if n > 1<<31-1 {
		return 1<<31 - 1
	}
	return int(n)
}
