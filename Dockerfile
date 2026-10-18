# Three targets from one file (ANALYSIS.md, *The two images*):
#
#   --target web       the server and the frontend on Alpine: what the container
#                      app runs when analysis runs as a job. No Python, no
#                      models, tens of MB.
#   --target birdnet   the BirdNET runtime alone -- Python, the venv, the models,
#                      nothing of Birdsense's own. The slow part of the build,
#                      and it changes only when the requirements do.
#   --target analyzer  the server and the frontend on the BirdNET runtime: what
#                      the analysis job's workers run (`birdsense worker`), and
#                      what serves everything in one container when analysis
#                      runs in the web app's own process. It is the last stage,
#                      so a plain `docker build` and compose get it, in one
#                      build and one container.
#
# scripts/deploy.ps1 builds `birdnet` on its own, into the registry, tagged by
# a hash of its stage and the requirements, and only when that tag isn't there
# yet; then it builds `web` and `analyzer` at the commit, with BIRDNET_BASE
# naming the registry's runtime. `az acr build` is Docker's legacy builder,
# which builds every stage before the target whether the target uses it or
# not -- so the `birdnet` stage has to cost nothing when its base already holds
# the runtime, which is what the `ready` marker is for.
ARG BIRDNET_BASE=python:3.12-slim-bookworm

# At least the `toolchain` line in backend/go.mod, or the build downloads its
# own copy; bump the two together (CLAUDE.md, *Dependencies are scanned*).
FROM golang:1.26-alpine AS build

WORKDIR /src/backend

# Copy the module files first so `go mod download` is cached until deps change.
COPY backend/go.mod backend/go.sum* ./
RUN go mod download

COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/birdsense ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/birdsense-analyze ./cmd/analyze


FROM alpine:3.22 AS web

# The server is a static binary; it needs CA roots for Azure, and nothing
# else. Time zones are compiled into it.
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 birdsense \
 && mkdir -p /app/data /app/audio \
 && chown birdsense:birdsense /app/data /app/audio

WORKDIR /app
COPY --from=build /out/birdsense /app/
COPY frontend/ /app/frontend/
COPY THIRD_PARTY_NOTICES.md /app/
COPY LICENSES/ /app/LICENSES/

USER birdsense
EXPOSE 8080

ENV BIRDSENSE_ADDR=":8080" \
    BIRDSENSE_STATIC_DIR="/app/frontend" \
    BIRDSENSE_STORAGE_DIR="/app/audio"

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/api/v1/health"]

ENTRYPOINT ["/app/birdsense"]


# The BirdNET runtime. BirdNET runs from the `birdnet` Python package on LiteRT
# (no TensorFlow). Its wheels are built for glibc, which is why the runtime is
# Debian, not Alpine. Keep this Python version in step with requirements.txt.
# Perch, the optional second model (BIRDSENSE_PERCH), only runs on TensorFlow,
# which requirements-perch.txt adds. It is always installed, so turning Perch
# on is a setting rather than a different image -- at ~1.3 GB of TensorFlow
# and ~400 MB of model on top of everything else (DEPLOYMENT.md).
#
# From python, every step runs. From a runtime deploy.ps1 built earlier (the
# BIRDNET_BASE it passes), /opt/birdnet/ready is already there and every step
# is a no-op -- so editing this stage changes deploy.ps1's tag for it, and the
# next deploy builds a fresh runtime rather than skipping over a stale one.
FROM ${BIRDNET_BASE} AS birdnet

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    BIRDNET_APP_DATA=/opt/birdnet/models

COPY analyzer/requirements.txt /opt/birdnet/requirements.txt
RUN test -e /opt/birdnet/ready \
 || { python -m venv /opt/birdnet/venv \
   && /opt/birdnet/venv/bin/pip install -r /opt/birdnet/requirements.txt; }
COPY analyzer/requirements-perch.txt /opt/birdnet/requirements-perch.txt
RUN test -e /opt/birdnet/ready \
 || /opt/birdnet/venv/bin/pip install -r /opt/birdnet/requirements.txt -r /opt/birdnet/requirements-perch.txt

# Download the models into the image (acoustic, plus geo for location
# filtering, plus Perch) so analysis never reaches the network. The loads must
# match the ones in analyze.py.
RUN test -e /opt/birdnet/ready \
 || { /opt/birdnet/venv/bin/python -c 'import birdnet; \
birdnet.load("acoustic", "2.4", "tf", library="litert"); \
birdnet.load("geo", "2.4", "tf", library="litert"); \
birdnet.load_perch_v2()' \
   && touch /opt/birdnet/ready; }


FROM birdnet AS analyzer

# /app/data is where BIRDSENSE_DB=local keeps its JSON file, and /app/audio is
# where BIRDSENSE_STORAGE=local keeps card audio. Creating them here, owned by
# the app user, means a named volume mounted over either is writable too.
RUN useradd --uid 10001 --create-home birdsense \
 && mkdir -p /app/data /app/audio \
 && chown birdsense:birdsense /app/data /app/audio

# The venv and the models are in the stage above, at /opt/birdnet, so when it
# came from the registry, pushing this image sends only the layers below.
WORKDIR /app
COPY --from=build /out/birdsense /out/birdsense-analyze /app/
COPY analyzer/analyze.py analyzer/clip.py /app/analyzer/
# The frontend has no build step, so the source files are the shipped files.
COPY frontend/ /app/frontend/
# The image carries the BirdNET and Perch models, so it carries their licences too.
COPY THIRD_PARTY_NOTICES.md /app/
COPY LICENSES/ /app/LICENSES/

USER birdsense
EXPOSE 8080

ENV BIRDSENSE_ADDR=":8080" \
    BIRDSENSE_STATIC_DIR="/app/frontend" \
    BIRDSENSE_STORAGE_DIR="/app/audio" \
    BIRDSENSE_BIRDNET_PYTHON="/opt/birdnet/venv/bin/python" \
    BIRDSENSE_BIRDNET_SCRIPT="/app/analyzer/analyze.py" \
    BIRDNET_APP_DATA="/opt/birdnet/models"

# The Debian base has no wget or curl, but it does have Python.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD ["python3", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/api/v1/health', timeout=2)"]

ENTRYPOINT ["/app/birdsense"]
