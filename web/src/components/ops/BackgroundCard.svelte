<script lang="ts">
  // Background removal: Greenscreen / Bluescreen (op "chromakey", YUV keying
  // plus despill) or "Pick a colour" (op "colorkey" on the colour the
  // eyedropper reads from the preview still). Sliders re-render the still.
  import { caps } from '../../lib/capabilities.svelte';
  import { normalizeHex } from '../../lib/format';
  import { app, CHROMA_BLUE, CHROMA_GREEN, type BackgroundMode } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
  }
  let { initialOpen = false }: Props = $props();

  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  let advOpen = $state(false);
  const bg = $derived(app.ops.background);
  type ModeId = BackgroundMode | 'none';
  const modes: { id: ModeId; label: string; title: string }[] = [
    { id: 'none', label: 'None', title: 'Keep the background' },
    { id: 'green', label: 'Greenscreen', title: 'Key out green (#00ff00) in YUV with despill' },
    { id: 'blue', label: 'Bluescreen', title: 'Key out blue (#0000ff) in YUV with despill' },
    { id: 'pick', label: 'Pick a colour', title: 'Key out one RGB colour picked from the preview' },
  ];
  const current = $derived<ModeId>(bg.enabled ? bg.mode : 'none');
  const chroma = $derived(bg.enabled && bg.mode !== 'pick');
  const picking = $derived(app.ui.pickColor);
  // An older server (features.keying off) rejects the chromakey / colorkey
  // ops: the card stays, with a notice.
  const supported = $derived(caps.features.keying);

  const summary = $derived.by(() => {
    if (!bg.enabled) return supported ? 'off' : 'off — not supported by this server';
    if (bg.mode === 'pick') return bg.pickColor ? `colour #${bg.pickColor} · similarity ${bg.pickSimilarity.toFixed(2)}` : 'pick a colour on the preview';
    const name = bg.mode === 'green' ? 'greenscreen' : 'bluescreen';
    return `${name} #${bg.color} · similarity ${bg.similarity.toFixed(2)}${bg.despill ? ' · despill' : ''}`;
  });

  function setMode(m: ModeId) {
    if (m === 'none') {
      app.ops.background.enabled = false;
      app.ui.pickColor = false;
      return;
    }
    app.ops.background.mode = m;
    app.ops.background.enabled = true;
    if (m === 'green') app.ops.background.color = CHROMA_GREEN;
    else if (m === 'blue') app.ops.background.color = CHROMA_BLUE;
    // Pick a colour without one yet: arm the eyedropper straight away.
    app.ui.pickColor = m === 'pick' && !bg.pickColor;
  }

  // The eyedropper only makes sense while the card is in pick mode.
  $effect(() => {
    if (app.ui.pickColor && !(bg.enabled && bg.mode === 'pick')) app.ui.pickColor = false;
  });

  function setChromaColor(hex: string) {
    const n = normalizeHex(hex);
    if (n) app.ops.background.color = n;
  }
  function setPickColor(hex: string) {
    const n = normalizeHex(hex);
    if (n) {
      app.ops.background.pickColor = n;
      app.ui.pickColor = false;
    }
  }
  function toggleEyedropper() {
    app.ui.pickColor = !app.ui.pickColor;
  }
</script>

<OpCard title="Background" {summary} bind:enabled={app.ops.background.enabled} bind:open>
  {#if !supported}
    <p class="note">This server does not support background removal (an older ezlg) — the keying op will be rejected at render; update the server.</p>
  {/if}
  <div class="row">
    <span class="field">
      <span>Mode</span>
      <span class="seg" role="group" aria-label="Background mode">
        {#each modes as m (m.id)}
          <button type="button" aria-pressed={current === m.id} title={m.title} onclick={() => setMode(m.id)}>{m.label}</button>
        {/each}
      </span>
    </span>
  </div>

  {#if chroma}
    <div class="row">
      <span class="field">
        <span>Key colour</span>
        <span class="row tight">
          <input type="color" value={'#' + bg.color} oninput={(e) => setChromaColor(e.currentTarget.value)} aria-label="Key colour" />
          <input type="text" class="hex mono" value={'#' + bg.color} onchange={(e) => setChromaColor(e.currentTarget.value)} maxlength="7" spellcheck="false" aria-label="Key colour hex" />
          {#if bg.color !== CHROMA_GREEN && bg.color !== CHROMA_BLUE}<span class="hint">custom — despill follows the dominant channel</span>{/if}
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
      blend softens the cut-off. Despill removes the green / blue cast from edge pixels.
    </p>
  {:else if bg.enabled}
    <div class="row">
      <span class="field">
        <span>Colour to remove</span>
        <span class="row tight">
          <button type="button" class="sm" class:primary={picking} aria-pressed={picking} onclick={toggleEyedropper} title="Then click the colour on the preview">
            {picking ? 'Click the preview…' : bg.pickColor ? 'Pick again' : 'Pick from preview'}
          </button>
          {#if bg.pickColor}
            <span class="swatch" style:background={'#' + bg.pickColor} aria-hidden="true"></span>
            <input type="text" class="hex mono" value={'#' + bg.pickColor} onchange={(e) => setPickColor(e.currentTarget.value)} maxlength="7" spellcheck="false" aria-label="Colour to remove (hex)" />
          {:else}
            <span class="hint">nothing picked yet</span>
          {/if}
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
      The eyedropper reads the preview still (shown unkeyed while it is armed); the colour is keyed in RGB at full
      resolution. Raise similarity for gradients and JPEG noise; blend feathers the edge.
    </p>
  {:else}
    <p class="hint">Pick a mode to make a flat background transparent. Soft edges need WebP / AVIF / APNG output — GIF cuts them to 1-bit alpha.</p>
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
  .hex {
    width: 84px;
  }
  .swatch {
    display: inline-block;
    width: 24px;
    height: 24px;
    border-radius: 4px;
    border: 1px solid var(--border-strong);
  }
  .adv {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0 10px;
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
