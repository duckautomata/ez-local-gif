<script lang="ts">
  import type { ProbeInfo } from '../../lib/api';
  import { clamp, fmtSeconds } from '../../lib/format';
  import {
    app,
    effectiveOps,
    frameWindow,
    planFPS,
    planFrames,
    sourceDuration,
    sourceFPS,
    sourceFrames,
    sourceSpan,
    trimRange,
    trimStartMax,
  } from '../../lib/state.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';

  interface Props {
    info: ProbeInfo;
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
  }
  let { info, initialOpen = false }: Props = $props();

  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  const trim = $derived(app.ops.trim);
  const range = $derived(trimRange(info, app.ops));
  // The effective source length: for an image sequence with the Delay op on,
  // the op rewrites the timing, so the probe's duration would be wrong here
  // (same helper the scrubber's range uses via trimRange).
  const dur = $derived(sourceDuration(info, app.ops));
  // The selection as frames on the *source* grid (a sequence's frame count,
  // else duration × source fps) — the numbers the user counted frames in.
  // For a sequence the span is exactly what the graph's trim selects on the
  // image2 grid (nearest frame, as ffmpeg reads it); for a clip the frames
  // the µs-precise window covers.
  const srcFps = $derived(sourceFPS(info, app.ops));
  const srcFrames = $derived(sourceFrames(info, app.ops));
  const span = $derived(sourceSpan(info, app.ops));
  const framesText = $derived(
    srcFps > 0 ? `${span.first === span.last ? `frame ${span.first}` : `frames ${span.first}–${span.last}`}${srcFrames > 0 ? ` of ${srcFrames}` : ''}` : '',
  );
  // The latest start the graph accepts: one source frame before the end.
  const startMax = $derived(trimStartMax(info, app.ops));
  $effect(() => {
    // a typed / stale start past the end (e.g. after the Delay op shortened
    // the clip) would make the graph reject the recipe
    if (app.ops.trim.start > startMax) app.ops.trim.start = startMax;
  });
  const summary = $derived(
    trim.enabled
      ? `${fmtSeconds(range.start)} → ${trim.end > 0 ? fmtSeconds(range.end) : 'end'} (${fmtSeconds(range.end - range.start)}${framesText ? `, ${framesText}` : ''})`
      : `whole clip (${fmtSeconds(dur)}${srcFrames > 0 ? `, ${srcFrames} frames` : ''})`,
  );

  // The scrubber: 0-based frame index on the plan grid (output time after
  // trim and speed); frameWindow maps it back to a source-time window.
  const ops = $derived(effectiveOps(app.ops, app.output));
  const fps = $derived(planFPS(info, ops, app.output));
  const total = $derived(planFrames(info, ops, app.output));
  const i = $derived(clamp(Math.round(app.ui.scrubFrame) || 0, 0, Math.max(0, total - 1)));
  const win = $derived(frameWindow(info, app.ops, app.output, i));
  const atLast = $derived(total > 0 && i >= total - 1);

  /** setStart: the start of the frame under the scrubber becomes the trim start. */
  function setStart() {
    if (!(fps > 0)) return;
    const s = win.start;
    app.ops.trim.start = s;
    if (app.ops.trim.end > 0 && app.ops.trim.end <= s) app.ops.trim.end = 0;
    app.ops.trim.enabled = true;
    app.ui.scrubFrame = 0;
  }
  /** setEnd: the end of the frame under the scrubber becomes the trim end (the last frame = to the end). */
  function setEnd() {
    if (!(fps > 0)) return;
    const e = win.end;
    if (e > 0 && e <= app.ops.trim.start) return;
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
      <NumField bind:value={app.ops.trim.start} min={0} max={startMax} step="any" />
    </label>
    <button
      type="button"
      class="sm"
      onclick={setStart}
      disabled={!(fps > 0)}
      title="Start at the frame under the scrubber (frame {i + 1}, {fmtSeconds(win.start)} of the source)"
    >
      ◂ from scrubber
    </button>
    <label class="field">
      <span>End (s, 0 = end)</span>
      <NumField bind:value={app.ops.trim.end} min={0} max={dur} step="any" />
    </label>
    <button
      type="button"
      class="sm"
      onclick={setEnd}
      disabled={!(fps > 0)}
      title={atLast ? 'End after the last frame (to the end)' : `End after the frame under the scrubber (frame ${i + 1}, ${fmtSeconds(win.end)} of the source)`}
    >
      ◂ from scrubber
    </button>
    <button type="button" class="sm ghost" onclick={reset}>Reset</button>
  </div>
  {#if trim.enabled && trim.end > 0 && trim.end <= trim.start}
    <p class="hint warn">End must be after start (0 = play to the end).</p>
  {/if}
  <p class="hint">
    Selected: {fmtSeconds(range.start)} → {fmtSeconds(range.end)} = {fmtSeconds(range.end - range.start)}{#if framesText}&nbsp;·
      {`source ${framesText}`}{/if}. The scrubber runs over the trimmed range; “from scrubber” takes the frame under it — its
    start for Start, the point after it for End (on the last frame: to the end).
  </p>
</OpCard>
