# ez-local-gif — web UI

Svelte 5 (runes) + Vite + TypeScript single-page app. No component library,
hand-rolled dark CSS. The production build lands in `web/dist` and is embedded
into the Go binary by `web/embed.go`.

## Commands (Node 22)

    npm ci              # install (lockfile committed)
    npm run dev         # Vite dev server on http://localhost:5173
    npm run build       # → web/dist (index.html + assets/), keeps dist/.gitkeep
    npm run check       # svelte-check (TypeScript + Svelte diagnostics)
    npm test            # vitest unit tests (src/**/*.test.ts, plain Node, no DOM):
                        #   lib: still, proxy (Play), state/buildOutput/buildOps/effectiveFPS,
                        #        state.phase3 (op order, overlay assets → sources), overlay
                        #        (anchor / drag-box maths), eyedropper (pixel mapping), fonts
                        #        fallback, presets, format (timecode, frame grid, drop rates),
                        #        rules labels, result grouping, drop planning, edit-as-source,
                        #        api helpers, render reset
                        #   components: ResultCard / OutputCard / Preview / TrimCard / CropCard /
                        #        BackgroundCard / OverlaysPanel rendered with svelte/server (SSR)
    npm run preview     # serve the built dist locally

`npm run dev` proxies `/api`, `/out` and `/healthz` to the Go server at
`http://localhost:8080` (`ezlg serve`). Point it elsewhere with
`EZLG_API=http://host:port npm run dev`. Inside the dev container
(`compose.dev.yaml`) it is started as `npm run dev -- --host 0.0.0.0 --port 5173`.

