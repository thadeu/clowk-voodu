# RabbitMQ — replaces the compose `rabbitmq` service.
#
# Statefulset with per-pod persistent volume for /var/lib/rabbitmq.
# Survives `vd apply --prune` — only `docker volume rm` actually
# removes the data.
#
# Harness — rarely changes. Apply along with redis.hcl on first
# bootstrap.

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
