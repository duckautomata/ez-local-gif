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

Edge-case GIFs (missing GCE, disposal 0/3, missing NETSCAPE, delay 0, comment
extension, local colour table, wrong LSD background index, …) are built in Go
by the tests with image/gif.EncodeAll plus byte surgery (see
`fixtures_test.go`).
