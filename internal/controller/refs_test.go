package controller

import (
	"strings"
	"testing"
)

func TestInterpolateRefs(t *testing.T) {
	lookup := func(kind, name, field string) (string, bool) {
		if kind == "database" && name == "main" && field == "url" {
			return "postgres://u:p@h:5432/db", true
		}
		return "", false
	}

	got, err := InterpolateRefs("connect=${ref.database.main.url}", lookup)
	if err != nil {
		t.Fatal(err)
	}

	if got != "connect=postgres://u:p@h:5432/db" {
		t.Errorf("got %q", got)
	}
}

func TestInterpolateRefsUnresolvedIsError(t *testing.T) {
	lookup := func(kind, name, field string) (string, bool) { return "", false }

	_, err := InterpolateRefs("x=${ref.database.ghost.url}", lookup)
	if err == nil {
		t.Fatal("expected error for unresolved ref")
	}

	if !strings.Contains(err.Error(), "database.ghost.url") {
		t.Errorf("error should name the missing ref, got: %v", err)
	}
}

func TestInterpolateRefsMapLeavesPlainValuesAlone(t *testing.T) {
	lookup := func(kind, name, field string) (string, bool) {
		return "filled", true
	}

	in := map[string]string{
		"STATIC":       "hello",
		"FROM_REF":     "${ref.database.main.url}",
		"WITH_PREFIX":  "prefix:${ref.database.main.user}",
		"NO_REFS_AT_ALL": "just a string",
	}

	out, err := InterpolateRefsMap(in, lookup)
	if err != nil {
		t.Fatal(err)
	}

	if out["STATIC"] != "hello" {
		t.Errorf("STATIC: got %q", out["STATIC"])
	}

	if out["FROM_REF"] != "filled" {
		t.Errorf("FROM_REF: got %q", out["FROM_REF"])
	}

	if out["WITH_PREFIX"] != "prefix:filled" {
		t.Errorf("WITH_PREFIX: got %q", out["WITH_PREFIX"])
	}

	// Input must not be mutated in place.
	if in["FROM_REF"] != "${ref.database.main.url}" {
		t.Errorf("input map was mutated")
	}
}

func TestInterpolateRefsPatternIgnoresEnvVars(t *testing.T) {
	// Env-style ${FOO} must pass through untouched — refs and envs share
	// the ${...} sigil and live in different interpolation phases.
	lookup := func(kind, name, field string) (string, bool) { return "", false }

	got, err := InterpolateRefs("hello=${PLAIN_VAR}", lookup)
	if err != nil {
		t.Fatal(err)
	}

	if got != "hello=${PLAIN_VAR}" {
		t.Errorf("got %q", got)
	}
}
