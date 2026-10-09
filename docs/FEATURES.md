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
  for GIF, 8-bit straight alpha for WebP, one global palette, held frames merged and then
  `gifsicle -O2 --careful` (the merge is a 2026-09-19 fix, see the end of this page),
  `libwebp_anim` with `-loop 0`.
- **Discord linter** (`internal/discordlint`): checks and fixes the byte-level rules that make
  files render black / opaque / flickering / play-once after Discord's server-side transcode
  (GCE on every frame, frame-0 transparency flag, explicit disposal, no frame that leaves the
  picture unchanged but changes the disposal, NETSCAPE loop, VP8X ALPHA/ANIM flags, loop 0, no
  metadata); the result card shows the report.
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
  the source alpha instead of replacing it. (Phase 5a fixed the chroma key's colour range, moved
  the defaults and reworked the card — see [Phase 5a](#phase-5a) below.)
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

## Post-Phase-4 fixes

- **Large sources are editable in-app (2026-09-13)** — the frame-master cap
  (`EZLG_MAX_MASTER_BYTES`, 2 GiB by default) now lives in the job admission alone. The still
  preview and the Play proxy of any source work whatever its length (an untrimmed 2560×1440,
  704-frame clip used to fail its very first preview with "exceeds the 8 GiB limit" — a cap in
  the filter compiler on a master that previews never build), so trim / crop / resize / fit are
  applied in the app and the render's master is measured at the *output* size: an emote from a
  long 4K clip is small. Reversed / bounced previews and static (PNG/JPEG) reversed renders are
  admitted by their `reverse` buffer instead (a bounce alone buffers nothing before a static
  render's first frame, so a bounced PNG costs one decoded frame), the estimate is overflow-safe,
  and the cap is clamped at a documented ceiling. No result-cache version was bumped (cached
  outputs are unaffected); DESIGN.md §4.1 has the practical-limits table.
- **Live size estimate under Render** — `W×H · N frames · ~X of decoded frames` (`· about k×
  that on scratch` for fit / AVIF / frames / gifski renders; `· ~Y buffered by the reverse` for a
  reversed PNG/JPEG export, the RAM the server admits it on; "up to … before crop-to-content"
  while auto-crop is on), computed from the probe and the op stack against the server's
  `maxMasterBytes` / `scratchBudgetBytes` (both new in `GET /api/capabilities`). Over any bound
  it becomes a note with the frames and seconds that fit at that size (seconds floored to a
  tenth, so a trim to them always fits; the scratch note names the published budget — `shm_size`
  minus the preview cache; with auto-crop on it says the render *may* be refused, since the
  server measures after detection); a Frames export over 2000 frames says so. Render stays
  enabled — the server has the last word. Batch rows get no estimate.
- **Preview zoom shows real output pixels (2026-09-13)** — 1× / 2× / 4× used to stretch the
  Fit-mode still (≤ 480 px wide, 720 on wide screens), so 4× of a 2560-wide output was a blurry
  480-px frame. A fixed zoom now requests the still unscaled (the output canvas, as overlay mode
  already did): 1× is one output pixel per CSS pixel and 2× / 4× magnify the frame the render
  produces. Fit keeps the small still; the larger PNG per scrub step is paid only while zoomed.
- **GIFs with held poses no longer "stack" on Discord (2026-09-19)** — a transparent GIF whose
  subject holds still for a while rendered correctly everywhere except Discord, where each new
  pose was drawn on top of the old one. Discord drops a frame that does not change the picture,
  and that frame's disposal with it; gifsicle's optimiser (`-O1` / `-O2` / `-O3` alike) turns the
  end of a hold into exactly such a frame — a short disposal-2 frame whose only job is to clear
  the area — so the clear was lost. Confirmed by the user with a six-variant upload test
  ([`reviews/discord-stack-test-2026-09-19.md`](reviews/discord-stack-test-2026-09-19.md));
  public lilliput does not reproduce it. Three changes: the GIF path merges held frames into the
  previous frame's delay **before** gifsicle (`discordlint.MergeGIFHolds` — also several times
  smaller without gifsicle: 486 KB → 73 KB on the reported clip, and 58.6 KB vs 59.3 KB with
  it); the linter's new `gif.noop-frame-disposal` check fails such frames (error for Discord
  targets, warning otherwise); and the re-encode ladder became `--colors N` → coalesce
  (`gifsicle -U --disposal=background`) → merge holds → `-O2 --careful` → the coalesced
  all-disposal-2 file. That repair runs whenever the check fails, for **every** target (also as
  the warning it is for target none): plain renders, the lossless fast path and gifski get it
  through the ladder; the optimize preset (which, like the fast path, cannot merge first:
  frame selection and crop happen inside the same gifsicle call) and the fit candidates run it
  when the check is their only structural failure (their description then says "held frames
  re-encoded for Discord"; gifski fit candidates walk the whole ladder — next entry). The old `-U -O2` / plain `-U` rungs are gone (plain `-U` kept the bad frames).
  Cached results are re-rendered (`PipelineVersion` 2026-09-19.2, `RulesVersion` 2026-09-19.1);
  DESIGN.md §5.2 item 9 and §5.3. **Verified on Discord:** the six-variant matrix, and the
  pipeline's output for the reported recipe was byte-identical to its variant 2 (58,640 B, ok)
  at `PipelineVersion` .2 (the .4 output differs by one restored pixel — see the entries below).
  The repaired / coalesced forms and other inputs have not been uploaded. Known limits
  (DESIGN.md §11): in the optimize preset and fit candidates the repair applies `--lossy` a
  second time; a GIF too large to analyse (16 M canvas pixels / 2³¹ decoded pixels), a frame
  outside the logical screen or a reserved disposal value ends the analysis and the check passes
  with a "not analysed" note.
- **gifski + fit-to-size finds a result again (2026-09-19)** — with the gifski encoder and a
  byte budget a search with loop forever ended in "no candidate passes the Discord rules", for
  every target:
  gifski writes a local colour table on each frame, which the linter refuses (frame 0 cannot
  get its transparency flag; Discord tiers also need one global palette), and only the
  single-output render ran the re-encode ladder that fixes it. Fit candidates of the gifski
  encoder now walk the same ladder (`gifsicle --colors N`, then the hold repair), each with its
  own scratch files since candidates encode concurrently. A finite loop count hid the bug on
  clips of ≤ 256 colours (its gifsicle pass merged the palettes by accident) and, for target
  none, turned it into a needlessly down-scaled result on richer clips. Note that whenever
  gifsicle is present the delivered gifski file is therefore re-quantised to one global palette
  by the lint ladder (single renders always were). Cached results are re-rendered
  (`PipelineVersion` 2026-09-19.3); DESIGN.md §4.2 (gifski row), §5.3.
- **Transparent GIFs: two ffmpeg encoder defects worked around (2026-09-19)** — ffmpeg's GIF
  encoder optimises frames in two ways that are wrong once frames have transparency. (1) It crops
  such a frame to its opaque bounding box, but the column scan skips the box's bottom row:
  whatever part of that row sticks out past the rows above was cut off and came out transparent
  (one pixel on 11 frames of the clip the held-frames fix was reported with; a whole ground line
  or shadow wider than the body on pixel art). (2) It decides per frame how to store it: a
  **fully opaque** frame inside a transparent clip — a keyed subject that fills the frame for a
  while, an alpha fade — was diffed against the previous frame and kept on the canvas, which left
  holes in it and then showed it *under* the transparent frames after it. Alpha GIFs are now
  encoded with `-gifflags -offsetting` (full-canvas frames: no crop, nothing cut off); when
  ffmpeg's output shows the mix — it marks the two kinds of frame with different disposals; an
  entirely transparent lead-in before opaque frames is exact already and is left alone — the
  clip is encoded a second time as complete frames (`-gifflags -offsetting-transdiff`) and every
  frame gets disposal 2 (`discordlint.DisposeCompleteFrames`), before the hold merge and
  gifsicle: both cases are exact, and clips without the mix keep ffmpeg's diffing (and their
  size). gifsicle re-crops the frames, so the final file grows by well under 1 KB (58,665 B vs
  58,640 B on the reported clip; without gifsicle the delivered full-canvas file is larger,
  noticeably so for a small sprite on a large canvas). Opaque GIFs
  were never affected (different, correct code path — verified) and keep their bytes.
  Real-ffmpeg regression tests reproduce both upstream bugs and pin the workaround. Cached
  results are re-rendered (`PipelineVersion` 2026-09-19.4); DESIGN.md §4.2 (GIF row), §5.2
  item 8. A mixed clip costs a second palette pass (once per size/rate in a fit search). Known
  limits, older than this fix: the *colours* of a picture that appears after the first frame and
  then holds still to the end can be off (ffmpeg's `stats_mode=diff` palette never counts them).
  Not yet uploaded to Discord in this form: the reported clip's pre-gifsicle file is
  byte-identical to the verified variant 6, the final file has variant 2's structure.
- **The held-frames repair no longer loses transparency (2026-09-20)** — repairing a GIF whose
  *first* frame is fully opaque while later frames are transparent (through the lossless fast
  path or the Optimize preset) painted the later frames' background solid, and the file was
  still reported as fine. gifsicle's coalesce decides from the first frame whether the canvas is
  transparent at all. The repair now hands gifsicle a tiny transparent lead-in frame (dropped
  again with `#1-`), which makes the coalesce exact, and the coalesced file — plus the
  re-optimised one when nothing lossy ran — is played and compared with the input (a file that
  cannot be played, or is too large to, is repaired unchecked): a repair that changed the picture — gifsicle also silently gives up on local
  colour tables or more than 256 colours per picture, yet still rewrites the disposals — is
  refused, and the file is delivered as it came with the failing check visible. Cached results
  are re-rendered (`PipelineVersion` 2026-09-20.1); DESIGN.md §5.3 rung 2.
- **The scrubber keeps its size while scrubbing (2026-09-19)** — the frame readout beside the
  preview slider gained a character whenever the frame number gained a digit (frame 10, 100, …).
  In the wrapping scrub row that shrank the slider a little each time and, on a column about as
  wide as the row (a 1080 px portrait monitor), pushed the readout onto its own line, so the
  slider jumped to the full row width in the later part of a clip and back again. The readout
  now reserves the width of its widest text (it is monospaced, so that is exact): slider and
  readout are the same size on every frame.

## Phase 5a

Background removal, classical fixes — built 2026-10-08 from the user-approved
[`background-removal-proposal.md`](background-removal-proposal.md) (its research notes:
[`reviews/background-removal-research-2026-10-08.md`](reviews/background-removal-research-2026-10-08.md)).
The AI matte of that proposal (an ONNX sidecar, a `matte` op) is [Phase 5b](#phase-5b) below —
opt-in, off by default, the operator picks the model and where it runs.

- **The chroma key keys the exact screen colour** — ffmpeg's `chromakey` converts the key colour
  with full-range coefficients but compares it with limited-range chroma, so the exact green sat
  0.047 away from its own key: nothing below that similarity keyed, and the old default 0.2 keyed
  the subject on every non-green background. The compiler now pins the keying format to BT.601
  limited range (`format=yuva444p:color_spaces=bt470bg:color_ranges=tv`) and passes the key as
  limited-range YUV (`yuv=1`), so tagged yuv video (a bt709 screen capture) and RGB sources alike
  key the exact colour at a similarity of 0.02 — the pin is what makes a tagged source key: a
  conversion through RGB alone keeps the source's own matrix (measured U 42 / V 27 for bt709
  green, 0.047 off). DESIGN.md §4.3.
- **New defaults** — chroma key similarity 0.1 (was 0.2), colour key similarity 0.08 (was 0.1);
  both the server and the card moved together. Cached results of keyed recipes are re-rendered
  (`PipelineVersion` 2026-10-08.1; the still / Play previews and the auto-crop memo were bumped
  too). One thing to know about the Screen default: the chroma key judges each pixel by its
  3×3 neighbourhood, so at 0.1 a one-pixel rim of screen colour stays opaque (despilled to
  grey) along the hard edges of a strongly coloured subject — flat line art on a green screen
  shows it; a similarity around 0.15 keys that rim softly, and the old 0.2 keyed it outright.
- **Background card: None · Colour · Screen** — *Colour* (the first classical mode: an RGB
  distance with a hard edge, measured to beat the chroma key everywhere except single-hue ramps)
  is the eyedropper pick plus **"+ add colour"** rows (up to 6, each armed through the same
  eyedropper or typed as hex) for 2-colour ramps and gradients, one Similarity / Blend pair for
  all of them — sent as one `colorkey` op per colour, each removing its own colour. *Screen* is
  the green / blue choice with an editable key colour, Similarity 0.1 and the Advanced despill
  fold as before. Summary lines: "2 colours · similarity 0.08", "greenscreen · similarity 0.10 ·
  fill pinholes". In batch the Colour mode takes typed hex colours (there is no preview to pick
  from); Screen as in single mode.
- **Edge cleanup fold** (closed by default) — **Fill pinholes** (on for new sessions; a 3×3
  close of the alpha: fills holes of up to 1 px without growing the silhouette, never hurt in
  any measurement) and **Grow matte — N source px** (0–4 extra dilations: +1–2 recovers eaten
  interiors at a 2 px fringe; in source pixels, like Feather, so 4 px on a 720 px source is
  under 1 px after the emote fit). Sent as the new `morph` op after the keys and before
  `feather`, so the feather softens the cleaned edge; auto-crop detects on the cleaned matte.
  Soft edges stay the Feather card's job. Shown only when the server reports `features.morph`.
- **Crop-mode still on the right frame grid** — the still the crop rectangle is drawn on now
  sends the preset's fps like the normal still, so both compile to the same frame (a 25 fps
  preset over a 30 / 60 fps source used to crop on the source's grid).
- API: `morph` (`close`, `grow`), the new defaults and the stacked `colorkey` ops in
  [`USAGE.md`](USAGE.md#http-api).

## Phase 5b

AI background removal — built 2026-10-08 from the same proposal (its §§4–9, 11–13), **off by
default**: a plain `docker compose up -d` is the app exactly as before, and the AI mode appears
only when the matte sidecar answers.

- **Background card: None · AI · Colour · Screen** — *AI* sends the new `matte` op. The frames the
  model sees are the clip after trim / speed / fps only, so every crop, resize, key, feather and
  edge-cleanup change re-uses the mattes already computed; on frames that already carry alpha
  (a ProRes 4444 export, an earlier key) the matte is multiplied in, never substituted. The
  **Model** select lists exactly what the sidecar offers (`GET /api/matte`): `isnet-anime`
  "Anime (fast)" and `birefnet-lite` "General (precise)" are shipped; the operator chooses which
  are offered and which is preselected, the user picks per recipe; a model that is loading or
  downloading says so, one the sidecar cannot run is greyed with its reason. A status line says
  "ready · GPU · up to ~1 s for this clip", "CPU", "loading model…", "downloading weights 43 %"
  or "sidecar unavailable — run `docker compose --profile matte-gpu up -d`" (Phase 5c: "loaded"
  in place of "ready" while a session is resident, and "· not loaded (the first Compute adds the
  model load)" when the sidecar says it is not — "ready" alone means downloaded and self-tested).
  The Edge cleanup fold applies to the AI matte too.
- **The matte sidecar** — a second container behind a compose profile, pull-only images:
  `docker compose --profile matte up -d` (CPU) or `--profile matte-gpu up -d` (CUDA; needs
  nvidia-container-toolkit on bare-metal Linux / inside a WSL distro and a host driver ≥ 580,
  nothing extra under Docker Desktop), or `COMPOSE_PROFILES=matte-gpu` in `.env` once. Start one,
  never both. The first start downloads the weights into the `ezlg-models` volume and self-tests
  them (the UI shows the state meanwhile); `docker compose run --rm matte download` pre-fetches
  every model for an air-gapped box. No published port, no auth: reachable on the compose
  network only. The device and the models are the operator's: `MATTE_DEVICE`, `MATTE_MODELS`,
  `MATTE_DEFAULT_MODEL`, the preload list and the idle-unload TTL — the table in
  [`USAGE.md`](USAGE.md#ai-background-removal-optional-matte-sidecar).
- **Previews while a matte is computed** — the still and Play answer **202** with the pass's
  progress instead of blocking: the picture on stage stays, a pill shows "AI matte 24/45 · GPU",
  "AI matte: loading model… (12 s)" or "downloading weights 43 %", and the preview re-requests
  itself every half second until the matte is on disk. A still over a long clip on CPU (an
  estimated 90 s or more) defers the pass and offers **Compute now**; Play and Render always
  start it (as built in 5b — [Phase 5c](#phase-5c) replaced this with the Compute matte button:
  no preview starts a pass any more). Renders run the pass as a pre-stage with its own SSE
  progress line, outside the render slots, so queued AI renders never hold up plain ones.
- **Estimate line under Render** — appends `· up to ~N s AI matte (GPU|CPU)` from the sidecar's
  measured speed for the model picked (frames × (ms per frame + 2 ms), the server's own
  figure — "up to", since frames already computed cost nothing); over the server's caps
  (`EZLG_MATTE_MAX_SECONDS` 600 / `EZLG_MATTE_MAX_FRAMES` 3000) an error note names the bound
  and the remedy (trim, lower the fps, pick the fast model) before Render is pressed.
- **Mattes are cached on `/data`** — per frame (keyed by the pixels the model saw, so a held
  pose, an fps-upsampled duplicate or a re-trim of the same clip is free) and per clip; a sidecar
  that is restarted, stopped or still downloading never blacks out previews of recipes whose
  mattes exist. The weights' identity is part of every result's hash, so pulling a sidecar image
  with new weights re-renders results under a new URL rather than mixing old results with new
  previews; the result card's **`render.matte`** info check says which model and weights made
  the file.
- **Failure modes** — the sidecar down mid-pass fails the job with "sidecar unreachable — is
  the matte profile up?"; the AI mode greys out after three missed probes (about 90 s) and
  returns at the first answer; `MATTE_DEVICE=cuda` on a box without GPU access stays up and
  reports "device unavailable" with the reason instead of crash-looping; a `matte` op is refused
  with 400 naming the profile while the feature is off. Never a silent fallback to a colour key.
- API: `matte` (`model`, `size`), `GET /api/matte`, the 202 preview answer and `"eager"`, the
  `EZLG_MATTE_*` variables and the sidecar's own table in [`USAGE.md`](USAGE.md).

## Phase 5c

On-demand AI mattes — built 2026-10-09 from the user's three complaints about 5b ("I don't want
the model loaded at all times", "AI runs the moment I select it — it should use a button", "the
models flicker and drop parts I want kept") and the follow-up measurements in
[`reviews/background-removal-stabilise-and-guided-2026-10-09.md`](reviews/background-removal-stabilise-and-guided-2026-10-09.md)
(the frame pairing is exact; BiRefNet-lite is stable on a moving subject, IoU 0.96–0.99 every
frame; isnet-anime detects nothing on non-anime content; a 3-frame temporal median removes every
single-frame pop; SAM 2.1 with a box prompt tracks the user's character at IoU 0.995 and drops
the stream-UI buttons both per-frame models bleed into).

- **Nothing stays loaded.** The sidecar downloads and self-tests every offered model on each
  offered device at start — so the estimate line is honest from the first click — and releases
  the sessions at once (`MATTE_PRELOAD` now defaults to nothing resident); a pass loads what it
  needs, the sidecar drops it after 300 s idle (`MATTE_MODEL_TTL`, was 600), and the app asks it
  to drop everything the moment the Background card leaves the AI mode or is disabled
  (`POST /api/matte/unload`). One sidecar process offers **both devices** when the CUDA EP exists
  (the cuda image carries the CPU provider too): the AI section gains **Run on: GPU / CPU**,
  shown only when `GET /api/matte` lists both, stored server-side (`PUT /api/matte/settings`,
  `/data/mattes/settings.json`; `EZLG_MATTE_DEVICE` seeds it) and never written into a recipe —
  the cache key of a matte is its weights, size and precision, so a CPU matte serves a later GPU
  pass of the same graph. "None" is simply the Background card's default mode.
- **A button starts the pass.** Selecting AI, a model, a device, Stabilise or a Keep colour never
  computes anything: the preview keeps the last picture under an "AI matte not computed —
  Compute" pill (`POST /api/still` without `eager` answers 202 `idle` at once and starts no
  pass; the 5b "deferred" state and its 90 s bound are gone), and **Compute matte** — enabled
  while the matte is idle or stale for the current model + device + trim + fps, "computing…
  24/45" while it runs, "computed" when the memo exists — or Render (always computes) starts it.
  Play behaves like a still. The last picture never flashes to an un-matted frame meanwhile.
- **General (precise) is the GPU default**, Anime (fast) the CPU default (`MATTE_DEFAULT_MODEL_CUDA`
  / `_CPU`; `defaultModels` per device in `GET /api/matte`), and the help text says which to use
  when: "General (precise) keeps thin strands and props and is stable on video; Anime (fast) is
  for anime-style characters only. If parts of the subject drop out, add a Keep colour or raise
  Grow; if edges flicker, set Stabilise." Labels: `isnet-anime` "Anime (fast)", `birefnet-lite`
  "General (precise)", `sam2-tiny` "Guided (click to select)".
- **Stabilise** (Off / **Light** / Strong; Light is the default) post-processes the matte
  *sequence* with a temporal filter — Light: a centred 3-frame median
  (`tpad=start=1:stop=1:start_mode=clone:stop_mode=clone,tmedian=radius=1`; exactly N frames
  out, zero lag, every single-frame pop in either direction removed: isnet-anime's worst frame
  IoU 0.935 → 0.986, BiRefNet-lite unchanged within 0.001); Strong: the median then a decay-0.7
  hold (`…,lagfun=decay=0.7`; "keeps parts that drop out for a frame at the cost of a short trail
  on fast motion": lost-subject pixels −15 %, a half-frame trail). Derived once per clip in
  ~0.1 s per 45 frames after the pass, cached next to the mattes under `<clip>/stab-<mode>/`, read
  by stills, Play and renders alike, so previews match renders; changing it needs no new pass. The
  rejected candidates (median 5, tmix, hold-only, temporal max, hysteresis, model union, alpha
  gain) and why are in the report; the existing alpha-threshold slider stays the "keep more" knob.
- **Keep colours** — up to six eyedropper picks (the same rows as the Colour mode) under the AI
  section's Advanced fold with one Keep similarity (default 0.08): every pixel of those colours is
  forced opaque, a union with the matte (`alpha = max(matte, colour match)`), for props, outlines
  or skin the model drops; in-graph, so the picture changes at once. `keep` / `keepSimilarity` on
  the `matte` op.
- **Guided (click to select)** — the SAM 2.1 hiera-tiny tracker (`sam2-tiny`, Apache-2.0 code and
  weights, 156 MB, downloaded like the others) as a model in the same select, listed last. Picking
  it opens a **Select subject** panel: the preview becomes the source frame of the current scrubber
  position and a prompt canvas — drag a box around the character (the lead; one click alone is
  unreliable: it selects a part, or floods the frame from just outside the subject), click for a
  + point, Shift-click / right-click for a − point, the frame's mask is overlaid at 50 % green
  after every change (`POST /api/matte/prompt`, < 100 ms on a GPU), a keyframe strip lists the
  prompted frames (jump / delete), "Clear" resets. **Compute matte** then sends the clip at a
  tracking size (long side ≤ 1024 px) with the prompts (`POST /v1/track`): every prompted frame
  conditions the tracker, which propagates backward to frame 0 and forward to the end (~30 ms/frame
  + ~30 ms/frame frame prep at 720², ~1.2 GB VRAM per 45 frames on an RTX 5080; corrections on
  later frames stick). SAM 2's edges are coarse (boundary MAE 0.046 vs BiRefNet-lite's 0.017), so
  the final matte **gates** the per-frame model of the **Edge** select — "General (precise)" by
  default on the GPU, "Anime (fast)" on the CPU, or "None — tracker mask only" — with the tracker's
  mask: alpha 255 inside erode(mask, 3 px), the per-frame matte inside the band, 0 outside
  dilate(mask, 3 px) (3 px at the tracking resolution), which matched BiRefNet-lite's edges on the
  corpus with the UI removed and the flicker gone. Help: "Draw a box around the character (or
  click it 2–3 times), add a − click on anything that stays; then Compute. Click on another frame
  where it drifts and Compute again." The prompts live in the recipe (`prompts` on the `matte`
  op, coordinates in 0..1 of the source frame, `frame` = the output frame index), and the tracker
  matte is a clip-level memo keyed by them (plus the tracker weights and the tracking size), so a
  guided result is reproducible and cached. The result card's `render.matte` line names
  "guided (sam2-tiny) + edge <model>", the device, stabilise and the keep count.
- **Images:** the CUDA sidecar carries PyTorch (cu130) next to onnxruntime, sharing the NVIDIA
  wheels; triton and cuSOLVER are removed after the install (the other CUDA libraries stay —
  libtorch links them at load time). Measured: 4.9 GB installed, 10.7 GB in `docker images` on
  Docker Desktop (which counts the compressed blobs as well), against the 5b image's 2.6 GB of
  `/usr` / 7.5 GB — the image grew by the PyTorch runtime; the CPU image grows by about 1 GB
  (1.2 GB installed) and runs the tracker in fp32 (slow; a CPU tier with EdgeTAM is a candidate
  for later). SAM 3 / 3.1 are not shipped (gated, non-OSI
  licence). Batch mode offers the same AI controls minus Compute (rows render).
- API: `stabilise`, `keep`, `keepSimilarity`, `prompts`, `edge` on the `matte` op; the `idle`
  202 state; `GET /api/matte`'s `devices`, `defaultModels`, `models.<id>.kind` and
  `models.<id>.devices`; `PUT /api/matte/settings`, `POST /api/matte/unload`,
  `POST /api/matte/prompt`; `EZLG_MATTE_DEVICE`, the sidecar's `MATTE_DEFAULT_MODEL_CUDA` /
  `_CPU`, `MATTE_MAX_TRACK_FRAMES` and the new `MATTE_PRELOAD` / `MATTE_MODEL_TTL` defaults —
  all in [`USAGE.md`](USAGE.md). `jobs.PipelineVersion` 2026-10-09.1: results of matte recipes
  are re-rendered once (the stabilise default and the keep union change what they render to);
  the matte memo key is untouched, so no pass is repeated.

## Phase 5c follow-up (2026-10-09): mask-prompted tracking and the loose-box guard

Measured on the live stack the same day: with a box slightly larger than the character the guided
matte is right (centre alpha 255, corners 0), but with a loose box covering most of the frame
SAM 2 selects the gradient inside the box instead of the character (centre alpha 0, IoU 0.004
against the per-frame matte) — and the prototype's best prompt was the per-frame model's matte of
one good frame (IoU 0.997, no drawing at all). So:

- **Use this frame's matte** in the Select subject panel, enabled unless the server has said the
  Edge model's matte of this clip is not computed (the panel's overlay asks at once and says so;
  before any answer the button's hint hedges) — the simplest flow: run General with Compute,
  scrub to a frame where it got the character right, switch to Guided, press the button, Compute. It adds the prompt
  `{frame, maskFrom: "edge"}` for the current frame (replacing a box / points on it), shows the
  tracker's mask for that frame from `POST /api/matte/prompt`, lists it in the prompted-frames
  strip as "f N ▣"; − clicks on the frame refine it. Help: "Scrub to a frame where General got it
  right and press Use this frame's matte — the most reliable start; refine with − clicks." The
  mask never travels with the recipe or from the browser: the server takes frame N's matte from
  the Edge model's own memo, scales it to the tracking size and appends it to the track request
  as one record, and the digest of the mask actually sent enters the matte's cache key (a new
  edge model or sidecar re-tracks). With Edge "None — tracker mask only" the button is disabled
  and such a recipe is refused (no edge model to take the matte from). Before that matte exists
  the overlay answers 202 `idle` with "compute the General matte first" (the 202's own
  `pendingReason`; the status's `reason` stays the feature-off reason), and while the Edge
  model's pass for the clip is in flight it shows that pass's progress — a mask prompt never
  starts a pass. A mask-prompted request runs the track's up-front refusals (body cap, prompt
  frames, frame / estimate caps) before the Edge pass, so a clip the track refuses never costs a
  General pass first.
