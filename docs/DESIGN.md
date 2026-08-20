# ez-local-gif — Design

Status: Phase 1 accepted by the user 2026-08-19 (Discord render matrix in §9a); Phase 2 (presets,
fit engine, APNG/AVIF/static/frames, sequences, optimiser — §10 item 2) built and reviewed
2026-08-19. Next: Phase 3 (editing ops). This document is the reference for the build;
it condenses a research pass done against current sources (Discord docs/blog, Discord's open-source
image pipeline `discord/lilliput`, FFmpeg/libwebp/libavif/gifsicle/gifski source and docs) plus local
encode benchmarks. Facts marked *(verify)* were not confirmable and must be tested empirically.

## 1. Goals and priorities

- Self-hosted ezgif replacement, running as one Docker container on the LAN Linux box.
- Inputs: GIF, AVIF (static/animated), image sequences, MP4, MKV, MOV (Apple ProRes 4444 with alpha), WebP (static/animated), WebM.
- Outputs: animated GIF / WebP / APNG / AVIF with transparency, PNG frames, MP4/WebM, static images.
- Features: ProRes-alpha → GIF/WebP; extract frames; sequence → animation; crop/resize/compress;
  fit into Discord emote (256 KiB) / sticker (512 KiB) budgets with best quality; background removal
  (chroma key, pick-a-colour); overlay text / static image / looping animated image.
- Hard requirement: outputs must not render broken/black on Discord.
- Ranking: (1) speed end-to-end, (2) simple UI/UX.

## 2. Decision summary

| Area | Decision | Why |
|---|---|---|
| Where processing runs | Server-side, in the container. Browser = UI + native `<img>/<video>` preview + canvas for crop/overlay/eyedropper. | 200 MB ProRes crosses 1 Gbps LAN in ~2 s. ffmpeg.wasm is 12–25× slower than native, capped ~2 GB, multi-thread needs SharedArrayBuffer (HTTPS + COOP/COEP — not available on a plain `http://LAN-IP` page), no AVIF, no CUDA. |
| Backend | Go, one static binary, stdlib-mostly, frontend embedded via `embed`, all pixel work by `os/exec` of CLIs. | Single artifact, trivial goroutine concurrency, `exec.CommandContext` cancellation, tiny runtime layer. Go never touches pixels except the byte-level Discord linter. |
| Frontend | Svelte 5 + Vite (TypeScript), no component library. Built in a Docker stage, embedded in the binary. | Crop rectangle / drag-to-place overlays / eyedropper / reactive op stack need state; Svelte is the least boilerplate for that. (Vanilla ES modules with no build is acceptable if you prefer zero toolchain — the design does not depend on the framework.) |
| Media engine | FFmpeg **9.0.1** (BtbN static gpl build) as the only decoder/filter engine. | 9.0 (Aug 2026) is the first FFmpeg that natively decodes/demuxes **animated WebP**; ProRes 4444 alpha decodes on CPU to `yuva444p10le`; libwebp_anim, palettegen/paletteuse, apng, drawtext, chromakey/colorkey/despill, overlay all in one binary. |
| Encoders / optimizers | `gifsicle` 1.96, libwebp 1.5 tools (`img2webp`, `webpinfo`, `anim_dump`), libavif (`avifenc`/`avifdec`), `pngquant`, `oxipng`, `gifski` 1.34 (opt-in HQ only). | Each covers a corner FFmpeg does not (lossy GIF, indexed 8-bit-alpha APNG, alpha-safe AVIF, verify decoders). |
| Op model | Non-destructive op stack (trim, crop, resize, fps, key, overlays, text) compiled to **one** ffmpeg filtergraph. Preview and export share the compiler. | No generational loss (ezgif re-quantises the GIF at every step). Preview == output. |
| Render model | Per render: decode once → RGBA rawvideo master on tmpfs (`/dev/shm`) → fan out all encoders / fit candidates in parallel → delete. Results memoised on disk by hash(source, ops, output). | Fit-to-size needs many encodes of the same frames; ProRes must be decoded once, not per candidate. Cross-request frame caching is *not* done (premature). |
| Jobs | In-memory job table + SSE progress (`ffmpeg -progress pipe:1`). No DB, no broker, no auth, no TLS (LAN only, documented). | Progress feedback and cancellation for ~150 lines of Go. |
| Discord safety | Encode-by-construction rules **plus** a mandatory Go post-encode linter/fixer (GIF/WebP/APNG) **plus** decode-verify, before a download link appears. | Discord re-encodes every preview through `lilliput`; files that look fine in Chrome still break there. See §5. |
| GPU | Not in v1. Later: opt-in compose `gpu` profile (NVDEC for long/hi-res H.264/HEVC/VP9/AV1 sources, NVENC for MP4 export), and an optional `rembg` sidecar for AI matting. | NVDEC cannot decode ProRes; every GIF/WebP/APNG/AVIF encoder is CPU; CUDA context init (~200 ms) rivals total work on 128–320 px clips. |

### 2.1 Decisions confirmed by the user (2026-08-18)

