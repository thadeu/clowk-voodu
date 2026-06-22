# voodu

[![ci](https://github.com/thadeu/clowk-voodu/actions/workflows/ci.yml/badge.svg)](https://github.com/thadeu/clowk-voodu/actions/workflows/ci.yml)
[![docs](https://github.com/thadeu/clowk-voodu/actions/workflows/docs.yml/badge.svg)](https://github.com/thadeu/clowk-voodu/actions/workflows/docs.yml)
[![release](https://img.shields.io/github/v/release/thadeu/clowk-voodu?label=release&color=blue)](https://github.com/thadeu/clowk-voodu/releases)

> Self-hosted, commitless-deploy PaaS. One HCL file. One `voodu apply`. No git push, no bare repo, no plugin sprawl.

Voodu is a Heroku-shaped, Kubernetes-honest deploy tool you run on your own boxes. HCL manifests describe the running system — apps, ingress with TLS, stateful services with backup. `voodu apply` builds, ships, routes, swaps. The CLI streams the build context straight to the server over SSH — no commit required, no push, no bare repo.

**Full documentation, examples, and architecture at [voodu.clowk.in/docs](https://voodu.clowk.in/docs).**

## Table of contents

- [Install](#install)
- [Quick start](#quick-start)
- [What you get](#what-you-get)
- [Plugins](#plugins)
- [Examples](#examples)
- [Architecture](#architecture)
- [Development](#development)
- [License](#license)

---

## Install

```sh
curl -fsSL voodu.clowk.in/install | bash
```

Auto-detects mode by OS — **server** on Linux (CLI + `voodu-controller` + Docker + systemd unit + default plugins), **client** on macOS (CLI only, for laptops that deploy to remote servers). Force explicitly:

```sh
curl -fsSL voodu.clowk.in/install | bash -s -- --client
curl -fsSL voodu.clowk.in/install | bash -s -- --server
```

Env knobs:

| Var | Default | What it does |
|---|---|---|
| `VERSION` | latest release | pin a tag, e.g. `v0.9.6` |
| `VOODU_ROOT` | `/opt/voodu` | server state directory |
| `VOODU_HTTP_ADDR` | `127.0.0.1:8686` | controller HTTP bind |
| `VOODU_INSTALL_REPO` | `thadeu/clowk-voodu` | source repo (for forks) |
| `SKIP_DOCKER=1` | — | skip Docker install (server mode only) |
| `SKIP_PLUGINS=1` | — | skip default plugin install |

Re-running the installer upgrades both binaries and restarts the controller — idempotent.

## Quick start

```hcl
# voodu.hcl
app "prod" "api" {
  image    = "ghcr.io/me/api:v1.2"
  replicas = 2
  ports    = ["8080"]

  env = {
    PORT = "8080"
  }

  host = "api.example.com"

  tls {
    email = "ops@example.com"
  }
}
```

```sh
voodu remote add voodu ubuntu@your-host    # default remote (alias `voodu`)
voodu apply -f voodu.hcl
```

That's it. TLS auto-provisions via Let's Encrypt, replicas roll in behind the readiness probe, traffic reaches `api.example.com`. For staging / prod, add more remotes:

```sh
voodu remote add staging ubuntu@staging.host
voodu remote add prod    ubuntu@prod.host

voodu apply -f voodu.hcl -r staging
voodu apply -f voodu.hcl -r prod
```

Full first-deploy walkthrough: [voodu.clowk.in/docs/getting-started/first-deploy](https://voodu.clowk.in/docs/getting-started/first-deploy).

## What you get

- **Six verbs cover ~95% of day-to-day** — `apply`, `diff`, `logs`, `config`, `remote`, `plugins`.
- **HCL manifests** — `deployment`, `app`, `statefulset`, `ingress`, `job`, `cronjob`, `asset`, `registry`, plus macros for `postgres` and `redis`.
- **Probes drive ingress** — declare a `readiness` probe, Caddy gates upstream membership on it automatically.
- **Per-pod volumes survive prune** — statefulset data is yours until `docker volume rm`.
- **Autoscale** — CPU-based hysteresis with asymmetric cooldown.
- **Multi-target `on_deploy`** — Slack + PagerDuty + Datadog in parallel goroutines, independent retry budgets.
- **Build cache shared** — content-addressed tarball hashes; identical source skips the rebuild.
- **`vd diff --detailed-exitcode`** — terraform-style CI exit codes (0 = no change, 2 = changes pending).
- **Embedded etcd** — single binary, no external dependencies, no operator sprawl.

Full manifest reference: [voodu.clowk.in/docs/manifests/overview](https://voodu.clowk.in/docs/manifests/overview).

## Plugins

| Repo | Purpose |
|---|---|
| [`thadeu/voodu-caddy`](https://github.com/thadeu/voodu-caddy) | Ingress — Caddy Admin API, ACME, on-demand wildcard TLS |
| [`thadeu/voodu-postgres`](https://github.com/thadeu/voodu-postgres) | Postgres — streaming replication, `pg_promote`, backups with retention |
| [`thadeu/voodu-redis`](https://github.com/thadeu/voodu-redis) | Redis — single-instance or sentinel HA |
| [`thadeu/voodu-mongo`](https://github.com/thadeu/voodu-mongo) | MongoDB |

Install one:

```sh
voodu plugins:install thadeu/voodu-postgres
```

Default install seeds `voodu-caddy` automatically so a fresh box can serve `app` blocks immediately.

## Examples

Production-grade examples in [`examples/`](examples/):

- [`fsw-esl/`](examples/fsw-esl/) — full telephony stack (redis + rabbitmq + 5 Go services) translated from docker-compose. Per-service file split for independent deploys, multi-target `on_deploy`, autoscale tier.
- [`stack/`](examples/stack/) — postgres + redis + asset + app with TLS, the canonical "real app" shape.
- [`on_deploy/`](examples/on_deploy/) — Slack, PagerDuty, Telegram, multi-target fan-out webhooks.
- [`probes/`](examples/probes/) — kubelet-style liveness / readiness / startup on HTTP / TCP / exec.
- [`autoscale/`](examples/autoscale/) — worker + HTTP tier scaling profiles.
- [`build/`](examples/build/) — `build {}` patterns: auto-detect runtime, custom Dockerfile, Go monorepo, statefulset build-mode.
- [`statefulset/`](examples/statefulset/) — postgres cluster, redis with persistent volumes.
- [`multi-env/`](examples/multi-env/) — one manifest, many remotes (`-r staging` / `-r prod`).
- [`shared-scope/`](examples/shared-scope/) — multiple repos applying into the same scope with `--no-prune`.

## Architecture

```
your laptop                                  server
───────────                                  ──────
voodu apply -f voodu.hcl  ──ssh──▶  voodu-controller
  │                                          │
  │ (build-mode: tarball)                    └─ reconcile → docker
  └─ tar -czf -  ──ssh──▶  voodu receive-pack
                              └─ extract → docker build → tag → swap → run
```

Single binary per host (`voodu-controller`), embedded etcd, in-process reconciler, plugin subprocesses for macros and CLI verbs. HTTP `:8686` on the controller; CLI talks to it via SSH-forwarded local port.

Deep dive: [voodu.clowk.in/docs/architecture/overview](https://voodu.clowk.in/docs/architecture/overview).

## Development

```sh
make tidy          # download deps
make build         # build voodu + voodu-controller into bin/
make vet           # go vet
make test          # go test -race -coverprofile=coverage.out
./bin/voodu --version
```

Releases are cut by pushing a `v*` tag — GoReleaser builds cross-platform binaries and publishes them to the GitHub release. The docs site rebuilds automatically on release via the [`docs` workflow](.github/workflows/docs.yml), so `voodu.clowk.in/install` and the landing-page version pill stay in sync with the latest binary.

## License

AGPL-3.0-only — see [LICENSE](LICENSE).
