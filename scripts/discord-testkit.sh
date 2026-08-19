#!/usr/bin/env bash
# discord-testkit.sh — emit the Discord render-test matrix (DESIGN.md §9 item 1).
#
#   discord-testkit.sh [--premultiplied|--straight] [--fps N] OUTDIR [SRC]
#
# Decodes SRC once into an RGBA master (like the real pipeline), then fans out
# every encoder variant we need to validate on a private Discord server:
#
#   a  GIF   ffmpeg palettegen/paletteuse (matte #313338, alpha_threshold 128,
#            bayer scale 3, one global palette) → gifsicle -O2 --careful
#   b  GIF   same base → gifsicle -U (coalesced full frames, explicit disposal)
#   c  GIF   gifski from RGBA PNG frames (per-frame local palettes)
#   d  GIF   ffmpeg palette output only (no gifsicle)
#   e  WebP  libwebp_anim lossy  yuva420p q80  -loop 0 -map_metadata -1
#   e2 WebP  libwebp_anim lossy  bgra     q80  (libwebp does the RGB→YUV step)
#   f  WebP  libwebp_anim lossless bgra        -loop 0 -map_metadata -1
#   g  APNG  rgba, -pred mixed, -plays 0 (+ oxipng -o2 --strip safe)
#   h  Emote   128×128 variants of (a) and (e), fitted under 256 KiB
#   i  Sticker 320×320 variants of (a) and (g), ≤ 5 s, fitted under 512 KiB:
#            i1 GIF, i2 RGBA APNG (fps ladder), i3 indexed 8-bit-alpha APNG
#            (tile → pngquant → untile → apng pal8, DESIGN.md §4.2 rung B)
#   j  AVIF  avifenc from PNG frames, alpha, infinite repetition (experimental)
#
# Without SRC a synthetic 3 s 320×320 premultiplied ProRes 4444 clip is made
# with make-test-clip.sh (same directory as this script) at the master rate
# (--fps), so no frame is dropped or duplicated on the way to the master. A
# user SRC whose rate differs from --fps is resampled by ffmpeg's fps filter;
# the kit then warns and the README notes that a periodic cadence hitch is
# expected in every file. ProRes sources are treated as premultiplied
# (DaVinci Resolve default) unless --straight is given; they are
# unpremultiplied at the source bit depth exactly like the app
# (format=gbrap10le|gbrap12le|gbrap,setparams=alpha_mode=premultiplied,
# unpremultiply=inplace=1 — internal/graph/compile.go).
#
# OUTDIR must be writable by the current uid. In the runtime container
# /output is compose.yaml's ./output bind: if the Docker daemon auto-created
# that folder it is root-owned and uid 1000 cannot write to it — the kit
# stops early with a chown hint (README.md "Troubleshooting").
#
# Scratch (the RGBA masters, ~W×H×4 bytes per frame) goes to
# /dev/shm/ezl-testkit when /dev/shm is writable and has ≥ 256 MiB free
# (compose.yaml: shm_size 4gb; plain docker run: --shm-size=1g), otherwise
# to a mktemp dir under $TMPDIR with a warning.
#
# Writes OUTDIR/README.md: what each file tests, where to upload it, on which
# clients/themes/autoplay settings to check, what to look for, and a sizes
# table with the fit rung used for the emote/sticker variants.
#
# Env: EZLG_FFMPEG / EZLG_FFPROBE / EZLG_GIFSICLE / EZLG_GIFSKI / EZLG_OXIPNG /
#      EZLG_PNGQUANT / EZLG_AVIFENC / EZLG_WEBPINFO (tool paths),
#      EZLG_TESTKIT_MAX_PX (chat variant cap, 480).
set -euo pipefail

# ----------------------------------------------------------------------------
# args / tools
# ----------------------------------------------------------------------------
usage() { sed -n '2,51p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

premult=auto
fps=25
while [ $# -gt 0 ]; do
  case "$1" in
    --premultiplied) premult=yes; shift ;;
    --straight)      premult=no;  shift ;;
    --fps)           [ $# -ge 2 ] || usage; fps=$2; shift 2 ;;
    -h|--help)       usage ;;
    --)              shift; break ;;
    -*)              echo "discord-testkit: unknown option $1" >&2; usage ;;
    *)               break ;;
  esac
