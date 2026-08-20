// Server-side renders of the Result card against synthetic Phase 2 manifests
// (fit alternatives, in-chat thumbnails, frames grid, new lint rules) — the
// template is checked as HTML text, no DOM needed.
import { render } from 'svelte/server';
import { describe, expect, it } from 'vitest';
import type { Check, Result, ResultFile } from '../lib/api';
import ResultCard from './ResultCard.svelte';

function file(name: string, extra: Partial<ResultFile> = {}): ResultFile {
  return {
    name,
    url: `/out/${'a'.repeat(64)}/${name}`,
    format: name.split('.').pop() ?? '',
    bytes: 200_000,
    width: 128,
    height: 128,
    frames: 40,
    fps: 20,
    duration: 2,
    limit: 262_144,
    ...extra,
  };
}

function check(rule: string, level: Check['level'], ok: boolean, detail = ''): Check {
  return { rule, level, ok, fixed: false, detail };
}

function result(files: ResultFile[], extra: Partial<Result> = {}): Result {
  return {
    recipeHash: 'b'.repeat(64),
    recipe: { v: 1, sources: ['c'.repeat(64)], ops: [], output: { format: 'gif', target: 'emote', preset: 'emote', fitBytes: 262_144 } },
    files,
    created: '2026-08-19T00:00:00Z',
    renderMs: 1234,
    cached: false,
    ...extra,
  };
}

function html(res: Result): string {
  return render(ResultCard, { props: { result: res, running: false } }).body;
}

