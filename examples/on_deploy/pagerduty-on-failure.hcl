# Asymmetric webhook routing: PagerDuty pages on failure, Slack
# gets a low-urgency note on success.
#
# Why asymmetric:
#
#   A successful deploy is INFORMATIONAL. The operator may want a
#   record ("yep, api 1.4.2 went out at 14:22") but nobody needs
#   to wake up for it. A green rollout at 3am is not a 3am
#   problem.
#
#   A failed deploy is ACTIONABLE. The rolling restart aborted
#   mid-flight, the readiness probe never went green, the new
#   replicas are unhealthy and old replicas may have already been
#   torn down. Someone has to look at this NOW. That's what
#   PagerDuty is for.
#
#   Sending both to the same channel collapses two very different
#   signals into one stream. Either the on-call gets paged for
#   every green deploy (alert fatigue, page ignored, real failure
#   missed) OR successes drown out failures in a chatty channel
#   (failure scrolls off-screen during a busy deploy window). Two
#   channels = two urgencies = no false alarms.
#
# How it fires:
#
#   1. Rolling restart completes (or aborts). voodu picks the URL
#      based on the terminal status:
#        success → posts to spec.Success (Slack low-urgency)
#        failure → posts to spec.Failure (PagerDuty Events API v2)
#   2. Single POST per release. voodu never sends BOTH for the
#      same rollout — a release is either success or failure,
#      not both.
#   3. PagerDuty's Events API v2 expects a specific JSON shape
#      (event_action, payload.summary, payload.severity, etc.).
#      voodu's payload is generic-JSON — kind/scope/name/status
#      etc. — so you'll typically point `failure` at a small
#      transformer (Cloudflare Worker, Lambda, or PagerDuty's
#      own incoming integration) that reshapes voodu's payload
#      into PagerDuty's expected schema before forwarding.
#      The URL below is the raw Events API for illustration;
#      swap in your transformer endpoint in real use.
#
# Best-effort delivery still applies:
#
#   3 attempts, 1s/5s backoff between them, 10s per-attempt HTTP
#   timeout. If PagerDuty is unreachable for ~36s, voodu logs the
#   drop and gives up. The rollout outcome was already determined
#   on its own merits — webhook delivery never changes it.
#
# An `ingress` block is included so the example is realistic
# end-to-end: deployment + webhook + public routing in one file.
#
# Apply:
#
#   cd examples/on_deploy
#   vd apply -f pagerduty-on-failure.hcl

deployment "prod" "checkout" {
  image    = "ghcr.io/acme/checkout:2.7.0"
  replicas = 4

  ports = ["8080"]

  env = {
    NODE_ENV = "production"
  }

  on_deploy {
    # Low-urgency success channel — Slack #deploys-info or
    # similar. No @mentions, no paging. Operator skims the
    # channel for a record of what shipped.
    success = "https://hooks.slack.com/services/T00000000/B11111111/XXXXXXXXXXXXXXXXXXXXXXXX"

    # PagerDuty (or a transformer fronting PagerDuty's Events
    # API v2). Pages the on-call rotation. Only fires when a
    # rolling restart aborts — never for green deploys.
    failure = "https://events.pagerduty.com/v2/enqueue?token=REDACTED"
  }
}

ingress "prod" "checkout" {
  service = "checkout"
  host    = "checkout.example.com"

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
