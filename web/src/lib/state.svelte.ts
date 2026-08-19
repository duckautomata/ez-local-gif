// Application state (Svelte 5 runes) plus the pure functions that turn the
// editable configuration into the wire types of internal/recipe.

import type {
  CropParams,
  FitMode,
  FlipParams,
  FPSParams,
  Op,
  Output,
  PresetId,
  ProbeInfo,
  ResizeParams,
  RotateParams,
  Source,
  SpeedParams,
  TrimParams,
} from './api';
import { GIF_MAX_FPS, round, snapFPS } from './format';
import { defaultOutput, presetById, type OutputCfg } from './presets';

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

/** OpsCfg is the editable form of the op stack; buildOps() serialises it. */
export interface OpsCfg {
  unpremultiply: boolean;
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

export function defaultOps(info?: ProbeInfo | null): OpsCfg {
  const w = info?.width ?? 0;
  const h = info?.height ?? 0;
  const srcFps = info?.fps && info.fps > 0 ? round(info.fps, 2) : 25;
  return {
    unpremultiply: info?.premultiplied ?? false,
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

/** setSource installs a freshly uploaded source and resets the op stack for it. */
export function setSource(src: Source | null): void {
  app.source = src;
  app.ops = defaultOps(src?.info ?? null);
  app.ui.scrubT = 0;
}

/** applyPreset switches the Output card to a preset (Custom keeps current values). */
export function applyPreset(id: PresetId): void {
  app.output.preset = id;
  presetById(id).apply(app.output);
}

/**
 * buildOps serialises the op configuration in the documented order:
 * unpremultiply, trim, speed, fps, crop, resize, canvas, flip, rotate.
 * With cropPreview the stack stops before crop, so the still shows the full
 * frame in source pixel coordinates for the drag rectangle.
 */
export function buildOps(c: OpsCfg, opts: { cropPreview?: boolean } = {}): Op[] {
  const ops: Op[] = [];
  if (c.unpremultiply) ops.push({ kind: 'unpremultiply' });

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
  // (canvas op: no card in Phase 1; Output.width/height/fit covers padding)
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
 * buildOutput serialises the Output card into recipe.Output (format-specific
 * knobs only). `loop` is emitted only for a non-zero count with no Discord
 * target; 0 (= loop forever, the recipe zero value) is left out, and Discord
 * targets always get 0 (DESIGN §5.3; discordlint requires loop forever for
 * every Discord target).
 */
export function buildOutput(c: OutputCfg): Output {
  const o: Output = { format: c.format };
  if (c.width > 0) o.width = Math.round(c.width);
  if (c.height > 0) o.height = Math.round(c.height);
  if (c.width > 0 || c.height > 0) o.fit = c.fit;
  if (c.fps > 0) o.fps = round(c.fps);
  if (c.format === 'gif') {
    o.colors = c.colors;
    o.dither = c.dither;
    if (c.lossy > 0) o.lossy = Math.round(c.lossy);
    o.alphaThreshold = c.alphaThreshold;
    o.matte = c.matte;
  } else {
    if (c.lossless) o.lossless = true;
    else o.quality = c.quality;
  }
  const loop = loopFor(c);
  if (loop > 0) o.loop = loop;
  o.preset = c.preset;
  if (c.target) o.target = c.target;
  return o;
}

/**
 * effectiveFPS mirrors graph.Compile's precedence: an enabled fps op wins
 * over Output.fps, which wins over the source rate; the result is snapped for
 * the output format (snapFPS). 0 when nothing is known.
 */
export function effectiveFPS(ops: OpsCfg, out: OutputCfg, srcFps: number): number {
  const requested = ops.fps.enabled && ops.fps.fps > 0 ? ops.fps.fps : out.fps > 0 ? out.fps : srcFps;
  return snapFPS(out.format, requested);
}

/** trimRange returns the selected source-time window [start, end] in seconds. */
export function trimRange(info: ProbeInfo, c: OpsCfg): { start: number; end: number } {
  const dur = Math.max(0, info.duration);
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
