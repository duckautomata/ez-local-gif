// Phase 4 bounce op: serialisation order and the doubled frame model
// (planFrames / previewDuration mirror graph.Plan's exact ×2; the
// "from scrubber" helpers fold the mirrored half back onto the forward
// timeline).
import { describe, expect, it } from 'vitest';
import type { ProbeInfo } from './api';
import { defaultOutput } from './presets';
import {
  buildOps,
  defaultOps,
  forwardDuration,
  forwardFrame,
  frameWindow,
  planFrames,
  previewDuration,
  toSourceTime,
} from './state.svelte';

const clip: ProbeInfo = {
  format: 'mov',
  codec: 'prores',
  pixFmt: 'yuva444p10le',
  bits: 10,
  width: 320,
  height: 240,
  fps: 25,
  duration: 2,
  frames: 50,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'video',
  premultiplied: false,
};
const still: ProbeInfo = { ...clip, isStill: true, duration: 0, frames: 1, kind: 'image' };
const seq: ProbeInfo = {
  ...clip,
  format: 'sequence',
  codec: 'png',
  fps: 10,
  duration: 1,
  frames: 10,
  kind: 'sequence',
  sequence: { count: 10, pattern: 'f%d.png', delayMs: 100, mixed: false },
};

describe('buildOps: bounce', () => {
  it('emits {kind:"bounce"} with no params, after reverse', () => {
    const ops = defaultOps(clip);
    ops.bounce = true;
    expect(buildOps(ops)).toEqual([{ kind: 'bounce' }]);
    ops.reverse = true;
    expect(buildOps(ops)).toEqual([{ kind: 'reverse' }, { kind: 'bounce' }]);
  });

  it('sits after the geometry ops and before the overlays', () => {
    const ops = defaultOps(clip);
    ops.bounce = true;
    ops.crop = { enabled: true, x: 0, y: 0, w: 100, h: 100 };
    ops.flipRotate = { enabled: true, horizontal: true, vertical: false, degrees: 90 };
    expect(buildOps(ops).map((o) => o.kind)).toEqual(['crop', 'flip', 'rotate', 'bounce']);
  });

  it('off by default; a new defaultOps clears it', () => {
    expect(defaultOps(clip).bounce).toBe(false);
    expect(buildOps(defaultOps(clip))).toEqual([]);
  });
});

describe('bounce frame model', () => {
  it('doubles previewDuration and planFrames exactly (2 s at 25 fps: 50 → 100 frames, 4 s)', () => {
    const ops = defaultOps(clip);
    const out = defaultOutput();
    const base = planFrames(clip, ops, out);
    expect(base).toBe(50);
    ops.bounce = true;
    expect(previewDuration(clip, ops)).toBe(4);
    expect(forwardDuration(clip, ops)).toBe(2);
    expect(planFrames(clip, ops, out)).toBe(100);
  });

  it('is an exact ×2 of the forward count, never a re-floor of the doubled duration', () => {
    // 0.9 s at 25 fps = 22 forward frames (floor 22.5); doubled = 44,
    // while floor(1.8 × 25) would be 45.
    const ops = defaultOps(clip);
    const out = defaultOutput();
    ops.trim = { enabled: true, start: 0, end: 0.9 };
    expect(planFrames(clip, ops, out)).toBe(22);
    ops.bounce = true;
    expect(planFrames(clip, ops, out)).toBe(44);
    expect(previewDuration(clip, ops)).toBeCloseTo(1.8, 9);
  });

  it('composes with trim and speed (doubling applies after them)', () => {
    const ops = defaultOps(clip);
    const out = defaultOutput();
    ops.trim = { enabled: true, start: 0.5, end: 1.5 };
    ops.speed = { enabled: true, factor: 2 };
    ops.bounce = true;
    expect(forwardDuration(clip, ops)).toBeCloseTo(0.5, 9);
    expect(previewDuration(clip, ops)).toBeCloseTo(1, 9);
    expect(planFrames(clip, ops, out)).toBe(24); // floor(0.5 × 25 − ε) = 12, ×2
  });

  it('doubles an image sequence on its exact grid', () => {
    const ops = defaultOps(seq);
    const out = defaultOutput();
    expect(planFrames(seq, ops, out)).toBe(10);
    ops.bounce = true;
    expect(planFrames(seq, ops, out)).toBe(20);
    expect(previewDuration(seq, ops)).toBeCloseTo(2, 9);
  });

  it('leaves a still source at one frame (the graph refuses bouncing stills)', () => {
    const ops = defaultOps(still);
    ops.bounce = true;
    expect(planFrames(still, ops, defaultOutput())).toBe(1);
    expect(previewDuration(still, ops)).toBe(0);
  });
});

