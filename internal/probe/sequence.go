package probe

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	// Header decoders for the stdlib fast path of sequenceFrameFacts.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

const (
	// DefaultSequenceDelayMS is the per-frame duration of an image sequence
	// when the client did not say (10 fps).
	DefaultSequenceDelayMS = 100
	// maxSequenceDimSamples bounds how many frames are opened to find the
	// largest frame size; beyond it the sequence is assumed uniform.
	maxSequenceDimSamples = 200
	// maxSequenceAlphaSamples bounds the per-file alpha scans (spread over
	// the frames whose header admits alpha at all).
	maxSequenceAlphaSamples = 5
	// sequenceFormat is the ProbeInfo.Format of image sequences (the ffmpeg
	// demuxer that reads them).
	sequenceFormat = "image2"
	// sequenceNameDigits is the zero-padded width of a frame number in a
	// sequence blob dir (store.SequencePattern: "%06d.<ext>").
	sequenceNameDigits = 6
)

// stdlibDimsExts are the frame extensions whose header stdlib image can read
// without a process.
var stdlibDimsExts = map[string]bool{"png": true, "jpg": true, "jpeg": true, "gif": true}

// frameFacts is what the header of one sampled frame tells.
type frameFacts struct {
	index  int // position in the sequence
	w, h   int
	alphaP bool // the pixel format has an alpha plane (a scan decides whether it is used)
}

// probeSequence implements ProbeSequence.
func probeSequence(ctx context.Context, tools ffrun.Tools, dir string, delayMS int) (recipe.ProbeInfo, error) {
	if tools.FFprobe == "" {
		return recipe.ProbeInfo{}, errors.New("probe: ffprobe is not available")
	}
	if delayMS <= 0 {
		delayMS = DefaultSequenceDelayMS
	}
	files, ext, err := listSequenceFrames(dir)
	if err != nil {
		return recipe.ProbeInfo{}, err
	}

	// Codec facts from the first frame.
	raw, err := ffrun.RunOutput(ctx, tools.FFprobe, enc.ProbeArgs(files[0]))
	if err != nil {
		return recipe.ProbeInfo{}, fmt.Errorf("ffprobe (frame 1): %w", err)
	}
	out, err := parseOutput(raw)
	if err != nil {
		return recipe.ProbeInfo{}, err
	}
	vs := pickVideoStream(out.Streams)
	if vs == nil {
		return recipe.ProbeInfo{}, ErrNoVideo
	}
	if vs.Width <= 0 || vs.Height <= 0 {
		return recipe.ProbeInfo{}, fmt.Errorf("probe: frame 1 has no dimensions (%dx%d)", vs.Width, vs.Height)
	}

	count := len(files)
	fps, duration := sequenceTiming(count, delayMS)
	info := recipe.ProbeInfo{
		Format:   sequenceFormat,
		Codec:    vs.CodecName,
		Profile:  strings.TrimSpace(string(vs.Profile)),
		PixFmt:   vs.PixFmt,
		Bits:     bitsFromPixFmt(vs.PixFmt),
		Width:    vs.Width,
		Height:   vs.Height,
		FPS:      fps,
		Duration: duration,
		Frames:   count,
		Kind:     recipe.KindSequence,
		Sequence: &recipe.SequenceInfo{Count: count, Pattern: sequencePattern(ext), DelayMS: delayMS},
	}
	if info.Profile == "unknown" {
		info.Profile = ""
	}

	// Per-frame header facts: largest frame, uniformity, alpha candidates.
	// This doubles as the "can the render open this?" check: the ffprobe
	// fallback reads the frames through the image2 pattern, exactly how the
	// render's ffmpeg opens a sequence, so when neither the stdlib nor that
	// ffprobe can read them every later still/proxy/render would fail too —
	// reject the sequence instead of assuming it is uniform. (The wrapped
	// error keeps ffprobe's own exit error, and the empty-pattern case
	// carries the "has no dimensions" marker, so the server answers 422 and
	// discards the blob.)
	facts, err := sequenceFrameFacts(ctx, tools.FFprobe, dir, files, ext)
	if err != nil {
		if ctx.Err() != nil {
			return recipe.ProbeInfo{}, ctx.Err()
		}
		return recipe.ProbeInfo{}, fmt.Errorf("probe: sequence %s: frames unreadable: %w", dir, err)
	}
	if len(facts) > 0 {
		info.Width, info.Height, info.Sequence.Mixed = largestFrame(facts)
	}

	// Alpha: a plane must exist in some frame's pixel format (the first
	// frame's ffprobe pix_fmt or any sampled header) and a scan of those
	// frames must find a pixel that uses it.
	firstAlpha := alphaFromPixFmt(vs.PixFmt) != alphaNone
	candidates := alphaCandidates(facts, firstAlpha, count)
	if len(candidates) > 0 {
		has, err := sequenceHasAlpha(ctx, tools.FFmpeg, files, candidates)
		if err != nil {
			return recipe.ProbeInfo{}, err
		}
		info.HasAlpha = has
	}
	return info, nil
}

