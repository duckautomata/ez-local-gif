# Background removal, part 2 — stability on video and guided segmentation (2026-10-09)

Addendum to [background-removal-research-2026-10-08.md](background-removal-research-2026-10-08.md). After Phase 5b
shipped, the user reported on real footage: the per-frame models (isnet-anime and BiRefNet-lite alike) "flicker in and
out over the whole video, remove parts I want kept and keep parts I want removed", and that a model must not stay
loaded when unused. Three measurements followed, on the same workstation (RTX 5080, host FFmpeg git 2026-08).

## 1. The pipeline pairs frames correctly

Suspect first: a one-frame drift between the matte sequence and the master would look exactly like flicker on a
moving subject. Test: the integration test's orbiting cartoon figure (160×160, 60 frames at 30 fps, exact ground truth
from its flat colours) rendered through the real app and sidecar to a lossless WebP.

| model | IoU mean (min) | matte centroid − figure centroid | best IoU at time shift | binary flicker vs GT motion |
|---|---|---|---|---|
| BiRefNet-lite | 0.980 (0.962) | 0.00 / −0.02 px (max 0.68 px) | **0 frames** (−1: 0.84, +1: 0.84) | 0.0216 vs 0.0231 |
| isnet-anime | 0.000 | — | — | — (detects nothing on a non-anime figure) |

So the frames are paired exactly and BiRefNet-lite is stable on a moving flat-colour subject. What the user sees is
model behaviour on their content, which the corpus (one character over synthetic backgrounds) does not reproduce.

## 2. Temporal stabilisation of a matte sequence

Candidates applied with ffmpeg to the saved mattes (gray PNG sequences), scored against the corpus ground truth and on
the real stream-UI capture; a sceptic re-derived every decision number independently.

| chain (gray image2 in → out) | what it does | verdict |
|---|---|---|
| `tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1` | centred 3-frame median, exactly N frames, ends pass through | **light, default on**: every 1-frame pop removed, zero lag (0.02 frame at 3 px/frame); isnet worst-frame IoU 0.935 → 0.986, flip-flopping pixels −9 k per clip; lite near-neutral (IoU −0.0005) |
| light + `lagfun=decay=0.7` | then a decay hold: an opaque pixel stays ≥ 128 one extra frame | **strong, opt-in**: lost-subject pixels −15 %, kept-background ×1.8, 0.5-frame trail on moving edges (−0.07 IoU at 3 px/frame) |
| median 5 | | rejected: kills 2-frame real events, no flicker gain over median 3 |
| `tmix` 1-2-1 | a blur, not a vote | rejected: half the effect, soft edges smeared |
| hold only (0.5 / 0.7 / 0.85 / 0.9) | | rejected: 0.5 is a no-op, ≥ 0.85 glues background to trailing edges (1.7–2.5-frame trails) |
| temporal max / 2-of-5 | | rejected: 1-frame ghosts on both sides of every moving edge |
| hysteresis (numpy, two thresholds) | | rejected: no better than the median, binary output, needs the sidecar |
| union max(isnet, lite) | | rejected: inherits both models' false positives, IoU 0.9954 vs lite 0.9970, both inferences |
| alpha gain ×1.25 / threshold 96 | | rejected as a filter; the existing Output alpha-threshold slider already is the "keep more" knob (96 costs IoU −0.0003) |

What a temporal filter cannot fix (measured): a part missing for 3+ consecutive frames, the anime model's mid-alpha
UI buttons on the capture (held steady instead of blinking), anything the model never saw as subject.

## 3. Guided segmentation with SAM 2.1 (prompt + temporal memory)

SAM 2.1 hiera-tiny (Apache-2.0 code and weights; torch 2.14.1+cu130; repo pinned at commit
2b90b9f5ceec907a1c18123530e92e794ad901a4; checkpoint sha256 7402e0d8…be69), bf16 on the RTX 5080.

| prompt (frame 0) | corpus vgrad-gif IoU (min) | flicker | moving figure IoU | real capture: UI buttons |
|---|---|---|---|---|
| one positive click at the subject's centre | 0.24 | — | 0.55 | the pendant only |
| three positive clicks | 0.15 | — | — | — |
| **box around the subject** | **0.995 (0.994)** | −0.0002 | **0.983** | removed (region 0.089, constant) |
| box 10 % too loose | 0.962 (0.896) | +0.005 | 0.982 | — |
| **mask of frame 0** (the per-frame model's own matte) | **0.997** | −0.0001 | 0.966 | removed |
| reference: BiRefNet-lite per frame | 0.998 | +0.0001 | 0.980 | bleeds in (0.111, 25 of 45 frames ≥ 10 %) |
| reference: isnet-anime per frame | 0.998 | +0.0012 | 0.000 | pops (0.224, up to 0.98) |

Facts for the design: a single click selects a part of an illustrated character and a click just outside the subject
floods the frame, so the UI leads with a box (or 2–3 clicks) and shows the prompted frame's mask live; − clicks remove.
Corrections on later frames stick only with `add_all_frames_to_correct_as_cond=true`. SAM 2's edges are coarser
(boundary MAE 0.046 vs 0.017 for BiRefNet-lite: 256² decoder logits), so the final matte gates the per-frame model:
alpha = 255 inside erode(mask, 3 px), the per-frame matte inside the band, 0 outside dilate(mask, 3 px) — equal to
BiRefNet-lite's edges on the corpus with the UI removed and the temporal error bounded. Cost: ~30 ms/frame propagation
+ ~30 ms/frame frame prep at 720², ~1.2 GB VRAM for 45 frames, model load 0.3–0.6 s warm. Packaging: a CUDA image with
torch + onnxruntime sharing the NVIDIA wheels is ≈ 6 GB, ≈ 4.4 GB after pruning unused CUDA libraries (today's ORT-only
image is 7.5 GB); the CPU image grows by ≈ 1 GB. SAM 3 / 3.1 are not shipped (gated, custom non-OSI licence, 3.5 GB);
EdgeTAM (Apache-2.0, 56 MB, 3.6× faster on CPU, validated ONNX video export) is the candidate for a CPU tier later.

## 4. What changed in the app (Phase 5c)

- No model stays resident: the sidecar self-tests at start and releases its sessions, unloads after 300 s idle and
  when the user leaves the AI mode; one process offers GPU and CPU; the user picks "Run on" in the card.
- Nothing runs until **Compute matte** (or a render); previews answer "not computed" until then.
- Default model on the GPU: General (BiRefNet-lite); Stabilise = Light by default; Keep colours force dropped parts
  back in; the Guided model adds the box/click panel described above.
