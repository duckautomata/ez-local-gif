import { describe, expect, it } from 'vitest';
import {
  dropRates,
  fmtLimit,
  fmtTimecode,
  FRAME_TOLERANCE,
  frameAt,
  frameCount,
  frameSpan,
  frameStart,
  GIF_MAX_FPS,
  gifDelays,
  MAX_FPS,
  round,
  snapFPS,
  stillTime,
  trimTime,
} from './format';

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

  it('does not lose a frame to float error when the duration is a whole number of frames (review bug 4)', () => {
    // 34 frames at 33 ms: the compiler's rate is round(1000/33, 3) = 30.303
    // and the duration count/rate, so duration × rate = 33.999999… — the old
    // 1e-9 tolerance floored that to 33.
    expect(frameCount(34 / 30.303, 30.303)).toBe(34);
    for (const [n, ms] of [
      [34, 33],
      [7, 67],
      [100, 41],
      [13, 17],
      [250, 24],
      [3, 333],
    ]) {
      const fps = round(1000 / ms, 3);
      expect(frameCount(n / fps, fps), `${n} frames at ${ms} ms`).toBe(n);
    }
    // but a genuinely short clip still floors
    expect(frameCount(34 / 30.303 - 0.01, 30.303)).toBe(33);
  });

  it('FRAME_TOLERANCE mirrors graph.FrameTolerance: µs-rounded grid bounds count every selected frame', () => {
    // 1 µs at the 60 fps cap is 6e-5 frames, and a genuine sub-frame
    // shortfall out of µs-precise times is never that small (graph's own
    // bounds on the constant).
    expect(FRAME_TOLERANCE).toBe(1e-4);
    expect(FRAME_TOLERANCE).toBeGreaterThanOrEqual(6e-5);
    expect(FRAME_TOLERANCE).toBeLessThanOrEqual(1e-3);
    // [2/30, 6/30) travels as -ss 0.066667 -to 0.2: 0.133333 × 30 = 3.99999
    // is the 4 frames ffmpeg renders (graph's "trim bounds keep microsecond
    // precision on a 30 fps frame grid" case)…
    expect(frameCount(trimTime(6 / 30) - trimTime(2 / 30), 30)).toBe(4);
    expect(frameCount(0.133333, 30)).toBe(4);
    // …which the old 1e-6 tolerance lost
    expect(Math.floor(0.133333 * 30 + 1e-6)).toBe(3);
    // every on-grid µs window at the usual rates counts exactly its frames
    for (const fps of [10, 12.5, 23.976, 24, 25, 29.97, 30, 30.303, 50, 58.824, 59.94, 60]) {
      for (const [i, j] of [
        [0, 1],
        [2, 6],
        [3, 7],
        [33, 34],
        [7, 100],
      ]) {
        expect(frameCount(trimTime(j / fps) - trimTime(i / fps), fps), `${fps} fps frames ${i}..${j}`).toBe(j - i);
      }
    }
    // a clip that really is half a frame short still floors
    expect(frameCount(0.133333 - 0.5 / 30, 30)).toBe(3);
  });

  it('maps times to 1-based frames with the boundary belonging to the next frame', () => {
    expect(frameAt(0, 25)).toBe(1);
    expect(frameAt(0.039, 25)).toBe(1);
    expect(frameAt(0.04, 25)).toBe(2);
    expect(frameAt(1.12, 25, 75)).toBe(29);
    expect(frameAt(1 / 3, 3)).toBe(2); // k/fps for k=1 → frame 2 despite fp error
    expect(frameAt(trimTime(1 / 30), 30)).toBe(2); // the µs-rounded 0.033333 × 30 = 0.99999 is still frame 2
    expect(frameAt(0.0333, 30)).toBe(1); // but 0.1 ms early is frame 1
    expect(frameAt(99, 25, 75)).toBe(75); // clamped
    expect(frameAt(-1, 25)).toBe(1);
    expect(frameAt(0.5, 0)).toBe(1);
  });

  it('trimTime keeps whole microseconds (ffmpeg -ss/-to resolution, graph.round6), never milliseconds', () => {
    expect(trimTime(2 / 30)).toBe(0.066667);
    expect(trimTime(6 / 30)).toBe(0.2);
    expect(trimTime(1.5)).toBe(1.5);
    expect(trimTime(3 / 30.303)).toBe(0.099);
    expect(trimTime(33 / 30.303)).toBe(1.089001);
    expect(trimTime(1.0000004)).toBe(1);
    expect(trimTime(1.0000006)).toBe(1.000001);
    expect(trimTime(0)).toBe(0);
    expect(Object.is(trimTime(-0.0000001), 0)).toBe(true); // no -0
  });

  it('stillTime asks for the middle of frame i, which the server maps back onto frame i', () => {
    for (const fps of [10, 12.5, 23.976, 25, 29.97, 30, 30.303, 50, 59.94, 60]) {
      const n = frameCount(2, fps);
      for (let i = 0; i < n; i++) {
        const t = stillTime(i, fps);
        expect(Math.round(t * 1000) / 1000, `${fps} fps frame ${i} is whole ms`).toBe(t);
        // what jobs.Still does with it: round to ms, slot = floor(t × fps)
        const ms = Math.floor(t * 1000 + 0.5) / 1000;
        expect(Math.floor(ms * fps), `${fps} fps frame ${i} server slot`).toBe(i);
        expect(frameAt(t, fps, n), `${fps} fps frame ${i} readout`).toBe(i + 1);
        expect(t, `${fps} fps frame ${i} inside the clip`).toBeLessThan(2);
        expect(t).toBeGreaterThan(frameStart(i, fps));
      }
    }
    expect(stillTime(0, 25)).toBe(0.02);
    expect(stillTime(28, 25)).toBe(1.14);
    expect(stillTime(-4, 25)).toBe(0.02);
    expect(stillTime(3, 0)).toBe(0);
  });

  it('frameStart is the plain i/fps (what the readout shows)', () => {
    expect(frameStart(0, 25)).toBe(0);
    expect(frameStart(28, 25)).toBe(1.12);
    expect(frameStart(-1, 25)).toBe(0);
    expect(frameStart(3, 0)).toBe(0);
  });

  it('frameSpan turns a [start, end) window into inclusive 1-based frames', () => {
    expect(frameSpan(0, 1, 25)).toEqual({ first: 1, last: 25 });
    expect(frameSpan(0.04, 0.08, 25)).toEqual({ first: 2, last: 2 }); // exactly frame 2
    expect(frameSpan(1 / 30, 2 / 30, 30)).toEqual({ first: 2, last: 2 }); // despite fp error
    expect(frameSpan(0.5, 1.5, 25)).toEqual({ first: 13, last: 38 });
    expect(frameSpan(0.5, 0.5, 25)).toEqual({ first: 13, last: 13 }); // empty window = one frame
    expect(frameSpan(0, 99, 25, 50)).toEqual({ first: 1, last: 50 }); // clamped
    expect(frameSpan(99, 100, 25, 50)).toEqual({ first: 50, last: 50 });
    expect(frameSpan(0, 1, 0)).toEqual({ first: 1, last: 1 });
  });

  it('frameSpan agrees with the graph on µs-rounded grid bounds (what "from scrubber" sends)', () => {
    // frames 3..5 of a 30 fps clip: -ss 0.066667 -to 0.166667, 3 frames
    expect(frameSpan(trimTime(2 / 30), trimTime(5 / 30), 30, 60)).toEqual({ first: 3, last: 5 });
    // 0.033333 × 30 = 0.99999 and 0.133333 × 30 = 3.99999: frames 2–4, the graph's count of 3
    expect(frameSpan(trimTime(1 / 30), trimTime(4 / 30), 30)).toEqual({ first: 2, last: 4 });
    expect(frameCount(trimTime(4 / 30) - trimTime(1 / 30), 30)).toBe(3);
    for (const fps of [23.976, 29.97, 30.303, 58.824, 59.94]) {
      expect(frameSpan(trimTime(7 / fps), trimTime(12 / fps), fps), `${fps} fps`).toEqual({ first: 8, last: 12 });
      expect(frameCount(trimTime(12 / fps) - trimTime(7 / fps), fps), `${fps} fps`).toBe(5);
    }
  });
});

