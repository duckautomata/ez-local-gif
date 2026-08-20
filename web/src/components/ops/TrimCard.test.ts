// Server-side renders of the Trim card: the duration it clamps/labels with
// must be the *effective* source duration — for an image sequence with the
// Delay op on, frames × ms, not the probe's duration (W6).
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

function html(info: ProbeInfo): string {
  return render(TrimCard, { props: { info } }).body;
}

describe('TrimCard (SSR)', () => {
  beforeEach(() => {
    setSource(seqSrc);
  });

  it('labels the whole clip with the probe duration while no Delay op runs', () => {
    expect(html(seqInfo)).toContain('whole clip (1.20 s)');
  });

  it('follows the Delay op for an image sequence (12 × 500 ms = 6 s, not the probed 1.2 s)', () => {
    app.ops.delay = { enabled: true, ms: 500 };
    const out = html(seqInfo);
    expect(out).toContain('whole clip (6.00 s)');
    expect(out).not.toContain('1.20 s');
  });

  it('clamps the selected range against the effective duration', () => {
    app.ops.delay = { enabled: true, ms: 500 };
    app.ops.trim = { enabled: true, start: 0, end: 4 }; // 4 s is beyond the probed 1.2 s but inside the effective 6 s
    const out = html(seqInfo);
    expect(out).toContain('0.00 s → 4.00 s (4.00 s)');
  });
});
