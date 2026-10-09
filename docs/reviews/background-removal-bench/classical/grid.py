"""Generic oracle grid runner: for every corpus variant run every param combo
through ffmpeg (or a numpy fn), score, keep the best (by MAE) and the single
default, save both matte sets, write <method>.json."""
import os
import sys
import time
import traceback

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L


def run_grid(method, build, params_list, default_params, variant_filter=None, workers=6,
             save=True, numpy_fn=None, per_variant_setup=None, verbose=True):
    """build(variant, indir, ctx, params) -> dict(args_in=[...], filt=str, filter_complex=bool, chain=str)
       or, with numpy_fn, numpy_fn(variant, indir, ctx, params) -> (array, seconds).
       per_variant_setup(variant, indir) -> ctx (e.g. sampled key colours)."""
    rows = []
    best = {}
    default = {}
    t_all = time.perf_counter()
    for variant, indir in L.variants():
        if variant_filter and variant not in variant_filter:
            continue
        ctx = per_variant_setup(variant, indir) if per_variant_setup else {}
        tv = time.perf_counter()

        def one(params):
            try:
                if numpy_fn:
                    arr, dt = numpy_fn(variant, indir, ctx, params)
                    chain = ctx.get("chain", "numpy")
                else:
                    b = build(variant, indir, ctx, params)
                    arr, dt = L.run_ffmpeg_gray(b["args_in"], b["filt"], filter_complex=b.get("filter_complex", False))
                    chain = b.get("chain", b["filt"])
                m = L.score(arr)
                m.update(variant=variant, params=params_str(params), params_raw=params, wall_s=dt, chain=chain)
                return m, arr
            except Exception as e:  # noqa
                traceback.print_exc()
                return {"variant": variant, "params": params_str(params), "error": str(e), "mae": 9.0, "iou": 0.0,
                        "boundary_mae": 9.0, "flicker": 9.0, "gif_mae": 9.0, "gif_boundary_mae": 9.0, "gif_flicker": 9.0}, None

        res = L.pmap(one, params_list, workers=workers)
        ok = [(m, a) for m, a in res if a is not None]
        if not ok:
            print("  %s: every run failed" % variant)
            rows += [m for m, _ in res]
            continue
        bm, ba = min(ok, key=lambda t: t[0]["mae"])
        best[variant] = bm
        dm = None
        for m, a in ok:
            if m["params_raw"] == default_params:
                dm, da = m, a
        if dm is None:
            # run the default separately if it was not part of the grid
            dm, da = one(default_params)
        default[variant] = dm
        rows += [m for m, _ in res]
        if save:
            L.save_mattes(ba, os.path.join(L.RESULTS, method, variant))
            if da is not None:
                L.save_mattes(da, os.path.join(L.RESULTS, method + "-default", variant))
        if verbose:
            print("  %-13s best mae=%.4f iou=%.4f bmae=%.4f flk=%+.4f [%s] | default mae=%.4f iou=%.4f [%s]  (%.0fs, %d runs)" % (
                variant, bm["mae"], bm["iou"], bm["boundary_mae"], bm["flicker"], bm["params"],
                dm["mae"], dm["iou"], dm["params"], time.perf_counter() - tv, len(res)), flush=True)
    out = {"method": method, "default_params": params_str(default_params), "best": best, "default": default,
           "grid": [{k: v for k, v in r.items() if k != "params_raw"} for r in rows],
           "total_wall_s": time.perf_counter() - t_all}
    L.write_json(os.path.join(L.RESULTS, method + ".json"), out)
    return out


def params_str(p):
    if isinstance(p, dict):
        return " ".join("%s=%s" % (k, L.fnum(v) if isinstance(v, float) else v) for k, v in p.items())
    return str(p)


def grid(**axes):
    """grid(sim=[...], blend=[...]) -> list of dicts (cartesian product, insertion order)."""
    keys = list(axes)
    out = [{}]
    for k in keys:
        out = [dict(o, **{k: v}) for o in out for v in axes[k]]
    return out
