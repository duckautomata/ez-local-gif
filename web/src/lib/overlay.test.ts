import { describe, expect, it } from 'vitest';
import { ANCHORS, type Source } from './api';
import { frameAt, frameCount, frameStart, trimTime } from './format';
import {
  ACTIVE_TOLERANCE,
  activeAt,
  anchorOrigin,
  anchorPoint,
  clampBoxInside,
  displayScale,
  imageBoxSize,
  isAnimatedAsset,
  isFontName,
  newImageOverlay,
  newTextOverlay,
  overlayBox,
  overlayLabel,
  overlayReady,
  pickerColor,
  scrubberEnd,
  scrubberRange,
  scrubberStart,
  selectionAfterToggle,
  textBoxSize,
} from './overlay';

const still: Source = {
  hash: 'a'.repeat(64),
  name: 'logo.png',
  size: 10,
  info: { format: 'png_pipe', codec: 'png', pixFmt: 'rgba', bits: 8, width: 200, height: 100, fps: 0, duration: 0, frames: 1, hasAlpha: true, hasAudio: false, isStill: true, kind: 'image', premultiplied: false },
};
const anim: Source = { ...still, name: 'spin.gif', info: { ...still.info, format: 'gif', codec: 'gif', fps: 10, duration: 1, frames: 10, isStill: false, kind: 'animation' } };

describe('anchor maths (drag boxes)', () => {
  it('anchorOrigin places a 40×20 element by each of the nine anchors', () => {
    const want: Record<string, [number, number]> = {
      tl: [100, 50],
      tc: [80, 50],
      tr: [60, 50],
      ml: [100, 40],
      mc: [80, 40],
      mr: [60, 40],
      bl: [100, 30],
      bc: [80, 30],
      br: [60, 30],
    };
    for (const a of ANCHORS) {
      const { left, top } = anchorOrigin(a, 100, 50, 40, 20);
      expect([left, top], a).toEqual(want[a]);
    }
  });

  it('anchorPoint inverts anchorOrigin for every anchor', () => {
    for (const a of ANCHORS) {
      const o = anchorOrigin(a, 37, -12, 41, 19);
      expect(anchorPoint(a, o.left, o.top, 41, 19), a).toEqual({ x: 37, y: -12 });
    }
  });

  it('overlayBox: a bottom-right anchored image sits with its corner at X/Y', () => {
    const o = newImageOverlay(1);
    o.asset = still;
    o.anchor = 'br';
    o.x = 128;
    o.y = 128;
    expect(overlayBox(o)).toEqual({ left: -72, top: 28, w: 200, h: 100 });
    // dragging the box to (10, 10) moves the anchor point to its bottom-right corner
    expect(anchorPoint('br', 10, 10, 200, 100)).toEqual({ x: 210, y: 110 });
  });

  it('clampBoxInside keeps at least the margin visible on every side', () => {
    const box = { left: 0, top: 0, w: 40, h: 20 };
    expect(clampBoxInside({ ...box, left: -100, top: -100 }, 128, 128)).toEqual({ left: -32, top: -12 });
    expect(clampBoxInside({ ...box, left: 500, top: 500 }, 128, 128)).toEqual({ left: 120, top: 120 });
    expect(clampBoxInside({ ...box, left: 10, top: 20 }, 128, 128)).toEqual({ left: 10, top: 20 });
    // a box smaller than the margin uses its own size
    expect(clampBoxInside({ left: -10, top: 0, w: 4, h: 4 }, 128, 128)).toEqual({ left: 0, top: 0 });
  });

  it('displayScale maps each axis by its own ratio — a squashed still at a fixed zoom (review W2)', () => {
    // fit: the 400×400 canvas shown at 200×200
    expect(displayScale(400, 400, 200, 200)).toEqual({ x: 2, y: 2 });
    // 1× with the stage's max-height clamp: 400 px wide but only 200 px tall
    const s = displayScale(400, 400, 400, 200);
    expect(s).toEqual({ x: 1, y: 2 });
    // a 10 px drag down moves the overlay 20 canvas px, not 10
    expect([10 * s.x, 10 * s.y]).toEqual([10, 20]);
    // the old single scale (from the width alone) was wrong on that axis
    expect(10 * (400 / 400)).not.toBe(10 * s.y);
    // no display size yet: 1:1
    expect(displayScale(400, 300, 0, 0)).toEqual({ x: 1, y: 1 });
  });
});

