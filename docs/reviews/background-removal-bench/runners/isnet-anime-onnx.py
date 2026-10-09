"""Runner: isnet-anime (SkyTNT anime-segmentation, ISNet / DIS architecture), ONNX.

Weights (no remote code, no trust_remote_code, onnxruntime only):
  https://github.com/danielgatis/rembg/releases/download/v0.0.0/isnet-anime.onnx
  176,069,933 bytes, md5 6f184e756bb3bd901c8849220a83e38e (= rembg's pinned
  pooch checksum in rembg/sessions/dis_anime.py), sha256
  f15622d853e8260172812b657053460e20806f04b9e05147d49af7bed31a6e99.
  That sha256 is exactly the LFS oid of `isnetis.onnx` in HF skytnt/anime-seg
  at revision 493cb60893f47441b26ec4fb9a306bce9e342982, i.e. rembg's copy and
  the author's HF upload are byte-identical.
  Licence: Apache-2.0 (LICENSE of github.com/SkyTNT/anime-segmentation and
  `license: apache-2.0` on the HF model card). rembg itself is MIT.
  Expected at $EZLG_BENCH_MODELS/isnet-anime.onnx (default: ../models next to
  this benchmark; ../models.json lists the file, ../tools/fetch-models.sh
  downloads it). The md5 is verified before the session is built.

Graph facts (onnx 1.23): opset 11, exported by pytorch 1.10, input `img`
float32 [1,3,1024,1024] (fixed batch 1 -> BATCH = 1), output `mask` float32
[1,1,1024,1024]; the last node is a Sigmoid, so the output is already a 0..1
probability. 355 nodes: Conv/Relu/Concat/Resize(linear, pytorch_half_pixel)/
MaxPool/Add/Sigmoid, 44.0 M float parameters. The 34 Resize nodes carry
absolute `sizes` initializers, so the graph only runs at its baked-in size
(`../tools/rescale_graph.py` rewrites them; ISNET_ANIME_SIZE below).

Pre-processing = rembg DisSession (dis_anime.py @ rembg main 202e4264,
2026-09-20): resize to 1024x1024 (rembg: PIL LANCZOS; here cv2 bilinear),
/255, minus mean (0.485, 0.456, 0.406), std (1, 1, 1) -- NOT the ImageNet std.
Post: take out[0][0, 0] as is (rembg additionally min-max normalises the
prediction per image; that is NOT done here by default, the sigmoid output is
used directly -- set ISNET_ANIME_MINMAX=1 for the rembg behaviour), bilinear
resize of the float matte back to the frame's HxW, clip 0..1.
Stateless per-frame model: reset() is a no-op.

Precision: the shipped graph is fp32. `../tools/to_fp16.py` makes an fp16 copy
(all float initializers + internal tensors fp16, Resize roi/scales kept fp32 as
the op requires, Cast at input/output so the IO stays float32); the runner
builds it on first use when ISNET_ANIME_PRECISION=fp16. The research measured
fp16 vs fp32 as MAE 0.00005 (max 0.005), identical IoU.

Derived graphs are written next to the source file as
<name>.fp16.onnx / <name>.<size>.onnx / <name>.fp16.<size>.onnx.

Env overrides:
  ISNET_ANIME_ONNX        path to a .onnx (default: $EZLG_BENCH_MODELS/isnet-anime.onnx)
  ISNET_ANIME_PRECISION   fp32 | fp16 (default fp16 on cuda, fp32 on cpu)
  ISNET_ANIME_SIZE        square input size; when it differs from the graph's
                          baked-in size a rescaled copy is derived on first use
                          with ../tools/rescale_graph.py (the proposal's CPU
                          configuration is ISNET_ANIME_SIZE=512 at fp32)
  ISNET_ANIME_MINMAX=1    rembg's per-image min-max normalisation of the output
  ISNET_ANIME_INTERP      linear (default) | lanczos | area : resize to the net
"""
from __future__ import annotations

import hashlib
import importlib.util
import os

import cv2
import numpy as np

NAME = "isnet-anime-onnx"
BATCH = 1  # the graph has a fixed batch dimension of 1

_HERE = os.path.dirname(os.path.abspath(__file__))
_BENCH = os.path.dirname(_HERE)
MODELS_DIR = os.environ.get("EZLG_BENCH_MODELS") or os.path.join(_BENCH, "models")
TOOLS_DIR = os.path.join(_BENCH, "tools")
DEFAULT_MODEL = os.path.join(MODELS_DIR, "isnet-anime.onnx")
MODEL_URL = "https://github.com/danielgatis/rembg/releases/download/v0.0.0/isnet-anime.onnx"
EXPECT_MD5 = "6f184e756bb3bd901c8849220a83e38e"

MEAN = np.array([0.485, 0.456, 0.406], dtype=np.float32)
STD = np.array([1.0, 1.0, 1.0], dtype=np.float32)  # rembg dis_anime.py: std 1, not ImageNet

_INTERP = {
    "linear": cv2.INTER_LINEAR,
    "lanczos": cv2.INTER_LANCZOS4,
    "area": cv2.INTER_AREA,
}


