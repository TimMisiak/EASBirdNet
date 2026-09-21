# What watches the stack. Log Analytics (app.tf) has always collected the
# logs; nothing read them, so none of these was visible to anyone: a replica
# restarting, the site returning 500s, BirdNET failing to import so that cards
# sit in processing forever, an Azure incident in the region, or the bill
# growing on its own because clips never expire.
#
# Everything here mails var.alert_emails through one action group. There is no
# paging, no on-call rotation and no ticketing: this is a volunteer-run
# program, and the useful question is "does anybody find out", not "how fast".
#
# Not covered, deliberately or otherwise:
#   - Whether the site is reachable *at all*. With one replica and little
#     traffic, a dead app produces no requests, so the 5xx rule below stays
#     quiet; only the resource-health rule would notice, and only if the
#     platform notices. An external ping test (Application Insights standard
#     web test against /api/v1/ready) is what actually covers this, and it
#     costs a resource this stack otherwise doesn't need.
#   - The Entra ID client secret expiring, which stops every sign-in. Azure
#     Monitor has nothing to alert on: an app registration's credential isn't
#     a resource and emits no metrics. See TODO.md 5.12.
#   - Cosmos 429s. The SDK retries them and serverless throttling during a
#     bulk upsert is expected rather than broken, so a rule here would be
#     tuned against noise before anyone knows what normal looks like.

resource "azurerm_monitor_action_group" "ops" {
  name                = "ag-${local.base}"
  resource_group_name = azurerm_resource_group.this.name
  short_name          = "birdsense" # at most 12 characters; it prefixes the mail subject
  tags                = local.tags

  dynamic "email_receiver" {
    for_each = var.alert_emails
    content {
      name          = "email-${email_receiver.key}"
      email_address = email_receiver.value
      # The common schema, so every rule below mails the same shape.
      use_common_alert_schema = true
    }
  }
}

# --- Metric alerts. Platform metrics need no diagnostic setting and no
# ingestion: Container Apps emits these whether or not anything reads them. ---

# One replica is the whole deployment, so a restart is a BirdNET run killed
# mid-file and every tus upload in flight dropped back to its last chunk.
# Neither loses data (the queue resumes from the documents, tus resumes from
# the blob), but both are invisible from outside.
#
# Gotcha: this is a replica's own restart count, reported for as long as that
# replica lives, so the alert stays fired rather than clearing itself once the
# window is quiet again. With exactly one replica that is the honest reading --
# the replica that restarted is still the one serving -- but it means
# acknowledging it rather than waiting for it to go away. A deploy replaces the
# replica and clears it.
resource "azurerm_monitor_metric_alert" "replica_restarts" {
  name                = "alert-${local.base}-replica-restarts"
  resource_group_name = azurerm_resource_group.this.name
  scopes              = [azurerm_container_app.this.id]
  description         = "A Birdsense replica restarted. It is the only one, so a BirdNET run and any upload in flight were cut. Check the console logs for why the process died."
  severity            = 2
  frequency           = "PT5M"
  window_size         = "PT5M"
  tags                = local.tags

  criteria {
    metric_namespace = "Microsoft.App/containerApps"
    metric_name      = "RestartCount"
    aggregation      = "Maximum"
    operator         = "GreaterThan"
    threshold        = 0
  }

  action {
    action_group_id = azurerm_monitor_action_group.ops.id
  }
}

# 5xx from ingress. The threshold is not zero: a single 500 is worth a log
# line, not a mail, and the app returns one for an ordinary store failure that
# the next attempt gets through. Several in a quarter of an hour is a pattern.
resource "azurerm_monitor_metric_alert" "server_errors" {
  name                = "alert-${local.base}-5xx"
  resource_group_name = azurerm_resource_group.this.name
  scopes              = [azurerm_container_app.this.id]
  description         = "Birdsense is returning server errors. Card uploads and review both go through this app; check the console logs and /api/v1/ready."
  severity            = 1
  frequency           = "PT5M"
  window_size         = "PT15M"
  tags                = local.tags

  criteria {
    metric_namespace = "Microsoft.App/containerApps"
    metric_name      = "Requests"
    aggregation      = "Total"
    operator         = "GreaterThan"
    threshold        = 5

    dimension {
      name     = "statusCodeCategory"
      operator = "Include"
      values   = ["5xx"]
    }
  }

  action {
    action_group_id = azurerm_monitor_action_group.ops.id
  }
}

# --- Log alert. This is the one that reads what the app itself says. ---

