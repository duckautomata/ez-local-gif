<script lang="ts">
  // Drag-to-place boxes over the preview still for the overlays shown at the
  // scrubber time. The still is rendered at the output canvas size in this
  // mode, so its natural size IS the canvas: a box at (left, top, w, h)
  // canvas px sits at the same fractions of the displayed image whatever the
  // zoom. Dragging moves the overlay's anchor point by the pointer delta
  // scaled from display px to canvas px — per axis (overlay.displayScale),
  // since the displayed image need not keep the canvas aspect; arrow keys
  // nudge a focused box.
  import { clamp } from '../lib/format';
  import { activeAt, anchorPoint, clampBoxInside, displayScale, overlayBox, overlayLabel, overlayReady, type Box } from '../lib/overlay';
  import { app, type OverlayCfg } from '../lib/state.svelte';

  interface Props {
    img: HTMLImageElement;
    /** the still's natural size = the output canvas */
    canvasW: number;
    canvasH: number;
    /** output time of the frame on screen (its start) */
    t: number;
  }
  let { img, canvasW, canvasH, t }: Props = $props();

  interface Shown {
    o: OverlayCfg;
    box: Box;
  }
  const shown = $derived.by((): Shown[] => app.ops.overlays.filter((o) => overlayReady(o) && activeAt(o, t)).map((o) => ({ o, box: overlayBox(o) })));
  const hidden = $derived(app.ops.overlays.filter((o) => overlayReady(o) && !activeAt(o, t)).length);

  interface Drag {
    id: number;
    pointerId: number;
    startX: number;
    startY: number;
    x0: number;
    y0: number;
    /** canvas px per display px at drag start, per axis */
    scaleX: number;
    scaleY: number;
    moved: boolean;
  }
  let drag: Drag | null = null;

  function find(id: number): OverlayCfg | undefined {
    return app.ops.overlays.find((o) => o.id === id);
  }

  /** place moves overlay o so its box's top-left is (left, top), kept at least partly on the canvas. */
  function place(o: OverlayCfg, left: number, top: number) {
    const box = overlayBox(o);
    const c = clampBoxInside({ ...box, left, top }, canvasW, canvasH);
    const p = anchorPoint(o.anchor, c.left, c.top, box.w, box.h);
    o.x = Math.round(p.x);
    o.y = Math.round(p.y);
  }

  function onDown(e: PointerEvent, o: OverlayCfg) {
    if (e.button !== 0) return;
    e.preventDefault();
    const el = e.currentTarget as HTMLElement;
    try {
      el.setPointerCapture(e.pointerId);
    } catch {
      // synthetic pointer: moves still bubble here
    }
    app.ui.selectedOverlay = o.id;
    const s = displayScale(canvasW, canvasH, img.clientWidth, img.clientHeight);
    drag = { id: o.id, pointerId: e.pointerId, startX: e.clientX, startY: e.clientY, x0: o.x, y0: o.y, scaleX: s.x, scaleY: s.y, moved: false };
  }

  function onMove(e: PointerEvent) {
    if (!drag || e.pointerId !== drag.pointerId) return;
    const o = find(drag.id);
    if (!o) return;
    const dx = (e.clientX - drag.startX) * drag.scaleX;
    const dy = (e.clientY - drag.startY) * drag.scaleY;
    if (!drag.moved && Math.abs(dx) < 1 && Math.abs(dy) < 1) return;
    drag.moved = true;
    // The anchor point moves with the pointer; then keep the box on the canvas.
    const box = overlayBox({ ...o, x: drag.x0 + dx, y: drag.y0 + dy } as OverlayCfg);
    place(o, box.left, box.top);
  }

  function onUp(e: PointerEvent) {
    if (!drag || e.pointerId !== drag.pointerId) return;
    try {
      (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
    } catch {
      // not captured
    }
    drag = null;
  }

  function onKey(e: KeyboardEvent, o: OverlayCfg) {
    if (e.altKey || e.ctrlKey || e.metaKey) return;
    const step = e.shiftKey ? 10 : 1;
    let dx = 0;
    let dy = 0;
    switch (e.key) {
      case 'ArrowLeft':
        dx = -step;
        break;
      case 'ArrowRight':
        dx = step;
        break;
      case 'ArrowUp':
        dy = -step;
        break;
      case 'ArrowDown':
        dy = step;
        break;
      default:
        return;
    }
    e.preventDefault();
    const box = overlayBox(o);
    place(o, box.left + dx, box.top + dy);
  }

  const pct = (v: number, total: number) => `${total > 0 ? clamp((v / total) * 100, -1000, 1000) : 0}%`;
</script>

<div class="layer" aria-label="Overlay placement">
  {#each shown as s (s.o.id)}
    <div
      class="box"
      class:selected={app.ui.selectedOverlay === s.o.id}
      class:text={s.o.kind === 'text'}
      style:left={pct(s.box.left, canvasW)}
      style:top={pct(s.box.top, canvasH)}
      style:width={pct(s.box.w, canvasW)}
      style:height={pct(s.box.h, canvasH)}
      role="button"
      tabindex="0"
      aria-label="{overlayLabel(s.o)} — drag to place, arrow keys nudge (Shift ×10)"
      title="{overlayLabel(s.o)} — drag to place; arrow keys nudge, Shift ×10"
      onpointerdown={(e) => onDown(e, s.o)}
      onpointermove={onMove}
      onpointerup={onUp}
      onpointercancel={onUp}
      onlostpointercapture={onUp}
      onkeydown={(e) => onKey(e, s.o)}
    >
      <span class="tag">{overlayLabel(s.o)}</span>
    </div>
  {/each}
  {#if hidden > 0}
    <span class="off">{hidden} overlay{hidden === 1 ? '' : 's'} outside this frame's time range</span>
  {/if}
</div>

<style>
  .layer {
    position: absolute;
    inset: 0;
    pointer-events: none;
    overflow: visible;
  }
  .box {
    position: absolute;
    box-sizing: border-box;
    border: 1px dashed rgba(255, 255, 255, 0.85);
    outline: 1px dashed rgba(0, 0, 0, 0.6);
    outline-offset: 1px;
    cursor: move;
    pointer-events: auto;
    touch-action: none;
    min-width: 6px;
    min-height: 6px;
  }
  .box:hover,
  .box.selected {
    border-color: var(--accent);
    border-style: solid;
    background: rgba(88, 101, 242, 0.12);
  }
  .box:focus-visible {
    outline: 2px solid var(--accent);
  }
  .tag {
    position: absolute;
    left: -1px;
    top: -17px;
    max-width: 160px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 10px;
    line-height: 14px;
    padding: 0 5px;
    background: rgba(30, 31, 34, 0.85);
    color: #fff;
    border-radius: 3px;
    pointer-events: none;
  }
  .box.selected .tag {
    background: var(--accent);
  }
  .off {
    position: absolute;
    right: 6px;
    bottom: 6px;
    font-size: 11px;
    padding: 1px 7px;
    border-radius: 999px;
    background: rgba(30, 31, 34, 0.85);
    color: var(--muted);
    pointer-events: none;
  }
</style>
