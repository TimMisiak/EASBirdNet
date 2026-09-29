# Parallel card uploads — design

Sending a few of a card's files at once, so a volunteer's line stays busy
while the server stages one file's chunk to Blob Storage or finishes another.
Built, not yet deployed or benchmarked (2026-09-28): the **Status** table at
the end is where to resume.
Where this and the code disagree, the code is what runs -- fix whichever is
wrong. The per-file recount it leans on is [CARD-COUNTS.md](CARD-COUNTS.md).

## Problem

On Azure, a card upload leaves the volunteer's connection idle for part of
every chunk and every file, so no line runs at its full speed. A volunteer
reported 80 GB (1,434 files) taking about 6 hours, which is about 30 Mbit/s.
On a gigabit line, test uploads reach 100-300 Mbit/s.

The browser sends one file at a time, in 50 MB tus PATCHes
(`frontend/js/upload-flow.js`). Two things on the server stop the line from
staying busy:

- **Each chunk is stored, then forwarded.** tusd's Azure store
  (`azurestore.go`, `WriteChunk`) copies the whole PATCH body to a temp file,
  then stages it to Blob Storage as one block. The PATCH isn't answered until
  that block lands, so the browser sends nothing in the meantime. The local
  store writes to disk as bytes arrive, which is part of why uploads ran
  faster before Azure.
- **Every file pays a fixed cost.** A POST and each PATCH make serial calls to
  Cosmos DB and Blob Storage: the session, the card, the file, tusd's `.info`
  blob and block list (twice per PATCH), and on the last chunk the commit and
  the card's recount. The recount grows with the card (CARD-COUNTS.md).

With one request in flight, each of these idle periods is lost time. On a
gigabit line, 50 MB arrives in about 0.4 s and then waits about a second while
it is staged, which matches the observed ceiling. On a 30 Mbit/s line the same
wait is under 10% of the chunk.

### Measured

The reported card, from the request log (`requestLogger`: method, path and
duration only) for the tus requests under the card's prefix:

| | |
|---|---|
| Time inside requests | 6.78 h |
| Time between requests | 0.43 h (~1.1 s per file, POSTs included) |
| PATCH duration, median | 4.2 s |
| PATCH duration, 90th percentile | 15.5 s |

Most of the time was spent inside requests, at a steady rate rather than in
stalls: a 50 MB PATCH at ~15.5 s is ~26 Mbit/s, receiving and staging
together. So the volunteer's line was the main limit, and parallel files will
save them less than they save a fast line.

**Every file is two PATCHes.** The card averages 55.8 MB a file, and a
ten-minute 48 kHz 16-bit mono WAV is 57.6 MB -- just over the 50,000,000-byte
chunk. So each file is a 50 MB PATCH and a 7.6 MB one, which is why the median
PATCH is the short one. At the big PATCH's rate, 7.6 MB takes ~2.4 s; it took
~4.2 s. The ~2 s difference is the finishing work that only the last PATCH
does: the block-list commit, the file's document, and the recount of all
1,434 files. Over the card that is ~0.8 h, plus a second round of per-PATCH
lookups on every file.

Estimated from these, about a fifth of the 6.78 h was the server: ~2 s of
finishing and ~1-2 s of staging per file. The rest was the line.

*To confirm:* the `patches` count from the same query should be ~2,868 (two
per file). The query in *Checking a card's upload* below splits each upload's
first PATCH from its last.

## Proposal

Send up to three files at once from the browser, so one file's bytes are on
the wire while another's chunk is being staged or finished. The change is all
in `upload-flow.js`; the server already takes it.

**Browser.** `sendWaiting` becomes a small pool: up to `PARALLEL_FILES = 3`
calls to `send()`, each taking the next `waiting` file in card order, so
nights still finish roughly oldest first.

- `inFlight` becomes a set, and `stop()` aborts every upload in it. A paused
  file resumes from its last stored chunk, as today.
- `progress()` already counts per file (`sent - entry.sent`), so the speed and
  totals add up across files unchanged.
- A refused file (400, 413, 422) is still marked failed while the others go
  on. Any other error aborts the rest and interrupts the card, as one file's
  error does now.
- A second and third file start only while the measured speed is above about
  20 Mbit/s. Below that the line is already the limit, and splitting it would
  stretch each 50 MB chunk toward the ingress's 240 s timeout (three chunks at
  5 Mbit/s take 240 s).

**Server.** Nothing is required to change:

- tusd locks per upload, so different files never contend for a lock.
- Each file is its own upload with its own id and blob, so staged blocks never
  mix.
- A PATCH body goes to a temp file, not memory, so three at once is up to
  150 MB of temp disk per volunteer.
- Card updates are replace-if-unchanged with 5 attempts, so files finishing
  together retry rather than lose a write.

