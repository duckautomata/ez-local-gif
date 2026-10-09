// The guided model's prompt maths (Phase 5c Part B): normalisation to the
// source frame, the per-frame edits, keyframes, marker hits, the wire /
// recipe shapes and their rounding, the memo key and the summary.
import { describe, expect, it } from 'vitest';
import {
  addFramePoint,
  anchored,
  boxRect,
  canPromptFrame,
  clearFrame,
  framePrompts,
  hasMask,
  hitPoint,
  keyframes,
  MASK_FROM_EDGE,
  maskFrame,
  MAX_PROMPT_FRAMES,
  normBox,
  normPoint,
  PROMPT_DECIMALS,
  promptsKey,
  promptsValid,
  promptSummary,
  removeFramePoint,
  round4,
  setFrameBox,
  setFrameMask,
  toMattePrompt,
  toMattePrompts,
  toWirePrompts,
  wireForFrame,
  type FramePrompts,
} from './prompts';

const W = 640;
const H = 360;

describe('normalisation', () => {
  it('normBox / boxRect map a source-pixel rectangle to 0..1 and back; degenerate boxes are null', () => {
    expect(normBox({ x: 64, y: 72, w: 320, h: 180 }, W, H)).toEqual([0.1, 0.2, 0.6, 0.7]);
    expect(boxRect([0.1, 0.2, 0.6, 0.7], W, H)).toEqual({ x: 64, y: 72, w: 320, h: 180 });
    expect(normBox({ x: 0, y: 0, w: 640, h: 360 }, W, H)).toEqual([0, 0, 1, 1]);
    expect(normBox({ x: 10, y: 10, w: 0, h: 5 }, W, H)).toBeNull();
    expect(normBox({ x: 10, y: 10, w: 5, h: 0.5 }, W, H)).toBeNull();
    expect(normBox({ x: 10, y: 10, w: 5, h: 5 }, 0, H)).toBeNull();
    // outside the frame is clamped, a box at least 1×1 comes back
    expect(normBox({ x: 630, y: 350, w: 100, h: 100 }, W, H)).toEqual([630 / 640, 350 / 360, 1, 1]);
    expect(boxRect([0.5, 0.5, 0.5001, 0.5001], W, H)).toEqual({ x: 320, y: 180, w: 1, h: 1 });
  });

  it('normPoint clamps into the frame and keeps the label; round4 is the wire precision', () => {
    expect(normPoint(160, 90, W, H, 1)).toEqual({ x: 0.25, y: 0.25, label: 1 });
    expect(normPoint(-5, 1000, W, H, 0)).toEqual({ x: 0, y: 1, label: 0 });
    expect(normPoint(1, 1, 0, 0, 1)).toEqual({ x: 0, y: 0, label: 1 });
    expect(PROMPT_DECIMALS).toBe(4);
    expect(round4(0.33333333)).toBe(0.3333);
    expect(round4(0.00005)).toBe(0.0001);
    expect(Object.is(round4(-0.00001), 0)).toBe(true); // −0 folded
  });
});

