"""pytest fixtures: an in-process sidecar on 127.0.0.1:<random port> whose segmenter is a synthetic 8×8 ONNX graph
(matte.build_synthetic_graph) and whose tracker is a fake predictor (FakeTracker, through Config.tracker_factory),
so the suite needs no download, no GPU and only the cpu requirements + pytest. A fake onnxruntime whose CUDA
provider "works" (FakeCudaOrt) lets the two-device paths run on any box.

Run from the repo root:  python -I -m pytest sidecar/tests -q      (or from sidecar/: python -I -m pytest tests -q)
"""
from __future__ import annotations

import http.client
import json
import os
import shutil
import sys
import threading
import time

import numpy as np
import pytest

SIDECAR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if SIDECAR not in sys.path:
    sys.path.insert(0, SIDECAR)

import matte  # noqa: E402

png = matte.png
SIZE = matte.SELFTEST_SIZE
CUDA_EP, CPU_EP = matte.CUDA_EP, matte.CPU_EP


@pytest.fixture(scope="session")
def synthetic_file(tmp_path_factory) -> str:
    p = tmp_path_factory.mktemp("graph") / "synthetic.onnx"
    matte.build_synthetic_graph(str(p))
    return str(p)


def frames(n: int, size: int = SIZE, seed: int = 0) -> np.ndarray:
    """n structured rgb24 frames: a bright square on a dark gradient plus a little noise (clear 0/1 matte regions)."""
    rng = np.random.default_rng(seed)
    f = np.zeros((n, size, size, 3), np.uint8)
    ramp = np.linspace(10, 90, size).astype(np.uint8)
    f[:, :, :, 0] = ramp[None, None, :]
    f[:, :, :, 2] = ramp[None, :, None]
    q = max(1, size // 4)
    for i in range(n):
        f[i, q + i % 2 : 3 * q + i % 2, q : 3 * q, :] = 255
    f = np.clip(f.astype(np.int16) + rng.integers(0, 24, f.shape, dtype=np.int16), 0, 255).astype(np.uint8)
    return f


def clip(n: int, w: int = 24, h: int = 16, seed: int = 0) -> np.ndarray:
    """n rgb24 frames w×h for the tracker tests (contents are irrelevant to the fake tracker)."""
    rng = np.random.default_rng(seed)
    return rng.integers(0, 256, (n, h, w, 3), dtype=np.uint8)


def reference(frames_u8: np.ndarray, graph_path: str, mean=(0.485, 0.456, 0.406), std=(1.0, 1.0, 1.0), logits=False) -> np.ndarray:
    """What the sidecar must answer for these frames: the same maths through a plain onnxruntime session."""
    import onnxruntime as ort

    sess = ort.InferenceSession(graph_path, providers=["CPUExecutionProvider"])
    inp, out = sess.get_inputs()[0].name, sess.get_outputs()[0].name
    mean = np.asarray(mean, np.float32)
    std = np.asarray(std, np.float32)
    res = []
    for f in frames_u8:
        x = (f.astype(np.float32) / 255.0 - mean) / std
        x = np.ascontiguousarray(x.transpose(2, 0, 1)[None])
        y = sess.run([out], {inp: x})[0].reshape(f.shape[0], f.shape[1]).astype(np.float32)
        if logits:
            y = 1.0 / (1.0 + np.exp(-y))
        res.append((np.clip(y, 0, 1) * 255.0 + 0.5).astype(np.uint8))
    return np.stack(res)


# ---------------------------------------------------------------------------
# the fake tracker: deterministic masks from the prompts, "tracked" by shifting one pixel per frame
# ---------------------------------------------------------------------------
POINT_RADIUS = 2


def fake_mask(w: int, h: int, prompt: dict) -> np.ndarray:
    """(mask ∪ box ∪ discs of radius 2 around positive points) minus discs around negative points — the mask prompt (a
    bool (h, w) array under "mask", Phase 5d) first, then the box / points, the order the sidecar applies them in;
    coordinates in pixels."""
    mask = prompt.get("mask")
    m = np.array(mask, dtype=bool, copy=True) if mask is not None else np.zeros((h, w), bool)
    if prompt.get("box") is not None:
        x0, y0, x1, y1 = prompt["box"]
        m[int(round(y0)) : int(round(y1)) + 1, int(round(x0)) : int(round(x1)) + 1] = True
    yy, xx = np.mgrid[0:h, 0:w]
    for (x, y), lab in zip(prompt.get("points", []), prompt.get("labels", [])):
        disc = (xx - x) ** 2 + (yy - y) ** 2 <= POINT_RADIUS**2
        if lab == 1:
            m |= disc
        else:
            m &= ~disc
    return m


def shift_right(m: np.ndarray, dx: int) -> np.ndarray:
    out = np.zeros_like(m)
    if dx >= 0:
        out[:, dx:] = m[:, : m.shape[1] - dx] if dx < m.shape[1] else out[:, dx:]
    else:
        dx = -dx
        out[:, : m.shape[1] - dx] = m[:, dx:] if dx < m.shape[1] else out[:, : m.shape[1] - dx]
    return out


def fake_track(n: int, w: int, h: int, prompts: dict[int, dict]) -> list[np.ndarray]:
    """Frame i takes the mask of its nearest prompted frame (ties → the earlier one) shifted by i − that frame."""
    keys = sorted(prompts)
    out = []
    for i in range(n):
        pf = min(keys, key=lambda k: (abs(k - i), k))
        out.append(shift_right(fake_mask(w, h, prompts[pf]), i - pf))
    return out


class FakeTracker:
    """Config.tracker_factory stand-in for Sam2Tracker: records every call, answers fake_track / fake_mask."""

    created = 0

    def __init__(self, rt, app):
        self.rt, self.app = rt, app
        self.calls: list[tuple] = []
        FakeTracker.created += 1

    def track(self, body, n, w, h, prompts, obj=1):
        self.calls.append(("track", n, w, h, prompts, obj, len(body)))
        return fake_track(n, w, h, prompts)

    def track_frame(self, body, w, h, prompt, obj=1):
        self.calls.append(("frame", w, h, prompt, obj, len(body)))
        return fake_mask(w, h, prompt)


# ---------------------------------------------------------------------------
# a fake onnxruntime whose CUDA provider "works": real CPU sessions that claim the CUDA EP
# ---------------------------------------------------------------------------
class _Wrap:
    """A session-like wrapper around a real onnxruntime session; run() is replaced per test."""

    def __init__(self, real):
        self.real = real

    def get_inputs(self):
        return self.real.get_inputs()

    def get_outputs(self):
        return self.real.get_outputs()

    def get_providers(self):
        return self.real.get_providers()

    def run(self, outputs, feeds, *args):
        return self.real.run(outputs, feeds, *args)


class CudaWrap(_Wrap):
    def get_providers(self):
        return [CUDA_EP, CPU_EP]


def wants_cuda(providers) -> bool:
    p = (providers or [CPU_EP])[0]
    return (p[0] if isinstance(p, tuple) else p) == CUDA_EP


class FakeCudaOrt:
    """onnxruntime stand-in: the CUDA EP is listed and its sessions are real CPU sessions claiming it."""

    def __init__(self):
        import onnxruntime as ort

        self.ort = ort

    @staticmethod
    def set_default_logger_severity(_level):
        pass

    @staticmethod
    def get_available_providers():
        return [CUDA_EP, CPU_EP]

    def SessionOptions(self):  # noqa: N802 - onnxruntime's name
        return self.ort.SessionOptions()

    def InferenceSession(self, path, so=None, providers=None):  # noqa: N802
        real = self.ort.InferenceSession(path, so, providers=[CPU_EP])
        return CudaWrap(real) if wants_cuda(providers) else real


FAKE_GPU = {"name": "Fake GPU", "totalGiB": 16.0, "freeGiB": 12.0}


class Sidecar:
    """One App with the synthetic graph registered under the given ids (and a fake tracker under `trackers`); `serve`
    runs the HTTP server in a thread. `cuda=True` fakes a working CUDA EP (FakeCudaOrt + a 16 GiB GPU sample)."""

    def __init__(self, tmp_path, synthetic_file: str, models=("synthetic",), precision="fp32", env=None, session_factory=None, gpu_query=None, pre_start=None, serve=True, trackers=(), tracker_factory=None, cuda=False, registry=None):
        self.models_dir = tmp_path / "models"
        self.models_dir.mkdir(exist_ok=True)
        shutil.copy(synthetic_file, self.models_dir / "synthetic.onnx")
        reg = {mid: matte.synthetic_spec(str(self.models_dir / "synthetic.onnx"), precision) for mid in models}
        if trackers:
            ckpt = self.models_dir / "synthetic.pt"
            ckpt.write_bytes(b"not a checkpoint: the fake tracker never reads it\n" * 8)
            for mid in trackers:
                reg[mid] = matte.synthetic_tracker_spec(str(ckpt))
        if registry:
            reg.update(registry)
        all_ids = list(models) + list(trackers) + list(registry or [])
        e = {
            "MATTE_DEVICE": "auto" if cuda else "cpu",
            "MATTE_MODELS_DIR": str(self.models_dir),
            "MATTE_BIND": "127.0.0.1",
            "MATTE_PORT": "0",
            "MATTE_MODELS": ",".join(all_ids),
            "MATTE_DEFAULT_MODEL": all_ids[0],
            "MATTE_PRELOAD": ",".join(all_ids),
            "MATTE_MODEL_TTL": "0",
            "MATTE_VERSION": "test",
            "MATTE_THREADS": "2",
        }
        if env:
            e.update(env)
        self.cfg = matte.Config(e)
        self.cfg.session_factory = session_factory
        self.cfg.gpu_query = gpu_query if gpu_query is not None else ((lambda: dict(FAKE_GPU)) if cuda else None)
        self.cfg.tracker_factory = tracker_factory if tracker_factory is not None else (FakeTracker if trackers else None)
        self.app = matte.App(self.cfg, reg)
        if cuda:
            self.app._ort = FakeCudaOrt()
        if pre_start:
            pre_start(self.app)
        self.app.start(serve=serve)
        self.thread = None
        if serve:
            self.thread = threading.Thread(target=self.app.serve_forever, name="pytest-sidecar", daemon=True)
            self.thread.start()

    @property
    def port(self) -> int:
        return self.app.port

    def close(self) -> None:
        self.app.shutdown()

    def rt(self, mid: str = "synthetic", device: str = "cpu"):
        return self.app.runtime(mid, device)

    def wait(self, mid: str = "synthetic", states=("ready",), timeout: float = 60.0, device: str | None = None):
        dev = device or self.app.default_device
        t0 = time.time()
        while time.time() - t0 < timeout:
            rt = self.app.runtime(mid, dev)
            assert rt is not None, f"{mid} is not offered on {dev}"
            if rt.state in states:
                return rt
            if rt.state == "unavailable" and "unavailable" not in states:
                raise AssertionError(f"{mid}@{dev} unavailable: {rt.reason}")
            time.sleep(0.02)
        rt = self.app.runtime(mid, dev)
        raise AssertionError(f"{mid}@{dev} still {rt.state} ({rt.reason})")

    def wait_resident(self, mid: str = "synthetic", device: str | None = None, resident: bool = True, timeout: float = 20.0):
        dev = device or self.app.default_device
        t0 = time.time()
        while time.time() - t0 < timeout:
            rt = self.app.runtime(mid, dev)
            if bool(rt.sessions) == resident:
                return rt
            time.sleep(0.02)
        raise AssertionError(f"{mid}@{dev} resident={bool(self.app.runtime(mid, dev).sessions)}")

    def conn(self, timeout: float = 60.0) -> http.client.HTTPConnection:
        return http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)

    def get(self, path: str):
        c = self.conn()
        try:
            c.request("GET", path)
            r = c.getresponse()
            return r.status, r.read(), dict(r.getheaders())
        finally:
            c.close()

    def post(self, path: str, body: bytes = b"", headers=None):
        c = self.conn()
        try:
            c.request("POST", path, body, headers or {})
            r = c.getresponse()
            return r.status, r.read(), dict(r.getheaders())
        finally:
            c.close()

    def ping(self) -> dict:
        return json.loads(self.get("/v1/ping")[1])

    def matte(self, frames_u8: np.ndarray, model: str = "synthetic", size: int | None = None, conn=None, frames_param: int | None = None, body: bytes | None = None, device: str | None = None):
        size = size or frames_u8.shape[1]
        n = frames_param if frames_param is not None else frames_u8.shape[0]
        data = frames_u8.tobytes() if body is None else body
        c = conn or self.conn()
        dev = f"&device={device}" if device else ""
        try:
            c.request("POST", f"/v1/matte?model={model}&size={size}&frames={n}{dev}", data, {"Content-Type": "application/octet-stream"})
            r = c.getresponse()
            return r.status, r.read(), dict(r.getheaders())
        finally:
            if conn is None:
                c.close()

    def track(self, frames_u8: np.ndarray, prompts, model: str = "tracker", device: str | None = None, frames_param: int | None = None, body: bytes | None = None, w: int | None = None, h: int | None = None, headers: dict | None = None, path: str = "/v1/track"):
        """POST /v1/track (or /v1/track/frame with path) with `prompts` as the X-Matte-Prompts JSON (a dict/list) or a raw string."""
        n = frames_param if frames_param is not None else frames_u8.shape[0]
        hh = frames_u8.shape[1] if h is None else h
        ww = frames_u8.shape[2] if w is None else w
        data = frames_u8.tobytes() if body is None else body
        hdr = {"Content-Type": "application/octet-stream"}
        if prompts is not None:
            hdr[matte.PROMPTS_HEADER] = prompts if isinstance(prompts, str) else json.dumps(prompts)
        if headers:
            hdr.update(headers)
        dev = f"&device={device}" if device else ""
        fr = "" if path.endswith("/frame") else f"&frames={n}"
        c = self.conn()
        try:
            c.request("POST", f"{path}?model={model}&w={ww}&h={hh}{fr}{dev}", data, hdr)
            r = c.getresponse()
            return r.status, r.read(), dict(r.getheaders())
        finally:
            c.close()

    def track_frame(self, frame_u8: np.ndarray, prompts, **kw):
        return self.track(frame_u8[None] if frame_u8.ndim == 3 else frame_u8, prompts, path="/v1/track/frame", **kw)


