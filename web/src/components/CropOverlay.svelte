<script lang="ts">
  // Canvas overlay drawn on top of the still while the Crop card is open.
  // The still shows the full pre-crop frame, so display pixels map linearly
  // onto source pixels: src = display * (srcW / clientWidth). Drag on empty
  // space to draw a new rectangle; drag inside the rectangle to move it.
  import { app } from '../lib/state.svelte';

  interface Props {
    img: HTMLImageElement;
    srcW: number;
    srcH: number;
  }
  let { img, srcW, srcH }: Props = $props();

  let canvas = $state<HTMLCanvasElement | null>(null);
  let cw = $state(0);
  let ch = $state(0);

  type Drag =
    | { mode: 'draw'; ax: number; ay: number }
    | { mode: 'move'; ox: number; oy: number; w: number; h: number };
  let drag: Drag | null = null;
  let hoverMove = $state(false);

  const crop = $derived(app.ops.crop);
  const rect = $derived(crop.enabled && crop.w > 0 && crop.h > 0 ? crop : null);

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

  // Redraw whenever geometry or the crop rectangle changes.
  $effect(() => {
    const c = canvas;
    if (!c || cw === 0 || ch === 0) return;
    const r = rect;
    const dpr = window.devicePixelRatio || 1;
    c.width = Math.round(cw * dpr);
    c.height = Math.round(ch * dpr);
    const ctx = c.getContext('2d');
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cw, ch);
    if (!r) return;
    const sx = cw / srcW;
    const sy = ch / srcH;
    const x = r.x * sx;
    const y = r.y * sy;
    const w = r.w * sx;
    const h = r.h * sy;
    ctx.fillStyle = 'rgba(0,0,0,0.55)';
    ctx.fillRect(0, 0, cw, ch);
    ctx.clearRect(x, y, w, h);
    ctx.lineWidth = 1;
    ctx.strokeStyle = 'rgba(0,0,0,0.8)';
    ctx.strokeRect(x - 0.5, y - 0.5, w + 1, h + 1);
    ctx.strokeStyle = '#fff';
    ctx.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1);
    // corner ticks
    ctx.fillStyle = '#fff';
    const s = 6;
    for (const [px, py] of [
      [x, y],
      [x + w - s, y],
      [x, y + h - s],
      [x + w - s, y + h - s],
    ]) {
      ctx.fillRect(px, py, s, s);
    }
    // size label
    const label = `${r.w}×${r.h}`;
    ctx.font = '11px system-ui, sans-serif';
    const tw = ctx.measureText(label).width + 8;
    const lx = Math.min(Math.max(0, x), cw - tw);
    const ly = y > 18 ? y - 17 : Math.min(ch - 16, y + h + 3);
    ctx.fillStyle = 'rgba(0,0,0,0.75)';
    ctx.fillRect(lx, ly, tw, 15);
    ctx.fillStyle = '#fff';
    ctx.fillText(label, lx + 4, ly + 11);
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

  function onDown(e: PointerEvent) {
    if (e.button !== 0 || !canvas || cw === 0 || ch === 0) return;
    e.preventDefault();
    try {
      canvas.setPointerCapture(e.pointerId);
    } catch {
      // synthetic / already-released pointer: dragging still works via bubbling moves
    }
    const p = toSrc(e);
    if (inside(p) && rect) {
      drag = { mode: 'move', ox: p.x - rect.x, oy: p.y - rect.y, w: rect.w, h: rect.h };
    } else {
      drag = { mode: 'draw', ax: p.x, ay: p.y };
    }
  }

  function onMove(e: PointerEvent) {
    const p = toSrc(e);
    if (!drag) {
      hoverMove = inside(p);
      return;
    }
    if (drag.mode === 'draw') {
      const x0 = Math.round(Math.min(drag.ax, p.x));
      const y0 = Math.round(Math.min(drag.ay, p.y));
      const x1 = Math.round(Math.max(drag.ax, p.x));
      const y1 = Math.round(Math.max(drag.ay, p.y));
      if (x1 - x0 < 1 || y1 - y0 < 1) return;
      app.ops.crop = { enabled: true, x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
    } else {
      const x = Math.round(Math.min(Math.max(0, p.x - drag.ox), srcW - drag.w));
      const y = Math.round(Math.min(Math.max(0, p.y - drag.oy), srcH - drag.h));
      app.ops.crop = { enabled: true, x, y, w: drag.w, h: drag.h };
    }
  }

  function onUp(e: PointerEvent) {
    if (!drag) return;
    try {
      canvas?.releasePointerCapture(e.pointerId);
    } catch {
      // not captured — nothing to release
    }
    drag = null;
  }
</script>

<canvas
  bind:this={canvas}
  class="crop-overlay"
  class:move={hoverMove}
  style:width="{cw}px"
  style:height="{ch}px"
  onpointerdown={onDown}
  onpointermove={onMove}
  onpointerup={onUp}
  onpointercancel={onUp}
  onlostpointercapture={onUp}
  aria-label="Crop rectangle: drag to draw, drag inside to move"
></canvas>

<style>
  .crop-overlay {
    position: absolute;
    left: 0;
    top: 0;
    cursor: crosshair;
    touch-action: none;
  }
  .crop-overlay.move {
    cursor: move;
  }
</style>
