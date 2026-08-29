// Package graph compiles a recipe's op stack into a single ffmpeg
// filter_complex graph (the "P0 universal decode" graph in docs/DESIGN.md
// §4.1). It is pure: no ffmpeg is executed here, only argv/filter text is
// produced, which makes it golden-file testable.
//
// Contract:
//   - The main input is ffmpeg input 0 ("[0:v]"). Plan.InputArgs are placed
//     immediately before "-i <main>" (e.g. -ss/-to for trim, -c:v libvpx-vp9
//     for VP9-alpha WebM, -ignore_loop 0 for gif inputs is NOT needed for the
//     main input).
//   - Plan.Filter is a complete filter_complex string ending in Plan.OutLabel
//     ("[out]") whose frames are format=rgba, at Plan.Width x Plan.Height,
//     Plan.FPS frames per second (constant frame rate). It is a single chain
//     from "[0:v]" except for sources with a separate alpha stream
//     (recipe.ProbeInfo.AlphaStream > 0, e.g. AVIF): those start with
//     "[0:v:0]format=rgba[c];[0:v:N]format=gray[a];[c][a]alphamerge," and
//     the chain follows; a key applied to frames that already carry alpha
//     (keyKeepingAlpha) and every overlay (below) add chains of their own.
//     The last chain always ends in "[out]", so consumers that append to
//     the filter (";[out]…") work either way.
//   - Image sequences (recipe.KindSequence, ProbeInfo.Sequence set) are read
//     by the image2 demuxer: InputArgs carry "-f image2 -framerate F
//     -start_number 1 -reinit_filter 0" (the demuxer is forced explicitly so
//     the render opens the frames exactly like the probe, independent of the
//     pattern's extension; F = 1000/delay ms, the hoisted "delay" op overriding
//     SequenceInfo.DelayMS, default 100 ms; see SequenceFPS), Plan.InputPattern
//     is the file pattern and Plan.SourceFPS is F. Every sequence chain starts
//     with a guarding scale-to-fit head (a pixel-exact pass-through for
//     uniform sequences) because frames may differ per frame in pixel format
//     or size, which would otherwise rebuild the filtergraph and lose frames;
//     mixed-size sequences add the normalising tail (format=rgba, transparent
//     pad to the largest frame, pad=…:eval=frame). See sequenceHead. The
//     frames are then CFR at F and everything below applies unchanged.
//   - Stage order in the filter text: the alpha head (the hoisted
//     unpremultiply at native depth, preceded by setparams=alpha_mode=
//     premultiplied so FFmpeg >= 8's alpha_mode negotiation does not
//     auto-insert a cancelling premultiply; right after the alpha merge /
//     mixed head when there is one. Sources that decode to an RGB format get
//     "format=gbrap|gbrap10le|gbrap12le,setparams=…,unpremultiply=inplace=1"
//     when the op is present; sources that decode to planar YUV with alpha —
//     pix_fmt yuva…: ProRes 4444, lossy WebP, VP8/VP9 alpha — get
//     "[setparams=…,unpremultiply=inplace=1,]format=rgba", unpremultiplied
//     natively and converted to rgba exactly once, never through gbrap: their
//     tv-range alpha plane only survives the direct yuva→rgba conversion
//     exactly, see compiler.alphaHead) → trim (only for the webp_anim
//     demuxer, which decodes nothing after an input seek: "trim=start=S
//     [:end=E],setpts=PTS-STARTPTS" and Plan.FilterTrim; such a source's
//     plan carries Plan.SeekUnsafe with or without a trim, so the preview
//     renderers never seek it; every other source is trimmed with -ss/-to
//     input args, below)
//     → speed (setpts) → fps (always present, so the output is CFR; emitted
//     as "fps=F:round=down" so the frame count is floor(Duration*F) and an
//     fps drop never lengthens the clip past the trimmed source) →
//     keying (Phase 3: chromakey/colorkey + despill, at full resolution,
//     before any crop or scale; on frames that already carry alpha — source
//     alpha, a merged alpha stream, transparent sequence padding, an earlier
//     key — the key is wrapped so the incoming alpha is intersected with the
//     key's matte instead of overwritten, which makes the filter a
//     multi-chain graph "…,split[k1][k1m];[k1m]alphaextract[k1a0];[k1]<key>,
//     split[k1k][k1km];[k1km]alphaextract[k1a1];[k1a0][k1a1]blend=
//     all_mode=multiply[k1a];[k1k][k1a]alphamerge,…", see keyKeepingAlpha;
//     feather ops are hoisted into this same group and interleave with the
//     keys in their stack order — each emits "format=gbrap,gblur=sigma=R:
//     planes=8,format=rgba" (gbrap orders the planes G,B,R,A, so planes=8
//     blurs only the alpha plane; the 8-bit gbrap↔rgba conversions are
//     lossless plane repacks), R being the Gaussian sigma in SOURCE pixels —
//     the stage precedes the geometry, so the softness scales with the image
//     (a 3 px feather on a 720 px source is ~0.5 px after a 128 px emote
//     fit) — and the stage is skipped entirely while the frame carries no
//     alpha at that point (blurring a constant opaque plane is a no-op that
//     would waste two conversions; see compiler.feather))
//     → the geometry ops in the order given (crop — including a resolved
//     autocrop —, premultiplied lanczos scale, canvas pad, flip/rotate; each
//     sees the frame size produced by the previous one) → output fit
//     (Output.Width/Height/Fit) → reverse (Phase 3: ffmpeg's reverse filter,
//     emitted as "format=rgba,reverse" after the output fit, so the filter —
//     which holds every frame until EOF — buffers frames at the output size
//     and at 4 B/px, which MaxMasterBytes (and jobs' master estimate)
//     bounds) → the final-canvas ops (Phase 3: text and overlays in the
//     order given, on the output canvas, which is forced to rgba first) →
//     format=rgba. Apart from the webp_anim case above, trim never becomes a
//     filter: it is expressed as -ss/-to input seek args (source time,
//     microsecond precision so a bound taken from the frame grid keeps its
//     frame — see compiler.trim). Temporal and spatial filters commute, so
//     hoisting setpts/fps in front of the geometry ops does not change the
//     result but keeps the frame count low before scaling.
//   - Overlays (Phase 3) make the filter a multi-chain graph: the chain so
//     far is closed with a base label "[bN]", the overlay source gets its
//     own input chain "[k:v]…[ovN]" (k = its ExtraInputs position + 1;
//     "-i" args per ExtraInput.Args, see there) and a composite chain
//     "[bN][ovN]overlay=x=…:y=…:format=auto:shortest=1:eof_action=repeat…"
//     continues; the last chain still ends in "[out]", and consumers that
//     append ";[out]…" keep working. shortest=1 makes the BASE decide the
//     output length, so every finite overlay input is padded with
//     "tpad=stop_mode=clone:stop=-1" (its last frame holds; see
//     overlayHold). The overlay's own timeline starts at output t=0 and
//     timed ops are gated with enable='gte(t+0.0001,S)*lt(t+0.0001,E)' /
//     'gte(t+0.0001,S)' in output seconds.
//   - Time windows (Phase 3: recipe.TextParams / OverlayParams Start and
//     End) are half-open, [Start, End) in output seconds, exactly like a
//     trim: the op is drawn on every frame whose timestamp t satisfies
//     Start <= t < End, so the frame at exactly End is NOT drawn — the UI's
//     "from scrubber" End is the start of the next frame, and adjacent
//     windows [A,B) / [B,C) never overlap. End 0 (or omitted) means to the
//     end of the clip ('gte(t+0.0001,S)'); Start 0 and End 0 mean the whole
//     clip (no enable option). ffmpeg's between() is inclusive on both ends
//     and is therefore not used. The 0.0001 s tolerance absorbs the
//     microsecond rounding of the bounds (a frame-grid start such as 2/30 →
//     0.066667 lies just above its frame's raw timestamp 0.0666666…, which a
//     plain gte would skip) and is far below any frame period; see
//     enableExpr / enableTolerance.
//   - Text ops (Phase 3) emit drawtext with textfile=<placeholder>
//     (Plan.TextFiles): jobs writes the bodies to files and calls
//     BindTextFiles, which escapes the paths for both filter parsing levels.
//     The font is a fontconfig family name ("font=DejaVu Sans"; spaces need
//     no quoting inside the option value), the text is printed verbatim
//     (expansion=none). drawtext runs in place on the canvas only while
//     every element it draws (glyphs, border, box) is opaque; a text op with
//     a translucent RRGGBBAA element (the default box colour 00000080
//     included) is drawn on transparent "color=…" layers — one per group of
//     adjacent elements sharing an alpha, in the order box → border → glyphs,
//     each "…,drawtext=…,format=gbrap,setparams=alpha_mode=premultiplied,
//     unpremultiply=inplace=1,format=rgba[,colorchannelmixer=aa=A][tK]" (the
//     layer's anti-aliased edges come out of drawtext premultiplied, see
//     textLayerTail) — that are composited like image overlays
//     ("[bN][tK]overlay=format=auto:shortest=1:eof_action=repeat[:enable=…]"),
//     because drawtext on an rgba canvas blends its alpha into the alpha
//     plane too (see compiler.textLayers). Every layer of an op repeats the
//     op's single textfile placeholder.
//   - Preview mapping (Phase 3): for Plan.Reversed plans output time t
//     corresponds to source time TrimStart + (Duration - t)*Speed instead of
//     TrimStart + t*Speed; the still/proxy seek logic in enc implements
//     both. Reverse never changes Frames or Duration.
//   - Output.FPS (or the fps op) is capped with SnapFPS(out.Format, fps): 50
//     for GIF, 60 otherwise; no other snapping (see SnapFPS for why 30 fps
//     GIFs need none). Every recipe.Format* compiles to the same RGBA master
//     (the static formats and frame exports take what they need from it);
//     Output.FrameFormat must be "", png, jpeg or webp and Output.FitBytes
//     >= 0.
//   - Sizes are bounded: resize/canvas/Output dimensions and every resulting
//     frame must be <= 8192 px per side and <= 32 megapixels, the speed factor
//     must lie in [0.05, 100] and the expected RGBA master (W*H*4*Frames, when
//     the frame count is known) must fit in 8 GiB; Compile rejects anything
//     larger with a descriptive error.
//   - Detection plans (Phase 3, CompileDetect): the autocrop op's content
//     box must be found on the frames the crop will apply to — the source
//     frame after keying, before any geometry. CompileDetect compiles only
//     the stages in front of the geometry (source head, unpremultiply,
//     trim/speed/fps, keying, feather) into a plan with the same contract (InputArgs,
//     a Filter ending in "[out]" at format=rgba, Width x Height = the source
//     frame) and no ExtraInputs/TextFiles; jobs appends its sampling and
//     bbox/cropdetect stages after "[out]". Its frames are never materialised,
//     so the size limits above do not apply to it.
package graph

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Plan is the compiled graph plus the facts the encoders need.
type Plan struct {
	InputArgs []string // args placed before "-i <main input>"
	Filter    string   // filter_complex text producing OutLabel
	OutLabel  string   // always "[out]"

	Width, Height int     // output frame size
	FPS           float64 // output frame rate (constant); > 0
	HasAlpha      bool    // whether the output carries (non-trivial) alpha
	Duration      float64 // expected output duration in seconds (0 = unknown)
	// Frames is the expected master frame count (0 = unknown). For image
	// sequences it is exact: the frames the trim selects on the image2 grid
	// (all of them without a trim), through the speed and fps stages the way
	// ffmpeg ends the stream (see sequenceFrames) — so a sequence that is not
	// retimed plans exactly its frame count. For every other source it is
	// the floor model of the fps stage's round=down, floor(Duration*FPS +
	// FrameTolerance), which is an estimate: the probed rate may not be the
	// source's exact cadence. The UI mirrors both rules for its scrubber.
	Frames int

	// Facts the still/proxy renderers need to map a preview time to source
	// time: TrimStart/TrimEnd are the source-time bounds selected by the trim
	// op (0,0 = whole source; TrimEnd 0 = to the end); Speed is the speed
	// factor (1 = unchanged). A preview at output time t corresponds to
	// source time TrimStart + t*Speed.
	TrimStart float64
	TrimEnd   float64
	Speed     float64

	// SourceFPS is the probed frame rate of the source (recipe.ProbeInfo.FPS
	// as given, not snapped; 0 = unknown, e.g. stills). The still renderer
	// uses it to decide how far before the wanted frame it must seek so that
	// at least one decodable source frame precedes the target. For image
	// sequences it is the image2 -framerate (1000/delay ms, 3 decimals).
	SourceFPS float64

	// SourceVFR is true for gif/apng/webp/avif animations (recipe.KindAnimation,
	// not image sequences): their frames carry individual delays, SourceFPS is
	// only ffmpeg's base-cadence estimate and a seek can land inside a hold; the
	// still renderer must not select reversed frames by index after a seek.
	SourceVFR bool

	// FilterTrim is true when the trim is a filter stage ("trim=start=S[:end=E],
	// setpts=PTS-STARTPTS", the first temporal stage) instead of -ss/-to input
	// args: FFmpeg 9's webp_anim demuxer decodes nothing after any input seek.
	// TrimStart/TrimEnd/Duration/Frames are set as usual; the still renderer
	// must not seek such a plan at all (the stage cuts and rebases the clock
	// exactly like an input seek). FilterTrim implies SeekUnsafe.
	FilterTrim bool

	// SeekUnsafe is true when the main source's demuxer decodes nothing after
	// an input seek (seekUnsafeDemuxer: FFmpeg 9's webp_anim — every animated
	// WebP main source, trimmed or not; never an image sequence, whose image2
	// input seeks fine). Such a plan is never seeked by the still/proxy
	// renderers (no -ss/-itsoffset/-to, not even for the reversed proxy's
	// tail): they decode from the file's start and pick the frame by output
	// time, as for a FilterTrim plan. A trim on a SeekUnsafe source is always
	// a filter stage (FilterTrim), so its InputArgs carry no seek either way.
	SeekUnsafe bool

	// InputPattern (Phase 2) is set for image-sequence sources: the image2
	// demuxer pattern relative to the blob directory (e.g. "%06d.png", from
	// recipe.SequenceInfo.Pattern). enc.MasterArgs/StillArgs/ProxyArgs then
	// pass "-i <blobDir>/<pattern>" instead of "-i <blobPath>", and InputArgs
	// carry "-f image2 -framerate F -start_number 1 -reinit_filter 0"
	// (-f image2 so the open never depends on the pattern's extension;
	// F = 1000/delayMs; the hoisted "delay" op overrides
	// SequenceInfo.DelayMS; -reinit_filter 0 because frames may differ per
	// frame in pixel format or size, see sequenceHead). Empty for other
	// sources.
	InputPattern string

	// Phase 3.

	// ExtraInputs are the overlay sources, in the order their "-i" must be
	// emitted after the main input (ffmpeg input index = position + 1). The
	// compiler fills Source/Args/Animated/Duration/Loop; jobs fills Path from
	// the store before handing the plan to enc.
	ExtraInputs []ExtraInput
	// Reversed is true when a reverse op is in the chain: output time t
	// corresponds to source time TrimStart + (Duration - t)*Speed, which the
	// still/proxy seek logic must honour.
	Reversed bool
	// TextFiles lists every text op's body in filter order. The compiler
	// emits "textfile=<Placeholder>" (a token of letters, digits and
	// underscores; once per drawtext stage the op compiles to — a translucent
	// text op draws the same body on several layers) and jobs writes Content
	// to a scratch file and calls BindTextFiles with the paths before
	// encoding; enc never sees a placeholder.
	TextFiles []TextFile
}

