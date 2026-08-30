package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// hashOf is the blob hash the store would assign to data.
func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newEnvInOut builds an env whose job manager has the given input/output
// dirs ("" leaves a feature off), mirroring what cmd/ezlg wires from
// EZLG_INPUT / EZLG_OUTPUT.
func newEnvInOut(t *testing.T, inputDir, outputDir string) *env {
	t.Helper()
	return newEnvWithOptions(t, Config{}, nil, hostTools(),
		jobs.Options{Concurrency: 1, InputDir: inputDir, OutputDir: outputDir})
}

// fabricateResult commits a fake finished result and returns its recipe
// hash. files maps result file names to contents; the first name in primary
// is the FileKindOutput entry, everything else FileKindAlternative. The
// manifest's recipe points at a seeded source blob named sourceName so the
// default save / download naming has a stem to work from.
func fabricateResult(t *testing.T, e *env, sourceName, primary string, files map[string][]byte) string {
	t.Helper()
	blob, err := e.st.PutBlob(strings.NewReader("source bytes for "+sourceName), sourceName)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("d", 64)
	stage := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	res := jobs.Result{
		RecipeHash: hash,
		Recipe:     recipe.Recipe{Sources: []string{blob.Hash}, Output: recipe.Output{Format: "gif"}},
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		kind := jobs.FileKindAlternative
		if name == primary {
			kind = jobs.FileKindOutput
		}
		f := jobs.File{Name: name, URL: "/out/" + hash + "/" + name, Kind: kind}
		if name == primary {
			// The manifest lists the primary first (the contract batch/save
			// clients rely on).
			res.Files = append([]jobs.File{f}, res.Files...)
		} else {
			res.Files = append(res.Files, f)
		}
	}
	mb, _ := json.Marshal(res)
	if err := os.WriteFile(filepath.Join(stage, store.ManifestName), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CommitResult(hash, stage); err != nil {
		t.Fatal(err)
	}
	return hash
}

// TestInOutDisabled: with no configured dirs (the default), the three
// Phase 4 endpoints answer 503 with an error naming the env knob, and the
// capabilities flags are off (TestCapabilities pins that too).
func TestInOutDisabled(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	hash := fabricateResult(t, e, "clip.gif", "out.gif", map[string][]byte{"out.gif": []byte("GIF89a-bytes")})

	resp, body := e.get(t, "/api/input")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(errorOf(t, body), "EZLG_INPUT") {
		t.Errorf("GET /api/input = %d %s, want 503 naming EZLG_INPUT", resp.StatusCode, body)
	}
	resp, body = e.postJSON(t, "/api/sources/from-input", map[string]string{"name": "clip.gif"})
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(errorOf(t, body), "EZLG_INPUT") {
		t.Errorf("from-input = %d %s, want 503", resp.StatusCode, body)
	}
	resp, body = e.postJSON(t, "/api/results/"+hash+"/save", map[string]string{"file": "out.gif"})
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(errorOf(t, body), "EZLG_OUTPUT") {
		t.Errorf("save = %d %s, want 503 naming EZLG_OUTPUT", resp.StatusCode, body)
	}
	// The feature-off save never wrote anything anywhere, and the result is
	// still served normally.
	if resp, _ := e.get(t, "/out/"+hash+"/out.gif"); resp.StatusCode != 200 {
		t.Errorf("result file gone after refused save: %d", resp.StatusCode)
	}
}

// TestListInput: the listing is {"files": [...]} — decodable extensions
// only, flat, name-sorted, per request (a file added later shows up), and
// an empty dir is an empty array, never null.
func TestListInput(t *testing.T) {
	in := t.TempDir()
	writeIn := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(in, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeIn("b.gif", []byte("GIF89a-b"))
	writeIn("a.mov", []byte("moov"))
	writeIn("notes.txt", []byte("not decodable"))
	writeIn("empty.png", nil) // zero bytes: skipped
	if err := os.MkdirAll(filepath.Join(in, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIn(filepath.Join("sub", "nested.gif"), []byte("GIF89a-n")) // no recursion

	e := newEnvInOut(t, in, "")
	resp, body := e.get(t, "/api/input")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/input = %d %s", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q", cc)
	}
	var got struct {
		Files []jobs.InputFile `json:"files"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(got.Files) != 2 || got.Files[0].Name != "a.mov" || got.Files[1].Name != "b.gif" {
		t.Fatalf("files = %+v, want a.mov then b.gif", got.Files)
	}
	if got.Files[1].Size != 8 || got.Files[0].Size != 4 {
		t.Errorf("sizes = %+v", got.Files)
	}
	// mtime is a real RFC 3339 timestamp (raw check: time.Time decodes
	// almost anything, the wire format is what the SPA parses).
	var raw struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	mt, _ := raw.Files[0]["mtime"].(string)
	if _, err := time.Parse(time.RFC3339, mt); err != nil {
		t.Errorf("mtime %q is not RFC 3339: %v", mt, err)
	}

	// Per-request: a new file shows up without a restart.
	writeIn("c.webm", []byte("webm"))
	_, body = e.get(t, "/api/input")
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 3 || got.Files[2].Name != "c.webm" {
		t.Errorf("after adding c.webm: %+v", got.Files)
	}

	// Empty dir: an empty array, not null.
	e2 := newEnvInOut(t, t.TempDir(), "")
	_, body = e2.get(t, "/api/input")
	if !strings.Contains(string(body), `"files":[]`) {
		t.Errorf("empty dir listing = %s, want \"files\":[]", body)
	}
}

// TestSourceFromInput: picking a file ingests it like an upload — the same
// bytes dedupe onto the same hash — and refuses anything that is not
// exactly a listed name (traversal included) with 404.
func TestSourceFromInput(t *testing.T) {
	in := t.TempDir()
	data := tinyGIF(t)
	if err := os.WriteFile(filepath.Join(in, "clip.gif"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// A file one level up that "../secret.gif" would reach if traversal
	// were possible.
	if err := os.WriteFile(filepath.Join(filepath.Dir(in), "secret.gif"), []byte("GIF89a-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnvInOut(t, in, "")

	// Seed the same bytes as an already-probed upload: from-input must
	// dedupe onto that hash and answer without re-probing (which also makes
	// this test independent of ffprobe).
	uploaded := putProbedSource(t, e, "clip.gif", data)

	resp, body := e.postJSON(t, "/api/sources/from-input", map[string]string{"name": "clip.gif"})
	if resp.StatusCode != 200 {
		t.Fatalf("from-input = %d %s", resp.StatusCode, body)
	}
	var src recipe.Source
	if err := json.Unmarshal(body, &src); err != nil {
		t.Fatal(err)
	}
	if src.Hash != uploaded {
		t.Errorf("from-input hash %s, want the uploaded blob's %s (sha256 dedupe)", src.Hash, uploaded)
	}
	if src.Info.Width == 0 || src.Info.Height == 0 {
		t.Errorf("source info missing: %+v", src.Info)
	}

	// Refusals: unknown name, traversal, empty name.
	for _, name := range []string{"nope.gif", "../secret.gif", "..\\secret.gif", "sub/clip.gif", ".", ".."} {
		resp, body = e.postJSON(t, "/api/sources/from-input", map[string]string{"name": name})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("from-input %q = %d %s, want 404", name, resp.StatusCode, body)
		}
	}
	resp, body = e.postJSON(t, "/api/sources/from-input", map[string]string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty name = %d %s, want 400", resp.StatusCode, body)
	}
	// Nothing was ingested for a refused name.
	if _, err := e.st.GetBlob(hashOf([]byte("GIF89a-secret"))); err == nil {
		t.Error("traversal target was ingested")
	}

	// Same guards as the sibling JSON endpoints: content type + same origin.
	if status, body := e.send(t, "POST", "/api/sources/from-input", "text/plain", `{"name":"clip.gif"}`, nil); status != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type = %d %s, want 415", status, body)
	}
	if status, body := e.send(t, "POST", "/api/sources/from-input", "application/json", `{"name":"clip.gif"}`,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusForbidden {
		t.Errorf("cross-site = %d %s, want 403", status, body)
	}
}

// TestSourceFromInputTooLarge: a listed /input file over the upload size
// limit (jobs.Options.MaxUploadBytes, wired from the same EZLG_MAX_UPLOAD_MB
// value as Config.MaxUploadBytes) is refused with 413 and nothing is
// ingested — a pick must not admit what an upload of the same bytes would
// refuse. The listing itself keeps showing the file (the 413 is the
// contract, hiding listed files would only confuse), and a file under the
// cap still ingests normally.
func TestSourceFromInputTooLarge(t *testing.T) {
	in := t.TempDir()
	small := tinyGIF(t)
	limit := int64(len(small)) + 16
	big := bytes.Repeat([]byte("x"), int(limit)+1)
	copy(big, "GIF89a")
	if err := os.WriteFile(filepath.Join(in, "small.gif"), small, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "big.gif"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnvWithOptions(t, Config{MaxUploadBytes: limit}, nil, hostTools(),
		jobs.Options{Concurrency: 1, InputDir: in, MaxUploadBytes: limit})
	// Seed small.gif as an already-probed upload so its pick answers without
	// ffprobe (same trick as TestSourceFromInput).
	putProbedSource(t, e, "small.gif", small)

	// The oversized file is still listed — refusal happens on pick, not list.
	resp, body := e.get(t, "/api/input")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"big.gif"`) {
		t.Fatalf("GET /api/input = %d %s, want 200 listing big.gif", resp.StatusCode, body)
	}

	resp, body = e.postJSON(t, "/api/sources/from-input", map[string]string{"name": "big.gif"})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("from-input big.gif = %d %s, want 413", resp.StatusCode, body)
	}
	if msg := errorOf(t, body); !strings.Contains(msg, "limit") {
		t.Errorf("413 error %q should name the limit", msg)
	}
	// The refused pick ingested nothing.
	if _, err := e.st.GetBlob(hashOf(big)); err == nil {
		t.Error("over-limit file was ingested into the blob store")
	}

	// A file under the cap ingests as before.
	resp, body = e.postJSON(t, "/api/sources/from-input", map[string]string{"name": "small.gif"})
	if resp.StatusCode != 200 {
		t.Errorf("from-input small.gif = %d %s, want 200", resp.StatusCode, body)
	}
}

// TestSourceFromInputProbesFreshFile: a picked file that was never uploaded
// goes through the full upload tail — probe, info stored, source answered.
func TestSourceFromInputProbesFreshFile(t *testing.T) {
	tools := hostTools()
	if tools.FFprobe == "" || tools.FFmpeg == "" {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	in := t.TempDir()
	if err := os.WriteFile(filepath.Join(in, "fresh.gif"), tinyGIF(t), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnvInOut(t, in, "")
	resp, body := e.postJSON(t, "/api/sources/from-input", map[string]string{"name": "fresh.gif"})
	if resp.StatusCode != 200 {
		t.Fatalf("from-input = %d %s", resp.StatusCode, body)
	}
	var src recipe.Source
	if err := json.Unmarshal(body, &src); err != nil {
		t.Fatal(err)
	}
	if src.Name != "fresh.gif" || src.Info.Width != 16 || src.Info.Height != 12 || src.Info.Frames != 6 {
		t.Errorf("source = %+v info %+v", src, src.Info)
	}
	// The info was persisted like an upload's.
	blob, err := e.st.GetBlob(src.Hash)
	if err != nil || blob.Info == nil {
		t.Errorf("ingested blob not probed/stored: %+v, %v", blob, err)
	}
}

// TestSaveResult: the save endpoint writes manifest-listed files into the
// output dir byte-identically, derives the ?dl=1 name by default, keeps a
// caller-given base, suffixes -2 on collision, and 404s everything else.
func TestSaveResult(t *testing.T) {
	out := t.TempDir()
	e := newEnvInOut(t, "", out)
	gifBytes := []byte("GIF89a-primary")
	mp4Bytes := []byte("mp4-alt-bytes")
	hash := fabricateResult(t, e, "myclip.gif", "out.gif", map[string][]byte{
		"out.gif": gifBytes,
		"alt.mp4": mp4Bytes,
	})

	save := func(file, name string) (int, []byte) {
		t.Helper()
		req := map[string]string{"file": file}
		if name != "" {
			req["name"] = name
		}
		resp, body := e.postJSON(t, "/api/results/"+hash+"/save", req)
		return resp.StatusCode, body
	}
	savedName := func(body []byte) string {
		t.Helper()
		var m map[string]string
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("save response %q: %v", body, err)
		}
		return m["name"]
	}

	// Default base = the ?dl=1 download name (primary → source stem).
	status, body := save("out.gif", "")
	if status != 200 {
		t.Fatalf("save = %d %s", status, body)
	}
	if got := savedName(body); got != "myclip.gif" {
		t.Errorf("saved name = %q, want myclip.gif", got)
	}
	if data, err := os.ReadFile(filepath.Join(out, "myclip.gif")); err != nil || !bytes.Equal(data, gifBytes) {
		t.Errorf("saved bytes differ: %q %v", data, err)
	}
	// Second identical save: a different, collision-safe name.
	status, body = save("out.gif", "")
	if status != 200 {
		t.Fatalf("second save = %d %s", status, body)
	}
	if got := savedName(body); got != "myclip-2.gif" {
		t.Errorf("second saved name = %q, want myclip-2.gif", got)
	}
	if data, err := os.ReadFile(filepath.Join(out, "myclip-2.gif")); err != nil || !bytes.Equal(data, gifBytes) {
		t.Errorf("second saved bytes differ: %q %v", data, err)
	}
	// A caller-given base (extension optional) is kept; non-primary files
	// save fine too.
	status, body = save("alt.mp4", "for-discord")
	if status != 200 || savedName(body) != "for-discord.mp4" {
		t.Errorf("named save = %d %s", status, body)
	}
	if data, err := os.ReadFile(filepath.Join(out, "for-discord.mp4")); err != nil || !bytes.Equal(data, mp4Bytes) {
		t.Errorf("named saved bytes differ: %q %v", data, err)
	}

	// 404s: files the manifest does not list (sidecars and traversal
	// included), an unknown result, a non-hash.
	for _, file := range []string{"missing.gif", "manifest.json", "report.json", "../escape.gif", "..\\escape.gif"} {
		if status, body := save(file, ""); status != http.StatusNotFound {
			t.Errorf("save %q = %d %s, want 404", file, status, body)
		}
	}
	resp, body2 := e.postJSON(t, "/api/results/"+strings.Repeat("a", 64)+"/save", map[string]string{"file": "out.gif"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown result = %d %s, want 404", resp.StatusCode, body2)
	}
	resp, body2 = e.postJSON(t, "/api/results/nothash/save", map[string]string{"file": "out.gif"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-hash = %d %s, want 404", resp.StatusCode, body2)
	}
	// 400 for a missing file field; the sibling guards hold here too.
	if status, body := save("", ""); status != http.StatusBadRequest {
		t.Errorf("empty file = %d %s, want 400", status, body)
	}
	if status, body := e.send(t, "POST", "/api/results/"+hash+"/save", "text/plain", `{"file":"out.gif"}`, nil); status != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type = %d %s, want 415", status, body)
	}
	if status, body := e.send(t, "POST", "/api/results/"+hash+"/save", "application/json", `{"file":"out.gif"}`,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusForbidden {
		t.Errorf("cross-site = %d %s, want 403", status, body)
	}
	// Nothing but the three saves landed in /output.
	entries, _ := os.ReadDir(out)
	if len(entries) != 3 {
		names := make([]string, 0, len(entries))
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Errorf("output dir = %v, want exactly 3 files", names)
	}
}

// TestInOutLostAfterStartup: a feature directory that was usable at startup
// but disappears afterwards (unmounted NAS, deleted bind mount) answers the
// same operator-facing 503 as a feature that was never configured — not a
// 500 leaking container filesystem paths from the raw os error, which stays
// in the server log only. (Capabilities keep claiming the feature until a
// restart — documented; the 503 is the SPA's feature-off signal.)
func TestInOutLostAfterStartup(t *testing.T) {
	root := t.TempDir()
	in, out := filepath.Join(root, "in"), filepath.Join(root, "out")
	for _, d := range []string{in, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(in, "clip.gif"), []byte("GIF89a-clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnvInOut(t, in, out)
	hash := fabricateResult(t, e, "myclip.gif", "out.gif", map[string][]byte{"out.gif": []byte("GIF89a-bytes")})

	// Sanity: both features passed the startup check and the listing works.
	if resp, body := e.get(t, "/api/input"); resp.StatusCode != 200 || !strings.Contains(string(body), `"clip.gif"`) {
		t.Fatalf("GET /api/input before removal = %d %s", resp.StatusCode, body)
	}

	// The mounts vanish after the startup check.
	for _, d := range []string{in, out} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}

	check503 := func(what string, resp *http.Response, body []byte, knob string) {
		t.Helper()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s = %d %s, want 503", what, resp.StatusCode, body)
			return
		}
		msg := errorOf(t, body)
		if !strings.Contains(msg, knob) {
			t.Errorf("%s error %q should name %s", what, msg, knob)
		}
		// The fixed operator message, never the raw os error with a server
		// path ("open /input: no such file or directory" and friends).
		for _, leak := range []string{root, "no such file", "cannot find", "open "} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s error %q leaks %q", what, msg, leak)
			}
		}
	}

	resp, body := e.get(t, "/api/input")
	check503("GET /api/input", resp, body, "EZLG_INPUT")
	resp, body = e.postJSON(t, "/api/sources/from-input", map[string]string{"name": "clip.gif"})
	check503("from-input", resp, body, "EZLG_INPUT")
	resp, body = e.postJSON(t, "/api/results/"+hash+"/save", map[string]string{"file": "out.gif"})
	check503("save", resp, body, "EZLG_OUTPUT")
	// The result itself is untouched — only the save target is gone.
	if resp, _ := e.get(t, "/out/"+hash+"/out.gif"); resp.StatusCode != 200 {
		t.Errorf("result file gone after refused save: %d", resp.StatusCode)
	}
}

// TestVideoResultServing: mp4/webm result files get their pinned
// Content-Type and keep the ?dl=1 friendly download name.
func TestVideoResultServing(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	hash := fabricateResult(t, e, "myclip.mov", "out.mp4", map[string][]byte{
		"out.mp4":  []byte("mp4-bytes"),
		"alt.webm": []byte("webm-bytes"),
	})
	resp, body := e.get(t, "/out/"+hash+"/out.mp4")
	if resp.StatusCode != 200 || string(body) != "mp4-bytes" {
		t.Fatalf("out.mp4 = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("mp4 content-type = %q", ct)
	}
	resp, _ = e.get(t, "/out/"+hash+"/out.mp4?dl=1")
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename=myclip.mp4` {
		t.Errorf("mp4 dl=1 content-disposition = %q", cd)
	}
	resp, _ = e.get(t, "/out/"+hash+"/alt.webm")
	if ct := resp.Header.Get("Content-Type"); ct != "video/webm" {
		t.Errorf("webm content-type = %q", ct)
	}
	resp, _ = e.get(t, "/out/"+hash+"/alt.webm?dl=1")
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename=myclip-alt.webm` {
		t.Errorf("webm dl=1 content-disposition = %q", cd)
	}
}

// TestCapabilitiesInOut: with usable dirs and a gifski that answers its
// version probe (the fake tool) the Phase 4 feature flags flip on.
func TestCapabilitiesInOut(t *testing.T) {
	exe, _ := fakeToolExe(t, "")
	tools := hostTools()
	tools.Gifski = exe
	e := newEnvWithOptions(t, Config{}, nil, tools,
		jobs.Options{Concurrency: 1, InputDir: t.TempDir(), OutputDir: t.TempDir()})
	resp, body := e.get(t, "/api/capabilities")
	if resp.StatusCode != 200 {
		t.Fatalf("capabilities = %d %s", resp.StatusCode, body)
	}
	var caps struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"inputPick", "outputSave", "gifski"} {
		if !caps.Features[f] {
			t.Errorf("features[%q] = false, want true (%v)", f, caps.Features)
		}
	}
}

// TestGifskiFeatureProbed: features.gifski reflects the startup version
// probe, not the bare resolved path. ffrun.LookupTools uses an EZLG_GIFSKI
// override verbatim (no existence check), so a typo'd path used to
// advertise the HQ encoder while every gifski render failed at exec time;
// now the failed probe hides the feature.
func TestGifskiFeatureProbed(t *testing.T) {
	tools := hostTools()
	tools.Gifski = filepath.Join(t.TempDir(), "gifsky") // typo'd path: nothing there
	e := newEnvWithOptions(t, Config{}, nil, tools, jobs.Options{Concurrency: 1})
	resp, body := e.get(t, "/api/capabilities")
	if resp.StatusCode != 200 {
		t.Fatalf("capabilities = %d %s", resp.StatusCode, body)
	}
	var caps struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Features["gifski"] {
		t.Errorf("features[gifski] = true for an unrunnable binary %q (%v)", tools.Gifski, caps.Features)
	}
}
