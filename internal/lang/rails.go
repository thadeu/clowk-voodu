package lang

import (
	"fmt"
	"os"
	"path/filepath"

	"go.voodu.clowk.in/internal/util"
)

type Rails struct{}

func (l *Rails) block(spec *BuildSpec) *LangBuildSpec {
	if spec != nil && spec.Lang != nil {
		return spec.Lang
	}

	return &LangBuildSpec{}
}

func (l *Rails) Build(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Building Rails application...")

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

	if err := runDockerBuild(appName, spec, releaseDir, l.block(spec).BuildArgs); err != nil {
		return err
	}

	fmt.Println("-----> Rails build complete!")

	return nil
}

func (l *Rails) Deploy(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Deploying Rails application...")
	return deployContainer(appName, spec, releaseDir)
}

func (l *Rails) Restart(appName string, spec *BuildSpec) error {
	return restartContainer(appName)
}

func (l *Rails) Cleanup(appName string, spec *BuildSpec) error {
	return cleanupReleases(appName)
}

func (l *Rails) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "Gemfile")); err == nil {
		return "ruby", nil
	}

	return "", fmt.Errorf("not a Rails project")
}

func (l *Rails) EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error {
	found, err := ensureCustomDockerfile(releaseDir, spec)
	if err != nil {
		return err
	}

	if found {
		return nil
	}

	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")
		return nil
	}

	fmt.Println("-----> Generating Dockerfile for Rails...")

	dockerfileContent := l.generateDockerfile(spec, appName)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Rails) generateDockerfile(spec *BuildSpec, appName string) string {
	block := l.block(spec)

	entrypoint := block.Entrypoint

	if entrypoint == "" {
		entrypoint = "app.rb"
	}

	baseImage := spec.Image

	if baseImage == "" {
		if block.Version != "" {
			baseImage = fmt.Sprintf("ruby:%s-alpine", block.Version)
		} else {
			baseImage = util.DetectRubyVersion(".")
			fmt.Printf("-----> Detected Ruby version: %s\n", baseImage)
		}
	}

	return fmt.Sprintf(`# Generated Dockerfile for Rails application
# App: %s
# Entrypoint: %s

FROM %s as builder

WORKDIR /app

# Install dependencies for building native extensions
RUN apt-get update -qq && apt-get install -y build-essential libpq-dev nodejs npm yarn sox libsox-fmt-all lame

COPY Gemfile Gemfile.lock ./
RUN bundle install --jobs 4 --retry 3

COPY . .

# Precompile assets (if using Sprockets or similar)
# For Rails 8 with Propshaft, this step might be different or handled automatically
RUN bin/rails assets:precompile || true

# Stage 2: Production Stage
FROM %s as production

WORKDIR /app

# Install only runtime dependencies
RUN apt-get update -qq && apt-get install -y libpq-dev && rm -rf /var/lib/apt/lists/*

# Copy built application from the builder stage
COPY --from=builder /app /app

# Set environment variables for production
ENV RAILS_ENV=production
ENV BUNDLE_WITHOUT="development test"

# Expose the port your Rails app will listen on
EXPOSE 3000

# Set a non-root user for security
RUN useradd -ms /bin/bash rails
USER rails

# Command to run the Rails server
CMD ["bundle", "exec", "puma", "-C", "config/puma.rb"]
`, appName, entrypoint, baseImage, baseImage)
}
