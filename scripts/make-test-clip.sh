#!/usr/bin/env bash
# make-test-clip.sh — synthesise a transparent test clip with ffmpeg only.
#
#   make-test-clip.sh OUT.mov  [seconds=3] [size=320x320] [premultiplied|straight]
#   make-test-clip.sh OUT.webm [seconds]   [size]         [premultiplied|straight]
#   make-test-clip.sh OUT.gif  [seconds]   [size]
#   make-test-clip.sh OUT.avif [seconds]   [size]         [premultiplied|straight]
#   make-test-clip.sh seq OUTDIR [frames=12] [size=320x320]
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
# .avif → animated AVIF with 8-bit alpha, repeats forever: avifenc from RGBA
#         PNG frames when avifenc is on PATH (the app's path), otherwise
#         ffmpeg's avif muxer with a colour + alpha (gray) libaom pair
# seq   → OUTDIR/f00001.png … f0000N.png, straight-alpha RGBA PNG frames of the
#         same animation (N frames at FPS, orbit closes over the N frames) —
#         an image-sequence upload for the integration test
#
# Env: FPS (default 30), EZLG_FFMPEG / EZLG_FFPROBE / EZLG_AVIFENC (tool paths).
set -euo pipefail

usage() {
  sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

[ $# -ge 1 ] || usage
fps=${FPS:-30}
ffmpeg=${EZLG_FFMPEG:-ffmpeg}
ffprobe=${EZLG_FFPROBE:-ffprobe}
avifenc=${EZLG_AVIFENC:-avifenc}

is_num()  { [[ "$1" =~ ^[0-9]+([.][0-9]+)?$ ]] && [ "$(awk -v s="$1" 'BEGIN{print (s>0)}')" = 1 ]; }
is_size() { [[ "$1" =~ ^[0-9]+x[0-9]+$ ]]; }
is_num "$fps" || { echo "make-test-clip: FPS must be a positive number (got '$fps')" >&2; exit 2; }

# --- mode / arguments ---------------------------------------------------------
if [ "$1" = seq ]; then
  kind=seq
  [ $# -ge 2 ] || usage
  out=$2
  frames=${3:-12}
  size=${4:-320x320}
  mode=straight
  [[ "$frames" =~ ^[0-9]+$ ]] && [ "$frames" -ge 1 ] || { echo "make-test-clip: frames must be a positive integer (got '$frames')" >&2; exit 2; }
  # The orbit closes over the whole sequence, like the clips do over 'seconds';
  # the generator runs half a frame longer and -frames:v cuts it exactly.
  seconds=$(awk -v n="$frames" -v f="$fps" 'BEGIN { printf "%.6f", n / f }')
  duration=$(awk -v n="$frames" -v f="$fps" 'BEGIN { printf "%.6f", (n + 0.5) / f }')
  frame_cap=(-frames:v "$frames")
else
  out=$1
  seconds=${2:-3}
  size=${3:-320x320}
  mode=${4:-}
  case "$out" in
    *.mov)  kind=mov ;;
    *.webm) kind=webm ;;
    *.gif)  kind=gif ;;
    *.avif) kind=avif ;;
    *) echo "make-test-clip: OUT must end in .mov, .webm, .gif or .avif, or use 'seq OUTDIR' (got '$out')" >&2; usage ;;
  esac
  is_num "$seconds" || { echo "make-test-clip: seconds must be a positive number (got '$seconds')" >&2; exit 2; }
  case "$mode" in
    "") if [ "$kind" = mov ]; then mode=premultiplied; else mode=straight; fi ;;
    premultiplied|straight) ;;
    *) echo "make-test-clip: mode must be 'premultiplied' or 'straight' (got '$mode')" >&2; exit 2 ;;
  esac
  if [ "$kind" = gif ] && [ "$mode" = premultiplied ]; then
    echo "make-test-clip: note: GIF has 1-bit alpha; 'premultiplied' is ignored" >&2
    mode=straight
  fi
  duration=$seconds
  frame_cap=()
fi
is_size "$size" || { echo "make-test-clip: size must look like 320x320 (got '$size')" >&2; exit 2; }

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

src="testsrc2=size=${size}:rate=${fps}:duration=${duration},format=rgba"
graph="${src},geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='${alpha}'"
if [ "$mode" = premultiplied ]; then
  graph="${graph},format=gbrap,premultiply=inplace=1"
fi

ff() { "$ffmpeg" -hide_banner -loglevel error -nostdin -y "$@"; }

