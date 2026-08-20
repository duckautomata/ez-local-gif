// Dropped / picked / pasted file handling: one file is a source, several
// image files are an image sequence (uploaded in one request, server-side
// store.PutSequence). Framework-free (files.test.ts).

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
 * fine as single files, one at a time).
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
  | { kind: 'none' };

/**
 * planDrop decides what a set of files is: nothing, one source, or — when
 * every file is a sequence-eligible image (png/jpeg/webp/bmp/tiff) and there
 * are at least two — one image sequence, sorted naturally by name. Several
 * files that do not all qualify (mixed drops, but also sets of gif/avif
 * files, which the server rejects as sequence frames) fall back to the first
 * one; `note` explains that.
 */
export function planDrop<F extends NamedFile>(list: readonly F[] | null | undefined): DropPlan<F> & { note?: string } {
  if (!list || list.length === 0) return { kind: 'none' };
  if (list.length === 1) return { kind: 'single', file: list[0] };
  if (list.every(isSequenceFrame)) {
    const files = [...list].sort((a, b) => naturalCompare(a.name, b.name));
    return { kind: 'sequence', files };
  }
  return {
    kind: 'single',
    file: list[0],
    note: `Using the first of ${list.length} files — only a set of png / jpeg / webp / bmp / tiff images is uploaded as a sequence`,
  };
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
