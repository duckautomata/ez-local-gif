"""The tracker API: /v1/track routing, prompt parsing + validation, record framing, caps, /v1/track/frame, the ping
shape of a tracker, the synthetic self-test — on the fake tracker; plus the real SAM 2 smoke (skipped without torch,
sam2 and a checkpoint) and the frame loader's parity with sam2's own."""
from __future__ import annotations

import contextlib
import importlib.util
import json
import os
import struct
import time

import numpy as np
import pytest

from conftest import FakeTracker, Sidecar, clip, decode_all, fake_mask, fake_track, frames, matte, png

W, H = 24, 16


def box_prompt(frame=0, box=(0.25, 0.25, 0.75, 0.75), points=None):
    p = {"frame": frame, "box": list(box)}
    if points:
        p["points"] = points
    return {"obj": 1, "prompts": [p]}


def disc(w: int = W, h: int = H, cx: float = 0.6, cy: float = 0.5, r: float = 0.3) -> np.ndarray:
    """A bool (h, w) mask: a disc at (cx, cy) with radius r, in fractions of the width."""
    yy, xx = np.mgrid[0:h, 0:w]
    return (xx - cx * w) ** 2 + (yy - cy * w) ** 2 <= (r * w) ** 2


def mask_png(mask: np.ndarray) -> bytes:
    return png.encode_gray(np.where(mask, 255, 0).astype(np.uint8))


def mask_record(mask: np.ndarray) -> bytes:
    """The [uint32 BE length][PNG] record the app appends to the frames for a mask prompt."""
    p = mask_png(mask)
    return struct.pack(">I", len(p)) + p


def mask_prompt(frame=0, points=None, box=None, listed=True):
    """The X-Matte-Prompts JSON of a mask prompt on `frame` (listed in "prompts" with its points / box, as the app sends it)."""
    d = {"obj": 1, "prompts": [], "mask": {"frame": frame}}
    if listed:
        d["prompts"].append({"frame": frame, "points": points or [], "box": list(box) if box else None})
    return d


def test_track_records_match_the_fake_tracker(tracker_sidecar):
    sc = tracker_sidecar
    c = clip(6, W, H)
    status, body, headers = sc.track(c, box_prompt())
    assert status == 200, body
    assert headers["Content-Type"] == matte.MATTES_CONTENT_TYPE and int(headers["Content-Length"]) == len(body)
    got = decode_all(body)
    assert got.shape == (6, H, W) and set(np.unique(got)) <= {0, 255}
    want = fake_track(6, W, H, {0: {"points": [], "labels": [], "box": (0.25 * W, 0.25 * H, 0.75 * W, 0.75 * H)}})
    assert all(np.array_equal(g >= 128, w) for g, w in zip(got, want))
    # the fake saw pixel coordinates of w×h, the object id and the whole body
    kind, n, w, h, prompts, obj, nbytes = sc.rt("tracker").sessions[64].calls[-1]
    assert (kind, n, w, h, obj, nbytes) == ("track", 6, W, H, 1, 6 * W * H * 3)
    assert prompts[0]["box"] == (0.25 * W, 0.25 * H, 0.75 * W, 0.75 * H) and prompts[0]["points"] == []


def test_points_and_later_frames_are_merged_per_frame(tracker_sidecar):
    sc = tracker_sidecar
    c = clip(8, W, H)
    prompts = {
        "obj": 3,
        "prompts": [
            {"frame": 5, "points": [[0.5, 0.5, 1], [0.9, 0.9, 0]]},
            {"frame": 2, "box": [0.1, 0.1, 0.4, 0.6], "points": [[0.2, 0.2, 1]]},
            {"frame": 5, "points": [[0.1, 0.1, 1]]},  # a second entry for frame 5 merges its points
        ],
    }
    status, body, _ = sc.track(c, prompts)
    assert status == 200, body
    _kind, _n, _w, _h, seen, obj, _ = sc.rt("tracker").sessions[64].calls[-1]
    assert obj == 3 and sorted(seen) == [2, 5]
    assert seen[2]["box"] == (0.1 * W, 0.1 * H, 0.4 * W, 0.6 * H) and seen[2]["points"] == [(0.2 * W, 0.2 * H)] and seen[2]["labels"] == [1]
    assert seen[5]["box"] is None and seen[5]["points"] == [(0.5 * W, 0.5 * H), (0.9 * W, 0.9 * H), (0.1 * W, 0.1 * H)] and seen[5]["labels"] == [1, 0, 1]
    got = decode_all(body)
    want = fake_track(8, W, H, seen)
    assert all(np.array_equal(g >= 128, w) for g, w in zip(got, want))


