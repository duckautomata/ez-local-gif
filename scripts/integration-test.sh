#!/usr/bin/env bash
# integration-test.sh — end-to-end smoke test against a running ezlg server.
#
# Makes a 2 s 160×160 premultiplied ProRes 4444 clip, uploads it, renders a
# Discord emote GIF (128×128) and a chat WebP through the HTTP API, downloads
# both and checks them with ffmpeg / gifsicle / webpinfo. Exit status is
# non-zero if anything fails; a summary is printed at the end.
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
#                       background (EZLG_DATA defaults to a temp dir), wait for
#                       /healthz, and stop it on exit
#   EZLG_TEST_TIMEOUT   seconds to wait for each job (default 120)
#   EZLG_TEST_KEEP      1 → keep the temp dir (printed at the end)
#   EZLG_FFMPEG / EZLG_FFPROBE / EZLG_GIFSICLE / EZLG_WEBPINFO  tool overrides
set -uo pipefail

url=${EZLG_URL:-http://localhost:8080}
url=${url%/}
timeout=${EZLG_TEST_TIMEOUT:-120}
ffmpeg=${EZLG_FFMPEG:-ffmpeg}
gifsicle=${EZLG_GIFSICLE:-gifsicle}
webpinfo=${EZLG_WEBPINFO:-webpinfo}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)

tmp=$(mktemp -d)
server_pid=""
pass=0; failn=0
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

log()  { printf '[itest] %s\n' "$*"; }
ok()   { pass=$((pass+1));  results+=("PASS  $*"); printf '[itest]   pass: %s\n' "$*"; }
fail() { failn=$((failn+1)); results+=("FAIL  $*"); printf '[itest]   FAIL: %s\n' "$*" >&2; }
die()  { fail "$*"; summary; exit 1; }
summary() {
  echo
  echo "== integration test summary ($pass passed, $failn failed) =="
  printf '  %s\n' "${results[@]}"
}
have() { command -v "$1" >/dev/null 2>&1; }

# ---- tiny JSON helpers (jq when present, otherwise regex on whitespace-stripped JSON)
use_jq=0; have jq && use_jq=1
json_str() { # json_str FILE JQ_PATH GREP_KEY → first string value
  if [ "$use_jq" = 1 ]; then jq -r "$2 // empty" "$1" 2>/dev/null | head -n 1
  else tr -d ' \n\r\t' < "$1" | grep -oE "\"$3\":\"[^\"]*\"" | head -n 1 | sed -E 's/^"[^"]+":"//; s/"$//'
  fi
}
job_state()   { json_str "$1" '.state' state; }
job_error()   { json_str "$1" '.error' error; }
job_urls() {  # all result file urls
  if [ "$use_jq" = 1 ]; then jq -r '.result.files[]?.url // empty' "$1" 2>/dev/null
  else tr -d ' \n\r\t' < "$1" | grep -oE '"url":"/out/[^"]+"' | sed -E 's/^"url":"//; s/"$//'
  fi
}
job_reports_ok() { # true if every file's report.ok is true (and at least one report exists)
  if [ "$use_jq" = 1 ]; then
    jq -e '(.result.files | length) > 0 and all(.result.files[]; .report.ok == true)' "$1" >/dev/null 2>&1
  else
    # Report.OK is emitted right after the checks array: `],"ok":true`
    local s; s=$(tr -d ' \n\r\t' < "$1")
    grep -qE '\],"ok":true' <<<"$s" && ! grep -qE '\],"ok":false' <<<"$s"
  fi
}

# ---- optional: build + start the server in the background
if [ "${EZLG_START_SERVER:-0}" = 1 ]; then
  have go || die "EZLG_START_SERVER=1 needs the Go toolchain (use the dev image)"
  export EZLG_DATA=${EZLG_DATA:-$tmp/data}
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

# ---- test clip
log "making test clip"
bash "$here/make-test-clip.sh" "$tmp/src.mov" 2 160x160 premultiplied >/dev/null || die "make-test-clip failed"

