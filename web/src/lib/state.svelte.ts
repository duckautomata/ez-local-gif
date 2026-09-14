// Application state (Svelte 5 runes) plus the pure functions that turn the
// editable configuration into the wire types of internal/recipe.

import {
  isAnimatedFormat,
  isStaticFormat,
  isVideoFormat,
  type AutoCropParams,
  type ChromaKeyParams,
  type ColorKeyParams,
  type CropParams,
  type DelayParams,
  type FeatherParams,
  type FitMode,
  type FlipParams,
  type FPSParams,
  type Op,
  type Output,
  type OverlayParams,
  type PresetId,
  type ProbeInfo,
  type ResizeParams,
  type RotateParams,
  type Source,
  type SpeedParams,
  type Target,
  type TextParams,
  type TrimParams,
} from './api';
import { clamp, fitSize, frameCount, frameSpan, GIF_MAX_FPS, round, snapFPS, trimTime } from './format';
import {
  isAnimatedAsset,
  newImageOverlay,
  newTextOverlay,
  overlayReady,
  TEXT_DEFAULTS,
  type ImageOverlayCfg,
  type OverlayCfg,
  type TextOverlayCfg,
} from './overlay';
import { defaultOutput, fitsFormat, gifskiAllowed, isSequence, limitKiB, presetAvailable, presetById, videoCRF, type OutputCfg } from './presets';

// isSequence lives in presets.ts (isGifSource needs it there); re-exported so
// components keep importing it from the state module.
export { isSequence };
export type { ImageOverlayCfg, OverlayCfg, TextOverlayCfg };

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

/** Key colours of the Greenscreen / Bluescreen modes (recipe.ChromaKeyParams defaults). */
export const CHROMA_GREEN = '00ff00';
export const CHROMA_BLUE = '0000ff';
/** CHROMA_DEFAULTS mirrors recipe.ChromaKeyParams' zero values. */
export const CHROMA_DEFAULTS = { similarity: 0.2, blend: 0.05, despillMix: 0.6, despillExpand: 0.3 };
/** COLORKEY_DEFAULTS mirrors recipe.ColorKeyParams' zero values. */
export const COLORKEY_DEFAULTS = { similarity: 0.1, blend: 0 };

export type BackgroundMode = 'green' | 'blue' | 'pick';

/**
 * BackgroundCfg: background removal. Greenscreen / Bluescreen key in YUV
 * (op "chromakey", with despill); "Pick a colour" keys one RGB colour from
 * the eyedropper (op "colorkey"). Each mode keeps its own tolerances.
 */
export interface BackgroundCfg {
  enabled: boolean;
  mode: BackgroundMode;
  /** chroma key colour RRGGBB: CHROMA_GREEN / CHROMA_BLUE, or a custom one */
  color: string;
  similarity: number;
  blend: number;
  despill: boolean;
  despillMix: number;
  despillExpand: number;
  /** colour picked from the preview (RRGGBB); '' = none yet */
  pickColor: string;
  pickSimilarity: number;
  pickBlend: number;
}

/**
 * FeatherCfg: Gaussian blur of the alpha plane (op "feather") — softens the
 * transparency edge after keying / on rough 1-bit GIF alpha. The radius is a
 * sigma in SOURCE pixels (the graph hoists the stage before any geometry, so
 * it scales down with the output).
 */
export interface FeatherCfg {
  enabled: boolean;
  radius: number;
}

/** AutoCropCfg: crop to the content box (op "autocrop"); wins over the manual rectangle while on. */
export interface AutoCropCfg {
  enabled: boolean;
  /** px added on every side */
  padding: number;
  /** alpha >= threshold counts as content (1..255) */
  threshold: number;
}

/** OpsCfg is the editable form of the op stack; buildOps() serialises it. */
export interface OpsCfg {
  unpremultiply: boolean;
  delay: DelayCfg;
  trim: TrimCfg;
  crop: CropCfg;
  autocrop: AutoCropCfg;
  resize: ResizeCfg;
  fps: FpsCfg;
  speed: SpeedCfg;
  /** play backwards (op "reverse", after the geometry) */
  reverse: boolean;
  /** forward then backward, "ping-pong" (op "bounce", after reverse): frames and duration double */
  bounce: boolean;
  flipRotate: FlipRotateCfg;
  background: BackgroundCfg;
  /** soft transparency edge (op "feather", emitted right after the keying op) */
  feather: FeatherCfg;
  /** text / image overlays in their own order (the last draws on top) */
  overlays: OverlayCfg[];
}

export interface UiState {
  backdrop: Backdrop;
  /**
   * Backdrop of the Result card — separate from the preview's, judged
   * against Discord dark first, and kept across re-renders / "Render again"
   * (the card remounts per render; review R1).
   */
  resultBackdrop: Backdrop;
  /**
   * Locked crop aspect ratio as w/h (> 0 = the Crop card's "Lock ratio" is
   * on; 0 = free). Purely a UI constraint on the rectangle — never part of
   * the recipe / crop op payload (review R2).
   */
  cropRatio: number;
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
  /** the eyedropper is armed: the next click on the preview picks the colour to key */
  pickColor: boolean;
  /** id of the overlay whose drag box is highlighted (0 = none) */
  selectedOverlay: number;
}

/** DEFAULT_DELAY_MS is the sequence frame delay the server assumes when the client sends none. */
export const DEFAULT_DELAY_MS = 100;

