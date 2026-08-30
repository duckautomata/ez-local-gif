// Crop-rectangle geometry (review R2/R3): handle hit-testing, edge/corner
// resizing with and without a locked ratio, clamping, flip-past-anchor, the
// ratio-locked draw and the W/H / label helpers of the Crop card.
import { describe, expect, it } from 'vitest';
import { cursorFor, drawRect, heightForWidth, hitHandle, ratioLabel, resizeRect, sizeForHeight, sizeForWidth, widthForHeight, type Handle, type Rect } from './croprect';

const r: Rect = { x: 10, y: 10, w: 40, h: 30 }; // corners (10,10)..(50,40)

describe('hitHandle', () => {
  it('finds the 4 corners, corners winning over edges', () => {
    expect(hitHandle(r, { x: 10, y: 10 }, 3, 3)).toBe('nw');
    expect(hitHandle(r, { x: 50, y: 10 }, 3, 3)).toBe('ne');
    expect(hitHandle(r, { x: 10, y: 40 }, 3, 3)).toBe('sw');
    expect(hitHandle(r, { x: 50, y: 40 }, 3, 3)).toBe('se');
    // near-but-not-exact still hits, on either side of the corner
    expect(hitHandle(r, { x: 49, y: 11 }, 3, 3)).toBe('ne');
    expect(hitHandle(r, { x: 52, y: 42 }, 3, 3)).toBe('se');
    // a tiny rectangle: every handle zone overlaps, corners win
    const tiny: Rect = { x: 10, y: 10, w: 2, h: 2 };
    expect(hitHandle(tiny, { x: 10, y: 10 }, 4, 4)).toBe('nw');
    expect(hitHandle(tiny, { x: 12, y: 12 }, 4, 4)).toBe('se');
  });

  it('finds the 4 edges anywhere along their segment', () => {
    expect(hitHandle(r, { x: 30, y: 10 }, 3, 3)).toBe('n');
    expect(hitHandle(r, { x: 20, y: 40 }, 3, 3)).toBe('s');
    expect(hitHandle(r, { x: 10, y: 25 }, 3, 3)).toBe('w');
    expect(hitHandle(r, { x: 50, y: 25 }, 3, 3)).toBe('e');
    // just inside / outside the line, within tolerance
    expect(hitHandle(r, { x: 30, y: 12 }, 3, 3)).toBe('n');
    expect(hitHandle(r, { x: 52, y: 25 }, 3, 3)).toBe('e');
  });

  it('misses the interior, the far outside, and beyond the tolerance', () => {
    expect(hitHandle(r, { x: 30, y: 25 }, 3, 3)).toBeNull(); // inside (a move, not a resize)
    expect(hitHandle(r, { x: 30, y: 60 }, 3, 3)).toBeNull();
    expect(hitHandle(r, { x: 5, y: 25 }, 3, 3)).toBeNull(); // 5 px off the left edge, tol 3
    expect(hitHandle(r, { x: 5, y: 25 }, 6, 3)).toBe('w'); // wider tolerance reaches it
  });

  it('uses per-axis tolerances (display pixels converted through an anisotropic zoom)', () => {
    expect(hitHandle(r, { x: 10, y: 18 }, 1, 10)).toBe('nw'); // 8 src px off vertically, tolY 10
    expect(hitHandle(r, { x: 18, y: 10 }, 1, 10)).toBe('n'); // but only 1 src px horizontally
    expect(hitHandle(r, { x: 12, y: 25 }, 1, 10)).toBeNull();
  });

  it('cursorFor maps handles onto the four resize cursors', () => {
    const want: Record<Handle, string> = {
      nw: 'nwse-resize',
      se: 'nwse-resize',
      ne: 'nesw-resize',
      sw: 'nesw-resize',
      n: 'ns-resize',
      s: 'ns-resize',
      e: 'ew-resize',
      w: 'ew-resize',
    };
    for (const [h, c] of Object.entries(want)) expect(cursorFor(h as Handle)).toBe(c);
  });
});

