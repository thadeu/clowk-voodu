// Combining asset + deployment: a stateless app reads its
// runtime config from a file mounted via the asset kind. Same
// pattern works for any kind that takes `volumes`.
//
// Workflow:
//
//   1. operator writes config.json on their Mac
//   2. `vd apply -f with-deployment.hcl` reads the file, embeds
//      the bytes in the manifest, POSTs to the server
//   3. server materialises /opt/voodu/assets/clowk-lp/web-config/runtime
//   4. deployment handler interpolates
//      ${asset.clowk-lp.web-config.runtime} into the host path
//      and mounts it as a bind volume
//   5. container at /etc/web/config.json reads the file
//   6. operator edits config.json, re-applies — content hash
//      changes → spec hash changes → rolling restart picks up
//      the new file automatically
//
// The 4-segment ref `${asset.<scope>.<name>.<key>}` addresses a
// scoped asset explicitly. For an unscoped (global) asset, use
// the 3-segment form `${asset.<name>.<key>}` instead.

asset "clowk-lp" "web-config" {
  runtime = file("./web/config.json")
}

deployment "clowk-lp" "web" {
  image    = "ghcr.io/clowk/web:latest"
  replicas = 2

  ports = ["8080"]

  volumes = [
    "${asset.clowk-lp.web-config.runtime}:/etc/web/config.json:ro",
  ]

  env = {
    PORT        = "8080"
    CONFIG_FILE = "/etc/web/config.json"
  }

  restart      = "always"
  health_check = "/healthz"
}

ingress "clowk-lp" "web" {
  host = "web.example.com"
  port = 8080

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
