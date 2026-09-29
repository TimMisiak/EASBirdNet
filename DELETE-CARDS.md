# Deleting a card in the background — design

Deleting a card by marking it `deleting` and answering at once, and letting a
sweep in the web app remove it, so a delete no longer has to finish inside
one request. Built on branch `delete-upload-failed`, not yet deployed: the
**Status** table at the end is where to resume. Where this and the code disagree, the code is what runs -- fix
whichever is wrong.

## Problem

`DELETE /admin/uploads/{ref}` (`deleteUpload`, `internal/api/api.go`) does the
whole delete inside the request. It removes every blob under the card's
prefix, then its `audioFiles`, its `detections` and the upload itself. On
2026-09-28 a delete failed with "something went wrong on our side". The
cause was a Cosmos 429 on `detections`, straight after a large card's delete:

- Transactional batches save round-trips, not request units. Each document
  deleted costs about what writing it did.
- A serverless partition allows ~5,000 RU/s, and a card holds tens of
  thousands of detections.
- Eight batches of 100 in flight ask for more than that at once.

`whileThrottled` (`internal/db/cosmos.go`) now waits those 429s out instead
of failing. That leaves three problems:

- **Two time limits.** A large delete now takes as long as the RU budget
  allows, and the request still has the ingress's 240 s (DEPLOYMENT.md,
  *Request time*) and ~30 s of shutdown grace on a deploy.
- **A cut delete leaves a half-deleted card, and nothing says so.** The
  order keeps it safe, because the upload goes last, so deleting again
  finishes it. But until someone does, the card shows missing clips, missing
  files or a stale count. Only the analysis queue ever finishes a delete by
  itself (`Queue.gone`), and only on a card it happens to be working on.
- **It competes for RU while it runs.** While it lasts, the delete takes the
  RU budget that reviewers, the landing page and a worker storing detections
  need, so their requests can hit the same 429.

## Proposal

Split the delete into a request that marks the card and a sweep that deletes
it.

1. **The request marks.** `DELETE /admin/uploads/{ref}` does one
   `UpdateUpload`: `status` becomes `deleting` and `deleteRequestedAt` is set.
   A card already `deleting` is left as it is. It logs who asked, wakes the
   deleter, and answers **202** `{"deleting": ref}`. A card that doesn't
   exist is still a 404. The one caller (`bs-admin-uploads.js`) ignores the
   body and reloads the list, so the change of status code needs nothing
   from it.
2. **A deleter does the work.** It is a new `internal/deletion` package with
   `Deleter.Run` and `Wake`, the same shape as `analysis.Dispatcher`: a wake
   channel, plus a backstop pass every 5 minutes. Each pass does this:
   - List `UploadFilter{Status: deleting}`, oldest `deleteRequestedAt`
     first.
   - Delete one card at a time, with today's steps in today's order: blobs
     under the prefix, with the shared-prefix guard (`deleteAudio`, which
     moves here from `internal/api`), then `store.DeleteUpload`.
   - When a card fails, log it and set `statusDetail`, for example
     `deleting: retrying (storage unreachable)`. Then go on to the next card
     and try again on a delay that grows from 30 s to 10 min.

   Nothing about the retrying is stored beyond that: a restart just starts
   again, and every step is idempotent.
3. **The deleter runs in the web app, in both modes.** The web app always has
   the store and file storage. In job mode the workers are only there while
   cards are waiting for analysis. `cmd/server/main.go` starts it beside
   retention, stops it the same way, and passes it to `newMux`, the way
   `analysisAPI` is passed.
4. **It waits for claims.** The deleter skips a card while any of its files
   has a step `analyzing` with a lease in force (SCHEMA.md, *Claims*),
   whether BirdNET's or Perch's. See below for why. It also leaves a card
   alone for its first minute in `deleting` (`settle`). The queue lists
   `processing` cards and then claims their files, so a claim on a card that
   was listed just before the mark can land just after it. The minute is
   there so that such a claim exists by the time the sweep looks for one.
5. **Other paths refuse a `deleting` card.**
   - `tusAccess` 404s it, as if it were already gone. Today a transfer in
     flight keeps sending chunks to a card being deleted, because only
     creating an upload checks the card's status.
   - `createUpload` 409s it with "this card is being deleted". Today
     registering it again answers 201 with the card as it is.
6. **The card shows it's being deleted.**
   - `upload-status.js` gets a `deleting` label ("Deleting") and chip kind.
   - `isMoving` includes `deleting`, so the card lists and the card page
     poll until it is gone.
   - `bs-admin-upload-detail.js` treats a 404 while polling as "this card
     was deleted", and links back to All uploads instead of keeping the old
     page.
   - The volunteer's My uploads shows the same chip, and doesn't offer to
     resume the card.
