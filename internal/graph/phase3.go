package graph

// Phase 3 stages (DESIGN.md §4.3): keying (chromakey / colorkey + despill),
// the resolved autocrop, reverse, and the final-canvas ops — drawtext and
// image / animation / video overlays from the recipe's extra sources. Every
// recipe emitted here was verified pixel-exact against FFmpeg 9.0.1 (host
// and the ezlg-dev image); phase3_ffmpeg_test.go re-checks them whenever an
// ffmpeg is on PATH.

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Defaults of the Phase 3 ops (the recipe.*Params zero values) and their
// bounds.
const (
	// Keying (recipe.ChromaKeyParams / ColorKeyParams).
	defaultKeyColor         = "00ff00"
	defaultChromaSimilarity = 0.2
	defaultChromaBlend      = 0.05
	defaultDespillMix       = 0.6
	defaultDespillExpand    = 0.3
	defaultColorSimilarity  = 0.1
	// MinSimilarity is the smallest accepted key similarity (ffmpeg's own
	// minimum is 1e-5; anything below 0.01 keys nothing in practice).
	MinSimilarity = 0.01

	// Feather (recipe.FeatherParams): the default Gaussian sigma and its
	// bounds, in source pixels.
	defaultFeatherRadius = 3.0
	// MinFeatherRadius and MaxFeatherRadius bound a feather op's radius
	// (below 0.1 the blur is invisible; above 50 the gblur IIR passes cost
	// real time for an edge wider than any Discord asset).
	MinFeatherRadius = 0.1
	MaxFeatherRadius = 50.0
	// MaxAutoCropPadding bounds recipe.AutoCropParams.Padding.
	MaxAutoCropPadding = 1024

	// Text (recipe.TextParams).
	defaultFont        = "DejaVu Sans"
	defaultFontSize    = 32
	defaultTextColor   = "ffffff"
	defaultBorderColor = "000000"
	defaultBoxColor    = "00000080"
	defaultBoxPad      = 8
	// MaxFontSize is the largest drawtext font size (pixels).
	MaxFontSize = MaxDim
	// MaxTextBorder bounds the text outline width.
	MaxTextBorder = 256
	// MaxTextBoxPad bounds the box padding.
	MaxTextBoxPad = 1024
	// MaxLineSpacing bounds |LineSpacing|.
	MaxLineSpacing = 1024
	// MaxFontNameLen bounds the fontconfig family name.
	MaxFontNameLen = 64
	// textPlaceholderFormat is the token embedded as textfile=<token>; jobs
	// replaces it through BindTextFiles.
	textPlaceholderFormat = "__EZLG_TEXT_%d__"
)

// overlayHold is the stage that keeps an overlay alive after its own end:
// the composite runs with shortest=1 so the BASE always decides the output
// length (an overlay longer than the base would otherwise extend it), which
// means a finite overlay must never end first. tpad with stop=-1 clones its
// last frame indefinitely; shortest=1 then cuts the output at the base's
// end. Verified on FFmpeg 9.0.1: without it a 2-frame non-looping overlay
// ended a 6-frame base after 2 frames; with it frames 2..5 hold the last
// overlay frame.
const overlayHold = "tpad=stop_mode=clone:stop=-1"

// ---------------------------------------------------------------------------
// Keying and feather: full resolution, right after the temporal stages,
// before any crop or scale (DESIGN.md §4.1).
// ---------------------------------------------------------------------------

