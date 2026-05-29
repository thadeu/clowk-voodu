package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.voodu.clowk.in/internal/procfile"
	"go.voodu.clowk.in/internal/remote"
	"go.voodu.clowk.in/internal/tarball"
)

// procfileApply is the parsed intent of a `vd apply -f Procfile [...]`
// invocation, extracted from the raw argv on the forwarded path (where
// we don't have the cobra applyFlags struct).
type procfileApply struct {
	path    string // resolved path to the Procfile
	scope   string // --app value, or "" → caller generates a random scope
	eject   bool   // --eject: scaffold HCL locally, don't ship
	force   bool   // --force: rebuild even on a content-hash hit
	verbose bool   // -v/--verbose: raw build output
}

// detectProcfileApply scans an apply argv for a Procfile input and the
// procfile-relevant flags. Returns (intent, true) only when the `-f`
// target is a Procfile (basename `Procfile` / `Procfile.*`) or
// `--format procfile` was passed. Everything else falls through to the
// normal HCL path.
func detectProcfileApply(args []string) (procfileApply, bool) {
	var (
		pa       procfileApply
		files    []string
		format   string
		wantProc bool
	)

	for i := 0; i < len(args); i++ {
		tok := args[i]

		switch {
		case tok == "-f" || tok == "--file":
			if i+1 < len(args) {
				files = append(files, args[i+1])
				i++
			}
		case strings.HasPrefix(tok, "-f="):
			files = append(files, strings.TrimPrefix(tok, "-f="))
		case strings.HasPrefix(tok, "--file="):
			files = append(files, strings.TrimPrefix(tok, "--file="))
		case tok == "--format":
			if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		case strings.HasPrefix(tok, "--format="):
			format = strings.TrimPrefix(tok, "--format=")
		case tok == "--app":
			if i+1 < len(args) {
				pa.scope = args[i+1]
				i++
			}
		case strings.HasPrefix(tok, "--app="):
			pa.scope = strings.TrimPrefix(tok, "--app=")
		case tok == "--eject":
			pa.eject = true
		case tok == "--force":
			pa.force = true
		case tok == "-v" || tok == "--verbose":
			pa.verbose = true
		}
	}

	if format == "procfile" {
		wantProc = true
	}

	for _, f := range files {
		if isProcfilePath(f) {
			wantProc = true
			pa.path = f

			break
		}
	}

	if !wantProc {
		return procfileApply{}, false
	}

	// --format procfile without an explicit Procfile path: assume
	// ./Procfile in the cwd.
	if pa.path == "" {
		if len(files) > 0 {
			pa.path = files[0]
		} else {
			pa.path = "Procfile"
		}
	}

	return pa, true
}

// procfilePathFromFiles resolves a Procfile target from structured apply
// flags (the local path, where we have applyFlags rather than raw argv).
// `--format procfile` forces Procfile mode even for an oddly-named file.
func procfilePathFromFiles(files []string, format string) (string, bool) {
	if format == "procfile" {
		if len(files) > 0 {
			return files[0], true
		}

		return "Procfile", true
	}

	for _, f := range files {
		if isProcfilePath(f) {
			return f, true
		}
	}

	return "", false
}

// discoverApplyFiles resolves the implicit `-f` target when the operator
// runs `vd apply` (or `vd apply -f`) with no manifest:
//
//   1. ./Procfile exists → apply it (the migration on-ramp).
//   2. else .voodu/ holds manifests → apply the whole dir.
//   3. else → nil, so the caller emits the normal "nothing to apply".
//
// Returns the value(s) to feed `-f`, and prints what it picked so the
// operator sees the implicit choice.
func discoverApplyFiles() []string {
	if fi, err := os.Stat("Procfile"); err == nil && !fi.IsDir() {
		fmt.Fprintf(os.Stdout, "%s no -f given — applying Procfile\n", check())

		return []string{"Procfile"}
	}

	if dirHasManifests(vooduDir) {
		fmt.Fprintf(os.Stdout, "%s no -f given — applying %s/\n", check(), vooduDir)

		return []string{vooduDir}
	}

	return nil
}

// dirHasManifests reports whether dir exists and holds at least one file
// with a recognized manifest extension — so discovery doesn't inject
// `-f .voodu` when the dir only carries app.json.
func dirHasManifests(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		switch filepath.Ext(e.Name()) {
		case ".hcl", ".voodu", ".vdu", ".vd":
			return true
		}
	}

	return false
}

