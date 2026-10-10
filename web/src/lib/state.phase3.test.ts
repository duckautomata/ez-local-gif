// Phase 3 op stack: buildOps ordering with the editing ops, recipe.sources
// bookkeeping for overlay assets, the preview-mode options, and (Phase 5a)
// the Background card's Color / Screen ops, the morph (Edges) cleanup and
// the Screen key color's pick / auto-match.
import { describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from './api';
import { newImageOverlay, newTextOverlay, scrubberRange } from './overlay';
import { defaultOutput, presetById } from './presets';
import {
  addKeyColor,
  addOverlay,
  app,
  applyPickedColor,
  armEyedropper,
  assetHashes,
  backgroundOps,
  buildOps,
  buildOutput,
  applyScreenSample,
  armScreenEyedropper,
  CHROMA_DEFAULTS,
  COLORKEY_DEFAULTS,
  cropActive,
  defaultBackground,
  defaultOps,
  hasOverlays,
  MAX_KEY_COLORS,
  MORPH_DEFAULTS,
  MORPH_MAX_GROW,
  MORPH_MAX_SMOOTH,
  morphOp,
  moveOverlay,
  planCanvas,
  previewOutput,
  recipeOps,
  recipeSources,
  removeKeyColor,
  removeOverlay,
  resetApp,
  setBackgroundMode,
  setChromaColor,
  setKeyColor,
  setScreenColor,
  setSource,
} from './state.svelte';

const gifInfo: ProbeInfo = {
  format: 'gif',
  codec: 'gif',
  pixFmt: 'bgra',
  bits: 8,
  width: 64,
  height: 64,
  fps: 25,
  duration: 2,
  frames: 50,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'animation',
  premultiplied: false,
};
const MAIN = 'a'.repeat(64);
const asset = (hash: string, name = 'ov.png'): Source => ({ hash, name, size: 1, info: { ...gifInfo, width: 20, height: 10, frames: 1, fps: 0, duration: 0, isStill: true, kind: 'image' } });
const animAsset = (hash: string): Source => ({ hash, name: 'ov.gif', size: 1, info: { ...gifInfo, width: 20, height: 10 } });

describe('buildOps — Phase 3 ordering', () => {
  it('serialises every op in the documented order', () => {
    const ops = defaultOps(gifInfo);
    ops.unpremultiply = true;
    ops.trim = { enabled: true, start: 0.5, end: 0 };
    ops.speed = { enabled: true, factor: 2 };
    ops.fps = { enabled: true, fps: 20 };
    ops.background = { ...defaultBackground(), enabled: true, mode: 'screen' };
    ops.crop = { enabled: true, x: 1, y: 2, w: 30, h: 40 };
    ops.resize = { enabled: true, width: 128, height: 0, fit: 'contain' };
    ops.flipRotate = { enabled: true, horizontal: true, vertical: false, degrees: 90 };
    ops.reverse = true;
    const t = newTextOverlay(1);
    t.text = 'hi';
    const im = newImageOverlay(2);
    im.asset = asset('b'.repeat(64));
    im.x = 5;
    im.y = 6;
    ops.overlays = [t, im];
    expect(buildOps(ops).map((o) => o.kind)).toEqual([
      'unpremultiply',
      'trim',
      'speed',
      'fps',
      'chromakey',
      'morph',
      'crop',
      'resize',
      'flip',
      'rotate',
      'reverse',
      'text',
      'overlay',
    ]);
    // the overlay ops keep the user's order and index the assets
    ops.overlays = [im, t];
    const out = buildOps(ops);
    expect(out.slice(-2)).toEqual([
      { kind: 'overlay', params: { source: 1, x: 5, y: 6 } },
      { kind: 'text', params: { text: 'hi', x: 0, y: 0, border: 2 } },
    ]);
  });

  it('autocrop replaces the manual crop and carries only non-default params', () => {
    const ops = defaultOps(gifInfo);
    ops.crop = { enabled: true, x: 1, y: 2, w: 30, h: 40 };
    ops.autocrop = { enabled: true, padding: 0, threshold: 1 };
    expect(buildOps(ops)).toEqual([{ kind: 'autocrop' }]);
    ops.autocrop = { enabled: true, padding: 4, threshold: 128 };
    expect(buildOps(ops)).toEqual([{ kind: 'autocrop', params: { threshold: 128, padding: 4 } }]);
    ops.autocrop.padding = 5000; // clamped to the recipe's range
    ops.autocrop.threshold = 999;
    expect(buildOps(ops)).toEqual([{ kind: 'autocrop', params: { threshold: 255, padding: 1024 } }]);
    expect(cropActive(ops)).toBe(true);
    ops.autocrop.enabled = false;
    expect(buildOps(ops)).toEqual([{ kind: 'crop', params: { x: 1, y: 2, w: 30, h: 40 } }]);
    ops.crop.enabled = false;
    expect(cropActive(ops)).toBe(false);
    // the crop preview stops before crop / autocrop but after the key op and its morph
    ops.autocrop.enabled = true;
    ops.background = { ...defaultBackground(), enabled: true, mode: 'screen', screen: 'blue', color: '0000ff' };
    expect(buildOps(ops, { cropPreview: true })).toEqual([{ kind: 'chromakey', params: { color: '0000ff' } }, { kind: 'morph', params: { close: true } }]);
  });

  it('reverse is emitted after the geometry and only when set', () => {
    const ops = defaultOps(gifInfo);
    expect(buildOps(ops)).toEqual([]);
    ops.reverse = true;
    expect(buildOps(ops)).toEqual([{ kind: 'reverse' }]);
    ops.flipRotate = { enabled: true, horizontal: true, vertical: false, degrees: 0 };
    expect(buildOps(ops).map((o) => o.kind)).toEqual(['flip', 'reverse']);
  });
});

describe('background ops (Phase 5a: Color / Screen, then morph)', () => {
  it('defaults mirror the recipe zero values of graph/phase3.go (Screen 0.1, Color 0.08) and a new session lands on Color with fill pinholes on', () => {
    expect(CHROMA_DEFAULTS).toEqual({ similarity: 0.1, blend: 0.05, despillMix: 0.5, despillExpand: 0 });
    expect(COLORKEY_DEFAULTS).toEqual({ similarity: 0.08, blend: 0 });
    const b = defaultBackground();
    expect(b).toMatchObject({ enabled: false, mode: 'color', screen: 'green', color: '00ff00', similarity: 0.1, blend: 0.05, pickSimilarity: 0.08, pickBlend: 0 });
    expect(b.colors).toEqual(['']); // one row waiting for its pick
    expect(b.morph).toEqual({ close: true, grow: 0, smooth: 0 });
    expect(MORPH_DEFAULTS).toEqual({ close: true, grow: 0, smooth: 0 });
    expect(defaultOps(gifInfo).background).toEqual(b);
  });

  it('Screen with defaults is a bare chromakey (0.1 is now the zero value) plus the default morph; changed knobs are carried', () => {
    const b = { ...defaultBackground(), enabled: true, mode: 'screen' as const };
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey' }, { kind: 'morph', params: { close: true } }]);
    b.morph = { close: false, grow: 0, smooth: 0 }; // the key alone from here on
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey' }]);
    b.similarity = 0.2; // the pre-5a default is a real value now
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey', params: { similarity: 0.2 } }]);
    b.similarity = 0.35;
    b.blend = 0.1;
    b.despillMix = 0.8;
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey', params: { similarity: 0.35, blend: 0.1, despillMix: 0.8 } }]);
    b.despill = false;
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey', params: { similarity: 0.35, blend: 0.1, despillOff: true } }]);
    b.screen = 'blue';
    b.color = '0000ff';
    b.similarity = 0.1;
    b.blend = 0;
    b.despill = true;
    b.despillMix = 0.5;
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey', params: { color: '0000ff' } }]);
    // the old despill defaults (0.6 / 0.3) are real values now; expand 0 is the default
    b.despillMix = 0.6;
    b.despillExpand = 0.3;
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey', params: { color: '0000ff', despillMix: 0.6, despillExpand: 0.3 } }]);
    b.despillMix = 0.5;
    b.despillExpand = 0;
    b.enabled = false;
    expect(backgroundOps(b)).toEqual([]);
  });

  it('Color is one colorkey per picked row, in row order, sharing similarity / blend (0.08 / 0 left out); empty rows and repeats emit nothing', () => {
    const b = { ...defaultBackground(), enabled: true, morph: { close: false, grow: 0, smooth: 0 } };
    expect(backgroundOps(b)).toEqual([]); // nothing picked yet
    b.colors = ['313338'];
    expect(backgroundOps(b)).toEqual([{ kind: 'colorkey', params: { color: '313338' } }]);
    b.colors = ['313338', '', '1e3a8a', '313338', 'facc82'];
    expect(backgroundOps(b)).toEqual([
      { kind: 'colorkey', params: { color: '313338' } },
      { kind: 'colorkey', params: { color: '1e3a8a' } },
      { kind: 'colorkey', params: { color: 'facc82' } },
    ]);
    b.pickSimilarity = 0.25;
    b.pickBlend = 0.05;
    expect(backgroundOps(b)).toEqual([
      { kind: 'colorkey', params: { color: '313338', similarity: 0.25, blend: 0.05 } },
      { kind: 'colorkey', params: { color: '1e3a8a', similarity: 0.25, blend: 0.05 } },
      { kind: 'colorkey', params: { color: 'facc82', similarity: 0.25, blend: 0.05 } },
    ]);
    b.pickSimilarity = 0.1; // the pre-5a default is carried now
    b.pickBlend = 0;
    expect(backgroundOps(b)[0]).toEqual({ kind: 'colorkey', params: { color: '313338', similarity: 0.1 } });
    b.enabled = false;
    expect(backgroundOps(b)).toEqual([]);
  });

  it('morph follows the keys: close / signed grow / smooth with the zero values left out and clamped, none without a key and none when all are off', () => {
    expect(MORPH_MAX_GROW).toBe(20);
    expect(MORPH_MAX_SMOOTH).toBe(10);
    const b = { ...defaultBackground(), enabled: true, colors: ['313338'] };
    expect(backgroundOps(b)).toEqual([{ kind: 'colorkey', params: { color: '313338' } }, { kind: 'morph', params: { close: true } }]);
    b.morph = { close: true, grow: 2, smooth: 0 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { close: true, grow: 2 } });
    b.morph = { close: false, grow: 1, smooth: 0 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { grow: 1 } });
    // Shift edge is signed: negative trims a leftover rim of background
    b.morph = { close: false, grow: -2, smooth: 0 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { grow: -2 } });
    b.morph = { close: true, grow: -2.6, smooth: 0 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { close: true, grow: -3 } });
    b.morph = { close: false, grow: 25, smooth: 0 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { grow: 20 } });
    b.morph = { close: false, grow: -99, smooth: 0 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { grow: -20 } });
    b.morph = { close: true, grow: 2.6, smooth: 0 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { close: true, grow: 3 } });
    // Soft edge: > 0 only, rounded, clamped to 0..10
    b.morph = { close: false, grow: 0, smooth: 1.5 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { smooth: 1.5 } });
    b.morph = { close: true, grow: -2, smooth: 1.23456 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { close: true, grow: -2, smooth: 1.235 } });
    b.morph = { close: false, grow: 0, smooth: 42 };
    expect(backgroundOps(b)[1]).toEqual({ kind: 'morph', params: { smooth: 10 } });
    // nothing to do: no morph op at all (a bare morph is a compile error server-side)
    b.morph = { close: false, grow: 0.4, smooth: -1 };
    expect(backgroundOps(b)).toEqual([{ kind: 'colorkey', params: { color: '313338' } }]);
    b.morph = { close: false, grow: -0.4, smooth: 0 };
    expect(morphOp(b.morph)).toBeNull(); // rounds to 0 (never −0)
    b.morph = { close: false, grow: 0, smooth: 0 };
    expect(backgroundOps(b)).toEqual([{ kind: 'colorkey', params: { color: '313338' } }]);
    // no key, no morph — the cleanup is the key's, never the source alpha's
    b.morph = { close: true, grow: 4, smooth: 2 };
    b.colors = [''];
    expect(backgroundOps(b)).toEqual([]);
    // Screen: after the chromakey
    b.mode = 'screen';
    expect(backgroundOps(b)).toEqual([{ kind: 'chromakey' }, { kind: 'morph', params: { close: true, grow: 4, smooth: 2 } }]);
    // the card off: nothing at all
    b.enabled = false;
    expect(backgroundOps(b)).toEqual([]);
  });

  it('buildOps: keys → morph → feather, before the geometry; keyPreview drops the card’s ops (keys and morph), the crop preview keeps them', () => {
    const ops = defaultOps(gifInfo);
    ops.background = { ...defaultBackground(), enabled: true, colors: ['313338', 'facc82'], morph: { close: true, grow: 1, smooth: 0 } };
    ops.feather = { enabled: true, radius: 2 };
    ops.crop = { enabled: true, x: 0, y: 0, w: 10, h: 10 };
    ops.reverse = true;
    expect(buildOps(ops).map((o) => o.kind)).toEqual(['colorkey', 'colorkey', 'morph', 'feather', 'crop', 'reverse']);
    expect(buildOps(ops, { keyPreview: true }).map((o) => o.kind)).toEqual(['feather', 'crop', 'reverse']);
    expect(buildOps(ops, { cropPreview: true }).map((o) => o.kind)).toEqual(['colorkey', 'colorkey', 'morph', 'feather']);
    ops.background.mode = 'screen';
    expect(buildOps(ops).map((o) => o.kind)).toEqual(['chromakey', 'morph', 'feather', 'crop', 'reverse']);
  });
});