// keying applies every chromakey / colorkey / feather op in the order given
// (the feather ops interleave with the keys per their stack order). Each key
// is emitted bare while the frames are opaque and through keyKeepingAlpha
// once they carry alpha (source alpha, a merged alpha stream, transparent
// sequence padding or an earlier key): ffmpeg's chromakey and colorkey
// OVERWRITE the alpha plane from the colour distance alone, so on their own
// they would turn every formerly transparent pixel whose colour is not the
// key colour opaque (verified on FFmpeg 9.0.1: a (0,0,0,0) pixel of a ProRes
// 4444 / transparent GIF source came out (0,0,0,255), a half-alpha edge
// pixel came out 255, and a colorkey after a chromakey brought the keyed
// screen back).
func (c *compiler) keying(ops []decodedOp) error {
	for _, d := range ops {
		var err error
		switch p := d.params.(type) {
		case *recipe.ChromaKeyParams:
			err = c.chromaKey(d, p)
		case *recipe.ColorKeyParams:
			err = c.colorKey(d, p)
		case *recipe.FeatherParams:
			err = c.feather(d, p)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// feather softens the alpha edge (recipe.FeatherParams): "format=gbrap,
// gblur=sigma=R:planes=8,format=rgba". gbrap orders the planes G,B,R,A, so
// planes=8 (bit 3) blurs only the alpha plane and the colour is untouched;
// the 8-bit gbrap↔rgba conversions on either side are lossless plane repacks
// (no range hazard — unlike the >8-bit gbrap copies alphaHead avoids). R is
// the Gaussian sigma in source pixels, printed like the other float knobs
// (fnum); 0 means defaultFeatherRadius, anything else must lie in
// [MinFeatherRadius, MaxFeatherRadius]. The stage sits with the keying ops,
// before any geometry, so the softness scales with the image (a 3 px feather
// on a 720 px source is ~0.5 px after a 128 px emote fit).
//
// On frames that carry no alpha at this point — an opaque source with no key
// in front of the op in the stack — the stage is skipped entirely: a blur of
// a constant opaque plane is a no-op that would waste the two conversions.
// The params are validated either way.
func (c *compiler) feather(d decodedOp, p *recipe.FeatherParams) error {
	r := p.Radius
	if r == 0 {
		r = defaultFeatherRadius
	}
	if math.IsNaN(r) || math.IsInf(r, 0) || r < MinFeatherRadius || r > MaxFeatherRadius {
		return opErrorf(d, "radius must be between %s and %s px (got %s)", fexact(MinFeatherRadius), fexact(MaxFeatherRadius), fexact(p.Radius))
	}
	if !c.hasAlpha {
		return nil
	}
	c.emit("format=gbrap")
	c.emit("gblur=sigma=" + fnum(r) + ":planes=8")
	c.emit("format=rgba")
	return nil
}

// chromaKey emits "format=yuva444p,chromakey=color=0xRRGGBB:similarity=S:
// blend=B" (4:4:4 so chroma edges are not smeared by subsampling) followed,
// unless DespillOff, by "despill=type=green|blue:mix=M:expand=E" to pull the
// screen colour out of the subject's edge pixels (plus a trailing
// "format=rgba" that only the alpha-keeping wrapper keeps, see key). The
// despill type follows the dominant channel of the key colour; a colour that
// is neither green- nor blue-dominant gets no despill (ffmpeg's despill
// knows only those two screens). despill's "type" only selects how the
// spill map is computed — which channel it is subtracted from is the
// red/green/blue scale options, whose defaults (green=-1) suit a green
// screen only, so the blue variant adds ":green=0:blue=-1". Verified: a
// half-blended (128,128,0) edge pixel of a red square on 0x00ff00 keeps
// alpha 255 and its green drops from 128 to 76 with the defaults; (128,0,128)
// on 0x0000ff drops its blue to 76 with the scales (and is untouched without
// them). On frames that already carry alpha the stages go through
// keyKeepingAlpha (see keying).
func (c *compiler) chromaKey(d decodedOp, p *recipe.ChromaKeyParams) error {
	color, err := keyColor(p.Color, defaultKeyColor)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	sim, err := unitParam("similarity", p.Similarity, defaultChromaSimilarity, MinSimilarity)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	blend, err := unitParam("blend", p.Blend, defaultChromaBlend, 0)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	mix, err := unitParam("despillMix", p.DespillMix, defaultDespillMix, 0)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	expand, err := unitParam("despillExpand", p.DespillExpand, defaultDespillExpand, 0)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	stages := []string{
		"format=yuva444p",
		fmt.Sprintf("chromakey=color=0x%s:similarity=%s:blend=%s", color, fnum(sim), fnum(blend)),
	}
	if !p.DespillOff {
		switch despillType(color) {
		case "green":
			stages = append(stages, fmt.Sprintf("despill=type=green:mix=%s:expand=%s", fnum(mix), fnum(expand)))
		case "blue":
			stages = append(stages, fmt.Sprintf("despill=type=blue:mix=%s:expand=%s:green=0:blue=-1", fnum(mix), fnum(expand)))
		}
	}
	c.key(append(stages, "format=rgba"))
	return nil
}

// colorKey emits "format=rgba,colorkey=color=0xRRGGBB:similarity=S:blend=B"
// (RGB distance; the eyedropper's pick-a-colour). Color is required. On
// frames that already carry alpha the stages go through keyKeepingAlpha
// (see keying).
func (c *compiler) colorKey(d decodedOp, p *recipe.ColorKeyParams) error {
	color, err := keyColor(p.Color, "")
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	sim, err := unitParam("similarity", p.Similarity, defaultColorSimilarity, MinSimilarity)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	blend, err := unitParam("blend", p.Blend, 0, 0)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	c.key([]string{"format=rgba", fmt.Sprintf("colorkey=color=0x%s:similarity=%s:blend=%s", color, fnum(sim), fnum(blend))})
	return nil
}

// key applies one key's stages (its format conversion first, the key
// filter, the optional despill, format=rgba last): inline on opaque frames
// — the bare filter defines the alpha plane then —, through keyKeepingAlpha
// on frames that already carry alpha. Either way the frames carry alpha
// afterwards. Inline, the trailing format=rgba is dropped (it only serves
// the wrapper, whose keyed copy must be rgba for the merge; in the main
// chain the next stage or the terminal format converts) and a leading
// format=rgba goes through ensureRGBA.
func (c *compiler) key(stages []string) {
	if c.hasAlpha {
		c.keyKeepingAlpha(stages)
		return
	}
	if n := len(stages); n > 1 && stages[n-1] == "format=rgba" {
		stages = stages[:n-1]
	}
	for i, s := range stages {
		if i == 0 && s == "format=rgba" {
			c.ensureRGBA()
			continue
		}
		c.emit(s)
	}
	c.hasAlpha = true
}

// keyKeepingAlpha applies a key's stages to frames that already carry alpha
// without losing it: the incoming alpha is intersected with the key's matte
// instead of being overwritten by the key filter. For the N-th wrapper the
// current chain is closed into a split and the graph continues with
//
//	<chain so far>,split[kN][kNm];
//	[kNm]alphaextract[kNa0];                       the incoming alpha
//	[kN]<key stages>,split[kNk][kNkm];             the key, on a copy
//	[kNkm]alphaextract[kNa1];                      the key's matte
//	[kNa0][kNa1]blend=all_mode=multiply[kNa];      straight-alpha intersection
//	[kNk][kNa]alphamerge,…                         the keyed colour with it
//
// multiply keeps the soft edges of both mattes (a half-alpha source edge
// stays half-alpha, a blended key edge stays blended; darken/min would do as
// well). The key stages end in format=rgba, so the keyed copy — and
// alphamerge's output, which keeps its main input's format — is rgba.
// alphaextract yields gray at the input's depth (gray off rgba — every
// planar YUV-with-alpha source arrives as rgba from its alpha head —,
// gray16 off a 16-bit RGB source) and blend/alphamerge negotiate a common
// depth for the mattes (alphamerge takes 8-bit gray); verified pixel-exact on FFmpeg 9.0.1
// (phase3_ffmpeg_test.go): a (0,0,0,0) pixel stays 0, a (255,0,0,128) edge
// stays 128, the key colour goes to 0 and the opaque subject stays 255,
// through chromakey, colorkey and a chromakey+colorkey stack. Every
// consumer that appends to the filter (";[out]…") keeps working: the last
// chain still ends in [out].
func (c *compiler) keyKeepingAlpha(stages []string) {
	c.keys++
	n := c.keys
	// c.input may be a bare "[0:v]" with no stages yet (a still, or a source
	// whose fps stage is skipped), so the split joins the chain directly
	// rather than through closeChain, which wants a stage in front of it.
	head := c.input + strings.Join(append(slices.Clone(c.stages), "split"), ",")
	c.chains = append(c.chains,
		fmt.Sprintf("%s[k%d][k%dm]", head, n, n),
		fmt.Sprintf("[k%dm]alphaextract[k%da0]", n, n),
		fmt.Sprintf("[k%d]%s,split[k%dk][k%dkm]", n, strings.Join(stages, ","), n, n),
		fmt.Sprintf("[k%dkm]alphaextract[k%da1]", n, n),
		fmt.Sprintf("[k%da0][k%da1]blend=all_mode=multiply[k%da]", n, n, n),
	)
	c.input, c.stages = fmt.Sprintf("[k%dk][k%da]", n, n), nil
	c.emit("alphamerge")
	c.hasAlpha = true
}

// keyColor validates a key colour: RRGGBB only (an alpha digit pair is
// meaningless for a key and rejected rather than silently dropped); ""
// means def, or an error when there is no default.
func keyColor(s, def string) (string, error) {
	if strings.TrimSpace(s) == "" {
		if def == "" {
			return "", fmt.Errorf("colour is required (the colour to make transparent, RRGGBB)")
		}
		return def, nil
	}
	hex, err := recipe.NormalizeHex(s)
	if err != nil {
		return "", err
	}
	if len(hex) != 6 {
		return "", fmt.Errorf("key colour %q must be RRGGBB (no alpha)", s)
	}
	return hex, nil
}

// despillType returns "green" or "blue" for a key colour dominated by that
// channel, else "".
func despillType(hex string) string {
	r, g, b := hexChannels(hex)
	switch {
	case g > r && g > b:
		return "green"
	case b > r && b > g:
		return "blue"
	}
	return ""
}

// hexChannels splits a normalised RRGGBB[AA] string into its RGB values.
func hexChannels(hex string) (r, g, b int) {
	ch := func(i int) int {
		v, _ := strconv.ParseUint(hex[i:i+2], 16, 8)
		return int(v)
	}
	return ch(0), ch(2), ch(4)
}

// unitParam resolves a [lo, 1] knob: 0 means def (the recipe zero-value
// convention), anything else must be a finite number in range.
func unitParam(name string, v, def, lo float64) (float64, error) {
	if v == 0 {
		return def, nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < lo || v > 1 {
		return 0, fmt.Errorf("%s must be between %s and 1 (got %s)", name, fexact(lo), fexact(v))
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Autocrop (resolved by jobs) and reverse.
// ---------------------------------------------------------------------------

// autocrop applies the Resolved crop exactly like a crop op at its position
// (same validation against the frame it sees). An unresolved op is an
// error: the detection pass belongs to jobs (ResolveAutoCrop), which runs it
// before compiling.
func (c *compiler) autocrop(d decodedOp, p *recipe.AutoCropParams) error {
	if p.Threshold < 0 || p.Threshold > 255 {
		return opErrorf(d, "threshold must be between 0 and 255 (got %d)", p.Threshold)
	}
	if p.Padding < 0 || p.Padding > MaxAutoCropPadding {
		return opErrorf(d, "padding must be between 0 and %d px (got %d)", MaxAutoCropPadding, p.Padding)
	}
	if p.Resolved == nil {
		return opErrorf(d, "autocrop not resolved (jobs resolves it before compiling)")
	}
	return c.crop(d, p.Resolved)
}

// reverse emits ffmpeg's reverse filter when an odd number of reverse ops
// is in the stack (two cancel out), after the geometry stages AND the
// output fit, and before the final-canvas ops. The filter holds every input
// frame in memory until EOF, so it must see the frames at their final size:
// after the output fit the frame is exactly Plan.Width x Plan.Height (no
// later stage changes the size), which MaxMasterBytes bounds — Output.
// Width/Height is where the UI's Emote/Sticker presets shrink the frame,
// and a reverse in front of that fit buffered the source-sized frames (a
// 1080p 60 s ProRes clip fit to 128 px: 28 GiB for a 0.1 GiB master) while
// the cap measured the post-fit size. Reverse commutes bit-exactly with the
// spatial stages (scale/crop/pad), and text/overlay timing stays in output
// (reversed) time because finalCanvas still follows it. The filter reuses
// the forward timestamps for the reversed frames, so Frames and Duration
// are unchanged (verified: six index frames come out 6..1 with the same
// count). A single-frame source has nothing to reverse and is left alone
// (Plan.Reversed stays false).
//
// The frames are forced to rgba right in front of the filter ("format=rgba,
// reverse"): the buffer then holds exactly Width x Height x 4 bytes per
// frame — the figure jobs' master/reverse memory estimate assumes — whatever
// depth the chain carried up to there (a 12-bit source whose chain had no
// reason to convert yet would otherwise be buffered at 8 B/px). The stages
// after the reverse are final-canvas ops and the terminal format=rgba, all
// on rgba anyway, so the rendered pixels are identical.
func (c *compiler) reverse(ops []decodedOp) {
	n := 0
	for _, d := range ops {
		if d.kind == recipe.OpReverse {
			n++
		}
	}
	if n%2 == 0 || c.singleFrame() {
		return
	}
	c.ensureRGBA()
	c.emit("reverse")
	c.plan.Reversed = true
}

// ---------------------------------------------------------------------------
// Final-canvas ops: text and overlays, in the order given, on the output
// canvas (after Output.Width/Height/Fit), so their coordinates are output
// pixels.
// ---------------------------------------------------------------------------

// finalCanvas applies the text/overlay ops. The canvas is forced to rgba
// once before the first of them: drawtext and overlay (format=auto) would
// otherwise work in the source's YUV format and blend against subsampled
// chroma.
func (c *compiler) finalCanvas(ops []decodedOp) error {
	for _, d := range ops {
		var err error
		switch p := d.params.(type) {
		case *recipe.TextParams:
			c.canvasRGBA()
			err = c.text(d, p)
		case *recipe.OverlayParams:
			c.canvasRGBA()
			err = c.overlay(d, p)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// canvasRGBA forces the canvas to rgba before the first final-canvas op
// (once: every later stage keeps rgba — drawtext is in-place and overlay's
// format=auto follows the main input).
func (c *compiler) canvasRGBA() {
	if c.rgbaCanvas {
		return
	}
	c.ensureRGBA()
	c.rgbaCanvas = true
}

// text emits drawtext. The body goes through a text file (never inline:
// drawtext's option escaping is a minefield) whose path jobs binds later
// (Plan.TextFiles / BindTextFiles); expansion=none makes drawtext print the
// file verbatim (the default "normal" expansion would eat backslashes and
// expand %{...} sequences in the user's text). The font is a fontconfig
// family name (font=, spaces need no quoting inside the option value —
// verified "font=DejaVu Sans" on FFmpeg 9; fontconfig falls back to its
// best match for an unknown family). Colours are RRGGBB or RRGGBBAA.
// Position expressions come from the anchor (text_w/text_h).
//
// drawtext is emitted in place — on the canvas, with the enable window on
// the stage — only while every element it draws is opaque (the glyphs, the
// border when Border > 0, the box when Box): on an rgba canvas drawtext
// blends its RRGGBBAA colour into the alpha plane too, so any translucent
// element (the default box colour 00000080 included) made the opaque pixels
// under it translucent (alpha 255*(1-a)+a*a) and rendered at alpha a*a/255
// over transparent pixels. A text op with a translucent element goes
// through textLayers instead.
func (c *compiler) text(d decodedOp, p *recipe.TextParams) error {
	if strings.TrimSpace(p.Text) == "" {
		return opErrorf(d, "text is required")
	}
	font, err := fontName(p.Font)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	size := p.Size
	if size == 0 {
		size = defaultFontSize
	}
	if size < 1 || size > MaxFontSize {
		return opErrorf(d, "size must be between 1 and %d px (got %d)", MaxFontSize, size)
	}
	color, err := hexColor(p.Color, defaultTextColor)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	borderColor, err := hexColor(p.BorderColor, defaultBorderColor)
	if err != nil {
		return opErrorf(d, "border colour: %v", err)
	}
	boxColor, err := hexColor(p.BoxColor, defaultBoxColor)
	if err != nil {
		return opErrorf(d, "box colour: %v", err)
	}
	if p.Border < 0 || p.Border > MaxTextBorder {
		return opErrorf(d, "border must be between 0 and %d px (got %d)", MaxTextBorder, p.Border)
	}
	if p.BoxPad < 0 || p.BoxPad > MaxTextBoxPad {
		return opErrorf(d, "boxPad must be between 0 and %d px (got %d)", MaxTextBoxPad, p.BoxPad)
	}
	if p.LineSpacing < -MaxLineSpacing || p.LineSpacing > MaxLineSpacing {
		return opErrorf(d, "lineSpacing must be between %d and %d px (got %d)", -MaxLineSpacing, MaxLineSpacing, p.LineSpacing)
	}
	x, y, err := anchorExprs(p.Anchor, p.X, p.Y, "text_w", "text_h")
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	enable, err := enableExpr(p.Start, p.End)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	spec := textSpec{
		placeholder: fmt.Sprintf(textPlaceholderFormat, len(c.plan.TextFiles)+1),
		font:        font, size: size, x: x, y: y,
		border: p.Border, lineSpacing: p.LineSpacing,
	}
	if p.Box {
		spec.boxPad = p.BoxPad
		if spec.boxPad == 0 {
			spec.boxPad = defaultBoxPad
		}
	}
	// The elements in drawtext's own draw order, each with its colour and
	// alpha; only the ones actually drawn take part.
	var elems []textElem
	if p.Box {
		elems = append(elems, textElem{kind: textBox, rgb: boxColor[:6], alpha: hexAlpha(boxColor)})
	}
	if p.Border > 0 {
		elems = append(elems, textElem{kind: textBorder, rgb: borderColor[:6], alpha: hexAlpha(borderColor)})
	}
	elems = append(elems, textElem{kind: textGlyphs, rgb: color[:6], alpha: hexAlpha(color)})
	c.plan.TextFiles = append(c.plan.TextFiles, TextFile{Placeholder: spec.placeholder, Content: p.Text})

	opaque := true
	for _, e := range elems {
		opaque = opaque && e.alpha == 255
	}
	if opaque {
		spec.fontColor, spec.borderColor, spec.boxColor = "0x"+color, "0x"+borderColor, "0x"+boxColor
		spec.borderOn, spec.boxOn, spec.enable = p.Border > 0, p.Box, enable
		c.emit(spec.stage())
		return nil
	}
	c.textLayers(spec, elems, enable)
	return nil
}

// Text element kinds, in drawtext's draw order.
const (
	textBox = iota
	textBorder
	textGlyphs
)

// textElem is one drawn element of a text op: its opaque colour (RRGGBB)
// and the alpha (0..255) it is meant to be composited with.
type textElem struct {
	kind  int
	rgb   string
	alpha int
}

// textSpec holds the options of one drawtext stage: the shared geometry of
// a text op plus the per-stage colours and which elements the stage draws.
type textSpec struct {
	placeholder, font string
	size              int
	x, y              string
	border            int // outline width (drawn only when borderOn)
	boxPad            int // box padding (drawn only when boxOn)
	lineSpacing       int

	fontColor, borderColor, boxColor string // ffmpeg colour values (0x…)
	borderOn, boxOn                  bool
	enable                           string // "" = always
}

// stage renders the drawtext filter text. Option order: textfile, expansion,
// font, fontsize, fontcolor, x, y, [borderw, bordercolor], [box, boxcolor,
// boxborderw], [line_spacing], [enable].
func (s textSpec) stage() string {
	parts := []string{
		"textfile=" + s.placeholder, "expansion=none",
		"font=" + s.font, "fontsize=" + strconv.Itoa(s.size), "fontcolor=" + s.fontColor,
		"x=" + s.x, "y=" + s.y,
	}
	if s.borderOn {
		parts = append(parts, "borderw="+strconv.Itoa(s.border), "bordercolor="+s.borderColor)
	}
	if s.boxOn {
		parts = append(parts, "box=1", "boxcolor="+s.boxColor, "boxborderw="+strconv.Itoa(s.boxPad))
	}
	if s.lineSpacing != 0 {
		parts = append(parts, "line_spacing="+strconv.Itoa(s.lineSpacing))
	}
	if s.enable != "" {
		parts = append(parts, "enable="+s.enable)
	}
	return "drawtext=" + strings.Join(parts, ":")
}

// textLayerTail follows every text layer's drawtext: drawtext blends its
// opaque colour over the layer's transparent black with the glyph coverage
// in BOTH the colour and the alpha plane (drawutils blends every component,
// alpha included, towards the colour's), so an anti-aliased edge pixel comes
// out premultiplied — white at coverage 0.24 is (61,61,61,61), measured on
// FFmpeg 9.0.1 — and a straight-alpha composite of that over the canvas
// would darken every glyph edge by the coverage a second time ((15,15,111)
// over blue where in-place drawtext gives (61,61,127)). Tagging the layer
// premultiplied and dividing the alpha out (on planar gbrap, the format the
// filter takes; the tag keeps FFmpeg >= 8 from auto-inserting a cancelling
// premultiply, exactly like the hoisted head) restores the straight colour
// ((255,255,255,61)) so the overlay composites the layer exactly like
// drawtext composites in place — and, over a transparent canvas, without
// the dark fringe in-place drawtext leaves there. Opaque box pixels and
// fully transparent pixels are unchanged by the division.
const textLayerTail = "format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba"

// textLayers draws a text op with translucent elements without touching
// the canvas' alpha plane with drawtext: the current chain is closed
// ("[bN]") and, per group of adjacent elements sharing one alpha, in draw
// order box → border → glyphs, a layer chain
//
//	color=c=0x00000000:s=WxH:r=FPS,format=rgba,drawtext=<the op's
//	textfile/expansion/font/fontsize/x/y/line_spacing, the group's elements
//	in their opaque RRGGBB, the other elements suppressed — box omitted,
//	borderw omitted, fontcolor=0x00000000 for a layer without glyphs>,
//	format=gbrap,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,
//	format=rgba[,colorchannelmixer=aa=<alpha/255>][tK]
//
// is composited with "[bN][tK]overlay=format=auto:shortest=1:eof_action=
// repeat[:enable=<window>]": the layer is drawn opaque (with the glyph
// coverage as its alpha, made straight by textLayerTail), its alpha scaled
// as a whole by colorchannelmixer, and the overlay blends it with straight
// alpha — the opaque canvas stays opaque, a box over transparent pixels
// comes out at exactly its alpha, and the glyph edges match in-place
// drawtext pixel for pixel (verified on FFmpeg 9.0.1, phase3_ffmpeg_test.go).
// The enable window moves from drawtext to the composite, exactly like an
// image overlay's. W x H and FPS are the plan's output canvas and rate; the
// color source is infinite, so shortest=1 lets the base decide the length
// (one frame for a still, no tpad). Every layer's drawtext carries the op's
// textfile placeholder; BindTextFiles replaces every occurrence.
func (c *compiler) textLayers(spec textSpec, elems []textElem, enable string) {
	composite := "overlay=format=auto:shortest=1:eof_action=repeat"
	if enable != "" {
		composite += ":enable=" + enable
	}
	for i := 0; i < len(elems); {
		// The group: elems[i:j], adjacent elements with one alpha.
		j := i + 1
		for j < len(elems) && elems[j].alpha == elems[i].alpha {
			j++
		}
		layer := spec
		layer.fontColor = "0x00000000"
		for _, e := range elems[i:j] {
			switch e.kind {
			case textBox:
				layer.boxOn, layer.boxColor = true, "0x"+e.rgb
			case textBorder:
				layer.borderOn, layer.borderColor = true, "0x"+e.rgb
			case textGlyphs:
				layer.fontColor = "0x" + e.rgb
			}
		}
		chain := fmt.Sprintf("color=c=0x00000000:s=%dx%d:r=%s,format=rgba,%s,%s", c.w, c.h, fnum(c.plan.FPS), layer.stage(), textLayerTail)
		if a := elems[i].alpha; a < 255 {
			chain += ",colorchannelmixer=aa=" + fnum(float64(a)/255)
		}
		base := c.closeChain()
		c.layers++
		label := fmt.Sprintf("[t%d]", c.layers)
		c.chains = append(c.chains, chain+label)
		c.input = base + label
		c.emit(composite)
		i = j
	}
}

// hexAlpha returns the alpha of a normalised RRGGBB[AA] colour (255 without
// an alpha pair).
func hexAlpha(hex string) int {
	if len(hex) < 8 {
		return 255
	}
	v, _ := strconv.ParseUint(hex[6:8], 16, 8)
	return int(v)
}

// fontName validates a fontconfig family name: letters, digits, spaces and
// hyphens only (recipe.TextParams.Font), so it can never break out of the
// filter option; "" means the default family.
func fontName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultFont, nil
	}
	if len(s) > MaxFontNameLen {
		return "", fmt.Errorf("font name is too long (%d characters, maximum %d)", len(s), MaxFontNameLen)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '-':
		default:
			return "", fmt.Errorf("font %q may only contain letters, digits, spaces and hyphens", s)
		}
	}
	return s, nil
}

// hexColor normalises an RRGGBB or RRGGBBAA colour ("" = def) to lowercase
// hex digits without the hash (recipe.NormalizeHex); "0x" + the result is
// ffmpeg's notation. RRGGBBAA is accepted as documented for the text op
// (the compiler decides how to honour the alpha, see text).
func hexColor(s, def string) (string, error) {
	if strings.TrimSpace(s) == "" {
		s = def
	}
	return recipe.NormalizeHex(s)
}

// anchorExprs returns the x/y expressions that place an element of size
// (wVar x hVar) — "text_w"/"text_h" for drawtext, "overlay_w"/"overlay_h"
// for overlay — so that its anchor point lands on (x, y): left/top anchors
// use the coordinate as is, centre anchors subtract half the size, right/
// bottom anchors the whole size.
func anchorExprs(anchor string, x, y int, wVar, hVar string) (xExpr, yExpr string, err error) {
	a := strings.ToLower(strings.TrimSpace(anchor))
	if a == "" {
		a = recipe.AnchorTopLeft
	}
	bad := fmt.Errorf("anchor %q must be one of tl, tc, tr, ml, mc, mr, bl, bc, br", anchor)
	if len(a) != 2 {
		return "", "", bad
	}
	xExpr, yExpr = strconv.Itoa(x), strconv.Itoa(y)
	switch a[1] {
	case 'l':
	case 'c':
		xExpr += "-" + wVar + "/2"
	case 'r':
		xExpr += "-" + wVar
	default:
		return "", "", bad
	}
	switch a[0] {
	case 't':
	case 'm':
		yExpr += "-" + hVar + "/2"
	case 'b':
		yExpr += "-" + hVar
	default:
		return "", "", bad
	}
	return xExpr, yExpr, nil
}

// enableTolerance is the slack, in seconds, added to the frame time in an
// enable expression: 'gte(t+0.0001,S)'. The window bounds are microsecond-
// rounded (like trim bounds), but unlike -ss/-to — which ffmpeg rescales
// onto the stream's tick grid — enable compares the raw double timestamp,
// so a bound taken from the frame grid can land just above its frame:
// frame 2 of a 30 fps clip is at t = 0.0666666…, round6(2/30) = 0.066667,
// and a plain gte(t,S) skipped the frame (and lt(t,E) drew the frame at an
// end of 5/30 → 0.166667). 1e-4 s swallows the rounding (at most 5e-7) and
// stays far below any frame period (1/60 s = 0.0167 at the MaxFPS cap), so
// no neighbouring frame is affected. Verified against ffmpeg in
// phase3_ffmpeg_test.go.
const enableTolerance = 1e-4

// enableExpr returns the timeline enable expression for an output-time
// window [start, end): "" for the whole clip, 'gte(t+T,S)' for an open end
// (end 0), 'gte(t+T,S)*lt(t+T,E)' otherwise (quoted, so the commas survive
// the filtergraph parser; T is enableTolerance). The end is exclusive like a
// trim's: the frame whose timestamp is exactly E is NOT drawn, so a window
// that ends at the start of the next frame (what the UI's "from scrubber"
// produces) covers exactly the frames up to the scrubber's one, and two
// adjacent windows [A,B) and [B,C) never overlap. ffmpeg's between() is
// inclusive on both ends and would draw the frame at E too (verified:
// phase3_ffmpeg_test.go).
//
// The contract between the two ends of a frame-grid bound: start and end
// are rounded to the nearest microsecond here (round6, like trim bounds),
// the expression compares t + 0.0001 s with them, and the SPA's "from
// scrubber" buttons store bounds FLOORED to whole microseconds
// (web/src/lib/overlay.ts scrubberRange: start = floor_µs(i/fps), end =
// floor_µs((i+1)/fps), 0 on the last frame). Either way a bound that names
// frame i lies within 1 µs of the frame's raw timestamp, below the
// tolerance, so the frame at a start bound is always drawn and the frame at
// an end bound never is — a bound can never miss its frame, and the
// tolerance can never reach the neighbouring frame. The expression compares
// t, not the frame index n, because enc's still path seeks the main input
// and relies on the absolute t clock.
func enableExpr(start, end float64) (string, error) {
	for _, v := range []float64{start, end} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", fmt.Errorf("start/end must be finite numbers")
		}
	}
	s, e := round6(start), round6(end)
	if s < 0 {
		return "", fmt.Errorf("start must be >= 0 (got %s)", fnum6(s))
	}
	if e < 0 {
		return "", fmt.Errorf("end must be >= 0 (got %s)", fnum6(e))
	}
	if e > 0 && e <= s {
		return "", fmt.Errorf("end (%s s) must be after start (%s s)", fnum6(e), fnum6(s))
	}
	tt := "t+" + fnum6(enableTolerance)
	switch {
	case e > 0:
		return fmt.Sprintf("'gte(%s,%s)*lt(%s,%s)'", tt, fnum6(s), tt, fnum6(e)), nil
	case s > 0:
		return fmt.Sprintf("'gte(%s,%s)'", tt, fnum6(s)), nil
	}
	return "", nil
}

// overlay composites recipe source p.Source onto the canvas. The source
// becomes an ExtraInput (one per distinct source, in order of first use;
// ffmpeg input index = position + 1) with its own input chain
//
//	[k:v]{unpremultiply head}format=rgba[,fps=F][,scale…][,colorchannelmixer=aa=O][,tpad…][ovN]
//
// and the current chain is closed ("[bM]") and continued as
//
//	[bM][ovN]overlay=x=X:y=Y:format=auto:shortest=1:eof_action=repeat[:enable=…]
//
// format=auto keeps the rgba main (base alpha survives, verified);
// shortest=1 ends the output with the base whatever the overlay's length
// (see overlayHold for why finite overlays are padded); eof_action=repeat is
// the framesync default spelled out. The overlay's own timeline starts at
// the output's t=0 (no offset: a timed overlay is gated by enable, not
// shifted), which is what enc's still/proxy seeks assume.
func (c *compiler) overlay(d decodedOp, p *recipe.OverlayParams) error {
	ov, err := c.overlaySource(d, p.Source)
	if err != nil {
		return err
	}
	animated := ov.Frames > 1 || !ov.IsStill
	loop := animated && !p.NoLoop
	w, h, err := overlaySize(ov, p.Width, p.Height)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	opacity, err := opacityParam(p.Opacity)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	x, y, err := anchorExprs(p.Anchor, p.X, p.Y, "overlay_w", "overlay_h")
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	enable, err := enableExpr(p.Start, p.End)
	if err != nil {
		return opErrorf(d, "%v", err)
	}
	ref, err := c.extraInput(d, p.Source, ov, animated, loop)
	if err != nil {
		return err
	}
	c.ovs++
	label := fmt.Sprintf("[ov%d]", c.ovs)
	chain := c.overlayChain(ref.index+1, c.ovs, ov, animated, ref.infinite, w, h, opacity) + label
	base := c.closeChain()
	c.chains = append(c.chains, chain)
	c.input = base + label
	composite := fmt.Sprintf("overlay=x=%s:y=%s:format=auto:shortest=1:eof_action=repeat", x, y)
	if enable != "" {
		composite += ":enable=" + enable
	}
	c.emit(composite)
	return nil
}

// overlaySource validates the overlay's source index and probe info.
func (c *compiler) overlaySource(d decodedOp, idx int) (recipe.ProbeInfo, error) {
	if idx <= 0 {
		return recipe.ProbeInfo{}, opErrorf(d, "source must be >= 1 (source 0 is the main source; got %d)", idx)
	}
	if idx >= len(c.srcs) {
		return recipe.ProbeInfo{}, opErrorf(d, "source index %d is out of range (the recipe has %d source(s))", idx, len(c.srcs))
	}
	ov := c.srcs[idx]
	if ov.Kind == recipe.KindSequence || ov.Sequence != nil {
		return recipe.ProbeInfo{}, opErrorf(d, "source %d is an image sequence; sequence overlays are not supported", idx)
	}
	if ov.Width <= 0 || ov.Height <= 0 {
		return recipe.ProbeInfo{}, opErrorf(d, "source %d has no usable frame size (%dx%d)", idx, ov.Width, ov.Height)
	}
	if err := checkStreams(ov); err != nil {
		return recipe.ProbeInfo{}, opErrorf(d, "source %d: %v", idx, err)
	}
	return ov, nil
}

// overlaySize resolves the overlay's frame size: natural, or Width x Height
// (exact when both are given; one of them keeps the aspect).
func overlaySize(ov recipe.ProbeInfo, width, height int) (int, int, error) {
	if width < 0 || height < 0 {
		return 0, 0, fmt.Errorf("width and height must be >= 0 (got %dx%d)", width, height)
	}
	if width > MaxDim || height > MaxDim {
		return 0, 0, fmt.Errorf("width and height must be <= %d (got %dx%d)", MaxDim, width, height)
	}
	w, h := ov.Width, ov.Height
	if width > 0 || height > 0 {
		w, h = scaledSize(ov.Width, ov.Height, width, height, fitExact)
	}
	if err := checkFrame(w, h); err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

// opacityParam resolves Opacity: 0 means 1, else (0, 1].
func opacityParam(v float64) (float64, error) {
	if v == 0 {
		return 1, nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return 0, fmt.Errorf("opacity must be between 0 and 1 (got %s)", fexact(v))
	}
	return v, nil
}

// extraInput registers source idx as an ExtraInput (once per source; a
// second overlay of the same source reuses the input — ffmpeg feeds one
// decoded stream to every "[k:v]" reference, verified — but must agree on
// looping, since the loop args belong to the input).
func (c *compiler) extraInput(d decodedOp, idx int, ov recipe.ProbeInfo, animated, loop bool) (extraRef, error) {
	if ref, ok := c.extras[idx]; ok {
		if c.plan.ExtraInputs[ref.index].Loop != loop {
			return extraRef{}, opErrorf(d, "source %d is already used by another overlay with a different loop setting", idx)
		}
		return ref, nil
	}
	args, infinite := overlayArgs(ov, animated, loop)
	in := ExtraInput{Source: idx, Args: args, Animated: animated, Loop: loop}
	if animated && ov.Duration > 0 {
		in.Duration = ov.Duration
	}
	c.plan.ExtraInputs = append(c.plan.ExtraInputs, in)
	ref := extraRef{index: len(c.plan.ExtraInputs) - 1, infinite: infinite}
	c.extras[idx] = ref
	return ref, nil
}

// overlayArgs returns the per-input args of an overlay source and whether
// the input is infinite by construction (then no hold stage is needed).
// All verified on FFmpeg 9.0.1 with a 6-frame base:
//
//   - still image read by image2 or a *_pipe demuxer (png, jpeg, bmp, tiff,
//     a still webp): "-loop 1" — an endless stream at the demuxer's rate;
//   - still carried by any other demuxer (a one-frame gif/apng/webp_anim, a
//     still AVIF through mov): those have no "loop" option ("Option loop
//     not found"), so no args — the hold stage clones the single frame;
//   - looping gif and video containers (mp4/mov/webm/mkv): "-stream_loop
//     -1" — deterministic, ignores the file's own loop count;
//   - looping apng / animated webp: "-ignore_loop 0" — "-stream_loop -1"
//     hangs ffmpeg on the webp_anim and apng demuxers (the seek back to the
//     start never yields a frame; even "-stream_loop -1 -t 1 -f null -"
//     never returns), so the demuxer obeys the file's loop count instead: a
//     loop-forever file (the usual case, what this app writes) repeats, a
//     play-N-times file plays N times and the hold stage then freezes its
//     last frame;
//   - non-looping animation: no args — every animation demuxer plays once
//     by default (ignore_loop=1) and the hold stage keeps the last frame;
//   - VP8/VP9 alpha: the libvpx decoder (decoderArgs), as for the main
//     input.
func overlayArgs(ov recipe.ProbeInfo, animated, loop bool) (args []string, infinite bool) {
	args = decoderArgs(ov)
	switch {
	case !animated:
		if !imageDemuxer(ov.Format) {
			return args, false
		}
		return append(args, "-loop", "1"), true
	case loop:
		if ignoreLoopDemuxer(ov.Format) {
			return append(args, "-ignore_loop", "0"), false
		}
		return append(args, "-stream_loop", "-1"), true
	default:
		return args, false
	}
}

// demuxerName is the first name of an ffprobe format_name list, lowercased
// ("mov,mp4,m4a,3gp,3g2,mj2" → "mov").
func demuxerName(format string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(format), ",")
	return strings.ToLower(name)
}

// imageDemuxer reports whether format is the image2 demuxer or one of its
// *_pipe siblings (png_pipe, jpeg_pipe, webp_pipe, …), the only ones with
// the "loop" option.
func imageDemuxer(format string) bool {
	name := demuxerName(format)
	return name == "image2" || strings.HasSuffix(name, "_pipe")
}

// seekUnsafeDemuxer reports whether format's demuxer decodes nothing after
// any input seek (FFmpeg 9's webp_anim), so a trim must be a filter stage
// and the plan is flagged Plan.SeekUnsafe for the preview renderers.
func seekUnsafeDemuxer(format string) bool {
	return demuxerName(format) == "webp_anim"
}

// ignoreLoopDemuxer reports whether looping format must go through
// "-ignore_loop 0" because "-stream_loop -1" hangs on its demuxer.
func ignoreLoopDemuxer(format string) bool {
	switch demuxerName(format) {
	case "apng", "webp", "webp_anim":
		return true
	}
	return false
}

// overlayChain builds the overlay input's chain (without its output label)
// for ffmpeg input k, overlay number n:
//
//	[k:v]                                   (or [k:v:C]; an alpha stream is merged first, as for the main source)
//	[format=gbrap…,]                        premultiplied RGB probe: the native-depth gbrap (not for planar YUV-with-alpha
//	[setparams=…,unpremultiply=inplace=1,]  sources, which unpremultiply takes natively — the same rule as alphaHead)
//	format=rgba
//	[,fps=F]                                animated overlays are resampled to the plan rate
//	[,scale…]                               premultiplied lanczos chain when the overlay has alpha, then format=rgba
//	[,colorchannelmixer=aa=O]               opacity < 1 scales the (straight) alpha
//	[,tpad=stop_mode=clone:stop=-1]         unless the input is infinite by construction
func (c *compiler) overlayChain(k, n int, ov recipe.ProbeInfo, animated, infinite bool, w, h int, opacity float64) string {
	alpha := ov.HasAlpha || ov.AlphaStream > 0
	depth := ov.Bits
	// yuva: the frames reaching the chain are planar YUV with alpha; they
	// must go through exactly one yuva→rgba conversion and never through
	// gbrap (see alphaHead).
	yuva := ov.AlphaStream == 0 && planarYUVAlpha(ov)
	var head string
	switch {
	case ov.AlphaStream > 0:
		head = fmt.Sprintf("[%d:v:%d]format=rgba[ov%dc];[%d:v:%d]format=gray[ov%da];[ov%dc][ov%da]alphamerge,",
			k, ov.ColorStream, n, k, ov.AlphaStream, n, n, n)
		depth = 8
	case ov.ColorStream > 0:
		head = fmt.Sprintf("[%d:v:%d]", k, ov.ColorStream)
	default:
		head = fmt.Sprintf("[%d:v]", k)
	}
	var stages []string
	if alpha && ov.Premultiplied {
		if !yuva {
			stages = append(stages, "format="+gbrapFormat(depth))
		}
		stages = append(stages, "setparams=alpha_mode=premultiplied", "unpremultiply=inplace=1")
	}
	stages = append(stages, "format=rgba")
	if animated {
		stages = append(stages, "fps="+fnum(c.plan.FPS))
	}
	if w != ov.Width || h != ov.Height {
		scale := fmt.Sprintf("scale=%d:%d:flags=lanczos", w, h)
		if alpha {
			// The premultiplied chain converts to gbrap itself: a format=rgba
			// right in front of it would be a no-op stage — except off a yuva
			// source, whose one conversion to rgba it is.
			if n := len(stages); !yuva && stages[n-1] == "format=rgba" {
				stages = stages[:n-1]
			}
			stages = append(stages, "format=gbrap", "premultiply=inplace=1", scale, "unpremultiply=inplace=1", "format=rgba")
		} else {
			stages = append(stages, scale)
		}
	}
	if opacity < 1 {
		stages = append(stages, "colorchannelmixer=aa="+fnum(opacity))
	}
	if !infinite {
		stages = append(stages, overlayHold)
	}
	return head + strings.Join(stages, ",")
}

// gbrapFormat is the planar RGBA format at the given bit depth (8 unless 10
// or 12), the format the (un)premultiply filters run on.
func gbrapFormat(depth int) string {
	switch depth {
	case 10:
		return "gbrap10le"
	case 12:
		return "gbrap12le"
	}
	return "gbrap"
}

// checkStreams validates a probe's colour/alpha stream indices.
func checkStreams(info recipe.ProbeInfo) error {
	if info.AlphaStream < 0 {
		return fmt.Errorf("source alpha stream index must be >= 0 (got %d)", info.AlphaStream)
	}
	if info.ColorStream < 0 {
		return fmt.Errorf("source colour stream index must be >= 0 (got %d)", info.ColorStream)
	}
	if info.AlphaStream > 0 && info.AlphaStream == info.ColorStream {
		return fmt.Errorf("source alpha stream and colour stream are both v:%d", info.AlphaStream)
	}
	return nil
}
