"""Shared helpers for the classical (ffmpeg / numpy) background-removal benchmark.

Paths (every script in this directory goes through these):
  EZLG_BENCH_CORPUS   corpus directory (default ../corpus, built by ../make-corpus.sh)
  EZLG_BENCH_RESULTS  results root (default ../results); this benchmark writes
                      under <results>/classical/
  FFMPEG              ffmpeg binary (default: ffmpeg on PATH)

The scripts here import each other by file name, so run them with their own
directory as the import root: `python -I classical/run_m1.py ...` works because
every script inserts its directory into sys.path first.
"""
import json
import os
import shutil
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor

import numpy as np
from PIL import Image

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)
import metrics  # noqa: E402  (classical/metrics.py: square-SE boundary band + gif_* extras)

BENCH_ROOT = os.path.dirname(HERE)
FFMPEG = os.environ.get("FFMPEG", "ffmpeg")
CORPUS = os.path.abspath(os.environ.get("EZLG_BENCH_CORPUS") or os.path.join(BENCH_ROOT, "corpus"))
RESULTS = os.path.join(os.path.abspath(os.environ.get("EZLG_BENCH_RESULTS") or os.path.join(BENCH_ROOT, "results")), "classical")
INPUTS = os.path.join(RESULTS, "_inputs")  # derived inputs (noise-mp4 frames)
N = 45
W = H = 720
FPS = 15

BGS = ["green", "white", "black", "vgrad", "multi", "skin", "anim", "busy", "noise"]


def noise_mp4_frames():
    """<results>/classical/_inputs/noise-mp4/001..045.png = corpus/bg/noise/noise.mp4
    decoded by ffmpeg to PNG (the 4:2:0 crf-18 version of the noise variant),
    made on first use. None when the corpus has no noise.mp4."""
    d = os.path.join(INPUTS, "noise-mp4")
    mp4 = os.path.join(CORPUS, "bg", "noise", "noise.mp4")
    if not os.path.isfile(mp4):
        return None
    have = len([n for n in os.listdir(d) if n.lower().endswith(".png")]) if os.path.isdir(d) else 0
    if have < N:
        os.makedirs(d, exist_ok=True)
        print("decoding %s -> %s" % (mp4, d), flush=True)
        subprocess.run([FFMPEG, "-v", "error", "-nostdin", "-y", "-i", mp4, "-vf", "format=rgb24", "-frames:v", str(N), os.path.join(d, "%03d.png")], check=True)
    return d


def variants():
    """(name, input_dir) for every corpus input variant: 9 png + 4 gifpng + noise-mp4."""
    v = [(bg, os.path.join(CORPUS, "bg", bg, "png")) for bg in BGS]
    v += [(bg + "-gifpng", os.path.join(CORPUS, "bg", bg, "gifpng")) for bg in ["vgrad", "multi", "skin", "noise"]]
    nm = noise_mp4_frames()
    if nm:
        v.append(("noise-mp4", nm))
    return v


_gt_cache = {}


def load_gt():
    if "gt" not in _gt_cache:
        gts = [np.asarray(Image.open(os.path.join(CORPUS, "gt", "%03d.png" % i)).convert("L"), dtype=np.uint8) for i in range(1, N + 1)]
        bands = [metrics.band_mask(g > 127) for g in gts]
        _gt_cache["gt"] = (gts, bands)
    return _gt_cache["gt"]


def load_frames(indir, n=N):
    return np.stack([np.asarray(Image.open(os.path.join(indir, "%03d.png" % i)).convert("RGB"), dtype=np.uint8) for i in range(1, n + 1)])


def score(preds):
    gts, bands = load_gt()
    return metrics.score_arrays(list(preds), gts, bands)


def input_args(indir):
    return ["-framerate", str(FPS), "-i", os.path.join(indir, "%03d.png")]


def run_ffmpeg_gray(args_in, filt, n=N, extra_out=(), filter_complex=False, threads=None):
    """Run ffmpeg with the given input args and a filter ending in gray output;
    returns (array (n,H,W) uint8, wall seconds). filt is -vf unless filter_complex."""
    cmd = [FFMPEG, "-v", "error", "-nostdin"]
    if threads:
        cmd += ["-threads", str(threads), "-filter_threads", str(threads)]
    cmd += list(args_in)
    if filter_complex:
        cmd += ["-filter_complex", filt, "-map", "[out]"]
    else:
        cmd += ["-vf", filt]
    cmd += list(extra_out) + ["-frames:v", str(n), "-f", "rawvideo", "-pix_fmt", "gray", "-"]
    t0 = time.perf_counter()
    p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    dt = time.perf_counter() - t0
    if p.returncode != 0:
        raise RuntimeError("ffmpeg failed (%d): %s\n%s" % (p.returncode, " ".join(cmd), p.stderr.decode(errors="replace")[-2000:]))
    buf = np.frombuffer(p.stdout, dtype=np.uint8)
    if buf.size != n * H * W:
        raise RuntimeError("ffmpeg produced %d bytes, expected %d: %s" % (buf.size, n * H * W, " ".join(cmd)))
    return buf.reshape(n, H, W), dt


