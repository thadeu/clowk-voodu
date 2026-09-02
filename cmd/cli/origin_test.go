package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/clientinfo"
)

// seedClientInfoCache writes a fresh cache entry so remoteEnv resolves without
// a network call. Uses the same on-disk shape clientinfo.Lookup reads.
func seedClientInfoCache(t *testing.T, info clientinfo.Info) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("HOME", dir)

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no user cache dir on this platform")
	}

	path := filepath.Join(cacheDir, "voodu", "client_info.json")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(map[string]any{"fetched_at": time.Now(), "info": info})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// THE regression this file exists for.
//
// The client attribution was first wired into ONE call site — and it was the
// wrong one: `vd apply` is routed through runApplyForwarded before the generic
// forwarder is ever reached, so every apply recorded nothing while the code
// looked correct. Pinning it to remoteEnv, which every action-forwarding path
// calls, is what makes that unrepeatable; this test pins the pinning.
func TestRemoteEnvCarriesTheClientAttribution(t *testing.T) {
	seedClientInfoCache(t, clientinfo.Info{IP: "189.4.22.10", City: "Sao Paulo", Country: "BR"})

	env := remoteEnv()

	encoded, ok := env[clientinfo.EnvKey]
	if !ok {
		t.Fatalf("%s missing from the forwarded environment: %v", clientinfo.EnvKey, env)
	}

	if got := clientinfo.Decode(encoded); got.IP != "189.4.22.10" {
		t.Fatalf("decoded %+v", got)
	}
}

func TestWithFilesEncodesThePathsTheRewriteDestroys(t *testing.T) {
	env := withFiles(map[string]string{}, []string{"infra/db.hcl", "infra/web app.hcl"})

	raw, ok := env[FilesEnv]
	if !ok {
		t.Fatal("no file list in the forwarded environment")
	}

	// It rides as a shell word in the forwarded command; a path holding a
	// space must not split the argv.
	if strings.ContainsAny(raw, " '\"") {
		t.Fatalf("encoded file list is not shell safe: %q", raw)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}

	if string(decoded) != "infra/db.hcl,infra/web app.hcl" {
		t.Fatalf("decoded %q", decoded)
	}
}

func TestWithFilesIsCapped(t *testing.T) {
	many := make([]string, maxForwardedFiles*3)
	for i := range many {
		many[i] = "f.hcl"
	}

	env := withFiles(map[string]string{}, many)

	decoded, err := base64.RawURLEncoding.DecodeString(env[FilesEnv])
	if err != nil {
		t.Fatal(err)
	}

	if n := len(strings.Split(string(decoded), ",")); n != maxForwardedFiles {
		t.Fatalf("carried %d paths, want %d", n, maxForwardedFiles)
	}
}

func TestWithFilesAddsNothingForAnEmptyList(t *testing.T) {
	if _, ok := withFiles(map[string]string{}, nil)[FilesEnv]; ok {
		t.Fatal("an empty file list still set the variable")
	}
}

// The forwarded apply and delete are the two commands that carry a manifest
// stream, and both are routed through their own orchestrator rather than the
// generic forwarder — which is exactly how the file list got missed the first
// time. Read the source and require them to build their env through withFiles.
func TestManifestForwardingPathsCarryTheFileList(t *testing.T) {
	for _, file := range []string{"apply_forwarded.go", "delete_forwarded.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(src), "withFiles(remoteEnv()") {
			t.Errorf("%s forwards a manifest stream without carrying the operator's -f list "+
				"— see withFiles in forward_remote.go", file)
		}
	}
}
