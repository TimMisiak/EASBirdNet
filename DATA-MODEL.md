# Birdsense data model

The API shapes are settled (see CLAUDE.md); this file is where those shapes live
once they stop being `internal/api/store.go`. Production is **Azure Cosmos DB
for NoSQL**; locally it is the Cosmos DB emulator in `docker-compose.yml`.

Nothing here exists yet. There is no database, no container, and no data —
`internal/cosmos` only connects and answers "is it reachable". This document is
the plan the first migration has to implement, and the place to argue with the
plan before it is expensive to change.

Numbers marked *order-of-magnitude* are arithmetic on the placeholder figures
already in `store.go` (24 one-hour files a night, ~383 MB a file, five
stations). They are here to tell decisions apart, not to be a budget.

## What goes in Cosmos, and what does not

Cosmos holds the program's records: recorders, people, cards, nights,
detections, and the rollups the public page reads. All small JSON, all queried
by the API.

Audio never goes in Cosmos. Raw recordings and the short clips a reviewer
listens to go to Azure Blob Storage, and items carry the blob path. An item is
capped at 2 MB; one hour of recording is ~383 MB.

That split is also where the money is (*order-of-magnitude*):

| | one year, five stations |
|---|---|
| Cosmos: detections, nights, cards | ~4 GB |
| Blob: raw recordings | ~17 TB |

So the interesting retention question is about Blob, not Cosmos, and it is not
settled — see *Open questions*.

## The account

One database, `birdsense`, four containers:

| container | partition key | `id` | holds | growth |
|---|---|---|---|---|
| `catalog` | `/type` | per type, below | recorders, roster, species, rollups | bounded, hundreds |
| `uploads` | `/volunteerId` | card reference | SD cards in flight and done | ~250/year |
| `nights` | `/stationId` | `YYYY-MM-DD` | one recorded dusk-to-dawn block | ~1.8k/year |
| `detections` | `/stationId`, `/night` | deterministic, below | what BirdNET heard | millions/year |

Four, rather than one container per type or one container for everything:
`uploads`, `nights` and `detections` each want a different partition key, and
forcing them together would mean a synthetic key plus a cross-partition read for
the common case. Everything that is small and read-often shares `catalog`, which
keeps its reads single-partition and its cost floor low.

## Conventions every item follows

Each item carries `id`, `type`, `schemaVersion`, and its partition key
field(s) — the partition key is an ordinary field, not metadata.

- **`id` is unique within a logical partition, not globally.** Two items in
  different partitions may share one. Where we can, `id` is *deterministic*, so
  re-running a pipeline upserts instead of duplicating. Cosmos forbids `/`,
  `\`, `#`, `?` and trailing whitespace in an `id`; ours are ASCII with `-`
  separators, which keeps them safe in a URL path too.
- **The partition key is immutable.** Changing it means delete-and-reinsert, so
  it may only be a fact that never changes about an item.
- **`type` is on every item**, even in a container with one type. It is what
  makes a mixed container queryable and a change-feed reader able to tell what
  it just read.
- **`schemaVersion` is an int, starting at 1.** Cosmos will happily store any
  shape; a version field is the only thing that makes a later migration
  tractable. Readers tolerate older versions, writers set the current one.
- **camelCase, and the same names as the API.** An API response should be a
  projection of an item, not a translation of one.
- **Never use a leading underscore.** `_rid`, `_self`, `_etag`, `_attachments`
  and `_ts` belong to Cosmos. We read `_etag` for optimistic concurrency;
  `_ts` is for the change feed and for debugging, never for business logic.
- **Instants are RFC3339 with `Z`**; fixed-width UTC strings sort
  lexicographically, which is to say chronologically, so `ORDER BY` and
  `BETWEEN` work without a second field. **Calendar dates stay
  `YYYY-MM-DD`**, because a date is not an instant — the reason is already in
  `day()` in `store.go`. Everything is displayed in America/Los_Angeles.
- **A denormalized copy sits next to its id** (`stationId` *and*
  `stationName`). The id is the truth and the copy is for rendering, so a
  screen costs one read. A rename therefore fixes new items only; backfilling
  old ones is a deliberate job, never something a reader does on the fly.

## `catalog` — recorders, roster, species, rollups

Partition key `/type`. Bounded, read-often, and every query is
single-partition: the roster, the recorder list, and the rollups behind the
public page are each one partition. Three or four logical partitions is not a
distribution problem at this size; it is the point.