export function defaultBackground(): BackgroundCfg {
  return {
    enabled: false,
    mode: 'green',
    color: CHROMA_GREEN,
    similarity: CHROMA_DEFAULTS.similarity,
    blend: CHROMA_DEFAULTS.blend,
    despill: true,
    despillMix: CHROMA_DEFAULTS.despillMix,
    despillExpand: CHROMA_DEFAULTS.despillExpand,
    pickColor: '',
    pickSimilarity: COLORKEY_DEFAULTS.similarity,
    pickBlend: COLORKEY_DEFAULTS.blend,
  };
}

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
    autocrop: { enabled: false, padding: 0, threshold: 1 },
    resize: { enabled: false, width: 0, height: 0, fit: 'contain' },
    fps: { enabled: false, fps: Math.min(srcFps, GIF_MAX_FPS) },
    speed: { enabled: false, factor: 1 },
    reverse: false,
    bounce: false,
    flipRotate: { enabled: false, horizontal: false, vertical: false, degrees: 0 },
    background: defaultBackground(),
    feather: { enabled: false, radius: FEATHER_DEFAULT },
    overlays: [],
  };
}

/** FEATHER_DEFAULT mirrors recipe.FeatherParams' zero value (radius 0 = 3). */
export const FEATHER_DEFAULT = 3;

function defaultUi(): UiState {
  return { backdrop: 'checker', resultBackdrop: 'dark', cropRatio: 0, scrubFrame: 0, cropOpen: false, pickColor: false, selectedOverlay: 0 };
}

export const app = $state({
  source: null as Source | null,
  ops: defaultOps(null),
  output: defaultOutput(),
  ui: defaultUi(),
});

/** resetUi puts the per-source UI flags back (the backdrop choices are kept). */
function resetUi(): void {
  app.ui.scrubFrame = 0;
  app.ui.cropOpen = false;
  app.ui.cropRatio = 0;
  app.ui.pickColor = false;
  app.ui.selectedOverlay = 0;
}

/**
 * setCropRatioLock turns the Crop card's "Lock ratio" on — capturing the
 * current rectangle's w:h as the locked ratio — or off (review R2). The lock
 * lives in UI state only; the crop op payload never carries it.
 */
export function setCropRatioLock(on: boolean): void {
  const c = app.ops.crop;
  app.ui.cropRatio = on ? (c.w > 0 && c.h > 0 ? c.w / c.h : 1) : 0;
}

/**
 * centerSquareCrop sets the manual crop to the largest centred square of a
 * width×height frame (handy for emotes); a locked ratio follows to 1:1.
 */
export function centerSquareCrop(width: number, height: number): void {
  const s = Math.min(width, height);
  app.ops.crop = {
    enabled: true,
    x: Math.floor((width - s) / 2),
    y: Math.floor((height - s) / 2),
    w: s,
    h: s,
  };
  if (app.ui.cropRatio > 0) app.ui.cropRatio = 1;
}

/**
 * setSource installs a freshly uploaded source and resets the op stack for
 * it. A preset the new source cannot use (Optimize needs a GIF) falls back to
 * Chat.
 */
export function setSource(src: Source | null): void {
  app.source = src;
  app.ops = defaultOps(src?.info ?? null);
  resetUi();
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
  resetUi();
}

// ---------------------------------------------------------------------------
// overlays (Phase 3)

let nextOverlayId = 1;

/** addOverlay appends a text or image card and returns it. */
export function addOverlay(kind: 'text' | 'image'): OverlayCfg {
  const o = kind === 'text' ? newTextOverlay(nextOverlayId++) : newImageOverlay(nextOverlayId++);
  app.ops.overlays.push(o);
  app.ui.selectedOverlay = o.id;
  return app.ops.overlays[app.ops.overlays.length - 1];
}

/** removeOverlay drops the card with that id (a no-op for an unknown id). */
export function removeOverlay(id: number): void {
  const i = app.ops.overlays.findIndex((o) => o.id === id);
  if (i < 0) return;
  app.ops.overlays.splice(i, 1);
  if (app.ui.selectedOverlay === id) app.ui.selectedOverlay = 0;
}

/** moveOverlay shifts a card by delta positions (−1 = up / drawn earlier, +1 = down / drawn later), clamped. */
export function moveOverlay(id: number, delta: number): void {
  const list = app.ops.overlays;
  const i = list.findIndex((o) => o.id === id);
  if (i < 0) return;
  const j = clamp(i + delta, 0, list.length - 1);
  if (j === i) return;
  const [o] = list.splice(i, 1);
  list.splice(j, 0, o);
}

/**
 * assetHashes lists the blobs the image overlays use, in first-use order
 * (the order their ops are emitted), deduplicated — these become
 * recipe.sources[1..] and the overlay ops reference them by index. Cards
 * that emit no op (disabled, no asset) contribute nothing, so removing a
 * card re-indexes the rest.
 */
export function assetHashes(c: OpsCfg): string[] {
  const out: string[] = [];
  for (const o of c.overlays) {
    if (o.kind !== 'image' || !overlayReady(o) || !o.asset) continue;
    if (!out.includes(o.asset.hash)) out.push(o.asset.hash);
  }
  return out;
}

/** recipeSources is recipe.sources: the main source, then the assets of effectiveOps (none for Optimize). */
export function recipeSources(mainHash: string, c: OpsCfg, out: Pick<OutputCfg, 'preset'>): string[] {
  return [mainHash, ...assetHashes(effectiveOps(c, out))];
}

