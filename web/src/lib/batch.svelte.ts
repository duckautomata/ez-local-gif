// Batch mode (Phase 4, DESIGN §7): dropping several video/animation files
// renders each with ONE shared configuration — the batch is pure frontend
// orchestration over the ordinary endpoints (one POST /api/upload per file,
// one POST /api/jobs per row, the usual SSE per job; no new job machinery).
// Only geometry-independent ops apply (batchOpsCfg): unpremultiply (per row,
// auto from the probe), fps, speed, feather, background keying, reverse and
// bounce; trim/crop/autocrop/resize/flip/rotate/overlays are per-source and
// disabled ("Open in editor" seeds the single view). Row errors are isolated:
// one failed upload or render never touches the other rows.
//
// Every network dependency is injectable (BatchDeps) so the state model is
// unit-testable without a DOM or server (batch.test.ts).

import {
  cancelJob,
  messageOf,
  RECIPE_VERSION,
  saveResult,
  submitJob,
  upload,
  watchJob,
  type Job,
  type Recipe,
  type Result,
  type ResultFile,
  type Source,
} from './api';
import type { OutputCfg } from './presets';
import { groupFiles } from './result';
import { app, applyPreset, buildOps, buildOutput, type OpsCfg } from './state.svelte';

export interface BatchRow {
  id: number;
  /** display name: the dropped file's until the upload answered, then the source's */
  name: string;
  /** the dropped File, kept so a failed upload can be retried (WEB-10) */
  file: File | null;
  source: Source | null;
  uploading: boolean;
  /** per-row "source alpha is premultiplied" (auto-defaulted from probe.premultiplied) */
  unpremultiply: boolean;
  running: boolean;
  job: Job | null;
  result: Result | null;
  /** upload or render error — per row, isolated */
  error: string;
  saving: boolean;
  /** final name in /output after "Save to /output" */
  savedAs: string;
  saveError: string;
}

export const batch = $state({
  active: false,
  rows: [] as BatchRow[],
});

let nextRowId = 1;
// Generation guard: bumped by enterBatch/exitBatch so an await that resolves
// after the batch was replaced or left cannot write into the fresh state.
let generation = 0;
const watchers = new Map<number, () => void>();

/** Injectable network dependencies (tests pass fakes; the app the real client). */
export interface BatchDeps {
  upload?: (file: File) => Promise<Source>;
  submit?: typeof submitJob;
  watch?: typeof watchJob;
  save?: typeof saveResult;
  cancel?: typeof cancelJob;
}

function dep<K extends keyof Required<BatchDeps>>(deps: BatchDeps, k: K): Required<BatchDeps>[K] {
  const defaults: Required<BatchDeps> = {
    upload: (f: File) => upload(f).promise,
    submit: submitJob,
    watch: watchJob,
    save: saveResult,
    cancel: cancelJob,
  };
  return (deps[k] ?? defaults[k]) as Required<BatchDeps>[K];
}

function stopWatchers(): void {
  for (const stop of watchers.values()) stop();
  watchers.clear();
}

function rowById(id: number): BatchRow | null {
  return batch.rows.find((r) => r.id === id) ?? null;
}

/** enterBatch clears any previous batch and switches the app into batch mode. */
export function enterBatch(): void {
  generation++;
  stopWatchers();
  batch.rows = [];
  batch.active = true;
  // A frames result has no primary file, so a batch row could never show or
  // save it (WEB-4): a Frames output selected BEFORE the multi-drop falls
  // back to Chat, mirroring setSource's presetAvailable guard. The Output
  // card hides Frames (chip and format) while the batch is active.
  if (app.output.format === 'frames') applyPreset('chat');
}

/**
 * cancelRowJob best-effort cancels a row's queued/running server job (WEB-9):
 * nobody will show it any more, so it must not keep holding a render slot.
 * A row still submitting (running, no job yet) is covered by renderRow's own
 * post-submit orphan check.
 */
function cancelRowJob(row: BatchRow, deps: BatchDeps): void {
  const job = row.job;
  if (!row.running || !job) return;
  if (job.state === 'queued' || job.state === 'running') void dep(deps, 'cancel')(job.id).catch(() => undefined);
}

