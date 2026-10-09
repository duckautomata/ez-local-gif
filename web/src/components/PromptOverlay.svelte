<script lang="ts">
  // The guided model's prompt canvas (Phase 5c Part B), drawn over the
  // SOURCE-frame still while the Select subject panel is open: drag on
  // empty space draws the frame's box (one per prompted frame; drag inside
  // it to move, drag a corner / edge handle to resize — the crop
  // rectangle's geometry, lib/croprect), a click adds a + point, a
  // Shift-click or right-click a − point, a click on a marker removes it.
  // After every change the frame's mask (POST /api/matte/prompt, lib/
  // promptmask) is painted at 50 % green. Coordinates are kept normalised
  // to the source frame (lib/prompts); the still shows the full frame, so
  // display pixels map linearly: src = display × (srcW / clientWidth).
  //
  // Phase 5d: a frame that is the set's MASK prompt ("Use this frame's
  // matte") shows the edge model's matte of it the same way (the server
  // sends it from its memo; a 202 idle means that matte is not computed
  // for this clip, and the status says so), and the loose-box guard
  // (lib/maskguard) measures every BOX prompt's mask as it is decoded for
  // the tint: one that looks like the background sets app.ui.promptWarning,
  // which the Background card shows under the panel.
  import { onDestroy, untrack } from 'svelte';
  import { fetchPromptMask, MATTE_EDGE_NONE } from '../lib/api';
  import { cursorFor, drawRect, hitHandle, resizeRect, type Handle, type Rect } from '../lib/croprect';
  import { looseBoxWarning, maskStats, type MaskStats } from '../lib/maskguard';
  import { matte, noteMatteMemo, notePending, pollWhilePending } from '../lib/matte.svelte';
  import { edgeModelFor, effectiveDevice, isIdleState, maskPromptIdleText, matteMemoKey, matteModel, mattePendingPill, modelLabel } from '../lib/matte';
  import { PromptMaskScheduler, type PromptMaskView } from '../lib/promptmask';
  import { addFramePoint, boxRect, canPromptFrame, framePrompts, hasMask, hitPoint, normBox, normPoint, removeFramePoint, setFrameBox, type PromptPoint } from '../lib/prompts';
  import { app, matteClipKey, promptMaskRequest, setMattePrompts } from '../lib/state.svelte';

  interface Props {
    img: HTMLImageElement;
    srcW: number;
    srcH: number;
    /** the prompted frame: the scrubber slot on the forward grid */
    frame: number;
  }
  let { img, srcW, srcH, frame }: Props = $props();

  let canvas = $state<HTMLCanvasElement | null>(null);
  let cw = $state(0);
  let ch = $state(0);

  type Drag =
    | { mode: 'draw'; ax: number; ay: number; sx: number; sy: number; moved: boolean; shift: boolean; button: number }
    | { mode: 'move'; ox: number; oy: number; w: number; h: number; sx: number; sy: number; moved: boolean }
    | { mode: 'resize'; handle: Handle; start: Rect };
  let drag: Drag | null = null;
  let hover = $state<'move' | 'point' | Handle | null>(null);

  const prompts = $derived(app.ops.background.ai.prompts);
  const current = $derived(framePrompts(prompts, frame));
  /** the frame's box as a source-pixel rectangle (the crop geometry works in source px) */
  const rect = $derived<Rect | null>(current?.box ? boxRect(current.box, srcW, srcH) : null);
  const points = $derived<readonly PromptPoint[]>(current?.points ?? []);
  const atCap = $derived(!canPromptFrame(prompts, frame));
  // Handle / marker hit tolerance: ~7 display px, converted into source px
  // (handles) or 0..1 units (markers) so they stay grabbable at any zoom.
  const HANDLE_TOL = 7;
  const MARKER_TOL = 9;
  const tolX = $derived(cw > 0 ? (HANDLE_TOL * srcW) / cw : 0);
  const tolY = $derived(ch > 0 ? (HANDLE_TOL * srcH) / ch : 0);
  const cursor = $derived.by(() => {
    const h = drag ? (drag.mode === 'resize' ? drag.handle : drag.mode === 'move' ? 'move' : null) : hover;
    if (h === null) return 'crosshair';
    if (h === 'move') return 'move';
    if (h === 'point') return 'pointer';
    return cursorFor(h);
  });

  // ---- the live mask: one request per (recipe, frame, that frame's prompts)
  const mask = $state<PromptMaskView>({ url: null, frame: -1, loading: false, error: '', pending: null });
  /** the reason the last 202 carried ('' = none): a mask prompt's idle answer may say why (the 202's own `pendingReason`, never the status's reason — that one is the feature-off reason the card reads) */
  let pendingReason = $state('');
  const masks = new PromptMaskScheduler(mask, {
    fetch: fetchPromptMask,
    createURL: (b) => URL.createObjectURL(b),
    revokeURL: (u) => URL.revokeObjectURL(u),
    onPending: (p) => {
      notePending(p);
      pendingReason = p.reason.trim();
    },
  });
  const maskReq = $derived(promptMaskRequest(app.source, app.ops, app.output, frame));
  $effect(() => {
    const r = maskReq;
    untrack(() => masks.request(r));
  });
  pollWhilePending(() => mask.pending !== null);
  onDestroy(() => masks.dispose());
  /** the mask on screen answers the CURRENT request (not a previous frame's or box's, kept while the next loads) */
  const shownCurrent = $derived(mask.url !== null && !mask.loading && mask.pending === null && !mask.error && mask.frame === frame && untrack(() => masks.displayedKey) === PromptMaskScheduler.key(maskReq));

  // ---- Phase 5d: the mask prompt ("Use this frame's matte") and the loose-box guard
  /** the frame is the set's mask prompt: the mask shown is the edge model's matte of it */
  const maskPrompt = $derived(hasMask(current));
  const device = $derived(effectiveDevice(matte.status));
  const edgeId = $derived(edgeModelFor(app.ops.background.ai.edge, matte.status, device));
  const edgeLabel = $derived(modelLabel(edgeId, matteModel(matte.status, edgeId)));
  const edgeMemoKey = $derived(edgeId === MATTE_EDGE_NONE ? '' : matteMemoKey(matteClipKey(app.source, app.ops, app.output), edgeId, device));
  // What a mask prompt's answer says about the edge model's memo of this
  // clip (lib/matte.svelte matteMemo, read by the card's button): a
  // picture = computed, a 202 idle = nothing on disk.
  $effect(() => {
    if (!maskPrompt || !edgeMemoKey) return;
    const p = mask.pending;
    if (p && isIdleState(p.state)) noteMatteMemo(edgeMemoKey, 'idle');
    else if (shownCurrent) noteMatteMemo(edgeMemoKey, 'computed');
  });
  /** the guard's measurement of the mask on screen (null until decoded) */
  let stats = $state<MaskStats | null>(null);
  /** the guard judges BOX prompts only (a mask prompt IS the matte; points alone are the user's choice) */
  const guardOn = $derived(!!current?.box && !maskPrompt);
  $effect(() => {
    const s = stats;
    app.ui.promptWarning = guardOn && shownCurrent ? looseBoxWarning(s) : '';
  });
  onDestroy(() => {
    app.ui.promptWarning = '';
  });
  // The mask PNG (white = subject) becomes a green tint with the mask as
  // its alpha (50 %), on an offscreen canvas redrawn when the URL changes;
  // the guard measures the same pixels before they are tinted.
  let tint = $state<HTMLCanvasElement | null>(null);
  $effect(() => {
    const url = mask.url;
    stats = null;
    if (!url) {
      tint = null;
      return;
    }
    const im = new Image();
    let cancelled = false;
    im.onload = () => {
      if (cancelled) return;
      const w = im.naturalWidth;
      const h = im.naturalHeight;
      if (!(w > 0) || !(h > 0)) return;
      const off = document.createElement('canvas');
      off.width = w;
      off.height = h;
      const ctx = off.getContext('2d', { willReadFrequently: true });
      if (!ctx) return;
      ctx.drawImage(im, 0, 0);
      const data = ctx.getImageData(0, 0, w, h);
      const d = data.data;
      stats = maskStats(d, w, h);
      for (let i = 0; i < d.length; i += 4) {
        const lum = d[i]; // 8-bit gray: r = g = b
        d[i] = 40;
        d[i + 1] = 220;
        d[i + 2] = 90;
        d[i + 3] = lum >> 1; // 50 % where white, 0 where black
      }
      ctx.putImageData(data, 0, 0);
      tint = off;
    };
    im.src = url;
    return () => {
      cancelled = true;
    };
  });
  let now = $state(Date.now());
  $effect(() => {
    if (!mask.pending) return;
    now = Date.now();
    const t = setInterval(() => (now = Date.now()), 1000);
    return () => clearInterval(t);
  });
  const status = $derived.by(() => {
    if (mask.error) return `mask failed: ${mask.error}`;
    if (mask.pending) {
      // a mask prompt's idle 202: the edge model's matte of this clip is not computed, so there is nothing to show
      if (maskPrompt && isIdleState(mask.pending.state)) return maskPromptIdleText(edgeLabel, pendingReason);
      return mattePendingPill(mask.pending, now).replace(/^AI matte/, 'tracker');
    }
    if (mask.loading) return maskPrompt ? `fetching ${edgeLabel}’s matte of this frame…` : 'asking the tracker…';
    return '';
  });

  // Track the displayed size of the image (zoom / layout changes / new still).
  $effect(() => {
    const el = img;
    const measure = () => {
      cw = el.clientWidth;
      ch = el.clientHeight;
    };
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    el.addEventListener('load', measure);
    measure();
    return () => {
      ro.disconnect();
      el.removeEventListener('load', measure);
    };
  });

  // Redraw whenever geometry, the prompts or the mask change.
  $effect(() => {
    const c = canvas;
    if (!c || cw === 0 || ch === 0) return;
    const r = rect;
    const pts = points;
    const t = tint;
    const dpr = window.devicePixelRatio || 1;
    c.width = Math.round(cw * dpr);
    c.height = Math.round(ch * dpr);
    const ctx = c.getContext('2d');
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cw, ch);
    if (t) ctx.drawImage(t, 0, 0, cw, ch);
    const sx = cw / srcW;
    const sy = ch / srcH;
    if (r) {
      const x = r.x * sx;
      const y = r.y * sy;
      const w = r.w * sx;
      const h = r.h * sy;
      ctx.lineWidth = 1;
      ctx.strokeStyle = 'rgba(0,0,0,0.8)';
      ctx.strokeRect(x - 0.5, y - 0.5, w + 1, h + 1);
      ctx.strokeStyle = '#fff';
      ctx.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1);
      ctx.fillStyle = '#fff';
      const s = 6;
      for (const [px, py] of [
        [x, y],
        [x + w - s, y],
        [x, y + h - s],
        [x + w - s, y + h - s],
        [x + (w - s) / 2, y],
        [x + (w - s) / 2, y + h - s],
        [x, y + (h - s) / 2],
        [x + w - s, y + (h - s) / 2],
      ]) {
        ctx.fillRect(px, py, s, s);
      }
    }
    for (const p of pts) {
      const x = p.x * cw;
      const y = p.y * ch;
      ctx.beginPath();
      ctx.arc(x, y, 6, 0, Math.PI * 2);
      ctx.fillStyle = p.label === 1 ? 'rgba(35,165,89,0.95)' : 'rgba(242,63,67,0.95)';
      ctx.fill();
      ctx.lineWidth = 1.5;
      ctx.strokeStyle = '#fff';
      ctx.stroke();
      ctx.strokeStyle = '#fff';
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.moveTo(x - 3, y);
      ctx.lineTo(x + 3, y);
      if (p.label === 1) {
        ctx.moveTo(x, y - 3);
        ctx.lineTo(x, y + 3);
      }
      ctx.stroke();
    }
  });

  function toSrc(e: PointerEvent): { x: number; y: number } {
    const b = canvas!.getBoundingClientRect();
    const x = ((e.clientX - b.left) / b.width) * srcW;
    const y = ((e.clientY - b.top) / b.height) * srcH;
    return { x: Math.min(Math.max(0, x), srcW), y: Math.min(Math.max(0, y), srcH) };
  }

  function inside(p: { x: number; y: number }): boolean {
    const r = rect;
    return !!r && p.x >= r.x && p.x <= r.x + r.w && p.y >= r.y && p.y <= r.y + r.h;
  }

  /** the marker under a source-pixel position (−1 for none) */
  function markerAt(p: { x: number; y: number }): number {
    if (!(srcW > 0) || !(srcH > 0) || cw === 0 || ch === 0) return -1;
    return hitPoint(points, p.x / srcW, p.y / srcH, MARKER_TOL / cw, MARKER_TOL / ch);
  }

  function setBox(r: Rect | null) {
    setMattePrompts(setFrameBox(prompts, frame, r ? normBox(r, srcW, srcH) : null));
  }

  function addPoint(p: { x: number; y: number }, label: 0 | 1) {
    if (atCap) return;
    setMattePrompts(addFramePoint(prompts, frame, normPoint(p.x, p.y, srcW, srcH, label)));
  }

  function onDown(e: PointerEvent) {
    if (!canvas || cw === 0 || ch === 0) return;
    if (e.button !== 0 && e.button !== 2) return;
    e.preventDefault();
    const p = toSrc(e);
    // a click on a marker removes it (either button)
    const m = markerAt(p);
    if (m >= 0) {
      setMattePrompts(removeFramePoint(prompts, frame, m));
      return;
    }
    if (e.button === 2) {
      // right-click: a − point
      addPoint(p, 0);
      return;
    }
    try {
      canvas.setPointerCapture(e.pointerId);
    } catch {
      // synthetic / already-released pointer: dragging still works via bubbling moves
    }
    const handle = rect ? hitHandle(rect, p, tolX, tolY) : null;
    if (handle && rect) {
      drag = { mode: 'resize', handle, start: { x: rect.x, y: rect.y, w: rect.w, h: rect.h } };
    } else if (inside(p) && rect) {
      drag = { mode: 'move', ox: p.x - rect.x, oy: p.y - rect.y, w: rect.w, h: rect.h, sx: e.clientX, sy: e.clientY, moved: false };
    } else {
      drag = { mode: 'draw', ax: p.x, ay: p.y, sx: e.clientX, sy: e.clientY, moved: false, shift: e.shiftKey, button: e.button };
    }
  }

  /** a drag has moved far enough (4 display px) to be a drag rather than a click */
  function movedEnough(e: PointerEvent, sx: number, sy: number): boolean {
    return Math.abs(e.clientX - sx) >= 4 || Math.abs(e.clientY - sy) >= 4;
  }

  function onMove(e: PointerEvent) {
    const p = toSrc(e);
    if (!drag) {
      hover = markerAt(p) >= 0 ? 'point' : rect ? (hitHandle(rect, p, tolX, tolY) ?? (inside(p) ? 'move' : null)) : null;
      return;
    }
    if (drag.mode === 'draw') {
      if (!drag.moved && !movedEnough(e, drag.sx, drag.sy)) return;
      if (!drag.moved && atCap) return; // no new frame may be prompted
      drag.moved = true;
      setBox(drawRect({ x: drag.ax, y: drag.ay }, p, 0, srcW, srcH));
    } else if (drag.mode === 'resize') {
      setBox(resizeRect(drag.start, drag.handle, p, 0, srcW, srcH));
    } else {
      if (!drag.moved && !movedEnough(e, drag.sx, drag.sy)) return;
      drag.moved = true;
      const x = Math.round(Math.min(Math.max(0, p.x - drag.ox), srcW - drag.w));
      const y = Math.round(Math.min(Math.max(0, p.y - drag.oy), srcH - drag.h));
      setBox({ x, y, w: drag.w, h: drag.h });
    }
  }

  function onUp(e: PointerEvent) {
    if (!drag) return;
    try {
      canvas?.releasePointerCapture(e.pointerId);
    } catch {
      // not captured — nothing to release
    }
    const d = drag;
    drag = null;
    if (d.mode === 'draw' && !d.moved) addPoint({ x: d.ax, y: d.ay }, d.shift ? 0 : 1);
    else if (d.mode === 'move' && !d.moved) addPoint(toSrc(e), e.shiftKey ? 0 : 1);
  }

  function onCancel(e: PointerEvent) {
    if (!drag) return;
    try {
      canvas?.releasePointerCapture(e.pointerId);
    } catch {
      // not captured
    }
    drag = null;
  }
</script>

<canvas
  bind:this={canvas}
  class="prompt-overlay"
  style:cursor
  style:width="{cw}px"
  style:height="{ch}px"
  onpointerdown={onDown}
  onpointermove={onMove}
  onpointerup={onUp}
  onpointercancel={onCancel}
  onlostpointercapture={onCancel}
  oncontextmenu={(e) => e.preventDefault()}
  aria-label="Select the subject: drag a box around it, click to add a keep point, Shift-click or right-click to add a remove point, click a marker to remove it"
></canvas>
{#if status}
  <div class="mask-status" role="status">{status}</div>
{/if}

<style>
  .prompt-overlay {
    position: absolute;
    left: 0;
    top: 0;
    cursor: crosshair;
    touch-action: none;
  }
  .mask-status {
    position: absolute;
    left: 8px;
    bottom: 8px;
    background: rgba(30, 31, 34, 0.92);
    border: 1px solid var(--blue);
    color: var(--blue);
    border-radius: 999px;
    padding: 2px 10px;
    font-size: 12px;
    line-height: 1.4;
    pointer-events: none;
  }
</style>
