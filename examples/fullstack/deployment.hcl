// Scoped kinds — `deployment` and `ingress` — take two labels:
// <scope> and <name>. The scope is a free-form organizational tag
// (app, team, environment); it's the selector for prune and the
// uniqueness boundary (names are unique per-scope).
deployment "clowk" "api" {
  image    = "ghcr.io/clowk/api:${VERSION:-latest}"
  replicas = 2

  env = {
    PORT     = "8080"
    DATABASE = "postgres://main:5432/app"
  }

  ports = ["8080"]

  restart = "always"
  // health_check defaults to "/"; set it explicitly for a custom path.
  health_check = "/healthz"
}

// `service` defaults to the ingress name when omitted, so the 1-to-1
// case (ingress "api" ↔ deployment "api" in the same scope) is pure
// declaration. Use `service = "other"` only when the ingress routes to
// a different deployment in the same scope.
ingress "clowk" "api" {
  host = "api.clowk.in"
  port = 8080

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@voodu.clowk.in"
  }
}
