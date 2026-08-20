package docker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/paths"
)

// TestUseVooduDockerConfigExportsEnv is the whole point of the
// function: DOCKER_CONFIG has to name a directory the sandboxed
// controller can actually write, because ~/.docker is neither
// writable nor readable under ProtectHome=yes.
func TestUseVooduDockerConfigExportsEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv("HOME", t.TempDir())

	dir, seeded, err := UseVooduDockerConfig()
	if err != nil {
		t.Fatalf("UseVooduDockerConfig: %v", err)
	}

	want := filepath.Join(root, "docker")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}

	if got := os.Getenv(EnvDockerConfig); got != want {
		t.Errorf("%s = %q, want %q", EnvDockerConfig, got, want)
	}

	if seeded {
		t.Errorf("seeded = true with an empty HOME")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}

	// 0750, not 0700: `voodu receive-pack` runs as the SSH remote's
	// ordinary user (real servers do not allow SSH as root) and needs
	// to read these auths to pull a private base image. That user is
	// necessarily in the docker group — it could not run docker at all
	// otherwise — and a docker-group member can already read every
	// byte on the host through the socket. Still off-limits to
	// everyone else: no world bit.
	perm := info.Mode().Perm()
	if perm != 0750 {
		t.Errorf("dir perm = %04o, want 0750", perm)
	}

	if perm&0007 != 0 {
		t.Errorf("dir perm = %04o, must not be world-accessible", perm)
	}
}

// TestUseVooduDockerConfigECRCacheIsPerUID — two processes share this
// tree (root controller, docker-group build user) and the ECR helper
// writes its cache 0600. A single shared cache directory would leave
// whichever process wrote second unable to read the other's file, so
// each uid gets its own.
func TestUseVooduDockerConfigECRCacheIsPerUID(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv(EnvECRCacheDir, "")
	t.Setenv("HOME", t.TempDir())

	if _, _, err := UseVooduDockerConfig(); err != nil {
		t.Fatalf("UseVooduDockerConfig: %v", err)
	}

	want := filepath.Join(root, "docker", "ecr-cache", strconv.Itoa(os.Geteuid()))

	if got := os.Getenv(EnvECRCacheDir); got != want {
		t.Errorf("%s = %q, want the per-uid dir %q", EnvECRCacheDir, got, want)
	}

	// The shared parent must let the unprivileged build user create
	// its own subdirectory.
	parent, err := os.Stat(filepath.Join(root, "docker", "ecr-cache"))
	if err != nil {
		t.Fatal(err)
	}

	if perm := parent.Mode().Perm(); perm != 0770 {
		t.Errorf("ecr-cache parent perm = %04o, want 0770", perm)
	}

	// The per-uid dir holds live tokens and is not shared at all.
	mine, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}

	if perm := mine.Mode().Perm(); perm != 0700 {
		t.Errorf("per-uid cache perm = %04o, want 0700", perm)
	}
}

// TestSeededConfigIsGroupReadable — the upgrade seed lands a file the
// build-side user has to read too, same as every subsequent
// RegistryHandler write.
func TestSeededConfigIsGroupReadable(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv("HOME", home)

	if err := os.MkdirAll(filepath.Join(home, ".docker"), 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"), []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}

	dir, seeded, err := UseVooduDockerConfig()
	if err != nil || !seeded {
		t.Fatalf("seed failed: seeded=%v err=%v", seeded, err)
	}

	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	perm := info.Mode().Perm()
	if perm&0040 == 0 {
		t.Errorf("seeded config perm = %04o, want group-readable", perm)
	}

	if perm&0007 != 0 {
		t.Errorf("seeded config perm = %04o, must not be world-accessible", perm)
	}
}

// TestUseVooduDockerConfigRedirectsECRCache covers the EC2
// instance-role path. `docker-credential-ecr-login` needs no static
// credential — it reads the role off IMDS — but it caches issued
// tokens under ${HOME}/.ecr, which ProtectHome=yes makes unwritable.
// Without the redirect the helper fails on its cache write, and the
// error reads like an AWS problem rather than a sandbox one.
func TestUseVooduDockerConfigRedirectsECRCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv(EnvECRCacheDir, "")
	t.Setenv("HOME", t.TempDir())

	if _, _, err := UseVooduDockerConfig(); err != nil {
		t.Fatalf("UseVooduDockerConfig: %v", err)
	}

	got := os.Getenv(EnvECRCacheDir)

	// Anywhere under VOODU_ROOT is the point — the exact per-uid shape
	// is pinned by TestUseVooduDockerConfigECRCacheIsPerUID. What must
	// never hold is a path under $HOME, which ProtectHome=yes makes
	// unwritable for the whole service cgroup.
	if !strings.HasPrefix(got, filepath.Join(root, "docker", "ecr-cache")) {
		t.Errorf("%s = %q, want it under the voodu-owned tree", EnvECRCacheDir, got)
	}

	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat %s: %v", got, err)
	}

	if !info.IsDir() {
		t.Errorf("%s is not a directory", got)
	}
}

