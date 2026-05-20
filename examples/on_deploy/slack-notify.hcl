# Slack notification for every rollout — bare default payload
# (kind/scope/name/status/...) goes through, receiving workflow
# branches on the `status` field for tone.
#
# Want richer Slack messages (headers, code blocks, @channel
# mentions, multi-column fields)? See slack-block-kit.hcl in
# this directory — same delivery contract, asset-backed Block
# Kit JSON templates for full visual control.
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
# Apply — two ways to feed ${SLACK_WEBHOOK_URL}:
#
#   ### Option A: store in scope bucket (recommended for teams)
#
#   Set once on the controller, every dev's vd apply reads it:
#
#     vd config set -s prod -n shared SLACK_WEBHOOK_URL="https://hooks.slack.com/..."
#     cd examples/on_deploy
#     vd apply -f slack-notify.hcl
#
#   Rotation via vd config set propagates to every dev's next apply.
#   The env_from line below declares the bucket; the CLI fetches it
#   at parse time and ${SLACK_WEBHOOK_URL} resolves from there.
#
#   ### Option B: local shell only (single-dev / iteration)
#
#     export SLACK_WEBHOOK_URL="https://hooks.slack.com/..."
#     vd apply -f slack-notify.hcl
#
#   Shell always wins over bucket on collision — useful for testing
#   a different URL temporarily without touching the bucket.

deployment "prod" "api" {
  image    = "ghcr.io/acme/api:1.4.2"

  # When this deployment declares env_from, the CLI fetches the
  # bucket BEFORE parsing — making bucket vars available for
  # ${VAR} substitution in this manifest. The bucket also gets
  # mounted as runtime env file for the container; same source,
  # both stages.
  env_from = ["prod/shared"]
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
