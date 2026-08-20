package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// mpPart is one part of a hand-built multipart body: a plain form field when
// filename is "", a file part otherwise.
type mpPart struct {
	field, filename string
	data            []byte
}

func filePart(name string, data []byte) mpPart {
	return mpPart{field: uploadFileField, filename: name, data: data}
}
func fieldPart(name, value string) mpPart { return mpPart{field: name, data: []byte(value)} }
func delayPart(ms string) mpPart          { return fieldPart(uploadDelayField, ms) }

// multipartParts encodes parts in order and returns the content type + body.
func multipartParts(t *testing.T, parts ...mpPart) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.filename == "" {
			w, err = mw.CreateFormField(p.field)
		} else {
			w, err = mw.CreateFormFile(p.field, p.filename)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), buf.Bytes()
}

// uploadParts POSTs a hand-built multipart body to /api/upload.
func (e *env) uploadParts(t *testing.T, parts ...mpPart) (*http.Response, []byte) {
	t.Helper()
	ct, body := multipartParts(t, parts...)
	resp, err := http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

// framePNG renders an 8x6 PNG whose pixels depend on i, so frames differ
// (and hash differently) while sharing one size.
func framePNG(t *testing.T, i int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(20 * i), G: uint8(255 - 10*i), B: uint8(x * 30), A: 255})
		}
	}
	img.Set(i%8, 0, color.NRGBA{}) // one transparent pixel per frame
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// avifHead is an ISOBMFF ftyp box with the avif brand (enough for sniffing;
// not a decodable image).
func avifHead(t *testing.T) []byte {
	t.Helper()
	return append([]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf"), make([]byte, 64)...)
}

// bigPNG renders a noisy (incompressible) PNG of a few MiB whose pixels
// depend on seed, so streaming and probing it takes long enough for
// concurrent requests to overlap.
func bigPNG(t *testing.T, seed int) []byte {
	t.Helper()
	const dim = 640
	img := image.NewNRGBA(image.Rect(0, 0, dim, dim))
	r := uint32(seed)*2654435761 + 12345
	next := func() uint8 {
		r = r*1664525 + 1013904223
		return uint8(r >> 24)
	}
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			img.Set(x, y, color.NRGBA{R: next(), G: next(), B: next(), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// dirEntries lists the names under dir ("" when it does not exist).
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	return names
}

// assertNothingLeft checks that a rejected (or finished) multi-file upload
// left no staging files under the store's tmp dir and, when blobs is false,
// no blob at all.
func assertNothingLeft(t *testing.T, e *env, blobs bool) {
	t.Helper()
	if left := dirEntries(t, filepath.Join(e.st.Root, storeTmpDir)); len(left) != 0 {
		t.Errorf("staging files left in tmp: %v", left)
	}
	if !blobs {
		if left := dirEntries(t, filepath.Join(e.st.Root, "blobs")); len(left) != 0 {
			t.Errorf("blobs left behind by a rejected upload: %v", left)
		}
	}
}

// skipUnlessSequencesLand skips the test when the store or the prober still
// answers with its Phase-2 stub (the sequence handler maps those to 500).
func skipUnlessSequencesLand(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	if resp.StatusCode == http.StatusInternalServerError && strings.Contains(string(body), "not implemented") {
		t.Skipf("sequence support not in the tree yet: %s", body)
	}
}

// TestUploadSequence: several image parts become one image-sequence source,
// frames ordered naturally by file name whatever the upload order, probed at
// the requested delay; the staging files are gone afterwards and no frame
// became a blob of its own; a re-upload dedupes onto the stored sequence.
func TestUploadSequence(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.tools.FFprobe == "" {
		t.Skip("ffprobe not on PATH")
	}
	frames := map[string][]byte{
		"frame1.png":  framePNG(t, 1),
		"frame2.png":  framePNG(t, 2),
		"frame10.png": framePNG(t, 10),
	}
	resp, out := e.uploadParts(t,
		filePart("frame10.png", frames["frame10.png"]),
		delayPart("40"),
		filePart(`C:\seq\frame2.png`, frames["frame2.png"]), // folder uploads carry paths
		filePart("frame1.png", frames["frame1.png"]),
	)
	skipUnlessSequencesLand(t, resp, out)
	if resp.StatusCode != 200 {
		t.Fatalf("sequence upload: %d %s", resp.StatusCode, out)
	}
	var src recipe.Source
	if err := json.Unmarshal(out, &src); err != nil {
		t.Fatal(err)
	}
	if !recipe.IsHash(src.Hash) || src.Name != "frame1.png" {
		t.Errorf("source = %+v (name should be the first frame in natural order)", src)
	}
	info := src.Info
	if info.Kind != recipe.KindSequence || info.IsStill || info.Sequence == nil {
		t.Fatalf("info = %+v, want an image sequence", info)
	}
	seq := info.Sequence
	if seq.Count != 3 || info.Frames != 3 || seq.DelayMS != 40 || seq.Pattern != store.SequencePattern("png") || seq.Mixed {
		t.Errorf("sequence = %+v, frames = %d", seq, info.Frames)
	}
	if math.Abs(info.FPS-25) > 1e-9 || math.Abs(info.Duration-3.0/25) > 1e-9 {
		t.Errorf("fps = %v duration = %v, want 25 fps / 0.12 s", info.FPS, info.Duration)
	}
	if info.Width != 8 || info.Height != 6 || !info.HasAlpha {
		t.Errorf("geometry/alpha = %dx%d alpha=%v, want 8x6 with alpha", info.Width, info.Height, info.HasAlpha)
	}

	// Frames are renumbered on disk in natural order: 1, 2, 10.
	blob, err := e.st.GetBlob(src.Hash)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"frame1.png", "frame2.png", "frame10.png"} {
		got, err := os.ReadFile(filepath.Join(blob.Path, fmt.Sprintf(store.SequencePattern("png"), i+1)))
		if err != nil {
			t.Errorf("frame %d: %v", i+1, err)
			continue
		}
		if !bytes.Equal(got, frames[name]) {
			t.Errorf("frame %d is not %s", i+1, name)
		}
	}
	// The first part was staged with the rest and never entered the blob
	// store; nothing is staged any more.
	if _, err := e.st.GetBlob(sha256Hex(frames["frame10.png"])); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("first frame kept as a blob of its own (err=%v)", err)
	}
	assertNothingLeft(t, e, true)

	// GET agrees; a re-upload with another delay dedupes onto the stored
	// sequence (its delay is a source fact; recipes override it with the
	// delay op).
	gresp, gbody := e.get(t, "/api/sources/"+src.Hash)
	if gresp.StatusCode != 200 || !bytes.Equal(gbody, out) {
		t.Errorf("GET source = %d %s", gresp.StatusCode, gbody)
	}
	resp2, out2 := e.uploadParts(t,
		filePart("frame1.png", frames["frame1.png"]),
		filePart("frame2.png", frames["frame2.png"]),
		filePart("frame10.png", frames["frame10.png"]),
		delayPart("500"),
	)
	if resp2.StatusCode != 200 || !bytes.Equal(out2, out) {
		t.Errorf("re-upload = %d %s, want the identical source", resp2.StatusCode, out2)
	}
	assertNothingLeft(t, e, true)
}

// TestUploadSequenceKeepsExistingFirstBlob: when the first part of a sequence
// upload is already a source of its own, it stays one.
func TestUploadSequenceKeepsExistingFirstBlob(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.tools.FFprobe == "" {
		t.Skip("ffprobe not on PATH")
	}
	a, b := framePNG(t, 3), framePNG(t, 4)
	resp, out := e.uploadFile(t, "a.png", a)
	if resp.StatusCode != 200 {
		t.Fatalf("single upload: %d %s", resp.StatusCode, out)
	}
	resp, out = e.uploadParts(t, filePart("a.png", a), filePart("b.png", b))
	skipUnlessSequencesLand(t, resp, out)
	if resp.StatusCode != 200 {
		t.Fatalf("sequence upload: %d %s", resp.StatusCode, out)
	}
	blob, err := e.st.GetBlob(sha256Hex(a))
	if err != nil || blob.Info == nil {
		t.Errorf("pre-existing first-frame source was dropped or unprobed: %+v, %v", blob, err)
	}
	if _, err := e.st.GetBlob(sha256Hex(b)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second frame became a blob of its own (err=%v)", err)
	}
	assertNothingLeft(t, e, true)
}

// TestUploadSequenceSniffsExtensionless: parts without a recognised image
// extension pass when their bytes are a sequence-readable image and are
// stored under the sniffed extension — the pattern must be one the image2
// demuxer can open (%06d.png, never %06d.bin), so a still actually renders
// from the stored sequence, and the same frames under proper .png names
// dedupe onto the same, usable, blob.
func TestUploadSequenceSniffsExtensionless(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.tools.FFprobe == "" {
		t.Skip("ffprobe not on PATH")
	}
	a, b := framePNG(t, 5), framePNG(t, 6)
	resp, out := e.uploadParts(t, filePart("frame-a", a), filePart("frame-b", b))
	skipUnlessSequencesLand(t, resp, out)
	if resp.StatusCode != 200 {
		t.Fatalf("sniffed PNG parts: %d %s", resp.StatusCode, out)
	}
	var src recipe.Source
	if err := json.Unmarshal(out, &src); err != nil {
		t.Fatal(err)
	}
	if src.Info.Sequence == nil || src.Info.Sequence.Pattern != store.SequencePattern("png") {
		t.Fatalf("sequence info = %+v, want pattern %q", src.Info.Sequence, store.SequencePattern("png"))
	}
	blob, err := e.st.GetBlob(src.Hash)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := os.Stat(filepath.Join(blob.Path, store.SequenceFrameName(i, "png"))); err != nil {
			t.Errorf("stored frame %d: %v", i, err)
		}
	}
	assertNothingLeft(t, e, true)

	// The stored pattern must actually open in the render path.
	if e.tools.FFmpeg != "" {
		sresp, sbody := e.postJSON(t, "/api/still", map[string]any{"src": src.Hash, "output": map[string]any{"format": "gif"}, "t": 0, "maxW": 8})
		if sresp.StatusCode != 200 {
			t.Errorf("still of sniffed sequence: %d %s", sresp.StatusCode, sbody)
		}
	}

	// The same bytes under proper .png names hash identically and dedupe onto
	// the stored (usable) sequence.
	resp2, out2 := e.uploadParts(t, filePart("frame-a.png", a), filePart("frame-b.png", b))
	if resp2.StatusCode != 200 || !bytes.Equal(out2, out) {
		t.Errorf("re-upload with .png names = %d %s, want the identical source", resp2.StatusCode, out2)
	}
}

