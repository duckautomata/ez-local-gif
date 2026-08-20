import { describe, expect, it } from 'vitest';
import { ceilMs, dropRates, fmtTimecode, frameAt, frameCount, frameTime, GIF_MAX_FPS, gifDelays, MAX_FPS, snapFPS } from './format';

describe('fmtTimecode', () => {
  it('renders MM:SS.cc like a Resolve timeline', () => {
    expect(fmtTimecode(0)).toBe('00:00.00');
    expect(fmtTimecode(1.12)).toBe('00:01.12');
    expect(fmtTimecode(72.5)).toBe('01:12.50');
    expect(fmtTimecode(599.999)).toBe('10:00.00'); // rounds to centiseconds with carry
    expect(fmtTimecode(3600)).toBe('1:00:00.00');
    expect(fmtTimecode(3725.04)).toBe('1:02:05.04');
  });
  it('never shows negative or NaN', () => {
    expect(fmtTimecode(-3)).toBe('00:00.00');
    expect(fmtTimecode(NaN)).toBe('00:00.00');
    expect(fmtTimecode(Infinity)).toBe('00:00.00');
  });
});

describe('frame grid', () => {
  it('counts frames on the plan fps', () => {
    expect(frameCount(1, 25)).toBe(25);
    expect(frameCount(3, 25)).toBe(75);
    expect(frameCount(0.999, 30)).toBe(29); // floor: the render's fps stage is round=down
    expect(frameCount(0.01, 25)).toBe(1); // never 0 for a real clip
    expect(frameCount(2, 0)).toBe(0); // unknown rate
  });
  it('maps times to 1-based frames with the boundary belonging to the next frame', () => {
    expect(frameAt(0, 25)).toBe(1);
    expect(frameAt(0.039, 25)).toBe(1);
    expect(frameAt(0.04, 25)).toBe(2);
    expect(frameAt(1.12, 25, 75)).toBe(29);
    expect(frameAt(1 / 3, 3)).toBe(2); // k/fps for k=1 → frame 2 despite fp error
    expect(frameAt(99, 25, 75)).toBe(75); // clamped
    expect(frameAt(-1, 25)).toBe(1);
    expect(frameAt(0.5, 0)).toBe(1);
  });
  it('round-trips frame ↔ time, also through the server-side floor(t × fps) on whole ms', () => {
    for (const fps of [10, 12.5, 23.976, 25, 29.97, 30, 50, 59.94, 60]) {
      const n = frameCount(2, fps);
      for (let f = 1; f <= n; f++) {
        const t = frameTime(f, fps);
        expect(Math.round(t * 1000) / 1000, `${fps} fps frame ${f} is whole ms`).toBe(t);
        expect(frameAt(t, fps, n), `${fps} fps frame ${f}`).toBe(f);
        // what jobs.Still does with it: round to ms, slot = floor(t × fps)
        const slot = Math.floor(Math.round(t * 1000) / 1000 * fps + 1e-9);
        expect(slot, `${fps} fps frame ${f} server slot`).toBe(f - 1);
      }
    }
    expect(frameTime(1, 25)).toBe(0);
    expect(frameTime(29, 25)).toBe(1.12);
    expect(frameTime(3, 23.976)).toBe(0.084); // ceiled, not 0.083
    expect(frameTime(3, 0)).toBe(0);
  });

  it('ceilMs rounds up to whole milliseconds, keeping exact ones', () => {
    expect(ceilMs(0.0834)).toBe(0.084);
    expect(ceilMs(0.04)).toBe(0.04);
    expect(ceilMs(0.37)).toBe(0.37);
    expect(ceilMs(1 / 25)).toBe(0.04);
    expect(ceilMs(0)).toBe(0);
    expect(ceilMs(-1)).toBe(0);
    expect(ceilMs(NaN)).toBe(0);
  });
});

