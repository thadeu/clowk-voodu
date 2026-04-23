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
// scope/name the server stores this deployment under, and which
// directory on the dev machine is the build context. Populated from
// the parsed manifest on the client side — that way ${VAR} expansion
// happens before scope/name reach the server.
type buildModeDep struct {
	Scope string
	Name  string
	Path  string
}

// streamResult is what rewriteForStdinStream returns: the rewritten
// argv, the serialized manifests ready for SSH stdin, and the list of
// build-mode deployments whose source must reach the server before the
// controller reconciles (empty when every deployment in the apply is
// image-mode).
type streamResult struct {
	args             []string
	stdin            io.Reader
	buildModeDeploys []buildModeDep
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

	mans, err := loadManifests(applyFlags{files: paths, format: formatSet})
	if err != nil {
		return streamResult{}, err
	}

	body, err := json.Marshal(mans)
	if err != nil {
		return streamResult{}, fmt.Errorf("marshal manifests: %w", err)
	}

	rest = append(rest, "-f", "-", "--format", "json")

	result := streamResult{
		args:  rest,
		stdin: bytes.NewReader(body),
	}

	if cmdName == "apply" {
		result.buildModeDeploys = extractBuildModeDeploys(mans)
	}

	return result, nil
}

// extractBuildModeDeploys returns one entry per deployment whose Image
// field is empty — those are the manifests that need source on the
// server before the controller can reconcile. The returned Path is
// already defaulted ("." for root), so callers don't repeat the
// defaulting logic.
func extractBuildModeDeploys(mans []controller.Manifest) []buildModeDep {
	var out []buildModeDep

	for _, m := range mans {
		if m.Kind != controller.KindDeployment {
			continue
		}

		var spec manifest.DeploymentSpec

		if err := json.Unmarshal(m.Spec, &spec); err != nil {
			continue
		}

		if spec.Image != "" {
			continue
		}

		path := spec.Path
		if path == "" {
			path = "."
		}

		out = append(out, buildModeDep{
			Scope: m.Scope,
			Name:  m.Name,
			Path:  path,
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

