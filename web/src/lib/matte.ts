// Pure helpers for the AI matte UI (Phase 5b, spec §9; Phase 5c: device
// choice, the Compute matte button, stabilise, the guided model): the
// Background card's status line, model / device / stabilise / edge options,
// the pending pill of the preview (the 202 answer of a still / proxy), the
// Render panel's estimate suffix and its caps note — all computed from the
// /api/matte object (lib/matte.svelte.ts keeps the live copy) and a 202's
// pending fields. Framework-free (matte.test.ts); the schedulers in
// still.ts / proxy.ts share the pending bookkeeping (nextPending) and the
// retry cadence.

import {
  MATTE_EDGE_NONE,
  MATTE_KIND_TRACKER,
  MATTE_MODEL_DEFAULT,
  MATTE_MODEL_SAM2_TINY,
  MATTE_PHASE_TRACKING,
  MATTE_STABILISE_LIGHT,
  MATTE_STABILISE_OFF,
  MATTE_STABILISE_STRONG,
  type MatteModelDeviceStatus,
  type MatteModelState,
  type MatteModelStatus,
  type MattePendingInfo,
  type MatteStatus,
} from './api';

/**
 * MATTE_RETRY_MS: a still / proxy whose answer was 202 is re-requested
 * after this — inside jobs' 5 s abandon grace (matteAbandonGrace), so the
 * pass keeps a waiter while the preview polls — unless superseded.
 */
export const MATTE_RETRY_MS = 500;
/**
 * MATTE_IDLE_RETRY_MS: a still whose matte is IDLE (nothing on disk, no
 * pass running — the Compute matte button or a render starts one; a 5b
 * server says "deferred") is re-requested at the poll cadence only — every
 * 500 ms would hammer the server for an answer that cannot change by
 * itself; the slow retry still notices a pass a render started.
 */
export const MATTE_IDLE_RETRY_MS = 5000;
/** @deprecated the 5b name of MATTE_IDLE_RETRY_MS (the "deferred" state of a 5b server retries at the same cadence) */
export const MATTE_DEFERRED_RETRY_MS = MATTE_IDLE_RETRY_MS;
/** MATTE_POLL_MS: GET /api/matte is polled at this cadence while the AI mode is visible or a pass is pending. */
export const MATTE_POLL_MS = 5000;
/**
 * MATTE_UNLOAD_DEBOUNCE_MS: leaving the AI mode (or switching the card off)
 * releases the sidecar's resident models after this much time without
 * coming back — a mode flicked back and forth never unloads a model that is
 * about to be used again.
 */
export const MATTE_UNLOAD_DEBOUNCE_MS = 1500;
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
const MATTE_LABELS: Record<string, string> = { 'isnet-anime': 'Anime (fast)', 'birefnet-lite': 'General (precise)', 'sam2-tiny': 'Guided (click to select)' };
/** display order of the model select: the shipped segmenters first, every other id after them alphabetically, trackers last */
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

/** shortModelLabel is a label without its parenthesised qualifier: "General (precise)" → "General", "Anime (fast)" → "Anime". */
export function shortModelLabel(label: string): string {
  const s = label.replace(/\s*\([^)]*\)\s*$/, '').trim();
  return s || label.trim();
}

/** matteModel reads one model's state from a status (null when the status lists none by that id). */
export function matteModel(status: MatteStatus | null | undefined, id: string): MatteModelStatus | null {
  const m = status?.models?.[id];
  return m && typeof m === 'object' ? m : null;
}

/**
 * isTracker: the model is the guided one (kind "tracker" in the status;
 * the shipped sam2-tiny id when the status says nothing — no status yet,
 * or a 5b server that cannot offer it anyway).
 */
export function isTracker(status: MatteStatus | null | undefined, id: string): boolean {
  const m = matteModel(status, id);
  if (m && typeof m.kind === 'string' && m.kind !== '') return m.kind === MATTE_KIND_TRACKER;
  return id === MATTE_MODEL_SAM2_TINY;
}

