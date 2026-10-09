package enc

import "testing"

// Phase 5d golden: the mask-prompt scale argv (one stored matte frame to
// the tracking size, into a one-file image2 sequence). The ffmpeg check is
// in matte_5d_ffmpeg_test.go.

func TestMatteMaskScaleArgs(t *testing.T) {
	in, out := "/data/mattes/edge-k/000004.png", "/data/mattes/edge-k/mask-f3-1024x576.tmp"
	want := func(scale string) []string {
		return []string{
			"-i", in,
			"-vf", "scale=" + scale + ":flags=bicubic,format=gray",
			"-frames:v", "1",
			"-pix_fmt", "gray",
			"-f", "image2", out + "/%06d.png",
		}
	}
	t.Run("the tracking size", func(t *testing.T) {
		assertArgs(t, MatteMaskScaleArgs(in, out, 1024, 576), want("1024:576"))
	})
	t.Run("a portrait size", func(t *testing.T) {
		assertArgs(t, MatteMaskScaleArgs(in, out, 576, 1024), want("576:1024"))
	})
	t.Run("either separator is joined with a slash", func(t *testing.T) {
		got := MatteMaskScaleArgs(`C:\data\mattes\k\000001.png`, `C:\data\mattes\k\mask-f0-32x32.tmp\`, 32, 32)
		if got[1] != `C:\data\mattes\k\000001.png` || got[len(got)-1] != `C:\data\mattes\k\mask-f0-32x32.tmp/%06d.png` {
			t.Errorf("paths %q and %q", got[1], got[len(got)-1])
		}
	})
	t.Run("no path, no dir or no size yields nil", func(t *testing.T) {
		for _, tc := range []struct {
			in, out string
			w, h    int
		}{
			{"", out, 32, 32},
			{in, "", 32, 32},
			{in, out, 0, 32},
			{in, out, 32, 0},
			{in, out, -2, -2},
		} {
			if got := MatteMaskScaleArgs(tc.in, tc.out, tc.w, tc.h); got != nil {
				t.Errorf("MatteMaskScaleArgs(%q, %q, %d, %d) = %q, want nil", tc.in, tc.out, tc.w, tc.h, got)
			}
		}
	})
}
