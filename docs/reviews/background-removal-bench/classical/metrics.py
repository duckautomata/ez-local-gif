#!/usr/bin/env python
"""Corpus metrics for a matte sequence - the CLASSICAL benchmark's scorer.

Kept verbatim from the research run so classical/*.py reproduce their numbers:
its boundary band uses a SQUARE 11x11 structuring element and it adds the
gif_* metrics; the AI harness's ../metrics.py uses a Euclidean disk of radius
5 for the band. MAE, IoU and flicker are the same definition in both.

Usage: metrics.py --gt DIR --pred DIR --json OUT [--quiet]

Both DIRs hold 001.png .. NNN.png 8-bit gray mattes (255 = subject). Alpha is
scaled to 0..1 for MAE; IoU and the boundary band threshold both sides at >127
(i.e. >=128). Metrics (averaged over frames, alpha in 0..1):
  mae          mean |a - gt|
  iou          IoU of (a > 127) vs (gt > 127)
  boundary_mae MAE restricted to the band dilate(gt>127, 5px) XOR erode(gt>127, 5px)
               (square 11x11 structuring element; outside the frame counts as
               background for the dilation and as foreground for the erosion)
  flicker      mean over t>=2 of MAE(pred_t, pred_{t-1}) minus the same on GT
Extras: gif_mae / gif_boundary_mae / gif_flicker = the same after binarising the
prediction at >=128 (what a GIF output shows).
"""
import argparse
import json
import os
import sys

import numpy as np
from PIL import Image
from scipy import ndimage

BAND_PX = 5


def load_gray(path):
    im = Image.open(path)
    if im.mode != "L":
        im = im.convert("L")
    return np.asarray(im, dtype=np.uint8)


def band_mask(gt_bin, px=BAND_PX):
    # dilate(gt,5px) XOR erode(gt,5px) with a (2px+1)^2 square SE. maximum_filter /
    # minimum_filter with mode='nearest' are exactly binary dilation with
    # border_value=0 / erosion with border_value=1 for a square footprint.
    size = 2 * px + 1
    d = ndimage.maximum_filter(gt_bin, size=size, mode="nearest")
    e = ndimage.minimum_filter(gt_bin, size=size, mode="nearest")
    return d ^ e


def frame_metrics(pred, gt, band=None):
    a = pred.astype(np.float32) / 255.0
    g = gt.astype(np.float32) / 255.0
    absd = np.abs(a - g)
    mae = float(absd.mean())
    pb = pred > 127
    gb = gt > 127
    inter = np.logical_and(pb, gb).sum()
    union = np.logical_or(pb, gb).sum()
    iou = float(inter / union) if union else 1.0
    if band is None:
        band = band_mask(gb)
    bmae = float(absd[band].mean()) if band.any() else 0.0
    ab = pb.astype(np.float32)
    absb = np.abs(ab - g)
    gif_mae = float(absb.mean())
    gif_bmae = float(absb[band].mean()) if band.any() else 0.0
    return dict(mae=mae, iou=iou, boundary_mae=bmae, gif_mae=gif_mae, gif_boundary_mae=gif_bmae,
                fg_px=int(gb.sum()), pred_px=int(pb.sum()))


def score_arrays(preds, gts, bands=None, per_frame=False):
    """preds, gts: sequences of uint8 HxW arrays (same length). Returns the metrics dict."""
    rows = []
    flick_p, flick_g, gif_flick_p = [], [], []
    prev_p = prev_g = None
    for i, (pred, gt) in enumerate(zip(preds, gts)):
        if pred.shape != gt.shape:
            pred = np.asarray(Image.fromarray(pred).resize((gt.shape[1], gt.shape[0]), Image.BILINEAR), dtype=np.uint8)
        m = frame_metrics(pred, gt, None if bands is None else bands[i])
        m["frame"] = i + 1
        rows.append(m)
        if prev_p is not None:
            flick_p.append(float(np.mean(np.abs(pred.astype(np.float32) - prev_p.astype(np.float32)) / 255.0)))
            flick_g.append(float(np.mean(np.abs(gt.astype(np.float32) - prev_g.astype(np.float32)) / 255.0)))
            gif_flick_p.append(float(np.mean(np.abs((pred > 127).astype(np.float32) - (prev_p > 127).astype(np.float32)))))
        prev_p, prev_g = pred, gt

    def avg(k):
        return float(np.mean([r[k] for r in rows]))

    out = {
        "frames": len(rows),
        "mae": avg("mae"),
        "iou": avg("iou"),
        "boundary_mae": avg("boundary_mae"),
        "flicker": (float(np.mean(flick_p)) - float(np.mean(flick_g))) if flick_p else 0.0,
        "flicker_pred_raw": float(np.mean(flick_p)) if flick_p else 0.0,
        "flicker_gt_raw": float(np.mean(flick_g)) if flick_g else 0.0,
        "gif_mae": avg("gif_mae"),
        "gif_boundary_mae": avg("gif_boundary_mae"),
        "gif_flicker": (float(np.mean(gif_flick_p)) - float(np.mean(flick_g))) if gif_flick_p else 0.0,
    }
    if per_frame:
        out["per_frame"] = rows
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--gt", required=True)
    ap.add_argument("--pred", required=True)
    ap.add_argument("--json", required=True)
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args()

    names = sorted(f for f in os.listdir(args.gt) if f.lower().endswith(".png"))
    preds, gts, missing = [], [], []
    for n in names:
        pp = os.path.join(args.pred, n)
        if not os.path.exists(pp):
            missing.append(n)
            continue
        gts.append(load_gray(os.path.join(args.gt, n)))
        preds.append(load_gray(pp))
    if not preds:
        print("no frames scored", file=sys.stderr)
        sys.exit(2)
    out = score_arrays(preds, gts, per_frame=True)
    out.update(gt=args.gt, pred=args.pred, missing=missing)
    os.makedirs(os.path.dirname(os.path.abspath(args.json)) or ".", exist_ok=True)
    with open(args.json, "w") as f:
        json.dump(out, f, indent=1)
    if not args.quiet:
        print("mae=%.4f iou=%.4f bmae=%.4f flicker=%+.4f gif_mae=%.4f frames=%d missing=%d" % (
            out["mae"], out["iou"], out["boundary_mae"], out["flicker"], out["gif_mae"], len(preds), len(missing)))


if __name__ == "__main__":
    main()
