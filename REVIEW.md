# Reviewing detections — design

How detections get from BirdNET to a verdict: a table of review rules routes
each detection when it is stored, so the common birds are accepted without
anyone listening; what is left is grouped into *bouts*; and reviewers hear
bouts, not detections, two to agree, in a Review tab built for speed.
Disagreements go to a birding expert. Not built yet: **Milestones** is the
order, and the **Status** table at the end is where to resume. Where this and
the code disagree, the code is what runs -- fix whichever is wrong.

## Problem

Today every detection is `unreviewed` until someone opens its page and
presses Confirm or Discard (`reviewDetection`, `internal/api/api.go`), and
only `confirmed` ones count publicly. That can't scale to what a recorder
hears. Measured on Marymoor (SW-01), Oct 1–15 2026, which is almost exactly
one card (`OWL-20261016-SR01`), through the MCP tools:

| Two weeks of one recorder | |
|---|---|
| BirdNET detections | **32,070** (~2,300 a day), 130 species |
| The six commonest species (crow, Canada Goose, Golden-crowned Kinglet, House Finch, two sparrows) | 74% of them |
| Owls (Great Horned 360, Northern Pygmy 17, Saw-whet 13, Short-eared 8, …) | 485 |
| At confidence ≥ 0.5 | 13,971 detections, 96 species |

The design this started from routed each detection by its species' eBird
share at the park and aimed at "200–300 clips a card, each listened to".
Taking the 25 commonest species as expected and every owl as always
reviewed:

| What goes to a reviewer | No floor | Confidence ≥ 0.5 |
|---|---|---|
| Detections | ~2,700 | **~830** |
| Species-days (the coarsest a bout could be) | 663 | **331** |
| Great Horned Owl detections | 360 | 102 |
| A 2% spot-check of what was accepted | ~600 | ~260 |

So a detection is the wrong unit, a 2% spot-check is the wrong shape (it is
the whole budget by itself, and almost all crows), and a confidence floor is
the biggest single lever. Three listeners per clip multiplies whatever is
left.

Two things worth knowing from the same data:
- Many of the "unexpected" species with lots of detections are BirdNET
  confusing something common: Mountain Chickadee (176), Snow Goose (140),
  Clark's Nutcracker. They are where the queue floods, and where a floor and
  a bout do the most.
- Great Horned Owl detections are spread through the middle of the day as
  well as the night, mostly at 0.25–0.45 and often minutes apart. Bouts of
  them will often be one detection long. Expect a few hundred bouts per
  two-week card at first, not fifty, until reviews say which species can be
  accepted or need a higher floor.

## Decisions

- **Accepted detections count.** A detection the rules accept counts on the
  public page and in the MCP summaries exactly as a confirmed one does. It
  has a status of its own, `accepted`, so nothing confuses it with a person
  having listened.
- **The unit of review is a bout**: one species on one card, its detections
  no more than a gap apart. A reviewer hears its best clips and the verdict
  is the bout's, copied to every detection in it.
- **Two listeners must agree** on a bout routed for review; **one** confirm
  settles a spot-check. Up to three listen; a split goes to the birding
  expert.
- **The rules are configured by hand**, on a page, and can be imported as
  JSON -- the first table is a draft generated from the program's own data.
  There is no eBird share: a species is `accept` or `review`, with a
  confidence floor. eBird import is later work.
- **Every clip is still stored**, accepted or not.

## Review rules

One document, `reviewRules`, in a new `settings` container (partition key
`/id`). It is small -- a row per species the program has heard, a few
hundred at most -- so one document is one point read for a worker and one
replace-if-unchanged for an edit.

```json
{
  "id": "reviewRules",
  "version": 7,
  "defaults": { "action": "review", "minConfidence": 0.5 },
  "boutGapMinutes": 15,
  "spotChecksPerSpecies": 3,
  "species": [
    { "scientificName": "Corvus brachyrhynchos", "commonName": "American Crow",
      "action": "accept", "minConfidence": 0.5 },
    { "scientificName": "Bubo virginianus", "commonName": "Great Horned Owl",
      "action": "review", "minConfidence": 0.3, "note": "Always reviewed: owls" },
    { "scientificName": "Strix varia", "commonName": "Barred Owl",
      "action": "review", "minConfidence": 0.3, "source": "review",
      "addedFrom": "OWL-20261016-SR01" }
  ],
  "updatedBy": "usr_…", "updatedAt": "2026-10-28T17:02:11Z"
}
```