## What else it touches

Most of the app already treats files independently; these are the places that
assume one file at a time.

| Area | Today | With parallel files |
|---|---|---|
| File list (`bs-upload-file-list.js`) | Scrolls to the one `sending` row | Several rows show a percentage; it follows the first, which already works |
| Card counts (`tallyFiles`) | One recount at a time | Two recounts can overlap and the older one can write last, so "files in" can briefly step back. The card still reaches `processing` correctly, because a file is marked before its recount lists. CARD-COUNTS.md removes this. |
| Pause, deploy, dropped line | Loses up to one partial chunk | Loses up to three partial chunks, each resumed from its last stored chunk |
| Server temp disk | Up to 50 MB per volunteer | Up to 150 MB per volunteer, which has to fit the replica's ephemeral storage when several cards arrive together |
| Ingress timeout (240 s) | A 50 MB chunk needs above 1.7 Mbit/s | Guarded by the 20 Mbit/s rule, so a slow line stays at one file |

The docs that describe "files go one at a time" change with it: the comment at
the top of `upload-flow.js` and CLAUDE.md, *Card audio goes over tus*.

## Alternatives considered

Parallel files is the smallest change that hides both kinds of idle time;
streaming chunks is the natural follow-up if staging still shows in the logs.

| Option | What it fixes | Cost | Verdict |
|---|---|---|---|
| Parallel files (this proposal) | Hides staging and per-file round trips behind other files | About 50 lines in `upload-flow.js` | Do first |
| A chunk that holds a whole file | Removes the second PATCH on a ten-minute recording (57.6 MB), and its round of lookups | `CHUNK_BYTES` to ~60 MB, which needs above 2 Mbit/s to beat the 240 s ingress timeout | Cheap; decide with the benchmark |
| Streaming `WriteChunk` | Removes store-and-forward: stage 8 MB blocks as they arrive, so one file never waits | Our own `WriteChunk` over tusd's `AzUpload`, and tests against Azurite | Follow-up if staging is still over ~20% of PATCH time |
| tus parallel parts (`parallelUploads`) | Splits one file across requests | Needs the concatenation extension, which is turned off and which the Azure store doesn't offer | No |
| Smaller chunks | Nothing: the idle time per chunk shrinks, but chunks multiply | More round trips | No |
| Direct-to-blob with SAS URLs | Takes the container out of the data path entirely | Card rules move out of tusd's hooks, and dev and prod stop sharing one path (CLAUDE.md, *Revisit when*) | Only if the container's share still costs too much |

## Measure first

What the server logs, so a card's upload can be split into the line and the
server, and each improvement seen or not. All of it is JSON on stdout, so it
reaches Log Analytics as `ContainerAppConsoleLogs_CL.Log_s`.

