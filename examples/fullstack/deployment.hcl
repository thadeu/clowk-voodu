deployment "api" {
  image    = "ghcr.io/clowk/api:${VERSION:-latest}"
  replicas = 2

  env = {
    PORT     = "8080"
    DATABASE = "postgres://main:5432/app"
  }

  ports = ["8080"]

  restart      = "always"
  health_check = "/healthz"
}

ingress "api" {
  # hosts = []
  host    = "api.clowk.in"
  service = "api"
  port    = 8080

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@voodu.clowk.in"
  }
}
