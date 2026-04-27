package controller

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestDeploymentHandler_LinkEnv_MergesEtcdConfig is the M-4
// integration guard: linkEnv must blend controller-managed config
// (etcd /config bucket) with the manifest's spec.Env, with manifest
// winning on conflict. Without this an operator who runs
// `vd config set` would see the value persisted but never reach
// the running container's env file.
func TestDeploymentHandler_LinkEnv_MergesEtcdConfig(t *testing.T) {
	store := newMemStore()

	// Seed etcd config: scope-level + app-level. App-level FOO
	// should override scope-level FOO; manifest should later
	// override app-level FOO.
	_ = store.SetConfig(context.Background(), "test", "", map[string]string{
		"FOO":       "scope",
		"SCOPE_KEY": "scope-only",
	})
	_ = store.SetConfig(context.Background(), "test", "api", map[string]string{
		"FOO":     "app",
		"APP_KEY": "app-only",
	})

	var captured []envWrite

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		WriteEnv: func(app string, pairs []string) (bool, error) {
			captured = append(captured, envWrite{App: app, Pairs: append([]string(nil), pairs...)})

			return true, nil
		},
	}

	// Manifest declares FOO and MANIFEST_KEY. Manifest FOO must win.
	specEnv := map[string]string{
		"FOO":          "manifest",
		"MANIFEST_KEY": "manifest-only",
	}

	if _, err := h.linkEnv(context.Background(), "test", "api", "test-api", specEnv); err != nil {
		t.Fatalf("linkEnv: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("expected one WriteEnv call, got %d", len(captured))
	}

	got := pairsToMap(captured[0].Pairs)

	for _, c := range []struct{ key, want string }{
		{"FOO", "manifest"},          // manifest wins
		{"SCOPE_KEY", "scope-only"},  // unique to scope, kept
		{"APP_KEY", "app-only"},      // unique to app, kept
		{"MANIFEST_KEY", "manifest-only"},
	} {
		if got[c.key] != c.want {
			t.Errorf("env[%s]=%q want %q (full env: %v)", c.key, got[c.key], c.want, got)
		}
	}
}

// pairsToMap parses the "KEY=VALUE" slice the env-writer hook
// receives back into a map for assertion-friendly access.
func pairsToMap(pairs []string) map[string]string {
	out := make(map[string]string, len(pairs))

	for _, p := range pairs {
		idx := strings.IndexByte(p, '=')
		if idx <= 0 {
			continue
		}

		out[p[:idx]] = p[idx+1:]
	}

	return out
}

// TestDeploymentHandler_LinkEnv_NoConfigStillWorks is the
// degenerate case: an empty etcd /config bucket must not break
// the existing "manifest only" path. Operators upgrading from
// pre-M-4 voodu should see no behaviour change until they actually
// `vd config set` something.
func TestDeploymentHandler_LinkEnv_NoConfigStillWorks(t *testing.T) {
	store := newMemStore()

	var captured []envWrite

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		WriteEnv: func(app string, pairs []string) (bool, error) {
			captured = append(captured, envWrite{App: app, Pairs: append([]string(nil), pairs...)})

			return true, nil
		},
	}

	specEnv := map[string]string{"FOO": "bar"}

	if _, err := h.linkEnv(context.Background(), "test", "api", "test-api", specEnv); err != nil {
		t.Fatalf("linkEnv: %v", err)
	}

	got := pairsToMap(captured[0].Pairs)
	if got["FOO"] != "bar" {
		t.Errorf("FOO=%q want bar", got["FOO"])
	}

	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	if len(keys) != 1 || keys[0] != "FOO" {
		t.Errorf("expected only FOO, got %v", keys)
	}
}
