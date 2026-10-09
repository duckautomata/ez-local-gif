// Pure maths of the guided model's Select subject panel (Phase 5c Part B):
// the prompts the user draws on the SOURCE-frame still — one box and any
// number of +/− points per prompted frame — kept normalised to 0..1 of the
// source frame (so a resize of the still or the window never moves them),
// keyed by the OUTPUT frame index on the matte plan's forward grid (the
// scrubber slot folded through forwardFrame). The helpers never mutate
// their input; the serialiser (toWirePrompts) renders what the recipe's
// matte op and POST /api/matte/prompt carry. No Svelte, no DOM
// (prompts.test.ts).
//
// Phase 5d: a frame may be a MASK prompt instead of (or on top of) a box
// and clicks — "Use this frame's matte": the edge model's per-frame matte
// of that frame is the tracker's starting mask (FramePrompts.maskFrom,
// MASK_FROM_EDGE). It anchors the set like a box, one frame per set
// carries it (the graph refuses a second), and the recipe carries only
// the source word, never the mask bytes.

import { MATTE_PROMPT_MASK_EDGE, type MattePrompt, type TrackPrompts } from './api';
import type { Rect } from './croprect';
import { clamp } from './format';

/** A +/− click: x / y in 0..1 of the source frame, label 1 = keep, 0 = remove. */
export interface PromptPoint {
  x: number;
  y: number;
  label: 0 | 1;
}

/** NormBox is a box in 0..1 of the source frame: [x0, y0, x1, y1] with x0 < x1, y0 < y1. */
export type NormBox = [number, number, number, number];

/** MASK_FROM_EDGE: the one mask source (recipe.MattePromptMaskEdge) — the edge model's per-frame matte of the prompted frame. */
export const MASK_FROM_EDGE = MATTE_PROMPT_MASK_EDGE;
/** MaskFrom is where a frame's mask prompt comes from (only the edge model's matte, so far). */
export type MaskFrom = typeof MASK_FROM_EDGE;

/** The prompts of one frame (BackgroundCfg.ai.prompts holds one per prompted frame). */
export interface FramePrompts {
  /** the OUTPUT frame index on the matte plan's forward grid */
  frame: number;
  box: NormBox | null;
  points: PromptPoint[];
  /** Phase 5d: the frame is a mask prompt — the edge model's matte of it starts the track (absent / undefined = none) */
  maskFrom?: MaskFrom;
}

/** MAX_PROMPT_FRAMES mirrors the graph's cap on MatteParams.Prompts (≤ 32 prompted frames). */
export const MAX_PROMPT_FRAMES = 32;
/** PROMPT_DECIMALS: coordinates are rounded to 1/10000 of the frame on the wire (matte.promptDecimals). */
export const PROMPT_DECIMALS = 4;

/** round4 rounds a 0..1 coordinate to PROMPT_DECIMALS and folds −0 (what the wire and the key carry). */
export function round4(v: number): number {
  const r = Math.round(v * 1e4) / 1e4;
  return r === 0 ? 0 : r;
}

/** unit clamps a coordinate into 0..1 (NaN → 0). */
function unit(v: number): number {
  return Number.isFinite(v) ? clamp(v, 0, 1) : 0;
}

/**
 * normBox turns a crop-style rectangle in SOURCE pixels (lib/croprect: x,
 * y, w, h) into a normalised box; null when it is degenerate (under one
 * source pixel on either side, or no frame size).
 */
export function normBox(r: Rect, srcW: number, srcH: number): NormBox | null {
  if (!(srcW > 0) || !(srcH > 0) || !(r.w >= 1) || !(r.h >= 1)) return null;
  const x0 = unit(r.x / srcW);
  const y0 = unit(r.y / srcH);
  const x1 = unit((r.x + r.w) / srcW);
  const y1 = unit((r.y + r.h) / srcH);
  if (!(x0 < x1) || !(y0 < y1)) return null;
  return [x0, y0, x1, y1];
}

