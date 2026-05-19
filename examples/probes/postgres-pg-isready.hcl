# Postgres statefulset with pg_isready as the canonical probe.
#
# Why pg_isready and not psql:
#
#   - pg_isready is built specifically for liveness/readiness
#     probes: it does a no-op connection, returns exit 0 for ready,
#     1 for "rejecting connections" (starting up), 2 for "no
#     response", 3 for "no attempt made". The exit codes map cleanly
#     to the probe contract.
#
#   - psql would either need a SQL command (extra moving parts,
#     extra log noise) or `psql -l` (lists databases — opens a
#     real session, more expensive than pg_isready's lightweight
#     handshake).
#
# Liveness uses TCP-socket (cheapest, no postgres binary call)
# because the failure mode we're catching is "process hung, port
# not accepting". Readiness uses pg_isready because we want the
# stricter "actually serving queries" signal — `pg_basebackup` on
# a standby will open the port long before it'll accept queries.
#
# Apply:
#
#   cd examples/probes
#   vd apply -f postgres-pg-isready.hcl

statefulset "data" "pg" {
  image    = "postgres:16"
  replicas = 2

  ports = ["5432"]

  env = {
    POSTGRES_DB       = "myapp"
    POSTGRES_USER     = "postgres"
    # POSTGRES_PASSWORD comes from `vd config set -s data POSTGRES_PASSWORD=...`
    PGDATA            = "/var/lib/postgresql/data/pgdata"
  }

  volume_claim "data" {
    mount_path = "/var/lib/postgresql/data"
    size       = "20Gi"
  }

  probes {
    # Liveness: TCP open = process alive. Cheap; catches the
    # "postgres crashed" case. pg_isready inside the container
    # would also work but adds a fork-exec per sample.
    liveness {
      tcp_socket {
        port = 5432
      }

      # Postgres takes ~5-10s on boot to bind the port. 20s
      # initial delay covers slow disks too.
      initial_delay     = "20s"
      period            = "10s"
      failure_threshold = 3
    }

    # Readiness: pg_isready is the canonical signal. Standby
    # nodes mid-pg_basebackup or primary mid-WAL-replay-on-
    # startup will fail this even with the port open — caddy
    # / consumer apps shouldn't route to them.
    readiness {
      exec {
        command = ["pg_isready", "-U", "postgres", "-d", "myapp"]
      }

      period            = "5s"
      failure_threshold = 1
      success_threshold = 2
    }
  }
}
