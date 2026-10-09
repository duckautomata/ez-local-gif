// Server-side renders of the Background card: the None · AI · Colour ·
// Screen segment, the Colour rows (eyedropper / typed hex / "+ add colour",
// the armed row), the Screen sub-choice with its sliders and the Advanced
// despill fold, the Edge cleanup fold gated on features.morph, the summary
// lines, the batch variant (typed hex only) and the notice for a server
// without keying support — and (Phase 5b) the AI mode: the Model select fed
// by /api/matte, its status line, and the mode disabled with the reason on
// a server without features.matte.
import { render } from 'svelte/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { MatteStatus, ProbeInfo, Source } from '../../lib/api';
import { resetFeatures, setFeatures } from '../../lib/capabilities.svelte';
import { matte, resetMatte, setMatteStatus } from '../../lib/matte.svelte';
import { app, armEyedropper, setBackgroundMode, setSource } from '../../lib/state.svelte';
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
/** every feature a Phase 5a server reports (matte joins at 5b: true while the sidecar answers) */
const all5a = { fit: true, sequence: true, optimize: true, keying: true, overlays: true, proxy: true, fonts: true, feather: true, bounce: true, inputPick: true, outputSave: true, gifski: true, morph: true, matte: true };
/** a GPU install's /api/matte: the fast model ready at 18 ms, the precise one loading */
function gpuStatus(over: Partial<MatteStatus> = {}): MatteStatus {
  return {
    enabled: true,
    device: 'cuda',
    gpu: { name: 'NVIDIA GeForce RTX 5080', totalGiB: 16, freeGiB: 12.4 },
    defaultModel: 'isnet-anime',
    models: {
      'isnet-anime': { label: 'Anime (fast)', state: 'ready', msPerFrame: 18, licence: 'Apache-2.0' },
      'birefnet-lite': { label: 'General (precise)', state: 'loading', percent: 60, msPerFrame: 170, licence: 'MIT' },
    },
    maxSeconds: 600,
    maxFrames: 3000,
    ...over,
  };
}

function html(initialOpen = true, picker = true): string {
  return render(BackgroundCard, { props: { initialOpen, picker } }).body;
}
/** the mode segment's button tag for that label */
function modeButton(out: string, label: string): string {
  return out.match(new RegExp(`<button[^>]*>${label}</button>`))?.[0] ?? '';
}
/** the Model <select> ('' when absent) and its <option> tags */
function modelSelect(out: string): string {
  return out.match(/<select[^>]*aria-label="Model"[^>]*>[\s\S]*?<\/select>/)?.[0] ?? '';
}
function options(out: string): string[] {
  return modelSelect(out).match(/<option[^>]*>[^<]*<\/option>/g) ?? [];
}
/** the status line's text */
function statusText(out: string): string {
  return out.match(/<span class="status[^"]*"[^>]*role="status"[^>]*>([\s\S]*?)<\/span>/)?.[1]?.trim() ?? '';
}
/** the aria-pressed value of the button with that label (mode segment, screen sub-choice) */
function pressed(out: string, label: string): boolean {
  const m = out.match(new RegExp(`<button[^>]*aria-pressed="(true|false)"[^>]*>${label}</button>`));
  return m?.[1] === 'true';
}
/** the eyedropper buttons of the Colour rows, in row order */
function pickButtons(out: string): string[] {
  return out.match(/<button[^>]*title="Then click the colour on the preview"[^>]*>[\s\S]*?<\/button>/g) ?? [];
}
/** the "+ add colour" button tag */
function addButton(out: string): string {
  return out.match(/<button[^>]*>\+ add colour<\/button>/)?.[0] ?? '';
}
/** the Edge cleanup <details> element ('' when absent) */
function edgeFold(out: string): string {
  return out.match(/<details[^>]*><summary[^>]*><span[^>]*>Edge cleanup<\/span>[\s\S]*?<\/details>/)?.[0] ?? '';
}

