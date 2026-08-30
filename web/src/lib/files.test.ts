import { describe, expect, it } from 'vitest';
import { extOf, isSequenceFrame, naturalCompare, planDrop, planDropSniffed, sequenceDelayOverride, sequenceFps, sniffAnimated } from './files';

const f = (name: string, type = '') => ({ name, type });

// ---- synthetic file heads for the animation sniff (WEB-2) ----------------
const ascii = (s: string) => [...s].map((c) => c.charCodeAt(0));
const u32be = (n: number) => [(n >>> 24) & 255, (n >>> 16) & 255, (n >>> 8) & 255, n & 255];
const u32le = (n: number) => [n & 255, (n >>> 8) & 255, (n >>> 16) & 255, (n >>> 24) & 255];
const bytes = (...parts: number[][]) => new Uint8Array(parts.flat());
const PNG_SIG = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
/** a PNG chunk: u32be length + type + zeroed data + zeroed CRC */
const pngChunk = (type: string, dataLen: number) => [...u32be(dataLen), ...ascii(type), ...new Array(dataLen + 4).fill(0)];
/** VP8X-led WebP head with the given flags byte (0x02 = Animation) */
const webpVP8X = (flags: number) => bytes(ascii('RIFF'), u32le(200), ascii('WEBP'), ascii('VP8X'), u32le(10), [flags, 0, 0, 0, 0, 0, 0, 0, 0, 0]);
const ANIM_WEBP = webpVP8X(0x12); // animation + alpha
const STILL_WEBP_X = webpVP8X(0x10); // alpha only
const LOSSY_WEBP = bytes(ascii('RIFF'), u32le(100), ascii('WEBP'), ascii('VP8 '), u32le(20), new Array(20).fill(0));
const APNG = bytes(PNG_SIG, pngChunk('IHDR', 13), pngChunk('acTL', 8), pngChunk('IDAT', 4));
const STILL_PNG = bytes(PNG_SIG, pngChunk('IHDR', 13), pngChunk('IDAT', 4));

describe('planDrop', () => {
  it('handles nothing / one file', () => {
    expect(planDrop(null)).toEqual({ kind: 'none' });
    expect(planDrop([])).toEqual({ kind: 'none' });
    expect(planDrop([f('clip.mov', 'video/quicktime')])).toEqual({ kind: 'single', file: f('clip.mov', 'video/quicktime') });
  });

  it('turns several images into one naturally sorted sequence', () => {
    const plan = planDrop([f('frame10.png', 'image/png'), f('frame2.png', 'image/png'), f('Frame1.PNG', '')]);
    expect(plan.kind).toBe('sequence');
    if (plan.kind === 'sequence') expect(plan.files.map((x) => x.name)).toEqual(['Frame1.PNG', 'frame2.png', 'frame10.png']);
    expect(plan.note).toBeUndefined();
  });

  // Phase 4: several video/animation files enter batch mode.
  it('plans ≥ 2 non-image files as a naturally sorted batch', () => {
    const plan = planDrop([f('b10.mov', 'video/quicktime'), f('b2.mov', 'video/quicktime'), f('a.webm', 'video/webm')]);
    expect(plan.kind).toBe('batch');
    if (plan.kind === 'batch') expect(plan.files.map((x) => x.name)).toEqual(['a.webm', 'b2.mov', 'b10.mov']);
  });

  // SRV-1: the server rejects gif/avif sequence frames with a 400, so a set
  // of them must never be offered as a sequence — they are a batch now.
  it('never plans gif/avif files as a sequence (they batch instead)', () => {
    const plan = planDrop([f('a1.gif', 'image/gif'), f('a2.gif', 'image/gif')]);
    expect(plan.kind).toBe('batch');
    if (plan.kind === 'batch') expect(plan.files.map((x) => x.name)).toEqual(['a1.gif', 'a2.gif']);
    expect(planDrop([f('b1.avif', 'image/avif'), f('b2.avif', 'image/avif')]).kind).toBe('batch');
  });

  // Phase 4: images + other files is ambiguous — the UI asks.
  it('marks a mix of images and other files as mixed, with the image subset split out', () => {
    const plan = planDrop([f('a.mov', 'video/quicktime'), f('b2.png', 'image/png'), f('b1.png', 'image/png')]);
    expect(plan.kind).toBe('mixed');
    if (plan.kind === 'mixed') {
      expect(plan.files.map((x) => x.name)).toEqual(['a.mov', 'b1.png', 'b2.png']);
      expect(plan.images.map((x) => x.name)).toEqual(['b1.png', 'b2.png']);
    }
    // a single image among videos still asks (batch all is the sane answer)
    const one = planDrop([f('a.mov', 'video/quicktime'), f('b.png', 'image/png')]);
    expect(one.kind).toBe('mixed');
    if (one.kind === 'mixed') expect(one.images.map((x) => x.name)).toEqual(['b.png']);
  });
});

