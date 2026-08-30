# ez-local-gif

A self-hosted [ezgif](https://ezgif.com) replacement that runs as **one Docker container** on
your LAN (no auth, no TLS — put it behind a reverse proxy if you need either). Drop a video
(including DaVinci Resolve **ProRes 4444 exports with alpha**), a GIF, an animated
WebP/APNG/AVIF or an image sequence into the browser UI and get back animated
**GIF / WebP / APNG / AVIF with transparency** — plus MP4/WebM chat clips, PNG frames and static
images — sized for Discord's emote (256 KiB, 128×128) and sticker (512 KiB, 320×320) budgets.

- **Presets + fit-to-size** — emote / sticker / chat presets, and a search that lands the file
  under any byte target while giving up as little quality as possible.
- **Discord byte-level linter** — checks and fixes the rules that make files render black,
  opaque, flickering or play-once after Discord's server-side transcode.
- **Editing ops** — trim, crop, resize, fps/speed, background removal (chroma/color key),
  feather (soft edges), image/text/animated overlays, reverse and bounce.
- **Batch** — apply one preset to a folder's worth of clips at once.
- **Lossless GIF fast path** — trim/crop/frame-drop a GIF without re-encoding it.
- **/input picker + /output save** — read sources from a mounted host folder and save results
  back to one, no upload/download round trip.
- **Live preview** — still scrubber over checkerboard/dark/light backdrops and an animated
  Play proxy, so keying and timed overlays can be judged in motion before rendering.

All pixel work is done by pinned CLI tools inside the image (FFmpeg 9, gifsicle, libwebp,
libavif, pngquant, oxipng, gifski); the non-destructive op stack compiles to a single ffmpeg
filtergraph — one decode per render, encoders fanning out in parallel from an RGBA master on
tmpfs — and results are memoised by recipe hash. Go backend (one static binary, SPA embedded),
Svelte 5 + Vite frontend.

Docs: [`docs/DESIGN.md`](docs/DESIGN.md) (architecture, Discord rules, encoder commands),
[`docs/FEATURES.md`](docs/FEATURES.md) (everything it can do, in detail),
[`docs/USAGE.md`](docs/USAGE.md) (configuration, HTTP API, WSL2 notes, troubleshooting),
[`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) (dev loop, scripts, tests).

## Developing and running locally

The dev stack runs everything in a container (tools + Go 1.26 + Node 22) with the repo
bind-mounted — no Go or ffmpeg needed on the host:

```sh
docker compose -f compose.yaml -f compose.dev.yaml up --build
#   UI  (Vite dev server, hot reload)   http://localhost:5173   ← use this one while developing
#   API (go build + ezlg serve)         http://localhost:8080   (Vite proxies /api and /out to it)

# Go tests inside the real toolchain image
docker compose -f compose.yaml -f compose.dev.yaml run --rm app go test ./...

# End-to-end API test (spawns its own server on a throw-away data dir)
docker compose -f compose.yaml -f compose.dev.yaml run --rm -e EZLG_START_SERVER=1 app bash scripts/integration-test.sh
```

The full loop (restarting the Go server, running things by hand, the Discord acceptance test
kit, production image builds) is in [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md).

### Directory map

| Path | What lives there |
|---|---|
| `cmd/ezlg` | Main binary (`serve` default, `testkit`, `version`) |
| `internal/recipe` | Shared data model (Recipe/Op/Output/ProbeInfo); stdlib only, no ffmpeg |
| `internal/graph` | Op stack → ffmpeg filter_complex (pure, golden-tested) |
| `internal/enc` | argv builders for ffmpeg/gifsicle/… (pure, golden-tested) |
| `internal/fit` | Fit-to-size search (ladder + secant over one knob; pure orchestration, fake-encoder tested) |
| `internal/ffrun` | The only package that spawns processes; ffmpeg progress parsing |
| `internal/discordlint` | GIF/WebP byte-level lint + fix (stdlib only) |
| `internal/probe` | ffprobe → ProbeInfo (+ alpha scan) |
| `internal/store` | `/data` layout: blobs by sha256, results by recipe hash, scratch, sweeper |
| `internal/jobs` | Job table, SSE events, render pipeline |
| `internal/server` | HTTP API + embedded SPA |
| `web/` | Svelte 5 + Vite SPA; `web/dist` is embedded by `web/embed.go` (not committed) |
| `scripts/` | Dev/test helpers — see the [scripts table](docs/DEVELOPMENT.md#scripts) |
| `docs/` | Design doc (source of truth), usage, development, Discord test results |

New code goes where its layer lives: anything that builds command lines belongs in the pure,
golden-tested `internal/graph` / `internal/enc`; anything that runs a process goes through
`internal/ffrun`; Discord rules live in `internal/discordlint` + `docs/DESIGN.md` §5.3.

## Running the Docker image

CI publishes the container image to Docker Hub:

```sh
docker pull duckautomata/ez-local-gif:latest
mkdir -p output && sudo chown 1000:1000 output    # Linux/WSL only; the container runs as uid 1000
docker run -d --name ezlg \
  -p 8080:8080 \
  --shm-size 4g \
  -v ezlg-data:/data \
  -v ./output:/output \
  -v ./input:/input:ro \
  duckautomata/ez-local-gif:latest
# open http://localhost:8080  (or http://<server-ip>:8080 from another machine)
```

`--shm-size 4g` matters: the RGBA frame master lives on `/dev/shm`, and Docker's default 64 MiB
tmpfs forces a slower disk-backed fallback (the log warns; ≥ 256 MiB silences it, 4 GiB is sized
for 1080p sources). The `./output` bind (result card's "Save to /output") and the read-only
`./input` bind (the in-app picker) are optional — the UI shows the buttons only when the folders
are mounted and usable.

Or as a minimal `compose.yaml`:

```yaml
services:
  app:
    image: duckautomata/ez-local-gif:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    shm_size: "4gb"
    volumes:
      - ezlg-data:/data
      - ./output:/output
      # - ./input:/input:ro
volumes:
  ezlg-data:
```

**Port:** change the *host* port via the mapping — `-p 9000:8080` (or `ports: ["9000:8080"]`)
serves on `http://host:9000` with nothing else to touch. The *container* port is `EZLG_ADDR`
(default `:8080`): set e.g. `-e EZLG_ADDR=:9000` and match the right-hand side of the mapping
(and the healthcheck, if you override it).

Every environment variable, the volume layout, the HTTP API, WSL2 notes and troubleshooting
(startup ownership errors, the shm warning) are in [`docs/USAGE.md`](docs/USAGE.md). The repo's
own [`compose.yaml`](compose.yaml) is the full commented example (fonts bind, retention knobs,
GPU stub) — it builds from source; swap `build:` for the Docker Hub `image:` to use the published one.

## License

Tools are executed as separate processes, not linked: gifsicle (GPL), gifski (AGPL), FFmpeg
(GPL build), libwebp/libavif/pngquant/oxipng under their own licences. See each project.
