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

echo "== integration-test selftest: $pass passed, $failn failed =="
[ "$failn" -eq 0 ]