def test_track_frame_returns_one_png(tracker_sidecar):
    sc = tracker_sidecar
    f = clip(1, W, H)
    prompts = {"prompts": [{"frame": 7, "points": [[0.5, 0.5, 1]]}, {"frame": 7, "box": [0.0, 0.0, 0.5, 0.5]}]}
    status, body, headers = sc.track_frame(f, prompts)
    assert status == 200, body
    assert headers["Content-Type"] == "image/png" and body.startswith(b"\x89PNG")
    m = png.decode_gray(body)
    assert m.shape == (H, W)
    want = fake_mask(W, H, {"points": [(0.5 * W, 0.5 * H)], "labels": [1], "box": (0.0, 0.0, 0.5 * W, 0.5 * H)})
    assert np.array_equal(m >= 128, want)
    kind, w, h, prompt, obj, nbytes = sc.rt("tracker").sessions[64].calls[-1]
    assert (kind, w, h, obj, nbytes) == ("frame", W, H, 1, W * H * 3)
    # two different frames in one /v1/track/frame header
    status, body, _ = sc.track_frame(f, {"prompts": [{"frame": 0, "box": [0, 0, 0.5, 0.5]}, {"frame": 1, "points": [[0.5, 0.5, 1]]}]})
    assert status == 400 and "same frame" in json.loads(body)["error"]


@pytest.mark.parametrize(
    "prompts,needle",
    [
        (None, "header required"),
        ("{", "not JSON"),
        ("[]", "prompts list"),
        ({"prompts": []}, "at least one prompt"),
        ({"obj": 0, "prompts": [{"frame": 0, "box": [0, 0, 1, 1]}]}, "obj must be"),
        ({"prompts": [{"frame": 0, "points": [[0.5, 0.5, 0]]}]}, "negative clicks alone"),
        ({"prompts": [{"frame": 0}]}, "needs a box, points or the mask"),
        ({"prompts": [{"frame": 0, "box": [0, 0, 1, 1]}], "mask": 3}, "mask must be"),
        ({"prompts": [{"frame": 0, "box": [0, 0, 1, 1]}], "mask": {"frame": -1}}, "mask must be"),
        ({"prompts": [{"frame": 0, "box": [0, 0, 1, 1]}], "mask": {"frame": True}}, "mask must be"),
        ({"prompts": [{"frame": 0, "box": [0, 0, 1, 1]}], "mask": {"frame": 6}}, "mask frame 6 is outside the clip"),
        ({"prompts": [{"frame": 0, "box": [0, 0, 1.5, 1]}]}, "within 0..1"),
        ({"prompts": [{"frame": 0, "box": [0.5, 0, 0.5, 1]}]}, "x0 < x1"),
        ({"prompts": [{"frame": 0, "box": [0, 0, 1]}]}, "box must be"),
        ({"prompts": [{"frame": 0, "points": [[0.5, 0.5, 2]]}]}, "label must be"),
        ({"prompts": [{"frame": 0, "points": [[0.5, 0.5]]}]}, "each point must be"),
        ({"prompts": [{"frame": 0, "points": [[-0.1, 0.5, 1]]}]}, "within 0..1"),
        ({"prompts": [{"frame": 0, "points": [["a", 0.5, 1]]}]}, "finite number"),
        ({"prompts": [{"frame": -1, "box": [0, 0, 1, 1]}]}, "non-negative"),
        ({"prompts": [{"frame": 6, "box": [0, 0, 1, 1]}]}, "outside the clip"),
        ({"prompts": [{"frame": 2, "box": [0, 0, 1, 1]}, {"frame": 2, "box": [0, 0, 0.5, 0.5]}]}, "more than one box"),
        ({"prompts": [{"frame": i, "box": [0, 0, 1, 1]} for i in range(6)] + [{"frame": 0, "box": None, "points": [[0.5, 0.5, 1]]} for _ in range(27)]}, "exceed 32"),
    ],
)
def test_bad_prompts_are_400(tracker_sidecar, prompts, needle):
    status, body, _ = tracker_sidecar.track(clip(6, W, H), prompts)
    assert status == 400, body
    assert needle in json.loads(body)["error"]


def test_parse_prompts_converts_to_pixels():
    obj, p, mask_fi = matte.parse_prompts(json.dumps({"obj": 2, "prompts": [{"frame": 1, "points": [[0.5, 0.25, 1], [1.0, 1.0, 0]], "box": [0.0, 0.5, 1.0, 1.0]}]}), 4, 200, 100)
    assert obj == 2 and list(p) == [1] and mask_fi is None
    assert p[1]["points"] == [(100.0, 25.0), (200.0, 100.0)] and p[1]["labels"] == [1, 0] and p[1]["box"] == (0.0, 50.0, 200.0, 100.0)
    assert "mask" not in p[1]  # the handler adds the decoded record; a frame without one has no key
    with pytest.raises(matte.PromptError, match="NaN|finite"):
        matte.parse_prompts('{"prompts":[{"frame":0,"box":[0,0,NaN,1]}]}', 1, 10, 10)


