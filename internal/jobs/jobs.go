// Package jobs runs recipes: probe → graph.Compile → master render → encoder
// tails → discordlint → verify → results dir. It keeps an in-memory job
// table with SSE-style event subscription. There is no queue broker: a
// semaphore bounds concurrent encodes.
//
// Render pipeline (Phase 1):
//  1. Look up sources in the store (probe info must exist); touch them so
//     the sweeper's TTL counts from this use.
//  2. plan := graph.Compile(mainInfo, ops, output). Estimate the RGBA
//     master (plan.Frames*W*H*4): refuse renders over Options.MaxMasterBytes
//     or larger than the scratch filesystem up-front, otherwise reserve the
//     estimate from the scratch budget (waiting for concurrent renders when
//     needed) — see scratch.go.
//  3. If store.HasResult(hash) → done immediately with the existing manifest.
//  4. scratch := store.ScratchDir(jobID); render the master with
//     enc.MasterArgs + ffrun.RunFFmpeg (progress → Percent, using
//     plan.Frames or plan.Duration for the denominator); scan alpha; fill
//     enc.Master.
//  5. Encode per output.Format: gif → enc.GIFArgs then enc.GifsicleArgs
//     (with output.Loop restated as --loopcount); webp → enc.WebPArgs.
//     (Phase 2 adds fit search / more formats.)
//  6. discordlint.LintGIF(fix=true) / LintWebP with output.Target; write the
//     fixed bytes; if a LevelError check remains for gif, retry once through
//     the gifsicle fallback ladder (--colors → -U -O2 → -U). Report.HasAlpha
//     is then overridden with the master's pixel alpha scan (the linter's
//     flag is structural and over-reports on frame-diff optimised opaque
//     animations; the structural verdict stays in a render.alpha info check
//     when they differ).
//  7. Verify: enc.VerifyDecodeArgs must succeed.
//  8. Write manifest.json (Result) into staging, store.CommitResult, publish
//     done.
package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/discordlint"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// ErrNotImplemented is kept for API compatibility with the Phase-1 stubs;
// nothing in this package returns it any more.
var ErrNotImplemented = errors.New("jobs: not implemented")

// ErrInvalidRecipe wraps client-side mistakes (bad recipe, unknown source,
// uncompilable ops) so the HTTP layer can answer 4xx instead of 5xx.
var ErrInvalidRecipe = errors.New("jobs: invalid recipe")

// ErrNotFound is returned by Get-like operations for unknown ids/hashes.
var ErrNotFound = errors.New("jobs: not found")

// State of a job.
type State string

const (
	StateQueued  State = "queued"
	StateRunning State = "running"
	StateDone    State = "done"
	StateError   State = "error"
)

// Stage names, in pipeline order.
const (
	StageProbe  = "probe"
	StageMaster = "master"
	StageEncode = "encode"
	StageLint   = "lint"
	StageVerify = "verify"
	StageDone   = "done"
)

// Event types.
const (
	EventProgress = "progress"
	EventDone     = "done"
	EventError    = "error"
)

// File is one produced output.
type File struct {
	Name     string              `json:"name"`   // file name inside the result dir
	URL      string              `json:"url"`    // /out/<recipeHash>/<name>
	Format   string              `json:"format"` // gif, webp, ...
	Bytes    int64               `json:"bytes"`
	Width    int                 `json:"width"`
	Height   int                 `json:"height"`
	Frames   int                 `json:"frames"`
	FPS      float64             `json:"fps"`
	Duration float64             `json:"duration"` // seconds
	Limit    int64               `json:"limit"`    // byte cap for the recipe's Discord target (0 = none)
	Report   *discordlint.Report `json:"report,omitempty"`
}

// Result is the manifest written to the result dir.
type Result struct {
	RecipeHash string            `json:"recipeHash"`
	Recipe     recipe.Recipe     `json:"recipe"`
	Files      []File            `json:"files"`
	Created    time.Time         `json:"created"`
	RenderMS   int64             `json:"renderMs"` // wall time of the render (0 if served from cache)
	Cached     bool              `json:"cached"`
	Tools      map[string]string `json:"tools,omitempty"` // tool versions used
}

// Job is the public view of a job (safe to JSON-encode; a snapshot).
type Job struct {
	ID         string        `json:"id"`
	RecipeHash string        `json:"recipeHash"`
	Recipe     recipe.Recipe `json:"recipe"`
	State      State         `json:"state"`
	Stage      string        `json:"stage"`   // "probe" | "master" | "encode" | "lint" | "verify" | "done"
	Percent    float64       `json:"percent"` // 0..100 within the job
	Message    string        `json:"message"` // human-readable progress line
	Result     *Result       `json:"result,omitempty"`
	Error      string        `json:"error,omitempty"`
	Created    time.Time     `json:"created"`
	Started    time.Time     `json:"started,omitempty"`
	Finished   time.Time     `json:"finished,omitempty"`
}

