# Analysis off the web app — design

Moving BirdNET and Perch out of the web container into a Container Apps job
that packs both models onto each replica, and recording enough about every run
to size that replica from measurements rather than guesses.

Nothing here is built yet. The **Status** table at the end is where to resume;
update it as milestones land. Where this and the code disagree, the code is
what runs -- fix whichever is wrong.

## Why

Today `internal/analysis` runs inside the web app (CLAUDE.md, *The analysis
queue is the database*), one file at a time. That ties three things together
that don't belong together:

- **The web app is sized for Perch.** Serving pages and tus needs ~0.25 vCPU;
  Perch needs 2 vCPU / 4 GiB (DEPLOYMENT.md, *Perch*), so with it on the
  always-on replica is eight times the size the site needs.
- **A card is slow.** One file at a time on one replica: a card takes most of a
  day with BirdNET alone and about a day and a half with Perch, and new cards
  queue behind it.
- **The probes are loose on purpose** because a health response waits behind
  hours of BirdNET (DEPLOYMENT.md, *Liveness and readiness*).

The work itself, from the measurements in DEPLOYMENT.md (one worker, 10 minutes
of audio: BirdNET 25 s, Perch 73 s) and **assuming ~16 hours of audio per
recorder-day** (5.5 GB/day as 48 kHz 16-bit mono -- the recorder settings decide
this, so check it):

| | BirdNET | Perch | Both |
|---|---|---|---|
| CPU per hour of audio | ~150 s | ~440 s | ~590 s |
| One card (~336 files, ~370 h of audio) | ~15 vCPU-h | ~45 vCPU-h | **~60 vCPU-h** |
| Five recorders, a month | ~100 vCPU-h | ~300 vCPU-h | **~400 vCPU-h** |
| Peak memory per process tree | ~0.3 GB | ~2.5 GB | |

Consumption billing is per vCPU-second and GiB-second, so **parallelism is
free**: twenty replicas for three hours cost what one costs for sixty. The
saving from getting analysis off the always-on replica is modest (an idle
replica bills at a reduced rate); the real gains are a card finishing in hours,
and a web app that stops being sized for Perch.

## Decisions

**A Container Apps job, not Azure Batch -- for now.** Batch on spot VMs is
several times cheaper per vCPU-hour, but at ~400 vCPU-hours a month the
difference is ~$30/month, against a second compute platform with its own pool,
image, autoscale formula, eviction handling and spot quota. The worker below is
a container that claims files from Cosmos and exits when there are none, which
runs unchanged on either; moving to Batch later is a new launcher, not a
redesign. *Revisit when:* a month of real bills (below) puts the analysis line
where $30+/month matters, or the measurements say we need a VM shape Container
Apps doesn't offer.

**One replica runs both models, packed by memory.** Rather than a BirdNET job
and a Perch job, each replica runs a scheduler that starts as many analysis
processes as fit, mixing the two. The models are opposite shapes -- Perch is
limited by memory (~2.5 GB per process), BirdNET by CPU (~0.3 GB) -- so on one
box BirdNET fills the cores Perch's memory leaves idle. It also means one
download of a file serves both models (below).

**Memory is a hard budget; CPU is a target.** Running more processes than
cores only slows them down; running more than fit in memory gets the replica
OOM-killed and every file on it restarted. So the scheduler admits a task only
if its memory estimate fits under the container's limit minus headroom, and
treats cores as a soft limit: it aims to keep them all busy.

**The queue stays the database.** No Storage Queue or Service Bus. A replica
claims a file by moving its step to `analyzing` with a replace-if-unchanged
update -- the Store's mutate funcs already do this race-free -- and records a
lease. What changes is that "found in `analyzing`" no longer means "cut off by
a restart", because other replicas are running. See *Claims and leases*.

**BirdNET still comes first.** Today's rule -- a new card's first opinion
never waits behind an old card's second (CLAUDE.md, *Perch is a second
opinion*) -- becomes the scheduler's priority: when a slot opens, a BirdNET
task anywhere in the queue beats a Perch task. Within a model, oldest card
first, as now.

**Every run records how it used the box.** See *Performance data*. The sizes
in this document are estimates from one 10-minute clip on one machine; the
point of M0 is that the first real cards replace them.

