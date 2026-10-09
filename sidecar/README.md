# ez-local-gif matte sidecar

The AI background-removal service behind the **AI** mode of the Background
card. It runs two kinds of model behind a tiny HTTP API: ONNX **segmenters**
(isnet-anime, BiRefNet-lite) on onnxruntime — one 8-bit gray PNG matte per
frame — and the SAM 2.1 **tracker** (sam2-tiny, the "Guided (click to select)"
mode) on PyTorch — the whole clip plus a box / clicks / a mask in, one binary
mask per frame out. The app streams the frames (already scaled by ffmpeg), memoises the
mattes on `/data` and merges them in its own filtergraph — the sidecar never
sees the recipe, the output format or the user. Design:
`docs/background-removal-proposal.md` §7 (API, lifecycle, models) and
`docs/DESIGN.md` §4.3.

**Off by default.** Without a running sidecar (`EZLG_MATTE_URL` empty, or set but
nothing answering) the app behaves exactly as before: `features.matte` is false
and the AI mode is greyed out with the reason.

## Phase 5c in one paragraph

**One process serves every device it can** — `devices: ["cuda","cpu"]` on the
cuda image when the CUDA execution provider works, `["cpu"]` otherwise — and
every request picks one with `device=` (default: `MATTE_DEVICE`); the app's
"Run on: GPU / CPU" choice is a per-request parameter, never part of a recipe
or a memo key. A model is a separate **runtime per device** with its own graph
choice (isnet: fp16 1024² on cuda, fp32 512² on cpu; lite: fp32 1024² on both,
gated on the cpu by 14 GiB of available RAM — listed `unavailable` with the
figure below that, never silently absent; sam2-tiny: bf16 on cuda, fp32 on
cpu), its own sessions and its own measured
`msPerFrame`. **Nothing stays resident:** at start every runtime is downloaded,
derived and self-tested (so a measured ms/frame exists before the first pass)
and then **released**; a request loads it again (isnet ~0.2 s, lite ~2–20 s,
sam2-tiny ~0.3–0.7 s) and the janitor releases it after `MATTE_MODEL_TTL`
(300 s) idle; the app calls `POST /v1/unload` when the card leaves the AI mode.
The default model is **per device**: General (BiRefNet-lite) on the GPU, Anime
(isnet) on the CPU.

## Running it

Pick where it runs with a compose profile (see `compose.yaml` and
`docs/USAGE.md`):

```
docker compose --profile matte up -d        # CPU: isnet-anime 512² (~110 ms/frame on 8 cores) + sam2-tiny (~1.1 s/frame)
docker compose --profile matte-gpu up -d    # CUDA: + BiRefNet-lite (~140 ms/frame), isnet fp16 (~40 ms), sam2-tiny (~50 ms)
                                            #       — and the same process also answers device=cpu
```

The GPU variant needs an NVIDIA driver >= 580 (CUDA 13 wheels) and
nvidia-container-toolkit on a bare-metal host; nothing extra under Docker
Desktop's WSL2 backend. Both variants answer at `http://matte:9402` on the
compose network only — **never publish the port**, there is no authentication.

