# Procfile — deploy a Heroku/Dokku app with zero HCL

A `Procfile` is the migration on-ramp: point `vd apply` at one and voodu reads
the `type: command` lines, generates the manifests, builds the image **once**,
and applies. No HCL to write before your first deploy.

```sh
vd apply -f Procfile
```

## What this directory shows

| file | what it is |
|---|---|
| [`Procfile`](./Procfile) | Four processes: `web` (puma), `worker` (sidekiq), `sync` (a bin script), and `release` (migrations). |
| [`.voodu/app.json`](./.voodu/app.json) | The project-link file: pins the scope (`ws`) and declares the `web` process's ingress (host + TLS) — the one thing a Procfile can't express. |

## How each line maps

| Procfile line | Generated resource |
|---|---|
| `web: …`, `worker: …`, `sync: …` | `deployment "ws" "<type>"` — long-running, `replicas = 1`, `restart = on-failure`, command shell-wrapped (`/bin/sh -c`), a `PORT` env + published port auto-assigned from `5000`. |
| `release: …` | `job "ws" "release"` — a one-shot that runs once per apply (migrations). |
| `ingress.web` in `app.json` | `ingress "ws" "web"` routing `app.example.com` → the `web` deployment, port defaulted from `web`'s assigned port. |

All four processes share one source tree, so they share **one runtime image**:
voodu builds the first process and retags that image for the rest. You'll see
six tags (`ws-web:latest`, `ws-web:<buildID>`, `ws-worker:…`, …) but a single
image ID — shared storage, not six copies.

## Scope identity

`app.json`'s `scope` keeps re-applies idempotent (same `(scope, name)` → voodu
reconciles in place instead of stacking duplicate pods). Commit `.voodu/app.json`
so the scope and routing stay stable across machines and CI. Without it, voodu
generates a random scope on first apply and writes the file for you; pass
`--app <name>` to pin one explicitly.

## Config & secrets

App-wide env (the Heroku `config:set` equivalent) is scope-level config, merged
into every process automatically:

```sh
vd config ws set RAILS_ENV=production
vd config ws set SECRET_KEY_BASE=…        # secrets live outside the manifest
```

## Graduate to HCL

When you outgrow the Procfile (per-process `replicas`, a `release {}` gate,
Postgres/Redis plugins), eject to HCL — no server contact, just scaffolding:

```sh
vd apply -f Procfile --eject     # writes .voodu/ws.voodu
vd apply -f ws                   # re-apply the generated HCL
```

See the docs: [Procfile reference](https://voodu.clowk.in/docs/manifests/procfile)
· [Migrate from Procfile](https://voodu.clowk.in/docs/reference/migrate-from-procfile)
