// Server-side render of the Preview card: role="slider" is a
// children-presentational role, so it must sit on a dedicated element with
// no interactive descendants (W10) — the position readout in the scrub row —
// and never on the stage (which contains the Retry button of the error
// overlay and the crop canvas).
import { render } from 'svelte/server';
import { beforeAll, describe, expect, it, vi } from 'vitest';
import type { ProbeInfo, Source } from '../lib/api';
import { setSource } from '../lib/state.svelte';
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

  it('keeps the readout slider present but disabled when frames cannot be stepped', () => {
    setSource({ ...gifSrc, info: { ...gifInfo, fps: 0, duration: 0, frames: 1, isStill: true, kind: 'image' } });
    const out = html();
    expect(out.match(/role="slider"/g)?.length).toBe(1);
    expect(out).toContain('aria-disabled="true"');
  });
});
