// Package recipe defines the data model shared by the HTTP API, the graph
// compiler (internal/graph), the encoders (internal/enc), the job runner
// (internal/jobs) and the frontend: what to process (Sources), how (Ops) and
// what to produce (Output). A Recipe is content-addressable via Hash(), which
// is what memoises results on disk.
//
// This package is stdlib-only and must stay free of ffmpeg specifics.
package recipe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Kind classifies a source.
type Kind string

const (
	KindVideo     Kind = "video"     // mp4, mkv, mov (ProRes), webm ...
	KindAnimation Kind = "animation" // animated gif / webp / apng / avif
	KindImage     Kind = "image"     // single still image
	KindSequence  Kind = "sequence"  // uploaded image sequence (Phase 2)
)

// ProbeInfo describes a source as reported by ffprobe plus derived facts.
type ProbeInfo struct {
	Format   string  `json:"format"`            // ffprobe format_name, e.g. "mov,mp4,m4a,3gp,3g2,mj2", "gif", "webp_anim", "apng", "png_pipe"
	Codec    string  `json:"codec"`             // codec_name of the first video stream: prores, h264, gif, webp, apng, png ...
	Profile  string  `json:"profile,omitempty"` // codec profile, e.g. "4444" for ProRes 4444
	PixFmt   string  `json:"pixFmt"`            // e.g. yuva444p10le, rgba, bgra, yuv420p
	Bits     int     `json:"bits"`              // component bit depth (8/10/12/16) when known, else 0
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	FPS      float64 `json:"fps"`      // nominal frame rate; 0 for stills
	Duration float64 `json:"duration"` // seconds; 0 for stills / unknown
	Frames   int     `json:"frames"`   // 1 for stills; best effort otherwise (0 = unknown)
	HasAlpha bool    `json:"hasAlpha"` // true if the pixel format carries alpha AND (for palette/animation formats) any pixel is not opaque
	HasAudio bool    `json:"hasAudio"`
	IsStill  bool    `json:"isStill"`
	Kind     Kind    `json:"kind"`
	// Premultiplied is the best guess of how the source's alpha is stored.
	// ProRes 4444 defaults to true (DaVinci Resolve exports premultiplied);
	// the UI shows this as the "premultiplied source" toggle whose value is
	// then expressed as the "unpremultiply" op.
	Premultiplied bool `json:"premultiplied"`

	// ColorStream is the video-stream index ("v:N") of the colour stream the
	// graph must read. 0 for almost every source; > 0 for animated AVIF,
	// where ffmpeg's mov demuxer lists the one-frame primary item first and
	// the animation track after it (typically v:2, with its alpha at v:3).
	ColorStream int `json:"colorStream,omitempty"`

	// AlphaStream is the video-stream index ("v:N") of a separate
	// single-plane alpha stream belonging to ColorStream (ffmpeg's mov
	// demuxer exposes AVIF alpha this way). 0 = the alpha, if any, is in the
	// colour stream's pix_fmt. When > 0 the graph merges it with alphamerge
	// before any other stage.
	AlphaStream int `json:"alphaStream,omitempty"`

	// Sequence is set for image-sequence sources (Kind == KindSequence): the
	// blob is a directory of frames named by Pattern.
	Sequence *SequenceInfo `json:"sequence,omitempty"`
}

// SequenceInfo describes an uploaded image sequence (Phase 2).
type SequenceInfo struct {
	Count   int    `json:"count"`   // number of frames
	Pattern string `json:"pattern"` // file name pattern inside the blob dir, e.g. "%06d.png" (ffmpeg image2 demuxer)
	DelayMS int    `json:"delayMs"` // default per-frame duration in ms (100 unless the client said otherwise); the "delay" op overrides it per recipe
	Mixed   bool   `json:"mixed"`   // frames differ in size; the graph scales/pads them to Width x Height (the largest frame)
}

// Source is an uploaded blob plus its probe info, as returned by the API.
type Source struct {
	Hash string    `json:"hash"` // sha256 hex of the file bytes; also the blob id
	Name string    `json:"name"` // original file name
	Size int64     `json:"size"`
	Info ProbeInfo `json:"info"`
}

// Op is one step of the non-destructive edit stack. Params are decoded per
// Kind by package graph using the *Params structs below. Unknown kinds are a
// compile error. Order matters except for OpUnpremultiply, which the compiler
// always hoists to run first (right after decode, at native bit depth).
type Op struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Op kinds implemented in Phase 1.
const (
	OpTrim          = "trim"          // TrimParams
	OpCrop          = "crop"          // CropParams
	OpResize        = "resize"        // ResizeParams
	OpCanvas        = "canvas"        // CanvasParams
	OpFPS           = "fps"           // FPSParams
	OpSpeed         = "speed"         // SpeedParams
	OpFlip          = "flip"          // FlipParams
	OpRotate        = "rotate"        // RotateParams
	OpUnpremultiply = "unpremultiply" // no params
)

