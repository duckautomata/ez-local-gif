"""Phase 5c devices: the offered list, the device= routing, 404 for an unoffered device, per-device defaults,
self-test-then-release, unload filters, the ping shape with the per-device maps."""
from __future__ import annotations

import json
import time

import numpy as np
import pytest

from conftest import SIZE, FakeTracker, Sidecar, decode_all, frames, matte, reference


def test_cpu_only_process_offers_the_cpu(sidecar):
    p = sidecar.ping()
    assert p["devices"] == ["cpu"] and p["defaultDevice"] == "cpu" and p["device"] == "cpu"
    assert p["defaultModels"] == {"cpu": "synthetic"} and p["defaultModel"] == "synthetic"
    status, body, _ = sidecar.post("/v1/matte?model=synthetic&size=8&frames=1&device=cuda", frames(1).tobytes())
    assert status == 404
    j = json.loads(body)
    assert j["error"] == "device cuda not offered" and j["devices"] == ["cpu"]
    assert sidecar.post("/v1/warm?model=synthetic&device=cuda")[0] == 404
    assert sidecar.post("/v1/unload?device=cuda")[0] == 404
    # the default device needs no device= and an explicit device=cpu is the same runtime
    assert sidecar.matte(frames(1))[0] == 200
    assert sidecar.matte(frames(1), device="cpu")[0] == 200


def test_two_devices_offered_with_a_working_cuda_ep(cuda_sidecar):
    sc = cuda_sidecar
    p = sc.ping()
    assert p["devices"] == ["cuda", "cpu"] and p["defaultDevice"] == "cuda" and p["device"] == "cuda" and p["reason"] == ""
    assert p["defaultModels"] == {"cuda": "synthetic", "cpu": "synthetic"} and p["defaultModel"] == "synthetic"
    assert p["gpu"]["totalGiB"] == 16.0
    m = p["models"]["synthetic"]
    # the top-level fields mirror the DEFAULT device (cuda: fp16), the per-device map carries both graph choices
    assert m["precision"] == "fp16" and m["state"] == "ready" and m["resident"] is True and m["kind"] == "segmenter"
    assert set(m["devices"]) == {"cuda", "cpu"}
    assert m["devices"]["cuda"]["precision"] == "fp16" and m["devices"]["cpu"]["precision"] == "fp32"
    assert m["devices"]["cuda"]["graphDigest"] != m["devices"]["cpu"]["graphDigest"]  # the fp16 derivation vs the source graph
    assert m["devices"]["cuda"]["msPerFrame"][str(SIZE)] > 0 and m["devices"]["cpu"]["msPerFrame"][str(SIZE)] > 0
    # a model offered on cuda only has one entry and refuses the cpu
    assert list(p["models"]["cuda-only"]["devices"]) == ["cuda"]
    status, body, _ = sc.matte(frames(1), model="cuda-only", device="cpu")
    assert status == 404 and "not offered on device cpu" in json.loads(body)["error"]
    assert sc.matte(frames(1), model="cuda-only")[0] == 200  # default device = cuda
    # the tracker is offered on both with its own precision per device
    t = p["models"]["tracker"]
    assert t["kind"] == "tracker" and t["devices"]["cuda"]["precision"] == "bf16" and t["devices"]["cpu"]["precision"] == "fp32"
    # an unoffered device name
    status, body, _ = sc.matte(frames(1), device="tpu")
    assert status == 404 and json.loads(body)["error"] == "device tpu not offered"


def test_device_param_routes_to_that_runtime(cuda_sidecar):
    sc = cuda_sidecar
    cuda, cpu = sc.rt("synthetic", "cuda"), sc.rt("synthetic", "cpu")
    before = (cuda.last_used, cpu.last_used)
    time.sleep(0.01)
    status, body, _ = sc.matte(frames(2), device="cpu")
    assert status == 200 and len(matte.parse_records(body)) == 2
    assert cpu.last_used > before[1] and cuda.last_used == before[0]
    # the cpu runtime runs the fp32 source graph: pixels match a plain CPU session
    want = reference(frames(2), str(sc.models_dir / "synthetic.onnx"))
    assert np.abs(decode_all(body).astype(np.int16) - want.astype(np.int16)).max() <= 1
    time.sleep(0.01)
    status, body, _ = sc.matte(frames(2))  # default = cuda
    assert status == 200 and cuda.last_used > before[0]
    # the per-device session dicts are separate: sizes created on one device do not appear on the other
    assert sc.matte(frames(1, size=2 * SIZE), size=2 * SIZE, device="cpu")[0] == 200
    assert 2 * SIZE in cpu.sessions and 2 * SIZE not in cuda.sessions


