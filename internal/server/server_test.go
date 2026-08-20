package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/probe"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

type env struct {
	st    *store.Store
	jm    *jobs.Manager
	tools ffrun.Tools
	s     *Server
	srv   *httptest.Server
}

// hostTools resolves the real ffmpeg/ffprobe when the host has them.
func hostTools() ffrun.Tools {
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
	return tools
}

func newEnv(t *testing.T, cfg Config, ui fstest.MapFS) *env {
	t.Helper()
	return newEnvWithTools(t, cfg, ui, hostTools())
}

func newEnvWithTools(t *testing.T, cfg Config, ui fstest.MapFS, tools ffrun.Tools) *env {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(filepath.Join(root, "data"), filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	jm := jobs.NewManager(st, tools, jobs.Options{Concurrency: 1})
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	var uiFS fs.FS
	if ui != nil {
		uiFS = ui
	}
	s := NewServer(cfg, st, jm, tools, uiFS)
	srv := httptest.NewServer(s)
	t.Cleanup(func() {
		// Stop background work (preview pre-warming, renders) before the
		// temp dirs vanish, then drop the listener.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		srv.Close()
	})
	return &env{st: st, jm: jm, tools: tools, s: s, srv: srv}
}

func (e *env) get(t *testing.T, p string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(e.srv.URL + p)
	if err != nil {
		t.Fatalf("GET %s: %v", p, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

func (e *env) postJSON(t *testing.T, p string, v any) (*http.Response, []byte) {
	t.Helper()
	var body []byte
	switch x := v.(type) {
	case string:
		body = []byte(x)
	default:
		body, _ = json.Marshal(v)
	}
	resp, err := http.Post(e.srv.URL+p, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", p, err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

func errorOf(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not a JSON error body: %q", body)
	}
	return m["error"]
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.NRGBA{R: 10, G: 200, B: 30, A: 255})
		}
	}
	img.Set(0, 0, color.NRGBA{})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func multipartBody(t *testing.T, field, filename string, data []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("note", "ignored field before the file")
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

// tinyGIF builds a 6-frame 16x12 animated GIF with a moving square over a
// transparent background.
func tinyGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 0}, color.RGBA{220, 30, 30, 255}, color.RGBA{30, 30, 220, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < 6; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, 16, 12), pal)
		for y := 2; y < 10; y++ {
			for x := i + 1; x < i+7; x++ {
				fr.SetColorIndex(x, y, 1)
			}
		}
		fr.SetColorIndex(0, 0, 2)
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 10)
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// putProbedSource seeds a blob with probe info: real ffprobe output when the
// tools exist, canned animated-GIF facts otherwise.
func putProbedSource(t *testing.T, e *env, name string, data []byte) string {
	t.Helper()
	b, err := e.st.PutBlob(bytes.NewReader(data), name)
	if err != nil {
		t.Fatal(err)
	}
	info := recipe.ProbeInfo{Format: "gif", Codec: "gif", PixFmt: "bgra", Bits: 8, Width: 16, Height: 12, FPS: 10, Duration: 0.6, Frames: 6, Kind: recipe.KindAnimation, HasAlpha: true}
	if e.tools.FFprobe != "" && e.tools.FFmpeg != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		probed, err := probe.Probe(ctx, e.tools, b.Path, 0)
		if err != nil {
			t.Fatalf("probe fixture: %v", err)
		}
		info = probed
	}
	if err := e.st.SetBlobInfo(b.Hash, info); err != nil {
		t.Fatal(err)
	}
	return b.Hash
}

func TestHealthz(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	resp, body := e.get(t, "/healthz")
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
}

