// Pure geometry for the crop rectangle: the resize handles of
// CropOverlay.svelte (review R3) and the "Lock ratio" helpers of the Crop
// card (review R2). Everything works in SOURCE pixels; the overlay converts
// its display-pixel tolerances before calling in. No Svelte, no DOM.

import { clamp } from './format';

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface Point {
  x: number;
  y: number;
}

/** The 8 resize handles: 4 corners + 4 edges, compass-named. */
export type Handle = 'nw' | 'n' | 'ne' | 'e' | 'se' | 's' | 'sw' | 'w';

/** cursorFor is the CSS cursor of a handle (nwse/nesw/ns/ew-resize). */
export function cursorFor(h: Handle): string {
  switch (h) {
    case 'nw':
    case 'se':
      return 'nwse-resize';
    case 'ne':
    case 'sw':
      return 'nesw-resize';
    case 'n':
    case 's':
      return 'ns-resize';
    default:
      return 'ew-resize';
  }
}

/**
 * hitHandle tests point p against the rectangle's handles. Tolerances are in
 * source pixels per axis (the caller converts ~7 display px through the
 * current zoom, so handles stay grabbable at any zoom). Corners win over
 * edges (they overlap on a small rectangle); an edge is hit anywhere along
 * its segment, not just at the drawn midpoint tick. null = no handle.
 */
export function hitHandle(rect: Rect, p: Point, tolX: number, tolY: number): Handle | null {
  const x0 = rect.x;
  const y0 = rect.y;
  const x1 = rect.x + rect.w;
  const y1 = rect.y + rect.h;
  const nearX0 = Math.abs(p.x - x0) <= tolX;
  const nearX1 = Math.abs(p.x - x1) <= tolX;
  const nearY0 = Math.abs(p.y - y0) <= tolY;
  const nearY1 = Math.abs(p.y - y1) <= tolY;
  // corners first — the nearest one (in tolerance-normalized distance), so a
  // tiny rectangle whose corner zones overlap still grabs the intended corner
  const corners: [Handle, number, number][] = [
    ['nw', x0, y0],
    ['ne', x1, y0],
    ['sw', x0, y1],
    ['se', x1, y1],
  ];
  let best: Handle | null = null;
  let bestD = Infinity;
  for (const [h, cx, cy] of corners) {
    if (Math.abs(p.x - cx) > tolX || Math.abs(p.y - cy) > tolY) continue;
    const d = ((p.x - cx) / (tolX || 1)) ** 2 + ((p.y - cy) / (tolY || 1)) ** 2;
    if (d < bestD) {
      bestD = d;
      best = h;
    }
  }
  if (best) return best;
  const inX = p.x >= x0 - tolX && p.x <= x1 + tolX;
  const inY = p.y >= y0 - tolY && p.y <= y1 + tolY;
  if (nearY0 && inX) return 'n';
  if (nearY1 && inX) return 's';
  if (nearX0 && inY) return 'w';
  if (nearX1 && inY) return 'e';
  return null;
}

/** rounds a span into an integer Rect inside the srcW×srcH frame, at least 1×1. */
function normalize(xa: number, ya: number, xb: number, yb: number, srcW: number, srcH: number): Rect {
  let w = Math.max(1, Math.round(Math.abs(xb - xa)));
  let h = Math.max(1, Math.round(Math.abs(yb - ya)));
  w = Math.min(w, Math.max(1, Math.round(srcW)));
  h = Math.min(h, Math.max(1, Math.round(srcH)));
  const x = clamp(Math.round(Math.min(xa, xb)), 0, Math.round(srcW) - w);
  const y = clamp(Math.round(Math.min(ya, yb)), 0, Math.round(srcH) - h);
  return { x, y, w, h };
}

/**
 * resizeRect drags one handle of `rect` (the rectangle AT DRAG START — pass
 * the same one for every move of a drag, so crossing the anchor flips the
 * rectangle instead of inverting w/h) to pointer p. ratio > 0 locks w:h to
 * it: corner drags keep the opposite corner as the anchor and follow the
 * pointer on the dominant axis; edge drags set the dragged axis and scale
 * the other around its centre. The result is integer, clamped into the
 * srcW×srcH frame and at least 1×1.
 */
