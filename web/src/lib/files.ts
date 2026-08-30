// Dropped / picked / pasted file handling: one file is a source, several
// image files are an image sequence (uploaded in one request, server-side
// store.PutSequence), several video/animation files are a batch (Phase 4:
// each its own source, same ops + output), a mix of both asks the user.
// Framework-free (files.test.ts).

/** Minimal shape of a File used here (so tests need no DOM File). */
export interface NamedFile {
  name: string;
  type: string;
}

/**
 * Extensions the server accepts as image-sequence frames — exactly the
 * still-image formats ffmpeg's image2 demuxer can read, mirroring
 * internal/server/upload.go sequenceImageExts. GIF and AVIF are deliberately
 * absent: the server refuses them as sequence frames with a 400 (they upload
 * fine as single files, one at a time). An ANIMATED WebP / APNG shares its
 * still extension — planDropSniffed reads the file heads and reclassifies
 * those as non-sequence (batch) files.
 */
const SEQUENCE_EXT = new Set(['png', 'jpg', 'jpeg', 'webp', 'bmp', 'tif', 'tiff']);

/** The MIME types of SEQUENCE_EXT (the server sniffs the content the same way). */
const SEQUENCE_MIME = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/bmp', 'image/tiff']);

/** extOf returns the lowercase extension without the dot ('' when none). */
export function extOf(name: string): string {
  const i = name.lastIndexOf('.');
  return i < 0 ? '' : name.slice(i + 1).toLowerCase();
}

/**
 * isSequenceFrame: a file the server accepts as an image-sequence frame
 * (png/jpeg/webp/bmp/tiff) — by extension or, failing that, by MIME type.
 * Notably false for gif/avif, which upload fine as single files only.
 */
export function isSequenceFrame(f: NamedFile): boolean {
  return SEQUENCE_EXT.has(extOf(f.name)) || SEQUENCE_MIME.has(f.type.toLowerCase());
}

/**
 * naturalCompare orders names the way a sequence is numbered: "f2" before
 * "f10" (numeric chunks compare by value), case-insensitive.
 */
export function naturalCompare(a: string, b: string): number {
  return a.localeCompare(b, 'en', { numeric: true, sensitivity: 'base' }) || a.localeCompare(b);
}

export type DropPlan<F extends NamedFile = NamedFile> =
  | { kind: 'single'; file: F }
  | { kind: 'sequence'; files: F[] }
  /** Phase 4: ≥ 2 video/animation files — each becomes its own source (batch mode) */
  | { kind: 'batch'; files: F[] }
  /**
   * Phase 4: a mix of sequence-eligible images and other files — ambiguous,
   * so the UI asks: batch every file, or (with ≥ 2 images) use the images as
   * one sequence. `images` is the sequence-eligible subset, `files` everything.
   */
  | { kind: 'mixed'; images: F[]; files: F[] }
  | { kind: 'none' };

/**
 * planDrop decides what a set of files is:
 *
 *   - nothing, or one file = one source (unchanged);
 *   - ≥ 2 files that are ALL sequence-eligible images (png/jpeg/webp/bmp/
 *     tiff) = one image sequence, sorted naturally by name (unchanged — a
 *     multi-drop of images only stays a sequence);
 *   - ≥ 2 files with NO sequence-eligible image (videos, gifs, animated
 *     webp/avif…) = a batch: every file its own source, the same ops +
 *     output for all (Phase 4);
 *   - a mix of both = 'mixed': the caller asks which was meant (batch all,
 *     or the images as one sequence).
 */
export function planDrop<F extends NamedFile>(list: readonly F[] | null | undefined): DropPlan<F> & { note?: string } {
  if (!list || list.length === 0) return { kind: 'none' };
  if (list.length === 1) return { kind: 'single', file: list[0] };
  const sorted = [...list].sort((a, b) => naturalCompare(a.name, b.name));
  const images = sorted.filter(isSequenceFrame);
  if (images.length === sorted.length) return { kind: 'sequence', files: sorted };
  if (images.length === 0) return { kind: 'batch', files: sorted };
  return { kind: 'mixed', images, files: sorted };
}

// ---------------------------------------------------------------------------
// Animation sniff (WEB-2): .webp and .png are sequence-eligible by extension,
// but an ANIMATED WebP / APNG dropped among several files must never become a
// first-frame slideshow — the server accepts the frames without an animation
// check, so the client reads each candidate's first 512 bytes and reclassifies
// animated files as non-sequence (all animated = a batch; animated + still =
// the mixed question). Mirrors the server backstop in
// internal/server/upload.go (sniffAnimated).

/** How many head bytes the sniff reads (matches the server's sniff window). */
export const SNIFF_HEAD_BYTES = 512;

function fourcc(b: Uint8Array, off: number): string {
  return off + 4 <= b.length ? String.fromCharCode(b[off], b[off + 1], b[off + 2], b[off + 3]) : '';
}

