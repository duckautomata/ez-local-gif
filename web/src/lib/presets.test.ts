import { describe, expect, it } from 'vitest';
import { OUTPUT_FORMATS, type PresetId, type ProbeInfo, type SequenceInfo, type Target } from './api';
import {
  DEFAULT_FIT_KIB,
  defaultOutput,
  FIT_FORMATS,
  FIT_KIB,
  fitKiBFor,
  fitsFormat,
  FORMAT_HINT,
  formatHint,
  formatsFor,
  isAttachmentTarget,
  isGifSource,
  limitKiB,
  limitOf,
  LIMITS,
  presetAvailable,
  PRESETS,
  presetById,
  TARGET_DEFS,
  TARGET_LABEL,
  targetLabel,
  TARGETS,
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

describe('Discord targets', () => {
  // One table mirrors internal/discordlint: Target constants, Limit and
  // IsAttachment (DESIGN §5.1). /api/capabilities "limits" lists the same.
  it('lists none, emote, sticker and the four attachment tiers in dropdown order', () => {
    expect(TARGETS).toEqual(['', 'emote', 'sticker', 'attachment', 'attachment-50', 'attachment-100', 'attachment-500']);
    expect(TARGET_DEFS.map((t) => t.id)).toEqual(TARGETS);
  });

  it('carries the discordlint byte caps', () => {
    expect(LIMITS).toEqual({
      '': 0,
      emote: 262_144,
      sticker: 524_288,
      attachment: 20_000_000,
      'attachment-50': 50_000_000,
      'attachment-100': 100_000_000,
      'attachment-500': 500_000_000,
    });
    for (const t of TARGETS) expect(limitOf(t), t).toBe(LIMITS[t]);
    expect(limitOf('bogus')).toBe(0);
    expect(limitOf(undefined)).toBe(0);
    expect(limitKiB('emote')).toBe(256);
    expect(limitKiB('sticker')).toBe(512);
    expect(limitKiB('attachment')).toBe(19_531); // floor(20e6 / 1024)
    expect(limitKiB('')).toBe(0);
  });

  it('isAttachmentTarget mirrors discordlint.IsAttachment', () => {
    const attachments: Target[] = ['attachment', 'attachment-50', 'attachment-100', 'attachment-500'];
    for (const t of TARGETS) expect(isAttachmentTarget(t), t).toBe(attachments.includes(t));
    expect(isAttachmentTarget('attachment-999')).toBe(false);
    expect(isAttachmentTarget(null)).toBe(false);
  });

  it('labels every tier with who gets it, and shows an unknown id as-is', () => {
    expect(TARGET_LABEL.emote).toBe('Discord emote');
    expect(TARGET_LABEL.attachment).toMatch(/free.*20 MB/);
    expect(TARGET_LABEL['attachment-50']).toMatch(/Nitro Basic/);
    expect(TARGET_LABEL['attachment-50']).toMatch(/Level-2/);
    expect(TARGET_LABEL['attachment-100']).toMatch(/Level-3/);
    expect(TARGET_LABEL['attachment-500']).toMatch(/Nitro, 500 MB/);
    for (const t of TARGETS) expect(targetLabel(t)).toBe(TARGET_LABEL[t]);
    expect(targetLabel('attachment-999')).toContain('attachment-999');
    // the dropdown option text names the cap
    for (const d of TARGET_DEFS) if (d.limit > 0) expect(d.label, d.id).toMatch(/\d+ (KiB|MB)/);
  });

  it('fit budgets follow the caps (KiB), with a default for no target', () => {
    expect(FIT_KIB.emote * 1024).toBe(LIMITS.emote);
    expect(FIT_KIB.sticker * 1024).toBe(LIMITS.sticker);
    expect(FIT_KIB.attachment).toBe(19_531);
    expect(FIT_KIB['attachment-500']).toBe(Math.floor(500_000_000 / 1024));
    expect(FIT_KIB['']).toBe(DEFAULT_FIT_KIB);
    expect(fitKiBFor('')).toBe(DEFAULT_FIT_KIB);
    expect(fitKiBFor('nope')).toBe(DEFAULT_FIT_KIB);
    expect(fitKiBFor('attachment-100')).toBe(Math.floor(100_000_000 / 1024));
    expect(TRIM_FRINGE_THRESHOLD).toBe(180);
  });
});

describe('presets', () => {
  it('offers Emote · Sticker · Chat · Optimize · Frames · Custom, in order', () => {
    expect(PRESETS.map((p) => p.id)).toEqual(['emote', 'sticker', 'chat', 'optimize', 'frames', 'custom']);
    expect(PRESETS.map((p) => p.label)).toEqual(['Emote', 'Sticker', 'Chat', 'Optimize', 'Frames', 'Custom']);
  });

  it('every preset only sets a default target — the dropdown stays editable', () => {
    const want: Record<PresetId, Target> = { emote: 'emote', sticker: 'sticker', chat: 'attachment', optimize: '', frames: '', custom: '' };
    for (const p of PRESETS) {
      expect(p.target, p.id).toBe(want[p.id]);
      if (p.id !== 'custom') expect(applied(p.id).target, p.id).toBe(want[p.id]);
    }
  });

  it('Emote: GIF, 128×128 contain, fit 256 KiB on, target emote, WebP noted as verified', () => {
    const o = applied('emote');
    expect(o).toMatchObject({ format: 'gif', target: 'emote', width: 128, height: 128, fit: 'contain', fitEnabled: true, fitKiB: 256 });
    expect(o.fitKiB * 1024).toBe(LIMITS.emote);
    const p = presetById('emote');
    expect(p.formats).toEqual(['gif', 'webp', 'avif', 'png']);
    expect(p.formats).not.toContain('apng');
    expect(p.locksSize).toBe(true);
    expect(formatHint(p, 'webp')).toMatch(/soft edges.*verified on Discord/);
  });

  it('Sticker: indexed APNG (256 colours), 320×320 contain, fit 512 KiB on, keep size, GIF as the fallback', () => {
    const o = applied('sticker');
    expect(o).toMatchObject({ format: 'apng', colors: 256, target: 'sticker', width: 320, height: 320, fit: 'contain', fitEnabled: true, fitKiB: 512, fitKeepSize: true });
    expect(o.fitKiB * 1024).toBe(LIMITS.sticker);
    const p = presetById('sticker');
    expect(p.formats).toEqual(['apng', 'gif', 'png']);
    expect(p.warn).toMatch(/shrinks/i);
    expect(p.warn).toMatch(/server sticker/i);
    expect(formatHint(p, 'gif')).toMatch(/verified as a sticker/);
  });

  it('Chat: one preset, GIF by default, attachment target, fit off, source size — the format select re-seeds the quality', () => {
    const p = presetById('chat');
    expect(p.formats).toEqual(['gif', 'webp', 'avif']);
    expect(p.locksSize).toBe(false);
    const o = applied('chat');
    expect(o).toMatchObject({ format: 'gif', target: 'attachment', width: 0, height: 0, fps: 0, colors: 256, dither: 'sierra2_4a', lossy: 20, fitEnabled: false });
    // the former chat-webp / chat-avif defaults live in onFormat
    o.format = 'webp';
    p.onFormat?.(o, 'webp');
    expect(o).toMatchObject({ format: 'webp', target: 'attachment', quality: 80, lossless: false, fitEnabled: false });
    o.format = 'avif';
    p.onFormat?.(o, 'avif');
    expect(o).toMatchObject({ format: 'avif', quality: 60, lossless: false });
    o.lossy = 100;
    o.format = 'gif';
    p.onFormat?.(o, 'gif');
    expect(o).toMatchObject({ format: 'gif', dither: 'sierra2_4a', lossy: 20 });
    // a user-chosen target survives the format switch
    o.target = 'attachment-500';
    p.onFormat?.(o, 'webp');
    expect(o.target).toBe('attachment-500');
    // the verified-on-Discord hints moved with the formats
    expect(formatHint(p, 'gif')).toMatch(/sierra2_4a.*lossy 20/);
    expect(formatHint(p, 'webp')).toMatch(/q 80/);
    expect(formatHint(p, 'webp')).toMatch(/480 px/);
    expect(formatHint(p, 'avif')).toMatch(/verified on Discord attachments/);
  });

  it('Optimize: GIF only, no ops, only for GIF sources', () => {
    const p = presetById('optimize');
    expect(p.usesOps).toBe(false);
    expect(p.formats).toEqual(['gif']);
    expect(presetAvailable(p, gifInfo)).toBe(true);
    expect(presetAvailable(p, movInfo)).toBe(false);
    expect(presetAvailable(p, null)).toBe(false);
    expect(p.unavailableHint).toBeTruthy();
    expect(applied('optimize', gifInfo)).toMatchObject({ format: 'gif', target: '', width: 0, height: 0, fps: 0, fitEnabled: false });
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
    // unknown / retired ids fall back to Custom
    expect(presetById('chat-gif').id).toBe('custom');
  });

  it('APNG is offered only for the Sticker preset and Custom', () => {
    for (const p of PRESETS) {
      const offers = p.formats.includes('apng');
      expect(offers, p.id).toBe(p.id === 'sticker' || p.id === 'custom');
    }
  });

  it('gifski never appears in the UI', () => {
    const text = JSON.stringify([
      PRESETS.map((p) => [p.id, p.label, p.hint, p.warn, p.unavailableHint, p.formatHints]),
      FORMAT_HINT,
      TARGET_DEFS,
    ]);
    expect(text.toLowerCase()).not.toContain('gifski');
  });

  it('formatsFor always includes the current format', () => {
    expect(formatsFor(presetById('emote'), 'gif')).toEqual(['gif', 'webp', 'avif', 'png']);
    expect(formatsFor(presetById('emote'), 'jpeg')).toEqual(['gif', 'webp', 'avif', 'png', 'jpeg']);
    expect(formatsFor(presetById('optimize'), 'gif')).toEqual(['gif']);
  });

  it('formatHint falls back to the generic format note', () => {
    expect(formatHint(presetById('custom'), 'png')).toBe(FORMAT_HINT.png);
    expect(FORMAT_HINT.png).toContain('no fit search for a static PNG');
    for (const f of OUTPUT_FORMATS) expect(formatHint(presetById('custom'), f), f).toBeTruthy();
  });

  it('defaultOutput is the Chat preset', () => {
    expect(defaultOutput()).toMatchObject({ preset: 'chat', format: 'gif', target: 'attachment', fitEnabled: false, loop: 0 });
  });

  it('FIT_FORMATS mirrors the server fit engine (internal/jobs/fit.go fitFormats)', () => {
    expect([...FIT_FORMATS].sort()).toEqual(['apng', 'avif', 'gif', 'jpeg', 'webp']);
    for (const f of OUTPUT_FORMATS) expect(fitsFormat(f), f).toBe(f !== 'png' && f !== 'frames');
  });
});
