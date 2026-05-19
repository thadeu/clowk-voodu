# Rails app with a single migration init container.
#
# The canonical "run db:migrate before the web pods start"
# pattern. Compare to release block:
#
#   - `release { command = [...] }` runs ONCE per deploy, before
#     any rolling restart. Best for things that should happen
#     exactly once (schema migrations on the deploy boundary).
#
#   - `init_container "migrate" { ... }` runs PER REPLICA, every
#     time a replica is born. Idempotent migrations are fine
#     either way; for "migration is the deploy gate, fail the
#     whole rollout if it can't run" use release. For "every pod
#     re-checks the schema before serving" use init_container.
#
# We use init_container here because:
#   (a) `db:migrate` is idempotent (Rails tracks schema versions).
#   (b) Scale-up after an out-of-band schema change (operator ran
#       `ALTER TABLE` directly) gets caught at the next spawn.
#   (c) The init runs inside the pod's network, so DATABASE_URL
#       resolves the same way the main container will see it —
#       no separate "release env" debug rabbit hole.
#
# Apply:
#
#   cd examples/init-containers
#   vd config set -s prod DATABASE_URL=postgres://...
#   vd apply -f rails-migrate.hcl

deployment "prod" "rails-web" {
  image    = "ghcr.io/acme/rails-web:2025-05-19"
  replicas = 3

  ports = ["3000"]

  env = {
    RAILS_ENV         = "production"
    RAILS_LOG_TO_STDOUT = "1"
  }

  # Bring DATABASE_URL in from a shared scope bucket.
  env_from = ["prod/db-credentials"]

  init_container "migrate" {
    # image omitted → inherits rails-web:2025-05-19. The
    # migration runs against the same code the main pod will
    # run. No drift possible.

    command = ["bin/rails", "db:migrate"]

    # Schema migrations on a multi-GB table can take a few
    # minutes. 10m default is fine; we set it explicitly for
    # documentation.
    timeout = "10m"

    # No retries — a failing migration should fail loudly, not
    # be masked. If you DO want resilience (e.g. flaky network
    # to a managed DB during connection warm-up), set retries = 1.
    retries = 0
  }
}

ingress "prod" "rails-web" {
  service = "rails-web"
  host    = "app.example.com"

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
