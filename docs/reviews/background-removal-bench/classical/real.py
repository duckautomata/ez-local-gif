"""Real screen capture (corpus/real/ex2/png, 256x256, no GT): run the main
methods, save mattes under <results>/classical/real-ex2/<method>/ and build a contact
sheet (frame 1 / 20 / 40: source, then each matte composited over white) at
<results>/classical/real-ex2/sheet.png for visual judgement."""
import os
import sys

import numpy as np
from PIL import Image

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L
import run_m2, run_m3, run_m6, plates

IN = os.path.join(L.CORPUS, "real", "ex2", "png")
OUT = os.path.join(L.RESULTS, "real-ex2")
NF = 45
W = H = 256


def run(args_in, filt, fc=False):
    arr, dt = L.run_ffmpeg_gray(args_in, filt, n=NF, filter_complex=fc)
    return arr


def main():
    L.H, L.W = H, W  # run_ffmpeg_gray checks the byte count against H*W
    frames = np.stack([np.asarray(Image.open(os.path.join(IN, "%03d.png" % i)).convert("RGB")) for i in range(1, NF + 1)])
    f0 = frames[0]
    c = L.corner_median(f0)
    ring = L.border_ring(f0, 4, run_m2.RING_EDGES)
    print("corner median", c, "ring k-means 4:", L.kmeans_colors(ring, 4))
    inp = L.input_args(IN)
    mattes = {}
    mattes["chromakey s0.2"] = run(inp, "format=yuva444p,chromakey=color=%s:similarity=0.2:blend=0.05,alphaextract,format=gray" % L.hexc(c))
    mattes["chromakey s0.1"] = run(inp, "format=yuva444p,chromakey=color=%s:similarity=0.1:blend=0,alphaextract,format=gray" % L.hexc(c))
    mattes["colorkey s0.1"] = run(inp, "format=rgba,colorkey=color=%s:similarity=0.1:blend=0,alphaextract,format=gray" % L.hexc(c))
    mattes["colorkey s0.2"] = run(inp, "format=rgba,colorkey=color=%s:similarity=0.2:blend=0,alphaextract,format=gray" % L.hexc(c))
    mattes["stack-colorkey k4 s0.08"] = run(inp, run_m2.stack_filter("color", L.kmeans_colors(ring, 4), 0.08, 0.0), True)
    mattes["stack-colorkey k6 s0.1"] = run(inp, run_m2.stack_filter("color", L.kmeans_colors(ring, 6), 0.1, 0.0), True)
    # plates
    pd = os.path.join(OUT, "_plates")
    for kind, fn in (("coons", plates.coons), ("axis1d", plates.axis1d)):
        od = os.path.join(pd, kind)
        os.makedirs(od, exist_ok=True)
        for i, f in enumerate(frames):
            Image.fromarray(fn(f)).save(os.path.join(od, "%03d.png" % (i + 1)))
        for T, Wd in ((16, 16), (32, 16)):
            filt = ("[0:v]format=gbrp[c];[1:v]format=gbrp[p];[c][p]blend=all_mode=difference:shortest=1,extractplanes=g+b+r[g][b][r];"
                    "[g][b]blend=all_mode=lighten[gb];[gb][r]blend=all_mode=lighten,lut=y='clip((val-%d)*255/%d\\,0\\,255)'[out]") % (T, Wd)
            mattes["platediff %s T%d" % (kind, T)] = run(inp + ["-framerate", "15", "-i", os.path.join(od, "%03d.png")], filt, True)
    od = os.path.join(pd, "median45")
    os.makedirs(od, exist_ok=True)
    Image.fromarray(np.median(frames, axis=0).astype(np.uint8)).save(os.path.join(od, "plate.png"))
    filt = ("[0:v]format=gbrp[c];[1:v]format=gbrp[p];[c][p]blend=all_mode=difference:shortest=1,extractplanes=g+b+r[g][b][r];"
            "[g][b]blend=all_mode=lighten[gb];[gb][r]blend=all_mode=lighten,lut=y='clip((val-16)*255/16\\,0\\,255)'[out]")
    mattes["platediff median45 T16"] = run(inp + ["-loop", "1", "-framerate", "15", "-i", os.path.join(od, "plate.png")], filt, True)
    ctx = run_m6.setup("real", IN)
    ctx["frames"] = frames
    L.N = NF
    for p in ({"mode": "seed", "k": 4, "tol": 24, "med": 0}, {"mode": "grow", "tol": 8, "med": 0}, {"mode": "grow", "tol": 12, "med": 3}, {"mode": "hybrid", "tol": 8, "gtol": 64, "med": 0}):
        arr, _ = run_m6.run_numpy("real", IN, ctx, p)
        mattes["wand " + " ".join("%s=%s" % kv for kv in p.items() if kv[0] != "mode") + " " + p["mode"]] = arr
    # save + sheet
    names = list(mattes)
    for n in names:
        L.save_mattes(mattes[n], os.path.join(OUT, n.replace(" ", "_")))
    sel = [0, 19, 39]
    cell = W
    sheet = Image.new("RGB", (cell * (len(names) + 1), cell * len(sel)), (255, 255, 255))
    for r, fi in enumerate(sel):
        sheet.paste(Image.fromarray(frames[fi]), (0, r * cell))
        for ci, n in enumerate(names):
            a = mattes[n][fi].astype(np.float32)[..., None] / 255.0
            comp = (frames[fi].astype(np.float32) * a + np.array([255, 255, 255], np.float32) * (1 - a)).astype(np.uint8)
            sheet.paste(Image.fromarray(comp), ((ci + 1) * cell, r * cell))
    sheet.save(os.path.join(OUT, "sheet.png"))
    with open(os.path.join(OUT, "sheet_columns.txt"), "w") as f:
        f.write("col0=source; " + "; ".join("col%d=%s" % (i + 1, n) for i, n in enumerate(names)))
    print("columns:", ["source"] + names)
    for n in names:
        print("  %-32s transparent fraction %.3f, flicker(raw) %.4f" % (n, 1 - mattes[n].mean() / 255, np.abs(np.diff(mattes[n].astype(np.float32), axis=0)).mean() / 255))


if __name__ == "__main__":
    main()
