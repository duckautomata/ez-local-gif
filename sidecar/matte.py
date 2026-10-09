#!/usr/bin/env python3
"""ez-local-gif matte sidecar — ONNX background-removal models behind a tiny HTTP/1.1 API.

One process on the stdlib ``http.server.ThreadingHTTPServer`` (one thread per
request), onnxruntime and numpy. The app (``internal/jobs``) streams rgb24
frames already stretched to the model's input square and gets one 8-bit gray
PNG matte per frame back; decoding, resizing, the memo and the merge are the
app's and ffmpeg's job. See docs/background-removal-proposal.md §7.

API (compose network only — no auth, never publish the port):

  GET  /v1/ping                      state snapshot, never behind the inference lock.
                                     200; 503 with the same body plus "error" when the device is unavailable
                                     (MATTE_DEVICE=cuda without a working CUDA EP) so the healthcheck fails.
  POST /v1/matte?model=&size=&frames= body: frames × size × size × 3 bytes of rgb24, row-major; Content-Length exact.
                                     200 application/x-ezlg-mattes: frames records [uint32 BE length][PNG], then uint32 0.
                                     400 bad params / length mismatch · 404 unknown model or size · 413 frames > MATTE_MAX_BATCH
                                     or body > 128 MiB · 503 {"error":"model loading","retryAfterMs":N} while the model is
                                     downloading / loading, {"error":"model unavailable: …"} · 503 {"error":"out of memory",
                                     "retryAfterMs":N} when unloading another model freed memory · 507 {"error":"out of
                                     memory: …"} when nothing could be freed (the model is re-tested before it is offered again).
  POST /v1/warm?model=               download / derive / self-test / create the session now (200 with the state).
  POST /v1/unload[?model=]           release the sessions of one or every model (200).

Subcommands: ``serve`` (default) · ``download [id …]`` (fetch + verify into /models, no onnxruntime needed; for
air-gapped hosts pre-seed the volume — with no ids EVERY model of models.json, whatever MATTE_MODELS says, so the
volume serves the cpu and the cuda service alike) · ``selftest`` (build-time check through a synthetic 8×8 graph).

Environment (defaults):
  MATTE_DEVICE=auto             auto | cuda | cpu. cuda without a working CUDA EP stays up and reports device "unavailable".
  MATTE_MODELS=<csv>            ids from models.json to offer (default: the ids models.json offers on the device).
  MATTE_DEFAULT_MODEL=isnet-anime
  MATTE_PRELOAD=<csv>           models downloaded + self-tested at start (default MATTE_MODELS; "" = none).
  MATTE_MODEL_TTL=600           seconds idle before a model's sessions are released (0 = never).
  MATTE_GPU_MEM_LIMIT_GIB=6     CUDA arena cap for models that need one (birefnet-lite); lower values fail, it is a knob for
                                larger cards.
  MATTE_THREADS=0               intra-op threads (0 = physical cores, capped by the cgroup CPU quota).
  MATTE_MODELS_BASE_URL=        download <base>/<file> instead of the pinned URL list (an internal mirror).
  MATTE_PORT=9402  MATTE_BIND=0.0.0.0  MATTE_MAX_BATCH=32  MATTE_MODELS_DIR=/models  MATTE_VERSION=dev
"""
from __future__ import annotations

import gc
import hashlib
import importlib.util
import json
import logging
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
MAX_BODY_BYTES = 128 << 20
SOCKET_TIMEOUT = 30.0  # read / idle timeout per connection
WARMUP_FRAMES = 8  # the self-test's timed frames (msPerFrame)
MISSING_RETRY_S = 600.0  # a model still missing after the download attempts is retried this often
DOWNLOAD_ATTEMPTS = 6  # rounds over the URL list, exponential backoff between rounds (2 s … 60 s)
GPU_SAMPLE_S = 10.0  # nvidia-smi cadence for freeGiB (a background thread: /v1/ping never waits on it)
TTL_TICK_S = 5.0
SMALL_CARD_GIB = 10.0  # under this one model is resident at a time (the residency rule)
OOM_RETRY_MS = 3000
SELFTEST_SIZE = 8

S_READY, S_LOADING, S_DOWNLOADING, S_MISSING, S_UNAVAILABLE = "ready", "loading", "downloading", "missing", "unavailable"
D_CUDA, D_CPU, D_UNAVAILABLE = "cuda", "cpu", "unavailable"
CUDA_EP = "CUDAExecutionProvider"
CPU_EP = "CPUExecutionProvider"

