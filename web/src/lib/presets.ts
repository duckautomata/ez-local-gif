// Discord presets, targets and limits (docs/DESIGN.md §5.1, §5.4, §9a).

import type { Dither, FitMode, FrameFormat, OutputFormat, PresetId, ProbeInfo, Target } from './api';

/**
 * TargetDef is one row of the Discord-target table, mirroring
 * internal/discordlint (Target constants, Limit, IsAttachment). The four
 * attachment tiers share every rule and differ only in the byte cap:
 * 20 MB free, 50 MB Nitro Basic or a Level-2 boosted server, 100 MB a
 * Level-3 boosted server, 500 MB Nitro (DESIGN §5.1).
 */
export interface TargetDef {
  id: Target;
  /** option text of the "Discord target" dropdown */
  label: string;
  /** prose name ("Discord emote") for notes and the lint report header */
  name: string;
  /** hard byte cap; 0 = none */
  limit: number;
  /** true for every attachment tier (discordlint.IsAttachment) */
  attachment: boolean;
}

export const TARGET_DEFS: readonly TargetDef[] = [
  { id: '', label: 'none — structural checks only', name: 'No Discord target', limit: 0, attachment: false },
  { id: 'emote', label: 'emote — 256 KiB, 128×128', name: 'Discord emote', limit: 262_144, attachment: false },
  { id: 'sticker', label: 'sticker — 512 KiB, 320×320, ≤ 5 s', name: 'Discord sticker', limit: 524_288, attachment: false },
  { id: 'attachment', label: 'attachment — free, 20 MB', name: 'Discord attachment (free, 20 MB)', limit: 20_000_000, attachment: true },
  {
    id: 'attachment-50',
    label: 'attachment — Nitro Basic / Level-2 server, 50 MB',
    name: 'Discord attachment (Nitro Basic / Level-2 server, 50 MB)',
    limit: 50_000_000,
    attachment: true,
  },
  { id: 'attachment-100', label: 'attachment — Level-3 server, 100 MB', name: 'Discord attachment (Level-3 server, 100 MB)', limit: 100_000_000, attachment: true },
  { id: 'attachment-500', label: 'attachment — Nitro, 500 MB', name: 'Discord attachment (Nitro, 500 MB)', limit: 500_000_000, attachment: true },
];

/** Every target id, in dropdown order. */
export const TARGETS: readonly Target[] = TARGET_DEFS.map((t) => t.id);

/** Byte caps per Discord target, mirroring discordlint.Limit (0 = none). */
export const LIMITS: Record<Target, number> = Object.fromEntries(TARGET_DEFS.map((t) => [t.id, t.limit])) as Record<Target, number>;

/** Prose names per target ("Discord emote"). */
export const TARGET_LABEL: Record<Target, string> = Object.fromEntries(TARGET_DEFS.map((t) => [t.id, t.name])) as Record<Target, string>;

/** targetDef looks a target up (null for an id this build does not know). */
export function targetDef(t: string | null | undefined): TargetDef | null {
  return TARGET_DEFS.find((d) => d.id === (t ?? '')) ?? null;
}

/** targetLabel is the prose name of a target; an unknown id (newer server) is shown as-is. */
export function targetLabel(t: string | null | undefined): string {
  return targetDef(t)?.name ?? `Discord target "${t}"`;
}

/** limitOf is the byte cap of a target (0 for none / unknown) — discordlint.Limit. */
export function limitOf(t: string | null | undefined): number {
  return targetDef(t)?.limit ?? 0;
}

/** isAttachmentTarget mirrors discordlint.IsAttachment: any of the attachment tiers. */
export function isAttachmentTarget(t: string | null | undefined): boolean {
  return targetDef(t)?.attachment ?? false;
}

/** limitKiB is the cap in whole KiB (0 = none) — what "Fit to ≤ … KiB = limit" uses. */
export function limitKiB(t: string | null | undefined): number {
  const b = limitOf(t);
  return b > 0 ? Math.floor(b / 1024) : 0;
}

export const FORMAT_LABEL: Record<OutputFormat, string> = {
  gif: 'GIF',
  webp: 'WebP (animated)',
  apng: 'APNG (sticker only)',
  avif: 'AVIF (animated)',
  png: 'PNG (static)',
  jpeg: 'JPEG (static)',
  frames: 'Frames (zip + grid)',
};

/**
 * FORMAT_HINT is the one-line note shown under the quality knobs of a
 * format when the preset has nothing more specific to say (presets may
 * override per format via formatHints). Verified-on-Discord facts only.
 */
