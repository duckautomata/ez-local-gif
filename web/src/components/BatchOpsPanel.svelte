<script lang="ts">
  // Batch mode (Phase 4): only the geometry-independent global ops are
  // editable — fps, speed (+ reverse / bounce), background keying and
  // feather; unpremultiply is per row (auto from each probe). Trim / crop /
  // auto-crop / resize / flip / rotate / overlays are per-source and stay
  // out of every batch recipe (lib/batch batchOpsCfg) — "Open in editor" on
  // a row seeds the normal single view for those.
  import { phase4OpsOffered } from '../lib/capabilities.svelte';
  import { fmtNum } from '../lib/format';
  import { app } from '../lib/state.svelte';
  import NumField from './NumField.svelte';
  import OpCard from './OpCard.svelte';
  import BackgroundCard from './ops/BackgroundCard.svelte';
  import FeatherCard from './ops/FeatherCard.svelte';

  let fpsOpen = $state(false);
  let speedOpen = $state(false);

  const fps = $derived(app.ops.fps);
  const speed = $derived(app.ops.speed);
  const fpsSummary = $derived(fps.enabled ? `${fmtNum(fps.fps)} fps` : 'source fps');
  const speedSummary = $derived.by(() => {
    const parts: string[] = [];
    if (speed.enabled && speed.factor !== 1) parts.push(`${fmtNum(speed.factor)}×`);
    if (app.ops.reverse) parts.push('reversed');
    if (app.ops.bounce) parts.push('bounce');
    return parts.length ? parts.join(' · ') : '1×';
  });
  const fpsQuick = [10, 12.5, 15, 20, 24, 25, 30, 50];
  const speedQuick = [0.5, 0.75, 1, 1.5, 2, 3, 4];
  // An older server (pre-Phase-4) rejects the bounce op with a 400 (WEB-7);
  // the Feather card carries its own notice.
  const bounceSupported = $derived(phase4OpsOffered());

  // The header toggle of the Speed card covers factor, reverse and bounce.
  function getSpeedEnabled(): boolean {
    return speed.enabled || app.ops.reverse || app.ops.bounce;
  }
  function setSpeedEnabled(v: boolean) {
    if (v) app.ops.speed.enabled = true;
    else {
      app.ops.speed.enabled = false;
      app.ops.reverse = false;
      app.ops.bounce = false;
    }
  }
</script>

<div class="ops">
  <div class="card note-card">
    <p>
      <b>Batch:</b> these ops apply to <b>every</b> file. Per-source ops (trim, crop, auto-crop, resize, flip/rotate, overlays,
      text) are disabled — use <i>Open in editor</i> on a row for those. “Source alpha is premultiplied” sits on each row.
    </p>
  </div>

  <OpCard title="Frame rate" summary={fpsSummary} bind:enabled={app.ops.fps.enabled} bind:open={fpsOpen}>
    <div class="row">
      <label class="field"><span>fps</span><NumField bind:value={app.ops.fps.fps} min={1} max={60} step="any" small /></label>
      <div class="chips">
        {#each fpsQuick as q (q)}
          <button
            type="button"
            class="chip"
            aria-pressed={fps.enabled && fps.fps === q}
            onclick={() => {
              app.ops.fps.fps = q;
              app.ops.fps.enabled = true;
            }}
          >
            {fmtNum(q)}
          </button>
        {/each}
      </div>
    </div>
    <p class="hint">Applied to every file; wins over the Output fps. GIF caps at 50, the other formats at 60.</p>
  </OpCard>

  <OpCard title="Speed" summary={speedSummary} bind:enabled={getSpeedEnabled, setSpeedEnabled} bind:open={speedOpen}>
    <div class="row">
      <label class="field"><span>Factor (2 = twice as fast)</span><NumField bind:value={app.ops.speed.factor} min={0.05} max={20} step="any" small /></label>
      <div class="chips">
        {#each speedQuick as q (q)}
          <button
            type="button"
            class="chip"
            aria-pressed={speed.enabled && speed.factor === q}
            onclick={() => {
              app.ops.speed.factor = q;
              app.ops.speed.enabled = true;
            }}
          >
            {fmtNum(q)}×
          </button>
        {/each}
      </div>
    </div>
    <div class="row">
      <label class="inline" title="Play each clip backwards">
        <input type="checkbox" bind:checked={app.ops.reverse} /><span>Reverse</span>
      </label>
      <label class="inline" title="Play forward, then backward (ping-pong): every clip's frames and duration double">
        <input type="checkbox" bind:checked={app.ops.bounce} /><span>Bounce — forward then back, doubles the length</span>
      </label>
    </div>
    {#if !bounceSupported}
      <p class="note">This server does not support Bounce (an older ezlg) — the bounce op will be rejected at render; update the server.</p>
    {/if}
  </OpCard>

  <BackgroundCard />
  <FeatherCard />
  <p class="hint eyedrop">In batch there is no preview to pick a colour from — type the hex value in the Background card instead.</p>
</div>

<style>
  .ops {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .note-card {
    padding: 10px 14px;
  }
  .note-card p {
    margin: 0;
  }
  .eyedrop {
    padding: 0 4px;
  }
</style>
