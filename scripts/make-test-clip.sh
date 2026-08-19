#!/usr/bin/env bash
# make-test-clip.sh — synthesise a transparent test clip with ffmpeg only.
#
#   make-test-clip.sh OUT.mov  [seconds=3] [size=320x320] [premultiplied|straight]
#   make-test-clip.sh OUT.webm [seconds]   [size]         [premultiplied|straight]
#   make-test-clip.sh OUT.gif  [seconds]   [size]
#
# Content: testsrc2 colour bars/gradients as RGB, with an alpha channel made of
#   - a soft-edged circle orbiting the centre once per clip (seamless loop),
#   - a hard-edged opaque block (top-left),
#   - a 50 %-alpha block (top-right) — GIF thresholds it, WebP/APNG keep it soft.
# Outside the shapes RGB is NOT black, so a viewer that ignores alpha shows
# colour there (an easy tell), and "premultiplied" mode multiplies RGB by alpha
# before encoding — exactly what DaVinci Resolve does for ProRes 4444
# "Alpha Mode: Premultiplied" exports.
#
# .mov  → Apple ProRes 4444 with alpha (prores_ks, yuva444p10le)
# .webm → VP9 with alpha  (libvpx-vp9, yuva420p, -auto-alt-ref 0)
# .gif  → palettegen/paletteuse GIF with 1-bit transparency, loop forever
#
# Env: FPS (default 30), EZLG_FFMPEG / EZLG_FFPROBE (tool paths).
set -euo pipefail

usage() {
  sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

[ $# -ge 1 ] || usage
out=$1
seconds=${2:-3}
size=${3:-320x320}
mode=${4:-}
fps=${FPS:-30}
ffmpeg=${EZLG_FFMPEG:-ffmpeg}
ffprobe=${EZLG_FFPROBE:-ffprobe}

case "$out" in
  *.mov)  kind=mov ;;
  *.webm) kind=webm ;;
  *.gif)  kind=gif ;;
  *) echo "make-test-clip: OUT must end in .mov, .webm or .gif (got '$out')" >&2; usage ;;
esac
if ! [[ "$seconds" =~ ^[0-9]+([.][0-9]+)?$ ]] || [ "$(awk -v s="$seconds" 'BEGIN{print (s>0)}')" != 1 ]; then
  echo "make-test-clip: seconds must be a positive number (got '$seconds')" >&2; exit 2
fi
if ! [[ "$size" =~ ^[0-9]+x[0-9]+$ ]]; then
  echo "make-test-clip: size must look like 320x320 (got '$size')" >&2; exit 2
fi
if ! [[ "$fps" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "make-test-clip: FPS must be a positive number (got '$fps')" >&2; exit 2
fi
case "$mode" in
  "") if [ "$kind" = mov ]; then mode=premultiplied; else mode=straight; fi ;;
  premultiplied|straight) ;;
  *) echo "make-test-clip: mode must be 'premultiplied' or 'straight' (got '$mode')" >&2; exit 2 ;;
esac
if [ "$kind" = gif ] && [ "$mode" = premultiplied ]; then
  echo "make-test-clip: note: GIF has 1-bit alpha; 'premultiplied' is ignored" >&2
  mode=straight
fi

# --- alpha channel (geq expression; W/H are the plane size, T the frame time) --
# Orbit once per clip so the last frame flows into the first.
cx="(W/2+W/4*sin(2*PI*T/${seconds}))"
cy="(H/2+H/4*cos(2*PI*T/${seconds}))"
radius="(min(W,H)/4)"
slope="(4080/min(W,H))"   # 255 over ~1/16 of the short side → visible soft edge
soft="clip(255-${slope}*(hypot(X-${cx},Y-${cy})-${radius}),0,255)"
hard="255*between(X,W*0.08,W*0.25)*between(Y,H*0.08,H*0.25)"
half="128*between(X,W*0.75,W*0.92)*between(Y,H*0.08,H*0.25)"
alpha="max(max(${soft},${hard}),${half})"

src="testsrc2=size=${size}:rate=${fps}:duration=${seconds},format=rgba"
graph="${src},geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='${alpha}'"
if [ "$mode" = premultiplied ]; then
  graph="${graph},format=gbrap,premultiply=inplace=1"
fi

echo "make-test-clip: ${out} (${kind}, ${seconds}s, ${size}, ${fps} fps, ${mode} alpha)"
case "$kind" in
  mov)
    "$ffmpeg" -hide_banner -loglevel error -nostdin -y \
      -f lavfi -i "$graph" \
      -c:v prores_ks -profile:v 4444 -pix_fmt yuva444p10le -vendor apl0 \
      -movflags +faststart "$out"
    ;;
  webm)
    "$ffmpeg" -hide_banner -loglevel error -nostdin -y \
      -f lavfi -i "$graph" \
      -c:v libvpx-vp9 -pix_fmt yuva420p -auto-alt-ref 0 -crf 20 -b:v 0 \
      -row-mt 1 -deadline good -cpu-used 4 "$out"
    ;;
  gif)
    "$ffmpeg" -hide_banner -loglevel error -nostdin -y \
      -f lavfi -i "$graph" \
      -filter_complex "[0:v]format=rgba,split[a][b];[a]palettegen=reserve_transparent=1:stats_mode=full[p];[b][p]paletteuse=dither=bayer:bayer_scale=3:alpha_threshold=128" \
      -loop 0 -f gif "$out"
    ;;
esac

echo "make-test-clip: wrote $(stat -c %s "$out") bytes"
"$ffprobe" -v error -select_streams v:0 \
  -show_entries stream=codec_name,profile,pix_fmt,width,height,r_frame_rate,nb_frames,duration \
  -of default=nw=1 "$out"
