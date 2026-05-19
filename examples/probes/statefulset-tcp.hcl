# Redis statefulset with TCP-level liveness + exec-level readiness.
#
# What this shows:
#
#   1. tcp_socket liveness — Redis listens on 6379; if the socket
#      doesn't accept connections, the process is hung. Cheapest
#      possible probe, no auth issues, no command overhead.
#
#   2. exec readiness — "alive" (socket open) is weaker than "ready
#      to serve" for Redis under load (loading AOF / RDB on startup,
#      MASTER→REPLICA failover). `redis-cli ping` returns PONG only
#      when Redis is in a serving state.
#
#   3. Per-ordinal application — each pod (cache-0, cache-1, cache-2)
#      gets its own runner instances. cache-0 unhealthy doesn't
#      affect cache-1's readiness. Caddy / consumer apps gate per-pod.
#
# This pattern works as-is for Postgres (swap tcp port + use
# `pg_isready`), Memcached (only TCP, no command — skip exec), and
# most stateful workloads with a "ping me" CLI.
#
# Apply:
#
#   cd examples/probes
#   vd apply -f statefulset-tcp.hcl

statefulset "data" "cache" {
  image    = "redis:7"
  replicas = 3

  ports = ["6379"]

  command = ["redis-server", "--appendonly", "yes"]

  probes {
    # Liveness: TCP socket open = alive. Failure → docker restart
    # the ordinal. Per-pod volume survives the restart so AOF/RDB
    # data continues from where it was.
    liveness {
      tcp_socket {
        port = 6379
      }

      period            = "10s"
      failure_threshold = 3
    }

    # Readiness: actual round-trip PING. Catches the "loading AOF
    # at boot" state (TCP open, but commands return LOADING) that
    # the tcp_socket probe alone would miss.
    readiness {
      exec {
        command = ["redis-cli", "ping"]
      }

      period            = "5s"
      failure_threshold = 1
      success_threshold = 2
    }
  }
}
