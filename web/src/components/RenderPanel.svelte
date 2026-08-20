<script lang="ts">
  import { cancelRender, render, startRender } from '../lib/render.svelte';
  import { app } from '../lib/state.svelte';
  import ResultCard from './ResultCard.svelte';

  const job = $derived(render.job);
  const canRender = $derived(app.source !== null && !render.running);
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
