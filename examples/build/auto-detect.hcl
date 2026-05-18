// The terse shape: no `image`, no `build {}`. Voodu's applyDefaults
// synthesises `build { context = "." }` and language handlers sniff
// the runtime from marker files (go.mod, Gemfile, package.json,
// pyproject.toml, ...) at the repo root.
//
// Use this when:
//
//   - the repo IS the app (no monorepo, no multi-service split)
//   - the runtime is one voodu auto-detects
//   - you don't need custom Dockerfile / build args
//
// On `vd apply`, the CLI tarballs the current directory and streams
// it to the controller via SSH. The controller picks a language
// handler, generates a Dockerfile if your repo doesn't have one,
// builds, tags `<scope>-<name>:latest`, and starts the container.

deployment "demo" "web" {
  replicas = 1
  ports    = ["3000"]

  env = {
    NODE_ENV = "production"
  }
}

ingress "demo" "web" {
  host = "demo.lvh.me"
  port = 3000

  // Declaring tls {} signals "I want TLS"; defaults fire:
  // enabled = true, provider = "letsencrypt". Override provider
  // here for dev/staging where you want voodu-issued self-signed
  // certs instead of contacting letsencrypt.
  tls {
    provider = "internal"
  }
}
