# Runner protocol for `harness.py`

A **runner** is one Python file. The harness imports it by path, so it can live
anywhere (`runners/*.py` here). Its own directory is put on `sys.path`, so
sibling helper modules import normally.

```python
NAME  = "my_method"      # optional, informational
BATCH = 4                # optional, frames per infer() call (default 1)

def load(device: str):
    """device is "cuda", "cuda:N" or "cpu". Build the model/session, move it to
    the device, return anything (the ctx). Called once per harness run."""

def infer(ctx, frames):
    """frames: list of HxWx3 uint8 RGB numpy arrays (len <= BATCH, the last batch
    of a sequence may be shorter; all frames of one call have the same H, W).
    Return a list of the same length of HxW float32 arrays, alpha in 0..1
    (1 = subject), same H and W as the input frame. Any resizing to the model's
    input size and back is the runner's job."""

def reset(ctx):            # optional
    """Called before each frame sequence (and again after the warm-up). Clear
    recurrent / temporal state here (RVM-style models, background estimators)."""
```

Rules

* `infer()` must be synchronous: when it returns, the mattes are on the host.
  The harness additionally calls `torch.cuda.synchronize()` around the timed
  pass when torch is importable and `--device cuda`.
* A uint8 matte (0..255) is accepted and divided by 255; a HxWx1 or 1xHxW array
  is squeezed; a wrong size is resized with a warning (counted in the result).
  Values are clipped to 0..1.
* The frames of one variant are fed in order 001..045, so a runner may use
  temporal context inside a batch; `BATCH=45` (or more) gives the whole clip in
  one call. Prefer a `BATCH` that fits the 8 GB RTX 3070 target.
* Do not print per-frame output (it skews timing); warnings are fine.
* Model files live under `$EZLG_BENCH_MODELS` (default `./models` next to the
  harness); `models.json` lists every file with its download URL and checksums
  and `tools/fetch-models.sh` downloads them. A runner verifies the checksum of
  its default file before building the session. Prefer ONNX + onnxruntime, no
  remote code; if `trust_remote_code=True` is unavoidable, pin the revision and
  say so in the runner docstring. Never apply per-image min-max normalisation
  to a model's output by default (it is a per-frame contrast hack = flicker).
* **onnxruntime-gpu runners**: call `onnxruntime.preload_dlls()` in `load()`
  *before* creating the `InferenceSession` (it loads the CUDA 13 / cuDNN 9 DLLs
  from the `nvidia-*` wheels that `onnxruntime-gpu[cuda,cudnn]` installs, or
  from `torch/lib` when torch is present), and assert
  `session.get_providers()[0] == "CUDAExecutionProvider"` (ORT silently falls
  back to CPU otherwise). Map `device == "cpu"` to `["CPUExecutionProvider"]`.
* Run the harness with the venv interpreter and `-I`
  (`<venv>/Scripts/python.exe -I harness.py ...` on Windows,
  `<venv>/bin/python -I harness.py ...` elsewhere): the harness puts its own
  directory and the runner's directory on `sys.path` itself.

Harness CLI

```
python -I harness.py --runner PATH --name NAME --device cuda|cpu
                     [--variants green vgrad-gifpng ex2-real ...] [--limit N]
                     [--batch B] [--corpus DIR] [--results DIR] [--no-warmup]
```

`--corpus` / `--results` default to `$EZLG_BENCH_CORPUS` / `$EZLG_BENCH_RESULTS`,
else `./corpus` / `./results` next to the harness.

Variant names: `<bg>` for `corpus/bg/<bg>/png` (anim black busy green multi
noise skin vgrad white), `<bg>-gifpng` for `corpus/bg/<bg>/gifpng` (vgrad multi
skin noise), `ex2-real` for `corpus/real/ex2/png` (256x256, no GT). A directory
path is accepted as well. `--limit N` takes the first N frames of each variant
(the preview then shows the last available frame if 020 is cut off).

What the harness measures, per variant

* warm-up: `infer()` on the first 3 frames (discarded), then `reset()` if any
* `ms_per_frame`: wall time of one pass over all frames in batches of BATCH,
  divided by the frame count (includes pre/post-processing and host<->device
  copies inside `infer()`, excludes PNG decode/encode)
* `peak_vram_mb_torch`: `torch.cuda.max_memory_allocated()` after a reset
  before the timed pass (torch allocations only: 0/None for ORT-only runners)
* `nvidia_smi_used_mb`: `nvidia-smi --query-gpu=memory.used` sampled right
  after the timed pass (whole GPU, includes other processes; the baseline before
  the runner loaded is in `nvidia_smi_used_mb_baseline`)
* `peak_rss_mb`: `psutil` peak working set (Windows) / `ru_maxrss` (POSIX);
  `None` without psutil on Windows
* metrics (`metrics.py`, when the variant has GT): `mae`, `iou` (@0.5, both
  sides thresholded at 128), `boundary_mae` (5 px band of the GT edge),
  `flicker` (pred temporal MAE minus GT temporal MAE)

Outputs under `results/NAME/`

* `<variant>/001.png .. 045.png` 8-bit gray mattes (what the metrics read back)
* `<variant>/preview_020.png` frame 020: original | matte | subject over checkerboard
* `<variant>/metrics.json` per-frame values (GT variants only)
* `metrics.json` everything, plus `overall` (mean over all GT variants),
  `overall_png`, `overall_gifpng`
* `summary.md` one table

Metrics CLI (for mattes produced some other way, e.g. by ffmpeg):

```
python -I metrics.py --gt corpus/gt --pred results/NAME/vgrad [--json out.json] [--limit N]
```

Predictions may be L / LA / RGB / RGBA (alpha channel if present, else the first
channel) and are resized to the GT size with a warning if they differ.
