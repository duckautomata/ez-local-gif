<script lang="ts">
  import { onMount } from 'svelte';
  import BatchOpsPanel from './components/BatchOpsPanel.svelte';
  import BatchPanel from './components/BatchPanel.svelte';
  import BatchRenderPanel from './components/BatchRenderPanel.svelte';
  import Header from './components/Header.svelte';
  import OpsPanel from './components/OpsPanel.svelte';
  import OutputCard from './components/OutputCard.svelte';
  import Preview from './components/Preview.svelte';
  import ProbeBadge from './components/ProbeBadge.svelte';
  import RenderPanel from './components/RenderPanel.svelte';
  import ShortcutsOverlay from './components/ShortcutsOverlay.svelte';
  import Toasts from './components/Toasts.svelte';
  import UploadZone from './components/UploadZone.svelte';
  import { getSource, messageOf, sourceHashFromSearch, sourceURL } from './lib/api';
  import { batch, renderAll } from './lib/batch.svelte';
  import { loadFeatures } from './lib/capabilities.svelte';
  import { resetRender, startRender } from './lib/render.svelte';
  import { cycleBackdrop, shortcutFor, togglePlay } from './lib/shortcuts';
  import { app, setSource } from './lib/state.svelte';
  import { toast } from './lib/toast.svelte';

  let helpOpen = $state(false);

  // Global shortcuts (lib/shortcuts.ts): Ctrl+Enter render (batch: render
  // all), Space play/stop (never while an interactive element has focus),
  // B backdrop cycle, ? this overlay, Esc closes it (the eyedropper's own
  // Esc lives in the Preview).
  function onKeydown(e: KeyboardEvent) {
    const target = e.target instanceof HTMLElement ? { tagName: e.target.tagName, isContentEditable: e.target.isContentEditable, role: e.target.getAttribute('role') } : null;
    switch (shortcutFor(e, target)) {
      case 'render':
        e.preventDefault();
        if (batch.active) void renderAll();
        else void startRender();
        break;
      case 'play':
        if (togglePlay()) e.preventDefault();
        break;
      case 'backdrop':
        app.ui.backdrop = cycleBackdrop(app.ui.backdrop);
        break;
      case 'help':
        helpOpen = true;
        e.preventDefault();
        break;
      case 'close':
        if (helpOpen) helpOpen = false;
        break;
    }
  }

  // A file dropped outside the drop zone must not navigate the page away.
  function swallowDrop(e: DragEvent) {
    e.preventDefault();
  }

  // '/?src=<hash>' (opened by "edit as source", or a reload) loads that
  // source on startup; a source that has been swept meanwhile is reported.
  // The hash is read synchronously here so the URL-sync effect below cannot
  // clear it first.
  const startHash = sourceHashFromSearch(window.location.search);
  let loadingSrc = $state(startHash !== null);
  onMount(() => {
    // The server's feature flags gate Play, the font picker and the
    // keying / overlay notices; a fetch that fails (server still starting)
    // is retried once a source is loaded — the server is reachable then.
    void loadFeatures();
    if (!startHash) return;
    void getSource(startHash)
      .then((src) => {
        resetRender();
        setSource(src);
      })
      .catch((e) => {
        toast.error(`Could not load source ${startHash.slice(0, 12)}…: ${messageOf(e)}`);
      })
      .finally(() => (loadingSrc = false));
  });

  // Keep the address bar pointing at the current source so a reload (or a
  // copied link) brings it back.
  $effect(() => {
    const hash = app.source?.hash ?? null;
    if (loadingSrc) return;
    const want = sourceURL(hash);
    if (window.location.pathname + window.location.search !== want) window.history.replaceState(null, '', want);
  });
  $effect(() => {
    if (app.source) void loadFeatures(); // a no-op once the flags are known
  });
</script>

<svelte:window onkeydown={onKeydown} ondragover={swallowDrop} ondrop={swallowDrop} />

<Header />

<main class="layout">
  <section class="col">
    <UploadZone />
    {#if loadingSrc}
      <p class="card hint">Loading source…</p>
    {/if}
    {#if batch.active}
      <BatchPanel />
      <BatchOpsPanel />
    {:else if app.source}
      <ProbeBadge source={app.source} />
      <!-- keyed on the file: a new source remounts the preview, so the previous
           file's still (and any in-flight still request) never lingers -->
      {#key app.source.hash}
        <Preview />
      {/key}
      <OpsPanel info={app.source.info} />
    {/if}
  </section>
  <section class="col">
    <OutputCard />
    {#if batch.active}
      <BatchRenderPanel />
    {:else}
      <RenderPanel />
    {/if}
  </section>
</main>

{#if helpOpen}
  <ShortcutsOverlay onclose={() => (helpOpen = false)} />
{/if}

<Toasts />
