<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { fetchProxy, fetchStill, hasMatteOp, MATTE_EDGE_NONE, matteModelOf, matteParamsOf, type Op, type ProxyRequest } from '../lib/api';
  import { caps } from '../lib/capabilities.svelte';
  import { displayToPixel, readPixel, rgbToHex } from '../lib/eyedropper';
  import { clamp, fmtNum, fmtSeconds, fmtTimecode, frameStart, stillTime } from '../lib/format';
  import { computeFromPending, edgeModelFor, effectiveDevice, isIdleState, isTracker, matteMemoKey, mattePendingPill, NO_COMPUTE } from '../lib/matte';
  import { matte, noteMatteMemo, notePending, pollWhilePending, registerMatteCompute, setMatteCompute } from '../lib/matte.svelte';
  import { ProxyPlayer, type ProxyView } from '../lib/proxy';
  import { keyframes } from '../lib/prompts';
  import {
    aiActive,
    app,
    applyPickedColor,
    buildOutput,
    effectiveOps,
    forwardFrame,
    hasOverlays,
    isGuided,
    matteClipKey,
    opsApply,
    planFPS,
    planFrames,
    previewDuration,
    previewOutput,
    recipeOps,
    recipeSources,
    stillRequest,
  } from '../lib/state.svelte';
  import { registerPlayToggle } from '../lib/shortcuts';
  import { StillScheduler, stillMaxW, type StillView } from '../lib/still';
  import BackdropToggle from './BackdropToggle.svelte';
  import CropOverlay from './CropOverlay.svelte';
  import OverlayLayer from './OverlayLayer.svelte';
  import PromptOverlay from './PromptOverlay.svelte';

  /** Proxy (Play) limits: DESIGN §7 — first 10 s, ≤ 360 px wide. */
  const PROXY_MAXW = 360;
  const PROXY_SECONDS = 10;

  // In Fit the still is at most 480 px wide, 720 on wide screens; overlays
  // and a fixed zoom request the output canvas unscaled (lib/still.stillMaxW).
  const wideQuery = window.matchMedia('(min-width: 1500px)');
  let wide = $state(wideQuery.matches);
  $effect(() => {
    const onChange = (e: MediaQueryListEvent) => (wide = e.matches);
    wideQuery.addEventListener('change', onChange);
    return () => wideQuery.removeEventListener('change', onChange);
  });

  // Zoom: Fit scales the still into the stage; 1× / 2× / 4× show the OUTPUT
  // canvas at that many CSS pixels per output pixel (zoomStyle), which is
  // only true when the still is requested unscaled — see stillMaxW.
  type Zoom = 'fit' | 1 | 2 | 4;
  const zooms: readonly Zoom[] = ['fit', 1, 2, 4];
  let zoom = $state<Zoom>('fit');
  const zoomed = $derived(zoom !== 'fit');

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
  // Prompt mode (Phase 5c): the guided model's Select subject panel is open
  // — the still is the SOURCE frame (crop mode's mechanism) unkeyed, with
  // the prompt canvas and the live mask over it. The eyedropper wins while
  // it is armed (a Keep colour pick needs the same unkeyed still).
  const promptMode = $derived(app.ui.promptOpen && withOps && !cropMode && aiActive(ops) && isGuided(ops.background));
  // Overlay placement: drag boxes over the still at output-canvas size.
  const overlayMode = $derived(withOps && !cropMode && !promptMode && hasOverlays(ops));

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

  // The still request (lib/state.stillRequest — pure, tested in
  // Preview.test.ts). In crop mode the op stack stops before crop and no
  // output fitting is applied, so the frame is the full source in source
  // pixel coordinates (the overlay maps display px → source px) — but the
  // output keeps its fps (cropPreviewOutput): geometry is what crop mode
  // drops, never the rate, or its plan resolves to another frame grid than
  // the normal still's. While the eyedropper is armed the keying ops (and
  // their morph) are left out. With overlays on the stage, or at a fixed
  // zoom, the still is unscaled (lib/still.stillMaxW: the zoom multiplies
  // the still's natural size, so 4× must magnify real output pixels, not a
  // 480-px preview). The output carries only what the server renders (and
  // memoises) the preview from — format, size, fit, fps (previewOutput) —
  // so a quality / lossy / colours / dither / matte / loop / fit-budget /
  // preset / target change never re-requests a still.
  //
  // Crop mode's truncated stack also drops reverse and bounce, so its time
  // must be on the forward un-reversed timeline: the scrubber frame is folded
  // back first (forwardFrame, WEB-5) — otherwise the mirrored half of a
  // bounced clip clamps to the clip end and the crop rectangle is drawn on
  // the wrong frame.
  /** the scrubber slot on the forward grid: what crop / prompt mode's truncated stack must ask for, and the prompted frame's index */
  const fwdFrame = $derived(info ? forwardFrame(info, app.ops, app.output, i) : 0);
  const stillT = $derived((cropMode || promptMode) && info ? stillTime(fwdFrame, fps) : t);
  const req = $derived(
    stillRequest(app.source, app.ops, app.output, { cropMode, picking, promptMode, t: stillT, maxW: stillMaxW({ overlay: overlayMode, zoomed, wide }) }),
  );

  // The still on screen (url), the in-flight flag and the last error live in a
  // $state object that StillScheduler mutates (debounce, abort of superseded
  // requests, object-URL lifecycle — see lib/still.ts).
  const view = $state<StillView>({ url: null, loading: false, error: '', pending: null });
  const still = new StillScheduler(view, {
    fetch: fetchStill,
    createURL: (b) => URL.createObjectURL(b),
    revokeURL: (u) => URL.revokeObjectURL(u),
    // a 202 carries the live /api/matte object: install it for the Background card
    onPending: notePending,
  });
  let natural = $state({ w: 0, h: 0 });
  let imgEl = $state<HTMLImageElement | null>(null);

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
  const proxy = $state<ProxyView>({ url: null, playing: false, loading: false, stale: false, error: '', pending: null });
  const player = new ProxyPlayer(proxy, {
    fetch: fetchProxy,
    createURL: (b) => URL.createObjectURL(b),
    revokeURL: (u) => URL.revokeObjectURL(u),
    onPending: notePending,
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
  // Crop / eyedropper / prompt modes need the still on the stage.
  $effect(() => {
    if (cropMode || picking || promptMode) untrack(() => player.stop());
  });
  // An older server has no POST /api/proxy (features.proxy off): Play stays
  // visible but disabled, its tooltip saying why.
  const proxyOn = $derived(caps.features.proxy);
  const canPlay = $derived(proxyOn && !!info && !cropMode && !picking && !promptMode && (total > 1 || hasOverlays(ops)));
  const playing = $derived(proxy.playing && !!proxy.url);
  /** Play is on its way: a fetch in flight, or its matte pending (Stop cancels either) */
  const playBusy = $derived(proxy.loading || proxy.pending !== null);

  // ---- Phase 5b / 5c: the AI matte pending pill. A still / proxy whose
  // answer was 202 keeps the picture on the stage and shows what the
  // sidecar is doing (lib/matte.mattePendingPill: "AI matte 24/45 · GPU",
  // "loading model… (12 s)", "downloading weights 43 %"); an IDLE matte
  // (nothing on disk, nothing running — a preview never starts a pass by
  // itself) reads "AI matte not computed" with a Compute button, which
  // re-requests the still (or the pending proxy) with `eager` and starts
  // the pass — the Background card's Compute matte button does the same
  // through registerMatteCompute; Render starts it anyway. The schedulers
  // re-request by themselves (still.ts / proxy.ts); while anything is
  // pending /api/matte is polled too, so the Background card's states
  // follow.
  const pendingView = $derived(proxy.pending ?? (playing ? null : view.pending));
  let now = $state(Date.now());
  $effect(() => {
    if (!pendingView) return;
    now = Date.now();
    const t = setInterval(() => (now = Date.now()), 1000);
    return () => clearInterval(t);
  });
  // A boolean, not the pending objects: the schedulers replace them on
  // every 202, and an effect reading them would re-hold per answer
  // (lib/matte.svelte pollWhilePending).
  pollWhilePending(() => view.pending !== null || proxy.pending !== null);
  const pillText = $derived(pendingView ? mattePendingPill(pendingView, now) : '');
  const canComputeNow = $derived(!!pendingView && isIdleState(pendingView.state) && !playing);
  /**
   * the Compute button: in prompt mode the panel closes and the keyed still
   * that follows starts the pass (still.computeNext); otherwise the pending
   * proxy's re-request when Play is waiting, else the still's
   */
  function computeNow() {
    if (promptMode) {
      still.computeNext();
      app.ui.promptOpen = false;
      return;
    }
    if (proxy.pending) player.computeNow();
    else still.computeNow();
  }
  // The compute state the Background card's Compute matte button shows
  // (lib/matte.svelte): 'none' without a matte op on the stage, 'idle' /
  // 'running' from the 202, 'computed' once the still of the CURRENT state
  // is on screen (displayedKey = the request's key), 'unknown' meanwhile.
  const stageMatte = $derived(!!req && hasMatteOp(req.ops));
  $effect(() => {
    const r = req;
    const p = proxy.pending ?? view.pending;
    const loading = view.loading;
    const url = view.url;
    if (!stageMatte) {
      setMatteCompute(NO_COMPUTE);
      return;
    }
    const shown = url !== null && !loading && p === null && untrack(() => still.displayedKey) === StillScheduler.key(r);
    const c = computeFromPending(p, shown);
    setMatteCompute(c);
    // Phase 5d: what the answer said about the server's memos (lib/
    // matte.svelte matteMemo — "Use this frame's matte" reads the edge
    // model's entry); the device and the clip key are read untracked so
    // the effect keeps the still's dependencies alone.
    if (r && (c.state === 'computed' || c.state === 'idle')) {
      const state = c.state;
      untrack(() => noteStageMemo(r.ops, state));
    }
  });
  /**
   * noteStageMemo records the stage's matte memo as computed / idle, and
   * — for a guided op that computed — its edge model's memo as computed
   * too: the gated matte is derived from the edge model's pass over the
   * same clip, so that pass ran (or was already on disk).
   */
  function noteStageMemo(ops: Op[], state: 'computed' | 'idle') {
    const clip = matteClipKey(app.source, app.ops, app.output);
    const device = effectiveDevice(matte.status);
    const model = matteModelOf(ops);
    noteMatteMemo(matteMemoKey(clip, model, device), state);
    if (state !== 'computed' || !isTracker(matte.status, model)) return;
    const edge = edgeModelFor(matteParamsOf(ops)?.edge, matte.status, device);
    if (edge !== MATTE_EDGE_NONE) noteMatteMemo(matteMemoKey(clip, edge, device), 'computed');
  }
  onMount(() => {
    registerMatteCompute(computeNow);
    return () => {
      registerMatteCompute(null);
      setMatteCompute(NO_COMPUTE);
    };
  });
  // The device preference changed (the card's Run on select, or the
  // sidecar's answer): the still on screen was made for the old device, so
  // the current state is asked again — a memo hit or an idle 202 says
  // where the new device stands. The first known device never retries.
  let lastDevice = '';
  $effect(() => {
    const d = effectiveDevice(matte.status);
    untrack(() => {
      if (d === lastDevice) return;
      const known = lastDevice !== '';
      lastDevice = d;
      if (known && d !== '' && stageMatte) still.retry();
    });
  });
  /** the keyframe strip's count for the prompt-mode meta line */
  const promptedFrames = $derived(promptMode ? keyframes(app.ops.background.ai.prompts).length : 0);
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
      if (proxy.playing || playBusy) player.stop();
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
    // The pick lands in the Colour row that armed the eyedropper
    // (app.ui.pickRow — state.applyPickedColor), enables the card and disarms.
    applyPickedColor(rgbToHex(px.r, px.g, px.b));
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
  // ratio anyway (overlay.displayScale). natural.w is the output canvas
  // width while zoomed (the still is requested unscaled), so the factor is
  // CSS pixels per output pixel; the proxy (≤ PROXY_MAXW wide, a deliberately
  // low-res animation) is simply enlarged by the same factor.
  const zoomStyle = $derived(zoom === 'fit' || natural.w === 0 ? '' : `width:${natural.w * zoom}px;max-width:none;max-height:none;`);
  const position = $derived(canStep ? `${fmtTimecode(shown)} · f ${i + 1} / ${total}` : fmtSeconds(shown));
  // The readout keeps ONE width while scrubbing. Its text gains a character
  // whenever the frame number gains a digit (frame 10, 100, …), and in the
  // wrapping scrub row that is enough to push it onto a line of its own when
  // the column is about as wide as the row (a 1080 px portrait screen): the
  // slider then jumps to the full row width, and back on the way down. The
  // readout is set in the monospace face, so the length of its widest text —
  // the last frame's — reserves exactly the room it will ever need.
  const positionCh = $derived(canStep ? `${fmtTimecode(duration)} · f ${total} / ${total} · ${fmtTimecode(duration)}`.length : 0);
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
  <div class="stage backdrop-{app.ui.backdrop}" class:cropping={cropMode || promptMode} class:picking>
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
        {#if promptMode && !picking && imgEl && info}
          <PromptOverlay img={imgEl} srcW={info.width} srcH={info.height} frame={fwdFrame} />
        {/if}
        {#if overlayMode && !picking && imgEl && natural.w > 0}
          <OverlayLayer img={imgEl} canvasW={natural.w} canvasH={natural.h} t={shown} />
        {/if}
      </div>
    {:else if !view.error}
      <div class="placeholder muted">{view.loading ? 'Rendering preview…' : 'No preview yet'}</div>
    {/if}
    {#if (view.loading && view.url && !playing && !view.pending) || (proxy.loading && !proxy.pending)}<div class="spinner" aria-label="Loading"></div>{/if}
    {#if pendingView}
      <div class="pill" role="status">
        <span>{pillText}</span>
        {#if canComputeNow}
          <button type="button" class="sm" onclick={computeNow} title="Start the AI matte pass for this clip now (Render starts it anyway)">
            {pendingView.state === 'deferred' ? 'Compute now' : 'Compute'}
          </button>
        {/if}
      </div>
    {/if}
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
    {#if playing || playBusy}
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
      style:min-width={positionCh > 0 ? `${positionCh}ch` : undefined}
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
          Eyedropper: click {app.ops.background.colors.length > 1 ? `colour ${app.ui.pickRow + 1}` : 'the colour'} to remove (the still is shown unkeyed){#if hover}
            &nbsp;· <span class="swatch" style:background={'#' + hover} aria-hidden="true"></span> #{hover}{/if} · <kbd>Esc</kbd> cancels
        </span>
      {:else if cropMode}
        <span class="warn">Crop mode: full frame shown, drag to set the rectangle</span>
      {:else if promptMode}
        <span class="warn">
          Select subject: drag a box around it, click to keep (+), <kbd>Shift</kbd>-click or right-click to remove (−), click a marker to
          delete it · the source frame is shown unkeyed with the tracker's mask in green · {promptedFrames} prompted {promptedFrames === 1
            ? 'frame'
            : 'frames'}
        </span>
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
  .stale,
  .pill {
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
  /* the AI matte pending pill (Phase 5b) sits where the stale pill does; never both at once (a pending proxy is not playing) */
  .pill {
    border-color: var(--blue);
    color: var(--blue);
    max-width: calc(100% - 16px);
  }
  .pill button {
    margin-left: 4px;
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