/**
 * sniffAnimated reports whether a file head is an animated WebP or an APNG:
 *
 *   - WebP: 'RIFF'…'WEBP' whose VP8X chunk (at offset 12) has the Animation
 *     bit (0x02) set in its flags byte at offset 20 — or, failing that, an
 *     'ANIM' chunk in the head (the check is exact per spec: VP8X is
 *     mandatory and first for animated WebP);
 *   - APNG: a PNG whose 'acTL' chunk occurs before 'IDAT' (walking the chunk
 *     list, so 'acTL' bytes inside chunk data never misfire). Best-effort: an
 *     acTL beyond the head (e.g. after a huge iCCP) is not seen.
 *
 * Everything else — still images, other formats, a short/empty head — is
 * false.
 */
export function sniffAnimated(head: Uint8Array): boolean {
  // Animated WebP: RIFF container, chunks of fourcc + u32le size (padded to even).
  if (fourcc(head, 0) === 'RIFF' && fourcc(head, 8) === 'WEBP') {
    if (fourcc(head, 12) === 'VP8X' && head.length > 20) return (head[20] & 0x02) !== 0;
    for (let off = 12; off + 8 <= head.length; ) {
      if (fourcc(head, off) === 'ANIM') return true;
      const size = head[off + 4] | (head[off + 5] << 8) | (head[off + 6] << 16) | (head[off + 7] << 24);
      if (size < 0) return false;
      off += 8 + size + (size & 1);
    }
    return false;
  }
  // APNG: PNG signature, chunks of u32be length + type + data + CRC.
  if (head.length >= 8 && head[0] === 0x89 && head[1] === 0x50 && head[2] === 0x4e && head[3] === 0x47) {
    for (let off = 8; off + 8 <= head.length; ) {
      const type = fourcc(head, off + 4);
      if (type === 'acTL') return true;
      if (type === 'IDAT' || type === 'IEND') return false;
      const len = (head[off] << 24) | (head[off + 1] << 16) | (head[off + 2] << 8) | head[off + 3];
      if (len < 0) return false;
      off += 12 + len;
    }
  }
  return false;
}

/** readHeadBlob reads a File/Blob's first SNIFF_HEAD_BYTES (the default head reader). */
async function readHeadBlob(f: NamedFile): Promise<Uint8Array> {
  const blob = f as unknown as Blob;
  if (typeof blob.slice !== 'function') return new Uint8Array(0);
  return new Uint8Array(await blob.slice(0, SNIFF_HEAD_BYTES).arrayBuffer());
}

/**
 * planDropSniffed is planDrop plus the animation sniff: when the extension
 * plan would treat files as sequence frames ('sequence' or 'mixed'), each
 * candidate's head is read and sniffed-animated files (animated WebP / APNG)
 * are reclassified as non-sequence before the plan is re-derived — so an
 * all-animated drop becomes a batch and an animated+still mix asks the user.
 * On any head-read failure the extension-based plan stands (best effort, like
 * the server's own sniff). The head reader is injectable so tests need no DOM
 * File.
 */
export async function planDropSniffed<F extends NamedFile>(
  list: readonly F[] | null | undefined,
  readHead: (f: F) => Promise<Uint8Array> = readHeadBlob,
): Promise<DropPlan<F> & { note?: string }> {
  const plan = planDrop(list);
  if (plan.kind !== 'sequence' && plan.kind !== 'mixed') return plan;
  const candidates = plan.kind === 'sequence' ? plan.files : plan.images;
  let animated: Set<F>;
  try {
    const heads = await Promise.all(candidates.map((f) => readHead(f)));
    animated = new Set(candidates.filter((_, i) => sniffAnimated(heads[i])));
  } catch {
    return plan; // could not read a head: keep the extension-based plan
  }
  if (animated.size === 0) return plan;
  const all = plan.files; // already naturally sorted by planDrop
  const images = all.filter((f) => isSequenceFrame(f) && !animated.has(f));
  if (images.length === 0) return { kind: 'batch', files: all };
  return { kind: 'mixed', images, files: all };
}

/** sequenceFps is the frame rate implied by a per-frame delay in ms (0 for no delay). */
export function sequenceFps(delayMs: number): number {
  return delayMs > 0 ? 1000 / delayMs : 0;
}

/**
 * sequenceDelayOverride: the server dedupes an image sequence by frame
 * content, so re-uploading identical frames returns the Source with the delay
 * the frames were FIRST stored with — the "delayMs" form field only seeds a
 * sequence that is new to the store (internal/server/upload.go). When the
 * returned sequence carries a different delay than the one requested, the
 * request must be honoured client-side via the "delay" op (the documented
 * override, recipe.DelayParams). Returns the ms to put into that op — the
 * requested value rounded and clamped to the op's 1..60000 range, so it
 * compares like buildOps serialises — or 0 when nothing needs overriding
 * (single file, no sequence info, or the stored delay already matches).
 */
export function sequenceDelayOverride(seq: { delayMs: number } | null | undefined, fileCount: number, requestedMs: number): number {
  if (fileCount <= 1 || !seq) return 0;
  const want = Math.min(60000, Math.max(1, Math.round(requestedMs)));
  return seq.delayMs !== want ? want : 0;
}
