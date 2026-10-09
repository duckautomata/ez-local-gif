#!/usr/bin/env bash
# Builds the background-removal benchmark corpus (layout and metric definitions
# in CORPUS.md) from a DaVinci Resolve ProRes 4444 export of an illustrated
# character with binary alpha. The subject is the window t = 2..5 s at 15 fps
# = 45 frames, 720x720; its alpha is the ground truth for every composite.
#
#   make-corpus.sh [OUTDIR]        OUTDIR defaults to ./corpus next to this script
#
# Environment:
#   EZLG_BENCH_SRC    the alpha source clip
#                     (default: <repo>/output/resolve/example-1.mov)
#   EZLG_BENCH_SRC2   a second alpha clip, a REAL screen capture, flattened to
#                     opaque RGB as the no-ground-truth case; skipped when the
#                     file is missing (default: <repo>/output/resolve/example-2.mov)
#   FFMPEG            ffmpeg binary (default: ffmpeg on PATH; FFmpeg >= 9 is
#                     what the research used)
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
S="${1:-$HERE/corpus}"
SRC="${EZLG_BENCH_SRC:-$REPO/output/resolve/example-1.mov}"
SRC2="${EZLG_BENCH_SRC2:-$REPO/output/resolve/example-2.mov}"
FFMPEG="${FFMPEG:-ffmpeg}"
if [ ! -f "$SRC" ]; then
  echo "make-corpus.sh: source clip not found: $SRC (set EZLG_BENCH_SRC)" >&2
  exit 1
fi
HEAD="setparams=alpha_mode=premultiplied,unpremultiply=inplace=1,format=rgba"
N=45; FPS=15; SZ=720x720
mkdir -p "$S/subject/rgba" "$S/gt"
# straight-alpha subject frames + ground-truth alpha
"$FFMPEG" -v error -y -ss 2 -t 3 -i "$SRC" -vf "fps=$FPS,$HEAD" -frames:v $N "$S/subject/rgba/%03d.png"
"$FFMPEG" -v error -y -ss 2 -t 3 -i "$SRC" -vf "fps=$FPS,$HEAD,alphaextract,format=gray" -frames:v $N "$S/gt/%03d.png"
# backgrounds (lavfi sources, 720x720 @ 15 fps)
declare -A BG
BG[green]="color=c=0x00ff00:s=$SZ:r=$FPS"
BG[white]="color=c=white:s=$SZ:r=$FPS"
BG[black]="color=c=black:s=$SZ:r=$FPS"
BG[vgrad]="gradients=s=$SZ:r=$FPS:c0=0x1e3a8a:c1=0x7c3aed:x0=360:y0=0:x1=360:y1=719:speed=0"
BG[multi]="gradients=s=$SZ:r=$FPS:c0=0xff6b6b:c1=0xfeca57:c2=0x48dbfb:c3=0x1dd1a1:nb_colors=4:x0=0:y0=0:x1=719:y1=719:speed=0"
BG[skin]="gradients=s=$SZ:r=$FPS:c0=0xffe9c4:c1=0xf5b041:x0=0:y0=0:x1=719:y1=719:speed=0"
BG[anim]="gradients=s=$SZ:r=$FPS:c0=0x1e3a8a:c1=0x7c3aed:c2=0x48dbfb:nb_colors=3:speed=0.05"
BG[busy]="testsrc2=s=$SZ:r=$FPS"
BG[noise]="gradients=s=$SZ:r=$FPS:c0=0x1e3a8a:c1=0x7c3aed:x0=360:y0=0:x1=360:y1=719:speed=0,noise=alls=24:allf=t+u"
for name in "${!BG[@]}"; do
  d="$S/bg/$name"; mkdir -p "$d/png"
  "$FFMPEG" -v error -y -ss 2 -t 3 -i "$SRC" -f lavfi -i "${BG[$name]}" \
    -filter_complex "[0:v]fps=$FPS,$HEAD[fg];[1:v]format=rgb24[bg];[bg][fg]overlay=format=auto:shortest=1,format=rgb24[o]" \
    -map "[o]" -frames:v $N "$d/png/%03d.png"
  # web-realistic MP4 (4:2:0, crf 18)
  "$FFMPEG" -v error -y -framerate $FPS -i "$d/png/%03d.png" -c:v libx264 -crf 18 -pix_fmt yuv420p -movflags +faststart "$d/$name.mp4"
done
# GIF variants (256-colour bayer dither — the typical emote source) for the gradient cases
for name in vgrad multi skin noise; do
  d="$S/bg/$name"; mkdir -p "$d/gifpng"
  "$FFMPEG" -v error -y -framerate $FPS -i "$d/png/%03d.png" \
    -filter_complex "[0:v]split[a][b];[a]palettegen=max_colors=256:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=3:diff_mode=rectangle" -loop 0 "$d/$name.gif"
  "$FFMPEG" -v error -y -i "$d/$name.gif" -vf "fps=$FPS,format=rgb24" -frames:v $N "$d/gifpng/%03d.png"
done
# real screen-capture case (no ground truth): the second clip flattened to opaque RGB, first 45 frames
if [ -f "$SRC2" ]; then
  mkdir -p "$S/real/ex2/png"
  "$FFMPEG" -v error -y -i "$SRC2" -vf "$HEAD,format=rgb24" -frames:v $N "$S/real/ex2/png/%03d.png"
  "$FFMPEG" -v error -y -framerate 16 -i "$S/real/ex2/png/%03d.png" -c:v libx264 -crf 18 -pix_fmt yuv420p "$S/real/ex2/ex2.mp4"
else
  echo "make-corpus.sh: EZLG_BENCH_SRC2 not found ($SRC2); skipping the real/ex2 case" >&2
fi
echo "done: $S"