/**
 * chromaKeyOp serialises the Greenscreen / Bluescreen mode; defaults (the
 * recipe's zero values: similarity 0.2, blend 0.05, despill 0.6 / 0.3) are
 * left out. A blend of exactly 0 cannot be expressed (the Go zero value is
 * the default 0.05), which is why the card's blend slider starts at 0.01.
 */
function chromaKeyOp(b: BackgroundCfg): Op {
  const p: ChromaKeyParams = {};
  if (b.color && b.color !== CHROMA_GREEN) p.color = b.color;
  if (b.similarity > 0 && b.similarity !== CHROMA_DEFAULTS.similarity) p.similarity = round(clamp(b.similarity, 0.01, 1));
  if (b.blend > 0 && b.blend !== CHROMA_DEFAULTS.blend) p.blend = round(clamp(b.blend, 0.01, 1));
  if (!b.despill) p.despillOff = true;
  else {
    if (b.despillMix > 0 && b.despillMix !== CHROMA_DEFAULTS.despillMix) p.despillMix = round(clamp(b.despillMix, 0, 1));
    if (b.despillExpand > 0 && b.despillExpand !== CHROMA_DEFAULTS.despillExpand) p.despillExpand = round(clamp(b.despillExpand, 0, 1));
  }
  return Object.keys(p).length ? { kind: 'chromakey', params: p } : { kind: 'chromakey' };
}

/** colorKeyOp serialises the Pick-a-colour mode (null until a colour was picked). */
function colorKeyOp(b: BackgroundCfg): Op | null {
  if (!b.pickColor) return null;
  const p: ColorKeyParams = { color: b.pickColor };
  if (b.pickSimilarity > 0 && b.pickSimilarity !== COLORKEY_DEFAULTS.similarity) p.similarity = round(clamp(b.pickSimilarity, 0.01, 1));
  if (b.pickBlend > 0) p.blend = round(clamp(b.pickBlend, 0, 1));
  return { kind: 'colorkey', params: p };
}

/** backgroundOp is the keying op of the Background card (null when off / nothing to key). */
export function backgroundOp(b: BackgroundCfg): Op | null {
  if (!b.enabled) return null;
  return b.mode === 'pick' ? colorKeyOp(b) : chromaKeyOp(b);
}

/** timeRange adds start/end (output seconds, whole µs like trim bounds) when they are set. */
function timeRange(p: { start?: number; end?: number }, o: { start: number; end: number }): void {
  if (o.start > 0) p.start = trimTime(o.start);
  if (o.end > 0) p.end = trimTime(o.end);
}

function textOp(o: TextOverlayCfg): Op {
  const p: TextParams = { text: o.text, x: Math.round(o.x), y: Math.round(o.y) };
  if (o.font && o.font !== TEXT_DEFAULTS.font) p.font = o.font;
  if (o.size > 0 && o.size !== TEXT_DEFAULTS.size) p.size = Math.round(o.size);
  if (o.color && o.color !== TEXT_DEFAULTS.color) p.color = o.color;
  if (o.border > 0) {
    p.border = Math.round(o.border);
    if (o.borderColor && o.borderColor !== TEXT_DEFAULTS.borderColor) p.borderColor = o.borderColor;
  }
  if (o.box) {
    p.box = true;
    if (o.boxColor && o.boxColor !== TEXT_DEFAULTS.boxColor) p.boxColor = o.boxColor;
    if (o.boxPad >= 0 && o.boxPad !== TEXT_DEFAULTS.boxPad) p.boxPad = Math.round(o.boxPad);
  }
  if (o.anchor !== 'tl') p.anchor = o.anchor;
  timeRange(p, o);
  return { kind: 'text', params: p };
}

function overlayOp(o: ImageOverlayCfg, source: number): Op {
  const p: OverlayParams = { source, x: Math.round(o.x), y: Math.round(o.y) };
  if (o.width > 0) p.width = Math.round(o.width);
  if (o.height > 0) p.height = Math.round(o.height);
  if (o.opacity > 0 && o.opacity < 1) p.opacity = round(o.opacity);
  if (!o.loop && isAnimatedAsset(o.asset)) p.noLoop = true;
  if (o.anchor !== 'tl') p.anchor = o.anchor;
  timeRange(p, o);
  return { kind: 'overlay', params: p };
}