- **Loose-box guard:** after a box's live overlay arrives the SPA measures, from the mask PNG,
  its coverage of the frame and how many frame edges it touches; over 60 % or two or more edges
  → the warning "That looks like the background — tighten the box to the character or add a +
  click on it" under the panel (nothing changes by itself; the heuristic is unit-tested).
- Sidecar: `POST /v1/track` and `POST /v1/track/frame` take `"mask": {"frame": i}` in the prompts
  JSON (the mask frame stays listed in `prompts`, with empty points and a null box when the mask
  is all it has) and ONE record `[uint32 BE length][8-bit gray PNG at w×h]` after the frames
  (≥ 128 = subject): the frame's box / points, if any, refine the mask first and geometrically
  (mask ∩ box; a + / − click adds / removes the smallest SAM candidate region at the click, a
  click no candidate contains is a no-op — SAM 2 cannot refine a mask prompt on its own frame,
  measured; [`sidecar/README.md`](../sidecar/README.md)), then `add_new_mask` conditions the frame
  with the refined mask (the predictor never sees the frame's clicks); 400
  without a record of the declared size or for a frame ≥ N; `/v1/ping` lists
  `"prompts": ["box", "points", "mask"]` for a tracker that supports it.
- API: `maskFrom` (`"" | "edge"`) on a `matte` op's prompt and, with the client flag
  `mask: true` beside it, in `POST /api/matte/prompt` (all in [`USAGE.md`](USAGE.md)); no new env. A recipe, a canonical prompt text or a memo key
  without a mask prompt is byte-identical to 5c's, so the matte memo key is untouched.
