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
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Phase 5b HTTP surface (docs/background-removal-proposal.md §6.2, §9):
// GET /api/matte, the features.matte flag, the 202 "pending" answer of
// stills and proxies, previewRequest.eager and the 400 for a matte op while
// the sidecar is off. The manager here never has a sidecar (Options.MatteURL
// ""), which is what a plain install looks like; the pending path is
// exercised through previewError with the errors jobs documents, since the
// server holds a concrete *jobs.Manager and a real pass needs the sidecar.

// matteStatusJSON is the GET /api/matte shape as a client decodes it; the
// pointer fields tell "absent / null" from "empty".
type matteStatusJSON struct {
	Enabled      bool                        `json:"enabled"`
	Device       string                      `json:"device"`
	Reason       string                      `json:"reason"`
	GPU          json.RawMessage             `json:"gpu"`
	DefaultModel string                      `json:"defaultModel"`
	Models       *map[string]json.RawMessage `json:"models"`
	MaxSeconds   int                         `json:"maxSeconds"`
	MaxFrames    int                         `json:"maxFrames"`
}

// TestMatteEndpoint: GET /api/matte answers 200 on every install with the
// manager's status — enabled false and a reason naming EZLG_MATTE_URL when
// no sidecar is configured, models always an object, the caps as applied
// (the defaults, or the configured values) — never cached, read-only, and
// features.matte is the same flag.
func TestMatteEndpoint(t *testing.T) {
	e := newEnvWithOptions(t, Config{}, nil, hostTools(), jobs.Options{Concurrency: 1, MatteMaxSeconds: 42, MatteMaxFrames: 7})
	resp, body := e.get(t, "/api/matte")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache (the SPA polls it)", cc)
	}
	var st matteStatusJSON
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if st.Enabled || st.Enabled != e.jm.MatteEnabled() {
		t.Errorf("enabled = %v, want false without EZLG_MATTE_URL (manager says %v)", st.Enabled, e.jm.MatteEnabled())
	}
	if !strings.Contains(st.Reason, "EZLG_MATTE_URL") {
		t.Errorf("reason = %q, want it to name EZLG_MATTE_URL", st.Reason)
	}
	if st.Models == nil {
		t.Fatalf("models is null or missing, want an object: %s", body)
	}
	if len(*st.Models) != 0 {
		t.Errorf("models = %v, want none without a sidecar", *st.Models)
	}
	if st.DefaultModel != recipe.MatteModelDefault {
		t.Errorf("defaultModel = %q, want %q", st.DefaultModel, recipe.MatteModelDefault)
	}
	if st.MaxSeconds != 42 || st.MaxFrames != 7 {
		t.Errorf("maxSeconds/maxFrames = %d/%d, want the configured 42/7", st.MaxSeconds, st.MaxFrames)
	}
	if len(st.GPU) != 0 && string(st.GPU) != "null" {
		t.Errorf("gpu = %s without a sidecar, want absent", st.GPU)
	}
	if flag := capsFeatures(t, e)["matte"]; flag != st.Enabled {
		t.Errorf("features.matte = %v, /api/matte enabled = %v: one flag, two answers", flag, st.Enabled)
	}
	// Read-only: a POST falls through to the /api catch-all (a JSON 404);
	// cross-site GETs are not guarded.
	if status, body := e.send(t, "POST", "/api/matte", "application/json", "{}", nil); status != http.StatusNotFound || errorOf(t, body) == "" {
		t.Errorf("POST /api/matte = %d %s, want a JSON 404", status, body)
	}
	if status, _ := e.send(t, "GET", "/api/matte", "", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusOK {
		t.Errorf("cross-site GET /api/matte = %d, want 200", status)
	}

	// The default caps on a default manager.
	e2 := newEnv(t, Config{}, nil)
	_, body = e2.get(t, "/api/matte")
	st = matteStatusJSON{}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode defaults: %v: %s", err, body)
	}
	if st.MaxSeconds != jobs.DefaultMatteMaxSeconds || st.MaxFrames != jobs.DefaultMatteMaxFrames {
		t.Errorf("default caps = %d/%d, want %d/%d", st.MaxSeconds, st.MaxFrames, jobs.DefaultMatteMaxSeconds, jobs.DefaultMatteMaxFrames)
	}

	// No manager: still 200, disabled, models an object.
	root := t.TempDir()
	stNoJM, err := store.New(filepath.Join(root, "data"), filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Version: "nojm"}, stNoJM, nil, hostTools(), nil)
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/matte", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("no manager: status %d: %s", rec.Code, rec.Body)
	}
	st = matteStatusJSON{Enabled: true}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("no manager: decode: %v: %s", err, rec.Body)
	}
	if st.Enabled || st.Models == nil || st.Reason == "" || st.DefaultModel != recipe.MatteModelDefault {
		t.Errorf("no manager: %+v (%s), want enabled false, a reason, models {} and the default model", st, rec.Body)
	}
	if s.features(false, nil)["matte"] {
		t.Error("no manager: features.matte = true")
	}
}

