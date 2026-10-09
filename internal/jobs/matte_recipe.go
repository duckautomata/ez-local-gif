package jobs

// The matte op at the recipe level (Phase 5b integration, Phase 5c
// params): what the pipeline's other stages read off a matte op's params
// without resolving anything — the canonical form the autocrop detection
// key hashes (canonicalMatteOp), the render.matte notes the report line
// carries beyond the resolved identity (matteRecipeNotes /
// applyMatteRecipeNotes), and how a compiled plan's matte input finds its
// resolution (findMatteInput: by model, size and the op's stabilise /
// prompts / edge) and the directory it reads (matteInputDir: the derived
// sequence the resolution made, else the memo). The resolution itself —
// facts, memo, pass, the derived sequences — is matte.go's / matte_5c.go's.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// findMatteInput pairs a plan's matte input with its resolution (Phase
// 5c): the resolved matte of the same model and requested size whose
// stabilise mode, canonical prompts and edge (as written) are the input's
// — the compiler's dedupe key, which resolveMattes resolves one request
// per — so two matte ops of one model that differ in a 5c param each read
// their own sequence. A resolution built without those fields (a literal:
// the tests, a caller that resolved by model and size alone) is matched
// the Phase 5b way as the fallback (findMatte); nil when there is none.
func findMatteInput(mattes []resolvedMatte, in *graph.MatteInput) *resolvedMatte {
	return findMatteFor(mattes, matteRequest{Model: in.Model, Size: in.Size, Stabilise: in.Stabilise, Prompts: in.Prompts, Edge: in.Edge})
}

// findMatteFor is findMatteInput for a request (matteRequestOf of an op's
// params): the resolution whose model, requested size, stabilise mode,
// canonical prompts and edge (as written) are the request's, else the
// Phase 5b match by model and size (findMatte) for a literal; nil when
// there is none. checkMatteResolved pairs a job's ops with their
// resolutions through it.
func findMatteFor(mattes []resolvedMatte, req matteRequest) *resolvedMatte {
	for i := range mattes {
		rm := &mattes[i]
		if rm.Model == req.Model && rm.ReqSize == req.Size && rm.Stabilise == req.Stabilise && rm.Prompts == req.Prompts && rm.ReqEdge == req.Edge {
			return rm
		}
	}
	return findMatte(mattes, req.Model, req.Size)
}

// matteInputDir is the directory a plan's matte input in reads its
// %06d.png sequence from, given the matte it resolved to (rm, a complete
// memo): the sequence the resolution derived for the op (rm.SeqDir —
// "<clipdir>/stab-<mode>/", "<clipdir>/gated-<edge>-<edgeKey>-r3/" or the gate's
// stabilised sequence, made by resolveMatte with enc.MatteStabiliseArgs /
// MatteGateArgs; the same frame count as the memo, so Frames stays the
// manifest's), else the raw memo dir — or, for a resolution built without
// one (a literal, see findMatteInput), the stabilised sequence the input's
// own mode names under the memo dir. Stills, proxies, the autocrop
// detection and the render all take their input through here, which is
// what makes a preview match its render.
func matteInputDir(rm *resolvedMatte, in *graph.MatteInput) string {
	if rm.SeqDir != "" {
		return rm.SeqDir
	}
	dir := rm.Dir
	if in.Stabilise != "" {
		dir = store.MatteDerivedDir(dir, store.MatteStabName(in.Stabilise))
	}
	return dir
}

// canonicalMatteOp returns op (a matte op) rewritten to its canonical
// decoded params for the autocrop detection key (autocropDetectionOps):
// decodeMatteOp's form — the model ("" resolved to the default), the size,
// never Resolved — plus the Phase 5c params in a spelling-independent
// shape (canonicalMatteParams), so a client that spells the same op as no
// params, {} or {"model": …}, "#3A7BD5" or "3a7bd5", or lists its prompts
// in another order gets the same detection memo. Malformed params are
// returned as they are for the compiler to report.
func canonicalMatteOp(idx int, op recipe.Op) recipe.Op {
	p, err := decodeMatteOp(idx, op)
	if err != nil {
		return op
	}
	raw, err := json.Marshal(canonicalMatteParams(p))
	if err != nil {
		return op
	}
	return recipe.Op{Kind: recipe.OpMatte, Params: raw}
}

