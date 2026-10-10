// Eyedropper for the Background card: maps a click on the displayed still
// onto the still's own pixels and reads the color there through a canvas,
// lands the pick in the Color mode row that armed it (landPick), and
// estimates a green / blue screen's real color from a whole unkeyed still
// for the Screen mode's auto-match (estimateScreenColor). The mapping, the
// landing and the estimate are pure (eyedropper.test.ts); readPixel and
// readImageData need a browser.

/** Rect is the displayed image's bounding box (DOMRect shape). */
export interface Rect {
  left: number;
  top: number;
  width: number;
  height: number;
}

/**
 * displayToPixel maps a client coordinate on an image displayed at `rect`
 * onto the image's natural naturalW × naturalH pixel grid (floor, so every
 * point inside a displayed pixel maps to that pixel). null outside the
 * image or when the geometry is unknown.
 */
export function displayToPixel(
  clientX: number,
  clientY: number,
  rect: Rect,
  naturalW: number,
  naturalH: number,
): { x: number; y: number } | null {
  if (!(rect.width > 0) || !(rect.height > 0) || !(naturalW > 0) || !(naturalH > 0)) return null;
  const fx = (clientX - rect.left) / rect.width;
  const fy = (clientY - rect.top) / rect.height;
  if (fx < 0 || fy < 0 || fx >= 1 || fy >= 1) return null;
  return { x: Math.min(naturalW - 1, Math.floor(fx * naturalW)), y: Math.min(naturalH - 1, Math.floor(fy * naturalH)) };
}

/** rgbToHex renders 8-bit channels as lowercase RRGGBB (no '#'). */
export function rgbToHex(r: number, g: number, b: number): string {
  const c = (v: number) => Math.min(255, Math.max(0, Math.round(v))).toString(16).padStart(2, '0');
  return c(r) + c(g) + c(b);
}

/** hexToRgb parses RRGGBB (optionally '#'-prefixed or with an AA suffix); null when malformed. */
export function hexToRgb(hex: string): { r: number; g: number; b: number } | null {
  const m = /^#?([0-9a-f]{6})(?:[0-9a-f]{2})?$/i.exec(hex.trim());
  if (!m) return null;
  const v = parseInt(m[1], 16);
  return { r: (v >> 16) & 255, g: (v >> 8) & 255, b: v & 255 };
}

/**
 * landPick returns the color rows with `hex` (RRGGBB, '#' and case
 * tolerated) landed in row `row` — the row whose "Pick from preview" armed
 * the eyedropper (app.ui.pickRow). A row index past the end means the
 * "+ add color" row that was armed and removed meanwhile, or a stale index:
 * the pick is appended while there is room (`max` rows) and otherwise
 * replaces the last row, so a click never lands nowhere. A malformed hex
 * leaves the rows as they are. Never mutates its input.
 */
export function landPick(colors: readonly string[], row: number, hex: string, max: number): string[] {
  const out = [...colors];
  const px = hexToRgb(hex);
  if (!px) return out;
  const n = rgbToHex(px.r, px.g, px.b);
  const i = Math.max(0, Math.floor(row));
  if (i < out.length) out[i] = n;
  else if (out.length < max) out.push(n);
  else if (out.length > 0) out[out.length - 1] = n;
  else out.push(n);
  return out;
}

/** a screen pixel's dominant channel must exceed both others by this much (8-bit) */
export const SCREEN_DOMINANCE = 40;
/** and be at least this bright (8-bit) */
export const SCREEN_MIN_LEVEL = 60;
/** below this share of the still's pixels there is no screen to match */
export const SCREEN_MIN_SHARE = 0.02;

