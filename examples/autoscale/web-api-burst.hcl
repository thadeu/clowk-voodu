# HTTP API tier with autoscale tuned for bursty traffic.
#
# Different shape from the worker example. Web tiers care
# about request latency (P95, P99), not throughput averaged
# over a minute, so the autoscale knobs lean conservative on
# capacity removal:
#
#   - Lower cpu_target (60 vs. 70). HTTP latency degrades
#     nonlinearly with CPU saturation — at 85% CPU your P99
#     is already in the seconds. We keep more headroom per
#     replica so request bursts have somewhere to go.
#
#   - Higher min (3 vs. 2). A web tier behind ingress needs
#     enough baseline replicas to absorb a single-replica
#     restart (rolling deploys, OOM kills) without falling
#     below quorum. min = 3 means you can lose one and still
#     serve traffic from two — important for any tier with
#     an SLA.
#
#   - Default cooldown_up (30s). Web bursts often have a few
#     seconds of warning (CDN cache miss wave, scheduled
#     campaign launch). 30s is fast enough — and a new
#     replica needs ~10-15s to boot and pass its readiness
#     probe anyway, so a tighter cooldown wouldn't actually
#     give you faster capacity.
#
#   - Generous cooldown_down (10m). This is the headline
#     difference from sidekiq-worker.hcl. HTTP traffic spikes
#     come in clusters: marketing campaign drives a 5-minute
#     burst, then a 90-second lull, then another burst as
#     mobile clients retry. If we collapse capacity during the
#     lull we'll be undersized and serving 503s when the next
#     wave hits. 10m holds the inflated fleet long enough to
#     ride out the entire campaign window.
#
# Apply:
#
#   cd examples/autoscale
#   vd config set -s prod -n shared DATABASE_URL=postgres://...
#   vd apply -f web-api-burst.hcl

deployment "prod" "api" {
  image = "ghcr.io/acme/api:1.4"

  ports = ["3000"]

  env = {
    RAILS_ENV           = "production"
    RAILS_LOG_TO_STDOUT = "1"
  }

  env_from = ["prod/shared"]

  autoscale {
    min = 3
    max = 15

    cpu_target = 60

    # cooldown_up omitted — 30s default is correct for HTTP.
    cooldown_down = "10m"
  }
}

# Paired ingress so the example is end-to-end. The web tier
# is useless without something routing traffic to it; pairing
# them here also documents the autoscale-aware deploy story:
# new replicas spun up by the autoscaler are auto-registered
# with caddy as upstream, no manual reconfiguration.
ingress "prod" "api" {
  service = "api"
  host    = "api.example.com"

  tls {
    email = "ops@example.com"
  }
}