- `action` is `accept` or `review`. `minConfidence` is the floor: below it a
  detection is neither counted nor queued (*Routing*).
- `defaults` applies to a species with no row.
- `version` goes up with every save. It is what tells the preparation loop
  that cards need routing again.
- `source: "review"` marks a row added because reviewers confirmed a
  species the table didn't have (*How a bout resolves*); a person edits it
  like any other.
- Species are matched by scientific name, which is how BirdNET's labels,
  corrections and the public page already key them.

**No rules document means no routing.** Until the first table is saved,
every detection stays `unreviewed` with no `route`, as today. Turning the
feature on is saving a table, and it can't route a program's backlog into
review by accident.

Who edits it: admins and birding experts. Reviewers can read it.

*Not in it yet:* months. A species expected in winter and unusual in summer
is one row with one action; when that matters, a row gains an optional list
of months its action applies in, and review outside them.

## Routing

`review.Route(d, rules)` is a pure function of a detection and the rules
(new package `internal/review`, which `internal/analysis` and
`internal/api` both import):

1. Find the species' row, or use `defaults`.
2. Confidence below its `minConfidence`: **`ignore`**.
3. Otherwise the row's action: **`accept`** or **`review`**.

It is stored on the detection as `route`. A detection routed `accept` is
stored `accepted`; `review` and `ignore` are stored `unreviewed`. Perch's
detections are never routed: they are a second opinion with their own
scores, kept for comparison, and have no `route`.

Routing happens in two places, with the same function:

- **In the worker, as a file's detections are stored** (`analyzeFile`,
  `internal/analysis`). It reads the rules once per file -- a point read --
  and costs no extra writes. It is also the only moment the original
  recording is certainly there, which is what *Clips with context* needs.
- **In the preparation loop, for a whole card** (*Preparing a card*), when
  the rules change or the card was analyzed before them. Only detections no
  person has reviewed, and not in a bout that has a verdict, are routed
  again; only those whose route changes are written.

## Statuses

| `reviewStatus` | Means | Counts publicly |
|---|---|---|
| `unreviewed` | BirdNET's word alone: routed `review` and not settled yet, routed `ignore`, or stored before routing | No |
| `accepted` | The rules accepted it, and no person has said otherwise | Yes |
| `confirmed` | People confirmed its bout (or, before bouts, one person confirmed it) | Yes |
| `rejected` | People rejected its bout, or its spot-check | No |

`route` says why an `unreviewed` one is: waiting for review, or below the
floor. A spot-check that is rejected turns `accepted` detections `rejected`,
so a rule that is wrong for a card stops counting there at once.

The existing `review` object stays, and keeps meaning *how this detection
was settled by people*: for a bout's verdict it holds the bout's id, the
names of who agreed, when, and any corrected species
(`correctedScientificName`, which becomes reachable at last). Detections
reviewed one at a time before this keep the `review` they have, and are left
out of bouts.

## Bouts

A bout is one species, heard on one card, in detections no more than
`boutGapMinutes` apart (by `detectedAt`, so a bout crosses file
boundaries). Bouts live in a new `bouts` container, partitioned by
`/uploadId` like everything else per card.

```json
{
  "id": "bout_<hash of card, species, first detection id>",
  "uploadId": "OWL-20261016-SR01",
  "recorderId": "SW-01",
  "kind": "review",
  "scientificName": "Bubo virginianus", "commonName": "Great Horned Owl",
  "firstHeardAt": "…", "lastHeardAt": "…",
  "detectionIds": ["det_…", "…"],
  "best": ["det_…", "det_…", "det_…"],
  "maxConfidence": 0.98,
  "status": "open",
  "reviews": [
    { "userId": "usr_…", "userName": "Ellen Park", "verdict": "confirm",
      "scientificName": "Bubo virginianus", "at": "…" }
  ],
  "resolution": null
}
```

- `kind` is `review` (its detections were routed `review`) or `spotcheck`
  (a sample of what was accepted).
- `best` are its three most confident detections: the clips a reviewer is
  played, most confident first. A reviewer can open any detection in it.
- `status` is `open`, `confirmed`, `rejected` or `disputed`.
- `resolution` is the birding expert's verdict on a disputed bout, or an
  expert's or admin's override (*Milestones*, M5).
