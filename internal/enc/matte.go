package enc

// Phase 5b: the AI matte (DESIGN.md §4.3, docs/background-removal-
// proposal.md §5.5). This file holds the one argv that feeds the sidecar
// (MatteSourceArgs) and the per-consumer forms of a matte INPUT
// (graph.ExtraInput.Matte: the "-f image2 … %06d.png" sequence a render /
// detection / proxy reads, the single unlooped PNG of a forward still, the
// shifted -start_number of a tail-seeked reversed plan) — extraInputArgsFor
// / matteInputArgs, which every builder goes through. Verified against a
// real ffmpeg by matte_ffmpeg_test.go whenever one is on PATH.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// MatteSourceArgs streams the frames the matte model sees: the matte input
// plan p (graph.CompileMatteInput — the render's temporal prefix ending in
// [out] at format=rgba, InputArgs identical to the render's) rendered to
// rgb24 rawvideo on stdout, each frame stretched to size x size (bicubic, no
// letterbox — what rembg and the benchmark do; the in-graph scale on the
// matte branch undoes it):
//
//	[p.InputArgs...] -i <srcPath|pattern> -filter_complex "<p.Filter>;[out]format=rgb24,scale=N:N:flags=bicubic[mi]" -map [mi] -an -sn -dn -f rawvideo -pix_fmt rgb24 pipe:1
//
// srcPath is the main source's blob path, or its blob directory for an
// image-sequence plan (p.InputPattern is appended, as MasterArgs does); size
// is the effective model input square (> 0). The matte plan carries no
// extra inputs or text files by contract, so none are emitted. jobs cuts
// the stream into exact size*size*3-byte frames (never decoding them),
// hashes each and sends the misses to the sidecar in batches. Transparent
// pixels of an alpha source reach the model as their stored colour; the
// intersection masks them again. Returns nil for a nil / unusable plan or a
// size < 1.
func MatteSourceArgs(srcPath string, p *graph.Plan, size int) []string {
	if !planUsable(p) || size < 1 {
		return nil
	}
	n := strconv.Itoa(size)
	args := make([]string, 0, len(p.InputArgs)+14)
	args = append(args, p.InputArgs...)
	args = append(args,
		"-i", inputPath(srcPath, p),
		"-filter_complex", p.Filter+";"+outLabel(p)+"format=rgb24,scale="+n+":"+n+":flags=bicubic"+matteSourceOutLabel,
		"-map", matteSourceOutLabel,
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgb24",
		"pipe:1",
	)
	return args
}

// matteSourceOutLabel is the output pad of MatteSourceArgs' stretch chain.
const matteSourceOutLabel = "[mi]"

// matteArgs selects the form a builder gives a matte input (ExtraInput.Matte)
// — spec §5.5's table. The zero value is the full sequence from frame 1
// (MasterArgs, CropDetectPlanArgs, unseeked proxies and stills).
type matteArgs struct {
	// single selects ONE unlooped PNG — the file of output slot slot —
	// instead of the sequence: the form of a forward still, whose single
	// image2 frame at pts 0 is paired with every later main frame by
	// framesync's defaults (eof_action repeat), whatever the seek-back.
	// Never "-loop 1": that makes the input infinite and costs a late still
	// over a second.
	single bool
	// slot is the forward still's absolute output slot (single), or the
	// slot count K a reversed plan's tail seek was snapped to — the main's
	// frames after the seek carry timestamps from 0, so the sequence starts
	// at "-start_number K+1". Both come from stillSeek.slot, never from a
	// time. Clamped to 0..Matte.Frames-1 when the memo's count is known.
	slot int
}

// stillMatteArgs returns the matte input form of a still: one unlooped PNG
// of the selected slot for a forward plan; the sequence from the seek's slot
// K for a reversed plan (K = 0, the full sequence, when the decode starts
// at TrimStart: VFR, FilterTrim, from-start); the full sequence for a
// bounced plan, forward or reversed — the merge precedes the bounce stage,
// so the mirrored half carries its own mattes by construction and the
// doubled-timeline slot must not pick a file.
func stillMatteArgs(p *graph.Plan, s stillSeek) matteArgs {
	switch {
	case p.Bounced:
		return matteArgs{}
	case s.reversed:
		return matteArgs{slot: s.slot}
	default:
		return matteArgs{single: true, slot: s.slot}
	}
}

// extraInputArgsFor returns the "-i" args of every extra input, in order
// (ffmpeg input index = position + 1): "[Args...] -i Path" for an overlay,
// matteInputArgs(e, m) for a matte input.
func extraInputArgsFor(p *graph.Plan, m matteArgs) []string {
	var args []string
	for _, e := range p.ExtraInputs {
		if e.Matte != nil {
			args = append(args, matteInputArgs(e, m)...)
			continue
		}
		args = append(args, e.Args...)
		args = append(args, "-i", e.Path)
	}
	return args
}

// matteInputArgs returns the "-i" of a matte input (e.Matte != nil) in the
// form m selects. The compiler's Args ("-f image2 -framerate <fnum(Plan.FPS)>
// -start_number 1", the same rate text as the plan's fps stage so matte i
// carries i/F like master slot i) are kept minus any -start_number / -loop
// pair, then:
//
//	sequence: … -start_number <slot+1> -i <dir>/%06d.png   (e.Path as jobs filled it)
//	single:   … -i <dir>/<slot+1 as the pattern>.png        (one file, no -loop)
//
// slot is clamped to 0..e.Matte.Frames-1 when Frames is known (Frames == 1
// → the first file): Plan.Frames is an estimate that overshoots the decoded
// count by one for ordinary VFR animations and container durations, and a
// still at the clip end selects the slot past the last matte — with the
// clamp the held last frame gets the last matte instead of a missing file.
func matteInputArgs(e graph.ExtraInput, m matteArgs) []string {
	slot := max(m.slot, 0)
	if e.Matte != nil && e.Matte.Frames > 0 {
		slot = min(slot, e.Matte.Frames-1)
	}
	args := make([]string, 0, len(e.Args)+4)
	for i := 0; i < len(e.Args); i++ {
		switch e.Args[i] {
		case "-start_number", "-loop":
			i++ // drop the value as well
		default:
			args = append(args, e.Args[i])
		}
	}
	if m.single {
		return append(args, "-i", matteFramePath(e.Path, slot+1))
	}
	return append(args, "-start_number", strconv.Itoa(slot+1), "-i", e.Path)
}

// matteFramePath returns the file of frame n (1-based) in a matte sequence
// path: the directory part is kept and the base name — an image2 printf
// pattern such as "%06d.png" — is formatted with n. A base without a
// pattern verb is returned unchanged (already a single file). Either
// separator is honoured so the argv is right on every host.
func matteFramePath(path string, n int) string {
	i := strings.LastIndexAny(path, `/\`)
	dir, base := path[:i+1], path[i+1:]
	if !strings.Contains(base, "%") {
		return path
	}
	return dir + fmt.Sprintf(base, n)
}