/** exitBatch leaves batch mode (watchers stopped; every row's running server job is cancelled, best-effort). */
export function exitBatch(deps: BatchDeps = {}): void {
  generation++;
  stopWatchers();
  for (const row of batch.rows) cancelRowJob(row, deps);
  batch.active = false;
  batch.rows = [];
}

/** addPendingRow appends a row that is still uploading and returns its id (`file` kept for upload retries). */
export function addPendingRow(name: string, file: File | null = null): number {
  const id = nextRowId++;
  batch.rows.push({
    id,
    name,
    file,
    source: null,
    uploading: true,
    unpremultiply: false,
    running: false,
    job: null,
    result: null,
    error: '',
    saving: false,
    savedAs: '',
    saveError: '',
  });
  return id;
}

/** rowUploaded installs the probed source (per-row unpremultiply auto-defaults from the probe). */
export function rowUploaded(id: number, source: Source): void {
  const row = rowById(id);
  if (!row) return;
  row.source = source;
  row.name = source.name;
  row.uploading = false;
  row.error = '';
  row.unpremultiply = source.info.premultiplied === true;
}

/** rowUploadFailed marks the row failed (the other rows are untouched). */
export function rowUploadFailed(id: number, error: string): void {
  const row = rowById(id);
  if (!row) return;
  row.uploading = false;
  row.error = error;
}

/** removeRow drops one row (its progress subscription included; a running server job is cancelled, best-effort). */
export function removeRow(id: number, deps: BatchDeps = {}): void {
  watchers.get(id)?.();
  watchers.delete(id);
  const i = batch.rows.findIndex((r) => r.id === id);
  if (i < 0) return;
  cancelRowJob(batch.rows[i], deps);
  batch.rows.splice(i, 1);
}

/**
 * startBatch enters batch mode for a set of files and uploads them one at a
 * time (rows appear immediately and fill in as the uploads answer; a failed
 * upload marks only its row). Resolves when every upload settled.
 */
export async function startBatch(files: readonly File[], deps: BatchDeps = {}): Promise<void> {
  const up = dep(deps, 'upload');
  enterBatch();
  const gen = generation;
  const ids = files.map((f) => addPendingRow(f.name, f));
  for (let i = 0; i < files.length; i++) {
    try {
      const src = await up(files[i]);
      if (gen !== generation) return;
      rowUploaded(ids[i], src);
    } catch (e) {
      if (gen !== generation) return;
      rowUploadFailed(ids[i], messageOf(e));
    }
  }
}

/**
 * batchOpsCfg is the shared op configuration a batch row renders with: the
 * edited stack with every per-source (geometry-dependent) op forced off —
 * delay, trim, crop, auto-crop, resize, flip/rotate and the overlays — and
 * the row's own unpremultiply. What remains is exactly the batch-editable
 * set: fps, speed, reverse, bounce, background keying and feather.
 */
export function batchOpsCfg(c: OpsCfg, unpremultiply: boolean): OpsCfg {
  return {
    ...c,
    unpremultiply,
    delay: { ...c.delay, enabled: false },
    trim: { ...c.trim, enabled: false },
    crop: { ...c.crop, enabled: false },
    autocrop: { ...c.autocrop, enabled: false },
    resize: { ...c.resize, enabled: false },
    flipRotate: { ...c.flipRotate, enabled: false },
    overlays: [],
  };
}

/**
 * rowRecipe is the recipe one batch row submits: the row's source alone (no
 * overlay assets — overlays are disabled in batch), the shared global ops
 * with the row's unpremultiply, and the shared Output card.
 */
export function rowRecipe(row: Pick<BatchRow, 'source' | 'unpremultiply'>, c: OpsCfg, out: OutputCfg): Recipe | null {
  if (!row.source) return null;
  return {
    v: RECIPE_VERSION,
    sources: [row.source.hash],
    ops: buildOps(batchOpsCfg(c, row.unpremultiply)),
    output: buildOutput(out),
  };
}

function finishRow(row: BatchRow, job: Job): void {
  watchers.delete(row.id);
  row.job = job;
  row.result = job.result ?? null;
  row.running = false;
  if (!row.result) row.error = 'The job finished without a result manifest';
}

function failRow(row: BatchRow, message: string, job?: Job): void {
  watchers.delete(row.id);
  if (job) row.job = job;
  row.error = message;
  row.running = false;
}

