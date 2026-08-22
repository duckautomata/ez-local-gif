<script lang="ts">
  import type { ProbeInfo } from '../lib/api';
  import { presetById } from '../lib/presets';
  import { app, applyPreset, isSequence, opsApply } from '../lib/state.svelte';
  import CropCard from './ops/CropCard.svelte';
  import DelayCard from './ops/DelayCard.svelte';
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
  const sequence = $derived(isSequence(info));
  const active = $derived(opsApply(app.output));
  const preset = $derived(presetById(app.output.preset));
</script>

<div class="ops">
  {#if !active}
    <div class="card note-card">
      <p>
        <b>{preset.label}</b> works on the GIF file directly with gifsicle — no decode, no re-quantisation — so the edit ops
        below are not applied (lossy, colours, dither and frame drop live in the Output card).
      </p>
      <p class="hint">Need to trim, crop or resize? <button type="button" class="sm" onclick={() => applyPreset('chat')}>Switch to Chat</button> (re-encodes).</p>
    </div>
  {:else}
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

    {#if sequence}<DelayCard {info} />{/if}
    {#if !still}<TrimCard {info} />{/if}
    <CropCard {info} />
    <ResizeCard {info} />
    {#if !still}<FpsCard {info} />{/if}
    {#if !still}<SpeedCard {info} />{/if}
    <FlipRotateCard />
  {/if}
</div>

<style>
  .ops {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .alpha,
  .note-card {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 10px 14px;
  }
  .note-card .hint {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  /* long toggle text must wrap on narrow screens */
  .alpha label.inline {
    align-items: flex-start;
  }
  .alpha label.inline > span {
    white-space: normal;
  }
</style>
