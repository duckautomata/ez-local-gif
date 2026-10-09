# Background-removal benchmark (2026-10-08)

The benchmark behind [`docs/background-removal-proposal.md`](../../background-removal-proposal.md):
a ground-truth corpus built from the user's own DaVinci Resolve alpha export,
an AI harness that scores ONNX background-removal models frame by frame, and
the classical ffmpeg / numpy grids (colour keys, plate difference, magic wand,
post-processing). Everything here is a script or a document; the corpus, the
model weights and the results are built or downloaded locally and ignored by
git (`.gitignore` in this directory).

The numbers in the proposal were measured on an **NVIDIA GeForce RTX 5080 /
AMD Ryzen 7 7800X3D (8C/16T), Windows 11**, Python 3.12.10, onnxruntime-gpu
1.30.0 (CUDA 13 / cuDNN 9), FFmpeg git 2026-08-17 (a 9.x build). CPU rows used
the same wheel's CPUExecutionProvider. Other hardware gives other timings; the
quality metrics only depend on the corpus and the model files.

## Layout

| path | what |
|---|---|
| `make-corpus.sh`, `CORPUS.md` | corpus builder and its description (backgrounds, metric definitions) |
| `harness.py`, `metrics.py`, `RUNNER_PROTOCOL.md` | the AI harness: runs a *runner* over every corpus variant, times it, saves mattes, scores them |
| `runners/` | `dummy.py` (constant 0.5 floor), `numpy_colorkey.py` (naive RGB key floor), `u2netp-onnx.py`, `isnet-anime-onnx.py`, `birefnet-lite-general-onnx.py` |
| `models.json`, `tools/fetch-models.sh` | the three model files: URL, size, md5, sha256, licence; the script downloads and verifies them |
| `tools/to_fp16.py`, `tools/rescale_graph.py` | derive the fp16 and the 512x512 isnet-anime graphs (the runner calls them on first use) |
| `classical/` | the ffmpeg / numpy grids: `bench_lib.py`, `grid.py`, `plates.py`, `run_m1.py` .. `run_m8.py`, `run_m5b.py`, `timing.py`, `real.py`, `sheet.py`, `report.py`, and their own `metrics.py` |

Every script reads its locations from the environment, with defaults next to
this README:

| variable | default | used by |
|---|---|---|
| `EZLG_BENCH_CORPUS` | `./corpus` | harness (`--corpus` overrides), classical |
| `EZLG_BENCH_RESULTS` | `./results` | harness (`--results` overrides), classical (writes under `<results>/classical/`), the lite runner's timing side file |
| `EZLG_BENCH_MODELS` | `./models` | the three ONNX runners, `tools/fetch-models.sh` |
| `EZLG_BENCH_SRC`, `EZLG_BENCH_SRC2` | `<repo>/output/resolve/example-{1,2}.mov` | `make-corpus.sh` |
| `FFMPEG` | `ffmpeg` on `PATH` | `make-corpus.sh`, classical |

## Prerequisites

* Python 3.12 in a venv. CPU:

  ```
  python -m venv .venv
  .venv/Scripts/pip install numpy pillow scipy opencv-python-headless onnx onnxruntime   # Windows
  .venv/bin/pip install numpy pillow scipy opencv-python-headless onnx onnxruntime       # Linux
  ```

  GPU (what the proposal's CUDA rows used): replace `onnxruntime` with
  `"onnxruntime-gpu[cuda,cudnn]==1.30.*"` — the extra pulls the CUDA 13 /
  cuDNN 9 wheels, no system CUDA needed; the NVIDIA driver must be >= 580.
  `psutil` is optional (peak-RSS column on Windows). `onnx` is only needed to
  derive the fp16 / 512 isnet graphs.
* **ffmpeg >= 9** on `PATH` (or `FFMPEG=/path/to/ffmpeg`), a full build:
  `make-corpus.sh` needs the ProRes 4444 decoder, `gradients`, `testsrc2`,
  `noise`, `palettegen`/`paletteuse`, libx264; the classical grids need
  `chromakey`, `colorkey`, `hsvkey`, `backgroundkey`, `blend`, `erosion`,
  `dilation`, `median`, `tmedian`, `hqdn3d`, `nlmeans`, `removegrain`, `gblur`.
  On the dev host another ffmpeg shadows the FFmpeg 9 build on `PATH`; prefix it:
  `export PATH="/c/Programs/bin/ffmpeg-master-latest-win64-gpl/bin:$PATH"`.
* bash for the two `.sh` scripts (Git Bash on Windows), `curl` + `sha256sum`
  for `tools/fetch-models.sh`.

Run every Python script with `python -I` (isolated mode: no user site, no
`PYTHON*` env, script directory not on `sys.path` — the scripts put what they
need on `sys.path` themselves). All commands below are given from the repo
root with `B=docs/reviews/background-removal-bench`.

## 1. Build the corpus from a Resolve alpha export

The source is a ProRes 4444 export with **binary** alpha of an illustrated
character, 720x720; the subject is the window t = 2..5 s (45 frames at 15 fps).
The research used the user's `output/resolve/example-1.mov` (and
`example-2.mov`, a real stream capture, for the no-ground-truth case).
Another clip is another ground truth — its numbers are not comparable with the
proposal's.

