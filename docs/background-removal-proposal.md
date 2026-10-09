# Background removal for ez-local-gif — design proposal (Phase 5)

Status: **built 2026-10-08 (Phase 5a + 5b); 5c built 2026-10-09 — not the fp16-lite item of §13 (dropped by the user) but on-demand mattes after the user's three complaints: no model resident between passes and a "Run on: GPU / CPU" choice (one sidecar process offers both devices; `EZLG_MATTE_DEVICE` / `PUT /api/matte/settings`, never in a recipe), a **Compute matte** button (previews answer `idle`, never start a pass), General (BiRefNet-lite) as the GPU default, a **Stabilise** temporal median (light / strong), **Keep colours**, and the **Guided (click to select)** mode — SAM 2.1 hiera-tiny prompted with boxes / clicks, gating the per-frame model's edges; measurements in [`docs/reviews/background-removal-stabilise-and-guided-2026-10-09.md`](reviews/background-removal-stabilise-and-guided-2026-10-09.md), what shipped in DESIGN.md §4.3 (AI matte row) and FEATURES.md Phase 5c.** The user's decisions that sharpened it at build time: the fixed "Precise" checkbox became a **Model** select fed by the sidecar's list (the operator sets `MATTE_MODELS` / `MATTE_DEFAULT_MODEL`, the user picks per recipe; labels "Anime (fast)" / "General (precise)"), the AI path is **off by default and configurable** (profiles `matte` / `matte-gpu`, `MATTE_DEVICE`, `EZLG_MATTE_URL`), only the RTX 5080 is measured, and 5c's fp16-lite benchmark is dropped. Where the build departed from a section below, DESIGN.md §4.3 (AI matte row) and §6 record what was built — notably the producer runs through `ffrun.RunFrom`, not `RunTo` (§4.1 step 6: exec's stdout copier is bounded by `WaitDelay` once ffmpeg exits, so a writer blocked in a slow POST lost the tail of the stream), the clip key's probe facts include `Sequence.Mixed` (§4.2), and the SPA's estimate line uses the server's own formula `frames × (msPerFrame + 2 ms)` (§9). Written 2026-10-08 from the background-removal research recorded in [`docs/reviews/background-removal-research-2026-10-08.md`](reviews/background-removal-research-2026-10-08.md) (a ground-truth corpus built from the user's own Resolve export; eight AI models and thirteen classical ffmpeg methods measured on the dev workstation; licences and runtime facts checked against primary sources) and from reading the code paths named below. Every code claim was checked against the named file; every number traces to the research notes or to the repo. *(verified)* = measured or read from source; *(to verify)* = pinned by a test the phase plan names before it is trusted. Voice and format follow `docs/DESIGN.md`; DESIGN.md itself is not changed until the user approves.

---

## 1. Summary

**What ships.** One new op kind, `matte` ("AI" in the Background card), resolved by `jobs` before compiling exactly like `autocrop` is today (`internal/jobs/autocrop.go`: memo key of canonical temporal ops, single-flight, detached leader, timeout; resolved for stills, Play proxies and renders alike). A small ONNX Runtime **sidecar** (`sidecar/`, ~250 lines of Python, no rembg, no torch) computes one 8-bit gray matte per master frame; the app streams the clip's frames to it in batches, files every matte in a **per-frame store** on `/data` (keyed by the pixels the model saw, so a trim, speed or fps change only re-mattes frames the store has never seen) and assembles the clip's sequence under `/data/mattes/<key>/`. The compiler reads that sequence as one more `Plan.ExtraInputs` entry and merges it in the keying group with `alphamerge` — the bare head shape the app already uses for AVIF alpha streams, or the `keyKeepingAlpha` intersection shape on frames that already carry alpha (`internal/graph/phase3.go:268`). Everything after the master — the GIF matte/threshold/`-gifflags -offsetting`/mixed-frame re-encode/`MergeGIFHolds`/lint ladder, WebP, APNG, AVIF — is byte-for-byte the same code, so Discord safety is unchanged by construction and `discordlint.RulesVersion` stays. Alongside, three classical fixes that need no sidecar (Phase 5a): the `chromakey` limited-range bug, saner key defaults, a second colour per key ("+ add colour"), and a `morph` op (fill pinholes / grow matte) that serves the AI matte and the colour keys alike.

**What it fixes, in the user's terms.**
- **Gradient, multi-colour, hair-coloured and GIF-dithered backgrounds — solved by AI with no knobs:** IoU ≥ 0.99 on every such corpus variant, nothing to tune (the colour keys top out at 0.54–0.75 IoU on 4-colour and hair-coloured backgrounds even with six picks, §2.2).
- **Stream-UI captures:** AI (isnet) leaves buttons touching the head at mid alpha, and they pop in a 1-bit GIF; **Precise** (BiRefNet-lite) rejects the UI entirely (GPU; unverified on the RTX 3070).
- **Busy, moving backgrounds:** IoU 0.978 and a **1 px colour fringe**, because AI mattes get no despill — visible in WebP/APNG on light backgrounds, hidden by the `#313338` matte in GIF.
- **Static full-range noise:** nothing works, classical or AI.
- **No sidecar** (a CPU-only box without the profile): only the classical fixes of 5a — the range bug, the defaults, a second colour, pinhole fill.
- **Sources below 256 px were not benchmarked** (every ground-truth variant is 720²); acceptance adds a real ≤ 256 px web GIF and the stream capture (§13).

**Models.** Default **isnet-anime** (Apache-2.0, 176 MB): IoU 0.993 over all 13 ground-truth variants at fp16 1024² on the RTX 5080 at **18 ms/frame** (0.76 GB VRAM, ~1 s session); on CPU the same weights on a 512² graph at **136 ms/frame** (Ryzen 7 7800X3D), IoU 0.991. One checkbox, **Precise** = **BiRefNet-lite** (MIT, 224 MB): IoU 0.997, every hair strand, the only model that rejects the stream-UI capture entirely; ~170 ms/frame on the 5080, 6.3 GB VRAM under a 6 GiB arena cap (3, 4 and 5 GiB caps **fail** — it needs ~7 GB free), 10–20 s session create. On CPU two measurements of the same lite graph on the 7800X3D disagree — **3.7 s/frame** (the research notes' timing table: 5 frames, 12.6 GB RSS) and **10 s/frame** (R2's microbench, 8 runs, random input) — so every estimate the app shows comes from the sidecar's own self-test, never from a constant.

