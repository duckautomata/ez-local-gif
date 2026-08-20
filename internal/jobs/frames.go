package jobs

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Frame extraction (Output.Format "frames"): every master frame as an image
// file (PNG RGBA by default, JPEG flattened onto the matte, or lossless
// WebP) plus a delays.json with the per-frame timing (DESIGN.md §4.2) and
// frames.zip (STORE, built in Go) holding both. No lint.

const (
	// MaxExtractFrames bounds a frame export (files in the result dir,
	// entries in the manifest, zip size).
	MaxExtractFrames = 2000
	// framePrefix names the frame files: f00001.png.
	framePrefix = "f"
	// frameNumberWidth is the zero padding of frame numbers in file names.
	frameNumberWidth = 5
	// framesZipName is the archive of every frame.
	framesZipName = "frames.zip"
	// framesDelaysName is the per-frame timing table delivered next to the
	// frames and inside frames.zip (DESIGN.md §4.2).
	framesDelaysName = "delays.json"
	// framesDirName is the scratch directory frames are written to.
	framesDirName = "frames"
	// pngFramesCompression is the -compression_level of delivered PNG frames
	// (6: smaller than the level 1 intermediates, still fast).
	pngFramesCompression = 6
)

// Frame formats (Output.FrameFormat).
const (
	FrameFormatPNG  = "png"
	FrameFormatJPEG = "jpeg"
	FrameFormatWebP = "webp"
)

// tooManyFramesMsg is the user-facing refusal above MaxExtractFrames.
func tooManyFramesMsg(n int) string {
	return fmt.Sprintf("too many frames (%d, the limit is %d); trim or lower fps", n, MaxExtractFrames)
}

// frameFormatOf normalises Output.FrameFormat ("" = png).
func frameFormatOf(out recipe.Output) (string, error) {
	switch f := strings.ToLower(strings.TrimSpace(out.FrameFormat)); f {
	case "", FrameFormatPNG:
		return FrameFormatPNG, nil
	case FrameFormatJPEG, "jpg":
		return FrameFormatJPEG, nil
	case FrameFormatWebP:
		return FrameFormatWebP, nil
	default:
		return "", fmt.Errorf("%w: unsupported frame format %q (png, jpeg, webp)", ErrInvalidRecipe, out.FrameFormat)
	}
}

// frameTiming is one row of delays.json.
type frameTiming struct {
	Index      int `json:"index"`      // 1-based, matches the f%05d file names
	TMs        int `json:"tMs"`        // frame start time
	DurationMs int `json:"durationMs"` // display duration
}

// frameTimings computes the per-frame timing table for n frames at fps.
// Start times are rounded from the exact positions and each duration is the
// gap to the next start, so the cumulative timing is exact. Unknown fps (0)
// gives all-zero times.
func frameTimings(n int, fps float64) []frameTiming {
	at := func(i int) int {
		if fps <= 0 {
			return 0
		}
		return int(math.Round(float64(i) * 1000 / fps))
	}
	out := make([]frameTiming, n)
	for i := range out {
		out[i] = frameTiming{Index: i + 1, TMs: at(i), DurationMs: at(i+1) - at(i)}
	}
	return out
}

// produceFrames writes every master frame into scratch/frames, adds
// delays.json, zips everything and returns the archive, the timing table and
// one produced per frame.
func (m *Manager) produceFrames(ctx context.Context, j *job, scratch string, master enc.Master, out recipe.Output) ([]produced, error) {
	if master.Frames > MaxExtractFrames {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRecipe, tooManyFramesMsg(master.Frames))
	}
	ff, err := frameFormatOf(out)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(scratch, framesDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("frames dir: %w", err)
	}
	ext := extFor(ff)
	pattern := filepath.Join(dir, framePrefix+"%0"+fmt.Sprint(frameNumberWidth)+"d."+ext)
	var args []string
	switch ff {
	case FrameFormatPNG:
		args = enc.PNGFramesArgs(master, nil, pattern, pngFramesCompression)
	case FrameFormatJPEG:
		args = enc.JPEGFramesArgs(master, nil, out.Matte, out.Quality, pattern)
	case FrameFormatWebP:
		args = enc.WebPFramesArgs(master, nil, pattern)
	}
	m.progress(j, pctEncodeStart, fmt.Sprintf("extracting %d frames as %s", master.Frames, ff))
	err = ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, func(p ffrun.Progress) {
		frac := progressFraction(p, master.Frames, 0)
		m.progress(j, pctEncodeStart+frac*(pctEncodeEnd-pctEncodeStart)*0.8, fmt.Sprintf("extracting frame %d/%d", p.Frame, master.Frames))
	})
	if err != nil {
		return nil, fmt.Errorf("frame export: %w", err)
	}
	files, err := listFrameFiles(dir, "."+ext)
	if err != nil {
		return nil, err
	}
	if len(files) > MaxExtractFrames {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRecipe, tooManyFramesMsg(len(files)))
	}

	timings := frameTimings(len(files), master.FPS)
	delaysPath := filepath.Join(dir, framesDelaysName)
	delaysData, err := json.MarshalIndent(timings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", framesDelaysName, err)
	}
	if err := os.WriteFile(delaysPath, delaysData, 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", framesDelaysName, err)
	}

	m.progress(j, pctEncodeStart+(pctEncodeEnd-pctEncodeStart)*0.85, fmt.Sprintf("zipping %d frames", len(files)))
	zipPath := filepath.Join(dir, framesZipName)
	zipFiles := append(append(make([]string, 0, len(files)+1), files...), delaysPath)
	if err := writeStoreZip(zipPath, zipFiles); err != nil {
		return nil, err
	}

	// The manifest lists the archive first so clients find it without
	// walking every frame, then the timing table, then the frames in order.
	items := make([]produced, 0, len(files)+2)
	items = append(items, produced{
		path:   zipPath,
		name:   framesZipName,
		format: "zip",
		kind:   FileKindArchive,
		desc:   fmt.Sprintf("%d frames (%s)", len(files), ff),
		width:  master.Width,
		height: master.Height,
		frames: len(files),
		fps:    master.FPS,
	})
	if master.FPS > 0 {
		items[0].duration = float64(len(files)) / master.FPS
	}
	items = append(items, produced{
		path:   delaysPath,
		name:   framesDelaysName,
		format: "json",
		desc:   "per-frame timing",
		frames: len(files),
		fps:    master.FPS,
	})
	for i, f := range files {
		items = append(items, produced{
			path:   f,
			name:   filepath.Base(f),
			format: ff,
			kind:   FileKindFrame,
			index:  i + 1,
			desc:   fmt.Sprintf("frame %d (%.2f s)", i+1, float64(timings[i].TMs)/1000),
			width:  master.Width,
			height: master.Height,
			frames: 1,
		})
	}
	return items, nil
}

// writeStoreZip writes files into a zip at zipPath with the STORE method
// (the frames are already compressed images) and their base names as entry
// names.
func writeStoreZip(zipPath string, files []string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(zipPath), err)
	}
	zw := zip.NewWriter(out)
	now := time.Now()
	for _, f := range files {
		if err := addStored(zw, f, now); err != nil {
			zw.Close()
			out.Close()
			return fmt.Errorf("zip %s: %w", filepath.Base(f), err)
		}
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return fmt.Errorf("finish %s: %w", filepath.Base(zipPath), err)
	}
	return out.Close()
}

// addStored appends one file to zw uncompressed.
func addStored(zw *zip.Writer, path string, mod time.Time) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	hdr := &zip.FileHeader{Name: filepath.Base(path), Method: zip.Store, Modified: mod}
	hdr.SetMode(0o644)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}
