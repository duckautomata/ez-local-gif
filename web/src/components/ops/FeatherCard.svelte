<script lang="ts">
  // Feather (review R4): Gaussian blur of the alpha plane — a soft
  // transparency edge. The op is emitted right after the Background (keying)
  // op and before the geometry (buildOps / the graph's hoisting), so the
  // radius is in source pixels and scales down with the output.
  import { phase4OpsOffered } from '../../lib/capabilities.svelte';
  import { fmtNum } from '../../lib/format';
  import { app } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
  }
  let { initialOpen = false }: Props = $props();

  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  const cfg = $derived(app.ops.feather);
  // An older server (pre-Phase-4) rejects the feather op with a 400: the
  // card stays, with a notice — like the Background / Overlays gates (WEB-7).
  const supported = $derived(phase4OpsOffered());
  const summary = $derived(cfg.enabled ? `${fmtNum(cfg.radius)} px soft edge` : supported ? 'off' : 'off — not supported by this server');
</script>

<OpCard title="Feather" {summary} bind:enabled={app.ops.feather.enabled} bind:open>
  {#if !supported}
    <p class="note">This server does not support feathering (an older ezlg) — the feather op will be rejected at render; update the server.</p>
  {/if}
  <label class="field">
    <span>Feather — {fmtNum(cfg.radius)} px (soft edge ≈ 2–3×{fmtNum(cfg.radius)})</span>
    <span class="row tight">
      <input type="range" min="0.5" max="20" step="0.5" bind:value={app.ops.feather.radius} aria-label="Feather radius" />
      <NumField bind:value={app.ops.feather.radius} min={0.5} max={20} step={0.5} small />
    </span>
  </label>
  <p class="hint">
    Softens the transparency edge (a Gaussian blur of the alpha) — most useful after Background removal, or on the
    rough 1-bit alpha of a GIF source. GIF output still thresholds back to 1-bit alpha (the matte colour decides);
    WebP, APNG and AVIF keep the soft edge. The radius is in source pixels, so it scales down with the output — a 3 px
    feather on a 720 px source is ~0.5 px after the emote fit.
  </p>
</OpCard>

<style>
  .row.tight {
    gap: 6px;
    flex-wrap: nowrap;
  }
  .field input[type='range'] {
    flex: 1;
    min-width: 100px;
  }
</style>
