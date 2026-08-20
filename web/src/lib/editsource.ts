// "Edit as source": turn a rendered result file into a new source and open it
// in a new tab. The tab is opened synchronously (inside the click) so popup
// blockers allow it, then navigated once POST /api/sources/from-result has
// answered; on failure it is closed again. Framework-free for testing: the
// window-ish dependencies are injected.

import { messageOf, sourceFromResult, sourceURL, type ResultFile, type Source } from './api';

export interface OpenedTab {
  /** navigate the tab to url */
  go(url: string): void;
  /** close the tab (after a failure) */
  close(): void;
}

export interface EditSourceDeps {
  /** open a blank tab now; null when the browser blocked it */
  openTab(): OpenedTab | null;
  fromResult?: (recipeHash: string, name: string) => Promise<Source>;
}

export interface EditSourceOutcome {
  ok: boolean;
  /** the new source (on success) */
  source?: Source;
  /** the URL of the new tab / to show when the popup was blocked */
  url?: string;
  /** human error text (on failure) */
  error?: string;
  /** true when the browser refused to open the tab — the caller should show url */
  blocked?: boolean;
}

/** browserTabOpener uses window.open; the tab is same-origin so it can be navigated later. */
export function browserTabOpener(): () => OpenedTab | null {
  return () => {
    const w = window.open('', '_blank');
    if (!w) return null;
    return {
      go: (url) => {
        w.location.href = url;
      },
      close: () => w.close(),
    };
  };
}

/**
 * editAsSource registers `file` of result `recipeHash` as a source and opens
 * '/?src=<hash>' in a new tab. Never throws; the outcome says what happened.
 */
export async function editAsSource(recipeHash: string, file: Pick<ResultFile, 'name'>, deps: EditSourceDeps): Promise<EditSourceOutcome> {
  const tab = deps.openTab();
  const fromResult = deps.fromResult ?? sourceFromResult;
  let src: Source;
  try {
    src = await fromResult(recipeHash, file.name);
  } catch (e) {
    tab?.close();
    return { ok: false, error: messageOf(e) };
  }
  const url = sourceURL(src.hash);
  if (!tab) return { ok: true, source: src, url, blocked: true };
  tab.go(url);
  return { ok: true, source: src, url };
}