**Wall clock, first pass over frames the store has never seen** (inference from `timing.json`; +1.8 ms/frame decode and ~2 ms hash, ~0.2 s PNG/HTTP per clip; every later still, slider and render — and every trim/speed/fps tweak whose frames were already matted — is a memo hit at ~20 ms on top of today's ~100 ms still, measured 96 vs 75 ms on the host for a late still of a 60 s clip):

| clip | RTX 5080 AI | RTX 5080 Precise | 7800X3D CPU AI (512²) | CPU Precise | RTX 3070 8 GB — scaled estimate, **not measured** |
|---|---|---|---|---|---|
| 45-frame emote | 45 × 18 ms ≈ **1 s** (+1 s cold) | 7.7 s (+10–20 s cold) | **6.1 s** | 2.8–7.5 min — offered when under the cap, estimate shown | AI ≈ 45–60 ms/frame (Ampere at ~2.5–3× the 5080) → **2–3 s**; Precise unverified under the cap |
| 300-frame clip | 5.4 s + decode ≈ **6 s** | 51 s | 41 s | 19–50 min → refused by the 600 s cap | 15–18 s |

**What it costs.** A second compose service pulled from Docker Hub (`duckautomata/ezlg-matte`: CPU image ≈ `python:3.12-slim` + 24 MB ORT; CUDA image +2.0 GB NVIDIA wheels + 327 MB ORT, ≈ 2.5 GB per pull), weights downloaded once into a named volume (sha256-pinned, 176 + 224 MB). While the sidecar runs it holds a CUDA context plus whichever models are loaded — isnet 0.76 GB, lite 6.3 GB — and unloads both after `MATTE_MODEL_TTL` idle (default 600 s; `0` = never, for a dedicated box; `120` for a desktop that also runs Resolve/OBS; `MATTE_PRELOAD=""` keeps the GPU free until the first request); reload is 1 s for isnet and 10–20 s for lite, started in the background the moment Precise is ticked (`/v1/warm`). In the repo: a new stdlib package `internal/matte`, ~15 touch points in graph/enc/jobs/store/server/SPA, memo/pipeline version bumps. Effort in the project's own cadence (Phases 3 and 4 were each built in one to two days of agent workflow): 5a ≈ 1 day, 5b ≈ 3–4 days, 5c ≈ 1 day; the ~20-minute sidecar image build and the user's upload/3070 checks are the wall-clock items.

**Phase plan.**
1. **5a — classical fixes** (shippable alone): chromakey `yuv=1`, defaults 0.1 / 0.08, "+ add colour", `morph` op, `PipelineVersion` + still/proxy/autocrop memo bumps.
2. **5b — sidecar + `matte` op** (feature off without the compose profile): sidecar images + CI matrix, `internal/matte`, compiler/enc/jobs/store/server/SPA integration, `/v1/warm`, fake-sidecar tests, integration test in the dev stack.
3. **5c — the fp16 lite variant for < 10 GiB cards, the 3070 verdict, docs** (DESIGN.md §4.3/§6/§10, USAGE.md).
4. **Acceptance** (user-run): one AI-matted emote and one sticker through the Discord test kit; a real ≤ 256 px gradient web GIF and the stream capture with AI and Precise, judged at 1×; the Precise self-test and `/v1/ping` timings on the RTX 3070.
5. **Not now:** border magic wand, plate-difference key (§10), temporal median (§5.7).

---

## 2. What the research showed

### 2.1 AI models (ONNX on onnxruntime-gpu 1.30.0 CUDA EP, RTX 5080; CPU = ORT CPU EP, Ryzen 7 7800X3D 8C/16T; corpus: 45 frames 720², 9 backgrounds + 4 GIF-dithered variants + one real stream-UI capture) *(verified)*

| model | licence | weights | IoU all | IoU vgrad / multi / skin / busy | flicker | GPU ms/frame | peak VRAM | CPU ms/frame | verdict |
|---|---|---|---|---|---|---|---|---|---|
| **isnet-anime** 1024² fp16 | Apache-2.0 | 176 MB | **0.993** | .995 / .993 / .991 / .978 | 0.0012 | **18** | 0.76 GB | 644 (fp32 1024²) · **136 at 512²** (IoU 0.991; 2.1 GB RSS) | ships (AI) — crisp; weakness: UI widgets touching the head come out at mid alpha |
| **BiRefNet-lite** 1024² | MIT | 224 MB | **0.997** | ~0.997 everywhere | 0.0002 | ~170 | 6.3 GB at a 6 GiB cap (3/4/5 GiB fail; 14.5 GB uncapped) | **3 700** (bench, 5 frames, 12.6 GB RSS) · 10 000 (R2 microbench) | ships (Precise) — session create 10–20 s; rejects the stream UI |
| BiRefNet-general / portrait | MIT | 973 MB | 0.996 | — | 0.0002 | 245–262 | 7.4 GB | 8 400 | same quality as lite at 4× the size |
| Lucida | MIT | 450 MB fp16 | 0.994 | — | 0.0008 | 167 | 4.3 GB | 8 300 | no measured edge over lite |
| BEN2 | MIT | 223 MB | 0.988 | — | 0.0008 | 124 | 5.4 GB | 5 700 | soft 2 px rim, UI ghosts |
| ToonOut | MIT | fp16 | 0.979 | .998 / .997 / .992 / **.812** | 0.002 | 174 | 5.4 GB | 9 700 | takes UI panels as subject; busy fails |
| u2netp 320² | Apache-2.0 | 4.7 MB | 0.962 | .984 / .974 / .936 / .888 | 0.0045 | 17 | 0.77 GB | 84 | mushy 3–5 px edges, dark plush half transparent |
| RobustVideoMatting | **GPL-3.0** | 15 MB | 0.58 | — | 0.01 | 16 | 0.38 GB | 32 | human-only, fails solid backgrounds; never ship |

Not benchmarked: RMBG-2.0 (CC-BY-NC), MatAnyone / VideoMaMa / SAM2Matting (non-commercial or multi-GB), SAM 2/3 (prompts). Per-frame models have ~zero flicker on static backgrounds (the metric only rises on the moving `testsrc2`). fp16 for isnet-anime is lossless in practice: MAE vs fp32 0.00005 (max 0.005), identical IoU (research notes, isnet-anime variants); the 512² graph costs IoU 0.993 → 0.991 and raises busy-background flicker 0.0023 → 0.0059, so 512² is the **CPU** configuration only. The two lite CPU figures were taken minutes apart on the same host with the same wheel (`timing.json` has no lite CPU row); the design treats them as a range and lets the self-test decide.

### 2.2 Classical / ffmpeg methods (oracle = best params per background; (d) = one fixed default) *(verified)*

| method | green | white | black | vgrad | multi | skin | anim | busy | gif-dithered vgrad / multi / skin |
|---|---|---|---|---|---|---|---|---|---|
| chromakey (d) app default .2/.05 | 1.00 | 0 | 0 | .41 | 0 | 0 | .43 | .16 | .40 / 0 / 0 |
| chromakey best | 1.00 | .44 | .44 | .996 | 0 | .57 | .92 | 0 | .996 / 0 / .57 |
| colorkey (d) app default .1 | 1.00 | .73 | .99 | .58 | .41 | .64 | .42 | .35 | .59 / .40 / .64 |
| colorkey best (.05–.15) | 1.00 | .89 | .997 | .95 | .26 | .72 | .75 | 0 | 1.0 / .27 / .72 |
| stacked colorkey (d) K=4 sim .08 | 1.00 | .79 | .99 | **1.00** | .54 | .74 | .85 | .58 | 1.0 / .57 / .63 |
| stacked colorkey best K=2..6 | 1.00 | .97 | .999 | 1.00 | .75 | .89 | 1.00 | .95 | 1.0 / .79 / .83 |
| plate-difference key, border-fitted plate | 1.00 | .98 | 1.00 | 1.00 | 1.00 | .998 | .99 | .63 | 1.0 / .97 / .96 |
| border magic wand (numpy; not expressible in ffmpeg) | .999 | .95 | .999 | .999 | .999 | .999 | .999 | .81 | .999 / .95 / .96 |

Real screen capture: **no classical method works** (UI panels stay, the wand stops at the first UI edge). Two picks solve a 2-colour ramp; 3–4-colour and hair-coloured gradients keep improving up to six picks and still leave 7–9 % MAE (the tolerance spheres also cover subject colours — a colour stack cannot know *where* a colour is). Post-processing: a 3×3 `close` never hurts and fixes line-art pinholes; `dilate` 1–2 px recovers eaten interiors at the cost of a fringe; `erode` never helps; feather is free for GIF (re-thresholded) and only matters for WebP/APNG; `tmedian` on alpha only helps the moving background (busy .0207 → .0177 — measured on the classical mattes, not the AI ones); source denoising before a key does not help dithered GIFs. Bugs in the current op: `chromakey`'s `color` goes through full-range macros while `format=yuva444p` gives limited-range chroma (0x00ff00 → U 54, V 34 in the frame, 44/21 from the macro, distance 0.047), so similarity below ~0.047 keys **nothing**; the default 0.2/0.05 keys the subject on every non-green background; colorkey at 0.05–0.08 beats chromakey everywhere but single-hue luma ramps.

### 2.3 Runtime facts *(verified on the workstation unless noted)*

onnxruntime-gpu 1.30.0 = CUDA 13 + cuDNN 9, host driver ≥ 580 (both targets qualify); `pip install "onnxruntime-gpu[cuda,cudnn]"` on plain `python:3.12-slim` runs the CUDA EP on the RTX 5080 under Docker Desktop `--gpus all` (no `nvidia/cuda` base; pip took 1,236 s → pre-built images). The CPU wheel is 24 MB. All rembg-zoo graphs are fixed batch 1 / fixed input square (throughput = pipelining, not batching). Session create: isnet ~1 s, BiRefNet-lite 10–20 s → a resident process with warm sessions. BiRefNet-lite under `gpu_mem_limit`: 6 GiB works (6.3 GB process footprint), 5/4/3 GiB fail with an ONNXRuntimeError (the fp32 graph needs a large contiguous chunk) — the research notes' VRAM-cap sweep. ffmpeg: `[0:v]format=rgba[c];[1:v]format=gray[a];[c][a]alphamerge` is pixel-exact with a gray PNG image2 sequence; alphamerge is 8-bit gray only; sizes must match (scale the matte in-graph); frames pair by **timestamp**, so the matte input must declare the master's rate; a count mismatch is **silent** (`eof_action=repeat` reuses the last matte) — lint the count. A single unlooped image2 frame at pts 0 is paired with every later main frame by framesync's defaults (`eof_action=repeat`, `repeatlast=1`); `-loop 1` makes the input infinite and framesync then decodes it up to the main's absolute timestamp — a still at t = 59.5 s of a 60 s clip at 50 fps with a looped 1024² matte PNG took **1.8 s** on the host, 96 ms unlooped, 75 ms with no matte at all. A yuv-decoded bt709 source keeps its matrix through `format=yuva444p` (chroma upsampling only): its exact green measures Y172 U42 V26, 0.057 from the 601-limited (54, 34) — §5.4. The `keyKeepingAlpha` shape with an external matte on the second leg is pixel-exact for intersecting existing alpha; reverse after the merge yields the mattes in reversed order exactly. 45 gray 720² mattes = 288 KB as PNG. Precedent: Immich's ML sidecar (own container per backend tag, `/ping`, lazy load, TTL unload, cache volume, `CUDA_CACHE_PATH` on the volume). `rembg s` rejected: no `/ping`, CORS `*`, per-frame min–max normalisation (flicker), non-commercial default model, 1.6 GB image.

### 2.4 Rejected alternatives (and why)

Python/ORT inside the app image (2.3 GB on every install, the GPU reservation on the app container); an exec'd CLI per job (10–20 s session create for lite, ~1 s per still for isnet against a ~100 ms budget); a shared scratch volume for pixels (couples uids/filesystems, forbids a remote sidecar; 8-frame raw batches over the compose network cost nothing); full-duplex streaming of a whole clip through one HTTP request (Python HTTP stacks interleave request-body reads with response writes unreliably — unverified, so not load-bearing); a **per-clip-only memo** (every trim-handle move, speed or fps change would re-matte the whole clip — ~1 s per tweak on the 5080, 6–41 s on CPU — although the pass already hashes every frame for its duplicate skip: the per-frame store costs nothing extra); `alphamerge=shortest=1` / `eof_action=endall` (silent truncation; an explicit count check is louder); matting on the untrimmed source grid (VFR animations have no constant grid; a 4K clip would be matted in full; the per-frame store gives the trim-independence that grid was meant to give); temporal models (GPL / non-commercial / human-only); a temporal median (its only measurement is on classical mattes, §5.7); TensorRT (minutes of engine build for a ≤ 20 ms workload); a PyTorch sidecar (≥ 3 GB of wheels); client-side WASM/WebGPU (DESIGN.md §12; a plain `http://LAN-IP` page is not a secure context); `backgroundkey` / `hsvkey` / stacked chromakey / `lumakey` / source denoise (research notes, classical methods: no gain or measured failure); k-means border sampling in the browser (the case it serves is the one AI makes obsolete, and batch mode has no preview to sample); post-processing in the sidecar (ffmpeg does close/grow/feather in-graph, so every tweak stays a ~100 ms still).

---

## 3. Op / recipe model

### 3.1 New op kinds (`internal/recipe/recipe.go`, stdlib-only, additive)

```go
// OpMatte (Phase 5) keys the main source with an AI matte: one alpha frame per
// master frame, computed by the matte sidecar on the frames the temporal
// stages yield (delay/unpremultiply/trim/speed/fps — the output frame grid),
// memoised by jobs per frame and per clip, and read back as an image2
// sequence input of the plan. Hoisted into the keying group like
// chromakey/colorkey/feather: full resolution, before any geometry, in stack
// order; on frames that already carry alpha the matte is intersected
// (multiply), never substituted. A plan with an unresolved matte input is
// unusable (enc returns nil).
const OpMatte = "matte"

// MatteParams. Model "" = MatteModelDefault. Size is the model input square
// in px; 0 = the server's default for its device (isnet-anime: 1024 on CUDA,
// 512 on CPU; birefnet-lite: 1024) — API-only, no UI control; only sizes the
// sidecar lists are valid. Resolved is filled by jobs (Submit, previews,
// render) from the sidecar's pinned facts and STRIPPED from client input
// exactly like AutoCropParams.Resolved — but, unlike the crop box, it stays
// in the recipe hash: a result rendered with other weights has another
// ResultKey (§11).
type MatteParams struct {
    Model    string         `json:"model,omitempty"`
    Size     int            `json:"size,omitempty"`
    Resolved *MatteResolved `json:"resolved,omitempty"`
}

// MatteResolved is the identity of the matte a render used.
type MatteResolved struct {
    Weights   string `json:"weights"`   // sha256 of the pinned source ONNX file (sidecar/models.json)
    Proc      string `json:"proc"`      // the sidecar's processingVersion (pre/post-processing + derivation recipe)
    Size      int    `json:"size"`      // the effective input square
    Precision string `json:"precision"` // fp16 / fp32 of the graph the sidecar runs for this device
}

const (
    MatteModelISNetAnime   = "isnet-anime"   // Apache-2.0, "AI"
    MatteModelBiRefNetLite = "birefnet-lite" // MIT, "Precise"
    MatteModelDefault      = MatteModelISNetAnime
)

// OpMorph (Phase 5) cleans the alpha plane with 3x3 morphology, hoisted into
// the keying group like feather (acts on whatever key or matte precedes it in
// the stack; skipped while the frames carry no alpha). Close = dilation then
// erosion (fills pinholes <= 1 px, never grows the silhouette); Grow 0..4 =
// extra dilations (recovers eaten interiors at the cost of a fringe).
const OpMorph = "morph"
type MorphParams struct {
    Close bool `json:"close,omitempty"`
    Grow  int  `json:"grow,omitempty"`
}
```

No erosion ("choke"): the brief measured it never helps. Nothing a client sends enters a hash unresolved: jobs strips `Resolved`, fills it from its own facts (§4.1 step 1) and supplies the memo path through `Plan.ExtraInputs` (§5.1). Multi-pick colour key needs **no new op**: the SPA emits one `colorkey` op per colour (same similarity/blend); `compiler.key` already stacks keys through `keyKeepingAlpha` (`phase3.go:225–285`; 3.7 ms/frame for four).

### 3.2 Defaults that change

| knob | today (`internal/graph/phase3.go:25–32`) | proposed | evidence |
|---|---|---|---|
| `defaultChromaSimilarity` (zero value) | 0.2 | **0.1** | §2.2: 0.2/0.05 keys the subject on every non-green background |
| `defaultColorSimilarity` (zero value) | 0.1 | **0.08** | §2.2: colorkey best at 0.05–0.08; the stacked default K=4 sim .08 |
| `MinSimilarity` | 0.01 | 0.01 (kept) | meaningful once the range bug is fixed (§5.4) — no floor at 0.06 |
| chromakey `color` emission | `0xRRGGBB` | `0xYYUUVV` limited-range + `:yuv=1`, in a working format pinned to BT.601 limited range: `format=rgba,format=yuva444p:color_spaces=bt470bg:color_ranges=tv` *(as built; the pin, not the rgba pass, is what makes tagged sources key — §5.4)* | §5.4 |
| SPA mirrors `CHROMA_DEFAULTS` / `COLORKEY_DEFAULTS` (`web/src/lib/state.svelte.ts:92–94`) | 0.2 / 0.1 | 0.1 / 0.08 | the serialisers omit the default, so the Go zero value is what renders |

Both default changes alter what existing recipes render → `jobs.PipelineVersion` bump (§11).

### 3.3 Where the ops sit; interactions *(read from `graph.go:355–377`, `phase3.go:93–109`)*

The stage order is unchanged: source head → alpha head → trim → speed → fps → **keying group** → geometry → output fit → reverse/bounce → final canvas. The keying group (`compiler.keying`) grows from `chromakey | colorkey | feather` to `chromakey | colorkey | matte | morph | feather`, still in stack order. `buildOps` emits `[matte | colorkey×N | chromakey]`, then `morph`, then `feather`, so cleanup acts on the key's or matte's alpha and the feather softens the cleaned edge.

- **Matte on opaque frames:** the matte becomes the alpha (bare `alphamerge`). **On alpha-carrying frames** (ProRes 4444, merged AVIF alpha stream, transparent sequence padding, an earlier key): intersected with `blend=all_mode=multiply` — the `keyKeepingAlpha` rule, same semantics. A key after a matte goes through the existing wrapper because `c.hasAlpha` is already true.
- **`morph` / `feather`:** in place on the alpha plane after whichever key/matte precedes them; skipped while `!c.hasAlpha` (feather's rule, `phase3.go:134`).
- **Autocrop:** `matte` and `morph` join `graph.detectOps` and `jobs.detectionOps`, so Crop to content finds the AI-keyed subject; the detection plan runs on the render's frame grid and reads the same memo (§5.6).
- **Reverse / bounce / text / overlays:** untouched (behind the keying group; the matte sequence is forward time; reverse and bounce reorder merged frames).
- **Batch mode:** geometry-independent → offered per row like keying (DESIGN.md §7).
- **Lossless gifsicle fast path:** `checkFastPathSpec` returns false for any op kind outside trim/crop/fps (`internal/jobs/phase4.go:340–342`), so `matte`/`morph` recipes take the decode path automatically — add both to its test table.

---

## 4. Matte pass (`internal/jobs/matte.go`, mirroring `autocrop.go`)

### 4.1 Who runs it, when

One function, `m.resolveMattes(ctx, src, ops, out, mode)`, three callers. **Previews:** inside `jobs.compile` (`internal/jobs/phase3.go:119–135`), which becomes `resolveMattes → resolveAutoCrop (with the resolved mattes) → graph.CompileWithSources → fillExtraInputs`; mode *preview* waits `mattePreviewWait` for a running pass and then returns `ErrMattePending`. **Renders:** a **pre-stage** in `render` (`render.go:93`), after `resolveSources` and the `HasResult` short-cut and **before the render slot `m.sem` is taken** (today the slot is acquired at `render.go:95–100` and `compile` runs inside it at :174 — a pass of up to 600 s would hold one of `EZLG_CONCURRENCY` slots while waiting on the serial sidecar, and N queued AI renders would starve every plain render); mode *render* waits without limit under the job ctx with `StageMatte` progress on SSE. The pass itself takes no render slot: it is bounded by its own single-flight and by the sidecar, which runs one batch at a time. `compile` then receives the resolved mattes, fills the input path and threads them into `ResolveAutoCropFor` (§5.6).

```go
const (
    matteDir           = "mattes"           // <root>/mattes/<key>/ — the per-clip sequences, on /data, not scratch
    matteFramesDir     = "mattes/frames"    // <root>/mattes/frames/<model>/<size>/<prec>/<weights[:8]>-<proc>/<frameSHA>.png
    matteKeyVersion    = "1"
    matteBatchFrames   = 8                   // frames per POST (8 × 3 MB rgb24 at 1024², 8 × 786 KB at 512²)
    mattePreviewWait   = 2 * time.Second     // stills/proxies wait this long for a running pass, then 202
    matteAbandonGrace  = 5 * time.Second     // a pass with no waiter for this long is cancelled
    matteEagerSeconds  = 90.0                // a still starts a pass only when the estimate is under this; Play/Render always do
    matteLoadTimeout   = 300 * time.Second   // a render waits this long for a model that is downloading/loading
    matteProbeFailures = 3                   // consecutive failed /v1/ping probes before features.matte flips off
)
```

The pass (`m.runMattePass`) runs inside `m.mattes.doDetachedAbandon(ctx, key, matteAbandonGrace, fn)`, one at a time per sidecar (no concurrency knob: the sidecar is serial, §7.2):

1. **Identity without a sidecar.** The facts the memo key needs — per model the pinned weights file's sha256 (`weights`, a static pin reported in every state), the sidecar's `processingVersion` (`proc`), its sizes, precision and `msPerFrame` per device — come from `/data/mattes/models.json`, the app's copy of the **last successful `/v1/ping`** (rewritten on every probe that answers). A memo hit therefore needs no live sidecar: a restart, a download, a 20 s session create or an operator who stopped the sidecar to free the GPU never blacks out previews or renders of recipes whose mattes exist (`still.go:97` compiles before its memo read at :110, as does `proxy.go:84`). No facts ever seen → `ErrMatteUnavailable` ("the matte service has not answered yet — is the `matte` profile up?").
2. Compile the **matte input plan** `graph.CompileMatteInput(srcs, ops, out)`: the temporal prefix — source head, hoisted unpremultiply, trim/delay/speed/fps — ending in `[out]` at `format=rgba`, W×H = the source frame, the same InputArgs as the render, Output-aware: `Plan.FPS = SnapFPS(out.Format, …)` exactly as `CompileWithSources` (`compile.go:766–815`: fps op → `Output.FPS` → source → default, then the GIF cap at 50). The Emote/Sticker presets set `Output.fps = 25` (`presets.ts:361, 388`) and `stillOutput` keeps `Format`+`FPS` (`still.go:169–177`), so still, proxy and render share one grid **provided every request carries the same fps** — §6.1 fixes the one that does not (the crop-mode still). One shared function, `c.temporalPrefix()`, produces this prefix for `CompileMatteInput`, `CompileDetectFor` and `CompileWithSources` alike (§5.1).
3. **Memo hit** (`<root>/mattes/<key>/matte.json` readable, `frames` PNGs present): touch the dir, return. Nothing below runs.
4. Up-front refusal (floats and comparisons only — nothing allocates from `Plan.Frames`): `estimate = Frames × (msPerFrame + 2 ms)` with the persisted `msPerFrame`; `Frames > EZLG_MATTE_MAX_FRAMES` (3000) or `estimate > EZLG_MATTE_MAX_SECONDS` (600) → `ErrInvalidRecipe`: "an AI matte of 1800 frames would take up to ~27 min on this server's CPU — trim the clip, lower the fps, or switch off Precise" ("up to": frames the store already holds cost nothing). `Frames == 0` (unknown): no up-front refusal; the run-time frame cap and the hard timeout bound it.
5. **Sidecar state** (`/v1/ping`, cached ≤ 15 s): the model `ready` → run. `downloading` / `loading` → **pending, not an error**: a render waits (honouring `retryAfterMs`, bounded by `matteLoadTimeout`), a preview answers 202 with that state and the percentage (§6.2), and the app POSTs `/v1/warm` so the load is under way. `missing` after a failed download, or `unavailable` (self-test failed, CUDA EP absent) → `ErrMatteUnavailable` with the sidecar's own reason; `protocol ≠ 1` → "update the matte service". The live answer's `weights`/`proc` are compared with the key's before the first POST; a mismatch (the sidecar was upgraded since the last probe) re-resolves with the new facts.
6. **Producer**, under a ctx of its own (`pctx`): `ffrun.RunTo(pctx, ffmpeg, enc.MatteSourceArgs(srcPath, plan, size), w)` where `w` is a `batchWriter` that cuts stdout into exact `size×size×3`-byte frames (Content-Length arithmetic, never decoding), counts them against the frame cap, hashes each (sha256 of the rgb24 bytes) and looks the hash up in the **frames store**: a hit — an earlier clip, an earlier trim of this clip, a held pose, an fps-upsampled duplicate — is copied into the clip dir at once; misses go to the sidecar in batches of `matteBatchFrames` distinct frames per `POST /v1/matte`. A POST failure or the frame cap cancels `pctx` first (so `RunTo`'s `cmd.Cancel` = `killTree` ends ffmpeg at once instead of at its next EPIPE, `ffrun/capture.go:38–45`) and the original error — never `ctx.Canceled` — reaches the waiters.
7. The response is `frames` records `[uint32 BE length][PNG bytes]` (8-bit gray, size×size). Go writes each PNG to the frames store (`<frameSHA>.png`, temp + rename) and copies it to `<tmpDir>/%06d.png` (duplicates re-inserted) — opaque bytes to files, as jobs already files every encoder's output; no pixel is decoded in Go, no second ffmpeg.
8. On success: write `matte.json` `{v, key, src, model, weights, proc, graphDigest, precision, size, fps, frames, device, msPerFrame, created}` then `os.Rename(tmpDir, <root>/mattes/<key>)` — atomic; a partial dir is never a memo. tmpDir is `<root>/mattes/.tmp-<key>-<rand>`, so the sweeper can tell an abandoned one from a memo (§4.4). On failure/cancel: remove tmpDir; the frames store keeps every matte already filed, so a killed 20-minute CPU pass resumes from the store, not from zero.
9. Progress: `m.matteProgress[key] = {state, done, total, percent, device}` under `m.mu`, per batch; the render job's stage is `StageMatte` ("AI matte 24/45 · GPU", "loading model (12 s)", "downloading weights 43 %"; percent band 2 → 20, the master band then 20 → 60); previews read it for the 202 body.

### 4.2 Memo keys

```
clipKey  = sha256("matte|" + matteKeyVersion + "\n" + canonical(Recipe{Sources:[src], Ops: temporal})
                 + "\nprobe=" + canonical(probe facts) + "|info=" + store.InfoVersion
                 + "\nfps=" + fnum(plan.FPS) + "|model=" + model + "|size=" + size + "|prec=" + precision
                 + "|weights=" + weights + "|proc=" + proc)
frameKey = sha256(the size×size×3 rgb24 bytes the model receives)       under frames/<model>/<size>/<prec>/<weights[:8]>-<proc>/
```

`temporal` = the stack's `delay/unpremultiply/trim/speed/fps` ops in stack order (keys, feather, morph, geometry are **not** in it: the model sees RGB only). *probe facts* = the `ProbeInfo` fields the prefix compiles from — `Kind`, `IsStill`, `Width/Height`, `FPS`, `Duration`, `ColorStream/AlphaStream`, `Premultiplied`, `Sequence{Pattern, Count, DelayMS, Mixed}` (`Mixed` as built: a mixed-size sequence is normalised to a canvas, which changes the frames the model sees) — plus `store.InfoVersion`: the memo lives on `/data` and outlives restarts and upgrades, and a re-probe under new semantics (`store.go:390–394`) changes the frame grid under an otherwise identical key; with the facts in the key an `InfoVersion` bump re-mattes exactly the sources it re-probes. `fps` is the compiled plan's effective rate (GIF-at-50 and WebP-at-60 are two clip keys whose frames mostly hit the store). `weights` and `proc` come from the persisted facts (§4.1 step 1), never from a live digest; the loaded graph's own digest is recorded in `matte.json` and in the `render.matte` report for forensics only. The device is **not** in the key (CPU and GPU mattes of the same graph are equivalent, not bit-identical); `PipelineVersion` is not either (a 20-minute CPU pass survives app upgrades, §11). The frame key sees only pixels: a trim, speed, fps, `Output.FPS` or format-class change costs the decode and the hash (1.8 + ~2 ms per frame) plus the model for frames the store has never seen — none at all for a GIF or sequence trimmed on its own grid.

### 4.3 Single-flight, timeouts, cancellation, limits

- `flight.doDetachedAbandon`: `doDetached` (`internal/jobs/flight.go:53`) plus a waiter count; when it stays at zero for the grace period the leader's ctx is cancelled. Autocrop keeps plain `doDetached` (a 5-minute pass that must finish for the next request); a 20-minute CPU matte nobody waits for must not. A polling SPA re-joins every 500 ms, inside the grace.
- Hard timeout per pass `min(2 × EZLG_MATTE_MAX_SECONDS, 3 × estimate + 60 s)`; per-batch HTTP timeout `60 s + batch × 5 × msPerFrame`, scaled by the `busy` count the sidecar reports (another app instance's pass inside it). Internal timeouts surface as plain errors, never context errors, so `flight` never retries them (autocrop's rule, `autocrop.go:257–262`).
- Run-time frame cap: the `batchWriter` aborts the pass at `EZLG_MATTE_MAX_FRAMES + 1` streamed frames with the same message as the pre-flight refusal (closes the unknown-count blind spot without allocating from `Plan.Frames`).
- Eager bound: a **still** starts a pass only when `estimate ≤ matteEagerSeconds` (90 s ≈ 660 frames of CPU isnet at 136 ms, ≈ 5 000 frames of GPU isnet); above it the still answers 202 `state: "deferred"` and the SPA offers "Compute now"; Play, "Compute now" and Render pass `eager: true` on `previewRequest` and always start it. With the frames store this mostly matters for the first pass over a long CPU clip.
- Memory: one batch in flight (≤ 25 MB at 1024²) plus 32 B of hash per streamed frame. Scratch: none — frames stream through pipes; the memo lands on `/data`.
- Admission order in `render`: `HasResult` → matte pre-stage (no slot) → render slot → `compile` → `admitScratch` (`render.go:222`); the autocrop stays inside `compile` as today ("refused up-front holds only after detection", DESIGN.md §4.1). Test: `Concurrency = 1`, one AI render blocked on a slow fake sidecar, a plain render completes meanwhile.

### 4.4 Memo on `/data` and the sweeper

`store.MatteDir(key)` and `store.MatteFrameDir(…)` under `<root>/mattes`. `store.Sweep` today lists only results and blobs (`store.go:655–734`); it learns a third class, **mattes** — the clip dirs and the frames store. Age pass: a clip dir is aged by its own mtime (touched on every read, like `readMemo` touches stills, and whenever a render touches its source blob) under the same TTL; a clip dir whose `src` blob no longer exists is removed (dead weight until its own TTL otherwise); a `.tmp-*` dir older than `inProgressGrace` (1 h — the results rule at `store.go:789–795`; a pass killed by shutdown or SIGKILL leaves one, since a detached pass runs under `context.WithoutCancel` and the drain is 8 s) is junk; frames-store files are aged by their mtime, touched on every hit. Size pass: results, then blobs, then mattes **last** — a matte is kilobytes to a few MB (288 KB per 45 frames at 720²; ≈ 30 MB for 3 000 × 1024²) and the most expensive thing on the disk to regenerate, so evicting it before blobs reclaims nothing. To close the eviction-during-render race the judges flagged: `store.Store.Protect(dir) (release func())` keeps an in-memory set the sweeper never deletes; `render` protects its matte dir from `renderMaster` to the end of the job, previews for the duration of their ffmpeg run. Clip dirs are copies, not hard links, so a clip dir and the frames store are independently sweepable (the copies are small; hard links would need inode-aware sizing).

---

## 5. Compiler integration (`internal/graph`, pure, golden-tested)

### 5.1 Plan changes (additive; `graph.go:298–318`)

```go
type ExtraInput struct {
    Source int; Path string; Args []string; Animated bool; Duration float64; Loop bool
    // Matte (Phase 5) marks a matte-sequence input of the main source
    // (Source 0): Args from the compiler are "-f image2 -framerate
    // <fnum(Plan.FPS)> -start_number 1"; Path is "" until jobs fills
    // "<dir>/%06d.png" and Frames from the memo's manifest. enc rewrites
    // Args/Path per consumer (one unlooped frame for forward stills, a later
    // -start_number for tail-seeked reversed plans), see enc.matteInputArgs.
    Matte *MatteInput
}
type MatteInput struct {
    Model  string
    Size   int
    FPS    string // fnum(Plan.FPS) — the text of the plan's fps stage, for a string-exact check in jobs
    Frames int    // the memo's frame count (set by jobs; 0 = unknown)
}
// Plan additionally gains Bounces int (the bounce count; Bounced == Bounces > 0):
// the count check in jobs divides the master's frames by 2^Bounces (§12).
```

Registration happens **in `compiler.keying`**, appending to `Plan.ExtraInputs` exactly like `extraInput` does for overlays (`phase3.go:994–1008`: index = position, ffmpeg input = index + 1), so the `[k:v]` label is known when the matte stage is emitted and overlays registered later in `finalCanvas` simply follow it. `enc.extraInputArgs` / `planUsable` (`enc/phase3.go:359–385`) keep working unchanged; `jobs.fillExtraInputs` (`jobs/phase3.go:103–112`) gains a branch: `Matte != nil` → `Path = store.MatteDir(key)/%06d.png`, `Frames` from `matte.json` (today `Source < 1` is an error). Two matte ops with the same model/size share one input (`c.matteRefs`). **The fps check lives in jobs, not the graph** (graph is pure and reads no file; `CLAUDE.md`): `fillExtraInputs` compares the manifest's `fps` with `MatteInput.FPS` string-exact and fails with an `ErrInvalidRecipe`-class "matte was produced at 15 fps, the plan runs at 25 — stale resolution"; with the Output-aware plan it never fires, it is the belt to the prefix test's braces.

New entry points: `CompileMatteInput(srcs, ops, out)` (the temporal prefix only, Output-aware fps, no ExtraInputs/TextFiles) and `CompileDetectFor(srcs, ops, out)` (`CompileDetect` with the render's `SnapFPS`; `CompileDetect` stays and delegates with an empty Output — it compiles with `recipe.Output{}` today, `graph.go:447`). All three entry points call one `c.temporalPrefix()`, and the golden asserts on **stage lists, not Filter bytes**: the matte plan's stages minus its terminal `format=rgba` equal the render plan's leading stages up to the first keying stage, and `InputArgs` are equal (the trim seek is half of the "same frames" guarantee), for six stacks — video, sequence, AVIF alpha stream, yuva ProRes, FilterTrim WebP, bounce. A byte-prefix assertion would fail on exactly the alpha-carrying stacks it names: `assemble` closes the matte plan with `format=rgba` (`compile.go:1010–1014`) while the render's chain continues into `split` (§5.2).

### 5.2 Filtergraph shape of the merge (`compiler.matte`, phase3.go) *(shapes verified pixel-exact in R2)*

Input k = ExtraInputs index + 1, wrapper n, current frame W×H (the source frame; the normalised canvas of a mixed sequence). The matte branch scales the model square to W×H in-graph (sizes must match; bicubic for a smooth rim — GIF thresholds it anyway). Both variants call `c.ensureRGBA()` first, so a > 8-bit RGB alpha source (`gbrap10le/12le` head, `alphaHead` `compile.go:535–537`) reaches `alphamerge` as rgba by an explicit conversion rather than one the negotiator picks.

Opaque frames:
```
<chain so far>,format=rgba[m1];
[k:v]format=gray,scale=W:H:flags=bicubic[m1a];
[m1][m1a]alphamerge,…
```
Alpha-carrying frames (the `keyKeepingAlpha` shape with the key-on-a-copy chain replaced by the matte branch):
```
<chain so far>,format=rgba,split[m1][m1m];
[m1m]alphaextract[m1a0];
[k:v]format=gray,scale=W:H:flags=bicubic[m1a1];
[m1a0][m1a1]blend=all_mode=multiply[m1a];
[m1][m1a]alphamerge,…
```
(`format=rgba` is emitted only when the chain does not already end in one — `ensureRGBA`'s rule.) Implementation: a sibling of `keyKeepingAlpha` (`mergeMatte`) closing the chain the same way (`head := c.input + join(stages…, "split")`), then `c.input = "[mN][mNa]"`, `c.emit("alphamerge")`, `c.hasAlpha = true`. `keyKeepingAlpha`'s own text stays byte-identical (existing goldens). No `shortest`/`eof_action` on `alphamerge`: the sequence has exactly the main's frame count by construction; jobs checks the count after `renderMaster` (§12) instead of letting framesync repeat silently. Goldens include a `gbrap12le` source.

### 5.3 `morph` op text (`compiler.morph`) *(option names verified with `ffmpeg -h filter=dilation` on the host build: `coordinates`, `threshold0..3` — there is no `planes` option)*

```
format=gbrap,dilation=coordinates=255:threshold0=0:threshold1=0:threshold2=0,erosion=coordinates=255:threshold0=0:threshold1=0:threshold2=0[,dilation=coordinates=255:threshold0=0:threshold1=0:threshold2=0 × Grow],format=rgba
```

gbrap orders the planes G,B,R,A (the feather precedent, `phase3.go:137–139`); `vf_neighbor.c` clamps each plane to `[p−threshold, p]` / `[p, p+threshold]`, so `threshold<N>=0` leaves G, B, R untouched and `threshold3`'s default 65535 makes the A plane a plain 3×3 min/max; the 8-bit repacks are lossless. Skipped while `!c.hasAlpha`. Validation: `Grow` in 0..4, at least one of Close/Grow set (else a compile error, like a feather radius out of range). *(to verify at pixel level, Phase 5a: a 1-px hole closes, colour planes byte-identical, grow +1 adds exactly one ring.)*

### 5.4 chromakey fix (`compiler.chromaKey`, `phase3.go:159–194`)

**As built (Phase 5a, ratified 2026-10-08 after the pixel test):** emit `format=rgba,format=yuva444p:color_spaces=bt470bg:color_ranges=tv,chromakey=color=0xYYUUVV:similarity=S:blend=B:yuv=1` (`yuv` verified present in `ffmpeg -h filter=chromakey`; `color_spaces` / `color_ranges` are `format` options since FFmpeg 7.0, listed by `ffmpeg -h filter=format` on the 9.0.1 runtime image and the host git build) with the BT.601 **limited-range** values of the RRGGBB key — what `format=yuva444p` produces from an RGB frame: `Y = round(16 + (65.481R + 128.553G + 24.966B)/255)`, `U = round(128 + (−37.797R − 74.203G + 112.0B)/255)`, `V = round(128 + (112.0R − 93.786G − 18.214B)/255)` — green 0x00ff00 → `0x913622` (U 54, V 34, the measured frame values; Y is ignored by the filter and emitted for honesty), blue 0x0000ff → `0x29f06e` (41/240/110 measured). The draft of this section prescribed the unpinned `format=rgba,format=yuva444p,chromakey=…:yuv=1` and expected the rgba pass alone to make a bt709-tagged source key (the tagged matrix on the way to RGB, the 601-limited one on the way back). **The pixel test disproved that**, and the brief said to report the values rather than loosen the test — measured on the host FFmpeg 9 git build with a bt709-tagged tv-range yuv420p clip of 0x00ff00 (stored Y 172 U 42 V 26): `format=yuva444p` → U 42 V 26; the spec's `format=rgba,format=yuva444p` → U 42 V 27, still 0.047 from the 601 key 0x913622 — libavfilter's colorspace / range negotiation carries the source's tags across the rgba link, so the second `format` re-encodes the frame with the source's own matrix; `format=rgba,format=yuva444p:color_spaces=bt470bg:color_ranges=tv` → U 54 V 35 (and `format=yuva444p:color_spaces=bt470bg:color_ranges=tv` alone → U 54 V 35 as well: the rgba pass is redundant for this purpose). So the **pin is the contract**: it yields U 54 / V 35 for every tagging tried (bt709 tv and pc, bt470bg, untagged, RGB-decoded, bt709 ProRes 4444) and the exact colour keys at 0.02. The leading `format=rgba` is kept as the brief mandates (through `ensureRGBA` inline on opaque frames; a redundant `format=rgba` inside the alpha-keeping wrapper is harmless) but is *not* what makes tagged sources key. `despill` stages unchanged. The 0.02 guarantee covers every frame in the pinned format. Pixel test (`internal/graph/keying_ffmpeg_test.go`, `TestChromaKeyExactColourPixels`): the exact key colour keys at similarity 0.02 for green, blue and two custom colours from an RGB-decoded PNG **and** from bt709-tagged yuv420p sources at tv and pc range, plus green from a bt709-tagged ProRes 4444 through the alpha-keeping wrapper (before the fix nothing below ~0.047 keyed).

### 5.5 ExtraInputs per consumer (`internal/enc`) and the still slot

`extraInputArgs` keeps emitting `[Args...] -i Path` for overlays; for a matte input each builder passes its own form through a sibling `extraInputArgsFor(p, matte matteArgs)`:

| consumer | matte input | why |
|---|---|---|
| `MasterArgs`, `CropDetectPlanArgs` | `-f image2 -framerate F -start_number 1 -i <dir>/%06d.png` | the decode starts at TrimStart = output t 0 = frame 1 |
| `ProxyArgs` forward / bounced / SeekUnsafe; `StillArgs` for bounced plans and for reversed plans decoded from TrimStart (VFR, FilterTrim, `fromStart`) | same — the full sequence | unseeked (`enc.go:278–331`, `reversedSeekFor`'s unseeked branches); the merge precedes the reverse/bounce stages, so reversed and mirrored frames carry their own mattes **by construction** — no slot fold, no count arithmetic |
| `ProxyArgs` and `StillArgs` for a reversed plan with a CFR tail seek to source time S | `-start_number K+1`, K = the aligned slot count the seek was snapped to | the main's frames after the seek carry timestamps from 0, so the matte sequence must begin at slot K. K is **returned** by `reversedSeekStart` (a sibling returning `(start, slots)`) and stored in `stillSeek.slot` — `proxySeekFor` (`enc.go:1101–1116`) sees only the time today, and re-deriving K from it would bring back the float phase error `alignedSlots` exists to avoid (`seekPhaseTolerance` 1e-7) |
| `StillArgs` / `StillArgsFromStart`, forward plans | `-f image2 -framerate F -i <dir>/<min(abs, Frames−1)+1 as %06d>.png` — one PNG, **no `-loop`** | a single image2 frame at pts 0 is paired with every later main frame by framesync's defaults, whatever the seek-back; `-loop 1` would make the input infinite and cost a late still 1.8 s (§2.3, measured) |

The still's slot is **not** re-derived: `stillSeekFor` already computes the absolute output slot it selects (`abs`, `enc.go:958`), stored in a new `stillSeek.slot`, and `matteInputArgs` clamps it to `MatteInput.Frames − 1` when the count is known. `Plan.Frames` is an estimate for every non-sequence source (`floor(Duration×FPS + 1e-4)` on the probed duration and rate, which "may not be the source's exact cadence", `graph.go:200–208`) and overshoots the decoded count by one for ordinary VFR animations and container durations; `clampStillTime` (`still.go:197–199`) and `capSlot` then select the slot past the last matte — with the clamp the held last frame gets the last matte instead of a missing file. `Frames == 1` → slot 0. No preview is ever refused on the estimate. *(to verify: a pixel test comparing the still's alpha with the rendered master's frame j for forward, reversed, bounced and `[reverse, bounce]` plans, plus a one-frame-overshoot case; a timing test that a still at t ≥ 30 s at 50 fps lands in the same time band as a matte-less still.)*

The producer builder (new, golden-tested):
```
MatteSourceArgs(src, p, N): [p.InputArgs...] -i <src|pattern> -filter_complex "<p.Filter>;[out]format=rgb24,scale=N:N:flags=bicubic[mi]" -map [mi] -an -sn -dn -f rawvideo -pix_fmt rgb24 pipe:1
```
The stretch to N×N (no letterbox) is what rembg and the benchmark do; the in-graph `scale=W:H` on the matte branch undoes it. Transparent pixels of an alpha source reach the model as their stored colour (black for premultiplied exports) and the intersection masks them again.

### 5.6 Framerate pairing and the detection plan

Frames pair by timestamp: the matte input declares `-framerate F` = `fnum(Plan.FPS)`, the same text as the plan's `fps=F:round=down` stage, so matte i carries i/F like master slot i. `CompileDetectFor` applies `SnapFPS(out.Format, …)` like the render. `resolveMattes` returns the resolved mattes (`[]resolvedMatte{model, size, prec, dir, manifest}`) and `compile` threads them into `ResolveAutoCropFor(ctx, src, ops, out, mattes)` → `runCropDetect`, which compiles `CompileDetectFor` and **fills every Matte ExtraInput's Path** (the full sequence, `-start_number 1`, `-framerate` = the detection plan's fps, which equals the memo's) before `CropDetectPlanArgs` — today `runCropDetect` hands the plan straight to enc (`autocrop.go:280–287`), and an extra input without a Path makes `planUsable` false and the args nil ("crop to content is not available in this build"); without this the headline interaction of §3.3 never runs. A matte op with no resolved matte for its model/size is an `ErrInvalidRecipe`. `autocropKey` adds `|fps=<fnum(plan.FPS)>` and, when a matte op is present, the clip key (`autocropKey` hashes only the canonical detection ops today, `autocrop.go:215–225`, so a weight re-pin would serve a stale box from the scratch memo). `detectOps`/`detectionOps` gain `OpMatte`, `OpMorph`. Jobs test: "matte + autocrop resolves to the subject box" against the fake sidecar.

### 5.7 Post-processing placement

`morph` and `feather` in-graph after the merge (one implementation for AI and colour keys). **No temporal median**: the only measurement of one (busy .0207 → .0177) is on the classical mattes, the AI mattes' flicker on the same moving background is 0.0023 (isnet) / 0.0002 (lite), and nobody measured whether a median helps them. If it is ever revisited, the precondition is one measurement of `tmedian` over the isnet mattes of `busy` and the real capture, and the place is the sidecar (±1 context frames per batch, memo-resident) — an in-graph `tmedian` on the matte branch cannot be reproduced by a single-frame still, so still and render would differ in exactly the flicker pixels.

---

## 6. Previews

### 6.1 Memo hit (the steady state)

A still adds one `-i` of a single gray PNG and an `alphamerge` (+ a `scale` of a ≤ 1024² gray to W×H): measured 96 vs 75 ms for a late still on the host. The proxy adds a 45–450-frame PNG sequence decode (≤ 1 ms per small gray PNG). `stillKey`/`proxyKey` already hash the canonical ops (`still.go:209–224`, `proxy.go:226–241`) and `stillOutput` keeps `Format`+`FPS`; when a matte op is present both keys additionally fold in the resolved clip key (which carries `weights`/`proc` from the persisted facts — no live sidecar needed): a weight re-pin or sidecar update can never serve a stale preview from the scratch memo, which outlives sidecar restarts. **Crop mode:** `Preview.svelte:140` sends `output: { format: app.output.format }` for the crop-rectangle still, dropping `Output.FPS`, so its plan's fps resolves to the fps op or else the **source** rate (`compile.go:778–788`) — with a 25 fps preset over a 30/60 fps source that is a second clip key per clip (and a second autocrop detection). Fix: crop mode sends `{ format, fps }` (geometry is what crop mode must drop, not the rate), pinned by a unit test that the crop-mode and normal stills compile to the same fps for a preset with `Output.fps`; the server rule, documented in §4.2, is that the matte key follows the request's effective fps and clients send `Output.FPS` consistently. `stillMemoVersion`/`proxyMemoVersion` are bumped at 5a anyway (the chromakey text change).

### 6.2 Memo miss (the pass runs, or the model is not ready)

`StillSources`/`Proxy` call `compile` with `matteWait = mattePreviewWait` (2 s): on the RTX 5080 a 45-frame pass finishes in ~1 s, so GPU users mostly never see a pending state. Past the wait the manager returns `ErrMattePending{State, Done, Total, Percent, Device, EstimateMS}`; the server maps it to **`202 Accepted`** with the `/api/matte` object (§9) plus `{"pending":"matte","state":"running"|"deferred"|"loading"|"downloading","done":24,"total":45,"percent":43,"estimateMs":810}` — `previewError` maps `ErrInvalidRecipe` to 400 and deadlines to 504 today (`api.go:625–638`); 202 is a new case before them. Blocking instead would 504 on CPU (`stillTimeout` is 60 s, `server.go:170`; a 300-frame CPU pass is 41 s, Precise far more). The SPA: `fetchStill`/`fetchProxy` (`api.ts:696–712`) see `res.status === 202` (today `request()` only throws on `!res.ok`, so the JSON would reach `res.blob()`) and throw a typed `MattePending`; `StillScheduler.load` (`still.ts:165–186`) keeps the image on stage, sets `view.pending`, and re-requests after 500 ms unless superseded (the abort/supersede logic is unchanged); the pill reads "AI matte 24/45 · GPU", "AI matte: loading model… (12 s)", "downloading weights 43 %", or "AI matte: ~3 min on CPU — Compute now" for `deferred`. The preview slot (`acquirePreview`) is not held while waiting; the pass takes no preview slot. Renders (`POST /api/jobs`) wait without limit under the job ctx with SSE progress at stage `matte` (the load states bounded by `matteLoadTimeout`).

### 6.3 Reversed / bounced / seek-unsafe plans

Covered by the table in §5.5: forward stills use one PNG, every other consumer the full sequence, reversed plans with a CFR tail seek the `-start_number` shift. Nothing is refused on `Plan.Frames`. `admitReversed` is unchanged (the matte adds no buffered frames; the reverse stage still buffers output-sized rgba).

---

## 7. Sidecar

### 7.1 Process model: a separate compose service

Chosen over a child process (2.3 GB of CUDA wheels in every app image, the GPU reservation on the app container) and over a per-job CLI (session create 10–20 s for lite). Immich's ML sidecar is the precedent. Directory `sidecar/`: `Dockerfile` (one file, `cpu` and `cuda` targets), `matte.py` (~250 lines on `http.server.ThreadingHTTPServer` — stdlib, one thread per request; `/v1/ping` never takes the inference lock), `png.py` (a ~30-line zlib + struct gray-PNG writer: resizing is ffmpeg's, so cv2's 50 MB and its LGPL FFmpeg notice bought one function), `models.json` (per model: a **list** of pinned URLs, sha256, pre/post-processing), `tools/to_fp16.py` and `tools/rescale_graph.py` (the benchmark's derivation scripts, checked in), `tools/pin-model.py`, `requirements-{cpu,cuda}.txt` generated by `pip-compile --generate-hashes` with `==` pins (`--require-hashes` cannot take `~=`), `LICENSES/` (ORT, both models; the cuda image adds the NVIDIA EULA and cuDNN SLA texts and a `NOTICE` listing every wheel's licence), pytest.

### 7.2 API (plain HTTP/1.1, compose network only)

```
GET  /v1/ping        answered from an atomically updated state snapshot, never behind the inference lock
  200 {"protocol":1,"version":"<image tag>","instance":"<random id per start>","processingVersion":"1",
       "device":"cuda"|"cpu"|"unavailable","reason":"",
       "gpu":{"name":"NVIDIA GeForce RTX 5080","totalGiB":16,"freeGiB":12.4}|null,
       "models":{"isnet-anime":{"state":"ready"|"loading"|"downloading"|"missing"|"unavailable","reason":"","percent":43,
                                "weights":"<sha256 of the pinned source file — reported in every state>",
                                "graphDigest":"<sha256 of the loaded graph>","precision":"fp16","sizes":[1024,512],
                                "defaultSize":1024,"msPerFrame":{"1024":18,"512":9},"licence":"Apache-2.0","lastError":""}, …},
       "busy":0}
POST /v1/matte?model=isnet-anime&size=1024&frames=8
    frames ≤ 32, size ∈ the model's sizes, Content-Length = frames × size × size × 3 ≤ 128 MB; socket read/idle timeout 30 s
    body: application/octet-stream, rgb24, row-major
    200: application/x-ezlg-mattes — frames records of [uint32 BE length][8-bit gray PNG size×size], then a zero-length terminator
    400 bad params / length mismatch · 404 unknown model/size · 413 too large · 503 {"error":"model loading","retryAfterMs":…} | {"error":"out of memory"} · 507 OOM after unload
POST /v1/warm?model=birefnet-lite     create the session now (called when Precise is ticked, so the 10–20 s load overlaps the first scrub)
POST /v1/unload[?model=…]            release sessions now (a desktop that wants its GPU back)
```

Sequential 8-frame batches keep request and response strictly ordered (no full-duplex streaming). A missing terminator or a short record count is a failed pass. Pre/post-processing per model is the benchmark's (`docs/reviews/background-removal-bench/runners/isnet-anime-onnx.py`, `models.json` entries): isnet-anime — resize done by ffmpeg, `/255`, mean (0.485, 0.456, 0.406), std 1 (rembg `dis_anime.py`, not ImageNet std), NCHW float32 (fp16 cast at IO on CUDA), the in-graph sigmoid output clipped to [0,1]; BiRefNet-lite — ImageNet mean/std, sigmoid on the logits. **Never min–max normalisation** (per-frame rescale = flicker; research notes, model landscape). One log line per request: `matte model=isnet-anime device=cuda frames=8 size=1024 ms_per_frame=18.4 total_s=0.15 vram_peak_gb=0.76`. pytest: `/v1/ping` answers in < 50 ms while a batch executes on a slow fake model (a CPU batch of 8 × 136 ms would otherwise stall it ~1 s and a lite-on-CPU batch over a minute, failing the healthcheck and the app's probe during every pass).

### 7.3 Lifecycle

- **Startup:** download `MATTE_PRELOAD` into `/models` — the cuda target defaults to `isnet-anime,birefnet-lite` (400 MB once into the named volume, so Precise is `ready` after start instead of `missing` until a request no UI would send), the cpu target to `isnet-anime`. Per model `models.json` lists the pinned rembg GitHub release `v0.0.0` asset first (`isnet-anime.onnx` sha256 `f15622d8…6e99`, `BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx`) and a Hugging Face mirror second, tried in order with exponential backoff; `.part` resumed via `Range`, rename on a verified sha256; `MATTE_MODELS_BASE_URL` overrides the list. `/ping` reports `downloading` with a percentage; a model still `missing` after the retries is retried every 10 min with `lastError` in `/ping`. `docker compose run --rm matte download [model…]` fetches everything for an air-gapped host (copy the volume).
- **Derived graphs — decided: derived at first load.** The sidecar derives `isnet-anime.fp16.onnx` (CUDA: fp16 initialisers and internal tensors, Resize roi/scales kept fp32, Cast at IO — `to_fp16.py`, measured MAE vs fp32 0.00005) and `isnet-anime.512.onnx` (CPU: IO dims and the 34 hard-coded Resize `sizes` rewritten, `rescale_graph.py`), caches them in `/models` next to the source with their own sha256 stamps, and reports the loaded file's `graphDigest`. The `onnx` package is ~16 MB and the derivation ~30 s once per volume; a derivation change bumps `processingVersion`, which keys the memo, so hosting the derived files as release assets would add an artefact to publish for no gain. The fp32 1024² source graph is the fallback if a derivation fails.
- **Self-test:** every model is self-tested when it loads — a **VRAM pre-check** first (lite is marked `unavailable` without a load when `gpu_mem_limit < 6 GiB` or the card's free memory < 7 GiB: the sweep shows 3/4/5 GiB caps fail and 6 GiB needs a 6.3 GB footprint, so on an 8 GB card the margin is the CUDA context; the reason names the figure and `/ping` reports `freeGiB`), then session create under its cap, one synthetic inference, `msPerFrame` from an 8-frame warm-up (refined by a running mean), peak arena — and marked `ready` or `unavailable` with the reason ("needs about 7 GB of free GPU memory, 5.9 GB free"; "cpu: 14 GiB of RAM needed, 9 GiB available"). Preloaded models self-test at startup, so the estimate line has a measured number before the first pass.
- **Sessions:** lazy per (model, size, precision), one inference at a time per model (a lock around `sess.run`; ORT releases the GIL inside it, so `/ping` and the next request's framing proceed), unloaded after `MATTE_MODEL_TTL` idle for **every** model (default 600 s; `0` = never; isnet recreates in 1 s, lite in 10–20 s, mostly hidden by `/v1/warm`). One knob, documented for both boxes (§8).
- **Residency rule:** on a card under 10 GiB only one model is resident at a time (LRU unload): lite's 6.3 GB + isnet's ~1 GB + the CUDA context does not fit the 8 GB RTX 3070 safely. After lite's startup self-test on such a card it is unloaded again and reloaded on demand (shown as `loading`). Lite stays **unverified** on the 3070; the self-test decides and the UI greys Precise with the reason.
- **VRAM:** `gpu_mem_limit = MATTE_GPU_MEM_LIMIT_GIB` (default 6; lite stays at 6.3 GB with it, 14.5 GB uncapped; **lower caps fail** — the knob is for larger cards, never a remedy for smaller ones), `arena_extend_strategy=kSameAsRequested`, default `cudnn_conv_algo_search` (HEURISTIC + the 6 GiB cap fails in a deform-conv). isnet runs uncapped (0.76 GB). **Pre-5b benchmark:** an fp16 lite graph on the 5080 — `to_fp16.py` over the rembg graph, or onnx-community's `BiRefNet_lite-ONNX` fp16 export (115 MB; the fp16 *general* graph held 4.35 GB under a 4 GiB cap) — for IoU against fp32 and VRAM under a 4 GiB cap; if it holds, it is the lite graph on cards under 10 GiB (`precision` keys the memo; 5c).
- **CPU:** `MATTE_DEVICE=auto` picks CUDA when the EP is available else CPU with a warning; `MATTE_DEVICE=cuda` without the EP **stays up and reports**: `/ping` answers `device: "unavailable"` with the reason ("CUDA EP not available — nvidia-container-toolkit / driver ≥ 580?"), the healthcheck returns 503 and the app shows AI off with that text (an exit would loop under `restart: unless-stopped`, with nothing answering to carry the reason). `intra_op_num_threads = MATTE_THREADS` (0 = physical cores — the pass is what the user waits for; lower it on a box whose renders share the cores), `session.intra_op.allow_spinning = 0` (no idle burn). isnet at 512² is the CPU default; **lite is offered on CPU** when the self-test runs it (`MemAvailable ≥ 14 GiB`, measured RSS 12.6 GB) and the clip's estimate is under the cap — no `MATTE_ALLOW_SLOW`.
- **Health:** `HEALTHCHECK CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:9402/v1/ping',timeout=3).status==200 else 1)"` — `python:3.12-slim` ships no curl (the app's own Dockerfile apt-installs curl for its healthcheck, `Dockerfile:105, 182`). The app's 30 s probe flips `features.matte` off only after `matteProbeFailures` consecutive failures (one timeout = degraded, logged) and logs state **transitions** only — with `EZLG_MATTE_URL` set in every install and no profile running, a warning every 30 s would otherwise never stop.
- **Security:** **no `ports:`** — reachable only as `http://matte:9402` on the compose network; binds 0.0.0.0 inside the container only; no auth; uid 1000, `read_only: true` root filesystem with `tmpfs: [/tmp]`, `HOME=/tmp`, `CUDA_CACHE_PATH=/models/.nv` (the PTX JIT cache survives restarts, Immich's precedent), `cap_drop: [ALL]`, `no-new-privileges`. The integration test asserts **no published port** (`docker compose port matte 9402` empty, `HostConfig.PortBindings` empty, `network_mode` not host) — a connectivity probe from the host would fail on bare-metal Linux for the wrong reason (the host reaches the bridge) and pass on Docker Desktop for another. Instance ids: a single change is logged at info ("matte sidecar restarted" — every `pull && up -d` and every crash restart makes one); the warning ("two matte sidecars answer at http://matte:9402 — start only one of the matte / matte-gpu profiles") fires only when ids **alternate** within the last few pings, which is what DNS round-robin over two services produces.

### 7.4 Models shipped

| id | file | licence | role | why |
|---|---|---|---|---|
| `isnet-anime` ("AI") | rembg `isnet-anime.onnx`, 176 MB; fp16 1024² on CUDA, fp32 512² on CPU (`Size` is API-only) | Apache-2.0 (weights confirmed by the author, HF discussion #4) | every install | IoU 0.993 (1024) / 0.991 (512); 18 ms GPU, 136 ms CPU; 0.76 GB; crisp; weakness: UI widgets at mid alpha |
| `birefnet-lite` ("Precise") | rembg `BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx`, 224 MB, fp32 1024² (fp16 variant for < 10 GiB cards pending the pre-5b benchmark) | MIT | GPU installs; CPU when the self-test allows (RAM ≥ 14 GiB) | IoU 0.997 everywhere, every hair strand, rejects the stream UI; 10–20 s session create, ~170 ms/frame, 6.3 GB capped |

Not shipped (§2.1): u2netp, BiRefNet-general/portrait, ToonOut, BEN2, Lucida, RVM (GPL), RMBG/MatAnyone (non-commercial). Nothing non-commercial appears in `models.json`.

---

## 8. Docker / compose

`compose.yaml` (additive; today it holds only `app`, with the GPU block commented out — `compose.yaml:72–81`, which stays commented: the sidecar is the only GPU consumer of this phase). The matte services are **pull-only**: compose.yaml documents `up -d --build` for install and update (`compose.yaml:3`, `USAGE.md:14, 30`), and with a `build:` on the sidecar every user who enabled a profile would build it locally — the CUDA target's pip install measured 1,236 s and ~2.3 GB of wheels — and rebuild on every requirements change; air-gapped boxes would fail outright. The `build:` blocks live in `compose.dev.yaml` only (the file that already carries the dev stack and the integration test). YAML `<<` merges top-level keys only, so each service restates its full `environment`.

```yaml
x-matte: &matte
  image: duckautomata/ezlg-matte:${EZLG_MATTE_TAG:-v0.5.0}  # the tag this release ships; pinned, never latest
  pull_policy: missing
  restart: unless-stopped
  read_only: true
  tmpfs: [/tmp]
  cap_drop: [ALL]
  security_opt: [no-new-privileges:true]
  volumes: [ezlg-models:/models]              # weights + derived graphs + CUDA JIT cache; survives upgrades
  networks: { default: { aliases: [matte] } } # the app talks to http://matte:9402 whichever variant runs
  healthcheck:
    test: ["CMD", "python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:9402/v1/ping',timeout=3).status==200 else 1)"]
    interval: 30s
    timeout: 5s
    start_period: 120s
  # NO ports: — no auth; never publish it on the LAN.
services:
  app:
    environment:
      EZLG_MATTE_URL: "http://matte:9402"       # probed every 30 s; 3 misses = feature off; "" disables
      # EZLG_MATTE_MAX_SECONDS: "600" · EZLG_MATTE_MAX_FRAMES: "3000"
      # no depends_on: the app must start without the sidecar
  matte:                                      # docker compose --profile matte up -d        (CPU)
    <<: *matte
    profiles: [matte]
    environment:
      MATTE_DEVICE: cpu
      MATTE_PRELOAD: isnet-anime
      MATTE_MODEL_TTL: "600"                  # 0 = never unload (dedicated box); 120 = give a shared desktop its GPU/RAM back sooner
      HOME: /tmp
      # MATTE_THREADS: "0" · MATTE_MODELS_BASE_URL: ""
    # mem_limit: 16g                          # Precise on CPU needs ~13 GB RSS; the sidecar refuses it below 14 GiB available
  matte-gpu:                                  # docker compose --profile matte-gpu up -d    (CUDA)
    <<: *matte
    image: duckautomata/ezlg-matte:${EZLG_MATTE_TAG:-v0.5.0}-cuda
    profiles: [matte-gpu]
    environment:
      MATTE_DEVICE: cuda                      # stays up and reports when the CUDA EP is missing
      MATTE_PRELOAD: isnet-anime,birefnet-lite
      MATTE_MODEL_TTL: "600"
      MATTE_GPU_MEM_LIMIT_GIB: "6"            # lite's arena cap; lower values fail (§7.3)
      CUDA_CACHE_PATH: /models/.nv
      HOME: /tmp
      NVIDIA_DRIVER_CAPABILITIES: compute,utility
    deploy: { resources: { reservations: { devices: [{ driver: nvidia, count: 1, capabilities: [gpu] }] } } }
volumes:
  ezlg-models:
```

One flag on either box (`COMPOSE_PROFILES=matte-gpu` in `.env` makes plain `up -d` include it); `.env.example` ships `COMPOSE_PROFILES` and `EZLG_MATTE_TAG`, and `.env` joins `.gitignore` (it is not there today, one `git add -A` from a commit). Both variants share the alias `matte`; starting both is a user error the app detects through alternating `instance` ids (§7.3). The profile name `gpu` stays reserved for the app's own NVDEC/NVENC block (DESIGN.md §6/§10); `COMPOSE_PROFILES=matte-gpu,gpu` composes later. Requirements: host driver ≥ 580 (CUDA 13 wheels; 616.92 and ≥ 610 qualify), nvidia-container-toolkit on the bare-metal box, nothing extra under Docker Desktop's WSL2 backend (verified). `ezlg-models` stays a named volume (ext4), never under `/mnt/c`; `docker compose down -v` removes it too (the wipe `compose.yaml:4` documents) — pre-seed with `docker compose run --rm matte download`.

**Images and registry — decided: Docker Hub**, the registry the project already publishes to (`docker-image.yml:87–108` pushes `duckautomata/ez-local-gif:latest` with `DOCKER_USERNAME`/`DOCKER_TOKEN`; GHCR would need `packages: write` and a second login). `sidecar/Dockerfile` = `python:3.12-slim`, `pip install --require-hashes -r requirements-{cpu,cuda}.txt` (`numpy onnxruntime==1.30.x onnx` — cpu, 24 MB ORT; `onnxruntime-gpu[cuda,cudnn]==1.30.x` — cuda, ~2.0 GB NVIDIA + 327 MB ORT), `useradd 1000`, **`mkdir -p /models && chown 1000:1000 /models` before `VOLUME /models`** (a named volume takes the image directory's ownership; without it the first download fails with EACCES as uid 1000 — the trap the app's Dockerfile avoids for `/data`, `Dockerfile:170–173`; USAGE.md gets the repair one-liner next to the `./output` one: `docker compose run --rm --user root --entrypoint chown matte -R 1000:1000 /models`), `LICENSES/` + `NOTICE`, a build-time self-test through a synthetic 8×8 ONNX graph. The workflow's matrix builds **app, matte-cpu and matte-cuda in one run**, tagging each `latest`, `<short-sha>` (the SHA `docker-image.yml` already computes for `VERSION`) and the release tag, so app and sidecar SHAs match; the compose file pins `EZLG_MATTE_TAG`'s default to the release tag it ships with and the `protocol` field guards skew between an app built from source and a pulled sidecar. Upgrade: `docker compose pull matte-gpu && docker compose up -d`; rollback: `EZLG_MATTE_TAG=<previous>` in `.env` and `up -d`; contributors: `docker compose -f compose.yaml -f compose.dev.yaml build matte`.

What a CPU-only install sees: nothing new without a profile (`features.matte=false`; the AI segment is disabled with "needs the matte sidecar — `docker compose --profile matte up -d`"); with `--profile matte` the same UI at CPU speed, the estimate line saying "up to ~6 s AI matte (CPU)" for a 45-frame emote, Precise offered when the self-test ran it and the clip is under the cap ("~3 min on CPU"), greyed with the sidecar's reason otherwise. Upgrade path: app image, `/data` and recipes unchanged; enabling is one profile; a sidecar image with new weights changes `weights`/`proc` → mattes, previews **and results** recompute (§11), no paired app release needed.

---

## 9. UI (Background card, `web/src/components/ops/BackgroundCard.svelte`)

Mode segment: **None · AI · Colour · Screen** (today None · Greenscreen · Bluescreen · Pick a colour): four entries, the screen colour (green / blue) a sub-choice of Screen; "AI" because that is the user's word and "Auto" would read as automatic colour detection next to the Crop card's auto crop. Enabling the card lands on **Colour** (instant, classical); AI is one click and never starts a GPU pass silently. `BackgroundMode` gains `'ai'`; `BackgroundCfg` gains `ai: { precise: boolean }`, `colors: string[]` (1–6), `morph: { close: boolean; grow: number }`; `backgroundOp` returns the matte op or N colorkey ops; `buildOps` appends `morph` after the key and before `feather` (`state.svelte.ts:506–515`).

- **AI:** one checkbox **Precise** ("about 10× slower; for UI stuck to the character or lost hair strands"), greyed with the sidecar's reason when `birefnet-lite` is `unavailable`/`missing`, or when its estimate exceeds the cap; ticking it POSTs `/v1/warm` through the app. Status line: "ready · GPU · up to ~1 s for this clip" / "CPU · up to ~6 s for this clip" / "loading model…" / "downloading weights 43 %" / "sidecar unavailable — run `docker compose --profile matte-gpu up -d`". While a pass runs: pill "AI matte 24/45 · GPU" on the preview (202 polling), Render enabled (the job waits with SSE progress); a deferred pass shows "AI matte: ~3 min on CPU · Compute now". Help text: "Computed once per frame and cached; trim, speed and fps changes re-use what is cached."
- **Live status:** `GET /api/matte` → `{device, reason, gpu, models{state, percent, msPerFrame, reason}, maxSeconds, maxFrames}` (the app's last probe), polled every 5 s by the Background card while AI is visible or a pass is pending, and carried in every 202 body. `/api/capabilities` keeps only the static flag: the SPA fetches it **once per page** (`capabilities.svelte.ts:119–140`), so none of the live states could reach an open page through it.
- **Colour:** the eyedropper pick as today (default similarity 0.08, Blend 0), plus **"+ add colour"** rows (up to 6, each armed through the same eyedropper, or typed hex) for 2-colour ramps; no sampling button. In batch only AI and typed hex colours apply (no preview to pick from, `BatchOpsPanel.svelte:113`).
- **Screen:** green / blue as today; Similarity default 0.1; the Advanced despill fold unchanged.
- **Edge cleanup fold (all modes, closed by default):** "Fill pinholes" checkbox (morph close; default **on** for new sessions — never hurt in any measurement), "Grow matte — N source px" stepper 0–4 (default 0; hint "+1–2 recovers eaten interiors at a 2 px fringe; in source pixels, like Feather, so 4 px on a 720 px source is under 1 px after the emote fit"), link "Soft edge → Feather card" (label note: only WebP/APNG/AVIF keep it — GIF is 1-bit).
- Summary line: "AI · fill pinholes" / "AI · precise" / "2 colours · similarity 0.08" / "greenscreen · similarity 0.10".
- Estimate line under Render (`planMaster`): appends `· up to ~N s AI matte (GPU|CPU)` = `planFrames × (msPerFrame + 2 ms)` from `/api/matte` — the server's own up-front estimate (§4.1 step 4, `matteEstimateMS`), so the SPA's caps note never under-predicts the server's refusal; an error note with the server's own hint when over `maxSeconds`/`maxFrames`.
- Capabilities gating (`capabilities.svelte.ts:27`): `FEATURE_NAMES` + `matte`; an older server leaves AI greyed with the standard "not supported by this server" note; the optimistic default keeps it on until the answer.
- Batch mode: the Background card in `BatchOpsPanel` shows the same modes; rows render through one pass at a time, each row's SSE shows its matte stage.
- Result card: the report gains an info check `render.matte` ("AI matte: isnet-anime fp16 1024², weights f15622d8…, 45 mattes, fill pinholes") so a result always says which model produced it.
- Errors: pass refused → the server message in the card; sidecar down mid-pass → toast + "AI matte failed: sidecar unreachable — retry"; OOM → "Precise needs about 7 GB of free GPU memory — AI is selected".

---

## 10. Classical improvements shipped alongside (Phase 5a, no sidecar)

1. **chromakey range bug** — fixed in the compiler (§5.4); `MinSimilarity` stays 0.01 and now means what it says (no floor at 0.06: the dead zone is the range mismatch, and the exact colour keys at 0.02 once fixed). Default 0.2 → 0.1.
2. **colorkey as the default single key** at similarity 0.08; the card's first classical mode is Colour.
3. **"+ add colour"** — UI only (N `colorkey` ops). Measured oracle: 1.00 on 2-colour ramps, 1.00 on the rotating gradient (6 picks), 0.95 busy; 7–9 % residual MAE on 4-colour and hair-coloured backgrounds — exactly where AI takes over, which is why 5a ships the row and not a swatch workflow.
4. **`morph` op** (close / grow 0..4) — §5.3; close on by default.
5. **Border magic wand — not now.** The classical ceiling on the user's line-art content (IoU 0.999 on every static gradient, 0.95–0.96 on dithered GIFs) but inexpressible in ffmpeg (`floodfill` has no tolerance): it needs a compiled region grow. If CPU-only installs without a sidecar ask for gradient removal later, its drop-in home is **a CPU "model" inside the sidecar** (`model: "wand"`, a `tolerance` query; a flood fill from border seeds is milliseconds) behind the identical `matte` op, memo, compiler and enc paths — never a pixel-touching Go binary. It fails on the stream UI and leaks on busy backgrounds, so it never replaces the AI path.
6. **Plate-difference key — not built** (the same plate-maker tool for cases the wand/AI cover).
7. **Not worth exposing** (research notes, classical methods): `backgroundkey`, `hsvkey`, stacked chromakey, `lumakey`, source denoising before a key, browser k-means sampling.

---

## 11. Versioning and caching

| version | today (read) | change | why |
|---|---|---|---|
| `jobs.PipelineVersion` | `2026-09-20.1` (`jobs.go:548`) | new stamp at 5a, again at 5b | 5a: chromakey `yuv=1` text + both similarity defaults change what every keyed recipe renders; 5b: new ops (one bump per shipped phase, the project's habit) |
| `jobs.ResultKey` | `sha256(recipe hash, PipelineVersion, RulesVersion)` (`jobs.go:553–556`) — never sees weights | the recipe hash now does: `Submit` (and the render) fill `MatteParams.Resolved{weights, proc, size, precision}` from the persisted facts **before** hashing, where `stripAutoCropResolved` already runs (`jobs.go:566`); a client-sent `Resolved` is stripped first | app and sidecar images are pulled independently, so `docker compose pull matte-gpu` alone must not leave Render serving a cached file made with the old weights while every preview shows the new ones (§12's one forbidden thing). With the identity in the hash a weight change is a new result under a new URL and `/out/<hash>/` stays immutable — no cross-image release rule to enforce |
| `discordlint.RulesVersion` | `2026-09-19.1` | unchanged | no Discord rule changes; `render.matte` is an info check like `render.alpha` |
| `store.InfoVersion` | 6 | unchanged (folded into the clip key) | probe semantics unchanged; a future bump re-mattes what it re-probes |
| `stillMemoVersion`, `proxyMemoVersion` | `2026-08-22.1` | bump at 5a | memoised stills/proxies of chromakey recipes show the old (wrong-range) key; at 5b the keys fold in the clip key when a matte op is present |
| `autocropKeyVersion` | `"3"` | 4 at 5a, 5 at 5b | 5a: the chromakey fix changes the keyed picture for identical canonical detection ops; 5b: detection fps follows the render's `SnapFPS`, `matte`/`morph` join the detection ops, the clip key joins the key |
| `matteKeyVersion` | new `"1"` | — | bump when the producer chain (stretch/format), the frame hash or the memo layout changes |
| sidecar `protocol` / `processingVersion` / per-model `weights` | — | folded into the clip key, the frame key, `stillKey`, `proxyKey`, `autocropKey` and the recipe hash | weights or normalisation change → mattes, previews and results recompute without an app release; `protocol ≠ 1` → feature off with "update the matte service" |

What invalidates what: trim/speed/fps/delay/unpremultiply/`Output.FPS`/format class (GIF 50 vs 60) → new clip key, frames re-used from the store; keys/feather/morph/geometry/overlays → same mattes, new still/proxy/result; model, size, Precise → new mattes; sidecar image with new weights or processing → new mattes **and** new results; app update with a `PipelineVersion` bump → new results, same mattes; `InfoVersion` bump → new mattes for re-probed sources.

---

## 12. Failure modes and Discord safety

- **Matte count mismatch:** after `renderMaster` jobs compares `manifest.frames` with the master's count (from the file size, `render.go:478–479`) **against the temporal count**: static renders skip the check (the plan is cut to one frame before `renderMaster`, `render.go:207–210`, and `-frames:v 1` decodes one), and a bounced render compares `manifest.frames × 2^Plan.Bounces` (`Frames *= 2` per bounce, `compile.go:1078–1085`). A mismatch fails the render ("AI matte has 44 frames but the clip decoded to 45 — report this with the source") and **keeps the memo**: the producer decodes the same prefix and would reproduce it, so dropping it would only re-run the pass (1 s GPU / 40 s+ CPU) and fail again. A memo is dropped only when its manifest is unreadable or short of its PNGs. Cannot happen by construction (same InputArgs + temporal stages; Go counts what it streamed) but framesync would otherwise repeat the last matte silently. Tests: `TestRenderMatteBouncedCount`, `TestRenderMatteStaticPNG` (§13).
- **Sidecar down with the matte on disk:** served — the key needs no live sidecar (§4.1 step 1). **Down mid-pass:** a batch POST fails → the pass fails (plain error, no `flight` retry), tmpDir removed, waiters get "AI matte failed: sidecar unreachable at http://matte:9402 — is the `matte` profile up?"; the render job ends in the error state; `features.matte` flips off after three failed probes and back on when the sidecar returns. No fallback to a colour key or to CPU (a different picture delivered silently is the one thing the hard requirement forbids). Partial memos never exist (rename-on-success); filed frames survive in the store.
- **Model loading / downloading:** pending, not an error — 202 with the state for previews, a bounded wait for renders (`matteLoadTimeout`), `/v1/warm` already running (§4.1 step 5). Only `missing` after a failed download and `unavailable` refuse, with the sidecar's reason.
- **OOM on 8 GB cards:** the VRAM pre-check marks lite `unavailable` when under 7 GB are free (the sweep: 3/4/5 GiB caps fail), Precise is greyed with that reason, AI selected; a mid-batch OOM → 507 → the pass fails with "Precise needs about 7 GB of free GPU memory", the sidecar unloads and re-tests before offering it again; isnet (0.76 GB) is never affected; the residency rule keeps one model resident on < 10 GiB cards. The fp16 lite variant (§7.3) is the real remedy for such cards.
- **CPU time:** refused up-front from `frames × msPerFrame` (600 s default ≈ 4 400 frames of isnet at 512² on the 7800X3D — the 3 000-frame cap binds first there — and 60–160 frames of lite at 3.7–10 s); hard timeout 2× the cap; per-batch timeout scaled by `msPerFrame`; passes nobody waits for are cancelled after 5 s; stills above the 90 s eager bound defer the pass to Play/Render/"Compute now"; frames already in the store cost nothing. The CPU figures are the dev workstation's; the UI shows the self-test's number only.
- **Unknown frame count** (`Frames` 0): no up-front refusal, no `total` in progress; the run-time frame cap and the hard timeout bound it — the same blind spot every byte cap shares (DESIGN.md §4.1), now closed by the streamed-frame count.
- **Frame-estimate overshoot** (`Plan.Frames` one above the decoded count): previews are never refused on it; a forward still's slot is clamped to the memo's last matte, reversed/bounced previews use the full sequence (§5.5).
- **Flicker in 1-bit GIF:** per-frame models measured ~0 flicker on static backgrounds (0.0002–0.0012); the 1–2 px soft rim thresholds cleanly at 128; `morph` close removes single-pixel pops; mid-alpha UI fragments (isnet's weakness) pop at the threshold → `Output.AlphaThreshold` ("trim fringe" 180) and Precise are the knobs. A stable matte keeps identical frames identical, so `MergeGIFHolds` and `gif.noop-frame-disposal` behave exactly as for keyed clips.
- **Alpha sources** (ProRes 4444, AVIF alpha stream, transparent sequence padding, keyed earlier): multiply-intersected, never overwritten; the model sees the unpremultiplied RGB after the alpha head.
- **VFR animation sources, SeekUnsafe animated WebP, FilterTrim:** the matte is computed on the plan's CFR output grid from the same unseeked/filter-trimmed input, so it aligns by construction; previews of such plans are unseeked today and start the matte at frame 1.
- **Sweeper / disk:** the pass uses no scratch (pipes); the memo is on `/data` under the sweeper, protected while a render or preview reads it, evicted last; ENOSPC maps to the existing operator message; a tmp dir left by a kill is junk after an hour.
- **Profile mistakes:** `--profile matte-gpu` on a box without the toolkit → the sidecar stays up with `device: "unavailable"` and the UI shows AI off with that reason; both profiles up → the alternating-instance warning.
- **Discord:** the matte feeds the master before any encoder; the GIF tail (`#313338` matte, threshold 128, `-gifflags -offsetting`, mixed-frame complete-frames encode, `MergeGIFHolds`, lint/fix ladder, hold repair) and the WebP/APNG/AVIF tails are the same code — nothing about Discord rendering changes and `RulesVersion` stays. The only new Discord-visible property is a cleaner mask; the user-run upload matrix (DESIGN.md §9) is extended by one AI-matted emote and one sticker because an upload is the only authoritative check.

---

## 13. Phase plan

**Phase 5a — classical fixes (≈ 1 day, shippable alone, no new container)**
- graph: chromakey `yuv=1` + the rgba pass + the BT.601 limited-range format pin (`color_spaces=bt470bg:color_ranges=tv`, §5.4 as built) + BT.601 limited conversion; defaults 0.1/0.08; `morph` op; `detectOps` + `OpMorph`.
- recipe: `OpMorph`, `MorphParams`. jobs: `detectionOps` + morph; `PipelineVersion`, `stillMemoVersion`, `proxyMemoVersion`, `autocropKeyVersion` 4; fast-path test table + morph.
- SPA: four-entry segment (Colour first, Screen with its sub-choice), "+ add colour" rows, Edge cleanup fold, default mirrors; crop-mode still sends `{ format, fps }`.
- Tests: goldens for every chromakey/colorkey/morph filter text (`internal/graph`); `alpha_ffmpeg_test.go`/`phase3_ffmpeg_test.go` pixel tests: exact key colour keys at 0.02 (green, blue, 2 custom) from an RGB-decoded and from a bt709-tagged yuv420p source, morph close fills a 1-px hole and leaves the colour planes byte-identical, grow +1 adds one ring, 3 stacked colorkeys intersect; SPA unit tests for `buildOps` (N colorkey ops, morph, order) and the crop-mode/normal still fps equality; `scripts/integration-test.sh` chromakey/colorkey cases re-baselined.

**Phase 5b — sidecar + matte op (≈ 3–4 days, shippable; off without the profile)**
- sidecar: `sidecar/{Dockerfile,matte.py,png.py,models.json,tools/}`, both targets, `ThreadingHTTPServer`, `/v1/ping` (snapshot, `weights`/`processingVersion`/`freeGiB`/`lastError`/`busy`), `/v1/matte` (caps, timeouts), `/v1/warm`, `/v1/unload`, URL lists + backoff + Range resume + sha256 + the 10-min retry + `download` subcommand, derived fp16/512 graphs with digests, VRAM pre-check + self-test + `msPerFrame`, residency rule, TTL for every model, `gpu_mem_limit`, stay-up-and-report on a missing CUDA EP, RAM gate for lite on CPU, python healthcheck, `/models` ownership, LICENSES + NOTICE, hashed `==` requirements, request log line; pytest (framing, no cross-frame dependence, OOM mapping, ping < 50 ms during a batch, caps, digest reporting, derived-graph equivalence IoU ≥ 0.999 on a fixture); `docker-image.yml` matrix (app + matte-cpu + matte-cuda, `latest`/`<sha>`/release tags, Docker Hub), `test.yml` sidecar job; `.env.example`, `.gitignore` + `.env`.
- recipe: `OpMatte`, `MatteParams` + `MatteResolved`, model constants.
- graph: `ExtraInput.Matte` (+ `FPS` text, `Frames`), `Plan.Bounces`, `temporalPrefix`, `CompileMatteInput`, `CompileDetectFor`, `compiler.matte` (both merge shapes, `ensureRGBA` first), dedupe, `detectOps` + matte; goldens: opaque merge, alpha-carrying merge, `gbrap12le` source, matte+colorkey stack, matte+morph+feather, matte before an overlay (input indices), detection plan with a matte input, the stage-list prefix property + equal InputArgs for 6 stacks (video, sequence, AVIF alpha stream, yuva ProRes, FilterTrim WebP, bounce).
- enc: `extraInputArgsFor`, `stillSeek.slot` (forward `abs`, reversed K from the `(start, slots)` sibling of `reversedSeekStart`), `MatteSourceArgs`; goldens for Master/Still (forward single PNG without `-loop`, clamped slot)/StillFromStart/Proxy (forward, reversed CFR tail seek at 30 fps source / 25 fps plan → `-start_number` = aligned K+1, bounced, SeekUnsafe)/CropDetectPlan with a matte input.
- store: `MatteDir`, `MatteFrameDir`, the mattes sweep class (clip dirs, frames store, `.tmp-*` junk rule, dead-blob rule, size pass last) + `Protect`; `internal/matte` (stdlib client: `Ping`, `MatteBatch`, record parser, manifest, keys, persisted facts).
- jobs: `matte.go` (resolve from persisted facts, pass with its own ctx, batch writer with frames-store lookup and frame cap, pending/deferred/loading states, abandon-aware flight), the render pre-stage before `m.sem`, `compile` order, `fillExtraInputs` matte branch + fps check, count check (static skip, bounce divisor, memo kept), `StageMatte`, `ResolveAutoCropFor` with matte paths + `autocropKeyVersion` 5, `Resolved` fill in `Submit`/render, still/proxy keys fold in the clip key, `MatteStatus()` for `/api/matte`, probe with transitions-only logging and the 3-miss rule, env wiring (`EZLG_MATTE_URL`, `_MAX_SECONDS`, `_MAX_FRAMES` in `cmd/ezlg/main.go`'s table and `docs/USAGE.md`), `PipelineVersion` bump.
- server: capabilities `features.matte`; `GET /api/matte`; 202 mapping for still/proxy; `previewRequest.eager`; refuse `matte` ops when the sidecar is off (400 naming the profile).
- SPA: AI mode + Precise (+ warm), `/api/matte` polling, pending/deferred/loading pill + polling in `still.ts`/`proxy.ts`, `MattePending` in `api.ts`, estimate line, `render.matte` in the result card.
- Tests: jobs against an `httptest` fake sidecar (deterministic matte: 255 − luma) — memo hit/miss, **sidecar stopped + memo present → still served**, **sidecar `loading` + memo present → served**, **`loading` for 3 s then `ready` → the render succeeds without a client retry**, single-flight (10 concurrent stills → one pass), 202 after 2 s, deferred above the eager bound, abandon cancel, a POST failure kills ffmpeg at once and reports the POST error, `TestRenderMatteBouncedCount`, `TestRenderMatteStaticPNG`, count mismatch keeps the memo, frames-store hit (a trimmed re-request POSTs zero frames; a 3× fps-upsampled sequence POSTs N/3), run-time frame cap, reversed proxy alignment, **matte + autocrop resolves to the subject box**, **Concurrency = 1: an AI render blocked on a slow sidecar does not block a plain render**, weights change re-keys mattes, stills and `ResultKey`; ffmpeg pixel tests with gray PNG fixtures: opaque main → alpha == matte scaled; rgba main → multiply; the forward still's single frame equals master frame j's alpha (incl. a one-frame estimate overshoot), reversed/bounced/`[reverse, bounce]` stills and the reversed proxy against the master; a still at t ≥ 30 s at 50 fps in the same time band as a matte-less still; integration (`scripts/integration-test.sh` in the dev stack with the cpu sidecar): synthetic character over a gradient → GIF/WebP/APNG with a matte op → `discordlint` passes, autocrop resolves to the subject, the same recipe twice → cached, the sidecar stopped mid-test → the job error names it, no published port; the Discord test kit (`scripts/discord-testkit.sh`) gains one AI-matted emote and one sticker variant for the user's upload.

**Phase 5c — cards under 10 GiB and docs (≈ 1 day)**
- The fp16 lite graph if the pre-5b benchmark holds (`precision` keys the memo), the 3070 verdict wired into `/api/matte`, batch-mode estimate, docs: DESIGN.md §4.3 rows for `matte`/`morph`, §6 GPU verdict, §9 the user's verdict on the small-source and capture renders, §10 phase 5, USAGE.md profile instructions + the new env table + the `/models` ownership note, CLAUDE.md rules (never `planes=` on erosion/dilation; the matte identity enters the recipe hash through `Resolved`, never from the client; the matte memo's key never sees `PipelineVersion`; every new matte consumer in enc gets a golden).

---

## 14. Open questions for the user

1. **Model set:** AI = isnet-anime, Precise = BiRefNet-lite only? (u2netp dropped for mushy edges; BiRefNet-general/portrait for 4× the size at equal quality; Lucida/ToonOut/BEN2 for no measured edge or measured failures.)
2. **Pending UX:** 2 s wait before a preview answers "pending" (GPU users never see a pill) and a 90 s eager bound for stills — right, or should previews always answer at once / always start the pass?
3. **Default mode:** enabling the Background card lands on Colour (instant) with AI one click away — or should AI be the landing mode when the sidecar is up (one click fewer, but a GPU pass on enable)?
4. **"Fill pinholes" on by default** for every mode in new sessions (measured never to hurt): acceptable that enabling the card adds a `morph` op to the recipe?
5. **Caps and Precise on CPU:** 600 s / 3000 frames defaults, refusing rather than warning above them; and Precise **offered on CPU** whenever the self-test runs it (≥ 14 GiB RAM) and the clip's estimate is under the cap — a 2.8–7.5 min emote is a legitimate "go make coffee" job with its estimate shown — rather than GPU-only. Fine?
6. **The RTX 3070:** can you run the sidecar there before 5b closes — the Precise self-test at the default 6 GiB cap (5 GiB is already known to fail), and `/v1/ping`'s measured `msPerFrame` for AI and Precise, so the table in §1 stops saying "estimate"? Until then Precise may simply be unavailable on that box.
7. **Default changes:** chromakey 0.1 and colorkey 0.08 re-render every cached recipe that relied on the zero values (one `PipelineVersion` bump) — OK, or fix only the range bug and keep the old defaults?
8. **Test fixture:** may a few frames of the research corpus (your own Resolve export) be committed as a pixel-test fixture, or should tests stay with the synthetic gradient clip?

Decided inside the document rather than asked: derived graphs at first load (§7.3), memo on `/data` (§4.4), profiles `matte` / `matte-gpu` with `gpu` reserved (§8), Docker Hub `duckautomata/ezlg-matte` on the existing workflow and secrets (§8), the border wand "not now" (§10).
