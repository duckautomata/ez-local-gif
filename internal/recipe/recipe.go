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

// Op kinds added in Phase 3 (editing ops, DESIGN.md §4.3). Keying ops run at
// full resolution before any scaling; reverse runs after the geometry;
// text/overlay ops run on the FINAL output canvas (after Output.Width/Height
// fit), so their coordinates are in output pixels — what the preview shows.
const (
	OpChromaKey = "chromakey" // ChromaKeyParams — greenscreen/bluescreen keying in YUV 4:4:4 + despill
	OpColorKey  = "colorkey"  // ColorKeyParams — make one RGB colour (eyedropper) transparent
	OpReverse   = "reverse"   // no params — play backwards (after the geometry stages; trim applies in source time)
	OpBounce    = "bounce"    // Phase 4, no params — forward then backward ("ping-pong"): frames and duration double; emitted after the output fit like reverse; stills/proxies of a bounced plan are never seeked
	OpAutoCrop  = "autocrop"  // AutoCropParams — crop to the content bounding box (resolved by jobs before compiling)
	OpText      = "text"      // TextParams — drawtext overlay
	OpOverlay   = "overlay"   // OverlayParams — image / animated image / video overlay from Recipe.Sources[Source]
)

// OpFeather (Phase 3 review) softens the frame's alpha edge (FeatherParams).
// Like the keying ops it runs at full resolution: the compiler hoists it into
// the keying group, right after the keys, before any geometry.
const OpFeather = "feather"

// FeatherParams softens the alpha edge with a Gaussian blur of the alpha
// plane only (a "feather" / "soft edge"); the colour planes are untouched.
// Radius is the Gaussian sigma in SOURCE pixels: 0 means the default 3,
// anything else must lie in 0.1..50 (a compile error otherwise). The visible
// soft edge spans roughly 2-3x Radius, which is how the UI labels the knob
// ("Feather — N px (soft edge ≈ 2–3×N)").
//
// The stage is hoisted with the keying ops: it is emitted right after them,
// before any geometry (crop/autocrop/resize/canvas/flip/rotate), wherever it
// sits in the stack, and several feather ops interleave with the keys in
// their stack order. Because it precedes the geometry, the radius scales
// with the image — a 3 px feather on a 720 px source is ~0.5 px after a
// 128 px emote fit. On frames that carry no alpha at that point (an opaque
// source with no key in front of it in the stack) the stage is skipped
// entirely: blurring a constant opaque plane changes nothing and would waste
// two format conversions. Its params are validated either way.
type FeatherParams struct {
	Radius float64 `json:"radius,omitempty"`
}

// ChromaKeyParams keys out a colour in YUV (soft edges). Zero values:
// Color "00ff00", Similarity 0.2 (0.01..1), Blend 0.05 (0..1), Despill
// on with Mix 0.6 and Expand 0.3 (DespillOff disables it).
//
// Blend 0 therefore means the DEFAULT 0.05, not "no blend": a hard key edge
// needs a small positive value such as 0.001 (the UI's slider floors at
// 0.01 for the same reason). Despill applies only when Color has a single
// dominant channel (green or blue; the despill type follows that channel);
// for any other colour it is skipped even when DespillMix/DespillExpand are
// set. On frames that already carry alpha the key's matte is intersected
// with the incoming alpha (the compiler wraps the key), never substituted
// for it.
type ChromaKeyParams struct {
	Color         string  `json:"color,omitempty"`
	Similarity    float64 `json:"similarity,omitempty"`
	Blend         float64 `json:"blend,omitempty"`
	DespillOff    bool    `json:"despillOff,omitempty"`
	DespillMix    float64 `json:"despillMix,omitempty"`
	DespillExpand float64 `json:"despillExpand,omitempty"`
}

// ColorKeyParams makes one RGB colour transparent (the eyedropper picks it).
// Zero values: Similarity 0.1 (0.01..1), Blend 0 (0..1). Color is required.
type ColorKeyParams struct {
	Color      string  `json:"color"`
	Similarity float64 `json:"similarity,omitempty"`
	Blend      float64 `json:"blend,omitempty"`
}

