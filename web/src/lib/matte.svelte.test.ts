// The live /api/matte store (Phase 5b): reference-counted polling every
// MATTE_POLL_MS while something holds it, an immediate fetch on the first
// hold, a failed fetch that keeps the last answer, and the status a 202
// preview answer carries.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MattePending, type MatteStatus } from './api';
import { MATTE_POLL_MS, MATTE_UNLOAD_DEBOUNCE_MS, NO_COMPUTE } from './matte';
import { ApiError } from './api';
import {
  aiModeChanged,
  cancelMatteUnload,
  computeMatte,
  holdMattePolling,
  matte,
  matteMemo,
  matteMemoState,
  mattePolling,
  matteUnloadScheduled,
  noteMatteMemo,
  notePending,
  pendingHold,
  refreshMatte,
  registerMatteCompute,
  resetMatte,
  setMatteCompute,
  setMatteDevice,
  setMatteStatus,
} from './matte.svelte';

function status(over: Partial<MatteStatus> = {}): MatteStatus {
  return { enabled: true, device: 'cuda', defaultModel: 'isnet-anime', models: { 'isnet-anime': { label: 'Anime (fast)', state: 'ready', msPerFrame: 18 } }, maxSeconds: 600, maxFrames: 3000, ...over };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

describe('matte store', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    resetMatte();
    vi.useRealTimers();
  });

  it('starts unknown; setMatteStatus installs an answer and clears the error; null forgets it', () => {
    const idle = { loaded: false, status: null, error: '', compute: { state: 'none', done: 0, total: 0, device: '', pending: '' }, settingDevice: false };
    expect(matte).toEqual(idle);
    matte.error = 'HTTP 502';
    setMatteStatus(status());
    expect(matte.loaded).toBe(true);
    expect(matte.status?.device).toBe('cuda');
    expect(matte.error).toBe('');
    setMatteStatus(null);
    expect(matte).toEqual(idle);
  });

  it('pendingHold: three consecutive 202s (an update per new pending object) are ONE hold and one fetch; false releases; release is idempotent', async () => {
    let n = 0;
    const fetcher = async () => {
      n++;
      return status();
    };
    // the first hold names the fetcher and the cadence (the store keeps them), then releases
    holdMattePolling({ fetch: fetcher, intervalMs: 60_000 })();
    await flush();
    n = 0;
    // What Preview.svelte's effect does on every run: the schedulers replace
    // their pending object on every 202 answer (still.ts / proxy.ts
    // nextPending), so the effect runs once per answer with the same boolean.
    const views: { pending: object | null }[] = [{ pending: null }, { pending: null }];
    const h = pendingHold();
    const run = () => h.update(views.some((v) => v.pending !== null));
    run();
    expect(mattePolling()).toBe(false);
    expect(n).toBe(0);
    views[0].pending = { state: 'running', done: 1 };
    run();
    await flush();
    expect(mattePolling()).toBe(true);
    expect(n).toBe(1);
    views[0].pending = { state: 'running', done: 2 }; // the second 202: a new object
    run();
    views[0].pending = { state: 'running', done: 3 }; // the third
    run();
    await flush();
    expect(mattePolling()).toBe(true);
    expect(n).toBe(1); // still the one hold: no release / re-hold, no extra fetch
    // the proxy's pending joins and the still's clears: still held, still one fetch
    views[1].pending = { state: 'loading' };
    run();
    views[0].pending = null;
    run();
    await flush();
    expect(mattePolling()).toBe(true);
    expect(n).toBe(1);
    views[1].pending = null;
    run();
    expect(mattePolling()).toBe(false);
    // a later pending holds afresh (and fetches at once); release() ends it, twice is harmless
    views[0].pending = { state: 'deferred' };
    run();
    await flush();
    expect(mattePolling()).toBe(true);
    expect(n).toBe(2);
    h.release();
    h.release();
    expect(mattePolling()).toBe(false);
  });

  it('holds poll: an immediate fetch, then every 5 s; the last release stops it; release is idempotent', async () => {
    let n = 0;
    const fetcher = async () => {
      n++;
      return status({ device: n % 2 ? 'cuda' : 'cpu' });
    };
    expect(mattePolling()).toBe(false);
    const release1 = holdMattePolling({ fetch: fetcher });
    expect(mattePolling()).toBe(true);
    await flush();
    expect(n).toBe(1);
    expect(matte.loaded).toBe(true);
    expect(matte.status?.device).toBe('cuda');
    await vi.advanceTimersByTimeAsync(MATTE_POLL_MS - 1);
    expect(n).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(n).toBe(2);
    expect(matte.status?.device).toBe('cpu');
    // a second hold joins the running poll: no extra fetch, no second interval
    const release2 = holdMattePolling({ fetch: fetcher });
    await flush();
    expect(n).toBe(2);
    await vi.advanceTimersByTimeAsync(MATTE_POLL_MS);
    expect(n).toBe(3);
    release1();
    release1(); // twice: harmless
    expect(mattePolling()).toBe(true);
    await vi.advanceTimersByTimeAsync(MATTE_POLL_MS);
    expect(n).toBe(4);
    release2();
    expect(mattePolling()).toBe(false);
    await vi.advanceTimersByTimeAsync(3 * MATTE_POLL_MS);
    expect(n).toBe(4); // stopped
    expect(matte.status?.device).toBe('cpu'); // the last answer stays
    // a fresh hold fetches again at once
    holdMattePolling({ fetch: fetcher });
    await flush();
    expect(n).toBe(5);
  });

  it('a failed fetch keeps the last status and records the error; the next success clears it; in-flight fetches are joined', async () => {
    setMatteStatus(status());
    let fail = true;
    let n = 0;
    const fetcher = () =>
      new Promise<MatteStatus>((resolve, reject) => {
        n++;
        setTimeout(() => (fail ? reject(new Error('HTTP 502')) : resolve(status({ device: 'cpu' }))), 100);
      });
    const p1 = refreshMatte(fetcher);
    const p2 = refreshMatte(fetcher);
    expect(p1).toBe(p2); // joined
    expect(n).toBe(1);
    await vi.advanceTimersByTimeAsync(100);
    await p1;
    expect(matte.error).toBe('HTTP 502');
    expect(matte.loaded).toBe(true);
    expect(matte.status?.device).toBe('cuda');
    fail = false;
    const p3 = refreshMatte(fetcher);
    await vi.advanceTimersByTimeAsync(100);
    await p3;
    expect(matte.error).toBe('');
    expect(matte.status?.device).toBe('cpu');
    expect(n).toBe(2);
  });

  it('notePending installs the status object a 202 answer carries', () => {
    const p = new MattePending({
      pending: 'matte',
      state: 'running',
      done: 3,
      total: 45,
      percent: 7,
      estimateMs: 810,
      device: 'cuda',
      enabled: true,
      defaultModel: 'birefnet-lite',
      models: { 'birefnet-lite': { label: 'General (precise)', state: 'ready', msPerFrame: 170 } },
      maxSeconds: 600,
      maxFrames: 3000,
    });
    notePending(p);
    expect(matte.loaded).toBe(true);
    expect(matte.status).toEqual({
      enabled: true,
      device: 'cuda',
      reason: undefined,
      gpu: undefined,
      defaultModel: 'birefnet-lite',
      models: { 'birefnet-lite': { label: 'General (precise)', state: 'ready', msPerFrame: 170 } },
      maxSeconds: 600,
      maxFrames: 3000,
    });
  });

  // ---- Phase 5c
  it('setMatteDevice stores the preference through the setter and installs its answer; a failure keeps the status and returns the message', async () => {
    setMatteStatus(status({ device: 'cuda', devices: ['cuda', 'cpu'] }));
    const calls: string[] = [];
    const ok = async (device: string) => {
      calls.push(device);
      return status({ device: device || 'cuda', devices: ['cuda', 'cpu'], defaultModel: device === 'cpu' ? 'isnet-anime' : 'birefnet-lite' });
    };
    const p = setMatteDevice('cpu', ok);
    expect(matte.settingDevice).toBe(true);
    expect(await p).toBe('');
    expect(matte.settingDevice).toBe(false);
    expect(calls).toEqual(['cpu']);
    expect(matte.status).toMatchObject({ device: 'cpu', defaultModel: 'isnet-anime' });
    expect(await setMatteDevice('', ok)).toBe(''); // back to the default
    expect(matte.status?.device).toBe('cuda');
    const err = await setMatteDevice('tpu', async () => {
      throw new ApiError('device tpu not offered', 400);
    });
    expect(err).toBe('device tpu not offered');
    expect(matte.status?.device).toBe('cuda'); // kept
    expect(matte.settingDevice).toBe(false);
  });

  it('aiModeChanged: leaving the AI mode unloads after the debounce, coming back in time cancels, repeats are idempotent, errors are ignored', async () => {
    let n = 0;
    let fail = false;
    const unload = async () => {
      n++;
      if (fail) throw new Error('HTTP 502');
    };
    aiModeChanged(false, { unload }); // never on: nothing to release
    await vi.advanceTimersByTimeAsync(MATTE_UNLOAD_DEBOUNCE_MS * 2);
    expect(n).toBe(0);
    expect(MATTE_UNLOAD_DEBOUNCE_MS).toBe(1500);
    aiModeChanged(true);
    aiModeChanged(true);
    expect(matteUnloadScheduled()).toBe(false);
    aiModeChanged(false);
    expect(matteUnloadScheduled()).toBe(true);
    aiModeChanged(false); // still one timer
    await vi.advanceTimersByTimeAsync(MATTE_UNLOAD_DEBOUNCE_MS - 1);
    expect(n).toBe(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(n).toBe(1);
    expect(matteUnloadScheduled()).toBe(false);
    // back and out again before the debounce: cancelled
    aiModeChanged(true);
    aiModeChanged(false);
    await vi.advanceTimersByTimeAsync(500);
    aiModeChanged(true);
    expect(matteUnloadScheduled()).toBe(false);
    await vi.advanceTimersByTimeAsync(MATTE_UNLOAD_DEBOUNCE_MS * 2);
    expect(n).toBe(1);
    // a failing unload is swallowed; cancelMatteUnload drops a pending one
    fail = true;
    aiModeChanged(false);
    await vi.advanceTimersByTimeAsync(MATTE_UNLOAD_DEBOUNCE_MS);
    expect(n).toBe(2);
    aiModeChanged(true);
    aiModeChanged(false, { delayMs: 10 });
    cancelMatteUnload();
    await vi.advanceTimersByTimeAsync(100);
    expect(n).toBe(2);
  });

  it('the Compute matte button reaches the registered preview; setMatteCompute publishes a state only when it changed', () => {
    expect(computeMatte()).toBe(false);
    let pressed = 0;
    registerMatteCompute(() => pressed++);
    expect(computeMatte()).toBe(true);
    expect(pressed).toBe(1);
    registerMatteCompute(null);
    expect(computeMatte()).toBe(false);
    const before = matte.compute;
    setMatteCompute({ ...NO_COMPUTE });
    expect(matte.compute).toBe(before); // same values: the object is kept (no spurious updates)
    setMatteCompute({ state: 'running', done: 24, total: 45, device: 'GPU', pending: 'running' });
    expect(matte.compute).toEqual({ state: 'running', done: 24, total: 45, device: 'GPU', pending: 'running' });
    setMatteCompute({ state: 'computed', done: 0, total: 0, device: '', pending: '' });
    expect(matte.compute.state).toBe('computed');
  });

  it('matteMemo (5d) records what the server said about a memo per key; an empty key is ignored; resetMatte forgets it; the matte store is untouched', () => {
    expect(matteMemoState('k')).toBe('unknown');
    expect(matteMemoState('')).toBe('unknown');
    noteMatteMemo('', 'computed');
    expect(matteMemo.known).toEqual({});
    noteMatteMemo('k', 'idle');
    expect(matteMemoState('k')).toBe('idle');
    noteMatteMemo('k', 'computed');
    expect(matteMemoState('k')).toBe('computed');
    noteMatteMemo('j', 'idle');
    expect(matteMemo.known).toEqual({ k: 'computed', j: 'idle' });
    expect(matte.compute.state).toBe('none');
    expect(matte.loaded).toBe(false);
    resetMatte();
    expect(matteMemo.known).toEqual({});
    expect(matteMemoState('k')).toBe('unknown');
  });
});
