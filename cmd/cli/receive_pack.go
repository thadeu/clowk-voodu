package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/deploy"
	"go.voodu.clowk.in/internal/progress"
)

// newReceivePackCmd is the server-side entry point for commitless
// deploys. The CLI on the developer's laptop pipes a gzipped tarball of
// the build context over SSH; this command reads it from stdin and runs
// the normal build/swap/container pipeline against it.
//
// Invoked as:
//
//	voodu receive-pack <scope>/<name>     # two-label form (preferred)
//	voodu receive-pack <name>             # legacy single-label, empty
//	                                        scope — AppID collapses to name
//
// The scope shapes the on-host identity: the release dir, env file,
// image tag, and container slots are all keyed by AppID(scope, name)
// (= "<scope>-<name>"), so two different scopes can deploy the same
// logical name without fighting over disk or docker. Hidden from
// `voodu --help` because normal users never invoke it by hand — the
// CLI drives it via SSH from `voodu apply`.
func newReceivePackCmd() *cobra.Command {
	var (
		force      bool
		specBase64 string
	)

	cmd := &cobra.Command{
		Use:    "receive-pack <scope>/<name>",
		Short:  "Internal: ingest a tarball build context over SSH and deploy",
		Long: `Reads a gzipped tar of the build context from stdin, extracts it to
a content-addressed release directory, and runs the build/swap/container
pipeline. This is the commitless-deploy counterpart of git's receive-pack
— the CLI pipes a tar here over SSH; no git commit required.

This command is plumbing. The supported user entry point is:

    voodu apply -f voodu.hcl

which detects build-mode deployments and streams the tarball for you.

Dedup: the build-id is the sha256 of the tarball content. A second
invocation with an identical tree skips rebuild and just repoints
'current'. Pass --force to rebuild anyway.

Build configuration (lang, go_version, dockerfile, build args, …)
arrives via --spec (base64-encoded JSON of the build-mode subset of
the deployment/statefulset spec). The CLI ships it inline so the
build pipeline doesn't need a controller round-trip to learn what
--build-arg values to pass docker. When --spec is absent (older
CLIs, manual receive-pack invocations), the pipeline falls back to
fetching the spec from the local controller and finally to
auto-detection if neither resolves.`,
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, name, err := parseScopedRef(args[0])
			if err != nil {
				return err
			}

			spec, err := resolveReceivePackSpec(cmd, scope, name, specBase64)
			if err != nil {
				return err
			}

			// Reporter picks JSON iff the client set VOODU_PROTOCOL to
			// the current wire version in the SSH env. Hello() lands
			// as the first line — NDJSON clients confirm the handshake
			// and pivot to structured rendering; legacy clients stay
			// on the existing text banner path (Hello is a no-op there).
			reporter := progress.NewReporterFromEnv(os.Stdout)
			reporter.Hello()

			defer reporter.Close()

			return deploy.RunFromTarball(controller.AppID(scope, name), os.Stdin, deploy.Options{
				LogWriter: os.Stdout,
				Reporter:  reporter,
				Force:     force,
				Spec:      spec,
			})
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "rebuild even if a release with the same content hash already exists")
	cmd.Flags().StringVar(&specBase64, "spec", "", "base64-encoded JSON of the deployment build spec (CLI-driven; skips controller FetchSpec)")

	return cmd
}

// resolveReceivePackSpec picks the build spec for this receive-pack
// invocation. Precedence:
//
//  1. --spec (inline JSON shipped by the CLI alongside the tarball) —
//     authoritative when present. Lifts the chicken-and-egg between
//     Phase 2 (build) and Phase 3 (apply) in runApplyForwarded: the
//     CLI already has the parsed manifest in hand, so it ships it
//     directly instead of relying on a controller round-trip.
//
//  2. FetchSpec against the local controller — back-compat for older
//     CLIs that don't pass --spec, and for direct receive-pack
//     invocations during debugging. Still returns (nil, nil) when
//     nothing is stored yet, which the build pipeline treats as
//     "auto-detect from release contents".
func resolveReceivePackSpec(cmd *cobra.Command, scope, name, specBase64 string) (*deploy.Spec, error) {
	if specBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(specBase64)
		if err != nil {
			return nil, fmt.Errorf("decode --spec: %w", err)
		}

		spec, err := deploy.SpecFromCLIJSON(raw)
		if err != nil {
			return nil, err
		}

		return spec, nil
	}

	spec, err := deploy.FetchSpec(controllerURL(cmd.Root()), scope, name)
	if err != nil {
		// Don't fall back silently — the operator needs to see
		// that the build-config source of truth is broken.
		return nil, fmt.Errorf("fetch deployment spec from controller: %w", err)
	}

	return spec, nil
}

// parseScopedRef splits "<scope>/<name>" into its two parts. A bare
// "<name>" is accepted with an empty scope for legacy callers. Leading
// and trailing slashes are rejected because they usually signal a bug
// in the caller (accidental absolute path, empty template variable).
func parseScopedRef(ref string) (scope, name string, err error) {
	if ref == "" {
		return "", "", fmt.Errorf("receive-pack: ref is required")
	}

	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return "", "", fmt.Errorf("receive-pack: malformed ref %q", ref)
	}

	parts := strings.Split(ref, "/")

	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("receive-pack: malformed ref %q", ref)
		}

		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("receive-pack: ref must be <scope>/<name>, got %q", ref)
	}
}
