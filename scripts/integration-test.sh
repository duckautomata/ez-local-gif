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
#   feather     chromakey + feather (radius 3) on the same clip → chat WebP:
#               frame 0 has ≥ 300 pixels with alpha strictly between 32 and
#               224 (the blurred edge band — background is 0 and subject 255,
#               so every in-between value sits on the subject's edge), and
#               ≥ 10× as many as the unfeathered colorkey WebP has
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
#
# Phase 4 (video exports, bounce, gifski, fast path, /input //output):
#   mp4         chat clip → attachment MP4 asked for 145×145 → job done, file
#               downloads, ffprobe says h264 / yuv420p / even dims (144–146),
#               moov precedes mdat both byte-level and via the lint report's
#               "video.faststart" row, report.ok
#   webm        attachment WebM → vp9 / yuv420p / even dims, report.ok
#   bounce      frames export of the 0–0.5 s trim with and without the bounce
#               op → exactly 2N frame files, bounce first == forward first,
#               bounce first == bounce last and bounce frame N+1 == forward
#               last (frames_match: sha256 or PSNR ≥ 50 dB)
#   gifski      format gif, no target, encoder "gifski" → done, the GIF decodes
#               and gifsicle parses > 1 frame, the primary desc names gifski;
#               a gifski recipe with target emote is refused with 4xx
#   fastpath    the Phase 2 GIF source + a crop op, target attachment → done,
#               96×96, report.ok, and the primary desc says the lossless
#               gifsicle path was taken (no re-encode)
#   input/output (EZLG_START_SERVER=1 only — the script owns the dirs then;
#               skipped with a note otherwise): capabilities inputPick /
#               outputSave true; GET /api/input lists a clip copied into the
#               temp /input dir; POST /api/sources/from-input ingests it and
#               dedupes to the already-uploaded hash; a traversal name is
#               refused; POST /api/results/{hash}/save writes the Phase 1 GIF
#               into the temp /output dir byte-identically, twice → two
#               distinct collision-safe names; an unknown file name is 404
#
# Phase 5 (AI background removal — the matte op + the matte sidecar,
#   docs/background-removal-proposal.md; the sidecar cases run only when
#   EZLG_TEST_MATTE_URL is set, see Env, and are skipped with a note otherwise):
#   matte-off   a recipe with a matte op against a server whose features.matte
#               is false → 400 naming the compose profile. Runs against the
#               server under test when its feature is off; otherwise (with
#               EZLG_START_SERVER=1) against a second throw-away server started
#               WITHOUT EZLG_MATTE_URL on EZLG_TEST_OFF_PORT; otherwise skipped
#   matte-api   GET /api/matte → enabled true with the default model "ready"
#               (waits up to EZLG_TEST_MATTE_WAIT s: a cold sidecar first
#               downloads and self-tests the weights; when it offers
#               birefnet-lite the wait also covers that model settling — it
#               is preloaded after the default), device cuda|cpu,
#               maxSeconds / maxFrames > 0, msPerFrame > 0 (jq only);
#               capabilities features.matte true.
#               Model for the pixel assertions: the cases below draw a
#               synthetic cartoon figure, which the default isnet-anime
#               ("Anime (fast)", trained on anime characters) does NOT detect
#               (alpha ≈ 2 everywhere, no opaque pixel), while the general
#               birefnet-lite ("General (precise)") does. Every Phase 5 recipe
#               names its model ({"kind":"matte","params":{"model":…}}):
#               birefnet-lite when GET /api/matte lists it "ready", else the
#               sidecar's default model, and then the assertions that need a
#               detected figure (frames > 1, opaque AND transparent pixels,
#               corner alpha, report.hasAlpha, VP8X alpha, autocrop smaller
#               than the source) are SKIPPED with a note (counted as skips),
#               while the pipeline assertions (jobs finish, report.ok, the
#               still's 202 → 200, the result cache, an autocrop PNG) still
#               run — report.ok of such a render is a skip naming the rules
#               when it fails (every frame is identical, so the animation
#               collapses to one frame: webp.anim-flag fires on the WebP).
#               EZLG_TEST_MATTE_PIXEL_MODEL=none forces this path.
#               The CPU `matte` profile offers isnet-anime only (see
#               MATTE_MODELS in compose.yaml), so the figure assertions need
#               the `matte-gpu` profile or MATTE_MODELS=isnet-anime,birefnet-lite
#   matte-gif / matte-webp / matte-apng
#               a synthetic cartoon figure orbiting over a blue gradient (an
#               opaque 2 s ProRes 4444 clip drawn in the test) with a matte op
#               → emote GIF / chat WebP / sticker APNG: job done, report.ok,
#               report.hasAlpha, frame 0 has transparent AND opaque pixels,
#               corner pixel transparent (the background is a gradient, so
#               only the AI matte can have keyed it) — exactly alpha 0 on the
#               GIF (its alpha is thresholded at 128), ≤ 8 on the soft-alpha
#               WebP / APNG / still PNG (the model's sigmoid floor leaves the
#               background at alpha ~2, never 0; the opaque / transparent
#               pixel counts use the same 8-step tolerance there); the APNG
#               recipe adds a morph close after the matte (the 5a Edge
#               cleanup on an AI matte)
#   matte-still Phase 5c Compute-button semantics, sent BEFORE any pass exists
#               for the clip: POST /api/still with the matte op and no
#               "eager" → 202 {"pending":"matte","state":"idle",…} (a preview
#               never starts a pass by itself; a 200 at once means the memo
#               was already on disk — a long-lived EZLG_URL stack — and the
#               idle check is skipped with a note); then the same still with
#               "eager": true → 202 running/loading/downloading re-requested
#               until 200 PNG (never idle again); corner pixel transparent
#               (alpha ≤ 8)
#   matte-settings Phase 5c device preference: PUT /api/matte/settings with a
#               device the sidecar does not offer ("tpu") → 400; when
#               GET /api/matte lists BOTH cuda and cpu in "devices": PUT cpu →
#               200 whose device is cpu and whose defaultModel is
#               defaultModels.cpu, GET /api/matte agrees; PUT "" → 200 with a
#               device the sidecar offers (its default). The device in force
#               before the case is restored at the end (a long-lived stack
#               keeps its preference; the spawned server's data dir is
#               throw-away anyway). Skipped with a note on a one-device sidecar
#   matte-stabilise the GIF recipe with "stabilise":"light" (the Phase 5c
#               temporal median over the matte sequence — a derived dir next
#               to the mattes, no second pass) → job done, report.ok, the
#               render.matte check's detail names "stabilise light", and the
#               figure assertions of matte-gif (figure = 1 only)
#   matte-keep  the GIF recipe with "keep":["3a7bd5"] (the figure's BODY
#               colour, forced opaque — a union with the matte) → job done,
#               report.ok, the render.matte detail names the keep colour, and
#               the figure assertions run WHATEVER the model finds: the body
#               rectangle comes back opaque even under isnet-anime (which
#               detects nothing), the gradient never comes within similarity
#               0.08 of 3a7bd5, so frame 0 has opaque AND transparent pixels,
#               the corner is alpha 0 and the orbit gives > 1 frame
#   matte-track Phase 5c guided mode: when GET /api/matte lists "sam2-tiny"
#               (kind "tracker") ready, the GIF recipe with model sam2-tiny
#               and ONE box prompt on frame 0 around the figure's known
#               position (the sprite is at x 48..112, y 54..150 of the 160²
#               frame at t = 0; its pixels span x 58..102, y 54..150 →
#               box [0.33, 0.31, 0.67, 0.97] of the source frame, ~4 % loose)
#               → job done, report.ok, the figure assertions of matte-gif
#               (the tracker finds the figure whatever the edge model does —
#               the gate keeps the tracker's mask where the edge model is
#               blank) and the render.matte detail names sam2-tiny; skipped
#               with a note when the tracker is not offered / not ready
#   matte-prompt POST /api/matte/prompt with the sam2-tiny op, frame 0 and the
#               same box → 200 image/png (202 while the tracker loads is
#               re-requested): an 8-bit gray mask that is white (255) inside
#               the box and black (0) at the corner; skipped like matte-track
#   matte-prompt-mask the 5c follow-up's mask prompt through the same endpoint:
#               the sam2-tiny op with "edge" naming the pixel model and the one
#               prompt {"frame":0,"maskFrom":"edge","mask":true} (the SPA's
#               wire shape: the recipe word plus the client flag) — no box: the
#               frame's mask is the edge model's matte of it, which the server
#               takes from its own memo (never sent by the browser). The idle half runs
#               BEFORE any pass exists for the clip → 202 {"pending":"matte",
#               "state":"idle",…} whose body names the reason (compute the
#               edge model's matte first; a mask prompt never starts a pass —
#               a 200 at once means the edge memo was already on disk, a
#               long-lived EZLG_URL stack, and the half is skipped with a
#               note); the PNG half runs AFTER the per-frame pass (matte-still's
#               eager still / the GIF jobs put the pixel model's matte of the
#               clip on disk) → 200 image/png, white (255) inside the figure
#               and black (0) at the corner, a 202 idle then being a FAIL (the
#               memo was not found). The idle half needs sam2-tiny ready, the
#               PNG half a detected figure too (figure = 1: the mask is the
#               pixel model's matte of frame 0, empty without one)
#   matte-track-mask the GIF recipe with model sam2-tiny, "edge" = the pixel
#               model and that one mask prompt on frame 0, submitted AFTER the
#               per-frame jobs are done (General first, then track from its
#               good frame — a render would run the edge pass itself anyway)
#               → done, report.ok, the render.matte detail names sam2-tiny
#               and the figure assertions of matte-gif (frame 0 has opaque AND
#               transparent pixels); needs sam2-tiny ready and a detected
#               figure, skipped with a note otherwise
#   matte-unload POST /api/matte/unload → 204 (after every matte job is done;
#               the sidecar releases its sessions, the next pass reloads)
#   matte-cached the GIF recipe submitted again → done with "cached": true
#               (the sidecar's weights identity is part of the recipe hash, so
#               the second submit is answered from the result cache — this is
#               the one cache hit the run expects and does not warn about)
#   matte-autocrop trim 0–0.1 s + matte + autocrop (threshold 32: alpha at or
#               above it is content, so the soft floor must stay below it) →
#               PNG smaller than the 160×160 source (the crop found the
#               figure, not the gradient)
#   matte-api   also checks the Phase 5c status fields when the sidecar
#               reports them: "devices" lists the effective "device",
#               defaultModels[device] == defaultModel, sam2-tiny carries
#               kind "tracker" when offered (a pre-5c sidecar without them is
#               a skip with a note, not a failure). The wait also covers
#               sam2-tiny settling when it is offered
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
#   # with the AI matte cases (Phase 5): start a sidecar next to it first — the
#   # GPU one offers birefnet-lite, which the figure assertions need (the CPU
#   # `matte` profile ships isnet-anime only: the pipeline cases run, the
#   # figure assertions are skipped with a note — except matte-keep, whose
#   # body-colour union needs no detected figure, and matte-track, whose
#   # tracker finds it from the box prompt). The Phase 5c cases (settings,
#   # idle, stabilise, keep, track, prompt, unload, and the follow-up's
#   # prompt-mask / track-mask) need a sidecar built from
#   # this checkout (compose.dev.yaml build blocks), not the published tag.
#   docker compose -f compose.yaml -f compose.dev.yaml --profile matte-gpu up -d matte-gpu
#   docker compose -f compose.yaml -f compose.dev.yaml run --rm \
#       -e EZLG_START_SERVER=1 -e EZLG_TEST_MATTE_URL=http://matte:9402 app bash scripts/integration-test.sh
#   # (an ezlg-models volume that predates the image, or was made by hand, fails the sidecar's
#   #  first download with EACCES; the documented repair needs the capability back — the
#   #  service drops them all and chown(2) needs CAP_CHOWN even as root:
#   #    docker compose run --rm --user root --cap-add CHOWN --entrypoint chown matte -R 1000:1000 /models
#   #  — keep the --cap-add when touching this line, USAGE.md or sidecar/README.md)
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
#   EZLG_TEST_PHASE2    0 → run the Phase 1 checks only (default 1; also skips Phases 3 and 4)
#   EZLG_TEST_PHASE3    0 → skip the Phase 3 checks (default 1; also skips Phase 4)
#   EZLG_TEST_PHASE4    0 → skip the Phase 4 checks (default 1; also skips Phase 5)
#   EZLG_TEST_PHASE5    0 → skip the Phase 5 checks (default 1)
#   EZLG_TEST_MATTE_URL the matte sidecar, e.g. http://matte:9402 (the `matte`
#                       profile up in the same compose network). Set → the
#                       Phase 5 sidecar cases run: with EZLG_START_SERVER=1
#                       the spawned server gets it as EZLG_MATTE_URL, with
#                       EZLG_URL it only says "that server has a sidecar".
#                       Unset → they are skipped with a note, and a spawned
#                       server runs with EZLG_MATTE_URL empty whatever the
#                       environment holds (the dev container inherits
#                       compose.yaml's http://matte:9402), so features.matte
#                       is deterministic either way
#   EZLG_TEST_MATTE_WAIT seconds to wait for GET /api/matte to report the
#                       default model ready (default 600 — a cold sidecar
#                       downloads ~176 MB of weights and self-tests them;
#                       the same deadline covers the pixel model and, when
#                       offered, the sam2-tiny tracker settling)
#   EZLG_TEST_TIMEOUT   (above) also bounds the Phase 5c tracker job: on a
#                       CPU sidecar sam2-tiny runs fp32 and a 40-frame track
#                       can take a minute — raise it there
#   EZLG_TEST_MATTE_PIXEL_MODEL
#                       the model the figure assertions use when GET /api/matte
#                       lists it "ready" (default birefnet-lite — the only
#                       shipped model that detects the synthetic figure). An id
#                       the sidecar does not offer (e.g. none) forces the
#                       fallback: the default model + the figure assertions
#                       skipped — handy to exercise that path on a GPU sidecar
#   EZLG_TEST_OFF_PORT  port of the second, sidecar-less server the matte-off
#                       case starts when the main one has the feature on
#                       (default 18089; EZLG_START_SERVER=1 only)
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
phase4=${EZLG_TEST_PHASE4:-1}
phase5=${EZLG_TEST_PHASE5:-1}
ffmpeg=${EZLG_FFMPEG:-ffmpeg}
ffprobe=${EZLG_FFPROBE:-ffprobe}
gifsicle=${EZLG_GIFSICLE:-gifsicle}
webpinfo=${EZLG_WEBPINFO:-webpinfo}
avifdec=${EZLG_AVIFDEC:-avifdec}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)

