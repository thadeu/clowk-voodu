package controller

import "testing"

// Normalize is the whole validation surface of a trust statement, so its edge
// cases are the ones worth naming.
func TestTriggerInputNormalize(t *testing.T) {
	cases := []struct {
		name    string
		in      TriggerInput
		want    TriggerInput
		wantErr bool
	}{
		{
			"canonical input passes through",
			TriggerInput{Repo: "acme/web", Branch: "main", AllowScopes: []string{"runa"}},
			TriggerInput{Repo: "acme/web", Branch: "main", AllowScopes: []string{"runa"}},
			false,
		},
		{
			// GitHub treats owner and repository names case-insensitively, so
			// a trigger that failed to match because somebody typed Acme/Web
			// would be debugged as "the webhook is broken".
			"repo lowercases",
			TriggerInput{Repo: "  ACME/Web  ", Branch: "main", AllowScopes: []string{"runa"}},
			TriggerInput{Repo: "acme/web", Branch: "main", AllowScopes: []string{"runa"}},
			false,
		},
		{
			// Branch names ARE case-sensitive in git. The asymmetry with repo
			// is deliberate, not an oversight.
			"branch keeps its case",
			TriggerInput{Repo: "acme/web", Branch: "Release-2.0", AllowScopes: []string{"runa"}},
			TriggerInput{Repo: "acme/web", Branch: "Release-2.0", AllowScopes: []string{"runa"}},
			false,
		},
		{
			"scopes dedupe and sort",
			TriggerInput{Repo: "acme/web", Branch: "main", AllowScopes: []string{"z", "a", "z", " a "}},
			TriggerInput{Repo: "acme/web", Branch: "main", AllowScopes: []string{"a", "z"}},
			false,
		},

		{"repo missing", TriggerInput{Branch: "main", AllowScopes: []string{"a"}}, TriggerInput{}, true},
		{"repo without owner", TriggerInput{Repo: "web", Branch: "main", AllowScopes: []string{"a"}}, TriggerInput{}, true},
		{"repo with too many parts", TriggerInput{Repo: "a/b/c", Branch: "main", AllowScopes: []string{"a"}}, TriggerInput{}, true},
		// These are interpolated into GitHub API URLs; a `..` addresses a
		// different endpoint than the code reads like it addresses.
		{"repo with dot dot", TriggerInput{Repo: "../etc", Branch: "main", AllowScopes: []string{"a"}}, TriggerInput{}, true},
		{"repo with space", TriggerInput{Repo: "acme/we b", Branch: "main", AllowScopes: []string{"a"}}, TriggerInput{}, true},
		{"branch missing", TriggerInput{Repo: "acme/web", AllowScopes: []string{"a"}}, TriggerInput{}, true},
		// An empty allow list is a mistake, not a policy: a trigger that
		// allows nothing can deploy nothing.
		{"no scopes", TriggerInput{Repo: "acme/web", Branch: "main"}, TriggerInput{}, true},
		{"blank scopes only", TriggerInput{Repo: "acme/web", Branch: "main", AllowScopes: []string{" ", ""}}, TriggerInput{}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.in.Normalize()

			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}

			if c.wantErr {
				return
			}

			if got.Repo != c.want.Repo || got.Branch != c.want.Branch {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}

			if len(got.AllowScopes) != len(c.want.AllowScopes) {
				t.Fatalf("scopes: got %v, want %v", got.AllowScopes, c.want.AllowScopes)
			}

			for i := range got.AllowScopes {
				if got.AllowScopes[i] != c.want.AllowScopes[i] {
					t.Fatalf("scopes: got %v, want %v", got.AllowScopes, c.want.AllowScopes)
				}
			}
		})
	}
}

// Two operators typing the same trust statement differently must produce the
// same record, or "is this repository already configured" is unanswerable by
// looking.
func TestNormalizeIsCanonical(t *testing.T) {
	a, err := TriggerInput{Repo: "Acme/Web", Branch: "main", AllowScopes: []string{"b", "a"}}.Normalize()
	if err != nil {
		t.Fatal(err)
	}

	b, err := TriggerInput{Repo: " acme/web ", Branch: "main", AllowScopes: []string{"a", "b", "a"}}.Normalize()
	if err != nil {
		t.Fatal(err)
	}

	if a.Repo != b.Repo || len(a.AllowScopes) != len(b.AllowScopes) {
		t.Fatalf("not canonical: %+v vs %+v", a, b)
	}
}

// The check that makes invariant II mean something: a manifest declaring a
// scope the operator did not authorise cannot be applied.
func TestAllowsScope(t *testing.T) {
	trigger := &Trigger{AllowScopes: []string{"runa", "staging"}}

	if !trigger.AllowsScope("runa") {
		t.Error("an allowed scope must be allowed")
	}

	if trigger.AllowsScope("producao") {
		t.Error("a scope nobody authorised must be refused")
	}

	// The empty scope is what unscoped kinds carry. Allowing it is something
	// an operator opts into, not something that falls out of a nil check.
	if trigger.AllowsScope("") {
		t.Error("the empty scope must not be allowed implicitly")
	}

	if !(&Trigger{AllowScopes: []string{""}}).AllowsScope("") {
		t.Error("an explicitly allowed empty scope must be allowed")
	}
}

// ALL refusals, not the first: an operator widening a trigger wants to do it
// once. One refusal per attempt turns a three-scope repository into three
// round trips through a failing deploy.
func TestRefusedScopesReportsEveryOne(t *testing.T) {
	trigger := &Trigger{AllowScopes: []string{"runa"}}

	got := trigger.RefusedScopes([]string{"runa", "producao", "staging"})

	if len(got) != 2 || got[0] != "producao" || got[1] != "staging" {
		t.Fatalf("got %v, want [producao staging]", got)
	}

	if refused := trigger.RefusedScopes([]string{"runa"}); refused != nil {
		t.Errorf("nothing should be refused: %v", refused)
	}
}

func TestMatchesRepoIsCaseInsensitive(t *testing.T) {
	trigger := &Trigger{Repo: "acme/web"}

	for _, in := range []string{"acme/web", "Acme/Web", " ACME/WEB "} {
		if !trigger.MatchesRepo(in) {
			t.Errorf("%q should match", in)
		}
	}

	if trigger.MatchesRepo("acme/other") {
		t.Error("a different repository must not match")
	}
}

// A repository can be renamed on GitHub without anything telling this box. An
// id derived from the name would break deploys in a way nobody could trace
// back to the rename.
func TestTriggerIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}

	for i := 0; i < 500; i++ {
		id := NewTriggerID()

		if id == "" {
			t.Fatal("empty id")
		}

		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}

		seen[id] = true
	}
}
