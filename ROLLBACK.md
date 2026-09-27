# Birdsense — rolling back

Rolling back Birdsense means running an older image. Terraform owns the running
image, so it is `terraform apply` with an earlier `image_tag` and nothing else
-- `scripts/deploy.ps1 -ImageTag` is that apply, with the checks that make it
safe to run under pressure. Every resource, setting and variable named here is
explained in [DEPLOYMENT.md](DEPLOYMENT.md); this file is only the rollback.

It rolls back **code**. It does not roll back configuration, and it cannot roll
back anything the old code already wrote -- see *What a rollback does not undo*,
which is the part worth reading before the commands.

## Rolling back

```powershell
./scripts/deploy.ps1 -ListTags        # 1. what can I go back to?
./scripts/deploy.ps1 -ImageTag <tag>  # 2. go there
```

Then check it came up. Both of these need no session, and the script prints the
app's URL when it finishes (`terraform output -raw app_url` otherwise):

```
GET <app_url>/api/v1/health    200, with "queue": how analysis stands
GET <app_url>/api/v1/ready     200, or 503 if Cosmos or storage is unreachable
```

A rollback is often about `queue` in particular; the table of states and what
each one means is in DEPLOYMENT.md (*When cards sit in processing*). Cards that
were waiting are picked up as soon as the revision is up, oldest first, with no
further prompting.

Nothing about this needs a clean working tree, a rebuild, or even git: the
image already exists. That is deliberate. At the moment a rollback is wanted
the tree is usually mid-fix, and a rebuild would be minutes of `pip install`
and a ~90 MB model download that a bad day at PyPI or Zenodo can fail outright.

## Reading the menu

`-ListTags` deploys nothing, needs no `prod.tfvars` and no secrets, and answers
"back to what?". Because the tag **is** the commit sha, it joins the two halves
of the answer -- `az acr repository show-tags --orderby time_desc` for what was
actually built and in what order, `git show` per tag for what each one is:

```
  * 4c0264d    2026-09-21  Fix health and readiness endpoints
    517e93d    2026-09-21  Fix insufficient validation on tus upload id
    a819310    2026-09-20  Enforce sane maximums for upload
```

`*` is the tag Terraform has applied. Three annotations are worth reading
rather than skipping past:

| Annotation | What it means |
|---|---|
| `[not on HEAD]` | The commit isn't an ancestor of the checked-out one: a branch build, or history that has been rewritten. Deployable, but it isn't "an earlier version of what you have". |
| `not a commit in this clone` | Git has never heard of the sha, usually a stale fetch. Still deployable; `git fetch` first if you want to know what it is. |
| `[dirty build]` | A `<sha>-dirty-<timestamp>` tag from `-AllowDirty`. It is annotated with the commit it was built *on*, which is as much as anything can know: what the tree held on top of that commit was never recorded. That is the cost of `-AllowDirty`, and this is the only place it shows. |

The menu shows ten, which is weeks of deploys; a rollback older than that is a
decision rather than a reflex. Nothing deletes old images (`infra/registry.tf`
sets no retention policy and no purge task), so the registry holds more than
the menu does: `az acr repository show-tags --name <acr> --repository birdsense
--orderby time_desc` for all of them.

## What a rollback does not undo

**Configuration.** `-ImageTag` applies the *current* `infra/prod.tfvars` with
the *old* image. Secrets, sizes, retention days, the OIDC client -- all stay as
they are now. If the problem is a setting rather than the code, the rollback
will change nothing; fix the variable and apply with the running tag instead
(the two lines for that are in README.md, under *Deploying*).

**Documents the old code already wrote.** There is no down-migration and no
schema version, so a rollback across a *stored field* being added leaves
documents carrying a field the running binary knows nothing about. Gotcha: it
does not merely ignore that field, it drops it. An update is read, mutate,
replace (`updateDoc` in `internal/db/cosmos.go`) and the read decodes into a Go
struct, so a field the struct doesn't declare is gone from the document the
next time anything writes it. Documents nothing touches keep it. (The dev
JSON backend is blunter still: it rewrites the whole file from the structs on
every write.) So before rolling back over a SCHEMA.md change,
decide whether losing that field on the documents that get touched is
acceptable; usually it is, but it is a decision, not a no-op.

