"""Serial wall-time pass: each method at ONE setting on the 45 png frames of a
variant, ffmpeg with default threading, nothing else running. Also the numpy
wand per 45 frames. Repeats 3x, reports the median."""
import json
import os
import statistics
import sys
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
import run_m1, run_m2, run_m3, run_m4, run_m5, run_m6


def t3(fn):
    ts = []
    for _ in range(3):
        _, dt = fn()
        ts.append(dt)
    return statistics.median(ts)


def main(variant="vgrad"):
    indir = dict(L.variants())[variant]
    f0 = L.load_frames(indir, 1)[0]
    c = L.corner_median(f0)
    ring = L.border_ring(f0, 4, run_m2.RING_EDGES)
    cols4 = L.kmeans_colors(ring, 4)
    out = {}
    # decode-only baseline: the PNG decode + gray conversion
    out["decode-only (png->gray)"] = t3(lambda: L.run_ffmpeg_gray(L.input_args(indir), "format=gray"))
    b = run_m1.build_chroma(variant, indir, {"color": c}, {"sim": 0.2, "blend": 0.05})
    out["chromakey"] = t3(lambda: L.run_ffmpeg_gray(b["args_in"], b["filt"]))
    b = run_m1.build_color(variant, indir, {"color": c}, {"sim": 0.1, "blend": 0.0})
    out["colorkey"] = t3(lambda: L.run_ffmpeg_gray(b["args_in"], b["filt"]))
    for kind in ("color", "chroma"):
        filt = run_m2.stack_filter(kind, cols4, 0.08, 0.0)
        out["stack-%skey x4" % kind] = t3(lambda: L.run_ffmpeg_gray(L.input_args(indir), filt, filter_complex=True))
    h, s, v = run_m3.ffmpeg_hsv(c)
    b = run_m3.build(variant, indir, {"hsv": (h, s, v)}, {"mode": "hsv", "sim": 0.2, "blend": 0.05})
    out["hsvkey"] = t3(lambda: L.run_ffmpeg_gray(b["args_in"], b["filt"]))
    b = run_m4.build(variant, indir, {}, {"thr": 0.08, "sim": 0.1, "blend": 0.0})
    out["backgroundkey"] = t3(lambda: L.run_ffmpeg_gray(b["args_in"], b["filt"]))
    b = run_m5.build_rgbmax(variant, indir, {}, {"kind": "coons", "T": 24, "W": 16})
    out["platediff-rgbmax (coons plates ready)"] = t3(lambda: L.run_ffmpeg_gray(b["args_in"], b["filt"], filter_complex=True))
    b = run_m5.build_yuvsum(variant, indir, {}, {"kind": "median45", "sim": 0.05, "blend": 0.0})
    out["platediff-yuvsum (backgroundkey+concat)"] = t3(lambda: L.run_ffmpeg_gray(b["args_in"], b["filt"], filter_complex=True))
    # plate making itself
    import plates
    frames = L.load_frames(indir)
    t0 = time.perf_counter(); [plates.coons(f) for f in frames]; out["plates: coons x45 (numpy)"] = time.perf_counter() - t0
    t0 = time.perf_counter(); [plates.laplace(f) for f in frames]; out["plates: laplace x45 (numpy+spsolve)"] = time.perf_counter() - t0
    t0 = time.perf_counter(); np.median(frames, axis=0); out["plates: median45 (numpy)"] = time.perf_counter() - t0
    ctx = run_m6.setup(variant, indir)
    for p in ({"mode": "seed", "k": 4, "tol": 24, "med": 0}, {"mode": "grow", "tol": 8, "med": 0}, {"mode": "grow", "tol": 8, "med": 3}, {"mode": "hybrid", "tol": 8, "gtol": 64, "med": 0}):
        _, dt = run_m6.run_numpy(variant, indir, ctx, p)
        out["wand-%s (numpy, 1 thread)" % " ".join("%s=%s" % kv for kv in p.items())] = dt
    # post-processing passes on a matte
    src = os.path.join(L.RESULTS, "chromakey", variant)
    if os.path.isdir(src):
        import run_m7
        for k in ("erode1", "median3", "feather1", "tmed3"):
            out["post %s" % k] = t3(lambda: L.run_ffmpeg_gray(L.input_args(src), "format=gray,%s,format=gray" % run_m7.POST[k]))
    for k, v in out.items():
        print("%-55s %6.2f s / 45 frames = %5.1f ms/frame" % (k, v, v / 45 * 1000))
    L.write_json(os.path.join(L.RESULTS, "timing-%s.json" % variant), out)


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "vgrad")
