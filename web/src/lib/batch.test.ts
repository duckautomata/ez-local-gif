// Phase 4 batch mode: the pure state model — rows, the global-ops-only
// recipe each row submits, render orchestration with injected fakes
// (one POST /api/jobs per row, per-row watcher, isolated errors) and the
// save-to-/output flow.
import { beforeEach, describe, expect, it } from 'vitest';
import type { Job, JobWatcher, ProbeInfo, Recipe, Result, Source } from './api';
import {
  addPendingRow,
  batch,
  batchOpsCfg,
  enterBatch,
  exitBatch,
  removeRow,
  renderAll,
  renderRow,
  retryRow,
  rowPrimary,
  rowRecipe,
  rowUploaded,
  rowUploadFailed,
  saveAll,
  saveRow,
  startBatch,
} from './batch.svelte';
import { defaultOutput } from './presets';
import { app, applyPreset, defaultOps, resetApp } from './state.svelte';

const info = (over: Partial<ProbeInfo> = {}): ProbeInfo => ({
  format: 'gif',
  codec: 'gif',
  pixFmt: 'bgra',
  bits: 8,
  width: 160,
  height: 120,
  fps: 25,
  duration: 2,
  frames: 50,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'animation',
  premultiplied: false,
  ...over,
});
const src = (hash: string, over: Partial<ProbeInfo> = {}): Source => ({ hash: hash.repeat(64).slice(0, 64), name: `${hash}.gif`, size: 10, info: info(over) });

function doneJob(id: string, recipe: Recipe, files = true): Job {
  const result: Result = {
    recipeHash: 'f'.repeat(64),
    recipe,
    files: files ? [{ name: 'out.gif', url: '/out/x/out.gif', format: 'gif', bytes: 1000, width: 128, height: 128, frames: 10, fps: 20, duration: 0.5, limit: 262144, kind: 'output' }] : [],
    created: '',
    renderMs: 1,
    cached: false,
  };
  return { id, recipeHash: result.recipeHash, recipe, state: 'done', stage: 'done', percent: 100, message: '', result, created: '' };
}

beforeEach(() => {
  exitBatch();
  resetApp();
});

describe('rows', () => {
  it('startBatch uploads every file into its own row; a failed upload marks only its row', async () => {
    const files = [new File(['a'], 'a.mov'), new File(['b'], 'bad.mov'), new File(['c'], 'c.mov')];
    await startBatch(files, {
      upload: async (f) => {
        if (f.name === 'bad.mov') throw new Error('boom');
        return src(f.name[0], f.name === 'c.mov' ? { premultiplied: true } : {});
      },
    });
    expect(batch.active).toBe(true);
    expect(batch.rows).toHaveLength(3);
    expect(batch.rows[0].source?.hash).toBe('a'.repeat(64));
    expect(batch.rows[0].error).toBe('');
    expect(batch.rows[1].source).toBeNull();
    expect(batch.rows[1].error).toBe('boom');
    // per-row unpremultiply auto-defaults from the probe
    expect(batch.rows[0].unpremultiply).toBe(false);
    expect(batch.rows[2].unpremultiply).toBe(true);
  });

  it('removeRow / exitBatch clean up', () => {
    enterBatch();
    const id = addPendingRow('x.mov');
    rowUploaded(id, src('a'));
    expect(batch.rows).toHaveLength(1);
    removeRow(id);
    expect(batch.rows).toHaveLength(0);
    addPendingRow('y.mov');
    exitBatch();
    expect(batch.active).toBe(false);
    expect(batch.rows).toHaveLength(0);
  });

  it('rowUploadFailed leaves the other rows untouched', () => {
    enterBatch();
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    rowUploaded(a, src('a'));
    rowUploadFailed(b, 'nope');
    expect(batch.rows[0].error).toBe('');
    expect(batch.rows[1].error).toBe('nope');
    expect(batch.rows[1].uploading).toBe(false);
  });
});

describe('batchOpsCfg / rowRecipe (global ops only)', () => {
  it('keeps exactly the geometry-independent ops and forces the per-source ones off', () => {
    const c = defaultOps(info());
    c.trim = { enabled: true, start: 0.5, end: 1 };
    c.crop = { enabled: true, x: 1, y: 1, w: 10, h: 10 };
    c.autocrop = { enabled: true, padding: 0, threshold: 1 };
    c.resize = { enabled: true, width: 64, height: 0, fit: 'contain' };
    c.flipRotate = { enabled: true, horizontal: true, vertical: false, degrees: 90 };
    c.delay = { enabled: true, ms: 40 };
    c.fps = { enabled: true, fps: 20 };
    c.speed = { enabled: true, factor: 2 };
    c.reverse = true;
    c.bounce = true;
    c.feather = { enabled: true, radius: 3 };
    c.background = { ...c.background, enabled: true };
    const b = batchOpsCfg(c, true);
    expect(b.unpremultiply).toBe(true);
    expect(b.trim.enabled).toBe(false);
    expect(b.crop.enabled).toBe(false);
    expect(b.autocrop.enabled).toBe(false);
    expect(b.resize.enabled).toBe(false);
    expect(b.flipRotate.enabled).toBe(false);
    expect(b.delay.enabled).toBe(false);
    expect(b.overlays).toEqual([]);
    // the original configuration is untouched
    expect(c.trim.enabled).toBe(true);

    const recipe = rowRecipe({ source: src('a'), unpremultiply: true }, c, defaultOutput());
    expect(recipe?.sources).toEqual(['a'.repeat(64)]);
    expect(recipe?.ops.map((o) => o.kind)).toEqual(['unpremultiply', 'speed', 'fps', 'chromakey', 'feather', 'reverse', 'bounce']);
    expect(rowRecipe({ source: null, unpremultiply: false }, c, defaultOutput())).toBeNull();
  });
});