```jsonc
// id: "SW-02", pk: "station"
{ "id": "SW-02", "type": "station", "schemaVersion": 1,
  "name": "Marymoor Park – Snag Row", "latitude": 47.66021, "longitude": -122.11384,
  "addedOn": "2026-03-14", "retiredOn": null }

// id: "p2", pk: "person"  — the roster is the allow-list; there is no password
{ "id": "p2", "type": "person", "schemaVersion": 1,
  "name": "Jane Volunteer", "email": "Jane@example.com", "emailLower": "jane@example.com",
  "provider": "Google", "role": "volunteer", "addedOn": "2026-04-14" }

// id: "barowl", pk: "species"  — only species the program has actually seen
{ "id": "barowl", "type": "species", "schemaVersion": 1,
  "commonName": "Barred Owl", "scientificName": "Strix varia" }

// id: "totals", pk: "program"  — the three headline numbers, all-time
{ "id": "totals", "type": "program", "schemaVersion": 1,
  "recorders": 5, "nightsRecorded": 1284, "confirmedDetections": 597,
  "updatedAt": "2026-09-14T09:02:00Z" }

// id: "2026-09-13", pk: "daily"  — one per day, written by ingest
{ "id": "2026-09-13", "type": "daily", "schemaVersion": 1,
  "confirmedDetections": 63,
  "species": [ { "code": "barowl", "commonName": "Barred Owl",
                 "scientificName": "Strix varia", "detections": 41,
                 "stations": ["SW-02", "SW-04"],
                 "lastDetectedAt": "2026-09-13T10:12:00Z" } ] }
```

`emailLower` exists because Cosmos string comparison is case-sensitive and the
roster is an allow-list: a capital letter must not turn into a failed sign-in.
(`store.PersonByEmail` already uses `strings.EqualFold`; this is the same rule,
expressed where the query runs.)

**Rollups are per-day, not per-window.** `GET /public/overview?days=` accepts
3–30, so the endpoint reads that many `daily` items with one
single-partition range query and folds them: detections sum, stations union,
and the count of days a species appears *is* its nights figure. Precomputing a
document per window would mean 28 documents to keep fresh, and aggregating over
`detections` instead would put a cross-partition scan of millions of items
behind an unauthenticated route. Neither is worth it.

## `uploads` — SD cards on their way in

Partition key `/volunteerId`, `id` is the card reference (`OWL-20260907-SR02`).

The point read `(id: reference, pk: volunteerId)` is the fetch **and** the
authorization check: a card that is not yours is simply not found, which is
exactly the 404 the API already promises. `GET /uploads` — my cards — is one
partition. `GET /admin/uploads` is a cross-partition query, deliberately: it is
the rarer coordinator route over a container holding hundreds of items, and it
needs a composite index to `ORDER BY startedAt DESC` across partitions.

A card belongs to whoever pulled it, forever, which is what makes
`volunteerId` legal as a partition key.

Because `id` is unique per *partition*, a reference is unique per volunteer
rather than globally. That is the determinism the API already leans on: posting
the same pull date and recorder again is a resume of the same card, not a new
one.

```jsonc
// id: "OWL-20260907-SR02", pk: "p2"
{ "id": "OWL-20260907-SR02", "type": "upload", "schemaVersion": 1,
  "volunteerId": "p2", "volunteerName": "Jane Volunteer",
  "stationId": "SW-02", "stationName": "Marymoor Park – Snag Row",
  "pulledOn": "2026-09-07", "notes": "Batteries at 20% when swapped.",
  "nights": [ { "date": "2026-08-24", "files": 24, "bytes": 9192000000, "flag": "" } ],
  "fileCount": 317, "filesUploaded": 214,
  "totalBytes": 121411000000, "bytesUploaded": 81974000000,
  "status": "interrupted", "statusDetail": "",
  "startedAt": "2026-09-13T03:14:00Z", "updatedAt": "2026-09-13T03:14:00Z" }
```

`nights[]` stays embedded: it is the manifest scanned off the card, bounded at
about fourteen entries, and never queried on its own.

**The apparent duplication with the `nights` container is not duplication.**
`uploads.nights[]` is what the card claimed to hold; a `nights` item is what was
actually recorded and analyzed. Two different facts, one of which is sometimes
wrong. Do not "fix" them into one.

**Progress only moves forward, enforced by the database.** `RecordProgress`
becomes a partial document update (`PATCH`) with a filter predicate —
`FROM c WHERE c.filesUploaded < @files` — so there is no read-modify-write and
no lost update when two batches report at once. The 412 that comes back when the
predicate does not match means "already ahead of you"; that is a success, not an
error. This is the invariant `store.go` currently keeps in a comment.

