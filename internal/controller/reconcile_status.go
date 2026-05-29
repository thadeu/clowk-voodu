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

// reconcileOutcomeRecorder is implemented by every status blob that
// carries the LastReconcileError / LastReconcileAt pair. It lets
// recordReconcileResult persist the outcome generically — the concrete
// type (DeploymentStatus, IngressStatus) is decoded from etcd, so
// json.Marshal round-trips ALL its fields and only the two reconcile
// fields get touched.
type reconcileOutcomeRecorder interface {
	recordOutcome(at time.Time, errMsg string)
}

// recordReconcileResult persists the latest reconcile outcome on the
// kind-specific status blob. Deployment/statefulset share
// DeploymentStatus; ingress uses IngressStatus. Both populate
// LastReconcileError + LastReconcileAt so `vd describe`, the post-apply
// polling path, and the `vd get` degraded scan have a queryable record
// of failures. Other kinds (asset, job, cronjob, registry) are no-ops —
// their status shapes don't carry reconcile-error fields today, and the
// journal log line is the existing recourse.
//
// Ingress is the kind that most needed this: an ingress applied before
// its target deployment had a live replica fails resolveUpstream,
// exhausts its transient-retry budget (~60s), and gives up — previously
// leaving NO status, so `vd describe ingress` printed "(no status
// recorded yet)" and the operator was left guessing. Now the failure is
// recorded here, AND a successful deployment reconcile re-triggers the
// stuck ingress (see retriggerDependentIngresses).
//
// Best-effort: failures to read/write the status blob log a warning
// and continue. The reconciler's primary job (acting on the spec)
// already happened by the time this runs; status persistence is a
// visibility layer, not a correctness layer.
//
// Race posture: the per-kind handler may have just written status
// (Replicas, Releases, ReplicaReadiness; or the ingress plugin's
// Plugin/Data) before returning. This call runs SEQUENTIALLY after the
// handler — by the time we read, the handler's write has landed.
// Read-modify-write of just the two reconcile fields preserves
// everything else.
func recordReconcileResult(ctx context.Context, store Store, ev WatchEvent, reconcileErr error, logger *log.Logger) {
	// WatchEvent doesn't carry the AppID directly — derive from
	// (Scope, Name). This is what every handler does too.
	app := AppID(ev.Scope, ev.Name)

	switch ev.Kind {
	case KindDeployment, KindStatefulset:
		persistReconcileOutcome(ctx, store, ev.Kind, app, reconcileErr, logger, &DeploymentStatus{})

		// Self-heal the ordering gap: a deployment that just
		// reconciled cleanly now has its container(s) created, so any
		// ingress in the scope that routes to it and previously gave up
		// can be re-applied successfully. Only KindDeployment — ingress
		// routes to deployments, never statefulsets (resolveUpstream
		// looks up KindDeployment).
		if ev.Kind == KindDeployment && reconcileErr == nil {
			retriggerDependentIngresses(ctx, store, ev.Scope, ev.Name, logger)
		}

	case KindIngress:
		persistReconcileOutcome(ctx, store, ev.Kind, app, reconcileErr, logger, &IngressStatus{})

	default:
		return
	}
}

// persistReconcileOutcome does the read-modify-write of the two
// reconcile fields on whatever status blob `st` decodes into. `st` must
// be a pointer (json.Unmarshal / recordOutcome mutate it). An absent
// blob (nil) starts from the zero value — the first-ever failure of a
// resource that never wrote status still produces a record.
func persistReconcileOutcome(ctx context.Context, store Store, kind Kind, app string, reconcileErr error, logger *log.Logger, st reconcileOutcomeRecorder) {
	raw, err := store.GetStatus(ctx, kind, app)
	if err != nil {
		// Read failure isn't fatal — but we'd risk clobbering fields
		// the handler wrote, so we log and BAIL rather than overwrite.
		if logger != nil {
			logger.Printf("reconcile status: read %s/%s failed (skipping persist): %v", kind, app, err)
		}

		return
	}

	if raw != nil {
		if err := json.Unmarshal(raw, st); err != nil {
			// Corrupted blob — same posture as the read error.
			if logger != nil {
				logger.Printf("reconcile status: decode %s/%s failed (skipping persist): %v", kind, app, err)
			}

			return
		}
	}

	errMsg := ""
	if reconcileErr != nil {
		errMsg = reconcileErr.Error()
	}

	// On success errMsg == "" clears any stale error so `vd describe`
	// stops showing a fixed problem as still active.
	st.recordOutcome(time.Now().UTC(), errMsg)

	blob, err := json.Marshal(st)
	if err != nil {
		if logger != nil {
			logger.Printf("reconcile status: marshal %s/%s failed: %v", kind, app, err)
		}

		return
	}

	if err := store.PutStatus(ctx, kind, app, blob); err != nil {
		if logger != nil {
			logger.Printf("reconcile status: persist %s/%s failed: %v", kind, app, err)
		}
	}
}
