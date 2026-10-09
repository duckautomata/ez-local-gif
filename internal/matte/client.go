package matte

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one matte sidecar (spec §7.2). BaseURL is the sidecar's
// origin, e.g. "http://matte:9402" (EZLG_MATTE_URL); HTTP nil means
// http.DefaultClient — callers bound every call with its ctx (the app's
// per-batch timeout is derived from msPerFrame, §4.3).
//
// Every request that runs a model takes a device: "" is the sidecar's
// default device (no device= on the wire — a pre-5c sidecar never sees the
// parameter), anything else is sent as device= and must be one the ping
// offers (Ping.Offers; the sidecar answers 404 "device cpu not offered"
// otherwise).
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// ContentType is the request body type of POST /v1/matte and /v1/track:
// rgb24, row-major, frames × pixels × 3 bytes (BodyLength /
// TrackBodyLength).
const ContentType = "application/octet-stream"

// Record-stream errors of POST /v1/matte and /v1/track. Each returned error
// wraps one of these sentinels (errors.Is) and says where the stream went
// wrong; all of them mean a failed pass (spec §7.2: "a missing terminator
// or a short record count is a failed pass").
var (
	ErrTruncated         = errors.New("matte: response truncated")
	ErrShortCount        = errors.New("matte: fewer records than frames")
	ErrMissingTerminator = errors.New("matte: missing terminator")
	ErrRecordTooLarge    = errors.New("matte: record too large")
)

// StatusError is a non-2xx answer: the status plus the sidecar's JSON error
// body ({"error": "...", "retryAfterMs": N}) when it carried one. The
// documented statuses: 400 bad params / length mismatch / bad prompts, 404
// unknown model or size or an unoffered device, 413 too large, 503 model
// loading (with retryAfterMs) or out of memory, 507 OOM after unload.
type StatusError struct {
	Status       int
	Message      string // the sidecar's "error" field, else the HTTP status text
	RetryAfterMS int    // a 503 "model loading" says when to ask again; 0 when absent
	Body         []byte // the raw body, capped at 64 KB, for logs
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("matte sidecar: HTTP %d: %s", e.Status, e.Message)
	if e.RetryAfterMS > 0 {
		s += fmt.Sprintf(" (retry after %d ms)", e.RetryAfterMS)
	}
	return s
}

// RetryAfter is RetryAfterMS as a duration, 0 when the sidecar gave none.
func (e *StatusError) RetryAfter() time.Duration {
	if e.RetryAfterMS <= 0 {
		return 0
	}
	return time.Duration(e.RetryAfterMS) * time.Millisecond
}

const (
	maxErrorBody = 64 << 10 // bytes of a non-2xx body kept on a StatusError
	maxPingBody  = 1 << 20  // bytes of a /v1/ping body decoded
)

// BodyLength is the Content-Length of a POST /v1/matte carrying frames
// rgb24 squares of size×size pixels: frames × size × size × 3.
func BodyLength(frames, size int) int64 {
	return int64(frames) * int64(size) * int64(size) * 3
}

// TrackBodyLength is the Content-Length of a POST /v1/track carrying frames
// rgb24 frames of w×h pixels (frames × w × h × 3), and of a
// POST /v1/track/frame with frames = 1 — without a mask record; with one
// the body is MaskRecordLength(mask) longer.
func TrackBodyLength(frames, w, h int) int64 {
	return int64(frames) * int64(w) * int64(h) * 3
}

// MaskRecordLength is the length of the mask record Track / TrackFrame
// append to the frames for a mask prompt (Phase 5d): a 4-byte big-endian
// length then the PNG — 0 for nil (no record).
func MaskRecordLength(mask []byte) int64 {
	if mask == nil {
		return 0
	}
	return 4 + int64(len(mask))
}

