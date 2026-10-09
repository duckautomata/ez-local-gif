import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MattePending, type MattePendingBody, type MattePendingState, type StillRequest } from './api';
import { MATTE_DEFERRED_RETRY_MS, MATTE_IDLE_RETRY_MS, MATTE_RETRY_MS } from './matte';
import { CANVAS_STILL_MAXW, DECODE_ERROR, FIT_STILL_MAXW, FIT_STILL_MAXW_WIDE, StillScheduler, stillMaxW, type StillView } from './still';

describe('stillMaxW', () => {
  it('Fit requests a small still: 480 px, 720 on a wide screen', () => {
    expect(stillMaxW({ overlay: false, zoomed: false, wide: false })).toBe(FIT_STILL_MAXW);
    expect(stillMaxW({ overlay: false, zoomed: false, wide: true })).toBe(FIT_STILL_MAXW_WIDE);
    expect(FIT_STILL_MAXW).toBe(480);
    expect(FIT_STILL_MAXW_WIDE).toBe(720);
  });

  it('a fixed zoom requests the output canvas unscaled, so 1× / 2× / 4× multiply real output pixels (bug: 4× of a 2560-wide output was a stretched 480-px still)', () => {
    expect(stillMaxW({ overlay: false, zoomed: true, wide: false })).toBe(CANVAS_STILL_MAXW);
    expect(stillMaxW({ overlay: false, zoomed: true, wide: true })).toBe(CANVAS_STILL_MAXW);
    // graph.MaxDim: the server's scale=min(iw, maxW) never shrinks the still
    expect(CANVAS_STILL_MAXW).toBe(8192);
  });

  it('overlays on the stage request the canvas unscaled whatever the zoom or screen', () => {
    expect(stillMaxW({ overlay: true, zoomed: false, wide: false })).toBe(CANVAS_STILL_MAXW);
    expect(stillMaxW({ overlay: true, zoomed: false, wide: true })).toBe(CANVAS_STILL_MAXW);
    expect(stillMaxW({ overlay: true, zoomed: true, wide: true })).toBe(CANVAS_STILL_MAXW);
  });
});

// A controllable fetch: every call is recorded with its signal and settled by
// the test. It deliberately does NOT reject on abort by itself, so a test can
// simulate the browser race where a superseded request still "lands".
interface Call {
  req: StillRequest;
  signal: AbortSignal;
  resolve(): void;
  reject(err: unknown): void;
}

function harness(debounceMs = 150) {
  const calls: Call[] = [];
  const revoked: string[] = [];
  const pendings: MattePending[] = [];
  const view: StillView = { url: null, loading: false, error: '', pending: null };
  const still = new StillScheduler(view, {
    fetch(req, signal) {
      return new Promise<Blob>((resolve, reject) => {
        calls.push({ req, signal, resolve: () => resolve(new Blob([`t=${req.t}`])), reject });
      });
    },
    // The URL names the frame it shows, so assertions can read it back.
    createURL: (b) => `blob:${b.size}`,
    revokeURL: (u) => revoked.push(u),
    debounceMs,
    onPending: (p) => pendings.push(p),
  });
  return { still, view, calls, revoked, pendings };
}

/** a 202 answer (Phase 5b): the pending fields plus a minimal status object */
function pendingErr(state: MattePendingState, over: Partial<MattePendingBody> = {}): MattePending {
  return new MattePending({
    pending: 'matte',
    state,
    done: 0,
    total: 0,
    percent: 0,
    estimateMs: 0,
    device: 'cuda',
    enabled: true,
    defaultModel: 'isnet-anime',
    models: { 'isnet-anime': { label: 'Anime (fast)', state: 'ready', msPerFrame: 18 } },
    maxSeconds: 600,
    maxFrames: 3000,
    ...over,
  });
}