- Stack: **Svelte + Vite** frontend, **Go** backend, deployed with **docker compose** (one `app` service — the SPA is embedded in the Go binary; an optional `rembg` Python sidecar for AI background removal is added to the same compose later under a `gpu` profile).
- Deployment targets (both have nvidia-container-toolkit, both drivers ≥ 610 → prebuilt FFmpeg 9 NVENC gate is satisfied):
  - **WSL2 on the Windows workstation** (RTX 5080, Windows driver 610.88 / CUDA 13.3). Zero network cost, same machine as DaVinci Resolve, best dev loop. Notes: keep `/data` on the WSL ext4 filesystem (a named volume or a path inside the distro), *not* under `/mnt/c` (9P is slow); an `/input` bind of a Windows export folder is fine for reading a few files; set `.wslconfig` `[wsl2] memory=` high enough that `shm_size 4g` + encoders fit (WSL defaults to 50 % of RAM); WSL's `nvidia-smi` prints its own version (580.x) but the driver version shown is the Windows one. Recent Windows drivers expose NVENC/NVDEC into WSL2 (verify: `ls /usr/lib/wsl/lib | grep -E 'nvcuvid|nvidia-encode'` and the container self-test); CUDA compute (rembg/onnxruntime) works.
  - **Bare-metal Linux box** (RTX 3070, driver 610.57.04 / CUDA 13.3). Always-on option; full NVENC/NVDEC (Ampere: no AV1 *encode*; AV1 decode ok). LAN upload cost ≈ 2 s per 200 MB.
  - Which is faster is decided by **CPU**, not GPU, for v1 (all animated-image encoders are CPU). The compose file is identical on both.
- Source material: **DaVinci Resolve → QuickTime, Apple ProRes 4444, Alpha Mode: Premultiplied.** → The "premultiplied source" toggle defaults **on**; `unpremultiply` runs at native bit depth right after decode (see §4.3). Bit depth is reported by ffprobe (`yuva444p10le` for 4444, `yuva444p12le` for 4444 XQ); all outputs are 8-bit anyway. If Resolve offers "Straight" alpha, prefer it (skips the unpremultiply, which loses precision in low-alpha edge pixels) — not required.

## 3. Architecture

```
Browser (Svelte SPA, embedded)                    Container (Go binary + CLIs)
┌──────────────────────────────┐   XHR multipart   ┌───────────────────────────────────────────┐
│ drop/paste/pick file(s)      │ ────────────────▶ │ /api/upload  → stream to disk, sha256,     │
│ still preview + scrubber     │ ◀──── PNG ─────── │   ffprobe, first still, 10 s low-res proxy │
│ preset chips                 │                   │ /api/still   → memoised (ops-hash, t, w)   │
│ op cards (collapsed)         │   POST /api/jobs  │ /api/jobs    → recipe → job → SSE events   │
│ result card + Discord check  │ ◀──── SSE ─────── │   compile ops → ONE filtergraph            │
│ download / save to /output   │                   │   ffmpeg → frames.rgba on /dev/shm         │
└──────────────────────────────┘                   │   fan-out encoders (semaphore NumCPU/2)    │
                                                   │   lint+fix → verify → /data/results/<hash> │
                                                   │ /out/<hash>/<file>  immutable              │
                                                   └───────────────────────────────────────────┘
Volumes: /data (blobs by sha256, results by recipe hash, TTL/LRU sweeper), /input (ro, optional),
/output (rw, optional), shm_size 4g.
```

Go packages (suggested): `cmd/ezlg`, `internal/graph` (op stack → filtergraph string + output dims;
golden-file tested), `internal/enc` (per-format argv builders), `internal/fit` (ladder + secant search),
`internal/discordlint` (GIF/WebP/APNG parse, fix, report), `internal/jobs` (table, SSE, cancellation),
`internal/store` (blobs, results, sweeper), `internal/probe` (ffprobe + alpha detection).

## 4. Processing model

### 4.1 Universal decode → master frames

```
ffmpeg -hide_banner -nostdin -y -threads 0 [-c:v libvpx-vp9  # only for VP9-alpha WebM]
  [-ss A -to B] -i SRC [overlay inputs...]
  -filter_complex "[0:v][format=gbrap|gbrap10le|gbrap12le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,]  # 'premultiplied source' toggle, hoisted, native depth (§4.3)
     fps=FPS:round=down                                     # a drop never lengthens the clip; Frames = floor(Duration*FPS)
     [,format=yuva444p,chromakey=...,despill=... | format=rgba,colorkey=...]  # background removal (full-res, 4:4:4)
     [,crop=w:h:x:y]
     ,format=gbrap,premultiply=inplace=1,scale=W:H:flags=lanczos,unpremultiply=inplace=1,format=rgba
     [,pad=CW:CH:(ow-iw)/2:(oh-ih)/2:color=0x00000000][,transpose|hflip|vflip][,setpts=PTS/S]
     [overlay/drawtext stages]"
  -f rawvideo -pix_fmt rgba /dev/shm/ezl/<job>/frames.rgba      + meta.json {w,h,fps,n,has_alpha,durations}
```

- Every source type enters through the same `-i` (FFmpeg 9 handles GIF, animated WebP, APNG, MP4/MKV/MOV/WebM). **Image sequences (as built in Phase 2)** use the **image2** demuxer, not concat: the store renumbers frames to `%06d.<ext>` (png/jpeg/webp/bmp/tiff only), the graph emits `-f image2 -framerate 1000/delayMs -start_number 1 -reinit_filter 0` plus a leading `scale=W:H:force_original_aspect_ratio=decrease` guard (fftools otherwise rebuilds the filtergraph on any frame size/format change and the fps stage loses frames — verified), and mixed-size sequences additionally pad onto a transparent W×H canvas (`pad=…:eval=frame`). The `delay` op re-times the whole sequence (uniform delays; per-frame delay lists are a later phase).
- **AVIF ingest (verified on FFmpeg 9.0.1, Phase 2):** ffmpeg's mov demuxer exposes a still AVIF as [colour, alpha(gray)] and an **animated** AVIF (avifenc and ffmpeg's own muxer alike) as **four** video streams — the one-frame primary item (colour, alpha) *first*, then the animation tracks (colour, alpha). `[0:v]` therefore decodes the still item. The probe picks the colour stream with the most frames and records `ProbeInfo.ColorStream`/`AlphaStream` (v:N indices; e.g. 2/3 for an alpha animation, 0/1 for a still); the graph reads `[0:v:C]format=rgba[c];[0:v:A]format=gray[a];[c][a]alphamerge` (or just `[0:v:C]` when opaque). `avifdec --index all` remains available as a fallback (enc.AVIFDecArgs) but is not needed.
- `has_alpha` = ffprobe `pix_fmt`/`alpha_mode` probe **plus** a real min-alpha scan of the master (GIF decoder always reports BGRA; VP9-alpha WebM only shows alpha with `-c:v libvpx-vp9`).
- Lower-fps candidates re-read `frames.rgba` with `fps=`; no re-decode. Reverse = Go reorders frames in the file.
- Sources exceeding a frame cap (default 600 frames at output size, or 2 GB) bypass the master and stream straight into the encoder.
- Duplicate-frame merge: `mpdecimate` **with `-fps_mode vfr`** (otherwise ffmpeg re-duplicates the dropped frames) — still a later-phase item; Phase 2 sequences are CFR at the image2 `-framerate`.
- Time base: GIF delays are whole centiseconds; browsers clamp delays ≤10 ms to 100 ms → minimum 2 cs, so GIF fps is **capped at 50** (other formats at 60) and otherwise left alone: ffmpeg's gif muxer runs at a 1/100 s timebase and rounds each frame's pts, so a 30 fps master gets 3/3/4 cs delays with exact total timing (Bresenham for free) and no frames are dropped or duplicated. (An earlier draft snapped to 100/n rates; that duplicated 1 in 9 frames for 30 fps sources.)

