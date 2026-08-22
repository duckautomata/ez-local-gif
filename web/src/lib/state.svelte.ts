// Application state (Svelte 5 runes) plus the pure functions that turn the
// editable configuration into the wire types of internal/recipe.

import {
  isAnimatedFormat,
  type CropParams,
  type DelayParams,
  type FitMode,
  type FlipParams,
  type FPSParams,
  type Op,
  type Output,
  type PresetId,
  type ProbeInfo,
  type ResizeParams,
  type RotateParams,
  type Source,
  type SpeedParams,
  type Target,
  type TrimParams,
} from './api';
import { clamp, frameCount, frameSpan, GIF_MAX_FPS, round, snapFPS, trimTime } from './format';
import { defaultOutput, fitsFormat, isSequence, limitKiB, presetAvailable, presetById, type OutputCfg } from './presets';

// isSequence lives in presets.ts (isGifSource needs it there); re-exported so
// components keep importing it from the state module.
export { isSequence };

export type Backdrop = 'checker' | 'dark' | 'white';

export interface TrimCfg {
  enabled: boolean;
  start: number; // seconds, source time
  end: number; // seconds, source time; 0 = to the end
}
export interface CropCfg {
  enabled: boolean;
  x: number;
  y: number;
  w: number;
  h: number;
}
export interface ResizeCfg {
  enabled: boolean;
  width: number; // 0 = keep aspect from height
  height: number;
  fit: FitMode;
}
export interface FpsCfg {
  enabled: boolean;
  fps: number;
}
export interface SpeedCfg {
  enabled: boolean;
  factor: number;
}
export interface FlipRotateCfg {
  enabled: boolean;
  horizontal: boolean;
  vertical: boolean;
  degrees: 0 | 90 | 180 | 270;
}
/** DelayCfg: per-frame duration of an image-sequence source (the "delay" op). */
export interface DelayCfg {
  enabled: boolean;
  ms: number;
}

/** OpsCfg is the editable form of the op stack; buildOps() serialises it. */
export interface OpsCfg {
  unpremultiply: boolean;
  delay: DelayCfg;
  trim: TrimCfg;
  crop: CropCfg;
  resize: ResizeCfg;
  fps: FpsCfg;
  speed: SpeedCfg;
  flipRotate: FlipRotateCfg;
}

export interface UiState {
  backdrop: Backdrop;
  /**
   * Scrubber position as a 0-based frame index on the plan's frame grid
   * (planFrames / planFPS — output time after trim and speed). The preview
   * requests the still at the middle of that frame (format.stillTime) and
   * displays its start time; Trim "from scrubber" maps it back to source
   * seconds. Components clamp it to [0, planFrames − 1].
   */
  scrubFrame: number;
  /** true while the Crop card is expanded: the preview shows the full pre-crop frame */
  cropOpen: boolean;
}

/** DEFAULT_DELAY_MS is the sequence frame delay the server assumes when the client sends none. */
export const DEFAULT_DELAY_MS = 100;

export function defaultOps(info?: ProbeInfo | null): OpsCfg {
  const w = info?.width ?? 0;
  const h = info?.height ?? 0;
  const srcFps = info?.fps && info.fps > 0 ? round(info.fps, 2) : 25;
  const delayMs = info?.sequence?.delayMs && info.sequence.delayMs > 0 ? info.sequence.delayMs : DEFAULT_DELAY_MS;
  return {
    unpremultiply: info?.premultiplied ?? false,
    delay: { enabled: false, ms: delayMs },
    trim: { enabled: false, start: 0, end: 0 },
    crop: { enabled: false, x: 0, y: 0, w, h },
    resize: { enabled: false, width: 0, height: 0, fit: 'contain' },
    fps: { enabled: false, fps: Math.min(srcFps, GIF_MAX_FPS) },
    speed: { enabled: false, factor: 1 },
    flipRotate: { enabled: false, horizontal: false, vertical: false, degrees: 0 },
  };
}

