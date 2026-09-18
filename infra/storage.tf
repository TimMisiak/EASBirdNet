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

# One rule, on the originals only: reviewers play clips on demand, so
# audio/clips/ stays hot. The day counts are placeholders until the retention
# question in DEPLOYMENT.md is answered, and there is deliberately no delete
# action -- nothing here throws audio away on its own.
resource "azurerm_storage_management_policy" "this" {
  storage_account_id = azurerm_storage_account.this.id

  rule {
    name    = "audio-originals-tiering"
    enabled = true

    filters {
      prefix_match = ["audio/uploads/"]
      blob_types   = ["blockBlob"]
    }

    actions {
      base_blob {
        tier_to_cool_after_days_since_modification_greater_than    = 30
        tier_to_archive_after_days_since_modification_greater_than = 180
      }
    }
  }
}