describe('overlay footprints', () => {
  it('textBoxSize grows with the longest line, the line count, the outline and the box padding', () => {
    const base = { text: 'Hello', size: 32, border: 0, box: false, boxPad: 8 };
    expect(textBoxSize(base)).toEqual({ w: 96, h: 39 }); // 5 × 0.6 em, 1.2 em
    expect(textBoxSize({ ...base, text: 'Hello\nWorld!!' })).toEqual({ w: 135, h: 77 }); // 7 chars, 2 lines
    expect(textBoxSize({ ...base, border: 3 })).toEqual({ w: 102, h: 45 });
    expect(textBoxSize({ ...base, box: true })).toEqual({ w: 112, h: 55 });
    expect(textBoxSize({ ...base, box: false, boxPad: 99 })).toEqual({ w: 96, h: 39 }); // padding only with the box
    // an empty text still yields a graspable em square
    expect(textBoxSize({ ...base, text: '' })).toEqual({ w: 32, h: 39 });
    expect(textBoxSize({ ...base, size: 0 })).toEqual({ w: 96, h: 39 }); // 0 = the default 32 px
  });

  it('imageBoxSize: natural, aspect-kept from one side, exact from both, placeholder without an asset', () => {
    const o = newImageOverlay(1);
    expect(imageBoxSize(o)).toEqual({ w: 64, h: 64 });
    o.asset = still; // 200×100
    expect(imageBoxSize(o)).toEqual({ w: 200, h: 100 });
    o.width = 50;
    expect(imageBoxSize(o)).toEqual({ w: 50, h: 25 });
    o.width = 0;
    o.height = 50;
    expect(imageBoxSize(o)).toEqual({ w: 100, h: 50 });
    o.width = 30;
    expect(imageBoxSize(o)).toEqual({ w: 30, h: 50 });
    o.asset = null;
    expect(imageBoxSize(o)).toEqual({ w: 30, h: 50 });
  });
});

describe('overlay state helpers', () => {
  it('activeAt: start inclusive, end exclusive, 0 = to the end', () => {
    expect(activeAt({ start: 0, end: 0 }, 0)).toBe(true);
    expect(activeAt({ start: 0, end: 0 }, 99)).toBe(true);
    expect(activeAt({ start: 1, end: 2 }, 0.999)).toBe(false);
    expect(activeAt({ start: 1, end: 2 }, 1)).toBe(true);
    expect(activeAt({ start: 1, end: 2 }, 1.999)).toBe(true);
    expect(activeAt({ start: 1, end: 2 }, 2)).toBe(false);
    expect(activeAt({ start: 1, end: 0 }, 50)).toBe(true);
  });

  it('activeAt mirrors the render’s 1e-4 s enable tolerance, gte(t+0.0001,S)*lt(t+0.0001,E) (review W-G3)', () => {
    expect(ACTIVE_TOLERANCE).toBe(1e-4);
    // frame 2 of a 30 fps clip is at t = 0.0666666…; a Start rounded to the
    // nearest µs (0.066667) sits above it and is still shown there, as the
    // render shows it
    expect(activeAt({ start: trimTime(2 / 30), end: 0 }, 2 / 30)).toBe(true);
    expect(activeAt({ start: 0.066667, end: 0 }, 2 / 30)).toBe(true);
    // an End rounded up (5/30 → 0.166667) still excludes frame 5
    expect(activeAt({ start: 0, end: trimTime(5 / 30) }, 5 / 30)).toBe(false);
    expect(activeAt({ start: 0.066667, end: 0.166667 }, 5 / 30)).toBe(false);
    // the floored "from scrubber" bounds agree with it too
    expect(activeAt(scrubberRange(2, 30, 60), 2 / 30)).toBe(true);
    expect(activeAt({ start: 0, end: scrubberRange(4, 30, 60).end }, 5 / 30)).toBe(false);
    // the tolerance never reaches a neighbouring frame
    expect(activeAt({ start: 2 / 30, end: 0 }, 1 / 30)).toBe(false);
    expect(activeAt({ start: 0, end: 5 / 30 }, 4 / 30)).toBe(true);
    expect(activeAt({ start: 1 / 60, end: 0 }, 0)).toBe(false);
    expect(activeAt({ start: 0, end: 1 / 60 }, 0)).toBe(true);
    expect(activeAt({ start: 0, end: 1 / 60 }, 1 / 60)).toBe(false);
  });

  it('selectionAfterToggle: expanding selects, collapsing clears only its own selection, mounting collapsed keeps it (review W3)', () => {
    // a new card mounts collapsed right after addOverlay selected it
    expect(selectionAfterToggle(7, 7, false, false)).toBe(7);
    // a collapsed card never resets a selection made elsewhere (a box click)
    expect(selectionAfterToggle(3, 7, false, false)).toBe(3);
    expect(selectionAfterToggle(3, 7, false, true)).toBe(3);
    // expanding selects the card's overlay
    expect(selectionAfterToggle(0, 7, true, false)).toBe(7);
    expect(selectionAfterToggle(3, 7, true, false)).toBe(7);
    // collapsing a card that was open clears the selection while it is still its own
    expect(selectionAfterToggle(7, 7, false, true)).toBe(0);
  });

  it('pickerColor keeps the alpha suffix of the colour the picker replaces (review W5)', () => {
    expect(pickerColor('#FF0000', 'ffffff80')).toBe('ff000080');
    expect(pickerColor('#ff0000', 'ffffff')).toBe('ff0000');
    expect(pickerColor('00ff00', '00000080')).toBe('00ff0080');
  });

  it('overlayReady: enabled with text / an asset', () => {
    const t = newTextOverlay(1);
    expect(overlayReady(t)).toBe(true);
    t.text = '  \n ';
    expect(overlayReady(t)).toBe(false);
    t.text = 'x';
    t.enabled = false;
    expect(overlayReady(t)).toBe(false);
    const im = newImageOverlay(2);
    expect(overlayReady(im)).toBe(false);
    im.asset = still;
    expect(overlayReady(im)).toBe(true);
  });

  it('isAnimatedAsset and the labels', () => {
    expect(isAnimatedAsset(still)).toBe(false);
    expect(isAnimatedAsset(anim)).toBe(true);
    expect(isAnimatedAsset(null)).toBe(false);
    const t = newTextOverlay(1);
    expect(overlayLabel(t)).toBe('Text');
    t.text = '  Sale!  \nsecond line';
    expect(overlayLabel(t)).toBe('Sale!');
    t.text = 'x'.repeat(40);
    expect(overlayLabel(t)).toBe('x'.repeat(24) + '…');
    const im = newImageOverlay(2);
    expect(overlayLabel(im)).toBe('Image');
    im.asset = anim;
    expect(overlayLabel(im)).toBe('spin.gif');
  });

  it('isFontName accepts what recipe.TextParams.Font allows', () => {
    expect(isFontName('DejaVu Sans')).toBe(true);
    expect(isFontName('Noto Sans CJK-JP 2')).toBe(true);
    expect(isFontName('Noto Sans, Bold')).toBe(false);
    expect(isFontName("O'Brien")).toBe(false);
    expect(isFontName('')).toBe(false);
  });
});

