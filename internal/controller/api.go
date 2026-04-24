package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// errScopeNotFound is returned by resolveScope when no manifest matches
// (kind, name) in any scope. applyDelete maps it to 404; ambiguous
// matches get a different error and map to 400.
var errScopeNotFound = errors.New("not found in any scope")

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

	// Two ingresses pointing at the same Host produce duplicate caddy
	// automation policies, which caddy rejects at /load with an opaque
	// error. Refuse here so the operator sees a clean "host already in
	// use" message instead of a reconcile-time cascade.
	if err := a.checkIngressHostCollisions(r.Context(), manifests); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// dry_run=true makes this a plan-only call: `voodu diff` uses it to
	// ask "what would happen if I applied this?" without touching the
	// store. We still run every validation (host collisions already
	// ran above) and fetch each input's current on-disk manifest so
	// the CLI can render a proper field-by-field diff client-side.
	dryRun := r.URL.Query().Get("dry_run") == "true"

	// `?prune=false` is the escape hatch for shared-scope setups where
	// several independent applies (different repos, different pipelines)
	// each declare only a slice of the scope. Default remains prune=on —
	// that's the source-of-truth contract most mono-repo users want, and
	// renames don't leave zombies behind. See README "Shared scope" for
	// the intended usage pattern.
	prune := r.URL.Query().Get("prune") != "false"

	var (
		applied = make([]*Manifest, 0, len(manifests))
		current = make([]*Manifest, 0, len(manifests))
	)

	for _, m := range manifests {
		// Capture the existing on-disk manifest BEFORE we overwrite it,
		// so diff can compare "was vs. will be". Nil slot = a create.
		// Unscoped kinds use empty scope; Store.Get handles that.
		before, err := a.Store.Get(r.Context(), m.Kind, m.Scope, m.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("read current: %w", err))
			return
		}

		current = append(current, before)

		if dryRun {
			applied = append(applied, m)
			continue
		}

		stored, err := a.Store.Put(r.Context(), m)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}

		applied = append(applied, stored)
	}

	var pruned []pruneTarget

	if prune {
		targets, err := a.computePruneTargets(r.Context(), manifests)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("prune plan: %w", err))
			return
		}

		pruned = targets

		if !dryRun {
			if err := a.applyPrune(r.Context(), pruned); err != nil {
				writeErr(w, http.StatusInternalServerError, fmt.Errorf("prune: %w", err))
				return
			}
		}
	}

	prunedRefs := make([]string, 0, len(pruned))
	for _, t := range pruned {
		prunedRefs = append(prunedRefs, t.String())
	}

	data := map[string]any{
		"applied": applied,
		"pruned":  prunedRefs,
	}

	// `current` is only meaningful for dry-run — it's the "before"
	// side of the diff. Regular apply callers don't need it and the
	// CLI's apply-output doesn't render it, so skip the payload bulk.
	if dryRun {
		data["current"] = current
		data["dry_run"] = true
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   data,
	})
}

// checkIngressHostCollisions rejects any apply that would leave two
// ingresses sharing the same Host. Caddy treats host as the
// automation-policy key, so duplicates produce "cannot apply more than
// one automation policy to host" at /load time. Enforcing uniqueness
// here trades a fuzzy reconcile error for a crisp validation error.
//
// The check covers both intra-request duplicates (two ingresses in the
// same body) and cross-request ones (the body's hosts against what's
// already in etcd, minus any ingress being re-applied under the same
// (scope, name) — that's an update, not a collision).
func (a *API) checkIngressHostCollisions(ctx context.Context, manifests []*Manifest) error {
	incoming := map[string]string{}

	for _, m := range manifests {
		if m.Kind != KindIngress {
			continue
		}

		host, err := ingressHost(m)
		if err != nil {
			return err
		}

		if host == "" {
			continue
		}

		ref := fmt.Sprintf("%s/%s", m.Scope, m.Name)

		if prev, dup := incoming[host]; dup {
			return fmt.Errorf("ingress host %q claimed by both %s and %s in this apply", host, prev, ref)
		}

		incoming[host] = ref
	}

	existing, err := a.Store.List(ctx, KindIngress)
	if err != nil {
		return err
	}

	replacing := map[string]struct{}{}

	for _, m := range manifests {
		if m.Kind == KindIngress {
			replacing[m.Scope+"/"+m.Name] = struct{}{}
		}
	}

	for _, e := range existing {
		if _, updating := replacing[e.Scope+"/"+e.Name]; updating {
			continue
		}

		host, err := ingressHost(e)
		if err != nil {
			return err
		}

		if host == "" {
			continue
		}

		if claimant, dup := incoming[host]; dup {
			return fmt.Errorf(
				"ingress host %q already owned by %s/%s — delete it or rename %s",
				host, e.Scope, e.Name, claimant,
			)
		}
	}

	return nil
}

