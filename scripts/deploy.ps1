#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Deploy Birdsense: build the image in ACR, then apply the tag with Terraform.

.DESCRIPTION
    Terraform owns the running image, so this is the whole deploy -- nothing
    here runs `az containerapp update`, which would be drift the next apply
    reverts. One-time setup, and what to do the first time, is in README.md;
    rolling back to an earlier image is ROLLBACK.md.

.PARAMETER AllowDirty
    Deploy a dirty working tree. The tag gets a timestamp appended, so it is
    still unique and still creates a new revision.

.PARAMETER ImageTag
    Deploy an image that is already in the registry, instead of building this
    commit. This is the rollback: pass a tag an earlier deploy pushed and the
    script skips git and the build entirely, so it neither needs a clean tree
    nor waits on `az acr build`. The tag has to exist in the registry already;
    if it doesn't, the script says so and lists what is there.

.PARAMETER ListTags
    Print the rollback menu and exit, deploying nothing: what the registry has,
    newest build first, each one matched against this clone's git history, with
    the running tag marked.

.EXAMPLE
    ./scripts/deploy.ps1

.EXAMPLE
    ./scripts/deploy.ps1 -ListTags

.EXAMPLE
    ./scripts/deploy.ps1 -ImageTag a1b2c3d
#>

[CmdletBinding(DefaultParameterSetName = 'Build')]
param(
    [Parameter(ParameterSetName = 'Build')]
    [switch]$AllowDirty,

    [Parameter(ParameterSetName = 'Rollback', Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$ImageTag,

    [Parameter(ParameterSetName = 'List', Mandatory = $true)]
    [switch]$ListTags
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

# How many images the rollback menu shows. Deploys are rare enough that ten is
# weeks of them, and a rollback older than that is a decision, not a reflex.
$script:MenuSize = 10

# Whether the menu could be annotated at all, set by Get-ImageMenu. A tag with
# no commit behind it means two different things -- git isn't here to ask, or
# it is and has never heard of this sha -- and only the second is interesting.
$script:HaveGit = $false

# The rollback menu. A tag is a commit, so the registry and git history
# together say both what was actually built and what each one *was* --
# `az acr repository show-tags` alone gives a column of shas, which is no help
# when the question is which one to go back to. `--orderby time_desc` is build
# order, which is the order someone rolling back thinks in; git supplies the
# date and subject. A tag can outlive the commit it was built from (a branch
# that was never merged, a clone that hasn't fetched), so git not knowing one
# is reported rather than hidden: it is still deployable, you just can't read
# what it is from here.
function Get-ImageMenu([string]$Acr, [int]$Count) {
    $tags = @(az acr repository show-tags --name $Acr --repository birdsense `
            --orderby time_desc --top $Count --output tsv 2>$null)
    if ($LASTEXITCODE -ne 0) {
        return $null
    }

    $script:HaveGit = [bool](Get-Command git -ErrorAction SilentlyContinue)
    $menu = @()
    foreach ($raw in $tags) {
        $tag = $raw.Trim()
        if ($tag -eq '') { continue }

        # `<sha>-dirty-<timestamp>`, from -AllowDirty: the commit is the part
        # in front, and what the tree held on top of it is unknowable from here.
        $dirty = $tag -match '-dirty-\d+$'
        $sha = $tag -replace '-dirty-\d+$', ''

        $entry = [pscustomobject]@{
            Tag     = $tag
            Dirty   = $dirty
            InGit   = $false
            OnHead  = $false
            Date    = ''
            Subject = ''
        }

        if ($script:HaveGit) {
            $line = (git show -s '--format=%cs%x09%s' "$sha^{commit}" 2>$null)
            if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($line)) {
                $parts = ([string]$line) -split "`t", 2
                $entry.InGit = $true
                $entry.Date = $parts[0]
                $entry.Subject = if ($parts.Count -gt 1) { $parts[1] } else { '' }
                git merge-base --is-ancestor "$sha^{commit}" HEAD 2>$null | Out-Null
                $entry.OnHead = ($LASTEXITCODE -eq 0)
            }
        }
        $menu += $entry
    }
    return , $menu
}

function Format-ImageMenu($Menu, [string]$Running) {
    $lines = @()
    foreach ($entry in $Menu) {
        $mark = if ($Running -and $entry.Tag -eq $Running) { '*' } else { ' ' }

        $subject = $entry.Subject
        if ($subject.Length -gt 52) {
            $subject = $subject.Substring(0, 51) + '...'
        }

        $what =
        if (-not $entry.InGit) { if ($script:HaveGit) { 'not a commit in this clone' } else { '' } }
        elseif ($entry.OnHead) { '{0}  {1}' -f $entry.Date, $subject }
        else { '{0}  {1}  [not on HEAD]' -f $entry.Date, $subject }

        if ($entry.Dirty) { $what = "$what  [dirty build]" }

        $lines += '  {0} {1,-26} {2}' -f $mark, $entry.Tag, $what
    }
    # One string, not the array: a pipeline unrolls an array of lines into
    # Write-Host one way and a single-line menu another.
    return ($lines -join [Environment]::NewLine)
}

# What Terraform currently has applied, or $null if it can't be read (no state
# access, nothing applied yet). Only ever used to annotate or warn, so a
# failure here is never fatal.
function Get-RunningTag {
    $running = (Invoke-Terraform 'output' '-raw' 'image_tag' 2>$null)
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($running)) {
        return $null
    }
    return ([string]$running).Trim()
}

