#!/usr/bin/env python3
"""ez-local-gif matte sidecar — background-removal models behind a tiny HTTP/1.1 API.

One process on the stdlib ``http.server.ThreadingHTTPServer`` (one thread per
request): ONNX segmenters (isnet-anime, BiRefNet-lite) on onnxruntime and the
SAM 2.1 tracker (sam2-tiny, "Guided") on PyTorch. The app (``internal/jobs``)
streams rgb24 frames and gets one 8-bit gray PNG matte per frame back;
decoding, resizing, the memo and the merge are the app's and ffmpeg's job.
See docs/background-removal-proposal.md §7 and docs/DESIGN.md §4.3.

Devices (Phase 5c): ONE process offers every device it can — ``["cuda", "cpu"]``
on the cuda image when the CUDA execution provider works, ``["cpu"]`` otherwise
— and every request picks one with ``device=`` (default: the default device,
``MATTE_DEVICE``). A model is a separate runtime per device (its own graph
choice, precision, sessions, self-test timing); nothing stays resident: every
runtime is self-tested at start and released, loads again on first use and is
released after ``MATTE_MODEL_TTL`` idle or on ``/v1/unload``.

API (compose network only — no auth, never publish the port):

  GET  /v1/ping                      state snapshot, never behind the inference lock.
                                     200; 503 with the same body plus "error" when the device is unavailable
                                     (MATTE_DEVICE=cuda without a working CUDA EP) so the healthcheck fails. Per model
                                     "prompts": the prompt kinds it takes — ["box","points","mask"] for a tracker, [] for a
                                     segmenter (additive, Phase 5d).
  POST /v1/matte?model=&device=&size=&frames=
                                     body: frames × size × size × 3 bytes of rgb24, row-major; Content-Length exact.
                                     200 application/x-ezlg-mattes: frames records [uint32 BE length][PNG], then uint32 0.
                                     400 bad params / length mismatch / a tracker model · 404 unknown model, size or device,
                                     or "device cpu not offered" · 413 frames > MATTE_MAX_BATCH or body > 128 MiB ·
                                     503 {"error":"model loading","retryAfterMs":N} while the model is downloading / loading,
                                     {"error":"model unavailable: …"} · 503 {"error":"out of memory","retryAfterMs":N} when
                                     unloading another model freed memory · 507 {"error":"out of memory: …"} when nothing
                                     could be freed (the model is re-tested before it is offered again).
  POST /v1/track?model=&device=&w=&h=&frames=N
                                     body: N rgb24 frames at w×h (the app sends the clip at the tracking size, long side ≤ 1024);
                                     header X-Matte-Prompts: {"obj":1,"prompts":[{"frame":i,"points":[[x,y,label],…],
                                     "box":[x0,y0,x1,y1]|null}]} with coordinates in 0..1 of the frame, label 1 = keep / 0 =
                                     remove; at least one prompt with a box, a positive point or a mask. Every prompted frame
                                     conditions the tracker; it propagates forward from the earliest prompted frame to N−1 and
                                     backward to 0.
                                     A mask prompt (Phase 5d): the header adds "mask":{"frame":i} and the body carries, AFTER the
                                     N frames, ONE record [uint32 BE length][8-bit gray PNG at w×h, >= 128 = subject] —
                                     Content-Length = N×w×h×3 + 4 + the PNG's length. The mask conditions frame i like a box
                                     (add_new_mask: the frame's output IS the mask); the frame's box / points, if any, refine
                                     the mask first, geometrically (Sam2Tracker._refine: the box keeps the mask inside it, a
                                     + / − click adds / removes the smallest region SAM sees at the click — SAM 2 cannot
                                     refine a mask prompt on its own frame, measured). One mask per request; its frame needs
                                     no box / points; 400 for a mask without its record, a record without a mask prompt, a
                                     non-PNG, or a PNG that is not w×h.
                                     200: N records of 8-bit gray PNGs at w×h (binary 0/255 from logit > 0), same framing as
                                     /v1/matte. 400 bad params / prompts / a segmenter model · 413 frames > MATTE_MAX_TRACK_FRAMES
                                     or body > 1 GiB · the 404/503/507 of /v1/matte.
  POST /v1/track/frame?model=&device=&w=&h=
                                     body: ONE rgb24 frame (+ the mask record after it for a mask prompt); header X-Matte-Prompts:
                                     the prompts of that frame (one frame index; a mask names the same frame)
                                     → 200 image/png, that frame's mask (for the live overlay while the user clicks).
  POST /v1/warm?model=&device=       download / derive / self-test / create the runtime now (200 with the state).
  POST /v1/unload[?model=&device=]   release the sessions of one model / device or everything (200; a runtime busy with a
                                     request is skipped and listed under "busy" — the TTL releases it later).

Subcommands: ``serve`` (default) · ``download [id …]`` (fetch + verify into /models, no runtime needed; for
air-gapped hosts pre-seed the volume — with no ids EVERY model of models.json, whatever MATTE_MODELS says, so the
volume serves the cpu and the cuda service alike) · ``selftest`` (build-time check through a synthetic 8×8 ONNX graph
and, when torch + sam2 import, the tracker plumbing with random weights; the real checkpoint when it is present).

Environment (defaults):
  MATTE_DEVICE=auto             the DEFAULT device: auto | cuda | cpu. auto = cuda when the EP works, else cpu. cuda without a
                                working CUDA EP stays up and reports device "unavailable". cpu offers the CPU only.
  MATTE_MODELS=<csv>            ids from models.json to offer (default: the ids models.json offers on the process's devices;
                                a model named here whose `offer` list has none of them is offered on every device).
  MATTE_DEFAULT_MODEL=          the default model on every device ("" = models.json's `defaultFor` hints: birefnet-lite on cuda,
                                isnet-anime on cpu); MATTE_DEFAULT_MODEL_CUDA / MATTE_DEFAULT_MODEL_CPU override one device.
  MATTE_PRELOAD=                models that STAY resident after the start self-test ("" = none: every runtime is released after
                                its self-test and loads again on first use).
  MATTE_SELFTEST=1              0 = no self-test at start (runtimes load on first use; msPerFrame unknown until then).
  MATTE_MODEL_TTL=300           seconds idle before a runtime's sessions are released (0 = never).
  MATTE_GPU_MEM_LIMIT_GIB=6     CUDA arena cap for models that need one (birefnet-lite); lower values fail, it is a knob for
                                larger cards.
  MATTE_THREADS=0               intra-op threads for the CPU EP and torch (0 = physical cores, capped by the cgroup CPU quota).
  MATTE_MAX_TRACK_FRAMES=3000   frames per POST /v1/track.
  MATTE_TRACK_OFFLOAD_FRAMES=300  a track of more frames keeps SAM 2's per-frame memory bank in host RAM instead of VRAM
                                (offload_state_to_cpu: ~1.2× slower, no OOM on long clips; 0 = always offload).
  MATTE_MODELS_BASE_URL=        download <base>/<file> instead of the pinned URL list (an internal mirror).
  MATTE_PORT=9402  MATTE_BIND=0.0.0.0  MATTE_MAX_BATCH=32  MATTE_MODELS_DIR=/models  MATTE_VERSION=dev
"""
from __future__ import annotations

import gc
import hashlib
import importlib.util
import json
import logging
import math
import os
import queue
import re
import secrets
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))

PROTOCOL = 1
PROCESSING_VERSION = "1"  # bump when pre/post-processing or a derivation changes: the app keys its memo on it
MATTES_CONTENT_TYPE = "application/x-ezlg-mattes"
PROMPTS_HEADER = "X-Matte-Prompts"
MAX_BODY_BYTES = 128 << 20  # /v1/matte
MAX_TRACK_BODY_BYTES = 1 << 30  # /v1/track
MAX_PROMPTS = 32  # prompted frames per track
TRACKER_PROMPTS = ["box", "points", "mask"]  # what /v1/ping reports a tracker takes (a segmenter takes none)
SOCKET_TIMEOUT = 30.0  # read / idle timeout per connection
WARMUP_FRAMES = 8  # the self-test's timed frames (msPerFrame)
MISSING_RETRY_S = 600.0  # a model still missing after the download attempts is retried this often
DOWNLOAD_ATTEMPTS = 6  # rounds over the URL list, exponential backoff between rounds (2 s … 60 s)
GPU_SAMPLE_S = 10.0  # nvidia-smi cadence for freeGiB (a background thread: /v1/ping never waits on it)
TTL_TICK_S = 5.0
SMALL_CARD_GIB = 10.0  # under this one model is resident at a time (the residency rule)
OOM_RETRY_MS = 3000
BUSY_RETRY_MS = 1000  # /v1/track/frame while a track holds the runtime: 503 "model busy" + this retry delay (the overlay re-asks)
FRAME_LOCK_WAIT_S = 1.0  # how long /v1/track/frame waits for the runtime before answering busy
UNLOAD_LOCK_WAIT_S = 0.5  # how long /v1/unload waits for a runtime's lock before skipping it as busy
SELFTEST_SIZE = 8
INLINE_CREATE_MS = 3000.0  # a released runtime whose last session create took longer answers 503 + loads in the background
TRACK_SELFTEST_WH = (160, 120)  # the tracker self-test clip (a square moving 1 px per frame on a dark gradient)

S_READY, S_LOADING, S_DOWNLOADING, S_MISSING, S_UNAVAILABLE = "ready", "loading", "downloading", "missing", "unavailable"
D_CUDA, D_CPU, D_UNAVAILABLE = "cuda", "cpu", "unavailable"
K_SEGMENTER, K_TRACKER = "segmenter", "tracker"
CUDA_EP = "CUDAExecutionProvider"
CPU_EP = "CPUExecutionProvider"
SAM2_IMAGE_MEAN = (0.485, 0.456, 0.406)  # the sam2 package's load_video_frames defaults
SAM2_IMAGE_STD = (0.229, 0.224, 0.225)

# What an onnxruntime / torch exception looks like when the device ran out of memory (CUDA arena, cuDNN/cuBLAS workspace, host).
OOM_RE = re.compile(r"out of memory|cudaErrorMemoryAllocation|CUDA failure 2\b|Failed to allocate memory|ALLOC_FAILED|bad_alloc|OutOfMemory|is smaller than requested bytes", re.I)

log = logging.getLogger("matte")


