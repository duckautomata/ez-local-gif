<script lang="ts">
  import { onMount } from 'svelte';
  import { isAbortError, listInput, messageOf, sourceFromInput, upload, type InputFile, type UploadHandle } from '../lib/api';
  import { batch, exitBatch, startBatch } from '../lib/batch.svelte';
  import { caps } from '../lib/capabilities.svelte';
  import { planDropSniffed, sequenceDelayOverride, sequenceFps } from '../lib/files';
  import { fmtBytes, fmtNum } from '../lib/format';
  import { resetRender } from '../lib/render.svelte';
  import { app, DEFAULT_DELAY_MS, setSource } from '../lib/state.svelte';
  import { toast } from '../lib/toast.svelte';
  import NumField from './NumField.svelte';

  const ACCEPT =
    '.gif,.webp,.apng,.png,.avif,.mp4,.mkv,.mov,.webm,.jpg,.jpeg,image/*,video/*,video/quicktime,video/x-matroska';

  let dragging = $state(false);
  let uploading = $state(false);
  let loaded = $state(0);
  let total = $state(0);
  let currentName = $state('');
  let handle: UploadHandle | null = null;
  let fileInput = $state<HTMLInputElement | null>(null);
  /** per-frame delay for image sequences (ms), sent with multi-file uploads */
  let delayMs = $state(DEFAULT_DELAY_MS);

  const percent = $derived(total > 0 ? Math.min(100, (loaded / total) * 100) : 0);
  const compact = $derived((app.source !== null || batch.active) && !uploading);
  const seqFps = $derived(sequenceFps(delayMs));

  async function send(files: File[]) {
    if (uploading) {
      toast.info('An upload is already in progress');
      return;
    }
    if (files.length === 0) return;
    uploading = true;
    loaded = 0;
    total = files.reduce((n, f) => n + f.size, 0);
    currentName = files.length === 1 ? files[0].name : `${files.length} images as a sequence (${fmtNum(delayMs, 0)} ms / frame)`;
    handle = upload(
      files,
      (l, t) => {
        loaded = l;
        total = t;
      },
      { delayMs },
    );
    try {
      const src = await handle.promise;
      // A new source replaces the old one: drop the previous job/result (and
      // its progress subscription) first so nothing rendered from a different
      // file stays on screen; the preview is keyed on the source hash and
      // remounts (App.svelte). A single upload also leaves batch mode.
      resetRender();
      exitBatch();
      setSource(src);
      const seq = src.info.sequence;
      const what = seq ? `sequence of ${seq.count} frames` : src.name;
      // The store dedupes identical frame sets and keeps the delay they were
      // first stored with, so a re-upload can come back with a different delay
      // than the one requested. Honour the request with the "delay" op (the
      // documented override) instead of silently keeping the stored timing.
      const override = sequenceDelayOverride(seq, files.length, delayMs);
      if (override > 0 && seq) {
        app.ops.delay = { enabled: true, ms: override };
        toast.success(
          `Uploaded ${what} (${fmtBytes(src.size)}) — the frames were already stored at ${seq.delayMs} ms / frame, so the Delay op now applies your ${override} ms`,
        );
      } else {
        toast.success(`Uploaded ${what} (${fmtBytes(src.size)})`);
      }
    } catch (e) {
      if (!isAbortError(e)) toast.error(`Upload failed: ${messageOf(e)}`);
    } finally {
      uploading = false;
      handle = null;
    }
  }

  /** files of a mixed drop (images + others) waiting for the user's choice */
  let mixed = $state<{ images: File[]; files: File[] } | null>(null);

  /** enter batch mode with one row per file (Phase 4). */
  function startFiles(files: File[]) {
    mixed = null;
    resetRender();
    setSource(null); // fresh state for a fresh drop, like a single upload
    void startBatch(files);
  }

  /**
   * accept turns a dropped/picked/pasted file list into one upload, an image
   * sequence, a batch (≥ 2 video/animation files) or — for a mixed drop — an
   * inline question. Sequence-eligible files are head-sniffed first so an
   * animated WebP / APNG never silently becomes a first-frame slideshow.
   */
  async function accept(list: FileList | File[] | null | undefined) {
    const plan = await planDropSniffed(list ? Array.from(list) : []);
    if (plan.note) toast.info(plan.note);
    if (plan.kind !== 'mixed') mixed = null;
    if (plan.kind === 'single') void send([plan.file]);
    else if (plan.kind === 'sequence') void send(plan.files);
    else if (plan.kind === 'batch') startFiles(plan.files);
    else if (plan.kind === 'mixed') mixed = { images: plan.images, files: plan.files };
  }

  // ---- "…or pick from /input" (Phase 4, features.inputPick) --------------
  const inputPick = $derived(caps.features.inputPick);
  let inputOpen = $state(false);
  let inputLoading = $state(false);
  let inputFiles = $state<InputFile[]>([]);
  let inputError = $state('');
  let ingesting = $state('');

  async function toggleInput() {
    inputOpen = !inputOpen;
    if (!inputOpen) return;
    inputLoading = true;
    inputError = '';
    try {
      inputFiles = await listInput();
    } catch (e) {
      inputError = messageOf(e);
      inputFiles = [];
    } finally {
      inputLoading = false;
    }
  }

  async function pickInput(f: InputFile) {
    if (ingesting) return;
    ingesting = f.name;
    try {
      const src = await sourceFromInput(f.name);
      resetRender();
      exitBatch();
      setSource(src);
      inputOpen = false;
      toast.success(`Loaded ${f.name} from /input (${fmtBytes(src.size)})`);
    } catch (e) {
      toast.error(`Could not load ${f.name}: ${messageOf(e)}`);
    } finally {
      ingesting = '';
    }
  }

  function cancel() {
    handle?.abort();
  }

  function openPicker() {
    if (!uploading) fileInput?.click();
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    dragging = false;
    void accept(e.dataTransfer?.files);
  }

  function onDragOver(e: DragEvent) {
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
    dragging = true;
  }

  function onPick(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const files = input.files ? Array.from(input.files) : [];
    input.value = '';
    void accept(files);
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
      if (files.length === 0) return;
      e.preventDefault();
      // Pasted bitmaps come in as "image.png" — give them a timestamped name.
      void accept(files.map((f) => (f.name ? f : new File([f], `pasted-${Date.now()}.png`, { type: f.type }))));
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
  <input bind:this={fileInput} type="file" accept={ACCEPT} multiple hidden onchange={onPick} />
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
      <span class="muted small">
        Drop or paste (<kbd>Ctrl</kbd>+<kbd>V</kbd>) another file — several images = a sequence, several videos = a batch — to
        replace the source, or
      </span>
      <span class="row tight">
        <label class="seq small" title="Per-frame delay used when several images are uploaded as a sequence">
          <span class="muted">sequence delay</span>
          <NumField bind:value={delayMs} min={1} max={60000} small /><span class="muted">ms{#if seqFps > 0}&nbsp;→ {fmtNum(seqFps)} fps{/if}</span>
        </label>
        <button type="button" class="sm" onclick={openPicker}>Choose file(s)…</button>
      </span>
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
        <span class="btn primary">Choose file(s)…</span>
      </button>
      <p class="hint">GIF · WebP · APNG · AVIF · PNG · JPEG · MP4 · MKV · MOV (ProRes 4444 alpha) · WebM</p>
      <label class="seq hint" title="Per-frame delay used when several images are uploaded as a sequence">
        <span>Several images (PNG / JPEG / WebP / BMP / TIFF) = one sequence · frame delay</span>
        <NumField bind:value={delayMs} min={1} max={60000} small />
        <span>ms{#if seqFps > 0}&nbsp;→ {fmtNum(seqFps)} fps{/if} (changeable later in the Delay card)</span>
      </label>
      <p class="hint">Several videos / animations = a batch: the same preset rendered for each file.</p>
    </div>
  {/if}

  {#if mixed && !uploading}
    {@const m = mixed}
    {@const others = m.files.length - m.images.length}
    <div class="mixed note">
      <p>
        That drop mixes {m.images.length} image{m.images.length === 1 ? '' : 's'} with {others} other file{others === 1 ? '' : 's'} —
        what did you mean?
      </p>
      <div class="row choices">
        <button type="button" class="sm primary" onclick={() => startFiles(m.files)}>Batch all {m.files.length} files</button>
        {#if m.images.length >= 2}
          <button
            type="button"
            class="sm"
            onclick={() => {
              mixed = null;
              void send(m.images);
            }}
          >
            Use the {m.images.length} images as one sequence
          </button>
        {/if}
        <button type="button" class="sm ghost" onclick={() => (mixed = null)}>Cancel</button>
      </div>
    </div>
  {/if}

  {#if inputPick && !uploading}
    <div class="inputpick">
      <button type="button" class="sm ghost" onclick={() => void toggleInput()} aria-expanded={inputOpen}>
        {inputOpen ? 'Hide /input' : '…or pick from /input'}
      </button>
      {#if inputOpen}
        {#if inputLoading}
          <p class="hint">Listing /input…</p>
        {:else if inputError}
          <p class="note error">/input: {inputError}</p>
        {:else if inputFiles.length === 0}
          <p class="hint">No decodable files in /input.</p>
        {:else}
          <ul class="inlist" aria-label="Files in /input">
            {#each inputFiles as f (f.name)}
              <li>
                <button type="button" class="infile" onclick={() => void pickInput(f)} disabled={!!ingesting} title="Load {f.name} as the source">
                  <span class="iname" title={f.name}>{ingesting === f.name ? `Loading ${f.name}…` : f.name}</span>
                  <span class="muted small">{fmtBytes(f.size)}</span>
                  <span class="muted small">{new Date(f.mtime).toLocaleString()}</span>
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      {/if}
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
  .row.tight {
    gap: 8px;
    flex-wrap: nowrap;
  }
  .seq {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
    justify-content: center;
  }
  .up {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .mixed {
    margin-top: 8px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .choices {
    gap: 8px;
    flex-wrap: wrap;
  }
  .inputpick {
    margin-top: 6px;
    display: flex;
    flex-direction: column;
    gap: 4px;
    align-items: flex-start;
  }
  .inlist {
    list-style: none;
    margin: 0;
    padding: 0;
    width: 100%;
    max-height: 240px;
    overflow: auto;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .infile {
    display: flex;
    align-items: baseline;
    gap: 10px;
    width: 100%;
    text-align: left;
    padding: 3px 8px;
  }
  .infile .iname {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
</style>
