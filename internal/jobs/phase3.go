package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/enc"
	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/graph"
	"github.com/duckautomata/ez-local-gif/internal/recipe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Phase 3 (DESIGN.md §4.3): editing ops need three things the Phase 2
// pipeline did not have —
//
//  1. multi-source recipes: render/Still/Proxy look up every
//     recipe.Sources[i] blob (resolveSources), compile with
//     graph.CompileWithSources (probe infos in recipe order), fill
//     graph.Plan.ExtraInputs[i].Path from the store, write each
//     graph.Plan.TextFiles entry to the job scratch dir and bind it
//     (graph.BindTextFiles) before any enc.* builder sees the plan
//     (bindTextFiles). Overlay sources must be single files: an image
//     sequence is an image2 pattern with its own timing and cannot be a
//     second -i, so it is refused with an ErrInvalidRecipe message;
//  2. autocrop resolution: every recipe.OpAutoCrop is replaced, before
//     compiling, by a copy whose Resolved crop comes from ResolveAutoCrop
//     (autocrop.go) — detected on the keyed, trimmed picture the stack's
//     detection ops produce (wherever they sit: the compiler hoists them in
//     front of the geometry) and memoised per (main source, those ops,
//     threshold, padding) so stills, proxies and renders agree and the
//     detection pass runs once per source;
//  3. the animated preview (Proxy, proxy.go) behind the UI's Play button.
//
// The result key (ResultKey) already covers multi-source recipes because
// recipe.Canonical hashes Sources and Ops; overlay assets are ordinary blobs.
// A Resolved crop a client sends along is stripped before hashing
// (stripAutoCropResolved): jobs always resolves it itself, so the key must
// not depend on it.

// ErrNotImplemented3 is kept for API compatibility with the Phase 3 stubs;
// nothing in this package returns it any more.
var ErrNotImplemented3 = errors.New("jobs: phase 3 not implemented")

// Phase 3 tunables.
const (
	// fontsTimeout bounds the one-off fc-list run behind Fonts.
	fontsTimeout = 15 * time.Second
	// textFilePrefix names the drawtext body files written to a scratch dir:
	// t1.txt, t2.txt, … in graph.Plan.TextFiles order.
	textFilePrefix = "t"
)

// sources is a recipe's source list resolved against the store: blobs[i]
// belongs to hashes[i], every blob has probe info and was touched
// (lookupSources), and every overlay source (i >= 1) is a single file.
type sources struct {
	hashes []string
	blobs  []*store.Blob
}

// resolveSources looks every source up (lookupSources: missing blobs and
// blobs without probe info are ErrInvalidRecipe) and refuses image
// sequences in overlay positions.
func (m *Manager) resolveSources(hashes []string) (*sources, error) {
	if len(hashes) == 0 {
		return nil, fmt.Errorf("%w: no sources", ErrInvalidRecipe)
	}
	blobs, err := m.lookupSources(hashes)
	if err != nil {
		return nil, err
	}
	for i, b := range blobs[1:] {
		if b.IsSequence() {
			return nil, fmt.Errorf("%w: source %d (%s) is an image sequence and cannot be an overlay; render it to an animated image first and use that as the overlay", ErrInvalidRecipe, i+1, short(hashes[i+1]))
		}
	}
	return &sources{hashes: hashes, blobs: blobs}, nil
}

// main is the recipe's main source.
func (s *sources) main() *store.Blob { return s.blobs[0] }

// infos returns the probe infos in recipe order, as CompileWithSources
// wants them.
func (s *sources) infos() []recipe.ProbeInfo {
	infos := make([]recipe.ProbeInfo, len(s.blobs))
	for i, b := range s.blobs {
		infos[i] = *b.Info
	}
	return infos
}

// fillExtraInputs points every overlay input of p at its blob file.
func (s *sources) fillExtraInputs(p *graph.Plan) error {
	for i := range p.ExtraInputs {
		in := &p.ExtraInputs[i]
		if in.Source < 1 || in.Source >= len(s.blobs) {
			return fmt.Errorf("compiled plan references source %d, but the recipe has %d", in.Source, len(s.blobs))
		}
		in.Path = s.blobs[in.Source].Path
	}
	return nil
}

