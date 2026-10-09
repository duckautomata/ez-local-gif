// The pure AI matte helpers (Phase 5b): device / duration words, the Model
// select's options, the clip estimate and the caps verdict mirroring jobs'
// up-front refusal, the Background card's status line, the preview's
// pending pill and the retry cadence.
import { describe, expect, it } from 'vitest';
import type { MatteStatus } from './api';
import {
  deviceLabel,
  humanSeconds,
  MATTE_DEFERRED_RETRY_MS,
  MATTE_FRAME_OVERHEAD_MS,
  MATTE_POLL_MS,
  MATTE_PROFILE_HINT,
  MATTE_RETRY_MS,
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
    expect(MATTE_DEFERRED_RETRY_MS).toBe(MATTE_POLL_MS);
    expect(MATTE_FRAME_OVERHEAD_MS).toBe(2);
    expect(MATTE_PROFILE_HINT).toBe('docker compose --profile matte-gpu up -d');
    expect(matteRetryMs('running')).toBe(500);
    expect(matteRetryMs('loading')).toBe(500);
    expect(matteRetryMs('downloading')).toBe(500);
    expect(matteRetryMs('deferred')).toBe(5000);
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
  });
});
