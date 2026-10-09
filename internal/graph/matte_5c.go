package graph

// Phase 5c (build brief 2026-10-09): the matte op's post-processing and
// guided-mode params. Stabilise, Prompts and Edge are validated here and
// recorded on MatteInput (compiler.matte) so enc / jobs can pick the
// derived sequence — the filter text does not change for them. Keep (the
// colours forced opaque after the merge) is the one 5c param that IS
// filter text: keepColour emits one union wrapper per colour behind the
// merge. CanonicalMattePrompts is the text jobs keys a tracker's clip memo
// under.

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

const (
	// MaxMattePrompts bounds recipe.MatteParams.Prompts (the prompted
	// frames of one guided matte op): every prompted frame is a
	// conditioning frame of the tracker, and the canonical text names the
	// clip memo.
	MaxMattePrompts = 32
	// defaultKeepSimilarity is the colour tolerance of the keep colours
	// (recipe.MatteParams.KeepSimilarity 0): colorkey's scale, the same
	// default as a colorkey op (defaultColorSimilarity). Mirrored by the SPA
	// like the other keying defaults (its serialiser omits it, so the zero
	// value renders through this); a change goes with a
	// jobs.PipelineVersion bump.
	defaultKeepSimilarity = 0.08
)

// matteStabiliseMode reports whether s is a stabilise mode: "" (off),
// recipe.MatteStabiliseLight or recipe.MatteStabiliseStrong.
func matteStabiliseMode(s string) bool {
	switch s {
	case "", recipe.MatteStabiliseLight, recipe.MatteStabiliseStrong:
		return true
	}
	return false
}

// matteEdge reports whether s is an edge choice of the guided model: ""
// (the device's default per-frame model), recipe.MatteEdgeNone, or a model
// id (matteModelID; whether the sidecar offers it as a segmenter is jobs'
// check — compiler.matte only refuses the tracker itself).
func matteEdge(s string) bool {
	return s == "" || s == recipe.MatteEdgeNone || matteModelID(s)
}

// matteKeep validates a matte op's keep colours and their tolerance
// (recipe.MatteParams.Keep / KeepSimilarity): at most recipe.MaxMatteKeep
// colours, each RRGGBB (keyColor: '#' and upper case are normalised, an
// alpha pair is refused, a blank entry is an error — there is no default
// keep colour), returned as lower-case hex in the order given (no dedupe:
// a repeated colour costs one redundant wrapper and changes nothing);
// KeepSimilarity through unitParam (0 = defaultKeepSimilarity, else
// MinSimilarity..1). The tolerance is validated even without colours — an
// out-of-range knob is an error, not a silent no-op, like morph and
// feather validate before deciding whether to emit.
func matteKeep(p *recipe.MatteParams) ([]string, float64, error) {
	if len(p.Keep) > recipe.MaxMatteKeep {
		return nil, 0, fmt.Errorf("at most %d keep colours (got %d)", recipe.MaxMatteKeep, len(p.Keep))
	}
	sim, err := unitParam("keepSimilarity", p.KeepSimilarity, defaultKeepSimilarity, MinSimilarity)
	if err != nil {
		return nil, 0, err
	}
	keep := make([]string, 0, len(p.Keep))
	for i, s := range p.Keep {
		if strings.TrimSpace(s) == "" {
			return nil, 0, fmt.Errorf("keep[%d]: colour is required (RRGGBB, the colour to keep opaque)", i)
		}
		hex, err := keyColor(s, "")
		if err != nil {
			return nil, 0, fmt.Errorf("keep[%d]: %v", i, err)
		}
		keep = append(keep, hex)
	}
	return keep, sim, nil
}

