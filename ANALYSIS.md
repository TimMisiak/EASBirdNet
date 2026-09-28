# Analysis off the web app — design

Moving BirdNET and Perch out of the web container into a Container Apps job
that packs both models onto each replica, and recording enough about every run
to size that replica from measurements rather than guesses.

M0 (measuring) is built and has run on a real card; the rest isn't built
yet. The **Status** table at the end is where to resume; update it as
milestones land. Where this and the code disagree, the code is what runs --
fix whichever is wrong.

## Why

Today `internal/analysis` runs inside the web app (CLAUDE.md, *The analysis
queue is the database*), one file at a time. That ties three things together
that don't belong together:

- **The web app is sized for Perch.** Serving pages and tus needs ~0.25 vCPU;
  Perch needs 2 vCPU / 4 GiB (DEPLOYMENT.md, *Perch*), so with it on the
  always-on replica is eight times the size the site needs.
- **A card is slow.** One file at a time on one replica: measured, a card's
  BirdNET takes about half a day and its Perch about four more (below), and
  new cards queue behind it.
- **The probes are loose on purpose** because a health response waits behind
  hours of BirdNET (DEPLOYMENT.md, *Liveness and readiness*).

### Measured

The first real card with Perch on (image `7495b6a`, 2 vCPU / 4 GiB replica on
a Xeon Platinum 8370C, ~15-minute files; 38 BirdNET and 5 Perch tasks, with
the slowest tenth within a few percent of the median for both):

| | BirdNET | Perch |
|---|---|---|
| Audio per CPU-second | 33.7 s | 1.99 s |
| **CPU per hour of audio** | **~107 s** | **~1,810 s** (~0.5 vCPU-h) |
| Cores in use | 0.96 -- LiteRT is single-threaded | 1.92 -- TensorFlow took both |
| Speed on that replica | 31.5x real time | 3.8x real time |
| Peak memory (PSS) | 261 MB | 1.95 GB |
| Wall time outside the model | store 9%, clip 3%, download 2% | ~1% |

**Perch is ~17x BirdNET's CPU, not the ~3x the 10-minute test clip
suggested**, and it is the whole cost of analysis. Scaled up, still
**assuming ~16 hours of audio per recorder-day** (5.5 GB/day as 48 kHz 16-bit
mono -- the `audioSec` totals in the records will say what it really is):

| | BirdNET | Perch | Both |
|---|---|---|---|
| One card (~370 h of audio) | ~11 vCPU-h | ~186 vCPU-h | **~197 vCPU-h** |
| One recorder, a month | ~14 vCPU-h | ~240 vCPU-h | ~255 vCPU-h |
| Five recorders, a month | ~71 vCPU-h | ~1,200 vCPU-h | **~1,280 vCPU-h** |

Consumption billing is per vCPU-second and GiB-second, so **parallelism is
free**: twenty replicas for ten hours cost what one costs for two hundred.
Moving analysis off the always-on replica makes a card take hours instead of
days, and the web app stops being sized for Perch; what it costs is set by how
much audio Perch is run over (*Open: how much audio Perch hears*).

## Decisions

**A Container Apps job, not Azure Batch -- for now.** Batch on spot VMs is
several times cheaper per vCPU-hour. At the ~400 vCPU-hours a month first
estimated, the difference was ~$30/month, not worth a second compute platform
with its own pool, image, autoscale formula, eviction handling and spot quota.
The worker below is a container that claims files from Cosmos and exits when
there are none, which runs unchanged on either; moving to Batch later is a new
launcher, not a redesign. *Revisit when:* the analysis line is where
$30+/month matters. **Measured, Perch over every file at five recorders puts
the difference near $125/month -- this trigger is met** unless Perch runs over
less audio (below). Decide that first: it changes the bill by more than the
platform does.

**One replica runs both models, packed by memory.** Rather than a BirdNET job
and a Perch job, each replica runs a scheduler that starts as many analysis
processes as fit, mixing the two. The models are opposite shapes -- Perch is
heavy in both CPU and memory (~2 GB a process), BirdNET is light in both
(~0.26 GB) -- so on one box BirdNET fills whatever Perch leaves. It also means
one download of a file serves both models (below).

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