describe('dropRates', () => {
  // jobs/optimize.go dropEveryN: Output.FPS must be srcFPS × (N−1)/N for N in
  // 2..4 within 5 %; 0 keeps every frame.
  const accepted = (src: number, want: number): number => {
    if (want <= 0 || want >= src) return 0;
    for (let n = 2; n <= 4; n++) {
      const kept = (src * (n - 1)) / n;
      if (Math.abs(want - kept) <= 0.05 * kept) return n;
    }
    return -1;
  };
  it('lists keep-all and the drop-every-Nth rates, mildest first', () => {
    const r = dropRates(25);
    expect(r.map((x) => x.n)).toEqual([0, 4, 3, 2]);
    expect(r.map((x) => x.fps)).toEqual([0, 18.75, 16.667, 12.5]);
    expect(r[0].label).toBe('keep all frames (25 fps)');
    expect(r[1].label).toBe('drop every 4th frame (18.75 fps)');
    expect(r[2].label).toBe('drop every 3rd frame (16.67 fps)');
    expect(r[3].label).toBe('drop every 2nd frame (12.5 fps)');
    expect(dropRates(0)).toEqual([]);
    expect(dropRates(NaN)).toEqual([]);
  });
  it('every offered rate maps back onto its N in the backend', () => {
    for (const src of [10, 12.5, 15, 20, 23.976, 25, 29.97, 30, 50]) {
      for (const r of dropRates(src)) expect(accepted(src, r.fps), `${src} fps n=${r.n}`).toBe(r.n);
    }
  });
});

describe('snapFPS', () => {
  // Mirrors graph.SnapFPS after DESIGN §4.1: GIF is capped at 50 (2 cs
  // minimum delay) and otherwise left alone — no 100/n snapping any more, the
  // gif muxer alternates 3/4 cs delays for 30 fps with exact total timing.
  it('caps GIF at 50 and leaves lower rates alone', () => {
    expect(GIF_MAX_FPS).toBe(50);
    for (const fps of [1, 10, 12.5, 15, 23.976, 24, 25, 29.97, 30, 33.33, 40, 50]) {
      expect(snapFPS('gif', fps), `${fps}`).toBe(fps);
    }
    expect(snapFPS('gif', 50.001)).toBe(50);
    expect(snapFPS('gif', 60)).toBe(50);
    expect(snapFPS('gif', 1000)).toBe(50);
    expect(snapFPS('gif', Infinity)).toBe(50);
  });

  it('caps other formats at 60', () => {
    expect(MAX_FPS).toBe(60);
    for (const fps of [1, 24, 30, 50, 59.94, 60]) {
      expect(snapFPS('webp', fps), `${fps}`).toBe(fps);
    }
    expect(snapFPS('webp', 60.5)).toBe(60);
    expect(snapFPS('webp', 120)).toBe(60);
    expect(snapFPS('apng', 120)).toBe(60);
  });

  it('returns 0 for no rate', () => {
    for (const format of ['gif', 'webp']) {
      expect(snapFPS(format, 0)).toBe(0);
      expect(snapFPS(format, -5)).toBe(0);
      expect(snapFPS(format, NaN)).toBe(0);
    }
  });
});

describe('gifDelays', () => {
  it('describes uniform centisecond delays', () => {
    expect(gifDelays(50)).toBe('2');
    expect(gifDelays(25)).toBe('4');
    expect(gifDelays(20)).toBe('5');
    expect(gifDelays(12.5)).toBe('8');
    expect(gifDelays(10)).toBe('10');
    // rates that only *look* fractional are uniform too
    expect(gifDelays(33.33)).toBe('3');
    expect(gifDelays(16.67)).toBe('6');
    expect(gifDelays(14.29)).toBe('7');
    expect(gifDelays(11.11)).toBe('9');
  });

  it('describes alternating delays for the rest', () => {
    expect(gifDelays(30)).toBe('3/4'); // 3/3/4 cs, exact timing
    expect(gifDelays(29.97)).toBe('3/4');
    expect(gifDelays(24)).toBe('4/5');
    expect(gifDelays(23.976)).toBe('4/5');
    expect(gifDelays(15)).toBe('6/7');
    expect(gifDelays(40)).toBe('2/3');
  });

  it('caps at the GIF maximum and ignores no rate', () => {
    expect(gifDelays(60)).toBe('2');
    expect(gifDelays(1000)).toBe('2');
    expect(gifDelays(0)).toBe('');
    expect(gifDelays(-1)).toBe('');
    expect(gifDelays(NaN)).toBe('');
  });
});
