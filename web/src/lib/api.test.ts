import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ApiError,
  fetchProxy,
  fetchStill,
  getMatte,
  hasMatteOp,
  isAnimatedFormat,
  isHash,
  isMattePending,
  isStaticFormat,
  isVideoFormat,
  MATTE_MODEL_DEFAULT,
  matteModelOf,
  MattePending,
  OUTPUT_FORMATS,
  sourceHashFromSearch,
  sourceURL,
  type ProxyRequest,
  type StillRequest,
} from './api';

const HASH = '0123456789abcdef'.repeat(4);

describe('source URL round trip', () => {
  it('reads ?src= only when it is a sha256 hex hash', () => {
    expect(sourceHashFromSearch(`?src=${HASH}`)).toBe(HASH);
    expect(sourceHashFromSearch(`?x=1&src=${HASH}&y=2`)).toBe(HASH);
    expect(sourceHashFromSearch('?src=../etc')).toBeNull();
    expect(sourceHashFromSearch(`?src=${HASH.toUpperCase()}`)).toBeNull();
    expect(sourceHashFromSearch('?src=')).toBeNull();
    expect(sourceHashFromSearch('')).toBeNull();
  });
  it('builds the address "edit as source" opens', () => {
    expect(sourceURL(HASH)).toBe(`/?src=${HASH}`);
    expect(sourceURL(null)).toBe('/');
    expect(sourceHashFromSearch(sourceURL(HASH).slice(1))).toBe(HASH);
  });
  it('isHash mirrors recipe.IsHash', () => {
    expect(isHash(HASH)).toBe(true);
    expect(isHash(HASH.slice(1))).toBe(false);
    expect(isHash(HASH.replace('a', 'g'))).toBe(false);
  });
});

describe('format classes', () => {
  it('mirror recipe.IsAnimatedFormat / IsStaticFormat / discordlint.IsVideoFormat', () => {
    expect(OUTPUT_FORMATS.filter(isAnimatedFormat)).toEqual(['gif', 'webp', 'apng', 'avif']);
    expect(OUTPUT_FORMATS.filter(isStaticFormat)).toEqual(['png', 'jpeg']);
    expect(OUTPUT_FORMATS.filter(isVideoFormat)).toEqual(['mp4', 'webm']);
    expect(isAnimatedFormat('frames')).toBe(false);
    expect(isStaticFormat('frames')).toBe(false);
    expect(isAnimatedFormat('mp4')).toBe(false); // video: no loop semantics, no animated-format rules
    expect(isVideoFormat('webp')).toBe(false);
  });
});

// Phase 5b: the matte op helpers, the 202 preview answer (MattePending) and
// GET /api/matte.
describe('matte ops', () => {
  it('hasMatteOp / matteModelOf read the stack: the recipe default when the op names no model', () => {
    expect(hasMatteOp([])).toBe(false);
    expect(hasMatteOp(null)).toBe(false);
    expect(hasMatteOp([{ kind: 'colorkey', params: { color: '313338' } }])).toBe(false);
    expect(hasMatteOp([{ kind: 'trim', params: { start: 1 } }, { kind: 'matte' }])).toBe(true);
    expect(matteModelOf([{ kind: 'matte' }])).toBe(MATTE_MODEL_DEFAULT);
    expect(matteModelOf([{ kind: 'matte', params: {} }])).toBe('isnet-anime');
    expect(matteModelOf([{ kind: 'matte', params: { model: 'birefnet-lite' } }])).toBe('birefnet-lite');
    expect(matteModelOf([{ kind: 'chromakey' }])).toBe('');
    expect(matteModelOf(undefined)).toBe('');
  });
});

const body202 = {
  pending: 'matte',
  state: 'running',
  done: 24,
  total: 45,
  percent: 53,
  estimateMs: 810,
  enabled: true,
  device: 'cuda',
  reason: '',
  gpu: { name: 'RTX 5080', totalGiB: 16, freeGiB: 12 },
  defaultModel: 'isnet-anime',
  models: { 'isnet-anime': { label: 'Anime (fast)', state: 'ready', msPerFrame: 18 } },
  maxSeconds: 600,
  maxFrames: 3000,
};

