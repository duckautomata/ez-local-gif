// Small pure formatting / arithmetic helpers shared by the components.

import type { FitMode, OutputFormat } from './api';

/** round to d decimals (default 3), avoiding -0. */
export function round(v: number, d = 3): number {
  const f = 10 ** d;
  const r = Math.round(v * f) / f;
  return r === 0 ? 0 : r;
}

export function clamp(v: number, min: number, max: number): number {
  return Math.min(Math.max(v, min), max);
}

/** fmtKiB formats bytes as KiB: one decimal below 1000 KiB, whole KiB with separators above. */
export function fmtKiB(bytes: number): string {
  const k = bytes / 1024;
  if (k < 1000) return k.toFixed(1);
  return Math.round(k).toLocaleString('en-US');
}

/** fmtBytes formats a byte count for humans (IEC units). */
export function fmtBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KiB', 'MiB', 'GiB'];
  let v = bytes / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(2) : v < 100 ? v.toFixed(1) : Math.round(v).toLocaleString('en-US')} ${units[i]}`;
}

/** fmtSeconds: "1.50 s", "0.033 s" for tiny values, "1:02.5" above a minute. */
export function fmtSeconds(s: number): string {
  if (!Number.isFinite(s)) return '—';
  if (s >= 60) {
    const m = Math.floor(s / 60);
    const rest = s - m * 60;
    return `${m}:${rest < 10 ? '0' : ''}${rest.toFixed(1)}`;
  }
  return `${s < 0.1 && s > 0 ? s.toFixed(3) : s.toFixed(2)} s`;
}

/** fmtNum trims trailing zeros: 25 → "25", 23.976 → "23.98". */
export function fmtNum(v: number, decimals = 2): string {
  if (!Number.isFinite(v)) return '—';
  return Number(v.toFixed(decimals)).toString();
}

/** GIF fps cap: delays are whole centiseconds and browsers clamp <= 10 ms to 100 ms, so 2 cs is the floor. */
export const GIF_MAX_FPS = 50;
/** fps cap for the other animated formats (WebP, APNG). */
export const MAX_FPS = 60;

/**
 * snapFPS mirrors graph.SnapFPS (DESIGN §4.1): GIF fps is capped at 50 and
 * otherwise left alone — ffmpeg's gif muxer runs at a 1/100 s timebase and
 * rounds each frame's pts, so e.g. 30 fps gets 3/3/4 cs delays with exact
 * total timing and no dropped or duplicated frames (there is no 100/n
 * snapping any more). Other animated formats are capped at 60. fps <= 0
 * returns 0.
 */
export function snapFPS(format: OutputFormat | string, fps: number): number {
  if (!(fps > 0)) return 0;
  return Math.min(fps, format === 'gif' ? GIF_MAX_FPS : MAX_FPS);
}

/**
 * gifDelays describes the centisecond frame delays a GIF gets at fps from
 * the 1/100 s muxer timebase: "4" for 25 fps, "3/4" for 30 fps (delays
 * alternate so the total timing stays exact). fps above the GIF cap is
 * capped first; fps <= 0 returns ''.
 */
export function gifDelays(fps: number): string {
  if (!(fps > 0)) return '';
  const d = 100 / Math.min(fps, GIF_MAX_FPS);
  // 0.01 cs tolerance: 33.33 fps is 3 cs, not "3/4".
  const lo = Math.floor(d + 0.01);
  const hi = Math.ceil(d - 0.01);
  return lo === hi ? `${lo}` : `${lo}/${hi}`;
}

/**
 * fitSize returns the frame size produced by scaling sw×sh into w×h with the
 * given fit ("contain" keeps aspect inside, "cover" scales to cover then
 * center-crops to w×h, "exact" stretches). 0 for w or h keeps the aspect.
 */
export function fitSize(sw: number, sh: number, w: number, h: number, fit: FitMode): { w: number; h: number } {
  if (sw <= 0 || sh <= 0) return { w: Math.max(0, w), h: Math.max(0, h) };
  if (w <= 0 && h <= 0) return { w: sw, h: sh };
  if (w > 0 && h <= 0) return { w, h: Math.max(1, Math.round((sh * w) / sw)) };
  if (h > 0 && w <= 0) return { w: Math.max(1, Math.round((sw * h) / sh)), h };
  if (fit === 'exact' || fit === 'cover') return { w, h };
  const s = Math.min(w / sw, h / sh);
  return { w: Math.max(1, Math.round(sw * s)), h: Math.max(1, Math.round(sh * s)) };
}

/** isHexColor accepts RRGGBB with or without a leading '#'. */
export function isHexColor(s: string): boolean {
  return /^#?[0-9a-fA-F]{6}$/.test(s.trim());
}

/** normalizeHex returns lowercase RRGGBB without '#', or null. */
export function normalizeHex(s: string): string | null {
  const t = s.trim().replace(/^#/, '').toLowerCase();
  return /^[0-9a-f]{6}$/.test(t) ? t : null;
}