// ---------------------------------------------------------------------------
// devices (Phase 5c)

/**
 * matteDevices lists the devices the sidecar offers, GPU first: the 5c
 * `devices` list, else (a 5b server) exactly its one device when it is
 * cuda / cpu. Empty with no status, the feature off or no device.
 */
export function matteDevices(status: MatteStatus | null | undefined): string[] {
  if (!status || !status.enabled) return [];
  const list = Array.isArray(status.devices) ? status.devices.filter((d) => typeof d === 'string' && d !== '') : [];
  if (list.length) {
    const order = (d: string) => (d === 'cuda' ? 0 : d === 'cpu' ? 1 : 2);
    return [...new Set(list)].sort((a, b) => order(a) - order(b) || a.localeCompare(b));
  }
  return status.device === 'cuda' || status.device === 'cpu' ? [status.device] : [];
}

/** effectiveDevice is the device passes run on ('' when unknown / unavailable): MatteStatus.device, which jobs reports as the effective one. */
export function effectiveDevice(status: MatteStatus | null | undefined): string {
  const d = status?.device?.trim() ?? '';
  return d === 'unavailable' ? '' : d;
}

/**
 * defaultModelFor is the sidecar's default model on a device: the 5c
 * per-device map, else the status's defaultModel (the effective device's),
 * else the recipe default.
 */
export function defaultModelFor(status: MatteStatus | null | undefined, device: string): string {
  const per = status?.defaultModels?.[device]?.trim();
  if (per) return per;
  const d = status?.defaultModel?.trim();
  return d || MATTE_MODEL_DEFAULT;
}

/**
 * matteDeviceState is one model's live state on one device: the 5c
 * per-device entry when the status carries one, else the top-level fields
 * (which mirror the sidecar's default device — the only one a 5b server
 * has). null when the model is not offered, or — with a per-device map —
 * not on that device. device '' = the effective device.
 */
export function matteDeviceState(status: MatteStatus | null | undefined, id: string, device = ''): MatteModelDeviceStatus | null {
  const m = matteModel(status, id);
  if (!m) return null;
  const dev = device || effectiveDevice(status);
  const map = m.devices && typeof m.devices === 'object' ? m.devices : null;
  if (map && dev) {
    const d = map[dev];
    return d && typeof d === 'object' ? d : null;
  }
  return { state: m.state, reason: m.reason, percent: m.percent, msPerFrame: m.msPerFrame, resident: m.resident };
}

/** matteMsPerFrame is the sidecar's measured ms per frame of a model on a device ('' = the effective one; 0 = unknown / not offered). */
export function matteMsPerFrame(status: MatteStatus | null | undefined, id: string, device = ''): number {
  const v = matteDeviceState(status, id, device)?.msPerFrame;
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : 0;
}

// ---------------------------------------------------------------------------
// model options

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
  /** Phase 5c: the guided model (listed last; needs the preview to prompt) */
  tracker: boolean;
}

function optionText(label: string, state: MatteModelState, percent: number): { text: string; disabled: boolean } {
  switch (state) {
    case 'ready':
      return { text: label, disabled: false };
    case 'loading':
      return { text: `${label} — loading…`, disabled: false };
    case 'downloading':
      return { text: `${label} — downloading ${percent} %`, disabled: false };
    case 'missing':
    case 'unavailable':
      return { text: `${label} — ${state}`, disabled: true };
    default:
      return { text: `${label} — ${state}`, disabled: false };
  }
}

/**
 * matteModelOptions lists the models a status offers, the shipped
 * segmenters first (isnet-anime, birefnet-lite), the rest alphabetically
 * and the guided tracker(s) last; a model that is loading / downloading
 * shows its state after the label, one that is missing / unavailable is
 * disabled with the sidecar's reason. The states are those of `device`
 * ('' = the effective device) when the status carries a per-device map; a
 * model absent from that device is listed as unavailable there. Empty for
 * no status / no models.
 */
