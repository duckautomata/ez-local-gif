"""pytest fixtures: an in-process sidecar on 127.0.0.1:<random port> whose model is a synthetic 8×8 ONNX graph
(matte.build_synthetic_graph), so the suite needs no download, no GPU and only the cpu requirements + pytest.

Run from the repo root:  python -I -m pytest sidecar/tests -q      (or from sidecar/: python -I -m pytest tests -q)
"""
from __future__ import annotations

import http.client
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


class Sidecar:
    """One App with the synthetic graph registered under the given ids; `serve` runs the HTTP server in a thread."""

    def __init__(self, tmp_path, synthetic_file: str, models=("synthetic",), precision="fp32", env=None, session_factory=None, gpu_query=None, pre_start=None, serve=True):
        self.models_dir = tmp_path / "models"
        self.models_dir.mkdir(exist_ok=True)
        shutil.copy(synthetic_file, self.models_dir / "synthetic.onnx")
        reg = {mid: matte.synthetic_spec(str(self.models_dir / "synthetic.onnx"), precision) for mid in models}
        e = {
            "MATTE_DEVICE": "cpu",
            "MATTE_MODELS_DIR": str(self.models_dir),
            "MATTE_BIND": "127.0.0.1",
            "MATTE_PORT": "0",
            "MATTE_MODELS": ",".join(models),
            "MATTE_DEFAULT_MODEL": models[0],
            "MATTE_PRELOAD": ",".join(models),
            "MATTE_MODEL_TTL": "0",
            "MATTE_VERSION": "test",
            "MATTE_THREADS": "2",
        }
        if env:
            e.update(env)
        self.cfg = matte.Config(e)
        self.cfg.session_factory = session_factory
        self.cfg.gpu_query = gpu_query
        self.app = matte.App(self.cfg, reg)
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

    def wait(self, mid: str = "synthetic", states=("ready",), timeout: float = 60.0):
        t0 = time.time()
        while time.time() - t0 < timeout:
            m = self.app.models[mid]
            if m.state in states:
                return m
            if m.state == "unavailable" and "unavailable" not in states:
                raise AssertionError(f"{mid} unavailable: {m.reason}")
            time.sleep(0.02)
        raise AssertionError(f"{mid} still {self.app.models[mid].state} ({self.app.models[mid].reason})")

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

    def matte(self, frames_u8: np.ndarray, model: str = "synthetic", size: int | None = None, conn=None, frames_param: int | None = None, body: bytes | None = None):
        size = size or frames_u8.shape[1]
        n = frames_param if frames_param is not None else frames_u8.shape[0]
        data = frames_u8.tobytes() if body is None else body
        c = conn or self.conn()
        try:
            c.request("POST", f"/v1/matte?model={model}&size={size}&frames={n}", data, {"Content-Type": "application/octet-stream"})
            r = c.getresponse()
            return r.status, r.read(), dict(r.getheaders())
        finally:
            if conn is None:
                c.close()


@pytest.fixture
def sidecar(tmp_path, synthetic_file):
    sc = Sidecar(tmp_path, synthetic_file)
    try:
        sc.wait()
        yield sc
    finally:
        sc.close()


def decode_all(payload: bytes) -> np.ndarray:
    return np.stack([png.decode_gray(r) for r in matte.parse_records(payload)])
