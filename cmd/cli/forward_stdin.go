package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

// manifestStreamCmds are the commands whose `-f PATH` arguments reference
// local files. When forwarding over SSH, we parse the files on the dev
// machine (so ${VAR} expands against the developer's env) and hand the
// result to the remote as a JSON array on stdin.
var manifestStreamCmds = map[string]bool{
	"apply":  true,
	"diff":   true,
	"delete": true,
}

// buildModeDep captures the minimum a source-push step needs: which
// scope/name the server stores this deployment under, which directory
// on the dev machine is the build context, and the parsed manifest's
// spec JSON (shipped to receive-pack via --spec so the build sees
// the right BuildArgs / Dockerfile / Context BEFORE Phase 3 has
// persisted the manifest in the controller). Populated from the
// parsed manifest on the client side — that way ${VAR} expansion
// happens before scope/name reach the server.
type buildModeDep struct {
	Scope    string
	Name     string
	Path     string
	SpecJSON json.RawMessage
}

// streamResult is what rewriteForStdinStream returns: the rewritten
// argv, the serialized manifests ready for SSH stdin, and the list of
// build-mode deployments whose source must reach the server before the
// controller reconciles (empty when every deployment in the apply is
// image-mode).
//
// manifests is the parsed list of every -f input — handed back so
// client-side orchestrators (apply, delete) can render plans without
// re-decoding the JSON body. Populated whenever rewriteForStdinStream
// actually parsed files; nil for the as-is path (stdin pipe, no -f).
type streamResult struct {
	args             []string
	stdin            io.Reader
	buildModeDeploys []buildModeDep
	manifests        []controller.Manifest
}

// rewriteForStdinStream inspects argv for a manifest-consuming command;
// if it finds one with file references, it returns a new argv that says
// `-f - --format json` plus a reader carrying the serialized manifests.
//
// A nil stdin means "forward the argv as-is" — no rewrite was needed.
func rewriteForStdinStream(args []string) (streamResult, error) {
	cmdIdx := findPrimaryCommand(args)
	if cmdIdx < 0 {
		return streamResult{args: args}, nil
	}

	cmdName := args[cmdIdx]

	if !manifestStreamCmds[cmdName] {
		return streamResult{args: args}, nil
	}

	paths, formatSet, rest := splitFileAndFormatFlags(args)

	if len(paths) == 0 {
		// No `-f` at all — let the remote error normally with its own
		// "at least one -f is required" message.
		return streamResult{args: args}, nil
	}

	// If every reference is already `-`, the user is piping their own
	// stream. Respect it: don't consume stdin twice.
	allStdin := true

	for _, p := range paths {
		if p != "-" {
			allStdin = false
			break
		}
	}

	if allStdin {
		return streamResult{args: args}, nil
	}

	if formatSet == "" {
		formatSet = "hcl"
	}

	// nil cmd: the SSH-forward path runs before cobra parses args
	// (it's the argv rewriter). env_from bucket enrichment is
	// skipped here — operator-supplied `${VAR}` in the manifest
	// still resolves against the LOCAL shell. For remote applies
	// that want bucket-sourced interpolation, the operator points
	// the controller URL at the remote and runs without -r so the
	// local apply path (which does have cmd) is taken. SSH-forward
	// for env_from is a deferred milestone.
	mans, err := loadManifests(nil, applyFlags{files: paths, format: formatSet})
	if err != nil {
		return streamResult{}, err
	}

	body, err := json.Marshal(mans)
	if err != nil {
		return streamResult{}, fmt.Errorf("marshal manifests: %w", err)
	}

	rest = append(rest, "-f", "-", "--format", "json")

	result := streamResult{
		args:      rest,
		stdin:     bytes.NewReader(body),
		manifests: mans,
	}

	if cmdName == "apply" {
		result.buildModeDeploys = extractBuildModeDeploys(mans)
	}

	return result, nil
}

// extractBuildModeDeploys returns one entry per build-capable manifest
// (deployment OR statefulset) whose Image field is empty — those are
// the manifests that need source on the server before the controller
// can reconcile. The returned Path is already defaulted ("." for
// root), so callers don't repeat the defaulting logic. Path is
// CWD-relative: the build context is whatever directory the operator
// invoked `voodu apply` from, matching the mental model of
// docker/podman (`docker build .` = "here").
//
// Statefulset is included because postgres/redis/etc. operators may
// want to bake extensions (pgvector, RediSearch) into a custom image
// without a separate CI — same build {} block surface as deployment,
// same receive-pack pipeline.
func extractBuildModeDeploys(mans []controller.Manifest) []buildModeDep {
	var out []buildModeDep

	for _, m := range mans {
		var (
			image string
			build *manifest.BuildSpec
		)

		switch m.Kind {
		case controller.KindDeployment:
			var spec manifest.DeploymentSpec

			if err := json.Unmarshal(m.Spec, &spec); err != nil {
				continue
			}

			image = spec.Image
			build = spec.Build

		case controller.KindStatefulset:
			var spec manifest.StatefulsetSpec

			if err := json.Unmarshal(m.Spec, &spec); err != nil {
				continue
			}

			image = spec.Image
			build = spec.Build

		default:
			continue
		}

		if image != "" {
			continue
		}

		// build.context is what the CLI streams as the build tarball
		// root. Falls back to "." when the manifest declared neither
		// `image` nor `build {}` — applyDefaults synthesises that case
		// already, but we belt-and-suspenders here so a malformed
		// manifest sneaking in via stdin doesn't produce a zero-path.
		path := "."
		if build != nil && build.Context != "" {
			path = build.Context
		}

		out = append(out, buildModeDep{
			Scope:    m.Scope,
			Name:     m.Name,
			Path:     path,
			SpecJSON: m.Spec,
		})
	}

	return out
}

// findPrimaryCommand returns the index of the first non-flag token, the
// thing Cobra would dispatch on. Flag values (`-o json`, `--remote foo`)
// are skipped so we land on the actual verb.
func findPrimaryCommand(args []string) int {
	for i := 0; i < len(args); i++ {
		tok := args[i]

		if !strings.HasPrefix(tok, "-") {
			return i
		}

		if strings.Contains(tok, "=") {
			continue
		}

		if takesValue(tok) && i+1 < len(args) {
			i++
		}
	}

	return -1
}

// splitFileAndFormatFlags pulls every -f/--file value and the last
// --format value out of args, returning the collected paths, the format
// string, and the remaining argv. We strip these because the rewrite
// replaces them with a single `-f -` + `--format json`.
func splitFileAndFormatFlags(args []string) (paths []string, format string, rest []string) {
	rest = make([]string, 0, len(args))

	i := 0
	for i < len(args) {
		tok := args[i]

		switch {
		case tok == "-f" || tok == "--file":
			if i+1 < len(args) {
				paths = append(paths, args[i+1])
				i += 2

				continue
			}

			i++

		case strings.HasPrefix(tok, "-f="):
			paths = append(paths, strings.TrimPrefix(tok, "-f="))
			i++

		case strings.HasPrefix(tok, "--file="):
			paths = append(paths, strings.TrimPrefix(tok, "--file="))
			i++

		case tok == "--format":
			if i+1 < len(args) {
				format = args[i+1]
				i += 2

				continue
			}

			i++

		case strings.HasPrefix(tok, "--format="):
			format = strings.TrimPrefix(tok, "--format=")
			i++

		default:
			rest = append(rest, tok)
			i++
		}
	}

	return paths, format, rest
}

