package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/matte"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Phase 5c HTTP surface (build brief 2026-10-09): PUT /api/matte/settings,
// POST /api/matte/unload, POST /api/matte/prompt, the extended GET
// /api/matte object and the 202 "idle" state. As in matte_test.go the
// manager has no sidecar, so the request validation is exercised end to
// end and the manager's own refusals through the mapping helpers with the
// errors jobs documents (an unoffered device wraps ErrInvalidRecipe like an
// unoffered model; no sidecar is ErrMatteUnavailable).

// matteStatus5cJSON is the GET /api/matte object with the Phase 5c fields,
// as a client decodes it; pointers tell "absent" from "empty".
type matteStatus5cJSON struct {
	matteStatusJSON
	Devices       *[]string          `json:"devices"`
	DefaultModels *map[string]string `json:"defaultModels"`
}

// TestMatteEndpoint5cShape: without a sidecar the Phase 5c fields are
// absent (omitempty — a 5b client sees the same object), and the status
// type carries them for the manager to fill: a status with devices,
// default models and per-model kind / devices encodes under the documented
// names, which the SPA agent codes against.
func TestMatteEndpoint5cShape(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	_, body := e.get(t, "/api/matte")
	var st matteStatus5cJSON
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if st.Devices != nil || st.DefaultModels != nil {
		t.Errorf("devices / defaultModels present without a sidecar: %s", body)
	}
	for _, key := range []string{`"devices"`, `"defaultModels"`, `"kind"`} {
		if strings.Contains(string(body), key) {
			t.Errorf("status carries %s without a sidecar (want omitted): %s", key, body)
		}
	}

	// The documented names of a filled status.
	full := jobs.MatteStatus{
		Enabled:       true,
		Device:        "cuda",
		Devices:       []string{"cuda", "cpu"},
		DefaultModel:  recipe.MatteModelBiRefNetLite,
		DefaultModels: map[string]string{"cuda": recipe.MatteModelBiRefNetLite, "cpu": recipe.MatteModelISNetAnime},
		Models: map[string]jobs.MatteModelStatus{
			recipe.MatteModelSAM2Tiny: {
				Label: "Guided (click to select)", Kind: matte.KindTracker, State: "ready",
				Devices: map[string]jobs.MatteModelDeviceStatus{
					"cuda": {State: "ready", Precision: "fp16", Size: 1024, MsPerFrame: 31.5, Resident: true},
					"cpu":  {State: "unavailable", Reason: "no RAM"},
				},
			},
		},
		MaxSeconds: 60, MaxFrames: 1800,
	}
	data, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"device":"cuda"`, `"devices":["cuda","cpu"]`, `"defaultModel":"birefnet-lite"`,
		`"defaultModels":{"cpu":"isnet-anime","cuda":"birefnet-lite"}`,
		`"kind":"tracker"`, `"resident":false,"devices":{"cpu":{"state":"unavailable","reason":"no RAM","resident":false},"cuda":{"state":"ready","precision":"fp16","size":1024,"msPerFrame":31.5,"resident":true}}`,
	} {
		if !strings.Contains(string(data), key) {
			t.Errorf("status JSON lacks %s: %s", key, data)
		}
	}
	var back matteStatus5cJSON
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Devices == nil || len(*back.Devices) != 2 || back.DefaultModels == nil || (*back.DefaultModels)["cpu"] != recipe.MatteModelISNetAnime {
		t.Errorf("round trip lost the 5c fields: %+v", back)
	}
}

// TestMatteSettingsEndpoint: PUT /api/matte/settings validates the body
// before the manager is asked — application/json only (415), a decodable
// body with a "device" string (400 otherwise: a misspelt key must not
// silently reset the preference), a name shaped like a device (400) — and
// answers the reset ("") with 200 and the GET /api/matte object, never
// cached. Same cross-site guard as the other state-changing endpoints; no
// GET or POST variant exists.
func TestMatteSettingsEndpoint(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	put := func(t *testing.T, contentType, body string) (int, []byte) {
		t.Helper()
		return e.send(t, http.MethodPut, "/api/matte/settings", contentType, body, nil)
	}

	if status, body := put(t, "text/plain", `{"device":""}`); status != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d %s, want 415", status, body)
	}
	for _, c := range []struct {
		body string
		want string // a fragment of the 400 message
	}{
		{`{"device":`, "invalid matte settings"},
		{`{}`, `"device" is required`},
		{`{"devcie":"cpu"}`, `"device" is required`},
		{`{"device":null}`, `"device" is required`},
		{`{"device":3}`, "invalid matte settings"},
		{`{"device":"GPU"}`, "not a device name"},
		{`{"device":"cuda "}`, "not a device name"},
		{`{"device":"cuda;rm -rf"}`, "not a device name"},
		{`{"device":"` + strings.Repeat("a", maxMatteDeviceLen+1) + `"}`, "not a device name"},
	} {
		status, body := put(t, "application/json", c.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", c.body, status, body)
			continue
		}
		if msg := errorOf(t, body); !strings.Contains(msg, c.want) {
			t.Errorf("%s: error %q lacks %q", c.body, msg, c.want)
		}
	}

	// The reset is always acceptable and answers the live status.
	req, err := http.NewRequest(http.MethodPut, e.srv.URL+"/api/matte/settings", strings.NewReader(`{"device":""}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st matteStatus5cJSON
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset: status %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("reset: cache-control = %q, want no-cache", cc)
	}
	if st.Models == nil || st.Enabled != e.jm.MatteEnabled() || st.MaxFrames != jobs.DefaultMatteMaxFrames {
		t.Errorf("reset: body is not the /api/matte object: %+v", st)
	}

	// Guards: cross-site 403 (before any validation), and the path has no
	// other method — a GET or POST falls through to the /api JSON 404.
	if status, body := e.send(t, http.MethodPut, "/api/matte/settings", "application/json", `{"device":""}`,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusForbidden {
		t.Errorf("cross-site = %d %s, want 403", status, body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if status, body := e.send(t, method, "/api/matte/settings", "application/json", `{"device":""}`, nil); status != http.StatusNotFound || errorOf(t, body) == "" {
			t.Errorf("%s /api/matte/settings = %d %s, want a JSON 404", method, status, body)
		}
	}
}

// TestMatteDeviceOffered: the pre-check against the status snapshot — a
// device the snapshot lists passes, "" (the reset) always passes, an
// unlisted one is refused naming the offered list, and a snapshot without
// a devices list (no sidecar answer, or a pre-5c one) passes everything to
// the manager's own validation.
func TestMatteDeviceOffered(t *testing.T) {
	both := jobs.MatteStatus{Devices: []string{"cuda", "cpu"}}
	for _, dev := range []string{"", "cuda", "cpu"} {
		if err := matteDeviceOffered(both, dev); err != nil {
			t.Errorf("%q with cuda+cpu: %v", dev, err)
		}
	}
	err := matteDeviceOffered(both, "rocm")
	if err == nil {
		t.Fatal("rocm with cuda+cpu: no error")
	}
	for _, want := range []string{`"rocm"`, "offered: cuda, cpu", `""`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if err := matteDeviceOffered(jobs.MatteStatus{Devices: []string{"cpu"}}, "cuda"); err == nil || !strings.Contains(err.Error(), "offered: cpu") {
		t.Errorf("cuda with cpu only: %v", err)
	}
	for _, st := range []jobs.MatteStatus{{}, {Devices: []string{}}} {
		for _, dev := range []string{"", "cuda", "rocm"} {
			if err := matteDeviceOffered(st, dev); err != nil {
				t.Errorf("%q with no devices list: %v (want the manager to decide)", dev, err)
			}
		}
	}
	// The name check is the shape only.
	for _, c := range []struct {
		dev string
		ok  bool
	}{
		{"", true}, {"cuda", true}, {"cpu", true}, {"cuda0", true}, {"rocm", true},
		{"GPU", false}, {"cuda ", false}, {" cpu", false}, {"cu-da", false}, {"cuda\n", false}, {"ünicode", false},
		{strings.Repeat("a", maxMatteDeviceLen), true}, {strings.Repeat("a", maxMatteDeviceLen+1), false},
	} {
		if got := validMatteDevice(c.dev); got != c.ok {
			t.Errorf("validMatteDevice(%q) = %v, want %v", c.dev, got, c.ok)
		}
	}
}

// TestMatteSettingsError: the manager's refusals map as documented — an
// unoffered device (wrapping jobs.ErrInvalidRecipe) 400 with the message,
// no sidecar (jobs.ErrMatteUnavailable, bare or wrapped) 503 naming the
// compose profile, a deadline 504, anything else 500 — and a client that
// hung up gets nothing.
func TestMatteSettingsError(t *testing.T) {
	call := func(t *testing.T, err error) (*httptest.ResponseRecorder, []byte) {
		t.Helper()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/api/matte/settings", nil)
		matteSettingsError(rec, r, err)
		return rec, rec.Body.Bytes()
	}
	for _, c := range []struct {
		err  error
		want int
		msg  string
	}{
		{fmt.Errorf("%w: the matte service does not offer device %q (offered: cpu)", jobs.ErrInvalidRecipe, "cuda"), http.StatusBadRequest, "does not offer device"},
		{jobs.ErrMatteUnavailable, http.StatusServiceUnavailable, "docker compose --profile matte-gpu up -d"},
		{fmt.Errorf("%w: the matte service at http://matte:9402 has not answered yet", jobs.ErrMatteUnavailable), http.StatusServiceUnavailable, "has not answered yet"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, "timed out"},
		{errors.New("write settings.json: permission denied"), http.StatusInternalServerError, "permission denied"},
	} {
		rec, body := call(t, c.err)
		if rec.Code != c.want {
			t.Errorf("%v: status %d %s, want %d", c.err, rec.Code, body, c.want)
			continue
		}
		if msg := errorOf(t, body); !strings.Contains(msg, c.msg) {
			t.Errorf("%v: error %q lacks %q", c.err, msg, c.msg)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/matte/settings", nil).WithContext(ctx)
	matteSettingsError(rec, r, errors.New("whatever"))
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
		t.Errorf("hung-up client got %d %s", rec.Code, rec.Body)
	}
}

// TestMatteUnloadEndpoint: POST /api/matte/unload is 204 with an empty body
// on every install — a sidecar that is off is not an error for the caller
// (best effort) — including a server without a manager; it takes no body
// and no content type; the cross-site guard applies; GET is the JSON 404.
func TestMatteUnloadEndpoint(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if status, body := e.send(t, http.MethodPost, "/api/matte/unload", "", "", nil); status != http.StatusNoContent || len(body) != 0 {
		t.Errorf("unload = %d %q, want 204 and no body", status, body)
	}
	if status, _ := e.send(t, http.MethodPost, "/api/matte/unload", "application/json", "{}", nil); status != http.StatusNoContent {
		t.Errorf("unload with a JSON body = %d, want 204 (the body is ignored)", status)
	}
	if status, body := e.send(t, http.MethodPost, "/api/matte/unload", "", "",
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusForbidden {
		t.Errorf("cross-site unload = %d %s, want 403", status, body)
	}
	if status, body := e.send(t, http.MethodGet, "/api/matte/unload", "", "", nil); status != http.StatusNotFound || errorOf(t, body) == "" {
		t.Errorf("GET /api/matte/unload = %d %s, want a JSON 404", status, body)
	}

	root := t.TempDir()
	st, err := store.New(filepath.Join(root, "data"), filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Version: "nojm"}, st, nil, hostTools(), nil)
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/matte/unload", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("no manager: unload = %d %s, want 204", rec.Code, rec.Body)
	}
}

// TestDecodePrompts: the "prompts" field decodes in both shapes — the
// matte op's bare array (one object implied, obj 1) and the
// matte.TrackPrompts object — and refuses an absent, null or other-typed
// value; the frame filter and validation that follow are the client
// package's (ForFrame / Validate).
func TestDecodePrompts(t *testing.T) {
	arr := `[{"frame":3,"points":[[0.5,0.5,1],[0.1,0.1,0]],"box":[0.1,0.2,0.6,0.9]},{"frame":0,"points":[[0.2,0.3,1]]}]`
	tp, err := decodePrompts(json.RawMessage(arr))
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if tp.Obj != 1 || len(tp.Prompts) != 2 || tp.Prompts[0].Frame != 3 || tp.Prompts[0].Box == nil || (*tp.Prompts[0].Box)[3] != 0.9 || len(tp.Prompts[0].Points) != 2 || tp.Prompts[1].Box != nil {
		t.Errorf("array decoded to %+v", tp)
	}
	obj := `{"obj":1,"prompts":` + arr + `}`
	tp2, err := decodePrompts(json.RawMessage(obj))
	if err != nil {
		t.Fatalf("object: %v", err)
	}
	if tp2.Canonical() != tp.Canonical() {
		t.Errorf("object and array differ: %q vs %q", tp2.Canonical(), tp.Canonical())
	}
	// Whitespace is fine; the recipe's own MattePrompt shape is the array's.
	rp := []recipe.MattePrompt{{Frame: 3, Points: [][3]float64{{0.5, 0.5, 1}}, Box: &[4]float64{0.1, 0.2, 0.6, 0.9}}}
	data, _ := json.Marshal(rp)
	tp3, err := decodePrompts(json.RawMessage("  " + string(data) + "\n"))
	if err != nil {
		t.Fatalf("recipe prompts: %v", err)
	}
	if tp3.Prompts[0].Frame != 3 || tp3.Prompts[0].Box == nil || tp3.Prompts[0].Points[0][2] != 1 {
		t.Errorf("recipe prompts decoded to %+v", tp3)
	}
	// Only the clicked frame's prompts reach the tracker.
	if f := tp.ForFrame(0); len(f.Prompts) != 1 || f.Prompts[0].Frame != 0 || f.Obj != 1 {
		t.Errorf("ForFrame(0) = %+v", f)
	}
	if f := tp.ForFrame(7); len(f.Prompts) != 0 {
		t.Errorf("ForFrame(7) = %+v, want none", f)
	}

	for _, raw := range []string{"", "null", "  ", `"box"`, `3`, `true`, `[1,2]`, `[{"frame":"x"}]`, `{"prompts":3}`} {
		if _, err := decodePrompts(json.RawMessage(raw)); err == nil {
			t.Errorf("%q: decoded without an error", raw)
		}
	}
}

// TestTrackerModel: the guided model is a tracker by id; a model the
// status lists with kind "tracker" is too; the default ("") and the
// per-frame segmenters never are.
func TestTrackerModel(t *testing.T) {
	st := jobs.MatteStatus{Models: map[string]jobs.MatteModelStatus{
		"sam2-small":                  {Kind: matte.KindTracker},
		recipe.MatteModelISNetAnime:   {Kind: matte.KindSegmenter},
		recipe.MatteModelBiRefNetLite: {},
	}}
	for _, c := range []struct {
		model string
		want  bool
	}{
		{recipe.MatteModelSAM2Tiny, true}, {"sam2-small", true},
		{"", false}, {recipe.MatteModelISNetAnime, false}, {recipe.MatteModelBiRefNetLite, false}, {"unknown", false},
	} {
		if got := trackerModel(c.model, st); got != c.want {
			t.Errorf("trackerModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
	if trackerModel("sam2-small", jobs.MatteStatus{}) {
		t.Error("an unlisted model other than sam2-tiny is a tracker on an empty status")
	}
	// matteOpModel: the first matte op's model, "" for the default, errors
	// without one or with params that do not decode.
	guided := recipe.Op{Kind: recipe.OpMatte, Params: json.RawMessage(`{"model":"sam2-tiny","prompts":[{"frame":0,"box":[0,0,1,1]}]}`)}
	if m, err := matteOpModel([]recipe.Op{{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0,"end":1}`)}, guided}); err != nil || m != recipe.MatteModelSAM2Tiny {
		t.Errorf("guided: %q, %v", m, err)
	}
	for _, ops := range [][]recipe.Op{{{Kind: recipe.OpMatte}}, {{Kind: recipe.OpMatte, Params: json.RawMessage(`{}`)}}} {
		if m, err := matteOpModel(ops); err != nil || m != "" {
			t.Errorf("default model: %q, %v", m, err)
		}
	}
	if _, err := matteOpModel(nil); err == nil || !strings.Contains(err.Error(), "sam2-tiny") {
		t.Errorf("no matte op: %v", err)
	}
	if _, err := matteOpModel([]recipe.Op{{Kind: recipe.OpMorph, Params: json.RawMessage(`{"close":true}`)}}); err == nil {
		t.Error("no matte op among others: no error")
	}
	if _, err := matteOpModel([]recipe.Op{{Kind: recipe.OpMatte, Params: json.RawMessage(`{"model":3}`)}}); err == nil || !strings.Contains(err.Error(), "invalid params") {
		t.Errorf("bad params: %v", err)
	}
}

// TestMattePromptEndpoint: POST /api/matte/prompt validates everything the
// server can before the manager is asked — content type (415), the body
// and the source list (400), the frame index (400), a matte op with a
// tracker model (400 otherwise, naming sam2-tiny), the prompts (400 when
// missing, when the frame holds none, when they have no box, no positive
// point and no mask, or when a maskFrom is not "edge"), then the sources
// (404 unknown) — and the cross-site guard applies. A request that passes
// all of it reaches the manager, which without a sidecar cannot answer a
// mask; a mask prompt (Phase 5d) passes the same way in every shape.
func TestMattePromptEndpoint(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	guided := recipe.Op{Kind: recipe.OpMatte, Params: json.RawMessage(`{"model":"sam2-tiny"}`)}
	box := json.RawMessage(`[{"frame":2,"box":[0.1,0.1,0.8,0.9]}]`)
	base := func() map[string]any {
		return map[string]any{"src": h, "ops": []recipe.Op{guided}, "output": recipe.Output{Format: "gif"}, "frame": 2, "prompts": box}
	}
	post := func(t *testing.T, body any) (int, string, []byte) {
		t.Helper()
		resp, data := e.postJSON(t, "/api/matte/prompt", body)
		msg := ""
		if resp.StatusCode != http.StatusOK && len(data) > 0 && data[0] == '{' {
			var m map[string]any
			if err := json.Unmarshal(data, &m); err == nil {
				msg, _ = m["error"].(string)
			}
		}
		return resp.StatusCode, msg, data
	}

	if status, body := e.send(t, http.MethodPost, "/api/matte/prompt", "text/plain", `{}`, nil); status != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d %s, want 415", status, body)
	}
	if status, msg, _ := post(t, `{"src":`); status != http.StatusBadRequest || !strings.Contains(msg, "invalid prompt request") {
		t.Errorf("bad JSON = %d %q", status, msg)
	}

	// Body validation, each with the rest of the request valid.
	for _, c := range []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"no source", func(m map[string]any) { delete(m, "src") }, "src"},
		{"bad source", func(m map[string]any) { m["src"] = "nope" }, "source hash"},
		{"negative frame", func(m map[string]any) { m["frame"] = -1 }, "frame must be"},
		{"no matte op", func(m map[string]any) {
			m["ops"] = []recipe.Op{{Kind: recipe.OpMorph, Params: json.RawMessage(`{"close":true}`)}}
		}, "matte op"},
		{"default model", func(m map[string]any) { m["ops"] = []recipe.Op{{Kind: recipe.OpMatte}} }, "sam2-tiny"},
		{"segmenter model", func(m map[string]any) {
			m["ops"] = []recipe.Op{{Kind: recipe.OpMatte, Params: json.RawMessage(`{"model":"isnet-anime"}`)}}
		}, `"isnet-anime" is not a tracker`},
		{"bad matte params", func(m map[string]any) {
			m["ops"] = []recipe.Op{{Kind: recipe.OpMatte, Params: json.RawMessage(`{"model":3}`)}}
		}, "invalid params"},
		{"no prompts", func(m map[string]any) { delete(m, "prompts") }, "prompts is required"},
		{"null prompts", func(m map[string]any) { m["prompts"] = nil }, "prompts is required"},
		{"prompts not an array", func(m map[string]any) { m["prompts"] = "box" }, "prompts must be"},
		{"malformed prompt", func(m map[string]any) { m["prompts"] = json.RawMessage(`[{"frame":"x"}]`) }, "invalid prompts"},
		{"no prompts on the frame", func(m map[string]any) { m["frame"] = 5 }, "no prompts on frame 5"},
		{"only a negative point", func(m map[string]any) {
			m["prompts"] = json.RawMessage(`[{"frame":2,"points":[[0.5,0.5,0]]}]`)
		}, "no box, no positive point and no mask"},
		{"box outside the frame", func(m map[string]any) {
			m["prompts"] = json.RawMessage(`[{"frame":2,"box":[0.1,0.1,1.8,0.9]}]`)
		}, "outside 0..1"},
		{"bad label", func(m map[string]any) {
			m["prompts"] = json.RawMessage(`[{"frame":2,"points":[[0.5,0.5,2]]}]`)
		}, "label"},
		// Phase 5d: a mask prompt's maskFrom is "edge" or nothing, in both shapes.
		{"unknown maskFrom", func(m map[string]any) {
			m["prompts"] = json.RawMessage(`[{"frame":2,"maskFrom":"png"}]`)
		}, "maskFrom"},
		{"unknown maskFrom in the object shape", func(m map[string]any) {
			m["prompts"] = json.RawMessage(`{"obj":1,"prompts":[{"frame":2,"maskFrom":"png","mask":true}]}`)
		}, "maskFrom"},
		{"mask prompt on another frame only", func(m map[string]any) {
			m["prompts"] = json.RawMessage(`[{"frame":0,"maskFrom":"edge"}]`)
		}, "no prompts on frame 2"},
	} {
		m := base()
		c.edit(m)
		status, msg, body := post(t, m)
		if status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", c.name, status, body)
			continue
		}
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: error %q lacks %q", c.name, msg, c.want)
		}
	}

	// A prompt on another frame is fine as long as the clicked one has its
	// own; the {obj, prompts} shape is accepted too. The sources come after
	// the body: an unknown one is a 404 like a still's.
	m := base()
	m["src"] = strings.Repeat("1", 64)
	m["prompts"] = json.RawMessage(`{"obj":1,"prompts":[{"frame":0,"points":[[0.5,0.5,0]]},{"frame":2,"box":[0.1,0.1,0.8,0.9]}]}`)
	if status, msg, body := post(t, m); status != http.StatusNotFound || !strings.Contains(msg, "not uploaded") {
		t.Errorf("unknown source = %d %q %s, want 404 not uploaded", status, msg, body)
	}
	if status, body := e.send(t, http.MethodPost, "/api/matte/prompt", "application/json", `{}`,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusForbidden {
		t.Errorf("cross-site = %d %s, want 403", status, body)
	}

	// Everything valid: the manager is asked. Without a sidecar no mask can
	// come back — 503 naming the compose profile once jobs.MattePromptMask
	// is implemented; until then its stub answers an error the server maps
	// to 500, which this test only tolerates when the body names the stub.
	// A mask prompt (Phase 5d: the recipe's {frame, maskFrom: "edge"} posted
	// verbatim, or with the client flag "mask": true, in either shape) needs
	// no box or point and passes the server's checks the same way.
	reaches := func(t *testing.T, name string, m map[string]any) {
		t.Helper()
		status, msg, body := post(t, m)
		switch {
		case status == http.StatusServiceUnavailable:
			if !strings.Contains(msg, "--profile") {
				t.Errorf("%s: 503 error %q does not name the compose profile", name, msg)
			}
		case status == http.StatusInternalServerError && strings.Contains(msg, "not implemented"):
			t.Logf("%s: jobs.MattePromptMask is still the Phase 5c stub: %s", name, msg)
		default:
			t.Errorf("%s: valid request without a sidecar = %d %s, want 503", name, status, body)
		}
	}
	reaches(t, "box", base())
	for name, prompts := range map[string]string{
		"mask prompt, recipe shape":               `[{"frame":2,"maskFrom":"edge"}]`,
		"mask prompt, SPA wire shape":             `{"obj":1,"prompts":[{"frame":2,"maskFrom":"edge","mask":true}]}`,
		"mask prompt, client flag only":           `[{"frame":2,"mask":true}]`,
		"mask prompt refined by a negative click": `[{"frame":2,"maskFrom":"edge","points":[[0.5,0.5,0]]}]`,
	} {
		m := base()
		m["prompts"] = json.RawMessage(prompts)
		reaches(t, name, m)
	}
}

// TestMattePromptPendingAndMask: the manager's answers map through
// previewError like a still's — a loading tracker is the 202 pending body
// (state loading, the status object alongside), a non-tracker recipe the
// manager refuses (ErrInvalidRecipe) 400, no sidecar 503 — and a mask is
// written as image/png, private and cacheable.
func TestMattePromptPendingAndMask(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/matte/prompt", nil)
	e.s.previewError(rec, r, &jobs.ErrMattePending{State: jobs.MattePendingLoading, Device: "cuda"}, http.StatusNotFound, "prompt mask")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("loading tracker: %d %s, want 202", rec.Code, rec.Body)
	}
	var p pendingJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Pending != "matte" || p.State != jobs.MattePendingLoading || p.Device != "cuda" || p.Models == nil {
		t.Errorf("202 body = %+v", p)
	}
	rec = httptest.NewRecorder()
	e.s.previewError(rec, r, fmt.Errorf("%w: op 0 (matte): model isnet-anime is not a tracker", jobs.ErrInvalidRecipe), http.StatusNotFound, "prompt mask")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("manager's non-tracker refusal: %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.s.previewError(rec, r, jobs.ErrMatteUnavailable, http.StatusNotFound, "prompt mask")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(errorOf(t, rec.Body.Bytes()), "--profile") {
		t.Errorf("no sidecar: %d %s, want 503 naming the profile", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	e.s.previewError(rec, r, context.DeadlineExceeded, http.StatusNotFound, "prompt mask")
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(errorOf(t, rec.Body.Bytes()), "prompt mask") {
		t.Errorf("deadline: %d %s, want 504 naming the prompt mask", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	writePreview(rec, "image/png", []byte("\x89PNG\r\n\x1a\n"))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "private, max-age=3600" || rec.Body.Len() != 8 {
		t.Errorf("mask answer: %d %v %d bytes", rec.Code, rec.Header(), rec.Body.Len())
	}
}