// isProcfilePath reports whether a -f target names a Procfile by its
// basename: exactly "Procfile" or "Procfile.<variant>" (Procfile.dev,
// Procfile.staging, …).
func isProcfilePath(p string) bool {
	base := filepath.Base(p)

	return base == "Procfile" || strings.HasPrefix(base, "Procfile.")
}

// Project-link file: `.voodu/app.json` in the Procfile's directory ties
// this checkout to a stable scope so repeated `vd apply -f Procfile` runs
// are IDEMPOTENT. Without a stable scope, a fresh random one each apply
// changes the (scope, name) identity of every resource and the controller
// stacks duplicate pods instead of reconciling. Same idea as Vercel's
// `.vercel/project.json` — machine-written linkage, commit it to keep the
// identity stable across machines / CI. The `.voodu/` dir is a namespace
// for future per-project state; the JSON grows gracefully (app name,
// formation, …) without littering the repo with dotfiles.
const (
	vooduDir    = ".voodu"
	appLinkFile = "app.json"
)

// projectLink is the schema of `.voodu/app.json`. Minimal today; the
// JSON shape leaves room to grow into a fuller app manifest later.
type projectLink struct {
	Scope string `json:"scope"`
}

// resolveProcfileScope returns the scope every generated resource lands
// under, with STABLE identity across applies:
//
//   - `--app <name>` set → use it AND save it to the link file (so a
//     later plain `vd apply -f Procfile` reuses it). --app stays optional.
//   - else, the link file exists → reuse its scope.
//   - else → generate a random 3-char scope AND write the link file, so
//     the next apply reuses it and reconciles in place.
//
// Random (not derived from the dirname) keeps two unrelated apps from
// colliding on a guessed scope; the link file keeps re-applies idempotent.
func resolveProcfileScope(app, procfilePath string) (string, error) {
	dir := filepath.Dir(procfilePath)
	existing := readProjectScope(dir)
	linkRel := filepath.Join(vooduDir, appLinkFile)

	// --app: use the given scope and persist it (only writing when it
	// actually differs from what's on disk, to avoid touching the file
	// on every apply).
	if app != "" {
		if existing != app {
			if err := writeProjectLink(dir, app); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", linkRel, err)
			}
		}

		return app, nil
	}

	if existing != "" {
		return existing, nil
	}

	s, err := randomScope(3)
	if err != nil {
		return "", err
	}

	// Persist so subsequent applies reconcile the same resources. Non-
	// fatal on failure: warn and continue, but flag that re-apply would
	// then spawn a fresh scope.
	if err := writeProjectLink(dir, s); err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: could not write %s: %v\n"+
				"         re-applies will generate a NEW scope (duplicate pods); pass --app to pin it\n",
			linkRel, err)

		return s, nil
	}

	fmt.Fprintf(os.Stdout, "%s scope %s (saved to %s — commit it or pass --app to pin)\n",
		check(), s, linkRel)

	return s, nil
}

// readProjectScope returns the scope recorded in `<dir>/.voodu/app.json`,
// or "" when the file is absent / unreadable / has no scope.
func readProjectScope(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, vooduDir, appLinkFile))
	if err != nil {
		return ""
	}

	var link projectLink
	if json.Unmarshal(data, &link) != nil {
		return ""
	}

	return strings.TrimSpace(link.Scope)
}

// writeProjectLink materialises `<dir>/.voodu/app.json` with the scope.
func writeProjectLink(dir, scope string) error {
	d := filepath.Join(dir, vooduDir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}

	b, err := json.MarshalIndent(projectLink{Scope: scope}, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(d, appLinkFile), append(b, '\n'), 0o644)
}

// Scope alphabets are LOWERCASE only: the scope becomes part of the
// Docker image tag (`<scope>-<name>:latest`) and container names, and
// Docker rejects any uppercase in a repository name ("repository name
// must be lowercase"). First char is a letter (avoids a leading-digit
// edge case in downstream identifiers); the rest are lowercase
// alphanumerics.
const (
	scopeAlpha = "abcdefghijklmnopqrstuvwxyz"
	scopeAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// randomScope returns an n-char docker-safe scope from a CSPRNG:
// lowercase, letter-first.
func randomScope(n int) (string, error) {
	if n < 1 {
		n = 3
	}

	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate scope: %w", err)
	}

	out := make([]byte, n)
	out[0] = scopeAlpha[int(buf[0])%len(scopeAlpha)]

	for i := 1; i < n; i++ {
		out[i] = scopeAlnum[int(buf[i])%len(scopeAlnum)]
	}

	return string(out), nil
}

