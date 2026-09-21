# Birdsense — what's left before it goes live

Last full review: `dd72866`, 2026-09-19 — update that line when the list is
swept again; individual items don't carry dates. This is a review of the whole
repo against the plan already written down in [CLAUDE.md](CLAUDE.md),
[SCHEMA.md](SCHEMA.md) and [DEPLOYMENT.md](DEPLOYMENT.md). Nothing here is a new
idea for the product: every item is either something those documents already
promise and the code doesn't do, or something that will go wrong on the first
real card.

There was no existing pre-launch checklist. The closest things are
DEPLOYMENT.md's *First deploy* and *Open questions that change this file*, and
CLAUDE.md's *State of the code*; all of them are folded in below.

**The core path works.** Verified end to end on a dev server against the real
model: register a card → tus upload → `processing` → BirdNET → `in_review`,
detection stored with a clip, clip served as WAV, review confirmed, public
overview picked it up. Blob layout matches SCHEMA.md. `gofmt -l`, `go vet ./...`
and `go test ./...` are all clean. What follows is what sits around that path.

Items marked **[verified]** were reproduced by running the code, not only by
reading it.

---

## How to work this list

**Picking an item.** "The next item" is the first one still listed in Part 1,
reading top to bottom. Part 1 is in working order — if something else matters
more, move it up the file rather than skipping past it. Part 2 isn't ordered:
take a cleanup item when it's asked for, or when you're already editing that
file for another reason.

**Item numbers are permanent addresses.** Never renumber. When an item goes, its
number retires with it, so `4.1` means the same thing next month as it does
today. Gaps in the numbering are correct and should be left alone.

**Finishing an item:**

1. Do the work. Where a regression is likely or would be expensive — anything
   that could lose or corrupt data, a rule the API enforces, a shape both
   database backends have to agree on, a check that only fires on deploy —
   leave behind something that fails if it comes back: a test, a Terraform
   `precondition`, a startup check. That check is what replaces the entry here.
   Where a regression would be obvious the moment someone looks at the screen,
   skip it; UI behaviour rarely earns a test. Don't add one for the sake of
   having added one, and don't leave an item unfinished for want of a test.
2. Put any lasting fact where that kind of fact already lives:
   - a stored field or document shape → SCHEMA.md, together with
     `internal/db/models.go`
   - an Azure resource or setting → DEPLOYMENT.md
   - a decision, or a constraint someone could undo without noticing →
     CLAUDE.md, in the same shape as the decisions already there, with its
     *Revisit when*
   - how to run, build or deploy it → README.md
3. Delete the entry. No strikethrough, no "DONE", no completed section. Git
   history is the record of what happened; this file is only what's left.

**Write the state, not the journey.** The docs say what is true now. No
changelog entries, no "this used to be…", no notes on what was tried before it
worked. A reader should not be able to tell from the docs that this list ever
existed.

**The one exception** is a dead end that would cost the next person real time:
the obvious approach that doesn't work, and one line on why. Put it next to what
it protects — a comment on the code, or a *Gotcha* in the doc that owns that
area — never in this file. `internal/storage/storage.go`'s note on tusd's empty
sentinel block is the model to copy.

**Two kinds of item you should not simply carry out:**

- **[decide]** — the item names a choice that isn't an implementer's to make.
  Bring the options and a recommendation, and stop.
- **[needs Azure]**, **[needs a real card]**, **[needs credentials]** — can't be
  finished from a laptop. Leave them listed; don't approximate them.

**If an item turns out to be wrong** — the bug isn't real, or the fix belongs
somewhere else — rewrite the entry to say what is actually true. Don't delete it
quietly.

---

# Part 1 — Before the first release

## 1. Correctness and data loss

Audio is irreplaceable: the volunteer erases the card, and originals are deleted
after 30 days. These are the items where being wrong costs recordings.

## 2. The app tells volunteers and coordinators things that aren't true

The program depends on volunteers trusting what the screen says about a card
they are about to erase.