// pngSignature opens every PNG file: the cheap check that a mask record
// holds a PNG and not, say, a raw gray plane.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// maskRecord pairs the prompts with the mask of a track request: it returns
// the record to append to the frames — nil when there is no mask prompt —
// after checking that the prompts have a mask prompt exactly when mask is
// non-nil, that mask is a PNG (pngSignature), and that the mask prompt's
// MaskDigest is MaskDigestOf(mask) (the key names what is sent; an empty
// digest is refused too — fill it from the bytes). Called before any
// request is made.
func maskRecord(prompts TrackPrompts, mask []byte) ([]byte, error) {
	frame, has := prompts.MaskFrame()
	switch {
	case !has && mask == nil:
		return nil, nil
	case !has:
		return nil, fmt.Errorf("a mask of %d bytes without a mask prompt (FramePrompt.Mask) to apply it to", len(mask))
	case mask == nil:
		return nil, fmt.Errorf("the prompt on frame %d has a mask but no mask was given", frame)
	case !bytes.HasPrefix(mask, pngSignature):
		return nil, fmt.Errorf("the mask for frame %d is not a PNG (%d bytes)", frame, len(mask))
	}
	var digest string
	for _, fp := range prompts.Prompts {
		if fp.Mask {
			digest = fp.MaskDigest
			break
		}
	}
	if want := MaskDigestOf(mask); digest != want {
		return nil, fmt.Errorf("the prompt on frame %d names mask %q but the mask sent is %q: fill FramePrompt.MaskDigest from the bytes sent (MaskDigestOf)", frame, digest, want)
	}
	rec := make([]byte, 4, 4+len(mask))
	binary.BigEndian.PutUint32(rec, uint32(len(mask)))
	return append(rec, mask...), nil
}

// Ping fetches GET /v1/ping. A non-200 answer is a *StatusError; a transport
// failure is the net/http error (the sidecar is down or unreachable).
func (c *Client) Ping(ctx context.Context) (*Ping, error) {
	req, err := c.request(ctx, http.MethodGet, "/v1/ping", nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	var p Ping
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPingBody)).Decode(&p); err != nil {
		return nil, fmt.Errorf("matte: /v1/ping: %w", err)
	}
	return &p, nil
}

// Warm asks the sidecar to create model's session on device now
// (POST /v1/warm?model=&device=; device "" = the default device) so a
// 10–20 s load overlaps the first scrub. Any 2xx is success; the sidecar
// answers before the load finishes.
func (c *Client) Warm(ctx context.Context, model, device string) error {
	if model == "" {
		return errors.New("matte: warm: empty model")
	}
	return c.post(ctx, "/v1/warm", withDevice(url.Values{"model": {model}}, device))
}

// Unload releases sessions now (POST /v1/unload[?model=&device=]): model's
// when model is set, every model's when it is ""; on device when device is
// set, on every device when it is "". Both empty releases everything the
// sidecar holds — what the app sends when the user leaves the AI mode.
func (c *Client) Unload(ctx context.Context, model, device string) error {
	q := url.Values{}
	if model != "" {
		q.Set("model", model)
	}
	return c.post(ctx, "/v1/unload", withDevice(q, device))
}

// Matte sends one batch: POST /v1/matte?model=&device=&size=&frames= (no
// device= when device is "", the sidecar's default) with body — frames
// rgb24 squares of size×size, exactly BodyLength(frames, size) bytes (the
// request fails client-side when the reader yields more or fewer) — and
// parses the response record stream, [uint32 BE length][PNG] per frame
// followed by a zero-length terminator, calling each per record in order
// with the PNG bytes (8-bit gray, size×size; the slice is the callback's to
// keep). each's error ends the parse and is returned as is.
//
// A non-200 answer is a *StatusError carrying the sidecar's JSON error and
// retryAfterMs; a short record count, a missing or non-zero terminator, or
// a stream cut mid-record is an ErrShortCount / ErrMissingTerminator /
// ErrTruncated error. When ctx ends mid-stream the ctx error is returned.
func (c *Client) Matte(ctx context.Context, model, device string, size, frames int, body io.Reader, each func(png []byte) error) error {
	switch {
	case model == "":
		return errors.New("matte: empty model")
	case size <= 0:
		return fmt.Errorf("matte: size %d", size)
	case frames <= 0:
		return fmt.Errorf("matte: frames %d", frames)
	case each == nil:
		return errors.New("matte: nil record callback")
	}
	q := withDevice(url.Values{"model": {model}, "size": {itoa(size)}, "frames": {itoa(frames)}}, device)
	return c.stream(ctx, "/v1/matte", q, nil, body, BodyLength(frames, size), frames, size*size, each)
}

