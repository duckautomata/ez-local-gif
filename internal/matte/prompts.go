package matte

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// PromptsHeader is the request header that carries a TrackPrompts JSON on
// POST /v1/track and POST /v1/track/frame (the body is the raw frames, so
// the prompts ride in a header):
//
//	{"obj":1,"prompts":[{"frame":0,"points":[[0.3,0.5,1]],"box":[0.1,0.2,0.6,0.9]},
//	                    {"frame":12,"points":[],"box":null}]}
//
// Coordinates are normalised to 0..1 of the frame the sidecar receives
// (the app sends the clip at the tracking size); the sidecar converts them
// to pixels.
//
// A mask prompt (Phase 5d, FramePrompt.Mask) adds one top-level "mask"
// key naming its frame, which stays listed in "prompts" (with its points
// and box, or empty points and a null box when the mask is all it has):
//
//	{"obj":1,"prompts":[{"frame":3,"points":[],"box":null}],"mask":{"frame":3}}
//
// and the mask itself — ONE record [uint32 BE length][8-bit gray PNG at
// the frames' w×h, >= 128 = subject] — follows the frames in the body
// (Client.Track / Client.TrackFrame append it). The sidecar applies the
// mask to that frame first and the frame's points / box after it.
const PromptsHeader = "X-Matte-Prompts"

// MaskDigestLen is the length of FramePrompt.MaskDigest: the first 12 hex
// characters (48 bits) of the sha256 of the mask PNG's bytes
// (MaskDigestOf) — plenty to tell one mask from another inside one key.
const MaskDigestLen = 12

// MaskDigestOf is the FramePrompt.MaskDigest of a mask PNG: the first
// MaskDigestLen hex characters of sha256(png). Callers fill the digest of
// the mask they are about to send before keying (TrackPrompts.Canonical)
// and before Track / TrackFrame, which refuse a mask whose digest is not
// the prompt's (what a key names must be what the sidecar received).
func MaskDigestOf(png []byte) string {
	sum := sha256.Sum256(png)
	return hex.EncodeToString(sum[:])[:MaskDigestLen]
}

// promptDecimals is the fixed precision of a prompt coordinate in Canonical
// and on the wire: 1/10000 of the frame, under half a pixel up to 4096 px.
const promptDecimals = 4

// TrackPrompts are the user's clicks and boxes that tell the tracker what
// to follow (Phase 5c Part B): one object, prompted on one or more frames.
// The same value makes the wire header (headerValue) and the memo key text
// (Canonical), both after the same rounding, so what a key names is what
// the sidecar received.
type TrackPrompts struct {
	Obj     int           `json:"obj"`     // the object id; 0 is sent as 1 (the app tracks one object)
	Prompts []FramePrompt `json:"prompts"` // at least one must carry a box, a positive point or a mask
}

// FramePrompt is one frame's prompts. Frame is the frame's index on the
// clip the sidecar receives (the plan's output grid); Points are [x, y,
// label] with x, y in 0..1 of the frame and label 1 = keep (positive) / 0 =
// remove (negative); Box is [x0, y0, x1, y1] in 0..1 with x0 < x1, y0 < y1,
// nil when the frame has none.
//
// Mask (Phase 5d) says the frame is prompted with a mask: the one mask
// record Track / TrackFrame append to the body (the frames' w×h, 8-bit
// gray PNG, >= 128 = subject), applied to the frame before its points and
// box, which refine it. A mask prompt needs neither; at most one prompt of
// a set carries a mask (one record per request, "mask":{"frame":N} on the
// wire — PromptsHeader). MaskDigest is MaskDigestOf the PNG the caller
// sends: it is the mask's identity in Canonical (the memo key) and is
// never on the wire or in a request body — the app fills it from the bytes
// it is about to send (never a client's claim), and Track / TrackFrame
// refuse a mask whose digest differs from it.
type FramePrompt struct {
	Frame      int          `json:"frame"`
	Points     [][3]float64 `json:"points,omitempty"`
	Box        *[4]float64  `json:"box,omitempty"`
	Mask       bool         `json:"mask,omitempty"` // this frame carries the request's mask record
	MaskDigest string       `json:"-"`              // MaskDigestOf the mask PNG sent (key identity; filled by the app)
}

// MaskFrame reports the frame of the prompt that carries the mask record
// and whether there is one (the first in list order; Validate refuses a
// second).
func (p TrackPrompts) MaskFrame() (int, bool) {
	for _, fp := range p.Prompts {
		if fp.Mask {
			return fp.Frame, true
		}
	}
	return 0, false
}