// ExtraInput is one overlay source. One entry per distinct source, in order
// of first use; two overlay ops of the same source share it (and must agree
// on Loop).
type ExtraInput struct {
	Source int    // index into recipe.Recipe.Sources (>= 1)
	Path   string // absolute blob path (set by jobs; "" from the compiler)
	// Args are placed before this input's "-i" (all verified on FFmpeg 9.0.1,
	// see overlayArgs): "-loop","1" for still images read by image2 /
	// *_pipe demuxers (a still from any other demuxer — a one-frame
	// gif/apng/webp, an AVIF — gets no args, those have no loop option, and
	// is held by the chain's tpad);
	// "-stream_loop","-1" for looping gif and video overlays;
	// "-ignore_loop","0" for looping apng / animated webp (-stream_loop -1
	// hangs their demuxers; the file's own loop count is obeyed); nothing for
	// a non-looping animation (plays once, then its last frame holds);
	// "-c:v","libvpx-vp9"/"libvpx" first for VP9/VP8 alpha sources.
	Args     []string
	Animated bool    // more than one frame
	Duration float64 // the overlay's own duration in seconds (0 = unknown/still)
	Loop     bool    // repeats until the base ends (else holds the last frame)
}

// TextFile is one drawtext body the compiler deferred to a file.
type TextFile struct {
	Placeholder string // token embedded in Plan.Filter as textfile=<Placeholder>
	Content     string // the text (may contain newlines)
}

