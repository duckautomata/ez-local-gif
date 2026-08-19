import { describe, expect, it } from 'vitest';
import type { OutputFormat, Target } from './api';
import { defaultOutput, PRESETS, presetById } from './presets';
import { buildOutput, defaultOps, effectiveFPS, loopFor } from './state.svelte';

const FORMATS: OutputFormat[] = ['gif', 'webp'];
const DISCORD_TARGETS: Target[] = ['emote', 'sticker', 'attachment'];

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