### 4.2 Encoder tails (all read `frames.rgba`)

| Output | Command (defaults) | Notes |
|---|---|---|
| GIF (default, "Discord-safe") | `… -filter_complex "[0:v]split[c][a];[a]alphaextract,lut=c0='gte(val,T)*255'[m];color=c=0x313338:s=WxH:r=FPS,format=rgba[bg];[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];[f][m]alphamerge,split[p1][p2];[p1]palettegen=max_colors=C:reserve_transparent=1:stats_mode=diff[pal];[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle:alpha_threshold=128" -loop 0 -f gif base.gif` then `gifsicle -O2 --lossy=L --careful --loopcount=forever [--colors C --dither=o8]` then lint/fix. | Matte semi-transparent edges onto Discord dark `#313338` (or white/custom) before 1-bit threshold (T=128; "trim fringe" T=180). Ordered dither is 15–20 % smaller than error-diffusion and avoids temporal shimmer; use `sierra2_4a` for photographic content. Single global palette. Never `-O3`. |
| GIF (opt-in "HQ chat") | `ffmpeg … -c:v png -compression_level 1 f%05d.png` → `gifski --fps FPS -W W -H H --quality 90 --repeat 0 --matte 313338 -o out.gif f*.png` → lint. | Best photographic quality, but per-frame local palettes (Discord glitch reports) and gifski's y4m/stdin path drops alpha (PNG round-trip required). Chat attachments only, labelled. |
| Animated WebP | `… -c:v libwebp_anim -lossless 0 -q:v 80 -compression_level 4 -pix_fmt yuva420p -loop 0 -map_metadata -1 -f webp out.webp` (lossless: `-lossless 1 -pix_fmt bgra`). Squeeze fallback: `img2webp -loop 0 -min_size -mixed -lossy -q Q -m 4 -d 1000/FPS f*.png`. | **Never** the legacy `libwebp` encoder for animation (ghost trails, trac #7941). **Always** `-loop 0` (webp muxer default is 1 = play once). Never `-compression_level 6` interactively (30× slower for ~2 %). Alpha is lossless in this path. |
| APNG (stickers) | Rung A: `-c:v apng -pred mixed -plays 0` (RGBA). Rung B (usual winner): `tile=CxR` sprite sheet → `pngquant --nofs 256|128|64` (shared PLTE+tRNS = 8-bit alpha) → untile → `-c:v apng -pix_fmt pal8 -pred mixed -plays 0` (or a small Go apngmux if ffmpeg won't pass pal8 through *(verify)*) → `oxipng -o2 --strip safe`. | **Always** `-plays 0` (apng muxer default is 1 = no loop). Delays ≥ 20 ms, never 0; ≤ 5.0 s; ≤ 1000 frames; ≤ 60 fps; 320×320 recommended (larger is shrunk by Discord — lint warns). |
| Animated AVIF | `avifenc -j all -s 8 -q 60 --qalpha 90 -y 420 --fps FPS --repetition-count infinite f*.png out.avif` (opaque fast path: `ffmpeg -f avif -c:v libsvtav1`). | libaom `-s 8` measured 2.5–6.5 s / 90 frames vs SVT-AV1 0.3–0.5 s → prefer `-c svt` if the libavif build has it *(verify)*. Discord transcodes AVIF→WebP; alpha survival for animated AVIF unverified. |
| MP4 | `color=#313338 [bg];[bg][0:v]overlay,format=yuv420p -c:v libx264 -preset veryfast -crf 20 -movflags +faststart` (gpu profile: `h264_nvenc -preset p4 -cq 23`). | Audio dropped (frames come from the master) — say so in the UI, or map source audio when ops are trim-only. |
| WebM alpha | `-c:v libvpx-vp9 -pix_fmt yuva420p -auto-alt-ref 0 -crf 30 -b:v 0 -row-mt 1 -deadline realtime -cpu-used 5` | `-auto-alt-ref 0` is required for alpha. Label: **plays on a black background in Discord**. |
| PNG frames | `ffmpeg -i SRC -fps_mode passthrough -frame_pts 1 -pix_fmt rgba -c:v png -compression_level 1 %05d.png` (+ `rgba64be` for 10/12-bit ProRes; `-c:v libwebp -lossless 1` / `mjpeg -q:v 2` variants) + `ffprobe … frame=pts_time,duration_time` → `delays.json` → zip (STORE). | Thumbnail grid served straight from the dir. |
| Static (n == 1) | PNG: `pngquant --quality 70-100 --speed 3` + `oxipng -o2 --strip safe`; WebP: `cwebp -q Q -m 4 -alpha_q 100 -metadata none` (or `-size BYTES`); AVIF: `avifenc -s 6 -q Q`; JPEG: mjpeg flattened on matte. | Static emote/sticker and "compress static image" use this path. |
| GIF → GIF (no decode) | Trim/loop/delay/crop/reverse: `gifsicle -U in.gif '#a-b' -d D --loopcount=forever [--crop x,y+WxH] -O2 --careful`. Optimise: `gifsicle -U in.gif [frame selection] -O2 --lossy=L --colors C --dither=o8 --careful` candidates in parallel. | Never `--resize-fit` on an already-quantised transparent GIF (jaggy alpha edges, gifsicle #111) — resize at RGBA level through the master and re-quantise. |

### 4.3 Editing ops

- **Background removal:** `format=yuva444p,chromakey=color=0x00FF00:similarity=0.20:blend=0.05,despill=type=green:mix=0.6:expand=0.3` (blue: `0x0000FF`, `type=blue`); pick-a-colour: `format=rgba,colorkey=color=0xRRGGBB:similarity=S:blend=B`. Eyedropper reads the still-frame PNG in the browser canvas; sliders re-render the still (~100 ms). Applied before scaling at full res. AI matting (rembg/BiRefNet on CUDA) = optional Python sidecar later.
- **Text:** `drawtext=fontfile=/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf:textfile=<job>/t1.txt:fontsize=48:fontcolor=white:borderw=3:bordercolor=black:x=X:y=Y:enable='between(t,A,B)'`. `textfile=` avoids escaping hell; font list from `/usr/share/fonts` + optional `/fonts` mount; `fonts-noto` for CJK/emoji.
- **Static image overlay:** `-i ov.png … "[1:v]format=rgba,scale=OW:OH[,colorchannelmixer=aa=0.6][o];[base][o]overlay=x=X:y=Y:format=auto:eval=init:enable='between(t,A,B)'"`.
- **Looping animated overlay:** prefer `-stream_loop -1 -i ov.gif|webp|apng|mp4|webm` (deterministic); `-ignore_loop 0` obeys the file's own loop count (a NETSCAPE loop=1 file plays once and freezes) — fallback only. `"[1:v]format=rgba,fps=FPS,scale=OW:OH[o];[base][o]overlay=x=X:y=Y:shortest=1:format=auto"` on an rgba main keeps base alpha. Base = still image: `-loop 1 -framerate FPS -t D -i still.png`.
- **Crop to content:** `alphaextract,cropdetect=limit=0:round=2` on the proxy (default suggestion for Emote — emoji render at ~22 px).
- **Alpha:** the user's Resolve exports are **premultiplied** (matted with black), so the "premultiplied source" toggle defaults on for ProRes; the graph inserts `format=gbrap|gbrap10le|gbrap12le,setparams=alpha_mode=premultiplied,unpremultiply=inplace=1` immediately after decode at the decoder's native depth (ProRes 4444 decodes as `yuva444p10le`/`12le`; unpremultiply supports the 10/12-bit gbrap variants) *before* any conversion to 8-bit RGBA, keying, or scaling. Otherwise (WebP/APNG/AVIF need straight alpha) edges get dark fringes on light backgrounds. **The `setparams` tag is required on FFmpeg ≥ 8:** libavfilter now negotiates `AVFrame.alpha_mode` across links; `unpremultiply` demands premultiplied input but ProRes/rawvideo/most decoders tag frames `unspecified`, so without the tag ffmpeg auto-inserts `premultiply_dynamic` in front (`-loglevel verbose`: "auto-inserting filter 'auto_premultiply_dynamic_N'") and the pair cancels out — the toggle silently became a no-op (verified 9.0.1: edge pixel R=64/A=64 stayed R=63; with the tag R=253). Its position relative to the `format=` stage does not matter (the auto-inserted conversion passes the mode through). `internal/graph/alpha_ffmpeg_test.go` checks this at pixel level whenever ffmpeg is on PATH. A heuristic (edge pixels dark over white) pre-selects the toggle for other sources. Scale is wrapped in `format=gbrap,premultiply/…/unpremultiply`, which needs no tag: `premultiply` requires straight input, which is what `unspecified` degrades to (and after the hoisted unpremultiply the frames are tagged straight anyway). GIF = matte-then-threshold; WebP/APNG/AVIF keep 8-bit straight alpha.

## 5. Discord

### 5.1 Limits and what renders (verified Aug 2026)

| Target | Limit | Formats | Notes |
|---|---|---|---|
| Custom emoji | **262,144 B** (256 KiB), 128×128 | JPEG/PNG/GIF/WebP/AVIF; animated = GIF, animated WebP, animated AVIF (since 2025-05-01). **APNG is not an animated emoji format.** | Discord serves all emoji re-encoded as WebP. Displayed at ~22 CSS px inline, 48 px jumbo, ~16 px reactions, ~32 px picker (fetched at 2×). Error 50138 = could not shrink below 262144. |
| Sticker | **524,288 B** (512 KiB), 320×320 recommended (Discord shrinks larger uploads and accepts smaller / non-square — verified 2026-08-19; the API doc's "320 x 320" is a target, not a hard reject), ≤ 5 s, ≤ 1000 frames, ≤ 60 fps | PNG static; **APNG or GIF** animated (GIF sticker upload via the client UI verified 2026-08-19; Lottie verified/partnered only). **WebP is not a sticker format.** | Rendered at 160×160 dp. Zero/odd delays → "frame rate too small or too large". APNG only animates when uploaded under Server Settings › Stickers; **APNG chat attachments show frame 0 only** (verified). Lint: dims > 320 on either side is a **warning** (Discord shrinks), not an error. |
| Attachment | 20 MB free (raised 2026-08-13), 50 MB Nitro Basic, 500 MB Nitro; boosted server L2 50 MB, L3 100 MB | GIF, animated WebP (with alpha), animated AVIF render inline on desktop/web/iOS/Android since Nov 2024 (AVIF is transcoded to WebP; large animated WebPs take 4–6 s to appear because proxy resize is slow). | **No transparent video path**: WebM VP9-alpha / HEVC-alpha MOV play on a black background; MKV is not previewed; ProRes MOV won't decode in the client. |

### 5.2 Why transparent files break on Discord (from `discord/lilliput` source)

Every inline preview (attachments, emoji, stickers) is decoded and re-encoded server-side by lilliput (giflib + libwebp + libavif). Known quirks as of Aug 2026:

1. GIF frame **without a Graphic Control Extension** → transparent index uninitialised (usually palette index 0) → wrong/flickering colours (issue #267, fix PR #276 unmerged).
2. Canvas background alpha is decided **only from frame 0's GCE**: if frame 0 has no transparency flag the canvas is opaque (LSD background colour) and later transparent frames get a solid (often black) background — the classic symptom. The encoder also deletes transparency for a frame whose transparent index equals the LSD background index when the background is opaque.
3. Disposal 0 (unspecified) treated as "do not dispose"; disposal 3 (restore previous) only supported since Apr 2025; disposal/background handling churned May 2025.
4. Missing NETSCAPE2.0 loop block → loop count 1 (plays once) after GIF→WebP transcode.
5. Frame-diff optimised GIFs / per-frame local palettes have produced "random colour per frame" glitches (local-palette corruption fixed Jun 2025; still cited in 2025 reports).
6. WebP: alpha is dropped if the VP8X container **ALPHA flag** is unset while frames carry alpha (PR #296 open); wrong VP8X canvas dims broke uploads; Discord's EXIF stripper used to corrupt animated WebP metadata (fixed) → emit no EXIF/XMP/ICC.
7. First frame is shown as the still whenever animation isn't playing (autoplay off, reduced motion, hover previews) → a blank first frame looks broken.
8. ffmpeg foot-guns: legacy `libwebp` encoder → ghost trails; webp muxer `-loop` default 1; apng muxer `-plays` default 1.

### 5.3 Encoder rules → enforced by `internal/discordlint` (lint → fix → verify)

**GIF**
- GCE on every frame **including frame 0**.
- If any frame is transparent: frame 0's GCE has the transparency flag set (allocate an unused index if needed) and LSD background index = that transparent index. If nothing is transparent, LSD bg index ≠ any frame's transparent index.
- Explicit disposal 1 or 2 on every frame; never 0 or 3.
- NETSCAPE2.0 loop block present; count 0 for every Discord target (Discord plays once otherwise). For non-Discord output (target none) the user's `Output.Loop` (N = play N+1 times) is honoured and only the block's presence is enforced. Delays ≥ 2 cs (guaranteed by the 50 fps cap; the muxer's centisecond rounding gives exact Bresenham timing).
- Single global palette, no local colour tables (enforce via `gifsicle --colors N`; fail lint otherwise). No interlacing; strip comment/plain-text/app extensions.
- 1-bit alpha via matte-then-threshold; first frame representative (warn if empty).
- Fixer scope: add/patch GCE, flag/index, delay, disposal 0→1, LSD bg index, NETSCAPE, strip extensions. **Cannot** fix disposal 3, interlace, full palette with no free slot, local tables → re-encode ladder: `gifsicle -O2 --lossy --careful --colors N` → `-U -O2 --careful` → `-U` (coalesced full frames, disposal 2).

**WebP**
- WebPAnimEncoder paths only (`libwebp_anim`, `img2webp`, `gif2webp`); loop count 0 for Discord targets (target none: `Output.Loop` N → ANIM plays N+1, informational only); no metadata; VP8X present with ALPHA **iff** frames have alpha, ANIM iff n > 1 (a single-frame master is written as a plain still WebP via `-c:v libwebp`); canvas == frame size; every ANMF inside canvas; never re-mux with tools that rewrite VP8X. Frame durations: ≤ 10 ms is a warning (browsers show 100 ms), 11–19 ms an info note, ≥ 20 ms clean. Verify with `webpinfo`/`anim_dump` (libwebp — what Discord uses) and FFmpeg 9.
- Keep Discord WebPs small (Emote 128², chat default ≤ 480 px with a warning above).

**APNG (stickers only)**
- `acTL num_plays = 0` (Discord targets; target none reports the count); first frame is a real fcTL frame; all fcTL rects inside the IHDR canvas (dims > 320 on a side is a sticker *warning* — Discord shrinks); zero delays are a sticker error; delays ≤ 10 ms warn (browsers show 100 ms), 11–19 ms get an info note, ≥ 20 ms clean; ≤ 5.0 s; ≤ 1000 frames; ≤ 60 fps; ≤ 524,288 B. Invalid fcTL dispose/blend ops and tRNS-before-PLTE fail the container check (libpng rejects / drops alpha). UI: "APNG animates only as a server sticker, not as a chat attachment; never for emoji."

**Verify** (Discord targets): decode with two decoders (ffmpeg + Go GIF compositor emulating lilliput's semantics / libwebp `anim_dump` / `apngdis`), check frame count/dims, and a *tolerant* comparison (mean colour error or SSIM on ~8 sampled frames over `#313338` and `#FFFFFF`) — not a strict pixel-mismatch threshold (lossy/1-bit/dithered output always differs). On failure → coalesce fallback and re-fit. Show the lilliput-simulated preview in the result card. Stamp the lint ruleset version into `report.json`.

### 5.4 Presets and fit-to-size

Byte caps are hard, expressed in KiB, with a 1–2 % margin. Fit = parallel ladder from one master + secant search on log(size) over the format's quality knob (GIF: `gifsicle --lossy` 0–200; WebP: `-q` 95–10; APNG: colours 256→64; AVIF: `-q`), 2 probes per rung (mild/harsh), skip rungs whose harsh probe is over budget, early-exit rungs whose mild probe fits, ≤ 5 iterations otherwise. Winner = mildest rung that fits; 2 runner-ups shown as alternatives; report the binding knob ("fit at 20 fps · 128 colours · lossy 40").

Perceptual cost order (cheapest first): autocrop/merge duplicates → ordered dither instead of error diffusion (−15–20 %) → lossy 30–80 (−20–40 %, near-invisible) → fps 30→24→20→15 → colours 256→128→64 → heavy lossy / alpha_q → downscale (never for stickers) → trim duration (ask).

| Preset | Canvas | Budget | Format ladder | Rungs (fps / colours / px / dither) |
|---|---|---|---|---|
| Emote (animated) | 128×128, transparent pad, auto-crop suggested | 262,144 B (aim ≤ 255,000) | GIF default; WebP when soft alpha matters (accepted since May 2025) | (25,256,128,bayer3) → (20,128) → (16.7,128) → (12.5,64,112,none) → (10,32,96) |
| Emote (static) | 128×128 | 262,144 B | PNG (pngquant+oxipng) | trivially fits |
| Sticker (animated) | 320×320 (preset canvas; larger only warns) | 524,288 B (aim ≤ 515,000), ≤ 5 s | **RGBA APNG probes** (single-point, 25→12.5 fps — §9a's "RGBA only at a usable fps") → **indexed 8-bit-alpha APNG** (palettes floored at 64 colours) → GIF (`--lossy` up to 200) | (25,256) → (20,256) → (16.7,256) → (12.5,128) → (10,64); helpers "speed up to fit 5 s" / "skip leading blank frames" are Phase 3+ |
| Sticker (static) | 320×320 | 524,288 B | PNG | — |
| Chat GIF / WebP | user size (≤ 480–800 px) | 20/50/100/500 MB | quality-first (GIF sierra2_4a + `-O2 --lossy 20`; WebP q 85 m4) | only if a target is set |
| Video attachment | as-is | 20/50/100/500 MB | MP4/WebM two-pass `-b:v` | audio note |
| Compress to X KiB | as-is | user | same format as input | never downscale unless allowed |

Measured (Ryzen 7 7800X3D, 90 frames 320×320): palettegen/paletteuse 0.16–0.5 s; libwebp_anim m4 0.5–0.75 s (m6 18–22 s); APNG pal8 0.4–1.6 s, RGBA 0.6–5.3 s; SVT-AV1 0.3–0.5 s; libaom cpu-used 8 2.5–6.5 s; 128 px emote variants 0.1–0.4 s each. Emote 128² GIF: 30 fps/256c 259 KB (cartoon) / 929 KB (photo); 15 fps/128c 121 / 416 KB; WebP 15 fps q60 88 / 118 KB. Sticker 320² APNG pal8 (cartoon): 30 fps 666 KB, 15 fps 332 KB. → A 6–12-candidate fit search finishes in ~3–5 s wall on a 16-thread CPU.

## 6. GPU verdict

- **Cannot help:** ProRes decode (no NVDEC; FFmpeg 8.1+ has a *Vulkan* ProRes hwaccel, not CUDA — CPU decode is already fast and multithreaded), palettegen/paletteuse, gif/apng/libwebp/libaom/libsvtav1 encoders, gifski, gifsicle, avifenc, pngquant, oxipng — all CPU.
- **Can help later:** NVDEC for H.264/HEVC/VP9/AV1 sources ≥ 1080p or ≥ 10 s (`-hwaccel cuda` alone — auto-download to system frames; or `hwdownload,format=nv12` before the first CPU filter; never for alpha sources), NVENC for MP4/WebM(AV1) export, and **AI background removal** (rembg/BiRefNet on onnxruntime-gpu) — the one place the GPU would shine.
- Packaging: the BtbN FFmpeg 9 build already contains nvenc/nvdec/cuda-llvm at zero image cost. `gpu` compose profile: `gpus: all`, `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`; **host driver ≥ 610** for prebuilt 9.0 NVENC (≥ 570 for 8.x). Startup self-test `ffmpeg -f lavfi -i nullsrc -c:v h264_nvenc -frames:v 1 -f null -` greys out GPU options.

## 7. UI / UX (one page)

Open `http://server:8080`. Drop / paste / pick file(s) (many images = a sequence; or pick from `/input`). Within ~1 s: probe badge (format, W×H, fps, frames, duration, alpha yes/no) + still preview with scrubber over checkerboard / Discord dark `#313338` / white. Preset chips: **Emote · Sticker · Chat GIF · Chat WebP · Frames · Custom** — a preset pre-fills the Output card and locks canvas/aspect. Op cards below, collapsed: Trim · Crop (drag rectangle) · Resize/Canvas · Speed/FPS · Background (eyedropper + tolerance/despill) · Overlay (text / image / animated, drag to place, time range) · Output (format, quality or "fit to" bytes, Advanced fold). Every change re-renders the memoised still (~100 ms); Play renders a low-res animated WebP proxy. Render → SSE progress. Result card: size badge green/red vs limit, W×H/frames/fps/duration, "fit at …", Discord-check list, preview on dark/light + 22/48 px emote and 160 px sticker "as seen in chat" thumbnails, Alternatives, Download (drag-out into Discord), Save to `/output`, "Use as source" to chain. Batch: same preset for several files. "Copy PNG" only when `window.isSecureContext` (Async Clipboard API is unavailable on a plain `http://LAN-IP` page; animated formats can't be copied anyway).

## 8. Speed strategy

Ingest: XHR multipart (no tus/chunking), streamed to disk with SHA-256 computed server-side (WebCrypto is unavailable on plain HTTP), deduped by hash; probe + still + proxy pre-warmed the moment the upload lands; `/input` picker skips upload. Processing: one decode + one filter pass per render into the RGBA master on tmpfs; every encoder/candidate/format fans out from it under a `NumCPU/2` semaphore with `-threads 0`; encoder knobs pinned to fast settings (heavy squeezes — `-m 6`, zopfli, `apngopt` — behind an explicit button); GIF→GIF edits never decode; stills served from the master when it exists (Go `image/png`, no ffmpeg spawn); results memoised by recipe hash. Delivery: files written once, served immutable, direct links, zips STORE'd, `/output` mount.

## 9a. Discord render-test results (user-run, 2026-08-19; full table in docs/discord-testkit-results.md)

Variant set from `scripts/discord-testkit.sh`, uploaded to a private server. Outcomes that now drive the encoders:

| Verified | Consequence |
|---|---|
| ffmpeg palette + `gifsicle -O2 --careful`, the coalesced `-U` variant and ffmpeg-only GIFs: transparent, no flicker (attachment) | The Discord-safe GIF path is correct as built. |
| **gifski (per-frame local palettes): not transparent, dark-grey background, ghosting** | gifski is **never** offered for a Discord target; Phase 4's HQ toggle stays chat-only with a warning (the linter's `gif.global-palette` already fails it for Discord targets). |
| libwebp_anim lossy (yuva420p and bgra) and lossless WebP: good, soft alpha survives; bgra ~4 % larger | Keep yuva420p as the lossy default. q80 blurs fine texture — offer lossless / higher q for detailed art. |
| APNG as attachment: frame 0 only (as predicted) | APNG stays sticker-only in the UI. |
| 128² emote GIF: ok (minor dither artefact on a fine grid); 128² emote WebP: good | Emote preset offers WebP prominently ("keeps soft edges — verified on Discord"); GIF stays the universal default. |
| 320² sticker GIF: good; **indexed 8-bit-alpha APNG at 25 fps: good**; RGBA APNG fit only at a low fps | **Indexed APNG is the sticker default rung**, GIF the fallback; RGBA APNG only when it fits at ≥ 12 fps. |
| **Animated AVIF with alpha (avifenc): animated, soft alpha survives as an attachment** | AVIF is a first-class alpha output (attachments); previously *(verify)*. |
| All GIFs show a dark outline in light mode and no smooth alpha | Inherent to 1-bit alpha + dark matte. UI: make the matte choice visible ("Discord dark / light / custom"), add a "trim fringe" toggle (threshold 180), and steer users to WebP/AVIF (attachments, emoji) or APNG (stickers) when soft alpha matters. |

## 9. Verify before/while building (empirical)

1. **Discord render matrix** on a private server — {attachment, emote, sticker} × {desktop, web, iOS, Android} × {dark, light} × {autoplay on/off, reduced motion} for: ffmpeg palette + `gifsicle -O2 --careful`; `gifsicle -U` coalesced; gifski (local palettes); libwebp_anim lossy (yuva420p vs bgra) and lossless; indexed vs RGBA APNG sticker; GIF sticker; animated AVIF with alpha. Check alpha survives, no black bg, no colour flicker, first-frame still, delays preserved through the WebP transcode. Do this in **Phase 1** with a `discord-testkit` command that emits the variant set.
2. Emote: animated WebP emoji animates + keeps alpha today; > 256 KiB animated → auto-shrunk or rejected; stored at 128².
3. Sticker: client uploader accepts `.gif`; APNG passthrough vs re-encode; non-320² rejected vs cropped; fps bounds behind error 170006.
4. lilliput PRs #276 / #296 merge status (keep rules mandatory regardless).
5. FFmpeg 9.0.1 static build: `webp_anim` demuxer/decoder on real files (alpha, durations, `-ignore_loop 0`); mov demuxer alpha stream for animated AVIF; `-hwaccel cuda` auto-download; drawtext with fontconfig; `libwebp_anim` with n == 1.
6. Indexed 8-bit-alpha APNG without a custom muxer (does `-pix_fmt pal8` apng keep PLTE+tRNS and inter-frame diffs?).
7. Real timings on the server CPU for gifsicle/gifski/avifenc(aom vs svt)/pngquant/oxipng; whole fit search ≤ 5 s.
8. Your ProRes 4444 exports: known premultiplied (Resolve setting). Confirm bit depth with `ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,profile,pix_fmt,width,height,r_frame_rate -of default=nw=1 file.mov` and eyeball an edge over white after `unpremultiply` (no dark halo = correct).
9. tmpfs sizing (1080p RGBA = 8.3 MB/frame; Docker default `/dev/shm` is 64 MB → `shm_size: 4g`, fallback `/data/tmp`; on WSL2 also raise `.wslconfig` memory).
10. GPU (only when the gpu profile is wanted): drivers are ≥ 610 on both machines; on WSL2 confirm `libnvcuvid`/`libnvidia-encode` are present under `/usr/lib/wsl/lib` and the in-container `h264_nvenc` self-test passes.

## 10. Build phases

1. **MVP — ProRes → Discord-safe GIF/WebP.** Dockerfile (trixie-slim + BtbN FFmpeg 9.0.1 pinned by hash + gifsicle/webp/libavif-bin/pngquant/apngdis/oxipng/gifski/fonts-dejavu) + compose (`app` service, `shm_size 4g`, `/data` volume, optional `/input`/`/output` binds; a `dev` override that bind-mounts the source and runs `go run` + `vite dev` (proxying `/api`) inside the container so the Windows host never runs a different ffmpeg — on WSL2 this is the day-to-day loop). Go: upload+sha256, ffprobe+alpha probe, still endpoint, jobs+SSE, results. `internal/graph` for trim/crop/resize/pad/fps; master frames on tmpfs; GIF (palette + gifsicle) and WebP (libwebp_anim) tails; `discordlint` GIF+WebP with fixer; `discord-testkit`. Svelte: drop zone, preview/scrubber/backdrops, crop rectangle, Output card, result card with Discord check. Golden-file argv tests + one integration test (synthetic transparent clip → every format → linter). **Acceptance:** ProRes 4444 → GIF and → WebP with transparency render correctly in a private Discord server on desktop and mobile.
2. **Discord presets + optimisation — built 2026-08-19.** Emote/Sticker/Chat presets, fit engine (`internal/fit`: parallel ladder + bracketed secant on log(size) + 2 alternatives + binding-knob text; Emote/Sticker presets fit by default), APNG (RGBA + indexed 8-bit alpha via tile→pngquant→untile, `LintAPNG`), static PNG/JPEG (+pngquant/oxipng, `LintStatic`), GIF→GIF gifsicle-only optimiser (preset `optimize`: lossy/colours/drop-every-Nth with merged delays, optional fit), frame extraction (png/jpeg/webp + STORE zip, grid UI, cap 2000), image sequences (multi-file upload → dir blob, `delay` op → `-framerate`, mixed sizes normalised), AVIF out (avifenc; svt for opaque ≥ 64 px) and in (4-stream layout, `ColorStream`/`AlphaStream`), in-chat thumbnails (22/48/160 px), "edit as source" (`POST /api/sources/from-result`, `?src=` deep link), frame/timecode readout + ←/→ stepping, matte/trim-fringe controls. Results keyed by `jobs.ResultKey` (PipelineVersion 2026-08-19.3, RulesVersion 2026-08-19.3 after the review passes).
3. **Editing ops.** Background removal (chromakey/colorkey/despill + eyedropper), overlays (drawtext via textfile, static image, looping animated with `-stream_loop -1`), speed/reverse/rotate/flip, crop-to-content, premultiplied toggle + matte picker, animated proxy preview, batch, "use as source".
4. **Polish + extras.** MP4/WebM export (+ video fit-to-size), lossless gifsicle fast path, `/input` watcher + `/output` save, gifski HQ toggle, op-registry structure for ezgif parity ops (effects, censor/pixelate, border, sprite sheet, append, bounce, frame editor), `/api/capabilities` + `/healthz`, keyboard shortcuts, re-run the Discord matrix.
5. **Optional.** `gpu` compose profile (NVDEC gating, NVENC exports, self-test); rembg AI-matting sidecar on CUDA; perceptual ranking (dssim/ssimulacra2) of alternatives; libass rich text; project save/load (JSON {source hash, ops, target}).

## 11. Risks

- Discord's lilliput/media proxy keeps changing (open PRs, 2025 disposal churn) → linter rules are data, versioned, re-validated per phase.
- FFmpeg 9.0.1 is weeks old (new webp_anim decoder) → fallbacks documented (`anim_dump`, `avifdec`), pin the tarball hash.
- Frame master sizing for long/hi-res chat exports → frame cap + streaming bypass + `shm_size` guidance.
- Animated AVIF alpha and GIF stickers unverified on Discord → labelled experimental; APNG stays the sticker default.
- Frame-diff GIF optimisation may still glitch through Discord in edge cases → verify step + coalesce fallback (costs bytes on a tight 256 KiB fit).
- Premultiplied vs straight alpha can't be auto-detected reliably → toggle + heuristic + dark/light preview.
- LAN-only assumptions (no auth/TLS) — keep behind the LAN or a reverse proxy.
- Licensing: gifski (AGPL) and gifsicle (GPL) are exec'd, not linked; cgo-linking gifski later would make the binary AGPL-derived.
- drawtext escaping/fonts (use `textfile=`, bundle fonts, add noto for CJK/emoji).

## 12. Rejected alternatives

Client-side WASM (ffmpeg.wasm/wasm-vips/gifski-wasm); Python/FastAPI main backend (kept only for an optional rembg sidecar); Node/Bun + sharp; Redis/Celery/SQLite job queues; ezgif-style per-tool pages with re-encoded intermediates; gifski as default GIF encoder; legacy ffmpeg `libwebp` encoder; CUDA-first pipeline; ImageMagick for animation; libvips via cgo; tus/chunked uploads; client-side hashing; WebSockets (SSE suffices); linuxserver/jrottenberg nvidia base images (700 MB–1.3 GB vs ~500 MB trixie-slim + static builds); APNG for chat/emoji; Lottie stickers; forking existing projects (ConvertX, OmniTools, imgproxy, gifcurry — borrow ideas only).

## 13. Key sources

- Discord: docs.discord.com/developers/resources/emoji, …/resources/sticker, …/topics/opcodes-and-status-codes; discord.com/blog/modern-image-formats-at-discord-supporting-webp-and-avif; support.discord.com File Attachments FAQ (Aug 2026), Server Boosting FAQ, Tips for Sticker Creators FAQ; github.com/discord/lilliput (giflib.cpp; issues #84 #101 #159 #202 #217 #267 #272; PRs #207 #244 #252–#256 #276 #296).
- FFmpeg: Changelog (9.0 "Lei"), libavformat/webpenc.c, apngenc.c, gif.c, webp_anim_dec.c, libavcodec/gif.c, libwebpenc_common.c, proresdec.c, nvenc.c; trac #7941; doc/filters.texi; BtbN/FFmpeg-Builds; nv-codec-headers README.
- Tools: gifski (lib.rs, y4m_source.rs, releases), gifsicle man/NEWS/kornel.ski/lossygif, libwebp docs (cwebp/img2webp/gif2webp) + NEWS, libavif avifenc.c/avifdec.c/CHANGELOG, oxipng CHANGELOG, Pillow release notes, wasm-vips, ffmpeg.wasm performance page, gifski-wasm.
- Browser: Chromium issue 40342620 / Mozilla bug 232822 (GIF delay clamp); web.dev ClipboardItem.supports; Chrome fetch streaming requests.
