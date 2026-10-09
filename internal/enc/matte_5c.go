package enc

// Phase 5c (docs/background-removal-proposal.md; build brief 2026-10-09;
// docs/reviews/background-removal-stabilise-and-guided-2026-10-09.md): the
// argv builders behind the matte post-processing and the guided (tracker)
// mode — temporal stabilisation of a stored matte sequence
// (MatteStabiliseArgs), the tracking-size source stream of a tracker pass
// (MatteTrackSourceArgs) and the one-slot still of it behind the live
// prompt overlay (MatteTrackFrameArgs), the gate that bounds a per-frame
// matte with the tracker's mask (MatteGateArgs) — and the tracking size
// itself (TrackSize). Every builder is pure argv; matte_5c_test.go pins the
// goldens and matte_5c_ffmpeg_test.go checks the chains against a real
// ffmpeg (frame counts, pop removal, the gate's core / band / outside).
//
// Stabilised and gated sequences are DERIVED memos under the clip dir
// ("<clipdir>/stab-<mode>/", "<clipdir>/gated-<edge>-<edgeKey>-r3/"): jobs runs the
// builders here once and then fills ExtraInput.Path with the derived
// directory's pattern, so every consumer form (matteInputArgs: the full
// sequence, a forward still's single PNG, a reversed tail's -start_number)
// reads the derived files without any enc change; the golden
// TestMatteInputDerivedPath pins that the Path is honoured verbatim and
// that MatteInput.Stabilise / Prompts / Edge are never read here.