done
[ $# -ge 1 ] && [ $# -le 2 ] || usage
outdir=$1
src=${2:-}
[[ "$fps" =~ ^[0-9]+([.][0-9]+)?$ ]] || { echo "discord-testkit: --fps must be a number" >&2; exit 2; }

# ----------------------------------------------------------------------------
# OUTDIR: creatable and writable by this uid, checked up front with a hint.
# In the runtime container /output is compose.yaml's ./output bind mount; when
# the Docker daemon auto-created that host folder (bare-metal Linux, Docker in
# a WSL distro) it is root:root 0755 and uid 1000 (ezlg) cannot write to it,
# and a run as another user leaves files this uid cannot overwrite. Failing
# here beats a "Permission denied" from ffmpeg halfway through the matrix.
# ----------------------------------------------------------------------------
outdir_unwritable() { # outdir_unwritable REASON → hint on stderr, exit 1
  cat >&2 <<EOF
discord-testkit: OUTDIR '$outdir' is not writable by uid $(id -u) ($(id -un 2>/dev/null || echo '?')): $1
  In the container /output is the ./output bind mount from compose.yaml. If the Docker
  daemon created that folder, or an earlier run as root wrote into it, it belongs to
  root. Fix it once on the host:
      mkdir -p output && sudo chown -R 1000:1000 output
  or without sudo:
      docker compose run --rm --user root --entrypoint chown app -R 1000:1000 /output
  then re-run. (Docker Desktop on Windows/macOS needs neither.)
EOF
  exit 1
}
if ! err=$(mkdir -p "$outdir" 2>&1); then outdir_unwritable "${err#mkdir: }"; fi
if ! wprobe=$(mktemp -q "$outdir/.testkit-write.XXXXXX"); then outdir_unwritable "cannot create a file there"; fi
rm -f "$wprobe"
stale=$(find "$outdir" -mindepth 1 -maxdepth 1 ! -writable -print -quit 2>/dev/null || true)
[ -z "$stale" ] || outdir_unwritable "existing entry '$stale' is not writable (left by a run as another user?)"
outdir=$(cd "$outdir" && pwd)

ffmpeg=${EZLG_FFMPEG:-ffmpeg}
ffprobe=${EZLG_FFPROBE:-ffprobe}
gifsicle=${EZLG_GIFSICLE:-gifsicle}
gifski=${EZLG_GIFSKI:-gifski}
oxipng=${EZLG_OXIPNG:-oxipng}
pngquant=${EZLG_PNGQUANT:-pngquant}
avifenc=${EZLG_AVIFENC:-avifenc}
webpinfo=${EZLG_WEBPINFO:-webpinfo}
here=$(cd "$(dirname "$0")" && pwd)

for t in "$ffmpeg" "$ffprobe" "$gifsicle"; do
  command -v "$t" >/dev/null 2>&1 || { echo "discord-testkit: required tool not found: $t" >&2; exit 1; }
done
have() { command -v "$1" >/dev/null 2>&1; }

log()  { printf '[testkit] %s\n' "$*"; }
warn() { printf '[testkit] warning: %s\n' "$*" >&2; }

# Discord constants (DESIGN.md §5).
MATTE=313338
ALPHA_T=128
EMOTE_PX=128
EMOTE_LIMIT=262144
EMOTE_AIM=255000
STICKER_PX=320
STICKER_LIMIT=524288
STICKER_AIM=515000
STICKER_MAX_S=5

# Scratch on tmpfs when /dev/shm is writable and not Docker's default 64 MiB
# (the 480 px master alone is ~70 MB for 3 s at 25 fps; the store applies the
# same 256 MiB minimum). Otherwise fall back to $TMPDIR (disk) with a warning.
# Not the server's EZLG_SCRATCH (/dev/shm/ezl): that tree belongs to ezlg's
# sweeper, and it does not exist until the server has run.
SHM_MIN_KIB=$((256 * 1024))
shm_avail_kib() { df -Pk /dev/shm 2>/dev/null | awk 'NR == 2 { print $4 + 0 }'; }
scratch=""
if [ -d /dev/shm ] && [ -w /dev/shm ]; then
  avail=$(shm_avail_kib)
  if [ -n "$avail" ] && [ "$avail" -lt "$SHM_MIN_KIB" ]; then
    warn "/dev/shm has only $((avail / 1024)) MiB free (Docker's default is 64 MiB) — using ${TMPDIR:-/tmp} (disk) instead; run with --shm-size=1g (compose.yaml sets shm_size: 4gb) to keep the masters on tmpfs"
  elif mkdir -p /dev/shm/ezl-testkit 2>/dev/null; then
    scratch=$(mktemp -d /dev/shm/ezl-testkit/run.XXXXXX)
  fi
fi
[ -n "$scratch" ] || scratch=$(mktemp -d)
trap 'rm -rf "$scratch"; rmdir /dev/shm/ezl-testkit 2>/dev/null || true' EXIT
log "scratch: $scratch"

# ----------------------------------------------------------------------------
# source
# ----------------------------------------------------------------------------
if [ -z "$src" ]; then
  src="$outdir/src.mov"
  log "no SRC given → synthesising $src at ${fps} fps"
  # Same rate as the master: fps=$fps below must not drop/duplicate frames, or
  # every variant inherits a periodic cadence hitch that looks like a Discord
  # timing bug (a 30 fps clip → 25 fps master doubles every 5th step).
  FPS="$fps" bash "$here/make-test-clip.sh" "$src" 3 320x320 premultiplied
fi
[ -f "$src" ] || { echo "discord-testkit: source not found: $src" >&2; exit 1; }

probe() { # probe KEY → value of stream entry KEY for the first video stream
  "$ffprobe" -v error -select_streams v:0 -show_entries "stream=$1" -of default=nw=1:nk=1 "$src" | head -n 1
}
# fps_dec RATE → RATE ("30/1", "30000/1001", "29.97") as a short decimal
# ("30", "29.97"); empty when unknown/zero.
fps_dec() {
  awk -v r="$1" 'BEGIN {
    n = split(r, a, "/"); v = (n == 2) ? (a[2] > 0 ? a[1] / a[2] : 0) : a[1] + 0
    if (v <= 0) exit
    s = sprintf("%.3f", v); sub(/0+$/, "", s); sub(/\.$/, "", s); print s }'
}
# fps_same A B → 1 when the two rates agree within 0.001 fps (0 otherwise / unknown)
fps_same() {
  awk -v x="$(fps_dec "$1")" -v y="$(fps_dec "$2")" 'BEGIN {
    if (x == "" || y == "") { print 0; exit }
    d = x - y; if (d < 0) d = -d; print (d < 0.001) ? 1 : 0 }'
}
src_codec=$(probe codec_name)
src_w=$(probe width)
src_h=$(probe height)
src_pix=$(probe pix_fmt)
src_fps=$(probe r_frame_rate)              # e.g. 30/1, 30000/1001, 0/0 when unknown
src_fmt=$("$ffprobe" -v error -show_entries format=format_name -of default=nw=1:nk=1 "$src" | head -n 1)
[ -n "$src_w" ] && [ -n "$src_h" ] || { echo "discord-testkit: could not probe $src" >&2; exit 1; }
if [ "$premult" = auto ]; then
  if [ "$src_codec" = prores ]; then premult=yes; else premult=no; fi
