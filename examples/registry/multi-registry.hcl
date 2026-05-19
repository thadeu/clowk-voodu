# Two private registries on the same host — public PaaS (GHCR) plus
# a self-hosted Harbor for internal-only images.
#
# What this shows:
#
#   1. Two registry blocks coexisting. Voodu rebuilds
#      ~/.docker/config.json from scratch on every apply, merging
#      both entries into the `auths` map. No precedence rules —
#      docker picks per-pull by matching the image's hostname
#      against the auths keys.
#
#   2. Registries are HOST-WIDE, not scoped. Notice the deployments
#      below live in different scopes (`public` and `internal`) but
#      neither needs its own registry block — once the two
#      registries are declared anywhere, every deployment on the
#      host can pull from either.
#
#   3. Both secrets come from the operator's shell env via ${...}.
#      Interpolation is client-side at parse time, so the plaintext
#      never enters this file. The controller only sees the
#      substituted values.
#
# Apply:
#
#   cd examples/registry
#   export GHCR_USER=acme-deploy-bot
#   export GHCR_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
#   export HARBOR_USER=acme-ci
#   export HARBOR_TOKEN=cli-secret-yyyyyyyyyyyyyyyyyyyyyyyy
#   vd apply -f multi-registry.hcl
#
# After apply, ~/.docker/config.json carries TWO entries under
# `auths`: "ghcr.io" and "harbor.internal.acme.com". The next
# `docker pull` against either host authenticates transparently.

registry "ghcr" {
  url      = "ghcr.io"
  username = "${GHCR_USER}"
  token    = "${GHCR_TOKEN}"
}

registry "harbor" {
  # Self-hosted Harbor on the corporate network. The url is the
  # bare hostname as docker sees it on the wire — no scheme. If
  # the registry runs on a non-standard port, include it
  # (`harbor.internal.acme.com:5000`) to match what `docker login`
  # would write.
  url = "harbor.internal.acme.com"

  username = "${HARBOR_USER}"

  # Harbor CLI secrets are commonly called "passwords" in their
  # UI. The HCL surface accepts both — `password = "..."` would
  # decode into the same Token field. Using `token` here for
  # consistency with the ghcr block above.
  token = "${HARBOR_TOKEN}"
}

deployment "public" "marketing-site" {
  # Pulls from ghcr.io — handled by the "ghcr" registry block.
  image    = "ghcr.io/acme/marketing-site:2.1"
  replicas = 2

  ports = ["8080"]
}

deployment "internal" "backend" {
  # Pulls from Harbor — handled by the "harbor" registry block.
  # The deployment doesn't reference the registry by name; docker
  # picks the right auth entry by matching the image hostname
  # against the auths keys in config.json.
  image    = "harbor.internal.acme.com/team/backend:2.5"
  replicas = 3

  ports = ["9000"]

  env = {
    SERVICE_TIER = "internal"
  }
}
