import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MattePending, type MattePendingBody, type MattePendingState, type ProxyRequest } from './api';
import { MATTE_RETRY_MS } from './matte';
import { ProxyPlayer, type ProxyView } from './proxy';

interface Call {
  req: ProxyRequest;
  signal: AbortSignal;
  resolve(): void;
  reject(err: unknown): void;
}

function harness() {
  const calls: Call[] = [];
  const revoked: string[] = [];
  const pendings: MattePending[] = [];
  const view: ProxyView = { url: null, playing: false, loading: false, stale: false, error: '', pending: null };
  let n = 0;
  const player = new ProxyPlayer(view, {
    fetch(req, signal) {
      return new Promise<Blob>((resolve, reject) => {
        calls.push({ req, signal, resolve: () => resolve(new Blob([`p${++n}`])), reject });
      });
    },
    createURL: (b) => `blob:${b.size}`,
    revokeURL: (u) => revoked.push(u),
    onPending: (p) => pendings.push(p),
  });
  return { player, view, calls, revoked, pendings };
}

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
    models: {},
    maxSeconds: 600,
    maxFrames: 3000,
    ...over,
  });
}

function req(ops: ProxyRequest['ops'] = []): ProxyRequest {
  return { sources: ['h'], ops, output: { format: 'gif' }, maxW: 360, maxSeconds: 10 };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

const A = req();
const B = req([{ kind: 'reverse' }]);

describe('ProxyPlayer', () => {
  it('never fetches on update — only Play does', async () => {
    const h = harness();
    h.player.update(A);
    h.player.update(B);
    h.player.update(A);
    await flush();
    expect(h.calls).toHaveLength(0);
    expect(h.view).toEqual({ url: null, playing: false, loading: false, stale: false, error: '', pending: null });
  });

  it('Play fetches the current recipe and shows it', async () => {
    const h = harness();
    h.player.update(A);
    const p = h.player.play();
    expect(h.view.loading).toBe(true);
    expect(h.calls).toHaveLength(1);
    expect(h.calls[0].req).toEqual(A);
    h.calls[0].resolve();
    await p;
    expect(h.view).toEqual({ url: 'blob:2', playing: true, loading: false, stale: false, error: '', pending: null });
    // playing the same recipe again is a no-op
    await h.player.play();
    expect(h.calls).toHaveLength(1);
  });

  it('a recipe change while playing marks the proxy stale and keeps it on screen until Play again', async () => {
    const h = harness();
    h.player.update(A);
    const p = h.player.play();
    h.calls[0].resolve();
    await p;
    h.player.update(B);
    expect(h.view.stale).toBe(true);
    expect(h.view.playing).toBe(true);
    expect(h.view.url).toBe('blob:2');
    expect(h.calls).toHaveLength(1); // not refetched by itself
    // back to the played recipe: fresh again
    h.player.update(A);
    expect(h.view.stale).toBe(false);
    h.player.update(B);
    const again = h.player.play();
    expect(h.calls).toHaveLength(2);
    expect(h.calls[1].req).toEqual(B);
    h.calls[1].resolve();
    await again;
    expect(h.view).toEqual({ url: 'blob:2', playing: true, loading: false, stale: false, error: '', pending: null });
    expect(h.revoked).toEqual(['blob:2']); // the old proxy URL was released
  });

  it('a recipe change during the fetch aborts it; the stale flag is correct when a late fetch lands', async () => {
    const h = harness();
    h.player.update(A);
    const p = h.player.play();
    const a = h.calls[0];
    h.player.update(B);
    expect(a.signal.aborted).toBe(true);
    expect(h.view.loading).toBe(false);
    a.resolve(); // lands anyway
    await p;
    expect(h.view.playing).toBe(false);
    expect(h.view.url).toBeNull();
    // Play of B then succeeds normally
    const pb = h.player.play();
    h.calls[1].resolve();
    await pb;
    expect(h.view.playing).toBe(true);
    expect(h.view.stale).toBe(false);
  });

  it('Stop returns to the still: aborts, releases the URL, clears stale', async () => {
    const h = harness();
    h.player.update(A);
    const p = h.player.play();
    h.calls[0].resolve();
    await p;
    h.player.update(B);
    h.player.stop();
    expect(h.view).toEqual({ url: null, playing: false, loading: false, stale: false, error: '', pending: null });
    expect(h.revoked).toEqual(['blob:2']);
    // a fetch in flight is aborted by Stop
    const p2 = h.player.play();
    const c = h.calls[1];
    h.player.stop();
    expect(c.signal.aborted).toBe(true);
    c.resolve();
    await p2;
    expect(h.view.playing).toBe(false);
  });

  it('no source stops playback; errors are reported and retryable', async () => {
    const h = harness();
    h.player.update(A);
    const p = h.player.play();
    h.calls[0].reject(new Error('ffmpeg exited with status 1'));
    await p;
    expect(h.view).toEqual({ url: null, playing: false, loading: false, stale: false, error: 'ffmpeg exited with status 1', pending: null });
    const p2 = h.player.play();
    expect(h.view.error).toBe('');
    h.calls[1].resolve();
    await p2;
    expect(h.view.playing).toBe(true);
    h.player.update(null);
    expect(h.view.playing).toBe(false);
    expect(h.view.url).toBeNull();
    await h.player.play(); // nothing to play
    expect(h.calls).toHaveLength(2);
  });

  it('an aborted fetch never records an error', async () => {
    const h = harness();
    h.player.update(A);
    const p = h.player.play();
    const a = h.calls[0];
    h.player.stop();
    a.reject(new DOMException('The operation was aborted.', 'AbortError'));
    await p;
    expect(h.view.error).toBe('');
  });
});

// Phase 5b: Play always starts a deferred matte pass (`eager`), and a 202
// answer keeps the still on the stage with the pending state until the
// proxy arrives — re-requested every MATTE_RETRY_MS — or Stop / a recipe
// change ends it. Without a matte op nothing changes on the wire.
describe('ProxyPlayer — AI matte pending (Phase 5b)', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());
  const M = req([{ kind: 'matte' }, { kind: 'morph', params: { close: true } }]);

  it('Play with a matte op sends eager; a 202 sets pending (not playing), hands the status over and re-requests until the proxy arrives', async () => {
    const h = harness();
    h.player.update(M);
    const p = h.player.play();
    expect(h.calls[0].req).toEqual({ ...M, eager: true });
    h.calls[0].reject(pendingErr('running', { done: 10, total: 45 }));
    await p;
    expect(h.view).toMatchObject({ url: null, playing: false, loading: false, error: '' });
    expect(h.view.pending).toMatchObject({ state: 'running', done: 10, total: 45, device: 'cuda', since: Date.now() });
    expect(h.pendings).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS - 1);
    expect(h.calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.calls).toHaveLength(2);
    expect(h.calls[1].req).toEqual({ ...M, eager: true });
    expect(h.view.loading).toBe(true);
    h.calls[1].resolve();
    await vi.advanceTimersByTimeAsync(0);
    expect(h.view).toEqual({ url: 'blob:2', playing: true, loading: false, stale: false, error: '', pending: null });
    // the plain recipe still goes out as is
    h.player.update(A);
    const pa = h.player.play();
    expect(h.calls[2].req).toEqual(A);
    h.calls[2].resolve();
    await pa;
  });

  it('Stop while pending cancels the re-request; a recipe change drops the old recipe’s pending state', async () => {
    const h = harness();
    h.player.update(M);
    let p = h.player.play();
    h.calls[0].reject(pendingErr('loading'));
    await p;
    expect(h.view.pending?.state).toBe('loading');
    h.player.stop();
    expect(h.view).toEqual({ url: null, playing: false, loading: false, stale: false, error: '', pending: null });
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(1);
    // pending for M, then the recipe changes: no re-request of M, pending gone
    p = h.player.play();
    h.calls[1].reject(pendingErr('running', { done: 1, total: 2 }));
    await p;
    expect(h.view.pending).not.toBeNull();
    h.player.update(B);
    expect(h.view.pending).toBeNull();
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(2);
    // an error after a pending answer clears it too
    h.player.update(M);
    p = h.player.play();
    h.calls[2].reject(pendingErr('running', { done: 1, total: 2 }));
    await p;
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    h.calls[3].reject(new Error('AI matte failed: sidecar unreachable'));
    await vi.advanceTimersByTimeAsync(0);
    expect(h.view.pending).toBeNull();
    expect(h.view.error).toBe('AI matte failed: sidecar unreachable');
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(4);
  });
});
