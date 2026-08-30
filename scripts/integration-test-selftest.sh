#!/usr/bin/env bash
# integration-test-selftest.sh — fast unit tests for scripts/integration-test.sh.
# No server and no toolchain needed: bash + coreutils only (jq optional — the
# jq branch of the helpers is only tested when jq is on PATH).
#
# Guards:
#   1. EZLG_START_SERVER=1 must ignore an inherited EZLG_DATA. The dev image
#      bakes EZLG_DATA=/data (the persistent ezlg-data-dev volume); if the
#      test honoured it, every re-run would be answered from the on-disk
#      result cache (jobs.Submit fast path) and the suite would pass without
#      rendering anything.
#   2. EZLG_TEST_DATA is the explicit opt-in override for the spawned
#      server's data dir.
#   3. job_cached() spots '"result":{"cached":true,...}' job manifests — with
#      jq and with the grep fallback — so cache hits are counted and warned
#      about (and fail the run under EZLG_TEST_STRICT=1).
#   4. The capture-then-grep helpers survive tool output larger than the pipe
#      buffer: under pipefail a `tool | grep -q` pipeline returns 141 when
#      grep exits at the match while the tool is still writing (SIGPIPE), so
#      job_cached must read a large manifest and gif_screen_size a chatty
#      `gifsicle --info` without reporting a false failure.
#   5. The frames-manifest helpers (first_frame_url / frame_url_at / frame_urls)
#      pick the "frame" files only. A frames manifest lists frames.zip
#      (archive), then delays.json (the timing table, no kind), then the
#      frames; the grep fallback used to hand back delays.json as the "first
#      frame", failing the Phase 2 PNG check and the Phase 3 reverse case
#      whenever jq was absent.
#   6. alpha_mid_count (the Phase 3 feather case) counts only alpha values
#      strictly between 32 and 224 — 32 and 224 themselves are out — checked
#      against an ffmpeg stub that emits a known raw alpha plane.
#   7. The Phase 4 manifest helpers: primary_desc must return the PRIMARY
#      file's desc (kind "output", listed first) and not an alternative's,
#      primary_check_ok must find a lint-report row by rule id only when its
#      ok is true, and input_lists must match a /api/input entry by exact
#      name — each with jq and with the grep fallback.
#   8. moov_before_mdat (the Phase 4 mp4 case) orders the first moov/mdat
#      byte offsets: moov-first passes, mdat-first and boxless files fail.
#
#   bash scripts/integration-test-selftest.sh
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
itest=$here/integration-test.sh
stmp=$(mktemp -d)
trap 'rm -rf "$stmp"' EXIT

pass=0; failn=0
ok()   { pass=$((pass+1));  printf '[selftest] pass: %s\n' "$*"; }
fail() { failn=$((failn+1)); printf '[selftest] FAIL: %s\n' "$*" >&2; }

# ---- a stub `go` that records $EZLG_DATA and fails, so integration-test.sh
# stops right after choosing the data dir (`die "go build failed"`) without
# building or contacting anything.
mkdir -p "$stmp/bin"
cat > "$stmp/bin/go" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "${EZLG_DATA:-}" >> "$RECORD"
exit 1
SH
chmod +x "$stmp/bin/go"

record_data_dir() { # record_data_dir RECORDFILE ENV=VAL... → the EZLG_DATA the spawned server would get
  local rec=$1; shift
  : > "$rec"
  # the run is expected to fail (the stub go exits 1); the record is what matters
  env "$@" RECORD="$rec" PATH="$stmp/bin:$PATH" EZLG_START_SERVER=1 \
      bash "$itest" >/dev/null 2>&1
  head -n 1 "$rec"
}

# ---- 1. inherited EZLG_DATA (dev image: /data) must be ignored
sentinel=$stmp/inherited-data
mkdir -p "$sentinel"
got=$(record_data_dir "$stmp/rec1" EZLG_DATA="$sentinel")
if [ -z "$got" ]; then
  fail "stub go never ran (no EZLG_DATA recorded)"
elif [ "$got" = "$sentinel" ]; then
  fail "EZLG_START_SERVER=1 honoured the inherited EZLG_DATA ($got) — re-runs would be served from the result cache"
else
  ok "EZLG_START_SERVER=1 ignores inherited EZLG_DATA (used $got)"
fi

# ---- 2. EZLG_TEST_DATA is the explicit override
override=$stmp/override-data
got=$(record_data_dir "$stmp/rec2" EZLG_DATA="$sentinel" EZLG_TEST_DATA="$override")
if [ "$got" = "$override" ]; then
  ok "EZLG_TEST_DATA overrides the fresh temp data dir"