// validateMattePrompts checks a guided matte op's prompts
// (recipe.MattePrompt): the rules the sidecar enforces with a 400 and the
// matte client mirrors (matte.TrackPrompts.Validate), so a bad prompt set
// fails at compile time instead of after the whole clip was collected and
// sent. 1..MaxMattePrompts prompts; each with Frame >= 0 (the output frame
// on the plan's grid — the upper bound is jobs' check, which knows the
// clip's frame count; the compiler does not at this point) and a box, at
// least one point or a mask; a box [x0,y0,x1,y1] finite in 0..1 with
// x0 < x1 and y0 < y1; points [x,y,label] finite in 0..1 with label 0
// (remove) or 1 (keep); MaskFrom "" or recipe.MattePromptMaskEdge (Phase
// 5d: the frame's mask is the edge model's matte of that frame — so it
// needs an edge model, i.e. edge, the op's Edge, must not be
// recipe.MatteEdgeNone; a mask prompt counts as positive, and at most one
// prompt carries one — one mask record per track); and, over the whole
// set, at least one box, positive point or mask (a set of removals selects
// nothing — the sidecar refuses it too). The error names the prompt by its
// index in the op's list.
func validateMattePrompts(ps []recipe.MattePrompt, edge string) error {
	if len(ps) == 0 {
		return fmt.Errorf("the guided model %q needs at least one prompt (a box, a positive point or a mask)", recipe.MatteModelSAM2Tiny)
	}
	if len(ps) > MaxMattePrompts {
		return fmt.Errorf("at most %d prompts (got %d)", MaxMattePrompts, len(ps))
	}
	positive := false
	masked := -1 // the index of the prompt carrying the mask, -1 for none
	for i, p := range ps {
		if p.Frame < 0 {
			return fmt.Errorf("prompts[%d]: frame must be >= 0 (got %d)", i, p.Frame)
		}
		switch p.MaskFrom {
		case "":
		case recipe.MattePromptMaskEdge:
			if edge == recipe.MatteEdgeNone {
				return fmt.Errorf("prompts[%d]: maskFrom %q needs an edge model, but edge is %q (the mask is the edge model's matte of the frame)", i, p.MaskFrom, edge)
			}
			if masked >= 0 {
				return fmt.Errorf("prompts[%d]: a second mask prompt (prompts[%d] has one): one mask per matte op", i, masked)
			}
			masked = i
			positive = true
		default:
			return fmt.Errorf("prompts[%d]: maskFrom must be \"\" or %q (got %q)", i, recipe.MattePromptMaskEdge, p.MaskFrom)
		}
		if p.Box == nil && len(p.Points) == 0 && p.MaskFrom == "" {
			return fmt.Errorf("prompts[%d]: a prompt needs a box, at least one point or a mask (maskFrom)", i)
		}
		if b := p.Box; b != nil {
			for j, v := range b {
				if !unitCoord(v) {
					return fmt.Errorf("prompts[%d]: box[%d] must be between 0 and 1 (got %s)", i, j, fexact(v))
				}
			}
			if b[0] >= b[2] || b[1] >= b[3] {
				return fmt.Errorf("prompts[%d]: box must have x0 < x1 and y0 < y1 (got %s,%s,%s,%s)", i, fexact(b[0]), fexact(b[1]), fexact(b[2]), fexact(b[3]))
			}
			positive = true
		}
		for j, pt := range p.Points {
			if !unitCoord(pt[0]) || !unitCoord(pt[1]) {
				return fmt.Errorf("prompts[%d]: points[%d] must be between 0 and 1 (got %s,%s)", i, j, fexact(pt[0]), fexact(pt[1]))
			}
			switch pt[2] {
			case 0:
			case 1:
				positive = true
			default:
				return fmt.Errorf("prompts[%d]: points[%d] label must be 0 (remove) or 1 (keep) (got %s)", i, j, fexact(pt[2]))
			}
		}
	}
	if !positive {
		return fmt.Errorf("prompts: at least one box, positive (keep) point or mask is needed")
	}
	return nil
}

// unitCoord reports whether v is a normalised coordinate: finite, 0..1.
func unitCoord(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }

