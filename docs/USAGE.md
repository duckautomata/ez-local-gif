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
with the reason, a recipe with a `matte` op is refused). Choose **which sidecar** by the profile
you start — one of the two, never both (they share the `matte` network alias):

```sh
docker compose --profile matte up -d        # CPU  (duckautomata/ezlg-matte:<tag>): isnet-anime at 512², ~136 ms/frame on 8 cores
docker compose --profile matte-gpu up -d    # CUDA (…:<tag>-cuda): nvidia-container-toolkit on bare-metal Linux / inside a WSL
                                            #   distro, host driver ≥ 580 (CUDA 13); Docker Desktop's WSL2 backend needs nothing extra.
                                            #   Offers the GPU AND the CPU (Phase 5c) — "Run on" in the card picks per pass
```

or once, in `.env` (copy `.env.example`): `COMPOSE_PROFILES=matte-gpu` makes a plain `up -d`
include it. **Where a pass runs** (Phase 5c, 2026-10-09): the sidecar's `MATTE_DEVICE` is only its
*default* device — the CUDA image offers `cuda` and `cpu` side by side, and the Background card
shows a **Run on: GPU / CPU** select whenever the sidecar lists both (`GET /api/matte` →
`devices`). The choice is a server-side setting (`PUT /api/matte/settings`, persisted under
`/data/mattes/settings.json`; `EZLG_MATTE_DEVICE` is its startup value), never part of a recipe
— a matte's identity is its weights, input size and precision, so CPU and GPU passes of one
graph share one cache. **Nothing stays loaded:** at start the sidecar downloads and self-tests
every offered model on each offered device (so the estimate line can be honest) and releases
them again; a pass loads what it needs, the sidecar drops it after `MATTE_MODEL_TTL` (300 s)
idle, and the app asks it to drop everything the moment the card leaves the AI mode
(`POST /api/matte/unload`). **Nothing runs by itself:** picking AI, a model or a device starts no pass —
the preview shows the last picture with an "AI matte not computed — Compute" pill until you press
**Compute matte** (or Render, which always computes; `POST /api/still` without `"eager": true`
answers 202 `"state": "idle"` and never starts a pass).

Choose **which models** the UI offers with `MATTE_MODELS` on that service — a comma list of ids
from `sidecar/models.json`: `isnet-anime` ("Anime (fast)", Apache-2.0, every install; 18 ms/frame
on an RTX 5080; anime-style characters only — it detects nothing on other content),
`birefnet-lite` ("General (precise)", MIT; ~7 GB of free VRAM, or ≥ 14 GiB of free RAM on CPU
where it takes seconds per frame; keeps thin strands and props and is **stable on video** —
IoU 0.96–0.99 on every frame of a moving subject) and, from Phase 5c, `sam2-tiny` ("Guided (click
to select)", Apache-2.0: the SAM 2.1 hiera-tiny tracker behind the box / click prompts, below).
The preselected model is **per device**: `MATTE_DEFAULT_MODEL_CUDA` (ships as `birefnet-lite`)
and `MATTE_DEFAULT_MODEL_CPU` (`isnet-anime`), or `MATTE_DEFAULT_MODEL` for both; `GET /api/matte`
reports `defaultModels` per device and `defaultModel` for the device in use. The Model select in
the Background card lists exactly what the sidecar reports and greys a model it cannot run with
the sidecar's reason; the estimate line under Render adds "up to ~N s AI matte (GPU|CPU)" from the
sidecar's measured speed on the device in use. The first start downloads the weights into the
`ezlg-models` named volume (176 MB for isnet-anime, 224 MB more for birefnet-lite, 156 MB for
sam2-tiny) and self-tests them — the UI says "downloading N %" / "loading" meanwhile;
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

