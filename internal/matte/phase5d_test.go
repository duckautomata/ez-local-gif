package matte

// Phase 5d: the mask prompt — FramePrompt.Mask / MaskDigest, MaskFrame,
// the ":mask=m<digest>" marker in Canonical, the "mask":{"frame":N} key in
// the wire header, and the one mask record Track / TrackFrame append to
// the frames (checked against the prompt's digest before any request).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// maskPNG is a stand-in mask: a PNG signature and a few bytes (the client
// never decodes the mask, only checks the signature and digests it).
var maskPNG = append([]byte("\x89PNG\r\n\x1a\n"), []byte("mask-of-frame-3")...)

// maskPrompts is a mask prompt on frame 3 (no box, no point) plus a box
// on frame 0, with the digest filled the way jobs does.
func maskPrompts() TrackPrompts {
	return TrackPrompts{Obj: 1, Prompts: []FramePrompt{
		{Frame: 3, Mask: true, MaskDigest: MaskDigestOf(maskPNG)},
		{Frame: 0, Box: &[4]float64{0.1, 0.2, 0.6, 0.9}},
	}}
}

func TestMaskDigestOf(t *testing.T) {
	sum := sha256.Sum256(maskPNG)
	want := hex.EncodeToString(sum[:])[:12]
	if got := MaskDigestOf(maskPNG); got != want || len(got) != MaskDigestLen {
		t.Errorf("MaskDigestOf = %q, want %q", got, want)
	}
	if MaskDigestOf(nil) == MaskDigestOf(maskPNG) || MaskDigestOf(nil) == "" {
		t.Error("the digest of nothing equals the mask's or is empty")
	}
	other := append(bytes.Clone(maskPNG), 'x')
	if MaskDigestOf(other) == MaskDigestOf(maskPNG) {
		t.Error("one extra byte left the digest alone")
	}
}

