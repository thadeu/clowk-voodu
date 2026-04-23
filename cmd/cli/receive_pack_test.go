package main

import "testing"

func TestParseScopedRef(t *testing.T) {
	cases := []struct {
		in    string
		scope string
		name  string
		ok    bool
	}{
		{"clowk/web", "clowk", "web", true},
		{"web", "", "web", true},
		{"", "", "", false},
		{"/web", "", "", false},
		{"clowk/", "", "", false},
		{"a/b/c", "", "", false},
		{"a//b", "", "", false},
	}

	for _, tc := range cases {
		scope, name, err := parseScopedRef(tc.in)

		if tc.ok {
			if err != nil {
				t.Errorf("parseScopedRef(%q) unexpected error: %v", tc.in, err)
				continue
			}

			if scope != tc.scope || name != tc.name {
				t.Errorf("parseScopedRef(%q) = (%q,%q), want (%q,%q)", tc.in, scope, name, tc.scope, tc.name)
			}

			continue
		}

		if err == nil {
			t.Errorf("parseScopedRef(%q) expected error, got (%q,%q)", tc.in, scope, name)
		}
	}
}
