"""M2: STACKED multi-colour keys. K key colours = k-means over the border ring
(top/left/right 4-px ring of frame 1 = the edges the subject never touches; a
user's eyedropper picks along the gradient would do the same). Each colour is a
separate colorkey / chromakey stage on the SAME opaque frame and the mattes are
intersected (multiply) exactly like the app's keyKeepingAlpha stack.
Oracle grid over K x similarity x blend (shared by every stage)."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid

KS = [2, 3, 4, 6]
SIMS = [0.03, 0.05, 0.08, 0.1, 0.15, 0.2, 0.3]
BLENDS = [0.0, 0.05, 0.1]
RING_EDGES = ("top", "left", "right")


def setup(variant, indir):
    f0 = L.load_frames(indir, 1)[0]
    ring = L.border_ring(f0, 4, RING_EDGES)
    return {"colors": {k: L.kmeans_colors(ring, k) for k in KS}}


def stack_filter(kind, colors, sim, blend):
    k = len(colors)
    fmt = "yuva444p" if kind == "chroma" else "rgba"
    parts = ["[0:v]format=%s,split=%d%s" % (fmt, k, "".join("[s%d]" % i for i in range(k)))]
    for i, c in enumerate(colors):
        if kind == "chroma":
            parts.append("[s%d]chromakey=color=%s:similarity=%s:blend=%s,alphaextract[m%d]" % (i, L.hexc(c), L.fnum(sim), L.fnum(blend), i))
        else:
            parts.append("[s%d]colorkey=color=%s:similarity=%s:blend=%s,alphaextract[m%d]" % (i, L.hexc(c), L.fnum(sim), L.fnum(blend), i))
    prev = "[m0]"
    for i in range(1, k):
        nxt = "[x%d]" % i if i < k - 1 else "[out]"
        parts.append("%s[m%d]blend=all_mode=multiply%s" % (prev, i, nxt))
        prev = nxt
    if k == 1:
        parts[-1] = parts[-1].replace("[m0]", "[out]")
    return ";".join(parts)


def make_build(kind):
    def build(variant, indir, ctx, p):
        cols = ctx["colors"][p["k"]]
        filt = stack_filter(kind, cols, p["sim"], p["blend"])
        return {"args_in": L.input_args(indir), "filt": filt, "filter_complex": True,
                "chain": "%d x %skey (%s), mattes multiplied" % (p["k"], kind, ",".join(L.hexc(c) for c in cols))}
    return build


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "both"
    vf = sys.argv[2].split(",") if len(sys.argv) > 2 else None
    smoke = os.environ.get("SMOKE") == "1"
    g = grid(k=KS, sim=SIMS, blend=BLENDS) if not smoke else grid(k=[2, 4], sim=[0.05, 0.1], blend=[0.0])
    if which in ("both", "color"):
        print("== stacked colorkey, ring k-means colours")
        run_grid("stack-colorkey", make_build("color"), g, {"k": 4, "sim": 0.08, "blend": 0.0}, variant_filter=vf, per_variant_setup=setup, save=not smoke)
    if which in ("both", "chroma"):
        print("== stacked chromakey, ring k-means colours")
        run_grid("stack-chromakey", make_build("chroma"), g, {"k": 4, "sim": 0.08, "blend": 0.0}, variant_filter=vf, per_variant_setup=setup, save=not smoke)
