<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { ratioLabel, sizeForHeight, sizeForWidth } from '../../lib/croprect';
  import { clamp } from '../../lib/format';
  import { app, backgroundOp, centerSquareCrop, setCropRatioLock } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    info: ProbeInfo;
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
  }
  let { info, initialOpen = false }: Props = $props();

  const crop = $derived(app.ops.crop);
  const auto = $derived(app.ops.autocrop);
  /** the locked w:h aspect ratio (> 0 = "Lock ratio" on; review R2) */
  const lockedRatio = $derived(app.ui.cropRatio);
  // Keying runs before the detection (DESIGN §4.3: keying precedes all
  // geometry), so with a Background op on auto-crop finds the box of what is
  // left after background removal — and that content has alpha whatever the
  // source, so the threshold applies to it too.
  const keyed = $derived(backgroundOp(app.ops.background) !== null);
  const alphaContent = $derived(info.hasAlpha || keyed);
  const summary = $derived.by(() => {
    if (auto.enabled) return `auto-crop to content${auto.padding > 0 ? ` + ${auto.padding} px` : ''}`;
    return crop.enabled ? `${crop.w}×${crop.h} at ${crop.x},${crop.y}` : `full frame ${info.width}×${info.height}`;
  });

  // The header toggle covers both the manual rectangle and auto-crop: off
  // clears both; on (re)enables the rectangle unless auto-crop already is.
  function getEnabled(): boolean {
    return crop.enabled || auto.enabled;
  }
  function setEnabled(v: boolean) {
    if (v) {
      if (!crop.enabled && !auto.enabled) app.ops.crop.enabled = true;
    } else {
      app.ops.crop.enabled = false;
      app.ops.autocrop.enabled = false;
    }
  }
  /**
   * The auto-crop checkbox must not fall through to the card's auto-enable
   * (unticking it would switch the rectangle on). A checkbox fires `input`
   * and then `change`, and OpCard listens to both, so both are handled here
   * (idempotently) and stopped.
   */
  function onAutoChange(e: Event & { currentTarget: HTMLInputElement }) {
    app.ops.autocrop.enabled = e.currentTarget.checked;
    e.stopPropagation();
  }

  // While the card is open (and auto-crop is off) the preview shows the full
  // pre-crop frame with the drag overlay (see Preview.svelte / CropOverlay.svelte).
  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  let advOpen = $state(false);
  $effect(() => {
    app.ui.cropOpen = open;
    return () => {
      app.ui.cropOpen = false;
    };
  });

  // Keep the rectangle inside the frame whatever the user types.
  function fix() {
    const c = app.ops.crop;
    c.w = clamp(Math.round(c.w), 1, info.width);
    c.h = clamp(Math.round(c.h), 1, info.height);
    c.x = clamp(Math.round(c.x), 0, info.width - c.w);
    c.y = clamp(Math.round(c.y), 0, info.height - c.h);
  }
  /**
   * An edited W drags H along while the ratio is locked (H = round(W / ratio));
   * when the derived H clamps at the frame edge the typed W shrinks with it
   * (sizeForWidth), so the pair always keeps the locked ratio instead of the
   * lock silently breaking (WEB-11).
   */
  function onWidth() {
    fix();
    if (lockedRatio > 0) {
      const s = sizeForWidth(app.ops.crop.w, lockedRatio, info.width, info.height);
      app.ops.crop.w = s.w;
      app.ops.crop.h = s.h;
      fix();
    }
  }
  /** an edited H drags W along while the ratio is locked (W = round(H × ratio)); clamps shrink the typed H (WEB-11). */
  function onHeight() {
    fix();
    if (lockedRatio > 0) {
      const s = sizeForHeight(app.ops.crop.h, lockedRatio, info.width, info.height);
      app.ops.crop.w = s.w;
      app.ops.crop.h = s.h;
      fix();
    }
  }
  /**
   * The lock is a UI constraint, not part of the op — like onAutoChange it
   * must not fall through to the card's auto-enable, so both events are
   * handled (idempotently) and stopped.
   */
  function onLockChange(e: Event & { currentTarget: HTMLInputElement }) {
    setCropRatioLock(e.currentTarget.checked);
    e.stopPropagation();
  }
  /** resetCrop clears the crop: op off, rectangle back to the full frame. */
  function resetCrop() {
    app.ops.crop = { enabled: false, x: 0, y: 0, w: info.width, h: info.height };
  }
  function centerSquare() {
    centerSquareCrop(info.width, info.height);
  }
</script>

