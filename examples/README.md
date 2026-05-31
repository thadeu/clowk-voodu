# voodu examples

End-to-end manifest examples grouped by what they showcase. Each subdirectory is self-contained — you can `vd apply -f <subdir>/voodu.hcl` against any voodu controller.

## Layout

| dir | what it shows |
|---|---|
| [`procfile/`](./procfile) | The zero-HCL on-ramp — `vd apply -f Procfile`. Heroku/Dokku-style process lines become deployments (+ a release job), build once and retag across processes, optional ingress via `.voodu/app.json`, eject to HCL when you outgrow it |
| [`asset/`](./asset) | Standalone `asset` blocks with `file()`, `url()`, and inline string sources; scoped (`asset "scope" "name"`) and unscoped (`asset "name"`) shapes; combination with a `deployment` mounting the materialised paths via `${asset.…}` |
| [`build/`](./build) | `build {}` block — build images from source instead of pulling from a registry. docker-compose-shaped (`context`, `dockerfile`, `args`). Covers auto-detect, custom Dockerfile, Go monorepo, statefulset build |
| [`statefulset/`](./statefulset) | Single-node and multi-replica statefulsets (postgres, redis) with per-pod ordinal identity and persistent volume claims |
| [`stack/`](./stack) | Production-grade full stack: postgres + redis (macro plugins) + asset (postgresql.conf / pg_hba.conf / redis.conf / ACL) + app (deployment + ingress) with TLS |
| [`app/`](./app) | The `app` sugar block (deployment + ingress in one). One example showing EVERY shipped feature composed together: init / probes / autoscale / on_deploy (asset-backed PagerDuty body) / resources / registry + postgres + redis |
| [`probes/`](./probes) | Kubelet-style health probes (liveness / readiness / startup) on deployments and statefulsets. HTTP, TCP, exec selectors. Auto caddy ingress gating via the readiness probe |
| [`init/`](./init) | Ordered one-shot prep steps (db:migrate, pg_basebackup, config-validate) that must complete before main container starts. HCL keyword: `init`. |
| [`autoscale/`](./autoscale) | CPU-based horizontal autoscale block on deployments. Worker (sidekiq) + HTTP-tier tunings with the asymmetric "respond fast, retreat slowly" posture |
| [`on_deploy/`](./on_deploy) | Post-rollout webhook notifications. `success` and `failure` slots are **repeatable** — declare multiple blocks per slot for parallel fan-out (Slack + Datadog + internal bot on success; PagerDuty + OpsGenie on failure). Each block carries `url` / `method` / `headers` / inline `body` / asset-backed `file`. `${VAR}` in any of those fields can come from `env_from`'d config buckets — set webhook URLs / API tokens once via `vd config set`, every dev's `vd apply` picks them up. Examples: bare Slack (default payload), Slack Block Kit (asset-backed rich messages), Telegram bot (inline body), PagerDuty Events v2 (asset-backed receiver-specific schema), multi-target fan-out (3 success + 2 failure targets, independent retry budgets) |
| [`registry/`](./registry) | The `registry` kind — private image pulls. Atomic ~/.docker/config.json regen, host-wide auth, single + multi-registry examples |
| [`fullstack/`](./fullstack) | Simple deployment + ingress pair (no databases). Good first read for the basic shapes |
| [`ingress/`](./ingress) | Path-based routing, multiple hosts, load-balancing knobs |
| [`multi-env/`](./multi-env) | One manifest, many remotes — apply the same file to staging / prod-1 / prod-2 via `-r` |
| [`shared-scope/`](./shared-scope) | Cross-repo applies into the same scope using `?prune=false` |
| [`docker-passthrough/`](./docker-passthrough) | Raw `docker run` pass-throughs — `ulimits = {}` (per-key override of platform defaults) and `docker_options = []` (verbatim flag bypass for `--shm-size`, `--sysctl`, `--pids-limit`, `--device`, etc.). Available on every kind |

## Pattern reference

### Where each kind lives

- **`asset`** — declarative file bundles. The body is a flat key-to-source map. Server materialises into `/opt/voodu/assets/<scope>/<name>/<key>` so any other resource can mount the path via bind volume. See `asset/basic.hcl`.

- **`statefulset`** — pods with stable per-ordinal identity, one docker volume per claim per ordinal, rolling restart that preserves data. Image-mode only. See `statefulset/postgres.hcl` for the bare shape.

