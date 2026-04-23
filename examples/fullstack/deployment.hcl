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

// `service` defaults to the ingress name when omitted, so the 1-to-1
// case (ingress "api" ↔ deployment "api") is pure declaration. Use
// `service = "other"` only when the ingress routes to a different app.
ingress "api" {
  host = "api.clowk.in"
  port = 8080

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@voodu.clowk.in"
  }
}