// IsFinished reports whether the job reached a terminal state.
func (j Job) IsFinished() bool { return j.State == StateDone || j.State == StateError }

// Event is published to subscribers on every state/progress change.
type Event struct {
	Type string `json:"type"` // "progress" | "done" | "error"
	Job  Job    `json:"job"`
}

// Options configures the manager.
type Options struct {
	Concurrency int    // max concurrent renders (0 = max(1, NumCPU/2))
	PublicBase  string // prefix for File.URL (default "/out")

	// MaxMasterBytes caps the RGBA frame master of one render (frames x
	// width x height x 4, estimated from the compiled plan before ffmpeg
	// starts). A render that would exceed it fails up-front with an
	// ErrInvalidRecipe error asking to trim / lower the fps / resize.
	// 0 = DefaultMaxMasterBytes (2 GiB, DESIGN.md §4.1). Wire to
	// EZLG_MAX_MASTER_BYTES.
	MaxMasterBytes int64

	// ScratchBudgetBytes bounds the sum of the master estimates (plus
	// encoder headroom) of concurrently running renders; a render whose
	// estimate does not fit waits for others to finish (cancellable), one
	// that could never fit fails up-front. 0 = the size of the scratch
	// filesystem when the platform can tell (the tmpfs shm_size), else
	// unlimited; < 0 = unlimited.
	ScratchBudgetBytes int64
}

// Tunables.
const (
	// DefaultStillWidth is the Still() maxW when the caller passes 0.
	DefaultStillWidth = 480
	// MaxStills bounds the still-frame memo directory (oldest evicted).
	MaxStills = 500
	// stillsDir is the memo directory name under Scratch.
	stillsDir = "stills"

	// subscriberBuffer is the per-subscriber event buffer; when full the
	// oldest event is dropped so a slow client never stalls the render.
	subscriberBuffer = 32
	// progressInterval coalesces progress events to at most 10/s.
	progressInterval = 100 * time.Millisecond

	// Finished jobs are pruned from the table after this age once the table
	// grows past pruneThreshold entries.
	pruneAge       = time.Hour
	pruneThreshold = 200

	// versionsTimeout bounds the one-off tool version probe.
	versionsTimeout = 10 * time.Second
)

// Percent milestones per stage (0..100); the probe stage sits at 0.
const (
	pctMasterStart = 2.0
	pctMasterEnd   = 60.0
	pctEncodeStart = 60.0
	pctEncodeEnd   = 85.0
	pctLint        = 88.0
	pctVerify      = 92.0
	pctCommit      = 98.0
)

// ffmpegPrefix is what ffrun.RunFFmpeg prepends; ffrun.RunOutput does not,
// so stdout-producing invocations (stills) add it themselves.
var ffmpegPrefix = []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error"}

// subscriber is one Subscribe() listener.
type subscriber struct {
	ch     chan Event
	closed bool
}

// job is the manager's mutable record; all fields are guarded by Manager.mu
// except cancel, which is immutable after creation.
type job struct {
	snap    Job
	cancel  context.CancelFunc
	subs    map[int]*subscriber
	nextSub int
	lastPub time.Time
}

// Manager owns the job table.
type Manager struct {
	st      *store.Store
	tools   ffrun.Tools
	opts    Options
	sem     chan struct{}
	scratch *byteBudget // scratch admission (see scratch.go)

	mu   sync.Mutex
	jobs map[string]*job

	stillMu sync.Mutex // serialises memo eviction

	versionsOnce sync.Once
	versions     map[string]string
}

// NewManager wires the store, tools and options.
func NewManager(st *store.Store, tools ffrun.Tools, opts Options) *Manager {
	if opts.Concurrency <= 0 {
		opts.Concurrency = max(1, runtime.NumCPU()/2)
	}
	if opts.PublicBase == "" {
		opts.PublicBase = "/out"
	}
	opts.PublicBase = strings.TrimRight(opts.PublicBase, "/")
	if opts.MaxMasterBytes <= 0 {
		opts.MaxMasterBytes = DefaultMaxMasterBytes
	}
	budget := opts.ScratchBudgetBytes
	switch {
	case budget < 0:
		budget = 0 // unlimited
	case budget == 0:
		if total, ok := st.ScratchTotal(); ok {
			budget = total
		}
	}
	m := &Manager{
		st:      st,
		tools:   tools,
		opts:    opts,
		sem:     make(chan struct{}, opts.Concurrency),
		scratch: newByteBudget(budget),
		jobs:    make(map[string]*job),
	}
	if budget > 0 && budget < opts.MaxMasterBytes {
		log.Printf("jobs: scratch %s holds %s, less than the %s frame-master cap; larger renders will be refused up-front (raise shm_size or lower EZLG_MAX_MASTER_BYTES)",
			st.Scratch, humanBytes(budget), humanBytes(opts.MaxMasterBytes))
	}
	return m
}