describe('ResultCard (SSR)', () => {
  it('shows the primary with its fit summary, in-chat thumbnails and two alternatives', () => {
    const out = html(
      result([
        file('out.gif', { kind: 'output', desc: 'fit at 20 fps · 128 colours · lossy 60', report: { rulesVersion: 'x', format: 'gif', target: 'emote', bytes: 200_000, limit: 262_144, width: 128, height: 128, frames: 40, durationMs: 2000, minDelayMs: 50, loopForever: true, hasAlpha: true, ok: true, checks: [check('gif.emote-dims', 'warn', true)] } }),
        file('alt1.gif', { kind: 'alternative', index: 1, bytes: 150_000, desc: 'fit at 16.7 fps · 128 colours · lossy 80' }),
        file('alt2.gif', { kind: 'alternative', index: 2, bytes: 100_000, desc: 'fit at 12.5 fps · 64 colours · lossy 100' }),
      ]),
    );
    expect(out).toContain('fit at 20 fps · 128 colours · lossy 60');
    expect(out).toContain('<b>Fit:</b>'); // the recipe carried fitBytes, so the desc is a fit report
    expect(out).not.toContain('Settings:');
    expect(out).toContain('Alternatives');
    expect(out).toContain('fit at 16.7 fps · 128 colours · lossy 80');
    expect(out).toContain('fit at 12.5 fps · 64 colours · lossy 100');
    // every file gets download + edit-as-source
    expect(out.match(/Edit as source/g)?.length).toBe(3);
    expect(out).toContain('?dl=1');
    // in-chat thumbnails for an emote: 22 px and 48 px, dark and light
    expect(out).toContain('inline 22 px');
    expect(out).toContain('jumbo 48 px');
    expect(out).toContain('theme-dark');
    expect(out).toContain('theme-light');
    // friendly rule label
    expect(out).toContain('Emote size 128×128');
    expect(out).toContain('gif.emote-dims');
  });

  it('renders a sticker result with the 160 px thumbnail and a warning-level sticker-dims check as a warning', () => {
    const res = result(
      [
        file('out.png', {
          kind: 'output',
          format: 'apng',
          bytes: 400_000,
          limit: 524_288,
          width: 400,
          height: 400,
          report: {
            rulesVersion: 'x',
            format: 'apng',
            target: 'sticker',
            bytes: 400_000,
            limit: 524_288,
            width: 400,
            height: 400,
            frames: 40,
            durationMs: 2000,
            minDelayMs: 50,
            loopForever: true,
            hasAlpha: true,
            ok: true,
            checks: [check('apng.sticker', 'warn', false, '400x400: Discord shrinks stickers to 320x320'), check('apng.indexed', 'info', false, 'RGBA truecolour output')],
          },
        }),
      ],
      { recipe: { v: 1, sources: ['c'.repeat(64)], ops: [], output: { format: 'apng', target: 'sticker', preset: 'sticker' } } },
    );
    const out = html(res);
    expect(out).toContain('sticker 160 px');
    expect(out).not.toContain('inline 22 px');
    expect(out).toContain('Discord-safe'); // report.ok: the oversize is only a warning
    expect(out).toContain('Sticker limits (320×320');
    expect(out).toContain('warned'); // amber row, not red
    expect(out).not.toContain('errored');
    // apng.indexed failing info-level (RGBA output): neutral dot, and the
    // label must read correctly for that outcome too, not just for ✓.
    expect(out).toContain('Indexed 8-bit-alpha APNG (sticker default rung)');
    expect(out).toContain('●');
    expect(out).toContain('RGBA truecolour output');
  });

  it('renders a frames result as a lazy thumbnail grid with per-frame downloads and the zip', () => {
    const files: ResultFile[] = [file('frames.zip', { kind: 'archive', format: 'zip', bytes: 50_000, limit: 0, frames: 0, fps: 0, duration: 0 })];
    for (let i = 1; i <= 5; i++) files.push(file(`f${String(i).padStart(5, '0')}.png`, { kind: 'frame', index: i, limit: 0, frames: 1, desc: `frame ${i} (${((i - 1) / 25).toFixed(2)} s)`, bytes: 3000 }));
    const res = result(files, { recipe: { v: 1, sources: ['c'.repeat(64)], ops: [], output: { format: 'frames', frameFormat: 'png', preset: 'frames' } } });
    const out = html(res);
    expect(out).toContain('5 frames');
    expect(out).toContain('Download all (zip');
    expect(out.match(/loading="lazy"/g)?.length).toBe(5);
    expect(out).toContain('frame 3 (0.08 s)');
    expect(out.match(/Download f\d{5}\.png/g)?.length).toBe(5); // per-frame download titles
    expect(out).not.toContain('As seen in chat');
    expect(out).not.toContain('Discord lint report'); // nothing to lint
  });

  it('keeps working for a Phase 1 manifest (no kinds) and reports an over-limit primary', () => {
    const out = html(result([file('out.webp', { bytes: 300_000, report: null })]));
    expect(out).toContain('over the 256.0 KiB limit');
    expect(out).toContain('No Discord lint report for this file.');
    expect(out).toContain('Download out.webp');
  });

  it('an over-limit fit-capable primary without a desc still reports the fit ran', () => {
    const out = html(result([file('out.gif', { kind: 'output', bytes: 300_000, report: null })]));
    expect(out).toContain('Fit search ran; the primary is the mildest rung that fits.');
    expect(out).toContain('Even the harshest fit rung was too big');
  });

  it('shows an Optimize desc under the neutral Settings label, never as a Fit line (W8)', () => {
    const res = result([file('out.gif', { kind: 'output', desc: 'gifsicle: lossy 30 · 256 colours', limit: 0, report: null })], {
      recipe: { v: 1, sources: ['c'.repeat(64)], ops: [], output: { format: 'gif', preset: 'optimize' } },
    });
    const out = html(res);
    expect(out).toContain('Settings:');
    expect(out).toContain('gifsicle: lossy 30 · 256 colours');
    expect(out).not.toContain('<b>Fit:</b>');
    expect(out).not.toContain('Fit search ran');
  });

  it('a static PNG whose recipe carries fitBytes never claims a fit ran (the server ignores it)', () => {
    const res = result([file('out.png', { kind: 'output', format: 'png', frames: 1, fps: 0, duration: 0, bytes: 300_000, report: null })], {
      recipe: { v: 1, sources: ['c'.repeat(64)], ops: [], output: { format: 'png', target: 'emote', preset: 'emote', fitBytes: 262_144 } },
    });
    const out = html(res);
    expect(out).not.toContain('Fit search ran');
    expect(out).not.toContain('Even the harshest fit rung was too big');
    // the over-limit advice falls back to the generic wording
    expect(out).toContain('over the 256.0 KiB limit');
    expect(out).toContain('Turn on');
  });
});