describe('fmtLimit', () => {
  it('renders attachment caps in MB and emote / sticker caps in KiB', () => {
    expect(fmtLimit(262_144)).toBe('256 KiB');
    expect(fmtLimit(524_288)).toBe('512 KiB');
    expect(fmtLimit(20_000_000)).toBe('20 MB');
    expect(fmtLimit(50_000_000)).toBe('50 MB');
    expect(fmtLimit(100_000_000)).toBe('100 MB');
    expect(fmtLimit(500_000_000)).toBe('500 MB');
    expect(fmtLimit(300_000)).toBe('293.0 KiB');
    expect(fmtLimit(0)).toBe('none');
    expect(fmtLimit(-1)).toBe('none');
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
  // gif muxer alternates 3/4 cs delays for 30 fps with exact timing.
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

  it('rounds to 3 decimals like graph.SnapFPS (the rate the fps filter and the frame count use)', () => {
    expect(snapFPS('webp', 30000 / 1001)).toBe(29.97);
    expect(snapFPS('gif', 30000 / 1001)).toBe(29.97);
    expect(snapFPS('gif', 1000 / 33)).toBe(30.303);
    expect(snapFPS('webp', 24000 / 1001)).toBe(23.976);
    expect(snapFPS('apng', 12.5)).toBe(12.5);
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