def test_caps_and_wrong_endpoints(tmp_path, synthetic_file):
    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",), env={"MATTE_MAX_TRACK_FRAMES": "4"})
    try:
        sc.wait()
        sc.wait("tracker")
        c = clip(5, W, H)
        status, body, _ = sc.track(c, box_prompt())
        assert status == 413 and "MATTE_MAX_TRACK_FRAMES=4" in json.loads(body)["error"]
        status, body, _ = sc.track(c[:3], box_prompt(), frames_param=2)
        assert status == 400 and "Content-Length" in json.loads(body)["error"]
        status, body, _ = sc.track(c[:3], box_prompt(), w="x")
        assert status == 400 and "integers" in json.loads(body)["error"]
        status, body, _ = sc.track(c[:3], box_prompt(), w=0)
        assert status == 400 and "1..8192" in json.loads(body)["error"]
        status, body, _ = sc.track(c[:3], box_prompt(), frames_param=0)
        assert status == 400 and "frames must be" in json.loads(body)["error"]
        # a segmenter on the tracker endpoints and the tracker on /v1/matte
        status, body, _ = sc.track(c[:3], box_prompt(), model="synthetic")
        assert status == 400 and "use /v1/matte" in json.loads(body)["error"]
        status, body, _ = sc.track_frame(c[0], box_prompt(), model="synthetic")
        assert status == 400
        status, body, _ = sc.post("/v1/matte?model=tracker&size=8&frames=1", frames(1).tobytes())
        assert status == 400 and "use /v1/track" in json.loads(body)["error"]
        assert sc.track(c[:3], box_prompt(), model="nope")[0] == 404
        assert sc.track(c[:3], box_prompt(), device="cuda")[0] == 404
    finally:
        sc.close()


def test_track_body_over_1_gib_is_413_before_reading(tracker_sidecar):
    sc = tracker_sidecar
    c = sc.conn()
    try:
        n = matte.MAX_TRACK_BODY_BYTES // (W * H * 3) + 1
        c.putrequest("POST", f"/v1/track?model=tracker&w={W}&h={H}&frames={n}")
        c.putheader("Content-Length", str(n * W * H * 3))
        c.putheader(matte.PROMPTS_HEADER, json.dumps(box_prompt()))
        c.endheaders()
        r = c.getresponse()
        assert r.status == 413 and "exceeds" in json.loads(r.read())["error"]
    finally:
        c.close()


def test_tracker_ping_shape_and_selftest(tracker_sidecar):
    p = tracker_sidecar.ping()
    t = p["models"]["tracker"]
    assert t["kind"] == "tracker" and t["state"] == "ready" and t["precision"] == "fp32" and t["sizes"] == [64] and t["defaultSize"] == 64
    assert t["msPerFrame"]["64"] > 0 and t["resident"] is True and t["licence"] == "n/a" and t["label"] == "synthetic tracker"
    assert t["devices"]["cpu"]["size"] == 64 and t["devices"]["cpu"]["msPerFrame"] == t["msPerFrame"]
    assert t["weights"] == matte.sha256_file(str(tracker_sidecar.models_dir / "synthetic.pt")) and t["graphDigest"] == t["weights"]
    # the self-test ran the synthetic moving-square clip twice (2 frames untimed, 8 timed) through the fake
    calls = tracker_sidecar.rt("tracker").sessions[64].calls
    assert [c[1] for c in calls[:2]] == [2, matte.WARMUP_FRAMES] and calls[0][4][0]["box"] is not None


def test_a_tracker_that_loses_the_square_is_unavailable(tmp_path, synthetic_file):
    class Lost(FakeTracker):
        def track(self, body, n, w, h, prompts, obj=1):
            return [np.zeros((h, w), bool) for _ in range(n)]

    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",), tracker_factory=Lost)
    try:
        rt = sc.wait("tracker", states=("unavailable",))
        assert "lost the synthetic square" in rt.reason and not rt.sessions
        status, body, _ = sc.track(clip(2, W, H), box_prompt())
        assert status == 503 and "model unavailable" in json.loads(body)["error"]
    finally:
        sc.close()


def test_tracker_unload_and_reload(tracker_sidecar):
    sc = tracker_sidecar
    assert sc.post("/v1/unload?model=tracker")[0] == 200
    assert not sc.rt("tracker").sessions and sc.ping()["models"]["tracker"]["resident"] is False
    status, body, _ = sc.track(clip(2, W, H), box_prompt())
    assert status == 200 and len(matte.parse_records(body)) == 2
    assert sc.rt("tracker").sessions


def test_oom_in_a_track_is_mapped_like_a_batch(tmp_path, synthetic_file):
    class Oom(FakeTracker):
        def track(self, body, n, w, h, prompts, obj=1):
            if n == 5:
                raise RuntimeError("CUDA out of memory. Tried to allocate 1.20 GiB")
            return super().track(body, n, w, h, prompts, obj)

    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",), tracker_factory=Oom)
    try:
        sc.wait("tracker")
        assert sc.rt("synthetic").sessions  # another runtime resident on the same device
        status, body, _ = sc.track(clip(5, W, H), box_prompt())
        assert status == 503, body  # the other runtime was released: retry
        j = json.loads(body)
        assert j["error"] == "out of memory" and j["retryAfterMs"] == matte.OOM_RETRY_MS and not sc.rt("synthetic").sessions
        status, body, _ = sc.track(clip(5, W, H), box_prompt())
        assert status == 507 and "out of memory" in json.loads(body)["error"]  # nothing left to release
        rt = sc.wait("tracker", states=("unavailable", "loading", "ready"), timeout=5)
        assert rt.last_error.startswith("RuntimeError")
        sc.wait("tracker", timeout=20)  # re-tested by the loader
        assert sc.track(clip(3, W, H), box_prompt())[0] == 200
    finally:
        sc.close()