// CompileWithSources is Compile for recipes with overlay sources: srcs[0] is
// the main source, srcs[i] the probe info of recipe.Sources[i] (needed for
// overlay sizing, alpha, animation and duration). Compile(src, ops, out) is
// CompileWithSources([]ProbeInfo{src}, ops, out); an overlay op that
// references a missing source index (or source 0, the main source) is an
// error, as is an image-sequence overlay source.
//
// It validates op params (unknown kinds, out-of-range crops, non-positive
// sizes, colours, fonts, time ranges) and the upper bounds (MaxDim,
// MaxPixels, MinSpeed/MaxSpeed, MaxMasterBytes) and returns descriptive
// errors suitable for showing in the UI.
func CompileWithSources(srcs []recipe.ProbeInfo, ops []recipe.Op, out recipe.Output) (*Plan, error) {
	if len(srcs) == 0 {
		return nil, errorf("no sources")
	}
	src := srcs[0]
	if src.Width <= 0 || src.Height <= 0 {
		return nil, errorf("source has no usable frame size (%dx%d)", src.Width, src.Height)
	}
	if err := validateOutput(out); err != nil {
		return nil, err
	}
	decoded, err := decodeOps(ops)
	if err != nil {
		return nil, err
	}
	c := newCompiler(srcs, out)
	if err := c.source(decoded); err != nil {
		return nil, err
	}
	if err := c.temporal(decoded); err != nil {
		return nil, err
	}
	if err := c.keying(decoded); err != nil {
		return nil, err
	}
	if err := c.geometry(decoded); err != nil {
		return nil, err
	}
	if err := c.outputFit(); err != nil {
		return nil, err
	}
	c.reverse(decoded)
	if err := c.finalCanvas(decoded); err != nil {
		return nil, err
	}
	return c.finish()
}

