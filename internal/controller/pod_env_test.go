package controller

import (
	"testing"
)

// TestBuildStatefulsetPodEnv pins the four-key shape every
// stateful pod gets at boot. These names cross the platform/plugin
// boundary — voodu-redis reads VOODU_REPLICA_ORDINAL to pick a
// role, voodu-postgres will too. A typo here breaks every plugin
// at once, silently, so the test names them explicitly.
func TestBuildStatefulsetPodEnv(t *testing.T) {
	env := BuildStatefulsetPodEnv("clowk-lp", "redis", 2)

	want := map[string]string{
		"VOODU_SCOPE":           "clowk-lp",
		"VOODU_NAME":            "redis",
		"VOODU_REPLICA_ORDINAL": "2",
		"VOODU_REPLICA_ID":      "2",
	}

	if len(env) != len(want) {
		t.Fatalf("env has %d keys, want %d: %+v", len(env), len(want), env)
	}

	for k, v := range want {
		if got := env[k]; got != v {
			t.Errorf("env[%q] = %q, want %q", k, got, v)
		}
	}
}

// TestBuildStatefulsetPodEnv_NegativeOrdinalClamped guards against
// programmer error reaching the docker layer with a negative ordinal.
// Same posture as containers.OrdinalReplicaID — clamp at the boundary
// rather than panic, so a buggy caller surfaces as ordinal 0 (the
// "primary" slot by convention) instead of a confusing docker run
// error about an empty env value.
func TestBuildStatefulsetPodEnv_NegativeOrdinalClamped(t *testing.T) {
	env := BuildStatefulsetPodEnv("scope", "name", -3)

	if got := env["VOODU_REPLICA_ORDINAL"]; got != "0" {
		t.Errorf("VOODU_REPLICA_ORDINAL = %q, want %q (negative clamps to 0)", got, "0")
	}

	if got := env["VOODU_REPLICA_ID"]; got != "0" {
		t.Errorf("VOODU_REPLICA_ID = %q, want %q", got, "0")
	}
}

// TestBuildDeploymentPodEnv pins the deployment-side identity env.
// No ordinal because deployment replicas are interchangeable —
// adding one would create the false impression that ordinal 0
// is special, which is the very assumption deployments DON'T
// make (and statefulsets DO).
func TestBuildDeploymentPodEnv(t *testing.T) {
	env := BuildDeploymentPodEnv("clowk-lp", "web")

	if len(env) != 2 {
		t.Fatalf("expected 2 keys (scope+name), got %d: %+v", len(env), env)
	}

	if env["VOODU_SCOPE"] != "clowk-lp" {
		t.Errorf("VOODU_SCOPE = %q, want %q", env["VOODU_SCOPE"], "clowk-lp")
	}

	if env["VOODU_NAME"] != "web" {
		t.Errorf("VOODU_NAME = %q, want %q", env["VOODU_NAME"], "web")
	}

	if _, exists := env["VOODU_REPLICA_ORDINAL"]; exists {
		t.Errorf("deployment env should NOT carry VOODU_REPLICA_ORDINAL; replicas are interchangeable")
	}
}

