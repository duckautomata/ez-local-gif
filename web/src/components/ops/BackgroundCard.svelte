<script lang="ts">
  // Background removal: AI (Phase 5b) keys with the matte sidecar's model
  // (op "matte" — the Model select is fed by GET /api/matte: ids, labels,
  // live states, the server's default; the status line says ready / GPU /
  // the clip's estimate, loading, downloading, or why the sidecar is
  // unavailable); Colour (Phase 5a) keys one or more RGB colours — one op
  // "colorkey" per row, the eyedropper's pick from the preview still or a
  // typed hex; Screen keys a green / blue screen in YUV (op "chromakey" plus
  // despill). The Edge cleanup fold (op "morph": fill pinholes / grow the
  // matte) follows whichever key is on. Sliders re-render the still. The AI
  // mode is disabled with the reason on a server without features.matte (a
  // plain install without the compose profile).
  //
  // Phase 5c: nothing in the AI mode starts a pass — selecting AI, a model
  // or a device shows the last picture with a "not computed" pill; the
  // Compute matte button (or Render) starts it, and leaving the AI mode
  // releases the sidecar's models (lib/matte.svelte trackAiMode). "Run on"
  // picks the device server-side (PUT /api/matte/settings; shown only when
  // the sidecar offers several), the Model select defaults to that device's
  // default, Stabilise (light by default) post-processes the matte sequence
  // on the server from the cached matte (no model run), and the Advanced
  // fold's Keep colours force picked colours opaque (in-graph, instant).
  // The guided model (Part B) adds the Select subject panel: the preview
  // becomes a prompt canvas (box drag, +/− clicks, live mask), a keyframe
  // strip lists the prompted frames, Edge picks the per-frame model that
  // refines the tracker's edge band.
  //
  // Phase 5d: "Use this frame's matte" makes the scrubber's frame a MASK
  // prompt — the edge model's per-frame matte of it starts the track (the
  // prototype's most reliable prompt: IoU 0.997, no drawing). It is
  // enabled when that matte is known to exist for this clip (the Preview /
  // the overlay note what the server answered, lib/matte.svelte matteMemo)
  // or nothing is known yet (the overlay's answer then says), disabled
  // when the server said it is not computed or Edge is None (no edge
  // model to take it from). The loose-box guard's warning (set by the
  // overlay, lib/maskguard) shows under the panel.
  import { untrack } from 'svelte';
  import { MATTE_EDGE_NONE, MATTE_MODEL_DEFAULT, MAX_MATTE_KEEP } from '../../lib/api';
  import { caps } from '../../lib/capabilities.svelte';
  import { normalizeHex } from '../../lib/format';
  import {
    computeButtonText,
    defaultEdgeFor,
    deviceLabel,
    edgeOptions,
    effectiveDevice,
    matteDevices,
    matteEstimate,
    matteMemoKey,
    matteModel,
    matteModelFor,
    matteModelOptions,
    matteStatusLine,
    MATTE_PROFILE_HINT,
    modelLabel,
    shortModelLabel,
    STABILISE_OPTIONS,
    stabiliseLabel,
  } from '../../lib/matte';
  import { computeMatte, holdMattePolling, matte, matteMemoState, refreshMatte, setMatteDevice, trackAiMode } from '../../lib/matte.svelte';
  import { canPromptFrame, framePrompts, hasMask, keyframes, MAX_PROMPT_FRAMES, promptSummary } from '../../lib/prompts';
  import {
    addKeepColor,
    addKeyColor,
    app,
    armEyedropper,
    armKeepEyedropper,
    CHROMA_BLUE,
    CHROMA_GREEN,
    clearFramePrompts,
    clearMattePrompts,
    disarmEyedropper,
    edgeIsNone,
    effectiveOps,
    fillMatteDefault,
    forwardFrame,
    guidedNeedsPrompts,
    isGuided,
    keepColors,
    matteClipKey,
    MAX_KEY_COLORS,
    MORPH_MAX_GROW,
    planFrames,
    promptGridKey,
    prunePrompts,
    removeKeepColor,
    removeKeyColor,
    scrubFrameFor,
    setBackgroundMode,
    setFrameMaskPrompt,
    setKeepColor,
    setKeyColor,
    setMatteEdge,
    setMatteModel,
    setMatteStabilise,
    STABILISE_DEFAULT,
    type BackgroundMode,
    type ScreenColor,
  } from '../../lib/state.svelte';
  import { toast } from '../../lib/toast.svelte';
  import NumField from '../NumField.svelte';
  import OpCard from '../OpCard.svelte';
  import ColourRows from './ColourRows.svelte';

  interface Props {
    /** start expanded (tests render the body server-side) */
    initialOpen?: boolean;
    /** offer the preview eyedropper / the prompt canvas / the Compute button; false in batch (no preview — rows render) */
    picker?: boolean;
  }
  let { initialOpen = false, picker = true }: Props = $props();

  // svelte-ignore state_referenced_locally -- the prop only seeds the initial state
  let open = $state(initialOpen);
  let advOpen = $state(false);
  let edgeOpen = $state(false);
  const bg = $derived(app.ops.background);
  type ModeId = BackgroundMode | 'none';
  const modes: { id: ModeId; label: string; title: string }[] = [
    { id: 'none', label: 'None', title: 'Keep the background' },
    { id: 'ai', label: 'AI', title: 'Cut the subject out with the AI matte of the matte sidecar (any background; computed once per frame and cached)' },
    { id: 'colour', label: 'Colour', title: 'Key out one or more RGB colours — picked from the preview or typed as hex' },
    { id: 'screen', label: 'Screen', title: 'Key out a green or blue screen in YUV with despill' },
  ];
  const current = $derived<ModeId>(bg.enabled ? bg.mode : 'none');
  const ai = $derived(bg.enabled && bg.mode === 'ai');
  const screen = $derived(bg.enabled && bg.mode === 'screen');
  const colour = $derived(bg.enabled && bg.mode === 'colour');
  const picking = $derived(app.ui.pickColor);
  const pickRow = $derived(app.ui.pickRow);
  const pickTarget = $derived(app.ui.pickTarget);
  /** the rows that carry a colour (the ones that emit an op) */
  const picked = $derived(bg.colors.filter((c) => c !== ''));
  /** the Screen sub-choice's preset colour; anything else in the key colour field is custom */
  const screenPreset = $derived(bg.screen === 'blue' ? CHROMA_BLUE : CHROMA_GREEN);
  // An older server (features.keying off) rejects the chromakey / colorkey
  // ops: the card stays, with a notice.
  const supported = $derived(caps.features.keying);
  // The Edge cleanup fold is gated on features.morph: an older server 400s
  // on the morph op, and fill pinholes is on by default, so on such a server
  // the fold is hidden AND the cleanup switched off — a default must never
  // get a render rejected. (Until the server answers the flags are
  // optimistic and nothing is touched.)
  const morphSupported = $derived(caps.features.morph);
  $effect(() => {
    if (caps.loaded && !caps.features.morph && (bg.morph.close || bg.morph.grow > 0)) app.ops.background.morph = { close: false, grow: 0 };
  });

  // ---- AI mode (Phase 5b). features.matte is true only while the app's
  // probe of the sidecar answers: off, the AI segment is disabled with the
  // reason (the /api/matte reason when it can be read — "no matte service
  // is configured (EZLG_MATTE_URL is empty …)" — else the standard note).
  // The live states come from GET /api/matte (lib/matte.svelte.ts), polled
  // while the AI body is on screen; the Render panel polls while AI is on
  // and the card is collapsed.
  const aiSupported = $derived(caps.features.matte);
  const aiReason = $derived.by(() => {
    if (aiSupported) return '';
    const r = matte.status?.reason?.trim();
    return r ? `AI matte is off on this server: ${r}` : `This server has no AI matte sidecar (features.matte off) — run \`${MATTE_PROFILE_HINT}\` next to it, or update the server`;
  });
  $effect(() => {
    if (open && ai) return holdMattePolling();
  });
  // The reason for a disabled AI mode is worth one read of /api/matte when
  // the card opens (no poll: nothing changes while the feature is off).
  $effect(() => {
    if (open && caps.loaded && !caps.features.matte && !matte.loaded) void refreshMatte();
  });
  // Leaving the AI mode (or switching the card off) releases the sidecar's
  // resident models after a short debounce (Phase 5c: nothing stays loaded
  // while AI is not in use); the card going away counts as leaving.
  trackAiMode(() => ai);
  /** the model the select shows and the op names: the choice, else the server's default, else the recipe default */
  const model = $derived(matteModelFor(bg.ai.model, matte.status));
  // As soon as the server's default (for the effective device) is known it
  // becomes the explicit choice, so what the select shows is what the op
  // sends (state.AiCfg); a model the user picked stays across a device
  // change, an auto-filled one follows the new device's default.
  $effect(() => {
    const d = matte.status?.defaultModel?.trim();
    if (d) fillMatteDefault(d);
  });
  const device = $derived(effectiveDevice(matte.status));
  const devices = $derived(matteDevices(matte.status));
  const options = $derived(matteModelOptions(matte.status, device));
  /** the chosen model is not among the offered ones (the server will refuse it): shown as an extra, flagged option */
  const unlisted = $derived(!options.some((o) => o.id === model));
  const guided = $derived(ai && isGuided(bg));
  /** the forward frame count of the clip (the matte pass runs on the temporal prefix, before a bounce doubles it) */
  const aiFrames = $derived.by(() => {
    const src = app.source;
    if (!src) return 0;
    const ops = effectiveOps(app.ops, app.output);
    const n = planFrames(src.info, ops, app.output);
    return ops.bounce && !src.info.isStill && n >= 2 ? n / 2 : n;
  });
  const aiEstimate = $derived(ai ? matteEstimate(matte.status, model, aiFrames) : null);
  // The guided model's prompts are OUTPUT frame indices on the matte
  // plan's forward grid: a change of the source, the trim start, the
  // delay, the speed or the plan fps moves every frame under them (the box
  // drawn on old frame 0 would condition old frame 10), and a shorter clip
  // leaves the frames past its end with nothing to prompt — so they are
  // dropped, with a toast, rather than tracking the wrong pictures in
  // silence. The trim end, crop, reverse and bounce leave the grid alone.
  let promptGrid: string | undefined;
  $effect(() => {
    const key = promptGridKey(app.source, app.ops, app.output);
    const frames = aiFrames;
    untrack(() => {
      const moved = promptGrid !== undefined && key !== promptGrid;
      promptGrid = key;
      const dropped = prunePrompts(moved, frames);
      if (dropped > 0 && guided) {
        toast.info(
          moved
            ? 'Guided prompts cleared: the trim, speed or fps change moved the frames they were drawn on — select the subject again'
            : `${dropped} guided ${dropped === 1 ? 'prompt' : 'prompts'} past the clip’s end dropped`,
        );
      }
    });
  });
  /** in batch: the per-frame model the rows key with when the editor's choice was the guided model (batchOpsCfg) */
  const batchFallback = $derived.by(() => {
    const id = matteModelFor('', matte.status);
    return modelLabel(id, matteModel(matte.status, id));
  });
  const statusLine = $derived.by(() => {
    if (matte.status) return matteStatusLine(matte.status, model, aiEstimate?.ms ?? 0);
    if (matte.error) return `matte status unavailable — ${matte.error}`;
    return matteStatusLine(null, model);
  });
  const aiLabel = $derived(modelLabel(model, matteModel(matte.status, model)));
  // The Compute matte button reflects the preview's compute state
  // (lib/matte.svelte: the Preview publishes idle / running / computed for
  // the still on its stage); in batch there is no preview and rows render.
  const compute = $derived(computeButtonText(matte.compute));
  const needsPrompts = $derived(ai && guidedNeedsPrompts(app.ops));
  /** the guided panel is open with a subject selected: the stage shows the unkeyed frame, so Compute closes the panel and runs the keyed still eagerly */
  const promptReady = $derived(ai && picker && isGuided(bg) && app.ui.promptOpen && !needsPrompts);
  const computeDisabled = $derived(needsPrompts || (compute.disabled && !promptReady));
  const computeHint = $derived.by(() => {
    if (needsPrompts) return 'select the subject first';
    if (promptReady) return 'closes the panel and runs the track (and the edge pass)';
    switch (matte.compute.state) {
      case 'idle':
        return 'not computed for this clip, model and device yet — previews show the last picture until then';
      case 'running':
        return matte.compute.total > 0 ? `${matte.compute.done} of ${matte.compute.total} frames` : 'the pass is running';
      case 'computed':
        return 'cached — trim, speed and fps changes re-use it';
      default:
        return '';
    }
  });
  const stabilise = $derived(bg.ai.stabilise);
  const keepRows = $derived(bg.ai.keep);
  const keepCount = $derived(keepColors(bg).length);
  const keepArmed = $derived(picking && pickTarget === 'keep' ? pickRow : -1);
  const colourArmed = $derived(picking && pickTarget === 'colour' ? pickRow : -1);
  // ---- guided model (Part B)
  const prompts = $derived(bg.ai.prompts);
  const promptFrames = $derived(keyframes(prompts));
  /** the prompted frame the scrubber is on (the forward grid index) */
  const currentFrame = $derived(app.source ? forwardFrame(app.source.info, app.ops, app.output, app.ui.scrubFrame) : 0);
  const edgeOpts = $derived(edgeOptions(matte.status, device));
  /** what the Edge select shows: the explicit choice, else the device's default per-frame model */
  const edgeValue = $derived(bg.ai.edge || defaultEdgeFor(matte.status, device));
  // ---- "Use this frame's matte" (Phase 5d)
  const edgeNone = $derived(edgeIsNone(edgeValue));
  const edgeLabel = $derived(edgeNone ? '' : modelLabel(edgeValue, matteModel(matte.status, edgeValue)));
  /** "General" / "Anime": the edge model's label without its qualifier, for the help text and the hints */
  const edgeShort = $derived(shortModelLabel(edgeLabel));
  /** the scrubber's frame is the set's mask prompt already (the button then takes it off) */
  const currentHasMask = $derived(hasMask(framePrompts(prompts, currentFrame)));
  /** what the server was seen to say about the edge model's matte of this clip */
  const edgeMemo = $derived(edgeNone ? 'unknown' : matteMemoState(matteMemoKey(matteClipKey(app.source, app.ops, app.output), edgeValue, device)));
  const useMatte = $derived.by(() => {
    if (currentHasMask) return { text: 'Drop this frame’s matte', disabled: !picker, title: 'Take the matte prompt off this frame (its box and clicks stay)' };
    const text = 'Use this frame’s matte';
    if (!picker || !app.source) return { text, disabled: true, title: 'Needs the editor’s preview' };
    if (edgeNone) return { text, disabled: true, title: 'Needs an edge model: Edge is None — tracker mask only' };
    if (!canPromptFrame(prompts, currentFrame)) return { text, disabled: true, title: `At most ${MAX_PROMPT_FRAMES} prompted frames` };
    if (edgeMemo === 'idle') {
      return { text, disabled: true, title: `${edgeShort} matte not computed for this clip: switch the Model to ${edgeLabel}, press Compute, then come back` };
    }
    const known = edgeMemo === 'computed' ? '' : ' (if that matte is computed for this clip — the overlay says otherwise)';
    return { text, disabled: false, title: `Start the track from ${edgeShort}’s matte of frame ${currentFrame + 1} — the most reliable start; replaces the box and clicks on it${known}` };
  });
  function useFrameMatte() {
    setFrameMaskPrompt(currentFrame, !currentHasMask);
  }
  function onEdge(id: string) {
    if (setMatteEdge(id)) toast.info('Edge None uses the tracker’s mask alone — the frame-matte prompt was dropped (its box and clicks stay)');
  }
  /** the help text's lead-in: the frame matte first (the most reliable start) when an edge model can provide one */
  const drawHint = $derived(
    edgeNone ? 'Draw' : `Scrub to a frame where ${edgeShort} got it right and press Use this frame’s matte — the most reliable start; refine with − clicks. Otherwise draw`,
  );

  async function onDevice(e: Event & { currentTarget: HTMLSelectElement }) {
    const select = e.currentTarget;
    const err = await setMatteDevice(select.value);
    if (err) {
      // The server refused (the sidecar no longer offers it): the control
      // goes back to the effective device, which did not change, instead
      // of claiming the one the server refused.
      select.value = device;
      toast.error(`Run on: ${err}`);
    }
  }
  function togglePrompt() {
    app.ui.promptOpen = !app.ui.promptOpen;
    if (app.ui.promptOpen) disarmEyedropper();
  }
  function jumpTo(frame: number) {
    const src = app.source;
    if (!src) return;
    app.ui.scrubFrame = scrubFrameFor(src.info, app.ops, app.output, frame);
    app.ui.promptOpen = true;
  }

  const morphSummary = $derived.by(() => {
    const parts: string[] = [];
    if (bg.morph.close) parts.push('fill pinholes');
    if (bg.morph.grow > 0) parts.push(`grow ${bg.morph.grow} px`);
    return parts.join(' · ');
  });
  const summary = $derived.by(() => {
    if (!bg.enabled) return supported ? 'off' : 'off — not supported by this server';
    const tail = morphSummary ? ` · ${morphSummary}` : '';
    if (bg.mode === 'ai') {
      const which = model !== MATTE_MODEL_DEFAULT ? ` · ${aiLabel}` : '';
      const extras: string[] = [];
      if (isGuided(bg)) extras.push(promptSummary(bg.ai.prompts));
      if (stabilise !== STABILISE_DEFAULT) extras.push(stabilise ? `stabilise ${stabiliseLabel(stabilise).toLowerCase()}` : 'stabilise off');
      if (keepCount) extras.push(`keep ${keepCount} ${keepCount === 1 ? 'colour' : 'colours'}`);
      const more = extras.length ? ` · ${extras.join(' · ')}` : '';
      return `AI${which}${aiSupported ? '' : ' — not available on this server'}${more}${tail}`;
    }
    if (bg.mode === 'colour') {
      if (!picked.length) return picker ? 'pick a colour on the preview' : 'type a colour to remove';
      const what = picked.length === 1 ? `colour #${picked[0]}` : `${picked.length} colours`;
      return `${what} · similarity ${bg.pickSimilarity.toFixed(2)}${tail}`;
    }
    const custom = bg.color !== screenPreset ? ` #${bg.color}` : '';
    return `${bg.screen}screen${custom} · similarity ${bg.similarity.toFixed(2)}${bg.despill ? '' : ' · despill off'}${tail}`;
  });

  // The segment and the header checkbox go through the same setter
  // (lib/state.setBackgroundMode): Colour without a pick yet arms the
  // eyedropper for the first row straight away, so enabling the card by its
  // checkbox is as quick as clicking "Colour".
  function setMode(m: ModeId) {
    setBackgroundMode(m, { picker });
  }
  function setScreen(s: ScreenColor) {
    app.ops.background.screen = s;
    app.ops.background.color = s === 'blue' ? CHROMA_BLUE : CHROMA_GREEN;
  }

  // The eyedropper only makes sense while the card is in Colour mode (a
  // Colour row) or in AI mode (a Keep colours row).
  $effect(() => {
    const ok = app.ui.pickTarget === 'keep' ? bg.enabled && bg.mode === 'ai' : bg.enabled && bg.mode === 'colour';
    if (app.ui.pickColor && !ok) disarmEyedropper();
  });

  function setChromaColor(hex: string) {
    const n = normalizeHex(hex);
    if (n) app.ops.background.color = n;
  }
  function toggleEyedropper(i: number) {
    if (picking && pickTarget === 'colour' && pickRow === i) disarmEyedropper();
    else armEyedropper(i);
  }
  function addColour() {
    const i = addKeyColor();
    if (i >= 0 && picker) armEyedropper(i);
  }
  function toggleKeepEyedropper(i: number) {
    if (picking && pickTarget === 'keep' && pickRow === i) disarmEyedropper();
    else armKeepEyedropper(i);
  }
  function addKeep() {
    const i = addKeepColor();
    if (i >= 0 && picker) armKeepEyedropper(i);
  }