describe('bounce time mapping (output seconds, mirrored half folds back)', () => {
  it('toSourceTime maps t < D forward and t ≥ D mirrored', () => {
    const ops = defaultOps(clip);
    ops.bounce = true;
    // D = 2 s: t = 0.5 forward, t = 3.5 mirrors to 0.5, t = 4 to 0
    expect(toSourceTime(0.5, clip, ops)).toBeCloseTo(0.5, 9);
    expect(toSourceTime(3.5, clip, ops)).toBeCloseTo(0.5, 9);
    expect(toSourceTime(4, clip, ops)).toBeCloseTo(0, 9);
    expect(toSourceTime(2, clip, ops)).toBeCloseTo(2, 9); // the seam plays the last forward frame
  });

  it('frameWindow folds a mirrored-half frame onto its forward twin', () => {
    const ops = defaultOps(clip);
    const out = defaultOutput();
    ops.bounce = true;
    const total = planFrames(clip, ops, out); // 100
    // frame k and its mirror 2N−1−k select the same source window
    expect(frameWindow(clip, ops, out, 3)).toEqual(frameWindow(clip, ops, out, total - 1 - 3));
    expect(frameWindow(clip, ops, out, 0)).toEqual(frameWindow(clip, ops, out, total - 1));
    // the forward half behaves exactly as without bounce
    const plain = defaultOps(clip);
    expect(frameWindow(clip, ops, out, 7)).toEqual(frameWindow(clip, plain, out, 7));
  });
});

// WEB-5: crop mode's still request stops before reverse/bounce (buildOps
// cropPreview), so the Preview folds the scrubber frame back onto the
// forward, un-reversed grid before asking for the still — otherwise the
// mirrored half of a bounced clip clamps to the clip end server-side.
describe('forwardFrame (crop-mode still folding)', () => {
  const out = defaultOutput();

  it('is the identity (clamped) without reverse/bounce', () => {
    const ops = defaultOps(clip); // 50 plan frames
    expect(forwardFrame(clip, ops, out, 0)).toBe(0);
    expect(forwardFrame(clip, ops, out, 37)).toBe(37);
    expect(forwardFrame(clip, ops, out, 999)).toBe(49);
    expect(forwardFrame(clip, ops, out, -3)).toBe(0);
  });

  it('folds a bounced plan’s mirrored half onto its forward twin (N+k → N−1−k)', () => {
    const ops = defaultOps(clip);
    ops.bounce = true; // 100 plan frames, forward N = 50
    expect(forwardFrame(clip, ops, out, 3)).toBe(3);
    expect(forwardFrame(clip, ops, out, 50)).toBe(49); // first mirrored frame = last forward
    expect(forwardFrame(clip, ops, out, 99)).toBe(0);
    expect(forwardFrame(clip, ops, out, 96)).toBe(3);
  });

  it('flips a reversed plan (output frame i shows forward frame N−1−i)', () => {
    const ops = defaultOps(clip);
    ops.reverse = true;
    expect(forwardFrame(clip, ops, out, 0)).toBe(49);
    expect(forwardFrame(clip, ops, out, 49)).toBe(0);
  });

  it('composes reverse before bounce as the graph renders it: [rev(F), F]', () => {
    const ops = defaultOps(clip);
    ops.reverse = true;
    ops.bounce = true; // 100 plan frames
    expect(forwardFrame(clip, ops, out, 0)).toBe(49); // starts on the LAST forward frame
    expect(forwardFrame(clip, ops, out, 49)).toBe(0);
    expect(forwardFrame(clip, ops, out, 50)).toBe(0); // the mirrored half plays forward
    expect(forwardFrame(clip, ops, out, 99)).toBe(49);
  });

  it('leaves a still source at frame 0', () => {
    const ops = defaultOps(still);
    ops.bounce = true;
    ops.reverse = true;
    expect(forwardFrame(still, ops, out, 0)).toBe(0);
  });
});
