// Pure helpers for the Result card: grouping the manifest's files by kind and
// describing them. Framework-free (unit-tested in result.test.ts).

import { matteParamsOf, MATTE_EDGE_NONE, type Check, type Recipe, type Report, type ResultFile, type Target } from './api';

/** RULE_RENDER_MATTE is jobs' info check naming the AI matte a render used (Phase 5b). */
export const RULE_RENDER_MATTE = 'render.matte';

/**
 * matteCheck finds the `render.matte` info check of a report — "AI matte:
 * isnet-anime fp16 1024², weights f15622d8…, 45 mattes, fill pinholes" —
 * so the Result card can say which model produced the file at a glance
 * (the check also lists with the others). null without one.
 */
export function matteCheck(report: Report | null | undefined): Check | null {
  return report?.checks?.find((c) => c.rule === RULE_RENDER_MATTE) ?? null;
}

/**
 * matteSummary is the Result card's "AI matte:" line — the check's detail
 * without a leading "AI matte:" of its own (jobs may word it either way),
 * or null when the report has no render.matte check.
 */
export function matteSummary(report: Report | null | undefined): string | null {
  const c = matteCheck(report);
  if (!c) return null;
  const text = c.detail.replace(/^\s*AI matte\s*:\s*/i, '').trim();
  return text || 'see the Discord checks';
}

/**
 * matteRecipeNotes words what the recipe's matte op asked for beyond the
 * model (Phase 5c) — "guided (sam2-tiny) + edge birefnet-lite · 3
 * prompted frames", "stabilise light", "2 keep colours" — so the Result
 * card's line names them even when a server's render.matte detail does
 * not. Empty without a matte op or with nothing to add.
 */
export function matteRecipeNotes(recipe: Pick<Recipe, 'ops'> | null | undefined): string[] {
  const p = matteParamsOf(recipe?.ops);
  if (!p) return [];
  const out: string[] = [];
  const prompts = Array.isArray(p.prompts) ? p.prompts : [];
  if (prompts.length) {
    const edge = typeof p.edge === 'string' ? p.edge.trim() : '';
    const resolvedEdge = p.resolved?.edge?.trim() ?? '';
    const edgeText = edge === MATTE_EDGE_NONE ? 'no edge model' : `edge ${resolvedEdge || edge || 'default'}`;
    out.push(`guided (${p.model || 'sam2-tiny'}) + ${edgeText} · ${prompts.length} prompted ${prompts.length === 1 ? 'frame' : 'frames'}`);
  }
  if (typeof p.stabilise === 'string' && p.stabilise.trim()) out.push(`stabilise ${p.stabilise.trim()}`);
  const keep = Array.isArray(p.keep) ? p.keep.filter((k) => typeof k === 'string' && k !== '') : [];
  if (keep.length) out.push(`${keep.length} keep ${keep.length === 1 ? 'colour' : 'colours'}`);
  return out;
}

/**
 * matteLine is the Result card's whole "AI matte:" line: the render.matte
 * detail, followed by the recipe notes the detail does not already mention
 * (a word-wise check: "stabilise", "keep", "guided"). null without a
 * render.matte check AND without a matte op.
 */
export function matteLine(report: Report | null | undefined, recipe: Pick<Recipe, 'ops'> | null | undefined): string | null {
  const summary = matteSummary(report);
  const notes = matteRecipeNotes(recipe);
  if (summary === null && !notes.length) return null;
  const have = (summary ?? '').toLowerCase();
  const extra = notes.filter((n) => {
    const word = n.startsWith('guided') ? 'guided' : n.startsWith('stabilise') ? 'stabilise' : 'keep';
    return !have.includes(word);
  });
  return [summary ?? '', ...extra].filter((s) => s !== '').join(' · ');
}