def _md5(path: str) -> str:
    h = hashlib.md5()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _tool(name: str):
    """Import ../tools/<name>.py by path (the tools are plain scripts, not a package)."""
    path = os.path.join(TOOLS_DIR, name + ".py")
    spec = importlib.util.spec_from_file_location("isnet_tool_" + name, path)
    if spec is None or spec.loader is None:
        raise FileNotFoundError(path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _derived(src: str, tag: str) -> str:
    base, _ = os.path.splitext(src)
    return base + "." + tag + ".onnx"


def _fp16_path(src: str) -> str:
    dst = _derived(src, "fp16")
    if not os.path.isfile(dst):
        _tool("to_fp16").convert(src, dst)
    return dst


def _graph_size(path: str) -> int:
    import onnx

    m = onnx.load(path, load_external_data=False)
    return int(m.graph.input[0].type.tensor_type.shape.dim[2].dim_value)


def _rescaled_path(src: str, size: int) -> str:
    if _graph_size(src) == size:
        return src
    dst = _derived(src, str(size))
    if not os.path.isfile(dst):
        _tool("rescale_graph").rescale(src, dst, size)
    return dst


def load(device: str):
    import onnxruntime as ort

    want_cuda = device.startswith("cuda")
    if want_cuda and hasattr(ort, "preload_dlls"):
        # CUDA 13 / cuDNN 9 DLLs from the nvidia-* wheels (onnxruntime-gpu[cuda,cudnn]) or
        # torch/lib must be loaded before the session exists, else ORT falls back to CPU.
        ort.preload_dlls()

    src = os.environ.get("ISNET_ANIME_ONNX", DEFAULT_MODEL)
    if not os.path.isfile(src):
        raise FileNotFoundError(f"{src} missing; download {MODEL_URL} into {MODELS_DIR} (md5 {EXPECT_MD5}; see models.json / tools/fetch-models.sh)")
    if src == DEFAULT_MODEL:
        got = _md5(src)
        assert got == EXPECT_MD5, f"isnet-anime.onnx md5 {got} != {EXPECT_MD5}"
    precision = os.environ.get("ISNET_ANIME_PRECISION", "fp16" if want_cuda else "fp32").lower()
    path = _fp16_path(src) if precision == "fp16" else src
    size_env = int(os.environ.get("ISNET_ANIME_SIZE", "0") or 0)
    if size_env:
        path = _rescaled_path(path, size_env)

    so = ort.SessionOptions()
    so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    so.log_severity_level = 3
    ort.set_default_logger_severity(3)
    if want_cuda:
        dev_id = int(device.split(":")[1]) if ":" in device else 0
        providers = [
            ("CUDAExecutionProvider", {"device_id": dev_id, "cudnn_conv_algo_search": "HEURISTIC"}),
            "CPUExecutionProvider",
        ]
    else:
        providers = ["CPUExecutionProvider"]
    sess = ort.InferenceSession(path, so, providers=providers)
    if want_cuda:
        assert sess.get_providers()[0] == "CUDAExecutionProvider", f"CUDA EP not active: {sess.get_providers()}"

    inp = sess.get_inputs()[0]
    out = sess.get_outputs()[0]
    assert inp.name == "img" and out.name == "mask", (inp.name, out.name)
    shape = list(inp.shape)
    assert len(shape) == 4 and shape[0] == 1 and shape[1] == 3, shape
    size = (int(shape[3]), int(shape[2]))  # (W, H)
    return {
        "sess": sess,
        "device": device,
        "path": path,
        "precision": precision,
        "size": size,
        "minmax": os.environ.get("ISNET_ANIME_MINMAX", "0") == "1",
        "interp": _INTERP[os.environ.get("ISNET_ANIME_INTERP", "linear").lower()],
    }


def _pre_numpy(frame: np.ndarray, size, interp) -> np.ndarray:
    """Reference pre-processing (kept for the equivalence check): ~21 ms/frame at 1024."""
    h, w = frame.shape[:2]
    x = frame if (w, h) == size else cv2.resize(frame, size, interpolation=interp)
    x = x.astype(np.float32) * (1.0 / 255.0)
    x = (x - MEAN) / STD
    return np.ascontiguousarray(x.transpose(2, 0, 1)[None])  # [1,3,H,W] float32


def _pre(frame: np.ndarray, size, interp) -> np.ndarray:
    """Same maths as _pre_numpy in one C++ call: blobFromImage computes
    (resize(x) - mean*255) * (1/255) and writes NCHW float32 directly
    (measured ~7 ms instead of ~19 ms per 720->1024 frame, ~1.3 vs 3.5 ms at
    512; max |diff| 6e-8 against _pre_numpy). It always resizes with
    INTER_LINEAR, so other ISNET_ANIME_INTERP values take the reference path."""
    h, w = frame.shape[:2]
    if interp != cv2.INTER_LINEAR and (w, h) != size:
        return _pre_numpy(frame, size, interp)
    return cv2.dnn.blobFromImage(frame, scalefactor=1.0 / 255.0, size=size, mean=tuple(float(m) for m in MEAN * 255.0), swapRB=False, crop=False, ddepth=cv2.CV_32F)


def _post(pred: np.ndarray, h: int, w: int, size, minmax: bool) -> np.ndarray:
    a = np.asarray(pred, dtype=np.float32).reshape(size[1], size[0])
    if minmax:  # rembg's per-image normalisation (optional A/B)
        mi, ma = float(a.min()), float(a.max())
        a = (a - mi) / max(ma - mi, 1e-6)
    if (w, h) != size:
        a = cv2.resize(a, (w, h), interpolation=cv2.INTER_LINEAR)
    return np.clip(a, 0.0, 1.0)


def infer(ctx, frames):
    sess, size = ctx["sess"], ctx["size"]
    out = []
    for f in frames:
        h, w = f.shape[:2]
        pred = sess.run(["mask"], {"img": _pre(f, size, ctx["interp"])})[0]
        out.append(_post(pred, h, w, size, ctx["minmax"]))
    return out


def reset(ctx):  # stateless per-frame model
    return None
