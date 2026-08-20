package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// fakeResultFile is one file of a fabricated result.
type fakeResultFile struct {
	name, format, kind string
	data               []byte
}

// commitFakeResult writes files plus a manifest listing them (and the given
// recipe) into a staging dir and commits it as the result for hash. Files
// with an empty format are written to disk but left out of the manifest.
func commitFakeResult(t *testing.T, e *env, hash string, rec recipe.Recipe, files []fakeResultFile) {
	t.Helper()
	stage := filepath.Join(t.TempDir(), "s")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	man := jobs.Result{RecipeHash: hash, Recipe: rec}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(stage, f.name), f.data, 0o644); err != nil {
			t.Fatal(err)
		}
		if f.format == "" {
			continue
		}
		man.Files = append(man.Files, jobs.File{Name: f.name, URL: "/out/" + hash + "/" + f.name, Format: f.format, Kind: f.kind, Bytes: int64(len(f.data))})
	}
	mb, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, store.ManifestName), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CommitResult(hash, stage); err != nil {
		t.Fatal(err)
	}
}

// TestSourceFromResult: a result file the manifest lists becomes a source of
// its own (copied into the blob store under its result name, probed); bad
// requests are 400/415, unknown results and files 404, archives refused.
func TestSourceFromResult(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	hash := strings.Repeat("d", 64)
	gifData := tinyGIF(t)
	commitFakeResult(t, e, hash, recipe.Recipe{Sources: []string{strings.Repeat("f", 64)}, Output: recipe.Output{Format: "gif"}}, []fakeResultFile{
		{name: "out.gif", format: "gif", kind: jobs.FileKindOutput, data: gifData},
		{name: "frames.zip", format: "zip", kind: jobs.FileKindArchive, data: []byte("PK\x03\x04zip")},
		{name: "gone.gif", format: "gif", kind: jobs.FileKindAlternative, data: gifData},
		{name: "report.json", data: []byte(`{"ok":true}`)}, // on disk, not in the manifest
	})
	if err := os.Remove(filepath.Join(e.st.ResultDir(hash), "gone.gif")); err != nil {
		t.Fatal(err)
	}
	// A file outside the result dir that traversal might reach.
	os.WriteFile(filepath.Join(e.st.Root, "results", "secret.gif"), gifData, 0o644)

	req := func(recipeHash, name string) string {
		b, _ := json.Marshal(map[string]string{"recipeHash": recipeHash, "name": name})
		return string(b)
	}
	t.Run("rejections", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			want int
			msg  string
		}{
			{"bad json", `{not json`, 400, "from-result"},
			{"empty", `{}`, 400, "recipeHash"},
			{"not a hash", req("nope", "out.gif"), 400, "recipeHash"},
			{"unknown result", req(strings.Repeat("a", 64), "out.gif"), 404, "no result"},
			{"unknown file", req(hash, "missing.gif"), 404, "no such file"},
			{"file not in manifest", req(hash, "report.json"), 404, "no such file"},
			{"manifest itself", req(hash, store.ManifestName), 404, "no such file"},
			{"listed but missing on disk", req(hash, "gone.gif"), 404, "missing"},
			{"archive", req(hash, "frames.zip"), 400, "archive"},
			{"traversal slash", req(hash, "../secret.gif"), 400, "name"},
			{"traversal backslash", req(hash, `..\secret.gif`), 400, "name"},
			{"traversal encoded", req(hash, "%2e%2e/secret.gif"), 400, "name"},
			{"absolute", req(hash, "/etc/passwd"), 400, "name"},
			{"dot file", req(hash, ".hidden"), 400, "name"},
			{"empty name", req(hash, ""), 400, "name"},
			{"nul", req(hash, "out.gif\x00"), 400, "name"},
		}
		for _, tc := range cases {
			resp, body := e.postJSON(t, "/api/sources/from-result", tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("%s: status %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
				continue
			}
			if msg := errorOf(t, body); !strings.Contains(msg, tc.msg) {
				t.Errorf("%s: error %q does not mention %q", tc.name, msg, tc.msg)
			}
		}
		// Wrong content type is refused before the body is looked at.
		if status, body := e.send(t, "POST", "/api/sources/from-result", "text/plain", req(hash, "out.gif"), nil); status != http.StatusUnsupportedMediaType {
			t.Errorf("text/plain = %d %s, want 415", status, body)
		}
		// Cross-site requests are refused like every other POST.
		if status, body := e.send(t, "POST", "/api/sources/from-result", "application/json", req(hash, "out.gif"), map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}); status != http.StatusForbidden {
			t.Errorf("cross-site = %d %s, want 403", status, body)
		}
		// None of that created a blob.
		if left := dirEntries(t, filepath.Join(e.st.Root, "blobs")); len(left) != 0 {
			t.Errorf("rejected from-result requests left blobs: %v", left)
		}
	})

	t.Run("happy path", func(t *testing.T) {
		if e.tools.FFprobe == "" {
			t.Skip("ffprobe not on PATH")
		}
		resp, body := e.postJSON(t, "/api/sources/from-result", req(hash, "out.gif"))
		if resp.StatusCode != 200 {
			t.Fatalf("from-result: %d %s", resp.StatusCode, body)
		}
		var src recipe.Source
		if err := json.Unmarshal(body, &src); err != nil {
			t.Fatal(err)
		}
		if src.Hash != sha256Hex(gifData) || src.Name != "out.gif" || src.Size != int64(len(gifData)) {
			t.Errorf("source = %+v", src)
		}
		if src.Info.Kind != recipe.KindAnimation || src.Info.Frames != 6 || src.Info.Width != 16 || src.Info.Height != 12 || src.Info.Codec != "gif" {
			t.Errorf("info = %+v, want the probed 6-frame 16x12 gif", src.Info)
		}
		blob, err := e.st.GetBlob(src.Hash)
		if err != nil || blob.Info == nil || blob.Ext != "gif" {
			t.Errorf("blob = %+v, %v (want stored and probed as .gif)", blob, err)
		}
		// The new source is a regular source from here on.
		gresp, gbody := e.get(t, "/api/sources/"+src.Hash)
		if gresp.StatusCode != 200 || !bytes.Equal(gbody, body) {
			t.Errorf("GET source = %d %s", gresp.StatusCode, gbody)
		}
		// Doing it again dedupes onto the same blob.
		resp2, body2 := e.postJSON(t, "/api/sources/from-result", req(hash, "out.gif"))
		if resp2.StatusCode != 200 || !bytes.Equal(body2, body) {
			t.Errorf("second from-result = %d %s, want the identical source", resp2.StatusCode, body2)
		}
		if left := dirEntries(t, filepath.Join(e.st.Root, storeTmpDir)); len(left) != 0 {
			t.Errorf("staging files left in tmp: %v", left)
		}
	})
}

