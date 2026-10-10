// Server-side renders of the Background card: the None · Color · Screen
// segment, the Color rows (eyedropper / typed hex / "+ add color", the armed
// row), the Screen sub-choice with its key color (Pick from preview, the
// auto-match hint), sliders and the Advanced despill fold, the Edges fold
// (Shift edge / Soft edge / Fill pinholes) gated on features.morph, the
// summary lines, the batch variant (typed hex only) and the notice for a
// server without keying support.
import { render } from 'svelte/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../../lib/api';
import { resetFeatures, setFeatures } from '../../lib/capabilities.svelte';
import { app, armEyedropper, armScreenEyedropper, resetApp, setBackgroundMode, setSource } from '../../lib/state.svelte';
import BackgroundCard from './BackgroundCard.svelte';

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
  hasAlpha: false,
  hasAudio: false,
  isStill: false,
  kind: 'animation',
  premultiplied: false,
};
const gifSrc: Source = { hash: 'e'.repeat(64), name: 'test.gif', size: 100, info: gifInfo };
/** every feature a Phase 5a server reports */
const all5a = { fit: true, sequence: true, optimize: true, keying: true, overlays: true, proxy: true, fonts: true, feather: true, bounce: true, inputPick: true, outputSave: true, gifski: true, morph: true };

function html(initialOpen = true, picker = true): string {
  return render(BackgroundCard, { props: { initialOpen, picker } }).body;
}
/** the aria-pressed value of the button with that label (mode segment, screen sub-choice) */
function pressed(out: string, label: string): boolean {
  const m = out.match(new RegExp(`<button[^>]*aria-pressed="(true|false)"[^>]*>${label}</button>`));
  return m?.[1] === 'true';
}
/** the eyedropper buttons of the Color rows, in row order */
function pickButtons(out: string): string[] {
  return out.match(/<button[^>]*title="Then click the color on the preview"[^>]*>[\s\S]*?<\/button>/g) ?? [];
}
/** the Screen key color's Pick from preview button ('' when absent) */
function screenPickButton(out: string): string {
  return out.match(/<button[^>]*title="Then click the screen on the preview"[^>]*>[\s\S]*?<\/button>/)?.[0] ?? '';
}
/** the Screen key color's hint text ('' when absent) */
function screenHint(out: string): string {
  return out.match(/<span class="hint screen-hint[^"]*"[^>]*>([\s\S]*?)<\/span>/)?.[1]?.trim() ?? '';
}
/** the "+ add color" button tag */
function addButton(out: string): string {
  return out.match(/<button[^>]*>\+ add color<\/button>/)?.[0] ?? '';
}
/** the Edges <details> element ('' when absent) */
function edgeFold(out: string): string {
  return out.match(/<details[^>]*><summary[^>]*><span[^>]*>Edges<\/span>[\s\S]*?<\/details>/)?.[0] ?? '';
}