// TestUseVooduDockerConfigSeedsFromHome guards the upgrade path: an
// operator who ran `docker login` as root before this change must not
// silently lose those credentials when the lookup path moves.
func TestUseVooduDockerConfigSeedsFromHome(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv("HOME", home)

	body := `{"auths":{"ghcr.io":{"auth":"dXNlcjp0b2tlbg=="}}}`

	if err := os.MkdirAll(filepath.Join(home, ".docker"), 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	dir, seeded, err := UseVooduDockerConfig()
	if err != nil {
		t.Fatalf("UseVooduDockerConfig: %v", err)
	}

	if !seeded {
		t.Errorf("seeded = false, want true")
	}

	got, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}

	if string(got) != body {
		t.Errorf("seeded config = %q, want %q", got, body)
	}
}

// TestUseVooduDockerConfigDoesNotClobber — the seed is a one-time
// upgrade convenience, not a sync. Once RegistryHandler owns the file,
// a stale ~/.docker must never overwrite it on the next boot.
func TestUseVooduDockerConfigDoesNotClobber(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv("HOME", home)

	if err := os.MkdirAll(filepath.Join(root, "docker"), 0700); err != nil {
		t.Fatal(err)
	}

	current := `{"auths":{"registry.example.com":{"auth":"Y3VycmVudA=="}}}`

	if err := os.WriteFile(filepath.Join(root, "docker", "config.json"), []byte(current), 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(home, ".docker"), 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"), []byte(`{"auths":{"stale":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	_, seeded, err := UseVooduDockerConfig()
	if err != nil {
		t.Fatalf("UseVooduDockerConfig: %v", err)
	}

	if seeded {
		t.Errorf("seeded = true over an existing config")
	}

	got, err := os.ReadFile(filepath.Join(root, "docker", "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != current {
		t.Errorf("config was clobbered: %q", got)
	}
}

// TestUseVooduDockerConfigSkipsUnreadableConfig is the regression that
// broke build-mode deploys: the controller creates the directory 0700
// as root, `voodu receive-pack` runs as the SSH remote's user, and
// pointing a non-root build at a root-owned credential file does not
// degrade gracefully — docker fails to load the config, loses its
// CLI-plugin discovery with it, and `docker build -f ...` dies with
// "unknown shorthand flag: 'f' in -f", naming a flag that was never
// the problem.
//
// Leaving DOCKER_CONFIG unset falls back to docker's default, which is
// what every build did before the variable existed.
func TestUseVooduDockerConfigSkipsUnreadableConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks (CAP_DAC_OVERRIDE)")
	}

	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv("HOME", t.TempDir())

	dir := filepath.Join(root, "docker")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}

	// Simulate "owned by another user": unreadable to us.
	if err := os.Chmod(cfg, 0000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(cfg, 0600) })

	_, _, err := UseVooduDockerConfig()
	if err == nil {
		t.Fatal("adopted an unreadable config — docker would fail to load it and lose plugin discovery")
	}

	if got := os.Getenv(EnvDockerConfig); got != "" {
		t.Errorf("%s = %q, want it left unset so docker uses its default", EnvDockerConfig, got)
	}
}

// TestUseVooduDockerConfigAdoptsMissingConfig — an absent file is not
// a failure. Nothing has written credentials yet, and docker treats a
// missing config exactly as the default path would.
func TestUseVooduDockerConfigAdoptsMissingConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)
	t.Setenv(EnvDockerConfig, "")
	t.Setenv("HOME", t.TempDir())

	dir, _, err := UseVooduDockerConfig()
	if err != nil {
		t.Fatalf("UseVooduDockerConfig: %v", err)
	}

	if os.Getenv(EnvDockerConfig) != dir {
		t.Errorf("%s = %q, want %q", EnvDockerConfig, os.Getenv(EnvDockerConfig), dir)
	}
}