```
export EZLG_BENCH_SRC=/path/to/example-1.mov      # default: <repo>/output/resolve/example-1.mov
export EZLG_BENCH_SRC2=/path/to/example-2.mov     # optional; the real/ex2 case is skipped when missing
bash $B/make-corpus.sh                            # -> $B/corpus  (or: make-corpus.sh /other/dir)
```

Takes ~10 s to a few minutes depending on the disk (every background decodes
3 s of the ProRes clip). Eight backgrounds rebuild bit-identically; `anim`
uses the `gradients` filter's random endpoints (no seed in the definition), so
it differs per build — see `CORPUS.md`. Output:
`corpus/gt/`, `corpus/subject/rgba/`, `corpus/bg/<name>/{png,<name>.mp4}`,
`corpus/bg/{vgrad,multi,skin,noise}/{<name>.gif,gifpng}`, `corpus/real/ex2/`.
`CORPUS.md` explains every variant and the metrics.

## 2. Download the three model files

```
bash $B/tools/fetch-models.sh      # -> $B/models (or $EZLG_BENCH_MODELS); verifies sha256; ~405 MB
```

or fetch them by hand from the rembg release `v0.0.0` and check them against
`models.json`:

| file | bytes | sha256 | licence | runner |
|---|---:|---|---|---|
| `u2netp.onnx` | 4,574,861 | `309c8469…76f4ddd8` | Apache-2.0 | `runners/u2netp-onnx.py` |
| `isnet-anime.onnx` | 176,069,933 | `f15622d8…31a6e99` | Apache-2.0 | `runners/isnet-anime-onnx.py` (the proposal's default **AI** model) |
| `BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx` | 224,005,088 | `56000243…d0f03333` | MIT | `runners/birefnet-lite-general-onnx.py` (the proposal's **Precise** model) |

The runners verify the checksum of their default file before building a
session. The fp16 and 512x512 isnet graphs are derived next to the source file
on first use (`tools/to_fp16.py`, `tools/rescale_graph.py`; their sha256 are in
`models.json` too).

## 3. Run the AI harness

One run = one runner over every corpus variant (9 backgrounds, 4 GIF-dithered
decodes, the real capture), with a 3-frame warm-up, timing, mattes, previews
and metrics. `RUNNER_PROTOCOL.md` documents the runner interface and every
column.

```
PY=.venv/Scripts/python.exe        # .venv/bin/python on Linux
export EZLG_BENCH_CORPUS=$B/corpus EZLG_BENCH_RESULTS=$B/results EZLG_BENCH_MODELS=$B/models   # the defaults; shown for clarity

# floors
$PY -I $B/harness.py --runner $B/runners/dummy.py          --name dummy          --device cpu
$PY -I $B/harness.py --runner $B/runners/numpy_colorkey.py --name numpy_colorkey --device cpu

# u2netp (320x320, fp32)
$PY -I $B/harness.py --runner $B/runners/u2netp-onnx.py --name u2netp-onnx     --device cuda
$PY -I $B/harness.py --runner $B/runners/u2netp-onnx.py --name u2netp-onnx-cpu --device cpu

# isnet-anime: GPU = fp16 at 1024x1024 (the default on cuda); CPU = fp32 on the 512x512 graph
$PY -I $B/harness.py --runner $B/runners/isnet-anime-onnx.py --name isnet-anime-onnx --device cuda
ISNET_ANIME_SIZE=512 $PY -I $B/harness.py --runner $B/runners/isnet-anime-onnx.py --name isnet-anime-onnx-512-cpu --device cpu
ISNET_ANIME_PRECISION=fp32 $PY -I $B/harness.py --runner $B/runners/isnet-anime-onnx.py --name isnet-anime-onnx-fp32 --device cuda --variants vgrad busy --limit 10   # fp16 vs fp32 check

# BiRefNet-lite (1024x1024 fp32; 7 GiB arena cap by default, see the runner docstring for the cap sweep)
$PY -I $B/harness.py --runner $B/runners/birefnet-lite-general-onnx.py --name birefnet-lite-general-onnx --device cuda
BIREFNET_GPU_MEM_LIMIT=$((6<<30)) $PY -I $B/harness.py --runner $B/runners/birefnet-lite-general-onnx.py --name birefnet-lite-general-onnx-cap6g --device cuda --variants vgrad
$PY -I $B/harness.py --runner $B/runners/birefnet-lite-general-onnx.py --name birefnet-lite-general-onnx-cpu5 --device cpu --variants vgrad --limit 5   # minutes: 3.7-10 s/frame
```

`--variants` takes names (`green`, `vgrad-gifpng`, `ex2-real`, …) or frame
directories; `--limit N` scores the first N frames only; `--device cpu` works
for every runner. A quick smoke of the whole pipeline:

```
$PY -I $B/harness.py --runner $B/runners/numpy_colorkey.py --name smoke --device cpu --limit 5
```

Results land under `$EZLG_BENCH_RESULTS/<name>/`: `metrics.json` (everything,
plus `overall`, `overall_png`, `overall_gifpng` means), `summary.md` (one
table), and per variant `001.png..045.png` (8-bit gray mattes),
`preview_020.png` (original | matte | subject over checkerboard) and
`metrics.json` (per-frame values). The lite runner also writes
`<results>/_timing/sess_run_timing_<model>_<device>.json` with pure
`sess.run()` times. Score mattes made some other way with
`$PY -I $B/metrics.py --gt $B/corpus/gt --pred DIR`.

In the research the `birefnet-lite-general-onnx*` rows were produced through
the BiRefNet-portrait runner with its model path pointed at the lite file;
`runners/birefnet-lite-general-onnx.py` is that code path with the lite file
as its default, so it reproduces them. The other research runners (ben2,
birefnet-general/-portrait, lucida, toonout, rvm) are not packaged — their
models were rejected on licence, size or quality.

## 4. Run the classical grids

Each `run_m*.py` is an *oracle* grid: for every corpus variant it runs every
parameter combination through ffmpeg (or numpy), scores each against the
ground truth, keeps the best by MAE and the single default setting, saves both
matte sets and writes `<method>.json` with every grid point. `SMOKE=1` shrinks
the grid to a few points and skips saving mattes. A second argument restricts
the variants (`vgrad,multi`).

```
export EZLG_BENCH_CORPUS=$B/corpus EZLG_BENCH_RESULTS=$B/results           # defaults
export PATH="/c/Programs/bin/ffmpeg-master-latest-win64-gpl/bin:$PATH"     # dev host only: the FFmpeg 9 build

$PY -I $B/classical/run_m1.py both            # M1 chromakey + colorkey, corner-median key colour   (args: both|chroma|color [variants])
$PY -I $B/classical/run_m2.py both            # M2 stacked multi-colour keys, ring k-means colours  (both|color|chroma [variants])
$PY -I $B/classical/run_m3.py                 # M3 hsvkey                                           ([variants])
$PY -I $B/classical/run_m4.py                 # M4 backgroundkey as shipped                         ([variants])
$PY -I $B/classical/plates.py                 # background plates for M5 (median45, first, coons, laplace); then axis1d:
$PY -I -c "import sys; sys.path.insert(0, '$B/classical'); import plates; plates.make_axis1d()"
$PY -I $B/classical/run_m5.py both            # M5 plate difference key, rgbmax + yuvsum            (both|rgbmax|yuvsum [variants])
$PY -I $B/classical/run_m5b.py                # M5b yuvsum key on the synthetic axis1d / coons plates
$PY -I $B/classical/run_m6.py all             # M6 border-connected magic wand (numpy)              (all|seed|grow|hybrid [variants])
$PY -I $B/classical/run_m7.py colorkey        # M7 post-processing of a method's saved best mattes  (<method> [variants]; after that method ran unsmoked)
$PY -I $B/classical/run_m8.py                 # M8 source denoising before the key (noise + dithered variants by default)
$PY -I $B/classical/timing.py vgrad           # serial wall-time pass, one setting per method, median of 3
$PY -I $B/classical/real.py                   # the real capture: mattes + contact sheet (no GT)
$PY -I $B/classical/sheet.py vgrad,multi,skin # visual sheet of the oracle-best mattes over Discord dark
$PY -I $B/classical/report.py                 # markdown tables from every <method>.json; add --robust for one setting per method
```

Tiny smoke of the machinery (seconds):

```
SMOKE=1 $PY -I $B/classical/run_m1.py color vgrad
```

Everything lands under `$EZLG_BENCH_RESULTS/classical/`: `<method>/<variant>/`
and `<method>-default/<variant>/` (mattes), `<method>.json`, `_plates/`,
`_inputs/noise-mp4/` (the noise mp4 decoded back to PNG, made on first use),
`timing-<variant>.json`, `real-ex2/`, `_sheet_<variant>.png`, `summary.json`,
`robust_defaults.json`. The classical scorer is `classical/metrics.py`: same
MAE / IoU / flicker as the harness's `metrics.py`, but its 5 px boundary band
is a square 11x11 element (the harness uses a disk) and it adds `gif_*`
metrics (prediction binarised at >= 128, what a GIF shows); the two boundary
columns are therefore not strictly comparable across the AI and classical
tables.

## What is not here

The research's full result trees, logs, per-model scratch probes (VRAM cap
sweeps, graph inspection) and the rejected models' runners stayed in the
session scratchpad; the proposal records the conclusions. Re-running the
commands above reproduces the quality numbers exactly (same corpus source,
same weights, same code paths) and the timings for the host they run on.