describe('per-frame edits (immutable)', () => {
  const p0: FramePrompts[] = [];
  it('setFrameBox / addFramePoint / removeFramePoint / clearFrame keep the list frame-sorted, drop empty frames and never mutate', () => {
    const p1 = setFrameBox(p0, 5, [0.1, 0.1, 0.5, 0.5]);
    expect(p0).toEqual([]);
    expect(p1).toEqual([{ frame: 5, box: [0.1, 0.1, 0.5, 0.5], points: [] }]);
    const p2 = addFramePoint(p1, 2, { x: 0.3, y: 0.3, label: 1 });
    expect(p2.map((p) => p.frame)).toEqual([2, 5]);
    expect(p1).toHaveLength(1);
    const p3 = addFramePoint(p2, 5, { x: 0.9, y: 0.9, label: 0 });
    expect(framePrompts(p3, 5)).toEqual({ frame: 5, box: [0.1, 0.1, 0.5, 0.5], points: [{ x: 0.9, y: 0.9, label: 0 }] });
    expect(framePrompts(p3, 4)).toBeNull();
    // the box replaced, then cleared: the frame stays through its point
    const p4 = setFrameBox(p3, 5, [0.2, 0.2, 0.4, 0.4]);
    expect(framePrompts(p4, 5)?.box).toEqual([0.2, 0.2, 0.4, 0.4]);
    const p5 = setFrameBox(p4, 5, null);
    expect(framePrompts(p5, 5)).toEqual({ frame: 5, box: null, points: [{ x: 0.9, y: 0.9, label: 0 }] });
    // the last point removed: the frame is dropped
    const p6 = removeFramePoint(p5, 5, 0);
    expect(p6.map((p) => p.frame)).toEqual([2]);
    expect(removeFramePoint(p6, 2, 3)).toEqual(p6);
    expect(removeFramePoint(p6, 9, 0)).toEqual(p6);
    expect(clearFrame(p3, 5).map((p) => p.frame)).toEqual([2]);
    expect(clearFrame(p3, 7)).toEqual(p3);
    // the stored copies are detached from the inputs
    const b: [number, number, number, number] = [0.1, 0.1, 0.5, 0.5];
    const p7 = setFrameBox(p0, 1, b);
    b[0] = 0.9;
    expect(p7[0].box?.[0]).toBe(0.1);
  });

  it('caps the prompted frames at MAX_PROMPT_FRAMES: a new frame is refused, an existing one still takes edits', () => {
    expect(MAX_PROMPT_FRAMES).toBe(32);
    let p: FramePrompts[] = [];
    for (let i = 0; i < 32; i++) p = addFramePoint(p, i, { x: 0.5, y: 0.5, label: 1 });
    expect(p).toHaveLength(32);
    expect(canPromptFrame(p, 40)).toBe(false);
    expect(canPromptFrame(p, 3)).toBe(true);
    expect(addFramePoint(p, 40, { x: 0.5, y: 0.5, label: 1 })).toHaveLength(32);
    expect(setFrameBox(p, 40, [0, 0, 1, 1])).toHaveLength(32);
    expect(framePrompts(addFramePoint(p, 3, { x: 0.1, y: 0.1, label: 0 }), 3)?.points).toHaveLength(2);
    expect(setFrameBox(p, 40, null)).toHaveLength(32); // clearing a box of an absent frame is a no-op, not a refusal
  });
});

describe('keyframes / anchoring / hits', () => {
  const ps: FramePrompts[] = [
    { frame: 9, box: null, points: [{ x: 0.1, y: 0.1, label: 0 }] },
    { frame: 1, box: [0, 0, 0.5, 0.5], points: [{ x: 0.2, y: 0.2, label: 1 }, { x: 0.3, y: 0.3, label: 0 }] },
    { frame: 4, box: null, points: [] },
  ];
  it('keyframes lists the prompted frames in order with their counts; anchored / promptsValid need a box or a + click', () => {
    expect(keyframes(ps)).toEqual([
      { frame: 1, box: true, mask: false, positive: 1, negative: 1, anchored: true },
      { frame: 9, box: false, mask: false, positive: 0, negative: 1, anchored: false },
    ]);
    expect(anchored(ps[0])).toBe(false);
    expect(anchored(ps[1])).toBe(true);
    expect(anchored(null)).toBe(false);
    expect(promptsValid(ps)).toBe(true);
    expect(promptsValid([ps[0]])).toBe(false);
    expect(promptsValid([])).toBe(false);
  });

  it('hitPoint finds the nearest marker within the per-axis tolerance', () => {
    const pts = ps[1].points;
    expect(hitPoint(pts, 0.205, 0.195, 0.01, 0.01)).toBe(0);
    expect(hitPoint(pts, 0.305, 0.305, 0.01, 0.01)).toBe(1);
    expect(hitPoint(pts, 0.25, 0.25, 0.01, 0.01)).toBe(-1);
    expect(hitPoint(pts, 0.25, 0.25, 0.06, 0.06)).toBe(0); // both in range: the nearer (equal here → the first)
    expect(hitPoint([], 0.2, 0.2, 1, 1)).toBe(-1);
  });
});

