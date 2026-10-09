<script lang="ts">
  // Background removal: AI (Phase 5b) keys with the matte sidecar's model
  // (op "matte" — the Model select is fed by GET /api/matte: ids, labels,
  // live states, the server's default; the status line says ready / GPU /
  // the clip's estimate, loading, downloading, or why the sidecar is
  // unavailable); Colour (Phase 5a) keys one or more RGB colours — one op
  // "colorkey" per row, the eyedropper's pick from the preview still or a
  // typed hex; Screen keys a green / blue screen in YUV (op "chromakey" plus
  // despill). The Edge cleanup fold (op "morph": fill pinholes / grow the
  // matte) follows whichever key is on. Sliders re-render the still. The AI
  // mode is disabled with the reason on a server without features.matte (a
  // plain install without the compose profile) and never starts a GPU pass
  // by itself: the preview's next still does.
  import { MATTE_MODEL_DEFAULT } from '../../lib/api';
  import { caps } from '../../lib/capabilities.svelte';
  import { normalizeHex } from '../../lib/format';
  import { matteEstimate, matteModel, matteModelFor, matteModelOptions, matteStatusLine, MATTE_PROFILE_HINT, modelLabel } from '../../lib/matte';
  import { holdMattePolling, matte, refreshMatte } from '../../lib/matte.svelte';
  import {
    addKeyColor,
    app,
    armEyedropper,
    CHROMA_BLUE,
    CHROMA_GREEN,
    disarmEyedropper,
    effectiveOps,
    MAX_KEY_COLORS,
    MORPH_MAX_GROW,
    planFrames,
    removeKeyColor,
    setBackgroundMode,
    setKeyColor,
    setMatteModel,
    type BackgroundMode,
    type ScreenColor,
  } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
    /** offer the preview eyedropper; false in batch (no preview to pick from — Colour takes typed hex only) */
    picker?: boolean;
  }
  let { initialOpen = false, picker = true }: Props = $props();

  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  let advOpen = $state(false);
  let edgeOpen = $state(false);
  const bg = $derived(app.ops.background);
  type ModeId = BackgroundMode | 'none';
  const modes: { id: ModeId; label: string; title: string }[] = [
    { id: 'none', label: 'None', title: 'Keep the background' },
    { id: 'ai', label: 'AI', title: 'Cut the subject out with the AI matte of the matte sidecar (any background; computed once per frame and cached)' },
    { id: 'colour', label: 'Colour', title: 'Key out one or more RGB colours — picked from the preview or typed as hex' },
    { id: 'screen', label: 'Screen', title: 'Key out a green or blue screen in YUV with despill' },
  ];
  const current = $derived<ModeId>(bg.enabled ? bg.mode : 'none');
  const ai = $derived(bg.enabled && bg.mode === 'ai');
  const screen = $derived(bg.enabled && bg.mode === 'screen');
  const colour = $derived(bg.enabled && bg.mode === 'colour');
  const picking = $derived(app.ui.pickColor);
  const pickRow = $derived(app.ui.pickRow);
  /** the rows that carry a colour (the ones that emit an op) */
  const picked = $derived(bg.colors.filter((c) => c !== ''));
  /** the Screen sub-choice's preset colour; anything else in the key colour field is custom */
  const screenPreset = $derived(bg.screen === 'blue' ? CHROMA_BLUE : CHROMA_GREEN);
  // An older server (features.keying off) rejects the chromakey / colorkey
  // ops: the card stays, with a notice.
  const supported = $derived(caps.features.keying);
  // The Edge cleanup fold is gated on features.morph: an older server 400s
  // on the morph op, and fill pinholes is on by default, so on such a server
  // the fold is hidden AND the cleanup switched off — a default must never
  // get a render rejected. (Until the server answers the flags are
  // optimistic and nothing is touched.)
  const morphSupported = $derived(caps.features.morph);
  $effect(() => {
    if (caps.loaded && !caps.features.morph && (bg.morph.close || bg.morph.grow > 0)) app.ops.background.morph = { close: false, grow: 0 };
  });

  // ---- AI mode (Phase 5b). features.matte is true only while the app's
  // probe of the sidecar answers: off, the AI segment is disabled with the
  // reason (the /api/matte reason when it can be read — "no matte service
  // is configured (EZLG_MATTE_URL is empty …)" — else the standard note).
  // The live states come from GET /api/matte (lib/matte.svelte.ts), polled
  // while the AI body is on screen; the Render panel polls while AI is on
  // and the card is collapsed.
  const aiSupported = $derived(caps.features.matte);
  const aiReason = $derived.by(() => {
    if (aiSupported) return '';
    const r = matte.status?.reason?.trim();
    return r ? `AI matte is off on this server: ${r}` : `This server has no AI matte sidecar (features.matte off) — run \`${MATTE_PROFILE_HINT}\` next to it, or update the server`;
  });
  $effect(() => {
    if (open && ai) return holdMattePolling();
  });
  // The reason for a disabled AI mode is worth one read of /api/matte when
  // the card opens (no poll: nothing changes while the feature is off).
  $effect(() => {
    if (open && caps.loaded && !caps.features.matte && !matte.loaded) void refreshMatte();
  });
  /** the model the select shows and the op names: the choice, else the server's default, else the recipe default */
  const model = $derived(matteModelFor(bg.ai.model, matte.status));
  // As soon as the server's default is known it becomes the explicit choice,
  // so what the select shows is what the op sends (state.AiCfg).
  $effect(() => {
    const d = matte.status?.defaultModel?.trim();
    if (d && !app.ops.background.ai.model) app.ops.background.ai.model = d;
  });
  const options = $derived(matteModelOptions(matte.status));
  /** the chosen model is not among the offered ones (the server will refuse it): shown as an extra, flagged option */
  const unlisted = $derived(!options.some((o) => o.id === model));
  /** the forward frame count of the clip (the matte pass runs on the temporal prefix, before a bounce doubles it) */
  const aiFrames = $derived.by(() => {
    const src = app.source;
    if (!src) return 0;
    const ops = effectiveOps(app.ops, app.output);
    const n = planFrames(src.info, ops, app.output);
    return ops.bounce && !src.info.isStill && n >= 2 ? n / 2 : n;
  });
  const aiEstimate = $derived(ai ? matteEstimate(matte.status, model, aiFrames) : null);
  const statusLine = $derived.by(() => {
    if (matte.status) return matteStatusLine(matte.status, model, aiEstimate?.ms ?? 0);
    if (matte.error) return `matte status unavailable — ${matte.error}`;
    return matteStatusLine(null, model);
  });
  const aiLabel = $derived(modelLabel(model, matteModel(matte.status, model)));

  const morphSummary = $derived.by(() => {
    const parts: string[] = [];
    if (bg.morph.close) parts.push('fill pinholes');
    if (bg.morph.grow > 0) parts.push(`grow ${bg.morph.grow} px`);
    return parts.join(' · ');
  });
  const summary = $derived.by(() => {
    if (!bg.enabled) return supported ? 'off' : 'off — not supported by this server';
    const tail = morphSummary ? ` · ${morphSummary}` : '';
    if (bg.mode === 'ai') {
      const which = model !== MATTE_MODEL_DEFAULT ? ` · ${aiLabel}` : '';
      return `AI${which}${aiSupported ? '' : ' — not available on this server'}${tail}`;
    }
    if (bg.mode === 'colour') {
      if (!picked.length) return picker ? 'pick a colour on the preview' : 'type a colour to remove';
      const what = picked.length === 1 ? `colour #${picked[0]}` : `${picked.length} colours`;
      return `${what} · similarity ${bg.pickSimilarity.toFixed(2)}${tail}`;
    }
    const custom = bg.color !== screenPreset ? ` #${bg.color}` : '';
    return `${bg.screen}screen${custom} · similarity ${bg.similarity.toFixed(2)}${bg.despill ? '' : ' · despill off'}${tail}`;
  });

  // The segment and the header checkbox go through the same setter
  // (lib/state.setBackgroundMode): Colour without a pick yet arms the
  // eyedropper for the first row straight away, so enabling the card by its
  // checkbox is as quick as clicking "Colour".
  function setMode(m: ModeId) {
    setBackgroundMode(m, { picker });
  }
  function setScreen(s: ScreenColor) {
    app.ops.background.screen = s;
    app.ops.background.color = s === 'blue' ? CHROMA_BLUE : CHROMA_GREEN;
  }

  // The eyedropper only makes sense while the card is in Colour mode.
  $effect(() => {
    if (app.ui.pickColor && !(bg.enabled && bg.mode === 'colour')) disarmEyedropper();
  });

  function setChromaColor(hex: string) {
    const n = normalizeHex(hex);
    if (n) app.ops.background.color = n;
  }
  /** a typed row hex: stored when valid, else the field is put back to what the row holds */
  function onHex(i: number, e: Event & { currentTarget: HTMLInputElement }) {
    if (!setKeyColor(i, e.currentTarget.value)) e.currentTarget.value = bg.colors[i] ? '#' + bg.colors[i] : '';
  }
  function toggleEyedropper(i: number) {
    if (picking && pickRow === i) disarmEyedropper();
    else armEyedropper(i);
  }
  function addColour() {
    const i = addKeyColor();
    if (i >= 0 && picker) armEyedropper(i);
  }
