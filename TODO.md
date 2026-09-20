# Birdsense — what's left before it goes live

Last full review: `dd72866`, 2026-09-19 — update that line when the list is
swept again; individual items don't carry dates. This is a review of the whole
repo against the plan already written down in [CLAUDE.md](CLAUDE.md),
[SCHEMA.md](SCHEMA.md) and [DEPLOYMENT.md](DEPLOYMENT.md). Nothing here is a new
idea for the product: every item is either something those documents already
promise and the code doesn't do, or something that will go wrong on the first
real card.

There was no existing pre-launch checklist. The closest things are
DEPLOYMENT.md's *First checks once we have Azure access*, *First deploy* and
*Open questions that change this file*, and CLAUDE.md's *State of the code*;
all of them are folded in below.

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

1. Do the work, and leave behind something that fails if it regresses — a test,
   a Terraform `precondition`, a startup check. That check is what replaces the
   entry here; it is the reason the entry doesn't need to stay.
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

### 1.1 Retention can strand a recording's pointer forever
`backend/internal/retention/retention.go:162-164` — **[verified]**

If the blob delete succeeds but `UpdateAudioFile` fails, the loop `continue`s
without incrementing `left`. Both sibling branches do increment it. With any
other file on the card succeeding, the card still gets `audioDeletedAt`, and
`Policy.ExpiresAt` (`retention.go:61`) then returns `nil` for that card for
good — so no later sweep ever looks at it again.

**If not fixed:** that file keeps a `blobName` naming a blob that no longer
exists, with `audioDeletedAt` unset, permanently. The card page shows audio that
isn't there, and `internal/analysis` would read it as "the audio isn't in
storage". One line: `left++` in the error branch.

### 1.2 A non-analyzer failure re-runs BirdNET forever and wedges the queue
`backend/internal/analysis/analysis.go:306-330`

`retryOrFail` and the `q.attempts` cap cover analyzer crashes only. The clip
upload (`putFile`), `UpsertDetections` and the final `UpdateAudioFile` return
raw errors that bypass the counter, so `Run` just pauses and loops.

**If not fixed:** a persistent non-analyzer failure on one file — a blob 403
after a role change, a store reject — re-runs the full multi-minute BirdNET pass
over a ~300 MB file forever at 1 vCPU. The file never reaches `failed`, the card
never reaches `needs_attention`, and every card behind it waits. CLAUDE.md's
"one file can't wedge the queue" is narrower than it reads.

### 1.3 Recording timestamps silently fall back to Pacific
`backend/internal/analysis/analysis.go:510` — **[verified]**

The `namedStart` regex is
`(?:^|\D)(\d{8}_\d{6})(?:\D|$)(?:.*?\(([+-]\d{4})\))?`. For the format its own
comment documents — `Marymoor_20260723_160624(-0700).wav` — the `(?:\D|$)`
consumes the `(`, so the offset group can never match:

```
"Marymoor_20260723_160624(-0700).wav" -> ("20260723_160624", "")   <- offset lost
"20260115_220000 (+0100).wav"         -> ("20260115_220000", "+0100")
```

**If not fixed:** every such file is read as Pacific. `recordedAt`, every
`detectedAt` derived from it, and the `night` grouping are wrong by the offset
difference for any recorder not on Pacific time. The existing test
(`analysis_test.go:552`) can't catch it — its only parenthesized case is a July
date where `-0700` *is* Pacific.

### 1.4 There is no transfer checksum, though the schema promises one — **[decide]**
`backend/internal/db/models.go:184`, SCHEMA.md `audioFiles` table — **[verified]**

`audioFiles.sha256` is documented as "Hex checksum, to tell a corrupt transfer
from a corrupt card", and `checksum mismatch` is a documented `statusDetail`.
The field exists in the Go model. Nothing in the repo ever computes, stores or
verifies it — the only integrity check on a 128 GB card is byte length.

**If not fixed:** a card corrupted in transfer is indistinguishable from one
BirdNET simply couldn't read, and the documented failure reason can never
occur. The UI meanwhile tells the volunteer the card was "checked against what
was on the card" and is "safe to erase", after which the original is gone. Either
implement it (a browser-side hash per file, verified in the tus finish hook) or
drop the field and the copy — but shipping as-is means the schema lies about a
data-integrity guarantee.

