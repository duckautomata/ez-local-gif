// Preview still scheduling: debounce, abort superseded requests, keep the
// displayed frame in sync with the requested state, object-URL lifecycle.
// Framework-free so it can be unit tested (still.test.ts); Preview.svelte
// hands it a $state object as the view, so every mutation is reactive.

import { isAbortError, messageOf, type StillRequest } from './api';

/** StillView is what the component renders; the scheduler mutates it in place. */
export interface StillView {
  /** object URL of the still on screen (null = nothing yet) */
  url: string | null;
  /** a request is in flight */
  loading: boolean;
  /** last load/decode failure for the current state ('' = none) */
  error: string;
}

export interface StillDeps {
  fetch: (req: StillRequest, signal: AbortSignal) => Promise<Blob>;
  createURL: (blob: Blob) => string;
  revokeURL: (url: string) => void;
  /** debounce before a request is sent (default 150 ms) */
  debounceMs?: number;
}

export const DECODE_ERROR = 'The preview image could not be decoded';

/** Fit-mode still width: at most this wide (the server scales the output canvas down to it). */
export const FIT_STILL_MAXW = 480;
/** Fit-mode still width on wide screens (≥ 1500 px). */
export const FIT_STILL_MAXW_WIDE = 720;
/**
 * The "unscaled" request width: graph.MaxDim, the largest frame the server
 * can produce, so its scale=min(iw, maxW) never shrinks the still and the
 * PNG is exactly the output canvas (Plan.Width × Plan.Height).
 */
export const CANVAS_STILL_MAXW = 8192;

/**
 * stillMaxW is the width cap a preview still is requested at:
 *
 * - Fit (the default): FIT_STILL_MAXW, or FIT_STILL_MAXW_WIDE on a wide
 *   screen — a scrub step then moves a small PNG (~100 ms server-side, and
 *   memoised), whatever the output size;
 * - with overlays on the stage: unscaled (CANVAS_STILL_MAXW), so the still's
 *   natural size IS the output canvas the overlay coordinates live on and the
 *   drag boxes map 1:1;
 * - at a fixed zoom (1× / 2× / 4×): unscaled too. The zoom multiplies the
 *   still's natural width, so 1× must be one OUTPUT pixel per CSS pixel and
 *   2× / 4× must magnify real output pixels (the img is rendered
 *   `pixelated` there): a 2560-wide output at 4× used to be a 480-px still
 *   stretched to 1920 px — a blur of preview pixels, not the frame the
 *   render produces. The larger PNG per scrub step is the price of a real
 *   zoom, paid only while zoomed; Fit goes back to the small still (its own
 *   memo entry, so switching back is instant).
 */
export function stillMaxW(opts: { overlay: boolean; zoomed: boolean; wide: boolean }): number {
  if (opts.overlay || opts.zoomed) return CANVAS_STILL_MAXW;
  return opts.wide ? FIT_STILL_MAXW_WIDE : FIT_STILL_MAXW;
}

/**
 * StillScheduler turns a stream of StillRequests (one per state change) into
 * at most one in-flight fetch whose result matches the latest state:
 *
 * - requests are debounced; a newer request aborts the older in-flight one;
 * - when the state returns to what is already on screen, the in-flight request
 *   for the superseded state is aborted (so it can never land later and show a
 *   frame that contradicts the controls) and the error it may have produced is
 *   dropped;
 * - an error is remembered together with the key it belongs to, so a decode
 *   error of the *displayed* still is not hidden by that same rule;
 * - the previous object URL is revoked only after the next image has loaded
 *   (no flash of a broken image);
 * - while paused (the still is off the stage because the proxy plays)
 *   nothing is fetched and the parked URLs are released; resuming fetches
 *   the latest state once (setPaused).
 */
export class StillScheduler {
  private readonly view: StillView;
  private readonly deps: StillDeps;
  private readonly debounceMs: number;

  private timer: ReturnType<typeof setTimeout> | undefined;
  private ctrl: AbortController | null = null;
  /** key of the still currently on screen (set on success only) */
  private lastKey = '';
  /** key whose load/decode produced view.error */
  private errorKey = '';
  /** the latest request and its key (for retry) */
  private current: StillRequest | null = null;
  private currentKey = '';
  private revokeOnLoad: string[] = [];
  /** the still is off the stage: remember requests, fetch nothing */
  private paused = false;