### 2.1 Four screens promise emails that are never sent — **[decide]**
`bs-upload-done.js:81`, `:99`; `bs-upload-details.js:232`;
`bs-upload-progress.js:227` — **[verified]**

There is no email anywhere in the codebase. The UI says "We've emailed you a
copy of this summary" (false at the moment it renders), "You'll get a second
email with the results for this card", and — twice — "Keep the card until you
get the confirmation email". Step 4 also contradicts itself: "The card is safe
to erase" sits three lines above the email claim.

**If not fixed:** a volunteer who follows the instruction keeps the card
forever, because the email never arrives. With five stations on a two-week
rotation that stops the program. Decide before launch: send the emails, or
change the copy to say what actually happens (the card page shows progress; come
back and look). The copy change is small and can ship now; email is its own
piece of work.

### 2.5 A placeholder graphic ships on the public landing page
`frontend/js/components/bs-home-page.js:105-121`, `:173-176` — **[verified]**

The hero `<figure>` is a CSS stripe pattern captioned
`photo — barred owl at dusk, member submission`.

**If not fixed:** on the one page an unauthenticated visitor sees, for a named
Audubon chapter, this reads as a broken image. It is also the only place in the
codebase with hard-coded hex colors, against the design-token rule.

### 2.6 A card can never be closed out
`backend/internal/db/models.go:79,117`, `frontend/js/upload-status.js:13` — **[verified]**

`results_sent` and `resultsSentAt` are modelled, rendered by the status chip,
and treated as settled by retention — but nothing sets them, and no route
exists. SCHEMA.md's status diagram also shows "any state → (coordinator flags
it) → `needs_attention` → (resolved) → back"; there is no route for that either.

**If not fixed:** every analyzed card sits in `in_review` forever. "All uploads"
only grows, a coordinator has no way to mark a card done or to clear a
`needs_attention`, and the volunteer's "My uploads" never reaches a terminal
state. (Retention still expires the audio, so nothing is stranded.)

---

## 3. Scale and cost — this bites during the first season, not later

### 3.4 Clip storage is probably budgeted orders of magnitude low, and nothing caps it — the measurement can now be read off the card that has run; the per-file cap is implementable now
`backend/internal/analysis/analysis.go:276-312`, `backend/internal/analysis/merge.go:15-25` — **[verified]**

Measured on the test clip: a 13.96 s detection produced a **1.23 MB** clip
(44.1 kHz 16-bit mono; the 30 s cap makes ~2.6 MB the maximum). CLAUDE.md and
SCHEMA.md both reason from "a card is ~128 GB of audio against a few megabytes
of clips", and clips are kept **for good**. At `minConfidence = 0.25` with
`topK = 5`, a noisy hour-long dawn file can merge to hundreds of detections, and
nothing bounds clips per file.

**If not fixed:** clip storage — the one thing that never expires — grows
without bound and may become the dominant long-term cost, while the retention
policy that exists to control the bill only touches originals. Temp space is the
short-term version of the same problem: every clip for a file is written to
`os.MkdirTemp` before upload. Measure detections-per-file on the card that has
already gone through Azure, then decide on a per-file cap, a higher clip
threshold, or a smaller clip format. The same card says how many detections a
card yields, which is what says whether the Detections tab's 30-day default
window is the right size.

### 3.5 The cost table contradicts `min_replicas = 1`
DEPLOYMENT.md *Cost* vs `infra/app.tf:76` — **[verified]**

The table lists Container Apps as "low traffic, scale to zero | usually within
the monthly free grant". `min_replicas = 1` is set, and DEPLOYMENT.md explains
at length why it must be. An always-on 1 vCPU / 2 GiB replica is tens of dollars
a month and the second-largest recurring line item after blob storage. Fix the
number before anyone budgets from it.

---

## 4. Security

### 4.7 Two decisions to make deliberately, not by default — **[decide]**

