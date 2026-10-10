// The Play preview: an animated WebP proxy of the current recipe
// (POST /api/proxy) shown in the preview stage instead of the still. It is
// fetched on demand only — Play — never on every change; when the recipe
// changes while a proxy is playing it is marked stale ("changed — Play
// again") and keeps playing the old one until the user asks for a new one
// or stops. Framework-free (proxy.test.ts); Preview.svelte hands it a
// $state object as the view.

import { isAbortError, messageOf, type ProxyRequest } from './api';

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
}

export interface ProxyDeps {
  fetch: (req: ProxyRequest, signal: AbortSignal) => Promise<Blob>;
  createURL: (blob: Blob) => string;
  revokeURL: (url: string) => void;
}

export class ProxyPlayer {
  private readonly view: ProxyView;
  private readonly deps: ProxyDeps;
  private ctrl: AbortController | null = null;
  /** key of the request ctrl belongs to ('' = none in flight) */
  private fetchingKey = '';
  /** the latest recipe and its key */
  private current: ProxyRequest | null = null;
  private currentKey = '';
  /** key of the proxy on screen ('' = none) */
  private shownKey = '';

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
    // A fetch for a superseded recipe must not land as "the" proxy.
    if (this.ctrl && this.fetchingKey !== this.currentKey) this.abort();
  }

  /** play fetches the proxy of the current recipe (a no-op when it is already shown and fresh). */
  async play(): Promise<void> {
    const r = this.current;
    if (!r) return;
    if (this.view.playing && !this.view.stale && this.view.url) return;
    const key = this.currentKey;
    if (this.ctrl && this.fetchingKey === key) return; // already on its way
    this.abort();
    const c = new AbortController();
    this.ctrl = c;
    this.fetchingKey = key;
    this.view.loading = true;
    this.view.error = '';
    try {
      const blob = await this.deps.fetch(r, c.signal);
      if (c.signal.aborted) return;
      this.swapUrl(this.deps.createURL(blob));
      this.shownKey = key;
      this.view.playing = true;
      this.view.stale = key !== this.currentKey;
    } catch (e) {
      if (isAbortError(e) || c.signal.aborted) return;
      this.view.error = messageOf(e);
    } finally {
      if (this.ctrl === c) {
        this.ctrl = null;
        this.fetchingKey = '';
        this.view.loading = false;
      }
    }
  }

  /** stop returns to the still: aborts any fetch and releases the proxy. */
  stop(): void {
    this.abort();
    this.swapUrl(null);
    this.shownKey = '';
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