export function matteModelOptions(status: MatteStatus | null | undefined, device = ''): MatteModelOption[] {
  const models = status?.models ?? null;
  if (!models) return [];
  const ids = Object.keys(models).filter((id) => models[id] && typeof models[id] === 'object');
  const rank = (id: string) => {
    if (isTracker(status, id)) return MATTE_ORDER.length + 1;
    const i = MATTE_ORDER.indexOf(id);
    return i < 0 ? MATTE_ORDER.length : i;
  };
  ids.sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
  const dev = device || effectiveDevice(status);
  return ids.map((id) => {
    const m = models[id];
    const label = modelLabel(id, m);
    const d = matteDeviceState(status, id, dev);
    const state: MatteModelState = d ? d.state : 'unavailable';
    const percent = typeof d?.percent === 'number' && Number.isFinite(d.percent) ? Math.round(d.percent) : 0;
    const reason = d ? (d.reason?.trim() ?? '') : dev ? `not offered on ${deviceLabel(dev)}` : 'not offered on this device';
    const { text, disabled } = optionText(label, state, percent);
    return { id, label, state, percent, msPerFrame: matteMsPerFrame(status, id, dev), reason, disabled, text, tracker: isTracker(status, id) };
  });
}

/**
 * matteModelFor is the model id the card shows / the op names: the config's
 * explicit choice, else the sidecar's default (for the effective device),
 * else the recipe default.
 */
export function matteModelFor(configured: string, status: MatteStatus | null | undefined): string {
  if (configured) return configured;
  const d = status?.defaultModel?.trim();
  return d || MATTE_MODEL_DEFAULT;
}

// ---------------------------------------------------------------------------
// stabilise / edge (Phase 5c)

/** One entry of the Stabilise select (recipe.MatteParams.Stabilise). */
export interface StabiliseOption {
  id: string;
  label: string;
  /** the tooltip: what it does and costs (from the 2026-10-09 experiment) */
  hint: string;
}

/** STABILISE_OPTIONS: Off · Light (the default) · Strong, with the experiment's words. */
export const STABILISE_OPTIONS: readonly StabiliseOption[] = [
  { id: MATTE_STABILISE_OFF, label: 'Off', hint: 'The matte of every frame as the model made it' },
  {
    id: MATTE_STABILISE_LIGHT,
    label: 'Light',
    hint: 'A centred 3-frame median over the matte sequence: removes every single-frame pop in either direction with no lag (default; about 0.1 s per clip)',
  },
  {
    id: MATTE_STABILISE_STRONG,
    label: 'Strong',
    hint: 'The median, then a short hold: keeps parts that drop out for a frame at the cost of a short trail on fast motion',
  },
];

/** stabiliseLabel is the select label of a stabilise mode ('' → "Off"; an unknown mode shows as itself). */
export function stabiliseLabel(mode: string): string {
  return STABILISE_OPTIONS.find((o) => o.id === mode)?.label ?? mode;
}

/** isStabiliseMode: one of the recipe's modes ("" / light / strong). */
export function isStabiliseMode(mode: string): boolean {
  return STABILISE_OPTIONS.some((o) => o.id === mode);
}

/** One entry of the guided model's Edge select. */
export interface EdgeOption {
  /** a segmenter model id, or MATTE_EDGE_NONE */
  id: string;
  text: string;
  disabled: boolean;
  reason: string;
}

/**
 * edgeOptions lists what may refine the tracker's edge band: every
 * segmenter the status offers (the per-device state words it like the
 * Model select) and "None — tracker mask only" last. Without a status the
 * two shipped segmenters are listed.
 */
