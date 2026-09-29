# Two images from one file (ANALYSIS.md, *The two images*):
#
#   --target web       the server and the frontend on Alpine: what the container
#                      app runs when analysis runs as a job. No Python, no
#                      models, tens of MB.
#   --target analyzer  the same, plus BirdNET's Python runtime and the models:
#                      what the analysis job's workers run (`birdsense worker`),
#                      and what serves everything in one container when
#                      analysis runs in the web app's own process. It is the
#                      last stage, so a plain `docker build` and compose get it.
#
# scripts/deploy.ps1 builds both at the same tag.
#
# `analyzer` starts from the BirdNET runtime -- Python, the venv, the models --
# which is its own image, built from analyzer/Dockerfile, because it is the
# slow part of the build and changes only when the requirements do. Name it
# with BIRDNET_IMAGE: deploy.ps1 passes the registry's copy, tagged by a hash
# of what it is built from; compose builds it as the `birdnet` service and
# supplies it under this default name. A plain `docker build` needs it first:
#
#   docker build -t birdnet analyzer && docker build .
ARG BIRDNET_IMAGE=birdnet

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


FROM ${BIRDNET_IMAGE} AS analyzer

# /app/data is where BIRDSENSE_DB=local keeps its JSON file, and /app/audio is
# where BIRDSENSE_STORAGE=local keeps card audio. Creating them here, owned by
# the app user, means a named volume mounted over either is writable too.
RUN useradd --uid 10001 --create-home birdsense \
 && mkdir -p /app/data /app/audio \
 && chown birdsense:birdsense /app/data /app/audio

# The venv and the models are in the base image, at /opt/birdnet, so pushing
# this one sends only the layers below.
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
