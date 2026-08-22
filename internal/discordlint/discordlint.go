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
// APNG is parsed at chunk level (IHDR, PLTE, tRNS, acTL, fcTL, fdAT, IDAT,
// IEND; CRCs are not verified): frame rectangles, delays, loop count,
// colour type / palette. Static images (PNG, JPEG, WebP, AVIF) only have
// their header read for dimensions and alpha.
//
// Rule ids are the Rule* constants in gif.go, webp.go, apng.go and
// static.go; every rule is reported (passing or not) so the UI can show a
// stable checklist, except target-specific rules (sticker/emote/attachment),
// the byte limit when the target has none, and gif.first-frame-visible when
// frame 0 cannot be decoded.
//
// Loop counts (gif.netscape-loop / webp.loop-forever) depend on the target:
// Discord targets (IsDiscord: emote, sticker and every attachment tier)
// require "loop forever" (GIF NETSCAPE2.0 count 0 — the fixer forces it;
// WebP ANIM loop count 0 — an error otherwise), because Discord honours a
// finite count and the animation stops. TargetNone treats the count as the
// user's choice: a GIF only needs a NETSCAPE2.0 block before the first
// image (inserted with count 0 when missing) and a WebP animation only
// needs an ANIM chunk; the count is reported in the detail. GIF counts
// follow NETSCAPE semantics (N = play N+1 times), WebP counts are the
// number of plays; 0 = forever in both.
//
// Report.HasAlpha is structural: it says the file carries transparency that
// decoders must honour (a GIF frame whose transparency flag is set and whose
// pixels use that index, or a frame 0 that leaves part of the canvas
// uncovered; a WebP frame with an ALPH chunk or VP8L alpha_is_used). Frame-
// diff optimised encoders (ffmpeg's gif and libwebp_anim) use transparency
// for unchanged pixels, so an opaque animation can legitimately report
// HasAlpha — the Discord rules apply to it all the same.
package discordlint

import (
	"errors"
	"fmt"
)

// Target selects which limits and rules apply. The attachment tiers
// (DESIGN.md §5.1) share every rule and differ only in the byte cap:
// IsAttachment groups them, Limit tells them apart.
type Target string

const (
	TargetNone          Target = ""               // generic (structural rules only, no byte limit)
	TargetEmote         Target = "emote"          // 262,144 B, 128x128 recommended
	TargetSticker       Target = "sticker"        // 524,288 B, 320x320 recommended (larger is shrunk by Discord — a warning), <= 5 s, <= 1000 frames, <= 60 fps
	TargetAttachment    Target = "attachment"     // 20 MB (free tier)
	TargetAttachment50  Target = "attachment-50"  // 50 MB (Nitro Basic, or a Level-2 boosted server)
	TargetAttachment100 Target = "attachment-100" // 100 MB (Level-3 boosted server)
	TargetAttachment500 Target = "attachment-500" // 500 MB (Nitro)
)

// targetInfo is what the package knows about one Discord target.
type targetInfo struct {
	limit      int64  // hard byte cap
	attachment bool   // a chat-attachment tier (IsAttachment)
	words      string // cap and who gets it, for Describe
}

// targets is the single source of truth for Limit, IsAttachment, IsDiscord
// and Describe; targetOrder is the display order Targets reports.
var (
	targets = map[Target]targetInfo{
		TargetEmote:         {limit: 262144, words: "256 KiB"},
		TargetSticker:       {limit: 524288, words: "512 KiB"},
		TargetAttachment:    {limit: 20_000_000, attachment: true, words: "20 MB, free tier"},
		TargetAttachment50:  {limit: 50_000_000, attachment: true, words: "50 MB, Nitro Basic or a Level-2 boosted server"},
		TargetAttachment100: {limit: 100_000_000, attachment: true, words: "100 MB, Level-3 boosted server"},
		TargetAttachment500: {limit: 500_000_000, attachment: true, words: "500 MB, Nitro"},
	}
	targetOrder = []Target{TargetEmote, TargetSticker, TargetAttachment, TargetAttachment50, TargetAttachment100, TargetAttachment500}
)

