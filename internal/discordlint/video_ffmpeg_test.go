package discordlint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live-encoder check: build a fresh 2-frame MP4/WebM with whatever ffmpeg
// is on PATH (the committed testdata fixtures cover the parsing paths when
// there is none) and lint it — guards against the probes drifting from
// real muxer output.

// videoFFmpegOrSkip returns the ffmpeg binary or skips the test.
func videoFFmpegOrSkip(t *testing.T) string {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	return ff
}

func TestLintVideoFFmpeg(t *testing.T) {
	ff := videoFFmpegOrSkip(t)
	dir := t.TempDir()
	cases := []struct {
		format string
		args   []string
	}{
		{"mp4", []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "30", "-pix_fmt", "yuv420p", "-movflags", "+faststart"}},
		{"webm", []string{"-c:v", "libvpx-vp9", "-crf", "40", "-b:v", "0", "-cpu-used", "8", "-row-mt", "1", "-pix_fmt", "yuv420p"}},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			out := filepath.Join(dir, "out."+tc.format)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-frames:v", "2", "-an"}, tc.args...)
			args = append(args, out)
			if o, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
				if tc.format == "webm" && strings.Contains(string(o), "Unknown encoder") {
					t.Skipf("ffmpeg has no libvpx-vp9: %s", o)
				}
				t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, o)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			r := lintVideo(t, tc.format, data, TargetAttachment)
			if !r.OK {
				t.Errorf("fresh ffmpeg %s does not lint clean: %+v", tc.format, r.Checks)
			}
			if r.Width != 64 || r.Height != 64 || r.Frames != 2 || r.DurationMS < 100 || r.DurationMS > 500 {
				t.Errorf("parsed values: w=%d h=%d frames=%d duration=%d", r.Width, r.Height, r.Frames, r.DurationMS)
			}
			for _, c := range r.Checks {
				if !c.OK {
					t.Errorf("%s: %s", c.Rule, c.Detail)
				}
			}
		})
	}
}
