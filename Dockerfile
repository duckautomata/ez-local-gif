# syntax=docker/dockerfile:1.7
#
# ez-local-gif — multi-stage build (BuildKit required).
#
#   web      node:22-alpine       builds the Svelte SPA          → /app/web/dist
#   gobuild  golang:1.26-trixie   builds the static Go binary    → /out/ezlg (SPA embedded)
#   tools    debian:trixie-slim   FFmpeg 9.0.1 + gifsicle + libwebp + libavif + pngquant
#                                 + oxipng + gifski + fonts + tini, all version-checked
#   runtime  tools + ezlg         what `docker compose up` runs (non-root, tini, healthcheck)
#   dev      tools + Go + Node    bind-mount the repo and run `go run` / `vite dev` inside
#
# Every third-party download is pinned by URL + sha256 (see the ARGs below).
# To move the FFmpeg pin: scripts/pin-ffmpeg.sh <autobuild-tag> prints new values.
#
#   docker build --target tools   -t ezlg-tools .
#   docker build --target dev     -t ezlg-dev .
#   docker build --target runtime -t ezlg:local --build-arg VERSION=$(git describe --tags --always) .

# ---------------------------------------------------------------------------
# Pinned third-party artefacts (amd64 / linux64).
# ---------------------------------------------------------------------------
# FFmpeg 9.0.1 — BtbN static gpl build. Dated autobuild tag (never the rolling
# "latest" asset). BtbN keeps daily tags for ~2 weeks and month-end tags
# permanently; refresh with scripts/pin-ffmpeg.sh when the tag disappears.
ARG FFMPEG_TAG=autobuild-2026-08-18-15-03
ARG FFMPEG_ASSET=ffmpeg-n9.0.1-6-g9d4ca21220-linux64-gpl-9.0.tar.xz
ARG FFMPEG_SHA256=c99d981946abf7f733590d0c2e8ee39316f2cf97927ef7aa2ea4edd456ac2d39
# libwebp 1.5.0 command-line tools, Google's official static Linux build
# (cwebp/dwebp/img2webp/gif2webp/webpmux/webpinfo/anim_dump/anim_diff).
# Debian's "webp" package would work too but drags in Mesa + LLVM (~200 MB)
# through vwebp's OpenGL dependency, so we take the static tools instead.
ARG LIBWEBP_URL=https://storage.googleapis.com/downloads.webmproject.org/releases/webp/libwebp-1.5.0-linux-x86-64.tar.gz
ARG LIBWEBP_SHA256=f4bf49f85991f50e86a5404d16f15b72a053bb66768ed5cc0f6d042277cc2bb8
# gifski 1.34.0 (linux/gifski inside the release tarball).
ARG GIFSKI_URL=https://github.com/ImageOptim/gifski/releases/download/1.34.0/gifski-1.34.0.tar.xz
ARG GIFSKI_SHA256=b9b6591aa163123d737353d9c8581efdf3234d28eeaa45329b31da905cd5a996
# oxipng 10.2.0 (static musl build).
ARG OXIPNG_URL=https://github.com/oxipng/oxipng/releases/download/v10.2.0/oxipng-10.2.0-x86_64-unknown-linux-musl.tar.gz
ARG OXIPNG_SHA256=a27ecb29faab9da1549f4a243bab12f1e43e4ed8ae6a2a2186a543dc9ffd3956
# Go 1.26.6 and Node 22.23.2 (dev stage only).
ARG GO_URL=https://go.dev/dl/go1.26.6.linux-amd64.tar.gz
ARG GO_SHA256=708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89
ARG NODE_URL=https://nodejs.org/dist/v22.23.2/node-v22.23.2-linux-x64.tar.xz
ARG NODE_SHA256=d60acfe00a2932254bb0ad20e01b0d74397a0875595de719654b214f4b03f307

# ---------------------------------------------------------------------------
# web: build the Svelte SPA
# ---------------------------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /app/web
COPY web/package*.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY web/ ./
RUN npm run build

# ---------------------------------------------------------------------------
# gobuild: static Go binary with the SPA embedded
# ---------------------------------------------------------------------------
FROM golang:1.26-trixie AS gobuild
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /app/web/dist ./web/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-buildvcs=false \
    go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /out/ezlg ./cmd/ezlg

# ---------------------------------------------------------------------------
# tools: the media toolchain (shared by runtime and dev)
# ---------------------------------------------------------------------------
FROM debian:trixie-slim AS tools
ARG FFMPEG_TAG
ARG FFMPEG_ASSET
ARG FFMPEG_SHA256
ARG LIBWEBP_URL
ARG LIBWEBP_SHA256
ARG GIFSKI_URL
ARG GIFSKI_SHA256
ARG OXIPNG_URL
ARG OXIPNG_SHA256

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8

# Distro tools. Package names verified against Debian 13 (trixie):
#   gifsicle 1.96, libavif-bin 1.2.1 (avifenc/avifdec; linked against aom,
#   dav1d, rav1e, svt-av1), pngquant 2.18, apngdis 2.9, fonts-dejavu-core
#   (drawtext), fontconfig, ca-certificates + curl (downloads, healthcheck),
#   xz-utils (tar.xz), tini (PID 1). libwebp tools come from Google's static
#   build below (see LIBWEBP_URL).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      gifsicle libavif-bin pngquant apngdis \
      fonts-dejavu-core fontconfig ca-certificates curl xz-utils tini \
 && rm -rf /var/lib/apt/lists/* \
 && fc-cache -f

# FFmpeg 9.0.1 (ffmpeg + ffprobe only; ffplay is dropped).
RUN set -eu; cd /tmp; \
    url="https://github.com/BtbN/FFmpeg-Builds/releases/download/${FFMPEG_TAG}/${FFMPEG_ASSET}"; \
    echo "downloading ${url}"; \
    curl -fsSL --retry 5 --retry-delay 3 -o ffmpeg.tar.xz "${url}"; \
    echo "${FFMPEG_SHA256}  ffmpeg.tar.xz" | sha256sum -c -; \
    tar -xJf ffmpeg.tar.xz --strip-components=2 -C /usr/local/bin \
        --wildcards '*/bin/ffmpeg' '*/bin/ffprobe'; \
    chmod 755 /usr/local/bin/ffmpeg /usr/local/bin/ffprobe; \
    rm -f ffmpeg.tar.xz

