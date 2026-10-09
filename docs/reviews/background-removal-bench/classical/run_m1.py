"""M1: the app's single-colour chromakey (format=yuva444p,chromakey) and colorkey
(format=rgba,colorkey) with the key colour = median of the 4 corners of frame 1;
oracle grid over similarity x blend."""
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid

SIMS = [0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.6]
BLENDS = [0.0, 0.05, 0.1, 0.2, 0.3]


def setup(variant, indir):
    f0 = L.load_frames(indir, 1)[0]
    return {"color": L.corner_median(f0)}


def build_chroma(variant, indir, ctx, p):
    c = L.hexc(ctx["color"])
    filt = "format=yuva444p,chromakey=color=%s:similarity=%s:blend=%s,alphaextract,format=gray" % (c, L.fnum(p["sim"]), L.fnum(p["blend"]))
    return {"args_in": L.input_args(indir), "filt": filt,
            "chain": "format=yuva444p,chromakey=color=%s:similarity=S:blend=B[,despill=...],format=rgba" % c}


def build_color(variant, indir, ctx, p):
    c = L.hexc(ctx["color"])
    filt = "format=rgba,colorkey=color=%s:similarity=%s:blend=%s,alphaextract,format=gray" % (c, L.fnum(p["sim"]), L.fnum(p["blend"]))
    return {"args_in": L.input_args(indir), "filt": filt, "chain": "format=rgba,colorkey=color=%s:similarity=S:blend=B" % c}


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "both"
    vf = sys.argv[2].split(",") if len(sys.argv) > 2 else None
    smoke = os.environ.get("SMOKE") == "1"
    g = grid(sim=SIMS, blend=BLENDS) if not smoke else grid(sim=[0.1, 0.2], blend=[0.0, 0.05])
    if which in ("both", "chroma"):
        print("== chromakey (app chain), corner-median key colour")
        run_grid("chromakey", build_chroma, g, {"sim": 0.2, "blend": 0.05}, variant_filter=vf, per_variant_setup=setup, save=not smoke)
    if which in ("both", "color"):
        print("== colorkey, corner-median key colour")
        run_grid("colorkey", build_color, g, {"sim": 0.1, "blend": 0.0}, variant_filter=vf, per_variant_setup=setup, save=not smoke)
