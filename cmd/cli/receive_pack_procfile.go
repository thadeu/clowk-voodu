package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/deploy"
	"go.voodu.clowk.in/internal/manifest"
	"go.voodu.clowk.in/internal/procfile"
	"go.voodu.clowk.in/internal/progress"
)

// runProcfileReceive is the server-side Procfile fan-out, invoked by
// `voodu receive-pack --procfile <scope>` over SSH. The client has piped
// a gzipped tar of the app source (Procfile included) on stdin.
//
// Steps:
//  1. Buffer the tarball to a temp file (we replay it once per process).
//  2. Read the Procfile from the tree and transform it into manifests.
//  3. Build each process's image from the source (per-process MVP).
//  4. Persist all manifests to the local controller via /apply, which
//     reconciles them into running containers / one-shot jobs.
//
// Build-before-persist ordering matters: RunFromTarball builds the
// `<scope>-<name>:latest` image the reconciler will look for, so by the
// time /apply lands the desired state in etcd the images already exist.
func runProcfileReceive(cmd *cobra.Command, scope string, src io.Reader, force bool) error {
	reporter := progress.NewReporterFromEnv(os.Stdout)
	reporter.Hello()

	defer reporter.Close()

	// 1. Buffer stdin so we can open a fresh reader per process build.
	tmp, err := os.CreateTemp("", "voodu-procfile-*.tar.gz")
	if err != nil {
		return fmt.Errorf("buffer tarball: %w", err)
	}
	tmpPath := tmp.Name()

	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()

		return fmt.Errorf("buffer tarball: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("buffer tarball: %w", err)
	}

	// 2. Read the Procfile from the buffered tree + transform.
	raw, err := readFileFromTar(tmpPath, "Procfile")
	if err != nil {
		if err == errFileNotInTar {
			return fmt.Errorf("no Procfile found at the root of the shipped source")
		}

		return err
	}

	procs, err := procfile.Parse(bytes.NewReader(raw))
	if err != nil {
		return err
	}

	// .voodu/app.json — per-process ingress declarations. The client
	// re-includes just this one file past the tarball's `.voodu/` ignore
	// (everything else under .voodu/ stays out of the build context).
	// Absent is fine (no routing); present-but-broken is a hard error so
	// the operator notices instead of silently losing ingress.
	opts := procfile.Options{Scope: scope}

	if appRaw, aerr := readFileFromTar(tmpPath, filepath.Join(".voodu", "app.json")); aerr == nil {
		// Interpolate ${VAR} from the scope's config bucket so ONE
		// app.json serves multiple stages: `vd config <scope> set
		// API_HOST=staging…` on the staging server vs `…=api…` on prod
		// resolves per-server at apply time. Missing var (no `:-default`)
		// is a hard error — set the config bucket before applying.
		cfg, cerr := fetchScopeConfig(cmd, scope)
		if cerr != nil {
			return fmt.Errorf("read config bucket for scope %q: %w", scope, cerr)
		}

		interpolated, ierr := manifest.Interpolate(string(appRaw), cfg)
		if ierr != nil {
			return fmt.Errorf("app.json interpolation (set the var via `vd config %s set …`): %w", scope, ierr)
		}

		appFile, perr := procfile.ParseAppFile([]byte(interpolated))
		if perr != nil {
			return perr
		}

		opts.Ingress = appFile.Ingress
	} else if aerr != errFileNotInTar {
		return aerr
	}

	mans, err := procfile.ToManifests(procs, opts)
	if err != nil {
		return err
	}

	// 3. Build ONCE, reuse for the rest. Every Procfile process shares one
	// source tree and Spec=nil (auto-detect), so they all resolve to the
	// SAME runtime image — only the tag differs. We build the first
	// buildable resource normally, then retag that image for the others
	// instead of rebuilding N times. Ingress manifests carry no source and
	// are skipped here; they're persisted in step 4 like everything else.
	primary, reuse := buildPlan(mans)
	if primary == "" {
		// A parsed Procfile always yields >=1 deployment/job, so this is
		// defensive — only reachable if ToManifests ever emits an
		// ingress-only set.
		return fmt.Errorf("procfile produced no buildable resource (deployment/job)")
	}

	// Primary: full build (extract + docker build + :latest/:<buildID>
	// tags + current symlink). Unchanged path — byte-identical to a
	// single-deployment build today. Spec nil → auto-detect language.
	if err := withTarball(tmpPath, func(f *os.File) error {
		return deploy.RunFromTarball(primary, f, deploy.Options{Reporter: reporter, Force: force})
	}); err != nil {
		return fmt.Errorf("build %s: %w", primary, err)
	}

	// Remaining processes: reuse the primary's image (retag) + own release
	// dir + symlink. No rebuild — that's the win.
	for _, appID := range reuse {
		if err := withTarball(tmpPath, func(f *os.File) error {
			return deploy.MaterializeFromBuilt(appID, f, primary, deploy.Options{Reporter: reporter, Force: force})
		}); err != nil {
			return fmt.Errorf("reuse build for %s: %w", appID, err)
		}
	}

	// 4. Persist all manifests to the local controller (etcd + reconcile).
	if err := applyManifestsLocally(cmd, mans); err != nil {
		return err
	}

	// Blank line: separate the build/stream block above from the per-
	// resource result block below — the third visual context (packing |
	// build | results). The build spinner has stopped by now (active=false
	// after "Build completed"), so the renderer prints this empty log
	// verbatim as a clean gap.
	reporter.Log(progress.LevelInfo, "")

	for _, m := range mans {
		reporter.Result(string(m.Kind), m.Scope, m.Name, "applied")
	}

	reporter.Summary(fmt.Sprintf("procfile applied: %d resource(s) under scope %q", len(mans), scope))

	return nil
}

