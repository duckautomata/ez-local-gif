// Server-side renders of the Trim card: the duration it clamps/labels with
// must be the *effective* source duration — for an image sequence with the
// Delay op on, frames / rate, not the probe's duration (W6) — and the
// selection is labelled as source frames too.
import { render } from 'svelte/server';
import { beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../../lib/api';
import { app, setSource } from '../../lib/state.svelte';
import TrimCard from './TrimCard.svelte';

const seqInfo: ProbeInfo = {
  format: 'image2',
  codec: 'png',
  pixFmt: 'rgba',
  bits: 8,
  width: 64,
  height: 64,
  fps: 10,
  duration: 1.2,
  frames: 12,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'sequence',
  premultiplied: false,
  sequence: { count: 12, pattern: '%06d.png', delayMs: 100, mixed: false },
};
const seqSrc: Source = { hash: 'e'.repeat(64), name: 'seq', size: 100, info: seqInfo };
const gifInfo: ProbeInfo = { ...seqInfo, format: 'gif', codec: 'gif', fps: 25, duration: 2, frames: 50, kind: 'animation', sequence: undefined };

function html(info: ProbeInfo, initialOpen = false): string {
  return render(TrimCard, { props: { info, initialOpen } }).body;
}

describe('TrimCard (SSR)', () => {
  beforeEach(() => {
    setSource(seqSrc);
  });

  it('labels the whole clip with the probe duration and frame count while no Delay op runs', () => {
    expect(html(seqInfo)).toContain('whole clip (1.20 s, 12 frames)');
  });

  it('follows the Delay op for an image sequence (12 × 500 ms = 6 s, not the probed 1.2 s)', () => {
    app.ops.delay = { enabled: true, ms: 500 };
    const out = html(seqInfo);
    expect(out).toContain('whole clip (6.00 s, 12 frames)');
    expect(out).not.toContain('1.20 s');
  });

  it('clamps the selected range against the effective duration and shows it as frames', () => {
    app.ops.delay = { enabled: true, ms: 500 };
    app.ops.trim = { enabled: true, start: 0, end: 4 }; // 4 s is beyond the probed 1.2 s but inside the effective 6 s
    expect(html(seqInfo)).toContain('0.00 s → 4.00 s (4.00 s, frames 1–8 of 12)'); // collapsed: the summary
    expect(html(seqInfo, true)).toContain('source frames 1–8 of 12'); // expanded: the hint
  });

  it('caps the Start field one source frame before the end (review bug 3)', () => {
    setSource({ ...seqSrc, info: gifInfo, hash: 'a'.repeat(64) });
    app.ops.trim = { enabled: true, start: 1.5, end: 0 };
    const out = html(gifInfo, true);
    // 2 s at 25 fps: 1.96 s is the last frame's start; ≥ 2 s would be "at or beyond the end" for the graph
    expect(out).toMatch(/<input[^>]*max="1.96"/);
    expect(out).toMatch(/<input[^>]*max="2"/); // End may reach the clip end
  });

  it('labels a sequence trim with the grid frames the graph selects (nearest frame, as ffmpeg reads it)', () => {
    app.ops.delay = { enabled: true, ms: 40 }; // 25 fps: 12 frames over 0.48 s
    app.ops.trim = { enabled: true, start: 0.06, end: 0.11 }; // both bounds mid-frame: frame 3 alone
    expect(html(seqInfo)).toContain('(0.050 s, frame 3 of 12)');
    app.ops.trim = { enabled: true, start: 0.03, end: 0.11 }; // start rounds down to frame 2
    expect(html(seqInfo)).toContain('(0.080 s, frames 2–3 of 12)');
    app.ops.trim = { enabled: true, start: 0.44, end: 0 }; // 0.44 × 25 = 11 → the last frame, to the end
    expect(html(seqInfo)).toContain('(0.040 s, frame 12 of 12)');
  });

  it('keeps "from scrubber" values and frame labels at microsecond precision on a 30 fps clip', () => {
    const clip30: ProbeInfo = { ...gifInfo, fps: 30, duration: 2, frames: 60 };
    setSource({ ...seqSrc, info: clip30, hash: 'c'.repeat(64) });
    app.ui.scrubFrame = 2; // frame 3 starts at 2/30
    let out = html(clip30, true);
    expect(out).toContain('frame 3, 0.067 s of the source'); // fmtSeconds of 0.066667
    expect(out).toMatch(/<input[^>]*max="1.966667"/); // Start cap: one source frame before the end, whole µs
    // the two "from scrubber" clicks on frames 3 and 5: the µs values, never 0.067 / 0.167
    app.ops.trim = { enabled: true, start: 0.066667, end: 0.166667 };
    // collapsed: the summary; 0.166667 − 0.066667 may sit 1 ulp under 0.1 s
    expect(html(clip30)).toMatch(/0\.067 s → 0\.17 s \(0\.10+ s, frames 3–5 of 60\)/);
    out = html(clip30, true); // expanded: the hint and the fields
    expect(out).toContain('source frames 3–5 of 60');
    // the fields carry the 6-decimal values untruncated
    expect(out).toMatch(/<input[^>]*value="0.066667"/);
    expect(out).toMatch(/<input[^>]*value="0.166667"/);
    expect(app.ops.trim).toEqual({ enabled: true, start: 0.066667, end: 0.166667 });
    // the scrubber now runs over the trimmed range: its frame 3 is source frame 5 (0.1333… s)
    expect(out).toContain('frame 3, 0.13 s of the source');
  });

  it('describes what "from scrubber" will set for the frame under the scrubber', () => {
    setSource({ ...seqSrc, info: gifInfo, hash: 'b'.repeat(64) });
    app.ui.scrubFrame = 12;
    let out = html(gifInfo, true);
    expect(out).toContain('frame 13, 0.48 s of the source');
    expect(out).toContain('frame 13, 0.52 s of the source');
    app.ui.scrubFrame = 49; // last frame: the end button means "to the end"
    out = html(gifInfo, true);
    expect(out).toContain('frame 50, 1.96 s of the source');
    expect(out).toContain('End after the last frame (to the end)');
  });
});