The first start downloads the weights (176 MB + 224 MB + 156 MB) into the
`ezlg-models` volume and derives the fp16 / 512² graphs next to them (~2 s
each); every runtime is self-tested before it is offered — on the cuda image
the first start also creates the CUDA context (+ PTX JIT, cached in
`/models/.nv` from then on), so `docker compose ps` shows `health: starting`
for a few minutes (the start period is 300 s), not `unhealthy`. Pre-seed the
volume on an air-gapped host with `docker compose run --rm matte download`
(then copy the volume) — with no ids it fetches **every** model of
`models.json`, whatever the service's `MATTE_MODELS` says, so the one volume
serves the `matte` and the `matte-gpu` profile alike; name ids to narrow it
(`… download isnet-anime`) — or point `MATTE_MODELS_BASE_URL` at an internal
mirror of the files.

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
| `MATTE_DEVICE` | `auto` | The **default** device (a request without `device=`). `auto` picks CUDA when the EP works, else CPU with a logged warning; with a working EP the process offers **both** (`devices: ["cuda","cpu"]`). `cuda` without a working EP **stays up** and reports `device: "unavailable"` with the reason (`/v1/ping` answers 503, the container shows unhealthy, the app shows AI off with that text) and offers nothing. `cpu` offers the CPU only (the cpu image). |
| `MATTE_MODELS` | the ids `models.json` offers on the process's devices: `isnet-anime,birefnet-lite,sam2-tiny` on every device (`birefnet-lite` on a cpu runtime is gated by 14 GiB of available RAM) | comma list of model ids to offer; each is offered on the devices its `offer` list names — a named model whose list has none of the process's devices is offered on every device |
| `MATTE_DEFAULT_MODEL` | `""` | the default model on **every** device; `""` = the `defaultFor` hints of `models.json` (birefnet-lite on cuda, isnet-anime on cpu). `MATTE_DEFAULT_MODEL_CUDA` / `MATTE_DEFAULT_MODEL_CPU` override one device. A name not offered on a device is logged and the hint used |
| `MATTE_PRELOAD` | `""` | models that **stay resident** after the start self-test (comma list). `""` = every runtime is released right after its self-test and loads again on first use |
| `MATTE_SELFTEST` | `1` | `0` skips the start self-tests: runtimes load on first use and `msPerFrame` is unknown until then (a laptop that should not spend a minute of CPU at start) |
| `MATTE_MODEL_TTL` | `300` | seconds idle before a runtime's sessions are released (`0` = never; `120` gives a shared desktop its GPU/RAM back sooner). Re-creating takes ~0.2 s for isnet, 2–20 s for lite, ~0.3–0.7 s for sam2-tiny |
| `MATTE_GPU_MEM_LIMIT_GIB` | `6` | ORT CUDA arena cap for models that need one (BiRefNet-lite: 6.3 GB footprint). **Lower values fail** — a knob for larger cards, not a remedy for smaller ones; isnet runs uncapped (~0.8 GB), the tracker is torch's allocator (~1.2 GB) |
| `MATTE_THREADS` | `0` | intra-op threads for the CPU EP and torch on the CPU; `0` = physical cores (capped by the container's CPU quota). Lower it on a box whose renders share the cores |
| `MATTE_MAX_TRACK_FRAMES` | `3000` | frames per `POST /v1/track` (the body is capped at 1 GiB as well: ~600 frames at 1024×576) |
| `MATTE_TRACK_OFFLOAD_FRAMES` | `300` | a track of more frames keeps SAM 2's per-frame memory bank (~1–1.5 MB per frame at 1024²) in host RAM instead of VRAM (`offload_state_to_cpu`: ~1.2× slower, no OOM on long clips on an 8 GB card); `0` = always offload |
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
       "devices":["cuda","cpu"],"defaultDevice":"cuda",
       "defaultModels":{"cuda":"birefnet-lite","cpu":"isnet-anime"},"defaultModel":"birefnet-lite",
       "models":{"isnet-anime":{"state":"ready"|"loading"|"downloading"|"missing"|"unavailable","reason":"",
                 "percent":0,"weights":"<sha256 of the pinned source file, in every state>",
                 "graphDigest":"<sha256 of the graph actually loaded>","precision":"fp16","sizes":[1024,512],
                 "defaultSize":1024,"msPerFrame":{"1024":37.4},"licence":"Apache-2.0","lastError":"",
                 "label":"Anime (fast)","resident":false,"kind":"segmenter",
                 "devices":{"cuda":{"state":"ready","reason":"","percent":0,"precision":"fp16","size":1024,
                                    "sizes":[1024,512],"msPerFrame":{"1024":37.4},"resident":false,
                                    "graphDigest":"…","lastError":""},
                            "cpu": {"state":"ready",…,"precision":"fp32","size":512,"msPerFrame":{"512":112.0},…}}},
                 "sam2-tiny":{…,"kind":"tracker","precision":"bf16","sizes":[1024],"defaultSize":1024,
                              "msPerFrame":{"1024":47.9},"prompts":["box","points","mask"],
                              "devices":{"cuda":{…},"cpu":{…,"precision":"fp32","msPerFrame":{"1024":1060}}}}},
       "busy":0}
     The top-level per-model fields mirror the DEFAULT device's runtime (pre-5c clients); `devices` carries every
     runtime; `prompts` lists the prompt kinds a model takes (a tracker: box, points, mask — Phase 5d; a segmenter:
     []). 503 same body + "error": <reason> when device is "unavailable" (then devices [] and defaultModels {}).