// keepColour forces the pixels of one keep colour opaque — the union of
// the merged alpha with the colour's mask (recipe.MatteParams.Keep, for
// parts of the subject the model drops). The current chain, which ends in
// the matte merge (rgba: alphamerge's output keeps its main's format, and
// mergeMatte converts the main explicitly), is closed into a three-way
// split and the graph continues with, for the N-th keep colour of the
// recipe:
//
//	<chain so far>,split=3[uN][uNm][uNe];
//	[uNm]colorkey=color=0xRRGGBB:similarity=S:blend=0,alphaextract,negate[uNk];  the keep mask
//	[uNe]alphaextract[uNa];                                                     the merged alpha
//	[uNa][uNk]blend=all_mode=lighten[uNx];                                      the maximum
//	[uN][uNx]alphamerge,…                                                       the frame with it
//
// colorkey on a copy reads the colour planes only and OVERWRITES the alpha
// from the RGB distance — 0 within similarity, 255 outside with blend=0 (a
// hard pick: the eyedropper's colour and its neighbours, no soft band that
// would half-restore the model's edges) —, and the colour planes of a
// straight-alpha rgba frame still hold the source colour where the matte
// cleared the alpha, so the pixels the model dropped are found again.
// negate on the 8-bit gray of alphaextract is 255 - v (gray8 is full
// range to lut; measured on the 2026-08 git build: 16 → 239), i.e. 255
// where the colour matched; lighten is max, so alpha = max(merged alpha,
// keep mask): a matched pixel becomes opaque whatever the matte (and
// whatever source alpha the merge multiplied in), every other pixel keeps
// the merged value exactly, and the colour planes never change. One
// wrapper per colour — chained colorkeys on one copy would not do, each
// overwrites the alpha so the last would win — at most recipe.MaxMatteKeep,
// each a split and four single-plane stages. Verified pixel-exact on the
// 2026-08 git build (matte_keep_ffmpeg_test.go): the kept colour and a
// colour within the similarity come back at 255 inside a region the matte
// removed, a colour outside it and every other pixel are byte-identical
// to the render without the keep, through both merge shapes.
func (c *compiler) keepColour(hex string, sim float64) {
	c.keeps++
	n := c.keeps
	head := c.input + strings.Join(append(slices.Clone(c.stages), "split=3"), ",")
	c.chains = append(c.chains,
		fmt.Sprintf("%s[u%d][u%dm][u%de]", head, n, n, n),
		fmt.Sprintf("[u%dm]colorkey=color=0x%s:similarity=%s:blend=0,alphaextract,negate[u%dk]", n, hex, fnum(sim), n),
		fmt.Sprintf("[u%de]alphaextract[u%da]", n, n),
		fmt.Sprintf("[u%da][u%dk]blend=all_mode=lighten[u%dx]", n, n, n),
	)
	c.input, c.stages = fmt.Sprintf("[u%d][u%dx]", n, n), nil
	c.emit("alphamerge")
}

// CanonicalMattePrompts renders guided prompts (recipe.MatteParams.Prompts)
// as one deterministic string — the text MatteInput.Prompts carries and
// jobs keys the tracker's clip memo under: the prompts sorted by Frame
// (a stable sort keeps the order of equal frames), each as
// "f<frame>" followed by ";m<maskFrom>" when the prompt carries a mask
// (Phase 5d: "f3;medge" — the mask SOURCE only; the digest of the mask
// actually sent is the matte client's business, matte.FramePrompt.MaskDigest
// in matte.TrackPrompts.Canonical), ";p<x>,<y>,<label>" per point and
// ";b<x0>,<y0>,<x1>,<y1>" for a box, coordinates with four decimals and the
// label as an integer when it is one, the prompts joined by "|". No prompts
// → "". The function only formats: it does not validate ranges, labels or
// the mask source (validateMattePrompts does, through compiler.matte).
func CanonicalMattePrompts(ps []recipe.MattePrompt) string {
	if len(ps) == 0 {
		return ""
	}
	sorted := slices.Clone(ps)
	slices.SortStableFunc(sorted, func(a, b recipe.MattePrompt) int { return a.Frame - b.Frame })
	var sb strings.Builder
	for i, p := range sorted {
		if i > 0 {
			sb.WriteByte('|')
		}
		sb.WriteByte('f')
		sb.WriteString(strconv.Itoa(p.Frame))
		if p.MaskFrom != "" {
			sb.WriteString(";m")
			sb.WriteString(p.MaskFrom)
		}
		for _, pt := range p.Points {
			sb.WriteString(";p")
			sb.WriteString(fixed4(pt[0]))
			sb.WriteByte(',')
			sb.WriteString(fixed4(pt[1]))
			sb.WriteByte(',')
			sb.WriteString(strconv.FormatFloat(pt[2], 'f', -1, 64))
		}
		if p.Box != nil {
			sb.WriteString(";b")
			for j, v := range p.Box {
				if j > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(fixed4(v))
			}
		}
	}
	return sb.String()
}

// fixed4 formats a normalised coordinate with four decimals.
func fixed4(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
