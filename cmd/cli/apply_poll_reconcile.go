// apply_poll_reconcile.go is the post-apply visibility layer — the
// operator-facing fix for "I applied this and nothing happened, why?".
//
// Flow: after `vd apply`'s aurora terminus prints, the CLI runs
// `vd describe -o json` over the same SSH connection used by apply,
// once per pod-bearing kind it just applied (deployment, statefulset).
// The controller's reconciler — running async to apply — will have
// updated LastReconcileError / LastReconcileAt on the status blob by
// then. Any resource with a fresh error gets a warning printed before
// the CLI exits, so operators don't have to dig through journald.
//
// Best-effort: every failure here (SSH glitch, parse error, timeout)
// silently degrades to "no warning printed". The apply already
// succeeded; the polling is a visibility belt-and-suspenders. Worst
// case the operator runs `vd describe <resource>` themselves and
// Phase A's persisted LastReconcileError shows the same content.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/remote"
)

// reconcileWarning is a single resource's post-apply failure detail.
// Format-agnostic — pollReconcileErrors collects these and the
// caller renders them at the end of apply output.
type reconcileWarning struct {
	Kind  controller.Kind
	Scope string
	Name  string
	Error string
	At    time.Time
}

// pollReconcileTotalBudget caps wall-clock for the entire polling
// pass. Apply latency was already established at ~2-5s for a typical
// stack; adding 8s of polling brings worst-case to ~13s — high
// enough to catch slow first-reconciles, low enough that operators
// don't notice on the happy path.
var pollReconcileTotalBudget = 8 * time.Second

// pollReconcileInitialWait is the up-front sleep before the first
// describe sweep. Gives the reconciler time to fire its OnReconcile
// callback for the just-applied resources. Most reconciles complete
// in <500ms; 1s covers the common case without obvious lag.
var pollReconcileInitialWait = 1 * time.Second

// pollReconcileRetryInterval is the gap between attempts on a single
// resource whose status hasn't refreshed yet. Short enough to catch
// a probe just landing, long enough to not hammer SSH.
var pollReconcileRetryInterval = 750 * time.Millisecond

// pollReconcileErrors fans out a `vd describe -o json` SSH call per
// pod-bearing applied resource, waits for the LastReconcileAt
// timestamp to advance past applyStartedAt (the moment the apply
// phase started), and returns any LastReconcileError seen.
//
// Resources for which the reconciler doesn't fire within the budget
// are silently skipped — better to miss a slow first-apply warning
// than to falsely flag a still-running reconcile as failed.
//
// Skipped entirely when zero pod-bearing resources were applied
// (e.g. ingress-only or asset-only manifests).
func pollReconcileErrors(info *remote.Info, identity string, applied []*controller.Manifest, applyStartedAt time.Time) []reconcileWarning {
	candidates := filterPodBearingForPoll(applied)
	if len(candidates) == 0 {
		return nil
	}

	// Initial wait — give the reconciler a head start so we don't
	// immediately observe "no fresh reconcile yet" on every resource
	// and burn budget on retries.
	time.Sleep(pollReconcileInitialWait)

	deadline := time.Now().Add(pollReconcileTotalBudget)

	var (
		mu       sync.Mutex
		warnings []reconcileWarning
		wg       sync.WaitGroup
	)

	for _, m := range candidates {
		wg.Add(1)

		go func(m *controller.Manifest) {
			defer wg.Done()

			if w, ok := pollOneResource(info, identity, m, applyStartedAt, deadline); ok && w.Error != "" {
				mu.Lock()
				warnings = append(warnings, w)
				mu.Unlock()
			}
		}(m)
	}

	wg.Wait()

	return warnings
}

// filterPodBearingForPoll narrows the applied set to kinds whose
// DeploymentStatus carries LastReconcileError (deployment +
// statefulset share that struct). Other kinds (ingress, asset, job,
// cronjob, registry) have their own status shapes and don't fit the
// "reconcile error visible to the operator" surface today.
func filterPodBearingForPoll(applied []*controller.Manifest) []*controller.Manifest {
	out := make([]*controller.Manifest, 0, len(applied))

	for _, m := range applied {
		if m == nil {
			continue
		}

		if m.Kind == controller.KindDeployment || m.Kind == controller.KindStatefulset {
			out = append(out, m)
		}
	}

	return out
}

