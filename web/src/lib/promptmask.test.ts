// The live mask scheduler of the Select subject panel (Phase 5c): one
// in-flight POST /api/matte/prompt whose answer matches the latest state,
// debounced, superseded requests aborted, a 202 retried, errors shown.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MattePending, type PromptMaskRequest } from './api';
import { MATTE_RETRY_MS } from './matte';
import { PromptMaskScheduler, type PromptMaskView } from './promptmask';

interface Call {
  req: PromptMaskRequest;
  signal: AbortSignal;
  resolve(): void;
  reject(err: unknown): void;
}

function harness() {
  const calls: Call[] = [];
  const revoked: string[] = [];
  const pendings: MattePending[] = [];
  const view: PromptMaskView = { url: null, frame: -1, loading: false, error: '', pending: null };
  let n = 0;
  const masks = new PromptMaskScheduler(view, {
    fetch(req, signal) {
      return new Promise<Blob>((resolve, reject) => {
        calls.push({ req, signal, resolve: () => resolve(new Blob([`m${++n}`])), reject });
      });
    },
    createURL: (b) => `blob:${b.size}`,
    revokeURL: (u) => revoked.push(u),
    onPending: (p) => pendings.push(p),
  });
  return { masks, view, calls, revoked, pendings };
}

function req(frame: number, x = 0.5): PromptMaskRequest {
  return {
    src: 'h',
    sources: ['h'],
    ops: [{ kind: 'matte', params: { model: 'sam2-tiny', prompts: [{ frame, points: [[x, 0.5, 1]] }] } }],
    output: { format: 'gif', fps: 25 },
    frame,
    prompts: { obj: 1, prompts: [{ frame, points: [[x, 0.5, 1]] }] },
  };
}

function pending(state: string): MattePending {
  return new MattePending({ pending: 'matte', state, done: 0, total: 0, percent: 0, estimateMs: 0, device: 'cuda', enabled: true, defaultModel: 'isnet-anime', models: {}, maxSeconds: 600, maxFrames: 3000 });
}

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

describe('PromptMaskScheduler', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('debounces, fetches once, shows the mask with its frame; the same request again is a no-op; null clears', async () => {
    const h = harness();
    h.masks.request(req(3));
    await vi.advanceTimersByTimeAsync(149);
    expect(h.calls).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.calls).toHaveLength(1);
    expect(h.view.loading).toBe(true);
    expect(h.masks.displayedKey).toBe('');
    h.calls[0].resolve();
    await flush();
    expect(h.view).toEqual({ url: 'blob:2', frame: 3, loading: false, error: '', pending: null });
    expect(h.masks.displayedKey).toBe(PromptMaskScheduler.key(req(3))); // the mask on screen answers the latest request
    h.masks.request(req(3));
    await vi.advanceTimersByTimeAsync(500);
    expect(h.calls).toHaveLength(1);
    h.masks.request(req(4)); // a newer request: the old mask stays on screen but no longer answers the current key
    expect(h.masks.displayedKey).not.toBe(PromptMaskScheduler.key(req(4)));
    h.masks.request(null);
    expect(h.view).toEqual({ url: null, frame: 3, loading: false, error: '', pending: null });
    expect(h.masks.displayedKey).toBe('');
    expect(h.revoked).toEqual(['blob:2']);
  });

  it('a newer request (another click or frame) aborts the in-flight one and only the latest lands', async () => {
    const h = harness();
    h.masks.request(req(3, 0.5));
    await vi.advanceTimersByTimeAsync(150);
    h.masks.request(req(3, 0.6));
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(2);
    expect(h.calls[0].signal.aborted).toBe(true);
    h.calls[0].resolve(); // lands late: ignored
    await flush();
    expect(h.view.url).toBeNull();
    h.calls[1].resolve();
    await flush();
    expect(h.view.url).toBe('blob:2');
    expect(h.view.loading).toBe(false);
  });

  it('a 202 (the tracker loading) keeps the last mask, sets pending, hands the status over and retries; an error is shown and cleared by the next answer', async () => {
    const h = harness();
    h.masks.request(req(3));
    await vi.advanceTimersByTimeAsync(150);
    h.calls[0].reject(pending('loading'));
    await flush();
    expect(h.view.pending?.state).toBe('loading');
    expect(h.pendings).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(MATTE_RETRY_MS);
    expect(h.calls).toHaveLength(2);
    h.calls[1].reject(new Error('tracker failed'));
    await flush();
    expect(h.view).toMatchObject({ error: 'tracker failed', pending: null, loading: false });
    await vi.advanceTimersByTimeAsync(5000);
    expect(h.calls).toHaveLength(2); // an error ends the retries
    h.masks.retry();
    await vi.advanceTimersByTimeAsync(150);
    expect(h.calls).toHaveLength(3);
    h.calls[2].resolve();
    await flush();
    expect(h.view).toMatchObject({ url: 'blob:2', error: '' });
    // dispose releases the mask and stops everything
    h.masks.request(req(4));
    await vi.advanceTimersByTimeAsync(150);
    h.masks.dispose();
    expect(h.view.url).toBeNull();
    expect(h.calls[3].signal.aborted).toBe(true);
  });
});
