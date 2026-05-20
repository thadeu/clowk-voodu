# Pull from a private GitHub Container Registry repo.
#
# What this shows:
#
#   1. A registry "ghcr" block declaring credentials for ghcr.io.
#      The block label is the registry's name — voodu uses it as
#      the manifest identity, but the docker config entry is keyed
#      by url.
#
#   2. A deployment in scope "acme" pulling from
#      ghcr.io/acme/private-api:1.0. The deployment carries NO
#      registry-specific config — once the registry manifest is
#      applied, every deployment on the host can pull from ghcr.io.
#
#   3. The secret comes from ${GHCR_TOKEN} in the operator's shell
#      env. The plaintext token NEVER lands in this file or in any
#      git repo. Interpolation happens client-side at parse time;
#      the controller only sees the substituted value, which it
#      then base64-encodes into ~/.docker/config.json.
#
# Creating the GHCR token — USE A SERVICE ACCOUNT, NOT YOUR
# PERSONAL ONE.
#
#   ~/.docker/config.json on the controller host is SINGULAR: one
#   credential per registry, host-wide. Every vd apply that
#   includes this registry block rewrites the file with WHATEVER
#   token the operator's shell holds. Per-dev personal PATs
#   trample each other — only the last applier's credential is
#   active on the host. See examples/registry/README.md
#   "One credential per registry, per host" for the full story.
#
#   The right shape for a team:
#
#     1. GitHub → Settings → Developer settings → Personal access
#        tokens (classic) on a DEDICATED MACHINE USER / SERVICE
#        ACCOUNT (not a human user). Scope: `read:packages`.
#     2. Store the token in your team password manager.
#     3. Drop a `.envrc` in this repo (gitignored) with:
#          export GHCR_USER=acme-deploy-bot
#          export GHCR_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
#     4. Devs use direnv (or source the file manually). Every
#        vd apply substitutes the same value, regardless of who
#        runs it. Rotation = one password manager update + every
#        dev's next direnv reload.
#
# Why `vd config set` isn't an option here: `registry` is unscoped
# and host-wide, and it does NOT support env_from. The recommended
# pattern is `.envrc` + service account token rather than a config
# bucket.
#
# Apply (after .envrc is in place):
#
#   cd examples/registry
#   vd apply -f ghcr-private.hcl
#
# After apply, ~/.docker/config.json on the host contains the auth
# entry; `docker pull ghcr.io/acme/private-api:1.0` succeeds without
# any further `docker login`, and persists across controller
# reboots and autoscaler-driven pulls.

registry "ghcr" {
  url = "ghcr.io"

  # Both ${VAR}s resolve from the operator's shell at parse time.
  # Use a SERVICE ACCOUNT (machine user / dedicated bot), NOT a
  # personal PAT — see the file header for the rationale. The
  # plaintext NEVER lands in this file; .envrc is the right
  # storage layer for distribution within a team.
  username = "${GHCR_USER}"

  # `token` and `password` are aliases — both decode into the
  # same wire field. `token` reads more naturally for a PAT.
  token = "${GHCR_TOKEN}"
}

deployment "acme" "api" {
  # Private image. Without the registry block above, docker pull
  # would return 401 here and the reconcile would loop forever
  # on "image fetch failed".
  image    = "ghcr.io/acme/private-api:1.0"
  replicas = 2

  ports = ["3000"]

  env = {
    RAILS_ENV = "production"
  }
}
