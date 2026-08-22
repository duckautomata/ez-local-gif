package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// TestPreviewAdmission: POST /api/proxy and /api/still hand the plan to
// jobs with the render path's frame-master admission for reversed plans. A
// probed 1920x1080 30 fps 20 s source with ops [{kind: reverse}] passes
// graph's 8 GiB compile cap but its reverse buffer exceeds the 2 GiB jobs
// cap (the whole clip for the still, the 10 s tail for the proxy), so both
// endpoints answer 400 naming EZLG_MAX_MASTER_BYTES without starting
// ffmpeg; the same plan without the reverse reaches ffmpeg.
func TestPreviewAdmission(t *testing.T) {
	tools, marker := fakeFFmpeg(t)
	e := newEnvWithTools(t, Config{}, nil, tools)
	b, err := e.st.PutBlob(strings.NewReader("a 1080p clip, allegedly"), "big.mp4")
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", PixFmt: "yuv420p", Bits: 8,
		Width: 1920, Height: 1080, FPS: 30, Duration: 20, Frames: 600, Kind: recipe.KindVideo}
	if err := e.st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	reverse := []recipe.Op{{Kind: recipe.OpReverse}}
	out := recipe.Output{Format: "gif"}

	for _, path := range []string{"/api/proxy", "/api/still"} {
		resp, body := e.postJSON(t, path, map[string]any{"src": b.Hash, "ops": reverse, "output": out, "t": 0.5})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s reversed: %d %s, want 400", path, resp.StatusCode, body)
			continue
		}
		msg := errorOf(t, body)
		for _, want := range []string{"EZLG_MAX_MASTER_BYTES", "reverse buffer", "trim the clip"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s reversed: error %q lacks %q", path, msg, want)
			}
		}
		if strings.Contains(msg, "ffmpeg") {
			t.Errorf("%s reversed: %q names ffmpeg", path, msg)
		}
	}
	if n := len(fakeMarkers(marker)); n != 0 {
		t.Fatalf("%d ffmpegs started for refused previews", n)
	}

	// The same plan without the reverse reaches ffmpeg (the fake blocks, so
	// the request is abandoned once the marker is there; the handler's ctx
	// then kills the child).
	body, _ := json.Marshal(map[string]any{"src": b.Hash, "output": out})
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/api/proxy", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitFor(t, 10*time.Second, "the forward proxy to reach ffmpeg", fakeStarted(marker))
	args := strings.Join(fakeArgs(t, marker), " ")
	if !strings.Contains(args, "libwebp_anim") || !strings.Contains(args, b.Path) {
		t.Errorf("forward proxy args are not a proxy of the source:\n%s", args)
	}
	if strings.Contains(args, "reverse") {
		t.Errorf("forward proxy args carry a reverse stage:\n%s", args)
	}
	cancel()
	<-done
}
