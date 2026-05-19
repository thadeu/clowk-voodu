// Feature-complete stack: postgres + redis + app, with EVERY
// post-launch voodu feature wired up. Use this as a "what does
// a real production deployment look like in 2026" reference.
//
// What runs after `vd apply`:
//
//   data-pg.0        postgres statefulset pod (single primary)
//   data-cache.0     redis statefulset pod
//   prod-api.<hash>  app deployment replicas (3-15 autoscaled)
//   ingress          caddy routing api.acme.com → prod-api with TLS
//
// Features demonstrated (every shipped milestone):
//
//   M1.1 — liveness probes (per-pod restart on deadlock)
//   M1.2 — readiness + startup probes + auto caddy gating
//   M1.3 — same probes on the statefulset side
//   M2   — private registry credentials (ghcr.io PAT)
//   M5   — init container (db:migrate before main pod starts)
//   M6   — on_deploy webhooks (success Slack + failure PagerDuty
//          direct via headers, no transformer Lambda)
//   M7   — CPU-based autoscale (replaces fixed replicas)
//
// Setup before first apply:
//
//   PG_PASS=$(openssl rand -hex 16)
//
//   # Postgres reads POSTGRES_PASSWORD from its scope env file
//   vd config set -s data -n pg POSTGRES_PASSWORD=$PG_PASS
//
//   # Shared scope bucket the app inherits via env_from. Both
//   # web and any future workers in the same scope can env_from
//   # this single source of truth — no per-app duplication.
//   vd config set -s prod -n shared \
//     DATABASE_URL="postgres://postgres:$PG_PASS@pg-0.data:5432/myapp" \
//     REDIS_URL="redis://cache-0.data:6379/0" \
//     RAILS_ENV="production"
//
//   # Registry credentials, on_deploy URLs, and on_deploy secrets
//   # all from the shell env at apply time. None reach git.
//   export GHCR_USER="acme-deploy-bot"
//   export GHCR_TOKEN="ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
//   export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T.../B.../XXXX"
//   export PD_ROUTING_KEY="R0000000000000000000000000000000"
//
//   vd apply -f everything.hcl
//
// On first apply, the controller JIT-installs voodu-postgres
// and voodu-redis plugins from thadeu/voodu-{postgres,redis}.

// ---------------------------------------------------------------------------
// Registry — private image pull credentials
// ---------------------------------------------------------------------------
//
// Host-wide (not scoped). Once declared, every deployment on
// this host can pull from ghcr.io without further auth setup.

registry "ghcr" {
  url      = "ghcr.io"
  username = "${GHCR_USER}"
  token    = "${GHCR_TOKEN}"
}

// ---------------------------------------------------------------------------
// Postgres statefulset (via macro plugin)
// ---------------------------------------------------------------------------
//
// Single-primary postgres. Probes use tcp_socket liveness (cheap
// "is the port up?") + pg_isready exec readiness (the strict
// "actually serving queries" signal). Liveness restart, readiness
// gating per-pod.

postgres "data" "pg" {
  plugin { version = "0.5.0" }

  image = "postgres:16"

  probes {
    liveness {
      tcp_socket { port = 5432 }
      initial_delay     = "20s"
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

  resources {
    limits {
      cpu    = "2"
      memory = "4Gi"
    }
  }
}

// ---------------------------------------------------------------------------
// Redis statefulset (via macro plugin)
// ---------------------------------------------------------------------------

redis "data" "cache" {
  plugin { version = "0.11.0" }

  image = "redis:7"

  probes {
    liveness {
      tcp_socket { port = 6379 }
      period            = "10s"
      failure_threshold = 3
    }

    readiness {
      exec { command = ["redis-cli", "ping"] }
      period            = "5s"
      failure_threshold = 1
      success_threshold = 2
    }
  }

  resources {
    limits {
      cpu    = "1"
      memory = "1Gi"
    }
  }
}

// ---------------------------------------------------------------------------
// Application — `app` sugar = deployment + ingress in one block
// ---------------------------------------------------------------------------
//
// All the deployment-side features available on standalone
// `deployment` are also available here: init, probes, autoscale,
// on_deploy, resources, env_from. The `app` shape just adds the
// ingress fields (host, tls, location, lb) at the bottom — no
// feature loss.

app "prod" "api" {
  image = "ghcr.io/acme/api:1.4.2"

  ports = ["3000"]

  // DATABASE_URL / REDIS_URL come from the shared scope bucket
  // we vd-config-set above. Both web and any future workers
  // inherit from the same source — no per-resource duplication.
  env_from = ["prod/shared"]

  // ------ M5: init container ------------------------------------
  //
  // Migrations run BEFORE every replica's main container starts.
  // Idempotent (Rails tracks schema versions), so re-running on
  // every spawn is safe and catches out-of-band schema changes.
  init "migrate" {
    command = ["bin/rails", "db:migrate"]
    timeout = "10m"
  }

  // ------ M1.1/M1.2: all three probes ---------------------------
  //
  // Startup gates ingress routing until the app first comes
  // online — Rails boots can take 30-40s with bootsnap cold +
  // asset precompile lazy. After the gate opens, readiness
  // controls "in rotation?" and liveness controls "needs
  // restart?".
  probes {
    startup {
      http_get { path = "/health" port = 3000 }
      period            = "2s"
      failure_threshold = 30   // up to 60s boot window
      success_threshold = 1
    }

    liveness {
      http_get { path = "/health" port = 3000 }
      period            = "5s"
      failure_threshold = 3
    }

    readiness {
      http_get { path = "/ready" port = 3000 }
      period            = "5s"
      failure_threshold = 1
      success_threshold = 2
    }
  }

  // ------ M7: CPU-based autoscale -------------------------------
  //
  // Replaces fixed `replicas = N`. Mutex with replicas at parse
  // time — operator picks one. Autoscaler runs every 15s,
  // reading runtime CPU% from the same StatsCollector that
  // powers `vd stats`.
  autoscale {
    min        = 3
    max        = 15
    cpu_target = 60

    // Default cooldown_up (30s) is fine for HTTP. Generous
    // cooldown_down so we don't collapse capacity during a
    // campaign lull — see examples/autoscale/web-api-burst.hcl
    // for the rationale.
    cooldown_down = "10m"
  }

  // ------ M6: post-deploy webhooks ------------------------------
  //
  // Success → Slack (informational, low urgency).
  // Failure → PagerDuty Events API v2 direct (no transformer
  //           Lambda — headers carry the routing key).
  //
  // Both URLs/secrets come from shell env.
  on_deploy {
    success {
      url = "${SLACK_WEBHOOK_URL}"
    }

    failure {
      url    = "https://events.pagerduty.com/v2/enqueue"
      method = "POST"

      headers = {
        "Content-Type"  = "application/json"
        "X-Routing-Key" = "${PD_ROUTING_KEY}"
      }
    }
  }

  // ------ Resource caps -----------------------------------------
  resources {
    limits {
      cpu    = "1"
      memory = "1Gi"
    }
  }

  // ------ Ingress side ------------------------------------------
  //
  // host + tls are app-block-only fields. The deployment side
  // above is identical to a standalone `deployment` declaration
  // — the sugar just pairs it with an ingress here.
  //
  // The M1.2 bridge: caddy active health check automatically
  // hits the SAME endpoint as the readiness probe (/ready on
  // port 3000) at the readiness probe's period (5s). No need
  // to set health_check or lb { interval } separately.
  host = "api.acme.com"

  tls {
    email = "ops@acme.com"
  }
}
