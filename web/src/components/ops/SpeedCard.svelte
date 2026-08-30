<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { phase4OpsOffered } from '../../lib/capabilities.svelte';
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
  const bounce = $derived(app.ops.bounce);
  const range = $derived(trimRange(info, app.ops));
  const inDur = $derived(range.end - range.start);
  const factor = $derived(cfg.enabled && cfg.factor > 0 ? cfg.factor : 1);
  const outDur = $derived((inDur / factor) * (bounce ? 2 : 1));
  const summary = $derived.by(() => {
    const parts: string[] = [];
    if (cfg.enabled && cfg.factor !== 1) parts.push(`${fmtNum(cfg.factor)}× → ${fmtSeconds(outDur)}`);
    if (reverse) parts.push('reversed');
    if (bounce) parts.push(`bounce (${fmtSeconds(outDur)})`);
    return parts.length ? parts.join(' · ') : '1×';
  });
  const quick = [0.5, 0.75, 1, 1.5, 2, 3, 4];
  // An older server (pre-Phase-4) rejects the bounce op with a 400: the
  // checkbox stays, with a notice — like the Background / Overlays gates (WEB-7).
  const bounceSupported = $derived(phase4OpsOffered());

  // The header toggle covers the factor, the reverse and the bounce switch.
  function getEnabled(): boolean {
    return cfg.enabled || reverse || bounce;
  }
  function setEnabled(v: boolean) {
    if (v) app.ops.speed.enabled = true;
    else {
      app.ops.speed.enabled = false;
      app.ops.reverse = false;
      app.ops.bounce = false;
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
    <label class="inline" title="Play forward, then backward (ping-pong): the clip's frames and duration double">
      <input type="checkbox" bind:checked={app.ops.bounce} /><span>Bounce — forward then back, doubles the length</span>
    </label>
  </div>
  {#if !bounceSupported}
    <p class="note">This server does not support Bounce (an older ezlg) — the bounce op will be rejected at render; update the server.</p>
  {/if}
  <p class="hint">
    Applied after trim. Stickers must be ≤ 5 s — speeding up is one way to fit. Reverse plays the trimmed range backwards; Bounce
    appends the reversed copy (with Reverse on: backwards first, then forwards).
  </p>
</OpCard>