export const app = $state({
  source: null as Source | null,
  ops: defaultOps(null),
  output: defaultOutput(),
  ui: { backdrop: 'checker', scrubFrame: 0, cropOpen: false } as UiState,
});

/**
 * setSource installs a freshly uploaded source and resets the op stack for
 * it. A preset the new source cannot use (Optimize needs a GIF) falls back to
 * Chat.
 */
export function setSource(src: Source | null): void {
  app.source = src;
  app.ops = defaultOps(src?.info ?? null);
  app.ui.scrubFrame = 0;
  app.ui.cropOpen = false;
  if (!presetAvailable(presetById(app.output.preset), src?.info ?? null)) applyPreset('chat');
}

/**
 * resetApp returns to the landing state (the header logo): no source,
 * default op stack and Output card, scrubber at frame 0. The render state
 * (job / result) lives in render.svelte.ts and is reset by the caller.
 */
export function resetApp(): void {
  app.source = null;
  app.ops = defaultOps(null);
  app.output = defaultOutput();
  app.ui.scrubFrame = 0;
  app.ui.cropOpen = false;
}

/** applyPreset switches the Output card to a preset (Custom keeps current values). */
export function applyPreset(id: PresetId): void {
  app.output.preset = id;
  presetById(id).apply(app.output, app.source?.info ?? null);
}

/**
 * setTarget changes the Discord target of an output configuration. A fit
 * budget that was sitting exactly on the old target's cap ("= limit")
 * follows the new cap, so the byte-limit readout and the fit stay in step
 * with the dropdown; any other budget is the user's and is left alone.
 */
export function setTarget(o: OutputCfg, t: Target): void {
  const prev = limitKiB(o.target);
  o.target = t;
  const next = limitKiB(t);
  if (next > 0 && prev > 0 && o.fitKiB === prev) o.fitKiB = next;
}

/** opsApply reports whether the op stack is part of the recipe (false for the gifsicle-only Optimize preset). */
export function opsApply(out: Pick<OutputCfg, 'preset'>): boolean {
  return presetById(out.preset).usesOps;
}

/**
 * buildOps serialises the op configuration in the documented order:
 * unpremultiply, delay, trim, speed, fps, crop, resize, canvas, flip, rotate.
 * With cropPreview the stack stops before crop, so the still shows the full
 * frame in source pixel coordinates for the drag rectangle.
 */
export function buildOps(c: OpsCfg, opts: { cropPreview?: boolean } = {}): Op[] {
  const ops: Op[] = [];
  if (c.unpremultiply) ops.push({ kind: 'unpremultiply' });
  if (c.delay.enabled && c.delay.ms > 0) {
    const p: DelayParams = { ms: Math.min(60000, Math.max(1, Math.round(c.delay.ms))) };
    ops.push({ kind: 'delay', params: p });
  }

  if (c.trim.enabled && (c.trim.start > 0 || c.trim.end > 0)) {
    // Microsecond precision (trimTime), never milliseconds: the graph writes
    // -ss/-to with µs, and a scrubber start of 2/30 sent as 0.067 would make
    // a 30 fps clip start one frame late.
    const p: TrimParams = { start: trimTime(Math.max(0, c.trim.start)) };
    if (c.trim.end > 0) p.end = trimTime(c.trim.end);
    ops.push({ kind: 'trim', params: p });
  }
  if (c.speed.enabled && c.speed.factor > 0 && c.speed.factor !== 1) {
    const p: SpeedParams = { factor: round(c.speed.factor) };
    ops.push({ kind: 'speed', params: p });
  }
  if (c.fps.enabled && c.fps.fps > 0) {
    const p: FPSParams = { fps: round(c.fps.fps) };
    ops.push({ kind: 'fps', params: p });
  }
  if (opts.cropPreview) return ops;

  if (c.crop.enabled && c.crop.w > 0 && c.crop.h > 0) {
    const p: CropParams = {
      x: Math.round(c.crop.x),
      y: Math.round(c.crop.y),
      w: Math.round(c.crop.w),
      h: Math.round(c.crop.h),
    };
    ops.push({ kind: 'crop', params: p });
  }
  if (c.resize.enabled && (c.resize.width > 0 || c.resize.height > 0)) {
    const p: ResizeParams = { fit: c.resize.fit };
    if (c.resize.width > 0) p.width = Math.round(c.resize.width);
    if (c.resize.height > 0) p.height = Math.round(c.resize.height);
    ops.push({ kind: 'resize', params: p });
  }
  // (canvas op: no card yet; Output.width/height/fit covers padding)
  if (c.flipRotate.enabled) {
    if (c.flipRotate.horizontal || c.flipRotate.vertical) {
      const p: FlipParams = {};
      if (c.flipRotate.horizontal) p.horizontal = true;
      if (c.flipRotate.vertical) p.vertical = true;
      ops.push({ kind: 'flip', params: p });
    }
    if (c.flipRotate.degrees === 90 || c.flipRotate.degrees === 180 || c.flipRotate.degrees === 270) {
      const p: RotateParams = { degrees: c.flipRotate.degrees };
      ops.push({ kind: 'rotate', params: p });
    }
  }
  return ops;
}