## Where the Consumption plan boxes us in

These are Azure's rules, not ours (verify against current docs before M3):

- A Consumption replica is at most **4 vCPU / 8 GiB**, and memory is always
  **2 GiB per vCPU**. There is no 16-vCPU replica and no buying less RAM per
  core.
- That ratio is slightly *short* for Perch run single-threaded: 2.5 GB for one
  core is more than 2 GiB per core. On 4 vCPU / 8 GiB with ~1 GiB of headroom,
  two Perch processes fit and the other two cores go to BirdNET. Once a card's
  BirdNET is done, half the replica idles unless Perch can use more than one
  thread per process. **Whether Perch scales across threads decides the
  replica shape**, which is why M0 measures it first:
  - If it does, give each Perch process 2 threads and the 4/8 replica is full.
  - If it doesn't, Perch wants *more* RAM per core, not less: a Dedicated
    workload profile (D-series, 4 GiB/vCPU) or Batch. The Dedicated profile
    needs a workload-profiles environment, which means recreating
    `cae-birdsense-prod` and the app in it -- a planned migration, not a flag.
- A job has **one trigger type**: manual, schedule, or event. See *Starting
  the job*.

## Architecture

```
 browser ──tus──▶ Web app  (Container App: 0.25–0.5 vCPU, min 1 / max 1 replica)
                   │  API, tusd, retention sweep; Alpine image, no Python
                   │  card's last file lands → start an execution (managed identity)
                   │  every few minutes: work queued and nothing running → start one
                   ▼
          Job "analyze"  (Container Apps job, manual trigger; analyzer image)
            execution: parallelism N, each replica 4 vCPU / 8 GiB
            replica:  check models → loop { claim, pack, run } → exit 0 when idle
                   │                          │
     Cosmos: uploads, audioFiles,      Blob: audio/uploads/ (read)
     detections, analysisStatus        audio/clips/ (write), perf/ (write)
```

### The two images

`Dockerfile` gets two targets from the same stages:

- **web** -- the server binary and `frontend/` on Alpine. No Python, no models;
  back to tens of MB, so new revisions pull fast.
- **analyzer** -- a new `cmd/worker` binary, the venv with BirdNET and
  TensorFlow, the models. The ~2 GB image `birdsense` is today.

`scripts/deploy.ps1` builds both at the same git sha, and Terraform applies
both tags together, so a web app and a worker never disagree about a document
shape. ROLLBACK.md gains a line: roll both back together.

### Starting the job

The job's trigger is **manual**. The web app starts executions through the ARM
API (`Microsoft.App/jobs/start/action`) with its managed identity, in two
places:

1. **When a card's last file lands**, where `Enqueue` is called today.
2. **As a backstop**, on a ticker in the web app (every ~5 minutes): if any card
   is in `processing` with claimable work and no execution is running, start
   one. This covers a failed start, a replica that died, and cards that were
   waiting when the job was deployed.

That is why the web app keeps `min_replicas = 1`: the backstop and the retention
sweep both live in it. At 0.25 vCPU / 0.5 GiB and mostly idle that is a few
dollars a month. (DEPLOYMENT.md notes 0.25 vCPU may limit upload speed; measure
a real card upload before settling on 0.25 vs 0.5.)

An execution's replica count is chosen when it is started, capped by the
`analysis_max_replicas` variable: roughly the queued vCPU-hours divided by
the hours a card should take, so one late file doesn't start ten replicas. A
second start while one is running is fine -- the claims keep them from
duplicating work -- but the web app avoids it to keep the replica count honest.

**Why not an event or schedule trigger?** An event trigger needs a queue for
KEDA to watch, and the queue here is Cosmos. A schedule trigger would pull the
2 GB image every few minutes to find nothing to do. Manual plus a backstop in
a process that is always up anyway costs neither.

### One replica's life

1. **Check.** Run `Check` (and `CheckPerch` if Perch is on). If the models
   can't run, write that to the status document (below), and exit non-zero --
   the job's failed-execution alert fires, rather than one warning in a log.