**Every run records how it used the box.** See *Performance data*. The
figures above come from it; the per-model estimates the scheduler uses should
too, and be revised when the records say they've moved.

### Open: how much audio Perch hears

Perch is a second opinion on BirdNET, and it costs ~17x BirdNET to run over
every second of every file. The largest saving available is running it over
less: only the stretches where BirdNET heard something (with a margin), or
only BirdNET's low-confidence ones. How much that saves depends on how much
of a night BirdNET's detections cover -- ~4,000 detections a recorder-day, but
merged and overlapping -- and what Perch hears outside those stretches.
`cmd/perchcover` measures both on real card files, locally: it runs both
models over them, then reports the share of the audio Perch would still hear
running only around BirdNET's detections (all of them, birds only, or only
low-confidence birds; with 0, 2 and 5 s of margin, on Perch's 5 s windows),
and the share of Perch's detections -- and which species -- that would fall
outside it:

```sh
cd backend && BIRDSENSE_BIRDNET_PYTHON=../.venv/bin/python \
  go run ./cmd/perchcover -lat LAT -lon LON -date YYYY-MM-DD -out cover.json FILES...
go run ./cmd/perchcover -in cover.json -v     # report again, without re-running
```

This is a product decision as much as a cost one: a second opinion only over
what BirdNET heard can't find what BirdNET missed, and the report's "cost"
columns are exactly that.

## Where the Consumption plan boxes us in

These are Azure's rules, not ours (verify against current docs before M3):

- A Consumption replica is at most **4 vCPU / 8 GiB**, and memory is always
  **2 GiB per vCPU**. There is no 16-vCPU replica and no buying less RAM per
  core.
- That ratio fits Perch: 1.95 GB measured, single process. On 4 vCPU / 8 GiB
  with ~1 GiB of headroom, three Perch processes fit, and BirdNET's 0.26 GB
  fits beside them. **Whether Perch uses a core better on one thread or
  several decides how many Perch processes a replica should run**: it used
  both of the replica's cores on TensorFlow's default threading, which the
  benchmark (`-threads 1,2,4`) has yet to compare against one thread each.
  - If one thread is as efficient per CPU-second, run three or four
    single-threaded Perch processes on a 4/8 replica.
  - If several threads are better, fewer processes with more threads each --
    and memory is then spare, not short.
