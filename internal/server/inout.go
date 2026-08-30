package server

// Phase 4 file exchange (DESIGN.md §4.4): the /input picker and the /output
// save. Both features live in the job manager (jobs.Options.InputDir /
// OutputDir, checked once at startup); the handlers here only map its
// results and sentinel errors onto HTTP — 503 while a feature is off (and
// when the directory behind it stopped being usable after the startup
// check, see featureUnavailable), 404 for unknown names (the traversal
// defence: a name must exactly match a listed /input entry, or a file the
// result manifest lists).

import (
	"errors"
	"io/fs"
	"log"
	"net/http"
	"syscall"

	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
)

// Operator-facing 503 messages: the same text whether the feature was off at
// startup (jobs.ErrNoInputDir / ErrNoOutputDir) or its directory stopped
// being usable afterwards — the SPA treats both as feature-off.
const (
	noInputDirMsg  = "no input directory is available (mount one and set EZLG_INPUT)"
	noOutputDirMsg = "no output directory is available (mount one and set EZLG_OUTPUT)"
)

// featureUnavailable reports whether an /input listing or /output save
// failed because the directory behind the feature stopped being usable
// after the startup check: the bind mount vanished (ENOENT), permissions
// were lost (EACCES/EPERM) or the disk filled up (ENOSPC — a NAS-backed
// ./output going away or filling mid-session). Those failures answer the
// same 503 the startup check would have produced; the raw os error — which
// names container filesystem paths — goes to the server log only.
func featureUnavailable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.ENOSPC)
}

// handleListInput answers GET /api/input with {"files": [{name,size,mtime}]}
// — the pickable files of the mounted input dir (jobs.Manager.ListInput:
// flat, decodable extensions only, name-sorted, capped at
// jobs.MaxInputFiles). The listing is read per request, so files dropped
// into /input show up without a restart — hence no caching. 503 when no
// usable input dir was found at startup (capabilities features.inputPick
// false), and — with the same message — when the dir stopped being usable
// afterwards (featureUnavailable).
func (s *Server) handleListInput(w http.ResponseWriter, r *http.Request) {
	files, err := s.jm.ListInput()
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNoInputDir):
			writeError(w, http.StatusServiceUnavailable, noInputDirMsg)
		case featureUnavailable(err):
			log.Printf("server: list input: %v", err)
			writeError(w, http.StatusServiceUnavailable, noInputDirMsg)
		default:
			log.Printf("server: list input: %v", err)
			writeError(w, http.StatusInternalServerError, "listing the input directory failed: "+errText(err))
		}
		return
	}
	if files == nil {
		files = []jobs.InputFile{} // the JSON must be an array, never null
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// fromInputRequest is the body of POST /api/sources/from-input.
type fromInputRequest struct {
	Name string `json:"name"`
}

// handleSourceFromInput answers POST /api/sources/from-input: the named
// /input file is ingested exactly like an upload of the same bytes
// (jobs.Manager.SourceFromInput: copy into the blob store, sha256 dedupe —
// a file that was also uploaded yields the same hash) and answered as a
// recipe.Source through the usual upload tail (probe + SetBlobInfo +
// pre-warm; a file ffprobe cannot read is a 422 like any other upload).
// The name must EXACTLY match a GET /api/input entry — an unknown name or
// a traversal attempt ("../x") is a 404; 503 when the picker is off, or
// when /input stopped being usable after the startup check
// (featureUnavailable). A file over the upload size limit
// (jobs.Options.MaxUploadBytes, the same EZLG_MAX_UPLOAD_MB knob behind
// Config.MaxUploadBytes) is a 413, exactly like an upload of the same bytes
// would be.
func (s *Server) handleSourceFromInput(w http.ResponseWriter, r *http.Request) {
	var req fromInputRequest
	if !decodeJSON(w, r, &req, "from-input request") {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name (a file GET /api/input lists) is required")
		return
	}
	blob, err := s.jm.SourceFromInput(req.Name)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNoInputDir):
			writeError(w, http.StatusServiceUnavailable, noInputDirMsg)
		case errors.Is(err, jobs.ErrInputTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, errText(err))
		case errors.Is(err, jobs.ErrNotFound):
			writeError(w, http.StatusNotFound, errText(err))
		case featureUnavailable(err):
			log.Printf("server: from-input %q: %v", req.Name, err)
			writeError(w, http.StatusServiceUnavailable, noInputDirMsg)
		default:
			log.Printf("server: from-input %q: %v", req.Name, err)
			writeError(w, http.StatusInternalServerError, errText(err))
		}
		return
	}
	s.answerSource(w, r, blob, s.probeFile)
}

// saveResultRequest is the body of POST /api/results/{recipeHash}/save.
type saveResultRequest struct {
	// File is a name the result's manifest lists (required).
	File string `json:"file"`
	// Name is an optional base for the saved file (extension optional); ""
	// derives the same friendly name a ?dl=1 download gets.
	Name string `json:"name"`
}

// handleSaveResult answers POST /api/results/{recipeHash}/save: the named
// result file is copied into the mounted output dir under a collision-safe
// name (jobs.Manager.SaveResult: base.ext, base-2.ext, … — two identical
// saves return two different names, so 409 never happens) and the final
// name is returned as {"name": "..."}. 404 for an unknown result or a file
// the manifest does not list (manifest.json/report.json included); 503
// when saving is off (features.outputSave false), and — with the same
// message — when /output stopped being usable after the startup check
// (featureUnavailable: unmounted, unwritable, disk full).
func (s *Server) handleSaveResult(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !recipe.IsHash(hash) {
		writeError(w, http.StatusNotFound, "not a recipe hash")
		return
	}
	var req saveResultRequest
	if !decodeJSON(w, r, &req, "save request") {
		return
	}
	if req.File == "" {
		writeError(w, http.StatusBadRequest, "file (a name the result manifest lists) is required")
		return
	}
	name, err := s.jm.SaveResult(hash, req.File, req.Name)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNoOutputDir):
			writeError(w, http.StatusServiceUnavailable, noOutputDirMsg)
		case errors.Is(err, jobs.ErrNotFound):
			writeError(w, http.StatusNotFound, errText(err))
		case featureUnavailable(err):
			log.Printf("server: save %s/%s: %v", hash, req.File, err)
			writeError(w, http.StatusServiceUnavailable, noOutputDirMsg)
		default:
			log.Printf("server: save %s/%s: %v", hash, req.File, err)
			writeError(w, http.StatusInternalServerError, "saving the result failed: "+errText(err))
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}
