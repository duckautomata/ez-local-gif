"""The downloader: URL order, Range resume, servers without Range support, sha256 verification, the base-URL override."""
from __future__ import annotations

import hashlib
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import pytest

from conftest import matte

DATA = np.random.default_rng(42).integers(0, 256, 3 * 1024 * 1024 + 123, dtype=np.uint8).tobytes()
SHA = hashlib.sha256(DATA).hexdigest()


class _Handler(BaseHTTPRequestHandler):
    seen: list = []

    def log_message(self, *args):
        pass

    def do_GET(self):
        self.seen.append((self.path, self.headers.get("Range")))
        if self.path == "/missing":
            self.send_error(404)
            return
        rng = self.headers.get("Range")
        if rng and self.path == "/model.onnx":
            start = int(rng.split("=")[1].rstrip("-"))
            body = DATA[start:]
            self.send_response(206)
            self.send_header("Content-Range", f"bytes {start}-{len(DATA) - 1}/{len(DATA)}")
        else:  # /norange ignores Range and answers the whole file with 200
            body = DATA
            self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


@pytest.fixture(scope="module")
def server():
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    try:
        yield f"http://127.0.0.1:{httpd.server_address[1]}"
    finally:
        httpd.shutdown()


def test_full_download_verifies_and_renames(tmp_path, server):
    dst = str(tmp_path / "m.onnx")
    seen = []
    matte.download_model([server + "/model.onnx"], dst, SHA, len(DATA), seen.append)
    assert open(dst, "rb").read() == DATA
    assert not os.path.exists(dst + ".part")
    assert seen[-1] == len(DATA) and seen == sorted(seen)


def test_resume_sends_a_range_header_and_appends(tmp_path, server):
    dst = str(tmp_path / "m.onnx")
    with open(dst + ".part", "wb") as f:
        f.write(DATA[:1000])
    _Handler.seen.clear()
    matte.download_model([server + "/model.onnx"], dst, SHA, len(DATA))
    assert ("/model.onnx", "bytes=1000-") in _Handler.seen
    assert open(dst, "rb").read() == DATA


def test_server_without_range_support_restarts_from_zero(tmp_path, server):
    dst = str(tmp_path / "m.onnx")
    with open(dst + ".part", "wb") as f:
        f.write(b"garbage" * 100)
    matte.download_model([server + "/norange"], dst, SHA, len(DATA))
    assert open(dst, "rb").read() == DATA


def test_sha256_mismatch_discards_the_file(tmp_path, server):
    dst = str(tmp_path / "m.onnx")
    with pytest.raises(matte.DownloadError, match="sha256 mismatch"):
        matte.download_model([server + "/model.onnx"], dst, "0" * 64, len(DATA), attempts=1)
    assert not os.path.exists(dst) and not os.path.exists(dst + ".part")


def test_urls_are_tried_in_order(tmp_path, server):
    dst = str(tmp_path / "m.onnx")
    _Handler.seen.clear()
    matte.download_model([server + "/missing", server + "/model.onnx"], dst, SHA, len(DATA), attempts=1)
    assert [p for p, _ in _Handler.seen] == ["/missing", "/model.onnx"]
    assert open(dst, "rb").read() == DATA


def test_all_urls_failing_raises_after_the_attempts(tmp_path, server, monkeypatch):
    monkeypatch.setattr(matte.time, "sleep", lambda s: None)
    dst = str(tmp_path / "m.onnx")
    with pytest.raises(matte.DownloadError):
        matte.download_model([server + "/missing"], dst, SHA, len(DATA), attempts=2)
    assert not os.path.exists(dst)


def test_base_url_override_replaces_the_url_list(tmp_path):
    reg = matte.load_registry(os.path.join(matte.HERE, "models.json"))
    cfg = matte.Config({"MATTE_MODELS_DIR": str(tmp_path), "MATTE_MODELS_BASE_URL": "https://mirror.example/models/"})
    m = matte.Model("isnet-anime", reg["isnet-anime"], cfg)
    assert m.urls == ["https://mirror.example/models/isnet-anime.onnx"]
    m2 = matte.Model("isnet-anime", reg["isnet-anime"], matte.Config({"MATTE_MODELS_DIR": str(tmp_path)}))
    assert m2.urls[0].startswith("https://github.com/danielgatis/rembg/releases/download/v0.0.0/") and len(m2.urls) == 2


def test_models_json_facts():
    reg = matte.load_registry(os.path.join(matte.HERE, "models.json"))
    assert set(reg) == {"isnet-anime", "birefnet-lite", "sam2-tiny"}
    for mid, s in reg.items():
        assert len(s["sha256"]) == 64 and s["bytes"] > 0 and s["urls"] and s["licence"] in ("Apache-2.0", "MIT")
        assert os.path.isfile(os.path.join(matte.HERE, s["licence_file"]))
        assert set(s["offer"]) <= {"cuda", "cpu"} and set(s.get("defaultFor", [])) <= set(s["offer"])
        if s.get("kind") == "tracker":
            assert s["config"].endswith(".yaml") and s["image_size"] == 1024 and s["precision"] == {"cuda": "bf16", "cpu": "fp32"}
            continue
        assert s["output"]["activation"] in ("sigmoid", "logits")
        assert s["default_size"]["cuda"] in s["sizes"] and s["default_size"]["cpu"] in s["sizes"]
    assert reg["isnet-anime"]["sha256"] == "f15622d853e8260172812b657053460e20806f04b9e05147d49af7bed31a6e99"
    assert reg["birefnet-lite"]["sha256"] == "5600024376f572a557870a5eb0afb1e5961636bef4e1e22132025467d0f03333"
    assert reg["sam2-tiny"]["sha256"] == "7402e0d864fa82708a20fbd15bc84245c2f26dff0eb43a4b5b93452deb34be69" and reg["sam2-tiny"]["bytes"] == 156008466
    assert reg["isnet-anime"]["input"]["std"] == [1.0, 1.0, 1.0]  # rembg dis_anime.py, not the ImageNet std
    # Phase 5c defaults: General on the GPU, Anime on the CPU, the tracker on both and never a default
    assert reg["birefnet-lite"]["defaultFor"] == ["cuda"] and reg["isnet-anime"]["defaultFor"] == ["cpu"] and "defaultFor" not in reg["sam2-tiny"]
    assert reg["isnet-anime"]["label"] == "Anime (fast)" and reg["birefnet-lite"]["label"] == "General (precise)" and reg["sam2-tiny"]["label"] == "Guided (click to select)"
    assert reg["sam2-tiny"]["kind"] == "tracker" and reg["sam2-tiny"]["offer"] == ["cuda", "cpu"]
    assert reg["birefnet-lite"]["offer"] == ["cuda", "cpu"] and reg["birefnet-lite"]["ram"] == {"min_available_gib": 14}  # on the cpu too, gated by RAM
