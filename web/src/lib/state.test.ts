import { describe, expect, it } from 'vitest';
import { OUTPUT_FORMATS, type OutputFormat, type ProbeInfo, type Source, type Target } from './api';
import { sequenceDelayOverride } from './files';
import { frameCount, round, trimTime } from './format';
import { defaultOutput, PRESETS, presetById, TARGETS, type OutputCfg } from './presets';
import {
  app,
  applyPreset,
  buildOps,
  buildOutput,
  defaultOps,
  effectiveFPS,
  effectiveOps,
  fitBytesFor,
  frameWindow,
  gridRound,
  loopFor,
  micros,
  planFPS,
  planFrames,
  previewDuration,
  recipeOps,
  resetApp,
  sequenceFrames,
  sequenceSelection,
  setSource,
  setTarget,
  sourceDuration,
  sourceFPS,
  sourceFrames,
  sourceSpan,
  speedFactor,
  trimRange,
  trimStartMax,
  usesMatte,
  type OpsCfg,
} from './state.svelte';

const FORMATS: OutputFormat[] = ['gif', 'webp'];
const DISCORD_TARGETS: Target[] = TARGETS.filter((t) => t !== '');

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
const seqInfo: ProbeInfo = {
  ...gifInfo,
  format: 'image2',
  codec: 'png',
  pixFmt: 'rgba',
  fps: 10,
  duration: 1.2,
  frames: 12,
  kind: 'sequence',
  sequence: { count: 12, pattern: '%06d.png', delayMs: 100, mixed: false },
};
/** the review's sequence: 34 frames uploaded at 100 ms, delay later set to 33 ms */
const seq34: ProbeInfo = {
  ...seqInfo,
  duration: 3.4,
  frames: 34,
  sequence: { count: 34, pattern: '%06d.png', delayMs: 100, mixed: false },
};
const src = (info: ProbeInfo, hash = 'a'.repeat(64)): Source => ({ hash, name: 'x', size: 1, info });

