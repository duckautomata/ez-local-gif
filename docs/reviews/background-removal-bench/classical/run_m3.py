"""M3: hsvkey. The key colour (corner median) is converted to ffmpeg's own
HSV convention (hue = atan2(U-128, V-128)+pi in the filter; the `hue` option
goes through hue_rad = sign(opt)*pi*fmod(526-|opt|,360)/180, so opt =
(526 - deg) mod 360). Modes: hsv (all three), hs (val ignored: val option
negative => the pixel's val is replaced by |val| so it no longer contributes),
h (sat and val ignored). Oracle grid over mode x similarity x blend."""
import math
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid

SIMS = [0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.6]
BLENDS = [0.0, 0.05, 0.1, 0.2]
MODES = ["hsv", "hs", "h"]


def ffmpeg_hsv(rgb):
    y, u, v = L.rgb_to_yuv_limited(rgb)
    uf, vf = u - 127.5, v - 127.5
    hue_rad = math.atan2(uf, vf) + math.pi
    deg = hue_rad * 180.0 / math.pi
    opt = (526.0 - deg) % 360.0
    if opt == 0:
        opt = 360.0
    sat = math.sqrt((uf * uf + vf * vf) / (127.5 * 127.5 * 2.0))
    val = y / 255.0
    return opt, sat, val


def setup(variant, indir):
    f0 = L.load_frames(indir, 1)[0]
    c = L.corner_median(f0)
    h, s, v = ffmpeg_hsv(c)
    return {"color": c, "hsv": (h, s, v)}


def build(variant, indir, ctx, p):
    h, s, v = ctx["hsv"]
    s = max(s, 0.001)
    v = max(v, 0.001)
    if p["mode"] == "hsv":
        opts = "hue=%s:sat=%s:val=%s" % (L.fnum(h), L.fnum(s), L.fnum(v))
    elif p["mode"] == "hs":
        opts = "hue=%s:sat=%s:val=%s" % (L.fnum(h), L.fnum(s), L.fnum(-v))
    else:
        opts = "hue=%s:sat=%s:val=%s" % (L.fnum(h), L.fnum(-s), L.fnum(-v))
    filt = "format=yuva444p,hsvkey=%s:similarity=%s:blend=%s,alphaextract,format=gray" % (opts, L.fnum(p["sim"]), L.fnum(p["blend"]))
    return {"args_in": L.input_args(indir), "filt": filt, "chain": "format=yuva444p,hsvkey=%s:similarity=S:blend=B" % opts}


if __name__ == "__main__":
    vf = sys.argv[1].split(",") if len(sys.argv) > 1 else None
    smoke = os.environ.get("SMOKE") == "1"
    g = grid(mode=MODES, sim=SIMS, blend=BLENDS) if not smoke else grid(mode=MODES, sim=[0.1, 0.2], blend=[0.0])
    print("== hsvkey, corner-median key colour")
    run_grid("hsvkey", build, g, {"mode": "hsv", "sim": 0.2, "blend": 0.05}, variant_filter=vf, per_variant_setup=setup, save=not smoke)
