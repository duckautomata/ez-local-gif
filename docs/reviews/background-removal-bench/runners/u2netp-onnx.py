"""Runner: u2netp (tiny U^2-Net, the rembg ONNX export) via onnxruntime.

Weights: https://github.com/danielgatis/rembg/releases/download/v0.0.0/u2netp.onnx
(4,574,861 bytes), pinned by checksum: md5 8e83ca70e441ab06c318d82300c84806 (the
value rembg itself verifies in rembg/sessions/u2netp.py), sha256
309c8469258dda742793dce0ebea8e6dd393174f89934733ecc8b14c76f4ddd8. The file is
verified before the session is built. Expected at
$EZLG_BENCH_MODELS/u2netp.onnx (default: ../models next to this benchmark;
override with U2NETP_MODEL=path). No trust_remote_code and no HF: only the ONNX
graph is executed, by onnxruntime.

Licence: the U^2-Net weights/code are Apache-2.0 (xuebinqin/U-2-Net LICENSE);
the rembg packaging/export is MIT (danielgatis/rembg LICENSE.txt, (c) 2020
Daniel Gatis).

Graph (inspected with onnx): ir_version 6, opset 11, producer pytorch 1.9,
input "input.1" float32 [1, 3, 320, 320] (fixed batch 1), 7 outputs
"1959".."1965" each the Sigmoid of one side output; output 0 ("1959") is the
fused d0 prediction (values already in 0..1). 1,125,485 fp32 parameters,
1055 nodes (119 Conv, 38 Resize, 33 MaxPool).

Recipe (rembg sessions/base.py normalize + sessions/u2netp.py predict, which
is also the original U-2-Net u2net_test.py ToTensorLab(flag=0)): RGB, resize
to 320x320 with LANCZOS, divide by the per-image max pixel value (NOT a fixed
255 - identical whenever the frame contains a 255 pixel; U2NETP_SCALE=255
forces /255), ImageNet mean (0.485, 0.456, 0.406) / std (0.229, 0.224, 0.225),
NCHW float32. Output ort_outs[0][:, 0] is taken as is (sigmoid in the graph);
rembg additionally min-max stretches it per image (U2NETP_MINMAX=1 reproduces
that, off by default - it is a per-image contrast hack, not the model) and
resizes with LANCZOS; here the matte is resized back to the frame size with a
bilinear cv2.resize (protocol) - the blocky 320 -> 720 edge IS the point of
this row. Values 0..1, 1 = subject.

Precision: fp32 as exported. No fp16 variant was made: the opset-11
Resize/Cast chains are fragile under a hand-made fp16 cast, and the fp32 graph
is already far inside the preview budget on the GPU.

Devices: "cuda[:N]" -> CUDAExecutionProvider (onnxruntime.preload_dlls() first
so the CUDA 13 + cuDNN 9 DLLs of the nvidia-* wheels, or torch/lib, are loaded;
asserted active, ORT would otherwise silently fall back to CPU); "cpu" ->
CPUExecutionProvider with intra_op_num_threads = U2NETP_CPU_THREADS (default 8,
recorded in ctx and printed once at load).
"""
from __future__ import annotations

import hashlib
import os
import time

import cv2
import numpy as np
from PIL import Image

NAME = "u2netp-onnx"
BATCH = 1  # the graph's batch dimension is fixed at 1

_HERE = os.path.dirname(os.path.abspath(__file__))
_BENCH = os.path.dirname(_HERE)
MODELS_DIR = os.environ.get("EZLG_BENCH_MODELS") or os.path.join(_BENCH, "models")
MODEL_MD5 = "8e83ca70e441ab06c318d82300c84806"
MODEL_SHA256 = "309c8469258dda742793dce0ebea8e6dd393174f89934733ecc8b14c76f4ddd8"
MODEL_URL = "https://github.com/danielgatis/rembg/releases/download/v0.0.0/u2netp.onnx"
INPUT_SIZE = 320
IMAGENET_MEAN = np.array([0.485, 0.456, 0.406], dtype=np.float32)
IMAGENET_STD = np.array([0.229, 0.224, 0.225], dtype=np.float32)


def _model_path() -> str:
    p = os.environ.get("U2NETP_MODEL") or os.path.join(MODELS_DIR, "u2netp.onnx")
    if not os.path.isfile(p):
        raise FileNotFoundError(f"{p} missing; download {MODEL_URL} there (md5 {MODEL_MD5}; see models.json / tools/fetch-models.sh)")
    with open(p, "rb") as f:
        blob = f.read()
    md5 = hashlib.md5(blob).hexdigest()
    sha = hashlib.sha256(blob).hexdigest()
    if md5 != MODEL_MD5 or sha != MODEL_SHA256:
        raise RuntimeError(f"{p}: checksum mismatch md5={md5} sha256={sha} (expected {MODEL_MD5} / {MODEL_SHA256})")
    return p