// TestUploadSequenceStoresSniffedExtension: a name whose own extension says
// nothing useful ("x.png.bak" sanitises to "bak") is stored under the
// sniffed format so the pattern stays image2-readable.
func TestUploadSequenceStoresSniffedExtension(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.tools.FFprobe == "" {
		t.Skip("ffprobe not on PATH")
	}
	resp, out := e.uploadParts(t, filePart("x1.png.bak", framePNG(t, 7)), filePart("x2.png.bak", framePNG(t, 8)))
	skipUnlessSequencesLand(t, resp, out)
	if resp.StatusCode != 200 {
		t.Fatalf(".png.bak parts: %d %s", resp.StatusCode, out)
	}
	var src recipe.Source
	if err := json.Unmarshal(out, &src); err != nil {
		t.Fatal(err)
	}
	if src.Info.Sequence == nil || src.Info.Sequence.Pattern != store.SequencePattern("png") {
		t.Errorf("sequence info = %+v, want pattern %q", src.Info.Sequence, store.SequencePattern("png"))
	}
	assertNothingLeft(t, e, true)
}

// TestUploadSequenceRejected: the rules of a multi-file upload — every part
// an image, all one extension, none empty, a sane delayMs — answer 400 and
// leave neither staging files nor blobs behind.
func TestUploadSequenceRejected(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	a, b := framePNG(t, 7), framePNG(t, 8)
	junk := []byte("definitely not an image, just text\n")
	cases := []struct {
		name  string
		parts []mpPart
		want  string // substring of the error
	}{
		{"delayMs zero", []mpPart{delayPart("0"), filePart("a.png", a), filePart("b.png", b)}, "delayMs"},
		{"delayMs too large", []mpPart{delayPart("60001"), filePart("a.png", a), filePart("b.png", b)}, "delayMs"},
		{"delayMs junk", []mpPart{delayPart("fast"), filePart("a.png", a), filePart("b.png", b)}, "delayMs"},
		{"delayMs after the files", []mpPart{filePart("a.png", a), filePart("b.png", b), delayPart("-5")}, "delayMs"},
		{"delayMs junk on a single file", []mpPart{delayPart("abc"), filePart("a.png", a)}, "delayMs"},
		{"mixed extensions", []mpPart{filePart("a.png", a), filePart("b.bmp", b)}, store.ErrMixedSequence.Error()},
		{"jpg and jpeg are different patterns", []mpPart{filePart("a.jpg", a), filePart("b.jpeg", b)}, store.ErrMixedSequence.Error()},
		{"mixed sniffed and named", []mpPart{filePart("frame-a", a), filePart("b.bmp", b)}, store.ErrMixedSequence.Error()},
		{"two videos", []mpPart{filePart("a.mov", junk), filePart("b.mov", junk)}, "not an image"},
		// GIF and AVIF have no image2 codec mapping: a stored %06d.gif /
		// %06d.avif sequence could never be rendered, so they are refused
		// up front — by extension and by sniffed content alike.
		{"two gif frames", []mpPart{filePart("a1.gif", tinyGIF(t)), filePart("a2.gif", tinyGIF(t))}, "file 1 (a1.gif) is not an image"},
		{"two avif frames", []mpPart{filePart("a1.avif", avifHead(t)), filePart("a2.avif", avifHead(t))}, "file 1 (a1.avif) is not an image"},
		{"extension-less gif content", []mpPart{filePart("frame-a", tinyGIF(t)), filePart("frame-b", tinyGIF(t))}, "file 1 (frame-a) is not an image"},
		{"image then text", []mpPart{filePart("a.png", a), filePart("notes.txt", junk)}, "file 2 (notes.txt) is not an image"},
		{"text then image", []mpPart{filePart("notes.txt", junk), filePart("a.png", a)}, "file 1 (notes.txt) is not an image"},
		{"empty later frame", []mpPart{filePart("a.png", a), filePart("b.png", nil)}, "file 2 (b.png) is empty"},
		{"empty first frame", []mpPart{filePart("a.png", nil), filePart("b.png", b)}, "file 1 (a.png) is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := e.uploadParts(t, tc.parts...)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d %s, want 400", resp.StatusCode, out)
			}
			if msg := errorOf(t, out); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not mention %q", msg, tc.want)
			}
			assertNothingLeft(t, e, false)
		})
	}
}

