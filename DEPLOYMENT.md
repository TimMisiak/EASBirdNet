# Birdsense — Azure deployment

This is the list of Azure resources Birdsense needs, with the settings that
matter. It is written to be turned into Terraform (`azurerm` provider) without
guessing: every resource names its Terraform type, and every setting that
differs from the default is spelled out. The data those resources hold is
described in [SCHEMA.md](SCHEMA.md).

Status: nothing here is provisioned yet. The app code supports the Cosmos DB
part; blob storage is listed because the upload flow needs it next, but no
code uses it yet.

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
| local_authentication_disabled | `true` | Entra ID only. No account keys to leak. |
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

Holds the audio. At ~128 GB per card, this is where the money goes (see *Cost*).

| Setting | Value | Why |
|---------|-------|-----|
| name | `stbirdsenseprod` | |
| account_kind | `StorageV2` | |
| account_tier / account_replication_type | `Standard` / `LRS` | Originals are recoverable from the card for a while, and results live in Cosmos. Revisit if the archive must survive a datacenter loss. |
| access_tier | `Hot` | Lifecycle rules move old audio down. |
| shared_access_key_enabled | `false` | Entra ID only. Browser uploads will use user-delegation SAS, which doesn't need account keys. |
| allow_nested_items_to_be_public | `false` | |
| min_tls_version | `TLS1_2` | |
| blob_properties.delete_retention_policy | 7 days | Soft delete. |
| blob_properties.cors_rule | *added with direct browser upload*: origin = the app's hostname, methods `PUT, OPTIONS`, headers `*` | Not needed yet. |

**Blob container** — `azurerm_storage_container`: name `audio`, access type
`private`. Blob names follow `uploads/{uploadId}/{path}` (SCHEMA.md).

**Lifecycle** — `azurerm_storage_management_policy`, one rule on prefix
`audio/uploads/`. The day counts are placeholders until the retention question
below is answered:

| Action | After |
|--------|-------|
| tier to cool | 30 days since last modification |
| tier to archive | 180 days |
| delete | *unset*: retention not decided |

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
| cpu / memory | `0.25` / `0.5Gi` | A Go binary serving small JSON and static files. |
| min_replicas / max_replicas | `0` / `1` | Scale to zero between visits (a cold start of a few seconds). **Keep max at 1** while `internal/api` still serves its in-memory placeholder data, or two replicas would disagree. Raise it once the handlers read from Cosmos. |
| ingress | external `true`, target_port `8080`, transport `auto`, allow_insecure_connections `false`, traffic 100% to latest revision | |
| liveness / readiness / startup probes | HTTP GET `/api/v1/health` on port `8080` | The same endpoint the Dockerfile `HEALTHCHECK` uses. |

**Environment variables:**

| Name | Value | Notes |
|------|-------|-------|
| `BIRDSENSE_DB` | `cosmos` | The default. Set explicitly so the config reads plainly. |
| `BIRDSENSE_COSMOS_ENDPOINT` | `azurerm_cosmosdb_account.this.endpoint` | e.g. `https://cosmos-birdsense-prod.documents.azure.com:443/` |
| `BIRDSENSE_COSMOS_DATABASE` | `birdsense` | |
| `AZURE_CLIENT_ID` | `azurerm_user_assigned_identity.this.client_id` | Tells the SDK *which* managed identity to use. Required for a user-assigned identity. |
| `AZURE_TOKEN_CREDENTIALS` | `ManagedIdentityCredential` | Stops `DefaultAzureCredential` trying developer credentials first in production. |
| `BIRDSENSE_ADDR`, `BIRDSENSE_STATIC_DIR` | *unset* | Already set in the image (`:8080`, `/app/frontend`). |

Never set `BIRDSENSE_COSMOS_KEY` in Azure. It exists only for the emulator.

**Custom domain** (optional, when chosen): `azurerm_container_app_custom_domain`
with a managed certificate, plus a CNAME and `asuid` TXT record at the DNS host.

## Ordering

Terraform infers most of this from references, but two dependencies are
invisible to it:

