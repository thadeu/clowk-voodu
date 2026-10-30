// cmd_images.go is `vd images prune`: the operator's way to run the cleanup the
// controller also runs on its own schedule. Like every other command it works
// on the box and from a laptop, where it forwards over SSH to the same binary.
//
// All the keep rules live in the controller (image_prune.go), so the CLI, the
// schedule and the WebUI button can't disagree about what is safe to delete.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
)

// imagePruneRequestTimeout outlasts the controller's own budget for a run, so
// the operator sees its answer and not a client timeout.
const imagePruneRequestTimeout = 11 * time.Minute

type imagePruneFlags struct {
	dryRun bool
	keep   int
}

func newImagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Manage docker images on the host",
	}

	cmd.AddCommand(newImagesPruneCmd())

	return cmd
}

func newImagesPruneCmd() *cobra.Command {
	var f imagePruneFlags

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove images this host no longer needs",
		Long: `prune removes every image on the host that voodu doesn't need. The
controller runs the same cleanup twice a day; this runs it now.

Not the same as ` + "`docker image prune -a`" + `. An image stays when:

  - a container uses it, even a stopped one
  - a manifest names it
  - it is <app>:latest of a deployment, statefulset, job or cronjob
  - it is one of the newest --keep builds of a deployment or statefulset
  - it was tagged in the last hour

With a --keep below 5, older releases stay in the history but their images
go: ` + "`vd rollback`" + ` to one fails with "image not present locally".

Examples:
  vd images prune --dry-run     # show what would go
  vd images prune               # keep the 5 newest builds per app
  vd images prune --keep 2      # free more disk, fewer rollback targets`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImagesPrune(cmd, f)
		},
	}

	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "list what would be removed, remove nothing")
	cmd.Flags().IntVar(&f.keep, "keep", 0, "builds to keep per deployment or statefulset (default 5)")

	return cmd
}

func runImagesPrune(cmd *cobra.Command, f imagePruneFlags) error {
	root := cmd.Root()

	q := url.Values{}

	if f.dryRun {
		q.Set("dry_run", "true")
	}

	if f.keep != 0 {
		q.Set("keep", strconv.Itoa(f.keep))
	}

	resp, err := controllerDoTimeout(root, http.MethodPost, "/images/prune", q.Encode(), nil, imagePruneRequestTimeout)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Data controller.ImagePruneReport `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode prune report: %w", err)
	}

	switch outputFormat(root) {
	case "json":
		out := json.NewEncoder(os.Stdout)
		out.SetIndent("", "  ")

		return out.Encode(env.Data)

	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(env.Data)
	}

	renderImagePruneReport(os.Stdout, env.Data)

	if len(env.Data.Errors) > 0 {
		return fmt.Errorf("%d image(s) could not be removed", len(env.Data.Errors))
	}

	return nil
}

func renderImagePruneReport(w io.Writer, r controller.ImagePruneReport) {
	verb := "removed"
	if r.DryRun {
		verb = "would remove"
	}

	for _, ref := range r.Removed {
		fmt.Fprintf(w, "%s %s\n", verb, ref)
	}

	for _, ref := range r.Skipped {
		fmt.Fprintf(w, "skipped %s (in use)\n", ref)
	}

	for _, msg := range r.Errors {
		fmt.Fprintf(w, "error: %s\n", msg)
	}

	fmt.Fprintf(w, "%s %d, kept %d of %d images", verb, len(r.Removed), r.Kept, r.Scanned)

	if !r.DryRun {
		fmt.Fprintf(w, ", reclaimed %s", humanBytes(r.ReclaimedBytes))
	}

	fmt.Fprintln(w)
}