// pendingJSON is the 202 body as a client decodes it: the pending fields
// plus the /api/matte object at the same level.
type pendingJSON struct {
	Pending       string `json:"pending"`
	State         string `json:"state"`
	Done          int    `json:"done"`
	Total         int    `json:"total"`
	Percent       int    `json:"percent"`
	EstimateMS    int64  `json:"estimateMs"`
	PendingReason string `json:"pendingReason"`
	matteStatusJSON
}

// TestPreviewMattePending: a jobs.ErrMattePending from a still or proxy —
// bare or wrapped — is a 202 Accepted whose body carries the pending state
// and figures together with the live /api/matte object (the pass's device
// winning over the status's), never cached; jobs.ErrMatteUnavailable is a
// 503 naming the compose profile, even when wrapped with ErrInvalidRecipe;
// and the pre-existing mappings (404/400 for a missing source, 400 for a
// refused recipe, 504 past the deadline, 500 otherwise) are untouched.
func TestPreviewMattePending(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	call := func(t *testing.T, err error, missingStatus int) (*httptest.ResponseRecorder, []byte) {
		t.Helper()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/still", nil)
		e.s.previewError(rec, r, err, missingStatus, "still render")
		return rec, rec.Body.Bytes()
	}
	decodePending := func(t *testing.T, body []byte) pendingJSON {
		t.Helper()
		var p pendingJSON
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("decode 202 body: %v: %s", err, body)
		}
		return p
	}

	t.Run("running", func(t *testing.T) {
		pending := &jobs.ErrMattePending{State: jobs.MattePendingRunning, Done: 24, Total: 45, Percent: 53, Device: "cuda", EstimateMS: 810}
		rec, body := call(t, pending, http.StatusNotFound)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status %d %s, want 202", rec.Code, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content-type = %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("cache-control = %q, want no-store", cc)
		}
		p := decodePending(t, body)
		if p.Pending != "matte" || p.State != "running" || p.Done != 24 || p.Total != 45 || p.Percent != 53 || p.EstimateMS != 810 {
			t.Errorf("pending fields = %+v, want matte/running 24/45 53 %% 810 ms", p)
		}
		if p.Device != "cuda" {
			t.Errorf("device = %q, want the pass's \"cuda\" over the status's", p.Device)
		}
		// The status object rides along at the top level.
		if p.Models == nil || p.MaxSeconds != jobs.DefaultMatteMaxSeconds || p.MaxFrames != jobs.DefaultMatteMaxFrames || p.DefaultModel != recipe.MatteModelDefault {
			t.Errorf("status fields = %+v (%s), want models {}, the caps and the default model", p.matteStatusJSON, body)
		}
		if p.Enabled != e.jm.MatteEnabled() {
			t.Errorf("enabled = %v, want the manager's %v", p.Enabled, e.jm.MatteEnabled())
		}
		for _, key := range []string{`"pending":"matte"`, `"state":"running"`, `"done":24`, `"total":45`, `"percent":53`, `"estimateMs":810`, `"enabled":`, `"models":{}`, `"maxSeconds":`, `"maxFrames":`, `"defaultModel":`} {
			if !strings.Contains(string(body), key) {
				t.Errorf("202 body lacks %s: %s", key, body)
			}
		}
		if strings.Contains(string(body), `"error"`) {
			t.Errorf("202 body carries an error field: %s", body)
		}
	})

	// Phase 5c: "idle" (nothing started — no eager mark, no pass in flight)
	// replaced the 5b "deferred" state; the server passes any state through.
	t.Run("wrapped and idle", func(t *testing.T) {
		pending := &jobs.ErrMattePending{State: jobs.MattePendingIdle, Total: 1800, Device: "cpu", EstimateMS: 180_000}
		rec, body := call(t, fmt.Errorf("compile: %w", pending), http.StatusBadRequest)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("wrapped pending: status %d %s, want 202", rec.Code, body)
		}
		p := decodePending(t, body)
		if p.State != "idle" || p.Done != 0 || p.Total != 1800 || p.Percent != 0 || p.EstimateMS != 180_000 || p.Device != "cpu" {
			t.Errorf("idle fields = %+v", p)
		}
		if !strings.Contains(string(body), `"state":"idle"`) {
			t.Errorf("202 body lacks the idle state: %s", body)
		}
		// The status's own reason is always the status's (this install's:
		// no EZLG_MATTE_URL), and an error without one sends no
		// pendingReason …
		want := e.jm.MatteStatus().Reason
		if p.Reason != want || p.PendingReason != "" {
			t.Errorf("reason = %q / pendingReason = %q, want the status's %q and none", p.Reason, p.PendingReason, want)
		}
		if strings.Contains(string(body), `"pendingReason"`) {
			t.Errorf("202 body carries pendingReason for an error without one: %s", body)
		}
		// … and the error's rides as pendingReason, its OWN field (Phase
		// 5d: a mask prompt whose edge matte is not computed — the SPA
		// shows it under the panel): never in the status's reason, which
		// the SPA installs wholesale as the live /api/matte object.
		const reason = "compute the General matte first (the mask prompt is its matte of this frame)"
		rec, body = call(t, &jobs.ErrMattePending{State: jobs.MattePendingIdle, Device: "cpu", Reason: reason}, http.StatusNotFound)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("idle with a reason: status %d %s, want 202", rec.Code, body)
		}
		if p = decodePending(t, body); p.State != "idle" || p.Device != "cpu" || p.PendingReason != reason || p.Reason != want {
			t.Errorf("idle with a reason: state %q device %q pendingReason %q reason %q, want idle / cpu / %q / the status's %q", p.State, p.Device, p.PendingReason, p.Reason, reason, want)
		}
		if !strings.Contains(string(body), `"pendingReason":"compute the General matte first`) {
			t.Errorf("202 body lacks the error's pendingReason: %s", body)
		}
	})

	t.Run("loading and downloading keep the status device", func(t *testing.T) {
		for _, state := range []string{jobs.MattePendingLoading, jobs.MattePendingDownloading} {
			rec, body := call(t, &jobs.ErrMattePending{State: state, Percent: 43}, http.StatusNotFound)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("%s: status %d %s, want 202", state, rec.Code, body)
			}
			p := decodePending(t, body)
			if p.State != state || p.Percent != 43 {
				t.Errorf("%s: fields = %+v", state, p)
			}
			if p.Device != e.jm.MatteStatus().Device {
				t.Errorf("%s: device = %q, want the status's %q when the error has none", state, p.Device, e.jm.MatteStatus().Device)
			}
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		for _, err := range []error{
			jobs.ErrMatteUnavailable,
			fmt.Errorf("%w: %w: CUDA EP not available", jobs.ErrInvalidRecipe, jobs.ErrMatteUnavailable),
		} {
			rec, body := call(t, err, http.StatusNotFound)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%v: status %d %s, want 503", err, rec.Code, body)
				continue
			}
			msg := errorOf(t, body)
			if !strings.Contains(msg, "docker compose --profile matte-gpu up -d") || !strings.Contains(msg, "matte service") {
				t.Errorf("%v: error %q, want the manager's message and the compose profile", err, msg)
			}
		}
	})

	t.Run("existing mappings", func(t *testing.T) {
		for _, c := range []struct {
			err     error
			missing int
			want    int
		}{
			{fmt.Errorf("%w: source abc", store.ErrNotFound), http.StatusNotFound, http.StatusNotFound},
			{fmt.Errorf("%w: source abc", store.ErrNotFound), http.StatusBadRequest, http.StatusBadRequest},
			{fmt.Errorf("%w: op 0 (bogus): unknown op", jobs.ErrInvalidRecipe), http.StatusNotFound, http.StatusBadRequest},
			{context.DeadlineExceeded, http.StatusNotFound, http.StatusGatewayTimeout},
			{errors.New("ffmpeg exited 1"), http.StatusNotFound, http.StatusInternalServerError},
		} {
			rec, body := call(t, c.err, c.missing)
			if rec.Code != c.want {
				t.Errorf("%v (missing %d): status %d %s, want %d", c.err, c.missing, rec.Code, body, c.want)
			}
			if errorOf(t, body) == "" {
				t.Errorf("%v: empty error message", c.err)
			}
		}
		// A client that hung up gets nothing, pending or not.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/still", nil).WithContext(ctx)
		e.s.previewError(rec, r, &jobs.ErrMattePending{State: jobs.MattePendingRunning}, http.StatusNotFound, "still render")
		if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
			t.Errorf("hung-up client got %d %s", rec.Code, rec.Body)
		}
	})
}

