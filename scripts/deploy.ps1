#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Deploy Birdsense: build the image in ACR, then apply the tag with Terraform.

.DESCRIPTION
    Terraform owns the running image, so this is the whole deploy -- nothing
    here runs `az containerapp update`, which would be drift the next apply
    reverts. One-time setup, and what to do the first time, is in README.md.

.PARAMETER AllowDirty
    Deploy a dirty working tree. The tag gets a timestamp appended, so it is
    still unique and still creates a new revision.

.EXAMPLE
    ./scripts/deploy.ps1
#>

[CmdletBinding()]
param(
    [switch]$AllowDirty
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Die([string]$Message) {
    Write-Host "deploy: $Message" -ForegroundColor Red
    exit 1
}

# Native commands don't fail the script on their own before PowerShell 7.4,
# so every az/terraform/git call checks its exit code.
function Assert-LastExitCode([string]$What) {
    if ($LASTEXITCODE -ne 0) {
        Die "$What failed (exit code $LASTEXITCODE)"
    }
}

# PowerShell splits a bareword like -var-file=prod.tfvars into two arguments
# at the dot, for native commands as well as functions, so every argument that
# contains `=` or `.` is passed as a quoted string. Terraform would otherwise
# see `-var-file=prod` and a stray `.tfvars`.
function Invoke-Terraform {
    terraform '-chdir=infra' @args
}

$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    foreach ($tool in 'az', 'terraform', 'git') {
        if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
            Die "$tool isn't installed, or isn't on PATH"
        }
    }

    if (-not (Test-Path 'infra/prod.tfvars')) {
        Die 'infra/prod.tfvars is missing; copy infra/prod.tfvars.example and fill it in'
    }

    # The image tag is the commit, so what's running is always something that
    # exists in git. A dirty tree would otherwise deploy code nobody can check
    # out -- and re-pushing a tag the app already runs creates no new revision
    # at all, since Container Apps keys revisions off the image string. Hence
    # the unique suffix when you insist.
    $sha = (git rev-parse --short HEAD)
    Assert-LastExitCode 'git rev-parse'

    $dirty = (git status --porcelain)
    Assert-LastExitCode 'git status'

    if ([string]::IsNullOrWhiteSpace($dirty)) {
        $tag = $sha
    }
    elseif ($AllowDirty) {
        $tag = '{0}-dirty-{1}' -f $sha, [DateTime]::UtcNow.ToString('yyyyMMddHHmmss')
    }
    else {
        Die 'working tree is dirty: commit first, or pass -AllowDirty to deploy it anyway'
    }

    $acr = $env:BIRDSENSE_ACR
    if ([string]::IsNullOrWhiteSpace($acr)) {
        $acr = (Invoke-Terraform 'output' '-raw' 'acr_name' 2>$null)
        if ($LASTEXITCODE -ne 0) { $acr = $null }
    }
    if ([string]::IsNullOrWhiteSpace($acr)) {
        # A targeted apply doesn't always write outputs, so the very first
        # deploy may not be able to read this. The name is `cr` + birdsense +
        # env + any suffix.
        Die 'could not read acr_name from Terraform (first deploy?): run the one-time setup in README.md, or pass it, e.g. $env:BIRDSENSE_ACR = "crbirdsenseprod"'
    }

    # `az acr build` uploads the whole build context to the registry, and
    # infra/prod.tfvars -- sitting right here, required above -- holds the OIDC
    # client secret and the session key. .dockerignore keeps them out by being
    # an allow-list: everything excluded, then the three directories the
    # Dockerfile copies added back. That property lives or dies on the bare `*`
    # coming first, so check it here rather than find out from ACR.
    $ignore = @(Get-Content '.dockerignore' | Where-Object { $_.Trim() -ne '' -and -not $_.TrimStart().StartsWith('#') })
    if ($ignore.Count -eq 0 -or $ignore[0].Trim() -ne '*') {
        Die '.dockerignore is no longer an allow-list (it must start with `*`); the build context would carry infra/prod.tfvars to ACR'
    }
    foreach ($line in $ignore) {
        if ($line.Trim() -match '^!\s*(infra|scripts)\b') {
            Die ".dockerignore re-includes $($line.Trim()) -- that sends secrets or Terraform state to ACR"
        }
    }

    # Built in ACR rather than locally: no local Docker, the model download in
    # the Dockerfile's birdnet stage happens on Azure's network rather than
    # yours, and the result is linux/amd64 whatever this machine is. An arm64
    # image (an Apple Silicon `docker build`) starts and dies in Container Apps
    # with an exec format error.
    Write-Host "deploy: building birdsense:$tag in $acr"
    az acr build --registry $acr --image "birdsense:$tag" --platform 'linux/amd64' '.'
    Assert-LastExitCode 'az acr build'

    Write-Host "deploy: applying image_tag=$tag"
    Invoke-Terraform apply '-var-file=prod.tfvars' '-var' "image_tag=$tag"
    Assert-LastExitCode 'terraform apply'

    $url = (Invoke-Terraform 'output' '-raw' 'app_url')
    Assert-LastExitCode 'terraform output'
    Write-Host "deploy: $tag is live at $url" -ForegroundColor Green
}
finally {
    Pop-Location
}
