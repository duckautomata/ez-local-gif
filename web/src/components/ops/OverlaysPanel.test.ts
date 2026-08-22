// Server-side renders of the Overlays section and its cards: the add
// buttons, a text card with the default font offered even before /api/fonts
// answered, an image card without an asset showing the picker, reorder /
// remove controls, the time-range hint following the scrubber, and the
// server-capability gates (font picker, older-server notice).
import { render } from 'svelte/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../../lib/api';
import { resetFeatures, setFeatures } from '../../lib/capabilities.svelte';
import { trimTime } from '../../lib/format';
import { scrubberRange } from '../../lib/overlay';
import { addOverlay, app, setSource } from '../../lib/state.svelte';
import OverlaysPanel from './OverlaysPanel.svelte';

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
const gifSrc: Source = { hash: 'b'.repeat(64), name: 'test.gif', size: 100, info: gifInfo };
const logo: Source = { hash: 'c'.repeat(64), name: 'logo.png', size: 10, info: { ...gifInfo, width: 40, height: 20, fps: 0, duration: 0, frames: 1, isStill: true, kind: 'image' } };

function html(initialOpen = false): string {
  return render(OverlaysPanel, { props: { info: gifInfo, initialOpen } }).body;
}

describe('OverlaysPanel (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
  });
  afterEach(() => resetFeatures());

  it('offers Add text / Add image and no cards for a fresh source', () => {
    const out = html();
    expect(out).toContain('+ Add text');
    expect(out).toContain('+ Add image');
    expect(out).not.toContain('Enable Text');
  });

  it('renders a text card with the default font, anchor grid, X/Y and time range; header has reorder/remove', () => {
    addOverlay('text');
    const out = html(true);
    expect(out).toContain('Text — Text');
    expect(out).toMatch(/<option value="DejaVu Sans"[^>]*>DejaVu Sans<\/option>/); // fonts fallback before /api/fonts answers
    expect(out).toContain('aria-label="Anchor"');
    expect(out).toMatch(/<button[^>]*aria-pressed="true"[^>]*aria-label="top left"/);
    expect(out).toContain('Whole clip');
    expect(out).toContain('◂ from scrubber');
    // the two "from scrubber" buttons have distinct accessible names (review W4)
    expect(out).toMatch(/<button[^>]*aria-label="Start from scrubber"[^>]*>◂ from scrubber<\/button>/);
    expect(out).toMatch(/<button[^>]*aria-label="End from scrubber"[^>]*>\s*◂ from scrubber\s*<\/button>/);
    expect(out.match(/aria-label="(Start|End) from scrubber"/g)).toHaveLength(2);
    expect(out).toContain('aria-label="Move up"');
    expect(out).toContain('aria-label="Remove overlay"');
    // the only card cannot move either way
    expect(out).toMatch(/<button[^>]*disabled[^>]*aria-label="Move up"/);
    expect(out).toMatch(/<button[^>]*disabled[^>]*aria-label="Move down"/);
  });

  it('renders an image card: picker without an asset, facts with one', () => {
    const o = addOverlay('image');
    let out = html(true);
    expect(out).toContain('Image — Image');
    expect(out).toContain('Choose image…');
    expect(html(false)).toContain('no image yet'); // the collapsed summary
    if (o.kind === 'image') o.asset = logo;
    out = html(true);
    expect(out).toContain('Image — logo.png');
    expect(out).toContain('>40×20<');
    expect(out).toContain('Replace…');
    expect(out).toContain('drawn at <b>40×20</b>');
    expect(out).not.toContain('>Loop<'); // a still never loops
  });

  it('a second card can move up; the collapsed summary names size / anchor / position', () => {
    const t = addOverlay('text');
    addOverlay('image');
    if (t.kind === 'text') {
      t.size = 48;
      t.anchor = 'br';
      t.x = 100;
      t.y = 90;
    }
    const out = html();
    expect(out).toContain('48 px · DejaVu Sans · br 100,90');
    // two cards: the second one's "Move up" is enabled
    const ups = out.match(/<button[^>]*aria-label="Move up"[^>]*>/g) ?? [];
    expect(ups).toHaveLength(2);
    expect(ups[0]).toContain('disabled');
    expect(ups[1]).not.toContain('disabled');
  });

  it('time range: warns when the scrubber frame is outside the range and offers to go there', () => {
    const t = addOverlay('text');
    if (t.kind === 'text') {
      t.start = 1;
      t.end = 1.5;
    }
    app.ui.scrubFrame = 0; // 0.00 s: before the range
    let out = html(true);
    expect(out).toContain('1.00 s → 1.50 s');
    expect(out).toContain('not shown at the scrubber');
    expect(out).toContain('▸ go to start');
    app.ui.scrubFrame = 25; // 1.00 s: inside
    out = html(true);
    expect(out).not.toContain('not shown at the scrubber');
    if (t.kind === 'text') t.end = 0.5; // end before start
    out = html(true);
    expect(out).toContain('End must be after start');
  });

  it('time range: says End is exclusive, like Trim', () => {
    addOverlay('text');
    app.ui.scrubFrame = 12;
    const out = html(true);
    expect(out).toContain('End (s, exclusive; 0 = end)');
    expect(out).toContain('Start is inclusive and End exclusive: the frame at End is not drawn');
    // "from scrubber" for End: the point after frame 13 — which is then the last frame drawn
    expect(out).toContain('End after the frame under the scrubber (frame 13, 0.52 s) — that frame is the last one drawn');
    app.ui.scrubFrame = 49;
    expect(html(true)).toContain('End after the last frame (to the end)');
  });

  it('time range: a window taken from the scrubber on a 30 fps clip is shown at that very frame (review W1)', () => {
    setSource({ ...gifSrc, info: { ...gifInfo, fps: 30, duration: 2, frames: 60 } });
    app.output.fps = 30; // the default preset would plan 25 fps; the grid under test is 30 fps
    const t = addOverlay('text');
    app.ui.scrubFrame = 2; // frame 3 starts at 2/30 = 0.0666666…, which rounds UP to the µs
    const w = scrubberRange(2, 30, 60);
    expect(w).toEqual({ start: 0.066666, end: 0.1 });
    if (t.kind === 'text') {
      t.start = w.start;
      t.end = w.end;
    }
    let out = html(true);
    expect(out).toContain('0.067 s → 0.10 s');
    expect(out).not.toContain('not shown at the scrubber');
    expect(out).toContain('Start at the frame under the scrubber (frame 3, 0.067 s)');
    expect(out).toContain('End after the frame under the scrubber (frame 3, 0.10 s) — that frame is the last one drawn');
    // the nearest-µs value the card used to store sits above the frame's
    // own time; the render's 1e-4 s enable tolerance shows it there anyway,
    // and the card agrees (review W-G3) — while the frame before stays out
    if (t.kind === 'text') t.start = trimTime(2 / 30);
    out = html(true);
    expect(out).not.toContain('not shown at the scrubber');
    app.ui.scrubFrame = 1;
    out = html(true);
    expect(out).toContain('not shown at the scrubber');
    // the frame after the window is not shown either
    if (t.kind === 'text') t.start = w.start;
    app.ui.scrubFrame = 3;
    out = html(true);
    expect(out).toContain('not shown at the scrubber');
  });

  it('text size hint: a percentage of the OUTPUT canvas height, omitted while auto-crop leaves it unknown (review W6)', () => {
    const t = addOverlay('text'); // 32 px on the chat preset: the 160×120 source as produced
    expect(html(true)).toContain('Size is 26.7% of the 120 px output height');
    expect(html(true)).not.toContain('source height');
    app.ops.resize = { enabled: true, width: 80, height: 0, fit: 'contain' }; // 80×60
    expect(html(true)).toContain('Size is 53.3% of the 60 px output height');
    app.output.width = 128;
    app.output.height = 128; // the Output card pins the canvas
    expect(html(true)).toContain('Size is 25% of the 128 px output height');
    if (t.kind === 'text') t.size = 64;
    expect(html(true)).toContain('Size is 50% of the 128 px output height');
    // auto-crop without both output dimensions: the canvas is resolved server-side, no hint
    app.output.width = 0;
    app.output.height = 0;
    app.ops.autocrop.enabled = true;
    const out = html(true);
    expect(out).not.toContain('output height');
    expect(out).not.toContain('source height');
  });

  it('offers the font picker only where the server lists fonts; otherwise a read-only DejaVu Sans with a hint', () => {
    addOverlay('text');
    // optimistic before the server answered, and on a server with fc-list
    let out = html(true);
    expect(out).toMatch(/<select[^>]*>[\s\S]*<option value="DejaVu Sans"/);
    expect(out).not.toContain('aria-label="Font (fixed)"');
    setFeatures({ features: { overlays: true, fonts: true } });
    out = html(true);
    expect(out).toMatch(/<select[^>]*>[\s\S]*<option value="DejaVu Sans"/);
    expect(out).toContain("Fonts come from the server's font list");
    // a server whose /api/fonts is empty: no picker, the fixed face and why
    setFeatures({ features: { overlays: true, fonts: false } });
    out = html(true);
    expect(out).not.toContain('<select');
    expect(out).not.toContain('<option');
    expect(out).toMatch(/<input[^>]*type="text"[^>]*value="DejaVu Sans"[^>]*readonly[^>]*aria-label="Font \(fixed\)"/);
    expect(out).toContain('Font (fixed)');
    expect(out).toContain('This server has no font list (fc-list unavailable), so only DejaVu Sans is offered.');
    expect(out).not.toContain("Fonts come from the server's font list");
    // the rest of the card is untouched
    expect(out).toContain('aria-label="Anchor"');
    expect(out).toContain('Whole clip');
  });

  it('keeps the section usable but notes when the server has no overlay support', () => {
    const notice = 'This server does not support overlays';
    expect(html()).not.toContain(notice);
    setFeatures({ features: { fit: true, sequence: true, optimize: true } }); // a Phase 2 server
    let out = html();
    expect(out).toContain(notice);
    expect(out).toMatch(/<p class="note[^"]*"[^>]*>This server does not support overlays/);
    expect(out).toContain('+ Add text'); // still offered
    addOverlay('text');
    out = html(true);
    expect(out).toContain(notice);
    expect(out).toContain('Text — Text');
    setFeatures({ features: { overlays: true } });
    expect(html(true)).not.toContain(notice);
  });
});
