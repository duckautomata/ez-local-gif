<script lang="ts">
  import type { Check, Report } from '../lib/api';
  import { TARGET_LABEL } from '../lib/presets';
  import { ruleLabel } from '../lib/rules';

  interface Props {
    report: Report;
  }
  let { report }: Props = $props();

  const checks = $derived(report.checks ?? []);
  const failed = $derived(checks.filter((c) => !c.ok));
  const errors = $derived(failed.filter((c) => c.level === 'error').length);
  const warns = $derived(failed.filter((c) => c.level === 'warn').length);
  const fixed = $derived(checks.filter((c) => c.fixed).length);

  function icon(c: Check): { glyph: string; cls: string; label: string } {
    if (c.ok) return { glyph: '✓', cls: 'ok', label: 'ok' };
    switch (c.level) {
      case 'error':
        return { glyph: '✕', cls: 'bad', label: 'error' };
      case 'warn':
        return { glyph: '▲', cls: 'warn', label: 'warning' };
      default:
        return { glyph: '●', cls: 'info', label: 'info' };
    }
  }
</script>

<div class="checks">
  <div class="head">
    <b class={report.ok ? 'ok' : 'bad'}>{report.ok ? '✓ Discord-safe' : '✕ Will not render safely on Discord'}</b>
    <span class="muted small">
      {report.target ? TARGET_LABEL[report.target] : 'structural rules only'}
      {#if errors}· {errors} error{errors === 1 ? '' : 's'}{/if}
      {#if warns}· {warns} warning{warns === 1 ? '' : 's'}{/if}
      {#if fixed}· {fixed} fixed{/if}
      · rules {report.rulesVersion}
    </span>
  </div>
  {#if checks.length === 0}
    <p class="hint">The linter reported no individual checks.</p>
  {:else}
    <ul>
      {#each checks as c, i (`${i}:${c.rule}`)}
        {@const ic = icon(c)}
        <li class:failed={!c.ok} class:errored={!c.ok && c.level === 'error'} class:warned={!c.ok && c.level === 'warn'}>
          <span class="ic {ic.cls}" title={ic.label} aria-label={ic.label}>{ic.glyph}</span>
          <span class="body">
            <span class="label" title={c.rule}>{ruleLabel(c.rule)}</span>
            <span class="rule mono">{c.rule}</span>
            {#if c.fixed}<span class="tag">fixed</span>{/if}
            {#if c.detail}<span class="detail">{c.detail}</span>{/if}
          </span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .checks {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 4px 10px;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 3px;
    max-height: 260px;
    overflow: auto;
  }
  li {
    display: flex;
    gap: 8px;
    align-items: baseline;
    font-size: 12.5px;
    padding: 2px 4px;
    border-radius: 4px;
  }
  li.failed {
    background: rgba(255, 255, 255, 0.03);
  }
  li.errored {
    background: rgba(242, 63, 67, 0.08);
  }
  li.warned {
    background: rgba(240, 178, 50, 0.08);
  }
  .ic {
    flex: none;
    width: 14px;
    text-align: center;
    font-size: 11px;
  }
  .ic.info {
    color: var(--blue);
  }
  .body {
    display: flex;
    flex-wrap: wrap;
    gap: 2px 8px;
    min-width: 0;
    align-items: baseline;
  }
  .label {
    color: var(--text);
    font-weight: 500;
  }
  li.failed .label {
    font-weight: 600;
  }
  .rule {
    color: var(--faint);
    font-size: 11px;
  }
  .detail {
    color: var(--text);
    word-break: break-word;
  }
  .tag {
    font-size: 10.5px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    background: rgba(35, 165, 89, 0.18);
    color: var(--green);
    border-radius: 3px;
    padding: 0 5px;
  }
</style>