// Concurrency returns the render semaphore size.
func (m *Manager) Concurrency() int { return cap(m.sem) }

// MaxMasterBytes returns the effective per-render frame-master cap.
func (m *Manager) MaxMasterBytes() int64 { return m.opts.MaxMasterBytes }

// ScratchBudgetBytes returns the effective scratch admission budget (0 =
// unlimited).
func (m *Manager) ScratchBudgetBytes() int64 { return m.scratch.Limit() }

// ToolVersions returns the (memoised) tool version map used in manifests
// and /api/capabilities.
func (m *Manager) ToolVersions() map[string]string {
	m.versionsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), versionsTimeout)
		defer cancel()
		m.versions = m.tools.Versions(ctx)
		if m.versions == nil {
			m.versions = map[string]string{}
		}
	})
	return m.versions
}

// supportedFormats lists the Phase 1 encoders.
var supportedFormats = map[string]bool{"gif": true, "webp": true}

// PipelineVersion salts the result key so that memoised outputs are only
// reused while the code that produced them is unchanged. Bump it whenever
// graph/enc/jobs change what a recipe renders to (filter text, encoder
// flags, lint fixes); discordlint.RulesVersion is folded in automatically so
// rule changes invalidate results too. Recipes themselves keep their
// content hash (recipe.Recipe.Hash) — only the on-disk result key changes.
const PipelineVersion = "2026-08-18.2"

// ResultKey is the on-disk / URL identity of a recipe's rendered result:
// sha256(recipe hash, PipelineVersion, discordlint.RulesVersion). It is what
// Job.RecipeHash and Result.RecipeHash carry.
func ResultKey(r recipe.Recipe) string {
	sum := sha256.Sum256([]byte(r.Hash() + "\n" + PipelineVersion + "\n" + discordlint.RulesVersion))
	return hex.EncodeToString(sum[:])
}

// Submit validates and enqueues r; returns the job immediately (State
// queued or, if the result is already on disk, done).
func (m *Manager) Submit(r recipe.Recipe) (Job, error) {
	if err := r.Validate(); err != nil {
		return Job{}, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	if !supportedFormats[strings.ToLower(r.Output.Format)] {
		return Job{}, fmt.Errorf("%w: unsupported output format %q (supported: gif, webp)", ErrInvalidRecipe, r.Output.Format)
	}
	if _, err := r.Canonical(); err != nil {
		return Job{}, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	hash := ResultKey(r)
	now := time.Now()

	id, err := newID()
	if err != nil {
		return Job{}, err
	}
	base := Job{ID: id, RecipeHash: hash, Recipe: r, Created: now}

	// Fast path: the result is already on disk.
	if m.st.HasResult(hash) {
		res, err := m.LoadResult(hash)
		if err == nil {
			res.Cached = true
			res.RenderMS = 0
			done := base
			done.State = StateDone
			done.Stage = StageDone
			done.Percent = 100
			done.Message = "served from cache"
			done.Result = res
			done.Started, done.Finished = now, now
			m.mu.Lock()
			m.pruneLocked(now)
			m.jobs[id] = &job{snap: done, cancel: func() {}, subs: map[int]*subscriber{}}
			m.mu.Unlock()
			return done, nil
		}
		// A result dir with an unreadable manifest is useless and would make
		// CommitResult keep it forever; drop it and re-render.
		log.Printf("jobs: result %s has a corrupt manifest (%v); re-rendering", hash, err)
		_ = os.RemoveAll(m.st.ResultDir(hash))
	}

	ctx, cancel := context.WithCancel(context.Background())
	queued := base
	queued.State = StateQueued
	queued.Stage = StageProbe
	queued.Message = "queued"
	j := &job{snap: queued, cancel: cancel, subs: map[int]*subscriber{}}

	m.mu.Lock()
	m.pruneLocked(now)
	m.jobs[id] = j
	m.mu.Unlock()

	go m.run(ctx, j)
	return queued, nil
}

// Get returns a snapshot of the job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return j.snap, true
}

// Cancel cancels a queued/running job (kills ffmpeg). It returns false when
// the job is unknown or already finished.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || j.snap.IsFinished() {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	j.cancel()
	return true
}

