// The pure AI matte helpers (Phase 5b): device / duration words, the Model
// select's options, the clip estimate and the caps verdict mirroring jobs'
// up-front refusal, the Background card's status line, the preview's
// pending pill and the retry cadence.
import { describe, expect, it } from 'vitest';
import type { MatteStatus } from './api';
import {
  computeButtonText,
  computeFromPending,
  defaultEdgeFor,
  defaultModelFor,
  deviceLabel,
  edgeModelFor,
  edgeOptions,
  effectiveDevice,
  humanSeconds,
  isIdleState,
  isStabiliseMode,
  isTracker,
  maskPromptIdleText,
  matteMemoKey,
  shortModelLabel,
  MATTE_DEFERRED_RETRY_MS,
  MATTE_FRAME_OVERHEAD_MS,
  MATTE_IDLE_RETRY_MS,
  MATTE_POLL_MS,
  MATTE_PROFILE_HINT,
  MATTE_RETRY_MS,
  MATTE_UNLOAD_DEBOUNCE_MS,
  matteDevices,
  matteDeviceState,
  matteEstimate,
  matteEstimateSuffix,
  matteModel,
  matteModelFor,
  matteModelOptions,
  matteMsPerFrame,
  mattePendingPill,
  matteRetryMs,
  matteStatusLine,
  matteVerdict,
  modelLabel,
  nextPending,
  NO_COMPUTE,
  STABILISE_OPTIONS,
  stabiliseLabel,
  type PendingView,
} from './matte';

/** a GPU install with both models: the fast one ready, the precise one loading */
function gpu(over: Partial<MatteStatus> = {}): MatteStatus {
  return {
    enabled: true,
    device: 'cuda',
    gpu: { name: 'NVIDIA GeForce RTX 5080', totalGiB: 16, freeGiB: 12.4 },
    defaultModel: 'isnet-anime',
    models: {
      'birefnet-lite': { label: 'General (precise)', state: 'loading', percent: 60, msPerFrame: 170, licence: 'MIT' },
      'isnet-anime': { label: 'Anime (fast)', state: 'ready', msPerFrame: 18, licence: 'Apache-2.0', sizes: [1024, 512] },
    },
    maxSeconds: 600,
    maxFrames: 3000,
    ...over,
  };
}
/** a CPU install: isnet alone at 136 ms, lite unavailable with a reason */
function cpu(): MatteStatus {
  return gpu({
    device: 'cpu',
    gpu: null,
    models: {
      'isnet-anime': { label: '', state: 'ready', msPerFrame: 136 },
      'birefnet-lite': { label: '', state: 'unavailable', reason: 'cpu: 14 GiB of RAM needed, 9 GiB available' },
    },
  });
}
/** the feature off: a plain install without the compose profile */
const off: MatteStatus = { enabled: false, device: '', reason: 'no matte service is configured (EZLG_MATTE_URL is empty — start the matte or matte-gpu compose profile)', defaultModel: 'isnet-anime', models: {}, maxSeconds: 600, maxFrames: 3000 };

