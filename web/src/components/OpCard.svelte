<script lang="ts">
  import type { Snippet } from 'svelte';

  // Collapsible op card with an enable toggle in the header. Any input /
  // change inside the body enables the op automatically (buttons that
  // change values call `enabled = true` themselves).
  interface Props {
    title: string;
    summary?: string;
    enabled?: boolean;
    open?: boolean;
    /** hide the enable checkbox (always-on cards) */
    toggle?: boolean;
    children: Snippet;
  }
  let { title, summary = '', enabled = $bindable(false), open = $bindable(false), toggle = true, children }: Props =
    $props();

  function autoEnable() {
    if (toggle && !enabled) enabled = true;
  }
</script>

<section class="card op" class:enabled class:open>
  <div class="op-head">
    {#if toggle}
      <input type="checkbox" bind:checked={enabled} title="Enable {title}" aria-label="Enable {title}" />
    {/if}
    <button type="button" class="op-title" onclick={() => (open = !open)} aria-expanded={open}>
      <span class="chev" aria-hidden="true">{open ? '▾' : '▸'}</span>
      <span class="name">{title}</span>
      {#if summary && !open}<span class="op-summary">{summary}</span>{/if}
    </button>
  </div>
  {#if open}
    <div class="op-body" oninput={autoEnable} onchange={autoEnable}>
      {@render children()}
    </div>
  {/if}
</section>

<style>
  .op {
    padding: 0;
    overflow: hidden;
  }
  .op-head {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 10px 6px 12px;
  }
  .op-title {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 8px;
    background: transparent;
    border: 0;
    padding: 4px 2px;
    color: var(--text);
    font-size: 13.5px;
    font-weight: 600;
    text-align: left;
    min-width: 0;
  }
  .op-title:hover {
    background: transparent;
    color: #fff;
  }
  .chev {
    color: var(--muted);
    width: 10px;
    flex: none;
  }
  .op:not(.enabled) .name {
    color: var(--muted);
    font-weight: 500;
  }
  .op-summary {
    font-weight: 400;
    color: var(--muted);
    font-size: 12px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
  .op.enabled .op-summary {
    color: var(--text);
  }
  .op.enabled {
    border-color: var(--border-strong);
  }
  .op-body {
    padding: 4px 14px 12px 34px;
    border-top: 1px solid var(--border);
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
</style>
