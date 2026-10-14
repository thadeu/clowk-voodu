package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakePluginRoutes is a static PluginRouteLookup for tests.
type fakePluginRoutes struct {
	routes map[string]pluginRoute
}

func (f fakePluginRoutes) LookupRoute(prefix string) (pluginRoute, bool) {
	r, ok := f.routes[prefix]

	return r, ok
}

// fakeResourceIPs is a static ResourceIPResolver for tests.
type fakeResourceIPs struct {
	ip  string
	err error
}

func (f fakeResourceIPs) ResolveIP(_, _ string) (string, error) {
	return f.ip, f.err
}

// upstreamEcho is a stand-in for a plugin's internal API: it echoes the
// method, path, query and body so the test can assert what the proxy
// forwarded.
func upstreamEcho(t *testing.T) (host string, port int, close func()) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		w.Header().Set("X-Upstream", "hep3")
		_, _ = io.WriteString(w, "method="+r.Method+" path="+r.URL.Path+" query="+r.URL.RawQuery+" body="+string(body))
	}))

	h, p, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split upstream URL: %v", err)
	}

	pn, _ := strconv.Atoi(p)

	return h, pn, srv.Close
}

// patAPIWithProxy builds the standard test PAT API and wires the plugin
// route seams to a fake lookup + resolver pointing at the upstream.
func patAPIWithProxy(t *testing.T, prefix string, ip string, port int, resolveErr error) (*API, string) {
	t.Helper()

	api, plain, _, _ := newTestPATAPI(t)

	api.PluginRoutes = fakePluginRoutes{routes: map[string]pluginRoute{
		prefix: {Prefix: prefix, Port: port},
	}}
	api.ContainerIPs = fakeResourceIPs{ip: ip, err: resolveErr}

	return api, plain
}

func TestPluginProxy_ForwardsGET(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain := patAPIWithProxy(t, "hep3", host, port, nil)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/hep3/voip/main/calls?q=INVITE", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// The proxy must strip the /api/pat/v1/hep3/voip/main prefix and
	// forward only /calls + the query to the upstream.
	if !strings.Contains(got, "method=GET") {
		t.Errorf("upstream method wrong: %q", got)
	}

	if !strings.Contains(got, "path=/calls") {
		t.Errorf("upstream path wrong: %q (prefix not stripped to /calls)", got)
	}

	if !strings.Contains(got, "query=q=INVITE") {
		t.Errorf("upstream query lost: %q", got)
	}

	if resp.Header.Get("X-Upstream") != "hep3" {
		t.Errorf("upstream response header not propagated back")
	}
}

func TestPluginProxy_ForwardsPOSTBody(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain := patAPIWithProxy(t, "hep3", host, port, nil)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pat/v1/hep3/voip/main/ingest", strings.NewReader("payload-123"))
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, "method=POST") || !strings.Contains(got, "path=/ingest") {
		t.Errorf("POST not forwarded correctly: %q", got)
	}

	if !strings.Contains(got, "body=payload-123") {
		t.Errorf("request body not forwarded: %q", got)
	}
}

// patAPIWithProxyScopes is patAPIWithProxy with a PAT minted for exactly
// the given scopes (the default test PAT carries read + actions).
func patAPIWithProxyScopes(t *testing.T, scopes []Scope, ip string, port int) (*API, string) {
	t.Helper()

	api, _ := patAPIWithProxy(t, "fswapi", ip, port, nil)

	plain, rec, err := GeneratePAT(scopes, "scoped")
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}

	if err := api.Store.PutPAT(t.Context(), rec); err != nil {
		t.Fatalf("PutPAT: %v", err)
	}

	return api, plain
}

