"""M8: SOURCE denoising before the key (for grain / dither / 4:2:0 sources):
pre-filter the RGB, then the stacked colorkey (ring k-means, K colours) or the
single colorkey. Pre-filters:
  none, median3 (median=radius=1), median5, tmed3 (tpad clone + tmedian=radius=1,
  temporal only: kills grain, keeps dither pattern if it is static),
  hqdn3d (default), hqdn3d-strong (luma 8, chroma 6, temporal 8/6), nlmeans (s=4),
  rg (removegrain mode 4 on every plane = 3x3 median w/o centre... mode 4 is the
  median of the 3x3), gauss1 (gblur=sigma=1)."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
import run_m2
from grid import run_grid, grid

PRE = {
    "none": "null",
    "median3": "median=radius=1",
    "median5": "median=radius=2",
    "tmed3": "tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1",
    "hqdn3d": "hqdn3d",
    "hqdn3d-strong": "hqdn3d=8:6:8:6",
    "nlmeans": "nlmeans=s=4",
    "rg4": "removegrain=4:4:4",
    "gauss1": "gblur=sigma=1",
}
KS = [1, 2, 4]
SIMS = [0.05, 0.08, 0.1, 0.15, 0.2, 0.3]


def setup(variant, indir):
    f0 = L.load_frames(indir, 1)[0]
    ring = L.border_ring(f0, 4, run_m2.RING_EDGES)
    return {"colors": {k: L.kmeans_colors(ring, k) for k in KS}, "corner": L.corner_median(f0)}


def build(variant, indir, ctx, p):
    pre = PRE[p["pre"]]
    if p["k"] == 1:
        filt = "[0:v]format=rgb24,%s,format=rgba,colorkey=color=%s:similarity=%s:blend=0,alphaextract[out]" % (pre, L.hexc(ctx["corner"]), L.fnum(p["sim"]))
    else:
        stack = run_m2.stack_filter("color", ctx["colors"][p["k"]], p["sim"], 0.0)
        filt = stack.replace("[0:v]format=rgba,", "[0:v]format=rgb24,%s,format=rgba," % pre, 1)
    return {"args_in": L.input_args(indir), "filt": filt, "filter_complex": True, "chain": "pre=%s -> %s" % (pre, "colorkey" if p["k"] == 1 else "stack-colorkey k=%d" % p["k"])}


if __name__ == "__main__":
    vf = sys.argv[1].split(",") if len(sys.argv) > 1 else ["noise", "noise-gifpng", "noise-mp4", "vgrad-gifpng", "multi-gifpng", "skin-gifpng"]
    g = grid(pre=list(PRE), k=KS, sim=SIMS)
    print("== source denoise before colorkey / stacked colorkey")
    run_grid("denoise-colorkey", build, g, {"pre": "hqdn3d", "k": 4, "sim": 0.1}, variant_filter=vf, per_variant_setup=setup, save=True)