// sequenceTiming returns the FPS and Duration an image sequence of count
// frames at delayMS per frame reports: the image2 -framerate the render
// actually uses (1000/delay rounded to 3 decimals, graph.SequenceFPS — 33 ms
// is 30.303 fps, not 30.30303…) and count/FPS, so Frames, Duration and FPS
// agree with graph.Plan exactly and Duration*FPS floors back to count (the
// unrounded rate with count*delay/1000 did not: 34 * 0.033 * 30.303 =
// 33.99997). delayMS <= 0 means DefaultSequenceDelayMS.
func sequenceTiming(count, delayMS int) (fps, duration float64) {
	if delayMS <= 0 {
		delayMS = DefaultSequenceDelayMS
	}
	fps = graph.SequenceFPS(delayMS)
	return fps, float64(count) / fps
}

// listSequenceFrames returns the frame files of a sequence dir in order and
// their shared extension. It accepts only the store layout (six digits, one
// extension) and ignores anything else in the directory.
func listSequenceFrames(dir string) ([]string, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("probe: read sequence dir: %w", err)
	}
	var files []string
	ext := ""
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		dot := strings.IndexByte(name, '.')
		if dot != sequenceNameDigits || dot == len(name)-1 {
			continue
		}
		if _, err := strconv.Atoi(name[:dot]); err != nil {
			continue
		}
		if ext == "" {
			ext = name[dot+1:]
		} else if name[dot+1:] != ext {
			return nil, "", fmt.Errorf("probe: sequence %s mixes extensions %q and %q", dir, ext, name[dot+1:])
		}
		files = append(files, filepath.Join(dir, name))
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("probe: %s holds no sequence frames", dir)
	}
	return files, ext, nil
}

// sequencePattern is the image2 pattern of a sequence dir (mirrors
// store.SequencePattern without importing the store).
func sequencePattern(ext string) string { return "%0" + strconv.Itoa(sequenceNameDigits) + "d." + ext }

// sampleIndices returns up to n indices spread evenly over [0, count),
// always including the first and (when n >= 2) the last.
func sampleIndices(count, n int) []int {
	if count <= 0 || n <= 0 {
		return nil
	}
	if count <= n {
		idx := make([]int, count)
		for i := range idx {
			idx[i] = i
		}
		return idx
	}
	if n == 1 {
		return []int{0}
	}
	idx := make([]int, 0, n)
	for i := 0; i < n; i++ {
		idx = append(idx, i*(count-1)/(n-1))
	}
	return idx
}

// sequenceFrameFacts reads the headers of sampled frames. png/jpeg/gif go
// through stdlib image (spread over the whole sequence); other formats —
// and stdlib formats whose headers the stdlib cannot read (an exotic JPEG
// variant, foreign bytes behind the extension) — through one ffprobe over
// the image2 pattern (the first frames up to the sample budget), which is
// authoritative because it opens the sequence the way the render will.
func sequenceFrameFacts(ctx context.Context, ffprobe, dir string, files []string, ext string) ([]frameFacts, error) {
	if stdlibDimsExts[strings.ToLower(ext)] {
		if facts, err := stdlibSequenceFrameFacts(files); err == nil {
			return facts, nil
		}
	}
	return ffprobeFrameFacts(ctx, ffprobe, filepath.Join(dir, sequencePattern(ext)), min(len(files), maxSequenceDimSamples))
}

