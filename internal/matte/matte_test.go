package matte

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// pingJSON is the spec §7.2 example plus defaultModel, as the sidecar sends it.
const pingJSON = `{"protocol":1,"version":"2026.10.1","instance":"a1b2c3","processingVersion":"1",
 "device":"cuda","reason":"","defaultModel":"isnet-anime",
 "gpu":{"name":"NVIDIA GeForce RTX 5080","totalGiB":16,"freeGiB":12.4},
 "models":{"isnet-anime":{"state":"ready","reason":"","percent":100,
   "weights":"f15622d8f15622d8f15622d8f15622d8f15622d8f15622d8f15622d8f15626e99",
   "graphDigest":"0123456789abcdef","precision":"fp16","sizes":[1024,512],
   "defaultSize":1024,"msPerFrame":{"1024":18,"512":9},"licence":"Apache-2.0","lastError":"","label":"Anime (fast)"},
  "birefnet-lite":{"state":"loading","reason":"","percent":43,
   "weights":"ee11ee11","graphDigest":"","precision":"fp32","sizes":[1024],
   "defaultSize":1024,"msPerFrame":{"1024":170.5},"licence":"MIT","lastError":"","label":"General (precise)"}},
 "busy":0}`

func newServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, &Client{BaseURL: srv.URL + "/"} // the trailing slash must not double up
}

func TestPing(t *testing.T) {
	var gotMethod, gotPath string
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, pingJSON)
	})
	p, err := c.Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/ping" {
		t.Errorf("request %s %s, want GET /v1/ping", gotMethod, gotPath)
	}
	if p.Protocol != Protocol || p.Version != "2026.10.1" || p.Instance != "a1b2c3" || p.ProcessingVersion != "1" {
		t.Errorf("header fields: %+v", p)
	}
	if p.Device != DeviceCUDA || p.DefaultModel != "isnet-anime" || p.Busy != 0 || p.Reason != "" {
		t.Errorf("device/default/busy: %+v", p)
	}
	if p.GPU == nil || p.GPU.Name != "NVIDIA GeForce RTX 5080" || p.GPU.TotalGiB != 16 || p.GPU.FreeGiB != 12.4 {
		t.Errorf("gpu: %+v", p.GPU)
	}
	m, ok := p.Model("isnet-anime")
	if !ok {
		t.Fatal("isnet-anime missing")
	}
	want := ModelState{State: StateReady, Percent: 100,
		Weights:     "f15622d8f15622d8f15622d8f15622d8f15622d8f15622d8f15622d8f15626e99",
		GraphDigest: "0123456789abcdef", Precision: "fp16", Sizes: []int{1024, 512}, DefaultSize: 1024,
		MsPerFrame: map[string]float64{"1024": 18, "512": 9}, Licence: "Apache-2.0", Label: "Anime (fast)"}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("isnet-anime:\n got %+v\nwant %+v", m, want)
	}
	lite, ok := p.Model("birefnet-lite")
	if !ok || lite.State != StateLoading || lite.Percent != 43 || lite.MsPerFrame["1024"] != 170.5 {
		t.Errorf("birefnet-lite: %+v", lite)
	}
	if _, ok := p.Model("u2netp"); ok {
		t.Error("unknown model reported as offered")
	}
	// A CPU ping has gpu: null.
	_, c2 := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"protocol":1,"device":"cpu","gpu":null,"models":{},"busy":1}`)
	})
	p2, err := c2.Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p2.GPU != nil || p2.Device != DeviceCPU || p2.Busy != 1 {
		t.Errorf("cpu ping: %+v", p2)
	}
}

func TestPingErrors(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"CUDA EP not available"}`)
	})
	_, err := c.Ping(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || se.Message != "CUDA EP not available" {
		t.Fatalf("503 ping: %v", err)
	}
	// Garbage body on a 200.
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>") })
	if _, err := c.Ping(context.Background()); err == nil {
		t.Error("garbage ping body: no error")
	}
	// Transport failure: a closed server is not a StatusError.
	srv, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {})
	srv.Close()
	_, err = c.Ping(context.Background())
	if err == nil || errors.As(err, &se) {
		t.Errorf("closed server: %v", err)
	}
	// No URL configured.
	if _, err := (&Client{}).Ping(context.Background()); err == nil {
		t.Error("empty BaseURL: no error")
	}
}