function req(t: number, extra: Partial<StillRequest> = {}): StillRequest {
  return { src: 'h', ops: [], output: { format: 'gif' }, t, maxW: 480, ...extra };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

/** display(t): request t, let the debounce elapse and the fetch succeed. */
async function display(h: ReturnType<typeof harness>, t: number): Promise<Call> {
  h.still.request(req(t));
  await vi.advanceTimersByTimeAsync(150);
  const call = h.calls[h.calls.length - 1];
  expect(call.req.t).toBe(t);
  call.resolve();
  await flush();
  return call;
}

const A = req(0);
const B = req(0.5);

describe('StillScheduler', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('debounces, then fetches once and shows the still', async () => {
    const h = harness();
    h.still.request(A);
    await vi.advanceTimersByTimeAsync(149);
    expect(h.calls).toHaveLength(0);
    expect(h.view.loading).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.calls).toHaveLength(1);
    expect(h.calls[0].req).toEqual(A);
    expect(h.view.loading).toBe(true);
    h.calls[0].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '', pending: null });
    expect(h.still.displayedKey).toBe(StillScheduler.key(A));
  });

  it('a newer request aborts the in-flight one; a late result of the old one is ignored', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    const b = h.calls[1];
    h.still.request(req(0.75));
    await vi.advanceTimersByTimeAsync(150);
    expect(b.signal.aborted).toBe(true);
    const c = h.calls[2];
    c.resolve();
    await flush();
    expect(h.view.url).toBe('blob:6'); // t=0.75
    b.resolve(); // lands after being superseded
    await flush();
    expect(h.view.url).toBe('blob:6');
    expect(h.still.displayedKey).toBe(StillScheduler.key(req(0.75)));
  });

  // The race from the finding: A is on screen, B is in flight, the state goes
  // back to A. Nothing must be fetched — and B must not land later and swap
  // in a frame that contradicts every control.
  it('returning to the displayed state aborts the superseded in-flight request', async () => {
    const h = harness();
    await display(h, 0);
    expect(h.view.url).toBe('blob:3');

    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(2);
    const b = h.calls[1];
    expect(h.view.loading).toBe(true);

    h.still.request(A); // back to what is on screen while B is pending
    expect(b.signal.aborted).toBe(true);
    expect(h.view.loading).toBe(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.calls).toHaveLength(2); // no re-fetch of A

    b.resolve(); // B "lands" anyway (e.g. the response was already in the pipe)
    await flush();
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '', pending: null });
    expect(h.still.displayedKey).toBe(StillScheduler.key(A));
    expect(h.revoked).toEqual([]);
  });

  it('returning to the displayed state drops the error left by a superseded state', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].reject(new Error('ffmpeg exited with status 1'));
    await flush();
    expect(h.view.error).toBe('ffmpeg exited with status 1');
    expect(h.view.url).toBe('blob:3'); // A stays visible under the overlay

    h.still.request(A);
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '', pending: null });
    expect(h.calls).toHaveLength(2);
  });

  it('a decode error of the displayed still is kept when the state returns to it', async () => {
    const h = harness();
    await display(h, 0);
    h.still.imageFailed();
    expect(h.view.error).toBe(DECODE_ERROR);

    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.still.request(A);
    expect(h.calls[1].signal.aborted).toBe(true);
    expect(h.view.error).toBe(DECODE_ERROR); // the current still really is broken

    // Retry forgets both the still and the error and fetches A again.
    h.still.retry();
    expect(h.view.error).toBe('');
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(3);
    expect(h.calls[2].req).toEqual(A);
    h.calls[2].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '', pending: null });
    // the broken URL is released once the new image has loaded
    h.still.imageLoaded();
    expect(h.revoked).toEqual(['blob:3']);
  });

  it('a successful load clears a previous error', async () => {
    const h = harness();
    h.still.request(A);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[0].reject(new Error('boom'));
    await flush();
    expect(h.view).toEqual({ url: null, loading: false, error: 'boom', pending: null });
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:5', loading: false, error: '', pending: null });
  });

  it('never records an error from an aborted request', async () => {
    const h = harness();
    h.still.request(A);
    await vi.advanceTimersByTimeAsync(150);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    const a = h.calls[0];
    expect(a.signal.aborted).toBe(true);
    a.reject(new DOMException('The operation was aborted.', 'AbortError'));
    await flush();
    expect(h.view.error).toBe('');
    // ...even when the transport surfaces the abort as an ordinary error
    h.still.request(req(0.75));
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].reject(new Error('network error'));
    await flush();
    expect(h.view.error).toBe('');
    expect(h.view.loading).toBe(true); // the current request is still pending
  });

  it('revokes the previous object URL only after the next image has loaded', async () => {
    const h = harness();
    await display(h, 0);
    await display(h, 0.5);
    expect(h.view.url).toBe('blob:5');
    expect(h.revoked).toEqual([]);
    h.still.imageLoaded();
    expect(h.revoked).toEqual(['blob:3']);
  });

  it('a null request (no source) cancels everything and clears the view', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    const b = h.calls[1];
    h.still.request(null);
    expect(b.signal.aborted).toBe(true);
    expect(h.view).toEqual({ url: null, loading: false, error: '', pending: null });
    expect(h.revoked).toEqual(['blob:3']);
    expect(h.still.displayedKey).toBe('');
  });

  // The proxy plays (review W8): the still <img> is off the stage, so no
  // still may be fetched for the scrub / op changes meanwhile — each was a
  // render (full-resolution in overlay mode) whose URL sat parked until the
  // next image load — and the parked URLs are released right away.
  it('paused: requests are remembered, not fetched; parked URLs are released; resuming fetches the latest once', async () => {
    const h = harness();
    await display(h, 0);
    await display(h, 0.5); // blob:3 is parked until the next image load
    h.still.request(req(0.75));
    await vi.advanceTimersByTimeAsync(150);
    const c = h.calls[2];
    expect(h.view.loading).toBe(true);

    h.still.setPaused(true);
    expect(c.signal.aborted).toBe(true);
    expect(h.view.loading).toBe(false);
    expect(h.revoked).toEqual(['blob:3']); // released now, not at the next load
    expect(h.view.url).toBe('blob:5'); // the last still stays (shown again on Stop)
    c.resolve(); // the aborted fetch lands anyway: ignored
    await flush();
    expect(h.view.url).toBe('blob:5');

    h.still.request(req(1));
    h.still.request(req(1.25));
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.calls).toHaveLength(3); // nothing fetched while paused
    expect(h.view.loading).toBe(false);

    h.still.setPaused(false);
    expect(h.calls).toHaveLength(3); // debounced like any request
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(4);
    expect(h.calls[3].req.t).toBe(1.25); // the latest state only
    h.calls[3].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:6', loading: false, error: '', pending: null });
    expect(h.revoked).toEqual(['blob:3']); // blob:5 waits for the next load as usual
    h.still.imageLoaded();
    expect(h.revoked).toEqual(['blob:3', 'blob:5']);
  });

  it('resuming with the displayed state fetches nothing; pausing twice is harmless', async () => {
    const h = harness();
    await display(h, 0);
    h.still.setPaused(true);
    h.still.setPaused(true);
    h.still.request(B);
    h.still.request(A); // back to what is on screen
    h.still.setPaused(false);
    h.still.setPaused(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.calls).toHaveLength(1);
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '', pending: null });
    expect(h.revoked).toEqual([]);
    // a request while not paused fetches as before
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(2);
  });

  it('dispose aborts the in-flight request and releases every object URL', async () => {
    const h = harness();
    await display(h, 0);
    await display(h, 0.5); // blob:3 is waiting for the next image load
    h.still.request(req(0.75));
    await vi.advanceTimersByTimeAsync(150);
    const c = h.calls[2];
    h.still.dispose();
    expect(c.signal.aborted).toBe(true);
    expect(h.view.url).toBeNull();
    expect(h.revoked.sort()).toEqual(['blob:3', 'blob:5']);
    // a late result of the aborted request does nothing
    c.resolve();
    await flush();
    expect(h.view.url).toBeNull();
  });
});

