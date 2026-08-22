package probe

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// TestSequenceTimingAgreesWithGraph: a sequence's probed FPS is the image2
// rate the render uses (graph.SequenceFPS, 3 decimals), Duration is
// count/FPS, and graph.Compile of that info plans exactly count frames with
// the same duration and rate — so Frames, Duration and FPS never disagree
// by a frame (34 frames at 33 ms are 34, not 33).
func TestSequenceTimingAgreesWithGraph(t *testing.T) {
	for _, delay := range []int{1, 3, 6, 7, 17, 33, 40, 41, 99, 100, 250, 333, 999, 1000, 60000} {
		for _, count := range []int{2, 3, 34, 60, 77, 1818, 5000} {
			fps, dur := sequenceTiming(count, delay)
			if fps != graph.SequenceFPS(delay) {
				t.Errorf("delay %d: fps %v, want graph.SequenceFPS %v", delay, fps, graph.SequenceFPS(delay))
			}
			if n := int(math.Floor(dur*fps + 1e-9)); n != count {
				t.Errorf("delay %d, %d frames: Duration %v * FPS %v floors to %d", delay, count, dur, fps, n)
			}
			if fps > 60 {
				continue // the output caps the rate; Plan.Frames is the capped count
			}
			info := recipe.ProbeInfo{
				Format: sequenceFormat, Codec: "png", PixFmt: "rgba", Bits: 8, Width: 16, Height: 16,
				FPS: fps, Duration: dur, Frames: count, Kind: recipe.KindSequence,
				Sequence: &recipe.SequenceInfo{Count: count, Pattern: sequencePattern("png"), DelayMS: delay},
			}
			p, err := graph.Compile(info, nil, recipe.Output{Format: "webp"})
			if err != nil {
				t.Fatalf("delay %d, %d frames: %v", delay, count, err)
			}
			if p.Frames != count || p.Duration != dur || p.SourceFPS != fps || p.FPS != fps {
				t.Errorf("delay %d, %d frames: plan Frames %d Duration %v SourceFPS %v FPS %v; probe Duration %v FPS %v", delay, count, p.Frames, p.Duration, p.SourceFPS, p.FPS, dur, fps)
			}
		}
	}
	if fps, dur := sequenceTiming(5, 0); fps != 10 || dur != 0.5 {
		t.Errorf("default delay: fps %v dur %v, want 10 / 0.5", fps, dur)
	}
	if fps, dur := sequenceTiming(34, 33); fps != 30.303 || dur != 34/30.303 {
		t.Errorf("33 ms: fps %v dur %v, want 30.303 / %v", fps, dur, 34/30.303)
	}
}

// mustDerive parses fixture JSON and derives; fatal on error.
func mustDerive(t *testing.T, raw string) derived {
	t.Helper()
	out, err := parseOutput([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	d, err := derive(out)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return d
}

const proresJSON = `{
  "streams": [
    {"index":0,"codec_name":"prores","codec_type":"video","profile":"4444","width":1920,"height":1080,
     "pix_fmt":"yuva444p10le","r_frame_rate":"30/1","avg_frame_rate":"30/1","duration":"3.000000","nb_frames":"90",
     "tags":{"handler_name":"VideoHandler"}},
    {"index":1,"codec_name":"pcm_s16le","codec_type":"audio","sample_rate":"48000"}
  ],
  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"3.000000","tags":{"major_brand":"qt  "}}
}`

func TestDeriveProRes(t *testing.T) {
	d := mustDerive(t, proresJSON)
	want := recipe.ProbeInfo{
		Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "prores", Profile: "4444", PixFmt: "yuva444p10le",
		Bits: 10, Width: 1920, Height: 1080, FPS: 30, Duration: 3, Frames: 90,
		HasAlpha: true, HasAudio: true, Kind: recipe.KindVideo,
	}
	if d.info != want {
		t.Errorf("info =\n%+v\nwant\n%+v", d.info, want)
	}
	if d.alpha != alphaDecided {
		t.Error("prores must not need an alpha scan")
	}
	if d.needFrameCount {
		t.Error("prores must not need a frame count")
	}
}

const gifJSON = `{
  "streams": [
    {"index":0,"codec_name":"gif","codec_type":"video","width":128,"height":96,"pix_fmt":"bgra",
     "r_frame_rate":"100/1","avg_frame_rate":"10/1","time_base":"1/100","duration":"3.600000"}
  ],
  "format": {"format_name":"gif","duration":"3.600000"}
}`

func TestDeriveGIF(t *testing.T) {
	d := mustDerive(t, gifJSON)
	in := d.info
	if in.Kind != recipe.KindAnimation || in.IsStill {
		t.Errorf("kind = %q still=%v", in.Kind, in.IsStill)
	}
	if in.FPS != 10 {
		t.Errorf("fps = %v, want avg_frame_rate 10 (r_frame_rate 100/1 is the centisecond tick fallback, not a cadence)", in.FPS)
	}
	if in.Duration != 3.6 || in.Frames != 36 {
		t.Errorf("duration/frames = %v/%d, want 3.6/36", in.Duration, in.Frames)
	}
	if in.Bits != 8 || in.PixFmt != "bgra" {
		t.Errorf("bits/pixfmt = %d/%s", in.Bits, in.PixFmt)
	}
	if d.alpha != alphaScan || !d.assumeAlpha {
		t.Errorf("gif must be alpha-scanned (assume=true on failure): %v/%v", d.alpha, d.assumeAlpha)
	}
	if d.needFrameCount {
		t.Error("frames were estimable; no count needed")
	}
}

// TestDeriveAnimationFPS pins the r_frame_rate/avg_frame_rate choice for
// animations: r is ffmpeg's base-cadence estimate and wins when plausible;
// avg (frames/duration) is only the fallback for the tick-rate case, since
// it collapses for "motion then hold" GIFs and a CFR resample at it drops
// most motion frames. Fixture values are what ffprobe 9.0.1 reports.
func TestDeriveAnimationFPS(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		fps       float64
		frames    int
		needCount bool
	}{
		// 20 frames at 5 cs + a 200 cs hold: r=20/1, avg=50/7.
		{"gif motion+hold", `{"streams":[{"codec_name":"gif","codec_type":"video","width":32,"height":32,"pix_fmt":"bgra",
		  "r_frame_rate":"20/1","avg_frame_rate":"50/7","nb_frames":"21","duration":"3.0"}],"format":{"format_name":"gif","duration":"3.0"}}`, 20, 21, false},
		// Two frames: the rfps estimator gives up and reports the 100/1 tick rate.
		{"gif two frames", `{"streams":[{"codec_name":"gif","codec_type":"video","width":32,"height":32,"pix_fmt":"bgra",
		  "r_frame_rate":"100/1","avg_frame_rate":"10/1","nb_frames":"2","duration":"0.2"}],"format":{"format_name":"gif","duration":"0.2"}}`, 10, 2, false},
		// CFR gif: r == avg.
		{"gif cfr", `{"streams":[{"codec_name":"gif","codec_type":"video","width":32,"height":32,"pix_fmt":"bgra",
		  "r_frame_rate":"10/1","avg_frame_rate":"10/1","nb_frames":"8","duration":"0.8"}],"format":{"format_name":"gif","duration":"0.8"}}`, 10, 8, false},
		// VFR animated webp: r=25/1 is the cadence, avg is tiny.
		{"webp vfr", `{"streams":[{"codec_name":"webp","codec_type":"video","width":64,"height":64,"pix_fmt":"yuv420p",
		  "r_frame_rate":"25/1","avg_frame_rate":"375/176","duration":"7.04"}],"format":{"format_name":"webp_anim","duration":"7.04"}}`, 25, 176, false},
		// WebP tick rate (1000/1) with no avg: r is implausible but nothing
		// else is known, so it is kept as a last resort (unchanged behaviour;
		// graph snaps it) and the frame counter runs.
		{"webp tick only", `{"streams":[{"codec_name":"webp","codec_type":"video","width":64,"height":64,"pix_fmt":"yuv420p",
		  "r_frame_rate":"1000/1","avg_frame_rate":"0/0"}],"format":{"format_name":"webp_anim"}}`, 1000, 0, true},
		// APNG with a plausible r.
		{"apng", `{"streams":[{"codec_name":"apng","codec_type":"video","width":64,"height":64,"pix_fmt":"rgba",
		  "r_frame_rate":"12/1","avg_frame_rate":"6/1","duration":"4.0"}],"format":{"format_name":"apng","duration":"4.0"}}`, 12, 48, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustDerive(t, c.raw)
			if d.info.FPS != c.fps {
				t.Errorf("fps = %v, want %v", d.info.FPS, c.fps)
			}
			if d.info.Frames != c.frames {
				t.Errorf("frames = %d, want %d", d.info.Frames, c.frames)
			}
			if d.needFrameCount != c.needCount {
				t.Errorf("needFrameCount = %v, want %v", d.needFrameCount, c.needCount)
			}
			if d.info.Kind != recipe.KindAnimation {
				t.Errorf("kind = %q", d.info.Kind)
			}
		})
	}
	// The gate itself.
	if got := animationFPS(100, 10); got != 10 {
		t.Errorf("animationFPS(100,10) = %v", got)
	}
	if got := animationFPS(20, 7.14); got != 20 {
		t.Errorf("animationFPS(20,7.14) = %v", got)
	}
	if got := animationFPS(0, 12.5); got != 12.5 {
		t.Errorf("animationFPS(0,12.5) = %v", got)
	}
	if got := animationFPS(1000, 0); got != 1000 {
		t.Errorf("animationFPS(1000,0) = %v (last resort keeps r)", got)
	}
	if got := animationFPS(60, 30); got != 60 {
		t.Errorf("animationFPS(60,30) = %v (60 is still plausible)", got)
	}
}

