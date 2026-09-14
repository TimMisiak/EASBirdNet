# Birdsense — architecture notes

Birdsense is the Eastside Audubon web app for bird-call detections (BirdNET)
from local listening stations. One Go binary serves the JSON API *and* the
static frontend, so the whole app is one container with no reverse proxy, no
Node runtime, and no separate deploy for the UI.

What it stores is documented in [SCHEMA.md](SCHEMA.md); the Azure resources it
runs on (and the source for Terraform) are in [DEPLOYMENT.md](DEPLOYMENT.md).

```
/
├── backend/            Go module: API + static file server
│   ├── cmd/server/     main(): config, routing, graceful shutdown
│   └── internal/
│       ├── api/        JSON handlers under /api/v1/
│       ├── db/         Data model + Store: Cosmos DB (prod) or a JSON file (dev)
│       └── web/        serves frontend/ (cache headers, SPA fallback)
├── frontend/           Shipped as-is; no build step, no bundler
│   ├── index.html      Loads /js/main.js as a module; body is just <bs-app>
│   ├── styles/app.css  Design tokens (--bs-*) + document styles
│   └── js/
│       ├── main.js         Imports every component so they self-register
│       ├── api.js          fetch wrapper for /api/v1
│       ├── router.js       History router; in-app links, guards live in <bs-app>
│       ├── session.js       Who is signed in, shared by every component
│       ├── shared-styles.js Constructable stylesheets for repeated primitives
│       ├── format.js        Dates, sizes, counts, durations
│       ├── upload-flow.js   The SD-card upload, which spans four routes
│       ├── card-scan.js     Reads a card folder into a night-by-night manifest
│       ├── upload-status.js Card status -> chip colour and wording
│       └── components/      One custom element per file, plus base-element.js
├── SCHEMA.md           Stored documents: containers, fields, queries
├── DEPLOYMENT.md       Azure resources and settings (Terraform source)
├── Dockerfile          Multi-stage: build Go, ship binary + frontend/
└── docker-compose.yml
```

## Decisions

**Vanilla JS + web components, no framework.** Components are native custom
elements with shadow DOM. The app is small, its lifetime is long, and browser
APIs don't need migrating every two years. Practical consequence: there is no
virtual DOM, so a component re-renders by rewriting its own shadow root.

**No build step.** `frontend/` is served byte-for-byte as written, using native
ES modules (`<script type="module">`). No bundler, no transpiler, no
`node_modules`, no `npm install` before you can see a change. This is the
constraint that pays for itself in the Dockerfile and in onboarding — it holds
until the frontend actually needs a third-party package. (This is a frontend
rule only; Go dependencies are a separate call, see *Go dependencies* below.)
*Revisit when:* we need an npm dependency, or asset fingerprinting for
long-lived caching. Then add one build stage to the Dockerfile and bump the
`max-age` in `internal/web`; don't reach for a framework at the same time.
The one thing not served from this repo is the webfonts (Newsreader, IBM Plex
Sans/Mono), linked from Google Fonts in `index.html`. Every rule names a real
fallback, so a container with no outbound network renders in system fonts rather
than breaking. Self-host them if that trade stops being worth it.

**Go backend, static content included.** `internal/web` mounts the frontend at
`/` as the catch-all; `internal/api` claims `/api/v1/`. Unmatched paths under
`/api/` return a JSON 404 rather than index.html, so a mistyped API path looks
like an API error instead of silently returning HTML.

Gotcha: the static handler registers `"/"`, not `"GET /"`. A method-restricted
root pattern conflicts with `"/api/"` and `http.ServeMux` *panics at startup* —
which no package-level test catches, so `cmd/server/main_test.go` wires the real
mux and asserts the routes coexist.

**Static files from disk, not `go:embed`.** The static directory is a runtime
setting (`BIRDSENSE_STATIC_DIR`), so editing a file in `frontend/` and hitting
reload shows the change without recompiling Go. The container gets
self-containment from `COPY frontend/`, not from embedding.
*Revisit when:* we want a single distributable binary outside Docker.

**The API is the same shape the real one will be.** Routes, methods and JSON
shapes are settled; only what is behind them is temporary (see *State of the
code*). Public data is separated from everything else at the route level, so the
landing page never needs a session:

```
GET    /api/v1/health
GET    /api/v1/public/overview?days=      program stats + confirmed species
GET    POST DELETE /api/v1/session        who you are; sign in; sign out
GET    /api/v1/stations                   recorders in the field
GET    POST /api/v1/uploads               your cards; register a card
GET    /api/v1/uploads/{reference}
POST   /api/v1/uploads/{reference}/progress
GET    /api/v1/admin/uploads              every card, admin only
GET    POST /api/v1/admin/people          the roster
POST   /api/v1/admin/stations
```

A card belongs to a volunteer: `/uploads/{ref}` 404s for anyone else, and the
`/admin/*` routes 403 for a volunteer. Card counts only ever move forward in
`RecordProgress`, so a retried batch is harmless and a client can't walk a
card's progress backwards.

**Routes are paths, not hashes.** `/`, `/signin`, `/app`, `/app/upload/check`,
`/admin/people`. `internal/web` already falls back to index.html for
extension-less paths, so a reload mid-wizard lands on the same screen, and a
coordinator can send a colleague a link to one admin tab. `js/router.js` is the
whole router; `<bs-app>` holds the route table and the two guards (signed in,
and admin for `/admin/*`). Guards are convenience only -- the API enforces the
same rules, so guessing a path gets you a 401 or 403, not data.

