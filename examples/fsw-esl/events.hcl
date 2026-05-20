# events — background AMQP consumer.
#
# Deploy independently:
#   vd apply -f infra/fsw/events.hcl -r prod
#
# No ports, no HTTP probes — pure consumer. Restart-on-failure is the
# health signal (default restart = unless-stopped).

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