// stdlibSequenceFrameFacts samples the sequence's frame headers with stdlib
// image; any unreadable header fails the whole pass.
func stdlibSequenceFrameFacts(files []string) ([]frameFacts, error) {
	var facts []frameFacts
	for _, i := range sampleIndices(len(files), maxSequenceDimSamples) {
		f, err := stdlibFrameFacts(files[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(files[i]), err)
		}
		f.index = i
		facts = append(facts, f)
	}
	return facts, nil
}

// stdlibFrameFacts reads an image header with image.DecodeConfig. Whether
// the frame is worth an alpha scan is decided from what ffmpeg's decoder
// would produce for that format, not from the colour model alone
// (headerAdmitsAlpha).
func stdlibFrameFacts(path string) (frameFacts, error) {
	f, err := os.Open(path)
	if err != nil {
		return frameFacts{}, err
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return frameFacts{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return frameFacts{}, fmt.Errorf("no dimensions (%dx%d)", cfg.Width, cfg.Height)
	}
	return frameFacts{w: cfg.Width, h: cfg.Height, alphaP: headerAdmitsAlpha(path, format, cfg.ColorModel)}, nil
}

// headerAdmitsAlpha reports whether a frame of the decoded format can carry
// transparency that ffmpeg would decode. The stdlib colour model alone
// under-reports two cases:
//   - GIF: image/gif's DecodeConfig returns the global colour table (all
//     opaque); the transparent index lives in each frame's Graphic Control
//     Extension, which a config-only decode never reads. ffmpeg's gif
//     decoder always emits bgra, so every GIF frame is a candidate.
//   - PNG: image/png reports RGBAModel/GrayModel for truecolour/gray files
//     and stops parsing at IHDR for them, so a tRNS colour key goes unseen;
//     a cheap chunk walk (pngHasTRNS) decides those. (Paletted PNGs are
//     fine: DecodeConfig folds tRNS into the palette's alpha.)
func headerAdmitsAlpha(path, format string, m color.Model) bool {
	if modelHasAlpha(m) {
		return true
	}
	switch format {
	case "gif":
		return true
	case "png":
		switch m {
		case color.RGBAModel, color.RGBA64Model, color.GrayModel, color.Gray16Model:
			return pngHasTRNS(path)
		}
	}
	return false
}

// modelHasAlpha reports whether a decoded header's colour model can carry
// transparency: the NRGBA models image/png uses for colour types with an
// alpha channel, or a palette with a translucent entry (PNG paletted tRNS).
// Format-level cases the model cannot show (GIF per-frame transparency, PNG
// truecolour/gray tRNS colour keys) live in headerAdmitsAlpha.
func modelHasAlpha(m color.Model) bool {
	switch m {
	case color.NRGBAModel, color.NRGBA64Model:
		return true
	}
	if pal, ok := m.(color.Palette); ok {
		for _, c := range pal {
			if _, _, _, a := c.RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}

// maxPNGHeaderChunks bounds pngHasTRNS's chunk walk; tRNS legally precedes
// IDAT, which follows within a handful of ancillary chunks in any sane file.
const maxPNGHeaderChunks = 64

// pngHasTRNS reports whether a PNG file carries a tRNS chunk before its
// image data — the transparency of truecolour/gray PNGs, which DecodeConfig's
// colour model cannot show. It walks the chunk list after the 8-byte
// signature (length, type, skip data+CRC), stdlib-only and reading a few
// hundred bytes; any read problem means "no".
func pngHasTRNS(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(8, io.SeekStart); err != nil {
		return false
	}
	var hdr [8]byte
	for range maxPNGHeaderChunks {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return false
		}
		length := binary.BigEndian.Uint32(hdr[:4])
		switch string(hdr[4:8]) {
		case "tRNS":
			return true
		case "IDAT", "IEND":
			return false
		}
		if _, err := f.Seek(int64(length)+4, io.SeekCurrent); err != nil { // data + CRC
			return false
		}
	}
	return false
}

// ffprobeFrameFacts decodes the first n frames of the image2 pattern:
// ffprobe -f image2 -start_number 1 -i pattern -select_streams v:0
// -show_entries frame=width,height,pix_fmt -read_intervals %+#n.
func ffprobeFrameFacts(ctx context.Context, ffprobe, pattern string, n int) ([]frameFacts, error) {
	args := []string{
		"-v", "error",
		"-f", "image2", "-start_number", "1",
		"-i", pattern,
		"-select_streams", "v:0",
		"-show_entries", "frame=width,height,pix_fmt",
		"-read_intervals", "%+#" + strconv.Itoa(max(n, 1)),
		"-print_format", "json",
	}
	raw, err := ffrun.RunOutput(ctx, ffprobe, args)
	if err != nil {
		return nil, fmt.Errorf("ffprobe (frames): %w", err)
	}
	var out struct {
		Frames []struct {
			Width  int    `json:"width"`
			Height int    `json:"height"`
			PixFmt string `json:"pix_fmt"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ffprobe (frames): decode json: %w", err)
	}
	facts := make([]frameFacts, 0, len(out.Frames))
	for i, f := range out.Frames {
		if f.Width > 0 && f.Height > 0 {
			facts = append(facts, frameFacts{index: i, w: f.Width, h: f.Height, alphaP: alphaFromPixFmt(f.PixFmt) != alphaNone})
		}
	}
	if len(facts) == 0 {
		// ffprobe ran but decoded nothing (a frame format image2 has no
		// decoder for, e.g. AVIF): the wording carries the "has no
		// dimensions" marker the server maps to an unreadable source.
		return nil, errors.New("ffprobe (frames): pattern has no dimensions (no decodable frames)")
	}
	return facts, nil
}

// largestFrame returns the largest width/height among the sampled frames
// and whether any sampled frame differs from it.
func largestFrame(facts []frameFacts) (w, h int, mixed bool) {
	for _, f := range facts {
		w, h = max(w, f.w), max(h, f.h)
	}
	for _, f := range facts {
		if f.w != w || f.h != h {
			return w, h, true
		}
	}
	return w, h, false
}

// alphaCandidates picks up to maxSequenceAlphaSamples frame indices worth
// an alpha scan: frames whose header admits alpha (spread over them), or —
// when no headers were read — the first frame plus a spread when its
// ffprobe pix_fmt admits alpha. Empty means "no frame can carry alpha".
func alphaCandidates(facts []frameFacts, firstAlpha bool, count int) []int {
	var possible []int
	if len(facts) == 0 {
		if !firstAlpha {
			return nil
		}
		return sampleIndices(count, maxSequenceAlphaSamples)
	}
	for _, f := range facts {
		if f.alphaP || (f.index == 0 && firstAlpha) {
			possible = append(possible, f.index)
		}
	}
	if len(possible) == 0 {
		return nil
	}
	picks := sampleIndices(len(possible), maxSequenceAlphaSamples)
	out := make([]int, 0, len(picks))
	for _, p := range picks {
		out = append(out, possible[p])
	}
	return out
}

// sequenceHasAlpha scans the candidate frames (one ffmpeg alpha scan per
// file) and reports whether any pixel is not opaque. Without ffmpeg, or
// when every scan fails, the alpha plane is assumed to be used. A cancelled
// context is returned as the error.
func sequenceHasAlpha(ctx context.Context, ffmpeg string, files []string, candidates []int) (bool, error) {
	if ffmpeg == "" {
		return true, nil
	}
	scanned := 0
	for _, i := range candidates {
		if i < 0 || i >= len(files) {
			continue
		}
		has, err := scanAlpha(ctx, ffmpeg, files[i], 1)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			log.Printf("probe: alpha scan of %s failed: %v", files[i], err)
			continue
		}
		scanned++
		if has {
			return true, nil
		}
	}
	return scanned == 0, nil
}