# BirdNET not being importable is the failure with no outward symptom at all:
# uploads work, the site is up, and received cards pile up in `processing`
# while the queue retries on a growing delay (backend/internal/analysis). The
# coordinator's card screens say so, but only to a coordinator who happens to
# look.
#
# The queue re-logs this at most ten minutes apart, so a fifteen-minute window
# always catches a run of it and clears about fifteen minutes after BirdNET
# starts working -- including when the fix was an environment change the queue
# picked up on its own, with no restart.
#
# Log_s is the raw stdout line. cmd/server logs JSON so this can match a field
# instead of a substring; tusd's own lines are text, don't parse, and fall out
# of the query rather than having to be excluded.
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "birdnet_unavailable" {
  name                = "alert-${local.base}-birdnet-unavailable"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  scopes              = [azurerm_log_analytics_workspace.this.id]
  description         = "BirdNET can't be run, so received cards are waiting in `processing` and nothing is being analyzed. See DEPLOYMENT.md, When cards sit in processing."
  severity            = 2
  tags                = local.tags

  evaluation_frequency = "PT15M"
  window_duration      = "PT15M"

  criteria {
    query                   = <<-KQL
      ContainerAppConsoleLogs_CL
      | where ContainerAppName_s == "${local.app_name}"
      | extend line = parse_json(Log_s)
      | where tostring(line.msg) startswith "BirdNET isn't available"
    KQL
    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  auto_mitigation_enabled = true

  action {
    action_groups = [azurerm_monitor_action_group.ops.id]
  }
}

# --- Activity log alerts. Free, and neither one is about our code. ---

# An Azure incident or planned maintenance touching the region. Scoped to the
# subscription because that is the only scope service health is raised at, and
# left across all services rather than naming Container Apps, Cosmos DB and
# Storage: a service name that doesn't match Azure's exactly matches nothing,
# silently, which is the opposite of what this is for.
resource "azurerm_monitor_activity_log_alert" "service_health" {
  name                = "alert-${local.base}-service-health"
  resource_group_name = azurerm_resource_group.this.name
  location            = "global"
  scopes              = ["/subscriptions/${data.azurerm_client_config.current.subscription_id}"]
  description         = "Azure is reporting an incident or planned maintenance affecting ${var.location}. Not a Birdsense fault; this is the mail that says so."
  tags                = local.tags

  criteria {
    category = "ServiceHealth"

    service_health {
      events    = ["Incident", "Maintenance"]
      locations = [var.location, "Global"]
    }
  }

  action {
    action_group_id = azurerm_monitor_action_group.ops.id
  }
}

# The platform's own view of a resource going unhealthy, scoped to the whole
# resource group so it covers Cosmos and the storage account as well as the
# app. Nothing to configure and nothing to keep in step with the code; if a
# resource type here doesn't report health, this rule simply never fires for
# it.
resource "azurerm_monitor_activity_log_alert" "resource_health" {
  name                = "alert-${local.base}-resource-health"
  resource_group_name = azurerm_resource_group.this.name
  location            = "global"
  scopes              = [azurerm_resource_group.this.id]
  description         = "Azure reports a Birdsense resource as degraded or unavailable."
  tags                = local.tags

  criteria {
    category = "ResourceHealth"

    resource_health {
      current  = ["Degraded", "Unavailable"]
      previous = ["Available"]
      reason   = ["PlatformInitiated", "UserInitiated", "Unknown"]
    }
  }

  action {
    action_group_id = azurerm_monitor_action_group.ops.id
  }
}

# --- Cost. ---

# The bill is the one thing here that changes without anybody touching
# anything: clips are kept for good and add roughly a terabyte a year at five
# recorders (DEPLOYMENT.md, Cost). This is a notification, not a cap -- Azure
# budgets stop nothing -- and it is what gives the chapter a year's warning
# rather than a surprise.
resource "azurerm_consumption_budget_resource_group" "this" {
  name              = "budget-${local.base}"
  resource_group_id = azurerm_resource_group.this.id

  amount     = var.budget_monthly_usd
  time_grain = "Monthly"

  # A monthly budget has to start on the first of a month. Deriving it from
  # the current one means a first apply needs no date in a tfvars file;
  # ignore_changes below is what stops that derivation re-planning every month
  # afterwards.
  time_period {
    start_date = formatdate("YYYY-MM-01'T'00:00:00Z", timestamp())
  }

  # 80% of what has actually been spent: something has changed, and there is
  # most of a month left to look at it.
  notification {
    enabled        = true
    threshold      = 80
    threshold_type = "Actual"
    operator       = "GreaterThan"
    contact_groups = [azurerm_monitor_action_group.ops.id]
  }

  # And a forecast of going over, which is the one that arrives early enough
  # to be useful.
  notification {
    enabled        = true
    threshold      = 100
    threshold_type = "Forecasted"
    operator       = "GreaterThan"
    contact_groups = [azurerm_monitor_action_group.ops.id]
  }

  lifecycle {
    ignore_changes = [time_period]
  }
}
