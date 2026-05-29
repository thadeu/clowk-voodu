package lang

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.voodu.clowk.in/internal/util"
)

type Nodejs struct{}

func (l *Nodejs) block(spec *BuildSpec) *LangBuildSpec {
	if spec != nil && spec.Lang != nil {
		return spec.Lang
	}

	return &LangBuildSpec{}
}

// toolchainLabel is the human-facing name for the JS toolchain, used in
// log lines. The handler is internally "Node.js" (it owns every
// package.json project), but `--verbose` should name what actually runs
// so it doesn't say "Node.js" while building a Bun image.
func toolchainLabel(manager string) string {
	switch manager {
	case "bun":
		return "bun"
	case "pnpm":
		return "node.js (pnpm)"
	case "yarn":
		return "node.js (yarn)"
	default:
		return "node.js"
	}
}

func (l *Nodejs) Build(appName string, spec *BuildSpec, releaseDir string) error {
	manager, _ := nodePackageManager(releaseDir)
	fmt.Printf("-----> building %s application...\n", toolchainLabel(manager))

	if spec.Image != "" && util.IsRegistryImage(spec.Image, util.GetCustomRegistries(appName)) {
		fmt.Println("-----> using pre-built image from registry...")

		if err := util.PullRegistryImage(spec.Image); err != nil {
			return fmt.Errorf("failed to pull pre-built image: %v", err)
		}

		if err := util.TagImageForApp(spec.Image, appName); err != nil {
			return fmt.Errorf("failed to tag image: %v", err)
		}

		fmt.Println("-----> pre-built image ready for deployment!")

		return nil
	}

	if err := l.EnsureDockerfile(releaseDir, appName, spec); err != nil {
		return fmt.Errorf("failed to ensure Dockerfile: %v", err)
	}

	if err := runDockerBuild(appName, spec, releaseDir, spec.BuildArgs); err != nil {
		return err
	}

	fmt.Println("-----> node.js build complete!")

	return nil
}

func (l *Nodejs) Deploy(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> deploying node.js application...")
	return deployContainer(appName, spec, releaseDir)
}

func (l *Nodejs) Restart(appName string, spec *BuildSpec) error {
	return restartContainer(appName)
}

func (l *Nodejs) Cleanup(appName string, spec *BuildSpec) error {
	return cleanupReleases(appName)
}

func (l *Nodejs) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "package.json")); err == nil {
		return "nodejs", nil
	}

	return "", fmt.Errorf("not a Node.js project")
}

func (l *Nodejs) EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error {
	found, err := ensureCustomDockerfile(releaseDir, spec)
	if err != nil {
		return err
	}

	if found {
		return nil
	}

	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> using existing dockerfile")
		return nil
	}

	fmt.Println("-----> generating dockerfile...")

	dockerfileContent := l.generateDockerfile(spec, appName, releaseDir)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

// declaredPackageManager reads package.json's `packageManager` field —
// the corepack convention (e.g. "bun@1.1.30", "pnpm@9.0.0"). Returns the
// manager name and version, or ("","") when absent/unparseable. The
// version is cleaned of the optional "+sha…" integrity suffix corepack
// sometimes appends, so it's usable as an image tag.
func declaredPackageManager(releaseDir string) (name, version string) {
	raw, err := os.ReadFile(filepath.Join(releaseDir, "package.json"))
	if err != nil {
		return "", ""
	}

	var pkg struct {
		PackageManager string `json:"packageManager"`
	}

	if json.Unmarshal(raw, &pkg) != nil || pkg.PackageManager == "" {
		return "", ""
	}

	name, version, _ = strings.Cut(pkg.PackageManager, "@")

	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}

	return strings.TrimSpace(name), strings.TrimSpace(version)
}

// nodePackageManager picks the JS toolchain + version for the build. The
// package.json `packageManager` field is the source of truth — it's the
// project's explicit, versioned declaration (and what corepack reads).
// When it's absent we fall back to a conservative lockfile sniff that
// only distinguishes Bun (which needs the oven/bun base image entirely)
// from everything else, which builds on node with a lockfile-agnostic
// `npm install`.
//
// Returns (manager, version): manager ∈ {bun, pnpm, yarn, npm}; version
// is the declared one ("" when unknown). pnpm/yarn activate ONLY when
// declared in the field — a project that merely happens to carry a
// pnpm-lock.yaml keeps the universal npm install path, so existing
// deployments don't silently change toolchain.
func nodePackageManager(releaseDir string) (manager, version string) {
	switch name, ver := declaredPackageManager(releaseDir); name {
	case "bun", "pnpm", "yarn", "npm":
		return name, ver
	}

	for _, lock := range []string{"bun.lock", "bun.lockb"} {
		if _, err := os.Stat(filepath.Join(releaseDir, lock)); err == nil {
			return "bun", ""
		}
	}

	return "npm", ""
}

