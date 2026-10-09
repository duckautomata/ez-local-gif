# syntax=docker/dockerfile:1.7
#
# ez-local-gif matte sidecar — one file, two targets (BuildKit required):
#
#   cpu    python:3.12-slim + onnxruntime 1.30.0 (CPU EP), ~300 MB
#          docker build --target cpu  -t duckautomata/ezlg-matte:local      sidecar/
#   cuda   python:3.12-slim + onnxruntime-gpu[cuda,cudnn] 1.30.0: the CUDA 13 runtime, cuBLAS, cuFFT, cuRAND, NVRTC
#          and cuDNN 9 wheels (~2.3 GB of wheels; the pip install alone takes many minutes), no system CUDA.
#          docker build --target cuda -t duckautomata/ezlg-matte:local-cuda sidecar/
#          Host side: an NVIDIA driver >= 580 and nvidia-container-toolkit (nothing extra under Docker Desktop's WSL2).
#
# Both targets: non-root uid 1000, /models as a volume (weights + derived graphs + the CUDA JIT cache; created and
# chowned BEFORE the VOLUME line so a fresh named volume inherits 1000:1000 — otherwise the first download fails with
# EACCES), the python healthcheck on /v1/ping (python:3.12-slim ships no curl; a 300 s start period — nothing listens
# until the CUDA session exists (context + first-time PTX JIT, cached in /models/.nv only after that run) and the
# preloaded weights are downloaded, so a cold first start of the cuda image shows "health: starting" for minutes, not
# "unhealthy"), and a build-time self-test that runs a
# synthetic 8×8 ONNX graph through the derivation tools, onnxruntime, numpy and the PNG writer. Dependencies are pinned
# with == and --require-hashes in requirements-{cpu,cuda}.txt (regenerate: see the .in files). The wheels are installed
# before the application files are copied, so a code change never rebuilds the wheel layer.
#
# Runtime: read_only root filesystem with tmpfs:/tmp and HOME=/tmp (compose.yaml sets them; nothing is written
# outside /models and /tmp). ENTRYPOINT is `python -I matte.py`, so `docker compose run --rm matte download` runs the
# download subcommand (pre-seed the volume for an air-gapped host) and `… selftest` the self-test.
ARG PYTHON_IMAGE=python:3.12-slim

# ---------------------------------------------------------------------------
# base: interpreter, user, /models (no packages, no application files)
# ---------------------------------------------------------------------------
FROM ${PYTHON_IMAGE} AS base
ARG VERSION=dev
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    MATTE_VERSION=${VERSION} \
    MATTE_PORT=9402 \
    MATTE_MODELS_DIR=/models \
    HOME=/tmp
RUN useradd --uid 1000 --user-group --create-home --shell /usr/sbin/nologin matte \
 && mkdir -p /models \
 && chown 1000:1000 /models
WORKDIR /app

# ---------------------------------------------------------------------------
# cpu
# ---------------------------------------------------------------------------
FROM base AS cpu
COPY requirements-cpu.txt ./
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    pip install --require-hashes -r requirements-cpu.txt
COPY matte.py png.py models.json NOTICE ./
COPY tools/ ./tools/
COPY LICENSES/*.txt ./LICENSES/
ENV MATTE_DEVICE=cpu \
    MATTE_MODELS=isnet-anime \
    MATTE_DEFAULT_MODEL=isnet-anime
# Build-time self-test: derivation (fp16 + 16²), sessions, the batch path, the PNG codec — on a synthetic graph.
RUN python -I -B /app/matte.py selftest && rm -rf /tmp/ezlg-matte-*
USER matte
VOLUME ["/models"]
EXPOSE 9402
HEALTHCHECK --interval=30s --timeout=5s --start-period=300s --retries=3 \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:9402/v1/ping',timeout=3).status==200 else 1)"]
ENTRYPOINT ["python", "-I", "-B", "/app/matte.py"]
CMD ["serve"]

# ---------------------------------------------------------------------------
# cuda
# ---------------------------------------------------------------------------
FROM base AS cuda
COPY requirements-cuda.txt ./
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    pip install --require-hashes -r requirements-cuda.txt
COPY matte.py png.py models.json NOTICE ./
COPY tools/ ./tools/
COPY LICENSES/*.txt ./LICENSES/
COPY LICENSES/cuda/ ./LICENSES/cuda/
ENV MATTE_DEVICE=cuda \
    MATTE_MODELS=isnet-anime,birefnet-lite \
    MATTE_DEFAULT_MODEL=isnet-anime \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility \
    CUDA_CACHE_PATH=/models/.nv
# No GPU at build time: the self-test runs the same wheel's CPU execution provider.
RUN MATTE_DEVICE=cpu python -I -B /app/matte.py selftest && rm -rf /tmp/ezlg-matte-*
USER matte
VOLUME ["/models"]
EXPOSE 9402
HEALTHCHECK --interval=30s --timeout=5s --start-period=300s --retries=3 \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:9402/v1/ping',timeout=3).status==200 else 1)"]
ENTRYPOINT ["python", "-I", "-B", "/app/matte.py"]
CMD ["serve"]
