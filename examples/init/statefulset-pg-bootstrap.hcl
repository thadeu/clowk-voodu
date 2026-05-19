# Postgres statefulset with init container running pg_basebackup
# against the primary on first boot.
#
# The pattern: pg-0 is the primary (operator initialises it
# manually or via a one-shot job). pg-1, pg-2, ... are streaming
# replicas. Each replica needs a baseline copy of the primary's
# data directory before postgres can start in standby mode.
#
# Init container approach:
#
#   - Mounted volume `data` is per-ordinal — pg-1's data volume
#     is independent of pg-2's.
#   - On first boot, the data volume is empty. pg_basebackup
#     bootstraps from pg-0.
#   - On subsequent boots (restart, rolling update), the data
#     volume already has PGDATA, so the init container's check
#     short-circuits ("data exists, skip").
#
# Alternative would be a custom entrypoint wrapper, but that
# couples bootstrap logic into the image. Init container keeps
# the image stock (postgres:16) and the orchestration in HCL.
#
# Apply:
#
#   cd examples/init-containers
#   vd config set -s data POSTGRES_PASSWORD=...
#   vd apply -f statefulset-pg-bootstrap.hcl

statefulset "data" "pg" {
  image    = "postgres:16"
  replicas = 3

  ports = ["5432"]

  env = {
    POSTGRES_DB       = "myapp"
    POSTGRES_USER     = "postgres"
    PGDATA            = "/var/lib/postgresql/data/pgdata"
  }

  volume_claim "data" {
    mount_path = "/var/lib/postgresql/data"
    size       = "20Gi"
  }

  # Bootstrap step. The pod-0 case is special: it's the primary,
  # PGDATA must be empty AND the init must NOT run pg_basebackup
  # (there's no primary to copy from yet). Real production
  # setups gate this on the ordinal — voodu emits
  # VOODU_REPLICA_ORDINAL into the pod env so the script can
  # check it.
  init "bootstrap" {
    image = "postgres:16"
    command = [
      "sh", "-c",
      # If PGDATA is non-empty, we've already bootstrapped — skip.
      # If ordinal=0, this IS the primary; init the cluster locally.
      # Otherwise, pg_basebackup from pg-0.
      <<-EOT
        set -e
        if [ -s "$PGDATA/PG_VERSION" ]; then
          echo "PGDATA already initialised, skipping bootstrap"
          exit 0
        fi
        if [ "$VOODU_REPLICA_ORDINAL" = "0" ]; then
          echo "ordinal 0 = primary, initdb"
          initdb -U postgres -D "$PGDATA"
        else
          echo "ordinal $VOODU_REPLICA_ORDINAL = replica, pg_basebackup from pg-0"
          PGPASSWORD="$POSTGRES_PASSWORD" pg_basebackup \
            -h pg-0.data -U postgres -D "$PGDATA" -X stream -P -R
        fi
      EOT
    ]

    # First-boot pg_basebackup on a multi-GB primary can take
    # 10-30 minutes. Generous timeout.
    timeout = "60m"

    # No retries — bootstrap failures need operator
    # investigation, not silent re-attempts.
    retries = 0
  }

  probes {
    liveness {
      tcp_socket { port = 5432 }
      initial_delay     = "30s"
      period            = "10s"
      failure_threshold = 3
    }

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