/** NO_OPS is an all-off stack: what the Optimize preset renders and previews with. */
const NO_OPS: OpsCfg = defaultOps(null);

/**
 * effectiveOps is the op configuration the recipe actually carries: the
 * edited stack, or an all-off one for the gifsicle-only Optimize preset (it
 * edits the GIF bytes directly, DESIGN §4.2 "GIF → GIF", so trim/crop/…
 * do not apply and the preview shows the source as-is).
 */
export function effectiveOps(c: OpsCfg, out: Pick<OutputCfg, 'preset'>): OpsCfg {
  return opsApply(out) ? c : NO_OPS;
}

/** recipeOps serialises effectiveOps (empty for Optimize). */
export function recipeOps(c: OpsCfg, out: Pick<OutputCfg, 'preset'>, opts: { cropPreview?: boolean } = {}): Op[] {
  return buildOps(effectiveOps(c, out), opts);
}

/**
 * loopFor returns the loop count actually requested for a configuration:
 * every Discord target requires loop forever (0), so the user's count only
 * counts with no target. Values are GIF NETSCAPE semantics (0 = forever,
 * N > 0 = play N+1 times).
 */
export function loopFor(c: Pick<OutputCfg, 'target' | 'loop'>): number {
  if (c.target) return 0;
  return c.loop > 0 ? Math.round(c.loop) : 0;
}

/**
 * fitBytesFor returns the fit budget in bytes (0 = fit off). Only fit-capable
 * formats (presets.FIT_FORMATS) carry one: the server has no fit ladder for
 * static PNG or frame extraction and would ignore fitBytes, so the recipe
 * must not carry it (it would only pollute the recipe hash and make the
 * Result card claim a fit ran).
 */
export function fitBytesFor(c: Pick<OutputCfg, 'format' | 'fitEnabled' | 'fitKiB'>): number {
  if (!fitsFormat(c.format) || !c.fitEnabled || !(c.fitKiB > 0)) return 0;
  return Math.round(c.fitKiB * 1024);
}

/** usesMatte: formats flattened onto / thresholded against the matte colour. */
export function usesMatte(c: Pick<OutputCfg, 'format' | 'frameFormat'>): boolean {
  return c.format === 'gif' || c.format === 'jpeg' || (c.format === 'frames' && c.frameFormat === 'jpeg');
}

/**
 * buildOutput serialises the Output card into recipe.Output (format-specific
 * knobs only). `loop` is emitted only for a non-zero count with no Discord
 * target on an animated format; 0 (= loop forever, the recipe zero value) is
 * left out, and Discord targets always get 0 (DESIGN §5.3; discordlint
 * requires loop forever for every Discord target). Fit fields are emitted
 * only when a budget is set.
 */
