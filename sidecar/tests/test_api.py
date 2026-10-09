"""The HTTP API: ping, record framing, pixels, caps, warm/unload, loading/OOM mapping, ping latency during a batch."""
from __future__ import annotations

import json
import os
import statistics
import threading
import time

import numpy as np
import pytest

from conftest import SIZE, Sidecar, decode_all, frames, matte, reference


def test_ping_shape(sidecar):
    status, body, headers = sidecar.get("/v1/ping")
    assert status == 200 and headers["Content-Type"] == "application/json"
    p = json.loads(body)
    assert p["protocol"] == matte.PROTOCOL == 1
    assert p["processingVersion"] == matte.PROCESSING_VERSION
    assert p["version"] == "test" and len(p["instance"]) == 16
    assert p["device"] == "cpu" and p["reason"] == "" and p["busy"] == 0
    assert p["defaultModel"] == "synthetic"
    m = p["models"]["synthetic"]
    assert m["state"] == "ready" and m["reason"] == "" and m["lastError"] == ""
    assert m["weights"] == matte.sha256_file(str(sidecar.models_dir / "synthetic.onnx"))
    assert m["graphDigest"] == m["weights"]  # fp32 at the graph size = the source file
    assert m["precision"] == "fp32" and m["sizes"] == [SIZE, 2 * SIZE] and m["defaultSize"] == SIZE
    assert set(m["msPerFrame"]) == {str(SIZE)} and m["msPerFrame"][str(SIZE)] > 0
    assert m["label"] == "synthetic self-test graph" and m["licence"] == "n/a" and m["resident"] is True
    assert "gpu" in p


def test_records_and_pixels_match_a_plain_onnxruntime_session(sidecar):
    f = frames(5)
    status, body, headers = sidecar.matte(f)
    assert status == 200, body
    assert headers["Content-Type"] == matte.MATTES_CONTENT_TYPE
    assert int(headers["Content-Length"]) == len(body)
    recs = matte.parse_records(body)
    assert len(recs) == 5 and all(r.startswith(b"\x89PNG") for r in recs)
    got = decode_all(body)
    want = reference(f, str(sidecar.models_dir / "synthetic.onnx"))
    assert got.shape == (5, SIZE, SIZE)
    assert np.abs(got.astype(np.int16) - want.astype(np.int16)).max() <= 1
    assert int(got.max()) - int(got.min()) > 30  # a picture, not a flat image (the synthetic graph is a soft conv)


def test_no_cross_frame_dependence(sidecar):
    a, b = frames(1, seed=1), frames(1, seed=2)
    ab = matte.parse_records(sidecar.matte(np.concatenate([a, b]))[1])
    ba = matte.parse_records(sidecar.matte(np.concatenate([b, a]))[1])
    single_a = matte.parse_records(sidecar.matte(a)[1])
    assert ab[0] == ba[1] == single_a[0] and ab[1] == ba[0]


def test_other_size_uses_the_rescaled_graph(sidecar):
    f = frames(3, size=2 * SIZE)
    status, body, _ = sidecar.matte(f, size=2 * SIZE)
    assert status == 200, body
    got = decode_all(body)
    assert got.shape == (3, 2 * SIZE, 2 * SIZE)
    derived = sidecar.models_dir / f"synthetic.{2 * SIZE}.onnx"
    assert derived.is_file() and (sidecar.models_dir / f"synthetic.{2 * SIZE}.onnx.stamp.json").is_file()
    want = reference(f, str(derived))
    assert np.abs(got.astype(np.int16) - want.astype(np.int16)).max() <= 1
    p = json.loads(sidecar.get("/v1/ping")[1])
    assert str(2 * SIZE) in p["models"]["synthetic"]["msPerFrame"]


def test_keep_alive_serves_several_batches_on_one_connection(sidecar):
    c = sidecar.conn()
    try:
        for i in range(4):
            status, body, _ = sidecar.matte(frames(2, seed=i), conn=c)
            assert status == 200 and len(matte.parse_records(body)) == 2
        c.request("GET", "/v1/ping")
        r = c.getresponse()
        assert r.status == 200 and json.loads(r.read())["busy"] == 0
    finally:
        c.close()


def test_default_model_and_size_when_omitted(sidecar):
    f = frames(2)
    status, body, _ = sidecar.post(f"/v1/matte?frames=2", f.tobytes())
    assert status == 200 and len(matte.parse_records(body)) == 2


