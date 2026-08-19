<script lang="ts">
  import type { Backdrop } from '../lib/state.svelte';

  interface Props {
    value: Backdrop;
  }
  let { value = $bindable() }: Props = $props();

  const options: { id: Backdrop; label: string; title: string }[] = [
    { id: 'checker', label: 'Checker', title: 'Checkerboard — shows transparency' },
    { id: 'dark', label: 'Discord dark', title: 'Discord dark theme #313338' },
    { id: 'white', label: 'White', title: 'Discord light theme / white' },
  ];
</script>

<div class="seg" role="group" aria-label="Preview backdrop">
  {#each options as o (o.id)}
    <button type="button" aria-pressed={value === o.id} title={o.title} onclick={() => (value = o.id)}>
      <span class="swatch backdrop-{o.id}" aria-hidden="true"></span>{o.label}
    </button>
  {/each}
</div>

<style>
  .swatch {
    display: inline-block;
    width: 12px;
    height: 12px;
    border-radius: 3px;
    border: 1px solid var(--border-strong);
    margin-right: 5px;
    vertical-align: -2px;
    background-size: 6px 6px;
    background-position:
      0 0,
      0 3px,
      3px -3px,
      -3px 0;
  }
</style>
