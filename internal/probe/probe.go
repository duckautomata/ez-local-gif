// Package probe inspects an uploaded file with ffprobe (+ a short ffmpeg
// alpha scan) and produces recipe.ProbeInfo.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// ErrNotImplemented is kept for API compatibility with the Phase-1 stubs;
// nothing in this package returns it any more.
var ErrNotImplemented = errors.New("probe: not implemented")

// ErrNoVideo is returned when ffprobe finds no video stream in the file.
var ErrNoVideo = errors.New("probe: no video stream")

// ErrNotImplementedSequence is kept for API compatibility with the Phase-2
// stub; ProbeSequence no longer returns it.
var ErrNotImplementedSequence = errors.New("probe: sequence probing not implemented")

// ProbeSequence (Phase 2) describes an image-sequence blob directory
// (store.PutSequence layout: 000001.<ext> … N.<ext>, one extension):
//   - Kind KindSequence, IsStill false, Codec/PixFmt/Bits from ffprobe of the
//     first frame, Format "image2".
//   - Width/Height = the largest frame (stdlib image.DecodeConfig for
//     png/jpeg/gif, ffprobe over the image2 pattern for other formats,
//     sampling at most 200 frames and assuming uniform beyond);
//     Sequence.Mixed when any sampled frame differs. A sequence whose
//     frames neither the stdlib nor ffprobe's image2 read is an error (the
//     render opens the same pattern, so every job would fail), one the
//     server classifies as an unreadable source.
//   - Frames = count, FPS = 1000/delayMS (delayMS <= 0 → 100), Duration =
//     Frames/FPS, Sequence = {Count, Pattern "%06d.<ext>", DelayMS, Mixed}.
//   - HasAlpha: an alpha scan (enc.AlphaScanArgs on individual files) of a
//     few frames sampled from those whose header can carry transparency
//     ffmpeg would decode — NRGBA/translucent-palette models, every GIF
//     frame (the transparent index hides in each frame's GCE), PNG
//     truecolour/gray with a tRNS colour key, or the first frame when its
//     ffprobe pix_fmt admits alpha.
//   - Premultiplied false.
//
// The implementation lives in sequence.go.
func ProbeSequence(ctx context.Context, tools ffrun.Tools, dir string, delayMS int) (recipe.ProbeInfo, error) {
	return probeSequence(ctx, tools, dir, delayMS)
}

// DefaultScanFrames is the alpha-scan frame budget when maxScanFrames <= 0.
const DefaultScanFrames = 60

// ffmpegPrefix is what ffrun.RunFFmpeg prepends; ffrun.RunOutput does not,
// so stdout-producing invocations add it themselves.
var ffmpegPrefix = []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error"}