describe('buildOutput', () => {
  // Every Discord target requires loop forever (DESIGN §5.3; discordlint keeps
  // gif.netscape-loop / webp.loop-forever as errors for them), so whatever the
  // Loop control holds, a Discord target sends loop 0 — i.e. no `loop` key
  // (0 is the recipe zero value and omitted from the JSON).
  it('never carries a loop count for a Discord target', () => {
    expect(DISCORD_TARGETS).toHaveLength(6);
    for (const p of PRESETS) {
      for (const format of FORMATS) {
        for (const loop of [0, 1, 5]) {
          const cfg = defaultOutput();
          cfg.preset = p.id;
          p.apply(cfg);
          cfg.format = format;
          cfg.loop = loop;
          if (!cfg.target) continue; // Custom keeps whatever target it had
          expect(buildOutput(cfg), `${p.id}/${format}/loop=${loop}`).not.toHaveProperty('loop');
          expect(loopFor(cfg)).toBe(0);
        }
      }
    }
    for (const target of DISCORD_TARGETS) {
      const cfg = defaultOutput();
      cfg.preset = 'custom';
      cfg.target = target;
      cfg.loop = 3;
      expect(buildOutput(cfg), target).not.toHaveProperty('loop');
      expect(buildOutput(cfg).target, target).toBe(target); // the tier string goes out as-is
    }
  });

  it('carries the loop count only with no Discord target', () => {
    const cfg = defaultOutput();
    cfg.preset = 'custom';
    cfg.target = '';
    cfg.loop = 0; // forever = the zero value, left out
    expect(buildOutput(cfg)).not.toHaveProperty('loop');
    expect(loopFor(cfg)).toBe(0);
    cfg.loop = 2; // plays 3 times
    expect(buildOutput(cfg)).toMatchObject({ format: 'gif', loop: 2 });
    expect(loopFor(cfg)).toBe(2);
    cfg.format = 'webp';
    expect(buildOutput(cfg)).toMatchObject({ format: 'webp', loop: 2 });
    cfg.loop = 2.6; // whole counts only
    expect(buildOutput(cfg)).toMatchObject({ loop: 3 });
    cfg.loop = -1;
    expect(buildOutput(cfg)).not.toHaveProperty('loop');
    // and switching to a Discord target drops it again without touching the config
    cfg.target = 'attachment-50';
    expect(buildOutput(cfg)).not.toHaveProperty('loop');
    expect(cfg.loop).toBe(-1);
  });

  it('defaults to loop forever', () => {
    expect(defaultOutput().loop).toBe(0);
  });

  it('emits only the knobs of the selected format', () => {
    const cfg = defaultOutput();
    cfg.preset = 'custom';
    cfg.target = '';
    cfg.format = 'gif';
    cfg.width = 128;
    cfg.height = 0;
    cfg.fps = 25;
    cfg.colors = 64;
    cfg.dither = 'bayer';
    cfg.lossy = 40;
    cfg.alphaThreshold = 100;
    cfg.matte = '313338';
    expect(buildOutput(cfg)).toEqual({
      format: 'gif',
      width: 128,
      fit: 'contain',
      fps: 25,
      colors: 64,
      dither: 'bayer',
      lossy: 40,
      alphaThreshold: 100,
      matte: '313338',
      preset: 'custom',
    });

    cfg.format = 'webp';
    cfg.quality = 70;
    cfg.lossless = false;
    expect(buildOutput(cfg)).toEqual({ format: 'webp', width: 128, fit: 'contain', fps: 25, quality: 70, preset: 'custom' });
    cfg.lossless = true;
    expect(buildOutput(cfg)).toEqual({ format: 'webp', width: 128, fit: 'contain', fps: 25, lossless: true, preset: 'custom' });
  });

  it('carries the Discord target only when one is set, and the Chat preset id', () => {
    const cfg = defaultOutput();
    presetById('emote').apply(cfg);
    cfg.preset = 'emote';
    expect(buildOutput(cfg)).toMatchObject({ format: 'gif', width: 128, height: 128, fit: 'contain', target: 'emote', preset: 'emote' });
    cfg.preset = 'custom';
    cfg.target = '';
    expect(buildOutput(cfg)).not.toHaveProperty('target');
    const chat = defaultOutput();
    expect(buildOutput(chat)).toMatchObject({ format: 'gif', target: 'attachment', preset: 'chat', dither: 'sierra2_4a', lossy: 20 });
    chat.format = 'webp';
    expect(buildOutput(chat)).toMatchObject({ format: 'webp', target: 'attachment', preset: 'chat', quality: 80 });
  });

  it('emits the fit fields only when a budget is on', () => {
    const cfg = defaultOutput();
    presetById('emote').apply(cfg);
    cfg.preset = 'emote';
    expect(buildOutput(cfg)).toMatchObject({ fitBytes: 262_144 });
    expect(buildOutput(cfg)).not.toHaveProperty('fitKeepSize');
    expect(buildOutput(cfg)).not.toHaveProperty('fitKeepFps');
    cfg.fitKeepSize = true;
    cfg.fitKeepFps = true;
    expect(buildOutput(cfg)).toMatchObject({ fitBytes: 262_144, fitKeepSize: true, fitKeepFps: true });
    cfg.fitEnabled = false;
    const off = buildOutput(cfg);
    expect(off).not.toHaveProperty('fitBytes');
    expect(off).not.toHaveProperty('fitKeepSize');
    expect(off).not.toHaveProperty('fitKeepFps');
    expect(fitBytesFor(cfg)).toBe(0);
    cfg.fitEnabled = true;
    cfg.fitKiB = 0;
    expect(fitBytesFor(cfg)).toBe(0);
    cfg.fitKiB = 512;
    expect(fitBytesFor(cfg)).toBe(524_288);
    // sticker keeps its size explicitly
    const st = defaultOutput();
    presetById('sticker').apply(st);
    st.preset = 'sticker';
    expect(buildOutput(st)).toMatchObject({ format: 'apng', colors: 256, fitBytes: 524_288, fitKeepSize: true, target: 'sticker' });
    // static PNG has no fit ladder (jobs.fitFormats): even with the preset's
    // fit left on, the recipe must not carry fitBytes — the server would
    // ignore it and the Result card would falsely claim a fit ran.
    st.format = 'png';
    const png = buildOutput(st);
    expect(png).not.toHaveProperty('fitBytes');
    expect(png).not.toHaveProperty('fitKeepSize');
    expect(png).not.toHaveProperty('fitKeepFps');
    expect(fitBytesFor(st)).toBe(0);
    expect(st.fitEnabled).toBe(true); // switching back to a fit-capable format restores the budget
    st.format = 'apng';
    expect(buildOutput(st)).toMatchObject({ fitBytes: 524_288 });
    // frame extraction never fits
    st.format = 'frames';
    expect(buildOutput(st)).not.toHaveProperty('fitBytes');
  });

  it('emits the knobs of the Phase 2 formats', () => {
    const cfg = defaultOutput();
    cfg.preset = 'custom';
    cfg.target = '';
    cfg.quality = 55;
    cfg.colors = 128;
    cfg.matte = 'ffffff';
    cfg.loop = 2;

    cfg.format = 'apng';
    expect(buildOutput(cfg)).toEqual({ format: 'apng', colors: 128, loop: 2, preset: 'custom' });
    cfg.colors = 0; // RGBA truecolour → no colours key
    expect(buildOutput(cfg)).toEqual({ format: 'apng', loop: 2, preset: 'custom' });

    cfg.format = 'avif';
    expect(buildOutput(cfg)).toEqual({ format: 'avif', quality: 55, loop: 2, preset: 'custom' });

    cfg.format = 'png'; // static: no quality, no loop; colours 0 = full colour
    expect(buildOutput(cfg)).toEqual({ format: 'png', preset: 'custom' });
    cfg.colors = 256; // pngquant palette
    expect(buildOutput(cfg)).toEqual({ format: 'png', colors: 256, preset: 'custom' });
    cfg.colors = 0;

    cfg.format = 'jpeg'; // flattened onto the matte
    expect(buildOutput(cfg)).toEqual({ format: 'jpeg', quality: 55, matte: 'ffffff', preset: 'custom' });

    cfg.format = 'frames';
    cfg.frameFormat = 'png';
    expect(buildOutput(cfg)).toEqual({ format: 'frames', frameFormat: 'png', preset: 'custom' });
    cfg.frameFormat = 'jpeg';
    expect(buildOutput(cfg)).toEqual({ format: 'frames', frameFormat: 'jpeg', quality: 55, matte: 'ffffff', preset: 'custom' });
    cfg.frameFormat = 'webp';
    expect(buildOutput(cfg)).toEqual({ format: 'frames', frameFormat: 'webp', preset: 'custom' });

    // every format serialises to its own name
    for (const f of OUTPUT_FORMATS) {
      cfg.format = f;
      expect(buildOutput(cfg).format).toBe(f);
    }
  });

  it('usesMatte covers GIF, JPEG and JPEG frames', () => {
    expect(usesMatte({ format: 'gif', frameFormat: 'png' })).toBe(true);
    expect(usesMatte({ format: 'jpeg', frameFormat: 'png' })).toBe(true);
    expect(usesMatte({ format: 'frames', frameFormat: 'jpeg' })).toBe(true);
    expect(usesMatte({ format: 'frames', frameFormat: 'png' })).toBe(false);
    expect(usesMatte({ format: 'webp', frameFormat: 'png' })).toBe(false);
    expect(usesMatte({ format: 'apng', frameFormat: 'png' })).toBe(false);
  });
});