// Op kinds added in Phase 2.
const (
	// OpDelay sets the per-frame duration of an image-sequence source
	// (DelayParams); ignored for other sources. Like OpUnpremultiply it is
	// hoisted by the compiler (it becomes the image2 demuxer's -framerate).
	OpDelay = "delay"
)

// DelayParams: frame duration in milliseconds for image sequences (1..60000).
type DelayParams struct {
	MS int `json:"ms"`
}

// TrimParams selects a time range of the source, in seconds. End <= 0 means
// "to the end".
type TrimParams struct {
	Start float64 `json:"start"`
	End   float64 `json:"end,omitempty"`
}

// CropParams crops in source pixel coordinates (after any previous crop).
type CropParams struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// ResizeParams scales the frame. Width or Height may be 0 to keep the aspect
// ratio. Fit: "contain" (default; largest size that fits inside Width x Height,
// keeping aspect), "cover" (smallest size that covers, keeping aspect, then
// center-crop), "exact" (stretch to Width x Height).
type ResizeParams struct {
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Fit    string `json:"fit,omitempty"`
}

// CanvasParams pads (or crops) the frame to Width x Height, centered. Color is
// a hex RRGGBB or RRGGBBAA; "" means fully transparent.
type CanvasParams struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Color  string `json:"color,omitempty"`
}

// FPSParams sets the output frame rate. The compiler may snap it per output
// format (see graph.SnapFPS).
type FPSParams struct {
	FPS float64 `json:"fps"`
}

// SpeedParams changes playback speed. Factor 2 = twice as fast (half the
// duration). Must be > 0.
type SpeedParams struct {
	Factor float64 `json:"factor"`
}

// FlipParams mirrors the frame.
type FlipParams struct {
	Horizontal bool `json:"horizontal,omitempty"`
	Vertical   bool `json:"vertical,omitempty"`
}

// RotateParams rotates clockwise by Degrees, one of 90, 180, 270.
type RotateParams struct {
	Degrees int `json:"degrees"`
}

// Output formats. Animated formats encode the whole master (a single-frame
// master yields a still in the same container); static formats encode the
// master's first frame; FormatFrames exports every frame as an image file
// plus a zip.
const (
	FormatGIF    = "gif"
	FormatWebP   = "webp"
	FormatAPNG   = "apng"   // Phase 2
	FormatAVIF   = "avif"   // Phase 2 (animated or still)
	FormatPNG    = "png"    // Phase 2, static
	FormatJPEG   = "jpeg"   // Phase 2, static (flattened onto Matte)
	FormatFrames = "frames" // Phase 2, frame extraction (FrameFormat per frame + frames.zip)
)

// IsAnimatedFormat reports whether f can hold more than one frame.
func IsAnimatedFormat(f string) bool {
	switch f {
	case FormatGIF, FormatWebP, FormatAPNG, FormatAVIF:
		return true
	}
	return false
}

// IsStaticFormat reports whether f always encodes a single image.
func IsStaticFormat(f string) bool {
	switch f {
	case FormatPNG, FormatJPEG:
		return true
	}
	return false
}

