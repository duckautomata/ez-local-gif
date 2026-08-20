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

/**
 * fmtTimecode renders seconds Resolve-style as MM:SS.cc (centiseconds):
 * 1.12 → "00:01.12", 72.5 → "01:12.50"; an hour or more adds H:.
 * Negative / non-finite values render as 00:00.00.
 */
export function fmtTimecode(s: number): string {
  if (!Number.isFinite(s) || s < 0) s = 0;
  // Work in whole centiseconds so 1.005 does not render as 00:01.00 + carry bugs.
  let cs = Math.round(s * 100);
  const h = Math.floor(cs / 360_000);
  cs -= h * 360_000;
  const m = Math.floor(cs / 6000);
  cs -= m * 6000;
  const sec = Math.floor(cs / 100);
  cs -= sec * 100;
  const pad = (v: number) => (v < 10 ? '0' : '') + v;
  const core = `${pad(m)}:${pad(sec)}.${pad(cs)}`;
  return h > 0 ? `${h}:${core}` : core;
}

/**
 * frameCount is the number of frames a clip of `duration` seconds has at
 * `fps` (the plan's frame grid): max(1, floor(duration × fps)); 0 when the
 * rate is unknown. Floor, not round: the render's fps stage runs
 * `round=down` so an fps drop never lengthens the clip (graph.Plan.Frames
 * uses the same model).
 */
export function frameCount(duration: number, fps: number): number {
  if (!(fps > 0) || !Number.isFinite(duration)) return 0;
  return Math.max(1, Math.floor(Math.max(0, duration) * fps + 1e-9));
}

/**
 * frameAt maps a time to its 1-based frame number on the fps grid, clamped
 * to [1, count] (count ≤ 0 = unclamped). A small epsilon absorbs the
 * floating-point error of k/fps so the frame boundary itself belongs to
 * frame k+1, not k.
 */
export function frameAt(t: number, fps: number, count = 0): number {
  if (!(fps > 0)) return 1;
  let f = Math.floor(Math.max(0, t) * fps + 1e-6) + 1;
  if (count > 0) f = Math.min(f, count);
  return Math.max(1, f);
}

/**
 * ceilMs rounds a time up to whole milliseconds (with a tiny tolerance so an
 * exact millisecond stays put). Still requests are keyed on milliseconds
 * server-side and map t to frame floor(t × fps), so rounding *up* keeps a
 * frame-start time inside its own frame (rounding down would show the
 * previous frame at 23.976 fps, where 2/23.976 = 0.08342 → 0.083 → frame 2).
 */
export function ceilMs(t: number): number {
  if (!Number.isFinite(t) || t <= 0) return 0;
  return Math.ceil(t * 1000 - 1e-6) / 1000;
}

/** frameTime is the start time of 1-based frame f on the fps grid, ceiled to ms (see ceilMs). */
export function frameTime(f: number, fps: number): number {
  if (!(fps > 0)) return 0;
  return ceilMs(Math.max(0, f - 1) / fps);
}

/**
 * dropRates lists the frame rates the gifsicle-only optimiser can reach from
 * a source at srcFps: it drops every Nth frame (N = 2..4, merging the dropped
 * delay into the previous frame), so the rate becomes srcFps × (N−1)/N —
 * 1/2, 2/3 or 3/4 of the source — which jobs/optimize.go (dropEveryN) maps
 * back onto N within 5 %. Ordered mildest first: keep all (n 0), every 4th,
 * every 3rd, every 2nd. `fps` is what the recipe carries (0 = keep all).
 * Empty for no rate.
 */
export function dropRates(srcFps: number): { n: number; fps: number; label: string }[] {
  if (!(srcFps > 0)) return [];
  const out = [{ n: 0, fps: 0, label: `keep all frames (${fmtNum(srcFps)} fps)` }];
  for (const n of [4, 3, 2]) {
    const fps = round((srcFps * (n - 1)) / n, 3);
    out.push({ n, fps, label: `drop every ${ordinal(n)} frame (${fmtNum(fps)} fps)` });
  }
  return out;
}

function ordinal(n: number): string {
  return n === 2 ? '2nd' : n === 3 ? '3rd' : `${n}th`;
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