// TestDeriveDisplayRotation: ffmpeg autorotates 90/270-degree sources on
// decode, so probe must report the displayed (swapped) size; 180 and odd
// angles keep the coded size.
func TestDeriveDisplayRotation(t *testing.T) {
	mp4 := func(sideData string) string {
		return `{"streams":[{"codec_name":"h264","codec_type":"video","width":1920,"height":1080,"pix_fmt":"yuv420p",
		  "r_frame_rate":"30/1","avg_frame_rate":"30/1","duration":"2.0","nb_frames":"60"` + sideData + `}],
		  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0"}}`
	}
	dm := func(rot string) string {
		return `,"side_data_list":[{"side_data_type":"Display Matrix","displaymatrix":"\n00000000: 0 65536 0\n","rotation":` + rot + `}]`
	}
	cases := []struct {
		name string
		raw  string
		w, h int
	}{
		{"none", mp4(""), 1920, 1080},
		{"iphone -90", mp4(dm("-90")), 1080, 1920},
		{"90", mp4(dm("90")), 1080, 1920},
		{"180", mp4(dm("-180")), 1920, 1080},
		{"270", mp4(dm("270")), 1080, 1920},
		{"-270", mp4(dm("-270")), 1080, 1920},
		{"string 90", mp4(dm(`"90"`)), 1080, 1920},
		{"odd angle 45", mp4(dm("45")), 1920, 1080},
		{"nearly 90", mp4(dm("89.6")), 1080, 1920},
		{"0", mp4(dm("0")), 1920, 1080},
		{"N/A", mp4(dm(`"N/A"`)), 1920, 1080},
		{"other side data first", mp4(`,"side_data_list":[{"side_data_type":"Content Light Level"},{"side_data_type":"Display Matrix","rotation":90}]`), 1080, 1920},
		{"rotation on unknown type", mp4(`,"side_data_list":[{"side_data_type":"Something","rotation":-90}]`), 1080, 1920},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustDerive(t, c.raw)
			if d.info.Width != c.w || d.info.Height != c.h {
				t.Errorf("dims = %dx%d, want %dx%d", d.info.Width, d.info.Height, c.w, c.h)
			}
			if d.info.Kind != recipe.KindVideo || d.info.Frames != 60 || d.info.FPS != 30 {
				t.Errorf("unexpected side effects: %+v", d.info)
			}
		})
	}
	if rotationSwapsDims(90, false) {
		t.Error("no rotation must not swap")
	}
}

func TestDeriveGIFSingleFrame(t *testing.T) {
	d := mustDerive(t, `{"streams":[{"codec_name":"gif","codec_type":"video","width":10,"height":10,"pix_fmt":"bgra",
	  "r_frame_rate":"25/1","avg_frame_rate":"0/0","nb_frames":"1"}],"format":{"format_name":"gif"}}`)
	if !d.info.IsStill || d.info.Kind != recipe.KindImage || d.info.Frames != 1 || d.info.FPS != 0 {
		t.Errorf("single-frame gif: %+v", d.info)
	}
}

func TestDeriveWebPAnimNeedsCount(t *testing.T) {
	d := mustDerive(t, `{"streams":[{"codec_name":"webp","codec_type":"video","width":64,"height":64,"pix_fmt":"yuva420p",
	  "r_frame_rate":"1000/1","avg_frame_rate":"0/0"}],"format":{"format_name":"webp_anim"}}`)
	if d.info.Kind != recipe.KindAnimation || d.info.Frames != 0 || !d.needFrameCount {
		t.Errorf("webp_anim without metadata: %+v need=%v", d.info, d.needFrameCount)
	}
	if d.alpha != alphaScan {
		t.Error("webp with yuva420p must be scanned")
	}
	// Opaque webp animation: no scan.
	d = mustDerive(t, `{"streams":[{"codec_name":"webp","codec_type":"video","width":64,"height":64,"pix_fmt":"yuv420p",
	  "r_frame_rate":"20/1","avg_frame_rate":"20/1","duration":"2.0"}],"format":{"format_name":"webp_anim","duration":"2.0"}}`)
	if d.alpha != alphaDecided || d.info.HasAlpha {
		t.Errorf("opaque webp: alpha=%v has=%v", d.alpha, d.info.HasAlpha)
	}
	if d.info.Frames != 40 || d.needFrameCount {
		t.Errorf("frames = %d (want 40), need=%v", d.info.Frames, d.needFrameCount)
	}
}

func TestDeriveStills(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		alpha alphaMode
		has   bool // when decided
		codec string
	}{
		{"png rgba", `{"streams":[{"codec_name":"png","codec_type":"video","width":100,"height":50,"pix_fmt":"rgba",
		  "r_frame_rate":"25/1","avg_frame_rate":"0/0"}],"format":{"format_name":"png_pipe"}}`, alphaScan, true, "png"},
		{"png rgb24", `{"streams":[{"codec_name":"png","codec_type":"video","width":100,"height":50,"pix_fmt":"rgb24",
		  "r_frame_rate":"25/1","avg_frame_rate":"0/0"}],"format":{"format_name":"png_pipe"}}`, alphaDecided, false, "png"},
		{"png pal8", `{"streams":[{"codec_name":"png","codec_type":"video","width":100,"height":50,"pix_fmt":"pal8",
		  "r_frame_rate":"25/1","avg_frame_rate":"0/0"}],"format":{"format_name":"png_pipe"}}`, alphaScan, true, "png"},
		{"jpeg image2", `{"streams":[{"codec_name":"mjpeg","codec_type":"video","width":640,"height":480,"pix_fmt":"yuvj420p",
		  "r_frame_rate":"25/1","avg_frame_rate":"25/1","duration":"0.040000","nb_frames":"1"}],"format":{"format_name":"image2","duration":"0.040000"}}`, alphaDecided, false, "mjpeg"},
		{"webp still", `{"streams":[{"codec_name":"webp","codec_type":"video","width":32,"height":32,"pix_fmt":"yuv420p",
		  "r_frame_rate":"25/1","avg_frame_rate":"0/0"}],"format":{"format_name":"webp_pipe"}}`, alphaDecided, false, "webp"},
		{"avif still", `{"streams":[{"codec_name":"av1","codec_type":"video","width":32,"height":32,"pix_fmt":"yuv420p",
		  "r_frame_rate":"25/1","avg_frame_rate":"25/1","nb_frames":"1"}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","tags":{"major_brand":"avif","compatible_brands":"avifmif1miaf"}}}`, alphaDecided, false, "av1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustDerive(t, c.raw)
			in := d.info
			if !in.IsStill || in.Kind != recipe.KindImage || in.Frames != 1 || in.FPS != 0 || in.Duration != 0 {
				t.Errorf("not a still: %+v", in)
			}
			if in.Codec != c.codec {
				t.Errorf("codec = %q", in.Codec)
			}
			if d.alpha != c.alpha {
				t.Errorf("alpha mode = %v, want %v", d.alpha, c.alpha)
			}
			if c.alpha == alphaDecided && in.HasAlpha != c.has {
				t.Errorf("hasAlpha = %v, want %v", in.HasAlpha, c.has)
			}
			if c.alpha == alphaScan && !d.assumeAlpha {
				t.Error("scan fallback should assume alpha")
			}
		})
	}
}