describe('words', () => {
  it('deviceLabel: cuda → GPU, cpu → CPU, anything else as reported', () => {
    expect(deviceLabel('cuda')).toBe('GPU');
    expect(deviceLabel('cpu')).toBe('CPU');
    expect(deviceLabel('unavailable')).toBe('unavailable');
    expect(deviceLabel('')).toBe('');
    expect(deviceLabel(undefined)).toBe('');
  });

  it('humanSeconds mirrors jobs: rounded seconds under two minutes, rounded minutes from there', () => {
    expect(humanSeconds(0)).toBe('0 s');
    expect(humanSeconds(499)).toBe('0 s');
    expect(humanSeconds(500)).toBe('1 s');
    expect(humanSeconds(810)).toBe('1 s');
    expect(humanSeconds(12_400)).toBe('12 s');
    expect(humanSeconds(119_499)).toBe('119 s');
    expect(humanSeconds(119_500)).toBe('2 min');
    expect(humanSeconds(180_000)).toBe('3 min');
    expect(humanSeconds(1_620_000)).toBe('27 min');
    expect(humanSeconds(-5)).toBe('0 s');
  });

  it('modelLabel: the sidecar’s label, else the shipped id’s, else the id', () => {
    expect(modelLabel('isnet-anime', { label: 'Anime', state: 'ready' })).toBe('Anime');
    expect(modelLabel('isnet-anime', { label: '  ', state: 'ready' })).toBe('Anime (fast)');
    expect(modelLabel('birefnet-lite')).toBe('General (precise)');
    expect(modelLabel('rmbg-2', null)).toBe('rmbg-2');
  });

  it('the cadences and the hint are what the spec names', () => {
    expect(MATTE_RETRY_MS).toBe(500);
    expect(MATTE_POLL_MS).toBe(5000);
    expect(MATTE_IDLE_RETRY_MS).toBe(MATTE_POLL_MS);
    expect(MATTE_DEFERRED_RETRY_MS).toBe(MATTE_IDLE_RETRY_MS);
    expect(MATTE_UNLOAD_DEBOUNCE_MS).toBe(1500);
    expect(MATTE_FRAME_OVERHEAD_MS).toBe(2);
    expect(MATTE_PROFILE_HINT).toBe('docker compose --profile matte-gpu up -d');
    expect(matteRetryMs('running')).toBe(500);
    expect(matteRetryMs('loading')).toBe(500);
    expect(matteRetryMs('downloading')).toBe(500);
    expect(matteRetryMs('deferred')).toBe(5000);
    expect(matteRetryMs('idle')).toBe(5000);
    expect(isIdleState('idle')).toBe(true);
    expect(isIdleState('deferred')).toBe(true);
    expect(isIdleState('running')).toBe(false);
    expect(isIdleState(null)).toBe(false);
  });
});

describe('model options', () => {
  it('lists the shipped ids first, then the rest alphabetically; loading / downloading show their state, missing / unavailable are disabled with the reason', () => {
    const st = gpu({
      models: {
        'zeta-model': { label: 'Zeta', state: 'ready', msPerFrame: 50 },
        'birefnet-lite': { label: 'General (precise)', state: 'downloading', percent: 43.4 },
        'alpha-model': { label: 'Alpha', state: 'missing', reason: 'download failed: 404' },
        'isnet-anime': { label: '', state: 'ready', msPerFrame: 18 },
        'other': { label: 'Other', state: 'unavailable', reason: 'needs about 7 GB of free GPU memory, 5.9 GB free' },
      },
    });
    const opts = matteModelOptions(st);
    expect(opts.map((o) => o.id)).toEqual(['isnet-anime', 'birefnet-lite', 'alpha-model', 'other', 'zeta-model']);
    expect(opts[0]).toMatchObject({ label: 'Anime (fast)', text: 'Anime (fast)', state: 'ready', disabled: false, msPerFrame: 18, reason: '' });
    expect(opts[1]).toMatchObject({ text: 'General (precise) — downloading 43 %', percent: 43, disabled: false, msPerFrame: 0 });
    expect(opts[2]).toMatchObject({ text: 'Alpha — missing', disabled: true, reason: 'download failed: 404' });
    expect(opts[3]).toMatchObject({ text: 'Other — unavailable', disabled: true, reason: 'needs about 7 GB of free GPU memory, 5.9 GB free' });
    expect(opts[4]).toMatchObject({ text: 'Zeta', disabled: false });
    expect(matteModelOptions(gpu())[1].text).toBe('General (precise) — loading…');
    // an unknown state from a newer sidecar is shown, never hidden
    expect(matteModelOptions(gpu({ models: { 'isnet-anime': { label: 'A', state: 'warming' } } }))[0].text).toBe('A — warming');
    expect(matteModelOptions(null)).toEqual([]);
    expect(matteModelOptions(off)).toEqual([]);
    expect(matteModelOptions(gpu({ models: null }))).toEqual([]);
  });

  it('matteModelFor: the explicit choice, else the server default, else the recipe default; matteModel / matteMsPerFrame read one entry', () => {
    expect(matteModelFor('birefnet-lite', gpu())).toBe('birefnet-lite');
    expect(matteModelFor('', gpu({ defaultModel: 'birefnet-lite' }))).toBe('birefnet-lite');
    expect(matteModelFor('', gpu({ defaultModel: ' ' }))).toBe('isnet-anime');
    expect(matteModelFor('', null)).toBe('isnet-anime');
    expect(matteModel(gpu(), 'isnet-anime')?.msPerFrame).toBe(18);
    expect(matteModel(gpu(), 'nope')).toBeNull();
    expect(matteModel(null, 'isnet-anime')).toBeNull();
    expect(matteMsPerFrame(gpu(), 'isnet-anime')).toBe(18);
    expect(matteMsPerFrame(gpu(), 'nope')).toBe(0);
    expect(matteMsPerFrame(gpu({ models: { 'isnet-anime': { label: '', state: 'ready', msPerFrame: NaN } } }), 'isnet-anime')).toBe(0);
  });
});

