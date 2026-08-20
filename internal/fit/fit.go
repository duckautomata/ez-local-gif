// Package fit implements the fit-to-size search of DESIGN.md §5.4: a ladder
// of rungs (fps / colours / scale / dither, mildest first) each searched over
// one monotone quality knob with a secant search on log(size), run in
// parallel, with a hard byte cap and a small margin. The winner is the
// mildest rung that fits; the next rungs' best candidates are reported as
// alternatives. It is pure orchestration: encoding is delegated to the
// caller's Encode function (internal/jobs wires enc + ffrun), so the package
// stays process-free and testable with fake encoders.
package fit

import (
	"context"
	"errors"
	"fmt"
)

// Rung is one ladder step: how the master is pre-filtered before encoding.
// Zero values mean "as the master" (no fps drop, no scale). Colors/Dither
// only matter for palette formats.
type Rung struct {
	FPS    float64 // output fps for this rung (0 = master fps)
	Width  int     // downscale target (0 = master size); Height follows aspect when 0
	Height int
	Colors int    // palette size for gif/apng rungs (0 = format default)
	Dither string // "bayer" | "sierra2_4a" | "none" | "" (format default)
	Format string // output format of this rung (a sticker ladder switches apng → gif); "" = request default
	Label  string // human label, e.g. "25 fps · 256 colours · 128 px"

	// Truecolor marks an RGBA truecolour probe rung (sticker APNG ladder,
	// DESIGN.md §5.4/§9a): the encoder must keep full RGBA instead of
	// quantising, and Colors is ignored. Candidate.Desc carries no knob for
	// such rungs (the knob changes nothing).
	Truecolor bool

	// Knob optionally overrides the search knob for this rung alone; it wins
	// over Request.Knobs and Request.Knob and is normalised/validated like
	// them. Ladder builders set it to bound the APNG/PNG colour-step knob by
	// the rung's own palette (floor 64: a 64-colour rung becomes a
	// single-point probe) and to give RGBA probe rungs their single-probe
	// knob. nil = no override.
	Knob *Knob
}

// Knob is the single monotone quality control searched per rung. Larger
// values always mean "smaller file, lower quality" from the engine's point
// of view (the caller maps it: gif → gifsicle --lossy 0..200; webp/avif →
// 100-quality; apng → colour reduction steps; jpeg → 100-quality).
type Knob struct {
	Min, Max int // inclusive range (Min = mildest, Max = harshest)
	Mild     int // first probe (mild); Min when 0
	Harsh    int // second probe (harsh); Max when 0
	Name     string
}

// Candidate is one encoded attempt that fit under the target.
type Candidate struct {
	Rung   Rung
	Knob   int
	Bytes  int64
	Path   string // file written by Encode
	Format string // effective format
	Desc   string // binding knob description, e.g. "fit at 20 fps · 128 colours · lossy 60"
}

// Request configures a search.
type Request struct {
	Target   int64   // hard byte cap (e.g. 262144)
	Margin   float64 // fraction kept free under Target (0 = 0.02)
	Ladder   []Rung  // mildest first; empty = error
	Knob     Knob
	MaxIter  int // secant iterations per rung after the two probes (0 = 5)
	Parallel int // concurrent Encode calls (0 = 1)
	// Alternatives is how many rungs harsher than the winner keep encoding
	// once a fit is found; their best candidates become Result.Alternatives
	// (0 = 2, negative = none).
	Alternatives int

	// Knobs optionally overrides Knob per rung format for ladders that mix
	// formats (StickerAPNGThenGIF: "apng" rungs search colour steps, "gif"
	// rungs search gifsicle lossy). Entries are looked up by a rung's
	// effective format (Rung.Format, or Request.Format when that is ""); a
	// rung whose effective format has no entry uses Knob. Optional; nil
	// means every rung uses Knob.
	Knobs map[string]Knob
	// Format is the request's default output format; it is copied into
	// Candidate.Format for rungs whose own Format is "" so Candidate.Format
	// is always the effective format. Optional ("" leaves such candidates
	// with an empty Format for the caller to resolve).
	Format string
}

