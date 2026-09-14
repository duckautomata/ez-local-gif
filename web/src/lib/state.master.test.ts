// The frame-master estimate the Render panel shows before Render
// (2026-09-13): planMaster mirrors jobs.masterBytes / scratchFactor /
// scratchReserve on the plan the SPA predicts (planCanvas × planFrames, the
// static one-frame cut of render.go), masterVerdict mirrors admitScratch's
// order (per-render cap, then the scratch budget) and says how much clip
// fits instead. Expectations are checked against internal/jobs where a table
// exists there (phase2_test.go's scratchFactor table).
import { describe, expect, it } from 'vitest';
import type { Output, ProbeInfo } from './api';
import { frameCount } from './format';
import { defaultOutput, presetById } from './presets';
import {
  DEFAULT_FPS,
  defaultOps,
  MAX_EXTRACT_FRAMES,
  masterVerdict,
  planCanvas,
  planFPS,
  planFrames,
  planMaster,
  SCRATCH_HEADROOM_MIN,
  scratchFactor,
  scratchReserve,
  type MasterEstimate,
} from './state.svelte';

/** the user's report: 2560×1440, 704 frames at 30 fps — a 9.7 GiB master untrimmed */
const clip4k: ProbeInfo = {
  format: 'mov,mp4,m4a,3gp,3g2,mj2',
  codec: 'h264',
  pixFmt: 'yuv420p',
  bits: 8,
  width: 2560,
  height: 1440,
  fps: 30,
  duration: 23.4667,
  frames: 704,
  hasAlpha: false,
  hasAudio: true,
  isStill: false,
  kind: 'video',
  premultiplied: false,
};
const still: ProbeInfo = { ...clip4k, width: 400, height: 300, fps: 0, duration: 0, frames: 1, isStill: true, kind: 'image', codec: 'png', format: 'png_pipe' };
const seq1: ProbeInfo = { ...still, fps: 10, duration: 0.1, kind: 'sequence', sequence: { count: 1, pattern: 'f%d.png', delayMs: 100, mixed: false } };
const noRate: ProbeInfo = { ...clip4k, fps: 0, duration: 0, frames: 0 };
/** a duration but no rate (ffprobe reported neither r_frame_rate nor avg_frame_rate): the graph plans at defaultFPS */
const noRateDur: ProbeInfo = { ...clip4k, fps: 0, frames: 0 };

const GIB = 2 ** 30;
const MIB = 2 ** 20;

/** the chat preset at the source size: 1 master, no fit */
function chat() {
  const out = defaultOutput();
  presetById('chat').apply(out);
  return out;
}

describe('scratchFactor / scratchReserve (jobs.scratchFactor, scratchReserve)', () => {
  it('matches the factor table of internal/jobs/phase2_test.go verbatim', () => {
    const table: [Output, number][] = [
      [{ format: 'gif' }, 1],
      [{ format: 'gif', fitBytes: 1 }, 2],
      [{ format: 'webp', fitBytes: 1 }, 2],
      [{ format: 'apng' }, 1],
      [{ format: 'apng', colors: 128 }, 2],
      [{ format: 'apng', fitBytes: 1 }, 3],
      [{ format: 'avif' }, 2],
      [{ format: 'avif', fitBytes: 1 }, 3],
      [{ format: 'frames' }, 2],
      [{ format: 'png' }, 1],
      [{ format: 'png', fitBytes: 1 }, 2], // jobs' fitFormats has png (presets.FIT_FORMATS does not)
    ];
    for (const [o, want] of table) expect(scratchFactor(o), JSON.stringify(o)).toBe(want);
  });

  it('gifski reads PNG frames of the whole master (2), a fit on top makes 3; video fits cost one more too; frames never fit', () => {
    expect(scratchFactor({ format: 'gif', encoder: 'gifski' })).toBe(2);
    expect(scratchFactor({ format: 'gif', encoder: 'gifski', fitBytes: 1 })).toBe(3);
    expect(scratchFactor({ format: 'mp4' })).toBe(1);
    expect(scratchFactor({ format: 'mp4', fitBytes: 1 })).toBe(2);
    expect(scratchFactor({ format: 'webm', fitBytes: 1 })).toBe(2);
    expect(scratchFactor({ format: 'jpeg', fitBytes: 1 })).toBe(2);
    expect(scratchFactor({ format: 'frames', fitBytes: 1 })).toBe(2); // frames is not a fit format
  });

  it('reserves the master plus max(master/8, 8 MiB) of headroom; 0 stays 0', () => {
    expect(scratchReserve(0)).toBe(0);
    expect(scratchReserve(-5)).toBe(0);
    expect(scratchReserve(1)).toBe(1 + SCRATCH_HEADROOM_MIN);
    expect(scratchReserve(64 * MIB)).toBe(72 * MIB); // the eighth equals the floor here
    expect(scratchReserve(64 * MIB - 8)).toBe(64 * MIB - 8 + 8 * MIB); // just under: the 8 MiB floor
    expect(scratchReserve(2 * GIB)).toBe(2 * GIB + 256 * MIB);
    expect(scratchReserve(1001)).toBe(1001 + 8 * MIB); // Go's need/8 is integer division — the floor wins anyway
    expect(scratchReserve(1 * GIB + 7)).toBe(1 * GIB + 7 + Math.floor((1 * GIB + 7) / 8));
  });
});