POST /v1/matte?model=isnet-anime&device=cuda&size=1024&frames=8     body: frames × size × size × 3 bytes rgb24, Content-Length exact
  200 application/x-ezlg-mattes: frames × [uint32 BE length][8-bit gray PNG size×size], then uint32 0
  400 bad params / Content-Length mismatch / a tracker model ("use /v1/track") · 404 unknown model or size,
      {"error":"device tpu not offered","devices":[…]}, "model 'birefnet-lite' is not offered on device cpu" ·
      413 frames > MATTE_MAX_BATCH or body > 128 MiB
  503 {"error":"model loading","retryAfterMs":N,"state":…,"percent":…}   downloading / loading (a request on a
      not-yet-loaded runtime starts its load; a released runtime whose last session create took over 3 s — lite —
      re-creates in the background and answers this, a quick one re-creates inline)
  503 {"error":"model unavailable: <reason>"}  503 {"error":"out of memory","retryAfterMs":3000}  (another runtime on
      that device was released; retry)   507 {"error":"out of memory: …"} (nothing to release; the runtime is
      re-tested before it is offered again)   500 {"error": …} anything else
POST /v1/track?model=sam2-tiny&device=cuda&w=720&h=720&frames=45    body: frames × w × h × 3 bytes rgb24 (the clip at the
                                                                     tracking size: long side <= 1024, even dims)
  header X-Matte-Prompts: {"obj":1,"prompts":[{"frame":0,"points":[[x,y,label],…],"box":[x0,y0,x1,y1]|null}, …]}
      coordinates in 0..1 of the frame (converted to pixels of w×h), label 1 = keep / 0 = remove; 1..32 prompts, frame in
      0..frames-1, one box per frame (entries for the same frame merge their points), box x0 < x1 and y0 < y1, and at
      least one box, positive point or mask (negative clicks alone select nothing → 400). Every prompted frame conditions
      the tracker (a correction on a later frame sticks); propagation runs forward from the earliest prompted frame to
      the end and backward from it to frame 0. One object.
  a MASK prompt (Phase 5d): the header adds a top-level "mask":{"frame":i} (the frame stays listed under "prompts", with
      its points / box or {"frame":i,"points":[],"box":null} — the entry is optional) and the body carries, AFTER the
      frames, ONE record [uint32 BE length][8-bit gray PNG at w×h, >= 128 = subject]: Content-Length = frames×w×h×3 + 4 +
      the PNG's length. The mask conditions frame i (SAM 2's add_new_mask: with the sam2.1 config's
      use_mask_input_as_output_without_sam the frame's output IS the mask, resized to 1024² and thresholded at 0.5, the
      prototype's best prompt). The frame's box / points, if any, refine the mask BEFORE it conditions the frame, and
      geometrically — SAM 2 cannot refine a mask prompt on its own frame (measured, see `Sam2Tracker._refine`: its decoder
      ignores a mask fed as the dense prompt and its memory correction is dominated by the mask's memory): the box keeps
      the mask inside it (mask ∩ box — a loose box is a no-op, never the background), a + click adds and a − click removes
      the SMALLEST of SAM's three candidate regions at the click (the finest part it sees there: a plush, a hair part;
      the IoU-best candidate would be the whole character and a − click would empty the mask; a click that NO candidate
      contains — the background — is a no-op for either label, never a fallback to the IoU-best region), in the
      prompt's order;
      the overlay and the track compute it with the same function. One mask per request; its frame needs no box /
      points. 400 for a mask prompt without
      its record, a record without a mask prompt, a record whose length field does not match, a non-PNG, a PNG that is
      not w×h, or a record over the cap for a w×h gray PNG ((w+1)×h + 1 % + 1 KiB); any PNG colour type is taken as its
      luma (Pillow).
  200 application/x-ezlg-mattes: frames × [uint32 BE length][8-bit gray PNG w×h, binary 0/255 from logit > 0], then uint32 0
  400 bad params / prompts / a segmenter model ("use /v1/matte") · 413 frames > MATTE_MAX_TRACK_FRAMES or body > 1 GiB ·
      the 404 / 503 / 507 of /v1/matte. One track at a time per device (the runtime lock); /v1/ping stays responsive.
POST /v1/track/frame?model=sam2-tiny&device=cuda&w=720&h=720      body: ONE frame (+ the mask record after it for a mask
      prompt); header X-Matte-Prompts: that frame's prompts (every entry — and the mask — names the same frame; its index
      is the clip slot and is passed through, not range-checked)
  200 image/png — that frame's mask, through the same conditioning path the track uses on a prompted frame (so the
      overlay shows exactly what the track holds there); ~45 ms on an RTX 5080, ~600 ms on 8 CPU cores at 720²
