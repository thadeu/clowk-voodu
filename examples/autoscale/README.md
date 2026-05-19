# autoscale/

CPU-based horizontal autoscaling on `deployment`. When the
`autoscale {}` block is present, the controller's autoscaler
owns the deployment's effective replica count — a periodic loop
samples mean CPU% across running replicas and adjusts replicas
by one within the declared `[min, max]` bounds.

## When to use it

- **Queue consumers** (sidekiq, resque, celery, bullmq, custom
  workers). Job throughput is bursty: CPU spikes when the
  queue fills, idles between drains. Autoscale lets you size
  for steady-state load and absorb spikes without paging.
- **HTTP API tiers** under variable traffic. Burst events
  (campaign launches, cron-driven client polls, geographic
  rush hours) overload a fixed replica count; autoscale
  inflates the fleet for the duration of the burst.

## When NOT to use it

- **Databases / stateful workloads.** Postgres, redis,
  elasticsearch — these run on `statefulset`, which doesn't
  expose `autoscale` because per-ordinal identity and
  persistent volumes can't be hot-scaled by replica count.
  Scaling a stateful tier is a deliberate, planned operation.
- **Workloads where CPU is NOT the bottleneck.** A deployment
  blocked on I/O (S3 uploads, slow upstream APIs) will sit at
  10% CPU regardless of queue depth. Autoscale won't help —
  the answer is more concurrency per replica, or a different
  metric entirely (which voodu doesn't yet support).

## Decision band — hysteresis

```
mean CPU > target * 1.1   →   scale up by 1
mean CPU < target * 0.7   →   scale down by 1
otherwise                 →   hold
```

The wide "hold" zone (between 70% and 110% of target) dampens
thrash. A target of 70 with naive thresholds would scale up at
71% and back down at 69%, churning replicas on every sample.
The asymmetric band gives genuine load room to register before
the autoscaler reacts.

Bands are **asymmetric on purpose**: a 10% over-target trigger
to scale up vs. a 30% under-target trigger to scale down.
Scale-up is cheap (an extra replica costs CPU/RAM the host
already has spare), scale-down causes 503s if traffic returns.
**Respond fast, retreat slowly.**

## Cooldowns

```
cooldown_up    = "30s"   # default — minimum interval between scale-up events
cooldown_down  = "5m"    # default — minimum interval between scale-down events
```

Same rationale as the band asymmetry: a tight up-cooldown gets
capacity online fast, a generous down-cooldown prevents
flapping after a burst subsides. A 30s burst that drove the
fleet from 3 to 8 replicas shouldn't collapse back to 3 ten
seconds later — the next burst may already be inbound.

For worker tiers where scale-down is safe (idle workers don't
serve traffic), tighten `cooldown_down` to "1m" or "2m". For
public HTTP tiers, leave it at the default or extend it.

## Mutex with `replicas`

A deployment declares **either** `replicas = N` **or** an
`autoscale {}` block. Not both. Parse-time error if you try.
Pick one — manual replica count or autoscaler-managed.

When autoscale is declared without an explicit replica count
elsewhere, the deployment boots with `min` replicas (not 1).

## Examples

| file | what it shows |
|---|---|
| [`sidekiq-worker.hcl`](./sidekiq-worker.hcl) | Rails sidekiq worker — canonical queue-consumer pattern with tight cooldowns and generous max |
| [`web-api-burst.hcl`](./web-api-burst.hcl) | HTTP API tier with ingress, conservative cooldown_down to ride out traffic lulls |
