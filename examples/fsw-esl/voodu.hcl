# fsw-esl: FreeSWITCH event-socket stack (telephony/VoIP)
#
# Direct translation of the docker-compose-prod.yml that runs this
# stack today. Every compose service maps to one voodu resource;
# every compose volume / network / healthcheck has a voodu equivalent
# that's either stricter (probes that gate ingress, not just restart),
# more durable (per-pod volume_claims that survive prune), or more
# expressive (autoscale + on_deploy multi-target webhooks).
#
# What voodu adds beyond `docker compose up`:
#
#   1. redis macro expansion: statefulset + entrypoint wrapper + default
#      probes (TCP + redis-cli ping) + REDIS_URL emission via `vd redis:link`.
#
#   2. rabbitmq as statefulset with per-pod volume claim — data survives
#      `vd apply --prune`. Compose's named volume only survives
#      `docker compose down`.
#
#   3. controller as deployment with replicas=1 — ESL connections from
#      FreeSWITCH break on rollout regardless of kind, so the simpler
#      shape wins. FreeSWITCH points at `controller.fsw.voodu:9090` and
#      reconnects after the rolling restart settles. Switch to statefulset
#      if you need persistent on-disk state or multi-replica sticky
#      routing.
#
#   4. autoscale on the stateless tier (api/adapter/events/jobs): CPU-based
#      hysteresis, asymmetric cooldowns (fast up, slow down). Compose has
#      no native scaling.
#
#   5. Shared config bucket (fsw/shared): set once with `vd config fsw/shared
#      set REDIS_ADDR=... RABBITMQ_URL=...`, every service picks it up via
#      `env_from`. Replaces the YAML-anchor + env_file dance, and rotates
#      via `vd config set` without re-applying HCL.
#
#   6. on_deploy multi-target: controller rollouts notify Slack AND
#      PagerDuty in parallel, with independent retry budgets.
#
#   7. Build-cache sharing: every service uses the same `./apps/esl`
#      context with a different SERVICE build-arg. voodu hashes the
#      tarball — identical context bytes mean identical build ID and
#      shared layer cache across services.
#
# Setup before first apply:
#
#   # 1. Set the shared config bucket once (replaces compose's YAML anchor
#   #    + env_file). Service names use voodu0 DNS aliases.
#   vd config fsw/shared set \
#     REDIS_ADDR="redis.fsw.voodu:6379" \
#     RABBITMQ_URL="amqp://guest:guest@rabbitmq-0.fsw.voodu:5672/" \
#     DIAL_ADAPTER_URL="http://adapter.fsw.voodu:8080" \
#     FSW_API_BASE_URL="http://api.fsw.voodu:9092" \
#     TTS_SERVICE_URL="http://api.fsw.voodu:9092" \
#     WEBSERVICE_URL="http://host.docker.internal:9099" \
#     ADAPTER_LISTEN_ADDR="0.0.0.0:8080" \
#     HTTP_ROUTER_LISTEN_ADDR="0.0.0.0:8080" \
#     TTS_LISTEN_ADDR="0.0.0.0:9092" \
#     CONTROLLER_ESL_LISTEN_ADDR="0.0.0.0:9090" \
#     CONTROLLER_API_LISTEN_ADDR="0.0.0.0:9091" \
#     ESL_LISTEN_ADDR="0.0.0.0:9090" \
#     CONTROLLER_ESL_SOCKET_ADDR="controller.fsw.voodu:9090" \
#     ESL_INBOUND_ADDR="fsw.voodu:8021" \
#     FSW_RECORDINGS_BASE_DIR="/var/lib/fsw/recordings"
#
#   # 2. Set notification webhooks (used by on_deploy on the controller).
#   vd config fsw/shared set \
#     SLACK_WEBHOOK_URL="https://hooks.slack.com/services/T../B../..." \
#     PD_ROUTING_KEY="R000..."
#
#   # 3. Apply manifests.
#   vd apply -f voodu.hcl
#
# The `vd redis:link` step is optional here — the Go code reads
# REDIS_ADDR (host:port), not the REDIS_URL the link command emits.
# If you refactor to consume REDIS_URL, add:
#   vd redis:link fsw/redis fsw/api
#   vd redis:link fsw/redis fsw/adapter
#   vd redis:link fsw/redis fsw/controller
#   vd redis:link fsw/redis fsw/events
#   vd redis:link fsw/redis fsw/jobs

# =====================================================================
# Infrastructure tier
# =====================================================================

# Redis — replaces the compose `redis` service.
# The macro expands to a statefulset with default probes (TCP + redis-cli
# ping) that already cover what compose's redis-cli healthcheck does.
redis "fsw" "redis" {
  image = "redis:8"

  resources {
    limits {
      cpu    = "1"
      memory = "512Mi"
    }
  }
}