export const FORMAT_HINT: Record<OutputFormat, string> = {
  gif: '1-bit alpha: soft edges are cut against the matte (Advanced). Plays everywhere.',
  webp: 'libwebp_anim, 8-bit alpha, no metadata. q 80 blurs fine texture — raise it or go lossless for detailed art.',
  apng: 'Indexed APNG: one palette with 8-bit alpha (pngquant) — soft edges at a fraction of the RGBA size; verified at 25 fps as a sticker.',
  avif: 'avifenc, alpha q 90, 4:2:0 — soft alpha verified on Discord attachments (Discord transcodes to WebP; large files take a few seconds to appear).',
  png: 'First frame as RGBA PNG: pngquant to the palette when set, then oxipng — no fit search for a static PNG.',
  jpeg: 'First frame, no alpha: transparent areas are flattened onto the matte (Advanced).',
  frames: 'One file per frame after the op stack, plus frames.zip (stored) and delays.json; the result shows a thumbnail grid.',
};

export const FRAME_FORMATS: { id: FrameFormat; label: string }[] = [
  { id: 'png', label: 'PNG (RGBA, lossless)' },
  { id: 'jpeg', label: 'JPEG (flattened onto the matte)' },
  { id: 'webp', label: 'WebP (lossless)' },
];

/** The editable output configuration behind the Output card. */
export interface OutputCfg {
  preset: PresetId;
  format: OutputFormat;
  target: Target;
  width: number; // 0 = as produced by the op stack
  height: number;
  fit: FitMode;
  fps: number; // 0 = source fps (snapped per format)
  // gif / apng
  colors: number; // gif 2..256; apng 0 = RGBA truecolour, 2..256 = indexed 8-bit alpha
  dither: Dither;
  lossy: number; // gifsicle --lossy 0..200
  alphaThreshold: number; // 1..255; TRIM_FRINGE_THRESHOLD = "trim fringe"
  matte: string; // RRGGBB without '#'
  // webp / avif / jpeg
  quality: number; // 1..100
  lossless: boolean;
  /**
   * Loop count with GIF NETSCAPE semantics: 0 = loop forever, N > 0 = play
   * N+1 times. Only honoured for target '' (no Discord target): every Discord
   * target requires loop forever, so buildOutput sends 0 for them.
   */
  loop: number;
  // fit-to-size (Phase 2)
  fitEnabled: boolean;
  fitKiB: number; // budget in KiB; sent as fitBytes = fitKiB * 1024
  fitKeepSize: boolean;
  fitKeepFps: boolean;
  // frames
  frameFormat: FrameFormat;
}

export const DEFAULT_MATTE = '313338';
export const WHITE_MATTE = 'ffffff';
/** GIF alpha threshold defaults: 128 normal, 180 = "trim fringe" (DESIGN §4.2, §9a). */
export const DEFAULT_ALPHA_THRESHOLD = 128;
export const TRIM_FRINGE_THRESHOLD = 180;

/**
 * Formats the server's fit-to-size engine can search — mirrors
 * internal/jobs/fit.go fitFormats (DESIGN §5.4: static PNG has no fit ladder,
 * frame extraction never fits). The server ignores fitBytes for the rest, so
 * buildOutput never emits it for them and the Output card hides the fit row.
 */
export const FIT_FORMATS: ReadonlySet<OutputFormat> = new Set<OutputFormat>(['gif', 'webp', 'apng', 'avif', 'jpeg']);

/** fitsFormat reports whether the fit-to-size engine applies to the format. */
export function fitsFormat(f: OutputFormat): boolean {
  return FIT_FORMATS.has(f);
}

/** DEFAULT_FIT_KIB is the budget offered when fit is switched on with no Discord target. */
export const DEFAULT_FIT_KIB = 256;

/** fitKiBFor is the default fit budget for a target: its cap in KiB, or DEFAULT_FIT_KIB with no target. */
export function fitKiBFor(t: string | null | undefined): number {
  return limitKiB(t) || DEFAULT_FIT_KIB;
}

/** Fit budgets in KiB per Discord target (the hard caps; the engine keeps a 1–2 % margin). */
export const FIT_KIB: Record<Target, number> = Object.fromEntries(TARGET_DEFS.map((t) => [t.id, fitKiBFor(t.id)])) as Record<Target, number>;

