package enc

// Phase 5d (build brief 2026-10-09, "mask-prompted tracking"): the argv
// behind a MASK PROMPT of the guided model (recipe.MattePrompt.MaskFrom):
// the prompt frame's mask is the edge model's per-frame matte of that
// frame, which the sidecar takes at the frames' size — so jobs scales the
// stored matte (one %06d.png of the edge memo, 8-bit gray at the model
// square) to the tracking size once (MatteMaskScaleArgs) and sends the
// result as the track request's mask record (matte.Client.Track /
// TrackFrame). matte_5d_test.go pins the golden, matte_5d_ffmpeg_test.go
// checks the scale against a real ffmpeg.

import "strconv"

// MatteMaskScaleArgs is the ffmpeg argv that scales ONE stored matte frame
// (inPath: a %06d.png of a per-frame model's memo, 8-bit gray) to w x h —
// the tracking size (TrackSize), aspect NOT kept: the memo's square is the
// source frame stretched to the model's input and the tracking size is the
// source frame scaled, so the plain stretch back is the exact inverse —
// into <outDir>/000001.png:
//
//	-i <inPath> -vf scale=W:H:flags=bicubic,format=gray -frames:v 1 -pix_fmt gray -f image2 <outDir>/%06d.png
//
// the mask record of a mask prompt (recipe.MattePrompt.MaskFrom; the
// sidecar thresholds it at 128). The image2 muxer numbers the one output
// file 000001.png (matte.FrameFile(1)), the layout of every derived matte
// sequence, so jobs' derive machinery files it like a one-frame sequence.
// The directory is joined with "/" (joinSlash) so the argv is identical on
// every host. Returns nil for an empty path or directory or non-positive
// dimensions.
func MatteMaskScaleArgs(inPath, outDir string, w, h int) []string {
	if inPath == "" || outDir == "" || w < 1 || h < 1 {
		return nil
	}
	return []string{
		"-i", inPath,
		"-vf", "scale=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":flags=bicubic,format=gray",
		"-frames:v", "1",
		"-pix_fmt", "gray",
		"-f", "image2", joinSlash(outDir, matteSeqPattern),
	}
}