/** renderRow submits one row's recipe and follows its progress (re-entrant calls while running are ignored). */
export async function renderRow(id: number, deps: BatchDeps = {}): Promise<void> {
  const row = rowById(id);
  if (!row || !row.source || row.uploading || row.running) return;
  const recipe = rowRecipe(row, app.ops, app.output);
  if (!recipe) return;
  watchers.get(id)?.();
  watchers.delete(id);
  const gen = generation;
  row.running = true;
  row.error = '';
  row.result = null;
  row.job = null;
  row.savedAs = '';
  row.saveError = '';

  let job: Job;
  try {
    job = await dep(deps, 'submit')(recipe);
  } catch (e) {
    if (gen !== generation) return;
    const r = rowById(id);
    if (r) failRow(r, messageOf(e));
    return;
  }
  const r = rowById(id);
  if (gen !== generation || !r) {
    // The batch was left / the row removed while submitting: nobody will
    // show this job, so do not leave it holding a render slot.
    if (job.state === 'queued' || job.state === 'running') void dep(deps, 'cancel')(job.id).catch(() => undefined);
    return;
  }
  r.job = job;
  if (job.state === 'done') {
    finishRow(r, job);
    return;
  }
  if (job.state === 'error') {
    failRow(r, job.error || 'Render failed', job);
    return;
  }
  const stop = dep(
    deps,
    'watch',
  )(job.id, {
    progress: (j) => {
      const cur = gen === generation ? rowById(id) : null;
      if (cur) cur.job = j;
    },
    done: (j) => {
      const cur = gen === generation ? rowById(id) : null;
      if (cur) finishRow(cur, j);
    },
    error: (message, j) => {
      const cur = gen === generation ? rowById(id) : null;
      if (cur) failRow(cur, message, j);
    },
  });
  watchers.set(id, stop);
}

/**
 * retryRow is the error row's Retry: a row with a source re-renders
 * (renderRow); a row whose UPLOAD failed still holds the dropped File
 * (BatchRow.file), so Retry re-uploads it instead of being permanently
 * disabled (WEB-10) — on success the row is "Ready" again for Render all.
 */
export async function retryRow(id: number, deps: BatchDeps = {}): Promise<void> {
  const row = rowById(id);
  if (!row) return;
  if (row.source) return renderRow(id, deps);
  const file = row.file;
  if (!file || row.uploading) return;
  const gen = generation;
  row.uploading = true;
  row.error = '';
  try {
    const src = await dep(deps, 'upload')(file);
    if (gen !== generation) return;
    rowUploaded(id, src);
  } catch (e) {
    if (gen !== generation) return;
    rowUploadFailed(id, messageOf(e));
  }
}

/** renderAll renders every uploaded row (jobs run concurrently; the server queue paces them). */
export async function renderAll(deps: BatchDeps = {}): Promise<void> {
  const ids = batch.rows.filter((r) => r.source && !r.uploading && !r.running).map((r) => r.id);
  await Promise.all(ids.map((id) => renderRow(id, deps)));
}

/** rowPrimary is the row's primary result file (null while unrendered / failed). */
export function rowPrimary(row: Pick<BatchRow, 'result'>): ResultFile | null {
  return row.result ? groupFiles(row.result.files).primary : null;
}

/** saveRow writes the row's primary file to /output (features.outputSave) and records the final name. */
export async function saveRow(id: number, deps: BatchDeps = {}): Promise<void> {
  const row = rowById(id);
  if (!row || row.saving) return;
  const result: Result | null = row.result;
  const primary = rowPrimary(row);
  if (!result || !primary) return;
  const gen = generation;
  row.saving = true;
  row.saveError = '';
  try {
    const name = await dep(deps, 'save')(result.recipeHash, primary.name);
    const cur = gen === generation ? rowById(id) : null;
    if (cur) cur.savedAs = name;
  } catch (e) {
    const cur = gen === generation ? rowById(id) : null;
    if (cur) cur.saveError = messageOf(e);
  } finally {
    const cur = gen === generation ? rowById(id) : null;
    if (cur) cur.saving = false;
  }
}

/** saveAll saves every rendered, not-yet-saved row's primary to /output. */
export async function saveAll(deps: BatchDeps = {}): Promise<void> {
  const ids = batch.rows.filter((r) => rowPrimary(r) && !r.savedAs && !r.saving).map((r) => r.id);
  for (const id of ids) await saveRow(id, deps);
}
