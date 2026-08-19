<script lang="ts">
  import type { Dither, FitMode, OutputFormat } from '../lib/api';
  import { fitSize, fmtKiB, fmtNum, gifDelays, MAX_FPS, normalizeHex } from '../lib/format';
  import { DEFAULT_MATTE, LIMITS, PRESETS, presetById, TARGET_LABEL, TARGETS, WHITE_MATTE } from '../lib/presets';
  import { app, applyPreset, effectiveFPS, loopFor, previewDuration } from '../lib/state.svelte';
  import NumField from './NumField.svelte';

  const out = $derived(app.output);
  const preset = $derived(presetById(out.preset));
  const locked = $derived(preset.locksSize);
  const limit = $derived(LIMITS[out.target]);
  const info = $derived(app.source?.info ?? null);

  // Frame size entering the output stage (after crop / resize) — for the hint.
  const stackSize = $derived.by(() => {
    if (!info) return null;
    let w = info.width;
    let h = info.height;
    if (app.ops.crop.enabled) {
      w = app.ops.crop.w;
      h = app.ops.crop.h;
    }
    if (app.ops.resize.enabled) ({ w, h } = fitSize(w, h, app.ops.resize.width, app.ops.resize.height, app.ops.resize.fit));
    if (app.ops.flipRotate.enabled && (app.ops.flipRotate.degrees === 90 || app.ops.flipRotate.degrees === 270)) [w, h] = [h, w];
    return { w, h };
  });
  const finalSize = $derived.by(() => {
    if (!stackSize) return null;
    if (out.width > 0 && out.height > 0) return { w: out.width, h: out.height }; // canvas is padded/cropped to W×H
    return fitSize(stackSize.w, stackSize.h, out.width, out.height, out.fit);
  });

  // graph.Compile precedence: fps op → Output.fps → source fps, then snapped
  // for the format (GIF capped at 50, else 60).
  const srcFps = $derived(info?.fps ?? 0);
  const opFpsOn = $derived(app.ops.fps.enabled && app.ops.fps.fps > 0);
  const effFps = $derived(effectiveFPS(app.ops, out, srcFps));
  const outFpsIgnored = $derived(opFpsOn && out.fps > 0);

  // Loop count (GIF NETSCAPE semantics: 0 = forever, N = play N+1 times) is
  // editable only with no Discord target; every Discord target requires
  // loop forever and always gets 0 (see loopFor / discordlint).
  const loopEditable = $derived(out.target === '');
  const loop = $derived(loopFor(out));
  const loopHint = $derived(loop === 0 ? 'forever' : `plays ${loop + 1} times (loop count ${loop})`);

  const formats: { id: OutputFormat; label: string }[] = [
    { id: 'gif', label: 'GIF' },
    { id: 'webp', label: 'WebP (animated)' },
  ];
  const fits: FitMode[] = ['contain', 'cover', 'exact'];
  const dithers: { id: Dither; label: string }[] = [
    { id: 'bayer', label: 'bayer (ordered, smallest, no shimmer)' },
    { id: 'sierra2_4a', label: 'sierra2_4a (photographic)' },
    { id: 'floyd_steinberg', label: 'floyd_steinberg' },
    { id: 'none', label: 'none' },
  ];
  const colorChoices = [256, 128, 64, 32];

  const matteMode = $derived(out.matte === DEFAULT_MATTE ? 'dark' : out.matte === WHITE_MATTE ? 'white' : 'custom');
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

  // The two chat presets differ only by format: keep the chip in sync when the
  // user flips the format, without resetting the other fields.
  function onFormatChange() {
    if (out.preset === 'chat-gif' && out.format === 'webp') app.output.preset = 'chat-webp';
    else if (out.preset === 'chat-webp' && out.format === 'gif') app.output.preset = 'chat-gif';
  }

  const webpTooWide = $derived(out.format === 'webp' && (finalSize?.w ?? 0) > 480);
  const stickerWebp = $derived(out.target === 'sticker' && out.format === 'webp');
  const stickerTooLong = $derived(out.target === 'sticker' && !!info && previewDuration(info, app.ops) > 5.0);
</script>

<section class="card output">
  <h2>Output</h2>

  <div class="chips presets" role="group" aria-label="Preset">
    {#each PRESETS as p (p.id)}
      <button type="button" class="chip" aria-pressed={out.preset === p.id} title={p.hint} onclick={() => applyPreset(p.id)}>
        {p.label}
      </button>
    {/each}
  </div>
  <p class="hint">{preset.hint}</p>
  {#if preset.warn}<p class="note">{preset.warn}</p>{/if}

  <div class="row">
    <label class="field">
      <span>Format</span>
      <select bind:value={app.output.format} onchange={onFormatChange}>
        {#each formats as f (f.id)}<option value={f.id}>{f.label}</option>{/each}
      </select>
    </label>
    <label class="field">
      <span>Discord target</span>
      {#if out.preset === 'custom'}
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
  </div>

  <div class="row">
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
        title={opFpsOn ? 'The Frame rate op has precedence over this value' : '0 = source fps; GIF is capped at 50, WebP at 60'}
      />
    </label>
    {#if finalSize}
      <span class="hint">
        → <b>{finalSize.w}×{finalSize.h}</b>{#if effFps > 0}&nbsp;· <b>{fmtNum(effFps)} fps</b>{#if out.format === 'gif'}
            <span class="muted">&nbsp;({gifDelays(effFps)} cs delays)</span>{/if}{#if outFpsIgnored}
            <span class="muted">&nbsp;· the Frame rate op overrides the fps above</span>{/if}{/if}
        {#if out.fit === 'contain' && out.width > 0 && out.height > 0}<span class="muted"> · transparent padding</span>{/if}
      </span>
    {/if}
  </div>

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

  <hr class="sep" />

  {#if out.format === 'gif'}
    <div class="row">
      <label class="field">
        <span>Colours</span>
        <select bind:value={app.output.colors}>
          {#each colorChoices as c (c)}<option value={c}>{c}</option>{/each}
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
    <div class="row">
      <label class="field slider">
        <span>Alpha threshold (1–255) — <b>{out.alphaThreshold}</b> (pixels below become transparent)</span>
        <span class="row">
          <input type="range" min="1" max="255" step="1" bind:value={app.output.alphaThreshold} aria-label="Alpha threshold" />
          <NumField bind:value={app.output.alphaThreshold} min={1} max={255} small />
        </span>
      </label>
    </div>
    <div class="row">
      <span class="field">
        <span>Matte (blended under semi-transparent edges)</span>
        <span class="row">
          <span class="seg" role="group" aria-label="Matte">
            <button type="button" aria-pressed={matteMode === 'dark'} onclick={() => setMatte(DEFAULT_MATTE)}>Discord dark</button>
            <button type="button" aria-pressed={matteMode === 'white'} onclick={() => setMatte(WHITE_MATTE)}>White</button>
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
  {:else}
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
      Alpha stays 8-bit and lossless in this path; libwebp_anim, no metadata, plays {loop === 0 ? 'forever' : `${loop + 1} times`}.
    </p>
  {/if}

  {#if stickerWebp}<p class="note error">WebP is not a Discord sticker format — the check will fail. Use GIF (or APNG in Phase 2).</p>{/if}
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
  .hex {
    width: 84px;
  }
  .row.loop {
    gap: 8px;
  }
  .loop-fixed {
    width: 220px;
  }
</style>