  constructor(view: StillView, deps: StillDeps) {
    this.view = view;
    this.deps = deps;
    this.debounceMs = deps.debounceMs ?? 150;
  }

  /** key is the identity of a request: identical state → identical key. */
  static key(r: StillRequest | null): string {
    return r ? JSON.stringify(r) : '';
  }

  /** request is called with every change of the derived request (null = no source). */
  request(r: StillRequest | null): void {
    this.current = r;
    this.currentKey = StillScheduler.key(r);
    this.schedule(this.currentKey, r);
  }

  /** The still on screen belongs to this key ('' = none). */
  get displayedKey(): string {
    return this.lastKey;
  }

  private schedule(key: string, r: StillRequest | null): void {
    if (this.timer !== undefined) clearTimeout(this.timer);
    this.timer = undefined;
    if (!r) {
      this.abortInFlight();
      this.lastKey = '';
      this.clearError();
      this.swapUrl(null);
      return;
    }
    if (key === this.lastKey) {
      // The displayed still already matches the state: drop any in-flight
      // request for a superseded state (it must never land on top of the
      // correct frame) and the error a superseded state may have left behind.
      this.abortInFlight();
      if (this.errorKey !== key) this.clearError();
      return;
    }
    if (this.paused) return; // remembered in `current`; setPaused(false) schedules it
    this.timer = setTimeout(() => void this.load(key, r), this.debounceMs);
  }

  /**
   * setPaused(true) takes the scheduler off duty while the still is not on
   * the stage (the proxy plays): the debounce timer and an in-flight
   * request are dropped, the object URLs parked for the next image load are
   * released now (nothing loads while paused, and none of them is the one
   * view.url names), and later requests are only remembered — every scrub
   * or op change used to render a still (at full resolution in overlay
   * mode) and park its URL until Stop. setPaused(false) fetches the
   * remembered state once, unless the still on screen already matches it.
   */
  setPaused(paused: boolean): void {
    if (this.paused === paused) return;
    this.paused = paused;
    if (paused) {
      if (this.timer !== undefined) clearTimeout(this.timer);
      this.timer = undefined;
      this.abortInFlight();
      this.releaseParked();
      return;
    }
    if (this.current) this.schedule(this.currentKey, this.current);
  }

  private async load(key: string, r: StillRequest): Promise<void> {
    this.ctrl?.abort();
    const c = new AbortController();
    this.ctrl = c;
    this.view.loading = true;
    try {
      const blob = await this.deps.fetch(r, c.signal);
      if (c.signal.aborted) return;
      this.lastKey = key;
      this.clearError();
      this.swapUrl(this.deps.createURL(blob));
    } catch (e) {
      if (isAbortError(e) || c.signal.aborted) return;
      this.view.error = messageOf(e);
      this.errorKey = key;
    } finally {
      if (this.ctrl === c) {
        this.view.loading = false;
        this.ctrl = null;
      }
    }
  }

  private abortInFlight(): void {
    if (!this.ctrl) return;
    this.ctrl.abort();
    this.ctrl = null;
    this.view.loading = false;
  }

  private clearError(): void {
    this.view.error = '';
    this.errorKey = '';
  }

  // Object URLs are revoked once the next image has loaded (so the previous
  // still stays visible until then — no flash of a broken image).
  private swapUrl(next: string | null): void {
    const prev = this.view.url;
    this.view.url = next;
    if (prev) {
      if (next) this.revokeOnLoad.push(prev);
      else this.deps.revokeURL(prev);
    }
  }

  /** imageLoaded: the <img> for view.url has decoded — release the previous URLs. */
  imageLoaded(): void {
    this.releaseParked();
  }

  private releaseParked(): void {
    for (const u of this.revokeOnLoad) this.deps.revokeURL(u);
    this.revokeOnLoad = [];
  }

  /** imageFailed: the <img> for view.url could not be decoded. */
  imageFailed(): void {
    this.view.error = DECODE_ERROR;
    this.errorKey = this.lastKey;
  }

  /** retry forgets the displayed still and the error and re-requests the current state. */
  retry(): void {
    this.clearError();
    this.lastKey = '';
    if (this.current) this.schedule(this.currentKey, this.current);
  }

  /** dispose cancels everything and releases all object URLs. */
  dispose(): void {
    if (this.timer !== undefined) clearTimeout(this.timer);
    this.timer = undefined;
    this.abortInFlight();
    if (this.view.url) this.deps.revokeURL(this.view.url);
    this.view.url = null;
    this.releaseParked();
  }
}
