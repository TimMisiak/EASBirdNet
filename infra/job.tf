# The analysis job: BirdNET (and Perch, when it is on) run by workers the web
# app starts, rather than in the web app's own process (ANALYSIS.md). Each
# execution is one worker replica running `birdsense worker`, which claims
# files until there are none and exits; the web app starts as many as the
# waiting work needs (BIRDSENSE_ANALYSIS_JOB below).
#
# Same identity as the app: it needs the same data roles (Cosmos, the blob
# container, AcrPull), and the app needs to start this job, which the role at
# the bottom allows and nothing more.

resource "azurerm_container_app_job" "analysis" {
  name                         = "caj-${local.base}"
  resource_group_name          = azurerm_resource_group.this.name
  location                     = azurerm_resource_group.this.location
  container_app_environment_id = azurerm_container_app_environment.this.id
  tags                         = local.tags

  # A worker exits once it has had nothing to claim for two minutes, so this
  # only bounds a very long queue. Reached, the worker is stopped, lets go of
  # its files, and the web app starts another.
  replica_timeout_in_seconds = var.analysis_replica_timeout_seconds
  # A failed worker isn't retried as the same execution: the web app's
  # backstop starts a new one if the work is still waiting.
  replica_retry_limit = 0

  # Started by the web app, one replica per execution, so the app decides
  # how many run at once.
  manual_trigger_config {
    parallelism              = 1
    replica_completion_count = 1
  }

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.this.id]
  }

  registry {
    server   = azurerm_container_registry.this.login_server
    identity = azurerm_user_assigned_identity.this.id
  }

  template {
    container {
      name = "worker"
      # The same tag as the app's image, from the same commit (deploy.ps1).
      image   = "${azurerm_container_registry.this.login_server}/birdsense-analyzer:${var.image_tag}"
      command = ["/app/birdsense"]
      args    = ["worker"]
      cpu     = var.analysis_cpu
      memory  = var.analysis_memory

      # What the worker shares with the app (app.tf): the data it works on,
      # the identity it signs in as, and the analysis settings.
      env {
        name  = "BIRDSENSE_DB"
        value = "cosmos"
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
        value = "azure"
      }
      env {
        name  = "BIRDSENSE_BLOB_ENDPOINT"
        value = azurerm_storage_account.this.primary_blob_endpoint
      }
      env {
        name  = "BIRDSENSE_BLOB_CONTAINER"
        value = azurerm_storage_container.audio.name
      }
      env {
        name  = "BIRDSENSE_PERCH"
        value = var.perch_enabled ? "on" : "off"
      }
      env {
        name  = "BIRDSENSE_IMAGE_TAG"
        value = var.image_tag
      }
      # The worker's replica size, which it packs BirdNET and Perch into.
      env {
        name  = "BIRDSENSE_REPLICA_CPU"
        value = tostring(var.analysis_cpu)
      }
      env {
        name  = "BIRDSENSE_REPLICA_MEMORY"
        value = var.analysis_memory
      }
      dynamic "env" {
        for_each = var.analysis_tuning
        content {
          name  = "BIRDSENSE_ANALYSIS_${upper(env.key)}"
          value = env.value
        }
      }
      env {
        name  = "AZURE_CLIENT_ID"
        value = azurerm_user_assigned_identity.this.client_id
      }
      env {
        name  = "AZURE_TOKEN_CREDENTIALS"
        value = "ManagedIdentityCredential"
      }
    }
  }

  depends_on = [
    azurerm_role_assignment.app_acr_pull,
    azurerm_role_assignment.app_blob,
    azurerm_cosmosdb_sql_role_assignment.app,
  ]

  lifecycle {
    # Perch's process tree peaks near 2 GB on its own; below 4Gi a worker
    # would be killed on every Perch file.
    precondition {
      condition     = !var.perch_enabled || tonumber(trimsuffix(var.analysis_memory, "Gi")) >= 4
      error_message = "perch_enabled needs analysis_memory of at least 4Gi (Perch peaks near 2 GB); Container Apps pairs 4Gi with cpu 2.0 and 8Gi with 4.0."
    }
  }
}

# Just enough for the app to start the job and see its executions -- not to
# change it, stop it, or touch anything else in the group.
resource "azurerm_role_definition" "job_starter" {
  name        = "Birdsense analysis job starter (${local.base})"
  scope       = azurerm_resource_group.this.id
  description = "Start executions of the Birdsense analysis job and list them. Given to the web app's identity (ANALYSIS.md, Starting the job)."

  permissions {
    actions = [
      "Microsoft.App/jobs/read",
      "Microsoft.App/jobs/start/action",
      "Microsoft.App/jobs/executions/read",
    ]
  }

  assignable_scopes = [azurerm_resource_group.this.id]
}

resource "azurerm_role_assignment" "app_job_starter" {
  scope              = azurerm_container_app_job.analysis.id
  role_definition_id = azurerm_role_definition.job_starter.role_definition_resource_id
  principal_id       = azurerm_user_assigned_identity.this.principal_id
}