describe('estimate and verdict (jobs’ up-front refusal: frames × (msPerFrame + 2 ms) against the caps)', () => {
  it('matteEstimate / matteEstimateSuffix: known ms per frame only', () => {
    expect(matteEstimate(gpu(), 'isnet-anime', 45)).toEqual({ frames: 45, msPerFrame: 18, ms: 900, device: 'GPU' });
    expect(matteEstimateSuffix(matteEstimate(gpu(), 'isnet-anime', 45))).toBe(' · up to ~1 s AI matte (GPU)');
    expect(matteEstimateSuffix(matteEstimate(cpu(), 'isnet-anime', 300))).toBe(' · up to ~41 s AI matte (CPU)');
    expect(matteEstimate(gpu(), 'birefnet-lite', 45)?.ms).toBe(45 * 172); // a loading model's measurement still counts
    expect(matteEstimate(cpu(), 'birefnet-lite', 45)).toBeNull(); // no measurement
    expect(matteEstimate(gpu(), 'isnet-anime', 0)).toBeNull();
    expect(matteEstimate(null, 'isnet-anime', 45)).toBeNull();
    expect(matteEstimateSuffix(null)).toBe('');
    expect(matteEstimateSuffix({ frames: 1, msPerFrame: 1, ms: 3, device: '' })).toBe(' · up to ~0 s AI matte');
  });

  it('the frames cap binds first, then the seconds cap; a 0 cap never binds; nothing binds without a status', () => {
    expect(matteVerdict(gpu(), 'isnet-anime', 45)).toEqual({ over: '', note: '', estimate: { frames: 45, msPerFrame: 18, ms: 900, device: 'GPU' } });
    const frames = matteVerdict(gpu(), 'isnet-anime', 3001);
    expect(frames.over).toBe('frames');
    expect(frames.note).toBe("An AI matte of 3001 frames is over this server's 3000-frame cap (EZLG_MATTE_MAX_FRAMES), so Render will be refused (unless this clip’s matte is already cached): trim the clip or lower the fps.");
    // 1800 frames of CPU lite at ~900 ms: ~27 min, over 600 s
    const lite = cpu();
    lite.models!['birefnet-lite'] = { label: '', state: 'ready', msPerFrame: 898 };
    const seconds = matteVerdict(lite, 'birefnet-lite', 1800);
    expect(seconds.over).toBe('seconds');
    expect(seconds.estimate?.ms).toBe(1800 * 900);
    expect(seconds.note).toBe(
      "An AI matte of 1800 frames would take up to ~27 min on this server's CPU — over its 600 s cap (EZLG_MATTE_MAX_SECONDS), so Render will be refused (unless this clip’s matte is already cached): trim the clip, lower the fps or pick a faster model.",
    );
    // exactly at the caps is fine
    expect(matteVerdict(gpu(), 'isnet-anime', 3000).over).toBe('');
    expect(matteVerdict(gpu({ maxSeconds: 1 }), 'isnet-anime', 50).over).toBe(''); // 50 × 20 = 1000 ms = 1 s
    expect(matteVerdict(gpu({ maxSeconds: 1 }), 'isnet-anime', 51).over).toBe('seconds');
    // the frames cap wins when both are over
    expect(matteVerdict(gpu({ maxSeconds: 1 }), 'isnet-anime', 5000).over).toBe('frames');
    // unknown caps / no measurement / no status
    expect(matteVerdict(gpu({ maxFrames: 0, maxSeconds: 0 }), 'isnet-anime', 100_000).over).toBe('');
    expect(matteVerdict(cpu(), 'birefnet-lite', 2000).over).toBe('');
    expect(matteVerdict(null, 'isnet-anime', 100_000)).toEqual({ over: '', note: '', estimate: null });
    expect(matteVerdict(gpu(), 'isnet-anime', 0)).toEqual({ over: '', note: '', estimate: null });
  });
});

