# fsw-esl/

Real-world translation of a production docker-compose stack that runs a
FreeSWITCH event-socket (ESL) telephony layer: redis + rabbitmq + 5 Go
services (api, adapter, controller, events, jobs), all built from one
shared `./apps/esl` context with a `SERVICE` build-arg.

This example exists to show how a working compose stack maps to voodu
**when you want the operational features compose doesn't give you** —
autoscale, probes that drive ingress gating, multi-target deploy
webhooks, statefulset volumes that survive prune, shared config buckets.

## Mapping

| Compose service | Voodu kind | Why |
|---|---|---|
| `redis` | `redis` macro | Plugin expansion gives default probes + REDIS_URL emission via `vd redis:link`. No hand-rolling. |
| `rabbitmq` | `statefulset` | Per-pod `volume_claim "data"` survives `vd apply --prune`. Compose's named volume only survives `docker compose down`. |
| `controller` | `deployment` (replicas=1) | Single-instance shape from compose. ESL connections break on rollout for either kind; deployment is simpler and supports `on_deploy` webhooks. Switch to statefulset if you need on-disk state or multi-replica sticky routing. |
| `api` | `deployment` + `autoscale` | Stateless HTTP. Compose runs 1 replica; voodu scales 2–8 on CPU. |
| `adapter` | `deployment` + `autoscale` | Stateless HTTP router. Compose runs 1 replica; voodu scales 2–6. |
| `events` | `deployment` + `autoscale` | Background AMQP consumer, no ports. Voodu scales 1–4. |
| `jobs` | `deployment` + `autoscale` | Background workers + host bind-mount for recordings. Voodu scales 2–10. |

## Translations of compose features

| Compose | Voodu |
|---|---|
| `env_file: ./apps/esl/.env` | `env_from = ["fsw/shared"]` → config bucket set via `vd config fsw/shared set` |
| YAML anchors `*docker_net_env` | Single config bucket consumed by all services |
| `depends_on: { service_healthy }` | `init "wait-X" { command = [...nc/redis-cli...] }` |
| `healthcheck:` | `probes { liveness { ... } readiness { ... } startup { ... } }` |
| `restart: unless-stopped` | Default in voodu (no field needed) |
| `extra_hosts: "host.docker.internal:host-gateway"` | Auto-injected by voodu for every container |
| `networks: [default, fsw_shared]` | All containers join `voodu0` automatically; cross-resource service-name DNS works out of the box |
| `volumes: [name:/path]` (named) | `volume_claim "name" { mount_path = "/path" }` on statefulsets; per-pod docker volumes |
| `volumes: ["/host/path:/in/container"]` (bind) | Same shape — `volumes = ["/host:/container:rw"]` on any kind |
| `build: { context, dockerfile, args }` | `build { context, dockerfile, args }` — same fields, content-addressed cache |

## What voodu adds

1. **Autoscale** — CPU-based, hysteresis ×1.1 / ×0.7, asymmetric cooldown
   (fast scale-up `30s`, slow scale-down `5m`). Compose has no native
   scaling; you'd hand-roll a sidecar.

2. **Probes drive caddy** — when you front a service with `app` + `host
   = "..."`, the readiness probe controls which replicas Caddy routes to.
   Compose's healthchecks only restart containers.

3. **`vd redis:link`** — emits `REDIS_URL` (and optionally `REDIS_READ_URL`)
   into a consumer's config bucket. The Go services in this stack read
   `REDIS_ADDR` directly, so we set that in `fsw/shared`. Migrate to
   `REDIS_URL` and `vd redis:link` becomes the canonical wiring.

4. **`on_deploy` multi-target** — controller rollouts fire Slack AND
   PagerDuty in parallel goroutines. Each target has its own retry
   budget; a slow PagerDuty doesn't delay Slack.

5. **Stable ordinals for stateful services** — rabbitmq-0 stays
   rabbitmq-0 across deploys, restarts, even prune+re-apply (the
   volume_claim's name is deterministic). FreeSWITCH's ESL outbound
   config can pin to `rabbitmq-0.fsw.voodu` for AMQP and the
   round-robin `controller.fsw.voodu` for the controller.

6. **Build-cache sharing** — every service uses the same
   `./apps/esl` context. voodu sha256s the tarball; identical context
   bytes mean the rebuild is skipped and layers are reused. One `vd
   apply` builds once for all 5 services that share the context.

7. **Config bucket = secrets-out-of-band** — `vd config fsw/shared set`
   replaces `.env` files. Rotate `RABBITMQ_DEFAULT_PASS` and every
   consumer picks it up on next reconcile.

8. **`vd apply --prune`** = source-of-truth apply. Remove a deployment
   from voodu.hcl + `--prune` and it's gone, cleanly. Compose's `down
   --remove-orphans` is the closest equivalent and it deletes
   everything in the stack.

## What's intentionally not here

- **FreeSWITCH itself** (`fsw:8021` in the env) — assumed to be on a
  separate voodu manifest or another host entirely. Adding it would be
  another `statefulset` with RTP/SIP UDP ports and heavy resources. See
  comments at the bottom of `voodu.hcl`.

- **Public ingress** — no service has a public hostname in the
  compose. To expose `api` externally, switch its `deployment` to
  `app` and add `host = "..."` + `tls { email = "..." }`.

- **AWS S3 / localstack** — commented out in the compose. The voodu
  pattern is a separate bucket (`vd config aws/cli set ...`) consumed
  via `env_from`.

## Apply

```bash
# 1. Set the shared config bucket (replaces compose's .env + YAML anchor)
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
  FSW_RECORDINGS_BASE_DIR="/var/lib/fsw/recordings"

# 2. Notification webhooks (used by on_deploy on the controller)
vd config fsw/shared set \
  SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T../B../..." \
  PD_ROUTING_KEY="R000..."

# 3. Apply.
vd apply -f voodu.hcl

# 4. Inspect.
vd describe statefulset fsw/controller
vd logs fsw/api -f
vd pods -s fsw
```

## Scaling

Compose scales by `docker compose up --scale api=4`. Voodu's autoscale
takes care of it automatically based on CPU — but you can also pin a
fixed count by dropping `autoscale {}` and adding `replicas = N`. Or
trigger a manual scale via:

```bash
vd config fsw set FORCE_REPLICAS=...   # not built-in, illustrative
vd restart fsw/api                      # rolls all replicas
```

## Rollback

```bash
vd rollback fsw/api                     # previous release
vd rollback fsw/api lyhmf6ab            # specific release ID
```

Releases are content-addressed (sha256 of the build tarball) so
rolling back is a symlink swap, no rebuild.