tmp=$(mktemp -d)
server_pid=""
server2_pid=""   # the sidecar-less server of the Phase 5 matte-off case
pass=0; failn=0; skipn=0; cached_n=0
results=()

cleanup() {
  local p
  for p in "$server_pid" "$server2_pid"; do
    [ -n "$p" ] || continue
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  done
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
skip()  { skipn=$((skipn+1)); results+=("SKIP  $*"); printf '[itest]   skip: %s\n' "$*"; }
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
  echo "== integration test summary ($pass passed, $failn failed, $skipn skipped) =="
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
# Phase 4 helpers.
primary_desc() { # primary_desc FILE → the primary file's desc ("" when absent)
  if [ "$use_jq" = 1 ]; then
    jq -r 'first(.result.files[]? | select((.kind // "") == "" or .kind == "output")) | .desc // empty' "$1" 2>/dev/null
  else
    # The primary file is listed first and nothing before the files carries a
    # "desc" key, so the first desc in the manifest is the primary's. Grep the
    # raw file, not stripped(): the desc value itself holds spaces (Go's
    # encoder writes no spaces around ':' — same reasoning as fonts_has).
    grep -oE '"desc":"[^"]*"' "$1" | head -n 1 | sed -E 's/^"desc":"//; s/"$//'
  fi
}
primary_check_ok() { # primary_check_ok FILE RULE → true if the primary file's lint report lists check RULE with ok true
  if [ "$use_jq" = 1 ]; then
    jq -e --arg r "$2" 'first(.result.files[]? | select((.kind // "") == "" or .kind == "output")) | any(.report.checks[]?; .rule == $r and .ok == true)' "$1" >/dev/null 2>&1
  else
    # discordlint.Check marshals in struct order: {"rule":…,"level":…,"ok":…,…}.
    stripped "$1" | grep -qE "\"rule\":\"$2\",\"level\":\"[^\"]*\",\"ok\":true"
  fi
}
primary_failed_rules() { # primary_failed_rules FILE → the primary file's lint rules with ok false, one per line ("" when none)
  if [ "$use_jq" = 1 ]; then
    jq -r 'first(.result.files[]? | select((.kind // "") == "" or .kind == "output")) | .report.checks[]? | select(.ok == false) | .rule // empty' "$1" 2>/dev/null
  else
    # The primary file is listed first and its checks array ends at the
    # report's own `],"ok":` (`null,"ok":` when empty), so cut the stripped
    # manifest there and keep the failed rows (struct order: rule, level, ok).
    local s; s=$(stripped "$1")
    sed -E 's/(\]|null),"ok":(true|false).*$//' <<<"$s" | grep -oE '"rule":"[^"]*","level":"[^"]*","ok":false' | sed -E 's/^"rule":"//; s/",.*$//'
  fi
}
input_lists() { # input_lists FILE NAME → true if GET /api/input lists the file name (exact)
  if [ "$use_jq" = 1 ]; then jq -e --arg n "$2" 'any(.files[]?; .name == $n)' "$1" >/dev/null 2>&1
  else grep -qF "\"name\":\"$2\"" "$1" # Go's encoder writes no spaces around ':'
  fi
}
# Phase 5 helpers (GET /api/matte: {"enabled","device","reason","gpu","defaultModel",
# "models":{id:{"label","state","percent","msPerFrame",…}},"maxSeconds","maxFrames"}).
matte_enabled() { # matte_enabled FILE → true if GET /api/matte says enabled == true
  if [ "$use_jq" = 1 ]; then jq -e '.enabled == true' "$1" >/dev/null 2>&1
  else stripped "$1" | grep -qF '"enabled":true' # no per-model object carries an "enabled" key
  fi
}
matte_model_state() { # matte_model_state FILE MODEL → that model's "state" ("" when unlisted)
  if [ "$use_jq" = 1 ]; then jq -r --arg m "$2" '.models[$m].state // empty' "$1" 2>/dev/null
  else
    # jobs.MatteModelStatus marshals in struct order: {"label":…,"kind":…,"state":…,…}
    # (label and the Phase 5c kind omitted when empty).
    stripped "$1" | grep -oE "\"$2\":\{(\"label\":\"[^\"]*\",)?(\"kind\":\"[^\"]*\",)?\"state\":\"[^\"]*\"" | head -n 1 | sed -E 's/.*"state":"//; s/"$//'
  fi
}
matte_model_kind() { # matte_model_kind FILE MODEL → that model's Phase 5c "kind" ("" when absent = segmenter)
  if [ "$use_jq" = 1 ]; then jq -r --arg m "$2" '.models[$m].kind // empty' "$1" 2>/dev/null
  else stripped "$1" | grep -oE "\"$2\":\{(\"label\":\"[^\"]*\",)?\"kind\":\"[^\"]*\"" | head -n 1 | sed -E 's/.*"kind":"//; s/"$//'
  fi
}
matte_devices_has() { # matte_devices_has FILE DEV → true when the top-level "devices" array (Phase 5c) lists DEV
  if [ "$use_jq" = 1 ]; then jq -e --arg d "$2" '(.devices // []) | index($d) != null' "$1" >/dev/null 2>&1
  else
    # The top-level list is an ARRAY ("devices":[…]); the per-model Phase 5c maps are objects ("devices":{…}).
    local s; s=$(stripped "$1" | grep -oE '"devices":\[[^]]*\]' | head -n 1)
    grep -qF "\"$2\"" <<<"$s"
  fi
}
matte_devices_text() { # matte_devices_text FILE → the top-level "devices" array as text ("" when absent)
  stripped "$1" | grep -oE '"devices":\[[^]]*\]' | head -n 1 | sed -E 's/^"devices"://'
}
matte_default_model_for() { # matte_default_model_for FILE DEV → defaultModels[DEV] (Phase 5c; "" when absent)
  if [ "$use_jq" = 1 ]; then jq -r --arg d "$2" '.defaultModels[$d] // empty' "$1" 2>/dev/null
  else stripped "$1" | grep -oE '"defaultModels":\{[^}]*\}' | head -n 1 | grep -oE "\"$2\":\"[^\"]*\"" | head -n 1 | sed -E 's/^"[^"]+":"//; s/"$//'
  fi
}
matte_model_ms() { # matte_model_ms FILE MODEL → that model's msPerFrame (jq only; "" otherwise)
  [ "$use_jq" = 1 ] || { echo ""; return; }
  jq -r --arg m "$2" '.models[$m].msPerFrame // empty' "$1" 2>/dev/null
}
matte_model_reason() { # matte_model_reason FILE MODEL → that model's "reason" ("" when none / unlisted)
  if [ "$use_jq" = 1 ]; then jq -r --arg m "$2" '.models[$m].reason // empty' "$1" 2>/dev/null
  else
    # "reason" precedes the only nested object (the Phase 5c "devices" map, last in struct
    # order; sizes is an array), so cutting at the first '}' keeps the model's own reason.
    stripped "$1" | grep -oE "\"$2\":\{[^}]*\}" | head -n 1 | grep -oE '"reason":"[^"]*"' | head -n 1 | sed -E 's/^"reason":"//; s/"$//'
  fi
}
matte_model_settled() { # matte_model_settled FILE MODEL → true when MODEL is not on its way: unlisted, ready, unavailable, or missing for a stated reason
  # (loading / downloading, and missing with no reason yet = queued behind
  # another model's load, are transient — the sidecar has one loader thread).
  local st; st=$(matte_model_state "$1" "$2")
  case "$st" in
    ""|ready|unavailable) return 0 ;;
    missing) [ -n "$(matte_model_reason "$1" "$2")" ] ;;
    *) return 1 ;;
  esac
}
matte_report_check() { # matte_report_check NAME → the primary report.ok of a Phase 5 render, naming the failed rules
  # With a detected figure ($figure = 1) a failure is a FAIL. Without one the
  # picture is undefined — every frame identical and (near) fully transparent —
  # so the animation collapses to one frame and a structural rule can fail
  # (webp.anim-flag: "ANIM flag set but the file has 1 frame"): that is the
  # fixture, not the pipeline, and is a SKIP that still names the rules.
  local name=$1 rules
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; return 0; fi
  rules=$(primary_failed_rules "$tmp/poll_$name.json" | tr '\n' ' '); rules=${rules% }
  if [ "$figure" = 1 ]; then fail "$name: primary report.ok != true (failed: ${rules:-?})"
  else skip "$name: primary report.ok != true (failed: ${rules:-?}) — lint of a render without a detected figure (every frame identical, the animation collapses to one frame; $pmodel does not find the synthetic one)"
  fi
}
still_pending_seen=0
still_first_state=""   # the "state" of the first 202 still_png_wait saw ("" when none)
still_png_wait() { # still_png_wait OUT JSON → 0 when POST /api/still answered 200 with a PNG; a 202 (AI matte pending) is re-requested until the job timeout
  local out=$1 body=$2 code deadline=$((SECONDS + timeout))
  still_first_state=""
  while :; do
    code=$(curl -sS -o "$out" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' --data "$body" "$url/api/still")
    [ "$code" = 202 ] || break
    still_pending_seen=1
    [ -n "$still_first_state" ] || still_first_state=$(json_str "$out" '.state' state)
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 1
  done
  still_code=$code
  [ "$code" = 200 ] && [ "$(magic_hex "$out" 8)" = "89504e470d0a1a0a" ]
}
still_once() { # still_once OUT JSON → prints the HTTP code of ONE POST /api/still (no re-request: the Phase 5c idle check)
  curl -sS -o "$1" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' --data "$2" "$url/api/still"
}
prompt_png_wait() { # prompt_png_wait OUT JSON → 0 when POST /api/matte/prompt (Phase 5c) answered 200 with a PNG; 202 (tracker loading) is re-requested until the job timeout
  local out=$1 body=$2 code deadline=$((SECONDS + timeout))
  while :; do
    code=$(curl -sS -o "$out" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' --data "$body" "$url/api/matte/prompt")
    [ "$code" = 202 ] || break
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 1
  done
  prompt_code=$code
  [ "$code" = 200 ] && [ "$(magic_hex "$out" 8)" = "89504e470d0a1a0a" ]
}
prompt_once() { # prompt_once OUT JSON → prints the HTTP code of ONE POST /api/matte/prompt (no re-request: the mask prompt's idle / after-pass checks judge each answer)
  curl -sS -o "$1" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' --data "$2" "$url/api/matte/prompt"
}
primary_check_detail() { # primary_check_detail FILE RULE → the primary file's lint-report detail for RULE ("" when absent)
  if [ "$use_jq" = 1 ]; then
    jq -r --arg r "$2" 'first(.result.files[]? | select((.kind // "") == "" or .kind == "output")) | first(.report.checks[]? | select(.rule == $r)) | .detail // empty' "$1" 2>/dev/null
  else
    # discordlint.Check marshals in struct order: {"rule":…,"level":…,"ok":…,"fixed":…,"detail":…};
    # the primary file is listed first. Grep the raw file: the detail holds spaces.
    grep -oE "\"rule\":\"$2\",\"level\":\"[^\"]*\",\"ok\":(true|false),\"fixed\":(true|false),\"detail\":\"[^\"]*\"" "$1" | head -n 1 | sed -E 's/.*"detail":"//; s/"$//'
  fi
}
gray_counts() { # gray_counts FILE → "white black": pixels of frame 0 (decoded as 8-bit gray) at exactly 255 / exactly 0 — the Phase 5c prompt mask
  local vals
  vals=$("$ffmpeg" -v error -nostdin -i "$1" -frames:v 1 -vf "format=gray" -f rawvideo -pix_fmt gray - 2>/dev/null \
         | od -An -v -tu1 | tr -s ' \n' '\n' | grep -v '^$' || true)
  awk 'NF && $1 == 255 { w++ } NF && $1 == 0 { b++ } END { printf "%d %d\n", w, b }' <<<"$vals"
}
gray_corner() { # gray_corner FILE → the gray value (0..255) of pixel (0,0) of frame 0
  "$ffmpeg" -v error -nostdin -i "$1" -frames:v 1 -vf "format=gray,crop=1:1:0:0" -f rawvideo -pix_fmt gray - 2>/dev/null \
    | od -An -tu1 | awk '{print $1}'
}
gif_figure_checks() { # gif_figure_checks NAME RUN → the Phase 5 GIF figure assertions on out_file[NAME] (RUN = 1), else one skip naming why
  # The GIF's alpha is thresholded at 128, so its background is exactly 0.
  local name=$1 run=$2 f=${out_file[$1]} n n_op n_tr ca
  if [ "$run" != 1 ]; then
    skip "$name: figure assertions (frames > 1 — got $(gif_frames "$f"), hasAlpha, opaque AND transparent pixels, corner alpha) need a detected figure ($pmodel does not find the synthetic one)"
    return 0
  fi
  n=$(gif_frames "$f")
  if [ "${n:-0}" -gt 1 ]; then ok "$name: $n frames"; else fail "$name: '${n:-?}' frame(s), want > 1 (identical fully transparent frames merge into one)"; fi
  if primary_has_alpha "$tmp/poll_$name.json"; then ok "$name: report.hasAlpha == true"; else fail "$name: report.hasAlpha != true (the matte produced no transparency)"; fi
  read -r n_op n_tr <<<"$(alpha_counts "$f")"
  if [ "${n_op:-0}" -gt 0 ] && [ "${n_tr:-0}" -gt 0 ]; then ok "$name: frame 0 has $n_op opaque and $n_tr transparent pixels"; else fail "$name: frame 0 has ${n_op:-0} opaque / ${n_tr:-0} transparent pixels, want both > 0"; fi
  ca=$(corner_alpha "$f")
  if [ "${ca:-255}" = 0 ]; then ok "$name: corner pixel transparent (alpha 0)"; else fail "$name: corner pixel alpha '${ca:-?}', want 0"; fi
}
make_matte_clip() { # make_matte_clip OUT.mov → a 2 s 160×160 30 fps OPAQUE ProRes 4444 clip for the matte op:
  # a cartoon figure (brown hair, face, two eyes, blue body — flat colours, 64×96) orbiting
  # once over a blue gradient. No colour key can separate it (the gradient spans the
  # figure's colours), so transparency in the output can only come from the AI matte.
  local eyes hair head body r g b a fig graph
  eyes='(lt((X-24)^2+(Y-26)^2,9)+lt((X-40)^2+(Y-26)^2,9))'
  hair='(lt((X-32)^2+(Y-20)^2,484)*lt(Y,20))'
  head='lt((X-32)^2+(Y-24)^2,400)'
  body='(between(X,14,49)*between(Y,44,95))'
  r="if($eyes,32,if($hair,74,if($head,242,if($body,58,0))))"
  g="if($eyes,32,if($hair,42,if($head,201,if($body,123,0))))"
  b="if($eyes,32,if($hair,16,if($head,160,if($body,213,0))))"
  a="255*gt($hair+$head+$body,0)"
  fig="color=c=0x000000:s=64x96:r=30:d=2,format=gbrap,geq=r='$r':g='$g':b='$b':a='$a'"
  graph="gradients=s=160x160:r=30:d=2:c0=0x1e2a4a:c1=0xa9c4e4:nb_colors=2:seed=7:speed=0.005:x0=0:y0=0:x1=159:y1=159[bg];"
  graph+="${fig}[fig];[bg][fig]overlay=x='(W-w)/2+0.35*(W-w)*sin(2*PI*t/2)':y='(H-h)/2+0.35*(H-h)*cos(2*PI*t/2)',format=yuv444p10le"
  "$ffmpeg" -hide_banner -loglevel error -nostdin -y -filter_complex "$graph" \
    -c:v prores_ks -profile:v 4444 -pix_fmt yuv444p10le -vendor apl0 -movflags +faststart "$1"
}
moov_before_mdat() { # moov_before_mdat FILE → 0 when the first moov box precedes the first mdat (both present)
  # Captured first (house SIGPIPE rule); top-level boxes only ever contain
  # these four-byte names once each in our own faststart outputs.
  local offs moov mdat
  offs=$(grep -abo -e moov -e mdat "$1" 2>/dev/null || true)
  moov=$(sed -n 's/^\([0-9]*\):moov$/\1/p' <<<"$offs" | head -n 1)
  mdat=$(sed -n 's/^\([0-9]*\):mdat$/\1/p' <<<"$offs" | head -n 1)
  [ -n "$moov" ] && [ -n "$mdat" ] && [ "$moov" -lt "$mdat" ]
}
pixfmt_of() { "$ffprobe" -v error -select_streams v:0 -show_entries stream=pix_fmt -of default=nw=1:nk=1 "$1" 2>/dev/null | head -n 1; }

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
alpha_counts() { # alpha_counts FILE [TOL] → "opaque transparent": pixels of frame 0 with alpha ≥ 255−TOL / ≤ TOL
  # TOL defaults to 0 (exactly 255 / exactly 0). The Phase 5 soft-alpha outputs
  # pass 8: an AI matte's sigmoid never reaches 0 (the background sits at ~2).
  local tol=${2:-0} vals
  vals=$("$ffmpeg" -v error -nostdin -i "$1" -frames:v 1 -vf "format=rgba,alphaextract" -f rawvideo -pix_fmt gray - 2>/dev/null \
         | od -An -v -tu1 | tr -s ' \n' '\n' | grep -v '^$' || true)
  awk -v t="$tol" 'NF && $1 >= 255 - t { o++ } NF && $1 <= t { z++ } END { printf "%d %d\n", o, z }' <<<"$vals"
}
corner_alpha() { # corner_alpha FILE → alpha (0..255) of pixel (0,0) of frame 0
  "$ffmpeg" -v error -nostdin -i "$1" -frames:v 1 -vf "format=rgba,crop=1:1:0:0" -f rawvideo -pix_fmt rgba - 2>/dev/null \
    | od -An -tu1 | awk '{print $4}'
}
alpha_mid_count() { # alpha_mid_count FILE → pixels of frame 0 with alpha strictly between 32 and 224
  # On the green-screen clip the keyed background is alpha 0 and the subject
  # 255, so every in-between value sits on the subject's edge: a feathered
  # (gblur'd) edge yields a wide band of them, a hard key (almost) none.
  "$ffmpeg" -v error -nostdin -i "$1" -frames:v 1 -vf "format=rgba,alphaextract" -f rawvideo -pix_fmt gray - 2>/dev/null \
    | od -An -v -tu1 | awk '{ for (i = 1; i <= NF; i++) if ($i > 32 && $i < 224) n++ } END { print n + 0 }'
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
  # Phase 4: own the picker/save dirs too (overriding anything inherited), so
  # the /input //output cases below can see what the server sees.
  export EZLG_INPUT="$tmp/inputdir" EZLG_OUTPUT="$tmp/outputdir"
  mkdir -p "$EZLG_INPUT" "$EZLG_OUTPUT"
  # Phase 5: the spawned server's sidecar is EZLG_TEST_MATTE_URL — empty unless
  # the caller opted in, overriding the compose.yaml EZLG_MATTE_URL the dev
  # container inherits, so features.matte is deterministic: off → the matte-off
  # case runs against this server; on → against a second one (see Phase 5).
  export EZLG_MATTE_URL=${EZLG_TEST_MATTE_URL:-}
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
# Phase 3: keying (+ feather), text / image / animated overlays, reverse,
# autocrop, the animated proxy, fonts
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
  recipe[feather]='{"v":1,"sources":["'"$green_hash"'"],"ops":[{"kind":"chromakey","params":{"color":"00ff00"}},{"kind":"feather","params":{"radius":3}}],"output":{"format":"webp","quality":80,"preset":"chat","target":"attachment"}}'
  # 50 fps out of the 30 fps clip: the fps filter repeats two frames in three,
  # i.e. a transparent GIF made of short holds (see the holds check below).
  recipe[holds]='{"v":1,"sources":["'"$green_hash"'"],"ops":[{"kind":"chromakey","params":{"color":"00ff00"}},{"kind":"fps","params":{"fps":50}}],"output":{"format":"gif","preset":"custom","target":"attachment"}}'
  phase3_jobs+=(chromakey colorkey feather holds)
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

# ---- holds: a transparent GIF with held frames. Discord drops frames that
# do not change the picture — and their disposal with them — and gifsicle's
# optimiser turns a run of identical frames into one long frame plus a short
# clear-only frame of exactly that kind (the next pose then stacks on the old
# one). The server merges held frames before gifsicle sees them: the lint row
# passes and the repeated frames are gone (2 s × 50 fps = 100 master frames,
# ~60 distinct ones). This case checks the outcome only: were the merge lost,
# the lint ladder's hold repair (which runs for every target, none included)
# would deliver the same structure at the cost of two more gifsicle passes.
# That the merge itself runs before gifsicle is pinned by the Go tests
# TestEncodeGIFAtMergesBeforeGifsicle (tool-free) and
# TestRenderGIFWithHolds/no-gifsicle in internal/jobs.
name=holds
if [ -n "$green_hash" ] && finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  if primary_check_ok "$tmp/poll_$name.json" gif.noop-frame-disposal; then
    ok "$name: lint row gif.noop-frame-disposal is ok (no clear-only frame Discord would drop)"
  else
    fail "$name: lint row gif.noop-frame-disposal missing or not ok"
  fi
  n=$(gif_frames "$f")
  if [ "${n:-0}" -ge 2 ] && [ "${n:-0}" -le 70 ]; then
    ok "$name: $n frames (held frames of the 100-frame master merged)"
  else
    fail "$name: ${n:-0} frames, want 2..70 — the held frames of the 100-frame master were not merged"
  fi
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

# ---- feather: chromakey + feather → intermediate alpha at the subject's edge
name=feather
if [ -n "$green_hash" ] && finish_job $name && fetch_primary $name webp; then
  f=${out_file[$name]}
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  mid=$(alpha_mid_count "$f")
  # A sigma-3 gblur on the alpha of the 160×160 clip's ~53 px square puts well
  # over 1000 frame-0 pixels strictly between 32 and 224 (measured ~1268); a
  # hard key leaves ~0. 300 keeps a wide margin either way.
  if [ "${mid:-0}" -ge 300 ]; then
    ok "$name: frame 0 has $mid pixels with alpha strictly between 32 and 224 (feathered edge)"
  else
    fail "$name: frame 0 has only ${mid:-0} pixels with intermediate alpha, want ≥ 300 — the edge was not feathered"
  fi
  # The colorkey case above is the same clip keyed to WebP *without* feather:
  # its edge must be (almost) hard — far fewer intermediate pixels.
  if [ -s "${out_file[colorkey]:-}" ]; then
    base_mid=$(alpha_mid_count "${out_file[colorkey]}")
    if [ $((${base_mid:-0} * 10)) -le "${mid:-0}" ]; then
      ok "$name: unfeathered colorkey WebP has only ${base_mid:-0} intermediate-alpha pixels (≤ a tenth of the feathered $mid)"
    else
      fail "$name: unfeathered colorkey WebP has ${base_mid:-0} intermediate-alpha pixels vs $mid feathered — feather made no difference"
    fi
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

if [ "$phase4" != 1 ]; then
  summary
  [ "$failn" -eq 0 ]
  exit
fi

# =============================================================================
# Phase 4: MP4/WebM export, bounce, gifski encoder, lossless gifsicle fast
# path, /input picker + /output save
# =============================================================================
log "phase 4: video exports, bounce, gifski, fast path"

# ---- submit every Phase 4 job up front
recipe[mp4]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"mp4","width":145,"height":145,"fit":"contain","preset":"chat","target":"attachment"}}'
recipe[webm]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"webm","preset":"chat","target":"attachment"}}'
trim05='{"kind":"trim","params":{"start":0,"end":0.5}}'
recipe[bounce-fwd]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"','"$trim05"'],"output":{"format":"frames","frameFormat":"png","preset":"frames"}}'
recipe[bounce]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"','"$trim05"',{"kind":"bounce"}],"output":{"format":"frames","frameFormat":"png","preset":"frames"}}'
recipe[gifski]='{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"gif","encoder":"gifski","quality":90,"preset":"custom"}}'
phase4_jobs=(mp4 webm bounce-fwd bounce gifski)
if [ -n "$gif_hash" ]; then
  recipe[fastpath]='{"v":1,"sources":["'"$gif_hash"'"],"ops":[{"kind":"crop","params":{"x":16,"y":16,"w":96,"h":96}}],"output":{"format":"gif","preset":"custom","target":"attachment"}}'
  phase4_jobs+=(fastpath)
