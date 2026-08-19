# ez-local-gif — web UI

Svelte 5 (runes) + Vite + TypeScript single-page app. No component library,
hand-rolled dark CSS. The production build lands in `web/dist` and is embedded
into the Go binary by `web/embed.go`.

## Commands (Node 22)

    npm ci              # install (lockfile committed)
    npm run dev         # Vite dev server on http://localhost:5173
    npm run build       # → web/dist (index.html + assets/), keeps dist/.gitkeep
    npm run check       # svelte-check (TypeScript + Svelte diagnostics)
    npm test            # vitest unit tests (src/**/*.test.ts, plain Node, no DOM:
                        #   still, state/buildOutput/effectiveFPS, format/snapFPS, render/resetRender)
    npm run preview     # serve the built dist locally

`npm run dev` proxies `/api`, `/out` and `/healthz` to the Go server at
`http://localhost:8080` (`ezlg serve`). Point it elsewhere with
`EZLG_API=http://host:port npm run dev`. Inside the dev container
(`compose.dev.yaml`) it is started as `npm run dev -- --host 0.0.0.0 --port 5173`.

## Layout

    src/main.ts                  mount
    src/App.svelte               two-column page (source + ops | output + result), Ctrl+Enter
    src/app.css                  theme + shared controls
    src/lib/api.ts               TS mirror of the Go JSON types + fetch/XHR/SSE client
    src/lib/state.svelte.ts      app state (source, ops, output, ui) + buildOps/buildOutput
    src/lib/still.ts             StillScheduler: preview still debounce/abort/object-URL logic (unit-tested)
    src/lib/presets.ts           Discord presets and byte limits
    src/lib/render.svelte.ts     job submission / progress / result state
    src/lib/format.ts            formatting helpers, snapFPS/gifDelays, fitSize
    src/lib/toast.svelte.ts      toasts
    src/components/…             UploadZone, ProbeBadge, Preview (+ CropOverlay), ops/*, OutputCard,
                                 RenderPanel, ResultCard, DiscordChecks, Header, Toasts

## Behaviour notes

- Stills: `POST /api/still` with `{src, ops, output, t, maxW}`; debounced 150 ms,
  in-flight requests aborted by newer ones (and when the state returns to the
  still already on screen, so a superseded frame can never land later), object
  URLs revoked after the next frame loads. `t` is output time (after
  trim/speed); the scrubber runs over that range. Logic lives in `lib/still.ts`.
- Loop count (`Output.loop`, GIF NETSCAPE semantics: 0 = forever, N = play N+1
  times) is editable only with no Discord target (Custom preset, target none);
  every Discord target requires loop forever, so the control shows
  "forever (Discord requires it)" disabled and `buildOutput` sends 0 (omitted)
  for them (DESIGN §5.3; discordlint keeps `gif.netscape-loop` /
  `webp.loop-forever` as errors for Discord targets, info for none).
- Effective fps shown in the Output card follows graph.Compile: the Frame rate
  op wins over `Output.fps`, which wins over the source rate; `snapFPS` caps GIF
  at 50 and the rest at 60 (no 100/n snapping — the gif muxer alternates 3/4 cs
  delays for 30 fps with exact timing; `gifDelays` renders that hint).
- A new source (drop / pick / paste) resets the previous Result card and any
  in-flight job subscription (`resetRender`, which also cancels the orphaned
  server job) and remounts the Preview (`{#key source.hash}` in App.svelte), so
  nothing from a different file stays on screen.
- UploadZone: the zone is a `role="group"` drop target; the picker is opened by
  a real `<button>` (Enter/Space work natively) and Cancel is a sibling button —
  no interactive descendants inside a `role="button"`.
- While the Crop card is open the still is requested **without** crop/resize/
  flip/rotate and without output sizing, so the canvas overlay maps display
  pixels straight onto source pixels.
- Ops are serialised in the order unpremultiply, trim, speed, fps, crop, resize,
  flip, rotate (the compiler hoists unpremultiply anyway).
- Jobs: `POST /api/jobs` → `EventSource /api/jobs/{id}/events`; if the stream
  cannot be opened or drops, the UI polls `GET /api/jobs/{id}` once a second.