POST /v1/warm?model=birefnet-lite&device=cuda   download / derive / self-test / create the runtime now → 200 {"model","device","state","resident"}
POST /v1/unload[?model=…][&device=…]           release sessions now → 200 {"unloaded":[ids…],"sessions":[{"model","device"},…],"busy":[…]}
                                               (no filter = everything; model = its every device; device = every model on it; a
                                               runtime busy with a request is skipped after 0.5 s and listed under "busy" — the
                                               TTL releases it later; clearing it mid-pass would only make that pass reload)
      /v1/track/frame while a track holds the runtime: 503 {"error":"model busy","retryAfterMs":1000} after 1 s (the overlay
      re-asks) instead of queueing behind the whole track
```

`msPerFrame` of a segmenter is measured over the whole per-frame path
(pre-processing, inference, sigmoid, PNG) from an 8-frame warm-up at the
self-test and refined by a running mean over real batches; the tracker's is
the whole pass per frame (frame prep, conditioning, propagation) over a
synthetic 8-frame clip, then over real tracks. One inference runs at a time per
runtime (a lock around `sess.run` / the predictor, which release the GIL), so
`/v1/ping` answers in a few milliseconds during a batch — pytest asserts
< 50 ms — and a cpu and a cuda request run side by side. One log line per
request:
`matte model=isnet-anime device=cuda frames=8 size=1024 ms_per_frame=33.1 total_s=0.27 vram_gb=2.9`,
`track model=sam2-tiny device=cuda frames=45 size=720x720 prompts=1 ms_per_frame=60.6 total_s=2.73 vram_gb=3.8`
(`vram_gb` = the card's used memory from the last nvidia-smi sample, all processes).

Pre/post-processing is the benchmark's: isnet-anime `/255`, minus mean
(0.485, 0.456, 0.406), std 1 (rembg `dis_anime.py` — **not** the ImageNet
std), sigmoid in the graph; BiRefNet-lite ImageNet mean/std, sigmoid on the
logits; sam2-tiny the sam2 package's own loader maths (bicubic resize to 1024²,
ImageNet mean/std) applied straight to the rgb24 bytes — no JPEG round trip,
no frame tensor held for the whole clip (frames are converted as the predictor
asks for them). Never a per-frame min–max normalisation (flicker). A change
here bumps `PROCESSING_VERSION`, which the app folds into its memo keys.

## Models (`models.json`)

| id | label | kind | file | licence | devices | precision / size |
|---|---|---|---|---|---|---|
| `isnet-anime` | Anime (fast) | segmenter | rembg `isnet-anime.onnx`, 176 MB | Apache-2.0 | cuda, cpu (**cpu default**) | fp16 1024² on cuda, fp32 512² on cpu |
| `birefnet-lite` | General (precise) | segmenter | rembg `BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx`, 224 MB | MIT | cuda (**cuda default**); cpu only when named | fp32 1024² |
| `sam2-tiny` | Guided (click to select) | tracker | `sam2.1_hiera_tiny.pt`, 156 MB (dl.fbaipublicfiles.com) | Apache-2.0 | cuda, cpu | bf16 on cuda, fp32 on cpu; 1024² inside |

Every entry pins a list of URLs (the pinned release asset first, then a mirror
holding the byte-identical file when one exists), the byte count and the
sha256; `tools/pin-model.py <path-or-url>` prints those facts for a new entry.
Downloads resume `.part` files with `Range`, back off exponentially over the
URL list and are renamed into place only after the sha256 matched; a model
that still failed is retried every 10 minutes and shows `missing` with
`lastError`. Derived graphs (`<name>.fp16.onnx`, `<name>.512.onnx`,
`<name>.fp16.512.onnx`) are made at first load with `tools/to_fp16.py` and
`tools/rescale_graph.py` and stamped (`*.stamp.json`: source sha256,
processing version, their own sha256); the fp32 source graph is the fallback
when a derivation fails. `offer` lists the devices a model is offered on by
default, `defaultFor` the device(s) it is the default model of.

BiRefNet-lite's VRAM pre-check marks it `unavailable` (with the figure in
`reason`) when the card has under 7 GiB free or the arena cap is below 6 GiB,
and on cards under 10 GiB only one CUDA runtime is resident at a time. On CPU
it is gated by 14 GiB of available RAM and is ~4–10 s/frame — General is the
GPU default because it is stable on video (IoU 0.96–0.99 every frame), Anime
the CPU default because it is fast there and General is not.

**sam2-tiny** is the SAM 2.1 hiera-tiny video predictor from the sam2 package
pinned at commit `2b90b9f5…` (`requirements-sam2.txt`, a hashed GitHub archive
built with `SAM2_BUILD_CUDA=0`: the CUDA connected-components extension is not
built, the hole filling (`fill_hole_area=8`) runs through scipy instead, 8-
connectivity like the kernel). Built with
`add_all_frames_to_correct_as_cond=true` so a correction on a later frame
sticks; bf16 autocast + TF32 on CUDA, fp32 on CPU. Measured on an RTX 5080
with the 45-frame corpus clip (720²) and a box prompt: 2.8 s for the clip
(~60 ms/frame including the transfer), IoU 0.994 against the ground truth on
every frame, 1.2 GB of VRAM; ~1.1 s/frame on 8 CPU cores at the same quality.
One click is unreliable on illustrated characters (it selects a part), so the
app leads with a box and uses − clicks for removal; edges are coarser than
BiRefNet's (256² decoder logits), which is why the app gates the per-frame
model's matte with this mask instead of using it alone. The **mask prompt**
(Phase 5d: the per-frame model's matte of one good frame, sent as one PNG
record after the frames) is the most reliable start — measured on the same
clip with the frame-0 ground truth as the mask: IoU 0.996 (min 0.995, the
prompted frame 0.9986) against 0.991 from a tight box and 0.000 from a loose
box covering most of the frame (SAM 2 selects the background inside such a
box); a mask on frame 20 propagates both ways at 0.996; 0.997 on the CPU.

## Developing and testing

```
python -m venv .venv && .venv/bin/pip install -r requirements-cpu.txt pytest      # Windows: .venv\Scripts\pip
SAM2_BUILD_CUDA=0 .venv/bin/pip install --require-hashes --no-deps --no-build-isolation -r requirements-sam2.txt
python -I -m pytest tests -q               # ~50 s; a synthetic 8×8 ONNX graph, a fake tracker, a fake CUDA EP — no download, no GPU
EZLG_SAM2_CHECKPOINT=/models/sam2.1_hiera_tiny.pt python -I -m pytest tests -q   # + the real SAM 2 smoke on a synthetic clip
python -I matte.py selftest                # what the Docker build runs (+ the real tracker when its checkpoint is in MATTE_MODELS_DIR)
MATTE_DEVICE=auto MATTE_MODELS_DIR=./models MATTE_BIND=127.0.0.1 python -I matte.py serve
```

Everything runs under `python -I` (isolated mode; `/app` is never on
`sys.path`, the scripts load `png.py` and `tools/*.py` by path). The pins are
regenerated with `uv pip compile` (see the `.in` files) so the hashes cover the
linux wheels regardless of the host that runs the command; the cuda lock
resolves torch's exact NVIDIA pins (cuDNN 9.24.0.43, cuBLAS 13.1.1.3, CUDA
runtime 13.0.96 …), which onnxruntime-gpu's `~=` extras accept, so one set of
wheels serves both runtimes. `import torch` happens before
`import onnxruntime` for the same reason. The tests fake the tracker through
`Config.tracker_factory` and a working CUDA EP through a wrapped CPU session;
the real SAM 2 path is covered by `test_tracker_plumbing_with_random_weights`
(when torch + sam2 import) and the checkpoint smoke.

Images: `docker build --target cpu -t duckautomata/ezlg-matte:local sidecar/`
and `--target cuda` (the CUDA target downloads ~4 GB of wheels; the build runs
the self-test on the CPU EP and the tracker plumbing with random weights, then
removes triton and cuSOLVER — NCCL, cuSPARSELt, cuFile and NVSHMEM have to stay,
libtorch links them at load time). Installed sizes (`du` of the root
filesystem): cpu 1.2 GB (was 0.5 GB before Phase 5c), cuda 4.9 GB (was
4.0 GB, of which 1.5 GB was a stray `/tmp` left behind by that build — its
`/usr` was 2.6 GB); `docker images` on Docker Desktop shows 2.4 GB and
10.7 GB because its containerd store counts the compressed blobs as well
(the old cuda image showed 7.5 GB the same way). CI builds both next to the app image
(`.github/workflows/docker-image.yml`); the compose file pins
`EZLG_MATTE_TAG` to the release it ships with.

Licences of everything inside the images: `NOTICE` and `LICENSES/`.
