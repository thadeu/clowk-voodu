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

// TestBuildPodNetworkAliases pins the per-pod alias contract for
// statefulset replicas. The shape mirrors BuildNetworkAliases but
// with the ordinal woven into the leftmost label — `pg-0.scope`
// resolves to ordinal 0 deterministically, never round-robined
// with pg-1+. Without the per-pod alias, postgres replicas
// couldn't address a specific primary by hostname.
func TestBuildPodNetworkAliases(t *testing.T) {
	cases := []struct {
		name    string
		scope   string
		res     string
		ordinal int
		want    []string
	}{
		{
			name:    "scoped statefulset pod gets ordinal-prefixed aliases",
			scope:   "data",
			res:     "pg",
			ordinal: 0,
			want:    []string{"pg-0.data", "pg-0.data.voodu"},
		},
		{
			name:    "second replica carries its own ordinal",
			scope:   "data",
			res:     "pg",
			ordinal: 2,
			want:    []string{"pg-2.data", "pg-2.data.voodu"},
		},
		{
			name:    "unscoped statefulset collapses scope segment",
			scope:   "",
			res:     "redis",
			ordinal: 1,
			want:    []string{"redis-1", "redis-1.voodu"},
		},
		{
			name:    "empty name yields no aliases",
			scope:   "data",
			res:     "",
			ordinal: 0,
			want:    nil,
		},
		{
			name:    "negative ordinal clamps to zero",
			scope:   "data",
			res:     "pg",
			ordinal: -3,
			want:    []string{"pg-0.data", "pg-0.data.voodu"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildPodNetworkAliases(c.scope, c.res, c.ordinal)

			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("BuildPodNetworkAliases(%q, %q, %d) = %v, want %v",
					c.scope, c.res, c.ordinal, got, c.want)
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
