// Discord presets and limits (docs/DESIGN.md §5.1, §5.4).

import type { Dither, FitMode, OutputFormat, PresetId, Target } from './api';

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

/** The editable output configuration behind the Output card. */
export interface OutputCfg {
  preset: PresetId;
  format: OutputFormat;
  target: Target;
  width: number; // 0 = as produced by the op stack
  height: number;
  fit: FitMode;
  fps: number; // 0 = source fps (snapped per format)
  // gif
  colors: number;
  dither: Dither;
  lossy: number; // gifsicle --lossy 0..200
  alphaThreshold: number; // 1..255
  matte: string; // RRGGBB without '#'
  // webp
  quality: number; // 1..100
  lossless: boolean;
  /**
   * Loop count with GIF NETSCAPE semantics: 0 = loop forever, N > 0 = play
   * N+1 times. Only honoured for target '' (no Discord target): every Discord
   * target requires loop forever, so buildOutput sends 0 for them.
   */
  loop: number;
}

export const DEFAULT_MATTE = '313338';
export const WHITE_MATTE = 'ffffff';

export interface PresetDef {
  id: PresetId;
  label: string;
  hint: string;
  warn?: string;
  /** width/height/fit are pinned by the preset and shown read-only. */
  locksSize: boolean;
  /** apply mutates o in place; Custom leaves everything as it is. */
  apply(o: OutputCfg): void;
}

export const PRESETS: PresetDef[] = [
  {
    id: 'emote',
    label: 'Emote',
    hint: '128×128, ≤ 256 KiB. GIF by default; WebP keeps soft alpha (Discord accepts animated WebP emoji).',
    locksSize: true,
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
    },
  },
  {
    id: 'sticker',
    label: 'Sticker',
    hint: 'Exactly 320×320, ≤ 512 KiB, ≤ 5 s, ≤ 1000 frames.',
    warn: 'APNG stickers come in Phase 2; GIF stickers are experimental on Discord.',
    locksSize: true,
    apply(o) {
      o.format = 'gif';
      o.target = 'sticker';
      o.width = 320;
      o.height = 320;
      o.fit = 'contain';
      o.fps = 25;
      o.colors = 256;
      o.dither = 'bayer';
      o.lossy = 0;
    },
  },
  {
    id: 'chat-gif',
    label: 'Chat GIF',
    hint: 'Quality-first GIF attachment (sierra2_4a dither, lossy 20), source size and fps.',
    locksSize: false,
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
    },
  },
  {
    id: 'chat-webp',
    label: 'Chat WebP',
    hint: 'Animated WebP attachment with 8-bit alpha (q 80). Keep it ≤ 480 px wide for fast Discord previews.',
    locksSize: false,
    apply(o) {
      o.format = 'webp';
      o.target = 'attachment';
      o.width = 0;
      o.height = 0;
      o.fit = 'contain';
      o.fps = 0;
      o.quality = 80;
      o.lossless = false;
    },
  },
  {
    id: 'custom',
    label: 'Custom',
    hint: 'Everything editable, including the Discord target used by the linter; with no target the loop count is editable too.',
    locksSize: false,
    apply() {
      /* keep the current values */
    },
  },
];

export function presetById(id: PresetId): PresetDef {
  return PRESETS.find((p) => p.id === id) ?? PRESETS[PRESETS.length - 1];
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
    alphaThreshold: 128,
    matte: DEFAULT_MATTE,
    quality: 80,
    lossless: false,
    loop: 0,
  };
  presetById(o.preset).apply(o);
  return o;
}
