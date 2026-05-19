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
# Creating the GHCR token:
#
#   GitHub → Settings → Developer settings → Personal access tokens
#   → Tokens (classic) → Generate new token. Required scope:
#   `read:packages`. For a fine-grained PAT, grant the target repo
#   "Read" on "Packages".
#
# Apply:
#
#   cd examples/registry
#   export GHCR_USER=acme-deploy-bot
#   export GHCR_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
#   vd apply -f ghcr-private.hcl
#
# After apply, ~/.docker/config.json on the host contains the auth
# entry; `docker pull ghcr.io/acme/private-api:1.0` succeeds without
# any further `docker login`.

registry "ghcr" {
  url = "ghcr.io"

  # The username on GHCR is the GitHub login (user or org) that
  # owns the PAT. For a service-account flow we typically create
  # a dedicated low-privilege bot account.
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