## `nights` — one recorded dusk-to-dawn block

Partition key `/stationId`, `id` is the date the evening began.

Point read per station-night; a season is
`WHERE c.stationId = @s AND c.id BETWEEN @from AND @to` — one partition, on the
always-indexed `id`. Five recorders means five logical partitions of ~365 small
items a year, which stays far inside the per-partition cap for decades.

This is the item counted by `nightsRecorded`, and the one that carries where
analysis got to.

```jsonc
// id: "2026-08-24", pk: "SW-02"
{ "id": "2026-08-24", "type": "night", "schemaVersion": 1,
  "stationId": "SW-02", "night": "2026-08-24",
  "uploadReference": "OWL-20260907-SR02",
  "files": 24, "bytes": 9192000000,
  "firstSampleAt": "2026-08-24T19:02:11Z", "lastSampleAt": "2026-08-25T12:00:00Z",
  "coverageMinutes": 1438, "flag": "",
  "analysis": { "state": "analyzed", "analyzer": "BirdNET-Analyzer 2.4",
                "minConfidence": 0.25, "ranAt": "2026-09-08T04:20:00Z",
                "detections": 2143, "confirmed": 61, "species": 6 },
  "audioPrefix": "raw/SW-02/2026-08-24/" }
```

## `detections` — what BirdNET heard

The only container that grows without bound, and the only one whose partition
key needed an argument.

**Hierarchical partition key `["/stationId", "/night"]`** — one logical
partition per station-night.

- The review screen asks for one night at one recorder: that is an exact
  partition-key match, no fan-out.
- Ingesting a night writes into exactly one partition, which is also what makes
  batched writes possible at all.
- A night's worth of detections is megabytes, so the per-logical-partition cap
  (20 GB at the time of writing) is never in sight.
- A **prefix query on `stationId` alone is still efficient** — that is what
  subpartitioning buys, and what a synthetic `"SW-02|2026-08-24"` string key
  could not do.

Not `/stationId` alone: five logical partitions for millions of items is a hot
partition during ingest and a hard ceiling later. Not `/id`: perfect spread, but
every screen we have asks for a night at a time, so every read would fan out.

```jsonc
// id: "SW-02-2026-08-24T22-btw-000180-barowl", pk: ["SW-02", "2026-08-24"]
{ "id": "SW-02-2026-08-24T22-btw-000180-barowl",
  "type": "detection", "schemaVersion": 1,
  "stationId": "SW-02", "night": "2026-08-24",
  "recordingId": "SW-02-2026-08-24T22",
  "detectedAt": "2026-08-25T05:03:00Z",
  "startOffsetSeconds": 180, "durationSeconds": 3,
  "speciesCode": "barowl", "commonName": "Barred Owl",
  "scientificName": "Strix varia", "confidence": 0.8123,
  "review": { "state": "unreviewed", "by": null, "at": null, "note": "" },
  "clipPath": "clips/SW-02/2026-08-24/000180-barowl.flac" }
```

Details that matter:

- **`night` is derived, not the date part of `detectedAt`.** A call at 03:12
  belongs to the previous evening's night. Ingest computes it once from local
  time on a noon-to-noon boundary; nothing else may compute it differently, or
  the same night ends up in two partitions.
- **The `id` is deterministic** — recording, offset, species — so re-analysing a
  night upserts over the old verdict instead of doubling every row. There is no
  transaction that can cover a whole night (thousands of items, far past the
  batch limits), so idempotent writes are what stands in for one, and the
  `nights` item flips to `analyzed` last.
- **Review state is a field, not a second container.** One item, one reviewer at
  a time, and the screen doing the review already has the item in hand. `_etag`
  covers the concurrent-reviewer case.
- **Indexing is opt-in here.** The default policy indexes every path, which on
  the one container we write millions of items to is write RU spent on paths no
  query mentions. Exclude `/*`, include `review/state`, `speciesCode`,
  `confidence`, `detectedAt`, and add the composite index the review queue
  orders by: `(review/state ASC, confidence DESC)`.

## Access patterns

