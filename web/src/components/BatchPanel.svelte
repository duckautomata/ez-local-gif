<script lang="ts">
  // Batch mode (Phase 4): one row per dropped file — name, compact probe
  // facts, per-row unpremultiply (auto from the probe), per-row render
  // progress (the ordinary job SSE) and a per-row result chip (size vs the
  // Discord limit, pass/fail) with Download / Save to /output. Row errors
  // are isolated. "Open in editor" leaves batch mode and seeds the normal
  // single view with that row's source.
  import { downloadURL, type Source } from '../lib/api';
  import { batch, exitBatch, removeRow, retryRow, rowPrimary, saveRow, type BatchRow } from '../lib/batch.svelte';
  import { caps } from '../lib/capabilities.svelte';
  import { fmtKiB, fmtNum, fmtSeconds } from '../lib/format';
  import { resetRender } from '../lib/render.svelte';
  import { sizeState } from '../lib/result';
  import { setSource } from '../lib/state.svelte';

  const canSave = $derived(caps.features.outputSave);

  function openInEditor(row: BatchRow) {
    const src: Source | null = row.source;
    if (!src) return;
    exitBatch();
    resetRender();
    setSource(src);
  }

  function leave() {
    exitBatch();
    resetRender();
  }

  function percentOf(row: BatchRow): number {
    if (!row.job) return 0;
    return Math.min(100, Math.max(0, row.job.percent));
  }
</script>

<section class="card rows">
  <div class="head">
    <h2>Batch — {batch.rows.length} file{batch.rows.length === 1 ? '' : 's'}</h2>
    <button type="button" class="sm ghost" onclick={leave} title="Leave batch mode (running renders are cancelled)">✕ Leave batch</button>
  </div>

  {#each batch.rows as row (row.id)}
    {@const info = row.source?.info ?? null}
    {@const primary = rowPrimary(row)}
    <div class="brow" class:failed={!!row.error}>
      <div class="row top">
        <span class="name" title={row.name}>{row.name}</span>
        {#if info}
          <span class="badge">{info.width}×{info.height}</span>
          {#if !info.isStill}
            {#if info.fps > 0}<span class="badge">{fmtNum(info.fps)} fps</span>{/if}
            {#if info.duration > 0}<span class="badge">{fmtSeconds(info.duration)}</span>{/if}
          {:else}
            <span class="badge">still</span>
          {/if}
          <span class="badge" class:ok={info.hasAlpha}>alpha {info.hasAlpha ? 'yes' : 'no'}</span>
        {/if}
        <span class="spacer"></span>
        <button type="button" class="sm" onclick={() => openInEditor(row)} disabled={!row.source} title="Leave batch and open this file in the single-source editor (for trim, crop, overlays…)">
          Open in editor
        </button>
        <button type="button" class="sm ghost" onclick={() => removeRow(row.id)} title="Remove this row (a running render is cancelled; the file stays in the store)">✕</button>
      </div>

      {#if info?.hasAlpha}
        <label class="inline small" title="Auto-detected from the probe; applies to this file only">
          <input type="checkbox" bind:checked={row.unpremultiply} />
          <span>Source alpha is premultiplied — unpremultiply {info.premultiplied ? '(detected)' : ''}</span>
        </label>
      {/if}

      {#if row.uploading}
        <p class="small muted">Uploading…</p>
      {:else if row.error}
        <!-- Retry re-renders a rendered row; after an upload failure it re-uploads the kept File (WEB-10) -->
        <p class="note error small">
          {row.error}
          <button
            type="button"
            class="sm"
            onclick={() => void retryRow(row.id)}
            disabled={!row.source && !row.file}
            title={row.source ? 'Render this row again' : 'Upload this file again'}
          >
            Retry
          </button>
        </p>
      {:else if row.running}
        <div class="prog">
          <div class="row between small">
            <span class="muted">{row.job?.stage || row.job?.state || 'queued'}</span>
            <span class="mono muted">{percentOf(row).toFixed(0)}%</span>
          </div>
          <div class="progress"><div style:width="{percentOf(row)}%"></div></div>
        </div>
      {:else if primary}
        <div class="row chip">
          <span class="badge {sizeState(primary)}" title="{primary.bytes.toLocaleString('en-US')} bytes{primary.limit ? ` of ${primary.limit.toLocaleString('en-US')} allowed` : ''}">
            <b>{fmtKiB(primary.bytes)}</b>{#if primary.limit > 0}&nbsp;/ {fmtKiB(primary.limit)}{/if}&nbsp;KiB
          </span>
          {#if primary.report}
            <span class="badge {primary.report.ok ? 'ok' : 'bad'}">{primary.report.ok ? 'Discord-safe ✓' : 'fails the Discord check'}</span>
          {/if}
          {#if row.result?.cached}<span class="badge muted">cached</span>{/if}
          <a class="btn sm" href={downloadURL(primary.url)} download={primary.name}>Download</a>
          {#if canSave}
            <button type="button" class="sm" onclick={() => void saveRow(row.id)} disabled={row.saving} title="Write the primary file into the server's /output directory">
              {row.saving ? 'Saving…' : 'Save to /output'}
            </button>
          {/if}
          {#if row.savedAs}<span class="small muted">→ /output/{row.savedAs}</span>{/if}
          {#if row.saveError}<span class="small bad">save failed: {row.saveError}</span>{/if}
          {#if primary.desc}<span class="small muted desc" title={primary.desc}>{primary.desc}</span>{/if}
        </div>
      {:else if row.result}
        <p class="note error small">The job finished without a primary file.</p>
      {:else}
        <p class="small muted">Ready — “Render all” renders every row with the shared ops + output.</p>
      {/if}
    </div>
  {/each}

  {#if batch.rows.length === 0}
    <p class="hint">No files — drop several videos/animations onto the zone above.</p>
  {/if}
</section>

<style>
  .rows {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .head h2 {
    margin: 0;
  }
  .brow {
    display: flex;
    flex-direction: column;
    gap: 5px;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    background: var(--panel-2);
  }
  .brow.failed {
    border-color: var(--red);
  }
  .top {
    gap: 6px;
    align-items: center;
    flex-wrap: wrap;
  }
  .name {
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
    max-width: 40%;
  }
  .spacer {
    flex: 1;
  }
  .chip {
    gap: 6px;
    align-items: center;
    flex-wrap: wrap;
  }
  .prog {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .row.between {
    justify-content: space-between;
  }
  .desc {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
  .bad {
    color: var(--red);
  }
  .note.small {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
</style>
