import { describe, expect, it } from 'vitest';
import { OUTPUT_FORMATS, type OutputFormat, type ProbeInfo, type Source, type Target } from './api';
import { sequenceDelayOverride } from './files';
import { defaultOutput, PRESETS, presetById } from './presets';
import {
  app,
  applyPreset,
  buildOps,
  buildOutput,
  defaultOps,
  effectiveFPS,
  effectiveOps,
  fitBytesFor,
  loopFor,
  previewDuration,
  recipeOps,
  setSource,
  sourceDuration,
  sourceFPS,
  usesMatte,
} from './state.svelte';

const FORMATS: OutputFormat[] = ['gif', 'webp'];
const DISCORD_TARGETS: Target[] = ['emote', 'sticker', 'attachment'];

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
const src = (info: ProbeInfo, hash = 'a'.repeat(64)): Source => ({ hash, name: 'x', size: 1, info });

describe('buildOutput', () => {
  // Every Discord target requires loop forever (DESIGN §5.3; discordlint keeps
  // gif.netscape-loop / webp.loop-forever as errors for them), so whatever the
  // Loop control holds, a Discord target sends loop 0 — i.e. no `loop` key
  // (0 is the recipe zero value and omitted from the JSON).
  it('never carries a loop count for a Discord target', () => {
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
    cfg.target = 'attachment';
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

  it('carries the Discord target only when one is set', () => {
    const cfg = defaultOutput();
    presetById('emote').apply(cfg);
    cfg.preset = 'emote';
    expect(buildOutput(cfg)).toMatchObject({ format: 'gif', width: 128, height: 128, fit: 'contain', target: 'emote', preset: 'emote' });
    cfg.preset = 'custom';
    cfg.target = '';
    expect(buildOutput(cfg)).not.toHaveProperty('target');
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
    out.preset = 'chat-gif';
    out.fps = 0;
    expect(recipeOps(ops, out)).toHaveLength(2);
    expect(effectiveFPS(ops, out, 25)).toBe(10);
    // the preview duration follows: whole clip for Optimize, trimmed otherwise
    expect(previewDuration(gifInfo, effectiveOps(ops, { preset: 'optimize' }))).toBe(2);
    expect(previewDuration(gifInfo, effectiveOps(ops, { preset: 'chat-gif' }))).toBe(1);
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

  it('derives sequence fps / duration from the delay op', () => {
    const ops = defaultOps(seqInfo);
    expect(sourceFPS(seqInfo, ops)).toBe(10);
    expect(sourceDuration(seqInfo, ops)).toBe(1.2);
    ops.delay = { enabled: true, ms: 50 };
    expect(sourceFPS(seqInfo, ops)).toBe(20);
    expect(sourceDuration(seqInfo, ops)).toBe(0.6);
    expect(previewDuration(seqInfo, ops)).toBe(0.6);
    // a non-sequence ignores the delay op
    expect(sourceFPS(gifInfo, ops)).toBe(25);
    expect(sourceDuration(gifInfo, ops)).toBe(2);
    expect(sourceFPS(null, ops)).toBe(0);
  });
});

describe('setSource', () => {
  it('falls back from Optimize when the new source is not a GIF', () => {
    setSource(src(gifInfo));
    applyPreset('optimize');
    expect(app.output.preset).toBe('optimize');
    setSource(src({ ...gifInfo, codec: 'prores', format: 'mov,mp4', kind: 'video' }, 'b'.repeat(64)));
    expect(app.output.preset).toBe('chat-gif');
    expect(app.output.target).toBe('attachment');
    // and keeps a preset the new source can use
    applyPreset('emote');
    setSource(src(gifInfo, 'c'.repeat(64)));
    expect(app.output.preset).toBe('emote');
    setSource(null);
    expect(app.output.preset).toBe('emote');
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
