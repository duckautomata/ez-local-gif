package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Phase 2 encoders: APNG (RGBA and indexed 8-bit alpha), AVIF via avifenc,
// static PNG/JPEG. Each path encodes from the RGBA master (optionally
// through an enc.Variant for fit rungs), lints with the matching
// discordlint checker and hands a produced file back. Shared by the single
// output path and the fit search (fit.go), which is why the encoders take a
// tag for unique scratch names and do their own progress reporting only
// when tag is "".

// Encoder defaults (DESIGN.md §4.2).
const (
	// DefaultAVIFQuality is avifenc -q when Output.Quality is 0.
	DefaultAVIFQuality = 60
	// avifAlphaQuality is avifenc --qalpha.
	avifAlphaQuality = 90
	// avifSpeed is avifenc -s (8 = fast; 6 for stills per §4.2 is slower
	// for little gain on emote-sized images).
	avifSpeed = 8
	// avifCodecSVT is the avifenc -c value for SVT-AV1.
	avifCodecSVT = "svt"
	// oxipngLevel is the oxipng -o level for delivered PNG/APNG files.
	oxipngLevel = 2
	// pngquantStaticSpeed is pngquant --speed for static PNG output.
	pngquantStaticSpeed = 3
	// tileSheetMaxSide refuses sprite sheets pngquant cannot reasonably hold
	// (16384 px per side, PNG/pngquant sanity; enc.TileGrid keeps the width
	// under it, the height is checked here).
	tileSheetMaxSide = 16384
)

// ---- variants ----------------------------------------------------------------

// variantFor builds the enc.Variant that realises a fit rung (fps/size)
// against the master, or nil when the rung equals the master. An fps at or
// above the master's and a width at or above the master's are "unchanged".
func variantFor(master enc.Master, fps float64, w, h int) *enc.Variant {
	v := enc.Variant{}
	if fps > 0 && master.FPS > 0 && fps < master.FPS && master.Frames > 1 {
		v.FPS = fps
	}
	if w > 0 && w < master.Width {
		v.Width, v.Height = w, h
	} else if h > 0 && h < master.Height && w == 0 {
		v.Height = h
	}
	if v == (enc.Variant{}) {
		return nil
	}
	return &v
}

// variantFPS is the frame rate a variant encodes at.
func variantFPS(master enc.Master, v *enc.Variant) float64 {
	return enc.VariantMaster(master, v).FPS
}

// variantFrames is the number of frames the variant has (ffmpeg's fps
// filter semantics, enc.VariantMaster).
func variantFrames(master enc.Master, v *enc.Variant) int {
	return enc.VariantMaster(master, v).Frames
}

// variantDims is the variant's frame size.
func variantDims(master enc.Master, v *enc.Variant) (w, h int) {
	vm := enc.VariantMaster(master, v)
	return vm.Width, vm.Height
}

// variantKey identifies a variant for per-variant caches.
func variantKey(v *enc.Variant) string {
	if v == nil {
		return "master"
	}
	return fmt.Sprintf("f%.3f-w%d-h%d", v.FPS, v.Width, v.Height)
}

// ---- APNG --------------------------------------------------------------------

// produceAPNG is the APNG path without fit: Colors 0 → RGBA (-c:v apng),
// Colors > 0 → indexed 8-bit-alpha pipeline; then oxipng, LintAPNG.
func (m *Manager) produceAPNG(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) (produced, error) {
	path := filepath.Join(scratch, "enc.png")
	if out.Colors > 0 {
		m.progress(j, pctEncodeStart, fmt.Sprintf("apng: quantising to %d colours", out.Colors))
		sheet, err := m.renderTileSheet(ctx, scratch, "", master, nil)
		if err != nil {
			return produced{}, err
		}
		err = m.quantizeUntile(ctx, scratch, "", sheet, out.Colors, out.Loop, path)
		os.Remove(sheet.path)
		if err != nil {
			return produced{}, err
		}
	} else {
		m.progress(j, pctEncodeStart, "apng: encoding RGBA")
		if err := m.encodeRGBAAPNG(ctx, master, nil, out.Loop, path); err != nil {
			return produced{}, err
		}
	}
	m.progress(j, pctEncodeEnd, "apng: oxipng")
	m.oxipng(ctx, path)
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(path)
	if err != nil {
		return produced{}, fmt.Errorf("read encoded output: %w", err)
	}
	report, err := discordlint.LintAPNG(data, target)
	if err != nil {
		return produced{}, fmt.Errorf("lint: %w", err)
	}
	applyMasterAlpha(&report, master)
	return m.finalFile(scratch, recipe.FormatAPNG, data, &report)
}

