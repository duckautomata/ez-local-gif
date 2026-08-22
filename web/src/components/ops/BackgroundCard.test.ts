// Server-side renders of the Background card: the mode control, the
// chroma sliders + Advanced despill fold, the pick-a-colour state, and the
// notice for a server without keying support.
import { render } from 'svelte/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../../lib/api';
import { resetFeatures, setFeatures } from '../../lib/capabilities.svelte';
import { app, setSource } from '../../lib/state.svelte';
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

function html(initialOpen = true): string {
  return render(BackgroundCard, { props: { initialOpen } }).body;
}
/** the aria-pressed value of the mode button with that label */
function pressed(out: string, label: string): boolean {
  const m = out.match(new RegExp(`<button[^>]*aria-pressed="(true|false)"[^>]*>${label}</button>`));
  return m?.[1] === 'true';
}

describe('BackgroundCard (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
  });
  afterEach(() => resetFeatures());

  it('starts off: summary "off", the four modes with None pressed, no sliders', () => {
    expect(html(false)).toContain('>off</span>');
    const out = html();
    expect(out).toContain('aria-label="Background mode"');
    for (const m of ['None', 'Greenscreen', 'Bluescreen', 'Pick a colour']) expect(out, m).toContain(`>${m}</button>`);
    expect(pressed(out, 'None')).toBe(true);
    expect(pressed(out, 'Greenscreen')).toBe(false);
    expect(out).not.toContain('aria-label="Similarity"');
  });

  it('greenscreen: key colour, similarity / blend sliders at the recipe defaults, despill in Advanced', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'green' };
    let out = html();
    expect(pressed(out, 'Greenscreen')).toBe(true);
    expect(pressed(out, 'None')).toBe(false);
    expect(out).toMatch(/<input[^>]*value="#00ff00"[^>]*aria-label="Key colour hex"/);
    expect(out).toMatch(/<input[^>]*type="range"[^>]*aria-label="Similarity"/);
    expect(out).toContain('Similarity (0.01–1) — <b>0.20</b>');
    expect(out).toContain('Blend (0.01–1) — <b>0.05</b>');
    expect(out).toContain('>Despill</span>');
    expect(out).toContain('despill on · mix 0.60 · expand 0.30');
    expect(out).not.toContain('Pick from preview');
    // the collapsed summary
    out = html(false);
    expect(out).toContain('greenscreen #00ff00 · similarity 0.20 · despill');
    app.ops.background.despill = false;
    app.ops.background.similarity = 0.35;
    out = html(false);
    expect(out).toContain('greenscreen #00ff00 · similarity 0.35');
    expect(out).not.toContain('· despill');
    app.ops.background.mode = 'blue';
    app.ops.background.color = '0000ff';
    expect(pressed(html(), 'Bluescreen')).toBe(true);
    expect(html(false)).toContain('bluescreen #0000ff');
    app.ops.background.color = '123456';
    expect(html()).toContain('custom — despill follows the dominant channel');
  });

  it('pick a colour: eyedropper button, swatch + hex once picked, its own sliders', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'pick', pickColor: '' };
    let out = html();
    expect(pressed(out, 'Pick a colour')).toBe(true);
    expect(out).toContain('Pick from preview');
    expect(out).toContain('nothing picked yet');
    expect(html(false)).toContain('pick a colour on the preview');
    app.ops.background.pickColor = '313338';
    app.ops.background.pickSimilarity = 0.25;
    out = html();
    expect(out).toContain('Pick again');
    expect(out).toMatch(/<input[^>]*value="#313338"[^>]*aria-label="Colour to remove \(hex\)"/);
    expect(out).toContain('Similarity (0.01–1) — <b>0.25</b>');
    expect(out).not.toContain('>Despill</span>'); // colorkey has no despill
    expect(html(false)).toContain('colour #313338 · similarity 0.25');
    // the armed eyedropper shows on the button
    app.ui.pickColor = true;
    expect(html()).toContain('Click the preview…');
  });

  it('stays visible with a one-line notice on a server without keying support (features.keying off)', () => {
    const notice = 'This server does not support background removal';
    expect(html()).not.toContain(notice); // optimistic until the server answers
    setFeatures({ features: { fit: true, sequence: true, optimize: true } }); // a Phase 2 server
    let out = html();
    expect(out).toMatch(/<p class="note[^"]*"[^>]*>This server does not support background removal/);
    expect(out).toContain('aria-label="Background mode"'); // the card is still there
    expect(html(false)).toContain('off — not supported by this server'); // the collapsed summary says so too
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'green' };
    out = html();
    expect(out).toContain(notice);
    expect(pressed(out, 'Greenscreen')).toBe(true);
    expect(html(false)).toContain('greenscreen #00ff00');
    setFeatures({ features: { keying: true } });
    expect(html()).not.toContain(notice);
  });
});