7. **What was heard on it stops counting.** The public overview skips
   `deleting` cards and their detections; it already lists every card, so it
   needs no extra read. `GET /detections` lists `deleting` cards (usually
   none) and drops their detections before counting species and totals.
   Point reads (a card's detections, one detection, its clip) are left alone:
   they 404 once the sweep reaches them, which the detection page already
   handles.

## Why it stays correct

**Setting `deleting` ends the card's other work.** The queue lists only
`processing` cards (`Queue.fill`, `Queue.Pending`), so no new step is claimed
and the dispatcher stops counting the card. `tally` changes a card only from
`processing`, so a task that ends afterwards can't move the card back to
`in_review`. Retention looks only at `in_review`, `needs_attention` and
`results_sent`. `tallyFiles` and `/progress` change a card only while it is
transferring.

**Why wait for claims.** A step that started before the mark runs to the end
and writes its detections, its clips and its file. If the sweep ran
underneath it, those writes could land after the sweep had passed:

- Detections written that way are caught by `Queue.gone`, which deletes the
  card again when a write finds it gone.
- Clips written that way are not: nothing names them, and nothing sweeps
  blobs after the fact. Today's synchronous delete has the same gap.

Waiting until no lease is in force closes it. The mark stops new claims, so
the wait is at most one step: a BirdNET file takes minutes, and a Perch one
longer. A worker that dies holding a file stops renewing its lease, so the
lease lapses within five minutes and the sweep goes ahead. `Queue.gone`
stays, as the backstop for a claim the sweep misjudges.

**The upload still goes last.** A pass that is cut leaves the card in
`deleting`, listed and saying so, and the next pass finishes it. Nothing is
ever left that no document names, except the one gap below.

**The deleter doesn't compete with itself.** One replica runs one deleter,
which deletes one card at a time, so two passes never race over a card. If
the analysis queue's `Queue.gone` overlaps a pass, the overlap is handled the
way two deletes already are: a document that is already gone counts as
deleted, and so does the upload.

## Gaps accepted

- **A chunk already streaming when the card is marked still finishes.**
  `tusAccess` refusing the card stops the next request, not one already
  being received. If that chunk is a file's last and lands after the sweep,
  it leaves a committed blob that no document names. It needs a delete
  during a live upload, on a card that is still transferring, in a window of
  one 50 MB request. The storage lifecycle rule covers uncommitted blocks,
  but not this blob.
- **Detections stay reviewable until they're swept.** The detection page of
  a `deleting` card still works, and still takes a review, until the sweep
  reaches it. That is harmless, because the review is deleted with it.

## Measure before tuning

With no request deadline, the deleter can go slower, leaving RU for everyone
else. `deletePartition` runs `batchWorkers` (8) batches at once, which a
background delete no longer needs. Before lowering it, delete one real card
and read the Cosmos **Total Request Units** metric, split by operation type:
how many RU the card cost, and for how long it sat at the cap. Then pick the
concurrency, for example by passing it into `deletePartition` rather than
changing `batchWorkers` for upserts too.

## Rollback

An image from before this doesn't know `deleting`. After a rollback, such a
card shows the raw word as a neutral chip, and nothing sweeps it. The older
image's `DELETE` removes a card in any status, so deleting it again finishes
it. ROLLBACK.md says so.

## Status

| Step | State |
|---|---|
| `internal/db/cosmos.go`: `whileThrottled` around every request in `deletePartition` and `deleteEach` | Built |
| `internal/db/models.go` + SCHEMA.md: `StatusDeleting`, `deleteRequestedAt`; the *Upload status* table and the `DELETE /admin/uploads/{ref}` row in *API mapping* | Built |
| `internal/deletion`: `Mark`; `Deleter` (`Run`, `Wake`, `Sweep`, one card at a time, a minute's settle, waits for leases, retries 30 s to 10 min, `statusDetail` on failure); `deleteAudio` moved here from `internal/api` | Built |
| `internal/api`: `deleteUpload` marks and answers 202; `tusAccess` and `createUpload` refuse a `deleting` card; the overview and `GET /detections` skip one | Built |
| `cmd/server/main.go`: start and stop the deleter beside retention, pass it to `newMux` | Built |
| Tests: `internal/deletion` (sweep, settle, BirdNET and Perch leases, a failed pass tried again, the retry bound, `Mark` keeping its time, `Run` woken); `TestDeleteUpload` (marked, tus and re-registering refused, then swept); `TestACardBeingDeletedIsNotCounted` | Built |
| Frontend: `upload-status.js` label, kind and `isMoving`; the card page's note and its 404 while polling; All uploads hides the bin on a `deleting` card; My uploads doesn't offer to resume one | Built; checked in a dev server with screenshots |
| CLAUDE.md (*Deleting a card is a status and a sweep*, the tree, the route table, *State of the code*), DEPLOYMENT.md *Request time*, ROLLBACK.md | Built |
| Deploy, then delete one large real card: check the deleter's log (`deleted a card`, `took`) and the Cosmos **Total Request Units** metric | Not started |
| Choose the deleter's concurrency from that measurement | Not started |
