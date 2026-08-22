// Text / image overlay configuration (the cards of the Overlays section) and
// the pure geometry behind the drag-to-place boxes on the preview: anchor
// maths, the approximate footprint of a text overlay, the footprint of an
// image overlay, and whether an overlay is visible at a scrubber time.
// Framework-free (overlay.test.ts); the app state in state.svelte.ts holds
// the OverlayCfg list and buildOps serialises it.

import type { Anchor, Source } from './api';
import { fitSize, floorTime, frameStart } from './format';

/** TEXT_DEFAULTS mirrors recipe.TextParams' zero values (what the graph substitutes). */
export const TEXT_DEFAULTS = {
  font: 'DejaVu Sans',
  size: 32,
  color: 'ffffff',
  border: 0,
  borderColor: '000000',
  boxColor: '00000080',
  boxPad: 8,
  anchor: 'tl' as Anchor,
};

interface OverlayBase {
  /** stable identity for list keys and the preview selection */
  id: number;
  /** a disabled card stays in the list but emits no op */
  enabled: boolean;
  anchor: Anchor;
  /** anchor point on the output canvas, output pixels */
  x: number;
  y: number;
  /** output seconds (after trim / speed); 0 / 0 = whole clip, end 0 = to the end */
  start: number;
  end: number;
}

export interface TextOverlayCfg extends OverlayBase {
  kind: 'text';
  text: string;
  /** fontconfig family; '' = TEXT_DEFAULTS.font */
  font: string;
  size: number;
  /** RRGGBB or RRGGBBAA without '#' */
  color: string;
  border: number;
  borderColor: string;
  box: boolean;
  boxColor: string;
  boxPad: number;
}

export interface ImageOverlayCfg extends OverlayBase {
  kind: 'image';
  /** the uploaded asset (hash + probe); null until one is picked */
  asset: Source | null;
  /** 0 = natural; one of them 0 keeps the aspect */
  width: number;
  height: number;
  /** 0..1 */
  opacity: number;
  /** repeat an animated asset until the base ends (else hold the last frame) */
  loop: boolean;
}

export type OverlayCfg = TextOverlayCfg | ImageOverlayCfg;

export function newTextOverlay(id: number): TextOverlayCfg {
  return {
    kind: 'text',
    id,
    enabled: true,
    text: 'Text',
    font: TEXT_DEFAULTS.font,
    size: TEXT_DEFAULTS.size,
    color: TEXT_DEFAULTS.color,
    border: 2,
    borderColor: TEXT_DEFAULTS.borderColor,
    box: false,
    boxColor: TEXT_DEFAULTS.boxColor,
    boxPad: TEXT_DEFAULTS.boxPad,
    anchor: 'tl',
    x: 0,
    y: 0,
    start: 0,
    end: 0,
  };
}

export function newImageOverlay(id: number): ImageOverlayCfg {
  return {
    kind: 'image',
    id,
    enabled: true,
    asset: null,
    width: 0,
    height: 0,
    opacity: 1,
    loop: true,
    anchor: 'tl',
    x: 0,
    y: 0,
    start: 0,
    end: 0,
  };
}

/** isAnimatedAsset: the asset has more than one frame (loop applies). */
export function isAnimatedAsset(asset: Source | null | undefined): boolean {
  if (!asset) return false;
  const info = asset.info;
  return !info.isStill && (info.frames > 1 || info.duration > 0);
}

/**
 * overlayReady: the card produces an op — enabled, and a text card has text
 * (whitespace-only text draws nothing and the graph rejects an empty string),
 * an image card has an asset.
 */
export function overlayReady(o: OverlayCfg): boolean {
  if (!o.enabled) return false;
  return o.kind === 'text' ? o.text.trim().length > 0 : o.asset !== null;
}

// ---------------------------------------------------------------------------
// geometry

export interface Box {
  left: number;
  top: number;
  w: number;
  h: number;
}

/** anchorOrigin is the top-left corner of a w×h element whose `anchor` point sits at (x, y). */
export function anchorOrigin(anchor: Anchor, x: number, y: number, w: number, h: number): { left: number; top: number } {
  const v = anchor[0];
  const hz = anchor[1];
  const left = hz === 'c' ? x - w / 2 : hz === 'r' ? x - w : x;
  const top = v === 'm' ? y - h / 2 : v === 'b' ? y - h : y;
  return { left, top };
}