describe('Color rows and the eyedropper (app helpers)', () => {
  it('a pick lands in the armed row; "+ add color" rows; typed hex; remove keeps the armed row in step; the cap', () => {
    setSource({ hash: MAIN, name: 'x.gif', size: 1, info: gifInfo });
    expect(app.ops.background.colors).toEqual(['']);
    expect(app.ui.pickRow).toBe(0);
    armEyedropper(0);
    expect(app.ui.pickColor).toBe(true);
    applyPickedColor('313338');
    expect(app.ops.background).toMatchObject({ enabled: true, mode: 'color', colors: ['313338'] });
    expect(app.ui.pickColor).toBe(false);
    // "+ add color": a new empty row (the card arms the eyedropper for it)
    expect(addKeyColor()).toBe(1);
    expect(app.ops.background.colors).toEqual(['313338', '']);
    armEyedropper(1);
    expect(app.ui.pickRow).toBe(1);
    applyPickedColor('#FACC82'); // what the card may hand over; the preview sends plain lowercase
    expect(app.ops.background.colors).toEqual(['313338', 'facc82']);
    expect(app.ui.pickColor).toBe(false);
    // a row that is gone by the time the click lands: the pick is appended, never lost
    armEyedropper(5);
    applyPickedColor('1e3a8a');
    expect(app.ops.background.colors).toEqual(['313338', 'facc82', '1e3a8a']);
    // re-picking the same color still disarms; a malformed pick changes nothing and stays armed
    armEyedropper(0);
    applyPickedColor('313338');
    expect(app.ui.pickColor).toBe(false);
    armEyedropper(0);
    applyPickedColor('nope');
    expect(app.ops.background.colors).toEqual(['313338', 'facc82', '1e3a8a']);
    expect(app.ui.pickColor).toBe(true);
    // typed hex: '#' and case tolerated, malformed refused, '' clears; a valid value disarms that row's eyedropper
    expect(setKeyColor(0, '#ABCDEF')).toBe(true);
    expect(app.ops.background.colors[0]).toBe('abcdef');
    expect(app.ui.pickColor).toBe(false);
    expect(setKeyColor(1, 'zzz')).toBe(false);
    expect(app.ops.background.colors[1]).toBe('facc82');
    expect(setKeyColor(1, '  ')).toBe(true);
    expect(app.ops.background.colors[1]).toBe('');
    expect(setKeyColor(9, 'abcdef')).toBe(false);
    expect(backgroundOps(app.ops.background).map((o) => o.kind)).toEqual(['colorkey', 'colorkey', 'morph']);
    // remove: an eyedropper armed for a later row follows its row down…
    armEyedropper(2);
    removeKeyColor(0);
    expect(app.ops.background.colors).toEqual(['', '1e3a8a']);
    expect(app.ui.pickRow).toBe(1);
    expect(app.ui.pickColor).toBe(true);
    // …removing the armed row disarms…
    removeKeyColor(1);
    expect(app.ops.background.colors).toEqual(['']);
    expect(app.ui.pickColor).toBe(false);
    // …and the only row is cleared, never removed
    app.ops.background.colors = ['313338'];
    removeKeyColor(0);
    expect(app.ops.background.colors).toEqual(['']);
    removeKeyColor(4); // unknown: no-op
    expect(app.ops.background.colors).toEqual(['']);
    // the cap: six rows, no seventh; a stale row index then replaces the last row
    app.ops.background.colors = ['111111', '222222', '333333', '444444', '555555', '666666'];
    expect(MAX_KEY_COLORS).toBe(6);
    expect(addKeyColor()).toBe(-1);
    armEyedropper(9);
    applyPickedColor('abcdef');
    expect(app.ops.background.colors).toEqual(['111111', '222222', '333333', '444444', '555555', 'abcdef']);
    // a new source disarms and forgets the row
    armEyedropper(3);
    setSource({ hash: 'd'.repeat(64), name: 'y.gif', size: 1, info: gifInfo });
    expect(app.ui.pickColor).toBe(false);
    expect(app.ui.pickRow).toBe(0);
    expect(app.ops.background.colors).toEqual(['']);
    resetApp();
  });
});