# What an onnxruntime exception looks like when the device ran out of memory (CUDA arena, cuDNN/cuBLAS workspace, host).
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
        self.default_model = (env.get("MATTE_DEFAULT_MODEL") or "isnet-anime").strip()
        self.preload_csv = env.get("MATTE_PRELOAD")  # None = MATTE_MODELS
        self.model_ttl = float(env.get("MATTE_MODEL_TTL") or 600)
        self.gpu_mem_limit_gib = float(env.get("MATTE_GPU_MEM_LIMIT_GIB") or 6)
        self.threads = int(env.get("MATTE_THREADS") or 0)
        self.base_url = (env.get("MATTE_MODELS_BASE_URL") or "").strip().rstrip("/")
        self.max_batch = int(env.get("MATTE_MAX_BATCH") or 32)
        self.version = env.get("MATTE_VERSION") or "dev"
        self.registry_path = env.get("MATTE_REGISTRY") or os.path.join(HERE, "models.json")
        # test hooks
        self.session_factory = None  # (path, SessionOptions, providers) -> session-like object
        self.gpu_query = None  # () -> {"name","totalGiB","freeGiB"} | None

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


def synthetic_spec(path: str, precision: str = "fp32", sizes=(SELFTEST_SIZE, 2 * SELFTEST_SIZE)) -> dict:
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
        "precision": {D_CUDA: precision, D_CPU: precision},
        "rescalable": True,
        "input": {"name": "img", "mean": [0.485, 0.456, 0.406], "std": [1.0, 1.0, 1.0]},
        "output": {"name": "mask", "activation": "sigmoid"},
    }


# ---------------------------------------------------------------------------
# models
# ---------------------------------------------------------------------------
class Session:
    __slots__ = ("sess", "size", "in_name", "out_name", "in_dtype", "path", "created_ms")

    def __init__(self, sess, size: int, in_name: str, out_name: str, in_dtype, path: str, created_ms: float):
        self.sess, self.size, self.in_name, self.out_name, self.in_dtype, self.path, self.created_ms = sess, size, in_name, out_name, in_dtype, path, created_ms


class Model:
    def __init__(self, mid: str, spec: dict, cfg: Config, device: str):
        self.id = mid
        self.spec = spec
        self.label = spec.get("label") or mid
        self.licence = spec.get("licence", "")
        self.file = os.path.join(cfg.models_dir, spec["file"])
        self.bytes = int(spec.get("bytes") or 0)
        self.weights = spec["sha256"]
        self.urls = [cfg.base_url + "/" + spec["file"]] if cfg.base_url else list(spec.get("urls") or [])
        dev = device if device in (D_CUDA, D_CPU) else D_CPU
        prec = spec.get("precision", "fp32")
        self.precision = prec.get(dev, "fp32") if isinstance(prec, dict) else str(prec)
        self.graph_size = int(spec.get("graph_size") or spec["sizes"][0])
        self.sizes = [int(s) for s in spec["sizes"]]
        ds = spec.get("default_size", self.graph_size)
        self.default_size = int(ds.get(dev, self.graph_size) if isinstance(ds, dict) else ds)
        self.rescalable = bool(spec.get("rescalable", False))
        mean = np.asarray(spec["input"].get("mean", [0, 0, 0]), np.float32)
        std = np.asarray(spec["input"].get("std", [1, 1, 1]), np.float32)
        self.scale = (1.0 / (255.0 * std)).astype(np.float32)
        self.offset = (-mean / std).astype(np.float32)
        self.in_name = spec["input"].get("name")
        self.out_name = spec["output"].get("name")
        self.logits = spec["output"].get("activation", "sigmoid") == "logits"
        self.state = S_MISSING if not os.path.isfile(self.file) else S_LOADING
        # a present file not in MATTE_PRELOAD stays "loading" with this reason until a request or /v1/warm loads it
        self.reason = "" if self.state == S_MISSING else "not loaded yet — loads on first use"
        self.percent = 0
        self.last_error = ""
        self.graph_digest = ""
        self.ms: dict[int, float] = {}
        self.paths: dict[int, str] = {}
        self.prepared = False
        self.sessions: dict[int, Session] = {}
        self.lock = threading.Lock()  # one inference (or session create) at a time per model
        self.last_used = time.monotonic()
        self.queued = False

    def retry_ms(self) -> int:
        if self.state == S_DOWNLOADING:
            return 5000
        return 5000 if "birefnet" in self.id else 1500

    def snapshot(self) -> dict:
        return {
            "state": self.state,
            "reason": self.reason,
            "percent": int(self.percent),
            "weights": self.weights,
            "graphDigest": self.graph_digest,
            "precision": self.precision,
            "sizes": list(self.sizes),
            "defaultSize": self.default_size,
            "msPerFrame": {str(k): round(v, 2) for k, v in sorted(self.ms.items())},
            "licence": self.licence,
            "lastError": self.last_error,
            "label": self.label,
            "resident": bool(self.sessions),
        }