describe('setTarget', () => {
  it('moves a budget that sat on the old cap onto the new cap, and leaves any other budget alone', () => {
    const o = defaultOutput();
    presetById('emote').apply(o); // fit on, 256 KiB = the emote cap
    setTarget(o, 'sticker');
    expect(o.target).toBe('sticker');
    expect(o.fitKiB).toBe(512);
    setTarget(o, 'attachment');
    expect(o.fitKiB).toBe(19_531);
    setTarget(o, 'attachment-50');
    expect(o.fitKiB).toBe(Math.floor(50_000_000 / 1024));
    // a hand-set budget is the user's
    o.fitKiB = 300;
    setTarget(o, 'attachment-500');
    expect(o).toMatchObject({ target: 'attachment-500', fitKiB: 300 });
    // no target: the budget is kept as it is
    o.fitKiB = Math.floor(500_000_000 / 1024);
    setTarget(o, '');
    expect(o).toMatchObject({ target: '', fitKiB: Math.floor(500_000_000 / 1024) });
    setTarget(o, 'emote');
    expect(o.fitKiB).toBe(Math.floor(500_000_000 / 1024)); // came from "none", not from a cap
    // the preset is untouched: the dropdown is independent of the chips
    expect(o.preset).toBe('chat');
    expect(buildOutput(o).target).toBe('emote');
  });
});

describe('ops', () => {
  it('serialises the delay op for sequences first after unpremultiply', () => {
    const ops = defaultOps(seqInfo);
    expect(ops.delay).toEqual({ enabled: false, ms: 100 });
    expect(buildOps(ops)).toEqual([]);
    ops.delay = { enabled: true, ms: 40 };
    ops.unpremultiply = true;
    expect(buildOps(ops)).toEqual([{ kind: 'unpremultiply' }, { kind: 'delay', params: { ms: 40 } }]);
    ops.delay.ms = 0.2; // clamped into 1..60000
    expect(buildOps(ops)[1]).toEqual({ kind: 'delay', params: { ms: 1 } });
    ops.delay.ms = 99_999;
    expect(buildOps(ops)[1]).toEqual({ kind: 'delay', params: { ms: 60000 } });
    expect(defaultOps(gifInfo).delay.ms).toBe(100); // default when the source is no sequence
  });

  it('sends no ops for the Optimize preset (gifsicle-only path)', () => {
    const ops = defaultOps(gifInfo);
    ops.trim = { enabled: true, start: 0.5, end: 1.5 };
    ops.fps = { enabled: true, fps: 10 };
    const out = defaultOutput();
    out.preset = 'optimize';
    expect(recipeOps(ops, out)).toEqual([]);
    expect(effectiveOps(ops, out).trim.enabled).toBe(false);
    expect(effectiveFPS(ops, out, 25)).toBe(25); // the fps op is ignored
    out.fps = 12.5;
    expect(effectiveFPS(ops, out, 25)).toBe(12.5); // but Output.fps (frame drop) counts
    out.preset = 'chat';
    out.fps = 0;
    expect(recipeOps(ops, out)).toHaveLength(2);
    expect(effectiveFPS(ops, out, 25)).toBe(10);
    // the preview duration follows: whole clip for Optimize, trimmed otherwise
    expect(previewDuration(gifInfo, effectiveOps(ops, { preset: 'optimize' }))).toBe(2);
    expect(previewDuration(gifInfo, effectiveOps(ops, { preset: 'chat' }))).toBe(1);
  });

  it('turns a deduped re-upload delay into an enabled delay op (UploadZone flow)', () => {
    // Re-uploading identical frames at 40 ms: the server dedupes and answers
    // with the stored 100 ms — UploadZone then installs the delay op, so the
    // recipe and the derived fps/duration honour the requested timing.
    setSource(src(seqInfo)); // seqInfo.sequence.delayMs === 100
    expect(app.ops.delay).toEqual({ enabled: false, ms: 100 });
    const want = sequenceDelayOverride(seqInfo.sequence, 12, 40);
    expect(want).toBe(40);
    app.ops.delay = { enabled: true, ms: want };
    expect(buildOps(app.ops)).toContainEqual({ kind: 'delay', params: { ms: 40 } });
    expect(sourceFPS(seqInfo, app.ops)).toBe(25);
    expect(sourceDuration(seqInfo, app.ops)).toBe(0.48);
    // same files re-uploaded at the stored delay: nothing to override
    expect(sequenceDelayOverride(seqInfo.sequence, 12, 100)).toBe(0);
  });

  it('derives sequence fps / duration from the delay op like the compiler (count / rate)', () => {
    const ops = defaultOps(seqInfo);
    expect(sourceFPS(seqInfo, ops)).toBe(10);
    expect(sourceDuration(seqInfo, ops)).toBe(1.2);
    ops.delay = { enabled: true, ms: 50 };
    expect(sourceFPS(seqInfo, ops)).toBe(20);
    expect(sourceDuration(seqInfo, ops)).toBe(0.6);
    expect(previewDuration(seqInfo, ops)).toBe(0.6);
    // 34 frames at 33 ms: rate round(1000/33, 3) = 30.303 as graph.SequenceFPS, duration count / rate
    ops.delay.ms = 33;
    expect(sourceFPS(seq34, ops)).toBe(30.303);
    expect(sourceDuration(seq34, ops)).toBe(34 / 30.303);
    expect(sourceDuration(seq34, ops)).toBeCloseTo(1.122, 3);
    // a non-sequence ignores the delay op
    expect(sourceFPS(gifInfo, ops)).toBe(25);
    expect(sourceDuration(gifInfo, ops)).toBe(2);
    expect(sourceFPS(null, ops)).toBe(0);
  });

  it('counts source frames: a sequence has its count, a clip duration × fps', () => {
    const ops = defaultOps(seqInfo);
    expect(sourceFrames(seqInfo, ops)).toBe(12);
    ops.delay = { enabled: true, ms: 33 };
    expect(sourceFrames(seq34, ops)).toBe(34);
    expect(sourceFrames(gifInfo, ops)).toBe(50);
    expect(sourceFrames({ ...gifInfo, isStill: true, duration: 0, fps: 0, frames: 1 }, ops)).toBe(1);
    expect(sourceFrames(null, ops)).toBe(0);
  });
});

