<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { fmtNum, fmtSeconds } from '../../lib/format';
  import { app, trimRange } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    info: ProbeInfo;
  }
  let { info }: Props = $props();

  let open = $state(false);
  const cfg = $derived(app.ops.speed);
  const reverse = $derived(app.ops.reverse);
  const range = $derived(trimRange(info, app.ops));
  const inDur = $derived(range.end - range.start);
  const factor = $derived(cfg.enabled && cfg.factor > 0 ? cfg.factor : 1);
  const summary = $derived.by(() => {
    const parts: string[] = [];
    if (cfg.enabled && cfg.factor !== 1) parts.push(`${fmtNum(cfg.factor)}× → ${fmtSeconds(inDur / factor)}`);
    if (reverse) parts.push('reversed');
    return parts.length ? parts.join(' · ') : '1×';
  });
  const quick = [0.5, 0.75, 1, 1.5, 2, 3, 4];

  // The header toggle covers the factor and the reverse switch.
  function getEnabled(): boolean {
    return cfg.enabled || reverse;
  }
  function setEnabled(v: boolean) {
    if (v) app.ops.speed.enabled = true;
    else {
      app.ops.speed.enabled = false;
      app.ops.reverse = false;
    }
  }

  function set(v: number) {
    app.ops.speed.factor = v;
    app.ops.speed.enabled = true;
  }
</script>

<OpCard title="Speed" {summary} bind:enabled={getEnabled, setEnabled} bind:open>
  <div class="row">
    <label class="field"><span>Factor (2 = twice as fast)</span><NumField bind:value={app.ops.speed.factor} min={0.05} max={20} step="any" small /></label>
    <div class="chips">
      {#each quick as q (q)}
        <button type="button" class="chip" aria-pressed={cfg.enabled && cfg.factor === q} onclick={() => set(q)}>{fmtNum(q)}×</button>
      {/each}
    </div>
    <span class="hint">{fmtSeconds(inDur)} → <b>{fmtSeconds(inDur / factor)}</b></span>
  </div>
  <div class="row">
    <label class="inline" title="Play the clip backwards (after trim, speed and the geometry ops)">
      <input type="checkbox" bind:checked={app.ops.reverse} /><span>Reverse</span>
    </label>
  </div>
  <p class="hint">Applied after trim. Stickers must be ≤ 5 s — speeding up is one way to fit. Reverse plays the trimmed range backwards.</p>
</OpCard>