describe('matteStatusLine', () => {
  it('ready · device · the clip’s estimate; loading; downloading %; a model’s reason; the sidecar unavailable with the profile hint', () => {
    expect(matteStatusLine(gpu(), 'isnet-anime', 900)).toBe('ready · GPU · up to ~1 s for this clip');
    expect(matteStatusLine(gpu(), 'isnet-anime')).toBe('ready · GPU');
    // Phase 5c residency: "loaded" when a session is resident, "not loaded" when the server says it is not, plain "ready" when it does not say
    expect(matteStatusLine(gpu({ models: { 'isnet-anime': { label: '', state: 'ready', msPerFrame: 18, resident: true } } }), 'isnet-anime', 900)).toBe('loaded · GPU · up to ~1 s for this clip');
    expect(matteStatusLine(gpu({ models: { 'isnet-anime': { label: '', state: 'ready', msPerFrame: 18, resident: false } } }), 'isnet-anime', 900)).toBe(
      'ready · GPU · up to ~1 s for this clip · not loaded (the first Compute adds the model load)',
    );
    expect(matteStatusLine(gpu({ models: { 'isnet-anime': { label: '', state: 'ready', msPerFrame: 18, resident: false } } }), 'isnet-anime')).toBe('ready · GPU · not loaded (the first Compute adds the model load)');
    const resident = both({
      models: { ...both().models, 'birefnet-lite': { label: 'General (precise)', state: 'ready', msPerFrame: 170, devices: { cuda: { state: 'ready', msPerFrame: 170, resident: true }, cpu: { state: 'ready', msPerFrame: 4000, resident: false } } } },
    });
    expect(matteStatusLine(resident, 'birefnet-lite', 7650)).toBe('loaded · GPU · up to ~8 s for this clip');
    expect(matteStatusLine({ ...resident, device: 'cpu' }, 'birefnet-lite')).toBe('ready · CPU · not loaded (the first Compute adds the model load)');
    expect(matteStatusLine(cpu(), 'isnet-anime', 40_800)).toBe('ready · CPU · up to ~41 s for this clip');
    expect(matteStatusLine(gpu(), 'birefnet-lite', 7000)).toBe('loading model…');
    expect(matteStatusLine(gpu({ models: { 'birefnet-lite': { label: '', state: 'downloading', percent: 43 } } }), 'birefnet-lite')).toBe('downloading weights 43 %');
    expect(matteStatusLine(cpu(), 'birefnet-lite')).toBe('General (precise) unavailable — cpu: 14 GiB of RAM needed, 9 GiB available');
    expect(matteStatusLine(gpu({ models: { 'isnet-anime': { label: '', state: 'missing' } } }), 'isnet-anime')).toBe('Anime (fast) missing — see the sidecar log');
    expect(matteStatusLine(gpu(), 'nope')).toBe('model nope is not offered by this server');
    expect(matteStatusLine(gpu(), '')).toBe('no model selected');
    expect(matteStatusLine(null, 'isnet-anime')).toBe('checking the matte service…');
    expect(matteStatusLine(off, 'isnet-anime')).toBe(
      'sidecar unavailable — run `docker compose --profile matte-gpu up -d` (no matte service is configured (EZLG_MATTE_URL is empty — start the matte or matte-gpu compose profile))',
    );
    expect(matteStatusLine({ ...off, reason: '' }, 'isnet-anime')).toBe('sidecar unavailable — run `docker compose --profile matte-gpu up -d`');
    // the profile IS up but the device is not: the sidecar's own reason, no profile hint
    expect(matteStatusLine(gpu({ device: 'unavailable', reason: 'CUDA EP not available — nvidia-container-toolkit / driver ≥ 580?' }), 'isnet-anime')).toBe(
      'sidecar unavailable — CUDA EP not available — nvidia-container-toolkit / driver ≥ 580?',
    );
    expect(matteStatusLine(gpu({ device: 'unavailable', reason: '' }), 'isnet-anime')).toBe('sidecar unavailable — the device is unavailable');
  });
});

