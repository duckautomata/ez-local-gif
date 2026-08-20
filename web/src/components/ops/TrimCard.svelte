<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { fmtSeconds, round } from '../../lib/format';
  import { app, sourceDuration, toSourceTime, trimRange } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    info: ProbeInfo;
  }
  let { info }: Props = $props();

  let open = $state(false);
  const trim = $derived(app.ops.trim);
  const range = $derived(trimRange(info, app.ops));
  // The effective source length: for an image sequence with the Delay op on,
  // the op rewrites the timing, so the probe's duration would be wrong here
  // (same helper the scrubber's range uses via trimRange).
  const dur = $derived(sourceDuration(info, app.ops));
  const summary = $derived(
    trim.enabled
      ? `${fmtSeconds(range.start)} → ${trim.end > 0 ? fmtSeconds(range.end) : 'end'} (${fmtSeconds(range.end - range.start)})`
      : `whole clip (${fmtSeconds(dur)})`,
  );

  /** current scrubber position expressed in source seconds */
  function scrubSource(): number {
    return round(toSourceTime(app.ui.scrubT, info, app.ops), 2);
  }
  function setStart() {
    const s = scrubSource();
    app.ops.trim.start = s;
    if (app.ops.trim.end > 0 && app.ops.trim.end <= s) app.ops.trim.end = 0;
    app.ops.trim.enabled = true;
    app.ui.scrubT = 0;
  }
  function setEnd() {
    const e = scrubSource();
    if (e <= app.ops.trim.start) return;
    app.ops.trim.end = e;
    app.ops.trim.enabled = true;
  }
  function reset() {
    app.ops.trim = { enabled: false, start: 0, end: 0 };
  }
</script>

<OpCard title="Trim" {summary} bind:enabled={app.ops.trim.enabled} bind:open>
  <div class="row">
    <label class="field">
      <span>Start (s)</span>
      <NumField bind:value={app.ops.trim.start} min={0} max={dur} step="any" />
    </label>
    <button type="button" class="sm" onclick={setStart} title="Use the scrubber position as the start">◂ from scrubber</button>
    <label class="field">
      <span>End (s, 0 = end)</span>
      <NumField bind:value={app.ops.trim.end} min={0} max={dur} step="any" />
    </label>
    <button type="button" class="sm" onclick={setEnd} title="Use the scrubber position as the end">◂ from scrubber</button>
    <button type="button" class="sm ghost" onclick={reset}>Reset</button>
  </div>
  {#if trim.enabled && trim.end > 0 && trim.end <= trim.start}
    <p class="hint warn">End must be after start (0 = play to the end).</p>
  {/if}
  <p class="hint">
    Selected: {fmtSeconds(range.start)} → {fmtSeconds(range.end)} = {fmtSeconds(range.end - range.start)}. The scrubber
    runs over the trimmed range; “from scrubber” converts it back to source time.
  </p>
</OpCard>
