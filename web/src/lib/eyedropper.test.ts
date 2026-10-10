import { describe, expect, it } from 'vitest';
import { displayToPixel, estimateScreenColor, hexToRgb, landPick, rgbToHex, SCREEN_MIN_SHARE } from './eyedropper';

// A 200×100 still displayed at 400×200 (2× CSS scale) at (10, 20) on the page.
const rect = { left: 10, top: 20, width: 400, height: 200 };

describe('displayToPixel', () => {
  it('maps display coordinates onto the still pixel grid (floor)', () => {
    expect(displayToPixel(10, 20, rect, 200, 100)).toEqual({ x: 0, y: 0 });
    expect(displayToPixel(11.9, 21.9, rect, 200, 100)).toEqual({ x: 0, y: 0 }); // inside the first displayed pixel
    expect(displayToPixel(12, 22, rect, 200, 100)).toEqual({ x: 1, y: 1 });
    expect(displayToPixel(210, 120, rect, 200, 100)).toEqual({ x: 100, y: 50 });
    expect(displayToPixel(409.9, 219.9, rect, 200, 100)).toEqual({ x: 199, y: 99 });
  });

  it('handles a downscaled display (still larger than shown)', () => {
    const small = { left: 0, top: 0, width: 100, height: 50 }; // 400×200 still at 0.25×
    expect(displayToPixel(0.5, 0.5, small, 400, 200)).toEqual({ x: 2, y: 2 });
    expect(displayToPixel(99.9, 49.9, small, 400, 200)).toEqual({ x: 399, y: 199 });
  });

  it('returns null outside the image or without geometry', () => {
    expect(displayToPixel(9.9, 20, rect, 200, 100)).toBeNull();
    expect(displayToPixel(410, 100, rect, 200, 100)).toBeNull();
    expect(displayToPixel(100, 220, rect, 200, 100)).toBeNull();
    expect(displayToPixel(100, 100, { ...rect, width: 0 }, 200, 100)).toBeNull();
    expect(displayToPixel(100, 100, rect, 0, 100)).toBeNull();
    // a keyboard-triggered click reports (0, 0): outside the displayed image
    expect(displayToPixel(0, 0, rect, 200, 100)).toBeNull();
  });
});

describe('hex helpers', () => {
  it('rgbToHex renders lowercase RRGGBB and clamps', () => {
    expect(rgbToHex(0, 255, 0)).toBe('00ff00');
    expect(rgbToHex(49, 51, 56)).toBe('313338');
    expect(rgbToHex(300, -5, 12.6)).toBe('ff000d');
  });
  it('hexToRgb round-trips and tolerates # / alpha suffix', () => {
    expect(hexToRgb('313338')).toEqual({ r: 49, g: 51, b: 56 });
    expect(hexToRgb('#00FF00')).toEqual({ r: 0, g: 255, b: 0 });
    expect(hexToRgb('00000080')).toEqual({ r: 0, g: 0, b: 0 });
    expect(hexToRgb('zz0000')).toBeNull();
    expect(hexToRgb('fff')).toBeNull();
  });
});

// Phase 5a: the sampled color lands in the Color row that armed the
// eyedropper (app.ui.pickRow), normalised to lowercase RRGGBB.
describe('landPick', () => {
  const six = ['111111', '222222', '333333', '444444', '555555', '666666'];

  it('replaces the armed row; a row past the end appends while there is room, else replaces the last row', () => {
    expect(landPick([''], 0, '313338', 6)).toEqual(['313338']);
    expect(landPick(['313338', ''], 1, '#FACC82', 6)).toEqual(['313338', 'facc82']);
    expect(landPick(['313338', 'facc82'], 0, '1e3a8a', 6)).toEqual(['1e3a8a', 'facc82']);
    // the "+ add color" row that armed it was removed meanwhile: appended
    expect(landPick(['313338'], 3, '1e3a8a', 6)).toEqual(['313338', '1e3a8a']);
    expect(landPick([], 4, 'abcdef', 6)).toEqual(['abcdef']);
    // at the cap: the last row takes it, the count stays
    expect(landPick(six, 9, 'abcdef', 6)).toEqual([...six.slice(0, 5), 'abcdef']);
    expect(landPick(six, 2, 'abcdef', 6)).toEqual(['111111', '222222', 'abcdef', '444444', '555555', '666666']);
    // a negative / fractional row is clamped to a row
    expect(landPick(['313338', 'facc82'], -2, 'abcdef', 6)).toEqual(['abcdef', 'facc82']);
    expect(landPick(['313338', 'facc82'], 1.7, 'abcdef', 6)).toEqual(['313338', 'abcdef']);
  });

  it('ignores a malformed hex and never mutates its input', () => {
    const rows = ['313338', ''];
    expect(landPick(rows, 1, 'zz', 6)).toEqual(['313338', '']);
    expect(landPick(rows, 1, 'fff', 6)).toEqual(['313338', '']);
    const out = landPick(rows, 1, 'facc82', 6);
    expect(out).toEqual(['313338', 'facc82']);
    expect(rows).toEqual(['313338', '']);
    expect(out).not.toBe(rows);
  });
});