/**
 * estimateScreenColor finds the real color of a green or blue screen in a
 * straight-RGBA still (the unkeyed preview frame): of the opaque pixels,
 * those whose `which` channel (G for green, B for blue) is at least
 * SCREEN_MIN_LEVEL and exceeds both other channels by SCREEN_DOMINANCE are
 * screen candidates; fewer than SCREEN_MIN_SHARE of all pixels → null (no
 * screen found — the caller keeps the preset), else the per-channel median
 * of the candidates as lowercase RRGGBB. The median ignores the subject's
 * green / blue clothing as long as the screen dominates the candidates, and
 * lands on a real camera screen (a blue screen is typically near #143cc8,
 * far from the pure #0000ff preset).
 */
export function estimateScreenColor(data: Uint8ClampedArray, which: 'green' | 'blue'): string | null {
  const total = Math.floor(data.length / 4);
  if (total === 0) return null;
  const hr = new Uint32Array(256);
  const hg = new Uint32Array(256);
  const hb = new Uint32Array(256);
  let n = 0;
  for (let i = 0; i + 3 < data.length; i += 4) {
    if (data[i + 3] !== 255) continue;
    const r = data[i];
    const g = data[i + 1];
    const b = data[i + 2];
    const [dom, o1, o2] = which === 'green' ? [g, r, b] : [b, r, g];
    if (dom < SCREEN_MIN_LEVEL || dom - o1 < SCREEN_DOMINANCE || dom - o2 < SCREEN_DOMINANCE) continue;
    hr[r]++;
    hg[g]++;
    hb[b]++;
    n++;
  }
  if (n === 0 || n < total * SCREEN_MIN_SHARE) return null;
  return rgbToHex(histMedian(hr, n), histMedian(hg, n), histMedian(hb, n));
}

/** histMedian is the lower median of n values counted in an 8-bit histogram. */
function histMedian(h: Uint32Array, n: number): number {
  const target = Math.floor((n - 1) / 2);
  let seen = 0;
  for (let v = 0; v < 256; v++) {
    seen += h[v];
    if (seen > target) return v;
  }
  return 255;
}

export interface Pixel {
  r: number;
  g: number;
  b: number;
  a: number;
}

/**
 * readPixel draws the loaded image onto a canvas and returns the straight
 * RGBA of pixel (x, y); null when the image cannot be read (not decoded yet,
 * canvas unavailable). The canvas is created once per image size and
 * redrawn only when the image source changed.
 */
export function readPixel(img: HTMLImageElement, x: number, y: number): Pixel | null {
  const w = img.naturalWidth;
  const h = img.naturalHeight;
  if (!(w > 0) || !(h > 0) || x < 0 || y < 0 || x >= w || y >= h) return null;
  const scratch = scratchFor(img);
  if (!scratch) return null;
  const d = scratch.getImageData(x, y, 1, 1).data;
  return { r: d[0], g: d[1], b: d[2], a: d[3] };
}

/**
 * readImageData returns the straight RGBA of the whole loaded image (the
 * Screen auto-match samples the unkeyed still with it); null when it cannot
 * be read (not decoded yet, canvas unavailable). Same canvas cache as
 * readPixel.
 */
export function readImageData(img: HTMLImageElement): Uint8ClampedArray | null {
  const w = img.naturalWidth;
  const h = img.naturalHeight;
  if (!(w > 0) || !(h > 0)) return null;
  const scratch = scratchFor(img);
  if (!scratch) return null;
  try {
    return scratch.getImageData(0, 0, w, h).data;
  } catch {
    return null;
  }
}

let cache: { src: string; w: number; h: number; ctx: CanvasRenderingContext2D } | null = null;

function scratchFor(img: HTMLImageElement): CanvasRenderingContext2D | null {
  const w = img.naturalWidth;
  const h = img.naturalHeight;
  if (cache && cache.src === img.currentSrc && cache.w === w && cache.h === h) return cache.ctx;
  const canvas = document.createElement('canvas');
  canvas.width = w;
  canvas.height = h;
  // willReadFrequently keeps getImageData cheap on repeated picks.
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  if (!ctx) return null;
  try {
    ctx.drawImage(img, 0, 0);
  } catch {
    return null;
  }
  cache = { src: img.currentSrc, w, h, ctx };
  return ctx;
}
