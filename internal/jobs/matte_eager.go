package jobs

import "context"

// Phase 5b: how the HTTP layer tells a preview that the user asked for the
// matte pass explicitly (docs/background-removal-proposal.md §4.3, §6.2).
// A still only STARTS a pass when its estimate is under the eager bound
// (matteEagerSeconds); above it the still answers ErrMattePending with State
// MattePendingDeferred and the SPA offers "Compute now". Play, "Compute now"
// and Render always start the pass: the server sets previewRequest.eager on
// the request's ctx with WithMatteEager, and the pass resolver reads it with
// MatteEager. A context value rather than a parameter so StillSources /
// Proxy keep their signatures (the warm-up still, tests and the batch path
// call them too); renders never consult it — a render is eager by definition.

// matteEagerKey is the context key WithMatteEager stores under.
type matteEagerKey struct{}

// WithMatteEager returns a ctx that marks (eager true) or unmarks (false)
// the preview request it carries as eager: under an eager ctx a still or
// proxy starts a matte pass whatever its estimate, instead of deferring a
// pass over the eager bound to Play / "Compute now" / Render. The server
// derives it from previewRequest.eager (POST /api/still, /api/proxy).
func WithMatteEager(ctx context.Context, eager bool) context.Context {
	return context.WithValue(ctx, matteEagerKey{}, eager)
}

// MatteEager reports whether ctx was marked eager by WithMatteEager (false
// for a nil or unmarked ctx).
func MatteEager(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	eager, _ := ctx.Value(matteEagerKey{}).(bool)
	return eager
}
