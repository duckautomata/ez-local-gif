<script lang="ts">
  import { downloadURL, type Result, type ResultFile } from '../lib/api';
  import { browserTabOpener, editAsSource } from '../lib/editsource';
  import { fmtBytes, fmtKiB, fmtNum, fmtSeconds } from '../lib/format';
  import { fitsFormat } from '../lib/presets';
  import { startRender } from '../lib/render.svelte';
  import { descLine, groupFiles, isFramesResult, isImageFormat, sizeState } from '../lib/result';
  import type { Backdrop } from '../lib/state.svelte';
  import { toast } from '../lib/toast.svelte';
  import BackdropToggle from './BackdropToggle.svelte';
  import DiscordChecks from './DiscordChecks.svelte';
  import InChat from './InChat.svelte';

  interface Props {
    result: Result;
    running: boolean;
  }
  let { result, running }: Props = $props();

  // Judge transparency against Discord dark first; independent of the source preview.
  let backdrop = $state<Backdrop>('dark');
  let showTools = $state(false);
  let showRecipe = $state(false);
  /** names of files whose "edit as source" request is in flight */
  let editing = $state<Record<string, boolean>>({});

  const groups = $derived(groupFiles(result.files));
  const framesResult = $derived(isFramesResult(groups));
  const primary = $derived(groups.primary);
  const target = $derived(result.recipe.output.target ?? '');
  // A fit ran only when the recipe carried a budget AND the format has a fit
  // ladder: the server ignores fitBytes for the rest (jobs.fitFormats /
  // presets.FIT_FORMATS), so e.g. a static PNG from an old recipe must not
  // claim "Fit search ran".
  const fitRequested = $derived((result.recipe.output.fitBytes ?? 0) > 0 && fitsFormat(result.recipe.output.format));
  // The primary's desc gets the bold 'Fit:' label only when a fit actually
  // ran; a non-fit desc (Optimize's "gifsicle: …") shows as neutral settings.
  const line = $derived(descLine(primary, fitRequested));
  const toolList = $derived(Object.entries(result.tools ?? {}).sort(([a], [b]) => a.localeCompare(b)));
  // Loop count the recipe asked for (0 = forever, N = play N+1 times). With no
  // Discord target a finite count is a legitimate choice, not a warning.
  const loopCount = $derived(result.recipe.output.loop ?? 0);
  const nothing = $derived((result.files ?? []).length === 0);

  const openTab = browserTabOpener();
  async function edit(f: ResultFile) {
    if (editing[f.name]) return;
    editing[f.name] = true;
    try {
      const out = await editAsSource(result.recipeHash, f, { openTab });
      if (!out.ok) toast.error(`Edit as source failed: ${out.error}`);
      else if (out.blocked) toast.info(`Pop-up blocked — open ${out.url} to edit ${f.name}`);
    } finally {
      delete editing[f.name];
    }
  }
</script>