- Container Apps doesn't let a replica read its own limits: in production
  there was no cgroup to read, and the process saw the 4-CPU, 5.9 GB machine
  under it rather than its 2 vCPU / 4 GiB. So the replica's size is passed in
  (`BIRDSENSE_REPLICA_CPU`, `BIRDSENSE_REPLICA_MEMORY`), and the scheduler
  must budget from those, not from what it can see.
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
2. **Read its limits**: the container's own (cgroup v2 or v1) where it can see
   them, and otherwise the size Terraform passes in (`BIRDSENSE_REPLICA_CPU`,
   `BIRDSENSE_REPLICA_MEMORY`) -- which is what production has, since a
   Container Apps replica can't see its own (*Where the Consumption plan boxes
   us in*). `internal/perf` already resolves them this way, and says which it
   used (`limitsFrom`). Environment variables can lower them.
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
BIRDSENSE_ANALYSIS_BIRDNET_MEM     default 0.35 GB peak per task (measured 0.26), with margin
BIRDSENSE_ANALYSIS_BIRDNET_WORKERS default 1       LiteRT is single-threaded; see *The benchmark*
BIRDSENSE_ANALYSIS_PERCH_MEM       default 2.3 GB  measured 1.95, with margin
BIRDSENSE_ANALYSIS_PERCH_THREADS   default 1       raised if M0 shows it scales
BIRDSENSE_ANALYSIS_MAX_TASKS       default = cores
```

When a slot opens:

1. Take the oldest claimable BirdNET file, if its memory fits.
2. Otherwise the oldest claimable Perch file, if it fits -- preferring one
   whose audio this replica already holds.
3. Give a Perch task `min(threads setting, free cores)` threads, at least 1.
   Threads are fixed when the process starts, so this is where a replica that
   has run out of BirdNET work hands its spare cores to Perch. (BirdNET's
   interpreter is single-threaded, so a BirdNET task only ever takes more
   cores as more workers, each costing its own model's memory.)

**Sharing a download.** Today each step copies the file out of storage into
its own temp directory. On a packed replica, a file's BirdNET and Perch steps
are often both queued; the worker keeps a small local cache (reference-counted,
removed when neither step still needs it) so one download serves both. The
temp directory is swept at startup (TODO.md already notes orphaned ones).

### Claims and leases

Built in M1 (`internal/analysis`: `take`, `hold`, `release`; SCHEMA.md,
*Claims*). Each step -- the file's own status for BirdNET, `perch` for Perch --
carries a `db.Claim`, whose fields sit beside the step's status:

| Field | Meaning |
|---|---|
| `claimedBy` | the worker holding the step: its process (`perf.Instance()` and pid), which is the replica in Azure |
| `leaseUntil` | when the claim lapses if not renewed |
| `attempts` | failed tries so far, stored rather than in memory, so a fleet of short-lived replicas can't reset it |

- Claiming is `uploaded`/`queued` → `analyzing` with `claimedBy` and
  `leaseUntil = now + 5 min`, in one replace-if-unchanged write (both
  backends re-run the mutation on a conflict). Losing the race means another
  worker has it; the queue moves on to the next file.
- A running step renews its lease every minute. If the claim turns out to be
  someone else's, or renewing fails twice in a row, the run is cancelled and
  the file left to whoever has it, with no attempt counted.
- An `analyzing` step whose lease has lapsed is claimable, and taking it
  counts an attempt: a file whose worker was OOM-killed comes back this way,
  and one that keeps killing its worker fails on the third. One with no
  `claimedBy` is from before claims -- cut off by a restart, as it always was
  -- and is taken without counting.
- A worker that is stopped (a deploy, a scale-in) lets go of its file, so the
  next start takes it up at once rather than waiting out the lease, and that
  isn't counted either.
- The in-process queue claims the same way, as the only claimant, so there is
  one code path. When the only queued files are held by someone else, `Run`
  looks again when the soonest lease lapses; nothing else would wake it.
- **Still to come (M2): `retryAfter`.** `maxAttempts` counts per stored
  `attempts`, but the 30-second retry spacing is still `Run`'s pause after a
  failed pass, which is right for one worker and wrong for several -- one
  replica pausing shouldn't be what spaces another's retries. M2 makes it a
  "not claimable before" on the step.

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

`internal/perf` writes **JSON Lines**, three record types. They are written by
today's in-process queue already, so production data starts with the next
real card (M0), and the worker will write the same records in M3.

**`run`** -- when the process starts, and again at the top of every segment
(below), so any one file says what it measured: the instance (the Container
Apps replica name, or the hostname), the image tag (`BIRDSENSE_IMAGE_TAG`), the
CPU model, the cgroup limits (`cpu.max`, `memory.max`, or the host's when there
are none), and the analysis settings (Perch on or off; the scheduler's once
there is one).

**`sample`** -- every 5 seconds while anything is being analyzed, and once
more when it stops; an idle server writes nothing:

```json
{"type":"sample","t":"2026-10-03T14:02:05Z",
 "cpu":{"usedCores":3.71,"limitCores":4,"throttledMs":120},
 "mem":{"current":6.42e9,"anon":5.90e9,"file":0.48e9,"limit":8.59e9,"oomKills":0},
 "tasks":[{"id":"…","model":"perch","phase":"analyze","cores":0.98,"pss":2.61e9,"procs":3},
          {"id":"…","model":"birdnet","phase":"clip","cores":1.02,"pss":0.31e9,"procs":1}]}