// canonicalMatteParams normalises the Phase 5c params of p the way the
// compiler reads them: keep colours as lowercase RRGGBB without the hash
// (recipe.NormalizeHex — an invalid entry is left as given, the compiler
// refuses it), prompts stably sorted by frame (graph.CanonicalMattePrompts
// orders them so; the input's identity does not depend on the order the
// card listed them in). Stabilise, Edge and KeepSimilarity are exact
// tokens / numbers already; Resolved is never part of the canonical form.
func canonicalMatteParams(p recipe.MatteParams) recipe.MatteParams {
	p.Resolved = nil
	if len(p.Keep) > 0 {
		keep := make([]string, len(p.Keep))
		for i, k := range p.Keep {
			if hex, err := recipe.NormalizeHex(k); err == nil {
				keep[i] = hex
			} else {
				keep[i] = k
			}
		}
		p.Keep = keep
	}
	if len(p.Prompts) > 1 {
		ps := slices.Clone(p.Prompts)
		slices.SortStableFunc(ps, func(a, b recipe.MattePrompt) int { return cmp.Compare(a.Frame, b.Frame) })
		p.Prompts = ps
	}
	return p
}

// matteRecipeNotes words what the recipe's matte ops asked for beyond the
// resolved identity (Phase 5c) — "guided (sam2-tiny) + edge birefnet-lite ·
// 3 prompted frames", "stabilise light", "keep 2 colours" — for the
// render.matte info line (applyMatteRecipeNotes). ops are the job's ops as
// Submit filled them (Resolved carries the edge model id a tracker matte
// was gated with); a malformed op contributes nothing (the compile reports
// it). Notes are in op order, duplicates dropped.
func matteRecipeNotes(ops []recipe.Op) []string {
	var notes []string
	add := func(s string) {
		if !slices.Contains(notes, s) {
			notes = append(notes, s)
		}
	}
	for i, op := range ops {
		if op.Kind != recipe.OpMatte {
			continue
		}
		var res *recipe.MatteResolved
		if len(op.Params) > 0 {
			var raw recipe.MatteParams
			if json.Unmarshal(op.Params, &raw) == nil {
				res = raw.Resolved
			}
		}
		p, err := decodeMatteOp(i, op)
		if err != nil {
			continue
		}
		if p.Model == recipe.MatteModelSAM2Tiny || (res != nil && res.Tracker != "") {
			s := "guided (" + p.Model + ")"
			edge := p.Edge
			if res != nil && res.Edge != "" {
				edge = res.Edge
			}
			switch edge {
			case recipe.MatteEdgeNone:
				s += ", tracker mask only"
			case "":
				s += " + edge (the device's default model)"
			default:
				s += " + edge " + edge
			}
			if n := len(p.Prompts); n == 1 {
				s += " · 1 prompted frame"
			} else if n > 1 {
				s += fmt.Sprintf(" · %d prompted frames", n)
			}
			add(s)
		}
		if mode := strings.TrimSpace(p.Stabilise); mode != "" {
			add("stabilise " + mode)
		}
		if n := len(p.Keep); n > 0 {
			s := "keep 1 colour"
			if n > 1 {
				s = fmt.Sprintf("keep %d colours", n)
			}
			if p.KeepSimilarity > 0 {
				s += " (similarity " + strconv.FormatFloat(p.KeepSimilarity, 'f', -1, 64) + ")"
			}
			add(s)
		}
	}
	return notes
}

// applyMatteRecipeNotes appends matteRecipeNotes(ops) to the render.matte
// check applyMatteInfo put on rep (nil-safe; nothing without that check or
// without notes): "AI matte: isnet-anime 1024 px fp16 · … · 45 frames · cuda
// 18.4 ms/frame · stabilise light · keep 1 colour". A note whose headword
// (guided / stabilise / keep) the detail already carries is not repeated —
// the same word-wise check the SPA's Result card applies before adding its
// own notes from the recipe, so the line never says a thing twice whoever
// worded it first.
func applyMatteRecipeNotes(rep *discordlint.Report, ops []recipe.Op) {
	if rep == nil {
		return
	}
	notes := matteRecipeNotes(ops)
	if len(notes) == 0 {
		return
	}
	for i := range rep.Checks {
		c := &rep.Checks[i]
		if c.Rule != RuleRenderMatte {
			continue
		}
		have := strings.ToLower(c.Detail)
		for _, n := range notes {
			word := "keep"
			switch {
			case strings.HasPrefix(n, "guided"):
				word = "guided"
			case strings.HasPrefix(n, "stabilise"):
				word = "stabilise"
			}
			if strings.Contains(have, word) {
				continue
			}
			c.Detail += " · " + n
			have += " · " + strings.ToLower(n)
		}
		return
	}
}