export function edgeOptions(status: MatteStatus | null | undefined, device = ''): EdgeOption[] {
  const out: EdgeOption[] = [];
  const opts = matteModelOptions(status, device).filter((o) => !o.tracker);
  if (opts.length) for (const o of opts) out.push({ id: o.id, text: o.text, disabled: o.disabled, reason: o.reason });
  else for (const id of MATTE_ORDER) out.push({ id, text: modelLabel(id), disabled: false, reason: '' });
  out.push({ id: MATTE_EDGE_NONE, text: 'None — tracker mask only', disabled: false, reason: '' });
  return out;
}

/**
 * defaultEdgeFor is the edge model a guided matte uses when the op names
 * none: the device's default per-frame model ("General (precise)" on the
 * GPU, "Anime (fast)" on the CPU — what the server resolves "" to), or the
 * first offered segmenter when that default is not a segmenter.
 */
export function defaultEdgeFor(status: MatteStatus | null | undefined, device = ''): string {
  const dev = device || effectiveDevice(status);
  const d = defaultModelFor(status, dev);
  if (!isTracker(status, d)) return d;
  const first = matteModelOptions(status, dev).find((o) => !o.tracker);
  return first?.id ?? MATTE_MODEL_DEFAULT;
}

/** edgeModelFor is the edge model a guided op resolves to: the op's explicit `edge` (MATTE_EDGE_NONE included), else defaultEdgeFor. */
export function edgeModelFor(edge: string | null | undefined, status: MatteStatus | null | undefined, device = ''): string {
  const e = edge?.trim() ?? '';
  return e || defaultEdgeFor(status, device);
}

// ---------------------------------------------------------------------------
// the computed-memo record (Phase 5d: "Use this frame's matte")

/**
 * MatteMemoState is what the SPA knows about one (clip, model, device)
 * memo on the server: 'computed' — a preview of that state came back as a
 * picture, or a mask prompt was answered; 'idle' — a 202 said nothing is
 * on disk; 'unknown' — nothing seen yet (a reload forgets everything: the
 * server's answer to the next request says).
 */
export type MatteMemoState = 'computed' | 'idle' | 'unknown';

/**
 * matteMemoKey names one memo the SPA has seen the state of: the clip
 * (lib/state.matteClipKey — what the server's clip key is made of), the
 * model and the device (the server keys per size / precision, which
 * follow the device). '' when the clip or the model is unknown.
 */
export function matteMemoKey(clipKey: string, model: string, device: string): string {
  if (!clipKey || !model) return '';
  return `${clipKey}|${model}|${device}`;
}

/**
 * maskPromptIdleText words the overlay's status when a mask prompt was
 * answered 202 idle: the edge model's matte of this clip is not computed,
 * so the server has nothing to send — with the server's reason when the
 * 202 carried one.
 */
export function maskPromptIdleText(edgeLabel: string, reason = ''): string {
  const short = shortModelLabel(edgeLabel) || 'edge';
  const r = reason.trim();
  return `${short} matte not computed for this clip — switch the Model to ${edgeLabel}, press Compute, then come back${r ? ` (${r})` : ''}`;
}

