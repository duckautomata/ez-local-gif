// The loose-box guard (Phase 5d): what a mask's coverage and touched edges
// measure to, and when they look like the background (the measured
// failure: a box over most of the frame makes SAM 2 pick the gradient).
import { describe, expect, it } from 'vitest';
import { EDGE_TOUCH_FRACTION, LOOSE_BOX_WARNING, LOOSE_COVERAGE, LOOSE_EDGES, looseBox, looseBoxWarning, MASK_SUBJECT_MIN, maskStats } from './maskguard';

/** an RGBA picture w×h, gray `value` inside the half-open rectangle [x0, y0, x1, y1) and 0 elsewhere */
function rgba(w: number, h: number, rect: [number, number, number, number] | null, value = 255): Uint8ClampedArray {
  const d = new Uint8ClampedArray(w * h * 4);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const on = !!rect && x >= rect[0] && y >= rect[1] && x < rect[2] && y < rect[3];
      const i = (y * w + x) * 4;
      d[i] = d[i + 1] = d[i + 2] = on ? value : 0;
      d[i + 3] = 255;
    }
  }
  return d;
}

describe('maskStats', () => {
  it('measures coverage and the touched edges; a stray edge pixel does not count as touching', () => {
    expect(LOOSE_COVERAGE).toBe(0.6);
    expect(LOOSE_EDGES).toBe(2);
    expect(EDGE_TOUCH_FRACTION).toBe(0.01);
    expect(MASK_SUBJECT_MIN).toBe(128);
    // a character standing on the bottom edge: 40 % wide, 80 % tall
    const s = maskStats(rgba(100, 100, [30, 20, 70, 100]), 100, 100);
    expect(s).toEqual({ width: 100, height: 100, subject: 3200, coverage: 0.32, top: false, bottom: true, left: false, right: false, edges: 1 });
    // a horizontal band across the frame touches left and right at 10 % coverage
    expect(maskStats(rgba(100, 100, [0, 40, 100, 50]), 100, 100)).toMatchObject({ coverage: 0.1, left: true, right: true, top: false, bottom: false, edges: 2 });
    // one pixel on the left and one on the right edge of a 200-px frame: under the 1 % (2 px) threshold
    const d = rgba(200, 200, [50, 50, 150, 150]);
    d[(10 * 200 + 0) * 4] = 255;
    d[(20 * 200 + 199) * 4] = 255;
    expect(maskStats(d, 200, 200)).toMatchObject({ subject: 10002, edges: 0, left: false, right: false });
    // two pixels on the left edge reach it
    d[(11 * 200 + 0) * 4] = 255;
    expect(maskStats(d, 200, 200)).toMatchObject({ edges: 1, left: true });
    // a tiny frame: one pixel is enough (ceil(w × 1 %) ≥ 1)
    expect(maskStats(rgba(4, 4, [0, 0, 1, 4]), 4, 4)).toMatchObject({ left: true, top: true, bottom: true, right: false, edges: 3, coverage: 0.25 });
  });

  it('reads the first channel at the stride: gray ≥ 128 is subject; a bare gray plane works too; degenerate input measures empty', () => {
    expect(maskStats(rgba(10, 10, [0, 0, 10, 10], 127), 10, 10).subject).toBe(0);
    expect(maskStats(rgba(10, 10, [0, 0, 10, 10], 128), 10, 10).subject).toBe(100);
    const gray = new Uint8Array(6 * 2);
    gray.fill(255, 0, 6); // the whole top row
    expect(maskStats(gray, 6, 2, 1)).toMatchObject({ subject: 6, coverage: 0.5, top: true, bottom: false, left: true, right: true, edges: 3 });
    const empty = { width: 0, height: 0, subject: 0, coverage: 0, top: false, bottom: false, left: false, right: false, edges: 0 };
    expect(maskStats(rgba(4, 4, null), 0, 4)).toEqual({ ...empty, height: 4 });
    expect(maskStats(rgba(4, 4, null), 4, 0)).toEqual({ ...empty, width: 4 });
    expect(maskStats(new Uint8ClampedArray(3), 4, 4)).toEqual({ ...empty, width: 4, height: 4 }); // too short
    expect(maskStats(rgba(4, 4, null), 4, 4, 0)).toEqual({ ...empty, width: 4, height: 4 }); // a stride under 1
    expect(maskStats(rgba(4, 4, null), 4, 4)).toMatchObject({ subject: 0, coverage: 0, edges: 0 }); // an empty mask
  });
});

describe('looseBox / looseBoxWarning', () => {
  it('warns over 60 % coverage or on two or more edges touched, never for an empty mask or nothing measured', () => {
    const tight = maskStats(rgba(100, 100, [30, 20, 70, 100]), 100, 100);
    expect(looseBox(tight)).toBe(false);
    expect(looseBoxWarning(tight)).toBe('');
    const most = maskStats(rgba(100, 100, [5, 5, 95, 95]), 100, 100); // 81 %, no edge
    expect(most.coverage).toBeCloseTo(0.81);
    expect(looseBox(most)).toBe(true);
    expect(looseBoxWarning(most)).toBe(LOOSE_BOX_WARNING);
    expect(LOOSE_BOX_WARNING).toBe('That looks like the background — tighten the box to the character or add a + click on it');
    const band = maskStats(rgba(100, 100, [0, 40, 100, 50]), 100, 100); // 10 %, left + right
    expect(looseBox(band)).toBe(true);
    const corner = maskStats(rgba(100, 100, [0, 0, 50, 50]), 100, 100); // 25 %, top + left
    expect(looseBox(corner)).toBe(true);
    const atLimit = maskStats(rgba(100, 100, [20, 1, 80, 99]), 100, 100); // 58.8 %, no edge
    expect(looseBox(atLimit)).toBe(false);
    expect(looseBox(maskStats(rgba(100, 100, null), 100, 100))).toBe(false);
    expect(looseBox(null)).toBe(false);
    expect(looseBoxWarning(undefined)).toBe('');
  });
});