// TestMergePodEnv_OperatorWins pins the last-wins contract —
// platform defaults inject first (low priority), operator's
// own env layers on top and beats collisions. Mirrors docker
// `-e` semantics (later flag wins) and gives operators a real
// override hatch (e.g., point VOODU_CONTROLLER_URL at a mock
// for local testing, or use a logical VOODU_SCOPE alias).
//
// Cross-tenant safety isn't enforced by env (voodu uses manifest
// source + container labels for authorization), so an operator
// "spoofing" VOODU_SCOPE in their OWN pod just confuses their
// OWN application — no platform invariant is broken, no other
// tenant's data is exposed.
func TestMergePodEnv_OperatorWins(t *testing.T) {
	platform := map[string]string{
		"VOODU_SCOPE": "platform-default",
		"VOODU_NAME":  "platform-name",
	}

	extra := map[string]string{
		"VOODU_SCOPE":   "operator-override",
		"APP_LOG_LEVEL": "info",
	}

	merged := MergePodEnv(platform, extra)

	if merged["VOODU_SCOPE"] != "operator-override" {
		t.Errorf("VOODU_SCOPE = %q, want %q (operator override must win, last-wins semantics)",
			merged["VOODU_SCOPE"], "operator-override")
	}

	if merged["VOODU_NAME"] != "platform-name" {
		t.Errorf("VOODU_NAME = %q, want %q (platform default must survive when operator doesn't override)",
			merged["VOODU_NAME"], "platform-name")
	}

	if merged["APP_LOG_LEVEL"] != "info" {
		t.Errorf("operator-supplied APP_LOG_LEVEL got dropped: %q", merged["APP_LOG_LEVEL"])
	}
}

// TestBuildPlatformEnv_EmitsControllerURL pins the auto-injection
// of VOODU_CONTROLLER_URL — the env key plugin entrypoint scripts
// (sentinel's failover hook, future probes) read to call back
// into /describe, /config, /plugin/<name>/<command> without
// operator-set env. Without this, every plugin needing controller
// callback would force the operator to set VOODU_CONTROLLER_URL
// manually in HCL — exactly the friction this auto-injection
// removes.
func TestBuildPlatformEnv_EmitsControllerURL(t *testing.T) {
	env := BuildPlatformEnv("http://controller.voodu:8686")

	if got := env[EnvControllerURL]; got != "http://controller.voodu:8686" {
		t.Errorf("VOODU_CONTROLLER_URL = %q, want %q", got, "http://controller.voodu:8686")
	}
}

// TestBuildPlatformEnv_EmptyURLReturnsNil is the test-friendly
// path: setups without a controller URL configured (unit tests,
// development) get a nil map and skip the platform layer entirely
// — no spurious empty env var leaks into pod specs.
func TestBuildPlatformEnv_EmptyURLReturnsNil(t *testing.T) {
	env := BuildPlatformEnv("")

	if env != nil {
		t.Errorf("BuildPlatformEnv(\"\") = %+v, want nil", env)
	}
}

// TestMergePodEnv_OperatorOverridesControllerURL pins the
// override hatch — operator can deliberately point
// VOODU_CONTROLLER_URL at a mock for local testing, a private
// reverse proxy for cross-VM setups where the auto-derived URL
// isn't reachable, or any other case where the platform default
// doesn't fit. Same last-wins semantics as the rest of the env
// merge.
func TestMergePodEnv_OperatorOverridesControllerURL(t *testing.T) {
	platform := BuildPlatformEnv("http://controller.voodu:8686")

	extra := map[string]string{
		EnvControllerURL: "http://my-mock:3000",
	}

	merged := MergePodEnv(platform, extra)

	if got := merged[EnvControllerURL]; got != "http://my-mock:3000" {
		t.Errorf("VOODU_CONTROLLER_URL = %q, want %q (operator override must win)",
			got, "http://my-mock:3000")
	}
}

// TestMergePodEnv_NilSafe — both inputs nil returns nil so the
// docker layer can short-circuit the `-e` loop without a length
// check at every call site.
func TestMergePodEnv_NilSafe(t *testing.T) {
	if got := MergePodEnv(nil, nil); got != nil {
		t.Errorf("MergePodEnv(nil, nil) = %+v, want nil", got)
	}

	platform := map[string]string{"K": "v"}

	if got := MergePodEnv(platform, nil); got["K"] != "v" {
		t.Errorf("MergePodEnv(platform, nil) lost the platform key: %+v", got)
	}

	extra := map[string]string{"K2": "v2"}

	if got := MergePodEnv(nil, extra); got["K2"] != "v2" {
		t.Errorf("MergePodEnv(nil, extra) lost the extra key: %+v", got)
	}
}