**Audio that retention has deleted.** Originals go a month after a card is
received, and that is permanent (CLAUDE.md, *Originals expire; clips don't*).
No image can bring them back.

**Sessions and the roster.** Neither lives in the image. Sessions are signed
with `BIRDSENSE_SESSION_KEY`, which is configuration; the roster is in Cosmos.
Rolling back signs nobody out and un-removes nobody.

## Gotchas

**Applying the tag that is already applied does nothing.** Container Apps keys
revisions off the image *string*, so re-applying the running tag creates no new
revision at all -- the deploy looks like it worked and changes nothing. The
script warns when the tag you passed is the one already applied, because
"nothing happened" is exactly the symptom a rollback is usually chasing. To
restart the same image, restart the revision instead (DEPLOYMENT.md, *When
cards sit in processing*).

**A tag that isn't in the registry is a revision that can't pull.** Container
Apps reports that minutes later, by which time it looks like the rollback
failed for some other reason. The script checks the tag exists before applying
and prints the menu instead, so a typo and a sha that was never built stop
being the same symptom.

**The two revisions overlap.** `max_replicas = 1` bounds a *revision*, not the
app, so during any swap -- a rollback included -- the outgoing and incoming
replicas briefly run together, and both run the analysis queue. That costs
duplicated work and not consistency, which is worth spelling out because the
in-memory tus lock looks like it should make it cost more:

- **An upload converges because the offset is the blob's, not a process's.**
  tusd re-reads it from the uncommitted block list on every request and
  answers 409 to a `PATCH` that doesn't match. So two `PATCH`es accepted at
  once are necessarily at the same offset, carry the same bytes and stage the
  same block id: each overwrites the other with itself.
- **Analysis converges because every write is idempotent.** Both replicas
  really do analyze the same file -- `queued` counts `analyzing`, so that a
  restart resumes a file cut off mid-run, which means claiming a file can't
  exclude a second process. Detection ids are deterministic, clips are the
  same bytes under the same names, and a card's counts are tallied from its
  documents rather than incremented.

What a card being uploaded through a swap can still meet is a file that fails
mid-`PATCH`, because the replica serving it went away. The browser resumes it.
That makes a swap during an upload a nuisance rather than a hazard: prefer one
when nothing is in flight -- *All uploads* shows what is -- but a rollback
worth doing doesn't have to wait for a card.

**Don't reach for `az containerapp update`.** It would set the image outside
Terraform, and the next `terraform apply` -- anyone's, for any reason -- would
silently put the broken tag back. The whole point of the image being a
Terraform variable is that one thing decides what is running.

## If the rollback itself won't go

- **`could not read acr_name from Terraform`** -- the state isn't reachable
  (`terraform -chdir=infra init -backend-config=backend.hcl`, or `az login`),
  or outputs were never written. Both `-ListTags` and `-ImageTag` accept the
  registry name directly: `$env:BIRDSENSE_ACR = "crbirdsenseprod"`.
- **`infra/prod.tfvars is missing`** -- only the applying modes need it, and it
  is not in git. Copy `infra/prod.tfvars.example` and fill it in; the secrets
  are in whatever the chapter keeps them in, not here. `-ListTags` works
  without it.
- **Terraform wants to change more than the image.** That is drift, not the
  rollback: something else has changed since the last apply. Read the plan. If
  the app has to move *now*, applying it is usually right -- Terraform's view
  is the intended one -- but know what else you are shipping.
- **Nobody can sign in afterwards.** That is configuration, not the image: the
  redirect URI or the client secret (DEPLOYMENT.md, *Sign-in*). Rolling back
  further will not fix it.