| Line (`msg`) | Where | Fields | Answers |
|---|---|---|---|
| `request` | `requestLogger` (`cmd/server/main.go`) | `method`, `path`, `status`, `bytes` (request body; a PATCH's chunk), `dur` | Each chunk's size and time, and whether it failed |
| `chunk staged` | `skipEmptyBlob.Upload` (`internal/storage`), card audio only | `blob`, `bytes`, `dur`, `ok` | How much of a PATCH was the server passing it on to Blob Storage; the rest was receiving it |
| `upload committed` | `skipEmptyBlob.Commit` | `blob`, `dur`, `ok` | The block-list commit a file's last PATCH waits on |
| `file received` | `afterFileUpload` (`internal/api/tus.go`) | `card`, `upload`, `counted`, `filesUploaded`, `fileCount`, `dur` | What recording a file cost, and whether it grows with the card (CARD-COUNTS.md) |

`dur` is nanoseconds. Staging and commit lines are Azure's only: the local
store writes as bytes arrive, and has neither.

**Benchmark.** Upload the same ~200-file folder with 1, 2, 3 and 4 files at
once from the gigabit line, and compare the queries below for each. A browser
picks its own number with `localStorage.setItem("birdsense.upload.parallelFiles", "4")`
in the console (1-8; remove it to go back to 3), so the runs need no deploys.
Keep 3 unless 4 is clearly better. One more run with the browser's network
throttling at 10 Mbit/s checks that a slow line stays at one file.

### Checking a card's upload

In the portal: open the `log-birdsense-prod` Log Analytics workspace, then
**Logs**, switch to **KQL mode**, set **Time range** to "Set in query", and put
the card's reference (as the app shows it) where the queries say
`OWL-REPLACE-ME`. Logs are kept 30 days. POSTs go to `/api/v1/tus/` before the
upload has an id, so their time lands in the gaps.

**Where the time went**, and whether files overlapped. `files_at_once` is the
PATCH time over the span of the upload: ~1 is one file at a time, and near 3
means parallel files kicked in. `mbit_s` is the card's rate through us.

```kusto
ContainerAppConsoleLogs_CL
| where TimeGenerated > ago(30d)
| extend l = parse_json(Log_s)
| where tostring(l.msg) == "request"
    and tostring(l.path) startswith "/api/v1/tus/OWL-REPLACE-ME/"
| extend method = tostring(l.method), dur_s = tolong(l.dur) / 1e9, bytes = tolong(l.bytes)
| extend started = datetime_add('millisecond', -tolong(dur_s * 1000), TimeGenerated)
| sort by started asc
| extend gap_s = datetime_diff('millisecond', started, prev(TimeGenerated)) / 1000.0
| summarize first = min(started), last = max(TimeGenerated),
            patches = countif(method == "PATCH"), failed = countif(toint(l.status) >= 400),
            in_requests_h = sum(dur_s) / 3600,
            between_requests_h = sumif(gap_s, gap_s > 0) / 3600,
            patch_gb = sumif(bytes, method == "PATCH") / 1e9
| extend span_s = datetime_diff('second', last, first)
| extend files_at_once = in_requests_h * 3600 / span_s,
         mbit_s = patch_gb * 8e3 / span_s
```

**Receiving against staging.** `staging_share` is the part of PATCH time the
server spent passing chunks on; the rest was the volunteer's bytes arriving.
A high share is what streaming `WriteChunk` would fix (*Alternatives*).

```kusto
let ref = "OWL-REPLACE-ME";
ContainerAppConsoleLogs_CL
| where TimeGenerated > ago(30d)
| extend l = parse_json(Log_s)
| extend msg = tostring(l.msg), dur_s = tolong(l.dur) / 1e9
| where (msg == "request" and tostring(l.method) == "PATCH"
         and tostring(l.path) startswith strcat("/api/v1/tus/", ref, "/"))
     or (msg in ("chunk staged", "upload committed")
         and tostring(l.blob) startswith strcat("uploads/", ref, "/"))
| summarize patch_s = sumif(dur_s, msg == "request"),
            staged_s = sumif(dur_s, msg == "chunk staged"),
            committed_s = sumif(dur_s, msg == "upload committed"),
            stage_mbit_s = sumif(tolong(l.bytes), msg == "chunk staged") * 8 / 1e6
                           / sumif(dur_s, msg == "chunk staged")
| extend staging_share = (staged_s + committed_s) / patch_s
```

**Recording each file**, bucketed by how far into the card it was. The time
should be flat from the first tenth to the last; one that climbs with
`filesUploaded` is a recount per file come back (CARD-COUNTS.md).

```kusto
ContainerAppConsoleLogs_CL
| where TimeGenerated > ago(30d)
| extend l = parse_json(Log_s)
| where tostring(l.msg) == "file received" and tostring(l.card) == "OWL-REPLACE-ME"
| extend dur_ms = tolong(l.dur) / 1e6,
         tenth = toint(10 * toint(l.filesUploaded) / max_of(toint(l.fileCount), 1))
| summarize files = count(), p50_ms = percentile(dur_ms, 50), p90_ms = percentile(dur_ms, 90) by tenth
| sort by tenth asc
```

A card uploaded before these lines existed still has the `request` lines'
method, path and duration; the first query works on it without `failed`,
`patch_gb` and `mbit_s`.

## Open questions

- What upload speed does the reporting volunteer's line test at? The log says
  ~26 Mbit/s through us; a speed test near 30 would put nearly all of the
  rest on the line.
- How much ephemeral storage does the 0.5 vCPU web replica get? It has to hold
  three 50 MB temp files per volunteer uploading at once.

## Status

| Step | State |
|---|---|
| Existing logs read on the reported card | Done 2026-09-28: 6.78 h in requests, 0.43 h between, bandwidth-bound (*Measured*) |
| Logging: request status and bytes, staging and commit times, each file's finish | Done 2026-09-28 |
| Card-count counter (CARD-COUNTS.md), so overlapping finishes can't step the count back | Done 2026-09-28 |
| Pool in `upload-flow.js`: `PARALLEL_FILES` (3, or a browser's own for measuring) and the 20 Mbit/s guard | Done 2026-09-28; checked against a stubbed tus client (pause and resume, a quick pause and resume, a refused file, a server error), and in headless Chromium against the dev server: at 100 Mbit/s 16 files went three at once (log: 2.77 at once, 98.7 Mbit/s), at 8 Mbit/s 6 files went one at a time (0.98, 7.8) |
| Deploy; upload a real card and read *Checking a card's upload* | Not started |
| Benchmark; set `PARALLEL_FILES` and decide `CHUNK_BYTES` | Not started |