// encodeRGBAAPNG writes a truecolour APNG of the master (through v).
func (m *Manager) encodeRGBAAPNG(ctx context.Context, master enc.Master, v *enc.Variant, loop int, outPath string) error {
	args := enc.APNGArgs(master, enc.APNGOptions{Loop: loop, Variant: v}, outPath)
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, nil); err != nil {
		return fmt.Errorf("apng encode: %w", err)
	}
	return nil
}

// tileSheet is the rendered sprite sheet of one variant (shared by every
// colour count the fit search probes on that rung).
type tileSheet struct {
	path       string
	cols, rows int
	frames     int     // frames in the sheet (variantFrames)
	fps        float64 // rate of the untiled animation
}

// renderTileSheet renders every frame of the master (through v) into one
// RGBA sprite sheet PNG so pngquant can build a single shared palette.
func (m *Manager) renderTileSheet(ctx context.Context, scratch, tag string, master enc.Master, v *enc.Variant) (*tileSheet, error) {
	frames := variantFrames(master, v)
	if frames <= 0 {
		return nil, errors.New("apng: the variant has no frames")
	}
	w, h := variantDims(master, v)
	cols, rows := enc.TileGrid(frames, w, h)
	if cols <= 0 || rows <= 0 || cols*rows < frames {
		return nil, fmt.Errorf("apng: no tile grid for %d frames of %dx%d", frames, w, h)
	}
	if cols*w > tileSheetMaxSide || rows*h > tileSheetMaxSide {
		return nil, fmt.Errorf("%w: %d frames of %dx%d do not fit one sprite sheet for palette quantisation; lower the fps, trim or resize (or use truecolour APNG: colours 0)", ErrInvalidRecipe, frames, w, h)
	}
	sheet := &tileSheet{path: filepath.Join(scratch, "tile"+tag+".png"), cols: cols, rows: rows, frames: frames, fps: variantFPS(master, v)}
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, enc.TileArgs(master, v, cols, rows, sheet.path), nil); err != nil {
		return nil, fmt.Errorf("apng tile sheet: %w", err)
	}
	return sheet, nil
}

// quantizeUntile quantises a sprite sheet to colors (pngquant, ordered — no
// error diffusion so frames do not shimmer) and slices it back into an
// indexed APNG at outPath.
func (m *Manager) quantizeUntile(ctx context.Context, scratch, tag string, sheet *tileSheet, colors, loop int, outPath string) error {
	if m.tools.Pngquant == "" {
		return errors.New("indexed APNG needs pngquant, which is not available on this server (use truecolour APNG: colours 0)")
	}
	q := filepath.Join(scratch, "quant"+tag+".png")
	if err := ffrun.Run(ctx, m.tools.Pngquant, enc.PngquantArgs(sheet.path, q, colors, false, 0)); err != nil {
		return fmt.Errorf("pngquant: %w", err)
	}
	defer os.Remove(q)
	args := enc.UntileAPNGArgs(q, sheet.cols, sheet.rows, sheet.frames, sheet.fps, enc.APNGOptions{Colors: colors, Loop: loop}, outPath)
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, nil); err != nil {
		return fmt.Errorf("apng untile: %w", err)
	}
	return nil
}

// oxipng optimises a PNG/APNG in place when oxipng is available: it works
// on a copy and only replaces the original when the result is smaller, so
// a failure (or a no-op) never costs the file. Best effort; errors are
// logged.
func (m *Manager) oxipng(ctx context.Context, path string) {
	if m.tools.Oxipng == "" {
		return
	}
	tmp := path + ".oxi.png"
	if err := copyFile(path, tmp); err != nil {
		log.Printf("jobs: oxipng copy: %v", err)
		return
	}
	defer os.Remove(tmp)
	if err := ffrun.Run(ctx, m.tools.Oxipng, enc.OxipngArgs(tmp, oxipngLevel)); err != nil {
		if ctx.Err() == nil {
			log.Printf("jobs: oxipng: %v", err)
		}
		return
	}
	before, err1 := os.Stat(path)
	after, err2 := os.Stat(tmp)
	if err1 != nil || err2 != nil || after.Size() <= 0 || after.Size() >= before.Size() {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("jobs: oxipng replace: %v", err)
	}
}