export interface PresetDef {
  id: PresetId;
  label: string;
  hint: string;
  warn?: string;
  /** width/height/fit are pinned by the preset and shown read-only. */
  locksSize: boolean;
  /** Formats offered in the Format select while this preset is active (the first is the preset default). */
  formats: readonly OutputFormat[];
  /** The Discord target the preset starts with — the dropdown stays editable. */
  target: Target;
  /** The op stack (trim/crop/…) applies. Optimize works on the GIF bytes directly and sends no ops. */
  usesOps: boolean;
  /** One-line notes per format shown under the quality knobs (fall back to FORMAT_HINT). */
  formatHints?: Partial<Record<OutputFormat, string>>;
  /** onFormat re-seeds the format's quality defaults when the user switches the Format select within the preset. */
  onFormat?(o: OutputCfg, format: OutputFormat): void;
  /** available reports whether the preset can be used with this source (default: always). */
  available?(info: ProbeInfo | null): boolean;
  unavailableHint?: string;
  /** apply mutates o in place; Custom leaves everything as it is. */
  apply(o: OutputCfg, info?: ProbeInfo | null): void;
}

/** isSequence reports whether the source is an uploaded image sequence. */
export function isSequence(info: ProbeInfo | null | undefined): boolean {
  return !!info && (info.kind === 'sequence' || !!info.sequence);
}

/**
 * isGifSource: the Optimize preset's gifsicle-only path needs an animated
 * GIF *file*. An image sequence is never one — the server's optimiser
 * rejects sequences with a 400 even when the frames are .gif files — and a
 * still image has no animation to optimise.
 */
export function isGifSource(info: ProbeInfo | null | undefined): boolean {
  if (!info || isSequence(info)) return false;
  return info.kind === 'animation' && info.codec === 'gif';
}

function setFit(o: OutputCfg, enabled: boolean, kib?: number, keepSize = false, keepFps = false): void {
  o.fitEnabled = enabled;
  if (kib !== undefined) o.fitKiB = kib;
  o.fitKeepSize = keepSize;
  o.fitKeepFps = keepFps;
}

/**
 * chatFormat seeds the quality-first chat defaults of a format: GIF
 * sierra2_4a + lossy 20, WebP q 80, AVIF q 60 (the former chat-gif /
 * chat-webp / chat-avif presets).
 */
function chatFormat(o: OutputCfg, format: OutputFormat): void {
  switch (format) {
    case 'gif':
      o.colors = 256;
      o.dither = 'sierra2_4a';
      o.lossy = 20;
      break;
    case 'webp':
      o.quality = 80;
      o.lossless = false;
      break;
    case 'avif':
      o.quality = 60;
      o.lossless = false;
      break;
  }
}

