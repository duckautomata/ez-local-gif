package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// The animated preview (DESIGN.md §7: "Play renders a low-res animated WebP
// proxy").

const (
	// MaxProxies bounds the proxy memo directory (oldest evicted). Proxies
	// are a few hundred KiB to a few MiB each, so the bound is lower than
	// MaxStills. Options.MaxProxyBytes bounds the directory in bytes too.
	MaxProxies = 60
	// proxyDir is the memo directory name under Scratch.
	proxyDir = "proxy"
	// ProxyTimeout bounds one proxy render (decode + libwebp_anim encode).
	ProxyTimeout = 60 * time.Second
	// MaxProxyWidth caps the maxW a client may ask for; larger requests are
	// clamped (the preview is a preview). The SPA asks for 360 px
	// (Preview.svelte PROXY_MAXW); the cap leaves room for a sharper preview
	// without letting a request render a full-size 60 s animation.
	MaxProxyWidth = 720
	// MaxProxySeconds caps maxSeconds the same way (the SPA asks for 10 s).
	MaxProxySeconds = 30.0
	// proxyName is the render's output file inside its scratch dir.
	proxyName = "proxy.webp"
	// proxyMemoVersion salts the proxy memo key (proxyKey): bump it whenever
	// enc.ProxyArgs or the proxy path here change what a (sources, ops,
	// output, maxW, maxSeconds) tuple renders to. 1: reversed plans decode
	// only the tail they show (enc proxySeekFor; a plain seek on an
	// animation source may land a frame off) and single-frame plans with an
	// animated overlay drop the fps=15 stage (they failed before).
	proxyMemoVersion = "2026-08-22.1"
)

