<script lang="ts">
  import type { Source } from '../lib/api';
  import { fmtBytes, fmtNum, fmtSeconds } from '../lib/format';
  import { isSequence } from '../lib/state.svelte';

  interface Props {
    source: Source;
  }
  let { source }: Props = $props();

  const info = $derived(source.info);
  const seq = $derived(isSequence(info) ? (info.sequence ?? null) : null);
  const sequence = $derived(isSequence(info));
  const codec = $derived(
    [info.codec, info.profile].filter(Boolean).join(' ') + (info.format ? ` · ${info.format.split(',')[0]}` : ''),
  );
  const pix = $derived(info.pixFmt + (info.bits ? ` ${info.bits}-bit` : ''));
  const frameCount = $derived(seq?.count ?? info.frames);
</script>

<div class="probe card">
  <div class="name" title={source.name}>
    <span class="kind">{info.kind}</span>
    <b>{source.name}</b>
    <span class="muted small">{fmtBytes(source.size)}</span>
  </div>
  <div class="facts">
    <span class="badge" title="Dimensions"><b>{info.width}×{info.height}</b></span>
    {#if sequence}
      <span class="badge" title="Uploaded image sequence: frames are played at the sequence delay (change it in the Delay card)">
        <b>{frameCount}</b> frame{frameCount === 1 ? '' : 's'} · sequence
      </span>
      {#if seq}<span class="badge" title="Per-frame delay">{seq.delayMs} ms / frame</span>{/if}
      <span class="badge" title="Frame rate from the delay">{fmtNum(info.fps)} fps</span>
      <span class="badge" title="Duration">{fmtSeconds(info.duration)}</span>
    {:else if !info.isStill}
      <span class="badge" title="Nominal frame rate">{fmtNum(info.fps)} fps</span>
      <span class="badge" title="Frame count">{info.frames > 0 ? `${info.frames} frames` : 'frames ?'}</span>
      <span class="badge" title="Duration">{fmtSeconds(info.duration)}</span>
    {:else}
      <span class="badge">still image</span>
    {/if}
    <span class="badge mono" title="codec · container">{codec}</span>
    <span class="badge mono" title="pixel format">{pix}</span>
    <span class="badge" class:ok={info.hasAlpha} title="Whether any pixel is not fully opaque">
      alpha {info.hasAlpha ? 'yes' : 'no'}
    </span>
    {#if info.alphaStream}
      <span class="badge" title="Alpha comes from a separate video stream (merged before any other stage)">alpha stream #{info.alphaStream}</span>
    {/if}
    {#if info.hasAlpha}
      <span class="badge" class:warn={info.premultiplied} title="Best guess of how alpha is stored; toggle in the Alpha row below">
        {info.premultiplied ? 'premultiplied (guess)' : 'straight alpha (guess)'}
      </span>
    {/if}
    {#if info.hasAudio}<span class="badge muted" title="Audio is dropped by the image outputs">audio (dropped)</span>{/if}
  </div>
  {#if seq?.mixed}
    <p class="note info">
      Mixed sizes: the frames differ in size and are scaled / padded (transparent, centred) to {info.width}×{info.height}, the
      largest frame.
    </p>
  {/if}
</div>

<style>
  .probe {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px 14px;
  }
  .name {
    display: flex;
    align-items: baseline;
    gap: 8px;
    min-width: 0;
  }
  .name b {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
  .kind {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--accent);
    font-weight: 700;
    flex: none;
  }
  .facts {
    display: flex;
    flex-wrap: wrap;
    gap: 5px;
  }
</style>