### 1.5 Re-registering a card at a different size orphans stored bytes
`backend/internal/api/api.go:561-569`

If a file is already `uploaded` but the re-read card reports a different size,
the document is rewritten as fresh `AudioPending`, dropping `blobName` and
`uploadedAt`. The sibling branch two lines down deliberately *keeps* the
document for a file taken off the list, "so whatever was stored for it is still
accounted for".

**If not fixed:** the old bytes stay in storage with nothing naming them.
Retention sweeps by `blobName`, so it can never see them; only deleting the
whole card reclaims them.

### 1.6 Recorder ids are unvalidated, and card references can collide
`backend/internal/api/api.go:1145`, `backend/internal/db/models.go:282-284`

`addStation` accepts any trimmed string as the recorder id, which is both the
document id and the partition key. `UploadID` is
`"OWL-" + date + "-SR" + strings.TrimPrefix(recorderID, "SW-")`.

**If not fixed:** recorders `SW-02` and `02` produce the *same card reference*
on the same pull date — a real document collision, not just a storage-prefix
one. And Cosmos forbids `/`, `\`, `?`, `#` in an item id, so a recorder id
containing one yields ids Cosmos rejects with a raw 400, a reference that breaks
`GET /uploads/{reference}` routing, and a prefix `storage.under()` refuses — so
the card could never be deleted. A character whitelist and a length cap on
`addStation` closes all of it.

### 1.7 Deleting a real card will exceed the ingress timeout
`backend/internal/db/cosmos.go:429-442`, `backend/internal/storage/storage.go:367-397`,
called synchronously from `backend/internal/api/api.go:722-762`

For a real card (~336 files, tens of thousands of detections) the handler does
serial blob deletes (~672 round-trips plus clips) and then one `DeleteItem` per
document, 8 at a time, across two containers. The Container Apps ingress ends a
request at 240 s (DEPLOYMENT.md).

**If not fixed:** the coordinator gets a 504 and the card is still listed. The
operation is re-runnable by design so nothing is lost, but delete looks broken.
`azcosmos` v1.5.0 has `NewTransactionalBatch` (100 same-partition ops per
request), which turns 30,000 requests into ~300.

---

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

### 2.2 Cancelling the folder picker permanently wedges the upload page on Firefox and Safari
`frontend/js/card-scan.js:58-88`, `bs-upload-details.js:115-129`

`viaInput()` resolves only on `change` and has no `cancel` handler.
`showDirectoryPicker` is Chromium-only, so every Firefox and Safari volunteer
takes this path.

**If not fixed:** closing the OS picker leaves the promise pending forever:
`#busy` stays true, every button renders disabled reading "Reading the card…",
and the only recovery is a page reload. The abandoned hidden `<input>` also
stays in the document — `input.remove()` runs only inside the `change` handler,
despite the comment claiming otherwise. `<input type=file>` has fired `cancel`
since Firefox 91 / Safari 16.4.

### 2.3 Upload steps 3 and 4 hang forever if the card lookup fails
`bs-upload-progress.js:41-44`, `bs-upload-done.js:18-21`

Both attach only a fulfilment handler, and `flow.current()` re-throws anything
that isn't a 404 (`upload-flow.js:195`).

**If not fixed:** a 500 or a dropped connection on reload leaves the volunteer
staring at "Finding the card…" with no message and an unhandled rejection in the
console. `bs-upload-check.js:23-27` already does this correctly.

### 2.4 An ended session shows raw errors instead of the sign-in page
`frontend/js/api.js:15-31`, `frontend/js/session.js:23-40`

`session.load()` runs once, at `bs-app.js:52`. Nothing re-checks it, and no
caller inspects `error.status`. The cookie lasts 90 days, and roster removal or
a `BIRDSENSE_SESSION_KEY` rotation ends a session immediately.

**If not fixed:** after that, every tab renders "Couldn't load your cards: sign
in first" while the header still shows the person's name and all their tabs. One
`if (status === 401)` in `api.js`'s `request()` covers the whole app; the tus
path already does it (`upload-flow.js:385-394`).

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