export const PRESETS: PresetDef[] = [
  {
    id: 'emote',
    label: 'Emote',
    hint: '128×128, ≤ 256 KiB, fit on. GIF renders everywhere; WebP keeps soft alpha.',
    locksSize: true,
    formats: ['gif', 'webp', 'avif', 'png'],
    target: 'emote',
    usesOps: true,
    formatHints: {
      gif: 'Universal emote format; 1-bit alpha — pick the matte for your audience’s theme under Advanced.',
      webp: 'Keeps soft edges — verified on Discord as an animated emoji.',
      avif: 'Animated AVIF is accepted as an emote (since 2025-05); soft alpha.',
      png: 'Static emote: pngquant palette + oxipng, fits 256 KiB trivially.',
    },
    apply(o) {
      o.format = 'gif';
      o.target = 'emote';
      o.width = 128;
      o.height = 128;
      o.fit = 'contain';
      o.fps = 25;
      o.colors = 256;
      o.dither = 'bayer';
      o.lossy = 0;
      setFit(o, true, FIT_KIB.emote);
    },
  },
  {
    id: 'sticker',
    label: 'Sticker',
    hint: '320×320, ≤ 512 KiB, ≤ 5 s, fit on (never downscaled). Indexed 8-bit-alpha APNG by default; GIF as the fallback.',
    warn: 'Discord shrinks stickers larger than 320×320 (smaller / non-square are accepted). APNG animates only as a server sticker — in chat it shows frame 0.',
    locksSize: true,
    formats: ['apng', 'gif', 'png'],
    target: 'sticker',
    usesOps: true,
    formatHints: {
      apng: 'Indexed 8-bit-alpha APNG — best sticker quality, verified at 25 fps; the fit ladder walks 256 → 128 → 64 colours.',
      gif: '1-bit alpha, plays everywhere — verified as a sticker.',
      png: 'Static sticker: pngquant palette + oxipng.',
    },
    apply(o) {
      o.format = 'apng';
      o.target = 'sticker';
      o.width = 320;
      o.height = 320;
      o.fit = 'contain';
      o.fps = 25;
      o.colors = 256; // indexed 8-bit-alpha APNG (0 would be RGBA truecolour)
      o.dither = 'bayer';
      o.lossy = 0;
      setFit(o, true, FIT_KIB.sticker, true); // stickers are never downscaled
    },
  },
  {
    id: 'chat',
    label: 'Chat',
    hint: 'Chat attachment at the source size and fps, quality first. Pick the format: GIF plays everywhere, WebP / AVIF keep soft alpha.',
    locksSize: false,
    formats: ['gif', 'webp', 'avif'],
    target: 'attachment',
    usesOps: true,
    formatHints: {
      gif: 'sierra2_4a dither, lossy 20 — 1-bit alpha, so pick the matte for your audience’s theme under Advanced.',
      webp: '8-bit alpha, q 80. Keep it ≤ 480 px wide — Discord’s proxy takes seconds to show bigger animated WebPs.',
      avif: 'avifenc q 60, alpha q 90 — soft alpha, verified on Discord attachments (transcoded to WebP on upload).',
    },
    onFormat: chatFormat,
    apply(o) {
      o.format = 'gif';
      o.target = 'attachment';
      o.width = 0;
      o.height = 0;
      o.fit = 'contain';
      o.fps = 0;
      o.quality = 80; // the WebP default; onFormat re-seeds per format on a switch
      o.lossless = false;
      chatFormat(o, 'gif');
      setFit(o, false);
    },
  },
  {
    id: 'optimize',
    label: 'Optimize',
    hint: 'GIF → GIF with gifsicle only — no decode, no re-quantisation: lossy, colours, dither, frame drop, optional fit.',
    locksSize: true,
    formats: ['gif'],
    target: '',
    usesOps: false,
    available: (info) => isGifSource(info),
    unavailableHint: 'Optimize needs a GIF source (it edits the file without re-encoding)',
    formatHints: { gif: 'The source palette is kept (gifsicle --colors only drops entries); no matte or alpha threshold applies.' },
    apply(o) {
      o.format = 'gif';
      o.target = '';
      o.width = 0;
      o.height = 0;
      o.fit = 'contain';
      o.fps = 0;
      o.colors = 256;
      o.dither = 'bayer';
      o.lossy = 30;
      setFit(o, false);
    },
  },
  {
    id: 'frames',
    label: 'Frames',
    hint: 'Extract every frame (after trim / fps / crop / resize) as PNG, JPEG or WebP files plus a zip.',
    locksSize: false,
    formats: ['frames'],
    target: '',
    usesOps: true,
    apply(o) {
      o.format = 'frames';
      o.target = '';
      o.width = 0;
      o.height = 0;
      o.fit = 'contain';
      o.fps = 0;
      o.frameFormat = 'png';
      setFit(o, false);
    },
  },
  {
    id: 'custom',
    label: 'Custom',
    hint: 'Everything editable; with no Discord target the loop count is editable too.',
    locksSize: false,
    formats: ['gif', 'webp', 'apng', 'avif', 'png', 'jpeg', 'frames'],
    target: '',
    usesOps: true,
    apply() {
      /* keep the current values */
    },
  },
];

export function presetById(id: PresetId | string): PresetDef {
  return PRESETS.find((p) => p.id === id) ?? PRESETS[PRESETS.length - 1];
}

/** presetAvailable: whether the preset chip is enabled for this source (no source: everything except Optimize). */
export function presetAvailable(p: PresetDef, info: ProbeInfo | null | undefined): boolean {
  return p.available ? p.available(info ?? null) : true;
}

/** formatsFor lists the formats the Output card offers for the preset (always includes the current one). */
export function formatsFor(p: PresetDef, current: OutputFormat): readonly OutputFormat[] {
  return p.formats.includes(current) ? p.formats : [...p.formats, current];
}

/** formatHint is the one-line note for a format under a preset (the preset's own, else the generic one). */
export function formatHint(p: PresetDef, format: OutputFormat): string {
  return p.formatHints?.[format] ?? FORMAT_HINT[format];
}

export function defaultOutput(): OutputCfg {
  const o: OutputCfg = {
    preset: 'chat',
    format: 'gif',
    target: 'attachment',
    width: 0,
    height: 0,
    fit: 'contain',
    fps: 0,
    colors: 256,
    dither: 'sierra2_4a',
    lossy: 20,
    alphaThreshold: DEFAULT_ALPHA_THRESHOLD,
    matte: DEFAULT_MATTE,
    quality: 80,
    lossless: false,
    loop: 0,
    fitEnabled: false,
    fitKiB: DEFAULT_FIT_KIB,
    fitKeepSize: false,
    fitKeepFps: false,
    frameFormat: 'png',
  };
  presetById(o.preset).apply(o);
  return o;
}