describe('render orchestration', () => {
  it('renderAll submits one job per uploaded row and finishes each from its watcher', async () => {
    enterBatch();
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    const pending = addPendingRow('c.mov'); // still uploading: skipped
    rowUploaded(a, src('a'));
    rowUploaded(b, src('b'));
    void pending;

    const submitted: Recipe[] = [];
    const watchersByJob = new Map<string, JobWatcher>();
    await renderAll({
      submit: async (recipe) => {
        submitted.push(recipe);
        const id = `job-${submitted.length}`;
        return { ...doneJob(id, recipe), state: 'running', stage: 'encode', percent: 10, result: null } as Job;
      },
      watch: (id, on) => {
        watchersByJob.set(id, on);
        return () => watchersByJob.delete(id);
      },
    });
    expect(submitted).toHaveLength(2);
    expect(submitted[0].sources).toEqual(['a'.repeat(64)]);
    expect(submitted[1].sources).toEqual(['b'.repeat(64)]);
    expect(batch.rows[0].running).toBe(true);

    // per-row progress and completion, errors isolated
    const j1 = doneJob('job-1', submitted[0]);
    watchersByJob.get('job-1')?.done(j1);
    expect(batch.rows[0].running).toBe(false);
    expect(rowPrimary(batch.rows[0])?.name).toBe('out.gif');
    expect(batch.rows[1].running).toBe(true);
    watchersByJob.get('job-2')?.error('encoder exploded');
    expect(batch.rows[1].running).toBe(false);
    expect(batch.rows[1].error).toBe('encoder exploded');
    expect(batch.rows[0].error).toBe('');
  });

  it('a row whose submit rejects fails alone; a job already done on submit finishes at once', async () => {
    enterBatch();
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    rowUploaded(a, src('a'));
    rowUploaded(b, src('b'));
    let n = 0;
    await renderAll({
      submit: async (recipe) => {
        n++;
        if (n === 1) throw new Error('HTTP 400: no');
        return doneJob('job-ok', recipe); // cached: done straight away
      },
      watch: () => () => undefined,
    });
    expect(batch.rows[0].error).toBe('HTTP 400: no');
    expect(batch.rows[0].running).toBe(false);
    expect(batch.rows[1].error).toBe('');
    expect(rowPrimary(batch.rows[1])).not.toBeNull();
  });

  // WEB-9: a row nobody will show any more must not keep holding a render
  // slot — removing a running row (or leaving the batch) cancels its job.
  it('removeRow cancels the row’s running job; settled rows are left alone', async () => {
    enterBatch();
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    rowUploaded(a, src('a'));
    rowUploaded(b, src('b'));
    const cancelled: string[] = [];
    const deps = {
      submit: async (recipe: Recipe) => ({ ...doneJob(`job-${recipe.sources[0][0]}`, recipe), state: 'running', stage: 'encode', percent: 5, result: null }) as Job,
      watch: () => () => undefined,
      cancel: async (id: string) => {
        cancelled.push(id);
      },
    };
    await renderAll(deps);
    removeRow(a, deps);
    expect(cancelled).toEqual(['job-a']);
    expect(batch.rows).toHaveLength(1);
    // a finished row has nothing to cancel
    const jb = batch.rows[0];
    jb.running = false;
    jb.job = { ...jb.job!, state: 'done' };
    removeRow(b, deps);
    expect(cancelled).toEqual(['job-a']);
  });

  it('exitBatch cancels every still-running job, best-effort', async () => {
    enterBatch();
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    rowUploaded(a, src('a'));
    rowUploaded(b, src('b'));
    const cancelled: string[] = [];
    const deps = {
      submit: async (recipe: Recipe) => ({ ...doneJob(`job-${recipe.sources[0][0]}`, recipe), state: 'queued', result: null }) as Job,
      watch: () => () => undefined,
      cancel: async (id: string) => {
        cancelled.push(id);
        if (id === 'job-a') throw new Error('gone already'); // best-effort: a failed cancel is swallowed
      },
    };
    await renderAll(deps);
    exitBatch(deps);
    expect(cancelled.sort()).toEqual(['job-a', 'job-b']);
    expect(batch.active).toBe(false);
  });

  it('a submit that lands after the batch was left cancels its orphan job', async () => {
    enterBatch();
    const a = addPendingRow('a.mov');
    rowUploaded(a, src('a'));
    let cancelled = '';
    let resolveSubmit: (j: Job) => void = () => undefined;
    const p = renderRow(a, {
      submit: (recipe) =>
        new Promise<Job>((res) => {
          resolveSubmit = (j) => res({ ...j, recipe });
        }),
      watch: () => () => undefined,
      cancel: async (id) => {
        cancelled = id;
      },
    });
    exitBatch();
    resolveSubmit({ ...doneJob('orphan', { v: 1, sources: [], ops: [], output: { format: 'gif' } }), state: 'queued', result: null });
    await p;
    expect(cancelled).toBe('orphan');
    expect(batch.rows).toHaveLength(0);
  });
});

