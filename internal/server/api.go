package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/probe"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// ---- capabilities -----------------------------------------------------------

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	var versions map[string]string
	if s.jm != nil {
		versions = s.jm.ToolVersions()
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		versions = s.tools.Versions(ctx)
	}
	if versions == nil {
		versions = map[string]string{}
	}
	conc := 0
	if s.jm != nil {
		conc = s.jm.Concurrency()
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"tools": versions,
		"limits": map[string]int64{
			"emote":      discordlint.Limit(discordlint.TargetEmote),
			"sticker":    discordlint.Limit(discordlint.TargetSticker),
			"attachment": discordlint.Limit(discordlint.TargetAttachment),
		},
		"rulesVersion":   discordlint.RulesVersion,
		"version":        s.cfg.Version,
		"concurrency":    conc,
		"maxUploadBytes": s.cfg.MaxUploadBytes,
		"formats":        []string{"gif", "webp"},
	})
}

// ---- upload / sources ---------------------------------------------------------

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > s.cfg.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds the %d byte limit", s.cfg.MaxUploadBytes))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data with a \"file\" field: "+errText(err))
		return
	}

	var blob *store.Blob
	for blob == nil {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.uploadReadError(w, err)
			return
		}
		if part.FormName() != "file" {
			continue // NextPart discards the rest of this part
		}
		blob, err = s.st.PutBlob(part, part.FileName())
		if err != nil {
			s.uploadReadError(w, err)
			return
		}
	}
	if blob == nil {
		writeError(w, http.StatusBadRequest, "multipart body has no \"file\" field")
		return
	}
	if blob.Size == 0 {
		s.discardBlob(blob.Hash)
		writeError(w, http.StatusBadRequest, "uploaded file is empty")
		return
	}

	if blob.Info == nil {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		info, err := probe.Probe(ctx, s.tools, blob.Path, probeScanFrames)
		if err != nil {
			s.probeFailed(w, r, blob.Hash, err)
			return
		}
		if err := s.st.SetBlobInfo(blob.Hash, info); err != nil {
			log.Printf("server: store probe info for %s: %v", blob.Hash, err)
			writeError(w, http.StatusInternalServerError, "failed to store probe info: "+errText(err))
			return
		}
		blob.Info = &info
		s.warmStill(blob.Hash, info)
	}
	writeJSON(w, http.StatusOK, sourceOf(blob))
}

// probeFailed answers an upload whose probe failed. Only a file that ffprobe
// ran on but could not make sense of is the client's fault: that blob is
// dead weight and is dropped (422) so the sweeper never has to and a retry
// re-probes from scratch. Everything else — ffprobe missing or not
// executable (500), the probe timing out (504) — is the server's problem;
// the blob is kept, unprobed, so a re-upload dedupes it and probes again. A
// client that hung up mid-probe gets no answer at all.
func (s *Server) probeFailed(w http.ResponseWriter, r *http.Request, hash string, err error) {
	switch {
	case r.Context().Err() != nil, errors.Is(err, context.Canceled):
		return // client went away; nobody is listening
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("server: probe %s: timed out after %s", hash, probeTimeout)
		writeError(w, http.StatusGatewayTimeout, "probing the upload timed out")
	case unreadableSource(err):
		s.discardBlob(hash)
		writeError(w, http.StatusUnprocessableEntity, "cannot read this file as an image or video: "+errText(err))
	default:
		log.Printf("server: probe %s: %v", hash, err)
		writeError(w, http.StatusInternalServerError, "probe failed: "+errText(err))
	}
}