# ---------------------------------------------------------------------------
# the application: device, models, loader, HTTP
# ---------------------------------------------------------------------------
class App:
    def __init__(self, cfg: Config, registry: dict | None = None):
        self.cfg = cfg
        self.registry = registry if registry is not None else load_registry(cfg.registry_path)
        self.instance = secrets.token_hex(8)
        self.lock = threading.Lock()  # state publication (ping), busy count
        self.create_lock = threading.Lock()  # serialises session creation + room making (order: create_lock → model.lock)
        self.device = D_CPU
        self.device_reason = ""
        self.gpu: dict | None = None
        self.models: dict[str, Model] = {}
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

    def resolve_device(self) -> None:
        want = self.cfg.device_want
        if want == D_CPU:
            self.device = D_CPU
            return
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
            self.device = D_CUDA
            return
        except Exception as e:  # noqa: BLE001 - every failure is reported, never fatal
            reason = f"CUDA EP not available — nvidia-container-toolkit / driver >= 580? ({short(e)})"
        if want == D_CUDA:
            self.device, self.device_reason = D_UNAVAILABLE, reason
            log.error("%s; staying up with device=unavailable (MATTE_DEVICE=cuda)", reason)
        else:
            self.device, self.device_reason = D_CPU, reason
            log.warning("%s; falling back to the CPU (MATTE_DEVICE=auto)", reason)

    def build_models(self) -> None:
        dev = self.device if self.device in (D_CUDA, D_CPU) else D_CPU
        ids = Config.csv(self.cfg.models_csv) if self.cfg.models_csv is not None else [k for k, s in self.registry.items() if dev in (s.get("offer") or [D_CUDA, D_CPU])]
        for mid in ids:
            if mid not in self.registry:
                raise SystemExit(f"MATTE_MODELS: unknown model {mid!r}; models.json has {sorted(self.registry)}")
            self.models[mid] = Model(mid, self.registry[mid], self.cfg, dev)
        if not self.models:
            log.warning("no models offered (MATTE_MODELS is empty)")
        if self.cfg.default_model not in self.models and self.models:
            log.warning("MATTE_DEFAULT_MODEL=%s is not offered; using %s", self.cfg.default_model, next(iter(self.models)))
            self.cfg.default_model = next(iter(self.models))
        pre = Config.csv(self.cfg.preload_csv) if self.cfg.preload_csv is not None else list(self.models)
        self.preload = [m for m in pre if m in self.models]

    def start(self, serve: bool = True) -> None:
        os.makedirs(self.cfg.models_dir, exist_ok=True)
        self.resolve_device()
        self.sample_gpu()
        self.build_models()
        log.info("matte sidecar %s instance=%s device=%s models=%s default=%s preload=%s models_dir=%s gpu=%s", self.cfg.version, self.instance, self.device, ",".join(self.models) or "-", self.cfg.default_model, ",".join(self.preload) or "-", self.cfg.models_dir, self.gpu)
        if self.device_reason:
            log.warning("device reason: %s", self.device_reason)
        self._thread(self._loader, "loader")
        self._thread(self._janitor, "janitor")
        if self.device == D_CUDA:
            self._thread(self._gpu_sampler, "gpu")
        if self.device != D_UNAVAILABLE:
            present = [m for m in self.preload if os.path.isfile(self.models[m].file)]
            for mid in present + [m for m in self.preload if m not in present]:  # files on disk first: ready sooner
                self.warm(self.models[mid])
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

    # -- state -------------------------------------------------------------------
    def set_state(self, m: Model, state: str, reason: str = "", percent: int | None = None) -> None:
        with self.lock:
            if state != m.state or reason != m.reason:
                log.info("model %s: %s%s", m.id, state, f" ({reason})" if reason else "")
            m.state, m.reason = state, reason
            if percent is not None:
                m.percent = percent
            if state == S_UNAVAILABLE:
                m.prepared = False  # re-tested (derive + self-test) before it is offered again

    def ping(self) -> tuple[int, dict]:
        with self.lock:
            models = {mid: m.snapshot() for mid, m in self.models.items()}
            busy, gpu = self.busy, self.gpu
        obj = {
            "protocol": PROTOCOL,
            "version": self.cfg.version,
            "instance": self.instance,
            "processingVersion": PROCESSING_VERSION,
            "device": self.device,
            "reason": self.device_reason,
            "gpu": gpu,
            "defaultModel": self.cfg.default_model,
            "models": models,
            "busy": busy,
        }
        if self.device == D_UNAVAILABLE:
            obj["error"] = self.device_reason
            return 503, obj
        return 200, obj

    def small_card(self) -> bool:
        return self.device == D_CUDA and self.gpu is not None and self.gpu.get("totalGiB", 0) < SMALL_CARD_GIB

    # -- loader: download → derive → self-test → resident ---------------------------
    def warm(self, m: Model) -> None:
        """Queue download → derive → self-test → resident session for m (idempotent while it is queued or loading)."""
        with self.lock:
            if m.queued:
                return
            m.queued = True
        self.loader_q.put(m)

    def _loader(self) -> None:
        while True:
            m = self.loader_q.get()
            if m is None or self.stop.is_set():
                return
            try:
                self._load(m)
            except Exception as e:  # noqa: BLE001 - the loader thread must survive anything
                log.exception("loader: %s", short(e))
                self.set_state(m, S_UNAVAILABLE, short(e))
                m.last_error = short(e, 400)
            finally:
                with self.lock:
                    m.queued = False

    def _load(self, m: Model) -> None:
        if self.device == D_UNAVAILABLE:
            return
        if not os.path.isfile(m.file):
            self.set_state(m, S_DOWNLOADING, "", 0)
            total = m.bytes

            def progress(n: int, _m=m, _t=total):
                _m.percent = min(100, int(100 * n / _t)) if _t else 0

            try:
                download_model(m.urls, m.file, m.weights, total, progress, self.stop, attempts=DOWNLOAD_ATTEMPTS)
            except Exception as e:  # noqa: BLE001
                m.last_error = short(e, 400)
                self.set_state(m, S_MISSING, f"download failed: {short(e)}", 0)
                if not self.stop.is_set():
                    t = threading.Timer(MISSING_RETRY_S, self.warm, args=(m,))
                    t.daemon = True
                    t.start()
                return
            m.percent = 100
        if m.prepared:
            self._create_resident(m, m.default_size)  # a no-op when it is resident already
            return
        self._prepare(m)

    def _prepare(self, m: Model) -> None:
        self.set_state(m, S_LOADING, "preparing graphs")
        try:
            self._derive(m)
            if self.device == D_CUDA:
                self.sample_gpu()
            why = self._precheck(m)
            if why:
                self.set_state(m, S_UNAVAILABLE, why)
                return
            self.set_state(m, S_LOADING, "creating session")
            with self.create_lock:
                self._make_room(m)
                with m.lock:
                    sess = self._create_session(m, m.default_size)
                    self._selftest(m, sess)
            m.prepared = True
            self.set_state(m, S_READY, "")
            log.info("model %s ready: device=%s precision=%s size=%d graph=%s ms_per_frame=%.1f", m.id, self.device, m.precision, m.default_size, os.path.basename(m.paths[m.default_size]), m.ms.get(m.default_size, 0.0))
            if self.small_card() and m.id != self.cfg.default_model:
                self.unload([m.id])  # residency rule: only the default model stays resident after its self-test
        except Exception as e:  # noqa: BLE001
            self._release(m)
            m.last_error = short(e, 400)
            need = (m.spec.get("vram") or {}).get("min_free_gib")
            why = f"needs about {need:g} GB of free GPU memory" if is_oom(e) and need else short(e)
            if is_oom(e) and self.gpu:
                why += f", {self.gpu['freeGiB']:.1f} GB free"
            self.set_state(m, S_UNAVAILABLE, why)
            log.error("model %s unavailable: %s", m.id, why)

    def _precheck(self, m: Model) -> str | None:
        if self.device == D_CUDA:
            v = m.spec.get("vram") or {}
            if v.get("cap") and self.cfg.gpu_mem_limit_gib < float(v.get("min_limit_gib") or 0):
                return f"MATTE_GPU_MEM_LIMIT_GIB={self.cfg.gpu_mem_limit_gib:g} is below the {float(v['min_limit_gib']):g} GiB this model needs"
            need = float(v.get("min_free_gib") or 0)
            if need and self.gpu is not None and self.gpu["freeGiB"] < need:
                return f"needs about {need:g} GB of free GPU memory, {self.gpu['freeGiB']:.1f} GB free"
        else:
            r = m.spec.get("ram") or {}
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

    def _derive(self, m: Model) -> None:
        src = m.file
        base = src[:-5] if src.endswith(".onnx") else src
        cur = src
        if m.precision == "fp16":
            try:
                cur = self._derived(m, src, base + ".fp16.onnx", "to_fp16", lambda tool, s, d: tool.convert(s, d))
            except Exception as e:  # noqa: BLE001 - the fp32 source graph is the fallback
                log.error("model %s: fp16 derivation failed, using the fp32 source graph: %s", m.id, short(e))
                m.last_error = f"fp16 derivation failed: {short(e)}"
                m.precision, cur = "fp32", src
        paths: dict[int, str] = {}
        for size in list(m.sizes):
            if size == m.graph_size:
                paths[size] = cur
                continue
            if not m.rescalable:
                continue
            dst = cur[:-5] + f".{size}.onnx"
            try:
                paths[size] = self._derived(m, cur, dst, "rescale_graph", lambda tool, s, d, _n=size: tool.rescale(s, d, _n))
            except Exception as e:  # noqa: BLE001
                log.error("model %s: %d² derivation failed, size dropped: %s", m.id, size, short(e))
                m.last_error = f"{size}² derivation failed: {short(e)}"
        with self.lock:
            m.paths = paths
            m.sizes = [s for s in m.sizes if s in paths]
            if m.default_size not in paths:
                m.default_size = m.graph_size if m.graph_size in paths else m.sizes[0]
            m.graph_digest = self._digest(m, paths[m.default_size])

    def _providers(self, m: Model) -> list:
        if self.device != D_CUDA:
            return [CPU_EP]
        # ORT's defaults otherwise: arena_extend_strategy kNextPowerOfTwo (kSameAsRequested fragments the capped arena —
        # lite's self-test then fails with "available memory of 221 MB is smaller than requested 892 MB" at 6 GiB) and the
        # default cudnn_conv_algo_search (HEURISTIC + the cap fails in a deform-conv). This is the configuration the
        # research measured at a stable ~6.3 GB footprint under the 6 GiB cap.
        opts = {"device_id": "0"}
        if (m.spec.get("vram") or {}).get("cap"):
            opts["gpu_mem_limit"] = str(int(self.cfg.gpu_mem_limit_gib * (1 << 30)))
        return [(CUDA_EP, opts), CPU_EP]

    def _create_session(self, m: Model, size: int) -> Session:
        """Caller holds create_lock and m.lock."""
        path = m.paths[size]
        ort = self.ort
        so = ort.SessionOptions()
        so.log_severity_level = 3
        so.intra_op_num_threads = self.cfg.threads or physical_cores()
        so.add_session_config_entry("session.intra_op.allow_spinning", "0")
        providers = self._providers(m)
        t0 = time.perf_counter()
        sess = self.cfg.session_factory(path, so, providers) if self.cfg.session_factory else ort.InferenceSession(path, so, providers=providers)
        if self.device == D_CUDA and sess.get_providers()[0] != CUDA_EP:
            raise RuntimeError(f"CUDA EP not active for {m.id}: {sess.get_providers()}")
        inp, out = sess.get_inputs()[0], sess.get_outputs()[0]
        in_name, out_name = m.in_name or inp.name, m.out_name or out.name
        shape = list(inp.shape)
        if len(shape) == 4 and all(isinstance(d, int) for d in shape) and shape != [1, 3, size, size]:
            raise RuntimeError(f"{os.path.basename(path)}: input {inp.name}{shape} is not [1,3,{size},{size}]")
        in_dtype = np.float16 if "float16" in str(inp.type) else np.float32
        s = Session(sess, size, in_name, out_name, in_dtype, path, (time.perf_counter() - t0) * 1000.0)
        m.sessions[size] = s
        m.last_used = time.monotonic()
        log.info("session %s@%d created in %.0f ms (%s, %s)", m.id, size, s.created_ms, os.path.basename(path), sess.get_providers()[0])
        return s

    def _make_room(self, m: Model) -> int:
        """Caller holds create_lock. On a small card unload every other model before a session is created; returns the count."""
        if not self.small_card():
            return 0
        return self._unload_others(m, blocking=True)

    def _unload_others(self, m: Model, blocking: bool) -> int:
        n = 0
        for other in self.models.values():
            if other is m or not other.sessions:
                continue
            if other.lock.acquire(timeout=120.0 if blocking else 2.0):  # a bounded wait either way (never a deadlock)
                try:
                    if other.sessions:
                        other.sessions.clear()
                        n += 1
                        log.info("released %s to make room for %s", other.id, m.id)
                finally:
                    other.lock.release()
        if n:
            gc.collect()
        return n

    def _release(self, m: Model) -> None:
        with m.lock:
            m.sessions.clear()
        gc.collect()

    def _create_resident(self, m: Model, size: int) -> Session:
        """Create m's session for size unless it exists (lock order: create_lock → m.lock). Errors propagate; the state
        goes back to ready so a failed create never leaves the model stuck in loading — the caller maps the error."""
        with self.create_lock:
            with m.lock:
                s = m.sessions.get(size)
                if s is not None:
                    return s
            self._make_room(m)
            self.set_state(m, S_LOADING, "creating session")
            try:
                with m.lock:
                    s = self._create_session(m, size)
            finally:
                if m.state == S_LOADING:
                    self.set_state(m, S_READY, "")
            return s

    def _selftest(self, m: Model, sess: Session) -> None:
        size = sess.size
        rng = np.random.default_rng(1234)
        frame = rng.integers(0, 256, (size, size, 3), dtype=np.uint8).tobytes()
        first = self._run_batch(m, sess, frame, 1, size)  # untimed: cuDNN algorithm search, arena growth
        a = png.decode_gray(first[0])
        if a.shape != (size, size):
            raise RuntimeError(f"self-test: matte shape {a.shape} != ({size}, {size})")
        t0 = time.perf_counter()
        pngs = self._run_batch(m, sess, frame * WARMUP_FRAMES, WARMUP_FRAMES, size)
        per = (time.perf_counter() - t0) * 1000.0 / WARMUP_FRAMES
        if len(pngs) != WARMUP_FRAMES or any(p != pngs[0] for p in pngs):
            raise RuntimeError("self-test: the model is not deterministic per frame")
        with self.lock:
            m.ms[size] = per

    # -- inference ------------------------------------------------------------------
    def _run_batch(self, m: Model, sess: Session, body: bytes, frames: int, size: int) -> list[bytes]:
        x = np.frombuffer(body, dtype=np.uint8).reshape(frames, size, size, 3).astype(np.float32)
        x *= m.scale
        x += m.offset
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
            if m.logits:
                with np.errstate(over="ignore"):
                    a = 1.0 / (1.0 + np.exp(-a))
            np.clip(a, 0.0, 1.0, out=a)
            out.append(png.encode_gray((a * 255.0 + 0.5).astype(np.uint8)))
        return out

    def gate(self, m: Model) -> tuple[int, dict] | None:
        """Why a /v1/matte on this model cannot run right now (status, body), or None."""
        if self.device == D_UNAVAILABLE:
            return 503, {"error": self.device_reason, "state": D_UNAVAILABLE}
        st = m.state
        if st == S_READY:
            return None
        if st in (S_MISSING, S_LOADING, S_DOWNLOADING):
            if not m.prepared:
                self.warm(m)  # not preloaded, or a download that failed earlier: start it now (idempotent while queued)
            return 503, {"error": "model loading", "retryAfterMs": 5000 if st == S_MISSING else m.retry_ms(), "state": S_DOWNLOADING if st == S_MISSING else st, "percent": int(m.percent)}
        return 503, {"error": f"model unavailable: {m.reason}", "state": S_UNAVAILABLE}

    def matte(self, m: Model, size: int, frames: int, body: bytes) -> tuple[int, bytes, str]:
        t0 = time.perf_counter()
        with self.lock:
            self.busy += 1
        try:
            pngs: list[bytes] | None = None
            run_s = 0.0
            for _attempt in range(3):
                err: BaseException | None = None
                with m.lock:  # one inference at a time per model; /v1/ping never takes this lock
                    sess = m.sessions.get(size)
                    if sess is not None:
                        t1 = time.perf_counter()
                        try:
                            pngs = self._run_batch(m, sess, body, frames, size)
                        except Exception as e:  # noqa: BLE001 - mapped below, outside the lock
                            err = e
                        else:
                            run_s = time.perf_counter() - t1
                            m.last_used = time.monotonic()
                if sess is not None:
                    if err is not None:
                        return self._failed(m, size, err, creating=False)
                    break
                gate = self.gate(m)
                if gate:
                    return gate[0], json.dumps(gate[1]).encode(), "application/json"
                try:
                    self._create_resident(m, size)  # released by the janitor / unload between create and run → retry
                except Exception as e:  # noqa: BLE001
                    return self._failed(m, size, e, creating=True)
            if pngs is None:
                return 503, json.dumps({"error": "model loading", "retryAfterMs": 1000, "state": m.state}).encode(), "application/json"
            payload = b"".join(struct.pack(">I", len(p)) + p for p in pngs) + b"\0\0\0\0"
            total = time.perf_counter() - t0  # the whole request: a session re-created on demand shows up here only
            per = run_s * 1000.0 / frames  # the per-frame path (pre-processing, inference, PNG): what the estimate needs
            with self.lock:
                old = m.ms.get(size)
                m.ms[size] = per if old is None else 0.7 * old + 0.3 * per
            vram = f" vram_gb={self.gpu['totalGiB'] - self.gpu['freeGiB']:.2f}" if self.device == D_CUDA and self.gpu else ""
            log.info("matte model=%s device=%s frames=%d size=%d ms_per_frame=%.1f total_s=%.2f%s", m.id, self.device, frames, size, per, total, vram)
            return 200, payload, MATTES_CONTENT_TYPE
        finally:
            with self.lock:
                self.busy -= 1

    def _failed(self, m: Model, size: int, e: BaseException, creating: bool) -> tuple[int, bytes, str]:
        what = "session create" if creating else "inference"
        if not is_oom(e):
            log.error("matte %s@%d %s failed: %s", m.id, size, what, short(e, 400))
            m.last_error = short(e, 400)
            return 500, json.dumps({"error": f"{what} failed: {short(e)}"}).encode(), "application/json"
        self._release(m)
        freed = self._unload_others(m, blocking=False)
        need = (m.spec.get("vram") or {}).get("min_free_gib")
        free = f", {self.gpu['freeGiB']:.1f} GB free" if self.gpu else ""
        m.last_error = short(e, 400)
        if freed:
            log.warning("matte %s@%d: out of memory during %s; released %d other model(s), retry", m.id, size, what, freed)
            return 503, json.dumps({"error": "out of memory", "retryAfterMs": OOM_RETRY_MS}).encode(), "application/json"
        why = f"needs about {need:g} GB of free GPU memory{free}" if need else f"out of memory during {what}{free}"
        self.set_state(m, S_UNAVAILABLE, why)
        log.error("matte %s@%d: out of memory during %s, nothing to release; unavailable until re-tested (%s)", m.id, size, what, why)
        self.warm(m)  # re-test before it is offered again
        return 507, json.dumps({"error": f"out of memory: {m.label} {why}"}).encode(), "application/json"

    def unload(self, ids: list[str] | None = None) -> list[str]:
        done = []
        for m in ([self.models[i] for i in ids if i in self.models] if ids else list(self.models.values())):
            with m.lock:
                if m.sessions:
                    m.sessions.clear()
                    done.append(m.id)
        if done:
            gc.collect()
            log.info("unloaded %s", ",".join(done))
        return done

    # -- background threads ------------------------------------------------------------
    def _janitor(self) -> None:
        while not self.stop.wait(TTL_TICK_S):
            ttl = self.cfg.model_ttl
            if ttl <= 0:
                continue
            now = time.monotonic()
            freed = []
            for m in list(self.models.values()):
                if m.sessions and now - m.last_used > ttl and m.lock.acquire(blocking=False):
                    try:
                        if m.sessions and now - m.last_used > ttl:
                            m.sessions.clear()
                            freed.append(m.id)
                    finally:
                        m.lock.release()
            if freed:
                gc.collect()
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

    def log_message(self, fmt, *args):  # the request log line is written by App.matte; keep http.server quiet
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
        keep = self._drain()
        if u.path == "/v1/warm":
            mid = q.get("model", [self.app.cfg.default_model])[0]
            m = self.app.models.get(mid)
            if m is None:
                return self._json(404, {"error": f"unknown model {mid!r}; offered: {sorted(self.app.models)}"}, close=not keep)
            if self.app.device == D_UNAVAILABLE:
                return self._json(503, {"error": self.app.device_reason, "state": D_UNAVAILABLE}, close=not keep)
            if not (m.state == S_READY and m.sessions):
                self.app.warm(m)
            return self._json(200, {"model": m.id, "state": m.state, "resident": bool(m.sessions)}, close=not keep)
        if u.path == "/v1/unload":
            ids = q.get("model")
            if ids and any(i not in self.app.models for i in ids):
                return self._json(404, {"error": f"unknown model {ids!r}"}, close=not keep)
            return self._json(200, {"unloaded": self.app.unload(ids)}, close=not keep)
        self._json(404, {"error": "not found"}, close=not keep)

    def _matte(self, q: dict) -> None:
        app = self.app
        if app.device == D_UNAVAILABLE:
            return self._json(503, {"error": app.device_reason, "state": D_UNAVAILABLE}, close=not self._drain())
        mid = q.get("model", [app.cfg.default_model])[0]
        m = app.models.get(mid)
        if m is None:
            return self._json(404, {"error": f"unknown model {mid!r}; offered: {sorted(app.models)}"}, close=not self._drain())
        try:
            frames = int(q.get("frames", ["0"])[0])
            size = int(q.get("size", ["0"])[0]) or m.default_size
        except ValueError:
            return self._json(400, {"error": "frames and size must be integers"}, close=not self._drain())
        if frames < 1:
            return self._json(400, {"error": "frames must be >= 1"}, close=not self._drain())
        if frames > app.cfg.max_batch:
            return self._json(413, {"error": f"frames={frames} exceeds MATTE_MAX_BATCH={app.cfg.max_batch}"}, close=not self._drain())
        if size not in m.sizes:
            return self._json(404, {"error": f"model {mid!r} has no size {size}; sizes: {m.sizes}"}, close=not self._drain())
        try:
            cl = int(self.headers.get("Content-Length") or "")
        except ValueError:
            return self._json(400, {"error": "Content-Length required"}, close=True)
        want = frames * size * size * 3
        if cl > MAX_BODY_BYTES:
            return self._json(413, {"error": f"body of {cl} bytes exceeds {MAX_BODY_BYTES} bytes"}, close=True)
        if cl != want:
            return self._json(400, {"error": f"Content-Length {cl} != frames×size×size×3 = {want}"}, close=not self._drain())
        gate = app.gate(m)
        if gate:
            return self._json(gate[0], gate[1], close=not self._drain())
        body = self._read_exact(cl)
        if body is None:
            return self._json(400, {"error": f"short body: expected {cl} bytes"}, close=True)
        try:
            status, payload, ctype = app.matte(m, size, frames, body)
        except Exception as e:  # noqa: BLE001 - a bug must answer 500, never close the connection without a response
            log.exception("matte %s@%d: %s", m.id, size, short(e))
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
    # offers only isnet-anime, which would leave birefnet-lite "missing" on
    # the matte-gpu profile. Name ids to narrow it.
    want = ids or list(reg)
    rc = 0
    os.makedirs(cfg.models_dir, exist_ok=True)
    for mid in want:
        if mid not in reg:
            print(f"unknown model {mid!r}; models.json has {sorted(reg)}", file=sys.stderr)
            rc = 1
            continue
        m = Model(mid, reg[mid], cfg, D_CPU)
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