describe('pending pill', () => {
  const base = { done: 0, total: 0, percent: 0, estimateMs: 0, device: 'cuda' };
  it('nextPending keeps `since` from the first answer and takes the fields of the latest', () => {
    const first = nextPending(null, { ...base, state: 'running', done: 1, total: 45 }, 1000);
    expect(first).toEqual({ ...base, state: 'running', done: 1, total: 45, since: 1000 });
    const next = nextPending(first, { ...base, state: 'running', done: 24, total: 45 }, 5000);
    expect(next).toEqual({ ...base, state: 'running', done: 24, total: 45, since: 1000 });
  });

  it('words every state like the spec: running counts, loading with the wait, downloading %, deferred with the estimate', () => {
    const p = (over: Partial<PendingView>): PendingView => ({ ...base, state: 'running', since: 10_000, ...over });
    expect(mattePendingPill(p({ done: 24, total: 45 }), 11_000)).toBe('AI matte 24/45 · GPU');
    expect(mattePendingPill(p({ done: 24, total: 45, device: 'cpu' }), 11_000)).toBe('AI matte 24/45 · CPU');
    expect(mattePendingPill(p({ done: 7, total: 0, device: '' }), 11_000)).toBe('AI matte 7 frames');
    expect(mattePendingPill(p({ done: 1, total: 0 }), 11_000)).toBe('AI matte 1 frame · GPU');
    expect(mattePendingPill(p({ state: 'loading' }), 22_400)).toBe('AI matte: loading model… (12 s)');
    expect(mattePendingPill(p({ state: 'loading' }), 9_000)).toBe('AI matte: loading model… (0 s)');
    expect(mattePendingPill(p({ state: 'downloading', percent: 43 }), 11_000)).toBe('AI matte: downloading weights 43 %');
    expect(mattePendingPill(p({ state: 'deferred', estimateMs: 180_000, device: 'cpu' }), 11_000)).toBe('AI matte: ~3 min on CPU');
    expect(mattePendingPill(p({ state: 'deferred', estimateMs: 0, device: 'cpu' }), 11_000)).toBe('AI matte: not computed yet');
    expect(mattePendingPill(p({ state: 'deferred', estimateMs: 95_000, device: '' }), 11_000)).toBe('AI matte: ~95 s on the matte service');
    // Phase 5c: idle — nothing started; the Compute button sits next to it
    expect(mattePendingPill(p({ state: 'idle', total: 45 }), 11_000)).toBe('AI matte not computed');
    // a guided pass: one POST carries the whole clip, so the pill says "tracking N frames" until the masks arrive
    expect(mattePendingPill(p({ total: 45, phase: 'tracking' }), 11_000)).toBe('AI matte: tracking 45 frames · GPU');
    expect(mattePendingPill(p({ total: 45, phase: 'tracking', device: '' }), 11_000)).toBe('AI matte: tracking 45 frames');
    expect(mattePendingPill(p({ done: 45, total: 45, phase: 'tracking' }), 11_000)).toBe('AI matte 45/45 · GPU');
    expect(mattePendingPill(p({ state: 'loading', phase: 'tracking' }), 10_000)).toBe('AI matte: loading model… (0 s)');
  });
});

// ---- Phase 5c: devices, the guided tracker, stabilise / edge, the compute state
/** a 5c GPU install offering both devices: lite the GPU default, isnet the CPU default, the tracker on the GPU only */
function both(over: Partial<MatteStatus> = {}): MatteStatus {
  return {
    enabled: true,
    device: 'cuda',
    devices: ['cpu', 'cuda'],
    gpu: { name: 'NVIDIA GeForce RTX 5080', totalGiB: 16, freeGiB: 12.4 },
    defaultModel: 'birefnet-lite',
    defaultModels: { cuda: 'birefnet-lite', cpu: 'isnet-anime' },
    models: {
      'sam2-tiny': {
        label: 'Guided (click to select)',
        kind: 'tracker',
        state: 'ready',
        msPerFrame: 60,
        licence: 'Apache-2.0',
        devices: { cuda: { state: 'ready', precision: 'bf16', msPerFrame: 60 } },
      },
      'birefnet-lite': {
        label: 'General (precise)',
        kind: 'segmenter',
        state: 'ready',
        msPerFrame: 170,
        devices: { cuda: { state: 'ready', precision: 'fp32', size: 1024, msPerFrame: 170 }, cpu: { state: 'unavailable', reason: 'cpu: 14 GiB of RAM needed, 9 GiB available' } },
      },
      'isnet-anime': {
        label: 'Anime (fast)',
        state: 'ready',
        msPerFrame: 18,
        devices: { cuda: { state: 'ready', precision: 'fp16', size: 1024, msPerFrame: 18 }, cpu: { state: 'downloading', percent: 43, precision: 'fp32', size: 512, msPerFrame: 136 } },
      },
    },
    maxSeconds: 600,
    maxFrames: 3000,
    ...over,
  };
}

