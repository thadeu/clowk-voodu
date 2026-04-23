// Path-based routing with `location {}` blocks. Each location is a URI
// prefix the ingress responds to. Without any location block, the
// ingress matches every request for its host — the common case.
//
// All locations within a single ingress route to the SAME service. To
// send different paths to different services, declare separate
// ingresses sharing the host (see "versioned API" below). The /apply
// boundary rejects two ingresses pointing to the same host, UNLESS they
// declare distinct `location {}` blocks — one host, many paths, many
// services is a legal fan-out.
//
// `strip = false` (default) preserves the prefix so the backend sees
// the full URI. `strip = true` removes it before forwarding — useful
// when routing a generic image (static nginx, arbitrary upstream) that
// expects root-relative URIs.

// 1) Single-service site with multiple accepted prefixes. Same backend
// answers /api/v1 and /api/v2 during a rolling cutover — drop the old
// block once clients have migrated.
ingress "acme" "api-dual" {
  host = "api.example.com"

  location { path = "/api/v1" }
  location { path = "/api/v2" }

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}

// 2) Versioned API fan-out. Two distinct deployments behind one host,
// each owning its path. Caddy matches the most specific prefix first,
// so requests route deterministically regardless of declaration order.
deployment "acme" "api-v1" {
  image = "ghcr.io/acme/api-v1:latest"
}

deployment "acme" "api-v2" {
  image = "ghcr.io/acme/api-v2:latest"
}

ingress "acme" "api-v1" {
  host    = "api.example.com"
  service = "api-v1"

  location { path = "/api/v1" }

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}

ingress "acme" "api-v2" {
  host    = "api.example.com"
  service = "api-v2"

  location { path = "/api/v2" }

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}

// 3) Docs site mounted under a path, with strip. The container runs a
// generic nginx that serves from `/`; strip removes `/docs/voodu`
// before the request arrives, so nginx sees `/getting-started`
// instead of `/docs/voodu/getting-started` and doesn't need a basePath.
ingress "clowk" "voodu-docs" {
  host    = "clowk.in"
  service = "voodu-docs"

  location {
    path  = "/docs/voodu"
    strip = true
  }
}

// 4) Catch-all next to specific paths. Requests matching /api/* go to
// the api ingress (declared elsewhere); anything else falls into the
// landing page. Omit `location {}` entirely for the catch-all — it's
// equivalent to `location { path = "/" }` but less boilerplate.
ingress "clowk" "landing" {
  host    = "clowk.in"
  service = "landing"
}
