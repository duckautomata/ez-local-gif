<script lang="ts">
  import type { FitMode, ProbeInfo } from '../../lib/api';
  import { fitSize } from '../../lib/format';
  import { app } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    info: ProbeInfo;
  }
  let { info }: Props = $props();

  let open = $state(false);
  const rs = $derived(app.ops.resize);
  // Frame size entering the resize op (after crop).
  const inW = $derived(app.ops.crop.enabled ? app.ops.crop.w : info.width);
  const inH = $derived(app.ops.crop.enabled ? app.ops.crop.h : info.height);
  const out = $derived(fitSize(inW, inH, rs.width, rs.height, rs.fit));
  const summary = $derived(rs.enabled && (rs.width > 0 || rs.height > 0) ? `${out.w}×${out.h} (${rs.fit})` : 'off');

  const fits: { id: FitMode; label: string; title: string }[] = [
    { id: 'contain', label: 'contain', title: 'Largest size that fits inside W×H, keeping aspect' },
    { id: 'cover', label: 'cover', title: 'Smallest size that covers W×H, keeping aspect, then centre-crop' },
    { id: 'exact', label: 'exact', title: 'Stretch to exactly W×H' },
  ];
</script>

<OpCard title="Resize" {summary} bind:enabled={app.ops.resize.enabled} bind:open>
  <div class="row">
    <label class="field"><span>Width (0 = auto)</span><NumField bind:value={app.ops.resize.width} min={0} max={8192} /></label>
    <label class="field"><span>Height (0 = auto)</span><NumField bind:value={app.ops.resize.height} min={0} max={8192} /></label>
    <label class="field">
      <span>Fit</span>
      <select bind:value={app.ops.resize.fit}>
        {#each fits as f (f.id)}<option value={f.id} title={f.title}>{f.label}</option>{/each}
      </select>
    </label>
    <span class="hint">{inW}×{inH} → <b>{out.w}×{out.h}</b></span>
  </div>
  <p class="hint">
    Lanczos scaling in premultiplied space (no dark fringes). The Output card's canvas size is applied afterwards; use
    this only when you need a specific intermediate size.
  </p>
</OpCard>
