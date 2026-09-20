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
│   ├── cmd/analyze/    CLI: BirdNET over audio files, JSON out (not the server)
│   └── internal/
│       ├── analysis/   The BirdNET queue: analyzes received cards, stores detections
│       ├── api/        JSON handlers under /api/v1/
│       ├── birdnet/    Runs analyzer/analyze.py (detections) and clip.py (clips)
│       ├── db/         Data model + Store: Cosmos DB (prod) or a JSON file (dev)
│       ├── devseed/    Placeholder program written into an empty dev database
│       ├── retention/  Deletes a card's originals a month on; clips are kept
│       ├── storage/    Card audio and clips: a tusd data store on disk (dev) or Azure Blob Storage
│       └── web/        serves frontend/ (cache headers, SPA fallback)
├── analyzer/           analyze.py, clip.py + pinned requirements.txt: BirdNET in Python
├── test/               Audio fixtures (a known Osprey clip)
├── frontend/           Shipped as-is; no build step, no bundler
│   ├── index.html      Loads /js/main.js as a module; body is just <bs-app>
│   ├── styles/app.css  Design tokens (--bs-*) + document styles
│   ├── images/         Third-party logos (Google, Microsoft), official files
│   └── js/
│       ├── main.js         Imports every component so they self-register
│       ├── api.js          fetch wrapper for /api/v1
│       ├── router.js       History router; in-app links, guards live in <bs-app>
│       ├── session.js       Who is signed in, shared by every component
│       ├── shared-styles.js Constructable stylesheets for repeated primitives
│       ├── format.js        Dates, sizes, counts, durations
│       ├── upload-flow.js   The SD-card upload over tus, which spans four routes
│       ├── card-scan.js     Reads a card folder into a night-by-night manifest
│       ├── upload-status.js Card status -> chip colour and wording
│       └── components/      One custom element per file, plus base-element.js
├── infra/              Terraform (azurerm) for the Azure resources
├── scripts/deploy.ps1  Build the image in ACR, apply the new tag (pwsh:
│                    deploying runs from Windows and Linux; dev is Linux)
├── SCHEMA.md           Stored documents: containers, fields, queries
├── DEPLOYMENT.md       Azure resources and settings: the why behind infra/
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
A few things are not served from this repo, and each degrades rather than
breaks in a container with no outbound network:
- The webfonts (Newsreader, IBM Plex Sans/Mono), linked from Google Fonts in
  `index.html`. Every rule names a real fallback, so you get system fonts.
- Leaflet, for the recorders map. It comes from jsDelivr through the import map
  in `index.html` (pinned version and SRI hash); its CSS is linked inside
  `<bs-station-map>`'s shadow root, so bump both together. The component
  `import()`s it lazily, so if the CDN is unreachable the map shows a note and
  the coordinate fields still work. A third-party frontend module that is
  loaded this way doesn't trigger *Revisit when* below; one that needs npm does.
- tus-js-client, for card uploads. It comes from jsDelivr as its browser build
  (a classic script that defines `window.tus`), pinned with an SRI hash in
  `js/upload-flow.js`, which injects it when an upload starts. It can't use the
  import map: the package's ES modules import CommonJS dependencies, and
  jsDelivr's generated `+esm` bundles can't carry an SRI hash. This one doesn't
  degrade: without jsDelivr a card can't be sent, and the upload page says the
  uploader didn't load.
- Map tiles, from OpenStreetMap's public tile servers. Their usage policy wants
  the attribution kept visible and light traffic; move to a paid provider or
  our own tiles before putting a map on the public landing page.
