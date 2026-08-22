// Font families for the Text overlay card: GET /api/fonts (fc-list inside
// the container) reduced to unique family names; the documented default
// (DejaVu Sans, what the graph uses for an empty font) is always offered,
// also when the endpoint is empty or unreachable.

import { getFonts, type Font } from './api';
import { isFontName, TEXT_DEFAULTS } from './overlay';

export const DEFAULT_FONT = TEXT_DEFAULTS.font;

/**
 * fontFamilies reduces the server's faces to unique, drawtext-safe family
 * names (letters, digits, spaces, hyphens — what recipe.TextParams.Font
 * accepts), sorted, with DEFAULT_FONT first. Never empty.
 */
export function fontFamilies(fonts: readonly Font[] | null | undefined): string[] {
  const set = new Set<string>();
  for (const f of fonts ?? []) {
    const name = f.family.trim();
    if (name && isFontName(name)) set.add(name);
  }
  set.delete(DEFAULT_FONT);
  return [DEFAULT_FONT, ...[...set].sort((a, b) => a.localeCompare(b))];
}

let cached: Promise<string[]> | null = null;

/**
 * loadFontFamilies fetches the list once per page (cached; a failed fetch is
 * not cached, so a server that comes up later is asked again). Never rejects:
 * any error yields the default-only list.
 */
export function loadFontFamilies(fetcher: (signal?: AbortSignal) => Promise<Font[]> = getFonts): Promise<string[]> {
  if (cached) return cached;
  const p = fetcher()
    .then((fonts) => fontFamilies(fonts))
    .catch(() => {
      cached = null;
      return fontFamilies([]);
    });
  cached = p;
  return p;
}

/** resetFontCache forgets the cached list (tests). */
export function resetFontCache(): void {
  cached = null;
}
