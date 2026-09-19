# Discord "frames stack" test — 2026-09-19

> **Provenance.** Written up by Claude (Claude Code) from the user's upload report and from
> structure dumps of the six test files. The Discord verdicts are the user's observations; the
> structure columns were measured from the files; the conclusion and the rule are the analysis
> that followed. Unlike the other files in this folder, this is not the user's own text.
> The test files live under `tmp/discord-stack-test/` (gitignored, not committed).

## Symptom

A transparent GIF rendered by the shipped pipeline (ffmpeg `palettegen`/`paletteuse` →
`gifsicle -O2 --careful` → `discordlint.LintGIF` fix) composites correctly in a spec decoder but
**"stacked" on Discord** as a chat attachment: each new pose was drawn on top of the previous
one, and the old pose was never cleared. The render (146×134, colour-keyed, 256 colours,
`sierra2_4a`, 354 cs) holds several poses for 0.1–1 s; the 2026-08-19 test-kit clip
([`discord-testkit-results.md`](discord-testkit-results.md)) never holds a pose, and its GIFs
rendered fine.

## Method

Six files made from that one render. Files 1–5 composite **pixel-identically** in a spec-correct
decoder (FFmpeg 9, sampled on a 10 ms grid); file 6 differs by a single pixel that ffmpeg's frame
cropping drops in the others. All total 354 cs. They differ only in GIF block structure, so
anything Discord shows differently is structural. Each was uploaded as a chat attachment.

ffmpeg's own output for this render (`base.gif`, the input of every variant) is 118 frames of
3 cs, every frame disposal 2, constant transparent index 255, 486,054 B — 17 distinct pictures,
each hold written as a run of identical frames. "Merge" below means folding those byte-identical
consecutive frames into one frame with the summed delay (118 → 17 frames).

| # | file | how it was made | frames | disposal | transparent index | frames that leave the picture unchanged | rects | bytes | Discord |
|---|---|---|---|---|---|---|---|---|---|
| 1 | `1-current-pipeline` | ffmpeg → `gifsicle -O2 --careful` (byte-identical to the file that stacked) | 20 | 1×8, 2×12 | varies (11 distinct) | **3 — frames 1, 3, 9**, all-transparent, disposal 2, 3 cs | cropped | 59,297 | **STACKS** |
| 2 | `2-no-clear-frames` | ffmpeg → merge → `gifsicle -O2 --careful` | 17 | 1×5, 2×12 | varies (11 distinct) | 0 | cropped | 58,640 | ok |
| 3 | `3-gifsicle-O1` | ffmpeg → `gifsicle -O1 --careful` | 20 | 1×8, 2×12 | varies (15 distinct) | **3 — frames 1, 3, 9**, *redraws* of the same pixels (none all-transparent), disposal 2, 3 cs | cropped | 74,090 | **STACKS** |
| 4 | `4-all-disposal2-varying-tidx` | ffmpeg → merge → `gifsicle` (no `-O`) | 17 | 2×17 | varies (8 distinct) | 0 | cropped | 72,830 | ok |
| 5 | `5-all-disposal2-cropped` | ffmpeg → merge (no gifsicle) | 17 | 2×17 | constant 255 | 0 | cropped | 72,959 | ok |
| 6 | `6-all-disposal2-fullcanvas` | ffmpeg `-gifflags -offsetting` → merge | 17 | 2×17 | constant 255 | 0 | full canvas | 73,985 | ok |

The user's report, verbatim: "`1-current-pipeline` and `3-gifsicle-O1` have the stacked frames
bug. Everything else (2,4,5,6) are all correct and look identical."

### What the three frames are

File 1, first frames (rect, disposal, delay):

