# Slack notification for every rollout — same URL for success and
# failure, receiving workflow branches on the payload's `status`
# field for tone.
#
# Two new things vs. the M6-original shape:
#
#   1. success / failure are now sub-BLOCKS, not bare attributes.
#      Each takes `url` + optional `method` (default POST) +
#      optional `headers` map.
#
#   2. URL comes from ${SLACK_WEBHOOK_URL} — shell env interpolated
#      client-side at parse time. The secret-bearing token in the
#      Slack webhook path (B0XXXXXX/XXX...) never lands in this
#      file or in git.
#
# Why same URL for both:
#
#   A single #deploys channel sees every rollout outcome in one
#   stream. The ops team eyeballs the channel and immediately
#   knows "all three afternoon deploys went green" or "the 14:22
#   api rollout failed". No context switching between channels,
#   no missed failures hiding in a low-traffic "successes" feed.
#
#   The Slack-side workflow (Slack Workflow Builder, an incoming-
#   webhook formatter, or your own Lambda fronting the URL) keys
#   off the JSON `status` field to switch tone:
#
#     status == "success" → green check, no @mention,
#                           "api 1.4.2 rolled out cleanly"
#     status == "failure" → red X, @channel mention,
#                           "api 1.4.2 ABORTED: probe never went ready"
#
#   Because voodu sends the same payload shape to both URLs, the
#   transformer doesn't care which slot fired — it can render any
#   `status` value uniformly.
#
# Why declare both sub-blocks with the same URL rather than only
# success:
#
#   `on_deploy { success { url = "..." } }` alone would silently
#   DROP every failure notification. That's the worst possible
#   failure mode: the operator thinks the channel is wired up,
#   but the only events they actually care about (failures) never
#   arrive. Declaring both explicitly is the operator stating "I
#   want to hear about all outcomes via this channel."
#
# Remember:
#
#   - Delivery is best-effort (3 attempts, 1s/5s backoff, drop on
#     the floor). If Slack is down, the rollout still succeeded —
#     voodu logs the drop and moves on.
#   - The on_deploy block is NOT in the spec hash. Rotating the
#     webhook URL (incoming-webhook leaked, channel renamed,
#     workspace migrated) does NOT churn replicas. Just edit and
#     re-apply.
#
# Apply:
#
#   cd examples/on_deploy
#   export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T.../B.../XXXX"
#   vd apply -f slack-notify.hcl

deployment "prod" "api" {
  image    = "ghcr.io/acme/api:1.4.2"
  replicas = 3

  ports = ["3000"]

  env = {
    RAILS_ENV = "production"
  }

  on_deploy {
    # Same URL in both sub-blocks. Slack-side workflow renders
    # green/red based on the JSON `status` field in the payload
    # voodu POSTs. method defaults to POST, headers map omitted
    # because Slack incoming webhooks accept the bare JSON body
    # with no auth — the secret IS the URL path.
    success {
      url = "${SLACK_WEBHOOK_URL}"
    }

    failure {
      url = "${SLACK_WEBHOOK_URL}"
    }
  }
}