def test_unload_filters_by_model_and_device(cuda_sidecar):
    sc = cuda_sidecar
    assert all(sc.rt(m, d).sessions for m in ("synthetic", "tracker") for d in ("cuda", "cpu"))
    status, body, _ = sc.post("/v1/unload?device=cpu")
    assert status == 200
    j = json.loads(body)
    assert j["unloaded"] == ["synthetic", "tracker"]
    assert {(s["model"], s["device"]) for s in j["sessions"]} == {("synthetic", "cpu"), ("tracker", "cpu")}
    assert not sc.rt("synthetic", "cpu").sessions and not sc.rt("tracker", "cpu").sessions
    assert sc.rt("synthetic", "cuda").sessions and sc.rt("tracker", "cuda").sessions
    p = sc.ping()["models"]["synthetic"]
    assert p["resident"] is True and p["devices"]["cuda"]["resident"] is True and p["devices"]["cpu"]["resident"] is False
    status, body, _ = sc.post("/v1/unload?model=tracker&device=cuda")
    assert status == 200 and json.loads(body)["sessions"] == [{"model": "tracker", "device": "cuda"}]
    assert sc.rt("synthetic", "cuda").sessions and not sc.rt("tracker", "cuda").sessions
    status, body, _ = sc.post("/v1/unload?model=synthetic")  # every device of one model
    assert status == 200 and json.loads(body)["unloaded"] == ["synthetic"]
    assert not sc.rt("synthetic", "cuda").sessions and sc.rt("cuda-only", "cuda").sessions
    assert sc.post("/v1/unload")[0] == 200 and not any(rt.sessions for rt in sc.app.runtimes())
    # warm one device back
    assert json.loads(sc.post("/v1/warm?model=synthetic&device=cpu")[1]) == {"model": "synthetic", "device": "cpu", "state": "ready", "resident": False}
    sc.wait_resident("synthetic", "cpu")
    assert not sc.rt("synthetic", "cuda").sessions


def test_default_model_per_device(tmp_path, synthetic_file):
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic", "other"), cuda=True, env={"MATTE_DEFAULT_MODEL": "", "MATTE_DEFAULT_MODEL_CPU": "other"})
    try:
        for d in ("cuda", "cpu"):
            sc.wait("synthetic", device=d)
            sc.wait("other", device=d)
        p = sc.ping()
        assert p["defaultModels"] == {"cuda": "synthetic", "cpu": "other"} and p["defaultModel"] == "synthetic"
        # a request without model= takes the default of the device it runs on
        assert sc.post("/v1/matte?frames=1&device=cpu", frames(1).tobytes())[0] == 200
        other_cpu, other_cuda = sc.rt("other", "cpu"), sc.rt("other", "cuda")
        assert str(SIZE) in sc.ping()["models"]["other"]["devices"]["cpu"]["msPerFrame"]
        t0 = other_cpu.last_used
        time.sleep(0.01)
        assert sc.post("/v1/matte?frames=1", frames(1).tobytes())[0] == 200  # default device (cuda) → "synthetic"
        assert other_cpu.last_used == t0 and other_cuda.last_used < sc.rt("synthetic", "cuda").last_used
    finally:
        sc.close()


