# syntax=docker/dockerfile:1.7
#
# ez-local-gif matte sidecar — one file, two targets (BuildKit required):
#
#   cpu    python:3.12-slim + onnxruntime 1.30.0 (CPU EP) + torch 2.14.1+cpu / sam2 (the Guided tracker): 1.2 GB installed
#          (`docker images` shows 2.4 GB — Docker Desktop's containerd store counts the compressed blobs as well)
#          docker build --target cpu  -t duckautomata/ezlg-matte:local      sidecar/
#   cuda   python:3.12-slim + onnxruntime-gpu[cuda,cudnn] 1.30.0 + torch 2.14.1+cu130 / torchvision / sam2 sharing ONE
#          set of NVIDIA wheels (the CUDA 13 runtime, cuBLAS, cuFFT, cuRAND, NVRTC, cuDNN 9, cuSPARSE, cuSPARSELt, CUPTI,
#          NCCL, NVSHMEM, cuFile, NVTX at torch's pins, ~4 GB of wheels; the pip install alone takes many minutes), no
#          system CUDA. triton and cuSOLVER are removed after the install (see PRUNE): 4.9 GB installed (`docker images`
#          10.7 GB), against 2.6 GB of /usr (+ a stray 1.5 GB /tmp; 7.5 GB in `docker images`) for the ORT-only image
#          before Phase 5c.
#          docker build --target cuda -t duckautomata/ezlg-matte:local-cuda sidecar/
#          Host side: an NVIDIA driver >= 580 and nvidia-container-toolkit (nothing extra under Docker Desktop's WSL2).
#
# Both targets: non-root uid 1000, /models as a volume (weights + derived graphs + the CUDA JIT cache; created and
# chowned BEFORE the VOLUME line so a fresh named volume inherits 1000:1000 — otherwise the first download fails with
# EACCES), the python healthcheck on /v1/ping (python:3.12-slim ships no curl; a 300 s start period — nothing listens
# until the CUDA session exists (context + first-time PTX JIT, cached in /models/.nv only after that run) and the
# weights are downloaded, so a cold first start of the cuda image shows "health: starting" for minutes, not
# "unhealthy"), and a build-time self-test that runs a synthetic 8×8 ONNX graph through the derivation tools,
# onnxruntime, numpy and the PNG writer, plus the tracker plumbing (torch, sam2, hydra, scipy hole filling, the frame
# loader) with random weights — the checkpoint is not baked in, it is downloaded into /models at start like the ONNX
# files. Dependencies are pinned with == and --require-hashes in requirements-{cpu,cuda}.txt (regenerate: see the .in
# files); sam2 itself comes from requirements-sam2.txt (a hashed GitHub archive of the pinned commit, built without
# its CUDA extension: SAM2_BUILD_CUDA=0). The wheels are installed before the application files are copied, so a code
# change never rebuilds the wheel layer.
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
COPY requirements-cpu.txt requirements-sam2.txt ./
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    pip install --require-hashes -r requirements-cpu.txt \
 && SAM2_BUILD_CUDA=0 pip install --require-hashes --no-deps --no-build-isolation -r requirements-sam2.txt \
 && rm -rf /usr/local/lib/python3.12/site-packages/torch/test /usr/local/lib/python3.12/site-packages/torch/include \
 && python -c "import torch, sam2, onnxruntime; print('torch', torch.__version__, 'onnxruntime', onnxruntime.__version__)" \
 && du -sh /usr/local/lib/python3.12/site-packages
COPY matte.py png.py models.json NOTICE ./
COPY tools/ ./tools/
COPY LICENSES/*.txt ./LICENSES/
# MATTE_MODELS defaults to what models.json offers on the CPU: isnet-anime and sam2-tiny (birefnet-lite only when named).
ENV MATTE_DEVICE=cpu
# Build-time self-test: derivation (fp16 + 16²), sessions, the batch path, the PNG codec — on a synthetic graph — and the
# tracker plumbing with random weights (no checkpoint in the image).
RUN MATTE_THREADS=2 python -I -B /app/matte.py selftest && rm -rf /tmp/ezlg-matte-* /tmp/.cache
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
COPY requirements-cuda.txt requirements-sam2.txt ./
# PRUNE: wheels torch pulls in that inference never uses and that nothing links at load time: torch.compile's triton
# (893 MB, imported lazily) and cuSOLVER (235 MB, dlopen'ed by torch.linalg only) — plus torch's own test binaries and
# C++ headers (only extension builds need them; sam2 is built before this line and builds none). NCCL, cuSPARSELt,
# cuFile and NVSHMEM stay: libtorch_cuda.so / libtorch_python.so / libtorch_nvshmem.so carry them as DT_NEEDED on
# torch 2.14.1+cu130 (readelf -d), so `import torch` fails without them. The import check afterwards proves it.
ARG PRUNE="triton nvidia-cusolver"
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    pip install --require-hashes -r requirements-cuda.txt \
 && SAM2_BUILD_CUDA=0 pip install --require-hashes --no-deps --no-build-isolation -r requirements-sam2.txt \
 && pip uninstall -y ${PRUNE} \
 && rm -rf /usr/local/lib/python3.12/site-packages/torch/test /usr/local/lib/python3.12/site-packages/torch/include \
 && python -c "import torch, sam2, onnxruntime; print('torch', torch.__version__, 'onnxruntime', onnxruntime.__version__, onnxruntime.get_available_providers())" \
 && du -sh /usr/local/lib/python3.12/site-packages
COPY matte.py png.py models.json NOTICE ./
COPY tools/ ./tools/
COPY LICENSES/*.txt ./LICENSES/
COPY LICENSES/cuda/ ./LICENSES/cuda/
# MATTE_MODELS defaults to what models.json offers: isnet-anime, birefnet-lite and sam2-tiny on cuda; isnet-anime and
# sam2-tiny on the cpu the same process also serves (device=cpu).
ENV MATTE_DEVICE=cuda \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility \
    CUDA_CACHE_PATH=/models/.nv
# No GPU at build time: the self-test runs the same wheels' CPU execution provider and the tracker plumbing on the CPU.
RUN MATTE_DEVICE=cpu MATTE_THREADS=2 python -I -B /app/matte.py selftest && rm -rf /tmp/ezlg-matte-* /tmp/.cache
USER matte
VOLUME ["/models"]
EXPOSE 9402
HEALTHCHECK --interval=30s --timeout=5s --start-period=300s --retries=3 \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:9402/v1/ping',timeout=3).status==200 else 1)"]
ENTRYPOINT ["python", "-I", "-B", "/app/matte.py"]
CMD ["serve"]