// Phase 5b: a 202 answer (the recipe's AI matte is being computed) is not an
// error — the still on screen stays, the pending state shows and the same
// state is re-requested after MATTE_RETRY_MS (inside jobs' abandon grace)
// unless superseded; a deferred pass waits for "Compute now" (eager).
describe('StillScheduler — AI matte pending (Phase 5b)', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('a 202 keeps the still on screen, shows the pending state, hands the status over and re-requests after 500 ms', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    const b = h.calls[1];
    b.reject(pendingErr('running', { done: 24, total: 45, percent: 53, estimateMs: 810 }));
    await flush();
    expect(h.view.url).toBe('blob:3'); // the previous still stays under the pill
    expect(h.view.error).toBe('');
    expect(h.view.loading).toBe(false);
    expect(h.view.pending).toMatchObject({ state: 'running', done: 24, total: 45, percent: 53, estimateMs: 810, device: 'cuda' });
    expect(h.view.pending?.since).toBe(Date.now());
    expect(h.pendings).toHaveLength(1);
    expect(h.pendings[0].status.models?.['isnet-anime']?.msPerFrame).toBe(18);
    expect(h.still.displayedKey).toBe(StillScheduler.key(A)); // B never landed
    // the re-request: the same state, no eager, exactly at the retry delay
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS - 1);
    expect(h.calls).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.calls).toHaveLength(3);
    expect(h.calls[2].req).toEqual(B);
    expect(h.calls[2].req).not.toHaveProperty('eager');
    expect(h.view.loading).toBe(true);
    expect(h.view.pending?.state).toBe('running'); // the pill stays up through the retry
    // a later 202 updates the counts but keeps `since`
    const since = h.view.pending?.since;
    await vi.advanceTimersByTimeAsync(700);
    h.calls[2].reject(pendingErr('running', { done: 40, total: 45 }));
    await flush();
    expect(h.view.pending).toMatchObject({ done: 40, total: 45, since });
    // the picture arrives: pending cleared, the still swapped
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    h.calls[3].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:5', loading: false, error: '', pending: null });
    expect(h.still.displayedKey).toBe(StillScheduler.key(B));
  });

  it('a newer request supersedes the re-request; returning to the displayed state drops the pending state', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].reject(pendingErr('running', { done: 1, total: 45 }));
    await flush();
    expect(h.view.pending).not.toBeNull();
    h.still.request(req(0.75)); // a newer state before the retry fires
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(3);
    expect(h.calls[2].req.t).toBe(0.75);
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.calls).toHaveLength(3); // no retry of B
    h.calls[2].reject(pendingErr('running', { done: 2, total: 45 }));
    await flush();
    h.still.request(A); // back to what is on screen
    expect(h.view.pending).toBeNull();
    await vi.advanceTimersByTimeAsync(2000);
    expect(h.calls).toHaveLength(3); // nothing fetched, nothing retried
    expect(h.view.url).toBe('blob:3');
  });

  it('a deferred pass is re-requested at the poll cadence only; "Compute now" re-requests at once with eager and keeps it until the picture', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].reject(pendingErr('deferred', { estimateMs: 180_000, device: 'cpu' }));
    await flush();
    expect(h.view.pending).toMatchObject({ state: 'deferred', estimateMs: 180_000, device: 'cpu' });
    await vi.advanceTimersByTimeAsync(MATTE_DEFERRED_RETRY_MS - 1);
    expect(h.calls).toHaveLength(2); // not every 500 ms: nothing changes by itself
    await vi.advanceTimersByTimeAsync(1);
    expect(h.calls).toHaveLength(3);
    expect(h.calls[2].req).not.toHaveProperty('eager');
    h.calls[2].reject(pendingErr('deferred', { estimateMs: 180_000, device: 'cpu' }));
    await flush();
    // Compute now: at once, with eager
    h.still.computeNow();
    expect(h.calls).toHaveLength(4);
    expect(h.calls[3].req).toEqual({ ...B, eager: true });
    expect(h.calls[3].signal.aborted).toBe(false);
    h.calls[3].reject(pendingErr('running', { done: 3, total: 300, device: 'cpu' }));
    await flush();
    expect(h.view.pending?.state).toBe('running');
    // its retries carry eager too, at the running cadence…
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    expect(h.calls).toHaveLength(5);
    expect(h.calls[4].req).toEqual({ ...B, eager: true });
    h.calls[4].resolve();
    await flush();
    expect(h.view.pending).toBeNull();
    expect(h.view.url).toBe('blob:5');
    // …but a new state after the picture is plain again
    h.still.request(req(0.75));
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls[5].req).toEqual(req(0.75));
  });

  it('pausing drops the pending state and its re-request; resuming fetches the state once', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].reject(pendingErr('loading'));
    await flush();
    expect(h.view.pending?.state).toBe('loading');
    h.still.setPaused(true);
    expect(h.view.pending).toBeNull();
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(2);
    h.still.setPaused(false);
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(3);
    expect(h.calls[2].req).toEqual(B);
  });

  it('an error, a null request, retry and dispose all clear the pending state and its re-request', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].reject(pendingErr('downloading', { percent: 43 }));
    await flush();
    expect(h.view.pending).toMatchObject({ state: 'downloading', percent: 43 });
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    h.calls[2].reject(new Error('AI matte failed: sidecar unreachable'));
    await flush();
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: 'AI matte failed: sidecar unreachable', pending: null });
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(3); // an error ends the polling
    // retry re-requests and clears
    h.still.retry();
    await vi.advanceTimersByTimeAsync(150);
    h.calls[3].reject(pendingErr('running', { done: 1, total: 2 }));
    await flush();
    expect(h.view.pending).not.toBeNull();
    h.still.request(null);
    expect(h.view).toEqual({ url: null, loading: false, error: '', pending: null });
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(4);
    // dispose
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[4].reject(pendingErr('running', { done: 1, total: 2 }));
    await flush();
    h.still.dispose();
    expect(h.view.pending).toBeNull();
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(5);
  });
});

