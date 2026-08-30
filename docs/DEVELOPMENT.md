# Development

The full development loop, scripts, test matrix and the Discord acceptance test.
The short version (and the directory map) is in the
[README](../README.md#developing-and-running-locally); running, configuration and
troubleshooting live in [`USAGE.md`](USAGE.md); the architecture, Discord rules
and phase plan live in [`DESIGN.md`](DESIGN.md).

## Development loop

Go is **not** required on the host: everything runs in the `dev` image (tools + Go 1.26 + Node 22)
with the repo bind-mounted at `/src`.

```sh
docker compose -f compose.yaml -f compose.dev.yaml up --build
#   UI  (Vite dev server, hot reload)   http://localhost:5173   ← use this one while developing
#   API (go build + ezlg serve)         http://localhost:8080   (proxied by Vite for /api and /out)
```

The Vite dev server proxies `/api` and `/out` to `:8080` (configured in `web/vite.config.ts`).
The Go server is not auto-restarted on edits: `docker compose -f compose.yaml -f compose.dev.yaml
restart app` rebuilds and restarts it (Ctrl-C / `stop` sends SIGTERM through `init: true` so it
drains cleanly). Or run things by hand in one interactive container — `run --rm` starts a
throw-away container (its name is random, so `docker exec -it ezlg-dev …` from the `up` flow does
not apply here); `--service-ports` publishes 8080 and 5173:

```sh
docker compose -f compose.yaml -f compose.dev.yaml run --rm --service-ports app bash
# inside the container (all in the same shell):
cd web && npm ci && (npm run dev -- --host 0.0.0.0 &)   # Vite in the background → :5173
cd .. && go run ./cmd/ezlg serve                        # API → :8080; Ctrl-C, edit, re-run
```

`npm ci` is needed once per fresh `ezlg-webnode` volume (the dev stack keeps `web/node_modules`
in a named volume, not in the bind mount). If you do prefer a second shell, use
`docker compose -f compose.yaml -f compose.dev.yaml exec app bash` while `up` is running (that
container *is* named `ezlg-dev`, so `docker exec -it ezlg-dev bash` works there too).

Useful one-liners:

```sh
# Go commands from the Windows host (golang:1.26-trixie, caches in named volumes)
.\scripts\go.ps1 vet ./...
.\scripts\go.ps1 test ./...
./scripts/go.sh test ./internal/discordlint/...     # Linux/macOS/WSL equivalent

# Go tests inside the real toolchain image
docker compose -f compose.yaml -f compose.dev.yaml run --rm app go test ./...

# End-to-end test (starts the server in the container on a throw-away temp data dir — the dev
# image's EZLG_DATA=/data is deliberately ignored so every re-run really re-renders instead of
# being answered from the on-disk result cache — then uploads a ProRes clip, renders the Phase 1
# GIF + WebP, then the Phase 2 cases: emote fit-to-size with alternatives, indexed APNG sticker,
# animated AVIF, PNG/JPEG stills, frames + zip, 3-PNG image sequence, GIF optimise, edit-as-source;
# then the Phase 3 cases: chromakey / colorkey on a green-screen clip (pixel-checked alpha),
# chromakey + feather (the WebP edge carries intermediate alpha; the unfeathered one does not), text
# overlay, PNG and looping-GIF overlays from a second source (stills prove the GIF overlay is
# painted and loops rather than holding its last frame), reverse (frame hashes vs the forward
# export), autocrop (smaller dims), trim on an animated WebP source built from the ProRes clip
# (exactly 10 frames), the animated proxy (VP8X ANIM), fonts + capability flags; and the Phase 4
# cases: attachment MP4 (h264 / yuv420p / even dims / moov-before-mdat via the lint report) and
# WebM (vp9), bounce (exactly 2N frames, first == last), the gifski encoder (desc names gifski;
# refused for an emote target), the lossless gifsicle fast path (desc), and the /input picker +
# /output save endpoints against temp dirs (start-server runs only))
docker compose -f compose.yaml -f compose.dev.yaml run --rm -e EZLG_START_SERVER=1 app bash scripts/integration-test.sh
#   against an already running stack instead:  EZLG_URL=http://localhost:8080 bash scripts/integration-test.sh
#     (that stack keeps its ezlg-data-dev volume, so repeat recipes are answered from the result
#      cache — the script warns per cached job and in the summary; wipe the volume (down -v) or
#      bump jobs.PipelineVersion to force a re-render, or set EZLG_TEST_STRICT=1 to fail on it)
#   Phase 1 checks only:                       … -e EZLG_TEST_PHASE2=0 …
#   Phase 1 + 2 only (skip Phase 3):           … -e EZLG_TEST_PHASE3=0 …

# Self-test of the Discord test kit (OUTDIR guard, frame-rate handling, variants, scratch; ~1–2 min)
docker compose -f compose.yaml -f compose.dev.yaml run --rm app bash scripts/testkit-test.sh

# Re-check every bundled tool / ffmpeg capability
docker run --rm --entrypoint bash ezlg:local /usr/local/share/ezlg/check-tools.sh
```

Production image build with a version stamp:

```sh
docker build --target runtime -t ezlg:local --build-arg VERSION=$(git describe --tags --always) .
```

Image layout (`Dockerfile`): `web` (npm build) → `gobuild` (static binary, SPA embedded) →
`tools` (trixie-slim + pinned tools — **FFmpeg 9.0.1** (BtbN static build, pinned by hash),
gifsicle 1.96, libwebp 1.5 tools, libavif 1.2, pngquant, oxipng, gifski 1.34, tini — the
DejaVu + Noto core fonts with fontconfig and the `/fonts` scan path, self-checked at build time by
`scripts/check-tools.sh` — tool versions, ffmpeg capabilities, font families, encode/decode
smoke) → `runtime` (non-root `ezlg`, tini, healthcheck) and `dev` (tools + Go + Node, root).
Third-party downloads are pinned by URL and
sha256 in the `ARG`s at the top of the Dockerfile; `scripts/pin-ffmpeg.sh` prints fresh values
when the BtbN autobuild tag is pruned (daily tags live ~2 weeks, month-end tags are permanent).

## CI

`.github/workflows/docker-image.yml` builds the `runtime` target on every push to `master` (and
on manual dispatch) and pushes it to Docker Hub as `duckautomata/ez-local-gif:latest`, stamping
the binary with the short commit SHA via the `VERSION` build-arg. The workflow needs two
repository Actions secrets: `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` (a Docker Hub access
token with Read & Write scope). The Docker Hub repository `duckautomata/ez-local-gif` is created
on the first push.

## Discord acceptance test

Files that look fine in a browser can still break on Discord, which re-encodes every preview
server-side. The Phase 1 acceptance run was done on 2026-08-19: every variant below was uploaded
to a private server and the per-file outcome is recorded in
[`reviews/discord-testkit-results.md`](reviews/discord-testkit-results.md); the consequences for the
encoders are in [`DESIGN.md` §9a](DESIGN.md). Headlines: the ffmpeg-palette GIF paths
(with or without gifsicle), lossy (`yuva420p` and `bgra`) and lossless WebP, the 128² emote GIF
and WebP, the 320² sticker GIF and both APNG stickers all render correctly; gifski's per-frame
palettes do **not** (dark background, ghosting — never offered for Discord targets); APNG
attachments show frame 0 only (sticker-only, as designed); the **indexed 8-bit-alpha APNG at
25 fps is the best sticker** and is the sticker default; animated **AVIF with alpha animates with
soft alpha** as an attachment; all GIFs show a dark 1-bit outline in light mode (inherent —
WebP/AVIF/APNG when soft edges matter).

Re-run the kit after any encoder or linter change and upload the variants to a **private**
Discord server again:

```sh
# writes ./output/testkit/{a..j}_*.{gif,webp,png,avif} + README.md  (15–50 s)
docker compose run --rm --entrypoint bash app /usr/local/share/ezlg/discord-testkit.sh /output/testkit
# or with your own clip (ProRes 4444 straight from Resolve is the point):
docker compose run --rm --entrypoint bash -v "$PWD/input:/input:ro" app \
    /usr/local/share/ezlg/discord-testkit.sh /output/testkit /input/clip.mov
# (dev stack — root, writes to ./output-dev/testkit so nothing root-owned lands in ./output:
#  docker compose -f compose.yaml -f compose.dev.yaml run --rm app bash scripts/discord-testkit.sh /output/testkit)
```

If the kit stops at once with `OUTDIR '/output/testkit' is not writable by uid 1000`, the
`./output` bind is root-owned — see [Troubleshooting](USAGE.md#troubleshooting) for the one-line
fix.

Options: `--straight` / `--premultiplied` override the alpha guess (ProRes defaults to
premultiplied, everything else to straight), `--fps N` sets the master rate (default 25 →
4 cs GIF delays; the synthetic clip is generated at that same rate, and a supplied clip whose
rate differs is resampled — the kit then warns, and its README notes that a periodic cadence
hitch is expected in every file, so pass `--fps <source rate>` for a clean timing check),
`EZLG_TESTKIT_MAX_PX` caps the chat variants (default 480).

The kit decodes the source once through the same alpha head as the app's render chain
(`internal/graph/compile.go`, [`DESIGN.md`](DESIGN.md) §4.1): `yuva*` sources such as ProRes 4444
are unpremultiplied natively at their 10/12-bit depth behind the
`setparams=alpha_mode=premultiplied`
tag and converted to rgba once, RGB-decoded sources go through `format=gbrap[1Nle],…,unpremultiply`
— so the kit's masters carry exactly the alpha a render does (transparent pixels at 0, not the
alpha 1 the old `gbrap12le` detour left; the self-test checks a corner pixel of the lossless WebP
and RGBA APNG). The RGBA masters live on `/dev/shm/ezl-testkit`, or under `$TMPDIR` with a warning
when `/dev/shm` is Docker's default 64 MiB. It emits every encoder path from
[`DESIGN.md`](DESIGN.md) §4.2 / §9:
ffmpeg palette GIF + `gifsicle -O2` (default), the same coalesced with `gifsicle -U`, gifski
(local palettes), ffmpeg-only GIF, animated WebP lossy (`yuva420p` and `bgra` input, e / e2) and
lossless, RGBA APNG, 128×128 emote GIF/WebP fitted under 256 KiB, 320×320 sticker indexed
8-bit-alpha APNG (i3, the default rung, listed first) / GIF / RGBA APNG fitted under 512 KiB, and
an animated AVIF with alpha. `output/testkit/README.md` opens with a "results so far" pointer to
[`reviews/discord-testkit-results.md`](reviews/discord-testkit-results.md), then says for each
file where to upload it (attachment /
Server Settings › Emoji / Server Settings › Stickers), lists the client matrix (desktop, web,
iOS, Android × dark/light theme × autoplay on/off, reduced motion), what to look for (alpha
survives, no black background, no colour flicker, first-frame still, loops forever, timing) and a
sizes table with the fit rung used.

Checklist for sign-off (per client and theme):

1. Attachments **a, b, d, e, e2, f**: transparent over both themes, no black box, no per-frame
   colour changes, loop forever, still frame with autoplay off is a real frame (e vs e2: note any
   difference in soft-edge colour or size — that decides the lossy WebP input format).
2. Emote **h1** (GIF) and **h2** (WebP): upload accepted, animate inline / jumbo / reaction /
   picker, transparent.
3. Sticker **i3** (indexed 8-bit-alpha APNG — the default), **i1** (GIF fallback), **i2** (RGBA
   APNG probe rung): upload accepted (no "frame rate too small or too large"), animate in the
   picker and in chat at the fitted fps, soft alpha kept (i3/i2).
4. **j** (animated AVIF with alpha) as an attachment: animates with soft alpha. **c** (gifski)
   is informational — expected to fail (dark background, ghosting).
5. `g` (APNG attachment) is expected to show only its first frame; that is a Discord limitation.

Record findings (client + version, theme, autoplay, screenshot of anything wrong) in
[`reviews/discord-testkit-results.md`](reviews/discord-testkit-results.md) and their consequences
in [`DESIGN.md`](DESIGN.md) §9a. Rules the linter enforces are versioned in
`internal/discordlint`.

## Scripts

| Script | Purpose |
|---|---|
| `scripts/go.ps1`, `scripts/go.sh` | Run `go …` in `golang:1.26-trixie` with the repo mounted (host has no Go) |
| `scripts/check-tools.sh` | Print + assert every bundled tool, ffmpeg capability (encoders incl. `libx264`/`libvpx-vp9` for the Phase 4 video exports, decoders incl. the libvpx VP8 decoder for VP8-alpha overlays, every filter the graph emits — Phase 3: `tpad`, `reverse`, `lagfun`, `bbox`, `colorchannelmixer`, `setparams`, `blend`, `gblur` (feather), `trim`, `setpts`; Phase 4: `concat` (bounce) — demuxers, muxers incl. `mp4`/`webm`) and font family (DejaVu, Noto; `/fonts` on the scan path; drawtext paints), plus functional smoke encodes (gif/webp/apng/drawtext, and Phase 4: mp4 with `+faststart`, webm, a 2-frame gifski run) — runs at image build, so a build lacking something the pipeline relies on fails there, not at render time |
| `scripts/make-test-clip.sh` | Synthesise a transparent test clip: ProRes 4444 (`.mov`), VP9 alpha (`.webm`), GIF, animated AVIF with alpha (`.avif`: avifenc, or ffmpeg's colour + alpha stream pair without it), `seq OUTDIR N` = N straight-alpha PNG frames for an image-sequence upload, or `green OUT.mov|OUT.mp4` = an opaque 4:4:4 green-screen clip (a bordered square orbiting over `0x00ff00`) for the keying ops; premultiplied or straight alpha |
| `scripts/discord-testkit.sh` | Emit the Discord render-test matrix + README (see above) |
| `scripts/testkit-test.sh` | Self-test for the test kit: OUTDIR guard + hint, synthetic clip at the master rate, resample warning, every variant produced, the app's yuva alpha head (native-depth unpremultiply, one rgba conversion, transparent corner pixel at alpha 0), /dev/shm scratch (full checks need the toolchain image) |
| `scripts/integration-test.sh` | End-to-end API test against a running server (`EZLG_START_SERVER=1` starts one on a throw-away data dir): the 18 Phase 1 checks (ProRes → emote GIF + chat WebP), the Phase 2 cases (fit-to-size + alternatives, indexed APNG sticker, AVIF, PNG/JPEG, frames + zip, image sequence, optimise, from-result) and the Phase 3 cases (chromakey / colorkey on a green-screen clip with pixel-level alpha checks, chromakey + feather (the WebP's edge carries intermediate alpha, the unfeathered render (almost) none), text overlay, PNG + looping GIF overlays from a second source (`/api/still` pixel checks: the GIF overlay is painted and loops rather than holding its last frame), reverse vs the forward frames export, autocrop, trim on an animated WebP source built from the ProRes clip (exactly 10 frames — FFmpeg 9's `webp_anim` demuxer decodes nothing after an input seek, so the server trims such sources in the filtergraph), `POST /api/proxy`, `GET /api/fonts`, capability flags) and the Phase 4 cases (attachment MP4: h264/yuv420p, even dims from an odd request, moov-before-mdat via the `video.faststart` lint row; WebM: vp9/yuv420p; bounce: exactly 2× the forward frame count, first == last frame; gifski encoder: desc names gifski, emote target refused; lossless gifsicle fast path: desc; `/input` picker + `/output` save against temp dirs — start-server runs only, skipped with a note otherwise); `EZLG_TEST_PHASE2=0` for Phase 1 only, `EZLG_TEST_PHASE3=0` to skip Phases 3+4, `EZLG_TEST_PHASE4=0` to skip Phase 4; jobs answered from the server's result cache are warned about (`EZLG_TEST_STRICT=1` fails on them) |
| `scripts/integration-test-selftest.sh` | Unit tests for `integration-test.sh` itself (no server/toolchain needed): `EZLG_START_SERVER=1` must ignore an inherited `EZLG_DATA` (else dev-image re-runs are vacuous cache hits), `EZLG_TEST_DATA` override, cached-result detection, the capture-then-grep helpers vs SIGPIPE, the frames-manifest helpers (first / last frame url must skip `frames.zip` and `delays.json`, with jq and with the grep fallback), and the Phase 4 helpers (`primary_desc`, `primary_check_ok` on lint report rows, `moov_before_mdat` box order) |
| `scripts/pin-ffmpeg.sh` | Print new `FFMPEG_TAG/ASSET/SHA256` ARG lines for a BtbN release tag |