export interface FileGroups {
  /** the main output (kind "" / "output"); null for a frames-only result */
  primary: ResultFile | null;
  /** fit-search runner-ups, by rank, at most MAX_ALTERNATIVES */
  alternatives: ResultFile[];
  /** extracted frames in frame order */
  frames: ResultFile[];
  /** frames.zip (or any archive) */
  archive: ResultFile | null;
  /** anything with an unknown kind — shown like an extra output so nothing is hidden */
  others: ResultFile[];
}

/** The fit engine reports at most two runner-ups (DESIGN §5.4). */
export const MAX_ALTERNATIVES = 2;

/** groupFiles sorts a manifest's files into primary / alternatives / frames / archive. */
export function groupFiles(files: readonly ResultFile[] | null | undefined): FileGroups {
  const g: FileGroups = { primary: null, alternatives: [], frames: [], archive: null, others: [] };
  for (const f of files ?? []) {
    switch (f.kind ?? '') {
      case '':
      case 'output':
        if (g.primary) g.others.push(f);
        else g.primary = f;
        break;
      case 'alternative':
        g.alternatives.push(f);
        break;
      case 'frame':
        g.frames.push(f);
        break;
      case 'archive':
        if (g.archive) g.others.push(f);
        else g.archive = f;
        break;
      default:
        g.others.push(f);
    }
  }
  g.alternatives.sort((a, b) => (a.index ?? 0) - (b.index ?? 0) || a.bytes - b.bytes);
  g.alternatives = g.alternatives.slice(0, MAX_ALTERNATIVES);
  g.frames.sort((a, b) => (a.index ?? 0) - (b.index ?? 0) || a.name.localeCompare(b.name));
  return g;
}

/** isFramesResult: the manifest is a frame extraction (frames and/or an archive, no primary output). */
export function isFramesResult(g: FileGroups): boolean {
  return !g.primary && (g.frames.length > 0 || g.archive !== null);
}

/** DescLine is the primary's one-line description with the label it renders under. */
export interface DescLine {
  /** 'Fit' only when the recipe actually carried a fit budget; 'Settings' otherwise */
  label: 'Fit' | 'Settings';
  text: string;
}

/**
 * descLine is the one-line description of the primary file. Only when the
 * recipe actually ran a fit search (`fitRan`: fitBytes > 0 on a fit-capable
 * format) does the desc read like a fit report ("fit at 20 fps · 128
 * colours · lossy 60" / "cannot fit under 256 KiB: …") and get the 'Fit'
 * label; any other desc (e.g. Optimize's "gifsicle: lossy 30 · 256 colours")
 * is plain settings. null when there is no desc.
 */
export function descLine(f: ResultFile | null, fitRan: boolean): DescLine | null {
  const text = f?.desc?.trim() ?? '';
  if (!text) return null;
  return { label: fitRan ? 'Fit' : 'Settings', text };
}

/** sizeState classifies a file against its Discord limit: 'ok' | 'bad' | '' (no limit). */
export function sizeState(f: Pick<ResultFile, 'bytes' | 'limit'>): '' | 'ok' | 'bad' {
  if (f.limit <= 0) return '';
  return f.bytes <= f.limit ? 'ok' : 'bad';
}

export interface ChatSize {
  px: number;
  label: string;
}

/**
 * chatSizes lists the "as seen in chat" thumbnail sizes for a Discord target
 * (DESIGN §5.1: emoji inline ~22 px, jumbo 48 px; stickers 160 px). Empty
 * for attachments / no target.
 */
export function chatSizes(target: Target | string | undefined): ChatSize[] {
  switch (target) {
    case 'emote':
      return [
        { px: 22, label: 'inline 22 px' },
        { px: 48, label: 'jumbo 48 px' },
      ];
    case 'sticker':
      return [{ px: 160, label: 'sticker 160 px' }];
    default:
      return [];
  }
}

/** isImageFormat: formats a browser <img> can show (everything the encoders produce except archives). */
export function isImageFormat(format: string): boolean {
  switch (format) {
    case 'gif':
    case 'webp':
    case 'apng':
    case 'png':
    case 'avif':
    case 'jpeg':
    case 'jpg':
      return true;
    default:
      return false;
  }
}
