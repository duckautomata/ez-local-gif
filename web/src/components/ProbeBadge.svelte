<script lang="ts">
  import type { Source } from '../lib/api';
  import { fmtBytes, fmtNum, fmtSeconds } from '../lib/format';

  interface Props {
    source: Source;
  }
  let { source }: Props = $props();

  const info = $derived(source.info);
  const codec = $derived(
    [info.codec, info.profile].filter(Boolean).join(' ') + (info.format ? ` · ${info.format.split(',')[0]}` : ''),
  );
  const pix = $derived(info.pixFmt + (info.bits ? ` ${info.bits}-bit` : ''));
</script>

<div class="probe card">
  <div class="name" title={source.name}>
    <span class="kind">{info.kind}</span>
    <b>{source.name}</b>
    <span class="muted small">{fmtBytes(source.size)}</span>
  </div>
  <div class="facts">
    <span class="badge" title="Dimensions"><b>{info.width}×{info.height}</b></span>
    {#if !info.isStill}
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
    {#if info.hasAlpha}
      <span class="badge" class:warn={info.premultiplied} title="Best guess of how alpha is stored; toggle in the Alpha row below">
        {info.premultiplied ? 'premultiplied (guess)' : 'straight alpha (guess)'}
      </span>
    {/if}
    {#if info.hasAudio}<span class="badge muted" title="Audio is dropped by GIF/WebP output">audio (dropped)</span>{/if}
  </div>
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
