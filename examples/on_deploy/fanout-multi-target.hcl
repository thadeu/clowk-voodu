# Multi-target fan-out: each slot accepts multiple webhooks,
# fired in parallel goroutines with independent retry budgets.
#
# Use case: your team uses Slack for human notifications, Datadog
# for the metrics dashboard, and an internal status-page bot for
# customer-facing announcements. On failure you want PagerDuty
# AND OpsGenie (different on-call rotations). One on_deploy block,
# multiple `success` / `failure` blocks — voodu fans them all out.
#
# Each declared block becomes ONE goroutine. Each goroutine gets
# its own 3-attempt retry budget (1s, 5s, 30s backoff). A slow
# PagerDuty does NOT delay Slack — they're racing, not queued.
#
# Failure semantics per target: dropped on the floor after retries
# exhausted, logged with target=<i>/<total> identity. The deploy
# is NEVER rolled back because a webhook target was down.

deployment "prod" "api" {
  image = "ghcr.io/myorg/api:1.7"

  env_from = ["prod/shared"]   # SLACK_WEBHOOK_URL, DD_API_KEY, PD_ROUTING_KEY, OPSGENIE_KEY

  on_deploy {
    # Success → 3 informational destinations
    success {
      url = "${SLACK_WEBHOOK_URL}"
    }

    success {
      url    = "https://api.datadoghq.com/api/v1/events"
      headers = {
        "DD-API-KEY" = "${DD_API_KEY}"
      }

      body = {
        title = "voodu deploy: {{name}}"
        text  = "Released {{release_id}} ({{image}})"
        tags  = ["env:{{scope}}", "service:{{name}}", "deploy_outcome:success"]
      }
    }

    success {
      url    = "https://status.example.com/internal/api/deploys"
      method = "POST"
      body = {
        service = "{{name}}"
        version = "{{image}}"
        when    = "{{completed_at}}"
      }
    }

    # Failure → 2 paging destinations (PagerDuty primary, OpsGenie backup)
    failure {
      url     = "https://events.pagerduty.com/v2/enqueue"
      headers = {
        "X-Routing-Key" = "${PD_ROUTING_KEY}"
      }
      file = "${asset.prod.webhooks.pagerduty_event}"
    }

    failure {
      url     = "https://api.opsgenie.com/v2/alerts"
      headers = {
        "Authorization" = "GenieKey ${OPSGENIE_KEY}"
      }
      body = {
        message     = "voodu rollout failed: {{scope}}/{{name}}"
        description = "{{error}}"
        priority    = "P2"
        tags        = ["voodu", "deploy-failure", "{{scope}}"]
      }
    }
  }
}

asset "prod" "webhooks" {
  pagerduty_event = file("./webhooks/pagerduty-event.json")
}
