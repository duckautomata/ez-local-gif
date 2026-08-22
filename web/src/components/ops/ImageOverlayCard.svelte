<script lang="ts">
  // One image overlay (op "overlay"): an uploaded asset — still image,
  // animated image or video, with alpha when it has one — composited onto
  // the output canvas. The asset is an ordinary upload; the recipe lists its
  // hash in `sources` and the op references it by index (state.assetHashes).
  import { untrack } from 'svelte';
  import { isAbortError, messageOf, upload, type Anchor, type ProbeInfo, type UploadHandle } from '../../lib/api';
  import { fmtBytes, fmtNum, fmtSeconds } from '../../lib/format';
  import { anchorPoint, imageBoxSize, isAnimatedAsset, overlayBox, overlayLabel, selectionAfterToggle } from '../../lib/overlay';
  import { app, moveOverlay, removeOverlay, type ImageOverlayCfg } from '../../lib/state.svelte';
  import { toast } from '../../lib/toast.svelte';
  import AnchorGrid from '../AnchorGrid.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';
  import TimeRangeFields from './TimeRangeFields.svelte';

  const ACCEPT = '.gif,.webp,.apng,.png,.avif,.jpg,.jpeg,.mp4,.mkv,.mov,.webm,image/*,video/*';

  interface Props {
    o: ImageOverlayCfg;
    index: number;
    count: number;
    info: ProbeInfo;
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
  }
  let { o, index, count, info, initialOpen = false }: Props = $props();

  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  // The expanded card's box is highlighted on the preview. Only `open` is
  // tracked: the selection is mirrored on open / collapse transitions
  // (overlay.selectionAfterToggle), never re-applied when the selection
  // itself changes — a collapsed card used to read selectedOverlay here and
  // reset what addOverlay or a box click on the preview had just selected.
  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let wasOpen = initialOpen;
  $effect(() => {
    const now = open;
    untrack(() => {
      app.ui.selectedOverlay = selectionAfterToggle(app.ui.selectedOverlay, o.id, now, wasOpen);
      wasOpen = now;
    });
  });

  let fileInput = $state<HTMLInputElement | null>(null);
  let uploading = $state(false);
  let percent = $state(0);
  let dragging = $state(false);
  let handle: UploadHandle | null = null;

  const asset = $derived(o.asset);
  const animated = $derived(isAnimatedAsset(asset));
  const drawn = $derived(imageBoxSize(o));
  const title = $derived(`Image — ${overlayLabel(o)}`);
  const summary = $derived(
    asset
      ? `${drawn.w}×${drawn.h}${o.opacity < 1 ? ` · ${Math.round(o.opacity * 100)}%` : ''} · ${o.anchor} ${o.x},${o.y}${animated ? (o.loop ? ' · loops' : ' · plays once') : ''}`
      : 'no image yet',
  );

  async function send(file: File) {
    if (uploading) return;
    uploading = true;
    percent = 0;
    handle = upload(file, (l, t) => (percent = t > 0 ? (l / t) * 100 : 0));
    try {
      const src = await handle.promise;
      o.asset = src;
      o.loop = true;
      o.enabled = true;
      toast.success(`Overlay ${src.name} (${src.info.width}×${src.info.height}, ${fmtBytes(src.size)})`);
    } catch (e) {
      if (!isAbortError(e)) toast.error(`Overlay upload failed: ${messageOf(e)}`);
    } finally {
      uploading = false;
      handle = null;
    }
  }
  function onPick(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const f = input.files?.[0];
    input.value = '';
    if (f) void send(f);
  }
  function onDrop(e: DragEvent) {
    e.preventDefault();
    dragging = false;
    const f = e.dataTransfer?.files?.[0];
    if (f) void send(f);
  }
  function onDragOver(e: DragEvent) {
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
    dragging = true;
  }
  function cancel() {
    handle?.abort();
  }
  function naturalSize() {
    o.width = 0;
    o.height = 0;
  }
  function setAnchor(a: Anchor) {
    const box = overlayBox(o);
    const p = anchorPoint(a, box.left, box.top, box.w, box.h);
    o.anchor = a;
    o.x = Math.round(p.x);
    o.y = Math.round(p.y);
  }
</script>

