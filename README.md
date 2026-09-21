# EASBirdNet — Birdsense

Web app for Eastside Audubon's BirdNET listening stations: which birds were
heard, where, and when.

A single Go binary serves both the JSON API and the static frontend, so the
whole app runs as one self-contained container.

## Quick start

```sh
cd backend && BIRDSENSE_DB=local go run ./cmd/server     # http://localhost:8080
```

`BIRDSENSE_DB=local` keeps data in a JSON file; without it the server expects
Azure Cosmos DB (see [DEPLOYMENT.md](DEPLOYMENT.md)).

Or in Docker:

```sh
HOST_PORT=8080 docker compose up --build
```

## Checks

```sh
cd backend && gofmt -l . && go vet ./... && go test ./... && govulncheck ./...
```

`govulncheck` installs with
`go install golang.org/x/vuln/cmd/govulncheck@latest`. GitHub Actions runs all
four on every push and pull request, and again weekly so a new advisory against
unchanged code still surfaces.

## Deploying

Azure runs it as one container app, with documents in Cosmos DB and card audio
in Blob Storage. `infra/` builds all of that; [DEPLOYMENT.md](DEPLOYMENT.md)
explains every resource and setting in it.

Terraform owns the running image, so a deploy is one script:

```powershell
./scripts/deploy.ps1          # build this commit in ACR, apply the tag
```

It refuses a dirty working tree, because the image tag is the commit sha.

Rolling back is the same script with an earlier tag, which skips the build:

```powershell
./scripts/deploy.ps1 -ListTags           # what could I roll back to?
./scripts/deploy.ps1 -ImageTag a1b2c3d   # apply an existing tag, don't build
```

`-ListTags` deploys nothing: it prints what the registry holds against this
clone's git history, newest build first. [ROLLBACK.md](ROLLBACK.md) is the
procedure and what a rollback doesn't undo.

### One-time setup

Needs PowerShell 7+ (`pwsh`), the Azure CLI (`az login`) and Terraform 1.9+.
Deploying is the one thing here that runs on both Windows and Linux, which is
why it is PowerShell; everything else in this file is Linux-only. On Linux the
shebang makes `./scripts/deploy.ps1` work directly, or run
`pwsh ./scripts/deploy.ps1`.

The provider wants the subscription named explicitly, so either set
`subscription_id` in `prod.tfvars` or:

```powershell
$env:ARM_SUBSCRIPTION_ID = (az account show --query id -o tsv)
```

```powershell
# 1. A home for the Terraform state, made by hand: Terraform shouldn't own the
#    state it depends on. Any names will do; put them in backend.hcl below.
az group create -n rg-birdsense-tfstate -l westus2
az storage account create -n stbirdsensetfstate -g rg-birdsense-tfstate `
  -l westus2 --sku Standard_LRS --allow-shared-key-access false

# Creating the container is a data-plane call, so grant yourself the data role.
$me = (az ad signed-in-user show --query id -o tsv)
$state = (az storage account show -n stbirdsensetfstate `
  -g rg-birdsense-tfstate --query id -o tsv)
az role assignment create --role "Storage Blob Data Contributor" `
  --assignee $me --scope $state
az storage container create -n tfstate `
  --account-name stbirdsensetfstate --auth-mode login

# Keep every version of the state file: losing it means re-importing Cosmos
# and the storage account by hand.
az storage account blob-service-properties update -n stbirdsensetfstate `
  -g rg-birdsense-tfstate --enable-versioning true

# 2. Point Terraform at it, and name the first admin.
Copy-Item infra/backend.hcl.example infra/backend.hcl    # the names used above
Copy-Item infra/prod.tfvars.example infra/prod.tfvars    # set bootstrap_admin
terraform "-chdir=infra" init "-backend-config=backend.hcl"

# 3. The registry has to exist before the container app can run an image from
#    it, so create it first. (image_tag is unused here, but still required.)
terraform "-chdir=infra" apply "-var-file=prod.tfvars" "-var" "image_tag=none" `
  "-target=azurerm_container_registry.this"

# 4. Build the first image and create everything else.
./scripts/deploy.ps1
```

The quotes around Terraform's arguments are not decoration: PowerShell splits a
bareword like `-var-file=prod.tfvars` into `-var-file=prod` and `.tfvars`
before the command ever sees it. Any argument with both `=` and `.` in it needs
them.

`bootstrap_admin` is the coordinator who will run the program: a new database
has an empty roster, and only an admin can add people. It is used **only** if
the roster is empty, so it is safe to leave set — but a typo on the first
deploy has to be fixed in the Cosmos Data Explorer afterwards.

Role assignments take a minute or two to propagate, so the first revision can
fail to pull its image or to reach Cosmos. Re-apply; it isn't a config error.

To change a setting without rebuilding, apply with the tag that's already
running:

```powershell
$tag = (terraform "-chdir=infra" output -raw image_tag)
terraform "-chdir=infra" apply "-var-file=prod.tfvars" "-var" "image_tag=$tag"
```

See [CLAUDE.md](CLAUDE.md) for the architecture decisions, layout, and
conventions, and [SCHEMA.md](SCHEMA.md) for the data model.
