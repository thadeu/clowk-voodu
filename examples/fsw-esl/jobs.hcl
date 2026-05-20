# jobs — background workers runner.
#
# Deploy independently:
#   vd apply -f infra/fsw/jobs.hcl -r prod
#
# Writes call recordings to a host bind-mount; the api service can
# mount the same path read-only for playback.

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
