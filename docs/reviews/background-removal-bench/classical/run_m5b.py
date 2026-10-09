"""M5b: the YUV-sum difference key (backgroundkey with a concat-prepended plate)
fed the SYNTHETIC axis1d / coons plate (frame-1 plate looped: fair for the
static gradients), to compare the |dY|+|dU|+|dV| distance with the RGB max-channel one."""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid
import run_m5

def plate_f1(variant, kind):
    return ["-loop", "1", "-framerate", str(L.FPS), "-i", os.path.join(run_m5.PLATES, variant, kind, "001.png")]

def build(variant, indir, ctx, p):
    filt = ("[1:v]format=rgb24,trim=end_frame=1,setpts=PTS-STARTPTS[p];[0:v]format=rgb24,setpts=PTS-STARTPTS[c];"
            "[p][c]concat=n=2:v=1:a=0,format=yuva444p,backgroundkey=threshold=1:similarity=%s:blend=%s,"
            "select=gte(n\\,1),alphaextract,format=gray[out]") % (L.fnum(p["sim"]), L.fnum(p["blend"]))
    return {"args_in": L.input_args(indir) + plate_f1(variant, p["kind"]), "filt": filt, "filter_complex": True,
            "chain": "plate=%s (frame 1) prepended by concat; backgroundkey=threshold=1:similarity=S:blend=B; select=gte(n,1)" % p["kind"]}

if __name__ == "__main__":
    vf = ["green", "white", "black", "vgrad", "multi", "skin", "vgrad-gifpng", "multi-gifpng", "skin-gifpng"]
    g = grid(kind=["axis1d", "coons"], sim=[0.005, 0.01, 0.02, 0.03, 0.05, 0.08, 0.12], blend=[0.0, 0.05])
    run_grid("platediff-yuvsum-synth", build, g, {"kind": "axis1d", "sim": 0.02, "blend": 0.0}, variant_filter=vf, save=True)
