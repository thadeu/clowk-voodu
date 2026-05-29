package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/deploy"
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
	raw, err := readProcfileFromTar(tmpPath)
	if err != nil {
		return err
	}

	procs, err := procfile.Parse(bytes.NewReader(raw))
	if err != nil {
		return err
	}

	mans, err := procfile.ToManifests(procs, procfile.Options{Scope: scope})
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

// readProcfileFromTar finds and returns the Procfile contents from a
// gzipped tarball. The client tars the Procfile's own directory, so the
// Procfile sits at the archive root ("Procfile" or "./Procfile").
func readProcfileFromTar(path string) ([]byte, error) {
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

		// Match the Procfile at the archive root only — `clean` collapses
		// a leading "./" so both "Procfile" and "./Procfile" resolve to
		// "Procfile". A nested path/Procfile is intentionally ignored.
		if filepath.Clean(hdr.Name) == "Procfile" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read Procfile entry: %w", err)
			}

			return data, nil
		}
	}

	return nil, fmt.Errorf("no Procfile found at the root of the shipped source")
}