describe('plan frame grid (contract B: graph.Plan.Frames / Plan.FPS)', () => {
  it('a clip has floor(duration × fps + FRAME_TOLERANCE) frames on the effective fps (rule 4)', () => {
    const ops = defaultOps(gifInfo);
    const out = defaultOutput();
    expect(planFPS(gifInfo, ops, out)).toBe(25);
    expect(planFrames(gifInfo, ops, out)).toBe(50);
    out.fps = 10;
    expect(planFPS(gifInfo, ops, out)).toBe(10);
    expect(planFrames(gifInfo, ops, out)).toBe(20);
    ops.fps = { enabled: true, fps: 12.5 }; // the op wins
    expect(planFrames(gifInfo, ops, out)).toBe(25);
    ops.fps.enabled = false;
    out.fps = 0;
    ops.trim = { enabled: true, start: 0.5, end: 1.5 };
    expect(planFrames(gifInfo, ops, out)).toBe(25);
    ops.speed = { enabled: true, factor: 2 };
    expect(planFrames(gifInfo, ops, out)).toBe(12); // 0.5 s × 25 = 12.5 → 12
    // the review's 4.217 s clip at 24 fps
    const clip = { ...gifInfo, duration: 4.217, fps: 24, frames: 101 };
    expect(planFrames(clip, defaultOps(clip), defaultOutput())).toBe(101);
  });

  it('counts a µs-precise grid trim exactly: frames 3..5 of a 30 fps clip are 3 (graph.FrameTolerance)', () => {
    const clip = { ...gifInfo, fps: 30, duration: 2, frames: 60 };
    const out = defaultOutput();
    const ops = defaultOps(clip);
    ops.trim = { enabled: true, start: trimTime(2 / 30), end: trimTime(5 / 30) }; // -ss 0.066667 -to 0.166667
    expect(planFrames(clip, ops, out)).toBe(3);
    ops.trim = { enabled: true, start: trimTime(1 / 30), end: trimTime(4 / 30) }; // 0.1 s × 30 = 2.99999…
    expect(planFrames(clip, ops, out)).toBe(3);
    // the plan's rate is the 3-decimal one the fps filter gets (graph.SnapFPS)
    const ntsc = { ...gifInfo, fps: 30000 / 1001, duration: 100.1, frames: 3000 };
    expect(planFPS(ntsc, defaultOps(ntsc), out)).toBe(29.97);
    expect(planFrames(ntsc, defaultOps(ntsc), out)).toBe(2999); // 100.1 × 29.97 = 2999.997
  });

  it('an untouched image sequence has exactly its frame count, whatever the delay (review bug 4, rule 1)', () => {
    const out = defaultOutput();
    const ops = defaultOps(seq34);
    expect(planFrames(seq34, ops, out)).toBe(34);
    ops.delay = { enabled: true, ms: 33 };
    expect(planFrames(seq34, ops, out)).toBe(34);
    // a naive floor on the *displayed* (3-decimal) duration lost a frame — that was
    // the bug (FRAME_TOLERANCE now absorbs even that, and the grid model never
    // computes a duration at all)
    expect(Math.floor(round(sourceDuration(seq34, ops), 3) * 30.303)).toBe(33);
    expect(frameCount(round(sourceDuration(seq34, ops), 3), 30.303)).toBe(34);
    for (const ms of [20, 33, 41, 67, 100, 333]) {
      ops.delay.ms = ms;
      expect(planFrames(seq34, ops, out), `${ms} ms`).toBe(34);
      expect(planFPS(seq34, ops, out), `${ms} ms`).toBe(round(1000 / ms, 3));
    }
    // 17 ms = 58.824 fps: untouched for WebP (cap 60), but the GIF 50 fps cap is an fps change
    ops.delay.ms = 17;
    expect(planFrames(seq34, ops, out)).toBe(28); // 0.578 s × 50
    out.format = 'webp';
    expect(planFrames(seq34, ops, out)).toBe(34);
    out.format = 'gif';
    // retimed sequences follow the image2 grid model like the compiler
    ops.delay.ms = 33;
    ops.trim = { enabled: true, start: 0, end: 0.5 };
    expect(planFrames(seq34, ops, out)).toBe(15); // round(0.5 × 30.303) = round(15.15) grid frames
    ops.trim = { enabled: false, start: 0, end: 0 };
    ops.speed = { enabled: true, factor: 2 };
    expect(planFrames(seq34, ops, out)).toBe(17); // trunc(34 / 2)
    ops.speed.enabled = false;
    ops.fps = { enabled: true, fps: 10 };
    expect(planFrames(seq34, ops, out)).toBe(11); // floor(34 × 10 / 30.303)
    ops.fps.enabled = false;
    // the GIF 50 fps cap is an fps change too
    ops.delay.ms = 10;
    expect(planFPS(seq34, ops, out)).toBe(50);
    expect(planFrames(seq34, ops, out)).toBe(17); // floor(34 × 50 / 100)
    out.format = 'webp';
    expect(planFPS(seq34, ops, out)).toBe(60);
    expect(planFrames(seq34, ops, out)).toBe(20); // floor(34 × 60 / 100)
  });

  it('has one frame for a still and none without a rate', () => {
    const still = { ...gifInfo, isStill: true, duration: 0, fps: 0, frames: 1, kind: 'image' as const };
    const out = defaultOutput();
    expect(planFrames(still, defaultOps(still), out)).toBe(0); // no rate at all → nothing to step
    out.fps = 25;
    expect(planFrames(still, defaultOps(still), out)).toBe(1);
    expect(planFrames(null, defaultOps(null), out)).toBe(0);
    expect(planFPS(null, defaultOps(null), out)).toBe(0);
  });

  it('speedFactor is the 3-decimal factor the recipe carries', () => {
    const ops = defaultOps(gifInfo);
    expect(speedFactor(ops)).toBe(1);
    ops.speed = { enabled: true, factor: 1.23456 };
    expect(speedFactor(ops)).toBe(1.235);
    expect(buildOps(ops)).toEqual([{ kind: 'speed', params: { factor: 1.235 } }]);
    ops.speed.factor = 1.0004; // goes out as factor 1: no retiming
    expect(speedFactor(ops)).toBe(1);
    ops.speed.enabled = false;
    expect(speedFactor(ops)).toBe(1);
  });
});

