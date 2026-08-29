// Phase 3 review fixes: the Result backdrop in app.ui (R1), the crop
// ratio-lock UI state (R2) and the feather op's place in buildOps (R4).
import { describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from './api';
import {
  app,
  buildOps,
  centerSquareCrop,
  defaultBackground,
  defaultOps,
  resetApp,
  setCropRatioLock,
  setSource,
} from './state.svelte';

const gifInfo: ProbeInfo = {
  format: 'gif',
  codec: 'gif',
  pixFmt: 'bgra',
  bits: 8,
  width: 160,
  height: 120,
  fps: 25,
  duration: 2,
  frames: 50,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'animation',
  premultiplied: false,
};
const src = (hash = 'a'.repeat(64)): Source => ({ hash, name: 'x.gif', size: 1, info: gifInfo });

describe('feather op (review R4)', () => {
  it('defaults off with radius 3 and emits nothing', () => {
    const ops = defaultOps(gifInfo);
    expect(ops.feather).toEqual({ enabled: false, radius: 3 });
    expect(buildOps(ops)).toEqual([]);
  });

  it('emits {kind:"feather", params:{radius}} after the keying op and before crop/autocrop', () => {
    const ops = defaultOps(gifInfo);
    ops.feather = { enabled: true, radius: 3 };
    expect(buildOps(ops)).toEqual([{ kind: 'feather', params: { radius: 3 } }]);
    ops.background = { ...defaultBackground(), enabled: true };
    ops.crop = { enabled: true, x: 1, y: 2, w: 30, h: 40 };
    ops.resize = { enabled: true, width: 128, height: 0, fit: 'contain' };
    ops.flipRotate = { enabled: true, horizontal: true, vertical: false, degrees: 90 };
    expect(buildOps(ops).map((o) => o.kind)).toEqual(['chromakey', 'feather', 'crop', 'resize', 'flip', 'rotate']);
    // after a colorkey too, and before autocrop
    ops.background = { ...defaultBackground(), enabled: true, mode: 'pick', pickColor: '313338' };
    ops.autocrop = { enabled: true, padding: 0, threshold: 1 };
    expect(buildOps(ops).map((o) => o.kind)).toEqual(['colorkey', 'feather', 'autocrop', 'resize', 'flip', 'rotate']);
  });

  it('is part of the crop preview (it precedes the geometry) and survives keyPreview', () => {
    const ops = defaultOps(gifInfo);
    ops.feather = { enabled: true, radius: 2.5 };
    ops.background = { ...defaultBackground(), enabled: true };
    ops.crop = { enabled: true, x: 0, y: 0, w: 10, h: 10 };
    expect(buildOps(ops, { cropPreview: true })).toEqual([{ kind: 'chromakey' }, { kind: 'feather', params: { radius: 2.5 } }]);
    // the eyedropper preview drops only the key, not the feather
    expect(buildOps(ops, { keyPreview: true }).map((o) => o.kind)).toEqual(['feather', 'crop']);
  });

  it('clamps the radius into the recipe range 0.1..50 and skips a zero radius', () => {
    const ops = defaultOps(gifInfo);
    ops.feather = { enabled: true, radius: 0.05 };
    expect(buildOps(ops)).toEqual([{ kind: 'feather', params: { radius: 0.1 } }]);
    ops.feather.radius = 99;
    expect(buildOps(ops)).toEqual([{ kind: 'feather', params: { radius: 50 } }]);
    ops.feather.radius = 0;
    expect(buildOps(ops)).toEqual([]);
  });

  it('a new source and resetApp clear it', () => {
    setSource(src());
    app.ops.feather = { enabled: true, radius: 7 };
    setSource(src('b'.repeat(64)));
    expect(app.ops.feather).toEqual({ enabled: false, radius: 3 });
    app.ops.feather = { enabled: true, radius: 7 };
    resetApp();
    expect(app.ops.feather).toEqual({ enabled: false, radius: 3 });
  });
});

describe('Result backdrop (review R1)', () => {
  it('defaults to Discord dark, independent of the preview backdrop', () => {
    resetApp();
    expect(app.ui.resultBackdrop).toBe('dark');
    expect(app.ui.backdrop).toBe('checker');
    app.ui.resultBackdrop = 'white';
    expect(app.ui.backdrop).toBe('checker');
  });

  it('survives a new source and resetApp (a viewing preference, like the preview backdrop)', () => {
    app.ui.resultBackdrop = 'checker';
    setSource(src('c'.repeat(64)));
    expect(app.ui.resultBackdrop).toBe('checker');
    resetApp();
    expect(app.ui.resultBackdrop).toBe('checker');
    app.ui.resultBackdrop = 'dark';
  });
});

describe('crop ratio lock (review R2)', () => {
  it('toggling on captures the current box ratio; off stops constraining', () => {
    setSource(src('d'.repeat(64)));
    expect(app.ui.cropRatio).toBe(0);
    // the default rectangle is the full frame
    setCropRatioLock(true);
    expect(app.ui.cropRatio).toBe(160 / 120);
    setCropRatioLock(false);
    expect(app.ui.cropRatio).toBe(0);
    app.ops.crop = { enabled: true, x: 0, y: 0, w: 90, h: 30 };
    setCropRatioLock(true);
    expect(app.ui.cropRatio).toBe(3);
    // a degenerate box falls back to 1:1
    app.ops.crop = { enabled: false, x: 0, y: 0, w: 0, h: 0 };
    setCropRatioLock(true);
    expect(app.ui.cropRatio).toBe(1);
  });

  it('lives in UI state only: the crop op payload is unchanged by the lock', () => {
    setSource(src('e'.repeat(64)));
    app.ops.crop = { enabled: true, x: 4, y: 8, w: 80, h: 60 };
    const before = buildOps(app.ops);
    setCropRatioLock(true);
    expect(buildOps(app.ops)).toEqual(before);
    expect(buildOps(app.ops)).toEqual([{ kind: 'crop', params: { x: 4, y: 8, w: 80, h: 60 } }]);
  });

  it('Centre square re-captures the ratio as 1:1 while the lock is on', () => {
    setSource(src('f'.repeat(64)));
    app.ops.crop = { enabled: true, x: 0, y: 0, w: 160, h: 80 };
    setCropRatioLock(true);
    expect(app.ui.cropRatio).toBe(2);
    centerSquareCrop(160, 120);
    expect(app.ops.crop).toEqual({ enabled: true, x: 20, y: 0, w: 120, h: 120 });
    expect(app.ui.cropRatio).toBe(1);
    // with the lock off, Centre square leaves the (absent) lock alone
    setCropRatioLock(false);
    centerSquareCrop(160, 120);
    expect(app.ui.cropRatio).toBe(0);
  });

  it('a new source clears the lock (the ratio belonged to the old rectangle)', () => {
    setSource(src('1'.repeat(64)));
    setCropRatioLock(true);
    expect(app.ui.cropRatio).toBeGreaterThan(0);
    setSource(src('2'.repeat(64)));
    expect(app.ui.cropRatio).toBe(0);
  });
});