**Fixing a matte that flickers or drops parts** (Phase 5c; the measurements are in
[`reviews/background-removal-stabilise-and-guided-2026-10-09.md`](reviews/background-removal-stabilise-and-guided-2026-10-09.md)):
the per-frame models judge every frame on its own, so a part can blink in and out. **Stabilise**
(Off / **Light** — the card's default / Strong, in the AI section) post-processes the matte
*sequence* with a temporal filter before it is merged: Light is a centred 3-frame median — every
single-frame pop in either direction disappears with no lag (isnet-anime's worst frame went from
IoU 0.935 to 0.986; BiRefNet-lite is unchanged within 0.001); Strong adds a decay-0.7 hold after
the median, keeping parts that drop out for a frame at the cost of a short (half-frame) trail on
fast motion. It runs once per clip (~0.1 s per 45 frames) after the pass, is cached next to the
mattes, and previews and renders read the same derived sequence; it cannot recover a part the
model misses for three or more frames. **Keep colours** (the AI section's Advanced fold: up to
six eyedropper picks with one similarity, default 0.08) force every pixel of those colours
opaque — a union with the matte — for props, outlines or skin the model drops; they change the
picture at once, with no new pass. And when a model keeps losing the subject, the **Guided**
model is told what to follow: pick "Guided (click to select)", draw a box around the character
on the current frame (or click it two or three times; Shift-click / right-click adds a removal
point — one click alone is unreliable, it selects a part), watch the frame's mask overlay, add a
box on another frame where it drifts, then **Compute matte**: SAM 2 tracks the object through
the whole clip (backward and forward from the earliest prompted frame; on the user's stream
capture it removed the UI buttons both per-frame models bleed into) and its coarse mask **gates**
the per-frame model of the **Edge** select ("General (precise)" on the GPU, "Anime (fast)" on the
CPU, or "None — tracker mask only") inside a 3 px band, so the edges are the per-frame model's
and the stability the tracker's. The most reliable start needs no drawing (the 5c follow-up,
2026-10-09 — measured on the live stack: a box slightly larger than the character tracks right,
centre alpha 255 and corners 0, but a loose box over most of the frame makes SAM 2 select the
gradient inside it, centre alpha 0, IoU 0.004 against the per-frame matte, while the per-frame
model's matte of one good frame tracked at IoU 0.997): run General first (Compute), scrub to a
frame where it got the character right, switch to Guided and press **Use this frame's matte**
(enabled unless the server has said that matte is not computed for this clip — the panel's
overlay asks at once and says so; before any answer the button's hint hedges): the prompt
`{frame, maskFrom: "edge"}` replaces any box or points on that frame, the panel shows the tracker's mask for it at
once (the prompted-frames strip lists it as "f N ▣"), − clicks on the frame refine it, and
Compute tracks from it. The browser never sends the mask: the server takes frame N's matte from
the **Edge** model's own memo (the model the Edge select names — "None — tracker mask only"
cannot supply one, so the button is disabled there and such a recipe is refused), scales it to
the tracking size and appends it to the tracker request, and the digest of the mask actually
sent enters the matte's cache key, so a new edge model or sidecar re-tracks. Before that matte
exists the overlay answers "compute the General matte first" (a 202 `idle` whose own
`pendingReason` carries the sentence; while the Edge model's pass for the clip is in flight the
overlay shows that pass's progress instead; a mask prompt never starts a pass, and a clip the
track would refuse up front — the caps, a prompt past the clip — is refused before the Edge
pass runs). And when a box selection looks like the background — its live mask covers more
than 60 % of the frame or touches two or more of its edges — the panel warns "That looks like
the background — tighten the box to the character or add a + click on it" and changes nothing by
itself. The prompts are part of the recipe (`prompts` on the `matte`
op: coordinates in 0..1 of the source frame, `frame` = the output frame index), so a guided
result is reproducible and cached per prompt set; the clip goes to the tracker at a tracking
size (long side ≤ 1024 px: ~30 ms/frame and ~1.2 GB of VRAM per 45 frames on an RTX 5080;
`MATTE_MAX_TRACK_FRAMES` 3000 caps one request). The CUDA image carries PyTorch for it next to
onnxruntime (they share the NVIDIA wheels; triton and cuSOLVER are removed after the install,
the other CUDA libraries stay because libtorch links them at load time). Measured: the cuda
image is 4.9 GB installed (`docker images` on Docker Desktop shows 10.7 GB, counting the
compressed blobs too) against the 5b image's 2.6 GB of `/usr` (7.5 GB in `docker images`) —
it grew by the PyTorch runtime, it did not shrink; the CPU image grows by about 1 GB (1.2 GB
installed) and runs the tracker in fp32, slowly. SAM 3 is not shipped (gated, non-OSI licence).

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
side is `EZLG_MATTE_URL`, the two caps and `EZLG_MATTE_DEVICE` in the table below):