# RabbitMQ — replaces the compose `rabbitmq` service.
# Statefulset with per-pod persistent volume for /var/lib/rabbitmq.
statefulset "fsw" "rabbitmq" {
  image    = "rabbitmq:3-management"
  replicas = 1
  ports    = ["5672", "15672"]

  env = {
    # Rotate in production via:
    #   vd config fsw/rabbitmq set RABBITMQ_DEFAULT_USER=... RABBITMQ_DEFAULT_PASS=...
    # The literal values here are dev defaults — config bucket overrides win at runtime.
    RABBITMQ_DEFAULT_USER = "guest"
    RABBITMQ_DEFAULT_PASS = "guest"
  }

  volume_claim "data" {
    mount_path = "/var/lib/rabbitmq"
  }

  probes {
    startup {
      tcp_socket { port = 5672 }
      period            = "5s"
      failure_threshold = 30
    }

    liveness {
      exec { command = ["rabbitmq-diagnostics", "-q", "ping"] }
      period            = "10s"
      timeout           = "5s"
      failure_threshold = 3
    }

    readiness {
      exec { command = ["rabbitmq-diagnostics", "-q", "check_running"] }
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

# =====================================================================
# Application tier
# =====================================================================

# Every service below uses the same `./apps/esl` context with a different
# SERVICE build-arg. voodu's content-addressed tarball cache shares the
# build across services — the first deployment builds, the rest reuse
# the image layers.

# ---- api ----
# Stateless HTTP service (TTS + FSW API). Public-facing within the
# voodu0 network. Autoscaled on CPU.
deployment "fsw" "api" {
  build {
    context    = "./apps/esl"
    dockerfile = "Dockerfile"
    args = {
      SERVICE = "api"
    }
  }

  env_from = ["fsw/shared"]
  ports    = ["9092"]

  autoscale {
    min        = 2
    max        = 4
    cpu_target = 60
  }

  probes {
    readiness {
      http_get {
        path = "/healthz"
        port = 9092
      }
      period            = "5s"
      success_threshold = 2
    }

    liveness {
      http_get {
        path = "/healthz"
        port = 9092
      }
      period            = "10s"
      failure_threshold = 3
    }
  }

  # Optional: if api serves recording playback (TTS_PLAYBACK_BASE_URL),
  # mount the same host path as jobs read-only:
  # volumes = ["/opt/voodu/volumes/fsw/recordings:/var/lib/fsw/recordings:ro"]

  resources {
    limits {
      cpu    = "2"
      memory = "1Gi"
    }
  }
}

# ---- adapter ----
# HTTP router / dial adapter. Stateless, scaled on CPU. Waits for redis
# at boot — compose's `depends_on { redis: { condition: service_healthy } }`
# in HCL form.
deployment "fsw" "adapter" {
  build {
    context    = "./apps/esl"
    dockerfile = "Dockerfile"
    args = {
      SERVICE = "adapter"
    }
  }

  env_from = ["fsw/shared"]
  ports    = ["8080"]

  init "wait-redis" {
    image   = "redis:8"
    command = ["sh", "-c", "until redis-cli -h redis.fsw.voodu -p 6379 ping; do echo 'waiting redis'; sleep 1; done"]
    timeout = "60s"
  }

  autoscale {
    min        = 2
    max        = 6
    cpu_target = 60
  }

  probes {
    readiness {
      http_get {
        path = "/healthz"
        port = 8080
      }
      period            = "5s"
      success_threshold = 2
    }

    liveness {
      http_get {
        path = "/healthz"
        port = 8080
      }
      period            = "10s"
      failure_threshold = 3
    }
  }

  resources {
    limits {
      cpu    = "1"
      memory = "512Mi"
    }
  }
}

# ---- controller ----
# ESL controller. Deployment with replicas=1 — the single-instance
# shape from compose. ESL TCP connections from FreeSWITCH break on
# rollout regardless of kind (statefulset rolling restart and deployment
# rolling restart both terminate the existing container). FreeSWITCH
# points at the round-robin alias `controller.fsw.voodu:9090`, which
# resolves to whichever replica is live.
#
# Switch to statefulset later only if you need (a) persistent on-disk
# state on the controller, or (b) multi-replica sticky-ordinal routing
# from FreeSWITCH.
#
# Production-critical: declares both `init` (wait for every dependency)
# AND `on_deploy` with multi-target Slack + PagerDuty fan-out.
deployment "fsw" "controller" {
  build {
    context    = "./apps/esl"
    dockerfile = "Dockerfile"
    args = {
      SERVICE = "controller"
    }
  }

  env_from = ["fsw/shared"]
  ports    = ["9090", "9091"]
  replicas = 1

  init "wait-deps" {
    image   = "alpine:latest"
    command = ["sh", "-c", <<-EOT
      set -eu
      apk add --no-cache redis netcat-openbsd > /dev/null
      echo "waiting for redis..."
      until redis-cli -h redis.fsw.voodu -p 6379 ping > /dev/null 2>&1; do sleep 1; done
      echo "waiting for rabbitmq..."
      until nc -z rabbitmq-0.fsw.voodu 5672; do sleep 1; done
      echo "waiting for api..."
      until nc -z api.fsw.voodu 9092; do sleep 1; done
      echo "waiting for adapter..."
      until nc -z adapter.fsw.voodu 8080; do sleep 1; done
      echo "deps ready"
    EOT
    ]
    timeout = "120s"
  }

  probes {
    startup {
      tcp_socket { port = 9091 }
      period            = "2s"
      failure_threshold = 30
    }

    readiness {
      http_get {
        path = "/healthz"
        port = 9091
      }
      period            = "5s"
      success_threshold = 2
    }

    liveness {
      http_get {
        path = "/healthz"
        port = 9091
      }
      period            = "10s"
      failure_threshold = 3
    }
  }

  # Controller is mission-critical — fan out failure to BOTH Slack
  # and PagerDuty. Each target fires in its own goroutine with its
  # own 3-attempt retry budget; a slow PagerDuty doesn't delay Slack.
  on_deploy {
    success {
      url = "${SLACK_WEBHOOK_URL}"
      body = {
        text = ":white_check_mark: controller deployed: {{release_id}} ({{image}})"
      }
    }

    failure {
      url = "${SLACK_WEBHOOK_URL}"
      body = {
        text = ":rotating_light: controller deploy failed: {{error}}"
      }
    }

    failure {
      url     = "https://events.pagerduty.com/v2/enqueue"
      headers = {
        "X-Routing-Key" = "${PD_ROUTING_KEY}"
      }
      body = {
        routing_key  = "${PD_ROUTING_KEY}"
        event_action = "trigger"
        payload = {
          summary  = "voodu controller deploy failed: {{error}}"
          source   = "fsw/{{name}}"
          severity = "critical"
          custom_details = {
            release_id = "{{release_id}}"
            image      = "{{image}}"
          }
        }
      }
    }
  }

  resources {
    limits {
      cpu    = "2"
      memory = "1Gi"
    }
  }
}

# ---- events ----
# Background AMQP consumer. No ports, no HTTP probes — pure consumer.
# Restart-on-failure is the health signal (default restart = unless-stopped).
deployment "fsw" "events" {
  build {
    context    = "./apps/esl"
    dockerfile = "Dockerfile"
    args = {
      SERVICE = "events"
    }
  }

  env_from = ["fsw/shared"]

  init "wait-rabbitmq" {
    image   = "alpine:latest"
    command = ["sh", "-c", "apk add -q netcat-openbsd && until nc -z rabbitmq-0.fsw.voodu 5672; do sleep 1; done"]
    timeout = "60s"
  }

  autoscale {
    min        = 1
    max        = 4
    cpu_target = 70
  }

  resources {
    limits {
      cpu    = "1"
      memory = "512Mi"
    }
  }
}

# ---- jobs ----
# Background jobs runner. Writes call recordings to a host bind-mount;
# the api service mounts the same path read-only for playback.
deployment "fsw" "jobs" {
  build {
    context    = "./apps/esl"
    dockerfile = "Dockerfile"
    args = {
      SERVICE = "jobs"
    }
  }

  env_from = ["fsw/shared"]

  # Compose's ${ESL_RECORDINGS_DIR:-/tmp/lib/fsw/recordings} → host-side path.
  # Mount under /opt/voodu/volumes so it shares the platform's volume tree.
  volumes = ["/opt/voodu/volumes/fsw/recordings:/var/lib/fsw/recordings:rw"]

  init "wait-deps" {
    image   = "alpine:latest"
    command = ["sh", "-c", <<-EOT
      set -eu
      apk add --no-cache redis netcat-openbsd > /dev/null
      until redis-cli -h redis.fsw.voodu -p 6379 ping > /dev/null 2>&1; do sleep 1; done
      until nc -z rabbitmq-0.fsw.voodu 5672; do sleep 1; done
    EOT
    ]
    timeout = "90s"
  }

  autoscale {
    min        = 2
    max        = 10
    cpu_target = 70
  }

  resources {
    limits {
      cpu    = "2"
      memory = "1Gi"
    }
  }
}

# =====================================================================
# Notes / what's not in this manifest yet
# =====================================================================
#
# FreeSWITCH itself — assumed to live on another voodu manifest (or
# another host entirely) reachable at `fsw.voodu:8021` (the ESL_INBOUND_ADDR
# env). Adding it would be another statefulset with RTP/SIP UDP ports +
# substantial resource limits. Cross-host reachability would require the
# voodu0 network to be bridged (or use a real DNS / overlay network).
#
# Public ingress — none of these services need public TLS. If you front
# api or adapter with a public hostname, swap the `deployment` for `app`
# and add:
#
#   host = "api.example.com"
#   tls  { email = "ops@example.com" }
#
# voodu-caddy will provision a Let's Encrypt cert and route traffic via
# the readiness probe.
#
# AWS S3 (commented out in compose) — when you wire S3, create a separate
# bucket for credentials:
#
#   vd config aws/cli set AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_REGION=us-east-1
#   vd config aws/recordings set JOBS_RECORD_S3_BUCKET=... JOBS_RECORD_S3_REGION=...
#
# Then on the jobs deployment:
#
#   env_from = ["fsw/shared", "aws/cli", "aws/recordings"]