def test_synthetic_track_clip_and_box_mask():
    body, boxes = matte.synthetic_track_clip(3, 32, 20)
    assert len(body) == 3 * 32 * 20 * 3 and len(boxes) == 3
    assert boxes[1][0] == boxes[0][0] + 1 and boxes[1][1] == boxes[0][1]  # one pixel per frame, to the right
    f = np.frombuffer(body, np.uint8).reshape(3, 20, 32, 3)
    x0, y0, x1, y1 = boxes[2]
    assert f[2, y0:y1 + 1, x0:x1 + 1, 0].min() >= 230 and f[2, :, :, 0][~matte.box_mask(boxes[2], 32, 20)].max() < 100
    assert matte.mask_iou(matte.box_mask(boxes[0], 32, 20), matte.box_mask(boxes[0], 32, 20)) == 1.0


# ---------------------------------------------------------------------------
# the mask prompt (Phase 5d): one [uint32 BE length][PNG] record after the frames, applied first on its frame
# ---------------------------------------------------------------------------
def test_track_mask_record_reaches_the_tracker(tracker_sidecar):
    sc = tracker_sidecar
    c = clip(6, W, H)
    m = disc()
    status, body, headers = sc.track(c, mask_prompt(frame=2), body=c.tobytes() + mask_record(m))
    assert status == 200, body
    assert headers["Content-Type"] == matte.MATTES_CONTENT_TYPE and int(headers["Content-Length"]) == len(body)
    kind, n, w, h, seen, obj, nbytes = sc.rt("tracker").sessions[64].calls[-1]
    assert (kind, n, w, h, obj, nbytes) == ("track", 6, W, H, 1, 6 * W * H * 3)  # the frames alone reach the tracker
    assert list(seen) == [2] and seen[2]["box"] is None and seen[2]["points"] == [] and seen[2]["labels"] == []
    assert seen[2]["mask"].dtype == bool and seen[2]["mask"].shape == (H, W) and np.array_equal(seen[2]["mask"], m)
    got = decode_all(body)
    want = fake_track(6, W, H, seen)
    assert got.shape == (6, H, W) and all(np.array_equal(g >= 128, x) for g, x in zip(got, want))
    assert np.array_equal(got[2] >= 128, m)  # the prompted frame's mask is the mask
    # the mask frame need not be listed under "prompts": the same request without the entry is the same track
    status, body2, _ = sc.track(c, mask_prompt(frame=2, listed=False), body=c.tobytes() + mask_record(m))
    assert status == 200 and body2 == body
    _kind, _n, _w, _h, seen2, _obj, _nb = sc.rt("tracker").sessions[64].calls[-1]
    assert list(seen2) == [2] and np.array_equal(seen2[2]["mask"], m)


def test_mask_then_points_and_box_on_its_frame_and_prompts_elsewhere(tracker_sidecar):
    """The mask conditions its frame first; the frame's points / box come after it (the fake removes the negative disc
    from the mask); other prompted frames are untouched by the mask."""
    sc = tracker_sidecar
    c = clip(8, W, H)
    m = disc()
    prompts = {
        "obj": 2,
        "prompts": [
            {"frame": 3, "points": [[0.6, 0.5, 0]]},  # a − click inside the disc
            {"frame": 3, "box": [0.1, 0.1, 0.3, 0.3]},  # merges with the entry above (one box per frame)
            {"frame": 6, "points": [[0.5, 0.5, 1]]},
        ],
        "mask": {"frame": 3},
    }
    status, body, _ = sc.track(c, prompts, body=c.tobytes() + mask_record(m))
    assert status == 200, body
    _kind, _n, _w, _h, seen, obj, _nb = sc.rt("tracker").sessions[64].calls[-1]
    assert obj == 2 and sorted(seen) == [3, 6]
    assert np.array_equal(seen[3]["mask"], m) and seen[3]["labels"] == [0] and seen[3]["box"] == (0.1 * W, 0.1 * H, 0.3 * W, 0.3 * H)
    assert "mask" not in seen[6] and seen[6]["labels"] == [1]
    got = decode_all(body)
    f3 = got[3] >= 128
    assert m[int(0.5 * H), int(0.6 * W)]  # the − click lands inside the disc …
    assert not f3[int(0.5 * H), int(0.6 * W)] and f3[int(0.2 * H), int(0.2 * W)]  # … and carved it; the box added its own
    assert np.array_equal(f3, fake_mask(W, H, seen[3]))
    # a mask alone counts as the positive prompt: a − click elsewhere does not make the set "negative only"
    status, body, _ = sc.track(c, {"prompts": [{"frame": 1, "points": [[0.5, 0.5, 0]]}], "mask": {"frame": 0}}, body=c.tobytes() + mask_record(m))
    assert status == 200, body