def _load_module(name: str, path: str):
    """Import a plain script by path (png.py and tools/*.py are not a package; ``python -I`` keeps HERE off sys.path)."""
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise FileNotFoundError(path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


png = _load_module("ezlg_png", os.path.join(HERE, "png.py"))


def is_oom(e: BaseException) -> bool:
    return isinstance(e, MemoryError) or bool(OOM_RE.search(str(e)))


def short(e: BaseException, n: int = 240) -> str:
    s = " ".join(str(e).split())
    return f"{type(e).__name__}: {s[:n]}" if s else type(e).__name__


# ---------------------------------------------------------------------------
# configuration and registry
# ---------------------------------------------------------------------------
class Config:
    def __init__(self, env=None):
        env = os.environ if env is None else env
        self.device_want = (env.get("MATTE_DEVICE") or "auto").strip().lower()
        if self.device_want not in ("auto", D_CUDA, D_CPU):
            raise SystemExit(f"MATTE_DEVICE={self.device_want!r}: expected auto, cuda or cpu")
        self.models_dir = env.get("MATTE_MODELS_DIR") or "/models"
        self.port = int(env.get("MATTE_PORT") or 9402)
        self.bind = env.get("MATTE_BIND") or "0.0.0.0"
        self.models_csv = env.get("MATTE_MODELS")  # None = the registry's per-device default
        self.default_model = (env.get("MATTE_DEFAULT_MODEL") or "").strip()  # "" = the registry's defaultFor hints
        self.default_model_for = {D_CUDA: (env.get("MATTE_DEFAULT_MODEL_CUDA") or "").strip(), D_CPU: (env.get("MATTE_DEFAULT_MODEL_CPU") or "").strip()}
        self.preload_csv = env.get("MATTE_PRELOAD") or ""  # "" = nothing stays resident after the start self-test
        self.selftest_at_start = (env.get("MATTE_SELFTEST") or "1").strip() != "0"
        self.model_ttl = float(env.get("MATTE_MODEL_TTL") or 300)
        self.gpu_mem_limit_gib = float(env.get("MATTE_GPU_MEM_LIMIT_GIB") or 6)
        self.threads = int(env.get("MATTE_THREADS") or 0)
        self.base_url = (env.get("MATTE_MODELS_BASE_URL") or "").strip().rstrip("/")
        self.max_batch = int(env.get("MATTE_MAX_BATCH") or 32)
        self.max_track_frames = int(env.get("MATTE_MAX_TRACK_FRAMES") or 3000)
        self.track_offload_frames = int(env.get("MATTE_TRACK_OFFLOAD_FRAMES") or 300)  # 0 = always keep the tracker state in host RAM
        self.version = env.get("MATTE_VERSION") or "dev"
        self.registry_path = env.get("MATTE_REGISTRY") or os.path.join(HERE, "models.json")
        # test hooks
        self.session_factory = None  # (path, SessionOptions, providers) -> session-like object
        self.gpu_query = None  # () -> {"name","totalGiB","freeGiB"} | None
        self.tracker_factory = None  # (Runtime, App) -> tracker-like object (track / track_frame)

    @staticmethod
    def csv(s: str | None) -> list[str]:
        return [x.strip() for x in (s or "").split(",") if x.strip()]


def load_registry(path: str) -> dict:
    with open(path, encoding="utf-8") as f:
        d = json.load(f)
    if d.get("v") != 1 or not isinstance(d.get("models"), dict):
        raise ValueError(f"{path}: unsupported models.json")
    return d["models"]


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def physical_cores() -> int:
    """Physical cores (not SMT threads), capped by the cgroup v2 CPU quota; os.cpu_count() where /proc is absent."""
    n = os.cpu_count() or 1
    try:
        cores = set()
        phys = core = None
        with open("/proc/cpuinfo", encoding="utf-8") as f:
            for line in f:
                k, _, v = line.partition(":")
                k = k.strip()
                if k == "physical id":
                    phys = v.strip()
                elif k == "core id":
                    core = v.strip()
                elif k == "" and phys is not None and core is not None:
                    cores.add((phys, core))
                    phys = core = None
        if cores:
            n = min(n, len(cores))
    except OSError:
        pass
    try:
        with open("/sys/fs/cgroup/cpu.max", encoding="utf-8") as f:
            quota, period = f.read().split()[:2]
        if quota != "max":
            n = max(1, min(n, -(-int(quota) // int(period))))
    except (OSError, ValueError):
        pass
    return max(1, n)


def mem_available_gib() -> float | None:
    try:
        with open("/proc/meminfo", encoding="utf-8") as f:
            for line in f:
                if line.startswith("MemAvailable:"):
                    return int(line.split()[1]) / (1 << 20)
    except OSError:
        pass
    return None


def query_gpu() -> dict | None:
    """name / totalGiB / freeGiB of GPU 0 through nvidia-smi (injected by the NVIDIA runtime with the `utility` capability)."""
    try:
        r = subprocess.run(["nvidia-smi", "--query-gpu=name,memory.total,memory.free", "--format=csv,noheader,nounits"], capture_output=True, text=True, timeout=5)
    except (OSError, subprocess.SubprocessError):
        return None
    if r.returncode != 0 or not r.stdout.strip():
        return None
    try:
        name, total, free = [s.strip() for s in r.stdout.strip().splitlines()[0].split(",")]
        return {"name": name, "totalGiB": round(int(total) / 1024, 2), "freeGiB": round(int(free) / 1024, 2)}
    except ValueError:
        return None


# ---------------------------------------------------------------------------
# downloads: URL list, exponential backoff, Range resume, sha256 verification
# ---------------------------------------------------------------------------
class DownloadError(Exception):
    pass


def fetch_url(url: str, part: str, total: int, progress, stop: threading.Event | None = None) -> None:
    have = os.path.getsize(part) if os.path.exists(part) else 0
    if total and have >= total:
        return
    req = urllib.request.Request(url, headers={"User-Agent": "ezlg-matte/" + PROCESSING_VERSION})
    if have:
        req.add_header("Range", f"bytes={have}-")
    with urllib.request.urlopen(req, timeout=30) as r:  # noqa: S310 - pinned https URLs from models.json
        if r.status == 206 and have:
            mode = "ab"
        elif r.status == 200:
            mode, have = "wb", 0  # no Range support: start over
        else:
            raise DownloadError(f"HTTP {r.status} from {url}")
        with open(part, mode) as f:
            while True:
                if stop is not None and stop.is_set():
                    raise DownloadError("stopped")
                chunk = r.read(1 << 20)
                if not chunk:
                    break
                f.write(chunk)
                have += len(chunk)
                progress(have)
    if total and have != total:
        raise DownloadError(f"short download: {have} of {total} bytes from {url}")


def download_model(urls: list[str], dst: str, sha256: str, total: int, progress=lambda n: None, stop: threading.Event | None = None, attempts: int = DOWNLOAD_ATTEMPTS) -> None:
    """Try the URLs in order with exponential backoff between rounds; resume a .part; rename on a verified sha256."""
    if not urls:
        raise DownloadError("no download URL")
    os.makedirs(os.path.dirname(dst) or ".", exist_ok=True)
    part = dst + ".part"
    last: Exception = DownloadError("download failed")
    for attempt in range(attempts):
        for url in urls:
            try:
                log.info("downloading %s -> %s (attempt %d/%d)", url, dst, attempt + 1, attempts)
                fetch_url(url, part, total, progress, stop)
                got = sha256_file(part)
                if got != sha256:
                    os.remove(part)
                    raise DownloadError(f"sha256 mismatch from {url}: got {got[:16]}…, want {sha256[:16]}…")
                os.replace(part, dst)
                return
            except (DownloadError, urllib.error.URLError, OSError, socket.timeout) as e:
                last = e
                log.warning("download %s: %s", url, short(e))
                if stop is not None and stop.is_set():
                    raise DownloadError("stopped") from e
        if attempt + 1 < attempts:
            time.sleep(min(2.0 * (2**attempt), 60.0))
    raise DownloadError(short(last))


# ---------------------------------------------------------------------------
# the synthetic self-test graph (no download needed): Conv 3→1 → Resize ½ → Resize ×2 → Sigmoid
# ---------------------------------------------------------------------------
def build_synthetic_graph(path: str, size: int = SELFTEST_SIZE) -> None:
    import onnx
    from onnx import TensorProto, helper, numpy_helper

    w = np.full((1, 3, 3, 3), -2.0 / 27.0, dtype=np.float32)
    half = size // 2
    nodes = [
        helper.make_node("Conv", ["img", "w", "b"], ["c"], name="conv", kernel_shape=[3, 3], pads=[1, 1, 1, 1]),
        helper.make_node("Resize", ["c", "roi", "scales", "sz_down"], ["d"], name="down", mode="linear", coordinate_transformation_mode="pytorch_half_pixel"),
        helper.make_node("Resize", ["d", "roi", "scales", "sz_up"], ["u"], name="up", mode="linear", coordinate_transformation_mode="pytorch_half_pixel"),
        helper.make_node("Sigmoid", ["u"], ["mask"], name="sig"),
    ]
    inits = [
        numpy_helper.from_array(w, "w"),
        numpy_helper.from_array(np.zeros(1, np.float32), "b"),
        numpy_helper.from_array(np.array([], np.float32), "roi"),
        numpy_helper.from_array(np.array([], np.float32), "scales"),
        numpy_helper.from_array(np.array([1, 1, half, half], np.int64), "sz_down"),
        numpy_helper.from_array(np.array([1, 1, size, size], np.int64), "sz_up"),
    ]
    g = helper.make_graph(
        nodes,
        "ezlg_matte_selftest",
        [helper.make_tensor_value_info("img", TensorProto.FLOAT, [1, 3, size, size])],
        [helper.make_tensor_value_info("mask", TensorProto.FLOAT, [1, 1, size, size])],
        inits,
    )
    m = helper.make_model(g, opset_imports=[helper.make_opsetid("", 11)], producer_name="ezlg-matte")
    m.ir_version = 7
    onnx.checker.check_model(m)
    onnx.save(m, path)


def synthetic_spec(path: str, precision="fp32", sizes=(SELFTEST_SIZE, 2 * SELFTEST_SIZE)) -> dict:
    """A segmenter registry entry for the synthetic graph; `precision` is a string (both devices) or a per-device dict."""
    return {
        "label": "synthetic self-test graph",
        "file": os.path.basename(path),
        "bytes": os.path.getsize(path),
        "sha256": sha256_file(path),
        "urls": [],
        "licence": "n/a",
        "offer": [D_CUDA, D_CPU],
        "graph_size": SELFTEST_SIZE,
        "sizes": list(sizes),
        "default_size": {D_CUDA: SELFTEST_SIZE, D_CPU: SELFTEST_SIZE},
        "precision": dict(precision) if isinstance(precision, dict) else {D_CUDA: precision, D_CPU: precision},
        "rescalable": True,
        "input": {"name": "img", "mean": [0.485, 0.456, 0.406], "std": [1.0, 1.0, 1.0]},
        "output": {"name": "mask", "activation": "sigmoid"},
    }


def synthetic_tracker_spec(path: str, image_size: int = 64) -> dict:
    """A tracker registry entry whose checkpoint is the file at `path` (tests replace the predictor with Config.tracker_factory)."""
    return {
        "label": "synthetic tracker",
        "kind": K_TRACKER,
        "file": os.path.basename(path),
        "bytes": os.path.getsize(path),
        "sha256": sha256_file(path),
        "urls": [],
        "licence": "n/a",
        "offer": [D_CUDA, D_CPU],
        "config": "configs/sam2.1/sam2.1_hiera_t.yaml",
        "image_size": image_size,
        "precision": {D_CUDA: "bf16", D_CPU: "fp32"},
    }


def synthetic_track_clip(n: int = WARMUP_FRAMES, w: int = TRACK_SELFTEST_WH[0], h: int = TRACK_SELFTEST_WH[1]) -> tuple[bytes, list[tuple[int, int, int, int]]]:
    """n rgb24 frames of a bright square moving one pixel per frame across a dark gradient, and the square's box per frame."""
    rng = np.random.default_rng(99)
    f = np.zeros((n, h, w, 3), np.uint8)
    f[:, :, :, 0] = np.linspace(10, 70, w).astype(np.uint8)[None, None, :]
    f[:, :, :, 2] = np.linspace(10, 70, h).astype(np.uint8)[None, :, None]
    side = max(4, min(w, h) // 3)
    x0, y0 = w // 4, (h - side) // 2
    boxes = []
    for i in range(n):
        x = min(x0 + i, w - side)
        f[i, y0 : y0 + side, x : x + side, :] = (240, 230, 90)
        boxes.append((x, y0, x + side - 1, y0 + side - 1))
    f = np.clip(f.astype(np.int16) + rng.integers(0, 12, f.shape, dtype=np.int16), 0, 255).astype(np.uint8)
    return f.tobytes(), boxes


def box_mask(box: tuple[int, int, int, int], w: int, h: int) -> np.ndarray:
    m = np.zeros((h, w), bool)
    x0, y0, x1, y1 = box
    m[max(0, y0) : y1 + 1, max(0, x0) : x1 + 1] = True
    return m


def mask_iou(a: np.ndarray, b: np.ndarray) -> float:
    u = np.count_nonzero(a | b)
    return float(np.count_nonzero(a & b) / u) if u else 1.0


# ---------------------------------------------------------------------------
# prompts (X-Matte-Prompts): parse + validate + convert to pixels of the clip
# ---------------------------------------------------------------------------
class PromptError(ValueError):
    pass


def _num(v, what: str) -> float:
    if isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v):
        raise PromptError(f"{what} must be a finite number")
    return float(v)


def _unit(v, what: str) -> float:
    x = _num(v, what)
    if x < 0.0 or x > 1.0:
        raise PromptError(f"{what} must be within 0..1")
    return x


def mask_record_cap(w: int, h: int) -> int:
    """The largest mask record (the PNG without its 4-byte length) accepted for w×h frames: an 8-bit gray PNG is at most
    the (w+1)×h filtered rows in zlib stored blocks (5 bytes per 64 KiB) plus chunk headers — 1 % slack and 1 KiB cover it."""
    rows = (w + 1) * h
    return rows + rows // 100 + 1024


def decode_mask(data: bytes, w: int, h: int) -> np.ndarray:
    """A mask prompt's PNG → (h, w) bool, >= 128 = subject. Any PNG colour type is taken as its luma (Pillow when it is
    installed — it is, for the tracker; the stdlib codec otherwise, 8-bit gray only); not a PNG or not w×h → PromptError."""
    if not data.startswith(png.SIGNATURE):
        raise PromptError("the mask record is not a PNG")
    try:
        try:
            from PIL import Image
        except ImportError:  # the cpu test environment without Pillow: the stdlib codec (8-bit gray only)
            a = png.decode_gray(data)
        else:
            import io

            with Image.open(io.BytesIO(data)) as im:
                if im.format != "PNG":
                    raise PromptError("the mask record is not a PNG")
                a = np.asarray(im.convert("L"))
    except PromptError:
        raise
    except Exception as e:  # noqa: BLE001 - a broken file is the client's error, not a 500
        raise PromptError(f"the mask PNG cannot be decoded ({short(e)})") from e
    if a.shape != (h, w):
        raise PromptError(f"the mask is {a.shape[1]}×{a.shape[0]}, the frames are {w}×{h}")
    return a >= 128


def parse_mask_record(rec: bytes, w: int, h: int) -> np.ndarray:
    """The [uint32 BE length][PNG] record that follows the frames of a mask-prompted request → decode_mask."""
    if len(rec) < 4:
        raise PromptError("the mask record is shorter than its 4-byte length")
    (n,) = struct.unpack(">I", rec[:4])
    if n != len(rec) - 4:
        raise PromptError(f"the mask record declares {n} bytes but {len(rec) - 4} follow (one record, nothing after it)")
    return decode_mask(rec[4:], w, h)


def parse_prompts(raw: str | None, frames: int, w: int, h: int, single: bool = False) -> tuple[int, dict[int, dict], int | None]:
    """The X-Matte-Prompts JSON → (obj id, {frame: {"points": [(x, y), …], "labels": [1|0, …], "box": (x0, y0, x1, y1) | None}},
    the mask prompt's frame or None) in pixels of w×h. Rules: 1..MAX_PROMPTS prompts, frame in 0..frames-1,
    coordinates in 0..1, labels 0/1, a box has x0 < x1 and y0 < y1, one box per frame, and at least one prompt with a
    box, a positive point or a mask. A mask prompt is the top-level "mask": {"frame": i} (one per request; i needs no
    entry in "prompts" — one is made — and no box / points); the handler decodes the body's record into that frame's
    "mask" key (an (h, w) bool array; absent / None on every other frame). `single` (/v1/track/frame) requires every
    prompt (and the mask) to name the same frame and does not range-check it (the clicked frame's index in the clip
    is passed through as given)."""
    if not raw or not raw.strip():
        raise PromptError(f"{PROMPTS_HEADER} header required")
    try:
        d = json.loads(raw)
    except ValueError as e:
        raise PromptError(f"{PROMPTS_HEADER}: not JSON ({short(e)})") from e
    if not isinstance(d, dict) or not isinstance(d.get("prompts"), list):
        raise PromptError(f"{PROMPTS_HEADER}: expected an object with a prompts list")
    obj = d.get("obj", 1)
    if isinstance(obj, bool) or not isinstance(obj, int) or obj < 1:
        raise PromptError("obj must be a positive integer")
    mask = d.get("mask")
    mask_fi: int | None = None
    if mask is not None:
        if not isinstance(mask, dict) or isinstance(mask.get("frame"), bool) or not isinstance(mask.get("frame"), int) or mask["frame"] < 0:
            raise PromptError('mask must be {"frame": N} with a non-negative integer frame')
        mask_fi = mask["frame"]
        if not single and mask_fi >= frames:
            raise PromptError(f"mask frame {mask_fi} is outside the clip of {frames} frames")
    if not d["prompts"] and mask_fi is None:
        raise PromptError("at least one prompt is required")
    if len(d["prompts"]) > MAX_PROMPTS:
        raise PromptError(f"{len(d['prompts'])} prompts exceed {MAX_PROMPTS}")
    out: dict[int, dict] = {}
    positive = mask_fi is not None
    for p in d["prompts"]:
        if not isinstance(p, dict):
            raise PromptError("each prompt must be an object")
        fi = p.get("frame", 0)
        if isinstance(fi, bool) or not isinstance(fi, int) or fi < 0:
            raise PromptError("frame must be a non-negative integer")
        if single:
            # /v1/track/frame carries the clicked frame's real index (the clip's slot, not 0): only "one frame" is checked
            if (out and fi not in out) or (mask_fi is not None and fi != mask_fi):
                raise PromptError("every prompt of /v1/track/frame must name the same frame" + (" (the mask's)" if mask_fi is not None else ""))
        elif fi >= frames:
            raise PromptError(f"frame {fi} is outside the clip of {frames} frames")
        e = out.setdefault(fi, {"points": [], "labels": [], "box": None})
        pts = p.get("points") or []
        if not isinstance(pts, list):
            raise PromptError("points must be a list of [x, y, label]")
        for pt in pts:
            if not isinstance(pt, list) or len(pt) != 3:
                raise PromptError("each point must be [x, y, label]")
            x, y = _unit(pt[0], "point x"), _unit(pt[1], "point y")
            lab = _num(pt[2], "point label")
            if lab not in (0.0, 1.0):
                raise PromptError("point label must be 1 (keep) or 0 (remove)")
            e["points"].append((x * w, y * h))
            e["labels"].append(int(lab))
            positive = positive or lab == 1.0
        box = p.get("box")
        if box is not None:
            if not isinstance(box, list) or len(box) != 4:
                raise PromptError("box must be [x0, y0, x1, y1]")
            x0, y0, x1, y1 = (_unit(v, "box coordinate") for v in box)
            if not (x0 < x1 and y0 < y1):
                raise PromptError("box must have x0 < x1 and y0 < y1")
            if e["box"] is not None:
                raise PromptError(f"frame {fi} has more than one box")
            e["box"] = (x0 * w, y0 * h, x1 * w, y1 * h)
            positive = True
        if not e["points"] and e["box"] is None and fi != mask_fi:
            raise PromptError(f"frame {fi}: a prompt needs a box, points or the mask")
    if mask_fi is not None:
        out.setdefault(mask_fi, {"points": [], "labels": [], "box": None})
    if not positive:
        raise PromptError("at least one box, positive point or mask is required (negative clicks alone select nothing)")
    return obj, out, mask_fi


# ---------------------------------------------------------------------------
# the SAM 2 tracker (torch): frames fed from the rgb24 body, one object, binary masks at the clip size
# ---------------------------------------------------------------------------
_torch_mod = None
_torch_tried = False
_torch_lock = threading.Lock()


def import_torch():
    """torch (or None when it is not installed / fails to import). Imported BEFORE onnxruntime so the shared NVIDIA
    wheels are loaded by torch's loader; the result is cached, the failure logged once."""
    global _torch_mod, _torch_tried
    with _torch_lock:
        if _torch_tried:
            return _torch_mod
        _torch_tried = True
        if importlib.util.find_spec("torch") is None:
            return None
        try:
            t0 = time.perf_counter()
            import torch

            _torch_mod = torch
            log.info("torch %s imported in %.1f s", torch.__version__, time.perf_counter() - t0)
        except Exception as e:  # noqa: BLE001 - the tracker reports it, the ONNX models do not need torch
            log.warning("import torch failed: %s", short(e))
        return _torch_mod


_sam2_patched = False


def _cc_scipy(mask):
    """Replacement for sam2's CUDA connected-components kernel (the extension is not built): 8-connectivity labels and
    per-pixel component areas of (N, 1, H, W) masks with scipy.ndimage.label, the kernel's contract."""
    import torch
    from scipy import ndimage

    m = mask.detach().cpu().numpy().astype(bool)
    labels = np.zeros(m.shape, np.int32)
    counts = np.zeros(m.shape, np.int32)
    eight = np.ones((3, 3), bool)
    for n in range(m.shape[0]):
        lab, k = ndimage.label(m[n, 0], structure=eight)
        if k:
            sizes = np.bincount(lab.ravel())
            sizes[0] = 0
            labels[n, 0] = lab
            counts[n, 0] = sizes[lab]
    return torch.from_numpy(labels).to(mask.device), torch.from_numpy(counts).to(mask.device)


def import_sam2():
    """The sam2 package with the two process-wide patches: frames from a `Frames` object instead of a JPEG folder and
    scipy connected components for the hole filling. Raises when torch or sam2 are missing."""
    global _sam2_patched
    torch = import_torch()
    if torch is None:
        raise RuntimeError("torch is not installed")
    import sam2.sam2_video_predictor as svp
    import sam2.utils.misc as misc
    from sam2.build_sam import build_sam2_video_predictor

    if not _sam2_patched:
        original = svp.load_video_frames

        def load_video_frames(video_path, image_size, offload_video_to_cpu, *args, **kwargs):
            if isinstance(video_path, Frames):
                if video_path.image_size != image_size:
                    raise RuntimeError(f"frames prepared for {video_path.image_size}², predictor wants {image_size}²")
                return video_path, video_path.h, video_path.w
            return original(video_path, image_size, offload_video_to_cpu, *args, **kwargs)

        svp.load_video_frames = load_video_frames
        svp.tqdm = lambda it, *a, **k: it  # the predictor wraps its loop in tqdm: never a progress bar on stderr
        misc.get_connected_components = _cc_scipy
        # the image predictor (Sam2Tracker._refine) narrates every set_image through the ROOT logger ("Computing image
        # embeddings for the provided image...", three lines per click): dropped at the root, the sidecar's own line stays
        logging.getLogger().addFilter(lambda r: not r.pathname.endswith("sam2_image_predictor.py"))
        _sam2_patched = True
    return build_sam2_video_predictor


class Frames:
    """The predictor's frame sequence, converted lazily from the raw rgb24 body: frame i → (3, S, S) float tensor on
    the compute device, PIL bicubic resize to S×S then the ImageNet normalisation — the maths of sam2's
    `_load_img_as_tensor` without a file or a JPEG round trip, and without holding every frame as a tensor (a 600-frame
    clip would be 7.6 GB at 1024²)."""

    def __init__(self, body, n: int, w: int, h: int, image_size: int, device, mean=SAM2_IMAGE_MEAN, std=SAM2_IMAGE_STD):
        import torch
        from PIL import Image

        self.body, self.n, self.w, self.h, self.image_size, self.device = memoryview(body), n, w, h, image_size, device
        self._image, self._torch = Image, torch
        self.mean = torch.tensor(mean, dtype=torch.float32)[:, None, None].to(device)
        self.std = torch.tensor(std, dtype=torch.float32)[:, None, None].to(device)
        self.stride = w * h * 3

    def __len__(self) -> int:
        return self.n

    def frame(self, i: int) -> np.ndarray:
        """Frame i as an (h, w, 3) uint8 array at the clip's size (a view of the body)."""
        if not 0 <= i < self.n:
            raise IndexError(i)
        return np.frombuffer(self.body[i * self.stride : (i + 1) * self.stride], np.uint8).reshape(self.h, self.w, 3)

    def __getitem__(self, i: int):
        if not 0 <= i < self.n:
            raise IndexError(i)
        im = self._image.frombuffer("RGB", (self.w, self.h), self.body[i * self.stride : (i + 1) * self.stride], "raw", "RGB", 0, 1)
        if (self.w, self.h) != (self.image_size, self.image_size):
            im = im.resize((self.image_size, self.image_size))
        arr = np.asarray(im, dtype=np.float32) / 255.0
        t = self._torch.from_numpy(arr).permute(2, 0, 1).to(self.device)
        return (t - self.mean) / self.std


class Sam2Tracker:
    """SAM 2.1 video predictor on one device: `track` a clip with prompts, `track_frame` one frame for the overlay."""

    def __init__(self, spec: dict, device: str, ckpt: str | None, image_size: int, precision: str, threads: int):
        build = import_sam2()
        torch = import_torch()
        self.torch, self.device, self.image_size, self.precision = torch, device, image_size, precision
        # Clips over this many frames keep SAM 2's per-frame memory bank (maskmem features, pred masks, object pointers:
        # ~1–1.5 MB per frame at image_size 1024) in host RAM (offload_state_to_cpu): ~1.2× slower, never an OOM on an
        # 8 GB card at 600 frames. App sets it from MATTE_TRACK_OFFLOAD_FRAMES; 0 = always offload.
        self.offload_frames = 300
        if device == D_CUDA:
            if not torch.cuda.is_available():
                raise RuntimeError("torch reports no CUDA device (driver / nvidia-container-toolkit?)")
            torch.backends.cuda.matmul.allow_tf32 = True
            torch.backends.cudnn.allow_tf32 = True
        else:
            torch.set_num_threads(threads)
        overrides = ["++model.add_all_frames_to_correct_as_cond=true"]  # corrections on later frames stick
        if image_size != 1024:
            overrides.append(f"++model.image_size={image_size}")
        t0 = time.perf_counter()
        self.predictor = build(spec.get("config") or "configs/sam2.1/sam2.1_hiera_t.yaml", ckpt, device=device, hydra_overrides_extra=overrides, apply_postprocessing=True)
        self.created_ms = (time.perf_counter() - t0) * 1000.0
        self._image = None  # the SAM2ImagePredictor of _refine, made on first use

    def _amp(self):
        torch = self.torch
        if self.device == D_CUDA and self.precision == "bf16":
            return torch.autocast("cuda", dtype=torch.bfloat16)
        if self.device == D_CUDA and self.precision == "fp16":
            return torch.autocast("cuda", dtype=torch.float16)
        return torch.autocast(self.device, enabled=False)

    def _add(self, state, frames: Frames, obj: int, fi: int, p: dict):
        """Condition frame fi with its prompt. Without a mask: the usual add_new_points_or_box. With one (Phase 5d): the
        frame's box / points first refine the mask geometrically (_refine), then add_new_mask conditions the frame with
        it — with the sam2.1 config's use_mask_input_as_output_without_sam the frame's output IS the mask (resized to
        image_size² and thresholded at 0.5; the prototype's best prompt). Returns the predictor's (frame_idx, obj_ids,
        video_res_masks)."""
        mask = p.get("mask")
        if mask is None:
            kw = {}
            if p["points"]:
                kw["points"] = np.asarray(p["points"], np.float32)
                kw["labels"] = np.asarray(p["labels"], np.int32)
            if p["box"] is not None:
                kw["box"] = np.asarray(p["box"], np.float32)
            return self.predictor.add_new_points_or_box(state, frame_idx=fi, obj_id=obj, clear_old_points=True, **kw)
        mask = np.ascontiguousarray(mask, dtype=bool)
        if p["points"] or p["box"] is not None:
            mask = self._refine(frames.frame(fi), frames.w, frames.h, mask, p)
        return self.predictor.add_new_mask(state, frame_idx=fi, obj_id=obj, mask=self.torch.from_numpy(mask))

    def _image_predictor(self):
        """SAM 2's image predictor over the same model (lazily; the image encoder is shared, nothing else is loaded)."""
        if self._image is None:
            from sam2.sam2_image_predictor import SAM2ImagePredictor

            ip = SAM2ImagePredictor(self.predictor)
            # its backbone feature sizes are written for image_size 1024 (256², 128², 64²: the strides 4, 8, 16); the
            # transforms already follow the model's image_size, this list has to as well (the 256² build self-test)
            s = self.predictor.image_size
            ip._bb_feat_sizes = [(s // 4, s // 4), (s // 8, s // 8), (s // 16, s // 16)]
            self._image = ip
        return self._image

    def _refine(self, frame, w: int, h: int, mask: np.ndarray, p: dict) -> np.ndarray:
        """A mask frame's box / points, applied to the mask after it — geometrically, from what SAM sees on the frame.

        Measured on the real checkpoint (corpus frame, ground-truth mask, 2026-10-09): SAM 2 cannot refine a mask prompt
        on its own frame. Its decoder ignores a mask fed as the dense prompt (the predictor's prev_sam_mask_logits path:
        a + click then gives the lone click's part, IoU 0.24, whatever the logit scale — the mask path bypasses the
        decoder in training), and its memory correction path (memory-encode the mask, click) is dominated by the mask's
        memory: a − click removes ~750 px nowhere near the click, a + click on a missing half adds 294 px, and a − click
        on the background flips the object score and empties the mask. What SAM does reliably is select a region at a
        click, in three candidates (sub-part / part / whole; SAM2ImagePredictor.predict(multimask_output=True)). So:
          · the box keeps the mask inside it (mask ∩ box): a loose box is a no-op, never the background;
          · a + click adds and a − click removes the SMALLEST candidate containing the click — the finest part SAM sees
            there (the plush 4.4k px, a hair part 5.9k, a torso part 23k), where the IoU-best candidate is the whole
            character for most clicks on it and would empty the mask on a − click; a click NO candidate contains (the
            background) is a no-op for either label — never a fallback to the IoU-best candidate, which on flat
            background is often a large region or the whole frame and would carve most of the mask out on a − click;
          · in the prompt's order (box first, then the points as listed); the frame then gets the refined mask.
        The overlay (track_frame) and the track compute the refinement with the same function on the same frame, so
        the overlay shows exactly what the track holds there."""
        torch = self.torch
        out = mask.copy()
        if p["box"] is not None:
            x0, y0, x1, y1 = p["box"]
            keep = np.zeros_like(out)
            keep[max(0, int(y0)) : min(h, int(math.ceil(y1))), max(0, int(x0)) : min(w, int(math.ceil(x1)))] = True
            out &= keep
        if not p["points"]:
            return out
        ip = self._image_predictor()
        with torch.inference_mode(), self._amp():
            ip.set_image(np.array(frame, copy=True))  # a writable copy: torchvision refuses a read-only buffer
            try:
                for (x, y), label in zip(p["points"], p["labels"]):
                    masks, scores, _ = ip.predict(point_coords=np.asarray([[x, y]], np.float32), point_labels=np.asarray([1], np.int32), multimask_output=True)
                    cands = np.asarray(masks).astype(bool)
                    ix, iy = min(w - 1, max(0, int(x))), min(h - 1, max(0, int(y)))
                    inside = [i for i in range(len(cands)) if cands[i][iy, ix]]
                    if not inside:
                        continue  # a click no candidate contains (the background): a no-op, never SAM's IoU-best region
                    k = min(inside, key=lambda i: int(cands[i].sum()))
                    if label == 1:
                        out |= cands[k]
                    else:
                        out &= ~cands[k]
            finally:
                ip.reset_predictor()
        return out

    def track(self, body, n: int, w: int, h: int, prompts: dict[int, dict], obj: int = 1) -> list[np.ndarray]:
        """Binary (h, w) masks for every frame: prompts condition their frames, propagation runs forward from the
        earliest prompted frame and backward from it to 0."""
        torch = self.torch
        frames = Frames(body, n, w, h, self.image_size, self.device)
        out: list[np.ndarray | None] = [None] * n
        offload = self.device == D_CUDA and n > self.offload_frames
        with torch.inference_mode(), self._amp():
            state = self.predictor.init_state(video_path=frames, offload_state_to_cpu=offload)
            try:
                for fi in sorted(prompts):
                    self._add(state, frames, obj, fi, prompts[fi])
                first = min(prompts)
                for fi, _ids, logits in self.predictor.propagate_in_video(state, start_frame_idx=first, reverse=False):
                    out[fi] = (logits[0, 0] > 0).cpu().numpy()
                if first > 0:
                    for fi, _ids, logits in self.predictor.propagate_in_video(state, start_frame_idx=first, reverse=True):
                        if out[fi] is None:
                            out[fi] = (logits[0, 0] > 0).cpu().numpy()
            finally:
                self.predictor.reset_state(state)
                del state
                if self.device == D_CUDA:
                    torch.cuda.empty_cache()
        if any(m is None for m in out):
            raise RuntimeError("the tracker did not visit every frame")
        return out  # type: ignore[return-value]

    def track_frame(self, body, w: int, h: int, prompt: dict, obj: int = 1) -> np.ndarray:
        """The prompted frame's own mask (a one-frame state through the same conditioning path the track uses, so
        the overlay shows exactly what the track will hold on that frame)."""
        torch = self.torch
        frames = Frames(body, 1, w, h, self.image_size, self.device)
        with torch.inference_mode(), self._amp():
            state = self.predictor.init_state(video_path=frames)
            try:
                _, _, logits = self._add(state, frames, obj, 0, prompt)
                return (logits[0, 0] > 0).cpu().numpy()
            finally:
                self.predictor.reset_state(state)
                del state


# ---------------------------------------------------------------------------
# models: one Model per registry id (facts + the shared download), one Runtime per (model, device)
# ---------------------------------------------------------------------------
class Session:
    __slots__ = ("sess", "size", "in_name", "out_name", "in_dtype", "path", "created_ms")

    def __init__(self, sess, size: int, in_name: str, out_name: str, in_dtype, path: str, created_ms: float):
        self.sess, self.size, self.in_name, self.out_name, self.in_dtype, self.path, self.created_ms = sess, size, in_name, out_name, in_dtype, path, created_ms


class Model:
    def __init__(self, mid: str, spec: dict, cfg: Config):
        self.id = mid
        self.spec = spec
        self.kind = spec.get("kind") or K_SEGMENTER
        if self.kind not in (K_SEGMENTER, K_TRACKER):
            raise ValueError(f"model {mid}: unknown kind {self.kind!r}")
        self.label = spec.get("label") or mid
        self.licence = spec.get("licence", "")
        self.file = os.path.join(cfg.models_dir, spec["file"])
        self.bytes = int(spec.get("bytes") or 0)
        self.weights = spec["sha256"]
        self.urls = [cfg.base_url + "/" + spec["file"]] if cfg.base_url else list(spec.get("urls") or [])
        self.offer = list(spec.get("offer") or [D_CUDA, D_CPU])
        self.default_for = list(spec.get("defaultFor") or [])
        self.runtimes: dict[str, Runtime] = {}
        # the download is one per model, mirrored by every runtime's snapshot
        self.downloading = False
        self.percent = 0
        self.download_error = ""

    @property
    def is_tracker(self) -> bool:
        return self.kind == K_TRACKER


class Runtime:
    """A model on one device: its graph choice (precision / sizes), its sessions and its state."""

    def __init__(self, m: Model, device: str, cfg: Config):
        self.model = m
        self.id = m.id
        self.device = device
        spec = m.spec
        prec = spec.get("precision", "fp32")
        self.precision = prec.get(device, "fp32") if isinstance(prec, dict) else str(prec)
        if m.is_tracker:
            self.graph_size = int(spec.get("image_size") or 1024)
            self.sizes = [self.graph_size]
            self.default_size = self.graph_size
            self.rescalable = False
        else:
            self.graph_size = int(spec.get("graph_size") or spec["sizes"][0])
            self.sizes = [int(s) for s in spec["sizes"]]
            ds = spec.get("default_size", self.graph_size)
            self.default_size = int(ds.get(device, self.graph_size) if isinstance(ds, dict) else ds)
            self.rescalable = bool(spec.get("rescalable", False))
            mean = np.asarray(spec["input"].get("mean", [0, 0, 0]), np.float32)
            std = np.asarray(spec["input"].get("std", [1, 1, 1]), np.float32)
            self.scale = (1.0 / (255.0 * std)).astype(np.float32)
            self.offset = (-mean / std).astype(np.float32)
            self.in_name = spec["input"].get("name")
            self.out_name = spec["output"].get("name")
            self.logits = spec["output"].get("activation", "sigmoid") == "logits"
        self.state = S_MISSING if not os.path.isfile(m.file) else S_LOADING
        # a present file not yet loaded stays "loading" with this reason until the start self-test / a request / /v1/warm loads it
        self.reason = "" if self.state == S_MISSING else "not loaded yet — loads on first use"
        self.percent = 0
        self.last_error = ""
        self.graph_digest = ""
        self.ms: dict[int, float] = {}
        self.paths: dict[int, str] = {}
        self.prepared = False
        self.create_ms = 0.0  # the last session create (decides whether a released runtime re-creates inline)
        self.sessions: dict[int, object] = {}
        self.lock = threading.Lock()  # one inference (or session create) at a time per runtime
        self.last_used = time.monotonic()
        self.queued = False
        self.want_size = 0  # the size a background warm should create (0 = default)

    @property
    def is_tracker(self) -> bool:
        return self.model.is_tracker

    @property
    def name(self) -> str:
        return f"{self.id}@{self.device}"

    def retry_ms(self) -> int:
        if self.state == S_DOWNLOADING or self.model.downloading:
            return 5000
        if self.create_ms:
            return int(min(max(self.create_ms * 0.5, 1000.0), 5000.0))
        return 5000 if "birefnet" in self.id else 1500

    def _state(self) -> tuple[str, str, int]:
        if self.model.downloading and not os.path.isfile(self.model.file):
            return S_DOWNLOADING, "", int(self.model.percent)
        return self.state, self.reason, int(self.percent)

    def device_snapshot(self) -> dict:
        state, reason, percent = self._state()
        return {
            "state": state,
            "reason": reason,
            "percent": percent,
            "precision": self.precision,
            "size": self.default_size,
            "sizes": list(self.sizes),
            "msPerFrame": {str(k): round(v, 2) for k, v in sorted(self.ms.items())},
            "resident": bool(self.sessions),
            "graphDigest": self.graph_digest,
            "lastError": self.last_error,
        }

    def snapshot(self) -> dict:
        """The top-level (pre-5c) model fields, mirrored from this runtime."""
        state, reason, percent = self._state()
        return {
            "state": state,
            "reason": reason,
            "percent": percent,
            "weights": self.model.weights,
            "graphDigest": self.graph_digest,
            "precision": self.precision,
            "sizes": list(self.sizes),
            "defaultSize": self.default_size,
            "msPerFrame": {str(k): round(v, 2) for k, v in sorted(self.ms.items())},
            "licence": self.model.licence,
            "lastError": self.last_error,
            "label": self.model.label,
            "resident": bool(self.sessions),
            "kind": self.model.kind,
            "prompts": list(TRACKER_PROMPTS) if self.is_tracker else [],
        }


# ---------------------------------------------------------------------------
# the application: devices, models, loader, HTTP
# ---------------------------------------------------------------------------
class App:
    def __init__(self, cfg: Config, registry: dict | None = None):
        self.cfg = cfg
        self.registry = registry if registry is not None else load_registry(cfg.registry_path)
        self.instance = secrets.token_hex(8)
        self.lock = threading.Lock()  # state publication (ping), busy count
        self.create_lock = threading.Lock()  # serialises session creation + room making (order: create_lock → runtime.lock)
        self.devices: list[str] = []  # every device the process offers, default first
        self.default_device = D_CPU
        self.device = D_CPU  # the default device, or "unavailable" (the pre-5c ping field)
        self.device_reason = ""
        self.gpu: dict | None = None
        self.models: dict[str, Model] = {}
        self.default_models: dict[str, str] = {}
        self.preload: list[str] = []
        self.busy = 0
        self.loader_q: queue.Queue = queue.Queue()
        self.stop = threading.Event()
        self.httpd: MatteServer | None = None
        self.threads: list[threading.Thread] = []
        self._tmp: str | None = None
        self._ort = None

    # -- startup ---------------------------------------------------------------
    @property
    def ort(self):
        if self._ort is None:
            import_torch()  # before onnxruntime: both share the NVIDIA wheels in the cuda image
            import onnxruntime as ort

            ort.set_default_logger_severity(3)
            self._ort = ort
        return self._ort

    def _synthetic_path(self) -> str:
        if self._tmp is None:
            self._tmp = tempfile.mkdtemp(prefix="ezlg-matte-")
        path = os.path.join(self._tmp, "synthetic.onnx")
        if not os.path.isfile(path):
            build_synthetic_graph(path)
        return path

    def sample_gpu(self) -> dict | None:
        q = self.cfg.gpu_query or query_gpu
        g = q()
        with self.lock:
            self.gpu = g
        return g

    def _probe_cuda(self) -> str | None:
        """None when the CUDA execution provider runs a session, else the reason it does not."""
        try:
            ort = self.ort
            if hasattr(ort, "preload_dlls"):
                try:
                    ort.preload_dlls()  # the CUDA / cuDNN libraries from the nvidia-* wheels, before any session exists
                except Exception as e:  # noqa: BLE001 - a missing wheel shows up in the session check below
                    log.warning("preload_dlls: %s", short(e))
            if CUDA_EP not in ort.get_available_providers():
                raise RuntimeError("this onnxruntime build has no CUDA execution provider (the cpu image?)")
            sess = ort.InferenceSession(self._synthetic_path(), providers=[(CUDA_EP, {"device_id": 0}), CPU_EP])
            if sess.get_providers()[0] != CUDA_EP:
                raise RuntimeError("the CUDA execution provider did not activate (missing CUDA / cuDNN libraries or no GPU visible)")
            sess.run(None, {"img": np.zeros((1, 3, SELFTEST_SIZE, SELFTEST_SIZE), np.float32)})
            del sess
            return None
        except Exception as e:  # noqa: BLE001 - every failure is reported, never fatal
            return f"CUDA EP not available — nvidia-container-toolkit / driver >= 580? ({short(e)})"

    def resolve_devices(self) -> None:
        want = self.cfg.device_want
        if want == D_CPU:
            self.devices, self.default_device, self.device = [D_CPU], D_CPU, D_CPU
            return
        reason = self._probe_cuda()
        if reason is None:
            self.devices, self.default_device, self.device = [D_CUDA, D_CPU], D_CUDA, D_CUDA
            return
        if want == D_CUDA:
            self.devices, self.default_device, self.device, self.device_reason = [], "", D_UNAVAILABLE, reason
            log.error("%s; staying up with device=unavailable (MATTE_DEVICE=cuda)", reason)
        else:
            self.devices, self.default_device, self.device, self.device_reason = [D_CPU], D_CPU, D_CPU, reason
            log.warning("%s; falling back to the CPU (MATTE_DEVICE=auto)", reason)

    def build_models(self) -> None:
        devs = self.devices or [D_CPU]
        explicit = self.cfg.models_csv is not None
        ids = Config.csv(self.cfg.models_csv) if explicit else [k for k, s in self.registry.items() if set(s.get("offer") or [D_CUDA, D_CPU]) & set(devs)]
        for mid in ids:
            if mid not in self.registry:
                raise SystemExit(f"MATTE_MODELS: unknown model {mid!r}; models.json has {sorted(self.registry)}")
            m = Model(mid, self.registry[mid], self.cfg)
            on = [d for d in devs if d in m.offer] or (list(devs) if explicit else [])
            for d in on:
                m.runtimes[d] = Runtime(m, d, self.cfg)
            if m.runtimes:
                self.models[mid] = m
        if not self.models:
            log.warning("no models offered (MATTE_MODELS is empty)")
        for d in self.devices:  # no default model on a device the process does not offer (device "unavailable": none)
            offered = [mid for mid, m in self.models.items() if d in m.runtimes]
            if not offered:
                continue
            want = self.cfg.default_model_for.get(d) or self.cfg.default_model
            hinted = [mid for mid in offered if d in self.models[mid].default_for]
            if want and want in offered:
                self.default_models[d] = want
            else:
                if want:
                    log.warning("default model %s is not offered on %s; using %s", want, d, (hinted or offered)[0])
                self.default_models[d] = (hinted or offered)[0]
        self.preload = [m for m in Config.csv(self.cfg.preload_csv) if m in self.models]

    def start(self, serve: bool = True) -> None:
        os.makedirs(self.cfg.models_dir, exist_ok=True)
        self.resolve_devices()
        self.sample_gpu()
        self.build_models()
        log.info(
            "matte sidecar %s instance=%s devices=%s default_device=%s models=%s default_models=%s preload=%s selftest=%s models_dir=%s gpu=%s",
            self.cfg.version, self.instance, ",".join(self.devices) or "-", self.default_device or "-", ",".join(self.models) or "-", self.default_models, ",".join(self.preload) or "-", self.cfg.selftest_at_start, self.cfg.models_dir, self.gpu,
        )
        if self.device_reason:
            log.warning("device reason: %s", self.device_reason)
        self._thread(self._loader, "loader")
        self._thread(self._janitor, "janitor")
        if D_CUDA in self.devices:
            self._thread(self._gpu_sampler, "gpu")
        if self.devices and self.cfg.selftest_at_start:
            # every runtime, the default device first and files already on disk first: ready sooner
            rts = [rt for d in self.devices for m in self.models.values() for rt in m.runtimes.values() if rt.device == d]
            for rt in sorted(rts, key=lambda r: not os.path.isfile(r.model.file)):
                self.warm(rt)
        if serve:
            self.httpd = MatteServer((self.cfg.bind, self.cfg.port), Handler)
            self.httpd.app = self
            log.info("listening on %s:%d", *self.httpd.server_address[:2])

    @property
    def port(self) -> int:
        return self.httpd.server_address[1] if self.httpd else 0

    def _thread(self, fn, name: str) -> None:
        t = threading.Thread(target=fn, name=name, daemon=True)
        t.start()
        self.threads.append(t)

    def serve_forever(self) -> None:
        assert self.httpd is not None
        self.httpd.serve_forever(poll_interval=0.5)

    def shutdown(self) -> None:
        self.stop.set()
        self.loader_q.put(None)
        if self.httpd is not None:
            self.httpd.shutdown()
            self.httpd.server_close()

    # -- lookups ---------------------------------------------------------------
    def runtimes(self) -> list[Runtime]:
        return [rt for m in self.models.values() for rt in m.runtimes.values()]

    def runtime(self, mid: str, device: str) -> Runtime | None:
        m = self.models.get(mid)
        return m.runtimes.get(device) if m else None

    def default_model(self, device: str | None = None) -> str:
        return self.default_models.get(device or self.default_device, "")

    # -- state -------------------------------------------------------------------
    def set_state(self, rt: Runtime, state: str, reason: str = "", percent: int | None = None) -> None:
        with self.lock:
            if state != rt.state or reason != rt.reason:
                log.info("model %s: %s%s", rt.name, state, f" ({reason})" if reason else "")
            rt.state, rt.reason = state, reason
            if percent is not None:
                rt.percent = percent
            if state == S_UNAVAILABLE:
                rt.prepared = False  # re-tested (derive + self-test) before it is offered again

    def ping(self) -> tuple[int, dict]:
        with self.lock:
            models = {}
            for mid, m in self.models.items():
                top = m.runtimes.get(self.default_device) or next(iter(m.runtimes.values()))
                snap = top.snapshot()
                snap["devices"] = {d: rt.device_snapshot() for d, rt in m.runtimes.items()}
                models[mid] = snap
            busy, gpu = self.busy, self.gpu
        obj = {
            "protocol": PROTOCOL,
            "version": self.cfg.version,
            "instance": self.instance,
            "processingVersion": PROCESSING_VERSION,
            "device": self.device,
            "reason": self.device_reason,
            "gpu": gpu,
            "devices": list(self.devices),
            "defaultDevice": self.default_device,
            "defaultModel": self.default_model(),
            "defaultModels": dict(self.default_models),
            "models": models,
            "busy": busy,
        }
        if self.device == D_UNAVAILABLE:
            obj["error"] = self.device_reason
            return 503, obj
        return 200, obj

    def small_card(self) -> bool:
        return D_CUDA in self.devices and self.gpu is not None and self.gpu.get("totalGiB", 0) < SMALL_CARD_GIB

    # -- loader: download → derive → self-test → (release | resident) ---------------
    def warm(self, rt: Runtime, size: int = 0) -> None:
        """Queue download → derive → self-test → session for rt (idempotent while it is queued or loading)."""
        with self.lock:
            if size:
                rt.want_size = size
            if rt.queued:
                return
            rt.queued = True
        self.loader_q.put(rt)

    def _loader(self) -> None:
        while True:
            rt = self.loader_q.get()
            if rt is None or self.stop.is_set():
                return
            try:
                self._load(rt)
            except Exception as e:  # noqa: BLE001 - the loader thread must survive anything
                log.exception("loader: %s", short(e))
                self.set_state(rt, S_UNAVAILABLE, short(e))
                rt.last_error = short(e, 400)
            finally:
                with self.lock:
                    rt.queued = False

    def _download(self, m: Model) -> bool:
        with self.lock:
            m.downloading, m.percent = True, 0
        for rt in m.runtimes.values():
            if rt.state == S_MISSING:
                self.set_state(rt, S_DOWNLOADING, "", 0)
        total = m.bytes

        def progress(n: int, _m=m, _t=total):
            _m.percent = min(100, int(100 * n / _t)) if _t else 0

        try:
            download_model(m.urls, m.file, m.weights, total, progress, self.stop, attempts=DOWNLOAD_ATTEMPTS)
        except Exception as e:  # noqa: BLE001
            m.download_error = short(e, 400)
            for rt in m.runtimes.values():
                rt.last_error = m.download_error
                self.set_state(rt, S_MISSING, f"download failed: {short(e)}", 0)
            if not self.stop.is_set():
                for rt in m.runtimes.values():
                    t = threading.Timer(MISSING_RETRY_S, self.warm, args=(rt,))
                    t.daemon = True
                    t.start()
            return False
        finally:
            with self.lock:
                m.downloading = False
        m.percent = 100
        for rt in m.runtimes.values():
            if rt.state in (S_MISSING, S_DOWNLOADING):
                self.set_state(rt, S_LOADING, "not loaded yet — loads on first use", 100)
        return True

    def _load(self, rt: Runtime) -> None:
        if self.device == D_UNAVAILABLE:
            return
        if not os.path.isfile(rt.model.file) and not self._download(rt.model):
            return
        if rt.prepared:
            size = rt.want_size or rt.default_size
            rt.want_size = 0
            self._create_resident(rt, size)  # a no-op when it is resident already
            return
        self._prepare(rt)

    def _prepare(self, rt: Runtime) -> None:
        self.set_state(rt, S_LOADING, "preparing graphs")
        try:
            if not rt.is_tracker:
                self._derive(rt)
            else:
                rt.graph_digest = rt.model.weights
            if rt.device == D_CUDA:
                self.sample_gpu()
            why = self._precheck(rt)
            if why:
                self.set_state(rt, S_UNAVAILABLE, why)
                return
            self.set_state(rt, S_LOADING, "creating session")
            with self.create_lock:
                self._make_room(rt)
                with rt.lock:
                    sess = self._create_session(rt, rt.default_size)
                    if rt.is_tracker:
                        self._selftest_tracker(rt, sess)
                    else:
                        self._selftest(rt, sess)
            rt.prepared = True
            self.set_state(rt, S_READY, "")
            log.info("model %s ready: precision=%s size=%d graph=%s ms_per_frame=%.1f create_ms=%.0f", rt.name, rt.precision, rt.default_size, os.path.basename(rt.paths.get(rt.default_size, rt.model.file)), rt.ms.get(rt.default_size, 0.0), rt.create_ms)
            if rt.id not in self.preload:
                self._release(rt)
                log.info("released %s after its self-test (not in MATTE_PRELOAD); it loads again on first use", rt.name)
        except Exception as e:  # noqa: BLE001
            self._release(rt)
            rt.last_error = short(e, 400)
            need = (rt.model.spec.get("vram") or {}).get("min_free_gib")
            why = f"needs about {need:g} GB of free GPU memory" if is_oom(e) and need and rt.device == D_CUDA else short(e)
            if is_oom(e) and self.gpu and rt.device == D_CUDA:
                why += f", {self.gpu['freeGiB']:.1f} GB free"
            self.set_state(rt, S_UNAVAILABLE, why)
            log.error("model %s unavailable: %s", rt.name, why)

    def _precheck(self, rt: Runtime) -> str | None:
        if rt.device == D_CUDA:
            v = rt.model.spec.get("vram") or {}
            if v.get("cap") and self.cfg.gpu_mem_limit_gib < float(v.get("min_limit_gib") or 0):
                return f"MATTE_GPU_MEM_LIMIT_GIB={self.cfg.gpu_mem_limit_gib:g} is below the {float(v['min_limit_gib']):g} GiB this model needs"
            need = float(v.get("min_free_gib") or 0)
            if need and self.gpu is not None and self.gpu["freeGiB"] < need:
                return f"needs about {need:g} GB of free GPU memory, {self.gpu['freeGiB']:.1f} GB free"
        else:
            r = rt.model.spec.get("ram") or {}
            need = float(r.get("min_available_gib") or 0)
            if need:
                avail = mem_available_gib()
                if avail is not None and avail < need:
                    return f"cpu: {need:g} GiB of RAM needed, {avail:.0f} GiB available"
        return None

    def _tool(self, name: str):
        return _load_module("ezlg_tool_" + name, os.path.join(HERE, "tools", name + ".py"))

    def _derived(self, m: Model, src: str, dst: str, tool: str, fn) -> str:
        """Derive dst from src with tools/<tool>.py once per (weights, processingVersion); a stamp next to it records the digest."""
        stamp = dst + ".stamp.json"
        try:
            with open(stamp, encoding="utf-8") as f:
                st = json.load(f)
            if st.get("src") == m.weights and st.get("proc") == PROCESSING_VERSION and st.get("tool") == tool and os.path.getsize(dst) == st.get("bytes"):
                return dst
        except (OSError, ValueError):
            pass
        t0 = time.perf_counter()
        tmp = dst + ".tmp"
        try:
            fn(self._tool(tool), src, tmp)
            os.replace(tmp, dst)
        finally:
            if os.path.exists(tmp):
                os.remove(tmp)
        st = {"src": m.weights, "proc": PROCESSING_VERSION, "tool": tool, "bytes": os.path.getsize(dst), "sha256": sha256_file(dst), "from": os.path.basename(src)}
        with open(stamp + ".tmp", "w", encoding="utf-8") as f:
            json.dump(st, f, indent=1)
        os.replace(stamp + ".tmp", stamp)
        log.info("derived %s from %s with %s in %.1f s", os.path.basename(dst), os.path.basename(src), tool, time.perf_counter() - t0)
        return dst

    def _digest(self, m: Model, path: str) -> str:
        if path == m.file:
            return m.weights
        try:
            with open(path + ".stamp.json", encoding="utf-8") as f:
                return json.load(f)["sha256"]
        except (OSError, ValueError, KeyError):
            return sha256_file(path)

    def _derive(self, rt: Runtime) -> None:
        m = rt.model
        src = m.file
        base = src[:-5] if src.endswith(".onnx") else src
        cur = src
        if rt.precision == "fp16":
            try:
                cur = self._derived(m, src, base + ".fp16.onnx", "to_fp16", lambda tool, s, d: tool.convert(s, d))
            except Exception as e:  # noqa: BLE001 - the fp32 source graph is the fallback
                log.error("model %s: fp16 derivation failed, using the fp32 source graph: %s", rt.name, short(e))
                rt.last_error = f"fp16 derivation failed: {short(e)}"
                rt.precision, cur = "fp32", src
        paths: dict[int, str] = {}
        for size in list(rt.sizes):
            if size == rt.graph_size:
                paths[size] = cur
                continue
            if not rt.rescalable:
                continue
            dst = cur[:-5] + f".{size}.onnx"
            try:
                paths[size] = self._derived(m, cur, dst, "rescale_graph", lambda tool, s, d, _n=size: tool.rescale(s, d, _n))
            except Exception as e:  # noqa: BLE001
                log.error("model %s: %d² derivation failed, size dropped: %s", rt.name, size, short(e))
                rt.last_error = f"{size}² derivation failed: {short(e)}"
        with self.lock:
            rt.paths = paths
            rt.sizes = [s for s in rt.sizes if s in paths]
            if rt.default_size not in paths:
                rt.default_size = rt.graph_size if rt.graph_size in paths else rt.sizes[0]
            rt.graph_digest = self._digest(m, paths[rt.default_size])

    def _providers(self, rt: Runtime) -> list:
        if rt.device != D_CUDA:
            return [CPU_EP]
        # ORT's defaults otherwise: arena_extend_strategy kNextPowerOfTwo (kSameAsRequested fragments the capped arena —
        # lite's self-test then fails with "available memory of 221 MB is smaller than requested 892 MB" at 6 GiB) and the
        # default cudnn_conv_algo_search (HEURISTIC + the cap fails in a deform-conv). This is the configuration the
        # research measured at a stable ~6.3 GB footprint under the 6 GiB cap.
        opts = {"device_id": "0"}
        if (rt.model.spec.get("vram") or {}).get("cap"):
            opts["gpu_mem_limit"] = str(int(self.cfg.gpu_mem_limit_gib * (1 << 30)))
        return [(CUDA_EP, opts), CPU_EP]

    def _create_session(self, rt: Runtime, size: int):
        """Caller holds create_lock and rt.lock. A Session (segmenter) or a tracker object, stored under `size`."""
        t0 = time.perf_counter()
        if rt.is_tracker:
            tracker = self.cfg.tracker_factory(rt, self) if self.cfg.tracker_factory else Sam2Tracker(rt.model.spec, rt.device, rt.model.file, size, rt.precision, self.cfg.threads or physical_cores())
            if hasattr(tracker, "offload_frames"):
                tracker.offload_frames = self.cfg.track_offload_frames
            rt.sessions[size] = tracker
            rt.last_used = time.monotonic()
            rt.create_ms = (time.perf_counter() - t0) * 1000.0
            log.info("tracker %s created in %.0f ms (%s)", rt.name, rt.create_ms, os.path.basename(rt.model.file))
            return tracker
        path = rt.paths[size]
        ort = self.ort
        so = ort.SessionOptions()
        so.log_severity_level = 3
        so.intra_op_num_threads = self.cfg.threads or physical_cores()
        so.add_session_config_entry("session.intra_op.allow_spinning", "0")
        providers = self._providers(rt)
        sess = self.cfg.session_factory(path, so, providers) if self.cfg.session_factory else ort.InferenceSession(path, so, providers=providers)
        if rt.device == D_CUDA and sess.get_providers()[0] != CUDA_EP:
            raise RuntimeError(f"CUDA EP not active for {rt.name}: {sess.get_providers()}")
        inp, out = sess.get_inputs()[0], sess.get_outputs()[0]
        in_name, out_name = rt.in_name or inp.name, rt.out_name or out.name
        shape = list(inp.shape)
        if len(shape) == 4 and all(isinstance(d, int) for d in shape) and shape != [1, 3, size, size]:
            raise RuntimeError(f"{os.path.basename(path)}: input {inp.name}{shape} is not [1,3,{size},{size}]")
        in_dtype = np.float16 if "float16" in str(inp.type) else np.float32
        s = Session(sess, size, in_name, out_name, in_dtype, path, (time.perf_counter() - t0) * 1000.0)
        rt.sessions[size] = s
        rt.last_used = time.monotonic()
        rt.create_ms = s.created_ms
        log.info("session %s@%d created in %.0f ms (%s, %s)", rt.name, size, s.created_ms, os.path.basename(path), sess.get_providers()[0])
        return s

    def _make_room(self, rt: Runtime) -> int:
        """Caller holds create_lock. On a small card unload every other CUDA runtime before a session is created; returns the count."""
        if rt.device != D_CUDA or not self.small_card():
            return 0
        return self._unload_others(rt, blocking=True)

    def _unload_others(self, rt: Runtime, blocking: bool) -> int:
        n = 0
        for other in self.runtimes():
            if other is rt or other.device != rt.device or not other.sessions:
                continue
            if other.lock.acquire(timeout=120.0 if blocking else 2.0):  # a bounded wait either way (never a deadlock)
                try:
                    if other.sessions:
                        other.sessions.clear()
                        n += 1
                        log.info("released %s to make room for %s", other.name, rt.name)
                finally:
                    other.lock.release()
        if n:
            self._collect(rt.device)
        return n

    def _collect(self, device: str = "") -> None:
        gc.collect()
        torch = _torch_mod
        if torch is not None and device in ("", D_CUDA):
            try:
                if torch.cuda.is_available():
                    torch.cuda.empty_cache()
            except Exception:  # noqa: BLE001
                pass

    def _release(self, rt: Runtime) -> None:
        with rt.lock:
            rt.sessions.clear()
        self._collect(rt.device)

    def _create_resident(self, rt: Runtime, size: int):
        """Create rt's session for size unless it exists (lock order: create_lock → rt.lock). Errors propagate; the state
        goes back to ready so a failed create never leaves the runtime stuck in loading — the caller maps the error."""
        with self.create_lock:
            with rt.lock:
                s = rt.sessions.get(size)
                if s is not None:
                    return s
            self._make_room(rt)
            self.set_state(rt, S_LOADING, "creating session")
            try:
                with rt.lock:
                    s = self._create_session(rt, size)
            finally:
                if rt.state == S_LOADING:
                    self.set_state(rt, S_READY, "")
            return s

    def _selftest(self, rt: Runtime, sess: Session) -> None:
        size = sess.size
        rng = np.random.default_rng(1234)
        frame = rng.integers(0, 256, (size, size, 3), dtype=np.uint8).tobytes()
        first = self._run_batch(rt, sess, frame, 1, size)  # untimed: cuDNN algorithm search, arena growth
        a = png.decode_gray(first[0])
        if a.shape != (size, size):
            raise RuntimeError(f"self-test: matte shape {a.shape} != ({size}, {size})")
        t0 = time.perf_counter()
        pngs = self._run_batch(rt, sess, frame * WARMUP_FRAMES, WARMUP_FRAMES, size)
        per = (time.perf_counter() - t0) * 1000.0 / WARMUP_FRAMES
        if len(pngs) != WARMUP_FRAMES or any(p != pngs[0] for p in pngs):
            raise RuntimeError("self-test: the model is not deterministic per frame")
        with self.lock:
            rt.ms[size] = per

    def _selftest_tracker(self, rt: Runtime, tracker) -> None:
        """A synthetic clip (a square moving one pixel per frame) with a box prompt on frame 0: every mask must follow the
        square; msPerFrame is the timed 8-frame pass (init, prompt, propagation) over the frames. Then the same 2-frame
        clip from a MASK prompt (the square's box as a mask on frame 0): the mask path (add_new_mask + propagation) is
        verified before the runtime is offered, not at a user's first mask."""
        w, h = TRACK_SELFTEST_WH
        body2, boxes2 = synthetic_track_clip(2, w, h)
        prompt = {0: {"points": [], "labels": [], "box": tuple(float(v) for v in boxes2[0])}}
        self._run_track(rt, tracker, body2, 2, w, h, prompt, 1)  # untimed: cuDNN autotune, allocator growth
        body, boxes = synthetic_track_clip(WARMUP_FRAMES, w, h)
        t0 = time.perf_counter()
        pngs = self._run_track(rt, tracker, body, WARMUP_FRAMES, w, h, prompt, 1)
        per = (time.perf_counter() - t0) * 1000.0 / WARMUP_FRAMES
        if len(pngs) != WARMUP_FRAMES:
            raise RuntimeError(f"self-test: {len(pngs)} masks for {WARMUP_FRAMES} frames")
        self._check_square(pngs, boxes, w, h, "a box prompt")
        masked = {0: {"points": [], "labels": [], "box": None, "mask": box_mask(boxes2[0], w, h)}}
        self._check_square(self._run_track(rt, tracker, body2, 2, w, h, masked, 1), boxes2, w, h, "a mask prompt")
        with self.lock:
            rt.ms[rt.default_size] = per

    @staticmethod
    def _check_square(pngs: list[bytes], boxes: list[tuple[int, int, int, int]], w: int, h: int, what: str) -> None:
        for i, (p, box) in enumerate(zip(pngs, boxes)):
            m = png.decode_gray(p)
            if m.shape != (h, w):
                raise RuntimeError(f"self-test: mask shape {m.shape} != ({h}, {w})")
            iou = mask_iou(m >= 128, box_mask(box, w, h))
            if iou < 0.5:
                raise RuntimeError(f"self-test: the tracker lost the synthetic square from {what} at frame {i} (IoU {iou:.2f})")

    # -- inference ------------------------------------------------------------------
    def _run_batch(self, rt: Runtime, sess: Session, body: bytes, frames: int, size: int) -> list[bytes]:
        x = np.frombuffer(body, dtype=np.uint8).reshape(frames, size, size, 3).astype(np.float32)
        x *= rt.scale
        x += rt.offset
        x = np.ascontiguousarray(x.transpose(0, 3, 1, 2))  # NCHW; never a per-frame min–max normalisation
        if sess.in_dtype is np.float16:
            x = x.astype(np.float16)
        out: list[bytes] = []
        for i in range(frames):
            y = sess.sess.run([sess.out_name], {sess.in_name: x[i : i + 1]})[0]
            a = np.asarray(y, dtype=np.float32)
            if a.size != size * size:
                raise RuntimeError(f"model output has {a.size} values, expected {size}×{size}")
            a = a.reshape(size, size)
            if rt.logits:
                with np.errstate(over="ignore"):
                    a = 1.0 / (1.0 + np.exp(-a))
            np.clip(a, 0.0, 1.0, out=a)
            out.append(png.encode_gray((a * 255.0 + 0.5).astype(np.uint8)))
        return out

    def _run_track(self, rt: Runtime, tracker, body: bytes, frames: int, w: int, h: int, prompts: dict[int, dict], obj: int) -> list[bytes]:
        masks = tracker.track(body, frames, w, h, prompts, obj)
        if len(masks) != frames:
            raise RuntimeError(f"tracker returned {len(masks)} masks for {frames} frames")
        out: list[bytes] = []
        for m in masks:
            a = np.asarray(m)
            if a.shape != (h, w):
                raise RuntimeError(f"tracker mask has shape {a.shape}, expected ({h}, {w})")
            out.append(png.encode_gray(np.where(a.astype(bool), 255, 0).astype(np.uint8)))
        return out

    def gate(self, rt: Runtime) -> tuple[int, dict] | None:
        """Why a request on this runtime cannot run right now (status, body), or None."""
        if self.device == D_UNAVAILABLE:
            return 503, {"error": self.device_reason, "state": D_UNAVAILABLE}
        st, _reason, percent = rt._state()
        if st == S_READY:
            return None
        if st in (S_MISSING, S_LOADING, S_DOWNLOADING):
            if not rt.prepared:
                self.warm(rt)  # not loaded at start, or a download that failed earlier: start it now (idempotent while queued)
            return 503, {"error": "model loading", "retryAfterMs": 5000 if st == S_MISSING else rt.retry_ms(), "state": S_DOWNLOADING if st == S_MISSING else st, "percent": percent}
        return 503, {"error": f"model unavailable: {rt.reason}", "state": S_UNAVAILABLE}

    def _run(self, rt: Runtime, size: int, fn, lock_wait: float | None = None):
        """fn(session) under rt.lock with the resident session for `size`, created on demand (inline when the last create
        was quick, else 503 + a background load); returns (status, payload, content type). `lock_wait` bounds the wait
        for the runtime (a track holds it for the whole clip): over it the answer is 503 "model busy" + retryAfterMs
        instead of queueing behind the request — the live overlay's /v1/track/frame, whose client times out."""
        with self.lock:
            self.busy += 1
        try:
            for _attempt in range(3):
                err: BaseException | None = None
                result = None
                if not rt.lock.acquire(timeout=lock_wait if lock_wait is not None else -1):
                    return 503, json.dumps({"error": "model busy", "retryAfterMs": BUSY_RETRY_MS, "state": rt.state}).encode(), "application/json"
                try:  # one inference at a time per runtime; /v1/ping never takes this lock
                    sess = rt.sessions.get(size)
                    if sess is not None:
                        try:
                            result = fn(sess)
                        except Exception as e:  # noqa: BLE001 - mapped below, outside the lock
                            err = e
                        else:
                            rt.last_used = time.monotonic()
                finally:
                    rt.lock.release()
                if sess is not None:
                    if err is not None:
                        return self._failed(rt, size, err, creating=False)
                    return result
                gate = self.gate(rt)
                if gate:
                    return gate[0], json.dumps(gate[1]).encode(), "application/json"
                if rt.create_ms > INLINE_CREATE_MS:
                    # a slow re-create (lite: 10–20 s) runs in the loader; the client retries after retryAfterMs
                    self.warm(rt, size)
                    return 503, json.dumps({"error": "model loading", "retryAfterMs": rt.retry_ms(), "state": S_LOADING, "percent": 100}).encode(), "application/json"
                try:
                    self._create_resident(rt, size)  # released by the janitor / unload between create and run → retry
                except Exception as e:  # noqa: BLE001
                    return self._failed(rt, size, e, creating=True)
            return 503, json.dumps({"error": "model loading", "retryAfterMs": 1000, "state": rt.state}).encode(), "application/json"
        finally:
            with self.lock:
                self.busy -= 1

    def matte(self, rt: Runtime, size: int, frames: int, body: bytes) -> tuple[int, bytes, str]:
        t0 = time.perf_counter()
        timing = {}

        def run(sess):
            t1 = time.perf_counter()
            pngs = self._run_batch(rt, sess, body, frames, size)
            timing["run_s"] = time.perf_counter() - t1
            return pngs

        r = self._run(rt, size, run)
        if isinstance(r, tuple):
            return r
        pngs: list[bytes] = r
        payload = b"".join(struct.pack(">I", len(p)) + p for p in pngs) + b"\0\0\0\0"
        total = time.perf_counter() - t0  # the whole request: a session re-created on demand shows up here only
        per = timing["run_s"] * 1000.0 / frames  # the per-frame path (pre-processing, inference, PNG): what the estimate needs
        self._note_ms(rt, size, per)
        log.info("matte model=%s device=%s frames=%d size=%d ms_per_frame=%.1f total_s=%.2f%s", rt.id, rt.device, frames, size, per, total, self._vram())
        return 200, payload, MATTES_CONTENT_TYPE

    def track(self, rt: Runtime, w: int, h: int, frames: int, body: bytes, obj: int, prompts: dict[int, dict]) -> tuple[int, bytes, str]:
        t0 = time.perf_counter()
        size = rt.default_size
        r = self._run(rt, size, lambda tracker: self._run_track(rt, tracker, body, frames, w, h, prompts, obj))
        if isinstance(r, tuple):
            return r
        pngs: list[bytes] = r
        payload = b"".join(struct.pack(">I", len(p)) + p for p in pngs) + b"\0\0\0\0"
        total = time.perf_counter() - t0
        per = total * 1000.0 / frames  # the whole pass per frame (frame prep + conditioning + propagation): what the estimate needs
        self._note_ms(rt, size, per)
        masked = [fi for fi, p in prompts.items() if p.get("mask") is not None]
        log.info("track model=%s device=%s frames=%d size=%dx%d prompts=%d mask=%s ms_per_frame=%.1f total_s=%.2f%s", rt.id, rt.device, frames, w, h, len(prompts), masked[0] if masked else "-", per, total, self._vram())
        return 200, payload, MATTES_CONTENT_TYPE

    def track_frame(self, rt: Runtime, w: int, h: int, body: bytes, obj: int, prompts: dict[int, dict]) -> tuple[int, bytes, str]:
        t0 = time.perf_counter()
        (prompt,) = prompts.values()

        def run(tracker):
            m = np.asarray(tracker.track_frame(body, w, h, prompt, obj))
            if m.shape != (h, w):
                raise RuntimeError(f"tracker mask has shape {m.shape}, expected ({h}, {w})")
            return png.encode_gray(np.where(m.astype(bool), 255, 0).astype(np.uint8))

        r = self._run(rt, rt.default_size, run, lock_wait=FRAME_LOCK_WAIT_S)
        if isinstance(r, tuple):
            return r
        log.info("track/frame model=%s device=%s size=%dx%d points=%d box=%s mask=%s ms=%.0f", rt.id, rt.device, w, h, len(prompt["points"]), prompt["box"] is not None, prompt.get("mask") is not None, (time.perf_counter() - t0) * 1000.0)
        return 200, r, "image/png"

    def _note_ms(self, rt: Runtime, size: int, per: float) -> None:
        with self.lock:
            old = rt.ms.get(size)
            rt.ms[size] = per if old is None else 0.7 * old + 0.3 * per

    def _vram(self) -> str:
        return f" vram_gb={self.gpu['totalGiB'] - self.gpu['freeGiB']:.2f}" if D_CUDA in self.devices and self.gpu else ""

    def _failed(self, rt: Runtime, size: int, e: BaseException, creating: bool) -> tuple[int, bytes, str]:
        what = "session create" if creating else "inference"
        if not is_oom(e):
            log.error("%s@%d %s failed: %s", rt.name, size, what, short(e, 400))
            rt.last_error = short(e, 400)
            return 500, json.dumps({"error": f"{what} failed: {short(e)}"}).encode(), "application/json"
        self._release(rt)
        freed = self._unload_others(rt, blocking=False)
        need = (rt.model.spec.get("vram") or {}).get("min_free_gib") if rt.device == D_CUDA else None
        free = f", {self.gpu['freeGiB']:.1f} GB free" if self.gpu and rt.device == D_CUDA else ""
        rt.last_error = short(e, 400)
        if freed:
            log.warning("%s@%d: out of memory during %s; released %d other model(s), retry", rt.name, size, what, freed)
            return 503, json.dumps({"error": "out of memory", "retryAfterMs": OOM_RETRY_MS}).encode(), "application/json"
        why = f"needs about {need:g} GB of free GPU memory{free}" if need else f"out of memory during {what}{free}"
        self.set_state(rt, S_UNAVAILABLE, why)
        log.error("%s@%d: out of memory during %s, nothing to release; unavailable until re-tested (%s)", rt.name, size, what, why)
        self.warm(rt)  # re-test before it is offered again
        return 507, json.dumps({"error": f"out of memory: {rt.model.label} {why}"}).encode(), "application/json"

    def unload(self, ids: list[str] | None = None, device: str | None = None) -> tuple[list[Runtime], list[Runtime]]:
        """Release the sessions of the matching runtimes now → (released, busy). A runtime whose lock is held by a request
        (a multi-minute CPU pass, a track) is never waited for beyond UNLOAD_LOCK_WAIT_S: clearing its sessions mid-pass
        would only make that pass reload its model; it is reported busy and the janitor's TTL releases it later."""
        done, busy = [], []
        for rt in self.runtimes():
            if (ids and rt.id not in ids) or (device and rt.device != device):
                continue
            if not rt.lock.acquire(timeout=UNLOAD_LOCK_WAIT_S):
                if rt.sessions:
                    busy.append(rt)
                continue
            try:
                if rt.sessions:
                    rt.sessions.clear()
                    done.append(rt)
            finally:
                rt.lock.release()
        if done:
            self._collect()
            log.info("unloaded %s", ",".join(rt.name for rt in done))
        if busy:
            log.info("unload: %s busy with a request, left resident (MATTE_MODEL_TTL releases it)", ",".join(rt.name for rt in busy))
        return done, busy

    # -- background threads ------------------------------------------------------------
    def _janitor(self) -> None:
        while not self.stop.wait(TTL_TICK_S):
            ttl = self.cfg.model_ttl
            if ttl <= 0:
                continue
            now = time.monotonic()
            freed = []
            for rt in self.runtimes():
                if rt.sessions and now - rt.last_used > ttl and rt.lock.acquire(blocking=False):
                    try:
                        if rt.sessions and now - rt.last_used > ttl:
                            rt.sessions.clear()
                            freed.append(rt.name)
                    finally:
                        rt.lock.release()
            if freed:
                self._collect()
                log.info("released %s after %.0f s idle (MATTE_MODEL_TTL)", ",".join(freed), ttl)

    def _gpu_sampler(self) -> None:
        while not self.stop.wait(GPU_SAMPLE_S):
            self.sample_gpu()


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------
class MatteServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 32
    app: App


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    timeout = SOCKET_TIMEOUT
    server_version = "ezlg-matte/" + PROCESSING_VERSION
    sys_version = ""

    @property
    def app(self) -> App:
        return self.server.app  # type: ignore[attr-defined]

    def log_message(self, fmt, *args):  # the request log line is written by App.matte / track; keep http.server quiet
        log.debug("http %s " + fmt, self.address_string(), *args)

    def _send(self, status: int, body: bytes, ctype: str, close: bool = False) -> None:
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        if close:
            self.send_header("Connection", "close")
            self.close_connection = True
        self.end_headers()
        self.wfile.write(body)

    def _json(self, status: int, obj: dict, close: bool = False) -> None:
        self._send(status, json.dumps(obj).encode(), "application/json", close)

    def _read_exact(self, n: int) -> bytes | None:
        chunks, got = [], 0
        while got < n:
            c = self.rfile.read(min(1 << 20, n - got))
            if not c:
                return None
            chunks.append(c)
            got += len(c)
        return b"".join(chunks)

    def _drain(self, limit: int = MAX_BODY_BYTES) -> bool:
        """Consume the request body before an error reply so the client reads the status instead of a reset while it is
        still sending (and the connection can be reused); False (and close) when it is over the limit."""
        try:
            n = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            n = 0
        if n <= 0:
            return True
        if n > limit:
            self.close_connection = True
            return False
        self._read_exact(n)
        return True

    def _resolve(self, q: dict, kind: str | None = None) -> tuple[Runtime | None, tuple[int, dict] | None]:
        """The runtime a request names (model + device, both optional) or the (status, body) that refuses it."""
        app = self.app
        if app.device == D_UNAVAILABLE:
            return None, (503, {"error": app.device_reason, "state": D_UNAVAILABLE})
        dev = (q.get("device", [""])[0] or "").strip().lower() or app.default_device
        if dev not in app.devices:
            return None, (404, {"error": f"device {dev} not offered", "devices": list(app.devices)})
        mid = q.get("model", [""])[0] or app.default_model(dev)
        m = app.models.get(mid)
        if m is None:
            return None, (404, {"error": f"unknown model {mid!r}; offered: {sorted(app.models)}"})
        rt = m.runtimes.get(dev)
        if rt is None:
            return None, (404, {"error": f"model {mid!r} is not offered on device {dev}; devices: {sorted(m.runtimes)}"})
        if kind and m.kind != kind:
            other = "/v1/track" if m.kind == K_TRACKER else "/v1/matte"
            return None, (400, {"error": f"model {mid!r} is a {m.kind}: use {other}"})
        return rt, None

    def do_GET(self):
        u = urlsplit(self.path)
        if u.path == "/v1/ping":
            status, snap = self.app.ping()
            return self._json(status, snap)
        self._json(404, {"error": "not found"})

    def do_POST(self):
        u = urlsplit(self.path)
        q = parse_qs(u.query)
        if u.path == "/v1/matte":
            return self._matte(q)
        if u.path == "/v1/track":
            return self._track(q)
        if u.path == "/v1/track/frame":
            return self._track_frame(q)
        keep = self._drain()
        if u.path == "/v1/warm":
            rt, err = self._resolve(q)
            if err:
                return self._json(err[0], err[1], close=not keep)
            assert rt is not None
            if not (rt.state == S_READY and rt.sessions):
                self.app.warm(rt)
            return self._json(200, {"model": rt.id, "device": rt.device, "state": rt.state, "resident": bool(rt.sessions)}, close=not keep)
        if u.path == "/v1/unload":
            ids = q.get("model")
            dev = (q.get("device", [""])[0] or "").strip().lower() or None
            if ids and any(i not in self.app.models for i in ids):
                return self._json(404, {"error": f"unknown model {ids!r}"}, close=not keep)
            if dev and dev not in self.app.devices:
                return self._json(404, {"error": f"device {dev} not offered", "devices": list(self.app.devices)}, close=not keep)
            done, busy = self.app.unload(ids, dev)
            return self._json(200, {"unloaded": sorted({rt.id for rt in done}), "sessions": [{"model": rt.id, "device": rt.device} for rt in done], "busy": [{"model": rt.id, "device": rt.device} for rt in busy]}, close=not keep)
        self._json(404, {"error": "not found"}, close=not keep)

    def _ints(self, q: dict, names: list[str]) -> list[int] | None:
        try:
            return [int(q.get(n, ["0"])[0]) for n in names]
        except ValueError:
            return None

    def _content_length(self) -> int | None:
        try:
            return int(self.headers.get("Content-Length") or "")
        except ValueError:
            return None

    def _matte(self, q: dict) -> None:
        app = self.app
        rt, err = self._resolve(q, K_SEGMENTER)
        if err:
            return self._json(err[0], err[1], close=not self._drain())
        assert rt is not None
        ints = self._ints(q, ["frames", "size"])
        if ints is None:
            return self._json(400, {"error": "frames and size must be integers"}, close=not self._drain())
        frames, size = ints[0], ints[1] or rt.default_size
        if frames < 1:
            return self._json(400, {"error": "frames must be >= 1"}, close=not self._drain())
        if frames > app.cfg.max_batch:
            return self._json(413, {"error": f"frames={frames} exceeds MATTE_MAX_BATCH={app.cfg.max_batch}"}, close=not self._drain())
        if size not in rt.sizes:
            return self._json(404, {"error": f"model {rt.id!r} has no size {size} on {rt.device}; sizes: {rt.sizes}"}, close=not self._drain())
        cl = self._content_length()
        if cl is None:
            return self._json(400, {"error": "Content-Length required"}, close=True)
        want = frames * size * size * 3
        if cl > MAX_BODY_BYTES:
            return self._json(413, {"error": f"body of {cl} bytes exceeds {MAX_BODY_BYTES} bytes"}, close=True)
        if cl != want:
            return self._json(400, {"error": f"Content-Length {cl} != frames×size×size×3 = {want}"}, close=not self._drain())
        gate = app.gate(rt)
        if gate:
            return self._json(gate[0], gate[1], close=not self._drain())
        body = self._read_exact(cl)
        if body is None:
            return self._json(400, {"error": f"short body: expected {cl} bytes"}, close=True)
        try:
            status, payload, ctype = app.matte(rt, size, frames, body)
        except Exception as e:  # noqa: BLE001 - a bug must answer 500, never close the connection without a response
            log.exception("matte %s@%d: %s", rt.name, size, short(e))
            return self._json(500, {"error": f"internal error: {short(e)}"})
        self._send(status, payload, ctype)

    def _track_common(self, q: dict, single: bool):
        """Shared validation of /v1/track and /v1/track/frame → (rt, w, h, frames, body, obj, prompts) or None after a reply."""
        app = self.app
        rt, err = self._resolve(q, K_TRACKER)
        if err:
            self._json(err[0], err[1], close=not self._drain())
            return None
        assert rt is not None
        ints = self._ints(q, ["w", "h", "frames"])
        if ints is None:
            self._json(400, {"error": "w, h and frames must be integers"}, close=not self._drain())
            return None
        w, h, frames = ints
        if single:
            frames = 1
        if w < 1 or h < 1 or w > 8192 or h > 8192:
            self._json(400, {"error": "w and h must be within 1..8192"}, close=not self._drain())
            return None
        if frames < 1:
            self._json(400, {"error": "frames must be >= 1"}, close=not self._drain())
            return None
        if frames > app.cfg.max_track_frames:
            self._json(413, {"error": f"frames={frames} exceeds MATTE_MAX_TRACK_FRAMES={app.cfg.max_track_frames}"}, close=not self._drain())
            return None
        cl = self._content_length()
        if cl is None:
            self._json(400, {"error": "Content-Length required"}, close=True)
            return None
        want = frames * w * h * 3
        if cl > MAX_TRACK_BODY_BYTES:
            self._json(413, {"error": f"body of {cl} bytes exceeds {MAX_TRACK_BODY_BYTES} bytes"}, close=True)
            return None
        try:
            obj, prompts, mask_fi = parse_prompts(self.headers.get(PROMPTS_HEADER), frames, w, h, single)
        except PromptError as e:
            self._json(400, {"error": str(e)}, close=not self._drain())
            return None
        # the body: the frames, then — for a mask prompt only — one [uint32 BE length][PNG] record, nothing else
        extra = cl - want
        if mask_fi is None and extra != 0:
            self._json(400, {"error": f"Content-Length {cl} != frames×w×h×3 = {want}" + (" (a mask record needs a mask prompt in the header)" if extra > 4 else "")}, close=not self._drain())
            return None
        if mask_fi is not None and extra < 4 + len(png.SIGNATURE):
            self._json(400, {"error": f"Content-Length {cl}: the mask prompt on frame {mask_fi} needs its record — frames×w×h×3 = {want} bytes of frames, then [uint32 length][PNG]"}, close=not self._drain())
            return None
        if mask_fi is not None and extra - 4 > mask_record_cap(w, h):
            self._json(400, {"error": f"the mask record of {extra - 4} bytes exceeds {mask_record_cap(w, h)} bytes, the most a {w}×{h} gray PNG takes"}, close=not self._drain())
            return None
        gate = app.gate(rt)
        if gate:
            self._json(gate[0], gate[1], close=not self._drain())
            return None
        body = self._read_exact(want)
        if body is None:
            self._json(400, {"error": f"short body: expected {cl} bytes"}, close=True)
            return None
        if mask_fi is not None:
            rec = self._read_exact(extra)
            if rec is None:
                self._json(400, {"error": f"short body: expected {cl} bytes (the mask record after the frames)"}, close=True)
                return None
            try:
                prompts[mask_fi]["mask"] = parse_mask_record(rec, w, h)
            except PromptError as e:
                self._json(400, {"error": str(e)})
                return None
        return rt, w, h, frames, body, obj, prompts

    def _track(self, q: dict) -> None:
        r = self._track_common(q, single=False)
        if r is None:
            return
        rt, w, h, frames, body, obj, prompts = r
        try:
            status, payload, ctype = self.app.track(rt, w, h, frames, body, obj, prompts)
        except Exception as e:  # noqa: BLE001
            log.exception("track %s: %s", rt.name, short(e))
            return self._json(500, {"error": f"internal error: {short(e)}"})
        self._send(status, payload, ctype)

    def _track_frame(self, q: dict) -> None:
        r = self._track_common(q, single=True)
        if r is None:
            return
        rt, w, h, _frames, body, obj, prompts = r
        try:
            status, payload, ctype = self.app.track_frame(rt, w, h, body, obj, prompts)
        except Exception as e:  # noqa: BLE001
            log.exception("track/frame %s: %s", rt.name, short(e))
            return self._json(500, {"error": f"internal error: {short(e)}"})
        self._send(status, payload, ctype)


# ---------------------------------------------------------------------------
# subcommands
# ---------------------------------------------------------------------------
def cmd_serve(cfg: Config) -> int:
    app = App(cfg)
    app.start(serve=True)

    def stop(signum, _frame):
        log.info("signal %d: shutting down", signum)
        threading.Thread(target=app.shutdown, name="shutdown", daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        app.serve_forever()
    finally:
        app.stop.set()
    return 0


def cmd_download(cfg: Config, ids: list[str]) -> int:
    reg = load_registry(cfg.registry_path)
    # A bare `download` fetches EVERY entry of models.json, not the service's
    # MATTE_MODELS: pre-seeding (an air-gapped box, a volume copied to the
    # GPU host) wants everything the volume may ever need, and the cpu
    # service — the one `docker compose run --rm matte download` reaches —
    # may offer fewer models than the gpu one. Name ids to narrow it.
    want = ids or list(reg)
    rc = 0
    os.makedirs(cfg.models_dir, exist_ok=True)
    for mid in want:
        if mid not in reg:
            print(f"unknown model {mid!r}; models.json has {sorted(reg)}", file=sys.stderr)
            rc = 1
            continue
        m = Model(mid, reg[mid], cfg)
        if os.path.isfile(m.file):
            got = sha256_file(m.file)
            if got == m.weights:
                print(f"{mid}: present and verified ({m.file})")
                continue
            print(f"{mid}: {m.file} has sha256 {got[:16]}…, want {m.weights[:16]}… — replacing", file=sys.stderr)
            os.remove(m.file)
        last = [0]

        def progress(n: int, _mid=mid, _t=m.bytes, _last=last):
            pct = int(100 * n / _t) if _t else 0
            if pct // 10 != _last[0] // 10:
                print(f"{_mid}: {pct}%", flush=True)
            _last[0] = pct

        try:
            download_model(m.urls, m.file, m.weights, m.bytes, progress)
            print(f"{mid}: downloaded and verified ({m.file})")
        except Exception as e:  # noqa: BLE001
            print(f"{mid}: download failed: {short(e)}", file=sys.stderr)
            rc = 1
    return rc


def _selftest_tracker_plumbing(device: str, threads: int) -> str:
    """Build-time check of the tracker path without weights: a random-initialised hiera-tiny at 256² on the CPU through
    `track` (4 synthetic frames, a box prompt) and `track_frame`; verifies the imports (torch, sam2, hydra config, scipy
    hole filling), the frame loader and the PNG framing — never the masks."""
    t0 = time.perf_counter()
    tr = Sam2Tracker({"config": "configs/sam2.1/sam2.1_hiera_t.yaml"}, device, None, 256, "fp32", threads)
    w, h = 64, 48
    body, boxes = synthetic_track_clip(4, w, h)
    prompt = {0: {"points": [(float(w // 2), float(h // 2))], "labels": [1], "box": tuple(float(v) for v in boxes[0])}}
    masks = tr.track(body, 4, w, h, prompt)
    if len(masks) != 4 or any(np.asarray(m).shape != (h, w) for m in masks):
        raise RuntimeError("tracker plumbing: wrong mask count or shape")
    one = tr.track_frame(body[: w * h * 3], w, h, prompt[0])
    if np.asarray(one).shape != (h, w):
        raise RuntimeError("tracker plumbing: wrong frame mask shape")
    png.decode_gray(png.encode_gray(np.where(np.asarray(one), 255, 0).astype(np.uint8)))
    # the mask prompt (add_new_mask) and its refinement by a point (the prev_sam_mask_logits override, Phase 5d)
    masked = {"points": [(float(w // 2), float(h // 2))], "labels": [0], "box": None, "mask": box_mask(boxes[0], w, h)}
    if np.asarray(tr.track_frame(body[: w * h * 3], w, h, masked)).shape != (h, w):
        raise RuntimeError("tracker plumbing: wrong mask-prompt frame shape")
    return f"tracker plumbing ok (random weights, 256px, {time.perf_counter() - t0:.1f} s)"


def cmd_selftest(cfg: Config) -> int:
    """Build-time check: a synthetic 8×8 graph through derivation (fp16 + 16²), sessions, the batch path and the PNG codec;
    the tracker plumbing when torch + sam2 import; the real tracker self-test when its checkpoint is in MATTE_MODELS_DIR."""
    tmp = tempfile.mkdtemp(prefix="ezlg-matte-selftest-")
    src = os.path.join(tmp, "synthetic.onnx")
    build_synthetic_graph(src)
    reg_real = load_registry(cfg.registry_path)
    trackers = {mid: s for mid, s in reg_real.items() if (s.get("kind") or K_SEGMENTER) == K_TRACKER and os.path.isfile(os.path.join(cfg.models_dir, s["file"]))}
    for mid, s in trackers.items():
        import shutil

        shutil.copy(os.path.join(cfg.models_dir, s["file"]), os.path.join(tmp, s["file"]))
    cfg.models_dir = tmp
    cfg.preload_csv = ""
    cfg.selftest_at_start = False
    cfg.models_csv = ",".join(["synthetic", "synthetic-fp32", *trackers])
    cfg.default_model = "synthetic"
    reg = {"synthetic": synthetic_spec(src, "fp16"), "synthetic-fp32": synthetic_spec(src, "fp32"), **trackers}
    app = App(cfg, reg)
    app.start(serve=False)
    if app.device == D_UNAVAILABLE:
        print(f"selftest: device unavailable: {app.device_reason}", file=sys.stderr)
        return 1
    for rt in app.runtimes():
        app._load(rt)
        if rt.state != S_READY:
            print(f"selftest: {rt.name} {rt.state}: {rt.reason}", file=sys.stderr)
            return 1
    rng = np.random.default_rng(7)
    frames = 8
    base = np.zeros((frames, SELFTEST_SIZE, SELFTEST_SIZE, 3), np.uint8)
    base[:, 2:6, 2:6, :] = 255
    base += rng.integers(0, 40, base.shape, dtype=np.uint8) // 2
    body = base.tobytes()
    results = {}
    for rt in app.runtimes():
        if rt.is_tracker:
            continue
        for size in rt.sizes:
            b = body if size == SELFTEST_SIZE else np.repeat(np.repeat(base, size // SELFTEST_SIZE, 1), size // SELFTEST_SIZE, 2).tobytes()
            status, payload, ctype = app.matte(rt, size, frames, b)
            if status != 200:
                print(f"selftest: {rt.name}@{size}: HTTP {status} {payload[:200]!r}", file=sys.stderr)
                return 1
            recs = parse_records(payload)
            if len(recs) != frames:
                print(f"selftest: {rt.name}@{size}: {len(recs)} records, expected {frames}", file=sys.stderr)
                return 1
            mats = [png.decode_gray(r) for r in recs]
            if any(a.shape != (size, size) for a in mats):
                print(f"selftest: {rt.name}@{size}: bad matte shape", file=sys.stderr)
                return 1
            results[(rt.name, size)] = np.stack(mats)
    for dev in app.devices:
        diff = np.abs(results[(f"synthetic@{dev}", SELFTEST_SIZE)].astype(np.int16) - results[(f"synthetic-fp32@{dev}", SELFTEST_SIZE)].astype(np.int16)).mean() / 255.0
        if diff > 0.02:
            print(f"selftest: {dev}: fp16 vs fp32 MAE {diff:.4f} > 0.02", file=sys.stderr)
            return 1
    track_note = "tracker plumbing skipped (torch or sam2 not importable)"
    if import_torch() is not None and importlib.util.find_spec("sam2") is not None:
        try:
            track_note = _selftest_tracker_plumbing(D_CPU, cfg.threads or physical_cores())
        except Exception as e:  # noqa: BLE001
            print(f"selftest: {short(e, 600)}", file=sys.stderr)
            return 1
    for rt in app.runtimes():
        if rt.is_tracker:
            w, h = TRACK_SELFTEST_WH
            body_t, boxes = synthetic_track_clip(4, w, h)
            prompts = {0: {"points": [], "labels": [], "box": tuple(float(v) for v in boxes[0])}}
            status, payload, _ = app.track(rt, w, h, 4, body_t, 1, prompts)
            if status != 200 or len(parse_records(payload)) != 4:
                print(f"selftest: {rt.name}: HTTP {status} {payload[:200]!r}", file=sys.stderr)
                return 1
            masked = {0: {"points": [], "labels": [], "box": None, "mask": box_mask(boxes[0], w, h)}}
            status, payload, _ = app.track(rt, w, h, 4, body_t, 1, masked)
            if status != 200 or len(parse_records(payload)) != 4:
                print(f"selftest: {rt.name} (mask prompt): HTTP {status} {payload[:200]!r}", file=sys.stderr)
                return 1
            track_note += f"; {rt.name} real self-test ok ms_per_frame={rt.ms.get(rt.default_size, 0):.0f}"
    first = app.runtimes()[0]
    print(f"selftest ok: devices={app.devices} ms_per_frame={first.ms} sizes={first.sizes} threads={cfg.threads or physical_cores()}; {track_note}")
    app.stop.set()
    return 0


def parse_records(payload: bytes) -> list[bytes]:
    """The application/x-ezlg-mattes framing: [uint32 BE length][PNG] … then a zero length."""
    out, pos = [], 0
    while True:
        if pos + 4 > len(payload):
            raise ValueError("truncated record stream")
        (n,) = struct.unpack(">I", payload[pos : pos + 4])
        pos += 4
        if n == 0:
            if pos != len(payload):
                raise ValueError("trailing bytes after the terminator")
            return out
        rec = payload[pos : pos + n]
        if len(rec) != n:
            raise ValueError("truncated record")
        out.append(rec)
        pos += n


def main(argv: list[str]) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s", stream=sys.stderr)
    cmd = argv[1] if len(argv) > 1 else "serve"
    cfg = Config()
    if cmd == "serve":
        return cmd_serve(cfg)
    if cmd == "download":
        return cmd_download(cfg, argv[2:])
    if cmd == "selftest":
        return cmd_selftest(cfg)
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
