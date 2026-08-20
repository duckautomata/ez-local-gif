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
		"formats":        outputFormats(),
		"features":       features(),
	})
}

// outputFormats lists the recipe.Output formats this build renders, in the
// order the UI offers them.
func outputFormats() []string {
	return []string{
		recipe.FormatGIF, recipe.FormatWebP, recipe.FormatAPNG, recipe.FormatAVIF,
		recipe.FormatPNG, recipe.FormatJPEG, recipe.FormatFrames,
	}
}

// features flags the Phase 2 capabilities the SPA gates its UI on: fit-to-
// size (Output.FitBytes), image-sequence uploads (several "file" parts) and
// the GIF→GIF optimiser path.
func features() map[string]bool {
	return map[string]bool{"fit": true, "sequence": true, "optimize": true}
}

// ---- upload / sources ---------------------------------------------------------

// handleUpload streams the multipart body (see upload.go): one "file" part
// becomes a blob source, several become an image-sequence source. The
// rules for several parts are enforced while reading, so a rejected upload
// stops early and leaves nothing behind.
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

	up, err := s.readUpload(mr)
	defer up.close()
	if err != nil {
		var ue *uploadError
		if errors.As(err, &ue) {
			writeError(w, ue.Status, ue.Msg)
			return
		}
		s.uploadReadError(w, err)
		return
	}
	if !up.hasFirst {
		writeError(w, http.StatusBadRequest, "multipart body has no \"file\" field")
		return
	}
	if up.isSequence() {
		s.finishSequenceUpload(w, r, up)
		return
	}
	if up.firstSize == 0 {
		writeError(w, http.StatusBadRequest, "uploaded file is empty")
		return
	}
	blob, err := s.storeFirstPart(up)
	if err != nil {
		log.Printf("server: store upload: %v", err)
		writeError(w, http.StatusInternalServerError, "upload failed: "+errText(err))
		return
	}
	s.answerSource(w, r, blob, s.probeFile)
}

// finishSequenceUpload turns the staged parts of a multi-file upload into a
// sequence blob, probes it at the requested delay and answers with the
// source. The staged files (the first part included) never entered the blob
// store, so nothing has to be deleted from it on any path.
func (s *Server) finishSequenceUpload(w http.ResponseWriter, r *http.Request, up *upload) {
	parts, closeParts, err := up.sequenceParts()
	if err != nil {
		log.Printf("server: upload sequence: %v", err)
		writeError(w, http.StatusInternalServerError, "upload failed: "+errText(err))
		return
	}
	blob, err := s.st.PutSequence(parts)
	closeParts()
	if err != nil {
		if errors.Is(err, store.ErrMixedSequence) {
			writeError(w, http.StatusBadRequest, errText(err))
			return
		}
		log.Printf("server: store image sequence: %v", err)
		writeError(w, http.StatusInternalServerError, "storing the image sequence failed: "+errText(err))
		return
	}
	s.answerSource(w, r, blob, func(ctx context.Context, b *store.Blob) (recipe.ProbeInfo, error) {
		return probe.ProbeSequence(ctx, s.tools, b.Path, up.delayMS)
	})
}

// prober describes one blob (a file or a sequence dir) for answerSource.
type prober func(ctx context.Context, b *store.Blob) (recipe.ProbeInfo, error)

// probeFile is the prober for ordinary (single-file) blobs.
func (s *Server) probeFile(ctx context.Context, b *store.Blob) (recipe.ProbeInfo, error) {
	return probe.Probe(ctx, s.tools, b.Path, probeScanFrames)
}

