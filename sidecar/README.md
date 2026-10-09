# ez-local-gif matte sidecar

The AI background-removal service behind the **AI** mode of the Background
card. It runs ONNX segmentation models (isnet-anime, BiRefNet-lite) on
onnxruntime behind a tiny HTTP API and answers one 8-bit gray PNG matte per
frame. The app streams the frames (already stretched to the model's input
square by ffmpeg), memoises the mattes on `/data` and merges them in its own
filtergraph — the sidecar never sees the recipe, the output format or the user.
Design: `docs/background-removal-proposal.md` §7 (API, lifecycle, models).

**Off by default.** Without a running sidecar (`EZLG_MATTE_URL` empty, or set but
nothing answering) the app behaves exactly as before: `features.matte` is false
and the AI mode is greyed out with the reason.

## Running it

Pick where it runs with a compose profile (see `compose.yaml` and
`docs/USAGE.md`):

```
docker compose --profile matte up -d        # CPU: isnet-anime at 512², ~130 ms/frame on an 8-core desktop
docker compose --profile matte-gpu up -d    # CUDA: isnet-anime fp16 1024² (~20–30 ms/frame) + BiRefNet-lite
```

The GPU variant needs an NVIDIA driver >= 580 (CUDA 13 wheels) and
nvidia-container-toolkit on a bare-metal host; nothing extra under Docker
Desktop's WSL2 backend. Both variants answer at `http://matte:9402` on the
compose network only — **never publish the port**, there is no authentication.

The first start downloads the weights (176 MB + 224 MB) into the `ezlg-models`
volume and derives the fp16 / 512² graphs next to them (~2 s each); the models
are self-tested before they are offered — on the cuda image the first start
also creates the CUDA session (context + PTX JIT, cached in `/models/.nv` from
then on), so `docker compose ps` shows `health: starting` for a few minutes
(the start period is 300 s), not `unhealthy`. Pre-seed the volume on an
air-gapped host with `docker compose run --rm matte download` (then copy the
volume) — with no ids it fetches **every** model of `models.json`, whatever the
service's `MATTE_MODELS` says, so the one volume serves the `matte` and the
`matte-gpu` profile alike; name ids to narrow it (`… download isnet-anime`) —
or point `MATTE_MODELS_BASE_URL` at an internal mirror of the files.

If a fresh volume was created by an older image or by hand and the download
fails with EACCES, repair the ownership once — `chown(2)` needs `CAP_CHOWN`
even as root, and the service drops every capability, so the one-off run must
add it back:

```
docker compose run --rm --user root --cap-add CHOWN --entrypoint chown matte -R 1000:1000 /models
# or, without touching the service's config (any image with chown; <project> = the compose project name, usually the folder):
docker run --rm -v <project>_ezlg-models:/models alpine chown -R 1000:1000 /models
```

## Environment

