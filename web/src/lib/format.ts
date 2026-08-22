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
 * FRAME_TOLERANCE is the slack, in frames, added before flooring
 * duration × fps (frameCount) and before deciding which frame a time
 * belongs to (frameAt, frameSpan). It mirrors graph.FrameTolerance
 * (internal/graph/compile.go) and must stay equal to it: trim bounds travel
 * at microsecond precision (trimTime), so a bound taken from the frame grid
 * lands up to 1 µs short of a whole number of frames — [2/30, 6/30) is sent
 * as 0.066667..0.2 and 0.133333 × 30 = 3.99999, which ffmpeg renders as 4
 * frames while a naive floor, or the old 1e-6 tolerance, counted 3. 1 µs at
 * the 60 fps cap is 6e-5 frames, hence 1e-4; a clip that is genuinely a
 * real sub-frame short of an integer by less than that cannot come out of
 * µs-precise times, so the tolerance only ever absorbs rounding.
 */
export const FRAME_TOLERANCE = 1e-4;

/**
 * trimTime rounds a trim bound to whole microseconds — ffmpeg's -ss/-to
 * resolution and what graph keeps of a recipe's trim params (round6 in
 * internal/graph). Never round a trim bound to milliseconds: 2/30 sent as
 * 0.067 makes ffmpeg's accurate seek drop frame 2 of a 30 fps source (its
 * pts 0.066667 precedes the seek point), and 5/30 sent as 0.167 lets frame 5
 * slip in. Hand-typed values keep whatever precision they have up to the µs.
 */
export function trimTime(v: number): number {
  return round(v, 6);
}

/**
 * frameCount is the number of frames a clip of `duration` seconds has at
 * `fps` (the plan's frame grid): max(1, floor(duration × fps +
 * FRAME_TOLERANCE)); 0 when the rate is unknown. Floor, not round: the
 * render's fps stage runs `round=down` so an fps drop never lengthens the
 * clip (graph.Plan.Frames uses the same model for every source that is not
 * an image sequence). The tolerance also absorbs the float error of a
 * duration that is exactly a whole number of frames — 34 frames at 33 ms is
 * 34 / 30.303 × 30.303 = 33.999…. Image sequences do not go through this at
 * all: state.planFrames counts their frames on the image2 grid exactly.
 */
export function frameCount(duration: number, fps: number): number {
  if (!(fps > 0) || !Number.isFinite(duration)) return 0;
  return Math.max(1, Math.floor(Math.max(0, duration) * fps + FRAME_TOLERANCE));
}

/**
 * frameAt maps a time to its 1-based frame number on the fps grid, clamped
 * to [1, count] (count ≤ 0 = unclamped). FRAME_TOLERANCE absorbs the
 * floating-point and µs-rounding error of k/fps so the frame boundary itself
 * belongs to frame k+1, not k.
 */
export function frameAt(t: number, fps: number, count = 0): number {
  if (!(fps > 0)) return 1;
  let f = Math.floor(Math.max(0, t) * fps + FRAME_TOLERANCE) + 1;
  if (count > 0) f = Math.min(f, count);
  return Math.max(1, f);
}

/** frameStart is the start time (seconds) of 0-based frame i on the fps grid. */
export function frameStart(i: number, fps: number): number {
  if (!(fps > 0)) return 0;
  return Math.max(0, i) / fps;
}

/**
 * stillTime is the time the preview requests for 0-based frame i: the
 * middle of the frame, (i + 0.5) / fps, rounded to whole milliseconds the
 * way jobs.Still keys its memo. The server maps t → floor(t × fps), so a
 * mid-frame time lands on frame i whatever the float error of either side
 * (a frame-start time would be one ulp away from the previous frame).
 */
export function stillTime(i: number, fps: number): number {
  if (!(fps > 0)) return 0;
  return round((Math.max(0, i) + 0.5) / fps, 3);
}

/**
 * frameSpan is the 1-based inclusive frame range [first, last] the window
 * [start, end) seconds covers on the fps grid, clamped to [1, count] when
 * count > 0 (end ≤ start yields a single frame). Tolerances as in frameAt,
 * so [i/fps, (i+1)/fps) is exactly frame i+1 — also when the bounds are the
 * µs-rounded trimTime values a recipe carries (0.033333..0.133333 at 30 fps
 * is frames 2–4, matching graph's count of 3). For image sequences the
 * graph snaps the bounds to the nearest grid frame instead: use
 * state.sourceSpan, which goes through sequenceSelection for them.
 */
export function frameSpan(start: number, end: number, fps: number, count = 0): { first: number; last: number } {
  if (!(fps > 0)) return { first: 1, last: 1 };
  let first = Math.floor(Math.max(0, start) * fps + FRAME_TOLERANCE) + 1;
  let last = Math.max(first, Math.ceil(Math.max(0, end) * fps - FRAME_TOLERANCE));
  if (count > 0) {
    first = Math.min(first, count);
    last = Math.min(last, count);
  }
  return { first: Math.max(1, first), last: Math.max(1, last) };
}

/**
 * fmtLimit renders a Discord byte cap: the attachment tiers are decimal
 * megabytes ("20 MB"), the emote / sticker caps whole KiB ("256 KiB").
 */
export function fmtLimit(bytes: number): string {
  if (!(bytes > 0)) return 'none';
  if (bytes % 1_000_000 === 0) return `${bytes / 1_000_000} MB`;
  const k = bytes / 1024;
  return `${Number.isInteger(k) ? k : fmtKiB(bytes)} KiB`;
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
 * snapping any more). Other animated formats are capped at 60. The result
 * is rounded to 3 decimals like graph.SnapFPS (the precision of the fps
 * filter text and of the recipe's fps fields): a 30000/1001 source plans
 * 29.97 fps, not 29.97002997…, and the frame count follows that rate.
 * fps <= 0 returns 0.
 */
export function snapFPS(format: OutputFormat | string, fps: number): number {
  if (!(fps > 0)) return 0;
  return round(Math.min(fps, format === 'gif' ? GIF_MAX_FPS : MAX_FPS), 3);
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
