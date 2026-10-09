#!/usr/bin/env python
"""Background-removal benchmark driver.

    python harness.py --runner PATH --name NAME --device cuda|cpu
                      [--variants V ...] [--limit N] [--corpus DIR] [--results DIR]

A *runner* is a Python module (see RUNNER_PROTOCOL.md) with
``load(device) -> ctx`` and ``infer(ctx, frames) -> mattes`` plus optional
``BATCH`` / ``NAME`` / ``reset(ctx)``.

For every variant the harness decodes the frames, runs a warm-up on the first
3 frames, times one pass over all frames in batches of BATCH, saves the mattes
as 8-bit gray ``NNN.png`` under ``results/NAME/<variant>/``, writes
``preview_020.png`` (original | matte | subject over checkerboard) and, when
the variant has ground truth, the metrics from ``metrics.py``. Everything is
collected into ``results/NAME/metrics.json`` and ``results/NAME/summary.md``.

Default variants: every ``corpus/bg/*/png`` (named ``<bg>``), the dithered-GIF
decodes ``corpus/bg/{vgrad,multi,skin,noise}/gifpng`` (named ``<bg>-gifpng``)
and the real capture ``corpus/real/ex2/png`` (``ex2-real``, no GT).

Paths: ``--corpus`` / ``--results`` default to ``$EZLG_BENCH_CORPUS`` /
``$EZLG_BENCH_RESULTS``, else ``./corpus`` / ``./results`` next to this script
(``make-corpus.sh`` builds the former). Model files are the runners' business:
``$EZLG_BENCH_MODELS`` (default ``./models``), see ``models.json``.
"""
from __future__ import annotations

import argparse
import datetime as _dt
import importlib.util
import json
import os
import platform
import subprocess
import sys
import time
import traceback
import warnings

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:  # works under `python -I` too
    sys.path.insert(0, HERE)
# Corpus / results locations: flags beat env vars beat these defaults.
DEFAULT_CORPUS = os.environ.get("EZLG_BENCH_CORPUS") or os.path.join(HERE, "corpus")
DEFAULT_RESULTS = os.environ.get("EZLG_BENCH_RESULTS") or os.path.join(HERE, "results")

import numpy as np  # noqa: E402
from PIL import Image  # noqa: E402

import metrics  # noqa: E402

try:
    import psutil
except ImportError:  # pragma: no cover
    psutil = None

WARMUP_FRAMES = 3
PREVIEW_FRAME = "020.png"
GIFPNG_BGS = ("vgrad", "multi", "skin", "noise")


# ------------------------------------------------------------------ variants
def default_variants(corpus: str) -> list[dict]:
    out = []
    bgdir = os.path.join(corpus, "bg")
    gt = os.path.join(corpus, "gt")
    for bg in sorted(os.listdir(bgdir)) if os.path.isdir(bgdir) else []:
        p = os.path.join(bgdir, bg, "png")
        if os.path.isdir(p):
            out.append({"name": bg, "dir": p, "gt": gt})
    for bg in GIFPNG_BGS:
        p = os.path.join(bgdir, bg, "gifpng")
        if os.path.isdir(p):
            out.append({"name": f"{bg}-gifpng", "dir": p, "gt": gt})
    real = os.path.join(corpus, "real")
    if os.path.isdir(real):
        for ex in sorted(os.listdir(real)):
            p = os.path.join(real, ex, "png")
            if os.path.isdir(p):
                out.append({"name": f"{ex}-real", "dir": p, "gt": None})
    return out


def resolve_variants(corpus: str, wanted: list[str] | None) -> list[dict]:
    allv = default_variants(corpus)
    if not wanted:
        return allv
    byname = {v["name"]: v for v in allv}
    out = []
    for w in wanted:
        if w in byname:
            out.append(byname[w])
        elif os.path.isdir(w):  # an arbitrary frame directory, no GT unless it is a corpus dir
            name = os.path.basename(os.path.normpath(w))
            parent = os.path.basename(os.path.dirname(os.path.normpath(w)))
            vname = f"{parent}-{name}" if name in ("png", "gifpng") else name
            gt = os.path.join(corpus, "gt")
            has_gt = os.path.isdir(gt) and os.path.abspath(w).startswith(os.path.abspath(os.path.join(corpus, "bg")))
            out.append({"name": vname, "dir": os.path.abspath(w), "gt": gt if has_gt else None})
        else:
            raise SystemExit(f"unknown variant {w!r}; known: {', '.join(byname)}")
    return out