# ---- upload
code=$(curl -sS -o "$tmp/upload.json" -w '%{http_code}' --max-time 60 -F "file=@$tmp/src.mov" "$url/api/upload")
if [ "$code" = 200 ]; then ok "POST /api/upload → 200"; else cat "$tmp/upload.json" >&2; die "POST /api/upload → $code"; fi
hash=$(json_str "$tmp/upload.json" '.hash' hash)
[[ "$hash" =~ ^[0-9a-f]{64}$ ]] || die "upload response has no sha256 hash: $(cat "$tmp/upload.json")"
log "source hash $hash"

# ---- jobs
declare -A recipe
recipe[gif]='{"v":1,"sources":["'"$hash"'"],"ops":[{"kind":"unpremultiply"}],"output":{"format":"gif","width":128,"height":128,"fit":"contain","fps":20,"preset":"emote","target":"emote"}}'
recipe[webp]='{"v":1,"sources":["'"$hash"'"],"ops":[{"kind":"unpremultiply"}],"output":{"format":"webp","quality":80,"preset":"chat-webp","target":"attachment"}}'

declare -A job_id
for fmt in gif webp; do
  code=$(curl -sS -o "$tmp/job_$fmt.json" -w '%{http_code}' --max-time 30 \
           -H 'Content-Type: application/json' --data "${recipe[$fmt]}" "$url/api/jobs")
  case "$code" in
    200|201|202) ok "POST /api/jobs ($fmt) → $code" ;;
    *) cat "$tmp/job_$fmt.json" >&2; die "POST /api/jobs ($fmt) → $code" ;;
  esac
  job_id[$fmt]=$(json_str "$tmp/job_$fmt.json" '.id' id)
  [ -n "${job_id[$fmt]}" ] || die "job response ($fmt) has no id: $(cat "$tmp/job_$fmt.json")"
done

# ---- poll
declare -A out_file
for fmt in gif webp; do
  id=${job_id[$fmt]}
  deadline=$((SECONDS + timeout))
  state=""
  while :; do
    code=$(curl -sS -o "$tmp/poll_$fmt.json" -w '%{http_code}' --max-time 10 "$url/api/jobs/$id")
    [ "$code" = 200 ] || die "GET /api/jobs/$id → $code"
    state=$(job_state "$tmp/poll_$fmt.json")
    case "$state" in
      done|error) break ;;
    esac
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 1
  done
  case "$state" in
    done)  ok "job $fmt finished" ;;
    error) die "job $fmt failed: $(job_error "$tmp/poll_$fmt.json")" ;;
    *)     die "job $fmt still '$state' after ${timeout}s" ;;
  esac
  if job_reports_ok "$tmp/poll_$fmt.json"; then ok "job $fmt report.ok == true"; else fail "job $fmt report.ok != true"; fi

  # download the first file
  furl=$(job_urls "$tmp/poll_$fmt.json" | head -n 1)
  [ -n "$furl" ] || die "job $fmt has no result files"
  case "$furl" in http*) dl=$furl ;; *) dl="$url$furl" ;; esac
  out_file[$fmt]="$tmp/out.$fmt"
  code=$(curl -sS -o "${out_file[$fmt]}" -w '%{http_code}' --max-time 60 "$dl")
  if [ "$code" = 200 ] && [ -s "${out_file[$fmt]}" ]; then
    ok "GET $furl → 200 ($(stat -c %s "${out_file[$fmt]}") bytes)"
  else
    die "GET $furl → $code"
  fi
done

# ---- decode / format checks
gif=${out_file[gif]}
if "$ffmpeg" -v error -nostdin -i "$gif" -f null - 2>"$tmp/gif.err"; then ok "gif decodes (ffmpeg)"; else fail "gif does not decode: $(head -c 300 "$tmp/gif.err")"; fi
info=$("$gifsicle" --info "$gif" 2>&1 || true)
if grep -q 'loop forever' <<<"$info"; then ok "gif loops forever"; else fail "gif is not 'loop forever' (gifsicle --info)"; fi
if grep -q 'logical screen 128x128' <<<"$info"; then ok "gif is 128x128"; else fail "gif is not 128x128: $(grep 'logical screen' <<<"$info")"; fi
if [ "$(stat -c %s "$gif")" -le 262144 ]; then ok "gif ≤ 262144 bytes (emote budget)"; else fail "gif exceeds the 256 KiB emote budget"; fi

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

summary
[ "$failn" -eq 0 ]