// buildPlan splits generated manifests into the single build target
// (primary) and the reuse targets for the build-once Procfile fan-out.
// Only deployment/job kinds carry source and need an image; ingress (and
// any other kind) is excluded — it's persisted later but never built.
// Declaration order is preserved, so the first Procfile process line
// becomes the primary that actually runs `docker build`.
func buildPlan(mans []controller.Manifest) (primary string, reuse []string) {
	for _, m := range mans {
		if m.Kind != controller.KindDeployment && m.Kind != controller.KindJob {
			continue
		}

		appID := controller.AppID(m.Scope, m.Name)

		if primary == "" {
			primary = appID

			continue
		}

		reuse = append(reuse, appID)
	}

	return primary, reuse
}

// withTarball opens the buffered tarball, runs fn against the fresh
// reader, and always closes it — so each build/reuse step replays the
// same source from the top. Centralizes the reopen+close so the build
// loop reads as intent, not file plumbing.
func withTarball(tmpPath string, fn func(*os.File) error) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("reopen tarball: %w", err)
	}

	defer f.Close()

	return fn(f)
}

// fetchScopeConfig returns the scope-level config bucket (the vars set
// via `vd config <scope> set KEY=val`) from the local controller. Used to
// interpolate ${VAR} in app.json. An absent bucket is an empty map, not
// an error — the interpolation then errors only on a referenced-but-unset
// var without a default.
func fetchScopeConfig(cmd *cobra.Command, scope string) (map[string]string, error) {
	resp, err := controllerDo(cmd.Root(), http.MethodGet, "/config", "scope="+url.QueryEscape(scope), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return map[string]string{}, nil
	}

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Data struct {
			Vars map[string]string `json:"vars"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode config response: %w", err)
	}

	if env.Data.Vars == nil {
		return map[string]string{}, nil
	}

	return env.Data.Vars, nil
}

// applyManifestsLocally POSTs the generated manifests to the local
// controller's /apply, reusing the exact persist + reconcile path a
// normal `vd apply` uses (upsert-only; no prune). receive-pack runs on
// the controller host, so controllerDo targets localhost.
func applyManifestsLocally(cmd *cobra.Command, mans []controller.Manifest) error {
	body, err := json.Marshal(mans)
	if err != nil {
		return fmt.Errorf("encode manifests: %w", err)
	}

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/apply", "", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	return nil
}

// errFileNotInTar signals a clean "not present" so optional reads (like
// .voodu/app.json) can distinguish absence from a real read error.
var errFileNotInTar = fmt.Errorf("file not found in tarball")

// readFileFromTar returns the contents of `want` (a `filepath.Clean`-ed
// archive path) from a gzipped tarball. The client tars the Procfile's
// own directory, so paths are root-relative ("Procfile",
// ".voodu/app.json"). Returns errFileNotInTar when the entry is absent.
func readFileFromTar(path, want string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gunzip tarball: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("read tarball: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// `clean` collapses a leading "./" so "./Procfile" matches
		// "Procfile". Root-level match only — a nested path is ignored.
		if filepath.Clean(hdr.Name) == want {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read %s entry: %w", want, err)
			}

			return data, nil
		}
	}

	return nil, errFileNotInTar
}