def load(device: str):
    import onnxruntime as ort

    scale_env = os.environ.get("U2NETP_SCALE", "max").strip().lower()
    if scale_env not in ("max", "255"):
        raise ValueError(f"U2NETP_SCALE must be max|255, got {scale_env!r}")
    minmax = os.environ.get("U2NETP_MINMAX", "0").strip() in ("1", "true", "yes")
    cpu_threads = int(os.environ.get("U2NETP_CPU_THREADS", "8"))

    so = ort.SessionOptions()
    so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    so.log_severity_level = 3
    want_cuda = device.startswith("cuda")
    if want_cuda:
        if hasattr(ort, "preload_dlls"):
            ort.preload_dlls()  # CUDA 13 / cuDNN 9 DLLs (nvidia-* wheels or torch/lib) before the session exists
        dev_id = int(device.split(":", 1)[1]) if ":" in device else 0
        providers = [
            ("CUDAExecutionProvider", {"device_id": dev_id, "cudnn_conv_algo_search": "EXHAUSTIVE", "cudnn_conv_use_max_workspace": "1"}),
            "CPUExecutionProvider",
        ]
    else:
        so.intra_op_num_threads = cpu_threads
        providers = ["CPUExecutionProvider"]

    path = _model_path()
    t0 = time.perf_counter()
    sess = ort.InferenceSession(path, so, providers=providers)
    active = sess.get_providers()
    if want_cuda and active[0] != "CUDAExecutionProvider":
        raise RuntimeError(f"CUDAExecutionProvider not active, got {active} (would silently benchmark on CPU)")
    inp = sess.get_inputs()[0]
    outs = sess.get_outputs()
    ctx = {
        "sess": sess,
        "input_name": inp.name,
        "output_name": outs[0].name,  # d0, the fused prediction
        "input_shape": list(inp.shape),
        "providers": active,
        "device": device,
        "cpu_threads": cpu_threads if not want_cuda else None,
        "scale": scale_env,
        "minmax": minmax,
        "model_path": path,
        "session_ms": (time.perf_counter() - t0) * 1000.0,
        "frames_max_not_255": 0,
        "raw_minmax": None,
    }
    print(
        f"[u2netp-onnx] providers={active} input={inp.name}{list(inp.shape)} outputs={len(outs)} "
        f"scale={scale_env} minmax={minmax} cpu_threads={ctx['cpu_threads']} session={ctx['session_ms']:.0f}ms"
    )
    return ctx


def preprocess(frame: np.ndarray, ctx) -> np.ndarray:
    """HxWx3 uint8 RGB -> 1x3x320x320 float32 (LANCZOS, /max (or /255), ImageNet norm)."""
    im = Image.fromarray(frame, "RGB").resize((INPUT_SIZE, INPUT_SIZE), Image.LANCZOS)
    x = np.asarray(im, dtype=np.float32)
    if ctx["scale"] == "255":
        x = x / 255.0
    else:
        mx = float(x.max())
        if mx != 255.0:
            ctx["frames_max_not_255"] += 1
        x = x / max(mx, 1e-6)
    x = (x - IMAGENET_MEAN) / IMAGENET_STD
    return np.ascontiguousarray(x.transpose(2, 0, 1)[None])


def postprocess(raw: np.ndarray, h: int, w: int, ctx) -> np.ndarray:
    m = np.asarray(raw, dtype=np.float32)[0, 0]  # [1,1,320,320] -> [320,320], sigmoid already
    if ctx.get("raw_minmax") is None:
        ctx["raw_minmax"] = (float(m.min()), float(m.max()))
    if ctx["minmax"]:
        mn, mx = float(m.min()), float(m.max())
        if mx > mn:
            m = (m - mn) / (mx - mn)
    if (h, w) != m.shape:
        m = cv2.resize(m, (w, h), interpolation=cv2.INTER_LINEAR)
    return np.clip(m, 0.0, 1.0).astype(np.float32)


def infer(ctx, frames):
    sess = ctx["sess"]
    out = []
    for f in frames:
        h, w = f.shape[:2]
        x = preprocess(f, ctx)
        y = sess.run([ctx["output_name"]], {ctx["input_name"]: x})[0]
        out.append(postprocess(y, h, w, ctx))
    return out


def reset(ctx):
    pass  # stateless per-frame model