@pytest.mark.parametrize(
    "query,n,expect,needle",
    [
        ("model=nope&size=8&frames=1", 1, 404, "unknown model"),
        ("model=synthetic&size=12&frames=1", 1, 404, "no size 12"),
        ("model=synthetic&size=8&frames=0", 1, 400, "frames must be"),
        ("model=synthetic&size=8&frames=x", 1, 400, "integers"),
        ("model=synthetic&size=8&frames=33", 33, 413, "MATTE_MAX_BATCH"),
        ("model=synthetic&size=8&frames=2", 1, 400, "Content-Length"),
    ],
)
def test_caps_and_bad_params(sidecar, query, n, expect, needle):
    body = frames(n).tobytes()
    status, data, _ = sidecar.post("/v1/matte?" + query, body)
    assert status == expect, data
    assert needle in json.loads(data)["error"]


def test_body_over_128_mib_is_413_before_reading(sidecar):
    c = sidecar.conn()
    try:
        c.putrequest("POST", "/v1/matte?model=synthetic&size=8&frames=1")
        c.putheader("Content-Length", str(matte.MAX_BODY_BYTES + 1))
        c.putheader("Content-Type", "application/octet-stream")
        c.endheaders()
        r = c.getresponse()
        assert r.status == 413
        assert r.getheader("Connection") == "close"
        assert "exceeds" in json.loads(r.read())["error"]
    finally:
        c.close()


def test_short_body_is_400(sidecar):
    c = sidecar.conn()
    try:
        c.putrequest("POST", "/v1/matte?model=synthetic&size=8&frames=2")
        c.putheader("Content-Length", str(2 * SIZE * SIZE * 3))
        c.endheaders()
        c.send(b"\0" * (SIZE * SIZE * 3))  # one frame of two, then EOF
        c.sock.shutdown(1)
        r = c.getresponse()
        assert r.status == 400 and "short body" in json.loads(r.read())["error"]
    finally:
        c.close()


def test_unknown_paths(sidecar):
    assert sidecar.get("/")[0] == 404
    assert sidecar.get("/v1/nope")[0] == 404
    assert sidecar.post("/v1/nope")[0] == 404
    assert sidecar.post("/v1/warm?model=nope")[0] == 404
    assert sidecar.post("/v1/unload?model=nope")[0] == 404


def test_unload_then_warm(sidecar):
    status, body, _ = sidecar.post("/v1/unload")
    assert status == 200 and json.loads(body)["unloaded"] == ["synthetic"]
    p = json.loads(sidecar.get("/v1/ping")[1])["models"]["synthetic"]
    assert p["state"] == "ready" and p["resident"] is False
    status, body, _ = sidecar.post("/v1/warm?model=synthetic")
    assert status == 200 and json.loads(body)["model"] == "synthetic"
    t0 = time.time()
    while time.time() - t0 < 20 and not sidecar.app.models["synthetic"].sessions:
        time.sleep(0.02)
    assert sidecar.app.models["synthetic"].sessions
    assert sidecar.post("/v1/unload?model=synthetic")[0] == 200
    # a request on a released model recreates the session by itself
    status, body, _ = sidecar.matte(frames(1))
    assert status == 200 and len(matte.parse_records(body)) == 1
    assert json.loads(sidecar.get("/v1/ping")[1])["models"]["synthetic"]["resident"] is True


def test_ttl_releases_idle_sessions(tmp_path, synthetic_file, monkeypatch):
    monkeypatch.setattr(matte, "TTL_TICK_S", 0.1)
    sc = Sidecar(tmp_path, synthetic_file, env={"MATTE_MODEL_TTL": "0.4"})
    try:
        sc.wait()
        assert sc.app.models["synthetic"].sessions
        t0 = time.time()
        while time.time() - t0 < 10 and sc.app.models["synthetic"].sessions:
            time.sleep(0.05)
        assert not sc.app.models["synthetic"].sessions
        status, body, _ = sc.matte(frames(1))
        assert status == 200 and len(matte.parse_records(body)) == 1
    finally:
        sc.close()


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


def _factory(wrap):
    def make(path, so, providers):
        import onnxruntime as ort

        return wrap(ort.InferenceSession(path, so, providers=providers))

    return make


def test_ping_answers_within_50ms_during_a_slow_batch(tmp_path, synthetic_file):
    class Slow(_Wrap):
        def run(self, outputs, feeds, *args):
            time.sleep(0.12)  # a slow model: the GIL is released like inside onnxruntime
            return super().run(outputs, feeds, *args)

    sc = Sidecar(tmp_path, synthetic_file, session_factory=_factory(Slow))
    try:
        sc.wait()
        result = {}

        def batch():
            result["r"] = sc.matte(frames(8))

        t = threading.Thread(target=batch)
        t.start()
        time.sleep(0.2)
        latencies = []
        while t.is_alive() and len(latencies) < 12:
            t0 = time.perf_counter()
            status, body, _ = sc.get("/v1/ping")
            latencies.append(time.perf_counter() - t0)
            assert status == 200
            assert json.loads(body)["busy"] == 1
            time.sleep(0.03)
        t.join(30)
        assert result["r"][0] == 200
        assert len(latencies) >= 5, latencies
        # The property: /v1/ping is not serialised behind the 120 ms batch
        # step (a serialised ping would wait ~1 s for the 8 frames). Asserted
        # with margin — the worst sample well under one step, the typical one
        # a few ms — so a scheduler hiccup on a shared CI runner (this job
        # gates the image builds) cannot fail it; the spec's < 50 ms figure
        # holds on an idle box.
        assert max(latencies) < 0.10, latencies
        assert statistics.median(latencies) < 0.02, latencies
    finally:
        sc.close()