# -------------------------------------------------------------------- runner
def load_runner(path: str):
    path = os.path.abspath(path)
    rdir = os.path.dirname(path)
    if rdir not in sys.path:
        sys.path.insert(0, rdir)
    modname = "runner_" + os.path.splitext(os.path.basename(path))[0]
    spec = importlib.util.spec_from_file_location(modname, path)
    if spec is None or spec.loader is None:
        raise SystemExit(f"cannot import runner {path}")
    mod = importlib.util.module_from_spec(spec)
    sys.modules[modname] = mod
    spec.loader.exec_module(mod)
    for fn in ("load", "infer"):
        if not callable(getattr(mod, fn, None)):
            raise SystemExit(f"runner {path} has no {fn}()")
    return mod


def read_frames(d: str, limit: int | None) -> tuple[list[str], list[np.ndarray]]:
    names = metrics.list_frames(d)
    if limit is not None:
        names = names[: int(limit)]
    frames = []
    for n in names:
        im = Image.open(os.path.join(d, n))
        im.load()
        frames.append(np.ascontiguousarray(np.asarray(im.convert("RGB"), dtype=np.uint8)))
    return names, frames


def coerce_matte(m, h: int, w: int, what: str) -> np.ndarray:
    a = np.asarray(m)
    if a.ndim == 3 and a.shape[-1] == 1:
        a = a[..., 0]
    if a.ndim == 3 and a.shape[0] == 1:
        a = a[0]
    if a.ndim != 2:
        raise ValueError(f"{what}: matte must be HxW, got {a.shape}")
    if a.dtype == np.uint8:
        a = a.astype(np.float32) / 255.0
    elif a.dtype != np.float32:
        a = a.astype(np.float32)
    if a.shape != (h, w):
        warnings.warn(f"{what}: matte {a.shape} != frame {(h, w)}; resizing")
        a = np.asarray(Image.fromarray(np.clip(a * 255 + 0.5, 0, 255).astype(np.uint8)).resize((w, h), Image.BILINEAR), dtype=np.float32) / 255.0
    return np.clip(a, 0.0, 1.0)


def run_batches(mod, ctx, frames: list[np.ndarray], batch: int) -> list[np.ndarray]:
    out = []
    for i in range(0, len(frames), batch):
        chunk = frames[i : i + batch]
        res = mod.infer(ctx, chunk)
        res = list(res)
        if len(res) != len(chunk):
            raise ValueError(f"infer returned {len(res)} mattes for {len(chunk)} frames")
        for f, m in zip(chunk, res):
            out.append(coerce_matte(m, f.shape[0], f.shape[1], "infer"))
    return out


