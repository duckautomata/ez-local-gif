# discordlint fixtures

Real encoder output used by the tests (each < 50 KB). Regenerate with a
recent ffmpeg (these came from a 2026-08 master build; FFmpeg 9.x is
equivalent). `SRC` is `testsrc2=size=64x64:rate=10:duration=1`; `ALPHA` adds a
synthetic alpha channel:

    ALPHA="$SRC,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(hypot(X-32,Y-32),20+4*sin(N/2)),255,0)'"

| File | Command |
|---|---|
| `ff_opaque.gif` | `ffmpeg -f lavfi -i $SRC -filter_complex "[0:v]split[a][b];[a]palettegen=max_colors=64:reserve_transparent=0[p];[b][p]paletteuse=dither=bayer:bayer_scale=3" -loop 0 -f gif` — opaque frame 0, per-frame diff transparent index on frames 1..9, LSD bg 31 |
| `ff_transdiff.gif` | same with `palettegen=max_colors=64` (reserve_transparent=1) — opaque frame 0, index 255 diffs, LSD bg 255 |
| `ff_alpha.gif` | `ffmpeg -f lavfi -i $ALPHA -filter_complex "[0:v]split[a][b];[a]palettegen=max_colors=64:reserve_transparent=1[p];[b][p]paletteuse=dither=bayer:bayer_scale=3:alpha_threshold=128" -loop 0 -f gif` — fully compliant transparent GIF |
| `ff_lossy_alpha.webp` | `ffmpeg -f lavfi -i $ALPHA -c:v libwebp_anim -lossless 0 -q:v 60 -compression_level 4 -pix_fmt yuva420p -loop 0 -map_metadata -1 -f webp` |
| `ff_lossless_alpha.webp` | `… -c:v libwebp_anim -lossless 1 -compression_level 4 -pix_fmt bgra -loop 0 -map_metadata -1 -f webp` |
| `ff_opaque_anim.webp` | `ffmpeg -f lavfi -i $SRC -c:v libwebp_anim -lossless 0 -q:v 60 -compression_level 4 -pix_fmt yuv420p -loop 0 -map_metadata -1 -f webp` |
| `ff_loop1.webp` | as `ff_lossy_alpha.webp` with `-loop 1` (ANIM loop count 1 = plays once; `webp.loop-forever` fails for Discord targets, passes with an info note for `TargetNone`) |
| `ff_still.webp` | `ffmpeg -f lavfi -i $SRC -frames:v 1 -c:v libwebp -lossless 0 -q:v 60 -pix_fmt yuv420p -map_metadata -1 -f webp` — simple VP8 still |
| `ff_still_alpha.webp` | `ffmpeg -f lavfi -i $ALPHA -frames:v 1 -c:v libwebp -lossless 1 -pix_fmt bgra -map_metadata -1 -f webp` — simple VP8L still with alpha |
| `ff_rgba.apng` | `ffmpeg -f lavfi -i $ALPHA -c:v apng -pred mixed -pix_fmt rgba -plays 0 -f apng` — RGBA APNG, 10 frames at 1/10 s (fcTL before IDAT, sub-rect diff frames, dispose/blend mixed), `acTL num_plays 0`; fully compliant sticker |
| `ff_plays1.apng` | same with `-plays 1` (`apng.plays-forever` fails for Discord targets, passes with an info note for `TargetNone`) |
| `ff_indexed.apng` | indexed 8-bit-alpha APNG built in the runtime image (DESIGN.md §4.2 rung B): `ffmpeg -f lavfi -i $ALPHA -vf "tile=4x3:color=black@0" -frames:v 1 -c:v png sheet.png` → `pngquant --nofs --speed 3 64 sheet.png` → `ffmpeg -framerate 10 -i sheet_q.png -vf "untile=4x3,setpts=N/(10*TB)" -frames:v 10 -fps_mode passthrough -c:v apng -pix_fmt pal8 -pred mixed -plays 0 -f apng` → `oxipng -o2 --strip safe` — colour type 3, 256-entry PLTE, 1-entry tRNS |
| `ff_still.png` | `ffmpeg -f lavfi -i $ALPHA -frames:v 1 -c:v png -pix_fmt rgba` — plain RGBA PNG (no acTL; ffmpeg's apng muxer writes the same for a single frame) |
| `ff_still_opaque.png` | `ffmpeg -f lavfi -i $SRC -frames:v 1 -c:v png -pix_fmt rgb24` — plain RGB PNG, no alpha |
| `ff_still.jpg` | `ffmpeg -f lavfi -i $SRC -frames:v 1 -c:v mjpeg -q:v 5 -pix_fmt yuvj420p` — baseline JPEG |
| `ff_still_alpha.avif` | `avifenc -s 10 -q 50 --qalpha 90 -y 420 ff_still.png` (libavif in the runtime image) — `avif` brand, ispe 64x64, auxC alpha item |
| `ff_still_opaque.avif` | `ffmpeg -f lavfi -i $SRC -frames:v 1 -c:v libaom-av1 -crf 40 -cpu-used 8 -pix_fmt yuv420p -f avif` — `avif` brand, no alpha |
| `ff_2frame.mp4` | `ffmpeg -f lavfi -i $SRC -frames:v 2 -c:v libx264 -preset veryfast -crf 30 -pix_fmt yuv420p -movflags +faststart -an` — ftyp isom, faststart (moov before mdat), avc1, 64x64, 2 frames / 0.2 s |
| `ff_2frame.webm` | `ffmpeg -f lavfi -i $SRC -frames:v 2 -c:v libvpx-vp9 -crf 40 -b:v 0 -cpu-used 8 -row-mt 1 -pix_fmt yuv420p -an` — EBML DocType webm, V_VP9, 64x64, 2 SimpleBlocks / 0.2 s |

Edge-case GIFs (missing GCE, disposal 0/3, missing NETSCAPE, delay 0, comment
extension, local colour table, wrong LSD background index, …) are built in Go
by the tests with image/gif.EncodeAll plus byte surgery (see
`fixtures_test.go`). The canvas-simulation fixtures for `gif.noop-frame-disposal`
and `MergeGIFHolds` (gifsicle's hold → clear-only frame in its -O2 and -O1
shapes, harmless holds, per-frame transparent indices, local tables, real
interlacing, frames without a GCE, disposal 3, reserved disposals and frames
outside the logical screen (both end the analysis), random animations checked
against an image/gif-based reference compositor) are built the same way by
`encodeCv` in `gifcanvas_test.go`; the Discord-verified GIFs of the 2026-09-19
investigation are not in the repository — `TestNoopFrameDisposalLocalFiles`
checks an explicit list of them (`tmp/discord-stack-test/1-…6-*.gif`,
`tmp/Dragoon.gif`, `tmp/repro/out/base.gif`) with their expected verdicts,
skips each one that is absent, and looks at no other file in those folders.
Edge-case APNGs (zero / short delays, oversized canvas,
frames outside the canvas, acTL after IDAT, missing IEND, hidden default
image, bad sequence numbers, truncation, …) are byte surgery on
`ff_rgba.apng` or synthetic chunk lists with dummy pixel data (see
`fixtures_apng_test.go`); CRCs are recomputed so the variants stay valid PNGs.
Edge-case MP4s/WebMs (wrong brand or DocType, moov after mdat, missing boxes,
odd dimensions, wrong codec, unknown-size Segment, trailing garbage, …) are
synthetic box/EBML streams built in Go (see `fixtures_video_test.go`);
`video_ffmpeg_test.go` additionally re-encodes fresh 2-frame files with the
host ffmpeg when one is on PATH.