```
f0  7,0 126x134  disposal 1  78 cs   the pose
f1  7,0 126x134  disposal 2   3 cs   every pixel transparent      <- leaves the picture unchanged
f2  7,0 126x134  disposal 1  93 cs   next pose
f3  7,0 126x134  disposal 2   3 cs   every pixel transparent      <-
...
f8  45,32 79x55  disposal 1  27 cs
f9  7,0 101x100  disposal 2   3 cs   every pixel transparent      <-
```

The merged files carry those three holds as single frames of 81, 96 and 30 cs; gifsicle's
optimiser splits each into (78 + 3), (93 + 3), (27 + 3): the run of identical frames becomes one
disposal-1 frame, and the **last duplicate is kept as a separate short disposal-2 frame whose
only job is to erase the area** before the next pose. Under `-O2` that frame is all-transparent;
under `-O1` (file 3) it redraws the same pixels (frame 1 has exactly frame 0's 8,337 transparent
pixels of 16,884). Holds whose next frame needs nothing cleared (the 12, 18 and 84 cs ones) are
left whole. Checked structurally (not uploaded): `-O3`, `-U -O2 --careful`, `-U -O1` and an
`-O2` keep-empty variant all carry the same three frames — no gifsicle flag avoids them — and
plain `gifsicle -U` keeps them too while writing a disposal 0/2 mix (0×8, 2×12), not the
"coalesced, disposal 2" file DESIGN.md §5.3 used to describe. `gifsicle -U --disposal=background`
without `-O` does give 20 full-canvas disposal-2 frames, whose three duplicates are then harmless.

## Conclusion

**Discord's production media pipeline drops a frame that does not change the composited picture
and folds its delay into the previous frame — and the dropped frame's disposal is lost with
it.** In files 1 and 3 the dropped frames are the ones carrying the disposal-2 clear, so the
clear never happens and the next pose is drawn over the old one.

- It is not about all-transparent frames: file 3 has none and stacks.
- It is not mixed disposal 1/2, sub-rect delta frames, or a per-frame transparent index: file 2
  has all three and is correct.
- All-disposal-2 files are correct whether cropped or full canvas, with a constant or a varying
  transparent index (files 4–6).
- **Public `discord/lilliput` does not reproduce it**: HEAD b337020 (2026-08-18) and four older
  commits (c11ef94, 29e8772, eaa601f, 686cbce), built locally, keep every frame. It cannot be
  tested locally, so the rule below is the contract, and only an upload verifies a change.

## The rule (`gif.noop-frame-disposal`)

Simulate the GIF per spec on a canvas where 0 = background/cleared and any drawn pixel is an
opaque colour value: pixels equal to the frame's transparent index (GCE transparency flag set)
are skipped; colours come from the frame's local table, else the global one; disposal 2 clears
the frame's rect to 0; disposal 0/1 leave it; disposal 3 restores the rect to what it was before
the frame was drawn. Let P[k] be the canvas after drawing frame k and D[k] the canvas after then
applying frame k's disposal. Nothing is clipped: a frame whose rect sticks out of the logical
screen (decoders clip, grow the screen or reject) and a frame with a reserved disposal value 4–7
(Chromium/Firefox read 4 as restore-previous, ffmpeg/gifsicle as leave) **end the analysis at that
frame**, as undecodable LZW data and the analysis caps do; the frames before it are still judged.

- Frame k ≥ 1 is a **no-op** when P[k] == P[k−1].
- A no-op is **harmless** when D[k] == D[k−1] — dropping it and adding its delay to frame k−1
  changes nothing — and also when it is the **last** frame (its disposal never matters: the
  canvas is reset on loop restart). Otherwise it is **unsafe**.
- Cheap equivalent for disposals 0/1/2: with C[k] = frame k's rect if its disposal is 2, else
  empty, D[k] == D[k−1] iff every pixel of P[k−1] inside the symmetric difference of C[k] and
  C[k−1] is already 0. If either frame uses disposal 3 and (disposal, rect) differ, treat the
  frame as unsafe.