<OpCard {title} {summary} bind:enabled={o.enabled} bind:open>
  {#snippet actions()}
    <button type="button" class="sm ghost" onclick={() => moveOverlay(o.id, -1)} disabled={index <= 0} title="Draw earlier (move up)" aria-label="Move up">▲</button>
    <button type="button" class="sm ghost" onclick={() => moveOverlay(o.id, 1)} disabled={index >= count - 1} title="Draw later / on top (move down)" aria-label="Move down">▼</button>
    <button type="button" class="sm ghost" onclick={() => removeOverlay(o.id)} title="Remove this overlay" aria-label="Remove overlay">✕</button>
  {/snippet}
  <div class="asset" class:dragging role="group" aria-label="Overlay image" ondragover={onDragOver} ondragenter={onDragOver} ondragleave={() => (dragging = false)} ondrop={onDrop}>
    <input bind:this={fileInput} type="file" accept={ACCEPT} hidden onchange={onPick} />
    {#if uploading}
      <div class="row between">
        <span class="small">Uploading… {percent.toFixed(0)}%</span>
        <button type="button" class="sm ghost" onclick={cancel}>Cancel</button>
      </div>
      <div class="progress"><div style:width="{percent}%"></div></div>
    {:else if asset}
      <div class="row">
        <span class="name" title={asset.name}>{asset.name}</span>
        <span class="badge">{asset.info.width}×{asset.info.height}</span>
        {#if animated}
          <span class="badge">{asset.info.frames > 0 ? `${asset.info.frames} frames` : 'animated'}{asset.info.fps > 0 ? ` · ${fmtNum(asset.info.fps)} fps` : ''}{asset.info.duration > 0 ? ` · ${fmtSeconds(asset.info.duration)}` : ''}</span>
        {:else}
          <span class="badge">still</span>
        {/if}
        <span class="badge" class:ok={asset.info.hasAlpha}>alpha {asset.info.hasAlpha ? 'yes' : 'no'}</span>
        <button type="button" class="sm" onclick={() => fileInput?.click()}>Replace…</button>
      </div>
    {:else}
      <div class="row">
        <button type="button" class="sm primary" onclick={() => fileInput?.click()}>Choose image…</button>
        <span class="hint">or drop a PNG / GIF / WebP / APNG / AVIF / video here — animated files loop over the clip</span>
      </div>
    {/if}
  </div>
  <div class="row">
    <label class="field"><span>Width (0 = natural)</span><NumField bind:value={o.width} min={0} max={8192} small /></label>
    <label class="field"><span>Height (0 = natural)</span><NumField bind:value={o.height} min={0} max={8192} small /></label>
    <button type="button" class="sm ghost" onclick={naturalSize} disabled={o.width === 0 && o.height === 0}>Natural size</button>
    {#if asset}<span class="hint">drawn at <b>{drawn.w}×{drawn.h}</b>{asset.info.width !== drawn.w || asset.info.height !== drawn.h ? ` (from ${asset.info.width}×${asset.info.height})` : ''}</span>{/if}
    <label class="field slider">
      <span>Opacity — <b>{Math.round(o.opacity * 100)}%</b></span>
      <input type="range" min="0.05" max="1" step="0.05" bind:value={o.opacity} aria-label="Opacity" />
    </label>
    {#if animated}
      <label class="inline" title="Repeat the animation until the clip ends; off holds its last frame"><input type="checkbox" bind:checked={o.loop} /><span>Loop</span></label>
    {/if}
  </div>
  <div class="row">
    <span class="field">
      <span>Anchor</span>
      <AnchorGrid value={o.anchor} onchange={setAnchor} />
    </span>
    <label class="field"><span>X (px)</span><NumField bind:value={o.x} min={-8192} max={8192} small /></label>
    <label class="field"><span>Y (px)</span><NumField bind:value={o.y} min={-8192} max={8192} small /></label>
    <span class="hint">Output-canvas pixels — or drag the box on the preview.</span>
  </div>
  <TimeRangeFields {o} {info} />
  <p class="hint">
    Composited on the final {info.width}×{info.height}-derived canvas after every other op; an animated overlay is
    resampled to the output fps and, with Loop on, repeated until the base ends (<code>-stream_loop -1</code>).
  </p>
</OpCard>

<style>
  .asset {
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .asset.dragging {
    border-color: var(--accent);
    background: rgba(88, 101, 242, 0.08);
  }
  .row.between {
    justify-content: space-between;
  }
  .name {
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 13px;
  }
  .field.slider {
    flex: 1 1 160px;
  }
  .field.slider input[type='range'] {
    width: 100%;
  }
</style>
