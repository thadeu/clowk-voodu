# fsw-esl/

Real-world translation of a production docker-compose stack for a
FreeSWITCH event-socket (ESL) telephony layer: redis + rabbitmq + 5
Go services (api, adapter, controller, events, jobs), all built from
one shared `./apps/esl` context with a `SERVICE` build-arg.

This example exists to show:

1. How a working compose stack maps to voodu **when you want the
   operational features compose doesn't give you** — autoscale, probes
   that drive ingress gating, multi-target deploy webhooks, statefulset
   volumes that survive prune, shared config buckets.

2. How to **split per-service files** so individual service deploys
   don't churn unrelated services.

3. How to run **the same HCL files against multiple environments**
   (staging, prod) by varying only the remote target and per-host
   config bucket values.

## Layout

```
infra/fsw/
├── README.md
├── redis.hcl           ← harness (rarely changes)
├── rabbitmq.hcl        ← harness (rarely changes)
├── api.hcl             ← per-service files, each deployable in isolation
├── adapter.hcl
├── controller.hcl
├── events.hcl
└── jobs.hcl
```

## Mapping (compose → voodu)

| Compose service | File | Voodu kind | Why |
|---|---|---|---|
| `redis` | `redis.hcl` | `redis` macro | Plugin expansion gives default probes + REDIS_URL emission via `vd redis:link`. |
| `rabbitmq` | `rabbitmq.hcl` | `statefulset` | Per-pod `volume_claim "data"` survives `vd apply --prune`. |
| `api` | `api.hcl` | `deployment` + `autoscale` | Stateless HTTP. Compose runs 1 replica; voodu scales 2–4. |
| `adapter` | `adapter.hcl` | `deployment` + `autoscale` | Stateless HTTP router. Voodu scales 2–6. |
| `controller` | `controller.hcl` | `deployment` (replicas=1) | Single-instance + `on_deploy` Slack/PagerDuty fan-out. |
| `events` | `events.hcl` | `deployment` + `autoscale` | Background AMQP consumer. Voodu scales 1–4. |
| `jobs` | `jobs.hcl` | `deployment` + `autoscale` | Background workers + host bind-mount. Voodu scales 2–10. |

## Why split into per-file?

**Independent deploys.** Touching `jobs.hcl` shouldn't deploy `api`,
`adapter`, or anything else. With one file:

```bash
vd apply -f infra/fsw/jobs.hcl -r prod
# → only the jobs deployment is reconciled.
# api/adapter/controller/events stay running on their current image.
```

With a monolithic `voodu.hcl`, every apply re-reconciles every resource.
Voodu's spec-hash check still skips no-op restarts (no churn if nothing
changed), but the per-file pattern gives you tighter blast-radius +
clearer diff output + faster apply.

## Why same files for staging and prod?

The HCL describes **shape**, not values. Values come from the config
bucket, which lives in etcd on **each remote independently**:

```bash
# Same HCL, different remotes.
vd apply -f infra/fsw/ -r staging
vd apply -f infra/fsw/ -r prod

# Different bucket values per remote (lives in each host's etcd).
vd config fsw/shared set REDIS_ADDR="..." RABBITMQ_URL="..." -r staging
vd config fsw/shared set REDIS_ADDR="..." RABBITMQ_URL="..." -r prod
```

Staging and prod run **bit-identical container images** with **different
env**. The contract: a successful staging deploy guarantees the same
image will boot on prod (modulo data differences).

## ⚠️ `--prune` gotcha

`--prune` is per-`(scope, kind)`. **Never use it on a per-file deploy**:

```bash
vd apply -f infra/fsw/jobs.hcl --prune -r prod   # ⚠️ NÃO!
```

Voodu would:

1. List existing `(fsw, deployment)` resources → finds 5 (api, adapter,
   controller, events, jobs)
2. See `jobs.hcl` declares 1 → marks the other 4 as "not in this apply"
3. Delete the 4 unrelated services

