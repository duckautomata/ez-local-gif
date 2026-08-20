<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { sequenceFps } from '../../lib/files';
  import { fmtNum, fmtSeconds } from '../../lib/format';
  import { app, sourceDuration } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  // Per-frame timing of an image-sequence source (op "delay"). The upload
  // carried a default (SequenceInfo.delayMs); this op overrides it per recipe.
  interface Props {
    info: ProbeInfo;
  }
  let { info }: Props = $props();

  let open = $state(false);
  const cfg = $derived(app.ops.delay);
  const uploaded = $derived(info.sequence?.delayMs ?? 0);
  const count = $derived(info.sequence?.count ?? info.frames);
  const fps = $derived(sequenceFps(cfg.ms));
  const summary = $derived(
    cfg.enabled ? `${fmtNum(cfg.ms, 0)} ms / frame (${fmtNum(fps)} fps)` : `as uploaded: ${uploaded} ms / frame (${fmtNum(info.fps)} fps)`,
  );
  const quick = [
    { ms: 20, label: '20 ms (50 fps)' },
    { ms: 33, label: '33 ms (30 fps)' },
    { ms: 40, label: '40 ms (25 fps)' },
    { ms: 50, label: '50 ms (20 fps)' },
    { ms: 67, label: '67 ms (15 fps)' },
    { ms: 100, label: '100 ms (10 fps)' },
    { ms: 200, label: '200 ms (5 fps)' },
  ];

  function set(ms: number) {
    app.ops.delay.ms = ms;
    app.ops.delay.enabled = true;
  }
</script>

<OpCard title="Delay (sequence timing)" {summary} bind:enabled={app.ops.delay.enabled} bind:open>
  <div class="row">
    <label class="field"><span>ms per frame</span><NumField bind:value={app.ops.delay.ms} min={1} max={60000} small /></label>
    <span class="hint">= <b>{fmtNum(fps)} fps</b> · {count} frames → {fmtSeconds(sourceDuration(info, app.ops))}</span>
    <div class="chips">
      {#each quick as q (q.ms)}
        <button type="button" class="chip" aria-pressed={cfg.enabled && cfg.ms === q.ms} onclick={() => set(q.ms)}>{q.label}</button>
      {/each}
    </div>
  </div>
  <p class="hint">
    Every frame of the sequence is shown for this long (the upload used {uploaded} ms). GIF delays are whole centiseconds
    and browsers clamp delays ≤ 10 ms to 100 ms, so 20 ms is the floor for GIF; WebP / APNG delays should be ≥ 20 ms too.
  </p>
</OpCard>