// detectOps are the op kinds CompileDetect applies: everything the stage
// order puts in front of the geometry (the source head's delay, the alpha
// head's hoisted unpremultiply, the temporal stages, the keying and the
// feather — a feather changes which alpha exceeds the autocrop threshold, so
// the detection must see it exactly like the render does).
var detectOps = map[string]bool{
	recipe.OpDelay: true, recipe.OpUnpremultiply: true,
	recipe.OpTrim: true, recipe.OpSpeed: true, recipe.OpFPS: true,
	recipe.OpChromaKey: true, recipe.OpColorKey: true, recipe.OpFeather: true,
}

// CompileDetect compiles the detection plan of the autocrop op: only the
// stages that precede the geometry — the source head (image sequence /
// separate alpha stream), the hoisted unpremultiply, trim / speed / fps and
// the keying and feather ops — so the frames it yields are in SOURCE
// coordinates, keyed and feathered exactly as the render keys them (both run
// before any crop or scale, see CompileWithSources; a feather changes which
// alpha exceeds the autocrop threshold). jobs runs it, with its own sampling
// and bbox/cropdetect stages appended after "[out]", to find the content box
// of a keyed clip; the box then becomes the op's Resolved crop.
//
// ops are the ops in front of the autocrop op: those of the kinds in
// detectOps (delay, unpremultiply, trim, speed, fps, chromakey, colorkey,
// feather) are applied in the usual stage order and validated like Compile does
// (errors name the op by its index in ops); every other kind — geometry,
// reverse, text, overlay, an autocrop itself, even an unknown kind — is
// ignored, params unread. srcs is the recipe's source list (only srcs[0],
// the main source, is read; the overlay sources are irrelevant without
// overlay ops). There is no Output: the fps is the fps op's, else the
// source's, capped at MaxFPS.
//
// The plan follows the Compile contract — InputArgs / InputPattern for the
// main input, Filter ending in "[out]" at format=rgba, constant Plan.FPS,
// TrimStart/TrimEnd/Speed/SourceFPS, Duration/Frames — with Width x Height
// the source frame (the normalised canvas for a mixed-size sequence),
// Plan.HasAlpha reporting whether those frames carry alpha (source alpha or
// keying: that decides whether the box is read off the alpha plane), and
// empty ExtraInputs/TextFiles, Reversed false. The size limits of Compile
// (MaxDim, MaxPixels, MaxMasterBytes) are not applied: the detection streams
// the frames through -f null and never materialises a master, and a source
// that only a later resize shrinks must still be croppable to its content.
func CompileDetect(srcs []recipe.ProbeInfo, ops []recipe.Op) (*Plan, error) {
	if len(srcs) == 0 {
		return nil, errorf("no sources")
	}
	src := srcs[0]
	if src.Width <= 0 || src.Height <= 0 {
		return nil, errorf("source has no usable frame size (%dx%d)", src.Width, src.Height)
	}
	var decoded []decodedOp
	for i, op := range ops {
		if !detectOps[op.Kind] {
			continue
		}
		d, err := decodeOp(i, op)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, d)
	}
	c := newCompiler(srcs, recipe.Output{})
	if err := c.source(decoded); err != nil {
		return nil, err
	}
	if err := c.temporal(decoded); err != nil {
		return nil, err
	}
	if err := c.keying(decoded); err != nil {
		return nil, err
	}
	return c.assemble()
}

