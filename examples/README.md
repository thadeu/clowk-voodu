# voodu examples

End-to-end manifest examples grouped by what they showcase. Each subdirectory is self-contained — you can `vd apply -f <subdir>/voodu.hcl` against any voodu controller.

## Layout

| dir | what it shows |
|---|---|
| [`asset/`](./asset) | Standalone `asset` blocks with `file()`, `url()`, and inline string sources; scoped (`asset "scope" "name"`) and unscoped (`asset "name"`) shapes; combination with a `deployment` mounting the materialised paths via `${asset.…}` |
| [`statefulset/`](./statefulset) | Single-node and multi-replica statefulsets (postgres, redis) with per-pod ordinal identity and persistent volume claims |
| [`stack/`](./stack) | Production-grade full stack: postgres + redis (macro plugins) + asset (postgresql.conf / pg_hba.conf / redis.conf / ACL) + app (deployment + ingress) with TLS |
| [`fullstack/`](./fullstack) | Simple deployment + ingress pair (no databases). Good first read for the basic shapes |
| [`fullstack-yaml/`](./fullstack-yaml) | Same shape as `fullstack/`, written in YAML to show the alternate format |
| [`ingress/`](./ingress) | Path-based routing, multiple hosts, load-balancing knobs |
| [`multi-env/`](./multi-env) | One manifest, many remotes — apply the same file to staging / prod-1 / prod-2 via `-r` |
| [`shared-scope/`](./shared-scope) | Cross-repo applies into the same scope using `?prune=false` |

## Pattern reference

### Where each kind lives

- **`asset`** — declarative file bundles. The body is a flat key-to-source map. Server materialises into `/opt/voodu/assets/<scope>/<name>/<key>` so any other resource can mount the path via bind volume. See `asset/basic.hcl`.

- **`statefulset`** — pods with stable per-ordinal identity, one docker volume per claim per ordinal, rolling restart that preserves data. Image-mode only. See `statefulset/postgres.hcl` for the bare shape.

- **`postgres` / `redis`** (macro plugins) — dumb aliases of `statefulset` with sensible defaults. Operator declares overrides; the plugin fills in what's missing. Custom configs flow through `asset` blocks, NOT through plugin-specific knobs. See `stack/voodu.hcl`.

- **`deployment`** — long-running stateless replicas, opaque interchangeable identity. See `fullstack/deployment.hcl`.

- **`app`** — sugar for `deployment` + `ingress` with the same identity. See `multi-env/app.voodu`.

- **`ingress`** — host routing, TLS, load balancing. See `ingress/`.

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
