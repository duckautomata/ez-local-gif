package matte

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// KeyVersion salts ClipKey (the spec's matteKeyVersion, §11): bump it when
// the producer chain (stretch / format), the frame hash or the memo layout
// changes. It never sees jobs.PipelineVersion — a 20-minute CPU pass
// survives app upgrades.
const KeyVersion = "1"

// FrameKey is the frames-store key of one model input: the sha256 hex of
// the size×size×3 rgb24 bytes the model receives. It sees only pixels, so a
// held pose, an fps-upsampled duplicate or an earlier trim of the same clip
// hits the store.
func FrameKey(rgb []byte) string {
	sum := sha256.Sum256(rgb)
	return hex.EncodeToString(sum[:])
}

// ClipKeyParts are the inputs of ClipKey (spec §4.2). Weights and Proc come
// from the persisted facts (Facts), never from a live digest; the device is
// deliberately absent (CPU and GPU mattes of one graph are equivalent).
type ClipKeyParts struct {
	Src         string           // the source blob hash
	Temporal    []recipe.Op      // the stack's temporal ops in stack order (TemporalOps)
	Probe       recipe.ProbeInfo // the source's probe; only the facts the temporal prefix compiles from are keyed
	InfoVersion int              // the store.InfoVersion the probe facts were stored under
	FPS         string           // the compiled plan's effective rate as filter text (graph's fnum(Plan.FPS))
	Model       string           // model id
	Size        int              // the effective input square
	Precision   string           // fp16 / fp32 of the graph the sidecar runs
	Weights     string           // sha256 of the pinned source ONNX
	Proc        string           // the sidecar's processingVersion
}

// TemporalOps returns the ops of a stack that shape the frames the model
// sees in time — delay, unpremultiply, trim, speed, fps — in stack order.
// Keys, feather, morph, geometry, reverse/bounce, text and overlays are not
// among them: the model sees RGB at the source frame size on the plan's
// forward output grid.
func TemporalOps(ops []recipe.Op) []recipe.Op {
	var out []recipe.Op
	for _, op := range ops {
		switch op.Kind {
		case recipe.OpDelay, recipe.OpUnpremultiply, recipe.OpTrim, recipe.OpSpeed, recipe.OpFPS:
			out = append(out, op)
		}
	}
	return out
}

// probeFacts are the ProbeInfo fields the temporal prefix compiles from —
// the frame grid and the alpha head — in a fixed order, so a re-probe under
// new semantics that changes them changes the key. Sequence.Mixed is among
// them (an addition to the spec's §4.2 list, recorded there): a mixed-size
// sequence is normalised to a canvas, which changes the frames the model
// sees under otherwise identical facts.
type probeFacts struct {
	Kind          recipe.Kind `json:"kind"`
	IsStill       bool        `json:"isStill"`
	Width         int         `json:"width"`
	Height        int         `json:"height"`
	FPS           float64     `json:"fps"`
	Duration      float64     `json:"duration"`
	ColorStream   int         `json:"colorStream"`
	AlphaStream   int         `json:"alphaStream"`
	Premultiplied bool        `json:"premultiplied"`
	Sequence      *seqFacts   `json:"sequence"`
}

type seqFacts struct {
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
	DelayMS int    `json:"delayMs"`
	Mixed   bool   `json:"mixed"`
}

// ClipKey is the memo key of one clip's matte sequence (spec §4.2):
//
//	sha256("matte|" + KeyVersion + "\n" + canonical(Recipe{Sources: [Src], Ops: Temporal})
//	       + "\nprobe=" + json(probe facts) + "|info=" + InfoVersion
//	       + "\nfps=" + FPS + "|model=" + Model + "|size=" + Size + "|prec=" + Precision
//	       + "|weights=" + Weights + "|proc=" + Proc)
//
// as lowercase hex, the probe facts being Kind, IsStill, Width, Height,
// FPS, Duration, ColorStream, AlphaStream, Premultiplied and Sequence
// {Pattern, Count, DelayMS, Mixed} (probeFacts). Like recipe.Hash it panics
// only when a temporal op's params are not valid JSON — the compiler has
// validated them by the time a key is made.
func ClipKey(parts ClipKeyParts) string {
	sum := sha256.Sum256([]byte(clipKeyText(parts)))
	return hex.EncodeToString(sum[:])
}

// clipKeyText is the text ClipKey hashes (golden-tested).
func clipKeyText(p ClipKeyParts) string {
	canon, err := recipe.Recipe{Sources: []string{p.Src}, Ops: p.Temporal}.Canonical()
	if err != nil {
		panic(err)
	}
	facts := probeFacts{
		Kind:          p.Probe.Kind,
		IsStill:       p.Probe.IsStill,
		Width:         p.Probe.Width,
		Height:        p.Probe.Height,
		FPS:           p.Probe.FPS,
		Duration:      p.Probe.Duration,
		ColorStream:   p.Probe.ColorStream,
		AlphaStream:   p.Probe.AlphaStream,
		Premultiplied: p.Probe.Premultiplied,
	}
	if s := p.Probe.Sequence; s != nil {
		facts.Sequence = &seqFacts{Pattern: s.Pattern, Count: s.Count, DelayMS: s.DelayMS, Mixed: s.Mixed}
	}
	fj, err := json.Marshal(facts)
	if err != nil {
		panic(err) // a struct of scalars cannot fail to marshal
	}
	return "matte|" + KeyVersion + "\n" + string(canon) +
		"\nprobe=" + string(fj) + "|info=" + itoa(p.InfoVersion) +
		"\nfps=" + p.FPS + "|model=" + p.Model + "|size=" + itoa(p.Size) + "|prec=" + p.Precision +
		"|weights=" + p.Weights + "|proc=" + p.Proc
}

func itoa(n int) string { return strconv.Itoa(n) }