describe('image-sequence frame grid (rules 2 and 3: graph.sequenceSelection / sequenceFrames)', () => {
  // The cases of graph's TestSequenceGridModel and
  // TestSequenceFrameCountsMatchFFmpeg, every one measured on ffmpeg 9:
  // -ss/-to rescale onto the one-frame image2 timebase to the nearest frame
  // (halves away from zero), setpts truncates the end timestamp, the fps
  // stage floors it onto the output grid.
  const seq = (count: number, delayMs: number): ProbeInfo => {
    const rate = round(1000 / delayMs, 3); // graph.SequenceFPS, what the probe reports
    return { ...seqInfo, fps: rate, duration: count / rate, frames: count, sequence: { count, pattern: '%06d.png', delayMs, mixed: false } };
  };
  const webp = (): OutputCfg => {
    const o = defaultOutput();
    o.format = 'webp'; // no GIF 50 fps cap, like the graph tests' webp()
    return o;
  };
  type Edit = (o: OpsCfg) => void;
  const trim = (start: number, end: number): Edit => (o) => (o.trim = { enabled: true, start, end });
  const speed = (factor: number): Edit => (o) => (o.speed = { enabled: true, factor });
  const fps = (f: number): Edit => (o) => (o.fps = { enabled: true, fps: f });
  const cfg = (info: ProbeInfo, ...edits: Edit[]): OpsCfg => {
    const o = defaultOps(info);
    for (const e of edits) e(o);
    return o;
  };

  it('plans the frames ffmpeg renders (graph TestSequenceGridModel)', () => {
    const cases: [string, ProbeInfo, Edit[], number, number][] = [
      ['34 at 33 ms', seq(34, 33), [], 34, 34 / 30.303],
      ['60 at 33 ms', seq(60, 33), [], 60, 60 / 30.303],
      ['1818 at 33 ms (one minute)', seq(1818, 33), [], 1818, 1818 / 30.303],
      ['5000 at 17 ms', seq(5000, 17), [], 5000, 5000 / 58.824],
      ['77 at 999 ms', seq(77, 999), [], 77, 77 / 1.001],
      ['trim 0..0.11 at 25 fps rounds the duration up to 3', seq(60, 40), [trim(0, 0.11)], 3, 0.12],
      ['trim 0..0.09 at 25 fps rounds the duration down to 2', seq(60, 40), [trim(0, 0.09)], 2, 0.08],
      ['trim 0.01..0.09: start rounds to frame 0, 2 frames', seq(60, 40), [trim(0.01, 0.09)], 2, 0.08],
      ['trim 0.03..0.11: start rounds to frame 1, 2 frames', seq(60, 40), [trim(0.03, 0.11)], 2, 0.08],
      ['trim 0.06..0.11: start 1.5 → 2, duration 1.25 → 1', seq(60, 40), [trim(0.06, 0.11)], 1, 0.04],
      ['trim 0.099..0.231 on the 33 ms grid: frames 3..6', seq(34, 33), [trim(3 / 30.303, 7 / 30.303)], 4, 4 / 30.303],
      ['to the end from 1.089 s on the 33 ms grid: the last frame', seq(34, 33), [trim(33 / 30.303, 0)], 1, 1 / 30.303],
      ['to the end from 5.9 s at 10 fps: the last frame', seq(60, 100), [trim(5.9, 0)], 1, 0.1],
      ['trim 0..5.95 at 10 fps rounds to all 60', seq(60, 100), [trim(0, 5.95)], 60, 6],
      ['trim 0..5.94 at 10 fps is 59', seq(60, 100), [trim(0, 5.94)], 59, 5.9],
      ['speed 2 halves 60', seq(60, 100), [speed(2)], 30, 3],
      ['speed 0.5 doubles 60', seq(60, 100), [speed(0.5)], 120, 12],
      ['speed 1.5 on 60 is 40', seq(60, 100), [speed(1.5)], 40, 4],
      ['7 frames at speed 2: the end truncates to tick 3', seq(60, 100), [trim(0, 0.7), speed(2)], 3, 0.35],
      ['7 frames at speed 2 resampled to 20 fps: 6, not 7', seq(60, 100), [trim(0, 0.7), speed(2), fps(20)], 6, 0.35],
      ['fps 25 from 10 fps', seq(60, 100), [fps(25)], 150, 6],
      ['speed 2 then fps 25', seq(60, 100), [speed(2), fps(25)], 75, 3],
      ['trim 1..3 at speed 2', seq(60, 100), [trim(1, 3), speed(2)], 10, 1],
      ['1 ms delay capped to 60 fps', seq(60, 1), [], 3, 0.06],
      ['60 s delay', seq(60, 60000), [], 60, 60 / 0.017],
      // the ffmpeg-verified set (phase2_ffmpeg_test.go) on the 34-frame sequence
      ['34 frames at 33 ms render 34 master frames', seq(34, 33), [], 34, 34 / 30.303],
      ['to the end from 3.3 s at 10 fps: the last frame', seq(34, 100), [trim(3.3, 0)], 1, 0.1],
      ['speed 2 halves 34 frames to 17', seq(34, 100), [speed(2)], 17, 1.7],
      ['fps 25 from 10 fps: floor(34 × 2.5) = 85', seq(34, 100), [fps(25)], 85, 3.4],
    ];
    for (const [name, info, edits, frames, dur] of cases) {
      const ops = cfg(info, ...edits);
      const out = webp();
      expect(planFrames(info, ops, out), name).toBe(frames);
      expect(previewDuration(info, ops), name).toBeCloseTo(dur, 9);
      // the frames never outrun the duration (graph's own invariant)
      expect(planFrames(info, ops, out) / planFPS(info, ops, out), name).toBeLessThanOrEqual(previewDuration(info, ops) + 1e-9);
    }
  });

  it('retimes through the delay op the same way (probed at 100 ms, retimed to 33 ms)', () => {
    const out = webp();
    const ops = cfg(seq34, (o) => (o.delay = { enabled: true, ms: 33 }));
    expect(planFrames(seq34, ops, out)).toBe(34);
    ops.trim = { enabled: true, start: 3 / 30.303, end: 7 / 30.303 };
    expect(planFrames(seq34, ops, out)).toBe(4);
    expect(sourceSpan(seq34, ops)).toEqual({ first: 4, last: 7 });
    ops.trim = { enabled: true, start: 33 / 30.303, end: 0 };
    expect(planFrames(seq34, ops, out)).toBe(1);
    expect(sourceSpan(seq34, ops)).toEqual({ first: 34, last: 34 });
  });

  it('keeps one notch where the graph rejects the recipe (a trim or speed that leaves no frame)', () => {
    const s = seq(60, 100);
    const out = webp();
    expect(sequenceSelection(60, 10, 0.1, 0.14)).toEqual({ first: 1, selected: 0 }); // shorter than half a frame
    expect(planFrames(s, cfg(s, trim(0.1, 0.14)), out)).toBe(1);
    expect(sourceSpan(s, cfg(s, trim(0.1, 0.14)))).toEqual({ first: 2, last: 2 });
    expect(sequenceFrames(1, 2, 10, 10)).toBe(0); // "speed 2 leaves no frame"
    expect(planFrames(s, cfg(s, trim(0, 0.1), speed(2)), out)).toBe(1);
    // a single-frame sequence is one frame whatever the ops (graph.singleFrame)
    const one = seq(1, 100);
    expect(planFrames(one, cfg(one, speed(2), fps(25)), out)).toBe(1);
  });

  it('sequenceSelection snaps the bounds to the nearest grid frame (first, selected)', () => {
    expect(sequenceSelection(60, 25, 0.06, 0.11)).toEqual({ first: 2, selected: 1 });
    expect(sequenceSelection(60, 25, 0.03, 0.11)).toEqual({ first: 1, selected: 2 });
    expect(sequenceSelection(34, 30.303, trimTime(3 / 30.303), trimTime(7 / 30.303))).toEqual({ first: 3, selected: 4 });
    expect(sequenceSelection(34, 30.303, trimTime(33 / 30.303), 0)).toEqual({ first: 33, selected: 1 });
    expect(sequenceSelection(60, 10, 5.96, 0)).toEqual({ first: 60, selected: 0 }); // rounds past the last frame: rejected
    expect(sequenceSelection(60, 10, 0, 0)).toEqual({ first: 0, selected: 60 });
    expect(sequenceSelection(60, 10, 0, 99)).toEqual({ first: 0, selected: 60 }); // an end past the sequence keeps all
    expect(sequenceSelection(0, 10, 0, 0)).toEqual({ first: 0, selected: 0 });
    expect(sequenceSelection(12, 0, 0.5, 0)).toEqual({ first: 0, selected: 12 }); // no rate: nothing to snap to
  });

  it('gridRound is ffmpeg av_rescale onto the frame grid: nearest, halves away from zero, exact', () => {
    for (const [us, rate1000, want] of [
      [0, 10000, 0],
      [-5, 10000, 0],
      [49999, 10000, 0],
      [50000, 10000, 1],
      [60000, 10000, 1],
      [149999, 10000, 1],
      [150000, 10000, 2],
      [99000, 30303, 3],
      [132000, 30303, 4],
      [1089000, 30303, 33],
      [5950000, 10000, 60],
      [5940000, 10000, 59],
      [10000, 25000, 0],
      [30000, 25000, 1],
      [60000, 25000, 2],
      [110000, 25000, 3],
      [90000, 25000, 2],
      [19000, 25000, 0],
      [60000 * 1000 * 5000, 17, 5100], // 5000 frames at 60 s: 300000 s × 0.017
    ]) {
      expect(gridRound(us, rate1000), `${us} µs at ${rate1000 / 1000} fps`).toBe(want);
    }
    expect(micros(trimTime(2 / 30))).toBe(66667);
    expect(micros(0.2)).toBe(200000);
    expect(micros(0)).toBe(0);
  });

  it('sequenceFrames ends the stream like setpts + fps round=down', () => {
    for (const [n, speed, rate, fps, want] of [
      [34, 1, 30.303, 30.303, 34],
      [7, 2, 10, 10, 3],
      [7, 2, 10, 20, 6],
      [60, 1.5, 10, 10, 40],
      [60, 1, 10, 25, 150],
      [60, 1, 1000, 60, 3],
      [1, 2, 10, 10, 0],
      [60, 100, 10, 10, 0],
      [0, 1, 10, 10, 0],
      [60, 1, 0.017, 0.017, 60],
    ]) {
      expect(sequenceFrames(n, speed, rate, fps), `${n} frames at ${rate} fps, speed ${speed}, ${fps} fps out`).toBe(want);
    }
  });

  it('sourceSpan labels a sequence with the grid frames the graph selects, a clip with the frames its window covers', () => {
    const s = seq(60, 40); // 25 fps
    expect(sourceSpan(s, cfg(s))).toEqual({ first: 1, last: 60 });
    expect(sourceSpan(s, cfg(s, trim(0.06, 0.11)))).toEqual({ first: 3, last: 3 }); // ffmpeg: frame 3 alone
    expect(sourceSpan(s, cfg(s, trim(0.03, 0.11)))).toEqual({ first: 2, last: 3 });
    expect(sourceSpan(s, cfg(s, trim(0, 0.11)))).toEqual({ first: 1, last: 3 });
    expect(sourceSpan(s, cfg(s, trim(2.36, 0)))).toEqual({ first: 60, last: 60 }); // 2.36 × 25 = 59
    // a clip: the frames the µs window covers on its source grid
    const clip = { ...gifInfo, fps: 30, duration: 2, frames: 60 };
    expect(sourceSpan(clip, cfg(clip, trim(2 / 30, 5 / 30)))).toEqual({ first: 3, last: 5 });
    expect(sourceSpan(clip, cfg(clip, trim(0.06, 0.11)))).toEqual({ first: 2, last: 4 });
    expect(sourceSpan(clip, cfg(clip))).toEqual({ first: 1, last: 60 });
  });
});

