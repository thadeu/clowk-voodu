# on_deploy/

Post-deploy notification webhooks for deployments. After a rolling
restart finishes — whether it succeeded or failed — voodu POSTs a
small JSON payload to the URL(s) the operator declared.

## What it IS

A fire-and-forget notification hook. Two independent slots
(`success`, `failure`); each slot accepts **zero, one, or many**
target blocks. Multiple targets per slot fire in parallel
goroutines with independent retry budgets — a slow PagerDuty
doesn't delay Slack.

Single target per slot — the common case:

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

Multiple targets per slot — fan-out:

```hcl
deployment "prod" "api" {
  on_deploy {
    success { url = "${SLACK_WEBHOOK_URL}" }
    success {
      url     = "https://api.datadoghq.com/api/v1/events"
      headers = { "DD-API-KEY" = "${DD_API_KEY}" }
    }

    failure {
      url     = "https://events.pagerduty.com/v2/enqueue"
      headers = { "X-Routing-Key" = "${PD_KEY}" }
    }
    failure {
      url     = "https://api.opsgenie.com/v2/alerts"
      headers = { "Authorization" = "GenieKey ${OPSGENIE_KEY}" }
    }
  }
}
```

Each declared block = one goroutine, one URL, one retry loop.
See [`fanout-multi-target.hcl`](./fanout-multi-target.hcl) for a
full example.

### Target fields (per block)

| field | required | default | notes |
|---|---|---|---|
| `url` | yes | — | Absolute endpoint. Supports `${VAR}` shell-env interpolation (client-side at parse time) so secret-bearing URLs and headers stay out of git. |
| `method` | no | `POST` | Whitelist: `POST` / `PUT` / `PATCH` / `DELETE`. GET / HEAD / OPTIONS rejected at parse — webhook payload IS a body. |
| `headers` | no | empty | Map of extra request headers. Operator overrides `Content-Type` if declared. **`User-Agent` is force-set** to `voodu-deploy-webhook` (source-of-call debug signal). |
| `body` | no, mutex with `file` | — | Inline HCL object literal. Becomes the JSON request body verbatim. Use for small / one-off bodies (≤ 5 lines of flat fields). |
| `file` | no, mutex with `body` | — | Asset reference (`${asset.scope.name.key}`) pointing at a JSON template file. Use for rich bodies (Slack Block Kit, PagerDuty Events v2, anything multi-line nested). Asset-only (bare paths rejected). |

### Body interpolation

Two contexts. Both work inside the inline `body { ... }` and inside the asset-backed JSON file:

| token | resolved | when | example |
|---|---|---|---|
| `${VAR}` | shell env **+ env_from'd config buckets** | parse-time, client-side | `${PD_ROUTING_KEY}`, `${SLACK_WEBHOOK_URL}` |
| `{{field}}` | release context | fire-time, controller-side | `{{name}}`, `{{status}}`, `{{error}}` |

### Where `${VAR}` reads from

When the resource declares `env_from = ["prod/shared"]`, the CLI **fetches that bucket from the controller before parsing the manifest** and layers its values into the `${VAR}` interpolation context. So:

```hcl
app "prod" "api" {
  env_from = ["prod/shared"]

  on_deploy {
    success {
      url = "${SLACK_WEBHOOK_URL}"  # ← resolves from prod/shared bucket
    }
  }
}
```

Setup the bucket once:
```bash
vd config set -s prod -n shared \
  SLACK_WEBHOOK_URL="https://hooks.slack.com/..." \
  PD_ROUTING_KEY="R000..."
```

After that, anyone on the team runs `vd apply` and the manifest substitutes correctly — no per-dev `export` needed, no `.envrc` to maintain. Rotation via `vd config set` propagates to every dev's next apply.

Precedence (later wins, mirrors runtime env_from layering):
1. env_from'd bucket vars, in declared order (later refs override earlier on the same key)
2. operator's shell env (always wins — allows ad-hoc override: `SLACK_URL=https://test/h vd apply`)

Caveat: this lookup is **local-apply only**. `vd apply -r prod` (SSH-forward path) keeps the legacy shell-only interpolation — the local CLI doesn't know which controller's buckets to ask. For remote applies, fall back to direnv / shell exports.

Allowed `{{...}}` tokens (case-sensitive):

```
{{kind}}        {{scope}}         {{name}}
{{release_id}}  {{image}}
{{status}}      {{error}}
{{started_at}}  {{completed_at}}
```

