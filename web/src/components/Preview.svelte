<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { fetchProxy, fetchStill, type ProxyRequest, type StillRequest } from '../lib/api';
  import { caps } from '../lib/capabilities.svelte';
  import { displayToPixel, readPixel, rgbToHex } from '../lib/eyedropper';
  import { clamp, fmtNum, fmtSeconds, fmtTimecode, frameStart, stillTime } from '../lib/format';
  import { ProxyPlayer, type ProxyView } from '../lib/proxy';
  import {
    app,
    buildOutput,
    effectiveOps,
    forwardFrame,
    hasOverlays,
    opsApply,
    planFPS,
    planFrames,
    previewDuration,
    previewOutput,
    recipeOps,
    recipeSources,
  } from '../lib/state.svelte';
  import { registerPlayToggle } from '../lib/shortcuts';
  import { StillScheduler, type StillView } from '../lib/still';
  import BackdropToggle from './BackdropToggle.svelte';
  import CropOverlay from './CropOverlay.svelte';
  import OverlayLayer from './OverlayLayer.svelte';

  /**
   * With overlays on the stage the still is requested unscaled (graph's
   * MaxDim as the width cap, so the server's min(iw, maxW) never shrinks it):
   * its natural size is then exactly the output canvas the overlay
   * coordinates live on, and the drag boxes map 1:1.
   */
  const CANVAS_STILL_MAXW = 8192;
  /** Proxy (Play) limits: DESIGN §7 — first 10 s, ≤ 360 px wide. */
  const PROXY_MAXW = 360;
  const PROXY_SECONDS = 10;

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
  const withOps = $derived(!!info && opsApply(app.output));
  // Crop mode: the Crop card is open and the manual rectangle applies (with
  // auto-crop on, the still shows the detected result instead).
  const cropMode = $derived(app.ui.cropOpen && withOps && !app.ops.autocrop.enabled);
  // Eyedropper: the Background card armed it; the still is requested unkeyed.
  const picking = $derived(app.ui.pickColor && withOps && !cropMode);
  // Overlay placement: drag boxes over the still at output-canvas size.
  const overlayMode = $derived(withOps && !cropMode && hasOverlays(ops));

  // The scrubber is a frame index on the plan's grid (planFPS / planFrames
  // mirror graph.Plan.FPS / Plan.Frames): i ∈ [0, total − 1], one notch per
  // frame, so it reaches both ends exactly and can never stop on a phantom
  // extra step. The still is requested at the middle of the frame
  // (stillTime) — the server maps t → floor(t × fps), which is robust there —
  // and the readout shows the frame's start time.
  const fps = $derived(planFPS(info, ops, app.output));
  const total = $derived(planFrames(info, ops, app.output));
  const last = $derived(Math.max(0, total - 1));
  const canStep = $derived(fps > 0 && total > 1);
  const i = $derived(clamp(Math.round(app.ui.scrubFrame) || 0, 0, last));
  const shown = $derived(frameStart(i, fps));
  const t = $derived(stillTime(i, fps));

  // Keep the scrubber inside the (trim / speed / fps dependent) frame range.
  $effect(() => {
    if (app.ui.scrubFrame > last) app.ui.scrubFrame = last;
  });

  /** seekFrame moves the scrubber to 0-based frame idx (clamped). */
  function seekFrame(idx: number) {
    if (!canStep) return;
    app.ui.scrubFrame = clamp(Math.round(idx), 0, last);
  }
  function step(delta: number) {
    seekFrame(i + delta);
  }

  // Arrow keys step frames while the scrubber (or the readout) is focused;
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
        seekFrame(0);
        break;
      case 'End':
        seekFrame(last);
        break;
      default:
        return;
    }
    e.preventDefault();
  }

  // The still request. In crop mode the op stack stops before crop and no
  // output fitting is applied, so the frame is the full source in source
  // pixel coordinates (the overlay maps display px → source px). While the
  // eyedropper is armed the keying op is left out. With overlays the still
  // is unscaled (see CANVAS_STILL_MAXW). The output carries only what the
  // server renders (and memoises) the preview from — format, size, fit, fps
  // (previewOutput) — so a quality / lossy / colours / dither / matte / loop
  // / fit-budget / preset / target change never re-requests a still.
  //
  // Crop mode's truncated stack also drops reverse and bounce, so its time
  // must be on the forward un-reversed timeline: the scrubber frame is folded
  // back first (forwardFrame, WEB-5) — otherwise the mirrored half of a
  // bounced clip clamps to the clip end and the crop rectangle is drawn on
  // the wrong frame.
  const stillT = $derived(cropMode && info ? stillTime(forwardFrame(info, app.ops, app.output, i), fps) : t);
  const req = $derived.by((): StillRequest | null => {
    const src = app.source;
    if (!src) return null;
    return {
      src: src.hash,
      sources: cropMode ? [src.hash] : recipeSources(src.hash, app.ops, app.output),
      ops: recipeOps(app.ops, app.output, { cropPreview: cropMode, keyPreview: picking }),
      output: cropMode ? { format: app.output.format } : previewOutput(buildOutput(app.output)),
      t: stillT,
      maxW: overlayMode ? CANVAS_STILL_MAXW : maxW,
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

  // ---- Play: the animated proxy (POST /api/proxy), fetched on demand only.
  const proxy = $state<ProxyView>({ url: null, playing: false, loading: false, stale: false, error: '' });
  const player = new ProxyPlayer(proxy, {
    fetch: fetchProxy,
    createURL: (b) => URL.createObjectURL(b),
    revokeURL: (u) => URL.revokeObjectURL(u),
  });
  // Like the still, the proxy is keyed on the geometry subset of the output
  // (previewOutput): a playing proxy is "changed — Play again" only when the
  // sources, the ops or that subset change, never for the encoder knobs the
  // server does not render the proxy from.
  const proxyReq = $derived.by((): ProxyRequest | null => {
    const src = app.source;
    if (!src) return null;
    return {
      sources: recipeSources(src.hash, app.ops, app.output),
      ops: recipeOps(app.ops, app.output),
      output: previewOutput(buildOutput(app.output)),
      maxW: PROXY_MAXW,
      maxSeconds: PROXY_SECONDS,
    };
  });
  $effect(() => {
    const r = proxyReq;
    untrack(() => player.update(r));
  });
  // Crop / eyedropper modes need the still on the stage.
  $effect(() => {
    if (cropMode || picking) untrack(() => player.stop());
  });
  // An older server has no POST /api/proxy (features.proxy off): Play stays
  // visible but disabled, its tooltip saying why.
  const proxyOn = $derived(caps.features.proxy);
  const canPlay = $derived(proxyOn && !!info && !cropMode && !picking && (total > 1 || hasOverlays(ops)));
  const playing = $derived(proxy.playing && !!proxy.url);
  // While the proxy plays the still <img> is unmounted: pause the still
  // scheduler so scrubbing / editing renders no stills (full-resolution ones
  // in overlay mode) that nothing shows, and release the URLs it parked for
  // the next image load; Stop fetches the current state once (still.ts).
  $effect(() => {
    const paused = playing;
    untrack(() => still.setPaused(paused));
  });
  const playTitle = $derived(
    proxyOn
      ? `Animated preview: the first ${PROXY_SECONDS} s at ≤ ${PROXY_MAXW} px (rendered on demand)`
      : 'Animated preview is not available: this server has no /api/proxy (update ezlg)',
  );

  // Space (the global shortcut in App.svelte) toggles Play/Stop while the
  // preview is mounted; with no preview (batch mode, no source) Space is
  // inert. The closure reads the current derived state at press time.
  onMount(() => {
    registerPlayToggle(() => {
      if (proxy.playing || proxy.loading) player.stop();
      else if (canPlay) void player.play();
    });
    return () => registerPlayToggle(null);
  });

  onDestroy(() => {
    still.dispose();
    player.dispose();
  });

  // ---- Eyedropper: click the still, read its pixel.
  let hover = $state<string>('');
  function pixelAt(e: MouseEvent): { r: number; g: number; b: number; a: number } | null {
    const el = imgEl;
    if (!el || natural.w === 0) return null;
    const p = displayToPixel(e.clientX, e.clientY, el.getBoundingClientRect(), natural.w, natural.h);
    return p ? readPixel(el, p.x, p.y) : null;
  }
  function onPickMove(e: MouseEvent) {
    if (!picking) return;
    const px = pixelAt(e);
    hover = px ? rgbToHex(px.r, px.g, px.b) : '';
  }
  function onPickClick(e: MouseEvent) {
    if (!picking) return;
    e.preventDefault();
    // A keyboard "click" has no pointer position: pixelAt then finds nothing
    // (outside the image) and the hex field in the card is the alternative.
    const px = pixelAt(e);
    if (!px) return;
    app.ops.background.pickColor = rgbToHex(px.r, px.g, px.b);
    app.ops.background.enabled = true;
    app.ui.pickColor = false;
    hover = '';
  }
  function onWindowKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && app.ui.pickColor) app.ui.pickColor = false;
  }

  // A fixed zoom sets the width explicitly and lifts BOTH stylesheet clamps:
  // with only max-width lifted, the img's max-height would still cap the
  // height of a tall still and squash it (an explicit width plus max-height
  // does not keep the aspect the way max-width + max-height alone do); the
  // stage scrolls instead. The overlay drag boxes map each axis by its own
  // ratio anyway (overlay.displayScale).
  const zoomStyle = $derived(zoom === 'fit' || natural.w === 0 ? '' : `width:${natural.w * zoom}px;max-width:none;max-height:none;`);
  const position = $derived(canStep ? `${fmtTimecode(shown)} · f ${i + 1} / ${total}` : fmtSeconds(shown));
  const positionText = $derived(canStep ? `frame ${i + 1} of ${total}, ${fmtTimecode(shown)}` : `${fmtSeconds(shown)} of ${fmtSeconds(duration)}`);
</script>

<svelte:window onkeydown={onWindowKey} />

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
  <div class="stage backdrop-{app.ui.backdrop}" class:cropping={cropMode} class:picking>
    {#if playing}
      <div class="img-wrap">
        <img src={proxy.url} alt="Animated preview (first {PROXY_SECONDS} s, low resolution)" class="proxy" style={zoomStyle} draggable="false" onerror={() => player.imageFailed()} />
      </div>
      {#if proxy.stale}
        <div class="stale">
          <span>changed — </span>
          <button type="button" class="sm" onclick={() => void player.play()} disabled={proxy.loading}>Play again</button>
        </div>
      {/if}
    {:else if view.url}
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
        {#if picking}
          <!-- a transparent button over the still: clicks read the pixel under the pointer -->
          <button
            type="button"
            class="pick-layer"
            aria-label="Pick the colour to remove from the preview (or type a hex value in the Background card)"
            title="Click the colour to remove"
            onclick={onPickClick}
            onmousemove={onPickMove}
            onmouseleave={() => (hover = '')}
          ></button>
        {/if}
        {#if cropMode && imgEl && info}
          <CropOverlay img={imgEl} srcW={info.width} srcH={info.height} />
        {/if}
        {#if overlayMode && !picking && imgEl && natural.w > 0}
          <OverlayLayer img={imgEl} canvasW={natural.w} canvasH={natural.h} t={shown} />
        {/if}
      </div>
    {:else if !view.error}
      <div class="placeholder muted">{view.loading ? 'Rendering preview…' : 'No preview yet'}</div>
    {/if}
    {#if (view.loading && view.url && !playing) || proxy.loading}<div class="spinner" aria-label="Loading"></div>{/if}
    {#if view.error && !playing}
      <div class="err">
        <b>Preview failed</b>
        <span>{view.error}</span>
        <button type="button" class="sm" onclick={retry}>Retry</button>
      </div>
    {/if}
    {#if proxy.error}
      <div class="err">
        <b>Play failed</b>
        <span>{proxy.error}</span>
        <button type="button" class="sm" onclick={() => void player.play()}>Retry</button>
        <button type="button" class="sm ghost" onclick={() => player.stop()}>Back to the still</button>
      </div>
    {/if}
  </div>

  <div class="scrub">
    {#if playing || proxy.loading}
      <button type="button" class="sm play" onclick={() => player.stop()} title="Back to the still" aria-label="Stop the animated preview">■ Stop</button>
    {:else}
      <button type="button" class="sm play" onclick={() => void player.play()} disabled={!canPlay} title={playTitle} aria-label="Play an animated preview">▶ Play</button>
    {/if}
    <div class="steps" role="group" aria-label="Step frames">
      <button type="button" class="sm" onclick={() => seekFrame(0)} disabled={!canStep || i <= 0} title="First frame (Home)" aria-label="First frame">⏮</button>
      <button type="button" class="sm" onclick={() => step(-1)} disabled={!canStep || i <= 0} title="Previous frame (←)" aria-label="Previous frame">◂</button>
      <button type="button" class="sm" onclick={() => step(1)} disabled={!canStep || i >= last} title="Next frame (→)" aria-label="Next frame">▸</button>
      <button type="button" class="sm" onclick={() => seekFrame(last)} disabled={!canStep || i >= last} title="Last frame (End)" aria-label="Last frame">⏭</button>
    </div>
    <!-- One notch per plan frame: min 0, max total − 1, step 1. -->
    <input
      type="range"
      min="0"
      max={last}
      step="1"
      disabled={!canStep}
      bind:value={app.ui.scrubFrame}
      onkeydown={onStageKey}
      aria-label="Scrub frames"
      aria-valuetext={positionText}
    />
    <!-- The position readout doubles as the frame stepper: a dedicated
         slider element with no interactive children (← → step one frame,
         Shift ×10, Home/End first/last — same keys as on the range input). -->
    <span
      class="time mono"
      role="slider"
      tabindex="0"
      aria-label="Preview frame — arrow keys step one frame, Shift for ten, Home/End for the first/last"
      aria-valuemin={1}
      aria-valuemax={Math.max(1, total)}
      aria-valuenow={i + 1}
      aria-valuetext={positionText}
      aria-disabled={!canStep}
      onkeydown={onStageKey}
      title={canStep ? `${fmtSeconds(shown)} of ${fmtSeconds(duration)} at ${fmtNum(fps)} fps` : ''}
    >
      {position}
      {#if canStep}<span class="muted"> · {fmtTimecode(duration)}</span>{:else if duration > 0}<span class="muted"> / {fmtSeconds(duration)}</span>{/if}
    </span>
  </div>
  <div class="row small muted meta">
    {#if playing}
      <span class="warn">Playing the proxy: first {PROXY_SECONDS} s, ≤ {PROXY_MAXW} px, ≤ 15 fps, lossy — Stop returns to the still</span>
    {:else}
      {#if natural.w > 0}<span>still {natural.w}×{natural.h}</span>{/if}
      {#if picking}
        <span class="warn">
          Eyedropper: click the colour to remove (the still is shown unkeyed){#if hover}
            &nbsp;· <span class="swatch" style:background={'#' + hover} aria-hidden="true"></span> #{hover}{/if} · <kbd>Esc</kbd> cancels
        </span>
      {:else if cropMode}
        <span class="warn">Crop mode: full frame shown, drag to set the rectangle</span>
      {:else if info && !withOps}
        <span>Optimize: the source GIF as-is (ops are not applied)</span>
      {:else if overlayMode}
        <span>
          overlays: drag the boxes to place them (arrow keys nudge, Shift ×10); the still is the real composite at the output
          size · <kbd>←</kbd> <kbd>→</kbd> step frames · <kbd>Ctrl</kbd>+<kbd>Enter</kbd> renders
        </span>
      {:else if info}
        <span>
          preview shows the op stack and output canvas · one notch = one output frame · <kbd>←</kbd> <kbd>→</kbd> on the scrubber or
          the frame readout step frames · <kbd>Ctrl</kbd>+<kbd>Enter</kbd> renders
        </span>
      {/if}
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
  .pick-layer {
    position: absolute;
    inset: 0;
    padding: 0;
    border: 0;
    border-radius: 0;
    background: transparent;
    cursor: crosshair;
  }
  .pick-layer:hover:not(:disabled) {
    background: transparent;
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
  .stale {
    position: absolute;
    top: 8px;
    left: 8px;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    background: rgba(30, 31, 34, 0.92);
    border: 1px solid var(--amber);
    color: var(--amber);
    border-radius: 999px;
    padding: 2px 6px 2px 10px;
    font-size: 12px;
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
  .play {
    flex: none;
    min-width: 64px;
    justify-content: center;
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
  .swatch {
    display: inline-block;
    width: 12px;
    height: 12px;
    border-radius: 3px;
    border: 1px solid var(--border-strong);
    vertical-align: -2px;
  }
</style>
