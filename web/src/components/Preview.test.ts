// Server-side render of the Preview card: the scrubber is a frame-index
// range input (one notch per plan frame, both ends reachable), and
// role="slider" — a children-presentational role — sits on a dedicated
// element with no interactive descendants (W10): the position readout in the
// scrub row, never the stage (which contains the Retry button of the error
// overlay and the crop canvas).
import { render } from 'svelte/server';
import { beforeAll, describe, expect, it, vi } from 'vitest';
import type { ProbeInfo, Source } from '../lib/api';
import { app, setSource } from '../lib/state.svelte';
import Preview from './Preview.svelte';

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
/** the review's sequence: 34 frames, uploaded at 100 ms */
const seq34: ProbeInfo = {
  ...gifInfo,
  format: 'image2',
  codec: 'png',
  pixFmt: 'rgba',
  fps: 10,
  duration: 3.4,
  frames: 34,
  kind: 'sequence',
  sequence: { count: 34, pattern: '%06d.png', delayMs: 100, mixed: false },
};

beforeAll(() => {
  // Preview reads window.matchMedia at instance init (the wide-screen query);
  // the tests run in plain Node, so provide the minimal stub.
  vi.stubGlobal('window', {
    matchMedia: () => ({ matches: false, addEventListener: () => {}, removeEventListener: () => {} }),
  });
  return () => vi.unstubAllGlobals();
});

function html(): string {
  return render(Preview, { props: {} }).body;
}

/** the rendered <input type="range" …> tag */
function rangeTag(out: string): string {
  return out.match(/<input[^>]*type="range"[^>]*>/)?.[0] ?? '';
}

describe('Preview (SSR)', () => {
  it('puts role="slider" on the position readout only, never on the stage (W10)', () => {
    setSource(gifSrc);
    const out = html();
    expect(out.match(/role="slider"/g)?.length).toBe(1);
    // the slider is the .time readout span…
    expect(out).toMatch(/<span[^>]*class="time[^"]*"[^>]*role="slider"/);
    // …and the stage is a plain container without slider semantics or tabindex
    expect(out).not.toMatch(/<div[^>]*role="slider"/);
    expect(out).not.toMatch(/<div[^>]*class="stage[^"]*"[^>]*tabindex/);
    // frame-stepper semantics live on the readout (25 fps × 2 s = 50 frames)
    expect(out).toContain('aria-valuemax="50"');
    expect(out).toContain('aria-valuenow="1"');
  });

  it('scrubs by frame index: min 0, max N − 1, step 1 — both ends reachable, no phantom stop (review bugs 1, 2)', () => {
    setSource(gifSrc);
    let out = html();
    let tag = rangeTag(out);
    expect(tag).toContain('min="0"');
    expect(tag).toContain('max="49"');
    expect(tag).toContain('step="1"');
    expect(tag).not.toContain('disabled');
    expect(out).toContain('00:00.00 · f 1 / 50');
    // the last frame reads as frame 50 at 1.96 s (its start), the clip is 2.00 s
    app.ui.scrubFrame = 49;
    out = html();
    expect(out).toContain('00:01.96 · f 50 / 50');
    expect(out).toContain('aria-valuenow="50"');
    // a stale position past the end is clamped, never shown as frame 51
    app.ui.scrubFrame = 80;
    out = html();
    expect(out).toContain('f 50 / 50');
    expect(out).not.toContain('f 51');
    // an fps change re-grids the slider: 10 fps → 20 frames
    app.ui.scrubFrame = 0;
    app.ops.fps = { enabled: true, fps: 10 };
    tag = rangeTag(html());
    expect(tag).toContain('max="19"');
  });

  it('a sequence has exactly its frame count whatever the delay, so one notch is one frame (review bug 4)', () => {
    setSource({ ...gifSrc, info: seq34 });
    expect(rangeTag(html())).toContain('max="33"');
    expect(html()).toContain('aria-valuemax="34"');
    app.ops.delay = { enabled: true, ms: 33 };
    const out = html();
    expect(rangeTag(out)).toContain('max="33"');
    expect(out).toContain('aria-valuemax="34"');
    expect(out).toContain('f 1 / 34');
    app.ui.scrubFrame = 33;
    expect(html()).toContain('f 34 / 34');
  });

  it('counts a µs-precise scrubber trim exactly: frames 3..5 of a 30 fps clip are 3 notches (graph.FrameTolerance)', () => {
    const clip30 = { ...gifInfo, fps: 30, duration: 2, frames: 60 };
    setSource({ ...gifSrc, info: clip30 });
    expect(rangeTag(html())).toContain('max="59"');
    // what Trim "from scrubber" sets on frames 3 and 5: -ss 0.066667 -to 0.166667
    // (0.1 s × 30 = 2.9999999…; a 1e-6 tolerance on the µs-rounded 0.133333 × 30 = 3.99999 lost a frame)
    app.ops.trim = { enabled: true, start: 0.066667, end: 0.166667 };
    let out = html();
    expect(rangeTag(out)).toContain('max="2"');
    expect(out).toContain('aria-valuemax="3"');
    expect(out).toContain('00:00.00 · f 1 / 3');
    app.ops.trim = { enabled: true, start: 0.033333, end: 0.133333 };
    out = html();
    expect(rangeTag(out)).toContain('max="2"');
    expect(out).toContain('f 1 / 3');
  });

  it('a retimed sequence has the frames ffmpeg ends the stream with: 7 frames at speed 2 resampled to 20 fps are 6', () => {
    const seq60 = { ...seq34, duration: 6, frames: 60, sequence: { count: 60, pattern: '%06d.png', delayMs: 100, mixed: false } };
    setSource({ ...gifSrc, info: seq60 });
    expect(rangeTag(html())).toContain('max="59"');
    app.ops.trim = { enabled: true, start: 0, end: 0.7 };
    app.ops.speed = { enabled: true, factor: 2 };
    expect(rangeTag(html())).toContain('max="2"'); // trunc(7 / 2) = 3 frames
    app.ops.fps = { enabled: true, fps: 20 };
    const out = html();
    expect(rangeTag(out)).toContain('max="5"'); // not the 7 of floor(0.35 s × 20)
    expect(out).toContain('aria-valuemax="6"');
    expect(out).toContain('f 1 / 6');
    // a mid-frame trim snaps to the nearest grid frame: 0.06..0.11 s at 25 fps is one frame
    app.ops = { ...app.ops, trim: { enabled: true, start: 0.06, end: 0.11 }, speed: { enabled: false, factor: 1 }, fps: { enabled: false, fps: 25 } };
    app.ops.delay = { enabled: true, ms: 40 };
    expect(rangeTag(html())).toContain('disabled'); // one frame: nothing to step
    expect(html()).toContain('aria-valuemax="1"');
  });

  it('keeps the readout slider present but disabled when frames cannot be stepped', () => {
    setSource({ ...gifSrc, info: { ...gifInfo, fps: 0, duration: 0, frames: 1, isStill: true, kind: 'image' } });
    const out = html();
    expect(out.match(/role="slider"/g)?.length).toBe(1);
    expect(out).toContain('aria-disabled="true"');
    expect(rangeTag(out)).toContain('disabled');
  });
});