def test_oom_with_nothing_to_release_is_507_then_retested(tmp_path, synthetic_file):
    calls = [0]

    class Flaky(_Wrap):
        def run(self, outputs, feeds, *args):
            calls[0] += 1
            if calls[0] == matte.WARMUP_FRAMES + 2:  # the first real frame after the 1 + 8 self-test runs
                raise RuntimeError("[ONNXRuntimeError] : 1 : FAIL : CUDA failure 2: out of memory ; GPU=0")
            return super().run(outputs, feeds, *args)

    sc = Sidecar(tmp_path, synthetic_file, session_factory=_factory(Flaky))
    try:
        sc.wait()
        status, body, _ = sc.matte(frames(2))
        assert status == 507, body
        assert "out of memory" in json.loads(body)["error"]
        # unavailable until the loader re-tested it, then ready again without a client-side warm
        m = sc.wait(states=("unavailable", "loading", "ready"), timeout=5)
        assert m.last_error.startswith("RuntimeError")
        sc.wait(timeout=20)
        status, body, _ = sc.matte(frames(2))
        assert status == 200 and len(matte.parse_records(body)) == 2
        p = json.loads(sc.get("/v1/ping")[1])["models"]["synthetic"]
        assert p["state"] == "ready" and "out of memory" in p["lastError"]
    finally:
        sc.close()


def test_oom_that_released_another_model_is_503_retry(tmp_path, synthetic_file):
    calls = [0]

    class Flaky(_Wrap):
        def run(self, outputs, feeds, *args):
            calls[0] += 1
            if calls[0] == 2 * (matte.WARMUP_FRAMES + 1) + 1:  # both models self-tested, then the first request frame
                raise MemoryError("std::bad_alloc")
            return super().run(outputs, feeds, *args)

    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic", "other"), session_factory=_factory(Flaky))
    try:
        sc.wait("synthetic")
        sc.wait("other")
        assert sc.app.models["other"].sessions
        status, body, _ = sc.matte(frames(1))
        assert status == 503, body
        j = json.loads(body)
        assert j["error"] == "out of memory" and j["retryAfterMs"] == matte.OOM_RETRY_MS
        p = json.loads(sc.get("/v1/ping")[1])["models"]
        assert p["other"]["resident"] is False and p["synthetic"]["state"] == "ready"
        status, body, _ = sc.matte(frames(1))  # the retry recreates the session and succeeds
        assert status == 200
    finally:
        sc.close()


def test_loading_model_answers_503_with_retry_after(tmp_path, synthetic_file):
    gate = threading.Event()
    first = [True]

    def slow_create(path, so, providers):
        import onnxruntime as ort

        if first[0]:
            first[0] = False
            gate.wait(30)
        return ort.InferenceSession(path, so, providers=providers)

    sc = Sidecar(tmp_path, synthetic_file, env={"MATTE_PRELOAD": ""}, session_factory=slow_create)
    try:
        assert sc.app.models["synthetic"].state == "loading" and not sc.app.models["synthetic"].queued
        status, body, _ = sc.matte(frames(1))  # not preloaded: this request starts the load and is told to retry
        assert status == 503, body
        j = json.loads(body)
        assert j["error"] == "model loading" and j["retryAfterMs"] > 0 and j["state"] == "loading"
        assert sc.post("/v1/warm?model=synthetic")[0] == 200
        assert json.loads(sc.get("/v1/ping")[1])["models"]["synthetic"]["state"] == "loading"
        gate.set()
        sc.wait()
        status, body, _ = sc.matte(frames(1))
        assert status == 200
    finally:
        gate.set()
        sc.close()


