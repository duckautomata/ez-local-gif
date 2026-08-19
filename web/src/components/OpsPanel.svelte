<script lang="ts">
  import type { ProbeInfo } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import CropCard from './ops/CropCard.svelte';
  import FlipRotateCard from './ops/FlipRotateCard.svelte';
  import FpsCard from './ops/FpsCard.svelte';
  import ResizeCard from './ops/ResizeCard.svelte';
  import SpeedCard from './ops/SpeedCard.svelte';
  import TrimCard from './ops/TrimCard.svelte';

  interface Props {
    info: ProbeInfo;
  }
  let { info }: Props = $props();

  const still = $derived(info.isStill || info.duration <= 0);
</script>

<div class="ops">
  <div class="card alpha">
    <label class="inline">
      <input type="checkbox" bind:checked={app.ops.unpremultiply} />
      <span><b>Source alpha is premultiplied</b> — unpremultiply right after decode</span>
    </label>
    <p class="hint">
      {#if !info.hasAlpha}
        The source has no alpha, so this makes no difference.
      {:else if info.premultiplied}
        Detected/guessed premultiplied (DaVinci Resolve ProRes 4444 exports are). Leave on: rendered without
        unpremultiply, a premultiplied source gets a dark halo on the white backdrop. Turn off only if the
        semi-transparent edges come out light / over-bright on the dark backdrop — then the source was straight
        after all.
      {:else}
        Guessed straight alpha. Leave off: unpremultiplying a straight source makes semi-transparent edges light /
        over-bright on the dark backdrop. Turn on only if the edges get a dark halo on the white backdrop — then the
        source is premultiplied after all.
      {/if}
    </p>
  </div>

  {#if !still}<TrimCard {info} />{/if}
  <CropCard {info} />
  <ResizeCard {info} />
  {#if !still}<FpsCard {info} />{/if}
  {#if !still}<SpeedCard {info} />{/if}
  <FlipRotateCard />
</div>

<style>
  .ops {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .alpha {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 10px 14px;
  }
</style>