func TestWarmAndUnload(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	status := http.StatusAccepted
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		if status/100 != 2 {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"model loading","retryAfterMs":2500}`)
			return
		}
		w.WriteHeader(status)
	})
	if err := c.Warm(context.Background(), "birefnet-lite", ""); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/warm" || gotQuery != "model=birefnet-lite" {
		t.Errorf("warm request: %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if err := c.Unload(context.Background(), "isnet-anime", ""); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/unload" || gotQuery != "model=isnet-anime" {
		t.Errorf("unload request: %s?%s", gotPath, gotQuery)
	}
	if err := c.Unload(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/unload" || gotQuery != "" {
		t.Errorf("unload-all request: %s?%s", gotPath, gotQuery)
	}
	if err := c.Warm(context.Background(), "", ""); err == nil {
		t.Error("warm with no model: no error")
	}
	status = http.StatusServiceUnavailable
	err := c.Warm(context.Background(), "birefnet-lite", "")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || se.Message != "model loading" || se.RetryAfterMS != 2500 {
		t.Fatalf("503 warm: %v", err)
	}
	if se.RetryAfter() != 2500*time.Millisecond {
		t.Errorf("RetryAfter = %v", se.RetryAfter())
	}
}

// writeRecords writes recs as the sidecar's record stream, with the
// zero-length terminator when term is true.
func writeRecords(w io.Writer, recs [][]byte, term bool) {
	var hdr [4]byte
	for _, r := range recs {
		binary.BigEndian.PutUint32(hdr[:], uint32(len(r)))
		w.Write(hdr[:])
		w.Write(r)
	}
	if term {
		w.Write([]byte{0, 0, 0, 0})
	}
}

func fakePNG(i int) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{byte(i)}, 10+i)...)
}

func TestMatteRecords(t *testing.T) {
	const size, frames = 4, 3
	body := make([]byte, BodyLength(frames, size))
	for i := range body {
		body[i] = byte(i * 7)
	}
	want := [][]byte{fakePNG(1), fakePNG(2), fakePNG(3)}
	var gotQuery, gotType string
	var gotLen int64
	var gotBody []byte
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotType, gotLen = r.URL.RawQuery, r.Header.Get("Content-Type"), r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/matte" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/x-ezlg-mattes")
		writeRecords(w, want, true)
	})
	var got [][]byte
	err := c.Matte(context.Background(), "isnet-anime", "", size, frames, bytes.NewReader(body), func(png []byte) error {
		got = append(got, png)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "frames=3&model=isnet-anime&size=4" {
		t.Errorf("query %q", gotQuery)
	}
	if gotType != ContentType || gotLen != int64(len(body)) || !bytes.Equal(gotBody, body) {
		t.Errorf("request body: type %q len %d (want %d), bytes equal %v", gotType, gotLen, len(body), bytes.Equal(gotBody, body))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records:\n got %q\nwant %q", got, want)
	}
	// The body may be any reader of the right length, not only a bytes.Reader.
	got = nil
	err = c.Matte(context.Background(), "isnet-anime", "", size, frames, io.MultiReader(bytes.NewReader(body[:5]), bytes.NewReader(body[5:])), func(png []byte) error {
		got = append(got, png)
		return nil
	})
	if err != nil || len(got) != frames || !bytes.Equal(gotBody, body) {
		t.Errorf("multireader body: err %v, %d records, body equal %v", err, len(got), bytes.Equal(gotBody, body))
	}
}

func TestMatteStreamErrors(t *testing.T) {
	const size, frames = 2, 3
	recs := [][]byte{fakePNG(1), fakePNG(2), fakePNG(3)}
	cases := []struct {
		name  string
		write func(w io.Writer)
		want  error
		calls int // records delivered before the error
	}{
		{"cut inside a header", func(w io.Writer) {
			writeRecords(w, recs[:1], false)
			w.Write([]byte{0, 0})
		}, ErrTruncated, 1},
		{"cut inside a record", func(w io.Writer) {
			writeRecords(w, recs[:1], false)
			w.Write([]byte{0, 0, 0, 20, 1, 2, 3})
		}, ErrTruncated, 1},
		{"empty response", func(w io.Writer) {}, ErrTruncated, 0},
		{"short count", func(w io.Writer) { writeRecords(w, recs[:2], true) }, ErrShortCount, 2},
		{"missing terminator", func(w io.Writer) { writeRecords(w, recs, false) }, ErrMissingTerminator, 3},
		{"extra record instead of the terminator", func(w io.Writer) { writeRecords(w, append(recs, fakePNG(4)), true) }, ErrMissingTerminator, 3},
		{"record too large", func(w io.Writer) { w.Write([]byte{0xff, 0xff, 0xff, 0xff}) }, ErrRecordTooLarge, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				tc.write(w)
			})
			calls := 0
			err := c.Matte(context.Background(), "isnet-anime", "", size, frames, bytes.NewReader(make([]byte, BodyLength(frames, size))), func([]byte) error {
				calls++
				return nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var se *StatusError
			if errors.As(err, &se) {
				t.Errorf("stream error reported as StatusError: %v", err)
			}
			if calls != tc.calls {
				t.Errorf("%d records delivered, want %d", calls, tc.calls)
			}
		})
	}
}

func TestMatteStatusErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantMsg string
		wantMS  int
	}{
		{"503 loading", 503, `{"error":"model loading","retryAfterMs":1500}`, "model loading", 1500},
		{"503 oom", 503, `{"error":"out of memory"}`, "out of memory", 0},
		{"507 oom after unload", 507, `{"error":"out of memory after unload"}`, "out of memory after unload", 0},
		{"404 plain text", 404, "no such model\n", "Not Found", 0},
		{"400 html", 400, "<html>bad</html>", "Bad Request", 0},
		{"413 empty", 413, "", "Request Entity Too Large", 0},
		{"599 unknown code", 599, "", "status 599", 0},
		{"float retryAfterMs", 503, `{"error":"model loading","retryAfterMs":1500.9}`, "model loading", 1500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			calls := 0
			err := c.Matte(context.Background(), "isnet-anime", "", 2, 1, bytes.NewReader(make([]byte, 12)), func([]byte) error {
				calls++
				return nil
			})
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *StatusError", err)
			}
			if se.Status != tc.status || se.Message != tc.wantMsg || se.RetryAfterMS != tc.wantMS {
				t.Errorf("got %+v, want status %d msg %q retry %d", se, tc.status, tc.wantMsg, tc.wantMS)
			}
			if string(se.Body) != tc.body {
				t.Errorf("Body %q, want %q", se.Body, tc.body)
			}
			if calls != 0 {
				t.Errorf("callback ran %d times on a %d", calls, tc.status)
			}
		})
	}
	se := &StatusError{Status: 503, Message: "model loading", RetryAfterMS: 1500}
	if se.Error() != "matte sidecar: HTTP 503: model loading (retry after 1500 ms)" {
		t.Errorf("Error() = %q", se.Error())
	}
	se = &StatusError{Status: 507, Message: "out of memory"}
	if se.Error() != "matte sidecar: HTTP 507: out of memory" || se.RetryAfter() != 0 {
		t.Errorf("Error() = %q, RetryAfter %v", se.Error(), se.RetryAfter())
	}
}

func TestMatteCallbackErrorStopsTheParse(t *testing.T) {
	recs := [][]byte{fakePNG(1), fakePNG(2), fakePNG(3)}
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		writeRecords(w, recs, true)
	})
	boom := errors.New("disk full")
	calls := 0
	err := c.Matte(context.Background(), "isnet-anime", "", 2, 3, bytes.NewReader(make([]byte, 36)), func([]byte) error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	})
	if err != boom {
		t.Errorf("err = %v, want the callback's error as is", err)
	}
	if calls != 2 {
		t.Errorf("callback ran %d times, want 2", calls)
	}
}

func TestMatteBadArgsMakeNoRequest(t *testing.T) {
	hits := 0
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) { hits++ })
	each := func([]byte) error { return nil }
	body := bytes.NewReader(nil)
	ctx := context.Background()
	if err := c.Matte(ctx, "", "", 1024, 8, body, each); err == nil {
		t.Error("empty model accepted")
	}
	if err := c.Matte(ctx, "isnet-anime", "", 0, 8, body, each); err == nil {
		t.Error("size 0 accepted")
	}
	if err := c.Matte(ctx, "isnet-anime", "", 1024, 0, body, each); err == nil {
		t.Error("frames 0 accepted")
	}
	if err := c.Matte(ctx, "isnet-anime", "", 1024, 8, body, nil); err == nil {
		t.Error("nil callback accepted")
	}
	if hits != 0 {
		t.Errorf("%d requests made", hits)
	}
}

func TestMatteBodyLengthMismatchFails(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		writeRecords(w, [][]byte{fakePNG(1)}, true)
	})
	// 1 frame of 2×2 is 12 bytes; a generic reader of 6 must fail the request.
	short := io.MultiReader(bytes.NewReader(make([]byte, 6)))
	err := c.Matte(context.Background(), "isnet-anime", "", 2, 1, short, func([]byte) error { return nil })
	if err == nil {
		t.Fatal("short body accepted")
	}
	var se *StatusError
	if errors.As(err, &se) {
		t.Errorf("length mismatch reported as a StatusError: %v", err)
	}
	long := io.MultiReader(bytes.NewReader(make([]byte, 20)))
	if err := c.Matte(context.Background(), "isnet-anime", "", 2, 1, long, func([]byte) error { return nil }); err == nil {
		t.Error("long body accepted")
	}
}

func TestMatteContextCancel(t *testing.T) {
	started := make(chan struct{})
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // the client's cancel closes the connection
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	err := c.Matte(ctx, "isnet-anime", "", 2, 1, bytes.NewReader(make([]byte, 12)), func([]byte) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFactsRoundTrip(t *testing.T) {
	var p Ping
	if err := json.Unmarshal([]byte(pingJSON), &p); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = time.Now })

	dir := t.TempDir()
	path := filepath.Join(dir, "mattes", FactsName) // the directory does not exist yet
	if _, err := LoadFacts(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing facts: err = %v, want fs.ErrNotExist", err)
	}
	if err := SaveFacts(path, &p); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.V != FactsVersion || !f.Saved.Equal(fixed) {
		t.Errorf("v %d saved %v", f.V, f.Saved)
	}
	if !reflect.DeepEqual(f.Ping, p) {
		t.Errorf("ping round trip:\n got %+v\nwant %+v", f.Ping, p)
	}
	m, ok := f.Model("isnet-anime")
	if !ok || m.Weights != p.Models["isnet-anime"].Weights || m.MsPerFrameAt(0) != 18 {
		t.Errorf("facts model: %+v", m)
	}
	// A second save replaces the file and leaves no temp file behind.
	p.Busy = 3
	p.Models["isnet-anime"] = ModelState{State: StateMissing, Reason: "download failed", Weights: "ff"}
	if err := SaveFacts(path, &p); err != nil {
		t.Fatal(err)
	}
	f2, err := LoadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Busy != 3 || f2.Models["isnet-anime"].State != StateMissing {
		t.Errorf("second save not visible: %+v", f2.Ping)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 || entries[0].Name() != FactsName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v, want only %s", names, FactsName)
	}
	// The flattened file shape: ping fields at the top level.
	raw, _ := os.ReadFile(path)
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "saved", "protocol", "processingVersion", "device", "defaultModel", "models", "busy", "gpu"} {
		if _, ok := top[k]; !ok {
			t.Errorf("facts file lacks %q", k)
		}
	}
	// Another version is refused; invalid JSON is refused; nil ping is refused.
	os.WriteFile(path, []byte(`{"v":2}`), 0o644)
	if _, err := LoadFacts(path); !errors.Is(err, ErrFactsVersion) {
		t.Errorf("v2 facts: err = %v", err)
	}
	os.WriteFile(path, []byte(`{`), 0o644)
	if _, err := LoadFacts(path); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("corrupt facts: err = %v", err)
	}
	if err := SaveFacts(path, nil); err == nil {
		t.Error("nil ping saved")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	fixed := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = time.Now })

	dir := t.TempDir()
	path := filepath.Join(dir, "clip", ManifestName)
	if _, err := ReadManifest(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing manifest: err = %v", err)
	}
	m := Manifest{Key: strings.Repeat("ab", 32), Src: strings.Repeat("cd", 32), Model: "isnet-anime",
		Weights: strings.Repeat("ef", 32), Proc: "1", GraphDigest: "0123", Precision: "fp16", Size: 1024,
		FPS: "25", Frames: 45, Device: "cuda", MsPerFrame: 18.4}
	if err := WriteManifest(path, &m); err != nil {
		t.Fatal(err)
	}
	if m.V != 0 || !m.Created.IsZero() {
		t.Error("WriteManifest modified the caller's struct")
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	want := m
	want.V = ManifestVersion
	want.Created = fixed
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("manifest round trip:\n got %+v\nwant %+v", *got, want)
	}
	// An explicit Created is kept.
	m.Created = fixed.Add(-time.Hour)
	if err := WriteManifest(path, &m); err != nil {
		t.Fatal(err)
	}
	if got, _ = ReadManifest(path); !got.Created.Equal(fixed.Add(-time.Hour)) {
		t.Errorf("Created = %v", got.Created)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("clip dir holds %d entries, want only the manifest", len(entries))
	}
	// The spec's field names (§4.1 step 8).
	raw, _ := os.ReadFile(path)
	var top map[string]any
	json.Unmarshal(raw, &top)
	for _, k := range []string{"v", "key", "src", "model", "weights", "proc", "graphDigest", "precision", "size", "fps", "frames", "device", "msPerFrame", "created"} {
		if _, ok := top[k]; !ok {
			t.Errorf("manifest lacks %q", k)
		}
	}
	os.WriteFile(path, []byte(`{"v":7,"frames":3}`), 0o644)
	if _, err := ReadManifest(path); !errors.Is(err, ErrManifestVersion) {
		t.Errorf("v7 manifest: err = %v", err)
	}
	os.WriteFile(path, []byte(`{"v":1,"frames":0}`), 0o644)
	if _, err := ReadManifest(path); err == nil {
		t.Error("manifest with 0 frames accepted")
	}
	if err := WriteManifest(path, nil); err == nil {
		t.Error("nil manifest written")
	}
	if FrameFile(1) != "000001.png" || FrameFile(45) != "000045.png" || FrameFile(1234567) != "1234567.png" {
		t.Errorf("FrameFile: %s %s %s", FrameFile(1), FrameFile(45), FrameFile(1234567))
	}
}

func TestFrameKeyGolden(t *testing.T) {
	rgb := make([]byte, 2*2*3)
	for i := range rgb {
		rgb[i] = byte(i)
	}
	sum := sha256.Sum256(rgb)
	if got := FrameKey(rgb); got != hex.EncodeToString(sum[:]) || len(got) != 64 {
		t.Errorf("FrameKey = %s", got)
	}
	// Pinned: a change here means every frames-store file is orphaned.
	const golden = "fff3a9bcdd37363d703c1c4f9512533686157868f0d4f16a0f02d0f1da24f9a2"
	if got := FrameKey(rgb); got != golden {
		t.Errorf("FrameKey golden:\n got %s\nwant %s", got, golden)
	}
	if FrameKey(nil) != FrameKey([]byte{}) {
		t.Error("nil and empty differ")
	}
}

func goldenParts() ClipKeyParts {
	return ClipKeyParts{
		Src: strings.Repeat("ab", 32),
		Temporal: []recipe.Op{
			{Kind: recipe.OpTrim, Params: json.RawMessage(`{ "start": 1.5, "end": 3 }`)},
			{Kind: recipe.OpFPS, Params: json.RawMessage(`{"fps":25}`)},
		},
		Probe: recipe.ProbeInfo{Format: "mov", Codec: "prores", Profile: "4444", PixFmt: "yuva444p10le", Bits: 10,
			Width: 1920, Height: 1080, FPS: 29.97, Duration: 12.5, Frames: 375, HasAlpha: true,
			Kind: recipe.KindVideo, Premultiplied: true},
		InfoVersion: 6,
		FPS:         "25",
		Model:       "isnet-anime",
		Size:        1024,
		Precision:   "fp16",
		Weights:     strings.Repeat("f1", 32),
		Proc:        "1",
	}
}

func TestClipKeyGolden(t *testing.T) {
	const wantText = "matte|1\n" +
		`{"v":1,"sources":["abababababababababababababababababababababababababababababababab"],"ops":[{"kind":"trim","params":{"end":3,"start":1.5}},{"kind":"fps","params":{"fps":25}}],"output":{"format":""}}` +
		"\nprobe=" + `{"kind":"video","isStill":false,"width":1920,"height":1080,"fps":29.97,"duration":12.5,"colorStream":0,"alphaStream":0,"premultiplied":true,"sequence":null}` + "|info=6" +
		"\nfps=25|model=isnet-anime|size=1024|prec=fp16|weights=f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1|proc=1"
	if got := clipKeyText(goldenParts()); got != wantText {
		t.Errorf("clip key text:\n got %q\nwant %q", got, wantText)
	}
	sum := sha256.Sum256([]byte(wantText))
	if got := ClipKey(goldenParts()); got != hex.EncodeToString(sum[:]) {
		t.Errorf("ClipKey = %s, not the sha256 of its text", got)
	}
	// Pinned: a change here orphans every memoised matte on /data.
	const golden = "9df7bf02b4738b9d69b386e6825900755e12b1fd6c98849de6bb71175b778a98"
	if got := ClipKey(goldenParts()); got != golden {
		t.Errorf("ClipKey golden:\n got %s\nwant %s", got, golden)
	}
	// A sequence source keys its pattern, count, delay and mixedness.
	seq := goldenParts()
	seq.Probe = recipe.ProbeInfo{Kind: recipe.KindSequence, Width: 64, Height: 64, FPS: 10, Duration: 0.5,
		Sequence: &recipe.SequenceInfo{Count: 5, Pattern: "%06d.png", DelayMS: 100, Mixed: true}}
	if got := clipKeyText(seq); !strings.Contains(got, `"sequence":{"pattern":"%06d.png","count":5,"delayMs":100,"mixed":true}`) {
		t.Errorf("sequence facts missing from %q", got)
	}
}

func TestClipKeyStability(t *testing.T) {
	base := ClipKey(goldenParts())
	if base != ClipKey(goldenParts()) {
		t.Fatal("ClipKey is not deterministic")
	}
	// Facts the prefix does not compile from, and client formatting, leave the key alone.
	same := []struct {
		name string
		mut  func(*ClipKeyParts)
	}{
		{"codec/pixfmt/bits/frames/hasAlpha", func(p *ClipKeyParts) {
			p.Probe.Codec, p.Probe.PixFmt, p.Probe.Bits, p.Probe.Frames, p.Probe.HasAlpha, p.Probe.HasAudio = "h264", "yuv420p", 8, 0, false, true
		}},
		{"param whitespace and key order", func(p *ClipKeyParts) {
			p.Temporal[0].Params = json.RawMessage(`{"end":3,"start":1.5}`)
		}},
		{"nil vs empty temporal", func(p *ClipKeyParts) { p.Temporal = nil }},
	}
	empty := goldenParts()
	empty.Temporal = []recipe.Op{}
	emptyKey := ClipKey(empty)
	for _, tc := range same {
		p := goldenParts()
		tc.mut(&p)
		got := ClipKey(p)
		want := base
		if tc.name == "nil vs empty temporal" {
			want = emptyKey
		}
		if got != want {
			t.Errorf("%s: key changed", tc.name)
		}
	}
	// Every keyed part changes it.
	diff := []struct {
		name string
		mut  func(*ClipKeyParts)
	}{
		{"src", func(p *ClipKeyParts) { p.Src = strings.Repeat("ba", 32) }},
		{"temporal op param", func(p *ClipKeyParts) { p.Temporal[0].Params = json.RawMessage(`{"start":1.6,"end":3}`) }},
		{"temporal op order", func(p *ClipKeyParts) { p.Temporal[0], p.Temporal[1] = p.Temporal[1], p.Temporal[0] }},
		{"temporal op removed", func(p *ClipKeyParts) { p.Temporal = p.Temporal[:1] }},
		{"width", func(p *ClipKeyParts) { p.Probe.Width = 1280 }},
		{"height", func(p *ClipKeyParts) { p.Probe.Height = 720 }},
		{"probe fps", func(p *ClipKeyParts) { p.Probe.FPS = 30 }},
		{"duration", func(p *ClipKeyParts) { p.Probe.Duration = 13 }},
		{"kind", func(p *ClipKeyParts) { p.Probe.Kind = recipe.KindAnimation }},
		{"isStill", func(p *ClipKeyParts) { p.Probe.IsStill = true }},
		{"colorStream", func(p *ClipKeyParts) { p.Probe.ColorStream = 2 }},
		{"alphaStream", func(p *ClipKeyParts) { p.Probe.AlphaStream = 3 }},
		{"premultiplied", func(p *ClipKeyParts) { p.Probe.Premultiplied = false }},
		{"sequence", func(p *ClipKeyParts) {
			p.Probe.Sequence = &recipe.SequenceInfo{Count: 3, Pattern: "%06d.png", DelayMS: 40}
		}},
		{"infoVersion", func(p *ClipKeyParts) { p.InfoVersion = 7 }},
		{"plan fps", func(p *ClipKeyParts) { p.FPS = "50" }},
		{"model", func(p *ClipKeyParts) { p.Model = "birefnet-lite" }},
		{"size", func(p *ClipKeyParts) { p.Size = 512 }},
		{"precision", func(p *ClipKeyParts) { p.Precision = "fp32" }},
		{"weights", func(p *ClipKeyParts) { p.Weights = strings.Repeat("f2", 32) }},
		{"proc", func(p *ClipKeyParts) { p.Proc = "2" }},
	}
	seen := map[string]string{base: "base"}
	for _, tc := range diff {
		p := goldenParts()
		tc.mut(&p)
		got := ClipKey(p)
		if prev, dup := seen[got]; dup {
			t.Errorf("%s: key equals %s", tc.name, prev)
		}
		seen[got] = tc.name
	}
	// Invalid params panic, like recipe.Hash.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("invalid params did not panic")
			}
		}()
		p := goldenParts()
		p.Temporal[0].Params = json.RawMessage(`{nope`)
		ClipKey(p)
	}()
}

func TestTemporalOps(t *testing.T) {
	ops := []recipe.Op{
		{Kind: recipe.OpChromaKey}, {Kind: recipe.OpDelay}, {Kind: recipe.OpCrop}, {Kind: recipe.OpTrim},
		{Kind: recipe.OpFeather}, {Kind: recipe.OpSpeed}, {Kind: recipe.OpReverse}, {Kind: recipe.OpFPS},
		{Kind: recipe.OpUnpremultiply}, {Kind: recipe.OpBounce}, {Kind: recipe.OpOverlay}, {Kind: recipe.OpMorph},
	}
	var got []string
	for _, op := range TemporalOps(ops) {
		got = append(got, op.Kind)
	}
	want := []string{recipe.OpDelay, recipe.OpTrim, recipe.OpSpeed, recipe.OpFPS, recipe.OpUnpremultiply}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TemporalOps = %v, want %v", got, want)
	}
	if TemporalOps(nil) != nil || TemporalOps([]recipe.Op{{Kind: recipe.OpCrop}}) != nil {
		t.Error("no temporal ops should give nil")
	}
}

func TestModelStateHelpers(t *testing.T) {
	s := ModelState{Sizes: []int{1024, 512}, DefaultSize: 1024, MsPerFrame: map[string]float64{"1024": 18, "512": 9}}
	if s.EffectiveSize(0) != 1024 || s.EffectiveSize(512) != 512 || s.EffectiveSize(640) != 640 {
		t.Error("EffectiveSize")
	}
	if !s.HasSize(1024) || !s.HasSize(512) || s.HasSize(640) || s.HasSize(0) {
		t.Error("HasSize")
	}
	if s.MsPerFrameAt(0) != 18 || s.MsPerFrameAt(512) != 9 || s.MsPerFrameAt(640) != 0 {
		t.Error("MsPerFrameAt")
	}
	var zero ModelState
	if zero.MsPerFrameAt(0) != 0 || zero.HasSize(1024) || zero.EffectiveSize(0) != 0 {
		t.Error("zero ModelState")
	}
	var nilPing *Ping
	if _, ok := nilPing.Model("isnet-anime"); ok {
		t.Error("nil ping offers a model")
	}
}