// Proxy renders the animated low-resolution preview (enc.ProxyArgs: first
// maxSeconds (0 = 10) of the op stack at <= 15 fps, at most maxW (0 = 360)
// pixels wide, lossy WebP with alpha) for the recipe's op stack and output
// canvas — what the UI's Play button shows so timed overlays and keying can
// be judged in motion. Memoised in <Scratch>/proxy like Still (keyed by
// every source, the canonical ops, the geometry part of out, maxW and
// maxSeconds; bounded by MaxProxies entries and Options.MaxProxyBytes); one
// render is bounded by ProxyTimeout and killed when ctx ends. maxW and
// maxSeconds are clamped to MaxProxyWidth / MaxProxySeconds. A missing
// source, a source without probe info, an image sequence in an overlay
// position or an uncompilable op stack is an ErrInvalidRecipe.
//
// Admission: a reversed plan is refused (ErrInvalidRecipe, naming
// EZLG_MAX_MASTER_BYTES) when the frames its reverse stage buffers — the
// tail from enc.ProxyArgs's seek to the trim end, or the whole clip when
// it does not seek (proxyBufferFrames) — would exceed Options.MaxMasterBytes
// in output-sized RGBA (admitReversed). A forward plan is never gated on
// its frame count: the proxy streams the first maxSeconds at <= maxW px and
// 15 fps into libwebp_anim, a buffer bounded by MaxProxySeconds /
// MaxProxyWidth (<= 450 frames at <= 720 px — constants, not the source),
// so an untrimmed 4K clip of any length plays (the graph caps no frame
// count either; the render's cap applies at Submit). The ffmpeg run takes
// a preview slot (PreviewConcurrency) and concurrent requests for the same
// memo key share one run; a memo hit waits for neither.
func (m *Manager) Proxy(ctx context.Context, srcs []string, ops []recipe.Op, out recipe.Output, maxW int, maxSeconds float64) ([]byte, error) {
	maxW, maxSeconds = proxyBounds(maxW, maxSeconds)
	ops = stripAutoCropResolved(ops)
	s, err := m.resolveSources(srcs)
	if err != nil {
		return nil, err
	}
	subset := stillOutput(out)
	plan, err := m.compile(ctx, s, ops, subset)
	if err != nil {
		return nil, err
	}
	srcPath := s.main().Path
	if plan.Reversed || plan.Bounced {
		// A bounced plan is never seeked (enc.ProxyArgs passes its InputArgs
		// through), so its bounce stage buffers the pre-bounce half of the
		// doubled Plan.Frames — the doubled count is the conservative bound.
		frames := plan.Frames
		if !plan.Bounced {
			frames = proxyBufferFrames(plan, enc.ProxyArgs(srcPath, plan, maxW, maxSeconds, proxyName))
		}
		if err := m.admitReversed(plan, frames, "this preview"); err != nil {
			return nil, err
		}
	}
	key, err := proxyKey(srcs, ops, subset, maxW, maxSeconds)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	memoPath := filepath.Join(m.st.Scratch, proxyDir, key+".webp")
	if data := readMemo(memoPath); data != nil {
		return data, nil
	}
	if m.tools.FFmpeg == "" {
		return nil, errors.New("ffmpeg is not available on this server")
	}

	return m.previews.do(ctx, key, func(ctx context.Context) ([]byte, error) {
		if data := readMemo(memoPath); data != nil {
			return data, nil // a previous leader finished while we waited
		}
		release, err := m.acquirePreview(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		ctx, cancel := context.WithTimeout(ctx, ProxyTimeout)
		defer cancel()
		dir, cleanup, err := m.st.ScratchDir("proxy-" + store.RandomID(8))
		if err != nil {
			return nil, err
		}
		defer cleanup()
		plan, err := bindTextFiles(plan, dir)
		if err != nil {
			return nil, err
		}
		outPath := filepath.Join(dir, proxyName)
		args := enc.ProxyArgs(srcPath, plan, maxW, maxSeconds, outPath)
		if err := ffrun.RunFFmpeg(ctx, m.tools.FFmpeg, args, nil); err != nil {
			return nil, fmt.Errorf("proxy render: %w", err)
		}
		data, err := os.ReadFile(outPath)
		if err != nil {
			return nil, fmt.Errorf("proxy render produced no file: %w", err)
		}
		if len(data) == 0 {
			return nil, errors.New("proxy render produced an empty file (is the trim range empty?)")
		}
		m.memoWrite(memoPath, data, MaxProxies, m.opts.MaxProxyBytes)
		return data, nil
	})
}

// proxyBounds applies the defaults and caps of Proxy's size/length knobs.
func proxyBounds(maxW int, maxSeconds float64) (int, float64) {
	if maxW <= 0 {
		maxW = enc.DefaultProxyMaxWidth
	}
	maxW = min(maxW, MaxProxyWidth)
	if !(maxSeconds > 0) { // also catches NaN
		maxSeconds = enc.DefaultProxyMaxSeconds
	}
	maxSeconds = min(maxSeconds, MaxProxySeconds)
	return maxW, maxSeconds
}

// proxyBufferFrames estimates how many output-sized frames the reverse
// stage of a reversed proxy plan buffers: the frames between the seek
// enc.ProxyArgs applies to the main input and the trim end, on the plan's
// output grid (one frame per Speed/FPS source seconds, plus the fps
// stage's rounding frame), or the whole plan (Frames) when the argv carries
// no seek — a forward plan, a source whose demuxer cannot seek (an animated
// WebP, graph.Plan.SeekUnsafe, trimmed or not), an unknown length, or a
// tail that reaches back to TrimStart anyway. The
// seek is read back from the argv enc builds ("-ss S" in front of the main
// "-i", as its doc promises) rather than re-derived here, so the estimate
// follows enc's seek maths exactly — including the cases where it decides
// not to seek. 0 when the plan's frame count is unknown.
func proxyBufferFrames(p *graph.Plan, args []string) int {
	if p == nil || p.Frames <= 0 {
		return 0
	}
	start, ok := proxySeekStart(args)
	if !ok {
		return p.Frames
	}
	speed := p.Speed
	if !(speed > 0) || math.IsInf(speed, 0) {
		speed = 1
	}
	fps := p.FPS
	if !(fps > 0) || math.IsInf(fps, 0) {
		return p.Frames
	}
	var srcEnd float64
	switch {
	case p.TrimEnd > 0:
		srcEnd = p.TrimEnd
	case p.Duration > 0:
		srcEnd = math.Max(p.TrimStart, 0) + p.Duration*speed
	default:
		return p.Frames
	}
	span := (srcEnd - start) / speed
	if !(span > 0) {
		return p.Frames
	}
	frames := int(math.Ceil(span*fps)) + 1
	return min(frames, p.Frames)
}

// proxySeekStart returns the "-ss" value an enc.ProxyArgs argv seeks its
// main input to (the first "-ss" in front of the first "-i"), and whether
// there is one.
func proxySeekStart(args []string) (float64, bool) {
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-i":
			return 0, false
		case "-ss":
			v, err := strconv.ParseFloat(args[i+1], 64)
			return v, err == nil
		}
	}
	return 0, false
}

// proxyKey hashes (proxyMemoVersion, sources, canonical ops, geometry
// output, maxW, maxSeconds).
func proxyKey(srcs []string, ops []recipe.Op, out recipe.Output, maxW int, maxSeconds float64) (string, error) {
	return proxyKeyV(srcs, ops, out, maxW, maxSeconds, proxyMemoVersion)
}

// proxyKeyV is proxyKey with an explicit version salt.
func proxyKeyV(srcs []string, ops []recipe.Op, out recipe.Output, maxW int, maxSeconds float64, version string) (string, error) {
	canon, err := recipe.Recipe{Sources: srcs, Ops: ops, Output: out}.Canonical()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("proxy|" + version + "\n"))
	h.Write(canon)
	h.Write([]byte("|w=" + strconv.Itoa(maxW) + "|s=" + strconv.FormatFloat(maxSeconds, 'f', 3, 64)))
	return hex.EncodeToString(h.Sum(nil)), nil
}
