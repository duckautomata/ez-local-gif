#!/usr/bin/env bash
# testkit-test.sh — self-test for discord-testkit.sh / make-test-clip.sh.
#
#   scripts/testkit-test.sh                       # host: tool-free checks + make-test-clip
#   docker compose -f compose.yaml -f compose.dev.yaml run --rm app bash scripts/testkit-test.sh
#   docker run --rm --shm-size 1g -v "$PWD:/src" -w /src ezlg-dev bash scripts/testkit-test.sh
#
# Checks (each prints PASS/FAIL/SKIP; exit status is non-zero if anything FAILs):
#   1. OUTDIR guard — an OUTDIR that cannot be created makes the kit exit 1 at
#      once with the ./output ownership hint (chown one-liner), before it needs
#      any media tool.
#   2. make-test-clip.sh honours FPS (r_frame_rate of the ProRes clip).
#   3. Synthetic source cadence — with no SRC the clip is generated at the master
#      rate, so the master has exactly as many frames as the source, the README
#      says "@ N fps → master N fps" and carries no resample note, and no
#      resample warning is printed. Same run: every variant file exists (incl.
#      e2 lossy-bgra WebP), the ProRes source is unpremultiplied at its native
#      depth (gbrap10le/gbrap12le), and scratch sits on /dev/shm/ezl-testkit
#      (or the tiny-/dev/shm warning is printed) and is cleaned up.
#   4. Resample warning — a SRC whose rate differs from --fps yields the warning
#      on stderr, the README note and a master with a different frame count.
# 3 and 4 run the whole matrix (~15 s each) and need the toolchain image
# (gifsicle, gifski, pngquant, ...); elsewhere they are SKIPped.
#
# Env: EZLG_FFMPEG / EZLG_FFPROBE / EZLG_GIFSICLE (tool paths),
#      EZLG_TEST_KEEP=1 keeps the temp dir.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
kit="$here/discord-testkit.sh"
clip="$here/make-test-clip.sh"
ffmpeg=${EZLG_FFMPEG:-ffmpeg}
ffprobe=${EZLG_FFPROBE:-ffprobe}
gifsicle=${EZLG_GIFSICLE:-gifsicle}

tmp=$(mktemp -d)
pass=0; failn=0; skipn=0
results=()
cleanup() {
  if [ "${EZLG_TEST_KEEP:-0}" = 1 ]; then echo "kept: $tmp"; else rm -rf "$tmp"; fi
}
trap cleanup EXIT