# ------------------------------------------------------------------- memory
def nvidia_smi_used_mb(index: int = 0) -> float | None:
    try:
        o = subprocess.run(
            ["nvidia-smi", f"--id={index}", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=10,
        )
        if o.returncode == 0 and o.stdout.strip():
            return float(o.stdout.strip().splitlines()[0])
    except Exception:
        pass
    return None


def peak_rss_mb() -> float | None:
    try:
        if psutil is not None:
            mi = psutil.Process().memory_info()
            if hasattr(mi, "peak_wset"):  # Windows
                return mi.peak_wset / 2**20
        import resource  # POSIX
        ru = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        return ru / 2**20 if sys.platform == "darwin" else ru / 1024.0
    except Exception:
        return None


def current_rss_mb() -> float | None:
    try:
        return psutil.Process().memory_info().rss / 2**20 if psutil else None
    except Exception:
        return None


class Cuda:
    """torch.cuda helpers that degrade to no-ops."""

    def __init__(self, device: str):
        self.torch = None
        self.on = False
        try:
            import torch
            self.torch = torch
            self.on = device.startswith("cuda") and torch.cuda.is_available()
        except Exception:
            pass

    def sync(self):
        if self.on:
            self.torch.cuda.synchronize()

    def reset_peak(self):
        if self.on:
            self.torch.cuda.reset_peak_memory_stats()

    def peak_mb(self) -> float | None:
        return self.torch.cuda.max_memory_allocated() / 2**20 if self.on else None

    def reserved_mb(self) -> float | None:
        return self.torch.cuda.max_memory_reserved() / 2**20 if self.on else None

    def name(self) -> str | None:
        return self.torch.cuda.get_device_name(0) if self.on else None

    def empty_cache(self):
        if self.on:
            self.torch.cuda.empty_cache()


# ------------------------------------------------------------------ preview
def checkerboard(h: int, w: int, cell: int = 16) -> np.ndarray:
    yy, xx = np.mgrid[0:h, 0:w]
    c = ((yy // cell + xx // cell) % 2).astype(np.uint8)
    board = np.where(c[..., None] == 1, 0xCC, 0x99).astype(np.uint8)
    return np.repeat(board, 3, axis=2)


def make_preview(frame: np.ndarray, matte: np.ndarray, path: str, gap: int = 8):
    h, w = matte.shape
    a = matte[..., None]
    board = checkerboard(h, w).astype(np.float32)
    comp = (frame.astype(np.float32) * a + board * (1 - a) + 0.5).astype(np.uint8)
    m8 = np.repeat((matte * 255 + 0.5).astype(np.uint8)[..., None], 3, axis=2)
    sep = np.full((h, gap, 3), 255, np.uint8)
    out = np.concatenate([frame, sep, m8, sep, comp], axis=1)
    Image.fromarray(out).save(path)


# ------------------------------------------------------------------- driver
def fmt(v, nd=4):
    if v is None:
        return "-"
    if isinstance(v, float):
        if v != v:
            return "nan"
        return f"{v:.{nd}f}"
    return str(v)


def write_summary(res: dict, path: str):
    rows = []
    rows.append(f"# {res['name']}\n")
    rows.append(f"runner `{res['runner']}` · device {res['device']} · batch {res['batch']} · limit {res['limit']} · {res['started']}")
    if res.get("gpu"):
        rows.append(f"GPU {res['gpu']} · torch {res.get('torch')} · load {fmt(res.get('load_ms'), 0)} ms")
    rows.append("")
    rows.append("| variant | frames | MAE | IoU@0.5 | bMAE(5px) | flicker | ms/frame | VRAM peak (torch) MB | nvidia-smi used MB | RSS peak MB |")
    rows.append("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
    for name, v in res["variants"].items():
        if "error" in v:
            rows.append(f"| {name} | - | ERROR: {v['error'][:60]} |  |  |  |  |  |  |  |")
            continue
        m = v.get("metrics") or {}
        rows.append(
            f"| {name} | {v['frames']} | {fmt(m.get('mae'))} | {fmt(m.get('iou'))} | {fmt(m.get('boundary_mae'))} | "
            f"{fmt(m.get('flicker'), 5)} | {fmt(v.get('ms_per_frame'), 2)} | {fmt(v.get('peak_vram_mb_torch'), 0)} | "
            f"{fmt(v.get('nvidia_smi_used_mb'), 0)} | {fmt(v.get('peak_rss_mb'), 0)} |"
        )
    for key, label in (("overall", "**mean (all GT variants)**"), ("overall_png", "mean (png only)"), ("overall_gifpng", "mean (gifpng only)")):
        o = res.get(key)
        if o:
            rows.append(
                f"| {label} | {o['variants']} | {fmt(o.get('mae'))} | {fmt(o.get('iou'))} | {fmt(o.get('boundary_mae'))} | "
                f"{fmt(o.get('flicker'), 5)} | {fmt(o.get('ms_per_frame'), 2)} | {fmt(o.get('peak_vram_mb_torch'), 0)} | "
                f"{fmt(o.get('nvidia_smi_used_mb'), 0)} | {fmt(o.get('peak_rss_mb'), 0)} |"
            )
    rows.append("")
    rows.append("MAE / bMAE / flicker: lower is better (alpha in 0..1). IoU: higher is better. ms/frame is wall time of infer() incl. host<->device copies, after a 3-frame warm-up, excluding PNG decode/encode.")
    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(rows) + "\n")


def mean_over(vs: list[dict], keys) -> dict | None:
    vs = [v for v in vs if "error" not in v]
    if not vs:
        return None
    out = {"variants": len(vs), "names": [v["name"] for v in vs]}
    for k in keys:
        vals = []
        for v in vs:
            src = v.get("metrics") if k in ("mae", "iou", "boundary_mae", "flicker") else v
            x = (src or {}).get(k)
            if isinstance(x, (int, float)) and x == x:
                vals.append(float(x))
        out[k] = float(np.mean(vals)) if vals else None
    return out


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--runner", required=True, help="path to the runner module (.py)")
    ap.add_argument("--name", required=True, help="results/NAME")
    ap.add_argument("--device", default="cuda", help="cuda | cuda:N | cpu (passed to load())")
    ap.add_argument("--variants", nargs="*", help="variant names (green, vgrad-gifpng, ex2-real, ...) or frame dirs; default: all")
    ap.add_argument("--limit", type=int, help="only the first N frames of each variant")
    ap.add_argument("--corpus", default=DEFAULT_CORPUS, help="corpus dir (default: $EZLG_BENCH_CORPUS, else ./corpus next to this script)")
    ap.add_argument("--results", default=DEFAULT_RESULTS, help="results root (default: $EZLG_BENCH_RESULTS, else ./results next to this script)")
    ap.add_argument("--batch", type=int, help="override the runner's BATCH")
    ap.add_argument("--no-warmup", action="store_true")
    a = ap.parse_args(argv)

    variants = resolve_variants(a.corpus, a.variants)
    if not variants:
        raise SystemExit(f"no variants found under {a.corpus}")
    outdir = os.path.join(a.results, a.name)
    os.makedirs(outdir, exist_ok=True)

    cuda = Cuda(a.device)
    if a.device.startswith("cuda") and not cuda.on:
        print("warning: --device cuda but torch.cuda is not available (runner may still use ORT/CUDA)", file=sys.stderr)
    smi_baseline = nvidia_smi_used_mb()

    mod = load_runner(a.runner)
    batch = int(a.batch or getattr(mod, "BATCH", 1) or 1)
    rname = getattr(mod, "NAME", None)
    t0 = time.perf_counter()
    ctx = mod.load(a.device)
    cuda.sync()
    load_ms = (time.perf_counter() - t0) * 1000.0
    reset = getattr(mod, "reset", None)

    res = {
        "name": a.name,
        "runner": os.path.abspath(a.runner),
        "runner_name": rname,
        "device": a.device,
        "batch": batch,
        "limit": a.limit,
        "started": _dt.datetime.now().isoformat(timespec="seconds"),
        "host": platform.node(),
        "python": sys.version.split()[0],
        "torch": getattr(cuda.torch, "__version__", None) if cuda.torch else None,
        "gpu": cuda.name(),
        "load_ms": load_ms,
        "nvidia_smi_used_mb_baseline": smi_baseline,
        "nvidia_smi_used_mb_after_load": nvidia_smi_used_mb(),
        "rss_mb_after_load": current_rss_mb(),
        "variants": {},
    }
    print(f"[{a.name}] runner={rname or os.path.basename(a.runner)} device={a.device} batch={batch} load={load_ms:.0f}ms gpu={res['gpu']}")

    for v in variants:
        vout = os.path.join(outdir, v["name"])
        os.makedirs(vout, exist_ok=True)
        entry = {"name": v["name"], "dir": v["dir"], "has_gt": v["gt"] is not None}
        res["variants"][v["name"]] = entry
        try:
            names, frames = read_frames(v["dir"], a.limit)
            if not frames:
                raise FileNotFoundError(f"no frames in {v['dir']}")
            h, w = frames[0].shape[:2]
            entry.update({"frames": len(frames), "width": w, "height": h})
            with warnings.catch_warnings(record=True) as wlog:
                warnings.simplefilter("always")
                if callable(reset):
                    reset(ctx)
                if not a.no_warmup:
                    run_batches(mod, ctx, frames[:WARMUP_FRAMES], batch)
                    cuda.sync()
                if callable(reset):
                    reset(ctx)
                cuda.reset_peak()
                t0 = time.perf_counter()
                mattes = run_batches(mod, ctx, frames, batch)
                cuda.sync()
                wall = time.perf_counter() - t0
                entry["warnings"] = sorted({str(x.message) for x in wlog})
            entry["ms_per_frame"] = wall * 1000.0 / len(frames)
            entry["wall_ms"] = wall * 1000.0
            entry["peak_vram_mb_torch"] = cuda.peak_mb()
            entry["peak_vram_reserved_mb_torch"] = cuda.reserved_mb()
            entry["nvidia_smi_used_mb"] = nvidia_smi_used_mb()
            entry["peak_rss_mb"] = peak_rss_mb()
            entry["rss_mb"] = current_rss_mb()

            for n, m in zip(names, mattes):
                Image.fromarray((m * 255.0 + 0.5).astype(np.uint8), "L").save(os.path.join(vout, n))
            pi = names.index(PREVIEW_FRAME) if PREVIEW_FRAME in names else len(names) - 1
            entry["preview_frame"] = names[pi]
            make_preview(frames[pi], mattes[pi], os.path.join(vout, "preview_020.png"))

            if v["gt"]:
                with warnings.catch_warnings(record=True) as wlog:
                    warnings.simplefilter("always")
                    mres = metrics.evaluate_dirs(v["gt"], vout, limit=a.limit)
                    entry["warnings"] += sorted({str(x.message) for x in wlog})
                entry["metrics"] = metrics.summary(mres)
                with open(os.path.join(vout, "metrics.json"), "w", encoding="utf-8") as f:
                    json.dump(mres, f, indent=1)
                m = entry["metrics"]
                print(f"  {v['name']:<14} {len(frames):>3}f  MAE {m['mae']:.4f}  IoU {m['iou']:.4f}  bMAE {m['boundary_mae']:.4f}  flicker {m['flicker']:+.5f}  {entry['ms_per_frame']:.2f} ms/f  vram {fmt(entry['peak_vram_mb_torch'], 0)} MB  smi {fmt(entry['nvidia_smi_used_mb'], 0)} MB  rss {fmt(entry['peak_rss_mb'], 0)} MB")
            else:
                print(f"  {v['name']:<14} {len(frames):>3}f  (no GT)  {entry['ms_per_frame']:.2f} ms/f  vram {fmt(entry['peak_vram_mb_torch'], 0)} MB  smi {fmt(entry['nvidia_smi_used_mb'], 0)} MB  rss {fmt(entry['peak_rss_mb'], 0)} MB")
        except Exception as e:  # keep going with the other variants
            entry["error"] = f"{type(e).__name__}: {e}"
            entry["traceback"] = traceback.format_exc()
            print(f"  {v['name']:<14} ERROR {entry['error']}", file=sys.stderr)
        # always persist progress
        with open(os.path.join(outdir, "metrics.json"), "w", encoding="utf-8") as f:
            json.dump(res, f, indent=1)

    keys = ("mae", "iou", "boundary_mae", "flicker", "ms_per_frame", "peak_vram_mb_torch", "nvidia_smi_used_mb", "peak_rss_mb")
    gtv = [v for v in res["variants"].values() if v.get("has_gt")]
    res["overall"] = mean_over(gtv, keys)
    res["overall_png"] = mean_over([v for v in gtv if not v["name"].endswith("-gifpng")], keys)
    res["overall_gifpng"] = mean_over([v for v in gtv if v["name"].endswith("-gifpng")], keys)
    res["finished"] = _dt.datetime.now().isoformat(timespec="seconds")
    with open(os.path.join(outdir, "metrics.json"), "w", encoding="utf-8") as f:
        json.dump(res, f, indent=1)
    write_summary(res, os.path.join(outdir, "summary.md"))
    o = res["overall"]
    if o:
        print(f"  overall ({o['variants']} GT variants): MAE {fmt(o['mae'])}  IoU {fmt(o['iou'])}  bMAE {fmt(o['boundary_mae'])}  flicker {fmt(o['flicker'], 5)}  {fmt(o['ms_per_frame'], 2)} ms/f")
    print(f"wrote {os.path.join(outdir, 'metrics.json')} and summary.md")
    errs = [n for n, v in res["variants"].items() if "error" in v]
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main())
