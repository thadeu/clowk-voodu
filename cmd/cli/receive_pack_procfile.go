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

	// 3. Build each generated resource's image from the same source.
	for _, m := range mans {
		appID := controller.AppID(m.Scope, m.Name)

		f, err := os.Open(tmpPath)
		if err != nil {
			return fmt.Errorf("reopen tarball for %s: %w", appID, err)
		}

		buildErr := deploy.RunFromTarball(appID, f, deploy.Options{
			Reporter: reporter,
			Force:    force,
			// Spec nil → the build pipeline auto-detects the language
			// from the extracted tree. All processes share one source,
			// so every build resolves to the same runtime; the per-
			// process command is set on the manifest, not baked in.
		})
		f.Close()

		if buildErr != nil {
			return fmt.Errorf("build %s: %w", appID, buildErr)
		}
	}

	// 4. Persist all manifests to the local controller (etcd + reconcile).
	if err := applyManifestsLocally(cmd, mans); err != nil {
		return err
	}

	for _, m := range mans {
		reporter.Result(string(m.Kind), m.Scope, m.Name, "applied")
	}

	reporter.Summary(fmt.Sprintf("Procfile applied: %d resource(s) under scope %q", len(mans), scope))

	return nil
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
