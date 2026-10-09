// Server-side render of the Preview card: the scrubber is a frame-index
// range input (one notch per plan frame, both ends reachable), and
// role="slider" — a children-presentational role — sits on a dedicated
// element with no interactive descendants (W10): the position readout in the
// scrub row, never the stage (which contains the Retry button of the error
// overlay and the crop canvas).
import { render } from 'svelte/server';
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import type { ProbeInfo, Source } from '../lib/api';
import { resetFeatures, setFeatures } from '../lib/capabilities.svelte';
import { defaultOutput, presetById } from '../lib/presets';
import { app, defaultAi, defaultOps, setSource, stillRequest } from '../lib/state.svelte';
import Preview from './Preview.svelte';

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
const gifSrc: Source = { hash: 'f'.repeat(64), name: 'test.gif', size: 100, info: gifInfo };
/** the review's sequence: 34 frames, uploaded at 100 ms */
const seq34: ProbeInfo = {
  ...gifInfo,
  format: 'image2',
  codec: 'png',
  pixFmt: 'rgba',
  fps: 10,
  duration: 3.4,
  frames: 34,
  kind: 'sequence',
  sequence: { count: 34, pattern: '%06d.png', delayMs: 100, mixed: false },
};

beforeAll(() => {
  // Preview reads window.matchMedia at instance init (the wide-screen query);
  // the tests run in plain Node, so provide the minimal stub.
  vi.stubGlobal('window', {
    matchMedia: () => ({ matches: false, addEventListener: () => {}, removeEventListener: () => {} }),
  });
  return () => vi.unstubAllGlobals();
});

function html(): string {
  return render(Preview, { props: {} }).body;
}

/** the rendered <input type="range" …> tag */
function rangeTag(out: string): string {
  return out.match(/<input[^>]*type="range"[^>]*>/)?.[0] ?? '';
}

