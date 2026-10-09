// The live mask of the Select subject panel (Phase 5c Part B): after every
// box / click the guided model is asked for THAT frame's mask under THAT
// frame's prompts (POST /api/matte/prompt → a gray PNG) and the overlay
// paints it at 50 % green. Same discipline as the still scheduler
// (lib/still.ts): debounce, abort a superseded request, keep the last mask
// on screen until the next arrives, a 202 (the tracker loading) keeps the
// view pending and retries, errors are shown and cleared by the next
// answer. Framework-free (promptmask.test.ts); PromptOverlay.svelte hands
// it a $state object as the view.

import { isAbortError, isMattePending, messageOf, type MattePending, type PromptMaskRequest } from './api';
import { matteRetryMs, nextPending, type PendingView } from './matte';

export interface PromptMaskView {
  /** object URL of the mask on screen (null = none) */
  url: string | null;
  /** the frame the mask on screen belongs to */
  frame: number;
  loading: boolean;
  /** last failure for the current request ('' = none) */
  error: string;
  /** the tracker is loading / downloading (a 202): the overlay says so */
  pending: PendingView | null;
}

export interface PromptMaskDeps {
  fetch: (req: PromptMaskRequest, signal: AbortSignal) => Promise<Blob>;
  createURL: (blob: Blob) => string;
  revokeURL: (url: string) => void;
  /** debounce before a request is sent (default 150 ms) */
  debounceMs?: number;
  /** every 202 answer (the status it carries is installed, lib/matte.svelte notePending) */
  onPending?: (p: MattePending) => void;
}

/** PromptMaskScheduler turns a stream of mask requests into at most one in-flight fetch whose answer matches the latest. */
export class PromptMaskScheduler {
  private readonly view: PromptMaskView;
  private readonly deps: PromptMaskDeps;
  private readonly debounceMs: number;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private ctrl: AbortController | null = null;
  private current: PromptMaskRequest | null = null;
  private currentKey = '';
  /** key of the mask on screen */
  private shownKey = '';
  /** key whose failure produced view.error */
  private errorKey = '';

  constructor(view: PromptMaskView, deps: PromptMaskDeps) {
    this.view = view;
    this.deps = deps;
    this.debounceMs = deps.debounceMs ?? 150;
  }

  /** key is the identity of a request (the recipe, the frame and that frame's prompts). */
  static key(r: PromptMaskRequest | null): string {
    return r ? JSON.stringify(r) : '';
  }

  /** displayedKey is the key of the mask on screen ('' = none): equal to key(current) exactly when view.url answers the latest request. */
  get displayedKey(): string {
    return this.shownKey;
  }

  /**
   * request is called with every change: null clears the mask (nothing to
   * ask — the frame has no anchoring prompt), a request identical to the
   * shown mask's is a no-op, anything else is fetched after the debounce.
   */
  request(r: PromptMaskRequest | null): void {
    this.current = r;
    this.currentKey = PromptMaskScheduler.key(r);
    this.clearTimers();
    if (!r) {
      this.abort();
      this.shownKey = '';
      this.clearError();
      this.view.pending = null;
      this.swapUrl(null);
      return;
    }
    if (this.currentKey === this.shownKey) {
      this.abort();
      if (this.errorKey !== this.currentKey) this.clearError();
      this.view.pending = null;
      return;
    }
    this.timer = setTimeout(() => void this.load(this.currentKey, r), this.debounceMs);
  }

  /** retry forgets the shown mask and the error and re-requests the current state. */
  retry(): void {
    this.clearError();
    this.view.pending = null;
    this.shownKey = '';
    if (this.current) this.request(this.current);
  }

  private async load(key: string, r: PromptMaskRequest): Promise<void> {
    this.ctrl?.abort();
    const c = new AbortController();
    this.ctrl = c;
    this.view.loading = true;
    try {
      const blob = await this.deps.fetch(r, c.signal);
      if (c.signal.aborted) return;
      this.shownKey = key;
      this.clearError();
      this.view.pending = null;
      this.view.frame = r.frame;
      this.swapUrl(this.deps.createURL(blob));
    } catch (e) {
      if (isAbortError(e) || c.signal.aborted) return;
      if (isMattePending(e)) {
        this.view.pending = nextPending(this.view.pending, e.info, Date.now());
        this.deps.onPending?.(e);
        this.retryTimer = setTimeout(() => {
          this.retryTimer = undefined;
          if (this.currentKey !== key) return;
          void this.load(key, r);
        }, matteRetryMs(e.state));
        return;
      }
      this.view.error = messageOf(e);
      this.errorKey = key;
      this.view.pending = null;
    } finally {
      if (this.ctrl === c) {
        this.view.loading = false;
        this.ctrl = null;
      }
    }
  }

  private clearTimers(): void {
    if (this.timer !== undefined) clearTimeout(this.timer);
    this.timer = undefined;
    if (this.retryTimer !== undefined) clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
  }

  private abort(): void {
    if (!this.ctrl) return;
    this.ctrl.abort();
    this.ctrl = null;
    this.view.loading = false;
  }

  private clearError(): void {
    this.view.error = '';
    this.errorKey = '';
  }

  private swapUrl(next: string | null): void {
    const prev = this.view.url;
    this.view.url = next;
    if (prev) this.deps.revokeURL(prev);
  }

  /** dispose cancels everything and releases the mask URL. */
  dispose(): void {
    this.clearTimers();
    this.abort();
    this.view.pending = null;
    this.swapUrl(null);
    this.shownKey = '';
  }
}
