<script lang="ts">
  // Background removal: Color keys one or more RGB colors — one op
  // "colorkey" per row, the eyedropper's pick from the preview still or a
  // typed hex; Screen keys a green / blue screen in YUV (op "chromakey" plus
  // despill), its key color auto-matched from the unkeyed preview frame when
  // Green / Blue is pressed (a real screen is rarely the pure preset), or
  // picked from the preview, or typed. The Edges fold (op "morph": fill
  // pinholes / shift edge / soft edge) follows whichever key is on. Sliders
  // re-render the still.
  import { caps } from '../../lib/capabilities.svelte';
  import {
    addKeyColor,
    app,
    armEyedropper,
    armScreenEyedropper,
    disarmEyedropper,
    MAX_KEY_COLORS,
    MORPH_MAX_GROW,
    MORPH_MAX_SMOOTH,
    morphShift,
    morphSoft,
    removeKeyColor,
    screenPreset,
    setBackgroundMode,
    setChromaColor,
    setKeyColor,
    setScreenColor,
    type BackgroundMode,
    type ScreenColor,
  } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';
  import ColorRows from './ColorRows.svelte';

  interface Props {
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
    /** offer the preview eyedropper and the Screen auto-match; false in batch (no preview — typed hex only) */
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
    { id: 'color', label: 'Color', title: 'Key out one or more RGB colors — picked from the preview or typed as hex' },
    { id: 'screen', label: 'Screen', title: 'Key out a green or blue screen in YUV with despill' },
  ];
  const current = $derived<ModeId>(bg.enabled ? bg.mode : 'none');
  const screen = $derived(bg.enabled && bg.mode === 'screen');
  const color = $derived(bg.enabled && bg.mode === 'color');
  const picking = $derived(app.ui.pickColor);
  const pickTarget = $derived(app.ui.pickTarget);
  const colorArmed = $derived(picking && pickTarget === 'color' ? app.ui.pickRow : -1);
  const screenArmed = $derived(picking && pickTarget === 'screen');
  const autoMatching = $derived(picking && pickTarget === 'screen-auto');
  /** the rows that carry a color (the ones that emit an op) */
  const picked = $derived(bg.colors.filter((c) => c !== ''));
  /** the Screen sub-choice's preset color */
  const preset = $derived(screenPreset(bg.screen));
  // An older server (features.keying off) rejects the chromakey / colorkey
  // ops: the card stays, with a notice.
  const supported = $derived(caps.features.keying);
  // The Edges fold is gated on features.morph: an older server 400s on the
  // morph op, and fill pinholes is on by default, so on such a server the
  // fold is hidden AND the cleanup switched off — a default must never get
  // a render rejected. (Until the server answers the flags are optimistic
  // and nothing is touched.)
  const morphSupported = $derived(caps.features.morph);
  $effect(() => {
    const m = bg.morph;
    if (caps.loaded && !caps.features.morph && (m.close || m.grow !== 0 || m.smooth !== 0)) app.ops.background.morph = { close: false, grow: 0, smooth: 0 };
  });

  const shift = $derived(morphShift(bg.morph));
  const soft = $derived(morphSoft(bg.morph));
  /** a signed pixel count: "−2 px", "0", "+3 px" */
  function signedPx(n: number): string {
    if (n === 0) return '0';
    return `${n < 0 ? '−' : '+'}${Math.abs(n)} px`;
  }
  const morphSummary = $derived.by(() => {
    const parts: string[] = [];
    if (bg.morph.close) parts.push('fill pinholes');
    if (shift !== 0) parts.push(`shift ${signedPx(shift)}`);
    if (soft > 0) parts.push(`soft ${soft} px`);
    return parts.join(' · ');
  });

  /** the Screen key color's hint: the auto-match in flight / its result, or a custom key */
  const screenHint = $derived.by(() => {
    if (autoMatching) return `matching the ${bg.screen} screen from the preview…`;
    const m = app.ui.screenMatch;
    if (m && m.which === bg.screen) {
      if (m.color && bg.color === m.color) return `matched #${m.color} from the preview`;
      if (!m.color && bg.color === preset) return `no ${bg.screen} screen found — using #${preset}`;
    }
    if (bg.color !== preset) return 'custom — despill follows the dominant channel';
    return '';
  });

  const summary = $derived.by(() => {
    if (!bg.enabled) return supported ? 'off' : 'off — not supported by this server';
    const tail = morphSummary ? ` · ${morphSummary}` : '';
    if (bg.mode === 'color') {
      if (!picked.length) return picker ? 'pick a color on the preview' : 'type a color to remove';
      const what = picked.length === 1 ? `color #${picked[0]}` : `${picked.length} colors`;
      return `${what} · similarity ${bg.pickSimilarity.toFixed(2)}${tail}`;
    }
    const custom = bg.color !== preset ? ` #${bg.color}` : '';
    return `${bg.screen}screen${custom} · similarity ${bg.similarity.toFixed(2)}${bg.despill ? '' : ' · despill off'}${tail}`;
  });

  // The segment and the header checkbox go through the same setter
  // (lib/state.setBackgroundMode): Color without a pick yet arms the
  // eyedropper for the first row straight away, so enabling the card by its
  // checkbox is as quick as clicking "Color"; entering Screen auto-matches
  // the key color from the preview.
  function setMode(m: ModeId) {
    setBackgroundMode(m, { picker });
  }
  function setScreen(s: ScreenColor) {
    setScreenColor(s, { picker });
  }

  // An armed pick only makes sense while the card is in the mode it lands
  // in: a Color row in Color mode, the Screen key color in Screen mode.
  $effect(() => {
    const ok = app.ui.pickTarget === 'color' ? bg.enabled && bg.mode === 'color' : bg.enabled && bg.mode === 'screen';
    if (app.ui.pickColor && !ok) disarmEyedropper();
  });

  /** a typed / color-input Screen key color: stored when valid, else the field is put back */
  function chromaHex(e: Event & { currentTarget: HTMLInputElement }) {
    if (!setChromaColor(e.currentTarget.value)) e.currentTarget.value = '#' + bg.color;
  }
  function toggleScreenEyedropper() {
    if (screenArmed) disarmEyedropper();
    else armScreenEyedropper();
  }
  function toggleEyedropper(i: number) {
    if (colorArmed === i) disarmEyedropper();
    else armEyedropper(i);
  }
  function addColor() {
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
          <button type="button" aria-pressed={current === m.id} title={m.title} onclick={() => setMode(m.id)}>{m.label}</button>
        {/each}
      </span>
    </span>
    {#if screen}
      <span class="field">
        <span>Screen</span>
        <span class="seg" role="group" aria-label="Screen color">
          <button type="button" aria-pressed={bg.screen === 'green'} title={picker ? 'Key out a green screen — the key color is matched from the preview' : 'Key out green (#00ff00)'} onclick={() => setScreen('green')}>Green</button>
          <button type="button" aria-pressed={bg.screen === 'blue'} title={picker ? 'Key out a blue screen — the key color is matched from the preview' : 'Key out blue (#0000ff)'} onclick={() => setScreen('blue')}>Blue</button>
        </span>
      </span>
    {/if}
  </div>

  {#if screen}
    <div class="row">
      <span class="field">
        <span>Key color</span>
        <span class="row tight">
          <input type="color" value={'#' + bg.color} oninput={(e) => setChromaColor(e.currentTarget.value)} aria-label="Key color" />
          <input type="text" class="hex mono" value={'#' + bg.color} onchange={chromaHex} maxlength="7" spellcheck="false" aria-label="Key color hex" />
          {#if picker}
            <button type="button" class="sm" class:primary={screenArmed} aria-pressed={screenArmed} onclick={toggleScreenEyedropper} title="Then click the screen on the preview">
              {screenArmed ? 'Click the preview…' : 'Pick from preview'}
            </button>
          {/if}
        </span>
        {#if screenHint}<span class="hint screen-hint" role="status">{screenHint}</span>{/if}
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
        <label class="inline" title="Remove the key color's spill from the band around the cut-out edge">
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
      Keys in YUV 4:4:4 at full resolution, before any scaling (soft edges survive). Green / Blue match the key color to the
      screen in the preview{#if picker}; Pick from preview takes it from a click{/if}. Similarity widens the keyed range; blend
      softens the cut-off. Despill removes the green / blue cast near the cut-out edge. The key judges each pixel by its 3×3
      neighbourhood, so at the default 0.10 a one-pixel rim of screen color can stay opaque along the hard edges of a strongly
      colored subject — around 0.15 keys it softly, or trim it with Shift edge.
    </p>
  {:else if color}
    <div class="row">
      <ColorRows
        colors={bg.colors}
        {picker}
        armedRow={colorArmed}
        max={MAX_KEY_COLORS}
        caption={bg.colors.length > 1 ? 'Colors to remove' : 'Color to remove'}
        hexLabel={'Color {i} to remove (hex)'}
        removeLabel={'Remove color {i}'}
        addTitle="Key another color (a 2-color ramp, a second flat tone) — up to {MAX_KEY_COLORS}"
        emptyHint={picker ? 'nothing picked yet' : 'type a hex value'}
        clearOnly
        onPick={toggleEyedropper}
        onHex={setKeyColor}
        onRemove={removeKeyColor}
        onAdd={addColor}
      />
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
      {#if picker}The eyedropper reads the preview still (shown unkeyed while it is armed); each{:else}Each{/if} color is keyed in RGB
      at full resolution, one key per row, all with the same similarity and blend. Raise similarity for gradients and JPEG noise;
      blend feathers the edge. “+ add color” handles a 2-color ramp or a second flat tone.
    </p>
  {:else}
    <p class="hint">
      Pick a mode to make a flat background transparent: Color keys picked colors, Screen a green or blue screen. Soft edges need
      WebP / AVIF / APNG output — GIF cuts them to 1-bit alpha.
    </p>
  {/if}

  {#if bg.enabled && morphSupported}
    <details class="adv" bind:open={edgeOpen}>
      <summary><span class="sum">Edges</span>{#if !edgeOpen}<span class="muted small">· {morphSummary || 'off'}</span>{/if}</summary>
      <div class="row">
        <label
          class="field slider"
          title="Negative trims the edge inward to remove a leftover rim of background; positive grows the subject. In source pixels."
        >
          <span>Shift edge (±{MORPH_MAX_GROW}) — <b>{signedPx(shift)}</b></span>
          <span class="row tight">
            <input type="range" min={-MORPH_MAX_GROW} max={MORPH_MAX_GROW} step="1" bind:value={app.ops.background.morph.grow} aria-label="Shift edge" />
            <NumField bind:value={app.ops.background.morph.grow} min={-MORPH_MAX_GROW} max={MORPH_MAX_GROW} step={1} small />
          </span>
        </label>
        <label class="field slider" title="Smooths a jagged outline into a clean edge. For a blurred, faded edge use the Feather card instead.">
          <span>Soft edge (0–{MORPH_MAX_SMOOTH}) — <b>{soft > 0 ? `${soft} px` : 'off'}</b></span>
          <span class="row tight">
            <input type="range" min="0" max={MORPH_MAX_SMOOTH} step="0.5" bind:value={app.ops.background.morph.smooth} aria-label="Soft edge" />
            <NumField bind:value={app.ops.background.morph.smooth} min={0} max={MORPH_MAX_SMOOTH} step={0.5} small />
          </span>
        </label>
        <label class="inline" title="A 3×3 close of the alpha: fills pinholes of up to 1 px without growing the silhouette">
          <input type="checkbox" bind:checked={app.ops.background.morph.close} /><span>Fill pinholes</span>
        </label>
      </div>
      <p class="hint">
        Shift edge: negative trims the edge inward to remove a leftover rim of background; positive grows the subject. In source
        pixels, like Feather, so 4 px on a 720 px source is under 1 px after the emote fit. Soft edge smooths a jagged outline into
        a clean edge; for a blurred, faded edge use the Feather card (only WebP / APNG / AVIF keep soft edges — GIF is 1-bit).
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
  .hex {
    width: 84px;
  }
  .screen-hint {
    white-space: normal;
  }
  .adv {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0 10px;
  }
  .hint + .adv,
  .row + .adv {
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