fi
for name in "${phase4_jobs[@]}"; do
  submit_job "$name" || true
done

# gifski is never allowed for emote/sticker targets (per-frame local palettes
# break on Discord emotes — DESIGN.md §9a): the recipe must be refused.
code=$(curl -sS -o "$tmp/gifski_emote.json" -w '%{http_code}' --max-time 30 -H 'Content-Type: application/json' \
         --data '{"v":1,"sources":["'"$hash"'"],"ops":['"$unp"'],"output":{"format":"gif","encoder":"gifski","width":128,"height":128,"fit":"contain","preset":"emote","target":"emote"}}' "$url/api/jobs")
case "$code" in
  4*) ok "gifski + target emote refused ($code)" ;;
  *)  cat "$tmp/gifski_emote.json" >&2; fail "gifski + target emote → $code, want 4xx" ;;
esac

# ---- attachment MP4: h264 / yuv420p / even dims / moov before mdat
name=mp4
if finish_job $name && fetch_primary $name mp4; then
  f=${out_file[$name]}
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>"$tmp/$name.err"; then ok "$name: decodes (ffmpeg)"; else fail "$name: does not decode: $(head -c 300 "$tmp/$name.err")"; fi
  if [ "$(codec_of "$f")" = h264 ]; then ok "$name: codec h264"; else fail "$name: codec '$(codec_of "$f")', want h264"; fi
  if [ "$(pixfmt_of "$f")" = yuv420p ]; then ok "$name: pix_fmt yuv420p"; else fail "$name: pix_fmt '$(pixfmt_of "$f")', want yuv420p"; fi
  d=$(dims_of "$f"); w=${d%x*}; h=${d#*x}
  if [ "${w:-1}" -ge 144 ] && [ "$w" -le 146 ] && [ "${h:-1}" -ge 144 ] && [ "$h" -le 146 ] \
     && [ $((w % 2)) -eq 0 ] && [ $((h % 2)) -eq 0 ]; then
    ok "$name: $d — the odd 145x145 request was made even"
  else
    fail "$name: dims '$d', want both sides even and within 144..146 (requested 145x145)"
  fi
  if moov_before_mdat "$f"; then ok "$name: moov precedes mdat (+faststart, byte check)"; else fail "$name: moov does not precede mdat"; fi
  if primary_check_ok "$tmp/poll_$name.json" video.faststart; then ok "$name: lint row video.faststart ok"; else fail "$name: lint report has no passing video.faststart row"; fi
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
fi

# ---- attachment WebM: vp9 / yuv420p / even dims
name=webm
if finish_job $name && fetch_primary $name webm; then
  f=${out_file[$name]}
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>"$tmp/$name.err"; then ok "$name: decodes (ffmpeg)"; else fail "$name: does not decode: $(head -c 300 "$tmp/$name.err")"; fi
  if [ "$(codec_of "$f")" = vp9 ]; then ok "$name: codec vp9"; else fail "$name: codec '$(codec_of "$f")', want vp9"; fi
  if [ "$(pixfmt_of "$f")" = yuv420p ]; then ok "$name: pix_fmt yuv420p"; else fail "$name: pix_fmt '$(pixfmt_of "$f")', want yuv420p"; fi
  d=$(dims_of "$f"); w=${d%x*}; h=${d#*x}
  if [ "${w:-1}" -gt 0 ] && [ $((w % 2)) -eq 0 ] && [ "${h:-1}" -gt 0 ] && [ $((h % 2)) -eq 0 ]; then
    ok "$name: $d (even dims)"
  else
    fail "$name: dims '$d', want both sides even"
  fi
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
fi

# ---- bounce: 2N frames, forward then backward (full copy: first == last)
name=bounce-fwd
n_bf=0
if finish_job $name; then
  n_bf=$(files_of_kind "$tmp/poll_$name.json" frame)
  if [ "${n_bf:-0}" -ge 2 ]; then ok "$name: forward trim exports $n_bf frames"; else fail "$name: only ${n_bf:-0} frame files, want ≥ 2"; n_bf=0; fi
fi
name=bounce
if finish_job $name; then
  n_bn=$(files_of_kind "$tmp/poll_$name.json" frame)
  if [ "${n_bf:-0}" -ge 2 ] && [ "${n_bn:-0}" = "$((n_bf * 2))" ]; then
    ok "$name: $n_bn frame files == 2 × the forward export's $n_bf"
  else
    fail "$name: ${n_bn:-0} frame files, want exactly 2 × ${n_bf:-?}"
  fi
  bf=$(frame_url_at "$tmp/poll_$name.json" first); bl=$(frame_url_at "$tmp/poll_$name.json" last)
  bm=$(frame_urls "$tmp/poll_$name.json" | sed -n "$((n_bf + 1))p") # first frame of the reversed half
  ff4=$(frame_url_at "$tmp/poll_bounce-fwd.json" first); fl4=$(frame_url_at "$tmp/poll_bounce-fwd.json" last)
  if [ -n "$bf" ] && [ -n "$bl" ] && [ -n "$bm" ] && [ -n "$ff4" ] && [ -n "$fl4" ] \
     && download "$bf" "$tmp/bnc_first.png" && download "$bl" "$tmp/bnc_last.png" \
     && download "$bm" "$tmp/bnc_mid.png" \
     && download "$ff4" "$tmp/bfwd_first.png" && download "$fl4" "$tmp/bfwd_last.png"; then
    if ! frames_match "$tmp/bfwd_first.png" "$tmp/bfwd_last.png"; then ok "$name: forward first and last frames differ (a bounce is detectable)"; else fail "$name: forward first and last frames are identical — cannot tell a bounce"; fi
    if frames_match "$tmp/bnc_first.png" "$tmp/bfwd_first.png"; then ok "$name: first frame == forward first frame"; else fail "$name: first frame != forward first frame (PSNR $(psnr_of "$tmp/bnc_first.png" "$tmp/bfwd_first.png") dB)"; fi
    if frames_match "$tmp/bnc_first.png" "$tmp/bnc_last.png"; then ok "$name: first frame == last frame (ping-pong returns to the start)"; else fail "$name: first frame != last frame (PSNR $(psnr_of "$tmp/bnc_first.png" "$tmp/bnc_last.png") dB)"; fi
    if frames_match "$tmp/bnc_mid.png" "$tmp/bfwd_last.png"; then ok "$name: frame N+1 == forward last frame (the reversed half starts at the end)"; else fail "$name: frame N+1 != forward last frame (PSNR $(psnr_of "$tmp/bnc_mid.png" "$tmp/bfwd_last.png") dB)"; fi
  else
    fail "$name: could not download the first/mid/last frames of both exports"
  fi
fi

# ---- gifski encoder (HQ toggle, non-Discord target)
name=gifski
if finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
  n=$(gif_frames "$f")
  if [ "${n:-0}" -gt 1 ]; then ok "$name: gifsicle-clean, $n frames"; else fail "$name: gifsicle/ffprobe report '${n:-?}' frame(s)"; fi
  d4=$(primary_desc "$tmp/poll_$name.json")
  case "$d4" in
    *gifski*) ok "$name: primary desc names gifski ('$d4')" ;;
    *)        fail "$name: primary desc '$d4' does not mention gifski" ;;
  esac