func TestPromptsMask(t *testing.T) {
	p := maskPrompts()
	if err := p.Validate(); err != nil {
		t.Fatalf("a mask prompt beside a box: %v", err)
	}
	if f, ok := p.MaskFrame(); !ok || f != 3 {
		t.Errorf("MaskFrame = %d, %v; want 3, true", f, ok)
	}
	if f, ok := boxPrompts().MaskFrame(); ok || f != 0 {
		t.Errorf("MaskFrame without a mask = %d, %v", f, ok)
	}
	// The mask alone anchors the set: no box, no positive point needed — a
	// negative click beside it is a refinement, not a refusal.
	alone := TrackPrompts{Prompts: []FramePrompt{{Frame: 3, Mask: true}}}
	if err := alone.Validate(); err != nil {
		t.Errorf("mask-only set: %v", err)
	}
	refined := TrackPrompts{Prompts: []FramePrompt{{Frame: 3, Mask: true, Points: [][3]float64{{0.5, 0.5, 0}}}}}
	if err := refined.Validate(); err != nil {
		t.Errorf("mask plus a negative click: %v", err)
	}
	// Two masks are refused (one record per request); a mask on a bad frame
	// or with a bad box still fails on those.
	two := TrackPrompts{Prompts: []FramePrompt{{Frame: 3, Mask: true}, {Frame: 5, Mask: true, Box: &[4]float64{0.1, 0.1, 0.5, 0.5}}}}
	if err := two.Validate(); err == nil || !strings.Contains(err.Error(), "second mask") {
		t.Errorf("two mask prompts: %v", err)
	}
	if err := (TrackPrompts{Prompts: []FramePrompt{{Frame: -1, Mask: true}}}).Validate(); err == nil {
		t.Error("mask on frame -1 accepted")
	}
	if err := (TrackPrompts{Prompts: []FramePrompt{{Frame: 3, Mask: true, Box: &[4]float64{0.6, 0.2, 0.1, 0.9}}}}).Validate(); err == nil {
		t.Error("mask with an inverted box accepted")
	}
	// The pre-5d refusals read the same: a prompt with nothing, a negative-only set.
	if err := (TrackPrompts{Prompts: []FramePrompt{{Frame: 0}}}).Validate(); err == nil {
		t.Error("an empty prompt accepted")
	}
	if err := (TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Points: [][3]float64{{0.5, 0.5, 0}}}}}).Validate(); err == nil {
		t.Error("a negative-only set accepted")
	}

	// ForFrame keeps the mask prompt of its frame, flag and digest included.
	f3 := p.ForFrame(3)
	if len(f3.Prompts) != 1 || !f3.Prompts[0].Mask || f3.Prompts[0].MaskDigest != MaskDigestOf(maskPNG) {
		t.Errorf("ForFrame(3) = %+v", f3)
	}
	if f0 := p.ForFrame(0); len(f0.Prompts) != 1 || f0.Prompts[0].Mask {
		t.Errorf("ForFrame(0) = %+v", f0)
	}

	// Canonical: the mask marker names the digest, in frame order, after the
	// frame's box and points; a prompt without a mask is the pre-5d text.
	wantCanon := "obj=1|f=0:box=0.1000,0.2000,0.6000,0.9000:pts=|f=3:box=-:pts=:mask=m" + MaskDigestOf(maskPNG)
	if got := p.Canonical(); got != wantCanon {
		t.Errorf("Canonical:\n got %s\nwant %s", got, wantCanon)
	}
	if got := boxPrompts().Canonical(); got != boxPromptsCanonical || strings.Contains(got, "mask") {
		t.Errorf("a set without a mask changed: %s", got)
	}
	// The digest is the mask's identity in the key: another mask, another
	// key; no digest, a bare marker (documented — callers fill it first).
	q := maskPrompts()
	q.Prompts[0].MaskDigest = MaskDigestOf(append(bytes.Clone(maskPNG), 'x'))
	if q.Canonical() == p.Canonical() {
		t.Error("a different mask digest gave the same key text")
	}
	q.Prompts[0].MaskDigest = ""
	if got := q.Canonical(); !strings.HasSuffix(got, ":mask=m") {
		t.Errorf("empty digest: %s", got)
	}
	// Refinements on the mask frame render with it.
	r := TrackPrompts{Prompts: []FramePrompt{{Frame: 3, Mask: true, MaskDigest: "abcdefabcdef", Points: [][3]float64{{0.7, 0.4, 0}}, Box: &[4]float64{0.1, 0.2, 0.6, 0.9}}}}
	if got, want := r.Canonical(), "obj=1|f=3:box=0.1000,0.2000,0.6000,0.9000:pts=0.7000,0.4000,0:mask=mabcdefabcdef"; got != want {
		t.Errorf("mask with refinements:\n got %s\nwant %s", got, want)
	}

	// The wire header: the mask frame stays listed in prompts (empty points,
	// null box when the mask is all it has) and a top-level mask names it;
	// the digest never travels. A set without a mask has no mask key.
	hdr, err := p.headerValue()
	if err != nil {
		t.Fatal(err)
	}
	const wantHdr = `{"obj":1,"prompts":[{"frame":0,"points":[],"box":[0.1,0.2,0.6,0.9]},{"frame":3,"points":[],"box":null}],"mask":{"frame":3}}`
	if hdr != wantHdr {
		t.Errorf("headerValue:\n got %s\nwant %s", hdr, wantHdr)
	}
	if strings.Contains(hdr, MaskDigestOf(maskPNG)) {
		t.Error("the digest is on the wire")
	}
	hdr, err = r.headerValue()
	if err != nil || hdr != `{"obj":1,"prompts":[{"frame":3,"points":[[0.7,0.4,0]],"box":[0.1,0.2,0.6,0.9]}],"mask":{"frame":3}}` {
		t.Errorf("mask with refinements on the wire: %v %s", err, hdr)
	}
	hdr, _ = boxPrompts().headerValue()
	if hdr != boxPromptsHeader {
		t.Errorf("a set without a mask grew a key: %s", hdr)
	}
	if _, err := two.headerValue(); err == nil {
		t.Error("headerValue of two masks")
	}
}

// readMaskRecord splits a track body into the frames and the trailing
// mask record's PNG (nil when the body is exactly the frames).
func readMaskRecord(t *testing.T, body []byte, frames int64) ([]byte, []byte) {
	t.Helper()
	if int64(len(body)) == frames {
		return body, nil
	}
	if int64(len(body)) < frames+4 {
		t.Fatalf("body of %d bytes: neither %d frames nor frames plus a record", len(body), frames)
	}
	n := binary.BigEndian.Uint32(body[frames : frames+4])
	if int64(len(body)) != frames+4+int64(n) {
		t.Fatalf("body of %d bytes: record claims %d after %d frame bytes", len(body), n, frames)
	}
	return body[:frames], body[frames+4:]
}