// Validate checks what the sidecar would answer 400 to, before a request is
// made: at least one prompt; every prompt on a frame >= 0 carrying a box
// (0..1, x0 < x1, y0 < y1), points (x, y in 0..1, label 0 or 1) or a mask
// (at most one prompt of the set — one mask record per request); and at
// least one box, positive point or mask across them all (a tracker needs
// something to follow — one negative click selects nothing). The mask's
// digest is not checked here (it is a key concern: Track / TrackFrame
// check it against the bytes they send).
func (p TrackPrompts) Validate() error {
	if p.Obj < 0 {
		return fmt.Errorf("matte: prompts: obj %d", p.Obj)
	}
	if len(p.Prompts) == 0 {
		return errors.New("matte: prompts: none")
	}
	anchored := false
	masked := -1
	for i, fp := range p.Prompts {
		if fp.Frame < 0 {
			return fmt.Errorf("matte: prompt %d: frame %d", i, fp.Frame)
		}
		if fp.Mask {
			if masked >= 0 {
				return fmt.Errorf("matte: prompt %d (frame %d): a second mask (prompt %d, frame %d, has one): one mask record per request", i, fp.Frame, masked, p.Prompts[masked].Frame)
			}
			masked = i
			anchored = true
		}
		if fp.Box == nil && len(fp.Points) == 0 && !fp.Mask {
			return fmt.Errorf("matte: prompt %d (frame %d): neither a box, a point nor a mask", i, fp.Frame)
		}
		if b := fp.Box; b != nil {
			for _, v := range b {
				if !unit(v) {
					return fmt.Errorf("matte: prompt %d (frame %d): box %v outside 0..1", i, fp.Frame, *b)
				}
			}
			if !(b[0] < b[2] && b[1] < b[3]) {
				return fmt.Errorf("matte: prompt %d (frame %d): box %v is not x0 < x1, y0 < y1", i, fp.Frame, *b)
			}
			anchored = true
		}
		for j, pt := range fp.Points {
			if !unit(pt[0]) || !unit(pt[1]) {
				return fmt.Errorf("matte: prompt %d (frame %d): point %d %v outside 0..1", i, fp.Frame, j, pt)
			}
			if pt[2] != 0 && pt[2] != 1 {
				return fmt.Errorf("matte: prompt %d (frame %d): point %d label %v is not 0 or 1", i, fp.Frame, j, pt[2])
			}
			if pt[2] == 1 {
				anchored = true
			}
		}
	}
	if !anchored {
		return errors.New("matte: prompts: no box, no positive point and no mask")
	}
	return nil
}

// unit reports whether v lies in 0..1 (NaN does not).
func unit(v float64) bool { return v >= 0 && v <= 1 }

// ForFrame returns the prompts of one frame only, with the same object id —
// what TrackFrame sends for the live overlay of the clicked frame. Prompts
// is nil when the frame has none.
func (p TrackPrompts) ForFrame(frame int) TrackPrompts {
	out := TrackPrompts{Obj: p.Obj}
	for _, fp := range p.Prompts {
		if fp.Frame == frame {
			out.Prompts = append(out.Prompts, fp)
		}
	}
	return out
}

// Canonical renders the prompts as one fixed string for memo keys: the
// object id, then every prompt in frame order (a stable sort — two prompts
// of one frame keep their order), each with its box ("-" when absent) and
// its points sorted by (x, y, label), every coordinate rounded to
// promptDecimals:
//
//	obj=1|f=0:box=0.1000,0.2000,0.6000,0.9000:pts=0.3000,0.5000,1;0.7000,0.4000,0|f=12:box=-:pts=0.5000,0.5000,1
//
// A mask prompt (Phase 5d) ends its entry with the mask marker ":mask=m"
// + MaskDigest — "f=3:box=-:pts=:mask=m0123456789ab" — so the key names
// the mask that was sent, not just that one was; a prompt without a mask
// has no marker (the pre-5d text is unchanged). Fill MaskDigest
// (MaskDigestOf) before keying: with it empty the marker is a bare ":mask=m"
// and two different masks would share a key.
//
// Two prompt sets that round to the same picture give the same string; it
// never depends on the order the user clicked in. An invalid set (Validate)
// still renders — callers validate before keying.
func (p TrackPrompts) Canonical() string {
	s := p.normalised()
	var b strings.Builder
	b.WriteString("obj=")
	b.WriteString(strconv.Itoa(s.Obj))
	for _, fp := range s.Prompts {
		b.WriteString("|f=")
		b.WriteString(strconv.Itoa(fp.Frame))
		b.WriteString(":box=")
		if fp.Box == nil {
			b.WriteByte('-')
		} else {
			for i, v := range fp.Box {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(fnum(v))
			}
		}
		b.WriteString(":pts=")
		for i, pt := range fp.Points {
			if i > 0 {
				b.WriteByte(';')
			}
			b.WriteString(fnum(pt[0]))
			b.WriteByte(',')
			b.WriteString(fnum(pt[1]))
			b.WriteByte(',')
			b.WriteString(strconv.Itoa(int(pt[2])))
		}
		if fp.Mask {
			b.WriteString(":mask=m")
			b.WriteString(fp.MaskDigest)
		}
	}
	return b.String()
}

