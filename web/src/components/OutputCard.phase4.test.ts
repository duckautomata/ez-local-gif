// Server-side renders of the Output card's Phase 4 additions: MP4/WebM in
// the Format select (chat/custom, attachment/none targets, capability-gated),
// the CRF knob, the hidden alpha knobs + kept matte for video, and the
// gifski encoder toggle.
import { render } from 'svelte/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Capabilities, ProbeInfo, Source } from '../lib/api';
import { enterBatch, exitBatch } from '../lib/batch.svelte';
import { resetFeatures, setFeatures } from '../lib/capabilities.svelte';
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
const gifSrc: Source = { hash: 'e'.repeat(64), name: 'test.gif', size: 100, info: gifInfo };

function html(): string {
  return render(OutputCard, { props: {} }).body;
}
function formatSelect(out: string): string {
  const m = out.match(/<select[^>]*>[\s\S]*?<\/select>/); // the Format select is the first one
  return m ? m[0] : '';
}
function targetSelect(out: string): string {
  const m = out.match(/<select[^>]*aria-label="Discord target"[^>]*>[\s\S]*?<\/select>/);
  return m ? m[0] : '';
}
/** a Phase 4 capabilities answer (everything on, all formats) */
function phase4Caps(over: Partial<Capabilities> = {}): Capabilities {
  return {
    tools: {},
    limits: {},
    rulesVersion: 'x',
    features: { fit: true, sequence: true, optimize: true, keying: true, overlays: true, proxy: true, fonts: true, inputPick: true, outputSave: true, gifski: true },
    formats: ['gif', 'webp', 'apng', 'avif', 'png', 'jpeg', 'frames', 'mp4', 'webm'],
    ...over,
  };
}

describe('OutputCard Phase 4 (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
    applyPreset('chat');
    app.output.encoder = '';
  });
  afterEach(() => {
    exitBatch();
    resetFeatures();
  });

  it('Chat offers MP4 / WebM; picking MP4 shows the CRF knob, keeps the matte and drops emote/sticker targets', () => {
    let out = html();
    expect(formatSelect(out)).toContain('MP4 (H.264 video)');
    expect(formatSelect(out)).toContain('WebM (VP9 video)');
    app.output.format = 'mp4';
    app.output.quality = 0;
    out = html();
    expect(out).toContain('x264 CRF');
    expect(out).toContain('20 (default)');
    // video can never be an emote/sticker: the rows are gone from the dropdown
    const tsel = targetSelect(out);
    expect(tsel).not.toContain('value="emote"');
    expect(tsel).not.toContain('value="sticker"');
    expect(tsel).toContain('value="attachment"');
    // matte stays (flattened onto it), the alpha-only knobs are gone
    expect(out).toContain('flattened onto this colour');
    expect(out).not.toContain('Alpha threshold');
    expect(out).not.toContain('Trim fringe');
    expect(out).not.toContain('>Dither<');
    expect(out).not.toContain('Loop count'); // no loop semantics for video
    // the format hint mentions the flatten / even-dims / no-audio contract
    expect(out).toContain('odd dimensions are made even');
  });

  it('WebM shows the VP9 CRF with its own range', () => {
    app.output.format = 'webm';
    app.output.quality = 35;
    const out = html();
    expect(out).toContain('VP9 CRF');
    expect(out).toMatch(/max="63"/);
  });

  it('an emote/sticker target hides MP4 / WebM from the Format select', () => {
    applyPreset('custom');
    app.output.target = 'emote';
    const out = html();
    const fsel = formatSelect(out);
    expect(fsel).toContain('GIF');
    expect(fsel).not.toContain('MP4');
    expect(fsel).not.toContain('WebM');
  });

  it('a pre-Phase-4 server (formats without mp4/webm) hides the video formats', () => {
    setFeatures(phase4Caps({ formats: ['gif', 'webp', 'apng', 'avif', 'png', 'jpeg', 'frames'] }));
    const out = html();
    const fsel = formatSelect(out);
    expect(fsel).not.toContain('MP4');
    expect(fsel).not.toContain('WebM');
  });

  it('gifski: the Advanced Encoder select appears for gif + attachment/none, and switches the knobs', () => {
    setFeatures(phase4Caps());
    let out = html(); // chat gif + attachment
    expect(out).toContain('ffmpeg palette (default)');
    expect(out).toContain('gifski (HQ, slow)');
    app.output.encoder = 'gifski';
    out = html();
    expect(out).toContain('gifski quality');
    expect(out).toContain('encoder gifski');
    expect(out).not.toContain('Lossy (gifsicle');
    expect(out).not.toContain('>Dither<');
    expect(out).not.toContain('Matte —'); // gifski quantises itself
    // emote target: the toggle disappears (server refuses gifski emotes)
    app.output.target = 'emote';
    out = html();
    expect(out).not.toContain('gifski (HQ, slow)');
    expect(out).toContain('Lossy (gifsicle'); // palette knobs are back
  });

  // WEB-4: a frames result has no primary file, so a batch row could never
  // show or save it — batch mode must not offer Frames at all.
  it('batch mode disables the Frames preset chip and hides frames from the Format select', () => {
    applyPreset('custom');
    enterBatch();
    let out = html();
    expect(out).toContain('Frames has no single output file'); // the disabled chip's title
    expect(formatSelect(out)).not.toContain('Frames (zip + grid)'); // Custom offers it otherwise
    exitBatch();
    out = html();
    expect(out).not.toContain('Frames has no single output file');
    expect(formatSelect(out)).toContain('Frames (zip + grid)');
  });

  it('a server without features.gifski never offers the encoder', () => {
    setFeatures(phase4Caps({ features: { fit: true, sequence: true, optimize: true, keying: true, overlays: true, proxy: true, fonts: true, inputPick: true, outputSave: true, gifski: false } }));
    const out = html();
    expect(out).not.toContain('gifski (HQ, slow)');
  });
});
