#!/usr/bin/env python
"""Background-removal benchmark metrics (corpus/README.md definitions).

Importable (``from metrics import evaluate_dirs, evaluate_arrays``) and a CLI::

    python metrics.py --gt DIR --pred DIR [--json out.json] [--limit N]

Definitions (alpha is treated as a float in 0..1 = uint8/255):

* ``mae``           mean |pred - gt| over all pixels, averaged over frames.
* ``iou``           intersection-over-union of (pred8 >= 128) vs (gt8 >= 128),
                    i.e. both sides thresholded at 128 (same as "> 127").
                    A frame where both masks are empty scores 1.0.
* ``boundary_mae``  MAE restricted to the band
                    ``dilate(gt > 127, 5 px) XOR erode(gt > 127, 5 px)``;
                    the structuring element is a Euclidean disk of radius 5
                    (x^2 + y^2 <= 25, an 11x11 footprint). Frames with an empty
                    band are NaN and excluded from the mean.
* ``flicker``       mean over t >= 2 of MAE(pred_t, pred_{t-1}) minus the same
                    quantity computed on the ground truth (0 = as stable as GT).

Frames are ``001.png .. 045.png`` (any zero-padded ``NNN.png`` names found in the
GT dir are used; a prediction is matched by file name). GT is 8-bit gray.
Predictions may be L / LA / RGB / RGBA: the alpha channel is used when there is
one, otherwise the first channel. A prediction whose size differs from the GT
is resized (bilinear) to the GT size with a warning.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import warnings
from typing import Iterable, Sequence

import numpy as np
from PIL import Image
from scipy import ndimage

BAND_RADIUS = 5
THRESHOLD = 128  # uint8 threshold: >= 128  <=>  > 127
_FRAME_RE = re.compile(r"^(\d{3,})\.png$", re.IGNORECASE)


def _disk(radius: int) -> np.ndarray:
    r = int(radius)
    y, x = np.mgrid[-r : r + 1, -r : r + 1]
    return (x * x + y * y) <= r * r


_DISK = _disk(BAND_RADIUS)


# --------------------------------------------------------------------------- IO
def load_gray(path: str, size: tuple[int, int] | None = None, what: str = "pred") -> np.ndarray:
    """Load an alpha/matte image as uint8 HxW.

    RGBA/LA -> alpha channel; RGB/L/P/other -> first channel after conversion.
    ``size`` is (W, H) of the reference; a mismatch is resized bilinearly with a
    warning.
    """
    im = Image.open(path)
    im.load()
    if im.mode in ("RGBA", "LA"):
        g = im.getchannel("A")
    elif im.mode == "L":
        g = im
    elif im.mode in ("I;16", "I;16B", "I;16L", "I", "F"):
        arr = np.asarray(im, dtype=np.float64)
        mx = 65535.0 if im.mode.startswith("I;16") else max(arr.max(), 1.0)
        g = Image.fromarray(np.clip(arr / mx * 255.0 + 0.5, 0, 255).astype(np.uint8), "L")
    else:  # RGB, P, CMYK, ...
        if im.mode == "P" and "transparency" in im.info:
            g = im.convert("RGBA").getchannel("A")
        else:
            g = im.convert("RGB").getchannel("R")
    if size is not None and g.size != size:
        warnings.warn(f"{what} {os.path.basename(path)}: size {g.size} != GT {size}; resizing")
        g = g.resize(size, Image.BILINEAR)
    return np.asarray(g, dtype=np.uint8)


def list_frames(gt_dir: str) -> list[str]:
    names = [n for n in os.listdir(gt_dir) if _FRAME_RE.match(n)]
    names.sort(key=lambda n: int(_FRAME_RE.match(n).group(1)))
    return names


# ---------------------------------------------------------------------- metrics
def _as_uint8(a: np.ndarray) -> np.ndarray:
    if a.dtype == np.uint8:
        return a
    if np.issubdtype(a.dtype, np.floating):
        return np.clip(np.rint(a * 255.0), 0, 255).astype(np.uint8)
    return np.clip(a, 0, 255).astype(np.uint8)


def band_mask(gt8: np.ndarray) -> np.ndarray:
    fg = gt8 >= THRESHOLD
    return ndimage.binary_dilation(fg, structure=_DISK) ^ ndimage.binary_erosion(fg, structure=_DISK)


def frame_metrics(pred8: np.ndarray, gt8: np.ndarray) -> dict:
    """Per-frame MAE, IoU@0.5 and boundary MAE for one uint8 pair."""
    pred8 = _as_uint8(pred8)
    gt8 = _as_uint8(gt8)
    if pred8.shape != gt8.shape:
        raise ValueError(f"shape mismatch pred {pred8.shape} vs gt {gt8.shape}")
    p = pred8.astype(np.float32) / 255.0
    g = gt8.astype(np.float32) / 255.0
    err = np.abs(p - g)
    mae = float(err.mean())
    pb = pred8 >= THRESHOLD
    gb = gt8 >= THRESHOLD
    union = int(np.count_nonzero(pb | gb))
    inter = int(np.count_nonzero(pb & gb))
    iou = 1.0 if union == 0 else inter / union
    band = band_mask(gt8)
    nb = int(np.count_nonzero(band))
    bmae = float(err[band].mean()) if nb else float("nan")
    return {"mae": mae, "iou": float(iou), "boundary_mae": bmae, "band_pixels": nb}


def temporal_mae(seq: Sequence[np.ndarray]) -> list[float]:
    """MAE(a_t, a_{t-1}) for t >= 2 (list of len(seq)-1 values, alpha in 0..1)."""
    out = []
    for prev, cur in zip(seq[:-1], seq[1:]):
        a = _as_uint8(prev).astype(np.float32)
        b = _as_uint8(cur).astype(np.float32)
        out.append(float(np.abs(a - b).mean() / 255.0))
    return out


def evaluate_arrays(gt: Sequence[np.ndarray], pred: Sequence[np.ndarray], names: Iterable[str] | None = None) -> dict:
    """Evaluate aligned sequences of uint8 (or float 0..1) HxW arrays.

    Returns ``{"mae", "iou", "boundary_mae", "flicker", "flicker_pred",
    "flicker_gt", "frames", "per_frame": [...]}``; means are over frames
    (boundary_mae ignores NaN frames).
    """
    if len(gt) != len(pred):
        raise ValueError(f"{len(gt)} GT frames vs {len(pred)} predictions")
    names = list(names) if names is not None else [f"{i + 1:03d}" for i in range(len(gt))]
    per = []
    for n, g, p in zip(names, gt, pred):
        m = frame_metrics(p, g)
        m["frame"] = n
        per.append(m)
    tp = temporal_mae(pred)
    tg = temporal_mae(gt)
    for i, (a, b) in enumerate(zip(tp, tg)):
        per[i + 1]["flicker_pred"] = a
        per[i + 1]["flicker_gt"] = b
    fp = float(np.mean(tp)) if tp else float("nan")
    fg = float(np.mean(tg)) if tg else float("nan")
    res = {
        "mae": float(np.mean([m["mae"] for m in per])),
        "iou": float(np.mean([m["iou"] for m in per])),
        "boundary_mae": float(np.nanmean([m["boundary_mae"] for m in per])) if per else float("nan"),
        "flicker": fp - fg,
        "flicker_pred": fp,
        "flicker_gt": fg,
        "frames": len(per),
        "per_frame": per,
    }
    return res


def evaluate_dirs(gt_dir: str, pred_dir: str, limit: int | None = None, strict: bool = False) -> dict:
    """Evaluate ``pred_dir/NNN.png`` against ``gt_dir/NNN.png``.

    Missing predictions are skipped with a warning (error if ``strict``). The
    result also lists ``missing`` frame names.
    """
    names = list_frames(gt_dir)
    if not names:
        raise FileNotFoundError(f"no NNN.png frames in {gt_dir}")
    if limit is not None:
        names = names[: int(limit)]
    gts, preds, used, missing = [], [], [], []
    for n in names:
        pp = os.path.join(pred_dir, n)
        if not os.path.exists(pp):
            missing.append(n)
            if strict:
                raise FileNotFoundError(pp)
            continue
        g = load_gray(os.path.join(gt_dir, n), what="gt")
        p = load_gray(pp, size=(g.shape[1], g.shape[0]), what="pred")
        gts.append(g)
        preds.append(p)
        used.append(n)
    if missing:
        warnings.warn(f"{len(missing)} prediction frame(s) missing in {pred_dir}: {missing[:5]}{'...' if len(missing) > 5 else ''}")
    if not used:
        raise FileNotFoundError(f"no matching prediction frames in {pred_dir}")
    res = evaluate_arrays(gts, preds, used)
    res["missing"] = missing
    res["gt_dir"] = os.path.abspath(gt_dir)
    res["pred_dir"] = os.path.abspath(pred_dir)
    return res


def summary(res: dict) -> dict:
    """The scalar part of an evaluate_* result (no per-frame list)."""
    return {k: res[k] for k in ("mae", "iou", "boundary_mae", "flicker", "flicker_pred", "flicker_gt", "frames") if k in res}


# -------------------------------------------------------------------------- CLI
def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--gt", required=True, help="directory of ground-truth NNN.png (8-bit gray)")
    ap.add_argument("--pred", required=True, help="directory of predicted mattes NNN.png")
    ap.add_argument("--json", help="write the full result (incl. per-frame values) to this file")
    ap.add_argument("--limit", type=int, help="only the first N frames")
    ap.add_argument("--strict", action="store_true", help="fail on a missing prediction frame")
    a = ap.parse_args(argv)
    with warnings.catch_warnings(record=True) as w:
        warnings.simplefilter("always")
        res = evaluate_dirs(a.gt, a.pred, limit=a.limit, strict=a.strict)
        for ww in w:
            print(f"warning: {ww.message}", file=sys.stderr)
    s = summary(res)
    print(f"frames        {s['frames']}")
    print(f"MAE           {s['mae']:.5f}")
    print(f"IoU@0.5       {s['iou']:.5f}")
    print(f"boundary MAE  {s['boundary_mae']:.5f}")
    print(f"flicker       {s['flicker']:+.5f}  (pred {s['flicker_pred']:.5f} - gt {s['flicker_gt']:.5f})")
    if a.json:
        os.makedirs(os.path.dirname(os.path.abspath(a.json)), exist_ok=True)
        with open(a.json, "w", encoding="utf-8") as f:
            json.dump(res, f, indent=1)
        print(f"wrote {a.json}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
