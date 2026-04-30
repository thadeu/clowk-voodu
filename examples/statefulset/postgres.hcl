// statefulset — pods with stable per-ordinal identity, one
// docker volume per (claim, ordinal), and rolling restart that
// preserves data across image bumps.
//
// Compared to deployment: replicas are NOT interchangeable.
// pod-0 is durably pod-0, with the same docker volume mount
// across restarts. DNS aliases include both the shared form
// (`pg.data`, round-robined across replicas) AND a per-pod
// form (`pg-0.data`, `pg-1.data`, ...). Plugins like postgres
// rely on the per-pod aliases — pod-0 is the primary by
// convention; pg-1+ run pg_basebackup against `pg-0.data`.
//
// Image-mode only: there's no build pipeline. Use a registry
// image (postgres, mysql, redis, mongo, etc.) and customise
// via env / command / volumes.
//
// Single-node example below. For multi-node replication, raise
// `replicas` and provide an init script via a custom image
// (the bare statefulset doesn't ship clustering logic — that's
// what voodu-postgres / voodu-mysql plugins eventually offer).

statefulset "data" "pg" {
  image    = "postgres:15-alpine"
  replicas = 1

  env = {
    POSTGRES_DB = "myapp"
    PGDATA      = "/var/lib/postgresql/data/pgdata"

    // POSTGRES_PASSWORD is NOT in the manifest — secrets stay
    // out of the declarative config. Operator runs:
    //
    //   vd config set -s data -n pg POSTGRES_PASSWORD=<value>
    //
    // before the first apply; the controller injects it via
    // the env file the container loads at boot.
  }

  ports = ["5432"]

  // Per-pod docker volume. Each ordinal gets its own:
  //   voodu-data-pg-data-0
  //   voodu-data-pg-data-1   (when replicas grows)
  //
  // Volumes survive restarts, image bumps, scale-down, and
  // `vd delete statefulset/data/pg`. They're destroyed only
  // when the operator explicitly opts in:
  //
  //   vd delete statefulset/data/pg --prune
  volume_claim "data" {
    mount_path = "/var/lib/postgresql/data"
  }
}