$rollback = $PSCmdlet.ParameterSetName -eq 'Rollback'
$listOnly = $PSCmdlet.ParameterSetName -eq 'List'

$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    # Only a build needs git (the tag is HEAD); -ListTags reads it if it's
    # there and says so if it isn't.
    $tools = if ($rollback -or $listOnly) { @('az', 'terraform') } else { @('az', 'terraform', 'git') }
    foreach ($tool in $tools) {
        if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
            Die "$tool isn't installed, or isn't on PATH"
        }
    }

    # Both of the other modes end in an apply, which reads it. -ListTags
    # deploys nothing, so it works on a machine that has no secrets at all.
    if (-not $listOnly -and -not (Test-Path 'infra/prod.tfvars')) {
        Die 'infra/prod.tfvars is missing; copy infra/prod.tfvars.example and fill it in'
    }

    if ($rollback) {
        # A rollback runs from whatever tree the person is standing in, which
        # on a bad day is the one they were mid-fix in. The image already
        # exists, so nothing here depends on git at all.
        $tag = $ImageTag.Trim()
    }
    elseif ($listOnly) {
        $tag = $null
    }
    else {
        # The image tag is the commit, so what's running is always something
        # that exists in git. A dirty tree would otherwise deploy code nobody
        # can check out -- and re-pushing a tag the app already runs creates no
        # new revision at all, since Container Apps keys revisions off the
        # image string. Hence the unique suffix when you insist.
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

    if ($listOnly) {
        $menu = Get-ImageMenu $acr $script:MenuSize
        if ($null -eq $menu) {
            Die "could not list the images in $acr"
        }
        if ($menu.Count -eq 0) {
            Die "$acr has no birdsense images yet"
        }

        $running = Get-RunningTag
        Write-Host "deploy: birdsense images in ${acr}, newest build first"
        Write-Host (Format-ImageMenu $menu $running)
        if ($running) {
            Write-Host "deploy: * is the tag Terraform has applied ($running)"
        }
        if (-not $script:HaveGit) {
            Write-Host 'deploy: git is not on PATH, so these are tags with no commits behind them'
        }
        Write-Host 'deploy: roll one back with ./scripts/deploy.ps1 -ImageTag <tag>'
        exit 0
    }

    if ($rollback) {
        # Applying a tag that isn't in the registry is a revision that can't
        # pull its image, found out minutes later from Container Apps. Ask
        # first, and show the menu -- "which sha do I go back to" is the
        # question a rollback starts with, and a typo'd sha and a sha that was
        # never built look identical until something lists them.
        az acr repository show --name $acr --image "birdsense:$tag" --output none 2>$null
        if ($LASTEXITCODE -ne 0) {
            Write-Host "deploy: birdsense:$tag isn't in $acr" -ForegroundColor Red
            $menu = Get-ImageMenu $acr $script:MenuSize
            if ($null -eq $menu -or $menu.Count -eq 0) {
                Die "and its images couldn't be listed; try: az acr repository show-tags --name $acr --repository birdsense"
            }
            Write-Host 'deploy: what is there, newest build first:'
            Write-Host (Format-ImageMenu $menu (Get-RunningTag))
            Die 'nothing was deployed'
        }

        # Container Apps keys revisions off the image string, so applying the
        # tag that is already applied produces no new revision at all. Worth
        # saying out loud in a rollback, where "nothing happened" is exactly
        # the symptom being chased.
        $running = Get-RunningTag
        if ($running -and $running -eq $tag) {
            Write-Host "deploy: warning -- $tag is already the applied tag; this creates no new revision" -ForegroundColor Yellow
        }
        Write-Host "deploy: birdsense:$tag is already built; skipping the build"
    }
    else {
        # `az acr build` uploads the whole build context to the registry, and
        # infra/prod.tfvars -- sitting right here, required above -- holds the
        # OIDC client secret and the session key. .dockerignore keeps them out
        # by being an allow-list: everything excluded, then the three
        # directories the Dockerfile copies added back. That property lives or
        # dies on the bare `*` coming first, so check it here rather than find
        # out from ACR.
        $ignore = @(Get-Content '.dockerignore' | Where-Object { $_.Trim() -ne '' -and -not $_.TrimStart().StartsWith('#') })
        if ($ignore.Count -eq 0 -or $ignore[0].Trim() -ne '*') {
            Die '.dockerignore is no longer an allow-list (it must start with `*`); the build context would carry infra/prod.tfvars to ACR'
        }
        foreach ($line in $ignore) {
            if ($line.Trim() -match '^!\s*(infra|scripts)\b') {
                Die ".dockerignore re-includes $($line.Trim()) -- that sends secrets or Terraform state to ACR"
            }
        }

        # Built in ACR rather than locally: no local Docker, the model download
        # in the Dockerfile's birdnet stage happens on Azure's network rather
        # than yours, and the result is linux/amd64 whatever this machine is.
        # An arm64 image (an Apple Silicon `docker build`) starts and dies in
        # Container Apps with an exec format error.
        Write-Host "deploy: building birdsense:$tag in $acr"
        az acr build --registry $acr --image "birdsense:$tag" --platform 'linux/amd64' '.'
        Assert-LastExitCode 'az acr build'
    }

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
