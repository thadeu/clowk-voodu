package plugin

import "testing"

func TestManifestHasAlias(t *testing.T) {
	m := Manifest{
		Name:    "postgres",
		Aliases: []string{"pg", "postgresql"},
	}

	cases := []struct {
		query string
		want  bool
	}{
		{"pg", true},
		{"postgresql", true},
		{"postgres", false}, // canonical name is NOT an alias
		{"", false},
		{"redis", false},
	}

	for _, tc := range cases {
		got := m.HasAlias(tc.query)
		if got != tc.want {
			t.Errorf("HasAlias(%q): got %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestManifestHasAlias_EmptyAliases(t *testing.T) {
	m := Manifest{Name: "postgres"}

	if m.HasAlias("pg") {
		t.Error("nil aliases should yield false")
	}
}

func TestManifestHasAlias_CaseSensitive(t *testing.T) {
	// We don't case-fold — aliases are conventionally lowercase
	// (`pg`, `r`) and case-folding adds confusion for marginal
	// gain. Pin the contract.
	m := Manifest{Aliases: []string{"pg"}}

	if !m.HasAlias("pg") {
		t.Error("exact lowercase match should hit")
	}

	if m.HasAlias("PG") {
		t.Error("PG should NOT match (case-sensitive contract)")
	}
}
