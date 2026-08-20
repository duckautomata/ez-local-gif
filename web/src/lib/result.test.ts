import { describe, expect, it } from 'vitest';
import type { ResultFile } from './api';
import { chatSizes, descLine, groupFiles, isFramesResult, isImageFormat, sizeState } from './result';

function file(name: string, extra: Partial<ResultFile> = {}): ResultFile {
  return {
    name,
    url: `/out/h/${name}`,
    format: name.split('.').pop() ?? '',
    bytes: 1000,
    width: 128,
    height: 128,
    frames: 10,
    fps: 25,
    duration: 0.4,
    limit: 0,
    ...extra,
  };
}

describe('groupFiles', () => {
  it('separates primary, alternatives, frames and archive', () => {
    const g = groupFiles([
      file('alt2.gif', { kind: 'alternative', index: 2, desc: 'fit at 12.5 fps · 64 colours' }),
      file('out.gif', { kind: 'output', desc: 'fit at 20 fps · 128 colours · lossy 60' }),
      file('alt1.gif', { kind: 'alternative', index: 1, desc: 'fit at 16.7 fps · 128 colours' }),
      file('frames.zip', { kind: 'archive', format: 'zip' }),
      file('f00002.png', { kind: 'frame', index: 2, desc: 'frame 2 (0.04 s)' }),
      file('f00001.png', { kind: 'frame', index: 1, desc: 'frame 1 (0.00 s)' }),
    ]);
    expect(g.primary?.name).toBe('out.gif');
    expect(g.alternatives.map((f) => f.name)).toEqual(['alt1.gif', 'alt2.gif']);
    expect(g.frames.map((f) => f.index)).toEqual([1, 2]);
    expect(g.archive?.name).toBe('frames.zip');
    expect(g.others).toEqual([]);
    expect(isFramesResult(g)).toBe(false);
    expect(descLine(g.primary, true)).toEqual({ label: 'Fit', text: 'fit at 20 fps · 128 colours · lossy 60' });
  });

  it('treats a missing kind as the primary (Phase 1 manifests)', () => {
    const g = groupFiles([file('out.webp')]);
    expect(g.primary?.name).toBe('out.webp');
    expect(descLine(g.primary, true)).toBeNull();
    expect(descLine(g.primary, false)).toBeNull();
  });

  it('keeps at most two alternatives, by rank', () => {
    const g = groupFiles([
      file('out.gif', { kind: 'output' }),
      file('a3.gif', { kind: 'alternative', index: 3 }),
      file('a1.gif', { kind: 'alternative', index: 1 }),
      file('a2.gif', { kind: 'alternative', index: 2 }),
    ]);
    expect(g.alternatives.map((f) => f.name)).toEqual(['a1.gif', 'a2.gif']);
  });

  it('recognises a frames-only result and never drops unknown kinds', () => {
    const g = groupFiles([
      file('frames.zip', { kind: 'archive', format: 'zip' }),
      file('f00001.png', { kind: 'frame', index: 1 }),
      file('weird.bin', { kind: 'something-new' }),
      file('out2.gif', { kind: 'output' }),
      file('out1.gif', { kind: '' }),
    ]);
    expect(g.primary?.name).toBe('out2.gif'); // first primary wins
    expect(g.others.map((f) => f.name)).toEqual(['weird.bin', 'out1.gif']);
    const framesOnly = groupFiles([file('frames.zip', { kind: 'archive' }), file('f1.png', { kind: 'frame', index: 1 })]);
    expect(isFramesResult(framesOnly)).toBe(true);
    expect(groupFiles(null).primary).toBeNull();
    expect(isFramesResult(groupFiles([]))).toBe(false);
  });
});

describe('descLine', () => {
  // W8: only a recipe that actually carried fitBytes > 0 gets the bold 'Fit:'
  // label; the Optimize preset's descs are plain settings.
  it('labels a non-fit desc (Optimize) as Settings, never Fit', () => {
    const f = file('out.gif', { kind: 'output', desc: 'gifsicle: lossy 30 · 256 colours' });
    expect(descLine(f, false)).toEqual({ label: 'Settings', text: 'gifsicle: lossy 30 · 256 colours' });
    expect(descLine(file('out.gif', { desc: 'gifsicle -O2 (lossless)' }), false)?.label).toBe('Settings');
  });
  it('labels a fit-search desc as Fit (including the cannot-fit report)', () => {
    expect(descLine(file('out.gif', { desc: 'fit at 20 fps · 128 colours' }), true)?.label).toBe('Fit');
    expect(descLine(file('out.gif', { desc: 'cannot fit under 256 KiB: smallest attempt is 300 KiB' }), true)?.label).toBe('Fit');
  });
  it('is null without a desc or file', () => {
    expect(descLine(file('out.gif', { desc: '  ' }), true)).toBeNull();
    expect(descLine(null, true)).toBeNull();
  });
});

describe('sizeState / chatSizes / isImageFormat', () => {
  it('classifies against the limit', () => {
    expect(sizeState({ bytes: 100, limit: 0 })).toBe('');
    expect(sizeState({ bytes: 100, limit: 100 })).toBe('ok');
    expect(sizeState({ bytes: 101, limit: 100 })).toBe('bad');
  });
  it('knows the in-chat sizes per target', () => {
    expect(chatSizes('emote').map((s) => s.px)).toEqual([22, 48]);
    expect(chatSizes('sticker').map((s) => s.px)).toEqual([160]);
    expect(chatSizes('attachment')).toEqual([]);
    expect(chatSizes('')).toEqual([]);
    expect(chatSizes(undefined)).toEqual([]);
  });
  it('previews every encoder output format with <img>', () => {
    for (const f of ['gif', 'webp', 'apng', 'avif', 'png', 'jpeg']) expect(isImageFormat(f), f).toBe(true);
    expect(isImageFormat('zip')).toBe(false);
    expect(isImageFormat('frames')).toBe(false);
  });
});