Unknown `{{...}}` markers are left **literal** — some webhook receivers themselves use handlebars-style templates and shouldn't be touched.

### Default body

If neither `body` nor `file` is declared, voodu sends the fixed default payload:

```json
{
  "kind": "deployment", "scope": "...", "name": "...",
  "release_id": "...", "image": "...", "status": "success|failure",
  "started_at": "...", "completed_at": "...", "error": ""
}
```

`Content-Type: application/json`, `User-Agent: voodu-deploy-webhook`.

When `body` or `file` IS declared, voodu sends EXCLUSIVELY the operator's body — the default fields are NOT merged in. If you want them, include them explicitly in your template (the `{{...}}` tokens cover every default field).

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

Best-effort, **per-target**. Each declared block fires in its
own goroutine with its own 3-attempt retry budget (exponential
backoff: 1s, 5s, 30s between attempts; 10s per-attempt HTTP
timeout). If all three fail for one target, voodu logs the drop
(`target=<i>/<total>` identity when multiple targets exist) and
moves on. A failure on one target does NOT affect the others.

**The rollout outcome is NEVER affected by webhook delivery** —
by the time the hook fires, the deploy already succeeded or
failed on its own merits.

### Parallel fan-out

When a slot has multiple blocks, voodu spawns one goroutine per
target. Total wall-clock = `max(per-target latency)`, not sum.
No ordering guarantees: log lines from different targets are
interleaved by goroutine scheduling.

### Validation errors with index

When multiple targets are declared and one fails parse-time
validation, the error includes the offending block index:

```
on_deploy.failure[2].url is required
```

Single-target validation stays terse:

```
on_deploy.failure.url is required
```

## Why NOT in spec hash

Editing `on_deploy.success` from one Slack URL to another is a
notification-routing change, not a runtime change. The container
behaves identically regardless of which webhook URL voodu POSTs
to afterward. Folding webhook URLs into the spec hash would churn
replicas on every Slack workflow rename — meaningful pain for zero
operational benefit. So `on_deploy` lives outside the hash.

You can rotate webhook URLs as freely as you'd rotate Slack channel
names.

## One URL vs two

- **Same URL for both** — single channel, ops team sees every
  rollout regardless of outcome. Default payload includes a
  `status` field receivers branch on. See [`slack-notify.hcl`](./slack-notify.hcl).
- **Different URLs per outcome** — successes are informational
  (low-urgency channel, no paging); failures are actionable
  (PagerDuty, on-call channel). See [`pagerduty-on-failure.hcl`](./pagerduty-on-failure.hcl).

## Inline `body` vs asset `file` — when to use which

Rule of thumb:

- **≤ a few flat fields** → inline body. Keeps the request shape
  visible alongside the URL it ships to. See [`telegram-bot.hcl`](./telegram-bot.hcl).
- **≥ 5 lines of nested JSON** → asset-backed file. The HCL stays
  short; the body template lives in a `.json` file you can lint,
  diff, syntax-highlight, and share across deployments. See
  [`slack-block-kit.hcl`](./slack-block-kit.hcl) and [`pagerduty-on-failure.hcl`](./pagerduty-on-failure.hcl).

## Examples

| file | what it shows |
|---|---|
| [`slack-notify.hcl`](./slack-notify.hcl) | Default voodu payload — no `body`/`file` declared. Slack workflow branches on `status` for tone |
| [`slack-block-kit.hcl`](./slack-block-kit.hcl) | Asset-backed Slack Block Kit JSON: rich messages with headers, code blocks, @channel mentions. Different template per outcome |
| [`telegram-bot.hcl`](./telegram-bot.hcl) | **Inline body** — small Telegram bot API payload (`chat_id`, `text`, `parse_mode`). Token in URL, chat_id from shell env |
| [`pagerduty-on-failure.hcl`](./pagerduty-on-failure.hcl) | Asymmetric routing: Slack on success (default body), PagerDuty Events API v2 direct on failure (asset-backed body with the receiver-specific schema) |
| [`fanout-multi-target.hcl`](./fanout-multi-target.hcl) | **Multi-target fan-out** — 3 success targets (Slack + Datadog + internal status bot) and 2 failure targets (PagerDuty + OpsGenie). Each fires in its own goroutine with its own retry budget |
| `webhooks/*.json` | Body template files referenced by the asset-backed examples |
