package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/util"
)

type Golang struct{}

// block normalises reading from spec.Lang — every accessor tolerates a
// nil pointer so the handler runs with pure defaults when the HCL omits
// `lang {}` altogether.
func (l *Golang) block(spec *BuildSpec) *LangBuildSpec {
	if spec != nil && spec.Lang != nil {
		return spec.Lang
	}

	return &LangBuildSpec{}
}

func (l *Golang) Build(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Building Go application...")

	if spec.Image != "" && util.IsRegistryImage(spec.Image, util.GetCustomRegistries(appName)) {
		fmt.Println("-----> Using pre-built image from registry...")

		if err := util.PullRegistryImage(spec.Image); err != nil {
			return fmt.Errorf("failed to pull pre-built image: %v", err)
		}

		if err := util.TagImageForApp(spec.Image, appName); err != nil {
			return fmt.Errorf("failed to tag image: %v", err)
		}

		fmt.Println("-----> Pre-built image ready for deployment!")

		return nil
	}

	if err := l.EnsureDockerfile(releaseDir, appName, spec); err != nil {
		return fmt.Errorf("failed to ensure Dockerfile: %v", err)
	}

	imageTag := fmt.Sprintf("%s:latest", appName)
	buildArgs := l.buildArgs(spec)

	var cmd *exec.Cmd
	if spec.Dockerfile != "" {
		dockerfilePath := filepath.Join(releaseDir, spec.Dockerfile)

		if spec.Workdir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, spec.Workdir, spec.Dockerfile)

			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				dockerfilePath = workdirDockerfilePath
			}
		}

		cmd = exec.Command("docker", "build", "--progress=plain", "-f", dockerfilePath, "-t", imageTag, releaseDir)

		fmt.Printf("-----> Using custom Dockerfile: %s\n", dockerfilePath)
	} else {
		cmd = exec.Command("docker", "build", "--progress=plain", "-t", imageTag, releaseDir)
	}

	for key, value := range buildArgs {
		cmd.Args = append(cmd.Args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
	}

	for _, label := range docker.GetVooduLabels() {
		cmd.Args = append(cmd.Args, "--label", label)
	}

	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := util.RunDockerBuildWithTimeout(cmd, 60); err != nil {
		return err
	}

	fmt.Println("-----> Go build complete!")

	return nil
}

func (l *Golang) Deploy(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Deploying Go application...")

	return deployContainer(appName, spec, releaseDir)
}

func (l *Golang) Restart(appName string, spec *BuildSpec) error {
	return restartContainer(appName)
}

func (l *Golang) Cleanup(appName string, spec *BuildSpec) error {
	return cleanupReleases(appName)
}

func (l *Golang) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "go.mod")); err == nil {
		return "go", nil
	}

	return "", fmt.Errorf("not a Go project")
}

func (l *Golang) EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error {
	if spec.Dockerfile != "" {
		customDockerfilePath := filepath.Join(releaseDir, spec.Dockerfile)

		if _, err := os.Stat(customDockerfilePath); err == nil {
			fmt.Printf("-----> Using custom Dockerfile: %s\n", spec.Dockerfile)

			return nil
		}

		if spec.Workdir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, spec.Workdir, spec.Dockerfile)

			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				fmt.Printf("-----> Using custom Dockerfile in workdir: %s/%s\n", spec.Workdir, spec.Dockerfile)

				return nil
			}
		}

		return fmt.Errorf("custom Dockerfile not found: %s or %s", customDockerfilePath, filepath.Join(releaseDir, spec.Workdir, spec.Dockerfile))
	}

	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")

		return nil
	}

	fmt.Println("-----> Generating Dockerfile for Go...")

	workDir := "."

	if spec.Workdir != "" {
		workDir = spec.Workdir
	}

	buildPath := "."

	if spec.Path != "" {
		buildPath = "./" + strings.TrimPrefix(spec.Path, "./")
	}

	dockerfileContent := l.generateDockerfile(spec, workDir, buildPath)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Golang) generateDockerfile(spec *BuildSpec, workDir, buildPath string) string {
	block := l.block(spec)

	baseImage := spec.Image

	if baseImage == "" {
		if block.Version != "" {
			baseImage = fmt.Sprintf("golang:%s-alpine", block.Version)
		} else {
			baseImage = util.DetectGoVersion(".")
			fmt.Printf("-----> Detected Go version: %s\n", baseImage)
		}
	}

	args := l.buildArgs(spec)

	fmt.Printf("-----> Final build config: GOOS=%s GOARCH=%s CGO_ENABLED=%s\n", args["GOOS"], args["GOARCH"], args["CGO_ENABLED"])

	return fmt.Sprintf(`# Generated Dockerfile for Go application
FROM %s AS builder

WORKDIR /app

# Copy only go.mod and go.sum first (for better Docker layer caching)
COPY %s/go.mod %s/go.sum* ./

# Download dependencies with cache mount (this layer will be cached if go.mod/go.sum don't change)
RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download

# Copy the rest of the application code
COPY %s .

# Build the application with cache mounts for faster builds
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=%s GOOS=%s GOARCH=%s \
  go build -ldflags="-w -s" -o app %s

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates \
  tzdata sox lame \
  && rm -rf /var/cache/apk/*

WORKDIR /root/

# Copy binary from builder
COPY --from=builder /app/app .

# Run the application
CMD ["/root/app"]
`, baseImage, workDir, workDir, workDir, args["CGO_ENABLED"], args["GOOS"], args["GOARCH"], buildPath)
}

// buildArgs produces the merged --build-arg map for `docker build`.
// Auto-generated args (GOOS, GOARCH, CGO_ENABLED, GO_VERSION) seed the
// map with sensible defaults; user-supplied BuildArgs take precedence
// when a key collides — operators must be able to override even the
// platform defaults when they know better.
func (l *Golang) buildArgs(spec *BuildSpec) map[string]string {
	block := l.block(spec)

	goos := "linux"
	goarch := runtime.GOARCH

	switch goarch {
	case "amd64", "arm64", "386", "arm":
	default:
		goarch = "amd64"
	}

	goVersion := block.Version
	if goVersion == "" {
		goVersion = "1.25"
	}

	out := map[string]string{
		"GOOS":        goos,
		"GOARCH":      goarch,
		"CGO_ENABLED": "0",
		"GO_VERSION":  goVersion,
	}

	for k, v := range block.BuildArgs {
		out[k] = v
	}

	fmt.Printf("-----> Build args: GOOS=%s GOARCH=%s CGO_ENABLED=%s GO_VERSION=%s\n", out["GOOS"], out["GOARCH"], out["CGO_ENABLED"], out["GO_VERSION"])

	return out
}
