#!/usr/bin/env bash
# integration-test.sh — end-to-end smoke test against a running ezlg server.
#
# Phase 1 (18 checks): makes a 2 s 160×160 premultiplied ProRes 4444 clip,
# uploads it, renders a Discord emote GIF (128×128) and a chat WebP through
# the HTTP API, downloads both and checks them with ffmpeg / gifsicle /
# webpinfo.
#
# Phase 2 (same source unless noted; every case is independent — a failing
# case is reported and the rest still run):
#   emote-fit   GIF 128×128, target emote, fitBytes 262144 → primary ≤ 262144,
#               report.ok, ≥ 1 "alternative" file in the manifest
#   sticker     APNG 320×320, colors 256, target sticker, fitBytes 524288 →
#               ≤ 524288, report.ok, indexed (PLTE + tRNS) and animated (acTL)
#   avif        animated AVIF → decodes with ffmpeg, > 1 frame
#   png / jpeg  static images from the clip → valid PNG / JPEG
#   frames      format frames, frameFormat png → manifest lists N "frame" files
#               + an "archive" (frames.zip); the zip downloads (?dl=1) and unzips
#   sequence    3 PNGs uploaded in one request (+ delayMs=100) → a "sequence"
#               source → rendered GIF has 3 frames
#   optimize    a GIF upload, preset optimize lossy 40 → report.ok, smaller
#               than the input
#   from-result POST /api/sources/from-result on the Phase 1 GIF → 200 + hash,
#               GET /api/sources/{hash} → 200
#
# Phase 3 (editing ops; needs Phase 2's forward frames export):
#   caps/fonts  features.keying/overlays/proxy/fonts true; GET /api/fonts lists
#               DejaVu Sans and Noto Sans
#   chromakey   a green-screen clip (make-test-clip.sh green) keyed with the
#               chromakey op → emote GIF: report.ok, report.hasAlpha, frame 0
#               has transparent AND opaque pixels, corner pixel transparent
#   colorkey    the same clip through the colorkey op → chat WebP: report.ok,
#               VP8X ALPHA flag, corner pixel transparent
#   text        drawtext op (font "DejaVu Sans") → GIF renders; the still with
#               the op differs from the plain still (something was painted)
#   overlay-png a 48×48 RGBA PNG uploaded as a second source → overlay op →
#               WebP renders; the "sources" still with/without it differ
#   overlay-gif a 0.5 s GIF as a second source over the 2 s clip (looping) →
#               GIF with as many frames as the forward export (> the overlay's);
#               stills via "sources": the overlay is painted at t=0.8 (differs
#               from the plain still), looping and noLoop agree at t=0.3 and
#               differ at t=0.8 (the overlay looped rather than holding)
#   reverse     frames export of the reversed clip: first/last frames are the
#               forward export's last/first (sha256, else PSNR ≥ 50 dB)
#   autocrop    trim 0–0.1 s + autocrop → PNG smaller than the 160×160 source
#   webp-trim   an animated WebP built from the ProRes clip in the test (10 fps,
#               2 s, libwebp_anim -loop 0) → trim 0.5–1.5 s, frames export →
#               job done with exactly (1.5-0.5)×10 = 10 frame files (FFmpeg 9's
#               webp_anim demuxer decodes nothing after an input seek, so the
#               server must trim such sources in the filtergraph: 0 frames =
#               it seeked, 20 = the trim was ignored)
#   proxy       POST /api/proxy → image/webp, VP8X ANIM flag, canvas ≤ maxW,
#               > 1 frame
# Exit status is non-zero if anything fails; a summary is printed at the end.
#
#   EZLG_URL=http://localhost:8080 scripts/integration-test.sh
#
#   # start the server yourself in the same container (dev image), then test:
#   EZLG_START_SERVER=1 scripts/integration-test.sh
#
#   # from the host, all in one go:
#   docker compose -f compose.yaml -f compose.dev.yaml run --rm \
#       -e EZLG_START_SERVER=1 app bash scripts/integration-test.sh
#
# Env:
#   EZLG_URL            server base URL (default http://localhost:8080)
#   EZLG_START_SERVER   1 → `go build ./cmd/ezlg` and run `ezlg serve` in the
#                       background on a fresh temp data dir, wait for /healthz,
#                       and stop it on exit. Any inherited EZLG_DATA is
#                       deliberately ignored (the dev image bakes
#                       EZLG_DATA=/data, backed by the persistent
#                       ezlg-data-dev volume — honouring it would answer every
#                       re-run from the on-disk result cache); set
#                       EZLG_TEST_DATA=/path to reuse a dir on purpose
#   EZLG_TEST_DATA      data dir for the spawned server: explicit opt-in
#                       override of the fresh temp dir (EZLG_START_SERVER only)
#   EZLG_TEST_STRICT    1 → jobs answered from the server's result cache
#                       ("cached": true) count as failures. Default is a loud
#                       warning only, so EZLG_URL runs against a long-lived
#                       stack still pass
#   EZLG_TEST_TIMEOUT   seconds to wait for each job (default 120)
#   EZLG_TEST_PHASE2    0 → run the Phase 1 checks only (default 1; also skips Phase 3)
#   EZLG_TEST_PHASE3    0 → skip the Phase 3 checks (default 1)
#   EZLG_TEST_KEEP      1 → keep the temp dir (printed at the end)
#   EZLG_FFMPEG / EZLG_FFPROBE / EZLG_GIFSICLE / EZLG_WEBPINFO / EZLG_AVIFDEC
#                       tool overrides (avifdec, unzip and jq are optional:
#                       the script falls back to ffprobe / a bash zip check /
#                       grep on the JSON)
set -uo pipefail

url=${EZLG_URL:-http://localhost:8080}
url=${url%/}
timeout=${EZLG_TEST_TIMEOUT:-120}
phase2=${EZLG_TEST_PHASE2:-1}
phase3=${EZLG_TEST_PHASE3:-1}
ffmpeg=${EZLG_FFMPEG:-ffmpeg}
ffprobe=${EZLG_FFPROBE:-ffprobe}
gifsicle=${EZLG_GIFSICLE:-gifsicle}
webpinfo=${EZLG_WEBPINFO:-webpinfo}
avifdec=${EZLG_AVIFDEC:-avifdec}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)

tmp=$(mktemp -d)
server_pid=""
pass=0; failn=0; cached_n=0
results=()

cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [ "${EZLG_TEST_KEEP:-0}" = 1 ]; then
    echo "kept: $tmp"
  else
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT

log()   { printf '[itest] %s\n' "$*"; }
ok()    { pass=$((pass+1));  results+=("PASS  $*"); printf '[itest]   pass: %s\n' "$*"; }
fail()  { failn=$((failn+1)); results+=("FAIL  $*"); printf '[itest]   FAIL: %s\n' "$*" >&2; }
abort() { summary; exit 1; }
die()   { fail "$*"; abort; }
summary() {
  echo
  if [ "${cached_n:-0}" -gt 0 ]; then
    printf '[itest] WARNING: %s job(s) came from the server'\''s result cache — this run did not exercise the render pipeline for them.\n' "$cached_n"
    printf '[itest]          Wipe the server'\''s data dir (docker compose -f compose.yaml -f compose.dev.yaml down -v, or docker volume rm <project>_ezlg-data-dev) or bump jobs.PipelineVersion to force a re-render.\n'
    if [ "${EZLG_TEST_STRICT:-0}" = 1 ]; then
      failn=$((failn+1))
      results+=("FAIL  $cached_n job(s) served from the result cache (EZLG_TEST_STRICT=1)")
    fi
  fi
  echo "== integration test summary ($pass passed, $failn failed) =="
  printf '  %s\n' "${results[@]}"
}
have()  { command -v "$1" >/dev/null 2>&1; }
fsize() { stat -c %s "$1"; }

