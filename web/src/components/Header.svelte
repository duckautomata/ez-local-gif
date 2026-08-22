<script lang="ts">
  import { onMount } from 'svelte';
  import { ping } from '../lib/api';
  import { resetRender } from '../lib/render.svelte';
  import { resetApp } from '../lib/state.svelte';

  // Server reachability: probe /healthz on load, then every 15 s (5 s while
  // offline) so a missing backend is visible before the first request fails.
  let online = $state<boolean | null>(null);

  onMount(() => {
    let timer: number | undefined;
    let cancelled = false;
    const check = async () => {
      const ok = await ping();
      if (cancelled) return;
      online = ok;
      timer = window.setTimeout(() => void check(), ok ? 15_000 : 5_000);
    };
    void check();
    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  });

  // The logo is the "home" link: back to the empty landing page without a
  // reload — drop the source, result and in-flight job, default ops and
  // Output card (App.svelte then syncs the address bar back to '/'). A plain
  // href keeps middle-click / open-in-new-tab working.
  function home(e: MouseEvent) {
    if (e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    resetRender();
    resetApp();
  }
</script>

<header class="hdr">
  <div class="brand">
    <a class="logo" href="/" onclick={home} title="Start over — back to the empty page" aria-label="ez-local-gif: start over with an empty page">GIF</a>
    <div>
      <h1>ez-local-gif</h1>
      <p class="tag">Discord-safe GIF / WebP / APNG / AVIF from ProRes, video, animations and image sequences — rendered locally</p>
    </div>
  </div>
  <div class="status" title={online === null ? 'Checking server…' : online ? 'Server reachable' : 'Server unreachable'}>
    <span class="dot" class:ok={online === true} class:bad={online === false}></span>
    <span class="small muted">{online === null ? 'connecting…' : online ? 'server online' : 'server offline'}</span>
  </div>
</header>

<style>
  .hdr {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    padding: 10px 16px;
    border-bottom: 1px solid var(--border);
    background: var(--panel);
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 12px;
  }
  .logo {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 36px;
    height: 36px;
    border-radius: 9px;
    background: var(--accent);
    color: #fff;
    font-weight: 700;
    font-size: 13px;
    letter-spacing: 0.02em;
    text-decoration: none;
    cursor: pointer;
    transition: background 0.12s;
  }
  .logo:hover {
    background: var(--accent-hover);
    text-decoration: none;
  }
  .logo:focus-visible {
    outline: 2px solid var(--text);
    outline-offset: 2px;
  }
  h1 {
    font-size: 17px;
  }
  .tag {
    font-size: 12px;
    color: var(--muted);
  }
  .status {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    white-space: nowrap;
  }
  .dot {
    width: 9px;
    height: 9px;
    border-radius: 50%;
    background: var(--faint);
  }
  .dot.ok {
    background: var(--green);
  }
  .dot.bad {
    background: var(--red);
  }
  @media (max-width: 640px) {
    .tag {
      display: none;
    }
  }
</style>