| route | container | shape | partitions |
|---|---|---|---|
| `GET /public/overview?days=n` | `catalog` | range over `daily` ids | 1 |
| `GET /session` | `catalog` | `type='person' AND emailLower=@e` | 1 |
| `GET /stations` | `catalog` | `type='station'` | 1 |
| `GET /uploads` | `uploads` | `pk=volunteerId` | 1 |
| `GET /uploads/{ref}` | `uploads` | point read `(ref, volunteerId)` | 1 |
| `POST /uploads/{ref}/progress` | `uploads` | patch with filter predicate | 1 |
| `GET /admin/uploads` | `uploads` | all, `ORDER BY startedAt DESC` | all |
| `GET /admin/people` | `catalog` | `type='person'` | 1 |
| review a night *(not built)* | `detections` | `pk=(station, night)` | 1 |
| a recorder's season *(not built)* | `detections` | prefix `pk=(station)` | that station's |

Every route the frontend has today is a point read or a single-partition query.
The one fan-out is an admin screen over hundreds of items. That is the property
worth protecting as the model grows.

## Throughput, consistency, auth

**Serverless account.** Traffic is a handful of volunteers plus a nightly batch;
serverless bills per request with no floor, and the ingest burst fits inside its
per-container ceiling. *Revisit when* ingestion becomes continuous, the public
page gets real traffic, or we want multi-region — then provisioned autoscale.

**Session consistency** (the account default) is the right one. The only
read-after-write that matters is a volunteer seeing their own upload progress,
and the SDK's session token covers it — as long as one long-lived client serves
the requests. Create the `azcosmos.Client` once, at startup, as
`internal/cosmos` does; a client per request silently loses that guarantee.

**Auth is a key today and should not be tomorrow.** The emulator's well-known
key is in `docker-compose.yml` on purpose — it is published in Microsoft's own
docs and unlocks nothing but a local container. In Azure the app should use a
managed identity with data-plane RBAC (`azidentity` + `azcosmos.NewClient`) and
hold no key at all. Note that OAuth refuses a plain-HTTP endpoint, which is the
other reason key auth is the local path.

**Backups**: periodic is enough. Detections can be recomputed from the audio;
the audio in Blob is the part that cannot be recovered.

**TTL**: off on every container. The candidate, later, is unreviewed detections
below the confidence threshold.

## The emulator against the real thing

Same wire API, same SDK, same auth scheme. What has been verified so far:
`azcosmos` v1.5.0 talks plain HTTP with the well-known key to a stand-in for the
emulator's gateway, so there is no certificate to trust in dev
(`internal/cosmos/cosmos_test.go`). The app's startup log line and
`/api/v1/health` are the check against the real emulator.

Worth knowing:

- **The local emulator is a year behind Azure.** It is pinned to
  `vnext-EN20251022` because later releases need an AVX2 CPU the dev host does
  not have (see `docker-compose.yml`). Treat a feature that is missing locally
  as a possible emulator gap before treating it as a modeling problem, and
  confirm against a real account.
- **RU charges reported locally are not Azure's.** Never tune throughput or
  indexing against emulator numbers.
- **Hierarchical partition keys are supported by the SDK** (`MultiHash`,
  including prefix queries). Confirm the emulator agrees the first time we
  create `detections`. If it does not, the fallback is a synthetic
  `nightId = "SW-02|2026-08-24"` with `/nightId` as the key — same partitioning,
  no cheap prefix query — and we revisit.
- **The emulator starts empty**, and `docker compose down -v` puts it back that
  way. Whatever creates the database and containers therefore has to be
  idempotent and run at startup (`CreateDatabaseIfNotExists` /
  `CreateContainerIfNotExists`), not as a step someone remembers to do.

## Open questions

1. **Audio retention.** ~17 TB a year of raw recordings dwarfs everything else
   in this document. Keep raw audio hot, tier it to archive, or keep only the
   review clips and discard the rest after analysis? This changes what a
   `nights` item has to point at, so it wants deciding before ingestion is
   built.
2. **Detection grain.** One item per 3-second BirdNET window, or merged
   intervals of consecutive windows for the same species? Merging cuts item
   count by a large factor and is what a reviewer actually listens to; it also
   loses the raw confidence curve.
3. **A confidence floor on what we store at all.** Everything BirdNET emits, or
   only above a threshold, with the rest left in the analyzer output on Blob?
4. **`person.id`.** A stable opaque id with `emailLower` indexed (as above), or
   the lowercased email as the id for a 1 RU point read? The first survives
   someone changing their address; the second is cheaper and simpler.
5. **Per-species counts on a `nights` item**, denormalized, so a station page
   needs no query into `detections`. Worth it only once there is a station page.