describe('wire / recipe shapes', () => {
  const ps: FramePrompts[] = [
    { frame: 12, box: null, points: [{ x: 0.5, y: 0.5, label: 1 }] },
    { frame: 0, box: [0.1, 0.2, 0.6, 0.9], points: [{ x: 0.7, y: 0.4, label: 0 }, { x: 0.33333333, y: 0.66666667, label: 1 }] },
  ];
  it('toMattePrompts sorts by frame, rounds to 4 decimals, drops empty frames and everything when nothing anchors', () => {
    expect(toMattePrompts(ps)).toEqual([
      { frame: 0, points: [[0.7, 0.4, 0], [0.3333, 0.6667, 1]], box: [0.1, 0.2, 0.6, 0.9] },
      { frame: 12, points: [[0.5, 0.5, 1]] },
    ]);
    expect(toMattePrompts([{ frame: 2, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }] }])).toEqual([]);
    expect(toMattePrompts([{ frame: 2, box: null, points: [] }, { frame: 3, box: [0, 0, 1, 1], points: [] }])).toEqual([{ frame: 3, box: [0, 0, 1, 1] }]);
    expect(toMattePrompts([])).toEqual([]);
    expect(toWirePrompts(ps)).toEqual({ obj: 1, prompts: toMattePrompts(ps) });
    // a frame whose index is negative or fractional is clamped / rounded (the server refuses < 0)
    expect(toMattePrompts([{ frame: -1.4, box: [0, 0, 1, 1], points: [] }])).toEqual([{ frame: 0, box: [0, 0, 1, 1] }]);
  });

  it('wireForFrame is one frame’s prompts, null when that frame cannot select anything', () => {
    expect(wireForFrame(ps, 12)).toEqual({ obj: 1, prompts: [{ frame: 12, points: [[0.5, 0.5, 1]] }] });
    expect(wireForFrame(ps, 0)?.prompts).toHaveLength(1);
    expect(wireForFrame(ps, 5)).toBeNull();
    expect(wireForFrame([{ frame: 5, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }] }], 5)).toBeNull();
  });

  it('promptsKey is a canonical text: point order does not matter, every other change does', () => {
    const k = promptsKey(toWirePrompts(ps));
    expect(k).toBe('f0:b0.1,0.2,0.6,0.9:p0.3333,0.6667,1;0.7,0.4,0|f12:b-:p0.5,0.5,1');
    const swapped: FramePrompts[] = [ps[0], { ...ps[1], points: [ps[1].points[1], ps[1].points[0]] }];
    expect(promptsKey(toWirePrompts(swapped))).toBe(k);
    expect(promptsKey(toWirePrompts([ps[1]]))).not.toBe(k);
    expect(promptsKey(null)).toBe('');
  });

  it('promptSummary words the set for the card', () => {
    expect(promptSummary([])).toBe('select the subject');
    expect(promptSummary([{ frame: 1, box: [0, 0, 1, 1], points: [] }])).toBe('1 frame · 1 box');
    expect(promptSummary(ps)).toBe('2 frames · 1 box · 3 points');
    expect(promptSummary([{ frame: 2, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }] }])).toBe('1 frame · 1 point · needs a box or a + click');
  });
});