func (l *Nodejs) generateDockerfile(spec *BuildSpec, appName, releaseDir string) string {
	block := l.block(spec)

	entrypoint := block.Entrypoint

	if entrypoint == "" {
		entrypoint = "index.js"
	}

	// Toolchain selection is auto-build only — an explicit registry image
	// (spec.Image) always wins and skips Dockerfile generation entirely.
	if spec.Image == "" {
		manager, pmVersion := nodePackageManager(releaseDir)

		switch manager {
		case "bun":
			// `lang { version }` overrides the packageManager field.
			bunVersion := block.Version
			if bunVersion == "" {
				bunVersion = pmVersion
			}

			return l.generateBunDockerfile(appName, entrypoint, bunVersion)

		case "pnpm", "yarn":
			return l.generateCorepackDockerfile(appName, entrypoint, manager, block.Version)
		}
		// "npm" falls through to the node + npm install path below.
	}

	baseImage := spec.Image

	if baseImage == "" {
		baseImage = nodeBaseImage(block.Version)
	}

	return fmt.Sprintf(`# Generated Dockerfile for Node.js application
# App: %s
# Entrypoint: %s

FROM %s

WORKDIR /app

# Enable corepack so pnpm and yarn (v4) sit alongside npm. The image
# installs with npm below, but the Procfile / manifest command is then
# free to run 'pnpm install' or 'yarn install' instead if the app prefers
# one of them. Non-fatal: npm works even on a node build that ships no
# corepack, so a failure here must not break the image.
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable || true

# Install dependencies with 'npm install' (NOT 'npm ci'): a project
# managed by pnpm or bun has no package-lock.json, and 'npm ci' hard-
# fails without one. 'npm install' resolves straight from package.json,
# so any package.json project builds regardless of which lockfile it
# ships. Dev deps are included on purpose — a runtime build step
# (vite/tsc/esbuild) needs its toolchain, and the Procfile command is
# free to run 'npm run build' before serving.
COPY package*.json ./
RUN npm install --no-audit --no-fund

# Copy application source. .dockerignore keeps node_modules/ and any
# prebuilt dist/ out, so the image's deps are the ones just installed.
COPY . .

# Default command — a Procfile line or manifest 'command' overrides it.
CMD ["node", "%s"]
`, appName, entrypoint, baseImage, entrypoint)
}

// nodeBaseImage resolves the node base image: an explicit version (from
// `lang { version }`) pins `node:<v>-alpine`; otherwise the source tree
// is sniffed (.nvmrc / engines) with a sane default.
func nodeBaseImage(version string) string {
	if version != "" {
		return fmt.Sprintf("node:%s-alpine", version)
	}

	img := util.DetectNodeVersion(".")
	fmt.Printf("-----> detected node.js version: %s\n", img)

	return img
}

// generateBunDockerfile emits a Bun-native image for projects that
// declare bun (packageManager field) or ship a bun lockfile. The base is
// oven/bun (Bun's own runtime — there's no node/npm here), install is the
// fast lockfile-aware `bun install`, and the default command runs the
// entrypoint with bun. The Procfile/manifest command still overrides CMD,
// so a static-SPA app would set e.g. `web: bun run build && bun run
// start` — using `bun run`, not npm. version pins the image tag (from the
// packageManager field or `lang { version }`); empty → the v1 major tag.
func (l *Nodejs) generateBunDockerfile(appName, entrypoint, version string) string {
	tag := "1"
	if version != "" {
		tag = version
	}

	baseImage := fmt.Sprintf("oven/bun:%s", tag)

	fmt.Printf("-----> detected bun project — base image: %s\n", baseImage)

	// `bun.lock*` matches BOTH bun.lock (text) and bun.lockb (binary),
	// and nodePackageManager only routes here when one of them exists, so
	// the glob always has a match (no empty-glob COPY failure). package.json
	// is copied alongside for the cached install layer.
	return fmt.Sprintf(`# Generated Dockerfile for Bun application
# App: %s
# Entrypoint: %s

FROM %s

WORKDIR /app

# Bun toolchain (detected via bun.lock/bun.lockb). 'bun install' is the
# fast, lockfile-aware install. All deps install so a runtime build step
# (vite/tsc/esbuild) has its toolchain. oven/bun ships Bun only — use
# 'bun run ...' in the Procfile/command, not npm.
COPY package.json bun.lock* ./
RUN bun install

# Copy application source. .dockerignore keeps node_modules/ and any
# prebuilt dist/ out.
COPY . .

# Default command — a Procfile line or manifest 'command' overrides it.
CMD ["bun", "%s"]
`, appName, entrypoint, baseImage, entrypoint)
}

// generateCorepackDockerfile emits a node image that drives pnpm or yarn
// through corepack. corepack ships with node and reads the exact manager
// version from package.json's `packageManager` field, so the build uses
// the same pnpm/yarn the developer pinned — no version to thread through
// here. This path activates only when `packageManager` declares pnpm/yarn
// (see nodePackageManager), so a project keeps its declared toolchain
// instead of silently falling back to npm.
//
// nodeVersion (from `lang { version }`) pins the node base; empty sniffs
// the tree. The COPY uses a glob on the lockfile so an absent one doesn't
// fail the layer (package.json always matches, so the COPY is non-empty).
func (l *Nodejs) generateCorepackDockerfile(appName, entrypoint, manager, nodeVersion string) string {
	baseImage := nodeBaseImage(nodeVersion)

	lockfile := "pnpm-lock.yaml"
	if manager == "yarn" {
		lockfile = "yarn.lock"
	}

	fmt.Printf("-----> detected %s project (packagemanager) — base image: %s\n", manager, baseImage)

	return fmt.Sprintf(`# Generated Dockerfile for Node.js (%s) application
# App: %s
# Entrypoint: %s

FROM %s

WORKDIR /app

# Enable the project's package manager via corepack. corepack reads the
# exact version from package.json's "packageManager" field. The prompt is
# disabled so the non-interactive build doesn't stall on first download.
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable

# Install with the declared manager. Dev deps included so a runtime build
# step has its toolchain. Use '%s run ...' in the Procfile/command.
COPY package.json %s* ./
RUN %s install

# Copy application source. .dockerignore keeps node_modules/ and any
# prebuilt dist/ out.
COPY . .

# Default command — a Procfile line or manifest 'command' overrides it.
CMD ["node", "%s"]
`, manager, appName, entrypoint, baseImage, manager, lockfile, manager, entrypoint)
}