else
  fail "EZLG_TEST_DATA not honoured (got '$got', want $override)"
fi

# ---- 3. job_cached() on real-shaped job manifests
printf '%s' '{"id":"j1","state":"done","message":"served from cache","result":{"cached":true,"renderMs":0,"files":[]}}' > "$stmp/cached.json"
printf '%s' '{"id":"j2","state":"done","result":{"cached":false,"renderMs":278,"files":[]}}' > "$stmp/fresh.json"

check_cached() { # check_cached USE_JQ FILE → job_cached's exit status
  local mode=$1 file=$2
  (
    export EZLG_ITEST_FUNCS_ONLY=1
    # shellcheck disable=SC1090,SC1091  # sourced for its function definitions only
    . "$itest"
    # shellcheck disable=SC2034  # read by job_cached, defined in the sourced file
    use_jq=$mode
    job_cached "$file"
  )
}

if check_cached 0 "$stmp/cached.json"; then ok "job_cached (grep) detects cached:true"; else fail "job_cached (grep) misses cached:true"; fi
if check_cached 0 "$stmp/fresh.json";  then fail "job_cached (grep) false positive on cached:false"; else ok "job_cached (grep) ignores cached:false"; fi
if command -v jq >/dev/null 2>&1; then
  if check_cached 1 "$stmp/cached.json"; then ok "job_cached (jq) detects cached:true"; else fail "job_cached (jq) misses cached:true"; fi
  if check_cached 1 "$stmp/fresh.json";  then fail "job_cached (jq) false positive on cached:false"; else ok "job_cached (jq) ignores cached:false"; fi
else
  printf '[selftest] note: jq not on PATH — the jq branch of job_cached was not tested\n'
fi

# ---- 4. capture-then-grep helpers vs SIGPIPE under pipefail (DS-2): a
# `tool | grep -q` pipeline returns 141 when grep exits at the match while the
# tool is still writing, so the helpers must capture the whole output first.

# job_cached (grep fallback) on a manifest much larger than the 64 KiB pipe
# buffer, with the match right at the front.
{
  printf '%s' '{"id":"j3","state":"done","result":{"cached":true,"renderMs":1,"files":['
  yes '{"kind":"frame","url":"/out/aaaaaaaaaaaa/f00001.png"},' | head -n 20000 | tr -d '\n'
  printf '%s' '{"kind":"archive","url":"/out/aaaaaaaaaaaa/frames.zip"}]}}'
} > "$stmp/cached-big.json"
if check_cached 0 "$stmp/cached-big.json"; then
  ok "job_cached (grep) reads a manifest larger than the pipe buffer"
else
  fail "job_cached (grep) fails on a large manifest (SIGPIPE under pipefail?)"
fi

# gif_screen_size against a gifsicle stand-in that prints the logical-screen
# line first and then ~1 MiB of per-frame info; its exit status is the big
# writer's, so it dies of SIGPIPE (141) exactly like the real tool would if
# the reader went away after the match.
cat > "$stmp/bin/gifsicle" <<'SH'
#!/usr/bin/env bash
echo '* stub.gif 500 images'
echo '  logical screen 128x128'
yes '  + image #1 128x128 transparent 0 disposal asis delay 0.04s' | head -n 20000
SH
chmod +x "$stmp/bin/gifsicle"
size=$(
  export EZLG_ITEST_FUNCS_ONLY=1 EZLG_GIFSICLE="$stmp/bin/gifsicle" EZLG_TEST_KEEP=0
  # shellcheck disable=SC1090,SC1091  # sourced for its function definitions only
  . "$itest"
  gif_screen_size ignored.gif
)
if [ "$size" = 128x128 ]; then
  ok "gif_screen_size reads a chatty gifsicle --info (capture, then grep)"
else
  fail "gif_screen_size got '$size', want 128x128 (SIGPIPE under pipefail?)"
fi

