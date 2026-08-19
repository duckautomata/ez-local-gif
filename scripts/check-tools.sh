#!/usr/bin/env bash
# check-tools.sh — print and sanity-check every CLI tool the ezlg image relies on.
#
# Runs at image build time (Dockerfile "tools" stage) so a broken or
# mis-pinned download fails the build, and can be re-run at any time:
#
#   docker run --rm --entrypoint bash ezlg:local /usr/local/share/ezlg/check-tools.sh
#
# Exit status is non-zero if any tool is missing, does not run, or if ffmpeg
# lacks an encoder / filter / demuxer the pipeline depends on.
set -uo pipefail

fail=0
ok()   { printf '  ok   %-14s %s\n' "$1" "$2"; }
bad()  { printf '  FAIL %-14s %s\n' "$1" "$2" >&2; fail=1; }

# first_line NAME CMD... — run CMD, print its first output line, fail if it
# exits non-zero or prints nothing. (Whole output is captured first so a
# chatty tool never dies of SIGPIPE under pipefail.)
first_line() {
  local name=$1; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  out=${out%%$'\n'*}
  if [ "$rc" -ne 0 ] || [ -z "$out" ]; then
    bad "$name" "did not run (exit $rc): $*"
    return
  fi
  ok "$name" "$out"
}

echo "== tool versions"
first_line ffmpeg   ffmpeg -version
first_line ffprobe  ffprobe -version
first_line gifsicle gifsicle --version
first_line gifski   gifski --version
first_line img2webp img2webp -version
first_line webpinfo webpinfo -version
first_line cwebp    cwebp -version
first_line avifenc  avifenc --version
first_line avifdec  avifdec --version
first_line pngquant pngquant --version
first_line oxipng   oxipng --version
first_line tini     tini --version
# apngdis has no version flag; with no arguments it prints its banner and
# exits non-zero, so probe the banner text instead.
apngdis_out=$(apngdis 2>&1 || true)
if printf '%s\n' "$apngdis_out" | grep -qi 'apng'; then
  ok apngdis "$(printf '%s\n' "$apngdis_out" | grep -i 'apng' | head -n 1)"
else
  bad apngdis "did not print its banner"
fi
# anim_dump (libwebp example) has no version flag either.
if command -v anim_dump >/dev/null 2>&1; then
  ok anim_dump "$(command -v anim_dump)"
else
  bad anim_dump "not on PATH (libwebp static tools)"
fi

# ffmpeg major version guard: the pipeline needs the 9.x line (native
# animated-WebP demuxer, libwebp_anim, ProRes 4444 alpha).
if ! ffmpeg -version 2>/dev/null | head -n 1 | grep -qE 'ffmpeg version (n)?9\.'; then
  bad ffmpeg "expected a 9.x build"
fi

echo "== ffmpeg capabilities"
# have_line LIST_FLAG NAME — true if NAME is listed by `ffmpeg LIST_FLAG`
# (second column; demuxer names may be comma-joined, e.g. "mov,mp4,m4a,...").
have_line() {
  ffmpeg -hide_banner "$1" 2>/dev/null \
    | awk -v n="$2" '{ split($2, a, ","); for (i in a) if (a[i] == n) found = 1 } END { exit !found }'
}
for enc in libwebp_anim apng gif png libaom-av1 libsvtav1 libx264 libvpx-vp9 prores_ks; do
  if have_line -encoders "$enc"; then ok "encoder" "$enc"; else bad "encoder" "$enc missing"; fi
done
for dec in prores webp gif apng png vp9 libvpx-vp9 h264; do
  if have_line -decoders "$dec"; then ok "decoder" "$dec"; else bad "decoder" "$dec missing"; fi
done
for flt in palettegen paletteuse chromakey colorkey despill drawtext premultiply unpremultiply \
           alphaextract alphamerge overlay scale pad fps crop tile mpdecimate cropdetect \
           format split lut geq testsrc2 color; do
  if have_line -filters "$flt"; then ok "filter" "$flt"; else bad "filter" "$flt missing"; fi