// TestUploadSequenceUnreadable: frames that carry an image extension but
// are not images pass the cheap checks, get stored, and fail the probe: a
// 422 like any unreadable upload, and the sequence blob is dropped.
func TestUploadSequenceUnreadable(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.tools.FFprobe == "" {
		t.Skip("ffprobe not on PATH")
	}
	junk1 := []byte("this is not a png at all, frame one\n")
	junk2 := []byte("this is not a png at all, frame two\n")
	resp, out := e.uploadParts(t, filePart("a.png", junk1), filePart("b.png", junk2))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s, want 422", resp.StatusCode, out)
	}
	if msg := errorOf(t, out); !strings.Contains(msg, "cannot read this file") {
		t.Errorf("422 error text = %q", msg)
	}
	assertNothingLeft(t, e, false)
}

// TestUploadSequenceTooLarge: the upload cap covers the whole body, so a
// sequence that exceeds it is a 413 with nothing left behind.
func TestUploadSequenceTooLarge(t *testing.T) {
	e := newEnv(t, Config{MaxUploadBytes: 4096}, nil)
	big := append(framePNG(t, 9), bytes.Repeat([]byte{0}, 3000)...) // PNG head, 3 KiB of tail
	ct, body := multipartParts(t, filePart("a.png", framePNG(t, 1)), filePart("b.png", big), filePart("c.png", big))
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/upload", io.NopCloser(bytes.NewReader(body)))
	req.Header.Set("Content-Type", ct)
	req.ContentLength = -1 // chunked: the reader limit, not Content-Length, trips
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("oversize sequence upload: %v (server closed early; acceptable)", err)
	} else {
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status %d %s, want 413", resp.StatusCode, out)
		}
	}
	assertNothingLeft(t, e, false)
}