# ---- 5. frames-manifest helpers on a real-shaped frames job manifest (DS3-1):
# the archive comes first, then delays.json (kind omitted), then the frames.
# The helpers must skip both fixed names, with jq and with the grep fallback.
printf '%s' '{"id":"j4","state":"done","recipeHash":"bbbbbbbbbbbb","result":{"cached":false,"renderMs":42,"files":['\
'{"name":"frames.zip","url":"/out/bbbbbbbbbbbb/frames.zip","bytes":3000,"format":"zip","kind":"archive","desc":"2 frames (png)","width":160,"height":160,"frames":2,"fps":30,"duration":0.0666},'\
'{"name":"delays.json","url":"/out/bbbbbbbbbbbb/delays.json","bytes":80,"format":"json","desc":"per-frame timing","frames":2,"fps":30},'\
'{"name":"f00001.png","url":"/out/bbbbbbbbbbbb/f00001.png","bytes":1500,"format":"png","kind":"frame","index":1,"desc":"frame 1 (0.00 s)","width":160,"height":160,"frames":1},'\
'{"name":"f00002.png","url":"/out/bbbbbbbbbbbb/f00002.png","bytes":1500,"format":"png","kind":"frame","index":2,"desc":"frame 2 (0.03 s)","width":160,"height":160,"frames":1}'\
']}}' > "$stmp/frames.json"

frames_helper() { # frames_helper USE_JQ HELPER ARGS... → the helper's stdout
  local mode=$1; shift
  (
    export EZLG_ITEST_FUNCS_ONLY=1
    # shellcheck disable=SC1090,SC1091  # sourced for its function definitions only
    . "$itest"
    # shellcheck disable=SC2034  # read by the helpers, defined in the sourced file
    use_jq=$mode
    "$@"
  )
}
check_frames_helpers() { # check_frames_helpers USE_JQ LABEL
  local mode=$1 label=$2 got want
  want=/out/bbbbbbbbbbbb/f00001.png
  got=$(frames_helper "$mode" first_frame_url "$stmp/frames.json")
  if [ "$got" = "$want" ]; then ok "first_frame_url ($label) skips frames.zip and delays.json"; else fail "first_frame_url ($label) got '$got', want $want"; fi
  got=$(frames_helper "$mode" frame_url_at "$stmp/frames.json" first)
  if [ "$got" = "$want" ]; then ok "frame_url_at first ($label) is the first frame"; else fail "frame_url_at first ($label) got '$got', want $want"; fi
  want=/out/bbbbbbbbbbbb/f00002.png
  got=$(frames_helper "$mode" frame_url_at "$stmp/frames.json" last)
  if [ "$got" = "$want" ]; then ok "frame_url_at last ($label) is the last frame"; else fail "frame_url_at last ($label) got '$got', want $want"; fi
  got=$(frames_helper "$mode" frame_urls "$stmp/frames.json" | tr '\n' ' ')
  want="/out/bbbbbbbbbbbb/f00001.png /out/bbbbbbbbbbbb/f00002.png "
  if [ "$got" = "$want" ]; then ok "frame_urls ($label) lists exactly the frame files in order"; else fail "frame_urls ($label) got '$got', want '$want'"; fi
  got=$(frames_helper "$mode" archive_url "$stmp/frames.json")
  if [ "$got" = /out/bbbbbbbbbbbb/frames.zip ]; then ok "archive_url ($label) is frames.zip"; else fail "archive_url ($label) got '$got'"; fi
}
check_frames_helpers 0 grep
if command -v jq >/dev/null 2>&1; then
  check_frames_helpers 1 jq
else
  printf '[selftest] note: jq not on PATH — the jq branch of the frames-manifest helpers was not tested\n'
fi

# ---- 7. Phase 4 manifest helpers on a real-shaped output manifest: the
# primary (kind "output") is listed first with its lint report, an
# alternative follows with a different desc.
printf '%s' '{"id":"j5","state":"done","recipeHash":"cccccccccccc","result":{"cached":false,"renderMs":9,"files":['\
'{"name":"out.gif","url":"/out/cccccccccccc/out.gif","bytes":2000,"format":"gif","kind":"output","desc":"lossless gifsicle (no re-encode)","report":{"rulesVersion":"v","format":"gif","checks":[{"rule":"gif.netscape-loop","level":"error","ok":true,"fixed":false,"detail":""},{"rule":"video.faststart","level":"error","ok":true,"fixed":false,"detail":"moov precedes mdat"}],"ok":true}},'\
'{"name":"alt1.gif","url":"/out/cccccccccccc/alt1.gif","bytes":1500,"format":"gif","kind":"alternative","desc":"fit at 20 fps"}'\
']}}' > "$stmp/primary.json"
printf '%s' '{"id":"j6","state":"done","result":{"files":[{"name":"out.mp4","url":"/out/dddddddddddd/out.mp4","kind":"output","report":{"checks":[{"rule":"video.faststart","level":"error","ok":false,"fixed":false,"detail":"mdat first"}],"ok":false}}]}}' > "$stmp/checkfail.json"
printf '%s' '{"files":[{"name":"clip.mov","size":123,"mtime":"2026-08-29T10:00:00Z"},{"name":"other.gif","size":5,"mtime":"2026-08-29T10:00:00Z"}]}' > "$stmp/input.json"

