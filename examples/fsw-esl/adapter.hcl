# adapter — HTTP router / dial adapter. Stateless, scaled on CPU.
#
# Deploy independently:
#   vd apply -f infra/fsw/adapter.hcl -r prod

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

  # Waits for redis at boot — compose's
  # `depends_on { redis: { condition: service_healthy } }` in HCL form.
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