### 2.7 A stuck card gives no reason
`backend/cmd/server/main.go:84-96`

`Analyzer.Check` runs once at startup; on failure it logs a warning, the queue
never starts, and it never retries. Nothing surfaces this on `/health`, on the
card, or in the admin UI.

**If not fixed:** a volunteer uploads 128 GB, the card sits in `processing`
indefinitely (retention correctly refuses to sweep it), and nobody learns why
until someone reads container logs. There is also no script, button or
documented command to restart the revision, which is the only recovery.

### 2.8 A half-filled coordinate puts a recorder in the Gulf of Guinea
`frontend/js/components/bs-admin-recorders.js:76-80`, `backend/internal/api/api.go:1242-1252` — **[verified]**

`Number("")` is `0`, and `stationProblem` rejects only `lat == 0 && lon == 0`.
Latitude `47.66` with an empty longitude is accepted as `{47.66, 0}`.

**If not fixed:** no pin appears, no warning is shown, and every detection from
that recorder carries the wrong position into BirdNET's geo filter — which
changes which species the model will report.

---

## 3. Scale and cost — this bites during the first season, not later

### 3.1 The Detections tab reads the entire detections container per request
`backend/internal/api/detections.go:56-69`, `backend/internal/db/cosmos.go:316`

The default view sends no `since` (`bs-detections.js:125-133`), so
`ListDetections` is an unbounded cross-partition `SELECT *` whose full result is
decoded into Go, tallied, sorted and then sliced to 50 rows — plus an
unconditional `ListUploads`. Every sort, filter and page change repeats it.

**If not fixed:** at the volume DEPLOYMENT.md's own cost table predicts (~2M
detection documents, ~2 GB), this is hundreds of MB of Go heap per request in a
2 GiB replica that is simultaneously running BirdNET at ~300 MB — an OOM restart
mid-analysis — and a serverless Cosmos bill for a full scan on every page view.
This is the single most likely production failure. Cheapest fix before release:
default `since` to a bounded window server-side when none is given.

### 3.2 A card's detections endpoint is unpaginated
`backend/internal/api/api.go:782`

`GET /detections/{ref}` with no `?file=` returns every detection on the card in
one body — tens of thousands of rows. Single-partition, so cheap in RU, but a
very large response. Cap it, or require `file`.

### 3.3 The public overview is an unauthenticated full scan with no cache
`backend/internal/api/overview.go:20-41`