describe('scrubberRange (Start / End "from scrubber")', () => {
  const RATES = [10, 12.5, 23.976, 24, 25, 29.97, 30, 30.303, 50, 59.94, 60];

  it('floors the grid time to whole µs so the scrubber frame is the first / last one drawn (review W1)', () => {
    // 30 fps, frame 3 (i = 2) starts at 0.0666666…: the nearest µs, 0.066667,
    // sits above the frame's own time, so both the render's gte(t,0.066667)
    // and the card's activeAt were false on the frame the user just chose.
    expect(scrubberRange(2, 30, 60)).toEqual({ start: 0.066666, end: 0.1 });
    expect(activeAt(scrubberRange(2, 30, 60), frameStart(2, 30))).toBe(true);
    // what the card used to store — shown too now that activeAt and the
    // render share the 1e-4 s tolerance (review W-G3), but the floor keeps
    // the bound at or below its frame independently of it
    expect(activeAt({ start: trimTime(2 / 30), end: 0 }, frameStart(2, 30))).toBe(true);
    // the render's raw comparison: ffmpeg's t is pts × (1/fps) in double
    expect(2 * (1 / 30) >= trimTime(2 / 30)).toBe(false);
    expect(2 * (1 / 30) >= scrubberRange(2, 30, 60).start).toBe(true);
    // End on frame 14 (i = 13): frame 14 is the last one drawn, 15 is not
    expect(scrubberRange(13, 30, 60).end).toBe(0.466666);
    expect(14 * (1 / 30) < trimTime(14 / 30)).toBe(true); // the old 0.466667 let frame 15 in
    expect(14 * (1 / 30) < scrubberRange(13, 30, 60).end).toBe(false);
    // Start + End from the same frame: exactly that frame, not nothing
    for (const i of [2, 5, 8]) {
      const w = scrubberRange(i, 30, 60);
      expect(activeAt(w, frameStart(i, 30)), `frame ${i}`).toBe(true);
      expect(activeAt(w, frameStart(i - 1, 30)), `frame ${i - 1}`).toBe(false);
      expect(activeAt(w, frameStart(i + 1, 30)), `frame ${i + 1}`).toBe(false);
    }
  });

  it('right after Start / End "from scrubber" the overlay is shown on the scrubber frame and not on the next one', () => {
    const o = newTextOverlay(1);
    for (const i of [2, 5, 8]) {
      scrubberStart(o, i, 30, 60);
      expect(o.start, `frame ${i} start`).toBe(scrubberRange(i, 30, 60).start);
      expect(activeAt(o, frameStart(i, 30)), `frame ${i} shown after Start`).toBe(true);
      expect(activeAt(o, frameStart(i - 1, 30)), `frame ${i - 1} hidden after Start`).toBe(false);
      expect(frameAt(o.start, 30, 60) - 1, `frame ${i} go to start`).toBe(i);
      // End on the same frame: that frame and only that frame
      expect(scrubberEnd(o, i, 30, 60)).toBe(true);
      expect(o.end, `frame ${i} end`).toBe(scrubberRange(i, 30, 60).end);
      expect(activeAt(o, frameStart(i, 30)), `frame ${i} shown after End`).toBe(true);
      expect(activeAt(o, frameStart(i + 1, 30)), `frame ${i + 1} hidden after End`).toBe(false);
    }
    // End on frame 14 (i = 13): shown up to and including it
    scrubberStart(o, 2, 30, 60);
    expect(scrubberEnd(o, 13, 30, 60)).toBe(true);
    expect(o).toMatchObject({ start: 0.066666, end: 0.466666 });
    expect(activeAt(o, frameStart(13, 30))).toBe(true);
    expect(activeAt(o, frameStart(14, 30))).toBe(false);
    // an End at or before Start is refused and leaves the range alone
    expect(scrubberEnd(o, 1, 30, 60)).toBe(false);
    expect(o).toMatchObject({ start: 0.066666, end: 0.466666 });
    // a Start past the End clears the End (to the end); the last frame ends "to the end"
    scrubberStart(o, 20, 30, 60);
    expect(o).toMatchObject({ start: 0.666666, end: 0 });
    expect(scrubberEnd(o, 59, 30, 60)).toBe(true);
    expect(o.end).toBe(0);
  });

  it('keeps on-grid times, ends "to the end" on the last frame, and is the whole clip without a rate', () => {
    expect(scrubberRange(13, 25, 50)).toEqual({ start: 0.52, end: 0.56 });
    expect(scrubberRange(0, 25, 50)).toEqual({ start: 0, end: 0.04 });
    expect(scrubberRange(49, 25, 50)).toEqual({ start: 1.96, end: 0 });
    expect(scrubberRange(3, 25, 0)).toEqual({ start: 0.12, end: 0.16 }); // unknown count: never the last frame
    expect(scrubberRange(-2, 25, 50)).toEqual({ start: 0, end: 0.04 });
    expect(scrubberRange(3, 0, 50)).toEqual({ start: 0, end: 0 });
  });

  it('at every common rate the scrubber frame is shown, its neighbours are not, and "go to start" lands on it', () => {
    for (const fps of RATES) {
      const n = frameCount(10, fps);
      const total = n + 100; // i is never the last frame here
      for (let i = 0; i < n; i++) {
        const w = scrubberRange(i, fps, total);
        const tag = `${fps} fps frame ${i}`;
        expect(activeAt(w, frameStart(i, fps)), `${tag} shown`).toBe(true);
        if (i > 0) expect(activeAt(w, frameStart(i - 1, fps)), `${tag} previous hidden`).toBe(false);
        expect(activeAt(w, frameStart(i + 1, fps)), `${tag} next hidden`).toBe(false);
        expect(frameAt(w.start, fps, total) - 1, `${tag} go to start`).toBe(i);
        // whole µs as buildOps sends them (trimTime leaves them alone), never above the grid, one frame long
        expect(trimTime(w.start), `${tag} start µs`).toBe(w.start);
        expect(trimTime(w.end), `${tag} end µs`).toBe(w.end);
        expect(w.start, `${tag} start`).toBeLessThanOrEqual(frameStart(i, fps));
        expect(w.end, `${tag} end`).toBeLessThanOrEqual((i + 1) / fps);
        expect(w.end - w.start, `${tag} length`).toBeGreaterThan(1 / fps - 2e-6);
      }
    }
  });
});