describe('resizeRect — free (no ratio)', () => {
  const W = 200;
  const H = 100;

  it('corner drags resize both axes from the opposite corner', () => {
    expect(resizeRect(r, 'se', { x: 80, y: 70 }, 0, W, H)).toEqual({ x: 10, y: 10, w: 70, h: 60 });
    expect(resizeRect(r, 'nw', { x: 0, y: 0 }, 0, W, H)).toEqual({ x: 0, y: 0, w: 50, h: 40 });
    expect(resizeRect(r, 'ne', { x: 60, y: 5 }, 0, W, H)).toEqual({ x: 10, y: 5, w: 50, h: 35 });
    expect(resizeRect(r, 'sw', { x: 5, y: 60 }, 0, W, H)).toEqual({ x: 5, y: 10, w: 45, h: 50 });
  });

  it('edge drags resize one axis and leave the other alone', () => {
    expect(resizeRect(r, 'e', { x: 80, y: 99 }, 0, W, H)).toEqual({ x: 10, y: 10, w: 70, h: 30 });
    expect(resizeRect(r, 'w', { x: 0, y: 0 }, 0, W, H)).toEqual({ x: 0, y: 10, w: 50, h: 30 });
    expect(resizeRect(r, 'n', { x: 199, y: 0 }, 0, W, H)).toEqual({ x: 10, y: 0, w: 40, h: 40 });
    expect(resizeRect(r, 's', { x: 0, y: 70 }, 0, W, H)).toEqual({ x: 10, y: 10, w: 40, h: 60 });
  });

  it('crossing the anchor flips the rectangle instead of inverting w/h', () => {
    // se dragged past the nw anchor: the rectangle lives on the other side
    expect(resizeRect(r, 'se', { x: 2, y: 4 }, 0, W, H)).toEqual({ x: 2, y: 4, w: 8, h: 6 });
    // w dragged past the right edge
    expect(resizeRect(r, 'w', { x: 60, y: 25 }, 0, W, H)).toEqual({ x: 50, y: 10, w: 10, h: 30 });
    // n dragged past the bottom edge
    expect(resizeRect(r, 'n', { x: 30, y: 55 }, 0, W, H)).toEqual({ x: 10, y: 40, w: 40, h: 15 });
  });

  it('clamps the pointer into the frame and never shrinks below 1×1', () => {
    expect(resizeRect(r, 'se', { x: 9999, y: 9999 }, 0, W, H)).toEqual({ x: 10, y: 10, w: 190, h: 90 });
    expect(resizeRect(r, 'nw', { x: -50, y: -50 }, 0, W, H)).toEqual({ x: 0, y: 0, w: 50, h: 40 });
    // dragged exactly onto the anchor: 1 px survives
    expect(resizeRect(r, 'e', { x: 10, y: 25 }, 0, W, H)).toEqual({ x: 10, y: 10, w: 1, h: 30 });
    expect(resizeRect(r, 'se', { x: 10, y: 10 }, 0, W, H)).toEqual({ x: 10, y: 10, w: 1, h: 1 });
  });
});

describe('resizeRect — locked ratio', () => {
  const W = 200;
  const H = 100;

  it('corner drags keep the ratio, anchored at the opposite corner, reaching the pointer on the dominant axis', () => {
    // ratio 2 (w:h = 2:1): dx dominates
    expect(resizeRect(r, 'se', { x: 90, y: 30 }, 2, W, H)).toEqual({ x: 10, y: 10, w: 80, h: 40 });
    // dy dominates
    expect(resizeRect(r, 'se', { x: 30, y: 60 }, 2, W, H)).toEqual({ x: 10, y: 10, w: 100, h: 50 });
    // a 1:1 lock from the nw handle (anchor = se corner at 50,40)
    expect(resizeRect(r, 'nw', { x: 20, y: 0 }, 1, W, H)).toEqual({ x: 10, y: 0, w: 40, h: 40 });
  });

  it('clamping to the frame preserves the ratio', () => {
    const out = resizeRect(r, 'se', { x: 200, y: 100 }, 2, W, H); // wants 190×95, height clamps to 90
    expect(out).toEqual({ x: 10, y: 10, w: 180, h: 90 });
    expect(out.w / out.h).toBe(2);
  });

  it('flipping past the anchor keeps the ratio on the far side', () => {
    const out = resizeRect(r, 'se', { x: 0, y: 0 }, 2, W, H); // past the nw anchor at (10,10)
    expect(out).toEqual({ x: 0, y: 5, w: 10, h: 5 });
    expect(out.w / out.h).toBe(2);
  });

  it('edge drags set their axis and scale the other around its centre', () => {
    const base: Rect = { x: 10, y: 20, w: 40, h: 30 }; // vertical centre 35
    expect(resizeRect(base, 'e', { x: 90, y: 0 }, 2, W, H)).toEqual({ x: 10, y: 15, w: 80, h: 40 });
    // n handle: anchor = the bottom edge, horizontal centre kept
    const b2: Rect = { x: 50, y: 40, w: 40, h: 30 }; // cx 70, bottom 70
    expect(resizeRect(b2, 'n', { x: 0, y: 10 }, 2, W, H)).toEqual({ x: 10, y: 10, w: 120, h: 60 });
    // and the centred axis clamps against the frame, rescaling the dragged one
    const nearTop: Rect = { x: 80, y: 4, w: 20, h: 10 }; // vertical centre 9: at most 18 px tall
    const out = resizeRect(nearTop, 'e', { x: 180, y: 9 }, 2, W, H);
    expect(out).toEqual({ x: 80, y: 0, w: 36, h: 18 });
    expect(out.w / out.h).toBe(2);
  });

  it('an edge drag through the anchor flips too', () => {
    const base: Rect = { x: 10, y: 20, w: 40, h: 30 };
    const out = resizeRect(base, 'e', { x: 2, y: 0 }, 2, W, H); // past the left edge at 10
    expect(out).toEqual({ x: 2, y: 33, w: 8, h: 4 });
  });
});

