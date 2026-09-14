// Server feature flags: the "features" map of GET /api/capabilities, loaded
// once per page into a $state the components gate their UI on — the Play
// button (proxy), the Text card's font picker (fonts), the notices on the
// Background / Overlays cards (keying / overlays), and Phase 4's /input
// picker (inputPick), "Save to /output" (outputSave), gifski encoder
// toggle (gifski) and the Phase 4 op cards (feather / bounce,
// phase4OpsOffered) when an older ezlg serves a newer SPA. The "formats"
// list gates the MP4 / WebM Format options the same way (formatOffered).
// The same answer carries the server's frame-master cap ("maxMasterBytes",
// jobs.Options.MaxMasterBytes) and scratch budget ("scratchBudgetBytes"),
// which the Render panel judges its live master estimate against
// (state.planMaster / masterVerdict) so an untrimmed 4K clip is trimmed,
// cropped or resized in-app instead of being refused at Render.
//
// Until the server has answered every flag is assumed on (the SPA ships with
// the server that has them all), so nothing flickers off and back on at page
// load; a server that answers without a "features" map at all (Phase 1) or
// without a given name (a Phase 2 build: fit / sequence / optimize only) has
// that feature off. The byte caps have no optimistic default: 0 = unknown
// (no answer yet, or a server from before they were published) and the
// estimate is then shown without a verdict — never a false refusal. A failed
// fetch keeps the optimistic defaults and is retried on the next
// loadFeatures() call.

import { getCapabilities, type Capabilities } from './api';

export const FEATURE_NAMES = ['fit', 'sequence', 'optimize', 'keying', 'overlays', 'proxy', 'fonts', 'feather', 'bounce', 'inputPick', 'outputSave', 'gifski'] as const;
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
  /** the server's "formats" list; null = no answer yet (every format assumed offered) */
  formats: null as string[] | null,
  /**
   * The server's per-render frame-master cap in bytes
   * (jobs.Options.MaxMasterBytes, "maxMasterBytes"); 0 = unknown — no answer
   * yet, or a server from before the field existed — so the Render panel
   * never claims a refusal it cannot know about.
   */
  maxMasterBytes: 0,
  /**
   * The server's scratch admission budget in bytes
   * (jobs.Manager.ScratchBudgetBytes(), "scratchBudgetBytes"); 0 = unlimited
   * or unknown.
   */
  scratchBudgetBytes: 0,
});

/**
 * capBytes reads one of the byte caps of a capabilities answer: a finite,
 * positive number, else 0 (absent on an older server, null, a string from a
 * misbehaving proxy, NaN / Infinity — none of which may become a verdict).
 */
function capBytes(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : 0;
}

/**
 * setFeatures installs a capabilities answer (null = back to "unknown":
 * everything on, both byte caps 0). Callers pass partial objects (tests, an
 * older server's answer), so the byte caps are optional and 0 when absent,
 * non-finite or ≤ 0.
 */
export function setFeatures(c: Pick<Capabilities, 'features' | 'formats' | 'maxMasterBytes' | 'scratchBudgetBytes'> | null): void {
  caps.features = featuresFrom(c);
  caps.loaded = c !== null;
  caps.formats = c ? (Array.isArray(c.formats) ? [...c.formats] : []) : null;
  caps.maxMasterBytes = c ? capBytes(c.maxMasterBytes) : 0;
  caps.scratchBudgetBytes = c ? capBytes(c.scratchBudgetBytes) : 0;
}

/**
 * formatOffered reports whether the server encodes this output format
 * (capabilities "formats"). Until the server has answered every format is
 * assumed offered; a server that answered without the name (pre-Phase-4:
 * no mp4/webm) has it off. An answer without a formats list at all (Phase 1)
 * turns nothing off — only an explicit list that lacks the name does.
 */
export function formatOffered(format: string): boolean {
  const list = caps.formats;
  if (list === null || list.length === 0) return true;
  return list.includes(format);
}

/**
 * phase4OpsOffered reports whether the server understands the Phase 4 op
 * kinds — feather and bounce (an older ezlg 400s on the unknown kind at the
 * first still / proxy / render, with no hint why). The primary signal is the
 * explicit "feather" / "bounce" names in the features map; a Phase 4 server
 * from before those names existed still answers with mp4 in its "formats"
 * list (ffmpeg-only, never toolchain-gated — no earlier server offers it),
 * so that list is kept as a fallback. Like formatOffered, no answer yet —
 * or an answer without a formats list at all — leaves the ops on.
 */
export function phase4OpsOffered(): boolean {
  return (caps.features.feather && caps.features.bounce) || formatOffered('mp4');
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
