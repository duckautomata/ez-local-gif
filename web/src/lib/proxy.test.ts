import { describe, expect, it } from 'vitest';
import type { ProxyRequest } from './api';
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
  const view: ProxyView = { url: null, playing: false, loading: false, stale: false, error: '' };
  let n = 0;
  const player = new ProxyPlayer(view, {
    fetch(req, signal) {
      return new Promise<Blob>((resolve, reject) => {
        calls.push({ req, signal, resolve: () => resolve(new Blob([`p${++n}`])), reject });
      });
    },
    createURL: (b) => `blob:${b.size}`,
    revokeURL: (u) => revoked.push(u),
  });
  return { player, view, calls, revoked };
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
    expect(h.view).toEqual({ url: null, playing: false, loading: false, stale: false, error: '' });
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
    expect(h.view).toEqual({ url: 'blob:2', playing: true, loading: false, stale: false, error: '' });
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
    expect(h.view).toEqual({ url: 'blob:2', playing: true, loading: false, stale: false, error: '' });
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
    expect(h.view).toEqual({ url: null, playing: false, loading: false, stale: false, error: '' });
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
    expect(h.view).toEqual({ url: null, playing: false, loading: false, stale: false, error: 'ffmpeg exited with status 1' });
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
