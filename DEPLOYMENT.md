# Birdsense — Azure deployment

This is the list of Azure resources Birdsense needs, with the settings that
matter. Every resource names its Terraform type, and every setting that differs
from the default is spelled out and explained. The data those resources hold is
described in [SCHEMA.md](SCHEMA.md).

`infra/` is this file as Terraform (`azurerm` provider), one file per group of
resources: `main.tf` (resource group, identity), `cosmos.tf`, `storage.tf`,
`registry.tf`, `app.tf` (logs, environment, container app), plus `variables.tf`
and `outputs.tf`. The short version of running it is in
[README.md](README.md#deploying); this file is the why. When the two disagree,
the Terraform is what runs -- fix this file.

Status: nothing here is provisioned yet; the Terraform has never been applied
against a real subscription. The app code supports both data
stores, Cosmos DB for documents and Blob Storage for card audio, but neither
has run against Azure. [Blob storage for uploads](#blob-storage-for-uploads)
lists what the storage side needs and what to check first.

## Shape of it

```
                    ┌──────────────── rg-birdsense-prod ────────────────┐
 browser ──HTTPS──▶ │ Container App  ca-birdsense-prod  (image from ACR)│
                    │   │ user-assigned managed identity id-birdsense-prod
                    │   ├──Entra ID──▶ Cosmos DB  cosmos-birdsense-prod │
                    │   └──Entra ID──▶ Storage    stbirdsenseprod/audio │
                    │ Container Apps env ──logs──▶ Log Analytics        │
                    └───────────────────────────────────────────────────┘
```

- **One container** serves the API and the frontend (see CLAUDE.md), so there
  is no front door, CDN or second app.
- **No secrets.** Every data-plane call authenticates as the app's managed
  identity. Cosmos key auth and storage shared keys are turned off.
- **Local development doesn't use Azure at all** (`BIRDSENSE_DB=local`).

## Naming and region

| Setting      | Value |
|--------------|-------|
| Region       | `westus2`, the nearest region to the Eastside with serverless Cosmos DB and Container Apps. |
| Environment  | `prod` only, for now. Put `env` in a Terraform variable so a `staging` copy is a second `tfvars` file. |
| Name pattern | `<type>-birdsense-<env>`, following Azure's abbreviation list. Storage and ACR names allow only lowercase alphanumerics, so they drop the dashes. |
| Tags         | `app = birdsense`, `env = <env>`, `owner = eastside-audubon` |

Cosmos DB, storage and registry names are globally unique. If one is taken,
add a short suffix and update this file.

## Resources

### 1. Resource group — `azurerm_resource_group`

| Setting  | Value |
|----------|-------|
| name     | `rg-birdsense-prod` |
| location | `westus2` |

### 2. User-assigned managed identity — `azurerm_user_assigned_identity`

| Setting | Value |
|---------|-------|
| name    | `id-birdsense-prod` |

A user-assigned identity, not a system-assigned one, so its role assignments
can exist *before* the container app does. Otherwise the first revision
can't pull its image. Its `client_id` goes into the app's `AZURE_CLIENT_ID`.

### 3. Cosmos DB account — `azurerm_cosmosdb_account`

| Setting | Value | Why |
|---------|-------|-----|
| name | `cosmos-birdsense-prod` | |
| kind | `GlobalDocumentDB` | The NoSQL API. |
| offer_type | `Standard` | Required by the provider; serverless is set by the capability below. |
| capabilities | `EnableServerless` | Pay per request. The load is a few cards a week, not a steady rate. |
| consistency_policy | `Session` | The default. A writer reads its own writes. |
| geo_location | one: `westus2`, failover_priority 0, zone_redundant `false` | Serverless is single-region. |
| local_authentication_enabled | `false` | Entra ID only. No account keys to leak. (`local_authentication_disabled = true` says the same thing and is deprecated from provider v5.) |
| public_network_access_enabled | `true` | The consumption Container Apps environment has no VNet. Revisit with private endpoints if that changes. |
| minimal_tls_version | `Tls12` | |
| backup | `type = Continuous`, `tier = Continuous7Days` | Point-in-time restore of the last 7 days at no extra cost. |
| free_tier_enabled | `false` | Not compatible with serverless (see *Cost* for the alternative). |

### 4. Cosmos DB database — `azurerm_cosmosdb_sql_database`

| Setting | Value |
|---------|-------|
| name | `birdsense` |
| throughput | *unset* (serverless) |

### 5. Cosmos DB containers — `azurerm_cosmosdb_sql_container` ×5

All five share these settings: `partition_key_version = 2`, default indexing
policy (consistent, all paths included), no unique keys, no default TTL, no
throughput (serverless).

| name         | partition_key_paths |
|--------------|---------------------|
| `users`      | `["/id"]`           |
| `recorders`  | `["/id"]`           |
| `uploads`    | `["/id"]`           |
| `audioFiles` | `["/uploadId"]`     |
| `detections` | `["/uploadId"]`     |

**Names and partition keys can't be changed after creation** without copying
the data to a new container. They must match
`backend/internal/db/cosmos.go` exactly, and the container names are
case-sensitive.

The app never creates the database or containers. On startup it checks that
all five exist and exits if one is missing.

### 6. Cosmos DB data-plane role — `azurerm_cosmosdb_sql_role_assignment`

| Setting | Value |
|---------|-------|
| resource_group_name / account_name | the account above |
| role_definition_id | `${azurerm_cosmosdb_account.this.id}/sqlRoleDefinitions/00000000-0000-0000-0000-000000000002` (**Cosmos DB Built-in Data Contributor**) |
| principal_id | `azurerm_user_assigned_identity.this.principal_id` |
| scope | `${azurerm_cosmosdb_account.this.id}/dbs/birdsense` |

This is a Cosmos **SQL role assignment**, not an ordinary Azure RBAC
`azurerm_role_assignment`. Portal roles like "Cosmos DB Account Reader" or
"DocumentDB Account Contributor" are control-plane roles and do **not** grant
reading or writing documents. The app needs no control-plane role at all.

### 7. Storage account — `azurerm_storage_account`

Holds the audio. At ~128 GB per card this is where the money goes, which is why
originals are kept for only a month (see *Audio retention* and *Cost*).

| Setting | Value | Why |
|---------|-------|-----|
| name | `stbirdsenseprod` | |
| account_kind | `StorageV2` | |
| account_tier / account_replication_type | `Standard` / `LRS` | Originals are recoverable from the card for a while, and results live in Cosmos. Revisit if the archive must survive a datacenter loss. |
| access_tier | `Hot` | Lifecycle rules move old audio down. |
| shared_access_key_enabled | `false` | Entra ID only. Browsers upload through the app, which writes with its managed identity, so nothing needs an account key or a SAS. |
| allow_nested_items_to_be_public | `false` | |
| min_tls_version | `TLS1_2` | |
| blob_properties.delete_retention_policy | 7 days | Soft delete. |
| blob_properties.cors_rule | *unset* | Browsers never talk to the storage account. |

**Blob container** — `azurerm_storage_container`: name `audio`, access type
`private`. Card audio is at `uploads/{uploadId}/{random}`, each with a
`.info` blob beside it, and the clips the server cuts for review are at
`clips/{uploadId}/{detectionId}.wav`, a few MB each at most (SCHEMA.md). The app tries to create the container at
startup and carries on if it exists; Terraform should still own it.

**Lifecycle** — `azurerm_storage_management_policy`, one rule on prefix
`audio/uploads/`. It deliberately never names `audio/clips/`, which stays hot:
clips are what retention keeps for good, and reviewers play them on demand.

The rule is **not** the retention policy. The app deletes originals itself
(*Audio retention* below), because only it knows whether BirdNET has finished
with a file and it has to mark the document either way. This rule is the net
underneath: it sweeps what the app never recorded — uploads abandoned part way,
and anything a failed delete left behind:

| Action | After | Why |
|--------|-------|-----|
| tier to cool | 30 days since last modification | For the stragglers the app doesn't manage. By then a managed blob is usually deleted anyway, and a straggler always outlives cool's 30-day early-deletion charge. |
| delete | `audio_backstop_days`, 180 by default | Far enough out that it is never what removes a card normally. A card the app is deliberately holding — one whose analysis never finished — does lose its audio here, so don't tighten it towards the retention window. |
| tier to archive | *unset* | An archived blob can't be read without a rehydrate, and `internal/analysis` reads originals straight out of the container. |

### 8. Storage data-plane role — `azurerm_role_assignment`

| Setting | Value |
|---------|-------|
| scope | the storage account |
| role_definition_name | `Storage Blob Data Contributor` |
| principal_id | the managed identity |

### 9. Container registry — `azurerm_container_registry`

| Setting | Value |
|---------|-------|
| name | `crbirdsenseprod` |
| sku | `Basic` |
| admin_enabled | `false` |

**Pull role** — `azurerm_role_assignment`: scope the registry,
`AcrPull`, principal the managed identity.

### 10. Log Analytics workspace — `azurerm_log_analytics_workspace`

| Setting | Value |
|---------|-------|
| name | `log-birdsense-prod` |
| sku | `PerGB2018` |
| retention_in_days | `30` |

The server logs with `log/slog` to stdout, which the Container Apps
environment forwards here.

### 11. Container Apps environment — `azurerm_container_app_environment`

| Setting | Value |
|---------|-------|
| name | `cae-birdsense-prod` |
| log_analytics_workspace_id | the workspace above |
| workload profile | Consumption (the default; no dedicated profile) |

### 12. Container app — `azurerm_container_app`

| Setting | Value | Why |
|---------|-------|-----|
| name | `ca-birdsense-prod` | |
| revision_mode | `Single` | |
| identity | `UserAssigned`, the identity above | |
| registry | server `crbirdsenseprod.azurecr.io`, identity = the identity above | Pulls with AcrPull, no password. |
| container image | `crbirdsenseprod.azurecr.io/birdsense:<git sha>` | Built from the repo's `Dockerfile`, unchanged. |
| cpu / memory | `1` / `2Gi` | The server runs BirdNET over received cards (`internal/analysis`): one Python process at a time, peaking near 300 MB, which is CPU-bound for hours per card. On this much CPU expect very roughly 2 minutes per hour of audio, so a ~336-file card takes most of a day; more CPU is faster. Moving analysis to a job would let the web app go back to `0.25` / `0.5Gi`; see *Open questions*. |
| min_replicas / max_replicas | `1` / `1` | **Analysis needs a replica that stays up**: it runs in the background with no HTTP traffic, and a scale-to-zero replica is stopped mid-card. Nothing is lost when that happens (the queue is in Cosmos and resumes at the next start), but it stops until someone visits. `0` is fine again once analysis is a job. **Uploads need exactly one**: tusd locks an upload in the memory of the replica serving it (see [Blob storage for uploads](#blob-storage-for-uploads)), and two replicas would also analyze the same file twice. |
| ingress | external `true`, target_port `8080`, transport `auto`, allow_insecure_connections `false`, traffic 100% to latest revision | |
| startup probe | HTTP GET `/api/v1/health` on `8080`; `initial_delay` 5, `timeout` 5, every 10 s, 10 failures | ~100 s to come up, which is room for a cold start and not for a broken configuration -- that one exits at startup instead, and restarts the container. |
| liveness probe | HTTP GET `/api/v1/health` on `8080`; `initial_delay` 10, `timeout` 5, every 30 s, 10 failures | `/api/v1/health` answers 200 whatever the dependencies are doing, so only a process that has stopped answering at all restarts this container -- and only after five unbroken minutes of it. Restarting never fixes Cosmos, and this replica is the one analyzing a card. The same endpoint the Dockerfile `HEALTHCHECK` uses. |
| readiness probe | HTTP GET `/api/v1/ready` on `8080`; `timeout` 5, every 30 s, 3 failures, 1 success | The one probe that can fail. `/api/v1/ready` pings Cosmos and the blob container, so a replica that has lost either leaves ingress after ~90 s instead of serving 500s, and is back the first time a ping succeeds. |

Every probe value is set on purpose. The provider's defaults -- `timeout` 1,
every 10 s, 3 failures -- describe a container that isn't also running BirdNET
CPU-bound for hours on one vCPU, where a health response waiting behind it is
normal rather than a sick replica.


**Environment variables:**

| Name | Value | Notes |
|------|-------|-------|
| `BIRDSENSE_DB` | `cosmos` | The default. Set explicitly so the config reads plainly. |
| `BIRDSENSE_COSMOS_ENDPOINT` | `azurerm_cosmosdb_account.this.endpoint` | e.g. `https://cosmos-birdsense-prod.documents.azure.com:443/` |
| `BIRDSENSE_COSMOS_DATABASE` | `birdsense` | |
| `BIRDSENSE_STORAGE` | `azure` | The default outside dev mode. Set explicitly so the config reads plainly. |
| `BIRDSENSE_BLOB_ENDPOINT` | `azurerm_storage_account.this.primary_blob_endpoint` | e.g. `https://stbirdsenseprod.blob.core.windows.net/` (a trailing slash is fine). |
| `BIRDSENSE_BLOB_CONTAINER` | `audio` | The default. Set explicitly. |
| `AZURE_CLIENT_ID` | `azurerm_user_assigned_identity.this.client_id` | Tells the SDK *which* managed identity to use. Required for a user-assigned identity. |
| `AZURE_TOKEN_CREDENTIALS` | `ManagedIdentityCredential` | Stops `DefaultAzureCredential` trying developer credentials first in production. |
| `BIRDSENSE_AUDIO_RETENTION_DAYS` | `var.audio_retention_days`, default `30` | How long a card's original recordings are kept once BirdNET has finished with them; `0` keeps them for good. Detections and their clips are never removed by it. See [Audio retention](#audio-retention). |
| `BIRDSENSE_BOOTSTRAP_ADMIN` | `var.bootstrap_admin`, e.g. `Your Name <you@eastsideaudubon.org>` | **Required on the first deploy.** The first admin; see [First deploy](#first-deploy). |
| `BIRDSENSE_PUBLIC_URL` | `var.public_url`, or the container app's own `https://<fqdn>` when that is empty | Where browsers reach Birdsense. The redirect URI is built from it, so it must match one registered with the provider. Not taken from the request's `Host` header, which a caller chooses. |
| `BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID` | `var.oidc_microsoft_client_id` | The Entra ID app registration; see [Sign-in](#sign-in). |
| `BIRDSENSE_OIDC_MICROSOFT_CLIENT_SECRET` | Container Apps secret `oidc-microsoft-client-secret` | Kept as a secret, so it isn't in the revision's environment listing. |
| `BIRDSENSE_OIDC_MICROSOFT_TENANT` | `var.oidc_microsoft_tenant`, default `common` | `common` accepts any organization and any personal Microsoft account. A tenant GUID restricts sign-in to that directory. |
| `BIRDSENSE_SESSION_KEY` | Container Apps secret `session-key` | Signs the session cookie. At least 32 characters, or the server won't start. Keep it stable across deploys; changing it signs everyone out. |
| `BIRDSENSE_ADDR`, `BIRDSENSE_STATIC_DIR`, `BIRDSENSE_STORAGE_DIR` | *unset* | Already set in the image (`:8080`, `/app/frontend`, and `/app/audio`, which only local storage uses). |
| `BIRDSENSE_BIRDNET_PYTHON`, `BIRDSENSE_BIRDNET_SCRIPT`, `BIRDNET_APP_DATA` | *unset* | Already set in the image, pointing at its BirdNET venv, `analyze.py` and the models baked in at build time. The analysis queue checks them before it starts, and again on a growing delay until they work, logging `BirdNET isn't available` (and analyzing nothing) meanwhile. `/api/v1/health` reports `"queue":"unavailable"`, and the coordinator's card screens say so; see *When cards sit in processing*. |

Never set `BIRDSENSE_COSMOS_KEY` in Azure. It exists only for the emulator.

**Custom domain** (optional, when chosen): `azurerm_container_app_custom_domain`
with a managed certificate, plus a CNAME and `asuid` TXT record at the DNS
host. The records have to resolve publicly before Azure will attach the domain
or issue the certificate, so a hosts-file entry can't stand in for them. When
it lands, register the matching redirect URI with the provider *first*, then
set `public_url` -- see [Sign-in](#sign-in).

## Sign-in

Volunteers sign in with OpenID Connect, and the roster is the allow-list: the
provider proves which address someone owns, and a coordinator having added that
address is what lets them in. Nothing in Azure holds passwords. The server
refuses to start outside dev mode without a provider configured, because the
development sign-in isn't registered there and it would have no way in at all.

**The app registration** lives in Entra ID, which is a *directory* object, not
an Azure resource: Contributor, Owner or Role Based Access Control
Administrator on the subscription grant nothing here. Creating one needs either
the tenant's "Users can register applications" setting left on, or the
**Application Developer** directory role (Entra ID → Roles and administrators),
which allows it even when that setting is off. Whoever creates a registration
becomes its owner and can manage its redirect URIs and secrets afterwards.
Terraform does not own it: it is created once, by hand, in a directory that may
not be the one the subscription lives in.

Set it up as:

| | |
|---|---|
| Supported account types | **Accounts in any organizational directory and personal Microsoft accounts.** Volunteers use the address they already have, which is usually not an Eastside Audubon one. Safe because the roster, not the audience, decides who gets in. |
| Redirect URI (Web) | `<public URL>/api/v1/auth/microsoft/callback`. Register one per hostname the app answers on -- the `azurecontainerapps.io` one for testing, the custom domain when it exists, and `http://localhost:8080/...` for development, which providers allow over plain http for localhost only. They coexist; adding one is additive. |
| Client secret | Copy the **value**, not the secret id, into `oidc_microsoft_client_secret`. Note its expiry: a secret that lapses stops all sign-in. |
| Optional claim (ID token) | **`xms_edov`**. See below -- without it, volunteers on a domain belonging to some *other* organization's tenant are refused. |
| API permissions | `openid`, `profile`, `email` (Microsoft Graph, delegated). All user-consentable, so no admin consent is needed unless the tenant restricts user consent, in which case an Application Administrator grants it once. |

**Why `xms_edov` matters.** Accepting any Microsoft account means accepting
every organization's directory, and a directory's administrators choose what
their own users' `email` claims say -- including a roster member's address.
Birdsense therefore believes an email claim only when the provider is in a
position to know it: a personal Microsoft account (the address *is* the
account), an organization that has proved to Microsoft it owns the domain
(`xms_edov`), or Google's `email_verified`. Anything else is refused and logged
as `unverified-email`. The rule is `trustedEmail` in `internal/api/auth.go`.

**The session key** (`session_key`, a Container Apps secret) signs the session
cookie. Generate it with `openssl rand -base64 32` and keep it: a new value
signs everyone out, which is also how to end every session at once on purpose.
It must be at least 32 characters -- the cookie it signs is the identity, so a
short one is a forgeable admin session. `terraform plan` refuses a shorter one,
and so does the server at startup, which is what covers a deployment that sets
`BIRDSENSE_SESSION_KEY` some other way.

**Adding Google** is two more variables and no code:
`oidc_google_client_id` and `_secret` would follow the same shape, and the
sign-in page grows a second button on its own -- `GET /api/v1/session` reports
which providers the server has, and the page renders a button per provider.

## Ordering

Terraform infers most of this from references, but two dependencies are
invisible to it:

1. The **AcrPull**, **Cosmos SQL role** and **Storage Blob Data Contributor**
   assignments must exist before the container app's first revision. Otherwise
   the image pull fails, or the app exits at startup because it can't read the
   Cosmos containers or reach the blob container. Add them to the app's
   `depends_on`.
2. Role assignments take a minute or two to propagate. A first `apply` can
   still race them. If the first revision fails, re-apply or restart the
   revision; it is not a config error.
3. Creating the `audio` blob container is a **data-plane** call, and shared
   keys are off, so the principal running Terraform needs Storage Blob Data
   Contributor on the storage account -- Owner on the subscription grants no
   data actions. `infra/storage.tf` assigns it (`grant_operator_blob_access`,
   default true) and the provider is configured with `storage_use_azuread`.
   This is the same role to give yourself for the local storage check below.
4. The **registry must exist before the container app's first apply**, because
   a revision can't be created pointing at an image that isn't there. Hence the
   one-time `apply -target=azurerm_container_registry.this` in README.md. It is
   only ever needed once, for an empty subscription.

## First deploy

A new database has an empty `users` container, and only an admin can add
people to the roster. So the **first deploy must set
`BIRDSENSE_BOOTSTRAP_ADMIN`** to the coordinator who will run the program,
as `Name <email>` or just the email. That address is the one they sign in with
through Google or Microsoft. Make it a Terraform variable (`bootstrap_admin`)
with no default, so a first `apply` can't go ahead without it.

On startup the server adds that person as an admin **only if the roster is
empty**, and logs `added the bootstrap admin to an empty roster`. Every other
start does nothing, so the variable is safe to leave set: it can't make someone
admin again after they've been demoted, removed or given a new address. If the
roster isn't empty and the address isn't on it, the server logs a warning and
carries on. A typo on the first deploy has to be fixed in the Cosmos Data
Explorer, because the roster is no longer empty.

Outside dev mode the server **refuses to start** if the bootstrap admin's name
or address is someone from the development placeholder roster
(`internal/devseed`), or if the address is on a reserved example domain
(`example.com`, `*.test`, ...). Dev placeholder people never go into Cosmos:
the seed that writes them only runs in dev mode, against the JSON file.

## Deploying a new version

```powershell
./scripts/deploy.ps1
```

**Terraform owns the image tag.** The script builds the image and then applies
`-var image_tag=<sha>`, so one thing decides what is running, a rollback is
applying an older tag, and `terraform plan` is never wrong about the app. The
alternative -- `lifecycle { ignore_changes = [template[0].container[0].image] }`
with deploys done by `az containerapp update` -- is the right split only once
deploys run from CI with an identity that has no Terraform state access. Until
then, nothing should `az containerapp update` this app: that is drift the next
`apply` reverts.

The script builds with `az acr build` rather than a local `docker build`. That
needs no local Docker, downloads the BirdNET models over Azure's network rather
than yours, and produces a `linux/amd64` image whatever the machine running it
is -- an arm64 image (an Apple Silicon `docker build`) starts and dies in
Container Apps with an exec format error.

`az acr build` uploads the whole build context to the registry's source
storage, and the repo it is run from holds `infra/prod.tfvars` -- the OIDC
client secret and the session key in plaintext -- next to `infra/.terraform`.
So `.dockerignore` is an allow-list: `*`, then back in only the three
directories the Dockerfile copies (`backend`, `analyzer`, `frontend`). A new
file has to be named there before it can leave the machine, which is the right
way round for a context that goes to a registry. The script checks that shape
before it builds, because the failure is silent from this end.

Two rules the script enforces, both about the tag being the commit sha:

- It refuses a dirty working tree, so what's deployed is something that can be
  checked out. `-AllowDirty` appends a timestamp instead.
- A tag is never reused. Container Apps keys revisions off the image *string*,
  so re-pushing a tag the app already runs creates no new revision at all: the
  deploy would look like it worked and change nothing.

Terraform's own state lives in a storage account created by hand, outside this
configuration (see [README.md](README.md#one-time-setup)). Terraform owning the
state it depends on is a knot nobody wants to untie at 11pm.

## When cards sit in processing

A card that has been received is analyzed by the queue inside the running app
(CLAUDE.md, *The analysis queue is the database*). There is one replica, so if
that queue isn't working, every received card stops where it is -- and audio
that hasn't been analyzed is audio that retention refuses to delete, so nothing
is lost while it waits.

**Ask the app first.** `GET /api/v1/health` answers `"queue"` with one word,
without a session:

| `queue` | What it means |
|---------|---------------|
| `ready` | BirdNET runs here and the last pass got through. |
| `starting` | The first BirdNET check is still running (seconds, at most a minute). |
| `unavailable` | BirdNET can't run in this container at all. |
| `failing` | BirdNET runs, but a pass stopped on something else -- Cosmos, blob storage -- and is being retried. |
| `off` | This build runs no queue. Not a thing the deployed image does. |

`/api/v1/ready` is a different question and answers about this replica's
dependencies, not about analysis: a queue that can't start leaves the site
working, so it never takes a replica out of ingress. See *Container app*.

Signed in as a coordinator, *All uploads* and any card's page say the same
thing in words, with the error the server got and since when. That is the
first place to look, and it is there so that nobody has to read container logs
to find out why a volunteer's card hasn't moved.

**Recovering.** The queue keeps checking, so most of this fixes itself:

- `unavailable` means the image or its settings are wrong -- the venv,
  `analyze.py` or the models aren't where `BIRDSENSE_BIRDNET_*` says. Nothing
  at runtime fixes that: deploy a good image (or apply an older `image_tag`,
  *Deploying a new version*). The cards are picked up as soon as the new
  revision comes up, in the order they were received.
- `failing` is usually the identity losing a role, or Cosmos throttling. Fix
  the cause; no restart is needed, because the queue retries the pass on its
  own.
- If a restart really is wanted -- to clear a wedged run rather than a
  configuration -- restart the revision, which changes nothing Terraform owns:

  ```sh
  az containerapp revision restart -n ca-birdsense-prod -g rg-birdsense-prod \
     --revision "$(az containerapp show -n ca-birdsense-prod -g rg-birdsense-prod \
                   --query properties.latestRevisionName -o tsv)"
  ```

  This is not `az containerapp update`, which would be drift the next
  `terraform apply` reverts. Nothing is lost by it: a file cut off mid-run is
  queued again, and detection ids are deterministic, so a re-run overwrites.

## Blob storage for uploads

Card audio reaches the storage account through the app, never straight from
the browser. Each file on a card is a tus upload to `/api/v1/tus/`, and the app
runs tusd's Azure store (`backend/internal/storage`): each request's bytes are
staged as a block, and the block list is committed when the last byte lands.
It signs in with `DefaultAzureCredential`, which here is the managed identity,
as for Cosmos. The code has run against Azurite with a shared key, and not yet
against Azure.

What it needs, beyond resources 7 and 8:

- **Settings** on the container app: `BIRDSENSE_STORAGE`,
  `BIRDSENSE_BLOB_ENDPOINT` and `BIRDSENSE_BLOB_CONTAINER` (see *Environment
  variables*).
- **Storage Blob Data Contributor** for the identity (resource 8). Besides
  reading and writing blobs, the app tries to create the container at startup,
  and exits if that fails for any reason other than the container already
  existing.
- **One replica.** tusd holds an upload's lock in memory while a request writes
  to it, so every request for one upload has to reach the same replica: keep
  `max_replicas = 1`. More replicas need ingress session affinity (sticky
  sessions) or a shared locker in `internal/storage`. Scaling to zero between
  uploads is fine; an upload resumes from what's stored.
- **Ephemeral disk.** The Azure store writes each request body to a temp file
  before staging it. The browser sends at most 50 MB a request and one file at
  a time, so that is 50 MB per volunteer uploading at once, against the
  replica's ephemeral storage allowance. Analysis downloads one whole file at a
  time to temp storage (a few hundred MB), cuts its clips beside it, and
  deletes both after.
- **Request time.** The Container Apps ingress ends a request after 240 s. At
  50 MB a request that holds down to about 2 Mb/s of upstream; for slower links,
  lower `CHUNK_BYTES` in `frontend/js/upload-flow.js`. Deleting a card is the
  other request that has to fit: it removes a few hundred recordings and, with
  them, tens of thousands of clips and documents. Blobs go in Blob Batch
  requests of 256 and documents in Cosmos transactional batches of 100, several
  of each in flight, which is a few hundred round-trips rather than tens of
  thousands. One request per blob or per document does not fit.
- **CPU and memory while a card uploads.** Every byte of a ~128 GB card passes
  through the container, for the hours the upload takes. Inbound transfer is
  free, and the app only copies bytes, but at `0.25` vCPU the upload rate may
  be CPU-bound. Watch the replica's CPU during the first real card, and raise
  cpu/memory if it sits at the limit.
- **Leftovers.** An upload that never finishes leaves uncommitted blocks, which
  Azure discards after 7 days, and a `.info` blob, which stays. A finished
  file's `.info` blob goes when the file does, under retention or with the card;
  an abandoned one has no document naming it, so only the lifecycle rule's
  delete action sweeps it. See *Open questions*.

## Audio retention

**A card's original recordings are kept for a month; its detections and their
clips are kept for good.** At ~128 GB a card, keeping originals is most of the
storage bill, and nothing reads one once BirdNET has: `internal/analysis` is
the only reader of `audio/uploads/`, and the only audio a browser ever plays is
a clip.

The app does the deleting, in `backend/internal/retention`, running in the
server process beside the analysis queue. It sweeps every few hours, and only
touches files BirdNET has finished with on cards it has finished with — a card
still being analyzed keeps its audio however old it is. It deletes the blob and
its `.info`, then marks the `audioFiles` document (`audioDeletedAt`, and
`blobName` cleared) and the card. Nothing under `audio/clips/` is ever named.
The rules and the failure ordering are in SCHEMA.md, *Audio retention*.

| Setting | Where | Default | What it does |
|---------|-------|---------|--------------|
| `audio_retention_days` | `infra/variables.tf` → `BIRDSENSE_AUDIO_RETENTION_DAYS` | 30 | The policy. `0` keeps originals until someone deletes the card. |
| `audio_backstop_days` | `infra/variables.tf` → the lifecycle rule | 180 | The net for what the app never recorded. Not the policy; see resource 7. |

Why the app rather than the lifecycle rule alone: the rule is prod-only, so dev
would behave differently; it can't tell an analyzed file from one still queued;
and it would leave `blobName` claiming audio that has gone.

Two things to know before this is switched on in production:

- **A card can't be re-analyzed after its window.** A better model, or a lower
  threshold, can only be run against cards still inside it. That is the real
  cost of the policy, and it is what the backstop's generous default protects
  a little of.
- **Detections stored before clips were cut have no clip**, and nothing
  backfills them (CLAUDE.md, *State of the code*). Once their originals go
  they are clipless for good, so backfill first if any are in Cosmos.

**First checks once we have Azure access:**

1. Give your own Entra user Storage Blob Data Contributor on a non-production
   account, `az login`, and run the app locally against it (the documents can
   stay in the JSON file):
   ```sh
   cd backend
   BIRDSENSE_DB=local BIRDSENSE_STORAGE=azure \
   BIRDSENSE_BLOB_ENDPOINT=https://<account>.blob.core.windows.net \
   go run ./cmd/server
   ```
   Upload a folder of a few large `.wav` files. Blobs should appear under
   `audio/uploads/{card}/`, each with a `.info` blob, and only once the file
   is complete. Pausing mid-file and resuming should carry on from the last
   50 MB chunk, not start over.
2. Deploy, and send one real card end to end. Watch the ingress for 499/504
   responses and the replica's CPU, memory and restarts while it runs.
3. If startup fails on the container check, the error names the refused
   operation; the role assignment may still be propagating (see *Ordering*).

## Developing against real Cosmos DB

Normal local development uses `BIRDSENSE_DB=local` and needs none of this. To
run a laptop against the Azure account (e.g. to test `cosmos.go`):

1. Give your own Entra user the same data role (step 6). Your user's object id
   goes in `principal_id`. Keep it scoped to the database, and prefer a
   non-production account.
2. `az login`
3. Run:
   ```sh
   cd backend
   BIRDSENSE_DB=cosmos \
   BIRDSENSE_COSMOS_ENDPOINT=https://cosmos-birdsense-prod.documents.azure.com:443/ \
   go run ./cmd/server
   ```
   `DefaultAzureCredential` picks up the Azure CLI login.

**Emulator** (untested). The Cosmos DB Linux emulator accepts the well-known
emulator key via `BIRDSENSE_COSMOS_KEY`. Create the `birdsense` database and the
five containers first (same partition keys), since the app won't. Run the
emulator over HTTP, or trust its self-signed certificate, or the Go client will
refuse the TLS handshake.

## Cost

Rough order of magnitude at five recorders, one card each every two weeks
(~130 cards a year). Check the Azure pricing calculator before relying on these.

| Item | Volume per year | Cost driver |
|------|-----------------|-------------|
| Audio in blob storage | ~2 cards held at a time ≈ **250 GB**, not 17 TB | Originals are deleted a month after a card is received (*Audio retention*), so this is flat rather than growing: about two cards' worth of hot LRS storage, tens of dollars a month. Without the policy it would be 130 cards × ~128 GB ≈ 17 TB by the end of a year, several hundred dollars a month. |
| Clips in blob storage | 130 cards × a few MB per detection | Kept for good, and small enough to stay hot. Grows with the detection threshold, not with hours of audio. |
| Cosmos DB, serverless | ~44k audio-file docs; detections depend on threshold (at 50 per file, ~2M docs, ~2 GB) | Low: a few dollars a year in request units, plus storage per GB-month. |
| Container Apps | low traffic, scale to zero | Usually within the monthly free grant, including the few hours of active replica each card upload takes. |
| Container Registry Basic | one small image | A few dollars a month. |
| Log Analytics | low volume | Within the free ingestion allowance at this scale. |

*Alternative for Cosmos.* One Cosmos account per subscription can use the free
tier (1000 RU/s and 25 GB free) with provisioned, database-level shared
throughput instead of serverless. It costs nothing until it's outgrown, but bulk
detection ingestion would be throttled (the SDK retries 429s). Worth it if the
account budget is tight; switching means a new account, since capacity mode
can't be changed in place.

## Open questions that change this file

- **Custom domain**: e.g. `birdsense.eastsideaudubon.org`, and who controls DNS.
- **Secrets**: the client secret and session key are Container Apps secrets,
  which means they are in the Terraform state file. Key Vault with the app's
  managed identity reading them would keep them out of state, and would give
  the client secret a rotation story before its expiry arrives.
- **BirdNET processing**: analysis runs in the container app today
  (`internal/analysis`), which is why it needs `min_replicas = 1` and more CPU
  and memory than serving pages does. A Container Apps job is the natural next
  step: it would run the same queue over the same Cosmos documents, from the
  same image, and let the web app scale to zero again. That adds a job and the
  same identity-based roles, and a way to start it (a schedule, or an event when
  a card is received). The build downloads the models from Zenodo, so
  `az acr build` needs outbound network.
- **Abandoned partial uploads**: a card registered and never sent leaves blocks
  with no `audioFiles` document naming them, so neither retention nor a card
  delete finds them, and only the lifecycle rule's delete action eventually
  sweeps them. A job that cleans up after a card that has gone quiet would do
  it sooner, and would be the place to reclaim the card's reference too.
- **Email**: the upload flow promises "card received" and "results" emails,
  which needs Azure Communication Services or an external provider.

## Where each resource is

Everything above is in `infra/`, applied as one stack. This table is the map
from this file to that one.

| # | Resource | Terraform type | File |
|---|----------|----------------|------|
| 1 | Resource group | `azurerm_resource_group` | `main.tf` |
| 2 | Managed identity | `azurerm_user_assigned_identity` | `main.tf` |
| 3 | Cosmos account (serverless, key auth off, continuous 7-day backup) | `azurerm_cosmosdb_account` | `cosmos.tf` |
| 4 | Cosmos database `birdsense` | `azurerm_cosmosdb_sql_database` | `cosmos.tf` |
| 5 | Containers `users`, `recorders`, `uploads` (`/id`), `audioFiles`, `detections` (`/uploadId`) | `azurerm_cosmosdb_sql_container` (`for_each`) | `cosmos.tf` |
| 6 | Cosmos Built-in Data Contributor → identity, database scope | `azurerm_cosmosdb_sql_role_assignment` | `cosmos.tf` |
| 7 | Storage account (shared keys off) + `audio` container + lifecycle policy (the backstop, not the retention policy) | `azurerm_storage_account`, `azurerm_storage_container`, `azurerm_storage_management_policy` | `storage.tf` |
| 8 | Storage Blob Data Contributor → identity, and → whoever runs Terraform | `azurerm_role_assignment` ×2 | `storage.tf` |
| 9 | Container registry (Basic, admin off) + AcrPull → identity | `azurerm_container_registry`, `azurerm_role_assignment` | `registry.tf` |
| 10 | Log Analytics workspace | `azurerm_log_analytics_workspace` | `app.tf` |
| 11 | Container Apps environment | `azurerm_container_app_environment` | `app.tf` |
| 12 | Container app (env vars incl. `BIRDSENSE_BOOTSTRAP_ADMIN` and `BIRDSENSE_AUDIO_RETENTION_DAYS`, three probes with every value set, exactly one replica, `depends_on` the role assignments) | `azurerm_container_app` | `app.tf` |

**Variables** (`variables.tf`): `image_tag` and `bootstrap_admin` are required
and have no default; `env`, `location`, `name_suffix`, `cpu`, `memory`,
`log_retention_days`, `audio_retention_days`, `audio_backstop_days` and
`grant_operator_blob_access` have the defaults this file describes. A `staging` copy is a second tfvars file with `env = "staging"`.

**Outputs** (`outputs.tf`): `app_url` and `app_fqdn`, `acr_name` (which
`scripts/deploy.ps1` reads) and `acr_login_server`, `cosmos_endpoint`,
`blob_endpoint`, `identity_client_id`, `resource_group`, and `image_tag` --
what is deployed right now, so a settings change can be applied without a
rebuild.