/** boxRect is the inverse of normBox: the box as an integer source-pixel rectangle (at least 1×1). */
export function boxRect(b: NormBox, srcW: number, srcH: number): Rect {
  const x = Math.round(b[0] * srcW);
  const y = Math.round(b[1] * srcH);
  const w = Math.max(1, Math.round(b[2] * srcW) - x);
  const h = Math.max(1, Math.round(b[3] * srcH) - y);
  return { x, y, w, h };
}

/** normPoint turns a source-pixel position into a normalised point (clamped into the frame). */
export function normPoint(px: number, py: number, srcW: number, srcH: number, label: 0 | 1): PromptPoint {
  if (!(srcW > 0) || !(srcH > 0)) return { x: 0, y: 0, label };
  return { x: unit(px / srcW), y: unit(py / srcH), label };
}

/** framePrompts finds the entry of a frame (null when it has none). */
export function framePrompts(prompts: readonly FramePrompts[], frame: number): FramePrompts | null {
  return prompts.find((p) => p.frame === frame) ?? null;
}

/** hasMask: the frame is a mask prompt (Phase 5d). */
export function hasMask(fp: Pick<FramePrompts, 'maskFrom'> | null | undefined): boolean {
  return !!fp && fp.maskFrom === MASK_FROM_EDGE;
}

/** maskFrame is the frame carrying the set's mask prompt (−1 for none; at most one by construction, the lowest frame otherwise). */
export function maskFrame(prompts: readonly FramePrompts[]): number {
  const m = [...prompts].filter(hasMask).sort((a, b) => a.frame - b.frame)[0];
  return m ? m.frame : -1;
}

/** anchored: the frame's prompts can select something — a box, a positive point or a mask. */
export function anchored(fp: Pick<FramePrompts, 'box' | 'points' | 'maskFrom'> | null | undefined): boolean {
  return !!fp && (fp.box !== null || hasMask(fp) || fp.points.some((p) => p.label === 1));
}

/** promptsValid: the set has at least one anchored frame (what the server needs: ≥ 1 box, positive point or mask). */
export function promptsValid(prompts: readonly FramePrompts[]): boolean {
  return prompts.some(anchored);
}

/** isEmptyFrame: nothing drawn on the frame and no mask (it is dropped from the list). */
function isEmptyFrame(fp: FramePrompts): boolean {
  return fp.box === null && fp.points.length === 0 && !hasMask(fp);
}

/** copyFrame is a detached copy of an entry (the mask word only when set, so entries without one keep their shape). */
function copyFrame(fp: FramePrompts): FramePrompts {
  const out: FramePrompts = { frame: fp.frame, box: fp.box ? [...fp.box] : null, points: fp.points.map((p) => ({ ...p })) };
  if (hasMask(fp)) out.maskFrom = MASK_FROM_EDGE;
  return out;
}

/** withFrame replaces (or appends, frame-sorted) the entry of a frame, dropping it when empty. */
function withFrame(prompts: readonly FramePrompts[], fp: FramePrompts): FramePrompts[] {
  const rest = prompts.filter((p) => p.frame !== fp.frame);
  if (isEmptyFrame(fp)) return rest;
  rest.push(copyFrame(fp));
  rest.sort((a, b) => a.frame - b.frame);
  return rest;
}

/** canPromptFrame: a prompt may be added on this frame — it already has one, or the cap (MAX_PROMPT_FRAMES) leaves room. */
export function canPromptFrame(prompts: readonly FramePrompts[], frame: number): boolean {
  return framePrompts(prompts, frame) !== null || prompts.length < MAX_PROMPT_FRAMES;
}

/** setFrameBox sets (or clears, null) the box of a frame; a mask on the frame stays (the box refines it). Unchanged when the cap forbids a new frame. */
export function setFrameBox(prompts: readonly FramePrompts[], frame: number, box: NormBox | null): FramePrompts[] {
  if (box && !canPromptFrame(prompts, frame)) return [...prompts];
  const cur = framePrompts(prompts, frame);
  return withFrame(prompts, { frame, box, points: cur?.points ?? [], maskFrom: cur?.maskFrom });
}