def test_track_frame_with_a_mask(tracker_sidecar):
    sc = tracker_sidecar
    f = clip(1, W, H)
    m = disc()
    prompts = mask_prompt(frame=7, points=[[0.2, 0.2, 1]])
    status, body, headers = sc.track_frame(f, prompts, body=f.tobytes() + mask_record(m))
    assert status == 200, body
    assert headers["Content-Type"] == "image/png"
    kind, w, h, prompt, obj, nbytes = sc.rt("tracker").sessions[64].calls[-1]
    assert (kind, w, h, obj, nbytes) == ("frame", W, H, 1, W * H * 3) and np.array_equal(prompt["mask"], m) and prompt["labels"] == [1]
    assert np.array_equal(png.decode_gray(body) >= 128, fake_mask(W, H, prompt))
    # the mask names the clicked frame — always, in /v1/track/frame
    status, body, _ = sc.track_frame(f, {"prompts": [{"frame": 7, "box": [0, 0, 0.5, 0.5]}], "mask": {"frame": 3}}, body=f.tobytes() + mask_record(m))
    assert status == 400 and "same frame" in json.loads(body)["error"]
    # a mask alone, on the clip's slot 7 (passed through, not range-checked)
    status, body, _ = sc.track_frame(f, mask_prompt(frame=7, listed=False), body=f.tobytes() + mask_record(m))
    assert status == 200 and np.array_equal(png.decode_gray(body) >= 128, m)


def test_bad_mask_records_are_400(tracker_sidecar):
    sc = tracker_sidecar
    c = clip(6, W, H)
    m = disc()
    frames_b = c.tobytes()
    rec = mask_record(m)
    p = mask_png(m)

    def bad(prompts, body, needle, status=400):
        st, out, _ = sc.track(c, prompts, body=body)
        assert st == status, (st, out)
        assert needle in json.loads(out)["error"], out

    bad(mask_prompt(), frames_b, "needs its record")  # declared, no record
    bad(box_prompt(), frames_b + rec, "Content-Length")  # a record, no mask prompt
    bad(box_prompt(), frames_b + rec, "needs a mask prompt in the header")
    bad(mask_prompt(), frames_b + struct.pack(">I", len(p) + 1) + p, "declares")  # length field off by one
    bad(mask_prompt(), frames_b + rec + rec, "declares")  # two records
    bad(mask_prompt(), frames_b + struct.pack(">I", 12) + b"x" * 12, "not a PNG")
    bad(mask_prompt(), frames_b + struct.pack(">I", 12) + png.SIGNATURE + b"xxxx", "cannot be decoded")
    wrong = mask_png(disc(W + 2, H))
    bad(mask_prompt(), frames_b + struct.pack(">I", len(wrong)) + wrong, "the frames are 24×16")
    cap = matte.mask_record_cap(W, H)
    bad(mask_prompt(), frames_b + struct.pack(">I", cap + 1) + b"\0" * (cap + 1), "exceeds")  # over the cap: refused before reading
    assert sc.track(c, mask_prompt(), body=frames_b + rec)[0] == 200  # and the connection / sidecar are fine after all that


def test_parse_prompts_mask():
    obj, p, mask_fi = matte.parse_prompts(json.dumps({"prompts": [{"frame": 1, "points": [[0.5, 0.25, 0]]}], "mask": {"frame": 1}}), 4, 200, 100)
    assert (obj, mask_fi) == (1, 1) and p[1]["labels"] == [0] and p[1]["points"] == [(100.0, 25.0)] and "mask" not in p[1]
    obj, p, mask_fi = matte.parse_prompts('{"prompts":[],"mask":{"frame":3}}', 4, 10, 10)
    assert mask_fi == 3 and p == {3: {"points": [], "labels": [], "box": None}}
    _obj, p, mask_fi = matte.parse_prompts('{"prompts":[{"frame":9,"box":[0,0,1,1]}],"mask":{"frame":9}}', 1, 10, 10, single=True)
    assert mask_fi == 9 and list(p) == [9]  # the clip slot passes through
    with pytest.raises(matte.PromptError, match="same frame"):
        matte.parse_prompts('{"prompts":[{"frame":9,"box":[0,0,1,1]}],"mask":{"frame":8}}', 1, 10, 10, single=True)
    with pytest.raises(matte.PromptError, match="outside the clip"):
        matte.parse_prompts('{"prompts":[],"mask":{"frame":4}}', 4, 10, 10)
    with pytest.raises(matte.PromptError, match="at least one prompt"):
        matte.parse_prompts('{"prompts":[]}', 4, 10, 10)
    with pytest.raises(matte.PromptError, match="mask must be"):
        matte.parse_prompts('{"prompts":[],"mask":{"frame":"0"}}', 4, 10, 10)


