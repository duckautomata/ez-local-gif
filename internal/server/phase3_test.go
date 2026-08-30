package server

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Phase 3 HTTP surface: GET /api/fonts, POST /api/proxy, "sources" on
// /api/still and /api/jobs, and the capability flags the SPA gates on.

// capsFeatures fetches /api/capabilities and returns its features map.
func capsFeatures(t *testing.T, e *env) map[string]bool {
	t.Helper()
	resp, body := e.get(t, "/api/capabilities")
	if resp.StatusCode != 200 {
		t.Fatalf("capabilities: %d %s", resp.StatusCode, body)
	}
	var caps struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatalf("decode capabilities: %v: %s", err, body)
	}
	return caps.Features
}

func TestFeatures(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	versions := e.jm.ToolVersions()
	for _, fonts := range []bool{false, true} {
		want := map[string]bool{
			"fit": true, "sequence": true, "optimize": true,
			"keying": true, "overlays": true, "proxy": true, "fonts": fonts,
			"feather": true, "bounce": true,
			// Phase 4 flags: this env has no input/output dirs, and gifski
			// mirrors the version-probed toolchain (TestCapabilitiesInOut
			// covers the enabled side, TestGifskiFeatureProbed the resolved-
			// but-unrunnable one).
			"inputPick": false, "outputSave": false, "gifski": versions["gifski"] != "",
		}
		if got := e.s.features(fonts, versions); !maps.Equal(got, want) {
			t.Errorf("features(%v) = %v, want %v", fonts, got, want)
		}
	}
}

