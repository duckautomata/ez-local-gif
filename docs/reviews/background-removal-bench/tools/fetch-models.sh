#!/usr/bin/env bash
# Downloads the three benchmark models listed in ../models.json into
# $EZLG_BENCH_MODELS (default: ../models next to this directory) and verifies
# their sha256. Files already present are only verified. Needs curl + sha256sum.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
DEST="${EZLG_BENCH_MODELS:-$HERE/../models}"
BASE="https://github.com/danielgatis/rembg/releases/download/v0.0.0"
mkdir -p "$DEST"
fetch() {  # fetch NAME SHA256
  local f="$DEST/$1"
  if [ ! -f "$f" ]; then
    echo "fetching $1"
    curl -L --fail --progress-bar -o "$f.part" "$BASE/$1"
    mv "$f.part" "$f"
  fi
  echo "$2  $f" | sha256sum -c -
}
fetch u2netp.onnx 309c8469258dda742793dce0ebea8e6dd393174f89934733ecc8b14c76f4ddd8
fetch isnet-anime.onnx f15622d853e8260172812b657053460e20806f04b9e05147d49af7bed31a6e99
fetch BiRefNet-general-bb_swin_v1_tiny-epoch_232.onnx 5600024376f572a557870a5eb0afb1e5961636bef4e1e22132025467d0f03333
echo "models ready in $DEST"