## Layout

    src/main.ts                  mount
    src/App.svelte               two-column page (source + ops | output + result), Ctrl+Enter,
                                 '/?src=<hash>' startup load + address-bar sync
    src/app.css                  theme + shared controls
    src/lib/api.ts               TS mirror of the Go JSON types + fetch/XHR/SSE client
                                 (upload of one file or an image sequence, from-result, ?src= helpers,
                                 Phase 3: /api/fonts, /api/proxy, multi-source stills)
    src/lib/state.svelte.ts      app state (source, ops, output, ui) + buildOps/buildOutput/recipeOps,
                                 recipeSources/assetHashes (overlay assets), background/autocrop/reverse
                                 /overlay cfg, sequence fps/duration, effectiveOps (Optimize sends no ops)
    src/lib/overlay.ts           text / image overlay cfg + the drag-box geometry (anchor maths,
                                 footprints, time-range visibility)
    src/lib/eyedropper.ts        display px → still px mapping, canvas pixel read, hex helpers
    src/lib/proxy.ts             ProxyPlayer: the on-demand animated preview behind Play (stale state)
    src/lib/fonts.ts             font families for the Text card (GET /api/fonts, DejaVu Sans fallback)
    src/lib/presets.ts           presets (Emote, Sticker, Chat GIF/WebP/AVIF, Optimize, Frames, Custom),
                                 formats per preset, fit budgets, matte / trim-fringe constants
    src/lib/still.ts             StillScheduler: preview still debounce/abort/object-URL logic (unit-tested)
    src/lib/render.svelte.ts     job submission / progress / result state
    src/lib/format.ts            formatting helpers, snapFPS/gifDelays, fitSize, fmtTimecode, frame grid
    src/lib/result.ts            result-manifest grouping (primary / alternatives / frames / archive), chat sizes
    src/lib/rules.ts             friendly labels for discordlint rule ids (gif.* webp.* apng.* static.*)
    src/lib/files.ts             drop planning: one file vs image sequence, natural sort
    src/lib/editsource.ts        "edit as source": open tab → POST /api/sources/from-result → navigate
    src/lib/toast.svelte.ts      toasts
    src/components/…             UploadZone, ProbeBadge, Preview (+ CropOverlay, OverlayLayer drag boxes,
                                 eyedropper layer, Play/Stop), AnchorGrid, ops/* (Trim, Crop + auto-crop,
                                 Resize, Fps, Speed + reverse, Background, FlipRotate, Delay,
                                 OverlaysPanel → TextOverlayCard / ImageOverlayCard + TimeRangeFields),
                                 OutputCard, RenderPanel, ResultCard (+ InChat), DiscordChecks, Header, Toasts

## Behaviour notes

- Stills: `POST /api/still` with `{src, ops, output, t, maxW}`; debounced 150 ms,
  in-flight requests aborted by newer ones (and when the state returns to the
  still already on screen, so a superseded frame can never land later), object
  URLs revoked after the next frame loads. Logic lives in `lib/still.ts` /
  `lib/format.ts`.
- The scrubber is a **frame index** `app.ui.scrubFrame` ∈ [0, N−1] on the
  plan's grid: N = `planFrames` (mirrors `graph.Plan.Frames`: an image sequence
  whose timing is untouched has exactly its frame count, everything else
  `floor(duration × fps + 1e-6)`), fps = `planFPS` (effective output fps). The
  range input has min 0 / max N−1 / step 1, so it reaches both ends and one
  notch is one frame. The still is requested at the *middle* of the frame
  (`stillTime` = (i + 0.5)/fps, whole ms) — the server maps t → floor(t × fps),
  which is robust there — and the readout shows the frame's start
  Resolve-style (`00:01.12 · f 29 / 75`). The readout is a focusable
  `role="slider"`: ← → step one frame, Shift ×10, Home/End; ⏮ ◂ ▸ ⏭ do the
  same.
- Trim "from scrubber" uses `frameWindow`: Start = the frame's start, End =
  the point after it, both mapped back through trim/speed to source seconds;
  on the last frame End means "to the end" (0). Start is capped at
  `trimStartMax` (one source frame before the end), so the graph never sees a
  trim start at or beyond the clip. The selection is also shown as source
  frames (`frameSpan`).
- Output card: "Use for" chips Emote · Sticker · Chat · Optimize · Frames ·
  Custom pre-fill it — Emote (GIF, 128², fit 256 KiB on), Sticker (indexed APNG
  256 colours, 320², fit 512 KiB on, keep size), Chat (GIF by default; the
  Format select offers GIF / WebP / AVIF and `onFormat` re-seeds lossy 20 /
  q 80 / q 60), Optimize (GIF source only: gifsicle-only, no ops,
  drop-every-Nth-frame fps chips), Frames (frame format png/jpeg/webp), Custom.
  The **Discord target** dropdown is always editable (presets only set its
  default): none · emote · sticker · attachment (free, 20 MB) · attachment-50
  (Nitro Basic / Level-2 server) · attachment-100 (Level-3 server) ·
  attachment-500 (Nitro); the table in `lib/presets.ts` (`TARGET_DEFS`)
  mirrors `discordlint` and drives the limit readout, the "= limit" fit budget
  (`setTarget` moves a budget that sat on the old cap) and the notes. Rows:
  format · target · limit | size + fps | the format's quality knobs | fit |
  an **Advanced** fold (matte, alpha threshold / trim fringe, dither, loop).
  APNG appears only for Sticker and Custom; gifski never appears.
- The header logo is a link to `/` that resets to the landing state
  (`resetRender` + `resetApp`: no source / result / job, default ops and
  output) without a reload.
- Fit-to-size: "Fit to ≤ N KiB" + "keep size" / "keep fps" → `fitBytes`,
  `fitKeepSize`, `fitKeepFps` (only sent when the budget is on; never for frames).
- GIF matte: "Discord dark / Discord light / Custom" with the 1-bit-alpha
  explanation; "Trim fringe" = alpha threshold 180 (128 otherwise).
- Image sequences: dropping / picking / pasting several images uploads them in
  one request (every file as a `file` part, plus `delayMs` from the zone's
  delay field, default 100 ms); the probe badge shows "N frames · sequence" and a
  mixed-size note; the Delay card (op `delay`) changes the timing later, and the
  preview duration / fps follow it.
- Loop count (`Output.loop`, GIF NETSCAPE semantics: 0 = forever, N = play N+1
  times) is editable only with no Discord target (Custom preset, target none);
  every Discord target requires loop forever, so the control shows
  "forever (Discord requires it)" disabled and `buildOutput` sends 0 (omitted)
  for them (DESIGN §5.3; discordlint keeps `gif.netscape-loop` /
  `webp.loop-forever` as errors for Discord targets, info for none). Static
  formats and frames carry no loop.
- Effective fps shown in the Output card follows graph.Compile: the Frame rate
  op wins over `Output.fps`, which wins over the source rate; `snapFPS` caps GIF
  at 50 and the rest at 60 (no 100/n snapping — the gif muxer alternates 3/4 cs
  delays for 30 fps with exact timing; `gifDelays` renders that hint).
- Result card: the primary file (size badge vs limit, fit summary from its
  `desc`, "as seen in chat" thumbnails — emote 22/48 px, sticker 160 px, dark and
  light — Discord checks with friendly rule labels; the preview backdrop choice
  is app-level UI state, so it persists across re-renders instead of resetting
  to dark — Phase 3 review fix), up to two fit alternatives
  as small cards, a lazy thumbnail grid with per-frame downloads + "Download all
  (zip)" for frame extraction. Every file has Download / Open / "Edit as source"
  (opens a tab synchronously, POSTs `/api/sources/from-result`, navigates it to
  `/?src=<hash>`; a blocked pop-up is reported with the URL).
- A new source (drop / pick / paste / ?src=) resets the previous Result card and
  any in-flight job subscription (`resetRender`, which also cancels the
  orphaned server job) and remounts the Preview (`{#key source.hash}` in
  App.svelte), so nothing from a different file stays on screen. The address bar
  tracks the current source (`/?src=<hash>`), so a reload brings it back.
- UploadZone: the zone is a `role="group"` drop target; the picker is opened by
  a real `<button>` (Enter/Space work natively) and Cancel is a sibling button —
  no interactive descendants inside a `role="button"`.
- While the Crop card is open the still is requested **without** crop/resize/
  flip/rotate and without output sizing, so the canvas overlay maps display
  pixels straight onto source pixels.
- Ops are serialised in the order unpremultiply, delay, trim, speed, fps,
  chromakey/colorkey, crop/autocrop, resize, flip, rotate, reverse, then the
  text/overlay ops in the user's card order (the compiler hoists
  unpremultiply and delay anyway).

### Phase 3 editing ops

- **Background card** (`app.ops.background`): None · Greenscreen · Bluescreen ·
  Pick a colour. Green/blue emit `chromakey` (key colour — preset `00ff00` /
  `0000ff` or custom —, similarity, blend, despill on/off + mix/expand under
  Advanced); recipe zero values are left out of the params, and because the
  Go zero value of blend *is* the 0.05 default, the blend slider floors at
  0.01. Pick a colour emits `colorkey` once a colour was picked: the
  **eyedropper** (`app.ui.pickColor`) arms a transparent button layer over the
  preview still, which is requested *without* the key op while armed
  (`buildOps({keyPreview})`); a click maps display px → still px
  (`lib/eyedropper.displayToPixel`), reads the pixel through a canvas and
  stores the hex; the hex field is the keyboard path; Esc cancels.
- **Crop card**: "Auto-crop to content" (`app.ops.autocrop`: padding, alpha
  threshold under Advanced for alpha sources) emits `autocrop` instead of the
  manual `crop`; the rectangle fields are disabled while it is on and the
  preview leaves crop mode (the server resolves the box; the still shows the
  result). The header toggle covers both. The manual rectangle has 8 resize
  handles (edges + corners) and a "Lock ratio" checkbox that captures the
  current w:h and constrains handle resizes, newly dragged rectangles and the
  W/H inputs (Phase 3 review fixes).
- **Speed card**: "Reverse" (`app.ops.reverse`) emits `reverse` after the
  geometry ops; the header toggle covers factor and reverse.
- **Feather** — "Feather — N px (soft edge ≈ 2–3×N)": emits the `feather` op
  (radius = Gaussian sigma in **source** pixels, 0.1–50, default 3). The server
  blurs only the alpha plane, right after keying and before any geometry, so
  the softness scales down with the output size; it skips the stage when the
  frame carries no alpha at that point (e.g. an opaque source with no key
  before it). GIF output thresholds the soft edge back to 1-bit —
  WebP/APNG/AVIF keep it.
- **Overlays** (`app.ops.overlays`, `lib/overlay.ts`): "+ Add text" / "+ Add
  image" append cards (stable ids; ▲ ▼ reorder = drawing order, ✕ remove; each
  card has its own enable toggle). Text: textarea, font from `GET /api/fonts`
  (`lib/fonts.ts` reduces faces to families; DejaVu Sans is always offered and
  is the fallback when the endpoint is empty or down), size, colour
  (RRGGBB[AA]), outline width/colour, box + colour + padding (≥ 1: the Go zero
  value means the default 8), anchor (3×3 grid — switching keeps the element
  in place by moving X/Y), X/Y, time range. Image: the asset is an ordinary
  `POST /api/upload` (pick or drop onto the card); the card keeps the returned
  `Source`; W/H (0 = natural, one side keeps the aspect), opacity, loop (only
  offered for animated assets; off → `noLoop`), anchor/X/Y, time range.
  `recipeSources` = `[main, ...assetHashes]` in first-use order, deduplicated;
  overlay ops carry `source` = that index, so removing a card re-indexes the
  rest; disabled / asset-less cards contribute nothing (unit-tested).
- **Drag to place**: with at least one ready overlay the still is requested
  with `maxW` 8192 (graph's MaxDim — the server scales to `min(iw, maxW)`, so
  the still arrives unscaled and its natural size *is* the output canvas).
  `OverlayLayer` draws one box per overlay active at the scrubber frame
  (`activeAt`: start ≤ t < end, 0 = whole clip; others are counted in a
  corner badge — scrub into their range to see them), positioned in % of the
  canvas so zoom does not matter; dragging moves the anchor point by the
  pointer delta × (canvas px / display px), clamped so ≥ 8 px stay visible;
  arrow keys nudge (Shift ×10). The still re-renders through the usual
  150 ms debounce, so the user sees the real composite. Text boxes are an
  estimate (0.6 em per char, 1.2 em per line + outline + box padding).
- **Time ranges** (`TimeRangeFields`): output seconds after trim/speed,
  half-open `[start, end)` like trim (the frame at exactly `end` is not
  drawn); "from scrubber" = the frame's start for Start, the point after it
  for End (last frame → 0 = to the end), both floored to whole µs so they
  sit on the server's `gte(t+0.0001,S)*lt(t+0.0001,E)` window (adjacent
  ranges never overlap), "Whole clip" clears both, "▸ go to start" moves the
  scrubber into the range.
- **Play** (`lib/proxy.ts` ProxyPlayer): `POST /api/proxy` with
  `{sources, ops, output, maxW: 360, maxSeconds: 10}` → animated WebP shown in
  the stage instead of the still; requested **on demand only**; every
  state change calls `update(req)`, which never fetches but marks the shown
  proxy **stale** ("changed — Play again") when the recipe key differs;
  Stop returns to the still and releases the object URL; crop / eyedropper
  modes stop playback. Memoised server-side like stills.
- Stills and jobs send `sources` (main + assets) alongside `src`; the server
  accepts either and checks they agree on the main source.
- Jobs: `POST /api/jobs` → `EventSource /api/jobs/{id}/events`; if the stream
  cannot be opened or drops, the UI polls `GET /api/jobs/{id}` once a second.