def save_mattes(arr, outdir):
    if os.path.isdir(outdir):
        shutil.rmtree(outdir)
    os.makedirs(outdir, exist_ok=True)
    for i, a in enumerate(arr):
        Image.fromarray(a, "L").save(os.path.join(outdir, "%03d.png" % (i + 1)), compress_level=1)


def corner_median(frame, p=8):
    cs = [frame[:p, :p], frame[:p, -p:], frame[-p:, :p], frame[-p:, -p:]]
    meds = np.stack([np.median(c.reshape(-1, 3), 0) for c in cs])
    return tuple(int(round(v)) for v in np.median(meds, 0))


def border_ring(frame, width=4, edges=("top", "left", "right", "bottom")):
    parts = []
    if "top" in edges:
        parts.append(frame[:width, :].reshape(-1, 3))
    if "bottom" in edges:
        parts.append(frame[-width:, :].reshape(-1, 3))
    if "left" in edges:
        parts.append(frame[:, :width].reshape(-1, 3))
    if "right" in edges:
        parts.append(frame[:, -width:].reshape(-1, 3))
    return np.concatenate(parts).astype(np.float32)


def kmeans_colors(pix, k, iters=25, seed=0):
    """Plain k-means (k-means++ init) over Nx3 float pixels; returns k RGB tuples sorted by cluster size desc."""
    rng = np.random.default_rng(seed)
    if len(pix) > 20000:
        pix = pix[rng.choice(len(pix), 20000, replace=False)]
    cents = [pix[rng.integers(len(pix))]]
    for _ in range(1, k):
        d = np.min([((pix - c) ** 2).sum(1) for c in cents], axis=0)
        probs = d / d.sum() if d.sum() > 0 else np.full(len(pix), 1.0 / len(pix))
        cents.append(pix[rng.choice(len(pix), p=probs)])
    cents = np.stack(cents)
    for _ in range(iters):
        d = ((pix[:, None, :] - cents[None, :, :]) ** 2).sum(2)
        lab = d.argmin(1)
        new = np.stack([pix[lab == j].mean(0) if (lab == j).any() else cents[j] for j in range(k)])
        if np.allclose(new, cents):
            break
        cents = new
    d = ((pix[:, None, :] - cents[None, :, :]) ** 2).sum(2)
    lab = d.argmin(1)
    sizes = np.bincount(lab, minlength=k)
    order = np.argsort(-sizes)
    return [tuple(int(round(v)) for v in cents[j]) for j in order]


def hexc(rgb):
    return "0x%02x%02x%02x" % tuple(int(v) & 255 for v in rgb)


def rgb_to_yuv_limited(rgb):
    """BT.601 limited-range 8-bit, what swscale's rgb24 -> yuv444p does (within rounding)."""
    r, g, b = [v / 255.0 for v in rgb]
    y = 16 + 65.481 * r + 128.553 * g + 24.966 * b
    u = 128 - 37.797 * r - 74.203 * g + 112.0 * b
    v = 128 + 112.0 * r - 93.786 * g - 18.214 * b
    return y, u, v


def fnum(x):
    s = ("%.4f" % x).rstrip("0").rstrip(".")
    return s if s else "0"


def pmap(fn, jobs, workers=6):
    with ThreadPoolExecutor(max_workers=workers) as ex:
        return list(ex.map(fn, jobs))


def best_of(results, key="mae"):
    return min(results, key=lambda r: r[key])


def write_json(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(obj, f, indent=1)


def summarize(name, rows):
    """rows: list of dict(variant, params, metrics...). Prints a per-variant best table."""
    print("== %s" % name)
    for v, _ in variants():
        rs = [r for r in rows if r["variant"] == v]
        if not rs:
            continue
        b = best_of(rs)
        print("  %-13s best mae=%.4f iou=%.4f bmae=%.4f flick=%+.4f  %s" % (v, b["mae"], b["iou"], b["boundary_mae"], b["flicker"], b["params"]))
