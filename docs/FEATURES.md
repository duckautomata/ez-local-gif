# Features in detail

Everything the app can do, grouped by the build phase that delivered it (the
phase plan itself is [`DESIGN.md`](DESIGN.md) §10). The short list is in the
[README](../README.md); how to drive it all over HTTP is in
[`USAGE.md`](USAGE.md#http-api).

## Phase 1

Accepted — renders verified on a private Discord server, see
[`reviews/discord-testkit-results.md`](reviews/discord-testkit-results.md):

- **ProRes 4444 / any video / GIF / animated WebP → Discord-safe GIF and animated WebP** with
  transparency: premultiplied-alpha toggle (on by default for ProRes), matte + 1-bit threshold
  for GIF, 8-bit straight alpha for WebP, one global palette, `gifsicle -O2 --careful`,
  `libwebp_anim` with `-loop 0`.
- **Discord linter** (`internal/discordlint`): checks and fixes the byte-level rules that make
  files render black / opaque / flickering / play-once after Discord's server-side transcode
  (GCE on every frame, frame-0 transparency flag, explicit disposal, NETSCAPE loop, VP8X
  ALPHA/ANIM flags, loop 0, no metadata); the result card shows the report.
- Trim, crop, resize/canvas, fps, speed, flip/rotate ops; still preview with scrubber over
  checkerboard / Discord dark / white.

## Phase 2

- **Fit to size** — `fitBytes` runs the ladder + secant search of DESIGN.md §5.4 (fps → colours
  → lossy/quality → downscale, mildest first, in parallel) so the primary file lands under the
  Discord emote (256 KiB) / sticker (512 KiB) budget or any byte target ("compress to X KiB",
  optionally keeping size / fps); the manifest reports the binding knob ("fit at 20 fps · 128
  colours · lossy 40") and lists the runner-up rungs as **alternatives**.
- **APNG stickers** — 320×320, ≤ 5 s, `-plays 0`: the sticker fit probes RGBA APNG first at
  ≥ 12.5 fps (best quality when it fits); the **indexed 8-bit-alpha APNG** (tile → pngquant →
  untile → `apng -pix_fmt pal8`, PLTE + tRNS) is the default rung (user-verified best on
  Discord) and GIF the fallback; APNG lint rules.
- **AVIF** — animated AVIF with alpha via `avifenc` (verified to animate with soft alpha as a
  Discord attachment; `--repetition-count infinite`), stills from a single-frame source; AVIF
  input is accepted (ffmpeg's mov demuxer exposes the alpha as a second stream).
- **Static images** — PNG (pngquant + oxipng) and JPEG (flattened onto the matte) from the first
  frame; static emote / sticker presets.
- **Frames** — export every frame as PNG / JPEG / lossless WebP plus a `frames.zip` (STORE'd;
  `?dl=1` downloads it as an attachment); the manifest lists every frame file.
- **Image sequences** — upload several images in one request (`delayMs` per frame, `delay` op
  to change it) → one sequence source → GIF / WebP / APNG / AVIF.
- **Optimize** — GIF → GIF without decoding (`gifsicle -O2 --lossy --colors`, frame dropping
  with delays merged), ezgif "optimize" parity.
- **Edit as source** — `POST /api/sources/from-result` turns any rendered file into a new source
  so results can be chained from the result card.
- Result memoisation by recipe hash + pipeline version, TTL / size sweeper for `/data`.

## Phase 3

Editing ops, DESIGN.md §4.3 — built and reviewed 2026-08-22:

- **Background removal** — `chromakey` (green/blue screen in YUV 4:4:4 with despill) and
  `colorkey` (pick any RGB colour with the eyedropper) ops, applied at full resolution before
  scaling; the result keeps 8-bit alpha in WebP/APNG/AVIF and is matted + thresholded for GIF.
  On a source that already has alpha (a ProRes 4444 export) the key's matte is intersected with
  the source alpha instead of replacing it.
- **Feather (soft edge)** — the `feather` op Gaussian-blurs the alpha edge (radius in source
  pixels, default 3; the soft band is ≈ 2–3× the radius), applied right after keying and before
  any geometry so it scales down with the output size; skipped on frames that carry no alpha.
  GIF thresholds the softened edge back to 1-bit — use WebP/APNG/AVIF to keep it.
- **Overlays** — `text` (ffmpeg `drawtext` through a text file, any fontconfig family the
  container knows: DejaVu and Noto Sans/Serif are bundled, `/fonts` can add more; semi-transparent
  colours are composited through a separate layer), static image and **animated** overlays
  (`overlay` op on a second uploaded source), each with anchor/position, opacity and a half-open
  `[start, end)` time range, placed on the final output canvas. Looping: GIF and video assets
  repeat until the base ends (`-stream_loop -1`); animated WebP/APNG assets loop through
  `-ignore_loop 0` and so obey their own loop count (a play-once file plays once); a finished
  overlay holds its last frame (`tpad`), and `noLoop` forces that for every asset.
- **Reverse** (ffmpeg's `reverse` filter after the output fit — its buffer is bounded by
  `EZLG_MAX_MASTER_BYTES`), **auto-crop** (crop to the content box of the alpha, detected on the
  keyed picture and memoised — the default suggestion for emotes), **animated Play preview**
  (`POST /api/proxy`: a low-res animated WebP of the op stack so keying and timed overlays can be
  judged in motion; stills and proxies are admitted through a preview semaphore and de-duplicated
  in flight), **font list** (`GET /api/fonts`).
- **ProRes alpha head** — `yuva*` sources are unpremultiplied at their native 10/12-bit depth and
  converted to RGBA once (ffmpeg-made ProRes stores alpha on the luma range; the earlier
  `gbrap12le` route left transparent pixels at alpha 1 — larger files, a useless auto-crop).
- **Trim on animated WebP sources** happens in the filtergraph (FFmpeg 9's `webp_anim` demuxer
  decodes nothing after an input seek); every other source keeps the µs-precise input seek.
- Multi-source recipes: overlay assets are ordinary uploads listed in `sources` after the main
  source and referenced by index; `/api/still`, `/api/proxy` and `/api/jobs` all take them.
- Phase 3 review fixes ([`reviews/phase3-review.md`](reviews/phase3-review.md)): the
  crop rectangle gained 8 edge/corner resize handles and a "Lock ratio" checkbox (constrains
  resizes, new rectangles and the W/H inputs), the Result card's backdrop choice persists across
  renders, and the Feather op above.

## Phase 4

Polish + extras, DESIGN.md §10 item 4 — built 2026-08-29:

- **Batch** — drop several videos/animations at once and apply one preset to all of them: a list
  view (per-file probe badge and premultiplied auto-default), the shared "Use for" chips and
  Output card once at the top, and only the geometry-independent global ops (fps, speed, feather,
  background removal, reverse, bounce); per-row render progress, result chip and Download, "Open
  in editor" per row, "Save all to /output". Pure frontend orchestration — one ordinary job per
  file.
- **MP4 / WebM export** — H.264 (`libx264`, `+faststart`) and VP9 (`libvpx-vp9`) video out, for
  chat clips: **opaque** (flattened onto the matte — Discord plays no transparent video), even
  dimensions enforced, audio dropped, attachment targets only (never emote/sticker). "Fit to
  ≤ N KiB" works via a CRF search, and a structural `LintVideo` (codec, even dims,
  moov-before-mdat, size vs target) fills the Discord checks.
- **`/input` picker and `/output` save** — bind a read-only host folder to `/input` to pick
  sources without uploading (`GET /api/input` + `POST /api/sources/from-input`), and save any
  result file to the `/output` bind with a collision-safe name
  (`POST /api/results/{recipeHash}/save`); both light up in the UI only when the folder is
  mounted (`features.inputPick` / `features.outputSave`). Paths configurable via `EZLG_INPUT` /
  `EZLG_OUTPUT`.
- **gifski HQ toggle** — Advanced GIF option "Encoder: gifski (HQ, slow)" for photographic
  content; chat/none targets only (gifski's per-frame palettes break on Discord emotes — verified
  in the render matrix), so it is never offered for emote/sticker.
- **Lossless gifsicle fast path** — a GIF source with only trim/crop/frame-drop/loop edits and no
  Discord fit budget skips the decode → re-quantise pipeline entirely (`gifsicle` operates on the
  original bytes; the result card says "lossless gifsicle (no re-encode)").
- **Bounce** — ezgif-style ping-pong (forward then back, doubles frames and duration), a checkbox
  next to Reverse.
- **Keyboard shortcuts** — Space plays/stops the preview, B cycles the backdrop, ? shows the
  shortcut overlay (plus the existing Ctrl+Enter render and ←/→ frame stepping).