# ---- tiny JSON helpers (jq when present, otherwise regex on whitespace-stripped JSON)
use_jq=0; have jq && use_jq=1
stripped() { tr -d ' \n\r\t' < "$1"; }
json_str() { # json_str FILE JQ_PATH GREP_KEY → first string value
  if [ "$use_jq" = 1 ]; then jq -r "$2 // empty" "$1" 2>/dev/null | head -n 1
  else stripped "$1" | grep -oE "\"$3\":\"[^\"]*\"" | head -n 1 | sed -E 's/^"[^"]+":"//; s/"$//'
  fi
}
json_num() { # json_num FILE JQ_PATH GREP_KEY → first numeric value
  if [ "$use_jq" = 1 ]; then jq -r "$2 // empty" "$1" 2>/dev/null | head -n 1
  else stripped "$1" | grep -oE "\"$3\":-?[0-9]+" | head -n 1 | sed -E 's/^"[^"]+"://'
  fi
}
job_state()   { json_str "$1" '.state' state; }
job_error()   { json_str "$1" '.error' error; }
job_urls() {  # all result file urls
  if [ "$use_jq" = 1 ]; then jq -r '.result.files[]?.url // empty' "$1" 2>/dev/null
  else stripped "$1" | grep -oE '"url":"/out/[^"]+"' | sed -E 's/^"url":"//; s/"$//'
  fi
}
job_cached() { # true if the job manifest says the result was served from the result cache
  if [ "$use_jq" = 1 ]; then jq -e '.result.cached == true' "$1" >/dev/null 2>&1
  else
    # Capture first: with pipefail, `stripped | grep -q` fails on SIGPIPE when
    # grep exits at the match while tr is still writing a large manifest.
    local s; s=$(stripped "$1")
    grep -q '"cached":true' <<<"$s"
  fi
}
job_reports_ok() { # true if every file's report.ok is true (and at least one report exists)
  if [ "$use_jq" = 1 ]; then
    jq -e '(.result.files | length) > 0 and all(.result.files[]; .report.ok == true)' "$1" >/dev/null 2>&1
  else
    # Report.OK is emitted right after the checks array: `],"ok":true` (`null,"ok":…` when empty)
    local s; s=$(stripped "$1")
    grep -qE '(\]|null),"ok":true' <<<"$s" && ! grep -qE '(\]|null),"ok":false' <<<"$s"
  fi
}
# Phase 2 manifests carry several files (primary + alternatives / frames +
# archive); these look at one kind at a time. File.Kind "" or "output" is the
# primary file, which is listed first.
primary_report_ok() { # true if the primary file's report.ok is true
  if [ "$use_jq" = 1 ]; then
    jq -e 'first(.result.files[]? | select((.kind // "") == "" or .kind == "output")) | .report.ok == true' "$1" >/dev/null 2>&1
  else
    [ "$(stripped "$1" | grep -oE '(\]|null),"ok":(true|false)' | head -n 1 | sed -E 's/.*"ok"://')" = true ]
  fi
}
files_of_kind() { # files_of_kind FILE KIND → count of result files with that kind
  if [ "$use_jq" = 1 ]; then jq -r "[.result.files[]? | select(.kind == \"$2\")] | length" "$1" 2>/dev/null
  else stripped "$1" | grep -oE "\"kind\":\"$2\"" | wc -l | tr -d ' '
  fi
}
primary_url() { # url of the primary file (kind ""/"output"), else the first file
  local u=""
  if [ "$use_jq" = 1 ]; then
    u=$(jq -r 'first(.result.files[]? | select((.kind // "") == "" or .kind == "output")) | .url // empty' "$1" 2>/dev/null)
  fi
  [ -n "$u" ] || u=$(job_urls "$1" | head -n 1)
  printf '%s\n' "$u"
}
archive_url() { # url of the "archive" file (frames.zip)
  if [ "$use_jq" = 1 ]; then jq -r 'first(.result.files[]? | select(.kind == "archive")) | .url // empty' "$1" 2>/dev/null
  else job_urls "$1" | grep -E '\.zip$' | head -n 1
  fi
}
# A frames manifest holds exactly frames.zip (kind "archive"), delays.json (the
# per-frame timing table, no kind) and then the frame files in order, so the
# grep fallbacks select the frames by excluding the two fixed names (a plain
# `grep -v .zip` used to hand back delays.json as the "first frame").
frame_urls() { # frame_urls FILE → urls of the "frame" files, in manifest order
  if [ "$use_jq" = 1 ]; then jq -r '.result.files[]? | select(.kind == "frame") | .url // empty' "$1" 2>/dev/null
  else job_urls "$1" | grep -vE '/(frames\.zip|delays\.json)$'
  fi
}
first_frame_url() { # url of the first "frame" file
  frame_urls "$1" | head -n 1
}
frame_url_at() { # frame_url_at FILE first|last → url of the first / last "frame" file
  if [ "$2" = first ]; then frame_urls "$1" | head -n 1
  else frame_urls "$1" | tail -n 1
  fi
}
primary_has_alpha() { # true if the primary file's report says hasAlpha (the rendered frames carry transparency)
  if [ "$use_jq" = 1 ]; then
    jq -e 'first(.result.files[]? | select((.kind // "") == "" or .kind == "output")) | .report.hasAlpha == true' "$1" >/dev/null 2>&1
  else
    # The primary file is listed first, and nothing before the files carries a hasAlpha key.
    local s; s=$(stripped "$1")
    [ "$(grep -oE '"hasAlpha":(true|false)' <<<"$s" | head -n 1)" = '"hasAlpha":true' ]
  fi
}
fonts_has() { # fonts_has FILE FAMILY → true if GET /api/fonts lists the family (exact name)
  if [ "$use_jq" = 1 ]; then jq -e --arg f "$2" 'any(.fonts[]?; .family == $f)' "$1" >/dev/null 2>&1
  else grep -qF "\"family\":\"$2\"" "$1" # Go's encoder writes no spaces around ':' (the family itself may hold spaces)
  fi
}
features_true() { # features_true FILE NAME → true if /api/capabilities says features.NAME == true
  if [ "$use_jq" = 1 ]; then jq -e --arg f "$2" '.features[$f] == true' "$1" >/dev/null 2>&1
  else stripped "$1" | grep -qF "\"$2\":true"
  fi
}

# ---- zip helpers (Go's archive/zip output: no archive comment, no zip64)
zip_entries() { # zip_entries FILE → total entry count from the end-of-central-directory record
  local f=$1 n
  [ "$(fsize "$f")" -ge 22 ] || { echo 0; return; }
  if [ "$(tail -c 22 "$f" | head -c 4 | od -An -tx1 | tr -d ' \n')" != "504b0506" ]; then echo 0; return; fi
  n=$(tail -c 22 "$f" | od -An -tu2 -j 10 -N 2 | tr -d ' \n')
  echo "${n:-0}"
}
zip_check() { # zip_check FILE DIR → extracts/tests the zip; 0 when it is sound
  local f=$1 dir=$2
  if have unzip; then
    unzip -q -o "$f" -d "$dir" >/dev/null 2>&1 && unzip -tqq "$f" >/dev/null 2>&1
  elif have python3; then
    python3 - "$f" "$dir" <<'PY' >/dev/null 2>&1
import sys, zipfile
with zipfile.ZipFile(sys.argv[1]) as z:
    if z.testzip() is not None:
        sys.exit(1)
    z.extractall(sys.argv[2])
PY
  elif have bsdtar; then
    bsdtar -xf "$f" -C "$dir" >/dev/null 2>&1
  else
    # No extractor: local-file-header magic at offset 0 and a sane EOCD record.
    [ "$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')" = "504b0304" ] && [ "$(zip_entries "$f")" -gt 0 ]
  fi
}
zip_tool_note() { # how zip_check verified the archive (for the pass message)
  if have unzip; then echo unzip
  elif have python3; then echo python3
  elif have bsdtar; then echo bsdtar
  else echo "magic+EOCD (no unzip on PATH)"
  fi
}

