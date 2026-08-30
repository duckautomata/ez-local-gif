<script lang="ts">
  // The "?" keyboard-shortcuts overlay (Phase 4, DESIGN §7). Esc or the
  // close button (or a click on the dimmed backdrop) closes it; the binding
  // list lives in lib/shortcuts.ts so it stays in sync with the handlers.
  import { SHORTCUT_LIST, trapTabTarget } from '../lib/shortcuts';

  interface Props {
    onclose: () => void;
  }
  let { onclose }: Props = $props();

  // Focus management (WEB-8): aria-modal promises an inert background, so
  // the dialog must take focus when it opens, Tab must loop within the
  // sheet, and the element focused before ? was pressed gets focus back on
  // close — otherwise a keyboard / screen-reader user keeps tabbing the
  // obscured page and can activate controls behind the dim layer.
  let sheet = $state<HTMLElement | null>(null);
  $effect(() => {
    const el = sheet;
    if (!el) return;
    const before = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    el.focus();
    return () => before?.focus();
  });
  function onTrapKey(e: KeyboardEvent) {
    if (e.key !== 'Tab' || !sheet) return;
    const items = Array.from(sheet.querySelectorAll<HTMLElement>('button, a[href], input, select, textarea, [tabindex]:not([tabindex="-1"])')).filter(
      (el) => !el.hasAttribute('disabled'),
    );
    const active = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const to = trapTabTarget(items.length, active ? items.indexOf(active) : -1, e.shiftKey);
    if (to === null && items.length > 0) return; // the default move stays inside
    items[to ?? 0]?.focus();
    e.preventDefault();
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -- the backdrop click is a convenience; Esc and the close button are the accessible paths (the keydown only traps Tab) -->
<div class="backdrop" onclick={(e) => e.target === e.currentTarget && onclose()} onkeydown={onTrapKey}>
  <div class="sheet card" role="dialog" aria-modal="true" aria-label="Keyboard shortcuts" tabindex="-1" bind:this={sheet}>
    <div class="head">
      <h2>Keyboard shortcuts</h2>
      <button type="button" class="sm ghost" onclick={onclose} title="Close (Esc)">✕</button>
    </div>
    <table>
      <tbody>
        {#each SHORTCUT_LIST as s (s.what)}
          <tr>
            <td class="keys">
              {#each s.keys as k, i (k)}{#if i > 0}<span class="plus">+</span>{/if}<kbd>{k}</kbd>{/each}
            </td>
            <td>{s.what}</td>
          </tr>
        {/each}
      </tbody>
    </table>
    <p class="hint">Single-letter shortcuts are ignored while an input has focus; Space also leaves buttons and sliders alone.</p>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.55);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
  }
  .sheet:focus {
    /* the dialog itself takes programmatic focus on open — no ring; the
       close button keeps its own focus style for Tab users */
    outline: none;
  }
  .sheet {
    max-width: 460px;
    width: calc(100% - 40px);
    max-height: 80vh;
    overflow: auto;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
  .head h2 {
    margin: 0;
  }
  table {
    border-collapse: collapse;
    width: 100%;
  }
  td {
    padding: 4px 6px;
    border-top: 1px solid var(--border);
    font-size: 13px;
  }
  .keys {
    white-space: nowrap;
    width: 130px;
  }
  .plus {
    color: var(--muted);
    margin: 0 3px;
  }
</style>