describe('drawRect', () => {
  it('draws a new rectangle between anchor and pointer, any direction', () => {
    expect(drawRect({ x: 10, y: 10 }, { x: 30, y: 50 }, 0, 200, 100)).toEqual({ x: 10, y: 10, w: 20, h: 40 });
    expect(drawRect({ x: 50, y: 50 }, { x: 20, y: 10 }, 0, 200, 100)).toEqual({ x: 20, y: 10, w: 30, h: 40 });
  });

  it('constrains a new rectangle to the locked ratio', () => {
    const out = drawRect({ x: 10, y: 10 }, { x: 30, y: 50 }, 1, 200, 100);
    expect(out).toEqual({ x: 10, y: 10, w: 40, h: 40 });
    expect(drawRect({ x: 10, y: 10 }, { x: 90, y: 20 }, 2, 200, 100)).toEqual({ x: 10, y: 10, w: 80, h: 40 });
  });
});

describe('W/H helpers of the locked Crop card inputs', () => {
  it('heightForWidth / widthForHeight round through the ratio and clamp into the frame', () => {
    expect(heightForWidth(100, 2, 100)).toBe(50);
    expect(heightForWidth(10, 4, 100)).toBe(3); // round(2.5) = 3
    expect(widthForHeight(30, 4 / 3, 200)).toBe(40);
    expect(heightForWidth(400, 1, 100)).toBe(100); // clamped to the frame
    expect(widthForHeight(500, 3, 200)).toBe(200);
    expect(heightForWidth(1, 50, 100)).toBe(1); // never below 1
  });

  // WEB-11: when the derived dimension clamps at the frame edge the typed
  // one shrinks with it, so the pair always keeps the locked ratio (the lock
  // label stays honest and later handle drags do not jump the box).
  it('sizeForWidth / sizeForHeight keep the locked ratio through a frame clamp', () => {
    // the finding's scenario: 160×120, lock 2:1, typed H 120 → 160×80 (not 160×120)
    expect(sizeForHeight(120, 2, 160, 120)).toEqual({ w: 160, h: 80 });
    // same clamp from the width side: typed W 160 at 1:2 on 160×120 → 60×120
    expect(sizeForWidth(160, 0.5, 160, 120)).toEqual({ w: 60, h: 120 });
    // the clamp floor: a huge ratio pins H at 1 and W follows it
    expect(sizeForWidth(10, 50, 160, 120)).toEqual({ w: 50, h: 1 });
  });

  it('sizeForWidth / sizeForHeight keep an unclamped typed value exactly', () => {
    expect(sizeForWidth(100, 2, 160, 120)).toEqual({ w: 100, h: 50 });
    expect(sizeForHeight(45, 4 / 3, 160, 120)).toEqual({ w: 60, h: 45 });
    // rounding alone (no frame clamp) never rewrites the typed value
    expect(sizeForWidth(101, 2, 160, 120)).toEqual({ w: 101, h: 51 });
  });
});

describe('ratioLabel', () => {
  it('prints clean reduced fractions and falls back to N.NN:1', () => {
    expect(ratioLabel(1)).toBe('1:1');
    expect(ratioLabel(4 / 3)).toBe('4:3');
    expect(ratioLabel(640 / 480)).toBe('4:3');
    expect(ratioLabel(16 / 9)).toBe('16:9');
    expect(ratioLabel(9 / 16)).toBe('9:16');
    expect(ratioLabel(2.5)).toBe('5:2');
    expect(ratioLabel(3)).toBe('3:1');
    expect(ratioLabel(1.33)).toBe('1.33:1'); // 133:100 is not clean
    expect(ratioLabel(127 / 96)).toBe('1.32:1');
    expect(ratioLabel(0)).toBe('');
    expect(ratioLabel(-2)).toBe('');
    expect(ratioLabel(NaN)).toBe('');
  });
});
