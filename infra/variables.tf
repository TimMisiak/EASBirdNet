variable "subscription_id" {
  description = "Subscription to deploy into. azurerm v4 requires one; leave null to use ARM_SUBSCRIPTION_ID from the environment."
  type        = string
  default     = null
}

variable "env" {
  description = "Environment name. Part of every resource name; `prod` is the only one today, and a `staging` copy is a second tfvars file."
  type        = string
  default     = "prod"

  validation {
    condition     = can(regex("^[a-z0-9]{2,8}$", var.env))
    error_message = "env must be 2-8 lowercase alphanumerics: it goes into storage and registry names, which allow nothing else."
  }
}

variable "location" {
  description = "Azure region. The nearest to the Eastside with serverless Cosmos DB and Container Apps."
  type        = string
  default     = "westus2"
}

variable "name_suffix" {
  description = "Appended to the globally unique names (storage account, registry, Cosmos account) if the plain ones are taken. Lowercase alphanumerics; update DEPLOYMENT.md if you set it."
  type        = string
  default     = ""

  validation {
    condition     = can(regex("^[a-z0-9]{0,6}$", var.name_suffix))
    error_message = "name_suffix must be up to 6 lowercase alphanumerics."
  }
}

variable "image_tag" {
  description = "Tag of the birdsense image in the registry to run. Terraform owns the running image, so a deploy is a new value here (scripts/deploy.sh passes the git sha)."
  type        = string
}

variable "bootstrap_admin" {
  description = "The first admin, as `Name <email>` or an email. Required: a new database has an empty roster and only an admin can add people. Ignored once the roster is not empty. See DEPLOYMENT.md, First deploy."
  type        = string

  validation {
    condition     = can(regex("@", var.bootstrap_admin))
    error_message = "bootstrap_admin must contain an email address, as `Name <email>` or just the email."
  }
}

variable "cpu" {
  description = "vCPU for the container. The analysis queue runs BirdNET in this container (CLAUDE.md), so this is sized for analysis, not for serving pages."
  type        = number
  default     = 1.0
}

variable "memory" {
  description = "Memory for the container. Container Apps only allows particular cpu/memory pairs; 1.0 goes with 2Gi."
  type        = string
  default     = "2Gi"
}

variable "log_retention_days" {
  description = "Log Analytics retention."
  type        = number
  default     = 30
}

variable "grant_operator_blob_access" {
  description = "Give whoever runs Terraform Storage Blob Data Contributor on the storage account. Needed because shared keys are off and the provider creates the `audio` container over the data plane; set false if that role is granted some other way."
  type        = bool
  default     = true
}