describe('planCanvas with ignoreAutocrop', () => {
  it('is null with auto-crop on (unchanged), the uncropped frame when the stage is ignored — the manual rectangle too', () => {
    const ops = defaultOps(clip4k);
    const out = chat();
    ops.autocrop.enabled = true;
    ops.crop = { enabled: true, x: 0, y: 0, w: 800, h: 600 }; // replaced by auto-crop in the stack (buildOps)
    expect(planCanvas(clip4k, ops, out)).toBeNull();
    expect(planCanvas(clip4k, ops, out, { ignoreAutocrop: true })).toEqual({ w: 2560, h: 1440 });
    // a resize after the (ignored) auto-crop still applies to the uncropped frame
    ops.resize = { enabled: true, width: 640, height: 0, fit: 'contain' };
    expect(planCanvas(clip4k, ops, out, { ignoreAutocrop: true })).toEqual({ w: 640, h: 360 });
    // with auto-crop off the option changes nothing: the manual rectangle applies as before
    ops.autocrop.enabled = false;
    ops.resize.enabled = false;
    expect(planCanvas(clip4k, ops, out, { ignoreAutocrop: true })).toEqual({ w: 800, h: 600 });
    expect(planCanvas(clip4k, ops, out)).toEqual({ w: 800, h: 600 });
  });
});

