# controller — ESL controller. Deployment with replicas=1.
#
# ESL connections from FreeSWITCH break on rollout regardless of kind
# (statefulset rolling restart and deployment rolling restart both
# terminate the existing container). FreeSWITCH points at
# `controller.fsw.voodu:9090` and reconnects after the rolling restart
# settles. Switch to statefulset if you need persistent on-disk state
# or multi-replica sticky-ordinal routing.
#
# Production-critical: declares both `init` (wait for every dependency)
# AND `on_deploy` with multi-target Slack + PagerDuty fan-out. Each
# target fires in its own goroutine with its own 3-attempt retry budget.

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