// ---- AVIF --------------------------------------------------------------------

// produceAVIF writes PNG frames of the master and runs avifenc (a single
// frame → AVIFStillArgs). Lint: LintStatic("avif") for the size/emote rules,
// with the animation facts filled from the master.
func (m *Manager) produceAVIF(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) (produced, error) {
	if m.tools.Avifenc == "" {
		return produced{}, errors.New("AVIF output needs avifenc, which is not available on this server")
	}
	m.progress(j, pctEncodeStart, "avif: writing frames")
	frames, err := m.renderPNGFrames(ctx, filepath.Join(scratch, "avif-frames"), master, nil, 1)
	if err != nil {
		return produced{}, err
	}
	m.progress(j, pctEncodeStart+(pctEncodeEnd-pctEncodeStart)*0.3, fmt.Sprintf("avifenc: %d frames", len(frames)))
	path := filepath.Join(scratch, "enc.avif")
	quality := out.Quality
	if quality <= 0 {
		quality = DefaultAVIFQuality
	}
	if err := m.encodeAVIF(ctx, frames, master.FPS, m.avifOptions(master, quality, out.Loop), path); err != nil {
		return produced{}, err
	}
	os.RemoveAll(filepath.Join(scratch, "avif-frames"))
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(path)
	if err != nil {
		return produced{}, fmt.Errorf("read encoded output: %w", err)
	}
	report, err := lintAVIF(data, target, len(frames), master.FPS, out.Loop)
	if err != nil {
		return produced{}, fmt.Errorf("lint: %w", err)
	}
	applyMasterAlpha(&report, master)
	return m.finalFile(scratch, recipe.FormatAVIF, data, &report)
}

// avifOptions maps quality/loop onto enc.AVIFOptions: SVT-AV1 (much faster
// than libaom) only for opaque content of at least enc.MinSVTDim per side
// (SVT-AV1 rejects smaller frames) and only when avifenc lists it. vm is
// the master as encoded (after any variant).
func (m *Manager) avifOptions(vm enc.Master, quality, loop int) enc.AVIFOptions {
	o := enc.AVIFOptions{Quality: quality, AlphaQuality: avifAlphaQuality, Speed: avifSpeed, Loop: loop}
	if !vm.HasAlpha && vm.Width >= enc.MinSVTDim && vm.Height >= enc.MinSVTDim && m.avifencHasSVT() {
		o.Codec = avifCodecSVT
	}
	return o
}

// avifencHasSVT reports whether the avifenc version line names the SVT-AV1
// encoder ("... svt [enc]:v2.3.0").
func (m *Manager) avifencHasSVT() bool {
	v := strings.ToLower(m.ToolVersions()["avifenc"])
	return strings.Contains(v, "svt [enc]") || strings.Contains(v, "svt[enc]")
}

// renderPNGFrames writes every frame of the master (through v) as RGBA PNG
// files into dir and returns their paths in order.
func (m *Manager) renderPNGFrames(ctx context.Context, dir string, master enc.Master, v *enc.Variant, compression int) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("frames dir: %w", err)
	}
	pattern := filepath.Join(dir, "f%05d.png")
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, enc.PNGFramesArgs(master, v, pattern, compression), nil); err != nil {
		return nil, fmt.Errorf("png frames: %w", err)
	}
	return listFrameFiles(dir, ".png")
}

// listFrameFiles returns the frame files in dir with ext, sorted by name
// (zero-padded numbers sort numerically).
func listFrameFiles(dir, ext string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list frames: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ext) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, errors.New("no frames were written")
	}
	return files, nil
}

// encodeAVIF runs avifenc on the frames (still when there is one).
func (m *Manager) encodeAVIF(ctx context.Context, frames []string, fps float64, o enc.AVIFOptions, outPath string) error {
	var args []string
	if len(frames) == 1 {
		args = enc.AVIFStillArgs(frames[0], o, outPath)
	} else {
		args = enc.AVIFEncArgs(frames, fps, o, outPath)
	}
	if err := ffrun.Run(ctx, m.tools.Avifenc, args); err != nil {
		return fmt.Errorf("avifenc: %w", err)
	}
	return nil
}