describe('overlay ops and asset bookkeeping', () => {
  it('a time range taken from the scrubber reaches the recipe as stored — floored µs, not re-rounded up (review W1)', () => {
    const ops = defaultOps(gifInfo);
    const t = newTextOverlay(1);
    const w = scrubberRange(2, 30, 60); // 30 fps frame 3: 0.066666, not trimTime's 0.066667
    t.start = w.start;
    t.end = w.end;
    ops.overlays = [t];
    expect(buildOps(ops)[0].params).toMatchObject({ start: 0.066666, end: 0.1 });
    t.end = scrubberRange(13, 30, 60).end;
    expect(buildOps(ops)[0].params).toMatchObject({ start: 0.066666, end: 0.466666 });
  });

  it('text params: defaults are left out, set knobs carried, time range at µs precision', () => {
    const ops = defaultOps(gifInfo);
    const t = newTextOverlay(1);
    t.text = 'Hello\nworld';
    t.font = 'Noto Sans';
    t.size = 48;
    t.color = 'ff000080';
    t.border = 3;
    t.borderColor = '101010';
    t.box = true;
    t.boxColor = '00000040';
    t.boxPad = 4;
    t.anchor = 'bc';
    t.x = 64;
    t.y = 120;
    t.start = 0.0333333333;
    t.end = 1.5;
    ops.overlays = [t];
    expect(buildOps(ops)).toEqual([
      {
        kind: 'text',
        params: {
          text: 'Hello\nworld',
          x: 64,
          y: 120,
          font: 'Noto Sans',
          size: 48,
          color: 'ff000080',
          border: 3,
          borderColor: '101010',
          box: true,
          boxColor: '00000040',
          boxPad: 4,
          anchor: 'bc',
          start: 0.033333,
          end: 1.5,
        },
      },
    ]);
    t.border = 0; // no outline: its color is irrelevant and dropped
    t.box = false;
    expect(buildOps(ops)[0].params).not.toHaveProperty('borderColor');
    expect(buildOps(ops)[0].params).not.toHaveProperty('boxColor');
    t.text = '   ';
    expect(buildOps(ops)).toEqual([]); // blank text draws nothing
  });

  it('image params: natural size / opacity 1 / loop are the defaults; noLoop only for animated assets', () => {
    const ops = defaultOps(gifInfo);
    const im = newImageOverlay(1);
    im.asset = asset('b'.repeat(64));
    im.anchor = 'mc';
    im.x = 32;
    im.y = 32;
    ops.overlays = [im];
    expect(buildOps(ops)).toEqual([{ kind: 'overlay', params: { source: 1, x: 32, y: 32, anchor: 'mc' } }]);
    im.width = 40;
    im.opacity = 0.5;
    im.loop = false; // a still never loops: nothing to say
    expect(buildOps(ops)).toEqual([{ kind: 'overlay', params: { source: 1, x: 32, y: 32, width: 40, opacity: 0.5, anchor: 'mc' } }]);
    im.asset = animAsset('c'.repeat(64));
    expect(buildOps(ops)[0].params).toMatchObject({ noLoop: true });
    im.loop = true;
    expect(buildOps(ops)[0].params).not.toHaveProperty('noLoop');
  });

  it('sources = main + assets in first-use order, deduplicated; removing a card re-indexes the rest', () => {
    const B = 'b'.repeat(64);
    const C = 'c'.repeat(64);
    const ops = defaultOps(gifInfo);
    const i1 = newImageOverlay(1);
    i1.asset = asset(B);
    const t = newTextOverlay(2);
    const i2 = newImageOverlay(3);
    i2.asset = asset(C);
    const i3 = newImageOverlay(4);
    i3.asset = asset(B); // the same blob twice: one source entry
    const empty = newImageOverlay(5); // no asset yet: no op, no source
    ops.overlays = [i1, t, i2, i3, empty];
    expect(assetHashes(ops)).toEqual([B, C]);
    const out = defaultOutput();
    expect(recipeSources(MAIN, ops, out)).toEqual([MAIN, B, C]);
    const sources = (o: ReturnType<typeof buildOps>) => o.filter((x) => x.kind === 'overlay').map((x) => (x.params as { source: number }).source);
    expect(sources(buildOps(ops))).toEqual([1, 2, 1]);
    // remove the first card: C is now source 1
    ops.overlays = [t, i2, i3, empty];
    expect(assetHashes(ops)).toEqual([C, B]);
    expect(recipeSources(MAIN, ops, out)).toEqual([MAIN, C, B]);
    expect(sources(buildOps(ops))).toEqual([1, 2]);
    // a disabled card contributes neither an op nor a source
    i2.enabled = false;
    expect(recipeSources(MAIN, ops, out)).toEqual([MAIN, B]);
    expect(sources(buildOps(ops))).toEqual([1]);
    expect(hasOverlays(ops)).toBe(true);
    i3.enabled = false;
    t.enabled = false;
    expect(hasOverlays(ops)).toBe(false);
    expect(recipeSources(MAIN, ops, out)).toEqual([MAIN]);
  });

  it('Optimize sends no overlays and no assets', () => {
    const ops = defaultOps(gifInfo);
    const im = newImageOverlay(1);
    im.asset = asset('b'.repeat(64));
    ops.overlays = [im, newTextOverlay(2)];
    const out = defaultOutput();
    out.preset = 'optimize';
    expect(recipeOps(ops, out)).toEqual([]);
    expect(recipeSources(MAIN, ops, out)).toEqual([MAIN]);
    out.preset = 'chat';
    expect(recipeOps(ops, out)).toHaveLength(2);
    expect(recipeSources(MAIN, ops, out)).toHaveLength(2);
  });

  it('app helpers: add / move / remove keep ids stable and the selection in step', () => {
    setSource({ hash: MAIN, name: 'x.gif', size: 1, info: gifInfo });
    const a = addOverlay('text');
    const b = addOverlay('image');
    const c = addOverlay('text');
    expect(app.ops.overlays.map((o) => o.kind)).toEqual(['text', 'image', 'text']);
    expect(new Set([a.id, b.id, c.id]).size).toBe(3);
    expect(app.ui.selectedOverlay).toBe(c.id);
    moveOverlay(c.id, -1);
    expect(app.ops.overlays.map((o) => o.id)).toEqual([a.id, c.id, b.id]);
    moveOverlay(a.id, -1); // already first: clamped
    expect(app.ops.overlays.map((o) => o.id)).toEqual([a.id, c.id, b.id]);
    moveOverlay(a.id, 5); // clamped to the end
    expect(app.ops.overlays.map((o) => o.id)).toEqual([c.id, b.id, a.id]);
    removeOverlay(c.id);
    expect(app.ops.overlays.map((o) => o.id)).toEqual([b.id, a.id]);
    expect(app.ui.selectedOverlay).toBe(0);
    removeOverlay(999); // unknown: no-op
    expect(app.ops.overlays).toHaveLength(2);
    // a new source / reset starts with no overlays and a disarmed eyedropper (its row forgotten)
    app.ui.pickColor = true;
    app.ui.pickRow = 2;
    setSource({ hash: 'd'.repeat(64), name: 'y.gif', size: 1, info: gifInfo });
    expect(app.ops.overlays).toEqual([]);
    expect(app.ui.pickColor).toBe(false);
    expect(app.ui.pickRow).toBe(0);
    resetApp();
    expect(app.ops.background.enabled).toBe(false);
  });
});