Every landing-page hit reads every recorder, every upload and every confirmed
detection since January 1 and aggregates in Go. No rate limiting, no cache, no
Front Door or WAF in front (DEPLOYMENT.md's *Shape of it*).

**If not fixed:** one `while true; do curl; done` against the public page burns
Cosmos RUs and CPU on the single replica that is also running BirdNET. The
response's `updatedAt` is already truncated to the minute — caching it for that
minute is nearly free.

### 3.4 Clip storage is probably budgeted orders of magnitude low, and nothing caps it — **[needs a real card]** for the measurement; the per-file cap is implementable now
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
`os.MkdirTemp` before upload. Measure detections-per-file on the first real
card, then decide on a per-file cap, a higher clip threshold, or a smaller clip
format.

### 3.5 The cost table contradicts `min_replicas = 1`
DEPLOYMENT.md *Cost* vs `infra/app.tf:76` — **[verified]**

The table lists Container Apps as "low traffic, scale to zero | usually within
the monthly free grant". `min_replicas = 1` is set, and DEPLOYMENT.md explains
at length why it must be. An always-on 1 vCPU / 2 GiB replica is tens of dollars
a month and the second-largest recurring line item after blob storage. Fix the
number before anyone budgets from it.

---

## 4. Security

### 4.1 The build context ships both production secrets to ACR
`.dockerignore`, `scripts/deploy.ps1:100` — **[verified]**

`.dockerignore` excludes `.git`, `*.md`, `backend/data`, `.venv` and `test` —
but not `infra/`, `scripts/`, `*.tfvars` or `.terraform/`. `az acr build ... '.'`
packs the whole context. `deploy.ps1:57` *requires* `infra/prod.tfvars` to
exist, and that file holds `oidc_microsoft_client_secret` and `session_key` in
plaintext.

**If not fixed:** every deploy uploads both production secrets into ACR's source
storage, along with 234 MB of `infra/.terraform` (measured). Add `infra/`,
`scripts/`, `**/*.tfvars`, `**/*.tfstate*`. Note also that `*.md` only matches
root-level files — Docker patterns don't cross `/`.

### 4.2 There are no security response headers at all
`backend/cmd/server/main.go:414-420`, `backend/internal/web/web.go` — **[verified]**

No `Content-Security-Policy`, `X-Content-Type-Options`, `Referrer-Policy`,
`Strict-Transport-Security` or `frame-ancestors` on any response. (tusd sets
`nosniff` on its own replies only.)

**If not fixed:** a cookie-authenticated SPA with one-click destructive admin
actions is framable and sniffable, and there is no defence in depth behind the
SRI pins on jsDelivr. A CSP has to allow jsDelivr (Leaflet, tus-js-client),
Google Fonts and OSM tiles — worth writing once, in the existing handler chain.

### 4.3 The session key has no strength requirement
`backend/internal/api/cookies.go:24-29`, `backend/cmd/server/main.go:360`

`newKeyset` SHA-256s whatever passphrase it's given; startup only checks
non-empty, and `api.Register` has no guard of its own.

**If not fixed:** `BIRDSENSE_SESSION_KEY=owls` yields a brute-forceable HMAC
key, and the cookie value *is* the identity — forging one mints
`dana@eastsideaudubon.org` and gets admin. Require ≥32 bytes in `readAuth`, and
make `newKeyset` reject empty.

### 4.4 No upper bound on a declared file or card size
`backend/internal/api/api.go:520-545`, `backend/internal/api/tus.go:45-65` — **[verified]**

`cardList` rejects only `f.Bytes < 0`, and tusd's `Config.MaxSize` is left at 0
(unlimited).

**If not fixed:** a signed-in volunteer can register a card claiming a 10 TB
file and stream it through the container into blob storage. The `int64` sums
also overflow identically on both sides of the `bytes != wantBytes` check, so a
crafted list can store a negative `totalBytes`. Set `MaxSize` and reject
implausible per-file and per-card sizes.

### 4.5 A known advisory is reachable from the sign-in path
`backend/go.mod` — **[verified]**

`govulncheck` reports GO-2026-4945 in `go-jose/v4@v4.1.3`, reachable via
`oidc.IDTokenVerifier.Verify` (`internal/api/auth.go:421`). Fixed in v4.1.4.
`golang.org/x/crypto@v0.55.0` has three more (ssh, openpgp) that the code
doesn't call. There is no dependency scanning in the repo and no CI at all.

**If not fixed:** a known-vulnerable JOSE parser sits on the only
unauthenticated code path that processes attacker-influenced input. One
`go get`; then decide who runs `govulncheck` and when.

### 4.6 tus upload ids from the URL aren't validated before reaching the store
`backend/internal/api/tus.go:80`

Go's `ServeMux` cleans the *escaped* path, so percent-encoded traversal
survives: `HEAD /api/v1/tus/%2e%2e/secret` reaches `filestore` at
`<dir>/uploads/../secret.info`. It is **contained** — the ownership check
(`tus.go:88-96`) rejects anything whose `.info` metadata doesn't name a card the
caller can see, and only the server picks ids — so there is no known exposure.
Worth a shape check anyway, because the containment is incidental rather than
intended.

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

Terraform has never been applied against a real subscription, so all of this is
untested in the direction that matters.

### 5.1 Probe timeouts default to 1 second on a container that is CPU-saturated for hours
`infra/app.tf:174-189` — **[verified]**

Only `transport`, `port`, `path` (and liveness `initial_delay`) are set. The
azurerm 4.81 defaults are `timeout = 1`, `interval_seconds = 10`,
`failure_count_threshold = 3`. The same container runs BirdNET CPU-bound for
hours on `cpu = 1.0`, which `app.tf:86-88` says explicitly.

**If not fixed:** a health response that misses a 1-second deadline three times
in 30 s restarts the *only* replica, mid-card, plausibly in a loop — and the
readiness probe with the same defaults 503s the site during analysis. This is
the most likely way the first real card fails.

### 5.2 `/api/v1/health` can't fail
`backend/internal/api/api.go:155-157`, `infra/app.tf:174-189` — **[verified]**

It returns `{"status":"ok"}` unconditionally and is wired as the liveness,
readiness *and* startup probe.

**If not fixed:** a replica that has lost Cosmos or Blob access stays "healthy"
and serves 500s forever. Liveness should stay unconditional — a dependency blip
shouldn't restart the container — but readiness wants a cheap store ping.

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

### 5.7 There is no rollback path in the tooling
`scripts/deploy.ps1:66`

DEPLOYMENT.md says "a rollback is applying an older tag", but the script always
derives the tag from `git rev-parse --short HEAD` and takes no `-ImageTag`
parameter.

**If not fixed:** a rollback means hand-running Terraform under pressure, with
the PowerShell argument-quoting rules from README.md. One parameter.

### 5.8 The lifecycle rule tiers blobs to cool just before the app deletes them
`infra/storage.tf:94` vs `infra/variables.tf:112`

`tier_to_cool_after_days_since_modification_greater_than = 30` is hard-coded,
and `audio_retention_days` also defaults to 30. The lifecycle scan runs daily;
the app's sweep runs every 6 hours.

**If not fixed:** normal cards get tiered to cool a few hours before deletion
and incur cool's 30-day early-deletion charge — roughly a card's full month of
cool storage each time, the opposite of what the doc claims the rule is for.
Either make it `audio_retention_days` + margin, or drop the cool action; the
rule's purpose is the delete backstop anyway.

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

### 5.10 A normal deploy looks like a crash
`backend/cmd/server/main.go:153-168` — **[verified]**

`srv.Shutdown` gets 10 seconds, inside Container Apps' 30 s default grace
period. A 50 MB tus PATCH on a home connection routinely exceeds that, so
Shutdown returns a deadline error and main exits 1.

**If not fixed:** every Terraform-driven revision deploy during an upload looks
like a crash to Container Apps and to whoever reads the logs. The upload itself
is fine — tus resumes — but the signal is wrong. Widen the window (it should be
the larger of the two, not the smaller) or treat a Shutdown deadline as a normal
outcome logged at Warn.

### 5.11 Every deploy rebuilds the BirdNET stage from scratch
`scripts/deploy.ps1:100`, `Dockerfile:21-37`

`az acr build` runs on a fresh agent with no `--cache-from` and no registry
cache, so every deploy pip-installs the pinned requirements and re-downloads
~90 MB of models from Zenodo.

**If not fixed:** multi-minute deploys, and a deploy — including an emergency
rollback — that can fail because PyPI or Zenodo is having a bad day. Compounded
by the 234 MB context upload in 4.1.

### 5.12 DEPLOYMENT.md's own pre-flight checks are still outstanding — **[needs Azure]**

*First checks once we have Azure access* lists three, and they remain the right
list: (1) run the app locally against a non-production storage account with your
own Entra user, upload a few large `.wav` files, and confirm pause/resume
carries on from the last 50 MB chunk rather than restarting; (2) deploy and send
one real card end to end, watching ingress for 499/504 and the replica's CPU,
memory and restarts; (3) expect the startup container check to fail while role
assignments propagate. Item (2) is also where 3.4 (clips per card) and 3.1
(detections volume) get their first real measurement.

---

## 6. Documents that are now wrong

These matter because the docs are how the next person — or the same person in
six months — decides what is true.

### 6.1 The documented dev quick-start serves no frontend
`backend/cmd/server/main.go:218`, README.md *Quick start*, CLAUDE.md *Running it* — **[verified]**

`BIRDSENSE_STATIC_DIR` defaults to `frontend`, but both docs say to run
`cd backend && BIRDSENSE_DB=local go run ./cmd/server`. From `backend/` that
resolves to `backend/frontend`, which doesn't exist. `curl localhost:8080/`
returns `404 page not found`; the API works. The analyzer defaults in the same
struct correctly use `../`.

**If not fixed:** the first thing a new contributor does, exactly as documented,
appears to be a completely broken app. Change the default to `../frontend`.

### 6.2 DEPLOYMENT.md and CLAUDE.md disagree about what has run in Azure — **[decide]** which is true
DEPLOYMENT.md `:15-19`, `:385`, `:456` vs CLAUDE.md *State of the code*

DEPLOYMENT.md says "nothing here is provisioned yet; the Terraform has never
been applied… neither has run against Azure". CLAUDE.md says both backends "have
run in Azure against a real card". The evidence favours CLAUDE.md:
`infra/.terraform` exists, and `storage.go:298-306` documents a tusd failure
that "Azurite never saw". This matters because DEPLOYMENT.md's checklist is
written as pre-flight — as it stands, a reader can't tell which checks are still
outstanding.

### 6.3 Smaller drift, each a one-line fix

- `backend/internal/db/cosmos.go:18-20` still carries an "UNTESTED" banner
  saying the backend has never run against Azure or the emulator.
- DEPLOYMENT.md's variable summary says only `image_tag` and `bootstrap_admin`
  are required with no default. Also required: `oidc_microsoft_client_id`,
  `oidc_microsoft_client_secret`, `session_key`. `subscription_id`,
  `public_url` and `oidc_microsoft_tenant` are missing from the list entirely.
  This is the paragraph a first-time deployer reads.
- `scripts/deploy.sh` doesn't exist; it's `deploy.ps1`. Four references:
  `infra/registry.tf:1`, `infra/variables.tf:36`, `infra/outputs.tf:12`,
  `infra/app.tf:82`.
- The analysis backoff never grows — `analysis.go:114-115` always sleeps the
  constant `retryDelay` — while `analysis.go:62`, SCHEMA.md and CLAUDE.md all
  say "30 s, then 60 s" / "30 s apart and growing".
- `card-scan.js:29-30` says the directory handle is kept "so a resume can
  re-read them without asking again", but the handle is dropped. A Chromium
  resume asks for the folder again exactly like the input fallback.
- `infra/app.tf:71-75` presents `max_replicas = 1` as absolute. During a
  revision swap — i.e. every deploy — old and new revisions briefly overlap, so
  two processes can run the queue and hold independent in-memory tus locks.
  Deterministic detection ids make double analysis harmless; the comment should
  say so, and it's an argument for not deploying mid-upload.

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

## Duplication worth collapsing

- **`storage.segment` vs `api.storagePrefix`** (`storage.go:94-106`,
  `tus.go:196-204`). Identical character maps, except `segment` has an all-dots
  guard the copy dropped. `ClipName` uses one; `DeleteAll`, `under` and tus ids
  use the other. They agree on every real reference today, but if they diverge,
  deleting a card silently leaves either its audio or its clips behind —
  `DeleteAll` returns no error for a prefix matching nothing. Export
  `storage.Segment` and call it from both.
- **`bs-my-uploads` and `bs-admin-uploads` are ~90% the same component** — same
  filter row, same nine-column table, same chip/notes/counts cells, same
  sub-row error pattern, same `COLUMNS = 9`. They differ by one column and the
  trailing action: 476 lines that could be one table plus two column tables.
- **The status-filter pill is copy-pasted character for character into four
  components** (`bs-detections.js:162`, `bs-admin-uploads.js:118`,
  `bs-my-uploads.js:97`, `bs-admin-upload-detail.js:151`), plus a `.tally` rule
  in three. This is what `shared-styles.js` exists for.
- **`.visually-hidden` is defined twice**, identically
  (`bs-admin-people.js:201-208`, `bs-admin-uploads.js:143-150`).
- **`randomKey` and `randomToken` are byte-identical** (`cookies.go:33-45`).

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
- **`var(--bs-font-body, inherit)` names a token that doesn't exist**
  (`bs-admin-upload-detail.js:146`); the token is `--bs-font`. The fallback
  hides it, so the text silently inherits Newsreader.
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

- **No CI.** There is no `.github/`; `gofmt`, `go vet`, `go test` and
  `govulncheck` are manual. A single workflow running the command CLAUDE.md
  already documents would have caught 4.5.
- **No frontend tests at all** — 7,120 lines of JavaScript, zero.
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