def test_decode_mask_and_record_cap():
    m = disc()
    p = mask_png(m)
    assert np.array_equal(matte.decode_mask(p, W, H), m)
    assert np.array_equal(matte.parse_mask_record(mask_record(m), W, H), m)
    with pytest.raises(matte.PromptError, match="not a PNG"):
        matte.decode_mask(b"abc" * 10, W, H)
    with pytest.raises(matte.PromptError, match="the frames are"):
        matte.decode_mask(p, W + 1, H)
    with pytest.raises(matte.PromptError, match="shorter"):
        matte.parse_mask_record(b"\0\0", W, H)
    with pytest.raises(matte.PromptError, match="declares"):
        matte.parse_mask_record(mask_record(m) + b"x", W, H)
    # a soft matte thresholds at 128
    soft = np.full((H, W), 127, np.uint8)
    soft[0, 0] = 128
    got = matte.decode_mask(png.encode_gray(soft), W, H)
    assert got[0, 0] and got.sum() == 1
    # the record cap holds the worst case, an incompressible (noise) mask, at the test size and at 1024²
    rng = np.random.default_rng(5)
    for w, h in ((W, H), (1024, 1024), (1024, 576)):
        noise = rng.integers(0, 256, (h, w), dtype=np.uint8)
        assert len(png.encode_gray(noise)) <= matte.mask_record_cap(w, h)
    if importlib.util.find_spec("PIL") is not None:
        import io

        from PIL import Image

        rgb = np.zeros((H, W, 3), np.uint8)
        rgb[m] = 255
        buf = io.BytesIO()
        Image.fromarray(rgb).save(buf, "PNG")
        assert np.array_equal(matte.decode_mask(buf.getvalue(), W, H), m)  # any PNG colour type: its luma
        buf = io.BytesIO()
        Image.fromarray(rgb).save(buf, "JPEG")
        with pytest.raises(matte.PromptError, match="not a PNG"):
            matte.decode_mask(buf.getvalue(), W, H)


def test_ping_lists_the_prompt_kinds_and_the_selftest_ran_the_mask_pass(tracker_sidecar):
    p = tracker_sidecar.ping()
    assert p["models"]["tracker"]["prompts"] == ["box", "points", "mask"] and p["models"]["synthetic"]["prompts"] == []
    # the start self-test: the box clip twice (2 untimed, 8 timed), then the 2-frame clip from a mask prompt
    calls = tracker_sidecar.rt("tracker").sessions[64].calls
    assert [c[1] for c in calls[:3]] == [2, matte.WARMUP_FRAMES, 2]
    assert calls[2][4][0]["box"] is None and calls[2][4][0]["mask"].shape == matte.TRACK_SELFTEST_WH[::-1] and calls[1][4][0].get("mask") is None


def test_a_tracker_that_loses_the_square_from_a_mask_is_unavailable(tmp_path, synthetic_file):
    class LostMask(FakeTracker):
        def track(self, body, n, w, h, prompts, obj=1):
            if any(p.get("mask") is not None for p in prompts.values()):
                return [np.zeros((h, w), bool) for _ in range(n)]
            return super().track(body, n, w, h, prompts, obj)

    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",), tracker_factory=LostMask)
    try:
        rt = sc.wait("tracker", states=("unavailable",))
        assert "from a mask prompt" in rt.reason and not rt.sessions
    finally:
        sc.close()


# ---------------------------------------------------------------------------
# the real thing (skipped without torch + sam2; the smoke also needs the checkpoint)
# ---------------------------------------------------------------------------
def _sam2_available() -> bool:
    return importlib.util.find_spec("torch") is not None and importlib.util.find_spec("sam2") is not None


@pytest.mark.skipif(not _sam2_available(), reason="torch / sam2 not installed")
def test_frames_loader_matches_sam2s_own(tmp_path):
    import torch
    from PIL import Image
    from sam2.utils.misc import _load_img_as_tensor

    body, _ = matte.synthetic_track_clip(2, 40, 30)
    f = np.frombuffer(body, np.uint8).reshape(2, 30, 40, 3)
    p = tmp_path / "f1.png"
    Image.fromarray(f[1]).save(p)
    want, h, w = _load_img_as_tensor(str(p), 64)
    mean = torch.tensor(matte.SAM2_IMAGE_MEAN)[:, None, None]
    std = torch.tensor(matte.SAM2_IMAGE_STD)[:, None, None]
    want = (want.float() - mean) / std
    fr = matte.Frames(body, 2, 40, 30, 64, "cpu")
    assert len(fr) == 2 and (h, w) == (30, 40)
    assert torch.allclose(fr[1], want, atol=1e-5)
    with pytest.raises(IndexError):
        fr[2]


@pytest.mark.skipif(not _sam2_available(), reason="torch / sam2 not installed")
def test_tracker_plumbing_with_random_weights():
    note = matte._selftest_tracker_plumbing("cpu", 2)
    assert note.startswith("tracker plumbing ok")


