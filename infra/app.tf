# The app itself: one container serving the API and the frontend, with the
# analysis queue running BirdNET in the same process.

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
    #   min 1 -- the analysis queue runs in the background with no HTTP
    #   traffic, and a scale-to-zero replica is stopped mid-card. Nothing is
    #   lost (the queue is the Cosmos documents, and it resumes at the next
    #   start) but it stops until someone visits.
    #
    #   max 1 -- CORRECTNESS, not cost. tusd holds an upload's lock in the
    #   memory of the replica serving it, so every request for one upload has
    #   to reach the same process; two replicas would also analyze the same
    #   file twice. Raising this needs a shared locker in internal/storage (or
    #   ingress session affinity) first. See CLAUDE.md and DEPLOYMENT.md.
    min_replicas = 1
    max_replicas = 1

    container {
      name = "birdsense"
      # Terraform owns the running image: a deploy is a new image_tag, applied
      # here (scripts/deploy.sh). Nothing should `az containerapp update` this
      # app -- that is drift the next apply reverts.
      image = "${azurerm_container_registry.this.login_server}/birdsense:${var.image_tag}"

      # Sized for analysis, not for serving pages: one Python process at a
      # time, peaking near 300 MB, CPU-bound for hours per card. Moving
      # analysis to a Container Apps job would let this go back to 0.25/0.5Gi.
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
      # Signs the session cookie. Changing it signs everyone out.
      env {
        name        = "BIRDSENSE_SESSION_KEY"
        secret_name = "session-key"
      }
      # BIRDSENSE_ADDR, BIRDSENSE_STATIC_DIR, BIRDSENSE_STORAGE_DIR and the
      # BirdNET paths are already set in the image. BIRDSENSE_COSMOS_KEY is for
      # the emulator and must never be set here.

      # The same endpoint the Dockerfile HEALTHCHECK uses.
      startup_probe {
        transport = "HTTP"
        port      = 8080
        path      = "/api/v1/health"
      }
      readiness_probe {
        transport = "HTTP"
        port      = 8080
        path      = "/api/v1/health"
      }
      liveness_probe {
        transport     = "HTTP"
        port          = 8080
        path          = "/api/v1/health"
        initial_delay = 10
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
  ]
}
