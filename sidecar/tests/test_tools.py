"""Derived graphs: tools/rescale_graph.py and tools/to_fp16.py reproduce the source graph, and the stamps memoise them."""
from __future__ import annotations

import json
import os
import subprocess
import sys

import numpy as np
import pytest

from conftest import SIDECAR, SIZE, Sidecar, frames, matte, reference


def _tool(name):
    return matte._load_module("t_" + name, os.path.join(SIDECAR, "tools", name + ".py"))


def _iou(a: np.ndarray, b: np.ndarray) -> float:
    x, y = a > 127, b > 127
    return float((x & y).sum() / max(1, (x | y).sum()))


def test_rescaled_graph_equals_a_natively_built_one(tmp_path, synthetic_file):
    native16 = str(tmp_path / "native16.onnx")
    matte.build_synthetic_graph(native16, 2 * SIZE)
    rescaled = str(tmp_path / "rescaled16.onnx")
    _tool("rescale_graph").rescale(synthetic_file, rescaled, 2 * SIZE)
    f = frames(4, size=2 * SIZE, seed=3)
    a, b = reference(f, native16), reference(f, rescaled)
    assert np.abs(a.astype(np.int16) - b.astype(np.int16)).max() <= 1
    assert _iou(a, b) >= 0.999


def test_fp16_graph_matches_fp32(tmp_path, synthetic_file):
    fp16 = str(tmp_path / "g.fp16.onnx")
    _tool("to_fp16").convert(synthetic_file, fp16)
    import onnx

    m = onnx.load(fp16)
    assert [n.op_type for n in m.graph.node][0] == "Cast" and [n.op_type for n in m.graph.node][-1] == "Cast"
    f = frames(6, seed=5)
    a, b = reference(f, synthetic_file), reference(f, fp16)
    mae = np.abs(a.astype(np.int16) - b.astype(np.int16)).mean() / 255.0
    assert mae < 0.01 and _iou(a, b) >= 0.999


def test_stamps_memoise_derivations_and_rekey_on_new_weights(tmp_path, synthetic_file):
    # MATTE_SELFTEST=0: the loader must not derive the same files concurrently (the test drives _derive itself)
    sc = Sidecar(tmp_path, synthetic_file, precision="fp16", env={"MATTE_PRELOAD": "", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        m = sc.rt()  # the cpu runtime of the synthetic model
        sc.app._derive(m)
        fp16 = sc.models_dir / "synthetic.fp16.onnx"
        r16 = sc.models_dir / f"synthetic.fp16.{2 * SIZE}.onnx"
        assert fp16.is_file() and r16.is_file()
        st = json.loads((sc.models_dir / "synthetic.fp16.onnx.stamp.json").read_text())
        assert st["src"] == m.model.weights and st["proc"] == matte.PROCESSING_VERSION and st["tool"] == "to_fp16"
        assert st["sha256"] == matte.sha256_file(str(fp16)) and st["bytes"] == fp16.stat().st_size
        assert m.graph_digest == st["sha256"] and m.precision == "fp16"
        assert m.paths == {SIZE: str(fp16), 2 * SIZE: str(r16)}
        mt = (fp16.stat().st_mtime_ns, r16.stat().st_mtime_ns)
        sc.app._derive(m)  # stamps match: nothing is rewritten
        assert (fp16.stat().st_mtime_ns, r16.stat().st_mtime_ns) == mt
        m.model.weights = "0" * 64  # a re-pinned source file: every derivation is redone
        sc.app._derive(m)
        assert json.loads((sc.models_dir / "synthetic.fp16.onnx.stamp.json").read_text())["src"] == "0" * 64
    finally:
        sc.close()


def test_failed_fp16_derivation_falls_back_to_fp32(tmp_path, synthetic_file, monkeypatch):
    sc = Sidecar(tmp_path, synthetic_file, precision="fp16", env={"MATTE_PRELOAD": "", "MATTE_SELFTEST": "0"}, serve=False)
    try:
        m = sc.rt()

        def boom(name):
            raise RuntimeError("no onnx here")

        monkeypatch.setattr(sc.app, "_tool", boom)
        sc.app._derive(m)
        assert m.precision == "fp32" and m.paths == {SIZE: m.model.file} and m.sizes == [SIZE]
        assert m.default_size == SIZE and "derivation failed" in m.last_error
        assert m.graph_digest == m.model.weights
    finally:
        sc.close()


def test_selftest_subcommand_under_python_I(tmp_path):
    env = dict(os.environ, MATTE_DEVICE="cpu", MATTE_THREADS="2")
    r = subprocess.run([sys.executable, "-I", os.path.join(SIDECAR, "matte.py"), "selftest"], capture_output=True, text=True, env=env, timeout=300)
    assert r.returncode == 0, r.stderr[-2000:]
    assert "selftest ok: devices=['cpu']" in r.stdout  # the derivation tools print their own lines first
    assert "propagate in video" not in r.stderr  # sam2's tqdm bar is silenced (when sam2 is importable at all)


def test_download_subcommand_reports_present_files(tmp_path, synthetic_file, capsys):
    import shutil

    reg_path = tmp_path / "models.json"
    shutil.copy(synthetic_file, tmp_path / "synthetic.onnx")
    spec = matte.synthetic_spec(str(tmp_path / "synthetic.onnx"))
    spec["urls"] = ["http://127.0.0.1:9/none"]
    reg_path.write_text(json.dumps({"v": 1, "models": {"synthetic": spec}}))
    cfg = matte.Config({"MATTE_MODELS_DIR": str(tmp_path), "MATTE_REGISTRY": str(reg_path)})
    assert matte.cmd_download(cfg, ["synthetic"]) == 0
    assert "present and verified" in capsys.readouterr().out
    assert matte.cmd_download(cfg, ["nope"]) == 1


@pytest.mark.parametrize("msg,oom", [("CUDA failure 2: out of memory", True), ("Failed to allocate memory for requested buffer of size 123", True), ("CUBLAS_STATUS_ALLOC_FAILED", True), ("bad_alloc", True), ("Non-zero status code returned while running Conv node", False), ("", False)])
def test_oom_classifier(msg, oom):
    assert matte.is_oom(RuntimeError(msg)) is oom
    assert matte.is_oom(MemoryError()) is True