{#snippet facts(f: ResultFile)}
  <div class="facts row">
    <span class="badge {sizeState(f)}" title="{f.bytes.toLocaleString('en-US')} bytes{f.limit ? ` of ${f.limit.toLocaleString('en-US')} allowed` : ''}">
      <b>{fmtKiB(f.bytes)}</b>{#if f.limit > 0}&nbsp;/ {fmtKiB(f.limit)}{/if}&nbsp;KiB
    </span>
    <span class="badge mono">{f.format}</span>
    <span class="badge">{f.width}×{f.height}</span>
    {#if f.frames > 0}<span class="badge">{f.frames} frame{f.frames === 1 ? '' : 's'}</span>{/if}
    {#if f.fps > 0}<span class="badge">{fmtNum(f.fps)} fps</span>{/if}
    {#if f.duration > 0}<span class="badge">{fmtSeconds(f.duration)}</span>{/if}
    {#if f.report}
      <span class="badge" class:ok={f.report.hasAlpha} title="Whether the file carries transparency">
        alpha {f.report.hasAlpha ? 'yes' : 'no'}
      </span>
      {#if !f.report.loopForever && f.frames > 1}
        {#if !f.report.target && loopCount > 0}
          <span class="badge" title="Loop count {loopCount} as requested (no Discord target)">plays {loopCount + 1}×</span>
        {:else}
          <span class="badge warn">does not loop forever</span>
        {/if}
      {/if}
    {/if}
  </div>
{/snippet}

{#snippet actions(f: ResultFile, primaryBtn: boolean)}
  <div class="row actions">
    <a class="btn" class:primary={primaryBtn} href={downloadURL(f.url)} download={f.name}>Download {f.name}</a>
    <a class="btn" href={f.url} target="_blank" rel="noopener">Open</a>
    <button type="button" onclick={() => void edit(f)} disabled={!!editing[f.name]} title="Register this file as a source and open it in a new tab">
      {editing[f.name] ? 'Opening…' : 'Edit as source ↗'}
    </button>
  </div>
{/snippet}

<section class="card result">
  <div class="head">
    <h2>Result</h2>
    <div class="row">
      <span class="muted small">
        {#if result.cached}served from cache{:else}rendered in {result.renderMs >= 1000
            ? `${(result.renderMs / 1000).toFixed(1)} s`
            : `${result.renderMs} ms`}{/if}
      </span>
      {#if !framesResult}<BackdropToggle bind:value={backdrop} />{/if}
    </div>
  </div>

  {#if nothing}
    <p class="note error">The manifest lists no files.</p>
  {/if}

  {#if primary}
    {@const f = primary}
    <div class="file">
      <div class="stage backdrop-{backdrop}">
        {#if isImageFormat(f.format)}
          <img src={f.url} alt="Rendered {f.format}" draggable="true" />
        {:else}
          <span class="muted">{f.name}</span>
        {/if}
      </div>

      {@render facts(f)}

      {#if line}
        <p class="fitline">
          {#if line.label === 'Fit'}<b>Fit:</b>{:else}<span class="muted">Settings:</span>{/if}
          {line.text}
        </p>
      {:else if fitRequested}
        <p class="fitline muted">Fit search ran; the primary is the mildest rung that fits.</p>
      {/if}

      {#if f.limit > 0 && f.bytes > f.limit}
        <p class="note error">
          {fmtBytes(f.bytes - f.limit)} over the {fmtKiB(f.limit)} KiB limit.
          {#if fitRequested}Even the harshest fit rung was too big — shorten the clip, lower the fps or allow downscaling.{:else}Turn on
            “Fit to ≤ … KiB” in the Output card, or try lossy / fewer colours / lower fps.{/if}
        </p>
      {/if}

      <InChat file={f} {target} />

      {#if f.report}
        <DiscordChecks report={f.report} />
      {:else}
        <p class="hint">No Discord lint report for this file.</p>
      {/if}

      <div class="row actions">
        <a class="btn primary" href={downloadURL(f.url)} download={f.name}>Download {f.name}</a>
        <a class="btn" href={f.url} target="_blank" rel="noopener">Open in new tab</a>
        <button type="button" onclick={() => void edit(f)} disabled={!!editing[f.name]} title="Register this file as a source and open it in a new tab">
          {editing[f.name] ? 'Opening…' : 'Edit as source ↗'}
        </button>
        <button type="button" onclick={() => void startRender()} disabled={running}>Render again</button>
        <span class="hint">Drag the image straight into Discord to upload it.</span>
      </div>
    </div>
  {/if}

  {#if groups.alternatives.length}
    <div class="alts">
      <h3>Alternatives <span class="muted small">fit-search runner-ups — harsher settings, smaller files</span></h3>
      <div class="alt-grid">
        {#each groups.alternatives as a (a.url)}
          <div class="alt">
            <div class="stage small backdrop-{backdrop}">
              {#if isImageFormat(a.format)}<img src={a.url} alt="Alternative {a.index ?? ''} ({a.format})" loading="lazy" />{/if}
            </div>
            <div class="row facts">
              <span class="badge {sizeState(a)}"><b>{fmtKiB(a.bytes)}</b> KiB</span>
              <span class="badge mono">{a.format}</span>
              <span class="badge">{a.width}×{a.height}</span>
              {#if a.fps > 0}<span class="badge">{fmtNum(a.fps)} fps</span>{/if}
            </div>
            {#if a.desc}<p class="small desc">{a.desc}</p>{/if}
            {#if a.report && !a.report.ok}<p class="small bad">fails the Discord check</p>{/if}
            {@render actions(a, false)}
          </div>
        {/each}
      </div>
    </div>
  {/if}

  {#each groups.others as o (o.url)}
    <div class="file">
      {@render facts(o)}
      {#if o.desc}<p class="small desc">{o.desc}</p>{/if}
      {@render actions(o, false)}
    </div>
  {/each}

  {#if groups.frames.length || groups.archive}
    <div class="frames">
      <div class="row between">
        <h3>
          {groups.frames.length} frame{groups.frames.length === 1 ? '' : 's'}
          {#if groups.frames[0]}<span class="muted small">· {groups.frames[0].width}×{groups.frames[0].height} · {groups.frames[0].format}</span>{/if}
        </h3>
        {#if groups.archive}
          <a class="btn primary" href={downloadURL(groups.archive.url)} download={groups.archive.name}>
            Download all (zip, {fmtBytes(groups.archive.bytes)})
          </a>
        {/if}
      </div>
      {#if groups.frames.length}
        <div class="grid backdrop-{backdrop}">
          {#each groups.frames as fr (fr.url)}
            <figure class="frame">
              <a href={fr.url} target="_blank" rel="noopener" title={fr.desc || fr.name}>
                <img src={fr.url} alt={fr.desc || fr.name} loading="lazy" decoding="async" />
              </a>
              <figcaption>
                <span class="idx mono">{fr.index ?? ''}</span>
                <span class="fdesc" title={fr.desc || fr.name}>{fr.desc || fr.name}</span>
                <span class="fbtns">
                  <a class="btn sm" href={downloadURL(fr.url)} download={fr.name} title="Download {fr.name}">↓</a>
                  <button type="button" class="sm" onclick={() => void edit(fr)} disabled={!!editing[fr.name]} title="Edit this frame as a source (new tab)">edit ↗</button>
                </span>
              </figcaption>
            </figure>
          {/each}
        </div>
      {/if}
      <div class="row actions">
        <button type="button" onclick={() => void startRender()} disabled={running}>Render again</button>
        <span class="hint">Thumbnails load lazily; open a frame for full size.</span>
      </div>
    </div>
  {/if}

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
  h3 {
    font-size: 13px;
    color: var(--muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  h3 .muted {
    text-transform: none;
    letter-spacing: 0;
    font-weight: 400;
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
  .stage.small {
    min-height: 90px;
    max-height: 200px;
    padding: 8px;
  }
  .stage.small img {
    max-height: 184px;
  }
  .facts {
    gap: 5px;
  }
  .fitline {
    font-size: 12.5px;
  }
  .actions {
    gap: 8px;
  }
  .alts,
  .frames {
    display: flex;
    flex-direction: column;
    gap: 8px;
    border-top: 1px solid var(--border);
    padding-top: 10px;
  }
  .alt-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 10px;
  }
  .alt {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 8px;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: var(--panel-2);
  }
  .desc {
    color: var(--text);
  }
  .row.between {
    justify-content: space-between;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(120px, 1fr));
    gap: 8px;
    padding: 8px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    max-height: 60vh;
    overflow: auto;
  }
  .frame {
    margin: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
  }
  .frame a {
    display: block;
    line-height: 0;
  }
  .frame img {
    width: 100%;
    height: 100px;
    object-fit: contain;
    display: block;
  }
  figcaption {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    background: rgba(30, 31, 34, 0.85);
    border-radius: 4px;
    padding: 2px 4px;
    color: var(--text);
    min-width: 0;
  }
  .idx {
    color: var(--muted);
    flex: none;
  }
  .fdesc {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
  .fbtns {
    display: inline-flex;
    gap: 3px;
    flex: none;
  }
  .fbtns .btn,
  .fbtns button {
    padding: 0 5px;
    font-size: 11px;
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
