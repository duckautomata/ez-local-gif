"""M7: matte post-processing, applied with ffmpeg to the saved BEST mattes of a
method (<results>/classical/<method>/<variant>/%03d.png, gray) and scored again:

  erode1/2   erosion=coordinates=255 (3x3, x1 / x2)  = choke: shrinks the subject
  dilate1/2  dilation=coordinates=255                 = grows the subject (fills holes/fringe)
  open       morpho=open  3x3 (erode then dilate: drops speckles)
  close      morpho=close 3x3 (dilate then erode: fills pinholes)
  median3/5  median=radius=1 / 2 (despeckle, keeps edges)
  feather1/2 gblur=sigma=1 / 2 (the app's feather op on alpha)
  tmed3      tmedian=radius=1 (temporal median of the matte: anti-flicker)
The GIF 128 threshold effect is the gif_* metrics (pred binarised at >=128)."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid

POST = {
    "none": "null",
    "erode1": "erosion=coordinates=255",
    "erode2": "erosion=coordinates=255,erosion=coordinates=255",
    "dilate1": "dilation=coordinates=255",
    "dilate2": "dilation=coordinates=255,dilation=coordinates=255",
    "open": "erosion=coordinates=255,dilation=coordinates=255",
    "close": "dilation=coordinates=255,erosion=coordinates=255",
    "median3": "median=radius=1",
    "median5": "median=radius=2",
    "feather1": "gblur=sigma=1",
    "feather2": "gblur=sigma=2",
    "median3+feather1": "median=radius=1,gblur=sigma=1",
    "tmed3": "tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1",
    "open+tmed3": "erosion=coordinates=255,dilation=coordinates=255,tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1",
}


def make_build(method):
    def build(variant, indir, ctx, p):
        src = os.path.join(L.RESULTS, method, variant)
        filt = "format=gray,%s,format=gray" % POST[p["post"]]
        return {"args_in": L.input_args(src), "filt": filt, "chain": "matte(%s) -> %s" % (method, POST[p["post"]])}
    return build


if __name__ == "__main__":
    method = sys.argv[1]
    vf = sys.argv[2].split(",") if len(sys.argv) > 2 else None
    g = grid(post=list(POST))
    print("== post-processing of %s best mattes" % method)
    run_grid("post-" + method, make_build(method), g, {"post": "none"}, variant_filter=vf, save=False)
