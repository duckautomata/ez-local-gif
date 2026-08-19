// Render job lifecycle: POST /api/jobs → SSE progress → Result.

import {
  cancelJob,
  isAbortError,
  messageOf,
  RECIPE_VERSION,
  submitJob,
  watchJob,
  type Job,
  type Recipe,
  type Result,
} from './api';
import { app, buildOps, buildOutput } from './state.svelte';
import { toast } from './toast.svelte';

export interface RenderState {
  running: boolean;
  job: Job | null;
  result: Result | null;
  error: string;
}

export const render = $state<RenderState>({
  running: false,
  job: null,
  result: null,
  error: '',
});

let stopWatch: (() => void) | null = null;

// Generation of the render state: bumped by every startRender/resetRender so
// an await that resolves after the state was reset (a new source arrived
// while POST /api/jobs was in flight) cannot revive the old job.
let generation = 0;

/** currentRecipe builds the recipe for the current source, ops and output. */
export function currentRecipe(): Recipe | null {
  const src = app.source;
  if (!src) return null;
  return {
    v: RECIPE_VERSION,
    sources: [src.hash],
    ops: buildOps(app.ops),
    output: buildOutput(app.output),
  };
}

function finish(job: Job): void {
  stopWatch = null;
  render.job = job;
  render.result = job.result ?? null;
  render.running = false;
  if (!render.result) {
    render.error = 'The job finished without a result manifest';
    toast.error(render.error);
  }
}

function fail(message: string, job?: Job): void {
  stopWatch = null;
  if (job) render.job = job;
  render.error = message;
  render.running = false;
  toast.error(message);
}

/** startRender submits the current recipe. Re-entrant calls while running are ignored. */
export async function startRender(): Promise<void> {
  if (render.running) return;
  const recipe = currentRecipe();
  if (!recipe) {
    toast.info('Upload a file first');
    return;
  }
  stopWatch?.();
  stopWatch = null;
  const gen = ++generation;
  render.running = true;
  render.error = '';
  render.result = null;
  render.job = null;

  let job: Job;
  try {
    job = await submitJob(recipe);
  } catch (e) {
    if (gen !== generation) return; // reset while submitting: the state was already cleared
    fail(isAbortError(e) ? 'Cancelled' : messageOf(e));
    return;
  }
  if (gen !== generation) {
    // The source was replaced while the job was being submitted: nobody
    // will show this job, so do not leave it running on the server.
    if (job.state === 'queued' || job.state === 'running') void cancelJob(job.id).catch(() => undefined);
    return;
  }
  render.job = job;
  if (job.state === 'done') {
    finish(job);
    return;
  }
  if (job.state === 'error') {
    fail(job.error || 'Render failed', job);
    return;
  }
  // The watcher stops itself once stopped, but guard the callbacks with the
  // generation anyway so a snapshot of a job that belongs to a replaced
  // source can never land in the fresh state.
  stopWatch = watchJob(job.id, {
    progress: (j) => {
      if (gen === generation) render.job = j;
    },
    done: (j) => {
      if (gen === generation) finish(j);
    },
    error: (message, j) => {
      if (gen === generation) fail(message, j);
    },
  });
}

/** cancelRender asks the server to kill the running job. */
export async function cancelRender(): Promise<void> {
  const job = render.job;
  if (!render.running || !job) return;
  const gen = generation;
  try {
    await cancelJob(job.id);
  } catch (e) {
    if (gen !== generation) return;
    toast.error(`Cancel failed: ${messageOf(e)}`);
    return;
  }
  if (gen !== generation) return; // reset meanwhile: nothing left to mark as cancelled
  stopWatch?.();
  stopWatch = null;
  render.running = false;
  render.error = 'Cancelled';
}

/**
 * resetRender forgets the current job and result — called when a new source
 * replaces the old one, so nothing rendered from a different file stays on
 * screen. A job still in flight is unsubscribed and cancelled on the server
 * (best effort: it would otherwise hold a render slot nobody is watching);
 * a submit or cancel that is still awaiting the server is ignored when it
 * lands.
 */
export function resetRender(): void {
  generation++;
  stopWatch?.();
  stopWatch = null;
  const job = render.job;
  if (render.running && job && (job.state === 'queued' || job.state === 'running')) {
    void cancelJob(job.id).catch(() => undefined);
  }
  render.running = false;
  render.job = null;
  render.result = null;
  render.error = '';
}