describe('planMaster (jobs.masterBytes on the predicted plan)', () => {
  it('the untrimmed 4K clip at the chat preset: 2560×1440 × 704 frames = 9.7 GiB, factor 1', () => {
    const est = planMaster(clip4k, defaultOps(clip4k), chat());
    expect(est).not.toBeNull();
    expect(est!.w).toBe(2560);
    expect(est!.h).toBe(1440);
    expect(est!.frames).toBe(704);
    expect(est!.bytes).toBe(2560 * 1440 * 4 * 704);
    expect(est!.bytes).toBe(10380902400);
    expect(est!.factor).toBe(1);
    expect(est!.reserve).toBe(scratchReserve(10380902400));
    expect(est!.fps).toBe(30);
    expect(est!.upperBound).toBe(false);
  });

  it('trim + crop + emote fit shrink it to what the emote render measures: 128×128 × 75 frames at 25 fps, factor 2 (fit)', () => {
    const ops = defaultOps(clip4k);
    ops.trim = { enabled: true, start: 0, end: 3 };
    ops.crop = { enabled: true, x: 0, y: 0, w: 800, h: 600 };
    const out = defaultOutput();
    presetById('emote').apply(out); // 128×128, 25 fps, fit ≤ 256 KiB
    const est = planMaster(clip4k, ops, out)!;
    expect(est.w).toBe(128);
    expect(est.h).toBe(128);
    expect(est.frames).toBe(75);
    expect(est.bytes).toBe(128 * 128 * 4 * 75);
    expect(est.factor).toBe(2); // gif + fit budget
    expect(est.reserve).toBe(scratchReserve(est.bytes) + est.bytes);
    expect(est.fps).toBe(25);
  });

  it('is null for no source, the Optimize preset (gifsicle only: no master) and an unknown frame count', () => {
    const out = chat();
    expect(planMaster(null, defaultOps(null), out)).toBeNull();
    expect(planMaster(undefined, defaultOps(null), out)).toBeNull();
    const opt = { ...defaultOutput(), preset: 'optimize' as const };
    presetById('optimize').apply(opt);
    expect(planMaster(clip4k, defaultOps(clip4k), opt)).toBeNull();
    // no rate, no duration: planFrames is 0 — jobs admits need 0 and only ENOSPC bounds it; no verdict here either
    expect(planFrames(noRate, defaultOps(noRate), out)).toBe(0);
    expect(planMaster(noRate, defaultOps(noRate), out)).toBeNull();
  });

  it('a clip with a duration but no rate plans at graph’s default 10 fps (compiler.fps → defaultFPS), so the estimate is not blank', () => {
    const out = chat();
    expect(DEFAULT_FPS).toBe(10);
    expect(planFPS(noRateDur, defaultOps(noRateDur), out)).toBe(10);
    expect(planFrames(noRateDur, defaultOps(noRateDur), out)).toBe(Math.floor(23.4667 * 10)); // assemble: floor(Duration × 10)
    expect(planMaster(noRateDur, defaultOps(noRateDur), out)).toMatchObject({ w: 2560, h: 1440, frames: 234, bytes: 2560 * 1440 * 4 * 234, fps: 10 });
    // an fps op / Output.fps still wins, and the gif snap applies to the default like any rate
    const ops = defaultOps(noRateDur);
    ops.fps = { enabled: true, fps: 24 };
    expect(planFPS(noRateDur, ops, out)).toBe(24);
    // a still (no length) keeps 0: its one frame needs no grid, and nothing changes for it
    expect(planFPS(still, defaultOps(still), out)).toBe(0);
    expect(planMaster(still, defaultOps(still), out)).toMatchObject({ frames: 1, fps: 0 });
  });

  it('a still source, a one-frame sequence and a static format are one frame (graph.singleFrame / render.go oneFramePlan)', () => {
    const out = chat();
    expect(planMaster(still, defaultOps(still), out)).toMatchObject({ w: 400, h: 300, frames: 1, bytes: 400 * 300 * 4 });
    expect(planMaster(seq1, defaultOps(seq1), out)).toMatchObject({ frames: 1, bytes: 400 * 300 * 4 });
    // png / jpeg of the whole 4K clip: the first frame only — 14 MiB, not 9.7 GiB
    for (const format of ['png', 'jpeg'] as const) {
      const o = { ...chat(), preset: 'custom' as const, format };
      const est = planMaster(clip4k, defaultOps(clip4k), o)!;
      expect(est.frames, format).toBe(1);
      expect(est.bytes, format).toBe(2560 * 1440 * 4);
      expect(est.factor, format).toBe(1);
    }
    // …but a bounce on a still stays at 1 (the graph refuses bouncing those) and on the clip doubles
    const ops = defaultOps(clip4k);
    ops.bounce = true;
    expect(planMaster(clip4k, ops, out)!.frames).toBe(1408);
    expect(planMaster(clip4k, ops, out)!.bytes).toBe(2560 * 1440 * 4 * 1408);
    const sops = defaultOps(still);
    sops.bounce = true;
    expect(planMaster(still, sops, out)!.frames).toBe(1);
  });

  it('a reversed png / jpeg keeps one frame of master but carries the whole clip as its reverse buffer (render.go admitReversed on the pre-cut plan)', () => {
    const frame = 2560 * 1440 * 4;
    for (const format of ['png', 'jpeg'] as const) {
      const o = { ...chat(), preset: 'custom' as const, format };
      // forward: no buffer at all
      expect(planMaster(clip4k, defaultOps(clip4k), o)).toMatchObject({ frames: 1, bufferFrames: 0, bufferBytes: 0 });
      // reverse: the untrimmed 704 frames = 9.7 GiB of RAM for one frame of master
      const ops = defaultOps(clip4k);
      ops.reverse = true;
      expect(planMaster(clip4k, ops, o)).toMatchObject({ frames: 1, bytes: frame, bufferFrames: 704, bufferBytes: frame * 704, fps: 30 });
      expect(planMaster(clip4k, ops, o)!.bufferBytes).toBe(10380902400);
      // trimmed to 3 s: 90 frames, 1.2 GiB
      ops.trim = { enabled: true, start: 0, end: 3 };
      expect(planMaster(clip4k, ops, o)).toMatchObject({ frames: 1, bufferFrames: 90, bufferBytes: frame * 90 });
      // a bounce on top of the reverse doubles the count like the server's Plan.Frames (its conservative bound)
      ops.bounce = true;
      expect(planMaster(clip4k, ops, o)).toMatchObject({ frames: 1, bufferFrames: 180, bufferBytes: frame * 180 });
      // a bounce alone buffers nothing before its first frame: the server does not check it, nor do we
      ops.reverse = false;
      ops.trim.enabled = false;
      expect(planMaster(clip4k, ops, o)).toMatchObject({ frames: 1, bufferFrames: 0, bufferBytes: 0 });
    }
    // an animated reversed render has no separate figure: its buffer is at most the master, which bytes measures
    const ops = defaultOps(clip4k);
    ops.reverse = true;
    expect(planMaster(clip4k, ops, chat())).toMatchObject({ frames: 704, bufferFrames: 0, bufferBytes: 0 });
    // a reversed still / one-frame sequence has nothing to reverse (the graph refuses it): no buffer
    const sops = defaultOps(still);
    sops.reverse = true;
    expect(planMaster(still, sops, { ...chat(), preset: 'custom' as const, format: 'png' as const })).toMatchObject({ frames: 1, bufferFrames: 0 });
    // an unknown count leaves the buffer 0 with the one-frame master still shown (admitReversed passes need 0 the same way)
    const nops = defaultOps(noRate);
    nops.reverse = true;
    expect(planMaster(noRate, nops, { ...chat(), preset: 'custom' as const, format: 'png' as const })).toMatchObject({ frames: 1, bufferFrames: 0, bufferBytes: 0 });
  });

  it('does not round the canvas to even for mp4 / webm: odd 127×127 stays 127×127 (the even-pad lives in the encoder tail)', () => {
    const ops = defaultOps(clip4k);
    ops.trim = { enabled: true, start: 0, end: 1 };
    ops.crop = { enabled: true, x: 0, y: 0, w: 127, h: 127 };
    for (const format of ['mp4', 'webm'] as const) {
      const o = { ...chat(), format };
      const est = planMaster(clip4k, ops, o)!;
      expect(est.w, format).toBe(127);
      expect(est.h, format).toBe(127);
      expect(est.bytes, format).toBe(127 * 127 * 4 * 30);
      expect(est.factor, format).toBe(1);
    }
  });

  it('with auto-crop on and no output size the uncropped canvas stands in, flagged as an upper bound; both output dims pin it', () => {
    const ops = defaultOps(clip4k);
    ops.autocrop.enabled = true;
    const out = chat();
    const est = planMaster(clip4k, ops, out)!;
    expect(est.upperBound).toBe(true);
    expect(est.w).toBe(2560);
    expect(est.h).toBe(1440);
    expect(est.frames).toBe(704);
    // one output dimension: still the pre-crop figure (the box's aspect is unknown)
    out.width = 640;
    expect(planMaster(clip4k, ops, out)).toMatchObject({ w: 640, h: 360, upperBound: true });
    // both: the canvas is exact, no upper bound
    const emote = defaultOutput();
    presetById('emote').apply(emote);
    expect(planMaster(clip4k, ops, emote)).toMatchObject({ w: 128, h: 128, upperBound: false });
  });

  it('the factor follows the wire output: frames 2, avif + fit 3, apng indexed 2, gifski 2, sticker apng + fit 3', () => {
    const ops = defaultOps(clip4k);
    ops.trim = { enabled: true, start: 0, end: 1 };
    const frames = defaultOutput();
    presetById('frames').apply(frames);
    expect(planMaster(clip4k, ops, frames)!.factor).toBe(2);
    const sticker = defaultOutput();
    presetById('sticker').apply(sticker); // indexed apng, fit on
    expect(planMaster(clip4k, ops, sticker)!.factor).toBe(3);
    const custom = { ...chat(), preset: 'custom' as const, format: 'avif' as const, fitEnabled: true, fitKiB: 512 };
    expect(planMaster(clip4k, ops, custom)!.factor).toBe(3);
    const gifski = { ...chat(), encoder: 'gifski' as const, target: '' as const };
    expect(planMaster(clip4k, ops, gifski)!.factor).toBe(2);
    // the frames export cap is jobs.MaxExtractFrames
    expect(MAX_EXTRACT_FRAMES).toBe(2000);
    expect(planMaster(clip4k, defaultOps(clip4k), frames)!.frames).toBe(704);
  });
});

