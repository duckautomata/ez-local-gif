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
                        #   lib: still, state/buildOutput/buildOps/effectiveFPS, presets, format
                        #        (timecode, frame grid, drop rates), rules labels, result
                        #        grouping, drop planning, edit-as-source, api helpers, render reset
                        #   components: ResultCard / OutputCard rendered with svelte/server (SSR)
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
                                 (upload of one file or an image sequence, from-result, ?src= helpers)
    src/lib/state.svelte.ts      app state (source, ops, output, ui) + buildOps/buildOutput/recipeOps,
                                 sequence fps/duration, effectiveOps (Optimize sends no ops)
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
    src/components/…             UploadZone, ProbeBadge, Preview (+ CropOverlay), ops/* (incl. DelayCard),
                                 OutputCard, RenderPanel, ResultCard (+ InChat), DiscordChecks, Header, Toasts

## Behaviour notes

- Stills: `POST /api/still` with `{src, ops, output, t, maxW}`; debounced 150 ms,
  in-flight requests aborted by newer ones (and when the state returns to the
  still already on screen, so a superseded frame can never land later), object
  URLs revoked after the next frame loads. `t` is output time (after
  trim/speed), ceiled to whole ms (`ceilMs`) so a frame-start time stays inside
  its own frame when the server maps t → floor(t × fps). Logic lives in
  `lib/still.ts` / `lib/format.ts`.
- Preview position is shown Resolve-style (`00:01.12 · f 28 / 75`) on the plan's
  frame grid (effective fps). The stage is focusable (`role="slider"`, visible
  focus ring): ← → step one frame, Shift ×10, Home/End; the ⏮ ◂ ▸ ⏭ buttons do
  the same; the range input's step is 1/fps.
- Presets pre-fill the Output card: Emote (GIF, 128², fit 256 KiB on, "WebP
  instead"), Sticker (indexed APNG 256 colours, 320², fit 512 KiB on, keep size,
  "GIF instead"), Chat GIF / WebP / AVIF (attachment), Optimize (GIF source only:
  gifsicle-only, no ops, drop-every-Nth-frame fps chips), Frames (frame format
  png/jpeg/webp), Custom. The Format select lists the formats sensible for the
  preset; APNG appears only for Sticker and Custom; gifski never appears.
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
  light — Discord checks with friendly rule labels), up to two fit alternatives
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
- Ops are serialised in the order unpremultiply, delay, trim, speed, fps, crop,
  resize, flip, rotate (the compiler hoists unpremultiply and delay anyway).
- Jobs: `POST /api/jobs` → `EventSource /api/jobs/{id}/events`; if the stream
  cannot be opened or drops, the UI polls `GET /api/jobs/{id}` once a second.
