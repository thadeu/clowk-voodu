package lang

import "testing"

// TestBuildSpec_BuildArgs covers the simplified BuildArgs surface:
// after the manifest refactor, lang handlers see a single flat
// BuildSpec.BuildArgs map (populated from the manifest's `build.args
// = {...}`). LangBuildSpec no longer carries its own build_args —
// one source of truth.
func TestBuildSpec_BuildArgs(t *testing.T) {
	s := &BuildSpec{
		BuildArgs: map[string]string{
			"SERVICE":    "api",
			"GO_VERSION": "1.25",
		},
		Lang: &LangBuildSpec{Name: "go"},
	}

	if s.BuildArgs["SERVICE"] != "api" {
		t.Errorf("SERVICE: got %q, want %q", s.BuildArgs["SERVICE"], "api")
	}

	if s.BuildArgs["GO_VERSION"] != "1.25" {
		t.Errorf("GO_VERSION: got %q, want %q", s.BuildArgs["GO_VERSION"], "1.25")
	}
}

// TestBuildSpec_NilSafe pins the zero-value contract: nil-pointer
// receiver on LangName, empty map on BuildArgs — both should be
// safe for handlers to consult without nil checks.
func TestBuildSpec_NilSafe(t *testing.T) {
	if name := (*BuildSpec)(nil).LangName(); name != "" {
		t.Errorf("nil spec LangName: got %q, want empty", name)
	}

	if name := (&BuildSpec{}).LangName(); name != "" {
		t.Errorf("empty spec LangName: got %q, want empty", name)
	}

	if name := (&BuildSpec{Lang: &LangBuildSpec{Name: "go"}}).LangName(); name != "go" {
		t.Errorf("populated LangName: got %q, want %q", name, "go")
	}
}
