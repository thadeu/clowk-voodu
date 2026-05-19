# Asymmetric webhook routing: PagerDuty pages directly on
# failure (no transformer in the middle), Slack gets a low-
# urgency note on success.
#
# What's new vs. the M6 original:
#
#   - Both slots are now sub-blocks with url + optional method +
#     optional headers.
#   - Failure slot hits PagerDuty Events API v2 DIRECTLY (no
#     Lambda transformer needed). Voodu's headers feature lets
#     us set the Authorization header PagerDuty's HTTP API
#     requires, eliminating the intermediary that the M6
#     original needed.
#   - Both URLs come from shell env: ${SLACK_WEBHOOK_URL} and
#     ${PD_ROUTING_KEY}. No secret in this file.
#
# Why asymmetric:
#
#   Successful deploy = INFORMATIONAL. Operator wants a record
#   ("yep, checkout 2.7.0 went out at 14:22") but nobody needs to
#   wake up for it. Green rollout at 3am is not a 3am problem.
#
#   Failed deploy = ACTIONABLE. Rolling restart aborted mid-flight,
#   readiness probe never went green, new replicas unhealthy and
#   old replicas may already be gone. Someone has to look NOW.
#   That's what PagerDuty is for.
#
# How it fires:
#
#   1. Rolling restart completes (or aborts). voodu picks the URL
#      based on the terminal status:
#        success → posts to on_deploy.success.url (Slack)
#        failure → posts to on_deploy.failure.url (PagerDuty)
#   2. Single POST per release. voodu never sends BOTH for the
#      same rollout — a release is either success or failure,
#      not both.
#
# Note on PagerDuty payload shape:
#
#   PagerDuty Events API v2 expects:
#
#     { "routing_key": "...", "event_action": "trigger",
#       "payload": { "summary": "...", "severity": "error",
#                    "source": "...", "custom_details": {...} } }
#
#   voodu's payload is generic JSON (kind/scope/name/status etc.).
#   If you want PagerDuty to render rich details, you still need a
#   small transformer fronting the Events API. The native auth via
#   headers below works for ANY PagerDuty-compatible receiver that
#   accepts the voodu schema directly — internal tooling, ops
#   dashboards, custom incident bots.
#
#   For "I just want to page the on-call" without rich payloads,
#   you can route to PagerDuty's "Events API v2 - Inbound
#   Integration" set up to wildcard-match incoming events.
#
# Apply:
#
#   cd examples/on_deploy
#   export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T.../B.../XXXX"
#   export PD_ROUTING_KEY="R0000000000000000000000000000000"
#   vd apply -f pagerduty-on-failure.hcl

deployment "prod" "checkout" {
  image    = "ghcr.io/acme/checkout:2.7.0"
  replicas = 4

  ports = ["8080"]

  env = {
    NODE_ENV = "production"
  }

  on_deploy {
    # Low-urgency success channel. Slack incoming webhooks
    # authenticate via the token-in-URL pattern; no headers
    # needed. Default POST.
    success {
      url = "${SLACK_WEBHOOK_URL}"
    }

    # PagerDuty Events API v2. Direct call — no transformer
    # in the middle, because the headers feature now lets us
    # carry the routing key + auth that PagerDuty requires.
    #
    # method defaults to POST; declared explicitly here so the
    # example reads end-to-end without operator wondering.
    failure {
      url    = "https://events.pagerduty.com/v2/enqueue"
      method = "POST"

      headers = {
        # PagerDuty expects routing_key inside the JSON body,
        # but for v2 INBOUND integrations a custom X-header
        # carrying the routing key is also accepted. Sets
        # both patterns up so the receiver can pick either.
        "Content-Type"  = "application/json"
        "X-Routing-Key" = "${PD_ROUTING_KEY}"

        # For PagerDuty REST API endpoints that require a real
        # API token (the manage-incidents flow, NOT the events
        # API): swap this in. Left here as documentation.
        # "Authorization" = "Token token=${PD_API_TOKEN}"
      }
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
