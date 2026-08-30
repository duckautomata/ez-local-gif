// Server-side renders of the Feather card (review R4): the radius slider and
// number field, the "soft edge ≈ 2–3×N" label and the format hint.
import { render } from 'svelte/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Source } from '../../lib/api';
import { resetFeatures, setFeatures } from '../../lib/capabilities.svelte';
import { app, setSource } from '../../lib/state.svelte';
import FeatherCard from './FeatherCard.svelte';

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

function html(initialOpen = false): string {
  return render(FeatherCard, { props: { initialOpen } }).body;
}

describe('FeatherCard (SSR)', () => {
  beforeEach(() => {
    setSource(gifSrc);
  });
  afterEach(() => resetFeatures());

  it('collapsed: off by default, the summary shows the radius once enabled', () => {
    let out = html();
    expect(out).toContain('Feather');
    expect(out).toContain('off');
    expect(out.match(/<input[^>]*aria-label="Enable Feather"[^>]*>/)?.[0]).not.toContain('checked');
    app.ops.feather = { enabled: true, radius: 4.5 };
    out = html();
    expect(out).toContain('4.5 px soft edge');
    expect(out.match(/<input[^>]*aria-label="Enable Feather"[^>]*>/)?.[0]).toContain('checked');
  });

  it('open: slider + number for the radius (0.5..20, step 0.5, default 3) and the source-pixel label', () => {
    const out = html(true);
    expect(out).toContain('Feather — 3 px (soft edge ≈ 2–3×3)');
    expect(out).toMatch(/<input type="range"[^>]*min="0.5"[^>]*max="20"[^>]*step="0.5"[^>]*aria-label="Feather radius"/);
    expect(out).toMatch(/<input[^>]*type="number"[^>]*min="0.5"[^>]*max="20"[^>]*step="0.5"/);
    app.ops.feather.radius = 7;
    expect(html(true)).toContain('Feather — 7 px (soft edge ≈ 2–3×7)');
  });

  it('the hint explains the alpha blur, the GIF threshold and the source-pixel scaling', () => {
    const out = html(true);
    expect(out).toContain('Gaussian blur of the alpha');
    expect(out).toContain('after Background removal');
    expect(out).toContain('GIF output still thresholds back to 1-bit alpha');
    expect(out).toContain('WebP, APNG and AVIF keep the soft edge');
    expect(out).toContain('scales down with the output');
  });

  // WEB-7: a pre-Phase-4 server 400s on the feather op — the card stays
  // visible with a one-line notice, gated on the Phase 4 signal that already
  // exists in /api/capabilities (a formats list carrying mp4).
  it('stays visible with a notice on a server without the Phase 4 ops (formats list lacks mp4)', () => {
    setFeatures({ features: { keying: true }, formats: ['gif', 'webp', 'apng', 'avif', 'png', 'jpeg', 'frames'] });
    expect(html(true)).toContain('does not support feathering');
    expect(html()).toContain('off — not supported by this server'); // the collapsed summary says so too
    setFeatures({ features: { keying: true }, formats: ['gif', 'mp4', 'webm'] });
    expect(html(true)).not.toContain('does not support feathering');
    expect(html()).not.toContain('not supported by this server');
  });
});
