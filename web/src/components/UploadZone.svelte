<script lang="ts">
  import { onMount } from 'svelte';
  import { isAbortError, messageOf, upload, type UploadHandle } from '../lib/api';
  import { fmtBytes } from '../lib/format';
  import { resetRender } from '../lib/render.svelte';
  import { app, setSource } from '../lib/state.svelte';
  import { toast } from '../lib/toast.svelte';

  const ACCEPT =
    '.gif,.webp,.apng,.png,.avif,.mp4,.mkv,.mov,.webm,.jpg,.jpeg,image/*,video/*,video/quicktime,video/x-matroska';

  let dragging = $state(false);
  let uploading = $state(false);
  let loaded = $state(0);
  let total = $state(0);
  let currentName = $state('');
  let handle: UploadHandle | null = null;
  let fileInput = $state<HTMLInputElement | null>(null);

  const percent = $derived(total > 0 ? Math.min(100, (loaded / total) * 100) : 0);
  const compact = $derived(app.source !== null && !uploading);

  async function send(file: File) {
    if (uploading) {
      toast.info('An upload is already in progress');
      return;
    }
    uploading = true;
    loaded = 0;
    total = file.size;
    currentName = file.name;
    handle = upload(file, (l, t) => {
      loaded = l;
      total = t;
    });
    try {
      const src = await handle.promise;
      // A new source replaces the old one: drop the previous job/result (and
      // its progress subscription) first so nothing rendered from a different
      // file stays on screen; the preview is keyed on the source hash and
      // remounts (App.svelte).
      resetRender();
      setSource(src);
      toast.success(`Uploaded ${src.name} (${fmtBytes(src.size)})`);
    } catch (e) {
      if (!isAbortError(e)) toast.error(`Upload failed: ${messageOf(e)}`);
    } finally {
      uploading = false;
      handle = null;
    }
  }

  function cancel() {
    handle?.abort();
  }

  function openPicker() {
    if (!uploading) fileInput?.click();
  }

  function pickFirst(list: FileList | File[] | null | undefined): File | null {
    if (!list || list.length === 0) return null;
    // Phase 1: one file per source (image sequences arrive in Phase 2).
    if (list.length > 1) toast.info(`Using the first of ${list.length} files — sequences arrive in Phase 2`);
    return list[0] ?? null;
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    dragging = false;
    const f = pickFirst(e.dataTransfer?.files);
    if (f) void send(f);
  }

  function onDragOver(e: DragEvent) {
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
    dragging = true;
  }

  function onPick(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const f = pickFirst(input.files);
    input.value = '';
    if (f) void send(f);
  }

  // Ctrl+V of a file (e.g. copied from Explorer) or an image bitmap.
  onMount(() => {
    const onPaste = (e: ClipboardEvent) => {
      const target = e.target as HTMLElement | null;
      if (target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable)) return;
      const files: File[] = [];
      const items = e.clipboardData?.items;
      if (items) {
        for (const it of items) {
          if (it.kind === 'file') {
            const f = it.getAsFile();
            if (f) files.push(f);
          }
        }
      }
      if (files.length === 0 && e.clipboardData?.files.length) files.push(...e.clipboardData.files);
      const f = pickFirst(files);
      if (!f) return;
      e.preventDefault();
      // Pasted bitmaps come in as "image.png" — give them a timestamped name.
      const named = f.name ? f : new File([f], `pasted-${Date.now()}.png`, { type: f.type });
      void send(named);
    };
    window.addEventListener('paste', onPaste);
    return () => window.removeEventListener('paste', onPaste);
  });
</script>

<!--
  The zone is a plain drop target (drag & drop is mouse-only by nature; paste
  is handled on the window) grouping real controls: a <button> opens the file
  picker — Enter/Space on it work natively — and Cancel is an ordinary sibling
  button, so no interactive element is nested inside another control (there
  is no role="button" wrapper any more). In the empty state the picker button
  spans the dashed box, so clicking anywhere in it still opens the dialog.
-->
<div
  class="zone card"
  class:dragging
  class:compact
  role="group"
  aria-label="Source file"
  ondragover={onDragOver}
  ondragenter={onDragOver}
  ondragleave={() => (dragging = false)}
  ondrop={onDrop}
>
  <input bind:this={fileInput} type="file" accept={ACCEPT} hidden onchange={onPick} />
  {#if uploading}
    <div class="up">
      <div class="row between">
        <span class="name" title={currentName}>Uploading {currentName}</span>
        <span class="muted small">{fmtBytes(loaded)} / {fmtBytes(total)} · {percent.toFixed(0)}%</span>
      </div>
      <div class="progress"><div style:width="{percent}%"></div></div>
      <button type="button" class="sm ghost" onclick={cancel}>Cancel</button>
    </div>
  {:else if compact}
    <div class="row between">
      <span class="muted small">Drop or paste (<kbd>Ctrl</kbd>+<kbd>V</kbd>) another file here to replace the source, or</span>
      <button type="button" class="sm" onclick={openPicker}>Choose file…</button>
    </div>
  {:else}
    <div class="big">
      <button type="button" class="pick" onclick={openPicker}>
        <span class="icon" aria-hidden="true">
          <svg width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">
            <path d="M12 16V4" /><path d="m7 9 5-5 5 5" /><path d="M4 16v3a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-3" />
          </svg>
        </span>
        <span class="title">Drop a file here, paste it, or</span>
        <span class="btn primary">Choose file…</span>
      </button>
      <p class="hint">GIF · WebP · APNG · AVIF · PNG · MP4 · MKV · MOV (ProRes 4444 alpha) · WebM</p>
    </div>
  {/if}
</div>

<style>
  .zone {
    border: 2px dashed var(--border-strong);
    background: var(--panel);
    transition:
      border-color 0.12s,
      background 0.12s;
  }
  .zone:hover,
  .zone.dragging {
    border-color: var(--accent);
    background: rgba(88, 101, 242, 0.08);
  }
  .zone.compact {
    padding: 8px 14px;
    border-style: dashed;
  }
  .big {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
    padding: 12px 8px 18px;
  }
  /* The empty-state button fills the zone and looks like plain content (the
     "Choose file…" pill inside is decoration — the whole block is the
     button); the zone border/background carry the hover and drag feedback. */
  button.pick {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 6px;
    width: 100%;
    padding: 10px 8px 12px;
    background: transparent;
    border: 0;
    border-radius: var(--radius-sm);
    color: inherit;
    font: inherit;
    text-align: center;
    white-space: normal;
    cursor: pointer;
  }
  button.pick:hover:not(:disabled) {
    background: transparent;
  }
  button.pick:hover .btn {
    background: var(--accent-hover);
  }
  .icon {
    color: var(--muted);
    line-height: 0;
  }
  .title {
    font-size: 15px;
    font-weight: 600;
  }
  button.pick .btn {
    margin-top: 2px;
  }
  .row.between {
    justify-content: space-between;
    width: 100%;
  }
  .up {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
</style>
