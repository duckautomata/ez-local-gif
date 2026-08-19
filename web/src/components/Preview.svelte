<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import { fetchStill, type StillRequest } from '../lib/api';
  import { fmtSeconds, round } from '../lib/format';
  import { app, buildOps, buildOutput, previewDuration } from '../lib/state.svelte';
  import { StillScheduler, type StillView } from '../lib/still';
  import BackdropToggle from './BackdropToggle.svelte';
  import CropOverlay from './CropOverlay.svelte';

  // Preview stills are at most 480 px wide, 720 on wide screens.
  const wideQuery = window.matchMedia('(min-width: 1500px)');
  let wide = $state(wideQuery.matches);
  $effect(() => {
    const onChange = (e: MediaQueryListEvent) => (wide = e.matches);
    wideQuery.addEventListener('change', onChange);
    return () => wideQuery.removeEventListener('change', onChange);
  });
  const maxW = $derived(wide ? 720 : 480);

  const info = $derived(app.source?.info ?? null);
  const duration = $derived(info ? previewDuration(info, app.ops) : 0);
  const cropMode = $derived(app.ui.cropOpen && !!info);
  const t = $derived(Math.min(Math.max(0, app.ui.scrubT), Math.max(0, duration)));

  // Keep the scrubber inside the (trim/speed dependent) range.
  $effect(() => {
    if (app.ui.scrubT > duration) app.ui.scrubT = duration;
  });

  // The still request. In crop mode the op stack stops before crop and no
  // output fitting is applied, so the frame is the full source in source
  // pixel coordinates (the overlay maps display px → source px).
  const req = $derived.by((): StillRequest | null => {
    const src = app.source;
    if (!src) return null;
    return {
      src: src.hash,
      ops: buildOps(app.ops, { cropPreview: cropMode }),
      output: cropMode ? { format: app.output.format } : buildOutput(app.output),
      t: round(t, 3),
      maxW,
    };
  });

  // The still on screen (url), the in-flight flag and the last error live in a
  // $state object that StillScheduler mutates (debounce, abort of superseded
  // requests, object-URL lifecycle — see lib/still.ts).
  const view = $state<StillView>({ url: null, loading: false, error: '' });
  const still = new StillScheduler(view, {
    fetch: fetchStill,
    createURL: (b) => URL.createObjectURL(b),
    revokeURL: (u) => URL.revokeObjectURL(u),
  });
  let natural = $state({ w: 0, h: 0 });
  let imgEl = $state<HTMLImageElement | null>(null);
  type Zoom = 'fit' | 1 | 2 | 4;
  const zooms: readonly Zoom[] = ['fit', 1, 2, 4];
  let zoom = $state<Zoom>('fit');

  $effect(() => {
    const r = req;
    untrack(() => still.request(r));
  });

  function onImgLoad(e: Event) {
    const el = e.currentTarget as HTMLImageElement;
    natural = { w: el.naturalWidth, h: el.naturalHeight };
    still.imageLoaded();
  }

  function onImgError() {
    still.imageFailed();
  }

  function retry() {
    still.retry();
  }

  onDestroy(() => still.dispose());

  const zoomStyle = $derived(zoom === 'fit' || natural.w === 0 ? '' : `width:${natural.w * zoom}px;max-width:none;`);
</script>

<section class="card preview">
  <div class="toolbar">
    <span class="card-title">Preview</span>
    <div class="row">
      <BackdropToggle bind:value={app.ui.backdrop} />
      <div class="seg" role="group" aria-label="Zoom">
        {#each zooms as z (z)}
          <button type="button" aria-pressed={zoom === z} onclick={() => (zoom = z)}>{z === 'fit' ? 'Fit' : `${z}×`}</button>
        {/each}
      </div>
    </div>
  </div>

  <div class="stage backdrop-{app.ui.backdrop}" class:cropping={cropMode}>
    {#if view.url}
      <div class="img-wrap">
        <img
          bind:this={imgEl}
          src={view.url}
          alt="Preview frame"
          class:pixel={zoom !== 'fit' && zoom >= 2}
          style={zoomStyle}
          draggable="false"
          onload={onImgLoad}
          onerror={onImgError}
        />
        {#if cropMode && imgEl && info}
          <CropOverlay img={imgEl} srcW={info.width} srcH={info.height} />
        {/if}
      </div>
    {:else if !view.error}
      <div class="placeholder muted">{view.loading ? 'Rendering preview…' : 'No preview yet'}</div>
    {/if}
    {#if view.loading && view.url}<div class="spinner" aria-label="Loading"></div>{/if}
    {#if view.error}
      <div class="err">
        <b>Preview failed</b>
        <span>{view.error}</span>
        <button type="button" class="sm" onclick={retry}>Retry</button>
      </div>
    {/if}
  </div>

  <div class="scrub">
    <input
      type="range"
      class="wide"
      min="0"
      max={Math.max(0, duration)}
      step="0.01"
      disabled={duration <= 0}
      bind:value={app.ui.scrubT}
      aria-label="Scrub time"
    />
    <span class="time mono">{fmtSeconds(t)} / {fmtSeconds(duration)}</span>
  </div>
  <div class="row small muted meta">
    {#if natural.w > 0}<span>still {natural.w}×{natural.h}</span>{/if}
    {#if cropMode}<span class="warn">Crop mode: full frame shown, drag to set the rectangle</span>{/if}
    {#if !cropMode && info}<span>preview shows the op stack and output canvas · press <kbd>Ctrl</kbd>+<kbd>Enter</kbd> to render</span>{/if}
  </div>
</section>

<style>
  .preview {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .toolbar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px;
  }
  .toolbar .card-title {
    margin: 0;
  }
  .stage {
    position: relative;
    display: flex;
    align-items: center;
    justify-content: center;
    min-height: 200px;
    max-height: 70vh;
    overflow: auto;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
  }
  .img-wrap {
    position: relative;
    line-height: 0;
    margin: auto;
  }
  img {
    display: block;
    max-width: 100%;
    max-height: calc(70vh - 2px);
    user-select: none;
  }
  img.pixel {
    image-rendering: pixelated;
  }
  .stage.cropping img {
    max-height: none;
  }
  .placeholder {
    padding: 40px;
    background: rgba(30, 31, 34, 0.7);
    border-radius: var(--radius-sm);
  }
  .spinner {
    position: absolute;
    top: 8px;
    right: 8px;
    width: 16px;
    height: 16px;
    border: 2px solid rgba(255, 255, 255, 0.35);
    border-top-color: #fff;
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .err {
    position: absolute;
    left: 8px;
    right: 8px;
    bottom: 8px;
    display: flex;
    flex-direction: column;
    gap: 2px;
    background: rgba(30, 31, 34, 0.92);
    border: 1px solid var(--red);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    font-size: 12px;
    word-break: break-word;
  }
  .err b {
    color: var(--red);
  }
  .err button {
    align-self: flex-start;
    margin-top: 4px;
  }
  .scrub {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .time {
    font-size: 12px;
    color: var(--muted);
    white-space: nowrap;
    min-width: 120px;
    text-align: right;
  }
  .meta {
    gap: 4px 14px;
  }
</style>
