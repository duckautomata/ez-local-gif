<script lang="ts">
  // Start / end of an overlay in output seconds (after trim and speed). Like
  // Trim, Start is inclusive and End exclusive — the frame that starts at
  // End is not drawn (overlay.activeAt) — so "from scrubber" takes the frame
  // under the scrubber: its start for Start, the point after it for End (the
  // frame under the scrubber is then the last one drawn; the last frame = to
  // the end, 0). Unlike Trim's bounds, these are floored to whole µs, not
  // rounded to the nearest (overlay.scrubberRange): the render compares them
  // against ffmpeg's double frame time with gte/lt, and a bound rounded up
  // past the frame it names would start / end the overlay one frame late.
  // "Whole clip" clears both. When the overlay is not shown at the scrubber
  // time, "go" jumps the scrubber to its start.
  import type { ProbeInfo } from '../../lib/api';
  import { clamp, fmtSeconds, frameAt, frameStart } from '../../lib/format';
  import { activeAt, scrubberEnd, scrubberRange, scrubberStart } from '../../lib/overlay';
  import { app, effectiveOps, planFPS, planFrames, previewDuration, type OverlayCfg } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';

  interface Props {
    o: OverlayCfg;
    info: ProbeInfo;
  }
  let { o, info }: Props = $props();

  const ops = $derived(effectiveOps(app.ops, app.output));
  const fps = $derived(planFPS(info, ops, app.output));
  const total = $derived(planFrames(info, ops, app.output));
  const duration = $derived(previewDuration(info, ops));
  const i = $derived(clamp(Math.round(app.ui.scrubFrame) || 0, 0, Math.max(0, total - 1)));
  const atLast = $derived(total > 0 && i >= total - 1);
  const canStep = $derived(fps > 0 && total > 1);
  const now = $derived(frameStart(i, fps));
  const visible = $derived(activeAt(o, now));
  const whole = $derived(!(o.start > 0) && !(o.end > 0));
  const rangeText = $derived(whole ? 'whole clip' : `${fmtSeconds(Math.max(0, o.start))} → ${o.end > 0 ? fmtSeconds(o.end) : 'end'}`);
  // what "from scrubber" stores for the frame under the scrubber (the tooltips)
  const win = $derived(scrubberRange(i, fps, total));

  function setStart() {
    if (canStep) scrubberStart(o, i, fps, total);
  }
  function setEnd() {
    if (canStep) scrubberEnd(o, i, fps, total);
  }
  function wholeClip() {
    o.start = 0;
    o.end = 0;
  }
  function goToStart() {
    if (!canStep) return;
    app.ui.scrubFrame = frameAt(o.start, fps, total) - 1;
  }
</script>

<div class="row range">
  <span class="field">
    <span>Shown</span>
    <span class="row tight">
      <button type="button" class="sm" aria-pressed={whole} onclick={wholeClip} title="No time range — shown on every frame">Whole clip</button>
      <span class="hint">{rangeText}</span>
    </span>
  </span>
  <label class="field">
    <span>Start (s)</span>
    <NumField bind:value={o.start} min={0} max={Math.max(0, duration)} step="any" small />
  </label>
  <button type="button" class="sm" onclick={setStart} disabled={!canStep} aria-label="Start from scrubber" title="Start at the frame under the scrubber (frame {i + 1}, {fmtSeconds(now)})">◂ from scrubber</button>
  <label class="field">
    <span>End (s, exclusive; 0 = end)</span>
    <NumField bind:value={o.end} min={0} max={Math.max(0, duration)} step="any" small />
  </label>
  <button
    type="button"
    class="sm"
    onclick={setEnd}
    disabled={!canStep}
    aria-label="End from scrubber"
    title={atLast
      ? 'End after the last frame (to the end)'
      : `End after the frame under the scrubber (frame ${i + 1}, ${fmtSeconds(win.end)}) — that frame is the last one drawn`}
  >
    ◂ from scrubber
  </button>
  {#if o.end > 0 && o.end <= o.start}
    <span class="hint warn">End must be after start (0 = to the end).</span>
  {:else if !visible}
    <span class="hint warn">
      not shown at the scrubber's frame
      <button type="button" class="sm" onclick={goToStart} disabled={!canStep} title="Move the scrubber to the start of this overlay">▸ go to start</button>
    </span>
  {/if}
  <span class="hint range-note">
    Output seconds (after trim and speed). As with Trim, Start is inclusive and End exclusive: the frame at End is not drawn.
  </span>
</div>

<style>
  .row.tight {
    gap: 6px;
    flex-wrap: nowrap;
  }
  .hint.warn {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .range-note {
    flex: 1 1 100%;
  }
</style>
