<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { fmtNum, GIF_MAX_FPS, gifDelays, MAX_FPS, snapFPS } from '../../lib/format';
  import { app, sourceFPS } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    info: ProbeInfo;
  }
  let { info }: Props = $props();

  let open = $state(false);
  const cfg = $derived(app.ops.fps);
  const format = $derived(app.output.format);
  const formatName = $derived(format === 'frames' ? 'Frame extraction' : format.toUpperCase());
  const snapped = $derived(snapFPS(format, cfg.fps));
  const capped = $derived(snapped > 0 && snapped < cfg.fps);
  const srcFps = $derived(sourceFPS(info, app.ops));
  const summary = $derived(cfg.enabled ? `${fmtNum(cfg.fps)} fps` : `source ${fmtNum(srcFps)} fps`);
  const quick = [10, 12.5, 15, 20, 24, 25, 30, 50];

  function set(v: number) {
    app.ops.fps.fps = v;
    app.ops.fps.enabled = true;
  }
</script>

<OpCard title="Frame rate" {summary} bind:enabled={app.ops.fps.enabled} bind:open>
  <div class="row">
    <label class="field"><span>fps</span><NumField bind:value={app.ops.fps.fps} min={1} max={60} step="any" small /></label>
    <div class="chips">
      {#each quick as q (q)}
        <button type="button" class="chip" aria-pressed={cfg.enabled && cfg.fps === q} onclick={() => set(q)}>{fmtNum(q)}</button>
      {/each}
    </div>
  </div>
  <p class="hint">
    Source: {fmtNum(srcFps)} fps. This op wins over the Output fps.
    {#if format === 'gif'}
      {#if capped}
        GIF delays are whole centiseconds (2 cs minimum), so {fmtNum(cfg.fps)} fps is capped at
        <b>{GIF_MAX_FPS} fps</b> ({gifDelays(snapped)} cs delays).
      {:else}
        {fmtNum(cfg.fps)} fps → <b>{gifDelays(snapped)} cs delays</b>, exact timing (GIF delays are whole
        centiseconds; the muxer alternates them so no frame is dropped or duplicated).
      {/if}
    {:else if capped}
      {formatName} is capped at <b>{MAX_FPS} fps</b>.
    {:else}
      {formatName} keeps {fmtNum(cfg.fps)} fps exactly (cap {MAX_FPS}).
    {/if}
    Lower fps is the cheapest way to shrink a file after lossy/dither.
  </p>
</OpCard>
