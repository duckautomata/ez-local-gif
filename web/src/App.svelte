<script lang="ts">
  import { onMount } from 'svelte';
  import Header from './components/Header.svelte';
  import OpsPanel from './components/OpsPanel.svelte';
  import OutputCard from './components/OutputCard.svelte';
  import Preview from './components/Preview.svelte';
  import ProbeBadge from './components/ProbeBadge.svelte';
  import RenderPanel from './components/RenderPanel.svelte';
  import Toasts from './components/Toasts.svelte';
  import UploadZone from './components/UploadZone.svelte';
  import { getSource, messageOf, sourceHashFromSearch, sourceURL } from './lib/api';
  import { resetRender, startRender } from './lib/render.svelte';
  import { app, setSource } from './lib/state.svelte';
  import { toast } from './lib/toast.svelte';

  function onKeydown(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      void startRender();
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
</script>

<svelte:window onkeydown={onKeydown} ondragover={swallowDrop} ondrop={swallowDrop} />

<Header />

<main class="layout">
  <section class="col">
    <UploadZone />
    {#if loadingSrc}
      <p class="card hint">Loading source…</p>
    {/if}
    {#if app.source}
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
    <RenderPanel />
  </section>
</main>

<Toasts />
