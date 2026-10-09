// Phase 5b op stack: the Background card's AI mode — the `matte` op (model
// omitted when it is the recipe default), its place before morph / feather,
// the mode setter, and the preview requests that carry it.
import { describe, expect, it } from 'vitest';
import { MATTE_MODEL_DEFAULT, type ProbeInfo, type Source } from './api';
import { defaultOutput, presetById } from './presets';
import {
  aiActive,
  app,
  armEyedropper,
  backgroundOps,
  buildOps,
  defaultBackground,
  defaultOps,
  recipeOps,
  resetApp,
  setBackgroundMode,
  setMatteModel,
  setSource,
  stillRequest,
  type BackgroundCfg,
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
  hasAlpha: false,
  hasAudio: false,
  isStill: false,
  kind: 'animation',
  premultiplied: false,
};
const src: Source = { hash: 'a'.repeat(64), name: 'x.gif', size: 1, info: gifInfo };

describe('AI mode ops (Phase 5b)', () => {
  it('a new session has no model chosen (the card fills the server default in); AI is one of the modes', () => {
    expect(defaultBackground().ai).toEqual({ model: '' });
    expect(defaultBackground().mode).toBe('colour'); // enabling still lands on Colour: AI never starts a pass silently
    expect(MATTE_MODEL_DEFAULT).toBe('isnet-anime');
  });

  it('backgroundOps: a bare matte op for the recipe default / no choice, the model otherwise, then the morph', () => {
    const b: BackgroundCfg = { ...defaultBackground(), enabled: true, mode: 'ai' };
    expect(backgroundOps(b)).toEqual([{ kind: 'matte' }, { kind: 'morph', params: { close: true } }]);
    b.morph = { close: false, grow: 0 };
    expect(backgroundOps(b)).toEqual([{ kind: 'matte' }]);
    b.ai = { model: 'isnet-anime' }; // the explicit default: still bare (the Go zero value resolves to it)
    expect(backgroundOps(b)).toEqual([{ kind: 'matte' }]);
    b.ai = { model: 'birefnet-lite' };
    expect(backgroundOps(b)).toEqual([{ kind: 'matte', params: { model: 'birefnet-lite' } }]);
    b.ai = { model: '  birefnet-lite ' };
    expect(backgroundOps(b)).toEqual([{ kind: 'matte', params: { model: 'birefnet-lite' } }]);
    b.morph = { close: true, grow: 2 };
    expect(backgroundOps(b)).toEqual([{ kind: 'matte', params: { model: 'birefnet-lite' } }, { kind: 'morph', params: { close: true, grow: 2 } }]);
    // never size / resolved: the server's
    for (const op of backgroundOps(b)) expect(op.params ?? {}).not.toHaveProperty('resolved');
    b.enabled = false;
    expect(backgroundOps(b)).toEqual([]);
    // the Colour rows and the Screen settings are untouched by the AI choice
    b.enabled = true;
    b.mode = 'colour';
    b.colors = ['313338'];
    expect(backgroundOps(b).map((o) => o.kind)).toEqual(['colorkey', 'morph']);
  });

  it('buildOps: matte → morph → feather, before the geometry; keyPreview drops it, the crop preview keeps it', () => {
    const ops = defaultOps(gifInfo);
    ops.trim = { enabled: true, start: 0.5, end: 0 };
    ops.fps = { enabled: true, fps: 20 };
    ops.background = { ...defaultBackground(), enabled: true, mode: 'ai', ai: { model: 'birefnet-lite' }, morph: { close: true, grow: 1 } };
    ops.feather = { enabled: true, radius: 2 };
    ops.crop = { enabled: true, x: 0, y: 0, w: 10, h: 10 };
    ops.bounce = true;
    expect(buildOps(ops).map((o) => o.kind)).toEqual(['trim', 'fps', 'matte', 'morph', 'feather', 'crop', 'bounce']);
    expect(buildOps(ops)[2]).toEqual({ kind: 'matte', params: { model: 'birefnet-lite' } });
    expect(buildOps(ops, { keyPreview: true }).map((o) => o.kind)).toEqual(['trim', 'fps', 'feather', 'crop', 'bounce']);
    expect(buildOps(ops, { cropPreview: true }).map((o) => o.kind)).toEqual(['trim', 'fps', 'matte', 'morph', 'feather']);
    expect(aiActive(ops)).toBe(true);
    ops.background.mode = 'screen';
    expect(aiActive(ops)).toBe(false);
    ops.background.mode = 'ai';
    ops.background.enabled = false;
    expect(aiActive(ops)).toBe(false);
    // Optimize carries no ops at all
    const out = defaultOutput();
    out.preset = 'optimize';
    ops.background.enabled = true;
    expect(recipeOps(ops, out)).toEqual([]);
  });

  it('setBackgroundMode("ai") enables the card in AI mode and disarms the eyedropper; setMatteModel picks the model', () => {
    setSource(src);
    armEyedropper(0);
    setBackgroundMode('ai', { picker: true });
    expect(app.ops.background).toMatchObject({ enabled: true, mode: 'ai' });
    expect(app.ui.pickColor).toBe(false);
    setMatteModel(' birefnet-lite ');
    expect(app.ops.background.ai.model).toBe('birefnet-lite');
    expect(backgroundOps(app.ops.background)[0]).toEqual({ kind: 'matte', params: { model: 'birefnet-lite' } });
    setMatteModel('');
    expect(backgroundOps(app.ops.background)[0]).toEqual({ kind: 'matte' });
    // the header checkbox off and on again keeps the mode
    setBackgroundMode('none', { picker: true });
    expect(app.ops.background.enabled).toBe(false);
    expect(app.ops.background.mode).toBe('ai');
    setBackgroundMode('ai', { picker: false });
    expect(app.ops.background.enabled).toBe(true);
    expect(app.ui.pickColor).toBe(false);
    // Colour from AI arms the eyedropper as before
    setBackgroundMode('colour', { picker: true });
    expect(app.ui.pickColor).toBe(true);
    // a new source resets the choice
    setSource({ ...src, hash: 'b'.repeat(64) });
    expect(app.ops.background.ai).toEqual({ model: '' });
    resetApp();
  });

  it('the still requests carry the matte op (normal and crop mode), never eager by themselves', () => {
    const out = defaultOutput();
    out.preset = 'emote';
    presetById('emote').apply(out);
    const ops = defaultOps(gifInfo);
    ops.background = { ...defaultBackground(), enabled: true, mode: 'ai' };
    ops.crop = { enabled: true, x: 0, y: 0, w: 10, h: 10 };
    const normal = stillRequest(src, ops, out, { cropMode: false, picking: false, t: 0.5, maxW: 480 });
    const crop = stillRequest(src, ops, out, { cropMode: true, picking: false, t: 0.5, maxW: 8192 });
    expect(normal?.ops).toEqual([{ kind: 'matte' }, { kind: 'morph', params: { close: true } }, { kind: 'crop', params: { x: 0, y: 0, w: 10, h: 10 } }]);
    expect(crop?.ops).toEqual([{ kind: 'matte' }, { kind: 'morph', params: { close: true } }]);
    expect(crop?.output).toEqual({ format: 'gif', fps: 25 }); // the same frame grid → the same clip key server-side
    expect(normal).not.toHaveProperty('eager');
    expect(crop).not.toHaveProperty('eager');
    // the eyedropper is a Colour-mode thing: keyPreview still drops the card's ops when asked
    expect(stillRequest(src, ops, out, { cropMode: false, picking: true, t: 0.5, maxW: 480 })?.ops.map((o) => o.kind)).toEqual(['crop']);
  });
});