@pytest.fixture
def sidecar(tmp_path, synthetic_file):
    sc = Sidecar(tmp_path, synthetic_file)
    try:
        sc.wait()
        yield sc
    finally:
        sc.close()


@pytest.fixture
def tracker_sidecar(tmp_path, synthetic_file):
    """A cpu sidecar offering the synthetic segmenter and the fake tracker ("tracker"), both resident after the start."""
    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",))
    try:
        sc.wait()
        sc.wait("tracker")
        yield sc
    finally:
        sc.close()


@pytest.fixture
def cuda_sidecar(tmp_path, synthetic_file):
    """A two-device sidecar (fake CUDA EP): "synthetic" (fp16 on cuda, fp32 on cpu), "cuda-only" (offer cuda) and the fake tracker."""
    reg = {"cuda-only": dict(matte.synthetic_spec(synthetic_file, "fp32"), offer=["cuda"])}
    sc = Sidecar(tmp_path, synthetic_file, precision={"cuda": "fp16", "cpu": "fp32"}, trackers=("tracker",), cuda=True, registry=reg)
    try:
        for mid in ("synthetic", "tracker"):
            sc.wait(mid, device="cuda")
            sc.wait(mid, device="cpu")
        sc.wait("cuda-only", device="cuda")
        yield sc
    finally:
        sc.close()


def decode_all(payload: bytes) -> np.ndarray:
    return np.stack([png.decode_gray(r) for r in matte.parse_records(payload)])