// AutoCropParams crops to the union bounding box of the content over the
// (trimmed) clip: alpha >= Threshold counts as content for sources with
// alpha, otherwise non-border (non-black/flat) pixels via cropdetect.
// Padding adds pixels on every side (clamped to the frame). Resolved is
// filled by jobs (it runs the detection pass; the graph compiler refuses an
// unresolved autocrop) and is ignored when supplied by a client.
type AutoCropParams struct {
	Threshold int         `json:"threshold,omitempty"` // 1..255 (0 = 1)
	Padding   int         `json:"padding,omitempty"`   // px (0..1024)
	Resolved  *CropParams `json:"resolved,omitempty"`
}

// Anchor names for text/overlay placement: two letters, vertical then
// horizontal — "tl" (default), "tc", "tr", "ml", "mc", "mr", "bl", "bc", "br".
// X/Y locate the anchor point of the element on the output canvas.
const (
	AnchorTopLeft      = "tl"
	AnchorTopCenter    = "tc"
	AnchorTopRight     = "tr"
	AnchorMiddleLeft   = "ml"
	AnchorMiddleCenter = "mc"
	AnchorMiddleRight  = "mr"
	AnchorBottomLeft   = "bl"
	AnchorBottomCenter = "bc"
	AnchorBottomRight  = "br"
)

// TextParams draws text. Zero values: Font "DejaVu Sans" (a fontconfig
// family name; letters, digits, spaces and hyphens only), Size 32 px, Color
// "ffffff", Border 0, BorderColor "000000", Box off with BoxColor
// "00000080" and BoxPad 8, Anchor "tl", X/Y 0, Start/End 0 = whole clip
// (output seconds; the window is [Start, End) — the frame at exactly End is
// not drawn, like trim; End 0 = to the end), LineSpacing 0. Text may
// contain newlines; it is passed to ffmpeg through a text file, never
// escaped inline.
type TextParams struct {
	Text        string  `json:"text"`
	Font        string  `json:"font,omitempty"`
	Size        int     `json:"size,omitempty"`
	Color       string  `json:"color,omitempty"` // RRGGBB or RRGGBBAA
	Border      int     `json:"border,omitempty"`
	BorderColor string  `json:"borderColor,omitempty"`
	Box         bool    `json:"box,omitempty"`
	BoxColor    string  `json:"boxColor,omitempty"`
	BoxPad      int     `json:"boxPad,omitempty"`
	X           int     `json:"x"`
	Y           int     `json:"y"`
	Anchor      string  `json:"anchor,omitempty"`
	Start       float64 `json:"start,omitempty"`
	End         float64 `json:"end,omitempty"`
	LineSpacing int     `json:"lineSpacing,omitempty"`
}

// OverlayParams composites Recipe.Sources[Source] (index >= 1; a still
// image, an animated image or a video, with alpha if it has one) onto the
// output canvas. Width/Height 0 = natural size (one of them 0 keeps the
// aspect); Opacity 0 = 1; Loop (default true for animated sources) repeats
// the overlay until the base ends, otherwise it holds its last frame;
// Start/End as for text; Anchor/X/Y as for text.
//
// Looping (NoLoop false) depends on the asset's container: GIF and video
// assets are repeated (-stream_loop -1) until the base ends; animated WebP
// and APNG assets are read with -ignore_loop 0 and therefore obey the
// asset's OWN loop count — a loop-forever file repeats until the base ends,
// a play-once / play-N-times file plays N times and then holds its last
// frame. With NoLoop every asset plays once and holds its last frame; a
// still image is shown for the whole window either way.
type OverlayParams struct {
	Source  int     `json:"source"`
	X       int     `json:"x"`
	Y       int     `json:"y"`
	Width   int     `json:"width,omitempty"`
	Height  int     `json:"height,omitempty"`
	Opacity float64 `json:"opacity,omitempty"`
	NoLoop  bool    `json:"noLoop,omitempty"`
	Anchor  string  `json:"anchor,omitempty"`
	Start   float64 `json:"start,omitempty"`
	End     float64 `json:"end,omitempty"`
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
	FormatMP4    = "mp4"    // Phase 4, opaque video: H.264 yuv420p, CRF quality/fit knob, +faststart, flattened onto Matte, even dims
	FormatWebM   = "webm"   // Phase 4, opaque video: VP9 yuv420p, CRF quality/fit knob, flattened onto Matte, even dims
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