1. The **AcrPull** and **Cosmos SQL role** assignments must exist before the
   container app's first revision. Otherwise the image pull fails, or the app
   exits at startup because it can't read the containers. Add them to the app's
   `depends_on`.
2. Role assignments take a minute or two to propagate. A first `apply` can
   still race them. If the first revision fails, re-apply or restart the
   revision; it is not a config error.

## Deploying a new version

Until CI exists:

```sh
# from the repo root
az acr build --registry crbirdsenseprod --image birdsense:$(git rev-parse --short HEAD) .
az containerapp update --name ca-birdsense-prod --resource-group rg-birdsense-prod \
  --image crbirdsenseprod.azurecr.io/birdsense:$(git rev-parse --short HEAD)
```

Pick one owner for the image tag. If Terraform sets the image, an `az`
update is drift the next `apply` reverts. The usual split is
`lifecycle { ignore_changes = [template[0].container[0].image] }` in
Terraform, with deploys done by `az`/CI.

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
| Audio in blob storage | 130 cards × ~128 GB ≈ **17 TB** | **The dominant cost.** Hot LRS storage is several hundred dollars a month by the end of a year; cool roughly halves that and archive cuts it by about an order of magnitude. Retention and tiering decide the bill. |
| Cosmos DB, serverless | ~44k audio-file docs; detections depend on threshold (at 50 per file, ~2M docs, ~2 GB) | Low: a few dollars a year in request units, plus storage per GB-month. |
| Container Apps | low traffic, scale to zero | Usually within the monthly free grant. |
| Container Registry Basic | one small image | A few dollars a month. |
| Log Analytics | low volume | Within the free ingestion allowance at this scale. |

*Alternative for Cosmos.* One Cosmos account per subscription can use the free
tier (1000 RU/s and 25 GB free) with provisioned, database-level shared
throughput instead of serverless. It costs nothing until it's outgrown, but bulk
detection ingestion would be throttled (the SDK retries 429s). Worth it if the
account budget is tight; switching means a new account, since capacity mode
can't be changed in place.

## Open questions that change this file

- **Audio retention**: how long originals are kept, and in which tier. This
  sets the lifecycle rule and most of the bill.
- **Custom domain**: e.g. `birdsense.eastsideaudubon.org`, and who controls DNS.
- **Sign-in**: real Google/Microsoft OIDC will add app registrations and
  client secrets. Those go in Key Vault or Container Apps secrets, and get
  their own section here.
- **BirdNET processing**: where analysis runs (a Container Apps job triggered
  by a queue is the natural fit). It will add a queue, a job, and the same
  identity-based roles.
- **Email**: the upload flow promises "card received" and "results" emails,
  which needs Azure Communication Services or an external provider.

## Terraform checklist

| # | Resource | Terraform type |
|---|----------|----------------|
| 1 | Resource group | `azurerm_resource_group` |
| 2 | Managed identity | `azurerm_user_assigned_identity` |
| 3 | Cosmos account (serverless, key auth off, continuous 7-day backup) | `azurerm_cosmosdb_account` |
| 4 | Cosmos database `birdsense` | `azurerm_cosmosdb_sql_database` |
| 5 | Containers `users`, `recorders`, `uploads` (`/id`), `audioFiles`, `detections` (`/uploadId`) | `azurerm_cosmosdb_sql_container` |
| 6 | Cosmos Built-in Data Contributor → identity, database scope | `azurerm_cosmosdb_sql_role_assignment` |
| 7 | Storage account (shared keys off) + `audio` container + lifecycle policy | `azurerm_storage_account`, `azurerm_storage_container`, `azurerm_storage_management_policy` |
| 8 | Storage Blob Data Contributor → identity | `azurerm_role_assignment` |
| 9 | Container registry (Basic, admin off) + AcrPull → identity | `azurerm_container_registry`, `azurerm_role_assignment` |
| 10 | Log Analytics workspace | `azurerm_log_analytics_workspace` |
| 11 | Container Apps environment | `azurerm_container_app_environment` |
| 12 | Container app (env vars, probes, 0–1 replicas, `depends_on` the role assignments) | `azurerm_container_app` |

**Outputs**: container app FQDN, Cosmos endpoint, ACR login server, identity
client id.
