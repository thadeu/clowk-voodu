# Rails app with all three probes (liveness + readiness + startup).
#
# The startup probe is the headline feature here — Rails apps
# (especially with bootsnap cold cache, asset precompile lazy
# loaded, DB connection pool warm-up) routinely take 20-40s to
# accept their first real request. Without a startup probe you'd
# have to either:
#
#   (a) crank `initial_delay` on liveness/readiness to the worst-
#       case boot time → steady-state checks are also delayed
#       → real deadlocks take a minute+ to detect.
#
#   (b) accept that ingress routes traffic to a half-booted Rails
#       process for the first ~30s and serves 503s.
#
# Startup probe gives the best of both: a generous boot window
# (here, up to 60s) followed by tight steady-state probes (every
# 5s).
#
# How the gate works:
#
#   1. Container starts. ProbeRegistry spawns 3 runners: liveness,
#      readiness, startup.
#   2. Pod is NOT ready (StartupPassed=false). Caddy ingress
#      bypasses this replica.
#   3. Startup probe samples /healthz every 2s. After
#      success_threshold=1 pass, runner self-stops, gate opens.
#   4. From here on, readiness controls "in rotation?" and
#      liveness controls "needs restart?".
#
# Apply:
#
#   cd examples/probes
#   vd apply -f rails-with-startup.hcl

deployment "prod" "rails-web" {
  image    = "ghcr.io/acme/rails-web:2025-05-19"
  replicas = 4

  ports = ["3000"]

  env = {
    RAILS_ENV         = "production"
    RAILS_LOG_TO_STDOUT = "1"
  }

  probes {
    # Generous boot window: 30 attempts × 2s = 60s before we
    # declare the pod stuck and let liveness take over.
    startup {
      http_get {
        path = "/health"
        port = 3000
      }

      # No initial_delay needed — startup probe is the
      # boot-grace mechanism itself. First sample fires
      # immediately on container start.
      period = "2s"

      # 30 attempts is plenty for any Rails app. Slower-than-
      # this boots are bugs in the app, not config issues.
      failure_threshold = 30

      # First successful sample → gate opens.
      success_threshold = 1
    }

    # Tight steady-state liveness. Once startup passes, we
    # expect every sample to succeed; 3 consecutive fails
    # (15s) signals a real deadlock.
    liveness {
      http_get {
        path = "/healthz"
        port = 3000
      }

      period            = "5s"
      failure_threshold = 3
    }

    # Readiness: same endpoint different path. /ready returns
    # 503 if the DB connection pool is exhausted or if the
    # app is in graceful shutdown — caddy stops routing
    # without restarting the pod.
    readiness {
      http_get {
        path = "/ready"
        port = 3000
      }

      period            = "5s"
      failure_threshold = 1
      success_threshold = 2
    }
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
