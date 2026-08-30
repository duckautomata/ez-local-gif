// Server-side renders of the batch UI (Phase 4): rows with probe facts,
// per-row unpremultiply, per-row progress / result chips with Download +
// Save to /output, and the batch render panel.
import { render } from 'svelte/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ProbeInfo, Recipe, Result, Source } from '../lib/api';
import { addPendingRow, batch, enterBatch, exitBatch, rowUploaded, rowUploadFailed } from '../lib/batch.svelte';
import { resetFeatures, setFeatures } from '../lib/capabilities.svelte';
import BatchPanel from './BatchPanel.svelte';
import BatchRenderPanel from './BatchRenderPanel.svelte';

const info = (over: Partial<ProbeInfo> = {}): ProbeInfo => ({
  format: 'mov',
  codec: 'prores',
  pixFmt: 'yuva444p10le',
  bits: 10,
  width: 640,
  height: 480,
  fps: 30,
  duration: 3,
  frames: 90,
  hasAlpha: true,
  hasAudio: false,
  isStill: false,
  kind: 'video',
  premultiplied: true,
  ...over,
});
const src = (h: string, over: Partial<ProbeInfo> = {}): Source => ({ hash: h.repeat(64).slice(0, 64), name: `${h}.mov`, size: 5, info: info(over) });

const recipe: Recipe = { v: 1, sources: ['a'.repeat(64)], ops: [], output: { format: 'gif', target: 'emote' } };
const result: Result = {
  recipeHash: 'f'.repeat(64),
  recipe,
  files: [
    {
      name: 'out.gif',
      url: '/out/x/out.gif',
      format: 'gif',
      bytes: 200_000,
      width: 128,
      height: 128,
      frames: 40,
      fps: 20,
      duration: 2,
      limit: 262_144,
      kind: 'output',
      desc: 'fit at 20 fps · 128 colours',
      report: { rulesVersion: 'x', format: 'gif', target: 'emote', bytes: 200_000, limit: 262_144, width: 128, height: 128, frames: 40, durationMs: 2000, minDelayMs: 50, loopForever: true, hasAlpha: true, ok: true, checks: [] },
    },
  ],
  created: '',
  renderMs: 10,
  cached: false,
};

describe('BatchPanel (SSR)', () => {
  beforeEach(() => enterBatch());
  afterEach(() => {
    exitBatch();
    resetFeatures();
  });

  it('renders rows: probe facts, per-row unpremultiply (auto-detected), upload states', () => {
    const a = addPendingRow('a.mov');
    rowUploaded(a, src('a'));
    addPendingRow('later.mov'); // still uploading
    const out = render(BatchPanel, { props: {} }).body;
    expect(out).toContain('Batch — 2 files');
    expect(out).toContain('a.mov');
    expect(out).toContain('640×480');
    expect(out).toContain('30 fps');
    expect(out).toContain('alpha yes');
    expect(out).toContain('(detected)'); // premultiplied probe → checkbox pre-ticked and labelled
    expect(out).toContain('Uploading…');
    expect(out).toContain('Open in editor');
  });

  it('a rendered row shows the size-vs-limit chip, pass/fail, Download and Save to /output', () => {
    const a = addPendingRow('a.mov');
    rowUploaded(a, src('a'));
    batch.rows[0].result = result;
    let out = render(BatchPanel, { props: {} }).body;
    expect(out).toContain('195.3'); // 200000 B as KiB
    expect(out).toContain('/ 256.0');
    expect(out).toContain('Discord-safe ✓');
    expect(out).toContain('Download');
    expect(out).toContain('?dl=1');
    expect(out).toContain('Save to /output'); // capabilities unknown → optimistic
    expect(out).toContain('fit at 20 fps · 128 colours');
    // after a save, the final name shows
    batch.rows[0].savedAs = 'out-3.gif';
    out = render(BatchPanel, { props: {} }).body;
    expect(out).toContain('/output/out-3.gif');
    // a server without outputSave hides the button
    setFeatures({ features: { fit: true } });
    out = render(BatchPanel, { props: {} }).body;
    expect(out).not.toContain('Save to /output');
  });

  it('a failed row shows its error and a Retry, without touching the others', () => {
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    rowUploaded(a, src('a'));
    rowUploaded(b, src('b'));
    batch.rows[0].error = 'encoder exploded';
    const out = render(BatchPanel, { props: {} }).body;
    expect(out).toContain('encoder exploded');
    expect(out).toContain('Retry');
    expect(out).toContain('Ready — “Render all”');
  });

  it('an upload-failed row keeps Retry enabled while the dropped File is on the row (WEB-10)', () => {
    const a = addPendingRow('a.mov', new File(['x'], 'a.mov'));
    rowUploadFailed(a, 'server busy');
    let out = render(BatchPanel, { props: {} }).body;
    expect(out).toContain('server busy');
    const btn = out.match(/<button[^>]*title="Upload this file again"[^>]*>/)?.[0];
    expect(btn).toBeTruthy();
    expect(btn).not.toContain('disabled');
    // a row without a kept File (nothing to retry with) disables the button
    exitBatch();
    enterBatch();
    const b = addPendingRow('b.mov');
    rowUploadFailed(b, 'server busy');
    out = render(BatchPanel, { props: {} }).body;
    expect(out.match(/<button[^>]*title="Upload this file again"[^>]*>/)?.[0]).toContain('disabled');
  });
});

describe('BatchRenderPanel (SSR)', () => {
  beforeEach(() => enterBatch());
  afterEach(() => {
    exitBatch();
    resetFeatures();
  });

  it('counts the renderable rows and summarises done/failed', () => {
    const a = addPendingRow('a.mov');
    const b = addPendingRow('b.mov');
    rowUploaded(a, src('a'));
    rowUploaded(b, src('b'));
    batch.rows[0].result = result;
    batch.rows[1].error = 'no';
    const out = render(BatchRenderPanel, { props: {} }).body;
    expect(out).toContain('Render all (2)');
    expect(out).toContain('1 done');
    expect(out).toContain('1 failed');
    expect(out).toContain('Save all to /output (1)');
  });
});
