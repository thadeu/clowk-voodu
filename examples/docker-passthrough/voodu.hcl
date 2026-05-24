// docker-passthrough/voodu.hcl
//
// Demonstrates the raw `docker run` pass-throughs voodu exposes for
// shapes the typed surface does not model directly:
//
//   - `ulimits = {}`        — per-resource ulimit table (map of name
//                             to "soft:hard" or "N"). Each entry
//                             becomes one `--ulimit <key>=<value>`
//                             flag. Voodu's platform defaults
//                             (nofile=65536:65536, nproc=4096:4096)
//                             still apply for any key the operator
//                             does NOT declare here.
//
//   - `docker_options = []` — raw list of strings appended verbatim
//                             to `docker run` before the image. No
//                             parsing, no validation. Use for compose
//                             knobs voodu doesn't model: `--shm-size`,
//                             `--pids-limit`, `--sysctl`, `--device`,
//                             `--privileged`, etc.
//
// FOOTGUN: do NOT redeclare flags voodu already manages (--name,
// --network, --restart, --env-file, --cpus, --memory, --ulimit,
// --label, --add-host, --cap-add, --log-opt). docker rejects
// duplicates at container create time.
//
// Both fields are available on every container-spawning kind:
// deployment, statefulset, job, cronjob, app, and init blocks. Plugin
// kinds (postgres, redis, mongo, caddy, …) inherit the same surface
// because they emit deployment/statefulset manifests via the standard
// apply pipeline.

deployment "demo" "search" {
  image = "ghcr.io/myorg/search:1.0"

  // Big file table + locked RSS — typical for in-memory search /
  // cache workloads with hot socket pools. `nofile` overrides the
  // platform default 65536:65536; `nproc` stays at the default
  // because we didn't declare it.
  ulimits = {
    nofile  = "1048576:1048576"
    memlock = "-1"
  }

  // Raw bypass — three knobs voodu's typed surface doesn't expose:
  docker_options = [
    "--shm-size=2g",                                // larger /dev/shm
    "--sysctl=net.core.somaxconn=4096",             // bigger accept queue
    "--sysctl=net.ipv4.tcp_keepalive_time=60",      // faster keepalive
    "--pids-limit=4096",                            // PID cgroup cap
  ]
}

// Statefulset shape — same fields, same behaviour.
statefulset "data" "redis" {
  image    = "redis:7"
  replicas = 1

  ulimits = {
    nofile = "200000:200000"
  }

  docker_options = [
    "--sysctl=net.core.somaxconn=2048",
  ]

  volume_claim "data" {
    mount_path = "/data"
    size       = "10Gi"
  }
}

// Per-init override: the migration step needs a huge shm for a heavy
// schema dump, even though the main app stays tight. When `ulimits` /
// `docker_options` are declared inside an `init {}`, they REPLACE the
// parent's at that init's docker-run (they don't merge per-key — the
// override is the whole field).
deployment "demo" "api" {
  image = "ghcr.io/myorg/api:1.0"

  init "schema-dump" {
    command        = ["bin/dump-schema"]
    docker_options = ["--shm-size=4g"]
  }
}