| Variable | Default | Meaning |
|---|---|---|
| `MATTE_DEVICE` | `cpu` (`matte`) · `cuda` (`matte-gpu`) | The sidecar's **default** device (Phase 5c: the CUDA image offers `cuda` **and** `cpu`; `/v1/ping` lists them as `devices`, the app's "Run on" select and `EZLG_MATTE_DEVICE` pick per pass). `auto` picks CUDA when the EP is available, else CPU with a warning. `cuda` without the EP **stays up and reports** `device: "unavailable"` with the reason (the healthcheck fails, the UI shows AI off with that text) instead of crash-looping |
| `MATTE_MODELS` | `isnet-anime,birefnet-lite,sam2-tiny` on every device (General on the CPU is gated by 14 GiB of available RAM: below that it is listed `unavailable` with the figure) | Models the sidecar offers, on every device it offers — ids from `sidecar/models.json`; nothing non-commercial is in that file |
| `MATTE_DEFAULT_MODEL` | — | Phase 5c: the preselected model on **every** device (overrides the two below); `defaultModel` in `GET /api/matte` is the one for the device in use |
| `MATTE_DEFAULT_MODEL_CUDA` / `MATTE_DEFAULT_MODEL_CPU` | `birefnet-lite` / `isnet-anime` | The preselected model per device (`defaultModels` in `GET /api/matte`) — General on the GPU because it is stable on video; Anime on the CPU because General there takes seconds per frame |
| `MATTE_PRELOAD` | `""` | Phase 5c: **every offered model is downloaded and self-tested on each offered device at start, then released** — so a measured ms/frame exists before the first pass and nothing stays resident. A comma list of ids keeps those resident instead (a dedicated box; birefnet-lite otherwise reloads in 2–20 s at the first pass) |
| `MATTE_MODEL_TTL` | `300` | Seconds idle before a model's sessions are released (`0` = never — a dedicated box; `120` hands a shared desktop its VRAM/RAM back sooner; isnet-anime reloads in ~1 s, birefnet-lite in 2–20 s, sam2-tiny in 0.3–0.6 s warm / ~5 s cold — a "loading model…" pill while the first pass starts). The app also releases everything when the card leaves the AI mode |
| `MATTE_MAX_TRACK_FRAMES` | `3000` | Phase 5c: frames one guided (`sam2-tiny`) request may carry (`POST /v1/track`; the app's `EZLG_MATTE_MAX_FRAMES` applies first); ~1.2 GB of VRAM per 45 frames at 720² |
| `MATTE_TRACK_OFFLOAD_FRAMES` | `300` | Phase 5c: a guided request of more frames keeps SAM 2's per-frame memory bank in host RAM instead of VRAM (~1.2× slower, no out-of-memory on long clips on an 8 GB card); `0` = always |
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
| `EZLG_MATTE_DEVICE` | `""` (the sidecar's default) | Phase 5c: the device AI matte passes run on when the sidecar offers several (`cuda` / `cpu`). Only the **startup** value of a server-side preference: the card's "Run on" select (`PUT /api/matte/settings`) changes it at run time and persists it under `/data/mattes/settings.json`, which then wins over this variable. Validated against the sidecar's offered devices; never part of a recipe, a result hash or a matte memo key (CPU and GPU mattes of one graph share one cache) |
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
| `POST /api/still` | `{"src": hash, "ops": […], "output": {…}, "t": 1.5, "maxW": 480}` → `image/png` preview frame. Recipes with overlay sources send `"sources": [main, overlay, …]` instead of (or agreeing with) `src`; 404 for an unknown source, 409 for one not probed yet. With a `matte` op whose AI matte is not on disk yet: **202** `{"pending": "matte", "state": idle · running · loading · downloading, "done", "total", "percent", "estimateMs", "phase" (only `"tracking"` while a guided pass's one POST is in flight: the masks arrive together at its end, so `done` stays 0 meanwhile — the UI says "tracking N frames"), …the `GET /api/matte` object}`. Phase 5c: a plain still **never starts a pass** — with no memo and no pass in flight it answers `"state": "idle"` at once (the UI shows "AI matte not computed — Compute"); `"eager": true` (the Compute matte button only — Play behaves like a still and is not eager) starts the pass, after which every still answers `running` / `loading` / `downloading` with the progress until the matte is on disk — re-request after ~500 ms (the UI keeps the last picture on stage meanwhile). The 5b `deferred` state and its 90 s bound are gone |
| `POST /api/proxy` | `{"sources": [hash, …], "ops": […], "output": {…}, "maxW": 360, "maxSeconds": 10}` → `image/webp`: the animated low-res **Play** preview (first `maxSeconds` of the op stack at ≤ 15 fps, lossy, with alpha); 400 for a missing source; 202 while a `matte` op's AI matte is pending, as for stills |
| `POST /api/jobs` | a `Recipe` (`{"v":1,"sources":[hash, …],"ops":[…],"output":{…}}`; overlay assets after the main source, referenced by index from `overlay` ops) → `202 Job`; the result is served from cache when the same recipe was rendered before; 400 for a source that is not uploaded, 409 for one not probed yet, 400 naming the compose profile for a `matte` op while `features.matte` is false |
| `GET /api/matte` | the AI matte sidecar as the app last saw it: `{"enabled", "device"` (the **effective** device passes run on — the preference, else the sidecar's default: `cuda` · `cpu` · `unavailable`)`, "devices": ["cuda", "cpu"]` (Phase 5c: every device the sidecar offers; the "Run on" select shows only with several)`, "reason", "gpu": {"name", "totalGiB", "freeGiB"}` or `null`, `"defaultModel"` (the sidecar's default for the effective device)`, "defaultModels": {"cuda": "birefnet-lite", "cpu": "isnet-anime"}, "models": {id: {"label", "kind"` (Phase 5c: `segmenter` · `tracker`, absent = segmenter)`, "state"` (`ready` · `loading` · `downloading` · `missing` · `unavailable`)`, "percent", "msPerFrame", "reason", "licence", "sizes", "resident"` (a session is loaded right now — `ready` alone means downloaded and self-tested: the first Compute adds the model load, ~1 s for isnet, 2–20 s for General)`, "devices": {dev: {"state", "reason", "precision", "percent", "size", "msPerFrame", "resident"}}}}, "maxSeconds", "maxFrames"}` — the per-model top-level fields mirror the sidecar's default device, `models.<id>.devices` carries each offered device's own state, estimate and residency. `enabled` is `features.matte`; the UI polls it every 5 s while the AI mode is visible or a pass is pending |
| `PUT /api/matte/settings` | Phase 5c: `{"device": "cuda"` · `"cpu"` · `""}` → the device AI passes run on from now on, persisted under `/data/mattes/settings.json` (`""` = back to the sidecar's default); 200 with the `GET /api/matte` object reflecting it, 400 for a device the sidecar does not offer. The card's "Run on" select; the device is never part of a recipe |
| `POST /api/matte/unload` | Phase 5c: asks the sidecar to release every resident model now (`POST /v1/unload`) → 204; best-effort (a sidecar that is off or does not answer is not an error). The card sends it when the Background mode leaves AI or the card is disabled; the next pass loads what it needs again. While a matte pass is in flight (a render's, say) nothing is sent — and the sidecar skips a runtime busy with a request — so a running pass never reloads its model mid-way; the idle TTL releases it afterwards |
| `POST /api/matte/prompt` | Phase 5c (guided mode): `{"src": hash` (or `"sources"`)`, "ops": […], "output": {…}, "frame": N, "prompts": [{"frame": N, "points": [[x, y, label], …], "box": [x0, y0, x1, y1]}, …]}` — the matte op's own `prompts` array (a `{"obj": 1, "prompts": [...]}` object is accepted too) → `image/png`: the 8-bit gray mask the tracker (`sam2-tiny`, or any model listed with kind `tracker`) predicts for output frame N of the recipe's matte plan under the prompts of **that frame only** (coordinates in 0..1 of the source frame, label 1 = keep / 0 = remove; the frame is rendered at the tracking size, long side ≤ 1024 px, so the mask comes back at that size) — the live overlay the Select-subject panel draws after every click. 202 (the usual pending body) while the tracker is loading or downloading; 400 without a matte op, for a model that is not a tracker, without prompts on that frame or for prompts without a box, positive point or mask; 404 / 409 for the sources as for a still; memoised per (clip, frame, prompts) on scratch. The 5c follow-up's **mask prompt**: a prompt may be `{"frame": N, "maskFrom": "edge", "mask": true}` instead of (or with) its box / points — `maskFrom` is the recipe's word, `mask` the client flag of the same prompt (the server reads either, so the op's prompts array can be posted verbatim — the SPA sends both; a recipe carries `maskFrom` only; any other `maskFrom` is 400) — the frame's mask is the **Edge** model's per-frame matte of output frame N (the model the op's `edge` names, the device's default per-frame model when it is `""`; `"none"` is refused with 400 — no edge model to take it from), which the server takes from that model's memo for this clip and never from the request (the body carries no mask bytes, a client cannot supply one): 200 PNG when it is on disk — the tracker's mask of the frame conditioned on it, the frame's clicks refining it —, 202 `{"pending": "matte", "state": "idle", "pendingReason": "compute the General matte first …", …}` when it is not computed yet (`pendingReason` is the 202's own field — the status's `reason` is never overwritten) and the Edge pass's own running 202 while one is in flight for the clip; a mask prompt never starts a pass (Compute the edge model first — the usual per-frame pass) |
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
computed; refused with 400 while `features.matte` is false. From Phase 5c (2026-10-09):
`stabilise` — `""` = off, `"light"` = a centred 3-frame temporal median over the matte sequence
(the card's default; it removes every single-frame pop with no lag), `"strong"` = the median
then a decay-0.7 hold (keeps parts that drop out for a frame, a half-frame trail on fast
motion) — a derived sequence cached next to the mattes, so it needs no new pass; `keep` — up to
6 `RRGGBB` colours forced opaque (alpha = max(matte, colour match), one `colorkey`-shaped mask
per colour on the merged frame) with `keepSimilarity` 0.01–1, 0 = the default 0.08; and, for the
guided tracker only (`model` `"sam2-tiny"`): `prompts` — 1..32 prompted frames `{"frame": N`
(the **output** frame index on the plan's grid)`, "points": [[x, y, label], …]` (0..1 of the
**source** frame, label 1 = keep / 0 = remove)`, "box": [x0, y0, x1, y1]` (0..1 of the source
frame)`, "maskFrom": "edge"` (the 5c follow-up: the frame's mask prompt is the edge model's
per-frame matte of that frame, resolved by the server from its memo — no mask bytes in a recipe;
at most one per op, it counts as the positive prompt, so it needs no box or point and the
frame's points / box refine it; refused with `edge` `"none"`)`}` with at least one box, positive
point or mask across them, and `edge` — `""` = the device's
default per-frame model shapes the edge band, `"none"` = the tracker's mask alone, else a
segmenter model id; `resolved` then also carries `tracker` (the tracker weights) and `edge` /
`edgeWeights` / `edgeProc`. The device a pass ran on is **not** a recipe parameter (see
`PUT /api/matte/settings`)), `reverse`,
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
cases (`GET /api/matte` — with the 5c `devices` / `defaultModels` / `kind` fields when the
sidecar reports them —, GIF / WebP / APNG with a `matte` op, the same recipe twice → cached, a
still that answers 202 `idle` without `eager` and runs the pass with it, matte + autocrop, and
from Phase 5c `PUT /api/matte/settings` (an unoffered device → 400; `cpu` then `""` when the
sidecar offers both devices, restoring the original preference), a recipe with
`stabilise: "light"` (its `render.matte` check names it), a `keep` colour (the figure's body colour comes
back opaque whatever the model finds), the guided tracker with a box prompt around the figure's
known position on frame 0 (when `sam2-tiny` is offered and ready), `POST /api/matte/prompt` →
a PNG mask that is white inside the box and black at the corner, the 5c follow-up's mask prompt
(`matte-prompt-mask`: `POST /api/matte/prompt` with `{"frame": 0, "maskFrom": "edge"}` and `edge`
naming the pixel model → 202 `idle` before any pass on the clip, 200 PNG after the per-frame
pass; `matte-track-mask`: the guided GIF recipe with that prompt, submitted after the per-frame
jobs → done, report.ok, frame 0 opaque and transparent), and `POST /api/matte/unload` →
204; the "`matte` op refused with 400 while the feature is off" case runs against whichever
server has it off; the pixel assertions name the general `birefnet-lite` model when the sidecar
lists it ready — the `matte-gpu` profile does, the CPU `matte` profile ships `isnet-anime` only,
which does not detect the synthetic figure the script draws, so those assertions are then skipped
with a note and the pipeline cases still run); it does
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
then one about a rendered file, and the last three about the AI matte sidecar.

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
the volume from then on) and the offered weights (~560 MB with the guided model) are down and
self-tested on each device — the start period is 300 s; `unhealthy` after that is a real fault
(`docker compose logs matte-gpu`).

**The preview says "AI matte not computed — Compute" and nothing happens when I pick a model.**
By design since Phase 5c: selecting AI, a model, a device, Stabilise or a Keep colour never starts
a pass — press **Compute matte** (or Render, which always computes). The pill then shows the
pass ("AI matte 24/45 · GPU") or "loading model…" first: the sidecar keeps **no model resident**
between passes (self-test at start, then released; `MATTE_MODEL_TTL` 300 s; everything is
released when the card leaves the AI mode), so the first pass after a pause pays the load —
~1 s for Anime, 2–20 s for General, under a second for Guided when warm. `MATTE_PRELOAD=<ids>`
on the sidecar keeps named models resident on a dedicated box. The **Run on** select is missing
when `GET /api/matte` lists one device only (the CPU `matte` profile, or a `matte-gpu` whose
CUDA EP is unavailable); a device the sidecar does not offer is refused by
`PUT /api/matte/settings` with 400 and the preference stays. A matte computed on the CPU is
reused by a GPU pass of the same model and graph (and the other way round): the cache key is
the weights, size and precision, never the device.

**`two matte sidecars answer at http://matte:9402`** in the app log: both profiles are up and DNS
alternates between them, so mattes would come from whichever answers. Stop one
(`docker compose --profile matte stop matte` keeps `matte-gpu`) and keep a single profile in `.env`.
