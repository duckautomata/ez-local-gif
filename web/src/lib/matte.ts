// Pure helpers for the AI matte UI (Phase 5b, spec §9): the Background
// card's status line and model options, the pending pill of the preview
// (the 202 answer of a still / proxy), the Render panel's estimate suffix
// and its caps note — all computed from the /api/matte object
// (lib/matte.svelte.ts keeps the live copy) and a 202's pending fields.
// Framework-free (matte.test.ts); the schedulers in still.ts / proxy.ts
// share the pending bookkeeping (nextPending) and the retry cadence.

import { MATTE_MODEL_DEFAULT, type MatteModelState, type MatteModelStatus, type MattePendingInfo, type MatteStatus } from './api';

/**
 * MATTE_RETRY_MS: a still / proxy whose answer was 202 is re-requested
 * after this — inside jobs' 5 s abandon grace (matteAbandonGrace), so the
 * pass keeps a waiter while the preview polls — unless superseded.
 */
export const MATTE_RETRY_MS = 500;
/**
 * MATTE_DEFERRED_RETRY_MS: a still whose pass was DEFERRED (the estimate is
 * over the eager bound; nothing runs until Play / "Compute now" / Render)
 * is re-requested at the poll cadence only — every 500 ms would hammer the
 * server for an answer that cannot change by itself.
 */
export const MATTE_DEFERRED_RETRY_MS = 5000;
/** MATTE_POLL_MS: GET /api/matte is polled at this cadence while the AI mode is visible or a pass is pending. */
export const MATTE_POLL_MS = 5000;
/** The compose command the status line names when no sidecar answers. */
export const MATTE_PROFILE_HINT = 'docker compose --profile matte-gpu up -d';
/**
 * MATTE_FRAME_OVERHEAD_MS mirrors jobs' up-front refusal: estimate =
 * Frames × (msPerFrame + 2 ms) — the decode + hash per frame on top of the
 * model — judged against EZLG_MATTE_MAX_SECONDS. The same figure is the
 * "up to ~N s" of the estimate line (frames the store already holds cost
 * nothing, hence "up to").
 */
export const MATTE_FRAME_OVERHEAD_MS = 2;

/** the shipped models' labels, used when the sidecar reports none (an older sidecar) */
const MATTE_LABELS: Record<string, string> = { 'isnet-anime': 'Anime (fast)', 'birefnet-lite': 'General (precise)' };
/** display order of the model select: the shipped ids first, every other id after them alphabetically */
const MATTE_ORDER: readonly string[] = ['isnet-anime', 'birefnet-lite'];

/** deviceLabel is the UI word for a sidecar device: cuda → GPU, cpu → CPU, anything else as reported ('' for none). */
export function deviceLabel(device: string | null | undefined): string {
  switch (device) {
    case 'cuda':
      return 'GPU';
    case 'cpu':
      return 'CPU';
    default:
      return device ?? '';
  }
}

/**
 * humanSeconds mirrors jobs' humanSeconds: a millisecond estimate as "12 s"
 * (rounded) below two minutes and "3 min" (rounded) from there.
 */
export function humanSeconds(ms: number): string {
  const s = Math.floor((Math.max(0, ms) + 500) / 1000);
  if (s >= 120) return `${Math.floor((s + 30) / 60)} min`;
  return `${s} s`;
}

/** modelLabel is a model's UI label: the sidecar's, else the shipped id's, else the id itself. */
export function modelLabel(id: string, m?: MatteModelStatus | null): string {
  const l = m?.label?.trim();
  return l || MATTE_LABELS[id] || id;
}

/** matteModel reads one model's state from a status (null when the status lists none by that id). */
export function matteModel(status: MatteStatus | null | undefined, id: string): MatteModelStatus | null {
  const m = status?.models?.[id];
  return m && typeof m === 'object' ? m : null;
}

/** matteMsPerFrame is the sidecar's measured ms per frame of a model (0 = unknown / not offered). */
export function matteMsPerFrame(status: MatteStatus | null | undefined, id: string): number {
  const v = matteModel(status, id)?.msPerFrame;
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : 0;
}

/** One entry of the Background card's Model select. */
export interface MatteModelOption {
  id: string;
  label: string;
  state: MatteModelState;
  /** download / load progress for the transient states */
  percent: number;
  msPerFrame: number;
  /** why the model cannot be picked ('' when it can) */
  reason: string;
  /** missing / unavailable: greyed in the select */
  disabled: boolean;
  /** the option's visible text: the label plus a state suffix for anything but ready */
  text: string;
}