done
for dmx in webp_anim gif apng mov concat image2 rawvideo; do
  if have_line -demuxers "$dmx"; then ok "demuxer" "$dmx"; else bad "demuxer" "$dmx missing"; fi
done
for mux in gif webp apng mp4 webm rawvideo image2; do
  if have_line -muxers "$mux"; then ok "muxer" "$mux"; else bad "muxer" "$mux missing"; fi
done

echo "== functional smoke test"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# 4 RGBA frames with a hard alpha edge → GIF (palette path), WebP (libwebp_anim),
# APNG, and drawtext through fontconfig (DejaVu). Each must decode again.
src="testsrc2=size=64x64:rate=10:duration=0.4,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(hypot(X-32,Y-32),24),255,0)'"
if ffmpeg -hide_banner -loglevel error -nostdin -y -f lavfi -i "$src" \
     -filter_complex "[0:v]split[a][b];[a]palettegen=reserve_transparent=1[p];[b][p]paletteuse=alpha_threshold=128" \
     -loop 0 "$tmp/t.gif" \
   && ffmpeg -hide_banner -loglevel error -nostdin -y -f lavfi -i "$src" \
     -c:v libwebp_anim -lossless 0 -q:v 80 -compression_level 4 -pix_fmt yuva420p -loop 0 -map_metadata -1 -f webp "$tmp/t.webp" \
   && ffmpeg -hide_banner -loglevel error -nostdin -y -f lavfi -i "$src" \
     -c:v apng -plays 0 -f apng "$tmp/t.png" \
   && ffmpeg -hide_banner -loglevel error -nostdin -y -f lavfi -i "$src" \
     -vf "drawtext=font=DejaVu Sans:text=ezlg:fontsize=20:fontcolor=white:x=4:y=4" -frames:v 1 -c:v png "$tmp/text.png"; then
  ok "encode" "gif / webp / apng / drawtext"
else
  bad "encode" "one of the smoke encodes failed"
fi
for f in t.gif t.webp t.png text.png; do
  if [ -s "$tmp/$f" ] && ffmpeg -hide_banner -v error -nostdin -i "$tmp/$f" -f null - 2>/dev/null; then
    ok "decode" "$f ($(stat -c %s "$tmp/$f") bytes)"
  else
    bad "decode" "$f"
  fi
done
if [ -s "$tmp/t.gif" ] && gifsicle --info "$tmp/t.gif" 2>/dev/null | grep -q 'loop forever'; then
  ok "gifsicle" "loop forever seen"
else
  bad "gifsicle" "--info did not report loop forever"
fi
if [ -s "$tmp/t.webp" ] && webpinfo "$tmp/t.webp" 2>/dev/null | grep -qE 'Animation: 1'; then
  ok "webpinfo" "ANIM flag seen"
else
  bad "webpinfo" "did not report an animated file"
fi
if [ -s "$tmp/t.gif" ] && gifsicle -O2 --careful --loopcount=forever "$tmp/t.gif" -o "$tmp/t2.gif" 2>/dev/null && [ -s "$tmp/t2.gif" ]; then
  ok "gifsicle" "-O2 --careful"
else
  bad "gifsicle" "-O2 --careful failed"
fi
if [ -s "$tmp/text.png" ] && oxipng -o 1 --strip safe -q "$tmp/text.png" 2>/dev/null; then
  ok "oxipng" "optimised text.png"
else
  bad "oxipng" "failed on text.png"
fi
if [ -s "$tmp/text.png" ] && pngquant --force --output "$tmp/q.png" 64 "$tmp/text.png" 2>/dev/null && [ -s "$tmp/q.png" ]; then
  ok "pngquant" "quantised text.png"
else
  bad "pngquant" "failed on text.png"
fi

if [ "$fail" -ne 0 ]; then
  echo "check-tools: FAILED" >&2
  exit 1
fi
echo "check-tools: all good"