describe('preview requests (still / proxy)', () => {
  it('previewOutput keeps the geometry subset the server memoises on — format, width, height, fit, fps (review W7)', () => {
    const out = defaultOutput();
    out.preset = 'emote';
    presetById('emote').apply(out);
    const key = JSON.stringify(previewOutput(buildOutput(out)));
    expect(previewOutput(buildOutput(out))).toEqual({ format: 'gif', width: 128, height: 128, fit: 'contain', fps: 25 });
    expect(buildOutput(out)).toMatchObject({ colors: 256, fitBytes: 262144, preset: 'emote', target: 'emote' });
    // none of the encoder / fit / preset knobs changes the preview key
    out.colors = 64;
    out.dither = 'none';
    out.lossy = 80;
    out.alphaThreshold = 200;
    out.matte = 'ffffff';
    out.fitKiB = 100;
    out.fitKeepSize = true;
    out.fitKeepFps = true;
    out.fitEnabled = false;
    out.preset = 'custom';
    out.target = '';
    out.loop = 3;
    expect(buildOutput(out)).toMatchObject({ colors: 64, lossy: 80, loop: 3, preset: 'custom' });
    expect(JSON.stringify(previewOutput(buildOutput(out)))).toBe(key);
    out.format = 'webp';
    out.quality = 50;
    out.lossless = true;
    expect(previewOutput(buildOutput(out))).toEqual({ format: 'webp', width: 128, height: 128, fit: 'contain', fps: 25 });
    // the geometry itself does
    out.width = 64;
    expect(previewOutput(buildOutput(out))).toEqual({ format: 'webp', width: 64, height: 128, fit: 'contain', fps: 25 });
    out.height = 0;
    out.fps = 0;
    expect(previewOutput(buildOutput(out))).toEqual({ format: 'webp', width: 64, fit: 'contain' });
    out.width = 0;
    expect(previewOutput(buildOutput(out))).toEqual({ format: 'webp' });
    // a chat GIF at the source size carries nothing but the format
    expect(previewOutput(buildOutput(defaultOutput()))).toEqual({ format: 'gif' });
  });

  it('planCanvas is the output canvas after crop / resize / rotation and the Output fit; unknown with auto-crop (review W6)', () => {
    const ops = defaultOps(gifInfo); // 64×64
    const out = defaultOutput(); // chat: as produced
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 64, h: 64 });
    expect(planCanvas(null, ops, out)).toBeNull();
    ops.crop = { enabled: true, x: 1, y: 2, w: 30, h: 40 };
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 30, h: 40 });
    ops.resize = { enabled: true, width: 128, height: 0, fit: 'contain' };
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 128, h: 171 });
    ops.flipRotate = { enabled: true, horizontal: false, vertical: false, degrees: 90 };
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 171, h: 128 });
    ops.flipRotate.degrees = 180;
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 128, h: 171 });
    // one output dimension scales keeping the aspect; both pin the canvas whatever the fit
    out.width = 32;
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 32, h: 43 });
    out.height = 32;
    for (const fit of ['contain', 'cover', 'exact'] as const) {
      out.fit = fit;
      expect(planCanvas(gifInfo, ops, out), fit).toEqual({ w: 32, h: 32 });
    }
    // the Emote preset: 128×128
    presetById('emote').apply(out);
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 128, h: 128 });
    // auto-crop: the server resolves the box, so the canvas is known only when both output dimensions pin it
    ops.autocrop.enabled = true;
    expect(planCanvas(gifInfo, ops, out)).toEqual({ w: 128, h: 128 });
    presetById('chat').apply(out);
    expect(planCanvas(gifInfo, ops, out)).toBeNull();
    out.width = 100;
    expect(planCanvas(gifInfo, ops, out)).toBeNull();
  });
});

