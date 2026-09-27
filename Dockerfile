# Three stages, one image: build the Go binaries, install BirdNET's Python
# runtime and models, then ship both next to the frontend files so a single
# container serves the API and the UI and can analyze audio.

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


# BirdNET runs from the `birdnet` Python package on LiteRT (no TensorFlow).
# Its wheels are built for glibc, which is why the runtime is Debian, not
# Alpine. Keep this Python version in step with analyzer/requirements.txt.
FROM python:3.12-slim-bookworm AS birdnet

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    BIRDNET_APP_DATA=/opt/birdnet/models

RUN python -m venv /opt/birdnet/venv
COPY analyzer/requirements.txt /opt/birdnet/requirements.txt
RUN /opt/birdnet/venv/bin/pip install -r /opt/birdnet/requirements.txt

# Download the models into the image (acoustic, plus geo for location
# filtering) so analysis never reaches the network. This layer depends only on
# requirements.txt, so editing analyze.py doesn't fetch them again. The loads
# must match the ones in analyze.py.
RUN /opt/birdnet/venv/bin/python -c 'import birdnet; \
birdnet.load("acoustic", "2.4", "tf", library="litert"); \
birdnet.load("geo", "2.4", "tf", library="litert")'


FROM python:3.12-slim-bookworm

# /app/data is where BIRDSENSE_DB=local keeps its JSON file, and /app/audio is
# where BIRDSENSE_STORAGE=local keeps card audio. Creating them here, owned by
# the app user, means a named volume mounted over either is writable too.
RUN useradd --uid 10001 --create-home birdsense \
 && mkdir -p /app/data /app/audio \
 && chown birdsense:birdsense /app/data /app/audio

# The venv's python links to this base image's /usr/local/bin/python3.12, which
# is why both stages use the same image.
COPY --from=birdnet /opt/birdnet/venv /opt/birdnet/venv
COPY --from=birdnet /opt/birdnet/models /opt/birdnet/models

WORKDIR /app
COPY --from=build /out/birdsense /out/birdsense-analyze /app/
COPY analyzer/analyze.py analyzer/clip.py /app/analyzer/
# The frontend has no build step, so the source files are the shipped files.
COPY frontend/ /app/frontend/

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