// TestCreateJobMatteOff: with no sidecar configured (features.matte false)
// a recipe with a matte op is a 400 that carries the reason and names the
// compose profile — before the sources are checked, like the target check
// — and leaves no result; the same recipe without the op is accepted, so
// the guard is exactly the op kind.
func TestCreateJobMatteOff(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.jm.MatteEnabled() {
		t.Fatal("precondition: the manager must have no sidecar")
	}
	h := putProbedSource(t, e, "a.gif", tinyGIF(t))
	matte := recipe.Op{Kind: recipe.OpMatte, Params: json.RawMessage(`{"model":"isnet-anime"}`)}
	for _, ops := range [][]recipe.Op{
		{matte},
		{{Kind: recipe.OpMatte}},
		{{Kind: recipe.OpTrim, Params: json.RawMessage(`{"start":0,"end":0.3}`)}, matte, {Kind: recipe.OpMorph, Params: json.RawMessage(`{"close":true}`)}},
	} {
		rec := recipe.Recipe{Sources: []string{h}, Ops: ops, Output: recipe.Output{Format: "gif", Width: 8, Height: 8, Target: "emote"}}
		resp, body := e.postJSON(t, "/api/jobs", rec)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("ops %v: %d %s, want 400", ops, resp.StatusCode, body)
			continue
		}
		msg := errorOf(t, body)
		for _, want := range []string{"docker compose --profile matte-gpu up -d", "EZLG_MATTE_URL", "AI matte"} {
			if !strings.Contains(msg, want) {
				t.Errorf("ops %v: error %q lacks %q", ops, msg, want)
			}
		}
		if resp, _ := e.get(t, "/api/results/"+jobs.ResultKey(rec)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("ops %v: a result exists for a refused recipe (%d)", ops, resp.StatusCode)
		}
	}
	// Before the sources: an unknown source with a matte op is still the
	// matte refusal, not "not uploaded".
	rec := recipe.Recipe{Sources: []string{strings.Repeat("1", 64)}, Ops: []recipe.Op{matte}, Output: recipe.Output{Format: "gif"}}
	resp, body := e.postJSON(t, "/api/jobs", rec)
	if msg := errorOf(t, body); resp.StatusCode != http.StatusBadRequest || !strings.Contains(msg, "--profile matte-gpu") || strings.Contains(msg, "not uploaded") {
		t.Errorf("unknown source + matte op: %d %q, want the matte refusal first", resp.StatusCode, msg)
	}
	// After the target check: a bad target wins.
	rec.Output.Target = "nitro"
	resp, body = e.postJSON(t, "/api/jobs", rec)
	if msg := errorOf(t, body); resp.StatusCode != http.StatusBadRequest || !strings.Contains(msg, "unknown output.target") {
		t.Errorf("bad target + matte op: %d %q, want the target refusal first", resp.StatusCode, msg)
	}
	// The op kind is the whole guard: without it the recipe is queued.
	plain := recipe.Recipe{Sources: []string{h}, Ops: []recipe.Op{{Kind: recipe.OpMorph, Params: json.RawMessage(`{"close":true}`)}}, Output: recipe.Output{Format: "gif", Width: 8, Height: 8, Target: "emote"}}
	resp, body = e.postJSON(t, "/api/jobs", plain)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("recipe without a matte op: %d %s, want 202", resp.StatusCode, body)
	}
}

