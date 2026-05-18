// Postgres + pgvector, built inline from a Dockerfile that does:
//
//   FROM postgres:16
//   RUN apt-get update && apt-get install -y postgresql-16-pgvector
//
// Use case: you want a custom postgres image (pgvector, pg_repack,
// timescaledb, postgis, ...) without setting up a separate CI to
// publish it to a registry. `vd apply` builds the image on the
// controller; the statefulset reconciler pulls it for the pod.
//
// Statefulsets support build mode identically to deployments — same
// `build {}` block, same applyDefaults. The difference vs a deployment
// is the runtime side (stable per-pod ordinal identity, persistent
// volume claims), which is orthogonal to how the image is produced.
//
// On `vd apply`:
//
//   - CLI tarballs ./infra/postgres
//   - Controller runs `docker build -f Dockerfile.pgvector ./infra/postgres`
//   - Tags `data-pg:latest`
//   - Statefulset pod pulls the tag, ordinal-0 boots as primary

statefulset "data" "pg" {
  replicas = 1
  ports    = ["5432"]

  env = {
    POSTGRES_DB = "appdata"
  }

  // Single per-pod volume claim — postgres data dir survives
  // restarts, scale-downs, and even `vd delete` (operator opts
  // into destruction via --prune).
  volume_claim "data" {
    mount_path = "/var/lib/postgresql/data"
    size       = "20Gi"
  }

  build {
    context    = "infra/postgres"
    dockerfile = "Dockerfile.pgvector"

    args = {
      PG_MAJOR    = "16"
      PGVECTOR_VERSION = "0.7.4"
    }
  }
}