// unreadableSource reports whether a probe error means "ffprobe ran and
// could not read this file" — it exited non-zero on the file, its output
// was not the JSON it prints for anything it can open, or it found no video
// stream — as opposed to ffprobe itself failing to run.
func unreadableSource(err error) bool {
	var (
		exit   *exec.ExitError
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	return errors.Is(err, probe.ErrNoVideo) ||
		errors.As(err, &exit) ||
		errors.As(err, &syntax) ||
		errors.As(err, &typ)
}

// warmStill pre-renders the first preview frame in the background
// (DESIGN.md §8: still pre-warmed the moment the upload lands) so the UI's
// first /api/still hits the memo. The request therefore mirrors what the
// SPA sends for a fresh source — the default preset's geometry (none), t=0,
// 480 px wide, and the unpremultiply op it turns on for a premultiplied
// source (state.svelte.ts defaultOps) — since anything else would key a
// different memo entry. Best effort, errors are ignored. The render runs
// under the server lifetime, so Shutdown kills it rather than orphaning an
// ffmpeg.
func (s *Server) warmStill(hash string, info recipe.ProbeInfo) {
	if s.jm == nil || s.tools.FFmpeg == "" {
		return
	}
	ops, out, t, maxW := warmStillRequest(info)
	s.spawn(func() {
		ctx, cancel := context.WithTimeout(s.ctx, stillTimeout)
		defer cancel()
		_, _ = s.jm.Still(ctx, hash, ops, out, t, maxW)
	})
}

// warmStillRequest is the still the SPA asks for first after an upload.
func warmStillRequest(info recipe.ProbeInfo) (ops []recipe.Op, out recipe.Output, t float64, maxW int) {
	if info.Premultiplied {
		ops = []recipe.Op{{Kind: recipe.OpUnpremultiply}}
	}
	return ops, recipe.Output{Format: "gif"}, 0, jobs.DefaultStillWidth
}

// discardBlob removes a blob that turned out to be unusable (best effort).
func (s *Server) discardBlob(hash string) {
	if err := s.st.DeleteBlob(hash); err != nil {
		log.Printf("server: discard blob %s: %v", hash, err)
	}
}

// uploadReadError maps body-read failures to 413 (limit hit) or 400/500.
func (s *Server) uploadReadError(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds the %d byte limit", s.cfg.MaxUploadBytes))
		return
	}
	// A client that hangs up mid-upload is not a server error.
	if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "multipart:") {
		writeError(w, http.StatusBadRequest, "malformed or truncated upload: "+errText(err))
		return
	}
	log.Printf("server: upload: %v", err)
	writeError(w, http.StatusInternalServerError, "upload failed: "+errText(err))
}

func sourceOf(b *store.Blob) recipe.Source {
	src := recipe.Source{Hash: b.Hash, Name: b.Name, Size: b.Size}
	if b.Info != nil {
		src.Info = *b.Info
	}
	return src
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !recipe.IsHash(hash) {
		writeError(w, http.StatusNotFound, "not a source hash")
		return
	}
	blob, err := s.st.GetBlob(hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown source")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	if blob.Info == nil {
		writeError(w, http.StatusConflict, "source has not been probed yet; upload it again")
		return
	}
	writeJSON(w, http.StatusOK, sourceOf(blob))
}

// ---- still --------------------------------------------------------------------

type stillRequest struct {
	Src    string        `json:"src"`
	Ops    []recipe.Op   `json:"ops"`
	Output recipe.Output `json:"output"`
	T      float64       `json:"t"`
	MaxW   int           `json:"maxW"`
}