// EncodeFunc encodes the master for rung at knob into a new file under
// scratch and returns its path and size. It must be safe to call
// concurrently. A non-nil error aborts that rung only (it is recorded in
// Result.Errors); ctx cancellation aborts the whole search.
//
// attempt counts this rung's Encode calls from 1 (1 = mild probe, 2 = harsh
// probe, 3.. = secant steps) so the caller can derive unique scratch names.
// A result of 0 bytes is treated as an encoder failure.
type EncodeFunc func(ctx context.Context, rung Rung, knob int, attempt int) (path string, bytes int64, err error)

// Result of a search.
type Result struct {
	Best         *Candidate  // nil if nothing fit
	Alternatives []Candidate // best candidate of the next rungs that fit (mildest first), at most Request.Alternatives
	Tried        int         // number of Encode calls
	Errors       []string    // per-rung failures (non-fatal)
	Skipped      []string    // rungs whose harsh probe was still over the target (labels)
}

// ErrNotImplemented is returned by stubs that have not been filled in yet.
var ErrNotImplemented = errors.New("fit: not implemented")

// ErrNoFit is returned when no rung produced a file under the target.
var ErrNoFit = errors.New("fit: no candidate fits under the target")

// PanicError is the value Search re-panics with, in its caller's goroutine,
// when the EncodeFunc panicked inside one of the search's rung goroutines.
// It carries the original panic value and the panicking goroutine's stack,
// so a recover around Search (e.g. a job runner's) sees the real fault
// instead of the process dying on an unguarded goroutine.
type PanicError struct {
	Value any
	Stack []byte
}

func (p *PanicError) Error() string {
	return fmt.Sprintf("fit: encoder panicked: %v\n%s", p.Value, p.Stack)
}

// Search runs the ladder: for each rung (in parallel, bounded by
// req.Parallel) probe Knob.Mild; if it fits the rung is done (record it);
// else probe Knob.Harsh; if that does not fit the rung is skipped; else
// iterate a secant search on log(size) vs knob for at most req.MaxIter
// steps, keeping the best candidate under Target*(1-Margin) (stop early when
// within [Target*(1-Margin-0.02), Target*(1-Margin)]). Rungs are started
// mildest first; once a rung fits, rungs milder than it that are still
// running are allowed to finish (they could still win) and at most
// req.Alternatives harsher rungs are started/finished. The winner is the
// mildest rung with a candidate; Result.Alternatives are the next rungs'
// best candidates. Returns ErrNoFit (with a populated Result) when nothing
// fits. The caller owns the files in Candidate.Path and must delete the rest.
//
// Details of this implementation (see search.go): the secant is a bracketed
// regula falsi on log(size) between the last over-budget and under-budget
// probes (Illinois damping, integer knobs, so it never leaves [Min, Max]),
// aiming at the centre of the stop window; the "best" candidate of a rung is
// the largest file under the limit (ties → milder knob). Harsher rungs that
// were already running when a milder rung fit are cancelled through their
// context as soon as they fall outside the winner + Alternatives window. Any
// rung outside the final window — cancelled or already completed under an
// earlier, wider window — contributes no candidate, error or skip (its
// Encode calls still count in Result.Tried), so the Result does not depend
// on goroutine completion order. When ctx is cancelled the search stops starting
// rungs, waits for the running Encode calls to return and reports the ctx
// error (wrapped) together with whatever had completed. Result.Errors and
// Result.Skipped are ordered by rung (mildest first). A rung with a non-nil
// Rung.Knob is searched over that knob instead of Request.Knobs/Knob.
//
// A panicking EncodeFunc does not kill the process from a search goroutine:
// the panic is recovered on the rung goroutine, every other rung is
// cancelled and drained, and Search re-panics in its caller's goroutine with
// a *PanicError carrying the original value and stack — so a recover around
// Search (e.g. a job runner's) turns it into an error.
func Search(ctx context.Context, req Request, encode EncodeFunc) (Result, error) {
	s, err := newSearcher(req, encode)
	if err != nil {
		return Result{}, err
	}
	return s.run(ctx)
}

