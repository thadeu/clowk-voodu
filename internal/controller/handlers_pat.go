// handlers_pat.go is the CLI-facing PAT management surface. Three
// HTTP endpoints, all on the orchestration plane (`:8686`, the
// localhost-only listener) — same trust posture as `/apply`. The
// observability plane (`/api/pat/v1/*`) uses these PATs but never
// creates/lists/revokes them.
//
//	POST   /pats              create a new PAT (plain shown once)
//	GET    /pats              list all PATs (NEVER includes plain)
//	DELETE /pats/{id}         revoke one PAT
//
// CLI `vd pat create/list/revoke` is the only documented consumer.
// Direct curl works too but operators rarely need it.

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// patCreateRequest is the POST /pats body. Minimal — scopes are
// required, name is optional, and the rest is server-generated.
type patCreateRequest struct {
	Scopes []string `json:"scopes"`
	Name   string   `json:"name,omitempty"`
}

// patCreateResponse is the POST /pats success body. The `token`
// field is the plain text — shown ONCE, never returned by any
// other endpoint. The `record` field has every persisted field
// EXCEPT HashHex (we don't ever expose the hash; the receiver
// already has the plain token, and listing endpoints redact it).
type patCreateResponse struct {
	Token  string      `json:"token"`
	Record patListItem `json:"record"`
}

// patListItem is the redacted view of a PAT. Used by both the
// create response and the list endpoint — HashHex is never
// included on either path so accidental log capture of the
// response body doesn't leak the secret.
type patListItem struct {
	ID         string   `json:"id"`
	Scopes     []string `json:"scopes"`
	Name       string   `json:"name,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
}

func redactPAT(p PAT) patListItem {
	scopes := make([]string, 0, len(p.Scopes))
	for _, s := range p.Scopes {
		scopes = append(scopes, string(s))
	}

	li := patListItem{
		ID:        p.ID,
		Scopes:    scopes,
		Name:      p.Name,
		CreatedAt: p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}

	if !p.LastUsedAt.IsZero() {
		li.LastUsedAt = p.LastUsedAt.Format("2006-01-02T15:04:05Z07:00")
	}

	return li
}

// handlePATCreate mints a fresh PAT. The plain token is returned
// ONCE in the response body — clients (CLI) display it to the
// operator and never receive it again.
//
// Operator-supplied input (scopes, name) is validated through the
// same `GeneratePAT` path the SDK would use; storage is direct
// `PutPAT`. No reconcile loop involved — PATs are not desired-state
// objects.
func (a *API) handlePATCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var req patCreateRequest

	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	if len(req.Scopes) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("scopes is required (e.g. [\"read\"] or [\"read\",\"actions\"])"))
		return
	}

	scopes := make([]Scope, 0, len(req.Scopes))
	for _, s := range req.Scopes {
		scopes = append(scopes, Scope(s))
	}

	plain, record, err := GeneratePAT(scopes, req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if err := a.Store.PutPAT(r.Context(), record); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusCreated, envelope{
		Status: "ok",
		Data: patCreateResponse{
			Token:  plain,
			Record: redactPAT(record),
		},
	})
}

// handlePATList returns every PAT on the host as a redacted list.
// Sorted by CreatedAt descending so the newest PATs appear first
// in `vd pat list` — operators usually want to inspect the one
// they just minted.
func (a *API) handlePATList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "POST, GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	pats, err := a.Store.ListPATs(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// Newest-first ordering. PATs without CreatedAt (impossible in
	// production but possible in older test fixtures) sort to the
	// end via the zero-time comparison.
	sort.Slice(pats, func(i, j int) bool {
		return pats[i].CreatedAt.After(pats[j].CreatedAt)
	})

	items := make([]patListItem, 0, len(pats))
	for _, p := range pats {
		items = append(items, redactPAT(p))
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]any{"pats": items},
	})
}

// handlePATRevoke removes one PAT by ID. Returns 404 when no such
// PAT exists (so operators see a clean error rather than a confusing
// success); idempotency is at the etcd level — re-running the same
// DELETE on a missing ID still gets 404.
//
// Path: DELETE /pats/{id}
func (a *API) handlePATRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("path: id is required"))
		return
	}

	deleted, err := a.Store.DeletePAT(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if !deleted {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no PAT with id %q", id))
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok"})
}