// TestFontsEndpoint: GET /api/fonts is always {"fonts": [...]} — an array,
// never null — of well-formed faces, and the "fonts" capability flag is
// true exactly when that array is non-empty. With the fake fc-list (a
// duplicate face and an unnameable family in its output) the list is the
// deduplicated, sorted faces.
func TestFontsEndpoint(t *testing.T) {
	exe, _ := fakeToolExe(t, fakeModeFcList)
	e := newEnvWithTools(t, Config{}, nil, ffrun.Tools{FcList: exe})

	resp, body := e.get(t, "/api/fonts")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache", cc)
	}
	var got struct {
		Fonts *[]enc.Font `json:"fonts"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if got.Fonts == nil {
		t.Fatalf("fonts is null or missing, want an array: %s", body)
	}
	fonts := *got.Fonts
	family := regexp.MustCompile(`^[A-Za-z0-9 -]+$`)
	for i, f := range fonts {
		if f.Family == "" || f.Style == "" || f.File == "" || !family.MatchString(f.Family) {
			t.Errorf("fonts[%d] = %+v, want family (letters/digits/spaces/hyphens), style and file", i, f)
		}
	}
	if flag := capsFeatures(t, e)["fonts"]; flag != (len(fonts) > 0) {
		t.Errorf("features.fonts = %v with %d fonts listed", flag, len(fonts))
	}
	if !reflect.DeepEqual(fonts, fakeFcListFonts) {
		t.Errorf("fonts = %+v, want the fake fc-list's faces deduplicated and sorted: %+v", fonts, fakeFcListFonts)
	}
	// Read-only: a POST falls through to the /api catch-all (a JSON 404,
	// like every unknown API path), and cross-site GETs are not guarded.
	if status, body := e.send(t, "POST", "/api/fonts", "application/json", "{}", nil); status != http.StatusNotFound || errorOf(t, body) == "" {
		t.Errorf("POST /api/fonts = %d %s, want a JSON 404", status, body)
	}
	if status, _ := e.send(t, "GET", "/api/fonts", "", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != 200 {
		t.Errorf("cross-site GET /api/fonts = %d, want 200", status)
	}
}

// TestPreviewRequestSourceList: "src" and "sources" resolve to one source
// list, agree when both are given, and every entry must be a hash.
func TestPreviewRequestSourceList(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	cases := []struct {
		name string
		req  previewRequest
		want []string
		err  string
	}{
		{"src only", previewRequest{Src: a}, []string{a}, ""},
		{"sources only", previewRequest{Sources: []string{a, b}}, []string{a, b}, ""},
		{"both, agreeing", previewRequest{Src: a, Sources: []string{a, b}}, []string{a, b}, ""},
		{"both, disagreeing", previewRequest{Src: b, Sources: []string{a}}, nil, "different main"},
		{"neither", previewRequest{}, nil, "required"},
		{"empty sources, no src", previewRequest{Sources: []string{}}, nil, "required"},
		{"bad src", previewRequest{Src: "nope"}, nil, "src must be"},
		{"bad main in sources", previewRequest{Sources: []string{"nope", b}}, nil, "src must be"},
		{"bad overlay hash", previewRequest{Sources: []string{a, "NOPE"}}, nil, "sources[1]"},
		{"uppercase hash", previewRequest{Src: strings.ToUpper(a)}, nil, "src must be"},
	}
	for _, tc := range cases {
		got, err := tc.req.sourceList()
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// previewCase is one request-level rejection of /api/still or /api/proxy.
type previewCase struct {
	name string
	body any
	want int
	msg  string
}

// previewCases are the rejections /api/still and /api/proxy share; missing
// is the status an unknown source gets on the endpoint.
func previewCases(main, other, unprobed, unknown string, missing int) []previewCase {
	return []previewCase{
		{"bad json", `{not json`, 400, "invalid"},
		{"no source", `{}`, 400, "required"},
		{"empty sources", `{"sources":[]}`, 400, "required"},
		{"bad src", `{"src":"nope"}`, 400, "source hash"},
		{"bad overlay hash", map[string]any{"sources": []string{main, "nope"}}, 400, "sources[1]"},
		{"src and sources disagree", map[string]any{"src": other, "sources": []string{main}}, 400, "different main"},
		{"unknown main", map[string]any{"sources": []string{unknown}}, missing, "source 0"},
		{"unknown main via src", map[string]any{"src": unknown}, missing, "source 0"},
		{"unknown overlay", map[string]any{"sources": []string{main, unknown}}, missing, "source 1"},
		{"unprobed overlay", map[string]any{"sources": []string{main, unprobed}}, 409, "source 1"},
		{"unprobed main", map[string]any{"src": unprobed}, 409, "source 0"},
	}
}

// TestStillSources: /api/still takes "sources" (several hashes) as well as
// "src"; one source through either spelling is the same preview (and the
// same memo entry); every source must be an uploaded, probed blob (404 /
// 409, naming the index); several sources render through the manager's
// multi-source still.
func TestStillSources(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	main := putProbedSource(t, e, "a.gif", tinyGIF(t))
	other := putProbedSource(t, e, "b.png", tinyPNG(t))
	unprobedBlob, err := e.st.PutBlob(strings.NewReader("raw"), "raw.mov")
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Repeat("b", 64)

	for _, tc := range previewCases(main, other, unprobedBlob.Hash, unknown, http.StatusNotFound) {
		resp, body := e.postJSON(t, "/api/still", tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
			continue
		}
		if msg := errorOf(t, body); !strings.Contains(msg, tc.msg) {
			t.Errorf("%s: error %q does not mention %q", tc.name, msg, tc.msg)
		}
	}

	out := map[string]any{"format": "gif"}
	if e.tools.FFmpeg != "" {
		var first []byte
		for i, req := range []map[string]any{
			{"src": main, "output": out, "t": 0, "maxW": 2},
			{"sources": []string{main}, "output": out, "t": 0, "maxW": 2},
			{"src": main, "sources": []string{main}, "output": out, "t": 0, "maxW": 2},
		} {
			resp, body := e.postJSON(t, "/api/still", req)
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(body, []byte("\x89PNG")) {
				t.Fatalf("request %d: %d %q %q", i, resp.StatusCode, resp.Header.Get("Content-Type"), body[:min(len(body), 40)])
			}
			if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
				t.Errorf("request %d: content-length %q for %d bytes", i, cl, len(body))
			}
			if i == 0 {
				first = body
			} else if !bytes.Equal(body, first) {
				t.Errorf("request %d: the sources spelling rendered a different frame than src", i)
			}
		}
	}

	// Two sources: an overlay recipe renders through the manager's
	// multi-source still (a 500 naming ffmpeg when there is none).
	overlay := recipe.Op{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1,"x":2,"y":2}`)}
	resp, body := e.postJSON(t, "/api/still", map[string]any{"sources": []string{main, other}, "ops": []recipe.Op{overlay}, "output": out, "t": 0, "maxW": 8})
	switch {
	case e.tools.FFmpeg == "":
		if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(errorOf(t, body), "ffmpeg") {
			t.Errorf("multi-source still without ffmpeg: %d %s, want 500 naming ffmpeg", resp.StatusCode, body)
		}
	case resp.StatusCode != 200:
		t.Errorf("multi-source still: %d %s, want 200", resp.StatusCode, body)
	case resp.Header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(body, []byte("\x89PNG")):
		t.Errorf("multi-source still: %q %q", resp.Header.Get("Content-Type"), body[:min(len(body), 40)])
	}
}