Rule:

| Command | `--prune` |
|---|---|
| `vd apply -f infra/fsw/<one>.hcl` | **never** |
| `vd apply -f infra/fsw/` (whole dir) | safe to use — voodu sees every declared resource |

The default upsert-only behavior protects you. Only opt into `--prune`
when you're applying the full source-of-truth.

## Apply

### First bootstrap (fresh host)

```bash
# 1. Set the shared config bucket (replaces compose's .env + YAML anchor).
#    Run this against each environment (staging + prod) with the right values.
vd config fsw/shared set \
  REDIS_ADDR="redis.fsw.voodu:6379" \
  RABBITMQ_URL="amqp://guest:guest@rabbitmq-0.fsw.voodu:5672/" \
  DIAL_ADAPTER_URL="http://adapter.fsw.voodu:8080" \
  FSW_API_BASE_URL="http://api.fsw.voodu:9092" \
  TTS_SERVICE_URL="http://api.fsw.voodu:9092" \
  WEBSERVICE_URL="http://host.docker.internal:9099" \
  ADAPTER_LISTEN_ADDR="0.0.0.0:8080" \
  HTTP_ROUTER_LISTEN_ADDR="0.0.0.0:8080" \
  TTS_LISTEN_ADDR="0.0.0.0:9092" \
  CONTROLLER_ESL_LISTEN_ADDR="0.0.0.0:9090" \
  CONTROLLER_API_LISTEN_ADDR="0.0.0.0:9091" \
  ESL_LISTEN_ADDR="0.0.0.0:9090" \
  CONTROLLER_ESL_SOCKET_ADDR="controller.fsw.voodu:9090" \
  ESL_INBOUND_ADDR="fsw.voodu:8021" \
  FSW_RECORDINGS_BASE_DIR="/var/lib/fsw/recordings" \
  -r prod

# 2. Notification webhooks.
vd config fsw/shared set \
  SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T../B../..." \
  PD_ROUTING_KEY="R000..." \
  -r prod

# 3. Apply the whole stack.
vd apply -f infra/fsw/ -r prod
```

### Day-to-day deploys

```bash
# Deploy one service after a code change in apps/esl
vd apply -f infra/fsw/jobs.hcl -r prod

# Inspect
vd describe deployment fsw/jobs -r prod
vd logs fsw/jobs -f -r prod
vd pods -s fsw -r prod
```

### Source-of-truth re-apply (cleanup)

```bash
# Walks the dir; deletes resources not declared anywhere.
vd apply -f infra/fsw/ --prune -r prod
```

### Promote staging → prod

```bash
# Same HCL, different remote. Image tag pinning in HCL ensures both
# environments run the same artifact.
vd apply -f infra/fsw/ -r staging
# ... validate ...
vd apply -f infra/fsw/ -r prod
```

## Rollback

```bash
# Per-service rollback to the previous release on prod.
vd rollback deployment fsw/api -r prod

# Pin to a specific release ID.
vd rollback deployment fsw/api lyhmf6ab -r prod
```

Releases are content-addressed (sha256 of the build tarball) — rolling
back is a symlink swap, no rebuild.

## What's intentionally not here

- **FreeSWITCH itself** (`fsw.voodu:8021` in the env) — assumed to be
  on a separate voodu manifest or another host. Adding it would be
  another `statefulset` with RTP/SIP UDP ports and heavy resources.

- **Public ingress** — no service has a public hostname in the compose.
  To expose `api` externally, switch its `deployment` to `app` and add
  `host = "..."` + `tls { email = "..." }`.

- **AWS S3 / localstack** — commented out in the compose. The voodu
  pattern is a separate bucket:

  ```bash
  vd config aws/cli set AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_REGION=us-east-1 -r prod
  ```

  Then on `jobs.hcl`:

  ```hcl
  env_from = ["fsw/shared", "aws/cli"]
  ```