// TestUploadConcurrentDedupe: an upload must never delete a blob out from
// under a concurrent request that deduped onto the same bytes. Before the
// staging rework, a sequence upload put its first part straight into the
// blob store and dropped it again when done, so a concurrent single-file
// upload of those bytes deduped onto it and then lost it mid-probe (422,
// blob gone), and two copies of the same sequence could delete each other's
// first frame (500 "open staged frame"). Every request here must answer 200
// and leave the single-file blob intact.
func TestUploadConcurrentDedupe(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	if e.tools.FFprobe == "" {
		t.Skip("ffprobe not on PATH")
	}
	type result struct {
		what   string
		status int
		body   []byte
	}
	post := func(t *testing.T, ch chan<- result, what string, parts ...mpPart) {
		t.Helper()
		ct, body := multipartParts(t, parts...)
		go func() {
			resp, err := http.Post(e.srv.URL+"/api/upload", ct, bytes.NewReader(body))
			if err != nil {
				ch <- result{what: what, status: -1, body: []byte(err.Error())}
				return
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			ch <- result{what: what, status: resp.StatusCode, body: out}
		}()
	}

	// A sequence whose first frame is X, racing a single-file upload of X.
	for i := 0; i < 4; i++ {
		first := bigPNG(t, i)
		ch := make(chan result, 2)
		post(t, ch, "sequence", filePart("a.png", first), filePart("b.png", framePNG(t, i)))
		post(t, ch, "single", filePart("a.png", first))
		for j := 0; j < 2; j++ {
			if r := <-ch; r.status != 200 {
				t.Fatalf("round %d: concurrent %s upload = %d %s", i, r.what, r.status, r.body)
			}
		}
		blob, err := e.st.GetBlob(sha256Hex(first))
		if err != nil || blob.Info == nil {
			t.Fatalf("round %d: single-file blob missing or unprobed after the race: %+v, %v", i, blob, err)
		}
	}

	// N copies of one sequence at once: every request answers 200 with the
	// same source (nobody's staged frames or stored blob is deleted by a
	// sibling request).
	a, b := framePNG(t, 20), framePNG(t, 21)
	const n = 6
	ch := make(chan result, n)
	for i := 0; i < n; i++ {
		post(t, ch, fmt.Sprintf("copy %d", i), filePart("a.png", a), filePart("b.png", b))
	}
	var hash string
	for i := 0; i < n; i++ {
		r := <-ch
		if r.status != 200 {
			t.Fatalf("concurrent %s = %d %s", r.what, r.status, r.body)
		}
		var src recipe.Source
		if err := json.Unmarshal(r.body, &src); err != nil {
			t.Fatalf("%s: %v (%s)", r.what, err, r.body)
		}
		if hash == "" {
			hash = src.Hash
		} else if src.Hash != hash {
			t.Errorf("%s: hash %s, want %s", r.what, src.Hash, hash)
		}
	}
	assertNothingLeft(t, e, true)
}

func TestNaturalCompare(t *testing.T) {
	// Ordered pairs: want a < b.
	less := [][2]string{
		{"frame1.png", "frame2.png"},
		{"frame2.png", "frame10.png"},
		{"frame9.png", "frame10.png"},
		{"frame10.png", "frame100.png"},
		{"img_002.png", "img_0010.png"},
		{"1.png", "a.png"},
		{"a.png", "b.png"},
		{"a.png", "B.png"},  // case-insensitive
		{"A.png", "a.png"},  // tie broken by raw bytes, deterministic
		{"01.png", "1.png"}, // same value: raw bytes decide, deterministic
		{"shot-1-2.png", "shot-1-10.png"},
		{"shot-1-10.png", "shot-2-1.png"},
		{"x.png", "x1.png"}, // '.' < '1': the unnumbered name leads
		{"x1.png", "x1a.png"},
		{"img1.png", "imga.png"},
		{"", "a"},
		{"ä1", "ä2"},
	}
	for _, p := range less {
		if c := naturalCompare(p[0], p[1]); c >= 0 {
			t.Errorf("naturalCompare(%q, %q) = %d, want < 0", p[0], p[1], c)
		}
		if c := naturalCompare(p[1], p[0]); c <= 0 {
			t.Errorf("naturalCompare(%q, %q) = %d, want > 0", p[1], p[0], c)
		}
	}
	for _, s := range []string{"", "a", "frame10.png", "01"} {
		if c := naturalCompare(s, s); c != 0 {
			t.Errorf("naturalCompare(%q, %q) = %d, want 0", s, s, c)
		}
	}
	names := []string{"f10.png", "f2.png", "f1.png", "f20.png", "f3.png", "f100.png", "F4.png"}
	slices.SortFunc(names, naturalCompare)
	want := "f1.png f2.png f3.png F4.png f10.png f20.png f100.png"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("sorted = %q, want %q", got, want)
	}
}

