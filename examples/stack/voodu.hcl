// Full production-grade stack:
//
//   asset       — postgresql.conf + pg_hba.conf + redis.conf
//                 served from local files (committed in
//                 ./configs) or remote URLs (R2/S3).
//   postgres    — macro plugin, expands to a statefulset with
//                 the asset bind-mounted as the config_file.
//   redis       — same pattern.
//   app         — sugar block expanding to deployment + ingress
//                 with TLS. Reads the database URLs from
//                 controller config (vd config set), NOT from
//                 the manifest.
//
// What ends up running on the host:
//
//   data-pg.0    statefulset pod (postgres single-node)
//   data-cache.0 statefulset pod (redis single-node)
//   myapp-web.<hash>  deployment replicas (3x)
//   voodu-caddy  ingress, terminates TLS, fronts myapp-web.*
//
// Setup before first apply:
//
//   PG_PASS=$(openssl rand -hex 16)
//
//   # postgres pod reads POSTGRES_PASSWORD from its env file
//   vd config set -s data -n pg POSTGRES_PASSWORD=$PG_PASS
//
//   # The app reads DATABASE_URL / REDIS_URL from its env file
//   vd config set -s myapp DATABASE_URL="postgres://postgres:$PG_PASS@pg-0.data:5432/myapp"
//   vd config set -s myapp REDIS_URL="redis://cache-0.data:6379/0"
//
// Apply:
//
//   vd apply -f voodu.hcl
//
// On first apply, the controller JIT-installs voodu-postgres
// and voodu-redis plugins from thadeu/voodu-postgres and
// thadeu/voodu-redis releases. Subsequent applies skip the
// install (plugins are pinned under /opt/voodu/plugins).
//
// To update a config: edit ./configs/redis.conf locally
// (or push a new version to R2), re-apply. The asset hash
// folds into the statefulset spec hash, so a rolling restart
// picks up the new file automatically.

// ---------------------------------------------------------------------------
// Database configs as assets
// ---------------------------------------------------------------------------
//
// Files committed under ./configs/ in this repo OR fetched
// remotely. Both flows reach the container as a bind mount —
// the source choice only affects who owns the bytes.

asset "data" "pg-config" {
  postgresql_conf = file("./configs/postgresql.conf")
  pg_hba_conf     = file("./configs/pg_hba.conf")
}

asset "data" "redis-config" {
  // Local file in this repo
  configuration = file("./configs/redis.conf")

  // Remote — R2 with a pre-signed URL. The controller fetches
  // at apply time, caches by ETag, refetches automatically
  // when the bucket content changes.
  users_acl = url("https://r2.example.com/voodu/redis-users.acl")
}

// ---------------------------------------------------------------------------
// Database statefulsets via macro plugins
// ---------------------------------------------------------------------------
//
// `postgres` and `redis` are macro plugin blocks — they expand
// server-side into core `statefulset` manifests. The plugin
// fills in defaults (image, replicas, default volume claim);
// everything else (image override, command, volumes, ports)
// is operator-supplied and wins outright.

postgres "data" "pg" {
  // Plugin version control. Three modes:
  //
  //   plugin { version = "0.2.0" }   pin to a specific git tag.
  //                                  Re-installs on mismatch;
  //                                  deterministic across applies.
  //
  //   plugin { version = "latest" }  always re-fetch the default
  //                                  branch every apply. Useful
  //                                  during plugin development;
  //                                  picks up new versions without
  //                                  changing HCL.
  //
  //   block omitted                  use whatever's installed
  //                                  locally; bump explicitly via
  //                                  `vd plugins:upgrade`.
  plugin {
    version = "0.2.0"
    // repo = "myorg/voodu-postgres-fork"   # optional override
  }

  image = "postgres:15-alpine"

  command = [
    "postgres",
    "-c", "config_file=/etc/postgresql/postgresql.conf",
    "-c", "hba_file=/etc/postgresql/pg_hba.conf",
  ]

  volumes = [
    "${asset.pg-config.postgresql_conf}:/etc/postgresql/postgresql.conf:ro",
    "${asset.pg-config.pg_hba_conf}:/etc/postgresql/pg_hba.conf:ro",
  ]
}

redis "data" "cache" {
  // While iterating on the redis plugin, `version = "latest"`
  // refreshes from the default branch every apply.
  plugin {
    version = "latest"
  }

  image   = "redis:8"
  command = ["redis-server", "/etc/redis/redis.conf"]

  volumes = [
    "${asset.redis-config.configuration}:/etc/redis/redis.conf:ro",
    "${asset.redis-config.users_acl}:/etc/redis/users.acl:ro",
  ]
}

// ---------------------------------------------------------------------------
// Application — `app` is sugar for deployment + ingress
// ---------------------------------------------------------------------------
//
// One `app` block produces two manifests: a deployment with
// the same identity (`myapp/web`) and an ingress routing
// `myapp.example.com` to that deployment. TLS via
// Let's Encrypt is handled by voodu-caddy automatically.
//
// DATABASE_URL / REDIS_URL are NOT in env here — secrets and
// connection strings live in `vd config set`. The app reads
// them from its env file (controller injects them at boot).

app "myapp" "web" {
  image    = "ghcr.io/myorg/myapp:latest"
  replicas = 3

  ports = ["8080"]

  env = {
    PORT     = "8080"
    NODE_ENV = "production"
  }

  health_check = "/healthz"

  // Ingress side
  host = "myapp.example.com"

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
