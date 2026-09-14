# Birdsense — architecture notes

Birdsense is the Eastside Audubon web app for bird-call detections (BirdNET)
from local listening stations. One Go binary serves the JSON API *and* the
static frontend, so the whole app is one container with no reverse proxy, no
Node runtime, and no separate deploy for the UI.

```
/
├── backend/            Go module: API + static file server
│   ├── cmd/server/     main(): config, routing, graceful shutdown
│   └── internal/
│       ├── api/        JSON handlers under /api/v1/
│       ├── cosmos/     Cosmos DB connection + reachability check
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
├── DATA-MODEL.md       Cosmos containers, partition keys, item shapes
├── Dockerfile          Multi-stage: build Go, ship binary + frontend/
└── docker-compose.yml  The app, plus the Cosmos DB emulator
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
until the frontend actually needs an npm package. (The "no dependencies" rule is
about the frontend only; Go modules in `backend/` are fine — see below.)
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

**Go modules are fine; the standard library just happens to cover the web
layer.** Unlike the frontend, the backend has no rule against dependencies —
`go.mod` costs nothing at deploy time, since the Dockerfile already downloads
modules in the build stage. Add one when it beats writing the code yourself.

In practice `net/http` with Go 1.22+ method-and-path patterns
(`"GET /api/v1/health"`) covers routing and `log/slog` covers logging, so there
is no router, web framework or logging library: not on principle, but because
nothing was missing.

`azcosmos` (the Azure SDK for Cosmos DB) is the first module, bringing `azcore`
and two `golang.org/x` modules with it. It requires Go 1.25, which is why
`go.mod` and the builder image in the Dockerfile are on 1.25; keep the two in
step when either moves.

**Azure Cosmos DB for NoSQL, emulated locally.** The containers, partition keys
and item shapes live in `DATA-MODEL.md`; `internal/cosmos` is only the
connection and a read-only reachability check. Nothing is created yet and the
API still answers from the in-memory placeholder store.

The app does not require a database: with no `BIRDSENSE_COSMOS_ENDPOINT` it
serves the frontend, the public page and the placeholder store exactly as
before, so `go run ./cmd/server` stays a one-command dev loop. Compose sets the
endpoint and the emulator's well-known key, and waits for the emulator to
report healthy before starting the app.

Gotcha: the `cosmos` service starts as root and hands its directories to
`cosmosdev` before running the image's own entrypoint. Under a rootless
container runtime the image can unpack with every file owned by root, so the
stock entrypoint (which runs as `cosmosdev`) cannot write anything, and
Postgres inside it refuses to run as root. Don't remove the wrapper because
it looks redundant on a machine where the plain image works — see the
comments in `docker-compose.yml`.

Gotcha: the emulator is pinned to `vnext-EN20251022`, a year behind, because
every later release loads a library into Postgres that is compiled for AVX2
with no fallback, and the dev host's Xeon E5-2667 v2 has no AVX2. The symptom
is Postgres exiting with SIGILL (exit 132) straight after its first log line,
and an emulator that never reports ready. Check the CPU before bumping the tag,
and rename the data volume with it.

Gotcha: the emulator's `GATEWAY_PUBLIC_ENDPOINT` must be `cosmos`, the name the
app dials. The Cosmos SDK reads the account first and then sends every request
to the address the account advertises, which defaults to `localhost` — inside
the app's container, that is the app. The SDK's own failover retries (separate
from the azcore retries `internal/cosmos` turns off) then spin until the
deadline, so the symptom is `read database "birdsense": context deadline
exceeded` in `/api/v1/health`, not a connection error. The same applies to any
future endpoint: what the account advertises has to be reachable from the app.

Gotcha: `/api/v1/health` is *liveness*, not readiness. It answers 200 whenever
the process can serve HTTP — the frontend and the public page do not need a
database — and reports a dependency that is down as `"status": "degraded"` with
the failure text in the body. A container healthcheck must not restart a server
that is still doing most of its job.

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

## Running it

Dev (two concerns, one process — Go serves the frontend from disk):

```sh
cd backend && go run ./cmd/server     # http://localhost:8080
```

Frontend edits need only a browser reload; Go edits need a restart. No database
is involved unless you point one at it:

```sh
BIRDSENSE_COSMOS_ENDPOINT=http://localhost:8081 \
BIRDSENSE_COSMOS_KEY=<emulator key from docker-compose.yml> \
go run ./cmd/server
```

Container (the app plus the Cosmos DB emulator):

```sh
HOST_PORT=8080 docker compose up --build
```

The first run pulls ~600 MB of emulator image, and the emulator takes the better
part of a minute to come up before the app starts. Then:

```sh
curl -s localhost:8080/api/v1/health     # {"status":"ok","dependencies":{"cosmos":...}}
docker compose logs birdsense | grep cosmos
docker compose down -v                   # -v also wipes the emulator's data
```

The emulator is on the compose network only (`http://cosmos:8081`, plain HTTP,
no certificate to trust) because the one assigned host port belongs to the app.
`docker compose exec cosmos cosmoshell` gets you a shell against it; the Data
Explorer needs `ENABLE_EXPLORER` and a published port — see the comments in
`docker-compose.yml`.

Checks before committing:

```sh
cd backend && gofmt -l . && go vet ./... && go test ./...
```

## State of the code

Cosmos DB is connected but unused: `internal/cosmos` dials the account and the
health endpoint reports whether it answers, and that is all. No database, no
containers, no queries — `DATA-MODEL.md` is the plan the first migration has to
implement, and its *Open questions* are still open.

The API is wired end-to-end but every answer comes from `internal/api/store.go`,
which seeds one in-memory copy of the program at startup and forgets it on
restart. There is no BirdNET ingestion, and no individual detections anywhere:
the public page reads hard-coded per-species summaries (`Species`), and the
per-detection shape exists only as a proposal in `DATA-MODEL.md`. Sign-in is
equally fake -- nothing signs the session cookie.
