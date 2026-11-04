// handlers_pat_manifests.go puts the resources themselves on the PAT plane:
// apply, delete, restart, rollback, stop and start, and the manifest reads.
// A hosted control plane (Anway Cloud) uses these to create and remove
// customer instances without SSH.
//
// Same invariant as handlers_pat_proxy.go: each handler checks its input
// and then calls the localhost handler, so both planes have one behaviour.
// The checks are the two things the localhost plane does not need:
//
//  1. Resource scope. Every route names its scope in the query, and the
//     PAT must hold it (PAT.ResourceScopes). The localhost handlers guess
//     the scope from the name when it is missing; on this plane a missing
//     scope is a 400, so a guess cannot land outside the token's scopes.
//  2. Apply policy. The body passes validatePATManifests (patpolicy.go)
//     before the shared apply path sees it.

package controller

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// patResourceScope reads the required `scope` query value and checks the
// PAT holds it. It writes the error response and returns false on refusal.
func patResourceScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))

	if scope == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("scope is required on the PAT plane"))

		return "", false
	}

	if pat := PATFromContext(r.Context()); pat != nil && !pat.AllowsResourceScope(scope) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("this token cannot use scope %q", scope))

		return "", false
	}

	return scope, true
}

// handlePATApply validates the batch against the PAT policy, then runs the
// shared apply (dry_run, prune and force work as on localhost).
func (a *API) handlePATApply(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	manifests, err := decodeManifests(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	if len(manifests) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("no manifests in body"))

		return
	}

	if err := validatePATManifests(PATFromContext(r.Context()), manifests); err != nil {
		writeErr(w, http.StatusForbidden, err)

		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	a.applyPost(w, r)
}

// handlePATManifests lists the manifests of one resource scope, optionally
// of one kind. The scope is required, so a scoped token never sees the
// manifests of other scopes, and their env values with them.
func (a *API) handlePATManifests(w http.ResponseWriter, r *http.Request) {
	scope, ok := patResourceScope(w, r)
	if !ok {
		return
	}

	list, err := a.Store.ListAll(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	out := make([]*Manifest, 0, len(list))

	for _, m := range list {
		if m == nil || m.Scope != scope {
			continue
		}

		if kind != "" && string(m.Kind) != kind {
			continue
		}

		out = append(out, m)
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: out})
}

// handlePATDescribe is `vd describe` for one resource: its manifest and
// status.
func (a *API) handlePATDescribe(w http.ResponseWriter, r *http.Request) {
	if _, ok := patResourceScope(w, r); !ok {
		return
	}

	a.handleDescribe(w, r)
}

// handlePATResourceDelete removes one resource (or one statefulset pod with
// ?ordinal=). ?prune=true also removes its volumes.
func (a *API) handlePATResourceDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := patResourceScope(w, r); !ok {
		return
	}

	a.handleResourceDelete(w, r)
}

// handlePATScopeWipe removes every resource of one scope. A hosted control
// plane that gives each customer a scope deletes the customer with it.
func (a *API) handlePATScopeWipe(w http.ResponseWriter, r *http.Request) {
	if _, ok := patResourceScope(w, r); !ok {
		return
	}

	a.handleScopeWipe(w, r)
}

// handlePATRestart is a rolling restart by (kind, scope, name).
func (a *API) handlePATRestart(w http.ResponseWriter, r *http.Request) {
	if _, ok := patResourceScope(w, r); !ok {
		return
	}

	a.handleRestart(w, r)
}

// handlePATRollback rolls a deployment back to an earlier release.
func (a *API) handlePATRollback(w http.ResponseWriter, r *http.Request) {
	if _, ok := patResourceScope(w, r); !ok {
		return
	}

	a.handleRollback(w, r)
}

// handlePATPodStop and handlePATPodStart stop and start one container, for
// example to pause the instance of a customer who stopped paying. The pod
// must belong to a scope the PAT holds.
func (a *API) handlePATPodStop(w http.ResponseWriter, r *http.Request) {
	if a.patPodInScope(w, r) {
		a.handlePodStop(w, r)
	}
}

func (a *API) handlePATPodStart(w http.ResponseWriter, r *http.Request) {
	if a.patPodInScope(w, r) {
		a.handlePodStart(w, r)
	}
}

func (a *API) patPodInScope(w http.ResponseWriter, r *http.Request) bool {
	pat := PATFromContext(r.Context())

	if pat == nil || len(pat.ResourceScopes) == 0 {
		return true
	}

	lister := a.Pods
	if lister == nil {
		lister = DockerPodsLister{}
	}

	pods, err := lister.ListPods()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("list pods: %w", err))

		return false
	}

	name := r.PathValue("name")

	for _, p := range pods {
		if p.Name != name {
			continue
		}

		if !pat.AllowsResourceScope(p.Scope) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("this token cannot use scope %q", p.Scope))

			return false
		}

		return true
	}

	writeErr(w, http.StatusNotFound, fmt.Errorf("no pod %q", name))

	return false
}

// patScopedConfig wraps a config handler so a PAT with ResourceScopes
// reads and writes config only in those scopes.
func patScopedConfig(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pat := PATFromContext(r.Context())

		if pat != nil && len(pat.ResourceScopes) > 0 {
			if _, ok := patResourceScope(w, r); !ok {
				return
			}
		}

		next(w, r)
	}
}