// TestPreviewRequestEager: "eager" decodes on the preview request (absent =
// false) and jobs.WithMatteEager / MatteEager carry it on a ctx the way the
// handlers pass it.
func TestPreviewRequestEager(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{`{"src":"a","eager":true}`, true},
		{`{"src":"a","eager":false}`, false},
		{`{"src":"a"}`, false},
	} {
		var req previewRequest
		if err := json.Unmarshal([]byte(c.body), &req); err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if req.Eager != c.want {
			t.Errorf("%s: eager = %v, want %v", c.body, req.Eager, c.want)
		}
		if got := jobs.MatteEager(jobs.WithMatteEager(context.Background(), req.Eager)); got != c.want {
			t.Errorf("%s: MatteEager(WithMatteEager) = %v, want %v", c.body, got, c.want)
		}
	}
	// An eager request is otherwise an ordinary one: the same validation.
	e := newEnv(t, Config{}, nil)
	resp, body := e.postJSON(t, "/api/still", map[string]any{"src": "nope", "eager": true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("eager still with a bad src: %d %s, want 400", resp.StatusCode, body)
	}
	resp, body = e.postJSON(t, "/api/proxy", map[string]any{"src": strings.Repeat("a", 64), "eager": true, "maxW": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("eager proxy with a negative maxW: %d %s, want 400", resp.StatusCode, body)
	}
}
