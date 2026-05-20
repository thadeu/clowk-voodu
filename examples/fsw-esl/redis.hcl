# Redis — replaces the compose `redis` service.
#
# The macro expands to a statefulset with default probes (TCP + redis-cli
# ping) that already cover what compose's redis-cli healthcheck does.
#
# Harness — rarely changes. Apply along with rabbitmq.hcl on first
# bootstrap; subsequent service deploys (api/adapter/jobs/...) won't
# touch this file.

redis "fsw" "redis" {
  image = "redis:8"

  resources {
    limits {
      cpu    = "1"
      memory = "512Mi"
    }
  }
}
