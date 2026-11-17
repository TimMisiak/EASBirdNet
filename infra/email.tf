# Mail, for the links that set passwords: Azure Communication Services Email,
# from a domain of ours (Azure's own managed domain is for testing, and is
# limited to 10 emails an hour). Optional: with `email_domain` empty nothing here
# exists and the app offers no password sign-in. DEPLOYMENT.md, *Email*, is the
# prose version, including what to ask whoever runs our DNS.
#
# ACS Email retires on 2028-09-30. Move to another sender before then: only
# backend/internal/email and this file know which one it is.
#
# Takes two applies, because a domain can only be linked once it is verified,
# and our DNS is hosted outside Azure, so the records are made by hand between
# them:
#
#   1. `email_domain` creates the services and the domain. `terraform output
#      email_dns_records` is what to put in DNS, and the domain is then verified
#      from the portal or `az communication email domain initiate-verification`.
#   2. `email_enabled` links the domain and gives the app the settings, which is
#      what turns password sign-in on.

resource "azurerm_email_communication_service" "this" {
  count               = var.email_domain != "" ? 1 : 0
  name                = "email-${local.base}${var.name_suffix}"
  resource_group_name = azurerm_resource_group.this.name
  data_location       = var.email_data_location
  tags                = local.tags
}

resource "azurerm_email_communication_service_domain" "this" {
  count             = var.email_domain != "" ? 1 : 0
  name              = var.email_domain
  email_service_id  = azurerm_email_communication_service.this[0].id
  domain_management = "CustomerManaged"
  # Off, so a sign-in link reaches the person as it was written. The server
  # asks for the same on each message.
  user_engagement_tracking_enabled = false
  tags                             = local.tags
}

resource "azurerm_communication_service" "this" {
  count               = var.email_domain != "" ? 1 : 0
  name                = "acs-${local.base}${var.name_suffix}"
  resource_group_name = azurerm_resource_group.this.name
  data_location       = var.email_data_location
  tags                = local.tags
}

resource "azurerm_communication_service_email_domain_association" "this" {
  count                    = var.email_enabled ? 1 : 0
  communication_service_id = azurerm_communication_service.this[0].id
  email_service_domain_id  = azurerm_email_communication_service_domain.this[0].id
}