func TestTrackMaskRecord(t *testing.T) {
	const w, h, frames = 3, 2, 4
	body := make([]byte, TrackBodyLength(frames, w, h))
	for i := range body {
		body[i] = byte(i * 3)
	}
	want := [][]byte{fakePNG(1), fakePNG(2), fakePNG(3), fakePNG(4)}
	var gotQuery, gotPrompts string
	var gotLen int64
	var gotBody []byte
	hits := 0
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotQuery, gotLen, gotPrompts = r.URL.RawQuery, r.ContentLength, r.Header.Get(PromptsHeader)
		gotBody, _ = io.ReadAll(r.Body)
		writeRecords(w, want, true)
	})
	var got [][]byte
	err := c.Track(context.Background(), "sam2-tiny", DeviceCUDA, w, h, frames, maskPrompts(), bytes.NewReader(body), func(png []byte) error {
		got = append(got, png)
		return nil
	}, maskPNG)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "device=cuda&frames=4&h=2&model=sam2-tiny&w=3" {
		t.Errorf("query ?%s", gotQuery)
	}
	// The body is the frames then ONE record [uint32 BE len][PNG]; the
	// Content-Length says so up front.
	if wantLen := TrackBodyLength(frames, w, h) + MaskRecordLength(maskPNG); gotLen != wantLen || int64(len(gotBody)) != wantLen {
		t.Errorf("Content-Length %d, body %d, want %d", gotLen, len(gotBody), wantLen)
	}
	fr, rec := readMaskRecord(t, gotBody, TrackBodyLength(frames, w, h))
	if !bytes.Equal(fr, body) || !bytes.Equal(rec, maskPNG) {
		t.Errorf("frames equal %v, record equal %v", bytes.Equal(fr, body), bytes.Equal(rec, maskPNG))
	}
	if !strings.HasSuffix(gotPrompts, `"mask":{"frame":3}}`) {
		t.Errorf("header without the mask frame: %s", gotPrompts)
	}
	if len(got) != frames || !bytes.Equal(got[3], want[3]) {
		t.Errorf("records: %d", len(got))
	}
	if MaskRecordLength(nil) != 0 || MaskRecordLength(maskPNG) != 4+int64(len(maskPNG)) {
		t.Error("MaskRecordLength")
	}

	// Without a mask the body is exactly the frames (the pre-5d request).
	hits = 0
	if err := c.Track(context.Background(), "sam2-tiny", "", w, h, frames, boxPrompts(), bytes.NewReader(body), func([]byte) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if gotLen != TrackBodyLength(frames, w, h) || !bytes.Equal(gotBody, body) || strings.Contains(gotPrompts, "mask") {
		t.Errorf("no mask: len %d body equal %v header %s", gotLen, bytes.Equal(gotBody, body), gotPrompts)
	}

	// Refused before any request: a mask without a mask prompt, a mask
	// prompt without a mask, a non-PNG mask, a digest that is not the
	// mask's (empty included).
	hits = 0
	each := func([]byte) error { return nil }
	ctx := context.Background()
	if err := c.Track(ctx, "sam2-tiny", "", w, h, frames, boxPrompts(), bytes.NewReader(body), each, maskPNG); err == nil || !strings.Contains(err.Error(), "without a mask prompt") {
		t.Errorf("mask without a mask prompt: %v", err)
	}
	if err := c.Track(ctx, "sam2-tiny", "", w, h, frames, maskPrompts(), bytes.NewReader(body), each, nil); err == nil || !strings.Contains(err.Error(), "no mask was given") {
		t.Errorf("mask prompt without a mask: %v", err)
	}
	raw := bytes.Repeat([]byte{255}, w*h)
	rawPrompts := maskPrompts()
	rawPrompts.Prompts[0].MaskDigest = MaskDigestOf(raw)
	if err := c.Track(ctx, "sam2-tiny", "", w, h, frames, rawPrompts, bytes.NewReader(body), each, raw); err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Errorf("raw mask: %v", err)
	}
	stale := maskPrompts()
	stale.Prompts[0].MaskDigest = MaskDigestOf(append(bytes.Clone(maskPNG), 'x'))
	if err := c.Track(ctx, "sam2-tiny", "", w, h, frames, stale, bytes.NewReader(body), each, maskPNG); err == nil || !strings.Contains(err.Error(), "MaskDigest") {
		t.Errorf("stale digest: %v", err)
	}
	stale.Prompts[0].MaskDigest = ""
	if err := c.Track(ctx, "sam2-tiny", "", w, h, frames, stale, bytes.NewReader(body), each, maskPNG); err == nil || !strings.Contains(err.Error(), "MaskDigest") {
		t.Errorf("empty digest: %v", err)
	}
	if hits != 0 {
		t.Errorf("%d requests made for refused masks", hits)
	}
	// A 503 while the tracker loads is the usual StatusError, mask or not.
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
		io.WriteString(w, `{"error":"model loading","retryAfterMs":800}`)
	})
	err = c.Track(ctx, "sam2-tiny", "", w, h, frames, maskPrompts(), bytes.NewReader(body), each, maskPNG)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || se.RetryAfterMS != 800 {
		t.Errorf("503 track with a mask: %v", err)
	}
}