# libwebp tools (static; vwebp/get_disto/webp_quality are not installed).
RUN set -eu; cd /tmp; \
    curl -fsSL --retry 5 --retry-delay 3 -o libwebp.tar.gz "${LIBWEBP_URL}"; \
    echo "${LIBWEBP_SHA256}  libwebp.tar.gz" | sha256sum -c -; \
    tar -xzf libwebp.tar.gz --strip-components=2 -C /usr/local/bin --wildcards \
        '*/bin/cwebp' '*/bin/dwebp' '*/bin/img2webp' '*/bin/gif2webp' \
        '*/bin/webpmux' '*/bin/webpinfo' '*/bin/anim_dump' '*/bin/anim_diff'; \
    chmod 755 /usr/local/bin/cwebp /usr/local/bin/dwebp /usr/local/bin/img2webp /usr/local/bin/gif2webp \
              /usr/local/bin/webpmux /usr/local/bin/webpinfo /usr/local/bin/anim_dump /usr/local/bin/anim_diff; \
    rm -f libwebp.tar.gz

# gifski + oxipng (static binaries).
RUN set -eu; cd /tmp; \
    curl -fsSL --retry 5 --retry-delay 3 -o gifski.tar.xz "${GIFSKI_URL}"; \
    echo "${GIFSKI_SHA256}  gifski.tar.xz" | sha256sum -c -; \
    tar -xJf gifski.tar.xz --warning=no-unknown-keyword --strip-components=1 -C /usr/local/bin linux/gifski; \
    curl -fsSL --retry 5 --retry-delay 3 -o oxipng.tar.gz "${OXIPNG_URL}"; \
    echo "${OXIPNG_SHA256}  oxipng.tar.gz" | sha256sum -c -; \
    tar -xzf oxipng.tar.gz --strip-components=1 -C /usr/local/bin --wildcards '*/oxipng'; \
    chmod 755 /usr/local/bin/gifski /usr/local/bin/oxipng; \
    rm -f gifski.tar.xz oxipng.tar.gz

# Helper scripts (Phase 1 testkit + tool self-check) and the build-time
# sanity check: every tool must run, and ffmpeg must have the encoders,
# filters and demuxers the pipeline relies on — a broken download fails here.
COPY scripts/check-tools.sh scripts/make-test-clip.sh scripts/discord-testkit.sh /usr/local/share/ezlg/
# (sed: survive a CRLF checkout on Windows hosts with core.autocrlf=true)
RUN sed -i 's/\r$//' /usr/local/share/ezlg/*.sh \
 && chmod 755 /usr/local/share/ezlg/*.sh \
 && /usr/local/share/ezlg/check-tools.sh

# ---------------------------------------------------------------------------
# runtime: what compose runs
# ---------------------------------------------------------------------------
FROM tools AS runtime
COPY --from=gobuild /out/ezlg /usr/local/bin/ezlg
RUN useradd --uid 1000 --user-group --create-home --shell /usr/sbin/nologin ezlg \
 && mkdir -p /data /input /output \
 && chown -R ezlg:ezlg /data /input /output
VOLUME ["/data"]
EXPOSE 8080
ENV EZLG_DATA=/data \
    EZLG_SCRATCH=/dev/shm/ezl
USER ezlg
WORKDIR /data
ENTRYPOINT ["tini", "--", "ezlg"]
CMD ["serve"]
# Uses the default EZLG_ADDR (:8080); adjust the URL if you change it.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD curl -fs http://localhost:8080/healthz || exit 1

# ---------------------------------------------------------------------------
# dev: tools + Go + Node, root, bind-mount the repo at /src
# ---------------------------------------------------------------------------
FROM tools AS dev
ARG GO_URL
ARG GO_SHA256
ARG NODE_URL
ARG NODE_SHA256
RUN apt-get update \
 && apt-get install -y --no-install-recommends git jq procps \
 && rm -rf /var/lib/apt/lists/*
RUN set -eu; cd /tmp; \
    curl -fsSL --retry 5 --retry-delay 3 -o go.tar.gz "${GO_URL}"; \
    echo "${GO_SHA256}  go.tar.gz" | sha256sum -c -; \
    tar -C /usr/local -xzf go.tar.gz; \
    rm -f go.tar.gz; \
    curl -fsSL --retry 5 --retry-delay 3 -o node.tar.xz "${NODE_URL}"; \
    echo "${NODE_SHA256}  node.tar.xz" | sha256sum -c -; \
    tar -C /usr/local --strip-components=1 -xJf node.tar.xz --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE'; \
    rm -f node.tar.xz; \
    /usr/local/go/bin/go version; node --version; npm --version
ENV PATH=/usr/local/go/bin:/go/bin:$PATH \
    GOPATH=/go \
    GOMODCACHE=/go/pkg/mod \
    GOCACHE=/root/.cache/go-build \
    GOFLAGS=-buildvcs=false \
    CGO_ENABLED=0 \
    EZLG_DATA=/data \
    EZLG_SCRATCH=/dev/shm/ezl
RUN mkdir -p /go/pkg/mod /root/.cache/go-build /data /src
WORKDIR /src
EXPOSE 8080 5173
CMD ["bash"]
