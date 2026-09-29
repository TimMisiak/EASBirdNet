# Counting a card's files without a recount — design

Keeping a card's `filesUploaded` and `bytesUploaded` as a running count,
instead of re-reading every file on the card each time one lands, so a card
of n files costs O(n) rather than O(n²). The upload side is built (2026-09-28);
analysis's recounts are still to do. The **Status** table at the end is where
to resume. Where this and the code disagree, the
code is what runs -- fix whichever is wrong. It comes before
[PARALLEL-UPLOADS.md](PARALLEL-UPLOADS.md), which would otherwise let two
recounts race.

## Problem

Every time a file finishes uploading, the server re-reads every file on the
card to recount it, so a card of n files costs about n² document reads. For
the reported card that is 1,434 recounts of 1,434 documents, about 2 million
reads, spent while the volunteer waits between files.

`afterFileUpload` (`internal/api/tus.go`) marks the file `uploaded`, then
calls `tallyFiles` (`internal/api/api.go`). That lists all of the card's
`audioFiles` (one Cosmos query, returned in pages), counts the received ones
and their bytes, and writes the totals to the card. The browser gets its
answer only after the recount, so the recount's time is idle line time. Every
file on the card has a document from registration on, so each recount reads
all 1,434 from the first file.

On that card, a file's last PATCH took ~2 s longer than its bytes explain
(PARALLEL-UPLOADS.md, *Measured*) -- the commit, the file's document and this
recount together, ~0.8 h over the card.

A 40-file test never shows it: 40 recounts of 40 documents is 1,600 reads. The
cost grows with the card, and a card is the thing that keeps getting bigger.

## Proposal

Count a file on the card at the moment its own document changes to
`uploaded`, and keep the full recount only for registration and the last
file. Each file then costs two point writes instead of a query over the card,
and the whole card costs O(n).

1. In `afterFileUpload`, the `UpdateAudioFile` mutate already decides whether
   the file moves from `pending` to `uploaded`. Record that in a local
   `counted` flag.
2. If `counted`, call a new `countFile(ctx, ref, size)`. It does one
   `UpdateUpload`: if the card is still transferring, `filesUploaded += 1`
   and `bytesUploaded += size`, capped at `fileCount` and `totalBytes` as now.
3. When that update brings `filesUploaded` to `fileCount`, run `tallyFiles`
   once. It confirms nothing is still `pending` and moves the card to
   `processing`, exactly as today.
4. Registration (`createUpload`) keeps calling `tallyFiles`, because that is
   where the list itself can change.

The count is still the server's own, from its own documents, never the
browser's. CLAUDE.md's rule that "a retried chunk can't count twice" holds,
because only the write that changes the file's status increments.

## Why it stays correct

The counter can drift only in ways that the full recount at the last file, or
at the next registration, already repairs. The file documents stay the truth,
and the counter is only the running total between recounts.

| Case | What happens | Repaired by |
|---|---|---|
| Final PATCH retried, or finish hook runs twice | The second mutate sees the file already `uploaded`, so `counted` is false and nothing is added | Nothing needed |
| Two files finish together (parallel uploads) | Both increments are replace-if-unchanged on the card, so one retries and neither is lost. Today's recount can instead write a stale total last. | Nothing needed |
| File marked, then the card update fails or the replica dies | The card is one short and never reaches `fileCount` | The browser's existing resync: after the last file it sees the card unfinished, registers it again, and registration recounts |
| Card re-registered while a file is finishing | The recount may already include the file, and the increment then adds it again, so the count is one high | The recount at `fileCount`, which sets the exact number and moves to `processing` only if nothing is `pending` |

The card still moves to `processing` only from `tallyFiles`, so the one
transition that starts analysis keeps its current, fully checked rule.

## Other per-file work that grows with the card

The upload recount is the only one a volunteer waits on, but analysis has the
same pattern twice, and a browser loop does too.

| Where | Per file | Whole card | Matters? |
|---|---|---|---|
| `afterFileUpload` → `tallyFiles` (upload) | Lists every file on the card | O(n²) | Yes: the volunteer waits on it. This proposal. |
| `Queue.runTask` → `Queue.tally` (analysis, `internal/analysis/analysis.go`) | Lists every file on the card to recount analyzed, failed and detections | O(n²) | Little time, since each file takes minutes of BirdNET, but it is the same reads. The same counter fits, with the recount kept for finishing the card. |
| `Queue.fill` (analysis) | Lists every file of every processing card on each pass | O(n²) per card | Same as above. Fixing it means a query for claimable files only, not a counter. |
| `sendWaiting` finds the next `waiting` file (`upload-flow.js`) | Scans the card's file list | O(n²) comparisons, about 1 ms for 1,434 files | No, but a cursor makes it O(n) for free |

The file list checks every row about every 150 ms while sending. That is O(n)
per redraw, not per file, so it doesn't grow with the card squared.

## Status

| Step | State |
|---|---|
| `internal/api/tus.go`: `counted` in `afterFileUpload`'s mutate; `countFile` only when it is true | Done 2026-09-28 |
| `internal/api/api.go`: `countFile`, which increments the card and calls `tallyFiles` when `filesUploaded` reaches `fileCount` | Done 2026-09-28 |
| Tests (`tus_test.go`): a 60-file card lists its files once, at the last file; a repeated finish adds nothing; a failed card update is repaired by registering again. The existing tests cover counts rising per file and the last file moving the card to `processing` | Done 2026-09-28 |
| SCHEMA.md, *API mapping*, and CLAUDE.md name `countFile` beside `tallyFiles` | Done 2026-09-28 |
| Deployed, and a large card's last-PATCH time checked in the logs (PARALLEL-UPLOADS.md, *Checking a card's upload*) | Not started |
| Later, as its own change: the same counter for `Queue.tally`, and a claimable-only query for `Queue.fill` | Not started |
