<script lang="ts">
  // The Overlays section: any number of text / image cards, in drawing order
  // (the last one draws on top), each collapsible, reorderable and removable.
  import type { ProbeInfo } from '../../lib/api';
  import { caps } from '../../lib/capabilities.svelte';
  import { addOverlay, app } from '../../lib/state.svelte';
  import ImageOverlayCard from './ImageOverlayCard.svelte';
  import TextOverlayCard from './TextOverlayCard.svelte';

  interface Props {
    info: ProbeInfo;
    /** expand every card (tests render the bodies server-side) */
    initialOpen?: boolean;
  }
  let { info, initialOpen = false }: Props = $props();

  const list = $derived(app.ops.overlays);
  // An older server (features.overlays off) rejects text / overlay ops: the
  // section stays usable so a recipe can be prepared, with a notice.
  const supported = $derived(caps.features.overlays);
</script>

<section class="overlays">
  <div class="row head">
    <span class="title">Overlays</span>
    <button type="button" class="sm" onclick={() => addOverlay('text')}>+ Add text</button>
    <button type="button" class="sm" onclick={() => addOverlay('image')}>+ Add image</button>
    {#if list.length === 0}
      <span class="hint">Text (drawtext) or an image / animated image / video on top of the output; drag to place on the preview.</span>
    {:else}
      <span class="hint">Drawn in this order — the last card is on top. Drag the boxes on the preview to place them.</span>
    {/if}
  </div>
  {#if !supported}
    <p class="note">This server does not support overlays (an older ezlg) — text and image overlays will be rejected at render; update the server.</p>
  {/if}
  {#each list as o, i (o.id)}
    {#if o.kind === 'text'}
      <TextOverlayCard {o} index={i} count={list.length} {info} {initialOpen} />
    {:else}
      <ImageOverlayCard {o} index={i} count={list.length} {info} {initialOpen} />
    {/if}
  {/each}
</section>

<style>
  .overlays {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .head {
    padding: 4px 2px 0;
    gap: 8px;
  }
  .title {
    font-size: 13px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
  }
</style>
