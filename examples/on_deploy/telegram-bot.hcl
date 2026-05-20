# Telegram bot API for deploy notifications.
#
# Different shape from Slack: Telegram expects
# `{chat_id, text, parse_mode}` and the URL itself carries
# the bot token (`/botSECRET/sendMessage`). Voodu's flexible
# body lets us hit it directly.
#
# What this shows:
#
#   1. Bot token comes from shell env (${TELEGRAM_BOT_TOKEN})
#      and lands in the URL — never in git.
#   2. chat_id comes from shell env too, inside the template
#      file (${TELEGRAM_CHAT_ID}). Both interpolations are
#      client-side at parse time.
#   3. Inline body (no asset, no file) — the Telegram payload
#      is small enough that the asset overhead would be more
#      noise than benefit. Demonstrates the OTHER body mode
#      (compare to pagerduty-on-failure.hcl / slack-block-kit.hcl
#      which use asset-backed file).
#
# Why inline here, asset elsewhere?
#
#   Rule of thumb: ≥ 5 lines of nested JSON → asset.
#                  ≤ a few flat fields → inline.
#
#   Telegram's payload is 3 flat fields. Inline keeps the
#   intent visible on the same screen as the URL it ships
#   to. Slack Block Kit's payload is 30+ lines of nested
#   structure — inline would dwarf the deployment block.
#
# Apply:
#
#   cd examples/on_deploy
#   export TELEGRAM_BOT_TOKEN="123456:abcdefXXX"
#   export TELEGRAM_CHAT_ID="-1009999999999"
#   vd apply -f telegram-bot.hcl

deployment "prod" "api" {
  image    = "ghcr.io/acme/api:1.4.2"
  replicas = 2

  ports = ["3000"]

  env = {
    RAILS_ENV = "production"
  }

  on_deploy {
    # Success notification — concise inline body. Telegram's
    # MarkdownV2 needs special characters escaped (`.`, `-`, `_`
    # etc.); we keep the template simple here, but a real
    # production setup would either avoid those characters or
    # use the `HTML` parse mode.
    success {
      url = "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage"

      body = {
        chat_id    = "${TELEGRAM_CHAT_ID}"
        parse_mode = "MarkdownV2"
        text       = "✅ deployed *{{scope}}/{{name}}*\n\nimage: `{{image}}`\nrelease: `{{release_id}}`"
      }
    }

    # Failure uses a separate inline body — same flat shape,
    # different content (loud emoji, error in code block,
    # disable_notification=false to override "silent hours"
    # bot settings since this is high-priority).
    failure {
      url = "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage"

      body = {
        chat_id              = "${TELEGRAM_CHAT_ID}"
        parse_mode           = "MarkdownV2"
        disable_notification = false
        text                 = "🚨 FAILED *{{scope}}/{{name}}*\n\nrelease: `{{release_id}}`\nerror: ```{{error}}```"
      }
    }
  }
}