```

Container figures come from cgroup v2 (`cpu.stat`, `memory.current`,
`memory.stat`, `memory.events`), else cgroup v1, else the whole machine from
`/proc/stat` and `/proc/meminfo` -- which is what Container Apps gives, where
the machine is a small VM around the replica; the `run` record's `source` says
which. The limits are the cgroup's, else the replica size Terraform passes in,
else the machine's (`limitsFrom`). A task's figures sum its scripts' process
groups from `/proc` -- `internal/birdnet` starts each script in a group of its
own and reports it through `birdnet.WithObserver` -- with CPU from `stat` and
memory as **PSS** from `smaps_rollup`, not RSS: birdnet's worker processes
share pages with the script, and RSS counts those twice. `anon` vs `file`
separates real memory from page cache, which the kernel reclaims before it
OOM-kills.

**`task`** -- once per model's pass over a file:

```json
{"type":"task","t":"…","id":"…","model":"perch","card":"…","file":"…","path":"DATA/…/x.WAV",
 "workers":1,"threads":0,"result":"ok","audioSec":3600,"bytes":345600000,"detections":212,
 "waitSec":840,"wallSec":468.3,
 "phases":{"download":6.1,"analyze":437,"clip":22.4,"store":2.8},
 "cpuSec":431,"peakPss":2.63e9,"maxRss":2.1e9,
 "runningAtStart":{"birdnet":2,"perch":1}}
```

`cpuSec` and `maxRss` are the kernel's own accounting of each script when it
exits (`wait4`'s rusage, which includes the workers the script joined), so
they are exact where the samples are not; `peakPss` is the highest a sample
saw, and a task shorter than one interval has none. `audioSec / cpuSec` is the
number that matters most: how much audio a core gets through, per model and
setting, while sharing the box. `phases` says whether a file is compute-bound
or waiting on storage. `result` is `ok`, `unreadable` or `error`.

### Where it goes

- **File storage**, under `perf/` in the `audio` container, beside the audio
  rather than in a container of its own, so it needs no new role or container:
  `perf/{yyyy-mm-dd}/{instance}/{segmentStart}.jsonl`. `storage.Store` can't
  append to a blob, so each file is a **segment** that is rewritten whole
  every 30 seconds and rolled after ten minutes (or 1 MB); a replica killed
  without warning loses at most 30 seconds. In dev the same names are under
  `backend/data/audio/perf/`. About 200 KB per busy replica-hour; a lifecycle
  rule deletes them after 180 days (`infra/storage.tf`). `BIRDSENSE_PERF=off`
  turns it all off.
- **Log Analytics**, for the `task` records, as a structured log line
  (`msg="analysis: task"`, with `audio_per_cpu_sec`), so a quick KQL query
  answers "how long is Perch taking this week" without downloading anything.

### Bringing it back

```sh
az storage blob download-batch --auth-mode login \
  --account-name <account> -s audio -d ./perf-data --pattern 'perf/2026-10-*'
```

(`grant_operator_blob_access` gives the operator read access.) Then either hand
over the `.jsonl` files as they are, or run the summary first:

```sh
cd backend && go run ./cmd/perf ../perf-data
```

`cmd/perf` prints three tables: what it was measured on (CPU, limits, image,
settings); per model, workers and threads -- audio per CPU-second (median and
slowest tenth), times real time, cores used, peak PSS (p50, p95, max), max RSS
and how wall time split across the phases; and per instance, while it had
work -- mean cores, the share of time at <25/<50/<75/≥75% of the CPU limit,
throttling, peak memory and anon against the limit, and OOM kills. It is text
to paste; the raw files are there when it doesn't answer the question.

### The benchmark

Production data shows the replica as it runs; it can't show a setting we
haven't deployed. So `cmd/analyze -bench` runs the model over a file once per
combination of `-workers` and `-threads` (each a comma-separated list, with
`-repeat`), one run at a time, samples it the same way, writes the same records
to `bench-MODEL-TIME.jsonl`, and prints a line per run:

```sh
cd backend && BIRDSENSE_BIRDNET_PYTHON=../.venv/bin/python \
  go run ./cmd/analyze -bench -model perch -threads 1,2,4,8 card-file.wav
