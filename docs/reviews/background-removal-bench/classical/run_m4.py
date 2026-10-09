"""M4: backgroundkey as shipped. Per the source its reference is the FIRST frame
(copied on the first filter_frame); after each frame the summed |dY|+|dU|+|dV|
over the picture is compared with threshold*max_sum and, when larger, the CURRENT
frame replaces the reference (a scene-change detector). threshold=1 => the first
frame is the reference for ever; threshold=0 => every frame replaces it, i.e. a
previous-frame motion key. similarity: a pixel is keyed when
|dY|+|dU|+|dV| <= 765*similarity; blend ramps alpha over 255*blend diff units."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid

THRS = [0.0, 0.08, 1.0]
SIMS = [0.02, 0.05, 0.1, 0.2, 0.3]
BLENDS = [0.0, 0.1]


def build(variant, indir, ctx, p):
    filt = "format=yuva444p,backgroundkey=threshold=%s:similarity=%s:blend=%s,alphaextract,format=gray" % (L.fnum(p["thr"]), L.fnum(p["sim"]), L.fnum(p["blend"]))
    return {"args_in": L.input_args(indir), "filt": filt, "chain": "format=yuva444p,backgroundkey=threshold=T:similarity=S:blend=B"}


if __name__ == "__main__":
    vf = sys.argv[1].split(",") if len(sys.argv) > 1 else None
    smoke = os.environ.get("SMOKE") == "1"
    g = grid(thr=THRS, sim=SIMS, blend=BLENDS) if not smoke else grid(thr=[1.0], sim=[0.1], blend=[0.0])
    print("== backgroundkey (reference = first frame / scene-change)")
    run_grid("backgroundkey", build, g, {"thr": 0.08, "sim": 0.1, "blend": 0.0}, variant_filter=vf, save=not smoke)