check_phase4_helpers() { # check_phase4_helpers USE_JQ LABEL
  local mode=$1 label=$2 got
  got=$(frames_helper "$mode" primary_desc "$stmp/primary.json")
  if [ "$got" = "lossless gifsicle (no re-encode)" ]; then
    ok "primary_desc ($label) returns the primary's desc, not the alternative's"
  else
    fail "primary_desc ($label) got '$got', want 'lossless gifsicle (no re-encode)'"
  fi
  if frames_helper "$mode" primary_check_ok "$stmp/primary.json" video.faststart >/dev/null; then
    ok "primary_check_ok ($label) finds a passing rule row"
  else
    fail "primary_check_ok ($label) misses the passing video.faststart row"
  fi
  if frames_helper "$mode" primary_check_ok "$stmp/checkfail.json" video.faststart >/dev/null; then
    fail "primary_check_ok ($label) accepts a failing rule row (ok:false)"
  else
    ok "primary_check_ok ($label) rejects a failing rule row"
  fi
  if frames_helper "$mode" primary_check_ok "$stmp/frames.json" video.faststart >/dev/null; then
    fail "primary_check_ok ($label) claims a row on a manifest without reports"
  else
    ok "primary_check_ok ($label) rejects a manifest without that rule"
  fi
  if frames_helper "$mode" input_lists "$stmp/input.json" clip.mov >/dev/null; then
    ok "input_lists ($label) finds clip.mov"
  else
    fail "input_lists ($label) misses clip.mov"
  fi
  if frames_helper "$mode" input_lists "$stmp/input.json" missing.mov >/dev/null; then
    fail "input_lists ($label) false positive on missing.mov"
  else
    ok "input_lists ($label) rejects an unlisted name"
  fi
}
check_phase4_helpers 0 grep
if command -v jq >/dev/null 2>&1; then
  check_phase4_helpers 1 jq
else
  printf '[selftest] note: jq not on PATH — the jq branch of the Phase 4 helpers was not tested\n'
fi

# ---- 8. moov_before_mdat on synthetic byte streams (the helper reads raw
# offsets — no real mp4 needed).
printf 'xxxxftypisomAAmoovAAAAAAmdatBBBB' > "$stmp/good.mp4"
printf 'xxxxftypisomAAmdatBBBBAAmoovAAAA' > "$stmp/bad.mp4"
printf 'no boxes here at all............' > "$stmp/none.mp4"
if frames_helper 0 moov_before_mdat "$stmp/good.mp4" >/dev/null; then
  ok "moov_before_mdat passes a moov-first file"
else
  fail "moov_before_mdat fails a moov-first file"
fi
if frames_helper 0 moov_before_mdat "$stmp/bad.mp4" >/dev/null; then
  fail "moov_before_mdat passes an mdat-first file"
else
  ok "moov_before_mdat rejects an mdat-first file"
fi
if frames_helper 0 moov_before_mdat "$stmp/none.mp4" >/dev/null; then
  fail "moov_before_mdat passes a file without moov/mdat"
else
  ok "moov_before_mdat rejects a file without moov/mdat"
fi

# ---- 6. alpha_mid_count (the feather case) on a known alpha plane, via an
# ffmpeg stub that prints the raw gray bytes the real decode pipeline would:
# of 0, 32, 33, 128, 223, 224, 255 only 33/128/223 are *strictly* between
# 32 and 224, so the count must be exactly 3.
cat > "$stmp/bin/ffmpeg" <<'SH'
#!/usr/bin/env bash
printf '\x00\x20\x21\x80\xdf\xe0\xff'
SH
chmod +x "$stmp/bin/ffmpeg"
got=$(
  export EZLG_ITEST_FUNCS_ONLY=1 EZLG_FFMPEG="$stmp/bin/ffmpeg"
  # shellcheck disable=SC1090,SC1091  # sourced for its function definitions only
  . "$itest"
  alpha_mid_count ignored.webp
)
if [ "$got" = 3 ]; then
  ok "alpha_mid_count counts only alpha strictly between 32 and 224 (3 of 0,32,33,128,223,224,255)"
else
  fail "alpha_mid_count got '$got', want 3 (bounds must be exclusive)"
fi

echo "== integration-test selftest: $pass passed, $failn failed =="
[ "$failn" -eq 0 ]
