import { describe, expect, it } from 'vitest';
import { OUTPUT_FORMATS, type PresetId, type ProbeInfo, type SequenceInfo } from './api';
import {
  chatPresetFor,
  defaultOutput,
  FIT_FORMATS,
  FIT_KIB,
  fitsFormat,
  formatsFor,
  isGifSource,
  LIMITS,
  presetAvailable,
  PRESETS,
  presetById,
  TRIM_FRINGE_THRESHOLD,
} from './presets';

const gifInfo: ProbeInfo = {
  format: 'gif',
  codec: 'gif',
  pixFmt: 'bgra',
  bits: 8,
  width: 64,
  height: 64,
  fps: 25,
  duration: 1,
  frames: 25,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'animation',
  premultiplied: false,
};
const movInfo: ProbeInfo = { ...gifInfo, format: 'mov,mp4,m4a,3gp,3g2,mj2', codec: 'prores', profile: '4444', pixFmt: 'yuva444p10le', bits: 10, kind: 'video', premultiplied: true };

function applied(id: PresetId, info: ProbeInfo | null = null) {
  const o = defaultOutput();
  o.preset = id;
  presetById(id).apply(o, info);
  return o;
}

describe('presets', () => {
  it('offers exactly the Phase 2 set, in order', () => {
    expect(PRESETS.map((p) => p.id)).toEqual(['emote', 'sticker', 'chat-gif', 'chat-webp', 'chat-avif', 'optimize', 'frames', 'custom']);
  });

  it('Emote: GIF, 128×128 contain, fit 256 KiB on, target emote, WebP one click away', () => {
    const o = applied('emote');
    expect(o).toMatchObject({ format: 'gif', target: 'emote', width: 128, height: 128, fit: 'contain', fitEnabled: true, fitKiB: 256 });
    expect(o.fitKiB * 1024).toBe(LIMITS.emote);
    const p = presetById('emote');
    expect(p.swap).toEqual({ format: 'webp', label: 'WebP instead', hint: 'keeps soft edges — verified on Discord' });
    expect(p.formats).not.toContain('apng');
    expect(p.locksSize).toBe(true);
  });

  it('Sticker: indexed APNG (256 colours), 320×320 contain, fit 512 KiB on, GIF one click away', () => {
    const o = applied('sticker');
    expect(o).toMatchObject({ format: 'apng', colors: 256, target: 'sticker', width: 320, height: 320, fit: 'contain', fitEnabled: true, fitKiB: 512, fitKeepSize: true });
    expect(o.fitKiB * 1024).toBe(LIMITS.sticker);
    const p = presetById('sticker');
    expect(p.swap?.format).toBe('gif');
    expect(p.swap?.label).toBe('GIF instead');
    expect(p.warn).toMatch(/shrinks/i);
    expect(p.warn).toMatch(/server sticker/i);
  });

  it('Chat presets: attachment target, fit off, source size', () => {
    expect(applied('chat-gif')).toMatchObject({ format: 'gif', target: 'attachment', width: 0, height: 0, fps: 0, dither: 'sierra2_4a', lossy: 20, fitEnabled: false });
    expect(applied('chat-webp')).toMatchObject({ format: 'webp', target: 'attachment', quality: 80, lossless: false, fitEnabled: false });
    expect(applied('chat-avif')).toMatchObject({ format: 'avif', target: 'attachment', quality: 60, fitEnabled: false });
    expect(presetById('chat-avif').hint).toMatch(/soft alpha, verified on Discord attachments/);
    expect(chatPresetFor('gif')).toBe('chat-gif');
    expect(chatPresetFor('webp')).toBe('chat-webp');
    expect(chatPresetFor('avif')).toBe('chat-avif');
    expect(chatPresetFor('apng')).toBeNull();
    expect(chatPresetFor('png')).toBeNull();
  });

  it('Optimize: GIF only, no ops, only for GIF sources', () => {
    const p = presetById('optimize');
    expect(p.usesOps).toBe(false);
    expect(p.formats).toEqual(['gif']);
    expect(presetAvailable(p, gifInfo)).toBe(true);
    expect(presetAvailable(p, movInfo)).toBe(false);
    expect(presetAvailable(p, null)).toBe(false);
    expect(p.unavailableHint).toBeTruthy();
    expect(applied('optimize', gifInfo)).toMatchObject({ format: 'gif', width: 0, height: 0, fps: 0, fitEnabled: false });
    // every other preset is always available
    for (const q of PRESETS) if (q.id !== 'optimize') expect(presetAvailable(q, null), q.id).toBe(true);
  });

  it('isGifSource requires an animated GIF file — never a sequence, never a still (W11)', () => {
    expect(isGifSource(gifInfo)).toBe(true);
    expect(isGifSource({ ...gifInfo, codec: 'gif', format: 'gif' })).toBe(true);
    expect(isGifSource(movInfo)).toBe(false);
    expect(isGifSource(null)).toBe(false);
    // an image sequence made of .gif frames is rejected by the server's
    // optimiser with a 400, so Optimize must stay disabled for it
    const seq: SequenceInfo = { count: 3, pattern: '%06d.gif', delayMs: 100, mixed: false };
    expect(isGifSource({ ...gifInfo, kind: 'sequence', sequence: seq })).toBe(false);
    expect(isGifSource({ ...gifInfo, sequence: seq })).toBe(false); // defensive: sequence info alone disables it
    expect(presetAvailable(presetById('optimize'), { ...gifInfo, kind: 'sequence', sequence: seq })).toBe(false);
    // a still image has no animation to optimise
    expect(isGifSource({ ...gifInfo, kind: 'image', isStill: true, frames: 1 })).toBe(false);
  });

  it('Frames: frames format, png per frame, uses the op stack', () => {
    const p = presetById('frames');
    expect(p.usesOps).toBe(true);
    expect(p.formats).toEqual(['frames']);
    expect(applied('frames')).toMatchObject({ format: 'frames', frameFormat: 'png', target: '', fitEnabled: false });
  });

  it('Custom offers every format and keeps the current values', () => {
    const p = presetById('custom');
    expect([...p.formats].sort()).toEqual([...OUTPUT_FORMATS].sort());
    const o = applied('emote');
    o.preset = 'custom';
    p.apply(o);
    expect(o).toMatchObject({ format: 'gif', width: 128, target: 'emote' });
  });

  it('APNG is offered only for the Sticker preset and Custom', () => {
    for (const p of PRESETS) {
      const offers = p.formats.includes('apng');
      expect(offers, p.id).toBe(p.id === 'sticker' || p.id === 'custom');
    }
  });

  it('gifski never appears in the UI', () => {
    const text = JSON.stringify(PRESETS.map((p) => [p.id, p.label, p.hint, p.warn, p.swap, p.unavailableHint]));
    expect(text.toLowerCase()).not.toContain('gifski');
  });

  it('formatsFor always includes the current format', () => {
    expect(formatsFor(presetById('emote'), 'gif')).toEqual(['gif', 'webp', 'avif', 'png']);
    expect(formatsFor(presetById('emote'), 'jpeg')).toEqual(['gif', 'webp', 'avif', 'png', 'jpeg']);
    expect(formatsFor(presetById('optimize'), 'gif')).toEqual(['gif']);
  });

  it('fit budgets match the Discord caps', () => {
    expect(FIT_KIB.emote * 1024).toBe(LIMITS.emote);
    expect(FIT_KIB.sticker * 1024).toBe(LIMITS.sticker);
    expect(TRIM_FRINGE_THRESHOLD).toBe(180);
  });

  it('FIT_FORMATS mirrors the server fit engine (internal/jobs/fit.go fitFormats)', () => {
    expect([...FIT_FORMATS].sort()).toEqual(['apng', 'avif', 'gif', 'jpeg', 'webp']);
    for (const f of OUTPUT_FORMATS) expect(fitsFormat(f), f).toBe(f !== 'png' && f !== 'frames');
  });
});