export function buildOutput(c: OutputCfg): Output {
  const o: Output = { format: c.format };
  if (c.width > 0) o.width = Math.round(c.width);
  if (c.height > 0) o.height = Math.round(c.height);
  if (c.width > 0 || c.height > 0) o.fit = c.fit;
  if (c.fps > 0) o.fps = round(c.fps);
  switch (c.format) {
    case 'gif':
      o.colors = c.colors;
      o.dither = c.dither;
      if (c.lossy > 0) o.lossy = Math.round(c.lossy);
      o.alphaThreshold = c.alphaThreshold;
      o.matte = c.matte;
      break;
    case 'apng':
      if (c.colors > 0) o.colors = c.colors; // 0 = RGBA truecolour
      break;
    case 'webp':
      if (c.lossless) o.lossless = true;
      else o.quality = c.quality;
      break;
    case 'avif':
      o.quality = c.quality;
      break;
    case 'jpeg':
      o.quality = c.quality;
      o.matte = c.matte;
      break;
    case 'png':
      if (c.colors > 0) o.colors = c.colors; // pngquant palette; 0 = full colour (oxipng only)
      break;
    case 'frames':
      o.frameFormat = c.frameFormat;
      if (c.frameFormat === 'jpeg') {
        o.quality = c.quality;
        o.matte = c.matte;
      }
      break;
  }
  if (isAnimatedFormat(c.format)) {
    const loop = loopFor(c);
    if (loop > 0) o.loop = loop;
  }
  const fitBytes = fitBytesFor(c);
  if (fitBytes > 0) {
    o.fitBytes = fitBytes;
    if (c.fitKeepSize) o.fitKeepSize = true;
    if (c.fitKeepFps) o.fitKeepFps = true;
  }
  o.preset = c.preset;
  if (c.target && c.format !== 'frames') o.target = c.target; // frame extraction has no Discord target
  return o;
}

/**
 * sourceFPS is the source frame rate the graph sees: the probe's rate, or
 * for an image sequence 1000 / the delay op's ms when that op is on
 * (otherwise the sequence's own delay, which the probe already turned into
 * fps). 0 when unknown.
 */
export function sourceFPS(info: ProbeInfo | null | undefined, c: OpsCfg): number {
  if (!info) return 0;
  if (isSequence(info) && c.delay.enabled && c.delay.ms > 0) return round(1000 / c.delay.ms, 3);
  return info.fps > 0 ? info.fps : 0;
}

/**
 * sourceDuration is the source length in seconds: for an image sequence with
 * the delay op on it is count / rate (the op rewrites the timing; the
 * compiler computes it the same way from the 3-decimal rate, so the two
 * sides floor to the same frame count); otherwise the probe's duration.
 */
export function sourceDuration(info: ProbeInfo | null | undefined, c: OpsCfg): number {
  if (!info) return 0;
  if (isSequence(info) && c.delay.enabled && c.delay.ms > 0) {
    const n = info.sequence?.count ?? info.frames;
    const rate = sourceFPS(info, c);
    if (n > 0 && rate > 0) return n / rate;
  }
  return Math.max(0, info.duration);
}

/**
 * sourceFrames is the number of frames on the *source* grid: a sequence's
 * frame count, else floor(duration × source fps) (0 when unknown). Used to
 * label trim points as frames.
 */
export function sourceFrames(info: ProbeInfo | null | undefined, c: OpsCfg): number {
  if (!info) return 0;
  if (isSequence(info)) {
    const n = info.sequence?.count ?? info.frames;
    if (n > 0) return n;
  }
  if (info.isStill) return 1;
  return frameCount(sourceDuration(info, c), sourceFPS(info, c));
}

