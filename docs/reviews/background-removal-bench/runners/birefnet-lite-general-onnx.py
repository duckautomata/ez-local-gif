"""Runner: BiRefNet-lite, general use (swin_v1_tiny backbone), the rembg ONNX export.

This is the graph behind the proposal's "Precise" model. In the research run
the rows results/birefnet-lite-general-onnx* were produced by the
BiRefNet-portrait runner with its model path pointed at this file
(BIREFNET_PORTRAIT_ONNX=.../BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx);
this runner is that code path with the lite file as its default, so it
reproduces those rows exactly (same pre/post-processing, same session options).

Weights (no remote code, no trust_remote_code, onnxruntime only):
  https://github.com/danielgatis/rembg/releases/download/v0.0.0/BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx
  224,005,088 bytes, md5 4fab47adc4ff364be1713e97b7e66334 (rembg's pinned
  checksum for its 'birefnet-general-lite' session), sha256
  5600024376f572a557870a5eb0afb1e5961636bef4e1e22132025467d0f03333.
  Upstream: ZhengPeng7/BiRefNet, BiRefNet_lite (general-use training set,
  swin_v1_tiny backbone, epoch 232). Licence: MIT (ZhengPeng7/BiRefNet
  LICENSE); rembg itself is MIT.
  Expected at $EZLG_BENCH_MODELS/BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx
  (default: ../models next to this benchmark; ../models.json lists the file,
  ../tools/fetch-models.sh downloads it). The md5 is verified before the
  session is built when the default path is used.

Graph (read in load(): input/output names and dtype come from the file):
fixed batch 1, float32 [1,3,1024,1024] input, float32 [1,1,1024,1024] output.
The final node is a Conv, so the output is LOGITS -> the runner applies a
sigmoid. DeformConv is decomposed, so the plain CUDAExecutionProvider runs the
whole graph.

Pre-processing = the authors' inference transform: PIL bilinear resize to
1024x1024, /255, ImageNet mean/std, NCHW. Post: sigmoid, PIL bilinear resize
(mode F) back to the frame size, clip 0..1. NO per-frame min-max normalisation.

VRAM: ORT's CUDA arena grows without bound on this graph unless capped (14.5 GB
uncapped on the RTX 5080). The research's cap sweep: a 6 GiB `gpu_mem_limit`
holds a stable ~6.3 GB process footprint, 7 GiB (this runner's default, what the
main rows were measured with) ~7.4 GB; 5, 4 and 3 GiB FAIL with an
ONNXRuntimeError (the fp32 graph needs one large contiguous allocation), so the
cap is a knob for larger cards, never a remedy for smaller ones. Session
create is 10-20 s on the 5080. On CPU the same graph took 3.7-10 s/frame on a
Ryzen 7 7800X3D (12.6 GB RSS).

A side file with the pure sess.run() wall times is written at exit to
<results>/_timing/sess_run_timing_<model>_<device>.json (the harness times
infer() end to end; results = $EZLG_BENCH_RESULTS or ../results).

Env overrides:
  BIREFNET_LITE_ONNX        path to the .onnx (default: the rembg file above)
  BIREFNET_SIZE             square input size (default: from the graph, else 1024)
  BIREFNET_GPU_MEM_LIMIT    CUDA arena cap in bytes (default 7 GiB; 0 = uncapped)
  BIREFNET_SHRINK=1         add run option memory.enable_memory_arena_shrinkage
"""
from __future__ import annotations

import atexit
import hashlib
import json
import os
import time

import numpy as np
from PIL import Image

NAME = "birefnet-lite-general-onnx"
BATCH = 1  # the graph has a fixed batch dimension of 1

_HERE = os.path.dirname(os.path.abspath(__file__))
_BENCH = os.path.dirname(_HERE)
MODELS_DIR = os.environ.get("EZLG_BENCH_MODELS") or os.path.join(_BENCH, "models")
RESULTS_DIR = os.environ.get("EZLG_BENCH_RESULTS") or os.path.join(_BENCH, "results")
MODEL_FILE = "BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx"
MODEL_URL = "https://github.com/danielgatis/rembg/releases/download/v0.0.0/" + MODEL_FILE
MODEL_MD5 = "4fab47adc4ff364be1713e97b7e66334"
DEFAULT_MODEL = os.path.join(MODELS_DIR, MODEL_FILE)
MODEL = os.environ.get("BIREFNET_LITE_ONNX", DEFAULT_MODEL)

MEAN = np.array([0.485, 0.456, 0.406], dtype=np.float32)
STD = np.array([0.229, 0.224, 0.225], dtype=np.float32)


