# Rails sidekiq worker with CPU-based autoscale.
#
# The canonical autoscale case: a queue consumer whose load
# tracks queue depth, not request rate. Workers are bursty by
# nature — CPU spikes hard while processing jobs, drops to near
# zero between drains. A fixed replica count either wastes
# capacity (sized for the burst) or backs up the queue (sized
# for the idle).
#
# Tuning rationale for each knob:
#
#   min = 2
#     Two baseline workers so a single host hiccup doesn't
#     drain the queue to zero throughput. For non-critical
#     queues you can drop to min = 1; for revenue-touching
#     jobs (payments, notifications) keep min >= 2.
#
#   max = 20
#     Hard ceiling. Twenty sidekiq processes with default
#     concurrency = 10 is 200 in-flight jobs — enough headroom
#     for spike drains without DoS'ing Postgres. Lower this if
#     the DB is the bottleneck.
#
#   cpu_target = 70
#     Worker pattern: keep mean CPU at 70% so each replica
#     has burst headroom for the next job batch. Higher than
#     the HTTP tier (which uses 60) because workers don't have
#     a latency SLA — a job that waits 200ms in the queue is
#     fine; an HTTP request that waits 200ms upstream is not.
#
#   cooldown_up = "15s"
#     Aggressive — half the default. Queue depth changes fast,
#     and a backed-up queue costs real money (delayed emails,
#     stale dashboards). 15s is enough for a new replica to
#     boot and start pulling jobs before we re-evaluate.
#
#   cooldown_down = "2m"
#     Tighter than the 5m default. Idle workers are cheap to
#     undo (they're not serving traffic, so no 503 risk), and
#     queue load tends to come in waves we want to track
#     closely. Don't go below 1m or you'll flap on every job
#     batch boundary.
#
# Apply:
#
#   cd examples/autoscale
#   vd config set -s prod -n shared DATABASE_URL=postgres://...
#   vd config set -s prod -n shared REDIS_URL=redis://...
#   vd apply -f sidekiq-worker.hcl

deployment "prod" "sidekiq" {
  image   = "ghcr.io/acme/api:1.4"
  command = ["bundle", "exec", "sidekiq"]

  env = {
    RAILS_ENV           = "production"
    RAILS_LOG_TO_STDOUT = "1"
  }

  # DATABASE_URL and REDIS_URL come from a shared scope bucket
  # the web tier reads too. Workers and web run the same image
  # against the same Postgres/Redis — config drift impossible.
  env_from = ["prod/shared"]

  autoscale {
    min = 2
    max = 20

    cpu_target = 70

    cooldown_up   = "15s"
    cooldown_down = "2m"
  }
}
