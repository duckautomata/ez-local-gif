"""Aggregate <results>/classical/*.json into <results>/classical/summary.json and print markdown
tables: per method x variant the oracle best (params) and the single default."""
import glob
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L

ORDER = ["chromakey", "colorkey", "hsvkey", "stack-colorkey", "stack-chromakey", "backgroundkey",
         "platediff-rgbmax", "platediff-yuvsum", "wand-seed", "wand-grow", "wand-hybrid"]
VARS = [v for v, _ in L.variants()]


def load():
    out = {}
    for p in glob.glob(os.path.join(L.RESULTS, "*.json")):
        name = os.path.basename(p)[:-5]
        if name.startswith("_") or name.startswith("timing") or name == "summary":
            continue
        with open(p) as f:
            d = json.load(f)
        if isinstance(d, dict) and "best" in d and "grid" in d:
            out[name] = d
    return out


def fmt(m):
    if m is None:
        return "-"
    return "%.4f / %.3f / %.4f / %+.4f" % (m["mae"], m["iou"], m["boundary_mae"], m["flicker"])


def main():
    data = load()
    names = [n for n in ORDER if n in data] + sorted(n for n in data if n not in ORDER)
    summary = {}
    for n in names:
        d = data[n]
        summary[n] = {"default_params": d.get("default_params"), "best": {}, "default": {}}
        for v in VARS:
            b = d["best"].get(v)
            df = d["default"].get(v)
            if b:
                summary[n]["best"][v] = {k: b[k] for k in ("mae", "iou", "boundary_mae", "flicker", "gif_mae", "gif_flicker", "params", "wall_s") if k in b}
                summary[n]["best"][v]["chain"] = b.get("chain", "")
            if df:
                summary[n]["default"][v] = {k: df[k] for k in ("mae", "iou", "boundary_mae", "flicker", "gif_mae", "gif_flicker", "params", "wall_s") if k in df}
    L.write_json(os.path.join(L.RESULTS, "summary.json"), summary)
    # tables
    print("## Oracle best per variant (MAE / IoU / boundary MAE / flicker) [params]\n")
    print("| method | " + " | ".join(VARS) + " |")
    print("|---|" + "---|" * len(VARS))
    for n in names:
        cells = []
        for v in VARS:
            b = summary[n]["best"].get(v)
            cells.append(("%.4f/%.3f [%s]" % (b["mae"], b["iou"], b["params"])) if b else "-")
        print("| %s | %s |" % (n, " | ".join(cells)))
    print("\n## Single default per variant (MAE / IoU)\n")
    print("| method (default) | " + " | ".join(VARS) + " |")
    print("|---|" + "---|" * len(VARS))
    for n in names:
        cells = []
        for v in VARS:
            b = summary[n]["default"].get(v)
            cells.append(("%.4f/%.3f" % (b["mae"], b["iou"])) if b else "-")
        print("| %s [%s] | %s |" % (n, summary[n]["default_params"], " | ".join(cells)))
    # stacked keys: best per K
    if "stack-colorkey" in data:
        print("\n## Stacked colorkey: best MAE per number of picks K\n")
        print("| variant | K=2 | K=3 | K=4 | K=6 |")
        print("|---|---|---|---|---|")
        for v in VARS:
            cells = []
            for k in (2, 3, 4, 6):
                rs = [r for r in data["stack-colorkey"]["grid"] if r["variant"] == v and r["params"].startswith("k=%d " % k) and "error" not in r]
                if rs:
                    b = min(rs, key=lambda r: r["mae"])
                    cells.append("%.4f/%.3f [%s]" % (b["mae"], b["iou"], b["params"]))
                else:
                    cells.append("-")
            print("| %s | %s |" % (v, " | ".join(cells)))
    # plate kinds: best per kind
    if "platediff-rgbmax" in data:
        print("\n## Plate difference key (rgbmax): best MAE per plate kind\n")
        kinds = ["median45", "first", "coons", "laplace", "axis1d"]
        print("| variant | " + " | ".join(kinds) + " |")
        print("|---|" + "---|" * len(kinds))
        for v in VARS:
            cells = []
            for k in kinds:
                rs = [r for r in data["platediff-rgbmax"]["grid"] if r["variant"] == v and r["params"].startswith("kind=%s " % k) and "error" not in r]
                if rs:
                    b = min(rs, key=lambda r: r["mae"])
                    cells.append("%.4f/%.3f [%s]" % (b["mae"], b["iou"], b["params"].split(" ", 1)[1]))
                else:
                    cells.append("-")
            print("| %s | %s |" % (v, " | ".join(cells)))
    # post-processing tables
    for n in names:
        if not n.startswith("post-"):
            continue
        print("\n## %s: MAE (gif_mae) per post step, relative to none\n" % n)
        posts = []
        for r in data[n]["grid"]:
            p = r["params"].split("=", 1)[1]
            if p not in posts:
                posts.append(p)
        print("| variant | " + " | ".join(posts) + " |")
        print("|---|" + "---|" * len(posts))
        for v in VARS:
            rs = {r["params"].split("=", 1)[1]: r for r in data[n]["grid"] if r["variant"] == v and "error" not in r}
            if not rs:
                continue
            cells = ["%.4f (%.4f)" % (rs[p]["mae"], rs[p]["gif_mae"]) if p in rs else "-" for p in posts]
            print("| %s | %s |" % (v, " | ".join(cells)))


if __name__ == "__main__":
    main()


def robust_defaults(data, names=None, exclude=("noise", "noise-gifpng", "noise-mp4", "busy")):
    """For each method: the single params minimising the mean MAE over the variants
    (excluding the static-noise ones and busy by default), plus its per-variant MAE."""
    out = {}
    for n in (names or data):
        if n.startswith("post-") or n not in data:
            continue
        by = {}
        for r in data[n]["grid"]:
            if "error" in r or r["variant"] in exclude:
                continue
            by.setdefault(r["params"], {})[r["variant"]] = r
        nvar = len({r["variant"] for r in data[n]["grid"] if r["variant"] not in exclude})
        cands = [(sum(m["mae"] for m in d.values()) / nvar, p, d) for p, d in by.items() if len(d) == nvar]
        if not cands:
            continue
        mean_mae, p, d = min(cands)
        out[n] = {"params": p, "mean_mae": mean_mae, "per_variant": {v: {k: d[v][k] for k in ("mae", "iou", "boundary_mae", "flicker")} for v in d}}
    return out


if __name__ == "__main__" and "--robust" in sys.argv:
    data = load()
    rd = robust_defaults(data)
    L.write_json(os.path.join(L.RESULTS, "robust_defaults.json"), rd)
    print("\n## Robust single setting per method (min mean MAE over the non-noise, non-busy variants)\n")
    for n, r in rd.items():
        print("- %s: [%s] mean MAE %.4f :: %s" % (n, r["params"], r["mean_mae"], " ".join("%s=%.4f" % (v, m["mae"]) for v, m in sorted(r["per_variant"].items()))))