/**
 * matteModelOptions lists the models a status offers, the shipped ids first
 * (isnet-anime, birefnet-lite) and the rest alphabetically; a model that is
 * loading / downloading shows its state after the label, one that is
 * missing / unavailable is disabled with the sidecar's reason. Empty for no
 * status / no models.
 */
export function matteModelOptions(status: MatteStatus | null | undefined): MatteModelOption[] {
  const models = status?.models ?? null;
  if (!models) return [];
  const ids = Object.keys(models).filter((id) => models[id] && typeof models[id] === 'object');
  ids.sort((a, b) => {
    const ia = MATTE_ORDER.indexOf(a);
    const ib = MATTE_ORDER.indexOf(b);
    if (ia >= 0 || ib >= 0) return (ia < 0 ? MATTE_ORDER.length : ia) - (ib < 0 ? MATTE_ORDER.length : ib);
    return a.localeCompare(b);
  });
  return ids.map((id) => {
    const m = models[id];
    const label = modelLabel(id, m);
    const percent = typeof m.percent === 'number' && Number.isFinite(m.percent) ? Math.round(m.percent) : 0;
    const reason = m.reason?.trim() ?? '';
    let text = label;
    let disabled = false;
    switch (m.state) {
      case 'ready':
        break;
      case 'loading':
        text += ' — loading…';
        break;
      case 'downloading':
        text += ` — downloading ${percent} %`;
        break;
      case 'missing':
      case 'unavailable':
        text += ` — ${m.state}`;
        disabled = true;
        break;
      default:
        text += ` — ${m.state}`;
    }
    return { id, label, state: m.state, percent, msPerFrame: matteMsPerFrame(status, id), reason, disabled, text };
  });
}

/**
 * matteModelFor is the model id the card shows / the op names: the config's
 * explicit choice, else the sidecar's default, else the recipe default.
 */
export function matteModelFor(configured: string, status: MatteStatus | null | undefined): string {
  if (configured) return configured;
  const d = status?.defaultModel?.trim();
  return d || MATTE_MODEL_DEFAULT;
}

/** MatteEstimate is the pass's predicted wall time for a clip (jobs' up-front figure). */
export interface MatteEstimate {
  frames: number;
  msPerFrame: number;
  /** frames × (msPerFrame + MATTE_FRAME_OVERHEAD_MS) */
  ms: number;
  /** "GPU" / "CPU" / the device as reported */
  device: string;
}

/**
 * matteEstimate predicts a pass of `frames` frames with `model` from the
 * sidecar's measured ms per frame: null when nothing is known (no status,
 * the model is not offered or has no measurement yet, no frames).
 */
export function matteEstimate(status: MatteStatus | null | undefined, model: string, frames: number): MatteEstimate | null {
  if (!status || !(frames > 0)) return null;
  const msPerFrame = matteMsPerFrame(status, model);
  if (!(msPerFrame > 0)) return null;
  return { frames, msPerFrame, ms: frames * (msPerFrame + MATTE_FRAME_OVERHEAD_MS), device: deviceLabel(status.device) };
}

/** matteEstimateSuffix is what the Render panel appends to its estimate line: " · up to ~1 s AI matte (GPU)" ('' without an estimate). */
export function matteEstimateSuffix(est: MatteEstimate | null): string {
  if (!est) return '';
  return ` · up to ~${humanSeconds(est.ms)} AI matte${est.device ? ` (${est.device})` : ''}`;
}

/** What binds in matteVerdict: the frame cap, the seconds cap, or nothing. */
export type MatteOver = '' | 'frames' | 'seconds';

export interface MatteVerdict {
  over: MatteOver;
  /** the .note.error text under Render — the server's own refusal, in advance ('' when nothing is over) */
  note: string;
  estimate: MatteEstimate | null;
}

/**
 * matteVerdict judges a clip of `frames` frames against the caps the server
 * published (jobs' up-front refusal, §4.1 step 4): over EZLG_MATTE_MAX_FRAMES
 * on the count alone, over EZLG_MATTE_MAX_SECONDS on the estimate when the
 * model's ms per frame is known. A cap of 0 (unknown) never binds; nothing
 * binds without a status. The note is phrased like the server's error with
 * the ways out — trim, lower the fps, pick a faster model — and hedged for
 * a cached matte (a memo hit is served before the refusal).
 */
