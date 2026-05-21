// reconcile_status.go owns the cross-kind "persist the last reconcile
// outcome on the status blob" hook. Lives outside the per-kind handler
// files because (a) the logic is identical for deployment and
// statefulset — both share DeploymentStatus on disk — and (b) keeping
// it here makes the surfacing-reconcile-errors story easy to find
// when someone asks "where does LastReconcileError come from".
//
// Wired in server.go via Reconciler.OnReconcile. Fires after every
// terminal handle attempt (success, non-transient failure, or
// transient-retries-exhausted). Transient retries still in flight do
// NOT fire — only the eventual outcome matters for operator-facing
// visibility.

package controller

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

// recordReconcileResult persists the latest reconcile outcome on the
// kind-specific status blob. For kinds that share DeploymentStatus
// (deployment, statefulset), this populates LastReconcileError +
// LastReconcileAt so `vd describe` and the post-apply polling path
// have a queryable record of failures. Other kinds (ingress, asset,
// job, cronjob, registry) are no-ops — their status shapes don't
// carry reconcile-error fields today, and the journal log line is
// the existing recourse.
//
// Best-effort: failures to read/write the status blob log a warning
// and continue. The reconciler's primary job (acting on the spec)
// already happened by the time this runs; status persistence is a
// visibility layer, not a correctness layer.
//
// Race posture: the per-kind handler may have just written status
// (Replicas, Releases, ReplicaReadiness) before returning. This call
// runs SEQUENTIALLY after the handler — by the time we read, the
// handler's write has landed. Read-modify-write of just the two
// new fields preserves everything else.
func recordReconcileResult(ctx context.Context, store Store, ev WatchEvent, reconcileErr error, logger *log.Logger) {
	// Only kinds with a DeploymentStatus-shaped blob participate.
	// Adding new kinds is a matter of extending this switch.
	switch ev.Kind {
	case KindDeployment, KindStatefulset:
		// Proceed below.
	default:
		return
	}

	// WatchEvent doesn't carry the AppID directly — derive from
	// (Scope, Name). This is what every handler does too.
	app := AppID(ev.Scope, ev.Name)

	raw, err := store.GetStatus(ctx, ev.Kind, app)
	if err != nil {
		// Read failure isn't fatal — we proceed with a zero-value
		// status, which means subsequent writers will see a fresh
		// blob. The downside is we'd clobber any fields the handler
		// wrote, so we log and BAIL rather than overwrite.
		if logger != nil {
			logger.Printf("reconcile status: read %s/%s failed (skipping persist): %v", ev.Kind, app, err)
		}

		return
	}

	var st DeploymentStatus

	if raw != nil {
		if err := json.Unmarshal(raw, &st); err != nil {
			// Corrupted blob — same posture as the read error.
			// Logging surfaces the inconsistency to journald
			// without dropping the rest of the platform's state.
			if logger != nil {
				logger.Printf("reconcile status: decode %s/%s failed (skipping persist): %v", ev.Kind, app, err)
			}

			return
		}
	}

	st.LastReconcileAt = time.Now().UTC()

	if reconcileErr == nil {
		// Success path: clear the stale error so `vd describe`
		// stops showing a fixed problem as still active. Without
		// this clear, operators would see ghost errors from
		// problems they already resolved.
		st.LastReconcileError = ""
	} else {
		st.LastReconcileError = reconcileErr.Error()
	}

	blob, err := json.Marshal(st)
	if err != nil {
		if logger != nil {
			logger.Printf("reconcile status: marshal %s/%s failed: %v", ev.Kind, app, err)
		}

		return
	}

	if err := store.PutStatus(ctx, ev.Kind, app, blob); err != nil {
		if logger != nil {
			logger.Printf("reconcile status: persist %s/%s failed: %v", ev.Kind, app, err)
		}
	}
}