describe('sniffAnimated', () => {
  it('detects animated WebP by the VP8X Animation bit and APNG by acTL before IDAT', () => {
    expect(sniffAnimated(ANIM_WEBP)).toBe(true);
    expect(sniffAnimated(STILL_WEBP_X)).toBe(false);
    expect(sniffAnimated(LOSSY_WEBP)).toBe(false);
    expect(sniffAnimated(APNG)).toBe(true);
    expect(sniffAnimated(STILL_PNG)).toBe(false);
  });

  it('falls back to an ANIM chunk when VP8X is not the first WebP chunk', () => {
    const head = bytes(ascii('RIFF'), u32le(200), ascii('WEBP'), ascii('ICCP'), u32le(4), [0, 0, 0, 0], ascii('ANIM'), u32le(6), new Array(6).fill(0));
    expect(sniffAnimated(head)).toBe(true);
  });

  it('never misfires on acTL bytes inside chunk data, short heads or other formats', () => {
    // "acTL" as literal bytes INSIDE the IDAT data must not count
    const tricky = bytes(PNG_SIG, pngChunk('IHDR', 13), [...u32be(8), ...ascii('IDAT'), ...ascii('acTL'), 0, 0, 0, 0, 0, 0, 0, 0]);
    expect(sniffAnimated(tricky)).toBe(false);
    expect(sniffAnimated(new Uint8Array(0))).toBe(false);
    expect(sniffAnimated(bytes(ascii('GIF89a')))).toBe(false);
  });
});

// WEB-2: a multi-drop of ANIMATED WebP/APNG (the core emote workflow) must
// never silently become a first-frame image sequence — the heads are sniffed
// and animated files reclassified as non-sequence before the plan stands.
describe('planDropSniffed', () => {
  const heads: Record<string, Uint8Array> = {
    'a1.webp': ANIM_WEBP,
    'a2.webp': ANIM_WEBP,
    's1.webp': STILL_WEBP_X,
    'p1.png': STILL_PNG,
    'p2.png': STILL_PNG,
    'ap1.png': APNG,
    'ap2.png': APNG,
  };
  const readHead = (x: { name: string }) => Promise.resolve(heads[x.name] ?? new Uint8Array(0));

  it('turns an all-animated drop into a batch (animated WebP and APNG alike)', async () => {
    const webp = await planDropSniffed([f('a2.webp', 'image/webp'), f('a1.webp', 'image/webp')], readHead);
    expect(webp.kind).toBe('batch');
    if (webp.kind === 'batch') expect(webp.files.map((x) => x.name)).toEqual(['a1.webp', 'a2.webp']);
    expect((await planDropSniffed([f('ap1.png', 'image/png'), f('ap2.png', 'image/png')], readHead)).kind).toBe('batch');
  });

  it('turns an animated + still drop into the mixed question (stills as the sequence subset)', async () => {
    const plan = await planDropSniffed([f('a1.webp', 'image/webp'), f('p1.png', 'image/png'), f('p2.png', 'image/png')], readHead);
    expect(plan.kind).toBe('mixed');
    if (plan.kind === 'mixed') {
      expect(plan.images.map((x) => x.name)).toEqual(['p1.png', 'p2.png']);
      expect(plan.files.map((x) => x.name)).toEqual(['a1.webp', 'p1.png', 'p2.png']);
    }
  });

  it('re-sniffs the image subset of an already-mixed drop', async () => {
    const plan = await planDropSniffed([f('clip.mov', 'video/quicktime'), f('a1.webp', 'image/webp'), f('p1.png', 'image/png')], readHead);
    expect(plan.kind).toBe('mixed');
    if (plan.kind === 'mixed') expect(plan.images.map((x) => x.name)).toEqual(['p1.png']); // the animated webp left the sequence subset
  });

  it('leaves an all-still drop as a sequence and non-sequence plans untouched', async () => {
    const seq = await planDropSniffed([f('p2.png', 'image/png'), f('p1.png', 'image/png'), f('s1.webp', 'image/webp')], readHead);
    expect(seq.kind).toBe('sequence');
    if (seq.kind === 'sequence') expect(seq.files.map((x) => x.name)).toEqual(['p1.png', 'p2.png', 's1.webp']);
    // single / batch / none plans never read a head
    const noRead = () => Promise.reject(new Error('must not be called'));
    expect((await planDropSniffed([f('ap1.png', 'image/png')], noRead)).kind).toBe('single');
    expect((await planDropSniffed([f('a.mov', 'video/quicktime'), f('b.mov', 'video/quicktime')], noRead)).kind).toBe('batch');
    expect((await planDropSniffed([], noRead)).kind).toBe('none');
  });

  it('falls back to the extension-based plan when a head cannot be read', async () => {
    const plan = await planDropSniffed([f('ap1.png', 'image/png'), f('ap2.png', 'image/png')], () => Promise.reject(new Error('io')));
    expect(plan.kind).toBe('sequence'); // best effort — the server backstop still refuses it
  });
});

