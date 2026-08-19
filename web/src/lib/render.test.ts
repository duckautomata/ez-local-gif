import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Job, JobWatcher, ProbeInfo, Source } from './api';

// The render module talks to the server through these three functions only;
// everything else in ./api stays real.
const submitJob = vi.fn<(recipe: unknown) => Promise<Job>>();
const watchJob = vi.fn<(id: string, on: JobWatcher) => () => void>();
const cancelJob = vi.fn<(id: string) => Promise<void>>();

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    submitJob: (r: unknown) => submitJob(r),
    watchJob: (id: string, on: JobWatcher) => watchJob(id, on),
    cancelJob: (id: string) => cancelJob(id),
  };
});

// toast.svelte.ts schedules dismissal with window.setTimeout; there is no DOM
// in these tests, so give it just that.
vi.stubGlobal('window', {
  setTimeout: (fn: () => void, ms: number) => setTimeout(fn, ms),
  clearTimeout: (id: ReturnType<typeof setTimeout>) => clearTimeout(id),
});

const { render, resetRender, startRender } = await import('./render.svelte');
const { app, setSource } = await import('./state.svelte');

const info: ProbeInfo = {
  format: 'gif',
  codec: 'gif',
  pixFmt: 'bgra',
  bits: 8,
  width: 64,
  height: 64,
  fps: 25,
  duration: 1,
  frames: 25,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'animation',
  premultiplied: false,
};

function source(hash: string): Source {
  return { hash, name: `${hash}.gif`, size: 100, info };
}

function job(id: string, state: Job['state']): Job {
  return { id, recipeHash: 'r' + id, recipe: { v: 1, sources: [], ops: [], output: { format: 'gif' } }, state, stage: 'encode', percent: 10, message: '', created: '' };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

describe('resetRender', () => {
  beforeEach(() => {
    submitJob.mockReset();
    watchJob.mockReset();
    cancelJob.mockReset();
    cancelJob.mockResolvedValue(undefined);
    setSource(source('a'));
    resetRender();
  });
  afterEach(() => {
    resetRender();
    setSource(null);
  });

  it('drops the result of the previous source', async () => {
    const done = job('j1', 'done');
    done.result = { recipeHash: 'rj1', recipe: done.recipe, files: [], created: '', renderMs: 1, cached: false };
    submitJob.mockResolvedValue(done);
    await startRender();
    expect(render.result).not.toBeNull();
    expect(render.job?.id).toBe('j1');

    resetRender();
    setSource(source('b'));
    expect(render).toEqual({ running: false, job: null, result: null, error: '' });
    expect(cancelJob).not.toHaveBeenCalled(); // nothing was running
  });

  it('unsubscribes from and cancels a job that is still running', async () => {
    const stop = vi.fn();
    submitJob.mockResolvedValue(job('j2', 'running'));
    watchJob.mockReturnValue(stop);
    await startRender();
    expect(render.running).toBe(true);
    expect(watchJob).toHaveBeenCalledWith('j2', expect.anything());

    resetRender();
    expect(stop).toHaveBeenCalledTimes(1);
    expect(cancelJob).toHaveBeenCalledWith('j2');
    expect(render).toEqual({ running: false, job: null, result: null, error: '' });

    // a late SSE snapshot for the old job must not come back
    const on = watchJob.mock.calls[0][1];
    on.done({ ...job('j2', 'done'), result: null });
    expect(render).toEqual({ running: false, job: null, result: null, error: '' });
  });

  it('ignores a submit that lands after the reset and cancels that orphan job', async () => {
    let resolve!: (j: Job) => void;
    submitJob.mockReturnValue(new Promise<Job>((r) => (resolve = r)));
    const p = startRender();
    expect(render.running).toBe(true);

    resetRender(); // new source while POST /api/jobs is in flight
    expect(render.running).toBe(false);
    resolve(job('j3', 'queued'));
    await p;
    await flush();
    expect(render).toEqual({ running: false, job: null, result: null, error: '' });
    expect(watchJob).not.toHaveBeenCalled();
    expect(cancelJob).toHaveBeenCalledWith('j3');
  });

  it('ignores a submit failure that lands after the reset', async () => {
    let reject!: (e: unknown) => void;
    submitJob.mockReturnValue(new Promise<Job>((_, r) => (reject = r)));
    const p = startRender();
    resetRender();
    reject(new Error('boom'));
    await p;
    expect(render.error).toBe('');
    expect(render.running).toBe(false);
  });

  it('is a no-op on a fresh state', () => {
    resetRender();
    expect(render).toEqual({ running: false, job: null, result: null, error: '' });
    expect(cancelJob).not.toHaveBeenCalled();
    expect(app.source?.hash).toBe('a');
  });
});
