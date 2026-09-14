# Two stages, one image: build the Go binary, then ship it next to the frontend
# files so a single container serves both the API and the UI.

FROM golang:1.25-alpine AS build

WORKDIR /src/backend

# Copy the module files first so `go mod download` is cached until deps change.
COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/birdsense ./cmd/server


FROM alpine:3.22

RUN adduser -D -u 10001 birdsense

WORKDIR /app
COPY --from=build /out/birdsense /app/birdsense
# The frontend has no build step, so the source files are the shipped files.
COPY frontend/ /app/frontend/

USER birdsense
EXPOSE 8080

ENV BIRDSENSE_ADDR=":8080" \
    BIRDSENSE_STATIC_DIR="/app/frontend"

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/api/v1/health || exit 1

ENTRYPOINT ["/app/birdsense"]