def cmd_selftest(cfg: Config) -> int:
    """Build-time check: a synthetic 8×8 graph through derivation (fp16 + 16²), sessions, the batch path and the PNG codec."""
    tmp = tempfile.mkdtemp(prefix="ezlg-matte-selftest-")
    src = os.path.join(tmp, "synthetic.onnx")
    build_synthetic_graph(src)
    cfg.models_dir = tmp
    cfg.preload_csv = ""
    cfg.models_csv = "synthetic,synthetic-fp32"
    cfg.default_model = "synthetic"
    reg = {"synthetic": synthetic_spec(src, "fp16"), "synthetic-fp32": synthetic_spec(src, "fp32")}
    app = App(cfg, reg)
    app.start(serve=False)
    if app.device == D_UNAVAILABLE:
        print(f"selftest: device unavailable: {app.device_reason}", file=sys.stderr)
        return 1
    for m in app.models.values():
        app._load(m)
        if m.state != S_READY:
            print(f"selftest: {m.id} {m.state}: {m.reason}", file=sys.stderr)
            return 1
    rng = np.random.default_rng(7)
    frames = 8
    base = np.zeros((frames, SELFTEST_SIZE, SELFTEST_SIZE, 3), np.uint8)
    base[:, 2:6, 2:6, :] = 255
    base += rng.integers(0, 40, base.shape, dtype=np.uint8) // 2
    body = base.tobytes()
    results = {}
    for mid, m in app.models.items():
        for size in m.sizes:
            b = body if size == SELFTEST_SIZE else np.repeat(np.repeat(base, size // SELFTEST_SIZE, 1), size // SELFTEST_SIZE, 2).tobytes()
            status, payload, ctype = app.matte(m, size, frames, b)
            if status != 200:
                print(f"selftest: {mid}@{size}: HTTP {status} {payload[:200]!r}", file=sys.stderr)
                return 1
            recs = parse_records(payload)
            if len(recs) != frames:
                print(f"selftest: {mid}@{size}: {len(recs)} records, expected {frames}", file=sys.stderr)
                return 1
            mats = [png.decode_gray(r) for r in recs]
            if any(a.shape != (size, size) for a in mats):
                print(f"selftest: {mid}@{size}: bad matte shape", file=sys.stderr)
                return 1
            results[(mid, size)] = np.stack(mats)
    diff = np.abs(results[("synthetic", SELFTEST_SIZE)].astype(np.int16) - results[("synthetic-fp32", SELFTEST_SIZE)].astype(np.int16)).mean() / 255.0
    if diff > 0.02:
        print(f"selftest: fp16 vs fp32 MAE {diff:.4f} > 0.02", file=sys.stderr)
        return 1
    print(f"selftest ok: device={app.device} fp16-vs-fp32 MAE={diff:.5f} ms_per_frame={app.models['synthetic'].ms} sizes={app.models['synthetic'].sizes} threads={cfg.threads or physical_cores()}")
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