fi
src_fps_dec=$(fps_dec "$src_fps")
resampled=no
if [ -n "$src_fps_dec" ] && [ "$(fps_same "$src_fps" "$fps")" != 1 ]; then resampled=yes; fi
log "source: $src ($src_codec $src_pix ${src_w}x${src_h} @ ${src_fps_dec:-?} fps, container $src_fmt, premultiplied=$premult)"
if [ "$resampled" = yes ]; then
  warn "source is ${src_fps_dec} fps but the master is decoded at ${fps} fps: fps=${fps} drops or duplicates frames periodically, so EVERY output will carry a cadence hitch that is not Discord's doing (pass --fps ${src_fps_dec} to keep the source cadence)"
fi

# ----------------------------------------------------------------------------
# master: decode once → RGBA rawvideo at $fps (mirrors DESIGN.md §4.1)
# ----------------------------------------------------------------------------
in_opts=()
case "$src_codec/$src_fmt" in
  vp9/*matroska*|vp9/*webm*) in_opts+=(-c:v libvpx-vp9) ;;   # alpha only via libvpx
esac
# bits_of PIX_FMT → sample depth like internal/probe.bitsFromPixFmt: strip a
# le/be suffix, then a planar "pN" tail (yuva444p10le → 10, gbrap12le → 12);
# everything else (rgba, bgra, pal8, …) is 8-bit.
bits_of() {
  local p=$1
  p=${p%le}; p=${p%be}
  case "$p" in
    *p9)  echo 9 ;;  *p10) echo 10 ;; *p12) echo 12 ;; *p14) echo 14 ;; *p16) echo 16 ;;
    gray9|gray10|gray12|gray14|gray16) echo "${p#gray}" ;;
    *) echo 8 ;;
  esac
}
# Hoisted unpremultiply at the source depth, exactly the app's chain
# (internal/graph/compile.go): planar GBRA at 10/12 bit for ProRes 4444 (XQ)
# so the alpha edges are divided before any 8-bit truncation. setparams tags
# the (untagged) decoded frames premultiplied; without it FFmpeg >= 8
# auto-inserts premultiply_dynamic in front of unpremultiply and the pair
# cancels out — the toggle silently becomes a no-op (DESIGN.md §4.3).
pre=""
if [ "$premult" = yes ]; then
  case "$(bits_of "$src_pix")" in
    10) ufmt=gbrap10le ;;
    12) ufmt=gbrap12le ;;
    *)  ufmt=gbrap ;;
  esac
  pre="format=${ufmt},setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,"
  log "unpremultiply at ${ufmt} (source ${src_pix})"
fi

# Chat variants are capped at EZLG_TESTKIT_MAX_PX on the long side (Discord's
# chat default is <= 480 px; a 1080p master would also blow through /dev/shm).
max_px=${EZLG_TESTKIT_MAX_PX:-480}
read -r W H < <(awk -v w="$src_w" -v h="$src_h" -v m="$max_px" 'BEGIN {
  s = (w > h ? w : h) > m ? m / (w > h ? w : h) : 1
  W = int(w * s / 2) * 2; H = int(h * s / 2) * 2
  if (W < 2) W = 2; if (H < 2) H = 2
  print W, H }')
scale=""
if [ "$W" != "$src_w" ] || [ "$H" != "$src_h" ]; then
  scale=",format=gbrap,premultiply=inplace=1,scale=${W}:${H}:flags=lanczos,unpremultiply=inplace=1"
  log "chat variants scaled to ${W}x${H} (EZLG_TESTKIT_MAX_PX=${max_px})"
fi

master_full="$scratch/master_full.rgba"
log "decoding master (${fps} fps, RGBA) ..."
"$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in_opts[@]+"${in_opts[@]}"}" -i "$src" \
  -filter_complex "[0:v]${pre}fps=${fps}${scale},format=rgba" \
  -f rawvideo -pix_fmt rgba "$master_full"
frame_bytes=$((W * H * 4))
n_frames=$(( $(stat -c %s "$master_full") / frame_bytes ))
[ "$n_frames" -gt 0 ] || { echo "discord-testkit: master is empty" >&2; exit 1; }
log "master: $n_frames frames of ${W}x${H}"

# read_master W H FILE [extra input options] — ffmpeg input args for a master.
read_master() { printf '%s\n' -f rawvideo -pix_fmt rgba -s "${1}x${2}" -framerate "$fps" "${@:4}" -i "$3"; }

# fit_filter W H — scale-to-contain (premultiplied lanczos) + transparent pad.
fit_filter() {
  printf 'format=gbrap,premultiply=inplace=1,scale=%s:%s:force_original_aspect_ratio=decrease:flags=lanczos,unpremultiply=inplace=1,format=rgba,pad=%s:%s:(ow-iw)/2:(oh-ih)/2:color=0x00000000' "$1" "$2" "$1" "$2"
}

# make_master_size W H → path of an RGBA master fitted to W×H (memoised).
make_master_size() {
  local w=$1 h=$2 out="$scratch/master_${1}x${2}.rgba"
  if [ "$w" = "$W" ] && [ "$h" = "$H" ]; then echo "$master_full"; return; fi
  if [ ! -s "$out" ]; then
    mapfile -t in < <(read_master "$W" "$H" "$master_full")
    "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" \
      -vf "$(fit_filter "$w" "$h")" -f rawvideo -pix_fmt rgba "$out"
  fi
  echo "$out"
}

# ----------------------------------------------------------------------------
# encoders (all read a master; args: MASTER W H OUT [knobs])
# ----------------------------------------------------------------------------
# GIF: matte → 1-bit threshold → single global palette → bayer dither.
#   gif_palette MASTER W H OUT [colors=256] [out_fps=$fps] [max_seconds]
gif_palette() {
  local m=$1 w=$2 h=$3 out=$4 colors=${5:-256} ofps=${6:-$fps} maxs=${7:-}
  local tail=() ; [ -n "$maxs" ] && tail=(-t "$maxs")
  local ratef=""; [ "$ofps" != "$fps" ] && ratef="fps=${ofps},"
  mapfile -t in < <(read_master "$w" "$h" "$m")
  "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" "${tail[@]+"${tail[@]}"}" \
    -filter_complex "[0:v]${ratef}split[c][a];[a]alphaextract,lut=c0='gte(val,${ALPHA_T})*255'[m];color=c=0x${MATTE}:s=${w}x${h}:r=${ofps},format=rgba[bg];[bg][c]overlay=format=auto:shortest=1,format=rgb24[f];[f][m]alphamerge,split[p1][p2];[p1]palettegen=max_colors=${colors}:reserve_transparent=1:stats_mode=diff[pal];[p2][pal]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle:alpha_threshold=${ALPHA_T}" \
    -loop 0 -f gif "$out"
}
#   gifsicle_o2 IN OUT [extra gifsicle args...]
gifsicle_o2() { local i=$1 o=$2; shift 2; "$gifsicle" -O2 --careful --loopcount=forever "$@" "$i" -o "$o"; }

#   webp_anim MASTER W H OUT lossy|lossless [q=80] [out_fps=$fps] [lossy_pix_fmt=yuva420p]
# Lossy takes yuva420p (ffmpeg does the RGB→YUV step, the app's default) or
# bgra (libwebp's own conversion; DESIGN.md §9 item 1 wants both compared).
webp_anim() {
  local m=$1 w=$2 h=$3 out=$4 kind=$5 q=${6:-80} ofps=${7:-$fps} pix=${8:-yuva420p}
  local vf=(); [ "$ofps" != "$fps" ] && vf=(-vf "fps=${ofps}")
  mapfile -t in < <(read_master "$w" "$h" "$m")
  if [ "$kind" = lossless ]; then
    "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" "${vf[@]+"${vf[@]}"}" \
      -c:v libwebp_anim -lossless 1 -compression_level 4 -pix_fmt bgra -loop 0 -map_metadata -1 -f webp "$out"
  else
    "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" "${vf[@]+"${vf[@]}"}" \
      -c:v libwebp_anim -lossless 0 -q:v "$q" -compression_level 4 -pix_fmt "$pix" -loop 0 -map_metadata -1 -f webp "$out"
  fi
}

#   apng_rgba MASTER W H OUT [out_fps=$fps] [max_seconds]
apng_rgba() {
  local m=$1 w=$2 h=$3 out=$4 ofps=${5:-$fps} maxs=${6:-}
  local tail=(); [ -n "$maxs" ] && tail=(-t "$maxs")
  local vf=(); [ "$ofps" != "$fps" ] && vf=(-vf "fps=${ofps}")
  mapfile -t in < <(read_master "$w" "$h" "$m")
  "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" "${tail[@]+"${tail[@]}"}" "${vf[@]+"${vf[@]}"}" \
    -c:v apng -pred mixed -pix_fmt rgba -plays 0 -f apng "$out"
  if have "$oxipng"; then "$oxipng" -o2 --strip safe -q "$out" || warn "oxipng failed on $out"; fi
}

#   apng_indexed MASTER W H OUT colors out_fps [max_seconds]
# Indexed 8-bit-alpha APNG: all frames are tiled into one sprite sheet,
# pngquant builds ONE palette with alpha for it, untile splits it back and
# the apng encoder writes pal8 (PLTE + tRNS, inter-frame diffs kept).
apng_indexed() {
  local m=$1 w=$2 h=$3 out=$4 colors=$5 ofps=$6 maxs=${7:-}
  local tail=(); [ -n "$maxs" ] && tail=(-t "$maxs")
  local sub="$scratch/idx.rgba" sheet="$scratch/sheet.png" sheetq="$scratch/sheet_q.png" n cols rows
  have "$pngquant" || { warn "pngquant not found; cannot build indexed APNG"; return 1; }
  # 1. resample to the rung fps; the exact frame count sets the tile layout
  mapfile -t in < <(read_master "$w" "$h" "$m")
  "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" "${tail[@]+"${tail[@]}"}" \
    -vf "fps=${ofps}" -f rawvideo -pix_fmt rgba "$sub"
  n=$(( $(fsize "$sub") / (w * h * 4) ))
  [ "$n" -gt 0 ] || { warn "no frames for indexed APNG"; return 1; }
  cols=$(awk -v n="$n" 'BEGIN { c = int(sqrt(n)); if (c * c < n) c++; print c }')
  rows=$(( (n + cols - 1) / cols ))
  # 2. sprite sheet → pngquant (shared palette, no dithering: keeps diffs small)
  mapfile -t in < <(read_master "$w" "$h" "$sub")
  "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" \
    -vf "tile=${cols}x${rows}:color=black@0" -frames:v 1 -c:v png -compression_level 1 "$sheet"
  "$pngquant" --nofs --speed 3 --force --output "$sheetq" "$colors" "$sheet"
  # 3. untile → apng pal8; pts = frame index / fps exactly, whatever untile's time base
  "$ffmpeg" -hide_banner -loglevel error -nostdin -y -framerate "$ofps" -i "$sheetq" \
    -vf "untile=${cols}x${rows},setpts=N/(${ofps}*TB)" -frames:v "$n" -fps_mode passthrough \
    -c:v apng -pix_fmt pal8 -pred mixed -plays 0 -f apng "$out"
  if have "$oxipng"; then "$oxipng" -o2 --strip safe -q "$out" || warn "oxipng failed on $out"; fi
}

#   png_frames MASTER W H DIR → RGBA PNG frames f00001.png ...
png_frames() {
  local m=$1 w=$2 h=$3 dir=$4
  mkdir -p "$dir"
  mapfile -t in < <(read_master "$w" "$h" "$m")
  "$ffmpeg" -hide_banner -loglevel error -nostdin -y "${in[@]}" -c:v png -compression_level 1 -pix_fmt rgba "$dir/f%05d.png"
}

fsize() { stat -c %s "$1"; }

# ----------------------------------------------------------------------------
# fit ladders for the byte-capped targets (mini version of DESIGN.md §5.4)
# ----------------------------------------------------------------------------
declare -A RUNG   # file → description of the winning rung
declare -A OVER   # file → 1 if nothing fitted

# fit_gif MASTER W H OUT AIM [max_seconds]
fit_gif() {
  local m=$1 w=$2 h=$3 out=$4 aim=$5 maxs=${6:-}
  local rung fps_ colors lossy tmp="$scratch/fit.gif" size
  for rung in "25 256 30" "20 128 40" "16.6667 128 60" "12.5 64 80" "10 32 120"; do
    read -r fps_ colors lossy <<<"$rung"
    gif_palette "$m" "$w" "$h" "$tmp" "$colors" "$fps_" "$maxs"
    gifsicle_o2 "$tmp" "$out" --lossy="$lossy" --colors "$colors"
    size=$(fsize "$out")
    RUNG[$(basename "$out")]="${fps_} fps · ${colors} colours · lossy ${lossy}"
    if [ "$size" -le "$aim" ]; then return; fi
  done
  OVER[$(basename "$out")]=1
}
# fit_webp MASTER W H OUT AIM
fit_webp() {
  local m=$1 w=$2 h=$3 out=$4 aim=$5 rung fps_ q size
  for rung in "25 80" "25 65" "20 60" "16.6667 50" "12.5 40" "10 30"; do
    read -r fps_ q <<<"$rung"
    webp_anim "$m" "$w" "$h" "$out" lossy "$q" "$fps_"
    size=$(fsize "$out")
    RUNG[$(basename "$out")]="${fps_} fps · q ${q}"
    if [ "$size" -le "$aim" ]; then return; fi
  done
  OVER[$(basename "$out")]=1
}
# fit_apng_rgba MASTER W H OUT AIM max_seconds
fit_apng_rgba() {
  local m=$1 w=$2 h=$3 out=$4 aim=$5 maxs=$6 fps_ size
  # RGBA is the probe rung of DESIGN.md §5.4; the ladder goes low so busy synthetic
  # content still yields an uploadable file (real cartoons fit at 20-25 fps).
  for fps_ in 25 20 15 12.5 10 8 5; do
    apng_rgba "$m" "$w" "$h" "$out" "$fps_" "$maxs"
    size=$(fsize "$out")
    RUNG[$(basename "$out")]="${fps_} fps · RGBA"
    if [ "$size" -le "$aim" ]; then return; fi
  done
  OVER[$(basename "$out")]=1
}
# fit_apng_indexed MASTER W H OUT AIM max_seconds
fit_apng_indexed() {
  local m=$1 w=$2 h=$3 out=$4 aim=$5 maxs=$6 rung fps_ colors size
  for rung in "25 256" "20 256" "15 256" "12.5 128" "10 64"; do
    read -r fps_ colors <<<"$rung"
    apng_indexed "$m" "$w" "$h" "$out" "$colors" "$fps_" "$maxs" || { rm -f "$out"; return; }
    size=$(fsize "$out")
    RUNG[$(basename "$out")]="${fps_} fps · ${colors} colours · pal8"
    if [ "$size" -le "$aim" ]; then return; fi
  done
  OVER[$(basename "$out")]=1
}

# ----------------------------------------------------------------------------
# produce the matrix
# ----------------------------------------------------------------------------
cd "$outdir"

# Every variant file, in table order (a … j). Also what a re-run clears first
# so a variant that is skipped this time (no gifski / avifenc) does not
# survive from an older run and get listed.
variants=(
  a_gif_ffmpeg-palette_gifsicle-O2.gif
  b_gif_ffmpeg-palette_gifsicle-U.gif
  c_gif_gifski_local-palettes.gif
  d_gif_ffmpeg-only.gif
  e_webp_lossy_yuva420p_q80.webp
  e2_webp_lossy_bgra_q80.webp
  f_webp_lossless_bgra.webp
  g_apng_rgba.png
  h1_emote128_gif.gif
  h2_emote128_webp.webp
  i1_sticker320_gif.gif
  i2_sticker320_apng-rgba.png
  i3_sticker320_apng-indexed.png
  j_avif_alpha.avif
)
rm -f -- "${variants[@]}"

log "a/b/d: ffmpeg palette GIF + gifsicle variants"
gif_palette "$master_full" "$W" "$H" d_gif_ffmpeg-only.gif
gifsicle_o2 d_gif_ffmpeg-only.gif a_gif_ffmpeg-palette_gifsicle-O2.gif
"$gifsicle" -U --careful --loopcount=forever d_gif_ffmpeg-only.gif -o b_gif_ffmpeg-palette_gifsicle-U.gif

log "c: gifski from RGBA PNG frames"
if have "$gifski"; then
  png_frames "$master_full" "$W" "$H" "$scratch/frames"
  "$gifski" --quiet --fps "$fps" -W "$W" -H "$H" --quality 90 --repeat 0 --matte "$MATTE" \
    -o c_gif_gifski_local-palettes.gif "$scratch"/frames/f*.png
else
  warn "gifski not found; skipping variant c"
fi

log "e/e2/f: libwebp_anim lossy yuva420p / lossy bgra / lossless"
webp_anim "$master_full" "$W" "$H" e_webp_lossy_yuva420p_q80.webp lossy 80 "$fps" yuva420p
webp_anim "$master_full" "$W" "$H" e2_webp_lossy_bgra_q80.webp lossy 80 "$fps" bgra
webp_anim "$master_full" "$W" "$H" f_webp_lossless_bgra.webp lossless

log "g: APNG rgba"
apng_rgba "$master_full" "$W" "$H" g_apng_rgba.png

log "h: emote ${EMOTE_PX}x${EMOTE_PX} (≤ ${EMOTE_LIMIT} B)"
m128=$(make_master_size "$EMOTE_PX" "$EMOTE_PX")
fit_gif  "$m128" "$EMOTE_PX" "$EMOTE_PX" h1_emote128_gif.gif  "$EMOTE_AIM"
fit_webp "$m128" "$EMOTE_PX" "$EMOTE_PX" h2_emote128_webp.webp "$EMOTE_AIM"

log "i: sticker ${STICKER_PX}x${STICKER_PX} (≤ ${STICKER_LIMIT} B, ≤ ${STICKER_MAX_S} s)"
m320=$(make_master_size "$STICKER_PX" "$STICKER_PX")
fit_gif          "$m320" "$STICKER_PX" "$STICKER_PX" i1_sticker320_gif.gif          "$STICKER_AIM" "$STICKER_MAX_S"
fit_apng_rgba    "$m320" "$STICKER_PX" "$STICKER_PX" i2_sticker320_apng-rgba.png    "$STICKER_AIM" "$STICKER_MAX_S"
fit_apng_indexed "$m320" "$STICKER_PX" "$STICKER_PX" i3_sticker320_apng-indexed.png "$STICKER_AIM" "$STICKER_MAX_S"

log "j: animated AVIF (experimental)"
if have "$avifenc"; then
  [ -d "$scratch/frames" ] || png_frames "$master_full" "$W" "$H" "$scratch/frames"
  if ! "$avifenc" -j all -s 8 -q 60 --qalpha 90 -y 420 --fps "$fps" --repetition-count infinite \
        "$scratch"/frames/f*.png j_avif_alpha.avif >/dev/null 2>&1; then
    warn "avifenc failed; skipping variant j"; rm -f j_avif_alpha.avif
  fi
else
  warn "avifenc not found; skipping variant j"
fi

# ----------------------------------------------------------------------------
# quick verification of every output (bytes, dims, frames, loop / flags)
# ----------------------------------------------------------------------------
verify_line() { # verify_line FILE → "WxH · N frames · <format facts>"
  local f=$1 dims frames facts=""
  dims=$("$ffprobe" -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$f" 2>/dev/null | head -n 1)
  frames=$("$ffprobe" -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -of default=nw=1:nk=1 "$f" 2>/dev/null | head -n 1)
  case "$f" in
    *.gif)
      facts=$("$gifsicle" --info "$f" 2>/dev/null | awk '/loop/ {l=$0} /global color table/ {g=$0} END { sub(/^ */,"",l); sub(/^ */,"",g); printf "%s; %s", l, g }') ;;
    *.webp)
      if have "$webpinfo"; then
        # VP8X flags come first; per-frame ANMF/VP8L blocks repeat "Alpha:" later.
        facts=$("$webpinfo" "$f" 2>/dev/null | awk '/Chunk VP8X/ {x=1} x && /Alpha:/ && a=="" {a=$2} x && /Animation:/ && an=="" {an=$2} /Loop count/ {lc=$NF} END { printf "VP8X alpha=%s anim=%s loop=%s", a, an, lc }')
      fi ;;
    *.png)
      # chunk census (acTL = animated, fcTL per frame, PLTE+tRNS = indexed with alpha)
      facts=$(printf 'acTL=%s fcTL=%s PLTE=%s tRNS=%s (-plays 0)' \
        "$(grep -a -o acTL "$f" | wc -l)" "$(grep -a -o fcTL "$f" | wc -l)" \
        "$(grep -a -o PLTE "$f" | wc -l)" "$(grep -a -o tRNS "$f" | wc -l)") ;;
    *.avif)
      # ffprobe sees the colour and alpha planes as two streams; avifdec knows the sequence.
      if have avifdec; then
        facts=$(avifdec --info "$f" 2>/dev/null | awk -F': *' '/Repeat Count/ {r=$2} /Alpha/ {a=$2} /timescales per second/ {n=$0; sub(/^ *\* */, "", n)} END { printf "avifdec: alpha=%s, repeat=%s, %s", a, r, n }')
      else
        facts="avifenc, repetition infinite"
      fi ;;
  esac
  printf '%s · %s frames · %s' "${dims:-?}" "${frames:-?}" "$facts"
}