// Track runs the sidecar's video tracker over a whole clip:
// POST /v1/track?model=&device=&w=&h=&frames= (no device= when device is
// "") with body — frames rgb24 frames of w×h, exactly
// TrackBodyLength(frames, w, h) bytes — and the prompts as the
// PromptsHeader JSON (TrackPrompts.Validate must pass: at least one prompt
// carrying a box, a positive point or a mask, else the call fails before
// any request is made). The answer is the same record stream as Matte, one
// 8-bit gray PNG of w×h per frame (binary 0/255 from the tracker's logits)
// in frame order, delivered to each; the error contract is Matte's.
//
// mask (Phase 5d) is the mask PNG of the prompts' mask prompt
// (FramePrompt.Mask; 8-bit gray at w×h, >= 128 = subject) or nil when there
// is none: when non-nil, one record [uint32 BE length][PNG] is appended to
// the body after the frames (Content-Length grows by
// MaskRecordLength(mask)) and the header names its frame. A mask without a
// mask prompt, a mask prompt without a mask, a non-PNG mask, or a mask
// whose MaskDigestOf is not the prompt's MaskDigest fail before any
// request is made (maskRecord).
func (c *Client) Track(ctx context.Context, model, device string, w, h, frames int, prompts TrackPrompts, body io.Reader, each func(png []byte) error, mask []byte) error {
	switch {
	case model == "":
		return errors.New("matte: track: empty model")
	case w <= 0 || h <= 0:
		return fmt.Errorf("matte: track: size %dx%d", w, h)
	case frames <= 0:
		return fmt.Errorf("matte: track: frames %d", frames)
	case each == nil:
		return errors.New("matte: track: nil record callback")
	}
	hdr, err := prompts.headerValue()
	if err != nil {
		return fmt.Errorf("matte: track: %w", err)
	}
	rec, err := maskRecord(prompts, mask)
	if err != nil {
		return fmt.Errorf("matte: track: %w", err)
	}
	length := TrackBodyLength(frames, w, h)
	if rec != nil {
		body = io.MultiReader(body, bytes.NewReader(rec))
		length += int64(len(rec))
	}
	q := withDevice(url.Values{"model": {model}, "w": {itoa(w)}, "h": {itoa(h)}, "frames": {itoa(frames)}}, device)
	return c.stream(ctx, "/v1/track", q, http.Header{PromptsHeader: {hdr}}, body, length, frames, w*h, each)
}

// TrackFrame asks the tracker for ONE frame's mask from that frame's
// prompts (the live overlay while the user clicks):
// POST /v1/track/frame?model=&device=&w=&h= with frame — one rgb24 frame
// of w×h, exactly TrackBodyLength(1, w, h) bytes — and the prompts
// (TrackPrompts.ForFrame of the clicked frame; Validate must pass) as the
// PromptsHeader JSON. The 200 body is the mask, an 8-bit gray PNG of w×h,
// returned as is; a non-200 answer is a *StatusError (503 with
// retryAfterMs while the tracker loads). mask is as for Track: the mask
// PNG of the frame's mask prompt, appended as one record after the frame,
// or nil.
func (c *Client) TrackFrame(ctx context.Context, model, device string, w, h int, prompts TrackPrompts, frame []byte, mask []byte) ([]byte, error) {
	switch {
	case model == "":
		return nil, errors.New("matte: track frame: empty model")
	case w <= 0 || h <= 0:
		return nil, fmt.Errorf("matte: track frame: size %dx%d", w, h)
	case int64(len(frame)) != TrackBodyLength(1, w, h):
		return nil, fmt.Errorf("matte: track frame: %d bytes, want %d for %dx%d rgb24", len(frame), TrackBodyLength(1, w, h), w, h)
	}
	hdr, err := prompts.headerValue()
	if err != nil {
		return nil, fmt.Errorf("matte: track frame: %w", err)
	}
	rec, err := maskRecord(prompts, mask)
	if err != nil {
		return nil, fmt.Errorf("matte: track frame: %w", err)
	}
	payload := frame
	if rec != nil {
		payload = make([]byte, 0, len(frame)+len(rec))
		payload = append(append(payload, frame...), rec...)
	}
	q := withDevice(url.Values{"model": {model}, "w": {itoa(w)}, "h": {itoa(h)}}, device)
	req, err := c.request(ctx, http.MethodPost, "/v1/track/frame", q, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set(PromptsHeader, hdr)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	limit := recordCap(w * h)
	png, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: reading the mask: %v", ErrTruncated, err)
	}
	if uint64(len(png)) > limit {
		return nil, fmt.Errorf("%w: a %dx%d mask over %d bytes", ErrRecordTooLarge, w, h, limit)
	}
	if len(png) == 0 {
		return nil, fmt.Errorf("%w: empty mask", ErrTruncated)
	}
	return png, nil
}

