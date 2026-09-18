output "app_url" {
  description = "Where the app is."
  value       = "https://${azurerm_container_app.this.ingress[0].fqdn}"
}

output "app_fqdn" {
  description = "The container app's hostname, for a CNAME if a custom domain is added."
  value       = azurerm_container_app.this.ingress[0].fqdn
}

output "acr_name" {
  description = "Registry name, as `az acr build --registry` wants it. scripts/deploy.sh reads this."
  value       = azurerm_container_registry.this.name
}

output "acr_login_server" {
  description = "Registry hostname, for image references."
  value       = azurerm_container_registry.this.login_server
}

output "cosmos_endpoint" {
  description = "For running a laptop against real Cosmos (DEPLOYMENT.md)."
  value       = azurerm_cosmosdb_account.this.endpoint
}

output "blob_endpoint" {
  description = "For running a laptop against real Blob Storage (DEPLOYMENT.md)."
  value       = azurerm_storage_account.this.primary_blob_endpoint
}

output "identity_client_id" {
  description = "The app's managed identity, as AZURE_CLIENT_ID."
  value       = azurerm_user_assigned_identity.this.client_id
}

output "resource_group" {
  description = "Everything above lives here."
  value       = azurerm_resource_group.this.name
}

output "image_tag" {
  description = "The tag Terraform currently has deployed. Handy for applying a settings change without a rebuild: pass it back as -var image_tag=..."
  value       = var.image_tag
}