def _md5(path: str) -> str:
    h = hashlib.md5()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load(device: str):
    import onnxruntime as ort

    if not os.path.isfile(MODEL):
        raise FileNotFoundError(f"{MODEL} missing; download {MODEL_URL} into {MODELS_DIR} (md5 {MODEL_MD5}; see models.json / tools/fetch-models.sh)")
    if MODEL == DEFAULT_MODEL:
        got = _md5(MODEL)
        assert got == MODEL_MD5, f"{MODEL_FILE} md5 {got} != {MODEL_MD5}"

    if device.startswith("cuda"):
        # The CUDA 13 / cuDNN 9 DLLs (nvidia-* wheels of onnxruntime-gpu[cuda,cudnn], or
        # torch/lib) must be loaded before the session is created, otherwise ORT
        # silently falls back to CPU.
        if hasattr(ort, "preload_dlls"):
            ort.preload_dlls()
        dev_id = int(device.split(":", 1)[1]) if ":" in device else 0
        cuda_opts = {"device_id": dev_id}
        limit = int(os.environ.get("BIREFNET_GPU_MEM_LIMIT", str(7 * 2**30)))
        if limit > 0:
            cuda_opts["gpu_mem_limit"] = str(limit)  # see the VRAM note in the docstring
        providers = [("CUDAExecutionProvider", cuda_opts), "CPUExecutionProvider"]
    else:
        dev_id = 0
        cuda_opts = None
        providers = ["CPUExecutionProvider"]

    so = ort.SessionOptions()
    so.log_severity_level = 3  # errors only (the "no plugin EP device" warning is noise)
    ro = ort.RunOptions()
    if os.environ.get("BIREFNET_SHRINK") == "1" and device.startswith("cuda"):
        ro.add_run_config_entry("memory.enable_memory_arena_shrinkage", f"gpu:{dev_id}")
    t0 = time.perf_counter()
    sess = ort.InferenceSession(MODEL, so, providers=providers)
    create_ms = (time.perf_counter() - t0) * 1000.0
    active = sess.get_providers()
    if device.startswith("cuda"):
        assert active[0] == "CUDAExecutionProvider", f"CUDA EP not active, got {active}"
    else:
        assert active[0] == "CPUExecutionProvider", active

    inp = sess.get_inputs()[0]
    out = sess.get_outputs()[0]
    size = int(os.environ.get("BIREFNET_SIZE", "0")) or (inp.shape[-1] if isinstance(inp.shape[-1], int) else 1024)
    in_dtype = np.float16 if "float16" in inp.type else np.float32
    ctx = {
        "sess": sess,
        "run_options": ro,
        "cuda_options": cuda_opts,
        "input": inp.name,
        "output": out.name,
        "size": int(size),
        "in_dtype": in_dtype,
        "providers": active,
        "model": MODEL,
        "device": device,
        "session_create_ms": create_ms,
        "run_ms": [],  # pure sess.run() wall times, one per frame (incl. warm-up)
    }
    print(f"[{NAME}] model={os.path.basename(MODEL)} providers={active} cuda_options={cuda_opts} input={inp.name}{inp.shape} {inp.type} "
          f"output={out.name}{out.shape} size={size} session_create={create_ms:.0f}ms")
    atexit.register(_dump_timing, ctx)
    return ctx


def _dump_timing(ctx):
    """Side file with the pure sess.run() timings (the harness times infer() end to end)."""
    try:
        r = np.asarray(ctx["run_ms"], dtype=np.float64)
        if r.size == 0:
            return
        steady = r[3:] if r.size > 3 else r
        out = {
            "model": os.path.basename(ctx["model"]), "device": ctx["device"], "providers": ctx["providers"], "cuda_options": ctx["cuda_options"],
            "session_create_ms": ctx["session_create_ms"], "frames_timed": int(r.size),
            "sess_run_ms_mean_all": float(r.mean()), "sess_run_ms_median_all": float(np.median(r)),
            "sess_run_ms_mean_after_first3": float(steady.mean()), "sess_run_ms_min": float(r.min()), "sess_run_ms_max": float(r.max()),
        }
        d = os.path.join(RESULTS_DIR, "_timing")
        os.makedirs(d, exist_ok=True)
        tag = os.path.splitext(os.path.basename(ctx["model"]))[0] + "_" + ctx["device"].replace(":", "")
        with open(os.path.join(d, f"sess_run_timing_{tag}.json"), "w", encoding="utf-8") as f:
            json.dump(out, f, indent=1)
    except Exception:
        pass


def _prep(frame: np.ndarray, size: int, dtype) -> np.ndarray:
    im = Image.fromarray(frame, "RGB")
    if im.size != (size, size):
        im = im.resize((size, size), Image.BILINEAR)  # torchvision Resize on a PIL image == this
    x = np.asarray(im, dtype=np.float32) / 255.0
    x = (x - MEAN) / STD
    return np.ascontiguousarray(x.transpose(2, 0, 1)[None]).astype(dtype, copy=False)


def _post(logits: np.ndarray, h: int, w: int) -> np.ndarray:
    m = logits.astype(np.float32)
    m = 1.0 / (1.0 + np.exp(-m))  # final node is a Conv -> logits
    if m.shape != (h, w):
        m = np.asarray(Image.fromarray(m, "F").resize((w, h), Image.BILINEAR), dtype=np.float32)
    return np.clip(m, 0.0, 1.0)


def infer(ctx, frames):
    sess, name, out, size, dt, ro = ctx["sess"], ctx["input"], ctx["output"], ctx["size"], ctx["in_dtype"], ctx["run_options"]
    res = []
    for f in frames:
        h, w = f.shape[:2]
        x = _prep(f, size, dt)
        t0 = time.perf_counter()
        y = sess.run([out], {name: x}, ro)[0]
        ctx["run_ms"].append((time.perf_counter() - t0) * 1000.0)
        res.append(_post(y[0, 0], h, w))
    return res


def reset(ctx):
    pass  # stateless per-frame model
