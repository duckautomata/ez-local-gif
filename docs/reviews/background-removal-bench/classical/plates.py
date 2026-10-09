"""Background plates for the difference key (M5), written as PNGs under
<results>/classical/_plates/<variant>/<kind>/ (single plate.png, or 001..045.png per frame).

kinds:
  median45  per-pixel temporal median over all 45 frames (= what tmedian radius=22
            would produce for the centre frame; numpy because tmedian is a sliding
            window with start-up delay)
  first     frame 1 as the plate
  coons     per-frame Coons patch (transfinite interpolation) of the 4 border
            edges: P = (1-u)L(y)+uR(y)+(1-v)T(x)+vB(x) - bilinear(corners).
            Edges are the mean of a 3-px strip 1 px in. The BOTTOM edge is
            replaced by the straight line between the bottom corners because
            the subject stands on it (a tool would detect the contaminated edge
            as the one that disagrees most with the other three; here we state
            it: the user's characters stand on the bottom edge).
  laplace   per-frame harmonic (Laplace) fill from the same 4 edges, solved at
            1/4 resolution with a sparse direct solver and bilinearly upsampled.
"""
import os
import sys

import numpy as np
from PIL import Image
from scipy import sparse
from scipy.sparse.linalg import spsolve

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L

PLATES = os.path.join(L.RESULTS, "_plates")


def edges_of(frame, strip=3, inset=1):
    f = frame.astype(np.float32)
    top = f[inset:inset + strip, :].mean(0)          # (W,3)
    bot = f[-inset - strip:-inset, :].mean(0)
    left = f[:, inset:inset + strip].mean(1)         # (H,3)
    right = f[:, -inset - strip:-inset].mean(1)
    return top, bot, left, right


def synth_bottom(top, bot, left, right):
    # straight line between the bottom corners (taken from the clean left/right edges' last rows)
    W = top.shape[0]
    bl = left[-1]
    br = right[-1]
    u = np.linspace(0, 1, W, dtype=np.float32)[:, None]
    return bl[None, :] * (1 - u) + br[None, :] * u


def coons(frame, fix_bottom=True):
    top, bot, left, right = edges_of(frame)
    if fix_bottom:
        bot = synth_bottom(top, bot, left, right)
    Hh, Ww = frame.shape[:2]
    u = np.linspace(0, 1, Ww, dtype=np.float32)[None, :, None]
    v = np.linspace(0, 1, Hh, dtype=np.float32)[:, None, None]
    L_ = left[:, None, :]
    R_ = right[:, None, :]
    T_ = top[None, :, :]
    B_ = bot[None, :, :]
    tl, tr, bl, br = top[0], top[-1], bot[0], bot[-1]
    P = (1 - u) * L_ + u * R_ + (1 - v) * T_ + v * B_ \
        - ((1 - u) * (1 - v) * tl + u * (1 - v) * tr + (1 - u) * v * bl + u * v * br)
    return np.clip(P, 0, 255).astype(np.uint8)


_lap_cache = {}


def laplace(frame, fix_bottom=True, ds=4):
    top, bot, left, right = edges_of(frame)
    if fix_bottom:
        bot = synth_bottom(top, bot, left, right)
    Hh, Ww = frame.shape[:2]
    h, w = Hh // ds, Ww // ds
    # boundary values at low res
    def rs(e, n):
        idx = (np.arange(n) * (e.shape[0] / n)).astype(int)
        return e[idx]
    t, b, l, r = rs(top, w), rs(bot, w), rs(left, h), rs(right, h)
    key = (h, w)
    if key not in _lap_cache:
        n = h * w
        idx = np.arange(n).reshape(h, w)
        interior = np.ones((h, w), bool)
        interior[0, :] = interior[-1, :] = interior[:, 0] = interior[:, -1] = False
        rows, cols, vals = [], [], []
        ii = idx[interior]
        rows += [ii] * 5
        cols += [ii, ii - 1, ii + 1, ii - w, ii + w]
        vals += [np.full(ii.size, 4.0), np.full(ii.size, -1.0), np.full(ii.size, -1.0), np.full(ii.size, -1.0), np.full(ii.size, -1.0)]
        bi = idx[~interior]
        rows.append(bi)
        cols.append(bi)
        vals.append(np.ones(bi.size))
        A = sparse.csc_matrix((np.concatenate(vals), (np.concatenate(rows), np.concatenate(cols))), shape=(n, n))
        _lap_cache[key] = (A, interior)
    A, interior = _lap_cache[key]
    out = np.zeros((h, w, 3), np.float32)
    for c in range(3):
        rhs = np.zeros((h, w), np.float64)
        rhs[0, :] = t[:, c]
        rhs[-1, :] = b[:, c]
        rhs[:, 0] = l[:, c]
        rhs[:, -1] = r[:, c]
        x = spsolve(A, rhs.reshape(-1))
        out[:, :, c] = x.reshape(h, w)
    up = np.asarray(Image.fromarray(np.clip(out, 0, 255).astype(np.uint8)).resize((Ww, Hh), Image.BILINEAR))
    return up


