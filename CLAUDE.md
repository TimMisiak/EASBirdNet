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
│       └── web/        serves frontend/ (cache headers, SPA fallback)
├── frontend/           Shipped as-is; no build step, no bundler
│   ├── index.html      Loads /js/main.js as a module; body is just <bs-app>
│   ├── styles/app.css  Design tokens (--bs-*) + document styles
│   └── js/
│       ├── main.js     Imports every component so they self-register
│       ├── api.js      fetch wrapper for /api/v1
│       └── components/ One custom element per file, plus base-element.js
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
until we actually need a dependency.
*Revisit when:* we need an npm dependency, or asset fingerprinting for
long-lived caching. Then add one build stage to the Dockerfile and bump the
`max-age` in `internal/web`; don't reach for a framework at the same time.

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

**Standard library only.** `net/http` with Go 1.22+ method-and-path patterns
(`"GET /api/v1/health"`) covers routing; `log/slog` covers logging. No router,
no web framework, no logging library. Keep it that way unless something
concrete is missing.

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

Frontend edits need only a browser reload; Go edits need a restart.

Container:

```sh
HOST_PORT=8080 docker compose up --build
```

Checks before committing:

```sh
cd backend && gofmt -l . && go vet ./... && go test ./...
```

## State of the code

The API is wired end-to-end but `internal/api.sampleDetections()` returns
hard-coded rows — there is no database and no BirdNET ingestion yet. The
`Detection` shape is a first guess at what the UI needs; change it freely, but
change `js/components/bs-detection-card.js` with it.