describe('Screen key color: pick from preview and auto-match', () => {
  /** an RGBA still: `screen` pixels of one color and `other` pixels of another, all opaque */
  function still(screen: number, sc: [number, number, number], other: number, oc: [number, number, number]): Uint8ClampedArray {
    const d = new Uint8ClampedArray((screen + other) * 4);
    for (let i = 0; i < screen + other; i++) {
      const c = i < screen ? sc : oc;
      d.set([c[0], c[1], c[2], 255], i * 4);
    }
    return d;
  }

  it('entering Screen and pressing Green / Blue arm the auto-match (the key-free still) with a source and a preview; batch never does', () => {
    resetApp();
    // no source: nothing to sample, the preset stays
    setBackgroundMode('screen', { picker: true });
    expect(app.ui.pickColor).toBe(false);
    setSource({ hash: MAIN, name: 'x.gif', size: 1, info: gifInfo });
    setBackgroundMode('screen', { picker: true });
    expect(app.ops.background).toMatchObject({ enabled: true, mode: 'screen', screen: 'green', color: '00ff00' });
    expect(app.ui).toMatchObject({ pickColor: true, pickTarget: 'screen-auto' });
    // re-pressing the mode it is already in does not restart it; the eyedropper's key-free still is what the preview shows
    applyScreenSample(null); // unreadable still: disarmed, preset kept, no hint
    expect(app.ui.pickColor).toBe(false);
    expect(app.ui.screenMatch).toBeNull();
    setBackgroundMode('screen', { picker: true });
    expect(app.ui.pickColor).toBe(false);
    // Blue: the preset, then the auto-match finds the real screen
    setScreenColor('blue', { picker: true });
    expect(app.ops.background).toMatchObject({ screen: 'blue', color: '0000ff' });
    expect(app.ui).toMatchObject({ pickColor: true, pickTarget: 'screen-auto' });
    applyScreenSample(still(80, [20, 60, 200], 20, [200, 180, 150]));
    expect(app.ops.background.color).toBe('143cc8');
    expect(app.ui.pickColor).toBe(false);
    expect(app.ui.screenMatch).toEqual({ which: 'blue', color: '143cc8' });
    expect(backgroundOps(app.ops.background)[0]).toEqual({ kind: 'chromakey', params: { color: '143cc8' } });
    // a second sample without an armed auto-match changes nothing
    applyScreenSample(still(100, [0, 0, 120], 0, [0, 0, 0]));
    expect(app.ops.background.color).toBe('143cc8');
    // Green on a still without a green screen: the preset stays, the hint says so
    setScreenColor('green', { picker: true });
    applyScreenSample(still(100, [200, 180, 150], 0, [0, 0, 0]));
    expect(app.ops.background.color).toBe('00ff00');
    expect(app.ui.screenMatch).toEqual({ which: 'green', color: null });
    // batch (no preview): the preset only
    setScreenColor('blue', { picker: false });
    expect(app.ops.background.color).toBe('0000ff');
    expect(app.ui.pickColor).toBe(false);
    expect(app.ui.screenMatch).toBeNull();
    // leaving Screen for Color cancels a pending auto-match (and arms the row pick instead)
    setScreenColor('green', { picker: true });
    setBackgroundMode('color', { picker: true });
    expect(app.ui).toMatchObject({ pickColor: true, pickTarget: 'color', pickRow: 0 });
    // entering Screen again with a non-preset key color keeps it (no re-match)
    app.ops.background.color = '143cc8';
    setBackgroundMode('screen', { picker: true });
    expect(app.ui.pickColor).toBe(false);
    expect(app.ops.background.color).toBe('143cc8');
    resetApp();
  });

  it('Pick from preview sets the Screen key color (custom), keeps the sub-choice; a typed key color cancels a pending pick', () => {
    resetApp();
    setSource({ hash: MAIN, name: 'x.gif', size: 1, info: gifInfo });
    setBackgroundMode('screen', { picker: false });
    app.ops.background.screen = 'blue';
    app.ops.background.color = '0000ff';
    armScreenEyedropper();
    expect(app.ui).toMatchObject({ pickColor: true, pickTarget: 'screen' });
    applyPickedColor('#1A40C0');
    expect(app.ops.background).toMatchObject({ mode: 'screen', screen: 'blue', color: '1a40c0', enabled: true });
    expect(app.ops.background.colors).toEqual(['']); // the Color rows are untouched
    expect(app.ui.pickColor).toBe(false);
    expect(app.ui.screenMatch).toBeNull();
    armScreenEyedropper();
    expect(setChromaColor('zz')).toBe(false);
    expect(app.ui.pickColor).toBe(true);
    expect(setChromaColor('#22AA44')).toBe(true);
    expect(app.ops.background.color).toBe('22aa44');
    expect(app.ui.pickColor).toBe(false);
    // a new source forgets the match and the pick target
    app.ui.screenMatch = { which: 'blue', color: '143cc8' };
    app.ui.pickTarget = 'screen';
    setSource({ hash: 'd'.repeat(64), name: 'y.gif', size: 1, info: gifInfo });
    expect(app.ui.screenMatch).toBeNull();
    expect(app.ui.pickTarget).toBe('color');
    resetApp();
  });
});
