# registry/

Private-registry pull secrets. One `registry "<name>" { ... }` block per
hostname; voodu rewrites `~/.docker/config.json` on the host atomically
on every apply or delete of a registry resource so subsequent
`docker pull` calls authenticate without a manual `docker login`.

## What a registry block does

Each block declares one entry under the `auths` map of the host's
docker config file. The handler:

- Lists every `registry` manifest in the store, rebuilds the `auths`
  map from scratch (voodu owns this section entirely), writes to
  `~/.docker/config.json.tmp-*`, then `rename`s into place. Atomic on
  POSIX — concurrent `docker pull` never sees a half-written file.
- Other top-level keys in `config.json` (credsStore, HTTPHeaders,
  plugins) round-trip verbatim. Only `auths` is voodu-owned.
- On delete, regenerates the same way — the removed entry disappears,
  every remaining manifest stays in place.

## Identity is host-wide, not per-app

The block label IS the registry name; no scope segment.
`~/.docker/config.json` is a single global file, so two scopes
cannot both declare `registry "ghcr"`. The parser enforces
uniqueness via the standard (kind, name) duplicate-detection sweep.

Practical effect: once `registry "ghcr"` is applied, EVERY deployment
on the host — across every scope — can pull `ghcr.io/...` images.
You do NOT add a registry block per app.

## Required fields

From `internal/manifest/types.go` (`RegistrySpec`):

| field | meaning |
|---|---|
| `url` | The hostname as docker sees it (`ghcr.io`, `registry-1.docker.io`, `harbor.internal.acme.com`). No scheme. |
| `username` | Account name. Forwarded into the base64 `auth` field. |
| `token` | The secret half (PAT, service-account credential). `password = "..."` is accepted as an alias and decodes into the same field. |

## Never inline the secret

`${VAR}` interpolation is applied to the raw manifest bytes on the
CLI's machine BEFORE the manifest leaves for the controller. The
secret lives in the operator's shell env (sourced from `direnv`, a
`.envrc`, a vault helper — anywhere). The HCL only references
`${GHCR_TOKEN}`; the plaintext never enters the file.

Two things to remember:

1. `vd config set` buckets are **scoped** (`<scope>` or
   `<scope>/<name>`). The `registry` kind is **unscoped** — its
   secret does NOT belong in a config bucket. Use shell env.
2. Interpolation happens client-side. The controller never sees
   `${GHCR_TOKEN}` — it sees the substituted plaintext, which is
   then base64-encoded into the auth entry.

## Removing a registry

Delete the `registry "<name>" { }` block from the file and re-apply
with `--prune` so voodu reconciles the absence (or use
`vd delete -f <file>` to soft-delete the manifests listed in the
file). After the next reconcile, the entry disappears from
`~/.docker/config.json` and pulls against that registry start
returning 401 again.

## Examples

| file | what it shows |
|---|---|
| [`ghcr-private.hcl`](./ghcr-private.hcl) | GitHub Container Registry — one registry block + a deployment pulling from `ghcr.io/acme/private-api:1.0`. Secret comes from `${GHCR_TOKEN}` in the shell env. |
| [`multi-registry.hcl`](./multi-registry.hcl) | Two registries simultaneously (ghcr.io + self-hosted Harbor). Demonstrates that registries are host-wide — deployments in different scopes pull from different registries with one set of declarations. |
