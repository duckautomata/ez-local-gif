#!/usr/bin/env bash
# pin-ffmpeg.sh — print Dockerfile ARG lines for a BtbN FFmpeg-Builds tag.
#
#   scripts/pin-ffmpeg.sh                         # newest dated autobuild with a 9.0 linux64 gpl asset
#   scripts/pin-ffmpeg.sh autobuild-2026-08-31-13-00
#
# BtbN keeps daily "autobuild-YYYY-MM-DD-hh-mm" releases for about two weeks
# and the last build of each month permanently, so the pinned tag in the
# Dockerfile eventually disappears. Run this, paste the three ARG lines into
# the Dockerfile, rebuild. The sha256 comes from the GitHub release asset
# digest and is re-verified by downloading the tarball (set NO_DOWNLOAD=1 to
# skip the ~125 MB download).
#
# Needs: curl, plus jq or node for JSON parsing.
set -euo pipefail

api=https://api.github.com/repos/BtbN/FFmpeg-Builds/releases
tag=${1:-}
series=${FFMPEG_SERIES:-9.0}
pattern="linux64-gpl-${series}.tar.xz"

fetch() { curl -fsSL --retry 3 -H 'Accept: application/vnd.github+json' "$@"; }

# pick_asset JSON → "tag name url digest" for the first matching linux64 gpl asset
pick_asset() {
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg p "$pattern" '
      (if type == "array" then . else [.] end)
      | map(select(.tag_name != "latest"))
      | map({tag: .tag_name, a: (.assets[] | select(.name | endswith($p)))})
      | .[0] | select(. != null)
      | "\(.tag) \(.a.name) \(.a.browser_download_url) \(.a.digest // "")"'
  elif command -v node >/dev/null 2>&1; then
    node -e '
      let s = ""; process.stdin.on("data", d => s += d).on("end", () => {
        let rs = JSON.parse(s); if (!Array.isArray(rs)) rs = [rs];
        for (const r of rs) { if (r.tag_name === "latest") continue;
          const a = r.assets.find(a => a.name.endsWith(process.argv[1]));
          if (a) { console.log(r.tag_name, a.name, a.browser_download_url, a.digest || ""); return; } }
      });' "$pattern"
  else
    echo "pin-ffmpeg: need jq or node" >&2; exit 1
  fi
}

if [ -n "$tag" ]; then
  line=$(fetch "$api/tags/$tag" | pick_asset)
else
  line=$(fetch "$api?per_page=30" | pick_asset)
fi
[ -n "$line" ] || { echo "pin-ffmpeg: no asset matching *$pattern found" >&2; exit 1; }
read -r tag asset url digest <<<"$line"
sha=${digest#sha256:}

if [ "${NO_DOWNLOAD:-0}" != 1 ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  echo "downloading $url ..." >&2
  curl -fsSL --retry 3 -o "$tmp/f.tar.xz" "$url"
  got=$(sha256sum "$tmp/f.tar.xz" | cut -d' ' -f1)
  if [ -n "$sha" ] && [ "$sha" != "$got" ]; then
    echo "pin-ffmpeg: digest mismatch: api=$sha download=$got" >&2; exit 1
  fi
  sha=$got
fi
[ -n "$sha" ] || { echo "pin-ffmpeg: no digest available; run without NO_DOWNLOAD" >&2; exit 1; }

cat <<EOF
ARG FFMPEG_TAG=$tag
ARG FFMPEG_ASSET=$asset
ARG FFMPEG_SHA256=$sha
EOF