# ---- media helpers
magic_hex() { head -c "$2" "$1" | od -An -tx1 | tr -d ' \n'; } # magic_hex FILE N
codec_of()  { "$ffprobe" -v error -select_streams v:0 -show_entries stream=codec_name -of default=nw=1:nk=1 "$1" 2>/dev/null | head -n 1; }
dims_of()   { "$ffprobe" -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$1" 2>/dev/null | head -n 1; }
png_chunks() { grep -a -o "$2" "$1" | wc -l | tr -d ' '; } # png_chunks FILE CHUNK → occurrences of the 4-byte chunk name
gif_screen_size() { # gif_screen_size FILE → "WxH" from gifsicle --info ("" when unknown)
  # Capture first: with pipefail, `gifsicle --info | grep -q` fails on SIGPIPE
  # when grep stops reading after the match while gifsicle is still printing
  # per-frame info (same fix as the avifdec Repeat Count check below).
  local info
  info=$("$gifsicle" --info "$1" 2>/dev/null || true)
  sed -nE 's/.*logical screen ([0-9]+x[0-9]+).*/\1/p' <<<"$info" | head -n 1
}
gif_frames() { # gif_frames FILE → frame count (gifsicle, else ffprobe)
  local n=""
  if have "$gifsicle"; then n=$("$gifsicle" --info "$1" 2>/dev/null | sed -nE 's/^.* ([0-9]+) images?.*/\1/p' | head -n 1); fi
  [ -n "$n" ] || n=$("$ffprobe" -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -of default=nw=1:nk=1 "$1" 2>/dev/null | head -n 1)
  echo "${n:-0}"
}
sha_of() { sha256sum "$1" | cut -d' ' -f1; }
# Pixel-level alpha checks (frame 0, decoded by ffmpeg as straight RGBA; the
# gif / webp decoders report transparent pixels as alpha 0). Every pipeline
# stage reads its whole input, so nothing here can die of SIGPIPE.
alpha_counts() { # alpha_counts FILE → "opaque transparent": pixels of frame 0 with alpha 255 / 0
  local vals
  vals=$("$ffmpeg" -v error -nostdin -i "$1" -frames:v 1 -vf "format=rgba,alphaextract" -f rawvideo -pix_fmt gray - 2>/dev/null \
         | od -An -v -tu1 | tr -s ' \n' '\n' | grep -v '^$' || true)
  printf '%s %s\n' "$(grep -cx 255 <<<"$vals")" "$(grep -cx 0 <<<"$vals")"
}
corner_alpha() { # corner_alpha FILE → alpha (0..255) of pixel (0,0) of frame 0
  "$ffmpeg" -v error -nostdin -i "$1" -frames:v 1 -vf "format=rgba,crop=1:1:0:0" -f rawvideo -pix_fmt rgba - 2>/dev/null \
    | od -An -tu1 | awk '{print $4}'
}
psnr_of() { # psnr_of A B → average PSNR between two images in dB ("inf" when identical; "" on failure)
  local out
  out=$("$ffmpeg" -v info -nostdin -i "$1" -i "$2" -lavfi "[0:v]format=rgba[a];[1:v]format=rgba[b];[a][b]psnr" -f null - 2>&1 || true)
  sed -nE 's/.*PSNR.* average:([0-9.]+|inf).*/\1/p' <<<"$out" | tail -n 1
}
frames_match() { # frames_match A B → 0 when the two images are identical (sha256) or near enough (PSNR ≥ 50 dB)
  [ "$(sha_of "$1")" = "$(sha_of "$2")" ] && return 0
  local p; p=$(psnr_of "$1" "$2")
  [ "$p" = inf ] || { [ -n "$p" ] && awk -v p="$p" 'BEGIN { exit !(p >= 50) }'; }
}
avif_frames() { # avif_frames FILE → frame count of an (animated) AVIF
  # ffmpeg's mov demuxer exposes an animated AVIF as up to four streams: the
  # primary still item (colour + alpha, 1 frame each) and the animation tracks
  # (colour + alpha, N frames), so "-select_streams v:0" always says 1. Take
  # avifdec's word when it is here, otherwise the largest nb_frames of any stream.
  local n=""
  if have "$avifdec"; then
    local info
    info=$("$avifdec" --info "$1" 2>/dev/null || true)
    n=$(sed -nE 's/^.* ([0-9]+) frames?$/\1/p' <<<"$info" | head -n 1 || true)
  fi
  if [ -z "$n" ]; then
    n=$("$ffprobe" -v error -show_entries stream=nb_frames -of default=nw=1:nk=1 "$1" 2>/dev/null | sort -n | tail -n 1)
  fi
  if [ -z "$n" ] || [ "$n" = "N/A" ]; then
    n=$("$ffprobe" -v error -count_frames -show_entries stream=nb_read_frames -of default=nw=1:nk=1 "$1" 2>/dev/null | sort -n | tail -n 1)
  fi
  echo "${n:-0}"
}

# ---- HTTP / job helpers
declare -A job_id      # name → job id
declare -A recipe      # name → recipe JSON
declare -A out_file    # name → downloaded primary file

upload() { # upload OUTJSON curl -F args... → 0 on HTTP 200 (message is the caller's)
  local out=$1; shift
  local code
  code=$(curl -sS -o "$out" -w '%{http_code}' --max-time 120 "$@" "$url/api/upload")
  [ "$code" = 200 ] || { cat "$out" >&2; echo "$code"; return 1; }
  echo 200
}
submit_job() { # submit_job NAME → POST recipe[NAME]; records ok/fail; 0 when accepted
  local name=$1 code
  code=$(curl -sS -o "$tmp/job_$name.json" -w '%{http_code}' --max-time 30 \
           -H 'Content-Type: application/json' --data "${recipe[$name]}" "$url/api/jobs")
  case "$code" in
    200|201|202) ;;
    *) cat "$tmp/job_$name.json" >&2; fail "POST /api/jobs ($name) → $code"; return 1 ;;
  esac
  job_id[$name]=$(json_str "$tmp/job_$name.json" '.id' id)
  if [ -z "${job_id[$name]}" ]; then fail "job response ($name) has no id: $(cat "$tmp/job_$name.json")"; return 1; fi
  ok "POST /api/jobs ($name) → $code"
}
wait_job() { # wait_job NAME → polls GET /api/jobs/{id} until done|error|timeout; prints the final state
  local name=$1 id=${job_id[$1]} code state=""
  local deadline=$((SECONDS + timeout))
  while :; do
    code=$(curl -sS -o "$tmp/poll_$name.json" -w '%{http_code}' --max-time 10 "$url/api/jobs/$id")
    [ "$code" = 200 ] || { echo "http:$code"; return; }
    state=$(job_state "$tmp/poll_$name.json")
    case "$state" in done|error) break ;; esac
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 1
  done
  echo "$state"
}
finish_job() { # finish_job NAME → 0 when the job reached "done" (ok/fail recorded)
  local name=$1 state
  [ -n "${job_id[$name]:-}" ] || return 1
  state=$(wait_job "$name")
  case "$state" in
    done)
      if job_cached "$tmp/poll_$name.json"; then
        cached_n=$((cached_n+1))
        log "WARNING: job $name was served from the server's result cache (not re-rendered)"
      fi
      ok "job $name finished"; return 0 ;;
    error)  fail "job $name failed: $(job_error "$tmp/poll_$name.json")" ;;
    http:*) fail "GET /api/jobs/${job_id[$name]} → ${state#http:}" ;;
    *)      fail "job $name still '$state' after ${timeout}s" ;;
  esac
  return 1
}
download() { # download URL OUT [curl args...] → records ok/fail; 0 on HTTP 200 with a body
  local u=$1 out=$2 dl code; shift 2
  case "$u" in http*) dl=$u ;; *) dl="$url$u" ;; esac
  code=$(curl -sS -o "$out" -w '%{http_code}' --max-time 120 "$@" "$dl")
  if [ "$code" = 200 ] && [ -s "$out" ]; then ok "GET $u → 200 ($(fsize "$out") bytes)"; return 0; fi
  fail "GET $u → $code"; return 1
}
fetch_primary() { # fetch_primary NAME EXT → downloads the primary file to out_file[NAME]
  local name=$1 ext=$2 furl
  furl=$(primary_url "$tmp/poll_$name.json")
  [ -n "$furl" ] || { fail "job $name has no result files"; return 1; }
  out_file[$name]="$tmp/out_$name.$ext"
  download "$furl" "${out_file[$name]}"
}
still_png() { # still_png OUT JSON → 0 when POST /api/still answered 200 with a PNG body (message is the caller's)
  local out=$1 body=$2 code
  code=$(curl -sS -o "$out" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' --data "$body" "$url/api/still")
  [ "$code" = 200 ] && [ "$(magic_hex "$out" 8)" = "89504e470d0a1a0a" ]
}

# Selftest hook: integration-test-selftest.sh sources this file with
# EZLG_ITEST_FUNCS_ONLY=1 to unit-test the helpers above without a server.
# Everything below talks to a server / spawns processes.
if [ "${EZLG_ITEST_FUNCS_ONLY:-0}" = 1 ]; then return 0 2>/dev/null || exit 0; fi

# ---- optional: build + start the server in the background
if [ "${EZLG_START_SERVER:-0}" = 1 ]; then
  have go || die "EZLG_START_SERVER=1 needs the Go toolchain (use the dev image)"
  # Always own the data dir: an inherited EZLG_DATA (the dev image bakes
  # EZLG_DATA=/data on the persistent ezlg-data-dev volume) would answer every
  # re-run from the on-disk result cache instead of rendering anything.
  # EZLG_TEST_DATA is the explicit opt-in override.
  export EZLG_DATA=${EZLG_TEST_DATA:-$tmp/data}
  mkdir -p "$EZLG_DATA"
  log "building ezlg ..."
  (cd "$repo" && go build -o "$tmp/ezlg" ./cmd/ezlg) || die "go build failed"
  log "starting server (data=$EZLG_DATA, log=$tmp/server.log)"
  "$tmp/ezlg" serve > "$tmp/server.log" 2>&1 &
  server_pid=$!
fi

# ---- wait for /healthz
log "waiting for $url/healthz"
deadline=$((SECONDS + 60))
until curl -fsS --max-time 2 "$url/healthz" >/dev/null 2>&1; do
  if [ -n "$server_pid" ] && ! kill -0 "$server_pid" 2>/dev/null; then
    cat "$tmp/server.log" >&2 || true
    die "server exited early"
  fi
  [ "$SECONDS" -lt "$deadline" ] || die "server not healthy after 60 s"
  sleep 1
done
ok "GET /healthz"

# =============================================================================
# Phase 1: ProRes → emote GIF + chat WebP
# =============================================================================
log "making test clip"
bash "$here/make-test-clip.sh" "$tmp/src.mov" 2 160x160 premultiplied >/dev/null || die "make-test-clip failed"

# ---- upload
code=$(upload "$tmp/upload.json" -F "file=@$tmp/src.mov") || die "POST /api/upload → $code"
ok "POST /api/upload → 200"
hash=$(json_str "$tmp/upload.json" '.hash' hash)
[[ "$hash" =~ ^[0-9a-f]{64}$ ]] || die "upload response has no sha256 hash: $(cat "$tmp/upload.json")"
log "source hash $hash"

# ---- jobs
recipe[gif]='{"v":1,"sources":["'"$hash"'"],"ops":[{"kind":"unpremultiply"}],"output":{"format":"gif","width":128,"height":128,"fit":"contain","fps":20,"preset":"emote","target":"emote"}}'
recipe[webp]='{"v":1,"sources":["'"$hash"'"],"ops":[{"kind":"unpremultiply"}],"output":{"format":"webp","quality":80,"preset":"chat-webp","target":"attachment"}}'
for fmt in gif webp; do
  submit_job "$fmt" || abort
done

# ---- poll + download
for fmt in gif webp; do
  finish_job "$fmt" || abort
  if job_reports_ok "$tmp/poll_$fmt.json"; then ok "job $fmt report.ok == true"; else fail "job $fmt report.ok != true"; fi
  furl=$(job_urls "$tmp/poll_$fmt.json" | head -n 1)
  [ -n "$furl" ] || die "job $fmt has no result files"
  out_file[$fmt]="$tmp/out.$fmt"
  download "$furl" "${out_file[$fmt]}" || abort
done

# ---- decode / format checks
gif=${out_file[gif]}
if "$ffmpeg" -v error -nostdin -i "$gif" -f null - 2>"$tmp/gif.err"; then ok "gif decodes (ffmpeg)"; else fail "gif does not decode: $(head -c 300 "$tmp/gif.err")"; fi
info=$("$gifsicle" --info "$gif" 2>&1 || true)
if grep -q 'loop forever' <<<"$info"; then ok "gif loops forever"; else fail "gif is not 'loop forever' (gifsicle --info)"; fi
if grep -q 'logical screen 128x128' <<<"$info"; then ok "gif is 128x128"; else fail "gif is not 128x128: $(grep 'logical screen' <<<"$info")"; fi
if [ "$(fsize "$gif")" -le 262144 ]; then ok "gif ≤ 262144 bytes (emote budget)"; else fail "gif exceeds the 256 KiB emote budget"; fi

webp=${out_file[webp]}
if "$ffmpeg" -v error -nostdin -i "$webp" -f null - 2>"$tmp/webp.err"; then ok "webp decodes (ffmpeg)"; else fail "webp does not decode: $(head -c 300 "$tmp/webp.err")"; fi
if have "$webpinfo"; then
  winfo=$("$webpinfo" "$webp" 2>&1 || true)
  vp8x_alpha=$(awk '/Chunk VP8X/ {x=1} x && /Alpha:/ {print $2; exit}' <<<"$winfo")
  vp8x_anim=$(awk '/Chunk VP8X/ {x=1} x && /Animation:/ {print $2; exit}' <<<"$winfo")
  loop=$(awk '/Loop count/ {print $NF; exit}' <<<"$winfo")
  if [ "$vp8x_anim" = 1 ];  then ok "webp VP8X ANIM flag set";     else fail "webp VP8X ANIM flag missing"; fi
  if [ "$vp8x_alpha" = 1 ]; then ok "webp VP8X ALPHA flag set";    else fail "webp VP8X ALPHA flag missing"; fi
  if [ "$loop" = 0 ];       then ok "webp loop count 0 (forever)"; else fail "webp loop count is '${loop:-?}', want 0"; fi
else
  fail "webpinfo not available (install the 'webp' package)"
fi

if [ "$phase2" != 1 ]; then
  summary
  [ "$failn" -eq 0 ]
  exit
fi

# =============================================================================
# Phase 2: fit-to-size, APNG stickers, AVIF, stills, frames, sequences,
# optimise, edit-as-source
# =============================================================================
log "phase 2: extra sources (image sequence, GIF)"
seq_hash=""; gif_hash=""
if bash "$here/make-test-clip.sh" seq "$tmp/seq" 3 96x96 >/dev/null 2>&1 \
   && [ -f "$tmp/seq/f00001.png" ] && [ -f "$tmp/seq/f00002.png" ] && [ -f "$tmp/seq/f00003.png" ]; then
  if code=$(upload "$tmp/upload_seq.json" -F "file=@$tmp/seq/f00001.png" -F "file=@$tmp/seq/f00002.png" -F "file=@$tmp/seq/f00003.png" -F delayMs=100); then
    ok "POST /api/upload (3 PNGs + delayMs=100) → 200"
  else
    fail "POST /api/upload (3 PNGs + delayMs=100) → $code"
  fi
  seq_hash=$(json_str "$tmp/upload_seq.json" '.hash' hash)
  if [[ "$seq_hash" =~ ^[0-9a-f]{64}$ ]]; then
    kind=$(json_str "$tmp/upload_seq.json" '.info.kind' kind)
    count=$(json_num "$tmp/upload_seq.json" '.info.sequence.count' count)
    if [ "$kind" = sequence ]; then ok "sequence upload → info.kind == sequence"; else fail "sequence upload → info.kind '$kind', want sequence"; fi
    if [ "$count" = 3 ]; then ok "sequence upload → info.sequence.count == 3"; else fail "sequence upload → info.sequence.count '$count', want 3"; fi
  else
    [ -z "$seq_hash" ] || fail "sequence upload response has no sha256 hash: $(head -c 300 "$tmp/upload_seq.json")"
    seq_hash=""
  fi
else
  fail "make-test-clip.sh seq (3 PNG frames) failed"
fi

if bash "$here/make-test-clip.sh" "$tmp/src.gif" 2 160x160 >/dev/null 2>&1 && [ -s "$tmp/src.gif" ]; then
  if code=$(upload "$tmp/upload_gif.json" -F "file=@$tmp/src.gif"); then
    ok "POST /api/upload (GIF) → 200"
  else
    fail "POST /api/upload (GIF) → $code"
  fi
  gif_hash=$(json_str "$tmp/upload_gif.json" '.hash' hash)
  [[ "$gif_hash" =~ ^[0-9a-f]{64}$ ]] || { fail "GIF upload response has no sha256 hash"; gif_hash=""; }
else
  fail "make-test-clip.sh (GIF source) failed"
fi

# ---- submit every Phase 2 job up front (the server renders them concurrently)
unp='{"kind":"unpremultiply"}'
recipe[emote-fit]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"gif","width":128,"height":128,"fit":"contain","preset":"emote","target":"emote","fitBytes":262144}}'
recipe[sticker]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"apng","colors":256,"width":320,"height":320,"fit":"contain","preset":"sticker","target":"sticker","fitBytes":524288}}'
recipe[avif]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"avif","quality":60,"preset":"custom","target":"attachment"}}'
recipe[png]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"png","preset":"custom"}}'
recipe[jpeg]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"jpeg","quality":85,"preset":"custom"}}'
recipe[frames]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"frames","frameFormat":"png","preset":"frames"}}'
phase2_jobs=(emote-fit sticker avif png jpeg frames)
if [ -n "$seq_hash" ]; then
  recipe[sequence]='{"v":1,"sources":["'"$seq_hash"'"],"ops":[],"output":{"format":"gif","preset":"custom"}}'
  phase2_jobs+=(sequence)