/** anchorPoint is the inverse of anchorOrigin: the anchor point of a w×h element placed at (left, top). */
export function anchorPoint(anchor: Anchor, left: number, top: number, w: number, h: number): { x: number; y: number } {
  const v = anchor[0];
  const hz = anchor[1];
  const x = hz === 'c' ? left + w / 2 : hz === 'r' ? left + w : left;
  const y = v === 'm' ? top + h / 2 : v === 'b' ? top + h : top;
  return { x, y };
}

/**
 * Approximate glyph metrics of the bundled sans faces (DejaVu Sans): an
 * average advance of 0.6 em and a line box of 1.2 em. The preview box only
 * has to be close enough to grab; the real composite is what the still shows.
 */
const CHAR_ADVANCE_EM = 0.6;
const LINE_HEIGHT_EM = 1.2;

/**
 * textBoxSize estimates the footprint of a text overlay in output pixels:
 * the longest line × 0.6 em wide, 1.2 em per line high, grown by the outline
 * on every side and by the box padding when the box is on. Never smaller
 * than one em, so an empty text still has something to drag.
 */
export function textBoxSize(t: Pick<TextOverlayCfg, 'text' | 'size' | 'border' | 'box' | 'boxPad'>): { w: number; h: number } {
  const size = Math.max(1, t.size || TEXT_DEFAULTS.size);
  const lines = t.text.split('\n');
  const longest = Math.max(...lines.map((l) => l.length), 1);
  const border = Math.max(0, t.border);
  const pad = t.box ? Math.max(0, t.boxPad) : 0;
  const w = Math.ceil(size * CHAR_ADVANCE_EM * longest) + 2 * border + 2 * pad;
  const h = Math.ceil(size * LINE_HEIGHT_EM * lines.length) + 2 * border + 2 * pad;
  return { w: Math.max(w, size), h: Math.max(h, size) };
}

/**
 * imageBoxSize is the size an image overlay is drawn at: the explicit
 * width × height, one of them derived from the asset's aspect when 0, or
 * the asset's natural size. Unknown (no asset) is a 64×64 placeholder.
 */
export function imageBoxSize(o: Pick<ImageOverlayCfg, 'asset' | 'width' | 'height'>): { w: number; h: number } {
  const sw = o.asset?.info.width ?? 0;
  const sh = o.asset?.info.height ?? 0;
  if (sw <= 0 || sh <= 0) return { w: o.width > 0 ? o.width : 64, h: o.height > 0 ? o.height : 64 };
  return fitSize(sw, sh, o.width, o.height, 'exact');
}

/** overlaySize is the footprint of either overlay kind. */
export function overlaySize(o: OverlayCfg): { w: number; h: number } {
  return o.kind === 'text' ? textBoxSize(o) : imageBoxSize(o);
}

/** overlayBox is the drag box of an overlay on the output canvas. */
export function overlayBox(o: OverlayCfg): Box {
  const { w, h } = overlaySize(o);
  const { left, top } = anchorOrigin(o.anchor, o.x, o.y, w, h);
  return { left, top, w, h };
}

/**
 * clampBoxInside keeps at least `margin` px of a box visible on a canvasW ×
 * canvasH canvas (so a dragged overlay can always be grabbed again) and
 * returns the adjusted top-left corner.
 */
export function clampBoxInside(box: Box, canvasW: number, canvasH: number, margin = 8): { left: number; top: number } {
  const m = Math.min(margin, box.w, box.h);
  return {
    left: Math.min(Math.max(box.left, m - box.w), canvasW - m),
    top: Math.min(Math.max(box.top, m - box.h), canvasH - m),
  };
}

/**
 * displayScale is the canvas px per display px of the still on the stage,
 * per axis: the fixed zooms give the image an explicit width, and a
 * stylesheet max-height (or any other constraint) can then squash it
 * vertically, so the two axes may differ and a drag must map each one by
 * its own ratio. An axis with no display size (0) maps 1:1.
 */
export function displayScale(canvasW: number, canvasH: number, displayW: number, displayH: number): { x: number; y: number } {
  return { x: displayW > 0 ? canvasW / displayW : 1, y: displayH > 0 ? canvasH / displayH : 1 };
}

/**
 * selectionAfterToggle is the preview selection (app.ui.selectedOverlay)
 * after a card's open state changed — the cards mirror open / collapse
 * transitions only: expanding a card selects its overlay; collapsing a card
 * that was open clears the selection, but only while it is still that
 * overlay's. A card mounting collapsed (wasOpen false) leaves the selection
 * alone, so addOverlay's selection and a box clicked or dragged on the
 * preview are never reset by the collapsed cards.
 */
export function selectionAfterToggle(selected: number, id: number, open: boolean, wasOpen: boolean): number {
  if (open) return id;
  if (wasOpen && selected === id) return 0;
  return selected;
}

