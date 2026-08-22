<script lang="ts">
  // 3×3 anchor picker for text / image overlays: which point of the element
  // X/Y refer to (recipe.Anchor*).
  import { ANCHORS, type Anchor } from '../lib/api';

  interface Props {
    value: Anchor;
    /** called with the new anchor (the caller may move X/Y so the element stays put) */
    onchange?: (a: Anchor) => void;
    label?: string;
  }
  let { value, onchange, label = 'Anchor' }: Props = $props();

  const names: Record<Anchor, string> = {
    tl: 'top left',
    tc: 'top centre',
    tr: 'top right',
    ml: 'middle left',
    mc: 'centre',
    mr: 'middle right',
    bl: 'bottom left',
    bc: 'bottom centre',
    br: 'bottom right',
  };
</script>

<span class="grid" role="group" aria-label={label} title="Which point of the element X / Y place">
  {#each ANCHORS as a (a)}
    <button type="button" class="cell" aria-pressed={value === a} aria-label={names[a]} title={names[a]} onclick={() => onchange?.(a)}>
      <span class="dot" aria-hidden="true"></span>
    </button>
  {/each}
</span>

<style>
  .grid {
    display: inline-grid;
    grid-template-columns: repeat(3, 18px);
    grid-template-rows: repeat(3, 18px);
    gap: 2px;
    padding: 3px;
    background: var(--bg);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
  }
  .cell {
    padding: 0;
    border: 0;
    border-radius: 3px;
    background: var(--panel-2);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 0;
  }
  .cell:hover:not(:disabled) {
    background: var(--panel-3);
  }
  .cell[aria-pressed='true'] {
    background: var(--accent);
  }
  .dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--muted);
  }
  .cell[aria-pressed='true'] .dot {
    background: #fff;
  }
</style>
