// Phase 5c op stack: the AI mode's stabilise / keep / prompts / edge on the
// matte op, the Keep colours rows and their eyedropper target, the model
// choice vs the auto-filled default, the guided model's prompt mode
// (still request, mask request, the keyframe strip's scrubber mapping).
import { describe, expect, it } from 'vitest';
import { MATTE_EDGE_NONE, MATTE_MODEL_SAM2_TINY, type ProbeInfo, type Source } from './api';
import { defaultOutput, presetById } from './presets';
import type { FramePrompts } from './prompts';
import {
  addKeepColor,
  aiActive,
  app,
  applyPickedColor,
  armEyedropper,
  armKeepEyedropper,
  backgroundOps,
  clearFramePrompts,
  clearMattePrompts,
  defaultAi,
  defaultBackground,
  defaultOps,
  edgeIsNone,
  fillMatteDefault,
  forwardFrame,
  guidedNeedsPrompts,
  isGuided,
  KEEP_DEFAULTS,
  keepColors,
  matteClipKey,
  promptGridKey,
  promptMaskRequest,
  prunePrompts,
  removeKeepColor,
  resetApp,
  scrubFrameFor,
  setBackgroundMode,
  setFrameMaskPrompt,
  setKeepColor,
  setMatteEdge,
  setMatteModel,
  setMattePrompts,
  setMatteStabilise,
  setSource,
  STABILISE_DEFAULT,
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
const box: FramePrompts = { frame: 3, box: [0.1, 0.2, 0.6, 0.9], points: [{ x: 0.33333333, y: 0.5, label: 1 }] };

function ai(over: Partial<BackgroundCfg['ai']> = {}): BackgroundCfg {
  return { ...defaultBackground(), enabled: true, mode: 'ai', morph: { close: false, grow: 0 }, ai: { ...defaultAi(), ...over } };
}

describe('AI mode ops (Phase 5c)', () => {
  it('a new session stabilises lightly, keeps nothing, has no prompts and the default edge; the device is nowhere in the config', () => {
    expect(defaultAi()).toEqual({ model: '', modelChosen: false, stabilise: 'light', keep: [], keepSimilarity: 0.08, prompts: [], edge: '' });
    expect(STABILISE_DEFAULT).toBe('light');
    expect(KEEP_DEFAULTS.similarity).toBe(0.08);
    expect(JSON.stringify(defaultAi())).not.toContain('device');
  });

  it('the matte op carries stabilise whenever set (the recipe zero value is off), never an unknown mode', () => {
    expect(backgroundOps(ai())).toEqual([{ kind: 'matte', params: { stabilise: 'light' } }]);
    expect(backgroundOps(ai({ stabilise: 'strong', model: 'birefnet-lite' }))).toEqual([{ kind: 'matte', params: { model: 'birefnet-lite', stabilise: 'strong' } }]);
    expect(backgroundOps(ai({ stabilise: '' }))).toEqual([{ kind: 'matte' }]);
    expect(backgroundOps(ai({ stabilise: 'median5' }))).toEqual([{ kind: 'matte' }]); // not a mode: dropped
    // the setter refuses anything but the three modes
    setSource(src);
    setBackgroundMode('ai', { picker: true });
    setMatteStabilise('strong');
    expect(app.ops.background.ai.stabilise).toBe('strong');
    setMatteStabilise(' ');
    expect(app.ops.background.ai.stabilise).toBe('');
    setMatteStabilise('median5');
    expect(app.ops.background.ai.stabilise).toBe('');
    setMatteStabilise('light');
    expect(app.ops.background.ai.stabilise).toBe('light');
    resetApp();
  });

  it('keep colours: picked rows only, deduplicated, at most 6, with the similarity when it is not the default 0.08', () => {
    const b = ai({ stabilise: '', keep: ['', 'ff0000', '', 'FF0000'.toLowerCase(), '00ff00'] });
    expect(keepColors(b)).toEqual(['ff0000', '00ff00']);
    expect(backgroundOps(b)).toEqual([{ kind: 'matte', params: { keep: ['ff0000', '00ff00'] } }]);
    b.ai.keepSimilarity = 0.2;
    expect(backgroundOps(b)[0].params).toEqual({ keep: ['ff0000', '00ff00'], keepSimilarity: 0.2 });
    b.ai.keepSimilarity = 0.08; // the default: left out
    expect(backgroundOps(b)[0].params).toEqual({ keep: ['ff0000', '00ff00'] });
    b.ai.keepSimilarity = 5; // clamped
    expect(backgroundOps(b)[0].params).toMatchObject({ keepSimilarity: 1 });
    b.ai.keep = ['111111', '222222', '333333', '444444', '555555', '666666', '777777'];
    expect(keepColors(b)).toHaveLength(6);
    // no keep rows at all: no keep fields (similarity alone says nothing)
    b.ai.keep = [];
    b.ai.keepSimilarity = 0.3;
    expect(backgroundOps(b)).toEqual([{ kind: 'matte' }]);
  });

  it('the Keep rows: add / set / remove / arm, and a pick lands in the keep row keeping the card in AI', () => {
    setSource(src);
    setBackgroundMode('ai', { picker: true });
    expect(addKeepColor()).toBe(0);
    expect(addKeepColor()).toBe(1);
    expect(app.ops.background.ai.keep).toEqual(['', '']);
    expect(setKeepColor(0, '#FF0000')).toBe(true);
    expect(setKeepColor(1, 'nope')).toBe(false);
    expect(setKeepColor(5, '00ff00')).toBe(false);
    expect(app.ops.background.ai.keep).toEqual(['ff0000', '']);
    armKeepEyedropper(1);
    expect(app.ui).toMatchObject({ pickColor: true, pickRow: 1, pickTarget: 'keep' });
    applyPickedColor('#00ff00');
    expect(app.ops.background.ai.keep).toEqual(['ff0000', '00ff00']);
    expect(app.ops.background).toMatchObject({ enabled: true, mode: 'ai' }); // not switched to Colour
    expect(app.ui.pickColor).toBe(false);
    // a typed hex disarms an eyedropper waiting for that row
    armKeepEyedropper(0);
    expect(setKeepColor(0, '123456')).toBe(true);
    expect(app.ui.pickColor).toBe(false);
    // removing: the armed row disarms, a later armed row follows its row down
    armKeepEyedropper(1);
    removeKeepColor(0);
    expect(app.ops.background.ai.keep).toEqual(['00ff00']);
    expect(app.ui).toMatchObject({ pickColor: true, pickRow: 0 });
    removeKeepColor(0);
    expect(app.ops.background.ai.keep).toEqual([]);
    expect(app.ui.pickColor).toBe(false);
    for (let i = 0; i < 6; i++) addKeepColor();
    expect(addKeepColor()).toBe(-1);
    // a Colour-row pick is untouched by the keep target: it still switches to Colour
    armEyedropper(0);
    expect(app.ui.pickTarget).toBe('colour');
    applyPickedColor('313338');
    expect(app.ops.background.mode).toBe('colour');
    expect(app.ops.background.colors).toEqual(['313338']);
    resetApp();
  });

  it('the guided model emits no matte op until the prompts can select something; then the rounded, frame-sorted prompts and the edge', () => {
    const b = ai({ stabilise: '', model: MATTE_MODEL_SAM2_TINY });
    expect(isGuided(b)).toBe(true);
    expect(backgroundOps(b)).toEqual([]);
    expect(guidedNeedsPrompts({ background: b })).toBe(true);
    expect(aiActive({ background: b })).toBe(true);
    b.ai.prompts = [{ frame: 2, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }] }]; // a − click alone selects nothing
    expect(backgroundOps(b)).toEqual([]);
    b.ai.prompts = [{ frame: 7, box: null, points: [{ x: 0.25, y: 0.75, label: 1 }] }, box];
    expect(backgroundOps(b)).toEqual([
      {
        kind: 'matte',
        params: {
          model: 'sam2-tiny',
          prompts: [
            { frame: 3, points: [[0.3333, 0.5, 1]], box: [0.1, 0.2, 0.6, 0.9] },
            { frame: 7, points: [[0.25, 0.75, 1]] },
          ],
        },
      },
    ]);
    expect(guidedNeedsPrompts({ background: b })).toBe(false);
    b.ai.edge = MATTE_EDGE_NONE;
    expect(backgroundOps(b)[0].params).toMatchObject({ edge: 'none' });
    expect(edgeIsNone(b.ai.edge)).toBe(true);
    b.ai.edge = 'birefnet-lite';
    b.ai.stabilise = 'light';
    b.ai.keep = ['ff0000'];
    expect(backgroundOps(b)[0].params).toMatchObject({ edge: 'birefnet-lite', stabilise: 'light', keep: ['ff0000'] });
    // a per-frame model ignores prompts and edge
    b.ai.model = 'birefnet-lite';
    expect(backgroundOps(b)[0].params).toEqual({ model: 'birefnet-lite', stabilise: 'light', keep: ['ff0000'] });
    expect(isGuided(b)).toBe(false);
  });

  it('setMatteModel pins the choice and opens the Select subject panel for the guided model; fillMatteDefault only moves an unchosen model', () => {
    setSource(src);
    setBackgroundMode('ai', { picker: true });
    fillMatteDefault('birefnet-lite');
    expect(app.ops.background.ai).toMatchObject({ model: 'birefnet-lite', modelChosen: false });
    fillMatteDefault('isnet-anime'); // the device changed: the unchosen model follows
    expect(app.ops.background.ai.model).toBe('isnet-anime');
    setMatteModel(' birefnet-lite ');
    expect(app.ops.background.ai).toMatchObject({ model: 'birefnet-lite', modelChosen: true });
    expect(app.ui.promptOpen).toBe(false);
    fillMatteDefault('isnet-anime'); // chosen: stays
    expect(app.ops.background.ai.model).toBe('birefnet-lite');
    setMatteModel(MATTE_MODEL_SAM2_TINY);
    expect(app.ui.promptOpen).toBe(true);
    setMatteModel('isnet-anime');
    expect(app.ui.promptOpen).toBe(false);
    setMatteModel('');
    expect(app.ops.background.ai).toMatchObject({ model: '', modelChosen: false });
    fillMatteDefault('  ');
    expect(app.ops.background.ai.model).toBe('');
    setMatteEdge(' none ');
    expect(app.ops.background.ai.edge).toBe('none');
    // a new source resets everything, the prompt panel included
    app.ui.promptOpen = true;
    setSource({ ...src, hash: 'b'.repeat(64) });
    expect(app.ops.background.ai).toEqual(defaultAi());
    expect(app.ui.promptOpen).toBe(false);
    resetApp();
  });

  it('prompt mode asks for the source frame (crop mode’s cut) without the keys; the mask request carries the frame’s prompts and the guided op', () => {
    const out = defaultOutput();
    out.preset = 'emote';
    presetById('emote').apply(out);
    const ops = defaultOps(gifInfo);
    ops.background = ai({ stabilise: '', model: MATTE_MODEL_SAM2_TINY, prompts: [box] });
    ops.crop = { enabled: true, x: 0, y: 0, w: 10, h: 10 };
    const prompt = stillRequest(src, ops, out, { cropMode: false, picking: false, promptMode: true, t: 0.14, maxW: 480 });
    expect(prompt?.ops).toEqual([]); // no keys, no crop
    expect(prompt?.sources).toEqual([src.hash]);
    expect(prompt?.output).toEqual({ format: 'gif', fps: 25 });
    expect(prompt).not.toHaveProperty('eager');
    // the normal still carries the guided op
    const normal = stillRequest(src, ops, out, { cropMode: false, picking: false, t: 0.14, maxW: 480 });
    expect(normal?.ops.map((o) => o.kind)).toEqual(['matte', 'crop']);
    // the mask request: that frame's prompts alone, the full recipe ops, the preview output
    const m = promptMaskRequest(src, ops, out, 3);
    expect(m).toEqual({
      src: src.hash,
      sources: [src.hash],
      ops: normal?.ops,
      output: { format: 'gif', width: 128, height: 128, fit: 'contain', fps: 25 },
      frame: 3,
      prompts: { obj: 1, prompts: [{ frame: 3, points: [[0.3333, 0.5, 1]], box: [0.1, 0.2, 0.6, 0.9] }] },
    });
    expect(promptMaskRequest(src, ops, out, 4)).toBeNull(); // nothing on frame 4
    ops.background.ai.prompts = [{ frame: 3, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }] }];
    expect(promptMaskRequest(src, ops, out, 3)).toBeNull(); // a − click alone
    ops.background.ai.prompts = [box];
    ops.background.ai.model = 'birefnet-lite';
    expect(promptMaskRequest(src, ops, out, 3)).toBeNull(); // not guided
    expect(promptMaskRequest(null, ops, out, 3)).toBeNull();
  });

  it('scrubFrameFor inverts forwardFrame: plain, reversed and bounced plans', () => {
    const out = defaultOutput();
    const ops = defaultOps(gifInfo); // 50 frames
    for (const f of [0, 7, 49]) expect(scrubFrameFor(gifInfo, ops, out, f)).toBe(f);
    ops.reverse = true;
    expect(scrubFrameFor(gifInfo, ops, out, 0)).toBe(49);
    expect(forwardFrame(gifInfo, ops, out, scrubFrameFor(gifInfo, ops, out, 7))).toBe(7);
    ops.reverse = false;
    ops.bounce = true; // 100 slots, the forward half is 0..49
    expect(scrubFrameFor(gifInfo, ops, out, 7)).toBe(7);
    expect(forwardFrame(gifInfo, ops, out, 92)).toBe(7); // the mirror shows the same frame
    ops.reverse = true;
    expect(forwardFrame(gifInfo, ops, out, scrubFrameFor(gifInfo, ops, out, 7))).toBe(7);
    expect(scrubFrameFor(gifInfo, ops, out, 500)).toBe(0); // clamped to the forward grid
  });

  it('promptGridKey follows the source, trim start, delay, speed and plan fps — not the trim end, crop, reverse or bounce; prunePrompts drops moved or past-the-end prompts', () => {
    setSource(src);
    const c = app.ops;
    const k0 = promptGridKey(app.source, c, app.output);
    expect(k0).not.toBe('');
    expect(promptGridKey(null, c, app.output)).toBe('');
    c.trim = { enabled: true, start: 0, end: 1 }; // the end alone keeps every earlier frame where it is
    c.crop.enabled = true;
    c.reverse = true;
    c.bounce = true;
    c.background.ai.keep = ['ff0000'];
    expect(promptGridKey(app.source, c, app.output)).toBe(k0);
    c.trim = { enabled: true, start: 0.4, end: 1 };
    const k1 = promptGridKey(app.source, c, app.output);
    expect(k1).not.toBe(k0);
    c.speed = { enabled: true, factor: 2 };
    const k2 = promptGridKey(app.source, c, app.output);
    expect(k2).not.toBe(k1);
    c.fps = { enabled: true, fps: 12 };
    const k3 = promptGridKey(app.source, c, app.output);
    expect(k3).not.toBe(k2);
    c.fps = { enabled: false, fps: 12 };
    app.output = { ...app.output, fps: 5 }; // the Output card's rate is the plan's when no fps op is on
    const k4 = promptGridKey(app.source, c, app.output);
    expect(k4).not.toBe(k2);
    expect(k4).not.toBe(k3);
    c.delay = { enabled: true, ms: 40 };
    expect(promptGridKey(app.source, c, app.output)).not.toBe(k4);
    const other: Source = { ...src, hash: 'b'.repeat(64) };
    expect(promptGridKey(other, c, app.output)).not.toBe(promptGridKey(src, c, app.output));

    const prompts: FramePrompts[] = [box, { frame: 40, box: null, points: [{ x: 0.5, y: 0.5, label: 1 }] }];
    setMattePrompts(prompts);
    expect(prunePrompts(false, 0)).toBe(0); // an unknown count drops nothing
    expect(app.ops.background.ai.prompts).toHaveLength(2);
    expect(prunePrompts(false, 41)).toBe(0); // frame 40 of 41 is inside
    expect(prunePrompts(false, 20)).toBe(1); // past the clip's end
    expect(app.ops.background.ai.prompts.map((p) => p.frame)).toEqual([3]);
    expect(prunePrompts(true, 20)).toBe(1); // the grid moved: everything goes
    expect(app.ops.background.ai.prompts).toEqual([]);
    expect(prunePrompts(true, 20)).toBe(0);
    resetApp();
  });

  it('clearMattePrompts / clearFramePrompts / setMattePrompts edit the prompt list', () => {
    setSource(src);
    setMattePrompts([box, { frame: 9, box: null, points: [{ x: 0.1, y: 0.1, label: 1 }] }]);
    expect(app.ops.background.ai.prompts.map((p) => p.frame)).toEqual([3, 9]);
    clearFramePrompts(3);
    expect(app.ops.background.ai.prompts.map((p) => p.frame)).toEqual([9]);
    clearMattePrompts();
    expect(app.ops.background.ai.prompts).toEqual([]);
    resetApp();
  });

  // ---- Phase 5d: the mask prompt ("Use this frame's matte") and the clip key
  it('a mask prompt serialises as {frame, maskFrom: "edge"} in the op — no bytes, no wire flag — and with mask: true on the live-mask wire', () => {
    const b = ai({ stabilise: '', model: MATTE_MODEL_SAM2_TINY, prompts: [{ frame: 5, box: null, points: [], maskFrom: 'edge' }] });
    expect(guidedNeedsPrompts({ background: b })).toBe(false);
    expect(backgroundOps(b)).toEqual([{ kind: 'matte', params: { model: 'sam2-tiny', prompts: [{ frame: 5, maskFrom: 'edge' }] } }]);
    b.ai.prompts = [{ frame: 5, box: null, points: [{ x: 0.5, y: 0.25, label: 0 }], maskFrom: 'edge' }, box];
    expect(backgroundOps(b)[0].params).toMatchObject({
      prompts: [
        { frame: 3, box: [0.1, 0.2, 0.6, 0.9], points: [[0.3333, 0.5, 1]] },
        { frame: 5, maskFrom: 'edge', points: [[0.5, 0.25, 0]] },
      ],
    });
    const ops = defaultOps(gifInfo);
    ops.background = b;
    const out = defaultOutput();
    const m = promptMaskRequest(src, ops, out, 5);
    expect(m?.prompts).toEqual({ obj: 1, prompts: [{ frame: 5, maskFrom: 'edge', mask: true, points: [[0.5, 0.25, 0]] }] });
    const op = m?.ops.find((o) => o.kind === 'matte');
    expect(op).toEqual({
      kind: 'matte',
      params: {
        model: 'sam2-tiny',
        prompts: [
          { frame: 3, points: [[0.3333, 0.5, 1]], box: [0.1, 0.2, 0.6, 0.9] },
          { frame: 5, maskFrom: 'edge', points: [[0.5, 0.25, 0]] },
        ],
      },
    });
    expect(JSON.stringify(m?.ops)).not.toContain('"mask":');
    expect(promptMaskRequest(src, ops, out, 3)?.prompts.prompts[0]).not.toHaveProperty('mask');
  });

  it('setFrameMaskPrompt makes the scrubber’s frame the mask prompt (replacing its box and clicks, one per set), opens the panel and clears the guard; off takes it back', () => {
    setSource(src);
    setMattePrompts([box, { frame: 9, box: null, points: [{ x: 0.1, y: 0.1, label: 1 }] }]);
    app.ui.promptOpen = false;
    app.ui.promptWarning = 'That looks like the background';
    setFrameMaskPrompt(3, true);
    expect(app.ops.background.ai.prompts).toEqual([
      { frame: 3, box: null, points: [], maskFrom: 'edge' },
      { frame: 9, box: null, points: [{ x: 0.1, y: 0.1, label: 1 }] },
    ]);
    expect(app.ui.promptOpen).toBe(true);
    expect(app.ui.promptWarning).toBe('');
    setFrameMaskPrompt(9, true);
    expect(app.ops.background.ai.prompts).toEqual([{ frame: 9, box: null, points: [], maskFrom: 'edge' }]); // frame 3 was mask-only: dropped
    app.ui.promptOpen = false;
    setFrameMaskPrompt(9, false);
    expect(app.ops.background.ai.prompts).toEqual([]);
    expect(app.ui.promptOpen).toBe(false); // taking a mask off never opens the panel
    // Edge None offers no frame matte: setMatteEdge takes the mask off (the frame's clicks stay) and says so once
    setMattePrompts([{ frame: 3, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }], maskFrom: 'edge' }, { frame: 9, box: null, points: [], maskFrom: 'edge' }]);
    expect(setMatteEdge('birefnet-lite')).toBe(false);
    expect(app.ops.background.ai.prompts).toHaveLength(2);
    expect(setMatteEdge(MATTE_EDGE_NONE)).toBe(true);
    expect(app.ops.background.ai.edge).toBe('none');
    expect(app.ops.background.ai.prompts).toEqual([{ frame: 3, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }] }, { frame: 9, box: null, points: [], maskFrom: 'edge' }]);
    expect(setMatteEdge(MATTE_EDGE_NONE)).toBe(true); // the second (malformed, two-mask) entry goes next
    expect(app.ops.background.ai.prompts).toEqual([{ frame: 3, box: null, points: [{ x: 0.5, y: 0.5, label: 0 }] }]);
    expect(setMatteEdge(MATTE_EDGE_NONE)).toBe(false);
    expect(setMatteEdge('')).toBe(false);
    // a new source clears the warning with the rest of the per-source UI
    app.ui.promptWarning = 'x';
    setSource({ ...src, hash: 'c'.repeat(64) });
    expect(app.ui.promptWarning).toBe('');
    resetApp();
  });

  it('matteClipKey names the clip the way the server’s clip key does: the source, the temporal ops in stack order and the plan fps — never the geometry, the keys or the trim end alone', () => {
    const out = defaultOutput();
    const ops = defaultOps(gifInfo);
    const k0 = matteClipKey(src, ops, out);
    expect(k0).toContain(src.hash);
    expect(matteClipKey(null, ops, out)).toBe('');
    expect(matteClipKey({ ...src, hash: 'b'.repeat(64) }, ops, out)).not.toBe(k0);
    ops.crop = { enabled: true, x: 0, y: 0, w: 10, h: 10 };
    ops.background = ai({ model: 'birefnet-lite' });
    ops.reverse = true;
    expect(matteClipKey(src, ops, out)).toBe(k0); // crop, the matte op and reverse are not in it
    ops.trim = { enabled: true, start: 0.5, end: 1.5 };
    const k1 = matteClipKey(src, ops, out);
    expect(k1).not.toBe(k0);
    ops.speed = { enabled: true, factor: 2 };
    const k2 = matteClipKey(src, ops, out);
    expect(k2).not.toBe(k1);
    ops.fps = { enabled: true, fps: 10 };
    const k3 = matteClipKey(src, ops, out);
    expect(k3).not.toBe(k2);
    expect(matteClipKey(src, ops, out)).toBe(k3); // stable
  });
});