// TestProxyEndpoint: POST /api/proxy shares the still's request shape and
// guards (415 / 403 / 400 / 409), refuses negative knobs, answers an
// unknown source with 400, and returns the animated WebP preview.
func TestProxyEndpoint(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	main := putProbedSource(t, e, "a.gif", tinyGIF(t))
	other := putProbedSource(t, e, "b.png", tinyPNG(t))
	unprobedBlob, err := e.st.PutBlob(strings.NewReader("raw"), "raw.mov")
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Repeat("b", 64)

	cases := previewCases(main, other, unprobedBlob.Hash, unknown, http.StatusBadRequest)
	cases = append(cases,
		previewCase{"negative maxW", map[string]any{"src": main, "maxW": -1}, 400, "maxW"},
		previewCase{"negative maxSeconds", map[string]any{"src": main, "maxSeconds": -0.5}, 400, "maxSeconds"},
	)
	for _, tc := range cases {
		resp, body := e.postJSON(t, "/api/proxy", tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
			continue
		}
		if msg := errorOf(t, body); !strings.Contains(msg, tc.msg) {
			t.Errorf("%s: error %q does not mention %q", tc.name, msg, tc.msg)
		}
	}
	valid := `{"sources":["` + main + `"],"output":{"format":"gif"},"maxW":16,"maxSeconds":1}`
	if status, body := e.send(t, "POST", "/api/proxy", "text/plain", valid, nil); status != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d %s, want 415", status, body)
	}
	if status, body := e.send(t, "POST", "/api/proxy", "application/json", valid, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusForbidden {
		t.Errorf("cross-site = %d %s, want 403", status, body)
	}
	if status, body := e.send(t, "GET", "/api/proxy", "", "", nil); status != http.StatusNotFound || errorOf(t, body) == "" {
		t.Errorf("GET /api/proxy = %d %s, want a JSON 404 (the /api catch-all)", status, body)
	}

	// A well-formed request: the animated WebP (both spellings of the
	// source), or a 500 naming the missing ffmpeg.
	for i, req := range []string{valid, `{"src":"` + main + `","output":{"format":"gif"},"maxW":16,"maxSeconds":1}`} {
		resp, body := e.postJSON(t, "/api/proxy", req)
		switch {
		case e.tools.FFmpeg == "":
			if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(errorOf(t, body), "ffmpeg") {
				t.Errorf("request %d without ffmpeg: %d %s, want 500 naming ffmpeg", i, resp.StatusCode, body)
			}
		case resp.StatusCode != 200:
			t.Errorf("request %d: %d %s, want 200", i, resp.StatusCode, body)
		default:
			if ct := resp.Header.Get("Content-Type"); ct != "image/webp" {
				t.Errorf("request %d: content-type %q, want image/webp", i, ct)
			}
			if len(body) < 12 || !bytes.HasPrefix(body, []byte("RIFF")) || string(body[8:12]) != "WEBP" {
				t.Errorf("request %d: body is not a WebP: %q", i, body[:min(len(body), 16)])
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=3600" {
				t.Errorf("request %d: cache-control %q", i, cc)
			}
			if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
				t.Errorf("request %d: content-length %q for %d bytes", i, cl, len(body))
			}
		}
	}
}

// TestCreateJobSources: every entry of a recipe's "sources" — overlay
// assets included — must be an uploaded (400) and probed (409) blob, named
// by index; a recipe whose sources all exist is accepted.
func TestCreateJobSources(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	main := putProbedSource(t, e, "a.gif", tinyGIF(t))
	other := putProbedSource(t, e, "b.png", tinyPNG(t))
	unprobedBlob, err := e.st.PutBlob(strings.NewReader("raw"), "raw.mov")
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Repeat("b", 64)
	out := recipe.Output{Format: "gif", Width: 8, Height: 8}

	resp, body := e.postJSON(t, "/api/jobs", recipe.Recipe{Sources: []string{main, unknown}, Output: out})
	if resp.StatusCode != 400 || !strings.Contains(errorOf(t, body), "source 1") || !strings.Contains(errorOf(t, body), "not uploaded") {
		t.Errorf("unknown overlay source: %d %s, want 400 naming source 1", resp.StatusCode, body)
	}
	resp, body = e.postJSON(t, "/api/jobs", recipe.Recipe{Sources: []string{main, unprobedBlob.Hash}, Output: out})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(errorOf(t, body), "source 1") {
		t.Errorf("unprobed overlay source: %d %s, want 409 naming source 1", resp.StatusCode, body)
	}
	overlay := recipe.Op{Kind: recipe.OpOverlay, Params: json.RawMessage(`{"source":1}`)}
	rec := recipe.Recipe{Sources: []string{main, other}, Ops: []recipe.Op{overlay}, Output: out}
	resp, body = e.postJSON(t, "/api/jobs", rec)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("two sources: %d %s, want 202", resp.StatusCode, body)
	}
	var job jobs.Job
	if err := json.Unmarshal(body, &job); err != nil || len(job.Recipe.Sources) != 2 || job.RecipeHash != jobs.ResultKey(rec) {
		t.Errorf("job = %s (%v)", body, err)
	}
}
