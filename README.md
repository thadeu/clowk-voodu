# voodu

> Self-hosted, git-push-style PaaS with first-class stateful services.

Voodu is the evolution of [Gokku](https://github.com/thadeu/gokku). It keeps
what works — deploys via `git push`, blue-green swaps, per-app env
management — and invests where Gokku is weak: Postgres, Mongo, and other
stateful services with backup, replica, and test-restore built in, without
requiring the plugin sprawl of a full Kubernetes stack.

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
voodu apps create prod           # creates dirs, bare repo, post-receive hook
```

From your laptop — declare the app with an HCL manifest:

```hcl
# voodu.hcl
deployment "api" {
  path     = "."
  replicas = 2
  ports    = ["8080"]

  env = {
    PORT = "8080"
  }

  health_check = "/healthz"
  restart      = "always"
}

ingress "api" {
  host = "api.example.com"
  port = 8080

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
```

`service` inside `ingress` defaults to the ingress name, so the
overwhelmingly common 1-to-1 shape (`deployment "api"` ↔ `ingress
"api"`) is declaration-only — no boilerplate `service = "api"` required.
Port defaults to 80 when the referenced service/deployment declares one.

Apply it:

```sh
voodu apply -f api -a prod
```

`voodu apply` is the single user-facing entrypoint. It parses the manifest,
pushes the current `HEAD` to the server when the deployment is build-mode
(no `image` field), POSTs the manifests to the controller, and reconciles
ingress/services in one shot.

For a deployment that already has a published image, drop `path` and set
`image = "ghcr.io/you/api:1.2.3"` — no `git push` happens, the controller
pulls from the registry.

More examples live in [`examples/`](examples/):

- [`fullstack/`](examples/fullstack/) — deployment + database + ingress
- [`ingress/profiles.hcl`](examples/ingress/profiles.hcl) — four TLS
  profiles (HTTP, Let's Encrypt, internal CA, on-demand wildcard)
- [`ingress/paths.hcl`](examples/ingress/paths.hcl) — path-based
  routing with `location {}` blocks

## Ingress routing

One host, many paths, one service:

```hcl
ingress "api" {
  host = "api.example.com"

  location { path = "/api/v1" }
  location { path = "/api/v2" }
}
```

One host, different services per path (classic versioned API):

```hcl
ingress "api-v1" {
  host    = "api.example.com"
  service = "api-v1"
  location { path = "/api/v1" }
}

ingress "api-v2" {
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
your laptop                            server
───────────                            ──────
voodu apply -f voodu.hcl  ──ssh──▶  voodu-controller
  │                                    │
  │  (build-mode only)                 ├─ reconcile ingress/services (etcd)
  └─ git push HEAD:main  ────────▶  bare repo
                                       │
                                       └─ post-receive hook
                                          └─ extract → build image
                                             → swap `current` symlink
                                             → run post_deploy hooks
                                             → recreate container
```

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
