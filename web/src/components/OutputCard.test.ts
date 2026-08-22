// Server-side renders of the Output card per preset: which controls and
// notes show up (formats offered, the always-editable Discord target and its
// limit, fit controls, the Advanced fold, APNG notes).
import { render } from 'svelte/server';
import { beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../lib/api';
import { PRESETS, TARGET_DEFS } from '../lib/presets';
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

/** the rendered <select aria-label="Discord target">…</select> */
function targetSelect(out: string): string {
  const m = out.match(/<select[^>]*aria-label="Discord target"[^>]*>[\s\S]*?<\/select>/);
  return m ? m[0] : '';
}

describe('OutputCard (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
    applyPreset('chat');
  });

  it('offers the six "Use for" chips', () => {
    const out = html();
    expect(out).toContain('aria-label="Use for"');
    for (const p of PRESETS) expect(out, p.id).toContain(`>${p.label}</button>`);
    expect(out).not.toContain('Chat GIF');
    expect(out).not.toContain('Chat WebP');
  });

  it('Chat: the Discord target is an editable dropdown with every tier, the limit follows it', () => {
    let out = html();
    const sel = targetSelect(out);
    expect(sel).toBeTruthy();
    expect(sel).not.toContain('disabled');
    for (const t of TARGET_DEFS) expect(sel, t.id).toContain(`<option value="${t.id}"`);
    expect(sel).toMatch(/<option value="attachment"[^>]*selected/);
    expect(sel).toContain('Nitro Basic / Level-2 server, 50 MB');
    expect(sel).toContain('Level-3 server, 100 MB');
    expect(sel).toContain('Nitro, 500 MB');
    expect(out).toContain('<b>20 MB</b>');
    expect(out).toContain('(20,000,000 B)');
    expect(out).not.toContain('<select disabled'); // neither the format nor the target is locked
    // switching the tier moves the readout and the "= limit" budget
    app.output.target = 'attachment-100';
    out = html();
    expect(targetSelect(out)).toMatch(/<option value="attachment-100"[^>]*selected/);
    expect(out).toContain('<b>100 MB</b>');
    expect(out).toContain('= limit (100 MB)');
    app.output.target = '';
    out = html();
    expect(out).toContain('>none<');
    expect(out).not.toContain('= limit');
  });

  it('Chat: one Format select (GIF / WebP / AVIF) with the format-specific hint, no swap buttons', () => {
    let out = html();
    expect(out).toContain('<option value="gif"');
    expect(out).toContain('<option value="webp"');
    expect(out).toContain('<option value="avif"');
    expect(out).not.toContain('<option value="apng"');
    expect(out).toContain('sierra2_4a dither, lossy 20');
    expect(out).not.toContain('instead');
    app.output.format = 'webp';
    out = html();
    expect(out).toContain('q 80');
    expect(out).toContain('480 px');
    expect(out).toContain('Lossless');
    expect(out).not.toContain('sierra2_4a dither, lossy 20');
    app.output.format = 'avif';
    out = html();
    expect(out).toContain('verified on Discord attachments');
  });

  it('Emote: target emote, fit 256 KiB on, GIF knobs with matte / threshold / dither / loop under Advanced', () => {
    applyPreset('emote');
    const out = html();
    expect(targetSelect(out)).toMatch(/<option value="emote"[^>]*selected/);
    expect(out).toContain('<b>256 KiB</b>');
    expect(out).toContain('Fit to ≤');
    expect(out).toContain('value="256"');
    expect(out).toContain('= limit (256 KiB)');
    expect(out).toContain('Universal emote format');
    // the Advanced fold holds the rest
    expect(out).toContain('<details');
    expect(out).toContain('Advanced');
    expect(out).toContain('Discord dark');
    expect(out).toContain('Discord light');
    expect(out).toContain('Trim fringe');
    expect(out).toContain('GIF has 1-bit alpha');
    expect(out).toContain('Dither');
    expect(out).toContain('forever (Discord requires it)');
    expect(out).not.toContain('APNG (sticker only)');
    // the Discord target of a preset is still editable
    expect(targetSelect(out)).not.toContain('disabled');
    app.output.target = 'attachment-500';
    expect(html()).toContain('<b>500 MB</b>');
  });

  it('Sticker: APNG with the indexed-palette choice and the server-sticker note', () => {
    applyPreset('sticker');
    const out = html();
    expect(out).toContain('option value="apng"');
    expect(out).toContain('indexed 8-bit alpha');
    expect(out).toContain('Discord shrinks stickers larger than 320×320');
    expect(out).toContain('animates only as a server sticker');
    expect(out).toContain('value="512"');
    expect(out).toContain('stickers are never downscaled');
    expect(targetSelect(out)).toMatch(/<option value="sticker"[^>]*selected/);
  });

  it('Custom with APNG off-target warns; APNG for an emote is an error', () => {
    applyPreset('custom');
    app.output.format = 'apng';
    app.output.target = 'attachment';
    expect(html()).toContain('Discord shows frame 0 only');
    app.output.target = 'attachment-50';
    expect(html()).toContain('Discord shows frame 0 only');
    app.output.target = 'emote';
    expect(html()).toContain('APNG is not an animated-emoji format');
    app.output.target = 'sticker';
    app.output.format = 'webp';
    expect(html()).toContain('WEBP is not a Discord sticker format');
  });

  it('Custom with no target exposes the loop count under Advanced', () => {
    applyPreset('custom');
    app.output.target = '';
    app.output.format = 'gif';
    const out = html();
    expect(out).toContain('Loop count (0 = forever, N = play N+1 times)');
    expect(out).not.toContain('forever (Discord requires it)');
    app.output.format = 'png'; // static: no loop, no matte, no Advanced at all
    expect(html()).not.toContain('<details');
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
    expect(out).toContain('Dither'); // gifsicle dithers when it drops colours
    expect(targetSelect(out)).toMatch(/<option value=""[^>]*selected/);
  });

  it('Optimize with fit on hides "keep size" (never scales), keeps "keep fps" and names the gifsicle ladder (W4)', () => {
    applyPreset('optimize');
    app.output.fitEnabled = true;
    const out = html();
    expect(out).toContain('Fit to ≤');
    expect(out).not.toContain('keep size');
    expect(out).toContain('keep fps');
    expect(out).toContain('lossy → frame drop → colours');
    expect(out).not.toContain('colours → downscale');
    // the re-encoding presets keep both checkboxes and the full ladder wording
    applyPreset('chat');
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
    expect(out).not.toContain('aria-label="Discord target"');
    expect(out).not.toContain('Limit');
    app.output.frameFormat = 'jpeg';
    expect(html()).toContain('JPEG quality');
  });

  it('static PNG hides the fit row even when the preset turned fit on (server has no PNG fit ladder)', () => {
    applyPreset('emote'); // sets fitEnabled = true
    app.output.format = 'png';
    const out = html();
    expect(out).not.toContain('Fit to ≤');
    expect(out).not.toContain('keep size');
    expect(out).toContain('Static emote');
    // switching back to a fit-capable format restores the preset's fit row
    app.output.format = 'gif';
    expect(html()).toContain('Fit to ≤');
    // and Custom shows the generic PNG note
    applyPreset('custom');
    app.output.format = 'png';
    expect(html()).toContain('no fit search for a static PNG');
  });

  it('Sticker with fit on locks the APNG colour select to the indexed fit ladder', () => {
    applyPreset('sticker'); // apng, fit 512 KiB on
    let out = html();
    expect(out).toContain('Colours (fit ladder: 256 → 128 → 64 indexed)');
    expect(out).toContain('turn fit off to pick a fixed palette or RGBA truecolour');
    expect(out).toContain('fit off only'); // the RGBA option no longer claims fit behaviour
    // preset-locked size-fit select + the locked colour select; the target select is never locked
    expect(out.match(/<select disabled/g)?.length).toBe(2);
    app.output.fitEnabled = false;
    out = html();
    expect(out).not.toContain('fit ladder: 256 → 128 → 64 indexed');
    expect(out).not.toContain('turn fit off to pick a fixed palette');
    expect(out.match(/<select disabled/g)?.length).toBe(1); // only the size-fit select stays preset-locked
  });

  it('warns when the fit budget is above the chosen tier', () => {
    applyPreset('custom');
    app.output.format = 'gif';
    app.output.target = 'emote';
    app.output.fitEnabled = true;
    app.output.fitKiB = 300;
    expect(html()).toContain('above the 256 KiB Discord emote cap');
    app.output.target = 'attachment';
    expect(html()).not.toContain('above the');
  });

  it('never mentions gifski', () => {
    for (const p of PRESETS) {
      applyPreset(p.id);
      expect(html().toLowerCase(), p.id).not.toContain('gifski');
    }
  });
});