func (s *Server) handleStill(w http.ResponseWriter, r *http.Request) {
	var req stillRequest
	if !decodeJSON(w, r, &req, "still request") {
		return
	}
	if !recipe.IsHash(req.Src) {
		writeError(w, http.StatusBadRequest, "src must be a source hash")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), stillTimeout)
	defer cancel()
	png, err := s.jm.Still(ctx, req.Src, req.Ops, req.Output, req.T, req.MaxW)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, errText(err))
		case errors.Is(err, jobs.ErrInvalidRecipe):
			writeError(w, http.StatusBadRequest, errText(err))
		case errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "still render timed out")
		case errors.Is(err, context.Canceled):
			return // client went away
		default:
			writeError(w, http.StatusInternalServerError, errText(err))
		}
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", fmt.Sprint(len(png)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// ---- jobs ---------------------------------------------------------------------

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown() {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "server is shutting down; retry shortly")
		return
	}
	var rec recipe.Recipe
	if !decodeJSON(w, r, &rec, "recipe JSON") {
		return
	}
	if err := rec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, errText(err))
		return
	}
	for i, h := range rec.Sources {
		blob, err := s.st.GetBlob(h)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("source %d (%s…) is not uploaded", i, h[:12]))
				return
			}
			writeError(w, http.StatusInternalServerError, errText(err))
			return
		}
		if blob.Info == nil {
			writeError(w, http.StatusConflict, fmt.Sprintf("source %d (%s…) has not been probed yet; upload it again", i, h[:12]))
			return
		}
	}
	job, err := s.jm.Submit(rec)
	if err != nil {
		if errors.Is(err, jobs.ErrInvalidRecipe) {
			writeError(w, http.StatusBadRequest, errText(err))
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	s.watchJob(job)
	writeJSON(w, http.StatusAccepted, job)
}

// watchJob ties an accepted job to the server lifetime: a tracked goroutine
// follows the job's events and, if Shutdown begins first, cancels the job
// and keeps waiting until the manager closes the subscription — which it
// does only after the pipeline goroutine has returned, i.e. after its
// ffmpeg/gifsicle were killed and its scratch dir removed. Shutdown's
// bg.Wait therefore covers every render this server accepted.
func (s *Server) watchJob(job jobs.Job) {
	if job.IsFinished() {
		return // served from cache: nothing runs
	}
	ch, unsubscribe, ok := s.jm.Subscribe(job.ID)
	if !ok {
		return
	}
	tracked := s.spawn(func() {
		defer unsubscribe()
		closing := s.ctx.Done()
		for {
			select {
			case _, open := <-ch:
				if !open {
					return
				}
			case <-closing:
				closing = nil
				s.jm.Cancel(job.ID)
			}
		}
	})
	if !tracked {
		// Shutdown began between the 503 check and Submit: nothing will
		// wait for this job, so stop it before it starts any process.
		unsubscribe()
		s.jm.Cancel(job.ID)
	}
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jm.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.jm.Get(id); !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	s.jm.Cancel(id) // false when already finished: still idempotent 204
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.jm.Get(id); !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	rc := http.NewResponseController(w)
	ch, cancel, ok := s.jm.Subscribe(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		log.Printf("server: sse: response writer does not support flushing: %v", err)
		return
	}

	ticker := time.NewTicker(ssePingInterval)
	defer ticker.Stop()
	ctx := r.Context()
	// On Shutdown the job is being cancelled; keep forwarding for a short
	// grace so the client gets the terminal "cancelled" event, then end the
	// stream so it cannot hold the HTTP drain up.
	closing := s.ctx.Done()
	var grace <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-closing:
			closing = nil
			grace = time.After(sseShutdownGrace)
		case <-grace:
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				log.Printf("server: sse: encode event: %v", err)
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
				return
			}
			_ = rc.Flush()
			if ev.Type == jobs.EventDone || ev.Type == jobs.EventError {
				return
			}
		}
	}
}

// ---- results ------------------------------------------------------------------

func (s *Server) handleGetResult(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !recipe.IsHash(hash) {
		writeError(w, http.StatusNotFound, "not a recipe hash")
		return
	}
	res, err := s.jm.LoadResult(hash)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no result for this recipe")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// validResultName accepts plain file names only: no separators, no dot
// segments, printable ASCII subset.
func validResultName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleOutFile(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	name := r.PathValue("name")
	if !recipe.IsHash(hash) || !validResultName(name) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	dir := s.st.ResultDir(hash)
	if !s.st.HasResult(hash) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	full := filepath.Join(dir, name)
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	if r.URL.Query().Get("dl") == "1" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": s.downloadName(hash, name)}))
	}
	http.ServeFile(w, r, full)
}

// downloadName derives a friendly attachment name from the recipe's main
// source ("myclip.gif"); falls back to the stored file name.
func (s *Server) downloadName(hash, name string) string {
	res, err := s.jm.LoadResult(hash)
	if err != nil || len(res.Recipe.Sources) == 0 {
		return name
	}
	blob, err := s.st.GetBlob(res.Recipe.Sources[0])
	if err != nil {
		return name
	}
	base := strings.TrimSuffix(blob.Name, path.Ext(blob.Name))
	base = strings.TrimSpace(base)
	if base == "" {
		return name
	}
	return base + path.Ext(name)
}