func TestCapabilities(t *testing.T) {
	e := newEnv(t, Config{MaxUploadBytes: 12345, Version: "v9"}, nil)
	resp, body := e.get(t, "/api/capabilities")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var caps struct {
		Tools          map[string]string `json:"tools"`
		Limits         map[string]int64  `json:"limits"`
		RulesVersion   string            `json:"rulesVersion"`
		Version        string            `json:"version"`
		Concurrency    int               `json:"concurrency"`
		MaxUploadBytes int64             `json:"maxUploadBytes"`
		Formats        []string          `json:"formats"`
		Features       map[string]bool   `json:"features"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if caps.Tools == nil {
		t.Error("tools must be an object")
	}
	if caps.Limits["emote"] != 262144 || caps.Limits["sticker"] != 524288 || caps.Limits["attachment"] != discordlint.Limit(discordlint.TargetAttachment) {
		t.Errorf("limits = %v", caps.Limits)
	}
	if caps.RulesVersion != discordlint.RulesVersion || caps.Version != "v9" || caps.Concurrency != 1 || caps.MaxUploadBytes != 12345 {
		t.Errorf("caps = %+v", caps)
	}
	if strings.Join(caps.Formats, ",") != "gif,webp,apng,avif,png,jpeg,frames" {
		t.Errorf("formats = %v", caps.Formats)
	}
	for _, f := range caps.Formats {
		if !recipe.IsAnimatedFormat(f) && !recipe.IsStaticFormat(f) && f != recipe.FormatFrames {
			t.Errorf("format %q is not a recipe.Format* constant", f)
		}
	}
	if len(caps.Features) != 3 || !caps.Features["fit"] || !caps.Features["sequence"] || !caps.Features["optimize"] {
		t.Errorf("features = %v, want fit/sequence/optimize all true", caps.Features)
	}
}

func TestUpload(t *testing.T) {
	e := newEnv(t, Config{MaxUploadBytes: 1 << 20}, nil)
	data := tinyPNG(t)
	ct, body := multipartBody(t, "file", "C:\\pics\\tiny.PNG", data)
	resp, err := http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sum := sha256.Sum256(data)
	wantHash := hex.EncodeToString(sum[:])

	if e.tools.FFprobe == "" {
		// No ffprobe is the server's problem, not the file's: 500, and the
		// blob is kept (unprobed) for a retry. TestUploadProbeFailures
		// covers this on hosts that do have ffprobe.
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("without ffprobe: status %d %s, want 500", resp.StatusCode, out)
		}
		if msg := errorOf(t, out); !strings.Contains(msg, "ffprobe") {
			t.Errorf("500 error text = %q", msg)
		}
		if b, err := e.st.GetBlob(wantHash); err != nil || b.Info != nil {
			t.Errorf("blob after tool failure: %+v, %v (want kept, unprobed)", b, err)
		}
		// A blob that exists without info (e.g. probe interrupted) is a 409.
		resp3, body3 := e.get(t, "/api/sources/"+wantHash)
		if resp3.StatusCode != http.StatusConflict {
			t.Errorf("unprobed source: %d %s", resp3.StatusCode, body3)
		}
		return
	}
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, out)
	}
	var src recipe.Source
	if err := json.Unmarshal(out, &src); err != nil {
		t.Fatal(err)
	}
	if src.Hash != wantHash || src.Name != "tiny.PNG" || src.Size != int64(len(data)) {
		t.Errorf("source = %+v", src)
	}
	if src.Info.Width != 4 || src.Info.Height != 3 || !src.Info.IsStill || src.Info.Codec != "png" {
		t.Errorf("info = %+v", src.Info)
	}
	// Second upload of the same bytes: dedupe, no re-probe, same answer.
	resp, err = http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(out, out2) {
		t.Errorf("dedupe upload: %d %s", resp.StatusCode, out2)
	}
	// GET /api/sources/{hash}
	resp3, body3 := e.get(t, "/api/sources/"+wantHash)
	if resp3.StatusCode != 200 || !bytes.Equal(body3, out) {
		t.Errorf("get source: %d %s", resp3.StatusCode, body3)
	}
}

func TestUploadErrors(t *testing.T) {
	e := newEnv(t, Config{MaxUploadBytes: 2048}, nil)

	// Not multipart.
	resp, body := e.postJSON(t, "/api/upload", `{"x":1}`)
	if resp.StatusCode != 400 {
		t.Errorf("non-multipart: %d %s", resp.StatusCode, body)
	}
	// Wrong field name.
	ct, b := multipartBody(t, "upload", "a.png", []byte("data"))
	resp2, err := http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 400 || !strings.Contains(errorOf(t, out), "file") {
		t.Errorf("missing field: %d %s", resp2.StatusCode, out)
	}
	// Empty file.
	ct, b = multipartBody(t, "file", "empty.png", nil)
	resp3, _ := http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(b))
	out, _ = io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 400 {
		t.Errorf("empty file: %d %s", resp3.StatusCode, out)
	}
	// Too large (limit 2048): Content-Length known → 413 before reading.
	ct, b = multipartBody(t, "file", "big.bin", bytes.Repeat([]byte("x"), 4096))
	resp4, err := http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, _ = io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d %s", resp4.StatusCode, out)
	}
	// Too large with unknown length (chunked): the reader limit trips → 413.
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/upload", io.NopCloser(bytes.NewReader(b)))
	req.Header.Set("Content-Type", ct)
	req.ContentLength = -1
	resp5, err := http.DefaultClient.Do(req)
	if err != nil {
		// The server may close the connection early; that is acceptable.
		t.Logf("chunked oversize upload: %v", err)
	} else {
		out, _ = io.ReadAll(resp5.Body)
		resp5.Body.Close()
		if resp5.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("chunked too large: %d %s", resp5.StatusCode, out)
		}
	}
	// No such blob after failures.
	entries, _ := os.ReadDir(filepath.Join(e.st.Root, "blobs"))
	if len(entries) != 0 {
		t.Errorf("blobs left behind by rejected uploads: %d", len(entries))
	}
}

// uploadFile POSTs data as the "file" field and returns the response.
func (e *env) uploadFile(t *testing.T, name string, data []byte) (*http.Response, []byte) {
	t.Helper()
	ct, body := multipartBody(t, "file", name, data)
	resp, err := http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

// writeSpy records whether a handler wrote anything to the response.
type writeSpy struct {
	http.ResponseWriter
	wrote bool
}

func (s *writeSpy) WriteHeader(code int) { s.wrote = true; s.ResponseWriter.WriteHeader(code) }
func (s *writeSpy) Write(p []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(p)
}

// TestUploadProbeFailures: only a file ffprobe ran on and could not read is
// the client's fault (422, blob dropped); a tool that could not run, or ran
// out of time, is ours (500 / 504, blob kept unprobed for a retry); a client
// that hangs up mid-probe gets no answer and the blob is kept.
func TestUploadProbeFailures(t *testing.T) {
	data := tinyPNG(t)
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	kept := func(t *testing.T, e *env) {
		t.Helper()
		b, err := e.st.GetBlob(hash)
		if err != nil {
			t.Fatalf("blob should have been kept: %v", err)
		}
		if b.Info != nil {
			t.Errorf("kept blob has probe info %+v, want none", b.Info)
		}
		resp, body := e.get(t, "/api/sources/"+hash)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("GET unprobed source = %d %s, want 409", resp.StatusCode, body)
		}
	}
	dropped := func(t *testing.T, e *env) {
		t.Helper()
		if _, err := e.st.GetBlob(hash); err == nil {
			t.Error("unreadable blob was kept")
		}
		if resp, _ := e.get(t, "/api/sources/"+hash); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET dropped source = %d, want 404", resp.StatusCode)
		}
	}
	unreadable := func(t *testing.T, e *env) {
		t.Helper()
		resp, out := e.uploadFile(t, "x.png", data)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d %s, want 422", resp.StatusCode, out)
		}
		if msg := errorOf(t, out); !strings.Contains(msg, "cannot read this file") {
			t.Errorf("422 error text = %q", msg)
		}
		dropped(t, e)
	}
	toolFailure := func(t *testing.T, e *env) {
		t.Helper()
		resp, out := e.uploadFile(t, "x.png", data)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status %d %s, want 500", resp.StatusCode, out)
		}
		if msg := errorOf(t, out); !strings.Contains(msg, "ffprobe") || strings.Contains(msg, "cannot read this file") {
			t.Errorf("500 error text = %q, want the tool error, not a file complaint", msg)
		}
		kept(t, e)
	}

	t.Run("no ffprobe configured", func(t *testing.T) {
		toolFailure(t, newEnvWithTools(t, Config{}, nil, ffrun.Tools{}))
	})
	t.Run("ffprobe not executable", func(t *testing.T) {
		tools := ffrun.Tools{FFprobe: filepath.Join(t.TempDir(), "no-such-ffprobe")}
		toolFailure(t, newEnvWithTools(t, Config{}, nil, tools))
	})
	t.Run("ffprobe rejects the file", func(t *testing.T) {
		tools, _ := fakeFFprobe(t, fakeModeFail)
		unreadable(t, newEnvWithTools(t, Config{}, nil, tools))
	})
	t.Run("ffprobe output is not JSON", func(t *testing.T) {
		tools, _ := fakeFFprobe(t, fakeModeJunk)
		unreadable(t, newEnvWithTools(t, Config{}, nil, tools))
	})
	t.Run("no video stream", func(t *testing.T) {
		tools, _ := fakeFFprobe(t, fakeModeNoV)
		unreadable(t, newEnvWithTools(t, Config{}, nil, tools))
	})
	t.Run("real ffprobe on garbage", func(t *testing.T) {
		tools := hostTools()
		if tools.FFprobe == "" {
			t.Skip("ffprobe not on PATH")
		}
		e := newEnvWithTools(t, Config{}, nil, tools)
		junk := []byte("definitely not an image or a video, just text\n")
		resp, out := e.uploadFile(t, "notes.txt", junk)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d %s, want 422", resp.StatusCode, out)
		}
		jsum := sha256.Sum256(junk)
		if _, err := e.st.GetBlob(hex.EncodeToString(jsum[:])); err == nil {
			t.Error("unreadable blob was kept")
		}
	})
	t.Run("real ffprobe on garbage behind an image extension", func(t *testing.T) {
		// The image2 demuxer trusts the .png name, so ffprobe exits 0 with a
		// video stream of no dimensions: still the file's fault.
		tools := hostTools()
		if tools.FFprobe == "" {
			t.Skip("ffprobe not on PATH")
		}
		e := newEnvWithTools(t, Config{}, nil, tools)
		junk := []byte("definitely not a png, just text\n")
		resp, out := e.uploadFile(t, "notes.png", junk)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d %s, want 422", resp.StatusCode, out)
		}
		jsum := sha256.Sum256(junk)
		if _, err := e.st.GetBlob(hex.EncodeToString(jsum[:])); err == nil {
			t.Error("unreadable blob was kept")
		}
	})
	t.Run("probe times out", func(t *testing.T) {
		tools, marker := fakeFFprobe(t, "") // blocks like a slow probe
		e := newEnvWithTools(t, Config{}, nil, tools)
		old := probeTimeout
		probeTimeout = 300 * time.Millisecond
		t.Cleanup(func() { probeTimeout = old })
		resp, out := e.uploadFile(t, "x.png", data)
		if resp.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("status %d %s, want 504", resp.StatusCode, out)
		}
		if msg := errorOf(t, out); !strings.Contains(msg, "timed out") {
			t.Errorf("504 error text = %q", msg)
		}
		if !fakeStarted(marker)() {
			t.Error("fake ffprobe never started")
		}
		kept(t, e)
	})
	t.Run("client hangs up", func(t *testing.T) {
		tools, marker := fakeFFprobe(t, "") // blocks like a slow probe
		e := newEnvWithTools(t, Config{}, nil, tools)
		// Front the server with a wrapper that swaps in a request context
		// the test controls (what the net/http server does when the client
		// disconnects) and records whether the handler wrote anything.
		reqCtx, hangUp := context.WithCancel(context.Background())
		defer hangUp()
		var spy *writeSpy
		handlerDone := make(chan struct{})
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			spy = &writeSpy{ResponseWriter: w}
			e.s.ServeHTTP(spy, r.WithContext(reqCtx))
			close(handlerDone)
		}))
		defer front.Close()

		ct, body := multipartBody(t, "file", "x.png", data)
		go func() {
			resp, err := http.Post(front.URL+"/api/upload", ct, bytes.NewReader(body))
			if err == nil {
				resp.Body.Close()
			}
		}()
		waitFor(t, 10*time.Second, "fake ffprobe to start", fakeStarted(marker))
		hangUp()
		select {
		case <-handlerDone:
		case <-time.After(10 * time.Second):
			t.Fatal("upload handler did not return after the client hung up")
		}
		if spy.wrote {
			t.Error("handler answered a client that had hung up")
		}
		kept(t, e)
	})
}

func TestNotFoundAndBadIDs(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	for _, p := range []string{
		"/api/sources/nope",
		"/api/sources/" + strings.Repeat("0", 64),
		"/api/jobs/unknown",
		"/api/jobs/unknown/events",
		"/api/results/" + strings.Repeat("0", 64),
		"/api/results/etc%2Fpasswd",
		"/api/does-not-exist",
		"/out/" + strings.Repeat("0", 64) + "/out.gif",
		"/out/nothash/out.gif",
	} {
		resp, body := e.get(t, p)
		if resp.StatusCode != 404 {
			t.Errorf("GET %s = %d, want 404 (%s)", p, resp.StatusCode, body)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s content-type %q, want JSON error", p, ct)
		}
		if errorOf(t, body) == "" {
			t.Errorf("GET %s: empty error message", p)
		}
	}
	// DELETE unknown job → 404.
	req, _ := http.NewRequest("DELETE", e.srv.URL+"/api/jobs/unknown", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("DELETE unknown job = %d", resp.StatusCode)
	}
}

func TestCreateJobValidation(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	resp, body := e.postJSON(t, "/api/jobs", `{not json`)
	if resp.StatusCode != 400 {
		t.Errorf("bad json: %d %s", resp.StatusCode, body)
	}
	resp, body = e.postJSON(t, "/api/jobs", recipe.Recipe{Output: recipe.Output{Format: "gif"}})
	if resp.StatusCode != 400 {
		t.Errorf("no sources: %d %s", resp.StatusCode, body)
	}
	resp, body = e.postJSON(t, "/api/jobs", recipe.Recipe{Sources: []string{strings.Repeat("1", 64)}, Output: recipe.Output{Format: "gif"}})
	if resp.StatusCode != 400 || !strings.Contains(errorOf(t, body), "not uploaded") {
		t.Errorf("unknown source: %d %s", resp.StatusCode, body)
	}
	// Blob without probe info → 409.
	b, _ := e.st.PutBlob(strings.NewReader("raw"), "raw.mov")
	resp, body = e.postJSON(t, "/api/jobs", recipe.Recipe{Sources: []string{b.Hash}, Output: recipe.Output{Format: "gif"}})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("unprobed source: %d %s", resp.StatusCode, body)
	}
	// Unsupported format (mp4 arrives in Phase 4) → 400 from the manager.
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	resp, body = e.postJSON(t, "/api/jobs", recipe.Recipe{Sources: []string{h}, Output: recipe.Output{Format: "mp4"}})
	if resp.StatusCode != 400 {
		t.Errorf("unsupported format: %d %s", resp.StatusCode, body)
	}
}

// readSSE reads events (type + decoded job) until the stream ends.
func readSSE(t *testing.T, r io.Reader) []jobs.Event {
	t.Helper()
	var evs []jobs.Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var typ string
	var data strings.Builder
	flush := func() {
		if typ == "" && data.Len() == 0 {
			return
		}
		var ev jobs.Event
		if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
			t.Fatalf("bad event data %q: %v", data.String(), err)
		}
		if ev.Type != typ {
			t.Errorf("event line %q != data.type %q", typ, ev.Type)
		}
		evs = append(evs, ev)
		typ = ""
		data.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment / ping
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		default:
			t.Errorf("unexpected SSE line %q", line)
		}
	}
	flush()
	return evs
}

func TestJobLifecycleAndSSE(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	// With a real ffmpeg this recipe could succeed; with none it fails in
	// the master stage. Either way the SSE stream must end with done|error.
	rec := recipe.Recipe{Sources: []string{h}, Output: recipe.Output{Format: "gif", Width: 8, Height: 8, Target: "emote"}}
	resp, body := e.postJSON(t, "/api/jobs", rec)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create job: %d %s", resp.StatusCode, body)
	}
	var job jobs.Job
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.RecipeHash != jobs.ResultKey(rec) {
		t.Errorf("job = %+v", job)
	}

	sresp, err := http.Get(e.srv.URL + "/api/jobs/" + job.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != 200 {
		t.Fatalf("events status %d", sresp.StatusCode)
	}
	if ct := sresp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if sresp.Header.Get("Cache-Control") != "no-cache" {
		t.Error("missing Cache-Control: no-cache")
	}
	done := make(chan []jobs.Event, 1)
	go func() { done <- readSSE(t, sresp.Body) }()
	var evs []jobs.Event
	select {
	case evs = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("SSE stream did not close")
	}
	if len(evs) == 0 {
		t.Fatal("no SSE events")
	}
	last := evs[len(evs)-1]
	if last.Type != jobs.EventDone && last.Type != jobs.EventError {
		t.Errorf("stream ended with %+v", last)
	}
	if last.Job.ID != job.ID {
		t.Errorf("event job id %q", last.Job.ID)
	}
	if e.tools.FFmpeg == "" && (last.Type != jobs.EventError || last.Job.Error == "") {
		t.Errorf("without ffmpeg the job must fail with a message: %+v", last.Job)
	}

	// GET /api/jobs/{id} agrees.
	gresp, gbody := e.get(t, "/api/jobs/"+job.ID)
	var got jobs.Job
	json.Unmarshal(gbody, &got)
	if gresp.StatusCode != 200 || got.State != last.Job.State {
		t.Errorf("get job: %d %+v", gresp.StatusCode, got)
	}
	// DELETE on a finished job is a 204 no-op.
	req, _ := http.NewRequest("DELETE", e.srv.URL+"/api/jobs/"+job.ID, nil)
	dresp, _ := http.DefaultClient.Do(req)
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d", dresp.StatusCode)
	}

	if last.Type == jobs.EventDone {
		// Result + file endpoints work end to end.
		rresp, rbody := e.get(t, "/api/results/"+jobs.ResultKey(rec))
		if rresp.StatusCode != 200 {
			t.Fatalf("result: %d %s", rresp.StatusCode, rbody)
		}
		var res jobs.Result
		json.Unmarshal(rbody, &res)
		if len(res.Files) != 1 || res.Files[0].Name != "out.gif" {
			t.Fatalf("result files: %+v", res.Files)
		}
		fresp, fbody := e.get(t, res.Files[0].URL+"?dl=1")
		if fresp.StatusCode != 200 || !bytes.HasPrefix(fbody, []byte("GIF8")) {
			t.Errorf("out file: %d %q", fresp.StatusCode, fbody[:min(len(fbody), 6)])
		}
		if cd := fresp.Header.Get("Content-Disposition"); !strings.Contains(cd, `attachment`) || !strings.Contains(cd, "a.gif") {
			t.Errorf("content-disposition = %q", cd)
		}
	}
}

func TestOutFileServingAndTraversal(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	hash := strings.Repeat("e", 64)
	// Fabricate a committed result.
	stage := filepath.Join(t.TempDir(), "s")
	os.MkdirAll(stage, 0o755)
	os.WriteFile(filepath.Join(stage, "out.gif"), []byte("GIF89a-bytes"), 0o644)
	os.WriteFile(filepath.Join(stage, "report.json"), []byte(`{"ok":true}`), 0o644)
	man := jobs.Result{RecipeHash: hash, Recipe: recipe.Recipe{Sources: []string{strings.Repeat("f", 64)}, Output: recipe.Output{Format: "gif"}},
		Files: []jobs.File{{Name: "out.gif", URL: "/out/" + hash + "/out.gif", Format: "gif"}}}
	mb, _ := json.Marshal(man)
	os.WriteFile(filepath.Join(stage, store.ManifestName), mb, 0o644)
	if err := e.st.CommitResult(hash, stage); err != nil {
		t.Fatal(err)
	}
	// A secret file outside the result dir that traversal might reach.
	os.WriteFile(filepath.Join(e.st.Root, "results", "secret.txt"), []byte("secret"), 0o644)

	resp, body := e.get(t, "/out/"+hash+"/out.gif")
	if resp.StatusCode != 200 || string(body) != "GIF89a-bytes" {
		t.Errorf("out.gif: %d %q", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("cache-control = %q", cc)
	}
	if resp.Header.Get("Content-Disposition") != "" {
		t.Error("attachment without dl=1")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/gif" {
		t.Errorf("content-type = %q", ct)
	}
	resp, _ = e.get(t, "/out/"+hash+"/out.gif?dl=1")
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename=out.gif` {
		t.Errorf("dl=1 content-disposition = %q (source blob unknown → file name)", cd)
	}
	resp, body = e.get(t, "/api/results/"+hash)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"recipeHash":"`+hash+`"`) {
		t.Errorf("results: %d %s", resp.StatusCode, body)
	}

	// Traversal / bad names. Redirects are followed: the mux may clean
	// "/../" into a different path, which must then also be rejected.
	for _, p := range []string{
		"/out/" + hash + "/..%2Fsecret.txt",
		"/out/" + hash + "/%2e%2e%2Fsecret.txt",
		"/out/" + hash + "/..%5Csecret.txt",
		"/out/" + hash + "/../secret.txt",
		"/out/" + hash + "/.hidden",
		"/out/" + hash + "/missing.gif",
		"/out/" + hash + "/out.gif%00",
		"/out/" + hash + "/",
		"/out/secret.txt",
		"/out/" + hash,
	} {
		resp, err := http.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 || bytes.Equal(b, []byte("secret")) {
			t.Errorf("GET %s = %d %q, want rejection", p, resp.StatusCode, b)
		}
	}
}

func TestStillEndpointErrors(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	resp, body := e.postJSON(t, "/api/still", `{"src":"nope"}`)
	if resp.StatusCode != 400 {
		t.Errorf("bad src: %d %s", resp.StatusCode, body)
	}
	resp, body = e.postJSON(t, "/api/still", map[string]any{"src": strings.Repeat("a", 64), "t": 0})
	if resp.StatusCode != 404 {
		t.Errorf("unknown src: %d %s", resp.StatusCode, body)
	}
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	resp, body = e.postJSON(t, "/api/still", map[string]any{"src": h, "ops": []map[string]any{{"kind": "bogus"}}, "t": 0})
	if resp.StatusCode != 400 {
		t.Errorf("bad op: %d %s", resp.StatusCode, body)
	}
	if e.tools.FFmpeg == "" {
		resp, body = e.postJSON(t, "/api/still", map[string]any{"src": h, "output": map[string]any{"format": "gif"}, "t": 0})
		if resp.StatusCode != 500 || !strings.Contains(errorOf(t, body), "ffmpeg") {
			t.Errorf("no ffmpeg: %d %s", resp.StatusCode, body)
		}
		return
	}
	resp, body = e.postJSON(t, "/api/still", map[string]any{"src": h, "output": map[string]any{"format": "gif"}, "t": 0, "maxW": 2})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(body, []byte("\x89PNG")) {
		t.Errorf("still: %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body[:min(len(body), 80)])
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=3600" {
		t.Errorf("cache-control = %q", cc)
	}
}

func TestSPA(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>ezlg</title>")},
		"assets/app-1.js":  {Data: []byte("console.log('hi')")},
		"assets/app-1.css": {Data: []byte("body{}")},
		"favicon.svg":      {Data: []byte("<svg/>")},
	}
	e := newEnv(t, Config{}, ui)

	resp, body := e.get(t, "/")
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("<title>ezlg")) {
		t.Errorf("/ = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index content-type = %q", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("index cache-control = %q", resp.Header.Get("Cache-Control"))
	}
	// Extension-less unknown paths are client routes → index.html (a dot in
	// a directory segment does not make it a file).
	for _, p := range []string{"/some/client/route?x=1", "/v1.2/settings", "/edit"} {
		resp, body = e.get(t, p)
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte("<title>ezlg")) {
			t.Errorf("fallback %s = %d %q", p, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("fallback %s content-type = %q", p, ct)
		}
	}
	// Missing files — anything with an extension, or under assets/ — are
	// genuine 404s, never an HTML page in disguise.
	for _, p := range []string{"/favicon.ico", "/robots.txt", "/some/route.html", "/assets/app-0.js", "/assets/", "/assets", "/assets/deep/x"} {
		resp, body = e.get(t, p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d %q, want 404", p, resp.StatusCode, body)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || errorOf(t, body) == "" {
			t.Errorf("GET %s: content-type %q body %q, want a JSON error", p, ct, body)
		}
	}
	// HEAD follows the same rules.
	hresp, err := http.Head(e.srv.URL + "/missing.png")
	if err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD /missing.png = %d, want 404", hresp.StatusCode)
	}
	resp, body = e.get(t, "/assets/app-1.js")
	if resp.StatusCode != 200 || string(body) != "console.log('hi')" {
		t.Errorf("js = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("js content-type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset cache-control = %q", cc)
	}
	resp, _ = e.get(t, "/assets/app-1.css")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("css content-type = %q", ct)
	}
	resp, _ = e.get(t, "/favicon.svg")
	if ct := resp.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("svg content-type = %q", ct)
	}
	// /api/* never falls back to the SPA.
	resp, body = e.get(t, "/api/nope")
	if resp.StatusCode != 404 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("/api/nope = %d %s", resp.StatusCode, body)
	}
	// Non-GET on the SPA is 405.
	presp, _ := e.postJSON(t, "/whatever", "{}")
	if presp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST spa = %d", presp.StatusCode)
	}
}

func TestSPANotBuilt(t *testing.T) {
	e := newEnv(t, Config{}, fstest.MapFS{".gitkeep": {Data: nil}})
	for _, p := range []string{"/", "/index.html", "/app/route"} {
		resp, body := e.get(t, p)
		if resp.StatusCode != 200 || string(body) != notBuiltNotice {
			t.Errorf("%s = %d %q", p, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s content-type = %q", p, ct)
		}
	}
	// A missing file is still a 404, built or not.
	for _, p := range []string{"/favicon.ico", "/assets/app.js"} {
		resp, body := e.get(t, p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d %q, want 404", p, resp.StatusCode, body)
		}
	}
	// nil FS behaves the same.
	e2 := newEnv(t, Config{}, nil)
	resp, body := e2.get(t, "/")
	if resp.StatusCode != 200 || string(body) != notBuiltNotice {
		t.Errorf("nil ui = %d %q", resp.StatusCode, body)
	}
	if resp, _ := e2.get(t, "/favicon.ico"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("nil ui /favicon.ico = %d, want 404", resp.StatusCode)
	}
}

func TestIsClientRoute(t *testing.T) {
	yes := []string{"edit", "some/client/route", "v1.2/settings", "a/b/c"}
	no := []string{"favicon.ico", "some/route.html", "assets", "assets/", "assets/app-1.js", "assets/deep/x", "x/y.map"}
	for _, n := range yes {
		if !isClientRoute(n) {
			t.Errorf("%q should fall back to index.html", n)
		}
	}
	for _, n := range no {
		if isClientRoute(n) {
			t.Errorf("%q should be a 404", n)
		}
	}
}

// send issues method+path with the given headers and returns status + body.
func (e *env) send(t *testing.T, method, p, contentType, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+p, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, p, err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, out
}

// TestSameOriginGuard: POST/DELETE from another site are refused with 403;
// the SPA's own requests (same-origin, with or without a proxy rewriting
// Host), header-less clients like curl, and every GET pass.
func TestSameOriginGuard(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	self := e.srv.URL // http://127.0.0.1:port — what a browser sends as Origin
	const evil = "http://evil.example"

	// Requests that reach the handler get its own answer (400: `{}` is not
	// a recipe / not a from-result request; 404: unknown job; 400: not
	// multipart) — never 403.
	pass := map[string]int{"POST /api/jobs": 400, "DELETE /api/jobs/unknown": 404, "POST /api/upload": 400, "POST /api/still": 400, "POST /api/sources/from-result": 400}
	cases := []struct {
		name string
		hdr  map[string]string
		ok   bool
	}{
		{"no headers (curl)", nil, true},
		{"sec-fetch-site same-origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": self}, true},
		{"same-origin behind a proxy that rewrote Host", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://gif.home.example"}, true},
		{"sec-fetch-site none", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"origin matches host (old browser)", map[string]string{"Origin": self}, true},
		{"same-site with matching origin", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": self}, true},
		{"sec-fetch-site cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": evil}, false},
		{"cross-site even with a forged matching origin", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": self}, false},
		{"same-site from another port", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://127.0.0.1:9"}, false},
		{"foreign origin", map[string]string{"Origin": evil}, false},
		{"opaque origin", map[string]string{"Origin": "null"}, false},
		{"origin differs by port", map[string]string{"Origin": "http://127.0.0.1:9"}, false},
	}
	for _, tc := range cases {
		for mp, want := range pass {
			method, p, _ := strings.Cut(mp, " ")
			ct := ""
			if method == "POST" && p != "/api/upload" {
				ct = "application/json"
			}
			status, body := e.send(t, method, p, ct, "{}", tc.hdr)
			if tc.ok && status != want {
				t.Errorf("%s: %s = %d %s, want %d (guard must let it through)", tc.name, mp, status, body, want)
			}
			if !tc.ok {
				if status != http.StatusForbidden {
					t.Errorf("%s: %s = %d %s, want 403", tc.name, mp, status, body)
				} else if msg := errorOf(t, body); !strings.Contains(msg, "cross-site") {
					t.Errorf("%s: 403 body %q", tc.name, body)
				}
			}
		}
	}
	// Safe methods are never guarded.
	for _, p := range []string{"/healthz", "/api/capabilities", "/"} {
		status, _ := e.send(t, "GET", p, "", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": evil})
		if status != 200 {
			t.Errorf("cross-site GET %s = %d, want 200", p, status)
		}
	}
	// The SPA's multipart upload passes the guard end to end.
	ct, body := multipartBody(t, "file", "tiny.png", tinyPNG(t))
	status, out := e.send(t, "POST", "/api/upload", ct, string(body), map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": self})
	if status == http.StatusForbidden || status == http.StatusUnsupportedMediaType {
		t.Errorf("same-origin multipart upload = %d %s", status, out)
	}
	if e.tools.FFprobe != "" && status != 200 {
		t.Errorf("same-origin multipart upload = %d %s, want 200", status, out)
	}
}

func TestOriginMatchesHost(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://example.com", "example.com", true},
		{"http://example.com:8080", "example.com:8080", true},
		{"HTTP://Example.COM:8080", "example.com:8080", true},
		{"http://example.com", "example.com:80", true},
		{"http://example.com:80", "example.com", true},
		{"https://example.com:443", "example.com", true},
		{"https://example.com", "example.com:443", true},
		{"http://[::1]:8080", "[::1]:8080", true},
		{"http://[::1]", "[::1]:80", true},
		{"http://192.168.1.5:8080", "192.168.1.5:8080", true},
		{"http://example.com:8080", "example.com", false},
		{"http://example.com", "example.com:8080", false},
		{"http://example.com:8081", "example.com:8080", false},
		{"https://example.com", "example.com:80", false},
		{"http://evil.example", "example.com", false},
		{"http://example.com.evil.example", "example.com", false},
		{"http://example.com@evil.example", "example.com", false},
		{"http://evil.example/example.com", "example.com", false},
		{"null", "example.com", false},
		{"", "example.com", false},
		{"file://", "example.com", false},
		{"ftp://example.com", "example.com", false},
		{"http://example.com", "", false},
	}
	for _, tc := range cases {
		if got := originMatchesHost(tc.origin, tc.host); got != tc.want {
			t.Errorf("originMatchesHost(%q, %q) = %v, want %v", tc.origin, tc.host, got, tc.want)
		}
	}
}

// TestJSONContentType: the JSON endpoints insist on application/json (any
// charset) and answer 415 otherwise, before looking at the body.
func TestJSONContentType(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	rec, _ := json.Marshal(recipe.Recipe{Sources: []string{h}, Output: recipe.Output{Format: "gif", Width: 8, Height: 8}})
	still := `{"src":"nope"}`

	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "application/json;charset=UTF-8", "APPLICATION/JSON"} {
		if status, body := e.send(t, "POST", "/api/still", ct, still, nil); status != 400 {
			t.Errorf("still with %q = %d %s, want 400 (bad src, past the content-type check)", ct, status, body)
		}
		if status, body := e.send(t, "POST", "/api/jobs", ct, `{}`, nil); status != 400 {
			t.Errorf("jobs with %q = %d %s, want 400 (empty recipe, past the content-type check)", ct, status, body)
		}
		if status, body := e.send(t, "POST", "/api/sources/from-result", ct, `{}`, nil); status != 400 {
			t.Errorf("from-result with %q = %d %s, want 400 (no hash, past the content-type check)", ct, status, body)
		}
	}
	fromResult := `{"recipeHash":"` + strings.Repeat("a", 64) + `","name":"out.gif"}`
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "text/json", "application/json-patch+json"} {
		for _, ep := range []struct{ p, body string }{{"/api/still", still}, {"/api/jobs", string(rec)}, {"/api/sources/from-result", fromResult}} {
			status, body := e.send(t, "POST", ep.p, ct, ep.body, nil)
			if status != http.StatusUnsupportedMediaType {
				t.Errorf("%s with content-type %q = %d %s, want 415", ep.p, ct, status, body)
				continue
			}
			if msg := errorOf(t, body); !strings.Contains(msg, "application/json") {
				t.Errorf("%s 415 body %q", ep.p, body)
			}
		}
	}
	// A well-formed recipe with the wrong Content-Type never became a job:
	// nothing was rendered for it.
	if resp, _ := e.get(t, "/api/results/"+jobs.ResultKey(recipe.Recipe{Sources: []string{h}, Output: recipe.Output{Format: "gif", Width: 8, Height: 8}})); resp.StatusCode != http.StatusNotFound {
		t.Errorf("result exists for a recipe that was only ever posted with a bad Content-Type (%d)", resp.StatusCode)
	}
}

func TestIsJSONContentType(t *testing.T) {
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON;charset=UTF-8", " application/json "} {
		if !isJSONContentType(ct) {
			t.Errorf("%q rejected", ct)
		}
	}
	for _, ct := range []string{"", "text/plain", "application/jsonx", "application/x-www-form-urlencoded", "json", "application/json/extra"} {
		if isJSONContentType(ct) {
			t.Errorf("%q accepted", ct)
		}
	}
}

// TestWarmStillRequest: the pre-warm keys the same memo entry as the SPA's
// first /api/still — no geometry, t=0, 480 px — plus the unpremultiply op
// the SPA turns on for a premultiplied source.
func TestWarmStillRequest(t *testing.T) {
	ops, out, ts, maxW := warmStillRequest(recipe.ProbeInfo{})
	if len(ops) != 0 || out != (recipe.Output{Format: "gif"}) || ts != 0 || maxW != 480 {
		t.Errorf("plain source: ops=%v out=%+v t=%v maxW=%d", ops, out, ts, maxW)
	}
	ops, out, ts, maxW = warmStillRequest(recipe.ProbeInfo{Premultiplied: true})
	if len(ops) != 1 || ops[0].Kind != recipe.OpUnpremultiply || len(ops[0].Params) != 0 || out != (recipe.Output{Format: "gif"}) || ts != 0 || maxW != 480 {
		t.Errorf("premultiplied source: ops=%v out=%+v t=%v maxW=%d", ops, out, ts, maxW)
	}
}

// TestWarmStillArgs: the pre-warm's ffmpeg carries the unpremultiply stage
// exactly when the source is premultiplied (checked against a fake ffmpeg
// that records its argv).
func TestWarmStillArgs(t *testing.T) {
	for _, premul := range []bool{false, true} {
		t.Run(fmt.Sprintf("premultiplied=%v", premul), func(t *testing.T) {
			tools, marker := fakeFFmpeg(t)
			e := newEnvWithTools(t, Config{}, nil, tools)
			h := putProbedSource(t, e, "a.gif", tinyGIF(t))
			blob, err := e.st.GetBlob(h)
			if err != nil {
				t.Fatal(err)
			}
			info := *blob.Info
			info.Premultiplied = premul
			if err := e.st.SetBlobInfo(h, info); err != nil {
				t.Fatal(err)
			}
			e.s.warmStill(h, info)
			waitFor(t, 10*time.Second, "pre-warm ffmpeg to start", fakeStarted(marker))
			args := strings.Join(fakeArgs(t, marker), " ")
			// The hoisted op is "declare premultiplied, then unpremultiply"
			// right after decode (the preview scale's own premultiply /
			// unpremultiply pair is always there).
			const hoisted = "setparams=alpha_mode=premultiplied,unpremultiply=inplace=1"
			if got := strings.Contains(args, hoisted); got != premul {
				t.Errorf("pre-warm args contain the unpremultiply op = %v, want %v:\n%s", got, premul, args)
			}
			if !strings.Contains(args, "-frames:v 1") || !strings.Contains(args, blob.Path) {
				t.Errorf("pre-warm args are not a still of the source:\n%s", args)
			}
		})
	}
}

// TestWarmStillHitsUIMemo: with a real ffmpeg, the SPA's first still request
// for a premultiplied source is served from the pre-warm's memo entry.
func TestWarmStillHitsUIMemo(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.tools.FFmpeg == "" || e.tools.FFprobe == "" {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	blob, err := e.st.GetBlob(h)
	if err != nil {
		t.Fatal(err)
	}
	info := *blob.Info
	info.Premultiplied = true // as a ProRes 4444 export from Resolve would be
	if err := e.st.SetBlobInfo(h, info); err != nil {
		t.Fatal(err)
	}
	stills := filepath.Join(e.st.Scratch, "stills")
	memoCount := func() int {
		entries, _ := os.ReadDir(stills)
		n := 0
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), ".png") {
				n++
			}
		}
		return n
	}

	e.s.warmStill(h, info)
	waitFor(t, 30*time.Second, "pre-warm to land in the memo", func() bool { return memoCount() == 1 })
	entries, _ := os.ReadDir(stills)
	var memo []byte
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".png") {
			memo, _ = os.ReadFile(filepath.Join(stills, en.Name()))
		}
	}

	// What Preview.svelte sends first: defaultOps → [unpremultiply], the
	// chat-gif preset's output (quality knobs only, no geometry), t=0, 480.
	uiOps := []recipe.Op{{Kind: recipe.OpUnpremultiply}}
	uiOut := recipe.Output{Format: "gif", Colors: 256, Dither: "sierra2_4a", Lossy: 20, AlphaThreshold: 128, Matte: "313338", Preset: "chat-gif", Target: "attachment"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	png, err := e.jm.Still(ctx, h, uiOps, uiOut, 0, 480)
	if err != nil {
		t.Fatalf("UI still: %v", err)
	}
	if n := memoCount(); n != 1 {
		t.Errorf("UI's first still missed the pre-warm memo (%d entries)", n)
	}
	if !bytes.Equal(png, memo) {
		t.Error("UI's first still differs from the pre-warmed frame")
	}
	// Control: a request the pre-warm did not cover renders a new entry.
	if _, err := e.jm.Still(ctx, h, nil, uiOut, 0, 480); err != nil {
		t.Fatalf("control still: %v", err)
	}
	if n := memoCount(); n != 2 {
		t.Errorf("control still should have added a memo entry (%d entries)", n)
	}
}

func TestValidResultName(t *testing.T) {
	good := []string{"out.gif", "out.webp", "report.json", "manifest.json", "frame_0001.png", "a-b.c"}
	bad := []string{"", ".", "..", "../x", "a/b", `a\b`, ".hidden", "out.gif\x00", "sp ace.gif", "ünïcode.gif", strings.Repeat("x", 200)}
	for _, n := range good {
		if !validResultName(n) {
			t.Errorf("%q rejected", n)
		}
	}
	for _, n := range bad {
		if validResultName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}