// TestOutFileTypesAndDownloadNames: result files carry a pinned Content-Type
// (frames.zip is application/zip, .apng image/apng) and ?dl=1 names them
// after the source. Only the primary output takes the source name alone
// ("My Clip.apng"); frames, alternatives and sidecar files keep their own
// stem so sibling downloads never collide — "My Clip-f00001.png",
// "My Clip-alt1.webp", "My Clip-frames.zip", "My Clip-delays.json".
func TestOutFileTypesAndDownloadNames(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	srcHash := putProbedSource(t, e, "My Clip.gif", tinyGIF(t))
	hash := strings.Repeat("c", 64)
	zipData := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00fake-zip-bytes")
	pngData := tinyPNG(t)
	commitFakeResult(t, e, hash, recipe.Recipe{Sources: []string{srcHash}, Output: recipe.Output{Format: "frames"}}, []fakeResultFile{
		{name: "frames.zip", format: "zip", kind: jobs.FileKindArchive, data: zipData},
		{name: "f00001.png", format: "png", kind: jobs.FileKindFrame, data: pngData},
		{name: "f00002.png", format: "png", kind: jobs.FileKindFrame, data: pngData},
		{name: "out.apng", format: "apng", kind: jobs.FileKindOutput, data: []byte("\x89PNG\r\n\x1a\nfake")},
		{name: "legacy.gif", format: "gif", kind: "", data: tinyGIF(t)}, // pre-Kind manifest entry = primary
		{name: "alt1.webp", format: "webp", kind: jobs.FileKindAlternative, data: []byte("RIFF\x00\x00\x00\x00WEBPVP8X")},
		{name: "alt2.avif", format: "avif", kind: jobs.FileKindAlternative, data: []byte("\x00\x00\x00\x1cftypavif")},
		{name: "delays.json", data: []byte(`[100,100]`)},   // on disk, not in the manifest
		{name: "report.json", data: []byte(`{"ok":true}`)}, // on disk, not in the manifest
	})

	cases := []struct {
		name, contentType, download string
	}{
		{"frames.zip", "application/zip", "My Clip-frames.zip"},
		{"f00001.png", "image/png", "My Clip-f00001.png"},
		{"f00002.png", "image/png", "My Clip-f00002.png"},
		{"out.apng", "image/apng", "My Clip.apng"},
		{"legacy.gif", "image/gif", "My Clip.gif"},
		{"alt1.webp", "image/webp", "My Clip-alt1.webp"},
		{"alt2.avif", "image/avif", "My Clip-alt2.avif"},
		{"delays.json", "application/json; charset=utf-8", "My Clip-delays.json"},
		{"report.json", "application/json; charset=utf-8", "My Clip-report.json"},
	}
	for _, tc := range cases {
		resp, body := e.get(t, "/out/"+hash+"/"+tc.name)
		if resp.StatusCode != 200 {
			t.Errorf("%s: status %d %s", tc.name, resp.StatusCode, body)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != tc.contentType {
			t.Errorf("%s: content-type %q, want %q", tc.name, ct, tc.contentType)
		}
		if resp.Header.Get("Content-Disposition") != "" {
			t.Errorf("%s: attachment without dl=1", tc.name)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s: cache-control %q", tc.name, cc)
		}
		resp, body = e.get(t, "/out/"+hash+"/"+tc.name+"?dl=1")
		if resp.StatusCode != 200 {
			t.Errorf("%s?dl=1: status %d %s", tc.name, resp.StatusCode, body)
			continue
		}
		if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="`+tc.download+`"` {
			t.Errorf("%s?dl=1: content-disposition %q, want attachment named %q", tc.name, cd, tc.download)
		}
	}
	resp, body := e.get(t, "/out/"+hash+"/frames.zip")
	if !bytes.Equal(body, zipData) {
		t.Errorf("frames.zip body = %q", body)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
}

func TestResultContentType(t *testing.T) {
	for name, want := range map[string]string{
		"out.gif": "image/gif", "out.webp": "image/webp", "out.png": "image/png", "out.apng": "image/apng",
		"out.avif": "image/avif", "out.jpg": "image/jpeg", "out.jpeg": "image/jpeg", "frames.zip": "application/zip",
		"report.json": "application/json; charset=utf-8", "OUT.GIF": "image/gif", "out.bin": "", "noext": "",
	} {
		if got := resultContentType(name); got != want {
			t.Errorf("resultContentType(%q) = %q, want %q", name, got, want)
		}
	}
}
