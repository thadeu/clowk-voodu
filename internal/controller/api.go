package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// API is the HTTP surface of the controller. Handlers are thin: decode,
// call into Store or a subsystem, encode. Business logic lives in the
// packages the handlers delegate to.
type API struct {
	Store Store
	// Version is reported by /health.
	Version string
}

// Handler returns an http.Handler with all endpoints registered.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/apply", a.handleApply)
	mux.HandleFunc("/status", a.handleStatus)
	mux.HandleFunc("/exec", a.handleExec)
	mux.HandleFunc("/logs", a.handleLogs)
	mux.HandleFunc("/plugins", a.handlePlugins)

	return logRequests(mux)
}

// envelope is the response shape used for /exec and /plugins. Matches
// the plugin JSON protocol so the CLI can render any of them uniformly.
type envelope struct {
	Status string `json:"status"`
	Data   any    `json:"data,omitempty"`
	Error  string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, envelope{Status: "error", Error: err.Error()})
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": a.Version,
	})
}

// handleApply accepts POST (upsert) / GET (list) / DELETE (remove).
// POST body is either a single manifest or a JSON array of manifests.
// DELETE takes ?kind=<k>&name=<n>.
func (a *API) handleApply(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.applyPost(w, r)
	case http.MethodGet:
		a.applyGet(w, r)
	case http.MethodDelete:
		a.applyDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))
	}
}

func (a *API) applyPost(w http.ResponseWriter, r *http.Request) {
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

	applied := make([]*Manifest, 0, len(manifests))

	for _, m := range manifests {
		stored, err := a.Store.Put(r.Context(), m)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}

		applied = append(applied, stored)
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: applied})
}

func (a *API) applyGet(w http.ResponseWriter, r *http.Request) {
	kindStr := r.URL.Query().Get("kind")

	if kindStr == "" {
		list, err := a.Store.ListAll(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}

		writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: list})
		return
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	list, err := a.Store.List(r.Context(), kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: list})
}

func (a *API) applyDelete(w http.ResponseWriter, r *http.Request) {
	kindStr := r.URL.Query().Get("kind")
	name := r.URL.Query().Get("name")

	if kindStr == "" || name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("kind and name are required"))
		return
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	deleted, err := a.Store.Delete(r.Context(), kind, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if !deleted {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s/%s not found", kind, name))
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok"})
}

// handleStatus is a union of /desired and (later) /actual. In M3 we only
// have desired state; actual state lands once the reconciler records
// container state in /actual/nodes/*.
func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	desired, err := a.Store.ListAll(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"desired": desired,
			"actual":  []any{},
		},
	})
}

// handleExec is the endpoint the CLI forwards unknown commands to. In M3
// there is no plugin system yet, so every call returns 404 with a clear
// pointer to M5.
func (a *API) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	var req struct {
		Args []string `json:"args"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	writeErr(w, http.StatusNotFound, fmt.Errorf(
		"no builtin and no plugin registered for %q (plugin system lands in M5)",
		strings.Join(req.Args, " "),
	))
}

// handleLogs is a placeholder for M4+. We reserve the shape now so the
// CLI can be wired against it today.
func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, fmt.Errorf("log streaming arrives with the reconciler in M4"))
}

// handlePlugins lists plugin manifests currently stored at /plugins/*.
// The real install/uninstall flow lives in M5.
func (a *API) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: []any{}})
}

// decodeManifests accepts either a single-object or array-of-objects body.
func decodeManifests(body []byte) ([]*Manifest, error) {
	trimmed := strings.TrimSpace(string(body))

	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty body")
	}

	if trimmed[0] == '[' {
		var list []*Manifest
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decode manifest array: %w", err)
		}

		return list, nil
	}

	var single Manifest
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	return []*Manifest{&single}, nil
}

func readBody(r *http.Request) ([]byte, error) {
	const maxBody = 1 << 20

	return io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
}
