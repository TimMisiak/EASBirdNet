# Reviewing detections — design

How detections get from BirdNET to a verdict: a table of review rules routes
each detection when it is stored, so the common birds are accepted without
anyone listening; what is left is grouped into *bouts*; and reviewers hear
bouts, not detections, two to agree, in a Review tab built for speed.
Disagreements go to a birding expert. Being built in milestones: M0 to M5
are built; **Milestones** is the order, and the **Status** table at the end is
where to resume. Where this and
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
  corrections and the public page already key them. The rules page adds or
  changes a row in a panel of its own, which looks either name up in
  BirdNET's own list (`frontend/data/birdnet-species.json`) and fills in the
  other, so a row names a species as BirdNET spells it; a name the list
  doesn't have is added as typed, with a warning that it will route nothing.
  The row goes into the table marked as new or changed -- as does any row
  edited in the table, or taken back to the default -- until Save.

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
floor. A spot-check that is rejected turns its `accepted` detections
`rejected`, so what a wrong rule accepted in that bout stops counting at
once; the rest of the species on the card is left as the rule has it, and
the rejection shows on the rules page (*Gaps accepted*).

The existing `review` object stays, and keeps meaning *how this detection
was settled by people*: for a bout's verdict it holds the bout's id, the
names of who agreed, when, and any corrected species
(`correctedScientificName`, which becomes reachable at last). Detections
reviewed one at a time before this keep the `review` they have, and are left
out of bouts. A detection in a bout that someone reviews on its own page
keeps that review when its bout settles; undoing it puts the detection back
to its bout's verdict, or with none to what its route says.

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
- A resolved or disputed bout takes no new verdict and no change to one --
  on a disputed one, the listeners have each seen the others' verdicts, so
  a change then would be a vote, not a hearing. Giving the verdict one
  already gave is harmless, which is what lets a client retry.

On resolution the bout's verdict is copied to its detections:
`reviewStatus`, and `review` with the bout's id, who agreed, and any
corrected species. A bout of 38 detections is 38 writes, once. A confirmed
species that has no row in the rules is added, `action: review` at the
default floor, `source: review`, so the table lists it from then on.

Adding a row is a new version of the rules, written by no person. Two
things follow. A Review rules page open at the version before would have
its save refused, losing its edits, so the rules carry `savedVersion`, the
version a person last saved: a save read at that version or later goes
over the added versions and keeps their rows. And every card is prepared
again under the new version, as for any save; with the defaults at
`review`, the new row routes exactly as they did, so that pass writes
nothing but reads every card (*Gaps accepted*).

**The verdicts are blind.** The Review tab's API doesn't show a bout's other
verdicts to someone until they have given their own (an expert or admin sees
them always). That is the Review tab's rule, not the whole app's: the
Detections tab and a detection's page show a detection's status to every
reviewer, as they do now. Hiding it there too would cost far more than it
buys from volunteers who are trying.

## Preparing a card

A background loop in the web app -- the Deleter's shape (`internal/deletion`),
`review.Preparer`: it runs at startup, is woken when the rules are saved, and
otherwise looks every five minutes, with a growing delay after a failed pass.
Nothing wakes it when a card's BirdNET analysis finishes -- that happens in
the analysis job, another process -- so a new card waits up to five minutes
for its preparation; its detections were routed as they were stored, so what
waits is only the summary's rebuild (and, from M2, its bouts). It prepares a
card when:

- BirdNET is done with every file (`filesAnalyzed + filesFailed ==
  fileCount`) -- Perch can still be running; a card waiting for Perch is
  still `processing`, and review needn't wait for it -- and
- the card isn't `deleting`, and
- its `review.rulesVersion` isn't the current rules' version, or its
  `review.preparation` is older than `review.Preparation` -- a card
  prepared before a step was added to preparing (bouts, in M2) is prepared
  again for it, though the rules haven't changed.

Preparing a card, all of it in the card's own partition:

1. Read its detections a page at a time (`EachDetection`), routing again
   any that no person has settled and that aren't in a bout with a verdict;
   write those whose route changed, and give any detection in a resolved
   bout that doesn't carry its verdict that verdict (a resolution cut
   short).
