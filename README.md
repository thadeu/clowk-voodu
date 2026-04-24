# voodu

> Self-hosted, commitless-deploy PaaS with first-class stateful services.

Voodu is the evolution of [Gokku](https://github.com/thadeu/gokku). It keeps
what works — single `voodu apply` deploys, blue-green swaps, per-app env
management — and invests where Gokku is weak: Postgres, Mongo, and other
stateful services with backup, replica, and test-restore built in, without
requiring the plugin sprawl of a full Kubernetes stack.

Commitless by default: edit code, run `voodu apply`, done. The CLI
streams the build context straight to the server over SSH — no git
commit required, no push, no bare repo.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/thadeu/clowk-voodu/main/install | bash
```

On a Linux host this is a full **server install**: drops `voodu` and
`voodu-controller` into `/usr/local/bin`, seeds `/opt/voodu/`, installs
the `voodu-controller.service` systemd unit, and starts the daemon on
`127.0.0.1:8686`. On macOS the same line installs only the CLI
(**client mode**), for laptops that deploy to remote servers.

Force mode explicitly:

```sh
curl -fsSL ...install | bash -s -- --client
curl -fsSL ...install | bash -s -- --server
```

Useful env knobs:

| Var | Default | What it does |
|---|---|---|
| `VERSION` | latest release | pin a tag, e.g. `v0.1.0` |
| `VOODU_ROOT` | `/opt/voodu` | server state directory |
| `VOODU_HTTP_ADDR` | `127.0.0.1:8686` | controller HTTP bind |
| `VOODU_INSTALL_REPO` | `thadeu/clowk-voodu` | source repo (for forks) |

Pre-built releases for Linux and macOS (amd64/arm64) live on the
[releases page](https://github.com/thadeu/clowk-voodu/releases).
Re-running the installer upgrades both binaries and restarts the
controller — it is idempotent.

## Quick start

After installing in server mode, `/opt/voodu/` is already seeded and the
controller is running. Create your first app:

```sh
voodu apps create prod           # creates /opt/voodu/apps/prod + initial .env
```

From your laptop — declare the app with an HCL manifest:

```hcl
# voodu.hcl
deployment "prod" "api" {
  replicas = 2
  ports    = ["8080"]

  env = {
    PORT = "8080"
  }
}

ingress "prod" "api" {
  host = "api.example.com"

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
```

Scoped kinds (`deployment`, `ingress`) take **two labels**: `<scope>` and
`<name>`. Scope is a free-form organizational tag (app, team,
environment); it groups manifests, selects what prune touches, and is
the uniqueness boundary for names. `service` inside `ingress` defaults
to the ingress name, so the 1-to-1 shape (`deployment "prod" "api"` ↔
`ingress "prod" "api"`) is declaration-only. Path, port, and
health_check all have sensible defaults (`.`, the deployment's declared
port, `/`) — set them only when you need to override. Dockerfile has no
default: the lang handler picks `Dockerfile` if you ship one, else
auto-generates one for the detected runtime (Go/Ruby/Rails/Python/Node).

`path` is **CWD-relative**: the build context is whichever directory you
invoked `voodu apply` from, same as `docker build .`. If your manifest
lives in a subdir (`infra/dev.voodu`) and your Dockerfile is at the
project root, run `voodu apply -f infra/dev.voodu` from the project
root — the tarball mirrors your shell's current dir.

Apply it:

```sh
voodu apply -f voodu.hcl
```

`voodu apply` is the single user-facing entrypoint and the source of
truth: the invocation (one file, many `-f`, or a directory) is the
desired state. The controller diffs against etcd and **prunes per
(scope, kind)** automatically — no confirm, no prompt. Applying only
`deployments.hcl` won't touch ingresses in the same scope, so you can
decompose by kind without cross-kind deletion.

Pass `--no-prune` to upsert without deletions — see
[Shared scope across repos](#shared-scope-across-repos) for the
intended use case.

### File extensions

All of these are parsed as HCL — pick whichever reads best in your
editor and file tree:

| Extension | When it's nice |
|---|---|
| `.hcl` | Tooling compatibility (most editors / IDEs highlight this by default) |
| `.voodu` | Branded, reads like a first-class config (`web.voodu`, `api.voodu`) |
| `.vdu`, `.vd` | Shorter aliases for the same |
| `.yml`, `.yaml` | YAML variant — same schema, different syntax |

`voodu apply -f web` resolves bare names against all of the above in
order, so editing `web.voodu` and running `voodu apply -f web` just
works.

For a deployment that already has a published image, drop `path` and set
`image = "ghcr.io/you/api:1.2.3"` — no tarball gets streamed, the
controller pulls from the registry.

More examples live in [`examples/`](examples/):

- [`fullstack/`](examples/fullstack/) — deployment + database + ingress
- [`multi-env/app.voodu`](examples/multi-env/app.voodu) — one manifest,
  many servers (staging / prod-1 / prod-2 selected with `-r`)
- [`shared-scope/`](examples/shared-scope/) — one scope fanned out
  across independent repos with `--no-prune` upsert
- [`ingress/profiles.hcl`](examples/ingress/profiles.hcl) — four TLS
  profiles (HTTP, Let's Encrypt, internal CA, on-demand wildcard)
- [`ingress/paths.hcl`](examples/ingress/paths.hcl) — path-based
  routing with `location {}` blocks

## Remotes

A **remote** is just an SSH target — a `user@host` pair stored as a git
remote so every developer clone already knows where the app ships.
Voodu inherits the git-remote lookup so there's no extra config file.

```sh
# one-shot bootstrap of a fresh host (ssh preflight + install + server setup)
voodu remote setup staging ubuntu@staging.example.com --binary ./bin/voodu

# or just register a remote for an already-provisioned host
voodu remote add    prod-1 ubuntu@prod-1.example.com
voodu remote add    prod-2 ubuntu@prod-2.example.com
voodu remote list
```

The HCL manifest owns the app identity (`scope` + `name`). The remote
owns only the SSH destination. So **one server runs as many apps as the
HCL declares**, and the same manifest ships unchanged to any server —
only `-r` changes:

```sh
voodu apply -f voodu.hcl              # default: looks up the "voodu" git remote
voodu apply -f voodu.hcl -r staging   # ship to staging
voodu apply -f voodu.hcl -r prod-1    # ship to prod-1
```

`-r` is the shorthand for `--remote`. Omit both and voodu uses the git
remote named `voodu` — handy when a repo targets a single server and
you want `voodu apply` to "just work".

Three prod hosts behind an AWS ALB? Add `prod-1`, `prod-2`, `prod-3` and
loop: `for r in prod-1 prod-2 prod-3; do voodu apply -f voodu.hcl -r $r;
done`. The scope+name in the manifest stays constant across rollouts.

## Shared scope across repos

By default every `voodu apply` is a full source-of-truth statement for
the `(scope, kind)` pairs it touches — anything the controller knows
about in that pair that isn't in this apply gets pruned. That's the
right default for a single repo that owns its scope: rename a
deployment in HCL and the old one disappears, no zombies left behind.

The shape below is **different**. Four independent repos, one shared
scope, each applying only its own slice:

```hcl
# github.com/you/clowk
deployment "clowk" "app" { image = "ghcr.io/you/clowk:1" }

# github.com/you/clowk-landingpage
deployment "clowk" "lp"  { image = "ghcr.io/you/clowk-lp:1" }

# github.com/you/clowk-api
deployment "clowk" "api" { image = "ghcr.io/you/clowk-api:1" }

# github.com/you/clowk-jobs
deployment "clowk" "jobs" { image = "ghcr.io/you/clowk-jobs:1" }
```

With the default behavior, each `voodu apply` would delete the three
others' deployments. Use `--no-prune` to opt into upsert-only:

```sh
voodu apply -f voodu.hcl --no-prune
```

The flag lives in every CI pipeline that shares a scope, so the choice
is explicit and grep-able. The default elsewhere stays strict.

**When to reach for this vs. distinct scopes.** The cleaner shape is
usually one scope per repo (`clowk-app`, `clowk-lp`, `clowk-api`,
`clowk-jobs`) — ownership is obvious, `voodu list -s clowk-api` scopes
to one repo, and no pipeline needs a flag. Pick shared scope only when
grouping is a first-class concern (a logical environment you want to
query and config together) and every apply that touches the scope
passes `--no-prune`.

## Ingress routing

One host, many paths, one service:

```hcl
ingress "acme" "api" {
  host = "api.example.com"

  location { path = "/api/v1" }
  location { path = "/api/v2" }
}
```

One host, different services per path (classic versioned API). The
`/apply` boundary rejects two ingresses claiming the same host **unless**
they declare distinct `location {}` blocks — one host, many paths, many
services is legal fan-out:

```hcl
ingress "acme" "api-v1" {
  host    = "api.example.com"
  service = "api-v1"
  location { path = "/api/v1" }
}

ingress "acme" "api-v2" {
  host    = "api.example.com"
  service = "api-v2"
  location { path = "/api/v2" }
}
```

`strip = true` on a location removes the prefix before forwarding — use
it when routing a generic image (static nginx, arbitrary upstream) that
expects root-relative URIs:

```hcl
location {
  path  = "/docs/voodu"
  strip = true   # backend sees /getting-started, not /docs/voodu/getting-started
}
```

Omitting `location {}` entirely is the catch-all for a host.
Everything inside the app itself (404 pages, rewrites, SPA fallback,
compression) stays in your Dockerfile's web server — the platform
terminates at `host → container:port`.

## Previewing changes with `voodu diff`

`voodu diff` is the "what would apply do?" button. It calls the
controller with `?dry_run=true`, so nothing gets persisted and the
output reflects **exactly** what the next `voodu apply` with the same
flags would do — same prune logic, same validation, same ordering.

```sh
$ voodu diff -f voodu.hcl
~ deployment/clowk/web
    ~ image     "nginx:1.26"  →  "nginx:1.27"
    ~ replicas  1  →  2
    + lang.name  "bun"
= ingress/clowk/web (unchanged)

--- Would prune (pass --no-prune to keep) ---
- deployment/clowk/old-worker

1 to modify, 1 to prune
```

Markers:
- `~ kind/scope/name` — resource exists and its spec changed. Each
  line underneath is one JSON field that differs, dotted for nested
  keys (`tls.email`, `lang.name`).
- `+ kind/scope/name (new)` — resource would be created; field lines
  underneath are its initial spec.
- `= kind/scope/name (unchanged)` — spec matches the controller.
- `--- Would prune ---` — resources that would be removed by the
  source-of-truth apply contract. Use `--no-prune` to simulate an
  upsert-only apply (shared-scope case).

### CI-friendly exit codes

Pass `--detailed-exitcode` to get `terraform plan`-style exit codes:

| Exit code | Meaning |
|---|---|
| 0 | No changes |
| 1 | Error (couldn't reach controller, invalid manifest, …) |
| 2 | Plan has pending changes |

Lets you wire a `voodu diff --detailed-exitcode` step in CI that
fails a branch when it drifts from the declared state, or gates a
deploy step behind an explicit "yes there are changes" signal.

## Configuration

Per-app environment variables are managed out-of-band from the manifest
so secrets don't live in your repo:

```sh
voodu config set DATABASE_URL=postgres://... SECRET_KEY=... -a prod
voodu config list   -a prod
voodu config get    SECRET_KEY -a prod
voodu config unset  OLD_FLAG -a prod
voodu config reload -a prod      # recreate the active container
```

Env set via `config:set` always wins over `env {}` blocks in the manifest,
so a `voodu apply` can't accidentally reset a production secret.

## How it works

```
your laptop                                 server
───────────                                 ──────
voodu apply -f voodu.hcl  ──ssh──▶  voodu-controller
  │                                         │
  │                                         └─ reconcile ingress/services (etcd)
  │  (build-mode only: stream tarball)
  └─ tar -czf - <path>  ──ssh──▶  voodu receive-pack <scope>/<name>
                                             └─ extract → build image
                                                → swap `current` symlink
                                                → run post_deploy hooks
                                                → recreate container
```

Tarball transport is content-addressed: an identical tree produces the
same build-id (sha256 of the tar bytes) and the server skips the
rebuild, just repointing `current`. Use `VOODU_FORCE_REBUILD=1` (or
`voodu receive-pack --force` on the server) to bypass.

- **CLI (`voodu`)** — parses HCL, forwards commands over SSH or to the
  controller's HTTP API. Installed on laptops and servers both.
- **Controller (`voodu-controller`)** — long-running daemon backed by an
  embedded etcd. Owns manifest state, reconciles services, routes unknown
  commands to plugins.
- **Plugins** — independent binaries discovered from `/opt/voodu/plugins`.
  `voodu plugins:install <github-repo>` clones and wires them. See
  [voodu-caddy](https://github.com/thadeu/voodu-caddy) for an example.

## Plugins

| Repo | Purpose |
|---|---|
| [`thadeu/voodu-caddy`](https://github.com/thadeu/voodu-caddy) | Ingress (Caddy Admin API, ACME, on-demand wildcard TLS) |
| [`thadeu/voodu-postgres`](https://github.com/thadeu/voodu-postgres) | Postgres service with backup / replica / test-restore |
| [`thadeu/voodu-mongo`](https://github.com/thadeu/voodu-mongo) | MongoDB service |

Install one with:

```sh
voodu plugins:install thadeu/voodu-caddy
```

## Development

```sh
make tidy          # download deps
make build         # build voodu + voodu-controller into bin/
make check         # fmt + vet + lint + test
./bin/voodu --version
```

Releases are cut by pushing a `v*` tag — GoReleaser builds cross-platform
binaries and publishes them to the GitHub release.

## License

MIT — see [LICENSE](LICENSE).