| variable | default | meaning |
|---|---|---|
| `MATTE_DEVICE` | `auto` | `auto` picks CUDA when the EP works, else CPU with a logged warning. `cuda` without a working EP **stays up** and reports `device: "unavailable"` with the reason (`/v1/ping` answers 503, the container shows unhealthy, the app shows AI off with that text). `cpu` forces the CPU EP. The images set `cpu` / `cuda`. |
| `MATTE_MODELS` | ids `models.json` offers on the device (cpu image: `isnet-anime`; cuda image: `isnet-anime,birefnet-lite`) | comma list of model ids to offer; the UI's Model select shows exactly these |
| `MATTE_DEFAULT_MODEL` | `isnet-anime` | reported as `defaultModel`; used when a request names no model |
| `MATTE_PRELOAD` | `MATTE_MODELS` | downloaded + derived + self-tested at start (`""` = nothing until requested) |
| `MATTE_MODEL_TTL` | `600` | seconds idle before a model's sessions are released (`0` = never; `120` gives a shared desktop its GPU/RAM back sooner). Re-creating takes ~0.3 s for isnet, 2–20 s for lite |
| `MATTE_GPU_MEM_LIMIT_GIB` | `6` | ORT CUDA arena cap for models that need one (BiRefNet-lite: 6.3 GB footprint). **Lower values fail** — a knob for larger cards, not a remedy for smaller ones; isnet runs uncapped (~0.8 GB) |
| `MATTE_THREADS` | `0` | intra-op threads for the CPU EP; `0` = physical cores (capped by the container's CPU quota). Lower it on a box whose renders share the cores |
| `MATTE_MODELS_BASE_URL` | | download `<base>/<file name>` instead of the pinned URL list |
| `MATTE_PORT` / `MATTE_BIND` | `9402` / `0.0.0.0` | listen address (inside the container) |
| `MATTE_MAX_BATCH` | `32` | frames per `POST /v1/matte` (the app sends 8) |
| `MATTE_MODELS_DIR` | `/models` | the weights volume |
| `MATTE_VERSION` | image tag (build arg `VERSION`) | reported as `version` |

## API (HTTP/1.1, compose network only)

```
GET  /v1/ping
  200 {"protocol":1,"version":"<tag>","instance":"<random per start>","processingVersion":"1",
       "device":"cuda"|"cpu"|"unavailable","reason":"","gpu":{"name","totalGiB","freeGiB"}|null,
       "defaultModel":"isnet-anime",
       "models":{"isnet-anime":{"state":"ready"|"loading"|"downloading"|"missing"|"unavailable","reason":"",
                 "percent":0,"weights":"<sha256 of the pinned source file, in every state>",
                 "graphDigest":"<sha256 of the graph actually loaded>","precision":"fp16","sizes":[1024,512],
                 "defaultSize":1024,"msPerFrame":{"1024":32.5},"licence":"Apache-2.0","lastError":"",
                 "label":"Anime (fast)","resident":true}, …},
       "busy":0}
  503 same body + "error": <reason>   when device is "unavailable"
POST /v1/matte?model=isnet-anime&size=1024&frames=8     body: frames × size × size × 3 bytes rgb24, Content-Length exact
  200 application/x-ezlg-mattes: frames × [uint32 BE length][8-bit gray PNG size×size], then uint32 0
  400 bad params / Content-Length mismatch · 404 unknown model or size · 413 frames > MATTE_MAX_BATCH or body > 128 MiB
  503 {"error":"model loading","retryAfterMs":N,"state":…,"percent":…}   downloading / loading (a request on a
      not-yet-loaded model starts its load)
  503 {"error":"model unavailable: <reason>"}  503 {"error":"out of memory","retryAfterMs":3000}  (another model was
      released; retry)   507 {"error":"out of memory: …"} (nothing to release; the model is re-tested before it is
      offered again)   500 {"error": …} anything else
POST /v1/warm?model=birefnet-lite      download / derive / self-test / create the session now → 200 {"model","state","resident"}
POST /v1/unload[?model=…]              release sessions now → 200 {"unloaded":[…]}
```

`msPerFrame` is measured over the whole per-frame path (pre-processing,
inference, sigmoid, PNG) from an 8-frame warm-up at load and refined by a
running mean over real batches. One inference runs at a time per model (a lock
around `sess.run`, which releases the GIL), so `/v1/ping` answers in a few
milliseconds during a batch — pytest asserts < 50 ms. One log line per batch:
`matte model=isnet-anime device=cuda frames=8 size=1024 ms_per_frame=33.1 total_s=0.27 vram_gb=2.9`
(`vram_gb` = the card's used memory from the last nvidia-smi sample, all processes).

Pre/post-processing is the benchmark's: isnet-anime `/255`, minus mean
(0.485, 0.456, 0.406), std 1 (rembg `dis_anime.py` — **not** the ImageNet
std), sigmoid in the graph; BiRefNet-lite ImageNet mean/std, sigmoid on the
logits. Never a per-frame min–max normalisation (flicker). A change here bumps
`PROCESSING_VERSION`, which the app folds into its memo keys.

## Models (`models.json`)

| id | label | file | licence | sizes | precision |
|---|---|---|---|---|---|
| `isnet-anime` | Anime (fast) | rembg `isnet-anime.onnx`, 176 MB | Apache-2.0 | 1024 (cuda default), 512 (cpu default) | fp16 on cuda, fp32 on cpu |
| `birefnet-lite` | General (precise) | rembg `BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx`, 224 MB | MIT | 1024 | fp32 |

Every entry pins a list of URLs (the rembg GitHub release `v0.0.0` asset first,
then a mirror holding the byte-identical file when one exists), the byte count
and the sha256; `tools/pin-model.py <path-or-url>` prints those facts for a new
entry. Downloads resume `.part` files with `Range`, back off exponentially over
the URL list and are renamed into place only after the sha256 matched; a model
that still failed is retried every 10 minutes and shows `missing` with
`lastError`. Derived graphs (`<name>.fp16.onnx`, `<name>.512.onnx`,
`<name>.fp16.512.onnx`) are made at first load with `tools/to_fp16.py` and
`tools/rescale_graph.py` and stamped (`*.stamp.json`: source sha256,
processing version, their own sha256); the fp32 source graph is the fallback
when a derivation fails.

BiRefNet-lite is offered on CUDA; its VRAM pre-check marks it `unavailable`
(with the figure in `reason`) when the card has under 7 GiB free or the arena
cap is below 6 GiB, and on cards under 10 GiB only one model stays resident at a
time. On CPU it is gated by 14 GiB of available RAM and is ~4–10 s/frame.

## Developing and testing

```
python -m venv .venv && .venv/bin/pip install -r requirements-cpu.txt pytest      # Windows: .venv\Scripts\pip
python -I -m pytest tests -q               # ~20 s; a synthetic 8×8 ONNX graph, no download, no GPU
python -I matte.py selftest                # what the Docker build runs
MATTE_DEVICE=cpu MATTE_MODELS_DIR=./models MATTE_BIND=127.0.0.1 python -I matte.py serve
```

Everything runs under `python -I` (isolated mode; `/app` is never on
`sys.path`, the scripts load `png.py` and `tools/*.py` by path). The pins are
regenerated with `uv pip compile` (see the `.in` files) so the hashes cover the
linux wheels regardless of the host that runs the command.

Images: `docker build --target cpu -t duckautomata/ezlg-matte:local sidecar/`
and `--target cuda` (the CUDA target downloads ~2.3 GB of wheels; the build
runs the self-test on the CPU EP). CI builds both next to the app image
(`.github/workflows/docker-image.yml`); the compose file pins
`EZLG_MATTE_TAG` to the release it ships with.

Licences of everything inside the images: `NOTICE` and `LICENSES/`.