/**
 * effectiveFPS mirrors graph.Compile's precedence: an enabled fps op wins
 * over Output.fps, which wins over the source rate; the result is snapped for
 * the output format (snapFPS). 0 when nothing is known. The op stack is
 * ignored for presets that do not use it (Optimize).
 */
export function effectiveFPS(ops: OpsCfg, out: OutputCfg, srcFps: number): number {
  const opFps = opsApply(out) && ops.fps.enabled && ops.fps.fps > 0 ? ops.fps.fps : 0;
  const requested = opFps > 0 ? opFps : out.fps > 0 ? out.fps : srcFps;
  return snapFPS(out.format, requested);
}

/**
 * trimRange returns the selected source-time window [start, end] in seconds,
 * with the bounds as the recipe carries them (trimTime: whole µs) and
 * clamped to the source: an end at or past the source end is the source end
 * (the graph reads "to the end" then).
 */
export function trimRange(info: ProbeInfo, c: OpsCfg): { start: number; end: number } {
  const dur = sourceDuration(info, c);
  if (!c.trim.enabled) return { start: 0, end: dur };
  const start = Math.min(Math.max(0, trimTime(c.trim.start)), dur);
  const end = c.trim.end > 0 ? Math.min(Math.max(trimTime(c.trim.end), start), dur) : dur;
  return { start, end };
}

/** speedFactor is the speed the recipe carries (buildOps sends 3 decimals): 1 when the op is off. */
export function speedFactor(c: OpsCfg): number {
  const f = c.speed.enabled && c.speed.factor > 0 ? round(c.speed.factor, 3) : 1;
  return f > 0 ? f : 1;
}

// ---------------------------------------------------------------------------
// Image-sequence frame grid — mirrors graph.sequenceSelection /
// graph.sequenceFrames (internal/graph/compile.go). ffmpeg reads a sequence
// through image2 at -framerate 1000/delay, whose timebase is one frame, so
// the trim bounds and the retiming stages work in whole frames:
//   - -ss/-to are rescaled into that timebase with av_rescale (nearest,
//     halves away from zero): the first frame read is round(start × rate) and
//     round((end − start) × rate) frames are kept;
//   - setpts=PTS/speed truncates the end timestamp to a whole tick;
//   - the fps stage (round=down) floors the end onto the output grid.
// Every expectation is pinned against ffmpeg in graph's TestSequenceGridModel
// and phase2_ffmpeg_test.go.
// ---------------------------------------------------------------------------

/** micros returns t seconds as whole microseconds (exact for trimTime values). */
export function micros(t: number): number {
  return Math.round(t * 1e6);
}

/**
 * gridRound converts a time in microseconds to a frame number at
 * rate1000/1000 fps the way ffmpeg's av_rescale does: nearest, halves away
 * from zero; <= 0 is frame 0. Integer arithmetic (BigInt) so it is exact for
 * every sequence the store accepts, like graph.gridRound's int64 math.
 */
export function gridRound(us: number, rate1000: number): number {
  if (!(us > 0) || !(rate1000 > 0)) return 0;
  const u = BigInt(Math.round(us));
  const r = BigInt(Math.round(rate1000));
  return Number((2n * u * r + 1_000_000_000n) / 2_000_000_000n);
}

/**
 * sequenceSelection maps a trim [start, end) (end 0 = to the end) onto the
 * image2 grid of a `count`-frame sequence at `rate` fps: `first` is the
 * 0-based source frame the render starts at and `selected` how many it
 * reads. `selected` is 0 where the graph rejects the trim: a start that
 * rounds past the last frame, or a range shorter than half a frame.
 */
export function sequenceSelection(count: number, rate: number, start: number, end: number): { first: number; selected: number } {
  if (!(count > 0)) return { first: 0, selected: 0 };
  const rate1000 = Math.round(rate * 1000);
  if (!(rate1000 > 0)) return { first: 0, selected: count };
  const first = gridRound(micros(start), rate1000);
  if (first >= count) return { first, selected: 0 };
  let selected = count - first;
  if (end > 0) selected = Math.min(selected, gridRound(micros(end) - micros(start), rate1000));
  return { first, selected };
}