fi

# ---- lossless gifsicle fast path (GIF source + crop only, attachment target)
name=fastpath
if [ -n "$gif_hash" ] && finish_job $name && fetch_primary $name gif; then
  f=${out_file[$name]}
  if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
  if have "$gifsicle" && [ "$(gif_screen_size "$f")" = 96x96 ]; then ok "$name: cropped to 96x96"; else fail "$name: logical screen '$(gif_screen_size "$f")', want 96x96"; fi
  if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"; else fail "$name: primary report.ok != true"; fi
  d4=$(primary_desc "$tmp/poll_$name.json")
  case "$d4" in
    *"lossless gifsicle"*) ok "$name: primary desc says the lossless path was taken ('$d4')" ;;
    *)                     fail "$name: primary desc '$d4' does not mention the lossless gifsicle path" ;;
  esac
fi

# ---- /input picker + /output save (only meaningful when this script started
# the server and therefore owns the dirs the server sees)
log "phase 4: /input picker + /output save"
if [ "${EZLG_START_SERVER:-0}" = 1 ]; then
  code=$(curl -sS -o "$tmp/caps4.json" -w '%{http_code}' --max-time 30 "$url/api/capabilities")
  if [ "$code" = 200 ]; then
    for feat in inputPick outputSave; do
      if features_true "$tmp/caps4.json" "$feat"; then ok "capabilities: features.$feat == true"; else fail "capabilities: features.$feat is not true"; fi
    done
  else
    fail "GET /api/capabilities → $code"
  fi
  if cp "$tmp/src.mov" "$EZLG_INPUT/clip.mov"; then
    code=$(curl -sS -o "$tmp/input.json" -w '%{http_code}' --max-time 30 "$url/api/input")
    if [ "$code" = 200 ]; then
      ok "GET /api/input → 200"
      if input_lists "$tmp/input.json" clip.mov; then ok "input: lists clip.mov"; else fail "input: clip.mov not listed: $(head -c 300 "$tmp/input.json")"; fi
    else
      fail "GET /api/input → $code"
    fi
    code=$(curl -sS -o "$tmp/frominput.json" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' \
             --data '{"name":"clip.mov"}' "$url/api/sources/from-input")
    if [ "$code" = 200 ]; then
      ok "POST /api/sources/from-input → 200"
      ihash=$(json_str "$tmp/frominput.json" '.hash' hash)
      if [ "$ihash" = "$hash" ]; then ok "from-input: deduped to the uploaded source hash"; else fail "from-input: hash '$ihash' != uploaded '$hash' (same bytes must dedupe)"; fi
    else
      cat "$tmp/frominput.json" >&2
      fail "POST /api/sources/from-input → $code"
    fi
    code=$(curl -sS -o "$tmp/frominput_bad.json" -w '%{http_code}' --max-time 30 -H 'Content-Type: application/json' \
             --data '{"name":"../clip.mov"}' "$url/api/sources/from-input")
    case "$code" in
      4*) ok "from-input: traversal name ../clip.mov refused ($code)" ;;
      *)  fail "from-input: traversal name ../clip.mov → $code, want 4xx" ;;
    esac
  else
    fail "could not copy the clip into $EZLG_INPUT"
  fi
  if [ -n "${rhash:-}" ] && [ -n "${rname:-}" ] && [ -s "${out_file[gif]:-}" ]; then
    save1=""; save2=""
    for i in 1 2; do
      code=$(curl -sS -o "$tmp/save$i.json" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' \
               --data '{"file":"'"$rname"'"}' "$url/api/results/$rhash/save")
      if [ "$code" = 200 ]; then
        sname=$(json_str "$tmp/save$i.json" '.name' name)
        if [ -n "$sname" ] && [ -s "$EZLG_OUTPUT/$sname" ] && cmp -s "$EZLG_OUTPUT/$sname" "${out_file[gif]}"; then
          ok "save #$i: wrote '$sname' to /output byte-identically"
        else
          fail "save #$i: '$sname' is missing from $EZLG_OUTPUT or differs from the downloaded gif"
        fi
        [ "$i" = 1 ] && save1=$sname || save2=$sname
      else
        cat "$tmp/save$i.json" >&2
        fail "POST /api/results/{recipeHash}/save #$i → $code"
      fi
    done
    if [ -n "$save1" ] && [ -n "$save2" ] && [ "$save1" != "$save2" ]; then
      ok "save: second save got a distinct collision-safe name ($save1 / $save2)"
    elif [ -n "$save1" ] && [ -n "$save2" ]; then
      fail "save: both saves returned '$save1' — not collision-safe"
    fi
    code=$(curl -sS -o "$tmp/save_bad.json" -w '%{http_code}' --max-time 30 -H 'Content-Type: application/json' \
             --data '{"file":"no-such-file.gif"}' "$url/api/results/$rhash/save")
    if [ "$code" = 404 ]; then ok "save: unknown file name → 404"; else fail "save: unknown file name → $code, want 404"; fi
  else
    fail "save: no recipeHash / file / downloaded gif from the Phase 1 gif job"
  fi
else
  skip "input/output cases: EZLG_START_SERVER != 1 — a remote server's /input //output dirs are not visible from here (run with EZLG_START_SERVER=1 to cover GET /api/input, POST /api/sources/from-input and POST /api/results/{recipeHash}/save)"
fi

if [ "$phase5" != 1 ]; then
  summary
  [ "$failn" -eq 0 ]
  exit
fi

# =============================================================================
# Phase 5: AI background removal — the matte op and the matte sidecar
# =============================================================================
log "phase 5: AI matte (matte op, GET /api/matte)"

# ---- matte-off: a matte op is refused with 400 while features.matte is false.
# Against the server under test when its own feature is off; else (the binary is
# here when EZLG_START_SERVER=1) against a second throw-away server started
# WITHOUT EZLG_MATTE_URL on EZLG_TEST_OFF_PORT; else skipped — a remote server
# with its sidecar up cannot be switched off from here.
name=matte-off
off_url=""; off_note=""
code=$(curl -sS -o "$tmp/caps5.json" -w '%{http_code}' --max-time 30 "$url/api/capabilities")
if [ "$code" != 200 ]; then
  fail "GET /api/capabilities → $code"
elif ! features_true "$tmp/caps5.json" matte; then
  off_url=$url
elif [ "${EZLG_START_SERVER:-0}" = 1 ]; then
  off_port=${EZLG_TEST_OFF_PORT:-18089}
  mkdir -p "$tmp/data-off"
  log "starting a second server without EZLG_MATTE_URL on :$off_port (log=$tmp/server-off.log)"
  EZLG_MATTE_URL="" EZLG_ADDR=":$off_port" EZLG_DATA="$tmp/data-off" "$tmp/ezlg" serve > "$tmp/server-off.log" 2>&1 &
  server2_pid=$!
  off_url="http://127.0.0.1:$off_port"
  deadline=$((SECONDS + 30))
  until curl -fsS --max-time 2 "$off_url/healthz" >/dev/null 2>&1; do
    if ! kill -0 "$server2_pid" 2>/dev/null || [ "$SECONDS" -ge "$deadline" ]; then
      cat "$tmp/server-off.log" >&2 || true
      fail "$name: the second (sidecar-less) server on :$off_port did not become healthy"
      off_url=""; break
    fi
    sleep 1
  done
else
  off_note="this server has features.matte true and EZLG_START_SERVER != 1 — nothing here can switch its sidecar off (run with EZLG_START_SERVER=1: the case then starts a second server without EZLG_MATTE_URL)"
fi
if [ -n "$off_url" ]; then
  # That server needs the source too (the same bytes dedupe to the Phase 1 hash).
  code=$(curl -sS -o "$tmp/upload_off.json" -w '%{http_code}' --max-time 120 -F "file=@$tmp/src.mov" "$off_url/api/upload")
  off_hash=$(json_str "$tmp/upload_off.json" '.hash' hash)
  if [ "$code" = 200 ] && [[ "$off_hash" =~ ^[0-9a-f]{64}$ ]]; then
    code=$(curl -sS -o "$tmp/job_$name.json" -w '%{http_code}' --max-time 30 -H 'Content-Type: application/json' \
             --data '{"v":1,"sources":["'"$off_hash"'"],"ops":[{"kind":"matte"}],"output":{"format":"gif","preset":"custom","target":"attachment"}}' "$off_url/api/jobs")
    # job_error's grep fallback reads whitespace-stripped JSON and stops at an
    # escaped quote, so the check uses it and the message shows the raw body.
    err=$(job_error "$tmp/job_$name.json"); raw=$(head -c 240 "$tmp/job_$name.json" | tr -d '\n')
    if [ "$code" = 400 ] && grep -qi 'matte' <<<"$err"; then
      ok "$name: matte op with features.matte false → 400 naming the matte profile ($raw)"
    else
      fail "$name: matte op with features.matte false → $code, want 400 naming the matte / matte-gpu profile ($raw)"
    fi
  else
    fail "$name: could not upload the clip to $off_url ($code: $(head -c 200 "$tmp/upload_off.json"))"
  fi
elif [ -n "$off_note" ]; then
  skip "$name: $off_note"
fi

# ---- the sidecar cases
if [ -z "${EZLG_TEST_MATTE_URL:-}" ]; then
  skip "matte sidecar cases: EZLG_TEST_MATTE_URL is not set — start a sidecar (docker compose -f compose.yaml -f compose.dev.yaml --profile matte up -d matte) and set EZLG_TEST_MATTE_URL=http://matte:9402 to cover GET /api/matte, the matte op (GIF / WebP / APNG / still / autocrop) and its result cache"
else
  # ---- matte-api: wait for the sidecar's default model (a cold start downloads
  # the weights) and, when it is offered, for the pixel model to settle too —
  # the sidecar preloads one model at a time, so birefnet-lite is still
  # downloading / loading when the default turns ready.
  pixel_model=${EZLG_TEST_MATTE_PIXEL_MODEL:-birefnet-lite}   # the model that detects the synthetic figure (see the header / Env)
  matte_wait=${EZLG_TEST_MATTE_WAIT:-600}
  log "phase 5: waiting up to ${matte_wait}s for GET /api/matte to report the default model ready (and '$pixel_model' settled when offered)"
  deadline=$((SECONDS + matte_wait)); matte_ready=0; mmodel=""; mstate=""; pstate=""; code=""
  while :; do
    code=$(curl -sS -o "$tmp/matte.json" -w '%{http_code}' --max-time 30 "$url/api/matte")
    if [ "$code" = 200 ]; then
      mmodel=$(json_str "$tmp/matte.json" '.defaultModel' defaultModel)
      mstate=$(matte_model_state "$tmp/matte.json" "$mmodel")
      pstate=$(matte_model_state "$tmp/matte.json" "$pixel_model")
      if matte_enabled "$tmp/matte.json" && [ "$mstate" = ready ]; then
        matte_ready=1
        # … and for the Phase 5c tracker (sam2-tiny) when it is offered: the
        # matte-track / matte-prompt cases need it ready.
        if matte_model_settled "$tmp/matte.json" "$pixel_model" && matte_model_settled "$tmp/matte.json" sam2-tiny; then break; fi
      fi
    fi
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 5
  done
  mdev=$(json_str "$tmp/matte.json" '.device' device)
  # figure = 1 when the pixel model answers the Phase 5 recipes; 0 = the default
  # model does, and the assertions that need a detected figure are skipped.
  # tracker = 1 when the Phase 5c guided model (sam2-tiny) is ready.
  figure=0; pmodel=""; fig_note=""; tracker=0
  if [ "$matte_ready" = 1 ]; then
    ok "GET /api/matte → enabled, default model '$mmodel' ready on '$mdev'"
    case "$mdev" in cuda|cpu) ok "matte: device '$mdev'" ;; *) fail "matte: device '$mdev', want cuda or cpu" ;; esac
    ms=$(json_num "$tmp/matte.json" '.maxSeconds' maxSeconds); mf=$(json_num "$tmp/matte.json" '.maxFrames' maxFrames)
    if [ "${ms:-0}" -gt 0 ] 2>/dev/null && [ "${mf:-0}" -gt 0 ] 2>/dev/null; then ok "matte: maxSeconds $ms / maxFrames $mf"; else fail "matte: maxSeconds '${ms:-?}' / maxFrames '${mf:-?}', want both > 0"; fi
    mspf=$(matte_model_ms "$tmp/matte.json" "$mmodel")
    if [ "$use_jq" = 1 ]; then
      if awk -v v="${mspf:-0}" 'BEGIN { exit !(v > 0) }'; then ok "matte: $mmodel msPerFrame $mspf (measured by the self-test)"; else fail "matte: $mmodel msPerFrame '${mspf:-?}', want > 0"; fi
    fi
    code=$(curl -sS -o "$tmp/caps5b.json" -w '%{http_code}' --max-time 30 "$url/api/capabilities")
    if [ "$code" = 200 ] && features_true "$tmp/caps5b.json" matte; then ok "capabilities: features.matte == true"; else fail "capabilities: features.matte is not true ($code)"; fi
    # Phase 5c status fields (a sidecar / server from before 5c has none — a skip, not a failure).
    devs=$(matte_devices_text "$tmp/matte.json")
    if [ -n "$devs" ]; then
      if matte_devices_has "$tmp/matte.json" "$mdev"; then ok "matte: devices $devs lists the effective device '$mdev'"; else fail "matte: devices $devs does not list the effective device '$mdev'"; fi
      dmd=$(matte_default_model_for "$tmp/matte.json" "$mdev")
      if [ -n "$dmd" ] && [ "$dmd" = "$mmodel" ]; then ok "matte: defaultModels.$mdev == defaultModel ('$mmodel')"; else fail "matte: defaultModels.$mdev is '${dmd:-?}', defaultModel is '$mmodel' — want them equal (the default for the effective device)"; fi
    else
      skip "matte: Phase 5c status fields (devices, defaultModels) absent — a pre-5c sidecar or server"
    fi
    tstate=$(matte_model_state "$tmp/matte.json" sam2-tiny)
    if [ -n "$tstate" ]; then
      tk=$(matte_model_kind "$tmp/matte.json" sam2-tiny)
      if [ "$tk" = tracker ]; then ok "matte: sam2-tiny carries kind 'tracker'"; else fail "matte: sam2-tiny kind '${tk:-}', want 'tracker'"; fi
      if [ "$tstate" = ready ]; then tracker=1; ok "matte: the guided model sam2-tiny is ready — the matte-track / matte-prompt cases run"
      else skip "matte-track / matte-prompt: sam2-tiny is '$tstate' on this sidecar$( r=$(matte_model_reason "$tmp/matte.json" sam2-tiny); [ -n "$r" ] && printf ' (%s)' "$r")"; fi
    else
      skip "matte-track / matte-prompt: sam2-tiny is not offered by this sidecar (MATTE_MODELS, or a pre-5c image)"
    fi
    if [ "$pstate" = ready ]; then
      figure=1; pmodel=$pixel_model
      ok "matte: pixel model '$pmodel' ready — the Phase 5 recipes name it and the figure assertions run"
    else
      pmodel=$mmodel
      if [ -n "$pstate" ]; then pwhy="'$pixel_model' is '$pstate' on this sidecar$( r=$(matte_model_reason "$tmp/matte.json" "$pixel_model"); [ -n "$r" ] && printf ' (%s)' "$r")"
      else pwhy="'$pixel_model' is not offered by this sidecar (MATTE_MODELS)"; fi
      fig_note="$pwhy, so the recipes name the default model '$mmodel' — isnet-anime is trained on anime characters and does not detect the synthetic figure this test draws (alpha ~2 everywhere); the pixel checks need birefnet-lite (the matte-gpu profile, or MATTE_MODELS=isnet-anime,birefnet-lite)"
      skip "matte figure assertions (frames > 1, opaque AND transparent pixels, corner alpha, report.hasAlpha, VP8X alpha, autocrop < source): $fig_note"
    fi
  else
    fail "GET /api/matte → ${code:-?}: not ready after ${matte_wait}s (enabled: $(matte_enabled "$tmp/matte.json" && echo true || echo false), model '${mmodel:-?}' state '${mstate:-?}', device '${mdev:-?}', reason '$(json_str "$tmp/matte.json" '.reason' reason)')"
  fi

  # ---- matte-settings (Phase 5c): the server-side device preference behind the
  # card's "Run on" select. Before any pass, so the switch disturbs nothing; the
  # device in force before the case ($mdev) is restored at the end.
  name=matte-settings
  if [ "$matte_ready" = 1 ]; then
    code=$(curl -sS -o "$tmp/settings_bad.json" -w '%{http_code}' --max-time 30 -X PUT -H 'Content-Type: application/json' --data '{"device":"tpu"}' "$url/api/matte/settings")
    if [ "$code" = 400 ]; then ok "$name: PUT /api/matte/settings {\"device\":\"tpu\"} → 400 (not offered)"
    else fail "$name: PUT /api/matte/settings {\"device\":\"tpu\"} → $code, want 400 for a device the sidecar does not offer: $(head -c 200 "$tmp/settings_bad.json")"; fi
    if matte_devices_has "$tmp/matte.json" cuda && matte_devices_has "$tmp/matte.json" cpu; then
      code=$(curl -sS -o "$tmp/settings_cpu.json" -w '%{http_code}' --max-time 30 -X PUT -H 'Content-Type: application/json' --data '{"device":"cpu"}' "$url/api/matte/settings")
      sdev=$(json_str "$tmp/settings_cpu.json" '.device' device)
      if [ "$code" = 200 ] && [ "$sdev" = cpu ]; then ok "$name: PUT {\"device\":\"cpu\"} → 200 with device 'cpu'"
      else fail "$name: PUT {\"device\":\"cpu\"} → $code with device '${sdev:-?}', want 200 / cpu: $(head -c 200 "$tmp/settings_cpu.json")"; fi
      dm=$(json_str "$tmp/settings_cpu.json" '.defaultModel' defaultModel); dmc=$(matte_default_model_for "$tmp/settings_cpu.json" cpu)
      if [ -n "$dmc" ] && [ "$dm" = "$dmc" ]; then ok "$name: defaultModel '$dm' follows the device (== defaultModels.cpu)"
      else fail "$name: defaultModel '${dm:-?}' vs defaultModels.cpu '${dmc:-?}' after switching to cpu — want them equal"; fi
      code=$(curl -sS -o "$tmp/matte_cpu.json" -w '%{http_code}' --max-time 30 "$url/api/matte")
      gdev=$(json_str "$tmp/matte_cpu.json" '.device' device)
      if [ "$code" = 200 ] && [ "$gdev" = cpu ]; then ok "$name: GET /api/matte reflects device 'cpu'"; else fail "$name: GET /api/matte → $code with device '${gdev:-?}' after PUT cpu, want cpu"; fi
      code=$(curl -sS -o "$tmp/settings_reset.json" -w '%{http_code}' --max-time 30 -X PUT -H 'Content-Type: application/json' --data '{"device":""}' "$url/api/matte/settings")
      rdev=$(json_str "$tmp/settings_reset.json" '.device' device)
      if [ "$code" = 200 ] && matte_devices_has "$tmp/settings_reset.json" "$rdev"; then ok "$name: PUT {\"device\":\"\"} → 200, back to the sidecar's default '$rdev'"
      else fail "$name: PUT {\"device\":\"\"} → $code with device '${rdev:-?}', want 200 and an offered device: $(head -c 200 "$tmp/settings_reset.json")"; fi
      # Restore what was in force before (a long-lived EZLG_URL stack keeps its preference).
      if [ "$rdev" != "$mdev" ]; then
        code=$(curl -sS -o "$tmp/settings_restore.json" -w '%{http_code}' --max-time 30 -X PUT -H 'Content-Type: application/json' --data '{"device":"'"$mdev"'"}' "$url/api/matte/settings")
        if [ "$code" = 200 ]; then log "$name: restored the device preference '$mdev'"; else fail "$name: could not restore the device preference '$mdev' ($code)"; fi
      fi
    else
      skip "$name: the device switch needs a sidecar offering cuda AND cpu (devices: ${devs:-none}) — the matte-gpu profile does"
    fi
  fi

  # ---- the figure-over-gradient clip + the matte jobs (the Phase 5c idle /
  # eager still first, then every job at once: the same clip at the same fps
  # shares one pass through the single-flight)
  char_hash=""
  if [ "$matte_ready" = 1 ]; then
    if make_matte_clip "$tmp/char.mov" 2>"$tmp/char.err" && [ -s "$tmp/char.mov" ]; then
      if code=$(upload "$tmp/upload_char.json" -F "file=@$tmp/char.mov"); then ok "POST /api/upload (figure-over-gradient clip) → 200"; else fail "POST /api/upload (figure-over-gradient clip) → $code"; fi
      char_hash=$(json_str "$tmp/upload_char.json" '.hash' hash)
      [[ "$char_hash" =~ ^[0-9a-f]{64}$ ]] || { fail "figure clip upload response has no sha256 hash"; char_hash=""; }
    else
      fail "ffmpeg could not draw the figure-over-gradient clip: $(head -c 300 "$tmp/char.err")"
    fi
  fi
  if [ -n "$char_hash" ]; then
    # Every recipe names its model (the pixel model when ready, else the default — see matte-api).
    mt='{"kind":"matte","params":{"model":"'"$pmodel"'"}}'
    # The autocrop reads the matte's soft alpha (alpha ≥ threshold = content):
    # 32 keeps the model's ~2 floor out of the box; the figure itself is 255.
    recipe[matte-gif]='{"v":1,"sources":["'"$char_hash"'"],"ops":['"$mt"'],"output":{"format":"gif","width":128,"height":128,"fit":"contain","fps":20,"preset":"emote","target":"emote"}}'
    recipe[matte-webp]='{"v":1,"sources":["'"$char_hash"'"],"ops":['"$mt"'],"output":{"format":"webp","quality":80,"preset":"chat","target":"attachment"}}'
    recipe[matte-apng]='{"v":1,"sources":["'"$char_hash"'"],"ops":['"$mt"',{"kind":"morph","params":{"close":true}}],"output":{"format":"apng","colors":256,"width":320,"height":320,"fit":"contain","preset":"sticker","target":"sticker"}}'
    recipe[matte-autocrop]='{"v":1,"sources":["'"$char_hash"'"],"ops":[{"kind":"trim","params":{"start":0,"end":0.1}},'"$mt"',{"kind":"autocrop","params":{"threshold":32}}],"output":{"format":"png","preset":"custom"}}'
    # Phase 5c: the same emote GIF with the temporal median, with the figure's body
    # colour kept, and (when the tracker is ready) guided by one box on frame 0.
    mt_stab='{"kind":"matte","params":{"model":"'"$pmodel"'","stabilise":"light"}}'
    mt_keep='{"kind":"matte","params":{"model":"'"$pmodel"'","keep":["3a7bd5"]}}'
    # The sprite (64×96) is overlaid at x = 48 + 33.6·sin(πt), y = 54.4 + 22.4·cos(πt)
    # (make_matte_clip): at t = 0 its pixels span x 58..102, y 54..150 of the 160²
    # frame → [0.36, 0.34, 0.64, 0.94]; ~4 % looser on every side.
    track_box='[0.33,0.31,0.67,0.97]'
    mt_track='{"kind":"matte","params":{"model":"sam2-tiny","prompts":[{"frame":0,"box":'"$track_box"'}]}}'
    gif_out='"output":{"format":"gif","width":128,"height":128,"fit":"contain","fps":20,"preset":"emote","target":"emote"}'
    recipe[matte-stabilise]='{"v":1,"sources":["'"$char_hash"'"],"ops":['"$mt_stab"'],'"$gif_out"'}'
    recipe[matte-keep]='{"v":1,"sources":["'"$char_hash"'"],"ops":['"$mt_keep"'],'"$gif_out"'}'
    recipe[matte-track]='{"v":1,"sources":["'"$char_hash"'"],"ops":['"$mt_track"'],'"$gif_out"'}'
    # The 5c follow-up's mask prompt: no box — frame 0's mask is the edge model's
    # matte of it, which the server takes from that model's memo (the pass the
    # eager still / the GIF jobs run on this clip at the same rate); "edge" names
    # the pixel model explicitly so the mask is the matte that detects the figure
    # (the device's default would be isnet-anime under a CPU preference: an empty mask).
    mt_track_mask='{"kind":"matte","params":{"model":"sam2-tiny","edge":"'"$pmodel"'","prompts":[{"frame":0,"maskFrom":"edge"}]}}'
    recipe[matte-track-mask]='{"v":1,"sources":["'"$char_hash"'"],"ops":['"$mt_track_mask"'],'"$gif_out"'}'
    # POST /api/matte/prompt with it, in the shape the SPA puts on the wire: the
    # recipe word maskFrom plus matte.FramePrompt's own flag "mask": true (the
    # server reads either; a recipe carries maskFrom only).
    pmask_body='{"src":"'"$char_hash"'","ops":['"$mt_track_mask"'],"output":{"format":"gif","fps":20},"frame":0,"prompts":[{"frame":0,"maskFrom":"edge","mask":true}]}'

    # ---- matte-still (Phase 5c Compute-button semantics), BEFORE any pass exists
    # for this clip: a plain still answers 202 "idle" and starts nothing; the same
    # still with "eager": true (the Compute matte button) starts the pass and is
    # re-requested through running / loading / downloading until the PNG arrives.
    name=matte-still
    still_body='{"src":"'"$char_hash"'","ops":['"$mt"'],"output":{"format":"gif","fps":20},"t":0.5,"maxW":160}'
    st=""; code=$(still_once "$tmp/still_idle.json" "$still_body")
    case "$code" in
      202)
        st=$(json_str "$tmp/still_idle.json" '.state' state)
        if [ "$st" = idle ]; then ok "$name: POST /api/still without eager → 202 {\"state\":\"idle\"} (no pass started)"
        else fail "$name: POST /api/still without eager → 202 state '${st:-?}', want idle (a preview must not start a pass by itself)"; fi ;;
      200) skip "$name: idle check — the matte memo was already on disk (200 at once; a fresh data dir exercises it)" ;;
      *)   fail "$name: POST /api/still without eager → $code: $(head -c 300 "$tmp/still_idle.json")" ;;
    esac
    if [ "$code" = 202 ] && [ "$st" = idle ]; then
      # Still idle a second later: the first plain still really started nothing.
      sleep 1
      code2=$(still_once "$tmp/still_idle2.json" "$still_body"); st2=$(json_str "$tmp/still_idle2.json" '.state' state)
      if [ "$code2" = 202 ] && [ "$st2" = idle ]; then ok "$name: a second plain still 1 s later is still idle"
      else fail "$name: a second plain still → $code2 state '${st2:-}', want 202 idle (the first plain still started a pass)"; fi
    fi
    # ---- matte-prompt-mask, the idle half (5c follow-up), still BEFORE any pass
    # exists for this clip: a mask prompt needs the edge model's matte of the frame
    # on disk and never starts a pass, so the overlay answers 202 idle with the
    # reason. A released tracker may answer loading / downloading first (nothing
    # stays resident): those are re-requested, the first other answer is judged.
    name=matte-prompt-mask
    if [ "$tracker" = 1 ]; then
      pcode=""; pst=""; deadline=$((SECONDS + timeout))
      while :; do
        pcode=$(prompt_once "$tmp/pmask_idle.json" "$pmask_body")
        [ "$pcode" = 202 ] || break
        pst=$(json_str "$tmp/pmask_idle.json" '.state' state)
        case "$pst" in loading|downloading) ;; *) break ;; esac
        [ "$SECONDS" -lt "$deadline" ] || break
        sleep 1
      done
      case "$pcode" in
        202)
          if [ "$pst" = idle ]; then
            ok "$name: POST /api/matte/prompt with {\"frame\":0,\"maskFrom\":\"edge\",\"mask\":true} before any pass → 202 {\"state\":\"idle\"} (the edge matte is not computed; no pass started)"
            if grep -qiE 'comput|matte first' "$tmp/pmask_idle.json"; then ok "$name: the 202 names the reason (compute the edge model's matte first)"
            else fail "$name: the 202 idle body carries no reason to show (want \"compute the General matte first\" or alike): $(head -c 300 "$tmp/pmask_idle.json")"; fi
          else fail "$name: POST /api/matte/prompt with the mask prompt before any pass → 202 state '${pst:-?}', want idle (a mask prompt must not start a pass): $(head -c 300 "$tmp/pmask_idle.json")"; fi ;;
        200) skip "$name: idle half — the edge model's matte was already on disk (200 at once; a fresh data dir exercises it)" ;;
        *)   fail "$name: POST /api/matte/prompt with the mask prompt before any pass → $pcode: $(head -c 300 "$tmp/pmask_idle.json")" ;;
      esac
    else
      skip "$name (idle half): needs the guided model sam2-tiny ready on the sidecar (see matte-api)"
    fi

    still_code=""; still_pending_seen=0
    if still_png_wait "$tmp/still_matte.png" "${still_body%\}}"',"eager":true}'; then
      if [ "$still_pending_seen" = 1 ]; then
        if [ "$still_first_state" = idle ]; then fail "$name: the eager still answered 202 idle (eager must start the pass)"
        else ok "$name: POST /api/still with \"eager\": true → 202 ${still_first_state:-pending}, then 200 PNG"; fi
      else ok "$name: POST /api/still with \"eager\": true → 200 PNG (memo already on disk, no 202 seen)"; fi
      if [ "$figure" = 1 ]; then
        ca=$(corner_alpha "$tmp/still_matte.png")
        if [ "${ca:-255}" -le 8 ] 2>/dev/null; then ok "$name: corner pixel transparent (alpha $ca ≤ 8, the soft matte's floor)"; else fail "$name: corner pixel alpha '${ca:-?}', want ≤ 8 (the matte did not key the gradient)"; fi
      else
        skip "$name: corner-alpha check needs a detected figure ($pmodel does not find the synthetic one)"
      fi
    else
      fail "$name: POST /api/still with the matte op → ${still_code:-?} (want 200 PNG, after any 202s): $(head -c 300 "$tmp/still_matte.png")"
    fi

    # The jobs, all at once (the GIF ones share the still's memo; the tracker
    # recipe only when sam2-tiny is ready).
    for name in matte-gif matte-webp matte-apng matte-autocrop matte-stabilise matte-keep; do
      submit_job $name || true
    done
    if [ "$tracker" = 1 ]; then submit_job matte-track || true; fi

    # ---- matte-gif (emote)
    name=matte-gif
    if finish_job $name && fetch_primary $name gif; then
      f=${out_file[$name]}
      if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
      matte_report_check $name
      gif_figure_checks $name "$figure"
    fi

    # ---- matte-webp (chat)
    name=matte-webp
    if finish_job $name && fetch_primary $name webp; then
      f=${out_file[$name]}
      if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: webp decodes (ffmpeg)"; else fail "$name: webp does not decode"; fi
      matte_report_check $name
      if [ "$figure" = 1 ]; then
        if primary_has_alpha "$tmp/poll_$name.json"; then ok "$name: report.hasAlpha == true"; else fail "$name: report.hasAlpha != true"; fi
        if have "$webpinfo"; then
          winfo=$("$webpinfo" "$f" 2>&1 || true)
          va=$(awk '/Chunk VP8X/ {x=1} x && /Alpha:/ {print $2; exit}' <<<"$winfo")
          if [ "$va" = 1 ]; then ok "$name: VP8X ALPHA flag set"; else fail "$name: VP8X ALPHA flag missing"; fi
        fi
        # Soft alpha: the model's sigmoid floor keeps the background at ~2, so ±8 counts.
        read -r n_op n_tr <<<"$(alpha_counts "$f" 8)"
        if [ "${n_op:-0}" -gt 0 ] && [ "${n_tr:-0}" -gt 0 ]; then ok "$name: frame 0 has $n_op opaque (≥ 247) and $n_tr transparent (≤ 8) pixels"; else fail "$name: frame 0 has ${n_op:-0} opaque (≥ 247) / ${n_tr:-0} transparent (≤ 8) pixels, want both > 0"; fi
        ca=$(corner_alpha "$f")
        if [ "${ca:-255}" -le 8 ] 2>/dev/null; then ok "$name: corner pixel transparent (alpha $ca ≤ 8)"; else fail "$name: corner pixel alpha '${ca:-?}', want ≤ 8"; fi
      else
        skip "$name: figure assertions (hasAlpha, VP8X alpha, opaque AND transparent pixels, corner alpha) need a detected figure ($pmodel does not find the synthetic one)"
      fi
    fi

    # ---- matte-apng (sticker, + morph close after the matte)
    name=matte-apng
    if finish_job $name && fetch_primary $name png; then
      f=${out_file[$name]}
      if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: apng decodes (ffmpeg)"; else fail "$name: apng does not decode"; fi
      if [ "$(png_chunks "$f" acTL)" -ge 1 ]; then ok "$name: animated (acTL chunk)"; else fail "$name: no acTL chunk — not an APNG"; fi
      matte_report_check $name
      if [ "$figure" = 1 ]; then
        if primary_has_alpha "$tmp/poll_$name.json"; then ok "$name: report.hasAlpha == true"; else fail "$name: report.hasAlpha != true"; fi
        # Soft alpha (indexed, but the palette keeps the ~2 floor): ±8 counts, as for the WebP.
        read -r n_op n_tr <<<"$(alpha_counts "$f" 8)"
        if [ "${n_op:-0}" -gt 0 ] && [ "${n_tr:-0}" -gt 0 ]; then ok "$name: frame 0 has $n_op opaque (≥ 247) and $n_tr transparent (≤ 8) pixels"; else fail "$name: frame 0 has ${n_op:-0} opaque (≥ 247) / ${n_tr:-0} transparent (≤ 8) pixels, want both > 0"; fi
        ca=$(corner_alpha "$f")
        if [ "${ca:-255}" -le 8 ] 2>/dev/null; then ok "$name: corner pixel transparent (alpha $ca ≤ 8)"; else fail "$name: corner pixel alpha '${ca:-?}', want ≤ 8"; fi
      else
        skip "$name: figure assertions (hasAlpha, opaque AND transparent pixels, corner alpha) need a detected figure ($pmodel does not find the synthetic one)"
      fi
    fi

    # ---- matte-autocrop: the crop found the figure, not the gradient
    name=matte-autocrop
    if finish_job $name && fetch_primary $name png; then
      f=${out_file[$name]}
      d=$(dims_of "$f"); w=${d%x*}; h=${d#*x}
      if [ "$figure" = 1 ]; then
        if [ "${w:-0}" -gt 0 ] && [ "${h:-0}" -gt 0 ] && { [ "$w" -lt 160 ] || [ "$h" -lt 160 ]; }; then
          ok "$name: cropped to $d (< 160x160 — the AI matte located the figure)"
        else
          fail "$name: dims '$d', want smaller than the 160x160 source"
        fi
      else
        if [ "${w:-0}" -gt 0 ] && [ "${h:-0}" -gt 0 ]; then ok "$name: matte + autocrop rendered a ${d} PNG"; else fail "$name: dims '$d' — no readable PNG"; fi
        skip "$name: crop smaller than the 160x160 source needs a detected figure ($pmodel does not find the synthetic one; got $d)"
      fi
    fi

    # ---- matte-stabilise (Phase 5c): the emote GIF through the light temporal
    # median — a derived sequence next to the mattes, no second pass; the
    # render.matte check names it.
    name=matte-stabilise
    if finish_job $name && fetch_primary $name gif; then
      f=${out_file[$name]}
      if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
      matte_report_check $name
      d=$(primary_check_detail "$tmp/poll_$name.json" render.matte)
      if grep -qiE 'stabilis(e|ed|ation)[^a-z0-9]{0,3}light' <<<"$d"; then ok "$name: render.matte names the stabilise mode ($d)"
      else fail "$name: render.matte does not name \"stabilise light\": '${d:-<no render.matte check>}'"; fi
      gif_figure_checks $name "$figure"
    fi

    # ---- matte-keep (Phase 5c): the figure's body colour kept — forced opaque as
    # a union with the matte — so the figure assertions hold WHATEVER the model
    # finds (the body rectangle orbits, the gradient never comes within 0.08 of it).
    name=matte-keep
    if finish_job $name && fetch_primary $name gif; then
      f=${out_file[$name]}
      if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
      if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"
      else fail "$name: primary report.ok != true (failed: $(primary_failed_rules "$tmp/poll_$name.json" | tr '\n' ' '))"; fi
      d=$(primary_check_detail "$tmp/poll_$name.json" render.matte)
      if grep -qi 'keep' <<<"$d"; then ok "$name: render.matte names the keep colour ($d)"
      else fail "$name: render.matte does not mention the keep colour: '${d:-<no render.matte check>}'"; fi
      gif_figure_checks $name 1
    fi

    # ---- matte-track (Phase 5c guided mode): one box on frame 0 around the
    # figure's known position → the tracker finds it whatever the edge model does.
    name=matte-track
    if [ "$tracker" = 1 ]; then
      if finish_job $name && fetch_primary $name gif; then
        f=${out_file[$name]}
        if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
        if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"
        else fail "$name: primary report.ok != true (failed: $(primary_failed_rules "$tmp/poll_$name.json" | tr '\n' ' '))"; fi
        d=$(primary_check_detail "$tmp/poll_$name.json" render.matte)
        if grep -qF 'sam2-tiny' <<<"$d"; then ok "$name: render.matte names the tracker ($d)"
        else fail "$name: render.matte does not name sam2-tiny: '${d:-<no render.matte check>}'"; fi
        gif_figure_checks $name 1
      fi
    else
      skip "$name: needs the guided model sam2-tiny ready on the sidecar (see matte-api)"
    fi

    # ---- matte-prompt (Phase 5c): the live mask behind the Select-subject panel.
    name=matte-prompt
    if [ "$tracker" = 1 ]; then
      prompt_code=""
      pbody='{"src":"'"$char_hash"'","ops":['"$mt_track"'],"output":{"format":"gif","fps":20},"frame":0,"prompts":{"obj":1,"prompts":[{"frame":0,"box":'"$track_box"'}]}}'
      if prompt_png_wait "$tmp/prompt_mask.png" "$pbody"; then
        ok "$name: POST /api/matte/prompt (box on frame 0) → 200 PNG"
        read -r n_w n_b <<<"$(gray_counts "$tmp/prompt_mask.png")"
        if [ "${n_w:-0}" -gt 0 ] && [ "${n_b:-0}" -gt 0 ]; then ok "$name: mask has $n_w white and $n_b black pixels"; else fail "$name: mask has ${n_w:-0} white / ${n_b:-0} black pixels, want both > 0 (a binary 0/255 mask of the figure)"; fi
        gc=$(gray_corner "$tmp/prompt_mask.png")
        if [ "${gc:-255}" = 0 ]; then ok "$name: corner pixel outside the box is 0"; else fail "$name: corner pixel '${gc:-?}', want 0 (outside the box)"; fi
      else
        fail "$name: POST /api/matte/prompt → ${prompt_code:-?} (want 200 PNG, after any 202s): $(head -c 300 "$tmp/prompt_mask.png")"
      fi
    else
      skip "$name: needs the guided model sam2-tiny ready on the sidecar (see matte-api)"
    fi

    # ---- matte-prompt-mask, the PNG half: the per-frame pass on this clip is on
    # disk now (the eager still and the GIF jobs), so the same request answers the
    # tracker's mask of frame 0 conditioned on the pixel model's matte of it.
    name=matte-prompt-mask
    if [ "$tracker" = 1 ] && [ "$figure" = 1 ]; then
      pcode=""; pst=""; deadline=$((SECONDS + timeout))
      while :; do
        pcode=$(prompt_once "$tmp/pmask.png" "$pmask_body")
        [ "$pcode" = 202 ] || break
        pst=$(json_str "$tmp/pmask.png" '.state' state)
        [ "$pst" != idle ] || break   # idle after the pass = the edge matte was not found: no point waiting
        [ "$SECONDS" -lt "$deadline" ] || break
        sleep 1
      done
      if [ "$pcode" = 200 ] && [ "$(magic_hex "$tmp/pmask.png" 8)" = "89504e470d0a1a0a" ]; then
        ok "$name: POST /api/matte/prompt with the mask prompt after the per-frame pass → 200 PNG"
        read -r n_w n_b <<<"$(gray_counts "$tmp/pmask.png")"
        if [ "${n_w:-0}" -gt 0 ] && [ "${n_b:-0}" -gt 0 ]; then ok "$name: mask has $n_w white and $n_b black pixels"; else fail "$name: mask has ${n_w:-0} white / ${n_b:-0} black pixels, want both > 0 (a binary 0/255 mask of the figure)"; fi
        gc=$(gray_corner "$tmp/pmask.png")
        if [ "${gc:-255}" = 0 ]; then ok "$name: corner pixel outside the figure is 0"; else fail "$name: corner pixel '${gc:-?}', want 0 (outside the figure)"; fi
      elif [ "$pcode" = 202 ] && [ "$pst" = idle ]; then
        fail "$name: POST /api/matte/prompt with the mask prompt → 202 idle AFTER the per-frame pass of $pmodel on this clip: the edge matte of frame 0 was not found on disk (the mask prompt must read the memo the per-frame pass wrote): $(head -c 300 "$tmp/pmask.png")"
      else
        fail "$name: POST /api/matte/prompt with the mask prompt → ${pcode:-?} (want 200 PNG after the per-frame pass, after any loading 202s): $(head -c 300 "$tmp/pmask.png")"
      fi
    elif [ "$tracker" = 1 ]; then
      skip "$name (PNG half): needs a detected figure — the mask is $pmodel's matte of frame 0, empty without one ($fig_note)"
    else
      skip "$name (PNG half): needs the guided model sam2-tiny ready on the sidecar (see matte-api)"
    fi

    # ---- matte-track-mask (5c follow-up): the guided recipe prompted with the
    # pixel model's matte of frame 0 — submitted now, after the per-frame jobs,
    # so "run General first, then track from the frame it got right" is the
    # literal order (a render would run the edge pass itself anyway).
    name=matte-track-mask
    if [ "$tracker" = 1 ] && [ "$figure" = 1 ]; then
      if submit_job $name && finish_job $name && fetch_primary $name gif; then
        f=${out_file[$name]}
        if "$ffmpeg" -v error -nostdin -i "$f" -f null - 2>/dev/null; then ok "$name: gif decodes (ffmpeg)"; else fail "$name: gif does not decode"; fi
        if primary_report_ok "$tmp/poll_$name.json"; then ok "$name: primary report.ok == true"
        else fail "$name: primary report.ok != true (failed: $(primary_failed_rules "$tmp/poll_$name.json" | tr '\n' ' '))"; fi
        d=$(primary_check_detail "$tmp/poll_$name.json" render.matte)
        if grep -qF 'sam2-tiny' <<<"$d"; then ok "$name: render.matte names the tracker ($d)"
        else fail "$name: render.matte does not name sam2-tiny: '${d:-<no render.matte check>}'"; fi
        if grep -qi 'mask' <<<"$d"; then ok "$name: render.matte notes the mask prompt ($d)"
        else fail "$name: render.matte does not note the mask prompt (want \"mask from the edge matte\"): '${d:-<no render.matte check>}'"; fi
        gif_figure_checks $name 1
      fi
    elif [ "$tracker" = 1 ]; then
      skip "$name: needs a detected figure — the mask prompt is $pmodel's matte of frame 0, empty without one ($fig_note)"
    else
      skip "$name: needs the guided model sam2-tiny ready on the sidecar (see matte-api)"
    fi

    # ---- matte-cached: the same recipe again comes from the result cache (the
    # sidecar's weights identity is in the recipe hash, so a hit is the contract
    # here — this one is NOT counted in the cache-hit warning).
    name=matte-cached
    recipe[$name]=${recipe[matte-gif]}
    if submit_job $name; then
      state=$(wait_job $name)
      case "$state" in
        done)
          if job_cached "$tmp/poll_$name.json"; then ok "$name: the matte GIF recipe again → done with \"cached\": true"
          else fail "$name: the same matte recipe rendered again instead of being served from the result cache"; fi ;;
        error)  fail "$name: second submit failed: $(job_error "$tmp/poll_$name.json")" ;;
        http:*) fail "GET /api/jobs/${job_id[$name]} → ${state#http:}" ;;
        *)      fail "$name: second submit still '$state' after ${timeout}s" ;;
      esac
    fi

    # ---- matte-unload (Phase 5c): what the card sends when it leaves the AI
    # mode — last, after every matte job, so no pass loses its session.
    name=matte-unload
    code=$(curl -sS -o "$tmp/unload.txt" -w '%{http_code}' --max-time 60 -X POST "$url/api/matte/unload")
    if [ "$code" = 204 ]; then ok "$name: POST /api/matte/unload → 204"; else fail "$name: POST /api/matte/unload → $code, want 204: $(head -c 200 "$tmp/unload.txt")"; fi
  fi
fi

summary
[ "$failn" -eq 0 ]