fi
if [ -n "$gif_hash" ]; then
  recipe[optimize]='{"v":1,"sources":["'"$gif_hash"'"],"ops":[],"output":{"format":"gif","lossy":40,"preset":"optimize","target":"attachment"}}'
  phase2_jobs+=(optimize)
fi
for name in "${phase2_jobs[@]}"; do
  submit_job "$name" || true
done

# ---- emote GIF fitted under 256 KiB (+ alternatives)
name=emote-fit
if finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if [ "$(fsize "$f")" -le 262144 ]; then ok "$name: primary ≤ 262144 bytes"; else fail "$name: primary is $(fsize "$f") bytes > 262144"; fi
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  n_alt=$(files_of_kind "$tmp/poll_$name.json" alternative)
  if [ "${n_alt:-0}" -ge 1 ]; then ok "$name: manifest lists $n_alt alternative(s)"; else fail "$name: manifest lists no alternative files"; fi
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
  if have "$gifsicle" && [ "$(gif_screen_size "$f")" = 128x128 ]; then ok "$name: gif is 128x128"; else fail "$name: gif is not 128x128"; fi
fi

# ---- sticker: indexed APNG fitted under 512 KiB
name=sticker
if finish_job $name && fetch_primary $name png; then
  f=${out_file[$name]}
  if [ "$(fsize "$f")" -le 524288 ]; then ok "$name: primary ≤ 524288 bytes"; else fail "$name: primary is $(fsize "$f") bytes > 524288"; fi
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  if [ "$(magic_hex "$f" 8)" = "89504e470d0a1a0a" ]; then ok "$name: PNG signature"; else fail "$name: not a PNG (magic $(magic_hex "$f" 8)); url $(primary_url "$tmp/poll_$name.json")"; fi
  if [ "$(png_chunks "$f" acTL)" -ge 1 ]; then ok "$name: animated (acTL chunk)"; else fail "$name: no acTL chunk (not an APNG)"; fi
  if [ "$(png_chunks "$f" PLTE)" -ge 1 ] && [ "$(png_chunks "$f" tRNS)" -ge 1 ]; then ok "$name: indexed 8-bit alpha (PLTE + tRNS)"; else fail "$name: PLTE/tRNS missing (PLTE=$(png_chunks "$f" PLTE) tRNS=$(png_chunks "$f" tRNS)) — not the indexed APNG path"; fi
  d=$(dims_of "$f")
  if [ "$d" = 320x320 ]; then ok "$name: 320x320"; else fail "$name: dims '$d', want 320x320"; fi
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>"$tmp/$name.err"; then ok "$name: apng decodes (ffmpeg)"; else fail "$name: apng does not decode: $(head -c 300 "$tmp/$name.err")"; fi
fi

