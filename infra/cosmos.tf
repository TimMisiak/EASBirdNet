# Documents: the roster, recorders, cards, their files, and detections.
# SCHEMA.md describes what is in them.

resource "azurerm_cosmosdb_account" "this" {
  name                = "cosmos-${local.base}${var.name_suffix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  kind                = "GlobalDocumentDB" # the NoSQL API
  offer_type          = "Standard"         # required by the provider; serverless is the capability below
  tags                = local.tags

  # Pay per request: the load is a few cards a week, not a steady rate.
  capabilities {
    name = "EnableServerless"
  }

  # The default. A writer reads its own writes, which is what the mutate-func
  # updates in internal/db assume.
  consistency_policy {
    consistency_level = "Session"
  }

  # Serverless is single-region.
  geo_location {
    location          = azurerm_resource_group.this.location
    failover_priority = 0
    zone_redundant    = false
  }

  # Entra ID only. No account keys to leak. BIRDSENSE_COSMOS_KEY exists in the
  # app for the emulator and is never set in Azure.
  local_authentication_enabled = false

  # The consumption Container Apps environment has no VNet, so the app reaches
  # Cosmos over the public endpoint. Revisit with private endpoints if that
  # changes.
  public_network_access_enabled = true
  minimal_tls_version           = "Tls12"

  # Point-in-time restore of the last 7 days, at no extra cost.
  backup {
    type = "Continuous"
    tier = "Continuous7Days"
  }

  free_tier_enabled = false # not compatible with serverless
}

resource "azurerm_cosmosdb_sql_database" "this" {
  name                = "birdsense"
  resource_group_name = azurerm_resource_group.this.name
  account_name        = azurerm_cosmosdb_account.this.name
  # throughput unset: serverless
}

# Names and partition keys cannot be changed after creation without copying the
# data to a new container, and the names are case-sensitive. They must match
# backend/internal/db/cosmos.go. The app never creates these: it checks at
# startup that all five exist and exits if one is missing.
locals {
  cosmos_containers = {
    users      = "/id"
    recorders  = "/id"
    uploads    = "/id"
    audioFiles = "/uploadId"
    detections = "/uploadId"
  }
}

resource "azurerm_cosmosdb_sql_container" "this" {
  for_each = local.cosmos_containers

  name                  = each.key
  resource_group_name   = azurerm_resource_group.this.name
  account_name          = azurerm_cosmosdb_account.this.name
  database_name         = azurerm_cosmosdb_sql_database.this.name
  partition_key_paths   = [each.value]
  partition_key_version = 2
  # Default indexing policy, no unique keys, no TTL, no throughput (serverless).
}

# A Cosmos *SQL role assignment*, not an ordinary Azure RBAC one: the portal
# roles named after Cosmos ("Cosmos DB Account Reader", "DocumentDB Account
# Contributor") are control-plane roles and do not grant reading or writing
# documents. The app needs no control-plane role at all.
resource "azurerm_cosmosdb_sql_role_assignment" "app" {
  resource_group_name = azurerm_resource_group.this.name
  account_name        = azurerm_cosmosdb_account.this.name

  # Built-in Data Contributor.
  role_definition_id = "${azurerm_cosmosdb_account.this.id}/sqlRoleDefinitions/00000000-0000-0000-0000-000000000002"
  principal_id       = azurerm_user_assigned_identity.this.principal_id
  scope              = "${azurerm_cosmosdb_account.this.id}/dbs/${azurerm_cosmosdb_sql_database.this.name}"
}
