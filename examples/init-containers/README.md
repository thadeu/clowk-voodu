# init-containers/

Ordered one-shot containers that must exit 0 before a replica's
main container starts. Kubelet-style — each runs sequentially,
inherits the parent's env / volumes / networks / extra_hosts /
cap_add, re-runs on every fresh replica spawn (scale-up, rolling
restart).

## What init containers solve

Three classes of problems:

| problem | example | why init container |
|---|---|---|
| **DB migrations** | `bin/rails db:migrate` | Must complete before main pods serve traffic. Running it on every pod is wasteful (one migration, N pods) — but on a single pod is racy across scale events. Init container scoped to one ordinal solves both. |
| **State bootstrap** | Download fixtures, warm a cache, sync from S3 | Main process can assume the shared volume already has the data. No "is it ready yet?" polling in the app. |
| **Schema / config validation** | Run a dry-run config parse, validate env | Fails the pod loudly at boot if the operator misconfigured something, instead of crash-looping the main process at runtime. |

## What init containers are NOT

- **Not a substitute for startup probes** — once the main
  container starts, init containers are gone. If your app
  needs to delay traffic for 30s while it warms up, that's a
  startup probe job (see `../probes/rails-with-startup.hcl`).

- **Not for shared-fate steady-state work** — they're one-shot.
  For "always-running sidecar", we don't have that surface yet.

- **Not for cross-replica work** — each replica re-runs every
  init. A migration step is naturally idempotent (Rails tracks
  schema versions); for genuinely once-per-deployment work,
  use the `release { command = [...] }` block instead.

## Surface

```hcl
deployment "prod" "api" {
  image = "ghcr.io/acme/api:1.4"

  init_container "<name>" {
    image   = "ghcr.io/acme/api:1.4"       # defaults to parent image when omitted
    command = ["bin/rails", "db:migrate"]   # required
    timeout = "5m"                          # per-attempt cap (default 10m)
    retries = 2                             # extra attempts after first failure (capped at 5)

    resources {                             # optional, overrides parent's resources
      limits { cpu = "1" memory = "512Mi" }
    }
  }
}
```

Multiple `init_container "name" { … }` blocks declare a sequence.
Execution order matches declaration order. Names must be unique
within the resource (used as docker container name suffix).

## Inheritance

The init container shares EVERYTHING from the parent except:
- `image` (defaults to parent's, but can override)
- `command` (required, no inheritance)
- `timeout`, `retries`, `resources` (init-specific)

Inherited verbatim: env, env_file, env_from, networks,
volumes, extra_hosts, cap_add, network_mode. So an init can
write to a shared volume or talk to a sibling service exactly
the way the main container will.

## Failure semantics

- Each init runs in order. Failure of init[N] aborts the
  replica spawn; init[N+1] never runs.
- Per-attempt timeout via `timeout` (default 10m). Hard kill on
  expiry.
- Retries: `retries = N` gives 1+N total attempts. 2s linear
  backoff between attempts. Capped at 5 (chronic-failure-loop
  guard).
- Failed init container is LEFT IN PLACE (not auto-removed) so
  the operator can `docker logs <container-name>` post-mortem.
- Failure surfaces in `vd describe deployment <ref>`:

```
init failures (recent):
  a3f9   init=migrate   exit=1   attempts=3   2m   container exited 1: PG::Error
```

## Hash inclusion

Init container list (image, command, order) folds into the
deployment / statefulset spec hash. Editing an init step
triggers a top-down rolling restart so every replica re-runs
the updated init flow. Reordering inits is also a hash flip
(order is semantic — kubelet contract).

## Examples

| file | what it shows |
|---|---|
| [`rails-migrate.hcl`](./rails-migrate.hcl) | Single migration init with parent image inherit |
| [`multi-step.hcl`](./multi-step.hcl) | Three sequential inits (validate → migrate → warm-cache) with per-step resource override |
| [`statefulset-pg-bootstrap.hcl`](./statefulset-pg-bootstrap.hcl) | Statefulset with init that runs `pg_basebackup` against an existing primary on first boot |