- **Volunteer addresses in logs.** `auth.go:337,347` and `main.go:398-402` log
  email addresses into Log Analytics. Defensible as an audit trail, but it puts
  a roster of volunteers' personal addresses in a second system with its own
  retention.
- **Recorder names on the public page.** The public overview publishes the
  recorder name per species (`Marymoor Park – Snag Row`), by design per
  SCHEMA.md's *API mapping*. For owls this is a location-disclosure decision;
  confirm it is the one the chapter wants before the page is public.

---

## 5. First deploy and running it

The stack is applied and running. What is left is what nobody is watching once
it runs, and what a fresh apply -- a staging copy, or a rebuild from scratch --
still walks into.

### 5.3 No monitoring, no alerts, no diagnostic settings
`infra/*.tf`

Zero `azurerm_monitor_*` resources, no action group. Log Analytics collects
logs; nothing reads them.

**If not fixed:** none of these is visible to anyone — BirdNET failing to import
at startup (so cards sit in `processing`), the Entra client secret expiring (so
all sign-in stops), Cosmos 429s during bulk upserts, replica restarts (5.1). For
a volunteer-run program the minimum is an action group plus alerts on replica
restarts, 5xx rate, and a scheduled query on the "BirdNET isn't available" log
line.

### 5.4 Rotating a secret never reaches the running container
`infra/app.tf:36-43`

Container Apps secrets live in `properties.configuration`, not in the revision
template. Changing `session_key` or `oidc_microsoft_client_secret` and
re-applying — especially via the settings-only workflow in README.md, which
reuses the running `image_tag` — produces no new revision, and the replica keeps
the old value.

