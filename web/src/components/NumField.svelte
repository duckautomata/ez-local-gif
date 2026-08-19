<script lang="ts">
  // Numeric input bound to a number (never null): empty / NaN keeps the
  // previous value and the box is re-synced on blur; values are clamped.
  interface Props {
    value: number;
    min?: number;
    max?: number;
    step?: number | 'any';
    disabled?: boolean;
    placeholder?: string;
    title?: string;
    id?: string;
    small?: boolean;
    onchange?: (v: number) => void;
  }
  let {
    value = $bindable(),
    min,
    max,
    step = 1,
    disabled = false,
    placeholder,
    title,
    id,
    small = false,
    onchange,
  }: Props = $props();

  let el = $state<HTMLInputElement | null>(null);

  function set(v: number | null) {
    if (v === null || Number.isNaN(v)) return;
    let n = v;
    if (min !== undefined && n < min) n = min;
    if (max !== undefined && n > max) n = max;
    if (n !== value) {
      value = n;
      onchange?.(n);
    }
  }

  function onblur() {
    if (el && el.value.trim() === '') el.value = String(value);
  }
</script>

<input
  bind:this={el}
  type="number"
  class:w-sm={small}
  {min}
  {max}
  {step}
  {disabled}
  {placeholder}
  {title}
  {id}
  bind:value={() => value, set}
  {onblur}
/>
