// The server feature flags: how a capabilities answer maps onto the flags
// the UI gates on, the optimistic defaults before the server answered, and
// the once-per-page fetch that retries after a failure.
import { afterEach, describe, expect, it } from 'vitest';
import type { Capabilities } from './api';
import { caps, FEATURE_NAMES, featuresFrom, loadFeatures, resetFeatures, setFeatures } from './capabilities.svelte';

/** a capabilities answer carrying the given features map (undefined = none: a Phase 1 server) */
function answer(features?: Record<string, boolean>): Capabilities {
  const c: Capabilities = { tools: {}, limits: {}, rulesVersion: '1' };
  if (features) c.features = features;
  return c;
}
const phase3 = { fit: true, sequence: true, optimize: true, keying: true, overlays: true, proxy: true, fonts: true };

describe('featuresFrom', () => {
  it('is everything-on while the server has not answered', () => {
    for (const n of FEATURE_NAMES) {
      expect(featuresFrom(null)[n], n).toBe(true);
      expect(featuresFrom(undefined)[n], n).toBe(true);
    }
  });

  it('takes exactly the names the server reports true; a missing name is off', () => {
    expect(featuresFrom(answer(phase3))).toEqual(phase3);
    // a Phase 2 server: no Phase 3 names at all
    const f = featuresFrom(answer({ fit: true, sequence: true, optimize: true }));
    expect(f).toEqual({ ...phase3, keying: false, overlays: false, proxy: false, fonts: false });
    // a Phase 3 server without fc-list
    expect(featuresFrom(answer({ ...phase3, fonts: false })).fonts).toBe(false);
    // only `true` counts: nothing truthy-but-not-boolean sneaks in from a newer server
    expect(featuresFrom(answer({ proxy: 1 as unknown as boolean })).proxy).toBe(false);
  });

  it('treats an answer without a features map (Phase 1) as nothing on', () => {
    for (const n of FEATURE_NAMES) expect(featuresFrom(answer())[n], n).toBe(false);
    expect(featuresFrom({ features: null }).proxy).toBe(false);
  });
});

describe('caps / loadFeatures', () => {
  afterEach(() => resetFeatures());

  it('starts unknown with every flag on; setFeatures installs an answer and resetFeatures forgets it', () => {
    expect(caps.loaded).toBe(false);
    expect(caps.features.proxy).toBe(true);
    setFeatures(answer({ ...phase3, proxy: false }));
    expect(caps.loaded).toBe(true);
    expect(caps.features.proxy).toBe(false);
    expect(caps.features.fonts).toBe(true);
    resetFeatures();
    expect(caps.loaded).toBe(false);
    expect(caps.features.proxy).toBe(true);
  });

  it('fetches once and caches the answer', async () => {
    let n = 0;
    const fetcher = async () => {
      n++;
      return answer({ ...phase3, fonts: false });
    };
    const [a, b] = await Promise.all([loadFeatures(fetcher), loadFeatures(fetcher)]);
    expect(a.fonts).toBe(false);
    expect(b.fonts).toBe(false);
    expect(n).toBe(1); // the second call joined the in-flight fetch
    expect(await loadFeatures(fetcher)).toEqual({ ...phase3, fonts: false });
    expect(n).toBe(1); // and once loaded nothing is fetched again
    expect(caps.loaded).toBe(true);
  });

  it('never rejects: a failed fetch keeps the optimistic defaults and is retried next time', async () => {
    let n = 0;
    const failing = async () => {
      n++;
      throw new Error('HTTP 502');
    };
    expect((await loadFeatures(failing)).proxy).toBe(true);
    expect(caps.loaded).toBe(false);
    expect((await loadFeatures(failing)).keying).toBe(true);
    expect(n).toBe(2);
    const f = await loadFeatures(async () => answer({ fit: true, sequence: true, optimize: true }));
    expect(f.keying).toBe(false);
    expect(caps.loaded).toBe(true);
    expect(caps.features.overlays).toBe(false);
  });
});