describe('BackgroundCard (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
  });
  afterEach(() => {
    resetFeatures();
    resetMatte();
  });

  it('starts off: summary "off", the four modes with None pressed, no sliders, no Edge cleanup', () => {
    expect(html(false)).toContain('>off</span>');
    const out = html();
    expect(out).toContain('aria-label="Background mode"');
    for (const m of ['None', 'AI', 'Colour', 'Screen']) expect(out, m).toContain(`>${m}</button>`);
    for (const m of ['Greenscreen', 'Bluescreen', 'Pick a colour']) expect(out, m).not.toContain(`>${m}</button>`);
    expect(out.indexOf('>AI</button>')).toBeLessThan(out.indexOf('>Colour</button>')); // None · AI · Colour · Screen
    expect(pressed(out, 'None')).toBe(true);
    expect(pressed(out, 'AI')).toBe(false);
    expect(pressed(out, 'Colour')).toBe(false);
    expect(pressed(out, 'Screen')).toBe(false);
    expect(modeButton(out, 'AI')).not.toContain('disabled'); // optimistic until the server answers
    expect(out).not.toContain('aria-label="Similarity"');
    expect(modelSelect(out)).toBe('');
    expect(edgeFold(out)).toBe('');
    expect(out).toContain('Pick a mode to make the background transparent');
  });

  it('enabling the card lands on Colour: one empty row with the eyedropper and a hex field, "+ add colour", sliders at 0.08 / 0', () => {
    app.ops.background.enabled = true; // the flag alone (a restored session): the default mode
    let out = html();
    expect(pressed(out, 'Colour')).toBe(true);
    expect(pressed(out, 'Screen')).toBe(false);
    expect(pressed(out, 'None')).toBe(false);
    expect(out).toContain('Colour to remove');
    expect(pickButtons(out)).toHaveLength(1);
    expect(pickButtons(out)[0]).toContain('Pick from preview');
    expect(out).toContain('nothing picked yet');
    expect(out).toMatch(/<input[^>]*type="text"[^>]*placeholder="#rrggbb"[^>]*aria-label="Colour 1 to remove \(hex\)"/);
    expect(addButton(out)).toBeTruthy();
    expect(addButton(out)).not.toContain('disabled');
    expect(out).toContain('Similarity (0.01–1) — <b>0.08</b>');
    expect(out).toContain('Blend (0–1) — <b>0.00</b>');
    expect(out).not.toContain('>Despill</span>'); // colorkey has no despill
    expect(out).not.toContain('aria-label="Screen colour"');
    expect(out).not.toContain('aria-label="Remove colour 1"'); // a single empty row has nothing to remove
    expect(html(false)).toContain('pick a colour on the preview');
    // once picked: swatch + hex, "Pick again", a clear button, the summary names the colour
    app.ops.background.colors = ['313338'];
    app.ops.background.pickSimilarity = 0.25;
    out = html();
    expect(pickButtons(out)[0]).toContain('Pick again');
    expect(out).toMatch(/class="swatch[^"]*"[^>]*background: #313338/);
    expect(out).toMatch(/<input[^>]*value="#313338"[^>]*aria-label="Colour 1 to remove \(hex\)"/);
    expect(out).toContain('aria-label="Remove colour 1"');
    expect(out).toContain('Similarity (0.01–1) — <b>0.25</b>');
    expect(html(false)).toContain('colour #313338 · similarity 0.25 · fill pinholes');
    // the armed eyedropper shows on its row's button
    app.ui.pickColor = true;
    app.ui.pickRow = 0;
    out = html();
    expect(pickButtons(out)[0]).toContain('Click the preview…');
    expect(pickButtons(out)[0]).toContain('aria-pressed="true"');
  });

  it('"+ add colour" rows: each with its own eyedropper / hex / remove; only the armed row says "Click the preview…"; capped at 6', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'colour', colors: ['313338', ''] };
    app.ui.pickColor = true;
    app.ui.pickRow = 1;
    let out = html();
    expect(out).toContain('Colours to remove');
    expect(out).toContain('aria-label="Colour 1 to remove (hex)"');
    expect(out).toContain('aria-label="Colour 2 to remove (hex)"');
    expect(out).toContain('aria-label="Remove colour 1"');
    expect(out).toContain('aria-label="Remove colour 2"');
    const buttons = pickButtons(out);
    expect(buttons).toHaveLength(2);
    expect(buttons[0]).toContain('Pick again');
    expect(buttons[0]).toContain('aria-pressed="false"');
    expect(buttons[1]).toContain('Click the preview…');
    expect(buttons[1]).toContain('aria-pressed="true"');
    // the summary counts the picked rows only
    expect(html(false)).toContain('colour #313338 · similarity 0.08 · fill pinholes');
    app.ops.background.colors = ['313338', 'facc82'];
    expect(html(false)).toContain('2 colours · similarity 0.08 · fill pinholes');
    // six rows: no seventh
    app.ops.background.colors = ['111111', '222222', '333333', '444444', '555555', '666666'];
    out = html();
    expect(pickButtons(out)).toHaveLength(6);
    expect(addButton(out)).toContain('disabled');
    expect(html(false)).toContain('6 colours · similarity 0.08');
    app.ops.background.colors = ['111111'];
    expect(addButton(html())).not.toContain('disabled');
  });

  it('in batch (picker off) Colour takes typed hex only: no eyedropper, the hex field stays, the summary asks for a value', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'colour', colors: [''] };
    const out = html(true, false);
    expect(pickButtons(out)).toHaveLength(0);
    expect(out).not.toContain('Pick from preview');
    expect(out).not.toContain('eyedropper reads the preview still');
    expect(out).toContain('aria-label="Colour 1 to remove (hex)"');
    expect(out).toContain('type a hex value');
    expect(addButton(out)).toBeTruthy();
    expect(html(false, false)).toContain('type a colour to remove');
    app.ops.background.colors = ['313338', 'facc82'];
    expect(html(false, false)).toContain('2 colours · similarity 0.08');
  });

  it('Screen: Green / Blue sub-choice, key colour, similarity / blend at the recipe defaults (0.10 / 0.05), despill in Advanced', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'screen' };
    let out = html();
    expect(pressed(out, 'Screen')).toBe(true);
    expect(pressed(out, 'None')).toBe(false);
    expect(out).toContain('aria-label="Screen colour"');
    expect(pressed(out, 'Green')).toBe(true);
    expect(pressed(out, 'Blue')).toBe(false);
    expect(out).toMatch(/<input[^>]*value="#00ff00"[^>]*aria-label="Key colour hex"/);
    expect(out).toMatch(/<input[^>]*type="range"[^>]*aria-label="Similarity"/);
    expect(out).toContain('Similarity (0.01–1) — <b>0.10</b>');
    expect(out).toContain('Blend (0.01–1) — <b>0.05</b>');
    expect(out).toContain('>Despill</span>');
    expect(out).toContain('despill on · mix 0.60 · expand 0.30');
    expect(pickButtons(out)).toHaveLength(0);
    expect(out).not.toContain('custom — despill follows');
    // the collapsed summary: a preset colour is not repeated, despill only when off
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
    expect(out).toMatch(/<input[^>]*value="#0000ff"[^>]*aria-label="Key colour hex"/);
    expect(out).not.toContain('custom — despill follows');
    expect(html(false)).toContain('bluescreen · similarity 0.35');
    // a custom key colour: the hint, and the summary names it
    app.ops.background.color = '123456';
    out = html();
    expect(out).toContain('custom — despill follows the dominant channel');
    expect(html(false)).toContain('bluescreen #123456 · similarity 0.35');
    // the Screen sub-choice is gone in Colour mode
    app.ops.background.mode = 'colour';
    expect(html()).not.toContain('aria-label="Screen colour"');
  });

  it('the header checkbox goes through setBackgroundMode: enabling with nothing picked arms the eyedropper for row 0 in the editor, not in batch; off disarms', () => {
    // the editor: what the checkbox setter calls with the card's mode (Colour by default)
    setBackgroundMode(app.ops.background.mode, { picker: true });
    expect(app.ops.background.enabled).toBe(true);
    expect(app.ops.background.mode).toBe('colour');
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
    setBackgroundMode('colour', { picker: false });
    expect(app.ops.background.enabled).toBe(true);
    expect(app.ui.pickColor).toBe(false);
    setBackgroundMode('none', { picker: false });
    // a colour already picked: enabling keys it, no eyedropper
    app.ops.background.colors = ['313338'];
    setBackgroundMode('colour', { picker: true });
    expect(app.ui.pickColor).toBe(false);
    out = html();
    expect(pickButtons(out)[0]).toContain('Pick again');
    // Screen disarms whatever was armed
    armEyedropper(0);
    setBackgroundMode('screen', { picker: true });
    expect(app.ops.background.mode).toBe('screen');
    expect(app.ops.background.enabled).toBe(true);
    expect(app.ui.pickColor).toBe(false);
    expect(pressed(html(), 'Screen')).toBe(true);
  });

  it('Edge cleanup fold on both modes, closed by default: Fill pinholes on, Grow matte 0–4 with its hint; its summary and the card’s', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'colour', colors: ['313338'] };
    let fold = edgeFold(html());
    expect(fold).toBeTruthy();
    expect(fold).not.toMatch(/<details[^>]*\sopen/); // closed by default
    expect(fold).toContain('>Fill pinholes</span>');
    expect(fold).toMatch(/<input[^>]*type="checkbox"[^>]*checked/);
    // Grow sits inline with its number box (one baseline with Fill pinholes): "Grow matte [0] source px"
    expect(fold).toMatch(/<label class="inline"[^>]*><span>Grow matte<\/span>\s*<input[^>]*type="number"[^>]*min="0"[^>]*max="4"[^>]*step="1"[^>]*value="0"[^>]*>(?:\s|<!---->)*<span>source px<\/span>/);
    expect(fold).toContain('+1–2 recovers eaten interiors at a 2 px fringe');
    expect(fold).toContain('4 px on a 720 px source is');
    expect(fold).toContain('Soft edges are the Feather card');
    expect(fold).toContain('· fill pinholes'); // the fold's own collapsed summary
    // off → "off", and the card's summary drops the morph part
    app.ops.background.morph = { close: false, grow: 0 };
    fold = edgeFold(html());
    expect(fold).toContain('· off');
    expect(fold).not.toMatch(/<input[^>]*type="checkbox"[^>]*checked/);
    expect(html(false)).toContain('colour #313338 · similarity 0.08</span>');
    expect(html(false)).not.toContain('fill pinholes');
    // grow
    app.ops.background.morph = { close: true, grow: 2 };
    fold = edgeFold(html());
    expect(fold).toContain('· fill pinholes · grow 2 px');
    expect(fold).toMatch(/<span>Grow matte<\/span>\s*<input[^>]*type="number"[^>]*max="4"[^>]*value="2"/);
    expect(html(false)).toContain('colour #313338 · similarity 0.08 · fill pinholes · grow 2 px');
    // on Screen too
    app.ops.background.mode = 'screen';
    expect(edgeFold(html())).toContain('>Fill pinholes</span>');
    expect(html(false)).toContain('greenscreen · similarity 0.10 · fill pinholes · grow 2 px');
    // not while the card is off
    app.ops.background.enabled = false;
    expect(edgeFold(html())).toBe('');
  });

  it('hides the Edge cleanup fold on a server without the morph op (features.morph off) and keeps the rest of the card', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'screen' };
    expect(edgeFold(html())).toBeTruthy(); // optimistic until the server answers
    setFeatures({ features: { ...all5a, morph: false } }); // a Phase 4 server
    let out = html();
    expect(edgeFold(out)).toBe('');
    expect(out).toContain('aria-label="Screen colour"');
    expect(out).not.toContain('This server does not support background removal');
    app.ops.background = { ...app.ops.background, mode: 'colour', colors: ['313338'] };
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

  // ---- Phase 5b: the AI mode
  it('AI: the Model select is fed by /api/matte (labels, states, the server default), the status line reads ready · GPU · the clip’s estimate, the Edge cleanup fold is shared', () => {
    setFeatures({ features: all5a });
    setMatteStatus(gpuStatus());
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'ai' };
    let out = html();
    expect(pressed(out, 'AI')).toBe(true);
    expect(pressed(out, 'None')).toBe(false);
    expect(modelSelect(out)).toBeTruthy();
    const opts = options(out);
    expect(opts).toHaveLength(2);
    expect(opts[0]).toMatch(/<option value="isnet-anime"[^>]*selected[^>]*>Anime \(fast\)<\/option>/);
    expect(opts[0]).not.toContain('disabled');
    expect(opts[1]).toMatch(/<option value="birefnet-lite"[^>]*>General \(precise\) — loading…<\/option>/);
    expect(opts[1]).not.toContain('selected');
    // 50 frames × (18 + 2) ms = 1 s
    expect(statusText(out)).toBe('ready · GPU · up to ~1 s for this clip');
    expect(out).toContain('Computed once per frame and cached');
    expect(edgeFold(out)).toContain('>Fill pinholes</span>');
    expect(out).not.toContain('aria-label="Similarity"');
    expect(out).not.toContain('aria-label="Screen colour"');
    expect(pickButtons(out)).toHaveLength(0);
    expect(html(false)).toContain('>AI · fill pinholes</span>');
    // the precise model chosen: selected, the status line is its state, the summary names it
    app.ops.background.ai.model = 'birefnet-lite';
    out = html();
    expect(options(out)[1]).toContain('selected');
    expect(options(out)[0]).not.toContain('selected');
    expect(statusText(out)).toBe('loading model…');
    expect(html(false)).toContain('>AI · General (precise) · fill pinholes</span>');
    // the server's default is what shows while nothing is chosen
    app.ops.background.ai.model = '';
    setMatteStatus(gpuStatus({ defaultModel: 'birefnet-lite' }));
    out = html();
    expect(options(out)[1]).toContain('selected');
    expect(html(false)).toContain('AI · General (precise)');
    // a model that is unavailable is greyed with the sidecar's reason; downloading shows its percentage
    setMatteStatus(
      gpuStatus({
        device: 'cpu',
        gpu: null,
        models: {
          'isnet-anime': { label: '', state: 'downloading', percent: 43, msPerFrame: 136 },
          'birefnet-lite': { label: '', state: 'unavailable', reason: 'cpu: 14 GiB of RAM needed, 9 GiB available' },
        },
      }),
    );
    out = html();
    expect(options(out)[0]).toMatch(/<option value="isnet-anime"[^>]*>Anime \(fast\) — downloading 43 %<\/option>/);
    expect(options(out)[1]).toMatch(/<option value="birefnet-lite"[^>]*disabled[^>]*>General \(precise\) — unavailable<\/option>/);
    expect(options(out)[1]).toContain('title="cpu: 14 GiB of RAM needed, 9 GiB available"');
    expect(statusText(out)).toBe('downloading weights 43 %');
    app.ops.background.ai.model = 'birefnet-lite';
    expect(statusText(html())).toBe('General (precise) unavailable — cpu: 14 GiB of RAM needed, 9 GiB available');
    // a chosen model the server does not offer: kept, flagged
    app.ops.background.ai.model = 'rmbg-2';
    out = html();
    expect(options(out)).toHaveLength(3);
    expect(options(out)[2]).toMatch(/<option value="rmbg-2"[^>]*selected[^>]*>rmbg-2 — not offered by this server<\/option>/);
    expect(statusText(out)).toBe('model rmbg-2 is not offered by this server');
    // the disarm: the eyedropper never stays armed in AI mode
    app.ops.background.ai.model = '';
    app.ops.background.mode = 'colour';
    expect(modelSelect(html())).toBe('');
  });

  it('AI with no /api/matte answer yet: "checking…", a failed read names the error, no options but the default', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'ai' };
    let out = html();
    expect(statusText(out)).toBe('checking the matte service…');
    expect(options(out)).toHaveLength(1);
    expect(options(out)[0]).toMatch(/<option value="isnet-anime"[^>]*selected[^>]*>isnet-anime<\/option>/);
    matte.error = 'HTTP 404 Not Found';
    out = html();
    expect(statusText(out)).toBe('matte status unavailable — HTTP 404 Not Found');
  });

  it('AI is disabled with the reason when features.matte is off (a plain install), and the status line names the compose profile', () => {
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'screen' };
    expect(modeButton(html(), 'AI')).not.toContain('disabled'); // optimistic until the server answers
    // a 5b server without the profile running: the flag is false, /api/matte carries the reason
    setFeatures({ features: { ...all5a, matte: false } });
    let btn = modeButton(html(), 'AI');
    expect(btn).toContain('disabled');
    expect(btn).toContain('title="This server has no AI matte sidecar (features.matte off) — run `docker compose --profile matte-gpu up -d` next to it, or update the server"');
    setMatteStatus({ enabled: false, device: '', reason: 'no matte service is configured (EZLG_MATTE_URL is empty — start the matte or matte-gpu compose profile)', defaultModel: 'isnet-anime', models: {}, maxSeconds: 600, maxFrames: 3000 });
    btn = modeButton(html(), 'AI');
    expect(btn).toContain('disabled');
    expect(btn).toContain('title="AI matte is off on this server: no matte service is configured (EZLG_MATTE_URL is empty — start the matte or matte-gpu compose profile)"');
    // the rest of the card is untouched
    expect(html()).toContain('aria-label="Screen colour"');
    expect(html()).not.toContain('This server does not support background removal');
    // AI selected when the sidecar went away mid-session: the note, the status line with the hint, the summary says so
    app.ops.background.mode = 'ai';
    const out = html();
    expect(out).toMatch(/<p class="note[^"]*"[^>]*>AI matte is off on this server: no matte service is configured .* — the matte op will be rejected at render\.<\/p>/);
    expect(statusText(out)).toBe('sidecar unavailable — run `docker compose --profile matte-gpu up -d` (no matte service is configured (EZLG_MATTE_URL is empty — start the matte or matte-gpu compose profile))');
    expect(out).toMatch(/<span class="status[^"]*bad[^"]*"/);
    expect(html(false)).toContain('>AI — not available on this server · fill pinholes</span>');
    // a Phase 5a server: no name at all → the same disabled button with the standard note
    setMatteStatus(null);
    setFeatures({ features: { ...all5a, matte: undefined as unknown as boolean } });
    expect(modeButton(html(), 'AI')).toContain('disabled');
    // the sidecar back: enabled again
    setFeatures({ features: all5a });
    expect(modeButton(html(), 'AI')).not.toContain('disabled');
  });

  it('AI in batch (picker off): offered with the same Model select and status line', () => {
    setFeatures({ features: all5a });
    setMatteStatus(gpuStatus());
    app.ops.background = { ...app.ops.background, enabled: true, mode: 'ai' };
    const out = html(true, false);
    expect(modeButton(out, 'AI')).not.toContain('disabled');
    expect(pressed(out, 'AI')).toBe(true);
    expect(options(out)).toHaveLength(2);
    // no source in batch (rows carry theirs): no clip estimate on the line
    app.source = null;
    expect(statusText(html(true, false))).toBe('ready · GPU');
    expect(html(false, false)).toContain('>AI · fill pinholes</span>');
  });
});
