// Global keyboard shortcuts (DESIGN §7, Phase 4): the pure key → action
// mapping App.svelte's window handler uses, the backdrop cycle, and the
// binding list the "?" help overlay renders. Framework-free
// (shortcuts.test.ts); the Play toggle itself is registered by the Preview
// while it is mounted (there is nothing to play in batch mode or before a
// source is loaded).

import type { Backdrop } from './state.svelte';

export type ShortcutAction = 'render' | 'play' | 'backdrop' | 'help' | 'close';

/** Minimal KeyboardEvent shape (tests need no DOM). */
export interface KeyLike {
  key: string;
  ctrlKey?: boolean;
  metaKey?: boolean;
  altKey?: boolean;
  shiftKey?: boolean;
}

/** Minimal event-target shape (tests need no DOM). */
export interface TargetLike {
  tagName?: string;
  isContentEditable?: boolean;
  /** the element's role attribute, if any */
  role?: string | null;
}

/** isEditableTarget: typing goes here — no single-letter shortcuts (B, ?). */
export function isEditableTarget(t: TargetLike | null | undefined): boolean {
  if (!t) return false;
  const tag = (t.tagName ?? '').toUpperCase();
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || t.isContentEditable === true;
}

/**
 * isInteractiveTarget: Space already means something here (activates a
 * button / link, toggles a checkbox, steps a slider like the scrubber
 * readout), so the play/stop shortcut must not steal it. A focused
 * <video controls> / <audio controls> owns Space too — its native
 * play/pause on the Result card's rendered MP4/WebM must not be replaced
 * by the source preview's toggle.
 */
export function isInteractiveTarget(t: TargetLike | null | undefined): boolean {
  if (isEditableTarget(t)) return true;
  const tag = ((t && t.tagName) ?? '').toUpperCase();
  if (tag === 'BUTTON' || tag === 'A' || tag === 'SUMMARY' || tag === 'VIDEO' || tag === 'AUDIO') return true;
  return (t && t.role) === 'slider';
}

/**
 * shortcutFor maps a keydown to a global action, or null when the key means
 * nothing (or belongs to the focused element):
 *
 *   Ctrl/Cmd+Enter  'render'   — always, inputs included (submit semantics)
 *   Space           'play'     — toggle the animated preview; never while an
 *                                interactive element has focus
 *   B               'backdrop' — cycle the preview backdrop; not while typing
 *   ?               'help'     — open the shortcuts overlay; not while typing
 *   Escape          'close'    — close an overlay (the caller decides which)
 *
 * Every other modifier combination is left to the browser.
 */
export function shortcutFor(e: KeyLike, target?: TargetLike | null): ShortcutAction | null {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') return 'render';
  if (e.ctrlKey || e.metaKey || e.altKey) return null;
  switch (e.key) {
    case ' ':
      return isInteractiveTarget(target) ? null : 'play';
    case 'b':
    case 'B':
      return isEditableTarget(target) ? null : 'backdrop';
    case '?':
      return isEditableTarget(target) ? null : 'help';
    case 'Escape':
      return 'close';
    default:
      return null;
  }
}

/**
 * trapTabTarget decides where a Tab press inside a modal dialog must move
 * focus so it keeps looping within the dialog (the "?" overlay sets
 * aria-modal, which promises an inert background — WEB-8). `count` is the
 * number of focusable items in the dialog and `index` the active element's
 * position among them (−1 = the active element is not one of them: the
 * dialog itself, or something behind the backdrop). Returns the index to
 * focus, or null when the browser's default move already stays inside.
 */
export function trapTabTarget(count: number, index: number, shiftKey: boolean): number | null {
  if (count <= 0) return null;
  if (index < 0 || index >= count) return shiftKey ? count - 1 : 0;
  if (shiftKey) return index === 0 ? count - 1 : null;
  return index === count - 1 ? 0 : null;
}

/** cycleBackdrop steps the preview backdrop: checker → dark → white → checker. */
export function cycleBackdrop(b: Backdrop): Backdrop {
  switch (b) {
    case 'checker':
      return 'dark';
    case 'dark':
      return 'white';
    default:
      return 'checker';
  }
}

// ---------------------------------------------------------------------------
// Play toggle registry: the Preview registers its play/stop while mounted;
// Space does nothing when nothing registered (no source, batch mode).

let playToggle: (() => void) | null = null;

/** registerPlayToggle installs (or, with null, removes) the Space handler. */
export function registerPlayToggle(fn: (() => void) | null): void {
  playToggle = fn;
}

/** togglePlay invokes the registered play/stop; false when none is mounted. */
export function togglePlay(): boolean {
  if (!playToggle) return false;
  playToggle();
  return true;
}

/** One row of the "?" help overlay. */
export interface ShortcutRow {
  keys: string[];
  what: string;
}

/** Everything the app binds, for the help overlay (keep in sync with the handlers). */
export const SHORTCUT_LIST: readonly ShortcutRow[] = [
  { keys: ['Ctrl', 'Enter'], what: 'Render (batch: render all)' },
  { keys: ['Space'], what: 'Play / stop the animated preview' },
  { keys: ['B'], what: 'Cycle the preview backdrop (checker → dark → white)' },
  { keys: ['←', '→'], what: 'Step one frame (on the scrubber / frame readout)' },
  { keys: ['Shift', '←/→'], what: 'Step ten frames' },
  { keys: ['Home', 'End'], what: 'First / last frame' },
  { keys: ['Esc'], what: 'Close this overlay / cancel the eyedropper' },
  { keys: ['?'], what: 'Show this overlay' },
];