// ---------------------------------------------------------------------------
// estimate / verdict

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
 * matteEstimate predicts a pass of `frames` frames with `model` on the
 * effective device from the sidecar's measured ms per frame: null when
 * nothing is known (no status, the model is not offered or has no
 * measurement yet, no frames).
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
 * matte-gpu up -d`" with the server's reason. The state is the model's on
 * the effective device. `estimateMs` is the clip's (matteEstimate; 0 =
 * none). null status = no answer yet. Phase 5c: nothing stays resident, so
 * "ready" alone means downloaded and self-tested — the line says "loaded"
 * when a session is resident (`resident` true) and "not loaded (the first
 * Compute adds the model load)" when the server says it is not; an older
 * server that reports no residency keeps the plain "ready".
 */
export function matteStatusLine(status: MatteStatus | null | undefined, model: string, estimateMs = 0): string {
  if (!status) return 'checking the matte service…';
  const reason = status.reason?.trim() ?? '';
  if (!status.enabled) return `sidecar unavailable — run \`${MATTE_PROFILE_HINT}\`${reason ? ` (${reason})` : ''}`;
  if (status.device === 'unavailable') return `sidecar unavailable — ${reason || 'the device is unavailable'}`;
  const m = matteModel(status, model);
  if (!m) return model ? `model ${model} is not offered by this server` : 'no model selected';
  const d = matteDeviceState(status, model);
  if (!d) return `${modelLabel(model, m)} is not offered on ${deviceLabel(status.device) || 'this device'}`;
  switch (d.state) {
    case 'ready': {
      let s = `${d.resident === true ? 'loaded' : 'ready'} · ${deviceLabel(status.device) || status.device || 'device unknown'}`;
      if (estimateMs > 0) s += ` · up to ~${humanSeconds(estimateMs)} for this clip`;
      if (d.resident === false) s += ' · not loaded (the first Compute adds the model load)';
      return s;
    }
    case 'loading':
      return 'loading model…';
    case 'downloading':
      return `downloading weights ${Math.round(d.percent ?? 0)} %`;
    default:
      return `${modelLabel(model, m)} ${d.state} — ${d.reason?.trim() || 'see the sidecar log'}`;
  }
}

// ---------------------------------------------------------------------------
// pending (202) and the compute state

/** PendingView is what a still / proxy view keeps while its answer is 202: the pending fields plus when they first appeared. */
export interface PendingView extends MattePendingInfo {
  /** Date.now() of the first 202 of this run (kept across updates): the "(12 s)" of the loading pill */
  since: number;
}

/** nextPending folds a new 202 into the view: the fields are the new answer's, `since` stays from the first. */
export function nextPending(prev: PendingView | null, info: MattePendingInfo, now: number): PendingView {
  return { ...info, since: prev?.since ?? now };
}

/** isIdleState: the 202 says nothing runs and nothing is on disk — the Compute button starts it ("idle"; a 5b server's "deferred"). */
export function isIdleState(state: MattePendingInfo['state'] | null | undefined): boolean {
  return state === 'idle' || state === 'deferred';
}

/** isTracking: a running guided pass whose masks have not arrived yet (the 202's phase "tracking" — the whole clip is one POST). */
export function isTracking(p: Pick<MattePendingInfo, 'state' | 'phase' | 'done' | 'total'>): boolean {
  return p.state === 'running' && p.phase === MATTE_PHASE_TRACKING && p.total > 0 && p.done < p.total;
}

/**
 * mattePendingPill is the pill text over a pending preview (spec §6.2 /
 * §9): "AI matte 24/45 · GPU" while the pass runs ("AI matte: tracking 45
 * frames · GPU" while a guided pass's one POST is in flight), "AI matte:
 * loading model… (12 s)", "AI matte: downloading weights 43 %", "AI matte
 * not computed" when idle (the Compute button sits next to it), and for a
 * 5b server's deferred pass "AI matte: ~3 min on CPU" (with "Compute now").
 */
export function mattePendingPill(p: PendingView, now: number): string {
  const dev = deviceLabel(p.device);
  switch (p.state) {
    case 'idle':
      return 'AI matte not computed';
    case 'deferred':
      return p.estimateMs > 0 ? `AI matte: ~${humanSeconds(p.estimateMs)} on ${dev || 'the matte service'}` : 'AI matte: not computed yet';
    case 'loading':
      return `AI matte: loading model… (${Math.max(0, Math.floor((now - p.since) / 1000))} s)`;
    case 'downloading':
      return `AI matte: downloading weights ${Math.round(p.percent)} %`;
    default: {
      const tail = dev ? ` · ${dev}` : '';
      if (isTracking(p)) return `AI matte: tracking ${p.total} frames${tail}`;
      return p.total > 0 ? `AI matte ${p.done}/${p.total}${tail}` : `AI matte ${p.done} ${p.done === 1 ? 'frame' : 'frames'}${tail}`;
    }
  }
}

