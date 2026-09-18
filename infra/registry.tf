# The image is built by scripts/deploy.sh (az acr build) and run by the
# container app, which pulls it as the managed identity.

resource "azurerm_container_registry" "this" {
  name                = "cr${local.compact}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  sku                 = "Basic"
  admin_enabled       = false # the app pulls with AcrPull, not a password
  tags                = local.tags
}

resource "azurerm_role_assignment" "app_acr_pull" {
  scope                = azurerm_container_registry.this.id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_user_assigned_identity.this.principal_id
}
