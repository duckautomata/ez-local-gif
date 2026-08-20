<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import { fetchStill, type StillRequest } from '../lib/api';
  import { ceilMs, fmtNum, fmtSeconds, fmtTimecode, frameAt, frameCount, frameTime } from '../lib/format';
  import {
    app,
    buildOutput,
    effectiveFPS,
    effectiveOps,
    opsApply,
    previewDuration,
    recipeOps,
    sourceFPS,
  } from '../lib/state.svelte';
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
  // The ops the recipe carries (none for Optimize — the preview then shows the source as-is).
  const ops = $derived(effectiveOps(app.ops, app.output));
  const duration = $derived(info ? previewDuration(info, ops) : 0);
  const cropMode = $derived(app.ui.cropOpen && !!info && opsApply(app.output));
  // The displayed / requested time: the scrubber position clamped to the clip
  // and ceiled to whole ms, which is what the still memo keys on and what
  // keeps a frame-start time inside its own frame (see ceilMs).
  const t = $derived(Math.min(ceilMs(Math.min(Math.max(0, app.ui.scrubT), Math.max(0, duration))), Math.max(0, duration)));

  // Frame grid of the plan: the effective output fps (fps op → Output.fps →
  // source), so stepping lands on the frames the render will contain.
  const fps = $derived(info ? effectiveFPS(ops, app.output, sourceFPS(info, ops)) : 0);
  const total = $derived(duration > 0 ? frameCount(duration, fps) : fps > 0 ? 1 : 0);
  const frame = $derived(fps > 0 ? frameAt(t, fps, total) : 0);
  const stepSec = $derived(fps > 0 ? 1 / fps : 0.01);
  const canStep = $derived(fps > 0 && total > 1);

  // Keep the scrubber inside the (trim/speed dependent) range.
  $effect(() => {
    if (app.ui.scrubT > duration) app.ui.scrubT = duration;
  });

  /** seekFrame moves the scrubber to 1-based frame f (clamped). */
  function seekFrame(f: number) {
    if (!canStep) return;
    const clamped = Math.min(Math.max(1, Math.round(f)), total);
    app.ui.scrubT = Math.min(frameTime(clamped, fps), duration);
  }
  function step(delta: number) {
    seekFrame(frame + delta);
  }

  // Arrow keys step frames while the preview (or the scrubber) is focused;
  // Shift steps 10, Home/End jump to the ends.
  function onStageKey(e: KeyboardEvent) {
    if (!canStep) return;
    if (e.altKey || e.ctrlKey || e.metaKey) return;
    const big = e.shiftKey ? 10 : 1;
    switch (e.key) {
      case 'ArrowLeft':
        step(-big);
        break;
      case 'ArrowRight':
        step(big);
        break;
      case 'Home':
        seekFrame(1);
        break;
      case 'End':
        seekFrame(total);
        break;
      default:
        return;
    }
    e.preventDefault();
  }

  // The still request. In crop mode the op stack stops before crop and no
  // output fitting is applied, so the frame is the full source in source
  // pixel coordinates (the overlay maps display px → source px).
  const req = $derived.by((): StillRequest | null => {
    const src = app.source;
    if (!src) return null;
    return {
      src: src.hash,
      ops: recipeOps(app.ops, app.output, { cropPreview: cropMode }),
      output: cropMode ? { format: app.output.format } : buildOutput(app.output),
      t,
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
  const position = $derived(canStep ? `${fmtTimecode(t)} · f ${frame} / ${total}` : fmtSeconds(t));
  const positionText = $derived(canStep ? `frame ${frame} of ${total}, ${fmtTimecode(t)}` : `${fmtSeconds(t)} of ${fmtSeconds(duration)}`);
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

  <!-- The stage is a plain container: slider is a children-presentational
       role, so it must not wrap interactive descendants (the error overlay's
       Retry button, the crop canvas). The frame slider lives on the position
       readout in the scrub row below instead. -->
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
    <div class="steps" role="group" aria-label="Step frames">
      <button type="button" class="sm" onclick={() => seekFrame(1)} disabled={!canStep || frame <= 1} title="First frame (Home)" aria-label="First frame">⏮</button>
      <button type="button" class="sm" onclick={() => step(-1)} disabled={!canStep || frame <= 1} title="Previous frame (←)" aria-label="Previous frame">◂</button>
      <button type="button" class="sm" onclick={() => step(1)} disabled={!canStep || frame >= total} title="Next frame (→)" aria-label="Next frame">▸</button>
      <button type="button" class="sm" onclick={() => seekFrame(total)} disabled={!canStep || frame >= total} title="Last frame (End)" aria-label="Last frame">⏭</button>
    </div>
    <input
      type="range"
      min="0"
      max={Math.max(0, duration)}
      step={canStep ? stepSec : 0.01}
      disabled={duration <= 0}
      bind:value={app.ui.scrubT}
      onkeydown={onStageKey}
      aria-label="Scrub time"
      aria-valuetext={positionText}
    />
    <!-- The position readout doubles as the frame stepper: a dedicated
         slider element with no interactive children (← → step one frame,
         Shift ×10, Home/End first/last — same keys as on the time scrubber). -->
    <span
      class="time mono"
      role="slider"
      tabindex="0"
      aria-label="Preview frame — arrow keys step one frame, Shift for ten, Home/End for the first/last"
      aria-valuemin={1}
      aria-valuemax={Math.max(1, total)}
      aria-valuenow={Math.max(1, frame)}
      aria-valuetext={positionText}
      aria-disabled={!canStep}
      onkeydown={onStageKey}
      title={canStep ? `${fmtSeconds(t)} of ${fmtSeconds(duration)} at ${fmtNum(fps)} fps` : ''}
    >
      {position}
      {#if canStep}<span class="muted"> · {fmtTimecode(duration)}</span>{:else if duration > 0}<span class="muted"> / {fmtSeconds(duration)}</span>{/if}
    </span>
  </div>
  <div class="row small muted meta">
    {#if natural.w > 0}<span>still {natural.w}×{natural.h}</span>{/if}
    {#if cropMode}<span class="warn">Crop mode: full frame shown, drag to set the rectangle</span>{/if}
    {#if !cropMode && info && !opsApply(app.output)}<span>Optimize: the source GIF as-is (ops are not applied)</span>{/if}
    {#if !cropMode && info && opsApply(app.output)}
      <span>
        preview shows the op stack and output canvas · <kbd>←</kbd> <kbd>→</kbd> on the scrubber or the frame readout step
        frames · <kbd>Ctrl</kbd>+<kbd>Enter</kbd> renders
      </span>
    {/if}
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
    flex-wrap: wrap;
    gap: 6px 10px;
  }
  .scrub input[type='range'] {
    flex: 1 1 140px;
    width: auto;
    min-width: 100px;
  }
  .steps {
    display: inline-flex;
    gap: 2px;
    flex: none;
  }
  .steps button {
    padding: 2px 7px;
    min-width: 28px;
    justify-content: center;
  }
  .time {
    flex: none;
    font-size: 12px;
    color: var(--text);
    white-space: nowrap;
    text-align: right;
    font-variant-numeric: tabular-nums;
    border-radius: 4px;
    outline: none;
  }
  .time:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  .meta {
    gap: 4px 14px;
  }
</style>
