import { describe, expect, it } from 'vitest';
import { extOf, isSequenceFrame, naturalCompare, planDrop, sequenceDelayOverride, sequenceFps } from './files';

const f = (name: string, type = '') => ({ name, type });

describe('planDrop', () => {
  it('handles nothing / one file', () => {
    expect(planDrop(null)).toEqual({ kind: 'none' });
    expect(planDrop([])).toEqual({ kind: 'none' });
    expect(planDrop([f('clip.mov', 'video/quicktime')])).toEqual({ kind: 'single', file: f('clip.mov', 'video/quicktime') });
  });

  it('turns several images into one naturally sorted sequence', () => {
    const plan = planDrop([f('frame10.png', 'image/png'), f('frame2.png', 'image/png'), f('Frame1.PNG', '')]);
    expect(plan.kind).toBe('sequence');
    if (plan.kind === 'sequence') expect(plan.files.map((x) => x.name)).toEqual(['Frame1.PNG', 'frame2.png', 'frame10.png']);
    expect(plan.note).toBeUndefined();
  });

  it('falls back to the first file when the set is not all images', () => {
    const plan = planDrop([f('a.mov', 'video/quicktime'), f('b.png', 'image/png')]);
    expect(plan.kind).toBe('single');
    if (plan.kind === 'single') expect(plan.file.name).toBe('a.mov');
    expect(plan.note).toMatch(/first of 2 files/);
  });

  // SRV-1: the server rejects gif/avif sequence frames with a 400, so a set
  // of them must never be offered as a sequence (each uploads fine alone).
  it('never plans gif/avif files as a sequence', () => {
    const plan = planDrop([f('a1.gif', 'image/gif'), f('a2.gif', 'image/gif')]);
    expect(plan.kind).toBe('single');
    if (plan.kind === 'single') expect(plan.file.name).toBe('a1.gif');
    expect(plan.note).toMatch(/png \/ jpeg \/ webp \/ bmp \/ tiff/);
    expect(planDrop([f('b1.avif', 'image/avif'), f('b2.avif', 'image/avif')]).kind).toBe('single');
  });
});

describe('helpers', () => {
  it('isSequenceFrame accepts png/jpeg/webp/bmp/tiff by extension or MIME type, never gif/avif', () => {
    expect(isSequenceFrame(f('x.bin', 'image/webp'))).toBe(true);
    expect(isSequenceFrame(f('x.JPG'))).toBe(true);
    expect(isSequenceFrame(f('x.bmp', 'image/bmp'))).toBe(true);
    expect(isSequenceFrame(f('x.tif'))).toBe(true);
    expect(isSequenceFrame(f('x.gif', 'image/gif'))).toBe(false);
    expect(isSequenceFrame(f('x.avif', 'image/avif'))).toBe(false);
    expect(isSequenceFrame(f('x.mov', 'video/quicktime'))).toBe(false);
    expect(isSequenceFrame(f('noext'))).toBe(false);
    expect(extOf('a.b.PNG')).toBe('png');
    expect(extOf('none')).toBe('');
  });
  it('naturalCompare orders numeric chunks by value', () => {
    expect(['f10', 'f9', 'f1'].sort(naturalCompare)).toEqual(['f1', 'f9', 'f10']);
    expect(['b', 'A', 'a'].sort(naturalCompare)[2]).toBe('b'); // case-insensitive
  });
  it('sequenceFps inverts the delay', () => {
    expect(sequenceFps(100)).toBe(10);
    expect(sequenceFps(40)).toBe(25);
    expect(sequenceFps(0)).toBe(0);
  });
});

describe('sequenceDelayOverride', () => {
  // The store dedupes identical frame sets: a re-upload comes back with the
  // FIRST stored delay, so a different requested delay must become a "delay" op.
  it('returns the requested delay when the deduped sequence came back with another one', () => {
    expect(sequenceDelayOverride({ delayMs: 100 }, 12, 40)).toBe(40);
    expect(sequenceDelayOverride({ delayMs: 40 }, 2, 100)).toBe(100);
  });

  it('returns 0 when the stored delay already matches, for single files, and without sequence info', () => {
    expect(sequenceDelayOverride({ delayMs: 100 }, 12, 100)).toBe(0);
    expect(sequenceDelayOverride({ delayMs: 100 }, 1, 40)).toBe(0); // one file is never a sequence
    expect(sequenceDelayOverride(null, 12, 40)).toBe(0);
    expect(sequenceDelayOverride(undefined, 12, 40)).toBe(0);
  });

  it('rounds and clamps to the delay op range (1..60000) before comparing', () => {
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 99.6)).toBe(0); // rounds to the stored 100
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 40.4)).toBe(40);
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 0.2)).toBe(1);
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 99_999)).toBe(60000);
    expect(sequenceDelayOverride({ delayMs: 60000 }, 3, 99_999)).toBe(0); // both clamp to the cap
  });
});
