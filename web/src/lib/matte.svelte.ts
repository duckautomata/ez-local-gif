// The live state of the AI matte sidecar (Phase 5b): the app's last probe,
// GET /api/matte, in a $state the Background card (model select, status
// line) and the Render panel (estimate suffix, caps note) read. Unlike the
// capabilities flags it is NOT fetched once per page: the model states
// change (loading → ready, a download's percentage, the sidecar going
// away), so it is polled every MATTE_POLL_MS while something shows it —
// the AI mode of the Background card, the Render panel with AI on, a
// preview whose answer is 202 — through reference-counted holds
// (holdMattePolling), and every 202 answer installs the status object it
// carries (notePending). A failed fetch keeps the last status and records
// the error; an older server without the endpoint (404) leaves it unknown
// and the mode greyed by the capabilities flag alone.

import { getMatte, messageOf, type MattePending, type MatteStatus } from './api';
import { MATTE_POLL_MS } from './matte';

export const matte = $state({
  /** GET /api/matte (or a 202) has answered at least once: `status` is the server's */
  loaded: false,
  status: null as MatteStatus | null,
  /** the last fetch failure ('' = none); the previous status stays on screen */
  error: '',
});

export type MatteFetcher = (signal?: AbortSignal) => Promise<MatteStatus>;

/** setMatteStatus installs an answer (null = back to unknown). */
export function setMatteStatus(s: MatteStatus | null): void {
  matte.status = s;
  matte.loaded = s !== null;
  if (s) matte.error = '';
}

/** notePending installs the status object a 202 preview answer carried — as fresh as a poll. */
export function notePending(p: MattePending): void {
  setMatteStatus(p.status);
}

let inFlight: Promise<void> | null = null;

/**
 * refreshMatte fetches /api/matte once (a call while one is in flight joins
 * it). Never rejects: a failure sets matte.error and keeps the last status.
 */
export function refreshMatte(fetcher: MatteFetcher = getMatte): Promise<void> {
  if (inFlight) return inFlight;
  const p = fetcher()
    .then((s) => setMatteStatus(s))
    .catch((e) => {
      matte.error = messageOf(e);
    })
    .finally(() => {
      inFlight = null;
    });
  inFlight = p;
  return p;
}

let holds = 0;
let timer: ReturnType<typeof setInterval> | undefined;
let pollFetcher: MatteFetcher = getMatte;
let pollMs = MATTE_POLL_MS;

export interface MattePollOptions {
  /** the fetcher the poll uses (tests); set by the first hold that names one */
  fetch?: MatteFetcher;
  /** the cadence (tests); set by the first hold that names one */
  intervalMs?: number;
}

/**
 * holdMattePolling asks for the status to be kept live: the first hold
 * fetches at once and starts the MATTE_POLL_MS interval, the last release
 * stops it. Returns the release (idempotent). Components hold it from an
 * $effect whose cleanup releases, so the poll runs exactly while the AI
 * mode is visible or a preview is pending.
 */
export function holdMattePolling(opts: MattePollOptions = {}): () => void {
  if (opts.fetch) pollFetcher = opts.fetch;
  if (opts.intervalMs && opts.intervalMs > 0) pollMs = opts.intervalMs;
  holds++;
  if (holds === 1) {
    void refreshMatte(pollFetcher);
    timer = setInterval(() => void refreshMatte(pollFetcher), pollMs);
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    holds--;
    if (holds === 0 && timer !== undefined) {
      clearInterval(timer);
      timer = undefined;
    }
  };
}

/** mattePolling reports whether a poll is running (at least one hold). */
export function mattePolling(): boolean {
  return holds > 0;
}

/** A hold on the poll that follows a boolean: see pendingHold. */
export interface PendingHold {
  /** holds the poll when `pending` is true (idempotent while it stays true), releases it when false */
  update(pending: boolean): void;
  /** releases the hold if any (idempotent) */
  release(): void;
}

/**
 * pendingHold is the hold Preview keeps while a still or Play answer is
 * pending: `update(true)` takes ONE hold and keeps it through any number of
 * further `update(true)` calls, `update(false)` releases it. The
 * idempotence is the point — the schedulers (still.ts / proxy.ts) assign a
 * NEW pending object on every 202 answer, so an effect written as
 * `if (view.pending) return holdMattePolling()` was torn down and re-run
 * per answer: release → hold, the interval cleared and restarted and an
 * extra /api/matte fetch every ~500 ms while a pass ran. Here three
 * consecutive 202s are one hold and one fetch, whatever re-runs the update.
 */
export function pendingHold(): PendingHold {
  let release: (() => void) | null = null;
  return {
    update(pending) {
      if (pending && release === null) release = holdMattePolling();
      else if (!pending && release !== null) {
        release();
        release = null;
      }
    },
    release() {
      release?.();
      release = null;
    },
  };
}

/**
 * pollWhilePending keeps the poll alive exactly while `pending()` is true
 * (a pendingHold driven from a $effect, released when the component goes
 * away). Call it from a component's script.
 */
export function pollWhilePending(pending: () => boolean): void {
  const hold = pendingHold();
  $effect(() => {
    hold.update(pending());
  });
  $effect(() => () => hold.release());
}

/** resetMatte forgets everything and stops the poll (tests). */
export function resetMatte(): void {
  holds = 0;
  if (timer !== undefined) clearInterval(timer);
  timer = undefined;
  pollFetcher = getMatte;
  pollMs = MATTE_POLL_MS;
  inFlight = null;
  setMatteStatus(null);
  matte.error = '';
}