describe('MattePending', () => {
  it('fromBody takes the documented shape, defaults absent numbers / strings, refuses anything else', () => {
    const p = MattePending.fromBody(body202);
    expect(p).toBeInstanceOf(MattePending);
    expect(isMattePending(p)).toBe(true);
    expect(p?.name).toBe('MattePending');
    expect(p?.message).toBe('AI matte pending: running');
    expect(p?.info).toEqual({ state: 'running', done: 24, total: 45, percent: 53, estimateMs: 810, device: 'cuda' });
    expect(p?.status).toEqual({
      enabled: true,
      device: 'cuda',
      reason: '',
      gpu: { name: 'RTX 5080', totalGiB: 16, freeGiB: 12 },
      defaultModel: 'isnet-anime',
      models: { 'isnet-anime': { label: 'Anime (fast)', state: 'ready', msPerFrame: 18 } },
      maxSeconds: 600,
      maxFrames: 3000,
    });
    // a minimal body: everything defaults
    const min = MattePending.fromBody({ pending: 'matte', state: 'deferred' });
    expect(min?.info).toEqual({ state: 'deferred', done: 0, total: 0, percent: 0, estimateMs: 0, device: '' });
    expect(min?.status).toEqual({ enabled: false, device: '', reason: '', gpu: null, defaultModel: '', models: null, maxSeconds: 0, maxFrames: 0 });
    // a newer server's extra fields are ignored, wrong types fall back
    expect(MattePending.fromBody({ ...body202, done: '24', estimateMs: NaN, extra: 1 })?.info).toMatchObject({ done: 0, estimateMs: 0 });
    for (const bad of [null, undefined, 'matte', 7, [], {}, { pending: 'other', state: 'running' }, { pending: 'matte' }, { pending: 'matte', state: 3 }]) {
      expect(MattePending.fromBody(bad), JSON.stringify(bad)).toBeNull();
    }
    expect(isMattePending(new Error('x'))).toBe(false);
    expect(isMattePending(new ApiError('x', 500))).toBe(false);
  });
});

describe('fetchStill / fetchProxy on 202, getMatte', () => {
  afterEach(() => vi.unstubAllGlobals());
  const stillReq: StillRequest = { src: 'h', ops: [{ kind: 'matte' }], output: { format: 'gif' }, t: 0, maxW: 480 };
  const proxyReq: ProxyRequest = { sources: ['h'], ops: [{ kind: 'matte' }], output: { format: 'gif' }, maxW: 360, maxSeconds: 10, eager: true };

  function stub(res: () => Response) {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      return res();
    });
    return calls;
  }
  const json202 = () => new Response(JSON.stringify(body202), { status: 202, headers: { 'content-type': 'application/json' } });

  it('a 202 throws MattePending carrying the pending fields and the status (request() alone would hand the JSON to blob())', async () => {
    const calls = stub(json202);
    await expect(fetchStill(stillReq)).rejects.toBeInstanceOf(MattePending);
    const e = (await fetchProxy(proxyReq).catch((x: unknown) => x)) as MattePending;
    expect(isMattePending(e)).toBe(true);
    expect(e.state).toBe('running');
    expect(e.done).toBe(24);
    expect(e.status.models?.['isnet-anime']?.msPerFrame).toBe(18);
    expect(calls.map((c) => c.url)).toEqual(['/api/still', '/api/proxy']);
    expect(JSON.parse(String(calls[1].init?.body))).toEqual(proxyReq); // eager rides along
    expect(calls[0].init?.method).toBe('POST');
  });

  it('a 202 with a malformed or foreign body is an ApiError, never a picture; a 200 is the blob', async () => {
    stub(() => new Response('not json', { status: 202 }));
    const e1 = (await fetchStill(stillReq).catch((x: unknown) => x)) as ApiError;
    expect(e1).toBeInstanceOf(ApiError);
    expect(e1.status).toBe(202);
    expect(e1.message).toContain('Malformed JSON');
    stub(() => new Response(JSON.stringify({ pending: 'other' }), { status: 202 }));
    const e2 = (await fetchProxy(proxyReq).catch((x: unknown) => x)) as ApiError;
    expect(e2).toBeInstanceOf(ApiError);
    expect(e2.message).toContain('Unexpected 202');
    stub(() => new Response(new Uint8Array([137, 80, 78, 71]), { status: 200, headers: { 'content-type': 'image/png' } }));
    const blob = await fetchStill(stillReq);
    expect(blob.size).toBe(4);
    // an ordinary error still maps to ApiError with the server's text
    stub(() => new Response(JSON.stringify({ error: 'invalid recipe: the matte service is not available — start the matte profile' }), { status: 400 }));
    const e3 = (await fetchStill(stillReq).catch((x: unknown) => x)) as ApiError;
    expect(e3.status).toBe(400);
    expect(e3.message).toContain('matte profile');
  });

  it('getMatte reads /api/matte uncached', async () => {
    const calls = stub(() => new Response(JSON.stringify({ enabled: false, device: '', reason: 'no matte service is configured', defaultModel: 'isnet-anime', models: {}, maxSeconds: 600, maxFrames: 3000 }), { status: 200 }));
    const st = await getMatte();
    expect(st.enabled).toBe(false);
    expect(st.reason).toContain('no matte service');
    expect(calls[0].url).toBe('/api/matte');
    expect(calls[0].init?.cache).toBe('no-store');
    // an older server: 404 → ApiError
    stub(() => new Response('404 page not found', { status: 404 }));
    const e = (await getMatte().catch((x: unknown) => x)) as ApiError;
    expect(e).toBeInstanceOf(ApiError);
    expect(e.status).toBe(404);
  });
});