2. **Read its limits** from cgroup v2 (`cpu.max`, `memory.max`) rather than
   from configuration, so a Terraform size change can't disagree with what the
   scheduler thinks it has. Environment variables can lower them.
3. **Loop:** while the memory budget has room for the cheapest queued task,
   claim one and start it. When a task finishes, loop. When nothing is
   claimable and nothing is running, **exit 0** after a short grace period (~2
   minutes, for the next file of a card still landing).

A task is one model over one file: download (or reuse, below), analyze, merge,
cut clips, upsert detections, mark the step done, recount the card. That is
`Queue.run` / `analyzeFile` / `perchFile` as they are today, called
concurrently instead of in a loop.

### Packing

The scheduler keeps a per-model estimate `{memory, threads}`, with defaults
from M0's measurements and overrides in environment variables so tuning is a
Terraform variable, not a rebuild:

```
BIRDSENSE_ANALYSIS_MEM_HEADROOM    default 1 GiB   kept free for Go, clips, page cache
BIRDSENSE_ANALYSIS_BIRDNET_MEM     default 0.4 GB  peak per task, with margin
BIRDSENSE_ANALYSIS_BIRDNET_THREADS default 1
BIRDSENSE_ANALYSIS_PERCH_MEM       default 2.8 GB
BIRDSENSE_ANALYSIS_PERCH_THREADS   default 1       raised if M0 shows it scales
BIRDSENSE_ANALYSIS_MAX_TASKS       default = cores
```

When a slot opens:

1. Take the oldest claimable BirdNET file, if its memory fits.
2. Otherwise the oldest claimable Perch file, if it fits -- preferring one
   whose audio this replica already holds.
3. Give it `min(threads setting, free cores)` threads, at least 1. Threads are
   fixed when the process starts, so this is where a replica that has run out
   of BirdNET work hands its spare cores to Perch.

**Sharing a download.** Today each step copies the file out of storage into
its own temp directory. On a packed replica, a file's BirdNET and Perch steps
are often both queued; the worker keeps a small local cache (reference-counted,
removed when neither step still needs it) so one download serves both. The
temp directory is swept at startup (TODO.md already notes orphaned ones).

### Claims and leases