def test_default_for_hints_and_env_override(tmp_path, synthetic_file):
    hinted = dict(matte.synthetic_spec(synthetic_file, "fp32"), defaultFor=["cuda"])
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry={"hinted": hinted}, cuda=True, env={"MATTE_DEFAULT_MODEL": "", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        assert sc.app.default_models == {"cuda": "hinted", "cpu": "synthetic"}
    finally:
        sc.close()
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry={"hinted": hinted}, cuda=True, env={"MATTE_DEFAULT_MODEL": "synthetic", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        assert sc.app.default_models == {"cuda": "synthetic", "cpu": "synthetic"}  # MATTE_DEFAULT_MODEL overrides both
    finally:
        sc.close()
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry={"hinted": hinted}, cuda=True, env={"MATTE_DEFAULT_MODEL": "nope", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        assert sc.app.default_models == {"cuda": "hinted", "cpu": "synthetic"}  # an unoffered name falls back to the hints
    finally:
        sc.close()


def test_registry_offer_decides_the_devices(tmp_path, synthetic_file):
    cpu_only = dict(matte.synthetic_spec(synthetic_file, "fp32"), offer=["cpu"])
    # MATTE_MODELS unset: the registry's offer lists pick the ids per device
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry={"cpu-only": cpu_only}, cuda=True, env={"MATTE_MODELS": None, "MATTE_SELFTEST": "0"}, serve=False)
    try:
        assert sorted(sc.app.models) == ["cpu-only", "synthetic"]
        assert list(sc.app.models["cpu-only"].runtimes) == ["cpu"] and sorted(sc.app.models["synthetic"].runtimes) == ["cpu", "cuda"]
    finally:
        sc.close()
    # a model named in MATTE_MODELS whose offer has none of the process's devices is offered on every device
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry={"cpu-only": cpu_only}, env={"MATTE_DEVICE": "cpu", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        assert list(sc.app.models["cpu-only"].runtimes) == ["cpu"]
    finally:
        sc.close()
    cuda_only = dict(matte.synthetic_spec(synthetic_file, "fp32"), offer=["cuda"])
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry={"gpu-only": cuda_only}, env={"MATTE_DEVICE": "cpu", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        assert list(sc.app.models["gpu-only"].runtimes) == ["cpu"]  # named explicitly on a cpu-only process
    finally:
        sc.close()


def test_selftest_then_release_is_the_default(tmp_path, synthetic_file):
    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",), env={"MATTE_PRELOAD": ""})
    try:
        sc.wait()
        sc.wait("tracker")
        for mid in ("synthetic", "tracker"):
            rt = sc.wait_resident(mid, resident=False)
            assert rt.state == "ready" and rt.prepared and rt.ms  # self-tested: msPerFrame exists before the first pass
        p = sc.ping()
        assert p["models"]["synthetic"]["resident"] is False and p["models"]["tracker"]["resident"] is False
        assert p["models"]["synthetic"]["state"] == "ready" and p["models"]["synthetic"]["msPerFrame"][str(SIZE)] > 0
        # the first request loads the runtime again (inline: the synthetic create is quick) and succeeds
        status, body, _ = sc.matte(frames(1))
        assert status == 200 and len(matte.parse_records(body)) == 1
        assert sc.rt().sessions and sc.ping()["models"]["synthetic"]["resident"] is True
        # MATTE_PRELOAD names what stays resident
    finally:
        sc.close()
    FakeTracker.created = 0
    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",), env={"MATTE_PRELOAD": "tracker"})
    try:
        sc.wait()
        sc.wait("tracker")
        sc.wait_resident("synthetic", resident=False)
        assert sc.rt("tracker").sessions and FakeTracker.created == 1
    finally:
        sc.close()


def test_slow_recreate_answers_503_and_loads_in_the_background(tmp_path, synthetic_file):
    sc = Sidecar(tmp_path, synthetic_file, env={"MATTE_PRELOAD": ""})
    try:
        sc.wait()
        rt = sc.wait_resident(resident=False)
        rt.create_ms = matte.INLINE_CREATE_MS + 1  # as a 10–20 s lite session create would be measured
        status, body, _ = sc.matte(frames(1))
        assert status == 503, body
        j = json.loads(body)
        assert j["error"] == "model loading" and j["state"] == "loading" and 1000 <= j["retryAfterMs"] <= 5000
        sc.wait_resident()  # the loader created it
        rt.create_ms = 10.0
        status, body, _ = sc.matte(frames(1))
        assert status == 200
    finally:
        sc.close()


def test_selftest_can_be_skipped(tmp_path, synthetic_file):
    sc = Sidecar(tmp_path, synthetic_file, env={"MATTE_SELFTEST": "0"})
    try:
        rt = sc.rt()
        assert rt.state == "loading" and not rt.sessions and not rt.ms and not rt.queued
        status, body, _ = sc.matte(frames(1))
        assert status == 503 and json.loads(body)["error"] == "model loading"
        sc.wait()
        status, body, _ = sc.matte(frames(1))
        assert status == 200 and sc.rt().ms
    finally:
        sc.close()


def test_ttl_releases_every_device(tmp_path, synthetic_file, monkeypatch):
    monkeypatch.setattr(matte, "TTL_TICK_S", 0.1)
    sc = Sidecar(tmp_path, synthetic_file, cuda=True, env={"MATTE_MODEL_TTL": "0.4"})
    try:
        sc.wait(device="cuda")
        sc.wait(device="cpu")
        t0 = time.time()
        while time.time() - t0 < 10 and (sc.rt("synthetic", "cuda").sessions or sc.rt("synthetic", "cpu").sessions):
            time.sleep(0.05)
        assert not sc.rt("synthetic", "cuda").sessions and not sc.rt("synthetic", "cpu").sessions
        assert sc.matte(frames(1), device="cpu")[0] == 200
    finally:
        sc.close()


def test_cuda_forced_without_the_ep_offers_nothing(tmp_path, synthetic_file):
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
        assert p["device"] == "unavailable" and p["devices"] == [] and p["defaultDevice"] == "" and p["defaultModels"] == {}
        assert sc.matte(frames(1))[0] == 503 and sc.post("/v1/warm?model=synthetic")[0] == 503
    finally:
        sc.close()


@pytest.mark.parametrize("device", ["cuda", "cpu"])
def test_warm_on_one_device(cuda_sidecar, device):
    sc = cuda_sidecar
    sc.post("/v1/unload")
    status, body, _ = sc.post(f"/v1/warm?model=synthetic&device={device}")
    assert status == 200 and json.loads(body)["device"] == device
    sc.wait_resident("synthetic", device)
    other = "cpu" if device == "cuda" else "cuda"
    assert not sc.rt("synthetic", other).sessions


def test_shipped_registry_offers_lite_on_the_cpu_runtime_too(tmp_path, synthetic_file, monkeypatch):
    """models.json offers birefnet-lite on BOTH devices (fp32 1024² on the cpu as well): on the cuda image a user who
    picks Run on: CPU + General sees it gated by the 14 GiB RAM precheck (unavailable with the figure), never "not
    offered on CPU"; the cpu image lists it the same way."""
    import os

    reg = matte.load_registry(os.path.join(matte.HERE, "models.json"))
    assert reg["birefnet-lite"]["offer"] == ["cuda", "cpu"] and reg["birefnet-lite"]["defaultFor"] == ["cuda"]
    monkeypatch.setattr(matte, "mem_available_gib", lambda: 9.0)
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry=reg, cuda=True, env={"MATTE_MODELS": None, "MATTE_DEFAULT_MODEL": "", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        lite = sc.app.models["birefnet-lite"]
        assert sorted(lite.runtimes) == ["cpu", "cuda"]
        cpu, cuda = lite.runtimes["cpu"], lite.runtimes["cuda"]
        assert cpu.precision == "fp32" and cpu.default_size == 1024 and cuda.precision == "fp32" and cuda.default_size == 1024
        assert sc.app._precheck(cpu) == "cpu: 14 GiB of RAM needed, 9 GiB available"
        assert sc.app.default_models == {"cuda": "birefnet-lite", "cpu": "isnet-anime"}
    finally:
        sc.close()
    sc = Sidecar(tmp_path, synthetic_file, models=("synthetic",), registry=reg, env={"MATTE_DEVICE": "cpu", "MATTE_MODELS": None, "MATTE_DEFAULT_MODEL": "", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        assert list(sc.app.models["birefnet-lite"].runtimes) == ["cpu"] and sc.app.default_models == {"cpu": "isnet-anime"}
    finally:
        sc.close()