# png_frames DIR → DIR/f00001.png … (straight/premultiplied as per $graph)
png_frames() {
  mkdir -p "$1"
  ff -f lavfi -i "$graph" "${frame_cap[@]+"${frame_cap[@]}"}" -c:v png -compression_level 1 -pix_fmt rgba "$1/f%05d.png"
}

# --- produce -------------------------------------------------------------------
case "$kind" in
  seq)
    echo "make-test-clip: ${out}/f00001.png … (${frames} PNG frames, ${size}, ${fps} fps, ${mode} alpha)"
    rm -f "$out"/f[0-9][0-9][0-9][0-9][0-9].png 2>/dev/null || true
    png_frames "$out"
    count=$(find "$out" -maxdepth 1 -name 'f[0-9][0-9][0-9][0-9][0-9].png' | wc -l | tr -d ' ')
    [ "$count" = "$frames" ] || { echo "make-test-clip: wrote $count frames, wanted $frames" >&2; exit 1; }
    echo "make-test-clip: wrote $count frames ($(du -sk "$out" | cut -f1) KiB)"
    "$ffprobe" -v error -select_streams v:0 \
      -show_entries stream=codec_name,pix_fmt,width,height -of default=nw=1 "$out/f00001.png"
    exit 0
    ;;
  mov)
    echo "make-test-clip: ${out} (${kind}, ${seconds}s, ${size}, ${fps} fps, ${mode} alpha)"
    ff -f lavfi -i "$graph" \
      -c:v prores_ks -profile:v 4444 -pix_fmt yuva444p10le -vendor apl0 \
      -movflags +faststart "$out"
    ;;
  webm)
    echo "make-test-clip: ${out} (${kind}, ${seconds}s, ${size}, ${fps} fps, ${mode} alpha)"
    ff -f lavfi -i "$graph" \
      -c:v libvpx-vp9 -pix_fmt yuva420p -auto-alt-ref 0 -crf 20 -b:v 0 \
      -row-mt 1 -deadline good -cpu-used 4 "$out"
    ;;
  gif)
    echo "make-test-clip: ${out} (${kind}, ${seconds}s, ${size}, ${fps} fps, ${mode} alpha)"
    ff -f lavfi -i "$graph" \
      -filter_complex "[0:v]format=rgba,split[a][b];[a]palettegen=reserve_transparent=1:stats_mode=full[p];[b][p]paletteuse=dither=bayer:bayer_scale=3:alpha_threshold=128" \
      -loop 0 -f gif "$out"
    ;;
  avif)
    echo "make-test-clip: ${out} (${kind}, ${seconds}s, ${size}, ${fps} fps, ${mode} alpha)"
    if command -v "$avifenc" >/dev/null 2>&1; then
      # The app's own path (DESIGN.md §4.2): RGBA PNG frames → avifenc.
      scratch=$(mktemp -d)
      trap 'rm -rf "$scratch"' EXIT
      png_frames "$scratch"
      "$avifenc" -j all -s 8 -q 60 --qalpha 90 -y 420 --fps "$fps" --repetition-count infinite \
        "$scratch"/f[0-9][0-9][0-9][0-9][0-9].png "$out" >/dev/null
    else
      # ffmpeg's avif muxer takes a colour stream plus an optional alpha stream
      # (single-plane gray). libaom needs explicit colour tags on the gray
      # stream or it rejects it ("Subsampling must be 0 with AOM_CICP_MC_IDENTITY").
      echo "make-test-clip: note: avifenc not found; using ffmpeg's avif muxer (libaom colour + alpha streams)" >&2
      ff -f lavfi -i "$graph" \
        -filter_complex "[0:v]format=rgba,split[c][a];[c]format=yuv420p[col];[a]alphaextract,format=gray,setparams=colorspace=bt709:color_primaries=bt709:color_trc=bt709[alp]" \
        -map "[col]" -map "[alp]" \
        -c:v libaom-av1 -usage realtime -cpu-used 8 -crf 30 -b:v 0 -row-mt 1 \
        -f avif "$out"
    fi
    ;;
esac

echo "make-test-clip: wrote $(stat -c %s "$out") bytes"
if [ "$kind" = avif ]; then
  # ffmpeg's mov demuxer shows an animated AVIF as up to four streams (the
  # primary still item colour/alpha, then the animation tracks), so report
  # every stream rather than just v:0 (which is always 1 frame).
  "$ffprobe" -v error -show_entries stream=index,codec_name,pix_fmt,width,height,nb_frames -of default=nw=1 "$out"
else
  "$ffprobe" -v error -select_streams v:0 \
    -show_entries stream=codec_name,profile,pix_fmt,width,height,r_frame_rate,nb_frames,duration \
    -of default=nw=1 "$out"
fi
