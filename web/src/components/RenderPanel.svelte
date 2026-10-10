<script lang="ts">
  import { caps } from '../lib/capabilities.svelte';
  import { fmtBytesShort, fmtNum } from '../lib/format';
  import { cancelRender, render, startRender } from '../lib/render.svelte';
  import { app, effectiveOps, masterVerdict, MAX_EXTRACT_FRAMES, planMaster } from '../lib/state.svelte';
  import ResultCard from './ResultCard.svelte';

  const job = $derived(render.job);
  const canRender = $derived(app.source !== null && !render.running);

  // The live frame-master estimate (state.planMaster mirrors jobs'
  // admission) and its verdict against the caps the server published.
  // Render stays enabled whatever the verdict says — the server's admission
  // has the last word (auto-crop recipes are judged after detection, an
  // unknown cap never binds) — the note only tells the user up-front what
  // the refusal would be and how much clip fits, so the trim / fps / crop /
  // resize happens here instead of in another tool.
  const est = $derived(app.source ? planMaster(app.source.info, effectiveOps(app.ops, app.output), app.output) : null);
  const verdict = $derived(est ? masterVerdict(est, caps) : null);
  /**
   * the muted one-liner: "2560×1440 · 704 frames · ~9.7 GiB of decoded
   * frames · about 2× that on scratch"; a reversed png / jpeg adds the RAM
   * its reverse filter holds before the one frame comes out ("· ~1.2 GiB
   * buffered by the reverse")
   */
  const estimateLine = $derived.by(() => {
    if (!est) return '';
    let s = `${est.upperBound ? 'up to ' : ''}${est.w}×${est.h} · ${est.frames} ${est.frames === 1 ? 'frame' : 'frames'} · ~${fmtBytesShort(est.bytes)} of decoded frames`;
    if (est.factor > 1) s += ` · about ${est.factor}× that on scratch`;
    if (est.bufferBytes > 0) s += ` · ~${fmtBytesShort(est.bufferBytes)} buffered by the reverse`;
    if (est.upperBound) s += ' before crop-to-content';
    return s;
  });
  /** what fits instead, for the refusal note: "At 2560×1440 about 145 frames fit (4.8 s at 30 fps)" */
  const fitsLine = $derived.by(() => {
    if (!est || !verdict) return '';
    if (verdict.maxFrames < 1) return `Not even one frame fits at ${est.w}×${est.h}: crop or resize the output`;
    let s = `At ${est.w}×${est.h} about ${verdict.maxFrames} ${verdict.maxFrames === 1 ? 'frame fits' : 'frames fit'}`;
    if (est.fps > 0) s += ` (${fmtNum(verdict.maxSeconds, 1)} s at ${fmtNum(est.fps)} fps)`;
    return `${s}: trim to that, lower the fps, crop or resize the output`;
  });
  /**
   * the .note.error text when the verdict is over ('' otherwise): the
   * server's refusal, in advance, with the way out. With auto-crop on and
   * no output size the figure is the pre-crop frame (planMaster's upper
   * bound) while the server measures the master AFTER the content box is
   * resolved, so it may well admit the render — that variant says "may be
   * refused" instead of promising a refusal it cannot know. The scratch
   * figure is the budget the server published: shm_size minus what the
   * still / proxy memos may hold (jobs.NewManager), not shm_size itself.
   */
  const overNote = $derived.by(() => {
    if (!est || !verdict?.over) return '';
    const pre = est.upperBound ? 'Up to ' : '';
    const post = est.upperBound ? ' before crop-to-content' : '';
    const refused = est.upperBound ? ' unless crop-to-content shrinks the frame enough, so Render may be refused' : ', so Render will be refused';
    const overCap = `over this server's ${fmtBytesShort(caps.maxMasterBytes)} limit${refused}. ${fitsLine} (or raise EZLG_MAX_MASTER_BYTES on the server).`;
    if (verdict.by === 'buffer') {
      // a reversed png / jpeg: one frame of master, but the reverse filter holds the whole clip in RAM before it comes out (render.go admitReversed)
      return `${pre}~${fmtBytesShort(est.bufferBytes)} buffered by the reverse (${est.bufferFrames} frames)${post} — ${overCap}`;
    }
    const what = `${pre}~${fmtBytesShort(est.bytes)} of decoded frames${post}`;
    if (verdict.by === 'cap') return `${what} — ${overCap}`;
    const multiple = est.factor > 1 ? ` (about ${est.factor}× that on scratch)` : '';
    return `${what}${multiple} — more than this server's scratch budget (${fmtBytesShort(caps.scratchBudgetBytes)}, shm_size minus the preview cache) can hold for this output${refused}. ${fitsLine} (or raise shm_size / point EZLG_SCRATCH at a larger filesystem).`;
  });
  // jobs refuses a frames export above MaxExtractFrames on the plan's full
  // frame count before any decode; the master cap is a separate matter.
  const framesOver = $derived(est !== null && app.output.format === 'frames' && est.frames > MAX_EXTRACT_FRAMES);
  const stageLabel: Record<string, string> = {
    probe: 'Probing source',
    master: 'Decoding frames',
    encode: 'Encoding',
    fit: 'Fitting to size',
    lint: 'Discord lint',
    verify: 'Verifying',
    done: 'Done',
  };
  const stage = $derived(job ? (stageLabel[job.stage] ?? job.stage ?? job.state) : '');
  const percent = $derived(job ? Math.min(100, Math.max(0, job.percent)) : 0);
  const indeterminate = $derived(render.running && (!job || job.state === 'queued' || percent <= 0));
</script>

<section class="card render">
  <div class="row top">
    <button type="button" class="primary big" onclick={() => void startRender()} disabled={!canRender} title="Ctrl+Enter">
      {#if render.running}Rendering…{:else}Render{/if}
    </button>
    {#if render.running}
      <button type="button" class="danger" onclick={() => void cancelRender()} disabled={!job}>Cancel</button>
    {/if}
    <span class="hint"><kbd>Ctrl</kbd>+<kbd>Enter</kbd>{#if !app.source}&nbsp;· upload a file first{/if}</span>
  </div>

  {#if est}
    {#if overNote}
      <p class="note error estimate">{overNote}</p>
    {:else}
      <p class="estimate small muted">{estimateLine}</p>
    {/if}
    {#if framesOver}
      <p class="note error estimate">Frames export is capped at {MAX_EXTRACT_FRAMES} frames — trim the clip or lower the fps.</p>
    {/if}
  {/if}

  {#if render.running}
    <div class="prog">
      <div class="row between small">
        <span><b>{stage || 'Queued'}</b>{#if job?.message}&nbsp;· <span class="muted">{job.message}</span>{/if}</span>
        <span class="mono muted">{percent.toFixed(0)}%</span>
      </div>
      <div class="progress" class:indeterminate><div style:width="{percent}%"></div></div>
    </div>
  {/if}

  {#if render.error && !render.running}
    <div class="note error err">
      <b>{render.error === 'Cancelled' ? 'Cancelled' : 'Render failed'}</b>
      {#if render.error !== 'Cancelled'}<span class="mono">{render.error}</span>{/if}
    </div>
  {/if}
</section>

{#if render.result}
  <ResultCard result={render.result} running={render.running} />
{/if}

<style>
  .render {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .top {
    gap: 10px;
  }
  button.big {
    padding: 8px 22px;
    font-size: 15px;
  }
  .prog {
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .row.between {
    justify-content: space-between;
  }
  .err {
    display: flex;
    flex-direction: column;
    gap: 3px;
    word-break: break-word;
    white-space: pre-wrap;
  }
</style>
