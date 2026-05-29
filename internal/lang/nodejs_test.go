package lang

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func writePackageJSON(t *testing.T, dir, packageManager string) {
	t.Helper()

	body := `{"name":"x"}`
	if packageManager != "" {
		body = `{"name":"x","packageManager":"` + packageManager + `"}`
	}

	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
}

// nodePackageManager treats the package.json `packageManager` field as
// the source of truth (with its version); absent the field it falls back
// to a conservative lockfile sniff that only distinguishes bun from npm
// (pnpm/yarn lockfiles alone stay on the universal npm install).
func TestNodePackageManager(t *testing.T) {
	cases := []struct {
		name        string
		pkgManager  string   // packageManager field; "" = absent
		extraFiles  []string // additional lockfiles
		wantManager string
		wantVersion string
	}{
		{"field bun with version", "bun@1.1.30", nil, "bun", "1.1.30"},
		{"field bun strips sha suffix", "bun@1.1.30+abc123", nil, "bun", "1.1.30"},
		{"field pnpm", "pnpm@9.0.0", []string{"pnpm-lock.yaml"}, "pnpm", "9.0.0"},
		{"field yarn", "yarn@4.1.0", []string{"yarn.lock"}, "yarn", "4.1.0"},
		{"field npm", "npm@10.0.0", nil, "npm", "10.0.0"},
		{"field wins over lockfile", "bun@1.2.0", []string{"pnpm-lock.yaml"}, "bun", "1.2.0"},
		{"no field, bun lockfile", "", []string{"bun.lock"}, "bun", ""},
		{"no field, bun binary lockfile", "", []string{"bun.lockb"}, "bun", ""},
		{"no field, pnpm lockfile only → npm", "", []string{"pnpm-lock.yaml"}, "npm", ""},
		{"no field, plain", "", nil, "npm", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePackageJSON(t, dir, tc.pkgManager)

			for _, f := range tc.extraFiles {
				writeFile(t, dir, f)
			}

			gotMgr, gotVer := nodePackageManager(dir)
			if gotMgr != tc.wantManager || gotVer != tc.wantVersion {
				t.Errorf("nodePackageManager = (%q,%q), want (%q,%q)", gotMgr, gotVer, tc.wantManager, tc.wantVersion)
			}
		})
	}
}

// A bun lockfile must produce an oven/bun Dockerfile with `bun install`,
// never node/npm.
func TestGenerateDockerfile_Bun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json")
	writeFile(t, dir, "bun.lock")

	n := &Nodejs{}
	df := n.generateDockerfile(&BuildSpec{}, "softphone-web", dir)

	for _, want := range []string{"FROM oven/bun:1", "RUN bun install", `COPY package.json bun.lock* ./`, `CMD ["bun",`} {
		if !strings.Contains(df, want) {
			t.Errorf("bun Dockerfile missing %q\n---\n%s", want, df)
		}
	}

	if strings.Contains(df, "npm ci") || strings.Contains(df, "npm install") {
		t.Errorf("bun Dockerfile must not invoke npm\n---\n%s", df)
	}
}

// The packageManager field alone (no lockfile) routes to bun and pins
// the image tag from the declared version.
func TestGenerateDockerfile_BunFromField(t *testing.T) {
	dir := t.TempDir()
	writePackageJSON(t, dir, "bun@1.1.30")

	n := &Nodejs{}
	df := n.generateDockerfile(&BuildSpec{}, "app", dir)

	if !strings.Contains(df, "FROM oven/bun:1.1.30") || !strings.Contains(df, "RUN bun install") {
		t.Errorf("packageManager:bun should drive the bun image+install\n---\n%s", df)
	}
}

// packageManager: pnpm → corepack-driven node image with pnpm install.
func TestGenerateDockerfile_PnpmCorepack(t *testing.T) {
	dir := t.TempDir()
	writePackageJSON(t, dir, "pnpm@9.0.0")

	n := &Nodejs{}
	df := n.generateDockerfile(&BuildSpec{}, "app", dir)

	for _, want := range []string{"RUN corepack enable", "RUN pnpm install", "COPY package.json pnpm-lock.yaml* ./", "COREPACK_ENABLE_DOWNLOAD_PROMPT=0"} {
		if !strings.Contains(df, want) {
			t.Errorf("pnpm Dockerfile missing %q\n---\n%s", want, df)
		}
	}

	if strings.Contains(df, "oven/bun") || strings.Contains(df, "RUN npm install") {
		t.Errorf("pnpm path must not use bun or npm install\n---\n%s", df)
	}
}

// lang { version } pins the bun image tag.
func TestGenerateDockerfile_BunVersionPin(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json")
	writeFile(t, dir, "bun.lockb")

	n := &Nodejs{}
	df := n.generateDockerfile(&BuildSpec{Lang: &LangBuildSpec{Version: "1.1.30"}}, "app", dir)

	if !strings.Contains(df, "FROM oven/bun:1.1.30") {
		t.Errorf("expected pinned bun tag\n---\n%s", df)
	}
}

// No bun lockfile → the node+npm path, using `npm install` (not npm ci).
func TestGenerateDockerfile_NpmFallback(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json")
	writeFile(t, dir, "pnpm-lock.yaml")

	n := &Nodejs{}
	df := n.generateDockerfile(&BuildSpec{}, "app", dir)

	if !strings.Contains(df, "RUN npm install") {
		t.Errorf("npm fallback should use `npm install`\n---\n%s", df)
	}

	if strings.Contains(df, "RUN npm ci") {
		t.Errorf("must not use `npm ci`\n---\n%s", df)
	}

	// corepack is enabled even on the npm path so the runtime command can
	// opt into pnpm/yarn; non-fatal so a corepack-less node build still works.
	if !strings.Contains(df, "corepack enable") {
		t.Errorf("npm path should still enable corepack for pnpm/yarn opt-in\n---\n%s", df)
	}

	if strings.Contains(df, "oven/bun") {
		t.Errorf("non-bun project must not use the bun image\n---\n%s", df)
	}
}

// An explicit registry image always wins over bun auto-detection.
func TestGenerateDockerfile_ImageWinsOverBun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json")
	writeFile(t, dir, "bun.lock")

	n := &Nodejs{}
	df := n.generateDockerfile(&BuildSpec{Image: "ghcr.io/acme/web:1.2.3"}, "app", dir)

	if !strings.Contains(df, "FROM ghcr.io/acme/web:1.2.3") {
		t.Errorf("explicit image should be the base\n---\n%s", df)
	}

	if strings.Contains(df, "oven/bun") {
		t.Errorf("explicit image must not be overridden by bun detection\n---\n%s", df)
	}
}