# ---- animated AVIF with alpha
name=avif
if finish_job $name && fetch_primary $name avif; then
  f=${out_file[$name]}
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>"$tmp/$name.err"; then ok "$name: decodes (ffmpeg)"; else fail "$name: does not decode: $(head -c 300 "$tmp/$name.err")"; fi
  n=$(avif_frames "$f")
  if [ "${n:-0}" -gt 1 ]; then ok "$name: animated ($n frames)"; else fail "$name: only '${n:-?}' frame(s) — not animated"; fi
  if have "$avifdec"; then
    # Capture first: with pipefail, `avifdec | grep -q` fails on SIGPIPE when
    # grep stops reading after the match, even though the match succeeded.
    info=$("$avifdec" --info "$f" 2>/dev/null || true)
    if grep -qE 'Repeat Count *: *Infinite' <<<"$info"; then ok "$name: repeats forever (avifdec)"; else fail "$name: avifdec does not report Repeat Count: Infinite"; fi
  fi
fi

# ---- static PNG / JPEG from the clip
name=png
if finish_job $name && fetch_primary $name png; then
  f=${out_file[$name]}
  if [ "$(magic_hex "$f" 8)" = "89504e470d0a1a0a" ]; then ok "$name: PNG signature"; else fail "$name: not a PNG (magic $(magic_hex "$f" 8))"; fi
  if [ "$(codec_of "$f")" = png ] && [ "$(dims_of "$f")" = 160x160 ]; then ok "$name: ffprobe png 160x160"; else fail "$name: ffprobe says '$(codec_of "$f")' $(dims_of "$f")"; fi
  if [ "$(png_chunks "$f" acTL)" -eq 0 ]; then ok "$name: still (no acTL)"; else fail "$name: has an acTL chunk (animated?)"; fi
fi
name=jpeg
if finish_job $name && fetch_primary $name jpg; then
  f=${out_file[$name]}
  if [ "$(magic_hex "$f" 2)" = "ffd8" ]; then ok "$name: JPEG signature"; else fail "$name: not a JPEG (magic $(magic_hex "$f" 2))"; fi
  if [ "$(codec_of "$f")" = mjpeg ] && [ "$(dims_of "$f")" = 160x160 ]; then ok "$name: ffprobe mjpeg 160x160"; else fail "$name: ffprobe says '$(codec_of "$f")' $(dims_of "$f")"; fi
fi

# ---- frame extraction: N frame files + frames.zip
name=frames
if finish_job $name; then
  n_frames=$(files_of_kind "$tmp/poll_$name.json" frame)
  n_arch=$(files_of_kind "$tmp/poll_$name.json" archive)
  if [ "${n_frames:-0}" -ge 2 ]; then ok "$name: manifest lists $n_frames frame files"; else fail "$name: manifest lists ${n_frames:-0} frame files, want ≥ 2"; fi
  if [ "${n_arch:-0}" -ge 1 ]; then ok "$name: manifest lists an archive"; else fail "$name: manifest lists no archive (frames.zip)"; fi
  furl=$(first_frame_url "$tmp/poll_$name.json")
  if [ -n "$furl" ] && download "$furl" "$tmp/frame1.png"; then
    if [ "$(magic_hex "$tmp/frame1.png" 8)" = "89504e470d0a1a0a" ]; then ok "$name: first frame is a PNG"; else fail "$name: first frame is not a PNG"; fi
  fi
  zurl=$(archive_url "$tmp/poll_$name.json")
  if [ -n "$zurl" ] && download "$zurl?dl=1" "$tmp/frames.zip" -D "$tmp/zip.headers"; then
    if grep -qi '^content-disposition: *attachment' "$tmp/zip.headers"; then ok "$name: ?dl=1 sets Content-Disposition: attachment"; else fail "$name: ?dl=1 without Content-Disposition: attachment"; fi
    n_zip=$(zip_entries "$tmp/frames.zip")
    if [ "${n_zip:-0}" -ge "${n_frames:-1}" ]; then ok "$name: zip holds $n_zip entries (≥ $n_frames frames)"; else fail "$name: zip holds ${n_zip:-0} entries, want ≥ ${n_frames:-1}"; fi
    mkdir -p "$tmp/unz"
    if zip_check "$tmp/frames.zip" "$tmp/unz"; then ok "$name: zip is sound ($(zip_tool_note))"; else fail "$name: zip does not extract / verify ($(zip_tool_note))"; fi
  elif [ -z "$zurl" ]; then
    fail "$name: no archive url in the manifest"
  fi
fi

# ---- image sequence → GIF with 3 frames
name=sequence
if [ -n "$seq_hash" ] && finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  n=$(gif_frames "$f")
  if [ "${n:-0}" = 3 ]; then ok "$name: gif has 3 frames"; else fail "$name: gif has '${n:-?}' frames, want 3"; fi
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
fi

