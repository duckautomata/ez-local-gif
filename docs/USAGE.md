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
path), `/dev/shm` sized by `shm_size`.

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
| `POST /api/still` | `{"src": hash, "ops": […], "output": {…}, "t": 1.5, "maxW": 480}` → `image/png` preview frame. Recipes with overlay sources send `"sources": [main, overlay, …]` instead of (or agreeing with) `src`; 404 for an unknown source, 409 for one not probed yet |
| `POST /api/proxy` | `{"sources": [hash, …], "ops": […], "output": {…}, "maxW": 360, "maxSeconds": 10}` → `image/webp`: the animated low-res **Play** preview (first `maxSeconds` of the op stack at ≤ 15 fps, lossy, with alpha); 400 for a missing source |
| `POST /api/jobs` | a `Recipe` (`{"v":1,"sources":[hash, …],"ops":[…],"output":{…}}`; overlay assets after the main source, referenced by index from `overlay` ops) → `202 Job`; the result is served from cache when the same recipe was rendered before; 400 for a source that is not uploaded, 409 for one not probed yet |
| `GET /api/jobs/{id}` · `DELETE /api/jobs/{id}` · `GET /api/jobs/{id}/events` | poll, cancel, or follow a job (SSE: `event: progress|done|error`, `data: Event`) |
| `GET /api/results/{recipeHash}` | the result manifest (`files[]` with `name`, `url`, `bytes`, W×H, frames, fps, `report` = Discord lint, `kind` = `output` / `alternative` / `frame` / `archive`, `desc` = binding fit knob / encoder path taken) |
| `POST /api/results/{recipeHash}/save` | `{"file": "<name from the manifest>", "name": "<optional base>"}` → writes that result file to `/output` under a collision-safe name (`base.ext`, `base-2.ext`, …) and returns `{"name": "<final name>"}`. 404 unknown result/file; 503 without `/output` (`features.outputSave` false) |
| `GET /out/{recipeHash}/{name}` | a result file (immutable; `?dl=1` adds `Content-Disposition: attachment` named after the source) |
| `GET /api/capabilities` | tool versions, Discord byte limits, lint rules version, concurrency, max upload, formats, `features` (`fit`, `sequence`, `optimize`, `keying`, `overlays`, `proxy`, `fonts` = whether `/api/fonts` lists anything, and from Phase 4 `inputPick` / `outputSave` = whether `/input` / `/output` are mounted and usable), and from 2026-09-13 `maxMasterBytes` (the frame-master cap in bytes — `EZLG_MAX_MASTER_BYTES` after clamping) and `scratchBudgetBytes` (the scratch byte budget renders reserve from — the scratch filesystem minus the still/proxy memo allowance, not `shm_size` itself; `0` = unlimited / unknown) — the UI's estimate line under Render is computed against both |
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
`chromakey` (`color`, `similarity`, `blend`, despill knobs), `colorkey` (`color`, `similarity`,
`blend`), `feather` (`radius` = Gaussian sigma in source pixels, 0 = the default 3), `reverse`,
`autocrop` (`threshold`, `padding`), `text` (`text`, `font` = fontconfig
family, `size`, `color`, `border`, `box`, `anchor`, `x`/`y`, `start`/`end`) and `overlay`
(`source` = index into `sources`, `width`/`height`, `opacity`, `noLoop`, `anchor`, `x`/`y`,
`start`/`end`), and from Phase 4 `bounce` (no params: forward then back — 2× frames and
duration). `scripts/integration-test.sh` covers `POST /api/upload`, `POST
/api/sources/from-result`, `GET /api/sources/{hash}`, `GET /api/fonts`, `POST /api/still`, `POST
/api/proxy`, `POST /api/jobs`, `GET /api/jobs/{id}` (polling), `GET /out/{hash}/{name}` (with and
without `?dl=1`), `GET /api/capabilities` and `GET /healthz`, including the Phase 3 and Phase 4
ops, plus — when it starts the server itself (`EZLG_START_SERVER=1`) — `GET /api/input`, `POST
/api/sources/from-input` and `POST /api/results/{recipeHash}/save` against temp `/input` and
`/output` dirs; it does
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
- **GPU:** not used in v1 (all animated-image encoders are CPU). The commented `gpu` block in
  `compose.yaml` and [`DESIGN.md`](DESIGN.md) §6 describe the later NVDEC/NVENC/rembg option; it
  needs nvidia-container-toolkit and a host driver ≥ 610.

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
