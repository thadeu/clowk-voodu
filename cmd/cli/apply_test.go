package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// TestApplyEndToEnd is the M4 done criterion in test form: voodu apply
// -f <dir> reads .hcl manifests, translates them to the controller wire
// shape, and POSTs them to /apply.
func TestApplyEndToEnd(t *testing.T) {
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "deployment.hcl"), `
deployment "api" {
  image = "nginx:${TAG:-1}"
}
`)

	mustWrite(t, filepath.Join(dir, "database.hcl"), `
database "main" {
  engine = "postgres"
}
`)

	mustWrite(t, filepath.Join(dir, "ingress.hcl"), `
ingress "api" {
  host    = "api.example.com"
  service = "api"
}
`)

	var (
		mu       sync.Mutex
		received []controller.Manifest
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apply" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}

		raw, _ := io.ReadAll(r.Body)

		var arr []controller.Manifest

		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, err.Error(), 500)

			return
		}

		mu.Lock()
		received = append(received, arr...)
		mu.Unlock()

		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	cmd, _, err := root.Find([]string{"apply"})
	if err != nil {
		t.Fatal(err)
	}

	if err := runApply(cmd, applyFlags{files: []string{dir}}); err != nil {
		t.Fatalf("runApply: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 3 {
		t.Fatalf("expected 3 manifests, got %d: %+v", len(received), received)
	}

	kinds := map[controller.Kind]bool{}
	for _, m := range received {
		kinds[m.Kind] = true
	}

	for _, want := range []controller.Kind{controller.KindDeployment, controller.KindDatabase, controller.KindIngress} {
		if !kinds[want] {
			t.Errorf("missing kind %s", want)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveManifestPath(t *testing.T) {
	dir := t.TempDir()

	hcl := filepath.Join(dir, "api.hcl")
	yml := filepath.Join(dir, "legacy.yml")
	yaml := filepath.Join(dir, "old.yaml")
	exact := filepath.Join(dir, "exact.hcl")
	subdir := filepath.Join(dir, "stack")

	mustWrite(t, hcl, `deployment "api" { image = "x" }`+"\n")
	mustWrite(t, yml, "apps:\n  - name: legacy\n")
	mustWrite(t, yaml, "apps:\n  - name: old\n")
	mustWrite(t, exact, `deployment "exact" { image = "x" }`+"\n")

	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"exact file wins over extension fallback", exact, exact},
		{"bare name resolves to .hcl", filepath.Join(dir, "api"), hcl},
		{"bare name resolves to .yml", filepath.Join(dir, "legacy"), yml},
		{"bare name resolves to .yaml", filepath.Join(dir, "old"), yaml},
		{"directory passes through", subdir, subdir},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := resolveManifestPath(c.in)
			if err != nil {
				t.Fatalf("resolveManifestPath(%q): %v", c.in, err)
			}

			if got != c.want {
				t.Errorf("resolveManifestPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	t.Run("missing path errors on original name", func(t *testing.T) {
		_, _, err := resolveManifestPath(filepath.Join(dir, "nope"))
		if err == nil {
			t.Fatal("expected error for missing path")
		}

		if !os.IsNotExist(err) {
			t.Errorf("expected not-exist error, got %T: %v", err, err)
		}
	})
}