describe('frameWindow (Trim "from scrubber")', () => {
  const out = defaultOutput();

  it('sends microsecond bounds, never milliseconds (rule A): frames 3..5 of a 30 fps clip', () => {
    const clip = { ...gifInfo, fps: 30, duration: 2, frames: 60 };
    const ops = defaultOps(clip);
    expect(planFrames(clip, ops, out)).toBe(60);
    // frame 3 (index 2) starts at 2/30 and frame 5 (index 4) ends at 5/30: whole µs, not 0.067 / 0.167
    expect(frameWindow(clip, ops, out, 2)).toEqual({ start: 0.066667, end: 0.1 });
    expect(frameWindow(clip, ops, out, 4)).toEqual({ start: 0.133333, end: 0.166667 });
    ops.trim = { enabled: true, start: frameWindow(clip, ops, out, 2).start, end: frameWindow(clip, ops, out, 4).end };
    // what goes on the wire, as is (graph rounds to the same 6 decimals)
    expect(buildOps(ops)).toEqual([{ kind: 'trim', params: { start: 0.066667, end: 0.166667 } }]);
    expect(trimRange(clip, ops)).toEqual({ start: 0.066667, end: 0.166667 });
    // and the plan has exactly the 3 frames ffmpeg renders for -ss 0.066667 -to 0.166667
    expect(planFrames(clip, ops, out)).toBe(3);
    expect(previewDuration(clip, ops)).toBeCloseTo(0.1, 9);
    expect(sourceSpan(clip, ops)).toEqual({ first: 3, last: 5 });
    // the scrubber over the trimmed range maps back onto the same source frames (the
    // start's own µs rounding carries into later frames: 0.066667 + 2/30 = 0.1333337)
    expect(frameWindow(clip, ops, out, 0)).toEqual({ start: 0.066667, end: 0.1 });
    expect(frameWindow(clip, ops, out, 2)).toEqual({ start: 0.133334, end: 0.166667 });
    // hand-typed values are sent with whatever precision they have, up to the µs
    ops.trim = { enabled: true, start: 0.25, end: 1.1234567 };
    expect(buildOps(ops)).toEqual([{ kind: 'trim', params: { start: 0.25, end: 1.123457 } }]);
  });

  it('maps plan frame i to [i/fps, (i+1)/fps) in source seconds; the last frame ends "to the end"', () => {
    const ops = defaultOps(gifInfo);
    expect(frameWindow(gifInfo, ops, out, 0)).toEqual({ start: 0, end: 0.04 });
    expect(frameWindow(gifInfo, ops, out, 12)).toEqual({ start: 0.48, end: 0.52 });
    expect(frameWindow(gifInfo, ops, out, 49)).toEqual({ start: 1.96, end: 0 });
    expect(frameWindow(gifInfo, ops, out, 99)).toEqual({ start: 1.96, end: 0 }); // clamped to the last frame
    expect(frameWindow(gifInfo, ops, out, -3)).toEqual({ start: 0, end: 0.04 });
  });

  it('never yields a start at or beyond the end of the source (review bug 3)', () => {
    // 4.217 s at 24 fps: the old time-based scrubber could reach 4.22 s
    const clip = { ...gifInfo, duration: 4.217, fps: 24, frames: 101 };
    const ops = defaultOps(clip);
    const n = planFrames(clip, ops, out);
    expect(n).toBe(101);
    const w = frameWindow(clip, ops, out, n - 1);
    expect(w.start).toBeLessThan(4.217);
    expect(w.start).toBeLessThanOrEqual(trimStartMax(clip, ops));
    expect(w.end).toBe(0);
    expect(trimStartMax(clip, ops)).toBe(trimTime(4.217 - 1 / 24)); // 4.175333: whole µs like every trim bound
    expect(trimStartMax(clip, ops)).toBe(4.175333);
    for (let i = 0; i < n; i++) expect(frameWindow(clip, ops, out, i).start, `frame ${i}`).toBeLessThan(4.217);
    // the last frame's start is never displaced by the clamp: 23/24 on a 1 s clip is
    // 0.958333, which a ms-rounded cap (0.958) used to pull back
    const one = { ...gifInfo, duration: 1, fps: 24, frames: 24 };
    expect(trimStartMax(one, defaultOps(one))).toBe(0.958333);
    expect(frameWindow(one, defaultOps(one), out, 23)).toEqual({ start: 0.958333, end: 0 });
  });

  it('goes through the active trim and speed', () => {
    const ops = defaultOps(gifInfo);
    ops.trim = { enabled: true, start: 0.5, end: 1.5 };
    expect(planFrames(gifInfo, ops, out)).toBe(25);
    expect(frameWindow(gifInfo, ops, out, 0)).toEqual({ start: 0.5, end: 0.54 });
    // the last frame of a trimmed range ends at the range end, not at the clip end
    expect(frameWindow(gifInfo, ops, out, 24)).toEqual({ start: 1.46, end: 1.5 });
    ops.speed = { enabled: true, factor: 2 }; // 12 frames over 0.5 s of output = 1 s of source
    expect(planFrames(gifInfo, ops, out)).toBe(12);
    expect(frameWindow(gifInfo, ops, out, 0)).toEqual({ start: 0.5, end: 0.58 });
    expect(frameWindow(gifInfo, ops, out, 11)).toEqual({ start: 1.38, end: 1.5 });
    // a trim that reaches the clip end reports "to the end"
    ops.speed.enabled = false;
    ops.trim = { enabled: true, start: 1, end: 0 };
    expect(frameWindow(gifInfo, ops, out, 24)).toEqual({ start: 1.96, end: 0 });
  });

  it('uses the sequence count for an untouched sequence', () => {
    const ops = defaultOps(seq34);
    ops.delay = { enabled: true, ms: 33 };
    // the last frame starts at 33/30.303 = 1.089001 s (µs, not the ms-rounded 1.089)
    expect(frameWindow(seq34, ops, out, 33)).toEqual({ start: trimTime(33 / 30.303), end: 0 });
    expect(frameWindow(seq34, ops, out, 33)).toEqual({ start: 1.089001, end: 0 });
    expect(frameWindow(seq34, ops, out, 0)).toEqual({ start: 0, end: 0.033 });
    // both land on the grid frame they name (graph.sequenceSelection)
    expect(sequenceSelection(34, 30.303, 1.089001, 0)).toEqual({ first: 33, selected: 1 });
    expect(sequenceSelection(34, 30.303, 0, 0.033)).toEqual({ first: 0, selected: 1 });
  });
});

