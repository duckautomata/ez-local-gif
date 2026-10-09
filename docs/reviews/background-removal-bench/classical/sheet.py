"""Visual sheet of the oracle-best mattes: for the given variants, frame 20 of
source | GT | each method's best matte composited over Discord dark #313338.
Writes <results>/classical/_sheet_<variant>.png and prints the column order."""
import os
import sys

import numpy as np
from PIL import Image

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # python -I: make the sibling modules importable
import bench_lib as L

METHODS = ["chromakey", "colorkey", "hsvkey", "stack-colorkey", "backgroundkey", "platediff-rgbmax", "wand-seed", "wand-grow", "wand-hybrid"]
DARK = np.array([0x31, 0x33, 0x38], np.float32)


def main(variants, frame=20, cell=240):
    for v in variants:
        indir = dict(L.variants())[v]
        src = np.asarray(Image.open(os.path.join(indir, "%03d.png" % frame)).convert("RGB"))
        gt = np.asarray(Image.open(os.path.join(L.CORPUS, "gt", "%03d.png" % frame)).convert("L"))
        cols = [("source", None), ("GT", gt)]
        for m in METHODS:
            p = os.path.join(L.RESULTS, m, v, "%03d.png" % frame)
            if os.path.exists(p):
                cols.append((m, np.asarray(Image.open(p).convert("L"))))
        sheet = Image.new("RGB", (cell * len(cols), cell), (0, 0, 0))
        for i, (name, a) in enumerate(cols):
            if a is None:
                im = src
            else:
                af = a.astype(np.float32)[..., None] / 255.0
                im = (src.astype(np.float32) * af + DARK * (1 - af)).astype(np.uint8)
            sheet.paste(Image.fromarray(im).resize((cell, cell), Image.BILINEAR), (i * cell, 0))
        out = os.path.join(L.RESULTS, "_sheet_%s.png" % v)
        sheet.save(out)
        print(v, "->", out, "columns:", [c[0] for c in cols])


if __name__ == "__main__":
    main(sys.argv[1].split(",") if len(sys.argv) > 1 else ["vgrad", "multi", "skin", "multi-gifpng", "anim"])