// Ladders (DESIGN.md §5.4 preset table). They take the master's fps and
// size and return rungs mildest-first; callers filter by KeepSize/KeepFPS
// (Filter does that for preset ladders; Generic takes the flags directly).
//
// Common rules (ladder.go): a rung fps at or above the master fps becomes
// "master fps" (FPS 0) and a rung size at or above the master's longer side
// becomes "master size" (Width/Height 0) — a ladder never asks for more
// frames or pixels than the master has; downscales set both Width and
// Height from the longer side so portrait masters are never upscaled; rungs
// that collapse onto an earlier one are dropped; labels are rendered from
// the effective values ("15 fps · 256 colours · 128 px" for a 15 fps
// master, "128×64" for non-square sizes). Unknown master fps/size (0) is
// treated as "keep the master's".

// EmoteGIF: (25,256,128,bayer) → (20,128) → (16.7,128) → (12.5,64,112,none) → (10,32,96).
func EmoteGIF(masterFPS float64, w, h int) []Rung {
	return buildLadder(master{masterFPS, w, h}, emoteGIFSteps, false)
}

// EmoteWebP: same fps rungs at 128/112/96 px with the quality knob.
func EmoteWebP(masterFPS float64, w, h int) []Rung {
	return buildLadder(master{masterFPS, w, h}, emoteWebPSteps, false)
}

// StickerAPNGThenGIF: RGBA truecolour APNG probes (25 → 20 → 16.7 → 12.5
// fps, DESIGN.md §5.4/§9a "RGBA APNG probe → indexed → GIF", ≥ 12 fps
// floor), then indexed APNG rungs (25,256) → (20,256) → (16.7,256) →
// (12.5,128) → (10,64), then GIF rungs with the lossy knob (Format "gif").
//
// The RGBA rungs carry Truecolor plus a single-point Rung.Knob, so each
// costs one encode (fits → done, else skipped); the encoder must keep full
// RGBA for them instead of quantising. The indexed rungs carry a Rung.Knob
// bounded by their own palette (floor 64). Stickers are never downscaled
// (w, h only feed the labels). The GIF rungs are (25,256,bayer) → (20,128) →
// (16.7,128) → (12.5,64,none) → (10,32). Labels carry the format
// ("APNG · RGBA · 25 fps · 320 px", "APNG · 25 fps · 256 colours · 320 px")
// since both halves share fps/colour values. Pair with Knob =
// KnobFor("apng") and Knobs = {"gif": KnobFor("gif")}; the per-rung knobs
// win where set.
func StickerAPNGThenGIF(masterFPS float64, w, h int) []Rung {
	m := master{masterFPS, w, h}
	rungs := buildLadder(m, stickerRGBASteps, true)
	rungs = append(rungs, buildLadder(m, stickerAPNGSteps, true)...)
	rungs = append(rungs, buildLadder(m, stickerGIFSteps, true)...)
	return dedupeRungs(rungs)
}

// Generic: for "compress to X" on an arbitrary output — knob-only at the
// master's settings first, then fps rungs (unless keepFPS), then colour
// rungs (palette formats), then scale rungs (unless keepSize).
//
// fps rungs are 30 → 24 → 20 → 15 (those below the master fps; none for
// static formats or an unknown fps); colour rungs 128 → 64 at the lowest fps
// reached (gif/apng/png only); scale rungs 75 % → 50 % of the longer side at
// the lowest fps and colours reached. Every rung carries Format = format.
func Generic(format string, masterFPS float64, w, h int, keepSize, keepFPS bool) []Rung {
	return genericLadder(format, master{masterFPS, w, h}, keepSize, keepFPS)
}

// KnobFor returns the Knob for a format: gif → lossy 0..200 (mild 30, harsh
// 150); webp/avif → 100-quality, 5..90 (mild 20 = q80, harsh 70 = q30);
// apng → colour halvings below the rung's colours, floored at 64 per
// DESIGN.md §5.4 (steps 0..2 = 256 → 128 → 64 for the default palette; mild
// 0, harsh 2); jpeg → 100-quality 10..80.
//
// Additions: png (static, pngquant) shares the apng colour-step knob; jpeg
// probes mild 20 (q80) / harsh 60 (q40); any other format gets a generic
// "level" knob 0..100 (mild 20, harsh 70). Names are the Knob* constants.
// Ladder-built APNG/PNG rungs with an explicit palette carry a per-rung
// Rung.Knob override bounding the steps by their own colours (a 64-colour
// rung is a single probe), so the search never quantises below 64.
func KnobFor(format string) Knob {
	return knobFor(format)
}
