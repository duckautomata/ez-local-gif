<script lang="ts">
  // Batch render controls (Phase 4): "Render all" submits one ordinary job
  // per row (POST /api/jobs — the server queue paces the concurrency);
  // "Save all to /output" saves every rendered primary (features.outputSave).
  import { batch, renderAll, rowPrimary, saveAll } from '../lib/batch.svelte';
  import { caps } from '../lib/capabilities.svelte';

  const ready = $derived(batch.rows.filter((r) => r.source && !r.uploading));
  const running = $derived(batch.rows.filter((r) => r.running).length);
  const done = $derived(batch.rows.filter((r) => rowPrimary(r)).length);
  const failed = $derived(batch.rows.filter((r) => !r.uploading && !r.running && r.error).length);
  const unsaved = $derived(batch.rows.filter((r) => rowPrimary(r) && !r.savedAs).length);
  const canSaveAll = $derived(caps.features.outputSave && unsaved > 0);
</script>

<section class="card render">
  <div class="row top">
    <button type="button" class="primary big" onclick={() => void renderAll()} disabled={ready.length === 0 || running > 0} title="Ctrl+Enter — one job per row, shared ops + output">
      Render all ({ready.length})
    </button>
    {#if canSaveAll}
      <button type="button" onclick={() => void saveAll()}>Save all to /output ({unsaved})</button>
    {/if}
    <span class="hint"><kbd>Ctrl</kbd>+<kbd>Enter</kbd></span>
  </div>
  {#if running > 0 || done > 0 || failed > 0}
    <p class="small muted">
      {done} done{#if running > 0}&nbsp;· {running} running{/if}{#if failed > 0}&nbsp;· <span class="bad">{failed} failed</span>{/if}
      — progress and results live on each row.
    </p>
  {/if}
</section>

<style>
  .render {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .top {
    gap: 10px;
    align-items: center;
    flex-wrap: wrap;
  }
  button.big {
    padding: 8px 22px;
    font-size: 15px;
  }
  .bad {
    color: var(--red);
  }
</style>
