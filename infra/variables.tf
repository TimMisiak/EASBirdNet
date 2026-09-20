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

variable "public_url" {
  description = "Where browsers reach Birdsense, scheme and host, e.g. https://owls.eastsideaudubon.org. The identity provider's redirect URI is built from it, so it must be registered with the provider exactly. Empty uses the container app's own hostname, which is what to use before a custom domain exists."
  type        = string
  default     = ""

  validation {
    condition     = var.public_url == "" || can(regex("^https://[^/]+$", var.public_url))
    error_message = "public_url must be https:// and a host with no path or trailing slash."
  }
}

variable "oidc_microsoft_client_id" {
  description = "Application (client) ID of the Entra ID app registration volunteers sign in through. Required: outside dev mode OpenID Connect is the only way in."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-fA-F-]{36}$", var.oidc_microsoft_client_id))
    error_message = "oidc_microsoft_client_id must be the app registration's GUID."
  }
}

variable "oidc_microsoft_client_secret" {
  description = "Client secret of that app registration. Kept as a Container Apps secret, not in the revision's environment."
  type        = string
  sensitive   = true
}

variable "oidc_microsoft_tenant" {
  description = "Directory to sign in against. `common` accepts any organization and any personal Microsoft account, which is what a roster of volunteers with their own addresses needs; a tenant GUID restricts sign-in to that one directory."
  type        = string
  default     = "common"
}

variable "session_key" {
  description = "Signs the session cookie. Any long random string (openssl rand -base64 32). Changing it signs everyone out, which is how to end every session at once."
  type        = string
  sensitive   = true

  validation {
    condition     = length(var.session_key) >= 32
    error_message = "session_key must be at least 32 characters; generate one with `openssl rand -base64 32`."
  }
}
