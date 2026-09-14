// Server-side renders of the Render panel: the frame-master estimate under
// the Render row (state.planMaster) and its refusal note, which appears only
// when the server's published cap says the render would be refused — never
// while the caps are unknown (an older server, no answer yet) and never
// under the cap. The Render button stays enabled either way.
import { render } from 'svelte/server';
import { afterEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../lib/api';
import { resetFeatures, setFeatures } from '../lib/capabilities.svelte';
import { app, applyPreset, resetApp, setSource } from '../lib/state.svelte';
import RenderPanel from './RenderPanel.svelte';

/** the user's report: 2560×1440 × 704 frames at 30 fps = 9.7 GiB untrimmed */
const clip4k: ProbeInfo = {
  format: 'mov,mp4,m4a,3gp,3g2,mj2',
  codec: 'h264',
  pixFmt: 'yuv420p',
  bits: 8,
  width: 2560,
  height: 1440,
  fps: 30,
  duration: 23.4667,
  frames: 704,
  hasAlpha: false,
  hasAudio: true,
  isStill: false,
  kind: 'video',
  premultiplied: false,
};
const src: Source = { hash: 'a'.repeat(64), name: 'clip.mp4', size: 1, info: clip4k };
const features = { fit: true, sequence: true, optimize: true, keying: true, overlays: true, proxy: true, fonts: true, feather: true, bounce: true, inputPick: false, outputSave: false, gifski: false };
const GIB = 2 ** 30;

function html(): string {
  return render(RenderPanel, { props: {} }).body;
}

/** the text of the first <p class="…estimate…"> (tags stripped) */
function estimateText(out: string): string {
  return (out.match(/<p class="[^"]*estimate[^"]*"[^>]*>([\s\S]*?)<\/p>/)?.[1] ?? '').replace(/<[^>]+>/g, '');
}

describe('RenderPanel (SSR): frame-master estimate', () => {
  afterEach(() => {
    resetFeatures();
    resetApp();
  });

  it('shows the muted estimate alone while the caps are unknown, whatever the size', () => {
    setSource(src);
    applyPreset('chat');
    const out = html();
    expect(out).toContain('2560×1440 · 704 frames · ~9.7 GiB of decoded frames');
    expect(out).not.toContain('note error');
    expect(out).not.toContain('Render will be refused');
    expect(out).toMatch(/<p class="estimate small muted[^"]*"/);
  });

  it('over the published cap the line becomes the refusal note with what fits, and Render stays enabled', () => {
    setSource(src);
    applyPreset('chat');
    setFeatures({ features, formats: ['gif', 'mp4'], maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 });
    const out = html();
    const note = estimateText(out);
    expect(out).toMatch(/<p class="note error estimate[^"]*"/);
    expect(note).toContain("~9.7 GiB of decoded frames — over this server's 2 GiB limit, so Render will be refused.");
    expect(note).toContain('At 2560×1440 about 145 frames fit (4.8 s at 30 fps): trim to that, lower the fps, crop or resize the output (or raise EZLG_MAX_MASTER_BYTES on the server).');
    // the button is not gated on the verdict (the server's admission has the last word)
    const button = out.match(/<button[^>]*class="primary big[^"]*"[^>]*>/)?.[0] ?? '';
    expect(button).not.toBe('');
    expect(button).not.toContain('disabled');
  });

  it('under the cap — trim + crop + the emote preset — the muted line is back, with the scratch multiple of the fit', () => {
    setSource(src);
    app.ops.trim = { enabled: true, start: 0, end: 3 };
    app.ops.crop = { enabled: true, x: 0, y: 0, w: 800, h: 600 };
    applyPreset('emote');
    setFeatures({ features, formats: ['gif'], maxMasterBytes: 2 * GIB, scratchBudgetBytes: 3 * GIB });
    const out = html();
    expect(out).not.toContain('note error');
    expect(out).toContain('128×128 · 75 frames · ~4.7 MiB of decoded frames · about 2× that on scratch');
  });

  it('names the scratch budget when it binds below the cap (a fit render), with the shm_size fix', () => {
    setSource(src);
    // 1920×1080 × 200 frames = 1.5 GiB: under a 2 GiB cap, but the emote-style fit doubles the reservation past a 2.5 GiB budget
    app.ops.trim = { enabled: true, start: 0, end: 200 / 30 };
    app.ops.resize = { enabled: true, width: 1920, height: 1080, fit: 'exact' };
    applyPreset('chat');
    applyPreset('custom'); // keeps chat's gif; the fit row is the user's
    app.output.fitEnabled = true;
    app.output.fitKiB = 8000;
    setFeatures({ features, formats: ['gif'], maxMasterBytes: 2 * GIB, scratchBudgetBytes: 2.5 * GIB });
    const note = estimateText(html());
    // the figure is the budget the server published — shm_size minus the still/proxy memos (jobs.NewManager) — never labelled as shm_size itself
    expect(note).toContain("~1.5 GiB of decoded frames (about 2× that on scratch) — more than this server's scratch budget (2.5 GiB, shm_size minus the preview cache) can hold for this output, so Render will be refused.");
    expect(note).toContain('(or raise shm_size / point EZLG_SCRATCH at a larger filesystem)');
    expect(note).not.toContain('EZLG_MAX_MASTER_BYTES');
  });

  it('flags the pre-crop figure as an upper bound with auto-crop on, and only says the render MAY be refused (the server measures after detection)', () => {
    setSource(src);
    app.ops.autocrop.enabled = true;
    applyPreset('chat');
    expect(html()).toContain('up to 2560×1440 · 704 frames · ~9.7 GiB of decoded frames before crop-to-content');
    setFeatures({ features, formats: ['gif'], maxMasterBytes: 2 * GIB });
    const note = estimateText(html());
    expect(note).toContain("Up to ~9.7 GiB of decoded frames before crop-to-content — over this server's 2 GiB limit unless crop-to-content shrinks the frame enough, so Render may be refused. At 2560×1440 about 145 frames fit");
    expect(note).not.toContain('will be refused');
    // the same hedge on the scratch bound
    app.ops.autocrop.enabled = false;
    app.ops.resize = { enabled: true, width: 1920, height: 1080, fit: 'exact' };
    app.ops.trim = { enabled: true, start: 0, end: 200 / 30 };
    app.ops.autocrop.enabled = true;
    applyPreset('custom');
    app.output.fitEnabled = true;
    app.output.fitKiB = 8000;
    setFeatures({ features, formats: ['gif'], maxMasterBytes: 2 * GIB, scratchBudgetBytes: 2.5 * GIB });
    const scratch = estimateText(html());
    expect(scratch).toContain('can hold for this output unless crop-to-content shrinks the frame enough, so Render may be refused.');
    expect(scratch).not.toContain('will be refused');
  });

  it('a reversed png export shows the reverse buffer next to its one frame of master and is refused on it, like render.go', () => {
    setSource(src);
    app.ops.reverse = true;
    applyPreset('chat');
    applyPreset('custom'); // keeps chat's source-size canvas and rate; the format is the user's
    app.output.format = 'png';
    // no caps yet: the muted line carries both figures
    let out = html();
    expect(out).toContain('2560×1440 · 1 frame · ~14 MiB of decoded frames · ~9.7 GiB buffered by the reverse');
    expect(out).not.toContain('note error');
    // over the cap: the buffer names the refusal, the way out is the clip length whose reverse fits
    setFeatures({ features, formats: ['gif', 'png'], maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 });
    const note = estimateText(html());
    expect(html()).toMatch(/<p class="note error estimate[^"]*"/);
    expect(note).toContain("~9.7 GiB buffered by the reverse (704 frames) — over this server's 2 GiB limit, so Render will be refused. At 2560×1440 about 145 frames fit (4.8 s at 30 fps): trim to that, lower the fps, crop or resize the output (or raise EZLG_MAX_MASTER_BYTES on the server).");
    // trimmed to 3 s the buffer fits: the muted line is back with the smaller figure
    app.ops.trim = { enabled: true, start: 0, end: 3 };
    out = html();
    expect(out).not.toContain('note error');
    expect(out).toContain('2560×1440 · 1 frame · ~14 MiB of decoded frames · ~1.2 GiB buffered by the reverse');
    // a bounce alone buffers nothing before its first frame (the server does not check it either): no figure, no note
    app.ops.trim.enabled = false;
    app.ops.reverse = false;
    app.ops.bounce = true;
    out = html();
    expect(out).not.toContain('buffered by the reverse');
    expect(out).not.toContain('note error');
    expect(out).toContain('2560×1440 · 1 frame · ~14 MiB of decoded frames');
  });

  it('the seconds in the fix hint are floored to the tenth, never rounded past what fits', () => {
    setSource(src);
    // 640×360 at 30 fps, slowed 10×: 7040 frames; 2330 fit under 2 GiB — 77.667 s, shown as 77.6 (a trim to "77.7" would plan 2331)
    app.ops.resize = { enabled: true, width: 640, height: 360, fit: 'exact' };
    app.ops.speed = { enabled: true, factor: 0.1 };
    applyPreset('chat');
    setFeatures({ features, formats: ['gif'], maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 });
    const note = estimateText(html());
    expect(note).toContain('At 640×360 about 2330 frames fit (77.6 s at 30 fps)');
    expect(note).not.toContain('77.7');
  });

  it('shows no estimate without a source or for the Optimize preset, and the frames cap note above 2000 frames', () => {
    expect(html()).not.toContain('decoded frames');
    setSource({ ...src, name: 'x.gif', info: { ...clip4k, format: 'gif', codec: 'gif', kind: 'animation' } });
    applyPreset('optimize');
    expect(app.output.preset).toBe('optimize');
    expect(html()).not.toContain('decoded frames');
    // frames export of a 2816-frame plan: the MaxExtractFrames note, independent of the master cap
    setSource(src);
    applyPreset('frames');
    app.ops.fps = { enabled: true, fps: 60 };
    app.ops.speed = { enabled: true, factor: 0.5 }; // 23.47 s × 2 × 60 fps ≈ 2816 frames
    const out = html();
    expect(out).toContain('Frames export is capped at 2000 frames — trim the clip or lower the fps.');
    expect(out).toContain('2816 frames');
    app.ops.speed.enabled = false;
    app.ops.fps.enabled = false;
    expect(html()).not.toContain('Frames export is capped');
  });
});