def make_all(variant_filter=None):
    for variant, indir in L.variants():
        if variant_filter and variant not in variant_filter:
            continue
        frames = L.load_frames(indir)
        d = os.path.join(PLATES, variant)
        os.makedirs(os.path.join(d, "median45"), exist_ok=True)
        os.makedirs(os.path.join(d, "first"), exist_ok=True)
        Image.fromarray(np.median(frames, axis=0).astype(np.uint8)).save(os.path.join(d, "median45", "plate.png"))
        Image.fromarray(frames[0]).save(os.path.join(d, "first", "plate.png"))
        for kind, fn in (("coons", coons), ("laplace", laplace)):
            od = os.path.join(d, kind)
            os.makedirs(od, exist_ok=True)
            import time
            t0 = time.perf_counter()
            for i, f in enumerate(frames):
                Image.fromarray(fn(f)).save(os.path.join(od, "%03d.png" % (i + 1)), compress_level=1)
            print("  %s %s plates: %.2fs for 45 frames" % (variant, kind, time.perf_counter() - t0), flush=True)
        # plate error vs the true background where GT says background
        gts, _ = L.load_gt()
        bg = np.stack(gts) <= 127
        for kind in ("coons", "laplace"):
            errs = []
            for i in range(L.N):
                p = np.asarray(Image.open(os.path.join(d, kind, "%03d.png" % (i + 1))), dtype=np.float32)
                errs.append(np.abs(p - frames[i].astype(np.float32)).max(2)[bg[i]].mean())
            print("  %s %s plate max-channel error on true bg: %.2f levels" % (variant, kind, np.mean(errs)), flush=True)


if __name__ == "__main__":
    vf = sys.argv[1].split(",") if len(sys.argv) > 1 else None
    make_all(vf)


# ---------------------------------------------------------------------------
# axis1d: background = f(projection of (x,y) on an axis). The axis angle is
# searched (0..175 deg, 5 deg steps): ring pixels (top/left/right, 4 px) are
# binned by projection (64 bins), the per-bin median colour is the model and the
# angle with the smallest within-bin residual wins. The plate is the model
# linearly interpolated over the frame. Exact for any 1-D gradient (linear,
# piecewise-linear, rotating per frame); a 2-D background degrades to the Coons
# plate's quality or worse.
def axis1d(frame, nbins=64, angles=None, ring=4, return_info=False):
    Hh, Ww = frame.shape[:2]
    ys, xs = np.mgrid[0:Hh, 0:Ww]
    ringmask = np.zeros((Hh, Ww), bool)
    ringmask[:ring, :] = True
    ringmask[:, :ring] = True
    ringmask[:, -ring:] = True
    rx = xs[ringmask].astype(np.float32)
    ry = ys[ringmask].astype(np.float32)
    rc = frame[ringmask].astype(np.float32)
    if angles is None:
        angles = np.arange(0, 180, 5)
    best = None
    for ang in angles:
        th = np.deg2rad(ang)
        t = rx * np.cos(th) + ry * np.sin(th)
        tmin, tmax = t.min(), t.max()
        if tmax - tmin < 1e-3:
            continue
        b = np.clip(((t - tmin) / (tmax - tmin) * nbins).astype(int), 0, nbins - 1)
        model = np.full((nbins, 3), np.nan, np.float32)
        resid = 0.0
        cnt = 0
        for i in range(nbins):
            sel = b == i
            if sel.any():
                m = np.median(rc[sel], 0)
                model[i] = m
                resid += np.abs(rc[sel] - m).max(1).sum()
                cnt += sel.sum()
        resid /= max(cnt, 1)
        if best is None or resid < best[0]:
            best = (resid, ang, model, tmin, tmax)
    resid, ang, model, tmin, tmax = best
    # fill empty bins by interpolation
    idx = np.arange(nbins)
    for c in range(3):
        good = ~np.isnan(model[:, c])
        model[:, c] = np.interp(idx, idx[good], model[good, c])
    th = np.deg2rad(ang)
    t = xs * np.cos(th) + ys * np.sin(th)
    tn = np.clip((t - tmin) / (tmax - tmin) * nbins - 0.5, 0, nbins - 1)
    centres = idx.astype(np.float32)
    plate = np.stack([np.interp(tn.ravel(), centres, model[:, c]).reshape(Hh, Ww) for c in range(3)], 2)
    plate = np.clip(plate, 0, 255).astype(np.uint8)
    if return_info:
        return plate, ang, resid
    return plate


def make_axis1d(variant_filter=None):
    import time
    gts, _ = L.load_gt()
    bg = np.stack(gts) <= 127
    for variant, indir in L.variants():
        if variant_filter and variant not in variant_filter:
            continue
        frames = L.load_frames(indir)
        od = os.path.join(PLATES, variant, "axis1d")
        os.makedirs(od, exist_ok=True)
        t0 = time.perf_counter()
        errs, angs = [], []
        for i, f in enumerate(frames):
            p, ang, resid = axis1d(f, return_info=True)
            Image.fromarray(p).save(os.path.join(od, "%03d.png" % (i + 1)), compress_level=1)
            errs.append(np.abs(p.astype(np.float32) - f.astype(np.float32)).max(2)[bg[i]].mean())
            angs.append(ang)
        print("  %s axis1d plates: %.2fs/45; angle f1=%d (ring resid %.2f); max-channel error on true bg: %.2f levels" % (
            variant, time.perf_counter() - t0, angs[0], resid, np.mean(errs)), flush=True)
