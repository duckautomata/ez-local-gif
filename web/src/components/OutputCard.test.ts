// Server-side renders of the Output card per preset: which controls and
// notes show up (formats offered, fit controls, matte choice, APNG notes).
import { render } from 'svelte/server';
import { beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../lib/api';
import { app, applyPreset, setSource } from '../lib/state.svelte';
import OutputCard from './OutputCard.svelte';

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
const gifSrc: Source = { hash: 'd'.repeat(64), name: 'test.gif', size: 100, info: gifInfo };

function html(): string {
  return render(OutputCard, { props: {} }).body;
}

describe('OutputCard (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
    applyPreset('chat-gif');
  });

  it('Emote: GIF default with the one-click WebP swap and fit 256 KiB on', () => {
    applyPreset('emote');
    const out = html();
    expect(out).toContain('WebP instead');
    expect(out).toContain('keeps soft edges — verified on Discord');
    expect(out).toContain('Fit to ≤');
    expect(out).toContain('value="256"');
    expect(out).toContain('Discord dark');
    expect(out).toContain('Discord light');
    expect(out).toContain('Trim fringe');
    expect(out).toContain('GIF has 1-bit alpha');
    expect(out).not.toContain('APNG (sticker only)');
  });

  it('Sticker: APNG with the indexed-palette choice, GIF swap and the server-sticker note', () => {
    applyPreset('sticker');
    const out = html();
    expect(out).toContain('GIF instead');
    expect(out).toContain('option value="apng"');
    expect(out).toContain('indexed 8-bit alpha');
    expect(out).toContain('Discord shrinks stickers larger than 320×320');
    expect(out).toContain('animates only as a server sticker');
    expect(out).toContain('value="512"');
    expect(out).toContain('stickers are never downscaled');
  });

  it('Custom with APNG off-target warns; APNG for an emote is an error', () => {
    applyPreset('custom');
    app.output.format = 'apng';
    app.output.target = 'attachment';
    expect(html()).toContain('Discord shows frame 0 only');
    app.output.target = 'emote';
    expect(html()).toContain('APNG is not an animated-emoji format');
    app.output.target = 'sticker';
    app.output.format = 'webp';
    expect(html()).toContain('WEBP is not a Discord sticker format');
  });

  it('Optimize: format locked to GIF, frame-drop chips from the source rate, no size fields', () => {
    applyPreset('optimize');
    const out = html();
    expect(out).toContain('keep all frames (25 fps)');
    expect(out).toContain('drop every 2nd frame (12.5 fps)');
    expect(out).toContain('drop every 3rd frame (16.67 fps)');
    expect(out).toContain('drop every 4th frame (18.75 fps)');
    expect(out).not.toContain('Width');
    expect(out).toContain('<select disabled'); // format select locked
    expect(out).not.toContain('Alpha threshold'); // no re-quantisation → no threshold/matte path
    expect(out).not.toContain('Matte');
    expect(out).toContain('Lossy');
  });

  it('Optimize with fit on hides "keep size" (never scales), keeps "keep fps" and names the gifsicle ladder (W4)', () => {
    applyPreset('optimize');
    app.output.fitEnabled = true;
    const out = html();
    expect(out).toContain('Fit to ≤');
    expect(out).not.toContain('keep size');
    expect(out).toContain('keep fps');
    expect(out).toContain('lossy → frame drop → colours');
    expect(out).not.toContain('downscale');
    // the re-encoding presets keep both checkboxes and the full ladder wording
    applyPreset('chat-gif');
    app.output.fitEnabled = true;
    const out2 = html();
    expect(out2).toContain('keep size');
    expect(out2).toContain('keep fps');
    expect(out2).toContain('lossy → fps → colours → downscale');
  });

  it('Frames: frame format select, no fit / target controls', () => {
    applyPreset('frames');
    const out = html();
    expect(out).toContain('Frame format');
    expect(out).toContain('JPEG (flattened onto the matte)');
    expect(out).not.toContain('Fit to ≤');
    expect(out).not.toContain('Byte limit');
    app.output.frameFormat = 'jpeg';
    expect(html()).toContain('JPEG quality');
  });

  it('static PNG hides the fit row even when the preset turned fit on (server has no PNG fit ladder)', () => {
    applyPreset('emote'); // sets fitEnabled = true
    app.output.format = 'png';
    const out = html();
    expect(out).not.toContain('Fit to ≤');
    expect(out).not.toContain('keep size');
    expect(out).toContain('no fit search for a static PNG');
    // switching back to a fit-capable format restores the preset's fit row
    app.output.format = 'gif';
    expect(html()).toContain('Fit to ≤');
  });

  it('Sticker with fit on locks the APNG colour select to the indexed fit ladder', () => {
    applyPreset('sticker'); // apng, fit 512 KiB on
    let out = html();
    expect(out).toContain('Colours (fit ladder: 256 → 128 → 64 indexed)');
    expect(out).toContain('turn fit off to pick a fixed palette or RGBA truecolour');
    expect(out).toContain('fit off only'); // the RGBA option no longer claims fit behaviour
    expect(out).not.toContain('fits only at low fps');
    // preset-locked size-fit select + the locked colour select
    expect(out.match(/<select disabled/g)?.length).toBe(2);
    app.output.fitEnabled = false;
    out = html();
    expect(out).not.toContain('fit ladder: 256 → 128 → 64 indexed');
    expect(out).not.toContain('turn fit off to pick a fixed palette');
    expect(out.match(/<select disabled/g)?.length).toBe(1); // only the size-fit select stays preset-locked
  });

  it('never mentions gifski', () => {
    for (const id of ['emote', 'sticker', 'chat-gif', 'chat-webp', 'chat-avif', 'optimize', 'frames', 'custom'] as const) {
      applyPreset(id);
      expect(html().toLowerCase(), id).not.toContain('gifski');
    }
  });
});