// answerSource completes every path that makes a blob a source: a blob
// without probe info is probed (under probeTimeout) and the info stored,
// then the source is written. Probe failures go through probeFailed.
func (s *Server) answerSource(w http.ResponseWriter, r *http.Request, blob *store.Blob, pr prober) {
	if blob.Info == nil {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		info, err := pr(ctx, blob)
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
// was not the JSON it prints for anything it can open, it found no video
// stream, or the stream it found has no dimensions (garbage behind an image
// extension: the image2 demuxer trusts the name and the decoder gives up) —
// as opposed to ffprobe itself failing to run.
func unreadableSource(err error) bool {
	var (
		exit   *exec.ExitError
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	return errors.Is(err, probe.ErrNoVideo) ||
		errors.As(err, &exit) ||
		errors.As(err, &syntax) ||
		errors.As(err, &typ) ||
		strings.Contains(err.Error(), noDimensionsMarker)
}

// noDimensionsMarker is the phrase package probe uses for a video stream
// whose width or height is 0 ("video stream has no dimensions (0x0)",
// "frame 1 has no dimensions (0x0)"); it has no sentinel of its own.
const noDimensionsMarker = "has no dimensions"

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

// fromResultRequest is the body of POST /api/sources/from-result.
type fromResultRequest struct {
	RecipeHash string `json:"recipeHash"`
	Name       string `json:"name"`
}

// handleSourceFromResult makes a rendered result file a source of its own
// ("edit as source"): the named file of the result — one the manifest lists,
// not the manifest or report — is copied into the blob store under its
// result file name, probed like an upload and answered as a recipe.Source.
// A file that is already a blob dedupes, so chaining is free.
func (s *Server) handleSourceFromResult(w http.ResponseWriter, r *http.Request) {
	var req fromResultRequest
	if !decodeJSON(w, r, &req, "from-result request") {
		return
	}
	if !recipe.IsHash(req.RecipeHash) {
		writeError(w, http.StatusBadRequest, "recipeHash must be a recipe hash")
		return
	}
	if !validResultName(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be a plain result file name")
		return
	}
	res, err := s.jm.LoadResult(req.RecipeHash)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no result for this recipe")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	file := resultFile(res, req.Name)
	if file == nil {
		writeError(w, http.StatusNotFound, "no such file in this result")
		return
	}
	if file.Kind == jobs.FileKindArchive || strings.EqualFold(path.Ext(req.Name), ".zip") {
		writeError(w, http.StatusBadRequest, "an archive cannot be used as a source; pick a frame or an output file")
		return
	}
	full := filepath.Join(s.st.ResultDir(req.RecipeHash), req.Name)
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "result file is missing")
			return
		}
		writeError(w, http.StatusInternalServerError, errText(err))
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "result file is missing")
		return
	}
	blob, err := s.st.PutBlob(f, req.Name)
	if err != nil {
		log.Printf("server: from-result %s/%s: %v", req.RecipeHash, req.Name, err)
		writeError(w, http.StatusInternalServerError, "copying the result into the store failed: "+errText(err))
		return
	}
	if blob.Size == 0 {
		s.discardBlob(blob.Hash)
		writeError(w, http.StatusUnprocessableEntity, "result file is empty")
		return
	}
	s.answerSource(w, r, blob, s.probeFile)
}

// resultFile returns the manifest entry named name, or nil.
func resultFile(res *jobs.Result, name string) *jobs.File {
	for i := range res.Files {
		if res.Files[i].Name == name {
			return &res.Files[i]
		}
	}
	return nil
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
	if ct := resultContentType(name); ct != "" {
		h.Set("Content-Type", ct)
	}
	if r.URL.Query().Get("dl") == "1" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": s.downloadName(hash, name)}))
	}
	http.ServeFile(w, r, full)
}

// resultContentTypes pins the Content-Type of every kind of result file
// rather than leaving it to the host's mime tables (Windows calls a zip
// "application/x-zip-compressed", a slim container image has no
// /etc/mime.types and Go's built-in table knows neither .zip nor .apng).
var resultContentTypes = map[string]string{
	".gif":  "image/gif",
	".webp": "image/webp",
	".png":  "image/png",
	".apng": "image/apng",
	".avif": "image/avif",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".zip":  "application/zip",
	".json": "application/json; charset=utf-8",
}

// resultContentType returns the Content-Type for a result file name, or ""
// when the extension is not one the pipeline writes (http.ServeFile then
// decides).
func resultContentType(name string) string {
	return resultContentTypes[strings.ToLower(path.Ext(name))]
}

// downloadName derives a friendly attachment name from the recipe's main
// source. Only the primary output is named after the source alone
// ("myclip.gif"); every other file keeps its own stem behind the source so
// sibling downloads stay distinct: "myclip-f00012.png" for an extracted
// frame, "myclip-alt1.gif" for a fit-search alternative, "myclip-frames.zip"
// for the frame archive and "myclip-delays.json" / "myclip-report.json" for
// sidecars the manifest does not list. Falls back to the stored file name
// when the source is unknown.
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
	for _, f := range res.Files {
		if f.Name == name {
			if f.Kind == "" || f.Kind == jobs.FileKindOutput {
				return base + path.Ext(name)
			}
			break
		}
	}
	return base + "-" + name
}
