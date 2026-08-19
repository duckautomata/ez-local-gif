// Package discordlint checks (and where possible fixes) encoded files
// against the Discord-safety rules in docs/DESIGN.md §5.3. Discord re-encodes
// every preview through discord/lilliput; these rules encode its known
// quirks. The package works on bytes only (no ffmpeg) and must stay
// stdlib-only so it can be unit tested with synthetic files.
//
// GIF is parsed and rewritten at block level (Logical Screen Descriptor,
// Global/Local Color Tables, Graphic Control / Application / Comment /
// Plain Text extensions, Image Descriptors, LZW sub-blocks) — LZW data is
// never re-encoded. Where the fixer needs to know which palette indices a
// frame actually uses (to pick an unused transparent index for frame 0), it
// decodes the frame's pixel indices with compress/lzw (the decoder image/gif
// is built on), one frame at a time so a damaged frame does not block the
// analysis of the others.
//
// WebP is parsed as RIFF: VP8X flags/canvas, ANIM (bg colour, loop count),
// ANMF frames (offset, size, duration, blend/dispose, ALPH/VP8/VP8L payload
// to detect per-frame alpha), EXIF/XMP/ICCP presence.
//
// Rule ids are the Rule* constants in gif.go and webp.go; every rule is
// reported (passing or not) so the UI can show a stable checklist, except
// target-specific rules (sticker/emote), the byte limit when the target has
// none, and gif.first-frame-visible when frame 0 cannot be decoded.
//
// Loop counts (gif.netscape-loop / webp.loop-forever) depend on the target:
// Discord targets require "loop forever" (GIF NETSCAPE2.0 count 0 — the
// fixer forces it; WebP ANIM loop count 0 — an error otherwise), because
// Discord honours a finite count and the animation stops. TargetNone treats
// the count as the user's choice: a GIF only needs a NETSCAPE2.0 block
// before the first image (inserted with count 0 when missing) and a WebP
// animation only needs an ANIM chunk; the count is reported in the detail.
// GIF counts follow NETSCAPE semantics (N = play N+1 times), WebP counts
// are the number of plays; 0 = forever in both.
//
// Report.HasAlpha is structural: it says the file carries transparency that
// decoders must honour (a GIF frame whose transparency flag is set and whose
// pixels use that index, or a frame 0 that leaves part of the canvas
// uncovered; a WebP frame with an ALPH chunk or VP8L alpha_is_used). Frame-
// diff optimised encoders (ffmpeg's gif and libwebp_anim) use transparency
// for unchanged pixels, so an opaque animation can legitimately report
// HasAlpha — the Discord rules apply to it all the same.
package discordlint

import "errors"

// Target selects which limits and rules apply.
type Target string

const (
	TargetNone       Target = ""           // generic (structural rules only, no byte limit)
	TargetEmote      Target = "emote"      // 262,144 B, 128x128 recommended
	TargetSticker    Target = "sticker"    // 524,288 B, exactly 320x320, <= 5 s, <= 1000 frames, <= 60 fps
	TargetAttachment Target = "attachment" // 20 MB (free tier)
)

// Limit returns the hard byte cap for target (0 = none).
func Limit(t Target) int64 {
	switch t {
	case TargetEmote:
		return 262144
	case TargetSticker:
		return 524288
	case TargetAttachment:
		return 20 * 1000 * 1000
	}
	return 0
}

// Level of a check outcome.
type Level string

const (
	LevelError Level = "error" // will render wrong / be rejected on Discord
	LevelWarn  Level = "warn"  // risky or degrades experience
	LevelInfo  Level = "info"  // informational
)

// Check is one rule outcome.
type Check struct {
	Rule   string `json:"rule"`   // stable id, e.g. "gif.gce-every-frame"
	Level  Level  `json:"level"`  // severity if not OK
	OK     bool   `json:"ok"`     // true if the rule holds (after fixing, if Fixed)
	Fixed  bool   `json:"fixed"`  // true if the fixer changed the file to satisfy the rule
	Detail string `json:"detail"` // human-readable explanation
}

// Report summarises a lint run.
type Report struct {
	RulesVersion string  `json:"rulesVersion"` // bump when rules change; stamped into results
	Format       string  `json:"format"`       // "gif" | "webp" | "apng" | "png"
	Target       Target  `json:"target"`
	Bytes        int64   `json:"bytes"`
	Limit        int64   `json:"limit"` // from Limit(Target); 0 = none
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	Frames       int     `json:"frames"`
	DurationMS   int     `json:"durationMs"`
	MinDelayMS   int     `json:"minDelayMs"`
	LoopForever  bool    `json:"loopForever"` // loop count 0 (after fixing); also true for stills and single-frame files, where looping does not apply
	HasAlpha     bool    `json:"hasAlpha"`
	Checks       []Check `json:"checks"`
	OK           bool    `json:"ok"` // no LevelError check failed
}

// RulesVersion identifies the rule set implemented by this build.
const RulesVersion = "2026-08-18.2"

// ErrNotImplemented was returned by the pre-implementation stubs. It is
// kept for API compatibility; LintGIF and LintWebP no longer return it.
var ErrNotImplemented = errors.New("discordlint: not implemented")

// LintGIF is implemented in gif.go and LintWebP in webp.go.