- Each detection in a bout carries `boutId`.

**Spot-checks.** For each species routed `accept` on a card, the
preparation loop bouts its accepted detections and picks
`spotChecksPerSpecies` of those bouts -- by a hash of the bout's id, so
preparing the card again picks the same ones. Only the picked bouts are
stored; the rest of an accepted species' detections have no bout.

## How a bout resolves

A verdict is `confirm` (optionally of another species: "it's a Barred Owl"),
`reject` or `unsure`. A reviewer gives one verdict per bout and may change it
until the bout resolves. A birding expert listening to an ordinary bout is
one of its listeners.

- Two verdicts *match* when both are `reject`, or both are `confirm` of the
  same species. `unsure` matches nothing.
- A `review` bout resolves when **two** verdicts match.
- A `spotcheck` bout resolves on **one** `confirm` of its own species.
  Anything else on it turns it into a two-to-match bout.
- **Three** verdicts without a resolution make it `disputed`, as does three
  `unsure`. It then waits for a birding expert, whose verdict is final.

On resolution the bout's verdict is copied to its detections:
`reviewStatus`, and `review` with the bout's id, who agreed, and any
corrected species. A bout of 38 detections is 38 writes, once. A confirmed
species that has no row in the rules is added, `action: review`,
`source: review`, so the table lists it from then on.

**The verdicts are blind.** The Review tab's API doesn't show a bout's other
verdicts to someone until they have given their own (an expert or admin sees
them always). That is the Review tab's rule, not the whole app's: the
Detections tab and a detection's page show a detection's status to every
reviewer, as they do now. Hiding it there too would cost far more than it
buys from volunteers who are trying.

## Preparing a card

A background loop in the web app -- the Deleter's shape (`internal/deletion`):
woken when a card's BirdNET analysis finishes and when the rules are saved,
with a backstop pass every five minutes and a growing delay after a failed
one. It prepares a card when:

- BirdNET is done with every file (`filesAnalyzed + filesFailed ==
  fileCount`) -- Perch can still be running; a card waiting for Perch is
  still `processing`, and review needn't wait for it -- and
- the card isn't `deleting`, and
- its `review.rulesVersion` isn't the current rules' version.

Preparing a card, all of it in the card's own partition:

1. Read its detections a page at a time (`EachDetection`), routing again
   any that no person has settled and that aren't in a bout with a verdict;
   write those whose route changed.
2. Bout the detections routed `review` that aren't in a bout with a verdict;
   pick the spot-check bouts. Write the bouts; delete stored bouts that have
   no verdict and are no longer produced; set `boutId` on detections whose
   bout changed.
3. Write the card's summary (*What the landing page reads*) and its review
   counts on the upload, `review: {rulesVersion, preparedAt, bouts,
   resolved, disputed}`, which the card picker shows.

Every step is idempotent and bout ids are deterministic, so a pass cut part
way is finished by the next. A card's first preparation, after the fact,
writes most of its detections once (~30k on a two-week card); after that a
pass writes only what changed.

It runs in the web app rather than the job because it needs only the store,
and because the job's workers are not always there.

## Clips with context

