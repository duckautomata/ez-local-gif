import { describe, expect, it } from 'vitest';
import { GIF_MAX_FPS, gifDelays, MAX_FPS, snapFPS } from './format';

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