func pluginRequest(t *testing.T, ts *httptest.Server, method, path, plain string) int {
	t.Helper()

	req, _ := http.NewRequest(method, ts.URL+path, nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// A read-only PAT (the WebUI viewer token) reads a plugin API but can never
// reach a mutating route: a plugin POST can be `hupall`, and the proxy
// forwards the method verbatim.
func TestPluginProxy_ReadScopeCannotMutate(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain := patAPIWithProxyScopes(t, []Scope{ScopeRead}, host, port)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	if got := pluginRequest(t, ts, http.MethodGet, "/api/pat/v1/fswapi/fsw/api/status", plain); got != http.StatusOK {
		t.Errorf("read PAT GET status = %d, want 200", got)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if got := pluginRequest(t, ts, method, "/api/pat/v1/fswapi/fsw/api/actions/hupall", plain); got != http.StatusForbidden {
			t.Errorf("read PAT %s status = %d, want 403", method, got)
		}
	}
}

func TestPluginProxy_ActionsScopeCanMutate(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain := patAPIWithProxyScopes(t, []Scope{ScopeActions}, host, port)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	if got := pluginRequest(t, ts, http.MethodPost, "/api/pat/v1/fswapi/fsw/api/actions/reloadxml", plain); got != http.StatusOK {
		t.Errorf("actions PAT POST status = %d, want 200", got)
	}
}

// Mutating plugin routes share the core action rate limit: a stolen
// actions token cannot loop `hupall`.
func TestPluginProxy_MutationsAreRateLimited(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain := patAPIWithProxyScopes(t, []Scope{ScopeActions}, host, port)

	ts := httptest.NewServer(api.PATHandler(nil, 0.001, 2))
	defer ts.Close()

	path := "/api/pat/v1/fswapi/fsw/api/actions/reloadxml"

	for i := range 2 {
		if got := pluginRequest(t, ts, http.MethodPost, path, plain); got != http.StatusOK {
			t.Fatalf("POST %d within burst = %d, want 200", i+1, got)
		}
	}

	if got := pluginRequest(t, ts, http.MethodPost, path, plain); got != http.StatusTooManyRequests {
		t.Errorf("POST past burst = %d, want 429", got)
	}
}

func TestIsReadMethod(t *testing.T) {
	for method, want := range map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true,
		http.MethodPost: false, http.MethodPut: false, http.MethodPatch: false, http.MethodDelete: false,
		"PROPFIND": false,
	} {
		if got := isReadMethod(method); got != want {
			t.Errorf("isReadMethod(%s) = %v, want %v", method, got, want)
		}
	}
}

// The root of a plugin route (no trailing path) forwards to "/".
func TestPluginProxy_RootPath(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain := patAPIWithProxy(t, "hep3", host, port, nil)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/hep3/voip/main", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "path=/") {
		t.Errorf("root plugin path should forward to /, got %q", body)
	}
}

// A plugin route is behind PAT auth like everything else on the plane.
func TestPluginProxy_RequiresAuth(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, _ := patAPIWithProxy(t, "hep3", host, port, nil)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/pat/v1/hep3/voip/main/calls") //nolint:noctx // test
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (plugin route must require PAT)", resp.StatusCode)
	}
}

// An unresolvable container surfaces as 502, not a silent hang or 200.
func TestPluginProxy_ResolveError(t *testing.T) {
	api, plain := patAPIWithProxy(t, "hep3", "", 0, errResolve())

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/hep3/voip/main/calls", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when container unresolvable", resp.StatusCode)
	}
}

// An unknown (undeclared, non-reserved) prefix must fall through to the
// mux's 404 — never be treated as a plugin route.
func TestPluginProxy_UnknownPrefixFallsThrough(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain := patAPIWithProxy(t, "hep3", host, port, nil)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/notaplugin/a/b", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown prefix", resp.StatusCode)
	}
}

