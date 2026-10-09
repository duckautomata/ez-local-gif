# Usage

Running, configuring and troubleshooting ez-local-gif, plus the HTTP API. For a
one-paragraph overview and the fastest way to run the published image, see the
[README](../README.md). Architecture and Discord rules live in
[`DESIGN.md`](DESIGN.md); the development loop lives in
[`DEVELOPMENT.md`](DEVELOPMENT.md).

## Quick start (from a source checkout)

```sh
git clone https://github.com/duckautomata/ez-local-gif && cd ez-local-gif
mkdir -p output && sudo chown 1000:1000 output   # Linux / WSL distro only — see the note below
docker compose up -d --build          # first build downloads ~150 MB of tools
# open http://localhost:8080  (or http://<server-ip>:8080 from another machine)
```

(To run the prebuilt image from Docker Hub instead of building, see the
[README](../README.md#running-the-docker-image).)

`compose.yaml` runs the `app` service with a named volume for `/data`, `./output` bound to
`/output` (the result card's "Save to /output" writes there, and so does the Discord test kit),
`shm_size: 4gb`
for the frame master, and sane defaults for retention. Uncomment the `/input` bind to pick files
from a host folder without uploading (the in-app "/input" picker appears whenever the folder is
mounted), and the `/fonts` bind to offer your own `.ttf`/`.otf`
files to the text overlay (DejaVu and Noto Sans/Serif are bundled; the list is read once at
startup, so `docker compose restart app` after adding files — `docker compose exec app fc-cache
-f` pre-builds fontconfig's cache for a big collection). Logs: `docker compose logs -f app`.
Update: `git pull && docker compose up -d --build`.

### AI background removal (optional matte sidecar)

The Background card's **AI** mode (the `matte` op) needs the matte sidecar — a second container
behind a compose profile, **off by default**: a plain `docker compose up -d` is exactly the app as
before (`GET /api/capabilities` says `features.matte: false`, the UI shows the AI mode disabled
with the reason, a recipe with a `matte` op is refused). Choose **where** it runs by the profile
you start — one of the two, never both (they share the `matte` network alias):

```sh
docker compose --profile matte up -d        # CPU  (duckautomata/ezlg-matte:<tag>): isnet-anime at 512², ~136 ms/frame on 8 cores
docker compose --profile matte-gpu up -d    # CUDA (…:<tag>-cuda): nvidia-container-toolkit on bare-metal Linux / inside a WSL
                                            #   distro, host driver ≥ 580 (CUDA 13); Docker Desktop's WSL2 backend needs nothing extra
```

or once, in `.env` (copy `.env.example`): `COMPOSE_PROFILES=matte-gpu` makes a plain `up -d`
include it. Choose **which models** the UI offers with `MATTE_MODELS` on that service — a comma
list of ids from `sidecar/models.json`: `isnet-anime` ("Anime (fast)", Apache-2.0, every install;
18 ms/frame on an RTX 5080) and `birefnet-lite` ("General (precise)", MIT; ~7 GB of free VRAM, or
≥ 14 GiB of free RAM on CPU where it takes seconds per frame) — and the preselected one with
`MATTE_DEFAULT_MODEL`. The Model select in the Background card lists exactly what the sidecar
reports and greys a model it cannot run with the sidecar's reason; the estimate line under Render
adds "up to ~N s AI matte (GPU|CPU)" from the sidecar's measured speed. The first start downloads
the weights into the `ezlg-models` named volume (176 MB for isnet-anime, 224 MB more for
birefnet-lite) and self-tests them — the UI says "downloading N %" / "loading" meanwhile;
`docker compose run --rm matte download` fetches them ahead of time — with no ids it fetches
**every** model of `sidecar/models.json`, whatever the service's `MATTE_MODELS` says, so the one
volume serves the `matte` and the `matte-gpu` profile alike (name ids to narrow it:
`… matte download isnet-anime`); air-gapped box: copy the volume, or point
`MATTE_MODELS_BASE_URL` at a mirror. Update the sidecar on its own:
`docker compose pull matte-gpu && docker compose up -d` — a sidecar with new weights re-renders
previews **and** results (the weights' identity is part of every result's hash, so a cached file
never silently mixes with new previews); roll back with `EZLG_MATTE_TAG=<previous tag>` in `.env`.
The tag `compose.yaml` pins exists on Docker Hub only once that release was tagged
(`.github/workflows/docker-image.yml` publishes `:vX.Y.Z` / `:vX.Y.Z-cuda` on a `v*` tag push and
`latest` / the short SHA on every master push); the release step is `git tag -a vX.Y.Z && git push
origin vX.Y.Z`, verified with `docker manifest inspect duckautomata/ezlg-matte:vX.Y.Z-cuda` — until
then run a master build with `EZLG_MATTE_TAG=<short sha>` in `.env`. The sidecar has **no
published port** and no auth: it is reachable as `http://matte:9402` on the compose network only
— keep it that way.

**`/models` ownership (bare-metal Linux, Docker inside a WSL distro):** the sidecar runs as uid
1000 too; the image creates `/models` owned by 1000 so the fresh named volume inherits it. A
volume that was populated by something else fails the first download with `EACCES` in
`docker compose logs matte` — fix it once. The service drops every capability and `chown(2)`
needs `CAP_CHOWN` even as root, so the one-off run adds it back:

```sh
docker compose run --rm --user root --cap-add CHOWN --entrypoint chown matte -R 1000:1000 /models
# (`… chown matte-gpu …` for the CUDA service — same volume), or without touching the service at all:
docker run --rm -v <project>_ezlg-models:/models alpine chown -R 1000:1000 /models   # <project> = the compose project name, usually the folder
```

Sidecar settings (`environment:` of the `matte` / `matte-gpu` service in `compose.yaml`; the app
side is `EZLG_MATTE_URL` and the two caps in the table below):

| Variable | Default | Meaning |
|---|---|---|
| `MATTE_DEVICE` | `cpu` (`matte`) · `cuda` (`matte-gpu`) | `auto` picks CUDA when the EP is available, else CPU with a warning. `cuda` without the EP **stays up and reports** `device: "unavailable"` with the reason (the healthcheck fails, the UI shows AI off with that text) instead of crash-looping |
| `MATTE_MODELS` | `isnet-anime` (cpu) · `isnet-anime,birefnet-lite` (cuda) | Models the sidecar offers — ids from `sidecar/models.json`; nothing non-commercial is in that file |
| `MATTE_DEFAULT_MODEL` | `isnet-anime` | The model the UI preselects (`defaultModel` in `GET /api/matte`) |
| `MATTE_PRELOAD` | = `MATTE_MODELS` | Downloaded and self-tested at start, so a measured ms/frame exists before the first pass |
| `MATTE_MODEL_TTL` | `600` | Seconds idle before a model's session is unloaded (`0` = never — a dedicated box; `120` hands a shared desktop its VRAM/RAM back sooner; isnet-anime reloads in ~1 s, birefnet-lite in 10–20 s, mostly hidden by the warm-up the UI triggers) |
| `MATTE_GPU_MEM_LIMIT_GIB` | `6` | ONNX Runtime arena cap for birefnet-lite (it needs ~6.3 GB — **lower values fail**: the knob is for larger cards, never a remedy for smaller ones; on cards under 10 GiB only one model stays resident) |
| `MATTE_THREADS` | `0` (physical cores) | CPU intra-op threads; lower it when renders share the cores |
| `MATTE_MODELS_BASE_URL` | — | Mirror for the weights (replaces the pinned URL list) |
| `MATTE_PORT` | `9402` | Listen port inside the container (the app's `EZLG_MATTE_URL` must match) |
| `MATTE_MAX_BATCH` | `32` | Frames per `/v1/matte` request the sidecar accepts (the app sends 8) |

**`./output` ownership (bare-metal Linux, Docker inside a WSL distro):** the container runs as
uid 1000 (`ezlg`). If `./output` does not exist when you first run `docker compose up`, the Docker
daemon creates it as `root:root 0755` and the app cannot write to it. Create it first as above, or
fix an existing one with `docker compose run --rm --user root --entrypoint chown app -R 1000:1000
/output`. Docker Desktop on Windows/macOS needs neither — its bind mounts appear world-writable.
See [Troubleshooting](#troubleshooting) for this and the other startup complaints.

## Configuration

Everything is optional; set it under `environment:` in `compose.yaml` (or with `-e` on
`docker run`).

| Variable | Default | Meaning |
|---|---|---|
| `EZLG_ADDR` | `:8080` | Listen address (change `ports:` and the healthcheck too) |
| `EZLG_DATA` | `/data` | Data root: blobs by sha256, results by recipe hash |
| `EZLG_SCRATCH` | `/dev/shm/ezl` | Scratch (tmpfs) root for frame masters; falls back to `$TMPDIR/ezl` |
| `EZLG_TTL_HOURS` | `24` | Delete blobs/results older than this (`0` = never) |
| `EZLG_MAX_BYTES` | `21474836480` (20 GiB) | Cap on total `/data` size (`0` = none) |
| `EZLG_MAX_UPLOAD_MB` | `2048` | Maximum upload size |
| `EZLG_CONCURRENCY` | `max(1, NumCPU/2)` | Concurrent renders |
| `EZLG_MAX_MASTER_BYTES` | `2147483648` (2 GiB) | Cap on one render's RGBA frame master (frames × W × H × 4 at the **output** size, so trim / crop / resize / fit all shrink it) and — the only bound there is — on the RAM buffer of `reverse` / `bounce`. Previews (still, Play) of any source work whatever its length: the cap binds renders, plus reversed/bounced previews and static (png/jpeg) *reversed* renders through their reverse buffer (a bounce alone costs a static render one decoded frame — nothing is buffered before its first frame). The UI shows the estimate under Render (size · frames · bytes · scratch multiple; a reversed png/jpeg also shows the RAM its reverse buffers) and, when over, how many frames / seconds fit at that size (seconds floored to a tenth, so a trim to them fits); a render over the cap is refused before ffmpeg runs (`… the limit is 2 GiB — trim the clip, lower the fps or resize the output`). For fit / AVIF / frames / gifski renders the scratch byte budget (`scratchBudgetBytes` in capabilities: the scratch filesystem — `shm_size` — minus what the still/proxy memos may hold, 768 MiB by default) can bind first, at 2–3× the master — the UI shows that too, naming the budget. `0`, negative or unparsable → default; clamped at `math.MaxInt64/16`; keep it below `shm_size` **and** host RAM (past RAM you get OOMs, not refusals) |
| `EZLG_INPUT` | `/input` | Folder the in-app `/input` picker lists (mount it read-only). Checked once at startup (exists + readable) → `features.inputPick`; the listing itself is per-request, so new files appear without a restart |
| `EZLG_OUTPUT` | `/output` | Folder "Save to /output" writes to (must be writable by uid 1000 — see [Quick start](#quick-start-from-a-source-checkout)). Checked once at startup (exists + writable) → `features.outputSave` |
| `EZLG_MATTE_URL` | `""` (AI mattes off) | Base URL of the AI matte sidecar; `compose.yaml` sets `http://matte:9402` and the `matte` / `matte-gpu` profiles start the service ([AI background removal](#ai-background-removal-optional-matte-sidecar)). Empty = off: `features.matte` false, a recipe with a `matte` op is refused with 400. Probed every 30 s and the feature follows the answers — off after 3 consecutive misses, back on at the next answer. Previews (still, Play) of a recipe whose matte is already on disk are served from the cache either way; a render needs the feature on (the 400 above) |
| `EZLG_MATTE_MAX_SECONDS` | `600` | Cap on the **estimated** wall time of one AI matte pass (frames × the sidecar's measured ms/frame); a longer clip is refused up-front with a message asking to trim, lower the fps or pick the fast model. `≤ 0` keeps the default |
| `EZLG_MATTE_MAX_FRAMES` | `3000` | Cap on the frames one AI matte pass may send to the sidecar, applied up-front from the plan and at run time from the frames actually streamed (so an unknown count is bounded too). `≤ 0` keeps the default |
| `EZLG_FFMPEG`, `EZLG_FFPROBE`, `EZLG_GIFSICLE`, `EZLG_GIFSKI`, `EZLG_IMG2WEBP`, `EZLG_WEBPINFO`, `EZLG_AVIFENC`, `EZLG_AVIFDEC`, `EZLG_PNGQUANT`, `EZLG_OXIPNG`, `EZLG_FC_LIST` | found on `PATH` | Override a tool path (`fc-list` feeds `GET /api/fonts`; without it the font list is empty and text overlays still resolve bundled families by name) |

Practical limits at the default 2 GiB cap, measured at the *output* size (max frames =
⌊2 GiB / (W × H × 4)⌋; seconds at 30 fps): 128 × 128 (emote) ≈ 32768 frames ≈ 18 min; 480 × 270
≈ 4142 frames ≈ 2 min 18 s; 1920 × 1080 → 258 frames ≈ 8.6 s; 2560 × 1440 → 145 frames ≈ 4.8 s.
An emote or sticker from a long 4K clip is small because the master is measured after the fit —
trim and crop in the app first (previews work on any source); what the cap refuses is a
full-resolution export of a long clip (a streaming encode without a master is a later item).

Volumes / mounts: `/data` (required, keep it on a Linux filesystem), `/output` (optional, rw,
must be writable by uid 1000 — see [Quick start](#quick-start-from-a-source-checkout)), `/input`
(optional, ro), `/fonts` (optional, ro: extra fonts for the text overlay, on fontconfig's scan
path), `/dev/shm` sized by `shm_size`. The matte sidecar keeps its weights, derived graphs and
CUDA JIT cache in the `ezlg-models` named volume at `/models` (owned by uid 1000; `docker compose
down -v` removes it with `ezlg-data`).

## HTTP API

Everything the UI does goes through this JSON API (errors are `{"error": "message"}` with a
4xx/5xx status; the full contract is the package comment of `internal/server/server.go`, the
recipe schema is `internal/recipe`). `curl` works as-is; browsers are held to same-origin.

| Method / path | What |
|---|---|
| `POST /api/upload` | multipart `file` → `Source` (`hash`, `name`, `size`, `info` = probe: format, codec, W×H, fps, frames, alpha, kind, premultiplied guess). Several `file` parts that are all images → one **image-sequence** source (optional `delayMs`, default 100). |
| `POST /api/sources/from-result` | `{"recipeHash": "…", "name": "out.gif"}` → copies that result file into the blob store and probes it → `Source` (**edit as source**) |
| `GET /api/input` | `{"files": [{"name", "size", "mtime"}, …]}` — flat listing of the `/input` mount (decodable extensions only, name-sorted, capped at 500; always an array). 503 when `/input` is not available (`features.inputPick` false) |
| `POST /api/sources/from-input` | `{"name": "clip.mov"}` → ingests that `/input` file exactly like an upload (copy, sha256 dedupe, probe) → `Source`. 404 unless `name` exactly matches a listed entry (no paths); 503 without `/input` |
| `GET /api/sources/{hash}` | `Source` |
| `GET /api/fonts` | `{"fonts": [{"family", "style", "file"}, …]}` — the faces the `text` op can use (`fc-list` inside the container; an empty array without it) |
| `POST /api/still` | `{"src": hash, "ops": […], "output": {…}, "t": 1.5, "maxW": 480}` → `image/png` preview frame. Recipes with overlay sources send `"sources": [main, overlay, …]` instead of (or agreeing with) `src`; 404 for an unknown source, 409 for one not probed yet. With a `matte` op whose AI matte is not on disk yet: **202** `{"pending": "matte", "state": running · deferred · loading · downloading, "done", "total", "percent", "estimateMs", …the `GET /api/matte` object}` — the pass runs (or waits for the model); re-request after ~500 ms (the UI keeps the last picture on stage meanwhile). `"eager": true` starts a pass the server would otherwise defer as too long for a scrub (Play, "Compute now") |
| `POST /api/proxy` | `{"sources": [hash, …], "ops": […], "output": {…}, "maxW": 360, "maxSeconds": 10}` → `image/webp`: the animated low-res **Play** preview (first `maxSeconds` of the op stack at ≤ 15 fps, lossy, with alpha); 400 for a missing source; 202 while a `matte` op's AI matte is pending, as for stills |
| `POST /api/jobs` | a `Recipe` (`{"v":1,"sources":[hash, …],"ops":[…],"output":{…}}`; overlay assets after the main source, referenced by index from `overlay` ops) → `202 Job`; the result is served from cache when the same recipe was rendered before; 400 for a source that is not uploaded, 409 for one not probed yet, 400 naming the compose profile for a `matte` op while `features.matte` is false |
| `GET /api/matte` | the AI matte sidecar as the app last saw it: `{"enabled", "device"` (`cuda` · `cpu` · `unavailable`)`, "reason", "gpu": {"name", "totalGiB", "freeGiB"}` or `null`, `"defaultModel", "models": {id: {"label", "state"` (`ready` · `loading` · `downloading` · `missing` · `unavailable`)`, "percent", "msPerFrame", "reason", "licence", "sizes"}}, "maxSeconds", "maxFrames"}`. `enabled` is `features.matte`; the UI polls it every 5 s while the AI mode is visible or a pass is pending |
| `GET /api/jobs/{id}` · `DELETE /api/jobs/{id}` · `GET /api/jobs/{id}/events` | poll, cancel, or follow a job (SSE: `event: progress|done|error`, `data: Event`) |
| `GET /api/results/{recipeHash}` | the result manifest (`files[]` with `name`, `url`, `bytes`, W×H, frames, fps, `report` = Discord lint, `kind` = `output` / `alternative` / `frame` / `archive`, `desc` = binding fit knob / encoder path taken) |
| `POST /api/results/{recipeHash}/save` | `{"file": "<name from the manifest>", "name": "<optional base>"}` → writes that result file to `/output` under a collision-safe name (`base.ext`, `base-2.ext`, …) and returns `{"name": "<final name>"}`. 404 unknown result/file; 503 without `/output` (`features.outputSave` false) |
| `GET /out/{recipeHash}/{name}` | a result file (immutable; `?dl=1` adds `Content-Disposition: attachment` named after the source) |
| `GET /api/capabilities` | tool versions, Discord byte limits, lint rules version, concurrency, max upload, formats, `features` (`fit`, `sequence`, `optimize`, `keying`, `overlays`, `proxy`, `fonts` = whether `/api/fonts` lists anything, `feather`, `bounce`, from Phase 4 `inputPick` / `outputSave` = whether `/input` / `/output` are mounted and usable and `gifski` = whether gifski is installed, from Phase 5a `morph` = the `morph` op is understood — the UI gates its Edge cleanup fold on it — and from Phase 5b `matte` = the AI matte sidecar answers (`EZLG_MATTE_URL` set and a `matte` / `matte-gpu` profile running; `GET /api/matte` has the detail)), and from 2026-09-13 `maxMasterBytes` (the frame-master cap in bytes — `EZLG_MAX_MASTER_BYTES` after clamping) and `scratchBudgetBytes` (the scratch byte budget renders reserve from — the scratch filesystem minus the still/proxy memo allowance, not `shm_size` itself; `0` = unlimited / unknown) — the UI's estimate line under Render is computed against both |
| `GET /healthz` | `ok` |

`Output` fields that matter most: `format` (`gif` · `webp` · `apng` · `avif` · `png` · `jpeg` ·
`frames` · `mp4` · `webm` — the video formats are opaque, even-dimensioned, and accept only the
attachment targets or none), `width`/`height`/`fit`, `fps`, `quality` (webp/avif 1–100; **the CRF
itself** for mp4/webm, 0 = 20/30; gifski `--quality` when `encoder` is `gifski`) / `lossless`
(webp), `encoder` (gif only: empty = ffmpeg palette, `"gifski"` = the HQ toggle — refused for
emote/sticker targets), `lossy` / `colors` / `dither` / `alphaThreshold` (gif), `matte` (gif and
the video formats: the flatten colour), `loop`, `fitBytes` (+ `fitKeepSize`, `fitKeepFps`),
`frameFormat` (frames: `png` · `jpeg` · `webp`), `preset` (UI label: `emote` ·
`sticker` · `chat` · `optimize` · `frames` · `custom`) and `target` (which Discord rules and byte
limit the linter enforces: `emote` · `sticker` · `attachment` (+ `-50` / `-100` / `-500` tiers) ·
none).

Ops (`{"kind": …, "params": {…}}`, see `internal/recipe`): `trim`, `crop`, `resize`, `canvas`,
`fps`, `speed`, `flip`, `rotate`, `unpremultiply`, `delay` (sequences), from Phase 3
`chromakey` (`color` — RRGGBB, empty = `00ff00`; `similarity` 0.01–1, **0 = the default 0.1**;
`blend` 0–1, 0 = the default 0.05; `despillOff`, `despillMix` 0 = 0.6, `despillExpand` 0 = 0.3 —
despill is applied only for a green- or blue-dominant key colour), `colorkey` (`color` — required;
`similarity` 0.01–1, **0 = the default 0.08**; `blend`, 0 = none — an RGB-distance key with a hard
edge; several `colorkey` ops stack, each removing its own colour, which is what the UI's "+ add
colour" sends), `feather` (`radius` = Gaussian sigma in source pixels, 0 = the default 3), from
Phase 5a (2026-10-08) `morph` (`close`: fill pinholes of up to 1 px — a 3×3 dilation then erosion
on the alpha plane, never growing the silhouette; `grow`: 0–4 extra dilations, in source pixels
like `feather`; at least one of the two must be set; it acts on the alpha the keys before it in the
stack produced and is skipped on frames without alpha — the UI sends it after the keys and before
`feather`, and auto-crop detects on the cleaned matte), from Phase 5b `matte` (AI background
removal — needs the sidecar, [above](#ai-background-removal-optional-matte-sidecar); `model`: an
id the sidecar offers, `GET /api/matte` lists them, empty = its `defaultModel`; `size`: the
model's input square, API-only, 0 = the sidecar's default for its device; `resolved` is filled by
the server from the sidecar's facts — weights sha256, processing version, size, precision — and is
what makes a sidecar with new weights a new result; anything a client sends in it is dropped. The
op is hoisted into the keying group with `chromakey` / `colorkey` / `morph` / `feather` in stack
order: on opaque frames the matte becomes the alpha, on frames that already carry alpha it is
multiplied in; the model sees the RGB after trim / speed / fps only, so its memoised mattes
survive every crop, resize, key and feather change; previews answer 202 while a matte is being
computed; refused with 400 while `features.matte` is false), `reverse`,
`autocrop` (`threshold`, `padding`), `text` (`text`, `font` = fontconfig
family, `size`, `color`, `border`, `box`, `anchor`, `x`/`y`, `start`/`end`) and `overlay`
(`source` = index into `sources`, `width`/`height`, `opacity`, `noLoop`, `anchor`, `x`/`y`,
`start`/`end`), and from Phase 4 `bounce` (no params: forward then back — 2× frames and
duration). The Phase 5a defaults (chromakey 0.1, colorkey 0.08 — before: 0.2 / 0.1) and the fixed
chromakey key colour (the exact screen colour now keys at any similarity ≥ 0.02; before, nothing
below ~0.05 keyed) changed what keyed recipes render to, so results cached before 2026-10-08 are
re-rendered on the next request. `scripts/integration-test.sh` covers `POST /api/upload`, `POST
/api/sources/from-result`, `GET /api/sources/{hash}`, `GET /api/fonts`, `POST /api/still`, `POST
/api/proxy`, `POST /api/jobs`, `GET /api/jobs/{id}` (polling), `GET /out/{hash}/{name}` (with and
without `?dl=1`), `GET /api/capabilities` and `GET /healthz`, including the Phase 3 and Phase 4
ops, plus — when it starts the server itself (`EZLG_START_SERVER=1`) — `GET /api/input`, `POST
/api/sources/from-input` and `POST /api/results/{recipeHash}/save` against temp `/input` and
`/output` dirs, and — when `EZLG_TEST_MATTE_URL` names a running sidecar — the Phase 5 `matte`
cases (`GET /api/matte`, GIF / WebP / APNG with a `matte` op, the same recipe twice → cached, a
pending still, matte + autocrop; the "`matte` op refused with 400 while the feature is off" case
runs against whichever server has it off; the pixel assertions name the general `birefnet-lite`
model when the sidecar lists it ready — the `matte-gpu` profile does, the CPU `matte` profile
ships `isnet-anime` only, which does not detect the synthetic figure the script draws, so those
assertions are then skipped with a note and the pipeline cases still run); it does
**not** request `GET /api/results/{recipeHash}`, `DELETE /api/jobs/{id}` or the SSE stream `GET
/api/jobs/{id}/events`.

## Running on WSL2 (Windows workstation)

Docker Desktop with the WSL2 backend or Docker inside a WSL distro both work; the compose file is
the same. Notes:

- **Keep `/data` on the WSL ext4 filesystem** — the default named volume does this. Do not bind
  `/data` to a `/mnt/c/...` path (9P is very slow for many small files).
- **`./output` ownership:** with Docker Desktop and the repo on the Windows drive there is nothing
  to do. With the repo (and/or Docker) inside a WSL distro the bind is a real Linux directory, so
  run `mkdir -p output && sudo chown 1000:1000 output` before the first `up`
  ([Quick start](#quick-start-from-a-source-checkout)), or the
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
- **GPU:** the app itself uses none (all animated-image encoders are CPU; the commented `gpu`
  block in `compose.yaml` and [`DESIGN.md`](DESIGN.md) §6 describe the later NVDEC/NVENC option,
  host driver ≥ 610). AI background removal runs on the GPU through the `matte-gpu` profile
  instead ([above](#ai-background-removal-optional-matte-sidecar)): Docker Desktop's WSL2
  backend passes the card through with nothing extra installed (host driver ≥ 580); Docker
  inside a WSL distro or bare-metal Linux needs nvidia-container-toolkit. The `ezlg-models`
  volume stays on ext4 like `/data`.

## Troubleshooting

The first three are ownership / sizing problems between the container (uid 1000, `/dev/shm`
scratch) and what the Docker daemon hands it — `docker compose logs app` shows the exact line;
then one about a rendered file, and the last two about the AI matte sidecar.

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
Since Phase 4 the server writes there too ("Save to /output"): at startup it checks the folder
once and, when it is missing or unwritable, simply disables the feature (`features.outputSave`
false — the UI hides the button) instead of failing, so a root-owned `./output` shows up as a
missing Save button plus this test-kit message — the kit checks up front and stops with this
hint before running the matrix. Fix on the host: `mkdir -p output && sudo chown -R 1000:1000 output`;
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

**A transparent GIF "stacks" on Discord — each new pose is drawn on top of the old one — although
it plays correctly in a browser.** GIFs rendered before the 2026-09-19 fix could do this when the
subject holds still between poses: Discord drops a frame that does not change the picture, and
with it the "clear this area" disposal gifsicle's optimiser had parked on that frame
([`DESIGN.md`](DESIGN.md) §5.2 item 9). Render the recipe again — the fix bumped the result-cache
versions, so it is re-rendered rather than served from cache — and upload the new file. The
result card's Discord check `gif.noop-frame-disposal` names such frames when a GIF has them (an
error for Discord targets, a warning for target none). When a render, an Optimize run or a fit
candidate comes out with such frames the app re-encodes it by itself, for every target (Optimize
and fit results then say "held frames re-encoded for Discord" in their description); you only
see the check fail when that repair was not possible — for example no gifsicle on the server.

**The Background card's AI mode is greyed — "needs the matte sidecar — `docker compose --profile
matte up -d`"** (`GET /api/matte` says `enabled: false`). Either no profile is running
(`docker compose ps` lists neither `matte` nor `matte-gpu` — start one; the plain stack never
includes it), `EZLG_MATTE_URL` was cleared in `compose.yaml`, or the sidecar is up but not
answering: `docker compose logs matte-gpu` (or `matte`). When it does answer, `reason` in
`GET /api/matte` is the sidecar's own text: `device: unavailable — CUDA EP not available` means
the container cannot see the GPU (nvidia-container-toolkit missing on a Linux host, or a driver
older than 580 — check `nvidia-smi` on the host; on Docker Desktop that GPU support is on); a
model `missing` with a `lastError` means the download failed (proxy / air-gapped box:
`MATTE_MODELS_BASE_URL`, or pre-seed with `docker compose run --rm matte download`);
`unavailable` with "needs about 7 GB of free GPU memory" means birefnet-lite does not fit next to
whatever else uses the card — pick the fast model; `EACCES` under `/models` in the sidecar log is
the ownership repair line in [AI background removal](#ai-background-removal-optional-matte-sidecar).
The app flips the feature off only after three consecutive missed probes (about 90 s) and back on
at the first answer, so a sidecar restart shows as a short gap, not a stuck state; a `Model loading`
/ `downloading N %` pill in the UI is the sidecar working, not an error. `docker compose ps`
showing `(health: starting)` for a few minutes on the **first** `matte-gpu` start is the same
thing: nothing listens until the CUDA session exists (context plus a first-time PTX JIT, cached in
the volume from then on) and the preloaded weights (400 MB) are down — the start period is 300 s;
`unhealthy` after that is a real fault (`docker compose logs matte-gpu`).

**`two matte sidecars answer at http://matte:9402`** in the app log: both profiles are up and DNS
alternates between them, so mattes would come from whichever answers. Stop one
(`docker compose --profile matte stop matte` keeps `matte-gpu`) and keep a single profile in `.env`.
