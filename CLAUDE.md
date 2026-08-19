# ez-local-gif — notes for Claude Code

Self-hosted ezgif replacement. Read `docs/DESIGN.md` first — it is the source
of truth for architecture, Discord rules, encoder commands and the phase plan.

## Layout

- `cmd/ezlg` — main binary (`serve` default, `testkit`, `version`)
- `internal/recipe` — shared data model (Recipe/Op/Output/ProbeInfo); stdlib only, no ffmpeg
- `internal/graph` — op stack → ffmpeg filter_complex (pure, golden-tested; `alpha_ffmpeg_test.go` is an external `graph_test` package that pixel-checks the alpha chains against a real ffmpeg when one is on PATH, skipped otherwise)
- `internal/enc` — argv builders for ffmpeg/gifsicle/… (pure, golden-tested)
- `internal/ffrun` — the only package that spawns processes; ffmpeg progress parsing
- `internal/discordlint` — GIF/WebP byte-level lint + fix (stdlib only)
- `internal/probe` — ffprobe → ProbeInfo (+ alpha scan)
- `internal/store` — /data layout: blobs by sha256, results by recipe hash, scratch, sweeper
- `internal/jobs` — job table, SSE events, render pipeline
- `internal/server` — HTTP API + embedded SPA
- `web/` — Svelte 5 + Vite SPA; `web/dist` is embedded by `web/embed.go`

## Toolchain on this Windows host

- Go 1.26.6 is installed natively at `C:\Program Files\Go\bin` (machine PATH).
  If a shell was started before the install, refresh PATH first:
  `$env:Path = [Environment]::GetEnvironmentVariable('Path','Machine') + ';' + [Environment]::GetEnvironmentVariable('Path','User')`
  then plain `go vet ./...` / `go test ./...`. `.\scripts\go.ps1 …` still
  works as a Docker fallback (`golang:1.26-trixie`, repo mounted at /src).
- `ffmpeg`/`ffprobe` on the host are a 2026-08 FFmpeg git build (≥ 9.0.1
  features: animated WebP demuxer, libwebp_anim, prores) at
  `C:\Programs\bin\ffmpeg-master-latest-win64-gpl\bin`. ffmpeg 9.0.1 also
  exists in WSL (`wsl -e ffmpeg …`, host paths under `/mnt/c/...`).
- gifsicle / gifski / libwebp CLI / libavif / pngquant / oxipng are NOT on the
  host. The authoritative toolchain is the Docker runtime image — integration
  tests run there: `docker compose build && docker compose run --rm app …`.
- Node 22 / npm are on the host for `web/`.

## Rules

- Keep `internal/recipe`, `internal/graph`, `internal/enc`, `internal/discordlint` stdlib-only and process-free (except discordlint may use image/gif for pixel-index analysis).
- Do not change exported signatures in the stubs without updating every caller and the docs comment; prefer adding.
- Every ffmpeg/gifsicle argv change needs its golden test updated.
- Discord rules live in `docs/DESIGN.md` §5.3 and `internal/discordlint`; bump `discordlint.RulesVersion` when rules change.
- Results are memoised on disk under `jobs.ResultKey(recipe)` = hash(recipe, `jobs.PipelineVersion`, `discordlint.RulesVersion`). Bump `jobs.PipelineVersion` whenever graph/enc/jobs change what a recipe renders to, or users get stale cached outputs.
- Bump `store.InfoVersion` when probe semantics change (persisted ProbeInfo is re-probed on the next upload).
- Never use ffmpeg's legacy `libwebp` encoder for animation (use `libwebp_anim`); always pass `-loop 0` (webp) / `-plays 0` (apng).
