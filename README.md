# voodu

> Self-hosted, git-push-style PaaS with first-class stateful services.

Voodu is the evolution of [Gokku](https://github.com/thadeu/gokku). It keeps
what works — deploys via `git push`, blue-green swaps, per-app env
management — and invests where Gokku is weak: Postgres, Mongo, and other
stateful services with backup, replica, and test-restore built in, without
requiring the plugin sprawl of a full Kubernetes stack.

## Install

```sh
curl -fsSL https://clowk.in/install | bash
```

This drops the `voodu` and `voodu-controller` binaries into `/usr/local/bin`.
Pre-built releases for Linux and macOS (amd64/arm64) are published on the
[releases page](https://github.com/thadeu/clowk-voodu/releases).

## Quick start

On the server:

```sh
voodu setup                     # initialise /opt/voodu
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
  host    = "api.example.com"
  service = "api"
  port    = 8080

  tls {
    enabled  = true
    provider = "letsencrypt"
    email    = "ops@example.com"
  }
}
```

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

More examples live in [`examples/`](examples/) — a full deployment +
ingress pair in [`fullstack/`](examples/fullstack/) and four TLS profiles
(HTTP, Let's Encrypt, internal CA, on-demand wildcard) in
[`ingress/profiles.hcl`](examples/ingress/profiles.hcl).

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
