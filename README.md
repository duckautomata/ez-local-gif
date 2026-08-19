# ez-local-gif

A self-hosted [ezgif](https://ezgif.com) replacement that runs as **one Docker container** on your
LAN. Drop a video (including **DaVinci Resolve ProRes 4444 exports with alpha**), a GIF, an
animated WebP/APNG/AVIF or an image sequence into the browser UI and get back animated
**GIF / WebP / APNG / AVIF with transparency**, PNG frames, MP4/WebM or static images — with
presets that fit Discord's **emote (256 KiB, 128×128)** and **sticker (512 KiB, 320×320)** budgets
and a byte-level linter that makes sure the result does **not** render black, opaque or flickering
on Discord.

- Go backend (one static binary, SPA embedded), Svelte 5 + Vite frontend.
- All pixel work is done by CLI tools inside the image: **FFmpeg 9.0.1** (BtbN static build,
  pinned by hash), gifsicle 1.96, libwebp 1.5 tools, libavif 1.2, pngquant, oxipng, gifski 1.34.
- Non-destructive op stack (trim, crop, resize, fps, background removal, overlays, text) compiled
  to a single ffmpeg filtergraph; one decode per render, encoders fan out in parallel from an RGBA
  master on tmpfs; results memoised by recipe hash.
- LAN only: no auth, no TLS. Put it behind a reverse proxy if you need either.

The design, Discord rules and phase plan live in [`docs/DESIGN.md`](docs/DESIGN.md).

## Quick start

```sh
git clone https://github.com/duckautomata/ez-local-gif && cd ez-local-gif
mkdir -p output && sudo chown 1000:1000 output   # Linux / WSL distro only — see the note below
docker compose up -d --build          # first build downloads ~150 MB of tools
# open http://localhost:8080  (or http://<server-ip>:8080 from another machine)
```

`compose.yaml` runs the `app` service with a named volume for `/data`, `./output` bound to
`/output` ("Save to /output" in the UI, and where the Discord test kit writes), `shm_size: 4gb`
for the frame master, and sane defaults for retention. Uncomment the `/input` bind to pick files
from a host folder without uploading. Logs: `docker compose logs -f app`. Update:
`git pull && docker compose up -d --build`.

**`./output` ownership (bare-metal Linux, Docker inside a WSL distro):** the container runs as
uid 1000 (`ezlg`). If `./output` does not exist when you first run `docker compose up`, the Docker
daemon creates it as `root:root 0755` and the app cannot write to it. Create it first as above, or
fix an existing one with `docker compose run --rm --user root --entrypoint chown app -R 1000:1000
/output`. Docker Desktop on Windows/macOS needs neither — its bind mounts appear world-writable.
See [Troubleshooting](#troubleshooting) for this and the other startup complaints.

## Configuration

Everything is optional; set it under `environment:` in `compose.yaml`.

| Variable | Default | Meaning |
|---|---|---|
| `EZLG_ADDR` | `:8080` | Listen address (change `ports:` and the healthcheck too) |
| `EZLG_DATA` | `/data` | Data root: blobs by sha256, results by recipe hash |
| `EZLG_SCRATCH` | `/dev/shm/ezl` | Scratch (tmpfs) root for frame masters; falls back to `$TMPDIR/ezl` |
| `EZLG_TTL_HOURS` | `24` | Delete blobs/results older than this (`0` = never) |
| `EZLG_MAX_BYTES` | `21474836480` (20 GiB) | Cap on total `/data` size (`0` = none) |
| `EZLG_MAX_UPLOAD_MB` | `2048` | Maximum upload size |
| `EZLG_CONCURRENCY` | `max(1, NumCPU/2)` | Concurrent renders |
| `EZLG_FFMPEG`, `EZLG_FFPROBE`, `EZLG_GIFSICLE`, `EZLG_GIFSKI`, `EZLG_IMG2WEBP`, `EZLG_WEBPINFO`, `EZLG_AVIFENC`, `EZLG_AVIFDEC`, `EZLG_PNGQUANT`, `EZLG_OXIPNG` | found on `PATH` | Override a tool path |

Volumes / mounts: `/data` (required, keep it on a Linux filesystem), `/output` (optional, rw,
must be writable by uid 1000 — see Quick start), `/input` (optional, ro), `/dev/shm` sized by
`shm_size`.

## Running on WSL2 (Windows workstation)

Docker Desktop with the WSL2 backend or Docker inside a WSL distro both work; the compose file is
the same. Notes:

- **Keep `/data` on the WSL ext4 filesystem** — the default named volume does this. Do not bind
  `/data` to a `/mnt/c/...` path (9P is very slow for many small files).
- **`./output` ownership:** with Docker Desktop and the repo on the Windows drive there is nothing
  to do. With the repo (and/or Docker) inside a WSL distro the bind is a real Linux directory, so
  run `mkdir -p output && sudo chown 1000:1000 output` before the first `up` (Quick start), or the
  daemon creates it root-owned and the uid-1000 container cannot write to it.
- **Memory:** the RGBA frame master lives on `/dev/shm` (`shm_size: 4gb`) and WSL2 defaults to
  50 % of host RAM. Raise it in `%UserProfile%\.wslconfig`:
  ```ini
  [wsl2]
  memory=24GB
  ```
  then `wsl --shutdown` and restart Docker Desktop.
- **Reading Resolve exports directly:** bind your Windows export folder read-only as `/input`
  (see the commented example in `compose.yaml`; on Docker Desktop for Windows use
  `C:/Users/you/Videos/Resolve Exports:/input:ro`, inside a WSL distro use
  `/mnt/c/Users/you/Videos/Resolve Exports:/input:ro`). Reading a few large files over 9P is fine.
- **GPU:** not used in v1 (all animated-image encoders are CPU). The commented `gpu` block in
  `compose.yaml` and `docs/DESIGN.md` §6 describe the later NVDEC/NVENC/rembg option; it needs
  nvidia-container-toolkit and a host driver ≥ 610.

## Troubleshooting

All three are ownership / sizing problems between the container (uid 1000, `/dev/shm` scratch)
and what the Docker daemon hands it. `docker compose logs app` shows the exact line.

**`store: /data/blobs is not writable by uid 1000 (...)` and the container exits at startup.**
The `ezlg-data` volume (or whatever you bound to `/data`) was created or populated by another
uid — typically the root-running dev stack pointed at the same volume, or a bind mount the daemon
auto-created as root. The server refuses to start rather than fail on the first upload. Fix the
ownership once (the runtime image has an `ezlg` user, uid 1000):

```sh
docker compose run --rm --user root --entrypoint chown app -R ezlg:ezlg /data
docker compose up -d
```

or throw the volume away and start clean: `docker compose down -v && docker compose up -d`
(deletes all cached uploads and results). The dev stack (`compose.dev.yaml`) uses its own
`ezlg-data-dev` volume for exactly this reason — keep it that way.

**`discord-testkit: OUTDIR '/output/testkit' is not writable by uid 1000 (ezlg): ...`.**
`compose.yaml` binds `./output` to `/output` and the container runs as uid 1000. On bare-metal
Linux or with Docker inside a WSL distro, a `./output` that did not exist at the first `up` is
created by the daemon as `root:root 0755`, and files written there by an earlier run as another
user (e.g. the root dev stack, if you pointed it at `./output`) are equally read-only for the app.
The server itself does not touch `/output` today (it is the "Save to /output" target and the test
kit's output dir), so nothing fails at startup — the kit checks up front and stops with this hint
before running the matrix. Fix on the host: `mkdir -p output && sudo chown -R 1000:1000 output`;
or without sudo: `docker compose run --rm --user root --entrypoint chown app -R 1000:1000
/output`. Docker Desktop on Windows/macOS: nothing to do (binds appear world-writable). The dev
stack writes to `./output-dev` (root-owned on Linux hosts, by design) so it never poisons `./output`.

**`store: WARNING scratch /dev/shm/ezl is on a 64 MiB filesystem (Docker's default /dev/shm is
64 MiB)` followed by `using disk-backed /data/scratch for job scratch instead`.** The RGBA frame
master lives on `/dev/shm`; Docker's default 64 MiB tmpfs would ENOSPC almost every render, so the
server falls back to `/data/scratch` (works, but disk-backed and slower). `compose.yaml` already
sets `shm_size: "4gb"`; you see this when the image is run with a plain `docker run` (add
`--shm-size=4g`), through an override that dropped `shm_size`, or on a Compose/orchestrator that
ignores it (Swarm/Kubernetes: mount an `emptyDir` with `medium: Memory` at `/dev/shm` instead).
Anything ≥ 256 MiB stops the warning; 4 GiB is sized for 1080p sources (8.3 MB per frame). On WSL2
also raise `memory=` in `%UserProfile%\.wslconfig` (see above) so the tmpfs has RAM to back it.

## Development loop

Go is **not** required on the host: everything runs in the `dev` image (tools + Go 1.26 + Node 22)
with the repo bind-mounted at `/src`.

```sh
docker compose -f compose.yaml -f compose.dev.yaml up --build
#   UI  (Vite dev server, hot reload)   http://localhost:5173   ← use this one while developing
#   API (go build + ezlg serve)         http://localhost:8080   (proxied by Vite for /api and /out)
```

The Vite dev server proxies `/api` and `/out` to `:8080` (configured in `web/vite.config.ts`).
The Go server is not auto-restarted on edits: `docker compose -f compose.yaml -f compose.dev.yaml
restart app` rebuilds and restarts it (Ctrl-C / `stop` sends SIGTERM through `init: true` so it
drains cleanly). Or run things by hand in one interactive container — `run --rm` starts a
throw-away container (its name is random, so `docker exec -it ezlg-dev …` from the `up` flow does
not apply here); `--service-ports` publishes 8080 and 5173:

```sh
docker compose -f compose.yaml -f compose.dev.yaml run --rm --service-ports app bash
# inside the container (all in the same shell):
cd web && npm ci && (npm run dev -- --host 0.0.0.0 &)   # Vite in the background → :5173
cd .. && go run ./cmd/ezlg serve                        # API → :8080; Ctrl-C, edit, re-run
```

`npm ci` is needed once per fresh `ezlg-webnode` volume (the dev stack keeps `web/node_modules`
in a named volume, not in the bind mount). If you do prefer a second shell, use
`docker compose -f compose.yaml -f compose.dev.yaml exec app bash` while `up` is running (that
container *is* named `ezlg-dev`, so `docker exec -it ezlg-dev bash` works there too).

Useful one-liners:

```sh
# Go commands from the Windows host (golang:1.26-trixie, caches in named volumes)
.\scripts\go.ps1 vet ./...
.\scripts\go.ps1 test ./...
./scripts/go.sh test ./internal/discordlint/...     # Linux/macOS/WSL equivalent

# Go tests inside the real toolchain image
docker compose -f compose.yaml -f compose.dev.yaml run --rm app go test ./...

# End-to-end test (starts the server in the container, uploads a ProRes clip, renders GIF + WebP)
docker compose -f compose.yaml -f compose.dev.yaml run --rm -e EZLG_START_SERVER=1 app bash scripts/integration-test.sh

# Self-test of the Discord test kit (OUTDIR guard, frame-rate handling, variants, scratch; ~1–2 min)
docker compose -f compose.yaml -f compose.dev.yaml run --rm app bash scripts/testkit-test.sh

# Re-check every bundled tool / ffmpeg capability
docker run --rm --entrypoint bash ezlg:local /usr/local/share/ezlg/check-tools.sh
```

Production image build with a version stamp:

```sh
docker build --target runtime -t ezlg:local --build-arg VERSION=$(git describe --tags --always) .
```

Image layout (`Dockerfile`): `web` (npm build) → `gobuild` (static binary, SPA embedded) →
`tools` (trixie-slim + pinned FFmpeg/gifsicle/libwebp/libavif/pngquant/oxipng/gifski/fonts/tini,
self-checked at build time by `scripts/check-tools.sh`) → `runtime` (non-root `ezlg`, tini,
healthcheck) and `dev` (tools + Go + Node, root). Third-party downloads are pinned by URL and
sha256 in the `ARG`s at the top of the Dockerfile; `scripts/pin-ffmpeg.sh` prints fresh values
when the BtbN autobuild tag is pruned (daily tags live ~2 weeks, month-end tags are permanent).

## Discord acceptance test (Phase 1)

Files that look fine in a browser can still break on Discord, which re-encodes every preview
server-side. Before trusting a build, run the test kit and upload the variants to a **private**
Discord server:

```sh
# writes ./output/testkit/{a..j}_*.{gif,webp,png,avif} + README.md  (15–50 s)
docker compose run --rm --entrypoint bash app /usr/local/share/ezlg/discord-testkit.sh /output/testkit
# or with your own clip (ProRes 4444 straight from Resolve is the point):
docker compose run --rm --entrypoint bash -v "$PWD/input:/input:ro" app \
    /usr/local/share/ezlg/discord-testkit.sh /output/testkit /input/clip.mov
# (dev stack — root, writes to ./output-dev/testkit so nothing root-owned lands in ./output:
#  docker compose -f compose.yaml -f compose.dev.yaml run --rm app bash scripts/discord-testkit.sh /output/testkit)
```

If the kit stops at once with `OUTDIR '/output/testkit' is not writable by uid 1000`, the
`./output` bind is root-owned — see [Troubleshooting](#troubleshooting) for the one-line fix.

Options: `--straight` / `--premultiplied` override the alpha guess (ProRes defaults to
premultiplied, everything else to straight), `--fps N` sets the master rate (default 25 →
4 cs GIF delays; the synthetic clip is generated at that same rate, and a supplied clip whose
rate differs is resampled — the kit then warns, and its README notes that a periodic cadence
hitch is expected in every file, so pass `--fps <source rate>` for a clean timing check),
`EZLG_TESTKIT_MAX_PX` caps the chat variants (default 480).

The kit decodes the source once (ProRes is unpremultiplied at its native 10/12-bit depth with the
same `format=gbrap1Nle,setparams=alpha_mode=premultiplied,unpremultiply` chain the app uses; the
RGBA masters live on `/dev/shm/ezl-testkit`, or under `$TMPDIR` with a warning when `/dev/shm` is
Docker's default 64 MiB) and emits every encoder path from `docs/DESIGN.md` §4.2 / §9:
ffmpeg palette GIF + `gifsicle -O2` (default), the same coalesced with `gifsicle -U`, gifski
(local palettes), ffmpeg-only GIF, animated WebP lossy (`yuva420p` and `bgra` input, e / e2) and
lossless, RGBA APNG, 128×128 emote GIF/WebP fitted under 256 KiB, 320×320 sticker GIF / RGBA APNG
/ indexed 8-bit-alpha APNG fitted under 512 KiB, and an experimental animated AVIF.
`output/testkit/README.md` says for each file
where to upload it (attachment / Server Settings › Emoji / Server Settings › Stickers), lists the
client matrix (desktop, web, iOS, Android × dark/light theme × autoplay on/off, reduced motion),
what to look for (alpha survives, no black background, no colour flicker, first-frame still,
loops forever, timing) and a sizes table with the fit rung used.

Checklist for sign-off (per client and theme):

1. Attachments **a, b, d, e, e2, f**: transparent over both themes, no black box, no per-frame
   colour changes, loop forever, still frame with autoplay off is a real frame (e vs e2: note any
   difference in soft-edge colour or size — that decides the lossy WebP input format).
2. Emote **h1** (GIF) and **h2** (WebP): upload accepted, animate inline / jumbo / reaction /
   picker, transparent.
3. Sticker **i1** (GIF), **i2** (RGBA APNG), **i3** (indexed APNG): upload accepted (no
   "frame rate too small or too large"), animate in the picker and in chat, soft alpha kept.
4. **c** (gifski) and **j** (AVIF) are informational — record what happens.
5. `g` (APNG attachment) is expected to show only its first frame; that is a Discord limitation.

Record findings (client + version, theme, autoplay, screenshot of anything wrong) against
`docs/DESIGN.md` §9. Rules the linter enforces are versioned in `internal/discordlint`.

## Scripts

| Script | Purpose |
|---|---|
| `scripts/go.ps1`, `scripts/go.sh` | Run `go …` in `golang:1.26-trixie` with the repo mounted (host has no Go) |
| `scripts/check-tools.sh` | Print + assert every bundled tool and ffmpeg capability (runs at image build) |
| `scripts/make-test-clip.sh` | Synthesise a transparent test clip: ProRes 4444 (`.mov`), VP9 alpha (`.webm`) or GIF; premultiplied or straight alpha |
| `scripts/discord-testkit.sh` | Emit the Discord render-test matrix + README (see above) |
| `scripts/testkit-test.sh` | Self-test for the test kit: OUTDIR guard + hint, synthetic clip at the master rate, resample warning, every variant produced, native-depth unpremultiply, /dev/shm scratch (full checks need the toolchain image) |
| `scripts/integration-test.sh` | End-to-end API test against a running server (`EZLG_START_SERVER=1` starts one) |
| `scripts/pin-ffmpeg.sh` | Print new `FFMPEG_TAG/ASSET/SHA256` ARG lines for a BtbN release tag |

## Layout

See [`CLAUDE.md`](CLAUDE.md) for the package map (`cmd/ezlg`, `internal/{recipe,graph,enc,ffrun,
discordlint,probe,store,jobs,server}`, `web/`) and [`docs/DESIGN.md`](docs/DESIGN.md) for the
architecture, Discord rules, encoder commands, verification list and build phases.

## License

Tools are executed as separate processes, not linked: gifsicle (GPL), gifski (AGPL), FFmpeg
(GPL build), libwebp/libavif/pngquant/oxipng under their own licences. See each project.
