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
	"go.voodu.clowk.in/internal/progress"
)

// Options tune a deploy. Zero value is fine (uses sensible defaults).
type Options struct {
	// LogWriter receives human-readable progress lines. Defaults to os.Stdout.
	// Also used as the fallback writer when Reporter is nil.
	LogWriter io.Writer

	// Reporter is the structured progress emitter. When nil, the
	// pipeline synthesises a TextReporter around LogWriter — so
	// existing callers keep their legacy `-----> Banner` output with
	// zero code change. Server entry points that want NDJSON
	// (receive-pack under VOODU_PROTOCOL=ndjson/1) pass a JSONReporter
	// explicitly via progress.NewReporterFromEnv.
	Reporter progress.Reporter

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

// reporter returns the configured Reporter, or synthesises a
// TextReporter around LogWriter (defaulting to os.Stdout) if none was
// set. Centralizes the "lazy default" pattern so every emission site
// reads the same way: `opts.reporter().StepStart(...)`.
func (o *Options) reporter() progress.Reporter {
	if o.Reporter != nil {
		return o.Reporter
	}

	w := o.LogWriter
	if w == nil {
		w = os.Stdout
	}

	// Memoize — multiple calls inside one pipeline would otherwise
	// each build a new TextReporter. Harmless (they're cheap and
	// stateless), but the test buffers check line counts.
	o.Reporter = progress.NewTextReporter(w)

	return o.Reporter
}

// log routes a free-form info message through the active reporter.
// Retained as a convenience for sites that emit plain status lines
// (post-deploy command echo, warnings, buildx sub-output). Structural
// events — step open/close, per-resource results, pipeline summaries —
// should call the reporter directly so NDJSON consumers get proper
// typed frames.
func (o *Options) log(format string, args ...any) {
	o.reporter().Log(progress.LevelInfo, fmt.Sprintf(format, args...))
}

// warn is log's cousin for things that didn't kill the pipeline but
// the operator should still notice (GC failed, post-deploy hook
// crashed). Surfaces as Level=warn on the wire — clients that
// differentiate levels (the NDJSON renderer does; the legacy text
// path doesn't) can highlight these.
func (o *Options) warn(format string, args ...any) {
	o.reporter().Log(progress.LevelWarn, fmt.Sprintf(format, args...))
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
		opts.warn("Warning: post-deploy hook failed: %v", err)
	}

	opts.reporter().Summary(fmt.Sprintf("Deploy completed successfully for '%s'", app))

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
	r := opts.reporter()

	r.StepStart("build", "Building release...")

	handler, err := lang.NewLang(spec, releaseDir)
	if err != nil {
		r.StepEnd("build", progress.StatusFail, err)

		return fmt.Errorf("select language handler: %w", err)
	}

	if err := handler.Build(appName, spec, releaseDir); err != nil {
		r.StepEnd("build", progress.StatusFail, err)

		return err
	}

	r.StepEnd("build", progress.StatusOK, nil)

	// Retain the "Build completed" summary for legacy text clients —
	// their stream_filter uses it as the close marker for the final
	// step and emits the overall `✓ Built <tag> in Ns` line. In
	// NDJSON mode the client synthesises that summary from step_end
	// elapsed time, so the Summary here is harmless but redundant;
	// keeping it keeps a single emission path for both reporters.
	r.Summary("Build completed")

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
//
// Subprocess stdout/stderr go through a LineWriter bridge so each
// output line becomes a reporter.Log event — in text mode that means
// the same plain lines that landed on stdout before; in NDJSON mode
// it prevents the hook's free-form bytes from corrupting the JSON
// wire by interleaving them between our frames.
func runPostDeploy(releaseDir string, spec *Spec, opts *Options) error {
	if spec == nil || len(spec.PostDeploy) == 0 {
		return nil
	}

	r := opts.reporter()

	r.StepStart("post-deploy", "Running post-deploy commands...")

	// One LineWriter reused across every command's stdout AND stderr —
	// the user cares about combined chronological output, not channel
	// separation, and stderr output (errors, warnings) as Log(info) is
	// more conservative than Log(error) because some hooks chatter on
	// stderr without anything actually being wrong.
	sink := progress.NewLineWriter(r, progress.LevelInfo)
	defer sink.Close()

	for _, command := range spec.PostDeploy {
		r.Log(progress.LevelInfo, fmt.Sprintf("       $ %s", command))

		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = releaseDir
		cmd.Stdout = sink
		cmd.Stderr = sink

		if err := cmd.Run(); err != nil {
			r.StepEnd("post-deploy", progress.StatusFail, err)

			return fmt.Errorf("post-deploy %q: %w", command, err)
		}
	}

	r.StepEnd("post-deploy", progress.StatusOK, nil)

	return nil
}
