// The loose-box guard of the Select subject panel (Phase 5d): measured on
// the live stack, a BOX that covers most of the frame makes SAM 2 select
// the gradient inside the box instead of the character (centre alpha 0,
// IoU 0.004 against the per-frame matte), while a box slightly larger than
// the character is right. So after the live overlay for a box arrives the
// SPA reads the mask PNG it painted (the canvas pixels the overlay already
// decodes for the green tint) and judges it: a mask covering more than
// LOOSE_COVERAGE of the frame, or touching at least LOOSE_EDGES of the
// frame's edges, looks like the background, and the card shows
// LOOSE_BOX_WARNING under the panel — a warning only, nothing is changed
// for the user. Framework-free (maskguard.test.ts).

/** LOOSE_COVERAGE: a mask covering more than this fraction of the frame looks like the background. */
export const LOOSE_COVERAGE = 0.6;
/** LOOSE_EDGES: a mask touching this many frame edges (or more) looks like the background. */
export const LOOSE_EDGES = 2;
/**
 * EDGE_TOUCH_FRACTION: an edge counts as touched when at least this
 * fraction of its pixels (and at least one) are subject — a character
 * standing on the bottom edge touches it, a stray pixel does not.
 */
export const EDGE_TOUCH_FRACTION = 0.01;
/** MASK_SUBJECT_MIN: a mask pixel at or above this gray value is subject (the sidecar's masks are 0 / 255). */
export const MASK_SUBJECT_MIN = 128;
/** LOOSE_BOX_WARNING is the text under the panel when a box selection looks like the background. */
export const LOOSE_BOX_WARNING = 'That looks like the background — tighten the box to the character or add a + click on it';

/** MaskStats is what the guard measures on a mask. */
export interface MaskStats {
  width: number;
  height: number;
  /** subject pixels (gray ≥ MASK_SUBJECT_MIN) */
  subject: number;
  /** subject / (width × height); 0 for an empty picture */
  coverage: number;
  top: boolean;
  bottom: boolean;
  left: boolean;
  right: boolean;
  /** how many of the four edges are touched */
  edges: number;
}

/**
 * maskStats measures a decoded mask: `data` is the picture's pixels with
 * `stride` channels per pixel (4 for canvas RGBA — a gray PNG decodes to
 * r = g = b — or 1 for a bare gray plane), the first channel being the
 * gray value. Coverage and the touched edges; a degenerate size yields an
 * empty measurement.
 */
export function maskStats(data: ArrayLike<number>, width: number, height: number, stride = 4): MaskStats {
  const w = Math.floor(width);
  const h = Math.floor(height);
  const out: MaskStats = { width: w, height: h, subject: 0, coverage: 0, top: false, bottom: false, left: false, right: false, edges: 0 };
  if (!(w > 0) || !(h > 0) || !(stride >= 1) || data.length < w * h * stride) return out;
  let subject = 0;
  let top = 0;
  let bottom = 0;
  let left = 0;
  let right = 0;
  for (let y = 0; y < h; y++) {
    const row = y * w;
    for (let x = 0; x < w; x++) {
      if (data[(row + x) * stride] < MASK_SUBJECT_MIN) continue;
      subject++;
      if (y === 0) top++;
      if (y === h - 1) bottom++;
      if (x === 0) left++;
      if (x === w - 1) right++;
    }
  }
  const minX = Math.max(1, Math.ceil(w * EDGE_TOUCH_FRACTION));
  const minY = Math.max(1, Math.ceil(h * EDGE_TOUCH_FRACTION));
  out.subject = subject;
  out.coverage = subject / (w * h);
  out.top = top >= minX;
  out.bottom = bottom >= minX;
  out.left = left >= minY;
  out.right = right >= minY;
  out.edges = Number(out.top) + Number(out.bottom) + Number(out.left) + Number(out.right);
  return out;
}

/** looseBox: the measured mask looks like the background (coverage over LOOSE_COVERAGE, or LOOSE_EDGES or more edges touched). */
export function looseBox(s: MaskStats | null | undefined): boolean {
  return !!s && s.subject > 0 && (s.coverage > LOOSE_COVERAGE || s.edges >= LOOSE_EDGES);
}

/** looseBoxWarning is LOOSE_BOX_WARNING when the mask looks like the background, '' otherwise. */
export function looseBoxWarning(s: MaskStats | null | undefined): string {
  return looseBox(s) ? LOOSE_BOX_WARNING : '';
}
