<script lang="ts">
  import { dismiss, toasts } from '../lib/toast.svelte';
</script>

<div class="toasts" aria-live="polite">
  {#each toasts as t (t.id)}
    <button type="button" class="toast {t.kind}" onclick={() => dismiss(t.id)} title="Dismiss">
      <span class="icon" aria-hidden="true">{t.kind === 'error' ? '✕' : t.kind === 'success' ? '✓' : 'ℹ'}</span>
      <span class="text">{t.text}</span>
    </button>
  {/each}
</div>

<style>
  .toasts {
    position: fixed;
    right: 16px;
    bottom: 16px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    z-index: 100;
    max-width: min(420px, calc(100vw - 32px));
  }
  .toast {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    text-align: left;
    background: var(--panel-3);
    border: 1px solid var(--border-strong);
    border-left: 4px solid var(--blue);
    color: var(--text);
    box-shadow: var(--shadow);
    padding: 9px 12px;
    border-radius: var(--radius);
    white-space: normal;
    font-size: 13px;
    line-height: 1.4;
  }
  .toast.error {
    border-left-color: var(--red);
  }
  .toast.success {
    border-left-color: var(--green);
  }
  .icon {
    font-weight: 700;
    flex: none;
  }
  .toast.error .icon {
    color: var(--red);
  }
  .toast.success .icon {
    color: var(--green);
  }
  .toast.info .icon {
    color: var(--blue);
  }
  .text {
    word-break: break-word;
  }
</style>