2. Bout the detections routed `review` that aren't in a bout with a verdict;
   pick the spot-check bouts. Write the bouts; delete stored bouts that have
   no verdict and are no longer produced; set `boutId` on detections whose
   bout changed.
3. Add to the rules any species a resolved bout confirmed that they lack.
   Write the card's summary (*What the landing page reads*) and its review
   counts on the upload, `review: {rulesVersion, preparation, preparedAt,
   bouts, resolved, disputed}`, which the card picker shows -- counted from
   the bouts as they are by then, so a verdict given during the pass counts.

A bout given a verdict between step 2's read and its write is left as it
is, and the pass stops for that card without recording it as prepared, so
the next pass prepares it again with that bout kept.

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

- It is built whole (`review.Build`) from the card's detections: by the
  landing page the first time it meets a card without one (M0), and by every
  preparation pass (M1).
- Every write that changes what counts adjusts it (`review.Adjust`) by the
  difference, with replace-if-unchanged: a review on a detection's page (M0),
  the worker storing `accepted` detections (M1), a bout resolving (M3). A
  card with no summary is left without one, since the build that makes it
  reads the write too. An adjustment can be lost to a build running at the
  same moment; the next build puts it right.
- A card being deleted has its summary deleted with it, and the overview
  skips `deleting` cards as it does now.

Counting by date makes the landing page's window the last `days` Pacific
dates, today's included, rather than the last `days` × 24 hours.

The figure on the landing page said "confirmed owl detections" and counted
every species's confirmed detections; it says "confirmed detections this
year" now, and once accepted ones count (M1) it and the sentence above it
("trained volunteers confirm what the model heard before it appears here")
have to stop saying everything was heard.

The MCP tools stream (`EachDetection`), so they need no summary: their
totals gain `accepted`, `find_detections` takes it as a status, and the
server instructions (`mcp.go`) describe it.

## API