/**
 * sequenceFrames is the number of master frames ffmpeg renders for n selected
 * sequence frames at `rate` fps, played at `speed` and resampled to `fps`:
 * floor(trunc(n / speed) × fps / rate) on the 3-decimal rates as integers —
 * 7 frames at speed 2 end at tick 3, not 3.5, and resampled to 20 fps that
 * is 6 frames, not the 7 of floor(0.35 s × 20). n itself without trim, speed
 * or an fps change. 0 means ffmpeg emits nothing (the graph rejects it).
 */
export function sequenceFrames(n: number, speed: number, rate: number, fps: number): number {
  if (!(n > 0)) return 0;
  const ticks = Math.trunc(n / speed);
  const rate1000 = Math.round(rate * 1000);
  const fps1000 = Math.round(fps * 1000);
  if (!(ticks > 0) || !(rate1000 > 0) || !(fps1000 > 0)) return 0;
  return Math.floor((ticks * fps1000) / rate1000);
}

/**
 * sequenceGrid resolves an image-sequence source the way the compiler does:
 * its frame count, image2 rate (sourceFPS: the delay op, else the probe)
 * and the frames the current trim selects on that grid. null for every
 * other source, or when the count / rate is unknown.
 */
function sequenceGrid(info: ProbeInfo, c: OpsCfg): { count: number; rate: number; first: number; selected: number } | null {
  if (!isSequence(info)) return null;
  const count = info.sequence?.count ?? info.frames;
  const rate = sourceFPS(info, c);
  if (!(count > 0) || !(rate > 0)) return null;
  if (count === 1) return { count, rate, first: 0, selected: 1 }; // a single frame is never trimmed (graph.singleFrame)
  const range = trimRange(info, c);
  const dur = count / rate;
  return { count, rate, ...sequenceSelection(count, rate, range.start, range.end >= dur ? 0 : range.end) };
}

/**
 * previewDuration is the output-time length of the clip after trim and speed
 * (graph.Plan.Duration): for an image sequence the selected grid frames at
 * their rate, else the trimmed source time; both divided by the speed.
 */
export function previewDuration(info: ProbeInfo, c: OpsCfg): number {
  const seq = sequenceGrid(info, c);
  if (seq) return seq.selected / seq.rate / speedFactor(c);
  const { start, end } = trimRange(info, c);
  return Math.max(0, (end - start) / speedFactor(c));
}

/**
 * sourceSpan is the selection as 1-based inclusive source frames
 * [first, last] (of sourceFrames) — the numbers the Trim card labels with.
 * For an image sequence it is exactly what the graph's trim selects on the
 * image2 grid (sequenceSelection: nearest frame, so a typed 0.06..0.11 s at
 * 25 fps is frame 3 alone, as ffmpeg renders it; a rejected trim shows the
 * frame its start lands on). For a clip it is the frames the window covers
 * on the source grid (format.frameSpan).
 */
export function sourceSpan(info: ProbeInfo, c: OpsCfg): { first: number; last: number } {
  const seq = sequenceGrid(info, c);
  if (seq) {
    const first = Math.min(seq.first, seq.count - 1) + 1;
    return { first, last: Math.max(first, seq.first + seq.selected) };
  }
  const range = trimRange(info, c);
  return frameSpan(range.start, range.end, sourceFPS(info, c), sourceFrames(info, c));
}

/** toSourceTime maps a scrubber (output-time) position back to source seconds. */
export function toSourceTime(t: number, info: ProbeInfo, c: OpsCfg): number {
  const { start, end } = trimRange(info, c);
  return Math.min(end, start + t * speedFactor(c));
}

/**
 * planFPS is the plan's frame grid: the effective output fps (fps op →
 * Output.fps → source rate, snapped for the format). 0 when unknown. `c`
 * should be effectiveOps (all-off for Optimize).
 */