export function matteVerdict(status: MatteStatus | null | undefined, model: string, frames: number): MatteVerdict {
  const estimate = matteEstimate(status, model, frames);
  if (!status || !(frames > 0)) return { over: '', note: '', estimate };
  const cached = ' (unless this clip’s matte is already cached)';
  if (status.maxFrames > 0 && frames > status.maxFrames) {
    return {
      over: 'frames',
      note: `An AI matte of ${frames} frames is over this server's ${status.maxFrames}-frame cap (EZLG_MATTE_MAX_FRAMES), so Render will be refused${cached}: trim the clip or lower the fps.`,
      estimate,
    };
  }
  if (estimate && status.maxSeconds > 0 && estimate.ms > status.maxSeconds * 1000) {
    const where = estimate.device ? ` on this server's ${estimate.device}` : '';
    return {
      over: 'seconds',
      note: `An AI matte of ${frames} frames would take up to ~${humanSeconds(estimate.ms)}${where} — over its ${status.maxSeconds} s cap (EZLG_MATTE_MAX_SECONDS), so Render will be refused${cached}: trim the clip, lower the fps or pick a faster model.`,
      estimate,
    };
  }
  return { over: '', note: '', estimate };
}

/**
 * matteStatusLine is the Background card's one-liner under the Model
 * select: "ready · GPU · up to ~1 s for this clip", "loading model…",
 * "downloading weights 43 %", the sidecar's reason for a model that is
 * missing / unavailable, or — with the feature off / the device
 * unavailable — "sidecar unavailable — run `docker compose --profile
 * matte-gpu up -d`" with the server's reason. `estimateMs` is the clip's
 * (matteEstimate; 0 = none). null status = no answer yet.
 */
export function matteStatusLine(status: MatteStatus | null | undefined, model: string, estimateMs = 0): string {
  if (!status) return 'checking the matte service…';
  const reason = status.reason?.trim() ?? '';
  if (!status.enabled) return `sidecar unavailable — run \`${MATTE_PROFILE_HINT}\`${reason ? ` (${reason})` : ''}`;
  if (status.device === 'unavailable') return `sidecar unavailable — ${reason || 'the device is unavailable'}`;
  const m = matteModel(status, model);
  if (!m) return model ? `model ${model} is not offered by this server` : 'no model selected';
  switch (m.state) {
    case 'ready': {
      let s = `ready · ${deviceLabel(status.device) || status.device || 'device unknown'}`;
      if (estimateMs > 0) s += ` · up to ~${humanSeconds(estimateMs)} for this clip`;
      return s;
    }
    case 'loading':
      return 'loading model…';
    case 'downloading':
      return `downloading weights ${Math.round(m.percent ?? 0)} %`;
    default:
      return `${modelLabel(model, m)} ${m.state} — ${m.reason?.trim() || 'see the sidecar log'}`;
  }
}

/** PendingView is what a still / proxy view keeps while its answer is 202: the pending fields plus when they first appeared. */
export interface PendingView extends MattePendingInfo {
  /** Date.now() of the first 202 of this run (kept across updates): the "(12 s)" of the loading pill */
  since: number;
}

/** nextPending folds a new 202 into the view: the fields are the new answer's, `since` stays from the first. */
export function nextPending(prev: PendingView | null, info: MattePendingInfo, now: number): PendingView {
  return { ...info, since: prev?.since ?? now };
}

/**
 * mattePendingPill is the pill text over a pending preview (spec §6.2 /
 * §9): "AI matte 24/45 · GPU" while the pass runs, "AI matte: loading
 * model… (12 s)", "AI matte: downloading weights 43 %", and for a deferred
 * pass "AI matte: ~3 min on CPU" (the "Compute now" button sits next to it).
 */
export function mattePendingPill(p: PendingView, now: number): string {
  const dev = deviceLabel(p.device);
  switch (p.state) {
    case 'deferred':
      return p.estimateMs > 0 ? `AI matte: ~${humanSeconds(p.estimateMs)} on ${dev || 'the matte service'}` : 'AI matte: not computed yet';
    case 'loading':
      return `AI matte: loading model… (${Math.max(0, Math.floor((now - p.since) / 1000))} s)`;
    case 'downloading':
      return `AI matte: downloading weights ${Math.round(p.percent)} %`;
    default: {
      const tail = dev ? ` · ${dev}` : '';
      return p.total > 0 ? `AI matte ${p.done}/${p.total}${tail}` : `AI matte ${p.done} ${p.done === 1 ? 'frame' : 'frames'}${tail}`;
    }
  }
}

/** matteRetryMs is the re-request delay for a pending state: the poll cadence for a deferred pass, MATTE_RETRY_MS otherwise. */
export function matteRetryMs(state: MattePendingInfo['state']): number {
  return state === 'deferred' ? MATTE_DEFERRED_RETRY_MS : MATTE_RETRY_MS;
}