</script>

<OpCard title="Background" {summary} bind:enabled={() => bg.enabled, (v: boolean) => setMode(v ? bg.mode : 'none')} bind:open>
  {#if !supported}
    <p class="note">This server does not support background removal (an older ezlg) — the keying op will be rejected at render; update the server.</p>
  {/if}
  <div class="row">
    <span class="field">
      <span>Mode</span>
      <span class="seg" role="group" aria-label="Background mode">
        {#each modes as m (m.id)}
          <button
            type="button"
            aria-pressed={current === m.id}
            title={m.id === 'ai' && !aiSupported ? aiReason : m.title}
            disabled={m.id === 'ai' && !aiSupported}
            onclick={() => setMode(m.id)}
          >
            {m.label}
          </button>
        {/each}
      </span>
    </span>
    {#if screen}
      <span class="field">
        <span>Screen</span>
        <span class="seg" role="group" aria-label="Screen colour">
          <button type="button" aria-pressed={bg.screen === 'green'} title="Key out green (#00ff00)" onclick={() => setScreen('green')}>Green</button>
          <button type="button" aria-pressed={bg.screen === 'blue'} title="Key out blue (#0000ff)" onclick={() => setScreen('blue')}>Blue</button>
        </span>
      </span>
    {/if}
  </div>

  {#if ai}
    {#if !aiSupported}
      <p class="note">{aiReason} — the matte op will be rejected at render.</p>
    {/if}
    <div class="row">
      <!-- Run on comes first: the Model default follows the device (an unchosen model moves to the new device's default), so the dependency reads left to right -->
      {#if devices.length > 1}
        <label class="field">
          <span>Run on</span>
          <select aria-label="Run on" value={device} disabled={matte.settingDevice} onchange={onDevice} title="Which device the sidecar runs the pass on — a server-side setting, not part of the recipe; the Model default follows it">
            {#each devices as d (d)}
              <option value={d} selected={d === device}>{deviceLabel(d)}</option>
            {/each}
          </select>
        </label>
      {/if}
      <label class="field">
        <span>Model</span>
        <select class="model" aria-label="Model" value={model} onchange={(e) => setMatteModel(e.currentTarget.value)}>
          {#each options as o (o.id)}
            <option value={o.id} selected={o.id === model} disabled={o.disabled || (o.tracker && !picker)} title={o.tracker && !picker ? 'The guided model needs the editor’s preview to select the subject' : o.reason || undefined}>
              {o.text}
            </option>
          {/each}
          {#if unlisted}
            <option value={model} selected>{model}{options.length ? ' — not offered by this server' : ''}</option>
          {/if}
        </select>
      </label>
      <label class="field">
        <span>Stabilise</span>
        <select aria-label="Stabilise" value={stabilise} onchange={(e) => setMatteStabilise(e.currentTarget.value)} title="Temporal smoothing of the matte sequence, derived on the server from the cached matte (no model run)">
          {#each STABILISE_OPTIONS as o (o.id)}
            <option value={o.id} selected={o.id === stabilise} title={o.hint}>{o.label}</option>
          {/each}
        </select>
      </label>
      <span class="field status-field">
        <span>Status</span>
        <span class="status" class:bad={!aiSupported || matte.status?.device === 'unavailable'} role="status">{statusLine}</span>
      </span>
    </div>
    {#if !picker && isGuided(bg)}
      <p class="note">Guided needs the editor’s preview to select the subject — batch rows key with {batchFallback} instead (no prompts, no edge model).</p>
    {/if}
    {#if picker}
      <div class="row compute">
        <button
          type="button"
          class:primary={!computeDisabled}
          disabled={computeDisabled}
          onclick={() => computeMatte()}
          title={promptReady ? 'Close the subject panel and run the guided matte for this clip now — Render runs it anyway' : compute.title}
          aria-label="Compute matte"
        >
          {promptReady ? 'Compute matte' : compute.text}
        </button>
        {#if computeHint}<span class="hint">{computeHint}</span>{/if}
      </div>
    {/if}

    {#if guided}
      <div class="guided">
        <div class="row">
          <button
            type="button"
            class="sm"
            class:primary={app.ui.promptOpen}
            aria-pressed={app.ui.promptOpen}
            disabled={!picker}
            onclick={togglePrompt}
            title={picker ? 'Draw the box and the +/− clicks on the preview' : 'Needs the editor’s preview'}
          >
            {app.ui.promptOpen ? 'Done selecting' : 'Select subject'}
          </button>
          <button type="button" class="sm" class:ghost={currentHasMask} disabled={useMatte.disabled} onclick={useFrameMatte} title={useMatte.title}>
            {useMatte.text}
          </button>
          <label class="field">
            <span>Edge</span>
            <select aria-label="Edge" value={edgeValue} onchange={(e) => onEdge(e.currentTarget.value)} title="The per-frame model that refines the tracker's edge band (the tracker is coarse at edges)">
              {#each edgeOpts as o (o.id)}
                <option value={o.id} selected={o.id === edgeValue} disabled={o.disabled} title={o.reason || undefined}>{o.text}</option>
              {/each}
            </select>
          </label>
          <button type="button" class="sm ghost" onclick={clearMattePrompts} disabled={!prompts.length} title="Forget every box and click">Clear</button>
        </div>
        {#if promptFrames.length}
          <div class="keyframes" role="list" aria-label="Prompted frames">
            {#each promptFrames as k (k.frame)}
              <span class="keyframe" role="listitem" class:current={k.frame === currentFrame}>
                <button type="button" class="sm" onclick={() => jumpTo(k.frame)} title="Show this frame on the preview" aria-label="Go to prompted frame {k.frame + 1}">
                  f {k.frame + 1}{k.mask ? ' ▣' : ''}{k.box ? ' ▭' : ''}{k.positive ? ` +${k.positive}` : ''}{k.negative ? ` −${k.negative}` : ''}
                </button>
                <button type="button" class="sm ghost" onclick={() => clearFramePrompts(k.frame)} aria-label="Delete the prompts of frame {k.frame + 1}" title="Delete this frame's prompts">×</button>
              </span>
            {/each}
          </div>
        {:else}
          <p class="hint">No subject selected yet{picker ? ' — open Select subject and draw a box around it on the preview' : ''}; no matte is applied until then.</p>
        {/if}
        {#if app.ui.promptWarning}
          <p class="warn" role="alert">{app.ui.promptWarning}</p>
        {/if}
        <p class="hint">
          {drawHint} a box around the character — a single click usually selects only a part (skin, a sleeve) or floods the frame when it
          lands beside the subject, so click it 2–3 times if you click — add a − click on anything that stays; then Compute. Click on
          another frame where it drifts and Compute again. Edge {edgeValue === MATTE_EDGE_NONE ? 'None uses the tracker’s mask alone (and offers no frame matte to start from)' : 'refines the band around the tracked outline with the per-frame model'}.
        </p>
      </div>
    {/if}

    <details class="adv" bind:open={advOpen}>
      <summary>
        <span class="sum">Advanced</span>{#if !advOpen}<span class="muted small">· keep {keepCount ? `${keepCount} ${keepCount === 1 ? 'colour' : 'colours'} · similarity ${bg.ai.keepSimilarity.toFixed(2)}` : 'none'}</span>{/if}
      </summary>
      <div class="row">
        <ColourRows
          colors={keepRows}
          {picker}
          armedRow={keepArmed}
          max={MAX_MATTE_KEEP}
          caption={keepRows.length > 1 ? 'Keep colours' : 'Keep colour'}
          hexLabel={'Keep colour {i} (hex)'}
          removeLabel={'Remove keep colour {i}'}
          addTitle="Force another colour opaque — up to {MAX_MATTE_KEEP}"
          emptyHint={picker ? 'pick a colour the model drops' : 'type a hex value'}
          onPick={toggleKeepEyedropper}
          onHex={setKeepColor}
          onRemove={removeKeepColor}
          onAdd={addKeep}
        />
        <label class="field slider">
          <span>Keep similarity (0.01–1) — <b>{bg.ai.keepSimilarity.toFixed(2)}</b></span>
          <span class="row tight">
            <input type="range" min="0.01" max="1" step="0.01" bind:value={app.ops.background.ai.keepSimilarity} aria-label="Keep similarity" />
            <NumField bind:value={app.ops.background.ai.keepSimilarity} min={0.01} max={1} step={0.01} small />
          </span>
        </label>
      </div>
      <p class="hint">
        Keep colours force every pixel of that colour (within the similarity) opaque, on top of the matte — for a prop or a
        flat-coloured part the model drops. Applied in the graph: it shows at once, no pass needed.
      </p>
    </details>
    <p class="hint">
      Computed once per frame and cached; trim, speed and fps changes re-use what is cached. General (precise) keeps thin strands
      and props and is stable on video; Anime (fast) is for anime-style characters only. If parts of the subject drop out, add a
      Keep colour or raise Grow; if edges flicker, set Stabilise (Light removes single-frame pops, Strong keeps parts that drop
      out for a frame at the cost of a short trail on fast motion). On frames that already carry transparency the matte is
      intersected with it, never substituted. Soft edges need WebP / AVIF / APNG output — GIF cuts them to 1-bit alpha.
    </p>
  {:else if screen}
    <div class="row">
      <span class="field">
        <span>Key colour</span>
        <span class="row tight">
          <input type="color" value={'#' + bg.color} oninput={(e) => setChromaColor(e.currentTarget.value)} aria-label="Key colour" />
          <input type="text" class="hex mono" value={'#' + bg.color} onchange={(e) => setChromaColor(e.currentTarget.value)} maxlength="7" spellcheck="false" aria-label="Key colour hex" />
          {#if bg.color !== screenPreset}<span class="hint">custom — despill follows the dominant channel</span>{/if}
        </span>
      </span>
      <label class="field slider">
        <span>Similarity (0.01–1) — <b>{bg.similarity.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0.01" max="1" step="0.01" bind:value={app.ops.background.similarity} aria-label="Similarity" />
          <NumField bind:value={app.ops.background.similarity} min={0.01} max={1} step={0.01} small />
        </span>
      </label>
      <label class="field slider">
        <span>Blend (0.01–1) — <b>{bg.blend.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0.01" max="1" step="0.01" bind:value={app.ops.background.blend} aria-label="Blend" />
          <NumField bind:value={app.ops.background.blend} min={0.01} max={1} step={0.01} small />
        </span>
      </label>
    </div>
    <details class="adv" bind:open={advOpen}>
      <summary><span class="sum">Advanced</span>{#if !advOpen}<span class="muted small">· despill {bg.despill ? `on · mix ${bg.despillMix.toFixed(2)} · expand ${bg.despillExpand.toFixed(2)}` : 'off'}</span>{/if}</summary>
      <div class="row">
        <label class="inline" title="Remove the key colour's spill from edges and semi-transparent pixels">
          <input type="checkbox" bind:checked={app.ops.background.despill} /><span>Despill</span>
        </label>
        <label class="field">
          <span>Mix (0–1)</span>
          <NumField bind:value={app.ops.background.despillMix} min={0} max={1} step={0.05} disabled={!bg.despill} small />
        </label>
        <label class="field">
          <span>Expand (0–1)</span>
          <NumField bind:value={app.ops.background.despillExpand} min={0} max={1} step={0.05} disabled={!bg.despill} small />
        </label>
      </div>
    </details>
    <p class="hint">
      Keys in YUV 4:4:4 at full resolution, before any scaling (soft edges survive). Similarity widens the keyed range;
      blend softens the cut-off. Despill removes the green / blue cast from edge pixels. The key judges each pixel by its
      3×3 neighbourhood, so at the default 0.10 a one-pixel rim of screen colour can stay opaque along the hard edges of a
      strongly coloured subject — around 0.15 keys it softly.
    </p>
  {:else if colour}
    <div class="row">
      <ColourRows
        colors={bg.colors}
        {picker}
        armedRow={colourArmed}
        max={MAX_KEY_COLORS}
        caption={bg.colors.length > 1 ? 'Colours to remove' : 'Colour to remove'}
        hexLabel={'Colour {i} to remove (hex)'}
        removeLabel={'Remove colour {i}'}
        addTitle="Key another colour (a 2-colour ramp, a second flat tone) — up to {MAX_KEY_COLORS}"
        emptyHint={picker ? 'nothing picked yet' : 'type a hex value'}
        clearOnly
        onPick={toggleEyedropper}
        onHex={setKeyColor}
        onRemove={removeKeyColor}
        onAdd={addColour}
      />
      <label class="field slider">
        <span>Similarity (0.01–1) — <b>{bg.pickSimilarity.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0.01" max="1" step="0.01" bind:value={app.ops.background.pickSimilarity} aria-label="Similarity" />
          <NumField bind:value={app.ops.background.pickSimilarity} min={0.01} max={1} step={0.01} small />
        </span>
      </label>
      <label class="field slider">
        <span>Blend (0–1) — <b>{bg.pickBlend.toFixed(2)}</b></span>
        <span class="row tight">
          <input type="range" min="0" max="1" step="0.01" bind:value={app.ops.background.pickBlend} aria-label="Blend" />
          <NumField bind:value={app.ops.background.pickBlend} min={0} max={1} step={0.01} small />
        </span>
      </label>
    </div>
    <p class="hint">
      {#if picker}The eyedropper reads the preview still (shown unkeyed while it is armed); each{:else}Each{/if} colour is keyed in RGB
      at full resolution, one key per row, all with the same similarity and blend. Raise similarity for gradients and JPEG noise;
      blend feathers the edge. “+ add colour” handles a 2-colour ramp or a second flat tone.
    </p>
  {:else}
    <p class="hint">
      Pick a mode to make the background transparent: AI cuts the subject out of any background, Colour and Screen key a flat
      one. Soft edges need WebP / AVIF / APNG output — GIF cuts them to 1-bit alpha.
    </p>
  {/if}

  {#if bg.enabled && morphSupported}
    <details class="adv" bind:open={edgeOpen}>
      <summary><span class="sum">Edge cleanup</span>{#if !edgeOpen}<span class="muted small">· {morphSummary || 'off'}</span>{/if}</summary>
      <div class="row">
        <label class="inline" title="A 3×3 close of the alpha: fills pinholes of up to 1 px without growing the silhouette">
          <input type="checkbox" bind:checked={app.ops.background.morph.close} /><span>Fill pinholes</span>
        </label>
        <label class="inline" title="Extra 3×3 dilations after the close: each grows the matte by one source pixel (0–{MORPH_MAX_GROW})">
          <span>Grow matte</span>
          <NumField bind:value={app.ops.background.morph.grow} min={0} max={MORPH_MAX_GROW} step={1} small />
          <span>source px</span>
        </label>
      </div>
      <p class="hint">
        Grow +1–2 recovers eaten interiors at a 2 px fringe; in source pixels, like Feather, so 4 px on a 720 px source is
        under 1 px after the emote fit. Soft edges are the Feather card (only WebP / APNG / AVIF keep them — GIF is 1-bit).
      </p>
    </details>
  {/if}
</OpCard>

<style>
  .row.tight {
    gap: 6px;
    flex-wrap: nowrap;
  }
  .field.slider {
    flex: 1 1 220px;
  }
  .field.slider input[type='range'] {
    flex: 1;
    min-width: 100px;
  }
  .field.slider > span:first-child {
    white-space: normal;
  }
  .hex {
    width: 84px;
  }
  select.model {
    min-width: 180px;
  }
  .status-field {
    flex: 1 1 220px;
  }
  .status {
    font-size: 12.5px;
    white-space: normal;
    word-break: break-word;
  }
  .status.bad {
    color: var(--amber);
  }
  .row.compute {
    gap: 8px 10px;
  }
  .guided {
    display: flex;
    flex-direction: column;
    gap: 6px;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
  }
  .keyframes {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 6px;
  }
  .keyframe {
    display: inline-flex;
    align-items: center;
    gap: 1px;
  }
  .keyframe.current > button:first-child {
    border-color: var(--accent);
    color: #fff;
  }
  .warn {
    margin: 0;
    font-size: 12.5px;
    color: var(--amber);
  }
  .adv {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0 10px;
  }
  .hint + .adv,
  .guided + .adv,
  .row + .adv {
    margin-top: 8px;
  }
  .adv > summary {
    cursor: pointer;
    padding: 6px 0;
    font-size: 12.5px;
    user-select: none;
    display: flex;
    align-items: baseline;
    gap: 6px;
    flex-wrap: wrap;
  }
  .adv > summary .sum {
    font-weight: 600;
  }
  .adv[open] > summary {
    border-bottom: 1px solid var(--border);
    margin-bottom: 8px;
  }
  .adv > .row {
    margin-bottom: 8px;
  }
</style>