// The Screen mode's auto-match: the real color of a green / blue screen in
// the unkeyed preview still (straight RGBA), so the key is not the pure
// preset a camera screen never is.
describe('estimateScreenColor', () => {
  /** an RGBA buffer from [count, [r, g, b, a]] runs */
  function pixels(...runs: [number, [number, number, number, number]][]): Uint8ClampedArray {
    const n = runs.reduce((s, [c]) => s + c, 0);
    const d = new Uint8ClampedArray(n * 4);
    let i = 0;
    for (const [c, px] of runs) for (let k = 0; k < c; k++, i++) d.set(px, i * 4);
    return d;
  }

  it('returns the per-channel median of the screen pixels (a camera blue screen, not #0000ff)', () => {
    const d = pixels([30, [20, 60, 200, 255]], [20, [18, 58, 196, 255]], [10, [24, 64, 210, 255]], [40, [200, 170, 150, 255]]);
    expect(estimateScreenColor(d, 'blue')).toBe('143cc8');
    // per channel, not per pixel: the medians may come from different pixels
    const mixed = pixels([1, [10, 100, 200, 255]], [1, [30, 60, 220, 255]], [1, [20, 80, 180, 255]]);
    expect(estimateScreenColor(mixed, 'blue')).toBe('1450c8');
    // green
    const g = pixels([60, [40, 180, 60, 255]], [40, [250, 250, 250, 255]]);
    expect(estimateScreenColor(g, 'green')).toBe('28b43c');
    expect(estimateScreenColor(g, 'blue')).toBeNull();
  });

  it('a candidate needs its channel ≥ 60 and ≥ 40 above both others, and to be opaque', () => {
    // dominance 39 on one side: not a candidate
    expect(estimateScreenColor(pixels([10, [100, 139, 20, 255]]), 'green')).toBeNull();
    expect(estimateScreenColor(pixels([10, [99, 139, 20, 255]]), 'green')).toBe('638b14');
    // too dark: G 59 even though it dominates by 40+
    expect(estimateScreenColor(pixels([10, [0, 59, 0, 255]]), 'green')).toBeNull();
    expect(estimateScreenColor(pixels([10, [0, 60, 0, 255]]), 'green')).toBe('003c00');
    // transparent / semi-transparent pixels never count (a keyed or alpha source)
    expect(estimateScreenColor(pixels([10, [0, 255, 0, 0]]), 'green')).toBeNull();
    expect(estimateScreenColor(pixels([10, [0, 255, 0, 254]]), 'green')).toBeNull();
    expect(estimateScreenColor(new Uint8ClampedArray(0), 'green')).toBeNull();
  });

  it('below 2 % of the still the screen is not there: null (the caller keeps the preset)', () => {
    expect(SCREEN_MIN_SHARE).toBe(0.02);
    // 1 screen pixel in 100 (1 %): none; 2 in 100: found; the share counts every pixel, transparent ones included
    expect(estimateScreenColor(pixels([1, [0, 0, 200, 255]], [99, [128, 128, 128, 255]]), 'blue')).toBeNull();
    expect(estimateScreenColor(pixels([2, [0, 0, 200, 255]], [98, [128, 128, 128, 255]]), 'blue')).toBe('0000c8');
    expect(estimateScreenColor(pixels([1, [0, 0, 200, 255]], [99, [0, 0, 200, 0]]), 'blue')).toBeNull();
    // the subject's green shirt among a blue screen does not drag the blue estimate
    expect(estimateScreenColor(pixels([70, [20, 60, 200, 255]], [30, [30, 160, 40, 255]]), 'blue')).toBe('143cc8');
  });
});