// A reserved prefix (pods) must keep serving the CORE route even if the
// front layer sees a plugin-shaped path — a plugin can never shadow a
// built-in endpoint.
func TestPluginProxy_ReservedPrefixServesCore(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	// Maliciously declare a plugin route on the reserved "pods" prefix.
	api, plain, _, _ := newTestPATAPI(t)
	api.PluginRoutes = fakePluginRoutes{routes: map[string]pluginRoute{
		"pods": {Prefix: "pods", Port: port},
	}}
	api.ContainerIPs = fakeResourceIPs{ip: host}

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	// /api/pat/v1/pods is the core pods list — must NOT be proxied.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/pods", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(body), "X-Upstream") || strings.Contains(string(body), "method=GET path=") {
		t.Errorf("reserved prefix was proxied to plugin upstream: %q", body)
	}

	// The core pods handler returns the seeded pod.
	if !strings.Contains(string(body), "clowk-web.a3f9") {
		t.Errorf("core pods route not served for reserved prefix: %q", body)
	}
}

// The dynamic port path: the route declares port_env (no fixed port),
// and the proxy reads the actual port from the resource's expanded spec
// env. Proven by pointing the upstream at a random port, putting ONLY
// that port in the spec env, and leaving the route's static Port at 0 —
// the request can only reach the upstream if the port was resolved from
// the spec.
func TestPluginProxy_DynamicPortFromSpec(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain, _, _ := newTestPATAPI(t)

	api.PluginRoutes = fakePluginRoutes{routes: map[string]pluginRoute{
		"hep3": {Prefix: "hep3", PortEnv: "HEP_API_ADDR"}, // no static Port
	}}
	api.ContainerIPs = fakeResourceIPs{ip: host}

	// Seed the statefulset spec the plugin would have expanded, with the
	// upstream port in HEP_API_ADDR.
	spec := json.RawMessage(fmt.Sprintf(`{"env":{"HEP_API_ADDR":"0.0.0.0:%d"}}`, port))
	if _, err := api.Store.Put(t.Context(), &Manifest{
		Kind: KindStatefulset, Scope: "voip", Name: "main", Spec: spec,
	}); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/hep3/voip/main/calls", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (port must resolve from spec env)", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "path=/calls") {
		t.Errorf("dynamic-port proxy didn't reach upstream: %q", body)
	}
}

// port_env that resolves to nothing (no spec) → 502, not a silent hit.
func TestPluginProxy_DynamicPortUnresolvable(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	api.PluginRoutes = fakePluginRoutes{routes: map[string]pluginRoute{
		"hep3": {Prefix: "hep3", PortEnv: "HEP_API_ADDR"},
	}}
	api.ContainerIPs = fakeResourceIPs{ip: "127.0.0.1"}
	// No manifest seeded → portFromResourceEnv finds nothing.

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/hep3/voip/main/calls", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when port_env unresolvable", resp.StatusCode)
	}
}

func TestPortFromSpecEnv(t *testing.T) {
	tests := []struct {
		spec string
		key  string
		want int
	}{
		{`{"env":{"HEP_API_ADDR":"0.0.0.0:8080"}}`, "HEP_API_ADDR", 8080},
		{`{"env":{"HEP_API_ADDR":"127.0.0.1:9123"}}`, "HEP_API_ADDR", 9123},
		{`{"env":{"OTHER":"x"}}`, "HEP_API_ADDR", 0}, // missing key
		{`{"env":{"HEP_API_ADDR":"noport"}}`, "HEP_API_ADDR", 0},
		{`{}`, "HEP_API_ADDR", 0}, // no env
		{``, "HEP_API_ADDR", 0},   // empty spec
	}

	for _, tt := range tests {
		got := portFromSpecEnv(json.RawMessage(tt.spec), tt.key)
		if got != tt.want {
			t.Errorf("portFromSpecEnv(%s, %s) = %d, want %d", tt.spec, tt.key, got, tt.want)
		}
	}
}