/**
 * pickerColor is the colour a card stores when <input type="color"> (which
 * only handles RRGGBB) picks `picked` for a field currently holding
 * `previous`: the picked RGB with the alpha suffix of the previous value,
 * if it had one, so an RRGGBBAA colour keeps its alpha across picks.
 */
export function pickerColor(picked: string, previous: string): string {
  return picked.trim().replace(/^#/, '').toLowerCase() + previous.slice(6);
}

/**
 * ACTIVE_TOLERANCE mirrors graph.enableTolerance (internal/graph/phase3.go)
 * and must stay equal to it: the render gates a timed overlay with
 * enable='gte(t+0.0001,S)*lt(t+0.0001,E)' on ffmpeg's double frame time,
 * so a bound rounded to the nearest µs that lands just above its frame
 * (2/30 → 0.066667 at frame 2's 0.0666666…) still starts there, and an end
 * of 5/30 → 0.166667 still excludes frame 5. 1e-4 s is far below any frame
 * period (1/60 s at the fps cap), so no neighbouring frame is affected.
 */
export const ACTIVE_TOLERANCE = 1e-4;

/**
 * activeAt reports whether an overlay with the given time range is shown at
 * output time t the way the render decides it: start <= t + tolerance, and
 * t + tolerance < end unless end is 0 (to the end) — see ACTIVE_TOLERANCE.
 */
export function activeAt(o: Pick<OverlayCfg, 'start' | 'end'>, t: number): boolean {
  const tt = t + ACTIVE_TOLERANCE;
  if (tt < Math.max(0, o.start)) return false;
  return !(o.end > 0) || tt < o.end;
}

/**
 * scrubberRange is the [start, end) window the time-range "from scrubber"
 * buttons store for 0-based plan frame i (output seconds, fps = the plan
 * rate, total = the plan's frame count): start = where the frame starts,
 * end = where the next one starts, so the frame under the scrubber is the
 * first / the last one drawn; on the last frame end is 0 (to the end). Both
 * are floored to whole µs (format.floorTime), never rounded to the nearest:
 * the graph gates the overlay with gte(t+T,start)*lt(t+T,end) on ffmpeg's
 * double frame time (T = ACTIVE_TOLERANCE absorbs the µs rounding), and a
 * bound kept at or below the frame it names can never start the overlay a
 * frame late, end it a frame late, or draw nothing for a single-frame
 * window, whatever the tolerance. activeAt(range, frameStart(i, fps)) is
 * therefore true and frameAt(range.start, fps) - 1 is i ("go to start"
 * lands on the scrubber's frame). { 0, 0 } when the rate is unknown.
 */
export function scrubberRange(i: number, fps: number, total: number): { start: number; end: number } {
  if (!(fps > 0)) return { start: 0, end: 0 };
  const idx = Math.max(0, Math.round(i));
  const start = floorTime(frameStart(idx, fps));
  const end = total > 0 && idx >= total - 1 ? 0 : floorTime((idx + 1) / fps);
  return { start, end };
}

/**
 * scrubberStart is the Start "from scrubber" button: the overlay starts at
 * plan frame i (scrubberRange), and an End that is no longer after that is
 * cleared (to the end).
 */
export function scrubberStart(o: Pick<OverlayCfg, 'start' | 'end'>, i: number, fps: number, total: number): void {
  const s = scrubberRange(i, fps, total).start;
  o.start = s;
  if (o.end > 0 && o.end <= s) o.end = 0;
}

/**
 * scrubberEnd is the End "from scrubber" button: plan frame i becomes the
 * last one drawn (scrubberRange; the last frame = to the end). Refused —
 * false, nothing changed — when that end would sit at or before Start.
 */
export function scrubberEnd(o: Pick<OverlayCfg, 'start' | 'end'>, i: number, fps: number, total: number): boolean {
  const e = scrubberRange(i, fps, total).end;
  if (e > 0 && e <= o.start) return false;
  o.end = e;
  return true;
}

/** overlayLabel is the short name shown on the drag box and in the card header. */
export function overlayLabel(o: OverlayCfg): string {
  if (o.kind === 'text') {
    const first = o.text.split('\n')[0].trim();
    return first ? (first.length > 24 ? `${first.slice(0, 24)}…` : first) : 'Text';
  }
  return o.asset ? o.asset.name : 'Image';
}

/** isFontName mirrors the graph's rule for recipe.TextParams.Font: letters, digits, spaces and hyphens only. */
export function isFontName(s: string): boolean {
  return /^[A-Za-z0-9 -]+$/.test(s);
}
