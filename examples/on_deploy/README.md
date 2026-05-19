# on_deploy/

Post-deploy notification webhooks for deployments. After a rolling
restart finishes — whether it succeeded or failed — voodu POSTs a
small JSON payload to the URL(s) the operator declared.

## What it IS

A fire-and-forget notification hook. Two independent sub-blocks
(`success`, `failure`); declare either or both.

```hcl
deployment "prod" "api" {
  on_deploy {
    success {
      url = "${SLACK_WEBHOOK_URL}"
    }

    failure {
      url    = "https://events.pagerduty.com/v2/enqueue"
      method = "POST"                     # default; explicit here
      headers = {
        "X-Routing-Key" = "${PD_KEY}"
      }
    }
  }
}
```

### Sub-block fields

| field | required | default | notes |
|---|---|---|---|
| `url` | yes | — | Absolute endpoint. Supports `${VAR}` shell-env interpolation (client-side at parse time) so secret-bearing URLs and headers stay out of git. |
| `method` | no | `POST` | Whitelist: `POST` / `PUT` / `PATCH` / `DELETE`. GET / HEAD / OPTIONS rejected at parse — webhook payload IS a body. |
| `headers` | no | empty | Map of extra request headers. Operator overrides `Content-Type` if declared. **`User-Agent` is force-set** to `voodu-deploy-webhook` (source-of-call debug signal). |

## What it ISN'T

- **Not a lifecycle hook.** voodu doesn't execute commands, doesn't
  run scripts, doesn't shell into anything. It POSTs a JSON body
  and walks away.
- **Not a gate.** The webhook fires AFTER the rolling restart
  completes. It cannot block, cancel, or roll back a rollout. The
  deploy outcome is already final by the time your endpoint hears
  about it.
- **Not synchronous with `vd apply`.** Delivery happens in a
  goroutine. A slow Slack server doesn't pin your CI.

## Delivery contract

Best-effort. **3 attempts** with exponential backoff (1s, 5s, 30s
between attempts; 10s per-attempt HTTP timeout). If all three
fail, voodu logs the drop and moves on. **The rollout outcome is
NEVER affected by webhook delivery** — by the time the hook fires,
the deploy already succeeded or failed on its own merits.

## Why NOT in spec hash

Editing `on_deploy.success` from one Slack URL to another is a
notification-routing change, not a runtime change. The container
behaves identically regardless of which webhook URL voodu POSTs
to afterward. Folding webhook URLs into the spec hash would churn
replicas on every Slack workflow rename — meaningful pain for zero
operational benefit. So `on_deploy` lives outside the hash.

You can rotate webhook URLs as freely as you'd rotate Slack channel
names.

## Payload shape

```json
{
  "kind": "deployment",
  "scope": "prod",
  "name": "api",
  "release_id": "a3f9c1b2e",
  "image": "ghcr.io/acme/api:1.4.2",
  "status": "success",
  "started_at": "2026-05-19T14:22:10Z",
  "completed_at": "2026-05-19T14:22:47Z",
  "error": ""
}
```

`status` is `"success"` or `"failure"`. On failure, `error` carries
the first failure message that aborted the rollout (same string
`vd release <ref>` shows). `release_id` is the 9-char release
record ID; empty for env-change-only restarts that don't mint a
release record. All timestamps are RFC3339 UTC.

`Content-Type: application/json`, `User-Agent: voodu-deploy-webhook`.

## One URL vs two

- **Same URL for both** — single channel, ops team sees every
  rollout regardless of outcome. The receiving workflow keys off
  the `status` field for tone (emoji, colour, mention rules). See
  [`slack-notify.hcl`](./slack-notify.hcl).
- **Different URLs per outcome** — successes are informational
  (low-urgency channel, no paging); failures are actionable
  (PagerDuty, on-call channel). Asymmetric urgency = asymmetric
  routing. See [`pagerduty-on-failure.hcl`](./pagerduty-on-failure.hcl).

## Examples

| file | what it shows |
|---|---|
| [`slack-notify.hcl`](./slack-notify.hcl) | Same Slack URL for both outcomes; receiving workflow branches on `status` |
| [`pagerduty-on-failure.hcl`](./pagerduty-on-failure.hcl) | PagerDuty page on failure, low-urgency Slack on success — asymmetric routing |