// Targets lists every Discord target (TargetNone excluded) in display
// order: emote, sticker, then the attachment tiers by cap.
func Targets() []Target {
	return append([]Target(nil), targetOrder...)
}

// Limit returns the hard byte cap for target (0 = none, also for a string
// that is not a known target).
func Limit(t Target) int64 {
	return targets[t].limit
}

// IsAttachment reports whether t is a chat-attachment tier (attachment,
// attachment-50, attachment-100, attachment-500). The tiers share every
// attachment rule and differ only in Limit.
func IsAttachment(t Target) bool {
	return targets[t].attachment
}

// IsDiscord reports whether t is a known Discord target — one that the
// loop-forever, palette and byte-limit rules apply to. TargetNone and
// unrecognised strings are not.
func IsDiscord(t Target) bool {
	_, ok := targets[t]
	return ok
}

// Valid reports whether t is a target a recipe may carry: TargetNone or
// any Discord target. Comparison is exact ("Emote" is not valid).
func Valid(t Target) bool {
	return t == TargetNone || IsDiscord(t)
}

// Describe words a target for details and error messages: "emote (256
// KiB)", "attachment-50 (50 MB, Nitro Basic or a Level-2 boosted server)",
// "no Discord target" for TargetNone, `unknown target "x"` otherwise.
func Describe(t Target) string {
	if t == TargetNone {
		return "no Discord target"
	}
	info, ok := targets[t]
	if !ok {
		return fmt.Sprintf("unknown target %q", string(t))
	}
	return fmt.Sprintf("%s (%s)", t, info.words)
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
	Format       string  `json:"format"`       // "gif" | "webp" | "apng" | "png" (LintAPNG: plain PNG; LintStatic) | "jpeg" | "avif"
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
//
//	2026-08-18.2  GIF + WebP rules (Phase 1).
//	2026-08-19.1  APNG (apng.*) and static-image (static.*) rules; gif.sticker-dims
//	              is a warning only when a side exceeds 320 px (Discord shrinks
//	              larger stickers and accepts smaller / non-square ones).
//	2026-08-19.2  apng.indexed OK now means colour type 3 + PLTE + tRNS (the
//	              indexed 8-bit-alpha APNG of the sticker default rung); RGBA
//	              and opaque-indexed files fail the check at LevelInfo, which
//	              does not affect Report.OK.
//	2026-08-19.3  apng.min-delay tiered like webp.min-delay: <= 10 ms warns
//	              (browsers show 100 ms), 11-19 ms is an info note recommending
//	              >= 20 ms (a Discord-legal 60 fps sticker now passes with only
//	              the note), >= 20 ms is clean; apng.container now rejects
//	              out-of-range fcTL dispose_op/blend_op (libpng rejects such
//	              files) and tRNS-before-PLTE for indexed colour (the palette
//	              alpha is silently discarded), and caps the listed unknown
//	              chunk types at 32 ("and N more types").
//	2026-08-19.4  Attachment tiers: "attachment" (20 MB, free) is joined by
//	              "attachment-50" (50 MB), "attachment-100" (100 MB) and
//	              "attachment-500" (500 MB); every attachment rule
//	              (apng.attachment, the size limits) keys on IsAttachment, so
//	              the tiers differ only in Limit. The size-limit failure
//	              detail names the tier and its cap ("… byte limit for
//	              attachment-50 (50 MB, …)"). Rules that apply to "a Discord
//	              target" (gif.netscape-loop forcing count 0, gif.global-
//	              palette as an error, webp.loop-forever / apng.plays-forever
//	              as errors) key on IsDiscord: an unrecognised target string
//	              now behaves like TargetNone instead of a cap-less Discord
//	              target.
const RulesVersion = "2026-08-19.4"

// ErrNotImplemented was returned by the pre-implementation stubs. It is
// kept for API compatibility; no linter returns it any more.
var ErrNotImplemented = errors.New("discordlint: not implemented")

// LintGIF is implemented in gif.go, LintWebP in webp.go, LintAPNG in apng.go
// (PNG chunk parser in png.go) and LintStatic in static.go.