CKPT = os.environ.get("EZLG_SAM2_CHECKPOINT", "")


@pytest.mark.skipif(not (_sam2_available() and CKPT and os.path.isfile(CKPT)), reason="EZLG_SAM2_CHECKPOINT not set")
def test_real_sam2_tiny_tracks_the_synthetic_square():
    import torch

    device = "cuda" if torch.cuda.is_available() else "cpu"
    tr = matte.Sam2Tracker({"config": "configs/sam2.1/sam2.1_hiera_t.yaml"}, device, CKPT, 1024, "bf16" if device == "cuda" else "fp32", 4)
    w, h = matte.TRACK_SELFTEST_WH
    body, boxes = matte.synthetic_track_clip(6, w, h)
    masks = tr.track(body, 6, w, h, {0: {"points": [], "labels": [], "box": tuple(float(v) for v in boxes[0])}})
    for i, (m, box) in enumerate(zip(masks, boxes)):
        assert m.shape == (h, w)
        assert matte.mask_iou(m, matte.box_mask(box, w, h)) >= 0.5, f"frame {i}"
    one = tr.track_frame(body[: w * h * 3], w, h, {"points": [], "labels": [], "box": tuple(float(v) for v in boxes[0])})
    assert matte.mask_iou(one, matte.box_mask(boxes[0], w, h)) >= 0.5
    # the mask prompt (Phase 5d): the square's box as a mask on frame 0 tracks like the box
    square = matte.box_mask(boxes[0], w, h)
    masks = tr.track(body, 6, w, h, {0: {"points": [], "labels": [], "box": None, "mask": square}})
    for i, (m, box) in enumerate(zip(masks, boxes)):
        assert matte.mask_iou(m, matte.box_mask(box, w, h)) >= 0.5, f"mask prompt, frame {i}"
    assert matte.mask_iou(masks[0], square) >= 0.9  # the prompted frame's output is the mask (1024² resample aside)
    # its refinement: a − click at the square's centre removes the square (the smallest region SAM sees there), a +
    # click there changes nothing, the box keeps the mask inside it
    x0, y0, x1, y1 = boxes[0]
    centre = [((x0 + x1) / 2.0, (y0 + y1) / 2.0)]
    minus = tr.track_frame(body[: w * h * 3], w, h, {"points": centre, "labels": [0], "box": None, "mask": square})
    assert minus.sum() < 0.5 * square.sum(), f"the − click left {int(minus.sum())} of {int(square.sum())} px"
    plus = tr.track_frame(body[: w * h * 3], w, h, {"points": centre, "labels": [1], "box": None, "mask": square})
    assert matte.mask_iou(plus, square) >= 0.8
    half = (float(x0), float(y0), float((x0 + x1) / 2.0), float(y1 + 1))
    boxed = tr.track_frame(body[: w * h * 3], w, h, {"points": [], "labels": [], "box": half, "mask": square})
    assert 0.3 * square.sum() <= boxed.sum() <= 0.7 * square.sum() and not boxed[:, int(x1) :].any()


def test_track_frame_and_unload_while_a_track_holds_the_runtime(tmp_path, synthetic_file):
    """The live overlay never queues behind a running track: /v1/track/frame answers 503 "model busy" + retryAfterMs
    after FRAME_LOCK_WAIT_S (the app maps it to a pending the overlay retries), and /v1/unload skips the busy runtime
    (listed under "busy", its session intact) instead of blocking behind the whole track."""
    import threading

    started, release = threading.Event(), threading.Event()

    class Blocking(FakeTracker):
        def track(self, body, n, w, h, prompts, obj=1):
            if (w, h) == (W, H):  # the test's clip only — the start self-test (TRACK_SELFTEST_WH) runs through
                started.set()
                assert release.wait(30), "the test never released the track"
            return super().track(body, n, w, h, prompts, obj)

    sc = Sidecar(tmp_path, synthetic_file, trackers=("tracker",), tracker_factory=Blocking)
    try:
        sc.wait("tracker")
        sc.wait_resident("tracker")
        result = {}

        def run_track():
            result["track"] = sc.track(clip(3, W, H), box_prompt())

        t = threading.Thread(target=run_track, daemon=True)
        t.start()
        assert started.wait(10)
        t0 = time.time()
        status, body, _ = sc.track_frame(clip(1, W, H), box_prompt(frame=1))
        waited = time.time() - t0
        assert status == 503, body
        j = json.loads(body)
        assert j["error"] == "model busy" and j["retryAfterMs"] == matte.BUSY_RETRY_MS
        assert matte.FRAME_LOCK_WAIT_S - 0.2 <= waited < matte.FRAME_LOCK_WAIT_S + 5
        # unload does not wait for the track: the busy runtime keeps its session and is reported
        t0 = time.time()
        status, body, _ = sc.post("/v1/unload")
        assert status == 200 and time.time() - t0 < matte.UNLOAD_LOCK_WAIT_S + 5
        j = json.loads(body)
        assert j["sessions"] == [{"model": "synthetic", "device": "cpu"}] and j["busy"] == [{"model": "tracker", "device": "cpu"}]
        assert sc.rt("tracker").sessions and not sc.rt("synthetic").sessions
        release.set()
        t.join(30)
        status, body, _ = result["track"]
        assert status == 200 and len(matte.parse_records(body)) == 3
        # the runtime free again: the overlay and the unload go through
        assert sc.track_frame(clip(1, W, H), box_prompt(frame=1))[0] == 200
        status, body, _ = sc.post("/v1/unload")
        assert status == 200 and json.loads(body)["busy"] == [] and not sc.rt("tracker").sessions
    finally:
        release.set()
        sc.close()


