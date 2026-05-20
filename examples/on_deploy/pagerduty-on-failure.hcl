# Asymmetric webhook routing: PagerDuty pages directly on
# failure with the correct Events API v2 body shape (no
# transformer Lambda required), low-urgency Slack on success.
#
# This file demonstrates the asset-backed body pattern. The
# JSON template lives at ./webhooks/pagerduty-event.json — a
# real file, editable in any tool that understands JSON. The
# HCL just references it via the asset block.
#
# What's actually shipped to PagerDuty:
#
#   1. voodu reads ./webhooks/pagerduty-event.json
#   2. ${PD_ROUTING_KEY} substituted client-side at apply
#      (operator's shell env)
#   3. {{name}}, {{scope}}, {{error}}, etc. substituted at
#      fire time on the controller with the live release data
#   4. The result is POSTed to PagerDuty Events API v2
#
# Try opening ./webhooks/pagerduty-event.json — it's the
# Events v2 schema verbatim, just with template tokens where
# the dynamic values go. PagerDuty receives an event in the
# exact shape it expects, complete with severity, source,
# component, group, and custom_details.
#
# Why asymmetric:
#
#   Successful deploy = INFORMATIONAL. Operator wants a record
#   ("yep, checkout 2.7.0 went out at 14:22") but nobody needs
#   to wake up for it. Green rollout at 3am is not a 3am
#   problem.
#
#   Failed deploy = ACTIONABLE. Rolling restart aborted mid-
#   flight, readiness probe never went green, new replicas
#   unhealthy and old replicas may already be gone. Someone
#   has to look NOW. That's what PagerDuty is for.
#
# Apply:
#
#   cd examples/on_deploy
#   export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T.../B.../XXXX"
#   export PD_ROUTING_KEY="R0000000000000000000000000000000"
#   vd apply -f pagerduty-on-failure.hcl

asset "prod" "webhooks" {
  # PagerDuty Events API v2 body template. Contains the
  # routing_key + event_action + payload structure PagerDuty
  # expects. Template tokens (${PD_ROUTING_KEY} for the
  # secret, {{name}} etc. for runtime values) get substituted
  # at apply + fire time respectively.
  pagerduty_event = file("./webhooks/pagerduty-event.json")
}

deployment "prod" "checkout" {
  image    = "ghcr.io/acme/checkout:2.7.0"
  replicas = 4

  ports = ["8080"]

  env = {
    NODE_ENV = "production"
  }

  on_deploy {
    # Low-urgency success channel. Slack incoming webhook —
    # no headers needed, secret is in the URL path.
    success {
      url = "${SLACK_WEBHOOK_URL}"
    }

    # PagerDuty Events API v2 — direct call. The asset-backed
    # template carries the receiver-specific schema, so
    # PagerDuty gets exactly what it expects.
    failure {
      url  = "https://events.pagerduty.com/v2/enqueue"
      file = "${asset.prod.webhooks.pagerduty_event}"
    }
  }
}

ingress "prod" "checkout" {
  service = "checkout"
  host    = "checkout.example.com"

  tls {
    email = "ops@example.com"
  }
}