// TestDeriveOneFrameVideoIsStill: a source whose container states a single
// frame is a still whatever the codec — a one-frame ProRes 4444 or H.264
// MOV (a Resolve/Premiere "export frame") must not be sent through the fps
// filter, which emits nothing for one input frame.
func TestDeriveOneFrameVideoIsStill(t *testing.T) {
	mov := func(codec, pixfmt, nb, dur string) string {
		return `{"streams":[{"codec_name":"` + codec + `","codec_type":"video","width":1920,"height":1080,"pix_fmt":"` + pixfmt + `",
		  "r_frame_rate":"25/1","avg_frame_rate":"25/1"` + nb + dur + `}],
		  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"0.040000","tags":{"major_brand":"qt  "}}}`
	}
	stills := []struct {
		name  string
		raw   string
		alpha bool
	}{
		{"prores 4444 one frame", mov("prores", "yuva444p10le", `,"nb_frames":"1"`, `,"duration":"0.040000"`), true},
		{"h264 one frame", mov("h264", "yuv420p", `,"nb_frames":"1"`, `,"duration":"0.040000"`), false},
		{"numeric nb_frames", mov("hevc", "yuv420p10le", `,"nb_frames":1`, ``), false},
		{"mjpeg one frame estimated", mov("mjpeg", "yuvj420p", ``, `,"duration":"0.040000"`), false},
	}
	for _, c := range stills {
		t.Run(c.name, func(t *testing.T) {
			d := mustDerive(t, c.raw)
			in := d.info
			if !in.IsStill || in.Kind != recipe.KindImage || in.Frames != 1 || in.FPS != 0 || in.Duration != 0 {
				t.Errorf("not a still: %+v", in)
			}
			if d.alpha != alphaDecided || in.HasAlpha != c.alpha {
				t.Errorf("alpha: mode=%v has=%v want %v", d.alpha, in.HasAlpha, c.alpha)
			}
			if d.needFrameCount {
				t.Error("frame count already established")
			}
		})
	}
	videos := []struct {
		name   string
		raw    string
		frames int
	}{
		{"h264 two frames", mov("h264", "yuv420p", `,"nb_frames":"2"`, `,"duration":"0.080000"`), 2},
		// No container count: a 0.04 s clip estimates to 1 frame, but that is
		// a guess, not an established count — it stays a video.
		{"h264 estimated one frame", mov("h264", "yuv420p", ``, `,"duration":"0.040000"`), 1},
		{"prores many frames", proresJSON, 90},
	}
	for _, c := range videos {
		t.Run(c.name, func(t *testing.T) {
			d := mustDerive(t, c.raw)
			in := d.info
			if in.IsStill || in.Kind != recipe.KindVideo || in.FPS == 0 {
				t.Errorf("must stay a video: %+v", in)
			}
			if in.Frames != c.frames {
				t.Errorf("frames = %d, want %d", in.Frames, c.frames)
			}
		})
	}
}

func TestDeriveAVIFWithAlphaStream(t *testing.T) {
	d := mustDerive(t, `{"streams":[
	  {"codec_name":"av1","codec_type":"video","width":32,"height":32,"pix_fmt":"yuv420p","r_frame_rate":"25/1","avg_frame_rate":"25/1","nb_frames":"1"},
	  {"codec_name":"av1","codec_type":"video","width":32,"height":32,"pix_fmt":"gray","r_frame_rate":"25/1","avg_frame_rate":"25/1","nb_frames":"1"}],
	  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","tags":{"major_brand":"avif"}}}`)
	if !d.info.HasAlpha || d.alpha != alphaDecided {
		t.Errorf("avif with auxiliary alpha stream: %+v", d.info)
	}
	if d.info.AlphaStream != 1 {
		t.Errorf("still avif alpha stream = %d, want v:1 (%+v)", d.info.AlphaStream, d.info)
	}
	if !d.info.IsStill || d.info.Frames != 1 {
		t.Errorf("still avif: %+v", d.info)
	}
	// Animated AVIF (avis) with duration → animation.
	d = mustDerive(t, `{"streams":[
	  {"codec_name":"av1","codec_type":"video","width":32,"height":32,"pix_fmt":"yuv420p","r_frame_rate":"10/1","avg_frame_rate":"10/1","duration":"2.0","nb_frames":"20"}],
	  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"2.0","tags":{"major_brand":"avis","compatible_brands":"avifmif1miafmsf1iso8"}}}`)
	if d.info.Kind != recipe.KindAnimation || d.info.Frames != 20 || d.info.HasAlpha || d.info.AlphaStream != 0 {
		t.Errorf("avis: %+v", d.info)
	}
}

// avifenc 1.2.1 writes an animated AVIF as a primary still item (+ its alpha
// item) followed by the colour/alpha tracks; ffprobe 9.0.1 reports them as
// [Color item 1 frame, Alpha item 1 frame, colour track, alpha track] (see
// TestProbeAVIFIntegration for the real file). The probe must describe the
// track, not the one-frame item, and name the track's alpha stream.
const avifencAnimJSON = `{"streams":[
  {"index":0,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"yuv420p","r_frame_rate":"1/1","avg_frame_rate":"1/1","nb_frames":"1","disposition":{"default":1},"tags":{"title":"Color"}},
  {"index":1,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"gray","r_frame_rate":"1/1","avg_frame_rate":"1/1","nb_frames":"1","disposition":{"default":0},"tags":{"title":"Alpha"}},
  {"index":2,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"yuv420p","r_frame_rate":"10/1","avg_frame_rate":"10/1","duration":"1.000000","nb_frames":"10","disposition":{"default":1}},
  {"index":3,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"gray","r_frame_rate":"10/1","avg_frame_rate":"10/1","duration":"1.000000","nb_frames":"10","disposition":{"default":1}}],
  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.000000","tags":{"major_brand":"avis","compatible_brands":"avifavismsf1iso8mif1miafMA1B"}}}`

func TestDeriveAVIFLayouts(t *testing.T) {
	d := mustDerive(t, avifencAnimJSON)
	in := d.info
	if in.Kind != recipe.KindAnimation || in.IsStill || in.Frames != 10 || in.FPS != 10 || in.Duration != 1 {
		t.Errorf("animated avif must be described by its track: %+v", in)
	}
	if !in.HasAlpha || in.AlphaStream != 3 || d.alpha != alphaDecided {
		t.Errorf("animated avif alpha: has=%v stream=%d mode=%v", in.HasAlpha, in.AlphaStream, d.alpha)
	}
	if in.ColorStream != 2 {
		t.Errorf("animated avif colour track must be v:2 (the one-frame primary item is v:0), got %d", in.ColorStream)
	}
	if in.Width != 64 || in.Height != 48 || in.Codec != "av1" {
		t.Errorf("animated avif facts: %+v", in)
	}

	// Opaque animated AVIF: [Color item, colour track] → no alpha stream.
	d = mustDerive(t, `{"streams":[
	  {"index":0,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"yuv420p","r_frame_rate":"1/1","avg_frame_rate":"1/1","nb_frames":"1","tags":{"title":"Color"}},
	  {"index":1,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"yuv420p","r_frame_rate":"10/1","avg_frame_rate":"10/1","duration":"1.000000","nb_frames":"10"}],
	  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.000000","tags":{"major_brand":"avis","compatible_brands":"avifavismsf1iso8mif1miafMA1B"}}}`)
	if d.info.HasAlpha || d.info.AlphaStream != 0 {
		t.Errorf("opaque animated avif reported alpha: %+v", d.info)
	}
	if d.info.Kind != recipe.KindAnimation || d.info.Frames != 10 || d.info.ColorStream != 1 {
		t.Errorf("opaque animated avif (colour track v:1): %+v", d.info)
	}

	// Opaque still: one stream.
	d = mustDerive(t, `{"streams":[
	  {"index":0,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"yuv420p","r_frame_rate":"1/1","avg_frame_rate":"1/1","nb_frames":"1","tags":{"title":"Color"}}],
	  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","tags":{"major_brand":"avif","compatible_brands":"avifmif1miaf"}}}`)
	if d.info.HasAlpha || d.info.AlphaStream != 0 || !d.info.IsStill {
		t.Errorf("opaque still avif: %+v", d.info)
	}

	// An alpha plane of another size or frame count does not belong to the
	// colour stream; a 10-bit gray plane does.
	d = mustDerive(t, `{"streams":[
	  {"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"yuv420p10le","nb_frames":"1"},
	  {"codec_name":"av1","codec_type":"video","width":32,"height":24,"pix_fmt":"gray","nb_frames":"1"},
	  {"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"gray10le","nb_frames":"1"}],
	  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","tags":{"major_brand":"avif"}}}`)
	if !d.info.HasAlpha || d.info.AlphaStream != 2 || d.info.Bits != 10 {
		t.Errorf("alpha plane matching: %+v", d.info)
	}
	// Non-AVIF containers never set AlphaStream.
	if d := mustDerive(t, proresJSON); d.info.AlphaStream != 0 {
		t.Errorf("prores AlphaStream = %d", d.info.AlphaStream)
	}
}

