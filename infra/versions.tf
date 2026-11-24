terraform {
  required_version = ">= 1.9.0"

  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
      # storage_account_id on azurerm_storage_container and
      # local_authentication_enabled on Cosmos need a recent 4.x.
      version = ">= 4.81.0, < 5.0.0"
    }
  }

  # Partial configuration: the state account's details live in backend.hcl,
  # which is created once by hand (see README.md) and is not in git, so this
  # file says nothing about a particular subscription.
  backend "azurerm" {}
}

provider "azurerm" {
  features {}

  # Required by azurerm v4. Left null here so it can come from
  # ARM_SUBSCRIPTION_ID instead of a tfvars file; set one or the other.
  subscription_id = var.subscription_id

  # The storage account has shared keys disabled, so the provider has to
  # authenticate its data-plane calls (creating the `audio` container) as the
  # signed-in principal. See azurerm_role_assignment.operator_blob in storage.tf.
  storage_use_azuread = true

  # azurerm v4 registers only its "core" set of resource providers, and that
  # set leaves out Azure Communication Services (email.tf), so a subscription
  # that has never used it refuses the email service with a 409,
  # MissingSubscriptionRegistration. Registering a provider is per subscription,
  # costs nothing, and the provider skips it once it's done, so it's named here
  # whether or not email_domain is set.
  resource_providers_to_register = ["Microsoft.Communication"]
}

data "azurerm_client_config" "current" {}