// BindTextFiles returns a copy of p whose Filter has every TextFile
// placeholder replaced by the matching path, escaped for filter syntax.
// paths must have exactly len(p.TextFiles) entries and every placeholder
// must occur in p.Filter (as "textfile=<Placeholder>"; every occurrence is
// replaced — a translucent text op repeats its placeholder on each of its
// layers); the copy's TextFiles is nil and its slices are not shared with p.
// p is not modified.
//
// A filter option value is un-escaped twice by ffmpeg — once by the
// filtergraph parser (which treats the backslash, the single quote, '[',
// ']', ',' and ';' as special) and once by the filter's own option parser
// (where ':', the backslash and the single quote are special) — so the path
// is escaped at both levels: first the backslash, ':' and the single quote
// get a backslash, then the result's backslashes, quotes and graph
// characters get another one. "C:\a\t.txt" becomes C\\:\\\\a\\\\t.txt;
// verified against FFmpeg 9 (a single level loses the quote on Linux and
// breaks on the colon of a Windows path).
func BindTextFiles(p *Plan, paths []string) (*Plan, error) {
	if p == nil {
		return nil, errorf("BindTextFiles: nil plan")
	}
	if len(paths) != len(p.TextFiles) {
		return nil, errorf("BindTextFiles: %d path(s) for %d text file(s)", len(paths), len(p.TextFiles))
	}
	q := *p
	q.InputArgs = slices.Clone(p.InputArgs)
	q.ExtraInputs = slices.Clone(p.ExtraInputs)
	for i := range q.ExtraInputs {
		q.ExtraInputs[i].Args = slices.Clone(p.ExtraInputs[i].Args)
	}
	q.TextFiles = nil
	for i, tf := range p.TextFiles {
		if paths[i] == "" {
			return nil, errorf("BindTextFiles: text file %d has an empty path", i+1)
		}
		token := "textfile=" + tf.Placeholder
		if tf.Placeholder == "" || !strings.Contains(q.Filter, token) {
			return nil, errorf("BindTextFiles: placeholder %q of text file %d is not in the filter", tf.Placeholder, i+1)
		}
		q.Filter = strings.ReplaceAll(q.Filter, token, "textfile="+EscapeFilterPath(paths[i]))
	}
	return &q, nil
}