// TestDeriveMonochromeAVIF: monochrome (yuv400) AVIFs report a gray pix_fmt
// for every stream, colour ones included, so pix_fmt alone cannot tell
// colour from alpha. The most-frames rule must still describe an animation
// by its track (excluding only "Alpha"-titled items — the alpha TRACK of an
// animation carries default=1 and no title, so only position separates it,
// and ties go to the first stream: libavif and ffmpeg's avif muxer write
// the colour track before its alpha track).
func TestDeriveMonochromeAVIF(t *testing.T) {
	grayStream := func(index int, nb string, extra string) string {
		return `{"index":` + strconv.Itoa(index) + `,"codec_name":"av1","codec_type":"video","width":64,"height":48,"pix_fmt":"gray",
		  "r_frame_rate":"10/1","avg_frame_rate":"10/1","nb_frames":"` + nb + `"` + extra + `}`
	}
	avis := func(streams ...string) string {
		return `{"streams":[` + strings.Join(streams, ",") + `],
		  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.000000","tags":{"major_brand":"avis","compatible_brands":"avifavismsf1iso8mif1miaf"}}}`
	}

	// Opaque gray animation: [gray Color item 1f, gray track 10f] → the
	// track (v:1), 10 frames, no alpha — not a 1-frame still of the item.
	d := mustDerive(t, avis(
		grayStream(0, "1", `,"disposition":{"default":1},"tags":{"title":"Color"}`),
		grayStream(1, "10", `,"duration":"1.000000","disposition":{"default":1}`)))
	if d.info.Kind != recipe.KindAnimation || d.info.IsStill || d.info.Frames != 10 || d.info.FPS != 10 {
		t.Errorf("gray animated avif must be described by its track: %+v", d.info)
	}
	if d.info.ColorStream != 1 || d.info.AlphaStream != 0 || d.info.HasAlpha {
		t.Errorf("gray animated avif streams: colour v:%d alpha v:%d has=%v", d.info.ColorStream, d.info.AlphaStream, d.info.HasAlpha)
	}

	// Gray animation with alpha: [gray Color item, gray Alpha item, gray
	// track, gray alpha track (default=1, no title)] → colour track v:2
	// (first of the 10-frame tie), alpha track v:3.
	d = mustDerive(t, avis(
		grayStream(0, "1", `,"disposition":{"default":1},"tags":{"title":"Color"}`),
		grayStream(1, "1", `,"disposition":{"default":0},"tags":{"title":"Alpha"}`),
		grayStream(2, "10", `,"duration":"1.000000","disposition":{"default":1}`),
		grayStream(3, "10", `,"duration":"1.000000","disposition":{"default":1}`)))
	if d.info.Kind != recipe.KindAnimation || d.info.Frames != 10 || d.info.ColorStream != 2 || d.info.AlphaStream != 3 || !d.info.HasAlpha {
		t.Errorf("gray animated avif with alpha: %+v", d.info)
	}

	// Gray still with an alpha item: [gray Color 1f, gray Alpha 1f] → v:0
	// colour, v:1 alpha, still.
	d = mustDerive(t, `{"streams":[`+
		grayStream(0, "1", `,"disposition":{"default":1},"tags":{"title":"Color"}`)+","+
		grayStream(1, "1", `,"disposition":{"default":0},"tags":{"title":"Alpha"}`)+
		`],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","tags":{"major_brand":"avif","compatible_brands":"avifmif1miaf"}}}`)
	if !d.info.IsStill || d.info.Frames != 1 || d.info.ColorStream != 0 || d.info.AlphaStream != 1 || !d.info.HasAlpha {
		t.Errorf("gray still avif with alpha: %+v", d.info)
	}

	// Gray still without alpha: the single item wins.
	d = mustDerive(t, `{"streams":[`+
		grayStream(0, "1", `,"tags":{"title":"Color"}`)+
		`],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","tags":{"major_brand":"avif","compatible_brands":"avifmif1miaf"}}}`)
	if !d.info.IsStill || d.info.ColorStream != 0 || d.info.AlphaStream != 0 || d.info.HasAlpha {
		t.Errorf("gray still avif: %+v", d.info)
	}
}

func TestDeriveVideos(t *testing.T) {
	// h264 mp4 with a cover-art stream first: the real video stream wins.
	d := mustDerive(t, `{"streams":[
	  {"codec_name":"mjpeg","codec_type":"video","width":500,"height":500,"pix_fmt":"yuvj444p","r_frame_rate":"90000/1","avg_frame_rate":"0/0","disposition":{"attached_pic":1}},
	  {"codec_name":"h264","codec_type":"video","profile":"High","width":1280,"height":720,"pix_fmt":"yuv420p","r_frame_rate":"30000/1001","avg_frame_rate":"30000/1001","duration":"10.010000","nb_frames":"300","disposition":{"attached_pic":0}},
	  {"codec_name":"aac","codec_type":"audio"}],
	  "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"10.010000","tags":{"major_brand":"isom"}}}`)
	in := d.info
	if in.Codec != "h264" || in.Width != 1280 || in.Profile != "High" {
		t.Errorf("picked wrong stream: %+v", in)
	}
	if in.Kind != recipe.KindVideo || in.IsStill || in.HasAlpha || !in.HasAudio {
		t.Errorf("h264: %+v", in)
	}
	if fps := in.FPS; fps < 29.96 || fps > 29.98 {
		t.Errorf("fps = %v", fps)
	}
	if in.Frames != 300 || in.Bits != 8 {
		t.Errorf("frames/bits = %d/%d", in.Frames, in.Bits)
	}

	// VP9 WebM with alpha_mode tag.
	d = mustDerive(t, `{"streams":[{"codec_name":"vp9","codec_type":"video","width":640,"height":360,"pix_fmt":"yuv420p",
	  "r_frame_rate":"30/1","avg_frame_rate":"30/1","tags":{"alpha_mode":"1"}}],"format":{"format_name":"matroska,webm","duration":"4.000000"}}`)
	if !d.info.HasAlpha || d.alpha != alphaDecided {
		t.Errorf("vp9 alpha_mode=1 → HasAlpha: %+v", d.info)
	}
	if d.info.Frames != 120 || d.info.Duration != 4 {
		t.Errorf("vp9 frames/duration from format: %d/%v", d.info.Frames, d.info.Duration)
	}
	d = mustDerive(t, `{"streams":[{"codec_name":"vp9","codec_type":"video","width":640,"height":360,"pix_fmt":"yuv420p",
	  "r_frame_rate":"30/1","avg_frame_rate":"30/1"}],"format":{"format_name":"matroska,webm"}}`)
	if d.info.HasAlpha {
		t.Error("vp9 without alpha_mode must not have alpha")
	}
	if d.needFrameCount {
		t.Error("videos never trigger the frame counter")
	}

	// mjpeg video in AVI (many frames) is video, not a still.
	d = mustDerive(t, `{"streams":[{"codec_name":"mjpeg","codec_type":"video","width":320,"height":240,"pix_fmt":"yuvj422p",
	  "r_frame_rate":"15/1","avg_frame_rate":"15/1","nb_frames":"45","duration":"3.0"}],"format":{"format_name":"avi","duration":"3.0"}}`)
	if d.info.IsStill || d.info.Kind != recipe.KindVideo {
		t.Errorf("mjpeg avi: %+v", d.info)
	}
}

func TestDeriveErrors(t *testing.T) {
	if _, err := parseOutput([]byte("")); err == nil {
		t.Error("empty output accepted")
	}
	if _, err := parseOutput([]byte("{not json")); err == nil {
		t.Error("bad json accepted")
	}
	out, _ := parseOutput([]byte(`{"streams":[{"codec_name":"aac","codec_type":"audio"}],"format":{"format_name":"mp3"}}`))
	if _, err := derive(out); err != ErrNoVideo {
		t.Errorf("audio-only: err = %v, want ErrNoVideo", err)
	}
	out, _ = parseOutput([]byte(`{"streams":[{"codec_name":"h264","codec_type":"video","width":0,"height":0}],"format":{"format_name":"mp4"}}`))
	if _, err := derive(out); err == nil {
		t.Error("zero dimensions accepted")
	}
	// Numeric profile / nb_frames tolerated.
	out, err := parseOutput([]byte(`{"streams":[{"codec_name":"h264","codec_type":"video","profile":100,"width":16,"height":16,"nb_frames":12,"duration":0.5}],"format":{"format_name":"mp4"}}`))
	if err != nil {
		t.Fatal(err)
	}
	d, err := derive(out)
	if err != nil {
		t.Fatal(err)
	}
	if d.info.Profile != "100" || d.info.Frames != 12 || d.info.Duration != 0.5 {
		t.Errorf("flex fields: %+v", d.info)
	}
}