import (
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Matte stabilisation modes (recipe.MatteParams.Stabilise, the mode
// MatteStabiliseArgs takes; the recipe constants, re-exported here).
const (
	// MatteStabiliseLight is a centred 3-frame temporal median:
	// "tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1"
	// — exactly N frames out, zero lag, removes every single-frame pop.
	MatteStabiliseLight = recipe.MatteStabiliseLight
	// MatteStabiliseStrong is the median followed by a decay-0.7 hold
	// ("…,lagfun=decay=0.7"): keep-biased, a ~0.5-frame trail on fast
	// motion. Never a hold without the median first, never decay above 0.85.
	MatteStabiliseStrong = recipe.MatteStabiliseStrong
)

// MatteTrackMaxSide is the long side of the tracking size (TrackSize): the
// frame size a tracker pass sends to the sidecar and the resolution the
// gate radius (MatteGateArgs) is measured in.
const MatteTrackMaxSide = 1024

// matteSeqPattern is the image2 pattern of every stored matte sequence
// (the memo's, the derived stabilised / gated ones): 1-based, six digits.
const matteSeqPattern = "%06d.png"

// The stabilisation chains, verbatim from the experiment report (measured
// bit-exact against their numpy definitions): the tpad clones give the
// centred median its missing neighbours at both ends, so exactly N frames
// come out with pts 0..N-1 (tmedian stamps each output with the pts of
// the oldest frame of its window, which after the leading clone IS the
// centre frame's original pts) and the first / last frames pass through.
const (
	matteStabiliseLightChain  = "tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1"
	matteStabiliseStrongChain = matteStabiliseLightChain + ",lagfun=decay=0.7"
)

// matteStabiliseChain returns the -vf chain of a stabilisation mode ("" for
// an unknown mode, including the empty "off" mode).
func matteStabiliseChain(mode string) string {
	switch mode {
	case MatteStabiliseLight:
		return matteStabiliseLightChain
	case MatteStabiliseStrong:
		return matteStabiliseStrongChain
	}
	return ""
}

// MatteStabiliseArgs is the ffmpeg argv that derives the stabilised matte
// sequence <outDir>/%06d.png from the stored gray sequence <inDir>/%06d.png
// (frames files, 1-based):
//
//	-f image2 -framerate 1 -i <inDir>/%06d.png -vf <chain> -fps_mode passthrough -frames:v N -pix_fmt gray -f image2 <outDir>/%06d.png
//
// the chain being the mode's (MatteStabiliseLight / MatteStabiliseStrong).
// -framerate 1 puts matte i at pts i; -fps_mode passthrough keeps one file
// per filtered frame (the image2 muxer numbers them from 1 whatever their
// pts); -frames:v N is a belt over the chain's own exact count. The
// directories are joined with "/" (joinSlash) so the argv is identical on
// every host. Returns nil for an unknown mode (the empty "off" mode
// included), frames < 1 or an empty directory.
func MatteStabiliseArgs(inDir, outDir string, frames int, mode string) []string {
	chain := matteStabiliseChain(mode)
	if chain == "" || frames < 1 || inDir == "" || outDir == "" {
		return nil
	}
	return []string{
		"-f", "image2", "-framerate", "1", "-i", joinSlash(inDir, matteSeqPattern),
		"-vf", chain,
		"-fps_mode", "passthrough",
		"-frames:v", strconv.Itoa(frames),
		"-pix_fmt", "gray",
		"-f", "image2", joinSlash(outDir, matteSeqPattern),
	}
}

// MatteTrackSourceArgs is MatteSourceArgs for a tracker pass: the matte
// input plan p rendered to rgb24 rawvideo on stdout at w x h — the
// tracking size (TrackSize of the plan's frame), aspect kept — instead of
// the square a segmenter takes:
//
//	[p.InputArgs...] -i <srcPath|pattern> -filter_complex "<p.Filter>;[out]format=rgb24,scale=W:H:flags=bicubic[mi]" -map [mi] -an -sn -dn -f rawvideo -pix_fmt rgb24 pipe:1
//
// jobs cuts the stream into exact w*h*3-byte frames and sends the whole
// clip in one Client.Track call. Returns nil for a nil / unusable plan or
// non-positive dimensions.
func MatteTrackSourceArgs(srcPath string, p *graph.Plan, w, h int) []string {
	if !planUsable(p) || w < 1 || h < 1 {
		return nil
	}
	return matteStreamArgs(srcPath, p, strconv.Itoa(w)+":"+strconv.Itoa(h))
}

// MatteTrackFrameArgs renders ONE output slot of the matte input plan p —
// output frame index slot on the plan's grid, the frame the master puts in
// that slot — to rgb24 rawvideo on stdout at w x h (the tracking size), the
// frame the prompt overlay sends with Client.TrackFrame (jobs'
// MattePromptMask). It is the forward still's seek (stillSeekFor at the
// middle of the slot, so float noise can never pick a neighbour; a slot past
// the end selects the last frame, like a still at t > Duration) with the
// tracker stream's tail:
//
//	[-ss S] [-itsoffset O] [-to E] [p.InputArgs minus seeks...] -i <src> -frames:v 1 -filter_complex "<p.Filter>;[out]tpad=stop_mode=clone:stop_duration=P,select='gte(t,T)',format=rgb24,scale=W:H:flags=bicubic[mi]" -map [mi] -an -sn -dn -f rawvideo -pix_fmt rgb24 pipe:1
//
// so a late slot costs one seek, not a decode of the clip. The matte plan
// is the render's temporal prefix (graph.CompileMatteInput: never reversed
// or bounced, no extra inputs); a reversed or bounced plan, an unusable
// one, a slot < 0, non-positive dimensions or a plan without a rate yield
// nil.
func MatteTrackFrameArgs(srcPath string, p *graph.Plan, w, h, slot int) []string {
	if !planUsable(p) || p.Reversed || p.Bounced || w < 1 || h < 1 || slot < 0 || !(p.FPS > 0) {
		return nil
	}
	s := stillSeekFor(p, (float64(slot)+0.5)/p.FPS, false)
	var f strings.Builder
	f.WriteString(p.Filter)
	f.WriteString(";")
	f.WriteString(outLabel(p))
	f.WriteString("tpad=stop_mode=clone:stop_duration=" + formatFloat(s.pad))
	f.WriteString(",select='gte(t," + formatFloat(s.threshold) + ")'")
	f.WriteString(",format=rgb24,scale=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":flags=bicubic")
	f.WriteString(matteSourceOutLabel)

	input := stripSeekArgs(p.InputArgs)
	args := make([]string, 0, len(input)+22)
	if s.start > 0 {
		args = append(args, "-ss", formatFloat(s.start))
	}
	if s.offset > 0 {
		args = append(args, "-itsoffset", formatFloat(s.offset))
	}
	if s.end > s.start {
		args = append(args, "-to", formatFloat(s.end))
	}
	args = append(args, input...)
	args = append(args, "-i", inputPath(srcPath, p))
	args = append(args, "-frames:v", "1")
	return append(args, matteStreamTail(f.String())...)
}

// matteStreamArgs is the shared body of MatteSourceArgs and
// MatteTrackSourceArgs: the plan's inputs, then its filter extended with
// "[out]format=rgb24,scale=<scale>:flags=bicubic[mi]" and the rgb24
// rawvideo tail. The matte plan carries no extra inputs or text files by
// contract, so none are emitted. The caller has checked planUsable.
func matteStreamArgs(srcPath string, p *graph.Plan, scale string) []string {
	args := make([]string, 0, len(p.InputArgs)+14)
	args = append(args, p.InputArgs...)
	args = append(args, "-i", inputPath(srcPath, p))
	return append(args, matteStreamTail(p.Filter+";"+outLabel(p)+"format=rgb24,scale="+scale+":flags=bicubic"+matteSourceOutLabel)...)
}

// matteStreamTail is the output half of every rgb24 matte stream: the
// filter_complex text ending in matteSourceOutLabel, mapped to rawvideo
// rgb24 on stdout, video only.
func matteStreamTail(filter string) []string {
	return []string{
		"-filter_complex", filter,
		"-map", matteSourceOutLabel,
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgb24",
		"pipe:1",
	}
}

// matteGateOutLabel is the output pad of the gate chain.
const matteGateOutLabel = "[gate]"

// MatteGateArgs is the ffmpeg argv that gates a per-frame matte sequence
// (edgeDir, the segmenter's output at its model square) with the tracker's
// mask sequence (trackDir, binary at the tracking size) into
// <outDir>/%06d.png: alpha = 255 inside erode(mask, radius), the per-frame
// matte inside the band, 0 outside dilate(mask, radius):
//
//	-f image2 -framerate 1 -i <trackDir>/%06d.png -f image2 -framerate 1 -i <edgeDir>/%06d.png
//	-filter_complex "[0:v]format=gray,split=3[t0][t1][tr];[t0]erosion=coordinates=255 x r[core];[t1]dilation=coordinates=255 x r[outer];[1:v][tr]scale=w=rw:h=rh:flags=bicubic,format=gray[edge];[outer][edge]blend=all_mode=darken[band];[core][band]blend=all_mode=lighten[gate]"
//	-map [gate] -fps_mode passthrough -frames:v N -pix_fmt gray -f image2 <outDir>/%06d.png
//
// The edge sequence is scaled to the tracking size first — by the scale
// filter's reference input (FFmpeg >= 7.1: rw / rh are the reference's
// size, the third split leg of the mask; the builder needs no w x h), so
// the band is min(dilated mask, edge matte) and the result max(eroded
// mask, band). Each erosion / dilation is one 3x3 pass (a (2r+1)^2 square
// after r of them; the gray frame has one plane, the default thresholds
// make it a plain min / max); radius 0 emits "null" legs and yields the
// mask itself. -framerate 1 pairs mask i with matte i by pts; exactly
// frames files out (-fps_mode passthrough, -frames:v N). radius is in
// tracking pixels (3 at 1024). Returns nil for frames < 1, radius < 0 or
// an empty directory.
func MatteGateArgs(trackDir, edgeDir, outDir string, frames, radius int) []string {
	if frames < 1 || radius < 0 || trackDir == "" || edgeDir == "" || outDir == "" {
		return nil
	}
	var f strings.Builder
	f.WriteString("[0:v]format=gray,split=3[t0][t1][tr];")
	f.WriteString("[t0]" + morphPasses("erosion", radius) + "[core];")
	f.WriteString("[t1]" + morphPasses("dilation", radius) + "[outer];")
	f.WriteString("[1:v][tr]scale=w=rw:h=rh:flags=bicubic,format=gray[edge];")
	f.WriteString("[outer][edge]blend=all_mode=darken[band];")
	f.WriteString("[core][band]blend=all_mode=lighten" + matteGateOutLabel)
	return []string{
		"-f", "image2", "-framerate", "1", "-i", joinSlash(trackDir, matteSeqPattern),
		"-f", "image2", "-framerate", "1", "-i", joinSlash(edgeDir, matteSeqPattern),
		"-filter_complex", f.String(),
		"-map", matteGateOutLabel,
		"-fps_mode", "passthrough",
		"-frames:v", strconv.Itoa(frames),
		"-pix_fmt", "gray",
		"-f", "image2", joinSlash(outDir, matteSeqPattern),
	}
}

// morphPasses returns n chained "<filter>=coordinates=255" stages (erosion
// / dilation: a 3x3 min / max each), or "null" for n == 0 so the leg stays
// a valid chain.
func morphPasses(filter string, n int) string {
	if n < 1 {
		return "null"
	}
	stage := filter + "=coordinates=255"
	return strings.Repeat(stage+",", n-1) + stage
}

// TrackSize returns the tracking size of a w x h frame: the frame scaled
// so its long side is at most MatteTrackMaxSide (never upscaled), the
// aspect kept (the short side rounded to nearest), then both dimensions
// rounded DOWN to even (never below 2). It is the size a tracker pass
// sends the clip at (MatteTrackSourceArgs), the size prompts are mapped
// to, and the resolution the gate radius is in. Non-positive input yields
// (0, 0).
func TrackSize(w, h int) (int, int) {
	if w < 1 || h < 1 {
		return 0, 0
	}
	tw, th := w, h
	if w >= h {
		if w > MatteTrackMaxSide {
			tw = MatteTrackMaxSide
			th = (h*MatteTrackMaxSide + w/2) / w
		}
	} else if h > MatteTrackMaxSide {
		th = MatteTrackMaxSide
		tw = (w*MatteTrackMaxSide + h/2) / h
	}
	return max(2, tw&^1), max(2, th&^1)
}
