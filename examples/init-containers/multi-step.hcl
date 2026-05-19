# Three sequential init containers — declared in execution order.
#
# Flow per replica spawn:
#
#   1. validate-config  — runs the app's own config-check
#      command. Fails the pod immediately if env / secrets are
#      malformed, before any expensive work.
#
#   2. migrate          — schema migration (DB write).
#
#   3. warm-cache       — pre-populates Redis with hot keys so the
#      first real request doesn't hit a cold cache. Heavier
#      memory footprint than the main pod, so we override
#      resources here.
#
# If init[1] (validate-config) fails, init[2] never runs and the
# replica never spawns. The failure is recorded:
#
#   vd describe deployment prod/api
#
#   init failures (recent):
#     a3f9   init=validate-config   exit=2   attempts=1   30s   container exited 2: missing DATABASE_URL
#
# Apply:
#
#   cd examples/init-containers
#   vd apply -f multi-step.hcl

deployment "prod" "api" {
  image    = "ghcr.io/acme/api:1.4"
  replicas = 3

  ports = ["3000"]

  env = {
    RAILS_ENV = "production"
  }

  env_from = ["prod/db-credentials", "prod/cache-credentials"]

  # Step 1: cheapest possible gate. Validate env before anything
  # else runs. If DATABASE_URL is malformed we want to know in
  # 2s, not after a 5-minute migration timeout.
  init_container "validate-config" {
    command = ["bin/config-check"]
    timeout = "30s"
    retries = 0
  }

  # Step 2: schema migration. Idempotent, may take a while.
  init_container "migrate" {
    command = ["bin/rails", "db:migrate"]
    timeout = "10m"

    # Migrations occasionally hit lock contention on the
    # migration_versions table during rapid scale-up. One retry
    # absorbs the race without masking real failures.
    retries = 1
  }

  # Step 3: cache warm-up. Memory-heavy compared to the main
  # pod (loads a large fixture set into Redis). Override
  # resources so we don't OOM under the parent's cap.
  init_container "warm-cache" {
    command = ["bin/warm-cache"]
    timeout = "5m"

    resources {
      limits {
        cpu    = "2"
        memory = "1Gi"
      }
    }
  }
}
