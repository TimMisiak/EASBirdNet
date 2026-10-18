# The app itself: one container serving the API and the frontend. Analysis
# runs as the job in job.tf, which this app starts -- or, with
# analysis_job_enabled off, in this container's own process.

resource "azurerm_log_analytics_workspace" "this" {
  name                = "log-${local.base}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  sku                 = "PerGB2018"
  retention_in_days   = var.log_retention_days
  tags                = local.tags
}

resource "azurerm_container_app_environment" "this" {
  name                       = "cae-${local.base}"
  resource_group_name        = azurerm_resource_group.this.name
  location                   = azurerm_resource_group.this.location
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id
  tags                       = local.tags
  # No workload_profile_name: Consumption, the default.
}

resource "azurerm_container_app" "this" {
  name                         = local.app_name
  resource_group_name          = azurerm_resource_group.this.name
  container_app_environment_id = azurerm_container_app_environment.this.id
  revision_mode                = "Single"
  tags                         = local.tags

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.this.id]
  }

  # Sign-in secrets. Container Apps keeps these out of the revision's
  # environment listing; the container reads them through secret_name below.
  secret {
    name  = "oidc-microsoft-client-secret"
    value = var.oidc_microsoft_client_secret
  }
  # Google is optional, and a Container Apps secret can't be empty.
  dynamic "secret" {
    for_each = var.oidc_google_client_id != "" ? [1] : []
    content {
      name  = "oidc-google-client-secret"
      value = var.oidc_google_client_secret
    }
  }
  secret {
    name  = "session-key"
    value = var.session_key
  }

  # Pulls with AcrPull as the identity above; no password, no admin user.
  registry {
    server   = azurerm_container_registry.this.login_server
    identity = azurerm_user_assigned_identity.this.id
  }

  ingress {
    external_enabled           = true
    target_port                = 8080
    transport                  = "auto"
    allow_insecure_connections = false

    traffic_weight {
      latest_revision = true
      percentage      = 100
    }
  }

  template {
    # Both bounds are 1 on purpose, and neither is a cost knob:
    #
    #   min 1 -- work runs in the background with no HTTP traffic: the
    #   retention sweep, the backstop that starts analysis workers for work
    #   left waiting (or, without the job, the analysis queue itself). A
    #   scale-to-zero replica would stop all of it until someone visits.
    #
    #   max 1 -- CORRECTNESS, not cost. tusd holds an upload's lock in the
    #   memory of the replica serving it, so every request for one upload has
    #   to reach the same process; two replicas would also analyze the same
    #   file twice. Raising this needs a shared locker in internal/storage (or
    #   ingress session affinity) first. See CLAUDE.md and DEPLOYMENT.md.
    #
    # max 1 bounds a *revision*, not the app, so during any swap -- every
    # deploy, and every rollback -- the outgoing and incoming replicas briefly
    # run together, both running the queue and holding their own in-memory tus
    # locks. That overlap costs duplicated work, not consistency: the tus
    # offset is read from the blob on every request rather than held in a
    # process, and every analysis write is idempotent. Nothing here needs
    # serializing (ROLLBACK.md, *The two revisions overlap*).
    min_replicas = 1
    max_replicas = 1

    container {
      name = "birdsense"
      # Terraform owns the running image: a deploy is a new image_tag, applied
      # here (scripts/deploy.ps1). Nothing should `az containerapp update` this
      # app -- that is drift the next apply reverts.
      # The web image serves; the analysis image also has BirdNET, for when
      # the app analyzes by itself (analysis_job_enabled off).
      image = "${azurerm_container_registry.this.login_server}/${var.analysis_job_enabled ? "birdsense" : "birdsense-analyzer"}:${var.image_tag}"

      # With the job, sized for serving pages and uploads. Without it, sized
      # for analysis too: CPU-bound for hours per card.
      cpu    = var.cpu
      memory = var.memory

      env {
        name  = "BIRDSENSE_DB"
        value = "cosmos" # the default; set explicitly so the config reads plainly
      }
      env {
        name  = "BIRDSENSE_COSMOS_ENDPOINT"
        value = azurerm_cosmosdb_account.this.endpoint
      }
      env {
        name  = "BIRDSENSE_COSMOS_DATABASE"
        value = azurerm_cosmosdb_sql_database.this.name
      }
      env {
        name  = "BIRDSENSE_STORAGE"
        value = "azure" # the default outside dev mode; set explicitly
      }
      env {
        name  = "BIRDSENSE_BLOB_ENDPOINT"
        value = azurerm_storage_account.this.primary_blob_endpoint
      }
      env {
        name  = "BIRDSENSE_BLOB_CONTAINER"
        value = azurerm_storage_container.audio.name
      }
      # How long a card's original recordings are kept once BirdNET has
      # finished with them. The app does the deleting (internal/retention);
      # the lifecycle rule in storage.tf is only a backstop.
      env {
        name  = "BIRDSENSE_AUDIO_RETENTION_DAYS"
        value = tostring(var.audio_retention_days)
      }
      # Perch as a second model after BirdNET. The image always carries it;
      # this is the switch (DEPLOYMENT.md, *Perch*).
      env {
        name  = "BIRDSENSE_PERCH"
        value = var.perch_enabled ? "on" : "off"
      }
      # So the analysis performance records under perf/ say which image they
      # measured (ANALYSIS.md, *Performance data*). They are on by default.
      env {
        name  = "BIRDSENSE_IMAGE_TAG"
        value = var.image_tag
      }
      # The replica's size, for the same records: the process can't see its
      # own limits in Container Apps, only the machine's.
      env {
        name  = "BIRDSENSE_REPLICA_CPU"
        value = tostring(var.cpu)
      }
      env {
        name  = "BIRDSENSE_REPLICA_MEMORY"
        value = var.memory
      }
      # How much analysis runs at once, and what each model is expected to
      # take: BIRDSENSE_ANALYSIS_* by its suffix (ANALYSIS.md, *Packing*).
      # Empty, the server's defaults apply.
      dynamic "env" {
        for_each = var.analysis_tuning
        content {
          name  = "BIRDSENSE_ANALYSIS_${upper(env.key)}"
          value = env.value
        }
      }
      # With the job, the app starts its workers instead of analyzing: as a
      # card's last file lands, and every few minutes as a backstop.
      dynamic "env" {
        for_each = var.analysis_job_enabled ? [1] : []
        content {
          name  = "BIRDSENSE_ANALYSIS_JOB"
          value = azurerm_container_app_job.analysis.id
        }
      }
      dynamic "env" {
        for_each = var.analysis_job_enabled ? [1] : []
        content {
          name  = "BIRDSENSE_ANALYSIS_JOB_WORKERS"
          value = tostring(var.analysis_max_workers)
        }
      }
      # Tells the SDK *which* managed identity to use. Required for a
      # user-assigned one.
      env {
        name  = "AZURE_CLIENT_ID"
        value = azurerm_user_assigned_identity.this.client_id
      }
      # Stops DefaultAzureCredential trying developer credentials first.
      env {
        name  = "AZURE_TOKEN_CREDENTIALS"
        value = "ManagedIdentityCredential"
      }
      # Only ever adds someone if the roster is empty, so it is safe to leave
      # set. See DEPLOYMENT.md, First deploy.
      env {
        name  = "BIRDSENSE_BOOTSTRAP_ADMIN"
        value = var.bootstrap_admin
      }
      # Sign-in. PUBLIC_URL is where the identity provider sends people back
      # to, and has to match a redirect URI registered with the provider: the
      # container app's own hostname until a custom domain exists, then the
      # custom domain. It is configuration rather than the request's Host
      # header on purpose (see internal/api/auth.go).
      env {
        name  = "BIRDSENSE_PUBLIC_URL"
        value = var.public_url != "" ? var.public_url : "https://${local.app_name}.${azurerm_container_app_environment.this.default_domain}"
      }
      env {
        name  = "BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID"
        value = var.oidc_microsoft_client_id
      }
      env {
        name        = "BIRDSENSE_OIDC_MICROSOFT_CLIENT_SECRET"
        secret_name = "oidc-microsoft-client-secret"
      }
      # "common" accepts any organization and any personal Microsoft account,
      # which is what volunteers with their own addresses need. A tenant id
      # here restricts sign-in to that one directory.
      env {
        name  = "BIRDSENSE_OIDC_MICROSOFT_TENANT"
        value = var.oidc_microsoft_tenant
      }
      # Google, if configured. The server offers a provider when its client id
      # is set, and the sign-in page grows a button for it on its own.
      dynamic "env" {
        for_each = var.oidc_google_client_id != "" ? [1] : []
        content {
          name  = "BIRDSENSE_OIDC_GOOGLE_CLIENT_ID"
          value = var.oidc_google_client_id
        }
      }
      dynamic "env" {
        for_each = var.oidc_google_client_id != "" ? [1] : []
        content {
          name        = "BIRDSENSE_OIDC_GOOGLE_CLIENT_SECRET"
          secret_name = "oidc-google-client-secret"
        }
      }
      # Signs the session cookie. Changing it signs everyone out.
      env {
        name        = "BIRDSENSE_SESSION_KEY"
        secret_name = "session-key"
      }
      # BIRDSENSE_ADDR, BIRDSENSE_STATIC_DIR, BIRDSENSE_STORAGE_DIR and the
      # BirdNET paths are already set in the image. BIRDSENSE_COSMOS_KEY is for
      # the emulator and must never be set here.

      # Probes. Every field is set on purpose: the provider's defaults are a
      # 1-second timeout and three failures 10 seconds apart, which is the
      # right shape for a web app and the wrong one for this container. The
      # same process runs BirdNET CPU-bound for hours on one vCPU (cpu above),
      # so a health response that waits behind it is normal rather than a sick
      # replica -- and there is exactly one replica, so a restart kills the
      # BirdNET run in flight and takes the site down with it.

      # Startup: /api/v1/health answers as soon as the server is listening,
      # which is after Cosmos and the blob container have answered or the
      # process has exited. ~100 s is room for a cold start, not for a broken
      # configuration -- that fails by exiting, and restarts the container.
      startup_probe {
        transport               = "HTTP"
        port                    = 8080
        path                    = "/api/v1/health"
        initial_delay           = 5
        timeout                 = 5
        interval_seconds        = 10
        failure_count_threshold = 10
      }

      # Readiness: /api/v1/ready is the one route that can fail. It pings
      # Cosmos and the blob container (internal/api, ready), so a replica that
      # has lost either leaves ingress after ~90 s instead of serving 500s,
      # and is back the first time a ping succeeds. It says nothing about the
      # analysis queue: a stuck queue is health's to report, and taking the
      # site down over it would help nobody.
      readiness_probe {
        transport               = "HTTP"
        port                    = 8080
        path                    = "/api/v1/ready"
        timeout                 = 5
        interval_seconds        = 30
        failure_count_threshold = 3
        success_count_threshold = 1
      }

      # Liveness: /api/v1/health, which answers 200 whatever the dependencies
      # are doing -- restarting a replica does not fix Cosmos, and this is the
      # replica analyzing a card. Only a process that has stopped answering at
      # all should restart the container, and five unbroken minutes of that is
      # the bar. This is the endpoint the Dockerfile HEALTHCHECK uses too.
      liveness_probe {
        transport               = "HTTP"
        port                    = 8080
        path                    = "/api/v1/health"
        initial_delay           = 10
        timeout                 = 5
        interval_seconds        = 30
        failure_count_threshold = 10
      }
    }
  }

  # Invisible to Terraform otherwise, and all three matter for the *first*
  # revision: without AcrPull the image pull fails, and without the data roles
  # the server exits at startup because it can't read the Cosmos containers or
  # reach the blob container. Role assignments still take a minute or two to
  # propagate, so a first apply can race them -- if the first revision fails,
  # re-apply or restart the revision. It is not a config error.
  depends_on = [
    azurerm_role_assignment.app_acr_pull,
    azurerm_role_assignment.app_blob,
    azurerm_cosmosdb_sql_role_assignment.app,
    azurerm_cosmosdb_sql_container.this,
    azurerm_role_assignment.app_job_starter,
  ]

  # Perch's process tree peaks near 2 GB on its own (1.95 GB measured on a
  # real card), so beside the server and BirdNET on the 2Gi replica
  # the analysis would be killed on every file and retried until it failed.
  lifecycle {
    precondition {
      condition     = var.analysis_job_enabled || !var.perch_enabled || tonumber(trimsuffix(var.memory, "Gi")) >= 4
      error_message = "With analysis_job_enabled off, perch_enabled needs memory of at least 4Gi (Perch peaks near 2 GB); Container Apps pairs 4Gi with cpu 2.0. Set memory = \"4Gi\" and cpu = 2.0, turn the job on, or leave Perch off."
    }
  }
}
