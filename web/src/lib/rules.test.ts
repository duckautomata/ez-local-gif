import { describe, expect, it } from 'vitest';
import { knownRules, ruleLabel } from './rules';

// The rule ids of internal/discordlint (gif.go, webp.go, apng.go) and
// internal/jobs (render.alpha). Keep in sync when rules are added.
const GIF = [
  'gif.gce-every-frame',
  'gif.frame0-transparency',
  'gif.lsd-background-index',
  'gif.disposal',
  'gif.netscape-loop',
  'gif.min-delay',
  'gif.global-palette',
  'gif.no-interlace',
  'gif.no-extra-extensions',
  'gif.first-frame-visible',
  'gif.trailer',
  'gif.size-limit',
  'gif.sticker-dims',
  'gif.sticker-duration',
  'gif.emote-dims',
];
const WEBP = [
  'webp.riff',
  'webp.anim-flag',
  'webp.alpha-flag',
  'webp.loop-forever',
  'webp.canvas',
  'webp.no-metadata',
  'webp.min-delay',
  'webp.size-limit',
  'webp.sticker',
  'webp.emote-dims',
];
const APNG = [
  'apng.container',
  'apng.plays-forever',
  'apng.first-frame',
  'apng.canvas',
  'apng.min-delay',
  'apng.size-limit',
  'apng.sticker',
  'apng.not-emote',
  'apng.attachment',
  'apng.indexed',
];
const STATIC = ['static.size-limit', 'static.emote-dims', 'static.sticker', 'static.format'];
// internal/jobs: the render pipeline's alpha check and the fit engine's
// fit.target (RuleFitTarget) — the one rule users hit when a fit fails.
const JOBS = ['render.alpha', 'fit.target'];

describe('ruleLabel', () => {
  it('has a friendly label for every known rule id', () => {
    for (const id of [...GIF, ...WEBP, ...APNG, ...STATIC, ...JOBS]) {
      const label = ruleLabel(id);
      expect(label, id).not.toBe(id);
      expect(label.length, id).toBeGreaterThan(4);
    }
  });
  it('falls back to the id for unknown rules so nothing is hidden', () => {
    expect(ruleLabel('gif.some-new-rule')).toBe('gif.some-new-rule');
    expect(ruleLabel('')).toBe('');
  });
  it('lists exactly the rules it knows', () => {
    expect(new Set(knownRules())).toEqual(new Set([...GIF, ...WEBP, ...APNG, ...STATIC, ...JOBS]));
  });
  it('labels the jobs-level fit rule and the apng.indexed check for both outcomes', () => {
    expect(ruleLabel('fit.target')).toBe('Fit-to-size budget reached');
    // must read correctly whether the output is indexed (✓) or RGBA (info dot)
    expect(ruleLabel('apng.indexed')).toBe('Indexed 8-bit-alpha APNG (sticker default rung)');
  });
});