/** addFramePoint appends a +/− point to a frame (a mask on it stays: the clicks refine it). Unchanged when the cap forbids a new frame. */
export function addFramePoint(prompts: readonly FramePrompts[], frame: number, point: PromptPoint): FramePrompts[] {
  if (!canPromptFrame(prompts, frame)) return [...prompts];
  const cur = framePrompts(prompts, frame);
  return withFrame(prompts, { frame, box: cur?.box ?? null, points: [...(cur?.points ?? []), point], maskFrom: cur?.maskFrom });
}

/** removeFramePoint drops point i of a frame (a no-op for an unknown index). */
export function removeFramePoint(prompts: readonly FramePrompts[], frame: number, i: number): FramePrompts[] {
  const cur = framePrompts(prompts, frame);
  if (!cur || i < 0 || i >= cur.points.length) return [...prompts];
  return withFrame(prompts, { frame, box: cur.box, points: cur.points.filter((_, j) => j !== i), maskFrom: cur.maskFrom });
}

/**
 * setFrameMask makes a frame the set's mask prompt ("Use this frame's
 * matte", Phase 5d) or takes the mask off it (null). Setting it REPLACES
 * the box and the points of that frame (the matte of a frame the edge
 * model got right is the whole selection; − clicks added afterwards
 * refine it) and takes the mask off any other frame — one mask per set,
 * as the graph demands — which is dropped when nothing else is on it.
 * Clearing keeps the frame's box and points. Unchanged when the cap
 * forbids a new frame.
 */
export function setFrameMask(prompts: readonly FramePrompts[], frame: number, from: MaskFrom | null): FramePrompts[] {
  if (from === null) {
    const cur = framePrompts(prompts, frame);
    if (!cur || !hasMask(cur)) return [...prompts];
    return withFrame(prompts, { frame, box: cur.box, points: cur.points });
  }
  if (!canPromptFrame(prompts, frame)) return [...prompts];
  let out: FramePrompts[] = [];
  for (const p of prompts) {
    if (p.frame === frame) continue;
    out = withFrame(out, hasMask(p) ? { frame: p.frame, box: p.box, points: p.points } : p);
  }
  return withFrame(out, { frame, box: null, points: [], maskFrom: from });
}

/** clearFrame drops everything drawn on a frame. */
export function clearFrame(prompts: readonly FramePrompts[], frame: number): FramePrompts[] {
  return prompts.filter((p) => p.frame !== frame);
}

/** One row of the keyframe strip. */
export interface Keyframe {
  frame: number;
  box: boolean;
  /** Phase 5d: the frame is the set's mask prompt (the strip shows ▣) */
  mask: boolean;
  positive: number;
  negative: number;
  /** the frame can select something on its own (a box, a + point or the mask) */
  anchored: boolean;
}

/** keyframes lists the prompted frames in frame order with what each carries. */
export function keyframes(prompts: readonly FramePrompts[]): Keyframe[] {
  return [...prompts]
    .filter((p) => !isEmptyFrame(p))
    .sort((a, b) => a.frame - b.frame)
    .map((p) => ({
      frame: p.frame,
      box: p.box !== null,
      mask: hasMask(p),
      positive: p.points.filter((q) => q.label === 1).length,
      negative: p.points.filter((q) => q.label === 0).length,
      anchored: anchored(p),
    }));
}

/**
 * hitPoint finds the marker under a normalised position: the nearest point
 * within tolX / tolY (per axis, in 0..1 units — the overlay converts ~8
 * display px). −1 for none.
 */
export function hitPoint(points: readonly PromptPoint[], x: number, y: number, tolX: number, tolY: number): number {
  let best = -1;
  let bestD = Infinity;
  points.forEach((p, i) => {
    const dx = Math.abs(p.x - x);
    const dy = Math.abs(p.y - y);
    if (dx > tolX || dy > tolY) return;
    const d = (dx / (tolX || 1)) ** 2 + (dy / (tolY || 1)) ** 2;
    if (d < bestD) {
      bestD = d;
      best = i;
    }
  });
  return best;
}

/**
 * toMattePrompt renders one frame's prompts as the recipe's MattePrompt
 * (rounded; an empty frame → null): `{frame, maskFrom: "edge"}` for a mask
 * prompt (plus its refining points / box) — the source word only, the
 * server takes the matte from its memo.
 */
