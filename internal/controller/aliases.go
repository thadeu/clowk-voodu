package controller

import "strings"

// networkAliasTLD is the synthetic top-level voodu reserves for the
// fully-qualified DNS form of every container alias. Branded so it
// never collides with real DNS (no ICANN TLD called "voodu") and
// avoids the mDNS pitfall around `.local`. Cheap insurance: every
// container picks up both the short form `<name>.<scope>` and the
// FQDN-ish `<name>.<scope>.voodu` so apps can target either.
const networkAliasTLD = "voodu"

// BuildNetworkAliases returns the DNS names a container should
// register on each network it joins. The order matches Docker's:
// the first alias is the "primary" name (what shows up first in
// `docker inspect`), followed by the FQDN form.
//
// Rules:
//
//   - Scoped resource (deployment/job/cronjob with a scope):
//     ["<name>.<scope>", "<name>.<scope>.voodu"]
//
//   - Unscoped resource (e.g. a future plugin-managed singleton):
//     ["<name>", "<name>.voodu"]
//
//   - Empty name → no aliases. The container falls back to its
//     docker container name for DNS, which is still valid via
//     Docker's automatic per-network name registration.
//
// Both scope and name are lowercased before composing — DNS is
// case-insensitive on the wire, but resolvers vary on whether they
// lowercase before cache lookup. Normalising at registration time
// guarantees the alias is found regardless of how the client
// uppercases its query.
//
// The function is pure (no host calls, no IO), so handlers can
// invoke it freely while building a ContainerSpec.
func BuildNetworkAliases(scope, name string) []string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil
	}

	scope = strings.ToLower(strings.TrimSpace(scope))

	if scope == "" {
		return []string{
			name,
			name + "." + networkAliasTLD,
		}
	}

	short := name + "." + scope

	return []string{
		short,
		short + "." + networkAliasTLD,
	}
}
