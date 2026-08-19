import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { StillRequest } from './api';
import { DECODE_ERROR, StillScheduler, type StillView } from './still';

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
  const view: StillView = { url: null, loading: false, error: '' };
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
  });
  return { still, view, calls, revoked };
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
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '' });
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
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '' });
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
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '' });
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
    expect(h.view).toEqual({ url: 'blob:3', loading: false, error: '' });
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
    expect(h.view).toEqual({ url: null, loading: false, error: 'boom' });
    h.still.request(B);
    await vi.advanceTimersByTimeAsync(150);
    h.calls[1].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:5', loading: false, error: '' });
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
    expect(h.view).toEqual({ url: null, loading: false, error: '' });
    expect(h.revoked).toEqual(['blob:3']);
    expect(h.still.displayedKey).toBe('');
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