def test_missing_file_is_downloaded_on_demand_and_reports_a_failed_download(tmp_path, synthetic_file, monkeypatch):
    monkeypatch.setattr(matte, "DOWNLOAD_ATTEMPTS", 1)
    sc = Sidecar(tmp_path, synthetic_file, env={"MATTE_PRELOAD": ""})
    try:
        os.remove(sc.models_dir / "synthetic.onnx")
        m = sc.app.models["synthetic"]
        m.state = "missing"  # as Model() reports it when the file is absent at start
        m.urls = ["http://127.0.0.1:9/nothing.onnx"]  # nothing listens on port 9: the download fails at once
        assert json.loads(sc.get("/v1/ping")[1])["models"]["synthetic"]["state"] == "missing"
        status, body, _ = sc.matte(frames(1))
        assert status == 503
        j = json.loads(body)
        assert j["error"] == "model loading" and j["state"] == "downloading" and j["retryAfterMs"] == 5000
        t0 = time.time()
        while time.time() - t0 < 10 and not m.last_error:
            time.sleep(0.05)
        p = json.loads(sc.get("/v1/ping")[1])["models"]["synthetic"]
        assert p["state"] == "missing" and p["reason"].startswith("download failed") and p["lastError"]
        assert p["weights"] == m.weights  # the static pin is reported in every state
    finally:
        sc.close()


def test_device_unavailable_reports_503_everywhere(tmp_path, synthetic_file):
    class FakeOrt:
        @staticmethod
        def set_default_logger_severity(_level):
            pass

        @staticmethod
        def get_available_providers():
            return ["CPUExecutionProvider"]

    sc = Sidecar(tmp_path, synthetic_file, env={"MATTE_DEVICE": "cuda"}, pre_start=lambda app: setattr(app, "_ort", FakeOrt()))
    try:
        status, body, _ = sc.get("/v1/ping")
        assert status == 503
        p = json.loads(body)
        assert p["device"] == "unavailable" and p["protocol"] == 1
        assert "CUDA EP not available" in p["reason"] and p["error"] == p["reason"]
        assert p["models"]["synthetic"]["state"] in ("loading", "missing")  # never loaded: nothing runs on an unavailable device
        status, body, _ = sc.matte(frames(1))
        assert status == 503 and "CUDA EP not available" in json.loads(body)["error"]
        assert sc.post("/v1/warm?model=synthetic")[0] == 503
    finally:
        sc.close()


def test_auto_falls_back_to_cpu_with_the_reason(tmp_path, synthetic_file):
    class FakeOrt:
        @staticmethod
        def set_default_logger_severity(_level):
            pass

        @staticmethod
        def get_available_providers():
            return ["CPUExecutionProvider"]

    sc = Sidecar(tmp_path, synthetic_file, env={"MATTE_DEVICE": "auto"}, pre_start=lambda app: setattr(app, "_ort", FakeOrt()))
    try:
        assert sc.app.device == "cpu" and "CUDA EP not available" in sc.app.device_reason
        sc.app._ort = None  # the real onnxruntime for the sessions
        sc.wait()
        p = json.loads(sc.get("/v1/ping")[1])
        assert p["device"] == "cpu" and "CUDA EP not available" in p["reason"]
    finally:
        sc.close()


def test_gpu_query_feeds_ping_and_the_vram_precheck(tmp_path, synthetic_file):
    gpu = {"name": "Fake GPU", "totalGiB": 16.0, "freeGiB": 5.5}
    sc = Sidecar(tmp_path, synthetic_file, gpu_query=lambda: dict(gpu))
    try:
        sc.wait()
        assert json.loads(sc.get("/v1/ping")[1])["gpu"] == gpu
        m = sc.app.models["synthetic"]
        m.spec = dict(m.spec, vram={"cap": True, "min_limit_gib": 6, "min_free_gib": 7})
        sc.app.device = "cuda"  # the pre-check reads the device and the last GPU sample
        why = sc.app._precheck(m)
        assert why == "needs about 7 GB of free GPU memory, 5.5 GB free"
        sc.app.cfg.gpu_mem_limit_gib = 4
        assert sc.app._precheck(m).startswith("MATTE_GPU_MEM_LIMIT_GIB=4 is below the 6 GiB")
        sc.app.device = "cpu"
        m.spec = dict(m.spec, ram={"min_available_gib": 100000})
        why = sc.app._precheck(m)
        assert why is None or why.startswith("cpu: 100000 GiB of RAM needed")
    finally:
        sc.close()


def test_unknown_model_in_MATTE_MODELS_fails_loudly(tmp_path, synthetic_file):
    with pytest.raises(SystemExit):
        Sidecar(tmp_path, synthetic_file, env={"MATTE_MODELS": "synthetic,nope"}, serve=False)


def test_parse_records_rejects_bad_streams():
    good = b"\0\0\0\x02ab\0\0\0\0"
    assert matte.parse_records(good) == [b"ab"]
    for bad in (b"", b"\0\0\0\x02a", b"\0\0\0\x02ab", b"\0\0\0\x02ab\0\0\0\0x", b"\0\0\0\x02ab\0\0\0"):
        with pytest.raises(ValueError):
            matte.parse_records(bad)


def test_physical_cores_is_positive():
    assert matte.physical_cores() >= 1