**If not fixed:** the documented rotation story ("a new value signs everyone
out… how to end every session at once on purpose") silently doesn't work, and an
emergency client-secret rotation would no-op. Derive
`template.revision_suffix` from a hash of the secret values, or document a
revision restart as part of rotation.

### 5.5 The first apply will probably fail on RBAC propagation
`infra/storage.tf:37-43`, `infra/registry.tf:112-116`

Creating the `audio` container is a data-plane call (`storage_use_azuread`) that
depends only on the role assignment resource, with no `time_sleep`; Azure RBAC
takes 1–5 minutes to propagate. None of the three role assignments sets
`principal_type = "ServicePrincipal"`, the standard mitigation for "principal
does not exist in the directory" against a just-created managed identity.
DEPLOYMENT.md's *Ordering* acknowledges the app-side race but not this one, so a
first-time deployer meets an unexplained 403.

### 5.6 Sign-in can't work on the first deploy, and the documented order is circular
`infra/variables.tf:148,158`, `infra/app.tf:147`, `infra/prod.tfvars.example:92`

The OIDC client id and secret are required with no default, so the app
registration must exist first. But `BIRDSENSE_PUBLIC_URL` defaults to the
container app's generated `default_domain`, which can't be known until the
environment exists — and the tfvars example tells you to register the redirect
URI from `terraform output -raw app_url`.

**If not fixed:** the first deploy comes up with a redirect URI the provider
doesn't know, and nobody — including the bootstrap admin — can sign in until
it's added afterwards. The server logs the exact URI at startup
(`main.go:120-122`), which is the recovery; the two-step just needs to be the
documented procedure. This is also where the custom-domain decision
(DEPLOYMENT.md *Open questions*) has to be made, since it changes the URI again.

### 5.9 The irreplaceable data has the weakest durability in the stack
`infra/storage.tf:23-28` vs `infra/cosmos.tf:41-44`

Cosmos gets continuous 7-day point-in-time restore. Blob gets
`delete_retention_policy { days = 7 }` and nothing else: no `versioning_enabled`,
no `restore_policy`, no container delete retention, LRS. Since originals are
deliberately deleted after 30 days, `clips/` is the only surviving audio for any
card older than a month.

**If not fixed:** a retention bug, a mistaken `DELETE /admin/uploads/{ref}` or
an operator error is recoverable for 7 days and then not at all.
**Caveat before "fixing" it:** `backend/internal/storage/storage.go:298-306`
documents that the `skipEmpty` tusd workaround is only safe *because* blob
versioning is off — turning on `versioning_enabled` would break every card
upload. `storage.tf` carries no warning about that coupling; it should.

### 5.11 Every deploy rebuilds the BirdNET stage from scratch
`scripts/deploy.ps1:110`, `Dockerfile:21-37`

`az acr build` runs on a fresh agent with no `--cache-from` and no registry
cache, so every deploy pip-installs the pinned requirements and re-downloads
~90 MB of models from Zenodo.

**If not fixed:** multi-minute deploys, and a deploy that can fail because PyPI
or Zenodo is having a bad day. A rollback is no longer exposed to this —
`deploy.ps1 -ImageTag` applies an image that is already built (ROLLBACK.md) —
so this is now about forward deploys only.

---

## 6. Documents that are now wrong

These matter because the docs are how the next person — or the same person in
six months — decides what is true.

---

## 7. Verification gaps that gate the release

### 7.1 The Cosmos backend has no tests of any kind
`backend/internal/db/` — **[verified]**

There is no `cosmos_test.go`. CLAUDE.md's stated invariant is that both backends
"must give identical answers" and that "the JSON-file backend and its tests
still define the behaviour Cosmos must match" — but nothing enforces it, and
Cosmos is what production runs. `Config.CosmosKey` exists specifically "for the
Cosmos DB emulator" with nothing using it.

The right shape is a conformance suite — the body of `jsonfile_test.go`
parameterised over both stores — run against the emulator behind an env-var
skip, exactly the pattern `TestAzure` / `BIRDSENSE_TEST_AZURITE` already uses in
`storage_test.go:201`. This is the highest-value test gap in the repo.

### 7.2 The OIDC HTTP flow has no test
`backend/internal/api/auth_test.go` — **[verified]**

It tests only the pure helpers (`trustedEmail`, `verifyIssuer`, the state
cookie, token randomness). Nothing exercises `authStart`, `authCallback` or
`identify` — zero test references to `/api/v1/auth/`. Sign-in is the security
boundary and the most recently changed file in the package. A state-mismatch, a
nonce-mismatch and a not-on-roster test against a stub provider would be cheap.

### 7.3 `secureCookies` is never exercised
`backend/internal/api/api_test.go:59-66` — a regression dropping `Secure` or
`HttpOnly` from the production session cookie would fail nothing.

### 7.4 Google sign-in is written and untested — **[needs credentials]**
CLAUDE.md says so. It needs a client id, a secret and someone to try it — or the
button should not be reachable at launch.

---

# Part 2 — Cleanup

None of this is needed for the first release. It is here because it makes the
code smaller or easier to keep correct, and because writing it down stops it
being rediscovered.

## Shape and size

- **`api.go` is 1292 lines and `createUpload` is 117 of them**
  (`api.go:379-495`), with a mutate closure nested inside a conflict branch
  inside an error chain. `detections.go`, `overview.go`, `auth.go` and `tus.go`
  are already split out; `people.go`, `stations.go` and `uploads.go` follow the
  same seam.
- **`retention.Sweep` runs three cross-partition `ListUploads` calls**
  (`retention.go:113`) because `UploadFilter` has no multi-status field.
- **`analysis.tally` replaces the upload document after every file**
  (`analysis.go:442`) even when nothing changed — ~336 no-op writes per card.
- **`analysis.drain` aborts the whole pass on the first card's error**
  (`analysis.go:148-163`), so one card's store failure stalls every other
  `processing` card for a cycle.
- **`Queue.Enqueue`'s `reference` parameter is unused** (`analysis.go:97`).

## Robustness, not urgent

- **Orphaned analysis temp directories are never swept** (`analysis.go:356`).
  A SIGKILL mid-run leaves a full copy of a file plus its clips in
  `birdsense-analysis-*`. Nothing removes stale ones at queue start; on a
  replica that also buffers tus bodies, a couple of crashed runs can fill the
  disk. Nothing bounds ephemeral disk in `infra/` either.
- **`DetectedAt` isn't normalised to UTC by the store**
  (`db.go:248-261`). The Cosmos range query depends on a `Z`-suffixed string;
  `analyzeFile` happens to do that, nothing enforces it. One `.UTC()` in
  `prepareDetection` makes the invariant real.
- **`analyzer/analyze.py:80` indexes `results[str(row["input"])]`** — a path
  birdnet reports differently from what was passed (a resolved symlink) raises
  `KeyError`, which then burns all three attempts on that file.
- **`OpenJSONFile` rejects a zero-byte file** with "is not a Birdsense data
  file" (`jsonfile.go:60-66`) — a plausible papercut if someone `touch`es the
  path. Treating empty as "new" would be kinder.
- **The JSON backend enforces a rule Cosmos can't see** — it rejects an upsert
  whose id exists under a different `uploadId` (`jsonfile.go:292-294,345-347`);
  Cosmos would just write it. Unreachable today, but it's a rule only one
  backend has.
- **Review corrections are modelled but unreachable**
  (`models.go:253-255`). `PUT .../review` takes only `{"status"}`, so
  `Detection.Species()` can never differ from the BirdNET label and the public
  overview's "grouped by `Species()`" is effectively grouping by the raw label.
  Either wire them up or drop the fields.
- **Abandoned partial uploads in dev.** Azure has the lifecycle backstop
  (`storage.tf:82-97`); the local backend has no equivalent, so
  `BIRDSENSE_STORAGE=local` accumulates orphaned `uploads/{ref}/{random}` +
  `.info` pairs indefinitely. CLAUDE.md's "nothing cleans up abandoned partial
  uploads" understates prod and overstates dev.
- **`internal/web` serves directory listings** (`web.go:26`) — `GET /js/`
  returns a full `<pre>` listing of every module. All public content, so not an
  exposure, but unintended.
- **`decode` flattens every body error into one message**
  (`api.go:1258-1267`), so a frontend/API field drift is undebuggable from the
  response. `http.MaxBytesReader(nil, ...)` also means the server is never told
  the request was over-long, so a client still sending gets a reset rather than
  the 400. Pass `w` through.
- **`publicOverview` silently ignores a bad `days`** (`api.go:167-171`), falling
  back to 7, while `parseDetectionQuery` 400s the same class of input.
- **The two signed cookies share one key with no domain separation**
  (`cookies.go:50-53`). Not exploitable — neither value parses as the other —
  but a one-byte type tag removes the class.

## Frontend details

- **Route changes move neither focus nor scroll, and never set
  `document.title`** (`router.js:13-18`). Opening a detection renders at the old
  scroll offset, focus stays put, and every history entry reads "Birdsense —
  Eastside Audubon".
- **Five of the eight signed-in pages have no heading at all** —
  `bs-detections`, `bs-my-uploads`, `bs-admin-uploads`, `bs-admin-people`,
  `bs-admin-recorders`. The only heading on those routes is the shell's
  time-of-day greeting, so a screen-reader user hears "Good morning." and then a
  table.
- **The spectrogram can't be seeked without a mouse**
  (`bs-spectrogram.js:185-190`). The `<audio>` has no `controls`; seeking is a
  click listener on a div with no `tabindex`, `role` or key handler. ←/→ are
  bound to navigation, not scrubbing, so the obvious keys do the opposite of
  what an audio player implies.
- **`bs-progress-bar` interpolates `label` into `aria-label` unescaped**
  (`bs-progress-bar.js:35`). The caller escapes on the way in
  (`bs-upload-file-list.js:55`), but the browser decodes entities, so
  `getAttribute` hands back the raw path. Only a self-supplied filename reaches
  it today; it is the one attribute interpolation that bypasses `escapeHTML`.
- **A malformed percent-escape in a path throws before anything renders**
  (`bs-app-page.js:79-84`) — `/admin/uploads/%zz` gives a blank page instead of
  the 404 screen.
- **`viaDirectoryPicker`'s bare `catch` turns every failure into "changed their
  mind"** (`card-scan.js:33-37`) — a `SecurityError` or `NotAllowedError` is
  reported as cancellation, which renders no message at all. Check
  `error.name === "AbortError"`.
- **The 404 page's link says "Back to the detections page" but points at `/`**,
  the public landing page (`bs-app.js:110`).
- **`<a href="#recheck" data-action="back">` is a fake link**
  (`bs-upload-check.js:98`) — the hash goes nowhere and the click is intercepted.
  Should be a `<button>`, like the identical control ten lines below.
- **A bare `<span>` is a direct child of `<ol class="band">`**
  (`bs-upload-steps.js:70`), which only permits `<li>`.
- **The document-level `:focus-visible` ring never reaches a component**
  (`app.css:128`) — outline isn't inherited and document stylesheets stop at the
  shadow boundary, so every button and link falls back to the UA ring. The
  comment above it claims the opposite. Move it into an adopted sheet, as
  `.field:focus-visible` already is.
- **Dead code**: `#route` assigned and never read (`bs-app.js:46,75`);
  `const user = await session.signIn(body)` unused (`bs-signin-page.js:81`);
  `--bs-amber-track`, `--bs-bg-deep`, `--bs-surface-band` defined and never
  referenced.
- **Hard-coded spacing is pervasive despite the token rule** — `1.125rem`,
  `1.375rem`, `2.125rem` and friends in nearly every component, plus
  `rgba(35, 64, 47, 0.16)` at `bs-station-map.js:101`. Either widen the spacing
  scale to cover the half-steps the design actually uses, or soften the rule to
  colors only; right now it reads as broken everywhere.

## Infrastructure and repo

- **CI covers the Go module only** (`.github/workflows/checks.yml` runs
  `gofmt`, `go vet`, `go test` and `govulncheck`). Nothing runs
  `terraform fmt -check` or `terraform validate` on `infra/`, so formatting
  drift and a syntax error both wait until someone deploys.
- **No frontend tests at all** — 7,120 lines of JavaScript, zero. Mostly fine
  (UI regressions show themselves), but there is no harness at all should a
  piece of frontend logic ever warrant one — `upload-flow.js`'s state machine
  and `format.js` are the candidates.
- **`${HOST_PORT}` has no default** (`docker-compose.yml:9`). Unset, compose
  publishes on a random host port, contradicting README.md and CLAUDE.md's
  `http://localhost:8080`. Use `${HOST_PORT:-8080}`.
- **No Cosmos equivalent of `grant_operator_blob_access`.** DEPLOYMENT.md tells
  you to grant your own user the Cosmos data role by hand, while the storage
  side has a variable for exactly that (`variables.tf:131`).
- **Name-length validation doesn't cover the composed storage account name** —
  `st` + `birdsense` + 8-char `env` + 6-char `name_suffix` = 25, over the 24-char
  limit. A `precondition` would catch it at plan time rather than mid-apply.
- **Smaller infra notes**: `Storage Blob Data Contributor` is scoped to the whole
  account rather than the `audio` container and includes container delete
  (`storage.tf:46-50`); `max_inactive_revisions` is unset, so Container Apps
  keeps up to 100 revisions; Log Analytics has no `daily_quota_gb` while
  `requestLogger` logs every request including every tus PATCH; the 7-day blob
  soft delete means deleted originals are still billed for a week, which the
  "~250 GB steady state" figure doesn't account for.
- **`requestLogger` logs method, path and duration only** (`main.go:414-420`),
  and `h.fail` logs only method/path/err. There is no status code, no request
  id and no user, so a volunteer's "it failed" can't be correlated with a log
  line. The status code alone would pay for itself on day one.
- **Seven stale remote branches** (`detection-details`, `processing`,
  `prototype`, `recorder-map`, `text-updates`, `ui-polish`, `cosmos_emulator`)
  all merged or superseded.
