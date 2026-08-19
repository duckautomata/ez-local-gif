<script lang="ts">
  import { app } from '../../lib/state.svelte';
  import OpCard from '../OpCard.svelte';

  let open = $state(false);
  const cfg = $derived(app.ops.flipRotate);
  const summary = $derived.by(() => {
    if (!cfg.enabled) return 'off';
    const parts: string[] = [];
    if (cfg.horizontal) parts.push('flip H');
    if (cfg.vertical) parts.push('flip V');
    if (cfg.degrees) parts.push(`rotate ${cfg.degrees}°`);
    return parts.length ? parts.join(' · ') : 'no change';
  });
  const degrees: (0 | 90 | 180 | 270)[] = [0, 90, 180, 270];

  function setDeg(d: 0 | 90 | 180 | 270) {
    app.ops.flipRotate.degrees = d;
    app.ops.flipRotate.enabled = true;
  }
</script>

<OpCard title="Flip / Rotate" {summary} bind:enabled={app.ops.flipRotate.enabled} bind:open>
  <div class="row">
    <label class="inline"><input type="checkbox" bind:checked={app.ops.flipRotate.horizontal} /><span>Flip horizontal</span></label>
    <label class="inline"><input type="checkbox" bind:checked={app.ops.flipRotate.vertical} /><span>Flip vertical</span></label>
    <span class="field">
      <span>Rotate clockwise</span>
      <span class="seg" role="group" aria-label="Rotate">
        {#each degrees as d (d)}
          <button type="button" aria-pressed={cfg.degrees === d} onclick={() => setDeg(d)}>{d}°</button>
        {/each}
      </span>
    </span>
  </div>
</OpCard>