</script>

<OpCard title="Background" {summary} bind:enabled={() => bg.enabled, (v: boolean) => setMode(v ? bg.mode : 'none')} bind:open>
  {#if !supported}
    <p class="note">This server does not support background removal (an older ezlg) — the keying op will be rejected at render; update the server.</p>
  {/if}
  <div class="row">
    <span class="field">
      <span>Mode</span>
      <span class="seg" role="group" aria-label="Background mode">
        {#each modes as m (m.id)}
          <button
            type="button"
            aria-pressed={current === m.id}
            title={m.id === 'ai' && !aiSupported ? aiReason : m.title}
            disabled={m.id === 'ai' && !aiSupported}
            onclick={() => setMode(m.id)}
          >
            {m.label}
          </button>
        {/each}
      </span>
    </span>
    {#if screen}
      <span class="field">
        <span>Screen</span>
        <span class="seg" role="group" aria-label="Screen colour">
          <button type="button" aria-pressed={bg.screen === 'green'} title="Key out green (#00ff00)" onclick={() => setScreen('green')}>Green</button>
          <button type="button" aria-pressed={bg.screen === 'blue'} title="Key out blue (#0000ff)" onclick={() => setScreen('blue')}>Blue</button>
        </span>
      </span>
    {/if}
  </div>

  {#if ai}
    {#if !aiSupported}
      <p class="note">{aiReason} — the matte op will be rejected at render.</p>
    {/if}
    <div class="row">
      <label class="field">
        <span>Model</span>
        <select class="model" aria-label="Model" value={model} onchange={(e) => setMatteModel(e.currentTarget.value)}>
          {#each options as o (o.id)}
            <option value={o.id} selected={o.id === model} disabled={o.disabled} title={o.reason || undefined}>{o.text}</option>
          {/each}
          {#if unlisted}
            <option value={model} selected>{model}{options.length ? ' — not offered by this server' : ''}</option>
          {/if}
        </select>
      </label>
      <span class="field status-field">
        <span>Status</span>
        <span class="status" class:bad={!aiSupported || matte.status?.device === 'unavailable'} role="status">{statusLine}</span>
      </span>
    </div>
    <p class="hint">
      Computed once per frame and cached; trim, speed and fps changes re-use what is cached. On frames that already carry
      transparency the matte is intersected with it, never substituted. Anime (fast) is crisp and quick; General (precise) keeps
      hair strands and rejects stream UI at about 10× the time. Soft edges need WebP / AVIF / APNG output — GIF cuts them to
      1-bit alpha.
    </p>
  {:else if screen}
    <div class="row">
      <span class="field">
        <span>Key colour</span>
        <span class="row tight">
          <input type="color" value={'#' + bg.color} oninput={(e) => setChromaColor(e.currentTarget.value)} aria-label="Key colour" />
          <input type="text" class="hex mono" value={'#' + bg.color} onchange={(e) => setChromaColor(e.currentTarget.value)} maxlength="7" spellcheck="false" aria-label="Key colour hex" />
          {#if bg.color !== screenPreset}<span class="hint">custom — despill follows the dominant channel</span>{/if}
        </span>
      </span>
      <label class="field slider">
        <span>Similarity (0.01–1) — <b>{bg.similarity.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0.01" max="1" step="0.01" bind:value={app.ops.background.similarity} aria-label="Similarity" />
          <NumField bind:value={app.ops.background.similarity} min={0.01} max={1} step={0.01} small />
        </span>
      </label>
      <label class="field slider">
        <span>Blend (0.01–1) — <b>{bg.blend.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0.01" max="1" step="0.01" bind:value={app.ops.background.blend} aria-label="Blend" />
          <NumField bind:value={app.ops.background.blend} min={0.01} max={1} step={0.01} small />
        </span>
      </label>
    </div>
    <details class="adv" bind:open={advOpen}>
      <summary><span class="sum">Advanced</span>{#if !advOpen}<span class="muted small">· despill {bg.despill ? `on · mix ${bg.despillMix.toFixed(2)} · expand ${bg.despillExpand.toFixed(2)}` : 'off'}</span>{/if}</summary>
      <div class="row">
        <label class="inline" title="Remove the key colour's spill from edges and semi-transparent pixels">
          <input type="checkbox" bind:checked={app.ops.background.despill} /><span>Despill</span>
        </label>
        <label class="field">
          <span>Mix (0–1)</span>
          <NumField bind:value={app.ops.background.despillMix} min={0} max={1} step={0.05} disabled={!bg.despill} small />
        </label>
        <label class="field">
          <span>Expand (0–1)</span>
          <NumField bind:value={app.ops.background.despillExpand} min={0} max={1} step={0.05} disabled={!bg.despill} small />
        </label>
      </div>
    </details>
    <p class="hint">
      Keys in YUV 4:4:4 at full resolution, before any scaling (soft edges survive). Similarity widens the keyed range;
      blend softens the cut-off. Despill removes the green / blue cast from edge pixels. The key judges each pixel by its
      3×3 neighbourhood, so at the default 0.10 a one-pixel rim of screen colour can stay opaque along the hard edges of a
      strongly coloured subject — around 0.15 keys it softly.
    </p>
  {:else if colour}
    <div class="row">
      <span class="field colours">
        <span>{bg.colors.length > 1 ? 'Colours to remove' : 'Colour to remove'}</span>
        {#each bg.colors as c, i (i)}
          <span class="row tight colour-row">
            {#if picker}
              <button
                type="button"
                class="sm"
                class:primary={picking && pickRow === i}
                aria-pressed={picking && pickRow === i}
                onclick={() => toggleEyedropper(i)}
                title="Then click the colour on the preview"
              >
                {picking && pickRow === i ? 'Click the preview…' : c ? 'Pick again' : 'Pick from preview'}
              </button>
            {/if}
            <span class="swatch" class:empty={!c} style:background={c ? '#' + c : undefined} aria-hidden="true"></span>
            <input
              type="text"
              class="hex mono"
              value={c ? '#' + c : ''}
              placeholder="#rrggbb"
              onchange={(e) => onHex(i, e)}
              maxlength="7"
              spellcheck="false"
              aria-label="Colour {i + 1} to remove (hex)"
            />
            {#if bg.colors.length > 1 || c}
              <button type="button" class="sm ghost" onclick={() => removeKeyColor(i)} aria-label="Remove colour {i + 1}" title={bg.colors.length > 1 ? 'Remove this colour' : 'Clear this colour'}>×</button>
            {/if}
          </span>
        {/each}
        <span class="row tight">
          <button type="button" class="sm" onclick={addColour} disabled={bg.colors.length >= MAX_KEY_COLORS} title="Key another colour (a 2-colour ramp, a second flat tone) — up to {MAX_KEY_COLORS}">+ add colour</button>
          {#if !picked.length}<span class="hint">{picker ? 'nothing picked yet' : 'type a hex value'}</span>{/if}
        </span>
      </span>
      <label class="field slider">
        <span>Similarity (0.01–1) — <b>{bg.pickSimilarity.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0.01" max="1" step="0.01" bind:value={app.ops.background.pickSimilarity} aria-label="Similarity" />
          <NumField bind:value={app.ops.background.pickSimilarity} min={0.01} max={1} step={0.01} small />
        </span>
      </label>
      <label class="field slider">
        <span>Blend (0–1) — <b>{bg.pickBlend.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0" max="1" step="0.01" bind:value={app.ops.background.pickBlend} aria-label="Blend" />
          <NumField bind:value={app.ops.background.pickBlend} min={0} max={1} step={0.01} small />
        </span>
      </label>
    </div>
    <p class="hint">
      {#if picker}The eyedropper reads the preview still (shown unkeyed while it is armed); each{:else}Each{/if} colour is keyed in RGB
      at full resolution, one key per row, all with the same similarity and blend. Raise similarity for gradients and JPEG noise;
      blend feathers the edge. “+ add colour” handles a 2-colour ramp or a second flat tone.
    </p>
  {:else}
    <p class="hint">
      Pick a mode to make the background transparent: AI cuts the subject out of any background, Colour and Screen key a flat
      one. Soft edges need WebP / AVIF / APNG output — GIF cuts them to 1-bit alpha.
    </p>
  {/if}

  {#if bg.enabled && morphSupported}
    <details class="adv" bind:open={edgeOpen}>
      <summary><span class="sum">Edge cleanup</span>{#if !edgeOpen}<span class="muted small">· {morphSummary || 'off'}</span>{/if}</summary>
      <div class="row">
        <label class="inline" title="A 3×3 close of the alpha: fills pinholes of up to 1 px without growing the silhouette">
          <input type="checkbox" bind:checked={app.ops.background.morph.close} /><span>Fill pinholes</span>
        </label>
        <label class="inline" title="Extra 3×3 dilations after the close: each grows the matte by one source pixel (0–{MORPH_MAX_GROW})">
          <span>Grow matte</span>
          <NumField bind:value={app.ops.background.morph.grow} min={0} max={MORPH_MAX_GROW} step={1} small />
          <span>source px</span>
        </label>
      </div>
      <p class="hint">
        Grow +1–2 recovers eaten interiors at a 2 px fringe; in source pixels, like Feather, so 4 px on a 720 px source is
        under 1 px after the emote fit. Soft edges are the Feather card (only WebP / APNG / AVIF keep them — GIF is 1-bit).
      </p>
    </details>
  {/if}
</OpCard>

<style>
  .row.tight {
    gap: 6px;
    flex-wrap: nowrap;
  }
  .field.slider {
    flex: 1 1 220px;
  }
  .field.slider input[type='range'] {
    flex: 1;
    min-width: 100px;
  }
  .field.slider > span:first-child {
    white-space: normal;
  }
  .field.colours {
    gap: 6px;
  }
  .field.colours > .row + .row {
    margin-top: 0;
  }
  .hex {
    width: 84px;
  }
  .swatch {
    display: inline-block;
    width: 24px;
    height: 24px;
    border-radius: 4px;
    border: 1px solid var(--border-strong);
    flex: none;
  }
  .swatch.empty {
    border-style: dashed;
    background: transparent;
  }
  select.model {
    min-width: 180px;
  }
  .status-field {
    flex: 1 1 220px;
  }
  .status {
    font-size: 12.5px;
    white-space: normal;
    word-break: break-word;
  }
  .status.bad {
    color: var(--amber);
  }
  .adv {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0 10px;
  }
  .hint + .adv {
    margin-top: 8px;
  }
  .adv > summary {
    cursor: pointer;
    padding: 6px 0;
    font-size: 12.5px;
    user-select: none;
    display: flex;
    align-items: baseline;
    gap: 6px;
    flex-wrap: wrap;
  }
  .adv > summary .sum {
    font-weight: 600;
  }
  .adv[open] > summary {
    border-bottom: 1px solid var(--border);
    margin-bottom: 8px;
  }
  .adv > .row {
    margin-bottom: 8px;
  }
</style>
