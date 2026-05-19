# probes/

Kubelet-style health probes on deployments and statefulsets.
Three independent probes per pod, each optional:

- **liveness** — failure threshold trips `docker restart`. For
  "the process is alive but deadlocked" detection.
- **readiness** — failure flips the pod's ready bit; caddy
  active-probe drops it from upstream rotation. For "still
  booting / draining / DB unreachable" gating.
- **startup** — short-lived. Pod is NOT ready until startup
  passes once. After that it self-stops and readiness takes
  over. For slow-boot apps (Rails, Java) where you want a
  generous boot window without affecting steady-state checks.

## Selector types

Each probe declares exactly one of:

| selector | shape | when to use |
|---|---|---|
| `http_get` | `path = "/healthz"`, `port = 8080`, optional `scheme` / `http_headers` | HTTP apps. Required if you want caddy ingress gating (see below). |
| `tcp_socket` | `port = 6379` | Raw-TCP daemons (redis, postgres). Open port = alive. |
| `exec` | `command = ["pg_isready", "-h", "localhost"]` | When a CLI inside the container is the canonical "ok" signal. |

## Tuning knobs (all optional, sensible defaults)

```hcl
liveness {
  http_get { path = "/healthz" port = 3000 }

  initial_delay     = "10s"   # grace period before first sample
  period            = "10s"   # how often (default 10s)
  timeout           = "1s"    # per-sample wall-clock cap
  failure_threshold = 3       # consecutive fails before action
  success_threshold = 1       # consecutive passes to recover (readiness only)
}
```

## Caddy ingress integration (M1.2)

When a deployment has an `http_get` readiness probe AND an
`ingress` block, the controller automatically points caddy's
active health check at the readiness path — same endpoint the
probe runner samples internally. Two independent gates, one
declaration.

You don't need to touch voodu-caddy or set `health_check` on
the deployment for this to work. The probe period also doubles
as caddy's check interval (override via `ingress { lb { interval = "..." } }`).

## Examples

| file | what it shows |
|---|---|
| [`deployment-http.hcl`](./deployment-http.hcl) | HTTP web app with liveness + readiness on `/healthz` + auto caddy ingress gating |
| [`statefulset-tcp.hcl`](./statefulset-tcp.hcl) | Redis statefulset with TCP liveness + `redis-cli ping` exec readiness |
| [`rails-with-startup.hcl`](./rails-with-startup.hcl) | Rails app with a generous startup probe (30 attempts × 1s) gating a tight steady-state readiness probe (every 5s) |
| [`postgres-pg-isready.hcl`](./postgres-pg-isready.hcl) | Postgres statefulset with `pg_isready` exec probe — canonical pattern for stateful DBs |

## Operator UX

`vd describe deployment <ref>` shows per-replica readiness:

```
readiness:
  api-web.a3f9   ready=true   phase=healthy
  api-web.b1c2   ready=false  startup=waiting  phase=unhealthy  reason="GET /ready → 502"
```

Caddy active-probe queries `GET /pods/<container-name>/ready`
on the controller, returning 200/503/404. Unready replicas are
removed from upstream rotation until they recover.
