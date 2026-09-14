# EASBirdNet — Birdsense

Web app for Eastside Audubon's BirdNET listening stations: which birds were
heard, where, and when.

A single Go binary serves both the JSON API and the static frontend, so the
whole app runs as one self-contained container.

## Quick start

```sh
cd backend && go run ./cmd/server     # http://localhost:8080
```

Or in Docker:

```sh
HOST_PORT=8080 docker compose up --build
```

See [CLAUDE.md](CLAUDE.md) for the architecture decisions, layout, and
conventions.