// normalised is the copy both Canonical and the wire are made from: Obj 0
// → 1, prompts stable-sorted by frame, coordinates rounded to
// promptDecimals (negative zero folded), points sorted, the mask flag and
// digest carried over. The receiver is not modified.
func (p TrackPrompts) normalised() TrackPrompts {
	out := TrackPrompts{Obj: p.Obj, Prompts: make([]FramePrompt, len(p.Prompts))}
	if out.Obj <= 0 {
		out.Obj = 1
	}
	for i, fp := range p.Prompts {
		n := FramePrompt{Frame: fp.Frame, Mask: fp.Mask, MaskDigest: fp.MaskDigest}
		if fp.Box != nil {
			b := *fp.Box
			for k := range b {
				b[k] = roundCoord(b[k])
			}
			n.Box = &b
		}
		if len(fp.Points) > 0 {
			n.Points = make([][3]float64, len(fp.Points))
			for j, pt := range fp.Points {
				n.Points[j] = [3]float64{roundCoord(pt[0]), roundCoord(pt[1]), pt[2]}
			}
			sort.SliceStable(n.Points, func(a, b int) bool {
				x, y := n.Points[a], n.Points[b]
				if x[0] != y[0] {
					return x[0] < y[0]
				}
				if x[1] != y[1] {
					return x[1] < y[1]
				}
				return x[2] < y[2]
			})
		}
		out.Prompts[i] = n
	}
	sort.SliceStable(out.Prompts, func(a, b int) bool { return out.Prompts[a].Frame < out.Prompts[b].Frame })
	return out
}

// roundCoord rounds v to promptDecimals and folds -0 into 0 (so "-0.0000"
// never appears in a key; NaN and infinities pass through — Validate
// refuses them).
func roundCoord(v float64) float64 {
	const scale = 1e4 // 10^promptDecimals
	r := math.Round(v*scale) / scale
	if r == 0 {
		return 0
	}
	return r
}

// fnum formats a rounded coordinate with promptDecimals decimals.
func fnum(v float64) string { return strconv.FormatFloat(v, 'f', promptDecimals, 64) }

// wirePrompts is the PromptsHeader JSON: points always an array, box always
// present (null when absent), so the sidecar's parser sees every field;
// Mask only when a prompt carries the mask record (the pre-5d header is
// byte-identical without one).
type wirePrompts struct {
	Obj     int          `json:"obj"`
	Prompts []wirePrompt `json:"prompts"`
	Mask    *wireMask    `json:"mask,omitempty"`
}

type wirePrompt struct {
	Frame  int          `json:"frame"`
	Points [][3]float64 `json:"points"`
	Box    *[4]float64  `json:"box"`
}

// wireMask names the frame the body's mask record belongs to.
type wireMask struct {
	Frame int `json:"frame"`
}

// headerValue validates p and renders the PromptsHeader value from the
// normalised copy (the rounding Canonical uses).
func (p TrackPrompts) headerValue() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	n := p.normalised()
	w := wirePrompts{Obj: n.Obj, Prompts: make([]wirePrompt, len(n.Prompts))}
	for i, fp := range n.Prompts {
		pts := fp.Points
		if pts == nil {
			pts = [][3]float64{}
		}
		w.Prompts[i] = wirePrompt{Frame: fp.Frame, Points: pts, Box: fp.Box}
		if fp.Mask {
			w.Mask = &wireMask{Frame: fp.Frame}
		}
	}
	data, err := json.Marshal(w)
	if err != nil {
		return "", err // NaN / Inf coordinates: Validate refused them already
	}
	return string(data), nil
}