describe('setSource / resetApp', () => {
  it('falls back from Optimize when the new source is not a GIF', () => {
    setSource(src(gifInfo));
    applyPreset('optimize');
    expect(app.output.preset).toBe('optimize');
    setSource(src({ ...gifInfo, codec: 'prores', format: 'mov,mp4', kind: 'video' }, 'b'.repeat(64)));
    expect(app.output.preset).toBe('chat');
    expect(app.output.target).toBe('attachment');
    // and keeps a preset the new source can use
    applyPreset('emote');
    setSource(src(gifInfo, 'c'.repeat(64)));
    expect(app.output.preset).toBe('emote');
    setSource(null);
    expect(app.output.preset).toBe('emote');
  });

  it('a new source rewinds the scrubber and closes crop mode', () => {
    setSource(src(gifInfo));
    app.ui.scrubFrame = 30;
    app.ui.cropOpen = true;
    setSource(src(gifInfo, 'd'.repeat(64)));
    expect(app.ui.scrubFrame).toBe(0);
    expect(app.ui.cropOpen).toBe(false);
  });

  it('resetApp returns to the landing state (header logo)', () => {
    setSource(src(gifInfo));
    applyPreset('emote');
    app.output.target = 'attachment-500';
    app.ops.trim = { enabled: true, start: 0.5, end: 1 };
    app.ui.scrubFrame = 7;
    app.ui.backdrop = 'white';
    resetApp();
    expect(app.source).toBeNull();
    expect(app.ops).toEqual(defaultOps(null));
    expect(app.output).toEqual(defaultOutput());
    expect(app.ui.scrubFrame).toBe(0);
    expect(app.ui.cropOpen).toBe(false);
    expect(app.ui.backdrop).toBe('white'); // a viewing preference, not document state
  });
});