A GIF with any unsafe frame fails the rule (error for Discord targets, warning for target none —
the re-encode below runs at either level). When the analysis ended early, unsafe frames found
before that point still fail the rule; otherwise the check passes with a "not analysed from frame
N on" note (a canvas over the 16 M-pixel cap is not analysed at all and passes with a note).
Checked against the files above and the earlier evidence with a stdlib prototype:

| file | verdict |
|---|---|
| `1-current-pipeline`, `3-gifsicle-O1` | unsafe frames 1, 3, 9 |
| `2-…`, `4-…`, `5-…`, `6-…` | clean, no no-op frames at all |
| ffmpeg's raw `base.gif` (118 frames, all disposal 2) | clean — 101 harmless no-op frames (every duplicate); `MergeGIFHolds` 118 → 17 |
| `tmp/Dragoon.gif` (119 frames, all disposal 1) | clean — 102 harmless no-op frames, no unsafe one; `MergeGIFHolds` 119 → 17 |
| the six GIFs of the 2026-08-19 test kit (75 frames each) | clean, no no-op frames — that clip has no holds |

## What changed because of it

- **Render path:** after ffmpeg writes `base.gif` and before gifsicle, `discordlint.MergeGIFHolds`
  folds every harmless no-op frame into the previous frame's delay, so the optimiser never sees a
  hold — file 2's recipe, the smallest of the six (58,640 B vs 59,297 B for the shipped
  pipeline). Without gifsicle the output has file 5's structure (72,959 B there, against
  486,054 B for the unmerged `base.gif`).
- **Lint:** the rule above, `discordlint.RulesVersion` 2026-09-19.1.
- **Re-encode ladder:** `gifsicle --colors N` (skipped when this rule is the only structural
  failure) → coalesce with `gifsicle -U --disposal=background` and no `-O` (every frame a
  full-canvas disposal-2 frame, pixel-exact) → `MergeGIFHolds` → `gifsicle -O2 --careful
  [--lossy]` → else the coalesced, merged file as is. The `-U -O2 --careful` and plain `-U`
  rungs are gone. A failed hold rule counts as a structural failure **at any level**, so this
  runs for target none (where the rule is only a warning) as for Discord targets: through
  `jobs.lintGIF` for plain renders, the lossless fast path and gifski, and through
  `jobs.repairIfOnlyHolds` — only when this rule is their *only* structural failure — for the
  optimize preset, its fit candidates and the render fit's GIF candidates. Known cost, not
  fixed: there the re-optimise step applies `--lossy` a second time to already-lossy bytes
  (second-generation lossy, ~2.3× the gifsicle work per repaired candidate).
  `jobs.PipelineVersion` 2026-09-19.2.
- **`MergeGIFHolds` contract:** harmless no-ops only, never across a missing/malformed GCE, a
  delay sum over 65535 cs or a user-input flag; an unsafe no-op goes only when merging the tail
  makes it the last frame (and only when every frame was analysed); nothing is touched from the
  first unanalysable frame on. It runs on the *encoder's* output before gifsicle
  (`jobs.encodeGIFAt`) and inside the repair — the fast path and the optimize preset cannot
  pre-merge (frame selection / crop happen inside the same gifsicle call) and rely on lint +
  repair.
- Design record: [`../DESIGN.md`](../DESIGN.md) §4.2 (GIF row), §5.2 item 9, §5.3 (rule, ladder,
  `MergeGIFHolds`), §9b.

What is verified by upload is the six-file matrix above, and one fact ties the fix to it: the
rebuilt pipeline's output for the reported recipe is **byte-identical to file 2** (58,640 B,
"ok" on Discord). Not uploaded: the repair's outputs — in particular the coalesced last rung
(full canvas + all disposal 2 as in file 6, but with gifsicle's per-frame transparent index as
in file 4: each verified, the combination not) — lossy / fit outputs, repaired foreign GIFs, and
any input other than this clip.