// lintAVIF evaluates the static rules (size limit, emote/sticker shape) on
// an AVIF and, for an animation, overrides the frame facts with the
// encoder's own (LintStatic reports a single frame).
func lintAVIF(data []byte, target discordlint.Target, frames int, fps float64, loop int) (discordlint.Report, error) {
	report, err := discordlint.LintStatic(recipe.FormatAVIF, data, target)
	if err != nil {
		return report, err
	}
	if frames > 1 {
		report.Frames = frames
		if fps > 0 {
			report.DurationMS = int(math.Round(float64(frames) / fps * 1000))
			report.MinDelayMS = int(math.Round(1000 / fps))
		}
		report.LoopForever = loop == 0
	}
	return report, nil
}

// ---- static PNG / JPEG ------------------------------------------------------

// produceStatic encodes the master's first frame as PNG (+ pngquant — the
// §4.2 quality pass by default, an exact palette when Colors > 0 — then
// oxipng) or JPEG and lints it with LintStatic.
func (m *Manager) produceStatic(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output, target discordlint.Target) (produced, error) {
	format := strings.ToLower(out.Format)
	path := filepath.Join(scratch, "enc."+extFor(format))
	m.progress(j, pctEncodeStart, "encoding "+format)
	switch format {
	case recipe.FormatPNG:
		if err := m.encodePNGStill(ctx, scratch, "", master, nil, out.Colors, path); err != nil {
			return produced{}, err
		}
		m.oxipng(ctx, path)
	case recipe.FormatJPEG:
		if err := m.encodeJPEGStill(ctx, master, nil, out.Quality, out.Matte, path); err != nil {
			return produced{}, err
		}
	default:
		return produced{}, fmt.Errorf("%w: %q is not a static format", ErrInvalidRecipe, out.Format)
	}
	m.setStage(j, StageLint, pctLint, "checking Discord rules")
	data, err := os.ReadFile(path)
	if err != nil {
		return produced{}, fmt.Errorf("read encoded output: %w", err)
	}
	report, err := discordlint.LintStatic(format, data, target)
	if err != nil {
		return produced{}, fmt.Errorf("lint: %w", err)
	}
	if format != recipe.FormatJPEG { // a JPEG is flattened: the file truly has no alpha
		applyMasterAlpha(&report, master)
	}
	return m.finalFile(scratch, format, data, &report)
}

// encodePNGStill writes the first frame as RGBA PNG and quantises it with
// pngquant (error diffusion is fine for a still): colors > 0 → an exact
// palette; colors 0 → the DESIGN.md §4.2 default --quality 70-100 pass,
// keeping the full-colour encode when pngquant reports the quality floor is
// unreachable (exit status 99) or is not installed.
func (m *Manager) encodePNGStill(ctx context.Context, scratch, tag string, master enc.Master, v *enc.Variant, colors int, outPath string) error {
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, enc.PNGStillArgs(master, enc.StillOptions{Variant: v}, outPath), nil); err != nil {
		return fmt.Errorf("png encode: %w", err)
	}
	if m.tools.Pngquant == "" {
		if colors > 0 {
			log.Printf("jobs: pngquant not available; png keeps full colour")
		}
		return nil
	}
	q := filepath.Join(scratch, "quant"+tag+".png")
	args := enc.PngquantFileArgs(outPath, q, 0, 0)
	if colors > 0 {
		args = enc.PngquantArgs(outPath, q, colors, true, pngquantStaticSpeed)
	}
	if err := ffrun.Run(ctx, m.tools.Pngquant, args); err != nil {
		if colors <= 0 && pngquantCannotReachQuality(err) {
			return nil // §4.2: below the quality floor everywhere — keep the full-colour encode
		}
		return fmt.Errorf("pngquant: %w", err)
	}
	return os.Rename(q, outPath)
}

// pngquantCannotReachQuality reports pngquant's exit status 99: no palette
// meets the --quality floor and no output was written.
func pngquantCannotReachQuality(err error) bool {
	var xe *exec.ExitError
	return errors.As(err, &xe) && xe.ExitCode() == 99
}

// encodeJPEGStill writes the first frame flattened onto matte as JPEG.
func (m *Manager) encodeJPEGStill(ctx context.Context, master enc.Master, v *enc.Variant, quality int, matte, outPath string) error {
	o := enc.StillOptions{Quality: quality, Matte: matte, Variant: v}
	if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, enc.JPEGStillArgs(master, o, outPath), nil); err != nil {
		return fmt.Errorf("jpeg encode: %w", err)
	}
	return nil
}

// copyFile copies src to dst (truncating dst).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