// Subscribe returns a channel that receives the current state immediately
// and every subsequent Event until the job finishes; call cancel to stop.
// ok is false if the job does not exist.
func (m *Manager) Subscribe(id string) (ch <-chan Event, cancel func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, found := m.jobs[id]
	if !found {
		return nil, func() {}, false
	}
	sub := &subscriber{ch: make(chan Event, subscriberBuffer)}
	sub.ch <- Event{Type: eventTypeFor(j.snap), Job: j.snap}
	if j.snap.IsFinished() {
		sub.closed = true
		close(sub.ch)
		return sub.ch, func() {}, true
	}
	key := j.nextSub
	j.nextSub++
	j.subs[key] = sub
	cancelFn := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if s, ok := j.subs[key]; ok {
			delete(j.subs, key)
			if !s.closed {
				s.closed = true
				close(s.ch)
			}
		}
	}
	return sub.ch, cancelFn, true
}

// LoadResult reads an existing manifest for recipeHash.
func (m *Manager) LoadResult(recipeHash string) (*Result, error) {
	if !recipe.IsHash(recipeHash) {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(m.st.ResultDir(recipeHash), store.ManifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("jobs: read manifest %s: %w", recipeHash, err)
	}
	res, err := decodeResult(data)
	if err != nil {
		return nil, fmt.Errorf("jobs: manifest %s: %w", recipeHash, err)
	}
	return res, nil
}

// ---- table maintenance ------------------------------------------------------

func eventTypeFor(j Job) string {
	switch j.State {
	case StateDone:
		return EventDone
	case StateError:
		return EventError
	}
	return EventProgress
}

// pruneLocked drops old finished jobs once the table is large.
func (m *Manager) pruneLocked(now time.Time) {
	if len(m.jobs) <= pruneThreshold {
		return
	}
	for id, j := range m.jobs {
		if j.snap.IsFinished() && now.Sub(j.snap.Finished) > pruneAge {
			delete(m.jobs, id)
		}
	}
}

// newID returns 16 random bytes as hex.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("jobs: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ---- publishing -------------------------------------------------------------

// update mutates the job snapshot under the lock and publishes a progress
// event, coalescing to <= 10/s unless force is set (stage/state changes and
// terminal events are always published).
func (m *Manager) update(j *job, force bool, fn func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.snap.IsFinished() {
		return
	}
	prevStage, prevState := j.snap.Stage, j.snap.State
	fn(&j.snap)
	now := time.Now()
	if !force && j.snap.Stage == prevStage && j.snap.State == prevState && now.Sub(j.lastPub) < progressInterval {
		return
	}
	j.lastPub = now
	m.publishLocked(j, eventTypeFor(j.snap))
}

// setStage moves the job to a new stage with a percent floor and message.
func (m *Manager) setStage(j *job, stage string, pct float64, msg string) {
	m.update(j, true, func(s *Job) {
		s.State = StateRunning
		s.Stage = stage
		s.Percent = pct
		s.Message = msg
	})
}

// progress reports within-stage progress (coalesced).
func (m *Manager) progress(j *job, pct float64, msg string) {
	m.update(j, false, func(s *Job) {
		if pct > s.Percent {
			s.Percent = pct
		}
		s.Message = msg
	})
}

// publishLocked fans an event out to every subscriber without blocking:
// a full buffer drops its oldest event first.
func (m *Manager) publishLocked(j *job, typ string) {
	ev := Event{Type: typ, Job: j.snap}
	for _, s := range j.subs {
		if s.closed {
			continue
		}
		select {
		case s.ch <- ev:
			continue
		default:
		}
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- ev:
		default:
		}
	}
}

// finish marks the job terminal (done or error), publishes the final event
// and closes every subscriber.
func (m *Manager) finish(j *job, fn func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.snap.IsFinished() {
		return
	}
	fn(&j.snap)
	j.snap.Finished = time.Now()
	m.publishLocked(j, eventTypeFor(j.snap))
	for key, s := range j.subs {
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
		delete(j.subs, key)
	}
	j.cancel() // release the context
}

func (m *Manager) fail(j *job, err error) {
	m.finish(j, func(s *Job) {
		s.State = StateError
		s.Error = err.Error()
		s.Message = "failed: " + err.Error()
	})
}

func (m *Manager) succeed(j *job, res *Result) {
	m.finish(j, func(s *Job) {
		s.State = StateDone
		s.Stage = StageDone
		s.Percent = 100
		s.Message = "done"
		s.Result = res
	})
}
