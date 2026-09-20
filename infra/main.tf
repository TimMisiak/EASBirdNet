# Birdsense in Azure: one container app serving the API and the frontend, with
# documents in Cosmos DB and card audio in Blob Storage. DEPLOYMENT.md is the
# prose version of this directory, resource by resource, and explains why each
# non-default setting is what it is.
#
# Every data-plane call authenticates as the app's managed identity: there are
# no keys or connection strings anywhere in here, and none in the state file.

locals {
  # `<type>-birdsense-<env>`, following Azure's abbreviation list.
  base = "birdsense-${var.env}"

  # Storage account and registry names allow only lowercase alphanumerics.
  compact = "birdsense${var.env}${var.name_suffix}"

  # The container app's name, as a local because its own default public URL is
  # built from it: referring to the resource from inside itself is a cycle.
  app_name = "ca-${local.base}"

  tags = {
    app   = "birdsense"
    env   = var.env
    owner = "eastside-audubon"
  }
}

resource "azurerm_resource_group" "this" {
  name     = "rg-${local.base}"
  location = var.location
  tags     = local.tags
}

# User-assigned, not system-assigned, so its role assignments can exist before
# the container app does. A system-assigned identity is created with the app's
# first revision, which would then have no AcrPull and fail to pull its image.
resource "azurerm_user_assigned_identity" "this" {
  name                = "id-${local.base}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tags                = local.tags
}