Each step (the file's own status for BirdNET, `perch` for Perch) gains:

| Field | Meaning |
|---|---|
| `claimedBy` | the replica holding the claim (execution and replica name) |
| `leaseUntil` | when the claim lapses if not renewed |
| `attempts` | failures so far, stored rather than in memory (`Queue.attempts` is in memory today, which a fleet of short-lived replicas would reset every time) |

- Claiming is `uploaded`/`queued` → `analyzing` with `claimedBy` and
  `leaseUntil = now + 5 min`, replace-if-unchanged. Losing the race means
  another replica has it; try the next file.
- A running task renews its lease every minute. Renewal failing twice in a row
  kills the task: someone else may already have it.
- An `analyzing` step whose lease has lapsed is claimable, which replaces
  today's "found `analyzing` at startup means cut off". A file whose worker was
  OOM-killed comes back this way, and counts an attempt.
- `maxAttempts` and the retry delays keep their meaning, now per stored
  `attempts`. The retry delay becomes "not claimable before" (`retryAfter`)
  rather than a sleep.
- Files stored before this change have none of the fields; absent means
  unclaimed. The in-process queue (dev) uses the same claims with itself as the
  only claimant, so there is one code path.

These are stored fields, so they change in `internal/db/models.go` and
SCHEMA.md together.

### Where the queue's status lives

`Queue.Status` is in-process state today, served on `/health` and the admin
card routes. With the work in another process it becomes a document the
workers write (CLAUDE.md already says so under *A stuck card says why*): one
`analysisStatus` document with the last check result, the running execution
and its replicas, and the last failure. The web app reads it where it calls
`Queue.Status()` now. It lives in an existing container rather than a new one;
which one is SCHEMA.md's call when M3 lands.

### Dev and compose

`BIRDSENSE_ANALYSIS=inprocess|job` picks the launcher. It defaults to
`inprocess` with `BIRDSENSE_DB=local`, where the server runs the same worker
loop in a goroutine, packed against the same cgroup limits (or the host's) --
so dev exercises the scheduler and the claims, and needs no job. Compose keeps
one container with the analyzer image. Outside dev it defaults to `job`.

## Performance data

The goal: after any real card, you can download one set of files, hand them
over, and we can answer *what size of replica, how many of them, and how many
threads per model*.

### What is recorded

Each worker writes **JSON Lines**, one file per replica, three record types:

**`run`** -- once at start:
execution and replica name, image tag, cgroup limits (`cpu.max`,
`memory.max`), CPU model (`/proc/cpuinfo`), the scheduler's settings
(every `BIRDSENSE_ANALYSIS_*` value), Perch on or off.

**`sample`** -- every 5 seconds, the whole container and each running task:

```json
{"type":"sample","t":"2026-10-03T14:02:05Z",
 "cpu":{"usedCores":3.71,"limitCores":4,"throttledMs":120},
 "mem":{"current":6.42e9,"anon":5.90e9,"file":0.48e9,"limit":8.59e9,"oomKills":0},
 "tasks":[{"id":"…","model":"perch","threads":1,"cores":0.98,"pss":2.61e9},
          {"id":"…","model":"birdnet","threads":1,"cores":1.02,"pss":0.31e9}]}
```

Container figures come from cgroup v2 (`cpu.stat`, `memory.current`,
`memory.stat`, `memory.events`). Per-task figures sum the task's process
group from `/proc`: CPU from `stat`, memory as **PSS** from `smaps_rollup`,
not RSS -- the birdnet package's worker processes share pages with their
parent, and RSS counts those twice. `anon` vs `file` separates real memory from
page cache, which the kernel reclaims before it OOM-kills.

**`task`** -- once per finished task:

```json
{"type":"task","model":"perch","card":"…","file":"…","threads":1,
 "audioSec":3600,"bytes":345600000,"result":"analyzed","detections":212,
 "phases":{"queuedSec":840,"downloadSec":6.1,"analyzeSec":437,"clipSec":22.4,"storeSec":3.0},
 "cpuSec":431,"peakPss":2.63e9,
 "runningAtStart":{"birdnet":2,"perch":1}}
```

`audioSec / cpuSec` is the number that matters most: how much audio a core
gets through per model, at that thread count, while sharing the box. `phases`
says whether a file is compute-bound or waiting on storage.

### Where it goes

- **Blob storage**, container `perf`, at
  `perf/{yyyy-mm-dd}/{execution}/{replica}.jsonl`, as an **append blob**
  flushed every 30 seconds, so a killed replica loses at most 30 seconds of it.
  In dev, the same layout under `backend/data/perf/`. About 200 KB per replica
  per hour; a lifecycle rule deletes it after 180 days.
- **Log Analytics**, for the `task` records only, as a structured log line
  (`msg="analysis: task"`), so a quick KQL query answers "how long is Perch
  taking this week" without downloading anything.

### Bringing it back

```sh
az storage blob download-batch --auth-mode login \
  --account-name <account> -s perf -d ./perf --pattern '2026-10-0*'
```

(`grant_operator_blob_access` already gives the operator read access.) Then
either hand over the `.jsonl` files as they are, or run the summary first:

```sh
cd backend && go run ./cmd/perf ../perf
```

`cmd/perf` prints, per model and thread count: audio per CPU-second (median and
p90), peak PSS (median, p95, max), phase breakdown; per replica: CPU
utilisation as a histogram over time, peak memory against the limit, OOM kills,
and **the tail** -- minutes at the end of an execution with cores idle while
the replica was still billed. It prints text you can paste, and the raw files
are there when the summary doesn't answer the question.

### The benchmark

Production data shows the packed replica as it runs; it can't show a thread
count we haven't deployed. So `cmd/analyze` gains a `-bench` mode:

```sh
go run ./cmd/analyze -bench -model perch -threads 1,2,4,8 card-file.wav
```

It runs the file once per thread count, one process at a time, with the same
sampler, and writes the same `task` and `sample` records to a local file.
`analyze.py` gains `--threads` for this (TensorFlow's intra/inter-op threads for
Perch, LiteRT's for BirdNET; which knobs the `birdnet` package actually honours
is part of M0). Run it on a real hour-long card file, not the Osprey clip: model
loading dominates a short clip.

### What the numbers decide

| Question | Read it from |
|---|---|
| Does Perch use more than one thread well? | bench: `audioSec/cpuSec` flat as threads rise = yes |
| What memory to budget per model | `task.peakPss`, p95 plus margin |
| Is the replica full? | `sample.cpu.usedCores / limitCores` over the execution |
| Is memory, not CPU, what stops it packing more? | CPU below the limit while `mem.anon` is near the budget |
| Consumption 4/8, Dedicated D-series, or Batch | the two rows above, together |
| How many replicas per execution | the tail, and vCPU-hours per card |
| Is storage a bottleneck? | `phases.downloadSec` against `analyzeSec` |

## Cost

List prices, pay-as-you-go, rough -- check the calculator. Consumption bills
~$0.086 per vCPU-hour and ~$0.011 per GiB-hour while active, so a 2 GiB/vCPU
replica is **~$0.11 per vCPU-hour**, less a monthly free grant.

| | Today, Perch on | This design |
|---|---|---|
| Web app | 2 vCPU / 4 GiB, always on, busy a few hours a day: **~$75/month** | 0.25 vCPU / 0.5 GiB, mostly idle: **~$5/month** |
| Analysis | included above | ~400 vCPU-h × $0.11 ≈ **$45/month** fully packed; up to ~$60 if Perch leaves cores idle |
| One card | ~1.5 days | ~2–3 hours at 10 replicas of 4 vCPU |

The gap between $45 and $60 is what the packing and the thread count are for.
Batch on spot VMs would bring the analysis line to roughly $5–15.

## Milestones

**M0 -- Measure, in today's architecture.** The sampler, the `run`/`sample`/
`task` records, the `perf` container and lifecycle rule, `cmd/analyze -bench`,
`analyze.py --threads`, `cmd/perf`. This runs inside today's in-process queue,
so the next real card in production produces data before anything else
changes. Then: bench Perch and BirdNET at 1/2/4/8 threads on a real card file.

**M1 -- Claims, leases, stored attempts.** Schema fields and SCHEMA.md,
`retryAfter`, the lease-renewal loop. Still in-process and still one task at a
time; the existing tests keep passing, plus two workers racing over one card in
the JSON backend.

**M2 -- The packing scheduler.** Memory budget from cgroups, per-model
estimates from M0, BirdNET-first, thread handoff, the shared download cache.
In-process, so it can run in the current web app with Perch on and be measured
there.

**M3 -- The job.** `cmd/worker`, the two Dockerfile targets, `deploy.ps1`
building both, the `analysisStatus` document, the web app's starter and
backstop, Terraform (job, roles including `jobs/start/action` for the web
identity, `analysis_cpu`, `analysis_memory`, `analysis_max_replicas`, the
`BIRDSENSE_ANALYSIS_*` knobs), a failed-execution alert, the web app back to
0.25–0.5 vCPU, and DEPLOYMENT.md, CLAUDE.md and ROLLBACK.md updated.

**M4 -- Tune.** A month of real cards, then the table in *What the numbers
decide*: settle the per-model estimates and threads, the replica count, and
whether Consumption 4/8 is the right shape or it is time for a Dedicated
profile or Batch.

## To verify before M3

- Consumption replica limits (4 vCPU / 8 GiB, fixed 2:1) and the ephemeral
  storage a 4-vCPU replica gets -- a few concurrent hour-long files plus their
  clips have to fit.
- Job limits: maximum replica timeout, the parallelism cap, whether a manual
  start can override parallelism per execution, and the environment's
  Consumption core quota.
- How long the 2 GB analyzer image takes to pull on a cold Consumption
  replica.
- The built-in role, or the custom one, that grants the web identity
  `Microsoft.App/jobs/start/action` and nothing more.
- Whether Container Apps jobs expose per-replica CPU/memory metrics in Azure
  Monitor. If they do, they are a cross-check on the sampler, not a
  replacement: they can't say which model used what.

## Status

| Milestone | State | Notes |
|---|---|---|
| M0 Measure | not started | |
| M1 Claims and leases | not started | |
| M2 Packing scheduler | not started | |
| M3 The job | not started | |
| M4 Tune | not started | |