Self-host any of these if that trade stops being worth it.

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
GET    DELETE /api/v1/session             who you are; sign out
GET    /api/v1/auth/{provider}/start      leave for Google or Microsoft
GET    /api/v1/auth/{provider}/callback   come back from them, signed in
GET    /api/v1/stations                   recorders in the field
GET    POST /api/v1/uploads               your cards; register a card and its files
GET    /api/v1/uploads/{reference}       one of your cards, with its files and their status
POST   /api/v1/uploads/{reference}/progress   the transfer is running or stopped
POST   HEAD PATCH /api/v1/tus/{id}        card audio: one tus upload per file
GET    /api/v1/detections                 every card's detections: filtered, sorted, a page at a time
GET    /api/v1/detections/{reference}?file=        what was heard on a card, or in one of its files
GET    /api/v1/detections/{reference}/{id}         one detection, with its card and file
GET    /api/v1/detections/{reference}/{id}/clip    its clip, as a WAV
PUT    /api/v1/detections/{reference}/{id}/review  confirm, discard, or undo
GET    /api/v1/admin/uploads              every card, admin only
GET    DELETE /api/v1/admin/uploads/{reference}  one card, with every file and its status; delete it
GET    POST /api/v1/admin/people          the roster
PUT    DELETE /api/v1/admin/people/{id}   edit or remove someone
POST   /api/v1/admin/stations
PUT    DELETE /api/v1/admin/stations/{id} rename, move or remove a recorder
GET    /api/v1/dev/people                 the roster, dev mode only
POST   /api/v1/session                    sign in as anyone on it, dev mode only
```

Dev mode follows `BIRDSENSE_DB=local` (Azure runs Cosmos, so it can't be on
there). It registers the `/dev/*` routes *and* `POST /session`, and
`GET /session` reports it as `dev`, so the sign-in page offers a picker of
everyone on the roster. A deployed server registers neither, so its only way in
is a provider.

A card belongs to a volunteer: `/uploads/{ref}` and its tus uploads 404 for
anyone else, and the `/admin/*` routes 403 for a volunteer.
Detections are not a card's: anyone signed in hears and reviews every card's,
under `/detections`. A card's counts are
the server's own tally of the files it has received (`tallyFiles`), never
reported by the client, so a retried chunk can't count twice, and a card moves
to `processing` only once every file on its list is in. The roster always keeps an admin: removing or
demoting the last one is a 409, and so is an admin removing themselves.

**Routes are paths, not hashes.** `/`, `/signin`, the tabs everyone signed in
has (`/app/upload` and its steps, such as `/app/upload/check`; `/app/uploads`;
`/app/detections`), the three a coordinator has as well (`/admin/uploads`,
`/admin/recorders`, `/admin/people`), a card (`/admin/uploads/{reference}`) and
a detection opened from it
(`/admin/uploads/{reference}/detections/{id}`), and one opened from the
detections list, `/app/detections/{reference}/{id}`. A page
may keep its view in the query string (`/app/detections?species=Strix+varia`):
the router carries it through links, `replaceQuery` rewrites it without a
history entry, and the detection page carries the list's back with it.
`internal/web` already falls back to index.html for
extension-less paths, so a reload mid-wizard lands on the same screen, and a
coordinator can send a colleague a link to one admin tab. `js/router.js` is the
whole router; `<bs-app>` holds the route table (exact paths, plus `prefix`
routes for a path with an id on the end) and the two guards (signed in,
and admin for `/admin/*`). Guards are convenience only -- the API enforces the
same rules, so guessing a path gets you a 401 or 403, not data.
A route may `redirect` instead of naming a page, as a path or as a function of
the path; the detections tab was two tabs once, so `/admin/detections` and
`/admin/detections/{reference}/{id}` redirect to the `/app` ones, filters and
all.

**Sign-in is OpenID Connect; the roster is the allow-list.** `internal/api/auth.go`
runs the authorization-code flow (PKCE, state and nonce in one short-lived
signed cookie) against Microsoft, and Google when a second client id is set.
Verifying the ID token is a real job, so it uses `github.com/coreos/go-oidc`
rather than parsing JWTs by hand. What a provider proves is *which address you
own*; what lets you in is a coordinator having put that address on the roster,
which is why the sign-in audience can safely be "any Microsoft account,
personal ones included" and volunteers use whatever address they already have.
Two consequences worth knowing:
- Accepting any Microsoft account means accepting any organization's
  directory, and a directory's administrators choose what their users' `email`
  claims say. So an email claim is only believed when the provider is in a
  position to know it: a personal account (`tid` is the consumer tenant), an
  organization that has proved it owns the domain (`xms_edov`, an optional
  claim the app registration has to ask for), or Google's `email_verified`.
  Anything else is refused with `unverified-email`. `trustedEmail` is the
  whole rule.
- Microsoft's multi-tenant endpoints report a *templated* issuer, which
  go-oidc refuses, so discovery passes `InsecureIssuerURLContext` and the
  issuer is checked per token against the token's own `tid` (`verifyIssuer`).
The session cookie is the address, HMAC-signed with `BIRDSENSE_SESSION_KEY`
(`cookies.go`); the key is configuration, not generated at startup, so a new
revision doesn't sign everyone out -- and rotating it deliberately is how you
end every session at once. Removing someone from the roster already ends
theirs, so there is no session store to revoke from.
*Revisit when:* a provider has to be something other than Google or Microsoft,
or sessions need to be listed and revoked one at a time.

**Card audio goes over tus, through the app.** A card is ~128 GB in files of a
few hundred MB, sent over home connections that drop. The browser sends each
file as its own tus upload (tus-js-client) to `/api/v1/tus/`, where tusd, used
as a Go library rather than its standalone server, writes it through
`internal/storage`: a directory in dev, block blobs in Azure. A dropped or
paused file resumes from the last chunk the server has. Birdsense's rules live
in tusd's hooks (`internal/api/tus.go`). A file can only be created if it is on
the list its card was registered with, at that size, and not already in. When
its last byte lands, its `audioFiles` document is marked `uploaded` and the card
recounted, before the browser is told it succeeded. Files go one at a time, in
50 MB chunks, so each request fits the Container Apps ingress timeout.
Going through the app rather than straight to Blob Storage with SAS URLs keeps
one upload path for dev and prod and the card rules in Go, at the cost of every
byte passing through the container.
Gotchas: tusd's locks are in memory, so every request for an upload has to
reach the same process -- one replica (see DEPLOYMENT.md). And tusd's
`Config.Logger` is a `golang.org/x/exp/slog` logger, not `log/slog`, so tusd
writes its own warnings to stdout.
*Revisit when:* the app needs more than one replica, or the container's share
of a card upload (CPU, bandwidth) costs more than direct-to-blob uploads would.

**Terraform owns what is deployed.** `infra/` is the whole Azure stack and
`scripts/deploy.ps1` is the whole deploy: build this commit in ACR, then
`terraform apply -var image_tag=<sha>`. One owner for the running image means a
rollback is applying an older tag and `terraform plan` is never wrong about the
app, at the cost of needing Terraform credentials to deploy. So nothing runs
`az containerapp update` -- that is drift the next apply reverts.
*Revisit when:* deploys move to CI, which should get an identity that can push
images and update the app but not touch state; that is the point of
`ignore_changes` on the image (DEPLOYMENT.md, *Deploying a new version*).

**One set of tabs, and the admin ones are hidden.** A coordinator is a
volunteer with three more tabs, not a second application:
`<bs-app-page>` is the shell for everyone signed in, and its tab table marks
three entries `admin`, which `session.isAdmin()` filters out. So a coordinator
uploads a card, resumes their own, and reviews detections on the same screens a
volunteer does, and a screen only has to be built and kept working once. The
admin tabs keep their `/admin/` paths, because that is what the route guard
reads and what makes an admin-only page obvious in a link.
*Revisit when:* the roster grows a third role, or a coordinator's version of a
shared screen has to differ by more than what it lists.

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

**BirdNET runs as a Python subprocess.** BirdNET's maintained runtime is the
`birdnet` Python package, so `internal/birdnet` runs `analyzer/analyze.py`
with a batch of files and parses the one JSON object it prints, rather than
binding a TFLite runtime into Go through cgo. Clips are cut the same way, by
`analyzer/clip.py` with soundfile, which is what birdnet reads audio with, so a
file BirdNET could analyze can be cut. The model loads once per batch, so
hand it a night of files, not one at a time. The package uses BirdNET v2.4 on
LiteRT (`library="litert"`), which needs no TensorFlow. Two consequences:
- The runtime image is Debian (`python:3.12-slim-bookworm`), not Alpine,
  because the LiteRT and numpy wheels are glibc builds. The venv and the models
  (~90 MB, acoustic plus the geo model for location filtering) are baked in at
  build time, so analysis never needs the network. The image is now several
  hundred MB rather than ~20.
- Each inference worker is its own process with its own model copy (~285 MB
  peak for the whole tree with one worker). Cancelling the context kills the
  process group, so workers don't outlive a cancelled run.
**The analysis queue is the database.** `internal/analysis` runs in the server
process. A card in `processing` with files in `uploaded` status is queued work;
there is no separate queue to keep in step with the documents, and nothing in
memory to lose. `Queue.Run` takes one file at a time, oldest card first: it
copies the file out of `internal/storage` to a temp file (keeping its
extension, which birdnet picks a decoder by), runs BirdNET with the recorder's
position and week, merges each species' consecutive windows into one detection
at the highest confidence (`merge.go`), cuts a clip of each with `clip.py` (the
run and 1 s either side, at most 30 s around its best window) into storage at
`clips/{card}/{detection}.wav`, upserts the detections, marks the file
`analyzed`, and recounts the card. The API only calls `Enqueue` to wake it when a card's last
file lands. At startup it resumes whatever was left, including a file cut off
mid-run (`analyzing`); detection ids are deterministic, so a re-run overwrites.
A file BirdNET can't read fails at once; a crashed run (of either script) is retried twice, 30 s
apart and growing, and then fails, so one file can't wedge the queue. The last
file moves the card to `in_review`, or `needs_attention` if any failed. If the
server's Python can't `import birdnet` at startup it logs a warning and doesn't
start the queue, so cards wait in `processing` rather than failing.
One file per run, not a night per run: measured on the Osprey clip, a warm run
spends ~3 s starting Python and loading the model, and BirdNET takes ~18 s per
10 minutes of audio. On hour-long card files that overhead is ~3%, and in
exchange each file's detections are stored as soon as it's done and only one
file sits in temp storage at a time.
Perch v2 is available in the same package but is TensorFlow-only (another
~600 MB). Add it as a second model in `analyze.py` if BirdNET's accuracy isn't
enough, not before.
*Revisit when:* analysis moves to its own Container Apps job (see
DEPLOYMENT.md). Then the web image can go back to Alpine and this stage moves to
the job's image, and `Queue.Run` is what the job runs.

**Originals expire; clips don't.** A card is ~128 GB of audio against a few
megabytes of clips, and nothing reads an original once BirdNET has:
`internal/analysis` is the only reader of `uploads/`, and the only audio a
browser ever plays is a clip. So `internal/retention` sweeps every few hours in
the server process and deletes a card's recordings a month after it was
received, while its detections and their clips are kept until someone deletes
the card. `BIRDSENSE_AUDIO_RETENTION_DAYS` sets the window; `0` keeps
everything. The sweep only looks at cards BirdNET has finished with
(`in_review`, `needs_attention`, `results_sent`) and, on them, only at files in
`analyzed` or `failed` status: a card stuck in `processing` keeps its audio
however old it is, so the policy can't take a recording the queue is still
waiting to read. The blob goes before the document is marked, because a sweep
interrupted in between leaves something the next sweep finishes, whereas the
other order leaks a blob nothing names. Doing it in Go rather than with an
Azure lifecycle rule keeps dev and prod the same, lets the rule see whether a
file has been analyzed, and keeps `blobName` honest; the lifecycle rule stays
as a backstop for abandoned partial uploads, which have no document at all.
The cost of the policy is that a card can't be re-analyzed once its window
passes -- a better model, or a lower threshold, only ever runs against cards
still inside it.
*Revisit when:* a card has to be held past its window (a hold flag on the
upload, which the sweep would skip), or re-analysis matters more than the bill.

**Spectrograms are drawn in the browser.** `<bs-spectrogram>` fetches a
detection's clip once, plays it from a blob URL, decodes it with Web Audio and
runs its own FFT onto a canvas, in the paper and ink tokens. The clip is the
only thing stored, and there is no image library or frontend package.
*Revisit when:* a spectrogram has to appear without JavaScript (an email), or
another page needs the same picture of a whole file.

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
`BIRDSENSE_DB=local` stores data in `backend/data/birdsense.json` (git-ignored,
override with `BIRDSENSE_LOCAL_DB_PATH`), and uploaded audio under
`backend/data/audio` (`BIRDSENSE_STORAGE_DIR`), at the names it would have in
Blob Storage. There is no sample card: to try an upload, point the folder
picker at any folder with a few `.wav` files in it. When that file is empty, startup fills
it with the placeholder program from `internal/devseed`: people to sign in as
and recorders. It seeds no cards or detections, so the card lists and the landing
page stay empty until you upload a card and BirdNET runs over it. Delete the file
to re-seed. Without
`BIRDSENSE_DB=local` the server expects Cosmos DB (`BIRDSENSE_COSMOS_ENDPOINT`,
`BIRDSENSE_COSMOS_DATABASE`) and Blob Storage (`BIRDSENSE_BLOB_ENDPOINT`,
`BIRDSENSE_BLOB_CONTAINER`), and exits if they aren't configured.
`BIRDSENSE_STORAGE=local|azure` overrides where audio goes either way.
`BIRDSENSE_AUDIO_RETENTION_DAYS` (default 30) is how long a card's original
recordings are kept; set it to `0` in dev if you want a test card's audio to
stay put for good.
Sign-in needs `BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID` and `_CLIENT_SECRET` (and
`_TENANT`, default `common`; `BIRDSENSE_OIDC_GOOGLE_*` the same way),
`BIRDSENSE_PUBLIC_URL` -- where a browser reaches the app, which the redirect
URI is built from and which has to match what is registered with the provider
-- and `BIRDSENSE_SESSION_KEY`. Outside dev mode the server refuses to start
without them, because the development sign-in isn't registered there and it
would have no way in at all. Dev mode fills all four in (a random session key,
`http://localhost:8080`) and offers the roster picker instead, so the real flow
is opt-in locally: set the three Microsoft variables and register
`http://localhost:8080/api/v1/auth/microsoft/callback`, which providers allow
over plain http for localhost only.
`BIRDSENSE_BOOTSTRAP_ADMIN="Name <email>"` adds the first admin to an empty
roster (required on a first deploy, see DEPLOYMENT.md). In dev it runs before
the seed, so setting it starts you from a clean roster. Outside dev mode it
refuses anyone from the placeholder roster, so dev people never reach Cosmos.

BirdNET, for the server's analysis queue, `internal/birdnet` and `cmd/analyze`
(needs Python 3.12; models download to `$BIRDNET_APP_DATA`, default
`~/.local/share/birdnet`, on first use). The server looks for
`../.venv/bin/python` and `../analyzer/analyze.py` from `backend/`
(`BIRDSENSE_BIRDNET_PYTHON`, `BIRDSENSE_BIRDNET_SCRIPT`); without them it says
so at startup and received cards wait in `processing`:

```sh
python3.12 -m venv .venv && .venv/bin/pip install -r analyzer/requirements.txt
cd backend && BIRDSENSE_BIRDNET_PYTHON=../.venv/bin/python go run ./cmd/analyze "../test/2026-09-09 Osprey.wav"
```

Container (compose sets `BIRDSENSE_DB=local` and `BIRDSENSE_STORAGE=local`, and
keeps the database and the audio in two named volumes):

```sh
HOST_PORT=8080 docker compose up --build
```

Checks before committing:

```sh
cd backend && gofmt -l . && go vet ./... && go test ./...
```

`TestAzure` in `internal/storage` runs the blob backend against Azurite, and is
skipped unless `BIRDSENSE_TEST_AZURITE` names its endpoint (the command is in
the test). Set it when changing `internal/storage` or the tusd version.

`TestAnalyzeOsprey` runs the real model, and `TestCutOsprey` the real
`clip.py`; both are skipped unless `BIRDSENSE_BIRDNET_PYTHON` names a Python
with `analyzer/requirements.txt` installed. Set it when changing
`internal/birdnet`, `analyze.py`, `clip.py`, or the pins.

BirdNET in the image, on the test clip:

```sh
docker compose run --rm -v "$PWD/test:/test:ro" --entrypoint /app/birdsense-analyze \
  birdsense "/test/2026-09-09 Osprey.wav"
```

## State of the code

Every handler reads and writes through `db.Store`; there is no in-memory data
left in `internal/api`. The API's JSON shapes live in `internal/api/shapes.go`
and differ from the stored documents in a few names. SCHEMA.md's *API mapping*
section is the rulebook for both the fields and what each write route does
(DELETE on people and stations sets `removedAt`/`retiredAt`; deleting a card
is the one hard delete, and takes its audio and detections with it).
`internal/api` tests build their own small fixed-date program in the JSON
backend, deliberately not the dev seed.

Sign-in is real: OpenID Connect against Microsoft, with the roster as the
allow-list (see *Sign-in* above). `POST /session {"role": ...}` still signs in
as the first person with that role, but only in dev mode. Google is written and
untested -- it needs a client id, a secret, and someone to try it.
The Cosmos DB backend and the Blob Storage backend have both run in Azure
against a real card: documents, uploads and analysis all work there. The
JSON-file backend and its tests still define the behaviour Cosmos must match.
Uploaded audio is stored, a card whose files are all in moves to `processing`,
and the analysis queue runs BirdNET over it in the server process, writing
`unreviewed` detections, each with a clip. The coordinator's card page
(`/admin/uploads/{ref}`) shows each file's status, when its recording is due to
be removed or was, and what was heard in it, and
each detection has its own page with its clip, a spectrogram, and Confirm and
Discard. The Detections tab (`/app/detections`) lists every card's detections,
sortable by when, species or confidence and filtered by review, species,
minimum confidence and the days heard, and opens the same detection page.
Anyone signed in can review. Everyone works in the same shell
(`<bs-app-page>`): Upload, the default, holding the card upload's four steps;
My uploads, their own cards, where an unfinished one is resumed; and
Detections. A coordinator gets three more tabs after those -- All uploads,
Recorders and People. Nothing moves a card from `in_review`
to `results_sent` yet, and there is no email. Detections stored before clips
were cut have no clip and weren't merged; nothing backfills them.
Nothing cleans up abandoned partial uploads, short of deleting their card or
the storage lifecycle rule getting to them: they have no `audioFiles` document,
so retention never sees them.