# ---- GIF → GIF optimise (gifsicle only)
name=optimize
if [ -n "$gif_hash" ] && finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  in_b=$(fsize "$tmp/src.gif"); out_b=$(fsize "$f")
  if [ "$out_b" -lt "$in_b" ]; then ok "$name: $out_b < $in_b bytes (smaller than the input)"; else fail "$name: $out_b bytes is not smaller than the $in_b byte input"; fi
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
fi

# ---- edit as source: the Phase 1 GIF result becomes a new source
rhash=$(json_str "$tmp/poll_gif.json" '.recipeHash' recipeHash)
rname=$(basename "$(job_urls "$tmp/poll_gif.json" | head -n 1)")
if [ -n "$rhash" ] && [ -n "$rname" ]; then
  code=$(curl -sS -o "$tmp/fromres.json" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' \
           --data '{"recipeHash":"'"$rhash"'","name":"'"$rname"'"}' "$url/api/sources/from-result")
  if [ "$code" = 200 ]; then
    ok "POST /api/sources/from-result → 200"
    fhash=$(json_str "$tmp/fromres.json" '.hash' hash)
    if [[ "$fhash" =~ ^[0-9a-f]{64}$ ]]; then
      ok "from-result: source hash returned"
      code=$(curl -sS -o "$tmp/fromres_get.json" -w '%{http_code}' --max-time 30 "$url/api/sources/$fhash")
      if [ "$code" = 200 ]; then ok "GET /api/sources/{hash} (from-result) → 200"; else fail "GET /api/sources/{hash} (from-result) → $code"; fi
      kind=$(json_str "$tmp/fromres_get.json" '.info.kind' kind)
      if [ "$kind" = animation ]; then ok "from-result: source kind == animation"; else fail "from-result: source kind '$kind', want animation"; fi
    else
      fail "from-result: no sha256 hash in $(head -c 300 "$tmp/fromres.json")"
    fi
  else
    cat "$tmp/fromres.json" >&2
    fail "POST /api/sources/from-result → $code"
  fi
else
  fail "from-result: no recipeHash / file name from the Phase 1 gif job"
fi

if [ "$phase3" != 1 ]; then
  summary
  [ "$failn" -eq 0 ]
  exit
fi

# =============================================================================
# Phase 3: keying, text / image / animated overlays, reverse, autocrop, the
# animated proxy, fonts
# =============================================================================
log "phase 3: capabilities + fonts"
code=$(curl -sS -o "$tmp/caps.json" -w '%{http_code}' --max-time 30 "$url/api/capabilities")
if [ "$code" = 200 ]; then
  for feat in keying overlays proxy fonts; do
    if features_true "$tmp/caps.json" "$feat"; then ok "capabilities: features.$feat == true"; else fail "capabilities: features.$feat is not true"; fi
  done
else
  fail "GET /api/capabilities → $code"
fi
code=$(curl -sS -o "$tmp/fonts.json" -w '%{http_code}' --max-time 30 "$url/api/fonts")
if [ "$code" = 200 ]; then
  ok "GET /api/fonts → 200"
  for fam in "DejaVu Sans" "Noto Sans"; do
    if fonts_has "$tmp/fonts.json" "$fam"; then ok "fonts: lists \"$fam\""; else fail "fonts: \"$fam\" not listed: $(head -c 300 "$tmp/fonts.json")"; fi
  done
else
  fail "GET /api/fonts → $code"
fi

log "phase 3: extra sources (green-screen clip, overlay PNG, 0.5 s overlay GIF, animated WebP from the ProRes clip)"
green_hash=""; png_hash=""; ov_hash=""; webp_hash=""
if bash "$here/make-test-clip.sh" green "$tmp/green.mov" 2 160x160 >/dev/null 2>&1 && [ -s "$tmp/green.mov" ]; then
  if code=$(upload "$tmp/upload_green.json" -F "file=@$tmp/green.mov"); then ok "POST /api/upload (green-screen clip) → 200"; else fail "POST /api/upload (green-screen clip) → $code"; fi
  green_hash=$(json_str "$tmp/upload_green.json" '.hash' hash)
  [[ "$green_hash" =~ ^[0-9a-f]{64}$ ]] || { fail "green-screen upload response has no sha256 hash"; green_hash=""; }
else
  fail "make-test-clip.sh green (green-screen clip) failed"
fi
if bash "$here/make-test-clip.sh" seq "$tmp/ovseq" 1 48x48 >/dev/null 2>&1 && [ -s "$tmp/ovseq/f00001.png" ]; then
  if code=$(upload "$tmp/upload_png.json" -F "file=@$tmp/ovseq/f00001.png"); then ok "POST /api/upload (overlay PNG) → 200"; else fail "POST /api/upload (overlay PNG) → $code"; fi
  png_hash=$(json_str "$tmp/upload_png.json" '.hash' hash)
  [[ "$png_hash" =~ ^[0-9a-f]{64}$ ]] || { fail "overlay PNG upload response has no sha256 hash"; png_hash=""; }
else
  fail "make-test-clip.sh seq (overlay PNG) failed"
fi
if bash "$here/make-test-clip.sh" "$tmp/ov.gif" 0.5 48x48 >/dev/null 2>&1 && [ -s "$tmp/ov.gif" ]; then
  if code=$(upload "$tmp/upload_ov.json" -F "file=@$tmp/ov.gif"); then ok "POST /api/upload (overlay GIF) → 200"; else fail "POST /api/upload (overlay GIF) → $code"; fi
  ov_hash=$(json_str "$tmp/upload_ov.json" '.hash' hash)
  [[ "$ov_hash" =~ ^[0-9a-f]{64}$ ]] || { fail "overlay GIF upload response has no sha256 hash"; ov_hash=""; }
else
  fail "make-test-clip.sh (0.5 s overlay GIF) failed"
fi
# An animated WebP source for the trim case: the 2 s ProRes clip at 10 fps
# through libwebp_anim (the app's own encoder path, -loop 0) = 20 frames, so
# a 0.5–1.5 s trim must yield exactly 10. FFmpeg 9's webp_anim demuxer decodes
# nothing after an input seek; the server trims such sources in the
# filtergraph instead of with -ss/-to.
webp_n=0
if "$ffmpeg" -hide_banner -loglevel error -nostdin -y -i "$tmp/src.mov" -vf "fps=10,format=yuva420p" \
     -c:v libwebp_anim -lossless 0 -q:v 80 -compression_level 4 -loop 0 -map_metadata -1 -f webp "$tmp/src.webp" 2>"$tmp/src_webp.err" \
   && [ -s "$tmp/src.webp" ]; then
  webp_n=$("$ffprobe" -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -of default=nw=1:nk=1 "$tmp/src.webp" 2>/dev/null | head -n 1)
  if [ "${webp_n:-0}" = 20 ]; then ok "animated WebP source has 20 frames (2 s at 10 fps)"; else fail "animated WebP source has '${webp_n:-?}' frames, want 20"; fi
  if code=$(upload "$tmp/upload_webp.json" -F "file=@$tmp/src.webp"); then ok "POST /api/upload (animated WebP) → 200"; else fail "POST /api/upload (animated WebP) → $code"; fi
  webp_hash=$(json_str "$tmp/upload_webp.json" '.hash' hash)
  if [[ "$webp_hash" =~ ^[0-9a-f]{64}$ ]]; then
    kind=$(json_str "$tmp/upload_webp.json" '.info.kind' kind)
    if [ "$kind" = animation ]; then ok "animated WebP upload → info.kind == animation"; else fail "animated WebP upload → info.kind '$kind', want animation"; fi
  else
    fail "animated WebP upload response has no sha256 hash"; webp_hash=""
  fi
else
  fail "ffmpeg could not build the animated WebP source: $(head -c 300 "$tmp/src_webp.err")"
fi

# ---- submit every Phase 3 job up front
text_op='{"kind":"text","params":{"text":"ezlg","font":"DejaVu Sans","size":40,"color":"ffffff","border":2,"borderColor":"000000","x":8,"y":8}}'
ov_png_op='{"kind":"overlay","params":{"source":1,"x":4,"y":4}}'
ov_gif_op='{"kind":"overlay","params":{"source":1,"x":100,"y":100}}'
recipe[text]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"','"$text_op"'],"output":{"format":"gif","preset":"custom","target":"attachment"}}'
recipe[reverse]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"',{"kind":"reverse"}],"output":{"format":"frames","frameFormat":"png","preset":"frames"}}'
recipe[autocrop]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"',{"kind":"trim","params":{"start":0,"end":0.1}},{"kind":"autocrop","params":{"threshold":1}}],"output":{"format":"png","preset":"custom"}}'
phase3_jobs=(text reverse autocrop)
if [ -n "$green_hash" ]; then
  recipe[chromakey]='{"v":1,"sources":["'"$green_hash"'"],"ops":[{"kind":"chromakey","params":{"color":"00ff00"}}],"output":{"format":"gif","width":128,"height":128,"fit":"contain","fps":20,"preset":"emote","target":"emote"}}'
  recipe[colorkey]='{"v":1,"sources":["'"$green_hash"'"],"ops":[{"kind":"colorkey","params":{"color":"00ff00","similarity":0.1}}],"output":{"format":"webp","quality":80,"preset":"chat","target":"attachment"}}'
  phase3_jobs+=(chromakey colorkey)
fi
if [ -n "$png_hash" ]; then
  recipe[overlay-png]='{"v":1,"sources":["'"$hash"'","'"$png_hash"'"],"ops":['"$unp"','"$ov_png_op"'],"output":{"format":"webp","quality":80,"preset":"chat","target":"attachment"}}'
  phase3_jobs+=(overlay-png)
fi
if [ -n "$ov_hash" ]; then
  recipe[overlay-gif]='{"v":1,"sources":["'"$hash"'","'"$ov_hash"'"],"ops":['"$unp"','"$ov_gif_op"'],"output":{"format":"gif","preset":"custom","target":"attachment"}}'
  phase3_jobs+=(overlay-gif)
fi
if [ -n "$webp_hash" ]; then
  recipe[webp-trim]='{"v":1,"sources":["'"$webp_hash"'"],"ops":[{"kind":"trim","params":{"start":0.5,"end":1.5}}],"output":{"format":"frames","frameFormat":"png","preset":"frames"}}'
  phase3_jobs+=(webp-trim)
fi
for name in "${phase3_jobs[@]}"; do
  submit_job "$name" || true
done

# ---- chromakey: green screen → transparent emote GIF
name=chromakey
if [ -n "$green_hash" ] && finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  if primary_has_alpha "$tmp/poll_$name.json"; then ok "$name: report.hasAlpha == true (background keyed out)"; else fail "$name: report.hasAlpha != true"; fi
  read -r opaque transparent <<<"$(alpha_counts "$f")"
  ca=$(corner_alpha "$f")
  if [ "${transparent:-0}" -gt 0 ] && [ "${opaque:-0}" -gt 0 ] && [ "${ca:-255}" = 0 ]; then
    ok "$name: frame 0 has $transparent transparent + $opaque opaque pixels, corner transparent"
  else
    fail "$name: frame 0 alpha: opaque=${opaque:-?} transparent=${transparent:-?} corner=${ca:-?} (want both kinds, corner 0)"
  fi
  if have "$gifsicle" && [ "$(gif_screen_size "$f")" = 128x128 ]; then ok "$name: gif is 128x128"; else fail "$name: gif is not 128x128"; fi
  if [ "$(fsize "$f")" -le 262144 ]; then ok "$name: ≤ 262144 bytes (emote budget)"; else fail "$name: $(fsize "$f") bytes > 262144"; fi
fi

# ---- colorkey: pick-a-colour key → chat WebP with alpha
name=colorkey
if [ -n "$green_hash" ] && finish_job $name && fetch_primary $name webp; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  if primary_has_alpha "$tmp/poll_$name.json"; then ok "$name: report.hasAlpha == true"; else fail "$name: report.hasAlpha != true"; fi
  if have "$webpinfo"; then
    winfo=$("$webpinfo" "$f" 2>&1 || true)
    vp8x_alpha=$(awk '/Chunk VP8X/ {x=1} x && /Alpha:/ {print $2; exit}' <<<"$winfo")
    if [ "$vp8x_alpha" = 1 ]; then ok "$name: webp VP8X ALPHA flag set"; else fail "$name: webp VP8X ALPHA flag missing"; fi
  fi
  read -r opaque transparent <<<"$(alpha_counts "$f")"
  ca=$(corner_alpha "$f")
  if [ "${transparent:-0}" -gt 0 ] && [ "${opaque:-0}" -gt 0 ] && [ "${ca:-255}" = 0 ]; then
    ok "$name: frame 0 has $transparent transparent + $opaque opaque pixels, corner transparent"
  else
    fail "$name: frame 0 alpha: opaque=${opaque:-?} transparent=${transparent:-?} corner=${ca:-?} (want both kinds, corner 0)"
  fi
fi

# ---- text overlay (drawtext via textfile, fontconfig family)
name=text
if finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
  plain='{"src":"'"$hash"'","ops":['"$unp"'],"output":{"format":"gif"},"t":0.5,"maxW":160}'
  withtext='{"src":"'"$hash"'","ops":['"$unp"','"$text_op"'],"output":{"format":"gif"},"t":0.5,"maxW":160}'
  if still_png "$tmp/still_plain.png" "$plain" && still_png "$tmp/still_text.png" "$withtext"; then
    if ! cmp -s "$tmp/still_plain.png" "$tmp/still_text.png"; then ok "$name: still with the text op differs from the plain still (drawtext painted)"; else fail "$name: still with the text op is identical to the plain still"; fi
  else
    fail "$name: POST /api/still (plain / with text) did not answer with a PNG"
  fi
fi

# ---- static image overlay from a second source
name=overlay-png
if [ -n "$png_hash" ] && finish_job $name && fetch_primary $name webp; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: webp decodes (ffmpeg)"; else fail "$name: webp does not decode"; fi
  plain='{"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"webp"},"t":0.5,"maxW":160}'
  withov='{"sources":["'"$hash"'","'"$png_hash"'"],"ops":['"$unp"','"$ov_png_op"'],"output":{"format":"webp"},"t":0.5,"maxW":160}'
  if still_png "$tmp/still_noov.png" "$plain" && still_png "$tmp/still_ov.png" "$withov"; then
    if ! cmp -s "$tmp/still_noov.png" "$tmp/still_ov.png"; then ok "$name: \"sources\" still with the overlay differs from the plain still"; else fail "$name: still with the overlay is identical to the plain still"; fi
  else
    fail "$name: POST /api/still with \"sources\" (plain / with overlay) did not answer with a PNG"
  fi
fi

# ---- looping animated overlay: a 0.5 s GIF over the 2 s clip
name=overlay-gif
if [ -n "$ov_hash" ] && finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
  n=$(gif_frames "$f"); ov_n=$(gif_frames "$tmp/ov.gif")
  if [ "${n:-0}" -gt "${ov_n:-0}" ]; then ok "$name: $n frames > the overlay's own $ov_n (the overlay did not cut the clip short: the base decides the length)"; else fail "$name: $n frames, overlay has $ov_n — the overlay cut the clip short?"; fi
  if [ "${n_frames:-0}" -gt 0 ]; then
    if [ "${n:-0}" = "$n_frames" ]; then ok "$name: frame count == forward frames export ($n)"; else fail "$name: $n frames, forward export has $n_frames"; fi
  fi
  # Pixel checks through POST /api/still. The frame counts above hold for the
  # base clip alone (overlay … shortest=1 lets the base decide the length
  # whatever the overlay does), so they cannot tell a dropped, transparent,
  # off-canvas or non-looping overlay. The overlay's timeline starts at the
  # output's t=0, so at t=0.8 the 0.5 s GIF has wrapped once when it loops
  # (overlay time 0.3 s) and shows its held last frame under noLoop, while at
  # t=0.3 both modes show the same (first-play) overlay frame.
  ov_hold_op='{"kind":"overlay","params":{"source":1,"x":100,"y":100,"noLoop":true}}'
  still_ovg() { # still_ovg OUT T [OVERLAY_OP] → POST /api/still with "sources" [clip, overlay GIF] (message is the caller's)
    local ops=$unp
    [ -z "${3:-}" ] || ops="$unp,$3"
    still_png "$1" '{"sources":["'"$hash"'","'"$ov_hash"'"],"ops":['"$ops"'],"output":{"format":"gif"},"t":'"$2"',"maxW":160}'
  }
  if still_ovg "$tmp/still_ovg_plain8.png" 0.8 \
     && still_ovg "$tmp/still_ovg_loop8.png" 0.8 "$ov_gif_op" && still_ovg "$tmp/still_ovg_hold8.png" 0.8 "$ov_hold_op" \
     && still_ovg "$tmp/still_ovg_loop3.png" 0.3 "$ov_gif_op" && still_ovg "$tmp/still_ovg_hold3.png" 0.3 "$ov_hold_op"; then
    if ! cmp -s "$tmp/still_ovg_plain8.png" "$tmp/still_ovg_loop8.png"; then ok "$name: still at t=0.8 with the overlay differs from the plain still (overlay painted)"; else fail "$name: still at t=0.8 with the overlay is identical to the plain still (overlay dropped, transparent or off-canvas?)"; fi
    if frames_match "$tmp/still_ovg_loop3.png" "$tmp/still_ovg_hold3.png"; then ok "$name: t=0.3: looping and noLoop stills agree (same overlay frame during the first play)"; else fail "$name: t=0.3: looping and noLoop stills differ (PSNR $(psnr_of "$tmp/still_ovg_loop3.png" "$tmp/still_ovg_hold3.png") dB) — the first play is not the same in both modes"; fi
    if ! frames_match "$tmp/still_ovg_loop8.png" "$tmp/still_ovg_hold8.png"; then ok "$name: t=0.8: looping still differs from the noLoop one (the 0.5 s overlay looped instead of holding its last frame)"; else fail "$name: t=0.8: looping and noLoop stills match (PSNR $(psnr_of "$tmp/still_ovg_loop8.png" "$tmp/still_ovg_hold8.png") dB) — the overlay held its last frame instead of looping?"; fi
  else
    fail "$name: POST /api/still with \"sources\" (plain / looping / noLoop overlay at t=0.3 and 0.8) did not answer with a PNG"
  fi
fi

# ---- reverse: the frames export mirrors the forward one
name=reverse
if finish_job $name; then
  n_rev=$(files_of_kind "$tmp/poll_$name.json" frame)
  if [ "${n_rev:-0}" -ge 2 ] && [ "${n_rev:-0}" = "${n_frames:-0}" ]; then ok "$name: $n_rev frame files (== forward export)"; else fail "$name: ${n_rev:-0} frame files, forward export has ${n_frames:-?}"; fi
  rf=$(frame_url_at "$tmp/poll_$name.json" first); rl=$(frame_url_at "$tmp/poll_$name.json" last)
  ff0=$(frame_url_at "$tmp/poll_frames.json" first); fl=$(frame_url_at "$tmp/poll_frames.json" last)
  if [ -n "$rf" ] && [ -n "$rl" ] && [ -n "$ff0" ] && [ -n "$fl" ] \
     && download "$rf" "$tmp/rev_first.png" && download "$rl" "$tmp/rev_last.png" \
     && download "$ff0" "$tmp/fwd_first.png" && download "$fl" "$tmp/fwd_last.png"; then
    if ! frames_match "$tmp/fwd_first.png" "$tmp/fwd_last.png"; then ok "$name: forward first and last frames differ (a reversal is detectable)"; else fail "$name: forward first and last frames are identical — cannot tell a reversal"; fi
    if frames_match "$tmp/rev_first.png" "$tmp/fwd_last.png"; then ok "$name: first frame == forward last frame"; else fail "$name: first frame != forward last frame (PSNR $(psnr_of "$tmp/rev_first.png" "$tmp/fwd_last.png") dB)"; fi
    if frames_match "$tmp/rev_last.png" "$tmp/fwd_first.png"; then ok "$name: last frame == forward first frame"; else fail "$name: last frame != forward first frame (PSNR $(psnr_of "$tmp/rev_last.png" "$tmp/fwd_first.png") dB)"; fi
  else
    fail "$name: could not download the first/last frames of both exports"
  fi
fi

# ---- autocrop: crop to the content box (alpha) of the trimmed clip
name=autocrop
if finish_job $name && fetch_primary $name png; then
  f=${out_file[$name]}
  d=$(dims_of "$f"); w=${d%x*}; h=${d#*x}
  if [ "${w:-0}" -gt 0 ] && [ "$w" -lt 160 ] && [ "${h:-0}" -gt 0 ] && [ "$h" -lt 160 ]; then ok "$name: $d is smaller than the 160x160 source"; else fail "$name: dims '$d', want both sides < 160"; fi
  if [ "${w:-0}" -ge 96 ] && [ "${h:-0}" -ge 96 ]; then ok "$name: $d keeps the content (both sides ≥ 96)"; else fail "$name: $d is too small — cropped into the content?"; fi
  if [ "$(magic_hex "$f" 8)" = "89504e470d0a1a0a" ]; then ok "$name: PNG signature"; else fail "$name: not a PNG"; fi
fi

# ---- trim on an animated WebP source (filter-level trim: webp_anim cannot be seeked)
name=webp-trim
if [ -n "$webp_hash" ] && finish_job $name; then
  n_wt=$(files_of_kind "$tmp/poll_$name.json" frame)
  if [ "${n_wt:-0}" = 10 ]; then
    ok "$name: trim 0.5–1.5 s of the 10 fps WebP → exactly 10 frame files"
  else
    fail "$name: ${n_wt:-0} frame files, want exactly 10 = (1.5-0.5) s × 10 fps (0: the demuxer was seeked and decoded nothing; ${webp_n:-20}: the trim was ignored)"
  fi
  furl=$(first_frame_url "$tmp/poll_$name.json")
  if [ -n "$furl" ] && download "$furl" "$tmp/webp_trim_f1.png"; then
    if [ "$(magic_hex "$tmp/webp_trim_f1.png" 8)" = "89504e470d0a1a0a" ] && [ "$(dims_of "$tmp/webp_trim_f1.png")" = 160x160 ]; then ok "$name: first frame is a 160x160 PNG"; else fail "$name: first frame is not a 160x160 PNG ($(dims_of "$tmp/webp_trim_f1.png"))"; fi
  fi
fi

# ---- animated proxy (the Play preview)
log "phase 3: animated proxy (POST /api/proxy)"
code=$(curl -sS -o "$tmp/proxy.webp" -D "$tmp/proxy.headers" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' \
         --data '{"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"gif"},"maxW":120,"maxSeconds":1}' "$url/api/proxy")
if [ "$code" = 200 ]; then
  ok "POST /api/proxy → 200 ($(fsize "$tmp/proxy.webp") bytes)"
  if grep -qi '^content-type: *image/webp' "$tmp/proxy.headers"; then ok "proxy: Content-Type image/webp"; else fail "proxy: Content-Type is not image/webp: $(grep -i '^content-type' "$tmp/proxy.headers")"; fi
  if "$ffmpeg" -v error -nostdin -i "$tmp/proxy.webp" -f null - 2>/dev/null; then ok "proxy: decodes (ffmpeg)"; else fail "proxy: does not decode"; fi
  if have "$webpinfo"; then
    winfo=$("$webpinfo" "$tmp/proxy.webp" 2>&1 || true)
    anim=$(awk '/Chunk VP8X/ {x=1} x && /Animation:/ {print $2; exit}' <<<"$winfo")
    if [ "$anim" = 1 ]; then ok "proxy: VP8X ANIM flag set"; else fail "proxy: VP8X ANIM flag missing"; fi
  fi
  d=$(dims_of "$tmp/proxy.webp"); w=${d%x*}
  if [ "${w:-0}" -gt 0 ] && [ "$w" -le 120 ]; then ok "proxy: width $w ≤ maxW 120 ($d)"; else fail "proxy: dims '$d', want width ≤ 120"; fi
  n=$("$ffprobe" -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -of default=nw=1:nk=1 "$tmp/proxy.webp" 2>/dev/null | head -n 1)
  if [ "${n:-0}" -gt 1 ]; then ok "proxy: animated ($n frames)"; else fail "proxy: '${n:-?}' frame(s) — not animated"; fi
else
  cat "$tmp/proxy.webp" >&2
  fail "POST /api/proxy → $code"
fi

summary
[ "$failn" -eq 0 ]
