"""M6: border-connected 'magic wand' / region grow, in numpy (ffmpeg cannot
express it: floodfill has NO tolerance - is_same* compare every component for
exact equality to the seed's value picked at (x,y), one seed, 4-connected).

Three variants, every one seeded from the frame border and 4-connected:
  seed    tolerance to the nearest of K border k-means colours (max |dRGB|
          channel distance <= tol), connected components (scipy.ndimage.label,
          4-conn) that touch the top/left/right border = background. This is the
          stacked colorkey PLUS connectivity.
  grow    neighbour tolerance: a pixel joins the background if it is 4-adjacent to
          a background pixel and differs from THAT neighbour by <= tol (max
          channel). Implemented as: 'smooth' pixels (max 4-neighbour diff <= tol)
          labelled, components touching the border kept, then `steps` rounds of
          conditional 1-px dilation (adds pixels within tol of an adjacent
          background pixel) to reach the last background pixel before an edge.
  hybrid  grow AND a loose global gate: the pixel must be within gtol (max
          channel) of the nearest border k-means colour (K=6).
Pre-filter `med` (0 / 3): 3x3 median on the RGB before the wand (dither).
Alpha is hard 0/255."""
import os
import sys
import time

import numpy as np
from scipy import ndimage

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
from grid import run_grid, grid

RING_EDGES = ("top", "left", "right")
STRUCT4 = np.array([[0, 1, 0], [1, 1, 1], [0, 1, 0]], bool)


def border_touch_labels(lab, nlab):
    """labels present on the top/left/right border rows/cols (the clean edges)."""
    b = np.concatenate([lab[0, :], lab[:, 0], lab[:, -1]])
    keep = np.zeros(nlab + 1, bool)
    keep[np.unique(b)] = True
    keep[0] = False
    return keep


def neighbour_maxdiff(f):
    """max over the 4 neighbours and channels of |f - neighbour| (uint8 HxWx3 -> HxW)."""
    fi = f.astype(np.int16)
    d = np.zeros(f.shape[:2], np.int16)
    d[:, 1:] = np.maximum(d[:, 1:], np.abs(fi[:, 1:] - fi[:, :-1]).max(2))
    d[:, :-1] = np.maximum(d[:, :-1], np.abs(fi[:, :-1] - fi[:, 1:]).max(2))
    d[1:, :] = np.maximum(d[1:, :], np.abs(fi[1:] - fi[:-1]).max(2))
    d[:-1, :] = np.maximum(d[:-1, :], np.abs(fi[:-1] - fi[1:]).max(2))
    return d


def conditional_dilate(bg, f, tol, steps):
    fi = f.astype(np.int16)
    for _ in range(steps):
        new = bg.copy()
        # right neighbour is bg and |f - f_right| <= tol
        c = bg[:, 1:] & (np.abs(fi[:, :-1] - fi[:, 1:]).max(2) <= tol)
        new[:, :-1] |= c
        c = bg[:, :-1] & (np.abs(fi[:, 1:] - fi[:, :-1]).max(2) <= tol)
        new[:, 1:] |= c
        c = bg[1:, :] & (np.abs(fi[:-1] - fi[1:]).max(2) <= tol)
        new[:-1, :] |= c
        c = bg[:-1, :] & (np.abs(fi[1:] - fi[:-1]).max(2) <= tol)
        new[1:, :] |= c
        if (new == bg).all():
            break
        bg = new
    return bg


def wand_seed(f, colors, tol):
    fi = f.astype(np.int16)
    near = np.zeros(f.shape[:2], bool)
    for c in colors:
        near |= np.abs(fi - np.array(c, np.int16)).max(2) <= tol
    lab, n = ndimage.label(near, STRUCT4)
    keep = border_touch_labels(lab, n)
    return keep[lab]


def wand_grow(f, tol, steps=3):
    smooth = neighbour_maxdiff(f) <= tol
    lab, n = ndimage.label(smooth, STRUCT4)
    keep = border_touch_labels(lab, n)
    bg = keep[lab]
    return conditional_dilate(bg, f, tol, steps)


def wand_hybrid(f, tol, colors, gtol, steps=3):
    fi = f.astype(np.int16)
    gate = np.zeros(f.shape[:2], bool)
    for c in colors:
        gate |= np.abs(fi - np.array(c, np.int16)).max(2) <= gtol
    smooth = (neighbour_maxdiff(f) <= tol) & gate
    lab, n = ndimage.label(smooth, STRUCT4)
    keep = border_touch_labels(lab, n)
    bg = keep[lab]
    fi2 = f.copy()
    bg = conditional_dilate(bg, fi2, tol, steps) & gate
    return bg


def exact_bfs(f, tol):
    """Reference: true region grow (frontier expansion until no change)."""
    bg = np.zeros(f.shape[:2], bool)
    bg[0, :] = bg[:, 0] = bg[:, -1] = True
    return conditional_dilate(bg, f, tol, steps=5000)


def setup(variant, indir):
    frames = L.load_frames(indir)
    f0 = frames[0]
    ring = L.border_ring(f0, 4, RING_EDGES)
    return {"frames": frames, "colors": {k: L.kmeans_colors(ring, k) for k in (2, 4, 6)}}


def run_numpy(variant, indir, ctx, p):
    frames = ctx["frames"]
    t0 = time.perf_counter()
    out = np.empty((L.N, L.H, L.W), np.uint8)
    for i, f in enumerate(frames):
        if p.get("med", 0):
            f = ndimage.median_filter(f, size=(p["med"], p["med"], 1))
        if p["mode"] == "seed":
            bg = wand_seed(f, ctx["colors"][p["k"]], p["tol"])
        elif p["mode"] == "grow":
            bg = wand_grow(f, p["tol"], p.get("steps", 3))
        else:
            bg = wand_hybrid(f, p["tol"], ctx["colors"][6], p["gtol"], p.get("steps", 3))
        out[i] = np.where(bg, 0, 255).astype(np.uint8)
    return out, time.perf_counter() - t0


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "all"
    vf = sys.argv[2].split(",") if len(sys.argv) > 2 else None
    smoke = os.environ.get("SMOKE") == "1"
    if which in ("all", "seed"):
        g = grid(mode=["seed"], k=[4, 6], tol=[8, 16, 32, 48], med=[0, 3]) if not smoke else grid(mode=["seed"], k=[4], tol=[24], med=[0])
        print("== wand seed-tolerance (stacked colorkey + connectivity)")
        run_grid("wand-seed", None, g, {"mode": "seed", "k": 4, "tol": 24, "med": 0}, variant_filter=vf, per_variant_setup=setup, numpy_fn=run_numpy, workers=6, save=not smoke)
    if which in ("all", "grow"):
        g = grid(mode=["grow"], tol=[4, 8, 16, 32], med=[0, 3]) if not smoke else grid(mode=["grow"], tol=[8], med=[0])
        print("== wand neighbour region grow")
        run_grid("wand-grow", None, g, {"mode": "grow", "tol": 8, "med": 0}, variant_filter=vf, per_variant_setup=setup, numpy_fn=run_numpy, workers=6, save=not smoke)
    if which in ("all", "hybrid"):
        g = grid(mode=["hybrid"], tol=[8, 16], gtol=[48, 96], med=[0, 3]) if not smoke else grid(mode=["hybrid"], tol=[8], gtol=[64], med=[0])
        print("== wand hybrid (grow + global gate)")
        run_grid("wand-hybrid", None, g, {"mode": "hybrid", "tol": 8, "gtol": 64, "med": 0}, variant_filter=vf, per_variant_setup=setup, numpy_fn=run_numpy, workers=6, save=not smoke)
