package enc

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/graph"
)

// Phase 5c goldens: the stabilisation argv (both chains verbatim), the
// tracker's source stream and one-slot still, the gate argv, the derived
// Path of a stabilised / gated matte input honoured by every consumer form,
// and TrackSize. The ffmpeg checks of the chains are in
// matte_5c_ffmpeg_test.go.

const (
	stabLightChain  = "tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1"
	stabStrongChain = stabLightChain + ",lagfun=decay=0.7"
)

func TestMatteStabiliseArgs(t *testing.T) {
	in, out := "/data/mattes/9df7bf02b4738b9d", "/data/mattes/9df7bf02b4738b9d/stab-light.tmp"
	want := func(chain string, n string) []string {
		return []string{
			"-f", "image2", "-framerate", "1", "-i", in + "/%06d.png",
			"-vf", chain,
			"-fps_mode", "passthrough",
			"-frames:v", n,
			"-pix_fmt", "gray",
			"-f", "image2", out + "/%06d.png",
		}
	}
	t.Run("light", func(t *testing.T) {
		assertArgs(t, MatteStabiliseArgs(in, out, 45, MatteStabiliseLight), want(stabLightChain, "45"))
	})
	t.Run("strong", func(t *testing.T) {
		assertArgs(t, MatteStabiliseArgs(in, out, 1, MatteStabiliseStrong), want(stabStrongChain, "1"))
	})
	t.Run("the modes alias the recipe's", func(t *testing.T) {
		if MatteStabiliseLight != "light" || MatteStabiliseStrong != "strong" {
			t.Errorf("modes %q / %q", MatteStabiliseLight, MatteStabiliseStrong)
		}
	})
	t.Run("either separator is joined with a slash", func(t *testing.T) {
		got := MatteStabiliseArgs(`C:\data\mattes\k\`, "/data/mattes/k/stab-strong.tmp/", 3, MatteStabiliseStrong)
		if got[5] != `C:\data\mattes\k/%06d.png` || got[len(got)-1] != "/data/mattes/k/stab-strong.tmp/%06d.png" {
			t.Errorf("paths %q and %q", got[5], got[len(got)-1])
		}
	})
	t.Run("unknown mode, off, no frames or no dir yields nil", func(t *testing.T) {
		for _, tc := range []struct {
			in, out string
			frames  int
			mode    string
		}{
			{in, out, 45, ""},
			{in, out, 45, "medium"},
			{in, out, 45, "Light"},
			{in, out, 0, MatteStabiliseLight},
			{in, out, -1, MatteStabiliseStrong},
			{"", out, 45, MatteStabiliseLight},
			{in, "", 45, MatteStabiliseLight},
		} {
			if got := MatteStabiliseArgs(tc.in, tc.out, tc.frames, tc.mode); got != nil {
				t.Errorf("MatteStabiliseArgs(%q, %q, %d, %q) = %q, want nil", tc.in, tc.out, tc.frames, tc.mode, got)
			}
		}
	})
}

// trackStreamTail is the rgb24 stream tail at scale (the same shape as
// MatteSourceArgs', the square replaced by the tracking size).
func trackStreamTail(filter, scale string) []string {
	return []string{
		"-filter_complex", filter + ";[out]format=rgb24,scale=" + scale + ":flags=bicubic[mi]",
		"-map", "[mi]",
		"-an", "-sn", "-dn",
		"-f", "rawvideo", "-pix_fmt", "rgb24",
		"pipe:1",
	}
}

func TestMatteTrackSourceArgs(t *testing.T) {
	const planFilter = "[0:v]fps=25,format=rgba[out]"
	t.Run("video plan at the tracking size", func(t *testing.T) {
		p := testPlan()
		p.Filter = planFilter
		got := MatteTrackSourceArgs(mainSrc, p, 1024, 576)
		assertArgs(t, got, cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, trackStreamTail(planFilter, "1024:576")))
	})
	t.Run("image sequence plan joins the pattern", func(t *testing.T) {
		p := testPlan()
		p.Filter = planFilter
		p.InputArgs = []string{"-f", "image2", "-framerate", "10", "-start_number", "1", "-reinit_filter", "0"}
		p.InputPattern = "%06d.png"
		got := MatteTrackSourceArgs("/data/blobs/ab/abcd", p, 320, 240)
		assertArgs(t, got, cat(p.InputArgs, []string{"-i", "/data/blobs/ab/abcd/%06d.png"}, trackStreamTail(planFilter, "320:240")))
	})
	t.Run("default out label", func(t *testing.T) {
		p := &graph.Plan{Filter: "[0:v]format=rgba[out]"}
		assertArgs(t, MatteTrackSourceArgs("in.gif", p, 576, 1024), cat([]string{"-i", "in.gif"}, trackStreamTail("[0:v]format=rgba[out]", "576:1024")))
	})
	t.Run("the segmenter stream is the same argv at a square", func(t *testing.T) {
		// MatteSourceArgs (Phase 5b) shares the body: its golden stays
		// byte-identical (TestMatteSourceArgs) and equals the tracker form
		// at size x size.
		p := testPlan()
		p.Filter = planFilter
		assertArgs(t, MatteSourceArgs(mainSrc, p, 512), MatteTrackSourceArgs(mainSrc, p, 512, 512))
	})
	t.Run("nil, unusable plan or no size yields nil", func(t *testing.T) {
		if got := MatteTrackSourceArgs(mainSrc, nil, 1024, 576); got != nil {
			t.Errorf("nil plan: %q", got)
		}
		for _, wh := range [][2]int{{0, 576}, {1024, 0}, {-2, -2}} {
			if got := MatteTrackSourceArgs(mainSrc, testPlan(), wh[0], wh[1]); got != nil {
				t.Errorf("size %v: %q", wh, got)
			}
		}
		unbound := testPlan()
		unbound.Filter = "[0:v]fps=25,format=rgba,drawtext=fontfile=/f.ttf:textfile=__EZLG_TEXT_0:fontsize=32[out]"
		if got := MatteTrackSourceArgs(mainSrc, unbound, 1024, 576); got != nil {
			t.Errorf("unbound placeholder: %q", got)
		}
		p := withMatte(testPlan(), matteFilter, 62)
		p.ExtraInputs[0].Path = ""
		if got := MatteTrackSourceArgs(mainSrc, p, 1024, 576); got != nil {
			t.Errorf("matte input without a Path: %q", got)
		}
	})
}

// TestMatteTrackFrameArgs: one output slot of the matte plan as a seeked
// forward still at the MIDDLE of the slot (slot 25 of testPlan = t 1.02:
// the seek-back lands one slot closer than TestStillArgs' t=1 still — -ss
// 2.38 = slot 22, three slots of pad — and selects the same frame by the
// same threshold 0.98) with the tracker stream's tail and -frames:v 1.
func TestMatteTrackFrameArgs(t *testing.T) {
	const planFilter = "[0:v]fps=25,format=rgba[out]"
	frameTail := func(filter, pad, threshold, scale string) []string {
		return []string{
			"-frames:v", "1",
			"-filter_complex", filter + ";[out]tpad=stop_mode=clone:stop_duration=" + pad + ",select='gte(t," + threshold + ")',format=rgb24,scale=" + scale + ":flags=bicubic[mi]",
			"-map", "[mi]",
			"-an", "-sn", "-dn",
			"-f", "rawvideo", "-pix_fmt", "rgb24",
			"pipe:1",
		}
	}
	plan := func() *graph.Plan { p := testPlan(); p.Filter = planFilter; return p }
	tests := []struct {
		name string
		plan func() *graph.Plan
		slot int
		lead []string
		pad  string
		thr  string
	}{
		{name: "slot 0 reads the trim start", plan: plan, slot: 0, lead: []string{"-ss", "1.5"}, pad: "1", thr: "0"},
		{name: "slot 25 seeks to slot 22 and selects by the t=1 threshold", plan: plan, slot: 25, lead: []string{"-ss", "2.38", "-itsoffset", "0.88"}, pad: "1.12", thr: "0.98"},
		{name: "a slot past the end selects the last frame", plan: plan, slot: 999, lead: []string{"-ss", "3.82", "-itsoffset", "2.32"}, pad: "1.12", thr: "2.42"},
		{name: "seek-unsafe plan: unseeked", plan: func() *graph.Plan { p := seekUnsafePlan(); p.Filter = planFilter; return p }, slot: 25, lead: nil, pad: "2", thr: "0.98"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.plan()
			got := MatteTrackFrameArgs(mainSrc, p, 160, 120, tc.slot)
			want := cat(tc.lead, []string{"-i", mainSrc}, frameTail(planFilter, tc.pad, tc.thr, "160:120"))
			assertArgs(t, got, want)
			noLoopBeforeFilter(t, got)
		})
	}
	t.Run("the slot's seek is the still's", func(t *testing.T) {
		// Whatever the grid maths choose, the frame still and StillArgs at
		// the middle of the same slot agree on the input half of the argv.
		for _, slot := range []int{0, 1, 7, 24, 25, 26, 61, 62, 100} {
			p := plan()
			got := MatteTrackFrameArgs(mainSrc, p, 160, 120, slot)
			still := StillArgs(mainSrc, p, (float64(slot)+0.5)/p.FPS, 0)
			gi, si := slices.Index(got, "-frames:v"), slices.Index(still, "-frames:v")
			if gi < 0 || si < 0 || !slices.Equal(got[:gi], still[:si]) {
				t.Errorf("slot %d: inputs %q, the still's %q", slot, got[:max(gi, 0)], still[:max(si, 0)])
			}
			if s := stillSeekFor(p, (float64(slot)+0.5)/p.FPS, false); s.slot != min(slot, p.Frames-1) {
				t.Errorf("slot %d: the seek selects slot %d", slot, s.slot)
			}
		}
	})
	t.Run("reversed, bounced, unusable, no rate, bad slot or size yields nil", func(t *testing.T) {
		rev := plan()
		rev.Reversed = true
		bounced := plan()
		bounced.Bounced = true
		noRate := plan()
		noRate.FPS = 0
		for name, p := range map[string]*graph.Plan{"nil": nil, "reversed": rev, "bounced": bounced, "no rate": noRate} {
			if got := MatteTrackFrameArgs(mainSrc, p, 160, 120, 0); got != nil {
				t.Errorf("%s plan: %q", name, got)
			}
		}
		for _, tc := range [][3]int{{0, 120, 0}, {160, 0, 0}, {160, 120, -1}} {
			if got := MatteTrackFrameArgs(mainSrc, plan(), tc[0], tc[1], tc[2]); got != nil {
				t.Errorf("%v: %q", tc, got)
			}
		}
	})
}

func TestMatteGateArgs(t *testing.T) {
	track, edge, out := "/data/mattes/track-k", "/data/mattes/edge-k", "/data/mattes/track-k/gated-birefnet-lite-r3.tmp"
	morph := func(filter string, n int) string {
		if n == 0 {
			return "null"
		}
		return strings.TrimSuffix(strings.Repeat(filter+"=coordinates=255,", n), ",")
	}
	want := func(r int, n string) []string {
		return []string{
			"-f", "image2", "-framerate", "1", "-i", track + "/%06d.png",
			"-f", "image2", "-framerate", "1", "-i", edge + "/%06d.png",
			"-filter_complex", "[0:v]format=gray,split=3[t0][t1][tr];" +
				"[t0]" + morph("erosion", r) + "[core];" +
				"[t1]" + morph("dilation", r) + "[outer];" +
				"[1:v][tr]scale=w=rw:h=rh:flags=bicubic,format=gray[edge];" +
				"[outer][edge]blend=all_mode=darken[band];" +
				"[core][band]blend=all_mode=lighten[gate]",
			"-map", "[gate]",
			"-fps_mode", "passthrough",
			"-frames:v", n,
			"-pix_fmt", "gray",
			"-f", "image2", out + "/%06d.png",
		}
	}
	t.Run("radius 3", func(t *testing.T) {
		got := MatteGateArgs(track, edge, out, 45, 3)
		assertArgs(t, got, want(3, "45"))
		// The chain is, verbatim, three erosions and three dilations.
		if f := got[slices.Index(got, "-filter_complex")+1]; !strings.Contains(f, "[t0]erosion=coordinates=255,erosion=coordinates=255,erosion=coordinates=255[core]") ||
			!strings.Contains(f, "[t1]dilation=coordinates=255,dilation=coordinates=255,dilation=coordinates=255[outer]") {
			t.Errorf("chain %s", f)
		}
	})
	t.Run("radius 1", func(t *testing.T) {
		assertArgs(t, MatteGateArgs(track, edge, out, 1, 1), want(1, "1"))
	})
	t.Run("radius 0 is the mask itself", func(t *testing.T) {
		assertArgs(t, MatteGateArgs(track, edge, out, 7, 0), want(0, "7"))
	})
	t.Run("no frames, a negative radius or an empty dir yields nil", func(t *testing.T) {
		for _, tc := range []struct {
			track, edge, out string
			frames, radius   int
		}{
			{track, edge, out, 0, 3},
			{track, edge, out, -5, 3},
			{track, edge, out, 45, -1},
			{"", edge, out, 45, 3},
			{track, "", out, 45, 3},
			{track, edge, "", 45, 3},
		} {
			if got := MatteGateArgs(tc.track, tc.edge, tc.out, tc.frames, tc.radius); got != nil {
				t.Errorf("MatteGateArgs(%q, %q, %q, %d, %d) = %q, want nil", tc.track, tc.edge, tc.out, tc.frames, tc.radius, got)
			}
		}
	})
}

// TestMatteInputDerivedPath: a stabilised (and a gated guided) matte input
// is nothing but a different Path to every consumer — jobs fills
// "<clipdir>/stab-<mode>/%06d.png" ("<clipdir>/gated-<edge>-<edgeKey>-r3/…") and
// MatteInput.Stabilise / Prompts / Edge, and the builders emit that Path in
// each form (the full sequence, the forward still's single PNG, the
// reversed tail's -start_number) exactly as they do the raw memo's: no enc
// change reads the new fields.
func TestMatteInputDerivedPath(t *testing.T) {
	derived := func(dir string, mi graph.MatteInput) func() *graph.Plan {
		return func() *graph.Plan {
			p := testPlan()
			p.Filter = matteFilter
			e := matteInput(62)
			e.Path = dir + "/%06d.png"
			mi.Model, mi.Size, mi.FPS, mi.Frames = "birefnet-lite", 1024, "25", 62
			e.Matte = &mi
			p.ExtraInputs = append(p.ExtraInputs, e)
			return p
		}
	}
	stabDir := matteDir + "/stab-light"
	gatedDir := "/data/mattes/4a1c0ffee5c0ffee/gated-birefnet-lite-r3/stab-strong"
	tests := []struct {
		name string
		plan func() *graph.Plan
		dir  string
	}{
		{"stabilised", derived(stabDir, graph.MatteInput{Stabilise: "light"}), stabDir},
		{"gated guided, stabilised", derived(gatedDir, graph.MatteInput{Stabilise: "strong", Prompts: "f0;b0.1000,0.2000,0.6000,0.9000", Edge: "birefnet-lite"}), gatedDir},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seq := func(start int) []string {
				return []string{"-f", "image2", "-framerate", "25", "-start_number", fmt.Sprint(start), "-i", tc.dir + "/%06d.png"}
			}
			master := MasterArgs(mainSrc, tc.plan(), "frames.rgba")
			assertArgs(t, master, cat([]string{"-ss", "1.5", "-to", "4", "-i", mainSrc}, seq(1), []string{
				"-filter_complex", matteFilter,
				"-map", "[out]",
				"-an", "-sn", "-dn",
				"-f", "rawvideo", "-pix_fmt", "rgba",
				"frames.rgba",
			}))
			still := StillArgs(mainSrc, tc.plan(), 1, 480)
			assertArgs(t, still, cat([]string{"-ss", "2.34", "-itsoffset", "0.84", "-i", mainSrc},
				[]string{"-f", "image2", "-framerate", "25", "-i", tc.dir + "/000026.png"},
				stillOut(stillFilter(matteFilter, "1.16", "0.98", alphaScale480))))
			noLoopBeforeFilter(t, still)
			rev := tc.plan()
			rev.Reversed = true
			rev.Filter = matteRevFilter
			revStill := StillArgs(mainSrc, rev, 1, 0)
			assertArgs(t, revStill, cat([]string{"-ss", "2.82", "-to", "4", "-i", mainSrc}, seq(34), stillOut(reversedStillFilter(matteRevFilter, "2", 25, ""))))
			proxy := ProxyArgs(mainSrc, tc.plan(), 0, 10, "p.webp")
			if i := slices.Index(proxy, tc.dir+"/%06d.png"); i < 0 || proxy[i-1] != "-i" || proxy[i-3] != "-start_number" || proxy[i-2] != "1" {
				t.Errorf("proxy argv %q: want the derived sequence from 1", proxy)
			}
			// The derived Path is only ever read through the input: nothing
			// in the filter names the raw memo or the derived dir.
			for _, args := range [][]string{master, still, revStill, proxy} {
				if f := args[slices.Index(args, "-filter_complex")+1]; strings.Contains(f, "mattes") {
					t.Errorf("filter names a matte path: %s", f)
				}
			}
		})
	}
}

// TestTrackSize pins the TrackSize contract: long side <= MatteTrackMaxSide,
// never upscaled, aspect kept (rounded), both dimensions even and >= 2.
func TestTrackSize(t *testing.T) {
	cases := []struct{ w, h, tw, th int }{
		{0, 0, 0, 0},
		{-1, 10, 0, 0},
		{1, 1, 2, 2},
		{720, 720, 720, 720},
		{719, 405, 718, 404},
		{1024, 576, 1024, 576},
		{1920, 1080, 1024, 576},
		{1080, 1920, 576, 1024},
		{1920, 1081, 1024, 576},
		{4096, 2160, 1024, 540},
		{3000, 2, 1024, 2},
		{2, 3000, 2, 1024},
		{1025, 1025, 1024, 1024},
	}
	for _, c := range cases {
		tw, th := TrackSize(c.w, c.h)
		if tw != c.tw || th != c.th {
			t.Errorf("TrackSize(%d, %d) = (%d, %d), want (%d, %d)", c.w, c.h, tw, th, c.tw, c.th)
		}
		if c.w > 0 && c.h > 0 {
			if tw > MatteTrackMaxSide || th > MatteTrackMaxSide || tw%2 != 0 || th%2 != 0 || tw < 2 || th < 2 {
				t.Errorf("TrackSize(%d, %d) = (%d, %d): long side / parity / floor contract broken", c.w, c.h, tw, th)
			}
			if tw > c.w+1 || th > c.h+1 {
				t.Errorf("TrackSize(%d, %d) = (%d, %d): upscaled", c.w, c.h, tw, th)
			}
		}
	}
}