// Phase 5c: a preview never starts a pass — the server answers 202 "idle"
// until the Compute matte button (computeNow: `eager`) or a render ran
// it. Like 5b's deferred state it is re-requested at the poll cadence only.
describe('StillScheduler — idle matte (Phase 5c)', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('an idle 202 keeps the still, retries slowly, and the Compute button re-requests at once with eager until the picture', async () => {
    const h = harness();
    await display(h, 0);
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].reject(pendingErr('idle', { total: 45, device: 'cuda' }));
    await flush();
    expect(h.view.url).toBe('blob:3');
    expect(h.view.pending).toMatchObject({ state: 'idle', total: 45, device: 'cuda' });
    expect(MATTE_IDLE_RETRY_MS).toBe(5000);
    await vi.advanceTimersByTimeAsync(MATTE_IDLE_RETRY_MS - 1);
    expect(h.calls).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.calls).toHaveLength(3);
    expect(h.calls[2].req).toEqual(B); // plain: the preview never starts the pass
    h.calls[2].reject(pendingErr('idle', { total: 45 }));
    await flush();
    h.still.computeNow();
    expect(h.calls).toHaveLength(4);
    expect(h.calls[3].req).toEqual({ ...B, eager: true });
    h.calls[3].reject(pendingErr('running', { done: 1, total: 45 }));
    await flush();
    expect(h.view.pending?.state).toBe('running');
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    expect(h.calls[4].req).toEqual({ ...B, eager: true });
    h.calls[4].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:5', loading: false, error: '', pending: null });
    expect(h.still.displayedKey).toBe(StillScheduler.key(B));
    // a render started the pass meanwhile: the slow retry of another idle state sees it running, no eager needed
    h.still.request(req(0.75));
    await vi.advanceTimersByTimeAsync(150);
    h.calls[5].reject(pendingErr('idle'));
    await flush();
    await vi.advanceTimersByTimeAsync(MATTE_IDLE_RETRY_MS);
    expect(h.calls[6].req).toEqual(req(0.75));
    h.calls[6].reject(pendingErr('running', { done: 10, total: 45 }));
    await flush();
    expect(h.view.pending?.state).toBe('running');
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    expect(h.calls).toHaveLength(8);
  });

  it('computeNext (the guided panel): the NEXT different request is loaded with eager, the same state and later ones are plain', async () => {
    const h = harness();
    await display(h, 0); // the prompt-mode still (no matte op) is on screen
    h.still.computeNext();
    h.still.request(A); // the same state: nothing to do
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.calls).toHaveLength(1);
    h.still.request(B); // the panel closed: the keyed still
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(2);
    expect(h.calls[1].req).toEqual({ ...B, eager: true });
    h.calls[1].reject(pendingErr('running', { done: 1, total: 45 }));
    await flush();
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    expect(h.calls[2].req).toEqual({ ...B, eager: true }); // its retries too
    h.calls[2].resolve();
    await flush();
    expect(h.view.pending).toBeNull();
    h.still.request(req(0.75));
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls[3].req).toEqual(req(0.75)); // plain again
    // pressed and then superseded twice: only the first different state is eager
    h.still.computeNext();
    h.still.request(A);
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls[4].req).toEqual({ ...A, eager: true });
    h.still.request(req(0.9)); // (B is the still on screen: nothing to fetch for it)
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls[5].req).toEqual(req(0.9));
  });
});
