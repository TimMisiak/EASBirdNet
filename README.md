# EASBirdNet — Birdsense

Web app for Eastside Audubon's BirdNET listening stations: which birds were
heard, where, and when.

A single Go binary serves both the JSON API and the static frontend, so the
whole app runs as one self-contained container.

## Quick start

```sh
cd backend && BIRDSENSE_DB=local go run ./cmd/server     # http://localhost:8080
```

`BIRDSENSE_DB=local` keeps data in a JSON file; without it the server expects
Azure Cosmos DB (see [DEPLOYMENT.md](DEPLOYMENT.md)).

Or in Docker:

```sh
HOST_PORT=8080 docker compose up --build
```

See [CLAUDE.md](CLAUDE.md) for the architecture decisions, layout, and
conventions, and [SCHEMA.md](SCHEMA.md) for the data model.