// compile resolves every autocrop op against the main source, compiles the
// op stack against all sources and fills the overlay input paths. The
// returned plan's TextFiles are still unbound: bindTextFiles writes them
// once a scratch directory exists. Compile errors are the client's
// (ErrInvalidRecipe).
func (m *Manager) compile(ctx context.Context, s *sources, ops []recipe.Op, out recipe.Output) (*graph.Plan, error) {
	ops, err := m.resolveAutoCrop(ctx, s.main(), ops)
	if err != nil {
		return nil, err
	}
	plan, err := graph.CompileWithSources(s.infos(), ops, out)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	if plan.Width <= 0 || plan.Height <= 0 {
		return nil, fmt.Errorf("compiled plan has an empty frame size (%dx%d)", plan.Width, plan.Height)
	}
	if err := s.fillExtraInputs(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// bindTextFiles writes every deferred drawtext body of p under dir (t1.txt,
// t2.txt, … — UTF-8, byte for byte: drawtext reads the file verbatim, so
// nothing is trimmed or escaped here) and returns a copy of p with the paths
// bound into its filter (graph.BindTextFiles). A plan without text files is
// returned as is.
func bindTextFiles(p *graph.Plan, dir string) (*graph.Plan, error) {
	if len(p.TextFiles) == 0 {
		return p, nil
	}
	paths := make([]string, len(p.TextFiles))
	for i, tf := range p.TextFiles {
		path := filepath.Join(dir, fmt.Sprintf("%s%d.txt", textFilePrefix, i+1))
		if err := os.WriteFile(path, []byte(tf.Content), 0o644); err != nil {
			return nil, fmt.Errorf("write text overlay %d: %w", i+1, err)
		}
		paths[i] = path
	}
	bound, err := graph.BindTextFiles(p, paths)
	if err != nil {
		return nil, fmt.Errorf("bind text overlays: %w", err)
	}
	return bound, nil
}

// stripAutoCropResolved returns ops with the Resolved box removed from every
// autocrop op that carries one (jobs resolves it itself; a client-supplied
// box is documented as ignored and must not change the recipe hash). The
// slice is returned unchanged when nothing was stripped; otherwise a copy.
// Malformed params are left alone for the compiler to report.
func stripAutoCropResolved(ops []recipe.Op) []recipe.Op {
	var out []recipe.Op
	for i, op := range ops {
		if op.Kind != recipe.OpAutoCrop || !bytes.Contains(op.Params, []byte("resolved")) {
			continue
		}
		var p recipe.AutoCropParams
		if err := json.Unmarshal(op.Params, &p); err != nil || p.Resolved == nil {
			continue
		}
		p.Resolved = nil
		raw, err := json.Marshal(p)
		if err != nil {
			continue
		}
		if out == nil {
			out = slices.Clone(ops)
		}
		out[i] = recipe.Op{Kind: op.Kind, Params: raw}
	}
	if out == nil {
		return ops
	}
	return out
}

// Fonts returns the drawtext-usable font faces of the container (fc-list via
// enc.FcListArgs/ParseFcList), cached for the manager's lifetime; empty when
// fc-list is not available. A failed fc-list run is logged and retried on
// the next call rather than cached as "no fonts".
func (m *Manager) Fonts(ctx context.Context) []enc.Font {
	m.fontsMu.Lock()
	defer m.fontsMu.Unlock()
	if m.fontsDone {
		return slices.Clone(m.fonts)
	}
	if m.tools.FcList == "" {
		m.fontsDone = true
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, fontsTimeout)
	defer cancel()
	out, err := ffrun.RunOutput(ctx, m.tools.FcList, enc.FcListArgs())
	if err != nil {
		log.Printf("jobs: fc-list: %v", err)
		return nil
	}
	m.fonts = enc.ParseFcList(string(out))
	m.fontsDone = true
	return slices.Clone(m.fonts)
}