log()  { printf '[ktest] %s\n' "$*"; }
ok()   { pass=$((pass+1));   results+=("PASS  $*"); printf '[ktest]   pass: %s\n' "$*"; }
fail() { failn=$((failn+1)); results+=("FAIL  $*"); printf '[ktest]   FAIL: %s\n' "$*" >&2; }
skip() { skipn=$((skipn+1)); results+=("SKIP  $*"); printf '[ktest]   skip: %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }
summary() {
  echo
  echo "== testkit self-test summary ($pass passed, $failn failed, $skipn skipped) =="
  printf '  %s\n' "${results[@]}"
}

probe() { # probe FILE KEY → first video stream entry
  "$ffprobe" -v error -select_streams v:0 -show_entries "stream=$2" -of default=nw=1:nk=1 "$1" 2>/dev/null | head -n 1
}
master_frames() { # master_frames LOGFILE → N from "[testkit] master: N frames of WxH"
  sed -n 's/^\[testkit\] master: \([0-9][0-9]*\) frames.*/\1/p' "$1" | head -n 1
}

# ---------------------------------------------------------------------------
# 1. OUTDIR guard (no tools needed: the guard runs before the tool checks)
# ---------------------------------------------------------------------------
log "1. OUTDIR guard"
: > "$tmp/notadir"                                  # a regular file → mkdir -p notadir/sub fails
if bash "$kit" "$tmp/notadir/sub" > "$tmp/guard.out" 2> "$tmp/guard.err"; then
  fail "guard: exit 0 for an uncreatable OUTDIR"
else
  rc=$?
  if [ "$rc" = 1 ]; then ok "guard: exit 1 for an uncreatable OUTDIR"; else fail "guard: exit $rc, want 1"; fi
fi
if grep -q "is not writable by uid" "$tmp/guard.err"; then ok "guard: names the uid"; else fail "guard: message lacks 'is not writable by uid': $(head -c 300 "$tmp/guard.err")"; fi
if grep -q -- "--entrypoint chown app -R 1000:1000 /output" "$tmp/guard.err"; then ok "guard: prints the chown one-liner"; else fail "guard: chown hint missing"; fi
if grep -q "sudo chown -R 1000:1000 output" "$tmp/guard.err"; then ok "guard: prints the host chown"; else fail "guard: host chown hint missing"; fi
if [ ! -s "$tmp/guard.out" ]; then ok "guard: nothing on stdout"; else fail "guard: unexpected stdout: $(head -c 200 "$tmp/guard.out")"; fi

# ---------------------------------------------------------------------------
# 2. make-test-clip.sh honours FPS
# ---------------------------------------------------------------------------
log "2. make-test-clip.sh FPS"
if have "$ffmpeg" && have "$ffprobe"; then
  if FPS=25 bash "$clip" "$tmp/c25.mov" 1 64x64 premultiplied > /dev/null 2>&1; then
    r=$(probe "$tmp/c25.mov" r_frame_rate)
    if [ "$r" = "25/1" ]; then ok "make-test-clip: FPS=25 → r_frame_rate 25/1"; else fail "make-test-clip: FPS=25 → r_frame_rate '$r'"; fi
  else
    fail "make-test-clip: FPS=25 run failed"
  fi
  if bash "$clip" "$tmp/c30.mov" 1 64x64 premultiplied > /dev/null 2>&1; then
    r=$(probe "$tmp/c30.mov" r_frame_rate)
    if [ "$r" = "30/1" ]; then ok "make-test-clip: default → r_frame_rate 30/1"; else fail "make-test-clip: default → r_frame_rate '$r'"; fi
  else
    fail "make-test-clip: default run failed"
  fi
else
  skip "make-test-clip checks (ffmpeg/ffprobe not found)"
fi

# ---------------------------------------------------------------------------
# 3 + 4. full matrix runs (toolchain image only)
# ---------------------------------------------------------------------------
if have "$ffmpeg" && have "$ffprobe" && have "$gifsicle"; then
  log "3. synthetic source is generated at the master rate (--fps 25)"
  out25="$tmp/kit25"
  if bash "$kit" --fps 25 "$out25" > "$tmp/kit25.log" 2> "$tmp/kit25.err"; then
    ok "kit --fps 25 (synthetic) exits 0"
    src_r=$(probe "$out25/src.mov" r_frame_rate)
    src_n=$(probe "$out25/src.mov" nb_frames)
    m_n=$(master_frames "$tmp/kit25.log")
    if [ "$src_r" = "25/1" ]; then ok "synthetic src.mov is 25/1 (master rate)"; else fail "synthetic src.mov r_frame_rate '$src_r', want 25/1"; fi
    if [ -n "$m_n" ] && [ "$m_n" = "$src_n" ]; then ok "master has all $src_n source frames (no drops)"; else fail "master frames '$m_n' != source frames '$src_n'"; fi
    if grep -q '@ 25 fps → master 25 fps' "$out25/README.md"; then ok "README states '@ 25 fps → master 25 fps'"; else fail "README Source line lacks the rates: $(grep -m1 '^Source' "$out25/README.md")"; fi
    if ! grep -q 'was resampled' "$out25/README.md"; then ok "README has no resample note"; else fail "README carries a resample note for a same-rate source"; fi
    if ! grep -q 'warning: source is' "$tmp/kit25.err"; then ok "no resample warning on stderr"; else fail "unexpected resample warning: $(grep -m1 'warning: source is' "$tmp/kit25.err")"; fi
    if grep -q 'Compare with local playback' "$out25/README.md"; then ok "README Timing bullet compares with local playback"; else fail "README Timing bullet not reworded"; fi
    for f in a_gif_ffmpeg-palette_gifsicle-O2.gif e_webp_lossy_yuva420p_q80.webp e2_webp_lossy_bgra_q80.webp f_webp_lossless_bgra.webp g_apng_rgba.png h1_emote128_gif.gif h2_emote128_webp.webp i1_sticker320_gif.gif i2_sticker320_apng-rgba.png i3_sticker320_apng-indexed.png; do
      if [ -s "$out25/$f" ]; then ok "produced $f"; else fail "missing $f"; fi
    done
    if grep -q '^| `e2_webp_lossy_bgra_q80.webp` |' "$out25/README.md"; then ok "README lists e2 (lossy bgra)"; else fail "README lacks the e2 row"; fi
    # The synthetic ProRes 4444 decodes as yuva444p12le (10-bit for plain 4444
    # exports); either way the kit must unpremultiply at that depth, not gbrap.
    if grep -qE 'unpremultiply at gbrap1[02]le \(source yuva444p1[02]le\)' "$tmp/kit25.log"; then ok "unpremultiply at the source depth (gbrap10le/gbrap12le)"; else fail "no native-depth unpremultiply log line: $(grep -m1 unpremultiply "$tmp/kit25.log")"; fi
    # Scratch: tmpfs when /dev/shm is big enough, otherwise a warning + $TMPDIR.
    shm_kib=$(df -Pk /dev/shm 2>/dev/null | awk 'NR == 2 { print $4 + 0 }')
    if [ -d /dev/shm ] && [ -w /dev/shm ] && [ -n "$shm_kib" ] && [ "$shm_kib" -ge $((256 * 1024)) ]; then
      if grep -q '^\[testkit\] scratch: /dev/shm/ezl-testkit/' "$tmp/kit25.log"; then ok "scratch on /dev/shm/ezl-testkit"; else fail "scratch not on /dev/shm/ezl-testkit: $(grep -m1 'scratch:' "$tmp/kit25.log")"; fi
      if [ ! -d /dev/shm/ezl-testkit ]; then ok "scratch dir removed on exit"; else fail "/dev/shm/ezl-testkit left behind"; fi
    elif [ -d /dev/shm ] && [ -w /dev/shm ]; then
      if grep -q 'warning: /dev/shm has only' "$tmp/kit25.err"; then ok "tiny /dev/shm → warning + \$TMPDIR fallback"; else fail "tiny /dev/shm but no warning"; fi
    else
      skip "scratch location check (no writable /dev/shm here)"
    fi
  else
    fail "kit --fps 25 (synthetic) failed: $(tail -n 5 "$tmp/kit25.err" | tr '\n' ' ')"
  fi

  log "4. a SRC at a different rate warns and is flagged in the README (--fps 20 on the 25 fps clip)"
  out20="$tmp/kit20"
  if [ -f "$out25/src.mov" ] && bash "$kit" --fps 20 "$out20" "$out25/src.mov" > "$tmp/kit20.log" 2> "$tmp/kit20.err"; then
    ok "kit --fps 20 SRC=25fps exits 0"
    m_n=$(master_frames "$tmp/kit20.log")
    src_n=$(probe "$out25/src.mov" nb_frames)
    if grep -q 'warning: source is 25 fps but the master is decoded at 20 fps' "$tmp/kit20.err"; then ok "resample warning printed"; else fail "resample warning missing: $(head -c 300 "$tmp/kit20.err")"; fi
    if grep -q 'source runs at 25 fps and was resampled to 20 fps' "$out20/README.md"; then ok "README carries the resample note"; else fail "README resample note missing"; fi
    if grep -q '@ 25 fps → master 20 fps' "$out20/README.md"; then ok "README states '@ 25 fps → master 20 fps'"; else fail "README Source line lacks the rates"; fi
    if [ -n "$m_n" ] && [ "$m_n" != "$src_n" ]; then ok "master resampled ($src_n → $m_n frames)"; else fail "master frames '$m_n' vs source '$src_n' (expected a change)"; fi
  else
    fail "kit --fps 20 with a 25 fps SRC failed: $(tail -n 5 "$tmp/kit20.err" 2>/dev/null | tr '\n' ' ')"
  fi
else
  skip "full matrix runs (need ffmpeg, ffprobe and gifsicle — run inside the ezlg-dev / runtime image)"
fi

summary
[ "$failn" -eq 0 ]