# outputs — every variant file that was produced, in table order (a … j;
# a plain glob would put e2_ before e_ and h1_ before h_-style names).
outputs() {
  local f
  for f in "${variants[@]}"; do
    [ -f "$f" ] && printf '%s\n' "$f"
  done
  return 0
}

# ----------------------------------------------------------------------------
# README.md
# ----------------------------------------------------------------------------
kib() { awk -v b="$1" 'BEGIN { printf "%.1f", b/1024 }'; }
budget_of() {
  case "$1" in
    h1_*|h2_*)      echo "$EMOTE_LIMIT" ;;
    i1_*|i2_*|i3_*) echo "$STICKER_LIMIT" ;;
    *) echo "" ;;
  esac
}
status_of() { # status_of FILE BUDGET
  local f=$1 b=$2 s
  s=$(fsize "$f")
  if [ -z "$b" ]; then echo "—"; return; fi
  if [ -n "${OVER[$f]:-}" ] || [ "$s" -gt "$b" ]; then echo "**OVER — Discord will reject**"; else echo "ok"; fi
}

{
  cat <<EOF
# Discord render-test matrix

Generated by \`discord-testkit.sh\` on $(date -u +%Y-%m-%dT%H:%MZ).
Source: \`$(basename "$src")\` — $src_codec $src_pix ${src_w}x${src_h} @ ${src_fps_dec:-?} fps → master ${fps} fps,
decoded once to an RGBA master of ${W}x${H} (${n_frames} frames, premultiplied source: ${premult}).
EOF
  if [ "$resampled" = yes ]; then
    cat <<EOF

> **Note:** the source runs at ${src_fps_dec} fps and was resampled to ${fps} fps, which drops or
> duplicates a frame periodically. A regular cadence hitch is therefore expected in **every** file
> below and is not a Discord artefact — judge timing against local playback of the same file, or
> re-run the kit with \`--fps ${src_fps_dec}\`.
EOF
  fi
  cat <<EOF

Every file below is one encoder path from \`docs/DESIGN.md\` §4.2 / §9. Upload them to a
**private** Discord server and tick the checklist for each client. The synthetic clip has a
soft-edged circle orbiting the centre, an opaque block top-left and a 50 %-alpha block
top-right (GIF thresholds it to opaque-on-matte; WebP/APNG/AVIF should show it translucent).

## Files

| File | What it tests | Upload as | Expect |
|---|---|---|---|
| \`a_gif_ffmpeg-palette_gifsicle-O2.gif\` | Default GIF path: palettegen/paletteuse, matte #313338, alpha_threshold 128, bayer 3, single global palette, then \`gifsicle -O2 --careful\` (frame-diff optimised) | chat attachment | transparent, dark fringe on light theme only, no flicker |
| \`b_gif_ffmpeg-palette_gifsicle-U.gif\` | Same frames coalesced by \`gifsicle -U\` (full frames, explicit disposal) — the fallback if (a) glitches through Discord's proxy | chat attachment | identical look to (a), bigger file |
| \`c_gif_gifski_local-palettes.gif\` | gifski quality 90 from RGBA PNGs, matte #313338 (per-frame local palettes — historically glitchy on Discord) | chat attachment | best gradients; watch for random per-frame colour changes |
| \`d_gif_ffmpeg-only.gif\` | ffmpeg palette GIF with no gifsicle pass (ffmpeg's own GCE/disposal choices) | chat attachment | same as (a); black background here means the linter fixer is required |
| \`e_webp_lossy_yuva420p_q80.webp\` | Animated WebP, libwebp_anim lossy q80 yuva420p (ffmpeg converts RGB→YUV, the app's default), \`-loop 0\`, no metadata | chat attachment | soft alpha survives, loops forever, no ghost trails |
| \`e2_webp_lossy_bgra_q80.webp\` | Same lossy settings fed \`-pix_fmt bgra\` (libwebp does the RGB→YUV step itself; alpha stays lossless) — the yuva420p vs bgra comparison from DESIGN.md §9 | chat attachment | same as (e); note any difference in edge colour, chroma bleed on the soft edge, or size |
| \`f_webp_lossless_bgra.webp\` | Animated WebP lossless bgra | chat attachment | pixel-exact, loops forever |
| \`g_apng_rgba.png\` | APNG RGBA \`-plays 0\` (Discord shows only frame 0 for APNG *attachments*) | chat attachment (expect still) | still first frame is a known limitation, not a bug |
| \`h1_emote128_gif.gif\` | Emote GIF 128×128 fitted under 256 KiB (${RUNG[h1_emote128_gif.gif]:-n/a}) | Server Settings › Emoji | animates at ~22 px, transparent, keeps looping |
| \`h2_emote128_webp.webp\` | Emote animated WebP 128×128 (${RUNG[h2_emote128_webp.webp]:-n/a}) | Server Settings › Emoji | accepted, animates, soft alpha kept |
| \`i1_sticker320_gif.gif\` | Sticker GIF exactly 320×320, ≤ 5 s (${RUNG[i1_sticker320_gif.gif]:-n/a}) | Server Settings › Stickers | accepted, animates in the picker and in chat |
| \`i2_sticker320_apng-rgba.png\` | Sticker APNG exactly 320×320, ≤ 5 s, RGBA 8-bit alpha (${RUNG[i2_sticker320_apng-rgba.png]:-n/a}) | Server Settings › Stickers | animates, soft alpha, no "frame rate too small/large" error |
| \`i3_sticker320_apng-indexed.png\` | Sticker APNG, indexed colour with 8-bit alpha (PLTE + tRNS, one shared palette; ${RUNG[i3_sticker320_apng-indexed.png]:-n/a}) — the usual fit winner | Server Settings › Stickers | same as (i2); confirms Discord accepts pal8 APNG with tRNS |
EOF
  if [ -f j_avif_alpha.avif ]; then
    echo '| `j_avif_alpha.avif` | Animated AVIF with alpha (avifenc, repetition infinite) — experimental; Discord transcodes AVIF→WebP | chat attachment | animates with alpha (or document that it does not) |'
  fi
  cat <<'EOF'

## Where to look

Check every file on each client, in both themes, with animation autoplay on **and** off
(User Settings › Accessibility › "Automatically play GIFs" and "Reduced motion"):

| Client | Dark theme | Light theme | Autoplay off / reduced motion |
|---|---|---|---|
| Desktop app (Windows/macOS/Linux) | ☐ | ☐ | ☐ |
| Web (Chrome / Firefox) | ☐ | ☐ | ☐ |
| iOS | ☐ | ☐ | ☐ |
| Android | ☐ | ☐ | ☐ |

For emotes also check: inline in a message (~22 px), jumbo (message with only the emote, 48 px),
as a reaction (~16 px), in the emoji picker (32 px, animates on hover). For stickers: picker
preview and in-chat (160 px).

## What to look for

- **Alpha survives** — the area outside the circle/blocks shows the chat background, not black,
  white or garbage colour. Compare dark and light theme.
- **No black background** — the classic lilliput symptom (frame 0 without a transparency flag).
- **No colour flicker / random per-frame colours** — especially (a), (c), (d) after Discord's
  GIF→WebP transcode.
- **First-frame still** — with autoplay off the still shown must be a real, non-blank frame.
- **Loops forever** — the animation must not stop after one pass (NETSCAPE loop / WebP loop 0 /
  APNG num_plays 0 all survived).
- **Timing** — the circle completes one smooth orbit per loop. Compare with local playback of
  the same file: stutter or double-speed that appears only in Discord means delays were mangled
  by the transcode.
- **Soft edges** — WebP/APNG/AVIF keep the soft circle edge and the 50 %-alpha block; GIF shows
  a hard edge and the block matte-blended (by design).
- Emote/sticker uploads that are rejected: note the error code (50138 = could not shrink,
  170006 = sticker frame rate) and the file size below.

## Sizes

| File | Bytes | KiB | Budget | Fit |
|---|---:|---:|---:|---|
EOF
  for f in $(outputs); do
    b=$(budget_of "$f")
    printf '| `%s` | %s | %s | %s | %s |\n' "$f" "$(fsize "$f")" "$(kib "$(fsize "$f")")" "${b:-—}" "$(status_of "$f" "$b")"
  done
  cat <<'EOF'

## Verify (local decoders)

| File | Facts |
|---|---|
EOF
  for f in $(outputs); do
    printf '| `%s` | %s |\n' "$f" "$(verify_line "$f")"
  done
  cat <<'EOF'

Record results in `docs/DESIGN.md` §9 (or an issue) with client + version, theme, autoplay
setting and a screenshot of anything that renders wrong.
EOF
} > README.md

log "done → $outdir"
echo
echo "file                                     bytes     KiB  budget   status"
for f in $(outputs); do
  b=$(budget_of "$f")
  printf '%-40s %8s %7s %8s  %s\n' "$f" "$(fsize "$f")" "$(kib "$(fsize "$f")")" "${b:-—}" "$(status_of "$f" "$b" | tr -d '*')"
done