// ingressHost pulls the `host` field out of the ingress spec JSON. Kept
// local to the validator so the API layer doesn't depend on the typed
// manifest package just for a single field read.
func ingressHost(m *Manifest) (string, error) {
	if len(m.Spec) == 0 {
		return "", nil
	}

	var spec struct {
		Host string `json:"host"`
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return "", fmt.Errorf("ingress/%s: decode spec: %w", m.Name, err)
	}

	return spec.Host, nil
}

// pruneTarget names one resource that would be (or has been) pruned.
// Kept as a struct so diff can surface structured info; the string form
// `kind/scope/name` matches what the store keys on disk.
type pruneTarget struct {
	Kind  Kind
	Scope string
	Name  string
}

func (p pruneTarget) String() string {
	return fmt.Sprintf("%s/%s/%s", p.Kind, p.Scope, p.Name)
}

// computePruneTargets is the pure half of prune: it figures out what
// *would* be deleted without touching the store. Splitting this from
// applyPrune lets `/apply?dry_run=true` answer "what would change" for
// `voodu diff`, while the real apply path runs both halves.
//
// The per-(scope, kind) granularity is deliberate: an apply of
// `deployments.hcl` won't touch ingresses in the same scope, so callers
// can decompose by kind without losing the pair they didn't include.
func (a *API) computePruneTargets(ctx context.Context, manifests []*Manifest) ([]pruneTarget, error) {
	keep := map[string]map[string]struct{}{}

	for _, m := range manifests {
		if !IsScoped(m.Kind) {
			continue
		}

		bucket := string(m.Kind) + "/" + m.Scope

		if _, ok := keep[bucket]; !ok {
			keep[bucket] = map[string]struct{}{}
		}

		keep[bucket][m.Name] = struct{}{}
	}

	var targets []pruneTarget

	for bucket, names := range keep {
		i := strings.Index(bucket, "/")
		kind := Kind(bucket[:i])
		scope := bucket[i+1:]

		existing, err := a.Store.ListByScope(ctx, kind, scope)
		if err != nil {
			return nil, err
		}

		for _, e := range existing {
			if _, kept := names[e.Name]; kept {
				continue
			}

			targets = append(targets, pruneTarget{Kind: e.Kind, Scope: e.Scope, Name: e.Name})
		}
	}

	return targets, nil
}

// applyPrune is the side-effect half: given a plan from
// computePruneTargets, delete each target from the store.
func (a *API) applyPrune(ctx context.Context, targets []pruneTarget) error {
	for _, t := range targets {
		if _, err := a.Store.Delete(ctx, t.Kind, t.Scope, t.Name); err != nil {
			return err
		}
	}

	return nil
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
	scope := r.URL.Query().Get("scope")

	if kindStr == "" || name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("kind and name are required"))
		return
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if IsScoped(kind) && scope == "" {
		// For scoped kinds the CLI must provide scope. Scan and return a
		// clear error if there's a single match, or ask for disambiguation.
		resolved, err := resolveScope(r.Context(), a.Store, kind, name)
		if err != nil {
			if errors.Is(err, errScopeNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}

			writeErr(w, http.StatusBadRequest, err)
			return
		}

		scope = resolved
	}

	deleted, err := a.Store.Delete(r.Context(), kind, scope, name)
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

// resolveScope finds the single scope that owns (kind, name) when the
// caller didn't provide one. Used by DELETE/GET convenience paths so
// `voodu scale -a web` keeps working without forcing the operator to
// remember which scope `web` lives in. Ambiguous matches return an
// error listing the candidates — there's no safe default to pick.
func resolveScope(ctx context.Context, store Store, kind Kind, name string) (string, error) {
	all, err := store.List(ctx, kind)
	if err != nil {
		return "", err
	}

	var matches []string

	for _, m := range all {
		if m.Name == name {
			matches = append(matches, m.Scope)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s/%s: %w", kind, name, errScopeNotFound)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%s/%s is ambiguous across scopes %v — pass ?scope=...", kind, name, matches)
	}
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
