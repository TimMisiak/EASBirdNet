output "app_url" {
  description = "Where the app is."
  value       = "https://${azurerm_container_app.this.ingress[0].fqdn}"
}

output "app_fqdn" {
  description = "The container app's own hostname. This is what the custom domain's CNAME points at."
  value       = azurerm_container_app.this.ingress[0].fqdn
}

# Read with `terraform output -raw custom_domain_verification_id`. The value is
# an attribute of the app as it already runs, so the TXT record can be made
# before any of domain.tf is configured -- which is the order it has to be done
# in (DEPLOYMENT.md, Custom domain). An output only reaches `terraform output`
# once an apply has written it into the state, so the first read after adding
# this one needs an apply -- which changes no infrastructure, only outputs.
# DEPLOYMENT.md gives the two ways to read it without that.
#
# Sensitive only because the provider marks the attribute that way; the value
# is about to be published in DNS.
output "custom_domain_verification_id" {
  description = "Value for the custom domain's `asuid.<name>` TXT record, which proves to Azure that we own the name."
  value       = azurerm_container_app.this.custom_domain_verification_id
  sensitive   = true
}

output "acr_name" {
  description = "Registry name, as `az acr build --registry` wants it. scripts/deploy.ps1 reads this."
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