describe('effectiveFPS', () => {
  // graph.Compile: fps op → Output.fps → source fps, then SnapFPS(format).
  it('lets an enabled fps op win over Output.fps, which wins over the source', () => {
    const ops = defaultOps(null);
    const out = defaultOutput();
    out.format = 'gif';
    out.fps = 0;
    expect(effectiveFPS(ops, out, 24)).toBe(24); // source
    out.fps = 30;
    expect(effectiveFPS(ops, out, 24)).toBe(30); // output fps
    ops.fps = { enabled: true, fps: 12 };
    expect(effectiveFPS(ops, out, 24)).toBe(12); // the op has precedence
    ops.fps.enabled = false;
    expect(effectiveFPS(ops, out, 24)).toBe(30);
    ops.fps = { enabled: true, fps: 0 }; // an op without a rate does not count
    expect(effectiveFPS(ops, out, 24)).toBe(30);
  });

  it('snaps for the output format', () => {
    const ops = defaultOps(null);
    const out = defaultOutput();
    out.fps = 0;
    out.format = 'gif';
    expect(effectiveFPS(ops, out, 60)).toBe(50);
    out.format = 'webp';
    expect(effectiveFPS(ops, out, 60)).toBe(60);
    expect(effectiveFPS(ops, out, 120)).toBe(60);
    ops.fps = { enabled: true, fps: 59 };
    out.format = 'gif';
    expect(effectiveFPS(ops, out, 24)).toBe(50);
    expect(effectiveFPS(ops, out, 0)).toBe(50);
    ops.fps.enabled = false;
    expect(effectiveFPS(ops, out, 0)).toBe(0); // nothing known
  });
});
