// Discord presets and limits (docs/DESIGN.md §5.1, §5.4, §9a).

import type { Dither, FitMode, FrameFormat, OutputFormat, PresetId, ProbeInfo, Target } from './api';

/** Byte caps per Discord target, mirroring discordlint.Limit. */
export const LIMITS: Record<Target, number> = {
  '': 0,
  emote: 262_144,
  sticker: 524_288,
  attachment: 20 * 1000 * 1000,
};

export const TARGET_LABEL: Record<Target, string> = {
  '': 'No Discord target',
  emote: 'Discord emote',
  sticker: 'Discord sticker',
  attachment: 'Discord attachment (free tier)',
};

export const TARGETS: Target[] = ['', 'emote', 'sticker', 'attachment'];

export const FORMAT_LABEL: Record<OutputFormat, string> = {
  gif: 'GIF',
  webp: 'WebP (animated)',
  apng: 'APNG (sticker only)',
  avif: 'AVIF (animated)',
  png: 'PNG (static)',
  jpeg: 'JPEG (static)',
  frames: 'Frames (zip + grid)',
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

/** Fit budgets in KiB per Discord target (the hard caps; the engine keeps a 1–2 % margin). */
export const FIT_KIB: Record<Target, number> = {
  '': 256,
  emote: 256,
  sticker: 512,
  attachment: 8192,
};

export interface PresetSwap {
  format: OutputFormat;
  label: string;
  hint: string;
}

export interface PresetDef {
  id: PresetId;
  label: string;
  hint: string;
  warn?: string;
  /** width/height/fit are pinned by the preset and shown read-only. */
  locksSize: boolean;
  /** Formats offered in the Format select while this preset is active (the first is the preset default). */
  formats: readonly OutputFormat[];
  /** A prominent one-click alternative format ("WebP instead"). */
  swap?: PresetSwap;
  /** The op stack (trim/crop/…) applies. Optimize works on the GIF bytes directly and sends no ops. */
  usesOps: boolean;
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

export const PRESETS: PresetDef[] = [
  {
    id: 'emote',
    label: 'Emote',
    hint: '128×128, ≤ 256 KiB, fit on. GIF by default (renders everywhere); WebP keeps soft alpha.',
    locksSize: true,
    formats: ['gif', 'webp', 'avif', 'png'],
    swap: { format: 'webp', label: 'WebP instead', hint: 'keeps soft edges — verified on Discord' },
    usesOps: true,
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
    hint: '320×320, ≤ 512 KiB, ≤ 5 s, fit on. Indexed 8-bit-alpha APNG by default (best sticker quality, verified); GIF as the fallback.',
    warn: 'Discord shrinks stickers larger than 320×320 (smaller / non-square are accepted). APNG animates only as a server sticker — in chat it shows frame 0.',
    locksSize: true,
    formats: ['apng', 'gif', 'png'],
    swap: { format: 'gif', label: 'GIF instead', hint: '1-bit alpha, plays everywhere — verified as a sticker' },
    usesOps: true,
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
    id: 'chat-gif',
    label: 'Chat GIF',
    hint: 'Quality-first GIF attachment (sierra2_4a dither, lossy 20), source size and fps.',
    locksSize: false,
    formats: ['gif', 'webp', 'avif'],
    usesOps: true,
    apply(o) {
      o.format = 'gif';
      o.target = 'attachment';
      o.width = 0;
      o.height = 0;
      o.fit = 'contain';
      o.fps = 0;
      o.colors = 256;
      o.dither = 'sierra2_4a';
      o.lossy = 20;
      setFit(o, false);
    },
  },
  {
    id: 'chat-webp',
    label: 'Chat WebP',
    hint: 'Animated WebP attachment with 8-bit alpha (q 80). Keep it ≤ 480 px wide for fast Discord previews.',
    locksSize: false,
    formats: ['gif', 'webp', 'avif'],
    usesOps: true,
    apply(o) {
      o.format = 'webp';
      o.target = 'attachment';
      o.width = 0;
      o.height = 0;
      o.fit = 'contain';
      o.fps = 0;
      o.quality = 80;
      o.lossless = false;
      setFit(o, false);
    },
  },
  {
    id: 'chat-avif',
    label: 'Chat AVIF',
    hint: 'Animated AVIF attachment (avifenc q 60, alpha q 90) — soft alpha, verified on Discord attachments.',
    locksSize: false,
    formats: ['gif', 'webp', 'avif'],
    usesOps: true,
    apply(o) {
      o.format = 'avif';
      o.target = 'attachment';
      o.width = 0;
      o.height = 0;
      o.fit = 'contain';
      o.fps = 0;
      o.quality = 60;
      o.lossless = false;
      setFit(o, false);
    },
  },
  {
    id: 'optimize',
    label: 'Optimize',
    hint: 'GIF → GIF with gifsicle only — no decode, no re-quantisation: lossy, colours, dither, frame drop, optional fit.',
    locksSize: true,
    formats: ['gif'],
    usesOps: false,
    available: (info) => isGifSource(info),
    unavailableHint: 'Optimize needs a GIF source (it edits the file without re-encoding)',
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
    hint: 'Everything editable, including the Discord target used by the linter; with no target the loop count is editable too.',
    locksSize: false,
    formats: ['gif', 'webp', 'apng', 'avif', 'png', 'jpeg', 'frames'],
    usesOps: true,
    apply() {
      /* keep the current values */
    },
  },
];

export function presetById(id: PresetId): PresetDef {
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

/** chatPresetFor maps an animated format to the matching Chat preset chip (null for the rest). */
export function chatPresetFor(format: OutputFormat): PresetId | null {
  switch (format) {
    case 'gif':
      return 'chat-gif';
    case 'webp':
      return 'chat-webp';
    case 'avif':
      return 'chat-avif';
    default:
      return null;
  }
}

export function defaultOutput(): OutputCfg {
  const o: OutputCfg = {
    preset: 'chat-gif',
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
    fitKiB: FIT_KIB[''],
    fitKeepSize: false,
    fitKeepFps: false,
    frameFormat: 'png',
  };
  presetById(o.preset).apply(o);
  return o;
}