func TestParsePluginRoutePath(t *testing.T) {
	tests := []struct {
		path                      string
		wantOK                    bool
		prefix, scope, name, rest string
	}{
		{"/api/pat/v1/hep3/voip/main/calls", true, "hep3", "voip", "main", "/calls"},
		{"/api/pat/v1/hep3/voip/main/calls/abc?x=1", true, "hep3", "voip", "main", "/calls/abc?x=1"},
		{"/api/pat/v1/hep3/voip/main", true, "hep3", "voip", "main", "/"},
		{"/api/pat/v1/hep3/voip", false, "", "", "", ""}, // missing name
		{"/api/pat/v1/stats", false, "", "", "", ""},     // core route, too few segs
		{"/other/path", false, "", "", "", ""},           // not under PAT base
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			prefix, scope, name, rest, ok := parsePluginRoutePath(tt.path)

			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}

			if !ok {
				return
			}

			if prefix != tt.prefix || scope != tt.scope || name != tt.name || rest != tt.rest {
				t.Errorf("got (%q,%q,%q,%q), want (%q,%q,%q,%q)",
					prefix, scope, name, rest, tt.prefix, tt.scope, tt.name, tt.rest)
			}
		})
	}
}

func TestIsReservedPATPrefix(t *testing.T) {
	for _, p := range []string{"stats", "system", "pods", "logs", "metrics", "pats", "plugins"} {
		if !isReservedPATPrefix(p) {
			t.Errorf("%q should be reserved", p)
		}
	}

	for _, p := range []string{"hep3", "redis", "postgres", "anything"} {
		if isReservedPATPrefix(p) {
			t.Errorf("%q should NOT be reserved", p)
		}
	}
}

func TestDirPluginRouteLookup(t *testing.T) {
	root := t.TempDir()

	// A plugin declaring a DYNAMIC route (port_env, no fixed port) —
	// the real hep3 shape. Must resolve (the guard must not skip it).
	writePluginYML(t, root, "hep3", "name: hep3\nroutes:\n  prefix: hep3\n  port_env: HEP_API_ADDR\n")

	// A plugin with NO routes block (CLI-only) — must not match.
	writePluginYML(t, root, "postgres", "name: postgres\naliases: [pg]\n")

	// A plugin whose prefix defaults to its name, fixed port.
	writePluginYML(t, root, "redis", "name: redis\nroutes:\n  port: 6390\n")

	// A builtin plugin: one resource, plugins/<name>, no scope in the URL.
	writePluginYML(t, root, "fswapi", "name: fswapi\nroutes:\n  prefix: fswapi\n  port_env: FREESWITCH_API_ADDR\n  builtin: true\n")

	look := dirPluginRouteLookup{Root: root}

	if r, ok := look.LookupRoute("fswapi"); !ok || !r.Builtin || r.Plugin != "fswapi" {
		t.Errorf("fswapi lookup = %+v ok=%v, want a builtin route of plugin fswapi", r, ok)
	}

	if r, _ := look.LookupRoute("hep3"); r.Builtin {
		t.Error("hep3 does not declare builtin")
	}

	if r, ok := look.LookupRoute("hep3"); !ok || r.PortEnv != "HEP_API_ADDR" || r.Port != 0 {
		t.Errorf("hep3 lookup = %+v ok=%v, want PortEnv=HEP_API_ADDR (dynamic, no fixed port)", r, ok)
	}

	if r, ok := look.LookupRoute("redis"); !ok || r.Port != 6390 {
		t.Errorf("redis (prefix defaults to name) = %+v ok=%v, want port 6390", r, ok)
	}

	if _, ok := look.LookupRoute("postgres"); ok {
		t.Error("postgres has no routes block; must not resolve")
	}

	if _, ok := look.LookupRoute("nonexistent"); ok {
		t.Error("nonexistent prefix must not resolve")
	}
}

// recordingResourceIPs is a ResourceIPResolver that remembers which
// resource the proxy asked for — the builtin route's whole point is WHICH
// resource it targets, so the static fake is not enough.
type recordingResourceIPs struct {
	ip          string
	scope, name string
}

func (f *recordingResourceIPs) ResolveIP(scope, name string) (string, error) {
	f.scope, f.name = scope, name

	return f.ip, nil
}

