// Phase 4 Output serialisation: MP4/WebM (opaque video, quality = CRF,
// matte, fit), the gifski encoder toggle, and the pure target/format
// eligibility helpers the Output card gates on.
import { describe, expect, it } from 'vitest';
import { isVideoFormat } from './api';
import {
  defaultOutput,
  formatAllowedForTarget,
  gifskiAllowed,
  targetDefsFor,
  videoCRF,
  type OutputCfg,
} from './presets';
import { buildOutput, fitBytesFor, loopFor, usesMatte } from './state.svelte';

function cfg(over: Partial<OutputCfg>): OutputCfg {
  return { ...defaultOutput(), ...over };
}

describe('MP4 / WebM output (opaque video)', () => {
  it('emits format, the CRF as quality (only when set) and the matte', () => {
    const o = buildOutput(cfg({ format: 'mp4', target: 'attachment', quality: 0, preset: 'chat' }));
    expect(o).toEqual({ format: 'mp4', matte: '313338', preset: 'chat', target: 'attachment' });
    const q = buildOutput(cfg({ format: 'webm', target: '', quality: 35, matte: 'ffffff', preset: 'custom' }));
    expect(q.quality).toBe(35);
    expect(q.matte).toBe('ffffff');
    expect(q.format).toBe('webm');
  });

  it('clamps the CRF into the encoder range (x264 ≤ 51, vp9 ≤ 63)', () => {
    expect(buildOutput(cfg({ format: 'mp4', quality: 80 })).quality).toBe(51);
    expect(buildOutput(cfg({ format: 'webm', quality: 80 })).quality).toBe(63);
  });

  it('never carries gif/webp knobs, loop or alpha fields', () => {
    const o = buildOutput(cfg({ format: 'mp4', target: 'attachment', loop: 3, lossy: 20, colors: 128, alphaThreshold: 180 }));
    expect(o.colors).toBeUndefined();
    expect(o.dither).toBeUndefined();
    expect(o.lossy).toBeUndefined();
    expect(o.alphaThreshold).toBeUndefined();
    expect(o.loop).toBeUndefined(); // video has no loop semantics
    expect(o.lossless).toBeUndefined();
  });

  it('carries a fit budget (the server searches the CRF knob)', () => {
    const c = cfg({ format: 'mp4', target: 'attachment', fitEnabled: true, fitKiB: 8192 });
    expect(fitBytesFor(c)).toBe(8192 * 1024);
    expect(buildOutput(c).fitBytes).toBe(8192 * 1024);
  });

  it('uses the matte (flattened) and is not loop-editable', () => {
    expect(usesMatte({ format: 'mp4', frameFormat: 'png' })).toBe(true);
    expect(usesMatte({ format: 'webm', frameFormat: 'png' })).toBe(true);
    expect(loopFor({ target: '', loop: 2 })).toBe(2); // loopFor itself is format-agnostic…
    expect(buildOutput(cfg({ format: 'webm', target: '', loop: 2 })).loop).toBeUndefined(); // …but video never emits it
  });
});

describe('video target eligibility', () => {
  it('mp4/webm go with none and the attachment tiers, never emote/sticker', () => {
    for (const f of ['mp4', 'webm'] as const) {
      expect(formatAllowedForTarget(f, '')).toBe(true);
      expect(formatAllowedForTarget(f, 'attachment')).toBe(true);
      expect(formatAllowedForTarget(f, 'attachment-500')).toBe(true);
      expect(formatAllowedForTarget(f, 'emote')).toBe(false);
      expect(formatAllowedForTarget(f, 'sticker')).toBe(false);
    }
    expect(formatAllowedForTarget('gif', 'emote')).toBe(true);
    expect(targetDefsFor('mp4').map((t) => t.id)).toEqual(['', 'attachment', 'attachment-50', 'attachment-100', 'attachment-500']);
    expect(targetDefsFor('gif').map((t) => t.id)).toContain('emote');
  });

  it('videoCRF describes the knob (and only for video)', () => {
    expect(videoCRF('mp4')).toEqual({ max: 51, default: 20, label: 'x264 CRF' });
    expect(videoCRF('webm')).toEqual({ max: 63, default: 30, label: 'VP9 CRF' });
    expect(videoCRF('gif')).toBeNull();
    expect(isVideoFormat('mp4')).toBe(true);
    expect(isVideoFormat('gif')).toBe(false);
  });
});

describe('gifski encoder', () => {
  it('is allowed for gif with target none / attachment tiers only', () => {
    expect(gifskiAllowed({ format: 'gif', target: '' })).toBe(true);
    expect(gifskiAllowed({ format: 'gif', target: 'attachment' })).toBe(true);
    expect(gifskiAllowed({ format: 'gif', target: 'attachment-100' })).toBe(true);
    expect(gifskiAllowed({ format: 'gif', target: 'emote' })).toBe(false);
    expect(gifskiAllowed({ format: 'gif', target: 'sticker' })).toBe(false);
    expect(gifskiAllowed({ format: 'webp', target: '' })).toBe(false);
    // the gifsicle-only Optimize path never re-encodes: no gifski there
    expect(gifskiAllowed({ format: 'gif', target: '', preset: 'optimize' })).toBe(false);
  });

  it('buildOutput emits encoder + quality and drops the ffmpeg-palette knobs', () => {
    const o = buildOutput(cfg({ format: 'gif', target: '', encoder: 'gifski', quality: 90, preset: 'custom' }));
    expect(o).toEqual({ format: 'gif', encoder: 'gifski', quality: 90, preset: 'custom' });
    // quality 0 = the server default 90 and is omitted
    expect(buildOutput(cfg({ format: 'gif', target: 'attachment', encoder: 'gifski', quality: 0 })).quality).toBeUndefined();
  });

  it('drops a stale gifski toggle on an emote/sticker target (the server would 400)', () => {
    const o = buildOutput(cfg({ format: 'gif', target: 'emote', encoder: 'gifski' }));
    expect(o.encoder).toBeUndefined();
    expect(o.colors).toBe(256); // the normal palette path rides again
    expect(o.matte).toBe('313338');
  });

  it('a plain gif recipe is byte-identical to Phase 3 (encoder omitted)', () => {
    const o = buildOutput(cfg({ format: 'gif', target: 'attachment' }));
    expect(o.encoder).toBeUndefined();
    expect(o.colors).toBe(256);
    expect(o.dither).toBe('sierra2_4a');
  });
});