// WEB-10: an upload failure leaves the row without a source, but the dropped
// File is kept on it — Retry re-uploads instead of being permanently disabled.
describe('retryRow', () => {
  it('re-uploads the kept File after an upload failure; the row is Ready again', async () => {
    await startBatch([new File(['a'], 'a.mov')], {
      upload: async () => {
        throw new Error('server busy');
      },
    });
    const row = batch.rows[0];
    expect(row.source).toBeNull();
    expect(row.error).toBe('server busy');
    expect(row.file?.name).toBe('a.mov');

    const uploaded: string[] = [];
    await retryRow(row.id, {
      upload: async (f) => {
        uploaded.push(f.name);
        return src('a');
      },
    });
    expect(uploaded).toEqual(['a.mov']);
    expect(batch.rows[0].source?.hash).toBe('a'.repeat(64));
    expect(batch.rows[0].error).toBe('');
    expect(batch.rows[0].uploading).toBe(false);
  });

  it('a failed retry marks the row again and can be retried once more', async () => {
    await startBatch([new File(['a'], 'a.mov')], {
      upload: async () => {
        throw new Error('boom 1');
      },
    });
    const id = batch.rows[0].id;
    await retryRow(id, {
      upload: async () => {
        throw new Error('boom 2');
      },
    });
    expect(batch.rows[0].error).toBe('boom 2');
    expect(batch.rows[0].uploading).toBe(false);
    expect(batch.rows[0].file?.name).toBe('a.mov'); // still there for the next attempt
  });

  it('with a source it renders (the render-failure Retry path)', async () => {
    enterBatch();
    const a = addPendingRow('a.mov', new File(['a'], 'a.mov'));
    rowUploaded(a, src('a'));
    let submitted = 0;
    await retryRow(a, {
      submit: async (recipe) => {
        submitted++;
        return doneJob('job-1', recipe);
      },
      watch: () => () => undefined,
    });
    expect(submitted).toBe(1);
    expect(rowPrimary(batch.rows[0])).not.toBeNull();
  });

  it('is inert for a row with neither a source nor a kept File', async () => {
    enterBatch();
    const a = addPendingRow('a.mov'); // no File kept
    rowUploadFailed(a, 'nope');
    await retryRow(a, {
      upload: async () => src('a'),
    });
    expect(batch.rows[0].source).toBeNull();
    expect(batch.rows[0].error).toBe('nope');
  });
});

describe('save to /output', () => {
  it('saveRow records the collision-safe name; saveAll skips saved rows; errors stay per row', async () => {
    enterBatch();
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    rowUploaded(a, src('a'));
    rowUploaded(b, src('b'));
    const recipe: Recipe = { v: 1, sources: ['a'.repeat(64)], ops: [], output: { format: 'gif' } };
    // pretend both rendered
    batch.rows[0].result = doneJob('j1', recipe).result ?? null;
    batch.rows[1].result = doneJob('j2', recipe).result ?? null;

    const saved: string[] = [];
    await saveRow(a, {
      save: async (hash, file) => {
        saved.push(`${hash.slice(0, 2)}:${file}`);
        return 'out-2.gif';
      },
    });
    expect(batch.rows[0].savedAs).toBe('out-2.gif');
    expect(saved).toEqual(['ff:out.gif']);

    await saveAll({
      save: async () => {
        throw new Error('disk full');
      },
    });
    // row a was already saved and skipped; row b failed alone
    expect(batch.rows[0].savedAs).toBe('out-2.gif');
    expect(batch.rows[0].saveError).toBe('');
    expect(batch.rows[1].savedAs).toBe('');
    expect(batch.rows[1].saveError).toBe('disk full');
  });
});

// WEB-4: a frames result has only kind 'frame'/'archive' files — no primary —
// so a batch row could never show or save it. Entering batch mode with a
// pre-selected Frames output falls back to Chat (the Output card hides the
// Frames chip and format while the batch is active).
describe('frames guard', () => {
  it('enterBatch replaces a pre-selected Frames output with the Chat preset', () => {
    applyPreset('frames');
    expect(app.output.format).toBe('frames');
    enterBatch();
    expect(app.output.preset).toBe('chat');
    expect(app.output.format).toBe('gif');
  });

  it('enterBatch leaves any non-frames output untouched', () => {
    applyPreset('emote');
    app.output.format = 'webp';
    enterBatch();
    expect(app.output.preset).toBe('emote');
    expect(app.output.format).toBe('webp');
  });
});