// stream posts body (contentLength bytes of rgb24) to path with q and the
// extra headers and parses the record stream of frames PNGs of pixels
// pixels each.
func (c *Client) stream(ctx context.Context, path string, q url.Values, extra http.Header, body io.Reader, contentLength int64, frames, pixels int, each func([]byte) error) error {
	req, err := c.request(ctx, http.MethodPost, path, q, body)
	if err != nil {
		return err
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", ContentType)
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	if err := readRecords(resp.Body, frames, pixels, each); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// recordCap bounds one record of a mask of pixels pixels: a gray PNG cannot
// reasonably exceed its raw bytes by much (zlib stored blocks add 5 bytes
// per 64 KB), so a header claiming more is a corrupt stream, not a frame.
func recordCap(pixels int) uint64 { return uint64(pixels)*2 + 1<<20 }

// readRecords parses frames records of masks of pixels pixels then the
// terminator from r.
func readRecords(r io.Reader, frames, pixels int, each func([]byte) error) error {
	var hdr [4]byte
	capBytes := recordCap(pixels)
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return fmt.Errorf("%w: before record %d of %d: %v", ErrTruncated, i+1, frames, err)
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 {
			return fmt.Errorf("%w: %d of %d", ErrShortCount, i, frames)
		}
		if uint64(n) > capBytes {
			return fmt.Errorf("%w: record %d of %d claims %d bytes", ErrRecordTooLarge, i+1, frames, n)
		}
		png := make([]byte, n)
		if _, err := io.ReadFull(r, png); err != nil {
			return fmt.Errorf("%w: inside record %d of %d: %v", ErrTruncated, i+1, frames, err)
		}
		if err := each(png); err != nil {
			return err
		}
	}
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("%w: after %d records: %v", ErrMissingTerminator, frames, err)
	}
	if n := binary.BigEndian.Uint32(hdr[:]); n != 0 {
		return fmt.Errorf("%w: a %d-byte record follows the %d requested", ErrMissingTerminator, n, frames)
	}
	return nil
}

// withDevice adds device= to q when device is set; "" (the default device)
// leaves the query as a pre-5c sidecar expects it.
func withDevice(q url.Values, device string) url.Values {
	if device != "" {
		q.Set("device", device)
	}
	return q
}

func (c *Client) post(ctx context.Context, path string, q url.Values) error {
	req, err := c.request(ctx, http.MethodPost, path, q, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	if c.BaseURL == "" {
		return nil, errors.New("matte: no sidecar URL")
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return http.NewRequestWithContext(ctx, method, u, body)
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	return h.Do(req)
}

// statusError reads a non-2xx body (capped) and lifts the sidecar's JSON
// error fields out of it when present.
func statusError(resp *http.Response) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := &StatusError{Status: resp.StatusCode, Body: body, Message: http.StatusText(resp.StatusCode)}
	if e.Message == "" {
		e.Message = "status " + itoa(resp.StatusCode)
	}
	var js struct {
		Error        string  `json:"error"`
		RetryAfterMS float64 `json:"retryAfterMs"`
	}
	if json.Unmarshal(body, &js) == nil && js.Error != "" {
		e.Message = js.Error
		if js.RetryAfterMS > 0 {
			e.RetryAfterMS = int(js.RetryAfterMS)
		}
	}
	return e
}
