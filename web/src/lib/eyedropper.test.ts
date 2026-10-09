import { describe, expect, it } from 'vitest';
import { displayToPixel, hexToRgb, landPick, rgbToHex } from './eyedropper';

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

// Phase 5a: the sampled colour lands in the Colour row that armed the
// eyedropper (app.ui.pickRow), normalised to lowercase RRGGBB.
describe('landPick', () => {
  const six = ['111111', '222222', '333333', '444444', '555555', '666666'];

  it('replaces the armed row; a row past the end appends while there is room, else replaces the last row', () => {
    expect(landPick([''], 0, '313338', 6)).toEqual(['313338']);
    expect(landPick(['313338', ''], 1, '#FACC82', 6)).toEqual(['313338', 'facc82']);
    expect(landPick(['313338', 'facc82'], 0, '1e3a8a', 6)).toEqual(['1e3a8a', 'facc82']);
    // the "+ add colour" row that armed it was removed meanwhile: appended
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
