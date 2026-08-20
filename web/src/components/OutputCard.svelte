<script lang="ts">
  import { isAnimatedFormat, type Dither, type FitMode } from '../lib/api';
  import { dropRates, fitSize, fmtKiB, fmtNum, gifDelays, MAX_FPS, normalizeHex } from '../lib/format';
  import {
    chatPresetFor,
    DEFAULT_ALPHA_THRESHOLD,
    DEFAULT_MATTE,
    FIT_KIB,
    fitsFormat,
    FORMAT_LABEL,
    formatsFor,
    FRAME_FORMATS,
    LIMITS,
    presetAvailable,
    PRESETS,
    presetById,
    TARGET_LABEL,
    TARGETS,
    TRIM_FRINGE_THRESHOLD,
    WHITE_MATTE,
  } from '../lib/presets';
  import { app, applyPreset, effectiveFPS, effectiveOps, loopFor, opsApply, previewDuration, sourceFPS, usesMatte } from '../lib/state.svelte';
  import NumField from './NumField.svelte';

  const out = $derived(app.output);
  const preset = $derived(presetById(out.preset));
  const locked = $derived(preset.locksSize);
  const limit = $derived(LIMITS[out.target]);
  const info = $derived(app.source?.info ?? null);
  const ops = $derived(effectiveOps(app.ops, out));
  const withOps = $derived(opsApply(out));
  const animated = $derived(isAnimatedFormat(out.format));
  const isFrames = $derived(out.format === 'frames');
  // The fit engine only searches fit-capable formats (jobs.fitFormats): no
  // fit row for static PNG / frames — the server would ignore fitBytes.
  const fitCapable = $derived(fitsFormat(out.format));
  // Under fit the APNG rungs dictate the palette (indexed 256 → 128 → 64,
  // internal/fit stickerAPNGSteps) and RGBA truecolour is never tried, so the
  // colour select is a fit-off knob.
  const apngFitLocked = $derived(out.format === 'apng' && out.fitEnabled);
  const formats = $derived(formatsFor(preset, out.format));
  const formatLocked = $derived(formats.length <= 1);

  // Frame size entering the output stage (after crop / resize) — for the hint.
  const stackSize = $derived.by(() => {
    if (!info) return null;
    let w = info.width;
    let h = info.height;
    if (ops.crop.enabled) {
      w = ops.crop.w;
      h = ops.crop.h;
    }
    if (ops.resize.enabled) ({ w, h } = fitSize(w, h, ops.resize.width, ops.resize.height, ops.resize.fit));
    if (ops.flipRotate.enabled && (ops.flipRotate.degrees === 90 || ops.flipRotate.degrees === 270)) [w, h] = [h, w];
    return { w, h };
  });
  const finalSize = $derived.by(() => {
    if (!stackSize) return null;
    if (out.width > 0 && out.height > 0) return { w: out.width, h: out.height }; // canvas is padded/cropped to W×H
    return fitSize(stackSize.w, stackSize.h, out.width, out.height, out.fit);
  });

  // graph.Compile precedence: fps op → Output.fps → source fps, then snapped
  // for the format (GIF capped at 50, else 60).
  const srcFps = $derived(sourceFPS(info, ops));
  const opFpsOn = $derived(withOps && ops.fps.enabled && ops.fps.fps > 0);
  const effFps = $derived(effectiveFPS(ops, out, srcFps));
  const outFpsIgnored = $derived(opFpsOn && out.fps > 0);
  // Optimize drops every Nth frame (N = 2..4): the reachable rates are src × (N−1)/N (see dropRates).
  const dropChoices = $derived(out.preset === 'optimize' ? dropRates(srcFps) : []);

  // Loop count (GIF NETSCAPE semantics: 0 = forever, N = play N+1 times) is
  // editable only with no Discord target; every Discord target requires
  // loop forever and always gets 0 (see loopFor / discordlint).
  const loopEditable = $derived(out.target === '');
  const loop = $derived(loopFor(out));
  const loopHint = $derived(loop === 0 ? 'forever' : `plays ${loop + 1} times (loop count ${loop})`);
  const targetEditable = $derived(out.preset === 'custom' || out.preset === 'optimize');

  const fits: FitMode[] = ['contain', 'cover', 'exact'];
  const dithers: { id: Dither; label: string }[] = [
    { id: 'bayer', label: 'bayer (ordered, smallest, no shimmer)' },
    { id: 'sierra2_4a', label: 'sierra2_4a (photographic)' },
    { id: 'floyd_steinberg', label: 'floyd_steinberg' },
    { id: 'none', label: 'none' },
  ];
  const gifColors = [256, 128, 64, 32];
  const apngColors: { v: number; label: string }[] = [
    { v: 256, label: '256 (indexed 8-bit alpha — sticker default)' },
    { v: 128, label: '128 (indexed)' },
    { v: 64, label: '64 (indexed)' },
    { v: 32, label: '32 (indexed)' },
    { v: 0, label: 'RGBA truecolour (largest; fit off only)' },
  ];
  const pngColors: { v: number; label: string }[] = [
    { v: 256, label: '256 (pngquant, near-lossless)' },
    { v: 128, label: '128 (pngquant)' },
    { v: 64, label: '64 (pngquant)' },
    { v: 32, label: '32 (pngquant)' },
    { v: 0, label: 'full colour (oxipng only, largest)' },
  ];

  const matteMode = $derived(out.matte === DEFAULT_MATTE ? 'dark' : out.matte === WHITE_MATTE ? 'light' : 'custom');
  let matteText = $state('#' + DEFAULT_MATTE);
  let hexInput = $state<HTMLInputElement | null>(null);
  $effect(() => {
    matteText = '#' + out.matte;
  });
  function setMatte(hex: string) {
    const n = normalizeHex(hex);
    if (n) app.output.matte = n;
  }
  function onMatteText(e: Event) {
    const v = (e.currentTarget as HTMLInputElement).value;
    matteText = v;
    setMatte(v);
  }
  const trimFringe = $derived(out.alphaThreshold === TRIM_FRINGE_THRESHOLD);
  function setTrimFringe(on: boolean) {
    app.output.alphaThreshold = on ? TRIM_FRINGE_THRESHOLD : DEFAULT_ALPHA_THRESHOLD;
  }

  // The chat presets differ only by format: keep the chip in sync when the
  // user flips the format, without resetting the other fields.
  function onFormatChange() {
    if (out.preset === 'chat-gif' || out.preset === 'chat-webp' || out.preset === 'chat-avif') {
      const p = chatPresetFor(out.format);
      if (p) app.output.preset = p;
    }
    normalizeColors();
  }
  /** swapFormat flips between the preset's default format and its one-click alternative. */
  function swapFormat() {
    const swap = preset.swap;
    if (!swap) return;
    app.output.format = out.format === swap.format ? preset.formats[0] : swap.format;
    normalizeColors();
  }
  /** GIF always has a palette: a "0 = truecolour" left over from APNG/PNG becomes 256. */
  function normalizeColors() {
    if (out.format === 'gif' && !(out.colors > 0)) app.output.colors = 256;
  }
  function setFit(kib: number) {
    app.output.fitKiB = kib;
    app.output.fitEnabled = true;
  }
  function onFitToggle(e: Event) {
    const on = (e.currentTarget as HTMLInputElement).checked;
    app.output.fitEnabled = on;
    if (on && !(out.fitKiB > 0)) app.output.fitKiB = FIT_KIB[out.target];
  }

  const webpTooWide = $derived(out.format === 'webp' && (finalSize?.w ?? 0) > 480);
  const stickerWebp = $derived(out.target === 'sticker' && (out.format === 'webp' || out.format === 'avif'));
  const stickerTooLong = $derived(out.target === 'sticker' && animated && !!info && previewDuration(info, ops) > 5.0);
  const apngOffTarget = $derived(out.format === 'apng' && out.target !== 'sticker');
  const emoteApng = $derived(out.format === 'apng' && out.target === 'emote');
  const fitLimitKiB = $derived(limit > 0 ? Math.floor(limit / 1024) : 0);
  const fitOverLimit = $derived(out.fitEnabled && fitLimitKiB > 0 && out.fitKiB * 1024 > limit);
