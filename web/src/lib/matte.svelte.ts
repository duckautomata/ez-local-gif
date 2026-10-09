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
//
// Phase 5c adds three things that cross components: the COMPUTE state of
// the current preview (the Preview derives it from its still's answer —
// idle / running / computed — and the Background card's Compute matte
// button shows it; the button's click reaches the Preview's scheduler
// through registerMatteCompute), the device preference (setMatteDevice:
// PUT /api/matte/settings, the answer installed as the status) and the
// unload-on-leave (aiModeChanged: a debounced POST /api/matte/unload when
// the Background card leaves the AI mode or is switched off).

import { getMatte, messageOf, putMatteSettings, unloadMatte, type MattePending, type MatteStatus } from './api';
import { MATTE_POLL_MS, MATTE_UNLOAD_DEBOUNCE_MS, NO_COMPUTE, type MatteComputeState, type MatteMemoState } from './matte';

export const matte = $state({
  /** GET /api/matte (or a 202) has answered at least once: `status` is the server's */
  loaded: false,
  status: null as MatteStatus | null,
  /** the last fetch failure ('' = none); the previous status stays on screen */
  error: '',
  /** Phase 5c: the compute state of the preview's matte (lib/matte.MatteComputeState), set by the Preview */
  compute: { ...NO_COMPUTE } as MatteComputeState,
  /** Phase 5c: a PUT /api/matte/settings is in flight (the Run on select is disabled meanwhile) */
  settingDevice: false,
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

// ---------------------------------------------------------------------------
// Phase 5c: the Compute matte button

let computeHook: (() => void) | null = null;

/**
 * registerMatteCompute installs what the Compute matte button does (the
 * Preview registers its still scheduler's computeNow while mounted; null
 * unregisters). One Preview at a time, like registerPlayToggle.
 */
export function registerMatteCompute(fn: (() => void) | null): void {
  computeHook = fn;
}

/** computeMatte presses the Compute matte button: true when a Preview was there to start the pass. */
export function computeMatte(): boolean {
  if (!computeHook) return false;
  computeHook();
  return true;
}

/** setMatteCompute publishes the preview's compute state (the Preview's effect; NO_COMPUTE when it unmounts). */
export function setMatteCompute(c: MatteComputeState): void {
  const cur = matte.compute;
  if (cur.state === c.state && cur.done === c.done && cur.total === c.total && cur.device === c.device && cur.pending === c.pending) return;
  matte.compute = { ...c };
}

// ---------------------------------------------------------------------------
// Phase 5d: which memos the server was seen to hold ("Use this frame's matte")

/**
 * matteMemo records, per lib/matte.matteMemoKey, whether the server holds
 * a memo (computed) or said it does not (idle): the Preview notes every
 * still that came back as a picture or as a 202 idle (for a guided op
 * that computed, the edge model's memo too — the gate needed it), the
 * prompt overlay notes a mask prompt's answer. The Background card's "Use
 * this frame's matte" reads the edge model's entry. Per page: a reload
 * starts unknown, which is as good as the next server answer.
 */
export const matteMemo = $state({ known: {} as Record<string, Exclude<MatteMemoState, 'unknown'>> });

/** noteMatteMemo records what the server said about a memo (an empty key is ignored). */
export function noteMatteMemo(key: string, state: Exclude<MatteMemoState, 'unknown'>): void {
  if (!key) return;
  if (matteMemo.known[key] === state) return;
  matteMemo.known[key] = state;
}

/** matteMemoState reads what is known about a memo ('unknown' for an empty key or nothing seen). */
export function matteMemoState(key: string): MatteMemoState {
  if (!key) return 'unknown';
  return matteMemo.known[key] ?? 'unknown';
}

// ---------------------------------------------------------------------------
// Phase 5c: the device preference ("Run on")

export type DeviceSetter = (device: string, signal?: AbortSignal) => Promise<MatteStatus>;

/**
 * setMatteDevice stores the server-side device preference (PUT
 * /api/matte/settings; '' = the sidecar's default) and installs the status
 * the server answers with. Resolves with '' on success, else the error
 * text (the card toasts it); never rejects. A second call while one is in
 * flight waits for the first (the select is disabled meanwhile anyway).
 */
export async function setMatteDevice(device: string, setter: DeviceSetter = putMatteSettings): Promise<string> {
  matte.settingDevice = true;
  try {
    const s = await setter(device);
    setMatteStatus(s);
    return '';
  } catch (e) {
    return messageOf(e);
  } finally {
    matte.settingDevice = false;
  }
}

// ---------------------------------------------------------------------------
// Phase 5c: unload on leaving the AI mode

export type Unloader = (signal?: AbortSignal) => Promise<void>;

let unloadTimer: ReturnType<typeof setTimeout> | undefined;
let unloader: Unloader = unloadMatte;
let unloadDelay = MATTE_UNLOAD_DEBOUNCE_MS;
/** whether the AI mode was on the last time aiModeChanged ran (so only a true → false edge unloads) */
let aiWasOn = false;

export interface UnloadOptions {
  /** the request (tests) */
  unload?: Unloader;
  /** the debounce (tests) */
  delayMs?: number;
}

/**
 * aiModeChanged is told, on every change, whether the Background card is
 * in the AI mode (enabled and mode 'ai'): a true → false edge schedules
 * POST /api/matte/unload after MATTE_UNLOAD_DEBOUNCE_MS (the sidecar
 * releases its resident models — nothing stays loaded while the user is
 * not using AI), coming back before it fires cancels it, and errors are
 * ignored (best-effort by contract). Idempotent for repeated same-value
 * calls.
 */
export function aiModeChanged(on: boolean, opts: UnloadOptions = {}): void {
  if (opts.unload) unloader = opts.unload;
  if (opts.delayMs !== undefined && opts.delayMs >= 0) unloadDelay = opts.delayMs;
  if (on) {
    cancelMatteUnload();
    aiWasOn = true;
    return;
  }
  if (!aiWasOn) return;
  aiWasOn = false;
  cancelMatteUnload();
  unloadTimer = setTimeout(() => {
    unloadTimer = undefined;
    void unloader().catch(() => undefined);
  }, unloadDelay);
}

/** cancelMatteUnload drops a scheduled unload (the AI mode came back in time). */
export function cancelMatteUnload(): void {
  if (unloadTimer !== undefined) clearTimeout(unloadTimer);
  unloadTimer = undefined;
}

/** matteUnloadScheduled reports whether an unload is waiting on its debounce (tests). */
export function matteUnloadScheduled(): boolean {
  return unloadTimer !== undefined;
}

/**
 * trackAiMode drives aiModeChanged from a component: `on()` is read in an
 * $effect (so every change reports), and the component going away counts
 * as leaving (a new source resets the card, the landing page unmounts it).
 */
export function trackAiMode(on: () => boolean): void {
  $effect(() => {
    aiModeChanged(on());
  });
  $effect(() => () => aiModeChanged(false));
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
  matte.compute = { ...NO_COMPUTE };
  matte.settingDevice = false;
  matteMemo.known = {};
  computeHook = null;
  cancelMatteUnload();
  unloader = unloadMatte;
  unloadDelay = MATTE_UNLOAD_DEBOUNCE_MS;
  aiWasOn = false;
}
