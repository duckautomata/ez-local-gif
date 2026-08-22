// Server feature flags: the "features" map of GET /api/capabilities, loaded
// once per page into a $state the components gate their UI on — the Play
// button (proxy), the Text card's font picker (fonts) and the notices on the
// Background / Overlays cards (keying / overlays) when an older ezlg serves
// a newer SPA.
//
// Until the server has answered every flag is assumed on (the SPA ships with
// the server that has them all), so nothing flickers off and back on at page
// load; a server that answers without a "features" map at all (Phase 1) or
// without a given name (a Phase 2 build: fit / sequence / optimize only) has
// that feature off. A failed fetch keeps the optimistic defaults and is
// retried on the next loadFeatures() call.

import { getCapabilities, type Capabilities } from './api';

export const FEATURE_NAMES = ['fit', 'sequence', 'optimize', 'keying', 'overlays', 'proxy', 'fonts'] as const;
export type FeatureName = (typeof FEATURE_NAMES)[number];
export type Features = Record<FeatureName, boolean>;

function allFeatures(on: boolean): Features {
  return Object.fromEntries(FEATURE_NAMES.map((n) => [n, on])) as Features;
}

/**
 * featuresFrom maps a capabilities answer to the flag set: exactly the names
 * the server reports as true (a missing map or name is off). null / undefined
 * — no answer yet — is "everything on".
 */
export function featuresFrom(c: Pick<Capabilities, 'features'> | null | undefined): Features {
  if (!c) return allFeatures(true);
  const map = c.features ?? {};
  const out = allFeatures(false);
  for (const n of FEATURE_NAMES) out[n] = map[n] === true;
  return out;
}

export const caps = $state({
  /** the server has answered (the flags are its, not the optimistic defaults) */
  loaded: false,
  features: featuresFrom(null),
});

/** setFeatures installs a capabilities answer (null = back to "unknown": everything on). */
export function setFeatures(c: Pick<Capabilities, 'features'> | null): void {
  caps.features = featuresFrom(c);
  caps.loaded = c !== null;
}

let pending: Promise<Features> | null = null;

/**
 * loadFeatures fetches /api/capabilities once (a second call while the first
 * is in flight, or after it succeeded, does not fetch again). Never rejects:
 * a failed fetch leaves the defaults and lets a later call retry.
 */
export function loadFeatures(fetcher: (signal?: AbortSignal) => Promise<Capabilities> = getCapabilities): Promise<Features> {
  if (caps.loaded) return Promise.resolve(caps.features);
  if (pending) return pending;
  const p = fetcher()
    .then((c) => {
      setFeatures(c);
      return caps.features;
    })
    .catch(() => caps.features)
    .finally(() => {
      pending = null;
    });
  pending = p;
  return p;
}

/** resetFeatures forgets the answer (tests). */
export function resetFeatures(): void {
  pending = null;
  setFeatures(null);
}