describe('devices (Phase 5c)', () => {
  it('matteDevices: the offered list GPU first; a 5b status offers exactly its device; nothing when off', () => {
    expect(matteDevices(both())).toEqual(['cuda', 'cpu']);
    expect(matteDevices(both({ devices: ['cpu'] }))).toEqual(['cpu']);
    expect(matteDevices(both({ devices: ['cpu', 'cpu', 'npu'] }))).toEqual(['cpu', 'npu']);
    expect(matteDevices(gpu())).toEqual(['cuda']);
    expect(matteDevices(cpu())).toEqual(['cpu']);
    expect(matteDevices(gpu({ device: 'unavailable' }))).toEqual([]);
    expect(matteDevices(off)).toEqual([]);
    expect(matteDevices(null)).toEqual([]);
  });

  it('effectiveDevice / defaultModelFor read the status (the per-device map, else the mirrored default, else the recipe default)', () => {
    expect(effectiveDevice(both())).toBe('cuda');
    expect(effectiveDevice(both({ device: 'cpu' }))).toBe('cpu');
    expect(effectiveDevice(gpu({ device: 'unavailable' }))).toBe('');
    expect(effectiveDevice(null)).toBe('');
    expect(defaultModelFor(both(), 'cpu')).toBe('isnet-anime');
    expect(defaultModelFor(both(), 'cuda')).toBe('birefnet-lite');
    expect(defaultModelFor(both(), 'npu')).toBe('birefnet-lite'); // falls back to the mirrored default
    expect(defaultModelFor(gpu(), 'cuda')).toBe('isnet-anime');
    expect(defaultModelFor(null, 'cuda')).toBe('isnet-anime');
  });

  it('matteDeviceState / matteMsPerFrame answer per device, mirroring the top-level fields on a 5b status', () => {
    expect(matteDeviceState(both(), 'isnet-anime')).toMatchObject({ state: 'ready', precision: 'fp16', size: 1024, msPerFrame: 18 }); // '' = the effective device
    expect(matteDeviceState(both(), 'isnet-anime', 'cpu')).toMatchObject({ state: 'downloading', percent: 43, msPerFrame: 136 });
    expect(matteDeviceState(both(), 'sam2-tiny', 'cpu')).toBeNull(); // not on that device
    expect(matteDeviceState(both(), 'nope')).toBeNull();
    expect(matteDeviceState(gpu(), 'isnet-anime', 'cpu')).toEqual({ state: 'ready', reason: undefined, percent: undefined, msPerFrame: 18 }); // 5b: no map → the one device's
    expect(matteMsPerFrame(both(), 'isnet-anime')).toBe(18);
    expect(matteMsPerFrame(both(), 'isnet-anime', 'cpu')).toBe(136);
    expect(matteMsPerFrame(both({ device: 'cpu' }), 'isnet-anime')).toBe(136); // the effective device moved
    expect(matteMsPerFrame(both(), 'birefnet-lite', 'cpu')).toBe(0);
    expect(matteEstimate(both({ device: 'cpu' }), 'isnet-anime', 10)).toEqual({ frames: 10, msPerFrame: 136, ms: 1380, device: 'CPU' });
  });

  it('the Model options follow the device: states, disabled with the reason, the tracker last and flagged; the status line says where it is not offered', () => {
    const cuda = matteModelOptions(both());
    expect(cuda.map((o) => o.id)).toEqual(['isnet-anime', 'birefnet-lite', 'sam2-tiny']);
    expect(cuda.map((o) => o.tracker)).toEqual([false, false, true]);
    expect(cuda[2]).toMatchObject({ text: 'Guided (click to select)', disabled: false, msPerFrame: 60 });
    const cpu5c = matteModelOptions(both(), 'cpu');
    expect(cpu5c[0]).toMatchObject({ text: 'Anime (fast) — downloading 43 %', disabled: false, msPerFrame: 136 });
    expect(cpu5c[1]).toMatchObject({ text: 'General (precise) — unavailable', disabled: true, reason: 'cpu: 14 GiB of RAM needed, 9 GiB available' });
    expect(cpu5c[2]).toMatchObject({ text: 'Guided (click to select) — unavailable', disabled: true, reason: 'not offered on CPU', tracker: true });
    expect(matteModelOptions(both({ device: 'cpu' }))[0].text).toBe('Anime (fast) — downloading 43 %'); // '' = the effective device
    expect(isTracker(both(), 'sam2-tiny')).toBe(true);
    expect(isTracker(both(), 'birefnet-lite')).toBe(false);
    expect(isTracker(null, 'sam2-tiny')).toBe(true); // the shipped id, no status yet
    expect(isTracker(both({ models: { 'sam2-tiny': { label: 'x', kind: 'segmenter', state: 'ready' } } }), 'sam2-tiny')).toBe(false); // the status wins
    expect(matteStatusLine(both({ device: 'cpu' }), 'sam2-tiny')).toBe('Guided (click to select) is not offered on CPU');
    expect(matteStatusLine(both({ device: 'cpu' }), 'isnet-anime')).toBe('downloading weights 43 %');
    expect(matteStatusLine(both({ device: 'cpu' }), 'birefnet-lite')).toBe('General (precise) unavailable — cpu: 14 GiB of RAM needed, 9 GiB available');
    expect(matteStatusLine(both(), 'birefnet-lite', 7650)).toBe('ready · GPU · up to ~8 s for this clip');
  });
});