```

What the two settings actually control, from reading birdnet 1.1.1:

- **BirdNET's LiteRT interpreter is hard-coded to one thread**
  (`num_threads=1` in `birdnet/core/backends.py`). It uses more cores only
  through `-workers`, which are processes, each with its own model copy.
- **Perch's TensorFlow uses TensorFlow's default threading: every core the
  process can see.** So DEPLOYMENT.md's "73 s on one worker" may already have
  been spread over several cores, and on a shared replica several Perch
  processes would contend for all of them. `analyze.py --threads N`
  (`birdnet.Options.Threads`) caps it by setting `TF_NUM_INTRAOP_THREADS` and
  `OMP_NUM_THREADS` to N and `TF_NUM_INTEROP_THREADS` to 1 before anything
  loads -- environment variables because birdnet's workers are child
  processes, which `tf.config` calls in the script wouldn't reach.
- Starting either model is not single-threaded: on the 14-second Osprey clip,
  BirdNET with 2 workers used 12.6 CPU-seconds in 3.1 s of wall time, most of
  it Python, numpy and the model loading. That is why the benchmark wants a
  real hour-long card file: on a short clip, startup is the measurement.

### What the numbers decide
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

With the measured figures, Perch over every file, five recorders:

| | Today, Perch on | This design |
|---|---|---|
| Web app | 2 vCPU / 4 GiB, always on, and Perch keeps it ~90% busy: **~$150/month** -- about as much as it can do, so any more audio and the backlog grows | 0.25 vCPU / 0.5 GiB, mostly idle: **~$5/month** |
| Analysis | included above | ~1,280 vCPU-h × $0.11 ≈ **~$140/month**, if packed well |
| Per recorder | | ~$28/month, of which BirdNET ~$1.50 |
| One card | ~12 h of BirdNET, then ~4 days of Perch | ~5 hours at 10 replicas of 4 vCPU |

So the job doesn't make Perch much cheaper -- it is billed for the same CPU --
it makes cards fast and stops the backlog. What makes it cheaper is Perch
hearing less audio (*Open: how much audio Perch hears*), and after that Batch
on spot VMs, which would bring ~1,280 vCPU-hours to roughly $10–15.

## Milestones

**M0 -- Measure, in today's architecture.** The sampler, the `run`/`sample`/
`task` records, the `perf/` prefix and its lifecycle rule, `cmd/analyze -bench`,
`analyze.py --threads`, `cmd/perf`. This runs inside today's in-process queue,
so the next real card in production produces data before anything else
changes. Then: bench Perch and BirdNET at 1/2/4/8 threads on a real card file.

**M1 -- Claims, leases, stored attempts.** Schema fields and SCHEMA.md, the
lease-renewal loop, letting go on the way out. Still in-process and still one
task at a time; the existing tests keep passing, plus two workers racing over
one card in the JSON backend.

**M2 -- The packing scheduler.** Memory budget from the replica's limits
(cgroups, else `BIRDSENSE_REPLICA_*`), per-model estimates from M0,
BirdNET-first, thread handoff, the shared download cache, and `retryAfter` in
place of `Run`'s pause as what spaces a file's retries.
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
| M0 Measure | done 2026-09-28, but for the benchmark | First real card measured (*Measured*). Production had no readable cgroup, so the container figures now fall back to cgroup v1 and `/proc`, and Terraform passes the replica's size in. Still to run: `cmd/analyze -bench -model perch -threads 1,2,4` on a real card file. |
| M1 Claims and leases | done 2026-09-28 | `db.Claim` on both steps; `take`/`hold`/`release` in `internal/analysis`; tests for two workers sharing a card, a lapsed claim, a lost claim, and stopping (race detector clean). `retryAfter` moved to M2. Not yet run against Cosmos. |
| M2 Packing scheduler | not started | |
| M3 The job | not started | |
| M4 Tune | not started | |