export function toMattePrompt(fp: FramePrompts): MattePrompt | null {
  if (isEmptyFrame(fp)) return null;
  const out: MattePrompt = { frame: Math.max(0, Math.round(fp.frame)) };
  if (hasMask(fp)) out.maskFrom = MASK_FROM_EDGE;
  if (fp.points.length) out.points = fp.points.map((p) => [round4(unit(p.x)), round4(unit(p.y)), p.label === 1 ? 1 : 0]);
  if (fp.box) out.box = [round4(unit(fp.box[0])), round4(unit(fp.box[1])), round4(unit(fp.box[2])), round4(unit(fp.box[3]))];
  return out;
}

/**
 * toMattePrompts renders the set for the recipe's matte op: frame-sorted,
 * rounded, empty frames dropped; empty when the set cannot select anything
 * (promptsValid) — the op then carries no prompts and the serialiser emits
 * no matte op at all.
 */
export function toMattePrompts(prompts: readonly FramePrompts[]): MattePrompt[] {
  if (!promptsValid(prompts)) return [];
  return [...prompts]
    .sort((a, b) => a.frame - b.frame)
    .map(toMattePrompt)
    .filter((p): p is MattePrompt => p !== null);
}

/** toWirePrompts wraps the set as matte.TrackPrompts (obj 1) for POST /api/matte/prompt. */
export function toWirePrompts(prompts: readonly FramePrompts[]): TrackPrompts {
  return { obj: 1, prompts: toMattePrompts(prompts) };
}

/**
 * wireForFrame is the live overlay's request body part: the prompts of ONE
 * frame (matte.TrackPrompts.ForFrame), null when that frame cannot select
 * anything by itself (a frame of − clicks alone: the sidecar would 400, so
 * nothing is asked and the overlay clears). A mask prompt goes on the wire
 * with both `maskFrom` (the recipe word) and `mask: true` (matte.
 * FramePrompt's own flag), so the server sees the mask whichever shape it
 * decodes the prompts in; the recipe op (toMattePrompts) carries only
 * `maskFrom`.
 */
export function wireForFrame(prompts: readonly FramePrompts[], frame: number): TrackPrompts | null {
  const fp = framePrompts(prompts, frame);
  if (!anchored(fp)) return null;
  const p = toMattePrompt(fp!);
  if (!p) return null;
  if (p.maskFrom) p.mask = true;
  return { obj: 1, prompts: [p] };
}

/** promptsKey is a canonical text of the wire prompts (the overlay's memo key; order-independent for points; ":m<source>" names a mask prompt). */
export function promptsKey(p: TrackPrompts | null): string {
  if (!p) return '';
  return p.prompts
    .map((fp) => {
      const pts = [...(fp.points ?? [])].sort((a, b) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2]).map((q) => q.join(',')).join(';');
      const mask = fp.maskFrom ? `:m${fp.maskFrom}` : '';
      return `f${fp.frame}:b${fp.box ? fp.box.join(',') : '-'}:p${pts}${mask}`;
    })
    .join('|');
}

/**
 * promptSummary words the set for the card's summary line: "select the
 * subject" with nothing drawn, "1 frame · box", "3 frames · 2 boxes · 4
 * points", "1 frame · frame matte" for a mask prompt; the anchoring caveat
 * when only − clicks exist.
 */
export function promptSummary(prompts: readonly FramePrompts[]): string {
  const ks = keyframes(prompts);
  if (!ks.length) return 'select the subject';
  const boxes = ks.filter((k) => k.box).length;
  const masks = ks.filter((k) => k.mask).length;
  const pts = ks.reduce((n, k) => n + k.positive + k.negative, 0);
  const parts = [`${ks.length} ${ks.length === 1 ? 'frame' : 'frames'}`];
  if (masks) parts.push('frame matte');
  if (boxes) parts.push(`${boxes} ${boxes === 1 ? 'box' : 'boxes'}`);
  if (pts) parts.push(`${pts} ${pts === 1 ? 'point' : 'points'}`);
  if (!promptsValid(prompts)) parts.push('needs a box or a + click');
  return parts.join(' · ');
}