<OpCard title="Crop" {summary} bind:enabled={getEnabled, setEnabled} bind:open>
  <div class="row">
    <label
      class="inline"
      title="Crop to the bounding box of the content over the whole (trimmed) clip — alpha for transparent sources and after background removal, non-black borders otherwise"
    >
      <input type="checkbox" checked={auto.enabled} oninput={onAutoChange} onchange={onAutoChange} />
      <span><b>Auto-crop to content</b></span>
    </label>
    <label class="field"><span>Padding (px each side)</span><NumField bind:value={app.ops.autocrop.padding} min={0} max={1024} small disabled={!auto.enabled} /></label>
    {#if alphaContent}
      <details class="adv" bind:open={advOpen}>
        <summary><span class="sum">Advanced</span>{#if !advOpen}<span class="muted small">· alpha threshold {auto.threshold}</span>{/if}</summary>
        <label class="field">
          <span>Alpha threshold (1–255): pixels at or above count as content</span>
          <span class="row tight">
            <input type="range" min="1" max="255" step="1" bind:value={app.ops.autocrop.threshold} disabled={!auto.enabled} aria-label="Auto-crop alpha threshold" />
            <NumField bind:value={app.ops.autocrop.threshold} min={1} max={255} small disabled={!auto.enabled} />
          </span>
        </label>
      </details>
    {/if}
  </div>
  <div class="row" class:off={auto.enabled}>
    <label class="field"><span>X</span><NumField bind:value={app.ops.crop.x} min={0} max={info.width - 1} small onchange={fix} disabled={auto.enabled} /></label>
    <label class="field"><span>Y</span><NumField bind:value={app.ops.crop.y} min={0} max={info.height - 1} small onchange={fix} disabled={auto.enabled} /></label>
    <label class="field"><span>Width</span><NumField bind:value={app.ops.crop.w} min={1} max={info.width} small onchange={onWidth} disabled={auto.enabled} /></label>
    <label class="field"><span>Height</span><NumField bind:value={app.ops.crop.h} min={1} max={info.height} small onchange={onHeight} disabled={auto.enabled} /></label>
    <label class="inline" title="Keep the current aspect ratio while drawing, resizing and editing W/H (X/Y and moves are unaffected)">
      <input type="checkbox" checked={lockedRatio > 0} oninput={onLockChange} onchange={onLockChange} disabled={auto.enabled} aria-label="Lock crop aspect ratio" />
      <span>{lockedRatio > 0 ? `Lock ratio (${ratioLabel(lockedRatio)})` : 'Lock ratio'}</span>
    </label>
    <button type="button" class="sm" onclick={centerSquare} disabled={auto.enabled} title="Largest centred square (handy for emotes)">Centre square</button>
    <button type="button" class="sm ghost" onclick={resetCrop} disabled={auto.enabled} title="Clear the crop — back to the full {info.width}×{info.height} frame" aria-label="Reset crop">Reset crop</button>
  </div>
  <p class="hint">
    {#if auto.enabled}
      The server detects the content box once per source (cropdetect over the trimmed clip) and the preview shows the
      result; the manual rectangle is ignored while auto-crop is on.
      {#if keyed}
        With the Background op on it crops to what is left after background removal; alpha ≥ threshold counts as content.
      {:else if info.hasAlpha}
        Alpha ≥ threshold counts as content (with a Background op on, after background removal).
      {:else}
        Non-black borders are trimmed; with a Background op on it crops to what is left after background removal (alpha ≥ threshold counts as content).
      {/if}
      Emotes render at ~22 px — cropping to content makes them readable.
    {:else}
      Drag on the preview to draw, drag inside to move, drag an edge or corner to resize. Coordinates are source
      pixels (before resize, flip and rotate). The preview shows the full frame while this card is open.
    {/if}
  </p>
</OpCard>

<style>
  .row.off :global(input),
  .row.off button {
    opacity: 0.55;
  }
  .row.tight {
    gap: 6px;
    flex-wrap: nowrap;
  }
  .field input[type='range'] {
    flex: 1;
    min-width: 100px;
  }
  .adv {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0 10px;
    flex: 1 1 100%;
  }
  .adv > summary {
    cursor: pointer;
    padding: 6px 0;
    font-size: 12.5px;
    user-select: none;
    display: flex;
    align-items: baseline;
    gap: 6px;
  }
  .adv > summary .sum {
    font-weight: 600;
  }
  .adv[open] > summary {
    border-bottom: 1px solid var(--border);
    margin-bottom: 8px;
  }
  .adv > .field {
    margin-bottom: 8px;
    width: 100%;
  }
  .adv > .field > span:first-child {
    white-space: normal;
  }
</style>
