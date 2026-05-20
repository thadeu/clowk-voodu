# api — stateless HTTP service (TTS + FSW API).
#
# Deploy independently:
#   vd apply -f infra/fsw/api.hcl -r prod
#
# Autoscaled on CPU. Independent of every other service file.

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