**Shared stylesheets live in a cascade layer.** `shared-styles.js` exports
`CSSStyleSheet` objects for the primitives that appear on nearly every screen
(buttons, tables, form fields, panels); a component adopts what it needs via
`static styles`. Gotcha: `adoptedStyleSheets` are ordered *after* a shadow
root's own `<style>`, so an unlayered shared rule silently beats the component
that adopted it. Every shared sheet is therefore wrapped in
`@layer bs-base { ... }`, because unlayered rules outrank every layer -- what a
component writes for itself always wins.

**Go dependencies are fine; the stdlib already covers HTTP.** The "no
dependencies" rule is about the *frontend* (npm packages, see *No build step*).
It does not apply to the Go module. Add a Go dependency when it does a real job
the standard library doesn't: a database driver, migrations, password hashing
(`golang.org/x/crypto`), BirdNET/audio parsing, and so on. A Go dependency costs
one `go get` plus the cached `go mod download` layer already in the Dockerfile.
Routing and logging are already covered: `net/http` with Go 1.22+
method-and-path patterns (`"GET /api/v1/health"`) handles routes, and
`log/slog` handles logs. So we don't add a router, web framework, or logging
library just out of habit.

**One Store, two backends.** `internal/db` defines a `Store` interface with an
entity-specific method per read or write the API needs (`ListUploads`,
`UpdateDetection`, ...) and two implementations: Cosmos DB for NoSQL in Azure,
and a single JSON file for local development. `BIRDSENSE_DB` picks one, and
defaults to `cosmos` so a misconfigured deployment fails at startup instead of
quietly writing to a file. Named methods rather than a query builder, because
both backends must give identical answers, and filtering and sorting live once
in `db.go`. Gotcha: the Go Cosmos SDK only runs cross-partition queries the
gateway can serve, so queries are `SELECT * ... WHERE` and anything like
`ORDER BY`, `COUNT` or `DISTINCT` happens in Go (details in SCHEMA.md). Updates
take a mutate func (read, change, replace-if-unchanged), which is where rules
like "progress only moves forward" become race-free.
*Revisit when:* the JSON file gets slow (it rewrites everything on each write) —
that means dev data has outgrown it, not that it needs indexing.

**Design tokens in CSS custom properties.** Custom properties pierce shadow DOM
boundaries, so `styles/app.css` defines `--bs-*` tokens and every component
styles itself with `var(--bs-*)`. That is the *only* styling channel across the
shadow boundary — components never hard-code colors or spacing, and page CSS
never reaches into a component.

## Conventions

- Custom elements are prefixed `bs-`, one per file, filename matching the tag
  (`bs-detection-card.js`). The class name is the CamelCase form; the tag is
  what everything else refers to.
- Components extend `BaseElement` (`js/components/base-element.js`): open shadow
  root, `render()` builds the whole subtree, observed attribute changes
  re-render. Extend `HTMLElement` directly if a component needs finer-grained
  updates — the base class is a convenience, not a framework.
- Rich data is passed as a **property** (`card.detection = {...}`); attributes
  are for simple strings/flags.
- Anything interpolated into an `innerHTML` template — API data, attribute
  values — goes through `escapeHTML()`.
- API responses are JSON objects, never bare arrays (`{"detections": [...]}`),
  so a response can grow fields without breaking clients.
- Go: handlers stay in `internal/api`, `internal/*` packages do not import
  `cmd/`. Tests sit beside the code (`api_test.go`).
- A stored field changes in `internal/db/models.go` and SCHEMA.md together;
  a container or partition key change also updates DEPLOYMENT.md.

## Running it

Dev (two concerns, one process — Go serves the frontend from disk):

```sh
cd backend && BIRDSENSE_DB=local go run ./cmd/server     # http://localhost:8080
```

Frontend edits need only a browser reload; Go edits need a restart.
`BIRDSENSE_DB=local` stores data in `backend/data/birdsense.json` (git-ignored;
delete it to start empty, override with `BIRDSENSE_LOCAL_DB_PATH`). Without it
the server expects Cosmos DB (`BIRDSENSE_COSMOS_ENDPOINT`,
`BIRDSENSE_COSMOS_DATABASE`) and exits if it isn't configured.

Container (compose sets `BIRDSENSE_DB=local` and keeps the file in a named
volume):

```sh
HOST_PORT=8080 docker compose up --build
```

Checks before committing:

```sh
cd backend && gofmt -l . && go vet ./... && go test ./...
```

## State of the code

The API is wired end to end, but every handler still reads the in-memory
placeholder data in `internal/api/store.go`. The persistence layer
(`internal/db`) and the data model (SCHEMA.md) exist, and the server opens the
configured database at startup, but no handler reads or writes it yet.
Handlers move onto `db.Store` one at a time as each API is fleshed out;
`store.go` is deleted when the last one has. SCHEMA.md's *API mapping* section
lists how today's JSON field names map onto the stored documents.

The Cosmos DB backend compiles but has not been run against Azure or the
emulator; the JSON-file backend and its tests define the behaviour it must
match. There is no BirdNET ingestion and no blob storage yet.