describe('stabilise / edge (Phase 5c)', () => {
  it('STABILISE_OPTIONS are Off · Light · Strong with hints; stabiliseLabel / isStabiliseMode', () => {
    expect(STABILISE_OPTIONS.map((o) => o.id)).toEqual(['', 'light', 'strong']);
    expect(STABILISE_OPTIONS.map((o) => o.label)).toEqual(['Off', 'Light', 'Strong']);
    expect(STABILISE_OPTIONS[1].hint).toContain('3-frame median');
    expect(STABILISE_OPTIONS[2].hint).toContain('short trail on fast motion');
    expect(stabiliseLabel('')).toBe('Off');
    expect(stabiliseLabel('strong')).toBe('Strong');
    expect(stabiliseLabel('median5')).toBe('median5');
    expect(isStabiliseMode('light')).toBe(true);
    expect(isStabiliseMode('')).toBe(true);
    expect(isStabiliseMode('median5')).toBe(false);
  });

  it('edgeOptions lists the segmenters per device then "None"; defaultEdgeFor is the device’s default per-frame model', () => {
    expect(edgeOptions(both()).map((o) => o.id)).toEqual(['isnet-anime', 'birefnet-lite', 'none']);
    expect(edgeOptions(both())[2]).toEqual({ id: 'none', text: 'None — tracker mask only', disabled: false, reason: '' });
    expect(edgeOptions(both(), 'cpu')[1]).toMatchObject({ id: 'birefnet-lite', disabled: true });
    expect(edgeOptions(null).map((o) => o.id)).toEqual(['isnet-anime', 'birefnet-lite', 'none']);
    expect(defaultEdgeFor(both())).toBe('birefnet-lite');
    expect(defaultEdgeFor(both(), 'cpu')).toBe('isnet-anime');
    expect(defaultEdgeFor(both({ defaultModels: { cuda: 'sam2-tiny' } }))).toBe('isnet-anime'); // a tracker default → the first segmenter
    expect(defaultEdgeFor(null)).toBe('isnet-anime');
  });
});