/** overlayOps serialises the overlay cards in their order; image cards index assetHashes. */
function overlayOps(c: OpsCfg): Op[] {
  const assets = assetHashes(c);
  const ops: Op[] = [];
  for (const o of c.overlays) {
    if (!overlayReady(o)) continue;
    if (o.kind === 'text') ops.push(textOp(o));
    else if (o.asset) ops.push(overlayOp(o, 1 + assets.indexOf(o.asset.hash)));
  }
  return ops;
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

export interface BuildOpsOptions {
  /** stop before crop: the still shows the full frame in source pixels for the drag rectangle */
  cropPreview?: boolean;
  /** leave the keying op out: the eyedropper needs the original colours */
  keyPreview?: boolean;
}

/**
 * buildOps serialises the op configuration in the documented order:
 * unpremultiply, delay, trim, speed, fps, chromakey/colorkey, feather,
 * crop/autocrop, resize, canvas, flip, rotate, reverse, bounce, then the
 * text/overlay ops in the user's order. With cropPreview the stack stops
 * before crop, so the still
 * shows the full frame in source pixel coordinates for the drag rectangle;
 * with keyPreview the keying op is skipped (the eyedropper picks from the
 * unkeyed frame).
 */
export function buildOps(c: OpsCfg, opts: BuildOpsOptions = {}): Op[] {
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
  // Keying runs at full resolution before any geometry (DESIGN §4.3).
  const key = opts.keyPreview ? null : backgroundOp(c.background);
  if (key) ops.push(key);
  // Feather sits with the keying stages (the graph hoists it right after the
  // keys, before any geometry — review R4), so it is emitted after the key op
  // and before crop/autocrop; the crop preview shows it too.
  if (c.feather.enabled && c.feather.radius > 0) {
    const p: FeatherParams = { radius: round(clamp(c.feather.radius, 0.1, 50)) };
    ops.push({ kind: 'feather', params: p });
  }
  if (opts.cropPreview) return ops;

  if (c.autocrop.enabled) {
    const p: AutoCropParams = {};
    if (c.autocrop.threshold > 1) p.threshold = clamp(Math.round(c.autocrop.threshold), 1, 255);
    if (c.autocrop.padding > 0) p.padding = clamp(Math.round(c.autocrop.padding), 0, 1024);
    ops.push(Object.keys(p).length ? { kind: 'autocrop', params: p } : { kind: 'autocrop' });
  } else if (c.crop.enabled && c.crop.w > 0 && c.crop.h > 0) {
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
  if (c.reverse) ops.push({ kind: 'reverse' });
  // Bounce follows reverse: [reverse, bounce] plays backwards then forwards,
  // [bounce] forwards then backwards (the graph compiles the {reverse, bounce}
  // group after the output fit in stack order).
  if (c.bounce) ops.push({ kind: 'bounce' });
  ops.push(...overlayOps(c));
  return ops;
}

/** cropActive: a crop op (manual or auto) is part of the stack. */
export function cropActive(c: OpsCfg): boolean {
  return c.autocrop.enabled || (c.crop.enabled && c.crop.w > 0 && c.crop.h > 0);
}

/** hasOverlays: at least one overlay card emits an op (the preview then shows the drag boxes). */
export function hasOverlays(c: OpsCfg): boolean {
  return c.overlays.some(overlayReady);
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
export function recipeOps(c: OpsCfg, out: Pick<OutputCfg, 'preset'>, opts: BuildOpsOptions = {}): Op[] {
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

/** usesMatte: formats flattened onto / thresholded against the matte colour (mp4/webm are fully flattened — Phase 4). */
export function usesMatte(c: Pick<OutputCfg, 'format' | 'frameFormat'>): boolean {
  return c.format === 'gif' || c.format === 'jpeg' || isVideoFormat(c.format) || (c.format === 'frames' && c.frameFormat === 'jpeg');
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
      if (c.encoder === 'gifski' && gifskiAllowed(c)) {
        // The gifski HQ path: gifski quantises and dithers itself, so the
        // ffmpeg-palette knobs (colours / dither / lossy / alpha threshold /
        // matte) do not apply and are left out of the recipe; quality is
        // gifski --quality (0 = the server default 90).
        o.encoder = 'gifski';
        if (c.quality > 0) o.quality = clamp(Math.round(c.quality), 1, 100);
        break;
      }
      o.colors = c.colors;
      o.dither = c.dither;
      if (c.lossy > 0) o.lossy = Math.round(c.lossy);
      o.alphaThreshold = c.alphaThreshold;
      o.matte = c.matte;
      break;
    case 'mp4':
    case 'webm': {
      // Opaque video (Phase 4): quality IS the CRF (0 = the server default
      // 20 / 30); the master is flattened onto the matte.
      const crf = videoCRF(c.format);
      if (c.quality > 0 && crf) o.quality = clamp(Math.round(c.quality), 1, crf.max);
      o.matte = c.matte;
      break;
    }
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
 * previewOutput is the part of a recipe output the preview endpoints render
 * from — format, width, height, fit and fps — mirroring jobs.stillOutput
 * (internal/jobs/still.go): the server keys its still and proxy memos on
 * exactly these, so the still / proxy requests carry nothing else and a
 * change of quality, lossy, colours, dither, matte, loop, fit budget,
 * preset or target neither re-requests the still nor marks a playing proxy
 * as changed.
 */
export function previewOutput(o: Output): Output {
  const p: Output = { format: o.format };
  if (o.width) p.width = o.width;
  if (o.height) p.height = o.height;
  if (o.fit) p.fit = o.fit;
  if (o.fps) p.fps = o.fps;
  return p;
}

/**
 * planCanvas is the output canvas the overlays are drawn on (graph.Plan's
 * Width × Height, what the preview still measures in overlay mode): the
 * source frame after crop, resize and a 90° / 270° rotation, then the
 * Output card's width / height / fit (graph.outputFit: both set → exactly
 * W×H, since contain pads, cover centre-crops and exact stretches; one set
 * → scaled to it keeping the aspect; none → as produced). null when it
 * cannot be known client-side: no source size, or auto-crop on (the server
 * resolves the content box) without both output dimensions. `c` should be
 * effectiveOps.
 *
 * With `ignoreAutocrop` the auto-crop stage is left out and the canvas is
 * the UNCROPPED frame's: the content box is at most the whole frame, and
 * the manual rectangle is skipped too because auto-crop replaces it in the
 * stack (buildOps emits one or the other), so the rectangle says nothing
 * about the box. planMaster uses it for the "up to … before
 * crop-to-content" estimate; nothing else should.
 */
export interface PlanCanvasOptions {
  /** treat auto-crop as absent: the pre-crop canvas (planMaster's upper bound) */
  ignoreAutocrop?: boolean;
}

export function planCanvas(info: ProbeInfo | null | undefined, c: OpsCfg, out: Pick<OutputCfg, 'width' | 'height' | 'fit'>, opts: PlanCanvasOptions = {}): { w: number; h: number } | null {
  if (!info || !(info.width > 0) || !(info.height > 0)) return null;
  if (out.width > 0 && out.height > 0) return { w: Math.round(out.width), h: Math.round(out.height) };
  if (c.autocrop.enabled && !opts.ignoreAutocrop) return null;
  let w = info.width;
  let h = info.height;
  if (!c.autocrop.enabled && c.crop.enabled && c.crop.w > 0 && c.crop.h > 0) {
    w = Math.round(c.crop.w);
    h = Math.round(c.crop.h);
  }
  if (c.resize.enabled && (c.resize.width > 0 || c.resize.height > 0)) ({ w, h } = fitSize(w, h, c.resize.width, c.resize.height, c.resize.fit));
  if (c.flipRotate.enabled && (c.flipRotate.degrees === 90 || c.flipRotate.degrees === 270)) [w, h] = [h, w];
  return fitSize(w, h, out.width, out.height, out.fit);
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
 * forwardDuration is the output-time length of the clip after trim and speed
 * but BEFORE any bounce doubling: for an image sequence the selected grid
 * frames at their rate, else the trimmed source time; both divided by the
 * speed. This is the timeline "from scrubber" maps through (toSourceTime):
 * a bounced clip's second half mirrors back onto it.
 */
export function forwardDuration(info: ProbeInfo, c: OpsCfg): number {
  const seq = sequenceGrid(info, c);
  if (seq) return seq.selected / seq.rate / speedFactor(c);
  const { start, end } = trimRange(info, c);
  return Math.max(0, (end - start) / speedFactor(c));
}

/**
 * previewDuration is the output-time length of the clip (graph.Plan.Duration):
 * forwardDuration, doubled by a bounce op (Phase 4 — the plan's Duration and
 * Frames already carry the ×2, so the scrubber and the sticker ≤ 5 s check
 * see the doubled length).
 */
export function previewDuration(info: ProbeInfo, c: OpsCfg): number {
  const d = forwardDuration(info, c);
  return c.bounce && !info.isStill ? d * 2 : d;
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

/**
 * toSourceTime maps a scrubber (output-time) position back to source seconds.
 * On a bounced clip the mirrored half (t ≥ forwardDuration) folds back onto
 * the forward timeline — output D + x shows the same source frame as D − x —
 * so trim-from-scrubber and the eyedropper keep working across the whole
 * doubled range.
 */
export function toSourceTime(t: number, info: ProbeInfo, c: OpsCfg): number {
  if (c.bounce && !info.isStill) {
    const fwd = forwardDuration(info, c);
    if (t > fwd) t = Math.max(0, 2 * fwd - t);
  }
  const { start, end } = trimRange(info, c);
  return Math.min(end, start + t * speedFactor(c));
}

/**
 * DEFAULT_FPS mirrors graph's defaultFPS (internal/graph/compile.go): the
 * rate the compiler plans at when neither an fps op, Output.fps nor the
 * probe names one.
 */
export const DEFAULT_FPS = 10;

/**
 * planFPS is the plan's frame grid: the effective output fps (fps op →
 * Output.fps → source rate, snapped for the format). When none of those is
 * known but the clip's length is, it is graph's default: compiler.fps falls
 * through to defaultFPS (10) and assemble then plans floor(Duration × 10)
 * frames — a probe that reports a duration but neither r_frame_rate nor
 * avg_frame_rate (rare) — so the scrubber and the master estimate count
 * the frames the server will render instead of showing nothing. 0 when
 * nothing is known, and for a still (no length: its one frame needs no
 * grid). `c` should be effectiveOps (all-off for Optimize).
 */
export function planFPS(info: ProbeInfo | null | undefined, c: OpsCfg, out: OutputCfg): number {
  if (!info) return 0;
  const fps = effectiveFPS(c, out, sourceFPS(info, c));
  if (fps > 0) return fps;
  return !info.isStill && sourceDuration(info, c) > 0 ? snapFPS(out.format, DEFAULT_FPS) : 0;
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
 * graph's error. A bounce op doubles the count EXACTLY (graph.Plan.Frames is
 * the forward count × 2, never a re-floor of the doubled duration); a still
 * or single-frame source stays at 1 (the graph refuses bouncing those).
 * `c` should be effectiveOps (all-off for Optimize).
 */
export function planFrames(info: ProbeInfo | null | undefined, c: OpsCfg, out: OutputCfg): number {
  if (!info) return 0;
  const fps = planFPS(info, c, out);
  if (!(fps > 0)) return 0;
  if (info.isStill) return 1;
  const bounce = c.bounce ? 2 : 1;
  const seq = sequenceGrid(info, c);
  if (seq) {
    if (seq.count === 1) return 1;
    return Math.max(1, sequenceFrames(seq.selected, speedFactor(c), seq.rate, fps)) * bounce;
  }
  const dur = forwardDuration(info, c);
  if (!(dur > 0)) return 1;
  return frameCount(dur, fps) * bounce;
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
  let idx = clamp(Math.round(i), 0, total - 1);
  // A bounced plan doubles the frame count; an index in the mirrored half
  // shows the same source frame as its reflection in the forward half, so
  // fold it back first (frame N+k mirrors frame N−1−k) and work on the
  // forward grid — the trim the window feeds runs BEFORE the bounce.
  let fwdTotal = total;
  if (ops.bounce && !info.isStill && total >= 2 && total % 2 === 0) {
    fwdTotal = total / 2;
    if (idx >= fwdTotal) idx = 2 * fwdTotal - 1 - idx;
  }
  const dur = sourceDuration(info, c);
  const range = trimRange(info, c);
  const start = Math.min(trimTime(toSourceTime(idx / fps, info, c)), trimStartMax(info, c));
  let end = idx >= fwdTotal - 1 ? range.end : Math.min(trimTime(toSourceTime((idx + 1) / fps, info, c)), dur);
  if (end >= dur) end = 0;
  return { start, end };
}

/**
 * forwardFrame folds 0-based plan frame i back onto the forward, un-reversed
 * frame grid — the frame a still request whose op stack stops BEFORE reverse
 * and bounce (crop mode: buildOps cropPreview) must ask for, or the server
 * clamps a mirrored-half time to the clip end and renders the wrong frame
 * (WEB-5). A bounced plan's mirrored half maps onto its forward twin (frame
 * N+k → N−1−k, like frameWindow), and a reverse op flips the forward grid
 * (with both the clip is [rev(F), F], so the two folds compose). Without
 * reverse/bounce it is i unchanged (clamped to the plan).
 */
export function forwardFrame(info: ProbeInfo, c: OpsCfg, out: OutputCfg, i: number): number {
  const ops = effectiveOps(c, out);
  const total = planFrames(info, ops, out);
  if (total <= 0) return 0;
  let idx = clamp(Math.round(i), 0, total - 1);
  let fwdTotal = total;
  if (ops.bounce && !info.isStill && total >= 2 && total % 2 === 0) {
    fwdTotal = total / 2;
    if (idx >= fwdTotal) idx = 2 * fwdTotal - 1 - idx;
  }
  if (ops.reverse) idx = fwdTotal - 1 - idx;
  return idx;
}

// ---------------------------------------------------------------------------
// Frame-master estimate — mirrors the render admission of internal/jobs
// (scratch.go: masterBytes / scratchFactor / scratchReserve / admitScratch;
// render.go: oneFramePlan, the frames-export cap). The graph never refuses a
// plan for its frame count (2026-09-13: forward stills and proxies of any
// source work), so the only thing standing between an untrimmed 4K clip and
// a refused Render is jobs' cap — and the user must see it BEFORE pressing
// Render, next to the trim / crop / resize / fit controls that shrink it.
// The Render button stays enabled: the server's admission has the last word
// (auto-crop recipes are admitted after detection, unknown lengths pass).
// ---------------------------------------------------------------------------

/**
 * MAX_EXTRACT_FRAMES mirrors jobs.MaxExtractFrames (internal/jobs/frames.go):
 * a frames export above it is refused before any decode ("too many frames
 * … trim or lower fps"), checked on the plan's full frame count — the
 * static-format one-frame cut does not apply to "frames".
 */
export const MAX_EXTRACT_FRAMES = 2000;

/**
 * SCRATCH_HEADROOM_MIN / SCRATCH_HEADROOM_DIV mirror jobs.scratchHeadroomMin
 * / scratchHeadroomDiv: the extra scratch reserved next to the master for
 * the encoder outputs is the larger of master/8 and 8 MiB.
 */
export const SCRATCH_HEADROOM_MIN = 8 << 20;
const SCRATCH_HEADROOM_DIV = 8;

/**
 * SCRATCH_FIT_FORMATS mirrors jobs.fitFormats (internal/jobs/fit.go) — the
 * formats whose fit search keeps candidates on scratch and so cost one
 * master more (scratchFactor). It deliberately differs from
 * presets.FIT_FORMATS, which lacks png because the Output card offers no fit
 * for a static PNG and buildOutput never emits fitBytes for it; jobs' list
 * has png (a static-PNG fit ladder exists server-side), and the mirror keeps
 * jobs' list so its unit test can check jobs' own factor table verbatim.
 */
const SCRATCH_FIT_FORMATS: ReadonlySet<string> = new Set(['gif', 'webp', 'apng', 'avif', 'png', 'jpeg', 'mp4', 'webm']);

/**
 * scratchFactor mirrors jobs.scratchFactor on the WIRE output (buildOutput's
 * result, so the same knobs the server sees): how many master-sized chunks
 * of scratch a render may need on top of the headroom — 1 for outputs
 * encoded straight from the master (gif, webp, RGBA apng, static, video); 2
 * when PNG intermediates of every frame are written next to it (avif,
 * frames, indexed apng — colors > 0 or a fit budget, gifski gif); +1 more
 * for a fit search on a fit format, whose ladder keeps candidates plus the
 * per-variant intermediates alive until the search returns.
 */
export function scratchFactor(o: Output): number {
  const format = String(o.format).toLowerCase();
  let f = 1;
  switch (format) {
    case 'avif':
    case 'frames':
      f = 2;
      break;
    case 'apng':
      if ((o.colors ?? 0) > 0 || (o.fitBytes ?? 0) > 0) f = 2;
      break;
    case 'gif':
      if ((o.encoder ?? '').trim().toLowerCase() === 'gifski') f = 2; // gifski reads PNG frames of the whole master
      break;
  }
  if ((o.fitBytes ?? 0) > 0 && SCRATCH_FIT_FORMATS.has(format)) f++;
  return f;
}

/**
 * scratchReserve mirrors jobs.scratchReserve: what a render reserves from
 * the scratch budget for a master of `need` bytes — the master plus
 * max(need/8, 8 MiB) headroom for the encoded outputs (integer division
 * like Go's). 0 stays 0 (unknown).
 */
export function scratchReserve(need: number): number {
  if (!(need > 0)) return 0;
  return need + Math.max(Math.floor(need / SCRATCH_HEADROOM_DIV), SCRATCH_HEADROOM_MIN);
}

/** MasterEstimate is what planMaster predicts jobs' admission will measure. */
export interface MasterEstimate {
  /** graph.Plan.Width × Height: the master's frame size (planCanvas) */
  w: number;
  h: number;
  /** master frames: 1 for a still / single-frame source and for a static format, else planFrames (a bounce already doubled it) */
  frames: number;
  /** jobs.masterBytes: w × h × 4 × frames */
  bytes: number;
  /** jobs.scratchFactor of the wire output */
  factor: number;
  /** what admitScratch reserves: scratchReserve(bytes) + (factor − 1) × bytes */
  reserve: number;
  /**
   * frames ffmpeg's reverse filter buffers for a static (png / jpeg) render
   * with the reverse op on — planFrames, the pre-cut count (doubled per
   * bounce, the bound render.go hands admitReversed); 0 for every other
   * render (an animated render's buffer is at most its master, which
   * `bytes` already measures; a bounce alone buffers nothing before its
   * first frame) and when the count is unknown (the server cannot check it either)
   */
  bufferFrames: number;
  /** w × h × 4 × bufferFrames: the reverse buffer admitReversed judges against the cap alone (RAM, not scratch); 0 when there is none */
  bufferBytes: number;
  /** planFPS (0 = unknown) — turns a frame budget into seconds */
  fps: number;
  /**
   * the canvas is the pre-crop one: auto-crop is on without both output
   * dimensions, so the content box the server resolves is unknown here and
   * the figure is "up to … before crop-to-content"
   */
  upperBound: boolean;
}

/**
 * planMaster is the SPA's copy of the frame-master estimate jobs admits a
 * render on (jobs.masterBytes / admitScratch): null when there is nothing to
 * estimate — no source, the gifsicle-only Optimize preset (no ops, no
 * master; !opsApply), or an unknown frame count (no rate: jobs then admits
 * need 0 and only the ENOSPC mapping bounds the render — the same blind
 * spot the server has, so no verdict is shown). `c` should be effectiveOps.
 *
 *   - frames: 1 for a still source or a one-frame sequence
 *     (graph.singleFrame → Frames 1) and for a static format
 *     (recipe.IsStaticFormat: png / jpeg — render.go cuts the plan to
 *     oneFramePlan before admitScratch); otherwise planFrames, which
 *     already doubles per bounce like graph's assemble.
 *   - bufferFrames / bufferBytes: the reverse buffer of a static render
 *     with the reverse op on — render.go admits it by admitReversed on the
 *     PRE-cut plan, before the one-frame cut, because "-frames:v 1"
 *     shortens the encode, not the decode: a reverse filter must consume
 *     the whole clip before it can emit its first frame, so a reversed PNG
 *     of an untrimmed 4K clip costs 9.7 GiB of RAM for one frame of
 *     master. The count is planFrames (doubled by a bounce on top of the
 *     reverse — [reverse, bounce] — exactly the doubled Plan.Frames the
 *     server judges, a conservative bound there too). 0 for a bounce
 *     alone: the bounce's first output frame is the forward branch's and
 *     the run ends before its reverse branch fills (render.go measured
 *     it), so the server does not check it either; 0 for animated
 *     renders, whose buffer is at most the master `bytes` measures. An
 *     unknown count leaves it 0 with the one-frame master still shown:
 *     admitReversed passes need 0 the same way.
 *   - canvas: planCanvas — the dimensions are NOT rounded to even for
 *     mp4 / webm: the even-pad lives in the encoder tail (enc), after the
 *     master, so a 127×127 master is 127×127 (jobs measures the plan).
 *     When planCanvas is null only because auto-crop is on without both
 *     output dimensions, the uncropped canvas stands in (ignoreAutocrop)
 *     and upperBound is set: the content box is at most the whole frame —
 *     though with a resize or one output dimension keeping the aspect, a
 *     box of another aspect can come out taller or wider than the frame
 *     scaled the same way, so it is the pre-crop figure, not a strict bound.
 *   - factor / reserve: scratchFactor of the wire output (buildOutput) and
 *     scratchReserve(bytes) + (factor − 1) × bytes, what admitScratch
 *     reserves from the scratch budget (jobs.admitScratch → reserveScratch).
 *
 * The lossless gifsicle fast path (jobs.fastPathFor: a GIF → GIF trim /
 * crop / fps-drop / loop edit with the default encoder, no fit budget,
 * target none / attachment — no decode, no master) is unreachable from the
 * SPA today: buildOutput always emits colors / dither / alphaThreshold /
 * matte for gif, which the eligibility check refuses. If a "keep palette"
 * option ever appears, fast-path-shaped recipes must return null here.
 */
export function planMaster(info: ProbeInfo | null | undefined, c: OpsCfg, out: OutputCfg): MasterEstimate | null {
  if (!info || !opsApply(out)) return null;
  let canvas = planCanvas(info, c, out);
  let upperBound = false;
  if (!canvas && c.autocrop.enabled) {
    canvas = planCanvas(info, c, out, { ignoreAutocrop: true });
    upperBound = canvas !== null;
  }
  if (!canvas) return null;
  const single = info.isStill || (isSequence(info) && (info.sequence?.count ?? info.frames) === 1);
  const staticOut = isStaticFormat(out.format);
  const frames = single || staticOut ? 1 : planFrames(info, c, out);
  if (!(frames > 0)) return null;
  const frameBytes = canvas.w * canvas.h * 4;
  const bytes = frameBytes * frames;
  const factor = scratchFactor(buildOutput(out));
  const bufferFrames = staticOut && !single && c.reverse ? Math.max(0, planFrames(info, c, out)) : 0;
  return {
    w: canvas.w,
    h: canvas.h,
    frames,
    bytes,
    factor,
    reserve: scratchReserve(bytes) + (factor - 1) * bytes,
    bufferFrames,
    bufferBytes: frameBytes * bufferFrames,
    fps: planFPS(info, c, out),
    upperBound,
  };
}

/** MasterCaps is the part of capabilities.caps masterVerdict reads (0 = unknown). */
export interface MasterCaps {
  maxMasterBytes: number;
  scratchBudgetBytes: number;
}

/** MasterVerdict is masterVerdict's answer: whether jobs would refuse the estimate, and what fits instead. */
export interface MasterVerdict {
  /** the server would refuse the render up-front */
  over: boolean;
  /**
   * what binds, in the server's order: the reverse buffer of a static
   * render (admitReversed, before the one-frame cut), the per-render cap
   * or the scratch budget ('' when nothing is over)
   */
  by: '' | 'buffer' | 'cap' | 'scratch';
  /** the binding bound in master bytes — when nothing is over, the tighter known bound (0 = none known) */
  limit: number;
  /** master frames of est's size that fit under limit (0 = unknown, or not even one) */
  maxFrames: number;
  /**
   * maxFrames / fps FLOORED to a tenth of a second (0 when the rate is
   * unknown) — never above what fits, so a trim to exactly this value
   * plans at most maxFrames frames: 2330 frames at 30 fps are 77.667 s,
   * and a trim to a nearest-rounded "77.7 s" would plan floor(77.7 × 30 +
   * 1e-4) = 2331 frames, one over the cap the note said it fits.
   */
  maxSeconds: number;
}

/**
 * scratchMasterLimit inverts the reservation admitScratch makes — bytes +
 * max(bytes/8, 8 MiB) + (factor − 1) × bytes — for the largest master whose
 * reservation fits under `budget`: bytes × (factor + 1/8) once the master
 * is ≥ 64 MiB (its eighth then exceeds the 8 MiB floor), else factor × bytes
 * + 8 MiB. The reservation is continuous and increasing in bytes, so
 * whichever branch the first answer lands in is the right one. 0 for no
 * budget.
 */
function scratchMasterLimit(budget: number, factor: number): number {
  if (!(budget > 0)) return 0;
  let b = budget / (factor + 1 / SCRATCH_HEADROOM_DIV);
  if (b < SCRATCH_HEADROOM_MIN * SCRATCH_HEADROOM_DIV) b = (budget - SCRATCH_HEADROOM_MIN) / factor;
  return Math.max(0, Math.floor(b));
}

/**
 * masterVerdict judges a planMaster estimate the way jobs will, in the
 * render's order: the reverse buffer of a static render first
 * (`caps.maxMasterBytes > 0 && bufferBytes > maxMasterBytes` — "buffer";
 * render.go's admitReversed runs before the one-frame cut and judges the
 * buffer against the cap alone, RAM being no scratch), then the master
 * over the per-render cap (`bytes > maxMasterBytes` — "cap"), else over
 * the scratch budget (`caps.scratchBudgetBytes > 0 && reserve >
 * scratchBudgetBytes` — "scratch"; a fit / AVIF / frames render's multiple
 * can bind below the cap). `limit` is the binding bound in master bytes —
 * for the scratch case the largest master whose reservation fits
 * (scratchMasterLimit) — and maxFrames / maxSeconds turn it into what fits
 * at the estimate's frame size and rate, so the note can say "trim to
 * about N frames (S s)"; for the buffer case that is the clip length whose
 * reverse fits. An unknown cap (0: no answer yet, or an older server)
 * never binds, so the UI shows the estimate alone — the server's admission
 * has the last word.
 */
export function masterVerdict(est: MasterEstimate, caps: MasterCaps): MasterVerdict {
  const cap = caps.maxMasterBytes > 0 ? caps.maxMasterBytes : 0;
  const scratch = caps.scratchBudgetBytes > 0 ? scratchMasterLimit(caps.scratchBudgetBytes, est.factor) : 0;
  let by: MasterVerdict['by'] = '';
  let limit = 0;
  if (cap > 0 && est.bufferBytes > cap) {
    by = 'buffer';
    limit = cap;
  } else if (cap > 0 && est.bytes > cap) {
    by = 'cap';
    limit = cap;
  } else if (caps.scratchBudgetBytes > 0 && est.reserve > caps.scratchBudgetBytes) {
    by = 'scratch';
    limit = scratch;
  } else {
    limit = cap > 0 && scratch > 0 ? Math.min(cap, scratch) : Math.max(cap, scratch);
  }
  const frameBytes = est.w * est.h * 4;
  const maxFrames = limit > 0 && frameBytes > 0 ? Math.floor(limit / frameBytes) : 0;
  // floor to the tenth (see MasterVerdict.maxSeconds); one division so an on-grid value (29 frames at 10 fps = 2.9 s) is not pushed under by float error
  const maxSeconds = est.fps > 0 ? Math.floor((maxFrames * 10) / est.fps) / 10 : 0;
  return { over: by !== '', by, limit, maxFrames, maxSeconds };
}
