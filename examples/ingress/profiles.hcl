// Four ingress profiles supported by voodu-caddy. Pick the block that
// matches your case; they can coexist in the same file.
//
// `service` is optional — if omitted, it defaults to the ingress name.
// These examples keep it explicit because ingress names (api_http,
// api_public, ...) do NOT match the service name "api" — i.e. this is
// cross-app routing by design. For the common 1-to-1 shape see
// examples/fullstack/deployment.hcl. For path-based routing and
// location {} blocks see examples/ingress/paths.hcl.
//
// Detailed semantics live in the plugin README:
//   https://github.com/thadeu/voodu-caddy#tls-profiles

// 1) HTTP only — no TLS block. Plain :80 proxy, upstream service:port.
// `port` may be omitted when the referenced service or deployment
// declares one; the controller resolves it before dispatch.
ingress "api_http" {
  host    = "api.internal"
  service = "api"
  port    = 3000
}

// 2) Public TLS via Let's Encrypt (ACME HTTP-01). Finite, known hosts.
// Does NOT support wildcards — HTTP-01 cannot validate *.example.com.
// Multiple ingresses sharing `email` reuse the same ACME account.
ingress "api_public" {
  host    = "api.clowk.in"
  service = "api"
  port    = 3000

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@clowk.in"
  }
}

// 3) Internal CA (Caddy self-signed). Dev/staging without a public
// domain — browsers warn until the CA is trusted.
ingress "api_internal" {
  host    = "api.dev.local"
  service = "api"
  port    = 3000

  tls {
    enabled  = true
    provider = "internal"
  }
}

// 4) On-demand TLS + ask callback. The only profile that accepts true
// wildcards. Cert is issued on the first HTTPS hit, gated by an HTTP
// endpoint on your app that returns 200 for legitimate tenants.
//
// `ask` is REQUIRED when on_demand = true. Without it the plugin would
// be an open cert-issuance proxy.
ingress "tenants_wildcard" {
  host    = "*.clowk.in"
  service = "app"
  port    = 3000

  tls {
    enabled   = true
    provider  = "letsencrypt"
    email     = "ssl@clowk.in"
    on_demand = true
    ask       = "http://app:3000/internal/allow_domain"
  }
}