func TestBitsFromPixFmt(t *testing.T) {
	cases := map[string]int{
		"yuv420p": 8, "yuvj420p": 8, "yuva444p10le": 10, "yuva444p12le": 12, "yuv420p10be": 10,
		"gbrap": 8, "gbrap12le": 12, "gbrap16le": 16, "rgba": 8, "bgra": 8, "rgba64le": 16, "rgb48be": 16,
		"gray": 8, "gray16le": 16, "gray10le": 10, "ya8": 8, "ya16le": 16, "pal8": 8, "nv12": 8, "nv21": 8,
		"yuyv422": 8, "p010le": 10, "p016le": 16, "xyz12le": 12, "gbrpf32le": 32, "grayf32le": 32,
		"gbrapf16le": 16, "monob": 8, "": 0, "yuv444p16le": 16, "rgb24": 8, "0rgb": 8,
	}
	for in, want := range cases {
		if got := bitsFromPixFmt(in); got != want {
			t.Errorf("bitsFromPixFmt(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAlphaFromPixFmt(t *testing.T) {
	yes := []string{"yuva420p", "yuva444p10le", "rgba", "bgra", "argb", "abgr", "gbrap", "gbrap12le", "gbrapf32le", "ya8", "ya16le", "rgba64le", "ayuv64le", "vuya"}
	no := []string{"yuv420p", "rgb24", "bgr24", "gray", "gbrp", "nv12", "yuvj444p", "rgb0", "0bgr", "bayer_rggb8", "yuyv422", "gray16le"}
	maybe := []string{"pal8", ""}
	for _, p := range yes {
		if alphaFromPixFmt(p) != alphaYes {
			t.Errorf("%q should have alpha", p)
		}
	}
	for _, p := range no {
		if alphaFromPixFmt(p) != alphaNone {
			t.Errorf("%q should not have alpha", p)
		}
	}
	for _, p := range maybe {
		if alphaFromPixFmt(p) != alphaMaybe {
			t.Errorf("%q should be ambiguous", p)
		}
	}
}

func TestParseRate(t *testing.T) {
	cases := map[string]float64{"30/1": 30, "30000/1001": 30000.0 / 1001, "0/0": 0, "": 0, "25": 25, "abc": 0, "1/0": 0, "-5/1": 0}
	for in, want := range cases {
		if got := parseRate(in); got != want {
			t.Errorf("parseRate(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestScanFrameBudget(t *testing.T) {
	if got := scanFrameBudget(60, 128, 128); got != 60 {
		t.Errorf("small frames: %d", got)
	}
	if got := scanFrameBudget(60, 3840, 2160); got != 8 {
		t.Errorf("4K: %d, want 8", got)
	}
	if got := scanFrameBudget(60, 20000, 20000); got != 2 {
		t.Errorf("huge: %d, want floor 2", got)
	}
	if got := scanFrameBudget(60, 0, 0); got != 60 {
		t.Errorf("unknown size: %d", got)
	}
}

func TestAnyBelow255(t *testing.T) {
	if anyBelow255(bytes.Repeat([]byte{0xFF}, 1000)) {
		t.Error("all-opaque reported alpha")
	}
	b := bytes.Repeat([]byte{0xFF}, 1000)
	b[999] = 0xFE
	if !anyBelow255(b) {
		t.Error("single non-opaque byte missed")
	}
	if anyBelow255(nil) {
		t.Error("empty reported alpha")
	}
}

// ---- integration (needs ffprobe/ffmpeg + implemented enc argv) ------------

func toolsOrSkip(t *testing.T) ffrun.Tools {
	t.Helper()
	tools := ffrun.LookupTools()
	if tools.FFprobe == "" {
		if p, err := exec.LookPath("ffprobe"); err == nil {
			tools.FFprobe = p
		}
	}
	if tools.FFmpeg == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			tools.FFmpeg = p
		}
	}
	if tools.FFprobe == "" || tools.FFmpeg == "" {
		t.Skip("ffprobe/ffmpeg not on PATH")
	}
	if len(enc.ProbeArgs("x")) == 0 {
		t.Skip("enc.ProbeArgs not implemented yet")
	}
	return tools
}

func writePNG(t *testing.T, dir, name string, transparent bool) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.NRGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	if transparent {
		img.Set(0, 0, color.NRGBA{A: 0})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbePNGIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := Probe(ctx, tools, writePNG(t, dir, "alpha.png", true), 0)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Codec != "png" || info.Width != 8 || info.Height != 6 || !info.IsStill || info.Kind != recipe.KindImage || info.Frames != 1 {
		t.Errorf("png: %+v", info)
	}
	if !info.HasAlpha {
		t.Errorf("transparent png: HasAlpha false: %+v", info)
	}
	if info.Premultiplied {
		t.Error("png must not be premultiplied")
	}

	info, err = Probe(ctx, tools, writePNG(t, dir, "opaque.png", false), 0)
	if err != nil {
		t.Fatalf("Probe opaque: %v", err)
	}
	if len(enc.AlphaScanArgs("x", 1)) > 0 && info.HasAlpha {
		t.Errorf("opaque NRGBA png should scan as opaque: %+v", info)
	}
}

// TestProbeRotatedMP4Integration builds a portrait phone-style MP4 (coded
// 160x90 with a 90 degree Display Matrix, as ffmpeg >= 6 writes with
// -display_rotation) and checks the probe reports the displayed 90x160.
func TestProbeRotatedMP4Integration(t *testing.T) {
	tools := toolsOrSkip(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src := filepath.Join(dir, "portrait.mp4")
	cmd := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-noautorotate", "-display_rotation", "90",
		"-f", "lavfi", "-i", "color=c=red:s=160x90:r=10:d=1",
		"-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build a rotated mp4 with this ffmpeg (needs >= 6.0 for -display_rotation): %v\n%s", err, out)
	}
	info, err := Probe(ctx, tools, src, 0)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Width != 90 || info.Height != 160 {
		t.Errorf("rotated mp4 dims = %dx%d, want the displayed 90x160 (%+v)", info.Width, info.Height, info)
	}
	if info.Kind != recipe.KindVideo || info.Frames != 10 || info.FPS != 10 || info.HasAlpha {
		t.Errorf("rotated mp4: %+v", info)
	}
	// The same clip without the matrix keeps its coded size.
	plain := filepath.Join(dir, "plain.mp4")
	cmd = exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=160x90:r=10:d=1",
		"-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", plain)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plain mp4: %v\n%s", err, out)
	}
	info, err = Probe(ctx, tools, plain, 0)
	if err != nil {
		t.Fatalf("Probe plain: %v", err)
	}
	if info.Width != 160 || info.Height != 90 {
		t.Errorf("plain mp4 dims = %dx%d, want 160x90", info.Width, info.Height)
	}
}

// TestProbeOneFrameMOVIntegration builds one-frame MOVs (ProRes 4444 with
// alpha, and an opaque mpeg4) and checks they probe as stills.
func TestProbeOneFrameMOVIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	build := func(name string, args ...string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
		full = append(full, "-frames:v", "1", p)
		if out, err := exec.CommandContext(ctx, tools.FFmpeg, full...).CombinedOutput(); err != nil {
			t.Skipf("cannot build %s with this ffmpeg: %v\n%s", name, err, out)
		}
		return p
	}

	opaque := build("one.mp4", "-f", "lavfi", "-i", "color=c=red:s=64x48:r=25:d=1", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p")
	info, err := Probe(ctx, tools, opaque, 0)
	if err != nil {
		t.Fatalf("Probe mpeg4: %v", err)
	}
	if !info.IsStill || info.Kind != recipe.KindImage || info.Frames != 1 || info.FPS != 0 || info.Duration != 0 {
		t.Errorf("one-frame mpeg4 mov must be a still: %+v", info)
	}
	if info.Width != 64 || info.Height != 48 || info.HasAlpha || info.Codec != "mpeg4" {
		t.Errorf("one-frame mpeg4 facts: %+v", info)
	}

	prores := build("one.mov", "-f", "lavfi", "-i", "color=c=red@0.5:s=64x48:r=25:d=1,format=rgba",
		"-c:v", "prores_ks", "-profile:v", "4444", "-pix_fmt", "yuva444p10le")
	info, err = Probe(ctx, tools, prores, 0)
	if err != nil {
		t.Fatalf("Probe prores: %v", err)
	}
	if !info.IsStill || info.Kind != recipe.KindImage || info.Frames != 1 || info.FPS != 0 || info.Duration != 0 {
		t.Errorf("one-frame prores mov must be a still: %+v", info)
	}
	if info.Codec != "prores" || !info.HasAlpha || !info.Premultiplied || info.Bits < 10 {
		t.Errorf("one-frame prores 4444 facts: %+v", info)
	}

	// The same encoder with several frames stays a video.
	clip := filepath.Join(dir, "clip.mp4")
	cmd := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=64x48:r=25:d=0.2", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", clip)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build clip: %v\n%s", err, out)
	}
	info, err = Probe(ctx, tools, clip, 0)
	if err != nil {
		t.Fatalf("Probe clip: %v", err)
	}
	if info.IsStill || info.Kind != recipe.KindVideo || info.Frames != 5 || info.FPS != 25 {
		t.Errorf("5-frame clip: %+v", info)
	}
}

func TestProbeGIFIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 0, 0}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < 4; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, 16, 16), pal)
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				fr.SetColorIndex(x, y, uint8(1))
			}
		}
		fr.SetColorIndex(i, 0, 2) // one transparent pixel per frame
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 10) // 10 cs = 10 fps
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "anim.gif")
	os.WriteFile(p, buf.Bytes(), 0o644)

	info, err := Probe(ctx, tools, p, 0)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Codec != "gif" || info.Kind != recipe.KindAnimation || info.IsStill {
		t.Errorf("gif: %+v", info)
	}
	if info.Width != 16 || info.Height != 16 {
		t.Errorf("gif dims: %+v", info)
	}
	if info.Frames != 4 {
		t.Errorf("gif frames = %d, want 4 (%+v)", info.Frames, info)
	}
	if info.FPS < 9.5 || info.FPS > 10.5 {
		t.Errorf("gif fps = %v, want ~10", info.FPS)
	}
	if !info.HasAlpha {
		t.Errorf("gif with transparent pixels: HasAlpha false: %+v", info)
	}
}

// TestProbeAVIFIntegration builds animated and still AVIFs and checks the
// probe describes the animation track (not the one-frame primary item
// libavif and ffmpeg's avif muxer both write first) and finds the alpha
// stream. With avifenc on PATH (the tools image) the files carry alpha;
// otherwise ffmpeg's own avif muxer provides the opaque layout.
func TestProbeAVIFIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	avifenc, _ := exec.LookPath("avifenc")
	anim, still := filepath.Join(dir, "anim.avif"), filepath.Join(dir, "still.avif")
	wantAlpha := false
	if avifenc != "" {
		// Half-transparent RGBA frames → avifenc (alpha as a separate plane).
		if out, err := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=1,format=rgba,geq=r=r(X\\,Y):g=g(X\\,Y):b=b(X\\,Y):a=if(lt(X\\,32)\\,255\\,128)",
			"-c:v", "png", filepath.Join(dir, "f%03d.png")).CombinedOutput(); err != nil {
			t.Fatalf("frames: %v\n%s", err, out)
		}
		frames, _ := filepath.Glob(filepath.Join(dir, "f*.png"))
		args := append([]string{"-j", "all", "-s", "8", "-q", "60", "--qalpha", "90", "-y", "420", "--fps", "10", "--repetition-count", "infinite"}, frames...)
		if out, err := exec.CommandContext(ctx, avifenc, append(args, anim)...).CombinedOutput(); err != nil {
			t.Skipf("avifenc cannot build an animated avif: %v\n%s", err, out)
		}
		if out, err := exec.CommandContext(ctx, avifenc, "-j", "all", "-s", "6", "-q", "60", "--qalpha", "90", "-y", "420", frames[0], still).CombinedOutput(); err != nil {
			t.Skipf("avifenc cannot build a still avif: %v\n%s", err, out)
		}
		wantAlpha = true
	} else {
		if out, err := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=1", "-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "30", "-f", "avif", anim).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot build an animated avif (no libaom?): %v\n%s", err, out)
		}
		if out, err := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=1", "-frames:v", "1", "-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "30", "-f", "avif", still).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot build a still avif: %v\n%s", err, out)
		}
	}

	info, err := Probe(ctx, tools, anim, 0)
	if err != nil {
		t.Fatalf("Probe anim: %v", err)
	}
	t.Logf("animated avif (alpha=%v): %+v", wantAlpha, info)
	if info.Kind != recipe.KindAnimation || info.IsStill || info.Frames != 10 || info.FPS != 10 {
		t.Errorf("animated avif must be described by its 10-frame track: %+v", info)
	}
	if info.Width != 64 || info.Height != 48 || info.Codec != "av1" {
		t.Errorf("animated avif facts: %+v", info)
	}
	if info.HasAlpha != wantAlpha || (wantAlpha && info.AlphaStream == 0) || (!wantAlpha && info.AlphaStream != 0) {
		t.Errorf("animated avif alpha: has=%v stream=%d, want alpha=%v", info.HasAlpha, info.AlphaStream, wantAlpha)
	}
	if wantAlpha && info.AlphaStream != 3 {
		t.Errorf("avifenc layout: alpha track is v:3, got %d", info.AlphaStream)
	}

	info, err = Probe(ctx, tools, still, 0)
	if err != nil {
		t.Fatalf("Probe still: %v", err)
	}
	t.Logf("still avif (alpha=%v): %+v", wantAlpha, info)
	if !info.IsStill || info.Kind != recipe.KindImage || info.Frames != 1 || info.FPS != 0 {
		t.Errorf("still avif: %+v", info)
	}
	if info.HasAlpha != wantAlpha || (wantAlpha && info.AlphaStream != 1) || (!wantAlpha && info.AlphaStream != 0) {
		t.Errorf("still avif alpha: has=%v stream=%d, want alpha=%v", info.HasAlpha, info.AlphaStream, wantAlpha)
	}
}

// TestProbeMonochromeAVIFIntegration builds a monochrome (yuv400) animated
// AVIF — ffmpeg's avif muxer writes [gray Color item (1 frame), gray track]
// — and checks the probe describes the 10-frame track, not a 1-frame still
// of the primary item (pix_fmt gray must not disqualify every stream as an
// alpha plane).
func TestProbeMonochromeAVIFIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	anim := filepath.Join(dir, "gray.avif")
	if out, err := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=1", "-vf", "format=gray",
		"-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "30", "-pix_fmt", "gray", "-f", "avif", anim).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot build a monochrome animated avif (no libaom / no monochrome?): %v\n%s", err, out)
	}
	info, err := Probe(ctx, tools, anim, 0)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("monochrome animated avif: %+v", info)
	if info.Kind != recipe.KindAnimation || info.IsStill || info.Frames != 10 || info.FPS != 10 {
		t.Errorf("monochrome animated avif reduced to a still of the primary item: %+v", info)
	}
	if info.ColorStream != 1 || info.AlphaStream != 0 || info.HasAlpha {
		t.Errorf("monochrome avif streams: colour v:%d alpha v:%d has=%v", info.ColorStream, info.AlphaStream, info.HasAlpha)
	}
	if info.Width != 64 || info.Height != 48 || info.Codec != "av1" {
		t.Errorf("monochrome avif facts: %+v", info)
	}
}

