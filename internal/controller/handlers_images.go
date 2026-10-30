// handlers_images.go is the HTTP surface of the image pruner. One handler
// behind both planes: `vd images prune` on the internal one, the WebUI's button
// on the PAT one.

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"go.voodu.clowk.in/internal/activity"
)

// POST /images/prune?dry_run=true&keep=3
//
// Synchronous: the caller wants the report, and a run takes seconds. The
// context is detached from the request on purpose, because a CLI that gives up
// should not stop a cleanup in the middle of a list of tags.
func (a *API) handleImagePrune(w http.ResponseWriter, r *http.Request) {
	if a.ImagePruner == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("image pruner not configured"))
		return
	}

	opts := ImagePruneOptions{DryRun: r.URL.Query().Get("dry_run") == "true"}

	if raw := r.URL.Query().Get("keep"); raw != "" {
		keep, err := strconv.Atoi(raw)
		if err != nil || keep < 1 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("keep must be a positive integer, got %q", raw))
			return
		}

		opts.KeepReleases = keep
	}

	// A dry run changes nothing, so it is no action for the trail.
	var act *activityTracker

	if !opts.DryRun {
		w, act = a.beginActivity(w, r, true, activity.Record{Action: activity.ActionImagePrune})
		defer act.Finish()
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), imagePruneTimeout)
	defer cancel()

	report, err := a.ImagePruner.Prune(ctx, opts)

	switch {
	case errors.Is(err, ErrImagePruneRunning):
		writeErr(w, http.StatusConflict, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if act != nil {
		report.annotate(&act.Record)
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: report})
}
