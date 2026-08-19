<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { clamp } from '../../lib/format';
  import { app } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    info: ProbeInfo;
  }
  let { info }: Props = $props();

  const crop = $derived(app.ops.crop);
  const summary = $derived(
    crop.enabled ? `${crop.w}×${crop.h} at ${crop.x},${crop.y}` : `full frame ${info.width}×${info.height}`,
  );

  // While the card is open the preview shows the full pre-crop frame with
  // the drag overlay (see Preview.svelte / CropOverlay.svelte).
  let open = $state(false);
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
  function fullFrame() {
    app.ops.crop = { enabled: false, x: 0, y: 0, w: info.width, h: info.height };
  }
  function centerSquare() {
    const s = Math.min(info.width, info.height);
    app.ops.crop = {
      enabled: true,
      x: Math.floor((info.width - s) / 2),
      y: Math.floor((info.height - s) / 2),
      w: s,
      h: s,
    };
  }
</script>

<OpCard title="Crop" {summary} bind:enabled={app.ops.crop.enabled} bind:open>
  <div class="row">
    <label class="field"><span>X</span><NumField bind:value={app.ops.crop.x} min={0} max={info.width - 1} small onchange={fix} /></label>
    <label class="field"><span>Y</span><NumField bind:value={app.ops.crop.y} min={0} max={info.height - 1} small onchange={fix} /></label>
    <label class="field"><span>Width</span><NumField bind:value={app.ops.crop.w} min={1} max={info.width} small onchange={fix} /></label>
    <label class="field"><span>Height</span><NumField bind:value={app.ops.crop.h} min={1} max={info.height} small onchange={fix} /></label>
    <button type="button" class="sm" onclick={centerSquare} title="Largest centred square (handy for emotes)">Centre square</button>
    <button type="button" class="sm ghost" onclick={fullFrame}>Full frame</button>
  </div>
  <p class="hint">
    Drag on the preview to draw the rectangle, drag inside it to move. Coordinates are source pixels (before resize,
    flip and rotate). The preview shows the full frame while this card is open.
  </p>
</OpCard>