// Probe runs ffprobe (enc.ProbeArgs) and derives ProbeInfo:
//   - Format/Codec/Profile/PixFmt/Width/Height from the first video stream.
//     Width/Height are the *displayed* dimensions: when the stream carries a
//     Display Matrix side data with a 90/270 degree rotation (portrait phone
//     MP4/MOV) the coded width and height are swapped, because ffmpeg's
//     default autorotate inserts the matching transpose in front of every
//     filtergraph input (master, still, proxy, alpha scan) and so emits
//     frames of the swapped size. 180 degrees and odd angles keep the coded
//     size, exactly like fftools.
//   - FPS from r_frame_rate (avg_frame_rate fallback); 0 for stills. For
//     GIF/APNG/WebP/AVIF animations r_frame_rate is used only when it is a
//     plausible cadence (<= 60), see derive.
//   - Duration from stream then format; Frames from nb_frames, else
//     round(Duration*FPS), else 0. Stills: Frames 1, IsStill true.
//   - Kind: image (single frame image codecs: png/jpeg/webp-still/…, and
//     any source whose established frame count is 1 — nb_frames == 1 or a
//     decode count of 1 — whatever the codec: a one-frame ProRes/H.264 MOV
//     is a still, since ffmpeg's fps filter emits nothing for a single
//     frame), animation (gif, apng, webp_anim/animated webp, avif with >1
//     frame), video otherwise.
//   - Bits: from pix_fmt name (…p10le → 10, …p12le → 12, 16 for rgba64,
//     else 8).
//   - HasAlpha: pix_fmt has an alpha plane (yuva*, rgba/bgra/argb/abgr,
//     gbrap*, ya8/ya16, pal8 → maybe) AND, for palette/animation formats
//     (gif, webp, apng, avif, png) or when the pix_fmt is ambiguous, an
//     alpha scan of up to maxScanFrames frames (enc.AlphaScanArgs) finds a
//     byte < 255. VP9/VP8 in WebM/MKV: alpha_mode stream tag == "1" → true
//     (decoder must then be libvpx-vp9/libvpx; graph handles that from
//     Codec+HasAlpha).
//   - Premultiplied: true when Codec == "prores" && HasAlpha (Resolve
//     exports premultiplied); false otherwise.
//   - AVIF (mov demuxer, brand avif/avis): libavif writes the primary
//     image item(s) and, for animations, separate tracks, so ffprobe lists
//     [colour item, (alpha item,) colour track, (alpha track)]. The main
//     stream is the video stream with the most frames (the track for an
//     animation, the item for a still); AlphaStream is the video-stream
//     index ("v:N") of the matching single-plane (gray) stream with the
//     same size and frame count, 0 when the file has no alpha. HasAlpha
//     follows from that (the colour stream's own pix_fmt is opaque).
//     Monochrome (yuv400) AVIFs report gray for every stream, colour ones
//     included; there the most-frames rule runs over the streams not
//     titled "Alpha" (ties to the first, the colour track precedes its
//     alpha track), so a gray animation is still described by its track.
//
// maxScanFrames <= 0 means 60.
func Probe(ctx context.Context, tools ffrun.Tools, path string, maxScanFrames int) (recipe.ProbeInfo, error) {
	if tools.FFprobe == "" {
		return recipe.ProbeInfo{}, errors.New("probe: ffprobe is not available")
	}
	if maxScanFrames <= 0 {
		maxScanFrames = DefaultScanFrames
	}

	raw, err := ffrun.RunOutput(ctx, tools.FFprobe, enc.ProbeArgs(path))
	if err != nil {
		return recipe.ProbeInfo{}, fmt.Errorf("ffprobe: %w", err)
	}
	out, err := parseOutput(raw)
	if err != nil {
		return recipe.ProbeInfo{}, err
	}
	d, err := derive(out)
	if err != nil {
		return recipe.ProbeInfo{}, err
	}
	info := d.info

	// Animations whose frame count could not be established from metadata:
	// count decodable frames with ffmpeg (slow-ish but exact).
	if d.needFrameCount && tools.FFmpeg != "" {
		if n, err := countFrames(ctx, tools.FFmpeg, path); err != nil {
			log.Printf("probe: frame count of %s failed: %v", path, err)
		} else {
			info.Frames = n
		}
		if info.Frames == 1 {
			markStill(&info)
		}
	}
	// Containers that state neither a duration nor a frame count (ffprobe on
	// the webp_anim demuxer reports only r_frame_rate) still need a duration
	// for the UI scrubber/trim and for graph.Plan: derive it from the counted
	// frames at the nominal rate.
	if !info.IsStill && info.Duration <= 0 && info.Frames > 1 && info.FPS > 0 {
		info.Duration = float64(info.Frames) / info.FPS
	}

	// Alpha decision.
	switch d.alpha {
	case alphaDecided:
		// already set on info
	case alphaScan:
		if tools.FFmpeg == "" {
			info.HasAlpha = d.assumeAlpha
			break
		}
		has, err := scanAlpha(ctx, tools.FFmpeg, path, scanFrameBudget(maxScanFrames, info.Width, info.Height))
		if err != nil {
			log.Printf("probe: alpha scan of %s failed (%v); assuming hasAlpha=%v from pix_fmt %q", path, err, d.assumeAlpha, info.PixFmt)
			info.HasAlpha = d.assumeAlpha
		} else {
			info.HasAlpha = has
		}
	}

	info.Premultiplied = info.Codec == "prores" && info.HasAlpha
	return info, nil
}