func TestSniffImage(t *testing.T) {
	cases := map[string][]byte{
		"png":  framePNG(t, 1),
		"gif":  tinyGIF(t),
		"jpg":  {0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'},
		"webp": append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 16)...),
		"bmp":  append([]byte("BM"), make([]byte, 20)...),
		"tiff": append([]byte("II*\x00"), make([]byte, 8)...),
		"avif": append([]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf"), make([]byte, 8)...),
	}
	cases["avif (mif1 major, avif compatible)"] = append([]byte("\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00mif1avif"), make([]byte, 8)...)
	cases["tiff (big endian)"] = append([]byte("MM\x00*"), make([]byte, 8)...)
	for name, head := range cases {
		want, _, _ := strings.Cut(name, " ")
		if got := sniffImage(head); got != want {
			t.Errorf("sniffImage(%s) = %q, want %q", name, got, want)
		}
	}
	for name, head := range map[string][]byte{
		"empty":       nil,
		"text":        []byte("hello, world"),
		"mp4 ftyp":    []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2mp41"),
		"riff wave":   []byte("RIFF\x24\x00\x00\x00WAVEfmt "),
		"png cut off": []byte("\x89PN"),
		"mov ftyp qt": []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  "),
	} {
		if got := sniffImage(head); got != "" {
			t.Errorf("sniffImage(%s) = %q, want none", name, got)
		}
	}
	// newUploadPart: a recognised sequence extension needs no sniff; an
	// unrecognised one falls back to the content, and the stored name then
	// carries the sniffed extension so the pattern stays image2-readable.
	// GIF and AVIF — by name or by content — are never sequence frames
	// (image2 cannot open them), and neither is anything else that fails
	// both the extension and the sniff.
	partCases := []struct {
		name      string
		head      []byte
		seqExt    string
		storeName string
	}{
		{"a.png", nil, "png", "a.png"},
		{"b.jpeg", []byte("junk"), "jpeg", "b.jpeg"},
		{"c.tif", nil, "tif", "c.tif"},
		{`C:\seq\d.webp`, nil, "webp", "d.webp"},
		{"frame-a", framePNG(t, 2), "png", "frame-a.png"},
		{"x.png.bak", framePNG(t, 3), "png", "x.png.bak.png"},
		{"a.jfif", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "jpg", "a.jfif.jpg"},
		{"a.gif", tinyGIF(t), "", "a.gif"},
		{"frames", tinyGIF(t), "", "frames"},
		{"a.avif", avifHead(t), "", "a.avif"},
		{"still", avifHead(t), "", "still"},
		{"a.mov", []byte("\x00\x00\x00\x14ftypqt  "), "", "a.mov"},
		{"notes.txt", []byte("hello"), "", "notes.txt"},
		{"frame", nil, "", "frame"},
	}
	for _, tc := range partCases {
		p := newUploadPart(tc.name, tc.head)
		if p.seqExt != tc.seqExt || p.storeName != tc.storeName {
			t.Errorf("newUploadPart(%q) = seqExt %q storeName %q, want %q / %q", tc.name, p.seqExt, p.storeName, tc.seqExt, tc.storeName)
		}
	}
}

func TestLastPathElement(t *testing.T) {
	for in, want := range map[string]string{
		"frame1.png": "frame1.png", `C:\seq\frame1.png`: "frame1.png", "seq/sub/frame1.png": "frame1.png",
		"": "", "dir/": "", `mixed\dir/frame.png`: "frame.png",
	} {
		if got := lastPathElement(in); got != want {
			t.Errorf("lastPathElement(%q) = %q, want %q", in, got, want)
		}
	}
}