// EscapeFilterPath escapes a file path for use as a filter option value
// inside a filter_complex string (both escaping levels, see BindTextFiles).
func EscapeFilterPath(path string) string {
	return escapeFilterValue(escapeFilterValue(path, `\:'`), `\'[],;`)
}

// escapeFilterValue prefixes every rune of s that occurs in special with a
// backslash.
func escapeFilterValue(s, special string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if strings.ContainsRune(special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ErrNotImplemented is kept for API compatibility with the Phase-1 stubs.
// Nothing in this package returns it any more.
var ErrNotImplemented = errors.New("graph: not implemented")

// outLabel is the output pad label of every compiled graph.
const outLabel = "[out]"

// Compile builds the Plan for a single-source recipe: src + ops + out. It is
// CompileWithSources([]recipe.ProbeInfo{src}, ops, out); see there for what
// is validated.
func Compile(src recipe.ProbeInfo, ops []recipe.Op, out recipe.Output) (*Plan, error) {
	return CompileWithSources([]recipe.ProbeInfo{src}, ops, out)
}

// Frame-rate caps applied by SnapFPS.
const (
	// MaxGIFFPS is the GIF cap. GIF delays are whole centiseconds and
	// browsers clamp delays <= 1 cs to 10 cs, so every delay must be >= 2 cs
	// (DESIGN.md §5.3). ffmpeg's gif muxer runs at a 1/100 s timebase and
	// rounds each frame's pts to the nearest centisecond, so consecutive
	// delays are floor(100/fps) or ceil(100/fps): at <= 50 fps every delay is
	// >= 2 cs (this cap is what protects the >= 2 cs rule), while 60 fps would
	// yield 2,1,2 cs delays. Nothing else needs snapping: a 30 fps master
	// gets 3,4,3 cs delays with an exact total (Bresenham for free) and no
	// frame is dropped or duplicated — an earlier draft snapped to 100/n rates
	// (30 → 33.333), which duplicated 1 in 9 frames. Verified against ffmpeg
	// in gif_timing_ffmpeg_test.go.
	MaxGIFFPS = 50
	// MaxFPS is the cap for every other animated format (Discord stickers
	// allow at most 60 fps; nothing above it survives a browser anyway).
	MaxFPS = 60
)

// SnapFPS returns the frame rate actually used for format: a plain cap at
// MaxGIFFPS (50) for GIF and MaxFPS (60) for everything else, rounded to 3
// decimals so filter text built from it is stable ("fps=29.97"). fps <= 0
// (or NaN) returns 0 (the caller substitutes the source fps first). See
// MaxGIFFPS for why 30 fps GIFs need no further snapping.
func SnapFPS(format string, fps float64) float64 {
	if !(fps > 0) { // also catches NaN
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(format), "gif") {
		return round3(math.Min(fps, MaxGIFFPS))
	}
	return round3(math.Min(fps, MaxFPS))
}

// round3 rounds v to 3 decimals (the precision used for rates in filter
// text).
func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// round6 rounds v to 6 decimals (microseconds, ffmpeg's -ss/-to resolution;
// used for trim bounds, see compiler.trim for why milliseconds lose frames).
func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// fnum renders v for filter/argv text: rounded to 3 decimals, minimal
// digits, no exponent, no trailing zeros, no "-0" (1.5 → "1.5", 2 → "2",
// 33.3333 → "33.333").
func fnum(v float64) string {
	v = round3(v)
	if v == 0 {
		v = 0 // normalise -0
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// fnum6 is fnum at 6 decimals (2/30 → "0.066667", 1.5 → "1.5").
func fnum6(v float64) string {
	v = round6(v)
	if v == 0 {
		v = 0 // normalise -0
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// fexact renders v with full precision (used for values that must not be
// rounded, like the speed factor, so Plan.Speed and the setpts expression
// agree exactly). Never uses scientific notation.
func fexact(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
