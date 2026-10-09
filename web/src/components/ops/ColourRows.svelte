<script lang="ts">
  // The colour rows shared by the Background card's Colour mode (colours
  // to remove: one colorkey op each) and the AI mode's Keep colours
  // (Phase 5c: colours forced opaque on the matte): per row an eyedropper
  // button (the editor; batch has no preview — typed hex only), a swatch,
  // a hex field and a remove button, then "+ add colour". Row state and
  // the eyedropper arming live in lib/state; this component only renders
  // and forwards.
  interface Props {
    /** RRGGBB per row ('' = waiting for a pick) */
    colors: readonly string[];
    /** offer the preview eyedropper */
    picker: boolean;
    /** the eyedropper is armed for one of THESE rows (the row index), −1 for none */
    armedRow: number;
    /** the row cap */
    max: number;
    /** the field's caption above the rows ("Colour to remove", "Keep colour") — pluralised by the caller */
    caption: string;
    /** the aria-label stem of the hex fields: "Colour {i} to remove (hex)" / "Keep colour {i} (hex)" — {i} is the 1-based row */
    hexLabel: string;
    /** the aria-label stem of the remove buttons: "Remove colour {i}" / "Remove keep colour {i}" */
    removeLabel: string;
    /** the "+ add colour" tooltip */
    addTitle: string;
    /** shown next to "+ add colour" while no row carries a colour */
    emptyHint: string;
    /** the remove button's tooltip when the row is the only one (the Colour mode clears it instead of removing) */
    clearOnly?: boolean;
    onPick: (i: number) => void;
    onHex: (i: number, value: string) => boolean;
    onRemove: (i: number) => void;
    onAdd: () => void;
  }
  let { colors, picker, armedRow, max, caption, hexLabel, removeLabel, addTitle, emptyHint, clearOnly = false, onPick, onHex, onRemove, onAdd }: Props = $props();

  const picked = $derived(colors.filter((c) => c !== ''));

  /** a typed row hex: stored when valid, else the field is put back to what the row holds */
  function hex(i: number, e: Event & { currentTarget: HTMLInputElement }) {
    if (!onHex(i, e.currentTarget.value)) e.currentTarget.value = colors[i] ? '#' + colors[i] : '';
  }
  function label(i: number): string {
    return hexLabel.replace('{i}', String(i + 1));
  }
  function removeName(i: number): string {
    return removeLabel.replace('{i}', String(i + 1));
  }
</script>

<span class="field colours">
  <span>{caption}</span>
  {#each colors as c, i (i)}
    <span class="row tight colour-row">
      {#if picker}
        <button type="button" class="sm" class:primary={armedRow === i} aria-pressed={armedRow === i} onclick={() => onPick(i)} title="Then click the colour on the preview">
          {armedRow === i ? 'Click the preview…' : c ? 'Pick again' : 'Pick from preview'}
        </button>
      {/if}
      <span class="swatch" class:empty={!c} style:background={c ? '#' + c : undefined} aria-hidden="true"></span>
      <input type="text" class="hex mono" value={c ? '#' + c : ''} placeholder="#rrggbb" onchange={(e) => hex(i, e)} maxlength="7" spellcheck="false" aria-label={label(i)} />
      {#if colors.length > 1 || c || !clearOnly}
        <button type="button" class="sm ghost" onclick={() => onRemove(i)} aria-label={removeName(i)} title={clearOnly && colors.length === 1 ? 'Clear this colour' : 'Remove this colour'}>×</button>
      {/if}
    </span>
  {/each}
  <span class="row tight">
    <button type="button" class="sm" onclick={onAdd} disabled={colors.length >= max} title={addTitle}>+ add colour</button>
    {#if !picked.length}<span class="hint">{emptyHint}</span>{/if}
  </span>
</span>

<style>
  .row.tight {
    gap: 6px;
    flex-wrap: nowrap;
  }
  .field.colours {
    gap: 6px;
  }
  .field.colours > .row + .row {
    margin-top: 0;
  }
  .hex {
    width: 84px;
  }
  .swatch {
    display: inline-block;
    width: 24px;
    height: 24px;
    border-radius: 4px;
    border: 1px solid var(--border-strong);
    flex: none;
  }
  .swatch.empty {
    border-style: dashed;
    background: transparent;
  }
</style>
