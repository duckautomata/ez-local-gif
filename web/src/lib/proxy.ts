// The Play preview: an animated WebP proxy of the current recipe
// (POST /api/proxy) shown in the preview stage instead of the still. It is
// fetched on demand only — Play — never on every change; when the recipe
// changes while a proxy is playing it is marked stale ("changed — Play
// again") and keeps playing the old one until the user asks for a new one
// or stops. Framework-free (proxy.test.ts); Preview.svelte hands it a
// $state object as the view.

import { hasMatteOp, isAbortError, isMattePending, messageOf, type MattePending, type ProxyRequest } from './api';
import { matteRetryMs, nextPending, type PendingView } from './matte';

export interface ProxyView {
  /** object URL of the proxy on screen (null = none) */
  url: string | null;
  /** a proxy is shown instead of the still */
  playing: boolean;
  /** a request is in flight */
  loading: boolean;
  /** the recipe changed since the shown proxy was rendered */
  stale: boolean;
  /** last failure ('' = none) */
  error: string;
  /**
   * Phase 5b: Play's answer was 202 — the recipe's AI matte is being
   * computed, or (Phase 5c) idle: Play behaves like a still and never
   * starts a pass; the Compute button (computeNow) re-requests with
   * `eager`. The still stays on the stage with the pill; the player
   * re-requests after matteRetryMs until the proxy arrives, Stop or a
   * recipe change.
   */
  pending: PendingView | null;
}

export interface ProxyDeps {
  fetch: (req: ProxyRequest, signal: AbortSignal) => Promise<Blob>;
  createURL: (blob: Blob) => string;
  revokeURL: (url: string) => void;
  /** called with every 202 answer (the component installs the status it carries) */
  onPending?: (p: MattePending) => void;
}

export class ProxyPlayer {
  private readonly view: ProxyView;
  private readonly deps: ProxyDeps;
  private ctrl: AbortController | null = null;
  /** key of the request ctrl belongs to ('' = none in flight) */
  private fetchingKey = '';
  /** the re-request of a pending (202) Play, and the key it is for */
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private pendingKey = '';
  /** the latest recipe and its key */
  private current: ProxyRequest | null = null;
  private currentKey = '';
  /** key of the proxy on screen ('' = none) */
  private shownKey = '';
  /** the key the Compute button was pressed for: its fetch and retries carry `eager` ('' = none) */
  private eagerKey = '';

  constructor(view: ProxyView, deps: ProxyDeps) {
    this.view = view;
    this.deps = deps;
  }

  /** key is the identity of a request: identical recipe → identical key. */
  static key(r: ProxyRequest | null): string {
    return r ? JSON.stringify(r) : '';
  }

  /**
   * update is called with every change of the recipe (null = no source).
   * Nothing is fetched; a playing proxy of another recipe becomes stale.
   */
  update(r: ProxyRequest | null): void {
    this.current = r;
    this.currentKey = ProxyPlayer.key(r);
    if (!r) {
      this.stop();
      return;
    }
    if (this.view.playing) this.view.stale = this.shownKey !== this.currentKey;
    if (this.eagerKey !== this.currentKey) this.eagerKey = '';
    // A fetch for a superseded recipe must not land as "the" proxy — nor
    // may the re-request of its pending matte.
    if (this.ctrl && this.fetchingKey !== this.currentKey) this.abort();
    if (this.view.pending && this.pendingKey !== this.currentKey) this.clearPending();
  }

  /**
   * play fetches the proxy of the current recipe (a no-op when it is
   * already shown and fresh). Play behaves like a still (Phase 5c): with a
   * matte op whose pass has not run the server answers 202 "idle" — the
   * pill with the Compute button shows (computeNow) — and a 202 of any
   * kind sets view.pending and re-requests after matteRetryMs until the
   * proxy arrives (the still stays on the stage). Only a request the
   * Compute button asked for carries `eager`.
   */
  async play(): Promise<void> {
    const r = this.current;
    if (!r) return;
    if (this.view.playing && !this.view.stale && this.view.url) return;
    const key = this.currentKey;
    if (this.ctrl && this.fetchingKey === key) return; // already on its way
    this.abort();
    this.clearRetry();
    const c = new AbortController();
    this.ctrl = c;
    this.fetchingKey = key;
    this.view.loading = true;
    this.view.error = '';
    try {
      const eager = this.eagerKey === key && hasMatteOp(r.ops);
      const blob = await this.deps.fetch(eager ? { ...r, eager: true } : r, c.signal);
      if (c.signal.aborted) return;
      this.swapUrl(this.deps.createURL(blob));
      this.shownKey = key;
      this.view.playing = true;
      this.view.stale = key !== this.currentKey;
      if (this.eagerKey === key) this.eagerKey = '';
      this.clearPending();
    } catch (e) {
      if (isAbortError(e) || c.signal.aborted) return;
      if (isMattePending(e)) {
        this.pendingAnswer(key, e);
        return;
      }
      this.view.error = messageOf(e);
      this.clearPending();
    } finally {
      if (this.ctrl === c) {
        this.ctrl = null;
        this.fetchingKey = '';
        this.view.loading = false;
      }
    }
  }

  /**
   * computeNow is the Compute button while Play's answer is pending and
   * idle: the current recipe is fetched at once with `eager: true` (the
   * pass starts) and its retries carry it until the proxy arrives.
   */
  computeNow(): void {
    if (!this.current) return;
    this.eagerKey = this.currentKey;
    this.abort();
    this.clearRetry();
    void this.play();
  }

  private pendingAnswer(key: string, e: MattePending): void {
    this.view.pending = nextPending(this.view.pending, e.info, Date.now());
    this.pendingKey = key;
    this.deps.onPending?.(e);
    this.clearRetry();
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      if (this.currentKey !== key) return;
      void this.play();
    }, matteRetryMs(e.state));
  }

  private clearRetry(): void {
    if (this.retryTimer !== undefined) clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
  }

  private clearPending(): void {
    this.clearRetry();
    this.view.pending = null;
    this.pendingKey = '';
  }

  /** stop returns to the still: aborts any fetch (and a pending matte's re-request) and releases the proxy. */
  stop(): void {
    this.abort();
    this.clearPending();
    this.swapUrl(null);
    this.shownKey = '';
    this.eagerKey = '';
    this.view.playing = false;
    this.view.stale = false;
    this.view.error = '';
  }

  private abort(): void {
    if (!this.ctrl) return;
    this.ctrl.abort();
    this.ctrl = null;
    this.fetchingKey = '';
    this.view.loading = false;
  }

  private swapUrl(next: string | null): void {
    const prev = this.view.url;
    this.view.url = next;
    if (prev) this.deps.revokeURL(prev);
  }

  /** imageFailed: the <img> could not decode the proxy. */
  imageFailed(): void {
    this.view.error = 'The preview animation could not be decoded';
    this.view.playing = false;
  }

  dispose(): void {
    this.stop();
  }
}