export function planFPS(info: ProbeInfo | null | undefined, c: OpsCfg, out: OutputCfg): number {
  if (!info) return 0;
  return effectiveFPS(c, out, sourceFPS(info, c));
}

/**
 * planFrames mirrors graph.Plan.Frames — the number of frames the render
 * (and so the scrubber) has, at least 1:
 *
 *   1. an image sequence with no trim, speed or fps change has exactly its
 *      frame count (34 frames at 33 ms are 34, whatever 34 / 30.303 × 30.303
 *      comes to in floating point);
 *   2. a trimmed sequence has the frames the trim selects on the image2 grid
 *      (sequenceSelection: nearest frame, end 0 = to the end);
 *   3. a sequence played at a speed and/or resampled to another fps has
 *      sequenceFrames(selected, speed, rate, fps) of them — the end
 *      timestamp is truncated by setpts and floored by the fps stage;
 *   4. every other clip has floor(duration × fps + FRAME_TOLERANCE)
 *      (format.frameCount).
 *
 * A still source has one frame; 0 when the rate is unknown. Where the graph
 * would reject the recipe (a sequence trim or speed that leaves no frame)
 * the scrubber keeps one notch, like rule 4's floor; the preview shows the
 * graph's error. `c` should be effectiveOps (all-off for Optimize).
 */
export function planFrames(info: ProbeInfo | null | undefined, c: OpsCfg, out: OutputCfg): number {
  if (!info) return 0;
  const fps = planFPS(info, c, out);
  if (!(fps > 0)) return 0;
  if (info.isStill) return 1;
  const seq = sequenceGrid(info, c);
  if (seq) {
    if (seq.count === 1) return 1;
    return Math.max(1, sequenceFrames(seq.selected, speedFactor(c), seq.rate, fps));
  }
  const dur = previewDuration(info, c);
  if (!(dur > 0)) return 1;
  return frameCount(dur, fps);
}

/**
 * trimStartMax is the latest trim start the graph accepts: one source frame
 * before the end of the clip ("trim start at or beyond the end of the
 * source" is rejected), at the µs precision trim bounds carry (trimTime) so
 * the start of the last plan frame is never displaced by the clamp.
 */
export function trimStartMax(info: ProbeInfo, c: OpsCfg): number {
  const dur = sourceDuration(info, c);
  const srcFps = sourceFPS(info, c);
  return Math.max(0, trimTime(dur - (srcFps > 0 ? 1 / srcFps : 0.001)));
}

/**
 * frameWindow is the source-time window [start, end] of 0-based plan frame
 * i — what Trim "from scrubber" uses: Start = where the frame starts, End =
 * where it ends. Both go through the current trim/speed (toSourceTime) and
 * are rounded to whole µs like buildOps sends them (trimTime — never ms: a
 * ms-rounded 0.067 starts a 30 fps clip one frame late); start is clamped
 * to trimStartMax so the graph never sees a start at or beyond the end. On
 * the last frame the window ends exactly at the current range end, and an
 * end at the clip end is reported as 0 ("to the end").
 */
export function frameWindow(info: ProbeInfo, c: OpsCfg, out: OutputCfg, i: number): { start: number; end: number } {
  const ops = effectiveOps(c, out);
  const fps = planFPS(info, ops, out);
  const total = planFrames(info, ops, out);
  if (!(fps > 0) || total <= 0) return { start: 0, end: 0 };
  const idx = clamp(Math.round(i), 0, total - 1);
  const dur = sourceDuration(info, c);
  const range = trimRange(info, c);
  const start = Math.min(trimTime(toSourceTime(idx / fps, info, c)), trimStartMax(info, c));
  let end = idx >= total - 1 ? range.end : Math.min(trimTime(toSourceTime((idx + 1) / fps, info, c)), dur);
  if (end >= dur) end = 0;
  return { start, end };
}