A reviewer's "± 10 s context" can't be cut on demand: the web image is
Alpine with no Python to run `clip.py`, and originals are deleted a month
after a card arrives. So the worker cuts a detection routed `review` with
`contextPadSec` (10 s) either side instead of `clipPadSec` (1 s), still
capped by `maxClipSec` around its best window (`internal/analysis/merge.go`).
At a 0.5 floor that is ~3% of detections (~830 of a two-week card's 32,000),
at ~4–5 times the size: about a tenth more clip storage. The player plays the detection's own span by default and the
whole clip for context; the spectrogram marks the span (the clip's and the
detection's `startSec` already say where it sits).

A species that only moves to `review` after its card was analyzed keeps its
short clip, and its context button is off.

## What the landing page reads

`publicOverview` reads every confirmed detection of the year with
`ListDetections`, under `db.MaxDetectionScan` (250,000). Counting accepted
detections too is ~14,000 per recorder per two weeks at a 0.5 floor, so one
recorder would reach the ceiling in about four months, and the landing page
would fail; streaming them instead (`EachDetection`) has no ceiling but
reads ~350,000 documents a minute per recorder-year.

So the landing page reads **per-card summaries** instead: one document per
card, in a new `cardSummaries` container (partition key `/id`, the card's
reference), holding its counted detections -- `accepted` and `confirmed`,
BirdNET's -- tallied per Pacific date and species. That is everything the
overview shows: per species its count, days and stations, and the year's
total. A summary is a few hundred rows, and the overview reads one per card.

- The preparation loop writes it whole from the card's detections.
- A bout resolving adjusts it by what changed (detections moving into or out
  of a counted status), with replace-if-unchanged; the next preparation pass
  rewrites it whole, so a missed adjustment doesn't last.
- A card being deleted has its summary deleted with it, and the overview
  skips `deleting` cards as it does now.

The figure on the landing page says "confirmed owl detections" and counts
every species's confirmed detections; with accepted ones it is mostly not
owls. Its wording changes with this.

The MCP tools stream (`EachDetection`), so they need no summary: their
totals gain `accepted`, `find_detections` takes it as a status, and the
server instructions (`mcp.go`) describe it.

## API

```
GET  /api/v1/review/rules                         the rules (a reviewer)
PUT  /api/v1/review/rules                         save them, with the version read (an admin or birding expert; 409 on a stale version)
GET  /api/v1/review/cards                         cards with bouts, and their progress
GET  /api/v1/review/cards/{ref}/bouts             the card's bouts, the caller's verdicts on them, and what is left for them
GET  /api/v1/review/cards/{ref}/bouts/{id}        one bout, with its detections; others' verdicts once the caller has given one
PUT  /api/v1/review/cards/{ref}/bouts/{id}/verdict    {verdict, scientificName?}: give or change one's own
GET  /api/v1/review/disagreements                 disputed bouts on every card (a birding expert)
PUT  /api/v1/review/cards/{ref}/bouts/{id}/resolution {verdict, scientificName?}: the final say (a birding expert)
```

All of it is for a reviewer unless it says otherwise (`requireRole`). The
verdict PUT is an `UpdateBout` with a mutate func, so two reviewers saving
at once, a fourth verdict, or a second from one person are settled by
replace-if-unchanged; resolving copies the verdict to the detections after
the bout is written, and a failure part way is finished by the next
preparation pass, which copies a resolved bout's verdict to any detection
that doesn't carry it yet.

`disagreements` is cross-partition, and small: only disputed bouts.

## Screens

- **Review** (`/app/review`): the cards with bouts, each with its recorder,
  recording dates and progress (`review.bouts`, `resolved`, `disputed`).
- **A card's review** (`/app/review/{ref}`, `?bout=` for the open one): the
  queue down the side, grouped by species -- bouts routed `review` first,
  then spot-checks in a group of their own (this replaces the design's
  "Exclude expected birds" switch: what was accepted is only ever in the
  queue as spot-checks). The bout's best clip on `<bs-spectrogram>`, its
  other detections a click away; ± 10 s context and 0.5× speed
  (`playbackRate`); Confirm, Reject and Unsure, and "another species" on
  Confirm; Save & next. After a verdict, the other listeners' verdicts.
  Keys: C, R, U, space, ← →.
- **Disagreements** (`/app/disagreements`), for a birding expert: disputed
  bouts, everyone's verdicts, and the final say.
- **Review rules** (`/app/review-rules`), for an admin or birding expert:
  a row per species with its action and floor, and beside each the
  program's own detections of it over the last four weeks -- count, mean
  confidence, and the spot-check rejection rate once there is one -- so the
  choice is made against the data. JSON import and export.

`<bs-app-page>`'s tab table gets Review (for a reviewer); Disagreements and
Review rules sit under it for those who have them. A detection's own page
keeps its clip and spectrogram and links to its bout; its Confirm and
Discard become an override for a birding expert or admin.

## Why it stays correct

**Analyzing a file again doesn't undo a review.** `UpsertDetections` writes
whole documents, so today a re-run -- a worker dying after storing a file's
detections and before marking it `analyzed` -- would replace any review
given in between. The worker reads the file's existing detections first and
carries `reviewStatus`, `review`, `route` and `boutId` over to any that a
person has settled or that are in a bout (M0).

**A rules change never touches what people have said.** Routing again skips
detections with a `review` and detections in a bout with a verdict; bouts
with a verdict are never deleted or rebuilt. A species moving from `review`
to `accept` takes its untouched bouts with it, and from `accept` to
`review` brings its accepted detections into bouts.

**Deleting a card takes its bouts and summary.** `DeleteUpload` deletes the
card's `bouts` partition and its summary before the upload, which still goes
last. The preparation loop skips a `deleting` card, and a verdict on one is
a 404, as tus is.

**Retention is untouched.** Preparing a card reads only documents; context
is cut while the original is certainly there.

## Gaps accepted

- **A bout's verdict stands for every detection in it.** A confirmed bout of
  38 Great Horned Owl detections is 38 confirmed detections, though people
  heard three of them. That is the point of a bout; the gap is what keeps
  one from swallowing unrelated calls.
- **Blind only in the Review tab** (*How a bout resolves*).
- **A late move to `review` has no context clip** (*Clips with context*).
- **An accepted species is checked a few bouts per card**, not in
  proportion to how much of it there is. A bad rule shows in its spot-check
  rejection rate, on the rules page, not in a count.

## Measure before tuning

- **The gap.** Before fixing `boutGapMinutes`, count a real card's bouts at
  5, 15, 30 and 60 minutes per species (a small `cmd/` reading one card's
  detections). The answer to pick is the gap past which bouts stop getting
  fewer much faster.
- **The floor.** The default 0.5 is from the table above. Per-species floors
  come from what reviewers reject: the rules page's rejection rate is the
  measurement.
- **The first table.** Generated from the Marymoor cards and imported; then
  set by hand as reviews come in.

## Rollback

An older image doesn't know `accepted`, `route`, bouts or the summaries. It
shows `accepted` as a raw neutral chip, counts only `confirmed` on the
landing page, and leaves the new containers alone; bouts stay where they
are until the newer image is back. ROLLBACK.md says so once M0 ships.

## Milestones

Each one ships on its own.

**M0 — Groundwork.**
- `UpsertDetections`' callers keep what people have said (*Why it stays
  correct*), tested on both backends.
- `accepted` everywhere detections are read: the Detections tab's status
  filter and chip, the MCP tools and their instructions.
- `cardSummaries`, written from a card's counted detections; the landing
  page reads them instead of a year of detections, and its wording follows.
  (A one-off pass writes them for cards analyzed before; until M1 that is a
  count of `confirmed` alone.)
- `settings`, `bouts` and `cardSummaries` containers in `infra/` and
  DEPLOYMENT.md; SCHEMA.md and ROLLBACK.md.

**M1 — Rules and routing.** Common birds count publicly; nothing is reviewed
differently yet.
- `internal/review`: the rules, `Route`.
- The worker routes, stores `route` and `accepted`, and cuts context clips.
- The preparation loop, routing only, which also routes the cards analyzed
  before.
- `GET`/`PUT /review/rules` and the Review rules page, with the program's
  counts beside each row and JSON import and export.
- The first table: drafted from the Marymoor data, imported through the
  page.

**M2 — Bouts.**
- Bouting and spot-checks in the preparation loop; `boutId`; the card's
  review counts.
- The gap measured on a real card (*Measure before tuning*).

**M3 — The review API.** Verdicts, resolution, copying to detections and the
summary, blind verdicts, adding confirmed species to the rules.

**M4 — The Review tab.** The card picker and a card's review: queue, player
with context and slow speed, verdicts, keys, Save & next, other listeners'
verdicts, progress.

**M5 — Disagreements and overrides.** The Disagreements page and the expert's
final say; a detection's page links to its bout, and its Confirm and Discard
become an expert's or admin's override.

**Later.** Closing a card when every bout is resolved (TODO.md 2.6, which
needs this first); the spot-check rejection rate on the rules page (M1 has
the column, M3 the data); months per species; eBird import.

## Status

| Step | State |
|---|---|
| This design | Written |
| M0: re-runs keep reviews; `accepted` read everywhere; card summaries and the landing page; containers, SCHEMA.md, DEPLOYMENT.md, ROLLBACK.md | Not started |
| M1: `internal/review`; routing and context clips in the worker; the preparation loop (routing); rules API and page; the first table | Not started |
| M2: bouts and spot-checks; the card's review counts; the gap measured | Not started |
| M3: the review API | Not started |
| M4: the Review tab | Not started |
| M5: disagreements and overrides | Not started |
