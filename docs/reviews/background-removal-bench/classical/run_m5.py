"""M5: background-plate difference key. Plate kinds from plates.py. Two
ffmpeg implementations of the per-pixel difference -> alpha:

  rgbmax  [clip]format=gbrp[c];[plate]format=gbrp[p];[c][p]blend=all_mode=difference,
          extractplanes=g+b+r[g][b][r];[g][b]blend=all_mode=lighten[gb];
          [gb][r]blend=all_mode=lighten,lut=y='clip((val-T)*255/W,0,255)'  -> alpha
          (max over channels of |clip-plate|, hard step at T levels, soft ramp of W
          levels). In the app: ...[alpha]; [clip][alpha]alphamerge.
  yuvsum  the plate is prepended as frame 0 (concat) and backgroundkey with
          threshold=1 (reference never replaced) keys on |dY|+|dU|+|dV| <= 765*S;
          select=gte(n,1) drops the plate frame. Only valid for a SINGLE plate.

Oracle grid over plate kind x T x W (rgbmax) and plate kind x S x B (yuvsum)."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid

PLATES = os.path.join(L.RESULTS, "_plates")
KINDS = ["median45", "first", "coons", "laplace", "axis1d"]
TS = [4, 8, 16, 24, 32, 48, 64]
WS = [1, 16]
SIMS = [0.01, 0.02, 0.03, 0.05, 0.08, 0.12, 0.2]
BLENDS = [0.0, 0.05]


def plate_input(variant, kind):
    d = os.path.join(PLATES, variant, kind)
    if os.path.exists(os.path.join(d, "plate.png")):
        return ["-loop", "1", "-framerate", str(L.FPS), "-i", os.path.join(d, "plate.png")]
    return ["-framerate", str(L.FPS), "-i", os.path.join(d, "%03d.png")]


def build_rgbmax(variant, indir, ctx, p):
    filt = ("[0:v]format=gbrp[c];[1:v]format=gbrp[p];[c][p]blend=all_mode=difference:shortest=1,extractplanes=g+b+r[g][b][r];"
            "[g][b]blend=all_mode=lighten[gb];[gb][r]blend=all_mode=lighten,lut=y='clip((val-%d)*255/%d\\,0\\,255)'[out]") % (p["T"], p["W"])
    return {"args_in": L.input_args(indir) + plate_input(variant, p["kind"]), "filt": filt, "filter_complex": True,
            "chain": "plate=%s; blend=difference -> max(G,B,R) -> lut clip((d-T)*255/W)" % p["kind"]}


def build_yuvsum(variant, indir, ctx, p):
    filt = ("[1:v]format=rgb24,trim=end_frame=1,setpts=PTS-STARTPTS[p];[0:v]format=rgb24,setpts=PTS-STARTPTS[c];"
            "[p][c]concat=n=2:v=1:a=0,format=yuva444p,backgroundkey=threshold=1:similarity=%s:blend=%s,"
            "select=gte(n\\,1),alphaextract,format=gray[out]") % (L.fnum(p["sim"]), L.fnum(p["blend"]))
    return {"args_in": L.input_args(indir) + plate_input(variant, p["kind"]), "filt": filt, "filter_complex": True,
            "chain": "plate=%s prepended by concat; backgroundkey=threshold=1:similarity=S:blend=B; select=gte(n,1)" % p["kind"]}


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "both"
    vf = sys.argv[2].split(",") if len(sys.argv) > 2 else None
    smoke = os.environ.get("SMOKE") == "1"
    if which in ("both", "rgbmax"):
        g = grid(kind=KINDS, T=TS, W=WS) if not smoke else grid(kind=KINDS, T=[16], W=[1])
        print("== plate difference key, rgb max-channel (blend=difference + lut)")
        run_grid("platediff-rgbmax", build_rgbmax, g, {"kind": "coons", "T": 24, "W": 16}, variant_filter=vf, save=not smoke)
    if which in ("both", "yuvsum"):
        g = grid(kind=["median45", "first"], sim=SIMS, blend=BLENDS) if not smoke else grid(kind=["first"], sim=[0.05], blend=[0.0])
        print("== plate difference key via backgroundkey (concat plate as frame 0)")
        run_grid("platediff-yuvsum", build_yuvsum, g, {"kind": "median45", "sim": 0.05, "blend": 0.0}, variant_filter=vf, save=not smoke)