describe('mask prompts (Phase 5d: "Use this frame’s matte")', () => {
  const ps: FramePrompts[] = [
    { frame: 1, box: [0, 0, 0.5, 0.5], points: [{ x: 0.2, y: 0.2, label: 1 }] },
    { frame: 4, box: null, points: [{ x: 0.9, y: 0.9, label: 0 }] },
  ];

  it('setFrameMask replaces the frame’s box and clicks with the mask, anchors it, keeps one mask per set, and clears without touching the rest', () => {
    expect(MASK_FROM_EDGE).toBe('edge');
    const m1 = setFrameMask(ps, 1, 'edge');
    expect(ps[0].maskFrom).toBeUndefined(); // immutable
    expect(framePrompts(m1, 1)).toEqual({ frame: 1, box: null, points: [], maskFrom: 'edge' });
    expect(hasMask(framePrompts(m1, 1))).toBe(true);
    expect(anchored(framePrompts(m1, 1))).toBe(true);
    expect(maskFrame(m1)).toBe(1);
    expect(maskFrame(ps)).toBe(-1);
    // − clicks and a box refine it: the mask stays through every other edit
    const m2 = addFramePoint(m1, 1, { x: 0.8, y: 0.8, label: 0 });
    expect(framePrompts(m2, 1)).toEqual({ frame: 1, box: null, points: [{ x: 0.8, y: 0.8, label: 0 }], maskFrom: 'edge' });
    const m3 = setFrameBox(m2, 1, [0.1, 0.1, 0.9, 0.9]);
    expect(framePrompts(m3, 1)?.maskFrom).toBe('edge');
    expect(removeFramePoint(m3, 1, 0)).toEqual([{ frame: 1, box: [0.1, 0.1, 0.9, 0.9], points: [], maskFrom: 'edge' }, ps[1]]);
    // the mask moves to frame 4 (its clicks are replaced); frame 1 keeps its box and clicks without the mask
    const m4 = setFrameMask(m3, 4, 'edge');
    expect(maskFrame(m4)).toBe(4);
    expect(framePrompts(m4, 1)).toEqual({ frame: 1, box: [0.1, 0.1, 0.9, 0.9], points: [{ x: 0.8, y: 0.8, label: 0 }] });
    expect(framePrompts(m4, 4)).toEqual({ frame: 4, box: null, points: [], maskFrom: 'edge' });
    // a mask-only frame that loses the mask is dropped
    expect(setFrameMask(m4, 7, 'edge').map((p) => p.frame)).toEqual([1, 7]);
    // clearing keeps the frame's other prompts; a frame without a mask is left alone; a mask-only frame goes
    const m5 = setFrameMask(setFrameMask(m3, 1, null), 1, null);
    expect(framePrompts(m5, 1)).toEqual({ frame: 1, box: [0.1, 0.1, 0.9, 0.9], points: [{ x: 0.8, y: 0.8, label: 0 }] });
    expect(setFrameMask(m1, 1, null)).toEqual([ps[1]]);
    expect(setFrameMask(ps, 4, null)).toEqual(ps);
    expect(clearFrame(m1, 1)).toEqual([ps[1]]);
    // the cap applies to a new frame only
    let full: FramePrompts[] = [];
    for (let i = 0; i < 32; i++) full = addFramePoint(full, i, { x: 0.5, y: 0.5, label: 1 });
    expect(setFrameMask(full, 40, 'edge')).toHaveLength(32);
    expect(maskFrame(setFrameMask(full, 40, 'edge'))).toBe(-1);
    expect(maskFrame(setFrameMask(full, 3, 'edge'))).toBe(3);
  });

  it('a mask prompt anchors the set, shows in the strip as a mask row, serialises as {frame, maskFrom} (mask: true on the wire only), keys and sums up', () => {
    const only: FramePrompts[] = [{ frame: 3, box: null, points: [], maskFrom: 'edge' }];
    expect(promptsValid(only)).toBe(true);
    expect(keyframes(only)).toEqual([{ frame: 3, box: false, mask: true, positive: 0, negative: 0, anchored: true }]);
    expect(toMattePrompt(only[0])).toEqual({ frame: 3, maskFrom: 'edge' });
    expect(toMattePrompts(only)).toEqual([{ frame: 3, maskFrom: 'edge' }]);
    expect(toMattePrompts(only)[0]).not.toHaveProperty('mask');
    expect(wireForFrame(only, 3)).toEqual({ obj: 1, prompts: [{ frame: 3, maskFrom: 'edge', mask: true }] });
    expect(toWirePrompts(only)).toEqual({ obj: 1, prompts: [{ frame: 3, maskFrom: 'edge' }] });
    const refined = addFramePoint(only, 3, { x: 0.25, y: 0.75, label: 0 });
    expect(toMattePrompts(refined)).toEqual([{ frame: 3, maskFrom: 'edge', points: [[0.25, 0.75, 0]] }]);
    expect(wireForFrame(refined, 3)?.prompts[0]).toEqual({ frame: 3, maskFrom: 'edge', mask: true, points: [[0.25, 0.75, 0]] });
    // the key names the mask, so a mask op never dedupes against the same clicks without one
    expect(promptsKey(toWirePrompts(refined))).toBe('f3:b-:p0.25,0.75,0:medge');
    expect(promptsKey(toWirePrompts(only))).not.toBe(promptsKey(toWirePrompts([{ frame: 3, box: null, points: [{ x: 0.5, y: 0.5, label: 1 }] }])));
    expect(promptSummary(only)).toBe('1 frame · frame matte');
    expect(promptSummary([...refined, { frame: 9, box: [0, 0, 1, 1], points: [] }])).toBe('2 frames · frame matte · 1 box · 1 point');
    expect(hasMask({ maskFrom: undefined })).toBe(false);
    expect(hasMask(null)).toBe(false);
    expect(anchored({ box: null, points: [{ x: 0.1, y: 0.1, label: 0 }], maskFrom: 'edge' })).toBe(true);
  });
});
