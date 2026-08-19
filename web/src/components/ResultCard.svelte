<script lang="ts">
  import { downloadURL, type Result, type ResultFile } from '../lib/api';
  import { fmtBytes, fmtKiB, fmtNum, fmtSeconds } from '../lib/format';
  import type { Backdrop } from '../lib/state.svelte';
  import { startRender } from '../lib/render.svelte';
  import BackdropToggle from './BackdropToggle.svelte';
  import DiscordChecks from './DiscordChecks.svelte';

  interface Props {
    result: Result;
    running: boolean;
  }
  let { result, running }: Props = $props();

  // Judge transparency against Discord dark first; independent of the source preview.
  let backdrop = $state<Backdrop>('dark');
  let showTools = $state(false);
  let showRecipe = $state(false);

  const files = $derived(result.files ?? []);
  const toolList = $derived(Object.entries(result.tools ?? {}).sort(([a], [b]) => a.localeCompare(b)));
  // Loop count the recipe asked for (0 = forever, N = play N+1 times). With no
  // Discord target a finite count is a legitimate choice, not a warning.
  const loopCount = $derived(result.recipe.output.loop ?? 0);

  function sizeClass(f: ResultFile): string {
    if (f.limit <= 0) return '';
    return f.bytes <= f.limit ? 'ok' : 'bad';
  }
</script>

<section class="card result">
  <div class="head">
    <h2>Result</h2>
    <div class="row">
      <span class="muted small">
        {#if result.cached}served from cache{:else}rendered in {result.renderMs >= 1000
            ? `${(result.renderMs / 1000).toFixed(1)} s`
            : `${result.renderMs} ms`}{/if}
      </span>
      <BackdropToggle bind:value={backdrop} />
    </div>
  </div>

  {#if files.length === 0}
    <p class="note error">The manifest lists no files.</p>
  {/if}

  {#each files as f (f.url)}
    <div class="file">
      <div class="stage backdrop-{backdrop}">
        <img src={f.url} alt="Rendered {f.format}" draggable="true" />
      </div>

      <div class="facts row">
        <span class="badge {sizeClass(f)}" title="{f.bytes.toLocaleString('en-US')} bytes{f.limit ? ` of ${f.limit.toLocaleString('en-US')} allowed` : ''}">
          <b>{fmtKiB(f.bytes)}</b>{#if f.limit > 0}&nbsp;/ {fmtKiB(f.limit)}{/if}&nbsp;KiB
        </span>
        <span class="badge mono">{f.format}</span>
        <span class="badge">{f.width}×{f.height}</span>
        <span class="badge">{f.frames} frame{f.frames === 1 ? '' : 's'}</span>
        <span class="badge">{fmtNum(f.fps)} fps</span>
        <span class="badge">{fmtSeconds(f.duration)}</span>
        {#if f.report}
          <span class="badge" class:ok={f.report.hasAlpha} title="Whether the file carries transparency">
            alpha {f.report.hasAlpha ? 'yes' : 'no'}
          </span>
          {#if !f.report.loopForever}
            {#if !f.report.target && loopCount > 0}
              <span class="badge" title="Loop count {loopCount} as requested (no Discord target)">plays {loopCount + 1}×</span>
            {:else}
              <span class="badge warn">does not loop forever</span>
            {/if}
          {/if}
        {/if}
      </div>

      {#if f.limit > 0 && f.bytes > f.limit}
        <p class="note error">
          {fmtBytes(f.bytes - f.limit)} over the {fmtKiB(f.limit)} KiB limit. Try lossy, fewer colours, lower fps or a shorter
          clip (the automatic fit search arrives in Phase 2).
        </p>
      {/if}

      {#if f.report}
        <DiscordChecks report={f.report} />
      {:else}
        <p class="hint">No Discord lint report for this file.</p>
      {/if}

      <div class="row actions">
        <a class="btn primary" href={downloadURL(f.url)} download={f.name}>Download {f.name}</a>
        <a class="btn" href={f.url} target="_blank" rel="noopener">Open in new tab</a>
        <button type="button" onclick={() => void startRender()} disabled={running}>Render again</button>
        <span class="hint">Drag the image straight into Discord to upload it.</span>
      </div>
    </div>
  {/each}

  <div class="foot small muted">
    <span class="mono" title="recipe hash {result.recipeHash}">{result.recipeHash.slice(0, 16)}…</span>
    <span class="row">
      <button type="button" class="ghost sm" onclick={() => (showRecipe = !showRecipe)}>{showRecipe ? 'hide' : 'show'} recipe</button>
      {#if toolList.length}
        <button type="button" class="ghost sm" onclick={() => (showTools = !showTools)}>{showTools ? 'hide' : 'show'} tool versions</button>
      {/if}
    </span>
  </div>
  {#if showRecipe}
    <pre class="recipe mono small">{JSON.stringify(result.recipe, null, 2)}</pre>
  {/if}
  {#if showTools}
    <ul class="tools mono small">
      {#each toolList as [name, version] (name)}<li><b>{name}</b> {version}</li>{/each}
    </ul>
  {/if}
</section>

<style>
  .result {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px;
  }
  .head h2 {
    margin: 0;
  }
  .file {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .stage {
    display: flex;
    align-items: center;
    justify-content: center;
    min-height: 140px;
    max-height: 60vh;
    padding: 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    overflow: auto;
  }
  .stage img {
    max-width: 100%;
    max-height: calc(60vh - 26px);
    display: block;
  }
  .facts {
    gap: 5px;
  }
  .actions {
    gap: 8px;
  }
  .foot {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .tools {
    margin: 0;
    padding: 0 0 0 4px;
    list-style: none;
    columns: 2;
  }
  .recipe {
    margin: 0;
    padding: 8px 10px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    max-height: 240px;
    overflow: auto;
    white-space: pre;
  }
</style>