// writeSeqPNG writes a w x h opaque (or one-pixel-transparent) PNG as
// sequence frame n of dir.
func writeSeqPNG(t *testing.T, dir string, n, w, h int, transparent bool) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(40 * n), G: 90, B: 160, A: 255})
		}
	}
	if transparent {
		img.Set(0, 0, color.NRGBA{A: 0})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, fmt.Sprintf("%06d.png", n))
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProbeSequenceIntegration probes real PNG sequences: a uniform opaque
// one, one with a transparent pixel in a late frame, and one whose frames
// differ in size (Mixed, largest frame wins).
func TestProbeSequenceIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Uniform, opaque, 5 frames at the default delay.
	uni := t.TempDir()
	for n := 1; n <= 5; n++ {
		writeSeqPNG(t, uni, n, 16, 12, false)
	}
	info, err := ProbeSequence(ctx, tools, uni, 0)
	if err != nil {
		t.Fatalf("ProbeSequence: %v", err)
	}
	t.Logf("uniform: %+v seq=%+v", info, info.Sequence)
	if info.Kind != recipe.KindSequence || info.IsStill || info.Format != "image2" || info.Codec != "png" {
		t.Errorf("kind/format: %+v", info)
	}
	if info.Width != 16 || info.Height != 12 || info.Frames != 5 || info.FPS != 10 || info.Duration != 0.5 {
		t.Errorf("geometry/timing: %+v", info)
	}
	if info.Sequence == nil || info.Sequence.Count != 5 || info.Sequence.Pattern != "%06d.png" || info.Sequence.DelayMS != 100 || info.Sequence.Mixed {
		t.Errorf("sequence info: %+v", info.Sequence)
	}
	if info.HasAlpha || info.Premultiplied || info.HasAudio {
		t.Errorf("opaque NRGBA frames: %+v", info)
	}
	if info.PixFmt == "" || info.Bits != 8 {
		t.Errorf("pix facts: %+v", info)
	}

	// Custom delay, transparent pixel only in the last frame (sampled).
	alpha := t.TempDir()
	for n := 1; n <= 4; n++ {
		writeSeqPNG(t, alpha, n, 16, 12, n == 4)
	}
	info, err = ProbeSequence(ctx, tools, alpha, 40)
	if err != nil {
		t.Fatalf("ProbeSequence alpha: %v", err)
	}
	if !info.HasAlpha {
		t.Errorf("transparent last frame not found: %+v", info)
	}
	if info.FPS != 25 || info.Duration != 0.16 || info.Sequence.DelayMS != 40 {
		t.Errorf("delay 40 ms: fps=%v dur=%v seq=%+v", info.FPS, info.Duration, info.Sequence)
	}

	// Mixed sizes: the largest frame sets Width/Height.
	mixed := t.TempDir()
	writeSeqPNG(t, mixed, 1, 16, 12, false)
	writeSeqPNG(t, mixed, 2, 24, 10, false)
	writeSeqPNG(t, mixed, 3, 8, 30, false)
	info, err = ProbeSequence(ctx, tools, mixed, 0)
	if err != nil {
		t.Fatalf("ProbeSequence mixed: %v", err)
	}
	if info.Width != 24 || info.Height != 30 || !info.Sequence.Mixed || info.Frames != 3 {
		t.Errorf("mixed: %+v seq=%+v", info, info.Sequence)
	}

	// A stray file in the dir is ignored; an empty dir is an error.
	os.WriteFile(filepath.Join(uni, "notes.txt"), []byte("x"), 0o644)
	if info, err := ProbeSequence(ctx, tools, uni, 0); err != nil || info.Frames != 5 {
		t.Errorf("stray file: %+v %v", info, err)
	}
	if _, err := ProbeSequence(ctx, tools, t.TempDir(), 0); err == nil {
		t.Error("empty dir accepted")
	}
}

// TestProbeSequenceFFprobeDims covers the non-stdlib path (ffprobe over the
// image2 pattern) with a BMP sequence.
func TestProbeSequenceFFprobeDims(t *testing.T) {
	tools := toolsOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	build := func(n, w, h int) {
		t.Helper()
		out, err := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("color=c=blue:s=%dx%d:r=1:d=1", w, h), "-frames:v", "1",
			"-c:v", "bmp", filepath.Join(dir, fmt.Sprintf("%06d.bmp", n))).CombinedOutput()
		if err != nil {
			t.Skipf("cannot build a bmp: %v\n%s", err, out)
		}
	}
	build(1, 20, 10)
	build(2, 20, 10)
	build(3, 30, 8)
	info, err := ProbeSequence(ctx, tools, dir, 50)
	if err != nil {
		t.Fatalf("ProbeSequence bmp: %v", err)
	}
	t.Logf("bmp: %+v seq=%+v", info, info.Sequence)
	if info.Codec != "bmp" || info.Width != 30 || info.Height != 10 || !info.Sequence.Mixed || info.Frames != 3 || info.FPS != 20 {
		t.Errorf("bmp sequence: %+v seq=%+v", info, info.Sequence)
	}
	if info.HasAlpha {
		t.Errorf("bmp has no alpha: %+v", info)
	}
}

// pngChunk appends one PNG chunk (length, type, data, CRC) to buf.
func pngChunk(buf *bytes.Buffer, typ string, data []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(data)))
	copy(hdr[4:], typ)
	buf.Write(hdr[:])
	buf.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc.Sum32())
	buf.Write(tail[:])
}

// writeRGBTrnsPNG writes a truecolour (colour type 2) PNG with a tRNS colour
// key — a layout Go's encoder never produces and whose DecodeConfig model is
// the opaque RGBAModel (the config decode stops at IHDR). keyUsed paints
// pixel (0,0) in the key colour, which ffmpeg decodes as transparent.
func writeRGBTrnsPNG(t *testing.T, path string, w, h int, keyUsed bool) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // colour type: truecolour
	pngChunk(&buf, "IHDR", ihdr)
	// Colour key (255, 0, 255), one 16-bit sample per channel.
	pngChunk(&buf, "tRNS", []byte{0, 255, 0, 0, 0, 255})
	var raw bytes.Buffer
	for y := 0; y < h; y++ {
		raw.WriteByte(0) // filter: none
		for x := 0; x < w; x++ {
			if keyUsed && x == 0 && y == 0 {
				raw.Write([]byte{255, 0, 255})
			} else {
				raw.Write([]byte{10, 20, 200})
			}
		}
	}
	var idat bytes.Buffer
	zw := zlib.NewWriter(&idat)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	pngChunk(&buf, "IDAT", idat.Bytes())
	pngChunk(&buf, "IEND", nil)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSeqGIF writes one single-frame GIF file as sequence frame n of dir.
