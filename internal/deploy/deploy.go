// Package deploy orchestrates a single app release after its source has
// landed on disk: build the image, swap the `current` symlink, run
// post-deploy hooks, and restart the container.
//
// Source lands via RunFromTarball in tarball.go — the CLI streams a
// gzipped build context over SSH into `voodu receive-pack`, which
// extracts to a content-addressed release dir and then drives the
// pipeline below.
//
// Build-time configuration (lang, go_version, dockerfile, post_deploy,
// etc.) reaches this package through the controller: receive-pack
// fetches the DeploymentSpec from the local controller HTTP API and
// hands it to RunFromTarball via Options.Spec. When absent (legacy
// callers or brand-new app with no manifest yet), the pipeline falls
// back to zero-value defaults and auto-detects the language from the
// extracted release tree.
package deploy

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"go.voodu.clowk.in/internal/lang"
	"go.voodu.clowk.in/internal/paths"
)

// Options tune a deploy. Zero value is fine (uses sensible defaults).
type Options struct {
	// LogWriter receives human-readable progress lines. Defaults to os.Stdout.
	LogWriter io.Writer

	// Force bypasses content-hash dedup: even if a release with the
	// incoming build-id already exists, rebuild from scratch.
	Force bool

	// Spec is the DeploymentSpec fetched from the controller for this
	// app. It carries both build-time inputs (consumed by lang handlers
	// via Spec.toBuildSpec) and pipeline-only fields (PostDeploy,
	// KeepReleases). Nil is valid — the pipeline falls back to
	// auto-detection with zero-value defaults.
	Spec *Spec
}

func (o *Options) log(format string, args ...any) {
	w := o.LogWriter

	if w == nil {
		w = os.Stdout
	}

	fmt.Fprintf(w, format+"\n", args...)
}

// runPipeline executes every stage *after* source has landed in releaseDir.
func runPipeline(app, releaseDir string, opts *Options) error {
	spec := opts.Spec
	if spec == nil {
		spec = &Spec{}
	}

	build := spec.toBuildSpec()

	if err := buildRelease(app, releaseDir, build, opts); err != nil {
		return fmt.Errorf("build release: %w", err)
	}

	if err := swapCurrentSymlink(app, releaseDir); err != nil {
		return fmt.Errorf("swap current symlink: %w", err)
	}

	if err := runPostDeploy(releaseDir, spec, opts); err != nil {
		opts.log("-----> Warning: post-deploy hook failed: %v", err)
	}

	opts.log("-----> Deploy completed successfully for '%s'", app)

	return nil
}

// buildRelease turns the extracted release directory into a Docker image
// tagged `<app>:latest`. Dispatch goes through the language-specific
// handlers in internal/lang — they own Dockerfile generation, build-args,
// and the `docker build` invocation itself (BuildKit enabled, labelled
// with Voodu metadata via internal/docker.GetVooduLabels).
//
// Resolution order:
//   - spec.Lang.Name non-empty → use that handler.
//   - spec.Image set to a registry image → pull + retag instead of build.
//   - otherwise → auto-detect language from release contents.
func buildRelease(appName, releaseDir string, spec *lang.BuildSpec, opts *Options) error {
	opts.log("-----> Building release...")

	handler, err := lang.NewLang(spec, releaseDir)
	if err != nil {
		return fmt.Errorf("select language handler: %w", err)
	}

	if err := handler.Build(appName, spec, releaseDir); err != nil {
		return err
	}

	opts.log("-----> Build completed")

	return nil
}

func swapCurrentSymlink(app, releaseDir string) error {
	link := paths.AppCurrentLink(app)
	_ = os.Remove(link)

	return os.Symlink(releaseDir, link)
}

// runPostDeploy runs spec.PostDeploy commands (if any) with the release
// dir as CWD. Each command runs through `bash -c` so shell features
// (pipes, redirection, `&&`) just work. First failure aborts the
// sequence and surfaces to the caller — the caller logs without
// failing the overall deploy.
func runPostDeploy(releaseDir string, spec *Spec, opts *Options) error {
	if spec == nil || len(spec.PostDeploy) == 0 {
		return nil
	}

	opts.log("-----> Running post-deploy commands...")

	for _, command := range spec.PostDeploy {
		opts.log("       $ %s", command)

		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = releaseDir
		cmd.Stdout = opts.LogWriter
		cmd.Stderr = opts.LogWriter

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("post-deploy %q: %w", command, err)
		}
	}

	return nil
}