describe('helpers', () => {
  it('isSequenceFrame accepts png/jpeg/webp/bmp/tiff by extension or MIME type, never gif/avif', () => {
    expect(isSequenceFrame(f('x.bin', 'image/webp'))).toBe(true);
    expect(isSequenceFrame(f('x.JPG'))).toBe(true);
    expect(isSequenceFrame(f('x.bmp', 'image/bmp'))).toBe(true);
    expect(isSequenceFrame(f('x.tif'))).toBe(true);
    expect(isSequenceFrame(f('x.gif', 'image/gif'))).toBe(false);
    expect(isSequenceFrame(f('x.avif', 'image/avif'))).toBe(false);
    expect(isSequenceFrame(f('x.mov', 'video/quicktime'))).toBe(false);
    expect(isSequenceFrame(f('noext'))).toBe(false);
    expect(extOf('a.b.PNG')).toBe('png');
    expect(extOf('none')).toBe('');
  });
  it('naturalCompare orders numeric chunks by value', () => {
    expect(['f10', 'f9', 'f1'].sort(naturalCompare)).toEqual(['f1', 'f9', 'f10']);
    expect(['b', 'A', 'a'].sort(naturalCompare)[2]).toBe('b'); // case-insensitive
  });
  it('sequenceFps inverts the delay', () => {
    expect(sequenceFps(100)).toBe(10);
    expect(sequenceFps(40)).toBe(25);
    expect(sequenceFps(0)).toBe(0);
  });
});

describe('sequenceDelayOverride', () => {
  // The store dedupes identical frame sets: a re-upload comes back with the
  // FIRST stored delay, so a different requested delay must become a "delay" op.
  it('returns the requested delay when the deduped sequence came back with another one', () => {
    expect(sequenceDelayOverride({ delayMs: 100 }, 12, 40)).toBe(40);
    expect(sequenceDelayOverride({ delayMs: 40 }, 2, 100)).toBe(100);
  });

  it('returns 0 when the stored delay already matches, for single files, and without sequence info', () => {
    expect(sequenceDelayOverride({ delayMs: 100 }, 12, 100)).toBe(0);
    expect(sequenceDelayOverride({ delayMs: 100 }, 1, 40)).toBe(0); // one file is never a sequence
    expect(sequenceDelayOverride(null, 12, 40)).toBe(0);
    expect(sequenceDelayOverride(undefined, 12, 40)).toBe(0);
  });

  it('rounds and clamps to the delay op range (1..60000) before comparing', () => {
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 99.6)).toBe(0); // rounds to the stored 100
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 40.4)).toBe(40);
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 0.2)).toBe(1);
    expect(sequenceDelayOverride({ delayMs: 100 }, 3, 99_999)).toBe(60000);
    expect(sequenceDelayOverride({ delayMs: 60000 }, 3, 99_999)).toBe(0); // both clamp to the cap
  });
});