describe('BackgroundCard (SSR)', () => {
  beforeEach(() => {
    resetApp();
    setSource(gifSrc);
  });
  afterEach(() => {
    resetFeatures();
    resetApp();
  });

  it('starts off: summary "off", the three modes with None pressed, no sliders, no Edges fold', () => {
    expect(html(false)).toContain('>off</span>');
    const out = html();
    expect(out).toContain('aria-label="Background mode"');
    for (const m of ['None', 'Color', 'Screen']) expect(out, m).toContain(`>${m}</button>`);
    for (const m of ['AI', 'Greenscreen', 'Bluescreen', 'Pick a color']) expect(out, m).not.toContain(`>${m}</button>`);
    expect(out.indexOf('>Color</button>')).toBeLessThan(out.indexOf('>Screen</button>')); // None · Color · Screen
    expect(pressed(out, 'None')).toBe(true);
    expect(pressed(out, 'Color')).toBe(false);
    expect(pressed(out, 'Screen')).toBe(false);
    expect(out).not.toContain('aria-label="Similarity"');
    expect(edgeFold(out)).toBe('');
    expect(out).toContain('Pick a mode to make a flat background transparent');
  });

  it('enabling the card lands on Color: one empty row with the eyedropper and a hex field, "+ add color", sliders at 0.08 / 0', () => {
    app.ops.background.enabled = true; // the flag alone (a restored session): the default mode
    let out = html();
    expect(pressed(out, 'Color')).toBe(true);
    expect(pressed(out, 'Screen')).toBe(false);
    expect(pressed(out, 'None')).toBe(false);
    expect(out).toContain('Color to remove');
    expect(pickButtons(out)).toHaveLength(1);
    expect(pickButtons(out)[0]).toContain('Pick from preview');
    expect(out).toContain('nothing picked yet');
    expect(out).toMatch(/<input[^>]*type="text"[^>]*placeholder="#rrggbb"[^>]*aria-label="Color 1 to remove \(hex\)"/);
    expect(addButton(out)).toBeTruthy();
    expect(addButton(out)).not.toContain('disabled');
    expect(out).toContain('Similarity (0.01–1) — <b>0.08</b>');
    expect(out).toContain('Blend (0–1) — <b>0.00</b>');
    expect(out).not.toContain('>Despill</span>'); // colorkey has no despill
    expect(out).not.toContain('aria-label="Screen color"');
    expect(out).not.toContain('aria-label="Remove color 1"'); // a single empty row has nothing to remove
    expect(html(false)).toContain('pick a color on the preview');
    // once picked: swatch + hex, "Pick again", a clear button, the summary names the color
    app.ops.background.colors = ['313338'];
    app.ops.background.pickSimilarity = 0.25;
    out = html();
    expect(pickButtons(out)[0]).toContain('Pick again');
    expect(out).toMatch(/class="swatch[^"]*"[^>]*background: #313338/);
    expect(out).toMatch(/<input[^>]*value="#313338"[^>]*aria-label="Color 1 to remove \(hex\)"/);
    expect(out).toContain('aria-label="Remove color 1"');
    expect(out).toContain('Similarity (0.01–1) — <b>0.25</b>');
    expect(html(false)).toContain('color #313338 · similarity 0.25 · fill pinholes');
    // the armed eyedropper shows on its row's button
    armEyedropper(0);
    out = html();
    expect(pickButtons(out)[0]).toContain('Click the preview…');
    expect(pickButtons(out)[0]).toContain('aria-pressed="true"');
  });

  it('"+ add color" rows: each with its own eyedropper / hex / remove; only the armed row says "Click the preview…"; capped at 6', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'color', colors: ['313338', ''] };
    armEyedropper(1);
    let out = html();
    expect(out).toContain('Colors to remove');
    expect(out).toContain('aria-label="Color 1 to remove (hex)"');
    expect(out).toContain('aria-label="Color 2 to remove (hex)"');
    expect(out).toContain('aria-label="Remove color 1"');
    expect(out).toContain('aria-label="Remove color 2"');
    const buttons = pickButtons(out);
    expect(buttons).toHaveLength(2);
    expect(buttons[0]).toContain('Pick again');
    expect(buttons[0]).toContain('aria-pressed="false"');
    expect(buttons[1]).toContain('Click the preview…');
    expect(buttons[1]).toContain('aria-pressed="true"');
    // the summary counts the picked rows only
    expect(html(false)).toContain('color #313338 · similarity 0.08 · fill pinholes');
    app.ops.background.colors = ['313338', 'facc82'];
    expect(html(false)).toContain('2 colors · similarity 0.08 · fill pinholes');
    // six rows: no seventh
    app.ops.background.colors = ['111111', '222222', '333333', '444444', '555555', '666666'];
    out = html();
    expect(pickButtons(out)).toHaveLength(6);
    expect(addButton(out)).toContain('disabled');
    expect(html(false)).toContain('6 colors · similarity 0.08');
    app.ops.background.colors = ['111111'];
    expect(addButton(html())).not.toContain('disabled');
  });

  it('in batch (picker off) Color takes typed hex only: no eyedropper, the hex field stays, the summary asks for a value', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'color', colors: [''] };
    const out = html(true, false);
    expect(pickButtons(out)).toHaveLength(0);
    expect(out).not.toContain('Pick from preview');
    expect(out).not.toContain('eyedropper reads the preview still');
    expect(out).toContain('aria-label="Color 1 to remove (hex)"');
    expect(out).toContain('type a hex value');
    expect(addButton(out)).toBeTruthy();
    expect(html(false, false)).toContain('type a color to remove');
    app.ops.background.colors = ['313338', 'facc82'];
    expect(html(false, false)).toContain('2 colors · similarity 0.08');
  });

  it('Screen: Green / Blue sub-choice, key color with Pick from preview, similarity / blend at the recipe defaults (0.10 / 0.05), despill 0.50 / 0.00 in Advanced', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'screen' };
    let out = html();
    expect(pressed(out, 'Screen')).toBe(true);
    expect(pressed(out, 'None')).toBe(false);
    expect(out).toContain('aria-label="Screen color"');
    expect(pressed(out, 'Green')).toBe(true);
    expect(pressed(out, 'Blue')).toBe(false);
    expect(out).toMatch(/<input[^>]*value="#00ff00"[^>]*aria-label="Key color hex"/);
    expect(screenPickButton(out)).toContain('Pick from preview');
    expect(screenPickButton(out)).toContain('aria-pressed="false"');
    expect(out).toMatch(/<input[^>]*type="range"[^>]*aria-label="Similarity"/);
    expect(out).toContain('Similarity (0.01–1) — <b>0.10</b>');
    expect(out).toContain('Blend (0.01–1) — <b>0.05</b>');
    expect(out).toContain('>Despill</span>');
    expect(out).toContain('despill on · mix 0.50 · expand 0.00');
    expect(pickButtons(out)).toHaveLength(0);
    expect(screenHint(out)).toBe('');
    // the collapsed summary: a preset color is not repeated, despill only when off
    out = html(false);
    expect(out).toContain('greenscreen · similarity 0.10 · fill pinholes');
    expect(out).not.toContain('#00ff00');
    app.ops.background.despill = false;
    app.ops.background.similarity = 0.35;
    expect(html(false)).toContain('greenscreen · similarity 0.35 · despill off · fill pinholes');
    // Blue
    app.ops.background.screen = 'blue';
    app.ops.background.color = '0000ff';
    out = html();
    expect(pressed(out, 'Blue')).toBe(true);
    expect(pressed(out, 'Green')).toBe(false);
    expect(out).toMatch(/<input[^>]*value="#0000ff"[^>]*aria-label="Key color hex"/);
    expect(screenHint(out)).toBe('');
    expect(html(false)).toContain('bluescreen · similarity 0.35');
    // a custom key color: the hint, and the summary names it
    app.ops.background.color = '123456';
    out = html();
    expect(screenHint(out)).toBe('custom — despill follows the dominant channel');
    expect(html(false)).toContain('bluescreen #123456 · similarity 0.35');
    // the armed Screen pick
    armScreenEyedropper();
    out = html();
    expect(screenPickButton(out)).toContain('Click the preview…');
    expect(screenPickButton(out)).toContain('aria-pressed="true"');
    // batch: no pick button
    expect(screenPickButton(html(true, false))).toBe('');
    // the Screen sub-choice is gone in Color mode
    app.ops.background.mode = 'color';
    expect(html()).not.toContain('aria-label="Screen color"');
  });

  it('Screen auto-match hints: matching…, matched #… from the preview, no screen found — using the preset', () => {
    setBackgroundMode('screen', { picker: true }); // a source is loaded: the auto-match arms
    expect(app.ui).toMatchObject({ pickColor: true, pickTarget: 'screen-auto' });
    let out = html();
    expect(screenHint(out)).toBe('matching the green screen from the preview…');
    expect(screenPickButton(out)).toContain('aria-pressed="false"'); // the auto-match is not a click pick
    app.ui.pickColor = false;
    app.ops.background.screen = 'blue';
    app.ops.background.color = '143cc8';
    app.ui.screenMatch = { which: 'blue', color: '143cc8' };
    out = html();
    expect(screenHint(out)).toBe('matched #143cc8 from the preview');
    expect(html(false)).toContain('bluescreen #143cc8 · similarity 0.10');
    // the user then edits the key color: custom
    app.ops.background.color = '143cc9';
    expect(screenHint(html())).toBe('custom — despill follows the dominant channel');
    // nothing found: the preset stays
    app.ops.background.screen = 'green';
    app.ops.background.color = '00ff00';
    app.ui.screenMatch = { which: 'green', color: null };
    expect(screenHint(html())).toBe('no green screen found — using #00ff00');
    // a match for the other sub-choice says nothing
    app.ui.screenMatch = { which: 'blue', color: null };
    expect(screenHint(html())).toBe('');
  });

  it('the header checkbox goes through setBackgroundMode: enabling with nothing picked arms the eyedropper for row 0 in the editor, not in batch; off disarms', () => {
    // the editor: what the checkbox setter calls with the card's mode (Color by default)
    setBackgroundMode(app.ops.background.mode, { picker: true });
    expect(app.ops.background.enabled).toBe(true);
    expect(app.ops.background.mode).toBe('color');
    expect(app.ops.background.colors).toEqual(['']);
    expect(app.ui.pickColor).toBe(true);
    expect(app.ui.pickRow).toBe(0);
    let out = html();
    expect(pickButtons(out)[0]).toContain('Click the preview…');
    expect(pickButtons(out)[0]).toContain('aria-pressed="true"');
    // off: disabled and disarmed
    setBackgroundMode('none', { picker: true });
    expect(app.ops.background.enabled).toBe(false);
    expect(app.ui.pickColor).toBe(false);
    expect(html(false)).toContain('>off</span>');
    // batch (typed hex only): nothing to arm
    setBackgroundMode('color', { picker: false });
    expect(app.ops.background.enabled).toBe(true);
    expect(app.ui.pickColor).toBe(false);
    setBackgroundMode('none', { picker: false });
    // a color already picked: enabling keys it, no eyedropper
    app.ops.background.colors = ['313338'];
    setBackgroundMode('color', { picker: true });
    expect(app.ui.pickColor).toBe(false);
    out = html();
    expect(pickButtons(out)[0]).toContain('Pick again');
    // Screen drops the row pick and starts the auto-match instead
    armEyedropper(0);
    setBackgroundMode('screen', { picker: true });
    expect(app.ops.background.mode).toBe('screen');
    expect(app.ops.background.enabled).toBe(true);
    expect(app.ui).toMatchObject({ pickColor: true, pickTarget: 'screen-auto' });
    expect(pressed(html(), 'Screen')).toBe(true);
    // in batch Screen arms nothing
    setBackgroundMode('none', { picker: false });
    setBackgroundMode('screen', { picker: false });
    expect(app.ui.pickColor).toBe(false);
  });

  it('Edges fold on both modes, closed by default: Shift edge ±20, Soft edge 0–10, Fill pinholes on; its summary and the card’s', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'color', colors: ['313338'] };
    let fold = edgeFold(html());
    expect(fold).toBeTruthy();
    expect(fold).not.toMatch(/<details[^>]*\sopen/); // closed by default
    expect(html()).not.toContain('Edge cleanup');
    expect(fold).toContain('>Fill pinholes</span>');
    expect(fold).toMatch(/<input[^>]*type="checkbox"[^>]*checked/);
    expect(fold).toMatch(/<input[^>]*type="range"[^>]*min="-20"[^>]*max="20"[^>]*step="1"[^>]*aria-label="Shift edge"/);
    expect(fold).toMatch(/<input[^>]*type="number"[^>]*min="-20"[^>]*max="20"[^>]*step="1"[^>]*value="0"/);
    expect(fold).toContain('Shift edge (±20) — <b>0</b>');
    expect(fold).toMatch(/<input[^>]*type="range"[^>]*min="0"[^>]*max="10"[^>]*step="0.5"[^>]*aria-label="Soft edge"/);
    expect(fold).toContain('Soft edge (0–10) — <b>off</b>');
    expect(fold).toContain('title="Negative trims the edge inward to remove a leftover rim of background; positive grows the subject. In source pixels."');
    expect(fold).toContain('title="Smooths a jagged outline into a clean edge. For a blurred, faded edge use the Feather card instead."');
    expect(fold).toContain('· fill pinholes'); // the fold's own collapsed summary
    // off → "off", and the card's summary drops the morph part
    app.ops.background.morph = { close: false, grow: 0, smooth: 0 };
    fold = edgeFold(html());
    expect(fold).toContain('· off');
    expect(fold).not.toMatch(/<input[^>]*type="checkbox"[^>]*checked/);
    expect(html(false)).toContain('color #313338 · similarity 0.08</span>');
    expect(html(false)).not.toContain('fill pinholes');
    // a negative shift and a soft edge
    app.ops.background.morph = { close: true, grow: -2, smooth: 1.5 };
    fold = edgeFold(html());
    expect(fold).toContain('· fill pinholes · shift −2 px · soft 1.5 px');
    expect(fold).toContain('Shift edge (±20) — <b>−2 px</b>');
    expect(fold).toContain('Soft edge (0–10) — <b>1.5 px</b>');
    expect(html(false)).toContain('color #313338 · similarity 0.08 · fill pinholes · shift −2 px · soft 1.5 px');
    // a positive shift is signed too
    app.ops.background.morph = { close: false, grow: 3, smooth: 0 };
    expect(edgeFold(html())).toContain('Shift edge (±20) — <b>+3 px</b>');
    expect(html(false)).toContain('color #313338 · similarity 0.08 · shift +3 px</span>');
    // on Screen too
    app.ops.background.mode = 'screen';
    expect(edgeFold(html())).toContain('>Fill pinholes</span>');
    expect(html(false)).toContain('greenscreen · similarity 0.10 · shift +3 px');
    // not while the card is off
    app.ops.background.enabled = false;
    expect(edgeFold(html())).toBe('');
  });

  it('hides the Edges fold on a server without the morph op (features.morph off) and keeps the rest of the card', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'screen' };
    expect(edgeFold(html())).toBeTruthy(); // optimistic until the server answers
    setFeatures({ features: { ...all5a, morph: false } }); // a Phase 4 server
    let out = html();
    expect(edgeFold(out)).toBe('');
    expect(out).toContain('aria-label="Screen color"');
    expect(out).not.toContain('This server does not support background removal');
    app.ops.background = { ...app.ops.background, mode: 'color', colors: ['313338'] };
    expect(edgeFold(html())).toBe('');
    setFeatures({ features: all5a });
    expect(edgeFold(html())).toBeTruthy();
  });

  it('stays visible with a one-line notice on a server without keying support (features.keying off)', () => {
    const notice = 'This server does not support background removal';
    expect(html()).not.toContain(notice); // optimistic until the server answers
    setFeatures({ features: { fit: true, sequence: true, optimize: true } }); // a Phase 2 server
    let out = html();
    expect(out).toMatch(/<p class="note[^"]*"[^>]*>This server does not support background removal/);
    expect(out).toContain('aria-label="Background mode"'); // the card is still there
    expect(html(false)).toContain('off — not supported by this server'); // the collapsed summary says so too
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'screen' };
    out = html();
    expect(out).toContain(notice);
    expect(pressed(out, 'Screen')).toBe(true);
    expect(html(false)).toContain('greenscreen · similarity 0.10');
    setFeatures({ features: { keying: true } });
    expect(html()).not.toContain(notice);
  });
});