</script>

<section class="card output">
  <h2>Output</h2>

  <div class="chips presets" role="group" aria-label="Preset">
    {#each PRESETS as p (p.id)}
      {@const ok = presetAvailable(p, info)}
      <button
        type="button"
        class="chip"
        aria-pressed={out.preset === p.id}
        disabled={!ok}
        title={ok ? p.hint : (p.unavailableHint ?? p.hint)}
        onclick={() => applyPreset(p.id)}
      >
        {p.label}
      </button>
    {/each}
  </div>
  <p class="hint">{preset.hint}</p>
  {#if preset.swap}
    {@const swap = preset.swap}
    {@const swapped = out.format === swap.format}
    <div class="row swap">
      <button type="button" class="sm" onclick={swapFormat} title={swap.hint}>
        {swapped ? `${FORMAT_LABEL[preset.formats[0]].split(' ')[0]} instead` : swap.label}
      </button>
      <span class="hint">{swapped ? `${FORMAT_LABEL[out.format].split(' ')[0]} selected — ${swap.hint}` : swap.hint}</span>
    </div>
  {/if}
  {#if preset.warn}<p class="note">{preset.warn}</p>{/if}

  <div class="row">
    <label class="field">
      <span>Format{formatLocked ? ' (preset)' : ''}</span>
      <select bind:value={app.output.format} onchange={onFormatChange} disabled={formatLocked}>
        {#each formats as f (f)}<option value={f}>{FORMAT_LABEL[f]}</option>{/each}
      </select>
    </label>
    {#if isFrames}
      <label class="field">
        <span>Frame format</span>
        <select bind:value={app.output.frameFormat}>
          {#each FRAME_FORMATS as f (f.id)}<option value={f.id}>{f.label}</option>{/each}
        </select>
      </label>
    {:else}
      <label class="field">
        <span>Discord target</span>
        {#if targetEditable}
          <select bind:value={app.output.target}>
            {#each TARGETS as t (t)}<option value={t}>{TARGET_LABEL[t]}</option>{/each}
          </select>
        {:else}
          <span class="static">{TARGET_LABEL[out.target]}</span>
        {/if}
      </label>
      <span class="field">
        <span>Byte limit</span>
        <span class="static">
          {#if limit > 0}<b>{fmtKiB(limit)} KiB</b> <span class="muted">({limit.toLocaleString('en-US')} B)</span>{:else}none{/if}
        </span>
      </span>
    {/if}
  </div>

  <div class="row">
    {#if out.preset !== 'optimize'}
      <label class="field">
        <span>Width {locked ? '(preset)' : '(0 = auto)'}</span>
        <NumField bind:value={app.output.width} min={0} max={8192} disabled={locked} small />
      </label>
      <label class="field">
        <span>Height {locked ? '(preset)' : '(0 = auto)'}</span>
        <NumField bind:value={app.output.height} min={0} max={8192} disabled={locked} small />
      </label>
      <label class="field">
        <span>Fit</span>
        <select bind:value={app.output.fit} disabled={locked}>
          {#each fits as f (f)}<option value={f}>{f}</option>{/each}
        </select>
      </label>
      <label class="field">
        <span>fps {outFpsIgnored ? '(Frame rate op wins)' : opFpsOn ? '(set by the Frame rate op)' : '(0 = source)'}</span>
        <NumField
          bind:value={app.output.fps}
          min={0}
          max={MAX_FPS}
          step="any"
          small
          title={opFpsOn ? 'The Frame rate op has precedence over this value' : '0 = source fps; GIF is capped at 50, the other formats at 60'}
        />
      </label>
    {:else}
      <span class="field">
        <span>Frames (drop every Nth frame, its delay merged into the previous one — no re-encode)</span>
        <span class="chips">
          {#if dropChoices.length === 0}
            <span class="hint">source frame rate unknown — all frames kept</span>
          {:else}
            {#each dropChoices as r (r.n)}
              <button
                type="button"
                class="chip"
                aria-pressed={r.n === 0 ? !(out.fps > 0) : Math.abs(out.fps - r.fps) < 0.002}
                onclick={() => (app.output.fps = r.fps)}
              >
                {r.label}
              </button>
            {/each}
          {/if}
        </span>
      </span>
    {/if}
    {#if finalSize}
      <span class="hint">
        → <b>{finalSize.w}×{finalSize.h}</b>{#if effFps > 0}&nbsp;· <b>{fmtNum(effFps)} fps</b>{#if out.format === 'gif'}
            <span class="muted">&nbsp;({gifDelays(effFps)} cs delays)</span>{/if}{#if outFpsIgnored}
            <span class="muted">&nbsp;· the Frame rate op overrides the fps above</span>{/if}{/if}
        {#if out.fit === 'contain' && out.width > 0 && out.height > 0}<span class="muted"> · transparent padding</span>{/if}
      </span>
    {/if}
  </div>

  {#if fitCapable}
    <div class="row fit">
      <label class="inline" title="Parallel ladder + secant search over the format's quality knob (DESIGN §5.4); the primary file ends up ≤ the budget, runner-ups are shown as alternatives">
        <input type="checkbox" checked={out.fitEnabled} onchange={onFitToggle} />
        <span>Fit to ≤</span>
      </label>
      <NumField bind:value={app.output.fitKiB} min={1} max={1_000_000} disabled={!out.fitEnabled} small />
      <span class="small">KiB</span>
      {#if fitLimitKiB > 0}
        <button type="button" class="sm ghost" onclick={() => setFit(fitLimitKiB)} title="Use the Discord limit as the budget">= limit ({fmtKiB(limit)})</button>
      {/if}
      {#if out.preset !== 'optimize'}
        <!-- the gifsicle-only Optimize ladder never scales, so "keep size" would be a no-op there -->
        <label class="inline" title="Never downscale to fit (FitKeepSize)">
          <input type="checkbox" bind:checked={app.output.fitKeepSize} disabled={!out.fitEnabled} /><span>keep size</span>
        </label>
      {/if}
      <label class="inline" title="Never lower the frame rate to fit (FitKeepFPS)">
        <input type="checkbox" bind:checked={app.output.fitKeepFps} disabled={!out.fitEnabled} /><span>keep fps</span>
      </label>
      <span class="hint">
        {#if !out.fitEnabled}
          off — the knobs below are used as-is.
        {:else if out.preset === 'optimize'}
          cheapest first: lossy → frame drop → colours (gifsicle only — never scaled); the 2 runner-ups are listed as
          alternatives.
        {:else}
          cheapest first: lossy → fps → colours → downscale{out.target === 'sticker' ? ' (stickers are never downscaled)' : ''}; the
          2 runner-ups are listed as alternatives.
        {/if}
      </span>
    </div>
    {#if fitOverLimit}<p class="note">The fit budget is above the {fmtKiB(limit)} KiB {TARGET_LABEL[out.target]} limit — the file may still be rejected.</p>{/if}
  {/if}

  {#if animated}
    <div class="row">
      <label class="field">
        <span>Loop count{loopEditable ? ' (0 = forever, N = play N+1 times)' : ''}</span>
        {#if loopEditable}
          <span class="row loop">
            <NumField bind:value={app.output.loop} min={0} max={65535} small title="GIF NETSCAPE loop count: 0 = forever, N = play N+1 times" />
            <span class="hint">{loopHint}</span>
          </span>
        {:else}
          <input type="text" class="loop-fixed" value="forever (Discord requires it)" disabled readonly />
        {/if}
      </label>
    </div>
  {/if}

  <hr class="sep" />

  {#if out.format === 'gif'}
    <div class="row">
      <label class="field">
        <span>Colours</span>
        <select bind:value={app.output.colors}>
          {#each gifColors as c (c)}<option value={c}>{c}</option>{/each}
        </select>
      </label>
      <label class="field">
        <span>Dither</span>
        <select bind:value={app.output.dither}>
          {#each dithers as d (d.id)}<option value={d.id}>{d.label}</option>{/each}
        </select>
      </label>
    </div>
    <div class="row">
      <label class="field slider">
        <span>Lossy (gifsicle 0–200) — <b>{out.lossy}</b>{out.lossy === 0 ? ' off' : out.lossy <= 80 ? ' near-invisible' : ' visible'}</span>
        <span class="row">
          <input type="range" min="0" max="200" step="1" bind:value={app.output.lossy} aria-label="Lossy" />
          <NumField bind:value={app.output.lossy} min={0} max={200} small />
        </span>
      </label>
    </div>
    {#if withOps}
      <div class="row">
        <label class="field slider">
          <span>Alpha threshold (1–255) — <b>{out.alphaThreshold}</b> (pixels below become transparent)</span>
          <span class="row">
            <input type="range" min="1" max="255" step="1" bind:value={app.output.alphaThreshold} aria-label="Alpha threshold" />
            <NumField bind:value={app.output.alphaThreshold} min={1} max={255} small />
          </span>
        </label>
        <label class="inline" title="Threshold {TRIM_FRINGE_THRESHOLD}: drops more of the semi-transparent edge, so less of the matte colour shows on the other theme">
          <input type="checkbox" checked={trimFringe} onchange={(e) => setTrimFringe(e.currentTarget.checked)} />
          <span>Trim fringe</span>
        </label>
      </div>
    {/if}
  {:else if out.format === 'apng'}
    <div class="row">
      <label class="field">
        <span>Colours{apngFitLocked ? ' (fit ladder: 256 → 128 → 64 indexed)' : ''}</span>
        <select bind:value={app.output.colors} disabled={apngFitLocked}>
          {#each apngColors as c (c.v)}<option value={c.v}>{c.label}</option>{/each}
        </select>
      </label>
    </div>
    {#if apngFitLocked}
      <p class="hint">Fit always searches the indexed pngquant ladder; turn fit off to pick a fixed palette or RGBA truecolour.</p>
    {/if}
    <p class="hint">
      Indexed APNG = one shared palette with 8-bit alpha (pngquant) — soft edges at a fraction of the RGBA size; verified at 25 fps
      as a sticker. Plays forever, delays ≥ 20 ms, ≤ 5 s / 1000 frames / 60 fps.
    </p>
  {:else if out.format === 'webp'}
    <div class="row">
      <label class="field slider">
        <span>Quality (1–100) — <b>{out.quality}</b></span>
        <span class="row">
          <input type="range" min="1" max="100" step="1" bind:value={app.output.quality} disabled={out.lossless} aria-label="Quality" />
          <NumField bind:value={app.output.quality} min={1} max={100} disabled={out.lossless} small />
        </span>
      </label>
      <label class="inline"><input type="checkbox" bind:checked={app.output.lossless} /><span>Lossless</span></label>
    </div>
    <p class="hint">
      Alpha stays 8-bit and lossless in this path (soft edges survive on Discord); libwebp_anim, no metadata, plays {loop === 0 ? 'forever' : `${loop + 1} times`}.
      q 80 blurs fine texture — raise it or go lossless for detailed art.
    </p>
  {:else if out.format === 'avif'}
    <div class="row">
      <label class="field slider">
        <span>Quality (1–100) — <b>{out.quality}</b></span>
        <span class="row">
          <input type="range" min="1" max="100" step="1" bind:value={app.output.quality} aria-label="Quality" />
          <NumField bind:value={app.output.quality} min={1} max={100} small />
        </span>
      </label>
    </div>
    <p class="hint">
      avifenc, alpha quality 90, 4:2:0 — soft alpha verified on Discord attachments (Discord transcodes it to WebP; large files
      take a few seconds to appear). Animated AVIF is also accepted as an emote.
    </p>
  {:else if out.format === 'png'}
    <div class="row">
      <label class="field">
        <span>Colours</span>
        <select bind:value={app.output.colors}>
          {#each pngColors as c (c.v)}<option value={c.v}>{c.label}</option>{/each}
        </select>
      </label>
    </div>
    <p class="hint">
      First frame as RGBA PNG: pngquant to the palette size (8-bit alpha kept) when set, then oxipng. Fits any Discord budget
      trivially — no fit search for a static PNG, pngquant palette + oxipng only.
    </p>
  {:else if out.format === 'jpeg'}
    <div class="row">
      <label class="field slider">
        <span>Quality (1–100) — <b>{out.quality}</b></span>
        <span class="row">
          <input type="range" min="1" max="100" step="1" bind:value={app.output.quality} aria-label="Quality" />
          <NumField bind:value={app.output.quality} min={1} max={100} small />
        </span>
      </label>
    </div>
    <p class="hint">First frame, no alpha: transparent areas are flattened onto the matte colour below.</p>
  {:else if isFrames}
    {#if out.frameFormat === 'jpeg'}
      <div class="row">
        <label class="field slider">
          <span>JPEG quality (1–100) — <b>{out.quality}</b></span>
          <span class="row">
            <input type="range" min="1" max="100" step="1" bind:value={app.output.quality} aria-label="Quality" />
            <NumField bind:value={app.output.quality} min={1} max={100} small />
          </span>
        </label>
      </div>
    {/if}
    <p class="hint">
      One file per frame after the op stack (trim / fps / crop / resize) plus frames.zip (stored, not compressed) and a
      delays.json with the per-frame timing. The result shows a thumbnail grid with per-frame downloads.
    </p>
  {/if}

  {#if usesMatte(out) && withOps}
    <div class="row">
      <span class="field wrap">
        <span>
          {#if out.format === 'gif'}
            Matte — blended under semi-transparent edges before the 1-bit cut
          {:else}
            Matte — background the image is flattened onto
          {/if}
        </span>
        <span class="row">
          <span class="seg" role="group" aria-label="Matte">
            <button type="button" aria-pressed={matteMode === 'dark'} onclick={() => setMatte(DEFAULT_MATTE)} title="#313338, Discord dark theme">Discord dark</button>
            <button type="button" aria-pressed={matteMode === 'light'} onclick={() => setMatte(WHITE_MATTE)} title="#ffffff, Discord light theme">Discord light</button>
            <button type="button" aria-pressed={matteMode === 'custom'} onclick={() => hexInput?.focus()}>Custom</button>
          </span>
          <input type="color" value={'#' + out.matte} oninput={(e) => setMatte(e.currentTarget.value)} aria-label="Matte colour" />
          <input
            bind:this={hexInput}
            type="text"
            class="hex mono"
            value={matteText}
            oninput={onMatteText}
            maxlength="7"
            spellcheck="false"
            aria-label="Matte hex"
          />
        </span>
      </span>
    </div>
    {#if out.format === 'gif'}
      <p class="hint">
        GIF has 1-bit alpha: every semi-transparent edge pixel is either dropped or blended onto this colour, so pick the matte for
        the theme your audience uses — on the other theme the edge shows as a thin outline. For soft alpha use WebP / AVIF
        (attachments, emoji) or APNG (stickers).
      </p>
    {/if}
  {/if}

  {#if stickerWebp}<p class="note error">{out.format.toUpperCase()} is not a Discord sticker format — the check will fail. Use APNG (best) or GIF.</p>{/if}
  {#if emoteApng}<p class="note error">APNG is not an animated-emoji format — Discord rejects it. Use GIF, WebP or AVIF for emotes.</p>{/if}
  {#if apngOffTarget && !emoteApng}<p class="note">APNG animates only when uploaded as a server sticker — as a chat attachment Discord shows frame 0 only.</p>{/if}
  {#if stickerTooLong}<p class="note">Stickers must be ≤ 5 s: trim or speed up the clip.</p>{/if}
  {#if webpTooWide}<p class="note info">Animated WebP above ~480 px takes Discord's proxy several seconds to show; consider a smaller width.</p>{/if}
</section>

<style>
  .output {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .output > h2 {
    margin-bottom: 0;
  }
  .presets {
    gap: 5px;
  }
  .swap {
    gap: 8px;
  }
  .static {
    display: inline-flex;
    align-items: center;
    min-height: 30px;
    font-size: 13px;
  }
  .field.slider {
    flex: 1 1 260px;
  }
  .field.slider input[type='range'] {
    flex: 1;
    min-width: 120px;
  }
  /* long captions wrap instead of widening the card on narrow screens */
  .field.slider > span:first-child,
  .field.wrap > span:first-child,
  .fit .hint {
    white-space: normal;
  }
  .hex {
    width: 84px;
  }
  .row.loop {
    gap: 8px;
  }
  .row.fit {
    gap: 6px 10px;
  }
  .loop-fixed {
    width: 220px;
  }
</style>
