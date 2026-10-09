package matte

import (
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
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// ContentType is the request body type of POST /v1/matte: rgb24, row-major,
// frames × size × size × 3 bytes (BodyLength).
const ContentType = "application/octet-stream"

// Record-stream errors of POST /v1/matte. Each returned error wraps one of
// these sentinels (errors.Is) and says where the stream went wrong; all of
// them mean a failed pass (spec §7.2: "a missing terminator or a short
// record count is a failed pass").
var (
	ErrTruncated         = errors.New("matte: response truncated")
	ErrShortCount        = errors.New("matte: fewer records than frames")
	ErrMissingTerminator = errors.New("matte: missing terminator")
	ErrRecordTooLarge    = errors.New("matte: record too large")
)

// StatusError is a non-2xx answer: the status plus the sidecar's JSON error
// body ({"error": "...", "retryAfterMs": N}) when it carried one. The
// documented statuses: 400 bad params / length mismatch, 404 unknown model
// or size, 413 too large, 503 model loading (with retryAfterMs) or out of
// memory, 507 OOM after unload.
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

// Warm asks the sidecar to create model's session now (POST /v1/warm) so a
// 10–20 s load overlaps the first scrub. Any 2xx is success; the sidecar
// answers before the load finishes.
func (c *Client) Warm(ctx context.Context, model string) error {
	if model == "" {
		return errors.New("matte: warm: empty model")
	}
	return c.post(ctx, "/v1/warm", url.Values{"model": {model}})
}

// Unload releases model's sessions now (POST /v1/unload), every model's
// when model is "".
func (c *Client) Unload(ctx context.Context, model string) error {
	q := url.Values{}
	if model != "" {
		q.Set("model", model)
	}
	return c.post(ctx, "/v1/unload", q)
}

// Matte sends one batch: POST /v1/matte?model=&size=&frames= with body —
// frames rgb24 squares of size×size, exactly BodyLength(frames, size) bytes
// (the request fails client-side when the reader yields more or fewer) —
// and parses the response record stream, [uint32 BE length][PNG] per frame
// followed by a zero-length terminator, calling each per record in order
// with the PNG bytes (8-bit gray, size×size; the slice is the callback's to
// keep). each's error ends the parse and is returned as is.
//
// A non-200 answer is a *StatusError carrying the sidecar's JSON error and
// retryAfterMs; a short record count, a missing or non-zero terminator, or
// a stream cut mid-record is an ErrShortCount / ErrMissingTerminator /
// ErrTruncated error. When ctx ends mid-stream the ctx error is returned.
func (c *Client) Matte(ctx context.Context, model string, size, frames int, body io.Reader, each func(png []byte) error) error {
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
	q := url.Values{"model": {model}, "size": {itoa(size)}, "frames": {itoa(frames)}}
	req, err := c.request(ctx, http.MethodPost, "/v1/matte", q, body)
	if err != nil {
		return err
	}
	req.ContentLength = BodyLength(frames, size)
	req.Header.Set("Content-Type", ContentType)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	if err := readRecords(resp.Body, frames, size, each); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// readRecords parses frames records then the terminator from r.
func readRecords(r io.Reader, frames, size int, each func([]byte) error) error {
	var hdr [4]byte
	// A gray PNG of size×size cannot reasonably exceed its raw bytes by much
	// (zlib stored blocks add 5 bytes per 64 KB); a header claiming more is
	// a corrupt stream, not a frame.
	capBytes := uint64(size)*uint64(size)*2 + 1<<20
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
