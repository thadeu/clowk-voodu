package controller

import (
	"strings"
	"testing"
)

// TestBuildNetworkAliases locks in the alias-naming contract every
// scoped resource exposes on the docker network: a short
// `<name>.<scope>` form for daily use plus the FQDN-ish
// `<name>.<scope>.voodu` for code that wants something obviously
// branded as voodu's. Any drift here ripples into every container
// the controller spawns, so the cases below cover the surfaces
// operators rely on.
func TestBuildNetworkAliases(t *testing.T) {
	cases := []struct {
		name  string
		scope string
		res   string
		want  []string
	}{
		{
			name:  "scoped resource gets short + FQDN",
			scope: "clowk-lp",
			res:   "web",
			want:  []string{"web.clowk-lp", "web.clowk-lp.voodu"},
		},
		{
			name:  "unscoped resource collapses to bare name",
			scope: "",
			res:   "main",
			want:  []string{"main", "main.voodu"},
		},
		{
			name:  "empty name yields no aliases",
			scope: "anything",
			res:   "",
			want:  nil,
		},
		{
			name:  "uppercase normalised down to lowercase",
			scope: "Clowk-LP",
			res:   "WEB",
			want:  []string{"web.clowk-lp", "web.clowk-lp.voodu"},
		},
		{
			name:  "surrounding whitespace is trimmed",
			scope: "  clowk-lp  ",
			res:   "  web  ",
			want:  []string{"web.clowk-lp", "web.clowk-lp.voodu"},
		},
		{
			name:  "hyphens preserved verbatim — DNS-safe",
			scope: "team-a",
			res:   "frontend-edge",
			want:  []string{"frontend-edge.team-a", "frontend-edge.team-a.voodu"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildNetworkAliases(c.scope, c.res)

			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("BuildNetworkAliases(%q, %q) = %v, want %v",
					c.scope, c.res, got, c.want)
			}
		})
	}
}

// TestNetworkAliasTLDIsBranded is a guardrail: the .voodu TLD has
// to stay branded enough to never collide with real DNS. If someone
// accidentally drops it down to ".local" a future test failure here
// surfaces the regression before mDNS-enabled containers start
// silently dropping queries.
func TestNetworkAliasTLDIsBranded(t *testing.T) {
	for _, forbidden := range []string{"local", "lan", "internal", "svc", "cluster"} {
		if networkAliasTLD == forbidden {
			t.Errorf("networkAliasTLD = %q must not be a generic-DNS reserved label like %q",
				networkAliasTLD, forbidden)
		}
	}
}
