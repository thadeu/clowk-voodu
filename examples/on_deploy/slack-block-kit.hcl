# Rich Slack messages via Block Kit, asset-backed.
#
# The default voodu webhook payload (kind, scope, name,
# release_id, status, etc.) is fine for generic JSON consumers.
# Slack incoming webhooks accept "any JSON" so it works — but
# the message renders as raw key-value pairs in the channel,
# which is ugly.
#
# Slack Block Kit lets the message use headers, multi-column
# fields, code blocks, @channel mentions, emojis. The body
# templates live in ./webhooks/slack-block-kit-*.json — open
# them in any editor for syntax highlighting; iterate without
# touching HCL.
#
# Different templates for success vs. failure on purpose:
#
#   success → green check, no @mention, compact field grid
#   failure → red emoji, @channel mention, error in a code
#             block, full timestamps for incident timeline
#
# The Slack workflow on the receiving end doesn't need to
# branch on `status` — each outcome already has its own
# rendered template. Less logic on the Slack side, more
# control on the operator side.
#
# Apply:
#
#   cd examples/on_deploy
#   export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T.../B.../XXXX"
#   vd apply -f slack-block-kit.hcl

asset "prod" "slack" {
  success_blocks = file("./webhooks/slack-block-kit-success.json")
  failure_blocks = file("./webhooks/slack-block-kit-failure.json")
}

deployment "prod" "api" {
  image    = "ghcr.io/acme/api:1.4.2"
  replicas = 3

  ports = ["3000"]

  env = {
    RAILS_ENV = "production"
  }

  on_deploy {
    success {
      url  = "${SLACK_WEBHOOK_URL}"
      file = "${asset.prod.slack.success_blocks}"
    }

    failure {
      url  = "${SLACK_WEBHOOK_URL}"
      file = "${asset.prod.slack.failure_blocks}"
    }
  }
}
