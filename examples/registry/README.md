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

1. `registry` does NOT accept `env_from`. Other kinds
   (`deployment`, `app`, `statefulset`, `job`, `cronjob`) feed
   their `${VAR}` interpolation from `env_from`'d config buckets;
   `registry` is the one exception. See the next section for
   why, and the recommended team workflow.
2. Interpolation happens client-side. The controller never sees
   `${GHCR_TOKEN}` — it sees the substituted plaintext, which is
   then base64-encoded into the auth entry.

## One credential per registry, per host

This is the constraint that shapes how teams should use the
`registry` kind. Read this section before adopting a per-dev
token workflow.

`~/.docker/config.json` is **a single file** on the host. Every
`vd apply` that includes a `registry "<name>" { ... }` block
rewrites the `auths.<url>` entry. The credential ACTIVE on the
host at any moment is whichever apply ran last.

What this means for teams:

- **Per-dev personal tokens don't compose.** If dev A has a PAT
  with access to org X repos and dev B has a PAT scoped to org Y,
  whichever applied last is the only credential the VM can use.
  Subsequent `docker pull` of the other org's images will 401
  until someone applies with a broader token.

- **The credential persists across reboots and reconciles.** Once
  written, `~/.docker/config.json` lives on disk. The autoscaler
  scaling up at 3am uses it. A container crash + reconcile uses
  it. No re-apply is needed for the controller to keep pulling —
  as long as the credential itself stays valid (not revoked, not
  expired).

- **The credential expires silently.** A revoked PAT (dev leaves
  the company, GitHub rotates the token) means the next pull
  fails. The VM has no way to refresh — it just keeps presenting
  the dead token until someone applies with a fresh one.

### Recommended team workflow

Use a **service-account / bot-account token** rather than personal
PATs. Common shape:

- GitHub: create a dedicated bot user (or a fine-grained PAT on an
  org-owned machine user). Scope: `read:packages` for the orgs/
  repos voodu needs to pull from. Token lifetime: as long as
  practicable (1y typical).
- Quay / Harbor / GitLab: equivalent service-account flow.

Then standardise how every dev gets the token at apply time:

- **`.envrc` in the repo** (gitignored, value distributed via
  team password manager / shared secrets vault). Devs source it
  via `direnv` and `vd apply` reads from `os.Environ()`.
- **CI runner secret** (GitHub Actions secret, etc.) for automated
  applies.

Rotation: rotate the bot token centrally, update the password
manager entry, every dev's next `direnv reload` picks it up. One
artifact, one rotation, no per-dev drift.

### What NOT to do

- ❌ Per-dev personal PATs declared inline. Whoever applied last
  wins; sometimes-failing pulls until someone re-applies.
- ❌ Long-lived plaintext tokens checked into git.
- ❌ Hoping `~/.docker/config.json` survives forever — it does,
  but only as long as the credential it holds stays valid.

### Why no `env_from` on `registry` today

We considered it ("just put the token in a `vd config` bucket").
But the bucket would solve only the WHERE-IS-THE-SECRET-STORED
question, not the underlying ONE-CREDENTIAL-PER-HOST constraint.
The right architectural answer is "use a service account so the
one credential serves the whole team", which the `.envrc` flow
already supports. Adding `env_from` to `RegistrySpec` would be
mostly redundant.

If your model truly needs per-dev tokens that don't trample each
other on the host (e.g. multi-tenant voodu controller), that's a
larger refactor than `env_from` — the `~/.docker/config.json`
singularity would need addressing first.

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
| [`ghcr-private.hcl`](./ghcr-private.hcl) | GitHub Container Registry — one registry block + a deployment pulling from `ghcr.io/acme/private-api:1.0`. Secret comes from `${GHCR_TOKEN}` in the shell env (service-account-fed via `.envrc`). |
| [`multi-registry.hcl`](./multi-registry.hcl) | Two registries simultaneously (ghcr.io + self-hosted Harbor). Demonstrates that registries are host-wide — deployments in different scopes pull from different registries with one set of declarations. |
| [`.envrc.example`](./.envrc.example) | Template for the gitignored `.envrc` your team distributes via password manager. Shows the SHAPE of the env vars the examples reference; copy + edit + `direnv allow`. |