- **`postgres` / `redis`** (macro plugins) — dumb aliases of `statefulset` with sensible defaults. Operator declares overrides; the plugin fills in what's missing. Custom configs flow through `asset` blocks, NOT through plugin-specific knobs. See `stack/voodu.hcl`.

- **`deployment`** — long-running stateless replicas, opaque interchangeable identity. See `fullstack/deployment.hcl`.

- **`app`** — sugar for `deployment` + `ingress` with the same identity. See `multi-env/app.voodu`.

- **`ingress`** — host routing, TLS, load balancing. See `ingress/`.

### Registry mode vs build mode

Every `deployment` / `statefulset` / `job` / `cronjob` / `app` picks one of two source modes (parse error if both):

- **`image = "ghcr.io/me/api:v1.2"`** — registry mode. Voodu pulls the named image and runs it. CI builds and pushes; voodu deploys. This is what all examples in this directory use by default.

- **`build { context = "apps/api", ... }`** — build mode. Voodu tarballs `context`, ships it to the controller, runs `docker build`, and tags `<scope>-<name>:latest` for the workload to pull. Use when you don't have (or want) a CI publishing images.

Auto-detect: `deployment "scope" "name" {}` with neither field gets `build = { context = "." }` synthesised — the "ship me from this repo, figure the rest out" shape. See [`build/`](./build) for end-to-end examples covering custom Dockerfiles, Go monorepos, statefulset build, and the auto-detect shape.

### Asset interpolation — scoped vs unscoped

Inside any string field of any kind (volumes, command, ports, env values, image), asset refs resolve at reconcile time to the materialised host path. Two shapes:

- **`${asset.<scope>.<name>.<key>}`** (4 segments) — addresses a **scoped** asset. Asset is declared with two labels: `asset "<scope>" "<name>"`. This is the common case — keeps assets isolated per tenant / scope.
- **`${asset.<name>.<key>}`** (3 segments) — addresses an **unscoped** (global) asset. Asset is declared with one label: `asset "<name>"`. Useful for shared bytes (CA bundles, shared ACLs, MOTDs) addressed from many scopes without duplication.

Both forms can coexist in the same string. There is no implicit-scope fallback: a 3-segment ref ONLY matches an unscoped asset, and a 4-segment ref ONLY matches a scoped asset with the matching `(scope, name)`.

```hcl
asset "data" "redis-config" {
  configuration = file("./redis/redis.conf")
}

asset "ca-bundle" {
  pem = file("./tls/ca.pem")
}

statefulset "data" "cache" {
  command = ["redis-server", "/etc/redis/redis.conf"]

  volumes = [
    "${asset.data.redis-config.configuration}:/etc/redis/redis.conf:ro",
    "${asset.ca-bundle.pem}:/etc/ssl/ca.pem:ro",
  ]
}
```

The asset's content hash folds into the statefulset's spec hash, so editing `./redis/redis.conf` and re-applying triggers a rolling restart automatically. No `vd restart` needed.

## Running an example

`file("./...")` resolves relative to the **CLI's CWD**, not to the manifest path. Always `cd` into the example's directory before applying so the relative paths line up:

```bash
cd examples/stack

# secrets stay out of the manifest
PG_PASS=$(openssl rand -hex 16)
vd config set -s data -n pg POSTGRES_PASSWORD=$PG_PASS
vd config set -s myapp DATABASE_URL="postgres://postgres:$PG_PASS@pg-0.data:5432/myapp"
vd config set -s myapp REDIS_URL="redis://cache-0.data:6379/0"

vd apply -f voodu.hcl
```

On first apply, the controller JIT-installs `voodu-postgres` and `voodu-redis` plugins from GitHub releases; subsequent applies reuse them.

Plugin version control via the nested `plugin { … }` block. Three modes:

```hcl
# 1. Pin a specific git tag — deterministic, re-installs on mismatch.
redis "data" "cache" {
  plugin { version = "0.2.0" }
}

# 2. Always-refresh — re-fetches the default branch every apply.
#    Picks up new plugin versions without changing HCL. Useful
#    during plugin development; pin a real tag for prod.
redis "data" "cache" {
  plugin { version = "latest" }
}

# 3. Block omitted — uses whatever's installed; bump explicitly
#    via `vd plugins:upgrade`. Fastest apply (no network roundtrip).
redis "data" "cache" {
}
```

Repo override (forks, internal mirrors):

```hcl
redis "data" "cache" {
  plugin {
    version = "0.2.2"
    repo    = "myorg/voodu-redis-fork"
  }
}
```
