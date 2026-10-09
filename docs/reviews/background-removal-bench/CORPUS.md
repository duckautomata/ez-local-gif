# Background-removal benchmark corpus

Built by `make-corpus.sh` from the user's own DaVinci Resolve export
(`output/resolve/example-1.mov` by default, `EZLG_BENCH_SRC` otherwise:
ProRes 4444, premultiplied alpha, 720x720, 60 fps): an illustrated
VTuber-style character (blonde hair, purple top, dark plush) with **binary
(0/255) alpha**. Window t = 2..5 s at 15 fps -> **45 frames**, 720x720. The
subject's alpha is the ground truth for every composite below. A different
source clip gives a different ground truth: its numbers are not comparable
with the ones in `docs/background-removal-proposal.md`.

Layout (all PNG sequences are 001.png .. 045.png, frame i of every variant is
the same instant):

- `subject/rgba/` straight-alpha RGBA subject frames (what a perfect removal returns)
- `gt/`           8-bit gray ground-truth alpha (255 = subject), one per frame
- `bg/<name>/png/`     opaque RGB composite of the subject over background <name>
- `bg/<name>/<name>.mp4`  the same, H.264 crf 18 yuv420p (web-realistic source)
- `bg/<name>/<name>.gif` + `bg/<name>/gifpng/`  (vgrad, multi, skin, noise only)
      256-colour bayer-dithered GIF of the composite and its decoded frames
      (the typical emote/sticker source; dither noise defeats colour keys)
- `real/ex2/png/`, `real/ex2/ex2.mp4`  a REAL screen capture (`EZLG_BENCH_SRC2`,
      `output/resolve/example-2.mov` by default, flattened: the character over
      a streaming UI, 256x256, 16 fps, 45 frames). No ground truth - judge
      visually. Skipped when the clip is missing.

The classical grids add one derived input, `noise-mp4`: `bg/noise/noise.mp4`
decoded back to PNG by ffmpeg (the 4:2:0 crf-18 version of the noise variant).
`classical/bench_lib.py` makes it on first use under `<results>/classical/_inputs/`.

Backgrounds:

| name  | what                                                   | why |
|-------|--------------------------------------------------------|-----|
| green | solid 0x00ff00                                         | control: chroma key should be ~perfect |
| white | solid white                                            | common web sticker source; subject has white highlights |
| black | solid black                                            | subject has black line art -> colour key eats outlines |
| vgrad | vertical 2-colour gradient (navy -> violet), static    | the user's complaint: gradients |
| multi | diagonal 4-colour gradient, static                     | multiple colours |
| skin  | pale-yellow -> orange gradient (hair-coloured), static | background shares the subject's hues |
| anim  | 3-colour gradient rotating (speed 0.05), random endpoints | moving background (defeats static-plate methods) |
| busy  | ffmpeg testsrc2 (moving, textured, many colours)       | worst case for colour methods |
| noise | vgrad + temporal grain (noise=alls=24), 4:2:0 in mp4   | compression/sensor noise |

Eight of the nine backgrounds rebuild bit-identically (checked against the
research corpus: every PNG, MP4 and GIF). `anim` is the exception: its
`gradients` source names no `x0/y0/x1/y1`, so ffmpeg picks random endpoints
per run (the filter's `seed` defaults to -1) and every build of the corpus gets
a different `anim` background. The definition is kept as the research used it;
`anim` numbers from two builds are comparable only statistically. Give the
source a `seed=N` if a fixed one is wanted.

Metrics per (method, background), averaged over the 45 frames, alpha in 0..1:
MAE (mean |a - gt|), IoU at threshold 0.5, and boundary error (MAE within a 5 px
band of the GT edge). Also measure wall time per frame and peak VRAM/RSS.

Plus a temporal **flicker** metric: mean over t>=2 of MAE(alpha_t, alpha_{t-1})
minus the same quantity computed on the ground truth (0 = as stable as the GT;
GIF output shows every bit of matte flicker as popping pixels).

Two scorers implement these definitions: `metrics.py` (the AI harness; the
5 px band is a Euclidean disk of radius 5, an 11x11 footprint) and
`classical/metrics.py` (the classical grids; the band uses a square 11x11
structuring element and the scorer adds `gif_*` variants of MAE / boundary MAE /
flicker with the prediction binarised at >= 128, what a GIF shows). MAE, IoU
and flicker are the same definition in both; only the boundary band differs.
