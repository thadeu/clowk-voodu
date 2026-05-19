# HTTP web app with liveness + readiness probes.
#
# What this shows:
#
#   1. Liveness on /healthz — if the app's request loop deadlocks
#      (Ruby GVL stuck, Go goroutine livelock), the docker restart
#      brings it back without operator intervention.
#
#   2. Readiness on /ready — separate endpoint that returns 503 while
#      the app is mid-shutdown (draining connections) or while a
#      dependency (DB, cache) is unreachable. Pod stays alive but
#      caddy stops routing traffic to it.
#
#   3. Auto caddy gating — the `ingress` block pairs with the readiness
#      probe automatically. Controller emits VOODU_INGRESS_HC_PATH=/ready
#      and VOODU_INGRESS_LB_INTERVAL=5s into voodu-caddy, which generates
#      a Caddyfile with `health_uri /ready` per upstream.
#
# Two independent gates from one declaration:
#   - controller probe loop → marks pod (un)ready in DeploymentStatus,
#     serves /pods/<name>/ready endpoint, drives `vd describe` rendering
#   - caddy active probe → removes unready upstreams from rotation
#
# Apply:
#
#   cd examples/probes
#   vd apply -f deployment-http.hcl

deployment "prod" "api" {
  image    = "ghcr.io/acme/api:1.4"
  replicas = 3

  ports = ["3000"]

  env = {
    RAILS_ENV = "production"
  }

  probes {
    liveness {
      http_get {
        path = "/healthz"
        port = 3000
      }

      # First sample 15s after boot. Most apps need ~5-10s to
      # bind the port; 15s is a safe margin.
      initial_delay = "15s"

      # Every 10s in steady state — frequent enough to catch a
      # deadlock fast, rare enough to not be a load source.
      period = "10s"

      # 3 consecutive fails (30s window) before docker restart.
      # Tolerates one-off GC pauses; gives up on real hangs.
      failure_threshold = 3
    }

    readiness {
      http_get {
        path = "/ready"
        port = 3000
      }

      # Tighter cadence than liveness — readiness fluctuates more
      # (a brief DB disconnect should drop the pod from routing
      # quickly but NOT trigger a restart).
      period = "5s"

      # Single failure flips the gate. The app's /ready endpoint
      # is the canonical "ready to serve traffic" signal — no
      # tolerance band needed.
      failure_threshold = 1

      # Two passes to come back. Avoids flapping during a brief
      # recovery window.
      success_threshold = 2
    }
  }
}

# Ingress pairs with the readiness probe automatically. No
# `health_check = "..."` needed on the deployment, no
# `lb { interval = "..." }` needed here — the controller derives
# both from probes.readiness.
ingress "prod" "api" {
  service = "api"
  host    = "api.example.com"

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