// pollOneResource describes a single resource over SSH until either:
//   - LastReconcileAt advances past applyStartedAt (fresh result — we
//     check for LastReconcileError and return), OR
//   - the deadline elapses (give up; return ok=false so the caller
//     skips this resource).
//
// SSH / decode errors are treated as "try again" up to the deadline —
// a transient blip shouldn't permanently mask a real reconcile error.
func pollOneResource(info *remote.Info, identity string, m *controller.Manifest, applyStartedAt, deadline time.Time) (reconcileWarning, bool) {
	for time.Now().Before(deadline) {
		st, ok := fetchReconcileStatusSSH(info, identity, m.Kind, m.Scope, m.Name)
		if !ok {
			time.Sleep(pollReconcileRetryInterval)
			continue
		}

		// Skip stale snapshots — the reconciler hasn't fired since
		// our apply landed, so any LastReconcileError we'd see is
		// from a previous run and not what the operator just did.
		if st.LastReconcileAt.Before(applyStartedAt) {
			time.Sleep(pollReconcileRetryInterval)
			continue
		}

		return reconcileWarning{
			Kind:  m.Kind,
			Scope: m.Scope,
			Name:  m.Name,
			Error: st.LastReconcileError,
			At:    st.LastReconcileAt,
		}, true
	}

	return reconcileWarning{}, false
}

// fetchReconcileStatusSSH runs `vd describe <kind> <ref> -o json`
// remotely and extracts the inner DeploymentStatus blob. Returns
// ok=false on any failure path (non-zero exit, JSON decode error,
// missing status field) — the caller retries until the polling
// deadline.
func fetchReconcileStatusSSH(info *remote.Info, identity string, kind controller.Kind, scope, name string) (controller.DeploymentStatus, bool) {
	ref := name
	if scope != "" {
		ref = scope + "/" + name
	}

	args := []string{"describe", string(kind), ref, "-o", "json"}

	var stdout bytes.Buffer

	code, err := remote.Forward(info, args, remote.ForwardOptions{
		Identity: identity,
		Stdout:   &stdout,
		Stderr:   io.Discard,
	})
	if err != nil || code != 0 {
		return controller.DeploymentStatus{}, false
	}

	// describe -o json shape (matches cmd_describe.go's writeDescribeJSON):
	// {"manifest": {...}, "status": {... DeploymentStatus blob ...}, "pods": [...]}
	//
	// We only care about the status blob for reconcile error
	// surfacing. Partial decode keeps us forward-compat with any
	// future top-level fields.
	var env struct {
		Status json.RawMessage `json:"status"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		return controller.DeploymentStatus{}, false
	}

	if len(env.Status) == 0 {
		return controller.DeploymentStatus{}, false
	}

	var st controller.DeploymentStatus
	if err := json.Unmarshal(env.Status, &st); err != nil {
		return controller.DeploymentStatus{}, false
	}

	return st, true
}

// renderReconcileWarnings prints the post-apply visibility block to
// stdout. Called from runApplyForwarded right after the aurora
// terminus when pollReconcileErrors returned non-empty.
//
// Visual: warn-coloured header line + indented per-resource details.
// The header signals "your apply succeeded BUT the reconciler is
// reporting a problem you should look at" — distinct from a failed
// apply (which would have aborted earlier with a red ✗).
//
// Example:
//
//	⚠ reconcile errors on 1 resource
//	  ~ deployment/fsw/adapter  12s ago
//	    env_from [fsw/shared]: no env files resolved
//	    Hint: vd describe deployment fsw/adapter
func renderReconcileWarnings(out io.Writer, ws []reconcileWarning) {
	if len(ws) == 0 {
		return
	}

	noun := "resource"
	if len(ws) > 1 {
		noun = "resources"
	}

	fmt.Fprintf(out, "\n%s reconcile errors on %d %s\n", warn(), len(ws), noun)

	for _, w := range ws {
		rel := formatRelativeTimeShort(w.At)
		ref := fmt.Sprintf("%s/%s/%s", w.Kind, w.Scope, w.Name)

		fmt.Fprintf(out, "  %s %s  %s\n", tilde(), ref, dim(rel))

		// Multi-line error messages indent each line for readability.
		for _, line := range strings.Split(strings.TrimSpace(w.Error), "\n") {
			fmt.Fprintf(out, "    %s\n", colorize(cRose, line))
		}

		fmt.Fprintf(out, "    %s vd describe %s %s/%s\n", dim("Hint:"), w.Kind, w.Scope, w.Name)
	}
}

// formatRelativeTimeShort mirrors cmd_describe.go's formatRelativeTime
// but without the parens — used inline next to the resource ref where
// the surrounding format already provides delimiters.
func formatRelativeTimeShort(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	d := time.Since(t).Round(time.Second)

	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