```
GET  /api/v1/review/rules                         the rules (a reviewer)
PUT  /api/v1/review/rules                         save them, with the version read (an admin or birding expert; 409 on a stale version)
GET  /api/v1/review/species?days=                 what was heard lately, species by species in confidence bands (a reviewer)
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
that doesn't carry it yet and adds any confirmed species the rules lack --
the verdict sets the card's `review.preparation` to 0 so that the pass
comes, or, failing even that, answers an error the client retries.

`disagreements` is cross-partition, and small: only disputed bouts. A
`resolution` may be given on any bout, not only a disputed one: on an open
or settled bout it is the expert's override, and a later one replaces it,
its verdict reaching the bout's detections in place of the last.

## Screens

- **Review** (`/app/review`): the cards with bouts, each with its recorder,
  recording dates and progress (`review.bouts`, `resolved`, `disputed`).
- **A card's review** (`/app/review/{ref}`, `?bout=` for the open one): the
  queue down the side, grouped by species -- bouts routed `review` first,
  then spot-checks in a group of their own (this replaces the design's
  "Exclude expected birds" switch: what was accepted is only ever in the
  queue as spot-checks). The bout's best clip on `<bs-spectrogram>`, its
  other two best a button away and the rest of its detections in a list;
  Play plays the detection's own span, with the rest of the clip dimmed,
  and ± 10 s context plays the whole clip (off where the clip was cut
  without it); half speed (`playbackRate`). Confirm, Reject and Unsure, and
  "another species" on Confirm -- picked from the rules' species and the
  card's, or written as "Common name (Scientific name)". A verdict saves at
  once, rather than on a Save & next: the other listeners' verdicts then
  show, and Next (or Enter) goes on to the next bout waiting for the
  reviewer. Keys: C, R, U, space, ← →, Enter.
- **Disagreements** (`/app/disagreements`), for a birding expert: disputed
  bouts on every card, with everyone's verdicts. Each opens in its card's
  review, where the expert hears it; the final say is a panel there, open
  on a disputed bout and a click away on any other, with no keys, so an
  expert hearing ordinary bouts as a listener can't give it by accident.
- **Review rules** (`/app/review-rules`), for an admin or birding expert:
  every species heard in the last four weeks and every species with a rule,
  in one table, each with its action and floor, and beside it what those
  four weeks come to under the rule as drafted -- counted, to review, below
  the floor -- and how many reviewers confirmed and rejected; the totals of
  all of them sit above the table, so a change shows its effect before it is
  saved. The spot-check rejection rate joins them once there is one (M3).
  JSON import and export, and with no rules yet, the suggested table
  (`frontend/data/suggested-review-rules.json`) to start from.

`<bs-app-page>`'s tab table gets Review (for a reviewer); Disagreements and
Review rules sit under it for those who have them. A detection's own page
keeps its clip and spectrogram and links to its bout; its Confirm and
Discard become an override for a birding expert or admin, and Undo shows
only on an override. A reviewer sees where the detection stands, and why --
in a bout, or not sent to reviewers at all.

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
- **A confirmed species the rules lack prepares every card again.** The row
  it adds is a new version of the rules, and the preparer reads every card
  under it, though a row matching the defaults changes no route. That is a
  few hundred thousand reads of a season's cards per new species, which
  come early and then rarely. *Revisit when* the cards outgrow it: then the
  preparer should compare what routes, not the version.

## Measure before tuning

- **The gap.** `cmd/bouts` routes and bouts a `detections_export` CSV
  exactly as the preparer would, at several gaps. Measured on the Marymoor
  card (`OWL-20261016-SR01`, 32,460 detections) under the suggested table,
  which routes 797 to review and accepts 13,491:

  | Gap | Review bouts | Of one detection | Largest | Spot-checks (largest) | Clips to hear |
  |---|---|---|---|---|---|
  | 2 min | 662 | 579 | 8 | 113 | 942 |
  | 5 min | 636 | 540 | 8 | 113 (65) | 951 |
  | 15 min | 578 | 465 | 10 | 113 (90) | 934 |
  | 30 min | 532 | 414 | 12 | 113 (28) | 907 |
  | 60 min | 475 | 348 | 23 | 113 (461) | 868 |
  | 2 h | 402 | 268 | 24 | 113 | 837 |

  There is no knee: each step takes off 4–15%, and the gap hardly moves
  what a reviewer hears (the best three clips of each bout), because what
  goes to review is mostly scattered single detections. Great Horned Owl is
  most of the difference (160 bouts at 15 minutes, 99 at an hour, 60 at two)
  -- its detections are faint and spread through the day as well as the
  night, which bouting can't fix and its floor can. The default stays at
  15 minutes: wider buys little, and at an hour a spot-check bout of an
  accepted species already swallows 461 detections. The lever on the queue
  is the rules -- the owl's floor above all -- not the gap. At 15 minutes,
  with two listeners on a review bout and one on a spot-check, the card is
  ~1,270 bouts heard.
- **The floor.** The default 0.5 is from the table above. Per-species floors
  come from what reviewers reject: the rules page's rejection rate is the
  measurement.
- **The first table.** Generated from the Marymoor cards and imported; then
  set by hand as reviews come in.

## Rollback

An older image doesn't know `accepted`, `route`, bouts or the summaries. It
shows `accepted` as a raw neutral chip, counts only `confirmed` on the
landing page, and leaves the new containers alone; bouts stay where they
are until the newer image is back. ROLLBACK.md says what that leaves stale.

## Milestones

Each one ships on its own.

**M0 — Groundwork.**
- The worker keeps what people have said when it stores a file's detections
  again (`keepVerdicts`, *Why it stays correct*).
- `accepted` everywhere detections are read: the list's status filter, the
  chip and the detection page, the MCP tools and their instructions.
- `internal/review` with `Build` and `Adjust`; the `cardSummaries` container;
  the landing page reads summaries, building a card's the first time it meets
  one without, and a review adjusts its card's. Until M1 a summary counts
  `confirmed` alone, since nothing writes `accepted`.
- Each container comes with the milestone that first uses it, because the
  app refuses to start without every container it knows: `settings` with M1,
  `bouts` with M2.

**M1 — Rules and routing.** Common birds count publicly; nothing is reviewed
differently yet.
- `internal/review`: the rules, `Route`.
- The worker routes, stores `route` and `accepted`, adjusts the card's
  summary by the `accepted` ones (`review.Adjust`), and cuts context clips.
- `PUT .../review` with `unreviewed` (the detection page's Undo) puts a
  detection back to what its route says -- `accepted` for an accepted one --
  rather than to `unreviewed`.
- The landing page's wording stops saying every counted detection was heard.
- The preparation loop, routing only, which also routes the cards analyzed
  before.
- `GET`/`PUT /review/rules` and the Review rules page, with the program's
  counts beside each row and JSON import and export.
- The first table: drafted from the Marymoor data, imported through the
  page.

**M2 — Bouts.**
- Bouting and spot-checks in the preparation loop; `boutId`; the card's
  review counts; the `bouts` container. A card prepared by M1 is prepared
  again for its bouts (`review.preparation`).
- The gap measured on a real card (*Measure before tuning*): `cmd/bouts`.

**M3 — The review API.** Verdicts, resolution, copying to detections and the
summary, blind verdicts, adding confirmed species to the rules.
- `GET /review/cards`, a card's bouts, one bout with its detections, and
  `PUT .../verdict` (*API*); `internal/review`'s `Give`, `Settled`, `Carry`
  and `Resolve`.
- The preparer finishes a resolution cut short; a page save goes over rows
  verdicts added (`savedVersion`).
- Undo on a detection's page goes back to its bout's verdict.

**M4 — The Review tab.** The card picker and a card's review: queue, player
with context and slow speed, verdicts, keys, Save & next, other listeners'
verdicts, progress.
- `<bs-review>`, `<bs-review-card>` and `js/review-queue.js` (the queue's
  order, stepping and species, tested with `node --test`);
  `<bs-spectrogram>` gains a focus span and a playback rate.
- A reviewer who doesn't upload lands on Review rather than Detections.

**M5 — Disagreements and overrides.** The Disagreements page and the expert's
final say; a detection's page links to its bout, and its Confirm and Discard
become an expert's or admin's override.
- `GET /review/disagreements` (`Store.ListBoutsByStatus`), and `PUT
  .../resolution` (`review.Decide`), which shares the verdict's path to the
  detections; `<bs-disagreements>`, and the final say in a card's review.
- `PUT /detections/{ref}/{id}/review` is a birding expert's. A program with
  no rules saved therefore has nothing a reviewer can review: saving the
  first table is part of turning review on.

**Later.** Closing a card when every bout is resolved (TODO.md 2.6, which
needs this first); the spot-check rejection rate on the rules page (M1 has
the column, M3 the data); months per species; eBird import.

## Status

| Step | State |
|---|---|
| This design | Written |
| M0: re-runs keep reviews; `accepted` read everywhere; card summaries and the landing page; the `cardSummaries` container; SCHEMA.md, DEPLOYMENT.md, ROLLBACK.md, CLAUDE.md | Built, not deployed |
| M1: `internal/review`; routing and context clips in the worker; the preparation loop (routing); rules API and page; the first table | Built, not deployed. The first table ships as the page's suggested one; saving it, on the page, is what turns routing on |
| M2: bouts and spot-checks; the card's review counts; the `bouts` container; the gap measured | Built, not deployed. Measured: the gap barely matters (*Measure before tuning*), so it stays at 15 minutes |
| M3: the review API | Built, not deployed. Tried on the dev card through the API: blind verdicts, a correction to another species, a rejected spot-check, a dispute, a row added to the rules, and preparing again under it changing nothing. `disagreements` and `resolution` are M5's |
| M4: the Review tab | Built, not deployed. Tried in a browser on the dev card (with test clips cut from the Osprey recording): two reviewers settling a bout blind, the expert's view, keys, context and half speed, another species, a phone's width |
| M5: disagreements and overrides | Built, not deployed. Tried in a browser on the dev card: a dispute settled from the Disagreements page, an override to another species on a settled bout, and a detection's page as an expert and as a reviewer |