export function resizeRect(rect: Rect, handle: Handle, p: Point, ratio: number, srcW: number, srcH: number): Rect {
  const left = handle.includes('w');
  const right = handle.includes('e');
  const top = handle.includes('n');
  const bottom = handle.includes('s');
  const px = clamp(p.x, 0, srcW);
  const py = clamp(p.y, 0, srcH);

  if (!(ratio > 0)) {
    // free: the dragged edge(s) follow the pointer, the anchor edges stand
    let xa = rect.x;
    let xb = rect.x + rect.w;
    let ya = rect.y;
    let yb = rect.y + rect.h;
    if (left) xa = px;
    if (right) xb = px;
    if (top) ya = py;
    if (bottom) yb = py;
    return normalize(xa, ya, xb, yb, srcW, srcH);
  }

  if ((left || right) && (top || bottom)) {
    // ratio-locked corner: anchor = the opposite corner
    const ax = left ? rect.x + rect.w : rect.x;
    const ay = top ? rect.y + rect.h : rect.y;
    const dx = px - ax;
    const dy = py - ay;
    const sx = dx !== 0 ? Math.sign(dx) : left ? -1 : 1;
    const sy = dy !== 0 ? Math.sign(dy) : top ? -1 : 1;
    // reach the pointer on the dominant axis, derive the other from the ratio
    let h = Math.max(Math.abs(dx) / ratio, Math.abs(dy), 1);
    let w = h * ratio;
    const maxW = sx > 0 ? srcW - ax : ax;
    const maxH = sy > 0 ? srcH - ay : ay;
    const s = Math.min(1, maxW / w, maxH / h);
    if (s > 0) {
      w *= s;
      h *= s;
    }
    return normalize(ax, ay, ax + sx * w, ay + sy * h, srcW, srcH);
  }

  if (left || right) {
    // ratio-locked horizontal edge: width follows the pointer, height scales
    // around the rectangle's vertical centre
    const ax = left ? rect.x + rect.w : rect.x;
    const dx = px - ax;
    const sx = dx !== 0 ? Math.sign(dx) : left ? -1 : 1;
    const cy = rect.y + rect.h / 2;
    let w = Math.max(Math.abs(dx), 1);
    let h = w / ratio;
    const maxW = sx > 0 ? srcW - ax : ax;
    const maxH = 2 * Math.min(cy, srcH - cy);
    const s = Math.min(1, maxW / w, maxH / h);
    if (s > 0) {
      w *= s;
      h *= s;
    }
    return normalize(ax, cy - h / 2, ax + sx * w, cy + h / 2, srcW, srcH);
  }

  // ratio-locked vertical edge: height follows, width scales around the centre
  const ay = top ? rect.y + rect.h : rect.y;
  const dy = py - ay;
  const sy = dy !== 0 ? Math.sign(dy) : top ? -1 : 1;
  const cx = rect.x + rect.w / 2;
  let h = Math.max(Math.abs(dy), 1);
  let w = h * ratio;
  const maxH = sy > 0 ? srcH - ay : ay;
  const maxW = 2 * Math.min(cx, srcW - cx);
  const s = Math.min(1, maxW / w, maxH / h);
  if (s > 0) {
    w *= s;
    h *= s;
  }
  return normalize(cx - w / 2, ay, cx + w / 2, ay + sy * h, srcW, srcH);
}

/**
 * drawRect is a new rectangle dragged out from anchor a to pointer p —
 * ratio > 0 constrains it (review R2). Same clamping as resizeRect.
 */
export function drawRect(a: Point, p: Point, ratio: number, srcW: number, srcH: number): Rect {
  return resizeRect({ x: a.x, y: a.y, w: 0, h: 0 }, 'se', p, ratio, srcW, srcH);
}

/** heightForWidth is the locked-ratio H for an edited W: round(w / ratio), clamped into the frame. */
export function heightForWidth(w: number, ratio: number, srcH: number): number {
  return clamp(Math.round(w / ratio), 1, Math.max(1, Math.round(srcH)));
}

/** widthForHeight is the locked-ratio W for an edited H: round(h × ratio), clamped into the frame. */
export function widthForHeight(h: number, ratio: number, srcW: number): number {
  return clamp(Math.round(h * ratio), 1, Math.max(1, Math.round(srcW)));
}

/**
 * ratioLabel prints a locked ratio for the "Lock ratio" label: a reduced
 * clean fraction when one with small terms exists ("4:3", "16:9", "1:1"),
 * else two decimals against 1 ("1.33:1"). '' for no lock.
 */
export function ratioLabel(ratio: number): string {
  if (!(ratio > 0) || !Number.isFinite(ratio)) return '';
  for (let d = 1; d <= 20; d++) {
    const n = ratio * d;
    const rn = Math.round(n);
    if (rn >= 1 && rn <= 60 && Math.abs(n - rn) < 1e-6) {
      const g = gcd(rn, d);
      return `${rn / g}:${d / g}`;
    }
  }
  return `${Number((ratio).toFixed(2))}:1`;
}

function gcd(a: number, b: number): number {
  while (b !== 0) [a, b] = [b, a % b];
  return a;
}