func TestTrackFrameMaskRecord(t *testing.T) {
	const w, h = 2, 2
	frame := make([]byte, TrackBodyLength(1, w, h))
	for i := range frame {
		frame[i] = byte(90 + i)
	}
	answer := fakePNG(9)
	var gotPrompts string
	var gotLen int64
	var gotBody []byte
	hits := 0
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotLen, gotPrompts = r.ContentLength, r.Header.Get(PromptsHeader)
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "image/png")
		w.Write(answer)
	})
	only := maskPrompts().ForFrame(3)
	got, err := c.TrackFrame(context.Background(), "sam2-tiny", "", w, h, only, frame, maskPNG)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, answer) {
		t.Errorf("mask %q, want %q", got, answer)
	}
	if wantLen := TrackBodyLength(1, w, h) + MaskRecordLength(maskPNG); gotLen != wantLen || int64(len(gotBody)) != wantLen {
		t.Errorf("Content-Length %d, body %d, want %d", gotLen, len(gotBody), wantLen)
	}
	fr, rec := readMaskRecord(t, gotBody, TrackBodyLength(1, w, h))
	if !bytes.Equal(fr, frame) || !bytes.Equal(rec, maskPNG) {
		t.Errorf("frame equal %v, record equal %v", bytes.Equal(fr, frame), bytes.Equal(rec, maskPNG))
	}
	if gotPrompts != `{"obj":1,"prompts":[{"frame":3,"points":[],"box":null}],"mask":{"frame":3}}` {
		t.Errorf("header: %s", gotPrompts)
	}
	// The frame's clicks refine the mask on the same request.
	refined := TrackPrompts{Prompts: []FramePrompt{{Frame: 3, Mask: true, MaskDigest: MaskDigestOf(maskPNG), Points: [][3]float64{{0.5, 0.5, 0}}}}}
	if _, err := c.TrackFrame(context.Background(), "sam2-tiny", DeviceCPU, w, h, refined, frame, maskPNG); err != nil {
		t.Fatal(err)
	}
	if gotPrompts != `{"obj":1,"prompts":[{"frame":3,"points":[[0.5,0.5,0]],"box":null}],"mask":{"frame":3}}` {
		t.Errorf("refined header: %s", gotPrompts)
	}
	// Without a mask: the pre-5d body and header.
	if _, err := c.TrackFrame(context.Background(), "sam2-tiny", "", w, h, boxPrompts().ForFrame(0), frame, nil); err != nil {
		t.Fatal(err)
	}
	if gotLen != TrackBodyLength(1, w, h) || !bytes.Equal(gotBody, frame) || strings.Contains(gotPrompts, "mask") {
		t.Errorf("no mask: len %d body equal %v header %s", gotLen, bytes.Equal(gotBody, frame), gotPrompts)
	}
	// Refused before any request, like Track.
	hits = 0
	ctx := context.Background()
	if _, err := c.TrackFrame(ctx, "sam2-tiny", "", w, h, boxPrompts().ForFrame(0), frame, maskPNG); err == nil {
		t.Error("mask without a mask prompt accepted")
	}
	if _, err := c.TrackFrame(ctx, "sam2-tiny", "", w, h, only, frame, nil); err == nil {
		t.Error("mask prompt without a mask accepted")
	}
	stale := maskPrompts().ForFrame(3)
	stale.Prompts[0].MaskDigest = "000000000000"
	if _, err := c.TrackFrame(ctx, "sam2-tiny", "", w, h, stale, frame, maskPNG); err == nil || !strings.Contains(err.Error(), "MaskDigest") {
		t.Errorf("stale digest: %v", err)
	}
	if _, err := c.TrackFrame(ctx, "sam2-tiny", "", w, h, only, frame, []byte("not a png")); err == nil {
		t.Error("non-PNG mask accepted")
	}
	if hits != 0 {
		t.Errorf("%d requests made for refused masks", hits)
	}
}

// TestClipKeyMaskDigest: the mask's digest reaches the clip key through
// Canonical — two tracks from different masks never share a memo.
func TestClipKeyMaskDigest(t *testing.T) {
	tr := goldenParts()
	tr.Model, tr.Size, tr.Precision, tr.Weights = "sam2-tiny", 0, "bf16", strings.Repeat("74", 32)
	tr.TrackW, tr.TrackH = 1024, 576
	a := maskPrompts()
	tr.Prompts = a.Canonical()
	keyA := ClipKey(tr)
	b := maskPrompts()
	b.Prompts[0].MaskDigest = MaskDigestOf(append(bytes.Clone(maskPNG), 'x'))
	tr.Prompts = b.Canonical()
	if ClipKey(tr) == keyA {
		t.Error("another mask, the same clip key")
	}
	tr.Prompts = boxPrompts().Canonical()
	if ClipKey(tr) == keyA {
		t.Error("a box-only set keys like the mask set")
	}
	if !strings.Contains(clipKeyText(func() ClipKeyParts { p := tr; p.Prompts = a.Canonical(); return p }()), ":mask=m"+MaskDigestOf(maskPNG)) {
		t.Error("the key text does not carry the mask marker")
	}
}