/** matteRetryMs is the re-request delay for a pending state: the poll cadence for an idle / deferred pass, MATTE_RETRY_MS otherwise. */
export function matteRetryMs(state: MattePendingInfo['state']): number {
  return isIdleState(state) ? MATTE_IDLE_RETRY_MS : MATTE_RETRY_MS;
}

/**
 * MatteComputeState is what the Background card's Compute matte button
 * reflects (lib/matte.svelte keeps it; the Preview derives it from its
 * still): 'none' = no matte op on the stage (nothing to compute — the
 * guided model without prompts, crop / eyedropper / prompt mode, no
 * source); 'unknown' = the still of the current state has not answered
 * yet; 'idle' = the server said nothing is on disk; 'running' = a pass or
 * a model load is under way (done / total from the 202); 'computed' = the
 * still of the current state arrived, so the memo exists.
 */
export interface MatteComputeState {
  state: 'none' | 'unknown' | 'idle' | 'running' | 'computed';
  done: number;
  total: number;
  /** "GPU" / "CPU" ('' unknown) */
  device: string;
  /** the pending sub-state while running (running / loading / downloading, or "tracking" while a guided pass's one POST is in flight) */
  pending: string;
}

/** NO_COMPUTE is the compute state with nothing to compute. */
export const NO_COMPUTE: MatteComputeState = { state: 'none', done: 0, total: 0, device: '', pending: '' };

/**
 * computeFromPending derives the compute state of a matte op's still from
 * its 202 answer (null = a picture arrived): idle / deferred → 'idle',
 * running / loading / downloading → 'running' with the pass's counts.
 */
export function computeFromPending(p: MattePendingInfo | null, hasPicture: boolean): MatteComputeState {
  if (!p) return hasPicture ? { ...NO_COMPUTE, state: 'computed' } : { ...NO_COMPUTE, state: 'unknown' };
  if (isIdleState(p.state)) return { state: 'idle', done: 0, total: p.total, device: deviceLabel(p.device), pending: p.state };
  return { state: 'running', done: p.done, total: p.total, device: deviceLabel(p.device), pending: isTracking(p) ? MATTE_PHASE_TRACKING : p.state };
}

/**
 * computeButtonText words the Compute matte button and its note: the
 * button reads "Compute matte" (enabled when idle, or unknown — a click is
 * harmless: the server answers from its memo), "computing… 24/45" while a
 * pass runs ("loading model…" / "downloading weights 43 %" for the load
 * states) and "computed" once the memo exists.
 */
export function computeButtonText(c: MatteComputeState): { text: string; disabled: boolean; title: string } {
  switch (c.state) {
    case 'computed':
      return { text: 'computed', disabled: true, title: 'The matte of this clip, model and device is cached on the server' };
    case 'running': {
      let text = 'computing…';
      if (c.pending === 'loading') text = 'loading model…';
      else if (c.pending === 'downloading') text = 'downloading weights…';
      else if (c.pending === MATTE_PHASE_TRACKING) text = c.total > 0 ? `tracking… ${c.total} frames` : 'tracking…';
      else if (c.total > 0) text = `computing… ${c.done}/${c.total}`;
      return { text, disabled: true, title: `The AI matte pass runs${c.device ? ` on the ${c.device}` : ''}` };
    }
    case 'none':
      return { text: 'Compute matte', disabled: true, title: 'Nothing to compute: no matte on the preview (guided: select the subject first)' };
    case 'idle':
      return { text: 'Compute matte', disabled: false, title: `Run the AI matte pass for this clip now${c.device ? ` (${c.device})` : ''} — Render runs it anyway` };
    default:
      return { text: 'Compute matte', disabled: false, title: 'Run the AI matte pass for this clip now — Render runs it anyway' };
  }
}