// ---- ffprobe JSON model ----------------------------------------------------

// flexString accepts a JSON string or number (ffprobe emits numbers as
// strings in most places, but be tolerant).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.TrimSpace(string(b)))
	return nil
}

type ffStream struct {
	CodecType    string            `json:"codec_type"`
	CodecName    string            `json:"codec_name"`
	Profile      flexString        `json:"profile"`
	PixFmt       string            `json:"pix_fmt"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	RFrameRate   string            `json:"r_frame_rate"`
	AvgFrameRate string            `json:"avg_frame_rate"`
	NbFrames     flexString        `json:"nb_frames"`
	Duration     flexString        `json:"duration"`
	Tags         map[string]string `json:"tags"`
	Disposition  map[string]int    `json:"disposition"`
	SideDataList []ffSideData      `json:"side_data_list"`
}

// ffSideData is one stream side-data entry. Only the Display Matrix entry is
// used: ffprobe prints its rotation as a bare integer (degrees, counter-
// clockwise, in -180..180), flexString tolerates a string too.
type ffSideData struct {
	Type     string     `json:"side_data_type"`
	Rotation flexString `json:"rotation"`
}

// displayMatrixType is the side_data_type ffprobe prints for
// AV_PKT_DATA_DISPLAYMATRIX.
const displayMatrixType = "Display Matrix"

type ffFormat struct {
	FormatName string            `json:"format_name"`
	Duration   flexString        `json:"duration"`
	Tags       map[string]string `json:"tags"`
}

type ffOutput struct {
	Streams []ffStream `json:"streams"`
	Format  ffFormat   `json:"format"`
}

func parseOutput(raw []byte) (ffOutput, error) {
	var out ffOutput
	if len(strings.TrimSpace(string(raw))) == 0 {
		return out, errors.New("ffprobe: empty output")
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("ffprobe: decode json: %w", err)
	}
	return out, nil
}

// ---- derivation -----------------------------------------------------------

type alphaMode int

const (
	alphaDecided alphaMode = iota // info.HasAlpha is final
	alphaScan                     // run the alpha scan; assumeAlpha on failure
)

// derived is the metadata-only result plus what still needs a process.
type derived struct {
	info           recipe.ProbeInfo
	alpha          alphaMode
	assumeAlpha    bool
	needFrameCount bool
}

// container classifies the demuxer/brand for the Kind decision.
type container int

const (
	cOther container = iota
	cGIF
	cAPNG
	cWebPAnim
	cWebPStill
	cAVIF // single-image AVIF (brand avif)
	cAVIS // animated AVIF (brand avis)
	cImage
)

// stillCodecs are image codecs: with a single frame (in any container) they
// mean "still image"; with many frames (mjpeg in AVI, png in MOV) they are
// video.
var stillCodecs = map[string]bool{
	"png": true, "mjpeg": true, "jpeg2000": true, "jpegls": true, "jpegxl": true,
	"webp": true, "bmp": true, "tiff": true, "targa": true, "ppm": true, "pgm": true,
	"pbm": true, "pam": true, "sgi": true, "pcx": true, "psd": true, "exr": true,
	"dds": true, "qoi": true, "xbm": true, "xpm": true, "hdr": true, "photocd": true,
}

// scanCodecs are the palette/animation codecs whose pix_fmt says "has an
// alpha plane" without proving any pixel uses it (the GIF decoder always
// reports bgra, PNG rgba may be fully opaque, ...).
var scanCodecs = map[string]bool{"gif": true, "webp": true, "apng": true, "png": true}

func classifyContainer(out ffOutput) container {
	name := out.Format.FormatName
	first := name
	if i := strings.IndexByte(name, ','); i >= 0 {
		first = name[:i]
	}
	// AVIF rides on the mov demuxer; the ftyp brands tell still (avif) from
	// image sequence (avis). libavif writes them as major_brand, other
	// muxers may only list them among compatible_brands.
	brands := strings.ToLower(out.Format.Tags["major_brand"] + " " + out.Format.Tags["compatible_brands"])
	isMov := strings.Contains(name, "mov")
	switch {
	case first == "gif":
		return cGIF
	case first == "apng":
		return cAPNG
	case first == "webp_pipe":
		return cWebPStill
	case strings.HasPrefix(first, "webp"):
		return cWebPAnim
	case isMov && strings.Contains(brands, "avis"):
		return cAVIS
	case isMov && strings.Contains(brands, "avif"):
		return cAVIF
	case first == "image2" || strings.HasSuffix(first, "_pipe"):
		return cImage
	}
	return cOther
}

// derive computes everything that does not need another process.
func derive(out ffOutput) (derived, error) {
	cont := classifyContainer(out)
	vs := pickVideoStream(out.Streams)
	if cont == cAVIF || cont == cAVIS {
		vs = pickAVIFColorStream(out.Streams)
	}
	if vs == nil {
		return derived{}, ErrNoVideo
	}
	if vs.Width <= 0 || vs.Height <= 0 {
		return derived{}, fmt.Errorf("probe: video stream has no dimensions (%dx%d)", vs.Width, vs.Height)
	}
	d := derived{}
	info := &d.info
	info.Format = out.Format.FormatName
	info.Codec = vs.CodecName
	info.Profile = strings.TrimSpace(string(vs.Profile))
	if info.Profile == "unknown" {
		info.Profile = ""
	}
	info.PixFmt = vs.PixFmt
	info.Bits = bitsFromPixFmt(vs.PixFmt)
	info.Width = vs.Width
	info.Height = vs.Height
	if rotationSwapsDims(displayRotation(vs)) {
		// ffmpeg autorotates on decode (transpose in front of every
		// filtergraph input), so the frames every consumer sees are HxW.
		info.Width, info.Height = vs.Height, vs.Width
	}
	for i := range out.Streams {
		if out.Streams[i].CodecType == "audio" {
			info.HasAudio = true
			break
		}
	}

	// Frame rate.
	rFPS := parseRate(vs.RFrameRate)
	avgFPS := parseRate(vs.AvgFrameRate)
	switch cont {
	case cGIF, cAPNG, cWebPAnim, cAVIS:
		info.FPS = animationFPS(rFPS, avgFPS)
	default:
		info.FPS = firstPositive(rFPS, avgFPS)
	}

	// Duration: stream, then format.
	info.Duration = firstPositive(parseFloat(string(vs.Duration)), parseFloat(string(out.Format.Duration)))

	// Frames. nbFrames is the container's own count (established); frames
	// falls back to an estimate from duration x fps.
	nbFrames := parseIntFlex(string(vs.NbFrames))
	frames := nbFrames
	if frames <= 0 && info.Duration > 0 && info.FPS > 0 {
		frames = int(math.Round(info.Duration * info.FPS))
		if frames < 1 {
			frames = 1
		}
	}
	info.Frames = frames

	// Kind.
	switch cont {
	case cGIF, cAPNG, cWebPAnim, cAVIS:
		info.Kind = recipe.KindAnimation
		if frames == 1 {
			markStill(info)
		} else if frames <= 0 {
			d.needFrameCount = true
		}
	case cWebPStill, cAVIF, cImage:
		markStill(info)
	default:
		// An image codec with one frame (png in MOV, mjpeg) is a still even
		// when the count is only estimated; for any other codec the container
		// must state a single frame (a one-frame ProRes/H.264 MOV): the
		// duration x fps estimate is too coarse to declare a tiny clip a still.
		if frames == 1 && (stillCodecs[vs.CodecName] || nbFrames == 1) {
			markStill(info)
		} else {
			info.Kind = recipe.KindVideo
		}
	}

	// Alpha.
	d.alpha = alphaDecided
	class := alphaFromPixFmt(vs.PixFmt)
	switch {
	case vs.CodecName == "vp8" || vs.CodecName == "vp9":
		info.HasAlpha = strings.TrimSpace(vs.Tags["alpha_mode"]) == "1"
	case cont == cAVIF || cont == cAVIS:
		// The mov demuxer exposes AVIF alpha as an auxiliary single-plane
		// video stream next to the colour stream, which itself reports an
		// opaque pix_fmt. For animations the colour track is not the first
		// video stream (the one-frame primary item is), so record its v:N
		// index for the graph.
		info.ColorStream = videoStreamIndex(out.Streams, vs)
		info.AlphaStream = avifAlphaStream(out.Streams, vs)
		info.HasAlpha = class == alphaYes || info.AlphaStream > 0
	case class == alphaNone:
		info.HasAlpha = false
	case class == alphaMaybe:
		d.alpha = alphaScan
		d.assumeAlpha = true
	case scanCodecs[vs.CodecName] || cont == cGIF || cont == cAPNG || cont == cWebPAnim || cont == cWebPStill:
		d.alpha = alphaScan
		d.assumeAlpha = true
	default:
		info.HasAlpha = true
	}
	return d, nil
}

func markStill(info *recipe.ProbeInfo) {
	info.IsStill = true
	info.Kind = recipe.KindImage
	info.Frames = 1
	info.FPS = 0
	info.Duration = 0
}

// pickVideoStream returns the first video stream that is not attached cover
// art, or the first video stream, or nil.
func pickVideoStream(streams []ffStream) *ffStream {
	var first *ffStream
	for i := range streams {
		s := &streams[i]
		if s.CodecType != "video" {
			continue
		}
		if first == nil {
			first = s
		}
		if s.Disposition["attached_pic"] == 0 {
			return s
		}
	}
	return first
}

// pickAVIFColorStream returns the AVIF colour stream: among the video
// streams that are not single-plane alpha, the one with the most frames
// (libavif writes the primary still item before the animation track, so
// the first video stream of an animated AVIF holds one frame), ties going
// to the first. nil when there is no video stream.
func pickAVIFColorStream(streams []ffStream) *ffStream {
	best := mostFrames(streams, func(s *ffStream) bool { return !isAlphaPlane(s) })
	if best == nil {
		// Monochrome (yuv400) AVIF: every stream is gray, so pix_fmt cannot
		// tell colour from alpha. Most frames still wins (primary item = 1
		// frame, animation track = N); an "Alpha"-titled item never wins and
		// ties go to the first stream (libavif and ffmpeg's avif muxer both
		// write the colour track before its alpha track — the alpha TRACK of
		// an animation carries no title and default=1, so position is the
		// only tell).
		best = mostFrames(streams, func(s *ffStream) bool { return !hasAlphaTitle(s) })
	}
	if best == nil {
		return pickVideoStream(streams)
	}
	return best
}

// mostFrames returns the video stream with the highest nb_frames among those
// ok accepts, ties going to the first; nil when none is accepted.
func mostFrames(streams []ffStream, ok func(*ffStream) bool) *ffStream {
	var best *ffStream
	bestFrames := -1
	for i := range streams {
		s := &streams[i]
		if s.CodecType != "video" || !ok(s) {
			continue
		}
		if n := parseIntFlex(string(s.NbFrames)); n > bestFrames {
			best, bestFrames = s, n
		}
	}
	return best
}

// videoStreamIndex returns the "v:N" index of s among the video streams (0
// when s is the first video stream or not found).
func videoStreamIndex(streams []ffStream, s *ffStream) int {
	vIndex := -1
	for i := range streams {
		if streams[i].CodecType != "video" {
			continue
		}
		vIndex++
		if &streams[i] == s {
			return vIndex
		}
	}
	return 0
}

// avifAlphaStream returns the video-stream index ("v:N") of the alpha plane
// that belongs to the colour stream main: a single-plane stream of the same
// size and frame count; 0 when there is none. The alpha item/track follows
// its colour stream in libavif's layout, but any matching stream is
// accepted.
func avifAlphaStream(streams []ffStream, main *ffStream) int {
	mainFrames := parseIntFlex(string(main.NbFrames))
	vIndex := -1
	for i := range streams {
		s := &streams[i]
		if s.CodecType != "video" {
			continue
		}
		vIndex++
		if s == main || !isAlphaPlane(s) {
			continue
		}
		if s.Width != main.Width || s.Height != main.Height {
			continue
		}
		if n := parseIntFlex(string(s.NbFrames)); n > 0 && mainFrames > 0 && n != mainFrames {
			continue
		}
		return vIndex
	}
	return 0
}

// isAlphaPlane reports whether a stream looks like an AVIF auxiliary alpha
// plane: a gray pix_fmt, or the "Alpha" title the mov demuxer copies from
// the HEIF item name.
func isAlphaPlane(s *ffStream) bool {
	p := strings.ToLower(strings.TrimSpace(s.PixFmt))
	return strings.HasPrefix(p, "gray") || hasAlphaTitle(s)
}

// hasAlphaTitle reports the "Alpha" title the mov demuxer copies from the
// HEIF item name (alpha TRACKS of animations carry no title).
func hasAlphaTitle(s *ffStream) bool {
	return strings.EqualFold(strings.TrimSpace(s.Tags["title"]), "alpha")
}

// parseRate parses "num/den" (or a plain number) into fps; 0 when invalid.
func parseRate(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		num, err1 := strconv.ParseFloat(s[:i], 64)
		den, err2 := strconv.ParseFloat(s[i+1:], 64)
		if err1 != nil || err2 != nil || den == 0 || num <= 0 {
			return 0
		}
		return num / den
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return f
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return f
}

func parseIntFlex(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt32 {
		return 0
	}
	return int(n)
}

func firstPositive(vals ...float64) float64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// maxAnimFPS is the highest r_frame_rate accepted as a real cadence for
// GIF/APNG/WebP/AVIF animations. Above it (100/1 for GIF centiseconds,
// 1000/1 for WebP milliseconds) the value is the demuxer's tick rate, which
// ffmpeg's rfps estimator falls back to for 1–2-frame or irregular files.
const maxAnimFPS = 60

// animationFPS picks the frame rate of a GIF/APNG/WebP/AVIF animation.
//
// r_frame_rate is ffmpeg's base-cadence estimate (10/1 for a CFR 10 fps
// GIF, 20/1 for a 20 fps GIF that holds its last frame, 25/1 for a VFR
// WebP); avg_frame_rate = frames/duration collapses for the very common
// "motion then hold" / duplicate-merged animations, and a CFR resample at
// that rate would drop most motion frames. So r wins whenever it is a
// plausible cadence and avg is only the fallback for the tick-rate case.
func animationFPS(rFPS, avgFPS float64) float64 {
	if rFPS > 0 && rFPS <= maxAnimFPS {
		return rFPS
	}
	return firstPositive(avgFPS, rFPS)
}

// displayRotation returns the Display Matrix rotation of the stream in
// degrees (ffprobe: counter-clockwise, -180..180) and whether one was found.
// The first Display Matrix entry wins; an entry of another type that still
// carries a rotation value is accepted as a fallback.
func displayRotation(vs *ffStream) (float64, bool) {
	var fallback *float64
	for i := range vs.SideDataList {
		sd := &vs.SideDataList[i]
		s := strings.TrimSpace(string(sd.Rotation))
		if s == "" || s == "N/A" {
			continue
		}
		rot, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(rot) || math.IsInf(rot, 0) {
			continue
		}
		if sd.Type == displayMatrixType {
			return rot, true
		}
		if fallback == nil {
			fallback = &rot
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return 0, false
}

// rotationSwapsDims mirrors fftools' autorotate: a rotation of 90 or 270
// degrees (either sign) becomes a transpose, which swaps width and height;
// 180 degrees (hflip+vflip) and odd angles (the rotate filter, whose output
// keeps the input size) do not.
func rotationSwapsDims(rot float64, ok bool) bool {
	if !ok {
		return false
	}
	theta := math.Mod(math.Abs(math.Round(rot)), 360)
	return math.Abs(theta-90) < 1 || math.Abs(theta-270) < 1
}

// ---- pix_fmt facts --------------------------------------------------------

var (
	planarDepthRE = regexp.MustCompile(`p(9|10|12|14|16)$`)
	grayDepthRE   = regexp.MustCompile(`^gray(9|10|12|14|16)$`)
)

// sixteenBitPacked are packed formats with 16-bit components.
var sixteenBitPacked = map[string]bool{
	"rgba64": true, "bgra64": true, "rgb48": true, "bgr48": true, "ya16": true,
	"ayuv64": true, "p016": true, "y216": true, "xv36": true, "y416": true,
}

// bitsFromPixFmt returns the component bit depth encoded in a pix_fmt name:
// …p10le → 10, …p12le → 12, rgba64/rgb48/gray16 → 16, f32 → 32, else 8.
func bitsFromPixFmt(p string) int {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return 0
	}
	p = strings.TrimSuffix(strings.TrimSuffix(p, "le"), "be")
	switch {
	case strings.HasSuffix(p, "f32"):
		return 32
	case strings.HasSuffix(p, "f16"):
		return 16
	}
	if m := planarDepthRE.FindStringSubmatch(p); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if m := grayDepthRE.FindStringSubmatch(p); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if sixteenBitPacked[p] {
		return 16
	}
	switch p {
	case "p010", "nv20", "xv30", "y210", "x2rgb10", "x2bgr10":
		return 10
	case "p012", "xyz12", "y212", "xv48":
		return 12
	}
	return 8
}

type alphaClass int

const (
	alphaNone  alphaClass = iota // no alpha plane
	alphaYes                     // pix_fmt carries alpha
	alphaMaybe                   // palette / unknown: needs a scan
)

// alphaFromPixFmt says whether the pixel format has an alpha component.
func alphaFromPixFmt(p string) alphaClass {
	p = strings.ToLower(strings.TrimSpace(p))
	switch {
	case p == "":
		return alphaMaybe
	case p == "pal8":
		return alphaMaybe
	case strings.HasPrefix(p, "yuva"),
		strings.HasPrefix(p, "rgba"), strings.HasPrefix(p, "bgra"),
		strings.HasPrefix(p, "argb"), strings.HasPrefix(p, "abgr"),
		strings.HasPrefix(p, "gbrap"),
		strings.HasPrefix(p, "ya8"), strings.HasPrefix(p, "ya16"),
		strings.HasPrefix(p, "ayuv"), strings.HasPrefix(p, "vuya"),
		strings.HasPrefix(p, "gbrapf"):
		return alphaYes
	}
	return alphaNone
}

// ---- process-backed helpers ----------------------------------------------

// maxScanPixels bounds the alpha scan output held in memory (1 byte per
// pixel): 64 Mi pixels ≈ 64 MB, e.g. 60 frames of 1024x1024 or 7 of 4K.
const maxScanPixels = 64 << 20

// scanFrameBudget lowers maxFrames for large frames so the scan output stays
// within maxScanPixels (never below 2 frames).
func scanFrameBudget(maxFrames, w, h int) int {
	px := w * h
	if px <= 0 {
		return maxFrames
	}
	if n := maxScanPixels / px; n < maxFrames {
		return max(n, 2)
	}
	return maxFrames
}

// scanAlpha decodes up to maxFrames frames to 8-bit alpha and reports whether
// any pixel is not fully opaque.
func scanAlpha(ctx context.Context, ffmpeg, path string, maxFrames int) (bool, error) {
	args := append(append([]string{}, ffmpegPrefix...), enc.AlphaScanArgs(path, maxFrames)...)
	out, err := ffrun.RunOutput(ctx, ffmpeg, args)
	if err != nil {
		return false, err
	}
	if len(out) == 0 {
		return false, errors.New("alpha scan produced no data")
	}
	return anyBelow255(out), nil
}

// anyBelow255 reports whether any byte in b is < 255.
func anyBelow255(b []byte) bool {
	for _, v := range b {
		if v != 0xFF {
			return true
		}
	}
	return false
}

// countFrames decodes the whole file with progress reporting and returns the
// final frame count.
func countFrames(ctx context.Context, ffmpeg, path string) (int, error) {
	var last atomic.Int64
	err := ffrun.RunFFmpeg(ctx, ffmpeg, enc.FrameCountArgs(path), func(p ffrun.Progress) {
		if int64(p.Frame) > last.Load() {
			last.Store(int64(p.Frame))
		}
	})
	if err != nil {
		return 0, err
	}
	n := last.Load()
	if n <= 0 {
		return 0, errors.New("no frames counted")
	}
	return int(n), nil
}