describe('masterVerdict (jobs.admitScratch, in its order)', () => {
  const untrimmed: MasterEstimate = { w: 2560, h: 1440, frames: 704, bytes: 10380902400, factor: 1, reserve: scratchReserve(10380902400), bufferFrames: 0, bufferBytes: 0, fps: 30, upperBound: false };

  it('the cap binds: 9.7 GiB over a 2 GiB cap, about 145 frames (4.8 s at 30 fps) fit at that size', () => {
    const v = masterVerdict(untrimmed, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 });
    expect(v.over).toBe(true);
    expect(v.by).toBe('cap');
    expect(v.limit).toBe(2 * GIB);
    expect(v.maxFrames).toBe(Math.floor((2 * GIB) / (2560 * 1440 * 4)));
    expect(v.maxFrames).toBe(145);
    expect(v.maxSeconds).toBe(4.8); // 4.833 floored to the tenth
  });

  it('floors maxSeconds to the tenth so a trim to it never plans one frame over the cap', () => {
    // 640×360 under a 2 GiB cap: 2330 frames fit; 2330 / 30 = 77.667 s — a nearest-rounded "77.7 s" plans 2331
    const frame = 640 * 360 * 4;
    const est: MasterEstimate = { w: 640, h: 360, frames: 7040, bytes: frame * 7040, factor: 1, reserve: scratchReserve(frame * 7040), bufferFrames: 0, bufferBytes: 0, fps: 30, upperBound: false };
    const v = masterVerdict(est, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 });
    expect(v.maxFrames).toBe(2330);
    expect(v.maxSeconds).toBe(77.6);
    expect(frameCount(77.7, 30)).toBe(2331); // what following the rounded figure would have planned
    expect(frameCount(v.maxSeconds, 30)).toBeLessThanOrEqual(v.maxFrames);
    // an on-grid value is not pushed under by float error: 29 frames at 10 fps are 2.9 s, not 2.8
    const ten: MasterEstimate = { ...est, fps: 10 };
    expect(masterVerdict({ ...ten, w: 128, h: 128, bytes: 128 * 128 * 4 * 5000, reserve: 0 }, { maxMasterBytes: 128 * 128 * 4 * 29, scratchBudgetBytes: 0 }).maxSeconds).toBe(2.9);
    // and the tenth itself is exact: 145 frames at 25 fps are 5.8 s
    expect(masterVerdict({ ...untrimmed, fps: 25 }, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 }).maxSeconds).toBe(5.8);
  });

  it('the reverse buffer of a static render binds first and against the cap alone (render.go admitReversed before the one-frame cut)', () => {
    const frame = 2560 * 1440 * 4;
    const reversedPng: MasterEstimate = { w: 2560, h: 1440, frames: 1, bytes: frame, factor: 1, reserve: scratchReserve(frame), bufferFrames: 704, bufferBytes: frame * 704, fps: 30, upperBound: false };
    const v = masterVerdict(reversedPng, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 });
    expect(v.over).toBe(true);
    expect(v.by).toBe('buffer');
    expect(v.limit).toBe(2 * GIB);
    expect(v.maxFrames).toBe(145); // the clip length whose reverse fits at that size
    expect(v.maxSeconds).toBe(4.8);
    // trimmed to 3 s: 90 frames = 1.2 GiB of buffer fit
    expect(masterVerdict({ ...reversedPng, bufferFrames: 90, bufferBytes: frame * 90 }, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 }).over).toBe(false);
    // the buffer is RAM, not scratch: a tiny scratch budget does not judge it (the one-frame master's reservation is what scratch sees)
    expect(masterVerdict({ ...reversedPng, bufferFrames: 90, bufferBytes: frame * 90 }, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 64 * MIB }).over).toBe(false);
    // an unknown cap never binds it
    expect(masterVerdict(reversedPng, { maxMasterBytes: 0, scratchBudgetBytes: 4 * GIB }).over).toBe(false);
    // judged before the master (the server's order): with both over, the buffer names the refusal
    expect(masterVerdict({ ...untrimmed, bufferFrames: 704, bufferBytes: untrimmed.bytes }, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 }).by).toBe('buffer');
  });

  it('unknown caps (0: no answer yet, an older server) never say over — the server has the last word', () => {
    const v = masterVerdict(untrimmed, { maxMasterBytes: 0, scratchBudgetBytes: 0 });
    expect(v.over).toBe(false);
    expect(v.by).toBe('');
    expect(v.limit).toBe(0);
    expect(v.maxFrames).toBe(0);
    expect(v.maxSeconds).toBe(0);
  });

  it('under both bounds: not over, limit is the tighter known bound', () => {
    const small: MasterEstimate = { ...untrimmed, frames: 10, bytes: 2560 * 1440 * 4 * 10, reserve: scratchReserve(2560 * 1440 * 4 * 10) };
    const v = masterVerdict(small, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 4 * GIB });
    expect(v.over).toBe(false);
    expect(v.limit).toBe(2 * GIB); // the cap is tighter than 4 GiB / 1.125
    expect(v.maxFrames).toBe(145);
    const w = masterVerdict(small, { maxMasterBytes: 8 * GIB, scratchBudgetBytes: 4 * GIB });
    expect(w.over).toBe(false);
    expect(w.limit).toBe(Math.floor((4 * GIB) / 1.125)); // the scratch bound is tighter now
    expect(masterVerdict(small, { maxMasterBytes: 0, scratchBudgetBytes: 4 * GIB }).limit).toBe(Math.floor((4 * GIB) / 1.125));
  });

  it('the scratch budget binds below the cap for a fit render: factor 2 on a 3.2 GiB budget vs a 2 GiB cap', () => {
    // a 1.6 GiB master fits the 2 GiB cap, but reserves 1.6 + 0.2 + 1.6 = 3.4 GiB of scratch
    const bytes = 1640 * MIB;
    const fit: MasterEstimate = { w: 1920, h: 1080, frames: Math.round(bytes / (1920 * 1080 * 4)), bytes, factor: 2, reserve: scratchReserve(bytes) + bytes, bufferFrames: 0, bufferBytes: 0, fps: 25, upperBound: false };
    const budget = 3277 * MIB;
    const v = masterVerdict(fit, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: budget });
    expect(v.over).toBe(true);
    expect(v.by).toBe('scratch');
    // the largest master whose reservation fits: budget / (factor + 1/8), and its reservation really fits
    expect(v.limit).toBe(Math.floor(budget / 2.125));
    expect(scratchReserve(v.limit) + v.limit).toBeLessThanOrEqual(budget);
    expect(scratchReserve(v.limit + 8) + v.limit + 8).toBeGreaterThan(budget);
    expect(v.maxFrames).toBe(Math.floor(v.limit / (1920 * 1080 * 4)));
    expect(v.maxSeconds).toBe(Math.floor((v.maxFrames * 10) / 25) / 10);
    expect(v.maxSeconds).toBeLessThanOrEqual(v.maxFrames / 25);
    // the same render with no scratch budget published is not over (the cap alone judges it)
    expect(masterVerdict(fit, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 0 }).over).toBe(false);
    // and factor 1 at the same budget is fine: 1.6 + 0.2 GiB fits
    expect(masterVerdict({ ...fit, factor: 1, reserve: scratchReserve(bytes) }, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: budget }).over).toBe(false);
  });

  it('the cap is judged first when both are over (admitScratch checks the cap before reserving)', () => {
    const v = masterVerdict({ ...untrimmed, factor: 3, reserve: scratchReserve(untrimmed.bytes) + 2 * untrimmed.bytes }, { maxMasterBytes: 2 * GIB, scratchBudgetBytes: 1 * GIB });
    expect(v.by).toBe('cap');
    expect(v.limit).toBe(2 * GIB);
  });

  it('inverts the small-master branch of the reservation (below 64 MiB the 8 MiB floor applies)', () => {
    const tiny: MasterEstimate = { w: 128, h: 128, frames: 2000, bytes: 128 * 128 * 4 * 2000, factor: 1, reserve: scratchReserve(128 * 128 * 4 * 2000), bufferFrames: 0, bufferBytes: 0, fps: 25, upperBound: false };
    // 125 MiB master + 15.6 MiB headroom = 140.6 MiB > a 100 MiB budget
    const budget = 100 * MIB;
    const v = masterVerdict(tiny, { maxMasterBytes: 0, scratchBudgetBytes: budget });
    expect(v.over).toBe(true);
    expect(v.by).toBe('scratch');
    // 100 MiB / 1.125 = 88.9 MiB ≥ 64 MiB: the large-master branch
    expect(v.limit).toBe(Math.floor(budget / 1.125));
    // a 40 MiB budget: 40 / 1.125 = 35.6 MiB < 64 MiB, so the floor branch: (40 − 8) MiB
    const w = masterVerdict(tiny, { maxMasterBytes: 0, scratchBudgetBytes: 40 * MIB });
    expect(w.limit).toBe(32 * MIB);
    expect(scratchReserve(32 * MIB)).toBe(40 * MIB);
    expect(w.maxFrames).toBe(Math.floor((32 * MIB) / (128 * 128 * 4)));
    // a budget under the 8 MiB floor fits nothing
    expect(masterVerdict(tiny, { maxMasterBytes: 0, scratchBudgetBytes: 4 * MIB }).limit).toBe(0);
  });
});
