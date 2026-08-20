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
  type TrimParams,
} from './api';
import { GIF_MAX_FPS, round, snapFPS } from './format';
import { defaultOutput, fitsFormat, isSequence, presetAvailable, presetById, type OutputCfg } from './presets';

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
  /** scrubber position in output time (after trim and speed), seconds */
  scrubT: number;
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
  ui: { backdrop: 'checker', scrubT: 0, cropOpen: false } as UiState,
});

/**
 * setSource installs a freshly uploaded source and resets the op stack for
 * it. A preset the new source cannot use (Optimize needs a GIF) falls back to
 * Chat GIF.
 */
export function setSource(src: Source | null): void {
  app.source = src;
  app.ops = defaultOps(src?.info ?? null);
  app.ui.scrubT = 0;
  if (!presetAvailable(presetById(app.output.preset), src?.info ?? null)) applyPreset('chat-gif');
}

/** applyPreset switches the Output card to a preset (Custom keeps current values). */
export function applyPreset(id: PresetId): void {
  app.output.preset = id;
  presetById(id).apply(app.output, app.source?.info ?? null);
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
    const p: TrimParams = { start: round(Math.max(0, c.trim.start)) };
    if (c.trim.end > 0) p.end = round(c.trim.end);
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
 * the delay op on it is frames × ms (the op rewrites the timing); otherwise
 * the probe's duration.
 */
export function sourceDuration(info: ProbeInfo | null | undefined, c: OpsCfg): number {
  if (!info) return 0;
  if (isSequence(info) && c.delay.enabled && c.delay.ms > 0) {
    const n = info.sequence?.count ?? info.frames;
    if (n > 0) return round((n * c.delay.ms) / 1000, 3);
  }
  return Math.max(0, info.duration);
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

/** trimRange returns the selected source-time window [start, end] in seconds. */
export function trimRange(info: ProbeInfo, c: OpsCfg): { start: number; end: number } {
  const dur = sourceDuration(info, c);
  if (!c.trim.enabled) return { start: 0, end: dur };
  const start = Math.min(Math.max(0, c.trim.start), dur);
  const end = c.trim.end > 0 ? Math.min(Math.max(c.trim.end, start), dur) : dur;
  return { start, end };
}

export function speedFactor(c: OpsCfg): number {
  return c.speed.enabled && c.speed.factor > 0 ? c.speed.factor : 1;
}

/** previewDuration is the output-time length of the clip after trim and speed. */
export function previewDuration(info: ProbeInfo, c: OpsCfg): number {
  const { start, end } = trimRange(info, c);
  return Math.max(0, (end - start) / speedFactor(c));
}

/** toSourceTime maps a scrubber (output-time) position back to source seconds. */
export function toSourceTime(t: number, info: ProbeInfo, c: OpsCfg): number {
  const { start, end } = trimRange(info, c);
  return Math.min(end, start + t * speedFactor(c));
}
