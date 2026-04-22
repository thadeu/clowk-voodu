package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// API is the HTTP surface of the controller. Handlers are thin: decode,
// call into Store or a subsystem, encode. Business logic lives in the
// packages the handlers delegate to.
type API struct {
	Store Store
	// Version is reported by /health.
	Version string

	// PluginsRoot is the filesystem directory where plugins live. When
	// empty, /exec and /plugins endpoints return "no plugin system"
	// errors — used by unit tests that don't care about plugins.
	PluginsRoot string

	// NodeName and EtcdClient are passed to plugins via environment so
	// plugin authors can reach back into the cluster if they need to.
	NodeName   string
	EtcdClient string

	// Invoker is the shared plugin-execution seam — the reconciler uses
	// the same interface for its handlers, so /exec and reconcile-time
	// calls go through one code path (env injection, plugin resolution,
	// envelope parsing). Nil means /exec falls back to loading plugins
	// directly from PluginsRoot; production wires DirInvoker.
	Invoker PluginInvoker
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
	mux.HandleFunc("POST /plugins/install", a.handlePluginInstall)
	mux.HandleFunc("DELETE /plugins/{name}", a.handlePluginRemove)

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

// handleExec dispatches unknown CLI commands to a plugin. Body is
// {"args": ["<plugin>", "<cmd>", ...]} plus optional env. The CLI's
// colon rewriter already split "postgres:create" into two args.
//
// Response is the plugin JSON envelope when the plugin emitted one,
// or a synthetic envelope wrapping plain-text stdout so the CLI can
// render with -o text|json|yaml uniformly.
func (a *API) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	var req struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	if len(req.Args) < 2 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("need at least <plugin> <command>"))
		return
	}

	if a.PluginsRoot == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf(
			"no builtin and no plugin registered for %q",
			strings.Join(req.Args, " "),
		))

		return
	}

	pluginName, cmd, cmdArgs := req.Args[0], req.Args[1], req.Args[2:]

	res, err := a.invoker().Invoke(r.Context(), pluginName, cmd, cmdArgs, req.Env)
	if err != nil {
		// LoadFromDir failure maps to 404 — everything else is 500.
		// A dedicated error type would be cleaner, but string matching
		// on the DirInvoker's wrapped error is good enough for one site.
		if strings.Contains(err.Error(), "plugin ") && strings.Contains(err.Error(), pluginName) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("plugin %q not installed", pluginName))
			return
		}

		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	if res.Envelope != nil {
		writeJSON(w, pluginExitToHTTP(res.ExitCode), res.Envelope)
		return
	}

	// Plain-text plugins: wrap stdout in an envelope so downstream
	// formatters have a consistent shape. Exit != 0 becomes error.
	if res.ExitCode != 0 {
		writeErr(w, pluginExitToHTTP(res.ExitCode), fmt.Errorf("%s", strings.TrimSpace(res.CombinedOutput())))
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]string{"stdout": string(res.Raw)},
	})
}

// invoker returns the configured PluginInvoker, falling back to a
// DirInvoker built from the API's plugins root. Lazy so callers that
// explicitly wire Invoker don't pay for the DirInvoker allocation.
func (a *API) invoker() PluginInvoker {
	if a.Invoker != nil {
		return a.Invoker
	}

	return &DirInvoker{
		PluginsRoot: a.PluginsRoot,
		NodeName:    a.NodeName,
		EtcdClient:  a.EtcdClient,
	}
}

// pluginExitToHTTP maps a plugin exit code to an HTTP status. Zero is
// 200, anything else is 500 — plugins should use the envelope's Error
// field for structured failure, not HTTP semantics.
func pluginExitToHTTP(code int) int {
	if code == 0 {
		return http.StatusOK
	}

	return http.StatusInternalServerError
}

// handleLogs is a placeholder for M4+. We reserve the shape now so the
// CLI can be wired against it today.
func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, fmt.Errorf("log streaming arrives with the reconciler in M4"))
}

// handlePlugins lists plugins currently installed under PluginsRoot.
// Plugins that fail to load are reported as errors alongside the list —
// the controller does not hide a broken plugin from the operator.
func (a *API) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	if a.PluginsRoot == "" {
		writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: []any{}})
		return
	}

	loaded, loadErrs := plugins.LoadAll(a.PluginsRoot)

	manifests := make([]plugin.Manifest, 0, len(loaded))
	for _, p := range loaded {
		manifests = append(manifests, p.Manifest)
	}

	errs := make([]string, 0, len(loadErrs))
	for _, e := range loadErrs {
		errs = append(errs, e.Error())
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"plugins": manifests,
			"errors":  errs,
		},
	})
}

// handlePluginInstall materialises a plugin from the given source (local
// path or git URL). Body: {"source": "owner/repo"} or {"source": "/path"}.
func (a *API) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	if a.PluginsRoot == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("plugin system not configured"))
		return
	}

	var req struct {
		Source string `json:"source"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	if strings.TrimSpace(req.Source) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("source is required"))
		return
	}

	inst := &plugins.Installer{Root: a.PluginsRoot}

	loaded, err := inst.Install(r.Context(), req.Source)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: loaded.Manifest})
}

// handlePluginRemove deletes the plugin directory. 404 if the plugin
// isn't installed; the DELETE is otherwise idempotent.
func (a *API) handlePluginRemove(w http.ResponseWriter, r *http.Request) {
	if a.PluginsRoot == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("plugin system not configured"))
		return
	}

	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin name is required"))
		return
	}

	inst := &plugins.Installer{Root: a.PluginsRoot}

	ok, err := inst.Remove(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("plugin %q not installed", name))
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok"})
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
