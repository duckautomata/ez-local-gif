// Phase 4 keyboard shortcuts: the pure key → action mapping, the backdrop
// cycle and the Play-toggle registry.
import { describe, expect, it } from 'vitest';
import { cycleBackdrop, isEditableTarget, isInteractiveTarget, registerPlayToggle, SHORTCUT_LIST, shortcutFor, togglePlay, trapTabTarget } from './shortcuts';

const key = (k: string, mods: { ctrlKey?: boolean; metaKey?: boolean; altKey?: boolean; shiftKey?: boolean } = {}) => ({ key: k, ...mods });

describe('shortcutFor', () => {
  it('maps Ctrl/Cmd+Enter to render — inputs included (submit semantics)', () => {
    expect(shortcutFor(key('Enter', { ctrlKey: true }))).toBe('render');
    expect(shortcutFor(key('Enter', { metaKey: true }))).toBe('render');
    expect(shortcutFor(key('Enter', { ctrlKey: true }), { tagName: 'INPUT' })).toBe('render');
    expect(shortcutFor(key('Enter'))).toBeNull();
  });

  it('Space toggles play, but never while an interactive element has focus', () => {
    expect(shortcutFor(key(' '))).toBe('play');
    expect(shortcutFor(key(' '), { tagName: 'DIV' })).toBe('play');
    expect(shortcutFor(key(' '), { tagName: 'BODY' })).toBe('play');
    // typing and native Space behaviours win
    expect(shortcutFor(key(' '), { tagName: 'INPUT' })).toBeNull();
    expect(shortcutFor(key(' '), { tagName: 'TEXTAREA' })).toBeNull();
    expect(shortcutFor(key(' '), { tagName: 'SELECT' })).toBeNull();
    expect(shortcutFor(key(' '), { tagName: 'BUTTON' })).toBeNull();
    expect(shortcutFor(key(' '), { tagName: 'A' })).toBeNull();
    // a focused <video controls> (the Result card's MP4/WebM) pauses itself
    expect(shortcutFor(key(' '), { tagName: 'VIDEO' })).toBeNull();
    expect(shortcutFor(key(' '), { tagName: 'AUDIO' })).toBeNull();
    expect(shortcutFor(key(' '), { tagName: 'DIV', isContentEditable: true })).toBeNull();
    // the frame readout is a role="slider" span — Space must not steal it
    expect(shortcutFor(key(' '), { tagName: 'SPAN', role: 'slider' })).toBeNull();
  });

  it('B cycles the backdrop and ? opens help — but not while typing', () => {
    expect(shortcutFor(key('b'))).toBe('backdrop');
    expect(shortcutFor(key('B', { shiftKey: true }))).toBe('backdrop');
    expect(shortcutFor(key('b'), { tagName: 'INPUT' })).toBeNull();
    expect(shortcutFor(key('?'))).toBe('help');
    expect(shortcutFor(key('?'), { tagName: 'TEXTAREA' })).toBeNull();
    // buttons are fine for letters (they do not consume them)
    expect(shortcutFor(key('b'), { tagName: 'BUTTON' })).toBe('backdrop');
  });

  it('Escape maps to close; modifier chords are left to the browser', () => {
    expect(shortcutFor(key('Escape'))).toBe('close');
    expect(shortcutFor(key('b', { ctrlKey: true }))).toBeNull();
    expect(shortcutFor(key(' ', { altKey: true }))).toBeNull();
    expect(shortcutFor(key('x'))).toBeNull();
  });
});

describe('target classification', () => {
  it('editable: input/textarea/select/contenteditable; interactive adds button/a/summary/slider', () => {
    expect(isEditableTarget({ tagName: 'input' })).toBe(true); // case-insensitive
    expect(isEditableTarget({ tagName: 'BUTTON' })).toBe(false);
    expect(isEditableTarget(null)).toBe(false);
    expect(isInteractiveTarget({ tagName: 'BUTTON' })).toBe(true);
    expect(isInteractiveTarget({ tagName: 'SUMMARY' })).toBe(true);
    expect(isInteractiveTarget({ tagName: 'VIDEO' })).toBe(true);
    expect(isInteractiveTarget({ tagName: 'AUDIO' })).toBe(true);
    expect(isInteractiveTarget({ tagName: 'SPAN', role: 'slider' })).toBe(true);
    expect(isInteractiveTarget({ tagName: 'SPAN' })).toBe(false);
    expect(isInteractiveTarget(null)).toBe(false);
  });
});

describe('cycleBackdrop', () => {
  it('steps checker → dark → white → checker', () => {
    expect(cycleBackdrop('checker')).toBe('dark');
    expect(cycleBackdrop('dark')).toBe('white');
    expect(cycleBackdrop('white')).toBe('checker');
  });
});

describe('play-toggle registry', () => {
  it('togglePlay is inert without a registration and calls through with one', () => {
    registerPlayToggle(null);
    expect(togglePlay()).toBe(false);
    let n = 0;
    registerPlayToggle(() => n++);
    expect(togglePlay()).toBe(true);
    expect(n).toBe(1);
    registerPlayToggle(null);
    expect(togglePlay()).toBe(false);
    expect(n).toBe(1);
  });
});

// WEB-8: the "?" overlay is aria-modal, so Tab must loop within the sheet
// instead of walking the obscured page behind the backdrop.
describe('trapTabTarget (the "?" overlay focus loop)', () => {
  it('wraps Tab from the last item (and Shift+Tab from the first)', () => {
    expect(trapTabTarget(3, 2, false)).toBe(0);
    expect(trapTabTarget(3, 0, true)).toBe(2);
    expect(trapTabTarget(1, 0, false)).toBe(0); // a single item (the close button) keeps focus
    expect(trapTabTarget(1, 0, true)).toBe(0);
  });

  it('lets the default move run while it stays inside the dialog', () => {
    expect(trapTabTarget(3, 0, false)).toBeNull();
    expect(trapTabTarget(3, 1, false)).toBeNull();
    expect(trapTabTarget(3, 2, true)).toBeNull();
    expect(trapTabTarget(3, 1, true)).toBeNull();
  });

  it('pulls focus from outside the items (the dialog itself, or behind the backdrop) into the loop', () => {
    expect(trapTabTarget(3, -1, false)).toBe(0);
    expect(trapTabTarget(3, -1, true)).toBe(2);
  });

  it('has nowhere to send focus with no focusable items', () => {
    expect(trapTabTarget(0, -1, false)).toBeNull();
    expect(trapTabTarget(0, -1, true)).toBeNull();
  });
});

describe('SHORTCUT_LIST', () => {
  it('documents every binding the handlers implement', () => {
    const all = SHORTCUT_LIST.map((s) => s.keys.join('+')).join(' ');
    for (const k of ['Ctrl+Enter', 'Space', 'B', 'Esc', '?']) expect(all).toContain(k);
  });
});