# ---------------------------------------------------------------------------
# Sam2Tracker._refine on a fake image predictor (no torch, no sam2): the geometric refinement of a mask prompt
# ---------------------------------------------------------------------------
class _NullTorch:
    """The torch surface _refine touches (inference_mode, autocast), for a tracker built without the runtime."""

    @staticmethod
    def inference_mode():
        return contextlib.nullcontext()

    @staticmethod
    def autocast(device, enabled=True, dtype=None):
        return contextlib.nullcontext()


class _FakeImagePredictor:
    """SAM2ImagePredictor stand-in: predict answers the candidates it was given, whatever the point."""

    def __init__(self, cands, scores):
        self.cands, self.scores = np.asarray(cands, bool), np.asarray(scores, np.float32)
        self.images, self.resets, self.points = 0, 0, []

    def set_image(self, im):
        assert im.flags.writeable and im.shape == (H, W, 3)
        self.images += 1

    def predict(self, point_coords, point_labels, multimask_output):
        assert multimask_output and point_labels.tolist() == [1]
        self.points.append(tuple(point_coords[0].tolist()))
        return self.cands, self.scores, None

    def reset_predictor(self):
        self.resets += 1


def refiner(cands, scores) -> matte.Sam2Tracker:
    tr = matte.Sam2Tracker.__new__(matte.Sam2Tracker)
    tr.torch, tr.device, tr.precision = _NullTorch(), "cpu", "fp32"
    tr._image = _FakeImagePredictor(cands, scores)
    return tr


def test_refine_click_no_candidate_contains_is_a_noop():
    """A click that no candidate contains (the background) changes nothing — for a − click as for a + click. Before
    (2026-10-09 review): the fallback picked SAM's IoU-best candidate, so a − click on flat background — where that
    candidate is often a large region or the whole frame — carved most of the mask out, on the overlay and in the
    track alike."""
    frame = np.zeros((H, W, 3), np.uint8)
    mask = disc()
    click = (int(0.6 * W), int(0.5 * H))  # inside the disc
    assert mask[click[1], click[0]]
    big = np.ones((H, W), bool)
    big[click[1], click[0]] = False  # the IoU-best candidate: everything but the clicked pixel
    far = np.zeros((H, W), bool)
    far[0:4, 0:6] = True
    tr = refiner([big, far, far], [0.95, 0.5, 0.1])
    for label in (0, 1):
        out = tr._refine(frame, W, H, mask, {"points": [click], "labels": [label], "box": None})
        assert np.array_equal(out, mask), f"label {label} changed the mask ({int(out.sum())} vs {int(mask.sum())} px)"
    assert tr._image.images == 2 and tr._image.resets == 2 and tr._image.points == [click, click]
    assert np.array_equal(mask, disc())  # the input is never modified


def test_refine_smallest_containing_candidate_and_the_box():
    """The rule that stays: a − click removes and a + click adds the SMALLEST candidate containing the click (never the
    IoU-best one); the box keeps the mask inside it, before the points."""
    frame = np.zeros((H, W, 3), np.uint8)
    mask = disc()
    click = (int(0.6 * W), int(0.5 * H))
    whole = np.ones((H, W), bool)
    small = np.zeros((H, W), bool)
    small[click[1] - 1 : click[1] + 2, click[0] - 1 : click[0] + 2] = True
    far = np.zeros((H, W), bool)
    far[0:4, 0:6] = True
    tr = refiner([whole, far, small], [0.95, 0.5, 0.1])
    minus = tr._refine(frame, W, H, mask, {"points": [click], "labels": [0], "box": None})
    assert np.array_equal(minus, mask & ~small) and minus.sum() == mask.sum() - 9
    plus = tr._refine(frame, W, H, ~mask, {"points": [click], "labels": [1], "box": None})
    assert np.array_equal(plus, ~mask | small)
    # the box first (mask ∩ box), then the click on what is left
    box = (0.0, 0.0, 14.0, float(H))  # whole pixels: the box's right edge is exclusive at ceil(x1)
    boxed = tr._refine(frame, W, H, mask, {"points": [click], "labels": [0], "box": box})
    keep = np.zeros((H, W), bool)
    keep[:, :14] = True
    assert np.array_equal(boxed, (mask & keep) & ~small)
    assert np.array_equal(tr._refine(frame, W, H, mask, {"points": [], "labels": [], "box": box}), mask & keep)