describe('Preview (SSR)', () => {
  afterEach(() => resetFeatures());

  it('puts role="slider" on the position readout only, never on the stage (W10)', () => {
    setSource(gifSrc);
    const out = html();
    expect(out.match(/role="slider"/g)?.length).toBe(1);
    // the slider is the .time readout span…
    expect(out).toMatch(/<span[^>]*class="time[^"]*"[^>]*role="slider"/);
    // …and the stage is a plain container without slider semantics or tabindex
    expect(out).not.toMatch(/<div[^>]*role="slider"/);
    expect(out).not.toMatch(/<div[^>]*class="stage[^"]*"[^>]*tabindex/);
    // frame-stepper semantics live on the readout (25 fps × 2 s = 50 frames)
    expect(out).toContain('aria-valuemax="50"');
    expect(out).toContain('aria-valuenow="1"');
  });

  it('scrubs by frame index: min 0, max N − 1, step 1 — both ends reachable, no phantom stop (review bugs 1, 2)', () => {
    setSource(gifSrc);
    let out = html();
    let tag = rangeTag(out);
    expect(tag).toContain('min="0"');
    expect(tag).toContain('max="49"');
    expect(tag).toContain('step="1"');
    expect(tag).not.toContain('disabled');
    expect(out).toContain('00:00.00 · f 1 / 50');
    // the last frame reads as frame 50 at 1.96 s (its start), the clip is 2.00 s
    app.ui.scrubFrame = 49;
    out = html();
    expect(out).toContain('00:01.96 · f 50 / 50');
    expect(out).toContain('aria-valuenow="50"');
    // a stale position past the end is clamped, never shown as frame 51
    app.ui.scrubFrame = 80;
    out = html();
    expect(out).toContain('f 50 / 50');
    expect(out).not.toContain('f 51');
    // an fps change re-grids the slider: 10 fps → 20 frames
    app.ui.scrubFrame = 0;
    app.ops.fps = { enabled: true, fps: 10 };
    tag = rangeTag(html());
    expect(tag).toContain('max="19"');
  });

  it('a sequence has exactly its frame count whatever the delay, so one notch is one frame (review bug 4)', () => {
    setSource({ ...gifSrc, info: seq34 });
    expect(rangeTag(html())).toContain('max="33"');
    expect(html()).toContain('aria-valuemax="34"');
    app.ops.delay = { enabled: true, ms: 33 };
    const out = html();
    expect(rangeTag(out)).toContain('max="33"');
    expect(out).toContain('aria-valuemax="34"');
    expect(out).toContain('f 1 / 34');
    app.ui.scrubFrame = 33;
    expect(html()).toContain('f 34 / 34');
  });

  it('counts a µs-precise scrubber trim exactly: frames 3..5 of a 30 fps clip are 3 notches (graph.FrameTolerance)', () => {
    const clip30 = { ...gifInfo, fps: 30, duration: 2, frames: 60 };
    setSource({ ...gifSrc, info: clip30 });
    expect(rangeTag(html())).toContain('max="59"');
    // what Trim "from scrubber" sets on frames 3 and 5: -ss 0.066667 -to 0.166667
    // (0.1 s × 30 = 2.9999999…; a 1e-6 tolerance on the µs-rounded 0.133333 × 30 = 3.99999 lost a frame)
    app.ops.trim = { enabled: true, start: 0.066667, end: 0.166667 };
    let out = html();
    expect(rangeTag(out)).toContain('max="2"');
    expect(out).toContain('aria-valuemax="3"');
    expect(out).toContain('00:00.00 · f 1 / 3');
    app.ops.trim = { enabled: true, start: 0.033333, end: 0.133333 };
    out = html();
    expect(rangeTag(out)).toContain('max="2"');
    expect(out).toContain('f 1 / 3');
  });

  it('a retimed sequence has the frames ffmpeg ends the stream with: 7 frames at speed 2 resampled to 20 fps are 6', () => {
    const seq60 = { ...seq34, duration: 6, frames: 60, sequence: { count: 60, pattern: '%06d.png', delayMs: 100, mixed: false } };
    setSource({ ...gifSrc, info: seq60 });
    expect(rangeTag(html())).toContain('max="59"');
    app.ops.trim = { enabled: true, start: 0, end: 0.7 };
    app.ops.speed = { enabled: true, factor: 2 };
    expect(rangeTag(html())).toContain('max="2"'); // trunc(7 / 2) = 3 frames
    app.ops.fps = { enabled: true, fps: 20 };
    const out = html();
    expect(rangeTag(out)).toContain('max="5"'); // not the 7 of floor(0.35 s × 20)
    expect(out).toContain('aria-valuemax="6"');
    expect(out).toContain('f 1 / 6');
    // a mid-frame trim snaps to the nearest grid frame: 0.06..0.11 s at 25 fps is one frame
    app.ops = { ...app.ops, trim: { enabled: true, start: 0.06, end: 0.11 }, speed: { enabled: false, factor: 1 }, fps: { enabled: false, fps: 25 } };
    app.ops.delay = { enabled: true, ms: 40 };
    expect(rangeTag(html())).toContain('disabled'); // one frame: nothing to step
    expect(html()).toContain('aria-valuemax="1"');
  });

  it('reserves one readout width for every frame, so the scrub row never re-wraps while scrubbing', () => {
    // 120 frames: the frame number is 1, 2 and 3 digits long over the clip.
    setSource({ ...gifSrc, info: { ...gifInfo, fps: 25, duration: 4.8, frames: 120 } });
    const readout = (frame: number) => {
      app.ui.scrubFrame = frame;
      const out = html();
      const tag = out.match(/<span[^>]*class="time[^"]*"[^>]*>/)?.[0] ?? '';
      // what the browser lays out: hydration comments and tags dropped, white space collapsed
      // (the readout is the last child of the scrub row)
      const text = (out.match(/<span[^>]*class="time[^"]*"[^>]*>([\s\S]*?)<\/span>\s*<\/div>/)?.[1] ?? '')
        .replace(/<!--[\s\S]*?-->/g, '')
        .replace(/<[^>]*>/g, '')
        .replace(/\s+/g, ' ')
        .trim();
      return { width: Number(tag.match(/min-width:\s*(\d+)ch/)?.[1] ?? NaN), text };
    };
    const first = readout(0);
    const mid = readout(54);
    const last = readout(119);
    expect(first.text).toBe('00:00.00 · f 1 / 120 · 00:04.80');
    expect(last.text).toBe('00:04.76 · f 120 / 120 · 00:04.80');
    expect(first.text.length).toBeLessThan(last.text.length); // the text itself still grows…
    // …but the box is as wide as the widest text on every frame
    expect(first.width).toBe(last.text.length);
    expect(mid.width).toBe(last.text.length);
    expect(last.width).toBe(last.text.length);
    app.ui.scrubFrame = 0;
  });

  it('keeps the readout slider present but disabled when frames cannot be stepped', () => {
    setSource({ ...gifSrc, info: { ...gifInfo, fps: 0, duration: 0, frames: 1, isStill: true, kind: 'image' } });
    const out = html();
    expect(out.match(/role="slider"/g)?.length).toBe(1);
    expect(out).toContain('aria-disabled="true"');
    expect(rangeTag(out)).toContain('disabled');
  });

  it('offers Play for an animation (on demand), not for a single still frame', () => {
    setSource(gifSrc);
    let out = html();
    const play = () => out.match(/<button[^>]*aria-label="Play an animated preview"[^>]*>/)?.[0] ?? '';
    expect(play()).toBeTruthy();
    expect(play()).not.toContain('disabled');
    expect(out).toContain('▶ Play');
    expect(out).not.toContain('■ Stop'); // nothing plays until asked
    // a still source: one frame, nothing to animate
    setSource({ ...gifSrc, info: { ...gifInfo, fps: 0, duration: 0, frames: 1, isStill: true, kind: 'image' } });
    out = html();
    expect(play()).toContain('disabled');
    // crop mode needs the still on the stage
    setSource(gifSrc);
    app.ui.cropOpen = true;
    out = html();
    expect(play()).toContain('disabled');
    expect(out).toContain('Crop mode: full frame shown');
    // auto-crop on: the preview shows the detected result, so no crop mode
    app.ops.autocrop.enabled = true;
    out = html();
    expect(play()).not.toContain('disabled');
    expect(out).not.toContain('Crop mode: full frame shown');
  });

  it('eyedropper (Phase 5a): the meta line names the Colour row the armed pick lands in when there are several, the colour otherwise', () => {
    setSource(gifSrc);
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'colour', colors: ['313338', ''] };
    app.ui.pickColor = true;
    app.ui.pickRow = 1;
    let out = html();
    expect(out).toContain('Eyedropper: click colour 2 to remove (the still is shown unkeyed)');
    expect(out).toContain('<kbd>Esc</kbd> cancels');
    app.ui.pickRow = 0;
    expect(html()).toContain('Eyedropper: click colour 1 to remove');
    app.ops.background.colors = [''];
    out = html();
    expect(out).toContain('Eyedropper: click the colour to remove (the still is shown unkeyed)');
    // Play is off while the eyedropper is armed (the still must be on the stage), crop mode wins over it
    expect(out.match(/<button[^>]*aria-label="Play an animated preview"[^>]*>/)?.[0]).toContain('disabled');
    app.ui.cropOpen = true;
    out = html();
    expect(out).not.toContain('Eyedropper:');
    expect(out).toContain('Crop mode: full frame shown');
    app.ui.cropOpen = false;
    app.ui.pickColor = false;
    expect(html()).not.toContain('Eyedropper:');
  });

  it('disables Play (with the reason) on a server without /api/proxy — features.proxy off', () => {
    setSource(gifSrc);
    const play = (out: string) => out.match(/<button[^>]*aria-label="Play an animated preview"[^>]*>/)?.[0] ?? '';
    // the flags are optimistic until the server answers: Play is offered
    expect(play(html())).not.toContain('disabled');
    expect(play(html())).toContain('rendered on demand');
    // a Phase 2 server: no proxy feature
    setFeatures({ features: { fit: true, sequence: true, optimize: true } });
    let tag = play(html());
    expect(tag).toContain('disabled');
    expect(tag).toContain('this server has no /api/proxy');
    expect(tag).not.toContain('rendered on demand');
    expect(html()).not.toContain('■ Stop');
    // a Phase 3 server: back to the normal tooltip
    setFeatures({ features: { proxy: true } });
    tag = play(html());
    expect(tag).not.toContain('disabled');
    expect(tag).toContain('rendered on demand');
  });

  it('still request (Phase 5a, spec §6.1): crop mode cuts the stack before crop and drops the geometry but keeps the fps — the same frame grid as the normal still', () => {
    const out = defaultOutput();
    out.preset = 'emote';
    presetById('emote').apply(out); // 128×128 contain at 25 fps
    const src30: Source = { ...gifSrc, info: { ...gifInfo, fps: 30, duration: 2, frames: 60 } };
    const ops = defaultOps(src30.info);
    ops.crop = { enabled: true, x: 10, y: 20, w: 100, h: 50 };
    ops.background = { ...ops.background, enabled: true, mode: 'colour', colors: ['313338'] };
    const normal = stillRequest(src30, ops, out, { cropMode: false, picking: false, t: 0.5, maxW: 480 });
    const crop = stillRequest(src30, ops, out, { cropMode: true, picking: false, t: 0.5, maxW: 8192 });
    expect(normal?.output).toEqual({ format: 'gif', width: 128, height: 128, fit: 'contain', fps: 25 });
    expect(crop?.output).toEqual({ format: 'gif', fps: 25 });
    expect(crop?.output.fps).toBe(normal?.output.fps);
    // the stack: the normal still carries the key, its morph and the crop; crop mode stops before the crop
    expect(normal?.ops.map((o) => o.kind)).toEqual(['colorkey', 'morph', 'crop']);
    expect(crop?.ops.map((o) => o.kind)).toEqual(['colorkey', 'morph']);
    expect(crop?.sources).toEqual([src30.hash]);
    expect(crop).toMatchObject({ src: src30.hash, t: 0.5, maxW: 8192 });
    expect(normal).toMatchObject({ src: src30.hash, sources: [src30.hash], t: 0.5, maxW: 480 });
    // the eyedropper armed: the Background card's ops are left out
    expect(stillRequest(src30, ops, out, { cropMode: false, picking: true, t: 0.5, maxW: 480 })?.ops.map((o) => o.kind)).toEqual(['crop']);
    // without Output.fps neither carries one (the server falls back to the fps op or the source rate on both)
    const noFps = { ...out, fps: 0 };
    expect(stillRequest(src30, ops, noFps, { cropMode: true, picking: false, t: 0.5, maxW: 8192 })?.output).toEqual({ format: 'gif' });
    // no source, no request
    expect(stillRequest(null, ops, out, { cropMode: false, picking: false, t: 0, maxW: 480 })).toBeNull();
  });

  it('prompt mode (Phase 5c): the guided model with its panel open shows the source frame as a prompt canvas — Play off, the meta line says how; crop mode and the eyedropper win', () => {
    setSource(gifSrc);
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'ai', ai: { ...defaultAi(), model: 'sam2-tiny', modelChosen: true } };
    const play = (out: string) => out.match(/<button[^>]*aria-label="Play an animated preview"[^>]*>/)?.[0] ?? '';
    // the panel closed: an ordinary preview
    let out = html();
    expect(out).not.toContain('Select subject:');
    expect(play(out)).not.toContain('disabled');
    app.ui.promptOpen = true;
    out = html();
    expect(out).toContain('Select subject: drag a box around it, click to keep (+)');
    expect(out).toContain('click a marker to');
    expect(out).toContain('0 prompted frames');
    expect(play(out)).toContain('disabled');
    expect(out).not.toContain('Crop mode:');
    app.ops.background.ai.prompts = [{ frame: 0, box: [0, 0, 1, 1], points: [] }];
    expect(html()).toContain('1 prompted frame');
    // a per-frame model: no prompt mode even with the flag on
    app.ops.background.ai.model = 'birefnet-lite';
    out = html();
    expect(out).not.toContain('Select subject:');
    expect(play(out)).not.toContain('disabled');
    app.ops.background.ai.model = 'sam2-tiny';
    // crop mode wins over the panel; the armed eyedropper too
    app.ui.cropOpen = true;
    out = html();
    expect(out).toContain('Crop mode: full frame shown');
    expect(out).not.toContain('Select subject:');
    app.ui.cropOpen = false;
    app.ui.pickColor = true;
    app.ui.pickTarget = 'keep';
    app.ui.pickRow = 0;
    out = html();
    expect(out).toContain('Eyedropper:');
    expect(out).not.toContain('Select subject:');
    app.ui.pickColor = false;
    app.ui.promptOpen = false;
    // the still request of prompt mode: the source frame unkeyed, the main source alone, format + fps only
    const out1 = defaultOutput();
    out1.preset = 'emote';
    presetById('emote').apply(out1);
    const ops = defaultOps(gifInfo);
    ops.background = { ...ops.background, enabled: true, mode: 'ai', ai: { ...defaultAi(), model: 'sam2-tiny', prompts: [{ frame: 0, box: [0, 0, 1, 1], points: [] }] } };
    ops.crop = { enabled: true, x: 10, y: 20, w: 100, h: 50 };
    const r = stillRequest(gifSrc, ops, out1, { cropMode: false, picking: false, promptMode: true, t: 0.02, maxW: 480 });
    expect(r?.ops).toEqual([]);
    expect(r?.sources).toEqual([gifSrc.hash]);
    expect(r?.output).toEqual({ format: 'gif', fps: 25 });
    expect(stillRequest(gifSrc, ops, out1, { cropMode: false, picking: false, t: 0.02, maxW: 480 })?.ops.map((o) => o.kind)).toEqual(['matte', 'morph', 'crop']);
  });
});
