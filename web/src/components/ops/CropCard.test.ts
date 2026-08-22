// Server-side renders of the Crop card: the auto-crop toggle disables the
// manual rectangle, carries padding (and the alpha threshold for alpha
// sources — or keyed ones), the header summary / toggle cover both ways of
// cropping, and the hint says what auto-crop crops to with a Background op.
import { render } from 'svelte/server';
import { beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../../lib/api';
import { app, buildOps, setSource } from '../../lib/state.svelte';
import CropCard from './CropCard.svelte';

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

function html(info = gifInfo, initialOpen = false): string {
  return render(CropCard, { props: { info, initialOpen } }).body;
}
/** the <input aria-label="Enable Crop"> tag of the header */
function headerToggle(out: string): string {
  return out.match(/<input[^>]*aria-label="Enable Crop"[^>]*>/)?.[0] ?? '';
}
/** the <input type="number"> tags of the manual rectangle (X, Y, Width, Height) */
function rectInputs(out: string): string[] {
  return (out.match(/<input type="number"[^>]*>/g) ?? []).filter((t) => / max="(159|119|160|120)"/.test(t));
}

describe('CropCard (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
  });

  it('collapsed: the summary names the full frame, the rectangle, or auto-crop; the toggle covers both', () => {
    expect(html()).toContain('full frame 160×120');
    expect(headerToggle(html())).not.toContain('checked');
    app.ops.crop = { enabled: true, x: 10, y: 20, w: 100, h: 50 };
    expect(html()).toContain('100×50 at 10,20');
    expect(headerToggle(html())).toContain('checked');
    app.ops.autocrop = { enabled: true, padding: 0, threshold: 1 };
    expect(html()).toContain('auto-crop to content');
    app.ops.autocrop.padding = 6;
    expect(html()).toContain('auto-crop to content + 6 px');
    app.ops.crop.enabled = false; // auto alone keeps the header on
    expect(headerToggle(html())).toContain('checked');
    expect(buildOps(app.ops)).toEqual([{ kind: 'autocrop', params: { padding: 6 } }]);
  });

  it('open: auto-crop off leaves the rectangle editable, on disables it and the buttons', () => {
    let out = html(gifInfo, true);
    expect(rectInputs(out)).toHaveLength(4);
    for (const tag of rectInputs(out)) expect(tag).not.toContain('disabled');
    expect(out).toMatch(/<button[^>]*>Centre square<\/button>/);
    expect(out).not.toMatch(/<button[^>]*disabled[^>]*>Centre square/);
    expect(out).toContain('Drag on the preview to draw the rectangle');
    app.ops.autocrop = { enabled: true, padding: 4, threshold: 64 };
    out = html(gifInfo, true);
    expect(rectInputs(out)).toHaveLength(4);
    for (const tag of rectInputs(out)) expect(tag).toContain('disabled');
    expect(out).toMatch(/<button[^>]*disabled[^>]*>Centre square/);
    expect(out).toMatch(/<button[^>]*disabled[^>]*aria-label="Reset crop"/);
    expect(out).toContain('the manual rectangle is ignored while auto-crop is on');
    // padding and the alpha threshold (Advanced) are live
    expect(out).toMatch(/<input type="number"[^>]*max="1024"[^>]*value="4"/);
    expect(out).toContain('alpha threshold 64');
    expect(out).toMatch(/<input type="range"[^>]*max="255"[^>]*value="64"/);
  });

  it('an opaque source has no alpha-threshold fold — unless a Background op keys it', () => {
    const opaque: ProbeInfo = { ...gifInfo, hasAlpha: false };
    setSource({ ...gifSrc, info: opaque, hash: '1'.repeat(64) });
    app.ops.autocrop = { enabled: true, padding: 0, threshold: 1 };
    let out = html(opaque, true);
    expect(out).toContain('Auto-crop to content');
    expect(out).not.toContain('alpha threshold');
    expect(out).not.toContain('aria-label="Auto-crop alpha threshold"');
    // keyed content has alpha whatever the source: the threshold applies
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'green' };
    app.ops.autocrop.threshold = 32;
    out = html(opaque, true);
    expect(out).toContain('alpha threshold 32');
    expect(out).toContain('aria-label="Auto-crop alpha threshold"');
    // a Pick-a-colour mode with nothing picked yet emits no op: no fold
    app.ops.background = { ...app.ops.background, mode: 'pick', pickColor: '' };
    expect(html(opaque, true)).not.toContain('alpha threshold');
  });

  it('hint: with a Background op on, auto-crop crops to what is left after background removal', () => {
    app.ops.autocrop = { enabled: true, padding: 0, threshold: 1 };
    const keyedText = 'With the Background op on it crops to what is left after background removal; alpha ≥ threshold counts as content.';
    // alpha source, no keying
    let out = html(gifInfo, true);
    expect(out).toContain('Alpha ≥ threshold counts as content (with a Background op on, after background removal).');
    expect(out).not.toContain(keyedText);
    expect(out).toContain('the manual rectangle is ignored while auto-crop is on');
    expect(out).toContain('alpha for transparent sources and after background removal, non-black borders otherwise'); // the checkbox title
    // opaque source, no keying
    const opaque: ProbeInfo = { ...gifInfo, hasAlpha: false };
    setSource({ ...gifSrc, info: opaque, hash: '2'.repeat(64) });
    app.ops.autocrop = { enabled: true, padding: 0, threshold: 1 };
    out = html(opaque, true);
    expect(out).toContain('Non-black borders are trimmed; with a Background op on it crops to what is left after background removal');
    expect(out).not.toContain(keyedText);
    // keyed: greenscreen, or a picked colour
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'green' };
    expect(html(opaque, true)).toContain(keyedText);
    app.ops.background = { ...app.ops.background, mode: 'pick', pickColor: '00ff00' };
    expect(html(opaque, true)).toContain(keyedText);
    setSource(gifSrc);
    app.ops.autocrop = { enabled: true, padding: 0, threshold: 1 };
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'blue' };
    out = html(gifInfo, true);
    expect(out).toContain(keyedText);
    expect(out).not.toContain('Non-black borders are trimmed');
    // manual crop: the drag hint, nothing about keying
    app.ops.autocrop.enabled = false;
    out = html(gifInfo, true);
    expect(out).toContain('Drag on the preview to draw the rectangle');
    expect(out).not.toContain('background removal;');
  });
});