// When transparent, the palette carries a fully transparent entry used by
// pixel (0,0), so the encoder writes the GCE transparency flag — which a
// config-only decode never sees (the global colour table is opaque either
// way).
func writeSeqGIF(t *testing.T, dir string, n int, transparent bool) {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{200, 30, 30, 255}}
	if transparent {
		pal = append(pal, color.RGBA{})
	}
	fr := image.NewPaletted(image.Rect(0, 0, 16, 12), pal)
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			fr.SetColorIndex(x, y, 1)
		}
	}
	if transparent {
		fr.SetColorIndex(0, 0, 2)
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, fr, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.gif", n)), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHeaderAlphaFacts pins the format-aware alpha-candidate decision on
// real files: GIF frames and colour-keyed truecolour PNGs must be scan
// candidates even though their stdlib colour models look opaque.
func TestHeaderAlphaFacts(t *testing.T) {
	dir := t.TempDir()

	keyed := filepath.Join(dir, "keyed.png")
	writeRGBTrnsPNG(t, keyed, 8, 6, false)
	if !pngHasTRNS(keyed) {
		t.Error("tRNS chunk missed")
	}
	if ff, err := stdlibFrameFacts(keyed); err != nil || !ff.alphaP || ff.w != 8 || ff.h != 6 {
		t.Errorf("colour-keyed png facts = %+v (%v)", ff, err)
	}

	// Plain opaque truecolour PNG (Go writes colour type 2 for an opaque
	// RGBA image): no tRNS, not a candidate.
	plain := filepath.Join(dir, "plain.png")
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{10, 20, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if pngHasTRNS(plain) {
		t.Error("plain truecolour png reported tRNS")
	}
	if ff, err := stdlibFrameFacts(plain); err != nil || ff.alphaP {
		t.Errorf("plain truecolour png must not be a candidate: %+v (%v)", ff, err)
	}

	// Every GIF frame is a candidate: the transparent index lives in the
	// frame's GCE, invisible to DecodeConfig.
	writeSeqGIF(t, dir, 1, false)
	if ff, err := stdlibFrameFacts(filepath.Join(dir, "000001.gif")); err != nil || !ff.alphaP {
		t.Errorf("gif frame must be an alpha candidate: %+v (%v)", ff, err)
	}

	// jpeg can never carry alpha; unknown formats fall back to the model.
	if headerAdmitsAlpha("nope", "jpeg", color.YCbCrModel) {
		t.Error("jpeg admits alpha")
	}
	if !headerAdmitsAlpha("nope", "bmp", color.NRGBAModel) {
		t.Error("NRGBA model must stay a candidate for any format")
	}
	if pngHasTRNS(filepath.Join(dir, "missing.png")) {
		t.Error("missing file has tRNS")
	}
}

// TestProbeSequenceLateAlphaIntegration: transparency that only later frames
// carry must be found — GIF frames hide the transparent index in each
// frame's GCE and truecolour PNGs in a tRNS colour key, both invisible to
// the stdlib header models, so an opaque first frame must not suppress the
// alpha scan of the rest.
func TestProbeSequenceLateAlphaIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// GIF: frame 1 opaque, frames 2-3 with a transparent pixel.
	gifDir := t.TempDir()
	writeSeqGIF(t, gifDir, 1, false)
	writeSeqGIF(t, gifDir, 2, true)
	writeSeqGIF(t, gifDir, 3, true)
	info, err := ProbeSequence(ctx, tools, gifDir, 0)
	if err != nil {
		t.Fatalf("ProbeSequence gif: %v", err)
	}
	if !info.HasAlpha {
		t.Errorf("gif frames with late transparency: HasAlpha false: %+v", info)
	}
	if info.Codec != "gif" || info.Frames != 3 || info.Width != 16 || info.Height != 12 {
		t.Errorf("gif sequence facts: %+v", info)
	}

	// Fully opaque GIF frames still scan as opaque (every gif header is a
	// candidate, but the scan decides).
	opaqueDir := t.TempDir()
	for n := 1; n <= 3; n++ {
		writeSeqGIF(t, opaqueDir, n, false)
	}
	info, err = ProbeSequence(ctx, tools, opaqueDir, 0)
	if err != nil {
		t.Fatalf("ProbeSequence opaque gif: %v", err)
	}
	if info.HasAlpha {
		t.Errorf("opaque gif frames: HasAlpha true: %+v", info)
	}

	// RGB + tRNS PNG: key unused in frame 1, used in frames 2-3.
	pngDir := t.TempDir()
	writeRGBTrnsPNG(t, filepath.Join(pngDir, "000001.png"), 8, 6, false)
	writeRGBTrnsPNG(t, filepath.Join(pngDir, "000002.png"), 8, 6, true)
	writeRGBTrnsPNG(t, filepath.Join(pngDir, "000003.png"), 8, 6, true)
	info, err = ProbeSequence(ctx, tools, pngDir, 0)
	if err != nil {
		t.Fatalf("ProbeSequence rgb+tRNS: %v", err)
	}
	if !info.HasAlpha {
		t.Errorf("colour-keyed png frames with late transparency: HasAlpha false: %+v", info)
	}
}

// TestProbeSequenceUnreadableIntegration: a sequence stored under an
// extension whose frames the image2 demuxer cannot decode probes its first
// frame fine standalone (ffprobe auto-detects the real format) but is
// unreadable through the pattern the render opens; ProbeSequence must
// reject it as an unreadable source (an ffprobe exit error or the "has no
// dimensions" marker, which the server maps to a 422 + blob discard), not
// assume a uniform renderable sequence.
func TestProbeSequenceUnreadableIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := t.TempDir()
	for n := 1; n <= 2; n++ {
		// Real GIF bytes behind ".tga": image2 picks the targa decoder from
		// the extension and decodes nothing.
		pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{200, 30, 30, 255}}
		fr := image.NewPaletted(image.Rect(0, 0, 8, 6), pal)
		var buf bytes.Buffer
		if err := gif.Encode(&buf, fr, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.tga", n)), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ProbeSequence(ctx, tools, dir, 0)
	if err == nil {
		t.Fatal("unreadable sequence accepted")
	}
	t.Logf("rejected with: %v", err)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) && !strings.Contains(err.Error(), "has no dimensions") {
		t.Errorf("error must classify as an unreadable source (exit error or marker): %v", err)
	}
}

func TestSampleIndices(t *testing.T) {
	if got := sampleIndices(3, 5); len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Errorf("small: %v", got)
	}
	got := sampleIndices(1000, 5)
	if len(got) != 5 || got[0] != 0 || got[4] != 999 || got[2] != 499 {
		t.Errorf("spread: %v", got)
	}
	if got := sampleIndices(1, 5); len(got) != 1 || got[0] != 0 {
		t.Errorf("one: %v", got)
	}
	if got := sampleIndices(0, 5); got != nil {
		t.Errorf("none: %v", got)
	}
	if got := sampleIndices(10, 1); len(got) != 1 || got[0] != 0 {
		t.Errorf("single sample: %v", got)
	}
	for _, c := range []struct{ ext, want string }{{"png", "%06d.png"}, {"tif", "%06d.tif"}} {
		if got := sequencePattern(c.ext); got != c.want {
			t.Errorf("sequencePattern(%q) = %q", c.ext, got)
		}
	}
}

func TestSequenceFrameFactHelpers(t *testing.T) {
	// Largest frame + mixed flag.
	if w, h, mixed := largestFrame([]frameFacts{{w: 16, h: 12}, {w: 24, h: 10}, {w: 8, h: 30}}); w != 24 || h != 30 || !mixed {
		t.Errorf("largestFrame mixed = %d x %d %v", w, h, mixed)
	}
	if w, h, mixed := largestFrame([]frameFacts{{w: 16, h: 12}, {w: 16, h: 12}}); w != 16 || h != 12 || mixed {
		t.Errorf("largestFrame uniform = %d x %d %v", w, h, mixed)
	}
	// Colour models.
	if !modelHasAlpha(color.NRGBAModel) || !modelHasAlpha(color.NRGBA64Model) {
		t.Error("NRGBA models carry alpha")
	}
	if modelHasAlpha(color.RGBAModel) || modelHasAlpha(color.YCbCrModel) || modelHasAlpha(color.GrayModel) {
		t.Error("opaque models alone must not count (format-level cases — GIF GCE, PNG tRNS keys — go through headerAdmitsAlpha)")
	}
	if modelHasAlpha(color.Palette{color.RGBA{1, 2, 3, 255}}) || !modelHasAlpha(color.Palette{color.RGBA{1, 2, 3, 255}, color.RGBA{0, 0, 0, 0}}) {
		t.Error("palette alpha detection")
	}
	// Candidates: only frames whose header admits alpha, spread, at most 5.
	facts := []frameFacts{{index: 0}, {index: 1, alphaP: true}, {index: 2}, {index: 3, alphaP: true}}
	if got := alphaCandidates(facts, false, 4); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("candidates = %v", got)
	}
	if got := alphaCandidates(facts, true, 4); len(got) != 3 || got[0] != 0 {
		t.Errorf("first frame pix_fmt admits alpha: %v", got)
	}
	if got := alphaCandidates([]frameFacts{{index: 0}, {index: 1}}, false, 2); got != nil {
		t.Errorf("no alpha anywhere: %v", got)
	}
	many := make([]frameFacts, 100)
	for i := range many {
		many[i] = frameFacts{index: i, alphaP: true}
	}
	if got := alphaCandidates(many, true, 100); len(got) != maxSequenceAlphaSamples || got[0] != 0 || got[len(got)-1] != 99 {
		t.Errorf("spread candidates = %v", got)
	}
	// No headers read: fall back to the first frame's pix_fmt.
	if got := alphaCandidates(nil, true, 10); len(got) != maxSequenceAlphaSamples {
		t.Errorf("no facts, alpha pix_fmt: %v", got)
	}
	if got := alphaCandidates(nil, false, 10); got != nil {
		t.Errorf("no facts, opaque pix_fmt: %v", got)
	}
}

// TestProbeAnimatedWebPIntegration covers the webp_anim demuxer, which
// reports neither duration nor nb_frames: the probe must count frames and
// derive Duration so the UI does not mistake the animation for a still.
func TestProbeAnimatedWebPIntegration(t *testing.T) {
	tools := toolsOrSkip(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := filepath.Join(dir, "anim.webp")
	out, err := exec.CommandContext(ctx, tools.FFmpeg, "-hide_banner", "-nostdin", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=32x32:rate=10:duration=2,format=rgba",
		"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "60", "-loop", "0", "-f", "webp", p).CombinedOutput()
	if err != nil {
		t.Skipf("cannot build an animated webp with this ffmpeg: %v\n%s", err, out)
	}

	info, err := Probe(ctx, tools, p, 0)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// FFmpeg 9's native animated decoder reports codec "webp_anim"; older
	// builds say "webp".
	if (info.Codec != "webp" && info.Codec != "webp_anim") || info.Kind != recipe.KindAnimation || info.IsStill {
		t.Errorf("animated webp: %+v", info)
	}
	if info.Frames != 20 {
		t.Errorf("frames = %d, want 20 (%+v)", info.Frames, info)
	}
	if info.FPS < 9.5 || info.FPS > 10.5 {
		t.Errorf("fps = %v, want ~10", info.FPS)
	}
	if info.Duration < 1.9 || info.Duration > 2.1 {
		t.Errorf("duration = %v, want ~2 s derived from 20 frames at 10 fps (%+v)", info.Duration, info)
	}
}