// Output describes what to encode. Zero values mean "default"; the effective
// defaults are documented per field and applied by the encoders.
type Output struct {
	Format string `json:"format"` // one of the Format* constants ("mp4"/"webm" arrive in Phase 4)

	// Final canvas. 0 = as produced by the op stack. Fit says how to reach
	// Width x Height from the op-stack result: "contain" (default: scale to
	// fit, keep aspect, pad transparent), "cover" (scale to cover, center-crop),
	// "exact" (stretch).
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Fit    string `json:"fit,omitempty"`

	FPS float64 `json:"fps,omitempty"` // 0 = source fps (capped/snapped per format)

	// Quality knobs. Which apply depends on Format.
	Quality        int    `json:"quality,omitempty"`        // webp/avif 1..100 (0 = 80)
	Lossless       bool   `json:"lossless,omitempty"`       // webp
	Lossy          int    `json:"lossy,omitempty"`          // gif: gifsicle --lossy N, 0 = off, typical 20..200
	Colors         int    `json:"colors,omitempty"`         // gif/apng palette size 2..256 (0 = 256)
	Dither         string `json:"dither,omitempty"`         // gif: "bayer" (default), "sierra2_4a", "floyd_steinberg", "none"
	AlphaThreshold int    `json:"alphaThreshold,omitempty"` // gif: 1..255 (0 = 128); pixels with alpha below become transparent
	Matte          string `json:"matte,omitempty"`          // gif: hex RRGGBB blended under semi-transparent pixels (0 = "313338", Discord dark)
	Loop           int    `json:"loop,omitempty"`           // 0 = loop forever, N = play N+1 times (gif semantics)

	// Fit-to-size (Phase 2). FitBytes > 0 runs the ladder + secant search of
	// DESIGN.md §5.4 so the primary file is <= FitBytes (1-2 % margin is
	// applied by the engine); the other knobs above are the starting point.
	// FitKeepSize forbids the downscale rungs, FitKeepFPS the fps rungs
	// ("compress to X KiB" without changing the look). For Discord targets
	// the engine also decides the format rung order (sticker: indexed APNG →
	// GIF) unless Format is set explicitly by the user.
	FitBytes    int64 `json:"fitBytes,omitempty"`
	FitKeepSize bool  `json:"fitKeepSize,omitempty"`
	FitKeepFPS  bool  `json:"fitKeepFps,omitempty"`

	// FrameFormat applies to FormatFrames: "png" (default, RGBA), "jpeg"
	// (flattened onto Matte, Quality), "webp" (lossless).
	FrameFormat string `json:"frameFormat,omitempty"`

	// Preset is informational for the UI ("emote", "sticker", "chat-gif",
	// "chat" (formerly chat-gif/chat-webp/chat-avif), "optimize", "frames",
	// "custom"); "optimize" additionally selects the no-decode GIF→GIF
	// pipeline in jobs. Target selects which Discord rules and byte limit the
	// linter enforces: "emote" | "sticker" | "attachment" (free, 20 MB) |
	// "attachment-50" (Nitro Basic / Level-2 boosted server) | "attachment-100"
	// (Level-3 boosted server) | "attachment-500" (Nitro) | "" (none). The
	// attachment tiers share every rule and differ only in the byte cap
	// (discordlint.IsAttachment / Limit).
	Preset string `json:"preset,omitempty"`
	Target string `json:"target,omitempty"`
}

// Recipe is the unit of work: sources + ops + output.
type Recipe struct {
	Version int      `json:"v"`       // schema version, currently 1
	Sources []string `json:"sources"` // blob hashes; index 0 is the main source
	Ops     []Op     `json:"ops"`
	Output  Output   `json:"output"`
}

// CurrentVersion is the recipe schema version written by this build.
const CurrentVersion = 1

// Validate checks structural sanity (not op semantics — package graph does
// that when compiling).
func (r Recipe) Validate() error {
	if len(r.Sources) == 0 {
		return fmt.Errorf("recipe: no sources")
	}
	for i, s := range r.Sources {
		if !IsHash(s) {
			return fmt.Errorf("recipe: source %d is not a sha256 hex hash", i)
		}
	}
	if r.Output.Format == "" {
		return fmt.Errorf("recipe: output.format is required")
	}
	for i, op := range r.Ops {
		if op.Kind == "" {
			return fmt.Errorf("recipe: op %d has no kind", i)
		}
	}
	return nil
}

// Canonical returns a canonical JSON encoding: version forced to
// CurrentVersion, op params re-marshalled with sorted keys and no whitespace,
// so equivalent recipes hash identically regardless of client formatting.
func (r Recipe) Canonical() ([]byte, error) {
	c := r
	c.Version = CurrentVersion
	c.Ops = make([]Op, len(r.Ops))
	for i, op := range r.Ops {
		c.Ops[i].Kind = op.Kind
		if len(bytes.TrimSpace(op.Params)) == 0 || bytes.Equal(bytes.TrimSpace(op.Params), []byte("null")) {
			c.Ops[i].Params = nil
			continue
		}
		var v any
		if err := json.Unmarshal(op.Params, &v); err != nil {
			return nil, fmt.Errorf("recipe: op %d (%s) params: %w", i, op.Kind, err)
		}
		b, err := json.Marshal(v) // encoding/json sorts map keys
		if err != nil {
			return nil, err
		}
		c.Ops[i].Params = b
	}
	if c.Ops == nil {
		c.Ops = []Op{}
	}
	return json.Marshal(c)
}

// Hash returns the sha256 hex of Canonical(). It panics only if the recipe
// contains params that are not valid JSON (Validate/Canonical should be
// called first by API code).
func (r Recipe) Hash() string {
	b, err := r.Canonical()
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// IsHash reports whether s looks like a lowercase sha256 hex digest.
func IsHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// NormalizeHex returns a lowercase RRGGBB (or RRGGBBAA) hex colour without a
// leading '#', or an error.
func NormalizeHex(s string) (string, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	s = strings.ToLower(s)
	if len(s) != 6 && len(s) != 8 {
		return "", fmt.Errorf("colour %q: want RRGGBB or RRGGBBAA", s)
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", fmt.Errorf("colour %q: not hex", s)
		}
	}
	return s, nil
}
