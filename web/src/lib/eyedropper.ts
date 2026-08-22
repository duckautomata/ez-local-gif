// Eyedropper for the "Pick a colour" background mode: maps a click on the
// displayed still onto the still's own pixels and reads the colour there
// through a canvas. The mapping is pure (eyedropper.test.ts); readPixel
// needs a browser.

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
