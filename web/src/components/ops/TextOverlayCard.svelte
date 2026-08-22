<script lang="ts">
  // One text overlay (op "text": drawtext through a text file). Coordinates
  // are output pixels on the final canvas — the preview shows the real
  // composite and a drag box (OverlayLayer).
  import { untrack } from 'svelte';
  import type { Anchor, ProbeInfo } from '../../lib/api';
  import { caps } from '../../lib/capabilities.svelte';
  import { loadFontFamilies } from '../../lib/fonts';
  import { round } from '../../lib/format';
  import { anchorPoint, overlayBox, overlayLabel, pickerColor, selectionAfterToggle, TEXT_DEFAULTS } from '../../lib/overlay';
  import { app, effectiveOps, moveOverlay, planCanvas, removeOverlay, type TextOverlayCfg } from '../../lib/state.svelte';
  import AnchorGrid from '../AnchorGrid.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';
  import TimeRangeFields from './TimeRangeFields.svelte';

  interface Props {
    o: TextOverlayCfg;
    index: number;
    count: number;
    info: ProbeInfo;
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
  }
  let { o, index, count, info, initialOpen = false }: Props = $props();

  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  // The expanded card's box is highlighted on the preview. Only `open` is
  // tracked: the selection is mirrored on open / collapse transitions
  // (overlay.selectionAfterToggle), never re-applied when the selection
  // itself changes — a collapsed card used to read selectedOverlay here and
  // reset what addOverlay or a box click on the preview had just selected.
  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let wasOpen = initialOpen;
  $effect(() => {
    const now = open;
    untrack(() => {
      app.ui.selectedOverlay = selectionAfterToggle(app.ui.selectedOverlay, o.id, now, wasOpen);
      wasOpen = now;
    });
  });

  // The font picker exists only where the server can list faces
  // (features.fonts: GET /api/fonts is non-empty); otherwise the face is
  // fixed at the bundled default and shown read-only.
  const fontsOn = $derived(caps.features.fonts);
  let fonts = $state<string[]>([TEXT_DEFAULTS.font]);
  $effect(() => {
    if (fontsOn) void loadFontFamilies().then((list) => (fonts = list));
  });
  // A font the list does not know (e.g. a loaded recipe) stays selectable.
  const fontOptions = $derived(fonts.includes(o.font) || !o.font ? fonts : [o.font, ...fonts]);

  const title = $derived(`Text — ${overlayLabel(o)}`);
  // Text is drawn on the OUTPUT canvas (after crop / resize / the Output
  // card's width-height fit) — the size hint is relative to that canvas, the
  // same one the preview's drag boxes live on; it is omitted while the
  // canvas cannot be known client-side (auto-crop on without both output
  // dimensions).
  const canvas = $derived(planCanvas(info, effectiveOps(app.ops, app.output), app.output));
  /** the font size as a percentage of the output canvas height (the hint) */
  const sizePct = $derived(round((o.size / Math.max(1, canvas?.h ?? 1)) * 100, 1));
  const summary = $derived(
    `${o.size} px · ${o.font || TEXT_DEFAULTS.font} · ${o.anchor} ${o.x},${o.y}${o.start > 0 || o.end > 0 ? ` · ${o.start.toFixed(2)} → ${o.end > 0 ? o.end.toFixed(2) + ' s' : 'end'}` : ''}`,
  );

  /** setAnchor changes the anchor while keeping the element where it is (X/Y move to the new anchor point). */
  function setAnchor(a: Anchor) {
    const box = overlayBox(o);
    const p = anchorPoint(a, box.left, box.top, box.w, box.h);
    o.anchor = a;
    o.x = Math.round(p.x);
    o.y = Math.round(p.y);
  }
  const hex = (s: string) => s.trim().replace(/^#/, '').toLowerCase();
  const validHex = (s: string) => /^[0-9a-f]{6}([0-9a-f]{2})?$/.test(s);
  function setColor(v: string) {
    const h = hex(v);
    if (validHex(h)) o.color = h;
  }
  function setBorderColor(v: string) {
    const h = hex(v);
    if (validHex(h)) o.borderColor = h;
  }
  function setBoxColor(v: string) {
    const h = hex(v);
    if (validHex(h)) o.boxColor = h;
  }
  /** the picker only handles RRGGBB: drop an alpha suffix for it (pickerColor puts it back on a pick) */
  const rgb6 = (s: string) => '#' + s.slice(0, 6);
</script>

<OpCard {title} {summary} bind:enabled={o.enabled} bind:open>
  {#snippet actions()}
    <button type="button" class="sm ghost" onclick={() => moveOverlay(o.id, -1)} disabled={index <= 0} title="Draw earlier (move up)" aria-label="Move up">▲</button>
    <button type="button" class="sm ghost" onclick={() => moveOverlay(o.id, 1)} disabled={index >= count - 1} title="Draw later / on top (move down)" aria-label="Move down">▼</button>
    <button type="button" class="sm ghost" onclick={() => removeOverlay(o.id)} title="Remove this overlay" aria-label="Remove overlay">✕</button>
  {/snippet}
  <div class="row">
    <label class="field grow">
      <span>Text (newlines allowed)</span>
      <textarea rows="2" bind:value={o.text} spellcheck="true"></textarea>
    </label>
  </div>
  <div class="row">
    {#if fontsOn}
      <label class="field">
        <span>Font</span>
        <select bind:value={o.font}>
          {#each fontOptions as f (f)}<option value={f}>{f}</option>{/each}
        </select>
      </label>
    {:else}
      <label class="field">
        <span>Font (fixed)</span>
        <input type="text" class="font-fixed" value={o.font || TEXT_DEFAULTS.font} readonly aria-label="Font (fixed)" title="This server lists no fonts — the bundled DejaVu Sans is used" />
      </label>
      <span class="hint">This server has no font list (fc-list unavailable), so only {TEXT_DEFAULTS.font} is offered.</span>
    {/if}
    <label class="field"><span>Size (px)</span><NumField bind:value={o.size} min={4} max={1024} small /></label>
    <span class="field">
      <span>Colour</span>
      <span class="row tight">
        <input type="color" value={rgb6(o.color)} oninput={(e) => setColor(pickerColor(e.currentTarget.value, o.color))} aria-label="Text colour" />
        <input type="text" class="hex mono" value={'#' + o.color} onchange={(e) => setColor(e.currentTarget.value)} maxlength="9" spellcheck="false" aria-label="Text colour hex (RRGGBB or RRGGBBAA)" />
      </span>
    </span>
  </div>
  <div class="row">
    <label class="field"><span>Outline (px)</span><NumField bind:value={o.border} min={0} max={64} small /></label>
    <span class="field">
      <span>Outline colour</span>
      <span class="row tight">
        <input type="color" value={rgb6(o.borderColor)} oninput={(e) => setBorderColor(pickerColor(e.currentTarget.value, o.borderColor))} disabled={o.border <= 0} aria-label="Outline colour" />
        <input type="text" class="hex mono" value={'#' + o.borderColor} onchange={(e) => setBorderColor(e.currentTarget.value)} disabled={o.border <= 0} maxlength="9" spellcheck="false" aria-label="Outline colour hex" />
      </span>
    </span>
    <label class="inline"><input type="checkbox" bind:checked={o.box} /><span>Box</span></label>
    <span class="field">
      <span>Box colour (RRGGBBAA)</span>
      <span class="row tight">
        <input type="color" value={rgb6(o.boxColor)} oninput={(e) => setBoxColor(pickerColor(e.currentTarget.value, o.boxColor))} disabled={!o.box} aria-label="Box colour" />
        <input type="text" class="hex mono" value={'#' + o.boxColor} onchange={(e) => setBoxColor(e.currentTarget.value)} disabled={!o.box} maxlength="9" spellcheck="false" aria-label="Box colour hex" />
      </span>
    </span>
    <label class="field"><span>Box padding</span><NumField bind:value={o.boxPad} min={1} max={256} small disabled={!o.box} /></label>
  </div>
  <div class="row">
    <span class="field">
      <span>Anchor</span>
      <AnchorGrid value={o.anchor} onchange={setAnchor} />
    </span>
    <label class="field"><span>X (px)</span><NumField bind:value={o.x} min={-8192} max={8192} small /></label>
    <label class="field"><span>Y (px)</span><NumField bind:value={o.y} min={-8192} max={8192} small /></label>
    <span class="hint">Output-canvas pixels — or drag the box on the preview.</span>
  </div>
  <TimeRangeFields {o} {info} />
  <p class="hint">
    Rendered with drawtext from a text file (no escaping issues); the colour takes an alpha suffix (#ffffff80).
    {#if fontsOn}Fonts come from the server's font list ({TEXT_DEFAULTS.font} is always there).{/if}
    {#if canvas}Size is {sizePct}% of the {canvas.h} px output height — check the result at Discord's display size.{/if}
  </p>
</OpCard>

<style>
  .row.tight {
    gap: 6px;
    flex-wrap: nowrap;
  }
  .field.grow {
    flex: 1 1 100%;
  }
  textarea {
    background: var(--bg);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 5px 7px;
    font: inherit;
    font-size: 13px;
    resize: vertical;
    min-height: 48px;
    width: 100%;
  }
  textarea:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
  .hex {
    width: 96px;
  }
  .font-fixed {
    width: 140px;
    color: var(--muted);
    cursor: default;
  }
</style>