describe('compute state (Phase 5c)', () => {
  const base = { done: 0, total: 0, percent: 0, estimateMs: 0, device: 'cuda' };
  it('computeFromPending: idle / deferred → idle, the pass states → running with the counts, a picture → computed, nothing yet → unknown', () => {
    expect(computeFromPending({ ...base, state: 'idle', total: 45 }, false)).toEqual({ state: 'idle', done: 0, total: 45, device: 'GPU', pending: 'idle' });
    expect(computeFromPending({ ...base, state: 'deferred' }, false).state).toBe('idle');
    expect(computeFromPending({ ...base, state: 'running', done: 24, total: 45 }, false)).toEqual({ state: 'running', done: 24, total: 45, device: 'GPU', pending: 'running' });
    expect(computeFromPending({ ...base, state: 'loading', device: 'cpu' }, false)).toMatchObject({ state: 'running', device: 'CPU', pending: 'loading' });
    expect(computeFromPending({ ...base, state: 'running', total: 45, phase: 'tracking' }, false)).toEqual({ state: 'running', done: 0, total: 45, device: 'GPU', pending: 'tracking' });
    expect(computeFromPending({ ...base, state: 'running', done: 45, total: 45, phase: 'tracking' }, false).pending).toBe('running');
    expect(computeFromPending(null, true)).toEqual({ ...NO_COMPUTE, state: 'computed' });
    expect(computeFromPending(null, false)).toEqual({ ...NO_COMPUTE, state: 'unknown' });
  });

  it('computeButtonText words the button: enabled when idle / unknown, the progress while running, "computed" once cached', () => {
    expect(computeButtonText(NO_COMPUTE)).toMatchObject({ text: 'Compute matte', disabled: true });
    expect(computeButtonText({ ...NO_COMPUTE, state: 'idle', device: 'GPU' })).toMatchObject({ text: 'Compute matte', disabled: false });
    expect(computeButtonText({ ...NO_COMPUTE, state: 'idle', device: 'GPU' }).title).toContain('(GPU)');
    expect(computeButtonText({ ...NO_COMPUTE, state: 'unknown' })).toMatchObject({ text: 'Compute matte', disabled: false });
    expect(computeButtonText({ state: 'running', done: 24, total: 45, device: 'GPU', pending: 'running' })).toMatchObject({ text: 'computing… 24/45', disabled: true });
    expect(computeButtonText({ state: 'running', done: 0, total: 0, device: '', pending: 'running' }).text).toBe('computing…');
    expect(computeButtonText({ state: 'running', done: 0, total: 0, device: '', pending: 'loading' }).text).toBe('loading model…');
    expect(computeButtonText({ state: 'running', done: 0, total: 0, device: '', pending: 'downloading' }).text).toBe('downloading weights…');
    expect(computeButtonText({ state: 'running', done: 0, total: 45, device: 'GPU', pending: 'tracking' })).toMatchObject({ text: 'tracking… 45 frames', disabled: true });
    expect(computeButtonText({ state: 'running', done: 0, total: 0, device: '', pending: 'tracking' }).text).toBe('tracking…');
    expect(computeButtonText({ ...NO_COMPUTE, state: 'computed' })).toMatchObject({ text: 'computed', disabled: true });
  });
});

describe('the mask prompt helpers (Phase 5d)', () => {
  it('shortModelLabel drops the qualifier; edgeModelFor resolves "" to the device default; matteMemoKey needs a clip and a model', () => {
    expect(shortModelLabel('General (precise)')).toBe('General');
    expect(shortModelLabel('Anime (fast)')).toBe('Anime');
    expect(shortModelLabel('sam2-tiny')).toBe('sam2-tiny');
    expect(shortModelLabel('(x)')).toBe('(x)');
    expect(shortModelLabel('  ')).toBe('');
    const s = gpu({ devices: ['cuda', 'cpu'], defaultModels: { cuda: 'birefnet-lite', cpu: 'isnet-anime' } });
    expect(edgeModelFor('', s, 'cuda')).toBe('birefnet-lite');
    expect(edgeModelFor(undefined, s, 'cpu')).toBe('isnet-anime');
    expect(edgeModelFor(' none ', s, 'cuda')).toBe('none');
    expect(edgeModelFor('isnet-anime', s, 'cuda')).toBe('isnet-anime');
    expect(edgeModelFor('', null, '')).toBe('isnet-anime'); // the recipe default without a status
    expect(matteMemoKey('["a"]', 'birefnet-lite', 'cuda')).toBe('["a"]|birefnet-lite|cuda');
    expect(matteMemoKey('', 'birefnet-lite', 'cuda')).toBe('');
    expect(matteMemoKey('["a"]', '', 'cuda')).toBe('');
    expect(matteMemoKey('["a"]', 'x', '')).toBe('["a"]|x|');
  });

  it('maskPromptIdleText names the edge model and carries the server’s reason', () => {
    expect(maskPromptIdleText('General (precise)')).toBe('General matte not computed for this clip — switch the Model to General (precise), press Compute, then come back');
    expect(maskPromptIdleText('Anime (fast)', ' compute the General matte first ')).toBe(
      'Anime matte not computed for this clip — switch the Model to Anime (fast), press Compute, then come back (compute the General matte first)',
    );
    expect(maskPromptIdleText('')).toContain('edge matte not computed');
  });
});