// runProcfileForwarded is the client-side orchestrator for a Procfile
// apply against a configured remote. Resolves the scope, then either
// ejects HCL locally (--eject, no server contact) or ships the source
// tarball to `receive-pack --procfile` for server-side transform + build.
func runProcfileForwarded(info *remote.Info, identity string, pa procfileApply) (int, error) {
	// Fail fast on a missing Procfile — before generating a scope or
	// packing/shipping a tarball — so the operator gets a clear message
	// instead of a server-side "no Procfile in the tree" after the upload.
	if _, err := os.Stat(pa.path); err != nil {
		return 1, fmt.Errorf("Procfile not found at %s", pa.path)
	}

	scope, err := resolveProcfileScope(pa.scope, pa.path)
	if err != nil {
		return 1, err
	}

	if pa.eject {
		if err := runProcfileEject(pa, scope); err != nil {
			return 1, err
		}

		return 0, nil
	}

	if err := pushProcfileTarball(info, identity, pa, scope); err != nil {
		return 1, err
	}

	return 0, nil
}

// runProcfileEject renders the Procfile to an HCL file the operator can
// own and edit (host/TLS via a commented ingress stub). Pure client-side
// — never contacts a server.
func runProcfileEject(pa procfileApply, scope string) error {
	f, err := os.Open(pa.path)
	if err != nil {
		return fmt.Errorf("open procfile: %w", err)
	}
	defer f.Close()

	procs, err := procfile.Parse(f)
	if err != nil {
		return err
	}

	hcl, err := procfile.ToHCL(procs, procfile.Options{Scope: scope})
	if err != nil {
		return err
	}

	// Write under the project's .voodu/ home (next to app.json), not the
	// repo root. resolveManifestPath searches .voodu/ first, so the
	// operator re-applies with the short `vd apply -f <scope>` form.
	dir := filepath.Join(filepath.Dir(pa.path), vooduDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", vooduDir, err)
	}

	out := filepath.Join(dir, scope+".voodu")
	if err := os.WriteFile(out, []byte(hcl), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}

	fmt.Fprintf(os.Stdout, "%s ejected %d process(es) → %s\n", check(), len(procs), out)
	fmt.Fprintf(os.Stdout, "   review it, then: vd apply -f %s\n", scope)

	return nil
}

// pushProcfileTarball streams the Procfile's directory as a gzipped tar
// into `voodu receive-pack --procfile=<scope>` on the server. The server
// reads the Procfile from the tree, generates the deployment/job specs,
// persists them, and builds each process — see runProcfileReceive.
//
// Mirrors pushSourceViaTarball (the build-mode deploy push) but ships a
// whole-app context with the procfile fan-out flag instead of a single
// (scope, name) ref + build spec.
func pushProcfileTarball(info *remote.Info, identity string, pa procfileApply, scope string) error {
	dir := filepath.Dir(pa.path)

	fmt.Fprintf(os.Stdout, "%s packing %s (procfile → scope %s)\n", check(), dir, scope)

	legacy := newProgressFilter(os.Stdout, pa.verbose)
	nd := newEventRenderer(os.Stdout, pa.verbose)
	filter := newNegotiatingWriter(legacy, nd)

	pr, pw := io.Pipe()

	var tarProgress io.Writer
	if pa.verbose {
		tarProgress = os.Stderr
	}

	go func() {
		_, err := tarball.Stream(pw, dir, tarball.Options{
			MaxSize:  buildContextMaxSize(),
			Progress: tarProgress,
		})
		_ = pw.CloseWithError(err)
	}()

	args := []string{"receive-pack", "--procfile=" + scope}
	if pa.force || os.Getenv("VOODU_FORCE_REBUILD") == "1" {
		args = append(args, "--force")
	}

	code, err := remote.Forward(info, args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    pr,
		Stdout:   filter,
		Stderr:   filter,
	})

	_ = filter.Close()

	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("remote exited %d", code)
	}

	return nil
}
