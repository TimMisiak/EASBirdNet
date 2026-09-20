# Card audio, and the clips the analysis queue cuts for review. At ~128 GB per
# card this is where the money goes -- see DEPLOYMENT.md, Cost.

resource "azurerm_storage_account" "this" {
  name                = "st${local.compact}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tags                = local.tags

  account_kind = "StorageV2"
  account_tier = "Standard"
  # Originals are recoverable from the card for a while, and results live in
  # Cosmos. Revisit if the archive has to survive losing a datacenter.
  account_replication_type = "LRS"
  access_tier              = "Hot" # the lifecycle rule below moves old audio down

  # Entra ID only. Browsers upload through the app, which writes with its
  # managed identity, so nothing needs an account key or a SAS.
  shared_access_key_enabled       = false
  allow_nested_items_to_be_public = false
  min_tls_version                 = "TLS1_2"

  blob_properties {
    delete_retention_policy {
      days = 7
    }
    # No cors_rule: browsers never talk to the storage account.
  }
}

# Terraform owns the container. The app also tries to create it at startup and
# carries on if it already exists.
#
# Creating it is a data-plane call, and shared keys are off, so the principal
# running Terraform needs the data role below -- not just Owner on the
# subscription, which grants no data actions.
resource "azurerm_storage_container" "audio" {
  name                  = "audio"
  storage_account_id    = azurerm_storage_account.this.id
  container_access_type = "private"

  depends_on = [azurerm_role_assignment.operator_blob]
}

# The app's own access: card audio, .info blobs, and clips.
resource "azurerm_role_assignment" "app_blob" {
  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.this.principal_id
}

# Whoever runs Terraform, so the provider can create the container above. This
# is also the role to give yourself for the local-against-Azure storage check
# in DEPLOYMENT.md.
resource "azurerm_role_assignment" "operator_blob" {
  count = var.grant_operator_blob_access ? 1 : 0

  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = data.azurerm_client_config.current.object_id
}

# One rule, on the originals only: reviewers play clips on demand, and a clip
# is what the retention policy keeps for good, so audio/clips/ is not named
# here at all and stays hot.
#
# The app is what deletes originals (backend/internal/retention, after
# var.audio_retention_days), because only it knows whether BirdNET has
# finished with a file and it has to mark the document either way. This rule
# is the net underneath that: it sweeps what the app never records -- uploads
# abandoned part way, and anything a failed delete left behind -- and it is set
# far enough out (var.audio_backstop_days) that it is never what removes a card
# in the normal course of things.
#
# No tier_to_archive: an archived blob can't be read without a rehydrate, and
# internal/analysis reads originals straight out of the container. Cool is
# fine, and by var.audio_retention_days the app has usually deleted the blob
# anyway; what this tiers is the stragglers the rule below eventually deletes,
# which are always older than cool's 30-day early-deletion charge.
resource "azurerm_storage_management_policy" "this" {
  storage_account_id = azurerm_storage_account.this.id

  rule {
    name    = "audio-originals"
    enabled = true

    filters {
      prefix_match = ["audio/uploads/"]
      blob_types   = ["blockBlob"]
    }

    actions {
      base_blob {
        tier_to_cool_after_days_since_modification_greater_than = 30
        delete_after_days_since_modification_greater_than       = var.audio_backstop_days
      }
    }
  }
}
