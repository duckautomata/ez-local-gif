<script lang="ts">
  import Header from './components/Header.svelte';
  import OpsPanel from './components/OpsPanel.svelte';
  import OutputCard from './components/OutputCard.svelte';
  import Preview from './components/Preview.svelte';
  import ProbeBadge from './components/ProbeBadge.svelte';
  import RenderPanel from './components/RenderPanel.svelte';
  import Toasts from './components/Toasts.svelte';
  import UploadZone from './components/UploadZone.svelte';
  import { startRender } from './lib/render.svelte';
  import { app } from './lib/state.svelte';

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
</script>

<svelte:window onkeydown={onKeydown} ondragover={swallowDrop} ondrop={swallowDrop} />

<Header />

<main class="layout">
  <section class="col">
    <UploadZone />
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