func TestPluginProxy_BuiltinRouteTargetsPluginsScope(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	api, plain, _, _ := newTestPATAPI(t)

	ips := &recordingResourceIPs{ip: host}

	api.PluginRoutes = fakePluginRoutes{routes: map[string]pluginRoute{
		"fswapi": {Prefix: "fswapi", Port: port, Plugin: "fswapi", Builtin: true},
	}}
	api.ContainerIPs = ips

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	for path, wantUpstream := range map[string]string{
		"/api/pat/v1/fswapi/calls/count?x=1": "path=/calls/count query=x=1",
		"/api/pat/v1/fswapi":                 "path=/ query=",
	} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		patSign(req, plain)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), wantUpstream) {
			t.Errorf("%s: status=%d body=%q, want upstream %q", path, resp.StatusCode, body, wantUpstream)
		}

		// No scope/name in the URL: the resource is always plugins/<plugin>.
		if ips.scope != "plugins" || ips.name != "fswapi" {
			t.Errorf("%s: resolved %s/%s, want plugins/fswapi", path, ips.scope, ips.name)
		}
	}
}

func TestPluginProxy_BuiltinRouteMutationNeedsActions(t *testing.T) {
	host, port, closeUp := upstreamEcho(t)
	defer closeUp()

	builtin := fakePluginRoutes{routes: map[string]pluginRoute{
		"fswapi": {Prefix: "fswapi", Port: port, Plugin: "fswapi", Builtin: true},
	}}

	readAPI, readPAT := patAPIWithProxyScopes(t, []Scope{ScopeRead}, host, port)
	readAPI.PluginRoutes = builtin

	actionsAPI, actionsPAT := patAPIWithProxyScopes(t, []Scope{ScopeActions}, host, port)
	actionsAPI.PluginRoutes = builtin

	readTS := httptest.NewServer(readAPI.PATHandler(nil, 10, 3))
	defer readTS.Close()

	actionsTS := httptest.NewServer(actionsAPI.PATHandler(nil, 10, 3))
	defer actionsTS.Close()

	if got := pluginRequest(t, readTS, http.MethodPost, "/api/pat/v1/fswapi/actions/hupall", readPAT); got != http.StatusForbidden {
		t.Errorf("read PAT POST on a builtin route = %d, want 403", got)
	}

	if got := pluginRequest(t, actionsTS, http.MethodPost, "/api/pat/v1/fswapi/actions/reloadxml", actionsPAT); got != http.StatusOK {
		t.Errorf("actions PAT POST on a builtin route = %d, want 200", got)
	}
}

func TestMatchPluginRoute_RegularRouteStillNeedsScopeAndName(t *testing.T) {
	api := &API{PluginRoutes: fakePluginRoutes{routes: map[string]pluginRoute{
		"hep3": {Prefix: "hep3", Port: 8080, Plugin: "hep3"},
	}}}

	if _, _, _, _, ok := api.matchPluginRoute("/api/pat/v1/hep3/voip"); ok {
		t.Error("a regular route without a name must fall through to the core mux")
	}

	_, scope, name, rest, ok := api.matchPluginRoute("/api/pat/v1/hep3/voip/main/calls")
	if !ok || scope != "voip" || name != "main" || rest != "/calls" {
		t.Errorf("regular route = (%q,%q,%q,%v), want voip/main /calls", scope, name, rest, ok)
	}

	if _, _, _, _, ok := api.matchPluginRoute("/api/pat/v1/plugins/fswapi"); ok {
		t.Error("the plugins prefix belongs to the core install routes")
	}
}

func writePluginYML(t *testing.T, root, name, yml string) {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(yml), 0o644); err != nil {
		t.Fatalf("write plugin.yml: %v", err)
	}
}

func errResolve() error {
	return &resolveErr{}
}

type resolveErr struct{}

func (e *resolveErr) Error() string { return "no running container" }
